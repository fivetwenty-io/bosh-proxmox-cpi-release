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
	statusResp     *cluster.ListStatusResponse
	statusErr      error
	resResp        *cluster.ListResourcesResponse
	resErr         error
	haStatusFn     func() (*cluster.ListHaStatusCurrentResponse, error)
	haStatusCalled bool
}

func (s *stubCluster) ListStatus(_ context.Context) (*cluster.ListStatusResponse, error) {
	return s.statusResp, s.statusErr
}

func (s *stubCluster) ListResources(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	return s.resResp, s.resErr
}

func (s *stubCluster) ListHaStatusCurrent(_ context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	s.haStatusCalled = true
	if s.haStatusFn != nil {
		return s.haStatusFn()
	}
	// Default: empty response (no nodes in maintenance).
	empty := cluster.ListHaStatusCurrentResponse{}
	return &empty, nil
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

// resourceQEMUWithMem builds a qemu cluster/resources entry carrying an
// explicit maxmem (bytes) and an optional status (e.g. "stopped"). status is
// omitted from the JSON entirely when empty; it is included only to
// demonstrate that GatherNodeFacts sums maxmem regardless of the guest's run
// state — the code has no field for status and does not filter on it.
func resourceQEMUWithMem(node, name, tags string, maxmem int64, status string) json.RawMessage {
	m := map[string]any{
		"type":   "qemu",
		"node":   node,
		"name":   name,
		"tags":   tags,
		"maxmem": maxmem,
	}
	if status != "" {
		m["status"] = status
	}
	raw, _ := json.Marshal(m)
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

func TestGatherNodeFacts_CommittedMemBytes_SumsGuestMaxmemIncludingStopped(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 32*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMUWithMem("pve1", "vm-100", "", 4*gib, "running"),
		// Stopped guests still reserve their configured RAM once BOSH starts
		// them, so a stopped guest's maxmem must count toward the node's
		// committed memory exactly like a running guest's.
		resourceQEMUWithMem("pve1", "vm-101", "", 8*gib, "stopped"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if want := 12 * gib; facts[0].CommittedMemBytes != want {
		t.Errorf("CommittedMemBytes = %d; want %d (stopped guest must count)", facts[0].CommittedMemBytes, want)
	}
}

func TestGatherNodeFacts_CommittedMemBytes_SummedPerNode(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 32*gib, 4*gib, 1, 0),
		statusNode("pve2", 8, 32*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMUWithMem("pve1", "vm-100", "", 2*gib, ""),
		resourceQEMUWithMem("pve1", "vm-101", "", 3*gib, ""),
		resourceQEMUWithMem("pve2", "vm-200", "", 10*gib, ""),
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
			if want := 5 * gib; f.CommittedMemBytes != want {
				t.Errorf("pve1 CommittedMemBytes = %d; want %d", f.CommittedMemBytes, want)
			}
		case "pve2":
			if want := 10 * gib; f.CommittedMemBytes != want {
				t.Errorf("pve2 CommittedMemBytes = %d; want %d", f.CommittedMemBytes, want)
			}
		}
	}
}

func TestGatherNodeFacts_CommittedMemBytes_MissingMaxmemToleratedAsZero(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 32*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		// resourceQEMU (pre-existing helper) omits maxmem entirely — decodes
		// to the Go zero value (0) and must not abort or skip the guest.
		resourceQEMU("pve1", "vm-100", ""),
		resourceQEMUWithMem("pve1", "vm-101", "", 6*gib, ""),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if want := 6 * gib; facts[0].CommittedMemBytes != want {
		t.Errorf("CommittedMemBytes = %d; want %d (missing maxmem must count as 0, not break)",
			facts[0].CommittedMemBytes, want)
	}
	if facts[0].GuestCount != 2 {
		t.Errorf("GuestCount = %d; want 2 (guest with missing maxmem still counts toward GuestCount)",
			facts[0].GuestCount)
	}
}

func TestGatherNodeFacts_CommittedMemBytes_ListResourcesError_ZeroFailOpen(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 32*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resErr: errors.New("api timeout")}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("expected no error despite ListResources failure; got: %v", err)
	}
	if facts[0].CommittedMemBytes != 0 {
		t.Errorf("CommittedMemBytes should be 0 when ListResources fails; got %d", facts[0].CommittedMemBytes)
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
		resourceQEMU("pve1", "web-0", "deployment--cf;job--web;index--0"),
		resourceQEMU("pve1", "web-1", "deployment--cf;job--web;index--1"),
		resourceQEMU("pve1", "db-0", "deployment--cf;job--db;index--0"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		GroupTag: "job--web",
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

// ---------------------------------------------------------------------------
// Maintenance node exclusion tests
// ---------------------------------------------------------------------------

func TestGatherNodeFacts_HAMaintenanceNode(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
		statusNode("pve2", 8, 8*gib, 4*gib, 1, 0),
	}
	// pve1 is in HA maintenance state.
	haResp := make(cluster.ListHaStatusCurrentResponse, 0, 1)
	raw, _ := json.Marshal(map[string]any{"type": "manager_status", "node": "pve1", "state": "maintenance"})
	haResp = append(haResp, json.RawMessage(raw))

	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) {
			return &haResp, nil
		},
	}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		ExcludeMaintenanceNodes: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts; got %d", len(facts))
	}
	for _, f := range facts {
		switch f.Node {
		case "pve1":
			if !f.InMaintenance {
				t.Error("pve1 should be InMaintenance=true (HA state)")
			}
		case "pve2":
			if f.InMaintenance {
				t.Error("pve2 should be InMaintenance=false")
			}
		}
	}
}

