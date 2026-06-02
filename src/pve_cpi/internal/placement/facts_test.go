package placement_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// stub implementations
// ---------------------------------------------------------------------------

type stubCluster struct {
	statusResp *cluster.ListStatusResponse
	statusErr  error
	resResp    *cluster.ListResourcesResponse
	resErr     error
}

func (s *stubCluster) ListStatus(_ context.Context) (*cluster.ListStatusResponse, error) {
	return s.statusResp, s.statusErr
}

func (s *stubCluster) ListResources(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	return s.resResp, s.resErr
}

type stubNodes struct {
	storageFn func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
}

func (s *stubNodes) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	if s.storageFn != nil {
		return s.storageFn(ctx, node, params)
	}
	// Default: active+images with avail=10GiB, total=100GiB.
	storageName := "local-lvm"
	if params != nil && params.Storage != nil {
		storageName = *params.Storage
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName,
		"active":  1,
		"enabled": 1,
		"content": "images,rootdir",
		"avail":   10 * gib,
		"total":   100 * gib,
	})
	resp := nodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func statusNode(name string, maxcpu, maxmem, mem int64, online int, cpuUsed float64) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type":   "node",
		"name":   name,
		"maxcpu": maxcpu,
		"maxmem": maxmem,
		"mem":    mem,
		"online": online,
		"cpu":    cpuUsed,
	})
	return raw
}

func resourceQEMU(node, name, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "qemu",
		"node": node,
		"name": name,
		"tags": tags,
	})
	return raw
}

func nopLogger() *log.Logger { return log.NewNopLogger() }

// ---------------------------------------------------------------------------
// GatherNodeFacts tests
// ---------------------------------------------------------------------------

func TestGatherNodeFacts_ListStatusError(t *testing.T) {
	t.Parallel()
	cl := &stubCluster{statusErr: errors.New("connection refused")}
	ns := &stubNodes{}
	_, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err == nil {
		t.Fatal("expected error from ListStatus failure")
	}
}

func TestGatherNodeFacts_BasicNodeFacts(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 16*gib, 4*gib, 1, 0.3),
	}
	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
	}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{StorageName: "local-lvm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	f := facts[0]
	if f.Node != "pve1" {
		t.Errorf("Node = %q; want pve1", f.Node)
	}
	if !f.Online {
		t.Error("Online should be true")
	}
	if f.TotalMemBytes != 16*gib {
		t.Errorf("TotalMemBytes = %d; want %d", f.TotalMemBytes, 16*gib)
	}
	if f.FreeMemBytes != 12*gib {
		t.Errorf("FreeMemBytes = %d; want %d", f.FreeMemBytes, 12*gib)
	}
	if f.MaxCPU != 8 {
		t.Errorf("MaxCPU = %d; want 8", f.MaxCPU)
	}
	if f.CPUUsed != 0.3 {
		t.Errorf("CPUUsed = %v; want 0.3", f.CPUUsed)
	}
}

func TestGatherNodeFacts_OfflineNodeIncluded(t *testing.T) {
	t.Parallel()
	// GatherNodeFacts returns all nodes; Filter handles offline exclusion.
	resp := cluster.ListStatusResponse{
		statusNode("offline1", 8, 8*gib, 4*gib, 0, 0),
		statusNode("online1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts (offline included); got %d", len(facts))
	}
	offlineSeen := false
	for _, f := range facts {
		if f.Node == "offline1" {
			offlineSeen = true
			if f.Online {
				t.Error("offline1 should have Online=false")
			}
		}
	}
	if !offlineSeen {
		t.Error("offline1 not in facts")
	}
}

