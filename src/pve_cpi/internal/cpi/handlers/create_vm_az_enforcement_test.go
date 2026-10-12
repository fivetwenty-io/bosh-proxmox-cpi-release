package handlers

import (
	"bytes"
	"context"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func offlineNodeFact(name string) map[string]any {
	return map[string]any{
		"type": "node", "name": name, "online": 0,
		"maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024),
		"mem": int64(0), "cpu": 0.0,
	}
}

// azEnforcementDeps builds placement deps with a capturing logger.
func azEnforcementDeps(t *testing.T, nodes []map[string]any, cfgFn func(*config.CPIConfig)) (Deps, *bytes.Buffer) {
	t.Helper()
	deps := buildPlacementDeps(nodes, cfgFn)
	buf := &bytes.Buffer{}
	logger, err := log.NewLogger("debug", buf)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	deps.Logger = logger
	return deps, buf
}

func strictPlacement(az map[string][]string) func(*config.CPIConfig) {
	return func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{AZMap: az, AZEnforcement: "strict"}
	}
}

func warnPlacement(az map[string][]string) func(*config.CPIConfig) {
	return func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{AZMap: az}
	}
}

func asCPIError(t *testing.T, err error) *cpierrors.Error {
	t.Helper()
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error; got %T: %v", err, err)
	}
	return cpiErr
}

func TestResolveTargetNodeWithRNG_AZExhausted_StrictFailsNamingAZAndReasons(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), offlineNodeFact("pve2"), onlineNode("pve9")},
		strictPlacement(map[string][]string{"z1": {"pve1", "pve2"}}))
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
	if err == nil {
		t.Fatalf("strict must fail instead of placing on config.node; got node %q", node)
	}
	for _, want := range []string{`availability_zone "z1"`, "pve1, pve2", "pve1: node offline", "pve2: node offline", "az_enforcement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if !asCPIError(t, err).OkToRetry() {
		t.Error("offline nodes are transient; the strict error must be retriable")
	}
}

func TestResolveTargetNodeWithRNG_AZExhausted_DefaultWarnFallsBackToConfigNode(t *testing.T) {
	t.Parallel()
	deps, buf := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), onlineNode("pve9")},
		warnPlacement(map[string][]string{"z1": {"pve1"}}))
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	node, alts, err := resolveTargetNodeWithFallbacks(context.Background(), deps, cp, "", nil, nil, nil, 2)
	if err != nil {
		t.Fatalf("default must keep the 0.9.3 fallback: %v", err)
	}
	if node != "pve9" || alts != nil {
		t.Errorf("node = %q alternates = %v, want config.node pve9 with no alternates", node, alts)
	}
	out := buf.String()
	for _, want := range []string{"falling back to config.node", `availability_zone \"z1\"`, "pve1: node offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("Warn output missing %q:\n%s", want, out)
		}
	}
}

func TestResolveTargetNodeWithRNG_OneNodeAZInMaintenance_StrictRetriableWarnFallsBack(t *testing.T) {
	t.Parallel()
	nodes := []map[string]any{offlineNodeFact("pve1"), onlineNode("pve9")}
	az := map[string][]string{"z1": {"pve1"}}
	cp := createVMCloudProps{AvailabilityZone: "z1"}

	strict, _ := azEnforcementDeps(t, nodes, strictPlacement(az))
	_, err := resolveTargetNodeWithRNG(context.Background(), strict, cp, "", nil, nil, nil)
	if err == nil || !asCPIError(t, err).OkToRetry() {
		t.Fatalf("strict one-node AZ down: want retriable error, got %v", err)
	}

	warn, _ := azEnforcementDeps(t, nodes, warnPlacement(az))
	node, err := resolveTargetNodeWithRNG(context.Background(), warn, cp, "", nil, nil, nil)
	if err != nil || node != "pve9" {
		t.Fatalf("warn one-node AZ down: node=%q err=%v, want pve9 and no error", node, err)
	}
}

func TestResolveTargetNodeWithRNG_AZMapTypoNode_ReasonNamesMissingNode(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), onlineNode("pve9")},
		strictPlacement(map[string][]string{"z1": {"pve1", "pve-typo"}}))
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{AvailabilityZone: "z1"}, "", nil, nil, nil)
	if err == nil {
		t.Fatal("expected strict failure")
	}
	if !strings.Contains(err.Error(), "pve-typo: node not found in the cluster") {
		t.Errorf("error %q should name the missing node", err.Error())
	}
	if asCPIError(t, err).OkToRetry() {
		t.Error("an az_map typo is permanent; the error must not be retriable")
	}
}

