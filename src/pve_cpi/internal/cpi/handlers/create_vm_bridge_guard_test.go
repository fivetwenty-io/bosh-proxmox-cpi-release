package handlers_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func bridgeArgs(cp map[string]any) []json.RawMessage {
	return mkArgs("agent-bridge", testStemcellCID, cp,
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
}

func ifaceList(names ...string) *sdknodes.ListNetworkResponse {
	out := sdknodes.ListNetworkResponse{}
	for _, n := range names {
		out = append(out, json.RawMessage(`{"iface":"`+n+`","type":"bridge"}`))
	}
	return &out
}

func TestCreateVM_PinnedNodeMissingBridge_EnforceFailsBeforeClone(t *testing.T) {
	t.Parallel()

	created := false
	q := &vmMockQEMU{createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
		created = true
		return "UPID:pve2:ok", nil
	}}
	n := &vmMockNodes{listNetworkFn: func(_ context.Context, _ string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		return ifaceList("vmbr1"), nil
	}}
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, &vmMockAgent{}, func(c *config.CPIConfig) {
		c.EnsureNoIPConflicts = placementDisabled
		c.Placement = &config.PlacementConfig{BridgeCheck: "enforce"}
	})
	h := handlers.HandleCreateVM(deps)

	_, err := h.Handle(context.Background(), bridgeArgs(map[string]any{"target_node": "pve2"}), mkCtx("bridge-enforce"))
	if err == nil {
		t.Fatal("enforce must fail a pinned node that lacks the bridge")
	}
	for _, want := range []string{`bridge "vmbr0"`, `node "pve2"`, "SDN vnet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if created {
		t.Error("the guard must fail before any VM is created or cloned")
	}
}

func TestCreateVM_PinnedNodeMissingBridge_DefaultWarnProceeds(t *testing.T) {
	t.Parallel()

	var createNode string
	q := &vmMockQEMU{createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
		createNode = node
		return "UPID:pve2:ok", nil
	}}
	n := &vmMockNodes{listNetworkFn: func(_ context.Context, _ string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		return ifaceList("vmbr1"), nil
	}}
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, &vmMockAgent{}, func(c *config.CPIConfig) {
		c.EnsureNoIPConflicts = placementDisabled
	})
	deps.Logger = logger
	h := handlers.HandleCreateVM(deps)

	if _, err := h.Handle(context.Background(), bridgeArgs(map[string]any{"target_node": "pve2"}), mkCtx("bridge-warn")); err != nil {
		t.Fatalf("the default must not reject a missing bridge: %v", err)
	}
	if createNode != "pve2" {
		t.Errorf("create node = %q, want pve2", createNode)
	}
	found := false
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "bridge is missing") &&
			e.Attrs["node"] == "pve2" && e.Attrs["bridge"] == "vmbr0" && e.Attrs["nic"] == "default" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Warn naming node pve2, bridge vmbr0, and NIC default; got %+v", obs.All())
	}
}

func TestCreateVM_BridgeCheckOff_NoNetworkListing(t *testing.T) {
	t.Parallel()

	calls := 0
	q := &vmMockQEMU{createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) { return "UPID:pve2:ok", nil }}
	n := &vmMockNodes{listNetworkFn: func(_ context.Context, _ string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		calls++
		return ifaceList(), nil
	}}
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, &vmMockAgent{}, func(c *config.CPIConfig) {
		c.EnsureNoIPConflicts = placementDisabled
		c.Placement = &config.PlacementConfig{BridgeCheck: "off"}
	})
	h := handlers.HandleCreateVM(deps)
	if _, err := h.Handle(context.Background(), bridgeArgs(map[string]any{"target_node": "pve2"}), mkCtx("bridge-off")); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("bridge_check off listed networks %d times, want 0", calls)
	}
}

