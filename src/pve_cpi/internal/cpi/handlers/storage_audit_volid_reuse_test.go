package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// The reuse fixture reproduces a persistent disk replaced twice. Disk "old"
// was attached to VM 2353 as vm-2353-disk-1, then detached and parked on
// parker 90366, where PVE renamed it vm-90366-disk-0. That freed the name,
// and PVE gave vm-2353-disk-1 to the next disk VM 2353 received, disk "new".
// Record "old" still carries vm-2353-disk-1 in the step volids of its attach
// and detach.
const (
	reuseVMID     = 2353
	reuseParker   = 90366
	reuseVolume   = "a:2353/vm-2353-disk-1.qcow2"
	reuseParked   = "a:90366/vm-90366-disk-0.qcow2"
	reuseOtherVM  = 2400
	reuseOtherVol = "a:2400/vm-2400-disk-0.qcow2"
	// reuseOtherVM2Volume is a second volume of VM 2400 that no record names.
	reuseOtherVM2Volume = "a:2400/vm-2400-disk-1.qcow2"
)

// reuseFixture describes how the audited VMs carry their disks. Each flag
// removes or adds one piece of evidence, so a test can show which piece
// decides the holder.
type reuseFixture struct {
	t    *testing.T
	deps Deps
	c    *allocationAuditClient

	// noSerial leaves the serial off VM 2353's drive, and noProvenance
	// leaves disk "new"'s provenance off VM 2353.
	noSerial, noProvenance bool

	old, fresh aj.Record
}

func newReuseFixture(t *testing.T) *reuseFixture {
	t.Helper()
	deps, _, c := auditFixture(t)
	return &reuseFixture{t: t, deps: deps, c: c}
}

