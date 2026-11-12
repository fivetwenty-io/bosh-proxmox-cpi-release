package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// nhNow is the entry node's clock in every node health test. It is far from the
// Director's real clock on purpose, because the helper must measure timestamps
// against the entry node and never against the machine it runs on.
const nhNow int64 = 1_000_000_000

type nhCluster struct {
	rows       []json.RawMessage
	statusErr  error
	manager    json.RawMessage
	managerErr error
	current    []json.RawMessage
	currentErr error

	// resources answers GET /cluster/ha/resources, and resourcesErr fails it.
	// NodeHealth reads it only to tell an idle CRM from a stale one.
	resources     []json.RawMessage
	resourcesErr  error
	resourceCalls int

	managerCalls int
	currentCalls int
}

func (c *nhCluster) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	resp := sdkcluster.ListStatusResponse(c.rows)
	return &resp, nil
}

func (c *nhCluster) ListHaStatusManagerStatus(_ context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	c.managerCalls++
	if c.managerErr != nil {
		return nil, c.managerErr
	}
	body := c.manager
	return &body, nil
}

func (c *nhCluster) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	c.currentCalls++
	if c.currentErr != nil {
		return nil, c.currentErr
	}
	resp := sdkcluster.ListHaStatusCurrentResponse(c.current)
	return &resp, nil
}

func (c *nhCluster) ListHaResources(
	_ context.Context, _ *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	c.resourceCalls++
	if c.resourcesErr != nil {
		return nil, c.resourcesErr
	}
	resp := sdkcluster.ListHaResourcesResponse(c.resources)
	return &resp, nil
}

type nhNodes struct {
	now   int64
	err   error
	asked []string
}

func (n *nhNodes) ListTime(_ context.Context, node string) (*sdknodes.ListTimeResponse, error) {
	n.asked = append(n.asked, node)
	if n.err != nil {
		return nil, n.err
	}
	var resp sdknodes.ListTimeResponse
	raw, _ := json.Marshal(map[string]any{"time": n.now, "localtime": n.now, "timezone": "UTC"})
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func nhNode(name string, online int, local bool, tags string) json.RawMessage {
	row := map[string]any{"type": "node", "name": name, "online": online, "id": "node/" + name}
	if local {
		row["local"] = 1
	}
	if tags != "" {
		row["tags"] = tags
	}
	raw, _ := json.Marshal(row)
	return raw
}

func nhClusterRow(quorate int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "cluster", "name": "lab", "quorate": quorate, "nodes": 3})
	return raw
}

// nhLRM is one lrm_status entry.
func nhLRM(mode, state string, ts int64) map[string]any {
	return map[string]any{"mode": mode, "state": state, "timestamp": ts}
}

func nhActive(ts int64) map[string]any { return nhLRM("active", "active", ts) }

// nhManager builds the body of GET /cluster/ha/status/manager_status.
func nhManager(crmTS int64, nodeStatus map[string]string, lrm map[string]map[string]any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{
			"master_node": "pve1", "node_status": nodeStatus, "service_status": map[string]any{},
			"timestamp": crmTS,
		},
		"lrm_status": lrm,
		"quorum":     map[string]any{"node": "pve1", "quorate": "1"},
	})
	return raw
}

// nhHealthy is a three-node cluster, entry pve1, with a fresh CRM and LRMs and
// one HA resource, so the CRM is active and an old timestamp means it is stale.
func nhHealthy() *nhCluster {
	return &nhCluster{
		resources: []json.RawMessage{haResource("vm:900", "started")},
		rows: []json.RawMessage{
			nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
		},
		manager: nhManager(nhNow-5,
			map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
			map[string]map[string]any{
				"pve1": nhActive(nhNow - 2), "pve2": nhActive(nhNow - 2), "pve3": nhActive(nhNow - 2),
			}),
	}
}

func nhRun(t *testing.T, c *nhCluster, opts pve.NodeHealthOptions) (*pve.NodeHealthReport, *nhNodes) {
	t.Helper()
	n := &nhNodes{now: nhNow}
	report, err := pve.NodeHealth(t.Context(), c, n, opts)
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	return report, n
}

func nhEntry(t *testing.T, r *pve.NodeHealthReport, name string) pve.NodeHealthEntry {
	t.Helper()
	e, ok := r.Node(name)
	if !ok {
		t.Fatalf("report has no entry for %q: %+v", name, r.Nodes)
	}
	return e
}

func TestNodeHealth_AllHealthy(t *testing.T) {
	t.Parallel()
	r, n := nhRun(t, nhHealthy(), pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus || !r.Quorate || !r.QuorumReported || r.EntryNode != "pve1" {
		t.Fatalf("report header: %+v", r)
	}
	if len(r.Nodes) != 3 || r.Nodes[0].Name != "pve1" || r.Nodes[2].Name != "pve3" {
		t.Fatalf("nodes not sorted by name: %+v", r.Nodes)
	}
	for _, e := range r.Nodes {
		if e.Health != pve.NodeHealthOnline || !e.Placeable || !e.ProxyTarget || e.DeadOrFenced ||
			e.Maintenance || e.Watchdog != pve.WatchdogArmed || e.CRMState != "online" {
			t.Errorf("%s: %+v", e.Name, e)
		}
	}
	if len(n.asked) != 1 || n.asked[0] != "pve1" {
		t.Errorf("the clock must be read once, on the entry node: %v", n.asked)
	}
}

func TestNodeHealth_MaintenanceFromCRM(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager = nhManager(nhNow-5,
		map[string]string{"pve1": "online", "pve2": "maintenance", "pve3": "online"},
		map[string]map[string]any{
			"pve1": nhActive(nhNow), "pve2": nhLRM("maintenance", "maintenance", nhNow), "pve3": nhActive(nhNow),
		})
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	e := nhEntry(t, r, "pve2")
	if e.Health != pve.NodeHealthMaintenance || !e.Maintenance || e.Placeable || !e.ProxyTarget || e.DeadOrFenced {
		t.Errorf("pve2: %+v", e)
	}
	if e.Watchdog != pve.WatchdogArmed {
		t.Errorf("a maintenance LRM keeps its watchdog armed: %+v", e)
	}
	if !r.Placeable("pve1") || r.Placeable("pve2") || !r.ProxyTarget("pve2") || r.DeadOrFenced("pve2") {
		t.Error("accessors disagree with the entries")
	}
}

func TestNodeHealth_MaintenanceFromLRMAlone(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager = nhManager(nhNow-5,
		map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		map[string]map[string]any{
			"pve1": nhActive(nhNow), "pve2": nhLRM("maintenance", "active", nhNow), "pve3": nhActive(nhNow),
		})
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable {
		t.Errorf("an LRM in maintenance mode must count: %+v", e)
	}
}

func TestNodeHealth_OperatorTagIsUnioned(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[2] = nhNode("pve2", 1, false, "bosh;drain other")
	r, _ := nhRun(t, c, pve.NodeHealthOptions{MaintenanceTags: []string{"", "drain"}})
	e := nhEntry(t, r, "pve2")
	if !e.Maintenance || e.Placeable || e.Health != pve.NodeHealthMaintenance || !e.ProxyTarget {
		t.Errorf("pve2: %+v", e)
	}
	if e.CRMState != "online" {
		t.Errorf("the tag must not hide what the CRM said: %+v", e)
	}
	// Without the option the same tag means nothing.
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if !r.Placeable("pve2") {
		t.Error("tag with no configured maintenance tags must not exclude the node")
	}
}

func TestNodeHealth_OperatorTagWithoutHASources(t *testing.T) {
	t.Parallel()
	c := &nhCluster{rows: []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "maintenance")}}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{MaintenanceTags: []string{"maintenance"}})
	if r.HA != pve.HAViewNotConfigured {
		t.Fatalf("HA = %q", r.HA)
	}
	if e := nhEntry(t, r, "pve1"); e.Placeable || !e.Maintenance || !e.ProxyTarget {
		t.Errorf("pve1: %+v", e)
	}
}

