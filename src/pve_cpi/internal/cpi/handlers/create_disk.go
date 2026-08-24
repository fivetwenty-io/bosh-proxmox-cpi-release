package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// createDiskCloudProperties holds the recognized fields from the cloud_properties
// argument of create_disk. All fields are optional; missing fields fall back to
// CPI configuration defaults.
type createDiskCloudProperties struct {
	// StoragePool is the preferred storage pool for this disk. Takes highest
	// precedence in the storage resolution chain:
	//   disk cloud_properties.storage_pool
	//     → disk cloud_properties.storage  (backward-compat alias)
	//       → cloud_properties.storage_tier (live criteria match)
	//         → config.DiskStorage          (global default)
	StoragePool string `json:"storage_pool"`
	// Storage is the backward-compatible alias for StoragePool. Manifests that
	// already set cloud_properties.storage continue to work unchanged.
	Storage string `json:"storage"`
	// StorageTier selects a storage pool by matching live PVE cluster storage
	// attributes against config.StorageTiers[StorageTier]. Only consulted when
	// neither storage_pool nor storage is set. Empty (default) disables tier
	// resolution and preserves byte-identical behavior to prior releases.
	StorageTier string `json:"storage_tier,omitempty"`
	// DiskFormat overrides deps.Config.VMDiskFormat for this disk.
	DiskFormat string `json:"disk_format"`
	// Node pins the new disk to a specific PVE node. Required-on-local-backend
	// only when no vm_cid hint can resolve the owner node; for shared backends
	// this is just a placement preference.
	Node string `json:"node"`
	// Tags is an operator-supplied map applied to the tags field of the VM
	// the disk is attached to. PVE has no native disk-volume tag field, so
	// tags ride on the hosting VM plus a sentinel record in its description
	// (see applyCustomTagsToVM). Tags are deferred when create_disk has no
	// vm_cid hint; set_disk_metadata applies them on the next sync.
	Tags map[string]string `json:"tags"`
	// AvailabilityZone records the AZ label for this disk at create time. When
	// non-empty it is encoded into the disk CID metadata so create_vm can use it
	// for fault-domain co-location: shared-storage disks in a given AZ constrain
	// the VM to that AZ, preventing cross-AZ attachment. Empty (default) imposes
	// no AZ constraint; create_vm placement proceeds unconstrained by this disk.
	AvailabilityZone string `json:"availability_zone,omitempty"`
	// Encrypted is the per-call opt-in for encrypted storage placement (§7.49).
	// When *true, storage-tier selection is restricted to tiers marked Encrypted:*true
	// in config.StorageTiers. Overrides the global CPIConfig.Encrypted (per-call >
	// global > off). When nil, the global setting applies. When *false, encrypted
	// filter is explicitly disabled even if global is true.
	Encrypted *bool `json:"encrypted,omitempty"`
	// RetainOnDelete opts this disk out of deletion when delete_vm is called.
	// When *true, "retain_on_delete":"1" is encoded into DiskCIDMeta.Opts so that
	// delete_vm (and any future GC) can read the intent from the disk CID alone,
	// independent of the VM config. Persistent disks created by create_disk are
	// already preserved on delete_vm by the foreign-VMID guard; this flag adds
	// explicit provenance and audit trail so the WARN log can identify the disk as
	// operator-retained rather than incidentally foreign. Nil → byte-identical CID.
	RetainOnDelete *bool `json:"retain_on_delete,omitempty"`
}

