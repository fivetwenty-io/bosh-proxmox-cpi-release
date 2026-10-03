package handlers

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// absentDiskSource describes one observed move that took the disk off a VM. It
// holds the node the move ran on, the name the disk had on that VM, and the
// backing the move recorded.
type absentDiskSource struct {
	node    string
	old     string
	backing string
}

// absentDiskRenamedSources returns the observed moves of a record that took the
// disk off a VM under one name and landed it under another, grouped by that
// VM. Each entry has the move's node and the old name. A later move that may
// have brought the disk back under the old name rules that name out, the same
// way the completion audit keeps that name as the record's own
// (storageAuditRenamedAway).
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

// absentDiskNames returns every name the disk has had that the record or the
// disk itself knows. Those are the disk's current and birth names and every
// volid that one of the record's observed steps names.
//
// Most of the volids that the steps name are names the disk gave up long ago,
// and PVE may have handed one of them to another disk since. Counting them
// anyway only ever makes the heal refuse more. The heal uses these names in
// two ways. A VM that holds any of them keeps its notes, so an extra name can
// only add a hold. An allocation entry goes only when it names one of them,
// and it must also be filed under the disk's stable ID and name the record's
// allocation, namespace, and backing, so an extra name can't make another
// disk's entry look like ours.
func absentDiskNames(record aj.Record, rd resolvedDisk) []string {
	names := []string{rd.volid, rd.birth}
	for i := range record.Steps {
		if record.Steps[i].State == aj.Observed {
			names = append(names, record.Steps[i].VolIDs...)
		}
	}
	names = slices.DeleteFunc(names, func(name string) bool { return name == "" })
	sort.Strings(names)
	return slices.Compact(names)
}

// absentDiskRecordNode reports whether node is one that the record's own disk
// provenance or one of its steps names.
func absentDiskRecordNode(record aj.Record, rd resolvedDisk, node string) bool {
	if node == rd.allocation.provenance.Node {
		return true
	}
	return slices.ContainsFunc(record.Steps, func(step aj.Step) bool { return step.Target.Node == node })
}

// absentDiskLeftoverVMs returns, sorted, the VMs the audit found carrying the
// record's allocation in their description, each with the node the audit
// found it on. It returns none unless the audit is complete, found no
// conflict, and saw the disk absent. The audit saw the disk held when the VM
// scan counted a drive as the disk's, when a VM carries a VM marker of the
// allocation, or when a volume of the allocation is listed on any storage. The
// drive and the listed volume each have one exception, which is an old name,
// meaning a name the disk had before an observed move renamed it. PVE may have
// given that name to another disk since. A drive without a serial under an old name, on the VM and node
// the move left, doesn't count when the audit counted it only because that
// VM's leftover entry names it. A volume listed under an old name doesn't
// count when the listing is on the move's node, or on any node for shared
// storage, and a VM's leftover entry still names the volume. A VM marker has
// no exception.
func absentDiskLeftoverVMs(report StorageAllocationAudit, record aj.Record, sources map[int][]absentDiskSource) ([]int, map[int]string) {
	if !report.Complete || !report.VMScanComplete {
		return nil, nil
	}
	// A report with any conflict leaves every VM as it is. The audit already
	// counts itself incomplete when it finds a conflict, but we check here as
	// well, so the heal doesn't depend on that behavior. delete_disk then
	// refuses on the conflict.
	if len(report.Conflicts) > 0 {
		return nil, nil
	}
	for _, holder := range report.holders[record.ID] {
		if !absentDiskNoteHolder(report, record, holder, sources[holder.VMID]) {
			return nil, nil
		}
	}
	nodes := map[int]string{}
	named := map[string]bool{}
	for _, e := range report.Evidence {
		if e.AllocationID != record.ID || e.VMID <= 0 {
			continue
		}
		if e.Kind != allocationKindDisk {
			return nil, nil
		}
		nodes[e.VMID] = e.Node
		named[e.VolumeID] = true
	}
	for _, e := range report.Evidence {
		if e.AllocationID != record.ID || e.VMID > 0 {
			continue
		}
		if e.Kind != allocationKindDisk || !named[e.VolumeID] || !absentDiskRenamedOff(report, sources, e.Node, e.VolumeID) {
			return nil, nil
		}
	}
	vmids := make([]int, 0, len(nodes))
	for vmid := range nodes {
		vmids = append(vmids, vmid)
	}
	sort.Ints(vmids)
	return vmids, nodes
}

