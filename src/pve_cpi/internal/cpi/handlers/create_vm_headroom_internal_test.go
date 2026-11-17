package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const headroomMiB = int64(1024 * 1024)

// headroomCluster answers the three reads the headroom estimate adds to
// placement, and counts them. The unfiltered ListResources that node facts
// make passes through to the base fake.
type headroomCluster struct {
	*fullClusterAdapter
	mu sync.Mutex

	guests      []map[string]any
	haResources []map[string]any
	rules       []map[string]any

	resourcesErr error
	haErr        error
	rulesErr     error

	guestReads, haReads, ruleReads int
}

func (c *headroomCluster) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guestReads + c.haReads + c.ruleReads
}

func rawRows(rows []map[string]any) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		b, _ := json.Marshal(r)
		out = append(out, b)
	}
	return out
}

func (c *headroomCluster) ListResources(
	ctx context.Context, p *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	if p == nil || p.Type == nil || *p.Type != "vm" {
		return c.fullClusterAdapter.ListResources(ctx, p)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.guestReads++
	if c.resourcesErr != nil {
		return nil, c.resourcesErr
	}
	out := sdkcluster.ListResourcesResponse(rawRows(c.guests))
	return &out, nil
}

func (c *headroomCluster) ListHaResources(
	_ context.Context, _ *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.haReads++
	if c.haErr != nil {
		return nil, c.haErr
	}
	out := sdkcluster.ListHaResourcesResponse(rawRows(c.haResources))
	return &out, nil
}

func (c *headroomCluster) ListHaRules(
	_ context.Context, _ *sdkcluster.ListHaRulesParams,
) (*sdkcluster.ListHaRulesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ruleReads++
	if c.rulesErr != nil {
		return nil, c.rulesErr
	}
	out := sdkcluster.ListHaRulesResponse(rawRows(c.rules))
	return &out, nil
}

type headroomPVE struct {
	*placementInternalTestPVE
	cluster *headroomCluster
}

func (p *headroomPVE) Cluster() sdkcluster.Service { return p.cluster }
func (p *headroomPVE) Nodes() sdknodes.Service     { return &placementInternalNodesSvc{} }

// headroomRig wires placement to a three-node cluster of 8 GiB nodes that each
// report 6 GiB free, with shared storage "nfs" for the VM's root disk.
type headroomRig struct {
	deps         Deps
	cluster      *headroomCluster
	profileCalls *int
	logs         *log.Observer
}

func qemuRow(vmid int, node string, memMiB int64) map[string]any {
	return map[string]any{"type": "qemu", "vmid": vmid, "node": node, "maxmem": memMiB * headroomMiB, "template": 0}
}

// newHeadroomRig builds the rig for pve.ha.mode haMode and pve.ha.headroom
// headroom. guests is the cluster's guest list.
func newHeadroomRig(t *testing.T, haMode, headroom string, guests []map[string]any, cfgFn func(*config.CPIConfig)) *headroomRig {
	t.Helper()
	deps := buildPlacementDeps(
		[]map[string]any{onlineNode("pve01"), onlineNode("pve02"), onlineNode("pve03")},
		func(c *config.CPIConfig) {
			c.VMStorage = "nfs"
			c.ISOStorage = "nfs"
			c.HA = &config.HAConfig{Mode: haChoiceOf(haMode), Headroom: haChoiceOf(headroom)}
			if cfgFn != nil {
				cfgFn(c)
			}
		})
	base := deps.PVE.(*placementInternalTestPVE)
	cluster := &headroomCluster{fullClusterAdapter: &fullClusterAdapter{sub: base.clusterClient}, guests: guests}
	deps.PVE = &headroomPVE{placementInternalTestPVE: base, cluster: cluster}
	deps.RequestDirectorUUID = "director-1"
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	deps.Logger = logger
	calls := new(int)
	profile := stagingProfile(haMode)
	deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		*calls++
		return profile, nil
	}
	return &headroomRig{deps: deps, cluster: cluster, profileCalls: calls, logs: obs}
}

// testBuildPlan builds the plan for a VM with no persistent disks and no
// storage selector. A nil resolver stands for a request with no cloud properties.
func testBuildPlan(
	t *testing.T, deps Deps, cp createVMCloudProps, resolver *layeredResolver, bridges bridgePlan,
	facts []placement.NodeFacts, logger *log.Logger,
) *headroomPlan {
	t.Helper()
	if resolver == nil {
		resolver = mustResolver(t, deps)
	}
	return buildHeadroomPlan(context.Background(), deps, cp, nil, resolver, nil, bridges, facts, logger)
}

func (r *headroomRig) place(t *testing.T, cp createVMCloudProps) (string, []string, error) {
	t.Helper()
	cp.Memory = 4096
	return resolveTargetNodeWithBridges(context.Background(), r.deps, cp, scorerGroup{}, nil, nil, nil, 3, bridgePlan{})
}

// fullNonHAGuests fills the named nodes with 6 GiB of non-HA guests, which
// leaves 2 GiB free on each.
func fullNonHAGuests(nodes ...string) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for i, n := range nodes {
		out = append(out, qemuRow(201+i, n, 6144))
	}
	return out
}

func TestHeadroom_EnforceRefusesWhenEveryNodeIsShort(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	_, _, err := rig.place(t, createVMCloudProps{})
	if err == nil {
		t.Fatal("placement succeeded; want a refusal, because the VM fits on no surviving node")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Fatalf("error = %v; want a Retriable error", err)
	}
	if !strings.Contains(err.Error(), placement.HeadroomShortPrefix) ||
		!strings.Contains(err.Error(), "ha: headroom estimate short after losing") {
		t.Errorf("error = %v; want the headroom shortfall line", err)
	}
}

func TestHeadroom_EnforceNeverFallsBackToConfigNode(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeManaged, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), func(c *config.CPIConfig) { c.Node = "pve01" })
	node, _, err := rig.place(t, createVMCloudProps{})
	if err == nil {
		t.Fatalf("placement chose %q; want a refusal, not a fallback to config.node", node)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Fatalf("error = %v; want a Retriable error", err)
	}
}

