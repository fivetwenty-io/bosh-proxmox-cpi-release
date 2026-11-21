package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// coloNow is the clock of the move tests. A member born a minute before it
// is joining for the first time. It is the wall clock at the start, because
// the spread claim record dates its claims by the wall clock.
var coloNow = time.Now().Truncate(time.Second)

var coloNodes = []string{"pve1", "pve2", "pve3", "pve4"}

// aaQEMUOn is aaQEMU for a guest the cluster lists on node.
func aaQEMUOn(vmid int, node, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": vmid, "node": node, "tags": tags})
	return raw
}

// markWebMembersBorn marks each VMID a member of the "web" group born at born.
func markWebMembersBorn(stub *aaClusterStub, born time.Time, vmids ...int) {
	for _, v := range vmids {
		stub.resources[haResourceSid(v)] = true
		stub.resComments[haResourceSid(v)] = aaIdent("web").resourceMarker("", born.Unix())
	}
}

// coloMigration is one migration a test's hooks were asked for.
type coloMigration struct {
	vmid     int
	from, to string
}

// coloHarness fakes the reads and the migration of the move.
type coloHarness struct {
	mu         sync.Mutex
	migrations []coloMigration
	// migrate answers a migration, or nil to succeed.
	migrate func(ctx context.Context, vmid int, from, to string) error
	// facts are the node facts the scorer reads, every node online and
	// alike by default.
	facts []placement.NodeFacts
	// config is the VM config, one disk on shared storage by default.
	config map[string]any
}

func newColoHarness() *coloHarness {
	h := &coloHarness{config: map[string]any{"scsi0": "shared-nfs:101/vm-101-disk-0.qcow2", "memory": "1024"}}
	for _, n := range coloNodes {
		h.facts = append(h.facts, placement.NodeFacts{
			Node: n, Online: true, FreeMemBytes: 64 << 30, TotalMemBytes: 128 << 30, MaxCPU: 16,
		})
	}
	return h
}

func (h *coloHarness) hooks() *aaColoMoveHooks {
	return &aaColoMoveHooks{
		now: func() time.Time { return coloNow },
		config: func(context.Context, string, int) (map[string]any, error) {
			return h.config, nil
		},
		reachable: func(_ context.Context, _ map[string]any, _, candidates []string) ([]string, error) {
			return candidates, nil
		},
		facts: func(context.Context, string) ([]placement.NodeFacts, error) {
			return slices.Clone(h.facts), nil
		},
		stamp: func(context.Context, string) string { return "stamp" },
		migrate: func(ctx context.Context, vmid int, from, to string, _ time.Duration) error {
			h.mu.Lock()
			h.migrations = append(h.migrations, coloMigration{vmid: vmid, from: from, to: to})
			h.mu.Unlock()
			if h.migrate != nil {
				return h.migrate(ctx, vmid, from, to)
			}
			return nil
		},
	}
}

func (h *coloHarness) moves() []coloMigration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.migrations)
}

// coloClaimDir returns a private directory for the spread claim record.
func coloClaimDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// coloDeps returns the deps of a join on four nodes that share the storage
// shared-nfs, with the move's hooks from h and its claims in dir.
func coloDeps(stub *aaClusterStub, h *coloHarness, dir string) Deps {
	deps := aaMetadataDeps(stub)
	deps.HAProfile = func(ctx context.Context) (*ha.Profile, error) {
		p, err := pve9ClusterProfile(coloNodes...)(ctx)
		if err != nil {
			return nil, err
		}
		p.Storages = []pve.StorageInfo{
			{Name: "shared-nfs", Type: "nfs", Shared: true},
			{Name: "local-lvm", Type: "lvmthin"},
		}
		return p, nil
	}
	deps.SpreadClaims = func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store {
		return &spreadclaim.Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "colo"}
	}
	deps.coloMove = h.hooks()
	return deps
}

// coloStub returns a stub whose cluster lists the four nodes online, so the
// guest scan finds each guest on the node it runs on.
func coloStub() *aaClusterStub {
	stub := newAAStub()
	stub.memberNodes = coloNodes
	stub.statusFn = func() (*cluster.ListStatusResponse, error) { return AbsenceOnlineStatus(coloNodes...), nil }
	return stub
}

