package ha_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func resolve(t *testing.T, w *world, cfg *config.CPIConfig) *ha.Profile {
	t.Helper()
	p, err := ha.Resolve(context.Background(), w.deps(cfg))
	if err != nil {
		t.Fatalf("Resolve returned an error: %v", err)
	}
	return p
}

func TestResolveDecisionTable(t *testing.T) {
	cases := []struct {
		name       string
		build      func() *world
		wantFull   bool
		wantReason string // substring of Reason when the profile downgrades
	}{
		{name: "healthy three-node cluster", build: three, wantFull: true},
		{
			name: "no quorum",
			build: func() *world {
				w := three()
				w.notQuorate = true
				return w
			},
			wantReason: "does not report the cluster quorate",
		},
		{
			name: "standalone host",
			build: func() *world {
				w := nNodes(1)
				w.noClusterRow = true
				return w
			},
			wantReason: "not a cluster member",
		},
		{
			name: "no CRM master named",
			build: func() *world {
				w := three()
				w.managerBody = mustJSON(map[string]any{
					"manager_status": map[string]any{"node_status": map[string]string{"pve1": "online"}, "timestamp": now},
				})
				return w
			},
			wantReason: "no CRM master",
		},
		{
			name: "configured votes do not survive one node",
			build: func() *world {
				w := nNodes(2)
				return w
			},
			wantReason: "2 configured votes do not survive the loss of one node",
		},
		{
			name: "PVE below the floor",
			build: func() *world {
				w := three()
				w.nodes[2].version = "8.4.1"
				return w
			},
			wantReason: "below the PVE 9 floor",
		},
		{
			name: "missing Sys.Console",
			build: func() *world {
				w := three()
				w.perms = map[string]map[string]bool{"/": {"Sys.Audit": true}, "/vms": {}}
				return w
			},
			wantReason: "missing Sys.Console on /",
		},
		{
			name: "only one node online",
			build: func() *world {
				w := three()
				w.nodes[1].online = false
				w.nodes[2].online = false
				return w
			},
			wantReason: "fewer than two nodes are online (1 of 3)",
		},
	}
	for _, tc := range cases {
		for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				p := resolve(t, tc.build(), cfgMode(mode))
				if tc.wantFull {
					if p.Effective != ha.EffectiveFull || p.Reason != "" || !p.WritesAllowed() {
						t.Fatalf("effective=%s reason=%q gaps=%v, want full", p.Effective, p.Reason, p.Gaps)
					}
					if err := p.PrerequisiteError(); err != nil {
						t.Fatalf("a full profile must not fail a call: %v", err)
					}
					return
				}
				if p.Effective != ha.EffectiveObserveOnly || p.WritesAllowed() {
					t.Fatalf("effective=%s, want observe-only (gaps=%v)", p.Effective, p.Gaps)
				}
				if !strings.Contains(p.Reason, tc.wantReason) {
					t.Fatalf("reason %q does not contain %q", p.Reason, tc.wantReason)
				}
				err := p.PrerequisiteError()
				if mode == config.HAModeAuto && err != nil {
					t.Fatalf("auto must never fail a call for a missing prerequisite: %v", err)
				}
				if mode == config.HAModeManaged && (err == nil || !strings.Contains(err.Error(), tc.wantReason)) {
					t.Fatalf("managed must fail the call naming %q, got %v", tc.wantReason, err)
				}
			})
		}
	}
}

func TestManagedErrorNamesExpectedVotesAndQDevice(t *testing.T) {
	p := resolve(t, nNodes(2), cfgMode(config.HAModeManaged))
	err := p.PrerequisiteError()
	if err == nil {
		t.Fatal("managed on a two-node cluster without a QDevice must fail")
	}
	for _, want := range []string{"expected votes", "QDevice no"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestModeOffProbesNothing(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeOff))
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveOff || p.WritesAllowed() || p.Reason != "" {
		t.Fatalf("off profile = %+v", p)
	}
	if w.cluster.total() != 0 || len(w.nodesAP.calls) != 0 || w.storage.calls != 0 {
		t.Fatalf("mode off must not probe; cluster calls %v", w.cluster.calls)
	}
	if got := p.LogLine(); got != "ha: mode=off effective=off" {
		t.Fatalf("log line = %q", got)
	}
}

