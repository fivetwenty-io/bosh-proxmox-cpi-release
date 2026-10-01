package pve

import (
	"context"
	"fmt"
	"sort"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// VolumeReference is one config key of one guest that names a volume, either
// an active bus slot ("scsi1") or an unused entry ("unused0").
type VolumeReference struct {
	VMID int
	Node string
	Slot string
}

// FindVolumeReferences reads every guest's current config and returns each
// active bus slot and each unusedN key whose bare volid is volid, sorted by
// VMID and then slot. It is the sole-reference proof a caller needs before it
// treats one guest as the only holder of a volume that no active slot names,
// which the identity scan cannot give because it stops at the first active
// match and never reports unused entries.
//
// It reads current configs only, never snapshot sections. A volume that a
// snapshot still references therefore does not show up here, but PVE refuses
// to move such a volume to another VM ("Can't move disk used by a snapshot to
// another VM"), so a transfer built on this answer fails and leaves the volume
// where it was.
//
// Guests come from the strict ListGuestsAuthoritative, because an empty or
// single-entry answer is an absence proof: a node that cannot be listed fails
// the call retriable rather than hiding a reference. A guest whose config read
// answers 404 was deleted after the listing and holds nothing; any other read
// error fails the call, the same way the identity scan treats one.
func FindVolumeReferences(ctx context.Context, c Client, volid string) ([]VolumeReference, error) {
	if c == nil {
		return nil, cpierrors.Cloud("FindVolumeReferences: client must not be nil")
	}
	if volid == "" {
		return nil, cpierrors.Cloud("FindVolumeReferences: volid must not be empty")
	}
	guests, err := ListGuestsAuthoritative(ctx, c, nil)
	if err != nil {
		return nil, cpierrors.Wrap(err, "FindVolumeReferences: enumerate cluster guests")
	}
	var refs []VolumeReference
	for _, g := range guests {
		views, cfgErr := ReadQemuViews(ctx, c, g.Node, g.VMID)
		if cfgErr != nil {
			if IsNotFound(cfgErr) {
				continue
			}
			return nil, cpierrors.Wrap(
				WrapConfigReadError(cfgErr),
				fmt.Sprintf("FindVolumeReferences: Config error for vm %d on node %s", g.VMID, g.Node),
			)
		}
		// A key counts when either view names the volume, so a slot whose
		// delete is pending, which the config endpoint hides, is still a
		// reference.
		for _, slot := range views.SlotsNaming(volid) {
			refs = append(refs, VolumeReference{VMID: g.VMID, Node: g.Node, Slot: slot})
		}
	}
	sortVolumeReferences(refs)
	return refs, nil
}

// sortVolumeReferences orders references by VMID and then slot, so a caller
// that names them in an error or compares two lists sees a stable order.
func sortVolumeReferences(refs []VolumeReference) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].VMID != refs[j].VMID {
			return refs[i].VMID < refs[j].VMID
		}
		return refs[i].Slot < refs[j].Slot
	})
}
