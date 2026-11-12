package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haCluster is the node health fake plus the HA resource list and a cluster
// status whose local node can change between reads.
type haCluster struct {
	*nhCluster
	resources    []json.RawMessage
	resourcesErr error
	nilResources bool

	// perSid answers GET /cluster/ha/resources/{sid} with a requested state per
	// sid, and perSidErr fails every such read. A sid absent from perSid and no
	// error answers a resource with no state.
	perSid    map[string]string
	perSidErr error

	statusCalls int
	// laterRows, when set, replaces the cluster status rows from the second
	// read on, which models an endpoint wrapper that moved to another node.
	laterRows []json.RawMessage
	laterErr  error
}

func (c *haCluster) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	c.statusCalls++
	if c.statusCalls > 1 && c.laterErr != nil {
		return nil, c.laterErr
	}
	if c.statusCalls > 1 && c.laterRows != nil {
		resp := sdkcluster.ListStatusResponse(c.laterRows)
		return &resp, nil
	}
	return c.nhCluster.ListStatus(ctx)
}

func (c *haCluster) ListHaResources(
	_ context.Context, _ *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	if c.resourcesErr != nil {
		return nil, c.resourcesErr
	}
	if c.nilResources {
		return nil, nil
	}
	resp := sdkcluster.ListHaResourcesResponse(c.resources)
	return &resp, nil
}

func (c *haCluster) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	if c.perSidErr != nil {
		return nil, c.perSidErr
	}
	resp := &sdkcluster.GetHaResourcesResponse{Sid: sid}
	if state, ok := c.perSid[sid]; ok {
		resp.State = &state
	}
	return resp, nil
}

// haSvc is one service_status entry.
func haSvc(state, node, uid string) map[string]any {
	svc := map[string]any{"state": state, "node": node}
	if uid != "" {
		svc["uid"] = uid
	}
	return svc
}

// haManagerBody builds the body of GET /cluster/ha/status/manager_status with a
// service_status map and the node that served the call.
func haManagerBody(
	crmTS int64, served string, nodeStatus map[string]string, lrm map[string]map[string]any,
	services map[string]any,
) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{
			"master_node": "pve1", "node_status": nodeStatus, "service_status": services, "timestamp": crmTS,
		},
		"lrm_status": lrm,
		"quorum":     map[string]any{"node": served, "quorate": "1"},
	})
	return raw
}

func haResource(sid, state string) json.RawMessage {
	row := map[string]any{"sid": sid, "type": "vm"}
	if state != "" {
		row["state"] = state
	}
	raw, _ := json.Marshal(row)
	return raw
}

// haScene is a three-node cluster entered through pve1 with the VM's service
// on pve2. Every node is online with a fresh LRM until a test changes it.
type haScene struct {
	rawState string
	nodes    map[string]string
	lrm      map[string]map[string]any
	rows     []json.RawMessage
	crmTS    int64
	requests string // configured request; empty means no HA resource
	noCRM    bool   // the service is absent from service_status
	uid      string
	svcNode  string
}

func newHAScene(raw string) *haScene {
	return &haScene{
		rawState: raw,
		nodes:    map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		lrm: map[string]map[string]any{
			"pve1": nhActive(nhNow - 2), "pve2": nhActive(nhNow - 2), "pve3": nhActive(nhNow - 2),
		},
		rows: []json.RawMessage{
			nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
		},
		crmTS:    nhNow - 5,
		requests: "started",
		uid:      "u-1",
		svcNode:  "pve2",
	}
}

func (s *haScene) cluster() *haCluster {
	services := map[string]any{"vm:200": haSvc("started", "pve3", "other")}
	if !s.noCRM {
		services["vm:100"] = haSvc(s.rawState, s.svcNode, s.uid)
	}
	c := &haCluster{nhCluster: &nhCluster{
		rows:    s.rows,
		manager: haManagerBody(s.crmTS, "pve1", s.nodes, s.lrm, services),
	}}
	c.resources = []json.RawMessage{haResource("vm:200", "started")}
	if s.requests != "" {
		c.resources = append(c.resources, haResource("vm:100", s.requests))
	}
	return c
}

func haRead(t *testing.T, c *haCluster, kind pve.HADecisionKind) (*pve.HAServiceState, *nhNodes) {
	t.Helper()
	n := &nhNodes{now: nhNow}
	st, err := pve.ReadHAServiceState(t.Context(), c, n, pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: kind})
	if err != nil {
		t.Fatalf("ReadHAServiceState: %v", err)
	}
	return st, n
}

