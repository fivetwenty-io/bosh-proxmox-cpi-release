package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// spreadClaimLockWait only guards against a hang. Parallel siblings queue on
// the claim record's file lock, and each holder fsyncs the record, so the
// default five seconds measures how loaded the machine is and not whether the
// siblings spread. A pick that waits this long has a stuck holder.
const spreadClaimLockWait = 2 * time.Minute

// spreadClaimStore returns a claim record in a new directory that only this
// user can open, which the cache directory checks require.
func spreadClaimStore(t *testing.T) *spreadclaim.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &spreadclaim.Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "lab",
		LockWait: spreadClaimLockWait}
}

// spreadDeps wires placement to three nodes that score the same, with soft
// anti-affinity on. A nil store leaves the claims off.
func spreadDeps(store *spreadclaim.Store) Deps {
	on := true
	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2"), onlineNode("pve3")},
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}}
		})
	deps.RequestDirectorUUID = aaDirector
	if store != nil {
		deps.SpreadClaims = func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return store }
	}
	return deps
}

func spreadParsed() *createVMParsedArgs {
	return &createVMParsedArgs{env: aaEnvForWebGroup()}
}

// placeSibling scores the nodes for one create of the web group, the way
// resolveVMShape does, through the create's claim.
func placeSibling(t *testing.T, deps Deps, claims *spreadClaims) string {
	t.Helper()
	node, err := trySibling(deps, claims)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// trySibling is placeSibling for a goroutine, which must not stop the test
// itself, so it returns the error instead.
func trySibling(deps Deps, claims *spreadClaims) (string, error) {
	group := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	group.claims = claims
	node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, createVMCloudProps{}, group, nil, nil,
		nil, 0, bridgePlan{})
	return node, err
}

func mustSpreadClaims(t *testing.T, deps Deps) *spreadClaims {
	t.Helper()
	s := newSpreadClaims(deps, spreadParsed(), time.Now().Add(time.Hour), deps.Logger)
	if s == nil {
		t.Fatal("a create that spreads must hold a claim")
	}
	return s
}

