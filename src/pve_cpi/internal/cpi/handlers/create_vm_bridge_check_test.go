package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// bridgeNodes answers ListNetwork per node: a node in errs fails, any other
// node reports the interfaces in ifaces. calls counts the listings per node.
type bridgeNodes struct {
	*placementInternalNodesSvc
	mu     sync.Mutex
	ifaces map[string][]string
	errs   map[string]error
	calls  map[string]int
	// block makes ListNetwork wait for its context to end, like a node that
	// accepted the connection and then stalled.
	block bool
	// blockNodes stalls only the named nodes.
	blockNodes map[string]bool
}

func (b *bridgeNodes) ListNetwork(ctx context.Context, node string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
	if b.block || b.blockNodes[node] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[node]++
	if err := b.errs[node]; err != nil {
		return nil, err
	}
	out := sdknodes.ListNetworkResponse{}
	for _, name := range b.ifaces[node] {
		out = append(out, json.RawMessage(`{"iface":"`+name+`","type":"bridge"}`))
	}
	return &out, nil
}

func (b *bridgeNodes) totalCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

// bridgeCluster adds SDN vnet listing to the placement cluster adapter.
type bridgeCluster struct {
	*fullClusterAdapter
	vnets   []string
	vnetErr error
}

func (c *bridgeCluster) ListSdnVnets(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	if c.vnetErr != nil {
		return nil, c.vnetErr
	}
	out := sdkcluster.ListSdnVnetsResponse{}
	for _, v := range c.vnets {
		out = append(out, json.RawMessage(`{"vnet":"`+v+`","zone":"z1"}`))
	}
	return &out, nil
}

type bridgePVE struct {
	*placementInternalTestPVE
	nodes   *bridgeNodes
	cluster *bridgeCluster
}

func (p *bridgePVE) Nodes() sdknodes.Service     { return p.nodes }
func (p *bridgePVE) Cluster() sdkcluster.Service { return p.cluster }

// bridgeDeps builds placement deps whose nodes report the given interfaces.
// Every call builds a fresh client, so the process-wide vnet cache never leaks
// between tests.
func bridgeDeps(t *testing.T, nodeFacts []map[string]any, ifaces map[string][]string, mode string, extra func(*config.CPIConfig)) (Deps, *bridgePVE, *log.Observer) {
	t.Helper()
	deps := buildPlacementDeps(nodeFacts, func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{BridgeCheck: mode}
		if extra != nil {
			extra(c)
		}
	})
	base := deps.PVE.(*placementInternalTestPVE)
	client := &bridgePVE{
		placementInternalTestPVE: base,
		nodes:                    &bridgeNodes{placementInternalNodesSvc: &placementInternalNodesSvc{}, ifaces: ifaces},
		cluster:                  &bridgeCluster{fullClusterAdapter: &fullClusterAdapter{sub: base.clusterClient}},
	}
	deps.PVE = client
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	deps.Logger = logger
	return deps, client, obs
}

func bridgeParsed(networks map[string]createVMNetworkSpec) *createVMParsedArgs {
	return &createVMParsedArgs{networks: networks}
}

func hasWarn(obs *log.Observer, substr string) (log.Entry, bool) {
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, substr) {
			return e, true
		}
	}
	return log.Entry{}, false
}

// ---------------------------------------------------------------------------
// collector
// ---------------------------------------------------------------------------

func collectorSpec() *createVMParsedArgs {
	parsed := bridgeParsed(map[string]createVMNetworkSpec{
		"default": {Type: "dynamic"},
		"svc":     {Type: "manual", CloudProperties: map[string]any{"bridge": "vmbr2"}},
		"v4":      {Type: "manual", NicGroup: "g", CloudProperties: map[string]any{"bridge": "vmbr3"}},
		"v6":      {Type: "manual", NicGroup: "g", CloudProperties: map[string]any{"bridge": "vmbr3"}},
		"lb":      {Type: "vip", CloudProperties: map[string]any{"bridge": "vmbr9"}},
	})
	return parsed
}