// reuseDiskRecord is a returned disk record with a valid disk token, born at
// its allocation-named volume on pve1 and attached to VM 2353 there.
func reuseDiskRecord(t *testing.T, a pve.StorageInfo) aj.Record {
	t.Helper()
	id := moveAllocationID(t)
	token, err := aj.DiskCorrelationToken(id)
	if err != nil {
		t.Fatal(err)
	}
	birth := fmt.Sprintf("a:26936/vm-26936-bosh-fdd7dc59536aba46-alloc-%s.qcow2", id)
	target := aj.Target{Node: "pve1", Storage: "a", Backing: a.BackingKey()}
	create := target
	create.IntendedVolume = birth
	attach := target
	attach.VMID = reuseVMID
	attach.IntendedVolume = reuseVolume
	return aj.Record{
		ID: id, Namespace: "director", Kind: allocationKindDisk, DiskToken: token, State: aj.ReadyToReturn, CID: birth,
		Intent: movePlan(t, "disk", "pve1", map[string]pve.StorageInfo{"a": a}, birth),
		Steps: []aj.Step{
			{ID: "create", Kind: "create_persistent_volume", State: aj.Observed, Target: create, VolIDs: []string{birth}},
			{ID: "attach-move", Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk", State: aj.Observed, Target: attach, VolIDs: []string{birth, reuseVolume}},
			{ID: "attach-config", Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Observed, Target: attach, VolIDs: []string{reuseVolume}},
		},
	}
}

func (f *reuseFixture) build() []aj.Record {
	t := f.t
	a := moveDefinition(t, moveDefinitions["a"])
	f.c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"])}

	// Disk "old" went on from VM 2353 to parker 90366, where PVE renamed it.
	f.old = reuseDiskRecord(t, a)
	detach := aj.Target{Node: "pve1", Storage: "a", Backing: a.BackingKey(), VMID: reuseVMID, IntendedVolume: reuseVolume}
	parked := detach
	parked.VMID = reuseParker
	parked.IntendedVolume = reuseParked
	f.old.Steps = append(f.old.Steps,
		aj.Step{ID: "detach-config", Kind: "lifecycle_detach_disk_Nodes_UpdateQemuConfig", State: aj.Observed, Target: detach, VolIDs: []string{reuseVolume}},
		aj.Step{ID: "detach-move", Kind: "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk", State: aj.Observed, Target: detach, VolIDs: []string{reuseVolume, reuseParked}},
		aj.Step{ID: "detach-attach", Kind: "lifecycle_detach_disk_QEMU_AttachDisk", State: aj.Observed, Target: parked, VolIDs: []string{reuseParked}},
	)
	// Disk "new" is attached to VM 2353 under the name "old" gave up.
	f.fresh = reuseDiskRecord(t, a)

	drive := reuseVolume + ",size=1G"
	if !f.noSerial {
		drive = reuseVolume + ",serial=" + f.fresh.DiskToken + ",size=1G"
	}
	vm := map[string]any{"scsi1": drive}
	if !f.noProvenance {
		entries, err := json.Marshal(map[string]pve.DiskAllocationProvenance{f.fresh.DiskToken: {Version: 1, AllocationID: f.fresh.ID, AllocationNamespace: "director", Volid: reuseVolume, Node: "pve1", Backing: a.BackingKey()}})
		if err != nil {
			t.Fatal(err)
		}
		description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": entries})
		if err != nil {
			t.Fatal(err)
		}
		vm["description"] = description
	}
	f.c.configs[reuseVMID] = vm

	entries, err := json.Marshal(map[string]any{f.old.DiskToken: map[string]any{"allocation_id": f.old.ID, "allocation_namespace": "director", "allocation_backing": a.BackingKey(), "disk_cid": f.old.CID, "source_vm_cid": "2353", "parked_at": "2026-09-30T10:05:24Z", "node": "pve1", "volid": reuseParked, "slot": "scsi0"}})
	if err != nil {
		t.Fatal(err)
	}
	description, err := pve.RenderSentinel("bosh parker", map[string]json.RawMessage{"bosh_parked_disks": entries})
	if err != nil {
		t.Fatal(err)
	}
	f.c.configs[reuseParker] = map[string]any{"description": description, "protection": 1, "scsi0": reuseParked + ",serial=" + f.old.DiskToken + ",size=1G"}

	f.c.nodesRead.volumesByNode = map[string][]string{"pve1": {reuseVolume, reuseParked}}
	return []aj.Record{f.old, f.fresh}
}

func (f *reuseFixture) audit() StorageAllocationAudit {
	f.t.Helper()
	records := f.build()
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

// holderConflict reports whether the audit raised the duplicate-holder
// conflict for allocationID.
func holderConflict(report StorageAllocationAudit, allocationID string) bool {
	return findingWith(report.Conflicts, "disk allocation "+allocationID+" has multiple active holders or duplicate stable tokens", "")
}

// TestAllocationAuditReusedVolumeNameIsNotASecondHolder is the field case. A
// deployment resized its persistent disk twice, so PVE gave the first disk's
// old name to the disk that replaced it. The provenance on PVE names the new
// allocation for that volume, yet the old record's historical step volids
// still named it, and the audit counted it as a second holder of the old
// allocation and refused every create_disk and create_vm in the namespace.
// The serial and the provenance each decide the holder on their own, so the
// test drops each in turn.
func TestAllocationAuditReusedVolumeNameIsNotASecondHolder(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		noSerial, noProvenance bool
	}{
		{name: "serial and provenance"},
		{name: "serial only", noProvenance: true},
		{name: "provenance only", noSerial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReuseFixture(t)
			f.noSerial, f.noProvenance = tc.noSerial, tc.noProvenance
			report := f.audit()
			if len(report.Conflicts) != 0 {
				t.Fatalf("reused volume name raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			if !report.Complete || !report.VMScanComplete {
				t.Fatalf("audit incomplete: complete=%t vm_scan_complete=%t issues:\n%s", report.Complete, report.VMScanComplete, strings.Join(report.Issues, "\n"))
			}
			// The reused volume is evidence of the new allocation only. Stale
			// evidence of the old allocation there would also block every
			// absence proof for it, such as delete_disk after the parker's
			// copy is gone.
			for _, evidence := range report.Evidence {
				if evidence.AllocationID == f.old.ID && evidence.VolumeID == reuseVolume {
					t.Fatalf("old allocation still evidenced at the reused volume: %+v", evidence)
				}
			}
			if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
				return e.AllocationID == f.fresh.ID && e.VolumeID == reuseVolume
			}) {
				t.Fatalf("new allocation lost its evidence at %s: %+v", reuseVolume, report.Evidence)
			}
			if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
				return e.AllocationID == f.old.ID && e.VolumeID == reuseParked && e.VMID == reuseParker
			}) {
				t.Fatalf("old allocation lost its parker holder: %+v", report.Evidence)
			}
		})
	}
}