func TestHeadroom_EnforceDropsOnlyTheShortNodes(t *testing.T) {
	t.Parallel()
	// pve03 holds no guest and so has room to take the VM from pve01 or pve02,
	// but losing pve03 itself would leave pve01 and pve02 with 2 GiB each.
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02"), nil)
	node, alternates, err := rig.place(t, createVMCloudProps{})
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if node == "pve03" || slices.Contains(alternates, "pve03") {
		t.Errorf("chose %q with alternates %v; pve03 is short and must be filtered out", node, alternates)
	}
	if !slices.Contains([]string{"pve01", "pve02"}, node) {
		t.Errorf("chose %q; want pve01 or pve02", node)
	}
	if rig.cluster.guestReads != 1 || rig.cluster.haReads != 1 || rig.cluster.ruleReads != 1 {
		t.Errorf("reads = %d/%d/%d; want one guest, one HA resource, one rule read for the whole call",
			rig.cluster.guestReads, rig.cluster.haReads, rig.cluster.ruleReads)
	}
}

func TestHeadroom_WarnPlacesTheVMAndLogsTheEstimate(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	node, _, err := rig.place(t, createVMCloudProps{})
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if node == "" {
		t.Fatal("no node chosen; warn must place the VM anyway")
	}
	entry, ok := hasWarn(rig.logs, "ha: headroom estimate short after losing")
	if !ok {
		t.Fatalf("no warning with the headroom line; logs: %v", rig.logs.All())
	}
	if got := entry.Attrs["node"]; got != node {
		t.Errorf("warning names node %v; want the chosen node %s", got, node)
	}
}

func TestHeadroom_WarnLogsOneLinePerShortfallOnlyForTheChosenNode(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn, fullNonHAGuests("pve01", "pve02"), nil)
	if _, _, err := rig.place(t, createVMCloudProps{}); err != nil {
		t.Fatalf("placement: %v", err)
	}
	n := 0
	for _, e := range rig.logs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "ha: headroom estimate short") {
			n++
		}
	}
	if n > 1 {
		t.Errorf("%d headroom warnings; want at most one shortfall for the single chosen node", n)
	}
}

func TestHeadroom_UnavailableEstimateIsTransientInEnforce(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, nil, nil)
	rig.cluster.resourcesErr = errors.New("pvedaemon restarting")
	_, _, err := rig.place(t, createVMCloudProps{})
	if err == nil {
		t.Fatal("placement succeeded; want a refusal because the estimate could not run")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Fatalf("error = %v; want a Retriable error", err)
	}
	if !strings.Contains(err.Error(), placement.HeadroomUnavailablePrefix) {
		t.Errorf("error = %v; want the %q reason", err, placement.HeadroomUnavailablePrefix)
	}
}

func TestHeadroom_UnavailableEstimateOnlyWarnsInWarnMode(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*headroomCluster){
		"guest listing":      func(c *headroomCluster) { c.resourcesErr = errors.New("boom") },
		"HA resource list":   func(c *headroomCluster) { c.haErr = errors.New("boom") },
		"HA rule list":       func(c *headroomCluster) { c.rulesErr = errors.New("boom") },
		"malformed guest":    func(c *headroomCluster) { c.guests = []map[string]any{{"type": "qemu", "vmid": 5}} },
		"malformed HA row":   func(c *headroomCluster) { c.haResources = []map[string]any{{"state": "started"}} },
		"malformed rule row": func(c *headroomCluster) { c.rules = []map[string]any{{"type": "node-affinity"}} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn, nil, nil)
			set(rig.cluster)
			node, _, err := rig.place(t, createVMCloudProps{})
			if err != nil || node == "" {
				t.Fatalf("placement = %q, %v; want the VM placed", node, err)
			}
			if _, ok := hasWarn(rig.logs, "ha: headroom estimate unavailable"); !ok {
				t.Errorf("no unavailable warning; logs: %v", rig.logs.All())
			}
		})
	}
}

func TestHeadroom_OffMakesNoReadsAndLeavesTheRequestAlone(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, mode, headroom string }{
		{"mode off", config.HAModeOff, config.HAHeadroomEnforce},
		{"mode off with warn", config.HAModeOff, config.HAHeadroomWarn},
		{"headroom off in auto", config.HAModeAuto, config.HAHeadroomOff},
		{"headroom off in managed", config.HAModeManaged, config.HAHeadroomOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newHeadroomRig(t, tc.mode, tc.headroom, fullNonHAGuests("pve01", "pve02", "pve03"), nil)
			node, _, err := rig.place(t, createVMCloudProps{})
			if err != nil || node == "" {
				t.Fatalf("placement = %q, %v; want the VM placed with no headroom check", node, err)
			}
			if got := rig.cluster.reads(); got != 0 {
				t.Errorf("%d headroom reads; want none", got)
			}
			if *rig.profileCalls != 0 {
				t.Errorf("the HA profile was resolved %d times; want none", *rig.profileCalls)
			}
			if plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil,
				bridgePlan{}, nil, rig.deps.Logger); plan != nil {
				t.Errorf("plan = %+v; want nil", plan)
			}
			pass, rejections := (*headroomPlan)(nil).filter(factsOf("pve01", "pve02"), placement.Request{},
				rig.deps.Logger)
			if len(pass) != 2 || rejections != nil {
				t.Errorf("nil plan filtered to %v with rejections %v; want every node and none", pass, rejections)
			}
		})
	}
}

func TestHeadroom_DefaultModeIsWarn(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn, nil, func(c *config.CPIConfig) {
		c.HA.Headroom = nil
	})
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridgePlan{},
		factsOf("pve01", "pve02", "pve03"), rig.deps.Logger)
	if plan == nil || plan.check.Mode != config.HAHeadroomWarn {
		t.Fatalf("plan = %+v; want a warn-mode check", plan)
	}
	// The filter keeps every node in warn mode, and the guests are not read until
	// a node is chosen.
	pass, rejections := plan.filter(factsOf("pve01", "pve02", "pve03"), placement.Request{}, rig.deps.Logger)
	if len(pass) != 3 || rejections != nil {
		t.Errorf("warn filtered to %v with rejections %v; want every node and none", pass, rejections)
	}
	if got := rig.cluster.reads(); got != 0 {
		t.Errorf("%d reads before a node was chosen; want none in warn mode", got)
	}
}