func TestCollectNICBridges_MatchesConfigureNICsResolution(t *testing.T) {
	t.Parallel()
	for _, override := range []string{"", "vmbr5"} {
		parsed := collectorSpec()
		if override != "" {
			parsed.cloudProps.NetworkDefaults = map[string]any{"bridge": override}
		}
		got, err := collectNICBridges(parsed, "vmbr0")
		if err != nil {
			t.Fatalf("collectNICBridges: %v", err)
		}

		// The set configureNICs attaches, resolved NIC by NIC with the same
		// attribute resolver it calls.
		want := map[string]struct{}{}
		deps := Deps{Config: icMinConfig()}
		for _, entry := range planNICs(parsed.networks) {
			attrs, aerr := resolveGroupNICAttributes(deps, parsed, entry, "vmbr0", "virtio")
			if aerr != nil {
				t.Fatalf("resolveGroupNICAttributes: %v", aerr)
			}
			want[attrs.bridge] = struct{}{}
		}
		if len(got) != len(want) {
			t.Fatalf("override %q: collector %v, configureNICs %v", override, got, want)
		}
		for _, b := range got {
			if _, ok := want[b]; !ok {
				t.Errorf("override %q: collector bridge %q is not attached by configureNICs (%v)", override, b, want)
			}
		}
	}
}

func TestCollectNICBridges_SortedAndDistinct(t *testing.T) {
	t.Parallel()
	got, err := collectNICBridges(collectorSpec(), "vmbr0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "vmbr0,vmbr2,vmbr3,vmbr9" {
		t.Errorf("bridges = %v, want vmbr0,vmbr2,vmbr3,vmbr9", got)
	}
}

func TestPlainCheckRefs_SkipsVIPAndSDNVnets(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, nil, "enforce", nil)
	client.cluster.vnets = []string{"vmbr2"}
	refs := plainCheckRefs(context.Background(), deps, collectorSpec())
	var got []string
	for _, r := range refs {
		got = append(got, r.bridge)
	}
	if strings.Join(got, ",") != "vmbr0,vmbr3,vmbr3" {
		t.Errorf("plain refs = %v, want vmbr0 and both vmbr3 members (vip and vnet skipped)", got)
	}
}

func TestPlainCheckRefs_SDNMembershipUnknown_Skipped(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, nil, "enforce", nil)
	client.cluster.vnetErr = errors.New("sdn list denied")
	if refs := plainCheckRefs(context.Background(), deps, collectorSpec()); len(refs) != 0 {
		t.Errorf("when SDN membership is unknown every bridge is left to the SDN gate; got %v", refs)
	}
}

// ---------------------------------------------------------------------------
// guard (verifyBridgesOnNode)
// ---------------------------------------------------------------------------

func oneNet() *createVMParsedArgs {
	return bridgeParsed(map[string]createVMNetworkSpec{"default": {Type: "dynamic"}})
}

func TestVerifyBridgesOnNode_Enforce_MissingBridge_FailsNonRetriable(t *testing.T) {
	t.Parallel()
	deps, _, _ := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr1"}}, "enforce", nil)
	parsed := oneNet()
	parsed.cloudProps.AvailabilityZone = "z2"
	err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2")
	if err == nil {
		t.Fatal("enforce must fail when the chosen node lacks the bridge")
	}
	for _, want := range []string{`bridge "vmbr0"`, `network "default"`, `node "pve2"`, `availability_zone "z2"`, "SDN vnet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if asCPIError(t, err).OkToRetry() {
		t.Error("a proven absence is permanent, so the error must not be retriable")
	}
}