// TestSpreadClaims_ParallelSiblingsPickDistinctNodes places three creates of
// one group at the same moment, before any of them carries the member tag.
// Without claims every create sees the same empty cluster and picks the same
// node. With them each pick sees the picks before it, so whatever order the
// picks run in, the three land on three nodes.
func TestSpreadClaims_ParallelSiblingsPickDistinctNodes(t *testing.T) {
	t.Parallel()

	plain := spreadDeps(nil)
	first := placeSibling(t, plain, nil)
	for range 2 {
		if got := placeSibling(t, plain, nil); got != first {
			t.Fatalf("without claims the scorer must pick %q every time, got %q", first, got)
		}
	}

	deps := spreadDeps(spreadClaimStore(t))
	claims := make([]*spreadClaims, 3)
	for i := range claims {
		claims[i] = mustSpreadClaims(t, deps)
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		picks []string
		errs  []error
	)
	start := make(chan struct{})
	for _, c := range claims {
		wg.Go(func() {
			<-start
			node, err := trySibling(deps, c)
			mu.Lock()
			defer mu.Unlock()
			picks = append(picks, node)
			errs = append(errs, err)
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	slices.Sort(picks)
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(picks, want) {
		t.Fatalf("picks = %v; want one sibling on each node %v", picks, want)
	}
	for _, c := range claims {
		if c.claim.Node() == "" {
			t.Error("every create must hold the node it picked until create_vm returns")
		}
	}
}

// TestSpreadClaims_ReleasedClaimNoLongerCounts gives a claim up the way a
// create does when create_vm returns. The member tag carries the count from
// then on, so the claim must not count twice.
func TestSpreadClaims_ReleasedClaimNoLongerCounts(t *testing.T) {
	t.Parallel()
	deps := spreadDeps(spreadClaimStore(t))
	a := mustSpreadClaims(t, deps)
	first := placeSibling(t, deps, a)
	a.release(context.Background())
	a.release(context.Background())
	if a.claim.Node() != "" {
		t.Fatalf("a released claim holds %q", a.claim.Node())
	}
	if got := placeSibling(t, deps, mustSpreadClaims(t, deps)); got != first {
		t.Errorf("after the release the next create picked %q; want %q, because nothing else counts", got, first)
	}
}

// TestSpreadClaims_MoveCountsOnTheAlternate moves a claim to an alternate, as
// the fallback loop does, and checks that later siblings count it there.
func TestSpreadClaims_MoveCountsOnTheAlternate(t *testing.T) {
	t.Parallel()
	deps := spreadDeps(spreadClaimStore(t))
	a := mustSpreadClaims(t, deps)
	if placeSibling(t, deps, a) == "pve3" {
		t.Fatal("the tie-break is expected to prefer another node over pve3")
	}
	a.move(context.Background(), "pve3")
	b := placeSibling(t, deps, mustSpreadClaims(t, deps))
	c := placeSibling(t, deps, mustSpreadClaims(t, deps))
	got := []string{b, c, "pve3"}
	slices.Sort(got)
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(got, want) {
		t.Fatalf("siblings after the move picked %q and %q; want them on the two nodes the claim left", b, c)
	}
}

// TestNewSpreadClaims_OnlyForACreateThatSpreads checks that a claim exists only
// when placement spreads the instance group with the anti-affinity axis, so
// every other create places exactly as before. A create that target_node pins
// spreads nothing itself, but it claims its node so the siblings that score
// while it clones count it.
func TestNewSpreadClaims_OnlyForACreateThatSpreads(t *testing.T) {
	t.Parallel()
	store := spreadClaimStore(t)
	exp := time.Now().Add(time.Hour)
	off := false

	cases := []struct {
		name   string
		deps   func() Deps
		parsed func() *createVMParsedArgs
		want   bool
	}{
		{"spreads", func() Deps { return spreadDeps(store) }, spreadParsed, true},
		{"no claim source", func() Deps { return spreadDeps(nil) }, spreadParsed, false},
		{"anti-affinity off", func() Deps {
			d := spreadDeps(store)
			d.Config.Placement.AntiAffinity.Enabled = &off
			return d
		}, spreadParsed, false},
		{"placement off", func() Deps {
			d := spreadDeps(store)
			d.Config.Placement.Enabled = &off
			return d
		}, spreadParsed, false},
		{"operator pin", func() Deps { return spreadDeps(store) }, func() *createVMParsedArgs {
			p := spreadParsed()
			p.cloudProps.TargetNode = "pve2"
			return p
		}, true},
		{"no instance group", func() Deps { return spreadDeps(store) }, func() *createVMParsedArgs {
			return &createVMParsedArgs{env: map[string]any{}}
		}, false},
	}
	for _, tc := range cases {
		deps := tc.deps()
		if got := newSpreadClaims(deps, tc.parsed(), exp, deps.Logger) != nil; got != tc.want {
			t.Errorf("%s: claim = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestSpreadClaims_NilSessionPicksPlainly checks the nil session every create
// without claims holds.
func TestSpreadClaims_NilSessionPicksPlainly(t *testing.T) {
	t.Parallel()
	var s *spreadClaims
	s.move(context.Background(), "pve1")
	s.release(context.Background())
	deps := spreadDeps(nil)
	if got, want := placeSibling(t, deps, s), placeSibling(t, deps, nil); got != want {
		t.Errorf("nil session picked %q; want %q", got, want)
	}
}

// TestSpreadClaimExpiry_FollowsTheCreateDeadline checks that a claim lapses
// with its create.
func TestSpreadClaimExpiry_FollowsTheCreateDeadline(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(7 * time.Minute).Truncate(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if got := spreadClaimExpiry(ctx, spreadDeps(nil)); !got.Equal(deadline) {
		t.Errorf("expiry = %v; want the create's deadline %v", got, deadline)
	}
	before := time.Now()
	got := spreadClaimExpiry(context.Background(), spreadDeps(nil))
	if budget := createVMIDClaimBudget(spreadDeps(nil)); got.Before(before.Add(budget)) {
		t.Errorf("expiry = %v; want the create budget %v from now", got, budget)
	}
}

// spreadFacts is three online nodes with room for any test VM.
func spreadFacts() []placement.NodeFacts {
	const gib = int64(1) << 30
	out := make([]placement.NodeFacts, 0, 3)
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		out = append(out, placement.NodeFacts{Node: n, Online: true, MaxCPU: 8, FreeMemBytes: 8 * gib,
			TotalMemBytes: 8 * gib})
	}
	return out
}

// claimCounts returns the claims a create counts against facts, without
// recording one itself.
func claimCounts(t *testing.T, deps Deps, facts []placement.NodeFacts) map[string]int {
	t.Helper()
	var seen map[string]int
	mustSpreadClaims(t, deps).pick(context.Background(), facts, func(m map[string]int) string {
		seen = maps.Clone(m)
		return ""
	})
	return seen
}

// TestSpreadClaims_TaggedSiblingCountsOnce checks that a sibling whose VM
// already carries the member tag counts once. Its claim names its VMID, and a
// create whose facts count that VMID from the tag leaves the claim out.
func TestSpreadClaims_TaggedSiblingCountsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps := spreadDeps(spreadClaimStore(t))
	a := mustSpreadClaims(t, deps)
	a.move(ctx, "pve1")
	a.bind(ctx, 101)
	if got := a.claim.VMID(); got != 101 {
		t.Fatalf("claim VMID = %d; want 101", got)
	}

	untagged := spreadFacts()
	if got := claimCounts(t, deps, untagged); !maps.Equal(got, map[string]int{"pve1": 1}) {
		t.Errorf("claims before the tag lands = %v; want the sibling's claim on pve1", got)
	}
	tagged := spreadFacts()
	tagged[0].SameGroupCount, tagged[0].SameGroupVMIDs = 1, []int{101}
	if got := claimCounts(t, deps, tagged); len(got) != 0 {
		t.Errorf("claims once the tag counts VM 101 = %v; want none, so the sibling counts once", got)
	}
	other := spreadFacts()
	other[0].SameGroupCount, other[0].SameGroupVMIDs = 1, []int{205}
	if got := claimCounts(t, deps, other); !maps.Equal(got, map[string]int{"pve1": 1}) {
		t.Errorf("claims when the tag counts another VM = %v; want the sibling's claim on pve1", got)
	}
}

// TestSpreadClaims_PinnedCreateHoldsItsNode checks that a create that
// target_node pins claims its node, so the siblings that score while it
// clones leave that node for last.
func TestSpreadClaims_PinnedCreateHoldsItsNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps := spreadDeps(spreadClaimStore(t))
	pinned := spreadParsed()
	pinned.cloudProps.TargetNode = "pve1"
	s := newSpreadClaims(deps, pinned, time.Now().Add(time.Hour), deps.Logger)
	if s == nil {
		t.Fatal("a pinned create in a spread group must hold a claim")
	}
	s.hold(ctx, "pve1")
	b := placeSibling(t, deps, mustSpreadClaims(t, deps))
	c := placeSibling(t, deps, mustSpreadClaims(t, deps))
	got := []string{b, c}
	slices.Sort(got)
	if want := []string{"pve2", "pve3"}; !slices.Equal(got, want) {
		t.Fatalf("siblings of the pinned create picked %v; want %v", got, want)
	}
}

// TestRankStorageComputeGroups_CountsAndClaimsThroughTheSpread checks that a
// storage plan ranks the nodes with the claims of the group's creates in
// flight, and claims the node it ranks first.
func TestRankStorageComputeGroups_CountsAndClaimsThroughTheSpread(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps := spreadDeps(spreadClaimStore(t))
	weights := placement.DefaultWeights()
	eligible := map[string]bool{"pve1": true, "pve2": true, "pve3": true}
	rank := func(parsed *createVMParsedArgs) string {
		t.Helper()
		groups, err := rankStorageComputeGroups(ctx, deps, parsed, []string{""}, "", spreadFacts(), weights, nil,
			nil, 1, 1, 512, eligible)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) == 0 || len(groups[0].Nodes) == 0 {
			t.Fatal("the ranking returned no node")
		}
		return groups[0].Nodes[0]
	}
	first := rank(spreadParsed())
	sibling := mustSpreadClaims(t, deps)
	sibling.move(ctx, first)

	parsed := spreadParsed()
	parsed.spread = mustSpreadClaims(t, deps)
	got := rank(parsed)
	if got == first {
		t.Fatalf("the plan ranked %q first, where a sibling's claim already sits", got)
	}
	if held := parsed.spread.claim.Node(); held != got {
		t.Errorf("the plan's claim holds %q; want the node it ranked first, %q", held, got)
	}
}

// TestSpreadClaimScope_NamesTheCluster checks that two cpi_config entries
// that reach one cluster through different hosts share a scope, that two
// clusters never do, and that an entry falls back to its endpoints only when
// the cluster's stamp is unknown.
func TestSpreadClaimScope_NamesTheCluster(t *testing.T) {
	t.Parallel()
	viaA := &config.CPIConfig{Host: "pve-a.example", Port: 8006}
	viaB := &config.CPIConfig{Host: "pve-b.example", Port: 8006}
	if a, b := spreadClaimScope(viaA, "1a2b3c4d"), spreadClaimScope(viaB, "1a2b3c4d"); a != b {
		t.Errorf("one cluster through two hosts got scopes %q and %q; want one", a, b)
	}
	if a, b := spreadClaimScope(viaA, "1a2b3c4d"), spreadClaimScope(viaA, "5e6f7a8b"); a == b {
		t.Errorf("two clusters share the scope %q", a)
	}
	for _, stamp := range []string{"", vmcid.UnknownCluster} {
		got := spreadClaimScope(viaA, stamp)
		if want := "endpoints:" + ha.EpisodeScope(viaA); got != want {
			t.Errorf("stamp %q: scope = %q; want the endpoints fallback %q", stamp, got, want)
		}
		if got == spreadClaimScope(viaB, stamp) {
			t.Errorf("stamp %q: two endpoints share the fallback scope %q", stamp, got)
		}
	}
	if got := spreadClaimScope(&config.CPIConfig{}, vmcid.UnknownCluster); got != "" {
		t.Errorf("an entry with no stamp and no endpoints got scope %q; want none", got)
	}
}

// TestEndpointCacheSpreadClaims_SharesTheRecordAcrossHosts checks the
// production source. A claim made through one host counts for a create that
// reaches the same cluster through another, and an entry with no scope makes
// no claims.
func TestEndpointCacheSpreadClaims_SharesTheRecordAcrossHosts(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	viaA := &config.CPIConfig{Host: "pve-a.example", Port: 8006, EndpointCacheDir: dir}
	viaB := &config.CPIConfig{Host: "pve-b.example", Port: 8006, EndpointCacheDir: dir}
	logger := log.NewNopLogger()
	exp := time.Now().Add(time.Hour)
	ctx := context.Background()

	a := EndpointCacheSpreadClaims(viaA, "1a2b3c4d", logger)
	if a == nil {
		t.Fatal("an entry with a cluster stamp must have a claim record")
	}
	claim, err := a.NewClaim("web", exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim.Move(ctx, "pve1"); err != nil {
		t.Fatal(err)
	}
	seen := func(store *spreadclaim.Store) map[string]int {
		t.Helper()
		c, err := store.NewClaim("web", exp)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]int
		if _, err := c.Pick(ctx, func(m map[string]int) string { got = maps.Clone(m); return "" }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := seen(EndpointCacheSpreadClaims(viaB, "1a2b3c4d", logger)); !maps.Equal(got, map[string]int{"pve1": 1}) {
		t.Errorf("claims through the other host = %v; want the claim on pve1", got)
	}
	if got := seen(EndpointCacheSpreadClaims(viaB, "5e6f7a8b", logger)); len(got) != 0 {
		t.Errorf("claims in another cluster = %v; want none", got)
	}
	if got := EndpointCacheSpreadClaims(&config.CPIConfig{EndpointCacheDir: dir}, "", logger); got != nil {
		t.Error("an entry with no stamp and no endpoints must make no claims")
	}
}

// TestSpreadClaims_PickErrorsSayWhatTheSpreadLost checks that each way a turn
// with the record fails gets its own warning, so a log never says the claims
// were unused when they were counted.
func TestSpreadClaims_PickErrorsSayWhatTheSpreadLost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("open: %w", spreadclaim.ErrUnavailable), "could not be used"},
		{fmt.Errorf("write: %w", spreadclaim.ErrNotRecorded), "counted the claims of in-flight siblings"},
		{fmt.Errorf("decode: %w", spreadclaim.ErrRewritten), "was rewritten"},
		{errors.New("other"), "the spread claim record failed"},
	}
	for _, tc := range cases {
		logger, obs := log.NewObservedLogger(log.LevelInfo)
		(&spreadClaims{logger: logger}).logPickError(tc.err)
		entries := obs.All()
		if len(entries) != 1 || !strings.Contains(entries[0].Message, tc.want) {
			t.Errorf("%v: logged %+v; want one warning that says %q", tc.err, entries, tc.want)
		}
	}
	logger, obs := log.NewObservedLogger(log.LevelInfo)
	(&spreadClaims{logger: logger}).logPickError(nil)
	if obs.Len() != 0 {
		t.Errorf("a turn that worked logged %+v", obs.All())
	}
}

// TestSpreadClaims_HoldWritesAClaimWhosePickFailedToRecord checks that a
// claim whose pick could not write the record is written again by the next
// hold or move to the same node, once the record works, so siblings that
// score after that count it.
func TestSpreadClaims_HoldWritesAClaimWhosePickFailedToRecord(t *testing.T) {
	t.Parallel()
	store := spreadClaimStore(t)
	deps := spreadDeps(store)
	// A directory in the record's place fails the write while the lock works.
	blocker := filepath.Join(store.Dir.Override, spreadclaim.FileName)
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	a := mustSpreadClaims(t, deps)
	node := placeSibling(t, deps, a)
	if a.claim.Node() != node || a.claim.Holds(node) {
		t.Fatalf("claim node %q held %v; want %q picked but not held", a.claim.Node(), a.claim.Holds(node), node)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	a.hold(context.Background(), node)
	if got := claimCounts(t, deps, spreadFacts()); !maps.Equal(got, map[string]int{node: 1}) {
		t.Fatalf("claims after the hold = %v; want the claim on %s", got, node)
	}
}

// TestSpreadClaims_ParallelSiblingsSpreadInEitherOverflowMode places four
// creates of one group on three nodes at the same moment, with HA on. The
// group has more members than the cluster has nodes, so in skip mode no HA
// rule keeps the members apart, and the claims are the only thing that
// spreads the parallel creates. The claim must not depend on the overflow
// mode or on a rule being written, so in both modes no node may get more
// than two of the four siblings.
func TestSpreadClaims_ParallelSiblingsSpreadInEitherOverflowMode(t *testing.T) {
	t.Parallel()
	for _, overflow := range []string{config.HAOverflowSkip, config.HAOverflowSplit} {
		t.Run(overflow, func(t *testing.T) {
			t.Parallel()
			deps := spreadDeps(spreadClaimStore(t))
			deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto),
				AntiAffinityOverflow: haChoiceOf(overflow)}
			claims := make([]*spreadClaims, 4)
			for i := range claims {
				claims[i] = mustSpreadClaims(t, deps)
			}
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				count = map[string]int{}
				errs  []error
			)
			start := make(chan struct{})
			for _, c := range claims {
				wg.Go(func() {
					<-start
					node, err := trySibling(deps, c)
					mu.Lock()
					defer mu.Unlock()
					count[node]++
					errs = append(errs, err)
				})
			}
			close(start)
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			if len(count) != 3 {
				t.Fatalf("picks per node = %v; want the four siblings on all three nodes", count)
			}
			for node, n := range count {
				if n > 2 {
					t.Fatalf("node %s got %d of the four siblings (%v); want at most two", node, n, count)
				}
			}
		})
	}
}

// TestSpreadClaims_ParallelLocalSiblingsInAnAZPickDistinctNodes places two
// creates of one group at the same moment in an AZ of two nodes, with their
// VMs on node-local storage and pve.ha.local_policy hold or replicate. A hold
// VM can't move after it's placed, and a replicate VM moves only to its
// replication target, so the spread must be right at the pick. Two siblings
// on one node would also make their strict pins conflict in the group's
// rule, and the guard would then take one of them out of HA. Whatever order
// the picks run in, the two must land on the AZ's two nodes.
func TestSpreadClaims_ParallelLocalSiblingsInAnAZPickDistinctNodes(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{config.HALocalPolicyHold, config.HALocalPolicyReplicate} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			deps := spreadDeps(spreadClaimStore(t))
			deps.Config.Placement.AZMap = map[string][]string{"z2": {"pve2", "pve3"}}
			deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto), LocalPolicy: haChoiceOf(policy)}
			cp := createVMCloudProps{AvailabilityZone: "z2"}
			claims := []*spreadClaims{mustSpreadClaims(t, deps), mustSpreadClaims(t, deps)}
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				picks []string
				errs  []error
			)
			start := make(chan struct{})
			for _, c := range claims {
				wg.Go(func() {
					<-start
					group := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
					group.claims = c
					node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, cp, group, nil, nil,
						nil, 0, bridgePlan{})
					mu.Lock()
					defer mu.Unlock()
					picks = append(picks, node)
					errs = append(errs, err)
				})
			}
			close(start)
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			slices.Sort(picks)
			if want := []string{"pve2", "pve3"}; !slices.Equal(picks, want) {
				t.Fatalf("picks = %v; want one sibling on each node of the AZ %v", picks, want)
			}
		})
	}
}
