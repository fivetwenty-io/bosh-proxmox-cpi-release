package handlers_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// atCreate is what the request that creates the guest sees of its context.
type atCreate struct {
	called      bool
	hasDeadline bool
	left        time.Duration
	hasSendBy   bool
	claimed     bool
}

// TestCreateVMKeepsEnoughContextAtThePostForAColdStartProbe runs the real
// create_vm handler under the default configuration and records the context
// that QEMU().Create receives. The endpoint pool starts its cold start for a
// request only when the request carries no send-by deadline and its context
// has at least endpointpool.ProbeLimit left, so a deadline from the VMID claim
// window or the create's own bound that fell short would turn the cold start
// off on every create_vm without any other test failing.
func TestCreateVMKeepsEnoughContextAtThePostForAColdStartProbe(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	deps := buildVMDeps(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{})
	pools := &noopPoolService{}
	deps.PVE.(*mockPVEClient).poolsSvc = pools
	var seen atCreate
	q.createFn = func(ctx context.Context, node string, params map[string]any) (string, error) {
		seen.called = true
		if deadline, ok := ctx.Deadline(); ok {
			seen.hasDeadline, seen.left = true, time.Until(deadline)
		}
		_, seen.hasSendBy = endpointpool.SendBy(ctx, http.MethodPost, "/nodes/"+node+"/qemu")
		_, seen.claimed, _ = pools.GetPoolComment(ctx, pve.ClusterLockPoolName(fmt.Sprintf("vm-%v", params["vmid"])))
		return "UPID:pve:create:ok", nil
	}

	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(),
		mkArgs("ag-1", testStemcellCID, map[string]any{}, defaultNetwork(), []string{}, map[string]any{}),
		mkCtxNoAPIVersion("cold-start-context"))

	if err != nil {
		t.Fatalf("create_vm: %v", err)
	}
	if !seen.called {
		t.Fatal("create_vm never sent the create")
	}
	if !seen.claimed {
		t.Error("the create ran without a VMID claim, so the test does not measure the claim's window")
	}
	if seen.hasSendBy {
		t.Error("the create carries a send-by deadline, which keeps the pool from starting the cold start")
	}
	if !seen.hasDeadline {
		t.Fatal("the create's context has no deadline, but the claim window and the create's bound always set one")
	}
	if seen.left < endpointpool.ProbeLimit {
		t.Errorf("the create's context has %v left at the POST, want at least the pool's probe limit %v",
			seen.left, endpointpool.ProbeLimit)
	}
}