func TestVerifyBridgesOnNode_Enforce_LookupFailure_FailsRetriable(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, nil, "enforce", nil)
	client.nodes.errs = map[string]error{"pve2": errors.New("pve 503")}
	err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2")
	if err == nil || !asCPIError(t, err).OkToRetry() {
		t.Fatalf("a failed lookup must be a retriable error, got %v", err)
	}
}

func TestVerifyBridgesOnNode_Warn_MissingBridge_ProceedsAndNamesNodeBridgeNIC(t *testing.T) {
	t.Parallel()
	deps, _, obs := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr1"}}, "", nil)
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
		t.Fatalf("the default must not reject: %v", err)
	}
	e, ok := hasWarn(obs, "bridge is missing")
	if !ok {
		t.Fatalf("expected a Warn about the missing bridge; entries: %+v", obs.All())
	}
	if e.Attrs["node"] != "pve2" || e.Attrs["bridge"] != "vmbr0" || e.Attrs["nic"] != "default" {
		t.Errorf("Warn must name node, bridge, and NIC; got %+v", e.Attrs)
	}
}

func TestVerifyBridgesOnNode_Warn_LookupFailure_ProceedsWithWarn(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, nil, nil, "warn", nil)
	client.nodes.errs = map[string]error{"pve2": errors.New("pve 503")}
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
		t.Fatalf("warn must not reject on a lookup failure: %v", err)
	}
	if _, ok := hasWarn(obs, "network lookup failed"); !ok {
		t.Errorf("expected a lookup-failure Warn; entries: %+v", obs.All())
	}
}

func TestVerifyBridgesOnNode_Off_MakesNoCalls(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, nil, nil, "off", nil)
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
		t.Fatal(err)
	}
	if client.nodes.totalCalls() != 0 || obs.Len() != 0 {
		t.Errorf("off must neither list networks nor log (calls=%d, logs=%d)", client.nodes.totalCalls(), obs.Len())
	}
}

func TestVerifyBridgesOnNode_PresentBridge_Quiet(t *testing.T) {
	t.Parallel()
	deps, _, obs := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr0"}}, "enforce", nil)
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
		t.Fatal(err)
	}
	if _, warned := hasWarn(obs, ""); warned {
		t.Errorf("a present bridge must not warn; entries: %+v", obs.All())
	}
}

func TestVerifyBridgesOnNode_SDNVnetNotYetPresent_Skipped(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"warn", "enforce"} {
		deps, client, obs := bridgeDeps(t, nil, map[string][]string{"pve2": {}}, mode, nil)
		client.cluster.vnets = []string{"vmbr0"}
		if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
			t.Fatalf("%s: a vnet that has not converged yet must never be rejected here: %v", mode, err)
		}
		if client.nodes.totalCalls() != 0 {
			t.Errorf("%s: a vnet is not listed by this check, calls=%d", mode, client.nodes.totalCalls())
		}
		if _, warned := hasWarn(obs, ""); warned {
			t.Errorf("%s: no warning expected for a vnet", mode)
		}
	}
}

func TestVerifyBridgesOnNode_SDNMembershipUnknown_Skipped(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, map[string][]string{"pve2": {}}, "enforce", nil)
	client.cluster.vnetErr = errors.New("sdn list denied")
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), oneNet(), "pve2"); err != nil {
		t.Fatalf("unknown SDN membership must skip the bridge, got %v", err)
	}
}

func TestVerifyBridgesOnNode_MultiNIC_ListsEachNodeOnce(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr0", "vmbr2", "vmbr3"}}, "enforce", nil)
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), collectorSpec(), "pve2"); err != nil {
		t.Fatal(err)
	}
	if got := client.nodes.calls["pve2"]; got != 1 {
		t.Errorf("ListNetwork calls on pve2 = %d, want 1 for a three-bridge VM", got)
	}
}

func TestVerifyBridgesOnNode_SkipsStoragePlanPath(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, map[string][]string{"pve2": {}}, "enforce", nil)
	parsed := oneNet()
	parsed.storagePlan = &StorageAllocationPlan{}
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2"); err != nil {
		t.Fatal(err)
	}
	if client.nodes.totalCalls() != 0 {
		t.Error("the storage-plan path already gates its node; the guard must not list networks there")
	}
}

