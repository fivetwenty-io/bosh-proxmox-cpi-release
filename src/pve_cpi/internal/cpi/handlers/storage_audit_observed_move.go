package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// storageMoveKindParker names a parked disk moved with its parker VM.
const storageMoveKindParker = "parker"

// StorageAllocationMove is a VM, disk, or parker that the audit found on a
// node other than the one the journal records, and accepted as a move on
// shared storage. Kind is vm, disk, or parker. RecordedNodes are the nodes
// the journal or the disk provenance names, and Volumes are every volume the
// audit checked before it accepted the move.
type StorageAllocationMove struct {
	AllocationID  string   `json:"allocation_id"`
	Kind          string   `json:"kind"`
	VMID          int      `json:"vmid"`
	RecordedNodes []string `json:"recorded_nodes"`
	ObservedNode  string   `json:"observed_node"`
	Volumes       []string `json:"volumes"`
}

// String renders the move as one line for logs and summaries.
func (m StorageAllocationMove) String() string {
	return fmt.Sprintf("%s allocation %s (VM %d) moved from %s to %s", m.Kind, m.AllocationID, m.VMID, strings.Join(m.RecordedNodes, ","), m.ObservedNode)
}

// observedMove reports whether the audit accepted a move of allocationID to
// node. Lifecycle paths consult it instead of re-deciding the move.
func (r StorageAllocationAudit) observedMove(allocationID, node string) bool {
	return slices.ContainsFunc(r.ObservedMoves, func(move StorageAllocationMove) bool {
		return move.AllocationID == allocationID && move.ObservedNode == node
	})
}

// storageAuditMoveIndex is the journal and storage state the move rules read.
type storageAuditMoveIndex struct {
	byID         map[string]aj.Record
	knownVolumes map[string][]aj.Record
	stores       map[string]pve.StorageInfo
	namespace    string
}

// storageAuditFrozen is what the owning record froze about one volume's
// storage. A volume without it cannot be accepted as moved.
type storageAuditFrozen struct {
	backing string
	shared  bool
	found   bool
}

// resolveStorageAuditMoves decides each pending node mismatch after the
// storage listings and correlation have run. An accepted move is reported in
// ObservedMoves; a refused one raises its conflict with the reason appended.
// It reads PVE only for snapshot configurations and never writes anything.
func resolveStorageAuditMoves(ctx context.Context, deps Deps, result *StorageAllocationAudit, index storageAuditMoveIndex) {
	for pendingIndex := range result.pending {
		pending := &result.pending[pendingIndex]
		var move StorageAllocationMove
		var refusal string
		if pending.kind == "vm" {
			move, refusal = storageAuditObservedSharedMove(ctx, deps, result, index, pending.evidence)
		} else {
			move, refusal = storageAuditObservedSharedDiskMove(result, index, *pending)
		}
		if refusal != "" {
			result.addConflict(pending.conflict+"; not accepted as a move because "+refusal, pending.brief+"; not a move: "+refusal)
			continue
		}
		result.ObservedMoves = append(result.ObservedMoves, move)
	}
	result.pending = nil
	sort.Slice(result.ObservedMoves, func(i, j int) bool { return result.ObservedMoves[i].String() < result.ObservedMoves[j].String() })
	result.ObservedMoves = slices.CompactFunc(result.ObservedMoves, func(a, b StorageAllocationMove) bool { return a.String() == b.String() })
}

