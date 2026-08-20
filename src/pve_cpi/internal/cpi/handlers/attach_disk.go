package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// diskHints is the v2 attach_disk return value. The Director uses the "path" key
// to tell the BOSH agent where the persistent disk is mounted.
//
// Shape: {"path": "/dev/sdX"}
//
// The Director passes disk_hints back to the agent via the agent API so the agent
// can populate /var/vcap/store correctly without requiring a registry lookup.
type diskHints struct {
	Path string `json:"path"`
}

// HandleAttachDisk returns a Handler for the BOSH CPI v2 attach_disk method.
//
// Arguments (positional JSON array):
//
//	[0] vm_cid   string — VMID of the target VM (integer as string, e.g. "100")
//	[1] disk_cid string — persistent disk CID in the "pvd-" (or compressed
//	                      "pvz-") envelope form emitted by create_disk
//
// Returns (v2): disk_hints object {"path": "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>"} per the BOSH CPI v2 spec.
//
// Logic:
//  1. Parse vm_cid to VMID int; parse disk_cid to storage+volid components.
//  2. Choose a scsi slot >= 1 (never scsi0 — see chooseSCSISlotSkippingZero
//     for rationale: BOSH agent's resolver resolves /dev/sda to /dev/vda when
//     a virtio root disk exists, so scsi0 attachments are silently shadowed
//     by the root disk). Call qemu.AttachDisk with the chosen DiskID; if the
//     volume is already attached at the chosen slot, the SDK is idempotent.
//  3. AttachDisk does not return a UPID (it is a synchronous config PUT, not an
//     async task), so no AwaitTask is needed.
//  4. Resolve the final diskID from the current VM config via pve.ResolveDiskID to
//     confirm the attachment and get the canonical slot name.
//  5. Derive device path from diskID as a PVE-stable by-id symlink:
//     "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>".
//  6. Record the verbatim disk_cid against the bare volid on the VM's
//     description sentinel (best-effort; see pve.UpdateAttachedDiskCID) so a
//     later get_disks call can return the exact CID the Director stored.
//  7. Return disk_hints{"path": devicePath}.
//
// Device path convention:
//
//	virtio0 → /dev/vda  (default root disk bus — not a persistent disk)
//	scsi0   → /dev/sda  (root disk under pve.root_disk_bus=scsi — not a persistent disk)
//	scsi<N> (N>=1) → /dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>
//
// We DO NOT return a "/dev/sd<X>" hint. BOSH agent's
// mappedDevicePathResolver probes prefixes in the order
// [/dev/xvd, /dev/vd, /dev/sd] when the hint starts with "/dev/sd", and
// would resolve "/dev/sd<X>" to the virtio root disk "/dev/vd<X>" because
// /dev/vda exists. By returning a path that does not start with "/dev/sd",
// the resolver falls into its else-branch, finds the by-id symlink,
// follows it to whatever /dev/sd<X> the kernel actually assigned, and
// returns that. PVE virtio-scsi-pci sets the QEMU disk serial to
// "drive-scsi<N>", and udev creates the corresponding by-id symlink.
//
// Bus selection: "scsi" is always used for persistent disks regardless of
// VMDiskFormat. The scsi bus matches the virtio-scsi-single controller that
// create_vm configures. The system disk lives on virtio0 by default; setting
// pve.root_disk_bus=scsi moves it to scsi0 instead — the resolver's probe
// order (see the System field comment in create_vm.go's agentCfg
// construction) finds it correctly either way with no agent-config change.
func HandleAttachDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// --------------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// --------------------------------------------------------------------
		vmCID, diskCID, err := attachDiskParseArgs(args)
		if err != nil {
			return nil, err
		}
		// Strip optional metadata suffix so all PVE API calls receive a plain
		// "<storage>:<volid>" string. diskCID (the full encoded form) is
		// preserved for agent hints and log fields so the Director can match
		// the CID it originally stored. meta carries per-disk performance
		// options resolved at create_disk time; nil for bare (legacy) CIDs.
		bareDiskCID, meta, err := decodeDiskCID(ctx, deps, "attach_disk", diskCID)
		if err != nil {
			return nil, err
		}

		// Resolve the CID to the volid the cluster currently knows the volume
		// by (the identity seam): legacy CIDs come back as-is with no API
		// cost; stable-ID CIDs pay one cluster scan and may surface an
		// interrupted transfer, which the guard below resumes.
		rd, err := resolveDiskForOp(ctx, deps, "attach_disk", diskCID, bareDiskCID, meta)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 2. Parse vm_cid → VMID; parse disk_cid → storage + volid.
		// --------------------------------------------------------------------
		node, vmid, err := attachDiskResolveNode(ctx, deps, vmCID, rd.volid)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 3. Resolve the volume's current holder, then unpark, plan a
		// reassignment transfer, or refuse.
		//
		// One cluster scan answers both questions this handler has to settle
		// before it writes a volid into a VM config: a parker holder must be
		// unparked (or reassigned) first, and any other holder means the
		// volume is already attached elsewhere. Unpark failure → retriable
		// error; the disk remains parked and the next BOSH retry will
		// re-attempt here.
		//
		// Co-location check (local backend) is unaffected: UnparkDisk performs
		// a DetachDisk on the parker VM but does not move the volume between
		// nodes, so the disk node established by attachDiskResolveNode remains
		// valid.
		// --------------------------------------------------------------------
		plan, err := guardAndUnparkBeforeAttach(ctx, deps, "attach_disk", &rd, node, vmid)
		if err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 4. Snapshot pre-flight guard.
		//
		// PVE permits attaching a disk to a VM that has existing snapshots, but the
		// newly attached disk is absent from all prior snapshots — on rollback the
		// disk disappears, causing silent data loss. Guard before any mutating PVE
		// call to surface the risk early with an actionable message.
		//
		// Policy (D2-C, D3-C):
		//   HasSnapshots error + RequireSnapshotCheckPass=true  → fail-closed (abort)
		//   HasSnapshots error + RequireSnapshotCheckPass=false → WARN + proceed
		//   Snapshots present + AllowDiskOpsWithSnapshots=false → Cloud error (hard fail)
		//   Snapshots present + AllowDiskOpsWithSnapshots=true  → WARN + proceed
		//   No snapshots                                        → proceed normally
		// --------------------------------------------------------------------
		if err := attachDiskSnapshotGuard(ctx, deps, vmCID, node, vmid, deps.Config, deps.Log(ctx)); err != nil {
			return nil, err
		}

		// --------------------------------------------------------------------
		// 4b. Per-node in-flight gate (opt-in; limit=0 → unlimited, no gating).
		// --------------------------------------------------------------------
		if deps.Config != nil {
			inflightRelease, inflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
			if inflightErr != nil {
				return nil, cpierrors.Retriable("attach_disk: in-flight limit exceeded or context cancelled on node %s: %s", node, inflightErr.Error())
			}
			defer inflightRelease()
		}

		// --------------------------------------------------------------------
		// 5–8. Shared attach core: slot choice, perf merge + invariants, the
		// reassignment transfer or config PUT, confirmation, and the sentinel
		// CID record. create_vm's disk_cids pre-attach runs the identical
		// sequence (see attachDiskCore).
		// --------------------------------------------------------------------
		diskID, devicePath, err := attachDiskCore(ctx, deps, "attach_disk", vmCID, node, vmid, diskCID, rd, plan)
		if err != nil {
			return nil, err
		}

		deps.Log(ctx).Info("attach_disk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
			log.String("device_path", devicePath),
		)

		// --------------------------------------------------------------------
		// 9. Return disk_hints (v2 spec: object with "path" key).
		// --------------------------------------------------------------------
		return diskHints{Path: devicePath}, nil
	})
}

