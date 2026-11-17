package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// listStatusWithoutQuorum is what the one surviving node of a three-node
// cluster answers once the other two are gone: the cluster row says it is not
// quorate, and the other members are offline.
func listStatusWithoutQuorum(reads *atomic.Int32) func(context.Context) (*sdkcluster.ListStatusResponse, error) {
	return func(context.Context) (*sdkcluster.ListStatusResponse, error) {
		reads.Add(1)
		gib := int64(1024 * 1024 * 1024)
		rows := make(sdkcluster.ListStatusResponse, 0, 4)
		for _, row := range []map[string]any{
			{"type": "cluster", "name": "az2", "quorate": 0, "nodes": 3},
			{"type": "node", "name": "lab-pve-cpi-az2-0", "online": 0},
			{"type": "node", "name": "lab-pve-cpi-az2-1", "online": 1, "local": 1,
				"maxcpu": int64(8), "maxmem": 16 * gib, "mem": 4 * gib, "cpu": 0.1},
			{"type": "node", "name": "lab-pve-cpi-az2-2", "online": 0},
		} {
			raw, _ := json.Marshal(row)
			rows = append(rows, raw)
		}
		return &rows, nil
	}
}

// TestCreateVM_FailsFastWithoutQuorum creates a VM on a cluster whose entry
// node reports it not quorate, once with a pinned target_node and once through
// placement. Both fail retriable with the same message, which names the lost
// quorum, and neither creates anything.
func TestCreateVM_FailsFastWithoutQuorum(t *testing.T) {
	t.Parallel()
	for name, cloudProps := range map[string]map[string]any{
		"target_node": {"target_node": "lab-pve-cpi-az2-0"},
		"placement":   {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var created atomic.Int32
			q := &vmMockQEMU{
				createFn: func(context.Context, string, map[string]any) (string, error) {
					created.Add(1)
					return "UPID:lab-pve-cpi-az2-1:ok", nil
				},
			}
			var reads atomic.Int32
			deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusWithoutQuorum(&reads), emptyListResources,
				&vmMockAgent{}, nil)
			deps.Config.EnsureNoIPConflicts = placementDisabled
			args := mkArgs("agent-no-quorum", testStemcellCID, cloudProps,
				map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
				[]string{}, map[string]any{})

			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("no-quorum"))
			if err == nil {
				t.Fatal("create_vm succeeded on a cluster without quorum")
			}
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || !typed.OkToRetry() {
				t.Fatalf("want a retriable error, got %T %v", err, err)
			}
			for _, want := range []string{"the cluster has lost quorum", "the entry node does not report the cluster quorate"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			for _, unwanted := range []string{"partial fleet", "in maintenance"} {
				if strings.Contains(err.Error(), unwanted) {
					t.Fatalf("error %q still says %q", err, unwanted)
				}
			}
			if created.Load() != 0 {
				t.Fatal("create_vm created a VM on a cluster without quorum")
			}
			if n := reads.Load(); n != 1 {
				t.Fatalf("create_vm read the cluster status %d times before it failed, want 1", n)
			}
		})
	}
}