func TestUnsetConfigIsModeOff(t *testing.T) {
	w := three()
	p, err := ha.Resolve(context.Background(), w.deps(nil))
	if err != nil || p.Mode != config.HAModeOff || p.Effective != ha.EffectiveOff {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestProbeWhenOffReportsGapsButStaysOff(t *testing.T) {
	w := nNodes(2)
	d := w.deps(cfgMode(config.HAModeOff))
	d.ProbeWhenOff = true
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveOff || p.Reason != "" || p.WritesAllowed() {
		t.Fatalf("off must stay off: %+v", p)
	}
	if len(p.Gaps) == 0 || !strings.Contains(p.Gaps[0], "do not survive") {
		t.Fatalf("the doctor needs the gaps even in off mode: %v", p.Gaps)
	}
	if p.PrerequisiteError() != nil {
		t.Fatal("off mode never fails a call")
	}
}

func TestResolveRejectsCallerMistakes(t *testing.T) {
	var nilCtx context.Context
	if _, err := ha.Resolve(nilCtx, ha.Deps{}); err == nil {
		t.Fatal("nil ctx must be rejected")
	}
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	for name, mutate := range map[string]func(*ha.Deps){
		"cluster": func(d *ha.Deps) { d.Cluster = nil },
		"nodes":   func(d *ha.Deps) { d.Nodes = nil },
		"storage": func(d *ha.Deps) { d.Storage = nil },
	} {
		t.Run(name, func(t *testing.T) {
			dd := d
			mutate(&dd)
			if _, err := ha.Resolve(context.Background(), dd); err == nil {
				t.Fatal("a missing client must be rejected")
			}
		})
	}
}

func TestQuorumScenarios(t *testing.T) {
	cases := []struct {
		name          string
		build         func() *world
		wantFull      bool
		survives      bool
		known         bool
		expected      int
		participating int
		fragile       []string
		qdevice       ha.QDeviceStatus
		gap           string
	}{
		{
			name: "two nodes without a QDevice", build: func() *world { return nNodes(2) },
			survives: false, known: true, expected: 2, participating: 2, fragile: []string{"pve1", "pve2"},
			qdevice: ha.QDeviceNone, gap: "2 configured votes do not survive",
		},
		{
			name: "two nodes with a connected QDevice",
			build: func() *world {
				w := nNodes(2)
				w.qdeviceBody = json.RawMessage(`{"model":"net","votes":"1","connected":1}`)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 3, qdevice: ha.QDeviceConnected,
		},
		{
			name: "two nodes with a QDevice that is configured but disconnected",
			build: func() *world {
				w := nNodes(2)
				w.qdeviceBody = json.RawMessage(`{"model":"net","votes":1,"connected":0}`)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 2,
			fragile: []string{"pve1", "pve2"}, qdevice: ha.QDeviceDisconnected,
		},
		{
			name: "two nodes with a QDevice whose status is not stated",
			build: func() *world {
				w := nNodes(2)
				w.qdeviceBody = json.RawMessage(`{"model":"net"}`)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 2,
			fragile: []string{"pve1", "pve2"}, qdevice: ha.QDeviceUnknown,
		},
		{
			name: "two nodes and an unreadable QDevice endpoint",
			build: func() *world {
				w := nNodes(2)
				w.qdeviceErr = errors.New("500 qdevice")
				return w
			},
			survives: false, known: true, expected: 2, participating: 2, fragile: []string{"pve1", "pve2"},
			qdevice: ha.QDeviceUnknown, gap: "do not survive",
		},
		{
			name: "three nodes where one carries two votes",
			build: func() *world {
				w := three()
				w.nodes[0].votes = votes(2)
				return w
			},
			survives: false, known: true, expected: 4, participating: 4, fragile: []string{"pve1"},
			qdevice: ha.QDeviceNone, gap: "4 configured votes do not survive",
		},
		{
			name: "three nodes with one node down",
			build: func() *world {
				w := three()
				w.nodes[2].online = false
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 2,
			fragile: []string{"pve1", "pve2"}, qdevice: ha.QDeviceNone,
		},
		{
			name: "three healthy nodes", build: three,
			wantFull: true, survives: true, known: true, expected: 3, participating: 3, qdevice: ha.QDeviceNone,
		},
		{
			name: "a node with zero votes does not count toward expected votes",
			build: func() *world {
				w := nNodes(4)
				w.nodes[3].votes = votes(0)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 3, qdevice: ha.QDeviceNone,
		},
		{
			name: "configured votes unreadable",
			build: func() *world {
				w := three()
				w.configNodesErr = errors.New("500 config")
				return w
			},
			known: false, qdevice: ha.QDeviceNone, gap: "cannot prove the configured votes survive",
		},
		{
			name: "cluster config lists no nodes",
			build: func() *world {
				w := three()
				w.configNodesBody = json.RawMessage(`[]`)
				return w
			},
			known: false, qdevice: ha.QDeviceNone, gap: "cluster config lists no nodes",
		},
		{
			name: "a status node missing from the cluster config",
			build: func() *world {
				w := three()
				w.configNodesBody = json.RawMessage(`[{"node":"pve1","quorum_votes":1},{"node":"pve2","quorum_votes":1}]`)
				return w
			},
			known: false, qdevice: ha.QDeviceNone, gap: "node pve3 is not in the cluster config",
		},
		{
			name: "absent quorum_votes count as one",
			build: func() *world {
				w := three()
				w.configNodesBody = json.RawMessage(`[{"node":"pve1"},{"node":"pve2"},{"node":"pve3"}]`)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 3, participating: 3, qdevice: ha.QDeviceNone,
		},
		{
			name: "a configured node that status does not list counts as offline",
			build: func() *world {
				w := three()
				w.configNodesBody = json.RawMessage(
					`[{"node":"pve1","quorum_votes":1},{"node":"pve2","quorum_votes":1},{"node":"pve3","quorum_votes":1},` +
						`{"node":"pve4","quorum_votes":1},{"node":"pve5","quorum_votes":1}]`)
				return w
			},
			wantFull: true, survives: true, known: true, expected: 5, participating: 3,
			fragile: []string{"pve1", "pve2", "pve3"}, qdevice: ha.QDeviceNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resolve(t, tc.build(), cfgMode(config.HAModeAuto))
			q := p.Quorum
			if q.Known != tc.known {
				t.Fatalf("Known = %v (%s), want %v", q.Known, q.UnknownReason, tc.known)
			}
			if tc.known {
				if q.SurvivesOne != tc.survives || q.Expected != tc.expected || q.Participating != tc.participating {
					t.Fatalf("quorum = %+v, want survives=%v expected=%d participating=%d",
						q, tc.survives, tc.expected, tc.participating)
				}
				if strings.Join(q.FragileNodes, ",") != strings.Join(tc.fragile, ",") {
					t.Fatalf("fragile = %v, want %v", q.FragileNodes, tc.fragile)
				}
				if q.OneFailureFromLoss() != (len(tc.fragile) > 0) {
					t.Fatalf("OneFailureFromLoss = %v", q.OneFailureFromLoss())
				}
			}
			if got := p.QDevice.Status; got != tc.qdevice {
				t.Fatalf("QDevice status = %q, want %q", got, tc.qdevice)
			}
			if tc.wantFull != (p.Effective == ha.EffectiveFull) {
				t.Fatalf("effective=%s gaps=%v, wantFull=%v", p.Effective, p.Gaps, tc.wantFull)
			}
			if tc.gap != "" && !strings.Contains(strings.Join(p.Gaps, "; "), tc.gap) {
				t.Fatalf("gaps %v lack %q", p.Gaps, tc.gap)
			}
		})
	}
}

func TestQDeviceRecommendation(t *testing.T) {
	cases := []struct {
		name   string
		build  func() *world
		advice string
	}{
		{"two voting nodes without a QDevice", func() *world { return nNodes(2) }, "add-qdevice(2 voting nodes can split evenly)"},
		{"four voting nodes without a QDevice", func() *world { return nNodes(4) }, "add-qdevice(4 voting nodes can split evenly)"},
		{"six voting nodes without a QDevice", func() *world { return nNodes(6) }, "add-qdevice(6 voting nodes can split evenly)"},
		{
			"four voting nodes with a QDevice",
			func() *world {
				w := nNodes(4)
				w.qdeviceBody = json.RawMessage(`{"connected":1}`)
				return w
			},
			"",
		},
		{"three voting nodes without a QDevice", three, ""},
		{"five voting nodes without a QDevice", func() *world { return nNodes(5) }, ""},
		{
			"a zero-vote node does not count as voting",
			func() *world {
				w := nNodes(4)
				w.nodes[3].votes = votes(0)
				return w
			},
			"",
		},
		{
			"four nodes of which one has zero votes leaves an even voting count after another is added",
			func() *world {
				w := nNodes(5)
				w.nodes[4].votes = votes(0)
				return w
			},
			"add-qdevice(4 voting nodes can split evenly)",
		},
		{
			"an unreadable QDevice endpoint gives no advice",
			func() *world {
				w := nNodes(4)
				w.qdeviceErr = errors.New("500")
				return w
			},
			"",
		},
		{
			"unreadable votes give no advice",
			func() *world {
				w := nNodes(4)
				w.configNodesErr = errors.New("500")
				return w
			},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resolve(t, tc.build(), cfgMode(config.HAModeAuto))
			if p.Advice != tc.advice {
				t.Fatalf("advice = %q, want %q", p.Advice, tc.advice)
			}
			line := p.LogLine()
			if tc.advice == "" && strings.Contains(line, "advice=") {
				t.Fatalf("line %q must carry no advice", line)
			}
			if tc.advice != "" && !strings.HasSuffix(line, " advice="+tc.advice) {
				t.Fatalf("line %q must end with the advice", line)
			}
		})
	}
}

// The recommendation must never change the effective mode: a four-node cluster
// keeps full HA, and a two-node cluster stays observe-only for its quorum
// reason alone.
func TestQDeviceRecommendationNeverChangesTheMode(t *testing.T) {
	four := resolve(t, nNodes(4), cfgMode(config.HAModeManaged))
	if four.Advice == "" || four.Effective != ha.EffectiveFull || len(four.Gaps) != 0 || four.PrerequisiteError() != nil {
		t.Fatalf("four nodes: advice=%q effective=%s gaps=%v", four.Advice, four.Effective, four.Gaps)
	}
	two := resolve(t, nNodes(2), cfgMode(config.HAModeAuto))
	if two.Advice == "" || two.Effective != ha.EffectiveObserveOnly || len(two.Gaps) != 1 {
		t.Fatalf("two nodes: advice=%q effective=%s gaps=%v", two.Advice, two.Effective, two.Gaps)
	}
	if !strings.Contains(two.Gaps[0], "do not survive the loss of one node") {
		t.Fatalf("the only gap must be the quorum reason, got %q", two.Gaps[0])
	}
}

func TestUnreadableHASourceInEachMode(t *testing.T) {
	mutations := map[string]func(*world){
		"manager status errors": func(w *world) { w.managerErr = errors.New("500 manager status") },
		"manager status stale":  func(w *world) { w.crmStamp = now - 3600 },
		"manager status empty":  func(w *world) { w.managerBody = json.RawMessage(`{}`) },
	}
	// An empty status is a readable answer that names no CRM master, so
	// placement can still read the HA state, while a status that errors or is
	// stale cannot be read at all.
	unreadable := map[string]bool{"manager status errors": true, "manager status stale": true}
	for name, mutate := range mutations {
		t.Run(name+"/auto", func(t *testing.T) {
			w := three()
			mutate(w)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if p.Effective != ha.EffectiveObserveOnly || p.Reason == "" {
				t.Fatalf("auto must observe only: %s %q", p.Effective, p.Reason)
			}
			if p.PrerequisiteError() != nil {
				t.Fatal("auto must not fail the call")
			}
			if p.HAStateReadable() == unreadable[name] {
				t.Fatalf("HAStateReadable = %v", p.HAStateReadable())
			}
		})
		t.Run(name+"/managed", func(t *testing.T) {
			w := three()
			mutate(w)
			p := resolve(t, w, cfgMode(config.HAModeManaged))
			if p.Effective != ha.EffectiveObserveOnly || p.PrerequisiteError() == nil {
				t.Fatalf("managed must fail the call: %s", p.Effective)
			}
		})
		t.Run(name+"/off", func(t *testing.T) {
			w := three()
			mutate(w)
			p := resolve(t, w, cfgMode(config.HAModeOff))
			if p.Effective != ha.EffectiveOff || p.PrerequisiteError() != nil {
				t.Fatalf("off fails open: %s", p.Effective)
			}
		})
	}
}

func TestClusterStatusUnreadable(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeManaged))
	d.Cluster = &failingStatusCluster{fakeCluster: w.cluster}
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "cluster status unreadable") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
	if p.PrerequisiteError() == nil {
		t.Fatal("managed must fail the call")
	}
	if got := p.LogLine(); !strings.Contains(got, "votes=unknown") || !strings.Contains(got, "nodes=0/0") {
		t.Fatalf("line = %q", got)
	}
}

type failingStatusCluster struct{ *fakeCluster }

func (f *failingStatusCluster) ListStatus(_ context.Context) (*sdkClusterStatusResponse, error) {
	return nil, errors.New("connection refused")
}

func TestOtherProbeFailuresFailClosedOnlyWhereTheyGate(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*world)
		wantFull   bool
		wantReason string
		check      func(*testing.T, *ha.Profile)
	}{
		{
			name: "an online node whose version cannot be read",
			mutate: func(w *world) {
				w.nodes[1].versionErr = errors.New("500 version")
			},
			wantReason: "PVE version unreadable on pve2",
		},
		{
			name:       "HA rules endpoint does not answer",
			mutate:     func(w *world) { w.rulesErr = errors.New("501 not implemented") },
			wantReason: "rules endpoint did not answer",
			check: func(t *testing.T, p *ha.Profile) {
				if p.RulesSupported || p.HAVersionOK || p.DLBSupported {
					t.Fatalf("rules=%v ok=%v dlb=%v", p.RulesSupported, p.HAVersionOK, p.DLBSupported)
				}
			},
		},
		{
			name:       "token permissions unreadable",
			mutate:     func(w *world) { w.permsErr = errors.New("403") },
			wantReason: "missing Sys.Console on / (token permissions unreadable",
		},
		{
			name:       "client cannot read permissions",
			mutate:     func(w *world) { w.noPerms = true },
			wantReason: "this client cannot read token permissions",
		},
		{
			name:     "storage unreadable does not gate HA",
			mutate:   func(w *world) { w.storageErr = errors.New("500 storage") },
			wantFull: true,
			check: func(t *testing.T, p *ha.Profile) {
				if p.Topology != ha.TopologyUnknown || p.StoragesErr == "" {
					t.Fatalf("topology=%s err=%q", p.Topology, p.StoragesErr)
				}
			},
		},
		{
			name:     "cluster options unreadable do not gate HA",
			mutate:   func(w *world) { w.optionsErr = errors.New("500 options") },
			wantFull: true,
			check: func(t *testing.T, p *ha.Profile) {
				if p.CRS.Known || p.CRS.Err == "" || !strings.Contains(p.LogLine(), "crs=unknown") {
					t.Fatalf("crs = %+v, line %q", p.CRS, p.LogLine())
				}
			},
		},
		{
			name:     "replication unreadable does not gate HA",
			mutate:   func(w *world) { w.replicationErr = errors.New("500 replication") },
			wantFull: true,
			check: func(t *testing.T, p *ha.Profile) {
				if p.ReplicationErr == "" || len(p.Replication) != 0 {
					t.Fatalf("replication = %v, err %q", p.Replication, p.ReplicationErr)
				}
			},
		},
		{
			name:     "a node status that cannot be read leaves its CPU unknown without gating HA",
			mutate:   func(w *world) { w.nodes[2].statusErr = errors.New("595 no route") },
			wantFull: true,
			check: func(t *testing.T, p *ha.Profile) {
				if len(p.UnknownCPUNodes) != 1 || p.UnknownCPUNodes[0] != "pve3" {
					t.Fatalf("unknown CPU nodes = %v", p.UnknownCPUNodes)
				}
			},
		},
		{
			name:       "node health cannot be built",
			mutate:     func(w *world) { w.managerErr = errors.New("500") },
			wantReason: "HA manager status unreadable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := three()
			tc.mutate(w)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if tc.wantFull {
				if p.Effective != ha.EffectiveFull {
					t.Fatalf("effective=%s gaps=%v, want full", p.Effective, p.Gaps)
				}
			} else if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(strings.Join(p.Gaps, "; "), tc.wantReason) {
				t.Fatalf("effective=%s gaps=%v, want observe-only with %q", p.Effective, p.Gaps, tc.wantReason)
			}
			if tc.check != nil {
				tc.check(t, p)
			}
		})
	}
}