// TestAllocationAuditReusedNameStillFindsTrueDuplicates shows that a volume
// still counts as a holder whenever the evidence at its location agrees with
// the record, so a real duplicate stays a conflict.
func TestAllocationAuditReusedNameStillFindsTrueDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name string
		// drive and description build VM 2400's extra drive entry and its
		// description from the old record.
		drive       func(old aj.Record) string
		description func(t *testing.T, old aj.Record) string
		// freshToo expects a duplicate-holder conflict for the new
		// allocation as well, because VM 2400 shares its volume.
		freshToo bool
	}{
		{
			// The old disk's serial rides a second drive, so one stable
			// token has two live holders.
			name:  "same serial at two holders",
			drive: func(old aj.Record) string { return reuseOtherVM2Volume + ",serial=" + old.DiskToken },
		},
		{
			// VM 2400 carries provenance naming the old allocation for a
			// drive with no serial, so one allocation has two holders.
			name:  "same allocation at two holders",
			drive: func(aj.Record) string { return reuseOtherVM2Volume },
			description: func(t *testing.T, old aj.Record) string {
				t.Helper()
				entries, err := json.Marshal(map[string]pve.DiskAllocationProvenance{"bpd-0000000000000000": {Version: 1, AllocationID: old.ID, AllocationNamespace: "director", Volid: reuseOtherVM2Volume, Node: "pve1", Backing: moveDefinition(t, moveDefinitions["a"]).BackingKey()}})
				if err != nil {
					t.Fatal(err)
				}
				description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": entries})
				if err != nil {
					t.Fatal(err)
				}
				return description
			},
		},
		{
			// A drive with no serial and no provenance holds the old
			// record's current volume, which the parker also holds.
			name:  "unattributed volume at the current location",
			drive: func(aj.Record) string { return reuseParked },
		},
		{
			// A drive with no serial and no provenance holds a historical
			// name of the old record. Nothing at that holder attributes it
			// to any other allocation, so the name match still stands. The
			// drive is also a second reference to the new disk's volume, so
			// the new allocation has two holders too.
			name:     "unattributed volume at a historical name",
			drive:    func(aj.Record) string { return reuseVolume + ",size=1G" },
			freshToo: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReuseFixture(t)
			// Build once to mint the records, then describe VM 2400 from the
			// old record and audit the same records.
			records := f.build()
			other := map[string]any{"virtio0": reuseOtherVol, "scsi2": tc.drive(f.old)}
			if tc.description != nil {
				other["description"] = tc.description(t, f.old)
			}
			f.c.configs[reuseOtherVM] = other
			f.c.nodesRead.volumesByNode["pve1"] = append(f.c.nodesRead.volumesByNode["pve1"], reuseOtherVol, reuseOtherVM2Volume)
			report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
			if err != nil {
				t.Fatal(err)
			}
			if !holderConflict(report, f.old.ID) {
				t.Fatalf("true duplicate of allocation %s not reported:\n%s", f.old.ID, strings.Join(report.Conflicts, "\n"))
			}
			if report.Complete {
				t.Fatal("audit with a duplicate holder reported complete")
			}
			if holderConflict(report, f.fresh.ID) != tc.freshToo {
				t.Fatalf("new allocation duplicate-holder conflict = %t, want %t:\n%s", !tc.freshToo, tc.freshToo, strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

// sharedReferenceConflict reports whether the audit raised the conflict for
// one volume referenced by more than one VM.
func sharedReferenceConflict(report StorageAllocationAudit, volume string) bool {
	return findingWith(report.Conflicts, "volume "+volume+" is referenced by more than one VM", "")
}

// TestAllocationAuditFindsForeignSerialOnHeldVolume is the reviewer's probe.
// VM 2400 references the old disk's current volume on the parker under a
// serial that no record owns, as a hand-edited slot or a copied serial would
// leave it. The serial rules the drive out as a holder of the old record, yet
// two VMs can now write one disk, so the audit must still refuse.
func TestAllocationAuditFindsForeignSerialOnHeldVolume(t *testing.T) {
	f := newReuseFixture(t)
	records := f.build()
	f.c.configs[reuseOtherVM] = map[string]any{"virtio0": reuseOtherVol, "scsi2": reuseParked + ",serial=bpd-00000000000000aa"}
	f.c.nodesRead.volumesByNode["pve1"] = append(f.c.nodesRead.volumesByNode["pve1"], reuseOtherVol)
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !findingWith(report.Conflicts, "volume "+reuseParked, "VM 2400 on pve1") || !findingWith(report.Conflicts, "volume "+reuseParked, "VM 90366 on pve1") {
		t.Fatalf("two VMs referencing %s not reported:\n%s", reuseParked, strings.Join(report.Conflicts, "\n"))
	}
	if report.Complete {
		t.Fatal("audit with two VMs on one volume reported complete")
	}
}

// sharedReferenceFixture places two VMs, 500 and 501, on the given nodes of
// a three-node cluster with shared storage "a" and node-local storage
// "local", and gives each the given config.
func sharedReferenceFixture(t *testing.T, nodes map[int]string, configs map[int]map[string]any) StorageAllocationAudit {
	t.Helper()
	deps, _, c := auditFixture(t)
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: moveNodes}
	c.nodesRead.nodeNames = moveNodes
	c.nodesRead.guestNodes = nodes
	for vmid, cfg := range configs {
		c.configs[vmid] = cfg
	}
	report, err := auditStorageAllocationRecords(context.Background(), deps, nil, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// TestAllocationAuditSharedReferenceRespectsNodeLocalStorage shows that the
// same volid on node-local storage names one physical volume only on one
// node. Two VMs on different nodes hold two different volumes, and two VMs
// on the same node share one.
func TestAllocationAuditSharedReferenceRespectsNodeLocalStorage(t *testing.T) {
	const volume = "local:500/vm-500-disk-0.qcow2"
	configs := map[int]map[string]any{500: {"scsi0": volume}, 501: {"scsi0": volume}}
	for _, tc := range []struct {
		name  string
		nodes map[int]string
		want  bool
	}{
		{name: "holders on different nodes", nodes: map[int]string{500: "pve1", 501: "pve2"}},
		{name: "holders on the same node", nodes: map[int]string{500: "pve2", 501: "pve2"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := sharedReferenceFixture(t, tc.nodes, configs)
			if got := sharedReferenceConflict(report, volume); got != tc.want {
				t.Fatalf("shared-reference conflict = %t, want %t:\n%s", got, tc.want, strings.Join(report.Conflicts, "\n"))
			}
		})
	}
}

// TestAllocationAuditSharedISOIsNotADuplicate shows that many VMs mounting
// one ISO as a CD-ROM is normal and raises nothing.
func TestAllocationAuditSharedISOIsNotADuplicate(t *testing.T) {
	const iso = "a:iso/shared.iso"
	report := sharedReferenceFixture(t, map[int]string{500: "pve1", 501: "pve1"}, map[int]map[string]any{
		500: {"scsi0": "a:500/vm-500-disk-0.qcow2", "ide2": iso + ",media=cdrom"},
		501: {"scsi0": "a:501/vm-501-disk-0.qcow2", "ide2": iso + ",media=cdrom"},
	})
	if len(report.Conflicts) != 0 {
		t.Fatalf("shared ISO raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
	}
}

// TestAllocationAuditVMRecordYieldsReusedName covers a VM record whose
// recorded volume name now belongs to a persistent disk. VM 123's record
// created a:123/vm-123-disk-1, and VM 123 now holds a disk under that name
// whose serial and provenance name disk allocation X. The listing must not
// evidence the VM record there, and must evidence X.
func TestAllocationAuditVMRecordYieldsReusedName(t *testing.T) {
	deps, _, c := auditFixture(t)
	a := moveDefinition(t, moveDefinitions["a"])
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"])}
	root, reused := "a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"
	vm := moveVMRecord(t, "agent", "pve1", 123, map[string]pve.StorageInfo{"a": a}, root, reused)
	disk := moveDiskRecord(t, "pve1", reused, map[string]pve.StorageInfo{"a": a})
	token, err := aj.DiskCorrelationToken(disk.ID)
	if err != nil {
		t.Fatal(err)
	}
	disk.DiskToken = token
	provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: disk.ID, AllocationNamespace: "director", Volid: reused, Node: "pve1", Backing: a.BackingKey()}
	c.configs[123] = map[string]any{"virtio0": root, "scsi1": reused + ",serial=" + token, "description": moveDescription(t, moveMarker(t, vm.ID, "agent"), provenance)}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": {root, reused}}
	report, err := auditStorageAllocationRecords(context.Background(), deps, []aj.Record{vm, disk}, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("reused VM volume name raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	if slices.ContainsFunc(report.Issues, func(issue string) bool { return strings.Contains(issue, "disagrees with recorded physical target") }) {
		t.Fatalf("reused VM volume name raised a target disagreement:\n%s", strings.Join(report.Issues, "\n"))
	}
	if slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool { return e.AllocationID == vm.ID && e.VolumeID == reused }) {
		t.Fatalf("VM record still evidenced at the reused name: %+v", report.Evidence)
	}
	if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool { return e.AllocationID == disk.ID && e.VolumeID == reused }) {
		t.Fatalf("disk allocation lost its evidence at the reused name: %+v", report.Evidence)
	}
	if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool { return e.AllocationID == vm.ID && e.VolumeID == root }) {
		t.Fatalf("VM record lost its evidence at its own root volume: %+v", report.Evidence)
	}
}

// TestAllocationAuditUnheldReusedNameStaysEvidenced pins the fail-closed side
// of the listing rule. When no VM holds a reused name, here because VM 2353
// keeps it only as unused0, nothing on PVE attributes the volume, so the old
// record's name still evidences it and absence proofs for the old record
// still refuse.
func TestAllocationAuditUnheldReusedNameStaysEvidenced(t *testing.T) {
	f := newReuseFixture(t)
	records := f.build()
	cfg := f.c.configs[reuseVMID]
	delete(cfg, "scsi1")
	cfg["unused0"] = reuseVolume
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool { return e.AllocationID == f.old.ID && e.VolumeID == reuseVolume }) {
		t.Fatalf("unheld reused name no longer evidences the old record: %+v", report.Evidence)
	}
}
