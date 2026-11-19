package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haMover plays PVE HA for one VM: it answers the state reads and settles of
// the barrier and of a move, and it takes the migrate request. The VM moves to
// the requested node on the next read, which is when the CRM's migration
// lands, and the index stub follows it.
type haMover struct {
	res  *attachRunRes
	stub *attachHAStub
	node string
	tick int64
	// pending is the node a migrate request sent the VM to.
	pending string
	// refuse makes PVE refuse every migrate request.
	refuse error
	// down marks the VM's node offline.
	down bool
	// driftTo moves the VM on the first read that finds the resource ignored,
	// the way an HA migration the CRM had queued before the barrier's ignored
	// request lands during the drain.
	driftTo string
}

func (m *haMover) place(node string) {
	m.node = node
	if m.stub != nil {
		m.stub.node = node
	}
}

func (m *haMover) CreateHaResourcesMigrate(_ context.Context, sid string, p *cluster.CreateHaResourcesMigrateParams) (
	*cluster.CreateHaResourcesMigrateResponse, error,
) {
	m.res.log.add("migrate:" + p.Node)
	if m.refuse != nil {
		return nil, m.refuse
	}
	m.pending = p.Node
	return &cluster.CreateHaResourcesMigrateResponse{Sid: sid, RequestedNode: p.Node}, nil
}

func (m *haMover) deps() ha.NarrowDeps {
	now := time.Unix(1000, 0)
	return ha.NarrowDeps{
		Resources: m.res,
		Migrate:   m,
		ReadState: func(context.Context, int, string) (*pve.HAServiceState, error) {
			m.tick += 10
			if m.pending != "" {
				m.place(m.pending)
				m.pending = ""
			}
			if m.driftTo != "" && m.res.state == "ignored" {
				m.place(m.driftTo)
				m.driftTo = ""
			}
			managed := !m.res.gone
			inCRM := managed && m.res.state != "ignored"
			st := &pve.HAServiceState{Known: true, Configured: managed, InCRM: inCRM, Requested: m.res.state,
				Node: m.node, EntryTime: m.tick, CRMStamp: m.tick - 1, LRMStamp: m.tick,
				NodeHealth: pve.NodeHealthEntry{Name: m.node, DeadOrFenced: m.down}}
			if inCRM {
				st.Raw = m.res.state
			}
			return st, nil
		},
		Settle: func(context.Context, int) (pve.VMLocation, error) {
			return pve.VMLocation{Node: m.node, Found: true}, nil
		},
		Now: func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(10 * time.Second)
			return nil
		},
		Logger: log.NewNopLogger(),
	}
}

// useMover makes decideAttachHA wire the barrier and the move to m.
func useMover(t *testing.T, m *haMover) {
	t.Helper()
	was := attachNarrowDeps
	attachNarrowDeps = func(pve.Client, *log.Logger, time.Duration) ha.NarrowDeps { return m.deps() }
	t.Cleanup(func() { attachNarrowDeps = was })
}

// driftRig is the attach of the disk on nfs-ab, which pve01 and pve02 reach,
// to VM 100 under a strict pin over all three nodes, with the VM placed on
// node and PVE HA played by the returned mover.
func driftRig(t *testing.T, mode, node string) (Deps, *attachHAStub, *haMover, *stagingLog) {
	t.Helper()
	deps, stub, _ := attachRig(mode)
	stub.pin(true, "pve01,pve02,pve03", "nfs")
	stub.node = node
	sl := &stagingLog{}
	m := &haMover{res: &attachRunRes{log: sl, state: "started", comment: stub.comment}, stub: stub, node: node}
	useMover(t, m)
	return deps, stub, m, sl
}

// runNarrowed runs the guarded attach of h, with a config that holds the new
// disk once it is attached.
func runNarrowed(t *testing.T, deps Deps, h *attachHA, sl *stagingLog) error {
	t.Helper()
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "scsi1": "nfs-ab:vm-100-disk-1"}, nil
	}}
	h.deps = deps
	return h.run(context.Background(), func(context.Context) (string, error) {
		sl.add("attach")
		return h.node, nil
	})
}

