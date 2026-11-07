package pve_test

import (
	"context"
	"encoding/json"
	"errors"
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

// nhHealthy is a three-node cluster, entry pve1, with a fresh CRM and LRMs.
func nhHealthy() *nhCluster {
	return &nhCluster{
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
