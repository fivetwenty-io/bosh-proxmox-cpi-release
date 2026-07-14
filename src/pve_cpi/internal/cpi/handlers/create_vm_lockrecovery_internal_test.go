// Package handlers — internal tests for cleanupVM's locked-VM (guest-config
// lock) recovery: skiplock retry restricted to root@pam, and best-effort
// bosh-create-failed tagging when the VM stays orphaned and locked.
package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// --------------------------------------------------------------------------
// lrNodesStub -- nodes.Service fake recording DeleteQemu / CreateQemuStatusStop
// call sequences so tests can assert whether a skiplock retry happened.
// --------------------------------------------------------------------------

type lrNodesStub struct {
	sdknodes.Service // embedded nil: panics on any unconfigured method

	// deleteErrs / deleteResps are consumed in FIFO order, one per DeleteQemu
	// call; when exhausted, the last entry repeats.
	deleteErrs  []error
	deleteResps []*sdknodes.DeleteQemuResponse
	deleteCalls []*sdknodes.DeleteQemuParams
	stopErrs    []error
	stopResps   []*sdknodes.CreateQemuStatusStopResponse
	stopCalls   []*sdknodes.CreateQemuStatusStopParams
	updateCalls int
	updatedTags *string
}

func (n *lrNodesStub) DeleteQemu(
	_ context.Context, _, _ string, params *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	idx := len(n.deleteCalls)
	n.deleteCalls = append(n.deleteCalls, params)
	var err error
	switch {
	case idx < len(n.deleteErrs):
		err = n.deleteErrs[idx]
	case len(n.deleteErrs) > 0:
		err = n.deleteErrs[len(n.deleteErrs)-1]
	}
	var resp *sdknodes.DeleteQemuResponse
	switch {
	case idx < len(n.deleteResps):
		resp = n.deleteResps[idx]
	case len(n.deleteResps) > 0:
		resp = n.deleteResps[len(n.deleteResps)-1]
	default:
		resp = &sdknodes.DeleteQemuResponse{}
	}
	return resp, err
}

func (n *lrNodesStub) CreateQemuStatusStop(
	_ context.Context, _, _ string, params *sdknodes.CreateQemuStatusStopParams,
) (*sdknodes.CreateQemuStatusStopResponse, error) {
	idx := len(n.stopCalls)
	n.stopCalls = append(n.stopCalls, params)
	var err error
	switch {
	case idx < len(n.stopErrs):
		err = n.stopErrs[idx]
	case len(n.stopErrs) > 0:
		err = n.stopErrs[len(n.stopErrs)-1]
	}
	var resp *sdknodes.CreateQemuStatusStopResponse
	switch {
	case idx < len(n.stopResps):
		resp = n.stopResps[idx]
	case len(n.stopResps) > 0:
		resp = n.stopResps[len(n.stopResps)-1]
	default:
		resp = &sdknodes.CreateQemuStatusStopResponse{}
	}
	return resp, err
}

func (n *lrNodesStub) UpdateQemuConfig(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	n.updateCalls++
	if params != nil && params.Tags != nil {
		n.updatedTags = params.Tags
	}
	return nil
}

// --------------------------------------------------------------------------
// lrQEMUStub -- qemu.Service fake for Stop + Config.
// --------------------------------------------------------------------------

type lrQEMUStub struct {
	qemu.Service // embedded nil: panics on any unconfigured method
	stopUPID     string
	stopErr      error
	stopCalls    int
}

func (q *lrQEMUStub) Stop(_ context.Context, _ string, _ int) (string, error) {
	q.stopCalls++
	return q.stopUPID, q.stopErr
}

func (q *lrQEMUStub) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

// --------------------------------------------------------------------------
// lrClient -- pve.Client fake wiring the stubs above.
// --------------------------------------------------------------------------

type lrClient struct {
	pve.Client
	nodes *lrNodesStub
	qemu  *lrQEMUStub
}

func (c *lrClient) Nodes() sdknodes.Service  { return c.nodes }
func (c *lrClient) QEMU() qemu.Service       { return c.qemu }
func (c *lrClient) Cluster() cluster.Service { return newNAStub() }

// Pools returns nil so tagFailedVM's withVMIDLock falls back to the
// best-effort unlocked path -- these tests focus on skiplock/tag behavior,
// not lock ordering.
func (c *lrClient) Pools() pve.PoolService { return nil }

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// lrRootPamConfig returns a CPIConfig whose password-auth identity resolves
// to root@pam (pve.IsRootPamIdentity == true).
func lrRootPamConfig() *config.CPIConfig {
	return &config.CPIConfig{User: "root", Realm: "pam", Password: "secret"}
}

