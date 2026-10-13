package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// parkerProbeFixture scripts a two-member cluster whose index row names
// pve-node1 with no tags, so delete_vm's parker check falls back to reading the
// config there. config answers each config read by node and by how many reads
// that node has already served, starting at zero.
type parkerProbeFixture struct {
	mu        sync.Mutex
	reads     map[string]int
	deletes   []string
	stops     []string
	config    func(node string, call int) (map[string]any, error)
	offline   string
	listNodes []string
}

func (f *parkerProbeFixture) readsOn(node string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[node]
}

func (f *parkerProbeFixture) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *parkerProbeFixture) deps(vmid int) handlers.Deps {
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, node string, _ int) (string, error) {
			f.mu.Lock()
			f.stops = append(f.stops, node)
			f.mu.Unlock()
			return "", nil
		},
		configFn: func(_ context.Context, node string, id int) (map[string]any, error) {
			if id != vmid {
				return map[string]any{}, nil
			}
			f.mu.Lock()
			call := f.reads[node]
			f.reads[node] = call + 1
			f.mu.Unlock()
			return f.config(node, call)
		},
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, node string, _ string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			f.mu.Lock()
			f.deletes = append(f.deletes, node)
			f.mu.Unlock()
			raw := nodes.DeleteQemuResponse{}
			return &raw, nil
		},
	}
	deps := parkerDepsUntaggedRow(vmid, qemuSvc, nodesSvc)
	members := f.listNodes
	if len(members) == 0 {
		members = []string{"pve-node1", "pve-node2"}
	}
	mc, ok := deps.PVE.(*mockPVEClient)
	if !ok {
		panic("parkerDepsUntaggedRow no longer returns a *mockPVEClient")
	}
	mc.clusterSvc = &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": "pve-node1", "type": "qemu", "tags": ""})
			resp := cluster.ListResourcesResponse{raw}
			return &resp, nil
		},
		listConfigNodesFn: func(_ context.Context) (*cluster.ListConfigNodesResponse, error) {
			resp := make(cluster.ListConfigNodesResponse, 0, len(members))
			for _, n := range members {
				raw, _ := json.Marshal(map[string]any{"name": n})
				resp = append(resp, raw)
			}
			return &resp, nil
		},
		listStatusFn: func(_ context.Context) (*cluster.ListStatusResponse, error) {
			resp := make(cluster.ListStatusResponse, 0, 1+len(members))
			resp = append(resp, json.RawMessage(`{"type":"cluster","quorate":1}`))
			for _, n := range members {
				online := 1
				if n == f.offline {
					online = 0
				}
				raw, _ := json.Marshal(map[string]any{"type": "node", "name": n, "online": online})
				resp = append(resp, raw)
			}
			return &resp, nil
		},
	}
	return deps
}

func newParkerProbeFixture(config func(node string, call int) (map[string]any, error)) *parkerProbeFixture {
	return &parkerProbeFixture{reads: map[string]int{}, config: config}
}

func runParkerProbeDelete(t *testing.T, f *parkerProbeFixture, vmid int) error {
	t.Helper()
	_, err := handlers.HandleDeleteVM(f.deps(vmid)).Handle(context.Background(), marshalArgs(strconv.Itoa(vmid)), jsonrpc.Context{})
	return err
}

func requireParkerProbeRetriable(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a retriable error, got success")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Fatalf("expected a retriable *cpierrors.Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err.Error(), want)
	}
}

// A VM that moves between the cluster scan and the parker check's config read
// answers not-found on the node the scan named. That answer is not proof the
// VM is gone, so delete_vm must locate it again and delete it where it now
// runs, never answer success while it still runs.
func TestHandleDeleteVM_ParkerCheck_VMMovedAfterScan_DeletesOnNewNode(t *testing.T) {
	t.Parallel()
	const vmid = 90061
	f := newParkerProbeFixture(func(node string, _ int) (map[string]any, error) {
		if node == "pve-node1" {
			return nil, notFoundAPIErr()
		}
		return map[string]any{"tags": "bosh-cpi"}, nil
	})

	if err := runParkerProbeDelete(t, f, vmid); err != nil {
		t.Fatalf("delete of a VM that moved after the scan: unexpected error: %v", err)
	}
	got := f.deleted()
	if len(got) != 1 || got[0] != "pve-node2" {
		t.Fatalf("destroy calls = %v, want one on pve-node2, where the VM moved", got)
	}
	f.mu.Lock()
	stops := append([]string(nil), f.stops...)
	f.mu.Unlock()
	for _, n := range stops {
		if n != "pve-node2" {
			t.Fatalf("stop ran on %s, the node the VM left; stops = %v", n, stops)
		}
	}
}