// absentDiskNoteHolder reports whether the audit counted holder as the
// disk's only because the VM's own leftover entry names the volume, under a
// name the disk had before an observed move took it off that VM and renamed
// it. The move has to be on the holder's node unless the volume's storage is
// shared, because the same name on another node's local storage is another
// volume. The drive carries no serial, so it belongs to whatever disk PVE gave
// the name to, and the heal decides from its own read (absentDiskHeldBy)
// whether that VM holds the disk. The VM can name the volume more than once,
// in its applied config and in a pending change, and every one of those claims
// has to be a drive that has no serial, isn't a CD-ROM, and is attributed to
// the record by the VM's own leftover entry.
func absentDiskNoteHolder(report StorageAllocationAudit, record aj.Record, holder StorageAllocationEvidence, sources []absentDiskSource) bool {
	shared := storageAuditVolumeShared(report.stores, holder.VolumeID)
	if !slices.ContainsFunc(sources, func(source absentDiskSource) bool {
		return source.old == holder.VolumeID && (shared || source.node == holder.Node)
	}) {
		return false
	}
	found := false
	for _, claim := range report.claims[holder.VolumeID] {
		if claim.node != holder.Node || claim.vmid != holder.VMID {
			continue
		}
		if claim.cdrom || claim.serial != "" || !slices.Contains(claim.allocations, record.ID) {
			return false
		}
		found = true
	}
	return found
}

// absentDiskRenamedOff reports whether an observed move took the disk off a VM
// under the name volume and renamed it, on node or on any node when volume's
// storage is shared, because then a volume listed under that name on node is
// the same one.
func absentDiskRenamedOff(report StorageAllocationAudit, sources map[int][]absentDiskSource, node, volume string) bool {
	shared := storageAuditVolumeShared(report.stores, volume)
	for _, list := range sources {
		if slices.ContainsFunc(list, func(source absentDiskSource) bool {
			return source.old == volume && (shared || source.node == node)
		}) {
			return true
		}
	}
	return false
}

