package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// scopedFaultFake is the replicate cluster with one fault of each kind whose
// report belongs to one VM or to the Director's cluster as a whole. VM 100 runs
// on shared storage and VM 101 replicates. VM 102 carries the managed tag but
// has no HA resource. The pin of VM 101, the pin of VM 5000, which no band
// holds, and a group rule are all disabled. VM 100's resource is ignored, VM
// 101's carries a rebalance mark no run holds, and node n2 keeps a volume of
// VM 560, which no longer exists. The dry run restores VM 100 into its AZ,
// pulls VM 102 apart from a group mate, and finds memory spread 30 points.
func scopedFaultFake() *fakeHA {
	f := replicaFake()
	f.guests = append(f.guests, pve.GuestRef{VMID: 102, Node: "n3", Name: "vm-102",
		Tags: "bosh-cpi;bosh-ha-managed;bosh-owner--" + haDirector, Status: "running"})
	f.memory[102] = 4 * haGiB
	f.configs[102] = map[string]any{"cpu": "x86-64-v2-AES", "scsi0": "nfs1:102/vm-102-disk-0.raw,size=10G"}
	for i := range f.guests {
		if !strings.Contains(f.guests[i].Tags, "bosh-owner--") {
			f.guests[i].Tags += ";bosh-owner--" + haDirector
		}
	}
	f.resources[0].State = "ignored"
	f.resources[1].Comment = ha.WithRebalanceMark(f.resources[1].Comment, "n3")
	f.rules[0].Disabled = true
	f.rules = append(f.rules,
		ha.Rule{ID: haPinRulePrefix + "5000", Type: ha.RuleTypeNodeAffinity, Comment: haMarker, Disabled: true,
			Resources: []string{"vm:5000"}},
		ha.Rule{ID: haGroupRulePrefix + "web", Type: ha.RuleTypeResourceAffinity, Comment: haMarker, Disabled: true})
	f.volumes["n2/local-zfs"] = map[string]bool{"local-zfs:vm-560-disk-0": true}
	f.plan = handlers.RebalanceReport{Cluster: "prod-east", SpreadBefore: 0.30,
		Gates: []handlers.RebalanceGate{{Name: handlers.RebalanceGateProfile, OK: true}},
		Moves: []handlers.RebalanceMove{{VMID: 100, Phase: harebalance.PhaseRestore},
			{VMID: 102, Phase: harebalance.PhaseSeparate}}}
	return f
}

// scopedRun runs the doctor for entry i of entries, all of them judged for
// the test Director, the way runHACPIConfig does.
func scopedRun(t *testing.T, entries []cpiConfigEntry, i int, unread ...int) *haReport {
	t.Helper()
	base := haCfg("auto", "replicate")
	owners := make([]string, len(entries))
	for k := range owners {
		owners[k] = haDirector
	}
	scopes := entryScopes(entries, base, owners)
	for _, k := range unread {
		markUnread(scopes, k)
	}
	eff, _, _, err := config.ApplyContextOverrides(base, entries[i].Properties)
	if err != nil {
		t.Fatal(err)
	}
	return collectHAScoped(context.Background(), scopedFaultFake(), eff, haDirector, haNow, scopes[i])
}

// subjects returns each finding's kind and subject, sorted.
func subjects(r *haReport) []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Kind+" "+f.Subject)
	}
	slices.Sort(out)
	return out
}

// TestCollectScopedEntriesOfOneDirectorReportEachFindingOnce pins that two
// cpi-config entries of the same Director that share a cluster and a VM ID
// band report each VM's findings, and the Director's rule, resource, and
// rebalance findings, once. The first entry reports what a run with no entry
// reports, and the second reports nothing.
func TestCollectScopedEntriesOfOneDirectorReportEachFindingOnce(t *testing.T) {
	t.Parallel()
	entries := []cpiConfigEntry{
		{Name: "az1", Type: "pve", Properties: map[string]any{}},
		{Name: "az2", Type: "pve", Properties: map[string]any{}},
	}
	whole := collectHA(context.Background(), scopedFaultFake(), haCfg("auto", "replicate"), haDirector, haNow)
	for _, kind := range []string{haKindRuleProblem, haKindResourceIgnored, haKindManagedNoResource,
		haKindReplicaGoneVM, haKindRebalancePending, haKindSpreadSkewed} {
		if len(findings(whole, kind)) == 0 {
			t.Fatalf("the fixture raises no %s finding: %+v", kind, whole.Findings)
		}
	}
	first := scopedRun(t, entries, 0)
	if got, want := subjects(first), subjects(whole); !slices.Equal(got, want) {
		t.Errorf("first entry reports\n%v\nwant what a run with no entry reports\n%v", got, want)
	}
	if second := scopedRun(t, entries, 1); len(second.Findings) != 0 {
		t.Errorf("second entry reports %+v, want nothing the first entry reports", second.Findings)
	}
	// When the doctor could not read the first entry's cluster, the second
	// entry reports everything in its place.
	if got, want := subjects(scopedRun(t, entries, 1, 0)), subjects(whole); !slices.Equal(got, want) {
		t.Errorf("second entry after an unread first reports\n%v\nwant\n%v", got, want)
	}
}