func haWant(t *testing.T, st *pve.HAServiceState, want pve.HADisposition) {
	t.Helper()
	got, why := st.Disposition()
	if got != want {
		t.Fatalf("disposition = %q (%s); want %q; state %+v", got, why, want, st)
	}
	if why == "" {
		t.Errorf("disposition %q carries no reason", got)
	}
}

func TestHAServiceState_RawStateOnOnlineNode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want pve.HADisposition
	}{
		{"stopped", pve.HAProceed},
		{"started", pve.HAProceed},
		{"error", pve.HAProceed},
		{"request_stop", pve.HAWaitForMotion},
		{"request_start", pve.HAWaitForMotion},
		{"request_start_balance", pve.HAWaitForMotion},
		{"migrate", pve.HAWaitForMotion},
		{"relocate", pve.HAWaitForMotion},
		{"freeze", pve.HAWaitForMotion},
		{"fence", pve.HADefer},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			for _, kind := range []pve.HADecisionKind{pve.HADecisionLive, pve.HADecisionRecovery} {
				st, _ := haRead(t, newHAScene(tc.raw).cluster(), kind)
				if !st.Known || st.Raw != tc.raw || st.UID != "u-1" || st.Node != "pve2" || !st.InCRM ||
					!st.Configured || st.Requested != "started" || !st.LRMFresh || st.EntryNode != "pve1" {
					t.Fatalf("model: %+v", st)
				}
				haWant(t, st, tc.want)
			}
		})
	}
}

func TestHAServiceState_RecoveryOnFencedNode(t *testing.T) {
	t.Parallel()
	scene := func() *haScene {
		s := newHAScene("recovery")
		s.nodes["pve2"] = "unknown"
		s.rows[2] = nhNode("pve2", 0, false, "")
		// A powered-off node cannot refresh its LRM, so the entry is hours old.
		s.lrm["pve2"] = nhActive(nhNow - 4*3600)
		return s
	}
	st, _ := haRead(t, scene().cluster(), pve.HADecisionRecovery)
	if !st.Known || st.LRMFresh {
		t.Fatalf("a recovery decision must accept the failed source's stale LRM: %+v", st)
	}
	haWant(t, st, pve.HAGraceWindow)

	// A live-node decision never runs the grace window; it defers.
	st, _ = haRead(t, scene().cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HADefer)

	// A recovery whose node the CRM does not report fenced is not provable.
	s := newHAScene("recovery")
	st, _ = haRead(t, s.cluster(), pve.HADecisionRecovery)
	haWant(t, st, pve.HAUnknown)

	// A recovery still needs a fresh CRM view.
	s = scene()
	s.crmTS = nhNow - 600
	st, _ = haRead(t, s.cluster(), pve.HADecisionRecovery)
	if st.Known {
		t.Fatalf("a stale CRM must make the model unknown: %+v", st)
	}
	haWant(t, st, pve.HAUnknown)
}

func TestHAServiceState_NodeStateOverridesTheServiceState(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"started", "stopped", "error", "migrate", "freeze"} {
		// Node state fence defers whatever the service state is.
		s := newHAScene(raw)
		s.nodes["pve2"] = "fence"
		st, _ := haRead(t, s.cluster(), pve.HADecisionRecovery)
		haWant(t, st, pve.HADefer)

		// A node that is unknown before its fence has not been proven dead.
		s = newHAScene(raw)
		s.nodes["pve2"] = "unknown"
		s.lrm["pve2"] = nhActive(nhNow - 3600)
		st, _ = haRead(t, s.cluster(), pve.HADecisionRecovery)
		haWant(t, st, pve.HADefer)

		// A node that cluster status lists offline is not online.
		s = newHAScene(raw)
		s.rows[2] = nhNode("pve2", 0, false, "")
		st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
		haWant(t, st, pve.HADefer)
	}
}

func TestHAServiceState_DisplayStatesAreNeverRawStates(t *testing.T) {
	t.Parallel()
	for _, display := range []string{"queued", "clearing error flag", "starting", "stopping", "deleting", "disabled", ""} {
		st, _ := haRead(t, newHAScene(display).cluster(), pve.HADecisionLive)
		if st.Known {
			t.Errorf("%q was read as a raw state: %+v", display, st)
		}
		haWant(t, st, pve.HAUnknown)
		if display != "" && !strings.Contains(st.Reason, "not a raw CRM state") {
			t.Errorf("%q: reason %q", display, st.Reason)
		}
	}
}