func TestAttachHARun_DriftDuringTheBarrierMovesTheVMBackUnderHA(t *testing.T) {
	// The VM is on pve01, inside the narrower set, when the decision runs. An
	// HA migration the CRM had queued takes it to pve03 during the drain. The
	// barrier puts the old state back, PVE HA moves the VM into the set, and
	// the barrier runs again, so the VM ends inside the set and under HA.
	deps, stub, m, sl := driftRig(t, config.HAModeAuto, "pve01")
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	m.driftTo = "pve03"
	if err := runNarrowed(t, deps, h, sl); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Failback goes off before the move and on again once the pin names
	// the set, so the CRM never pulls the VM back to the node it left.
	want := []string{"request:ignored", "request:started", "failback:false", "migrate:pve01", "request:ignored",
		"attach", "request:started", "failback:true"}
	if !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
	if m.node != "pve01" || m.res.state != "started" || m.res.gone || m.res.comment != stub.comment {
		t.Fatalf("node %s state %q gone %v comment %q, want pve01 started under HA with no token", m.node,
			m.res.state, m.res.gone, m.res.comment)
	}
	if pin := stub.rules["bosh-na-100"]; pin.Nodes == nil || *pin.Nodes != "pve01,pve02" {
		t.Fatalf("pin nodes %v, want the narrower set", pin.Nodes)
	}
}

func TestDecideAttachHA_RetryMovesAVMOutsideItsSetBackUnderHA(t *testing.T) {
	// The retry after the drift finds the VM on pve03, outside the set the new
	// disk allows. PVE HA moves it into the set before the decision, so the
	// VM keeps HA in either mode rather than leave it or fail.
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			deps, stub, m, sl := driftRig(t, mode, "pve03")
			h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
			if err != nil || h == nil {
				t.Fatalf("decision: h %+v err %v", h, err)
			}
			if h.action != attachHAGuarded || h.node != "pve01" {
				t.Fatalf("action %v node %s, want a guarded attach on pve01", h.action, h.node)
			}
			if err := runNarrowed(t, deps, h, sl); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := []string{"failback:false", "migrate:pve01", "request:ignored", "attach", "request:started",
				"failback:true"}
			if !slices.Equal(sl.events, want) {
				t.Fatalf("events %v, want %v", sl.events, want)
			}
			if m.res.state != "started" || m.res.gone {
				t.Fatalf("state %q gone %v, want the VM under HA", m.res.state, m.res.gone)
			}
			if pin := stub.rules["bosh-na-100"]; pin.Nodes == nil || *pin.Nodes != "pve01,pve02" {
				t.Fatalf("pin nodes %v, want the narrower set", pin.Nodes)
			}
		})
	}
}

// refusedMove is PVE's refusal of a migrate request.
var refusedMove = &sdkerrors.APIError{HTTPCode: 500, Message: "resource not allowed on target node"}

func TestDecideAttachHA_RefusedMoveKeepsHAUnderManaged(t *testing.T) {
	deps, _, m, sl := driftRig(t, config.HAModeManaged, "pve03")
	m.refuse = refusedMove
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
		t.Fatalf("h %+v err %v, want a retriable error", h, err)
	}
	for _, part := range []string{"runs on node pve03, outside its admissible set pve01,pve02",
		"managed never takes a VM out of HA", "keeps its HA resource, its tag, and its pin",
		"resource not allowed on target node", "ha-manager migrate vm:100 pve01", "retry the attach"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("err %q lacks %q", err, part)
		}
	}
	if !slices.Equal(sl.events, []string{"failback:false", "migrate:pve01"}) || m.res.state != "started" {
		t.Fatalf("events %v state %q", sl.events, m.res.state)
	}
}

func TestDecideAttachHA_RefusedMoveKeepsHAWhenTheNodeMissesTheDisk(t *testing.T) {
	// Under auto a refused move would take the VM out of HA, but pve03 does
	// not reach nfs-ab, so the attach would still fail there. The VM keeps HA
	// and the attach waits.
	deps, _, m, sl := driftRig(t, config.HAModeAuto, "pve03")
	m.refuse = refusedMove
	deps = withLiveStorage(deps, map[string]any{"storage": "nfs-ab", "type": "nfs", "shared": 1, "nodes": "pve01,pve02",
		"content": "images"})
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
		t.Fatalf("h %+v err %v, want a retriable error", h, err)
	}
	for _, part := range []string{"outside its admissible set pve01,pve02",
		"the node does not reach the new disk's storage", "ha-manager migrate vm:100 pve01"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("err %q lacks %q", err, part)
		}
	}
	if !slices.Equal(sl.events, []string{"failback:false", "migrate:pve01"}) || m.res.state != "started" {
		t.Fatalf("events %v state %q", sl.events, m.res.state)
	}
}

