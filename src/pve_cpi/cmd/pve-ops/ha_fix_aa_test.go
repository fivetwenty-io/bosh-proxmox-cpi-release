package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// fixAACall is one run of the anti-affinity fix a test saw.
type fixAACall struct {
	owner  string
	rule   string
	dryRun bool
}

// withFixAA makes d's anti-affinity fix record each call and answer with
// results and err.
func withFixAA(d *haTestDeps, results []handlers.AntiAffinityFixResult, err error) *[]fixAACall {
	calls := &[]fixAACall{}
	d.deps.fixAA = func(_ context.Context, _ *config.CPIConfig, _ *log.Logger, owner, rule string,
		dryRun bool,
	) ([]handlers.AntiAffinityFixResult, error) {
		*calls = append(*calls, fixAACall{owner: owner, rule: rule, dryRun: dryRun})
		return results, err
	}
	return calls
}

func fixAAResults(outcome string) []handlers.AntiAffinityFixResult {
	return []handlers.AntiAffinityFixResult{{
		AntiAffinityGroupCheck: handlers.AntiAffinityGroupCheck{
			Rule: "bosh-aa-0123456789ab", Group: "web", Deployment: "cf", Members: 4, Pattern: "block",
			Drift: []string{"update bosh-aa-0123456789ab with vm:100,vm:101,vm:102"},
		},
		Outcome: outcome,
	}}
}

func TestRunHAFixAADryRunByDefault(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	calls := withFixAA(d, fixAAResults(handlers.AntiAffinityWouldFix), nil)
	code, out, errOut := runHATest(t, d, "--fix-anti-affinity", "--director-uuid", haDirector)
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || !(*calls)[0].dryRun || (*calls)[0].owner != haDirector || (*calls)[0].rule != "" {
		t.Fatalf("calls = %+v; want one dry run for the Director", *calls)
	}
	if len(d.opened) != 0 {
		t.Fatal("the fix opened the doctor's reader")
	}
	for _, s := range []string{
		"a dry run, so nothing was written; add --apply to write",
		"Group web of deployment cf (bosh-aa-0123456789ab), 4 members, pattern block: the rules differ",
		"update bosh-aa-0123456789ab with vm:100,vm:101,vm:102",
		"0 in layout, 1 would be fixed, 0 fixed, 0 failed, and 0 not planned",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestRunHAFixAAApplyOneGroupAsJSON(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	calls := withFixAA(d, fixAAResults(handlers.AntiAffinityFixed), nil)
	code, out, errOut := runHATest(t, d, "--fix-anti-affinity", "--apply", "--group", "bosh-aa-0123456789ab",
		"--director-uuid", haDirector, "--json")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].dryRun || (*calls)[0].rule != "bosh-aa-0123456789ab" {
		t.Fatalf("calls = %+v; want one applied run of the group", *calls)
	}
	var got haFixAAOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !got.Applied || got.DirectorUUID != haDirector || len(got.Groups) != 1 ||
		got.Groups[0].Outcome != handlers.AntiAffinityFixed || got.Groups[0].Rule != "bosh-aa-0123456789ab" {
		t.Fatalf("output = %+v", got)
	}
}

func TestRunHAFixAAFailures(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	withFixAA(d, nil, errors.New("the settings write no anti-affinity rule"))
	code, _, errOut := runHATest(t, d, "--fix-anti-affinity", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(errOut, "the anti-affinity fix did not run") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	for _, outcome := range []string{handlers.AntiAffinityFailed, handlers.AntiAffinityProblem} {
		d = newHATestDeps(haCfg("auto", "none"))
		withFixAA(d, fixAAResults(outcome), nil)
		if code, out, _ := runHATest(t, d, "--fix-anti-affinity", "--apply", "--director-uuid",
			haDirector); code != exitError {
			t.Fatalf("%s: code = %d\n%s", outcome, code, out)
		}
	}
}

func TestRunHAFixAAUsageErrors(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no Director":       {"--fix-anti-affinity"},
		"group alone":       {"--group", "bosh-aa-0123456789ab"},
		"with widen":        {"--fix-anti-affinity", "--widen-replicas", "--director-uuid", haDirector},
		"with --cpi-config": {"--fix-anti-affinity", "--director-uuid", haDirector, "--cpi-config", "x.json"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newHATestDeps(haCfg("auto", "none"))
			calls := withFixAA(d, nil, nil)
			code, _, errOut := runHATest(t, d, args...)
			if code != exitUsage || len(*calls) != 0 || !strings.Contains(errOut, "usage: pve-ops ha") {
				t.Fatalf("code = %d, calls = %d, stderr = %q", code, len(*calls), errOut)
			}
		})
	}
}
