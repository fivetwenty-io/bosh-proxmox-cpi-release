package handlers

// A managed disk that a move renamed gives up its old name, and PVE hands that
// name to the next disk it puts there. delete_disk's completion audit used to
// count a volume with no serial under the old name as the renamed disk's, so
// the record stayed in reconciliation_required for good. The first rows run
// delete_disk end to end on the flow fake after an observed rename. The
// guards show that the audit keeps the old name for the record when no
// observed move renamed the disk off it, or when the volume carries the
// record's own serial, and that conflicts from any other source still fire.
// TestAllocationAuditUnheldReusedNameStaysEvidenced guards a volume that no
// VM holds on a slot.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// renamedNameState is what 777 and storage hold before delete_disk runs on a
// renamed disk whose old name another disk now has.
type renamedNameState struct {
	notes   map[string]string
	rest    string
	volumes []string
}

func captureRenamedNameState(t *testing.T, f digestManaged) renamedNameState {
	t.Helper()
	cfg := f.client.state.configs[777]
	return renamedNameState{notes: descriptionNotes(t, cfg), rest: configWithoutNotes(cfg), volumes: volumeNames(f)}
}

// requireRenamedDiskDeletedAlone checks that delete_disk ended the renamed
// disk's record Deleted, destroyed its landed volume and nothing else, and
// left 777 as it was apart from the renamed disk's allocation entry. That
// covers the other disk's slot, volume, and any notes, byte for byte.
func requireRenamedDiskDeletedAlone(t *testing.T, f digestManaged, key, landed string, before renamedNameState) {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Deleted {
		t.Fatalf("record state = %s (%s), want deleted", record.State, record.Reason)
	}
	var destroyed []string
	for _, volid := range before.volumes {
		if f.client.state.volumes[volid] == nil {
			destroyed = append(destroyed, volid)
		}
	}
	if len(destroyed) != 1 || destroyed[0] != landed || len(volumeNames(f)) != len(before.volumes)-1 {
		t.Fatalf("delete_disk destroyed %v and left %v, want only the landed volume %s destroyed", destroyed, volumeNames(f), landed)
	}
	cfg := f.client.state.configs[777]
	notes := descriptionNotes(t, cfg)
	gone := "bosh_disk_allocations/" + key
	if _, found := notes[gone]; found {
		t.Fatal("777 still carries the renamed disk's allocation entry")
	}
	for note, was := range before.notes {
		if note != gone && notes[note] != was {
			t.Fatalf("777's %s = %q, want %s left as it was", note, notes[note], was)
		}
	}
	if len(notes) != len(before.notes)-1 {
		t.Fatalf("777's notes went from %v to %v, want only %s gone", before.notes, notes, gone)
	}
	if rest := configWithoutNotes(cfg); rest != before.rest {
		t.Fatalf("777's config changed beyond its notes:\nbefore %s\nafter  %s", before.rest, rest)
	}
}

func getDisks777(t *testing.T, f digestManaged) []string {
	t.Helper()
	result, err := HandleGetDisks(f.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("get_disks on 777: %v", err)
	}
	cids, ok := result.([]string)
	if !ok {
		t.Fatalf("get_disks on 777 returned %T, want []string", result)
	}
	return cids
}

// TestDeleteDiskRenamedOldNameHandAttached is a renamed disk whose old name an
// operator has since given to a volume and attached to 777 by hand. The volume
// has no serial and no notes, so the only CID it has is the one get_disks
// mints. The move that renamed the disk is observed, so the old name is no
// longer the disk's, and delete_disk ends Deleted with the hand-attached
// volume, its slot, and get_disks' answer as they were.
func TestDeleteDiskRenamedOldNameHandAttached(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["scsi3"] = f.volume + ",size=1G"
	if notes := notesFiledUnder(t, f.client.state.configs[777], f.volume); len(notes) != 0 {
		t.Fatalf("777 files %v under the hand-attached volume, want no notes", notes)
	}
	minted, err := pve.EncodeDiskCID(f.volume, nil)
	if err != nil {
		t.Fatal(err)
	}
	cidsBefore := getDisks777(t, f)
	if !slices.Contains(cidsBefore, minted) {
		t.Fatalf("get_disks on 777 = %v, want the minted CID %s for %s", cidsBefore, minted, f.volume)
	}
	before := captureRenamedNameState(t, f)
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk with a hand-attached volume on the old name: %v", err)
	}
	requireRenamedDiskDeletedAlone(t, f, key, landed, before)
	if cids := getDisks777(t, f); fmt.Sprint(cids) != fmt.Sprint(cidsBefore) {
		t.Fatalf("get_disks on 777 = %v, want %v", cids, cidsBefore)
	}
}

