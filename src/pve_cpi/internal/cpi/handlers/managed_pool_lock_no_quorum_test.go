package handlers

import (
	"errors"
	"fmt"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// noQuorumLockCreate is PVE's answer to a sentinel create when it could not
// take its user.cfg lock because the cluster has no quorum.
func noQuorumLockCreate(t *testing.T) error {
	t.Helper()
	return livePoolVerdict(t, "create pool failed: cfs-lock 'file-user_cfg' error: no quorum!")
}

// TestGuardedLockCreateRefusedForNoQuorumSettles covers a guarded detach whose
// lock sentinel create PVE refused for lack of quorum. PVE never took its
// lock, so the create never ran. The guard settles the step as a refusal, the
// detach fails retriable with the lost quorum named, and the record is not
// sent to reconciliation the way an uncertain create sends it.
func TestGuardedLockCreateRefusedForNoQuorumSettles(t *testing.T) {
	locks := newLockContention(t)
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	d := &parkedFlowDisk{deps: deps, client: client, journal: journal, id: id, cid: cid, gate: make(chan struct{})}
	close(d.gate)
	d.deps.PVE = contendedFlowPVE{lifecycleFlowPVE: client, locks: locks, gate: d.gate}
	delete(client.state.configs[777], "scsi1")
	d.deps.Config.DetachedDiskStrategy = "parked"
	parker := d.deps.Config.ParkedDiskVMIDRangeStartValue()
	client.state.configs[parker] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", parker), "tags": "bosh-parker", "protection": 1, "scsihw": "virtio-scsi-pci", "digest": "1"}
	d.parker = parker
	if err := d.attach(t.Context()); err != nil {
		t.Fatalf("setup attach: %v", err)
	}
	locks.reset()
	locks.createErr = noQuorumLockCreate(t)
	err := d.detach(t)
	if err == nil {
		t.Fatal("a detach whose lock PVE refused for lack of quorum succeeded")
	}
	if !errors.Is(err, pve.ErrClusterLockNoQuorum) {
		t.Fatalf("the detach error does not carry the no-quorum refusal: %v", err)
	}
	if errors.Is(err, pve.ErrClusterLockStateUnknown) {
		t.Fatalf("the detach error calls the refused create unknown: %v", err)
	}
	locks.reset()
	record := d.record(t)
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("a create PVE refused for lack of quorum sent the record to reconciliation: %+v", record.Steps)
	}
	for _, step := range record.Steps {
		if step.Attempt == record.ActiveAttempt() && step.Kind == "lifecycle_detach_disk_Pool_CreatePool" && step.State == aj.Planned {
			t.Fatalf("the refused lock create stayed planned: %+v", record.Steps)
		}
	}
	if err := d.detach(t); err != nil {
		t.Fatalf("the detach retried after quorum returned failed: %v", err)
	}
}
