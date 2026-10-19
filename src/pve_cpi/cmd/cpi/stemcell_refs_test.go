package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

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
// stemcell-refs scan makes, for one node. Without pools, Pools returns nil,
// so any lock attempt fails, and every other write reaches a nil embed and
// panics; those tests prove the report and the dry run change nothing. With
// pools, the per-VMID lock works and UpdateQemuConfig edits the guests, so
// an apply can succeed.
type refsCLIClient struct {
	nilPVEClient
	mu     sync.Mutex
	guests []refsCLIGuest
	pools  *refsCLIPools
	writes int
	// sizes holds the size a storage content read reports for each volid;
	// a volid it lacks fails the read.
	sizes map[string]int64
}

// refsCLIPools is a pool store that answers the cluster lock's create, read
// back, and delete calls.
type refsCLIPools struct {
	mu     sync.Mutex
	claims map[string]string
}

func (p *refsCLIPools) AddVM(context.Context, string, int64) error        { return nil }
func (p *refsCLIPools) MoveVMToPool(context.Context, string, int64) error { return nil }
func (p *refsCLIPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (p *refsCLIPools) CreatePool(_ context.Context, poolID, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, held := p.claims[poolID]; held {
		return errors.New("pool already exists")
	}
	if p.claims == nil {
		p.claims = map[string]string{}
	}
	p.claims[poolID] = comment
	return nil
}
func (p *refsCLIPools) DeletePool(_ context.Context, poolID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.claims, poolID)
	return nil
}
func (p *refsCLIPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.claims[poolID]
	return c, ok, nil
}