func TestHAServiceState_ConfiguredButNotYetInTheCRM(t *testing.T) {
	t.Parallel()
	s := newHAScene("")
	s.noCRM = true
	st, _ := haRead(t, s.cluster(), pve.HADecisionLive)
	if !st.Known || st.InCRM || !st.Configured || st.Raw != "" || st.Node != "pve2" {
		t.Fatalf("model: %+v", st)
	}
	haWant(t, st, pve.HAWaitForMotion)

	// The old request name enabled reads as started.
	s.requests = "enabled"
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	if st.Requested != "started" {
		t.Errorf("requested = %q; want enabled read as started", st.Requested)
	}

	// A disabled request is also not ignored.
	s.requests = "disabled"
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAWaitForMotion)
}

func TestHAServiceState_IgnoredResource(t *testing.T) {
	t.Parallel()
	s := newHAScene("")
	s.noCRM = true
	s.requests = "ignored"
	st, _ := haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAProceed)

	// The CRM never fences an ignored resource, so a dead node still defers on
	// the node's own state.
	s.rows[2] = nhNode("pve2", 0, false, "")
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HADefer)

	// With a CRM view in play, the node's LRM must still be fresh.
	s = newHAScene("")
	s.noCRM = true
	s.requests = "ignored"
	s.lrm["pve2"] = nhActive(nhNow - 300)
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAUnknown)
}

func TestHAServiceState_NotHAManaged(t *testing.T) {
	t.Parallel()
	s := newHAScene("")
	s.noCRM = true
	s.requests = ""
	st, _ := haRead(t, s.cluster(), pve.HADecisionLive)
	if !st.Known || st.Managed() || st.Configured || st.InCRM {
		t.Fatalf("model: %+v", st)
	}
	haWant(t, st, pve.HAProceed)

	// PVE gives no proof of death for an unmanaged VM on a node that is not online.
	s.rows[2] = nhNode("pve2", 0, false, "")
	st, _ = haRead(t, s.cluster(), pve.HADecisionRecovery)
	haWant(t, st, pve.HADefer)

	// A cluster without HA has no CRM view at all and still answers.
	c := &haCluster{nhCluster: &nhCluster{rows: newHAScene("").rows, manager: json.RawMessage(`{}`)}}
	c.resources = nil
	st, n := haRead(t, c, pve.HADecisionLive)
	haWant(t, st, pve.HAProceed)
	if st.CRMPresent || len(n.asked) != 0 {
		t.Errorf("with no CRM there is no clock to read: %+v asked=%v", st, n.asked)
	}
}

func TestHAServiceState_LRMFreshnessDependsOnTheDecision(t *testing.T) {
	t.Parallel()
	s := newHAScene("started")
	s.lrm["pve2"] = nhActive(nhNow - 31)
	st, _ := haRead(t, s.cluster(), pve.HADecisionLive)
	if !st.Known || st.LRMFresh {
		t.Fatalf("model: %+v", st)
	}
	// A live-node row needs a fresh LRM for either kind.
	haWant(t, st, pve.HAUnknown)
	st, _ = haRead(t, s.cluster(), pve.HADecisionRecovery)
	haWant(t, st, pve.HAUnknown)

	// A missing LRM entry reads the same way.
	s = newHAScene("started")
	delete(s.lrm, "pve2")
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAUnknown)

	// A motion row on a stale LRM is unknown too, because waiting is a live decision.
	s = newHAScene("migrate")
	s.lrm["pve2"] = nhActive(nhNow - 90)
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAUnknown)

	// A timestamp in the future beyond the skew limit is stale.
	s = newHAScene("started")
	s.lrm["pve2"] = nhActive(nhNow + 60)
	st, _ = haRead(t, s.cluster(), pve.HADecisionLive)
	haWant(t, st, pve.HAUnknown)
}