// storageAuditObservedSharedMove decides whether a VM sighted on a node its
// record does not name was moved there on shared storage. It returns the
// accepted move, or the reason the mismatch stays a conflict.
//
// It accepts only a returned or adopted record whose active attempt targets
// this VMID, whose marker was sighted exactly once in a complete VM scan, and
// whose agent digest matches. Every config volume must belong to this record
// or to a disk record whose provenance this VM carries. Every volume the VM
// holds, including its vmstate and every snapshot's volumes, and every volume
// the active attempt recorded, must sit on storage that is shared now, was
// shared and had the same backing when the owning plan was frozen, is
// available on the new node, and listed that volume there in this audit.
func storageAuditObservedSharedMove(ctx context.Context, deps Deps, result *StorageAllocationAudit, index storageAuditMoveIndex, evidence StorageAllocationEvidence) (StorageAllocationMove, string) {
	if !result.VMScanComplete {
		return StorageAllocationMove{}, "the VM scan is incomplete, so another sighting could be hidden"
	}
	record, found := index.byID[evidence.AllocationID]
	if !found || record.Kind != "vm" || record.Namespace != index.namespace {
		return StorageAllocationMove{}, "no VM record of this namespace owns the allocation"
	}
	if record.State != aj.ReadyToReturn && record.State != aj.Adopted {
		return StorageAllocationMove{}, fmt.Sprintf("the record is in state %s, not ready_to_return or adopted", record.State)
	}
	recorded := storageAuditActiveVMNodes(record, evidence.VMID)
	if len(recorded) == 0 {
		return StorageAllocationMove{}, fmt.Sprintf("VM %d is not a target of the record's active attempt", evidence.VMID)
	}
	if sightings := storageAuditMarkerSightings(result, record.ID); sightings != 1 {
		return StorageAllocationMove{}, fmt.Sprintf("the allocation marker was sighted %d times", sightings)
	}
	digest := sha256.Sum256([]byte(record.AgentID))
	if evidence.AgentSHA256 != hex.EncodeToString(digest[:]) {
		return StorageAllocationMove{}, "the VM carries a different agent digest"
	}
	vm, refusal := storageAuditHolder(result, evidence.Node, evidence.VMID)
	if refusal != "" {
		return StorageAllocationMove{}, refusal
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return StorageAllocationMove{}, "the record's plan could not be decoded"
	}
	// Every volume maps to the backing its owner froze. Config volumes need a
	// journal owner; other held volumes fall back to the VM's own plan.
	held := map[string]storageAuditFrozen{}
	for _, volume := range storageAuditSortedValues(vm.volumes) {
		frozen, owned := storageAuditConfigVolumeOwner(index, record, plan, vm, volume)
		if !owned {
			return StorageAllocationMove{}, fmt.Sprintf("volume %s is owned by no journal record of this VM", volume)
		}
		held[volume] = frozen
	}
	extra, refusal := storageAuditSnapshotVolumes(ctx, deps, vm)
	if refusal != "" {
		return StorageAllocationMove{}, refusal
	}
	if vm.hasVMState {
		extra = append(extra, vm.vmstate)
	}
	for stepIndex := range record.Steps {
		step := record.Steps[stepIndex]
		if step.Attempt == record.ActiveAttempt() {
			extra = append(extra, step.VolIDs...)
		}
	}
	for _, volume := range extra {
		if _, seen := held[volume]; !seen {
			held[volume] = storageAuditHeldVolumeOwner(index, record, plan, volume)
		}
	}
	volumes := make([]string, 0, len(held))
	for volume := range held {
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)
	for _, volume := range volumes {
		if refusal := storageAuditSharedVolumeRefusal(result, index, evidence.Node, volume, held[volume]); refusal != "" {
			return StorageAllocationMove{}, refusal
		}
	}
	return StorageAllocationMove{AllocationID: record.ID, Kind: "vm", VMID: evidence.VMID, RecordedNodes: recorded, ObservedNode: evidence.Node, Volumes: volumes}, ""
}

// storageAuditObservedSharedDiskMove decides whether a disk whose provenance
// names another node moved there with its holder on shared storage. It covers
// both current disk provenance and parked disks. The holder must be sighted
// exactly once in a complete VM scan and still hold the volume, and the volume
// must meet the same storage conditions as a moved VM's volumes, measured
// against the backing the disk record froze.
func storageAuditObservedSharedDiskMove(result *StorageAllocationAudit, index storageAuditMoveIndex, pending storageAuditPendingMove) (StorageAllocationMove, string) {
	evidence := pending.evidence
	if !result.VMScanComplete {
		return StorageAllocationMove{}, "the VM scan is incomplete, so another sighting could be hidden"
	}
	vm, refusal := storageAuditHolder(result, evidence.Node, evidence.VMID)
	if refusal != "" {
		return StorageAllocationMove{}, refusal
	}
	if !slices.Contains(storageAuditSortedValues(vm.volumes), evidence.VolumeID) {
		return StorageAllocationMove{}, fmt.Sprintf("VM %d does not hold volume %s in its configuration", evidence.VMID, evidence.VolumeID)
	}
	record, found := index.byID[evidence.AllocationID]
	if !found || record.Kind != allocationKindDisk || record.Namespace != index.namespace {
		return StorageAllocationMove{}, "no disk record of this namespace owns the allocation"
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return StorageAllocationMove{}, fmt.Sprintf("the disk record is in state %s", record.State)
	}
	frozen := storageAuditDiskFrozen(record, evidence.VolumeID)
	if refusal := storageAuditSharedVolumeRefusal(result, index, evidence.Node, evidence.VolumeID, frozen); refusal != "" {
		return StorageAllocationMove{}, refusal
	}
	return StorageAllocationMove{AllocationID: record.ID, Kind: pending.kind, VMID: evidence.VMID, RecordedNodes: []string{pending.recorded}, ObservedNode: evidence.Node, Volumes: []string{evidence.VolumeID}}, ""
}

