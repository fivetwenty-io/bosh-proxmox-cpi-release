package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const phSID = "vm:90000"

func (r *phRig) reconcile(t *testing.T) error {
	t.Helper()
	return r.pass().reconcile(context.Background(), 90000)
}

func (r *phRig) pin(t *testing.T) *ha.Rule {
	t.Helper()
	resp, err := r.cluster.naClusterStub.ListHaRules(context.Background(), &cluster.ListHaRulesParams{})
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	set, err := ha.ParseRuleList(resp)
	if err != nil {
		t.Fatalf("parse rules: %v", err)
	}
	return set.Find(naRuleNameFor(90000))
}

func (r *phRig) state() string {
	if res, ok := r.cluster.res[phSID]; ok {
		return res.state
	}
	return ""
}

func TestParkerHA_RegistersAWidelySharedParkerAsStopped(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := []string{"stage:ignored", "pin:" + naRuleNameFor(90000), "request:stopped"}
	if !slices.Equal(r.cluster.log.events, want) {
		t.Fatalf("events %v, want %v", r.cluster.log.events, want)
	}
	if r.state() != parkerHAStateStopped || r.cluster.res[phSID].comment != ourMarker() {
		t.Fatalf("resource %+v, want stopped with this Director's marker", r.cluster.res[phSID])
	}
	pin := r.pin(t)
	if pin == nil || pin.Strict {
		t.Fatalf("a parker every node reaches gets a non-strict record pin; got %+v", pin)
	}
	if got := r.client.protectionWrites(); len(got) != 0 {
		t.Fatalf("registration must leave the parker's protection alone; writes %v", got)
	}
}

func TestParkerHA_NarrowAccessSetGetsAStrictPinBeforeStopped(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ev := r.cluster.log.events
	stage, pinAt, stop := slices.Index(ev, "stage:ignored"), slices.Index(ev, "pin:"+naRuleNameFor(90000)),
		slices.Index(ev, "request:stopped")
	if stage < 0 || pinAt < 0 || stop < 0 || stage >= pinAt || pinAt >= stop {
		t.Fatalf("the order must be stage, pin, then stopped; events %v", ev)
	}
	pin := r.pin(t)
	if pin == nil || !pin.Strict || !slices.Equal(pin.NodeNames(), []string{"pve01", "pve02"}) {
		t.Fatalf("pin %+v, want strict on pve01 and pve02", pin)
	}
}

func TestParkerHA_LeavesParkersThatMayNotRegister(t *testing.T) {
	for name, volumes := range map[string][]string{
		"a node-local disk":          {"local-lvm:vm-90000-disk-0"},
		"a local and a shared disk":  {"nfs:vm-90000-disk-0", "local-lvm:vm-90000-disk-1"},
		"disks sharing a lone node":  {"ab:vm-90000-disk-0", "ac:vm-90000-disk-1"},
		"a storage the profile lost": {"gone:vm-90000-disk-0"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker(volumes...)})
			if err := r.reconcile(t); err != nil && name != "a storage the profile lost" {
				t.Fatalf("reconcile: %v", err)
			}
			if len(r.cluster.log.events) != 0 {
				t.Fatalf("an ineligible parker must stay unregistered; events %v", r.cluster.log.events)
			}
		})
	}
}

func TestParkerHA_ANodeDownLeavesTheParkerUnregistered(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	r.profile.Nodes[1].Online = false // pve02, the only other node of storage ab
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(r.cluster.log.events) != 0 {
		t.Fatalf("a parker with one placeable node must stay unregistered; events %v", r.cluster.log.events)
	}
}

func TestParkerHA_ANodeDownKeepsAStagedParkerStaged(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateIgnored, comment: ourMarker()}
	r.profile.Nodes[1].Online = false
	err := r.reconcile(t)
	if err == nil || !okToRetryCPIError(err) {
		t.Fatalf("a staged parker whose refusal may pass must defer retriably; got %v", err)
	}
	if len(r.cluster.log.events) != 0 || r.state() != parkerHAStateIgnored {
		t.Fatalf("the staged parker must stay staged; events %v state %q", r.cluster.log.events, r.state())
	}
}

