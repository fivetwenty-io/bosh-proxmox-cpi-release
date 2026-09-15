package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

type provenanceGuardNodes struct {
	sdknodes.Service
	updates int
	err     error
}

func (n *provenanceGuardNodes) UpdateQemuConfig(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
	n.updates++
	return n.err
}

type provenanceGuardQEMU struct {
	sdkqemu.Service
}

func (q *provenanceGuardQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return map[string]any{"description": ""}, nil
}

type provenanceGuardClient struct {
	pve.Client
	nodes *provenanceGuardNodes
	qemu  *provenanceGuardQEMU
}

func (c provenanceGuardClient) Nodes() sdknodes.Service { return c.nodes }
func (c provenanceGuardClient) QEMU() sdkqemu.Service   { return c.qemu }

// The create-time provenance write must never reach the managed allocation
// guard. On the managed storage-placement path deps.PVE is the guard's client,
// and the guard treats every UpdateQemuConfig as a gated mutation: a transient
// failure on this one advisory description PUT would poison the allocation,
// fail a create_vm that had otherwise succeeded, and leave a record in
// ReconciliationRequired for an operator to clear. The write is not part of the
// allocation's mutation set, so it goes to the client underneath the guard.
func TestPersistCreateProvenance_BypassesTheAllocationGuard(t *testing.T) {
	t.Parallel()

	raw := provenanceGuardClient{nodes: &provenanceGuardNodes{}, qemu: &provenanceGuardQEMU{}}
	gated := 0
	guard, err := NewManagedAllocationGuard(raw, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) {
			gated++
			return "step", nil
		},
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(_ context.Context, _ ManagedAllocationMutation, _ string, err error) error { return err },
	})
	if err != nil {
		t.Fatalf("NewManagedAllocationGuard: %v", err)
	}

	deps := Deps{PVE: guard.Client(), Logger: log.NewNopLogger()}
	parsed := &createVMParsedArgs{
		stemcellCID:      ":heavy:local:import/bosh-stemcell-ubuntu-noble-1.585-deadbeef.qcow2",
		stemcellKind:     pve.StemcellKindHeavy,
		stemcellFilename: "bosh-stemcell-ubuntu-noble-1.585-deadbeef.qcow2",
		rawVolid:         "local:import/bosh-stemcell-ubuntu-noble-1.585-deadbeef.qcow2",
	}
	shape := &createVMShape{node: "pve1"}

	persistCreateProvenance(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)

	if raw.nodes.updates != 1 {
		t.Errorf("provenance write reached the underlying client %d times, want 1", raw.nodes.updates)
	}
	if gated != 0 {
		t.Errorf("provenance write was gated by the allocation guard %d times, want 0", gated)
	}
	if guardErr := guard.Err(); guardErr != nil {
		t.Errorf("guard was touched by the provenance write: %v", guardErr)
	}

	// The same holds when the write fails: the guard stays clean, so create_vm
	// is not turned into a reconciliation.
	raw.nodes.err = context.DeadlineExceeded
	persistCreateProvenance(context.Background(), deps, log.NewNopLogger(), parsed, shape, 100)
	if gated != 0 {
		t.Errorf("a failing provenance write was gated %d times, want 0", gated)
	}
	if guardErr := guard.Err(); guardErr != nil {
		t.Errorf("a failing provenance write poisoned the allocation: %v", guardErr)
	}
}
