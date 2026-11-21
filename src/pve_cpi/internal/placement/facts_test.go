package placement_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
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
	managerFn      func() (*cluster.ListHaStatusManagerStatusResponse, error)
	managerCalled  bool
	// statusFails lists the 1-based status reads that fail.
	statusFails map[int32]bool
	statusReads atomic.Int32
}

func (s *stubCluster) ListStatus(_ context.Context) (*cluster.ListStatusResponse, error) {
	if s.statusFails[s.statusReads.Add(1)] {
		return nil, errors.New("cluster status timed out")
	}
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

func (s *stubCluster) ListHaStatusManagerStatus(_ context.Context) (*cluster.ListHaStatusManagerStatusResponse, error) {
	s.managerCalled = true
	if s.managerFn != nil {
		return s.managerFn()
	}
	// Default: empty response (no HA view).
	empty := cluster.ListHaStatusManagerStatusResponse{}
	return &empty, nil
}

func (s *stubCluster) ListHaResources(
	_ context.Context, _ *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	empty := cluster.ListHaResourcesResponse{}
	return &empty, nil
}

type stubNodes struct {
	storageFn func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
	now       int64
	// notes are the node notes each node's config answers with.
	notes map[string]string
	// notesFails lists the 1-based node config reads that fail.
	notesFails map[int32]bool
	notesReads atomic.Int32
}

func (s *stubNodes) ListConfig(
	_ context.Context, node string, _ *nodes.ListConfigParams,
) (*nodes.ListConfigResponse, error) {
	if s.notesFails[s.notesReads.Add(1)] {
		return nil, errors.New("node config unavailable")
	}
	desc := s.notes[node]
	return &nodes.ListConfigResponse{Description: &desc}, nil
}

// markedNodes answers pve1's notes with the maintenance marker and fails the
// node config reads in fails.
func markedNodes(fails ...int32) *stubNodes {
	ns := &stubNodes{notes: map[string]string{"pve1": "Rack 4\ntags: maintenance"}, notesFails: map[int32]bool{}}
	for _, n := range fails {
		ns.notesFails[n] = true
	}
	return ns
}

func (s *stubNodes) ListTime(_ context.Context, _ string) (*nodes.ListTimeResponse, error) {
	var resp nodes.ListTimeResponse
	raw, _ := json.Marshal(map[string]any{"time": s.now, "localtime": s.now, "timezone": "UTC"})
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
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

// localStatusNode is statusNode with the local flag the entry node carries.
func localStatusNode() json.RawMessage {
	const name = "pve1"
	raw, _ := json.Marshal(map[string]any{
		"type": "node", "name": name, "maxcpu": 8, "maxmem": 8 * gib, "mem": 4 * gib,
		"online": 1, "local": 1, "cpu": 0.0,
	})
	return raw
}

// quorateRow is the cluster row of GET /cluster/status.
func quorateRow(quorate int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "cluster", "name": "lab", "quorate": quorate, "nodes": 3})
	return raw
}

// managerStatusJSON builds the body of GET /cluster/ha/status/manager_status.
func managerStatusJSON(now int64, nodeStatus map[string]string, lrm map[string]map[string]any) *cluster.ListHaStatusManagerStatusResponse {
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{"node_status": nodeStatus, "timestamp": now},
		"lrm_status":     lrm,
		"quorum":         map[string]any{"node": "pve1", "quorate": "1"},
	})
	resp := cluster.ListHaStatusManagerStatusResponse(raw)
	return &resp
}