// attachDiskCore is the shared attach sequence used by attach_disk and by
// create_vm's disk_cids pre-attach: slot choice → perf merge + invariants →
// config PUT → confirm → sentinel record. The holder guard + unpark step is
// invoked by each caller before this (attach_disk needs it ahead of its
// snapshot guard; create_vm runs it inside attachPersistentDisks) so both
// paths refuse a foreign holder and unpark a parked disk before any config
// write. Everything that decides how a volid lands in a VM config lives here
// so the two handlers cannot drift; node resolution, snapshot policy, and
// rollback semantics stay with the callers. op is the CPI method name for
// error and log prefixes.
//
// Slot selection detail (why scsi0 is never used): the SDK's default slot
// selection picks the lowest free index — 0 for a VM with no other scsi
// disks. That would yield /dev/sda inside the guest, which collides with the
// BOSH agent's mappedDevicePathResolver: the resolver strips the "/dev/sd"
// prefix and probes "/dev/xvd", "/dev/vd", "/dev/sd" in turn (see
// create_vm.go agent-Disks.System note). With the default virtio0 root disk,
// /dev/vda exists, so the resolver returns /dev/vda for any "/dev/sda" hint —
// including the persistent disk hint — then runs persistent-disk partitioning
// against the root disk and fails. Under pve.root_disk_bus=scsi, scsi0 IS the
// root disk, so reserving scsi0 is what makes root and persistent disks
// distinguishable at all in that mode. Reserving scsi0 forces persistent
// disks to scsi1+ (/dev/sdb+); the resolver finds no /dev/vdb, falls through
// to /dev/sdb, and operates on the correct disk.
//
// Perf options: global defaults come from config (no call-level
// cloud_properties at attach time); per-disk options stored in the CID
// metadata at create_disk time take precedence over globals. The merged set
// is bus-filtered (scsi keeps all) then baked into the volid argument passed
// to AttachDisk. When no options are present the call is byte-identical to
// the pre-feature behavior.
//
// Bus is always "scsi" for persistent disks. PVE config disk values are
// canonical "<storage>:<volname>" (e.g. "data:vm-9003-disk-0"); pass the
// full disk_cid — a bare volname is rejected with
// "scsi0.file: invalid format ...".
func attachDiskCore(
	ctx context.Context,
	deps Deps,
	op string,
	vmCID string,
	node string,
	vmid int,
	diskCID string,
	rd resolvedDisk,
	plan attachPlan,
) (diskID, devicePath string, err error) {
	const bus = "scsi"

	desiredDiskID, prepErr := chooseSCSISlotSkippingZero(ctx, deps, op, node, vmid, rd.volid)
	if prepErr != nil {
		if pve.IsNotFound(prepErr) {
			return "", "", cpierrors.VMNotFound(vmCID)
		}
		return "", "", cpierrors.Wrap(pve.WrapError(prepErr), fmt.Sprintf("%s: slot selection for VM %s disk %s", op, vmCID, diskCID))
	}

	// Compute effective per-disk performance options (see doc comment).
	globalOpts, gerr := attachDiskGlobalPerfOpts(ctx, deps, rd.volid, rd.meta)
	if gerr != nil {
		return "", "", gerr
	}
	var metaOpts map[string]string
	if rd.meta != nil {
		metaOpts = rd.meta.Opts
	}
	// Merge order: global < CID-recorded opts < recorded operator overrides
	// (rightmost wins; an empty-string value deletes the key). The overrides
	// layer applies to legacy disks too — a bare CID (meta nil) can still
	// carry an overlay recorded by update_disk.
	effectiveOpts := filterDiskPerfForBus(mergeDiskOptions(mergeDiskOptions(globalOpts, metaOpts), plan.overlay), bus)
	// CID opts are a mixed namespace: CPI-internal provenance keys (e.g.
	// retain_on_delete) ride in meta.Opts but are not PVE drive options, and
	// PVE rejects the whole config write on any key outside its drive schema.
	stripCPIInternalDiskOpts(effectiveOpts)

	// Enforce creation-time disk-performance invariants (§7.26), before any
	// mutating PVE call so an enforce-mode reject leaves no orphan.
	if err := enforceDiskPerfInvariants(deps.Config, deps.Log(ctx), op, vmCID, diskCID, rd.meta, plan.overlay, effectiveOpts); err != nil {
		return "", "", err
	}

	// The stable ID rides the drive entry as its serial: a genuine PVE drive
	// key, injected after the invariant check (it is identity, not a
	// performance option) and at an attach boundary, the only point D13
	// permits writing one.
	if rd.stableID != "" {
		effectiveOpts["serial"] = rd.stableID
	}

	if plan.viaTransfer {
		return attachDiskViaTransfer(ctx, deps, op, vmCID, node, vmid, diskCID, rd, plan, desiredDiskID, effectiveOpts)
	}
	return attachDiskConfigPut(ctx, deps, op, vmCID, node, vmid, diskCID, rd, desiredDiskID, effectiveOpts)
}