// TestDeleteDiskRenamedOldNameRetryAfterRefusal is the hand-attached volume
// on the old name after an earlier delete_disk deleted the renamed disk's
// volume and then refused at its completion audit, which is where a release
// without this fix left the record. A read that fails once the volume is
// deleted stands in for that refusal. A retry of delete_disk ends Deleted,
// with the hand-attached volume, its slot, and get_disks' answer as they
// were.
func TestDeleteDiskRenamedOldNameRetryAfterRefusal(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["scsi3"] = f.volume + ",size=1G"
	cidsBefore := getDisks777(t, f)
	before := captureRenamedNameState(t, f)
	f.client.visibilityErrAfterDelete = errors.New("visibility lost")
	_, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if message := directorMessage(err); !strings.HasPrefix(message, "delete_disk refused: ") {
		t.Fatalf("delete_disk without audit visibility after the deletion = %q, want an audit refusal", message)
	}
	stuck, inspectErr := f.journal.Inspect(f.id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if stuck.State != aj.ReconciliationRequired || f.client.state.volumes[landed] != nil {
		t.Fatalf("refused delete_disk left state=%s and landed volume present=%t, want reconciliation_required with %s deleted", stuck.State, f.client.state.volumes[landed] != nil, landed)
	}
	if !strings.Contains(stuck.Reason, "completion audit failed") {
		t.Fatalf("refused delete_disk left reason %q, want its completion audit", stuck.Reason)
	}
	f.client.visibilityErrAfterDelete, f.client.visibilityErr = nil, nil
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried delete_disk with a hand-attached volume on the old name: %v", err)
	}
	requireRenamedDiskDeletedAlone(t, f, key, landed, before)
	if cids := getDisks777(t, f); fmt.Sprint(cids) != fmt.Sprint(cidsBefore) {
		t.Fatalf("get_disks on 777 = %v, want %v", cids, cidsBefore)
	}
}

// TestAllocationAuditOldNameKeptWithoutObservedRename guards the attribution
// the audit keeps. VM 2353 holds the old record's historical name with no
// serial and no provenance, as a hand-attached volume would be. While the
// record holds no observed move that renamed the disk off that name, the
// audit still counts the volume as the old record's, both as VM 2353's
// holding and in the storage listing, exactly as it always has.
func TestAllocationAuditOldNameKeptWithoutObservedRename(t *testing.T) {
	for _, tc := range []struct {
		name string
		// detachMove rewrites the old record's move off VM 2353.
		detachMove func(step *aj.Step)
		// extra are steps the old record took after that move.
		extra []aj.Step
	}{
		{
			// The move's outcome is unknown, so the disk may still be
			// there.
			name: "move not observed",
			detachMove: func(step *aj.Step) {
				step.State = aj.Planned
				step.VolIDs = nil
			},
		},
		{
			// PVE refused the move, which the journal records as observed
			// with only the volume it set out from.
			name: "refused move landed where it started",
			detachMove: func(step *aj.Step) {
				step.VolIDs = []string{reuseVolume}
			},
		},
		{
			// The move landed the disk under the name it set out from.
			name: "move kept the name",
			detachMove: func(step *aj.Step) {
				step.VolIDs = []string{reuseVolume, reuseVolume}
			},
		},
		{
			// A later move brought the disk back under the old name.
			name:       "later move landed under the old name again",
			detachMove: func(*aj.Step) {},
			extra: []aj.Step{{
				ID: "return-move", Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Observed,
				Target: aj.Target{Node: "pve1", Storage: "a", VMID: reuseParker, IntendedVolume: reuseParked},
				VolIDs: []string{reuseParked, reuseVolume},
			}},
		},
		{
			// A later move's outcome is unknown, so it may have brought
			// the disk back under the old name.
			name:       "later move not observed",
			detachMove: func(*aj.Step) {},
			extra: []aj.Step{{
				ID: "return-move", Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Planned,
				Target: aj.Target{Node: "pve1", Storage: "a", VMID: reuseParker, IntendedVolume: reuseParked},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReuseFixture(t)
			f.noSerial, f.noProvenance = true, true
			records := f.build()
			old := &records[0]
			moved := slices.IndexFunc(old.Steps, func(step aj.Step) bool { return step.ID == "detach-move" })
			if moved < 0 {
				t.Fatal("the old record holds no move off VM 2353")
			}
			tc.detachMove(&old.Steps[moved])
			backing := moveDefinition(t, moveDefinitions["a"]).BackingKey()
			for _, step := range tc.extra {
				step.Target.Backing = backing
				old.Steps = append(old.Steps, step)
			}
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			for _, vmid := range []int{reuseVMID, 0} {
				if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
					return e.AllocationID == old.ID && e.VolumeID == reuseVolume && e.VMID == vmid
				}) {
					t.Fatalf("old record not evidenced at %s with VMID %d: %+v\nconflicts:\n%s", reuseVolume, vmid, report.Evidence, strings.Join(report.Conflicts, "\n"))
				}
			}
		})
	}
}