// factsOf returns online facts with 8 GiB each for the named nodes.
func factsOf(nodes ...string) []placement.NodeFacts {
	out := make([]placement.NodeFacts, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, placement.NodeFacts{Node: n, Online: true, TotalMemBytes: 8192 * headroomMiB})
	}
	return out
}

func TestHeadroom_NoCheckWhenTheVMWillNotBeRegistered(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*headroomRig){
		"observe-only profile": func(r *headroomRig) {
			r.deps.HAProfile = func(context.Context) (*ha.Profile, error) {
				p := stagingProfile(config.HAModeAuto)
				p.Effective, p.Reason = ha.EffectiveObserveOnly, "no CRM master"
				return p, nil
			}
		},
		"unresolvable profile": func(r *headroomRig) {
			r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("probe failed") }
		},
		"local root disk with local_policy none": func(r *headroomRig) {
			r.deps.Config.VMStorage = testPinStorage
		},
		"local root disk with local_policy hold": func(r *headroomRig) {
			r.deps.Config.VMStorage = testPinStorage
			r.deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
		},
		"storage the profile does not list": func(r *headroomRig) {
			r.deps.Config.VMStorage = "missing"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
				fullNonHAGuests("pve01", "pve02", "pve03"), nil)
			mutate(rig)
			node, _, err := rig.place(t, createVMCloudProps{})
			if err != nil || node == "" {
				t.Fatalf("placement = %q, %v; want the VM placed, because it gets no HA", node, err)
			}
			if got := rig.cluster.reads(); got != 0 {
				t.Errorf("%d headroom reads; want none for a VM that gets no HA", got)
			}
		})
	}
}

func TestHeadroom_StrictAZNarrowsTheVMsAdmissibleSet(t *testing.T) {
	t.Parallel()
	// zone-a is pve01 and pve02. pve02 is nearly full and pve03 is empty. A VM
	// placed on pve01 can be recovered only onto pve02 under a strict AZ, so
	// losing pve01 is short, while a non-strict AZ lets it go to pve03.
	setAZ := func(strict bool) func(*config.CPIConfig) {
		return func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve01", "pve02"}}}
			c.HA.PinAZ = haChoiceOf(config.HARuleOn)
			s := strict
			c.HA.PinAZStrict = &s
		}
	}
	guests := fullNonHAGuests("pve02")
	for _, strict := range []bool{true, false} {
		rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, guests, setAZ(strict))
		cp := createVMCloudProps{AvailabilityZone: "zone-a", Memory: 4096}
		plan := testBuildPlan(t, rig.deps, cp, mustResolver(t, rig.deps), bridgePlan{},
			factsOf("pve01", "pve02", "pve03"), rig.deps.Logger)
		if plan == nil {
			t.Fatalf("strict=%v: no plan", strict)
		}
		got := plan.check.VM.AdmissibleFor("pve01")
		want := []string{"pve01", "pve02", "pve03"}
		if strict {
			want = []string{"pve01", "pve02"}
		}
		if !sameNodeSet(got, want) {
			t.Errorf("strict=%v: admissible set for pve01 = %v; want %v", strict, got, want)
		}
	}
}

func mustResolver(t *testing.T, deps Deps) *layeredResolver {
	t.Helper()
	r, err := newLayeredResolver(nil, deps.Config)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	return r
}

func sameNodeSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func TestHeadroom_StrictAZShortfallRefusesPlacementOnThatNode(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, fullNonHAGuests("pve02"),
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve01", "pve02"}}}
			c.HA.PinAZ = haChoiceOf(config.HARuleOn)
		})
	// Only pve01 and pve02 are candidates. pve02 is the only recovery target for
	// a VM on pve01, and it has 2 GiB free. A VM on pve02 would need pve01's
	// 8 GiB, which fits.
	node, alternates, err := rig.place(t, createVMCloudProps{AvailabilityZone: "zone-a"})
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if node != "pve02" || len(alternates) != 0 {
		t.Errorf("chose %q with alternates %v; want pve02 alone, because pve01 is short under the strict AZ", node, alternates)
	}
}

func TestHeadroom_BridgeConstraintNarrowsTheAdmissibleSet(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, nil, nil)
	holders := map[string]bool{"pve01": true, "pve02": true}
	bridges := bridgePlan{
		required: []string{"vmbr7"},
		checker: func(node, _ string) (bool, error) {
			if node == "pve03" {
				return false, errors.New("network lookup failed")
			}
			return holders[node], nil
		},
	}
	facts := factsOf("pve01", "pve02", "pve03")
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridges,
		facts, rig.deps.Logger)
	if plan == nil {
		t.Fatal("no plan; two nodes hold the bridge, so the VM registers")
	}
	if got := plan.check.VM.AdmissibleFor("pve01"); !sameNodeSet(got, []string{"pve01", "pve02"}) {
		t.Errorf("admissible set = %v; want the two nodes that hold the bridge", got)
	}
	// pve03 cannot run the VM at all, so no decision registers it there and the
	// check does not apply to it.
	if !plan.exempt["pve03"] || plan.exempt["pve01"] || plan.exempt["pve02"] {
		t.Errorf("exempt = %v; want only pve03, which does not hold the bridge", plan.exempt)
	}

	holders = map[string]bool{"pve01": true}
	if plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridges,
		facts, rig.deps.Logger); plan != nil {
		t.Errorf("plan = %+v; want none, because one holder leaves the VM without a recovery node", plan.check.VM)
	}
}

func TestHeadroom_PCIPassthroughPinsTheVMToItsNode(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, nil, nil)
	cp := createVMCloudProps{Memory: 4096, PCIPassthroughs: []PCIPassthrough{{Address: "0000:01:00.0"}}}
	plan := testBuildPlan(t, rig.deps, cp, nil, bridgePlan{},
		factsOf("pve01", "pve02", "pve03"), rig.deps.Logger)
	if plan != nil {
		t.Fatalf("plan = %+v; want none, because a PCI VM is pinned to its node and gets no HA recovery", plan.check.VM)
	}
}