// coloPair stages two members of the web group, 100 with index 0 and 101
// with index 1, on the given nodes, both born a minute before coloNow.
func coloPair(node100, node101 string) *aaClusterStub {
	stub := coloStub()
	stub.tagged(aaQEMUOn(100, node100, aaTag+";index--0"), aaQEMUOn(101, node101, aaTag+";index--1"))
	markWebMembersBorn(stub, coloNow.Add(-time.Minute), 100, 101)
	return stub
}

func coloJoin(deps Deps, vmid int, index string, logger *log.Logger) {
	joinAntiAffinityAtMetadata(context.Background(), deps, vmid, map[string]any{"index": index}, logger)
}

// coloLiveClaims returns the live claims of the web group in dir.
func coloLiveClaims(t *testing.T, dir string) []spreadclaim.Live {
	t.Helper()
	store := &spreadclaim.Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "colo"}
	claim, err := store.NewClaim(aaRule, coloNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var live []spreadclaim.Live
	if _, err := claim.PickLive(context.Background(), 1, func(l []spreadclaim.Live) string {
		live = l
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	return live
}

func coloWantRule(t *testing.T, stub *aaClusterStub) {
	t.Helper()
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Fatalf("rule = %q; want the join to write vm:100,vm:101", got)
	}
}

func coloWantNoMove(t *testing.T, h *coloHarness) {
	t.Helper()
	if got := h.moves(); len(got) != 0 {
		t.Fatalf("migrations = %+v; want none", got)
	}
}

// TestColoMove_MovesTheJoinerOffItsPartnersNode pins the move itself. Two
// members whose pairing is final share pve1, so the joiner moves to a node
// neither uses before the rule pairs them, its partner stays, and the claim
// of the move is gone once the join returns.
func TestColoMove_MovesTheJoinerOffItsPartnersNode(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	dir := coloClaimDir(t)
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	coloJoin(coloDeps(stub, h, dir), 101, "1", logger)
	got := h.moves()
	if len(got) != 1 || got[0].vmid != 101 || got[0].from != "pve1" || got[0].to == "pve1" {
		t.Fatalf("migrations = %+v; want one move of 101 off pve1", got)
	}
	coloWantRule(t, stub)
	if live := coloLiveClaims(t, dir); len(live) != 0 {
		t.Fatalf("live claims = %+v; want the move's claim released after the join", live)
	}
	if !observedHas(obs, slog.LevelInfo, "PVE HA moved the VM off its anti-affinity partner's node") {
		t.Fatal("want an Info line for the finished move")
	}
}

// TestColoMove_PicksTheBestScoredFreeNode pins that the target comes from the
// create_vm scorer, whose anti-affinity axis prefers the free node with no
// member of the group.
func TestColoMove_PicksTheBestScoredFreeNode(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	for i := range h.facts {
		if h.facts[i].Node != "pve3" {
			h.facts[i].SameGroupCount = 1
		}
	}
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", log.NewNopLogger())
	if got := h.moves(); len(got) != 1 || got[0].to != "pve3" {
		t.Fatalf("migrations = %+v; want 101 moved to pve3, the free node with no member of the group", got)
	}
}

// TestColoMove_NoFreeNodeLeavesTheVM pins that a joiner with no node to go to
// stays, and the join writes the rule as before.
func TestColoMove_NoFreeNodeLeavesTheVM(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	for i := range h.facts {
		if h.facts[i].Node != "pve1" {
			h.facts[i].InMaintenance = true
		}
	}
	dir := coloClaimDir(t)
	coloJoin(coloDeps(stub, h, dir), 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
	coloWantRule(t, stub)
	if live := coloLiveClaims(t, dir); len(live) != 0 {
		t.Fatalf("live claims = %+v; want no claim left behind", live)
	}
}

// TestColoMove_StaysInsideThePinsTopPriority pins that the target is one of
// the nodes the VM's pin prefers most, so failback never pulls it back.
func TestColoMove_StaysInsideThePinsTopPriority(t *testing.T) {
	t.Parallel()
	set := &ha.RuleSet{Rules: []ha.Rule{{
		ID: "pin", Type: ha.RuleTypeNodeAffinity, Resources: []string{"vm:101"},
		Nodes: map[string]int{"pve1": 2, "pve4": 2, "pve2": 1, "pve3": 1},
	}}}
	got := aaPinTopNodes(set, "vm:101")
	if len(got) != 2 || !got["pve1"] || !got["pve4"] {
		t.Fatalf("top nodes = %v; want pve1 and pve4", got)
	}
	if aaPinTopNodes(set, "vm:100") != nil {
		t.Fatal("a VM with no pin may go to any node")
	}
}

// TestColoMove_LocalDiskLeavesTheVMToPVE pins that a VM with a volume on
// storage that is not shared stays where it is.
func TestColoMove_LocalDiskLeavesTheVMToPVE(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	h.config = map[string]any{"scsi0": "shared-nfs:101/vm-101-disk-0.qcow2", "scsi1": "local-lvm:vm-101-disk-1"}
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", logger)
	coloWantNoMove(t, h)
	coloWantRule(t, stub)
	if !observedHas(obs, slog.LevelInfo, "local-lvm") {
		t.Fatal("want an Info line that names the local storage")
	}
}

// TestColoMove_AFailedMoveStillWritesTheRule pins that a refused move only
// warns, and the join writes the rule as it does without the move.
func TestColoMove_AFailedMoveStillWritesTheRule(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	h.migrate = func(context.Context, int, string, string) error {
		return &ha.MoveError{Failure: ha.MoveRefused, Err: errors.New("refused")}
	}
	dir := coloClaimDir(t)
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	coloJoin(coloDeps(stub, h, dir), 101, "1", logger)
	if len(h.moves()) != 1 {
		t.Fatal("want the move tried once")
	}
	coloWantRule(t, stub)
	if !observedHas(obs, slog.LevelWarn, "did not move the VM off its anti-affinity partner's node") {
		t.Fatal("want a Warn line for the failed move")
	}
	if live := coloLiveClaims(t, dir); len(live) != 0 {
		t.Fatalf("live claims = %+v; want the claim released", live)
	}
}

// TestColoMove_ATimedOutMoveStillWritesTheRule pins the wait. The move runs
// under a deadline no later than aaColoMoveWaitCap, and a move that runs past
// it warns that the partner may move too, and the rule is written.
func TestColoMove_ATimedOutMoveStillWritesTheRule(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	var deadline time.Time
	h.migrate = func(ctx context.Context, _ int, _, _ string) error {
		deadline, _ = ctx.Deadline()
		return &ha.MoveError{Failure: ha.MoveIncomplete, Err: errors.New("still migrating")}
	}
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	start := time.Now()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", logger)
	if deadline.IsZero() || deadline.After(start.Add(aaColoMoveWaitCap+time.Second)) {
		t.Fatalf("move deadline = %v; want one within %s of the join", deadline, aaColoMoveWaitCap)
	}
	coloWantRule(t, stub)
	if !observedHas(obs, slog.LevelWarn, "PVE may move the partner too") {
		t.Fatal("want a Warn line for the move that ran past its wait")
	}
}

// TestColoMove_AJoinerOffItsPartnersNodeStays pins that a VM already apart
// from its partners does not move.
func TestColoMove_AJoinerOffItsPartnersNodeStays(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve2")
	h := newColoHarness()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
	coloWantRule(t, stub)
}

// TestColoMove_ConcurrentSiblingsMoveOnce pins the claim between siblings.
// Two members share pve1 and join at the same moment. The first takes the
// claim turn and moves, and the second, which joins while the first is still
// migrating and still shows on pve1, sees the first's claim and stays, so the
// pair costs one migration.
func TestColoMove_ConcurrentSiblingsMoveOnce(t *testing.T) {
	t.Parallel()
	dir := coloClaimDir(t)
	stubA, stubB := coloPair("pve1", "pve1"), coloPair("pve1", "pve1")
	hA, hB := newColoHarness(), newColoHarness()
	depsB := coloDeps(stubB, hB, dir)
	hA.migrate = func(context.Context, int, string, string) error {
		coloJoin(depsB, 100, "0", log.NewNopLogger())
		return nil
	}
	coloJoin(coloDeps(stubA, hA, dir), 101, "1", log.NewNopLogger())
	if got := hA.moves(); len(got) != 1 || got[0].vmid != 101 {
		t.Fatalf("first sibling migrations = %+v; want 101 moved", got)
	}
	coloWantNoMove(t, hB)
	coloWantRule(t, stubA)
	coloWantRule(t, stubB)
}

// TestColoMove_PairingThatCanStillChangeStays pins that only a final pairing
// moves the joiner. With index 1 missing, the plan pairs 100 and 102, but the
// create of index 1 can still change that, so 102 stays.
func TestColoMove_PairingThatCanStillChangeStays(t *testing.T) {
	t.Parallel()
	stub := coloStub()
	stub.tagged(aaQEMUOn(100, "pve1", aaTag+";index--0"), aaQEMUOn(102, "pve1", aaTag+";index--2"))
	markWebMembersBorn(stub, coloNow.Add(-time.Minute), 100, 102)
	h := newColoHarness()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 102, "2", log.NewNopLogger())
	coloWantNoMove(t, h)
	if got := stub.rules[aaRule]; got != "vm:100,vm:102" {
		t.Fatalf("rule = %q; want vm:100,vm:102", got)
	}
}

// TestColoMove_AVMAlreadyInARuleStays pins that only the first join moves a
// VM. A later set_vm_metadata of a member its group's rule already holds
// leaves it to PVE.
func TestColoMove_AVMAlreadyInARuleStays(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	stub.seedRule(aaRule, "vm:100,vm:101")
	h := newColoHarness()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
}

// TestColoMove_AnOldMemberStays pins that a member born more than
// aaColoMoveMaxAge ago is a running workload, which the join does not move.
func TestColoMove_AnOldMemberStays(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve2", "pve2")
	markWebMembersBorn(stub, coloNow.Add(-2*aaColoMoveMaxAge), 100, 101)
	h := newColoHarness()
	coloJoin(coloDeps(stub, h, coloClaimDir(t)), 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
	coloWantRule(t, stub)
}

// TestColoMove_NoClaimRecordNoMove pins that a join with no spread claim
// record leaves the VM to PVE, because a sibling could not see the move.
func TestColoMove_NoClaimRecordNoMove(t *testing.T) {
	t.Parallel()
	stub := coloPair("pve1", "pve1")
	h := newColoHarness()
	deps := coloDeps(stub, h, coloClaimDir(t))
	deps.SpreadClaims = func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return nil }
	coloJoin(deps, 101, "1", log.NewNopLogger())
	coloWantNoMove(t, h)
	coloWantRule(t, stub)
}

// TestColoClaimCounts_MovesAClaimedVMsCount pins that a live claim counts its
// VM on the claimed node in place of the node its tag shows it on.
func TestColoClaimCounts_MovesAClaimedVMsCount(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{
		{Node: "pve1", SameGroupCount: 2, SameGroupVMIDs: []int{100, 101}},
		{Node: "pve2"},
		{Node: "pve3"},
	}
	out := aaColoClaimCounts(facts, []spreadclaim.Live{{Node: "pve2", VMID: 100}, {Node: "pve3"}})
	want := map[string]int{"pve1": 1, "pve2": 1, "pve3": 1}
	for _, f := range out {
		if f.SameGroupCount != want[f.Node] {
			t.Fatalf("counts = %+v; want %v", out, want)
		}
	}
	if facts[0].SameGroupCount != 2 {
		t.Fatal("the facts passed in must not change")
	}
}

// observedHas reports whether obs holds an entry at level whose message
// contains part.
func observedHas(obs *log.Observer, level slog.Level, part string) bool {
	for _, e := range obs.All() {
		if e.Level == level && strings.Contains(e.Message, part) {
			return true
		}
	}
	return false
}