// removeAbsentDiskNotes removes this disk's own notes from every VM that still
// carries them after its volume is gone, as long as no VM holds the disk. An
// earlier release can leave them on a VM that the disk left through a move or
// a reattach, or on the VM the record still names. The completion audit then
// counts each one as the disk's and refuses every delete_disk. The VMs come
// from report, the completion audit's first run, so a VM that no record names
// any more is found as well.
//
// We read every one of those VMs and look for its holds before we write to
// any of them. A VM holds the disk when it holds any of the disk's names on
// an unused entry, when it holds any of them on a bus slot unless an observed
// move took the disk off that VM under that name, or when it holds a drive
// with the disk's serial. A snapshot holds the disk by the same rules, so an
// old name on a snapshot's bus slot doesn't count, while a drive with the
// disk's serial or the name on an unused entry still does. When any VM holds
// the disk, no VM loses anything. Otherwise, on each VM the allocation entry goes only when it is
// keyed by the disk's stable ID, it names the record's allocation, namespace,
// and backing, it names a node that the record names, and it names a volid
// that the disk has had. The same write removes the attached-disk notes whose
// value is the disk's own CID and the overlay keyed by the stable ID. No note
// goes when its key names a volume that the VM holds on a slot or an unused
// entry. Each VM gets one write built from its own read.
//
// While any step of the record's active attempt is unobserved, nothing goes
// from any VM, and that step comes back so the caller can name it when it
// refuses. A step of a closed attempt doesn't stop the heal
// (absentDiskUnsettled says why).
// It reports whether it removed anything, so the caller knows to audit again.
// record is the disk's journal record as the caller holds it under the
// journal's lock.
func removeAbsentDiskNotes(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, report StorageAllocationAudit) (bool, *aj.Step, error) {
	if rd.allocation == nil || rd.stableID == "" {
		return false, nil, nil
	}
	sources := absentDiskRenamedSources(record)
	vmids, nodes := absentDiskLeftoverVMs(report, record, sources)
	if len(vmids) == 0 {
		return false, nil, nil
	}
	if step := absentDiskUnsettled(record); step != nil {
		return false, step, nil
	}
	names := absentDiskNames(record, rd)
	removals := make([]absentDiskRemoval, 0, len(vmids))
	for _, vmid := range vmids {
		removal, held, err := absentDiskPlanRemoval(ctx, deps, rd, record, nodes[vmid], vmid, names, sources[vmid])
		if err != nil {
			return false, nil, retriableUnlessPermanent(err, fmt.Sprintf("delete_disk: remove disk %s's notes from VM %d", rd.diskCID, vmid))
		}
		if held {
			deps.Log(ctx).Debug("delete_disk: a VM holds the disk, so no VM loses its notes",
				log.String("disk_cid", rd.diskCID), log.Int("held_vmid", vmid), log.String("node", nodes[vmid]))
			return false, nil, nil
		}
		if removal != nil {
			removals = append(removals, *removal)
		}
	}
	removed := false
	for i := range removals {
		removal := &removals[i]
		// This write isn't journaled. It removes only our own notes about a
		// disk whose volume is already gone, and it carries the digest of the
		// read it was built from, so PVE refuses it rather than overwrite a
		// newer description. Every call reads each VM again and decides
		// afresh, so a rerun repairs a write whose outcome we never learned.
		if err := pve.RemoveDiskAllocationEntry(ctx, deps.PVE, removal.node, removal.vmid, rd.stableID, removal.entry, removal.config, removal.attached, removal.overlays); err != nil {
			return removed, nil, retriableUnlessPermanent(err, fmt.Sprintf("delete_disk: remove disk %s's notes from VM %d", rd.diskCID, removal.vmid))
		}
		removed = true
	}
	return removed, nil, nil
}

// absentDiskRemoval is the one write the heal makes to a VM. It holds the read
// of the VM's config that the write is built from, the allocation entry that
// read carried, and the attached-disk and overlay keys that go with it.
type absentDiskRemoval struct {
	node     string
	vmid     int
	entry    pve.DiskAllocationProvenance
	config   map[string]any
	attached []string
	overlays []string
}

// absentDiskPlanRemoval reads one VM on node, in both views and in its
// snapshots, and decides what removeAbsentDiskNotes may do there. held reports
// that the VM holds the disk. When held is false, removal is the write that
// removes the disk's notes from the VM, or nil when the VM carries none that
// are provably ours.
func absentDiskPlanRemoval(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, node string, vmid int, names []string, sources []absentDiskSource) (*absentDiskRemoval, bool, error) {
	views, err := pve.ReadQemuViews(ctx, deps.PVE, node, vmid)
	if err != nil {
		// The audit saw this VM, so a read that fails, a view that comes back
		// malformed, or a VM that has gone since all leave the audit out of
		// date. The call comes back retriable, and the next one audits again.
		return nil, false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("read VM %d on %s", vmid, node))
	}
	if absentDiskHeldBy(views, rd.stableID, names, sources) {
		return nil, true, nil
	}
	inSnapshot, err := absentDiskSnapshotHeld(ctx, deps, node, vmid, rd.stableID, names, sources)
	if err != nil || inSnapshot {
		return nil, inSnapshot, err
	}
	config := views.Applied()
	description := pve.DescriptionFromConfig(config)
	// A description we can't parse is left for the completion audit, which
	// refuses it as it always has.
	entries, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return nil, false, nil
	}
	key := rd.stableID
	entry, found := entries[key]
	if !found || entry.AllocationID != record.ID || entry.AllocationNamespace != record.Namespace || entry.Backing != rd.allocation.provenance.Backing {
		return nil, false, nil
	}
	if !absentDiskRecordNode(record, rd, entry.Node) || !slices.Contains(names, entry.Volid) {
		return nil, false, nil
	}
	held := map[string]bool{}
	candidates := []string{key}
	for name, cid := range pve.GetAttachedDiskCIDs(description) {
		if cid == rd.diskCID {
			candidates = append(candidates, name)
		}
	}
	for _, name := range candidates {
		if views.NamesVolume(name) {
			held[name] = true
		}
	}
	if held[key] {
		return nil, false, nil
	}
	attached := unheldNames(held, candidates[1:]...)
	slices.Sort(attached)
	return &absentDiskRemoval{node: node, vmid: vmid, entry: entry, config: config, attached: attached, overlays: unheldNames(held, key)}, false, nil
}

