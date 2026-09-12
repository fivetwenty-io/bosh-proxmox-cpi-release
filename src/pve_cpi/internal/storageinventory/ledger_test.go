package storageinventory

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func plan(t *testing.T, l Ledger, s *Snapshot, id, storage string, bytes uint64) Ledger {
	t.Helper()
	next, err := l.WithPlanned(s, Charge{ID: id, Role: id, Node: "n1", StorageID: storage, Bytes: bytes})
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func submit(t *testing.T, l Ledger, id string) Ledger {
	t.Helper()
	next, err := l.Submit(id)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func acquire(t *testing.T, c *Collector, l Ledger) Ledger {
	const id = "root"
	t.Helper()
	var record ChargeRecord
	for rIndex := range l.Records() {
		r := l.Records()[rIndex]
		if r.Charge.ID == id {
			record = r
		}
	}
	proof, err := c.MarkCompletion(record, "owned:"+id, true, true)
	if err != nil {
		t.Fatal(err)
	}
	next, err := l.Acquire(id, proof)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestLedgerWholeBundlesAndSharedDomainCharges(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "a", "b")
	source.set("n1", status(t, "a", 2000, 1000), status(t, "b", 2000, 1000))
	c := collector(t, source, newClock())
	cfg := policy("a", "b")
	s := discover(t, c, cfg, "n1")
	empty := NewLedger()
	if _, err := empty.Candidate(s, "n1", "a", 600, Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Candidate(s, "n1", "a", 500, Limits{}); err != nil {
		t.Fatal(err)
	}
	root := plan(t, empty, s, "root", "a", 600)
	if _, err := root.WithPlanned(s, Charge{ID: "ephemeral", Role: "ephemeral", Node: "n1", StorageID: "a", Bytes: 500}); err == nil {
		t.Fatal("individually fitting disks manufactured bundle capacity")
	}
	if len(empty.Records()) != 0 || len(root.Records()) != 1 {
		t.Fatal("ledger mutation leaked between planning branches")
	}
	if _, err := root.Candidate(s, "n1", "b", 500, Limits{}); err != nil {
		t.Fatalf("independent backing wrongly charged: %v", err)
	}
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"nas": {Members: []string{"a", "b"}}}
	shared := discover(t, c, cfg, "n1")
	root = plan(t, NewLedger(), shared, "root", "a", 600)
	if _, err := root.Candidate(shared, "n1", "b", 500, Limits{}); err == nil {
		t.Fatal("distinct exports multiplied domain capacity")
	}
	fit, err := root.Candidate(shared, "n1", "b", 400, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if fit.Member.OutstandingBytes != 0 || fit.Domain.OutstandingBytes != 600 {
		t.Fatalf("wrong charge scope: %+v", fit)
	}
	// Shared domains may span the global roles with disjoint physical members.
	cfg.EphemeralStorageSet = "E"
	cfg.PersistentStorageSet = "P"
	cfg.StorageSets = map[string]config.StorageSet{"E": {Names: []string{"a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}, "P": {Names: []string{"b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}
	if _, err := c.Discover(context.Background(), cfg, Request{Nodes: []string{"n1"}}); err != nil {
		t.Fatalf("domain falsely violated role disjointness: %v", err)
	}
}

func TestLedgerChargesRemainScopedToLocalNode(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "shared", "local")
	source.defs[1] = definition(t, "local", map[string]any{"type": "dir", "shared": 0, "path": "/var/lib/vz"})
	source.set("n1", status(t, "shared", 1000, 1000), status(t, "local", 1000, 1000))
	source.set("n2", status(t, "shared", 1000, 1000), status(t, "local", 1000, 1000))
	c := collector(t, source, newClock())
	s, err := c.Discover(context.Background(), policy("shared"), Request{Nodes: []string{"n1", "n2"}, CompanionStorageIDs: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	l := plan(t, NewLedger(), s, "legacy-root", "local", 700)
	if _, err := l.Candidate(s, "n1", "local", 400, Limits{}); err == nil {
		t.Fatal("local same-node charge ignored")
	}
	if candidate, err := l.Candidate(s, "n2", "local", 400, Limits{}); err != nil || candidate.Member.OutstandingBytes != 0 {
		t.Fatalf("local charge leaked to different node: %+v %v", candidate, err)
	}
}

func TestLedgerCombinesPolicyLimitsAndCheckedBytes(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "a", "b")
	source.set("n1", status(t, "a", 1000, 1000), status(t, "b", 1000, 1000))
	cfg := policy("a", "b")
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"nas": {Members: []string{"a", "b"}}}
	s := discover(t, collector(t, source, newClock()), cfg, "n1")
	combined, err := CombineLimits(Limits{ReserveBytes: 200, MaxUtilizationPct: 80}, Limits{ReserveBytes: 300, MaxUtilizationPct: 90}, Limits{MaxUtilizationPct: 70})
	if err != nil || combined.ReserveBytes != 300 || combined.MaxUtilizationPct != 70 {
		t.Fatalf("combined policy: %+v %v", combined, err)
	}
	l, err := NewLedger().WithPlanned(s, Charge{ID: "root", Role: "root", Node: "n1", StorageID: "a", Bytes: 400, Limits: combined})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Candidate(s, "n1", "b", 301, Limits{MaxUtilizationPct: 100}); err == nil {
		t.Fatal("domain weakened earlier role reserve/ceiling")
	}
	if _, err := l.Candidate(s, "n1", "b", 300, Limits{}); err != nil {
		t.Fatalf("exact policy boundary failed: %v", err)
	}
	for _, tc := range []struct{ value, unit, want uint64 }{{1, 4096, 4096}, {4096, 4096, 4096}, {0, 4096, 0}, {math.MaxUint64, 1, math.MaxUint64}} {
		got, err := RoundBytes(tc.value, tc.unit)
		if err != nil || got != tc.want {
			t.Fatalf("rounding %+v: %d %v", tc, got, err)
		}
	}
	if _, err := RoundBytes(math.MaxUint64, 4096); err == nil {
		t.Fatal("rounding overflow accepted")
	}
	if _, err := RoundBytes(1, 0); err == nil {
		t.Fatal("zero alignment accepted")
	}
	if _, err := CombineLimits(Limits{MaxUtilizationPct: 101}); err == nil {
		t.Fatal("invalid ceiling accepted")
	}
	source.set("n1", status(t, "a", math.MaxUint64, math.MaxUint64))
	large := discover(t, collector(t, source, newClock()), policy("a"), "n1")
	all := plan(t, NewLedger(), large, "all", "a", math.MaxUint64)
	if _, err := all.Candidate(large, "n1", "a", 1, Limits{}); err == nil {
		t.Fatal("aggregate byte overflow accepted")
	}
	set := cfg.StorageSets["active"]
	set.MinFreeMB = 1
	ceiling := 75
	set.MaxUtilizationPct = &ceiling
	cfg.StorageSets["active"] = set
	limitsSnapshot := discover(t, collector(t, source, newClock()), cfg, "n2")
	limits, err := limitsSnapshot.SetLimits("active")
	if err != nil || limits.ReserveBytes != 1024*1024 || limits.MaxUtilizationPct != 75 {
		t.Fatalf("MiB conversion: %+v %v", limits, err)
	}
}

func TestLedgerDoesNotRetirePrecompletionOrDelayedObservation(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a")
	source.set("n1", status(t, "a", 1000, 1000))
	c := collector(t, source, clock)
	before := discover(t, c, policy("a"), "n1")
	l := submit(t, plan(t, NewLedger(), before, "root", "a", 600), "root")
	entered, release := make(chan struct{}), make(chan struct{})
	source.hook = func(context.Context, string, int) ([]json.RawMessage, error) {
		close(entered)
		<-release
		return []json.RawMessage{status(t, "a", 1000, 400)}, nil
	}
	refreshed := make(chan *Snapshot, 1)
	fail := make(chan error, 1)
	go func() {
		s, err := c.Refresh(context.Background(), before)
		if err != nil {
			fail <- err
			return
		}
		refreshed <- s
	}()
	<-entered
	clock.Advance(time.Second)
	l = acquire(t, c, l)
	close(release)
	var delayed *Snapshot
	select {
	case delayed = <-refreshed:
	case err := <-fail:
		t.Fatal(err)
	}
	for _, s := range []*Snapshot{before, delayed} {
		kept, err := l.Refresh(s, clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if kept.Records()[0].Reflected {
			t.Fatal("precompletion request retired acquired charge")
		}
		totals, err := kept.Totals("nfs://nas/a")
		if err != nil || totals.AcquiredBytes != 600 || totals.OutstandingBytes != 600 {
			t.Fatalf("lost outstanding root: %+v %v", totals, err)
		}
	}
	// A genuinely later fetch can retire the charge, avoiding double-counting.
	source.hook = nil
	source.set("n1", status(t, "a", 1000, 400))
	clock.Advance(time.Second)
	after, err := c.Refresh(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	accounted, err := l.Refresh(after, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !accounted.Records()[0].Reflected {
		t.Fatal("new postcompletion observation did not retire charge")
	}
	if _, err := accounted.Candidate(after, "n1", "a", 400, Limits{}); err != nil {
		t.Fatalf("root was charged twice: %v", err)
	}
	if _, err := accounted.Candidate(before, "n1", "a", 500, Limits{}); err == nil {
		t.Fatal("old status reused reduced ledger")
	}
	reverted, err := accounted.Refresh(before, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Records()[0].Reflected {
		t.Fatal("old refresh retained reduced charge")
	}
}

func TestLedgerSparseGrowthFreshnessAndEpoch(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a")
	source.set("n1", status(t, "a", 1000, 1000))
	c := collector(t, source, clock)
	before := discover(t, c, policy("a"), "n1")
	l := acquire(t, c, submit(t, plan(t, NewLedger(), before, "root", "a", 600), "root"))
	// The observed physical write is only 20 bytes: retiring the immediate
	// charge does not pretend that free-space statistics reserve virtual growth.
	source.set("n1", status(t, "a", 1000, 980))
	after, err := c.Refresh(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	l, err = l.Refresh(after, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Candidate(after, "n1", "a", 900, Limits{}); err != nil {
		t.Fatalf("sparse observed usage double charged: %v", err)
	}
	source.set("n1", status(t, "a", 1000, 300))
	grown, err := c.Refresh(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Candidate(grown, "n1", "a", 400, Limits{}); err == nil {
		t.Fatal("later sparse growth ignored")
	}
	clock.Advance(6 * time.Second)
	stale, err := l.Refresh(after, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stale.Records()[0].Reflected {
		t.Fatal("stale snapshot retired charge")
	}
	otherCollector := collector(t, source, clock)
	other := discover(t, otherCollector, policy("a"), "n1")
	conservative, err := l.Refresh(other, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if conservative.Records()[0].Reflected {
		t.Fatal("other collector epoch certified old proof")
	}
	clock.Advance(-time.Hour)
	backwards, err := l.Refresh(after, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if backwards.Records()[0].Reflected {
		t.Fatal("backward clock retired charge")
	}
}

func TestLedgerProofBindingSubmissionAndRetention(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a")
	source.set("n1", status(t, "a", 1000, 1000))
	c := collector(t, source, clock)
	s := discover(t, c, policy("a"), "n1")
	planned := plan(t, NewLedger(), s, "root", "a", 300)
	if _, err := c.MarkCompletion(planned.Records()[0], "owned", true, true); err == nil {
		t.Fatal("unsubmitted charge accepted")
	}
	removed, err := planned.RemovePlanned("root")
	if err != nil || len(removed.Records()) != 0 {
		t.Fatalf("speculative plan removal: %v", err)
	}
	submitted := submit(t, planned, "root")
	if _, err := submitted.RemovePlanned("root"); err == nil {
		t.Fatal("uncertain submitted charge removed")
	}
	if _, err := submitted.Acquire("root", CompletionEvidence{}); err == nil {
		t.Fatal("unsealed completion accepted")
	}
	for _, flags := range [][2]bool{{false, true}, {true, false}, {false, false}} {
		if _, err := c.MarkCompletion(submitted.Records()[0], "owned", flags[0], flags[1]); err == nil {
			t.Fatal("incomplete ownership proof accepted")
		}
	}
	wrong := submitted.Records()[0]
	wrong.Charge.Bytes++
	proof, err := c.MarkCompletion(wrong, "owned", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := submitted.Acquire("root", proof); err == nil {
		t.Fatal("proof for another charge adopted")
	}
	acquired := acquire(t, c, submitted)
	retained, err := acquired.Retain("root")
	if err != nil {
		t.Fatal(err)
	}
	if !retained.HasRetained() {
		t.Fatal("retention was lost")
	}
	if _, err := retained.Candidate(s, "n1", "a", 1, Limits{}); err == nil {
		t.Fatal("retained volume permitted fallback")
	}
	if _, err := retained.WithPlanned(s, Charge{ID: "retry", Role: "root", Node: "n1", StorageID: "a", Bytes: 1}); err == nil {
		t.Fatal("retained allocation allowed replacement plan")
	}
	fresh, err := c.Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	retained, err = retained.Refresh(fresh, clock.Now())
	if err != nil || retained.Records()[0].Reflected {
		t.Fatalf("retained charge retired: %v", err)
	}
	if _, err := planned.Retain("root"); err == nil {
		t.Fatal("unacquired retention accepted")
	}
	if _, err := submitted.Submit("root"); err == nil {
		t.Fatal("submitted twice")
	}
	if _, err := acquired.Acquire("root", proof); err == nil {
		t.Fatal("acquired twice")
	}
	// A snapshot from a changed physical target can never repoint old records.
	source.defs[0] = definition(t, "a", map[string]any{"export": "/other"})
	repointed := discover(t, c, policy("a"), "n1")
	if _, err := acquired.Refresh(repointed, clock.Now()); err == nil {
		t.Fatal("repointed backing silently adopted")
	}
}

func TestDomainRetirementRequiresEveryContributingObservation(t *testing.T) {
	t.Parallel()
	clock := newClock()
	source := sourceFor(t, "a", "b")
	source.set("n1", status(t, "a", 1000, 1000), status(t, "b", 1000, 1000))
	cfg := policy("a")
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"nas": {Members: []string{"a", "b"}}}
	c := collector(t, source, clock)
	before := discover(t, c, cfg, "n1")
	l := acquire(t, c, submit(t, plan(t, NewLedger(), before, "root", "a", 100), "root"))
	source.set("n1", status(t, "a", 1000, 900), raw(t, map[string]any{"storage": "b", "active": 0, "enabled": 1}))
	incomplete, err := c.Refresh(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := l.Refresh(incomplete, clock.Now())
	if err != nil || kept.Records()[0].Reflected {
		t.Fatalf("incomplete domain retired charge: %v", err)
	}
	source.set("n1", status(t, "a", 1000, 900), status(t, "b", 1000, 900))
	complete, err := c.Refresh(context.Background(), before)
	if err != nil {
		t.Fatal(err)
	}
	done, err := l.Refresh(complete, clock.Now())
	if err != nil || !done.Records()[0].Reflected {
		t.Fatalf("complete domain did not retire charge: %v", err)
	}
}

func TestLedgerRejectsChangedDomainsAndUnavailableTargets(t *testing.T) {
	t.Parallel()
	source := sourceFor(t, "a", "b")
	c := collector(t, source, newClock())
	s := discover(t, c, policy("a"), "n1")
	l := plan(t, NewLedger(), s, "root", "a", 100)
	cfg := policy("a")
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"new-domain": {Members: []string{"a", "b"}}}
	changed := discover(t, c, cfg, "n1")
	if _, err := l.Candidate(changed, "n1", "a", 100, Limits{}); err == nil {
		t.Fatal("changed domain ignored existing charge")
	}
	if _, err := l.Refresh(changed, newClock().Now()); err == nil {
		t.Fatal("changed domain reinterpreted old ledger")
	}
	other := discover(t, c, policy("b"), "n2")
	if _, err := l.Refresh(other, newClock().Now()); err == nil {
		t.Fatal("missing historical target certified")
	}
	if _, err := NewLedger().Candidate(s, "missing-node", "a", 1, Limits{}); err == nil {
		t.Fatal("absent node accepted")
	}
	if _, err := NewLedger().WithPlanned(s, Charge{}); err == nil {
		t.Fatal("empty charge accepted")
	}
	if _, err := l.WithPlanned(s, l.Records()[0].Charge); err == nil {
		t.Fatal("duplicate charge accepted")
	}
	if _, err := l.Submit("missing"); err == nil {
		t.Fatal("submitted missing charge")
	}
	if _, err := l.RemovePlanned("missing"); err == nil {
		t.Fatal("removed missing charge")
	}
	if _, err := s.SetLimits("unknown"); err == nil {
		t.Fatal("unknown set limits accepted")
	}
}