// attachDiskConfigPut is the config-edit attach tail: the volid (plus baked
// options) is written into the target slot by a config PUT. The only path for
// legacy disks, and the fallback for stable-ID disks whose reassignment is
// not available (cross-node parker with a birth-named volume, or a snapshot
// refusal).
func attachDiskConfigPut(
	ctx context.Context,
	deps Deps,
	op string,
	vmCID string,
	node string,
	vmid int,
	diskCID string,
	rd resolvedDisk,
	desiredDiskID string,
	effectiveOpts map[string]string,
) (diskID, devicePath string, err error) {
	const bus = "scsi"

	// Build the volid arg: bake options in when present, bare CID otherwise.
	volidArg := rd.volid
	if len(effectiveOpts) > 0 {
		volidArg = buildDiskOptStr(rd.volid, effectiveOpts)
	}

	err = pve.RetryOnTransient(ctx, deps.Log(ctx), op, 0, func() error {
		var attachErr error
		diskID, attachErr = deps.PVE.QEMU().AttachDisk(ctx, node, vmid, volidArg, bus, &qemu.AttachOpts{
			DiskID: desiredDiskID,
		})
		return attachErr
	})
	if err != nil {
		wrapped := pve.WrapError(err)
		if pve.IsNotFound(err) {
			return "", "", cpierrors.VMNotFound(vmCID)
		}
		return "", "", cpierrors.Wrap(wrapped, fmt.Sprintf("%s: AttachDisk failed for VM %s disk %s", op, vmCID, diskCID))
	}

	// Confirm attachment (resolve diskID) and derive device path.
	devicePath, err = attachDiskConfirmAndPath(ctx, deps, vmCID, node, vmid, rd.volid, diskID, deps.Log(ctx))
	if err != nil {
		return "", "", err
	}

	// Record the Director's verbatim disk_cid on the VM's description
	// sentinel — keyed by the stable ID when the disk has one, the volid
	// otherwise — so a later get_disks call can return this exact string
	// instead of the bare volid: cloudcheck membership fidelity (see
	// pve.UpdateAttachedDiskCID doc comment). Best-effort: never fails the
	// attach.
	pve.UpdateAttachedDiskCID(ctx, deps.PVE, deps.Log(ctx), node, vmid, rd.sentinelKey(), diskCID)

	return diskID, devicePath, nil
}

// attachDiskViaTransfer attaches a parked stable-ID disk by move_disk
// reassignment: the volume moves from the parker slot straight onto the
// target VM's slot, renamed for its new owner, with the full drive option
// string (serial included) riding along. The receiving side's record (the
// holder sentinel) is written before the giving side's (the parker
// provenance entry) is removed — D13's crash-window ordering.
//
// A snapshot refusal from PVE falls back to the config-edit path when the
// volume is not named for the parker (the unpark cannot deallocate it); a
// parker-named volume has no safe fallback and returns a hard error naming
// the snapshot as the thing to remove.
func attachDiskViaTransfer(
	ctx context.Context,
	deps Deps,
	op string,
	vmCID string,
	node string,
	vmid int,
	diskCID string,
	rd resolvedDisk,
	plan attachPlan,
	targetSlot string,
	effectiveOpts map[string]string,
) (string, string, error) {
	preVolid := rd.volid
	optStr := buildDiskOptStr(preVolid, effectiveOpts)
	parkerCfg := parkerReadConfigFor(deps)

	landed, terr := pve.TransferDiskFromParker(ctx, deps.PVE, deps.Log(ctx), plan.parker, vmid, targetSlot, preVolid, optStr, parkerCfg)
	if terr != nil {
		if errors.Is(terr, pve.ErrMoveDiskSnapshotRefused) {
			if embedded, ok := pve.EmbeddedDiskVMID(preVolid); ok && embedded == plan.parker.VMID {
				return "", "", cpierrors.Cloud(
					"%s: disk %s is parked on parker VM %d as %q and a snapshot references it, which blocks the "+
						"reassignment transfer; the config-edit fallback would let PVE deallocate a volume its parker "+
						"owns, so there is no safe automatic path. Remove the snapshot referencing the volume, then retry",
					op, diskCID, plan.parker.VMID, preVolid,
				)
			}
			deps.Log(ctx).Warn(op+": reassignment refused because a snapshot references the disk; falling back to the config-edit attach",
				log.String("disk_cid", diskCID),
				log.Int("parker_vmid", plan.parker.VMID),
			)
			if unErr := pve.UnparkDiskAt(ctx, deps.PVE, deps.Log(ctx), preVolid, plan.parker, parkerCfg); unErr != nil {
				return "", "", retriableUnlessPermanent(unErr, fmt.Sprintf("%s: unpark disk %s (snapshot fallback)", op, diskCID))
			}
			return attachDiskConfigPut(ctx, deps, op, vmCID, node, vmid, diskCID, rd, targetSlot, effectiveOpts)
		}
		return "", "", retriableUnlessPermanent(terr, fmt.Sprintf("%s: reassign disk %s from parker to VM %s", op, diskCID, vmCID))
	}
	rd.volid = landed

	devicePath, err := attachDiskConfirmAndPath(ctx, deps, vmCID, node, vmid, landed, targetSlot, deps.Log(ctx))
	if err != nil {
		return "", "", err
	}

	// Receiving side first: record the Director's CID on the VM, then drop
	// the parker's provenance entry (matched by the pre-move volid it
	// recorded). Both best-effort — the drive serial is the authoritative
	// carrier by this point.
	pve.UpdateAttachedDiskCID(ctx, deps.PVE, deps.Log(ctx), node, vmid, rd.sentinelKey(), diskCID)
	pve.RemoveParkerProvenanceEntry(ctx, deps.PVE, deps.Log(ctx), plan.parker.Node, plan.parker.VMID, preVolid, parkerCfg)

	deps.Log(ctx).Info(op+": persistent disk attached by reassignment",
		log.String("vm_cid", vmCID),
		log.String("disk_cid", diskCID),
		log.String("volid_before", preVolid),
		log.String("volid_after", landed),
		log.String("disk_id", targetSlot),
	)
	return targetSlot, devicePath, nil
}