// storageAuditSharedVolumeRefusal returns why volume on node fails the
// shared-storage conditions, or "" when it meets all of them. A successful
// listing is not proof on its own, because an unmounted dir storage lists
// the empty directory beneath it, so the volume itself must be listed.
func storageAuditSharedVolumeRefusal(result *StorageAllocationAudit, index storageAuditMoveIndex, node, volume string, frozen storageAuditFrozen) string {
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return fmt.Sprintf("volume %s names no storage", volume)
	}
	current, defined := index.stores[storage]
	if !defined {
		return fmt.Sprintf("storage %q of volume %s is not defined", storage, volume)
	}
	if !current.IsShared() {
		return fmt.Sprintf("volume %s is node-local", volume)
	}
	if !frozen.found {
		return fmt.Sprintf("the owning record froze no definition of storage %q for volume %s", storage, volume)
	}
	if !frozen.shared {
		return fmt.Sprintf("storage %q of volume %s was node-local when its plan was frozen", storage, volume)
	}
	if frozen.backing == "" || current.BackingKey() != frozen.backing {
		return fmt.Sprintf("the backing of storage %q changed since the plan for volume %s was frozen", storage, volume)
	}
	if len(current.Nodes) > 0 && !slices.Contains(current.Nodes, node) {
		return fmt.Sprintf("storage %q of volume %s is not available on %s", storage, volume, node)
	}
	if !result.listed[storageAuditTarget{node: node, storage: storage}][volume] {
		return fmt.Sprintf("volume %s was not listed on storage %q on %s", volume, storage, node)
	}
	return ""
}

// storageAuditActiveVMNodes returns the sorted nodes the record's active
// attempt names for vmid. A preservation step never counts.
func storageAuditActiveVMNodes(record aj.Record, vmid int) []string {
	var nodes []string
	for stepIndex := range record.Steps {
		step := record.Steps[stepIndex]
		if step.Attempt != record.ActiveAttempt() || step.Target.VMID != vmid || vmid <= 0 || step.Target.Node == "" {
			continue
		}
		if step.Target.External || strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") {
			continue
		}
		nodes = append(nodes, step.Target.Node)
	}
	sort.Strings(nodes)
	return slices.Compact(nodes)
}

// storageAuditMarkerSightings counts the VMs carrying the allocation marker,
// the same way resume counts them. Listings also emit vm evidence for
// ephemeral volumes, and those carry a VolumeID.
func storageAuditMarkerSightings(result *StorageAllocationAudit, allocationID string) int {
	sightings := 0
	for _, evidence := range result.Evidence {
		if evidence.Kind == "vm" && evidence.VolumeID == "" && evidence.AllocationID == allocationID {
			sightings++
		}
	}
	return sightings
}

// storageAuditHolder returns the inventory of the VM on node, which the scan
// must have listed exactly once and whose volumes it must have recognized.
func storageAuditHolder(result *StorageAllocationAudit, node string, vmid int) (storageAuditVM, string) {
	var holder storageAuditVM
	listings := 0
	for _, vm := range result.vms {
		if vm.vmid == vmid {
			listings++
			if vm.node == node {
				holder = vm
			}
		}
	}
	switch {
	case listings != 1:
		return storageAuditVM{}, fmt.Sprintf("VM %d was listed %d times", vmid, listings)
	case holder.node != node:
		return storageAuditVM{}, fmt.Sprintf("VM %d was not inventoried on %s", vmid, node)
	case holder.volumes == nil:
		return storageAuditVM{}, fmt.Sprintf("VM %d holds a volume reference the CPI does not recognize", vmid)
	}
	return holder, ""
}

