package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// aaCanonFleet is the "web" group's guests as the stub lists them. A guest
// with index -1 shows no index tag yet, the way a new VM does between its
// create and its set_vm_metadata.
type aaCanonFleet struct {
	stub  *aaClusterStub
	index map[int]int
	// node, when set, places each VMID's guest on a node, and nodeRows lists
	// the nodes' own rows with the guests, so placement reads their counters.
	node     map[int]string
	nodeRows []json.RawMessage
}

// newAACanonFleet lists each VMID as a member whose index is the VMID's last
// two digits, created long enough ago to join a rule without an index.
func newAACanonFleet(stub *aaClusterStub, vmids ...int) *aaCanonFleet {
	f := &aaCanonFleet{stub: stub, index: map[int]int{}}
	for _, v := range vmids {
		f.index[v] = v % 100
	}
	markWebMembers(stub, vmids...)
	f.list()
	return f
}

func (f *aaCanonFleet) list() {
	rows := make([]json.RawMessage, 0, len(f.index))
	for _, v := range slices.Sorted(maps.Keys(f.index)) {
		tags := aaTag
		if i := f.index[v]; i >= 0 {
			tags += ";index--" + strconv.Itoa(i)
		}
		if n := f.node[v]; n != "" {
			raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": v, "tags": tags, "node": n})
			rows = append(rows, raw)
			continue
		}
		rows = append(rows, aaQEMU(v, tags))
	}
	rows = append(rows, f.nodeRows...)
	f.stub.listResourcesFn = aaResourcesFn(rows...)
}

// create lists a new VM with no index tag and stages its HA resource, marked
// as created now, the way create_vm leaves it.
func (f *aaCanonFleet) create(vmid int) {
	f.index[vmid] = -1
	f.stub.resources[haResourceSid(vmid)] = true
	f.stub.resComments[haResourceSid(vmid)] = aaIdent("web").resourceMarker("", time.Now().Unix())
	f.list()
}

// tag shows the VM's index tag, the way set_vm_metadata writes it.
func (f *aaCanonFleet) tag(vmid int) {
	f.index[vmid] = vmid % 100
	f.list()
}

func (f *aaCanonFleet) destroy(vmid int) {
	delete(f.index, vmid)
	f.list()
}

// aaRuleWrites counts the rule writes the stub has taken.
func aaRuleWrites(stub *aaClusterStub) int {
	return stub.createRuleCalls + stub.updateRuleCalls + stub.deleteRuleCalls
}

// aaMoveLogs counts the Info entries that say a plan moved members of the
// group to other rules.
func aaMoveLogs(obs *log.Observer) int {
	n := 0
	for _, e := range obs.All() {
		if e.Level == slog.LevelInfo && strings.Contains(e.Message, "so that the rules follow the members' indexes") {
			n++
		}
	}
	return n
}

// TestJoinAntiAffinityAtMetadata_TheLabJoinOrderGivesTheCanonicalLayout
// replays the parallel deploy of five members on three nodes in the order the
// lab recorded it. The five VMs are created and their HA resources staged
// first. Their set_vm_metadata calls then write the index tags in the order
// index 1, 4, 3, 0, and 2, and their plans run in the order index 4, 1, 3, 0,
// and 2, so the first two plans each see indexes 1 and 4. The deploy ends
// with indexes 0 and 1 in the first rule and 2 and 3 in the second, with
// index 4 alone, and after every plan the doctor's plan writes nothing. A
// plan that both changes the first rule and creates the second sends two
// writes, and between them the doctor would send the second, so the check
// runs once each plan has sent all of its writes.
func TestJoinAntiAffinityAtMetadata_TheLabJoinOrderGivesTheCanonicalLayout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stub := newAAStub()
	fleet := &aaCanonFleet{stub: stub, index: map[int]int{}}
	for v := 100; v <= 104; v++ {
		fleet.create(v)
	}
	deps := aaReplanDepsWithLocks(stub, true, newAALockPools(nil))
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	join := func(vmid int) {
		joinAntiAffinityAtMetadata(ctx, deps, vmid, map[string]any{"index": float64(vmid % 100)}, log.NewNopLogger())
		if d := aaDoctorDrift(t, deps); len(d) != 0 {
			t.Errorf("after the plan of vm:%d the doctor would write %v; rules %v", vmid, d, stub.rules)
		}
	}
	fleet.tag(101)
	fleet.tag(104)
	join(104)
	join(101)
	for _, v := range []int{103, 100, 102} {
		fleet.tag(v)
		join(v)
	}
	want := map[string]string{aaRule: "vm:100,vm:101", aaRule + "-2": "vm:102,vm:103"}
	if !maps.Equal(stub.rules, want) {
		t.Fatalf("rules = %v; want %v", stub.rules, want)
	}
	if d := aaDoctorDrift(t, deps); len(d) != 0 {
		t.Fatalf("the doctor would write %v after the deploy", d)
	}
}