func TestOfflineNodeIsNotProbedAndDoesNotBlockTheFloor(t *testing.T) {
	w := three()
	w.nodes[2].online = false
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveFull || !p.HAVersionOK {
		t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
	}
	for k := range w.nodesAP.calls {
		if strings.HasSuffix(k, ":pve3") {
			t.Fatalf("the offline node was probed: %s", k)
		}
	}
	if p.MinVersion != "9.2.4" {
		t.Fatalf("min version = %q", p.MinVersion)
	}
}

func TestVersionFloorAndDLB(t *testing.T) {
	cases := []struct {
		name     string
		versions []string
		okFloor  bool
		dlb      bool
		min      string
	}{
		{"all 9.2", []string{"9.2.4", "9.2.0", "9.2.1"}, true, true, "9.2.0"},
		{"one node on 9.1", []string{"9.2.4", "9.1.5", "9.2.1"}, true, false, "9.1.5"},
		{"9.0 is at the floor", []string{"9.0", "9.0.3", "9.0.1"}, true, false, "9.0"},
		{"a node on 8.4", []string{"9.2.4", "8.4.1", "9.2.1"}, false, false, "8.4.1"},
		{"a suffix is ignored", []string{"9.2.4~2", "9.2.4", "9.3.0"}, true, true, "9.2.4~2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := three()
			for i, v := range tc.versions {
				w.nodes[i].version = v
			}
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if p.HAVersionOK != tc.okFloor || p.DLBSupported != tc.dlb || p.MinVersion != tc.min {
				t.Fatalf("ok=%v dlb=%v min=%q, want ok=%v dlb=%v min=%q",
					p.HAVersionOK, p.DLBSupported, p.MinVersion, tc.okFloor, tc.dlb, tc.min)
			}
		})
	}
}