func TestDecideAttachHA_RefusedMoveLeavesHAUnderAuto(t *testing.T) {
	// The VM's root disk is on nfs-ab, so pve03 lies outside the admissible
	// set, and the new disk on nfs2 reaches every node. A refused move takes
	// the VM out of HA under auto, because the attach can then run on pve03.
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.pin(true, "pve01,pve02,pve03", "nfs-ab")
	stub.node = "pve03"
	sl := &stagingLog{}
	m := &haMover{res: &attachRunRes{log: sl, state: "started", comment: stub.comment}, stub: stub, node: "pve03",
		refuse: refusedMove}
	useMover(t, m)
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs-ab:vm-100-disk-0,size=10G", "cpu": "x86-64-v3"}, nil
	}}
	deps = withLiveStorage(deps, map[string]any{"storage": "nfs2", "type": "nfs", "shared": 1, "content": "images"})
	var logs bytes.Buffer
	logger, err := log.NewLogger("info", &logs)
	if err != nil {
		t.Fatal(err)
	}
	deps.Logger = logger
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil || h == nil {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	if h.action != attachHALeave || !slices.Equal(sl.events, []string{"failback:false", "migrate:pve01"}) {
		t.Fatalf("action %v events %v, want a leave after the refused move", h.action, sl.events)
	}
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "did not move the VM") &&
			strings.Contains(line, `"vmid":100`) && strings.Contains(line, `"set":"pve01,pve02"`) &&
			strings.Contains(line, "resource not allowed on target node") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names the VM, the set, and the reason:\n%s", logs.String())
	}
}

func TestDecideAttachHA_OfflineNodeDefersInEveryMode(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			deps, _, m, sl := driftRig(t, mode, "pve03")
			m.down = true
			h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
				t.Fatalf("h %+v err %v, want a retriable error", h, err)
			}
			for _, part := range []string{"node pve03 is offline", "outside its admissible set pve01,pve02",
				"keeps its HA resource"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("err %q lacks %q", err, part)
				}
			}
			if len(sl.events) != 0 || m.res.state != "started" {
				t.Fatalf("events %v state %q, want nothing sent", sl.events, m.res.state)
			}
		})
	}
}

// driftStorage serves the live storage index of a move's fit check.
type driftStorage struct {
	clusterstorage.Service
	rows []map[string]any
}

func (s *driftStorage) ListStorage(context.Context, *clusterstorage.ListStorageParams) (
	*clusterstorage.ListStorageResponse, error,
) {
	resp := clusterstorage.ListStorageResponse{}
	for _, row := range s.rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		resp = append(resp, raw)
	}
	return &resp, nil
}

// driftClient is the attach rig's client with a live storage index.
type driftClient struct {
	*icPVEClient
	storage *driftStorage
}

func (c *driftClient) ClusterStorage() clusterstorage.Service { return c.storage }

// withLiveStorage returns deps with a live storage index that lists rows.
func withLiveStorage(deps Deps, rows ...map[string]any) Deps {
	deps.PVE = &driftClient{icPVEClient: deps.PVE.(*icPVEClient), storage: &driftStorage{rows: rows}}
	return deps
}

// preferringPin gives VM 100 a strict pin over all three nodes that prefers
// pve03, so a VM with failback on that leaves pve03 is pulled back to it by
// the CRM until the pin names another node.
func preferringPin(stub *attachHAStub) {
	stub.pin(true, "pve03:2,pve01,pve02", "nfs")
}

// watchFailbackWrites records, for each write of the failback flag, the value,
// the node the VM was on, and the pin's node list at the moment it landed.
type failbackWrite struct {
	on   bool
	node string
	pin  string
}