func freshLRM() map[string]any {
	return map[string]any{"mode": "active", "state": "active", "timestamp": haTestNow}
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
func resourceQEMUWithMem(node, name string, maxmem int64, status string) json.RawMessage {
	m := map[string]any{
		"type":   "qemu",
		"node":   node,
		"name":   name,
		"tags":   "",
		"maxmem": maxmem,
	}
	if status != "" {
		m["status"] = status
	}
	raw, _ := json.Marshal(m)
	return raw
}

// haTestNow is the entry node clock in the node health tests.
const haTestNow = 1_800_000_000

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
		resourceQEMUWithMem("pve1", "vm-100", 4*gib, "running"),
		// Stopped guests still reserve their configured RAM once BOSH starts
		// them, so a stopped guest's maxmem must count toward the node's
		// committed memory exactly like a running guest's.
		resourceQEMUWithMem("pve1", "vm-101", 8*gib, "stopped"),
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
		resourceQEMUWithMem("pve1", "vm-100", 2*gib, ""),
		resourceQEMUWithMem("pve1", "vm-101", 3*gib, ""),
		resourceQEMUWithMem("pve2", "vm-200", 10*gib, ""),
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
		resourceQEMUWithMem("pve1", "vm-101", 6*gib, ""),
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

// TestGatherNodeFacts_SameGroupVMIDs checks that the facts name the VMIDs of
// the members they count, which is how a create's spread leaves out the claim
// of a sibling whose tag already counts.
func TestGatherNodeFacts_SameGroupVMIDs(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{statusNode("pve1", 8, 8*gib, 4*gib, 1, 0)}
	member := func(vmid int, name, tags string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"type": "qemu", "node": "pve1", "vmid": vmid, "name": name, "tags": tags})
		return raw
	}
	resResp := cluster.ListResourcesResponse{
		member(101, "web-0", "deployment--cf;job--web;index--0"),
		member(102, "db-0", "deployment--cf;job--db;index--0"),
		member(103, "web-1", "deployment--cf;job--web;index--1"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{}, nopLogger(),
		placement.GatherOptions{GroupTag: "job--web"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	if got := facts[0].SameGroupVMIDs; !slices.Equal(got, []int{101, 103}) {
		t.Errorf("SameGroupVMIDs = %v; want [101 103]", got)
	}
	if facts[0].SameGroupCount != len(facts[0].SameGroupVMIDs) {
		t.Errorf("SameGroupCount = %d; want one per member VMID", facts[0].SameGroupCount)
	}
}

// TestGatherNodeFacts_SameGroupCountLegacyTags covers a group whose older
// members carry only the tags from before the member tag existed. A guest
// counts when it carries the member tag or every legacy tag, and counts once
// when it carries both.
func TestGatherNodeFacts_SameGroupCountLegacyTags(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMU("pve1", "web-0", "bosh-aa-0123456789ab;deployment--cf;job--web"),
		resourceQEMU("pve1", "web-1", "deployment--cf;job--web;bosh-owner--dir-1"),
		resourceQEMU("pve1", "web-2", "bosh-aa-0123456789ab"),
		resourceQEMU("pve1", "web-3", "deployment--cf;job--web;bosh-owner--dir-2"),
		resourceQEMU("pve1", "web-4", "deployment--other;job--web;bosh-owner--dir-1"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{}, nopLogger(), placement.GatherOptions{
		GroupTag:        "bosh-aa-0123456789ab",
		LegacyGroupTags: []string{"job--web", "deployment--cf", "bosh-owner--dir-1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact")
	}
	if facts[0].SameGroupCount != 3 {
		t.Errorf("SameGroupCount = %d; want 3 (web-0, web-1, web-2)", facts[0].SameGroupCount)
	}
}

// TestGatherNodeFacts_SameGroupCountLegacyTagsOnly covers a request with no
// member tag, which still spreads on the legacy tags alone.
func TestGatherNodeFacts_SameGroupCountLegacyTagsOnly(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		statusNode("pve1", 8, 8*gib, 4*gib, 1, 0),
	}
	resResp := cluster.ListResourcesResponse{
		resourceQEMU("pve1", "web-0", "deployment--cf;job--web"),
		resourceQEMU("pve1", "db-0", "deployment--cf;job--db"),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &resResp}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{}, nopLogger(), placement.GatherOptions{
		LegacyGroupTags: []string{"job--web", "deployment--cf"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts[0].SameGroupCount != 1 {
		t.Errorf("SameGroupCount = %d; want 1", facts[0].SameGroupCount)
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
// Maintenance node exclusion tests
// ---------------------------------------------------------------------------

func TestGatherNodeFacts_HAMaintenanceNode(t *testing.T) {
	t.Parallel()
	const now = haTestNow
	resp := cluster.ListStatusResponse{
		quorateRow(1),
		localStatusNode(),
		statusNode("pve2", 8, 8*gib, 4*gib, 1, 0),
	}
	// pve1 is healthy; pve2 is in HA maintenance.
	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		managerFn: func() (*cluster.ListHaStatusManagerStatusResponse, error) {
			return managerStatusJSON(now,
				map[string]string{"pve1": "online", "pve2": "maintenance"},
				map[string]map[string]any{
					"pve1": freshLRM(),
					"pve2": {"mode": "maintenance", "state": "maintenance", "timestamp": now},
				}), nil
		},
	}
	ns := &stubNodes{now: now}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts; got %d", len(facts))
	}
	for _, f := range facts {
		switch f.Node {
		case "pve1":
			if f.InMaintenance {
				t.Error("pve1 should be InMaintenance=false")
			}
		case "pve2":
			if !f.InMaintenance {
				t.Error("pve2 should be InMaintenance=true (HA maintenance)")
			}
		}
	}
}

// One errored VM must not mark its healthy node. The old check read the state
// of every status entry and condemned the node for it.
func TestGatherNodeFacts_ErroredServiceDoesNotMarkNode(t *testing.T) {
	t.Parallel()
	const now = haTestNow
	resp := cluster.ListStatusResponse{quorateRow(1), localStatusNode()}
	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		managerFn: func() (*cluster.ListHaStatusManagerStatusResponse, error) {
			return managerStatusJSON(now, map[string]string{"pve1": "online"},
				map[string]map[string]any{"pve1": freshLRM()}), nil
		},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) {
			raw, _ := json.Marshal(map[string]any{
				"type": "service", "id": "vm:100", "node": "pve1", "state": "error",
			})
			return &cluster.ListHaStatusCurrentResponse{json.RawMessage(raw)}, nil
		},
	}
	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{now: now}, nopLogger(),
		placement.GatherOptions{})
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v err=%v", facts, err)
	}
	if facts[0].InMaintenance {
		t.Error("an errored service must not mark its node")
	}
}