func TestHAServiceState_ListWithoutStateFallsBackToTheResource(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		perSid     map[string]string
		perSidErr  error
		wantKnown  bool
		wantReq    string
		wantReason string
	}{
		"resource supplies the state":  {perSid: map[string]string{"vm:100": "started"}, wantKnown: true, wantReq: "started"},
		"enabled maps to started":      {perSid: map[string]string{"vm:100": "enabled"}, wantKnown: true, wantReq: "started"},
		"resource read fails":          {perSidErr: errors.New("500 boom"), wantReason: "no requested state"},
		"resource has no state either": {wantReason: "no requested state"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newHAScene("started").cluster()
			c.resources = []json.RawMessage{haResource("vm:100", "")}
			c.perSid, c.perSidErr = tc.perSid, tc.perSidErr
			st, _ := haRead(t, c, pve.HADecisionLive)
			if st.Known != tc.wantKnown || st.Requested != tc.wantReq {
				t.Fatalf("known=%v requested=%q reason=%q", st.Known, st.Requested, st.Reason)
			}
			if !tc.wantKnown {
				haWant(t, st, pve.HAUnknown)
				if !strings.Contains(st.Reason, tc.wantReason) {
					t.Errorf("reason = %q", st.Reason)
				}
			}
		})
	}
}

func TestHAServiceState_FieldsThatMakeTheModelUnknown(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		scene   func(*haScene)
		cluster func(*haCluster)
	}{
		"stale CRM timestamp":     {scene: func(s *haScene) { s.crmTS = nhNow - 61 }},
		"future CRM timestamp":    {scene: func(s *haScene) { s.crmTS = nhNow + 30 }},
		"missing CRM timestamp":   {scene: func(s *haScene) { s.crmTS = 0 }},
		"missing uid":             {scene: func(s *haScene) { s.uid = "" }},
		"missing node":            {scene: func(s *haScene) { s.svcNode = "" }},
		"cluster not quorate":     {scene: func(s *haScene) { s.rows[0] = nhClusterRow(0) }},
		"service node not listed": {scene: func(s *haScene) { s.svcNode = "pve9" }},
		"missing request": {cluster: func(c *haCluster) {
			c.resources = []json.RawMessage{haResource("vm:100", "")}
		}},
		"unreadable resource row": {cluster: func(c *haCluster) {
			c.resources = append(c.resources, json.RawMessage(`"not an object"`))
		}},
		"unreadable resources": {cluster: func(c *haCluster) { c.resourcesErr = errors.New("500 boom") }},
		"no resources body":    {cluster: func(c *haCluster) { c.nilResources = true }},
		"unreadable manager status": {cluster: func(c *haCluster) {
			c.managerErr = errors.New("503 no master")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newHAScene("started")
			if tc.scene != nil {
				tc.scene(s)
			}
			c := s.cluster()
			if tc.cluster != nil {
				tc.cluster(c)
			}
			st, _ := haRead(t, c, pve.HADecisionLive)
			if st.Known {
				t.Fatalf("the model must be unknown: %+v", st)
			}
			haWant(t, st, pve.HAUnknown)
			if st.Reason == "" {
				t.Error("an unknown model carries a reason")
			}
		})
	}
}

func TestHAServiceState_EntryNodeMustNotChange(t *testing.T) {
	t.Parallel()
	// Manager status served by another node than the one the report names.
	c := newHAScene("started").cluster()
	c.manager = haManagerBody(nhNow-5, "pve3",
		map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		map[string]map[string]any{"pve2": nhActive(nhNow)}, map[string]any{"vm:100": haSvc("started", "pve2", "u")})
	st, _ := haRead(t, c, pve.HADecisionLive)
	if st.Known || !strings.Contains(st.Reason, "pve3") {
		t.Fatalf("two entry nodes must make the model unknown: %+v", st)
	}

	// The wrapper moves to pve3 after the first reads, so the closing check differs.
	c = newHAScene("started").cluster()
	c.laterRows = []json.RawMessage{
		nhClusterRow(1), nhNode("pve1", 1, false, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, true, ""),
	}
	st, _ = haRead(t, c, pve.HADecisionLive)
	if st.Known || !strings.Contains(st.Reason, "entry node changed") {
		t.Fatalf("an entry change during the read must make the model unknown: %+v", st)
	}

	// Losing quorum during the read does the same.
	c = newHAScene("started").cluster()
	c.laterRows = []json.RawMessage{
		nhClusterRow(0), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
	}
	st, _ = haRead(t, c, pve.HADecisionLive)
	if st.Known {
		t.Fatalf("a quorum loss during the read must make the model unknown: %+v", st)
	}

	// Manager status that does not name its node cannot be tied to the entry.
	c = newHAScene("started").cluster()
	c.manager = haManagerBody(nhNow-5, "",
		map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		map[string]map[string]any{"pve2": nhActive(nhNow)}, map[string]any{"vm:100": haSvc("started", "pve2", "u")})
	st, _ = haRead(t, c, pve.HADecisionLive)
	if st.Known {
		t.Fatalf("manager status without a quorum node must be unknown: %+v", st)
	}
}

