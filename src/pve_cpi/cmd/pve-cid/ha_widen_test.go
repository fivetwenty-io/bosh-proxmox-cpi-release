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

// widenCall is one run of the widening pass a test saw.
type widenCall struct {
	cfg    *config.CPIConfig
	owner  string
	dryRun bool
}

// withWiden makes d's widening pass record each call and answer with report
// and err.
func withWiden(d *haTestDeps, report handlers.ReplicaWidenReport, err error) *[]widenCall {
	calls := &[]widenCall{}
	d.deps.widen = func(_ context.Context, cfg *config.CPIConfig, _ *log.Logger, owner string,
		dryRun bool,
	) (handlers.ReplicaWidenReport, error) {
		*calls = append(*calls, widenCall{cfg: cfg, owner: owner, dryRun: dryRun})
		return report, err
	}
	return calls
}

func widenReport() handlers.ReplicaWidenReport {
	return handlers.ReplicaWidenReport{Results: []handlers.ReplicaWidenResult{
		{VMID: 100, Node: "pve01", Partner: "pve02", Outcome: handlers.ReplicaWidened,
			Reason: "a sync of job 100-0 at 2026-11-19T10:00:00Z covers the VM's volumes"},
		{VMID: 101, Node: "pve01", Outcome: handlers.ReplicaWidenSkipped,
			Reason: "the pin's last volume change was less than five minutes ago"},
	}, Deferred: 1}
}

func TestRunHAWidenDryRunByDefault(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	report := widenReport()
	report.Results[0].Outcome = handlers.ReplicaWouldWiden
	calls := withWiden(d, report, nil)
	code, out, errOut := runHATest(t, d, "--widen-replicas", "--director-uuid", haDirector)
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || !(*calls)[0].dryRun || (*calls)[0].owner != haDirector {
		t.Fatalf("calls = %+v; want one dry run for the named Director", *calls)
	}
	if len(d.opened) != 0 {
		t.Fatal("the widening run opened the doctor's reader")
	}
	for _, frag := range []string{
		"a dry run, so nothing was written; add --apply to widen",
		"VM 100 on pve01: would widen to pve02, because a sync of job 100-0",
		"VM 101 on pve01: skipped, because the pin's last volume change was less than five minutes ago",
		"0 widened, 1 would widen, 1 skipped, 0 failed, and 1 deferred",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}
}

func TestRunHAWidenApplyJSON(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	calls := withWiden(d, widenReport(), nil)
	code, out, errOut := runHATest(t, d, "--widen-replicas", "--apply", "--director-uuid", haDirector, "--json")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].dryRun {
		t.Fatalf("calls = %+v; want one applied run", *calls)
	}
	var got struct {
		DirectorUUID string `json:"director_uuid"`
		Applied      bool   `json:"applied"`
		Results      []handlers.ReplicaWidenResult
		Deferred     int `json:"deferred"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.DirectorUUID != haDirector || !got.Applied || got.Deferred != 1 || len(got.Results) != 2 ||
		got.Results[0].Outcome != handlers.ReplicaWidened || got.Results[1].Reason == "" {
		t.Fatalf("json = %+v", got)
	}

	d = newHATestDeps(haCfg("auto", "none"))
	withWiden(d, handlers.ReplicaWidenReport{}, nil)
	_, out, _ = runHATest(t, d, "--widen-replicas", "--director-uuid", haDirector, "--json")
	if !strings.Contains(out, `"results": []`) {
		t.Fatalf("an empty pass prints no results list:\n%s", out)
	}
}

func TestRunHAWidenOwnerFromRequest(t *testing.T) {
	t.Parallel()
	request := `{"context":{"director_uuid":"` + haDirector + `","pve_vmid_range_start":5000,"pve_vmid_range_end":5999}}`
	path := writeTemp(t, "request.json", request)
	d := newHATestDeps(haCfg("auto", "none"))
	calls := withWiden(d, handlers.ReplicaWidenReport{}, nil)
	code, out, errOut := runHATest(t, d, "--widen-replicas", "--request", path)
	if code != exitOK {
		t.Fatalf("code = %d\n%s\n%s", code, out, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].owner != haDirector || (*calls)[0].cfg.VMIDRangeStart != 5000 {
		t.Fatalf("calls = %+v; want the request's Director and cpi_config entry", *calls)
	}
	if !strings.Contains(out, "No replicate VM of this Director has a pin that names its source node alone") {
		t.Fatalf("output:\n%s", out)
	}

	d = newHATestDeps(haCfg("auto", "none"))
	calls = withWiden(d, handlers.ReplicaWidenReport{}, nil)
	code, _, errOut = runHATest(t, d, "--widen-replicas", "--request", path, "--director-uuid",
		"99999999-8888-7777-6666-555555555555")
	if code != exitUsage || len(*calls) != 0 || !strings.Contains(errOut, "different Director") {
		t.Fatalf("code = %d, calls = %d, stderr = %q", code, len(*calls), errOut)
	}
}

func TestRunHAWidenUsageErrors(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no Director":           {"--widen-replicas"},
		"apply alone":           {"--apply"},
		"director-uuid alone":   {"--director-uuid", haDirector},
		"with --cpi-config":     {"--widen-replicas", "--director-uuid", haDirector, "--cpi-config", "x.json"},
		"positional argument":   {"--widen-replicas", "--director-uuid", haDirector, "extra"},
		"apply with cpi-config": {"--widen-replicas", "--apply", "--cpi-config", "x.json"},
	} {
		t.Run(name, func(t *testing.T) {
			d := newHATestDeps(haCfg("auto", "none"))
			calls := withWiden(d, handlers.ReplicaWidenReport{}, nil)
			code, _, errOut := runHATest(t, d, args...)
			if code != exitUsage || len(*calls) != 0 || !strings.Contains(errOut, "usage: pve-cid ha") {
				t.Fatalf("code = %d, calls = %d, stderr = %q", code, len(*calls), errOut)
			}
		})
	}
}

func TestRunHAWidenFailures(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(haCfg("auto", "none"))
	withWiden(d, handlers.ReplicaWidenReport{}, errors.New("pve.ha.mode is off"))
	code, _, errOut := runHATest(t, d, "--widen-replicas", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(errOut, "the replica widening pass did not run: pve.ha.mode is off") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}

	failed := widenReport()
	failed.Results[1].Outcome = handlers.ReplicaWidenFailed
	failed.Results[1].Reason = "the HA rules write failed"
	d = newHATestDeps(haCfg("auto", "none"))
	withWiden(d, failed, nil)
	code, out, _ := runHATest(t, d, "--widen-replicas", "--apply", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(out, "VM 101 on pve01: failed, because the HA rules write failed") {
		t.Fatalf("code = %d\n%s", code, out)
	}

	halted := handlers.ReplicaWidenReport{Halted: "the token lacks Pool.Allocate"}
	d = newHATestDeps(haCfg("auto", "none"))
	withWiden(d, halted, nil)
	code, out, _ = runHATest(t, d, "--widen-replicas", "--apply", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(out, "The pass stopped early: the token lacks Pool.Allocate") {
		t.Fatalf("code = %d\n%s", code, out)
	}
}
