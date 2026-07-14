package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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

// resolveStorageLayered returns the storage pool name to use for a create_disk call,
// consulting layers in precedence order:
//
//  1. r.String("storage_pool","storage") — per-call or profile cloud_properties
//  2. configDiskStorage — global CPI config default
//
// An empty or whitespace-only result at all levels is a misconfigured CPI manifest;
// the returned error is a non-retriable CloudError.
//
// No PVE storage-type query is performed (v1: name-only resolution). The
// caller is responsible for backend resolution via backendResolverOrDefault.
func resolveStorageLayered(r *layeredResolver, configDiskStorage string) (string, error) {
	if s, found := r.String("storage_pool", "storage"); found {
		return s, nil
	}
	if s := strings.TrimSpace(configDiskStorage); s != "" {
		return s, nil
	}
	return "", cpierrors.Cloud(
		"create_disk: no storage configured (disk_storage empty and neither" +
			" cloud_properties.storage_pool nor cloud_properties.storage is set)",
	)
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
// nolint:gocognit // Orchestration shell: parse args, attemptCreateVolume retry loop, deferred rollbackCreatedVolume. The retry+rollback wiring lives in extracted helpers; the closure carries the orchestration glue.
func HandleCreateDisk(deps Deps) Handler {
	return HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
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

		// VMID-collision attempts: retry.vmid_alloc.max_attempts overrides the
		// existing vmid_alloc_attempts, which overrides the built-in default 5.
		maxAttempts := deps.Config.RetryVMIDAlloc().MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = deps.Config.VMIDAllocAttempts
		}
		if maxAttempts <= 0 {
			maxAttempts = 5
		}
		// Lock retries scale with how busy the storage is, not with how many
		// VMID collisions we can tolerate. Precedence (first > 0 wins):
		//   1. retry.storage_lock.max_attempts  — dedicated storage-lock budget (primary)
		//   2. retry.storage_import.max_attempts — legacy fallback preserves deployments
		//      that set this knob before storage_lock existed
		//   3. vmid_alloc_attempts              — legacy scalar override
		//   4. pve.DefaultStorageLockMaxAttempts — shipped constant (10)
		lockAttempts := deps.Config.RetryStorageLock().MaxAttempts
		if lockAttempts <= 0 {
			lockAttempts = deps.Config.RetryStorageImport().MaxAttempts
		}
		if lockAttempts <= 0 {
			lockAttempts = deps.Config.VMIDAllocAttempts
		}
		if lockAttempts <= 0 {
			lockAttempts = pve.DefaultStorageLockMaxAttempts
		}

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
		namingVMID, diskCID, canonicalVolID, err := attemptCreateVolume(
			ctx, deps, node, storage, sizeGiB, formatArg, lockAttempts, maxAttempts,
			cloudProps.AvailabilityZone, diskPerfOpts,
		)
		if err != nil {
			return nil, cpierrors.Wrap(err, "create_disk: CreateVolume failed on node "+node+" storage "+storage)
		}

		// From here on, any failure path must roll back the just-created
		// volume so we never leak storage on partial failure. The flag is
		// flipped to true only on the success return below.
		success := false
		defer func() {
			if success {
				return
			}
			rollbackCreatedVolume(ctx, deps, node, storage, canonicalVolID, deps.Log(ctx))
		}()

		deps.Log(ctx).Info("create_disk",
			log.String("disk_cid", diskCID),
			log.Int("size_mb", sizeMB),
			log.Int("size_gib", sizeGiB),
			log.String("storage", storage),
			log.String("format", format),
			log.Int("naming_vmid", namingVMID),
		)

		// Apply operator-supplied tags to the attached VM, if any. PVE has
		// no native disk-volume tag field, so disk tags can only ride on the
		// hosting VM. When create_disk is called without a vm_cid hint we
		// can't attribute them yet — set_disk_metadata picks them up later.
		if len(cloudProps.Tags) > 0 {
			if vmCID == "" {
				deps.Log(ctx).Warn(
					"create_disk: tags supplied but disk has no attached VM; tags deferred until set_disk_metadata is called",
					log.String("disk_cid", diskCID),
				)
			} else if vmid, parseErr := strconv.Atoi(vmCID); parseErr == nil && vmid > 0 {
				if applyErr := applyCustomTagsToVM(ctx, deps, node, vmid, cloudProps.Tags, diskCID); applyErr != nil {
					// Tagging is best-effort metadata; do not fail volume
					// creation when only the tag write fails. The next
					// set_disk_metadata call will re-apply.
					deps.Log(ctx).Warn("create_disk: failed to apply tags to attached VM",
						log.String("disk_cid", diskCID),
						log.String("vm_cid", vmCID),
						log.Err(applyErr),
					)
				}
			} else {
				deps.Log(ctx).Warn(
					"create_disk: tags supplied but vm_cid is not a valid integer; tags deferred",
					log.String("disk_cid", diskCID),
					log.String("vm_cid", vmCID),
				)
			}
		}

		success = true
		return diskCID, nil
	})
}