func TestHeadroom_StoragePoolOverrideSelectsTheDecisionStorage(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, nil, func(c *config.CPIConfig) {
		c.VMStorage = testPinStorage
	})
	resolver, err := newLayeredResolver(map[string]any{"storage_pool": "nfs"}, rig.deps.Config)
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, resolver,
		bridgePlan{}, factsOf("pve01", "pve02", "pve03"), rig.deps.Logger)
	if plan == nil {
		t.Fatal("no plan; storage_pool nfs is shared and registers the VM")
	}
}

func TestHeadroom_OfflineNodeIsExempt(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, nil, nil)
	facts := factsOf("pve01", "pve02", "pve03")
	facts[2].Online = false
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridgePlan{},
		facts, rig.deps.Logger)
	if plan == nil {
		t.Fatal("no plan")
	}
	if !plan.exempt["pve03"] || plan.exempt["pve01"] {
		t.Errorf("exempt = %v; want only the offline pve03", plan.exempt)
	}
}

func TestHeadroom_VMMemoryComesFromTheRequest(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn, nil, nil)
	facts := factsOf("pve01", "pve02", "pve03")
	for _, tc := range []struct {
		cp   createVMCloudProps
		want int64
	}{
		{createVMCloudProps{Memory: 2048}, 2048},
		{createVMCloudProps{RAM: 1024}, 1024},
		{createVMCloudProps{}, 512},
	} {
		plan := testBuildPlan(t, rig.deps, tc.cp, nil, bridgePlan{}, facts, rig.deps.Logger)
		if plan == nil || plan.check.VM.MemBytes != tc.want*headroomMiB {
			t.Errorf("cp %+v: VM memory = %+v; want %d MiB", tc.cp, plan, tc.want)
		}
		if plan != nil && plan.check.VM.VMID != 0 {
			t.Errorf("VM ID = %d; want 0 before the ID is allocated", plan.check.VM.VMID)
		}
	}
}

func TestHeadroom_ExistingHAGuestsAreRecoveredWithinTheirPins(t *testing.T) {
	t.Parallel()
	// VM 301 (5 GiB, HA) lives on pve01 under a strict pin to pve01 and pve02.
	// pve02 holds 4 GiB of non-HA guests, so only 4 GiB are free there. Losing
	// pve01 leaves 301 nowhere, whatever pve03 has free, so the estimate is
	// short for any planned node.
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, []map[string]any{
		qemuRow(301, "pve01", 5120),
		qemuRow(302, "pve02", 4096),
	}, nil)
	rig.cluster.haResources = []map[string]any{{"sid": "vm:301", "state": "started"}}
	rig.cluster.rules = []map[string]any{{
		"rule": "bosh-na-301", "type": "node-affinity", "resources": "vm:301", "nodes": "pve01:2,pve02:1",
		"strict": 1, "affinity": "positive",
	}}
	_, _, err := rig.place(t, createVMCloudProps{})
	if err == nil || !strings.Contains(err.Error(), "VM 301") {
		t.Fatalf("placement error = %v; want a refusal naming VM 301", err)
	}
}

func TestReadHeadroomGuests(t *testing.T) {
	t.Parallel()
	nodes := []string{"pve01", "pve02", "pve03"}
	cluster := &headroomCluster{
		fullClusterAdapter: &fullClusterAdapter{},
		guests: []map[string]any{
			qemuRow(101, "pve01", 1024), // HA, strict pin to pve01 and pve02
			qemuRow(102, "pve01", 2048), // HA, no rule
			qemuRow(103, "pve02", 512),  // HA, held on its own node
			qemuRow(104, "pve02", 256),  // HA resource in the ignored state
			qemuRow(105, "pve03", 128),  // HA, negative strict rule
			qemuRow(106, "pve03", 64),   // HA, disabled rule and non-strict rule
			qemuRow(107, "pve03", 32),   // not HA
			{"type": "lxc", "vmid": 108, "node": "pve02", "maxmem": 100 * headroomMiB},
			{"type": "qemu", "vmid": 109, "node": "pve02", "maxmem": 999 * headroomMiB, "template": 1},
			{"type": "storage", "node": "pve01"},
		},
		haResources: []map[string]any{
			{"sid": "vm:101", "state": "started"}, {"sid": "vm:102", "state": "started"},
			{"sid": "vm:103", "state": "started"}, {"sid": "vm:104", "state": "ignored"},
			{"sid": "vm:105", "state": "started"}, {"sid": "vm:106", "state": "started"},
			{"sid": "ct:108"},
		},
		rules: []map[string]any{
			{"rule": "na-101", "type": "node-affinity", "resources": "vm:101", "nodes": "pve01,pve02", "strict": 1},
			{"rule": "na-103", "type": "node-affinity", "resources": "vm:103", "nodes": "pve02", "strict": 1},
			{"rule": "na-105", "type": "node-affinity", "resources": "vm:105", "nodes": "pve01", "strict": 1,
				"affinity": "negative"},
			{"rule": "na-106a", "type": "node-affinity", "resources": "vm:106", "nodes": "pve03", "strict": 1,
				"disable": 1},
			{"rule": "na-106b", "type": "node-affinity", "resources": "vm:106", "nodes": "pve03", "strict": 0},
			{"rule": "aa", "type": "resource-affinity", "resources": "vm:101,vm:102", "affinity": "negative"},
		},
	}
	got, err := readHeadroomGuests(context.Background(), cluster, nodes)
	if err != nil {
		t.Fatalf("readHeadroomGuests: %v", err)
	}
	byID := map[int]placement.HeadroomGuest{}
	for _, g := range got {
		byID[g.VMID] = g
	}
	for _, id := range []int{109} {
		if _, ok := byID[id]; ok {
			t.Errorf("VM %d is a template and must not count as a guest", id)
		}
	}
	if len(byID) != 8 {
		t.Errorf("%d guests; want 8 (seven VMs and one container)", len(byID))
	}
	want := map[int]struct {
		ha  bool
		set []string
	}{
		101: {true, []string{"pve01", "pve02"}},
		102: {true, nodes},
		103: {false, []string{"pve02"}},
		104: {false, nil},
		105: {true, []string{"pve02", "pve03"}},
		106: {true, nodes},
		107: {false, nil},
		108: {true, nodes},
	}
	for id, w := range want {
		g, ok := byID[id]
		if !ok {
			t.Errorf("guest %d missing", id)
			continue
		}
		if g.HA != w.ha {
			t.Errorf("guest %d: HA = %v; want %v", id, g.HA, w.ha)
		}
		if w.set != nil && !sameNodeSet(g.Admissible, w.set) {
			t.Errorf("guest %d: admissible = %v; want %v", id, g.Admissible, w.set)
		}
	}
	if byID[102].MemBytes != 2048*headroomMiB || byID[108].Node != "pve02" {
		t.Errorf("guest memory or node wrong: %+v %+v", byID[102], byID[108])
	}
}

