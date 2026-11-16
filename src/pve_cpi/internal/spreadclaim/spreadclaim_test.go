package spreadclaim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
)

// privateDir returns a new directory that only this user can open, which the
// cache directory checks require.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	s := &Store{Dir: endpointcache.DirOptions{Override: privateDir(t)}, Scope: "scope-a"}
	if now != nil {
		s.Now = func() time.Time { return *now }
	}
	return s
}

// leastClaimed picks the node with the fewest claims, the first by name on a
// tie, the way the scorer's anti-affinity penalty would.
func leastClaimed(nodes ...string) func(map[string]int) string {
	return func(counts map[string]int) string {
		sorted := append([]string(nil), nodes...)
		sort.Strings(sorted)
		best := sorted[0]
		for _, n := range sorted[1:] {
			if counts[n] < counts[best] {
				best = n
			}
		}
		return best
	}
}

func mustClaim(t *testing.T, s *Store, group string, expires time.Time) *Claim {
	t.Helper()
	c, err := s.NewClaim(group, expires)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPickSeesTheClaimsOfEarlierPicks(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	exp := time.Now().Add(time.Hour)
	got := make([]string, 0, 4)
	for range 4 {
		c := mustClaim(t, s, "bosh-aa-g", exp)
		node, err := c.Pick(context.Background(), leastClaimed("pve1", "pve2", "pve3"))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, node)
	}
	want := []string{"pve1", "pve2", "pve3", "pve1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("picks = %v; want %v", got, want)
		}
	}
}

func TestPickCountsOnlyTheSameGroupAndScope(t *testing.T) {
	t.Parallel()
	dir := privateDir(t)
	exp := time.Now().Add(time.Hour)
	other := &Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "scope-b"}
	mine := &Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "scope-a"}
	for _, c := range []*Claim{mustClaim(t, other, "bosh-aa-g", exp), mustClaim(t, mine, "bosh-aa-other", exp)} {
		if err := c.Move(context.Background(), "pve1"); err != nil {
			t.Fatal(err)
		}
	}
	var seen map[string]int
	if _, err := mustClaim(t, mine, "bosh-aa-g", exp).Pick(context.Background(), func(c map[string]int) string {
		seen = c
		return "pve1"
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Errorf("counts = %v; another scope's or group's claims must not count", seen)
	}
}

func TestReleaseAndMove(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	exp := time.Now().Add(time.Hour)
	a := mustClaim(t, s, "g", exp)
	if _, err := a.Pick(context.Background(), func(map[string]int) string { return "pve1" }); err != nil {
		t.Fatal(err)
	}
	if err := a.Move(context.Background(), "pve2"); err != nil {
		t.Fatal(err)
	}
	b := mustClaim(t, s, "g", exp)
	var seen map[string]int
	probe := func(c map[string]int) string { seen = c; return "" }
	if _, err := b.Pick(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	if seen["pve1"] != 0 || seen["pve2"] != 1 {
		t.Fatalf("counts after the move = %v; want the claim on pve2 only", seen)
	}
	if b.Node() != "" {
		t.Errorf("a pick that chose nothing holds %q", b.Node())
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("a second release = %v; want nothing to do", err)
	}
	if _, err := b.Pick(context.Background(), probe); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Errorf("counts after the release = %v; want none", seen)
	}
}

func TestExpiredAndImplausibleClaimsDoNotCount(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	s := testStore(t, &now)
	if err := mustClaim(t, s, "g", now.Add(time.Minute)).Move(context.Background(), "pve1"); err != nil {
		t.Fatal(err)
	}
	if err := mustClaim(t, s, "g", now.Add(MaxClaimLifetime+time.Hour)).Move(context.Background(), "pve2"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	var seen map[string]int
	if _, err := mustClaim(t, s, "g", now.Add(time.Minute)).Pick(context.Background(), func(c map[string]int) string {
		seen = c
		return "pve3"
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Fatalf("counts = %v; an expired claim and one dated too far ahead must not count", seen)
	}
	root, err := os.OpenRoot(s.Dir.Override)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	rec, err := readRecord(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Claims) != 1 {
		t.Errorf("record = %v; want the dead claims pruned on the write", rec.Claims)
	}
}

func TestPickWithoutTheRecordStillChooses(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	s.LockWait = 10 * time.Millisecond
	root, err := os.OpenRoot(s.Dir.Override)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	held, err := endpointcache.LockFile(context.Background(), root, LockFileName, Name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	called := false
	node, err := mustClaim(t, s, "g", time.Now().Add(time.Hour)).Pick(context.Background(), func(c map[string]int) string {
		called = true
		if c != nil {
			t.Errorf("counts = %v; want none without the record", c)
		}
		return "pve1"
	})
	if err == nil || !called || node != "pve1" {
		t.Fatalf("node %q err %v called %v; want the pick to run and the busy lock reported", node, err, called)
	}
}

func TestPickRewritesADamagedRecord(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	if err := os.WriteFile(filepath.Join(s.Dir.Override, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	node, err := mustClaim(t, s, "g", time.Now().Add(time.Hour)).Pick(context.Background(), leastClaimed("pve1"))
	if node != "pve1" || err == nil {
		t.Fatalf("node %q err %v; want the pick and the damaged read reported", node, err)
	}
	var seen map[string]int
	if _, err := mustClaim(t, s, "g", time.Now().Add(time.Hour)).Pick(context.Background(), func(c map[string]int) string {
		seen = c
		return ""
	}); err != nil {
		t.Fatalf("the rewritten record must read cleanly: %v", err)
	}
	if seen["pve1"] != 1 {
		t.Errorf("counts = %v; want the claim the rewrite recorded", seen)
	}
}

func TestNewClaimRefusesAnEmptyGroup(t *testing.T) {
	t.Parallel()
	if _, err := testStore(t, nil).NewClaim(" ", time.Now()); err == nil {
		t.Fatal("an empty group must be refused")
	}
}

func TestNilClaimPicksWithoutTheRecord(t *testing.T) {
	t.Parallel()
	var c *Claim
	node, err := c.Pick(context.Background(), func(counts map[string]int) string {
		if counts != nil {
			t.Errorf("counts = %v", counts)
		}
		return "pve1"
	})
	if node != "pve1" || err != nil || c.Move(context.Background(), "pve2") != nil ||
		c.Release(context.Background()) != nil || c.Node() != "" {
		t.Fatalf("a nil claim must pick plainly and do nothing else")
	}
}

// TestConcurrentPicksSpreadEvenly runs creates that pick at the same moment.
// Each pick reads and writes the record in one turn, so whatever order the
// turns run in, every node ends with the same number of claims.
func TestConcurrentPicksSpreadEvenly(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	exp := time.Now().Add(time.Hour)
	const creates = 9
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		picks = map[string]int{}
		errs  []error
	)
	start := make(chan struct{})
	for range creates {
		c := mustClaim(t, s, "bosh-aa-g", exp)
		wg.Go(func() {
			<-start
			node, err := c.Pick(context.Background(), leastClaimed("pve1", "pve2", "pve3"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			picks[node]++
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		if picks[n] != creates/3 {
			t.Fatalf("picks = %v; want %d on every node", picks, creates/3)
		}
	}
}