// attemptCreateVolume allocates a synthetic disk VMID and calls CreateVolume,
// retrying on VMID conflicts up to maxAttempts times. On a non-conflict
// CreateVolume failure it performs best-effort orphan cleanup before returning.
//
// az is the availability-zone label from cloud_properties.availability_zone.
// When non-empty it is encoded into the returned disk CID metadata so that
// create_vm can enforce fault-domain co-location for shared-storage disks.
// An empty az produces a CID identical to pre-AZ releases (backward-compatible).
//
// diskPerfOpts holds per-disk PVE performance options (iothread, cache, etc.)
// resolved by resolveDiskPerfOptions from the §7.8 layered resolver. A nil or
// empty map is encoded as-is; omitempty on DiskCIDMeta.Opts keeps the CID
// byte-identical to pre-performance-options releases when no options are set.
//
// Returns:
//   - namingVMID: the VMID allocated by AllocateDiskWithRetry (for logging)
//   - diskCID: the volid to use as the BOSH disk CID; equals the PVE-returned
//     volid when non-empty, otherwise falls back to the constructed canonical form
//   - canonicalVolID: the constructed "storage:name" form, used for rollback
//   - err: non-nil when all attempts fail
func attemptCreateVolume(
	ctx context.Context,
	deps Deps,
	node, storage string,
	sizeGiB int,
	formatArg string,
	lockAttempts, maxAttempts int,
	az string,
	diskPerfOpts map[string]string,
) (namingVMID int, diskCID, canonicalVolID string, err error) {
	var volid string

	namingVMID, err = pve.AllocateDiskWithRetry(ctx, deps.PVE, node, storage,
		func(candidate int) error {
			volName := fmt.Sprintf("vm-%d-disk-0", candidate)
			candidateCanonical := fmt.Sprintf("%s:%s", storage, volName)

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
				rollbackCtx := contextWithoutCancel(ctx)
				if exists, exErr := deps.PVE.Storage().Exists(rollbackCtx, node, storage, candidateCanonical); exErr == nil && exists {
					upid, delErr := deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, candidateCanonical)
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
	// filename. Use the canonical form when empty so the disk_cid is always
	// well-formed. The volid is already a valid disk CID; re-prefixing with
	// storage would double it (e.g. "data:data:...").
	diskCID = volid
	if diskCID == "" {
		diskCID = canonicalVolID
	}
	// Append placement metadata so downstream handlers (attach_disk and
	// fault-domain co-location in create_vm) can read pool, node, and AZ
	// without an extra PVE API call. Pool is always the resolved storage;
	// node is the PVE node that holds the volume (meaningful for local-backend
	// deployments). AZ is set from cloud_properties.availability_zone when
	// provided; otherwise left empty so the CID is backward-compatible.
	diskCID = pve.EncodeDiskCID(diskCID, &pve.DiskCIDMeta{
		Pool: storage,
		Node: node,
		AZ:   az,
		Opts: diskPerfOpts,
	})
	return namingVMID, diskCID, canonicalVolID, nil
}

// rollbackCreatedVolume best-effort deletes a successfully created volume when
// a subsequent step in create_disk fails. Uses contextWithoutCancel so cleanup
// completes even if the caller cancelled the request. Logs errors; never panics.
func rollbackCreatedVolume(
	ctx context.Context,
	deps Deps,
	node, storage, canonicalVolID string,
	logger *log.Logger,
) {
	rollbackCtx := contextWithoutCancel(ctx)
	upid, delErr := deps.PVE.Storage().DeleteVolumeAsync(rollbackCtx, node, storage, canonicalVolID)
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

// contextWithoutCancel returns a context derived from parent that carries
// parent's values but is detached from its cancellation/deadline. Used so
// rollback I/O completes even when the caller cancelled the request.
func contextWithoutCancel(parent context.Context) context.Context {
	if parent == nil {
		return context.Background()
	}
	return context.WithoutCancel(parent)
}