// resolveStorageForDisk resolves the target storage pool for a create_disk call.
//
// When encrypted=false (the default, unset path): byte-identical to pre-§7.49 —
// precedence order (first non-empty wins):
//  1. r.String("storage_pool","storage") — explicit per-call or profile override
//  2. r.String("storage_tier") — live criteria match against cluster storages
//  3. deps.Config.DiskStorage — global config default
//
// When encrypted=true: §7.49 enforcement applies at every level:
//  1. Explicit storage_pool/storage present → non-retriable CloudError
//     (CPI cannot verify an arbitrary named pool is encrypted).
//  2. storage_tier named → tier must be marked Encrypted:*true in config
//     (existing named-tier-not-encrypted error, already in resolveStorageTier).
//  3. No tier and no pool → auto-select: lex-first tier in config with Encrypted:*true;
//     run it through resolveStorageTier (Types/Shared predicates + live query).
//     No encrypted tier in config → non-retriable CloudError.
//  4. Global DiskStorage with encrypted=true and no tier path taken → same auto-select.
//     If ClusterStorage is not wired and no tier resolves → non-retriable CloudError.
//
// All paths log a warning when an encrypted tier is selected (operator responsibility).
func resolveStorageForDisk(ctx context.Context, r *layeredResolver, deps Deps, encrypted bool) (string, error) {
	hasExplicitPool := func() bool {
		_, found := r.String("storage_pool", "storage")
		return found
	}
	hasTier := func() (string, bool) {
		return r.String("storage_tier")
	}
	lister := func() storageLister {
		if deps.PVE != nil {
			return deps.PVE.ClusterStorage()
		}
		return nil
	}
	warnEncrypted := func(tier, pool string) {
		deps.Log(ctx).Warn("create_disk: selected encrypted storage tier — CPI cannot verify pool encryption; operator responsibility",
			log.String("tier", tier),
			log.String("pool", pool),
		)
	}

	// Encrypted enforcement (§7.49).
	if encrypted {
		return resolveStorageForDiskEncrypted(ctx, deps.Config, lister(), warnEncrypted, hasExplicitPool, hasTier)
	}

	// Unencrypted path: byte-identical to pre-§7.49.
	if s, found := r.String("storage_pool", "storage"); found {
		return s, nil
	}
	if tier, found := r.String("storage_tier"); found {
		if l := lister(); l != nil {
			return resolveStorageTier(ctx, l, deps.Config, tier, false)
		}
	}
	if s := strings.TrimSpace(deps.Config.DiskStorage); s != "" {
		return s, nil
	}
	return "", cpierrors.Cloud(
		"create_disk: no storage configured (disk_storage empty and neither" +
			" cloud_properties.storage_pool nor cloud_properties.storage is set)",
	)
}

// resolveStorageForDiskEncrypted handles the encrypted=true path for
// resolveStorageForDisk. Extracted to keep the parent function under gocognit 40.
func resolveStorageForDiskEncrypted(
	ctx context.Context,
	cfg *config.CPIConfig,
	lister storageLister,
	warn func(tier, pool string),
	hasExplicitPool func() bool,
	hasTier func() (string, bool),
) (string, error) {
	// Level 1: explicit pool — contradiction, CPI cannot verify.
	if hasExplicitPool() {
		return "", cpierrors.Cloud(
			"create_disk: encrypted=true is set but an explicit storage_pool is also set;" +
				" the CPI cannot verify that a named pool is encrypted." +
				" Use storage_tier with an encrypted tier instead.",
		)
	}
	// Level 2: named tier — resolveStorageTier enforces Encrypted:*true on the tier.
	if tier, found := hasTier(); found {
		if lister == nil {
			return "", cpierrors.Cloud(
				"create_disk: encrypted=true with storage_tier %q but cluster storage API is not available",
				tier,
			)
		}
		pool, err := resolveStorageTier(ctx, lister, cfg, tier, true)
		if err != nil {
			return "", err
		}
		warn(tier, pool)
		return pool, nil
	}
	// Level 3 & 4: no explicit tier or pool → auto-select lex-first encrypted tier.
	if lister == nil {
		return "", cpierrors.Cloud(
			"create_disk: encrypted=true but cluster storage API is not available for auto-tier selection",
		)
	}
	pool, tier, err := resolveEncryptedPool(ctx, lister, cfg, "create_disk")
	if err != nil {
		return "", err
	}
	warn(tier, pool)
	return pool, nil
}

// createDiskAttemptBudgets resolves the two retry budgets create_disk uses.
//
// VMID-collision attempts: retry.vmid_alloc.max_attempts overrides the
// existing vmid_alloc_attempts, which overrides the built-in default 5.
//
// Lock retries scale with how busy the storage is, not with how many VMID
// collisions we can tolerate. Precedence (first > 0 wins):
//  1. retry.storage_lock.max_attempts — dedicated storage-lock budget (primary)
//  2. retry.storage_import.max_attempts — legacy fallback preserves deployments
//     that set this knob before storage_lock existed
//  3. vmid_alloc_attempts — legacy scalar override
//  4. pve.DefaultStorageLockMaxAttempts — shipped constant (10)
func createDiskAttemptBudgets(deps Deps) (maxAttempts, lockAttempts int) {
	maxAttempts = deps.Config.RetryVMIDAlloc().MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = deps.Config.VMIDAllocAttempts
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	lockAttempts = deps.Config.RetryStorageLock().MaxAttempts
	if lockAttempts <= 0 {
		lockAttempts = deps.Config.RetryStorageImport().MaxAttempts
	}
	if lockAttempts <= 0 {
		lockAttempts = deps.Config.VMIDAllocAttempts
	}
	if lockAttempts <= 0 {
		lockAttempts = pve.DefaultStorageLockMaxAttempts
	}
	return maxAttempts, lockAttempts
}