// ---------------------------------------------------------------------------
// placement filter
// ---------------------------------------------------------------------------

// forceBridges builds an enforcing plan for the given bridges, the way
// placementBridgePlan does at bridge_check enforce.
func forceBridges(deps Deps, bridges ...string) bridgePlan {
	return bridgePlan{required: bridges, checker: newNodeBridgeChecker(context.Background(), deps.PVE).check}
}

func TestPlacementBridgePlan_ByMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"off"} {
		deps, client, _ := bridgeDeps(t, nil, nil, mode, nil)
		if got := placementBridgePlan(context.Background(), deps, oneNet()); len(got.required) != 0 {
			t.Errorf("mode %q: placement must require no bridge, got %v", mode, got.required)
		}
		if client.nodes.totalCalls() != 0 {
			t.Errorf("mode %q: placement must make no network call", mode)
		}
	}
	for mode, warnOnly := range map[string]bool{"": true, "warn": true, "enforce": false} {
		deps, client, _ := bridgeDeps(t, nil, nil, mode, nil)
		got := placementBridgePlan(context.Background(), deps, oneNet())
		if len(got.required) != 1 || got.required[0] != "vmbr0" || got.warnOnly != warnOnly {
			t.Errorf("mode %q: plan = %+v, want vmbr0 with warnOnly=%v", mode, got, warnOnly)
		}
		if client.nodes.totalCalls() != 0 {
			t.Errorf("mode %q: building the plan must make no network call", mode)
		}
	}
}

func TestResolveTargetNode_BridgeMissingOnNode_SkipsToNextNode(t *testing.T) {
	t.Parallel()
	// pve1 is idle and would win on score, but it lacks vmbr7.
	busy := onlineNode("pve2")
	busy["mem"] = int64(6 * 1024 * 1024 * 1024)
	deps, _, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), busy},
		map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0", "vmbr7"}}, "enforce", func(c *config.CPIConfig) {
			c.Placement.AZMap = map[string][]string{"z": {"pve1", "pve2"}}
		})
	cp := createVMCloudProps{AvailabilityZone: "z"}
	node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, "", nil, nil, nil, 0, forceBridges(deps, "vmbr7"))
	if err != nil || node != "pve2" {
		t.Fatalf("node=%q err=%v, want pve2 (pve1 lacks vmbr7)", node, err)
	}
	// Without the requirement the idle node wins, so the bridge filter is what moved the VM.
	node, _, err = resolveTargetNodeWithBridges(context.Background(), deps, cp, "", nil, nil, nil, 0, bridgePlan{})
	if err != nil || node != "pve1" {
		t.Fatalf("control: node=%q err=%v, want pve1", node, err)
	}
}

func TestResolveTargetNode_BridgeMissingEverywhere_StrictExhaustionNamesBridge(t *testing.T) {
	t.Parallel()
	deps, _, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2"), onlineNode("pve9")},
		map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}}, "enforce", func(c *config.CPIConfig) {
			c.Placement.AZMap = map[string][]string{"z": {"pve1", "pve2"}}
			c.Placement.AZEnforcement = "strict"
		})
	cp := createVMCloudProps{AvailabilityZone: "z"}
	_, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, "", nil, nil, nil, 0, forceBridges(deps, "vmbr7"))
	if err == nil || !strings.Contains(err.Error(), "pve1: missing bridge vmbr7") {
		t.Fatalf("want an exhaustion error naming the missing bridge, got %v", err)
	}
	if asCPIError(t, err).OkToRetry() {
		t.Error("a missing bridge is permanent, so the exhaustion must not be retriable")
	}
}

