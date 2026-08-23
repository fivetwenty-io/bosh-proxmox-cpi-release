// create_vm_disk.go builds and manages VM disks/volumes: template CID
// parsing, clone-from-template, root disk sizing/resize, and ephemeral and
// persistent disk attachment.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// liveStorageInfo issues a live /storage listing and decodes it through
// pve.ParseStorageEntry — the SAME decoder StorageInfoCache.refresh uses —
// so this file's storage-classification call sites cannot silently diverge
// from the canonical parsing (in particular: this is what gives lookupVMStorageType
// and needsReplicaCheck access to the backing-identity fields (Path/Server/
// Export) needed by the clone-mode storageMismatch check below, without each
// duplicating its own ad-hoc JSON decode).
//
// This deliberately does NOT go through deps.Resolver/StorageInfoCache: the
// static fallback backend built when Resolver is unwired (the common shape
// for handler unit tests — see backendResolverOrDefault) reports every
// storage as shared with no StorageInfo at all, which would silently defeat
// every caller below. A live, uncached listing exactly matches the pre-
// existing behavior of the two ad-hoc readers this replaces: works whether or
// not Resolver is wired, at the cost of one extra API call per invocation
// (unchanged from before — neither prior implementation cached either).
//
// Returns (info, false) on any failure: nil PVE/ClusterStorage, empty name,
// transport error, or the name absent from the index. Callers treat false as
// "unknown" and fail open, exactly as the pre-consolidation ad-hoc decoders
// did on any failure.
func liveStorageInfo(ctx context.Context, deps Deps, storageName string) (pve.StorageInfo, bool) {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil || storageName == "" {
		return pve.StorageInfo{}, false
	}
	resp, err := deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil || resp == nil {
		return pve.StorageInfo{}, false
	}
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			continue
		}
		if info.Name == storageName {
			return info, true
		}
	}
	return pve.StorageInfo{}, false
}

// lookupVMStorageType fetches the PVE storage type for storageName. Returns
// "" on any error — callers treat "" as linked-clone-capable (permissive).
// This is intentionally best-effort: the create_vm flow must not fail on a
// storage-lookup error that does not affect the import path; the clone-mode
// decision downstream uses "" → linked safely.
//
// ClusterStorage() == nil (e.g. test mocks that don't wire it) is the expected
// case in unit tests; the function returns "" without logging to keep test
// output clean.
func lookupVMStorageType(ctx context.Context, deps Deps, storageName string) string {
	info, ok := liveStorageInfo(ctx, deps, storageName)
	if !ok {
		return ""
	}
	return info.Type
}

// resolveTemplateDiskStorage reads templateVMID's config on templateNode and
// returns the PVE storage pool name its root disk resides on, plus the PVE
// config key ("virtio0" or "scsi0") that root disk actually lives under.
// create_stemcell writes the template's root disk under rootDiskKey(cfg) as
// resolved AT TEMPLATE CREATION TIME (see create_stemcell.go); since templates
// are reused by content-hash tag match across an arbitrary span of time, a
// template built before a pve.root_disk_bus change can carry the other key —
// this function auto-detects whichever key is actually present rather than
// assuming the CPI's current setting, so cloneFromTemplate can compare the
// two and fail fast on a mismatch instead of silently cloning the wrong bus.
//
// A linked clone's overlay volume always lands on the SAME storage pool as
// its base (PVE does not honor the Storage/Format params on a linked clone),
// so cloneFromTemplate needs the template's actual pool — not vm_storage — to
// decide whether clone_mode=linked would silently misplace the disk.
//
// Any failure to determine the pool (config read error, missing/unparseable
// root disk entry) returns ("", "", non-nil err). Callers MUST treat a
// non-nil err as "undeterminable" and fail open to the pre-existing
// (vm_storage-keyed) behavior with a Warn — the template's storage is cluster
// state the CPI does not control, and a transient read hiccup here must never
// hard-fail create_vm.
func resolveTemplateDiskStorage(ctx context.Context, deps Deps, templateNode string, templateVMID int64) (storage, rootKey string, err error) {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return "", "", fmt.Errorf("PVE QEMU service unavailable")
	}
	cfg, cfgErr := deps.PVE.QEMU().Config(ctx, templateNode, int(templateVMID))
	if cfgErr != nil {
		return "", "", fmt.Errorf("read template %d config on node %q: %w", templateVMID, templateNode, cfgErr)
	}
	rootKey = diskKeyVirtio0
	v0, ok := cfg[rootKey].(string)
	if !ok || v0 == "" {
		rootKey = diskKeyScsi0
		v0, ok = cfg[rootKey].(string)
	}
	if !ok || v0 == "" {
		return "", "", fmt.Errorf("template %d config on node %q has neither a %s nor a %s entry",
			templateVMID, templateNode, diskKeyVirtio0, diskKeyScsi0)
	}
	bare := v0
	if comma := strings.Index(bare, ","); comma >= 0 {
		bare = bare[:comma]
	}
	storage, _, parseErr := pve.ParseDiskCID(bare)
	if parseErr != nil {
		return "", "", fmt.Errorf("parse template %d %s volid %q: %w", templateVMID, rootKey, bare, parseErr)
	}
	return storage, rootKey, nil
}

