// Handler-level tests: delete_vm invokes advertised-route subnet cleanup on
// both the synchronous and fast-path delete flows.
package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// advrtTagFor recomputes the provenance tag contract from the outside:
// "advrt-<vnet>-<first 8 hex of FNV-1a-64 over vnet/cidr>". Deliberately
// re-implemented here (not shared with production code) so a format drift
// breaks this test.
func advrtTagFor(vnet, cidr string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(vnet + "/" + cidr))
	return "advrt-" + vnet + "-" + fmt.Sprintf("%016x", h.Sum64())[:8]
}

// routesClusterSvc builds a mockClusterSvc that places vmid on pve-node1
// carrying the given tags, and wires the SDN subnet surface for cleanup.
func routesClusterSvc(vmid int, tags string, subnets map[string][]json.RawMessage, deleted *[]string, applies *int) *mockClusterSvc {
	svc := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			row, _ := json.Marshal(map[string]any{
				"vmid": vmid, "node": "pve-node1", "type": "qemu", "tags": tags,
			})
			resp := cluster.ListResourcesResponse{row}
			return &resp, nil
		},
	}
	svc.listSdnVnetsSubnetsFn = func(_ context.Context, vnet string, _ *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
		resp := cluster.ListSdnVnetsSubnetsResponse(subnets[vnet])
		return &resp, nil
	}
	svc.deleteSdnVnetsSubnetsFn = func(_ context.Context, vnet string, subnet string, _ *cluster.DeleteSdnVnetsSubnetsParams) error {
		*deleted = append(*deleted, vnet+"/"+subnet)
		return nil
	}
	svc.updateSdnFn = func(_ context.Context, _ *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
		*applies++
		return nil, nil
	}
	return svc
}

func routesSubnetRow(id, cidr string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"subnet": id, "cidr": cidr, "type": "subnet"})
	return raw
}

func TestHandleDeleteVM_SyncPath_CleansAdvertisedRoutes(t *testing.T) {
	t.Parallel()
	const vmid = 321
	tag := advrtTagFor("vnet1", "10.64.0.0/16")

	var deleted []string
	var applies int
	clusterSvc := routesClusterSvc(vmid, "bosh-cpi;"+tag, map[string][]json.RawMessage{
		"vnet1": {routesSubnetRow("z1-10.64.0.0-16", "10.64.0.0/16")},
	}, &deleted, &applies)

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:pve-node1:stop", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}

	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, tasksSvc, &mockAgentService{})
	deps.PVE = &mockPVEClient{
		qemuSvc:    qemuSvc,
		nodesSvc:   nodesSvc,
		tasksSvc:   tasksSvc,
		storageSvc: &mockStorageService{},
		clusterSvc: clusterSvc,
		poolsSvc:   &noopPoolService{},
	}

	h := handlers.HandleDeleteVM(deps)
	if _, err := h.Handle(context.Background(), marshalArgs("321"), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "vnet1/z1-10.64.0.0-16" {
		t.Errorf("deleted = %v, want the advertised-route subnet", deleted)
	}
	if applies != 1 {
		t.Errorf("apply calls = %d, want 1", applies)
	}
}

func TestHandleDeleteVM_FastPath_CleansAdvertisedRoutes(t *testing.T) {
	t.Parallel()
	const vmid = 322
	tag := advrtTagFor("vnet1", "10.64.0.0/16")

	var deleted []string
	var applies int
	clusterSvc := routesClusterSvc(vmid, "bosh-cpi;"+tag, map[string][]json.RawMessage{
		"vnet1": {routesSubnetRow("z1-10.64.0.0-16", "10.64.0.0/16")},
	}, &deleted, &applies)

	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "UPID:pve-node1:stop", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil // bosh-deleting tag stamp
		},
	}

	deps := testDepsFoundVM(vmid, qemuSvc, nodesSvc, &mockTasksService{}, &mockAgentService{})
	enabled := true
	deps.Config.FastPathDelete = &enabled
	deps.PVE = &mockPVEClient{
		qemuSvc:    qemuSvc,
		nodesSvc:   nodesSvc,
		tasksSvc:   &mockTasksService{},
		storageSvc: &mockStorageService{},
		clusterSvc: clusterSvc,
		poolsSvc:   &noopPoolService{},
	}

	h := handlers.HandleDeleteVM(deps)
	if _, err := h.Handle(context.Background(), marshalArgs("322"), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "vnet1/z1-10.64.0.0-16" {
		t.Errorf("fast path deleted = %v, want the advertised-route subnet", deleted)
	}
	if applies != 1 {
		t.Errorf("fast path apply calls = %d, want 1", applies)
	}
}