func TestResolveTargetNode_BridgeLookupFailsEverywhere_StrictRetriable(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")}, nil, "enforce", func(c *config.CPIConfig) {
		c.Placement.AZMap = map[string][]string{"z": {"pve1", "pve2"}}
		c.Placement.AZEnforcement = "strict"
	})
	client.nodes.errs = map[string]error{"pve1": errors.New("503"), "pve2": errors.New("503")}
	cp := createVMCloudProps{AvailabilityZone: "z"}
	_, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, "", nil, nil, nil, 0, forceBridges(deps, "vmbr0"))
	if err == nil || !strings.Contains(err.Error(), "network lookup failed") || !asCPIError(t, err).OkToRetry() {
		t.Fatalf("an AZ exhausted by lookup errors must be retriable and say so, got %v", err)
	}
}

func TestResolveTargetNode_BridgeFilter_MemoizesListingPerNode(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1")},
		map[string][]string{"pve1": {"vmbr0", "vmbr2", "vmbr3"}}, "enforce", nil)
	cp := createVMCloudProps{}
	if _, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, "", nil, nil, nil, 0,
		forceBridges(deps, "vmbr0", "vmbr2", "vmbr3")); err != nil {
		t.Fatal(err)
	}
	if got := client.nodes.calls["pve1"]; got != 1 {
		t.Errorf("ListNetwork calls on pve1 = %d, want 1 for three required bridges", got)
	}
}

func TestResolveTargetNode_Warn_LogsWouldBeRejectionsAndRejectsNothing(t *testing.T) {
	t.Parallel()
	// pve1 is idle and lacks vmbr0, so enforce would skip it. At warn it must
	// still win, and the filter must say which nodes enforce would have dropped.
	busy := onlineNode("pve2")
	busy["mem"] = int64(6 * 1024 * 1024 * 1024)
	deps, client, obs := bridgeDeps(t, []map[string]any{onlineNode("pve1"), busy},
		map[string][]string{"pve1": {"vmbr1"}, "pve2": {"vmbr0"}}, "warn", func(c *config.CPIConfig) {
			c.Placement.AZMap = map[string][]string{"z": {"pve1", "pve2"}}
		})
	parsed := oneNet()
	parsed.cloudProps.AvailabilityZone = "z"
	node, err := resolveVMShapeNodeOnly(context.Background(), deps, parsed)
	if err != nil || node != "pve1" {
		t.Fatalf("node=%q err=%v, want pve1 (warn rejects nothing)", node, err)
	}
	warned := map[string]bool{}
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "enforce would reject") {
			warned[e.Attrs["node"].(string)] = true
			if e.Attrs["bridge"] != "vmbr0" {
				t.Errorf("warning names bridge %v, want vmbr0", e.Attrs["bridge"])
			}
		}
	}
	if !warned["pve1"] || warned["pve2"] {
		t.Errorf("want a would-reject Warn for pve1 only, got %v", warned)
	}
	if got := client.nodes.calls["pve1"]; got != 1 {
		t.Errorf("pve1 listed %d times, want 1", got)
	}
}

func TestResolveTargetNode_Warn_LookupFailureLoggedAndNodeKept(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, []map[string]any{onlineNode("pve1")}, nil, "warn", nil)
	client.nodes.errs = map[string]error{"pve1": errors.New("503")}
	node, err := resolveVMShapeNodeOnly(context.Background(), deps, oneNet())
	if err != nil || node != "pve1" {
		t.Fatalf("node=%q err=%v, want pve1 kept despite the failed lookup", node, err)
	}
	if _, ok := hasWarn(obs, "network lookup failed"); !ok {
		t.Error("a failed lookup must be logged at Warn")
	}
}

func TestResolveTargetNode_BridgeOff_NoBridgeCallsDuringPlacement(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")}, nil, "off", nil)
	if _, err := resolveVMShapeNodeOnly(context.Background(), deps, oneNet()); err != nil {
		t.Fatal(err)
	}
	if client.nodes.totalCalls() != 0 {
		t.Errorf("off must not list networks during placement, calls=%d", client.nodes.totalCalls())
	}
}

