package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// diskHolderBySerial returns the VM whose config holds a drive with serial,
// and that drive's volume. It fails when more than one VM holds it.
func diskHolderBySerial(t *testing.T, client *lifecycleFlowPVE, serial string) (int, string) {
	t.Helper()
	holder, volume := 0, ""
	for vmid, cfg := range client.state.configs {
		for key, value := range cfg {
			text, ok := value.(string)
			if !ok || strings.HasPrefix(key, "unused") || !strings.Contains(text, "serial="+serial) {
				continue
			}
			if holder != 0 && holder != vmid {
				t.Fatalf("VMs %d and %d both hold the disk with serial %s", holder, vmid, serial)
			}
			holder, volume = vmid, strings.Split(text, ",")[0]
		}
	}
	if holder == 0 {
		t.Fatalf("no VM holds the disk with serial %s", serial)
	}
	return holder, volume
}

// recreateDiskCall runs a disk handler against vmCID with the flow fixture's
// disk CID.
func recreateDiskCall(t *testing.T, h Handler, vmCID, cid string) error {
	t.Helper()
	_, err := h.Handle(digestCtx(), []json.RawMessage{planJSON(t, vmCID), planJSON(t, cid)}, jsonrpc.Context{})
	return err
}

// TestReplicatedVMRecreateCarriesItsPersistentDisk runs the sequence a BOSH
// recreate, a stemcell update, and a resurrection send for a replicate VM
// whose persistent disk is on shared storage, twice over. detach_disk parks
// the disk, delete_vm removes the VM and its replication job, create_vm makes
// the new VM and attaches the disk it is given before it sets up the new
// job, and attach_disk then finds the disk in place. The second pass starts
// from a disk the first pass's reassignment named for the VM, so its detach
// renames it for the parker, and the next create_vm still takes it, because
// the new VM has no job yet. No move is refused, no volume is destroyed, and
// the disk's record never asks for reconciliation.
func TestReplicatedVMRecreateCarriesItsPersistentDisk(t *testing.T) {
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	_, serial, _ := strings.Cut(client.state.configs[777]["scsi1"].(string), "serial=")
	serial, _, _ = strings.Cut(serial, ",")
	client.replicatedVMs = map[int]bool{777: true}

	vm := 777
	for pass, next := range []int{778, 779} {
		if err := recreateDiskCall(t, HandleDetachDisk(deps), strconv.Itoa(vm), cid); err != nil {
			t.Fatalf("pass %d: detach_disk from replicated VM %d: %v", pass, vm, err)
		}
		parker, parked := diskHolderBySerial(t, client, serial)
		if parker == vm {
			t.Fatalf("pass %d: the detach left the disk on VM %d", pass, vm)
		}
		if pass > 0 && !fakeVolumeOwnedBy(parked, parker) {
			t.Fatalf("pass %d: the detach of a VM-named disk parked %s, want it renamed for parker %d", pass,
				parked, parker)
		}
		// delete_vm: the VM and its replication job go with the purge.
		delete(client.state.configs, vm)
		delete(client.replicatedVMs, vm)

		// create_vm: the new VM exists without a job while the disk_cids
		// pre-attach runs, and the replicate policy adds the job after.
		client.state.configs[next] = map[string]any{"name": "workload", "digest": "1"}
		bare, meta, err := decodeDiskCID(context.Background(), deps, "create_vm", cid)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := resolveDiskForOp(context.Background(), deps, "create_vm", cid, bare, meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attachManagedPersistentDisk(context.Background(), deps, strconv.Itoa(next), "n1", next,
			rd); err != nil {
			t.Fatalf("pass %d: create_vm pre-attach of %s from parker %d into VM %d: %v", pass, parked, parker,
				next, err)
		}
		client.replicatedVMs[next] = true

		// attach_disk: the Director attaches the disk it passed in disk_cids.
		moves := client.moveCalls
		if err := recreateDiskCall(t, HandleAttachDisk(deps), strconv.Itoa(next), cid); err != nil {
			t.Fatalf("pass %d: attach_disk into replicated VM %d after the pre-attach: %v", pass, next, err)
		}
		if client.moveCalls != moves {
			t.Fatalf("pass %d: attach_disk sent %d moves for a disk already in place", pass, client.moveCalls-moves)
		}
		holder, volume := diskHolderBySerial(t, client, serial)
		if holder != next || !strings.Contains(volume, "vm-"+strconv.Itoa(next)+"-") {
			t.Fatalf("pass %d: VM %d holds %s, want VM %d under its own name", pass, holder, volume, next)
		}
		vm = next
	}
	if client.replicationAnswers != 0 || len(client.destroyed) != 0 {
		t.Fatalf("%d refused moves, destroyed %v; the cycle never meets PVE's refusal or frees the disk",
			client.replicationAnswers, client.destroyed)
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the cycle left the record asking for reconciliation: %q", record.Reason)
	}
}