// attachDiskGlobalPerfOpts resolves the global (no call-level cloud_properties)
// disk-performance options for the disk identified by bareDiskCID, extracted
// from HandleAttachDisk to keep that function's cognitive complexity under
// the project threshold.
//
// discard/ssd auto-resolution needs the target pool's storage type and
// disk-image format (see resolveDiskPerfOptions). The pool name comes from
// the disk CID itself. The format prefers the value recorded in the CID
// envelope at create_disk time (DiskCIDMeta.Format), so the disk keeps the
// format it was created under even when vm_disk_format has changed since;
// legacy CIDs carry no format, and for those this falls back to the CPI's
// configured default — the same fallback create_disk applies when no
// per-call format is given. The storage-type lookup only runs when
// auto-resolution would actually consult it
// (needsDiskPerfStorageTypeLookup) — an operator who explicitly
// configures both discard and ssd never pays for the extra API round trip
// on every attach_disk call. A failed/unresolvable lookup fails open to ""
// (not TRIM-capable), matching resolveDiskPerfOptions's documented
// fail-open contract — never an error from this function on that account.
func attachDiskGlobalPerfOpts(ctx context.Context, deps Deps, bareDiskCID string, meta *pve.DiskCIDMeta) (map[string]string, error) {
	globalR, gerr := newLayeredResolver(nil, deps.Config)
	if gerr != nil {
		return nil, gerr
	}
	var storageType string
	if needsDiskPerfStorageTypeLookup(globalR, deps.Config) {
		if storageName, _, parseErr := pve.ParseDiskCID(bareDiskCID); parseErr == nil {
			storageType = lookupVMStorageType(ctx, deps, storageName)
		}
	}
	format := diskFormatQCOW2
	if deps.Config != nil && deps.Config.VMDiskFormat != "" {
		format = deps.Config.VMDiskFormat
	}
	if meta != nil && meta.Format != "" {
		format = meta.Format
	}
	return resolveDiskPerfOptions(globalR, deps.Config, storageType, format)
}

// enforceDiskPerfInvariants applies the §7.26 creation-time disk-performance
// invariant policy at attach_disk time. Structural options (cache/iothread/ssd)
// recorded in the disk CID at create_disk time must not change on re-attach. The
// §7.9 merge already pins CID-recorded values (per-disk wins over global), so the
// only way an invariant diverges is when global config has introduced a
// structural option the disk lacked at creation.
//
// Opt-in by data presence: a disk whose CID carries no performance options
// (bare/legacy CID, meta == nil) is skipped, so behavior is byte-identical for
// those disks regardless of mode. On divergence the disk_perf_invariant_mode
// knob decides: enforce (default) → non-retriable CloudError; warn → log and
// proceed; off → skip.
//
// The comparison baseline is the creation-time options with the disk's
// recorded operator overrides layered on top: an option an operator updated
// through update_disk is an intended change, not a divergence, while a global
// config drift the disk never opted into still trips the guard.
//
// Ordering note: this runs after chooseSCSISlotSkippingZero, which may detach a
// legacy scsi0 attachment (a config PUT) before this check. That migration is
// data-loss-safe (a scsi0 persistent disk was never successfully partitioned by
// the agent) and idempotent, so an enforce-mode reject after it leaves no
// orphaned data — only a slot freed for the (now rejected) re-attach. The
// reject still precedes the AttachDisk that would bind the volume.
//
// A nil cfg is safe: DiskPerfInvariantModeValue defaults a nil receiver to
// enforce, so a divergence with no config is rejected (fail-closed).
func enforceDiskPerfInvariants(
	cfg *config.CPIConfig,
	logger *log.Logger,
	op string,
	vmCID, diskCID string,
	meta *pve.DiskCIDMeta,
	overlay map[string]string,
	effectiveOpts map[string]string,
) error {
	if meta == nil || len(meta.Opts) == 0 {
		return nil
	}
	violations := diskPerfInvariantViolations(mergeDiskOptions(meta.Opts, overlay), effectiveOpts)
	if len(violations) == 0 {
		return nil
	}

	switch cfg.DiskPerfInvariantModeValue() {
	case "off":
		return nil
	case "warn":
		logger.Warn(op+": disk-performance invariant divergence (warn mode — proceeding)",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("violations", strings.Join(violations, "; ")),
		)
		return nil
	default: // "enforce"
		return cpierrors.Cloud(
			"%s: disk-performance invariant divergence for disk %s: %s."+
				" The disk's structural options changed since create_disk."+
				" Align CPI disk_performance config to the disk's creation-time options,"+
				" or set pve.disk_perf_invariant_mode=warn|off to bypass this guard.",
			op, diskCID, strings.Join(violations, "; "),
		)
	}
}

