package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// These tests drive set_vm_metadata through the handler while the VM moves or
// locks under it. They pin that the description write follows the VM to the
// node it is on now, that a write whose answer was lost is read back before it
// is sent again, and that a node which cannot answer is refused.

// motionFixture is VM 101 whose index row the test moves between nodes.
type motionFixture struct {
	mu      sync.Mutex
	node    string
	lock    string
	reads   int
	onRead  func(f *motionFixture, reads int)
	offline map[string]bool

	// indexLag, when set, answers the cluster scan with no row for the VM
	// while it reports true, the way an index that has not caught up with a
	// VM that was just created does.
	indexLag func(reads int) bool

	// descriptions and tags are what the config holds, and writes are the
	// nodes each description write was sent to.
	description string
	tags        string
	name        string
	writes      []string

	// updateErr, when set, answers a write after it runs: landed decides
	// whether the write took effect.
	updateErr func(node string, call int) (err error, landed bool)
	// configErr answers a config read on a node, or nil.
	configErr func(node string) error

	deps handlers.Deps
}

func newMotionFixture(t *testing.T, node string) *motionFixture {
	t.Helper()
	f := &motionFixture{node: node, offline: map[string]bool{}, tags: "bosh-cpi"}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, at string, _ int) (map[string]any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.configErr != nil {
				if err := f.configErr(at); err != nil {
					return nil, err
				}
			}
			cfg := map[string]any{"description": f.description, "tags": f.tags}
			if f.name != "" {
				cfg["name"] = f.name
			}
			return cfg, nil
		},
	}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, at, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.writes = append(f.writes, at)
			var err error
			landed := true
			if f.updateErr != nil {
				err, landed = f.updateErr(at, len(f.writes))
			}
			if landed {
				if params.Description != nil {
					f.description = *params.Description
				}
				if params.Tags != nil {
					f.tags = *params.Tags
				}
				if params.Name != nil {
					f.name = *params.Name
				}
			}
			return err
		},
	}
	f.deps = testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{})
	mc := f.deps.PVE.(*mockPVEClient)
	mc.clusterSvc = &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads++
			if f.onRead != nil {
				f.onRead(f, f.reads)
			}
			if f.indexLag != nil && f.indexLag(f.reads) {
				return &cluster.ListResourcesResponse{}, nil
			}
			row := map[string]any{"vmid": 101, "node": f.node, "type": "qemu", "tags": "bosh-cpi"}
			if f.lock != "" {
				row["lock"] = f.lock
			}
			raw, _ := json.Marshal(row)
			return &cluster.ListResourcesResponse{raw}, nil
		},
		listConfigNodesFn: func(context.Context) (*cluster.ListConfigNodesResponse, error) {
			return &cluster.ListConfigNodesResponse{
				json.RawMessage(`{"name":"pve-node1","node":"pve-node1"}`),
				json.RawMessage(`{"name":"pve-node2","node":"pve-node2"}`),
			}, nil
		},
		listStatusFn: func(context.Context) (*cluster.ListStatusResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			members := []string{}
			for _, n := range []string{"pve-node1", "pve-node2"} {
				if !f.offline[n] {
					members = append(members, n)
				}
			}
			return handlers.AbsenceOnlineStatus(members...), nil
		},
	}
	return f
}

func (f *motionFixture) run(ctx context.Context) error {
	md := map[string]any{"director": "bosh", "deployment": "cf", "job": "cell", "index": "0", "id": "vm-1"}
	_, err := handlers.HandleSetVMMetadata(f.deps).Handle(ctx, marshalArgs("101", md), jsonrpc.Context{})
	return err
}

func (f *motionFixture) writeNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// TestHandleSetVMMetadata_WaitsOutMigrationAndWritesOnNewNode holds a migrate
// lock for several polls and lands the VM on node2. The write goes to node2.
func TestHandleSetVMMetadata_WaitsOutMigrationAndWritesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.lock = "migrate"
	f.onRead = func(f *motionFixture, reads int) {
		if reads >= 4 {
			f.node, f.lock = "pve-node2", ""
		}
	}

	if err := f.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 1 || got[0] != "pve-node2" {
		t.Fatalf("description writes = %v, want one on pve-node2", got)
	}
}

// TestHandleSetVMMetadata_MoveAfterTheWaitIsFollowed moves the VM after the
// wait returned. The config read on node1 answers config missing, and the write
// is sent to node2, where the VM is now.
func TestHandleSetVMMetadata_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	moved := false
	f.configErr = func(node string) error {
		if node == "pve-node1" && !moved {
			moved = true
			f.node = "pve-node2"
			return makeAPIErr(500, "Configuration file 'nodes/pve-node1/qemu-server/101.conf' does not exist")
		}
		if node == "pve-node1" {
			return makeAPIErr(500, "Configuration file 'nodes/pve-node1/qemu-server/101.conf' does not exist")
		}
		return nil
	}

	if err := f.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 1 || got[0] != "pve-node2" {
		t.Fatalf("description writes = %v, want one on pve-node2", got)
	}
}