func TestHAServiceState_ReusesACallerSuppliedHealthReport(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	n := &nhNodes{now: nhNow}
	report, err := pve.NodeHealth(t.Context(), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := c.statusCalls
	st, err := pve.ReadHAServiceState(t.Context(), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive, Health: report})
	if err != nil {
		t.Fatal(err)
	}
	haWant(t, st, pve.HAProceed)
	if got := c.statusCalls - before; got != 1 {
		t.Errorf("the supplied report replaces the health read, so only the closing check reads cluster status; got %d", got)
	}

	// A report whose HA view was not trusted cannot ground a decision.
	c = newHAScene("started").cluster()
	c.manager = haManagerBody(nhNow-500, "pve1", map[string]string{"pve1": "online"}, nil, nil)
	st, _ = haRead(t, c, pve.HADecisionLive)
	if st.Known || !strings.Contains(st.Reason, "stale") {
		t.Errorf("a stale HA view must make the model unknown: %+v", st)
	}
}

func TestHAServiceState_ScanNodeFallsBackWhenTheCRMNamesNone(t *testing.T) {
	t.Parallel()
	// The CRM's node wins over a scan row that lags a migration.
	s := newHAScene("started")
	s.svcNode = "pve3"
	c := s.cluster()
	st, _ := haRead(t, c, pve.HADecisionLive)
	if st.Node != "pve3" || st.ScanNode != "pve2" {
		t.Errorf("node = %q scan = %q; want the CRM's node", st.Node, st.ScanNode)
	}

	// With no CRM entry and no scan node there is nothing to decide about.
	s = newHAScene("")
	s.noCRM = true
	n := &nhNodes{now: nhNow}
	st, err := pve.ReadHAServiceState(t.Context(), s.cluster(), n,
		pve.HAServiceStateInput{VMID: 100, Kind: pve.HADecisionLive})
	if err != nil || st.Known {
		t.Fatalf("a model with no node must be unknown: %+v err=%v", st, err)
	}
}

func TestHAServiceState_TopLevelCRMFieldsAreAccepted(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	raw, _ := json.Marshal(map[string]any{
		"node_status":    map[string]string{"pve1": "online", "pve2": "online", "pve3": "online"},
		"lrm_status":     map[string]map[string]any{"pve2": nhActive(nhNow)},
		"service_status": map[string]any{"vm:100": haSvc("stopped", "pve2", "u-9")},
		"timestamp":      nhNow - 1,
		"quorum":         map[string]any{"node": "pve1"},
	})
	c.manager = raw
	st, _ := haRead(t, c, pve.HADecisionLive)
	if !st.Known || st.Raw != "stopped" || st.UID != "u-9" {
		t.Fatalf("model: %+v", st)
	}
}

