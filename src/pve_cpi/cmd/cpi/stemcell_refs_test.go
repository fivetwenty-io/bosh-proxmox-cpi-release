package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// refsCLIGuest is one guest the read-only CLI fake serves.
type refsCLIGuest struct {
	vmid     int
	name     string
	template bool
	tags     string
	desc     string
	disks    map[string]string
}

// refsCLIClient serves the membership, per-node listing, and config reads a
// stemcell-refs scan makes, for one node. Every write panics through the nil
// embeds, so these tests prove the report and the dry run change nothing.
type refsCLIClient struct {
	nilPVEClient
	guests []refsCLIGuest
}

type refsCLICluster struct {
	cluster.Service
}

func (refsCLICluster) ListConfigNodes(context.Context) (*cluster.ListConfigNodesResponse, error) {
	raw, _ := json.Marshal(map[string]string{"name": "pve"})
	resp := cluster.ListConfigNodesResponse{raw}
	return &resp, nil
}

type refsCLINodes struct {
	nodes.Service
	c *refsCLIClient
}

func (n refsCLINodes) ListQemu(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	resp := make(nodes.ListQemuResponse, 0, len(n.c.guests))
	for _, g := range n.c.guests {
		tmpl := 0
		if g.template {
			tmpl = 1
		}
		raw, _ := json.Marshal(map[string]any{"vmid": g.vmid, "name": g.name, "tags": g.tags, "template": tmpl, "status": "stopped"})
		resp = append(resp, raw)
	}
	return &resp, nil
}

type refsCLIQEMU struct {
	qemu.Service
	c *refsCLIClient
}

func (q refsCLIQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	for _, g := range q.c.guests {
		if g.vmid == vmid {
			cfg := map[string]any{"description": g.desc, "tags": g.tags}
			for k, v := range g.disks {
				cfg[k] = v
			}
			return cfg, nil
		}
	}
	return nil, errors.New("unexpected config read")
}

func (c *refsCLIClient) Cluster() cluster.Service { return refsCLICluster{} }
func (c *refsCLIClient) Nodes() nodes.Service     { return refsCLINodes{c: c} }
func (c *refsCLIClient) QEMU() qemu.Service       { return refsCLIQEMU{c: c} }

func refsCLIFactory(c pve.Client) func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
	return func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) { return c, nil }
}

func refsCLIFixture() *refsCLIClient {
	desc := func(refs string) string {
		return `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":` + refs + `}`
	}
	return &refsCLIClient{guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid", desc: desc(`["old-uuid"]`)},
		{vmid: 103, name: "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", template: true},
		{vmid: 300, name: "live", tags: "bosh-owner--old-uuid"},
	}}
}

func runStemcellRefsTest(t *testing.T, client pve.Client, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runWithArgs(append([]string{"stemcell-refs"}, args...), strings.NewReader(""), &out, &errOut,
		runOptions{ClientFactory: refsCLIFactory(client)})
	return code, out.String(), errOut.String()
}

func TestStemcellRefsUsageErrors(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	cases := map[string][]string{
		"no action":            {},
		"unknown action":       {"purge", "--config", cfgPath},
		"missing config":       {"report"},
		"bad flag":             {"report", "--config", cfgPath, "--nope"},
		"positional":           {"report", "--config", cfgPath, "extra"},
		"both targets":         {"prune", "--config", cfgPath, "--director", "x", "--vmid", "5"},
		"no target":            {"prune", "--config", cfgPath},
		"blank director":       {"prune", "--config", cfgPath, "--director", "   "},
		"zero vmid":            {"prune", "--config", cfgPath, "--vmid", "0"},
		"allow-owned on vmid":  {"prune", "--config", cfgPath, "--vmid", "5", "--allow-owned-vms"},
		"prune flag on report": {"report", "--config", cfgPath, "--apply"},
	}
	for name, args := range cases {
		code, _, stderr := runStemcellRefsTest(t, &refsCLIClient{}, args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %q)", name, code, stderr)
		}
		if !strings.Contains(stderr, "usage: cpi stemcell-refs") {
			t.Errorf("%s: stderr lacks usage: %q", name, stderr)
		}
	}
}

func TestStemcellRefsHelpExitsZero(t *testing.T) {
	code, _, stderr := runStemcellRefsTest(t, &refsCLIClient{}, "prune", "-h")
	if code != 0 || !strings.Contains(stderr, "-allow-owned-vms") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestStemcellRefsConfigAndClientFailuresExitOne(t *testing.T) {
	code, _, _ := runStemcellRefsTest(t, &refsCLIClient{}, "report", "--config", "/nonexistent/cpi.json")
	if code != 1 {
		t.Fatalf("missing config file: exit %d, want 1", code)
	}
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	var out, errOut bytes.Buffer
	code = runWithArgs([]string{"stemcell-refs", "report", "--config", cfgPath}, strings.NewReader(""), &out, &errOut,
		runOptions{ClientFactory: func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
			return nil, errors.New("dial refused")
		}})
	if code != 1 || !strings.Contains(errOut.String(), "PVE client unavailable") {
		t.Fatalf("client failure: exit %d stderr %q", code, errOut.String())
	}
}

func TestStemcellRefsReportJSON(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIFixture(), "report", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var rep handlers.StemcellRefsReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report JSON: %v\n%s", err, stdout)
	}
	if len(rep.Templates) != 2 || len(rep.Refs) != 1 || rep.Refs[0].Ref != "old-uuid" || rep.Refs[0].OwnedVMCount != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Unreferenced) != 1 || rep.Unreferenced[0].VMID != 103 || !rep.Unreferenced[0].Freeable {
		t.Fatalf("unreferenced = %+v", rep.Unreferenced)
	}
}

func TestStemcellRefsReportText(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIFixture(), "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{"TEMPLATES", "REFS", "UNREFERENCED", "old-uuid", "legacy"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text report lacks %q:\n%s", want, stdout)
		}
	}
}

func TestStemcellRefsPruneDryRunRefusesOwnedVMs(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath, "--director", "old-uuid")
	if code != 1 || !strings.Contains(stderr, "refused") || !strings.Contains(stdout, "remove-ref-and-destroy vmid 101") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// With the override, the dry run prints the same plan and exits 0.
	code, stdout, stderr = runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath,
		"--director", "old-uuid", "--allow-owned-vms", "--json")
	if code != 0 {
		t.Fatalf("allow-owned dry run: exit %d stderr %q", code, stderr)
	}
	var plan handlers.StemcellRefsPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, stdout)
	}
	if plan.Refused || len(plan.Actions) != 1 || plan.Actions[0].Kind != handlers.StemcellRefsActionRemoveRefAndDestroy {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestStemcellRefsPruneApplyRefusedChangesNothing(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	// The fake panics on any write, so exit 1 without a panic proves the
	// refused apply wrote nothing.
	code, _, stderr := runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath, "--vmid", "101", "--apply")
	if code != 1 || !strings.Contains(stderr, "still referenced") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}