// TestAllocationAuditOldNameWithOwnSerialStaysTheRecords guards the serial.
// VM 2353 holds the old record's historical name on a slot, as a
// hand-attached volume would be, but the drive carries the old record's own
// disk token as its serial. The serial is the disk's identity, so the volume
// is the old record's even though an observed move renamed the disk off that
// name. The audit counts it as the record's, both as VM 2353's holding and in
// the storage listing, and refuses on the record's two holders.
func TestAllocationAuditOldNameWithOwnSerialStaysTheRecords(t *testing.T) {
	f := newReuseFixture(t)
	f.noProvenance = true
	records := f.build()
	f.c.configs[reuseVMID]["scsi1"] = reuseVolume + ",serial=" + f.old.DiskToken + ",size=1G"
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, vmid := range []int{reuseVMID, 0} {
		if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
			return e.AllocationID == f.old.ID && e.VolumeID == reuseVolume && e.VMID == vmid
		}) {
			t.Fatalf("old record not evidenced at %s with VMID %d: %+v", reuseVolume, vmid, report.Evidence)
		}
	}
	if !holderConflict(report, f.old.ID) {
		t.Fatalf("the old record's two holders not reported:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	if report.Complete {
		t.Fatal("audit with two holders of one disk reported complete")
	}
}

// TestAllocationAuditGivenUpNameKeepsOtherConflicts guards the conflict checks.
// VM 2353 holds the old record's historical name on a slot with no serial and
// no provenance, so the old record has given that name up. A conflict that
// comes from anything else still fires, which here is VM 2400 holding a
// second volume under the old record's serial, or VM 2400 holding the old
// record's current volume on the parker as well.
func TestAllocationAuditGivenUpNameKeepsOtherConflicts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(old aj.Record) string
		// shared expects the conflict for the parker's volume being
		// referenced by two VMs as well.
		shared bool
	}{
		{
			name:  "second volume with the record's serial",
			drive: func(old aj.Record) string { return reuseOtherVM2Volume + ",serial=" + old.DiskToken },
		},
		{
			name:   "second VM on the record's current volume",
			drive:  func(aj.Record) string { return reuseParked },
			shared: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReuseFixture(t)
			f.noSerial, f.noProvenance = true, true
			records := f.build()
			f.c.configs[reuseOtherVM] = map[string]any{"virtio0": reuseOtherVol, "scsi2": tc.drive(f.old)}
			f.c.nodesRead.volumesByNode["pve1"] = append(f.c.nodesRead.volumesByNode["pve1"], reuseOtherVol, reuseOtherVM2Volume)
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if !holderConflict(report, f.old.ID) {
				t.Fatalf("the old record's second holder not reported:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			if tc.shared && !sharedReferenceConflict(report, reuseParked) {
				t.Fatalf("two VMs referencing %s not reported:\n%s", reuseParked, strings.Join(report.Conflicts, "\n"))
			}
			if report.Complete {
				t.Fatal("audit with a duplicate holder reported complete")
			}
		})
	}
}