// HandleCreateDisk returns a Handler for the BOSH CPI create_disk method.
//
// Arguments (positional JSON array):
//
//	[0] size_mb        int     — requested disk size in MiB (must be > 0)
//	[1] cloud_props    object  — optional overrides: storage, disk_format
//	[2] vm_cid         string  — optional; when non-empty and parseable as int,
//	                             used as the VMID label in the volume name;
//	                             otherwise NextDiskVMID allocates a free VMID.
//
// Returns: disk_cid string of the form "<storage>:<volid>".
//
// Node selection: deps.Config.Node is used for all storage operations. PVE
// shared storage (e.g. ceph-rbd, NFS) is cluster-visible; local storage
// requires the disk and its VM to reside on the same node. Operators using
// local storage must ensure vm_cid and the new disk share the same node.
//
//nolint:gocognit // Orchestration shell: parse args, attemptCreateVolume retry loop, deferred rollbackCreatedVolume. The retry+rollback wiring lives in extracted helpers; the closure carries the orchestration glue.
func HandleCreateDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// 1. Unmarshal and validate arguments.
		// ----------------------------------------------------------------
		if len(args) < 2 {
			return nil, cpierrors.Cloud("create_disk: expected at least 2 arguments (size_mb, cloud_properties), got %d", len(args))
		}

		var sizeMB int
		if err := json.Unmarshal(args[0], &sizeMB); err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: args[0] size_mb must be an integer")
		}
		if sizeMB <= 0 {
			return nil, cpierrors.Cloud("create_disk: size_mb must be > 0, got %d", sizeMB)
		}

		var cloudProps createDiskCloudProperties
		// args[1] may be null, {}, or a populated object.
		if err := json.Unmarshal(args[1], &cloudProps); err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: args[1] cloud_properties must be an object")
		}

		// Also unmarshal args[1] into a raw map for the layered resolver.
		// Tolerate null/missing: nil and empty map are both safe for the resolver.
		var callCP map[string]any
		if args[1] != nil {
			// Unmarshal errors here are non-fatal: a null/empty JSON value decodes
			// to a nil map, which newLayeredResolver handles as an empty call layer.
			_ = json.Unmarshal(args[1], &callCP)
		}

		// args[2] (vm_cid) is optional; an absent or null third argument is fine.
		var vmCID string
		if len(args) >= 3 && args[2] != nil {
			// Accept both JSON string "\"100\"" and JSON null.
			// Ignore unmarshal errors for null/missing.
			_ = json.Unmarshal(args[2], &vmCID)
		}

		// ----------------------------------------------------------------
		// 2. Build layered resolver, then resolve storage, format, and node.
		// ----------------------------------------------------------------
		r, err := newLayeredResolver(callCP, deps.Config)
		if err != nil {
			// CloudError: unknown vm_type/disk_type selector or non-string value.
			return nil, err
		}

		// Resolve encrypted flag: per-call > global > false.
		// layeredResolver.Bool reads call/disk_type/vm_type layers; global is the
		// CPIConfig.Encrypted field. When neither is set, encrypted=false → no filter.
		var encryptedCallLevel *bool
		if v, ok := r.Bool("encrypted"); ok {
			encryptedCallLevel = &v
		}
		encrypted := ResolveEncrypted(deps.Config.Encrypted, encryptedCallLevel)

		storage, err := resolveStorageForDisk(ctx, r, deps, encrypted)
		if err != nil {
			return nil, err
		}

		// Resolve disk_format through the resolver: per-call or profile wins;
		// else global config; else the QCOW2 built-in default.
		// found tracks whether an explicit value was supplied at any layer so
		// formatArg is only set when the operator expressed a preference.
		resolvedFormat, formatFound := r.String("disk_format")
		format := resolvedFormat
		if !formatFound {
			format = deps.Config.VMDiskFormat
			if format == "" {
				format = diskFormatQCOW2
			}
		}

		// Disk-performance options are resolved after storage+format so the
		// discard/ssd auto-resolution (see resolveDiskPerfOptions) can classify
		// the resolved pool's TRIM capability. The storage-type lookup only
		// runs when discard/ssd auto-resolution would actually consult it
		// (needsDiskPerfStorageTypeLookup) — an operator who explicitly sets
		// both discard and ssd never pays for the extra API round trip.
		// Best-effort: a failed/unavailable lookup yields "" and
		// pve.IsTrimCapable("", ...) fails open to false — no discard/ssd
		// bake, never an error.
		var storageType string
		if needsDiskPerfStorageTypeLookup(r, deps.Config) {
			storageType = lookupVMStorageType(ctx, deps, storage)
		}

		// Diagnostic only, never mutates storage config, never fails this
		// call: warns (once per pool per process) when storage resolves to a
		// thick-provisioning zfspool. Strictly gated on storageType above —
		// it re-queries ListStorage for the "sparse" flag only when the type
		// is already resolved to "zfspool", and at most once per pool. An
		// operator with both discard and ssd explicit (storageType stays ""
		// — see needsDiskPerfStorageTypeLookup) does not pay for or receive
		// this diagnostic; that tradeoff preserves the "no live lookup when
		// opted out" contract discard/ssd auto-resolution already
		// established. See warnIfZFSThickProvisioned.
		warnIfZFSThickProvisioned(ctx, deps, storage, storageType)

		diskPerfOpts, err := resolveDiskPerfOptions(r, deps.Config, storageType, format)
		if err != nil {
			return nil, err // non-retriable CloudError: bad cache mode / negative throttle
		}
		// Encode retain_on_delete provenance into the disk CID opts so delete_vm
		// can surface audit provenance in its WARN log even when the disk is
		// foreign-guarded by the VMID guard. Nil → byte-identical (no opts key added).
		if cloudProps.RetainOnDelete != nil && *cloudProps.RetainOnDelete {
			if diskPerfOpts == nil {
				diskPerfOpts = make(map[string]string)
			}
			diskPerfOpts[diskOptRetainOnDelete] = "1"
		}

		backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: backend resolution failed for storage "+storage)
		}
		node, err := backend.NodeForCreate(ctx, vmCID, cloudProps.Node)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk")
		}

		// ----------------------------------------------------------------
		// 3. Compute size in GiB (ceiling division).
		// ----------------------------------------------------------------
		sizeGiB := (sizeMB + 1023) / 1024
		if sizeGiB <= 0 {
			sizeGiB = 1
		}

		// ----------------------------------------------------------------
		// 3a. Storage-utilization gate (pve.storage.max_utilization_pct).
		// No-op when the ceiling is unset (0, the default). addBytes is the
		// new disk's own size — the bytes this call is about to ADD to the
		// pool, not the pool's current or resulting total.
		// ----------------------------------------------------------------
		if gateErr := checkMaxUtilizationGate(ctx, deps, node, storage, int64(sizeGiB)*storageUtilBytesPerGiB, "create_disk"); gateErr != nil {
			return nil, gateErr
		}

		// Block storages (lvm/lvmthin/zfspool) reject qcow2 and only accept
		// raw. The CPI's default disk_format is qcow2 which works for file
		// storages (dir/nfs/cifs). Only pass the explicit format to CreateVolume
		// when the operator expressed a preference at some layer (call or profile);
		// when no layer supplied a value, omit the param so PVE auto-selects the
		// right default for the target storage type.
		formatArg := ""
		if formatFound {
			formatArg = format
		}

		// File-based storages (dir/nfs/cifs/glusterfs/btrfs) require the
		// volume name to carry a format extension and reject bare names with
		// "unable to parse volume filename"; block storages take bare names
		// only. The storage type comes from the perf-options lookup when it
		// ran, else from the backend's cached classification — never a second
		// live /storage query (preserving the no-lookup contract when the
		// operator opted out of auto-resolution). When file-based, append the
		// extension and pin the format param so the two always agree. An
		// unknown type ("") keeps the bare name — unchanged behavior on block
		// storages.
		if storageType == "" {
			if info, ok := pve.BackendStorageInfo(backend); ok {
				storageType = info.Type
			}
		}
		volExt := ""
		if pve.StorageUsesFileVolumes(storageType) {
			if formatArg == "" {
				formatArg = format
			}
			volExt = "." + formatArg
		}

		// Recording only; formatArg (what is sent to PVE) is untouched, and
		// an operator-set format at any layer still wins verbatim.
		format = recordedDiskFormat(format, storageType)

		maxAttempts, lockAttempts := createDiskAttemptBudgets(deps)

		// ----------------------------------------------------------------
		// 4. Per-node in-flight gate (opt-in; limit=0 → unlimited, no gating).
		// ----------------------------------------------------------------
		if deps.Config != nil {
			inflightRelease, inflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
			if inflightErr != nil {
				return nil, cpierrors.Retriable("create_disk: in-flight limit exceeded or context cancelled on node %s: %s", node, inflightErr.Error())
			}
			defer inflightRelease()
		}

		// ----------------------------------------------------------------
		// 5. Allocate a synthetic disk VMID + create the volume.
		//
		// attemptCreateVolume re-runs NextDiskVMID(node, storage) every
		// attempt so the storage scan picks up orphan volumes from prior
		// failed iterations. On non-conflict CreateVolume failures the
		// callback removes any partially-committed volume before propagating.
		// ----------------------------------------------------------------
		namingVMID, bareDiskCID, canonicalVolID, err := attemptCreateVolume(
			ctx, deps, node, storage, sizeGiB, formatArg, volExt, lockAttempts, maxAttempts,
		)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: CreateVolume failed on node "+node+" storage "+storage)
		}

		// From here on, any failure path must roll back the just-created
		// volume so we never leak storage on partial failure. The flag is
		// flipped to true only on the success return below. Registered before
		// the CID encode/length-check below so a failure there (including the
		// over-255 hard error) also triggers rollback.
		success := false
		defer func() {
			if success {
				return
			}
			// Under the parked strategy the disk may already sit on a parker
			// slot by the time a later step fails; delete would then rip the
			// volume out from under that reference. Unpark first (best-effort;
			// a no-op when the park never ran or never committed).
			if deps.Config.DetachedDiskParkedEnabled() {
				unparkCreatedDiskForRollback(ctx, deps, bareDiskCID)
			}
			rollbackCreatedVolume(ctx, deps, node, storage, canonicalVolID, deps.Log(ctx))
		}()

		// Append placement metadata so downstream handlers (attach_disk and
		// fault-domain co-location in create_vm) can read pool, node, and AZ
		// without an extra PVE API call. Pool is always the resolved storage;
		// node is the PVE node that holds the volume (meaningful for
		// local-backend deployments). AZ is set from
		// cloud_properties.availability_zone when provided; otherwise left
		// empty so the CID carries no AZ constraint.
		//
		// DiskCIDMeta.Node documents "populated for node-local backends;
		// empty for shared storage" — a shared-storage volume is reachable
		// from any node, so the node the create happened to run on is not a
		// property of the disk, and the fault-domain reader only consults
		// Node for local backends anyway. Honor the contract here rather
		// than recording a meaningless pin.
		metaNode := node
		if backend.Kind() == pve.BackendShared {
			metaNode = ""
		}
		stableID, idErr := stableIDForNewDisk(deps)
		if idErr != nil {
			return nil, idErr
		}
		meta := &pve.DiskCIDMeta{
			Pool: storage,
			Node: metaNode,
			AZ:   cloudProps.AvailabilityZone,
			Opts: diskPerfOpts,
			// The parked strategy promises this disk a parker anchor whenever
			// it is detached (parkFreshDisk below parks it right away). The
			// promise rides in the CID so the attach/delete holder guards can
			// tell a legitimately free-floating legacy disk from one whose
			// parker vanished (see pve.parked_anchor_strict).
			Anchor: deps.Config.DetachedDiskParkedEnabled(),
			// Record the resolved disk-image format so attach_disk reuses the
			// value this disk was created under instead of re-deriving it from
			// whatever vm_disk_format says at attach time.
			Format: format,
			ID:     stableID,
		}
		// A CID whose pvd- form would overflow MySQL-backed Directors'
		// varchar(255) disk_cid column is emitted as a pvz- gzip envelope
		// instead; CIDs that fit stay pvd- and byte-identical. The fallback is
		// unconditional (not gated on the disk_cid_compression property, which
		// is retained as an accepted no-op): the stable-identity field makes
		// richly-annotated envelopes overflow in ordinary configurations, and
		// decode has always accepted both forms, so the only alternative to a
		// pvz- CID here is failing the disk's creation outright.
		var diskCID string
		diskCID, err = pve.EncodeDiskCIDCompressed(bareDiskCID, meta)
		if err != nil {
			// Unreachable in practice: bareDiskCID is always non-empty here
			// (attemptCreateVolume guarantees it). Kept as a hard error rather
			// than a panic so a future refactor that breaks the invariant
			// fails loudly instead of corrupting a disk CID.
			return nil, cpierrors.Wrap(err, "create_disk: encode disk CID")
		}

		deps.Log(ctx).Info("create_disk",
			log.String("disk_cid", diskCID),
			log.Int("size_mb", sizeMB),
			log.Int("size_gib", sizeGiB),
			log.String("storage", storage),
			log.String("format", format),
			log.Int("naming_vmid", namingVMID),
		)

		// MySQL-backed Directors store disk_cid in a VARCHAR(255) column
		// (Postgres uses TEXT). A CID that still overflows this bound even
		// after the compressed-encoding fallback would silently truncate or
		// be rejected by such a Director on the next create_disk-adjacent
		// write, orphaning the volume just created above — so this is a hard
		// error, not a warning, and the deferred rollback above reclaims the
		// volume.
		if len(diskCID) > pve.DiskCIDLengthTarget {
			return nil, cpierrors.Cloud(
				"create_disk: encoded disk CID is %d characters even after gzip compression, exceeding the %d-character limit enforced by MySQL-backed BOSH Directors (varchar(255) disk_cid column); reduce per-disk metadata (fewer performance options, shorter storage/node names) to bring it under the limit",
				len(diskCID), pve.DiskCIDLengthTarget,
			)
		}

		// Park the fresh disk before returning the CID so it is never handed
		// to the Director unowned (see parkFreshDisk). Runs after the CID
		// length check: a CID that is about to be rejected should not spend
		// the parker round trips first.
		if parkErr := parkFreshDisk(ctx, deps, node, diskCID, bareDiskCID, stableID); parkErr != nil {
			return nil, parkErr
		}

		// Apply operator-supplied tags to the attached VM, if any. PVE has
		// no native disk-volume tag field, so disk tags can only ride on the
		// hosting VM. When create_disk is called without a vm_cid hint we
		// can't attribute them yet — set_disk_metadata picks them up later.
		applyCreateDiskTags(ctx, deps, node, vmCID, diskCID, cloudProps.Tags)

		success = true
		return diskCID, nil
	})
}