func (c *refsCLIClient) Pools() pve.PoolService {
	if c.pools == nil {
		return nil
	}
	return c.pools
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

func (n refsCLINodes) UpdateQemuConfig(_ context.Context, _, vmid string, p *nodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	id, _ := strconv.Atoi(vmid)
	for i := range n.c.guests {
		if n.c.guests[i].vmid != id {
			continue
		}
		if p.Description != nil {
			n.c.guests[i].desc = *p.Description
		}
		if p.Tags != nil {
			n.c.guests[i].tags = *p.Tags
		}
		n.c.writes++
		return nil
	}
	return errors.New("unexpected config write")
}

func (n refsCLINodes) ListQemu(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
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

func (n refsCLINodes) GetStorageContent(
	_ context.Context, _, storage, volume string,
) (*nodes.GetStorageContentResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	size, ok := n.c.sizes[storage+":"+volume]
	if !ok {
		return nil, errors.New("volume not found")
	}
	return &nodes.GetStorageContentResponse{Size: sdkclient.PVEInt(size)}, nil
}

type refsCLIQEMU struct {
	qemu.Service
	c *refsCLIClient
}

func (q refsCLIQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	for _, g := range q.c.guests {
		if g.vmid != vmid {
			continue
		}
		tmpl := 0.0
		if g.template {
			tmpl = 1
		}
		cfg := map[string]any{"description": g.desc, "tags": g.tags, "name": g.name, "template": tmpl,
			"digest": "cli-digest"}
		for k, v := range g.disks {
			cfg[k] = v
		}
		return cfg, nil
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
		"no action":              {},
		"unknown action":         {"purge", "--config", cfgPath},
		"missing config":         {"report"},
		"bad flag":               {"report", "--config", cfgPath, "--nope"},
		"positional":             {"report", "--config", cfgPath, "extra"},
		"both targets":           {"prune", "--config", cfgPath, "--director", "x", "--vmid", "5"},
		"no target":              {"prune", "--config", cfgPath},
		"blank director":         {"prune", "--config", cfgPath, "--director", "   "},
		"zero vmid":              {"prune", "--config", cfgPath, "--vmid", "0"},
		"allow-owned on vmid":    {"prune", "--config", cfgPath, "--vmid", "5", "--allow-owned-vms"},
		"prune flag on report":   {"report", "--config", cfgPath, "--apply"},
		"reconcile and director": {"prune", "--config", cfgPath, "--reconcile-tags", "--director", "x"},
		"reconcile and vmid":     {"prune", "--config", cfgPath, "--reconcile-tags", "--vmid", "5"},
		"reconcile allow-owned":  {"prune", "--config", cfgPath, "--reconcile-tags", "--allow-owned-vms"},
		"reconcile on report":    {"report", "--config", cfgPath, "--reconcile-tags"},
		"untracked alone":        {"prune", "--config", cfgPath, "--untracked"},
		"untracked director":     {"prune", "--config", cfgPath, "--director", "x", "--untracked"},
		"untracked reconcile":    {"prune", "--config", cfgPath, "--reconcile-tags", "--untracked"},
		"untracked on report":    {"report", "--config", cfgPath, "--untracked"},
		"negative repeated vmid": {"prune", "--config", cfgPath, "--vmid", "5", "--vmid", "-3"},
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
	if len(rep.Templates) != 2 || len(rep.Refs) != 1 || rep.Refs[0].Ref != "old-uuid" ||
		rep.Refs[0].OwnedVMCount == nil || *rep.Refs[0].OwnedVMCount != 1 {
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

func TestStemcellRefsPruneDryRunListsReplicas(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	desc := func(refs string) string {
		return `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":` + refs + `}`
	}
	rtags := "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;bosh-stemcell-node-pve-node2;director--old-uuid"
	client := &refsCLIClient{guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid", desc: desc(`["old-uuid"]`)},
		{vmid: 150, name: "bosh-stemcell-a-1-aaaa1111", template: true, tags: rtags, desc: desc(`["old-uuid"]`)},
		{vmid: 152, name: "bosh-stemcell-a-1-aaaa1111", template: true, tags: rtags + ";director--new-uuid",
			desc: desc(`["old-uuid","new-uuid"]`)},
		{vmid: 153, name: "bosh-stemcell-a-1-aaaa1111", template: true, tags: rtags, desc: desc(`["old-uuid"]`)},
		{vmid: 200, name: "clone", disks: map[string]string{"scsi0": "local-lvm:base-153-disk-0/vm-200-disk-0"}},
	}}
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"remove-ref-and-destroy vmid 101",
		"remove-ref-and-destroy vmid 150 (bosh-stemcell-a-1-aaaa1111 on pve), replica",
		"remove-ref vmid 152 (bosh-stemcell-a-1-aaaa1111 on pve), replica, removes tag director--old-uuid, " +
			"leaves refs new-uuid",
		"remove-ref vmid 153 (bosh-stemcell-a-1-aaaa1111 on pve), replica, removes tag director--old-uuid: " +
			"the replica stays with no refs: template backs 1 linked clone(s) (200)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run lacks %q:\n%s", want, stdout)
		}
	}
}

func TestStemcellRefsPruneApplyRefusedChangesNothing(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	// The fake has no pool service and panics on any other write, so exit 1
	// without a panic proves the refused apply wrote nothing.
	code, _, stderr := runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath, "--vmid", "101", "--apply")
	if code != 1 || !strings.Contains(stderr, "still referenced") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestStemcellRefsPruneApplyRefusedJSONShape(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath,
		"--director", "old-uuid", "--apply", "--json")
	if code != 1 || !strings.Contains(stderr, "refused") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var res handlers.StemcellRefsApplyResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("refused apply JSON: %v\n%s", err, stdout)
	}
	if !res.Refused || !res.Plan.Refused || res.Results == nil || len(res.Results) != 0 {
		t.Fatalf("refused apply result = %+v; want the apply shape with refused set", res)
	}
	if !strings.Contains(stdout, `"results": []`) {
		t.Errorf("refused apply JSON lacks an empty results list:\n%s", stdout)
	}
}