// lrTokenConfig returns a CPIConfig authenticated via a non-root@pam API
// token (pve.IsRootPamIdentity == false) -- a least-privilege token, the
// common production shape.
func lrTokenConfig() *config.CPIConfig {
	return &config.CPIConfig{APIToken: "bosh@pve!bosh-token=uuid"}
}

func lrEnv() map[string]any {
	return map[string]any{"bosh": map[string]any{
		"group":  "dir-cf-router",
		"groups": []any{"router"},
	}}
}

const lockedCloneMsg = "500 unable to destroy VM 100: VM is locked (clone)\n"

// --------------------------------------------------------------------------
// DeleteQemu (purge) skiplock recovery
// --------------------------------------------------------------------------

func TestCleanupVM_DeleteLocked_RootPam_RetriesWithSkiplockAndSucceeds(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{errors.New(lockedCloneMsg), nil},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(context.Background(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (initial + skiplock retry), got %d", len(nodes.deleteCalls))
	}
	first, second := nodes.deleteCalls[0], nodes.deleteCalls[1]
	if first.Skiplock != nil && *first.Skiplock {
		t.Error("first DeleteQemu call must not carry skiplock=true")
	}
	if second.Skiplock == nil || !*second.Skiplock {
		t.Error("retry DeleteQemu call must carry skiplock=true")
	}
	if nodes.updateCalls != 0 {
		t.Errorf("skiplock retry succeeded: bosh-create-failed tag must NOT be written, got %d tag writes", nodes.updateCalls)
	}
}

func TestCleanupVM_DeleteLocked_NonRootPam_NoSkiplockRetry_TagsFailedVM(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{errors.New(lockedCloneMsg)},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(context.Background(), deps, "pve01", 100, lrEnv(), log.NewNopLogger())

	if len(nodes.deleteCalls) != 1 {
		t.Fatalf("non-root@pam identity: expected exactly 1 DeleteQemu call (no skiplock retry), got %d", len(nodes.deleteCalls))
	}
	if nodes.deleteCalls[0].Skiplock != nil && *nodes.deleteCalls[0].Skiplock {
		t.Error("the single DeleteQemu call must not carry skiplock=true for a non-root@pam identity")
	}
	if nodes.updateCalls != 1 {
		t.Fatalf("VM remains locked and orphaned: expected 1 bosh-create-failed tag write, got %d", nodes.updateCalls)
	}
	if nodes.updatedTags == nil {
		t.Fatal("expected non-nil tags written")
	}
}

func TestCleanupVM_DeleteLocked_NonRootPam_NilEnv_TaggingSkipped(t *testing.T) {
	nodes := &lrNodesStub{
		deleteErrs: []error{errors.New(lockedCloneMsg)},
	}
	q := &lrQEMUStub{}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	// env == nil: tagging path not reachable at this call site (e.g. an
	// intermediate placement-fallback candidate cleanup).
	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if nodes.updateCalls != 0 {
		t.Errorf("nil env: tagging must be skipped, got %d tag writes", nodes.updateCalls)
	}
}

// --------------------------------------------------------------------------
// Stop skiplock recovery
// --------------------------------------------------------------------------

func TestCleanupVM_StopLocked_RootPam_RetriesStopWithSkiplock(t *testing.T) {
	nodes := &lrNodesStub{
		stopErrs: []error{nil}, // CreateQemuStatusStop retry succeeds synchronously (empty response)
	}
	q := &lrQEMUStub{stopErr: errors.New(lockedCloneMsg)}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if q.stopCalls != 1 {
		t.Errorf("expected exactly 1 QEMU().Stop call, got %d", q.stopCalls)
	}
	if len(nodes.stopCalls) != 1 {
		t.Fatalf("expected exactly 1 CreateQemuStatusStop (skiplock) retry call, got %d", len(nodes.stopCalls))
	}
	if nodes.stopCalls[0].Skiplock == nil || !*nodes.stopCalls[0].Skiplock {
		t.Error("skiplock retry stop call must carry skiplock=true")
	}
}

func TestCleanupVM_StopLocked_NonRootPam_NoSkiplockRetry(t *testing.T) {
	nodes := &lrNodesStub{}
	q := &lrQEMUStub{stopErr: errors.New(lockedCloneMsg)}
	deps := Deps{Config: lrTokenConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	// Purge succeeds unconditionally (no lock) so this test isolates the Stop
	// skiplock behavior from the Delete path.
	nodes.deleteErrs = []error{nil}

	cleanupVM(context.Background(), deps, "pve01", 100, nil, log.NewNopLogger())

	if len(nodes.stopCalls) != 0 {
		t.Errorf("non-root@pam identity: CreateQemuStatusStop must never be called, got %d calls", len(nodes.stopCalls))
	}
}