// stableIDForNewDisk generates the disk's stable identity token (D13) when
// the parked strategy is enabled. The token is recorded in the CID and baked
// onto the parker slot as a drive serial, and is the identity the CPI
// resolves by after a move_disk reassignment renames the volume — the
// envelope volid becomes a birth record. Free-strategy disks stay fully
// legacy: no token, volid-resolved, config-edit transfers.
func stableIDForNewDisk(deps Deps) (string, error) {
	if !deps.Config.DetachedDiskParkedEnabled() {
		return "", nil
	}
	stableID, err := pve.GenerateDiskStableID()
	if err != nil {
		return "", cpierrors.Wrap(err, "create_disk: generate stable disk ID")
	}
	return stableID, nil
}

// applyCreateDiskTags applies cloud_properties.tags to the VM the new disk
// is attached to, when vm_cid was supplied and parses to a positive VMID.
// Tagging is best-effort metadata: a failure here is logged and never fails
// create_disk (the volume is already committed); the next set_disk_metadata
// call re-applies. Extracted from HandleCreateDisk to keep its cyclomatic
// complexity under the project threshold; behavior is unchanged.
func applyCreateDiskTags(ctx context.Context, deps Deps, node, vmCID, diskCID string, tags map[string]string) {
	if len(tags) == 0 {
		return
	}
	if vmCID == "" {
		deps.Log(ctx).Warn(
			"create_disk: tags supplied but disk has no attached VM; tags deferred until set_disk_metadata is called",
			log.String("disk_cid", diskCID),
		)
		return
	}
	vmid, parseErr := strconv.Atoi(vmCID)
	if parseErr != nil || vmid <= 0 {
		deps.Log(ctx).Warn(
			"create_disk: tags supplied but vm_cid is not a valid integer; tags deferred",
			log.String("disk_cid", diskCID),
			log.String("vm_cid", vmCID),
		)
		return
	}
	if applyErr := applyCustomTagsToVM(ctx, deps, node, vmid, tags, diskCID); applyErr != nil {
		// Tagging is best-effort metadata; do not fail volume
		// creation when only the tag write fails. The next
		// set_disk_metadata call will re-apply.
		deps.Log(ctx).Warn("create_disk: failed to apply tags to attached VM",
			log.String("disk_cid", diskCID),
			log.String("vm_cid", vmCID),
			log.Err(applyErr),
		)
	}
}

