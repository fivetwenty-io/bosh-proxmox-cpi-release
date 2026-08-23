// Package handlers internal tests for the commit-indeterminate sweep's
// peer-won-VMID guard (sweepCandidateVMID).
package handlers

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// sweepGuardDeps builds Deps whose QEMU.Config answers the ownership probe
// with cfg/cfgErr and whose Nodes.DeleteQemu records destroys. Stop returns
// a plain error so the rollback's best-effort stop phase exits immediately.
func sweepGuardDeps(cfg map[string]any, cfgErr error, deleted *[]string) Deps {
	qemuSvc := &etQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return cfg, cfgErr
		},
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", stderrors.New("VM not running")
		},
	}
	ns := &templateGapNodesSvc{
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			*deleted = append(*deleted, vmidStr)
			return nil, nil // synchronous destroy, no task to await
		},
	}
	return Deps{
		Config: &config.CPIConfig{Node: "pve-vm", VMStorage: "local-lvm"},
		PVE: &templateGapPVE{
			nodes:   ns,
			cluster: &templateGapClusterSvc{},
			qemu:    qemuSvc,
		},
		Logger: log.NewNopLogger(),
	}
}

// TestSweepCandidateVMID_PeerWonVMID_LeftAlone pins the guard's core case:
// between a commit-indeterminate create failure and the sweep, a concurrent
// create won the same VMID. The guest's name differs from the one our create
// params carried, so the sweep must not destroy it.
func TestSweepCandidateVMID_PeerWonVMID_LeftAlone(t *testing.T) {
	t.Parallel()
	var deleted []string
	deps := sweepGuardDeps(map[string]any{"name": "vm-peer"}, nil, &deleted)

	sweepCandidateVMID(context.Background(), deps, "pve-vm", 4242, "vm-ours", nil, log.NewNopLogger())

	if len(deleted) != 0 {
		t.Errorf("a peer's VM at the candidate VMID must be left alone; destroys=%v", deleted)
	}
}

// TestSweepCandidateVMID_MissingGuest_NothingToSweep: a 404 on the probe
// means the failed create registered nothing; there is nothing to destroy.
func TestSweepCandidateVMID_MissingGuest_NothingToSweep(t *testing.T) {
	t.Parallel()
	var deleted []string
	notFound := sdkerrors.ParseAPIError(404, []byte(`{"message":"Configuration file 'nodes/pve-vm/qemu-server/4242.conf' does not exist"}`))
	deps := sweepGuardDeps(nil, notFound, &deleted)

	sweepCandidateVMID(context.Background(), deps, "pve-vm", 4242, "vm-ours", nil, log.NewNopLogger())

	if len(deleted) != 0 {
		t.Errorf("a missing guest must not trigger a destroy; destroys=%v", deleted)
	}
}

// TestSweepCandidateVMID_MatchingName_Swept: the guest at the candidate VMID
// carries the name our create params wrote: it is ours, sweep it.
func TestSweepCandidateVMID_MatchingName_Swept(t *testing.T) {
	t.Parallel()
	var deleted []string
	deps := sweepGuardDeps(map[string]any{"name": "vm-ours"}, nil, &deleted)

	sweepCandidateVMID(context.Background(), deps, "pve-vm", 4242, "vm-ours", nil, log.NewNopLogger())

	if len(deleted) != 1 || deleted[0] != "4242" {
		t.Errorf("our own partial VM must be swept; destroys=%v", deleted)
	}
}

// TestSweepCandidateVMID_UnnamedGuest_Swept: an unnamed guest cannot be
// discriminated, so the sweep proceeds (best-effort; the destroy itself
// tolerates already-gone verdicts).
func TestSweepCandidateVMID_UnnamedGuest_Swept(t *testing.T) {
	t.Parallel()
	var deleted []string
	deps := sweepGuardDeps(map[string]any{}, nil, &deleted)

	sweepCandidateVMID(context.Background(), deps, "pve-vm", 4242, "vm-ours", nil, log.NewNopLogger())

	if len(deleted) != 1 {
		t.Errorf("an unnamed guest falls through to the normal cleanup; destroys=%v", deleted)
	}
}
