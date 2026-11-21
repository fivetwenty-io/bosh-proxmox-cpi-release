package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaPlacementCluster serves the placement reads that the anti-affinity stub
// leaves out, so one stub answers both the rule writers and the scorer.
type aaPlacementCluster struct {
	*aaClusterStub
}

func (aaPlacementCluster) ListHaStatusCurrent(context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	empty := cluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

func (aaPlacementCluster) ListHaStatusManagerStatus(
	context.Context,
) (*cluster.ListHaStatusManagerStatusResponse, error) {
	empty := cluster.ListHaStatusManagerStatusResponse{}
	return &empty, nil
}

// aaPartnerNodes names the three nodes of the partner tests.
var aaPartnerNodes = []string{"pve1", "pve2", "pve3"}

// aaPartnerFleet lays out the lab group, five members of "web" on three
// nodes, with indexes 0 and 1 in the first rule and 2 and 3 in the second,
// and index 4 alone. Index 0 runs on pve1, 1 and 2 on pve2, and 3 and 4 on
// pve3, so each rule's members run apart. pve1 has the most free memory and
// pve3 the least, which breaks a tie in the group count toward pve1. Nodes
// named in maintenance carry the maintenance tag.
func aaPartnerFleet(t *testing.T, maintenance ...string) (Deps, *aaClusterStub, *aaCanonFleet) {
	t.Helper()
	stub := newAAStub()
	stub.purgeFromRules = true
	stub.memberNodes = aaPartnerNodes
	free := map[string]int64{"pve1": 6, "pve2": 4, "pve3": 2}
	stub.statusFn = func() (*cluster.ListStatusResponse, error) {
		raw, _ := json.Marshal(map[string]any{"type": "cluster", "name": "lab", "quorate": 1, "nodes": 3})
		resp := make(cluster.ListStatusResponse, 0, 1+len(aaPartnerNodes))
		resp = append(resp, raw)
		for _, n := range aaPartnerNodes {
			row := onlineNode(n)
			if slices.Contains(maintenance, n) {
				row["tags"] = "maintenance"
			}
			raw, _ := json.Marshal(row)
			resp = append(resp, raw)
		}
		return &resp, nil
	}
	fleet := &aaCanonFleet{stub: stub, index: map[int]int{}, node: map[int]string{}}
	const gib = int64(1) << 30
	for _, n := range aaPartnerNodes {
		raw, _ := json.Marshal(map[string]any{"type": "node", "node": n, "maxmem": 8 * gib,
			"mem": (8 - free[n]) * gib, "maxcpu": 4, "cpu": 0.1})
		fleet.nodeRows = append(fleet.nodeRows, raw)
	}
	for v, n := range map[int]string{100: "pve1", 101: "pve2", 102: "pve2", 103: "pve3", 104: "pve3"} {
		fleet.index[v], fleet.node[v] = v%100, n
	}
	markWebMembers(stub, 100, 101, 102, 103, 104)
	fleet.list()
	deps := aaReplanDepsWithLocks(stub, true, newAALockPools(nil))
	deps.HAProfile = pve9ClusterProfile(aaPartnerNodes...)
	deps.Config.Node, deps.Config.VMStorage = "", ""
	client := *deps.PVE.(*icPVEClient)
	client.clusterSvc = aaPlacementCluster{stub}
	deps.PVE = &client
	if _, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{}); err != nil {
		t.Fatalf("lay the group out: %v", err)
	}
	want := map[string]string{aaRule: "vm:100,vm:101", aaRule + "-2": "vm:102,vm:103"}
	if !maps.Equal(stub.rules, want) {
		t.Fatalf("start rules = %v; want %v", stub.rules, want)
	}
	return deps, stub, fleet
}

// aaDeleteMember deletes the member with VMID vmid the way delete_vm does,
// which takes it out of its rules and records a hold for its index.
func aaDeleteMember(t *testing.T, deps Deps, fleet *aaCanonFleet, vmid int) {
	t.Helper()
	fleet.destroy(vmid)
	if err := removeAntiAffinityMembership(context.Background(), deps, vmid, webTags(vmid),
		log.NewNopLogger()); err != nil {
		t.Fatalf("delete vm:%d: %v", vmid, err)
	}
}

// aaPlaceRecreate scores the nodes for a create of the web group the way
// create_vm does, reading the group's partners first unless plain is set,
// and returns the node and the partners it read.
func aaPlaceRecreate(t *testing.T, deps Deps, plain bool) (string, map[string]int) {
	t.Helper()
	ctx := context.Background()
	var cp createVMCloudProps
	group := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	if !plain {
		group.partners = antiAffinityPartnerNodes(ctx, deps, aaEnvForWebGroup(), cp)
	}
	node, _, err := resolveTargetNodeWithBridges(ctx, deps, cp, group, nil, nil, nil, 0, bridgePlan{})
	if err != nil {
		t.Fatalf("place the recreate: %v", err)
	}
	return node, group.partners
}

