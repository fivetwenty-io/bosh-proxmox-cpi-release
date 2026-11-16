package spreadclaim

import (
	"context"
	"errors"
	"maps"
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
	if !errors.Is(err, ErrUnavailable) || !called || node != "pve1" {
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
	if node != "pve1" || !errors.Is(err, ErrRewritten) || errors.Is(err, ErrUnavailable) {
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

// TestReleaseOfAnUnrecordedClaimDoesNotWait pins that a claim whose pick could
// not take the record's lock gives up at once, without a second wait on the
// same stuck lock. The move and the bind that follow skip the record too, so
// the create pays for one wait in all.
func TestReleaseOfAnUnrecordedClaimDoesNotWait(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	s.LockWait = 200 * time.Millisecond
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
	c := mustClaim(t, s, "g", time.Now().Add(time.Hour))
	start := time.Now()
	if _, err := c.Pick(context.Background(), leastClaimed("pve1")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pick err = %v; want the busy lock", err)
	}
	if err := c.Move(context.Background(), "pve2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("move after the lock wait ran out = %v; want the record reported unavailable", err)
	}
	if err := c.Bind(context.Background(), 101); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bind after the lock wait ran out = %v; want the record reported unavailable", err)
	}
	if err := c.Release(context.Background()); err != nil {
		t.Fatalf("release = %v; want nothing to remove", err)
	}
	// The pick waits once. The move, the bind, and the release must not wait
	// again, so the four together take less than two waits.
	if waited := time.Since(start); waited >= 2*s.LockWait {
		t.Fatalf("pick, move, bind, and release took %v; want one wait of %v in all", waited, s.LockWait)
	}
	if c.Node() != "" || c.Holds("pve2") {
		t.Fatalf("node %q after the release; want none", c.Node())
	}
}

// TestARecordedClaimStopsWaitingButIsStillReleased pins that a claim whose
// pick was recorded stops using the record once a later turn's lock wait
// runs out, and that its release still removes it from the record, which
// would otherwise count it until it expires.
func TestARecordedClaimStopsWaitingButIsStillReleased(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	s.LockWait = 200 * time.Millisecond
	exp := time.Now().Add(time.Hour)
	c := mustClaim(t, s, "g", exp)
	if _, err := c.Pick(context.Background(), leastClaimed("pve1")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(s.Dir.Override)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	held, err := endpointcache.LockFile(context.Background(), root, LockFileName, Name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Move(context.Background(), "pve2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("move under the busy lock = %v; want the record reported unavailable", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// The lock is free again, so a bind that tried the record would land.
	// It must not try, because the create already paid for one wait.
	if err := c.Bind(context.Background(), 101); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bind after a lock wait ran out = %v; want it not to try the record again", err)
	}
	var seen map[string]int
	sibling := func() map[string]int {
		seen = nil
		if _, err := mustClaim(t, s, "g", exp).Pick(context.Background(), func(counts map[string]int) string {
			seen = counts
			return ""
		}); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	if got := sibling(); !maps.Equal(got, map[string]int{"pve1": 1}) {
		t.Fatalf("counts before the release = %v; want the claim the pick recorded", got)
	}
	if err := c.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sibling(); len(got) != 0 {
		t.Fatalf("counts after the release = %v; want the recorded claim removed", got)
	}
}

// TestPickReportsAFailedWriteAfterCounting pins that a write that fails after
// the record was read is told apart from a record that could not be used.
func TestPickReportsAFailedWriteAfterCounting(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	// A directory in the record's place fails both the read and the rename
	// that writes the record, while the lock works.
	if err := os.Mkdir(filepath.Join(s.Dir.Override, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	c := mustClaim(t, s, "g", time.Now().Add(time.Hour))
	node, err := c.Pick(context.Background(), func(map[string]int) string {
		called = true
		return "pve2"
	})
	if node != "pve2" || !called {
		t.Fatalf("node %q called %v; want the pick made under the lock", node, called)
	}
	if !errors.Is(err, ErrNotRecorded) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v; want the failed write reported, and not an unavailable record", err)
	}
	start := time.Now()
	if err := c.Release(context.Background()); err != nil || time.Since(start) > time.Second {
		t.Fatalf("release = %v; want nothing to remove for a claim no write recorded", err)
	}
}

// TestPickExceptLeavesOutTaggedVMIDs pins that a claim whose VM the caller
// already counts from its member tag counts once, and that a claim without a
// VMID still counts.
func TestPickExceptLeavesOutTaggedVMIDs(t *testing.T) {
	t.Parallel()
	s := testStore(t, nil)
	exp := time.Now().Add(time.Hour)
	bound := mustClaim(t, s, "g", exp)
	if err := bound.Move(context.Background(), "pve1"); err != nil {
		t.Fatal(err)
	}
	if err := bound.Bind(context.Background(), 101); err != nil {
		t.Fatal(err)
	}
	if err := mustClaim(t, s, "g", exp).Move(context.Background(), "pve1"); err != nil {
		t.Fatal(err)
	}
	var seen map[string]int
	if _, err := mustClaim(t, s, "g", exp).PickExcept(context.Background(),
		func(vmid int) bool { return vmid == 101 },
		func(counts map[string]int) string { seen = counts; return "" }); err != nil {
		t.Fatal(err)
	}
	if seen["pve1"] != 1 {
		t.Fatalf("counts = %v; want only the claim whose VM is not tagged yet", seen)
	}
	// A move to another attempt's node forgets the old VMID.
	if err := bound.Move(context.Background(), "pve2"); err != nil {
		t.Fatal(err)
	}
	if bound.VMID() != 0 {
		t.Fatalf("vmid = %d after a move; want none until the next bind", bound.VMID())
	}
}

// TestClaimsOfGoneProcessesDoNotCount pins the liveness check on a host with
// a boot identity: a claim from a process of this boot and PID namespace that
// has ended stops counting, while one whose process runs, one from another
// boot, one from another PID namespace, and one without a PID all keep
// counting. A claim from another boot may come from another host that shares
// the cache directory, so it lapses only at its expiry.
func TestClaimsOfGoneProcessesDoNotCount(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(time.Hour)
	dir := endpointcache.DirOptions{Override: privateDir(t)}
	alive := func(pid int) bool { return pid != 11 }
	maker := func(pid int, boot string) *Store {
		return &Store{Dir: dir, Scope: "s", Process: func() (int, string) { return pid, boot }, Alive: alive}
	}
	for _, tc := range []struct {
		node string
		pid  int
		boot string
	}{
		{"other-boot", 12, "b0/ns1"},
		{"running", 10, "b1/ns1"},
		{"ended", 11, "b1/ns1"},
		{"other-namespace", 13, "b1/ns2"},
		{"no-pid", 0, "b1/ns1"},
		{"no-boot", 14, ""},
	} {
		if err := mustClaim(t, maker(tc.pid, tc.boot), "g", exp).Move(context.Background(), tc.node); err != nil {
			t.Fatal(err)
		}
	}
	reader := maker(99, "b1/ns1")
	var seen map[string]int
	if _, err := mustClaim(t, reader, "g", exp).Pick(context.Background(), func(c map[string]int) string {
		seen = c
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]int{"running": 1, "ended": 0, "other-boot": 1, "other-namespace": 1,
		"no-pid": 1, "no-boot": 1} {
		if seen[node] != want {
			t.Errorf("claims on %s = %d; want %d (counts %v)", node, seen[node], want, seen)
		}
	}
	// A reader without a boot identity knows nothing about any process.
	blindDir := endpointcache.DirOptions{Override: privateDir(t)}
	writer := &Store{Dir: blindDir, Scope: "s", Process: func() (int, string) { return 11, "b1/ns1" },
		Alive: func(int) bool { return true }}
	if err := mustClaim(t, writer, "g", exp).Move(context.Background(), "ended"); err != nil {
		t.Fatal(err)
	}
	blind := &Store{Dir: blindDir, Scope: "s", Process: func() (int, string) { return 98, "" },
		Alive: func(int) bool { return false }}
	if _, err := mustClaim(t, blind, "g", exp).Pick(context.Background(), func(c map[string]int) string {
		seen = c
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	if seen["ended"] != 1 {
		t.Fatalf("counts = %v; want the claim counted by a reader without a boot identity", seen)
	}
}

// TestProcessAliveSeesThisProcess pins the operating system check on this
// process, which runs.
func TestProcessAliveSeesThisProcess(t *testing.T) {
	t.Parallel()
	if !processAlive(os.Getpid()) {
		t.Fatal("this process reads as gone")
	}
}

// TestBootIdentityTurnsOffWithoutAPIDNamespace pins that a process that cannot
// read its PID namespace reports no boot identity, which turns the liveness
// check off, rather than one that another process could share by accident.
func TestBootIdentityTurnsOffWithoutAPIDNamespace(t *testing.T) {
	t.Parallel()
	boot := func(string) ([]byte, error) { return []byte("b1\n"), nil }
	ns := func(string) (string, error) { return "pid:[4026531836]", nil }
	failed := func(string) (string, error) { return "", os.ErrPermission }
	noBoot := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	for name, tc := range map[string]struct {
		readFile func(string) ([]byte, error)
		readlink func(string) (string, error)
		want     string
	}{
		"both":         {boot, ns, "b1/pid:[4026531836]"},
		"no namespace": {boot, failed, ""},
		"no boot id":   {noBoot, ns, ""},
	} {
		if got := bootIdentity(tc.readFile, tc.readlink); got != tc.want {
			t.Errorf("%s: identity = %q; want %q", name, got, tc.want)
		}
	}
}

// TestAClaimWhoseWriteFailedIsWrittenAgain pins that a claim whose write did
// not land holds nothing as far as its caller knows, and that the next Move to
// the same node, or the next Bind, writes it once the record works again. A
// Move to the node the claim already holds keeps its VMID.
func TestAClaimWhoseWriteFailedIsWrittenAgain(t *testing.T) {
	t.Parallel()
	for _, retry := range []string{"move", "bind"} {
		s := testStore(t, nil)
		blocker := filepath.Join(s.Dir.Override, FileName)
		if err := os.Mkdir(blocker, 0o700); err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(time.Hour)
		c := mustClaim(t, s, "g", exp)
		if _, err := c.Pick(context.Background(), leastClaimed("pve1")); !errors.Is(err, ErrNotRecorded) {
			t.Fatalf("%s: pick err = %v; want the failed write", retry, err)
		}
		if c.Holds("pve1") {
			t.Fatalf("%s: a claim whose write failed reads as held", retry)
		}
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
		switch retry {
		case "move":
			if err := c.Move(context.Background(), "pve1"); err != nil {
				t.Fatal(err)
			}
		case "bind":
			if err := c.Bind(context.Background(), 101); err != nil {
				t.Fatal(err)
			}
		}
		if !c.Holds("pve1") {
			t.Fatalf("%s: the claim does not read as held after its write landed", retry)
		}
		var seen map[string]int
		if _, err := mustClaim(t, s, "g", exp).Pick(context.Background(), func(counts map[string]int) string {
			seen = counts
			return ""
		}); err != nil {
			t.Fatal(err)
		}
		if seen["pve1"] != 1 {
			t.Fatalf("%s: counts = %v; want the claim a sibling can see", retry, seen)
		}
	}
	s := testStore(t, nil)
	c := mustClaim(t, s, "g", time.Now().Add(time.Hour))
	if err := c.Move(context.Background(), "pve1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Bind(context.Background(), 101); err != nil {
		t.Fatal(err)
	}
	if err := c.Move(context.Background(), "pve1"); err != nil || c.VMID() != 101 {
		t.Fatalf("vmid = %d (%v) after a move to the same node; want 101 kept", c.VMID(), err)
	}
	if _, err := mustClaim(t, s, "g", time.Now().Add(time.Hour)).PickExcept(context.Background(),
		func(vmid int) bool { return vmid == 101 }, func(counts map[string]int) string {
			if counts["pve1"] != 0 {
				t.Errorf("counts = %v; want the bound claim left out", counts)
			}
			return ""
		}); err != nil {
		t.Fatal(err)
	}
}