func TestParkerHA_APermanentRefusalUnstagesAndUnpins(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("local-lvm:vm-90000-disk-0")})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateIgnored, comment: ourMarker()}
	r.cluster.setPin(true, "pve01,pve02", ourMarker()+" bosh-vols=ab")
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := r.cluster.res[phSID]; ok {
		t.Fatal("the staged resource must be removed")
	}
	if !slices.Contains(r.cluster.log.events, "unstage") ||
		!slices.Contains(r.cluster.log.events, "unpin:"+naRuleNameFor(90000)) {
		t.Fatalf("want the unstage and the pin removal; events %v", r.cluster.log.events)
	}
}

func TestParkerHA_FinishesAnEarlierStaging(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateIgnored, comment: ourMarker()}
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if r.state() != parkerHAStateStopped {
		t.Fatalf("state %q, want stopped", r.state())
	}
	if slices.Contains(r.cluster.log.events, "stage:ignored") {
		t.Fatalf("an existing staged resource must not be created again; events %v", r.cluster.log.events)
	}
}

func TestParkerHA_AnUnverifiedPinStaysIgnored(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("ab:vm-90000-disk-0")})
	r.cluster.mangle = func(p *cluster.CreateHaRulesParams) {
		wide := "pve01,pve02,pve03"
		p.Nodes = &wide
	}
	err := r.reconcile(t)
	if err == nil {
		t.Fatalf("a pin that does not hold must fail the registration; events %v", r.cluster.log.events)
	}
	if r.state() != parkerHAStateIgnored || slices.Contains(r.cluster.log.events, "request:stopped") {
		t.Fatalf("the parker must stay staged as ignored; state %q events %v", r.state(), r.cluster.log.events)
	}
}

func TestParkerHA_AStoppedRequestThatDoesNotLandIsRetriable(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateIgnored, comment: ourMarker()}
	r.cluster.updateErr = errors.New("500 ha config locked")
	err := r.reconcile(t)
	if err == nil || !okToRetryCPIError(err) || r.state() != parkerHAStateIgnored {
		t.Fatalf("a failed stopped request must defer retriably and stay ignored; err %v state %q", err, r.state())
	}
}

func TestParkerHA_ReadFailuresDeferWithoutWrites(t *testing.T) {
	t.Run("HA resource", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.cluster.getErr = crh500("pve02 unreachable")
		err := r.reconcile(t)
		if err == nil || !okToRetryCPIError(err) || len(r.cluster.log.events) != 0 {
			t.Fatalf("want a retriable deferral and no write; err %v events %v", err, r.cluster.log.events)
		}
	})
	t.Run("parker config", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.client.configErr[90000] = crh500("pmxcfs timeout")
		err := r.reconcile(t)
		if err == nil || !okToRetryCPIError(err) || len(r.cluster.log.events) != 0 {
			t.Fatalf("want a retriable deferral and no write; err %v events %v", err, r.cluster.log.events)
		}
	})
}