func TestReadHeadroomGuests_ErrorsAreReported(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*headroomCluster){
		"resources": func(c *headroomCluster) { c.resourcesErr = errors.New("x") },
		"ha":        func(c *headroomCluster) { c.haErr = errors.New("x") },
		"rules":     func(c *headroomCluster) { c.rulesErr = errors.New("x") },
		"no node":   func(c *headroomCluster) { c.guests = []map[string]any{{"type": "qemu", "vmid": 7}} },
		"bad rule":  func(c *headroomCluster) { c.rules = []map[string]any{{"rule": ""}} },
		"bad ha":    func(c *headroomCluster) { c.haResources = []map[string]any{{"sid": " "}} },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &headroomCluster{fullClusterAdapter: &fullClusterAdapter{}}
			set(c)
			if _, err := readHeadroomGuests(context.Background(), c, []string{"pve01"}); err == nil {
				t.Error("readHeadroomGuests succeeded; want an error")
			}
		})
	}
}

func TestIsTransientRejectionReason_HeadroomPrefixes(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		placement.HeadroomShortPrefix + "ha: headroom estimate short after losing pve1: VM 4 (1 MiB) fits on no node",
		placement.HeadroomUnavailablePrefix + "ha: headroom estimate unavailable: the guests could not be read",
	} {
		if !isTransientRejectionReason(reason) {
			t.Errorf("isTransientRejectionReason(%q) = false; want true", reason)
		}
		if !classifyFilterResult(map[string]string{"pve1": reason, "pve2": "node offline"}) {
			t.Errorf("classifyFilterResult with %q = false; want retriable", reason)
		}
	}
	if classifyFilterResult(map[string]string{"pve1": placement.HeadroomShortPrefix + "x", "pve2": "missing bridge vmbr9"}) {
		t.Error("a permanent reason beside a headroom reason must stay permanent")
	}
	if isTransientRejectionReason("headroom") || isTransientRejectionReason("insufficient free storage") {
		t.Error("a near miss or a permanent reason classified as transient")
	}
}

func TestHeadroomRejected(t *testing.T) {
	t.Parallel()
	if headroomRejected(nil) || headroomRejected(map[string]string{"a": "node offline"}) {
		t.Error("headroomRejected true without a headroom reason")
	}
	if !headroomRejected(map[string]string{"a": "node offline", "b": placement.HeadroomShortPrefix + "x"}) {
		t.Error("headroomRejected false with a shortfall")
	}
	if !headroomRejected(map[string]string{"b": placement.HeadroomUnavailablePrefix + "x"}) {
		t.Error("headroomRejected false with an unavailable estimate")
	}
}

func TestNoViableCandidatesError(t *testing.T) {
	t.Parallel()
	rejections := map[string]string{"pve1": "node offline"}
	var e *cpierrors.Error
	if err := noViableCandidatesError(rejections, true); !errors.As(err, &e) || !e.OkToRetry() ||
		!strings.Contains(err.Error(), "(transient)") {
		t.Errorf("retriable error = %v", err)
	}
	if err := noViableCandidatesError(rejections, false); !errors.As(err, &e) || e.OkToRetry() ||
		strings.Contains(err.Error(), "(transient)") {
		t.Errorf("permanent error = %v", err)
	}
}

func TestHeadroomWarnChosen_NilAndEnforceAreSilent(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	(*headroomPlan)(nil).warnChosen(logger, nil, "pve01")
	enforce := &headroomPlan{check: &placement.HeadroomCheck{Mode: config.HAHeadroomEnforce}}
	enforce.warnChosen(logger, nil, "pve01")
	if len(obs.All()) != 0 {
		t.Errorf("logged %v; want nothing", obs.All())
	}
	if !enforce.enforced() || (*headroomPlan)(nil).enforced() {
		t.Error("enforced() wrong")
	}
}

func azHeadroomRig(t *testing.T, headroom string) *headroomRig {
	t.Helper()
	return newHeadroomRig(t, config.HAModeAuto, headroom, fullNonHAGuests("pve01", "pve02"),
		func(c *config.CPIConfig) {
			c.Node = "pve03"
			c.Placement = &config.PlacementConfig{
				AZMap:         map[string][]string{"zone-a": {"pve01", "pve02"}},
				AZEnforcement: "warn",
			}
			c.HA.PinAZ = haChoiceOf(config.HARuleOn)
		})
}