// extractSHA8FromFilename extracts the 8-hex-char content sha from a stemcell
// qcow2 filename produced by BuildStemcellFilename.
//
// Format: bosh-stemcell-<name>-<version>-<sha8>.qcow2
// The sha8 is the last "-"-delimited segment before ".qcow2". Because <name>
// and <version> themselves may contain hyphens, the function takes the segment
// between the final "-" and the ".qcow2" suffix rather than splitting on the
// third hyphen.
//
// Returns ("", false) when:
//   - filename does not end with ".qcow2"
//   - there is no "-" before ".qcow2"
//   - the candidate sha8 is not exactly 8 ASCII hex characters
//
// The caller treats ("", false) as "skip lookup, fall back to import-from".
func extractSHA8FromFilename(filename string) (sha8 string, ok bool) {
	const suffix = ".qcow2"
	if !strings.HasSuffix(filename, suffix) {
		return "", false
	}
	base := filename[:len(filename)-len(suffix)]
	lastDash := strings.LastIndexByte(base, '-')
	if lastDash < 0 {
		return "", false
	}
	candidate := base[lastDash+1:]
	if len(candidate) != 8 {
		return "", false
	}
	for _, c := range candidate {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return strings.ToLower(candidate), true
}

// extractSHA8FromFilenameInCID extracts the sha8 from the filename embedded in
// a raw stemcell CID of the form "<storage>:import/<filename>".
//
// It calls ParseStemcellCID to get the volumePath ("import/<filename>"),
// strips the "import/" prefix to obtain the bare filename, then delegates to
// extractSHA8FromFilename. Returns ("", false) on any parse error or when the
// filename does not match the expected pattern — callers skip the lookup.
func extractSHA8FromFilenameInCID(rawCID string) (sha8 string, ok bool) {
	_, volumePath, err := pve.ParseStemcellCID(rawCID)
	if err != nil {
		return "", false
	}
	const importPrefix = "import/"
	if !strings.HasPrefix(volumePath, importPrefix) {
		return "", false
	}
	filename := volumePath[len(importPrefix):]
	return extractSHA8FromFilename(filename)
}

// needsReplicaCheck reports whether the template-gap guard should run for the
// given vmStorage. The guard is needed when storage is local (not shared across
// the cluster): on a multi-node cluster, a local-storage template is only
// accessible on the node that holds it. When storage information is unavailable
// (lookup error or nil PVE client), returns false (fail-open: skip guard).
//
// Classification is via StorageInfo.IsShared() (routed through the shared
// liveStorageInfo decoder above) rather than a raw "shared" flag comparison:
// IsShared() also treats network-backed types (nfs/cifs/rbd/cephfs/
// glusterfs/pbs) as shared by definition even when storage.cfg leaves the
// "shared" flag unset — the same classification every other shared/local
// decision in this codebase uses (StorageInfoCache, Backend resolution,
// placement, DLB). A prior version of this function read only the raw flag,
// which meant an NFS/CIFS/etc pool without an explicit "shared: 1" entry was
// misclassified as needing the local-template guard.
func needsReplicaCheck(ctx context.Context, deps Deps, vmStorage string) bool {
	info, ok := liveStorageInfo(ctx, deps, vmStorage)
	if !ok {
		// Storage undeterminable or not found in the index: fail-open (skip guard).
		return false
	}
	return !info.IsShared()
}

// extractSHA8FromParsed extracts the 8-hex-char content sha from the parsed
// stemcell CID's rawVolid ("<storage>:import/<file>") — delegating to
// extractSHA8FromFilenameInCID so both callers share one parse path. Returns
// ("", false) when the stemcell filename does not match the expected
// bosh-stemcell-<name>-<version>-<sha8>.qcow2 pattern (pre-upgrade or custom
// stems, or the "00000000" unknown-digest placeholder some paths use).
// Callers treat this as "skip the cluster template-cache lookup, fall back to
// import-from" rather than an error.
func extractSHA8FromParsed(parsed *createVMParsedArgs) (sha8 string, ok bool) {
	if parsed == nil || parsed.rawVolid == "" {
		return "", false
	}
	return extractSHA8FromFilenameInCID(parsed.rawVolid)
}

// handleCloneError classifies a cloneFromTemplate error and logs appropriately.
// VMID conflicts and transient transport faults are retryable (same semantics as
// handleCreateError). Storage-lock timeouts from the clone task are also retried.
// Local-storage cross-node violations are NOT retryable — they are
// configuration errors that must propagate immediately.
//
// Clone-source-missing never reaches this function: attemptStemcellTemplateClone
// intercepts it before classification and falls back to strategy=import.
func handleCloneError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	cerr error,
) error {
	switch {
	case pve.IsVMIDConflict(cerr):
		logger.Info("create_vm: vmid conflict on clone, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsStorageLockTimeout(cerr):
		logger.Info("create_vm: storage lock timeout on clone, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsCloneToNonSharedStorage(cerr):
		// PVE rejected the cross-node clone because the DESTINATION storage
		// is node-local. Permanent configuration condition — checked before
		// IsTransientTransport, whose classification the underlying SDK
		// error can also match.
		logger.Error("create_vm: cross-node clone rejected by PVE: destination storage is node-local",
			log.Int("vmid_attempted", candidate),
			log.ErrScrubbed(cerr),
		)
		cleanupVMDetached(ctx, deps, node, candidate, nil, logger)
	case pve.IsTransientTransport(cerr):
		// Clone POST may or may not have committed — sweep the candidate VMID
		// before retrying so the cluster list is clean.
		logger.Info("create_vm: transient transport fault on clone, retrying",
			log.Int("vmid_attempted", candidate),
			log.ErrScrubbed(cerr),
		)
		cleanupVMDetached(ctx, deps, node, candidate, nil, logger)
	default:
		// Non-retryable error (e.g. local-storage cross-node violation,
		// template not found, or other PVE fatal). Clean up any partial VM
		// state and propagate — AllocateWithRetry will not retry.
		cleanupVMDetached(ctx, deps, node, candidate, nil, logger)
	}
	return cerr
}

// enforceCrossNodeCloneTarget applies the local-storage cross-node constraint
// and, when the clone legitimately crosses nodes, sets params.Target. No-op
// when the template already sits on the VM's node.
//
// PVE requires BOTH sides of a cross-node clone to be shared:
//
//   - The ORIGINAL VM's storage (per the SDK's CreateQemuCloneParams.Target
//     doc): consult templateStorage's shared-ness, not shape.vmStorage's — a
//     template on local storage with a shared vm_storage would otherwise pass
//     and PVE itself would reject the clone with a less actionable error.
//     Falls back to shape.vmStorage only when the template's own storage is
//     undeterminable (Config read failure), preserving fail-open behavior.
//   - The DESTINATION storage the new disks land on: a shared-storage
//     template cloning into node-local vm_storage fails PVE-side with
//     "can't clone to non-shared storage". Live-hit shape: an adopted cache
//     template whose disk sits on the shared stemcell pool while vm_storage
//     is lvmthin.
func enforceCrossNodeCloneTarget(
	ctx context.Context,
	deps Deps,
	policyDeps pve.PolicyDeps,
	shape *createVMShape,
	templateNode, templateStorage string,
	templateStorageKnown bool,
	params *sdknodes.CreateQemuCloneParams,
) error {
	if templateNode == shape.node {
		return nil
	}
	checkStorage := shape.vmStorage
	if templateStorageKnown {
		checkStorage = templateStorage
	}
	storageInfo, infoErr := policyDeps.StorageInfo(ctx, checkStorage)
	if infoErr != nil {
		return cpierrors.Wrap(infoErr,
			"create_vm: cross-node clone: cannot look up storage "+checkStorage+" to determine if Target is safe")
	}
	if !storageInfo.IsShared() {
		return cpierrors.Cloud(
			"create_vm: cross-node clone rejected: template is on node %q but VM is targeted to node %q;"+
				" storage %q is local (not shared) — PVE cannot cross-node clone local storage;"+
				" set cloud_properties.node to match the template node (%q),"+
				" or use shared storage",
			templateNode, shape.node, checkStorage, templateNode)
	}
	if needsReplicaCheck(ctx, deps, shape.vmStorage) {
		return cpierrors.Cloud(
			"create_vm: cross-node clone rejected: template is on node %q but VM is targeted to node %q,"+
				" and destination storage %q is local (not shared) — PVE cannot write a cross-node clone's"+
				" disks to local storage; enable stemcell_replicate_local so every node gets its own"+
				" template replica, set cloud_properties.node to match the template node (%q),"+
				" or use shared storage for vm_storage",
			templateNode, shape.node, shape.vmStorage, templateNode)
	}
	// Both sides shared: set Target so PVE lands the clone on shape.node.
	targetNode := shape.node
	params.Target = &targetNode
	return nil
}

// resolveCloneFullFlag decides linked vs full clone from clone_mode plus the
// template/vm_storage placement facts already gathered by cloneFromTemplate.
// Extracted from cloneFromTemplate to keep its cognitive complexity under the
// project threshold; behavior is unchanged from the inline version.
//
// Returns (nil, nil) for a linked clone, (&true, nil) for a full clone, or a
// non-nil error when clone_mode=linked cannot be honored (see the inline
// cases below for the specific rejection reasons).
func resolveCloneFullFlag(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	mode string,
	templateStorage string,
	templateStorageKnown bool,
	storageMismatch bool,
) (*bool, error) {
	linkedOK := pve.IsLinkedCloneSupported(shape.vmStorageType)

	switch mode {
	case config.CloneModeLinked:
		switch {
		case storageMismatch:
			return nil, cpierrors.Cloud(
				"create_vm: clone_mode=linked but vm_storage %q differs from the template's storage %q; "+
					"linked clones always land on the template's storage pool, so this would silently place "+
					"the root disk on %q instead of the configured vm_storage — set clone_mode: auto or "+
					"clone_mode: full, or align pve.stemcell_storage/pve.vm_storage (or cloud_properties.storage) "+
					"to the same pool as the template",
				shape.vmStorage, templateStorage, templateStorage,
			)
		case templateStorageKnown:
			// Same pool as the template: the capability that matters is the
			// template's own storage type (identical pool to vm_storage here,
			// so this is also vm_storage's type — but resolved directly from
			// the template for clarity and to avoid relying on that equality).
			templateStorageType := lookupVMStorageType(ctx, deps, templateStorage)
			if !pve.IsLinkedCloneSupported(templateStorageType) {
				return nil, cpierrors.Cloud(
					"create_vm: clone_mode=linked but the template's storage %q (type %q) does not support"+
						" linked clones; use clone_mode=auto or clone_mode=full, or switch to a"+
						" snapshot-capable storage backend",
					templateStorage, templateStorageType,
				)
			}
		case !linkedOK:
			// Template storage undeterminable: fail open to the pre-1.3
			// vm_storage-keyed capability check.
			return nil, cpierrors.Cloud(
				"create_vm: clone_mode=linked but storage %q (type %q) does not support linked clones;"+
					" use clone_mode=auto or clone_mode=full, or switch to a snapshot-capable storage backend",
				shape.vmStorage, shape.vmStorageType,
			)
		}
		// nil → linked clone.
		return nil, nil
	case config.CloneModeFull:
		t := true
		return &t, nil
	default: // "auto"
		switch {
		case storageMismatch:
			t := true
			logger.Info("create_vm: clone_mode=auto: downgrading linked clone to full clone because"+
				" vm_storage differs from the template's storage and the two storage IDs do not"+
				" share a physical backing (not the same NFS export or directory)",
				log.String("vm_storage", shape.vmStorage),
				log.String("template_storage", templateStorage),
			)
			return &t, nil
		case templateStorageKnown:
			// Same pool as the template: key the capability check off the
			// template's own storage type, freshly resolved — mirrors the
			// forced-linked branch above and avoids relying on shape.vmStorageType
			// (populated once at VM-shape build time) staying in sync with the
			// template's pool.
			templateStorageType := lookupVMStorageType(ctx, deps, templateStorage)
			if !pve.IsLinkedCloneSupported(templateStorageType) {
				t := true
				return &t, nil
			}
			// Otherwise nil → linked clone.
			return nil, nil
		case !linkedOK:
			// Template storage undeterminable: fail open to the pre-1.3
			// vm_storage-keyed capability check.
			t := true
			return &t, nil
		case shape.vmStorageType == "":
			// Template storage undeterminable AND vm_storage's own type lookup
			// failed or returned empty; IsLinkedCloneSupported treats unknown type
			// as linked-capable (permissive default). Log at debug so a PVE
			// rejection of a linked clone is diagnosable even when the storage
			// type could not be determined at clone time.
			logger.Debug("create_vm: clone_mode=auto: storage type unknown, assuming linked-clone support",
				log.String("vm_storage", shape.vmStorage),
			)
		}
		// Otherwise nil → linked clone.
		return nil, nil
	}
}

// checkRootDiskBusMatch enforces the root_disk_bus=scsi clone-path guard,
// extracted from cloneFromTemplate to keep its cognitive complexity under the
// project threshold. See the doc comment inline below for rationale.
func checkRootDiskBusMatch(
	shape *createVMShape,
	templateVMID int64,
	templateStorageKnown bool,
	templateStorageErr error,
	templateRootKey string,
) error {
	// root_disk_bus=scsi requires the source template's root disk to already
	// be on scsi0 — a clone inherits its source's exact disk-key layout, so
	// there is no way to move it post-clone without a config PUT PVE treats as
	// a disk swap (unsupported here). Templates are built once and reused by
	// content-hash tag match, so a template built before this setting was
	// enabled (or under a different setting) would silently clone a
	// virtio0-bus root while every payload elsewhere in this VM claims scsi.
	// Fail fast, before any PVE mutation, rather than produce that split-brain
	// VM. Only checked when scsi is the resolved setting: the default
	// (virtio) path stays fail-open on an undeterminable template exactly as
	// before, so unset/virtio payloads and behavior are byte-identical —
	// every template that exists before this property was introduced is
	// virtio0, so the mismatch branch is unreachable on the default path
	// today. Under root_disk_bus=scsi an undeterminable template also fails
	// fast (rather than falling open, as the storage-pool check does)
	// because there is no safe way to verify the bus without the read that
	// just failed.
	if shape.rootDiskKey != diskKeyScsi0 {
		return nil
	}
	if !templateStorageKnown {
		return cpierrors.Cloud(
			"create_vm: root_disk_bus=scsi requires verifying stemcell template %d's root disk bus before "+
				"cloning, but its config could not be read (%s); retry, or set pve.root_disk_bus back to "+
				"virtio for this deployment",
			templateVMID, templateStorageErr.Error(),
		)
	}
	if templateRootKey != diskKeyScsi0 {
		return cpierrors.Cloud(
			"create_vm: root_disk_bus=scsi requires stemcell template %d to have a scsi0 root disk, "+
				"but it was built with a %s root disk (predates this setting, or was built while it was "+
				"unset/virtio); re-run create_stemcell for this stemcell to rebuild its template on the "+
				"scsi bus, or set pve.root_disk_bus back to virtio for this deployment",
			templateVMID, templateRootKey,
		)
	}
	return nil
}

// storageMismatchByBacking reports whether templateStorage and vmStorage are
// a genuine clone-mode misplacement risk: their PVE storage IDs differ AND
// they do not share a physical backing (Kevin's trap — see the storageMismatch
// doc comment in cloneFromTemplate). Two IDs that differ but resolve to the
// same NFS export (or the same dir path) are NOT a mismatch: a linked clone's
// overlay lands on the same bytes either way, so clone_mode=auto must not
// downgrade to a full clone, and clone_mode=linked must not be rejected.
//
// templateStorage == vmStorage short-circuits to false without any lookup —
// the common case, and avoids a wasted /storage round trip.
//
// Both storages' backing identity is resolved via liveStorageInfo; an
// undeterminable backing on either side is conservative — falls back to the
// plain ID-differs-so-mismatch result, matching the pre-backing-identity
// behavior exactly rather than guessing "same" on missing data.
func storageMismatchByBacking(ctx context.Context, deps Deps, logger *log.Logger, templateStorage, vmStorage string) bool {
	if templateStorage == vmStorage {
		return false
	}
	templateInfo, templateOK := liveStorageInfo(ctx, deps, templateStorage)
	vmInfo, vmOK := liveStorageInfo(ctx, deps, vmStorage)
	if templateOK && vmOK && pve.SameBacking(templateInfo, vmInfo) {
		logger.Info("create_vm: vm_storage and the template's storage are different PVE storage IDs"+
			" but share one physical backing; treating clone_mode placement as a match rather than"+
			" a mismatch (two names, one export)",
			log.String("vm_storage", vmStorage),
			log.String("template_storage", templateStorage),
			log.String("backing", templateInfo.BackingKey()),
		)
		return false
	}
	return true
}

func cloneFromTemplate(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	candidate int,
	candidateName string,
	templateNode string,
	templateVMID int64,
) error {
	// Clone mode: call cloud_properties.clone_mode → vm_type profile → config → "auto".
	// shape carries the resolved cloudPropsMap from the parsed args.
	mode, err := resolveCloneMode(deps.Config, shape.cloudPropsMap)
	if err != nil {
		return err
	}

	templateStorage, templateRootKey, templateStorageErr := resolveTemplateDiskStorage(ctx, deps, templateNode, templateVMID)
	templateStorageKnown := templateStorageErr == nil
	if !templateStorageKnown {
		logger.Warn("create_vm: could not determine template's storage pool; "+
			"clone_mode placement checks fall back to vm_storage's own capability only",
			log.Int64("template_vmid", templateVMID),
			log.String("template_node", templateNode),
			log.Err(templateStorageErr),
		)
	}
	// A linked clone's overlay always lands on templateStorage, never on
	// vm_storage, so any mismatch between the two is a real misplacement risk
	// — but only when templateStorage is actually known.
	//
	// A plain string-compare of the two storage IDs is Kevin's trap: two PVE
	// storage IDs configured against the same physical backing (e.g. the same
	// NFS export registered twice under different names) are NOT a
	// misplacement risk even though their IDs differ — a linked clone's
	// overlay lands on the same bytes either way. storageMismatchByBacking
	// resolves both storages' backing identity and only reports a mismatch
	// when the IDs differ AND the backing genuinely differs too; an
	// undeterminable backing on either side is conservative (falls back to
	// the plain ID compare, same as before this fix — never silently treats
	// "unknown" as "same").
	storageMismatch := templateStorageKnown && storageMismatchByBacking(ctx, deps, logger, templateStorage, shape.vmStorage)

	if busErr := checkRootDiskBusMatch(shape, templateVMID, templateStorageKnown, templateStorageErr, templateRootKey); busErr != nil {
		return busErr
	}

	full, fullErr := resolveCloneFullFlag(ctx, deps, logger, shape, mode, templateStorage, templateStorageKnown, storageMismatch)
	if fullErr != nil {
		return fullErr
	}

	newid := int64(candidate)
	params := &sdknodes.CreateQemuCloneParams{
		Newid: newid,
		Name:  &candidateName,
		Full:  full,
	}

	// Set Storage and Format only for full clones. The SDK validates that
	// these fields are absent on linked clones; setting them on a nil-Full
	// (linked) request triggers a PVE API error.
	if full != nil && *full {
		params.Storage = &shape.vmStorage
		params.Format = &shape.vmDiskFormat
	}

	// The resolved pool (if any) must exist before the clone request
	// references it below — PVE rejects a clone targeting a non-existent
	// pool. No-op when shape.vmPool == "".
	if err := ensureResolvedPool(ctx, deps, shape, logger); err != nil {
		return err
	}
	// Assign the clone to the resolved pool when set — mirrors the import
	// path's "pool" createParams key. Absent (empty) means no pool
	// assignment, byte-identical to every release before this property
	// existed.
	if shape.vmPool != "" {
		poolVal := shape.vmPool
		params.Pool = &poolVal
	}

	// Cross-node Target= enforcement.
	//
	// The clone task is submitted to templateNode. When templateNode != shape.node
	// (BOSH's desired VM node), PVE must move the resulting VM to shape.node.
	// PVE supports cross-node placement via params.Target ONLY on shared storage.
	// Local storage (dir, lvm, lvmthin, zfspool) cannot cross nodes — PVE rejects
	// Target on local-storage clones with a hard error.
	//
	// Topology matrix:
	//   single-node (≤1)          → accept any storage; templateNode==shape.node; no Target.
	//   multi-node + shared       → accept; set Target when templateNode != shape.node.
	//   multi-node + local + pin  → operator must pin to the template node; if
	//       shape.node != templateNode the configuration is wrong — return error.
	//   multi-node + local + no pin → ValidateTemplateCloneStorage rejects (rule 4).
	policyDeps := newHandlerPolicyDeps(deps)
	_, policyErr := pve.ValidateTemplateCloneStorage(ctx, policyDeps, shape.vmStorage, shape.node)
	if policyErr != nil {
		return policyErr
	}

	if err := enforceCrossNodeCloneTarget(ctx, deps, policyDeps, shape, templateNode,
		templateStorage, templateStorageKnown, params); err != nil {
		return err
	}

	logger.Info("create_vm: cloning template",
		log.Int("template_vmid", int(templateVMID)),
		log.String("template_node", templateNode),
		log.Int("new_vmid", candidate),
		log.String("clone_mode", mode),
		log.Bool("full_clone", full != nil && *full),
	)

	upid, cloneErr := pve.CloneQemuVM(ctx, deps.PVE, templateNode, templateVMID, params)
	if cloneErr != nil {
		return cpierrors.Wrap(cloneErr, fmt.Sprintf(
			"create_vm: clone template vmid=%d → new vmid=%d", templateVMID, candidate))
	}

	if upid != "" {
		if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, templateNode, upid, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
			return cpierrors.Wrap(werr, fmt.Sprintf(
				"create_vm: await clone task template vmid=%d → new vmid=%d", templateVMID, candidate))
		}
	}

	logger.Info("create_vm: template clone complete",
		log.Int("template_vmid", int(templateVMID)),
		log.Int("new_vmid", candidate),
	)

	// The clone inherits the template's minimal resources (templates are created
	// with PVE defaults: 512 MiB RAM, 1 core). Apply the requested CPU/memory
	// shape to the cloned VM — the import-from path sets these in CreateQemuParams
	// at create time, but a clone must set them explicitly or the VM boots
	// undersized (e.g. a 512 MiB director that never reaches "running").
	//
	// Also re-enable the QEMU guest agent channel. The stemcell template is
	// created with agent=enabled=0 (a frozen template needs no agent), and a
	// clone inherits that. Without overriding it here every cloned VM has the
	// agent channel disabled, so `qm guest exec`/QGA cannot reach the guest —
	// which removes the only out-of-band path to a VM whose bosh-agent has
	// wedged (the import-from path sets agent=enabled=1 at create time).
	//
	// Also disable the emulated USB tablet. The stemcell template carries no
	// explicit "tablet" key, so PVE's on-by-default applies to both the
	// template and the clone it produces; every BOSH VM is headless, so the
	// tablet device is pure overhead (2-3% CPU at scale) with nothing to
	// benefit from it. No cloud_properties override — unconditional on every
	// cloned VM, matching the import path.
	//
	// Also add the serial0 console device. The template carries no explicit
	// "serial0" key either (PVE's default is no serial device at all), so the
	// clone inherits none and every cloned VM needs it added explicitly, same
	// as agent/tablet above — matching the import path's default write. A
	// pve_config.serial0 override (applied later, post-clone) still wins as
	// the final value.
	//
	// Also REPLACE the inherited tags and clear the inherited description. PVE
	// copies both from the clone source, so without this a workload VM comes up
	// advertising the cache template's identity: bosh-stemcell,
	// bosh-stemcell-cache, bosh-stemcell-sha-<sha8>, the name/version tags, the
	// template's director--<uuid> ref tag, and the template's provenance JSON as
	// its description. Those tags are the exact keys the stemcell lookups,
	// delete_stemcell's cluster-wide sha8 sweep, and the orphan prune all match
	// on; only the template=1 predicate keeps a clone out of those paths today,
	// which is one predicate of margin for a VM that has no business claiming a
	// stemcell identity at all. shape.initialTags is the workload tag set the
	// import path writes at create time (bosh-cpi plus any operator tags and
	// advertised-route provenance), and the import path leaves the description
	// empty, so writing both here makes the two paths converge on the same
	// VM identity. set_vm_metadata later adds the director/deployment/job triple
	// and its own description to either path.
	memStr := strconv.Itoa(shape.memMiB)
	cores64 := int64(shape.cores)
	sockets64 := int64(shape.sockets)
	agentEnabled := "enabled=1"
	tabletOff := false
	workloadTags := shape.initialTags
	clearedDescription := ""
	resourceParams := &sdknodes.UpdateQemuConfigParams{
		Memory:      &memStr,
		Cores:       &cores64,
		Sockets:     &sockets64,
		Agent:       &agentEnabled,
		Tablet:      &tabletOff,
		Serial:      map[int]string{0: "socket"},
		Tags:        &workloadTags,
		Description: &clearedDescription,
	}
	// Apply scsihw override only when switched away from the historic default.
	// Emitting "virtio-scsi-pci" explicitly would be byte-identical in effect but
	// would produce unnecessary diff in the config PUT — keep default path clean.
	if shape.scsihw != "virtio-scsi-pci" {
		scsiVal := shape.scsihw
		resourceParams.Scsihw = &scsiVal
	}
	// Apply cpu type only when resolved (cloud_properties.cpu_type or the
	// global pve.cpu_type value, which ApplyDefaults fills with
	// "host"); empty only via the "pve-default" sentinel, in which
	// case PVE keeps its own kvm64 default on the cloned VM. The clone
	// inherits the template's "cpu" value (templates carry no explicit cpu
	// setting either), so the sentinel path is the same "unset means unset"
	// contract as the import path.
	if shape.cpuType != "" {
		cpuVal := shape.cpuType
		resourceParams.Cpu = &cpuVal
	}
	applyCloneBalloon(resourceParams, shape.balloonMiB)
	// Apply root-disk performance options to shape.rootDiskKey when any are set.
	// The clone inherits the template's root disk string under that same key
	// (verified against templateRootKey before cloning started — see the
	// root_disk_bus=scsi guard above); we append our opts to it. When
	// rootDiskPerfOpts is empty nothing is emitted (byte-identical path).
	if len(shape.rootDiskPerfOpts) > 0 {
		// PVE's config PUT requires the full "volid,opts" value for the root
		// disk key — an options-only delta (",cache=writeback") is rejected as a
		// bad volid. The clone inherited the template's root disk string, so
		// fetch the cloned VM's current value, strip any existing options, and
		// re-append our resolved opts. A Config read failure is non-fatal: the
		// VM is already cloned and functional, so we log and skip rather than
		// roll back over a tuning patch.
		clonedCfg, cfgGetErr := deps.PVE.QEMU().Config(ctx, shape.node, candidate)
		if cfgGetErr != nil {
			// Non-fatal best-effort: log and skip the perf-opts patch. The VM is
			// functional; operator can set opts manually. A CloudError here would
			// roll back a successfully cloned VM unnecessarily.
			logger.Warn("create_vm: could not fetch cloned VM config to apply root-disk perf opts; skipping",
				log.Int("vmid", candidate),
				log.ErrScrubbed(cfgGetErr),
			)
		} else {
			currentRootDiskVal, _ := clonedCfg[shape.rootDiskKey].(string)
			if currentRootDiskVal == "" {
				// Fallback: use storage:index bare form that PVE recognises. The
				// "vm-<id>-disk-0" naming convention is bus-agnostic (PVE names the
				// underlying volume the same way regardless of which controller
				// key it is attached under).
				currentRootDiskVal = shape.vmStorage + ":vm-" + strconv.Itoa(candidate) + "-disk-0"
			}
			// splitDiskOptStr extracts the bare volid (stripping any existing opts)
			// so we can re-append fresh opts cleanly without duplicates.
			bareVolid, _ := splitDiskOptStr(currentRootDiskVal)
			patchedRootDiskVal := buildDiskOptStr(bareVolid, shape.rootDiskPerfOpts)
			if shape.rootDiskKey == diskKeyScsi0 {
				if resourceParams.Scsi == nil {
					resourceParams.Scsi = make(map[int]string)
				}
				resourceParams.Scsi[0] = patchedRootDiskVal
			} else {
				if resourceParams.Virtio == nil {
					resourceParams.Virtio = make(map[int]string)
				}
				resourceParams.Virtio[0] = patchedRootDiskVal
			}
		}
	}
	if cfgErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, shape.node, strconv.Itoa(candidate), resourceParams); cfgErr != nil {
		return cpierrors.Wrap(pve.WrapError(cfgErr), fmt.Sprintf(
			"create_vm: apply cpu/memory to cloned vmid=%d: %s", candidate, cfgErr.Error()))
	}
	logger.Info("create_vm: applied cpu/memory to cloned vm",
		log.Int("new_vmid", candidate),
		log.Int("cores", shape.cores),
		log.Int("sockets", shape.sockets),
		log.Int("memory_mib", shape.memMiB),
	)

	return nil
}

