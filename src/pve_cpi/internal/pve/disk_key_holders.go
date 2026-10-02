package pve

import (
	"context"
	"fmt"
	"slices"
	"sort"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// DiskKeyHolder is one disk key of one guest that names a volume or carries a
// disk's stable-ID serial, in either view.
type DiskKeyHolder struct {
	VMID int
	Node string
	Slot string
	// Parker is set when either view's tags mark the guest as a parker.
	Parker bool
	// NamesVolume is set when the key's current or pending value names the
	// volume.
	NamesVolume bool
	// CarriesSerial is set when the key's current or pending value carries
	// the serial.
	CarriesSerial bool
	// StillNames is MoveSourceStillNames for the key and the volume, read
	// from the same views, so the applied value names the volume and the key
	// has no pending delete and no pending replacement.
	StillNames bool
}

// FindDiskKeyHolders reads every guest's configuration in both views and returns
// each disk key, bus slot or unused entry, that names volume or carries
// serial, sorted by VMID and then slot. Unlike the identity scan it never
// stops at the first match, so a caller proving that only one known key holds
// a disk sees every other holder. A key counts when its current value or its
// pending value matches, so a slot whose delete is pending and a slot whose
// replacement is pending both still hold the disk.
//
// Guests come from the strict ListGuestsAuthoritative, because an answer with
// no other holder is an absence proof. A guest whose config read answers 404
// was deleted after the listing and holds nothing, and any other read error
// fails the call.
func FindDiskKeyHolders(ctx context.Context, c Client, volume, serial string) ([]DiskKeyHolder, error) {
	if c == nil {
		return nil, cpierrors.Cloud("FindDiskKeyHolders: client must not be nil")
	}
	if volume == "" || serial == "" {
		return nil, cpierrors.Cloud("FindDiskKeyHolders: volume and serial must not be empty")
	}
	guests, err := ListGuestsAuthoritative(ctx, c, nil)
	if err != nil {
		return nil, cpierrors.Wrap(err, "FindDiskKeyHolders: enumerate cluster guests")
	}
	var holders []DiskKeyHolder
	for _, g := range guests {
		views, cfgErr := ReadQemuViews(ctx, c, g.Node, g.VMID)
		if cfgErr != nil {
			if IsNotFound(cfgErr) {
				continue
			}
			return nil, cpierrors.Wrap(
				WrapConfigReadError(cfgErr),
				fmt.Sprintf("FindDiskKeyHolders: Config error for vm %d on node %s", g.VMID, g.Node),
			)
		}
		appliedTags, _ := ConfigString(views.Applied(), "tags")
		currentTags, _ := ConfigString(views.Current(), "tags")
		parker := TagsMarkParker(appliedTags) || TagsMarkParker(currentTags)
		for key, entry := range views.entries {
			if !isQemuDiskKey(key) {
				continue
			}
			holder := DiskKeyHolder{VMID: g.VMID, Node: g.Node, Slot: key, Parker: parker}
			for _, value := range []struct {
				present bool
				text    string
			}{{entry.hasValue, entry.value}, {entry.hasPending, entry.pending}} {
				if !value.present {
					continue
				}
				if bareDriveVolid(value.text) == volume {
					holder.NamesVolume = true
				}
				if found, ok := StableIDFromDriveOptStr(value.text); ok && found == serial {
					holder.CarriesSerial = true
				}
			}
			if holder.NamesVolume || holder.CarriesSerial {
				holder.StillNames = MoveSourceStillNames(views, key, volume)
				holders = append(holders, holder)
			}
		}
	}
	sortDiskKeyHolders(holders)
	return holders, nil
}

func sortDiskKeyHolders(holders []DiskKeyHolder) {
	sort.Slice(holders, func(i, j int) bool {
		if holders[i].VMID != holders[j].VMID {
			return holders[i].VMID < holders[j].VMID
		}
		return holders[i].Slot < holders[j].Slot
	})
}

// DiskTransferRecord is one parker's record of a disk's transfer, the entry
// keyed by the disk's stable ID in the parker's description.
type DiskTransferRecord struct {
	ParkerVMID  int
	ParkerNode  string
	Slot        string
	Volid       string
	SourceVMCID string
	// UnclaimedLanding names the first disk key of the parker, bus slot or
	// unused entry, that holds in either view a volume named for the parker
	// on a value with no stable-ID serial, or is empty when no key does.
	// That's what a move leaves when it lands after its answer is lost,
	// because the serial is written only after the move. Every key counts,
	// not only Slot, because a resume can land on a fallback slot, and a
	// landing of another disk counts too, because nothing on the parker tells
	// the two apart. It's read from the same views as the record.
	UnclaimedLanding string
}

// FindDiskTransferRecords reads every guest's configuration in both views and
// returns each transfer record keyed by stableID on a guest whose tags, in
// either view, mark it as a parker, sorted by parker VMID. A record that
// differs between the two views' descriptions comes back once for each view.
//
// Guests come from the strict ListGuestsAuthoritative, because an answer with
// no record is an absence proof. A guest whose config read answers 404 was
// deleted after the listing and holds nothing, and any other read error fails
// the call, the same rule FindDiskKeyHolders follows.
func FindDiskTransferRecords(ctx context.Context, c Client, stableID string) ([]DiskTransferRecord, error) {
	if c == nil {
		return nil, cpierrors.Cloud("FindDiskTransferRecords: client must not be nil")
	}
	if stableID == "" {
		return nil, cpierrors.Cloud("FindDiskTransferRecords: stable ID must not be empty")
	}
	guests, err := ListGuestsAuthoritative(ctx, c, nil)
	if err != nil {
		return nil, cpierrors.Wrap(err, "FindDiskTransferRecords: enumerate cluster guests")
	}
	var records []DiskTransferRecord
	for _, g := range guests {
		views, cfgErr := ReadQemuViews(ctx, c, g.Node, g.VMID)
		if cfgErr != nil {
			if IsNotFound(cfgErr) {
				continue
			}
			return nil, cpierrors.Wrap(
				WrapConfigReadError(cfgErr),
				fmt.Sprintf("FindDiskTransferRecords: Config error for vm %d on node %s", g.VMID, g.Node),
			)
		}
		appliedTags, _ := ConfigString(views.Applied(), "tags")
		currentTags, _ := ConfigString(views.Current(), "tags")
		if !TagsMarkParker(appliedTags) && !TagsMarkParker(currentTags) {
			continue
		}
		var seen []DiskTransferRecord
		for _, view := range []map[string]any{views.Applied(), views.Current()} {
			_, disks, _ := parseParkerSentinel(DescriptionFromConfig(view))
			entry, ok := disks[stableID]
			if !ok {
				continue
			}
			record := DiskTransferRecord{
				ParkerVMID:  g.VMID,
				ParkerNode:  g.Node,
				Slot:        entry.Slot,
				Volid:       entry.Volid,
				SourceVMCID: entry.SourceVMCID,
			}
			record.UnclaimedLanding = parkerUnclaimedLanding(views, g.VMID)
			if !slices.Contains(seen, record) {
				seen = append(seen, record)
			}
		}
		records = append(records, seen...)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].ParkerVMID < records[j].ParkerVMID })
	return records, nil
}

// parkerUnclaimedLanding returns the first disk key of a parker, in key
// order, whose current or pending value is a volume named for the parker with
// no stable-ID serial, or "" when no key holds one.
func parkerUnclaimedLanding(views QemuViews, parkerVMID int) string {
	keys := make([]string, 0, len(views.entries))
	for key := range views.entries {
		if isQemuDiskKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := views.entries[key]
		for _, value := range []struct {
			present bool
			text    string
		}{{entry.hasValue, entry.value}, {entry.hasPending, entry.pending}} {
			if !value.present {
				continue
			}
			if embedded, named := EmbeddedDiskVMID(bareDriveVolid(value.text)); !named || embedded != parkerVMID {
				continue
			}
			if _, hasSerial := StableIDFromDriveOptStr(value.text); !hasSerial {
				return key
			}
		}
	}
	return ""
}