func TestResolveTargetNodeWithRNG_PerAZReasonsNotMaskedByFirstWins(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), offlineNodeFact("pve2"), onlineNode("pve9")},
		strictPlacement(map[string][]string{"z1": {"pve1"}, "z2": {"pve2"}}))
	cp := createVMCloudProps{AvailabilityZones: []string{"z1", "z2"}}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
	if err == nil {
		t.Fatal("expected strict failure")
	}
	if !strings.Contains(err.Error(), "pve2: node offline") {
		t.Errorf("z2's own reason was masked by z1's filter pass: %q", err.Error())
	}
	if strings.Contains(err.Error(), "not in candidate node set") {
		t.Errorf("out-of-AZ scope reasons must not appear: %q", err.Error())
	}
}

func TestAZRecordsRetriable_PermanentReasonInOneAZ(t *testing.T) {
	t.Parallel()
	inCluster := map[string]struct{}{"pve1": {}, "pve2": {}}
	transient := newAZRejectionRecord("z1", []string{"pve1"}, map[string]string{"pve1": "node offline", "pve2": "not in candidate node set"}, inCluster)
	permanent := newAZRejectionRecord("z2", []string{"pve2"}, map[string]string{"pve2": "insufficient free storage", "pve1": "not in candidate node set"}, inCluster)
	if !azRecordsRetriable([]azRejectionRecord{transient}) {
		t.Error("transient-only record must be retriable")
	}
	if azRecordsRetriable([]azRejectionRecord{transient, permanent}) {
		t.Error("one AZ with a permanent reason must make the create non-retriable")
	}
	if _, leaked := transient.Reasons["pve2"]; leaked {
		t.Error("a node outside the AZ's list must not carry a reason in that AZ's record")
	}
}

func TestResolveTargetNodeWithRNG_NoAZ_KeepsConfigNodeFallback(t *testing.T) {
	t.Parallel()
	deps, buf := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1")},
		strictPlacement(map[string][]string{"z1": {"pve1"}}))
	node, alts, err := resolveTargetNodeWithFallbacks(context.Background(), deps, createVMCloudProps{}, "", nil, nil, nil, 2)
	if err != nil || node != "pve9" || alts != nil {
		t.Fatalf("no AZ requested: node=%q alternates=%v err=%v, want config.node pve9 even under strict", node, alts, err)
	}
	if !strings.Contains(buf.String(), "pve1: node offline") {
		t.Errorf("the no-AZ Warn should now carry the rejection reasons:\n%s", buf.String())
	}
}

func TestResolveTargetNodeWithRNG_OnlyDLBSentinel_KeepsConfigNodeFallback(t *testing.T) {
	t.Parallel()
	deps, buf := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1")},
		strictPlacement(map[string][]string{"z1": {"pve1"}}))
	node, alts, err := resolveTargetNodeWithFallbacks(context.Background(), deps, createVMCloudProps{AvailabilityZone: "dlb"}, "", nil, nil, nil, 2)
	if err != nil || node != "pve9" || alts != nil {
		t.Fatalf("DLB sentinel only: node=%q alternates=%v err=%v, want config.node pve9 even under strict", node, alts, err)
	}
	if !strings.Contains(buf.String(), "falling back to config.node") || strings.Contains(buf.String(), "requested availability zones") {
		t.Errorf("sentinel-only order must take the plain fallback Warn, not the AZ violation Warn:\n%s", buf.String())
	}
}

func TestResolveTargetNodeWithRNG_MixedSentinelAndRealAZ_EnforcesRealAZ(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1")},
		strictPlacement(map[string][]string{"z1": {"pve1"}}))
	cp := createVMCloudProps{AvailabilityZones: []string{"dlb", "z1"}}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `availability_zone "z1"`) {
		t.Fatalf("mixed order must enforce the real AZ; got %v", err)
	}
}