// TestAntiAffinity_AStickyLayoutMovesToTheCanonicalOneOnce pins that a group
// whose rules an earlier release laid out by the order its members joined in
// is rewritten into the canonical layout once, by its next set_vm_metadata,
// by its next delete_vm, or by pve-ops ha --fix-anti-affinity. On four nodes
// a rule holds three members. The rules hold indexes 0, 1, and 2, and 4, 5,
// and 6, with index 3 in no rule, where the canonical layout puts 3, 4, and 5
// in the second rule and leaves 6 alone. The rewrite of the second rule is
// one write, it logs once at Info, and the next plan writes nothing.
func TestAntiAffinity_AStickyLayoutMovesToTheCanonicalOneOnce(t *testing.T) {
	t.Parallel()
	canonical := "vm:103,vm:104,vm:105"
	for name, tc := range map[string]struct {
		run      func(t *testing.T, deps Deps, fleet *aaCanonFleet, logger *log.Logger)
		again    func(t *testing.T, deps Deps, fleet *aaCanonFleet)
		wantBase string
	}{
		"set_vm_metadata": {
			run: func(_ *testing.T, deps Deps, _ *aaCanonFleet, logger *log.Logger) {
				joinAntiAffinityAtMetadata(context.Background(), deps, 103, map[string]any{"index": 3.0}, logger)
			},
			again: func(_ *testing.T, deps Deps, _ *aaCanonFleet) {
				joinAntiAffinityAtMetadata(context.Background(), deps, 103, map[string]any{"index": 3.0},
					log.NewNopLogger())
			},
			wantBase: "vm:100,vm:101,vm:102",
		},
		"delete_vm": {
			run: func(t *testing.T, deps Deps, fleet *aaCanonFleet, logger *log.Logger) {
				fleet.destroy(100)
				if err := removeAntiAffinityMembership(context.Background(), deps, 100, webTags(100),
					logger); err != nil {
					t.Fatalf("remove 100: %v", err)
				}
			},
			again: func(_ *testing.T, deps Deps, fleet *aaCanonFleet) {
				fleet.create(107)
				fleet.index[107] = 0
				fleet.list()
				joinAntiAffinityAtMetadata(context.Background(), deps, 107, map[string]any{"index": 0.0},
					log.NewNopLogger())
			},
			wantBase: "vm:101,vm:102",
		},
		"fix-anti-affinity": {
			run: func(t *testing.T, deps Deps, _ *aaCanonFleet, logger *log.Logger) {
				deps.Logger = logger
				got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
				if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityFixed {
					t.Fatalf("fix = %+v, %v; want the group fixed", got, err)
				}
			},
			again: func(t *testing.T, deps Deps, _ *aaCanonFleet) {
				got, err := FixAntiAffinityGroups(context.Background(), deps, AntiAffinityFixOptions{})
				if err != nil || len(got) != 1 || got[0].Outcome != AntiAffinityInLayout {
					t.Fatalf("second fix = %+v, %v; want the group in its layout", got, err)
				}
			},
			wantBase: "vm:100,vm:101,vm:102",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.purgeFromRules = true
			fleet := newAACanonFleet(stub, 100, 101, 102, 103, 104, 105, 106)
			stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
			stub.seedRule(aaRule+"-2", "vm:104,vm:105,vm:106")
			deps := aaReplanDepsWithLocks(stub, true, newAALockPools(nil))
			second := 0
			stub.onRuleWrite = func(rule string) {
				if rule == aaRule+"-2" {
					second++
				}
			}
			logger, obs := log.NewObservedLogger(slog.LevelDebug)
			tc.run(t, deps, fleet, logger)
			if got := stub.rules[aaRule+"-2"]; got != canonical || second != 1 {
				t.Fatalf("second rule = %q after %d writes; want %q in one write (rules %v)", got, second,
					canonical, stub.rules)
			}
			if got := stub.rules[aaRule]; got != tc.wantBase {
				t.Fatalf("first rule = %q; want %q", got, tc.wantBase)
			}
			if n := aaMoveLogs(obs); n != 1 {
				t.Fatalf("the rewrite logged %d times at Info; want once (%+v)", n, obs.All())
			}
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("the doctor would write %v after the rewrite", d)
			}
			before := aaRuleWrites(stub)
			second = 0
			tc.again(t, deps, fleet)
			if second != 0 || stub.rules[aaRule+"-2"] != canonical {
				t.Fatalf("the next plan wrote the second rule %d times, to %q; want it left alone", second,
					stub.rules[aaRule+"-2"])
			}
			if name != "delete_vm" && aaRuleWrites(stub) != before {
				t.Fatalf("the next plan wrote %d rules; want none", aaRuleWrites(stub)-before)
			}
			if d := aaDoctorDrift(t, deps); len(d) != 0 {
				t.Fatalf("the doctor would write %v after the next plan", d)
			}
		})
	}
}