func TestGatherNodeFacts_NotQuorateEntryExcludesEveryNode(t *testing.T) {
	t.Parallel()
	resp := cluster.ListStatusResponse{
		quorateRow(0),
		localStatusNode(),
		statusNode("pve2", 8, 8*gib, 4*gib, 1, 0),
	}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}}
	facts, err := placement.GatherNodeFacts(context.Background(), cl, &stubNodes{now: 1}, nopLogger(),
		placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, f := range facts {
		if !f.InMaintenance {
			t.Errorf("%s: a non-quorate entry node must leave no node placeable", f.Node)
		}
	}
	if cl.managerCalled {
		t.Error("no HA source is read through a non-quorate entry node")
	}
}

func TestGatherNodeFacts_MaintenanceViaOperatorTag(t *testing.T) {
	t.Parallel()
	// pve1's notes carry the "tags: maintenance" marker line.
	statusRaw, _ := json.Marshal(map[string]any{
		"type":   "node",
		"name":   "pve1",
		"maxcpu": 8,
		"maxmem": 8 * gib,
		"mem":    4 * gib,
		"online": 1,
		"cpu":    0.0,
	})
	resp := cluster.ListStatusResponse{json.RawMessage(statusRaw)}

	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
	}
	ns := markedNodes()

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		MaintenanceNodeTags: []string{"maintenance"},
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
		managerFn: func() (*cluster.ListHaStatusManagerStatusResponse, error) {
			return nil, errors.New("HA API unavailable")
		},
		haStatusFn: func() (*cluster.ListHaStatusCurrentResponse, error) {
			return nil, errors.New("HA API unavailable")
		},
	}
	ns := &stubNodes{}

	// Must NOT return error; node must NOT be marked in maintenance (fail-open).
	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{})
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