func TestOnlyDLBSentinel(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	cases := []struct {
		order []string
		want  bool
	}{
		{nil, false},
		{[]string{"dlb"}, true},
		{[]string{"dlb", "dlb"}, true},
		{[]string{"dlb", "z1"}, false},
		{[]string{"z1"}, false},
	}
	for _, tc := range cases {
		if got := onlyDLBSentinel(tc.order, cfg); got != tc.want {
			t.Errorf("onlyDLBSentinel(%v) = %v, want %v", tc.order, got, tc.want)
		}
	}
}

func TestResolveTargetNodeWithRNG_AZFromVMTypeProfile_ResolvedBeforeBranch1(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), onlineNode("pve9")},
		func(c *config.CPIConfig) {
			strictPlacement(map[string][]string{"z1": {"pve1"}})(c)
			c.VMTypes = map[string]config.TypeProfile{
				"small": {CloudProperties: map[string]any{"availability_zone": "z1"}},
			}
		})
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", nil, nil,
		map[string]any{"vm_type": "small"})
	if err == nil || !strings.Contains(err.Error(), `availability_zone "z1"`) {
		t.Fatalf("an AZ from the vm_type profile must be enforced; got %v", err)
	}

	warn, buf := azEnforcementDeps(t,
		[]map[string]any{offlineNodeFact("pve1"), onlineNode("pve9")},
		func(c *config.CPIConfig) {
			warnPlacement(map[string][]string{"z1": {"pve1"}})(c)
			c.VMTypes = map[string]config.TypeProfile{
				"small": {CloudProperties: map[string]any{"availability_zone": "z1"}},
			}
		})
	node, err := resolveTargetNodeWithRNG(context.Background(), warn, createVMCloudProps{}, "", nil, nil,
		map[string]any{"vm_type": "small"})
	if err != nil || node != "pve9" || !strings.Contains(buf.String(), "pve1: node offline") {
		t.Fatalf("warn with a profile AZ: node=%q err=%v, want pve9 and a Warn naming pve1", node, err)
	}
}

func TestResolveTargetNodeWithRNG_TargetNodeIgnoresBadVMType(t *testing.T) {
	t.Parallel()
	deps, _ := azEnforcementDeps(t, []map[string]any{onlineNode("pve1")}, warnPlacement(map[string][]string{"z1": {"pve1"}}))
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{TargetNode: "pve1"}, "", nil, nil,
		map[string]any{"vm_type": "no-such-profile"})
	if err != nil || node != "pve1" {
		t.Fatalf("an operator pin never needed the resolver; node=%q err=%v", node, err)
	}
}

func TestResolveTargetNodeWithRNG_PlacementDisabled_WarnsWhenAZRequested(t *testing.T) {
	t.Parallel()
	off := false
	deps, buf := azEnforcementDeps(t, []map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{Enabled: &off, AZMap: map[string][]string{"z1": {"pve1"}}, AZEnforcement: "strict"}
	})
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{AvailabilityZone: "z1"}, "", nil, nil, nil)
	if err != nil || node != "pve9" {
		t.Fatalf("disabled placement must not fail: node=%q err=%v", node, err)
	}
	if !strings.Contains(buf.String(), "availability zone is ignored") || !strings.Contains(buf.String(), "z1") {
		t.Errorf("expected the ignored-AZ Warn naming z1:\n%s", buf.String())
	}

	quiet, quietBuf := azEnforcementDeps(t, []map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{Enabled: &off, AZMap: map[string][]string{"z1": {"pve1"}}}
	})
	if _, err := resolveTargetNodeWithRNG(context.Background(), quiet, createVMCloudProps{}, "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quietBuf.String(), "ignored") {
		t.Errorf("no AZ requested, so no ignored-AZ Warn expected:\n%s", quietBuf.String())
	}
}

