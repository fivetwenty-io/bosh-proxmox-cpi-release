package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const cliLiveUUID = "6a1b2c3d-0000-4000-8000-00000000000a"

// uncoveredCLIClient is a cluster whose live director, "bosh", has VMs that
// predate owner tags, plus one create-env Director VM.
func uncoveredCLIClient() *refsCLIClient {
	desc := `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":["old-uuid"]}`
	return &refsCLIClient{pools: &refsCLIPools{}, guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid", desc: desc},
		{vmid: 300, name: "cf-router", tags: "bosh-cpi;director--bosh;deployment--cf"},
		{vmid: 301, name: "redis", tags: "bosh-cpi;director--bosh;deployment--redis"},
		{vmid: 400, name: "director", tags: "bosh-cpi;director--bosh-init;deployment--lab-bosh"},
	}}
}

func TestStemcellRefsAdoptUsageErrors(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	cases := map[string][]string{
		"missing config":        {"adopt-owners", "--director-uuid", cliLiveUUID, "--director-name", "bosh"},
		"missing uuid":          {"adopt-owners", "--config", cfgPath, "--director-name", "bosh"},
		"missing name":          {"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID},
		"uuid not a uuid":       {"adopt-owners", "--config", cfgPath, "--director-uuid", "bosh", "--director-name", "bosh"},
		"bad vmid":              {"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID, "--director-name", "bosh", "--vmid", "x"},
		"zero vmid":             {"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID, "--director-name", "bosh", "--vmid", "0"},
		"positional":            {"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID, "--director-name", "bosh", "extra"},
		"bosh-init no selector": {"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID, "--director-name", "bosh-init"},
	}
	for name, args := range cases {
		code, _, stderr := runStemcellRefsTest(t, &refsCLIClient{}, args...)
		if code != 2 || !strings.Contains(stderr, "usage: cpi stemcell-refs") {
			t.Errorf("%s: exit %d stderr %q; want 2 with usage", name, code, stderr)
		}
	}
}

func TestStemcellRefsAdoptDryRunText(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := uncoveredCLIClient()
	code, stdout, stderr := runStemcellRefsTest(t, client, "adopt-owners", "--config", cfgPath,
		"--director-uuid", cliLiveUUID, "--director-name", "bosh", "--deployment", "cf,redis")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{
		"stamps bosh-owner--" + cliLiveUUID + " on guests tagged director--bosh in deployment(s) cf, redis",
		"  deployment cf:\n    stamp vmid 300 (cf-router on pve)",
		"  deployment redis:\n    stamp vmid 301 (redis on pve)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run lacks %q:\n%s", want, stdout)
		}
	}
	if client.writes != 0 {
		t.Fatalf("dry run wrote %d times", client.writes)
	}
}

func TestStemcellRefsAdoptApplyThenPrune(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := uncoveredCLIClient()

	// The prune refuses while guests lack owner tags, override or not.
	for _, extra := range [][]string{nil, {"--allow-owned-vms"}, {"--allow-owned-vms", "--apply"}} {
		args := append([]string{"prune", "--config", cfgPath, "--director", "old-uuid"}, extra...)
		code, stdout, stderr := runStemcellRefsTest(t, client, args...)
		if code != 1 || !strings.Contains(stderr, "--allow-owned-vms does not override this") ||
			!strings.Contains(stdout, "owned VMs: unknown, because 3 guest(s) carry no owner tag or could not be read (300,301,400)") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", extra, code, stdout, stderr)
		}
	}
	// The JSON apply with the override refuses too, and prints the refused plan.
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid",
		"--apply", "--json", "--allow-owned-vms")
	if code != 1 || !strings.Contains(stderr, "refused: ownership is unknown for every ref") {
		t.Fatalf("json apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var refused handlers.StemcellRefsApplyResult
	if err := json.Unmarshal([]byte(stdout), &refused); err != nil || !refused.Refused || !refused.Plan.Refused ||
		!refused.Plan.CoverageIncomplete || len(refused.Results) != 0 {
		t.Fatalf("json apply result %+v (%v)", refused, err)
	}
	if client.writes != 0 {
		t.Fatalf("refused prunes wrote %d times", client.writes)
	}

	// The create-env Director waits until the deployed director is adopted.
	createEnv := []string{"adopt-owners", "--config", cfgPath,
		"--director-uuid", "bbbbbbbb-0000-4000-8000-000000000002", "--director-name", "bosh-init",
		"--deployment", "lab-bosh", "--apply", "--json"}
	code, _, stderr = runStemcellRefsTest(t, client, createEnv...)
	if code != 1 || !strings.Contains(stderr, "Adopt the deployed directors first") || client.writes != 0 {
		t.Fatalf("early bosh-init apply: exit %d stderr %q writes %d", code, stderr, client.writes)
	}

	code, stdout, stderr = runStemcellRefsTest(t, client, "adopt-owners", "--config", cfgPath,
		"--director-uuid", cliLiveUUID, "--director-name", "bosh", "--deployment", "cf", "--deployment", "redis", "--apply")
	if code != 0 || !strings.Contains(stdout, "vmid 300 (cf-router on pve): applied") {
		t.Fatalf("bosh apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runStemcellRefsTest(t, client, createEnv...)
	if code != 0 {
		t.Fatalf("bosh-init apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var res handlers.StemcellRefsAdoptResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || len(res.Results) != 1 ||
		res.Results[0].Outcome != handlers.StemcellRefsOutcomeApplied {
		t.Fatalf("bosh-init result %+v (%v)", res, err)
	}
	if !strings.HasPrefix(client.guests[1].tags, "bosh-cpi;bosh-owner--"+cliLiveUUID+";") {
		t.Errorf("300 tags = %q", client.guests[1].tags)
	}

	code, stdout, stderr = runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid")
	if code != 0 || strings.Contains(stdout, "owned VMs: unknown") {
		t.Fatalf("prune after adoption: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestStemcellRefsAdoptRefusalExitsOne(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := uncoveredCLIClient()
	client.guests = append(client.guests, refsCLIGuest{vmid: 302, name: "claimed",
		tags: "bosh-cpi;bosh-owner--7b1b2c3d-0000-4000-8000-00000000000b;director--bosh;deployment--cf"})
	for _, extra := range [][]string{nil, {"--apply"}, {"--apply", "--json"}} {
		args := append([]string{"adopt-owners", "--config", cfgPath, "--director-uuid", cliLiveUUID,
			"--director-name", "bosh", "--deployment", "cf,redis"}, extra...)
		code, stdout, stderr := runStemcellRefsTest(t, client, args...)
		if code != 1 || !strings.Contains(stderr, "refused: ") {
			t.Errorf("%v: exit %d stderr %q", extra, code, stderr)
		}
		if len(extra) < 2 && !strings.Contains(stdout, "conflict: vmid 302") {
			t.Errorf("%v: stdout lacks the conflict: %q", extra, stdout)
		}
	}
	if client.writes != 0 {
		t.Fatalf("refused runs wrote %d times", client.writes)
	}
}

func TestStemcellRefsAdoptRefusesWhenDeploymentMatchesNothing(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	for _, tc := range []struct{ name, deployments, want string }{
		{"boshh", "cf", `--deployment "cf" matches nothing`},
		{"bosh", "cf,rediss", `no guest tagged director--bosh carries deployment--rediss, so --deployment "rediss" matches nothing`},
	} {
		client := uncoveredCLIClient()
		code, _, stderr := runStemcellRefsTest(t, client, "adopt-owners", "--config", cfgPath,
			"--director-uuid", cliLiveUUID, "--director-name", tc.name, "--deployment", tc.deployments, "--apply")
		if code != 1 || !strings.Contains(stderr, "refused: ") || !strings.Contains(stderr, tc.want) || client.writes != 0 {
			t.Errorf("%s %s: exit %d stderr %q writes %d", tc.name, tc.deployments, code, stderr, client.writes)
		}
	}
}

func TestStemcellRefsReportTextShowsUncoveredGroups(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, uncoveredCLIClient(), "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	base := "cpi stemcell-refs adopt-owners --config <cpi.json> --director-uuid "
	for _, want := range []string{
		"OWNERSHIP UNKNOWN",
		"ownership is unknown for every ref: 3 guest(s)",
		"director bosh, deployment cf: vmids 300",
		"    " + base + "<director-uuid> --director-name bosh --deployment cf --deployment redis\n",
		"director bosh-init, deployment lab-bosh: vmids 400",
		"    " + base + "<director_id> --director-name bosh-init --deployment lab-bosh --vmid 400\n",
		"director_id in the create-env state file",
		"WOULD FREE reads blocked",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
	refsLine := ""
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "old-uuid") {
			refsLine = line
		}
	}
	if !strings.Contains(refsLine, "unknown") || !strings.Contains(refsLine, "blocked") {
		t.Errorf("ref row = %q; want unknown owned VMs and a blocked WOULD FREE", refsLine)
	}
}
