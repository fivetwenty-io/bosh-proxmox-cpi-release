// Proof that a volume is no longer on storage, for the callers that have to
// fail closed when they cannot establish it: the delete_disk parked-anchor
// refusal, the has_disk answer bosh cck reads, the stale-slot check in
// delete_vm, and the local-backend cluster scan.
package pve

import (
	"context"
	"fmt"
	"strings"
)

// StorageClassifier answers what kind of storage we are proving against. It is
// consulted only after the point probe has failed and we are about to read a
// listing, so the fast paths pay nothing for it. A false return means the
// storage could not be identified, which is not a license to guess.
type StorageClassifier func(context.Context) (StorageInfo, bool)

// ProveVolumeAbsent reports whether volume is provably not on storage, as
// observed from node. (true, nil) means the volume is not there. (false, nil)
// means it is. A non-nil error means the observation did not land and the
// caller has to fail closed, because nothing about the volume has been
// established.
//
// The volume argument is a full volid such as "nfs-images:9000/vm-9000-disk-0.qcow2",
// and storage names the storage it lives on.
func ProveVolumeAbsent(
	ctx context.Context, client Client, node, storage, volume string, classify StorageClassifier,
) (bool, error) {
	if ctx == nil || client == nil || node == "" {
		return false, fmt.Errorf("volume absence proof requires context, client, and node")
	}
	if storage == "" || volume == "" {
		return false, fmt.Errorf("volume absence proof requires a storage name and a volume")
	}
	// The point probe reads the storage service, so a client without one
	// cannot start. That is a wiring fault rather than a cluster condition,
	// and on a delete path it has to fail closed rather than panic on the nil
	// interface. The nodes service is checked where the listing needs it, so a
	// caller whose point probe answers is not held to a surface it never uses.
	if client.Storage() == nil {
		return false, fmt.Errorf("volume absence proof requires the storage service")
	}
	exists, err := ExistsTolerant(ctx, client, node, storage, volume)
	if err == nil {
		return !exists, nil
	}
	// The point probe answers with a 404 on some backends and with CLI text on
	// the block ones, and ExistsTolerant folds both. On file storage it answers
	// with "volume_size_info ... failed - no format" for a stat that did not
	// work, whatever stopped it, so the reply cannot be read either way. The
	// info handler behind it never activates the storage, so it cannot tell a
	// missing file from an export that went away. Only a content listing can.
	if client.Nodes() == nil {
		return false, fmt.Errorf(
			"volume absence proof needs the nodes service to read a content listing (point probe: %s)", err.Error())
	}
	found, listed, observeErr := ObserveStorageVolumeContent(ctx, client, node, volume)
	if observeErr != nil {
		// Both observations failed, so the caller's warning is the only place
		// an operator will learn why. The observation error stays the wrapped
		// one, so errors.Is still matches the bounded diagnostic category the
		// managed paths test for, and the point probe's own reason rides
		// along as text rather than as a second wrapped error, so nothing
		// downstream starts classifying this outcome from the probe's HTTP
		// code. Callers log it through log.Err, which scrubs URL credentials.
		return false, fmt.Errorf("%w (point probe: %s)", observeErr, err.Error())
	}
	if found {
		return false, nil
	}
	if proofErr := listingProvesAbsence(ctx, storage, listed, classify); proofErr != nil {
		return false, proofErr
	}
	return true, nil
}

// listingProvesAbsence reports whether a volid missing from a successful
// listing actually establishes that the volume is gone. It does on any storage
// PVE refuses to activate when its backing is unreachable: nfs and cifs, dir
// and btrfs carrying is_mountpoint, and the block and Ceph plugins whose own
// tooling fails rather than listing nothing. A plain dir storage with no
// is_mountpoint lists an empty array when its mount drops, and activate_storage
// recreates images/ under the bare path, so there an empty listing says nothing
// and only other volumes in the listing prove the tree is really there.
func listingProvesAbsence(ctx context.Context, storage string, listed int, classify StorageClassifier) error {
	if classify == nil {
		return fmt.Errorf("storage %s could not be classified, so an empty content listing proves nothing", storage)
	}
	info, ok := classify(ctx)
	if !ok {
		return fmt.Errorf("storage %s could not be classified, so an empty content listing proves nothing", storage)
	}
	if info.IsMountpoint || listingFailsWhenBackingIsGone(info.Type) {
		return nil
	}
	// Everything else is a path-based plugin, or a type we do not recognize.
	// Other volumes in the listing prove the tree is really mounted; an empty
	// listing proves nothing at all.
	if listed > 0 {
		return nil
	}
	return fmt.Errorf(
		"storage %s is %s with no is_mountpoint and its content listing came back empty, "+
			"which a dropped mount produces as readily as a genuinely empty storage; set is_mountpoint "+
			"on that storage so PVE reports it offline instead of listing nothing",
		storage, describeStorageType(info.Type),
	)
}

// describeStorageType names the storage in the unproven message. A classifier
// can answer with a storage whose type is empty: the local backend's cache-miss
// fallback fabricates a StorageInfo carrying only the name, and it reports that
// as a successful classification. Rendering the type verbatim there produced
// "is a  storage", so an empty type is named for what it is. The conservative
// rule is unchanged either way, because an unclassified storage takes the same
// path as an unrecognized one: only other volumes in the listing prove the tree
// is really there.
func describeStorageType(storageType string) string {
	if strings.TrimSpace(storageType) == "" {
		return "an unclassified storage"
	}
	return "a " + storageType + " storage"
}

// listingFailsWhenBackingIsGone names the storage types whose own listing path
// errors rather than returning nothing when the backing is unreachable, so a
// volid missing from a successful listing is an absence. It is an allow-list on
// purpose: a plugin nobody anticipated lands on the conservative side, where we
// ask to see other volumes before believing an empty answer.
//
// dir and btrfs are left out because they are the path-based plugins this rule
// exists for, glusterfs joins them because a fuse mount can also present an
// empty directory once its backing goes away, and pbs never holds disk volumes,
// so it never reaches here.
func listingFailsWhenBackingIsGone(storageType string) bool {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case StorageTypeNFS, StorageTypeCIFS, StorageTypeRBD, StorageTypeCephFS,
		StorageTypeLVM, StorageTypeLVMThin, StorageTypeZFSPool:
		return true
	}
	return false
}