// SkipHA reads no HA source: no manager status, no status rows, and no clock.
// Cluster status alone classifies the nodes, so a node listed offline is still
// dead, and the operator's maintenance tag still applies.
func TestNodeHealth_SkipHAReadsNoHASource(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows = []json.RawMessage{
		nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, "maintenance"), nhNode("pve3", 0, false, ""),
	}
	r, n := nhRun(t, c, pve.NodeHealthOptions{SkipHA: true, MaintenanceTags: []string{"maintenance"}})
	if r.HA != pve.HAViewNotRead {
		t.Fatalf("HA = %q, want %q", r.HA, pve.HAViewNotRead)
	}
	if c.managerCalls != 0 || c.currentCalls != 0 || len(n.asked) != 0 {
		t.Fatalf("read an HA source: manager %d, status rows %d, clock %v", c.managerCalls, c.currentCalls, n.asked)
	}
	if e := nhEntry(t, r, "pve1"); !e.ProxyTarget || e.DeadOrFenced {
		t.Errorf("pve1 is online in cluster status: %+v", e)
	}
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || !e.ProxyTarget {
		t.Errorf("pve2 carries the maintenance tag and answers: %+v", e)
	}
	if e := nhEntry(t, r, "pve3"); e.ProxyTarget || !e.DeadOrFenced {
		t.Errorf("pve3 is offline in cluster status: %+v", e)
	}
}

// One errored VM on a healthy node must not mark the node. The old check read the
// state field of every entry in status/current and condemned the node for it.
func TestNodeHealth_ErroredServiceDoesNotMarkNode(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	svc, _ := json.Marshal(map[string]any{"type": "service", "id": "vm:100", "node": "pve2", "state": "error"})
	c.current = []json.RawMessage{svc}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if e := nhEntry(t, r, "pve2"); !e.Placeable || e.Maintenance || e.DeadOrFenced {
		t.Errorf("pve2: %+v", e)
	}
	if c.currentCalls != 0 {
		t.Error("status/current is a fallback and must not be read beside a fresh manager status")
	}

	// The same entry through the text fallback is still ignored, because only lrm
	// entries are read there.
	c.manager = json.RawMessage(`{}`)
	lrm, _ := json.Marshal(map[string]any{"type": "lrm", "node": "pve1", "status": "pve1 (active, Mon Jan  1 2001)"})
	c.current = []json.RawMessage{svc, lrm}
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewLRMText || !r.Placeable("pve2") {
		t.Errorf("HA=%q pve2=%+v", r.HA, nhEntry(t, r, "pve2"))
	}
}

func TestNodeHealth_UnreadableManagerStatusFallsBackToText(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.managerErr = errors.New("503 pve-ha-crm not running")
	lrm := func(node, status string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"type": "lrm", "id": "lrm:" + node, "status": status})
		return raw
	}
	c.current = []json.RawMessage{
		lrm("pve1", "pve1 (active, Mon Jan  1 00:00:00 2001)"),
		lrm("pve2", "pve2 (maintenance mode, Mon Jan  1 00:00:00 2001)"),
		lrm("pve3", "pve3 (old timestamp - dead?, Mon Jan  1 00:00:00 2001)"),
		json.RawMessage(`{"type":"lrm"}`),
		json.RawMessage(`not json`),
	}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewLRMText || r.HAReason == "" {
		t.Fatalf("HA=%q reason=%q", r.HA, r.HAReason)
	}
	if e := nhEntry(t, r, "pve1"); !e.Placeable || e.Watchdog != pve.WatchdogUnknown {
		t.Errorf("pve1: %+v", e)
	}
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable || !e.ProxyTarget {
		t.Errorf("pve2: %+v", e)
	}
	if e := nhEntry(t, r, "pve3"); e.Health != pve.NodeHealthUnknown || e.Placeable || e.DeadOrFenced {
		t.Errorf("a silent LRM text is unknown, not dead: %+v", e)
	}
}

func TestNodeHealth_UnreadableManagerStatusAndCurrent(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.managerErr = errors.New("HA API unavailable")
	c.currentErr = errors.New("HA API unavailable")
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("HA = %q", r.HA)
	}
	for _, e := range r.Nodes {
		if !e.Placeable || !e.ProxyTarget || e.Watchdog != pve.WatchdogUnknown {
			t.Errorf("an unreadable HA source leaves cluster status to decide: %+v", e)
		}
	}

	// An empty manager status plus an unreadable status/current is also unreadable.
	c.managerErr = nil
	c.manager = json.RawMessage(`{}`)
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("empty manager + failed current: HA = %q", r.HA)
	}

	// An unreadable manager status next to a readable but empty status/current is
	// unreadable too, since the empty list proves nothing.
	c.managerErr = errors.New("boom")
	c.currentErr = nil
	c.current = nil
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("failed manager + empty current: HA = %q", r.HA)
	}
}