func TestLogFilterRejections_BridgeReasonsAreWarn(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	logFilterRejections(logger, map[string]string{
		"pve1": "missing bridge vmbr7",
		"pve2": "network lookup failed: 503",
		"pve3": "offline",
	}, "z")
	levels := map[string]slog.Level{}
	for _, e := range obs.All() {
		levels[e.Attrs["node"].(string)] = e.Level
	}
	if levels["pve1"] != log.LevelWarn || levels["pve2"] != log.LevelWarn {
		t.Errorf("bridge rejections must log at Warn, got %v", levels)
	}
	if levels["pve3"] != log.LevelDebug {
		t.Errorf("other rejections stay at Debug, got %v", levels["pve3"])
	}
}

func TestSharedBridgeChecker_FilterAndGuardListNodeOnce(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1")}, map[string][]string{"pve1": {"vmbr0"}}, "enforce", nil)
	parsed := oneNet()
	node, err := resolveVMShapeNodeOnly(context.Background(), deps, parsed)
	if err != nil || node != "pve1" {
		t.Fatalf("node=%q err=%v", node, err)
	}
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, node); err != nil {
		t.Fatal(err)
	}
	if got := client.nodes.calls["pve1"]; got != 1 {
		t.Errorf("chosen node listed %d times across filter and guard, want 1", got)
	}
}

func TestPlainBridgeRefs_SDNMembershipUnknown_WarnsOnce(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, nil, nil, "enforce", nil)
	client.cluster.vnetErr = errors.New("sdn list denied")
	parsed := collectorSpec()
	// Placement, the guard (once per fallback candidate), and the storage plan
	// all reach plainBridgeRefs, so call it repeatedly.
	for range 4 {
		_ = plainCheckRefs(context.Background(), deps, parsed)
	}
	n := 0
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "SDN membership could not be read") {
			n++
		}
	}
	// VIP networks are never checked, so they never warn.
	var nonVIP []nicBridgeRef
	for _, ref := range mustRefs(t, parsed) {
		if !ref.vip {
			nonVIP = append(nonVIP, ref)
		}
	}
	if want := len(distinctBridges(nonVIP)); n != want || n == 0 {
		t.Fatalf("want one SDN-skip Warn per bridge per call (%d), got %d", want, n)
	}
}

func TestNodeBridgeChecker_ListingTimeoutIsALookupFailure(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, nil, "enforce", nil)
	client.nodes.block = true
	check := newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, 20*time.Millisecond).check
	present, err := check("pve1", "vmbr0")
	if err == nil || present {
		t.Fatalf("a stalled listing must be an error, never an absence: present=%v err=%v", present, err)
	}
}

func TestKeepEarlierAttemptError(t *testing.T) {
	t.Parallel()
	transient := errors.New("clone 503")
	guard := errors.New("bridge missing")
	if got := keepEarlierAttemptError(transient, guard); !errors.Is(got, transient) {
		t.Errorf("a guard failure must not replace an earlier attempt error, got %v", got)
	}
	if got := keepEarlierAttemptError(nil, guard); !errors.Is(got, guard) {
		t.Errorf("with no earlier error the guard error surfaces, got %v", got)
	}
}

// resolveVMShapeNodeOnly runs the placement step resolveVMShape runs, without
// building the whole shape.
func resolveVMShapeNodeOnly(ctx context.Context, deps Deps, parsed *createVMParsedArgs) (string, error) {
	node, _, err := resolveTargetNodeWithBridges(ctx, deps, parsed.cloudProps, "", parsed.diskCIDs, nil, parsed.cloudPropsMap, 0,
		placementBridgePlan(ctx, deps, parsed))
	return node, err
}

var _ pve.Client = (*bridgePVE)(nil)

