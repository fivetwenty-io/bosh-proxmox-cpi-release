package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// rebalanceCall is one run of the rebalance a test saw.
type rebalanceCall struct {
	cfg   *config.CPIConfig
	owner string
	opts  handlers.RebalanceOptions
}

// withRebalance makes d's rebalance record each call and answer with report
// and err.
func withRebalance(d *haTestDeps, report handlers.RebalanceReport, err error) *[]rebalanceCall {
	calls := &[]rebalanceCall{}
	d.deps.rebalance = func(_ context.Context, cfg *config.CPIConfig, _ *log.Logger, owner string,
		opts handlers.RebalanceOptions,
	) (handlers.RebalanceReport, error) {
		*calls = append(*calls, rebalanceCall{cfg: cfg, owner: owner, opts: opts})
		return report, err
	}
	return calls
}

// rbCfg is haCfg with an API token whose secret the output must never show.
func rbCfg() *config.CPIConfig {
	cfg := haCfg("auto", "none")
	cfg.APIToken = "bosh@pve!cpi=5ecret-uuid"
	return cfg
}

func rebalanceDryRun() handlers.RebalanceReport {
	return handlers.RebalanceReport{
		Cluster: "lab",
		Gates: []handlers.RebalanceGate{
			{Name: handlers.RebalanceGateQuorum, OK: true, Detail: "quorum is held"},
			{Name: handlers.RebalanceGateJournal, OK: true, Detail: "the allocation journal holds no open record"},
		},
		Warnings: []string{},
		Nodes: []handlers.RebalanceNode{
			{Node: "pve1", CommittedBefore: 0.22, CommittedAfter: 0.58, GuestsBefore: 3, GuestsAfter: 6},
			{Node: "pve2", CommittedBefore: 0.81, CommittedAfter: 0.57, GuestsBefore: 6, GuestsAfter: 4},
		},
		Resumed: []handlers.RebalanceResumed{},
		Moves: []handlers.RebalanceMove{{VMID: 1042, Name: "vm-3f2a9c1e", From: "pve2", To: "pve1",
			Phase: harebalance.PhaseRestore, Reason: "its AZ z1 is pve1 and it stayed on pve2 after maintenance",
			Outcome: handlers.RebalancePlanned}},
		Skipped: []handlers.RebalanceSkip{{VMID: 1060, Name: "vm-0aa1e7c3", Node: "pve3",
			Kind: harebalance.SkipReplicate, Reason: "it is a replicate VM, and a move would reverse its replication job"}},
		SpreadBefore: 0.59, SpreadAfter: 0.02,
	}
}

func TestRunHARebalanceDryRunText(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	calls := withRebalance(d, rebalanceDryRun(), nil)
	code, out, errOut := runHATest(t, d, "rebalance", "--director-uuid", " "+haDirector+" ", "--config", "/x/cpi.json")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].owner != haDirector || (*calls)[0].opts.Apply {
		t.Fatalf("calls = %+v; want one dry run for the named Director", *calls)
	}
	want := handlers.RebalanceOptions{MaxMoves: 10, SettleTimeout: 10 * time.Minute, MaxDuration: time.Hour,
		StartThreshold: 0.15, StopThreshold: 0.05}
	if (*calls)[0].opts != want {
		t.Fatalf("options = %+v, want the defaults %+v", (*calls)[0].opts, want)
	}
	for _, frag := range []string{
		"HA rebalance for Director " + haDirector + " on cluster lab (token bosh@pve!****), a dry run, so " +
			"nothing moved; add --apply to move\n",
		"pve-ops " + cliVersion + " with the configuration /x/cpi.json\n",
		"Gates\n  quorum is held\n  the allocation journal holds no open record\n",
		"Nodes, memory committed now and after the plan\n  pve1   22%   58%\n  pve2   81%   57%\n",
		"Moves\n  1. VM 1042 (vm-3f2a9c1e) from pve2 to pve1, because its AZ z1 is pve1 and it stayed on pve2 " +
			"after maintenance\n",
		"Skipped\n  VM 1060 (vm-0aa1e7c3) on pve3, because it is a replicate VM",
		"1 move planned, 0 made, 1 VM skipped, and 0 deferred, and the spread goes from 59 points to 2\n",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}
	if strings.Contains(out, "5ecret") || strings.Contains(out, "!cpi") {
		t.Fatalf("output shows the token:\n%s", out)
	}
}

