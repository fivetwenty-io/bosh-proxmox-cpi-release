package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// drainCalls are the runs of the drain and the undrain a test saw.
type drainCalls struct {
	owners  []string
	drain   []handlers.DrainOptions
	undrain []handlers.UndrainOptions
}

// withDrain makes d's drain and undrain record each call and answer with
// report and err.
func withDrain(d *haTestDeps, report handlers.DrainReport, err error) *drainCalls {
	calls := &drainCalls{}
	d.deps.drain = func(_ context.Context, _ *config.CPIConfig, _ *log.Logger, owner string,
		opts handlers.DrainOptions,
	) (handlers.DrainReport, error) {
		calls.owners = append(calls.owners, owner)
		calls.drain = append(calls.drain, opts)
		return report, err
	}
	d.deps.undrain = func(_ context.Context, _ *config.CPIConfig, _ *log.Logger, owner string,
		opts handlers.UndrainOptions,
	) (handlers.DrainReport, error) {
		calls.owners = append(calls.owners, owner)
		calls.undrain = append(calls.undrain, opts)
		return report, err
	}
	return calls
}

func drainDryRun() handlers.DrainReport {
	return handlers.DrainReport{
		Command: handlers.DrainCommand, Cluster: "lab", Node: "pve1",
		Gates: []handlers.RebalanceGate{{Name: handlers.RebalanceGateMaintenance, OK: true,
			Detail: "for --apply, the HA manager doesn't report node pve1 in maintenance"}},
		Warnings: []string{},
		VMs: []handlers.DrainVM{
			{VMID: 101, Name: "vm-a", Node: "pve1", Fate: handlers.DrainLeaves, Targets: []string{"pve2", "pve3"}},
			{VMID: 102, Name: "vm-b", Node: "pve1", Deployment: "cf", Fate: handlers.DrainStays,
				Reason:  "its strict node-affinity rule bosh-na-102 names no other node that can take it",
				Outcome: handlers.DrainOutcomePlanned},
			{VMID: 103, Node: "pve1", Fate: handlers.DrainNotHA, Reason: "it has no HA resource"},
		},
		Deployments: []string{"cf"},
		Reminder:    "Turn resurrection off for `cf` until `pve-ops ha undrain pve1 --apply` finishes.",
	}
}

func TestRunHADrainDryRunText(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	calls := withDrain(d, drainDryRun(), nil)
	code, out, errOut := runHATest(t, d, "drain", "pve1", "--director-uuid", haDirector, "--config", "/x/cpi.json")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	want := handlers.DrainOptions{Node: "pve1", SettleTimeout: 30 * time.Minute}
	if len(calls.drain) != 1 || calls.drain[0] != want || calls.owners[0] != haDirector {
		t.Fatalf("calls = %+v, want one dry run with %+v", calls, want)
	}
	for _, frag := range []string{
		"HA drain of node pve1 for Director " + haDirector + " on cluster lab (token bosh@pve!****), a dry run, " +
			"so nothing stopped; add --apply to stop the VMs that can't leave\n",
		"Gates\n  for --apply, the HA manager doesn't report node pve1 in maintenance\n",
		"VMs on pve1\n  VM 101 (vm-a), leaves for pve2 or pve3\n",
		"  VM 102 (vm-b), can't leave, because its strict node-affinity rule bosh-na-102 names no other node " +
			"that can take it: stop planned\n",
		"  VM 103, stays, because it has no HA resource\n",
		"1 VM can leave, 1 VM can't, and 0 stopped\n",
		"Turn resurrection off for `cf`",
	} {
		if !strings.Contains(out, frag) {
			t.Fatalf("output lacks %q:\n%s", frag, out)
		}
	}
	if strings.Contains(out, "5ecret") {
		t.Fatal("the output shows the token's secret")
	}
}