// TestHandleSetVMMetadata_LockAfterTheWaitIsWaitedOut answers a config lock to
// the first write and clears it by the next poll, so the write is sent again.
func TestHandleSetVMMetadata_LockAfterTheWaitIsWaitedOut(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.updateErr = func(_ string, call int) (error, bool) {
		if call == 1 {
			return errors.New("can't lock file '/var/lock/qemu-server/lock-101.conf' - got timeout"), false
		}
		return nil, true
	}

	if err := f.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 2 {
		t.Fatalf("description writes = %v, want the refused write and its resend", got)
	}
}

// TestHandleSetVMMetadata_DeadNodeDefersWithoutWrite leaves the VM on a node
// the cluster does not list as online. The call is retriable and writes nothing.
func TestHandleSetVMMetadata_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node2")
	f.offline["pve-node2"] = true

	requireHARetriable(t, f.run(settleTestCtx()), "pve-node2")
	if got := f.writeNodes(); len(got) != 0 {
		t.Fatalf("no write may reach a dead node, got %v", got)
	}
}

// TestHandleSetVMMetadata_LostAnswerThatLandedIsNotResent loses the answer of a
// write that took effect. The handler reads the config back, sees its
// description, and does not send it again.
func TestHandleSetVMMetadata_LostAnswerThatLandedIsNotResent(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.updateErr = func(_ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), true
		}
		return nil, true
	}

	if err := f.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 1 {
		t.Fatalf("description writes = %v, want exactly one", got)
	}
}

// TestHandleSetVMMetadata_LostAnswerThatDidNotLandIsResent loses the answer of
// a write that did not take effect, so the read-back finds the old description
// and the write is sent again.
func TestHandleSetVMMetadata_LostAnswerThatDidNotLandIsResent(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.updateErr = func(_ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), false
		}
		return nil, true
	}

	if err := f.run(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 2 {
		t.Fatalf("description writes = %v, want the lost write and its resend", got)
	}
}

// TestHandleSetVMMetadata_NotFoundWithVMStillPresentIsNotGone answers 404 to
// every write while the cluster still lists the VM. The call is retriable, and
// it is never VMNotFound.
func TestHandleSetVMMetadata_NotFoundWithVMStillPresentIsNotGone(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.updateErr = func(string, int) (error, bool) { return notFoundAPIErr(), false }

	err := f.run(settleTestCtx())
	requireHARetriable(t, err)
	var typed *cpierrors.Error
	if errors.As(err, &typed) && typed.Type() == cpierrors.TypeVMNotFound {
		t.Fatalf("a VM that is still listed must not be reported gone: %v", err)
	}
}

// TestSetVMMetadata_PoolSentinelFollowsTheVM moves the VM to node2 between the
// metadata write and the pool record, so the first record write answers config
// missing on node1. The record is written to node2, once, and the call
// succeeds.
func TestSetVMMetadata_PoolSentinelFollowsTheVM(t *testing.T) {
	t.Parallel()
	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh", Layer: pve.PoolLayerTemplate, Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh")
	mc := fx.deps.PVE.(*mockPVEClient)

	var mu sync.Mutex
	node := "pve-node1"
	var writes []string
	mc.clusterSvc = &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			return clusterVMOnNodeWithPool(101, node, "bosh"), nil
		},
	}
	mc.nodesSvc.(*mockNodesService).updateQemuConfigFn = func(
		_ context.Context, at, _ string, params *sdknodes.UpdateQemuConfigParams,
	) error {
		mu.Lock()
		defer mu.Unlock()
		if params == nil || params.Description == nil {
			return nil
		}
		writes = append(writes, at)
		// The metadata write is the first; the pool record is the second.
		if len(writes) == 2 {
			node = "pve-node2"
			return makeAPIErr(500, "Configuration file 'nodes/pve-node1/qemu-server/101.conf' does not exist")
		}
		return nil
	}

	h := handlers.HandleSetVMMetadata(fx.deps)
	if _, err := h.Handle(settleTestCtx(), marshalArgs("101", standardMetadata()), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"pve-node1", "pve-node1", "pve-node2"}
	if len(writes) != len(want) {
		t.Fatalf("description writes = %v, want %v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Fatalf("description writes = %v, want %v", writes, want)
		}
	}
}

// TestHandleSetVMMetadata_VMOnlyTheProbeFindsIsWrittenAtOnce is the call that
// follows create_vm before the cluster index has a row for the new VM. The
// config probe finds the VM, and the handler writes to it without waiting for
// the row, because the write's own protocol meets a lock or a move.
func TestHandleSetVMMetadata_VMOnlyTheProbeFindsIsWrittenAtOnce(t *testing.T) {
	t.Parallel()
	f := newMotionFixture(t, "pve-node1")
	f.indexLag = func(int) bool { return true }
	clk := newElapsedClock()

	if err := f.run(clk.ctx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := f.writeNodes(); len(got) != 1 || got[0] != "pve-node1" {
		t.Fatalf("description writes = %v, want one on pve-node1", got)
	}
	if spent := clk.Since(clk.start); spent != 0 {
		t.Fatalf("the call waited %s for an index row, want no wait", spent)
	}
}