func TestUnparseableVersionFailsClosed(t *testing.T) {
	w := three()
	w.nodes[0].version = "nine"
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "PVE version unreadable on pve1") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
	if !strings.Contains(p.Nodes[0].VersionErr, "unparseable") {
		t.Fatalf("version err = %q", p.Nodes[0].VersionErr)
	}
}

func TestCPUDomains(t *testing.T) {
	flagsA := "fpu sse2 avx avx2"
	cases := []struct {
		name        string
		mutate      func(*world)
		wantDomains [][]string // node sets, sorted
		wantUnknown []string
	}{
		{"identical nodes form one domain", func(w *world) {}, [][]string{{"pve1", "pve2", "pve3"}}, nil},
		{
			"same model and flags in a different order form one domain",
			func(w *world) {
				w.nodes[0].flags, w.nodes[1].flags, w.nodes[2].flags = flagsA, "avx2 avx sse2 fpu", "fpu fpu sse2 avx avx2"
			},
			[][]string{{"pve1", "pve2", "pve3"}}, nil,
		},
		{
			"same model with different flags split",
			func(w *world) {
				w.nodes[0].flags, w.nodes[1].flags, w.nodes[2].flags = flagsA, flagsA, "fpu sse2 avx"
			},
			[][]string{{"pve1", "pve2"}, {"pve3"}}, nil,
		},
		{
			"a node whose status carries no flags is its own domain beside nodes that report flags",
			func(w *world) { w.nodes[0].flags, w.nodes[1].flags = flagsA, flagsA },
			[][]string{{"pve1", "pve2"}, {"pve3"}}, nil,
		},
		{
			"nodes that all report no flags compare by model alone",
			func(w *world) { w.nodes[2].cpu = "Xeon B" },
			[][]string{{"pve1", "pve2"}, {"pve3"}}, nil,
		},
		{
			"a node whose status cannot be read belongs to no domain",
			func(w *world) { w.nodes[1].statusErr = errors.New("595") },
			[][]string{{"pve1", "pve3"}}, []string{"pve2"},
		},
		{
			"a node whose status carries no cpuinfo belongs to no domain",
			func(w *world) { w.nodes[0].noCPU = true },
			[][]string{{"pve2", "pve3"}}, []string{"pve1"},
		},
		{
			"an offline node belongs to no domain",
			func(w *world) { w.nodes[2].online = false },
			[][]string{{"pve1", "pve2"}}, []string{"pve3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := three()
			tc.mutate(w)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if len(p.CPUDomains) != len(tc.wantDomains) {
				t.Fatalf("domains = %+v, want %v", p.CPUDomains, tc.wantDomains)
			}
			for i, d := range p.CPUDomains {
				if strings.Join(d.Nodes, ",") != strings.Join(tc.wantDomains[i], ",") {
					t.Fatalf("domain %d = %v, want %v", i, d.Nodes, tc.wantDomains[i])
				}
			}
			if strings.Join(p.UnknownCPUNodes, ",") != strings.Join(tc.wantUnknown, ",") {
				t.Fatalf("unknown = %v, want %v", p.UnknownCPUNodes, tc.wantUnknown)
			}
			for _, n := range tc.wantUnknown {
				if _, ok := p.DomainOf(n); ok {
					t.Fatalf("%s must be in no domain", n)
				}
			}
			if d, ok := p.DomainOf(tc.wantDomains[0][0]); !ok || !d.Contains(tc.wantDomains[0][0]) {
				t.Fatalf("DomainOf(%s) = %+v, %v", tc.wantDomains[0][0], d, ok)
			}
		})
	}
}

