package handlers

import (
	"errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
func parkerReadConfigFor(deps Deps) pve.ParkerConfig {
	return pve.ParkerConfig{
		VMIDRangeStart: deps.Config.ParkedDiskVMIDRangeStartValue(),
		VMIDRangeEnd:   deps.Config.ParkedDiskVMIDRangeEndValue(),
		DirectorID:     deps.RequestDirectorUUID,
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