// absentDiskUnsettled returns the first step of the record's active attempt
// that was never observed, or nil when every one was. An interrupted attach
// leaves its move that way. Such a step may still put the disk on a VM, and a
// move's step names the VM it leaves rather than the one it lands on, so we
// can't tell which VM the disk may land on. Every VM keeps its notes until the
// step settles. A closed attempt's steps don't count. The proof that closed
// the attempt showed that it left nothing behind and that every one of its
// outcomes was known, so those steps can't put the disk anywhere, and they
// can never settle either. A create_disk that PVE rejected before it ran and
// that a retry then completed leaves such a step, planned for good.
func absentDiskUnsettled(record aj.Record) *aj.Step {
	active := record.ActiveAttempt()
	index := slices.IndexFunc(record.Steps, func(step aj.Step) bool { return step.Attempt == active && step.State != aj.Observed })
	if index < 0 {
		return nil
	}
	step := record.Steps[index]
	return &step
}

// absentDiskSnapshotHeld reports whether any snapshot of the VM names one of
// the disk's names or carries a drive with the disk's serial. A rollback would
// put that drive back, so the VM still holds the disk. A name the disk had
// before an observed move took it off this VM doesn't count on a bus slot, for
// the reason absentDiskHeldBy gives. That name still counts on an unused entry,
// and a drive with the disk's serial still counts too. A snapshot we can't
// list or read could hold the disk too, so a failed read comes back as an
// error.
func absentDiskSnapshotHeld(ctx context.Context, deps Deps, node string, vmid int, stableID string, names []string, sources []absentDiskSource) (bool, error) {
	snapshots, err := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
	if err != nil {
		return false, err
	}
	for _, snapshot := range snapshots {
		cfg, err := pve.SnapshotConfig(ctx, deps.PVE, node, vmid, snapshot)
		if err != nil {
			return false, err
		}
		for key := range cfg {
			value, ok := pve.ConfigString(cfg, key)
			if !ok || key == "description" {
				continue
			}
			name := strings.Split(value, ",")[0]
			if slices.Contains(names, name) && (strings.HasPrefix(key, "unused") || !absentDiskRenamedOffVM(sources, name)) {
				return true, nil
			}
			if serial, found := pve.StableIDFromDriveOptStr(value); found && serial == stableID {
				return true, nil
			}
		}
	}
	return false, nil
}

// absentDiskHeldBy reports whether a VM, read as views, may still hold the
// disk. It does when a drive in either view carries the disk's serial, or
// when it names any of the disk's names on an unused entry. It also does
// when it names one of them on a bus slot, unless an observed move took the
// disk off this VM under that name, because then the slot holds whatever disk
// PVE gave the name to since.
func absentDiskHeldBy(views pve.QemuViews, stableID string, names []string, sources []absentDiskSource) bool {
	for _, cfg := range []map[string]any{views.Holding(), views.PendingReplacements()} {
		for _, drive := range qemu.ParseDisks(cfg) {
			if serial, found := pve.StableIDFromDriveOptStr(drive); found && serial == stableID {
				return true
			}
		}
	}
	for _, name := range names {
		for _, slot := range views.SlotsNaming(name) {
			if strings.HasPrefix(slot, "unused") {
				return true
			}
			if !absentDiskRenamedOffVM(sources, name) {
				return true
			}
		}
	}
	return false
}

// absentDiskRenamedOffVM reports whether one of a VM's sources is a move that
// took the disk off that VM under name.
func absentDiskRenamedOffVM(sources []absentDiskSource, name string) bool {
	return slices.ContainsFunc(sources, func(source absentDiskSource) bool { return source.old == name })
}
