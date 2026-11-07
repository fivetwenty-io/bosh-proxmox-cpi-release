package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// These tests tie the time set_vm_metadata spends waiting under the VMID lock
// to the TTL the lock claims with. They run the handler on a fake clock where
// a migration lock appears after the first wait and never clears, and they
// compare the fake time the handler spent under the lock with
// handlers.VMIDLockTTLFor. A wait added under the lock without a matching term
// in the lock budget makes the elapsed time pass the TTL, and these fail.

// elapsedClock is a fake clock whose time advances only through sleep, and
// which reports the time since it was created.
type elapsedClock struct {
	mu    sync.Mutex
	start time.Time
	now   time.Time
}

func newElapsedClock() *elapsedClock {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &elapsedClock{start: t0, now: t0}
}

func (c *elapsedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *elapsedClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *elapsedClock) Since(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(t)
}

func (c *elapsedClock) ctx() context.Context {
	return pve.WithVMSettleClockForTest(context.Background(), c.Now, c.Sleep)
}

// lockedAPIErr is PVE's answer to a config change on a VM that holds lock.
func lockedAPIErr(vmid int, lock string) error {
	return makeAPIErr(500, fmt.Sprintf("VM %d is locked (%s)\n", vmid, lock))
}

func withMotionWait(t *testing.T, deps handlers.Deps, wait time.Duration) {
	t.Helper()
	secs := int(wait / time.Second)
	deps.Config.HA = &config.HAConfig{MotionWaitSec: &secs}
}

// TestHandleSetVMMetadata_MetadataWaitStaysInsideTheLockTTL leaves the VM under
// a migrate lock that appears after the first settle and never clears. The
// handler gives up after the write's own budget, and that time plus the lock's
// margin fits the TTL the handler claimed with.
func TestHandleSetVMMetadata_MetadataWaitStaysInsideTheLockTTL(t *testing.T) {
	t.Parallel()
	wait := 30 * time.Minute
	f := newMotionFixture(t, "pve-node1")
	withMotionWait(t, f.deps, wait)
	f.updateErr = func(string, int) (error, bool) {
		f.lock = "migrate"
		return lockedAPIErr(101, "migrate"), false
	}

	clk := newElapsedClock()
	err := f.run(clk.ctx())
	requireHARetriable(t, err, "locked (migrate)")
	spent := clk.Since(clk.start)
	if spent < wait {
		t.Fatalf("spent %s, want the handler to have waited out the migration lock for the motion wait", spent)
	}
	if ttl := handlers.VMIDLockTTLFor(wait); spent+handlers.VMIDLockTTLMargin > ttl {
		t.Fatalf("the handler spent %s under the lock, which with the %s margin passes the %s TTL it claims with",
			spent, handlers.VMIDLockTTLMargin, ttl)
	}
}

// TestSetVMMetadata_PoolSentinelWaitIsShort leaves the VM under a migrate lock
// from the pool record's write on. The record is advisory, so the call
// succeeds after waiting only the sentinel's short budget, never the motion
// wait.
func TestSetVMMetadata_PoolSentinelWaitIsShort(t *testing.T) {
	t.Parallel()
	wait := 30 * time.Minute
	desc := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh", Layer: pve.PoolLayerTemplate, Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	fx := newReconcileFixture(t, desc, "bosh")
	withMotionWait(t, fx.deps, wait)
	mc := fx.deps.PVE.(*mockPVEClient)

	var mu sync.Mutex
	locked := false
	writes := 0
	mc.clusterSvc = &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			row := map[string]any{"vmid": 101, "node": "pve-node1", "type": "qemu", "pool": "bosh"}
			if locked {
				row["lock"] = "migrate"
			}
			raw, _ := json.Marshal(row)
			return &cluster.ListResourcesResponse{raw}, nil
		},
	}
	mc.nodesSvc.(*mockNodesService).updateQemuConfigFn = func(
		_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams,
	) error {
		mu.Lock()
		defer mu.Unlock()
		if params == nil || params.Description == nil {
			return nil
		}
		writes++
		// The metadata write is the first. The pool record is the second, and
		// a migration holds the VM from then on.
		if writes >= 2 {
			locked = true
			return lockedAPIErr(101, "migrate")
		}
		return nil
	}

	clk := newElapsedClock()
	h := handlers.HandleSetVMMetadata(fx.deps)
	if _, err := h.Handle(clk.ctx(), marshalArgs("101", standardMetadata()), jsonrpc.Context{}); err != nil {
		t.Fatalf("the pool record is advisory, so the call must succeed: %v", err)
	}
	spent := clk.Since(clk.start)
	if spent > handlers.PoolSentinelBudget {
		t.Fatalf("the call waited %s for the pool record, want no more than %s", spent, handlers.PoolSentinelBudget)
	}
	if spent == 0 {
		t.Fatal("the pool record's write was never refused, so the test did not reach the wait")
	}
}