// The re-located VM is judged again, so a parker that moved after the scan is
// still refused.
func TestHandleDeleteVM_ParkerCheck_VMMovedAfterScan_StillRefusesParker(t *testing.T) {
	t.Parallel()
	const vmid = 90062
	f := newParkerProbeFixture(func(node string, _ int) (map[string]any, error) {
		if node == "pve-node1" {
			return nil, notFoundAPIErr()
		}
		return map[string]any{"tags": "bosh-cpi;bosh-parker"}, nil
	})

	err := runParkerProbeDelete(t, f, vmid)
	if err == nil {
		t.Fatal("expected the parker refusal, got success")
	}
	var cpiErr *cpierrors.Error
	// The parker check itself refuses, ahead of the ownership guard that
	// would also refuse a parker under a different message.
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() || !strings.Contains(err.Error(), "refusing to delete parker VM") {
		t.Fatalf("expected a non-retriable parker refusal, got %T: %v", err, err)
	}
	if got := f.deleted(); len(got) != 0 {
		t.Fatalf("destroy ran on %v for a parker", got)
	}
}

// Absence needs two clean sweeps. An error on any member in the second sweep
// leaves it unproven, so the answer is retriable and nothing is destroyed.
func TestHandleDeleteVM_ParkerCheck_SecondSweepErrorIsRetriable(t *testing.T) {
	t.Parallel()
	const vmid = 90063
	f := newParkerProbeFixture(func(node string, call int) (map[string]any, error) {
		if node == "pve-node2" && call >= 1 {
			return nil, &sdkerrors.APIError{HTTPCode: 403}
		}
		return nil, notFoundAPIErr()
	})

	err := runParkerProbeDelete(t, f, vmid)
	requireParkerProbeRetriable(t, err, "cannot prove absence")
	if n := f.readsOn("pve-node2"); n < 2 {
		t.Fatalf("pve-node2 served %d config reads; the second sweep never reached it", n)
	}
	if got := f.deleted(); len(got) != 0 {
		t.Fatalf("destroy ran on %v while absence was unproven", got)
	}
}

// A member the quorate cluster reports offline can't answer the probe, so it
// can't count toward absence.
func TestHandleDeleteVM_ParkerCheck_OfflineMemberIsRetriable(t *testing.T) {
	t.Parallel()
	const vmid = 90064
	f := newParkerProbeFixture(func(string, int) (map[string]any, error) {
		return nil, notFoundAPIErr()
	})
	f.offline = "pve-node2"

	err := runParkerProbeDelete(t, f, vmid)
	requireParkerProbeRetriable(t, err, "offline")
	if n := f.readsOn("pve-node2"); n != 0 {
		t.Fatalf("the offline member served %d config reads, want 0", n)
	}
	if got := f.deleted(); len(got) != 0 {
		t.Fatalf("destroy ran on %v while a member was offline", got)
	}
}

// When both sweeps find every member answering not-found, the VM is proven
// gone and delete_vm answers success without a destroy.
func TestHandleDeleteVM_ParkerCheck_ProvenAbsenceSucceeds(t *testing.T) {
	t.Parallel()
	const vmid = 90065
	f := newParkerProbeFixture(func(string, int) (map[string]any, error) {
		return nil, notFoundAPIErr()
	})

	if err := runParkerProbeDelete(t, f, vmid); err != nil {
		t.Fatalf("proven absence: unexpected error: %v", err)
	}
	// One read from the parker check, then two sweeps.
	if n := f.readsOn("pve-node1"); n != 3 {
		t.Fatalf("pve-node1 served %d config reads, want 3 (the check and two sweeps)", n)
	}
	if n := f.readsOn("pve-node2"); n != 2 {
		t.Fatalf("pve-node2 served %d config reads, want 2 (two sweeps)", n)
	}
	if got := f.deleted(); len(got) != 0 {
		t.Fatalf("destroy ran on %v for an absent VM", got)
	}
}