func TestHeadroom_EnforceInAZOrderRefusesInsteadOfFallingBack(t *testing.T) {
	t.Parallel()
	// Both nodes of zone-a are short under the strict AZ. With az_enforcement
	// warn the exhausted AZ would fall back to config.node pve03, which the
	// enforced check did not clear for this VM, so the create is refused.
	rig := azHeadroomRig(t, config.HAHeadroomEnforce)
	node, _, err := rig.place(t, createVMCloudProps{AvailabilityZone: "zone-a"})
	if err == nil {
		t.Fatalf("placement chose %q; want a refusal", node)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() || !strings.Contains(err.Error(), placement.HeadroomShortPrefix) {
		t.Errorf("error = %v; want a Retriable headroom refusal", err)
	}
}

func TestHeadroom_WarnInAZOrderPlacesAndLogs(t *testing.T) {
	t.Parallel()
	rig := azHeadroomRig(t, config.HAHeadroomWarn)
	node, _, err := rig.place(t, createVMCloudProps{AvailabilityZone: "zone-a"})
	if err != nil || (node != "pve01" && node != "pve02") {
		t.Fatalf("placement = %q, %v; want a node of zone-a", node, err)
	}
	entry, ok := hasWarn(rig.logs, "ha: headroom estimate short after losing")
	if !ok {
		t.Fatalf("no headroom warning; logs: %v", rig.logs.All())
	}
	if entry.Attrs["node"] != node {
		t.Errorf("warning names node %v; want the chosen %s", entry.Attrs["node"], node)
	}
}

// placeWith places a VM like place, with the persistent disk CIDs, cloud
// properties map, and bridge plan the caller names.
func (r *headroomRig) placeWith(
	cp createVMCloudProps, diskCIDs []string, cpMap map[string]any, bridges bridgePlan,
) (string, []string, error) {
	cp.Memory = 4096
	return resolveTargetNodeWithBridges(context.Background(), r.deps, cp, scorerGroup{}, diskCIDs, nil, cpMap, 3,
		bridges)
}

// assertPlacedWithNoCheck fails unless the placement succeeded and made no
// headroom read, which is what a VM that gets no HA must see even in enforce
// mode over a cluster with no room.
func assertPlacedWithNoCheck(t *testing.T, rig *headroomRig, node string, err error) {
	t.Helper()
	if err != nil || node == "" {
		t.Fatalf("placement = %q, %v; want the VM placed, because the real decision registers no HA for it", node, err)
	}
	if got := rig.cluster.reads(); got != 0 {
		t.Errorf("%d headroom reads; want none", got)
	}
}

func TestHeadroom_NoDirectorUUIDMeansNoCheck(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.HAHeadroomEnforce, config.HAHeadroomWarn} {
		for _, haMode := range []string{config.HAModeAuto, config.HAModeManaged} {
			t.Run(haMode+"/"+mode, func(t *testing.T) {
				t.Parallel()
				rig := newHeadroomRig(t, haMode, mode, fullNonHAGuests("pve01", "pve02", "pve03"), nil)
				rig.deps.RequestDirectorUUID = ""
				node, _, err := rig.place(t, createVMCloudProps{})
				assertPlacedWithNoCheck(t, rig, node, err)
				if *rig.profileCalls != 0 {
					t.Errorf("the HA profile was resolved %d times; want none, because the gate comes first",
						*rig.profileCalls)
				}
				if _, ok := hasWarn(rig.logs, "headroom estimate"); ok {
					t.Errorf("a headroom warning for a VM that gets no HA; logs: %v", rig.logs.All())
				}
				if !hasDebug(rig.logs, "the request carries no director_uuid") {
					t.Errorf("no Debug line naming the skipped check; logs: %v", rig.logs.All())
				}
			})
		}
	}
}

func hasDebug(obs *log.Observer, substr string) bool {
	for _, e := range obs.All() {
		if e.Level == log.LevelDebug && strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

func TestHeadroom_EveryVolumeOfTheConfigDecidesWhetherTheVMRegisters(t *testing.T) {
	t.Parallel()
	local := testPinStorage
	cases := []struct {
		name   string
		mutate func(*config.CPIConfig)
		cp     createVMCloudProps
		cpMap  map[string]any
		disks  []string
		// refuse is true when every volume is shared, so the real decision
		// registers the VM and enforce refuses it on a full cluster.
		refuse bool
	}{
		{name: "root and ISO on shared storage", refuse: true},
		{
			name: "ephemeral disk on shared storage", refuse: true,
			cp: createVMCloudProps{EphemeralDiskSizeMB: 10240, EphemeralStoragePool: "nfs"},
		},
		{
			name: "ephemeral disk on local storage",
			cp:   createVMCloudProps{EphemeralDiskSizeMB: 10240, EphemeralStoragePool: local},
		},
		{
			name:  "ephemeral disk on local storage from the layered pool",
			cp:    createVMCloudProps{EphemeralDiskSizeMB: 10240},
			cpMap: map[string]any{"ephemeral_storage_pool": local},
		},
		{
			name: "ephemeral disk falls back to a local vm_storage root", cp: createVMCloudProps{EphemeralDiskSizeMB: 10240},
			cpMap:  map[string]any{"storage_pool": "nfs"},
			mutate: func(c *config.CPIConfig) { c.VMStorage = local },
		},
		{
			name:   "config-drive ISO on local storage",
			mutate: func(c *config.CPIConfig) { c.ISOStorage = local },
		},
		{
			name: "no agent means no ISO", refuse: true,
			mutate: func(c *config.CPIConfig) { c.ISOStorage = local; c.AgentMode = config.AgentModeNoAgent },
		},
		{
			name:  "persistent disk on local storage",
			disks: []string{local + ":vm-9-disk-1"},
		},
		{
			name:  "persistent disk on shared storage",
			disks: []string{"nfs:vm-9-disk-1"}, refuse: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
				fullNonHAGuests("pve01", "pve02", "pve03"), func(c *config.CPIConfig) {
					c.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyNone)
					if tc.mutate != nil {
						tc.mutate(c)
					}
				})
			node, _, err := rig.placeWith(tc.cp, tc.disks, tc.cpMap, bridgePlan{})
			if tc.refuse {
				if err == nil {
					t.Fatalf("placement chose %q; want a refusal, because the VM registers and no node has room", node)
				}
				return
			}
			assertPlacedWithNoCheck(t, rig, node, err)
		})
	}
}