func TestGatherNodeFacts_GuestCountFromResources(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
		statusNode("pve2", 8, 8*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMU("pve1", "vm-100", ""),
		resourceQEMU("pve1", "vm-101", ""),
		resourceQEMU("pve2", "vm-200", ""),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range facts {
		switch f.Node {
		case "pve1":
			if f.GuestCount != 2 {
				t.Errorf("pve1 GuestCount = %d; want 2", f.GuestCount)
			}
		case "pve2":
			if f.GuestCount != 1 {
				t.Errorf("pve2 GuestCount = %d; want 1", f.GuestCount)
			}
		}
	}
}

func TestGatherNodeFacts_ListResourcesError_NonFatal(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{
		statusResp: &resp,
		resErr:     errors.New("api timeout"),
	}
	ns := &stubNodes{}

	// Must NOT return an error; must return node with GuestCount=0.
	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("expected no error despite ListResources failure; got: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if facts[0].GuestCount != 0 {
		t.Errorf("GuestCount should be 0 when ListResources fails; got %d", facts[0].GuestCount)
	}
}

func TestGatherNodeFacts_SameGroupCount(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMU("pve1", "director-deploy-web-0", "bosh.director-deploy-web"),
		resourceQEMU("pve1", "director-deploy-web-1", "bosh.director-deploy-web"),
		resourceQEMU("pve1", "director-deploy-db-0", "bosh.director-deploy-db"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		BOSHGroup: "director-deploy-web",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	if facts[0].SameGroupCount != 2 {
		t.Errorf("SameGroupCount = %d; want 2", facts[0].SameGroupCount)
	}
}

func TestGatherNodeFacts_StorageFactsFromNodes(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}

	ns := &stubNodes{
		storageFn: func(_ context.Context, _ string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			storageName := "custom-pool"
			if params != nil && params.Storage != nil {
				storageName = *params.Storage
			}
			raw, _ := json.Marshal(map[string]any{
				"storage": storageName,
				"active":  1,
				"enabled": 1,
				"content": "images",
				"avail":   50 * gib,
				"total":   200 * gib,
			})
			resp := nodes.ListStorageResponse{json.RawMessage(raw)}
			return &resp, nil
		},
	}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		StorageName: "custom-pool",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	if facts[0].FreeStorageBytes != 50*gib {
		t.Errorf("FreeStorageBytes = %d; want %d", facts[0].FreeStorageBytes, 50*gib)
	}
	if facts[0].TotalStorageBytes != 200*gib {
		t.Errorf("TotalStorageBytes = %d; want %d", facts[0].TotalStorageBytes, 200*gib)
	}
}

func TestGatherNodeFacts_StorageListErrorIsNonFatal(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	ns := &stubNodes{
		storageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			return nil, errors.New("ListStorage failed")
		},
	}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		StorageName: "local-lvm",
	})
	if err != nil {
		t.Fatalf("expected no error despite ListStorage failure; got: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	// Storage facts should be zero (storage axis disabled for this node).
	if facts[0].FreeStorageBytes != 0 || facts[0].TotalStorageBytes != 0 {
		t.Errorf("storage bytes should be 0 on ListStorage error; got avail=%d total=%d",
			facts[0].FreeStorageBytes, facts[0].TotalStorageBytes)
	}
}

func TestGatherNodeFacts_NoStorageName_StorageFactsZero(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	// stub that would panic if called; should not be called when StorageName == "".
	ns := &stubNodes{
		storageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			t.Error("ListStorage should NOT be called when StorageName is empty")
			return nil, nil
		},
	}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	if facts[0].FreeStorageBytes != 0 || facts[0].TotalStorageBytes != 0 {
		t.Errorf("storage bytes should be 0 when StorageName not set")
	}
}

func TestGatherNodeFacts_NonNodeStatusEntriesSkipped(t *testing.T) {
	t.Parallel()
	// PVE /cluster/status also returns quorum-info and other entries.
	quorumRaw, _ := json.Marshal(map[string]any{"type": "quorum", "name": "q1"})
	resp := cluster.ListStatusResponse{
		json.RawMessage(quorumRaw),
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the "node" type entry should produce a NodeFacts.
	if len(facts) != 1 || facts[0].Node != "pve1" {
		t.Errorf("expected only pve1 in facts; got %v", facts)
	}
}

func TestGatherNodeFacts_MultipleNodes(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 16*gib, 4*gib, 1, 0.2),
		statusNode("pve2", 16, 32*gib, 8*gib, 1, 0.5),
		statusNode("pve3", 4, 8*gib, 8*gib, 0, 0.9), // offline
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("expected 3 facts; got %d", len(facts))
	}
	// Verify pve3 is marked offline.
	for _, f := range facts {
		if f.Node == "pve3" && f.Online {
			t.Error("pve3 should be offline")
		}
	}
}

// ---------------------------------------------------------------------------
// ParseNodeResources tests
// ---------------------------------------------------------------------------

func TestParseNodeResources_EmptySlice(t *testing.T) {
	t.Parallel()
	result, err := placement.ParseNodeResources(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result; got %d", len(result))
	}
}

func TestParseNodeResources_SkipsMalformed(t *testing.T) {
	t.Parallel()
	raw := []json.RawMessage{
		json.RawMessage(`{invalid}`),
		json.RawMessage(`{"type":"qemu","node":"pve1","name":"vm-100"}`),
	}
	result, err := placement.ParseNodeResources(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Malformed entry skipped; valid entry decoded.
	if len(result) != 1 {
		t.Errorf("expected 1 result (malformed skipped); got %d", len(result))
	}
}