// enforceTwoNodeCreate creates a VM with no pin on two nodes where pve1 lacks
// vmbr0 and pve2 has it, and returns the node the clone landed on plus how many
// times each node's network was listed.
func enforceTwoNodeCreate(t *testing.T, fallbackMax int) (string, map[string]int) {
	t.Helper()
	var mu sync.Mutex
	listings := map[string]int{}
	var createNode string
	q := &vmMockQEMU{createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
		createNode = node
		return "UPID:" + node + ":ok", nil
	}}
	n := &vmMockNodes{listNetworkFn: func(_ context.Context, node string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		listings[node]++
		if node == "pve1" {
			return ifaceList("vmbr1"), nil
		}
		return ifaceList("vmbr0"), nil
	}}
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, &vmMockAgent{}, func(c *config.CPIConfig) {
		c.EnsureNoIPConflicts = placementDisabled
		c.Placement = &config.PlacementConfig{BridgeCheck: "enforce", FallbackMax: &fallbackMax}
	})
	h := handlers.HandleCreateVM(deps)
	if _, err := h.Handle(context.Background(), bridgeArgs(map[string]any{}), mkCtx("bridge-two-node")); err != nil {
		t.Fatalf("placement should have skipped the bridgeless node: %v", err)
	}
	return createNode, listings
}

// The single-shot path (fallback_max 0) goes through resolveVMShape. If that
// call dropped the bridge requirement, placement would pick the idle pve1 and
// the guard would fail the create instead of moving to pve2.
func TestCreateVM_SingleShot_EnforceSkipsNodeMissingBridge(t *testing.T) {
	t.Parallel()
	node, listings := enforceTwoNodeCreate(t, 0)
	if node != "pve2" {
		t.Errorf("create node = %q, want pve2 (pve1 lacks vmbr0)", node)
	}
	if listings["pve2"] != 1 {
		t.Errorf("chosen node listed %d times across filter and guard, want 1", listings["pve2"])
	}
}

func TestCreateVM_FallbackPath_EnforceSkipsNodeMissingBridge(t *testing.T) {
	t.Parallel()
	node, listings := enforceTwoNodeCreate(t, 2)
	if node != "pve2" {
		t.Errorf("create node = %q, want pve2 (pve1 lacks vmbr0)", node)
	}
	if listings["pve2"] != 1 {
		t.Errorf("chosen node listed %d times across filter and guard, want 1", listings["pve2"])
	}
}

// At warn the filter keeps pve1, which lacks the bridge, and logs that enforce
// would have dropped it.
func TestCreateVM_Warn_FilterLogsWouldRejectNodeAndStillPlaces(t *testing.T) {
	t.Parallel()
	var createNode string
	q := &vmMockQEMU{createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
		createNode = node
		return "UPID:" + node + ":ok", nil
	}}
	n := &vmMockNodes{listNetworkFn: func(_ context.Context, node string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
		if node == "pve1" {
			return ifaceList("vmbr1"), nil
		}
		return ifaceList("vmbr0"), nil
	}}
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	zero := 0
	deps := buildVMDepsPlacement(q, n, listStatusTwoNodes(), emptyListResources, &vmMockAgent{}, func(c *config.CPIConfig) {
		c.EnsureNoIPConflicts = placementDisabled
		c.Placement = &config.PlacementConfig{FallbackMax: &zero}
	})
	deps.Logger = logger
	h := handlers.HandleCreateVM(deps)
	if _, err := h.Handle(context.Background(), bridgeArgs(map[string]any{}), mkCtx("bridge-warn-filter")); err != nil {
		t.Fatal(err)
	}
	if createNode != "pve1" {
		t.Errorf("warn must reject nothing, so the idle pve1 still wins; got %q", createNode)
	}
	found := false
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "enforce would reject") && e.Attrs["node"] == "pve1" && e.Attrs["bridge"] == "vmbr0" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a would-reject Warn for pve1 and vmbr0; got %+v", obs.All())
	}
}