// readRootDiskSizeGiB reads the root disk size from the VM config at rootKey
// ("virtio0" or "scsi0" per pve.root_disk_bus).
//
// A failed Config call is propagated (not swallowed): on a non-5-GiB template a
// transient read failure would otherwise fabricate base=5 and grow by the wrong
// delta — over-growing a large template or under-growing a small one (risking
// the very ephemeral-space boot failure the resize exists to prevent). The
// caller wraps this through pve.WrapError so a transient surfaces as retriable.
//
// Falls back to defaultStemcellDiskGiB only when the config is readable but
// rootKey is absent or unparseable — there is no transient ambiguity there and
// 5 GiB is the safe BOSH-stemcell baseline.
func readRootDiskSizeGiB(ctx context.Context, deps Deps, node string, vmid int, rootKey string) (int, error) {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return 0, err
	}
	v0, ok := cfg[rootKey].(string)
	if !ok || v0 == "" {
		return defaultStemcellDiskGiB, nil
	}
	gib, parseErr := parseDiskSizeGiB(v0)
	if parseErr != nil {
		return defaultStemcellDiskGiB, nil
	}
	return gib, nil
}

// resizeRootDisk grows the root disk (shape.rootDiskKey) by the delta between
// shape.rootDiskGiB and the actual template size read from the VM config
// after creation. It is a no-op when the requested size equals the template
// size, and returns a Cloud error when the requested size is smaller (shrink
// not supported).
//
// PVE silently ignores the `size=<N>G` directive on the import-from
// scsi/virtio param when the source image is smaller than N — the new
// volume keeps the source image's size (~5 GiB for BOSH stemcells).
// Without an explicit resize, the BOSH agent's bootstrap fails at
// "Setting up ephemeral disk: Insufficient remaining disk space"
// (CreatePartitionIfNoEphemeralDisk=true in the stemcell's agent.json
// requires free space at the end of the root disk).
func resizeRootDisk(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	vmid int,
) error {
	actualTemplateGiB, sizeErr := readRootDiskSizeGiB(ctx, deps, shape.node, vmid, shape.rootDiskKey)
	if sizeErr != nil {
		return cpierrors.Wrap(pve.WrapError(sizeErr),
			fmt.Sprintf("create_vm: read template disk size for resize vmid=%d", vmid))
	}
	growGiB := shape.rootDiskGiB - actualTemplateGiB
	if growGiB < 0 {
		return cpierrors.Cloud(
			"create_vm: root disk shrink not supported: requested %d GiB, template %d GiB; use a larger disk size or a smaller stemcell",
			shape.rootDiskGiB, actualTemplateGiB,
		)
	}
	if growGiB == 0 {
		return nil
	}
	// PVE's `qm resize` runs `qemu-img resize` under the per-storage
	// lockfile (/var/lock/pve-manager/pve-storage-<name>). Under a
	// concurrent CF deploy this contends with parallel stemcell imports
	// and other resizes and surfaces as "can't lock file ... got timeout"
	// in the task log. Retry the whole submit+await with seconds-scale
	// backoff against the lock holder finishing.
	rerr := pve.RetryOnTransientOrLock(ctx, logger, "resize_root_disk", shape.maxAttempts, func() error {
		upid, e := deps.PVE.QEMU().ResizeDisk(ctx, shape.node, vmid, shape.rootDiskKey, growGiB)
		if e != nil {
			return e
		}
		if upid == "" {
			return nil
		}
		return pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, upid, logger)
	})
	if rerr != nil {
		// Route through WrapError so task-level transients (LVM command
		// timeouts under VG contention, pmxcfs sync races where the just-
		// created conf is briefly absent) surface as RetriableCloudError
		// — director re-issues create_vm with a fresh VMID instead of
		// failing the deploy.
		return cpierrors.Wrap(pve.WrapError(rerr),
			fmt.Sprintf("create_vm: resize root disk (%s) vmid=%d +%dG", shape.rootDiskKey, vmid, growGiB))
	}
	logger.Info("create_vm: grew root disk",
		log.Int(metadataKeyVMID, vmid),
		log.String("root_disk_key", shape.rootDiskKey),
		log.Int("delta_gib", growGiB),
		log.Int("final_gib", shape.rootDiskGiB),
	)
	return nil
}