func TestHeadroom_UnknownPoolsRunNoCheckAndLogAtDebug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cp     createVMCloudProps
		cpMap  map[string]any
		disks  []string
		mutate func(*config.CPIConfig)
		want   string
	}{
		{name: "root storage tier", cpMap: map[string]any{"storage_tier": "fast"}, want: "a storage tier picks the root"},
		{
			name: "ephemeral storage tier", cp: createVMCloudProps{EphemeralDiskSizeMB: 1024},
			cpMap: map[string]any{"ephemeral_storage_tier": "fast"}, want: "picks the ephemeral disk's pool",
		},
		{
			name: "encrypted ephemeral disk", cp: createVMCloudProps{EphemeralDiskSizeMB: 1024},
			cpMap: map[string]any{"encrypted": true, "ephemeral_storage_tier": "enc"}, want: "encrypted ephemeral disk",
			mutate: func(c *config.CPIConfig) {
				enc := true
				c.StorageTiers = map[string]config.StorageTierCriteria{"enc": {Encrypted: &enc}}
			},
		},
		{name: "no root pool", mutate: func(c *config.CPIConfig) { c.VMStorage = "" }, want: "root disk's pool is not known"},
		{
			name: "no ISO pool", mutate: func(c *config.CPIConfig) { c.ISOStorage = "" },
			want: "config-drive ISO's pool is not known",
		},
		{name: "persistent disk with no pool", disks: []string{"nopool"}, want: "names no pool"},
		{
			name: "storage selectors that do not resolve", cpMap: map[string]any{"storage_set": "set-a"},
			want: "the storage selectors do not resolve",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
				fullNonHAGuests("pve01", "pve02", "pve03"), tc.mutate)
			node, _, err := rig.placeWith(tc.cp, tc.disks, tc.cpMap, bridgePlan{})
			assertPlacedWithNoCheck(t, rig, node, err)
			if !hasDebug(rig.logs, tc.want) {
				t.Errorf("no Debug line containing %q; logs: %v", tc.want, rig.logs.All())
			}
		})
	}
}

func TestHeadroom_LinkedCloneReadingNeverRefuses(t *testing.T) {
	t.Parallel()
	// The root disk is on node-local ZFS under the replicate policy. A full clone
	// registers, and a linked clone cannot be replicated, so the real decision
	// leaves the VM unregistered if create_vm clones it linked. Whether it does is
	// known only after placement, so the prediction runs no check.
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), func(c *config.CPIConfig) {
			c.VMStorage = "tank"
			c.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
		})
	profile := stagingProfile(config.HAModeAuto)
	profile.Storages = append(profile.Storages, pve.StorageInfo{Name: "tank", Type: pve.StorageTypeZFSPool, Content: "images"})
	rig.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }
	volumes := []ha.VMVolume{
		{Volume: "tank:vm-1-disk-0"}, {Volume: "nfs:iso/vm-1-config.iso", Kind: ha.VolumeISO},
	}
	for _, linked := range []bool{false, true} {
		in := ha.VMInput{Config: rig.deps.Config, VMID: 1, Node: "pve01", Volumes: slices.Clone(volumes), LinkedClone: linked}
		in.Volumes[0].LinkedClone = linked
		d, err := profile.DecideVM(in)
		if err != nil {
			t.Fatalf("linked=%v: %v", linked, err)
		}
		if d.Registers() == linked {
			t.Fatalf("linked=%v: registers = %v; the test premise is that only the full clone registers",
				linked, d.Registers())
		}
	}
	node, _, err := rig.place(t, createVMCloudProps{})
	assertPlacedWithNoCheck(t, rig, node, err)
}

func TestHeadroom_NodeWhereTheVMWouldNotRegisterIsNotRefused(t *testing.T) {
	t.Parallel()
	// pve02 lacks the bridge, and bridge_check is warn, so the filter keeps it.
	// The real decision registers the VM on pve01 and pve03 and not on pve02. All
	// three nodes are full. The VM would be refused on pve01 and pve03, where it
	// is an HA guest and has nowhere to go, and it is not refused on pve02.
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	bridges := bridgePlan{
		required: []string{"vmbr7"},
		warnOnly: true,
		checker:  func(node, _ string) (bool, error) { return node != "pve02", nil },
	}
	node, alternates, err := rig.placeWith(createVMCloudProps{}, nil, nil, bridges)
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if node != "pve02" || len(alternates) != 0 {
		t.Errorf("chose %q with alternates %v; want pve02 alone, because the VM is not an HA guest there", node, alternates)
	}
	if !hasDebug(rig.logs, "would not be an HA guest on this node") {
		t.Errorf("no Debug line for the exempt node; logs: %v", rig.logs.All())
	}
}

func TestHeadroomFilter_RestoresExemptNodesInFactOrderAndKeepsOtherRejections(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	facts := factsOf("pve01", "pve02", "pve03")
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridgePlan{}, facts, rig.deps.Logger)
	if plan == nil {
		t.Fatal("no plan")
	}
	plan.exempt = map[string]bool{"pve02": true, "pve03": true}
	// pve03 misses the PCI device, which Filter tests before headroom, so the
	// exemption must not bring it back.
	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:01:00.0"},
		PCIChecker:           func(node string) (bool, error) { return node != "pve03", nil },
	}
	pass, rejections := plan.filter(facts, req, rig.deps.Logger)
	names := make([]string, 0, len(pass))
	for _, f := range pass {
		names = append(names, f.Node)
	}
	if !slices.Equal(names, []string{"pve02"}) {
		t.Errorf("pass = %v; want pve02, restored; pve01 is short and pve03 lacks the device", names)
	}
	if reason := rejections["pve03"]; reason != "missing required PCI device" {
		t.Errorf("pve03 reason = %q; want the PCI reason", reason)
	}
	if reason := rejections["pve01"]; !strings.HasPrefix(reason, placement.HeadroomShortPrefix) {
		t.Errorf("pve01 reason = %q; want a shortfall", reason)
	}
	if _, ok := rejections["pve02"]; ok {
		t.Error("pve02 is still rejected")
	}
}

func TestHeadroom_MixedRejectionsStayPermanent(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), func(c *config.CPIConfig) { c.Node = "" })
	bridges := bridgePlan{
		required: []string{"vmbr7"},
		checker:  func(node, _ string) (bool, error) { return node != "pve03", nil },
	}
	_, _, err := rig.placeWith(createVMCloudProps{}, nil, nil, bridges)
	if err == nil {
		t.Fatal("placement succeeded; want a refusal")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Fatalf("error = %v; want a permanent Cloud error, because a missing bridge is permanent", err)
	}
	if !strings.Contains(err.Error(), "missing bridge vmbr7") || !strings.Contains(err.Error(), placement.HeadroomShortPrefix) {
		t.Errorf("error = %v; want both the bridge and the headroom reasons", err)
	}
}

