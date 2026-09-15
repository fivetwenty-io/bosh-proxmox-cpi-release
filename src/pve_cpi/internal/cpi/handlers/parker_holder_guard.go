package handlers

import (
	"context"
	"errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// parkerReadConfigFor builds the parker band and attribution a READ path should
// use: resolving a volume's holder, deciding whether that holder is a parker,
// and unparking from it. The band comes from the effective accessors, which
// resolve unset bounds to the built-in band under every strategy; only a band
// moved away from where parker VMs already live leaves a parker outside it --
// which is precisely the state the stranded-parker guards exist for.
//
// It deliberately leaves DiskStorage empty. That field feeds the storage-content
// scan pve.ParkDisk uses when it allocates a parker VMID, and no path built on
// this config allocates one. A caller that parks a disk must build its own
// ParkerConfig with DiskStorage set -- see parkAfterDetach -- or it reopens the
// cross-cluster parker-VMID collision that frees another cluster's parked disks.
// An empty DiskStorage makes WithStorageScan a silent no-op rather than an
// error, so nothing catches that misuse at runtime; the name is the guard. Do
// not populate the field to make this helper park-safe -- that would only make
// it LOOK park-safe while still missing whatever park-only field comes next.
//
// Prefix and Pool are resolved here rather than at each park site, because this
// is the one builder every parker path shares and because both accessors resolve
// against the effective per-request config. A per-entry pve_vm_prefix or
// pve_parker_prefix override therefore moves the parker names and the parker
// pool together, on the request it was routed to.
func parkerReadConfigFor(deps Deps) pve.ParkerConfig {
	return pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.RequestDirectorUUID,
		// The prefix names a parker VM, and the pool is rendered from it. Both
		// fall back to values that reproduce prior releases byte for byte when
		// an operator sets neither, so a read path that never creates a parker
		// carries them harmlessly.
		Prefix: deps.Config.ParkerPrefixValue(),
		Pool:   deps.Config.ParkerPoolValue(),
		// The holder scan drops any /cluster/resources row that elides "node"
		// unless it has a fallback, and on a PVE that elides it for every row
		// that means the scan finds no holder at all -- so attach_disk's
		// double-attach guard never fires and delete_disk's stranded-parker
		// refusal never fires. resize_disk already passes Config.Node to the
		// same scan for the same reason.
		FallbackNode: deps.Config.Node,
		// Log-level hint only: the in-band-without-tag anomaly warns under
		// "parked" and logs at debug under "free"/stand-down, where the band
		// may legitimately overlap vmid_range.
		ParkedEnabled: deps.Config.DetachedDiskParkedEnabled(),
		// A parker that vanishes between the cluster listing and its config
		// read (or before an unpark detach) is refused under strict rather
		// than silently treated as free-floating. See ParkerConfig.AnchorStrict.
		AnchorStrict: deps.Config.ParkedAnchorStrictValue(),
	}
}

// wrapHolderScanError labels a failed holder resolution for the Director.
//
// The scan is overwhelmingly a transport-shaped operation -- a cluster listing
// plus a config read per VM -- so retriable is the right default and the one the
// Director needs to re-drive around a node that is briefly unreachable. But
// pve.ResolveDiskHolder can also return permanently broken conditions ("nil
// response from cluster resources", "client must not be nil"), and forcing
// those into retriable turns a defect that will never come right into a
// Director retry loop that never ends. An error that already carries a
// non-retriable type keeps it.
func wrapHolderScanError(err error, msg string) error {
	return retriableUnlessPermanent(err, msg)
}

// retriableUnlessPermanent labels err retriable unless it already carries a
// permanent class, which it keeps. It is the right default for the parker paths
// the Director drives: they are overwhelmingly transport shaped, so retriable is
// what re-drives them around a node that is briefly unreachable, while the few
// conditions that will never come right on their own -- a permission to grant,
// a reference an operator has to clear by hand -- have already said so and must
// not be turned back into a loop.
func retriableUnlessPermanent(err error, msg string) error {
	// The test is on whether the error carries a class at all, not on one
	// permanent type. Checking for TypeCloud alone would relabel every other
	// permanent class -- a disk that is not there, a VM that is not there, an
	// unsupported request -- as retriable the moment one of the parker paths
	// starts returning it.
	if isTypedCPIError(err) {
		return cpierrors.Wrap(err, msg)
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, msg)
}

// isTypedCPIError reports whether err already carries a CPI error type, which
// means something upstream classified it deliberately and that classification
// should survive the wrap. An untyped error carries no decision to preserve.
func isTypedCPIError(err error) bool {
	var typed *cpierrors.Error
	return errors.As(err, &typed)
}