// recordedDiskFormat returns the disk-image format to record in the CID
// envelope. The recorded format must describe the volume PVE actually
// creates: block-native storages (lvm/lvmthin/zfspool/rbd) have no file
// format at all — PVE allocates raw no matter what format any config layer
// expressed — so the block-native answer is always raw; recording an
// expressed qcow2 there would describe a volume that does not exist (and
// trips the out-of-band format verification against the storage content
// listing). On file-based storages the resolved format (expressed at any
// layer, or the built-in qcow2 fallback) is what PVE creates and is recorded
// verbatim. An unknown storage type ("") keeps the resolved default — the
// fail-open shape every other type-dependent decision in create_disk takes.
func recordedDiskFormat(resolved, storageType string) string {
	if pve.IsBlockNativeStorage(storageType) {
		return diskFormatRaw
	}
	return resolved
}

// attemptCreateVolume allocates a synthetic disk VMID and calls CreateVolume,
// retrying on VMID conflicts up to maxAttempts times. On a non-conflict
// CreateVolume failure it performs best-effort orphan cleanup before returning.
//
// volExt is the filename extension (".qcow2", ".raw"; leading dot included)
// required by dir-style storages, or "" for block storages. When set, the
// canonical volid takes the dir-plugin path form
// "<storage>:<vmid>/vm-<vmid>-disk-0<ext>" so rollback targets the volid PVE
// actually allocated.
//
// Returns:
//   - namingVMID: the VMID allocated by AllocateDiskWithRetry (for logging)
//   - bareDiskCID: the bare PVE volid ("<storage>:<volname>"), equal to the
//     PVE-returned volid when non-empty, otherwise the constructed canonical
//     form. Never empty when err is nil. The caller (HandleCreateDisk) is
//     responsible for wrapping it in the pvd-/pvz- envelope and adding
//     placement metadata — that step happens after the rollback defer is
//     registered so an encode/length failure still reclaims the volume.
//   - canonicalVolID: the constructed "storage:name" form, used for rollback
//   - err: non-nil when all attempts fail
func attemptCreateVolume(
	ctx context.Context,
	deps Deps,
	node, storage string,
	sizeGiB int,
	formatArg, volExt string,
	lockAttempts, maxAttempts int,
) (namingVMID int, bareDiskCID, canonicalVolID string, err error) {
	var volid string

	namingVMID, err = pve.AllocateDiskWithRetry(ctx, deps.PVE, node, storage,
		func(candidate int) error {
			volName := fmt.Sprintf("vm-%d-disk-0%s", candidate, volExt)
			candidateCanonical := fmt.Sprintf("%s:%s", storage, volName)
			if volExt != "" {
				// Dir-style plugins store the volume under a per-VMID
				// directory and the volid carries that path.
				candidateCanonical = fmt.Sprintf("%s:%d/%s", storage, candidate, volName)
			}

			var v string
			cerr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "create_disk", lockAttempts, func() error {
				var innerErr error
				v, innerErr = deps.PVE.Storage().CreateVolume(
					ctx, node, storage, sizeGiB, formatArg, candidate, volName,
				)
				return innerErr
			})
			if cerr != nil {
				if pve.IsVMIDConflict(cerr) {
					// Pure conflict (volume name already taken at storage
					// level). No partial commit possible; let the helper
					// pick a new VMID.
					deps.Log(ctx).Info("create_disk: vmid conflict, retrying",
						log.Int("vmid_attempted", candidate),
						log.String("storage", storage),
					)
					return cerr
				}
				// Non-conflict CreateVolume failure: PVE may have
				// partially committed the volume (network drop mid-task,
				// storage daemon timeout). Best-effort: remove it before
				// propagating so retries (which won't run) and operator
				// re-runs see a clean slate.
				rollbackCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
				defer rbCancel()
				exists, exErr := deps.PVE.Storage().Exists(rollbackCtx, node, storage, candidateCanonical)
				if exErr != nil {
					// A failed probe means the sweep is silently skipped;
					// name the volid so operators can distinguish
					// "nothing to clean" from "could not look".
					deps.Log(rollbackCtx).Warn("create_disk: orphan volume existence probe failed; sweep skipped",
						log.String("volid", candidateCanonical),
						log.Err(exErr),
					)
				}
				if exErr == nil && exists {
					var upid string
					delErr := pve.RetryOnTransientOrLock(rollbackCtx, deps.Log(rollbackCtx), "create_disk.orphan_sweep", cleanupSweepMaxAttempts, func() error {
						var innerErr error
						upid, innerErr = deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, candidateCanonical)
						return innerErr
					})
					switch {
					case delErr != nil:
						deps.Log(rollbackCtx).Warn("create_disk: orphan volume cleanup after CreateVolume error failed",
							log.String("volid", candidateCanonical),
							log.Err(delErr),
						)
					case upid != "":
						if werr := pve.AwaitTaskWithLogger(rollbackCtx, deps.PVE, node, upid, deps.Log(rollbackCtx)); werr != nil {
							deps.Log(rollbackCtx).Warn("create_disk: orphan volume cleanup await failed",
								log.String("volid", candidateCanonical),
								log.String("upid", upid),
								log.Err(werr),
							)
						} else {
							deps.Log(rollbackCtx).Info("create_disk: removed orphan volume after CreateVolume error",
								log.String("volid", candidateCanonical),
							)
						}
					default:
						deps.Log(rollbackCtx).Info("create_disk: removed orphan volume after CreateVolume error",
							log.String("volid", candidateCanonical),
						)
					}
				}
				return cerr
			}

			volid = v
			canonicalVolID = candidateCanonical
			return nil
		},
		pve.IsVMIDConflict,
		maxAttempts,
		pve.WithRange(deps.Config.DiskVMIDRangeStart, deps.Config.DiskVMIDRangeEnd),
	)
	if err != nil {
		return 0, "", "", err
	}

	// PVE returns the full volid in canonical "storage:name" form
	// ("local-lvm:vm-9001-disk-0") or an empty string when it echoes the
	// filename. Use the canonical form when empty so the bare CID is always
	// well-formed. The volid is already a valid disk CID; re-prefixing with
	// storage would double it (e.g. "data:data:...").
	bareDiskCID = volid
	if bareDiskCID == "" {
		bareDiskCID = canonicalVolID
	}
	return namingVMID, bareDiskCID, canonicalVolID, nil
}