func TestParkerHA_ActionTable(t *testing.T) {
	foreign := haIdentity{Owner: "dir-other"}.marker()
	full := phParker("nfs:vm-90000-disk-0")
	empty := phParker()
	ours := func(state string) *phResource { return &phResource{state: state, comment: ourMarker()} }
	theirs := func(state string) *phResource { return &phResource{state: state, comment: foreign} }
	notParker := map[string]any{"scsi0": "nfs:vm-90000-disk-0"}
	mover := map[string]any{"tags": "bosh-parker;" + pve.DiskMoverTag}
	cases := map[string]struct {
		cfg  map[string]any
		res  *phResource
		want parkerHAAction
	}{
		"empty, unregistered":    {cfg: empty, want: parkerHANothing},
		"holding, unregistered":  {cfg: full, want: parkerHARegister},
		"holding, staged":        {cfg: full, res: ours("ignored"), want: parkerHAFinishStaging},
		"holding, stopped":       {cfg: full, res: ours("stopped"), want: parkerHANothing},
		"empty, registered":      {cfg: empty, res: ours("stopped"), want: parkerHARetire},
		"empty, staged":          {cfg: empty, res: ours("ignored"), want: parkerHARetire},
		"empty, another owner":   {cfg: empty, res: theirs("stopped"), want: parkerHANothing},
		"holding, another owner": {cfg: full, res: theirs("ignored"), want: parkerHANothing},
		"not a parker":           {cfg: notParker, want: parkerHANothing},
		"a mover":                {cfg: mover, want: parkerHANothing},
		"empty, record pending":  {cfg: phParkerWithRecord(), res: ours("stopped"), want: parkerHANothing},
		"only a record":          {cfg: phParkerWithRecord(), want: parkerHARegister},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: tc.cfg})
			if tc.res != nil {
				r.cluster.res[phSID] = tc.res
			}
			snap, err := r.pass().read(context.Background(), 90000)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got := r.pass().action(90000, snap); got != tc.want {
				t.Fatalf("action %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("gone", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{})
		snap, err := r.pass().read(context.Background(), 90000)
		if err != nil || !snap.gone || r.pass().action(90000, snap) != parkerHANothing {
			t.Fatalf("a parker that left the node needs nothing here; snap %+v err %v", snap, err)
		}
	})
}

// phParkerWithRecord is an empty parker whose provenance still records a disk
// an unfinished transfer is moving in.
func phParkerWithRecord() map[string]any {
	cfg := phParker()
	cfg["description"] = `<!--BOSH:{"bosh_parked_disks":{"bpd-0011223344556677":{"disk_cid":"pvd-x",` +
		`"parked_at":"2026-11-01T00:00:00Z","node":"pve01","volid":"nfs:vm-700-disk-1","slot":"scsi0"}}}-->`
	return cfg
}

func TestReconcileParkerHA_Gates(t *testing.T) {
	t.Run("off mode reads nothing", func(t *testing.T) {
		r := newPHRig(config.HAModeOff, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
		if r.client.listCalls != 0 || r.cluster.getCalls != 0 || len(r.cluster.log.events) != 0 {
			t.Fatalf("off mode must not touch PVE; lists %d gets %d events %v", r.client.listCalls,
				r.cluster.getCalls, r.cluster.log.events)
		}
	})
	t.Run("no director UUID", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.deps.RequestDirectorUUID = ""
		reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
		if r.client.listCalls != 0 || len(r.cluster.log.events) != 0 {
			t.Fatal("a request without a director UUID must change nothing")
		}
	})
	t.Run("observe only", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.profile.Effective = ha.EffectiveObserveOnly
		reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
		if len(r.cluster.log.events) != 0 {
			t.Fatalf("a profile that may not write must change nothing; events %v", r.cluster.log.events)
		}
	})
	t.Run("auto registers every eligible parker", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{
			90000: phParker("nfs:vm-90000-disk-0"),
			90001: phParker("local-lvm:vm-90001-disk-0"),
			700:   {"scsi0": "nfs:vm-700-disk-0"},
		})
		reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
		if r.state() != parkerHAStateStopped {
			t.Fatalf("parker 90000 must be registered; events %v", r.cluster.log.events)
		}
		if _, ok := r.cluster.res["vm:90001"]; ok {
			t.Fatal("the parker on node-local storage must stay unregistered")
		}
		if _, ok := r.cluster.res["vm:700"]; ok {
			t.Fatal("a VM that is not a parker must never be touched")
		}
	})
	t.Run("profile failure changes nothing", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("boom") }
		reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
		if len(r.cluster.log.events) != 0 {
			t.Fatal("an unresolved profile must change nothing")
		}
	})
}