func TestStemcellRefsPruneApplySucceeds(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := &refsCLIClient{pools: &refsCLIPools{}, guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid;director--new-uuid",
			desc: `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":["old-uuid","new-uuid"]}`},
	}}
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid", "--apply")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "remove-ref vmid 101") || !strings.Contains(stdout, "applied") ||
		!strings.Contains(stdout, "removed tag director--old-uuid") {
		t.Fatalf("stdout %q", stdout)
	}
	if p, a := strings.Index(stdout, "plan:"), strings.Index(stdout, "applied:"); p < 0 || a < p {
		t.Errorf("apply text does not print the plan before the results:\n%s", stdout)
	}
	g := client.guests[0]
	if strings.Contains(g.desc, "old-uuid") || !strings.Contains(g.desc, "new-uuid") {
		t.Errorf("description after apply = %s", g.desc)
	}
	if strings.Contains(g.tags, "director--old-uuid") || !strings.Contains(g.tags, "director--new-uuid") {
		t.Errorf("tags after apply = %s", g.tags)
	}
	if client.writes != 1 {
		t.Errorf("writes = %d, want one combined description and tag write", client.writes)
	}
	if len(client.pools.claims) != 0 {
		t.Errorf("lock pools left behind: %v", client.pools.claims)
	}
}

func TestStemcellRefsPruneWarnsWhenRefMatchesNothing(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	for _, extra := range [][]string{nil, {"--apply"}} {
		args := append([]string{"prune", "--config", cfgPath, "--director", "old-uuidd"}, extra...)
		client := refsCLIFixture()
		client.pools = &refsCLIPools{}
		code, _, stderr := runStemcellRefsTest(t, client, args...)
		if code != 0 || !strings.Contains(stderr, `warning: no stemcell cache template holds the ref "old-uuidd"`) {
			t.Errorf("%v: exit %d stderr %q; want a no-match warning", extra, code, stderr)
		}
	}
	_, _, stderr := runStemcellRefsTest(t, refsCLIFixture(), "prune", "--config", cfgPath, "--director", "old-uuid",
		"--allow-owned-vms")
	if strings.Contains(stderr, "warning: no stemcell") {
		t.Errorf("matched ref warned: %q", stderr)
	}
}

func TestStemcellRefsKeptReplicasAreListed(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	desc := func(refs string) string {
		return `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":` + refs + `}`
	}
	client := &refsCLIClient{guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid", desc: desc(`["old-uuid"]`)},
		// 155 is untracked, and 157 holds no ref but has its own remove-tag.
		{vmid: 155, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-sha-aaaa1111;bosh-stemcell-node-pve-node2", desc: desc(`[]`)},
		{vmid: 157, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;bosh-stemcell-node-pve-node2;director--old-uuid",
			desc: desc(`[]`)},
	}}
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"keeps replica 155 (bosh-stemcell-a-1-aaaa1111 on pve), not freeable by --vmid: ",
		"keeps replica 157 (bosh-stemcell-a-1-aaaa1111 on pve), freeable by --vmid afterwards: ",
		"free it with prune --vmid 157",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run lacks %q:\n%s", want, stdout)
		}
	}
	code, stdout, stderr = runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("report: exit %d stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "KEPT REPLICAS") || !strings.Contains(stdout, "a prune of old-uuid keeps replica 157") {
		t.Errorf("report does not list the kept replicas:\n%s", stdout)
	}
}

func TestStemcellRefsApplyTextShowsDowngradedReplica(t *testing.T) {
	act := handlers.StemcellRefsAction{Kind: handlers.StemcellRefsActionRemoveRefAndDestroy, VMID: 150,
		Node: "pve-node2", Name: "bosh-stemcell-a-1-aaaa1111", Replica: true}
	result := handlers.StemcellRefsApplyResult{
		Plan:     handlers.StemcellRefsPlan{Mode: "director", DirectorRef: "old-uuid", Actions: []handlers.StemcellRefsAction{act}},
		Failures: 0,
		Results: []handlers.StemcellRefsActionResult{{Action: act, AppliedKind: handlers.StemcellRefsActionRemoveRef,
			Outcome: handlers.StemcellRefsOutcomeApplied, KeptReason: "replica 150 not destroyed: anchor kept",
			TagRemoved: true, CacheMarkerAdded: true}},
	}
	var out, errOut bytes.Buffer
	if code := writeStemcellRefsApply(&out, &errOut, result, false); code != 0 {
		t.Fatalf("exit %d stderr %q", code, errOut.String())
	}
	want := "remove-ref vmid 150 (bosh-stemcell-a-1-aaaa1111 on pve-node2), replica: applied " +
		"(planned remove-ref-and-destroy), kept: replica 150 not destroyed: anchor kept"
	text := out.String()
	if !strings.Contains(text, want) || !strings.Contains(text, "added tag bosh-stemcell-cache") {
		t.Errorf("apply text lacks the downgrade:\n%s", text)
	}
	if strings.Contains(text, "remove-ref-and-destroy vmid 150 (bosh-stemcell-a-1-aaaa1111 on pve-node2), replica: applied") {
		t.Errorf("apply text reports the planned destroy as applied:\n%s", text)
	}
}

