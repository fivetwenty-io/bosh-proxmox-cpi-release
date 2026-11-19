package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haListsCluster answers the HA resource and rule lists from fixed rows, or
// fails them with err, and counts the reads.
type haListsCluster struct {
	sdkcluster.Service
	resources []map[string]any
	rules     []map[string]any
	err       error

	mu    sync.Mutex
	reads int
}

func (c *haListsCluster) ListHaResources(
	context.Context, *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := make(sdkcluster.ListHaResourcesResponse, 0, len(c.resources))
	for _, row := range c.resources {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

func (c *haListsCluster) ListHaRules(
	context.Context, *sdkcluster.ListHaRulesParams,
) (*sdkcluster.ListHaRulesResponse, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	out := make(sdkcluster.ListHaRulesResponse, 0, len(c.rules))
	for _, row := range c.rules {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

func (c *haListsCluster) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// haScanClient serves the cluster API through an haListsCluster wrapped
// around the base client's own cluster service.
type haScanClient struct {
	pve.Client
	lists *haListsCluster
}

func (c *haScanClient) Cluster() sdkcluster.Service {
	c.lists.Service = c.Client.Cluster()
	return c.lists
}

func newHAScanVMIDClient(lists *haListsCluster) *haScanClient {
	base := newVMIDClient(func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})
	return &haScanClient{Client: base, lists: lists}
}

// TestNextVMID_HAStateScan_SkipsVMIDsTheHAStateNames proves the HA state scan
// keeps out of the allocation every VMID an HA resource names, every VMID a
// rule names as a resource, and every VMID a pin's name carries, whether the
// rule lists its resources as a string or as an array.
func TestNextVMID_HAStateScan_SkipsVMIDsTheHAStateNames(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{
		resources: []map[string]any{{"sid": "vm:200", "state": "stopped"}, {"sid": "ct:201"}},
		rules: []map[string]any{
			{"rule": "keep-apart", "type": "resource-affinity", "resources": "vm:202,ct:203"},
			{"rule": "pin-array", "type": "node-affinity", "resources": []string{"vm:204"}},
			{"rule": "bosh-na-205", "type": "node-affinity", "resources": ""},
		},
	}
	c := newHAScanVMIDClient(lists)
	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 206), pve.WithHAStateScan())
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if id != 206 {
		t.Fatalf("NextVMID = %d, want 206, the one VMID the HA state does not name", id)
	}
}

// TestNextVMID_HAStateScan_ExhaustsTheRangeTheHAStateFills proves a range
// whose every VMID the HA state names allocates nothing.
func TestNextVMID_HAStateScan_ExhaustsTheRangeTheHAStateFills(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{
		resources: []map[string]any{{"sid": "vm:200"}},
		rules:     []map[string]any{{"rule": "bosh-na-201", "type": "node-affinity", "resources": "vm:201"}},
	}
	c := newHAScanVMIDClient(lists)
	if id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 201), pve.WithHAStateScan()); err == nil {
		t.Fatalf("NextVMID = %d, want an exhausted range", id)
	}
}

// TestNextVMID_HAStateScan_FailsClosed proves a failed HA read fails the
// allocation instead of allocating blind to the HA state.
func TestNextVMID_HAStateScan_FailsClosed(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{err: errors.New("connection reset by peer")}
	c := newHAScanVMIDClient(lists)
	if id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 210), pve.WithHAStateScan()); err == nil {
		t.Fatalf("NextVMID = %d, want the HA read's failure", id)
	}
}

// TestNextVMID_WithoutHAStateScanReadsNoHAState proves an allocation that
// does not ask for the scan reads no HA state.
func TestNextVMID_WithoutHAStateScanReadsNoHAState(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{resources: []map[string]any{{"sid": "vm:200"}}}
	c := newHAScanVMIDClient(lists)
	if _, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 200)); err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if n := lists.readCount(); n != 0 {
		t.Fatalf("HA reads = %d, want none", n)
	}
}