func TestParkerHA_WatchdogNotArmedLeavesTheParker(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	r.profile.Quorum = ha.Quorum{Known: true, FragileNodes: []string{"pve01"}}
	if err := r.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(r.cluster.log.events) != 0 {
		t.Fatalf("an unarmed watchdog on a fragile cluster must leave the parker unregistered; events %v",
			r.cluster.log.events)
	}
}

// --------------------------------------------------------------------------
// Retirement
// --------------------------------------------------------------------------

// retireCluster answers the parker's HA resource with a comment of the
// test's choosing while listing no HA resource, so the teardown's HA gate
// reads the parker as one the CRM does not act on and the test exercises the
// retirement's own steps.
type retireCluster struct {
	*naClusterStub
	comment string
	state   string
	missing bool
}

func (c *retireCluster) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	if c.missing {
		return nil, errors.New("no such resource '" + sid + "'")
	}
	state, comment := "stopped", c.comment
	if c.state != "" {
		state = c.state
	}
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: &comment}, nil
}

type retireRig struct {
	nodes   *lrNodesStub
	qemu    *lrQEMUStub
	cluster *retireCluster
	r       *parkerHA
}

func newRetireRig(parkerCfg map[string]any) *retireRig {
	nodes := &lrNodesStub{}
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		out := map[string]any{}
		for k, v := range parkerCfg {
			out[k] = v
		}
		return out, nil
	}}
	rc := &retireCluster{naClusterStub: newNAStub(), comment: ourMarker()}
	deps := Deps{
		Config: lrTokenConfig(), RequestDirectorUUID: aaDirector, Logger: log.NewNopLogger(),
		PVE: gateClient{lrClient: &lrClient{nodes: nodes, qemu: q}, cluster: rc},
	}
	return &retireRig{nodes: nodes, qemu: q, cluster: rc, r: &parkerHA{
		deps: deps, logger: log.NewNopLogger(), node: "pve01", cfg: phParkerCfg(), owner: aaDirector,
	}}
}

// retire runs the retirement on a fresh read of the parker, the way the pass
// runs it under the parker's claim. between, when set, runs after the read.
func (rr *retireRig) retire(t *testing.T, ctx context.Context, between func()) error {
	t.Helper()
	snap, err := rr.r.read(ctx, 90000)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if between != nil {
		between()
	}
	return rr.r.retire(ctx, 90000, snap)
}

func (rr *retireRig) protectionWrites() []bool {
	var out []bool
	for _, p := range rr.nodes.updateParams {
		if p != nil && p.Protection != nil {
			out = append(out, *p.Protection)
		}
	}
	return out
}

func TestParkerRetire_DestroysAnEmptyParkerWithPurge(t *testing.T) {
	rr := newRetireRig(phParker())
	if err := rr.retire(t, crhCtx(), nil); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(rr.nodes.deleteCalls) != 1 {
		t.Fatalf("want one destroy, got %d", len(rr.nodes.deleteCalls))
	}
	if p := rr.nodes.deleteCalls[0].Purge; p == nil || !*p {
		t.Fatal("the destroy must purge, which removes the HA resource and its rule memberships")
	}
	if got := rr.protectionWrites(); !slices.Equal(got, []bool{false}) {
		t.Fatalf("protection writes %v, want one clear right before the destroy", got)
	}
	if rr.qemu.stopCalls != 0 || len(rr.nodes.stopCalls) != 0 {
		t.Fatal("a parker never runs, so the retirement sends no stop")
	}
}