// resolveEphemeralShape resolves the ephemeral disk size and storage pool from
// cloud_properties. Returns (0, "", nil) when EphemeralDiskSizeMB is unset —
// no ephemeral disk is created and the agent carves ephemeral storage from the
// root disk (default behavior, byte-identical to pre-feature behavior).
//
// When encrypted=false (unset): storage resolution order is:
//  1. resolver "ephemeral_storage_tier" — live criteria match
//  2. resolver "ephemeral_storage_pool" — explicit pool name
//  3. struct field EphemeralStoragePool
//  4. cfg.VMStorage fallback
//
// When encrypted=true: §7.49 enforcement applies (same rules as create_disk):
//   - explicit ephemeral_storage_pool present → non-retriable CloudError
//   - ephemeral_storage_tier named + not encrypted → non-retriable CloudError
//   - neither tier nor pool → auto-select lex-first encrypted tier from config
//   - A warning is logged on every encrypted-tier selection.
func resolveEphemeralShape(
	ctx context.Context,
	deps Deps,
	cp createVMCloudProps,
	cpMap map[string]any,
) (int, string, error) {
	cfg := deps.Config
	if cp.EphemeralDiskSizeMB <= 0 {
		return 0, "", nil
	}
	r, rErr := newLayeredResolver(cpMap, cfg)
	if rErr != nil {
		return 0, "", rErr
	}
	gib := (cp.EphemeralDiskSizeMB + 1023) / 1024

	// Resolve encrypted flag: per-call > global > false (§7.49).
	var encryptedCallLevel *bool
	if v, ok := r.Bool("encrypted"); ok {
		encryptedCallLevel = &v
	}
	encrypted := ResolveEncrypted(cfg.Encrypted, encryptedCallLevel)

	stor, err := resolveEphemeralStorage(ctx, deps, cfg, r, cp, encrypted)
	if err != nil {
		return 0, "", err
	}
	if stor == "" {
		return 0, "", cpierrors.Cloud(
			"create_vm: ephemeral_disk_size_mb set but no storage pool resolved (set ephemeral_storage_pool or vm_storage)")
	}
	return gib, stor, nil
}