// TestAntiAffinity_RollsKeepThePlannerAndTheDoctorInAgreement pins that a
// rolling update at a max_in_flight of 1 to 3 moves no member but the ones it
// recreates, and that after every write the doctor's plan of the state writes
// nothing. Within a batch the deletes, the creates, and the set_vm_metadata
// calls of its members interleave in a random order in which each member's
// delete comes before its create and its create before its set_vm_metadata.
// No member is ever in a rule its index was not in before the roll, and after
// the roll every index is back in the rules it started in.
func TestAntiAffinity_RollsKeepThePlannerAndTheDoctorInAgreement(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(5))
	for iter := range 40 {
		nodes := 3 + r.Intn(2)
		size := 4 + r.Intn(8)
		mif := 1 + r.Intn(3)
		order := r.Perm(size)
		var batches [][]int
		for b := 0; b < size; b += mif {
			batches = append(batches, order[b:min(b+mif, size)])
		}
		events := make([][]string, 0, len(batches))
		for _, batch := range batches {
			events = append(events, aaRollEvents(r, batch))
		}
		name := fmt.Sprintf("case %d, %d members on %d nodes, max_in_flight %d", iter, size, nodes, mif)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testRollAgreement(t, nodes, size, batches, events)
		})
	}
}

// aaRollEvents returns the events of one batch in a random order in which
// each member's delete comes before its create and its create before its
// set_vm_metadata. Each event is the kind and the index, such as "delete 3".
func aaRollEvents(r *rand.Rand, batch []int) []string {
	next := map[int]int{}
	var out []string
	for len(out) < 3*len(batch) {
		i := batch[r.Intn(len(batch))]
		if next[i] == 3 {
			continue
		}
		out = append(out, []string{"delete", "create", "join"}[next[i]]+" "+strconv.Itoa(i))
		next[i]++
	}
	return out
}