func TestRunHARebalanceApplyPassesTheOptionsAndPrintsEachMove(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	report := rebalanceDryRun()
	report.Applied = true
	report.Planned = report.Moves
	report.Moves = []handlers.RebalanceMove{report.Planned[0], {VMID: 1077, From: "pve2", To: "pve3",
		Reason: "the move lowers the spread", Outcome: handlers.RebalanceLeftPending, Seconds: 600,
		Error: "the VM had not settled on pve3"}}
	report.Moves[0].Outcome, report.Moves[0].Seconds = handlers.RebalanceMoved, 41.2
	report.Resumed = []handlers.RebalanceResumed{{VMID: 1001, Node: "pve1", Target: "pve1", Action: "finish",
		Outcome: handlers.RebalanceFinished}}
	report.Warnings = []string{"the cluster's CRS auto-rebalance is on"}
	report.DiffersFromPlan, report.Halted, report.ExitCode = true, "VM 1077 was left part way", 1
	calls := withRebalance(d, report, nil)
	code, out, _ := runHATest(t, d, "rebalance", "--apply", "--director-uuid", haDirector, "--max-moves", "2",
		"--settle-timeout", "5m", "--max-duration", "30m", "--start-threshold", "0.2", "--stop-threshold", "0.1",
		"--restore-only")
	if code != exitError {
		t.Fatalf("code = %d, want the report's exit code 1", code)
	}
	want := handlers.RebalanceOptions{Apply: true, MaxMoves: 2, SettleTimeout: 5 * time.Minute,
		MaxDuration: 30 * time.Minute, StartThreshold: 0.2, StopThreshold: 0.1, RestoreOnly: true}
	if len(*calls) != 1 || (*calls)[0].opts != want {
		t.Fatalf("calls = %+v, want %+v", *calls, want)
	}
	for _, frag := range []string{
		"), applied\n",
		"Warning: the cluster's CRS auto-rebalance is on\n",
		"Moves of an earlier run\n  VM 1001 on pve1, marked to move to pve1: finish, finished\n",
		"Plan\n  1. VM 1042 (vm-3f2a9c1e) from pve2 to pve1, because",
		"Moves\n  1. VM 1042 (vm-3f2a9c1e) from pve2 to pve1, moved in 41.2s, because",
		"  2. VM 1077 from pve2 to pve3, left part way in 600.0s, because the move lowers the spread " +
			"(the VM had not settled on pve3)\n",
		"1 move planned, 1 made,",
		"The moves made differ from the first plan",
		"The run stopped, because VM 1077 was left part way.\n",
		"Run the command again with --apply",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("output lacks %q:\n%s", frag, out)
		}
	}
}

func TestRunHARebalanceAGateExitsThree(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	report := rebalanceDryRun()
	report.Gates[0] = handlers.RebalanceGate{Name: handlers.RebalanceGateNodes, Detail: "pve2 is in maintenance"}
	report.Halted, report.ExitCode = handlers.RebalanceGateNodes, handlers.RebalanceExitGate
	withRebalance(d, report, nil)
	code, out, _ := runHATest(t, d, "rebalance", "--director-uuid", haDirector)
	if code != exitGate || !strings.Contains(out, "  REFUSED: pve2 is in maintenance\n") ||
		!strings.Contains(out, "The run stopped, because the nodes gate refused.\n") {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
}

func TestRunHARebalanceJSON(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	withRebalance(d, rebalanceDryRun(), nil)
	code, out, errOut := runHATest(t, d, "rebalance", "--director-uuid", haDirector, "--json",
		"--config", "/x/cpi.json")
	if code != exitOK || errOut != "" {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"director_uuid", "cluster", "token", "applied", "gates", "nodes", "resumed",
		"moves", "skipped", "spread_before", "spread_after", "deferred", "halted", "exit_code", "version", "config"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON lacks %q:\n%s", key, out)
		}
	}
	if got["token"] != "bosh@pve!****" || got["director_uuid"] != haDirector || got["cluster"] != "lab" ||
		strings.Contains(out, "5ecret") || strings.Contains(out, "tokenAuth") {
		t.Fatalf("JSON:\n%s", out)
	}
	move := got["moves"].([]any)[0].(map[string]any)
	if move["phase"] != "restore" || move["outcome"] != "planned" || move["pin_rewritten"] != false {
		t.Fatalf("move %+v", move)
	}
}

func TestRunHARebalanceUsageErrors(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"no Director":                  {},
		"cpi-config":                   {"--director-uuid", haDirector, "--cpi-config", "x.json"},
		"widen-replicas":               {"--director-uuid", haDirector, "--widen-replicas"},
		"widen-replicas with a value":  {"--director-uuid", haDirector, "-widen-replicas=true"},
		"an argument":                  {"--director-uuid", haDirector, "extra"},
		"an unknown flag":              {"--director-uuid", haDirector, "--force"},
		"no moves":                     {"--director-uuid", haDirector, "--max-moves", "0"},
		"a short settle timeout":       {"--director-uuid", haDirector, "--settle-timeout", "30s"},
		"no duration":                  {"--director-uuid", haDirector, "--max-duration", "0s"},
		"stop at start":                {"--director-uuid", haDirector, "--stop-threshold", "0.15"},
		"a zero start":                 {"--director-uuid", haDirector, "--start-threshold", "0"},
		"a start above one":            {"--director-uuid", haDirector, "--start-threshold", "1.5"},
		"a negative stop":              {"--director-uuid", haDirector, "--stop-threshold", "-0.1"},
		"a request that is not a file": {"--request", "/nonexistent/request.json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			d := newHATestDeps(rbCfg())
			calls := withRebalance(d, rebalanceDryRun(), nil)
			code, _, errOut := runHATest(t, d, append([]string{"rebalance"}, args...)...)
			if code != exitUsage || len(*calls) != 0 || errOut == "" {
				t.Fatalf("code = %d calls = %d stderr = %q", code, len(*calls), errOut)
			}
		})
	}
}