func TestRunHADrainTakesTheNodeAfterTheFlagsAndPrintsJSON(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	rep := drainDryRun()
	rep.Applied, rep.ExitCode, rep.Halted = true, handlers.RebalanceExitGate, handlers.RebalanceGateMaintenance
	calls := withDrain(d, rep, nil)
	code, out, _ := runHATest(t, d, "drain", "--apply", "--settle-timeout", "45m", "--json", "--director-uuid",
		haDirector, "pve1")
	if code != exitGate {
		t.Fatalf("code = %d, want the report's", code)
	}
	if want := (handlers.DrainOptions{Node: "pve1", Apply: true, SettleTimeout: 45 * time.Minute}); calls.drain[0] !=
		want {
		t.Fatalf("options = %+v, want %+v", calls.drain[0], want)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got["command"] != "drain" || got["node"] != "pve1" || got["exit_code"] != float64(exitGate) ||
		got["token"] != "bosh@pve!****" || got["director_uuid"] != haDirector {
		t.Fatalf("json = %v", got)
	}
}

func TestRunHAUndrainPassesTheRebalanceOptions(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	rep := handlers.DrainReport{Command: handlers.UndrainCommand, Node: "pve1", Applied: true,
		VMs:       []handlers.DrainVM{{VMID: 102, Node: "pve1", Outcome: handlers.DrainOutcomeStarted}},
		Rebalance: &handlers.RebalanceReport{Applied: true, Cluster: "lab"}}
	calls := withDrain(d, rep, nil)
	code, out, errOut := runHATest(t, d, "undrain", "pve1", "--apply", "--director-uuid", haDirector,
		"--max-moves", "3", "--restore-only")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	o := calls.undrain[0]
	if o.Node != "pve1" || !o.Apply || o.SettleTimeout != 10*time.Minute || o.NoRebalance ||
		o.Rebalance.MaxMoves != 3 || !o.Rebalance.RestoreOnly || o.Rebalance.MaxDuration != time.Hour {
		t.Fatalf("options = %+v", o)
	}
	for _, frag := range []string{
		"HA undrain of node pve1 for Director " + haDirector + " on cluster (unnamed)",
		"VMs the drain of pve1 stopped\n  VM 102 on pve1, started\n",
		"1 VM to start, and 1 started\n",
		"HA rebalance for Director " + haDirector + " on cluster lab",
	} {
		if !strings.Contains(out, frag) {
			t.Fatalf("output lacks %q:\n%s", frag, out)
		}
	}
	_, _, _ = runHATest(t, d, "undrain", "--no-rebalance", "pve1", "--director-uuid", haDirector)
	if !calls.undrain[1].NoRebalance || calls.undrain[1].Apply {
		t.Fatalf("options = %+v", calls.undrain[1])
	}
}

func TestRunHADrainUsageErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"drain", "--director-uuid", haDirector}, "name exactly one node"},
		{[]string{"drain", "pve1", "pve2", "--director-uuid", haDirector}, "name exactly one node"},
		{[]string{"drain", "pve1", "--settle-timeout", "30s", "--director-uuid", haDirector},
			"--settle-timeout must be at least 1m"},
		{[]string{"drain", "pve1", "--widen-replicas"}, "--widen-replicas does not go with drain"},
		{[]string{"undrain", "pve1", "--cpi-config", "x.json"}, "--cpi-config does not go with undrain"},
		{[]string{"undrain", "pve1", "--max-moves", "0", "--director-uuid", haDirector},
			"--max-moves must be at least 1"},
		{[]string{"drain", "pve1", "--max-moves", "3", "--director-uuid", haDirector}, "flag provided but not defined"},
		{[]string{"drain", "pve1"}, "the Director's UUID must come from --request or --director-uuid"},
	} {
		d := newHATestDeps(rbCfg())
		calls := withDrain(d, handlers.DrainReport{}, nil)
		code, _, errOut := runHATest(t, d, tc.args...)
		if code != exitUsage || !strings.Contains(errOut, tc.want) || len(calls.owners) != 0 {
			t.Fatalf("%v: code = %d, stderr = %q, want %q and no run", tc.args, code, errOut, tc.want)
		}
	}
}

func TestRunHADrainARunThatDoesNotStartExits1(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	withDrain(d, handlers.DrainReport{}, errors.New("no PVE client"))
	code, _, errOut := runHATest(t, d, "drain", "pve1", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(errOut, "pve-ops ha drain: the run did not start: no PVE client") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunHADrainHelp(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	code, out, _ := runHATest(t, d, "undrain", "--help")
	if code != exitOK || !strings.Contains(out, haUndrainUsage) || !strings.Contains(out, "-no-rebalance") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}