// attachDiskParseArgs unmarshals and validates the two positional attach_disk
// arguments. Returns (vmCID, diskCID, err).
//
// Failures:
//   - len(args) < 2           → Cloud error (missing args)
//   - args[0] not a JSON string → Wrap error
//   - vmCID == ""             → Cloud error
//   - args[1] not a JSON string → Wrap error
//   - diskCID == ""           → Cloud error
func attachDiskParseArgs(args []json.RawMessage) (vmCID, diskCID string, err error) {
	if len(args) < 2 {
		return "", "", cpierrors.Cloud("attach_disk: expected 2 arguments (vm_cid, disk_cid), got %d", len(args))
	}

	if err := json.Unmarshal(args[0], &vmCID); err != nil {
		return "", "", cpierrors.Wrap(err, "attach_disk: args[0] vm_cid must be a string")
	}
	if vmCID == "" {
		return "", "", cpierrors.Cloud("attach_disk: args[0] vm_cid must not be empty")
	}

	if err := json.Unmarshal(args[1], &diskCID); err != nil {
		return "", "", cpierrors.Wrap(err, "attach_disk: args[1] disk_cid must be a string")
	}
	if diskCID == "" {
		return "", "", cpierrors.Cloud("attach_disk: args[1] disk_cid must not be empty")
	}

	return vmCID, diskCID, nil
}

// attachDiskResolveNode parses vmCID to a VMID, parses diskCID to its storage
// component, resolves the storage backend, and determines which cluster node
// holds the disk. For local backends it also verifies VM/disk co-location.
//
// Returns (node, vmid, err).
//
// Failures:
//   - vmCID not a positive integer        → VMNotFound
//   - diskCID not in "<storage>:<volid>"  → DiskNotFound
//   - backend resolution error            → Wrap(Cloud)
//   - NodeForExisting: not-found          → DiskNotFound
//   - NodeForExisting: other error        → Wrap
//   - local backend + node mismatch       → Cloud error
func attachDiskResolveNode(ctx context.Context, deps Deps, vmCID, diskCID string) (node string, vmid int, err error) {
	vmid, err = strconv.Atoi(vmCID)
	if err != nil || vmid <= 0 {
		return "", 0, cpierrors.VMNotFound(vmCID)
	}

	storage, _, err := pve.ParseDiskCID(diskCID)
	if err != nil {
		return "", 0, cpierrors.DiskNotFound(diskCID)
	}
	// The storage pool is recovered directly from the volid prefix (e.g.
	// "data" in "data:vm-9003-disk-0") — it was baked in at create_disk
	// time. attach_disk therefore routes to exactly the pool that held the
	// disk at creation without any extra metadata lookup. This is the
	// stickiness guarantee: a disk placed on storage pool X at create_disk
	// is always re-attached from pool X across every director round-trip,
	// because the volid itself carries the pool name as its prefix.

	// Resolve disk's owning node via the backend abstraction. For shared
	// backends this is the configured default; for local backends a
	// cluster scan locates the node that holds the volume. attach_disk
	// then runs the QEMU config PUT against that node. PVE's storage
	// content endpoint wants the canonical "<storage>:<volname>" form,
	// which is the disk_cid as-is.
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return "", 0, cpierrors.Wrap(err, fmt.Sprintf("attach_disk: backend resolution failed for storage %q", storage))
	}
	node, err = backend.NodeForExisting(ctx, diskCID)
	if err != nil {
		if pve.IsNotFound(err) {
			return "", 0, cpierrors.DiskNotFound(diskCID)
		}
		return "", 0, cpierrors.Wrap(err, "attach_disk: node lookup failed")
	}

	// The QEMU config operations that follow (snapshot guard, slot
	// selection, the attach PUT) must target the node that RUNS the VM —
	// PVE serves /nodes/<n>/qemu/<vmid>/config only from the owning node.
	// For shared backends NodeForExisting returns the configured default
	// node, which is a storage routing hint, not the VM's location: on a
	// multi-node cluster the VM may live elsewhere ("Configuration file
	// ... does not exist"). The cluster scan is authoritative; use it.
	//
	// For local backends the disk and VM MUST additionally live on the
	// same node — the SDK call would otherwise PUT a config update on a
	// node that cannot see the volume, producing a confusing storage
	// error. Verify co-location explicitly and surface a clear message
	// when violated.
	vmNode, found, lookupErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, vmid)
	if lookupErr != nil {
		return "", 0, cpierrors.Wrap(lookupErr, fmt.Sprintf("attach_disk: lookup VM %s node failed", vmCID))
	}
	if backend.Kind() == pve.BackendLocal {
		if found && vmNode != "" && vmNode != node {
			return "", 0, cpierrors.Cloud(
				"attach_disk: local-backend disk %s lives on node %s but VM %s runs on node %s — local-storage disks cannot cross nodes",
				diskCID, node, vmCID, vmNode,
			)
		}
	} else if found && vmNode != "" {
		node = vmNode
	}
	// VM absent from /cluster/resources: keep the backend-derived node so
	// the subsequent config fetch surfaces PVE's own not-found error with
	// unchanged semantics (rather than guessing here).

	return node, vmid, nil
}