func TestNodeHealth_NoHAConfigured(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]json.RawMessage{
		"empty object": json.RawMessage(`{}`),
		"null":         json.RawMessage(`null`),
		"no body":      nil,
		"empty maps":   json.RawMessage(`{"manager_status":{},"lrm_status":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nhHealthy()
			c.manager = body
			r, n := nhRun(t, c, pve.NodeHealthOptions{})
			if r.HA != pve.HAViewNotConfigured {
				t.Fatalf("HA = %q (%s)", r.HA, r.HAReason)
			}
			for _, e := range r.Nodes {
				if !e.Placeable || e.Watchdog != pve.WatchdogUnknown {
					t.Errorf("%+v", e)
				}
			}
			if len(n.asked) != 0 {
				t.Errorf("no clock read is needed without an HA view: %v", n.asked)
			}
		})
	}
}

func TestNodeHealth_OfflineInClusterStatus(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[3] = nhNode("pve3", 0, false, "")
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	e := nhEntry(t, r, "pve3")
	if e.Health != pve.NodeHealthDead || !e.DeadOrFenced || e.Placeable || e.ProxyTarget {
		t.Errorf("pve3: %+v", e)
	}
	if e.CRMState != "online" {
		t.Errorf("the CRM's lagging view is still reported: %+v", e)
	}
}

func TestNodeHealth_NodeMissingFromClusterStatus(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows = c.rows[:3] // pve3 is gone from cluster status but the CRM still lists it
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	e := nhEntry(t, r, "pve3")
	if !e.DeadOrFenced || e.Placeable || e.ProxyTarget {
		t.Errorf("pve3: %+v", e)
	}
}

// A quorum loss leaves the entry node's own flag as the only trustworthy fact.
func TestNodeHealth_QuorumLoss(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows = []json.RawMessage{
		nhClusterRow(0), nhNode("pve1", 1, true, ""), nhNode("pve2", 0, false, ""), nhNode("pve3", 0, false, ""),
	}
	r, n := nhRun(t, c, pve.NodeHealthOptions{})
	if r.Quorate || !r.QuorumReported || r.HA != pve.HAViewNotQuorate {
		t.Fatalf("report header: %+v", r)
	}
	for _, e := range r.Nodes {
		if e.Health != pve.NodeHealthUnknown || e.Placeable || e.ProxyTarget || e.DeadOrFenced ||
			e.Watchdog != pve.WatchdogUnknown {
			t.Errorf("%s must be unknown for every question: %+v", e.Name, e)
		}
	}
	if c.managerCalls != 0 || c.currentCalls != 0 || len(n.asked) != 0 {
		t.Error("no HA source or clock may be read through a non-quorate entry node")
	}
}

// The entry node sits in a minority partition that still sees the others online.
func TestNodeHealth_NonQuorateEntryNode(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[0] = nhClusterRow(0)
	r, _ := nhRun(t, c, pve.NodeHealthOptions{MaintenanceTags: []string{"drain"}})
	for _, e := range r.Nodes {
		if e.Health != pve.NodeHealthUnknown || e.Placeable || e.ProxyTarget {
			t.Errorf("%s: %+v", e.Name, e)
		}
	}
	if r.Placeable("nope") || r.ProxyTarget("nope") || r.DeadOrFenced("nope") || r.WatchdogOf("nope") != pve.WatchdogUnknown {
		t.Error("a node no source mentions answers false and unknown")
	}
}

func TestNodeHealth_NonQuorateKeepsOperatorTag(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[0] = nhClusterRow(0)
	c.rows[2] = nhNode("pve2", 1, false, "drain")
	r, _ := nhRun(t, c, pve.NodeHealthOptions{MaintenanceTags: []string{"drain"}})
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Health != pve.NodeHealthUnknown {
		t.Errorf("pve2: %+v", e)
	}
}

func TestNodeHealth_StandaloneHost(t *testing.T) {
	t.Parallel()
	c := &nhCluster{rows: []json.RawMessage{nhNode("solo", 1, true, "")}}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if !r.Quorate || r.QuorumReported {
		t.Fatalf("a host with no cluster row has no quorum report: %+v", r)
	}
	if e := nhEntry(t, r, "solo"); !e.Placeable || !e.ProxyTarget || e.Health != pve.NodeHealthOnline {
		t.Errorf("solo: %+v", e)
	}
}

func TestNodeHealth_MultiNodeWithoutClusterRowFailsClosed(t *testing.T) {
	t.Parallel()
	c := &nhCluster{rows: []json.RawMessage{nhNode("pve1", 1, true, ""), nhNode("pve2", 2, false, "")}}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.Quorate || r.QuorumReported {
		t.Fatalf("two node rows without a cluster row must not count as quorate: %+v", r)
	}
	for _, n := range []string{"pve1", "pve2"} {
		if e := nhEntry(t, r, n); e.Placeable || e.Health != pve.NodeHealthUnknown {
			t.Errorf("%s: %+v", n, e)
		}
	}
}

func TestNodeHealth_LRMEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		lrm          map[string]any // nil means no entry
		health       pve.NodeHealthState
		placeable    bool
		watchdog     pve.WatchdogState
		maintenance  bool
		reasonSubstr string
	}{
		{"active and fresh", nhActive(nhNow - 3), pve.NodeHealthOnline, true, pve.WatchdogArmed, false, ""},
		{"active at the 30 second limit", nhActive(nhNow - 30), pve.NodeHealthOnline, true, pve.WatchdogArmed, false, ""},
		{"wait for agent lock", nhLRM("active", "wait_for_agent_lock", nhNow-3), pve.NodeHealthOnline, true,
			pve.WatchdogIdle, false, ""},
		{"31 seconds old", nhActive(nhNow - 31), pve.NodeHealthUnknown, false, pve.WatchdogUnknown, false, "older"},
		{"missing entry", nil, pve.NodeHealthUnknown, false, pve.WatchdogUnknown, false, "missing"},
		{"future timestamp", nhActive(nhNow + 6), pve.NodeHealthUnknown, false, pve.WatchdogUnknown, false, "older"},
		{"slightly ahead is skew", nhActive(nhNow + 5), pve.NodeHealthOnline, true, pve.WatchdogArmed, false, ""},
		{"zero timestamp", nhActive(0), pve.NodeHealthUnknown, false, pve.WatchdogUnknown, false, "older"},
		{"lost agent lock", nhLRM("active", "lost_agent_lock", nhNow), pve.NodeHealthUnknown, false,
			pve.WatchdogUnknown, false, "lost_agent_lock"},
		{"restarting", nhLRM("restart", "active", nhNow), pve.NodeHealthOnline, false, pve.WatchdogArmed, false,
			"restart"},
		{"shutting down", nhLRM("shutdown", "active", nhNow), pve.NodeHealthOnline, false, pve.WatchdogArmed, false,
			"shutdown"},
		{"stale maintenance", nhLRM("maintenance", "maintenance", nhNow-90), pve.NodeHealthUnknown, false,
			pve.WatchdogUnknown, false, "older"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := nhHealthy()
			lrm := map[string]map[string]any{"pve1": nhActive(nhNow), "pve3": nhActive(nhNow)}
			if tt.lrm != nil {
				lrm["pve2"] = tt.lrm
			}
			c.manager = nhManager(nhNow-5,
				map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"}, lrm)
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			e := nhEntry(t, r, "pve2")
			if e.Health != tt.health || e.Placeable != tt.placeable || e.Watchdog != tt.watchdog ||
				e.Maintenance != tt.maintenance {
				t.Errorf("got %+v", e)
			}
			if !e.ProxyTarget || e.DeadOrFenced {
				t.Errorf("an LRM answer never makes an online node dead or unreachable: %+v", e)
			}
			if tt.reasonSubstr != "" && !strings.Contains(e.Reason, tt.reasonSubstr) {
				t.Errorf("reason %q lacks %q", e.Reason, tt.reasonSubstr)
			}
		})
	}
}

func TestNodeHealth_CRMFreshness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		crmTS int64
		want  pve.HAView
	}{
		{"fresh", nhNow - 10, pve.HAViewManagerStatus},
		{"at the 60 second limit", nhNow - 60, pve.HAViewManagerStatus},
		{"61 seconds old", nhNow - 61, pve.HAViewStale},
		{"hours old", nhNow - 7200, pve.HAViewStale},
		{"within the future skew", nhNow + 5, pve.HAViewManagerStatus},
		{"in the future", nhNow + 6, pve.HAViewStale},
		{"missing", 0, pve.HAViewStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := nhHealthy()
			c.manager = nhManager(tt.crmTS,
				map[string]string{"pve1": "online", "pve2": "fence", "pve3": "online"},
				map[string]map[string]any{"pve1": nhActive(nhNow), "pve2": nhActive(nhNow), "pve3": nhActive(nhNow)})
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			if r.HA != tt.want {
				t.Fatalf("HA = %q (%s)", r.HA, r.HAReason)
			}
			e := nhEntry(t, r, "pve2")
			if tt.want == pve.HAViewStale {
				// A stale CRM view is not trusted, so its fence report is ignored
				// and cluster status decides. The watchdog stays unknown.
				if e.DeadOrFenced || !e.Placeable || e.Watchdog != pve.WatchdogUnknown || e.CRMState != "" {
					t.Errorf("stale CRM view leaked into the answer: %+v", e)
				}
			} else if !e.DeadOrFenced {
				t.Errorf("a fresh CRM view reporting fence must classify the node dead: %+v", e)
			}
		})
	}
}

// The helper measures against the entry node's clock. A Director whose own clock
// is hours off must neither fail a fresh view nor pass a stale one, because the
// helper never consults it.
func TestNodeHealth_DirectorClockSkewIsIrrelevant(t *testing.T) {
	t.Parallel()
	// Timestamps look current by the real wall clock but are hours old on the
	// entry node's clock.
	realNow := int64(1_900_000_000)
	c := nhHealthy()
	c.manager = nhManager(realNow,
		map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		map[string]map[string]any{"pve1": nhActive(realNow), "pve2": nhActive(realNow), "pve3": nhActive(realNow)})
	n := &nhNodes{now: realNow - 4*3600}
	r, err := pve.NodeHealth(t.Context(), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewStale {
		t.Fatalf("a view from the entry node's future must be stale: %q", r.HA)
	}

	// And the reverse: a fresh view on a Director whose clock is far from it.
	r, _ = nhRun(t, nhHealthy(), pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus {
		t.Fatalf("HA = %q", r.HA)
	}
}

func TestNodeHealth_FencedNodeWithOldLRMIsDeadNeverUnknown(t *testing.T) {
	t.Parallel()
	for _, crm := range []string{"fence", "unknown", "gone"} {
		for _, clusterOnline := range []int{0, 1} {
			c := nhHealthy()
			c.rows[2] = nhNode("pve2", clusterOnline, false, "")
			c.manager = nhManager(nhNow-5,
				map[string]string{"pve1": "online", "pve2": crm, "pve3": "online"},
				map[string]map[string]any{
					"pve1": nhActive(nhNow), "pve3": nhActive(nhNow),
					// The failed node's LRM stopped several hours ago.
					"pve2": nhActive(nhNow - 5*3600),
				})
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			e := nhEntry(t, r, "pve2")
			if e.Health != pve.NodeHealthDead || !e.DeadOrFenced || e.Placeable || e.ProxyTarget ||
				e.Watchdog != pve.WatchdogUnknown {
				t.Errorf("crm=%s online=%d: %+v", crm, clusterOnline, e)
			}
			if e.CRMState != crm {
				t.Errorf("crm=%s online=%d: CRMState %q", crm, clusterOnline, e.CRMState)
			}
			if !r.Placeable("pve1") || !r.Placeable("pve3") {
				t.Errorf("crm=%s: the healthy nodes must stay placeable", crm)
			}
		}
	}
}

func TestNodeHealth_NodeAbsentFromNodeStatus(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager = nhManager(nhNow-5,
		map[string]string{"pve1": "online", "pve3": "online"},
		map[string]map[string]any{"pve1": nhActive(nhNow), "pve2": nhActive(nhNow), "pve3": nhActive(nhNow)})
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	e := nhEntry(t, r, "pve2")
	if e.Health != pve.NodeHealthUnknown || e.Placeable || !e.ProxyTarget || e.DeadOrFenced {
		t.Errorf("pve2: %+v", e)
	}

	c.manager = nhManager(nhNow-5,
		map[string]string{"pve1": "online", "pve2": "rebooting?", "pve3": "online"},
		map[string]map[string]any{"pve1": nhActive(nhNow), "pve2": nhActive(nhNow), "pve3": nhActive(nhNow)})
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if e := nhEntry(t, r, "pve2"); e.Health != pve.NodeHealthUnknown || e.Placeable {
		t.Errorf("an unrecognized CRM state is unknown: %+v", e)
	}
}

func TestNodeHealth_EntryClockUnreadable(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]*nhNodes{
		"error":    {err: errors.New("595 no connection")},
		"zero":     {now: 0},
		"negative": {now: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, err := pve.NodeHealth(t.Context(), nhHealthy(), n, pve.NodeHealthOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if r.HA != pve.HAViewUnreadable {
				t.Fatalf("HA = %q", r.HA)
			}
			for _, e := range r.Nodes {
				if e.Watchdog != pve.WatchdogUnknown || e.CRMState != "" {
					t.Errorf("an unmeasurable view must not feed any answer: %+v", e)
				}
			}
		})
	}
}

func TestNodeHealth_EntryNodeFromQuorumWhenNoLocalFlag(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[1] = nhNode("pve1", 1, false, "")
	r, n := nhRun(t, c, pve.NodeHealthOptions{})
	if r.EntryNode != "pve1" || r.HA != pve.HAViewManagerStatus || len(n.asked) != 1 || n.asked[0] != "pve1" {
		t.Errorf("entry=%q HA=%q asked=%v", r.EntryNode, r.HA, n.asked)
	}

	// With neither a local flag nor a quorum node, timestamps cannot be measured.
	c.manager = json.RawMessage(`{"manager_status":{"node_status":{"pve1":"online"},"timestamp":` +
		`999999995},"lrm_status":{}}`)
	r, n = nhRun(t, c, pve.NodeHealthOptions{})
	if r.EntryNode != "" || r.HA != pve.HAViewUnreadable || len(n.asked) != 0 {
		t.Errorf("entry=%q HA=%q asked=%v", r.EntryNode, r.HA, n.asked)
	}
}

func TestNodeHealth_TopLevelManagerFields(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager, _ = json.Marshal(map[string]any{
		"node_status": map[string]string{"pve1": "online", "pve2": "maintenance", "pve3": "online"},
		"timestamp":   nhNow - 1,
		"lrm_status": map[string]any{
			"pve1": nhActive(nhNow), "pve2": nhActive(nhNow), "pve3": nhActive(nhNow),
		},
	})
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus || !nhEntry(t, r, "pve2").Maintenance {
		t.Errorf("HA=%q pve2=%+v", r.HA, nhEntry(t, r, "pve2"))
	}
}

func TestNodeHealth_ToleratesWireScalarsAndBadRows(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows = append(c.rows, json.RawMessage(`not json`), json.RawMessage(`{"type":"node","name":""}`))
	c.rows[0] = json.RawMessage(`{"type":"cluster","quorate":"1"}`)
	c.rows[1] = json.RawMessage(`{"type":"node","name":"pve1","online":"1","local":"1"}`)
	c.manager = json.RawMessage(`{"manager_status":{"node_status":{"pve1":"online","pve2":"online","pve3":"online"},` +
		`"timestamp":"999999990"},"lrm_status":{"pve1":{"state":"active","mode":"active","timestamp":"999999995"},` +
		`"pve2":{"state":"active","mode":"active","timestamp":"999999995"},` +
		`"pve3":{"state":"active","mode":"active","timestamp":"999999995"}}}`)
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus || !r.Placeable("pve1") || len(r.Nodes) != 3 {
		t.Errorf("HA=%q nodes=%+v", r.HA, r.Nodes)
	}
}

func TestNodeHealth_QuorumRowTypeAlsoCounts(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.rows[0] = json.RawMessage(`{"type":"quorum","quorate":0}`)
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.Quorate || r.HA != pve.HAViewNotQuorate {
		t.Errorf("%+v", r)
	}
}

func TestNodeHealth_MalformedManagerStatusFallsBack(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]json.RawMessage{
		"not json":           json.RawMessage(`<html>`),
		"bad node_status":    json.RawMessage(`{"node_status":{"pve1":7}}`),
		"bad lrm_status":     json.RawMessage(`{"lrm_status":{"pve1":"x"}}`),
		"bad timestamp":      json.RawMessage(`{"node_status":{"pve1":"online"},"timestamp":{}}`),
		"bad nested status":  json.RawMessage(`{"manager_status":{"node_status":[1]}}`),
		"bad nested payload": json.RawMessage(`{"manager_status":{"timestamp":{}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nhHealthy()
			c.manager = body
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			if r.HA != pve.HAViewUnreadable {
				t.Errorf("HA = %q (%s)", r.HA, r.HAReason)
			}
			for _, e := range r.Nodes {
				if !e.Placeable {
					t.Errorf("fail-open to cluster status expected: %+v", e)
				}
			}
		})
	}
}