// TestAntiAffinityPartnerNodes_ARecreateLandsAwayFromItsPartners pins that a
// create during a roll at a max_in_flight of 1, 2, and 3 counts the nodes of
// the present rule partners of every index whose hold is live, and lands on
// a node that runs none of them. Where the group count and free memory alone
// would pick a partner's node, the case says so, which shows the penalty is
// what moves the create.
func TestAntiAffinityPartnerNodes_ARecreateLandsAwayFromItsPartners(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		deleted  []int
		partners map[string]int
		// plainPick is the node the scorer picks without the partners.
		plainPick string
	}{
		{name: "max_in_flight 1", deleted: []int{101}, partners: map[string]int{"pve1": 1}, plainPick: "pve1"},
		{name: "max_in_flight 2", deleted: []int{101, 103}, partners: map[string]int{"pve1": 1, "pve2": 1},
			plainPick: "pve1"},
		{name: "max_in_flight 3", deleted: []int{100, 102, 104}, partners: map[string]int{"pve2": 1, "pve3": 1},
			plainPick: "pve1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, _, fleet := aaPartnerFleet(t)
			for _, v := range tc.deleted {
				aaDeleteMember(t, deps, fleet, v)
			}
			node, partners := aaPlaceRecreate(t, deps, false)
			if !maps.Equal(partners, tc.partners) {
				t.Fatalf("partner nodes = %v; want %v", partners, tc.partners)
			}
			if partners[node] > 0 {
				t.Fatalf("the recreate landed on %s, which runs a partner (partners %v)", node, partners)
			}
			if plain, _ := aaPlaceRecreate(t, deps, true); plain != tc.plainPick {
				t.Fatalf("without the partners the scorer picks %s; want %s", plain, tc.plainPick)
			}
		})
	}
}

// TestAntiAffinityPartnerNodes_NoLiveHoldKeepsTodaysPlacement pins that a
// create whose index cannot be inferred, because no hold of the group is
// live, reads no partners and places the way it did before: a scale-out, a
// recreate whose hold its own set_vm_metadata already consumed, and a group
// with no rule at all.
func TestAntiAffinityPartnerNodes_NoLiveHoldKeepsTodaysPlacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps, stub, fleet := aaPartnerFleet(t)
	rules, resources := stub.listRulesCalls, stub.listHaResCalls
	if got := antiAffinityPartnerNodes(ctx, deps, aaEnvForWebGroup(), createVMCloudProps{}); got != nil {
		t.Errorf("scale-out: partner nodes = %v; want none", got)
	}
	// With no hold recorded anywhere, the read stops after the rules and
	// the HA resources, so a create that is not a recreate pays two reads.
	if n, m := stub.listRulesCalls-rules, stub.listHaResCalls-resources; n != 1 || m != 1 {
		t.Errorf("scale-out: %d rule reads and %d resource reads; want one of each", n, m)
	}

	aaDeleteMember(t, deps, fleet, 101)
	fleet.node[201] = "pve2"
	fleet.create(201)
	fleet.tag(201)
	joinAntiAffinityAtMetadata(ctx, deps, 201, map[string]any{"index": float64(1)}, log.NewNopLogger())
	if got := antiAffinityPartnerNodes(ctx, deps, aaEnvForWebGroup(), createVMCloudProps{}); got != nil {
		t.Errorf("consumed hold: partner nodes = %v; want none (rules %v)", got, stub.rules)
	}

	empty := newAAStub()
	plain := aaReplanDepsWithLocks(empty, true, newAALockPools(nil))
	if got := antiAffinityPartnerNodes(ctx, plain, aaEnvForWebGroup(), createVMCloudProps{}); got != nil {
		t.Errorf("no rules: partner nodes = %v; want none", got)
	}
}