func TestGatherNodeFacts_MaintenanceUnionHAAndTag(t *testing.T) {
	t.Parallel()
	const now = haTestNow
	// pve1 has HA maintenance; pve2's notes carry the marker; pve3 is clean.
	taggedRaw, _ := json.Marshal(map[string]any{
		"type": "node", "name": "pve2", "maxcpu": 8, "maxmem": 8 * gib,
		"mem": 4 * gib, "online": 1, "cpu": 0.0,
	})
	resp := cluster.ListStatusResponse{
		quorateRow(1),
		localStatusNode(),
		json.RawMessage(taggedRaw),
		statusNode("pve3", 8, 8*gib, 4*gib, 1, 0),
	}

	cl := &stubCluster{
		statusResp: &resp,
		resResp:    &cluster.ListResourcesResponse{},
		managerFn: func() (*cluster.ListHaStatusManagerStatusResponse, error) {
			return managerStatusJSON(now,
				map[string]string{"pve1": "maintenance", "pve2": "online", "pve3": "online"},
				map[string]map[string]any{
					"pve1": {"mode": "maintenance", "state": "maintenance", "timestamp": now},
					"pve2": freshLRM(),
					"pve3": freshLRM(),
				}), nil
		},
	}
	ns := &stubNodes{now: now, notes: map[string]string{"pve2": "tags: maintenance;other"}}

	facts, err := placement.GatherNodeFacts(context.Background(), cl, ns, nopLogger(), placement.GatherOptions{
		MaintenanceNodeTags: []string{"maintenance"},
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
	_, rejections := placement.Filter(facts, placement.Request{})
	if got := rejections["pve1"]; got != "node in maintenance" {
		t.Errorf("pve1: rejection = %q, want %q", got, "node in maintenance")
	}
	want := `node in maintenance (its notes carry the maintenance tag "maintenance")`
	if got := rejections["pve2"]; got != want {
		t.Errorf("pve2: rejection = %q, want %q", got, want)
	}
	for _, n := range []string{"pve1", "pve2"} {
		if !placement.IsMaintenanceRejection(rejections[n]) {
			t.Errorf("%s: rejection %q is not recognized as a maintenance rejection", n, rejections[n])
		}
	}
}

// Real PVE /cluster/status reports membership only; compute counters are
// supplied separately by /cluster/resources. Both static and managed placement
// use these facts for CPU and free-memory admission.
func TestGatherNodeFacts_RealMembershipAndNodeResources(t *testing.T) {
	statuses := cluster.ListStatusResponse{
		quorateRow(1),
		json.RawMessage(`{"type":"node","name":"pve1","online":1}`),
		json.RawMessage(`{"type":"node","name":"pve2","online":0}`),
		json.RawMessage(`{"type":"node","name":"pve3","online":1}`),
	}
	resources := cluster.ListResourcesResponse{
		json.RawMessage(`{"type":"node","node":"pve1","maxcpu":"8","maxmem":"8589934592","mem":"2147483648","cpu":"0.125"}`),
		json.RawMessage(`{"type":"node","node":"pve2","maxcpu":8,"maxmem":8589934592,"mem":0,"status":"online"}`),
		json.RawMessage(`{"type":"node","node":"not-a-member","maxcpu":64,"maxmem":8589934592,"mem":0}`),
		resourceQEMUWithMem("pve1", "stopped", 1*gib, "stopped"),
	}
	cl := &stubCluster{statusResp: &statuses, resResp: &resources}
	facts, err := placement.GatherNodeFacts(t.Context(), cl, &stubNodes{}, nopLogger(), placement.GatherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("membership changed: %+v", facts)
	}
	first := facts[0]
	if first.MaxCPU != 8 || first.TotalMemBytes != 8*gib || first.FreeMemBytes != 6*gib || first.CPUUsed != 0.125 || first.CommittedMemBytes != gib || first.GuestCount != 1 {
		t.Fatalf("actual node and guest facts not merged: %+v", first)
	}
	pass, rejected := placement.Filter(facts, placement.Request{RequiredCPU: 1, RequiredMemBytes: 2 * gib})
	if len(pass) != 1 || pass[0].Node != "pve1" || rejected["pve2"] != "node offline" || rejected["pve3"] != "insufficient CPU" {
		t.Fatalf("admission did not preserve membership and missing-facts constraints: pass=%+v rejected=%v", pass, rejected)
	}
	// An old status counter cannot override a current resource observation.
	statuses[1] = statusNode("pve1", 1, 1*gib, 1*gib, 1, 0.9)
	facts, err = placement.GatherNodeFacts(t.Context(), cl, &stubNodes{}, nopLogger(), placement.GatherOptions{})
	if err != nil || facts[0].MaxCPU != 8 || facts[0].FreeMemBytes != 6*gib {
		t.Fatalf("resource observation was not authoritative: %+v %v", facts, err)
	}
}

// taggedMaintenanceCluster returns a one-node cluster, with the status reads
// in fails failing. The first read is the gather's own, and the ones after it
// are the node health reads. markedNodes gives its node the maintenance marker.
func taggedMaintenanceCluster(fails ...int32) *stubCluster {
	statusRaw, _ := json.Marshal(map[string]any{
		"type": "node", "name": "pve1", "maxcpu": 8, "maxmem": 8 * gib, "mem": 4 * gib, "online": 1, "cpu": 0.0,
	})
	resp := cluster.ListStatusResponse{json.RawMessage(statusRaw)}
	cl := &stubCluster{statusResp: &resp, resResp: &cluster.ListResourcesResponse{}, statusFails: map[int32]bool{}}
	for _, n := range fails {
		cl.statusFails[n] = true
	}
	return cl
}

// TestGatherNodeFacts_RetriesAFailedHealthReadOnce pins that one failed node
// health read is read again, so a single timeout does not hide a node in
// maintenance from placement.
func TestGatherNodeFacts_RetriesAFailedHealthReadOnce(t *testing.T) {
	t.Parallel()
	for _, require := range []bool{false, true} {
		cl := taggedMaintenanceCluster(2)
		facts, err := placement.GatherNodeFacts(context.Background(), cl, markedNodes(), nopLogger(),
			placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}, RequireNodeHealth: require})
		if err != nil {
			t.Fatalf("require %v: unexpected error: %v", require, err)
		}
		if len(facts) != 1 || !facts[0].InMaintenance {
			t.Fatalf("require %v: facts = %+v; want pve1 in maintenance from the second read", require, facts)
		}
	}
}