func TestNodeHealth_ClusterStatusFailureIsTheOnlyError(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.statusErr = errors.New("500 boom")
	if _, err := pve.NodeHealth(t.Context(), c, &nhNodes{now: nhNow}, pve.NodeHealthOptions{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNodeHealth_RejectsNilInputs(t *testing.T) {
	t.Parallel()
	n := &nhNodes{now: nhNow}
	var nilCtx context.Context // a nil context is the case under test
	if _, err := pve.NodeHealth(nilCtx, nhHealthy(), n, pve.NodeHealthOptions{}); err == nil {
		t.Error("nil ctx")
	}
	if _, err := pve.NodeHealth(t.Context(), nil, n, pve.NodeHealthOptions{}); err == nil {
		t.Error("nil cluster")
	}
	if _, err := pve.NodeHealth(t.Context(), nhHealthy(), nil, pve.NodeHealthOptions{}); err == nil {
		t.Error("nil nodes")
	}
}

func TestNodeHealthReport_NilReceiver(t *testing.T) {
	t.Parallel()
	var r *pve.NodeHealthReport
	if _, ok := r.Node("pve1"); ok || r.Placeable("pve1") || r.ProxyTarget("pve1") || r.DeadOrFenced("pve1") ||
		r.WatchdogOf("pve1") != pve.WatchdogUnknown {
		t.Error("a nil report knows nothing")
	}
}

func TestNodeHealth_TextFallbackHonorsOperatorTagAndDownNodes(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.manager = json.RawMessage(`{}`)
	c.rows[2] = nhNode("pve2", 1, false, "drain")
	c.rows[3] = nhNode("pve3", 0, false, "")
	idle, _ := json.Marshal(map[string]any{"type": "lrm", "node": "pve2", "status": "pve2 (idle, Mon Jan  1 2001)"})
	c.current = []json.RawMessage{idle}
	r, _ := nhRun(t, c, pve.NodeHealthOptions{MaintenanceTags: []string{"drain"}})
	if r.HA != pve.HAViewLRMText {
		t.Fatalf("HA = %q", r.HA)
	}
	if e := nhEntry(t, r, "pve2"); !e.Maintenance || e.Placeable || !e.ProxyTarget {
		t.Errorf("pve2: %+v", e)
	}
	if e := nhEntry(t, r, "pve3"); !e.DeadOrFenced || e.Placeable {
		t.Errorf("pve3: %+v", e)
	}
	if e := nhEntry(t, r, "pve1"); !e.Placeable {
		t.Errorf("a node with no lrm entry stays on cluster status: %+v", e)
	}
}

// nhIdleManager is the manager status of a cluster whose CRM went idle long
// ago: the CRM's last round is weeks old on the entry node's clock and holds no
// service, and each LRM, which keeps writing, waits for the agent lock. It is
// the shape PVE 9 shows once the last HA resource is gone.
func nhIdleManager(extra map[string]any) json.RawMessage {
	return nhIdleManagerLRM(extra, map[string]int64{"pve1": 2, "pve2": 2, "pve3": 2})
}

// nhOneNodeRound makes nhIdleManagerLRM's last round know only pve1, as on a
// one-node cluster or a standalone host.
var nhOneNodeRound = map[string]any{
	"master_node": "pve1",
	"node_status": map[string]string{"pve1": "online"},
}

// nhIdleCRMStamp is the idle CRM's last round on the entry node's clock.
const nhIdleCRMStamp = nhNow - 1908*3600

// nhIdleManagerLRM is nhIdleManager with one waiting LRM entry per node in
// lrmAge, each that many seconds old on the entry node's clock.
func nhIdleManagerLRM(extra map[string]any, lrmAge map[string]int64) json.RawMessage {
	crm := map[string]any{
		"master_node":    "pve2",
		"node_status":    map[string]string{"pve1": "online", "pve2": "fence", "pve3": "online"},
		"service_status": map[string]any{},
		"timestamp":      nhIdleCRMStamp,
	}
	for k, v := range extra {
		crm[k] = v
	}
	lrm := map[string]any{}
	for node, age := range lrmAge {
		lrm[node] = nhLRM("active", "wait_for_agent_lock", nhNow-age)
	}
	raw, _ := json.Marshal(map[string]any{
		"manager_status": crm,
		"lrm_status":     lrm,
		"quorum":         map[string]any{"node": "pve1", "quorate": "1"},
	})
	return raw
}

// nhIdleCurrent is the status/current answer that PVE gives through pve1 for
// the idle CRM of nhIdleManager: the entry node quorate, the master idle with
// the last round's timestamp, the fencing standby row, and an idle row for
// each LRM. Each extra row is appended.
func nhIdleCurrent(extra ...json.RawMessage) []json.RawMessage {
	row := func(v map[string]any) json.RawMessage {
		raw, _ := json.Marshal(v)
		return raw
	}
	rows := []json.RawMessage{
		row(map[string]any{"id": "quorum", "type": "quorum", "node": "pve1", "status": "OK", "quorate": 1}),
		row(map[string]any{
			"id": "master", "type": "master", "node": "pve2", "timestamp": nhIdleCRMStamp,
			"status": "pve2 (idle, Mon Aug 24 16:37:54 2026)",
		}),
		row(map[string]any{
			"id": "fencing", "type": "fencing", "node": "pve2", "status": "standby (CRM watchdog standby)",
			"armed-state": "standby",
		}),
	}
	for _, node := range []string{"pve1", "pve2", "pve3"} {
		rows = append(rows, row(map[string]any{
			"id": "lrm:" + node, "type": "lrm", "node": node, "timestamp": nhNow - 2,
			"status": node + " (idle, watchdog standby, Thu Nov 12 10:00:00 2026)",
		}))
	}
	return append(rows, extra...)
}

// nhIdleCluster is nhHealthy with the CRM idle weeks ago and no HA resource,
// as PVE shows it through every source.
func nhIdleCluster() *nhCluster {
	c := nhHealthy()
	c.resources = nil
	c.manager = nhIdleManager(nil)
	c.current = nhIdleCurrent()
	return c
}

// An idle CRM stops writing manager status, so its timestamp ages without
// limit on a healthy cluster. With no HA resource, an empty last round, and no
// disarm request, the view is idle and cluster status classifies the nodes, as
// it does when HA is not configured. The idle CRM's node_status is ignored.
func TestNodeHealth_IdleCRMWithNoResources(t *testing.T) {
	t.Parallel()
	c := nhIdleCluster()
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewIdle {
		t.Fatalf("HA = %q (%s); want idle", r.HA, r.HAReason)
	}
	for _, want := range []string{"idle", "no HA resources", "1908h0m0s old"} {
		if !strings.Contains(r.HAReason, want) {
			t.Errorf("HAReason %q does not name %q", r.HAReason, want)
		}
	}
	if c.resourceCalls != 1 || c.currentCalls != 1 {
		t.Errorf("the idle check reads the resource list and status/current once each; got %d and %d reads",
			c.resourceCalls, c.currentCalls)
	}
	for _, name := range []string{"pve1", "pve2", "pve3"} {
		e := nhEntry(t, r, name)
		if !e.ProxyTarget || !e.Placeable || e.DeadOrFenced || e.CRMState != "" || e.Watchdog != pve.WatchdogUnknown {
			t.Errorf("node %s: the idle CRM's last round leaked into the answer: %+v", name, e)
		}
	}
}

// Every condition PVE's CRM checks before it goes idle must hold, so an old
// timestamp that fails any of them stays stale and says why.
func TestNodeHealth_OldCRMThatIsNotIdleStaysStale(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(c *nhCluster)
		want  string
	}{
		"a configured resource": {
			setup: func(c *nhCluster) { c.resources = []json.RawMessage{haResource("vm:900", "started")} },
			want:  "the cluster has HA resources",
		},
		"an ignored resource": {
			setup: func(c *nhCluster) { c.resources = []json.RawMessage{haResource("vm:900", "ignored")} },
			want:  "the cluster has HA resources",
		},
		"a service in the last round": {
			setup: func(c *nhCluster) {
				c.manager = nhIdleManager(map[string]any{
					"service_status": map[string]any{"vm:900": haSvc("started", "pve2", "u-9")},
				})
			},
			want: "its last round still held services",
		},
		"a disarm request": {
			setup: func(c *nhCluster) {
				disarm := map[string]any{"mode": "freeze", "state": "disarmed"}
				c.manager = nhIdleManager(map[string]any{"disarm": disarm})
			},
			want: "its last round held a disarm request",
		},
		"an unreadable resource list": {
			setup: func(c *nhCluster) { c.resourcesErr = errors.New("403 permission denied") },
			want:  "the HA resources are unreadable",
		},
		"a service state that does not parse": {
			setup: func(c *nhCluster) { c.manager = nhIdleManager(map[string]any{"service_status": "garbage"}) },
			want:  "its last round's service state does not parse",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nhIdleCluster()
			tc.setup(c)
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			if r.HA != pve.HAViewStale {
				t.Fatalf("HA = %q (%s); want stale", r.HA, r.HAReason)
			}
			if !strings.Contains(r.HAReason, "CRM timestamp is 1908h0m0s old") ||
				!strings.Contains(r.HAReason, tc.want) {
				t.Errorf("HAReason %q does not name the age and %q", r.HAReason, tc.want)
			}
		})
	}
}

// A CRM timestamp from the entry node's future is a clock problem, never an
// idle CRM, and a fresh timestamp needs no resource read at all.
func TestNodeHealth_IdleCheckOnlyForAnOldTimestamp(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	c.resources = nil
	c.manager = nhManager(nhNow+600, map[string]string{"pve1": "online"}, nil)
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewStale || c.resourceCalls != 0 {
		t.Fatalf("a future timestamp: HA = %q (%s), resource reads %d; want stale and none",
			r.HA, r.HAReason, c.resourceCalls)
	}

	c = nhHealthy()
	c.resources = nil
	r, _ = nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus || c.resourceCalls != 0 {
		t.Fatalf("a fresh timestamp: HA = %q, resource reads %d; want manager_status and none", r.HA, c.resourceCalls)
	}
}

// A 200 answer with no data reaches the CPI as an empty resource list, so an
// empty list alone cannot prove that the cluster has no HA resources. The idle
// verdict also needs status/current, which PVE builds on the entry node from
// the same manager status and resource config, to report the entry node
// quorate, the master idle with the last round's timestamp, and no service.
// Any answer short of that keeps the view stale.
func TestNodeHealth_IdleNeedsStatusCurrentToConfirmIt(t *testing.T) {
	t.Parallel()
	row := func(v map[string]any) json.RawMessage {
		raw, _ := json.Marshal(v)
		return raw
	}
	withRow := func(typ string, replace map[string]any) []json.RawMessage {
		rows := nhIdleCurrent()
		for i, raw := range rows {
			var r map[string]any
			_ = json.Unmarshal(raw, &r)
			if r["type"] != typ {
				continue
			}
			for k, v := range replace {
				r[k] = v
			}
			rows[i] = row(r)
		}
		return rows
	}
	cases := map[string]struct {
		setup func(c *nhCluster)
		want  string
	}{
		"an answer with no data": {
			setup: func(c *nhCluster) { c.current = nil },
			want:  "status/current does not report the entry node quorate",
		},
		"an unreadable answer": {
			setup: func(c *nhCluster) { c.currentErr = errors.New("connection reset") },
			want:  "status/current is unreadable",
		},
		"a service row although the resource list came back empty": {
			setup: func(c *nhCluster) {
				c.current = nhIdleCurrent(row(map[string]any{
					"id": "service:vm:900", "type": "service", "sid": "vm:900", "status": "vm:900 (pve2, started)",
				}))
			},
			want: "status/current lists HA services",
		},
		"a master that is not idle": {
			setup: func(c *nhCluster) {
				c.current = withRow("master", map[string]any{"status": "pve2 (old timestamp - dead?, Mon Aug 24)"})
			},
			want: "status/current does not report the CRM master idle",
		},
		"a master row from another round": {
			setup: func(c *nhCluster) { c.current = withRow("master", map[string]any{"timestamp": nhNow - 3}) },
			want:  "status/current does not report the CRM master idle",
		},
		"no master row": {
			setup: func(c *nhCluster) {
				rows := nhIdleCurrent()
				c.current = slices.Delete(rows, 1, 2)
			},
			want: "status/current does not report the CRM master idle",
		},
		"a quorum row from another node": {
			setup: func(c *nhCluster) { c.current = withRow("quorum", map[string]any{"node": "pve3"}) },
			want:  "status/current does not report the entry node quorate",
		},
		"a quorum row that is not quorate": {
			setup: func(c *nhCluster) { c.current = withRow("quorum", map[string]any{"quorate": 0}) },
			want:  "status/current does not report the entry node quorate",
		},
		"a row that does not parse": {
			setup: func(c *nhCluster) { c.current = nhIdleCurrent(json.RawMessage(`"service"`)) },
			want:  "a status/current row does not parse",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nhIdleCluster()
			tc.setup(c)
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			if r.HA != pve.HAViewStale {
				t.Fatalf("HA = %q (%s); want stale", r.HA, r.HAReason)
			}
			if !strings.Contains(r.HAReason, "the CRM is not idle because "+tc.want) {
				t.Errorf("HAReason %q does not name %q", r.HAReason, tc.want)
			}
		})
	}
}