func TestHeadroom_PathsThatSkipTheNodeScoringMakeNoReads(t *testing.T) {
	t.Parallel()
	t.Run("target_node", func(t *testing.T) {
		t.Parallel()
		rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
			fullNonHAGuests("pve01", "pve02", "pve03"), nil)
		node, _, err := rig.placeWith(createVMCloudProps{TargetNode: "pve02"}, nil, nil, bridgePlan{})
		assertPlacedWithNoCheck(t, rig, node, err)
		if node != "pve02" {
			t.Errorf("node = %q; want the operator's pin", node)
		}
	})
	t.Run("local disk pin", func(t *testing.T) {
		t.Parallel()
		rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
			fullNonHAGuests("pve01", "pve02", "pve03"), nil)
		rig.deps.Resolver = &staticKindResolver{sharedPools: nil}
		node, _, err := rig.placeWith(createVMCloudProps{}, []string{encodeLocalCID(t, "pve01")}, nil, bridgePlan{})
		assertPlacedWithNoCheck(t, rig, node, err)
		if node != "pve01" {
			t.Errorf("node = %q; want the local disk's node", node)
		}
	})
	t.Run("placement disabled", func(t *testing.T) {
		t.Parallel()
		deps := Deps{Config: &config.CPIConfig{
			Host: "pve.test", Node: "pve02", VMStorage: "nfs", NetworkBridge: "vmbr0",
			HA: &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto), Headroom: haChoiceOf(config.HAHeadroomEnforce)},
		}, Logger: log.NewNopLogger(), RequestDirectorUUID: "director-1"}
		node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, createVMCloudProps{}, scorerGroup{}, nil,
			nil, nil, 0, bridgePlan{})
		if err != nil || node != "pve02" {
			t.Errorf("placement = %q, %v; want config.node", node, err)
		}
	})
}

func TestHeadroom_WarnReadsOnlyAfterANodeIsChosen(t *testing.T) {
	t.Parallel()
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	if _, _, err := rig.place(t, createVMCloudProps{}); err != nil {
		t.Fatalf("placement: %v", err)
	}
	if rig.cluster.guestReads != 1 || rig.cluster.haReads != 1 || rig.cluster.ruleReads != 1 {
		t.Errorf("reads = %d/%d/%d; want one of each, made after the pick",
			rig.cluster.guestReads, rig.cluster.haReads, rig.cluster.ruleReads)
	}

	// A chosen node where the VM is no HA guest costs no read and logs nothing.
	rig = newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	facts := factsOf("pve01", "pve02", "pve03")
	plan := testBuildPlan(t, rig.deps, createVMCloudProps{Memory: 4096}, nil, bridgePlan{}, facts, rig.deps.Logger)
	plan.exempt = map[string]bool{"pve02": true}
	plan.warnChosen(rig.deps.Logger, facts, "pve02")
	if got := rig.cluster.reads(); got != 0 {
		t.Errorf("%d reads for an exempt chosen node; want none", got)
	}
	if _, ok := hasWarn(rig.logs, "headroom estimate"); ok {
		t.Errorf("a warning for an exempt chosen node; logs: %v", rig.logs.All())
	}
}

func TestHeadroom_LinesNameTheNewVMNotVMZero(t *testing.T) {
	t.Parallel()
	enforce := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	_, _, err := enforce.place(t, createVMCloudProps{})
	if err == nil {
		t.Fatal("placement succeeded; want a refusal")
	}
	if strings.Contains(err.Error(), "VM 0") || !strings.Contains(err.Error(), "the new VM (4096 MiB)") {
		t.Errorf("error = %v; want the new VM named, never VM 0", err)
	}

	warn := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn,
		fullNonHAGuests("pve01", "pve02", "pve03"), nil)
	if _, _, err := warn.place(t, createVMCloudProps{}); err != nil {
		t.Fatalf("placement: %v", err)
	}
	entry, ok := hasWarn(warn.logs, "headroom estimate")
	if !ok {
		t.Fatalf("no warning; logs: %v", warn.logs.All())
	}
	if strings.Contains(entry.Message, "VM 0") || !strings.Contains(entry.Message, "the new VM (4096 MiB)") {
		t.Errorf("warning = %q; want the new VM named, never VM 0", entry.Message)
	}
	if _, has := entry.Attrs["vmid"]; has {
		t.Errorf("warning carries vmid=%v; the VM has no ID yet", entry.Attrs["vmid"])
	}
}

func TestRelabelNewVM(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"x: VM 0 (4096 MiB) fits on no node":                 "x: the new VM (4096 MiB) fits on no node",
		"VM 0 has no configured memory":                      "the new VM has no configured memory",
		"VM 301 (5120 MiB) fits on no node, VM 100 fits too": "VM 301 (5120 MiB) fits on no node, VM 100 fits too",
		"VM 10 (1 MiB)": "VM 10 (1 MiB)",
	} {
		if got := relabelNewVM(in); got != want {
			t.Errorf("relabelNewVM(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestPlacementDiskVolume(t *testing.T) {
	t.Parallel()
	vol, ok := placementDiskVolume("nfs:vm-9-disk-1")
	if !ok || vol.Volume != "nfs:vm-9-disk-1" || !vol.Legacy || vol.HomeNode != "" {
		t.Errorf("bare CID = %+v, %v; want a legacy volume with no home node", vol, ok)
	}
	vol, ok = placementDiskVolume(encodeLocalCID(t, "pve01"))
	if !ok || vol.HomeNode != "pve01" || vol.Volume == "" {
		t.Errorf("encoded CID = %+v, %v; want the CID's volume on its home node", vol, ok)
	}
	if _, ok := placementDiskVolume("nopool"); ok {
		t.Error("a CID with no storage prefix produced a volume")
	}
}