// rollbackCreatedVolume best-effort deletes a successfully created volume when
// a subsequent step in create_disk fails. Runs on a detached, bounded context
// (detachedContext) so cleanup completes even if the caller cancelled the
// request without stalling past rollbackCleanupTimeout. Logs errors; never
// panics.
func rollbackCreatedVolume(
	ctx context.Context,
	deps Deps,
	node, storage, canonicalVolID string,
	logger *log.Logger,
) {
	rollbackCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
	defer rbCancel()
	var upid string
	delErr := pve.RetryOnTransientOrLock(rollbackCtx, logger, "create_disk.rollback_volume", cleanupSweepMaxAttempts, func() error {
		var innerErr error
		upid, innerErr = deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, canonicalVolID)
		return innerErr
	})
	if delErr != nil {
		logger.Warn("create_disk: rollback DeleteVolume failed",
			log.String("volid", canonicalVolID),
			log.Err(delErr),
		)
		return
	}
	if upid != "" {
		if werr := pve.AwaitTaskWithLogger(rollbackCtx, deps.PVE, node, upid, logger); werr != nil {
			logger.Warn("create_disk: rollback DeleteVolume await failed",
				log.String("volid", canonicalVolID),
				log.String("upid", upid),
				log.Err(werr),
			)
			return
		}
	}
	logger.Info("create_disk: rolled back created volume after failure",
		log.String("volid", canonicalVolID),
	)
}