// TestAllocationAuditGivenUpNameSecondHolderWithOwnSerial guards the rule that
// an old record gives up a name, against a claim from another VM. VM 2353
// holds the old record's historical name on a slot with no serial and no
// provenance, so on its own the old record has given that name up. VM 2400
// holds the same volume as well, and its drive carries the old record's own
// stable ID as its serial. That claim names the volume as the old record's,
// so the audit keeps the name for the record, counts it in the storage
// listing, and reports the record's two holders and the volume's two VMs.
func TestAllocationAuditGivenUpNameSecondHolderWithOwnSerial(t *testing.T) {
	f := newReuseFixture(t)
	f.noSerial, f.noProvenance = true, true
	records := f.build()
	f.c.configs[reuseOtherVM] = map[string]any{"virtio0": reuseOtherVol, "scsi2": reuseVolume + ",serial=" + f.old.DiskToken}
	f.c.nodesRead.volumesByNode["pve1"] = append(f.c.nodesRead.volumesByNode["pve1"], reuseOtherVol)
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, vmid := range []int{reuseOtherVM, 0} {
		if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
			return e.AllocationID == f.old.ID && e.VolumeID == reuseVolume && e.VMID == vmid
		}) {
			t.Fatalf("old record not evidenced at %s with VMID %d: %+v\nconflicts:\n%s", reuseVolume, vmid, report.Evidence, strings.Join(report.Conflicts, "\n"))
		}
	}
	if !holderConflict(report, f.old.ID) {
		t.Fatalf("the old record's second holder not reported:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	if !sharedReferenceConflict(report, reuseVolume) {
		t.Fatalf("two VMs referencing %s not reported:\n%s", reuseVolume, strings.Join(report.Conflicts, "\n"))
	}
	if report.Complete {
		t.Fatal("audit with a second holder of the given-up name reported complete")
	}
}

// reuseLocalDefinition is storage a as a directory on each node, so the same
// volid on two nodes names two volumes.
const reuseLocalDefinition = `{"storage":"a","type":"dir","path":"/mnt/a","content":"images,iso"}`