// resolveEphemeralStorage returns the storage pool name for a dedicated ephemeral disk.
// Extracted from resolveEphemeralShape to keep that function under gocognit 40.
//
// When encrypted=false: standard tier→pool→struct→fallback precedence (byte-identical).
// When encrypted=true: §7.49 rules — explicit pool is a contradiction error; named tier
// must be marked encrypted; no tier/pool → auto-select lex-first encrypted tier.
func resolveEphemeralStorage(
	ctx context.Context,
	deps Deps,
	cfg *config.CPIConfig,
	r *layeredResolver,
	cp createVMCloudProps,
	encrypted bool,
) (string, error) {
	lister := storageLister(nil)
	if deps.PVE != nil {
		lister = deps.PVE.ClusterStorage()
	}
	warnEncrypted := func(tier, pool string) {
		deps.Log(ctx).Warn("create_vm: selected encrypted ephemeral storage tier — CPI cannot verify pool encryption; operator responsibility",
			log.String("tier", tier),
			log.String("pool", pool),
		)
	}

	if encrypted {
		return resolveEphemeralStorageEncrypted(ctx, lister, cfg, r, cp, warnEncrypted)
	}

	// Unencrypted path: byte-identical to pre-§7.49.
	if tier, ok := r.String("ephemeral_storage_tier"); ok {
		if lister != nil {
			return resolveStorageTier(ctx, lister, cfg, tier, false)
		}
	}
	if pool, ok := r.String("ephemeral_storage_pool"); ok {
		return pool, nil
	}
	if cp.EphemeralStoragePool != "" {
		return cp.EphemeralStoragePool, nil
	}
	return cfg.VMStorage, nil
}

