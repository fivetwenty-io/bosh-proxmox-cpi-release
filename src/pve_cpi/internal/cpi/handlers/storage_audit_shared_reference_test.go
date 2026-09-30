package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// sharedReferenceCluster places VMs on the given nodes of a three-node
// cluster with shared storage "a" and node-local storage "local", gives each
// the given config, and audits it against records. It returns the fixture's
// empty journal too, so a test can run the admission gates.
func sharedReferenceCluster(t *testing.T, nodes map[int]string, configs map[int]map[string]any, records []aj.Record) (Deps, *aj.Journal, StorageAllocationAudit) {
	t.Helper()
	deps, j, c := auditFixture(t)
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: moveNodes}
	c.nodesRead.nodeNames = moveNodes
	c.nodesRead.guestNodes = nodes
	for vmid, cfg := range configs {
		c.configs[vmid] = cfg
	}
	report, err := auditStorageAllocationRecords(context.Background(), deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	return deps, j, report
}

// sharedReferenceAllocationVolume is a persistent-disk volid of VM 500 on
// storage whose name carries our namespace's locator.
func sharedReferenceAllocationVolume(t *testing.T, storage string) string {
	t.Helper()
	name, err := pve.AllocationVolumeName(500, "director", moveAllocationID(t), "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	return storage + ":500/" + name
}

// requireAdmitted runs the create_disk and create_vm admission gates and
// fails when either refuses.
func requireAdmitted(t *testing.T, deps Deps, j *aj.Journal) {
	t.Helper()
	if _, err := admitStorageAllocation(context.Background(), deps, j, []string{"pve1"}); err != nil {
		t.Fatalf("create_disk admission refused: %v", err)
	}
	if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent"); err != nil {
		t.Fatalf("create_vm admission refused: %v", err)
	}
}