func TestRunHARebalanceHelp(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	code, out, _ := runHATest(t, d, "rebalance", "--help")
	if code != exitOK || !strings.Contains(out, "pve-ops ha rebalance") || !strings.Contains(out, "-max-moves") {
		t.Fatalf("code = %d, output:\n%s", code, out)
	}
}

func writeRequest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunHARebalanceRequestNamesTheDirector(t *testing.T) {
	t.Parallel()
	path := writeRequest(t, `{"context":{"director_uuid":"`+haDirector+`"}}`)
	d := newHATestDeps(rbCfg())
	calls := withRebalance(d, rebalanceDryRun(), nil)
	if code, _, errOut := runHATest(t, d, "rebalance", "--request", path); code != exitOK {
		t.Fatalf("code = %d stderr = %q", code, errOut)
	}
	if len(*calls) != 1 || (*calls)[0].owner != haDirector {
		t.Fatalf("calls = %+v", *calls)
	}

	d = newHATestDeps(rbCfg())
	calls = withRebalance(d, rebalanceDryRun(), nil)
	if code, _, _ := runHATest(t, d, "rebalance", "--request", path, "--director-uuid", haDirector); code != exitOK ||
		len(*calls) != 1 {
		t.Fatalf("the same Director twice: code = %d", code)
	}

	d = newHATestDeps(rbCfg())
	calls = withRebalance(d, rebalanceDryRun(), nil)
	code, _, errOut := runHATest(t, d, "rebalance", "--request", path, "--director-uuid", "other")
	if code != exitUsage || len(*calls) != 0 || !strings.Contains(errOut, "different Director") {
		t.Fatalf("code = %d stderr = %q", code, errOut)
	}
}

func TestRunHARebalanceRunErrors(t *testing.T) {
	t.Parallel()
	d := newHATestDeps(rbCfg())
	withRebalance(d, handlers.RebalanceReport{}, errors.New("no PVE client"))
	code, _, errOut := runHATest(t, d, "rebalance", "--director-uuid", haDirector)
	if code != exitError || !strings.Contains(errOut, "the run did not start: no PVE client") {
		t.Fatalf("code = %d stderr = %q", code, errOut)
	}

	d = newHATestDeps(rbCfg())
	d.deps.loadConfig = func(string, io.Writer) (*config.CPIConfig, *log.Logger, error) {
		return nil, nil, errors.New("pve-ops: config load failed")
	}
	if code, _, _ := runHATest(t, d, "rebalance", "--director-uuid", haDirector); code != exitError {
		t.Fatalf("a config that does not load: code = %d", code)
	}

	path := writeRequest(t, `{"context":{"director_uuid":"`+haDirector+`","pve_vmid_range_start":50}}`)
	d = newHATestDeps(rbCfg())
	calls := withRebalance(d, rebalanceDryRun(), nil)
	code, _, errOut = runHATest(t, d, "rebalance", "--request", path)
	if code != exitError || len(*calls) != 0 || !strings.Contains(errOut, "do not apply") {
		t.Fatalf("code = %d stderr = %q", code, errOut)
	}
}

func TestRunHARebalanceNamesAUserWithNoToken(t *testing.T) {
	t.Parallel()
	cfg := rbCfg()
	cfg.APIToken = ""
	d := newHATestDeps(cfg)
	withRebalance(d, rebalanceDryRun(), nil)
	if _, out, _ := runHATest(t, d, "rebalance", "--director-uuid", haDirector); !strings.Contains(out,
		"on cluster lab (user root@pam), a dry run") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestMaskedCredential(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cfg  *config.CPIConfig
		want string
	}{
		{nil, ""},
		{&config.CPIConfig{APIToken: "bosh@pve!cpi=secret"}, "bosh@pve!****"},
		{&config.CPIConfig{APIToken: "no-bang-secret"}, "****"},
		{&config.CPIConfig{APIToken: "!cpi=secret"}, "****"},
		{&config.CPIConfig{User: "root"}, "root@pam"},
		{&config.CPIConfig{User: "bosh", Realm: "pve"}, "bosh@pve"},
		{&config.CPIConfig{User: "bosh@pve"}, "bosh@pve"},
	}
	for _, c := range cases {
		if got := maskedCredential(c.cfg); got != c.want {
			t.Errorf("maskedCredential(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestResolveConfigPath(t *testing.T) {
	t.Setenv(envConfigPath, "")
	if got := resolveConfigPath(""); got != defaultConfigPath {
		t.Fatalf("default %q", got)
	}
	t.Setenv(envConfigPath, "/env/cpi.json")
	if got := resolveConfigPath(""); got != "/env/cpi.json" {
		t.Fatalf("env %q", got)
	}
	if got := resolveConfigPath("/flag/cpi.json"); got != "/flag/cpi.json" {
		t.Fatalf("flag %q", got)
	}
}