func watchFailbackWrites(m *haMover, stub *attachHAStub) *[]failbackWrite {
	var writes []failbackWrite
	var hook func()
	hook = func() {
		pin := ""
		if p := stub.rules["bosh-na-100"]; p.Nodes != nil {
			pin = *p.Nodes
		}
		writes = append(writes, failbackWrite{on: ha.FailbackOf(m.res.failback), node: m.node, pin: pin})
		m.res.afterFailbackWrite = hook
	}
	m.res.afterFailbackWrite = hook
	return &writes
}

// eventIndex returns the index of the first event equal to want, or -1.
func eventIndex(events []string, want string) int {
	return slices.Index(events, want)
}

func TestDecideAttachHA_MoveIntoTheSetHoldsFailbackOffUntilThePinNamesTheNewNode(t *testing.T) {
	// The VM runs on pve03, which its old pin prefers, and the new disk allows
	// only pve01 and pve02. With failback on, the CRM would pull the VM back
	// to pve03 as soon as PVE HA moved it, so the attach turns failback off
	// before the move and turns it on again only once the pin names the set.
	deps, stub, m, sl := driftRig(t, config.HAModeAuto, "pve03")
	preferringPin(stub)
	writes := watchFailbackWrites(m, stub)
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	off, moved := eventIndex(sl.events, "failback:false"), eventIndex(sl.events, "migrate:pve01")
	if off < 0 || moved < 0 || off > moved {
		t.Fatalf("events %v, want failback turned off before the move", sl.events)
	}
	if err := runNarrowed(t, deps, h, sl); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, w := range *writes {
		if w.on && w.pin != "pve01,pve02" {
			t.Fatalf("failback turned on while the pin named %q, want only after it names the set", w.pin)
		}
	}
	if h.decision.Failback && !ha.FailbackOf(m.res.failback) {
		t.Fatalf("failback stayed off after the pin named the set; writes %+v", *writes)
	}
	if m.node != "pve01" {
		t.Fatalf("node %s, want pve01", m.node)
	}
}

func TestAttachHARun_MoveBackAfterADriftHoldsFailbackOffUntilThePinNamesTheSet(t *testing.T) {
	// A queued HA migration takes the VM to pve03, which the old pin prefers,
	// during the drain. The barrier has PVE HA move it back into the set,
	// which failback on would undo, so the attach turns failback off first.
	deps, stub, m, sl := driftRig(t, config.HAModeAuto, "pve01")
	preferringPin(stub)
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	if !h.decision.Failback {
		t.Fatal("a shared VM with a shared disk keeps failback, which this test needs")
	}
	writes := watchFailbackWrites(m, stub)
	m.driftTo = "pve03"
	if err := runNarrowed(t, deps, h, sl); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	off, moved := eventIndex(sl.events, "failback:false"), eventIndex(sl.events, "migrate:pve01")
	if off < 0 || moved < 0 || off > moved {
		t.Fatalf("events %v, want failback turned off before the move", sl.events)
	}
	for _, w := range *writes {
		if w.on && w.pin != "pve01,pve02" {
			t.Fatalf("failback turned on while the pin named %q, want only after it names the set", w.pin)
		}
	}
	if !ha.FailbackOf(m.res.failback) || m.node != "pve01" {
		t.Fatalf("failback %v node %s, want failback on again on pve01; writes %+v",
			ha.FailbackOf(m.res.failback), m.node, *writes)
	}
}

func TestDecideAttachHA_FailbackWriteFailureBeforeTheMoveKeepsHA(t *testing.T) {
	// A failback write that fails leaves the CRM free to pull the VM back, so
	// the move does not start, and the attach waits with the VM under HA.
	deps, stub, m, sl := driftRig(t, config.HAModeAuto, "pve03")
	preferringPin(stub)
	m.res.failbackErr = &sdkerrors.APIError{HTTPCode: 500, Message: "cfs lock timeout"}
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
		t.Fatalf("h %+v err %v, want a retriable error", h, err)
	}
	for _, part := range []string{"turn off failback", "cfs lock timeout", "keeps its HA resource"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("err %q lacks %q", err, part)
		}
	}
	if len(sl.events) != 0 || m.res.state != "started" || m.node != "pve03" {
		t.Fatalf("events %v state %q node %s, want nothing sent", sl.events, m.res.state, m.node)
	}
}
