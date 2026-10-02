package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// absentDiskSource is a VM that one of a record's observed moves took the disk
// off, together with the name the disk had there and the backing the move
// recorded.
type absentDiskSource struct {
	node    string
	old     string
	backing string
}

// absentDiskRenamedSources returns, for each VM a record's observed moves took
// the disk off under one name and landed under another, the node and old
// name of each such move. A later move that may have brought the disk back
// under the old name rules that name out, the same way the completion audit
// keeps it the record's (storageAuditRenamedAway).
func absentDiskRenamedSources(record aj.Record) map[int][]absentDiskSource {
	sources := map[int][]absentDiskSource{}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed || !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
			continue
		}
		if step.Target.VMID <= 0 || step.Target.Node == "" || len(step.VolIDs) < 2 {
			continue
		}
		old, landed := step.VolIDs[0], step.VolIDs[len(step.VolIDs)-1]
		if old != step.Target.IntendedVolume || landed == "" || landed == old {
			continue
		}
		if !storageAuditRenamedAway(record, old, step.Target.Node, false) {
			continue
		}
		source := absentDiskSource{node: step.Target.Node, old: old, backing: step.Target.Backing}
		if !slices.Contains(sources[step.Target.VMID], source) {
			sources[step.Target.VMID] = append(sources[step.Target.VMID], source)
		}
	}
	return sources
}

// removeAbsentDiskNotes removes the allocation entry that a renamed disk's
// earlier release left on the VM the disk was moved off, once the disk's
// volume is gone and another disk holds the old name on a bus slot of that
// VM. The entry still names the old name, so without this the completion
// audit counts it as the disk's and refuses every delete_disk. The entry goes
// only when it is keyed by the disk's token, names the record's allocation,
// namespace, and backing, and records the node, old name, and backing of an
// observed move that renamed the disk off that VM. The same write removes the
// attached-disk notes whose value is the disk's own CID. Nothing else in the
// description changes, and no note goes whose key names a volume the VM still
// holds on a slot or an unused entry. A VM that holds the old name on an
// unused entry keeps everything, because that volume could still be the
// disk's own data, and so does a VM that holds the old name nowhere. A VM
// that's gone leaves nothing to do. record is the disk's journal record as
// the caller holds it under the journal's lock.
func removeAbsentDiskNotes(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record) error {
	if rd.allocation == nil || rd.stableID == "" {
		return nil
	}
	for vmid, sources := range absentDiskRenamedSources(record) {
		location, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if err != nil {
			return retriableUnlessPermanent(err, fmt.Sprintf("delete_disk: locate VM %d to remove disk %s's allocation entry", vmid, rd.diskCID))
		}
		if !location.Found {
			continue
		}
		if err := removeAbsentDiskNotesOn(ctx, deps, rd, record, location.Node, vmid, sources); err != nil {
			return retriableUnlessPermanent(err, fmt.Sprintf("delete_disk: remove disk %s's allocation entry from VM %d", rd.diskCID, vmid))
		}
	}
	return nil
}

// removeAbsentDiskNotesOn is removeAbsentDiskNotes for one VM on node. It
// reads the VM once in both views and builds the one write from that read.
func removeAbsentDiskNotesOn(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, node string, vmid int, sources []absentDiskSource) error {
	views, err := pve.ReadQemuViews(ctx, deps.PVE, node, vmid)
	if err != nil {
		if pve.IsNotFound(err) {
			return nil
		}
		return err
	}
	config := views.Applied()
	description := pve.DescriptionFromConfig(config)
	entries, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return err
	}
	entry, found := entries[rd.stableID]
	if !found || entry.AllocationID != record.ID || entry.AllocationNamespace != record.Namespace || entry.Backing != rd.allocation.provenance.Backing {
		return nil
	}
	if !slices.Contains(sources, absentDiskSource{node: entry.Node, old: entry.Volid, backing: entry.Backing}) {
		return nil
	}
	held := views.SlotsNaming(entry.Volid)
	if len(held) == 0 {
		return nil
	}
	for _, key := range held {
		if strings.HasPrefix(key, "unused") {
			return nil
		}
	}
	var attached []string
	for key, cid := range pve.GetAttachedDiskCIDs(description) {
		if cid == rd.diskCID && !views.NamesVolume(key) {
			attached = append(attached, key)
		}
	}
	slices.Sort(attached)
	// This write isn't journaled. It removes only our own notes about a disk
	// whose volume is already gone, and it carries the digest of the read it
	// was built from, so PVE refuses it rather than overwrite a newer
	// description. Every call reads the VM again and decides afresh, so a
	// rerun repairs a write whose outcome we never learned.
	return pve.RemoveDiskAllocationEntry(ctx, deps.PVE, node, vmid, rd.stableID, entry, config, attached, nil)
}