func TestCPUDomainsDoNotChangeTheMode(t *testing.T) {
	w := three()
	w.nodes[0].cpu, w.nodes[1].cpu, w.nodes[2].cpu = "A", "B", "C"
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveFull || len(p.CPUDomains) != 3 {
		t.Fatalf("effective=%s domains=%d", p.Effective, len(p.CPUDomains))
	}
}

func TestTopology(t *testing.T) {
	nfs := storageRow("nfs1", "nfs", "images,iso", false, "")
	cephShared := storageRow("ceph", "rbd", "images,rootdir", true, "")
	lvm := storageRow("local-lvm", "lvmthin", "images,rootdir", false, "pve1")
	isoOnly := storageRow("local", "dir", "iso,backup", false, "")
	cases := []struct {
		name       string
		cfgTopo    string
		storages   []json.RawMessage
		wantTopo   string
		wantSource string
	}{
		{"all shared", "auto", []json.RawMessage{nfs, cephShared, isoOnly}, "shared", ha.TopologyFromProbe},
		{"none shared", "auto", []json.RawMessage{lvm, isoOnly}, "local", ha.TopologyFromProbe},
		{"mixed", "auto", []json.RawMessage{nfs, lvm}, "mixed", ha.TopologyFromProbe},
		{"no disk storage", "auto", []json.RawMessage{isoOnly}, ha.TopologyUnknown, ha.TopologyFromNothing},
		{"empty content counts as disk capable", "auto", []json.RawMessage{storageRow("x", "lvm", "", false, "")}, "local", ha.TopologyFromProbe},
		{"a disabled storage is ignored", "auto", []json.RawMessage{nfs, mustJSON(map[string]any{
			"storage": "old", "type": "lvm", "content": "images", "disable": 1,
		})}, "shared", ha.TopologyFromProbe},
		{"configured shared wins", "shared", []json.RawMessage{lvm}, "shared", ha.TopologyFromConfig},
		{"configured local wins", "local", []json.RawMessage{nfs}, "local", ha.TopologyFromConfig},
		{"configured mixed wins", "mixed", []json.RawMessage{nfs}, "mixed", ha.TopologyFromConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := three()
			w.storageBody = tc.storages
			p := resolve(t, w, cfgModeTopology(config.HAModeAuto, tc.cfgTopo))
			if p.Topology != tc.wantTopo || p.TopologySource != tc.wantSource {
				t.Fatalf("topology=%s source=%s, want %s %s", p.Topology, p.TopologySource, tc.wantTopo, tc.wantSource)
			}
		})
	}
}

func TestStorageDefinitionMalformedIsUnreadable(t *testing.T) {
	w := three()
	w.storageBody = []json.RawMessage{json.RawMessage(`"not an object"`)}
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.StoragesErr == "" || p.Topology != ha.TopologyUnknown || len(p.Storages) != 0 {
		t.Fatalf("storages=%v err=%q topology=%s", p.Storages, p.StoragesErr, p.Topology)
	}
}

func TestPrivilegesAreProbedOnTheRightPaths(t *testing.T) {
	w := three()
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if !p.SysConsole || !p.VMReplicate {
		t.Fatalf("console=%v replicate=%v", p.SysConsole, p.VMReplicate)
	}
	if strings.Join(w.permAP.paths, ",") != "/,/vms" {
		t.Fatalf("paths = %v", w.permAP.paths)
	}

	w = three()
	w.perms = map[string]map[string]bool{"/": {"Sys.Console": false}, "/vms": {"VM.Replicate": false}}
	p = resolve(t, w, cfgMode(config.HAModeAuto))
	if !p.SysConsole {
		t.Fatal("Sys.Console on / counts whether or not it propagates")
	}
	if p.VMReplicate {
		t.Fatal("VM.Replicate must propagate to the VM paths to count")
	}
}

func TestReplicationAndCRS(t *testing.T) {
	w := three()
	w.replication = []json.RawMessage{
		mustJSON(map[string]any{"id": "101-0", "guest": 101, "source": "pve1", "target": "pve2", "type": "local", "fail_count": 2}),
		mustJSON(map[string]any{"id": "100-0", "guest": "100", "source": "pve1", "target": "pve3"}),
	}
	w.optionsBody = json.RawMessage(`{"ha":"static","ha-rebalance-on-start":1}`)
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if len(p.Replication) != 2 || p.Replication[0].ID != "100-0" || p.Replication[0].Guest != 100 ||
		p.Replication[1].FailCount != 2 || p.Replication[1].Target != "pve2" {
		t.Fatalf("replication = %+v", p.Replication)
	}
	if !p.CRS.Known || p.CRS.Mode != "static" || !p.CRS.RebalanceOnStart || p.CRS.AutoRebalance {
		t.Fatalf("crs = %+v", p.CRS)
	}

	w = three()
	w.optionsBody = json.RawMessage(`"ha=basic,ha-auto-rebalance=1"`)
	p = resolve(t, w, cfgMode(config.HAModeAuto))
	if p.CRS.Mode != "basic" || !p.CRS.AutoRebalance || p.CRS.RebalanceOnStart {
		t.Fatalf("crs from property string = %+v", p.CRS)
	}

	w = three()
	p = resolve(t, w, cfgMode(config.HAModeAuto))
	if !p.CRS.Known || p.CRS.Mode != "basic" {
		t.Fatalf("an absent crs reads as basic: %+v", p.CRS)
	}

	w = three()
	w.optionsBody = json.RawMessage(`[1]`)
	p = resolve(t, w, cfgMode(config.HAModeAuto))
	if p.CRS.Known || p.CRS.Err == "" {
		t.Fatalf("a crs of the wrong shape must be unreadable: %+v", p.CRS)
	}

	w = three()
	w.replication = []json.RawMessage{json.RawMessage(`"bad"`)}
	p = resolve(t, w, cfgMode(config.HAModeAuto))
	if p.ReplicationErr == "" {
		t.Fatal("a malformed job must make the list unreadable")
	}
}