// TestCollectScopedEntriesSplitFindingsByBand pins that entries of the same
// Director with disjoint bands each report the findings of the VMs in their own
// band, that a rule or resource no band holds and a rule that names no VM go
// to the first entry, and that the dry run's moves and memory spread are split
// the same way.
func TestCollectScopedEntriesSplitFindingsByBand(t *testing.T) {
	t.Parallel()
	entries := []cpiConfigEntry{
		{Name: "low", Type: "pve", Properties: map[string]any{"pve_vmid_range_start": 100, "pve_vmid_range_end": 101}},
		{Name: "high", Type: "pve", Properties: map[string]any{"pve_vmid_range_start": 102, "pve_vmid_range_end": 999}},
	}
	low, high := scopedRun(t, entries, 0), scopedRun(t, entries, 1)
	wantLow := []string{
		haKindPinMissing + " vm:101",
		haKindRebalancePending + " vm:101",
		haKindResourceIgnored + " vm:100",
		haKindRuleProblem + " " + haGroupRulePrefix + "web",
		haKindRuleProblem + " " + haPinRulePrefix + "101",
		haKindRuleProblem + " " + haPinRulePrefix + "5000",
		haKindSpreadSkewed + " prod-east",
	}
	slices.Sort(wantLow)
	if got := subjects(low); !slices.Equal(got, wantLow) {
		t.Errorf("entry low reports\n%v\nwant\n%v", got, wantLow)
	}
	wantHigh := []string{
		haKindManagedNoResource + " vm:102",
		haKindReplicaGoneVM + " local-zfs:vm-560-disk-0",
		haKindSpreadSkewed + " prod-east",
	}
	slices.Sort(wantHigh)
	if got := subjects(high); !slices.Equal(got, wantHigh) {
		t.Errorf("entry high reports\n%v\nwant\n%v", got, wantHigh)
	}

	lowSpread := findings(low, haKindSpreadSkewed)[0].Detail
	for _, s := range []string{"1 HA VM of this Director runs outside its AZ", "spreads 30 points",
		"The counts cover the VMs this entry reports on",
		"pve-ops ha rebalance --director-uuid " + haDirector + " --request <file>",
		"the cpi_config entry low,"} {
		if !strings.Contains(lowSpread, s) {
			t.Errorf("entry low's advice lacks %q: %s", s, lowSpread)
		}
	}
	highSpread := findings(high, haKindSpreadSkewed)[0].Detail
	for _, s := range []string{"A rebalance would make 1 separation move. It moves", "the cpi_config entry high,"} {
		if !strings.Contains(highSpread, s) {
			t.Errorf("entry high's advice lacks %q: %s", s, highSpread)
		}
	}
	for _, s := range []string{"outside", "spreads"} {
		if strings.Contains(highSpread, s) {
			t.Errorf("entry high's advice repeats what entry low reports (%q): %s", s, highSpread)
		}
	}
}

// TestCollectSpreadSkewedNamesTheDryRunsMoveCap pins that the advice names
// --max-moves when the plan holds more moves than the dry run lists by default,
// so the count the advice states matches the moves the named command lists.
func TestCollectSpreadSkewedNamesTheDryRunsMoveCap(t *testing.T) {
	t.Parallel()
	for _, n := range []int{handlers.RebalanceDefaultMaxMoves, handlers.RebalanceDefaultMaxMoves + 2} {
		f := haBaseFake()
		f.plan = handlers.RebalanceReport{Cluster: "prod-east",
			Gates: []handlers.RebalanceGate{{Name: handlers.RebalanceGateProfile, OK: true}}}
		for i := range n {
			f.plan.Moves = append(f.plan.Moves, handlers.RebalanceMove{VMID: 300 + i, Phase: harebalance.PhaseRestore})
		}
		detail := findings(runCollect(t, f, haCfg("auto", "none")), haKindSpreadSkewed)[0].Detail
		capped := strings.Contains(detail, "--max-moves")
		if n <= handlers.RebalanceDefaultMaxMoves && capped {
			t.Errorf("%d moves: the advice names --max-moves although the default lists them all: %s", n, detail)
		}
		if n > handlers.RebalanceDefaultMaxMoves {
			for _, s := range []string{
				"pve-ops ha rebalance --director-uuid " + haDirector + " --max-moves " + itoa(n) + " to see",
				"The plan holds " + itoa(n) + " moves, and without --max-moves the dry run lists the first " +
					itoa(handlers.RebalanceDefaultMaxMoves),
			} {
				if !strings.Contains(detail, s) {
					t.Errorf("%d moves: the advice lacks %q: %s", n, s, detail)
				}
			}
		}
	}
}

// TestRunHACPIConfig_HandsAnUnreadEntrysFindingsToTheNext pins that when the
// doctor cannot read the cluster of the first of two entries of one Director
// that share a cluster and a band, the second entry reports the VM's findings.
func TestRunHACPIConfig_HandsAnUnreadEntrysFindingsToTheNext(t *testing.T) {
	t.Parallel()
	doc := `[{"name":"a","type":"pve","director_uuid":"` + haDirector + `",` +
		`"properties":{"pve_api_token":"root@pam!bad=x"}},` +
		`{"name":"b","type":"pve","director_uuid":"` + haDirector + `","properties":{}}]`
	deps := haDeps{
		openReader: func(cfg *config.CPIConfig, _ *log.Logger) (HAReader, error) {
			if strings.Contains(cfg.APIToken, "bad") {
				return nil, errFake
			}
			f := haBaseFake()
			f.guests[0].Tags += ";bosh-owner--" + haDirector
			f.resources = nil
			return f, nil
		},
		now: func() time.Time { return haNow },
	}
	path := filepath.Join(t.TempDir(), "cpi-config.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runHACPIConfig(context.Background(), path, haCfg("auto", "none"), nil, "", true, &stdout, &stderr, deps)
	var out haMultiReport
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Clusters) != 2 {
		t.Fatalf("output %q (%v), stderr %q", stdout.String(), err, stderr.String())
	}
	want(t, out.Clusters[1], haKindManagedNoResource, haSeverityProblem, "vm:100")
}