// okToRetryCPIError reports whether err carries a *cpierrors.Error anywhere
// in its chain whose type the BOSH Director may retry (ok_to_retry).
func okToRetryCPIError(err error) bool {
	var typed *cpierrors.Error
	return errors.As(err, &typed) && typed.OkToRetry()
}

// anchorMissingRefusal returns the strict-mode refusal for a disk whose CID
// envelope promises a parker anchor while the holder scan found no holder at
// all. Under the parked strategy a promised disk is on a parker whenever it is
// detached (create_disk parks it at birth, detach_disk re-parks it), so
// "promised and free-floating" has one cause: a parker VM was deleted
// out-of-band, and every operation that then treats the volume as free runs
// against a disk whose anchor — protection flag, provenance, PVE-visible
// ownership — silently vanished.
//
// Permissive outcomes, in order: a holder was found (the normal guards apply);
// the CID carries no promise (legacy disks and disks created under "free" —
// their anchor was never promised); the strategy is not "parked" (free-floating
// is the expected detached state there); pve.parked_anchor_strict is false
// (the escape hatch for labs that intentionally delete parkers — logged so the
// choice is visible).
func anchorMissingRefusal(ctx context.Context, deps Deps, method, diskCID string, meta *pve.DiskCIDMeta, holder pve.DiskHolder) error {
	if holder.Found {
		return nil
	}
	if meta == nil || !meta.Anchor {
		return nil
	}
	if !deps.Config.DetachedDiskParkedEnabled() {
		return nil
	}
	if !deps.Config.ParkedAnchorStrictValue() {
		deps.Log(ctx).Warn("parked anchor missing; proceeding because pve.parked_anchor_strict is false",
			log.String("method", method),
			log.String("disk_cid", diskCID),
		)
		return nil
	}
	return cpierrors.Cloud(
		"%s: disk %s was created under the parked strategy and its CID promises a parker anchor, but no VM "+
			"in the cluster references the volume; the parker holding it was likely deleted out-of-band. "+
			"Verify the volume is intact on storage, then set pve.parked_anchor_strict: false to proceed "+
			"against the free-floating volume and retry",
		method, diskCID,
	)
}

// proveAnchorVolumeGone reports whether the volume behind a promised anchor is
// provably not on storage, anywhere the volume could be. The anchor refusal
// reads "no holder" as a parker deleted out of band, which is only the right
// reading while the volume is still there. An absence we cannot prove is not
// an absence, so the caller keeps its refusal, and the warning says so: the
// operator is looking at a refusal standing on an unproven absence, not at an
// established fact.
//
// The attach paths must not pass their own node in. By the time
// guardAndUnparkBeforeAttach runs, the node has been retargeted to the VM the
// disk is being attached to, and create_vm passes the node its placement
// chose. Probing a node-local storage from a node the disk was never on reads
// as a clean absence on lvmthin and zfspool, whose "Failed to find logical
// volume" and "dataset does not exist" replies fold into one, so an operator
// would be told the data is gone about a disk sitting healthy on its own node.
// The disk's location is resolved from its own volid instead.
func proveAnchorVolumeGone(ctx context.Context, deps Deps, method, diskCID, volid string) bool {
	gone, err := anchorVolumeAbsentAnywhere(ctx, deps, volid)
	return anchorAbsenceProven(ctx, deps, method, diskCID, "every node that could hold the volume", gone, err)
}

// proveAnchorVolumeGoneAt is the same question asked of one node, for a caller
// that already resolved the disk's own node through its backend. delete_disk
// is the only one: its node comes from NodeForExisting on the volume's
// storage, which is the node the volume is actually on.
func proveAnchorVolumeGoneAt(ctx context.Context, deps Deps, method, diskCID, volid, node string) bool {
	gone, err := volumeAbsentFromStorage(ctx, deps, node, volid)
	return anchorAbsenceProven(ctx, deps, method, diskCID, "node "+node, gone, err)
}

// anchorAbsenceProven reports the probe's verdict and, when the probe did not
// reach one, writes the warning that says the refusal about to be returned is
// standing on an unproven absence.
//
// log.Err delegates to ErrScrubbed, so the probe error reaches the sink with
// URL credentials masked, which is the house rule for logging an error this
// package did not construct.
func anchorAbsenceProven(ctx context.Context, deps Deps, method, diskCID, scope string, gone bool, err error) bool {
	if err != nil {
		deps.Log(ctx).Warn("parked anchor missing and the volume's absence could not be proven; the refusal stands",
			log.String("method", method),
			log.String("disk_cid", diskCID),
			log.String("probe_scope", scope),
			log.Err(err),
		)
		return false
	}
	return gone
}