// ---------------------------------------------------------------------------
// storage-plan path
// ---------------------------------------------------------------------------

func observePlanNodes(t *testing.T, deps Deps) ([]string, error) {
	t.Helper()
	groups, err := ObserveStoragePlanCompute(context.Background(), deps, oneNet(), nil)
	var nodes []string
	for _, g := range groups {
		nodes = append(nodes, g.Nodes...)
	}
	return nodes, err
}

func TestObserveStoragePlanCompute_PlainBridgeMissing_ExcludedAtEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"off", "warn", "enforce"} {
		deps, _, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")},
			map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr1"}}, mode, nil)
		nodes, err := observePlanNodes(t, deps)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(nodes) != 1 || nodes[0] != "pve1" {
			t.Errorf("%s: plan nodes = %v, want only pve1 (the storage plan always enforces)", mode, nodes)
		}
	}
}

func TestObserveStoragePlanCompute_SDNVnetNotYetPresent_NodeStaysEligible(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		map[string][]string{"pve1": {"vmbr0"}, "pve2": {}}, "enforce", nil)
	client.cluster.vnets = []string{"vmbr0"}
	nodes, err := observePlanNodes(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Errorf("plan nodes = %v, want both nodes: a converging vnet must not exclude a node", nodes)
	}
	if client.nodes.totalCalls() != 0 {
		t.Errorf("a vnet is left to the NIC-time gate, so no network listing is expected; calls=%d", client.nodes.totalCalls())
	}
}

func TestObserveStoragePlanCompute_BridgeLookupFails_NodeExcludedAndLogged(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		map[string][]string{"pve1": {"vmbr0"}}, "warn", nil)
	client.nodes.errs = map[string]error{"pve2": errors.New("pve 503")}
	nodes, err := observePlanNodes(t, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != "pve1" {
		t.Errorf("plan nodes = %v, want only pve1", nodes)
	}
	e, ok := hasWarn(obs, "bridge lookup failed")
	if !ok || e.Attrs["node"] != "pve2" {
		t.Errorf("expected a Warn naming pve2 for the failed lookup; entries: %+v", obs.All())
	}
	warns := 0
	for _, entry := range obs.All() {
		if entry.Level == slog.LevelWarn && entry.Attrs["node"] == "pve2" {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("a failed node must be logged once, got %d Warns", warns)
	}
}

func mustRefs(t *testing.T, parsed *createVMParsedArgs) []nicBridgeRef {
	t.Helper()
	refs, err := collectNICBridgeRefs(parsed, "vmbr0")
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestNodeBridgeChecker_OneBudgetAcrossNodes(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, nil, "enforce", nil)
	client.nodes.block = true
	check := newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, 200*time.Millisecond).check
	start := time.Now()
	for _, node := range []string{"pve1", "pve2", "pve3", "pve4"} {
		if present, err := check(node, "vmbr0"); err == nil || present {
			t.Fatalf("%s: a stalled listing must be an error, got present=%v err=%v", node, present, err)
		}
	}
	// A per-node budget would take about 800ms here.
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("four stalled nodes took %v, want one shared 200ms budget", elapsed)
	}
}

func TestResolveTargetNode_Warn_StalledNodesOnlyWarn(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve2")}, nil, "warn", nil)
	client.nodes.block = true
	parsed := oneNet()
	parsed.nodeBridgeCheck = newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, 50*time.Millisecond)
	node, err := resolveVMShapeNodeOnly(context.Background(), deps, parsed)
	if err != nil || node == "" {
		t.Fatalf("a stalled listing must not reject at warn: node=%q err=%v", node, err)
	}
	if _, ok := hasWarn(obs, "network lookup failed"); !ok {
		t.Error("a stalled listing must be logged at Warn")
	}
}