// parkFreshDisk parks a just-created volume onto a parker VM when
// detached_disk_strategy=parked, so create_disk never returns a CID for an
// unowned disk. Between create_disk and the eventual attach_disk the volume
// otherwise carries no protection flag and no tags (a real window during
// deploys, indefinite when the deploy fails before attaching), leaving it
// fully exposed to orphan sweeps. attach_disk and create_vm both unpark
// through the shared holder guard, so a disk parked here attaches exactly
// like one parked by detach_disk. No-op under strategy=free.
//
// Failure is fail-closed: the caller returns the error, the deferred rollback
// unparks (best-effort, in case the park half-committed) and deletes the
// volume, and a Director retry re-creates from scratch.
func parkFreshDisk(ctx context.Context, deps Deps, node, diskCID, bareDiskCID, stableID string) error {
	if !deps.Config.DetachedDiskParkedEnabled() {
		return nil
	}
	parkerCfg := pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.RequestDirectorUUID,
		// DiskStorage feeds WithStorageScan on parker VMID allocation so a
		// VMID whose number is already claimed by orphaned volumes on the
		// disk storage is skipped (same guard detach_disk's park applies).
		DiskStorage: deps.Config.DiskStorage,
		// Always true here (the gate above), recorded for the holder scan's
		// log-level choice.
		// Same strict anchor invariant the read paths apply; see
		// ParkerConfig.AnchorStrict.
		ParkedEnabled: deps.Config.DetachedDiskParkedEnabled(),
		AnchorStrict:  deps.Config.ParkedAnchorStrictValue(),
	}
	// StableID makes the park attach bake the serial= identity onto the
	// parker slot, and keys the provenance record by the token.
	pctx := pve.ParkContext{DiskCID: diskCID, StableID: stableID}
	if parkErr := pve.ParkDisk(ctx, deps.PVE, deps.Log(ctx), node, bareDiskCID, parkerCfg, pctx); parkErr != nil {
		return retriableUnlessPermanent(parkErr,
			fmt.Sprintf("create_disk: park fresh disk %s (fail-closed: rollback deletes the volume)", diskCID))
	}
	return nil
}