// TestAllocationAuditForeignSharedVolumeDoesNotBlockCreateVM covers guests
// that BOSH does not manage and that share a writable volume among
// themselves, as a shared-disk cluster or a hand-copied configuration does.
// The volume is not ours, so it raises nothing and both admissions pass.
func TestAllocationAuditForeignSharedVolumeDoesNotBlockCreateVM(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nodes  map[int]string
		volume func(t *testing.T) string
	}{
		{name: "data disk on shared storage held on two nodes", nodes: map[int]string{500: "pve1", 501: "pve3"}, volume: func(*testing.T) string { return "a:500/vm-500-disk-1.qcow2" }},
		{name: "data disk on node-local storage held on one node", nodes: map[int]string{500: "pve2", 501: "pve2"}, volume: func(*testing.T) string { return "local:500/vm-500-disk-1.qcow2" }},
		{name: "another namespace's allocation volume", nodes: map[int]string{500: "pve1", 501: "pve3"}, volume: func(t *testing.T) string {
			name, err := pve.AllocationVolumeName(500, "other", moveAllocationID(t), "qcow2")
			if err != nil {
				t.Fatal(err)
			}
			return "a:500/" + name
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			volume := tc.volume(t)
			deps, j, report := sharedReferenceCluster(t, tc.nodes, map[int]map[string]any{
				500: {"scsi0": "a:500/vm-500-disk-0.qcow2", "scsi1": volume},
				501: {"scsi0": "a:501/vm-501-disk-0.qcow2", "scsi1": volume},
			}, nil)
			if len(report.Conflicts) != 0 {
				t.Fatalf("foreign shared volume raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			requireAdmitted(t, deps, j)
		})
	}
}

// TestAllocationAuditForeignPassthroughDoesNotBlockCreateVM covers one
// physical disk passed through to two guests that BOSH does not manage on one
// node, as a stopped copy of a NAS VM leaves it. A device carries no
// namespace locator and no record names one, so it raises nothing.
func TestAllocationAuditForeignPassthroughDoesNotBlockCreateVM(t *testing.T) {
	const device = "/dev/disk/by-id/ata-SHARED"
	deps, j, report := sharedReferenceCluster(t, map[int]string{500: "pve1", 501: "pve1"}, map[int]map[string]any{
		500: {"scsi0": "a:500/vm-500-disk-0.qcow2", "sata1": device},
		501: {"scsi0": "a:501/vm-501-disk-0.qcow2", "sata1": device},
	}, nil)
	if len(report.Conflicts) != 0 {
		t.Fatalf("foreign passthrough device raised conflicts:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	requireAdmitted(t, deps, j)
}

// TestAllocationAuditOurVolumeSharedWithForeignVMConflicts shows that a
// volume of ours still raises the conflict when a VM BOSH does not manage
// references it too, whichever piece of evidence makes the volume ours.
func TestAllocationAuditOurVolumeSharedWithForeignVMConflicts(t *testing.T) {
	a := moveDefinition(t, moveDefinitions["a"])
	for _, tc := range []struct {
		name string
		// build returns the shared volume, VM 500's config beyond the
		// volume, and the records to audit against.
		build func(t *testing.T) (string, map[string]any, []aj.Record)
	}{
		{name: "allocation name carries our locator", build: func(t *testing.T) (string, map[string]any, []aj.Record) {
			return sharedReferenceAllocationVolume(t, "a"), map[string]any{}, nil
		}},
		{name: "ephemeral name carries our locator", build: func(t *testing.T) (string, map[string]any, []aj.Record) {
			_, suffix, err := pve.ManagedEphemeralVolumeName("nfs", "qcow2", 500, "director", moveAllocationID(t))
			if err != nil {
				t.Fatal(err)
			}
			return "a:" + suffix, map[string]any{}, nil
		}},
		{name: "a record names the volume", build: func(t *testing.T) (string, map[string]any, []aj.Record) {
			const volume = "a:500/vm-500-disk-1.qcow2"
			return volume, map[string]any{}, []aj.Record{moveDiskRecord(t, "pve1", volume, map[string]pve.StorageInfo{"a": a})}
		}},
		{name: "our disk provenance names the volume", build: func(t *testing.T) (string, map[string]any, []aj.Record) {
			const volume = "a:500/vm-500-disk-1.qcow2"
			provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: moveAllocationID(t), AllocationNamespace: "director", Volid: volume, Node: "pve1", Backing: a.BackingKey()}
			return volume, map[string]any{"description": moveDescription(t, moveMarker(t, moveAllocationID(t), "agent"), provenance)}, nil
		}},
		{name: "the drive serial is our disk token", build: func(t *testing.T) (string, map[string]any, []aj.Record) {
			const volume = "a:500/vm-500-disk-1.qcow2"
			record := moveDiskRecord(t, "pve1", "a:9500/vm-9500-disk-0.qcow2", map[string]pve.StorageInfo{"a": a})
			token, err := aj.DiskCorrelationToken(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			record.DiskToken = token
			return volume + ",serial=" + token, map[string]any{}, []aj.Record{record}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive, ours, records := tc.build(t)
			ours["scsi1"] = drive
			volume := strings.Split(drive, ",")[0]
			_, _, report := sharedReferenceCluster(t, map[int]string{500: "pve1", 501: "pve3"}, map[int]map[string]any{
				500: ours,
				501: {"scsi0": "a:501/vm-501-disk-0.qcow2", "scsi1": volume},
			}, records)
			if !findingWith(report.Conflicts, "volume "+volume+" is referenced by more than one VM", "VM 500 on pve1, VM 501 on pve3") {
				t.Fatalf("our volume shared with a foreign VM not reported:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			if report.Complete {
				t.Fatal("audit with two VMs on one volume reported complete")
			}
		})
	}
}

// TestAllocationAuditTwoOfOurVMsSharingAVolumeConflicts shows that two VMs
// referencing one volume of ours raise the conflict, and that create_vm
// admission refuses on it with the short form.
func TestAllocationAuditTwoOfOurVMsSharingAVolumeConflicts(t *testing.T) {
	volume := sharedReferenceAllocationVolume(t, "local")
	deps, j, report := sharedReferenceCluster(t, map[int]string{500: "pve2", 501: "pve2"}, map[int]map[string]any{
		500: {"scsi1": volume},
		501: {"scsi1": volume},
	}, nil)
	if !findingWith(report.Conflicts, "volume "+volume+" is referenced by more than one VM", "VM 500 on pve2, VM 501 on pve2") {
		t.Fatalf("two VMs on one volume of ours not reported:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent")
	if err == nil || !strings.Contains(err.Error(), "volume "+volume+" is attached to 2 VMs: VM 500 on pve2, VM 501 on pve2") {
		t.Fatalf("create_vm admission error = %v, want the shared-volume refusal", err)
	}
}