// attachDiskSnapshotGuard runs the snapshot pre-flight check. See the policy
// comment in HandleAttachDisk step 3 for full semantics.
//
// Failures:
//   - HasSnapshots error + cfg.RequireSnapshotCheckPass  → Wrap error (fail-closed)
//   - HasSnapshots error + !cfg.RequireSnapshotCheckPass → nil (WARN + proceed)
//   - snapshots present + !cfg.AllowDiskOpsWithSnapshots → Cloud error (hard fail)
//   - snapshots present + cfg.AllowDiskOpsWithSnapshots  → nil (WARN + proceed)
//   - no snapshots                                       → nil
func attachDiskSnapshotGuard(ctx context.Context, deps Deps, vmCID, node string, vmid int, cfg *config.CPIConfig, logger *log.Logger) error {
	snapNames, snapErr := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if snapErr != nil {
		if cfg.RequireSnapshotCheckPass {
			return cpierrors.Wrap(snapErr,
				"attach_disk: snapshot pre-flight check failed and require_snapshot_check_pass is set",
			)
		}
		logger.Warn("attach_disk: snapshot pre-flight check failed — proceeding (fail-open)",
			log.String("node", node),
			log.Int("vmid", vmid),
			log.Err(snapErr),
		)
		return nil
	}
	if len(snapNames) > 0 {
		if cfg.AllowDiskOpsWithSnapshots {
			logger.Warn("attach_disk: proceeding despite snapshots (allow_disk_ops_with_snapshots=true)",
				log.String("vm_cid", vmCID),
				log.String("node", node),
				log.String("snapshots", strings.Join(snapNames, ", ")),
			)
			return nil
		}
		return cpierrors.Cloud(
			"attach_disk: VM %s (node %s) has %d snapshot(s) [%s]: attaching a persistent disk while"+
				" snapshots exist makes the disk invisible in all prior snapshot rollbacks."+
				" Delete all snapshots before attaching persistent disks, or set"+
				" pve.allow_disk_ops_with_snapshots=true in CPI config to bypass this guard.",
			vmCID, node, len(snapNames), strings.Join(snapNames, ", "),
		)
	}
	return nil
}

// attachDiskConfirmAndPath resolves the canonical diskID from the current VM
// config (step 6) then derives the PVE-stable device path (step 7).
//
// If ResolveDiskID fails after a successful AttachDisk, the function falls back
// to the diskID returned by AttachDisk and logs a warning — the device path is
// still valid because devicePathByID is a pure function of the slot index.
//
// Failures:
//   - devicePathByID error (non-scsi diskID) → Wrap error
func attachDiskConfirmAndPath(ctx context.Context, deps Deps, vmCID, node string, vmid int, diskCID, fallbackDiskID string, logger *log.Logger) (devicePath string, err error) {
	// --------------------------------------------------------------------
	// 6. Confirm attachment by resolving diskID from current VM config.
	//    This guards against edge cases where AttachDisk returns stale data.
	// --------------------------------------------------------------------
	resolvedDiskID, resolveErr := pve.ResolveDiskID(ctx, deps.PVE, node, vmid, diskCID)
	if resolveErr != nil {
		// ResolveDiskID failure after a successful AttachDisk is unexpected.
		// Use the diskID returned by AttachDisk as fallback; log the anomaly.
		logger.Warn("attach_disk: ResolveDiskID failed after successful attach; using diskID from AttachDisk",
			log.String("vm_cid", vmCID),
			log.String("disk_cid", diskCID),
			log.String("fallback_disk_id", fallbackDiskID),
			log.Err(resolveErr),
		)
		resolvedDiskID = fallbackDiskID
	}

	// --------------------------------------------------------------------
	// 7. Derive device path from diskID.
	//
	// We use a PVE-stable by-id symlink rather than a "/dev/sd<X>" hint
	// because BOSH agent's mappedDevicePathResolver substitutes /dev/sd
	// prefixes with /dev/vd before falling back to /dev/sd (see
	// infrastructure/devicepathresolver/mapped_device_path_resolver.go).
	// A virtio root disk (/dev/vda) on this VM makes "/dev/sda" resolve
	// to the root disk; the agent then runs persistent-disk partitioning
	// against /dev/vda and fails with
	//     "Persistent disks with many partitions are not supported.
	//      Expected 1, got 4."
	//
	// PVE configures virtio-scsi-pci disks with QEMU disk serial
	// "drive-scsi<N>" (where N is the slot index), and udev creates the
	// symlink "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>"
	// pointing to whatever /dev/sd<X> letter the guest kernel assigned.
	// Because this path does not start with "/dev/sd", the agent's
	// resolver skips its substitution table, follows the symlink, and
	// returns the correct device.
	// --------------------------------------------------------------------
	devicePath, devErr := devicePathByID(resolvedDiskID)
	if devErr != nil {
		return "", cpierrors.Wrap(devErr, fmt.Sprintf("attach_disk: cannot compute device path for diskID %q", resolvedDiskID))
	}

	return devicePath, nil
}

// chooseSCSISlotSkippingZero returns the diskID the persistent disk should be
// attached at. It guarantees scsi0 is never used so the BOSH agent's
// mappedDevicePathResolver does not silently resolve a "/dev/sda" hint to the
// virtio root disk (/dev/vda).
//
// Behavior:
//
//   - If volid is already present in the VM config at scsiN with N >= 1, that
//     diskID is reused (idempotent reattach).
//   - If volid is present at scsi0 (legacy from prior CPI versions), the
//     attachment is removed and a fresh scsi index >= 1 is chosen. Persistent
//     disks orphaned at scsi0 have, by construction, never been successfully
//     partitioned by the agent (the resolver always picked /dev/vda instead),
//     so detaching them loses no data.
//   - Otherwise the lowest free scsi index >= 1 is returned.
func chooseSCSISlotSkippingZero(
	ctx context.Context,
	deps Deps,
	op string,
	node string,
	vmid int,
	volid string,
) (string, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", err
	}

	if existing, ok := pve.FindDiskIDByVolID(qemu.ParseDisks(cfg), volid); ok {
		if existing != "scsi0" {
			return existing, nil
		}
		// Legacy scsi0 attachment from a prior CPI version. Detach so the
		// reattach below lands on scsi1+. DetachDisk also sweeps the
		// resulting unusedN entry, leaving the config clean.
		deps.Log(ctx).Warn(op+": migrating legacy scsi0 attachment to scsi1+",
			log.Int("vmid", vmid),
			log.String("volid", volid),
		)
		if detachErr := deps.PVE.QEMU().DetachDisk(ctx, node, vmid, "scsi0"); detachErr != nil {
			return "", cpierrors.Wrap(detachErr, op+": detach legacy scsi0")
		}
		// Re-read config so NextFreeSCSIIndexAtLeast sees scsi0 as free.
		cfg, err = deps.PVE.QEMU().Config(ctx, node, vmid)
		if err != nil {
			return "", cpierrors.Wrap(err, op+": re-read config after scsi0 detach")
		}
	}

	idx := nextFreeSCSIIndexAtLeast(cfg, 1)
	return fmt.Sprintf("scsi%d", idx), nil
}