// TestAllocationAuditOldNameAcrossNodes runs the given-up rule across nodes.
// The old record's move off VM 2353 is recorded on pve1, and VM 2353 holds the
// old name with no serial and no provenance on the node each row names. On
// shared storage every node sees the one volume, so a holder on pve2 after a
// migration still shows the name went to another disk. On node-local storage
// only a holder on pve1 sees the volume the move renamed, and a holder on pve2
// holds a different volume that the move never touched, so the name stays the
// old record's there, and the audit reports the record's two holders.
func TestAllocationAuditOldNameAcrossNodes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		definition string
		holderNode string
		givenUp    bool
	}{
		{name: "shared storage with the VM migrated to another node", holderNode: "pve2", givenUp: true},
		{name: "node-local storage with the VM on the move's node", definition: reuseLocalDefinition, holderNode: "pve1", givenUp: true},
		{name: "node-local storage with the VM on another node", definition: reuseLocalDefinition, holderNode: "pve2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReuseFixture(t)
			f.noSerial, f.noProvenance = true, true
			f.definition = tc.definition
			records := f.build()
			f.c.clusterRead = &allocationAuditCluster{Service: f.c.idFakeClient.Cluster(), members: moveNodes, guestNodes: map[int]string{reuseVMID: tc.holderNode}}
			f.c.nodesRead.nodeNames = moveNodes
			f.c.nodesRead.guestNodes = map[int]string{reuseVMID: tc.holderNode, reuseParker: "pve1"}
			if tc.definition == "" {
				for _, node := range moveNodes {
					f.c.nodesRead.volumesByNode[node] = []string{reuseVolume, reuseParked}
				}
			} else {
				f.c.nodesRead.volumesByNode = map[string][]string{"pve1": {reuseParked}}
				f.c.nodesRead.volumesByNode[tc.holderNode] = append(f.c.nodesRead.volumesByNode[tc.holderNode], reuseVolume)
			}
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
				return e.AllocationID == f.old.ID && e.VolumeID == reuseParked && e.VMID == reuseParker
			}) {
				t.Fatalf("old record lost its parker holder: %+v", report.Evidence)
			}
			if tc.givenUp {
				if slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool { return e.AllocationID == f.old.ID && e.VolumeID == reuseVolume }) {
					t.Fatalf("old record still evidenced at the name it gave up: %+v", report.Evidence)
				}
				if holderConflict(report, f.old.ID) {
					t.Fatalf("VM 2353 on %s counted as the old record's second holder:\n%s", tc.holderNode, strings.Join(report.Conflicts, "\n"))
				}
				return
			}
			if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
				return e.AllocationID == f.old.ID && e.VolumeID == reuseVolume && e.VMID == reuseVMID && e.Node == tc.holderNode
			}) {
				t.Fatalf("old record not evidenced at VM 2353's %s on %s: %+v", reuseVolume, tc.holderNode, report.Evidence)
			}
			if !holderConflict(report, f.old.ID) {
				t.Fatalf("the old record's holders on pve1 and %s not reported:\n%s", tc.holderNode, strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

// TestAllocationAuditVMRecordKeepsRetainedName guards the record kinds the
// given-up rule reaches. VM 123's record moved its ephemeral disk off
// vm-123-disk-1 to a retention VM, where PVE renamed it, and a VM 123 holds a
// volume under the old name on a slot with no serial and no provenance. Only a
// disk record gives a name up, so the listing still evidences the VM record at
// that name, as it did before the rule existed.
func TestAllocationAuditVMRecordKeepsRetainedName(t *testing.T) {
	deps, _, c := auditFixture(t)
	a := moveDefinition(t, moveDefinitions["a"])
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"])}
	root, eph := "a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"
	vm := moveVMRecord(t, "agent", "pve1", 123, map[string]pve.StorageInfo{"a": a}, root, eph)
	vm.Steps = append(vm.Steps, aj.Step{
		ID: "retain-move", Kind: "lifecycle_delete_vm_retain_ephemeral_Nodes_CreateQemuMoveDisk", State: aj.Observed,
		Target: aj.Target{Node: "pve1", VMID: 123, Storage: "a", Backing: a.BackingKey(), IntendedVolume: eph},
		VolIDs: []string{eph, "a:999/vm-999-disk-0.qcow2"},
	})
	c.configs[123] = map[string]any{"virtio0": root, "scsi1": eph + ",size=1G"}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": {root, eph}}
	report, err := auditStorageAllocationRecords(context.Background(), deps, []aj.Record{vm}, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
		return e.AllocationID == vm.ID && e.VolumeID == eph && e.VMID == 0
	}) {
		t.Fatalf("VM record not evidenced at its retained ephemeral's old name %s: %+v", eph, report.Evidence)
	}
}

// TestDeleteDiskRenamedOldNameSharedMigrated is the hand-attached volume on
// the old name after 777 migrated from n1 to n2. The storage is shared, so the
// volume 777 holds on n2 is the one the move on n1 renamed the disk off, and
// delete_disk ends Deleted with the hand-attached volume and its slot as they
// were.
func TestDeleteDiskRenamedOldNameSharedMigrated(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["scsi3"] = f.volume + ",size=1G"
	if f.client.vmNodes == nil {
		f.client.vmNodes = map[int]string{}
	}
	f.client.vmNodes[777] = "n2"
	before := captureRenamedNameState(t, f)
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk with a hand-attached volume on the old name after 777 migrated: %v", err)
	}
	requireRenamedDiskDeletedAlone(t, f, key, landed, before)
}

// TestDeleteDiskRenamedOldNameOnlyUnused is the old name held only as 777's
// unused0. No VM holds the volume on a slot, so it could still be a leftover
// of the renamed disk's own data, and the completion audit keeps counting it
// as the record's. delete_disk refuses, the record stays in
// reconciliation_required, and the unused volume and its entry are left as
// they were, on a retry as well.
func TestDeleteDiskRenamedOldNameOnlyUnused(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["unused0"] = f.volume
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err == nil {
			t.Fatalf("delete_disk %d with the old name only on unused0 succeeded, want a refusal", attempt)
		}
		record, err := f.journal.Inspect(f.id)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != aj.ReconciliationRequired || !strings.Contains(record.Reason, "completion audit failed") {
			t.Fatalf("delete_disk %d left state %s (%s), want reconciliation_required from its completion audit", attempt, record.State, record.Reason)
		}
		if f.client.state.volumes[f.volume] == nil {
			t.Fatalf("delete_disk %d destroyed the unused volume %s", attempt, f.volume)
		}
		if unused := f.client.state.configs[777]["unused0"]; unused != f.volume {
			t.Fatalf("delete_disk %d left 777's unused0 = %v, want %s", attempt, unused, f.volume)
		}
	}
}