func TestQDeviceBodies(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		configured bool
		status     ha.QDeviceStatus
		votes      int
		read       bool
	}{
		{"empty object", `{}`, false, ha.QDeviceNone, 0, true},
		{"null", `null`, false, ha.QDeviceNone, 0, true},
		{"empty array", `[]`, false, ha.QDeviceNone, 0, true},
		{"connected flag", `{"connected":true}`, true, ha.QDeviceConnected, 1, true},
		{"disconnected flag", `{"connected":0,"votes":2}`, true, ha.QDeviceDisconnected, 2, true},
		{"status string", `{"status":"Connected"}`, true, ha.QDeviceConnected, 1, true},
		{"state string", `{"state":"disconnected"}`, true, ha.QDeviceDisconnected, 1, true},
		{"not connected string", `{"status":"not connected"}`, true, ha.QDeviceDisconnected, 1, true},
		{"nothing stated", `{"model":"net"}`, true, ha.QDeviceUnknown, 1, true},
		{"a status string we do not know", `{"status":"ACK"}`, true, ha.QDeviceUnknown, 1, true},
		{"votes as a string", `{"votes":"3","connected":1}`, true, ha.QDeviceConnected, 3, true},
		{"a zero vote count falls back to one", `{"votes":0,"connected":1}`, true, ha.QDeviceConnected, 1, true},
		{"a body that is not an object", `"text"`, false, ha.QDeviceUnknown, 0, false},
		{"votes of the wrong type", `{"votes":{"a":1}}`, false, ha.QDeviceUnknown, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := nNodes(2)
			w.qdeviceBody = json.RawMessage(tc.body)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			q := p.QDevice
			if q.Configured != tc.configured || q.Status != tc.status || q.Read != tc.read || (tc.configured && q.Votes != tc.votes) {
				t.Fatalf("qdevice = %+v", q)
			}
		})
	}
}

func TestEntryAndNodeFacts(t *testing.T) {
	w := three()
	w.entry = "pve2"
	w.nodes[0].votes = votes(1)
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.EntryNode != "pve2" || !p.Clustered || !p.Quorate || p.CRMMaster != "pve1" {
		t.Fatalf("entry=%s clustered=%v quorate=%v master=%s", p.EntryNode, p.Clustered, p.Quorate, p.CRMMaster)
	}
	if len(p.Nodes) != 3 || p.Nodes[2].Name != "pve3" || p.Nodes[2].Votes != 1 || !p.Nodes[2].VotesKnown ||
		p.Nodes[2].Version != "9.2.4" || !p.Nodes[2].CPU.Known {
		t.Fatalf("nodes = %+v", p.Nodes)
	}
	if got := strings.Join(p.OnlineNodes(), ","); got != "pve1,pve2,pve3" {
		t.Fatalf("online = %s", got)
	}
	if !p.HAStateReadable() || p.Health == nil {
		t.Fatal("a fresh manager status is readable")
	}
}

func TestManagerMasterShapes(t *testing.T) {
	for name, body := range map[string]string{
		"top level":  `{"master_node":"pve2","node_status":{"pve1":"online","pve2":"online","pve3":"online"},"timestamp":1000000000}`,
		"nested":     `{"manager_status":{"master_node":"pve2","node_status":{"pve1":"online","pve2":"online","pve3":"online"},"timestamp":1000000000}}`,
		"null body":  `null`,
		"empty list": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			w := three()
			w.managerBody = json.RawMessage(body)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			switch name {
			case "top level", "nested":
				if p.CRMMaster != "pve2" || p.Effective != ha.EffectiveFull {
					t.Fatalf("master=%q effective=%s gaps=%v", p.CRMMaster, p.Effective, p.Gaps)
				}
			default:
				if p.CRMMaster != "" || p.Effective != ha.EffectiveObserveOnly {
					t.Fatalf("master=%q effective=%s", p.CRMMaster, p.Effective)
				}
			}
		})
	}
}

func TestHAStateReadableWhenNotConfigured(t *testing.T) {
	var nilProfile *ha.Profile
	if nilProfile.WritesAllowed() || nilProfile.HAStateReadable() || nilProfile.PrerequisiteError() != nil {
		t.Fatal("a nil profile allows nothing and fails nothing")
	}
}

func TestMalformedClusterStatusRowsAreSkipped(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	d.Cluster = &rowsCluster{fakeCluster: w.cluster, extra: []json.RawMessage{
		json.RawMessage(`"junk"`), mustJSON(map[string]any{"type": "node", "online": 1}),
	}}
	p, err := ha.Resolve(context.Background(), d)
	if err != nil || p.Effective != ha.EffectiveFull || len(p.Nodes) != 3 {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestMultiNodeStatusWithoutClusterRowIsNotQuorate(t *testing.T) {
	w := three()
	w.noClusterRow = true
	p := resolve(t, w, cfgMode(config.HAModeManaged))
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "not a cluster member") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
}

func TestLogLineAndLogging(t *testing.T) {
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatal(err)
	}
	w := three()
	w.nodes[2].online = false
	w.optionsBody = json.RawMessage(`{"ha":"static"}`)
	d := w.deps(cfgMode(config.HAModeAuto))
	d.Log = logger
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := "ha: mode=auto effective=full topology=unknown pve=9.2.4 rules=yes crm=master@pve1 quorum=yes " +
		"votes=2/3 survives_one=yes qdevice=no nodes=2/3 sys_console=yes crs=static rebalance_on_start=no"
	if got := p.LogLine(); got != want {
		t.Fatalf("line =\n%s\nwant\n%s", got, want)
	}
	out := buf.String()
	if !strings.Contains(out, want) {
		t.Fatalf("the line was not logged: %s", out)
	}
	if !strings.Contains(out, "one failure from losing quorum") || !strings.Contains(out, "pve1,pve2") {
		t.Fatalf("the fragile-quorum warning was not logged: %s", out)
	}
}

