package handlers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
)

// spreadClaimStore returns a claim record in a new directory that only this
// user can open, which the cache directory checks require.
func spreadClaimStore(t *testing.T) *spreadclaim.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &spreadclaim.Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "lab"}
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
		deps.SpreadClaims = func(*config.CPIConfig, *log.Logger) *spreadclaim.Store { return store }
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
	group := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	group.claims = claims
	node, _, err := resolveTargetNodeWithBridges(context.Background(), deps, createVMCloudProps{}, group, nil, nil,
		nil, 0, bridgePlan{})
	if err != nil {
		t.Fatal(err)
	}
	return node
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
	)
	start := make(chan struct{})
	for _, c := range claims {
		wg.Go(func() {
			<-start
			node := placeSibling(t, deps, c)
			mu.Lock()
			defer mu.Unlock()
			picks = append(picks, node)
		})
	}
	close(start)
	wg.Wait()
	slices.Sort(picks)
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(picks, want) {
		t.Fatalf("picks = %v; want one sibling on each node %v", picks, want)
	}
	for _, c := range claims {
		if c.claim.Node() == "" {
			t.Error("every create must hold the node it picked until its clone carries the tag")
		}
	}
}

// TestSpreadClaims_ReleasedClaimNoLongerCounts gives a claim up the way a
// create does once its clone carries the member tag. The tag then carries the
// count, so the claim must not count twice.
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
// when placement scores nodes with the anti-affinity axis, so every other
// create places exactly as before.
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
		}, false},
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