// unparkCreatedDiskForRollback best-effort detaches a possibly-parked fresh
// disk from its parker before the rollback delete, so a half-registered
// parker slot never outlives the volume it references. Runs on a detached,
// bounded context like rollbackCreatedVolume. All failures are logged and
// swallowed: the volume delete still runs, and the provenance-tagged
// disk-audit sweep covers whatever a crash mid-rollback leaves behind.
func unparkCreatedDiskForRollback(ctx context.Context, deps Deps, bareDiskCID string) {
	rollbackCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
	defer rbCancel()
	logger := deps.Log(rollbackCtx)
	parkerCfg := parkerReadConfigFor(deps)
	holder, resolveErr := pve.ResolveDiskHolder(rollbackCtx, deps.PVE, logger, bareDiskCID, parkerCfg)
	if resolveErr != nil {
		logger.Warn("create_disk: rollback holder scan failed; volume delete may be refused while a parker still references it",
			log.String("volid", bareDiskCID),
			log.Err(resolveErr),
		)
		return
	}
	// UnparkDiskAt is a no-op when the resolved holder is not a parker,
	// which covers every failure path that never reached parkFreshDisk.
	if unparkErr := pve.UnparkDiskAt(rollbackCtx, deps.PVE, logger, bareDiskCID, holder, parkerCfg); unparkErr != nil {
		logger.Warn("create_disk: rollback unpark failed",
			log.String("volid", bareDiskCID),
			log.Err(unparkErr),
		)
	}
}