// anchorVolumeAbsentAnywhere reports whether the volume is provably not on
// storage, asking the question of wherever the volume's own storage can put
// it rather than of a node the caller happens to hold.
//
// A shared storage is visible from any node, so one probe against the
// backend's node settles it. A node-local storage needs the cluster sweep, and
// NodeForExisting already is that sweep: it runs the same absence proof
// against every candidate node, returns the node on the first volume it
// finds, answers DiskNotFound only when every node proved the volume absent,
// and turns any node it could not ask into a retriable error rather than a
// miss. Reusing it keeps one sweep in the codebase instead of two.
func anchorVolumeAbsentAnywhere(ctx context.Context, deps Deps, volid string) (bool, error) {
	storage, _, err := pve.ParseDiskCID(volid)
	if err != nil {
		return false, err
	}
	backend, resolveErr := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if resolveErr != nil {
		return false, resolveErr
	}
	node, nodeErr := backend.NodeForExisting(ctx, volid)
	if nodeErr != nil {
		if backend.Kind() == pve.BackendLocal && pve.IsNotFound(nodeErr) {
			// Every candidate node answered a clean absence, which is the
			// complete sweep this conclusion requires.
			return false, nil
		}
		return false, nodeErr
	}
	if backend.Kind() == pve.BackendLocal {
		// A node came back, so a node holds the volume.
		return false, nil
	}
	return volumeAbsentFromStorage(ctx, deps, node, volid)
}

// anchorVolumeGoneRefusal returns the refusal for a promised anchor whose
// parker and whose volume are both gone. It is deliberately a different
// message from anchorMissingRefusal, and deliberately carries no strict-mode
// advice: relaxing pve.parked_anchor_strict lets a caller proceed against a
// free-floating volume, and there is no volume left to proceed against. The
// only way forward is to take the disk out of the Director's records.
//
// The class is cpierrors.Cloud rather than DiskNotFound. DiskNotFound is
// semantically closer, but it is a class the Director acts on, and an attach
// step acting on it for a disk the deployment manifest still asks for is not a
// behavior we want to discover in the field.
func anchorVolumeGoneRefusal(method, diskCID, volid string) error {
	return cpierrors.Cloud(
		"%s: disk %s was created under the parked strategy and its CID promises a parker anchor, but no VM "+
			"in the cluster references the volume and the volume %s is not on storage; the parker VM and the "+
			"disk it held were both removed out-of-band, so the data is gone. Remove the disk from the "+
			"Director's records with `bosh -d <deployment> cck` (or drop it from the create-env state file) "+
			"and redeploy to have a fresh disk created",
		method, diskCID, volid,
	)
}

// strandedParkerRefusal returns a refusal when a volume's holder carries the
// bosh-parker tag but sits outside the configured band, and nil otherwise.
//
// That combination has one cause: the parker band was moved away from where
// parker VMs already live while disks were still parked. (An unset band
// resolves to the built-in one under every strategy, so opting out with
// detached_disk_strategy "free" no longer creates this state on its own.) The
// CPI can no longer recognize the parker, so it sees an ordinary VM holding the
// volume -- and every operation that would then treat the volume as free
// (attaching it elsewhere, deleting it) leaves the parker's scsi slot pointing
// at bytes that are about to belong to someone else, or to no one.
//
// It reads the tags the holder scan already carried out of the config it read
// to identify the holder, so it makes no API call and has no error path. That
// matters more than the saved call: a version of this that read the config
// itself had to decide what an unreadable config meant, and for delete_disk the
// only available answer -- carry on -- is the one that destroys the volume.
func strandedParkerRefusal(deps Deps, method, diskCID string, holder pve.DiskHolder) error {
	if !holder.Found || holder.IsParker {
		return nil
	}
	if !pve.TagsMarkParker(holder.Tags) {
		return nil
	}
	return cpierrors.Cloud(
		"%s: disk %s is parked on VM %d (node %s), which carries the bosh-parker tag but falls outside "+
			"the configured parker band [%d,%d], so it cannot be unparked; set "+
			"parked_disk_vmid_range_start/end to the band those parker VMs occupy "+
			"(90000/90999 unless it was changed) and retry",
		method, diskCID, holder.VMID, holder.Node,
		deps.Config.ParkedDiskVMIDRangeStartValue(), deps.Config.ParkedDiskVMIDRangeEndValue(),
	)
}