func TestLogLineCarriesReasonAndAdvice(t *testing.T) {
	p := resolve(t, nNodes(4), cfgMode(config.HAModeAuto))
	if !strings.HasSuffix(p.LogLine(), " advice=add-qdevice(4 voting nodes can split evenly)") {
		t.Fatalf("line = %s", p.LogLine())
	}
	p = resolve(t, nNodes(2), cfgMode(config.HAModeAuto))
	line := p.LogLine()
	if !strings.Contains(line, "effective=observe-only reason=2 configured votes do not survive the loss of one node") ||
		!strings.Contains(line, "survives_one=no") || !strings.HasSuffix(line, "advice=add-qdevice(2 voting nodes can split evenly)") {
		t.Fatalf("line = %s", line)
	}
}

func TestNoFragileWarningInModeOff(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := log.NewLogger("debug", &buf)
	w := nNodes(2)
	d := w.deps(cfgMode(config.HAModeOff))
	d.ProbeWhenOff = true
	d.Log = logger
	if _, err := ha.Resolve(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "one failure from losing quorum") {
		t.Fatalf("mode off must not warn: %s", buf.String())
	}
}

// A cache wraps SlowSource and returns Cached facts. Resolve must read the
// QDevice's connection status again, because the cache never holds it.
type cachedSlow struct {
	facts   *ha.SlowFacts
	seen    []ha.NodeRef
	cluster string
}

func (c *cachedSlow) Slow(_ context.Context, cluster string, nodes []ha.NodeRef) (*ha.SlowFacts, error) {
	c.seen, c.cluster = nodes, cluster
	cp := *c.facts
	cp.Cached = true
	return &cp, nil
}

func TestCachedSlowFactsGetAFreshQDeviceStatus(t *testing.T) {
	w := nNodes(2)
	w.qdeviceBody = json.RawMessage(`{"votes":1,"connected":0}`)
	d := w.deps(cfgMode(config.HAModeAuto))

	live, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if live.QDevice.Status != ha.QDeviceDisconnected {
		t.Fatalf("live status = %s", live.QDevice.Status)
	}

	// The cached copy was taken while the QDevice was connected.
	stale := &ha.SlowFacts{
		Nodes: map[string]ha.NodeSlowFacts{
			"pve1": {Probed: true, Version: "9.2.4", CPU: ha.CPUInfo{Known: true, Model: "Xeon A"}},
			"pve2": {Probed: true, Version: "9.2.4", CPU: ha.CPUInfo{Known: true, Model: "Xeon A"}},
		},
		RulesSupported: true,
		ConfigVotes:    map[string]int{"pve1": 1, "pve2": 1},
		QDevice:        ha.QDevice{Read: true, Configured: true, Votes: 1, Status: ha.QDeviceConnected},
		SysConsole:     true,
	}
	src := &cachedSlow{facts: stale}
	d.Slow = src
	before := w.cluster.calls["qdevice"]
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if w.cluster.calls["qdevice"] != before+1 {
		t.Fatalf("the QDevice status must be read once on a cached call, calls %d -> %d", before, w.cluster.calls["qdevice"])
	}
	if p.QDevice.Status != ha.QDeviceDisconnected || p.Quorum.Participating != 2 || p.Quorum.Expected != 3 {
		t.Fatalf("qdevice=%+v quorum=%+v", p.QDevice, p.Quorum)
	}
	if len(src.seen) != 2 || src.seen[0].Name != "pve1" || !src.seen[0].Online {
		t.Fatalf("the source must be told the members: %+v", src.seen)
	}
	if src.cluster != "lab" {
		t.Fatalf("the source must be told the cluster's name from cluster status, got %q", src.cluster)
	}
	if p.Effective != ha.EffectiveFull {
		t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
	}
	if stale.QDevice.Status != ha.QDeviceConnected {
		t.Fatal("Resolve must not mutate the cached facts")
	}

	// The QDevice was removed after the cache was filled.
	w.qdeviceBody = json.RawMessage(`{}`)
	p, err = ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.QDevice.Configured || p.Effective != ha.EffectiveObserveOnly {
		t.Fatalf("a removed QDevice must show: %+v effective=%s", p.QDevice, p.Effective)
	}
}

