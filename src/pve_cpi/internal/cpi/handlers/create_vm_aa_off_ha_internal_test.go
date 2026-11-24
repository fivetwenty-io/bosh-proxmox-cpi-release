package handlers

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// aaOffHADeps returns deps under pve.ha.anti_affinity on with scored
// placement and anti-affinity enabled, and the job default pattern given,
// which "" leaves unset.
func aaOffHADeps(t *testing.T, jobDefault string) Deps {
	t.Helper()
	trueVal := true
	cfg := isoHABaseConfig("local")
	cfg.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &trueVal}}
	cfg.HA = haAntiAffinityOn()
	if jobDefault != "" {
		var p config.HAPattern
		if err := p.UnmarshalJSON([]byte(jobDefault)); err != nil {
			t.Fatalf("job default %s: %v", jobDefault, err)
		}
		cfg.HA.AntiAffinityPattern = &p
	}
	return Deps{Config: cfg, RequestDirectorUUID: "director-1"}
}

// aaOffParsed resolves the anti-affinity pattern of a create with the cloud
// properties given, the way create_vm does, for a VM of the web group.
func aaOffParsed(t *testing.T, deps Deps, cloudProps map[string]any) *createVMParsedArgs {
	t.Helper()
	parsed := &createVMParsedArgs{cloudPropsMap: cloudProps, env: isoHAGroupEnv("web")}
	if err := resolveCreateVMAntiAffinityPattern(parsed, deps.Config); err != nil {
		t.Fatalf("resolve the pattern: %v", err)
	}
	return parsed
}

// TestResolveCreateVMAntiAffinityPattern_MarksOff pins that the create's
// resolve marks the cloud properties as off exactly when the effective
// pattern is off, from the call or from the job default.
func TestResolveCreateVMAntiAffinityPattern_MarksOff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		jobDefault string
		cloudProps map[string]any
		want       bool
	}{
		"unset":                    {cloudProps: map[string]any{}},
		"call off":                 {cloudProps: map[string]any{"ha_anti_affinity": "off"}, want: true},
		"call false":               {cloudProps: map[string]any{"ha_anti_affinity": false}, want: true},
		"call block":               {cloudProps: map[string]any{"ha_anti_affinity": "block"}},
		"job off":                  {jobDefault: `"off"`, cloudProps: map[string]any{}, want: true},
		"job off, call opts in":    {jobDefault: `"off"`, cloudProps: map[string]any{"ha_anti_affinity": "block"}},
		"job block, call opts out": {jobDefault: `"block"`, cloudProps: map[string]any{"ha_anti_affinity": "off"}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parsed := aaOffParsed(t, aaOffHADeps(t, tc.jobDefault), tc.cloudProps)
			if parsed.cloudProps.antiAffinityOff != tc.want {
				t.Fatalf("antiAffinityOff = %v (pattern %q); want %v", parsed.cloudProps.antiAffinityOff,
					parsed.aaPattern, tc.want)
			}
			if got := parsed.aaPattern == "off"; got != tc.want {
				t.Fatalf("aaPattern = %q; want off %v", parsed.aaPattern, tc.want)
			}
		})
	}
}

// TestHARegistrationFeatures_OffLeavesAntiAffinityOut pins that a VM whose
// pattern is off gets no anti-affinity feature under pve.ha.anti_affinity
// on, and that a VM with block still gets it.
func TestHARegistrationFeatures_OffLeavesAntiAffinityOut(t *testing.T) {
	t.Parallel()
	deps := aaOffHADeps(t, "")
	off := aaOffParsed(t, deps, map[string]any{"ha_anti_affinity": "off"})
	if got := haRegistrationFeatures(deps, off.cloudProps, "pve01", off.env); slices.Contains(got, haFeatureAntiAffinity) {
		t.Fatalf("off: features = %v; want no anti-affinity feature", got)
	}
	block := aaOffParsed(t, deps, map[string]any{"ha_anti_affinity": "block"})
	if got := haRegistrationFeatures(deps, block.cloudProps, "pve01", block.env); !slices.Contains(got,
		haFeatureAntiAffinity) {
		t.Fatalf("block: features = %v; want the anti-affinity feature", got)
	}
}

// TestWarnRequestedHAWithoutRegistration_OffLogsNoWarning pins that a VM
// whose pattern is off logs no warning about a missing anti-affinity rule
// when the HA decision does not register it, and that a VM with block still
// does.
func TestWarnRequestedHAWithoutRegistration_OffLogsNoWarning(t *testing.T) {
	t.Parallel()
	deps := aaOffHADeps(t, "")
	d := &ha.VMDecision{VMID: 100, Reason: "local storage"}
	for name, tc := range map[string]struct {
		pattern string
		warn    bool
	}{
		"off":   {pattern: "off"},
		"block": {pattern: "block", warn: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			parsed := aaOffParsed(t, deps, map[string]any{"ha_anti_affinity": tc.pattern})
			warnRequestedHAWithoutRegistration(deps, parsed, d, capturingLogger(t, &buf))
			if got := strings.Contains(buf.String(), "no HA rule is written"); got != tc.warn {
				t.Fatalf("warning logged = %v; want %v (log %q)", got, tc.warn, buf.String())
			}
		})
	}
}

// TestStorageComputeHANodes_OffFollowsTheFeatures pins that the storage
// plan's HA node read follows haRegistrationFeatures through the same cloud
// properties, so a VM whose pattern is off and that has no other HA feature
// gets no HA nodes, while a VM with block gets them.
func TestStorageComputeHANodes_OffFollowsTheFeatures(t *testing.T) {
	t.Parallel()
	deps := aaOffHADeps(t, "")
	facts := []placement.NodeFacts{{Node: "pve01"}, {Node: "pve02"}}
	off := aaOffParsed(t, deps, map[string]any{"ha_anti_affinity": "off"})
	if got := storageComputeHANodes(deps, off, "", []string{"pve01"}, facts); len(got) != 0 {
		t.Fatalf("off: HA nodes = %v; want none", got)
	}
	block := aaOffParsed(t, deps, map[string]any{"ha_anti_affinity": "block"})
	if got := storageComputeHANodes(deps, block, "", []string{"pve01"}, facts); len(got) == 0 {
		t.Fatal("block: HA nodes = none; want the AZ's nodes")
	}
}
