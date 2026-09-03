package placement_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestGatherNodeFacts_QuotedCountersAndBooleanOnline feeds the /cluster/status,
// /cluster/resources, and /nodes/{node}/storage decoders the scalar shapes PVE
// has been seen to emit (quoted integers, a JSON boolean for online, a quoted
// float for cpu) instead of the spec's integers. Plain int64/float64 fields
// rejected these rows, and the loops skipped a rejected row, so the node fell
// out of placement with only a Debug line to show for it.
func TestGatherNodeFacts_QuotedCountersAndBooleanOnline(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		json.RawMessage(`{"type":"node","name":"pve1","online":true,"maxcpu":"8","maxmem":"17179869184","mem":"4294967296","cpu":"0.25"}`),
	}
	res := cluster.ListResourcesResponse{
		json.RawMessage(`{"type":"qemu","node":"pve1","name":"vm-a","maxmem":"2147483648"}`),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &res}
	ns := &stubNodes{
		storageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			r := nodes.ListStorageResponse{
				json.RawMessage(`{"storage":"local-lvm","active":"1","enabled":true,"content":"images,rootdir","avail":"10737418240","total":"107374182400"}`),
			}
			return &r, nil
		},
	}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{StorageName: "local-lvm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	f := facts[0]
	if !f.Online {
		t.Error("Online should be true for a JSON-boolean online flag")
	}
	if f.TotalMemBytes != 16*gib || f.FreeMemBytes != 12*gib {
		t.Errorf("mem = total %d free %d; want %d / %d", f.TotalMemBytes, f.FreeMemBytes, 16*gib, 12*gib)
	}
	if f.MaxCPU != 8 || f.CPUUsed != 0.25 {
		t.Errorf("cpu = max %d used %v; want 8 / 0.25", f.MaxCPU, f.CPUUsed)
	}
	if f.CommittedMemBytes != 2*gib {
		t.Errorf("CommittedMemBytes = %d; want %d", f.CommittedMemBytes, 2*gib)
	}
	if f.FreeStorageBytes != 10*gib || f.TotalStorageBytes != 100*gib {
		t.Errorf("storage = free %d total %d; want %d / %d", f.FreeStorageBytes, f.TotalStorageBytes, 10*gib, 100*gib)
	}
}

// PVEFloat parses any string strconv accepts, which includes "nan" and "inf".
// A non-finite cpu figure must not reach the scorer, where it would poison
// every comparison, so the gatherer reads it as unknown load.
func TestGatherNodeFacts_NonFiniteCPUReadsAsZero(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		json.RawMessage(`{"type":"node","name":"pve1","online":1,"maxcpu":8,"maxmem":17179869184,"mem":4294967296,"cpu":"nan"}`),
		json.RawMessage(`{"type":"node","name":"pve2","online":1,"maxcpu":8,"maxmem":17179869184,"mem":4294967296,"cpu":"inf"}`),
	}
	res := cluster.ListResourcesResponse{}
	cl := &stubCluster{statusResp: &resp, resResp: &res}
	ns := &stubNodes{
		storageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			r := nodes.ListStorageResponse{}
			return &r, nil
		},
	}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{StorageName: "local-lvm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts; got %d", len(facts))
	}
	for _, f := range facts {
		if f.CPUUsed != 0 {
			t.Errorf("node %s: CPUUsed = %v, want 0 for a non-finite wire value", f.Node, f.CPUUsed)
		}
	}
}