// TestGatherNodeFacts_FailsWhenTheHealthReadFailsTwiceAndHealthIsRequired
// pins that a gather that needs node health, because the CPI writes HA state
// or the operator tags nodes for maintenance, fails with
// ErrNodeHealthUnavailable when the read fails twice, instead of scoring a
// node in maintenance like any other.
func TestGatherNodeFacts_FailsWhenTheHealthReadFailsTwiceAndHealthIsRequired(t *testing.T) {
	t.Parallel()
	cl := taggedMaintenanceCluster(2, 3)
	_, err := placement.GatherNodeFacts(context.Background(), cl, markedNodes(), nopLogger(),
		placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}, RequireNodeHealth: true})
	if !errors.Is(err, placement.ErrNodeHealthUnavailable) {
		t.Fatalf("err = %v; want ErrNodeHealthUnavailable", err)
	}
}

// TestGatherNodeFacts_FailsOpenWhenHealthIsNotRequired pins that a gather
// that does not need node health still fails open after two failed reads.
func TestGatherNodeFacts_FailsOpenWhenHealthIsNotRequired(t *testing.T) {
	t.Parallel()
	cl := taggedMaintenanceCluster(2, 3)
	facts, err := placement.GatherNodeFacts(context.Background(), cl, markedNodes(), nopLogger(),
		placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || facts[0].InMaintenance {
		t.Fatalf("facts = %+v; want pve1 scored as a normal node", facts)
	}
}

// TestGatherNodeFacts_RetriesAFailedNotesReadOnce pins that a failed node
// notes read counts as a failed node health read, so it is read once more, and
// a node whose notes carry the marker on the second read is in maintenance.
func TestGatherNodeFacts_RetriesAFailedNotesReadOnce(t *testing.T) {
	t.Parallel()
	ns := markedNodes(1)
	facts, err := placement.GatherNodeFacts(context.Background(), taggedMaintenanceCluster(), ns, nopLogger(),
		placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}, RequireNodeHealth: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || !facts[0].InMaintenance {
		t.Fatalf("facts = %+v; want pve1 in maintenance from the second read", facts)
	}
	if got := ns.notesReads.Load(); got != 2 {
		t.Errorf("notes read %d times, want 2", got)
	}
}

// TestGatherNodeFacts_LeavesOutAnUnreadNodeWhenHealthIsRequired pins that a
// notes read that fails on its retry too leaves only that node out, marked
// NotesUnread with the read's cause, and does not fail the gather, so
// placement goes on with the nodes that answered.
func TestGatherNodeFacts_LeavesOutAnUnreadNodeWhenHealthIsRequired(t *testing.T) {
	t.Parallel()
	facts, err := placement.GatherNodeFacts(context.Background(), taggedMaintenanceCluster(), markedNodes(1, 2),
		nopLogger(), placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}, RequireNodeHealth: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || !facts[0].InMaintenance || !facts[0].NotesUnread ||
		!strings.Contains(facts[0].UnplaceableReason, "node config unavailable") {
		t.Fatalf("facts = %+v; want pve1 left out because its notes could not be read", facts)
	}
}

// TestGatherNodeFacts_WithholdsAnUnreadNodeWhenHealthIsNotRequired pins that a
// gather that does not need node health keeps the rest of the report when a
// node's notes cannot be read, and leaves that node out with the reason.
func TestGatherNodeFacts_WithholdsAnUnreadNodeWhenHealthIsNotRequired(t *testing.T) {
	t.Parallel()
	facts, err := placement.GatherNodeFacts(context.Background(), taggedMaintenanceCluster(), markedNodes(1, 2),
		nopLogger(), placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || !facts[0].InMaintenance || !strings.Contains(facts[0].UnplaceableReason, "notes") {
		t.Fatalf("facts = %+v; want pve1 left out because its notes could not be read", facts)
	}
}

// TestGatherNodeFacts_ReadsNoNotesWithoutMaintenanceTags pins that the node
// config is not read when no maintenance tag is configured.
func TestGatherNodeFacts_ReadsNoNotesWithoutMaintenanceTags(t *testing.T) {
	t.Parallel()
	ns := markedNodes()
	facts, err := placement.GatherNodeFacts(context.Background(), taggedMaintenanceCluster(), ns, nopLogger(),
		placement.GatherOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || facts[0].InMaintenance {
		t.Fatalf("facts = %+v; want pve1 scored as a normal node", facts)
	}
	if got := ns.notesReads.Load(); got != 0 {
		t.Errorf("notes read %d times, want none", got)
	}
}

// TestGatherNodeFacts_WarnsAboutANotesLineThatLooksLikeAMarker pins that a
// node whose notes hold a line that looks like a maintenance marker but is not
// one stays in placement, and the gather logs a warning that names the node
// and the line, so the operator learns the marker does nothing.
func TestGatherNodeFacts_WarnsAboutANotesLineThatLooksLikeAMarker(t *testing.T) {
	t.Parallel()
	ns := &stubNodes{notes: map[string]string{"pve1": "Rack 4\n- tags: maintenance"}, notesFails: map[int32]bool{}}
	logger, observed := log.NewObservedLogger(slog.LevelDebug)
	facts, err := placement.GatherNodeFacts(context.Background(), taggedMaintenanceCluster(), ns, logger,
		placement.GatherOptions{MaintenanceNodeTags: []string{"maintenance"}, RequireNodeHealth: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 || facts[0].InMaintenance {
		t.Fatalf("facts = %+v; want pve1 still in placement", facts)
	}
	for _, e := range observed.All() {
		if e.Level == slog.LevelWarn && e.Attrs["node"] == "pve1" && e.Attrs["line"] == "- tags: maintenance" {
			return
		}
	}
	t.Fatalf("no warning names pve1 and its near-miss line: %+v", observed.All())
}