// A node that has just rejoined can report quorum before its copy of the
// cluster state has caught up, and that old copy shows an idle CRM even when
// the rest of the cluster has since configured a resource and woken the CRM.
// Its copies of the other nodes' LRM entries are old then, so the idle verdict
// needs every online node's LRM to be fresh and at least one fresh entry from
// a node other than the entry node. A single node, which has no other copy
// that could be newer, needs its own entry to be fresh, and only membership
// that proves a single node earns that rule.
func TestNodeHealth_IdleNeedsFreshLRMsFromTheRestOfTheCluster(t *testing.T) {
	t.Parallel()
	const old = 1800
	cases := map[string]struct {
		setup func(c *nhCluster)
		want  string // empty means the view is idle
	}{
		"every other LRM old, as in a copy from before a rejoin": {
			setup: func(c *nhCluster) {
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2, "pve2": old, "pve3": old})
			},
			want: "the LRM on pve2 has no fresh timestamp",
		},
		"one online LRM old": {
			setup: func(c *nhCluster) {
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2, "pve2": 2, "pve3": old})
			},
			want: "the LRM on pve3 has no fresh timestamp",
		},
		"the entry node's own LRM old": {
			setup: func(c *nhCluster) {
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": old, "pve2": 2, "pve3": 2})
			},
			want: "the LRM on pve1 has no fresh timestamp",
		},
		"only the entry node's LRM in a cluster of three": {
			setup: func(c *nhCluster) { c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2}) },
			want:  "no LRM on a node other than the entry node has a fresh timestamp",
		},
		"the only other online node offline in cluster status": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 0, false, "")}
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2, "pve2": old})
			},
			want: "no LRM on a node other than the entry node has a fresh timestamp",
		},
		"an offline node's old LRM beside a fresh peer": {
			setup: func(c *nhCluster) {
				c.rows[3] = nhNode("pve3", 0, false, "")
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2, "pve2": 2, "pve3": old})
			},
		},
		"a node that joined after the CRM went idle and has no LRM entry": {
			setup: func(c *nhCluster) {
				c.rows = append(c.rows, nhNode("pve4", 1, false, ""))
			},
		},
		"a one-node cluster with a fresh LRM": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{"pve1": 2})
			},
		},
		"a one-node cluster with an old LRM": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{"pve1": old})
			},
			want: "the LRM on pve1 has no fresh timestamp; start pve-ha-lrm on pve1 if it is stopped",
		},
		"a one-node cluster with no LRM entry": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{})
			},
			want: "the LRM on pve1 has no fresh timestamp; start pve-ha-lrm on pve1 if it is stopped",
		},
		"a standalone host with a fresh LRM": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{"pve1": 2})
			},
		},
		"an empty cluster status on a cluster of three": {
			setup: func(c *nhCluster) { c.rows = nil },
			want:  "cluster status lists no node, so no LRM can show the copy is current",
		},
		"one node in cluster status while the last round knew three": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
			},
			want: "the last round knew a node that cluster status does not list",
		},
		"one node in cluster status while the last round's LRMs name another": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{"pve1": 2, "pve2": 2})
			},
			want: "the last round knew a node that cluster status does not list",
		},
		"one node in cluster status while only the last round's node_status names three": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(nil, map[string]int64{"pve1": 2})
			},
			want: "the last round knew a node that cluster status does not list",
		},
		"one node in cluster status while the last round has a null node_status": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, true, "")}
				c.manager = nhIdleManagerLRM(map[string]any{"node_status": nil}, map[string]int64{"pve1": 2})
			},
			want: "the last round does not list the entry node",
		},
		"one node in cluster status that is not the entry node": {
			setup: func(c *nhCluster) {
				c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve2", 1, false, "")}
				c.manager = nhIdleManagerLRM(nhOneNodeRound, map[string]int64{"pve1": 2})
			},
			want: "cluster status does not list the entry node",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nhIdleCluster()
			tc.setup(c)
			r, _ := nhRun(t, c, pve.NodeHealthOptions{})
			if tc.want == "" {
				if r.HA != pve.HAViewIdle {
					t.Fatalf("HA = %q (%s); want idle", r.HA, r.HAReason)
				}
				return
			}
			if r.HA != pve.HAViewStale {
				t.Fatalf("HA = %q (%s); want stale", r.HA, r.HAReason)
			}
			if !strings.Contains(r.HAReason, "the CRM is not idle because "+tc.want) {
				t.Errorf("HAReason %q does not name %q", r.HAReason, tc.want)
			}
			if c.resourceCalls != 0 || c.currentCalls != 0 {
				t.Errorf("an LRM gap needs no further read; got %d resource and %d status/current reads",
					c.resourceCalls, c.currentCalls)
			}
		})
	}
}