// TestAntiAffinityPartnerNodes_ALapsedHoldInfersNoIndex pins that a hold
// past its expiry infers no index, so the create places the way it did
// before, while the same hold within its expiry names its partners.
func TestAntiAffinityPartnerNodes_ALapsedHoldInfersNoIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		until time.Duration
		want  map[string]int
	}{
		{name: "lapsed", until: -time.Minute, want: nil},
		{name: "live", until: time.Minute, want: map[string]int{"pve1": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, stub, fleet := aaPartnerFleet(t)
			aaDeleteMember(t, deps, fleet, 101)
			hold := aaHold{Index: 1, VMID: 101, Until: time.Now().Add(tc.until).Unix()}
			recorded := false
			for name, comment := range stub.ruleComments {
				if len(holdsOf(comment)) > 0 {
					stub.ruleComments[name] = withHolds(comment, []aaHold{hold})
					recorded = true
				}
			}
			for sid, comment := range stub.resComments {
				if len(holdsOf(comment)) > 0 {
					stub.resComments[sid] = withHolds(comment, []aaHold{hold})
					recorded = true
				}
			}
			if !recorded {
				t.Fatalf("the delete recorded no hold (rules %v)", stub.ruleComments)
			}
			got := antiAffinityPartnerNodes(ctx, deps, aaEnvForWebGroup(), createVMCloudProps{})
			if !maps.Equal(got, tc.want) {
				t.Fatalf("partner nodes = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestAntiAffinityPartnerNodes_AFailedReadKeepsTodaysPlacement pins that a
// create whose read of the rules or the HA resources fails reads no
// partners, logs one Warn line, and still places.
func TestAntiAffinityPartnerNodes_AFailedReadKeepsTodaysPlacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		breakRead func(*aaClusterStub)
	}{
		{name: "rules", breakRead: func(s *aaClusterStub) { s.failListRules = true }},
		{name: "resources", breakRead: func(s *aaClusterStub) { s.listHaResErr = errors.New("resources unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, stub, fleet := aaPartnerFleet(t)
			aaDeleteMember(t, deps, fleet, 101)
			tc.breakRead(stub)
			logger, obs := log.NewObservedLogger(slog.LevelDebug)
			deps.Logger = logger
			got := antiAffinityPartnerNodes(context.Background(), deps, aaEnvForWebGroup(), createVMCloudProps{})
			if got != nil {
				t.Fatalf("partner nodes = %v; want none after a failed read", got)
			}
			warns := 0
			for _, e := range obs.All() {
				if e.Level == slog.LevelWarn && strings.Contains(e.Message, "rule partners") {
					warns++
				}
			}
			if warns != 1 {
				t.Fatalf("Warn lines about the partners = %d; want 1 (%v)", warns, obs.All())
			}
			if node, _ := aaPlaceRecreate(t, deps, false); node == "" {
				t.Fatal("the create placed nowhere")
			}
		})
	}
}

// TestAntiAffinityPartnerNodes_TargetNodeWins pins that a create that names
// target_node reads no partners, sends no read for them, and lands on its
// target even when the target runs a partner, the way target_node already
// overrides the group count.
func TestAntiAffinityPartnerNodes_TargetNodeWins(t *testing.T) {
	t.Parallel()
	deps, stub, fleet := aaPartnerFleet(t)
	aaDeleteMember(t, deps, fleet, 101)
	cp := createVMCloudProps{TargetNode: "pve1"}
	reads := stub.listRulesCalls
	if got := antiAffinityPartnerNodes(context.Background(), deps, aaEnvForWebGroup(), cp); got != nil {
		t.Fatalf("partner nodes = %v; want none with target_node", got)
	}
	if stub.listRulesCalls != reads {
		t.Fatalf("the partner read sent %d rule reads with target_node; want none", stub.listRulesCalls-reads)
	}
	group := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	group.partners = map[string]int{"pve1": 1}
	node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, group, nil, nil, nil, 0, bridgePlan{})
	if err != nil || node != "pve1" {
		t.Fatalf("node = %q, %v; want the target pve1", node, err)
	}
}

// TestAntiAffinityPartnerNodes_MaintenanceComesFirst pins that the partner
// penalty never brings back a node in maintenance and never fails a create.
// With pve2 in maintenance, the recreate of index 1 goes to pve3, which runs
// two members of the group, instead of pve1, which runs its partner. With
// pve2 and pve3 in maintenance it lands on pve1 next to its partner, because
// the penalty is soft.
func TestAntiAffinityPartnerNodes_MaintenanceComesFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		maintenance []string
		want        string
	}{
		{maintenance: []string{"pve2"}, want: "pve3"},
		{maintenance: []string{"pve2", "pve3"}, want: "pve1"},
	} {
		t.Run(strings.Join(tc.maintenance, "+"), func(t *testing.T) {
			t.Parallel()
			deps, _, fleet := aaPartnerFleet(t, tc.maintenance...)
			aaDeleteMember(t, deps, fleet, 101)
			node, partners := aaPlaceRecreate(t, deps, false)
			if !maps.Equal(partners, map[string]int{"pve1": 1}) {
				t.Fatalf("partner nodes = %v; want pve1", partners)
			}
			if node != tc.want {
				t.Fatalf("node = %s; want %s", node, tc.want)
			}
		})
	}
}

// TestAntiAffinityPartnerNodes_TheLabRollKeepsEveryPairApart replays the lab
// roll of five members on three nodes, the batches index 0, then 1 and 4,
// then 2 and 3. In each batch the deletes run first, then each create, and
// then each set_vm_metadata, which is when the rules take the recreate back.
// Every create lands on a node that runs none of its index's present
// partners, so once the joins land no rule holds two members on one node,
// and PVE has nothing to move. The same roll without the partners puts the
// recreate of index 1 next to index 0, which is the collision the penalty
// prevents.
func TestAntiAffinityPartnerNodes_TheLabRollKeepsEveryPairApart(t *testing.T) {
	t.Parallel()
	if got := aaReplayLabRoll(t, false); len(got) != 0 {
		t.Fatalf("rules with two members on one node: %v", got)
	}
	if got := aaReplayLabRoll(t, true); !slices.Contains(got, "index 1 with index 0 on pve1") {
		t.Fatalf("without the partners the roll should put index 1 next to index 0; collisions %v", got)
	}
}

// aaReplayLabRoll runs the roll of TestAntiAffinityPartnerNodes_TheLabRollKeepsEveryPairApart
// and returns each pair of a rule that shared a node after a batch's joins.
// With plain set the creates place without the partners.
func aaReplayLabRoll(t *testing.T, plain bool) []string {
	t.Helper()
	ctx := context.Background()
	deps, stub, fleet := aaPartnerFleet(t)
	var collisions []string
	for _, batch := range [][]int{{0}, {1, 4}, {2, 3}} {
		for _, i := range batch {
			aaDeleteMember(t, deps, fleet, 100+i)
		}
		for _, i := range batch {
			node, partners := aaPlaceRecreate(t, deps, plain)
			if !plain && partners[node] > 0 {
				t.Errorf("batch %v: the recreate of index %d landed on %s, which runs a partner (%v)", batch, i,
					node, partners)
			}
			v := 200 + i
			fleet.node[v] = node
			fleet.create(v)
		}
		for _, i := range batch {
			v := 200 + i
			fleet.tag(v)
			joinAntiAffinityAtMetadata(ctx, deps, v, map[string]any{"index": float64(i)}, log.NewNopLogger())
		}
		for _, csv := range stub.rules {
			sids := strings.Split(csv, ",")
			for x, a := range sids {
				for _, o := range sids[x+1:] {
					av, _ := strconv.Atoi(strings.TrimPrefix(a, "vm:"))
					ov, _ := strconv.Atoi(strings.TrimPrefix(o, "vm:"))
					if fleet.node[av] == fleet.node[ov] {
						first, second := fleet.index[av], fleet.index[ov]
						if slices.Contains(batch, first) {
							first, second = second, first
						}
						collisions = append(collisions, "index "+strconv.Itoa(second)+" with index "+
							strconv.Itoa(first)+" on "+fleet.node[av])
					}
				}
			}
		}
	}
	return collisions
}

// TestResolveVMShape_ScoresThePartnersTheCreateRead pins that the scored
// placement of a plain create counts the partner nodes create_vm read. For
// each node of three, partners on the other two put the VM on that node.
func TestResolveVMShape_ScoresThePartnersTheCreateRead(t *testing.T) {
	t.Parallel()
	for _, free := range aaPartnerNodes {
		deps := spreadDeps(nil)
		parsed := minimalParsedArgs("local-lvm")
		parsed.cloudProps.TargetNode = ""
		parsed.env = aaEnvForWebGroup()
		parsed.aaPartners = map[string]int{}
		for _, n := range aaPartnerNodes {
			if n != free {
				parsed.aaPartners[n] = 1
			}
		}
		shape, err := resolveVMShape(context.Background(), deps, parsed)
		if err != nil {
			t.Fatalf("partners %v: %v", parsed.aaPartners, err)
		}
		if shape.node != free {
			t.Errorf("partners %v: node = %s; want %s", parsed.aaPartners, shape.node, free)
		}
	}
}

// TestObserveStoragePlanCompute_RanksPartnerNodesLast pins that the compute
// ranking of a managed create counts the partner nodes create_vm read, so the
// node that runs a partner ranks after the one that runs none.
func TestObserveStoragePlanCompute_RanksPartnerNodesLast(t *testing.T) {
	t.Parallel()
	on := true
	for _, tc := range []struct{ partner, first string }{{"n1", "n2"}, {"n2", "n1"}} {
		deps := buildPlacementDeps([]map[string]any{onlineNode("n1"), onlineNode("n2")}, func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}}
		})
		deps.RequestDirectorUUID = aaDirector
		parsed := spreadParsed()
		parsed.aaPartners = map[string]int{tc.partner: 1}
		groups, err := ObserveStoragePlanCompute(context.Background(), deps, parsed, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || len(groups[0].Nodes) != 2 || groups[0].Nodes[0] != tc.first {
			t.Errorf("partner on %s: groups = %+v; want %s ranked first", tc.partner, groups, tc.first)
		}
	}
}
