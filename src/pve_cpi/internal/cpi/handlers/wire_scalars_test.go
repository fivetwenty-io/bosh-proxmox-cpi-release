package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestHandleCalculateVMCloudProperties_QuotedCountersAndBooleanFlags feeds
// the node and storage decoders the scalar shapes PVE has been seen to emit
// (a JSON boolean for online, quoted integers for the counters, a quoted
// flag for active). Plain int64 fields rejected these rows and the loop
// skipped them, so a perfectly good node was reported as having no fit.
func TestHandleCalculateVMCloudProperties_QuotedCountersAndBooleanFlags(t *testing.T) {
	t.Parallel()

	resp := cluster.ListStatusResponse{
		json.RawMessage(`{"type":"node","name":"` + testNode + `","online":true,"maxcpu":"8","maxmem":"8589934592","mem":"2147483648"}`),
	}
	svc := &mockClusterService{statusResp: &resp}
	nodesSvc := &mockNodesService{
		listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			r := nodes.ListStorageResponse{
				json.RawMessage(`{"storage":"` + storageName + `","type":"lvmthin","active":"1","enabled":1,"content":"images,rootdir","avail":"10737418240","total":"107374182400"}`),
			}
			return &r, nil
		},
	}
	deps := makeCalcDepsWithNodes(svc, nodesSvc)
	h := handlers.HandleCalculateVMCloudProperties(deps)

	result, err := h.Handle(context.Background(), makeCalcArgs(2, 2048, 10240), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := decodeCloudProps(t, result)
	var targetNode string
	if e := json.Unmarshal(m["target_node"], &targetNode); e != nil || targetNode != testNode {
		t.Errorf("target_node = %q; want %q", targetNode, testNode)
	}
}