func TestResolveTargetNodeWithRNG_EmptyConfigNode_WarnKeepsFirstWinsClass(t *testing.T) {
	t.Parallel()
	// z1 lists pve1 (offline, transient) and a typo node. Main classified the
	// first-wins merge, which never sees the typo node, so the class at warn is
	// Retriable. The strict error keeps the per-AZ view and is not retriable.
	nodes := []map[string]any{offlineNodeFact("pve1")}
	az := map[string][]string{"z1": {"pve1", "pve-typo"}}
	cp := createVMCloudProps{AvailabilityZone: "z1"}

	warn, _ := azEnforcementDeps(t, nodes, func(c *config.CPIConfig) {
		warnPlacement(az)(c)
		c.Node = ""
	})
	_, err := resolveTargetNodeWithRNG(context.Background(), warn, cp, "", nil, nil, nil)
	if err == nil || !asCPIError(t, err).OkToRetry() {
		t.Fatalf("warn with empty config.node must keep the 0.9.3 Retriable class; got %v", err)
	}

	strict, _ := azEnforcementDeps(t, nodes, func(c *config.CPIConfig) {
		strictPlacement(az)(c)
		c.Node = ""
	})
	_, err = resolveTargetNodeWithRNG(context.Background(), strict, cp, "", nil, nil, nil)
	if err == nil || asCPIError(t, err).OkToRetry() {
		t.Fatalf("strict with a typo node must be non-retriable; got %v", err)
	}
}

func TestResolveTargetNodeWithRNG_OneNodeAZInMaintenanceTag_StrictRetriableWarnFallsBack(t *testing.T) {
	t.Parallel()
	az := map[string][]string{"z1": {"pve1"}}
	cp := createVMCloudProps{AvailabilityZone: "z1"}
	withTag := func(mode func(map[string][]string) func(*config.CPIConfig)) func(*config.CPIConfig) {
		return func(c *config.CPIConfig) {
			mode(az)(c)
			c.Placement.MaintenanceNodeTags = []string{"maint"}
		}
	}
	tagged := []map[string]any{onlineNode("pve1"), onlineNode("pve9")}
	tagged[0]["tags"] = "maint"

	strict, _ := azEnforcementDeps(t, tagged, withTag(strictPlacement))
	_, err := resolveTargetNodeWithRNG(context.Background(), strict, cp, "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "pve1: node in maintenance") || !asCPIError(t, err).OkToRetry() {
		t.Fatalf("strict: want retriable maintenance error, got %v", err)
	}
	warn, _ := azEnforcementDeps(t, tagged, withTag(warnPlacement))
	node, err := resolveTargetNodeWithRNG(context.Background(), warn, cp, "", nil, nil, nil)
	if err != nil || node != "pve9" {
		t.Fatalf("warn: node=%q err=%v, want pve9", node, err)
	}
}

func observeWithPlacementDisabled(t *testing.T, cp createVMCloudProps) (nodes []string, planErr error, logs string) {
	t.Helper()
	off := false
	deps, buf := azEnforcementDeps(t, []map[string]any{onlineNode("pve1"), onlineNode("pve9")}, func(c *config.CPIConfig) {
		c.Node = "pve9"
		c.Placement = &config.PlacementConfig{
			Enabled: &off,
			AZMap:   map[string][]string{"z1": {"pve1", "pve9"}},
		}
	})
	groups, err := ObserveStoragePlanCompute(context.Background(), deps, &createVMParsedArgs{cloudProps: cp}, nil)
	for _, g := range groups {
		nodes = append(nodes, g.Nodes...)
	}
	return nodes, err, buf.String()
}

func TestObserveStoragePlanCompute_PlacementDisabled_WarnsAndPinsConfigNode(t *testing.T) {
	t.Parallel()
	nodes, err, logs := observeWithPlacementDisabled(t, createVMCloudProps{AvailabilityZone: "z1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 1 || nodes[0] != "pve9" {
		t.Errorf("plan nodes = %v, want only the config.node pin pve9", nodes)
	}
	if !strings.Contains(logs, "availability zone is ignored") || !strings.Contains(logs, "z1") {
		t.Errorf("expected the ignored-AZ Warn naming z1:\n%s", logs)
	}
}

func TestObserveStoragePlanCompute_PlacementDisabled_NoWarnWithoutMappedAZOrWithTargetNode(t *testing.T) {
	t.Parallel()
	_, _, logs := observeWithPlacementDisabled(t, createVMCloudProps{AvailabilityZone: "unmapped"})
	if strings.Contains(logs, "availability zone is ignored") {
		t.Errorf("an AZ absent from az_map must not warn:\n%s", logs)
	}
	_, _, logs = observeWithPlacementDisabled(t, createVMCloudProps{AvailabilityZone: "z1", TargetNode: "pve1"})
	if strings.Contains(logs, "availability zone is ignored") {
		t.Errorf("an operator target_node pin must not warn:\n%s", logs)
	}
}