func TestParkerRetire_RestoresProtectionWhenTheDestroyFails(t *testing.T) {
	rr := newRetireRig(phParker())
	rr.nodes.deleteErrs = []error{errors.New("500 storage busy")}
	if err := rr.retire(t, crhCtx(), nil); err == nil {
		t.Fatal("a failed destroy must fail the retirement")
	}
	if got := rr.protectionWrites(); len(got) < 2 || got[0] || !got[len(got)-1] {
		t.Fatalf("protection writes %v, want a clear and then a restore", got)
	}
}

func TestParkerRetire_Refusals(t *testing.T) {
	cases := map[string]struct {
		cfg       map[string]any
		mutate    func(*retireRig)
		between   func(*retireRig)
		retriable bool
		want      string
	}{
		"a disk came back": {cfg: phParker("nfs:vm-90000-disk-0"), retriable: true, want: "holds a disk again"},
		"snapshots": {cfg: phParker(), retriable: false, want: "carries snapshots",
			mutate: func(rr *retireRig) {
				rr.qemu.snapshots = []map[string]any{{"name": "current"}, {"name": "before-upgrade"}}
			}},
		"running": {cfg: phParker(), retriable: false, want: "never runs",
			mutate: func(rr *retireRig) { rr.qemu.status = "running" }},
		"not a parker": {cfg: map[string]any{}, retriable: false, want: "no longer a parker"},
		"no resource": {cfg: phParker(), retriable: false, want: "does not carry this Director's marker",
			mutate: func(rr *retireRig) { rr.cluster.missing = true }},
		"resource gone since the read": {cfg: phParker(), retriable: false, want: "is gone",
			between: func(rr *retireRig) { rr.cluster.missing = true }},
		"foreign marker": {cfg: phParker(), retriable: false, want: "does not carry this Director's marker",
			mutate: func(rr *retireRig) { rr.cluster.comment = haIdentity{Owner: "dir-other"}.marker() }},
		"foreign marker since the read": {cfg: phParker(), retriable: false,
			want:    "does not carry this Director's marker",
			between: func(rr *retireRig) { rr.cluster.comment = haIdentity{Owner: "dir-other"}.marker() }},
		"requests started": {cfg: phParker(), retriable: false, want: "only ever requests stopped",
			mutate: func(rr *retireRig) { rr.cluster.state = "started" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rr := newRetireRig(tc.cfg)
			if tc.mutate != nil {
				tc.mutate(rr)
			}
			var between func()
			if tc.between != nil {
				between = func() { tc.between(rr) }
			}
			err := rr.retire(t, crhCtx(), between)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to name %q", err, tc.want)
			}
			if okToRetryCPIError(err) != tc.retriable {
				t.Fatalf("retriable %v, want %v: %v", okToRetryCPIError(err), tc.retriable, err)
			}
			if len(rr.nodes.deleteCalls) != 0 || len(rr.protectionWrites()) != 0 {
				t.Fatalf("a refused retirement must not clear protection or destroy; protection %v destroys %d",
					rr.protectionWrites(), len(rr.nodes.deleteCalls))
			}
		})
	}
}

// TestSweepParkerPool_RunsTheParkerHAPass proves the pool sweep that follows
// every park runs the parker HA pass of the park's node, between the pool
// placement and the expired lock sweep.
func TestSweepParkerPool_RunsTheParkerHAPass(t *testing.T) {
	var order []string
	defer setPlaceParkersInPoolForTest(func(context.Context, pve.Client, *log.Logger, string, pve.ParkerConfig) error {
		order = append(order, "pool")
		return nil
	})()
	defer setSweepExpiredDiskLocksForTest(func(context.Context, pve.Client, *log.Logger) (int, error) {
		order = append(order, "locks")
		return 0, nil
	})()
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	sweepParkerPool(context.Background(), r.deps, phNode, phParkerCfg())
	if r.state() != parkerHAStateStopped {
		t.Fatalf("the sweep must register the parker; events %v", r.cluster.log.events)
	}
	if !slices.Equal(order, []string{"pool", "locks"}) {
		t.Fatalf("order %v", order)
	}
}