// devicePathByID returns the PVE-stable udev by-id symlink path for the
// disk attached at diskID. PVE configures virtio-scsi-pci disks with the
// QEMU disk serial "drive-scsi<N>" (where N is the slot index from the
// "scsiN" key), and udev's 60-persistent-storage.rules creates a symlink
//
//	/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi<N>
//
// pointing to whatever /dev/sd<X> the guest kernel assigned. Returning
// this path (instead of a guessed /dev/sd<X>) bypasses BOSH agent's
// mappedDevicePathResolver substitution that would otherwise collide with
// the virtio root disk.
//
// Returns an error if diskID is not a scsi slot.
func devicePathByID(diskID string) (string, error) {
	var idx int
	if _, err := fmt.Sscanf(diskID, "scsi%d", &idx); err != nil {
		return "", cpierrors.Cloud("attach_disk: diskID %q is not a scsi slot", diskID)
	}
	return fmt.Sprintf("/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi%d", idx), nil
}

// attachPlan is the pre-attach holder guard's verdict on how the volume
// reaches the target VM.
type attachPlan struct {
	// viaTransfer is true when the disk sits on a same-node parker and moves
	// by move_disk reassignment (stable-ID disks only). The guard leaves such
	// a disk parked; attachDiskCore performs the transfer.
	viaTransfer bool
	// parker is the holder the transfer moves the disk off; meaningful only
	// when viaTransfer is true.
	parker pve.DiskHolder
	// overlay is the disk's recorded drive-option overrides, read from
	// wherever the disk currently lives (a parker's provenance entry, or the
	// target VM's own sentinel on an idempotent re-attach). attachDiskCore
	// merges it as the rightmost layer over global and CID options.
	overlay map[string]string
}

// guardAndUnparkBeforeAttach settles who currently holds the volume, then
// acts: a parker holding a stable-ID disk on the target's node is planned for
// a reassignment transfer (and left in place for attachDiskCore to move), any
// other parker holder is unparked, the target VM itself is left alone (the
// attach that follows is idempotent), and any other VM is a hard stop. An
// interrupted detach-side transfer is resumed first, so the disk the rest of
// this function reasons about is in a settled state.
//
// The holder scan is unconditional, unlike the unpark it gates. PVE will happily
// let two VM configs reference one volume; nothing downstream detects it, and
// the damage only surfaces when whichever holder is destroyed takes the volume
// out from under the other. The parked strategy makes that state reachable
// without anyone doing anything wrong — the parker band resolves under every
// strategy, so setting detached_disk_strategy to "free" no longer hides a
// parker, but a band moved away from live parkers still does, and so does a
// release rolled back to one that has no concept of parking. A single cluster
// scan per attach_disk is a cheap price for turning silent volume loss into a
// message naming the VM to look at. For stable-ID disks the identity
// resolution already paid that scan, so the holder comes from rd.
func guardAndUnparkBeforeAttach(ctx context.Context, deps Deps, op string, rd *resolvedDisk, node string, targetVMID int) (attachPlan, error) {
	if deps.Config == nil {
		return attachPlan{}, nil
	}
	refreshed, resumeErr := resumeTransferIfNeeded(ctx, deps, op, *rd)
	if resumeErr != nil {
		return attachPlan{}, resumeErr
	}
	*rd = refreshed

	parkerCfg := parkerReadConfigFor(deps)
	var holder pve.DiskHolder
	if rd.stableID != "" {
		if rd.holder != nil {
			holder = *rd.holder
		}
	} else {
		var err error
		holder, err = pve.ResolveDiskHolder(ctx, deps.PVE, deps.Log(ctx), rd.volid, parkerCfg)
		if err != nil {
			return attachPlan{}, wrapHolderScanError(err, fmt.Sprintf("%s: resolve current holder of disk %s", op, rd.diskCID))
		}
	}

	// A disk whose CID promises a parker anchor must have a holder while
	// detached; no holder at all means the parker vanished out-of-band.
	if anchorErr := anchorMissingRefusal(ctx, deps, op, rd.diskCID, rd.meta, holder); anchorErr != nil {
		return attachPlan{}, anchorErr
	}

	if holder.Found && !holder.IsParker && holder.VMID != targetVMID {
		return attachPlan{}, foreignHolderError(deps, op, rd.diskCID, targetVMID, holder)
	}

	// The disk's recorded drive-option overrides ride wherever it lives; read
	// them now, and when they ride a parker, record them on the receiving VM
	// before anything can remove the parker's record (the same
	// receiving-side-first ordering D13 gives every transfer).
	overlay, ovErr := attachOverlayForHolder(ctx, deps, op, rd, holder, node, targetVMID)
	if ovErr != nil {
		return attachPlan{}, ovErr
	}

	if holder.Found && holder.IsParker && rd.stableID != "" {
		if holder.Node == node {
			return attachPlan{viaTransfer: true, parker: holder, overlay: overlay}, nil
		}
		// PVE reassignment is same-node only ("Both VMs need to be on the
		// same node"). A birth-named volume can still take the config-edit
		// path safely — the unpark preserves a volume the parker does not
		// own — so cross-node attach of a fresh parked disk works as it
		// always has. A parker-NAMED volume cannot: the config-edit unpark's
		// sweep would let PVE deallocate it, so it is refused with the two
		// operator escapes.
		if embedded, ok := pve.EmbeddedDiskVMID(rd.volid); ok && embedded == holder.VMID {
			return attachPlan{}, cpierrors.Cloud(
				"%s: disk %s is parked as %q on parker VM %d (node %s) but VM %s runs on node %s, and PVE "+
					"reassignment is same-node only. Either migrate the parker to the VM's node "+
					"(qm migrate %d %s — parkers are always stopped) and retry, or recreate the VM pinned to "+
					"node %s (cloud_properties.node or an AZ on that node)",
				op, rd.diskCID, rd.volid, holder.VMID, holder.Node, strconv.Itoa(targetVMID), node,
				holder.VMID, node, holder.Node,
			)
		}
		deps.Log(ctx).Warn(op+": stable-ID disk is parked on another node; using the config-edit attach path (birth-named volume, safe)",
			log.String("disk_cid", rd.diskCID),
			log.Int("parker_vmid", holder.VMID),
			log.String("parker_node", holder.Node),
			log.String("vm_node", node),
		)
	}

	// UnparkDiskAt reuses the holder just resolved, so the parked path still
	// costs one cluster scan rather than the two a plain UnparkDisk would.
	if unErr := pve.UnparkDiskAt(ctx, deps.PVE, deps.Log(ctx), rd.volid, holder, parkerCfg); unErr != nil {
		// Keep the class the unpark chose. Most of what it returns is transport
		// shaped and retriable, but two of its outcomes are not: a permission
		// the operator has to grant, and a detach that succeeded while leaving
		// an unusedN reference behind. Flattening those into a retriable makes
		// the Director drive the first forever, and drive the second straight
		// into attaching a volume the parker still references.
		return attachPlan{}, retriableUnlessPermanent(unErr, fmt.Sprintf("%s: unpark disk %s", op, rd.diskCID))
	}
	return attachPlan{overlay: overlay}, nil
}