// storageAuditConfigVolumeOwner finds the journal record that owns a config
// volume: the VM record itself, or a disk record whose provenance the VM
// carries for that exact volume. The second result is false when neither
// owns it.
func storageAuditConfigVolumeOwner(index storageAuditMoveIndex, record aj.Record, plan *StorageAllocationPlan, vm storageAuditVM, volume string) (storageAuditFrozen, bool) {
	owners := index.knownVolumes[volume]
	if slices.ContainsFunc(owners, func(owner aj.Record) bool { return owner.ID == record.ID }) {
		return storageAuditPlanFrozen(plan, volume), true
	}
	for ownerIndex := range owners {
		owner := owners[ownerIndex]
		if owner.Kind == allocationKindDisk && owner.Namespace == index.namespace && vm.disks[owner.ID] == volume {
			return storageAuditDiskFrozen(owner, volume), true
		}
	}
	return storageAuditFrozen{}, false
}

// storageAuditHeldVolumeOwner finds the frozen backing of a volume the VM
// holds outside its config, such as a vmstate or a snapshot's volume. A disk
// record's own backing wins when one owns the volume; otherwise the VM's plan
// must have frozen the storage.
func storageAuditHeldVolumeOwner(index storageAuditMoveIndex, record aj.Record, plan *StorageAllocationPlan, volume string) storageAuditFrozen {
	owners := index.knownVolumes[volume]
	if !slices.ContainsFunc(owners, func(owner aj.Record) bool { return owner.ID == record.ID }) {
		for ownerIndex := range owners {
			if owners[ownerIndex].Kind == allocationKindDisk {
				return storageAuditDiskFrozen(owners[ownerIndex], volume)
			}
		}
	}
	return storageAuditPlanFrozen(plan, volume)
}

// storageAuditPlanFrozen reads the definition a VM plan froze for a volume's
// storage.
func storageAuditPlanFrozen(plan *StorageAllocationPlan, volume string) storageAuditFrozen {
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil || plan == nil {
		return storageAuditFrozen{}
	}
	definition, found := plan.Definitions[storage]
	if !found {
		return storageAuditFrozen{}
	}
	return storageAuditFrozen{backing: definition.BackingKey(), shared: definition.IsShared(), found: true}
}

// storageAuditDiskFrozen reads what a disk record froze for one volume. The
// backing comes from the steps that name the volume, which must all agree,
// and the shared flag comes from the definition in the disk record's plan.
func storageAuditDiskFrozen(record aj.Record, volume string) storageAuditFrozen {
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return storageAuditFrozen{}
	}
	backing := ""
	for stepIndex := range record.Steps {
		step := record.Steps[stepIndex]
		if step.Target.External || step.Target.Backing == "" || step.Target.Storage != storage {
			continue
		}
		if step.Target.IntendedVolume != volume && !slices.Contains(step.VolIDs, volume) {
			continue
		}
		if backing != "" && backing != step.Target.Backing {
			return storageAuditFrozen{}
		}
		backing = step.Target.Backing
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil || backing == "" {
		return storageAuditFrozen{}
	}
	definition, found := plan.Definitions[storage]
	if !found {
		return storageAuditFrozen{}
	}
	return storageAuditFrozen{backing: backing, shared: definition.IsShared(), found: true}
}

// storageAuditSnapshotVolumes reads every snapshot of a VM and returns the
// volumes they hold, including each snapshot's vmstate. Any failed read
// refuses the move, because an unread snapshot could hold a local volume.
func storageAuditSnapshotVolumes(ctx context.Context, deps Deps, vm storageAuditVM) ([]string, string) {
	names, err := pve.HasSnapshots(ctx, deps.PVE, vm.node, vm.vmid)
	if err != nil {
		return nil, fmt.Sprintf("the snapshots of VM %d could not be listed: %s", vm.vmid, pve.DescribeAuditError(err))
	}
	var volumes []string
	for _, name := range names {
		cfg, err := pve.SnapshotConfig(ctx, deps.PVE, vm.node, vm.vmid, name)
		if err != nil {
			return nil, fmt.Sprintf("snapshot %q of VM %d could not be read: %s", storageAuditField(name), vm.vmid, pve.DescribeAuditError(err))
		}
		held, err := managedVMConfigVolumes(cfg)
		if err != nil {
			return nil, fmt.Sprintf("snapshot %q of VM %d holds a volume reference the CPI does not recognize", storageAuditField(name), vm.vmid)
		}
		volumes = append(volumes, storageAuditSortedValues(held)...)
		if state, found := storageAuditVMState(cfg); found {
			volumes = append(volumes, state)
		}
	}
	return volumes, ""
}

// storageAuditSortedValues returns a slot map's volids in a stable order.
func storageAuditSortedValues(slots map[string]string) []string {
	values := make([]string, 0, len(slots))
	for _, value := range slots {
		values = append(values, value)
	}
	sort.Strings(values)
	return slices.Compact(values)
}