func TestCachedFactsWithoutQDeviceDoNotReadItAgain(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	live, _ := ha.Resolve(context.Background(), d)
	_ = live
	d.Slow = &cachedSlow{facts: &ha.SlowFacts{
		Nodes: map[string]ha.NodeSlowFacts{
			"pve1": {Probed: true, Version: "9.2.4"}, "pve2": {Probed: true, Version: "9.2.4"}, "pve3": {Probed: true, Version: "9.2.4"},
		},
		RulesSupported: true, SysConsole: true,
		ConfigVotes: map[string]int{"pve1": 1, "pve2": 1, "pve3": 1},
		QDevice:     ha.QDevice{Read: true, Status: ha.QDeviceNone},
	}}
	before := w.cluster.calls["qdevice"]
	p, err := ha.Resolve(context.Background(), d)
	if err != nil || p.Effective != ha.EffectiveFull {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if w.cluster.calls["qdevice"] != before {
		t.Fatal("an absent QDevice needs no status read")
	}
}

type failingSlow struct{ err error }

func (f failingSlow) Slow(context.Context, string, []ha.NodeRef) (*ha.SlowFacts, error) {
	return nil, f.err
}

func TestSlowSourceFailureFailsClosed(t *testing.T) {
	for name, src := range map[string]ha.SlowSource{
		"error": failingSlow{err: errors.New("cache exploded")},
		"nil":   failingSlow{},
	} {
		t.Run(name, func(t *testing.T) {
			w := three()
			d := w.deps(cfgMode(config.HAModeManaged))
			d.Slow = src
			p, err := ha.Resolve(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if p.Effective != ha.EffectiveObserveOnly || p.PrerequisiteError() == nil {
				t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
			}
			if !strings.Contains(strings.Join(p.Gaps, "; "), "slow facts unavailable") {
				t.Fatalf("gaps = %v", p.Gaps)
			}
		})
	}
}

func TestNodeProbedByTheSourceButMissingFromItsFacts(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	d.Slow = &cachedSlow{facts: &ha.SlowFacts{
		Nodes:          map[string]ha.NodeSlowFacts{"pve1": {Probed: true, Version: "9.2.4"}},
		RulesSupported: true, SysConsole: true,
		ConfigVotes: map[string]int{"pve1": 1, "pve2": 1, "pve3": 1},
		QDevice:     ha.QDevice{Read: true, Status: ha.QDeviceNone},
	}}
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "PVE version unreadable on pve2, pve3") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
	if !strings.Contains(p.Nodes[1].VersionErr, "not probed") {
		t.Fatalf("node err = %q", p.Nodes[1].VersionErr)
	}
}

func noMasterBody() json.RawMessage {
	return mustJSON(map[string]any{
		"manager_status": map[string]any{"node_status": map[string]string{"pve1": "online"}, "timestamp": now},
	})
}

func TestIdleCRMOnAnEmptyResourceListIsNotAGap(t *testing.T) {
	w := three()
	w.managerBody = noMasterBody()
	w.noResources = true
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveFull || len(p.Gaps) != 0 {
		t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
	}
	if !strings.Contains(p.Advice, "HA stack idle, no resources configured yet") {
		t.Fatalf("advice = %q", p.Advice)
	}
}

func TestIdleCRMWithResourcesKeepsTheGap(t *testing.T) {
	w := three()
	w.managerBody = noMasterBody()
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "no CRM master") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
	if strings.Contains(p.Advice, "idle") {
		t.Fatalf("advice = %q", p.Advice)
	}
}

func TestIdleCRMWithUnreadableResourcesKeepsTheGap(t *testing.T) {
	w := three()
	w.managerBody = noMasterBody()
	w.noResources = true
	w.resourcesErr = errors.New("500 resources")
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "no CRM master") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
}

// A CRM that went idle after the last resource was removed still names its old
// master, and its timestamp is weeks old. With no resource left, that is the
// idle stack PVE reports, not a missing master, so the profile stays full.
func TestIdleCRMThatNamesItsOldMasterIsNotAGap(t *testing.T) {
	w := three()
	w.crmStamp = now - 1908*3600
	w.noResources = true
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveFull || len(p.Gaps) != 0 {
		t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
	}
	if !p.HAStateReadable() || p.Health.HA != pve.HAViewIdle {
		t.Fatalf("an idle CRM must read as idle: view=%s readable=%v", p.Health.HA, p.HAStateReadable())
	}
	// The resources were removed, so "yet" would be wrong here.
	if !strings.Contains(p.Advice, "HA stack idle, no resources configured") || strings.Contains(p.Advice, "yet") {
		t.Fatalf("advice = %q", p.Advice)
	}
	// No node runs the master of an idle CRM, so the old master_node is not
	// reported as one.
	if p.CRMMaster != "" || !strings.Contains(p.LogLine(), " crm=idle ") {
		t.Fatalf("master=%q line=%q", p.CRMMaster, p.LogLine())
	}
}

// An idle CRM writes nothing, so the profile cannot tell an idle stack from a
// dead one by its manager status. Both idle cases reach full only when
// pve-ha-crm runs on the entry node, and a stopped or unreadable service keeps
// a gap, so auto mode never registers resources that no CRM would process.
func TestIdleCRMNeedsARunningCRMService(t *testing.T) {
	cases := map[string]struct {
		setup func(w *world)
		want  string
	}{
		"stopped after the last resource was removed": {
			setup: func(w *world) { w.crmStamp = now - 1908*3600; w.crmService = "dead" },
			want:  `pve-ha-crm on pve1 is not running (state "dead")`,
		},
		"stopped on a cluster that never had a resource": {
			setup: func(w *world) { w.managerBody = noMasterBody(); w.crmService = "dead" },
			want:  `pve-ha-crm on pve1 is not running (state "dead")`,
		},
		"unreadable": {
			setup: func(w *world) { w.crmStamp = now - 1908*3600; w.crmServiceErr = errors.New("403 forbidden") },
			want:  "the state of pve-ha-crm on pve1 is unreadable: 403 forbidden",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := three()
			w.noResources = true
			tc.setup(w)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, tc.want) {
				t.Fatalf("effective=%s reason=%q; want observe-only naming %q", p.Effective, p.Reason, tc.want)
			}
			if strings.Contains(p.Advice, "idle") || strings.Contains(p.LogLine(), "crm=idle") {
				t.Fatalf("a dead CRM must not read as idle: advice=%q line=%q", p.Advice, p.LogLine())
			}
		})
	}
}

// The same old timestamp with a resource configured is a stale CRM, which
// keeps the gap.
func TestOldCRMWithResourcesKeepsTheGap(t *testing.T) {
	w := three()
	w.crmStamp = now - 1908*3600
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(p.Reason, "HA view stale") {
		t.Fatalf("effective=%s reason=%q", p.Effective, p.Reason)
	}
	if p.HAStateReadable() || strings.Contains(p.Advice, "idle") {
		t.Fatalf("a stale CRM must not read as idle: readable=%v advice=%q", p.HAStateReadable(), p.Advice)
	}
}

// movingEntryCluster reports a different local node on every status read after
// the first, which models cluster status and node health disagreeing.
type movingEntryCluster struct {
	*fakeCluster
	reads int
}

func (m *movingEntryCluster) ListStatus(ctx context.Context) (*sdkClusterStatusResponse, error) {
	m.reads++
	if m.reads == 1 {
		return m.fakeCluster.ListStatus(ctx)
	}
	m.w.entry = "pve2"
	return m.fakeCluster.ListStatus(ctx)
}

func TestDifferingEntryNodesAreAGap(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	d.Cluster = &movingEntryCluster{fakeCluster: w.cluster}
	p, err := ha.Resolve(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveObserveOnly || !strings.Contains(strings.Join(p.Gaps, "|"), "entry node pve1 but node health from pve2") {
		t.Fatalf("effective=%s gaps=%v", p.Effective, p.Gaps)
	}
}