// resolveEphemeralStorageEncrypted handles the encrypted=true path for
// resolveEphemeralStorage. Extracted to keep the parent under gocognit 40.
func resolveEphemeralStorageEncrypted(
	ctx context.Context,
	lister storageLister,
	cfg *config.CPIConfig,
	r *layeredResolver,
	cp createVMCloudProps,
	warn func(tier, pool string),
) (string, error) {
	// Explicit pool → contradiction.
	if pool, ok := r.String("ephemeral_storage_pool"); ok && pool != "" {
		return "", cpierrors.Cloud(
			"create_vm: encrypted=true is set but an explicit ephemeral_storage_pool is also set;" +
				" the CPI cannot verify that a named pool is encrypted." +
				" Use ephemeral_storage_tier with an encrypted tier instead.",
		)
	}
	if cp.EphemeralStoragePool != "" {
		return "", cpierrors.Cloud(
			"create_vm: encrypted=true is set but ephemeral_storage_pool (struct field) is set;" +
				" the CPI cannot verify that a named pool is encrypted." +
				" Use ephemeral_storage_tier with an encrypted tier instead.",
		)
	}
	// Named tier → resolveStorageTier enforces Encrypted:*true.
	if tier, ok := r.String("ephemeral_storage_tier"); ok {
		if lister == nil {
			return "", cpierrors.Cloud(
				"create_vm: encrypted=true with ephemeral_storage_tier %q but cluster storage API is not available",
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
	// No tier, no pool → auto-select lex-first encrypted tier.
	if lister == nil {
		return "", cpierrors.Cloud(
			"create_vm: encrypted=true but cluster storage API is not available for auto-tier selection",
		)
	}
	pool, tier, err := resolveEncryptedPool(ctx, lister, cfg, "create_vm")
	if err != nil {
		return "", err
	}
	warn(tier, pool)
	return pool, nil
}

// ephemeralMinSizeViolation reports a human-readable deficit string when the
// opt-in §7.40 ephemeral-disk minimum-size invariant is violated, or "" when it
// is satisfied, disabled (ratio 0), or not applicable (no dedicated ephemeral
// disk / unknown RAM). The invariant is
//
//	ephemeralGiB >= ratio * (memMiB / 1024)
//
// Both sizes are binary GiB: ephemeralGiB is already ceil-rounded from
// ephemeral_disk_size_mb in resolveEphemeralShape, and memMiB is the VM's
// configured RAM in MiB, so the comparison is unit-consistent with the agent's
// own swap+/var/vcap/data layout.
func ephemeralMinSizeViolation(cfg *config.CPIConfig, ephemeralGiB, memMiB int) string {
	ratio := cfg.EphemeralDiskMinRatioValue()
	if ratio <= 0 || ephemeralGiB <= 0 || memMiB <= 0 {
		return ""
	}
	ramGiB := float64(memMiB) / 1024.0
	required := ratio * ramGiB
	// epsilon absorbs IEEE-754 drift so an exact-boundary disk (e.g. ratio×RAM
	// that is mathematically an integer but computes to N+1e-15) is not falsely
	// reported "N is below N". 1e-9 GiB is ~1 byte — orders below the 1 GiB
	// sizing granularity, so it never wrongly passes a genuinely undersized disk.
	if float64(ephemeralGiB)+1e-9 >= required {
		return ""
	}
	return fmt.Sprintf(
		"ephemeral disk %dGiB is below the required %.2fGiB (ratio %.2f × RAM %.2fGiB)",
		ephemeralGiB, required, ratio, ramGiB)
}

// enforceEphemeralMinSize applies the opt-in §7.40 ephemeral-disk minimum-size
// invariant. When ephemeral_disk_min_ratio is unset (0) or no dedicated
// ephemeral disk is being created, it is a no-op so behavior is byte-identical.
// On violation the ephemeral_disk_min_mode knob decides: enforce (default) →
// non-retriable CloudError naming the deficit; warn → log and proceed. The
// logger may be nil (defensive — some early shape-resolution paths run before
// deps.Logger is set); a nil logger silently skips the warn log.
func enforceEphemeralMinSize(cfg *config.CPIConfig, logger *log.Logger, ephemeralGiB, memMiB int) error {
	violation := ephemeralMinSizeViolation(cfg, ephemeralGiB, memMiB)
	if violation == "" {
		return nil
	}
	if cfg.EphemeralDiskMinModeValue() == "warn" {
		if logger != nil {
			logger.Warn("create_vm: ephemeral-disk minimum-size invariant violation (warn mode — proceeding)",
				log.String("deficit", violation),
			)
		}
		return nil
	}
	// enforce (default): a config error, not a transient — must not be retriable.
	return cpierrors.Cloud(
		"create_vm: %s. The BOSH agent places a RAM-sized swap file and /var/vcap/data on the"+
			" ephemeral disk, so it must be at least ratio×RAM. Increase ephemeral_disk_size_mb,"+
			" lower pve.ephemeral_disk_min_ratio, or set pve.ephemeral_disk_min_mode=warn to bypass.",
		violation)
}

// attachEphemeralDisk creates and attaches a dedicated ephemeral disk when
// shape.ephemeralDiskGiB > 0. Returns the device path the BOSH agent expects
// (e.g. "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"), or ("", nil)
// when no dedicated ephemeral disk is requested (default no-op path).
//
// Orphan safety: if CreateVolume succeeds but a subsequent step fails, the
// created volume is deleted before the error is returned. The VM-rollback
// defer in createVM (purge=true) auto-purges attached disks but not unattached
// orphan volumes — so explicit cleanup is needed between CreateVolume and
// AttachDisk.
func attachEphemeralDisk(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	vmid int,
) (string, error) {
	if shape.ephemeralDiskGiB <= 0 {
		return "", nil
	}

	volName := fmt.Sprintf("vm-%d-ephemeral-0", vmid)
	createdVolid, err := deps.PVE.Storage().CreateVolume(
		ctx, shape.node, shape.ephemeralStorage,
		shape.ephemeralDiskGiB, shape.vmDiskFormat, vmid, volName,
	)
	if err != nil {
		return "", cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("create_vm: create ephemeral volume vmid=%d size=%dG storage=%s",
				vmid, shape.ephemeralDiskGiB, shape.ephemeralStorage))
	}

	// Rollback uses DeleteVolumeAsync + AwaitTaskWithLogger, matching
	// rollbackCreatedVolume in create_disk.go: the SDK's DeleteVolume doc warns
	// that a caller re-uploading to the same name must await the imgdel task,
	// otherwise a queued delete can run later and silently remove the reused
	// volume. Errors are logged, never propagated — this is best-effort cleanup
	// of an already-failed create_vm attempt.
	cleanupVol := func() {
		rollbackCtx, rbCancel := detachedContext(ctx, rollbackCleanupTimeout)
		defer rbCancel()
		stor, _, _ := pve.ParseDiskCID(createdVolid)
		if stor == "" {
			stor = shape.ephemeralStorage
		}
		upid, delErr := deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, shape.node, stor, createdVolid)
		if delErr != nil {
			logger.Warn("create_vm: ephemeral volume orphan cleanup failed",
				log.Int(metadataKeyVMID, vmid),
				log.String("volid", createdVolid),
				log.Err(delErr),
			)
			return
		}
		if upid != "" {
			if werr := pve.AwaitTaskWithLogger(rollbackCtx, deps.PVE, shape.node, upid, logger); werr != nil {
				logger.Warn("create_vm: ephemeral volume orphan cleanup await failed",
					log.Int(metadataKeyVMID, vmid),
					log.String("volid", createdVolid),
					log.String("upid", upid),
					log.Err(werr),
				)
			}
		}
	}

	// Read current VM config to find next free scsi slot.
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
	if cfgErr != nil {
		cleanupVol()
		return "", cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("create_vm: read VM config for ephemeral slot vmid=%d", vmid))
	}

	slot := nextFreeSCSIIndexAtLeast(vmCfg, 1)
	if slot >= 29 {
		cleanupVol()
		return "", cpierrors.Cloud(
			"create_vm: no free scsi slot for ephemeral disk vmid=%d (scsi1..28 exhausted by persistent disks)", vmid)
	}

	// Force the computed slot via AttachOpts.DiskID. Passing nil here would let
	// the SDK assign from scsi0, which the agent's mappedDevicePathResolver maps
	// onto /dev/sda and collides with the virtio0 root disk — the same reason
	// attach_disk uses chooseSCSISlotSkippingZero. The floor of 1 guarantees a
	// non-zero slot.
	desiredDiskID := fmt.Sprintf("scsi%d", slot)
	if _, attachErr := deps.PVE.QEMU().AttachDisk(
		ctx, shape.node, vmid, createdVolid, "scsi",
		&qemu.AttachOpts{DiskID: desiredDiskID},
	); attachErr != nil {
		cleanupVol()
		return "", cpierrors.Wrap(pve.WrapError(attachErr),
			fmt.Sprintf("create_vm: attach ephemeral disk vmid=%d volid=%s", vmid, createdVolid))
	}

	devPath, pathErr := devicePathByID(desiredDiskID)
	if pathErr != nil {
		// Disk is attached; the VM-rollback defer (purge=true) will destroy
		// it along with the VM. Return a Cloud error to trigger the rollback.
		return "", cpierrors.Cloud(
			"create_vm: ephemeral disk attached as %q but devicePathByID failed: %s",
			desiredDiskID, pathErr.Error())
	}

	logger.Info("create_vm: attached ephemeral disk",
		log.Int(metadataKeyVMID, vmid),
		log.String("volid", createdVolid),
		log.String("slot", desiredDiskID),
		log.String("dev_path", devPath),
	)
	return devPath, nil
}