// A change to node maintenance wakes the CRM, so a node that the idle CRM's
// last round left in maintenance is still in maintenance and takes no new
// work. The round's other node states are ignored.
func TestNodeHealth_IdleRoundKeepsHAMaintenance(t *testing.T) {
	t.Parallel()
	c := nhIdleCluster()
	c.manager = nhIdleManager(map[string]any{
		"node_status": map[string]string{"pve1": "online", "pve2": "fence", "pve3": "maintenance"},
	})
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewIdle {
		t.Fatalf("HA = %q (%s); want idle", r.HA, r.HAReason)
	}
	e := nhEntry(t, r, "pve3")
	if !e.Maintenance || e.Placeable || !e.ProxyTarget || e.DeadOrFenced || e.Health != pve.NodeHealthMaintenance ||
		e.CRMState != "" {
		t.Errorf("pve3 must stay in maintenance: %+v", e)
	}
	if e := nhEntry(t, r, "pve2"); !e.Placeable || e.DeadOrFenced || e.Maintenance {
		t.Errorf("pve2's fence state from the idle round must be ignored: %+v", e)
	}
}

// Only the idle check reads service_status, so a body there that does not
// parse leaves a fresh manager view trusted.
func TestNodeHealth_MalformedServiceStatusKeepsAFreshView(t *testing.T) {
	t.Parallel()
	c := nhHealthy()
	online := map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"}
	lrm := map[string]any{"pve1": nhActive(nhNow - 2), "pve2": nhActive(nhNow - 2), "pve3": nhActive(nhNow - 2)}
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{
			"master_node": "pve1", "node_status": online, "service_status": "garbage", "timestamp": nhNow - 5,
		},
		"lrm_status": lrm,
		"quorum":     map[string]any{"node": "pve1", "quorate": "1"},
	})
	c.manager = raw
	r, _ := nhRun(t, c, pve.NodeHealthOptions{})
	if r.HA != pve.HAViewManagerStatus {
		t.Fatalf("HA = %q (%s); want manager_status", r.HA, r.HAReason)
	}
	if e := nhEntry(t, r, "pve2"); !e.Placeable || e.CRMState != "online" {
		t.Errorf("pve2: %+v", e)
	}
}