func TestGatherNodeFacts_MaintenanceViaOperatorTag(t *testing.T) {
	t.Parallel()
	// pve1 carries a "maintenance" operator tag in cluster status.
	statusRaw, _ := json.Marshal(map[string]any{
		"type":   "node",
		"name":   "pve1",
		"maxcpu": 8,
		"maxmem": 8 * gib,
		"mem":    4 * gib,
		"online": 1,
		"cpu":    0.0,
		"tags":   "maintenance",
	})
	resp := cluster.ListStatusResponse{json.RawMessage(statusRaw)}

	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
	}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		ExcludeMaintenanceNodes: true,
		MaintenanceNodeTags:     []string{"maintenance"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if !facts[0].InMaintenance {
		t.Error("pve1 should be InMaintenance=true (operator tag)")
	}
}

func TestGatherNodeFacts_HAStatusError_FailOpen(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) {
			return nil, errors.New("HA API unavailable")
		},
	}
	ns := &stubNodes{}

	// Must NOT return error; node must NOT be marked in maintenance (fail-open).
	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		ExcludeMaintenanceNodes: true,
	})
	if err != nil {
		t.Fatalf("expected no error on HA fetch failure; got: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if facts[0].InMaintenance {
		t.Error("InMaintenance should be false when HA API errors (fail-open)")
	}
}

func TestGatherNodeFacts_ExcludeDisabled_HANotCalled(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) {
			// Should not be called when ExcludeMaintenanceNodes is false.
			return nil, errors.New("should not be called")
		},
	}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		ExcludeMaintenanceNodes: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact; got %d", len(facts))
	}
	if cl.haStatusCalled {
		t.Error("ListHaStatusCurrent should NOT be called when ExcludeMaintenanceNodes=false")
	}
	if facts[0].InMaintenance {
		t.Error("InMaintenance should be false when exclusion is disabled")
	}
}

func TestGatherNodeFacts_MaintenanceUnionHAAndTag(t *testing.T) {
	t.Parallel()
	// pve1 has HA maintenance; pve2 has operator tag; pve3 is clean.
	statusRaw1 := statusNode("pve1", 8, 8*gib, 4*gib, 1, 0)
	taggedRaw, _ := json.Marshal(map[string]any{
		"type": "node", "name": "pve2", "maxcpu": 8, "maxmem": 8 * gib,
		"mem": 4 * gib, "online": 1, "cpu": 0.0, "tags": "maintenance;other",
	})
	statusRaw3 := statusNode("pve3", 8, 8*gib, 4*gib, 1, 0)
	resp := cluster.ListStatusResponse{
		statusRaw1, json.RawMessage(taggedRaw), statusRaw3,
	}

	haResp := make(cluster.ListHaStatusCurrentResponse, 0, 1)
	raw, _ := json.Marshal(map[string]any{"type": "manager_status", "node": "pve1", "state": "maintenance"})
	haResp = append(haResp, json.RawMessage(raw))

	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) { return &haResp, nil },
	}
	ns := &stubNodes{}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		ExcludeMaintenanceNodes: true,
		MaintenanceNodeTags:     []string{"maintenance"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("expected 3 facts; got %d", len(facts))
	}
	maintenance := map[string]bool{}
	for _, f := range facts {
		maintenance[f.Node] = f.InMaintenance
	}
	if !maintenance["pve1"] {
		t.Error("pve1: expected InMaintenance=true (HA state)")
	}
	if !maintenance["pve2"] {
		t.Error("pve2: expected InMaintenance=true (operator tag)")
	}
	if maintenance["pve3"] {
		t.Error("pve3: expected InMaintenance=false")
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