// attachOverlayForHolder reads the disk's recorded drive-option overrides
// from its current holder. For a parker holder it also records a non-empty
// result on the receiving VM's own sentinel before returning, so the
// receiving side carries the overrides before the unpark or transfer removes
// the parker's provenance entry — a crash mid-attach then leaves at least one
// carrier. A stale entry on a VM the attach never completes onto is harmless:
// it is keyed by the disk and overwritten by the next successful attach.
//
// Fail-closed on read: concluding "no overrides" from a read that never
// arrived is exactly the silent revert the record exists to prevent, and the
// Director retries a failed attach safely.
func attachOverlayForHolder(ctx context.Context, deps Deps, op string, rd *resolvedDisk, holder pve.DiskHolder, node string, targetVMID int) (map[string]string, error) {
	if !holder.Found {
		return nil, nil
	}
	if holder.IsParker {
		overlay, err := pve.ReadParkerDiskOverlay(ctx, deps.PVE, holder.Node, holder.VMID, rd.volid, rd.stableID)
		if err != nil {
			return nil, retriableUnlessPermanent(err,
				fmt.Sprintf("%s: read recorded option overrides for disk %s", op, rd.diskCID))
		}
		if len(overlay) == 0 {
			return nil, nil
		}
		if setErr := pve.SetVMDiskOptOverlay(ctx, deps.PVE, node, targetVMID, rd.sentinelKey(), overlay, rd.volid, rd.birth); setErr != nil {
			if pve.IsNotFound(setErr) {
				return nil, cpierrors.VMNotFound(strconv.Itoa(targetVMID))
			}
			return nil, retriableUnlessPermanent(setErr,
				fmt.Sprintf("%s: record option overrides for disk %s on VM %d", op, rd.diskCID, targetVMID))
		}
		return overlay, nil
	}
	if holder.VMID == targetVMID {
		// Idempotent re-attach: the overrides already live on the target.
		overlay, err := pve.GetVMDiskOptOverlay(ctx, deps.PVE, holder.Node, holder.VMID, rd.stableID, rd.volid, rd.birth)
		if err != nil {
			return nil, retriableUnlessPermanent(err,
				fmt.Sprintf("%s: read recorded option overrides for disk %s", op, rd.diskCID))
		}
		return overlay, nil
	}
	return nil, nil
}

// foreignHolderError builds the refusal for a volume already attached to a VM
// other than the target, separating the two cases an operator handles
// differently: a parker stranded outside the configured band by a strategy or
// band change, and a genuine second attachment.
func foreignHolderError(deps Deps, op, diskCID string, targetVMID int, holder pve.DiskHolder) error {
	if strandedErr := strandedParkerRefusal(deps, op, diskCID, holder); strandedErr != nil {
		return strandedErr
	}
	return cpierrors.Cloud(
		"%s: disk %s is already attached to VM %d (node %s) and cannot be attached to VM %d as well; "+
			"attaching one volume to two VMs corrupts it. Detach it from VM %d first",
		op, diskCID, holder.VMID, holder.Node, targetVMID, holder.VMID,
	)
}

// nextFreeSCSIIndexAtLeast returns the lowest scsi slot index >= floor that is
// not present in cfg. Mirrors qemu.NextIndexForBus semantics but with a
// configurable floor so the caller can reserve low-numbered slots.
func nextFreeSCSIIndexAtLeast(cfg map[string]any, floor int) int {
	used := map[int]bool{}
	for k := range cfg {
		var idx int
		if _, scanErr := fmt.Sscanf(k, "scsi%d", &idx); scanErr == nil {
			used[idx] = true
		}
	}
	for i := floor; ; i++ {
		if !used[i] {
			return i
		}
	}
}
