package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
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
// quorum and the entry node that reported it, and neither creates anything.
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
			for _, want := range []string{"the cluster has lost quorum", "the entry node lab-pve-cpi-az2-1 does not report the cluster quorate"} {
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

// TestCreateVM_SkipsTheTeardownSweepWithoutQuorum creates a VM on a cluster
// that has lost quorum. The deferred-teardown sweep lists the cluster's VMs
// and takes a cluster lock for each VM it disposes of, and no lock can be
// taken without quorum, so the call must skip the sweep and list nothing for it.
func TestCreateVM_SkipsTheTeardownSweepWithoutQuorum(t *testing.T) {
	t.Parallel()
	var vmListings atomic.Int32
	listResources := func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		if params != nil && params.Type != nil && *params.Type == "vm" {
			vmListings.Add(1)
		}
		return emptyListResources(ctx, params)
	}
	var reads atomic.Int32
	deps := buildVMDepsPlacement(&vmMockQEMU{}, &vmMockNodes{}, listStatusWithoutQuorum(&reads), listResources,
		&vmMockAgent{}, nil)
	deps.Config.EnsureNoIPConflicts = placementDisabled
	args := mkArgs("agent-no-quorum", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("no-quorum"))
	if err == nil || !strings.Contains(err.Error(), "the cluster has lost quorum") {
		t.Fatalf("want the quorum error, got %v", err)
	}
	if n := vmListings.Load(); n != 0 {
		t.Fatalf("create_vm listed the cluster's VMs %d times for the teardown sweep without quorum, want 0", n)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("create_vm read the cluster status %d times, want 1", n)
	}
}

// listStatusWithOfflineNode is a quorate cluster whose member lab-pve-cpi-az2-0
// is offline, as the entry node lab-pve-cpi-az2-1 reports it.
func listStatusWithOfflineNode(reads *atomic.Int32) func(context.Context) (*sdkcluster.ListStatusResponse, error) {
	return func(context.Context) (*sdkcluster.ListStatusResponse, error) {
		reads.Add(1)
		rows := make(sdkcluster.ListStatusResponse, 0, 3)
		for _, row := range []map[string]any{
			{"type": "cluster", "name": "az2", "quorate": 1, "nodes": 2},
			{"type": "node", "name": "lab-pve-cpi-az2-0", "online": 0},
			{"type": "node", "name": "lab-pve-cpi-az2-1", "online": 1, "local": 1},
		} {
			raw, _ := json.Marshal(row)
			rows = append(rows, raw)
		}
		return &rows, nil
	}
}

// TestCreateVM_FailsFastWhenTheTargetNodeIsOffline pins target_node to a member
// that the cluster status reports offline. The call must fail at once with a
// retriable error that names the node and says it is offline, reading the
// status once and creating nothing, instead of reaching the VMID allocation.
func TestCreateVM_FailsFastWhenTheTargetNodeIsOffline(t *testing.T) {
	t.Parallel()
	var created atomic.Int32
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			created.Add(1)
			return "UPID:lab-pve-cpi-az2-1:ok", nil
		},
	}
	var reads atomic.Int32
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusWithOfflineNode(&reads), emptyListResources,
		&vmMockAgent{}, nil)
	deps.Config.EnsureNoIPConflicts = placementDisabled
	args := mkArgs("agent-offline-target", testStemcellCID, map[string]any{"target_node": "lab-pve-cpi-az2-0"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	start := time.Now()
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("offline-target"))
	elapsed := time.Since(start)
	t.Logf("create_vm on an offline target_node failed in %s", elapsed)
	if err == nil {
		t.Fatal("create_vm succeeded on an offline target_node")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{`"lab-pve-cpi-az2-0"`, "offline"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created.Load() != 0 {
		t.Fatal("create_vm created a VM on an offline node")
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("create_vm read the cluster status %d times, want 1", n)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("create_vm took %s to fail", elapsed)
	}
}

// TestCreateVM_ReadsTheClusterIdentityOffTheOfflineConfiguredNode runs create_vm
// with the configured node offline in the status. The identity read must start
// on the entry node that answered the status and never touch the offline node,
// whose read would run out the whole identity timeout.
func TestCreateVM_ReadsTheClusterIdentityOffTheOfflineConfiguredNode(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var tried []string
	nodes := &vmMockNodes{
		certificatesInfoFn: func(_ context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
			mu.Lock()
			tried = append(tried, node)
			mu.Unlock()
			if node == "lab-pve-cpi-az2-0" {
				return nil, errors.New("network timeout")
			}
			return testCertificatesInfo(), nil
		},
	}
	var reads atomic.Int32
	deps := buildVMDepsPlacement(&vmMockQEMU{}, nodes, listStatusWithoutQuorum(&reads), emptyListResources,
		&vmMockAgent{}, func(cfg *config.CPIConfig) { cfg.Node = "lab-pve-cpi-az2-0" })
	deps.Config.EnsureNoIPConflicts = placementDisabled
	args := mkArgs("agent-offline-config", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("offline-config"))
	if err == nil || !strings.Contains(err.Error(), "the cluster has lost quorum") {
		t.Fatalf("want the quorum error, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tried) != 1 || tried[0] != "lab-pve-cpi-az2-1" {
		t.Fatalf("the identity read tried %v, want only the entry node lab-pve-cpi-az2-1", tried)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("create_vm read the cluster status %d times, want 1", n)
	}
}