// The guard can run long after the filter pass. A shared budget that expired in
// between must not make it refuse a healthy node it has not listed yet.
func TestGuard_UncachedNodeAfterBudgetExpiry_ListsUnderFreshBudget(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, nil, map[string][]string{"pve1": {"vmbr0"}, "pve2": {"vmbr0"}}, "enforce", nil)
	parsed := oneNet()
	parsed.nodeBridgeCheck = newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, 30*time.Millisecond)
	// The filter pass lists pve1 and uses up the shared budget.
	if present, err := parsed.nodeBridgeCheck.check("pve1", "vmbr0"); err != nil || !present {
		t.Fatalf("filter pass: present=%v err=%v", present, err)
	}
	time.Sleep(60 * time.Millisecond)
	// A node the filter never listed, such as a fallback alternate, is healthy.
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2"); err != nil {
		t.Fatalf("an expired filter budget must not refuse a healthy uncached node: %v", err)
	}
	if _, ok := hasWarn(obs, "bridge"); ok {
		t.Error("no Warn expected for a healthy node")
	}
	// A cached node is answered from its listing, never listed again.
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve1"); err != nil {
		t.Fatal(err)
	}
	if client.nodes.calls["pve1"] != 1 || client.nodes.calls["pve2"] != 1 {
		t.Errorf("each node must be listed once, got %v", client.nodes.calls)
	}
}

// When the shared budget runs out while node A is listed, node B gets a cached
// deadline error without ever being listed. The guard must list B once under a
// fresh budget instead of reusing that error.
func TestGuard_NodeCachedAsBudgetExpired_IsListedOnceFresh(t *testing.T) {
	t.Parallel()
	deps, client, obs := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr0"}}, "enforce", nil)
	client.nodes.blockNodes = map[string]bool{"pve1": true}
	parsed := oneNet()
	parsed.nodeBridgeCheck = newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, 30*time.Millisecond)
	// Node A stalls and uses up the budget, so node B is refused on the spot.
	if _, err := parsed.nodeBridgeCheck.check("pve1", "vmbr0"); err == nil {
		t.Fatal("the stalled node must report an error")
	}
	if _, err := parsed.nodeBridgeCheck.check("pve2", "vmbr0"); err == nil {
		t.Fatal("the filter pass must have cached a budget error for pve2")
	}
	if got := client.nodes.calls["pve2"]; got != 0 {
		t.Fatalf("pve2 was never really listed during the filter pass, got %d listings", got)
	}
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2"); err != nil {
		t.Fatalf("the guard must not refuse a healthy node over a budget error: %v", err)
	}
	if _, ok := hasWarn(obs, "bridge"); ok {
		t.Error("no Warn expected for a healthy node")
	}
	// The re-list is cached unflagged, so asking again lists nothing more.
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2"); err != nil {
		t.Fatal(err)
	}
	if got := client.nodes.calls["pve2"]; got != 1 {
		t.Errorf("pve2 listed %d times, want exactly 1", got)
	}
}

// A real API failure is reused by the guard, not retried.
func TestGuard_CachedAPIFailureIsReused(t *testing.T) {
	t.Parallel()
	deps, client, _ := bridgeDeps(t, nil, map[string][]string{"pve2": {"vmbr0"}}, "enforce", nil)
	client.nodes.errs = map[string]error{"pve2": errors.New("pve 503")}
	parsed := oneNet()
	parsed.nodeBridgeCheck = newNodeBridgeCheckerTimeout(context.Background(), deps.PVE, time.Second)
	_, _ = parsed.nodeBridgeCheck.check("pve2", "vmbr0")
	if err := verifyBridgesOnNode(context.Background(), deps, deps.Log(context.Background()), parsed, "pve2"); err == nil {
		t.Fatal("a real API failure must still refuse at enforce")
	}
	// The filter and the guard each call NodeBridgeNames once for the node's
	// first read, and the guard must not have listed it again.
	if got := client.nodes.calls["pve2"]; got > 2 {
		t.Errorf("pve2 listed %d times", got)
	}
}