func TestReadHAServiceState_RejectsWrongCalls(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	n := &nhNodes{now: nhNow}
	good := pve.HAServiceStateInput{VMID: 100, Kind: pve.HADecisionLive}
	bad := map[string]func() error{
		"nil ctx": func() error {
			var ctx context.Context
			_, err := pve.ReadHAServiceState(ctx, c, n, good)
			return err
		},
		"nil cluster": func() error {
			_, err := pve.ReadHAServiceState(t.Context(), nil, n, good)
			return err
		},
		"nil nodes": func() error {
			_, err := pve.ReadHAServiceState(t.Context(), c, nil, good)
			return err
		},
		"zero vmid": func() error {
			_, err := pve.ReadHAServiceState(t.Context(), c, n, pve.HAServiceStateInput{Kind: pve.HADecisionLive})
			return err
		},
		"no kind": func() error {
			_, err := pve.ReadHAServiceState(t.Context(), c, n, pve.HAServiceStateInput{VMID: 100})
			return err
		},
	}
	for name, call := range bad {
		if err := call(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestHAServiceState_NilReceiver(t *testing.T) {
	t.Parallel()
	var st *pve.HAServiceState
	if st.Managed() {
		t.Error("a nil model is not managed")
	}
	if got, why := st.Disposition(); got != pve.HAUnknown || why == "" {
		t.Errorf("disposition = %q (%s)", got, why)
	}
	if pve.HAServiceSID(7) != "vm:7" {
		t.Error("service ID")
	}
}

func TestHAServiceState_SourceFailuresAreUnknownNotErrors(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, c *haCluster, n *nhNodes, health *pve.NodeHealthReport) *pve.HAServiceState {
		t.Helper()
		st, err := pve.ReadHAServiceState(t.Context(), c, n,
			pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive, Health: health})
		if err != nil {
			t.Fatalf("a source failure must be an unknown model, not an error: %v", err)
		}
		if st.Known {
			t.Fatalf("the model must be unknown: %+v", st)
		}
		haWant(t, st, pve.HAUnknown)
		return st
	}
	report := func(t *testing.T, c *haCluster) *pve.NodeHealthReport {
		t.Helper()
		r, err := pve.NodeHealth(t.Context(), c, &nhNodes{now: nhNow}, pve.NodeHealthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("cluster status unreadable", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		c.statusErr = errors.New("500 boom")
		read(t, c, &nhNodes{now: nhNow}, nil)
	})
	t.Run("entry clock unreadable", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		r := report(t, c)
		st := read(t, c, &nhNodes{err: errors.New("500 no clock")}, r)
		if !strings.Contains(st.Reason, "clock") {
			t.Errorf("reason = %q", st.Reason)
		}
	})
	t.Run("manager status does not parse", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		r := report(t, c)
		c.manager = json.RawMessage(`{"manager_status":{"service_status":[1]}}`)
		read(t, c, &nhNodes{now: nhNow}, r)
	})
	t.Run("manager status is not json", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		r := report(t, c)
		c.manager = json.RawMessage(`[`)
		read(t, c, &nhNodes{now: nhNow}, r)
	})
	t.Run("crm entry does not parse", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		c.manager = haManagerBody(nhNow-5, "pve1", map[string]string{"pve1": "online", "pve2": "online"},
			map[string]map[string]any{"pve2": nhActive(nhNow)}, map[string]any{"vm:100": "garbage"})
		read(t, c, &nhNodes{now: nhNow}, nil)
	})
	t.Run("closing status read fails", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		c.laterErr = errors.New("500 boom")
		read(t, c, &nhNodes{now: nhNow}, nil)
	})
	t.Run("manager status vanished after the report", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		r := report(t, c)
		c.manager = json.RawMessage(`{}`)
		read(t, c, &nhNodes{now: nhNow}, r)
	})
	t.Run("manager status appeared after the report", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		c.manager = json.RawMessage(`{}`)
		r := report(t, c)
		c.manager = newHAScene("started").cluster().manager
		read(t, c, &nhNodes{now: nhNow}, r)
	})
	t.Run("report names no entry node", func(t *testing.T) {
		t.Parallel()
		c := newHAScene("started").cluster()
		c.rows = []json.RawMessage{nhClusterRow(1), nhNode("pve1", 1, false, ""), nhNode("pve2", 1, false, "")}
		c.manager = json.RawMessage(`{}`)
		r := report(t, c)
		if r.EntryNode != "" {
			t.Fatalf("setup: entry = %q", r.EntryNode)
		}
		read(t, c, &nhNodes{now: nhNow}, r)
	})
	t.Run("report does not mention the node", func(t *testing.T) {
		t.Parallel()
		scene := newHAScene("")
		scene.noCRM = true
		c := scene.cluster()
		r := report(t, c)
		st, err := pve.ReadHAServiceState(t.Context(), c, &nhNodes{now: nhNow},
			pve.HAServiceStateInput{VMID: 100, VMNode: "pve9", Kind: pve.HADecisionLive, Health: r})
		if err != nil || st.Known {
			t.Fatalf("st=%+v err=%v", st, err)
		}
	})
}

func TestHAServiceState_CarriesTheClockAndRoundStamps(t *testing.T) {
	t.Parallel()
	st, _ := haRead(t, newHAScene("started").cluster(), pve.HADecisionLive)
	if st.EntryTime != nhNow || st.CRMStamp != nhNow-5 || st.LRMStamp != nhNow-2 {
		t.Fatalf("stamps entry=%d crm=%d lrm=%d", st.EntryTime, st.CRMStamp, st.LRMStamp)
	}
	scene := newHAScene("started")
	delete(scene.lrm, "pve2")
	st, _ = haRead(t, scene.cluster(), pve.HADecisionLive)
	if st.LRMStamp != 0 || st.LRMFresh {
		t.Fatalf("a node without an LRM entry carries a stamp: %+v", st)
	}
}