// reconcileCLIClient holds 101 with a stale tag for old-uuid beside its own
// tag for new-uuid, and 102 whose stale tag is its only marker.
func reconcileCLIClient() *refsCLIClient {
	desc := func(sha8, refs string) string {
		return `{"name":"a","version":"1","sha8":"` + sha8 + `","kind":"heavy","director_refs":` + refs + `}`
	}
	return &refsCLIClient{pools: &refsCLIPools{}, guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid;director--new-uuid",
			desc: desc("aaaa1111", `["new-uuid"]`)},
		{vmid: 102, name: "bosh-stemcell-b-1-bbbb2222", template: true,
			tags: "bosh-stemcell-sha-bbbb2222;director--old-uuid", desc: desc("bbbb2222", `["new-uuid"]`)},
	}}
}

func TestStemcellRefsPruneReconcileTagsDryRun(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := reconcileCLIClient()
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--reconcile-tags")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"remove-tags vmid 101 (bosh-stemcell-a-1-aaaa1111 on pve), removes tags director--old-uuid, " +
			"keeps tags director--new-uuid",
		"remove-tags vmid 102 (bosh-stemcell-b-1-bbbb2222 on pve), removes tags director--old-uuid, " +
			"adds tag bosh-stemcell-cache",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run lacks %q:\n%s", want, stdout)
		}
	}
	if client.writes != 0 {
		t.Fatalf("dry run wrote %d times", client.writes)
	}

	code, stdout, stderr = runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--reconcile-tags", "--json")
	if code != 0 {
		t.Fatalf("JSON dry run: exit %d stderr %q", code, stderr)
	}
	var plan handlers.StemcellRefsPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, stdout)
	}
	if plan.Mode != "reconcile-tags" || len(plan.Actions) != 2 ||
		plan.Actions[0].Kind != handlers.StemcellRefsActionRemoveTags {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestStemcellRefsPruneReconcileTagsApply(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := reconcileCLIClient()
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--reconcile-tags", "--apply")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "removed tags director--old-uuid") ||
		!strings.Contains(stdout, "added tag bosh-stemcell-cache") {
		t.Fatalf("stdout %q", stdout)
	}
	for _, g := range client.guests {
		if strings.Contains(g.tags, "director--old-uuid") || !strings.Contains(g.desc, "new-uuid") {
			t.Errorf("vmid %d after apply: tags %q desc %s", g.vmid, g.tags, g.desc)
		}
	}
	if !strings.Contains(client.guests[0].tags, "director--new-uuid") {
		t.Errorf("101 lost the tag its ref backs: %q", client.guests[0].tags)
	}
	if !strings.Contains(client.guests[1].tags, "bosh-stemcell-cache") {
		t.Errorf("102 has no generation marker: %q", client.guests[1].tags)
	}
	if client.writes != 2 {
		t.Errorf("writes = %d, want one tags write per template", client.writes)
	}
	if len(client.pools.claims) != 0 {
		t.Errorf("lock pools left behind: %v", client.pools.claims)
	}
}

func TestStemcellRefsReportTextFlagsDescriptionNearLimit(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	desc := `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":["new-uuid"],"pad":"` +
		strings.Repeat("x", 7300) + `"}`
	client := &refsCLIClient{guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--new-uuid", desc: desc},
	}}
	code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{
		"description " + strconv.Itoa(len(desc)) + " bytes, near the 8192-byte limit",
		"1 template(s) have a description within 1024 bytes of PVE's 8192-byte limit",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
}

// refsCLIUntrackedFixture adds untracked template 110, which holds no ref,
// to refsCLIFixture.
func refsCLIUntrackedFixture() *refsCLIClient {
	c := refsCLIFixture()
	c.guests = append(c.guests, refsCLIGuest{vmid: 110, name: "bosh-stemcell-u-1-uuuu1111", template: true,
		tags: "bosh-stemcell;bosh-stemcell-sha-uuuu1111",
		desc: `{"name":"u","version":"1","sha8":"uuuu1111","kind":"heavy","director_refs":[]}`,
		disks: map[string]string{
			"scsi0":    "local-lvm:base-110-disk-0,size=4G",
			"efidisk0": "local-lvm:base-110-disk-1,size=4M",
		}})
	c.sizes = map[string]int64{"local-lvm:base-110-disk-0": 4 << 30}
	return c
}

func TestStemcellRefsPruneUntrackedDryRunText(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIUntrackedFixture(), "prune", "--config", cfgPath,
		"--vmid", "110", "--untracked")
	if code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"destroy-untracked vmid 110 (bosh-stemcell-u-1-uuuu1111 on pve)",
		"frees efidisk0 local-lvm:base-110-disk-1 on local-lvm, size unknown: ",
		"frees scsi0 local-lvm:base-110-disk-0 on local-lvm, 4.0 GiB (4294967296 bytes)",
		"frees at least 4.0 GiB (4294967296 bytes) (1 volume size(s) unknown) in all",
		"total freed: at least 4.0 GiB",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run lacks %q:\n%s", want, stdout)
		}
	}

	// A repeated --vmid judges each template: the tracked 101 is refused
	// and the run exits 1, while 110 is still planned.
	code, stdout, stderr = runStemcellRefsTest(t, refsCLIUntrackedFixture(), "prune", "--config", cfgPath,
		"--vmid", "110", "--vmid", "101", "--untracked")
	if code != 1 || !strings.Contains(stdout, "destroy-untracked vmid 110") ||
		!strings.Contains(stdout, "blocked vmid 101") || !strings.Contains(stdout, "tracked template") {
		t.Fatalf("repeated vmid: exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Without --untracked, the refusal names the flag.
	code, _, stderr = runStemcellRefsTest(t, refsCLIUntrackedFixture(), "prune", "--config", cfgPath, "--vmid", "110")
	if code != 1 || !strings.Contains(stderr, "prune --vmid 110 --untracked") {
		t.Fatalf("plain vmid: exit %d stderr %q", code, stderr)
	}
}

func TestStemcellRefsPruneUntrackedDryRunJSON(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	code, stdout, stderr := runStemcellRefsTest(t, refsCLIUntrackedFixture(), "prune", "--config", cfgPath,
		"--vmid", "110", "--untracked", "--json")
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var plan handlers.StemcellRefsPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, stdout)
	}
	if plan.Mode != "untracked" || len(plan.Actions) != 1 || plan.TotalBytes != 4<<30 || plan.SizesUnknown != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	f := plan.Actions[0].Frees
	if f == nil || len(f.Volumes) != 2 || f.Volumes[1].SizeBytes == nil || *f.Volumes[1].SizeBytes != 4<<30 ||
		f.Volumes[0].SizeBytes != nil || f.Volumes[0].SizeError == "" {
		t.Fatalf("frees = %+v", f)
	}
}

func TestStemcellRefsReportTextNamesUntrackedCommand(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	c := refsCLIUntrackedFixture()
	c.guests = append(c.guests, refsCLIGuest{vmid: 111, name: "bosh-stemcell-v-1-vvvv2222", template: true,
		tags:  "bosh-stemcell;bosh-stemcell-sha-vvvv2222",
		desc:  `{"name":"v","version":"1","sha8":"vvvv2222","kind":"heavy","director_refs":[]}`,
		disks: map[string]string{"scsi0": "local-lvm:base-111-disk-0"}},
		refsCLIGuest{vmid: 201, name: "clone",
			disks: map[string]string{"scsi0": "local-lvm:base-111-disk-0/vm-201-disk-0"}})
	code, stdout, stderr := runStemcellRefsTest(t, c, "report", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{"FREE WITH", "cpi stemcell-refs prune --vmid 110 --untracked",
		"cpi stemcell-refs prune --vmid 103", "blocked by linked clones 201"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
}