// testRollAgreement runs one case of
// TestAntiAffinity_RollsKeepThePlannerAndTheDoctorInAgreement.
func testRollAgreement(t *testing.T, nodes, size int, batches [][]int, events [][]string) {
	t.Helper()
	ctx := context.Background()
	stub := newAAStub()
	stub.purgeFromRules = true
	vmids := make([]int, 0, size)
	for i := range size {
		vmids = append(vmids, 100+i)
	}
	fleet := newAACanonFleet(stub, vmids...)
	deps := aaReplanDepsWithLocks(stub, true, newAALockPools(nil))
	names := make([]string, 0, nodes)
	for n := range nodes {
		names = append(names, "pve"+strconv.Itoa(n+1))
	}
	deps.HAProfile = pve9ClusterProfile(names...)
	if _, err := FixAntiAffinityGroups(ctx, deps, AntiAffinityFixOptions{}); err != nil {
		t.Fatalf("lay the group out: %v", err)
	}
	start := aaRulesOfIndex(stub.rules)
	writes := 0
	where := ""
	check := func(what string) {
		writes++
		for i, rules := range aaRulesOfIndex(stub.rules) {
			for _, rn := range rules {
				if !slices.Contains(start[i], rn) {
					t.Errorf("%s, write %d (%s): index %d is in %s; it started in %v (rules %v)", where, writes,
						what, i, rn, start[i], stub.rules)
				}
			}
		}
		if d := aaDoctorDrift(t, deps); len(d) != 0 {
			t.Errorf("%s, write %d (%s): the doctor would write %v; rules %v, comments %v", where, writes, what,
				d, stub.rules, stub.ruleComments)
		}
	}
	stub.onRuleWrite = func(rule string) { check("rule " + rule) }
	stub.onResourceWrite = func(sid string) { check("resource " + sid) }
	for b, batch := range batches {
		for _, ev := range events[b] {
			where = fmt.Sprintf("batch %v, %s", batch, ev)
			kind, idx, _ := strings.Cut(ev, " ")
			i, _ := strconv.Atoi(idx)
			switch kind {
			case "delete":
				fleet.destroy(100 + i)
				if err := removeAntiAffinityMembership(ctx, deps, 100+i, webTags(100+i),
					log.NewNopLogger()); err != nil {
					t.Fatalf("%s: %v", where, err)
				}
			case "create":
				fleet.create(200 + i)
			case "join":
				fleet.tag(200 + i)
				joinAntiAffinityAtMetadata(ctx, deps, 200+i, map[string]any{"index": float64(i)}, log.NewNopLogger())
			}
			if t.Failed() {
				t.FailNow()
			}
		}
	}
	if end := aaRulesOfIndex(stub.rules); !maps.EqualFunc(end, start, slices.Equal) {
		t.Fatalf("the rules of each index end as %v; they started as %v (rules %v)", end, start, stub.rules)
	}
	if d := aaDoctorDrift(t, deps); len(d) != 0 {
		t.Fatalf("the doctor would write %v after the roll", d)
	}
}

// aaRulesOfIndex maps each index to the sorted names of the rules that hold
// a member with that index, reading the index from the VMID's last two
// digits.
func aaRulesOfIndex(rules map[string]string) map[int][]string {
	out := map[int][]string{}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		for _, sid := range strings.Split(rules[name], ",") {
			v, err := strconv.Atoi(strings.TrimPrefix(sid, "vm:"))
			if err != nil {
				continue
			}
			out[v%100] = append(out[v%100], name)
		}
	}
	return out
}

// TestJoinAntiAffinityAtMetadata_AMemberAloneByDesignLogsNoWarning pins the
// log line for a member that the canonical layout leaves alone. Five members
// on three nodes leave index 4 in a run of its own, so no rule holds it while
// the group keeps five members. The plan for index 0, which writes the second
// rule, says so at Debug, and the plan for index 4 itself says it at Info.
// Neither is a warning, and the line promises no member that will join index
// 4's rule.
func TestJoinAntiAffinityAtMetadata_AMemberAloneByDesignLogsNoWarning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stub := newAAStub()
	newAACanonFleet(stub, 100, 101, 102, 103, 104)
	stub.seedRule(aaRule, "vm:100,vm:101")
	deps := aaReplanDepsWithLocks(stub, true, newAALockPools(nil))
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
	for _, tc := range []struct {
		vmid  int
		level slog.Level
	}{{100, slog.LevelDebug}, {104, slog.LevelInfo}} {
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		joinAntiAffinityAtMetadata(ctx, deps, tc.vmid, map[string]any{"index": float64(tc.vmid % 100)}, logger)
		var alone []log.Entry
		for _, e := range obs.All() {
			if strings.Contains(e.Message, "alone") {
				alone = append(alone, e)
			}
		}
		if len(alone) != 1 {
			t.Fatalf("plan of vm:%d logged %d lines about the member alone; want one (%+v)", tc.vmid, len(alone),
				obs.All())
		}
		e := alone[0]
		if e.Level != tc.level || strings.Contains(e.Message, "next member") ||
			!strings.Contains(e.Message, "by design") || e.Attrs["sid"] != "vm:104" {
			t.Fatalf("plan of vm:%d logged %+v; want one %v line for vm:104 that says it is alone by design and "+
				"promises no later member", tc.vmid, e, tc.level)
		}
		if stub.rules[aaRule] != "vm:100,vm:101" || stub.rules[aaRule+"-2"] != "vm:102,vm:103" {
			t.Fatalf("rules = %v; want the canonical layout", stub.rules)
		}
	}
}