// haIdleCluster is a quorate cluster whose CRM went idle weeks ago: no HA
// resource, an empty last round, and the VM on pve2 not HA-managed.
func haIdleCluster() *haCluster {
	return &haCluster{nhCluster: &nhCluster{
		rows: []json.RawMessage{
			nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
		},
		manager: nhIdleManager(nil),
		current: nhIdleCurrent(),
	}}
}

// A VM on a cluster whose CRM is idle is not HA-managed, and its state is
// known, so a live or a recovery decision proceeds. This is the delete that a
// stale reading of the idle CRM deferred without end.
func TestHAServiceState_IdleCRMIsKnownAndNotManaged(t *testing.T) {
	t.Parallel()
	for _, kind := range []pve.HADecisionKind{pve.HADecisionLive, pve.HADecisionRecovery} {
		st, _ := haRead(t, haIdleCluster(), kind)
		if !st.Known || st.Managed() || st.CRMPresent || st.CRMStamp != 0 || st.Node != "pve2" {
			t.Fatalf("kind %d: want a known, unmanaged state on pve2 with no CRM view: %+v", kind, st)
		}
		haWant(t, st, pve.HAProceed)
	}
}

// A resource configured after the health read found the CRM idle is one the
// CRM has not processed yet, so the decision waits for motion rather than
// proceeding.
func TestHAServiceState_ResourceAddedWhileTheCRMIsIdleWaits(t *testing.T) {
	t.Parallel()
	c := haIdleCluster()
	n := &nhNodes{now: nhNow}
	report, err := pve.NodeHealth(t.Context(), c, n, pve.NodeHealthOptions{})
	if err != nil || report.HA != pve.HAViewIdle {
		t.Fatalf("NodeHealth: HA = %v, err = %v; want idle", report, err)
	}
	c.resources = []json.RawMessage{haResource("vm:100", "started")}
	st, err := pve.ReadHAServiceState(t.Context(), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive, Health: report})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Known || !st.Configured || st.InCRM {
		t.Fatalf("want a configured service the CRM has not processed: %+v", st)
	}
	haWant(t, st, pve.HAWaitForMotion)
}

// The model reads manager status again. A service or a disarm request there
// means the CRM was not idle after all, so the state is unknown.
func TestHAServiceState_IdleReportWithABusyManagerStatusIsUnknown(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"a service":        {"service_status": map[string]any{"vm:100": haSvc("started", "pve2", "u-1")}},
		"a disarm request": {"disarm": map[string]any{"mode": "freeze", "state": "disarmed"}},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := haIdleCluster()
			n := &nhNodes{now: nhNow}
			report, err := pve.NodeHealth(t.Context(), c, n, pve.NodeHealthOptions{})
			if err != nil || report.HA != pve.HAViewIdle {
				t.Fatalf("NodeHealth: HA = %v, err = %v; want idle", report, err)
			}
			c.manager = nhIdleManager(extra)
			st, err := pve.ReadHAServiceState(t.Context(), c, n,
				pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionRecovery, Health: report})
			if err != nil {
				t.Fatal(err)
			}
			if st.Known || !strings.Contains(st.Reason, "found the CRM idle") {
				t.Fatalf("want an unknown state that names the idle report: %+v", st)
			}
			haWant(t, st, pve.HAUnknown)
		})
	}
}

// An old CRM timestamp on a cluster that still has an HA resource is stale, so
// a VM that is not HA-managed there stays unknown, as before.
func TestHAServiceState_OldCRMWithAnotherResourceIsUnknown(t *testing.T) {
	t.Parallel()
	c := haIdleCluster()
	c.resources = []json.RawMessage{haResource("vm:200", "started")}
	st, _ := haRead(t, c, pve.HADecisionRecovery)
	if st.Known || !strings.Contains(st.Reason, "the HA view is stale") ||
		!strings.Contains(st.Reason, "the cluster has HA resources") {
		t.Fatalf("want an unknown state from a stale view: %+v", st)
	}
	haWant(t, st, pve.HAUnknown)
}