// attachPersistentDisks attaches each disk CID in parsed.diskCIDs to the VM
// through the same holder guard → unpark → slot choice → perf merge +
// invariants → config PUT → confirm sequence attach_disk runs
// (guardAndUnparkBeforeAttach + attachDiskCore), so a disk handed to
// create_vm gets every protection the standalone handler provides. Before
// this shared path existed, the pre-attach wrote the bare volid with nil
// options: a parked disk ended up referenced by both the parker and the new
// VM (the corruption case attach_disk's foreign-holder refusal exists to
// prevent), and the disk landed on scsi0 where the virtio root disk shadows
// it. Now a parked disk is unparked first, a disk held by any other VM is
// refused, the slot choice skips scsi0, and per-disk performance options
// merge and enforce exactly as they would on a later attach_disk call.
//
// Node note: the core targets shape.node — the node the VM was just created
// on. Placement has already co-located local-backend disks with that node
// (or failed the create); shared backends attach from any node.
func attachPersistentDisks(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
) error {
	vmCID := strconv.Itoa(vmid)
	for _, diskCID := range parsed.diskCIDs {
		if diskCID == "" {
			continue
		}
		// PVE disk config values are the canonical "<storage>:<volname>"
		// form (e.g. "data:vm-9003-disk-0"). decodeDiskCID strips the encoded
		// metadata suffix and logs the codec's rejection reason on failure;
		// PVE rejects non-volid suffixes with
		// "scsi0.file: invalid format - unable to parse volume ID ...".
		bareDiskCID, meta, decErr := decodeDiskCID(ctx, deps, "create_vm", diskCID)
		if decErr != nil {
			return decErr
		}
		if _, _, parseErr := pve.ParseDiskCID(bareDiskCID); parseErr != nil {
			return cpierrors.Cloud("create_vm: parse disk_cid %q: %s", diskCID, parseErr.Error())
		}
		rd, resolveErr := resolveDiskForOp(ctx, deps, "create_vm", diskCID, bareDiskCID, meta)
		if resolveErr != nil {
			return resolveErr
		}
		plan, guardErr := guardAndUnparkBeforeAttach(ctx, deps, "create_vm", &rd, shape.node, vmid)
		if guardErr != nil {
			return guardErr
		}
		diskID, devPath, err := attachDiskCore(ctx, deps, "create_vm", vmCID, shape.node, vmid, diskCID, rd, plan)
		if err != nil {
			return err
		}
		logger.Info("create_vm: attached persistent disk",
			log.Int(metadataKeyVMID, vmid),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
			log.String("device_path", devPath),
		)
	}
	return nil
}
