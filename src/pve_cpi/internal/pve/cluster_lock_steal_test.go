package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// stealWho names the acquirer a pool call belongs to.
type stealWho struct{}

func whoOf(ctx context.Context) string {
	who, _ := ctx.Value(stealWho{}).(string)
	return who
}

// stealPools is a sentinel store for racing acquirers. Before every call it
// runs hook with the caller's name and the operation, outside the store's
// lock, so a test can hold one acquirer at an exact point while another runs.
type stealPools struct {
	mu    sync.Mutex
	pools map[string]string
	log   []string
	hook  func(who, op string)
}

func (p *stealPools) AddVM(context.Context, string, int64) error             { return nil }
func (p *stealPools) MoveVMToPool(context.Context, string, int64) error      { return nil }
func (p *stealPools) PoolHasVM(context.Context, string, int64) (bool, error) { return false, nil }

func (p *stealPools) record(ctx context.Context, op string) {
	if p.hook != nil {
		p.hook(whoOf(ctx), op)
	}
}

func (p *stealPools) CreatePool(ctx context.Context, id, claim string) error {
	p.record(ctx, "create")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, whoOf(ctx)+" create")
	if _, taken := p.pools[id]; taken {
		return fmt.Errorf("pool '%s' already exists", id)
	}
	p.pools[id] = claim
	return nil
}

func (p *stealPools) DeletePool(ctx context.Context, id string) error {
	p.record(ctx, "delete")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log = append(p.log, whoOf(ctx)+" delete "+p.pools[id])
	if _, taken := p.pools[id]; !taken {
		return fmt.Errorf("pool '%s' does not exist", id)
	}
	delete(p.pools, id)
	return nil
}

func (p *stealPools) GetPoolComment(ctx context.Context, id string) (string, bool, error) {
	p.record(ctx, "get")
	p.mu.Lock()
	defer p.mu.Unlock()
	claim, found := p.pools[id]
	return claim, found, nil
}

func (p *stealPools) claim(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pools[id]
}

// sharedClock is one fake wall clock for several acquirers. A sleep advances it
// by the duration slept, after running onSleep, so a test can act at the moment
// an acquirer starts a particular pause.
type sharedClock struct {
	mu      sync.Mutex
	t       time.Time
	onSleep func(who string, d time.Duration)
}

func (c *sharedClock) lockClock() lockClock {
	return lockClock{
		now: func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.t
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			if c.onSleep != nil {
				c.onSleep(whoOf(ctx), d)
			}
			c.mu.Lock()
			c.t = c.t.Add(d)
			c.mu.Unlock()
			return nil
		},
	}
}

const stealPool = "bosh-lock-vm-90000"

type acquired struct {
	handle *ClusterLockHandle
	err    error
}

// acquireAs takes the parker-shaped lock on p as who, with the grace pause.
func acquireAs(p *stealPools, clk lockClock, who, owner string, timeout time.Duration) acquired {
	ctx := context.WithValue(context.Background(), stealWho{}, who)
	h, err := acquireClusterLockWithClock(ctx, p, "vm-90000", owner, 3*time.Minute, timeout, clk, WithCreateGrace())
	return acquired{h, err}
}

// TestSteal_ChangedClaimAbandonsTheSteal is the two-stealer race.
// A holder crashed, and stealers S1 and S3 both judged its claim expired. S3
// stalls after that judgement while S1 steals and confirms. When S3 resumes,
// its re-read immediately before the delete finds S1's claim instead of the
// one it judged, so it abandons the steal rather than deleting S1's live claim.
func TestSteal_ChangedClaimAbandonsTheSteal(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	crashed := encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	clock := &sharedClock{t: start}
	stalled, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gets := 0
	p := &stealPools{pools: map[string]string{stealPool: crashed}}
	p.hook = func(who, op string) {
		if who != "S3" {
			return
		}
		if op == "get" {
			gets++
			if gets == 1 {
				return
			}
		}
		if gets >= 1 {
			// S3's first call after the read that judged the claim expired.
			once.Do(func() {
				close(stalled)
				<-resume
			})
		}
	}

	s3 := make(chan acquired, 1)
	go func() { s3 <- acquireAs(p, clock.lockClock(), "S3", "S3@h/3-c-1", 5*time.Second) }()
	<-stalled
	s1 := acquireAs(p, clock.lockClock(), "S1", "S1@h/2-b-1", 5*time.Second)
	close(resume)
	r3 := <-s3

	if s1.handle == nil {
		t.Fatalf("S1 did not take the lock: %v", s1.err)
	}
	if r3.handle != nil {
		t.Fatalf("both stealers hold the lock; log:\n%s", strings.Join(p.log, "\n"))
	}
	if !errors.Is(r3.err, ErrClusterLockTimeout) {
		t.Fatalf("S3 should have waited behind S1, got %v", r3.err)
	}
	if got := p.claim(stealPool); !strings.Contains(got, "owner=S1@h/2-b-1 ") {
		t.Fatalf("S1's claim was removed: sentinel holds %q; log:\n%s", got, strings.Join(p.log, "\n"))
	}
}

// TestSteal_GraceCatchesAStaleDelete covers the case the re-read cannot. S3's
// re-read still saw the expired claim, and its delete is in flight when S1
// steals and confirms its claim once. S3's delete lands during S1's grace
// pause and removes S1's sentinel, so S1's second confirmation finds its claim
// gone and S1 must not take the lock. Only one of them may end up holding it.
func TestSteal_GraceCatchesAStaleDelete(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	crashed := encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))
	clock := &sharedClock{t: start}
	inDelete, release, landed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var holdOnce, releaseOnce, landOnce sync.Once
	releaseS3 := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	s3Released := false
	p := &stealPools{pools: map[string]string{stealPool: crashed}}
	p.hook = func(who, op string) {
		if who != "S3" {
			return
		}
		if op == "delete" {
			holdOnce.Do(func() {
				close(inDelete)
				<-release
				mu.Lock()
				s3Released = true
				mu.Unlock()
			})
			return
		}
		mu.Lock()
		done := s3Released
		mu.Unlock()
		if done {
			// This is S3's first call after its delete, so the stale delete
			// has landed.
			landOnce.Do(func() { close(landed) })
		}
	}
	clock.onSleep = func(who string, d time.Duration) {
		if who == "S1" && d == clusterLockGrace() {
			// S1 confirmed its claim once and is pausing before the second
			// confirmation. Let S3's stale delete land inside that pause.
			releaseS3()
			<-landed
		}
	}

	s3 := make(chan acquired, 1)
	go func() {
		r := acquireAs(p, clock.lockClock(), "S3", "S3@h/3-c-1", 3*time.Second)
		landOnce.Do(func() { close(landed) })
		s3 <- r
	}()
	<-inDelete
	r1 := acquireAs(p, clock.lockClock(), "S1", "S1@h/2-b-1", 3*time.Second)
	releaseS3()
	r3 := <-s3

	if r1.handle != nil && r3.handle != nil {
		t.Fatalf("both acquirers hold the lock; log:\n%s", strings.Join(p.log, "\n"))
	}
	if r1.handle != nil {
		t.Fatalf("S1 took the lock although S3's delete removed its claim during the grace; log:\n%s", strings.Join(p.log, "\n"))
	}
	if r3.handle == nil || !strings.Contains(p.claim(stealPool), "owner=S3@h/3-c-1 ") {
		t.Fatalf("S3 should hold the lock it recreated: handle=%v sentinel=%q err=%v", r3.handle != nil, p.claim(stealPool), r3.err)
	}
}

// TestSteal_OverBudgetDeleteAbandonsTheCreate covers the steal budget. The
// stealer's delete takes longer than clusterLockStealBudget, so its re-read is
// older than another acquirer's grace would cover. It must not create as part
// of that steal. It waits a poll and starts over with an ordinary create.
func TestSteal_OverBudgetDeleteAbandonsTheCreate(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	clock := &sharedClock{t: start}
	var events []string
	var mu sync.Mutex
	note := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	p := &stealPools{pools: map[string]string{stealPool: encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))}}
	p.hook = func(_, op string) {
		note(op)
		if op == "delete" {
			clock.mu.Lock()
			clock.t = clock.t.Add(clusterLockStealBudget + time.Second)
			clock.mu.Unlock()
		}
	}
	clock.onSleep = func(_ string, d time.Duration) { note(fmt.Sprintf("sleep %s", d)) }

	r := acquireAs(p, clock.lockClock(), "S", "S@h/1-d-1", 10*time.Second)
	if r.handle == nil {
		t.Fatalf("the acquirer never took the lock: %v", r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, e := range events {
		if e != "delete" {
			continue
		}
		if i+1 >= len(events) || !strings.HasPrefix(events[i+1], "sleep") {
			t.Fatalf("an over-budget steal went on to create; events %v", events)
		}
		return
	}
	t.Fatalf("the steal never deleted; events %v", events)
}

// TestSteal_SlowReReadAbandonsTheSteal covers where the steal budget starts.
// The re-read that confirms the expired claim is answered only after more than
// clusterLockStealBudget has passed since it was sent. Its answer may predate
// another acquirer's confirmed create by more than the grace covers, so the
// steal must not delete on it. The budget has to run from before the re-read
// went out; measured from its return, the slow answer would look fresh. The
// acquirer waits a poll and takes the lock on a later, prompt attempt.
func TestSteal_SlowReReadAbandonsTheSteal(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	start := time.Unix(10_000, 0)
	clock := &sharedClock{t: start}
	var mu sync.Mutex
	var events []string
	gets := 0
	p := &stealPools{pools: map[string]string{stealPool: encodeLockComment("crashed@h/1-a-1", start.Add(-time.Second))}}
	p.hook = func(_, op string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, op)
		if op != "get" {
			return
		}
		gets++
		if gets == 2 {
			// The second read of the attempt is the re-read before the delete.
			// Its answer arrives after the whole budget has gone by.
			clock.mu.Lock()
			clock.t = clock.t.Add(clusterLockStealBudget + time.Second)
			clock.mu.Unlock()
		}
	}
	clock.onSleep = func(_ string, d time.Duration) {
		mu.Lock()
		events = append(events, "sleep")
		mu.Unlock()
	}

	r := acquireAs(p, clock.lockClock(), "S", "S@h/1-d-1", 30*time.Second)
	if r.handle == nil {
		t.Fatalf("the acquirer never took the lock: %v", r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e == "delete" {
			t.Fatalf("the steal deleted on a re-read answered after the budget; events %v", events)
		}
		if e == "sleep" {
			return
		}
	}
	t.Fatalf("the acquirer never waited a poll; events %v", events)
}

// confirmReads scripts the reads of one sentinel for the confirm tests. Every
// read of a sentinel that does not exist yet answers normally. Once the
// sentinel exists, fail says whether the next read fails; a read that does
// not fail answers with what the store holds.
func confirmReads(f *fakeLockPools, fail func(read int) bool) *int {
	reads := 0
	f.getFn = func(poolID string) (string, bool, error, bool) {
		if _, exists := f.pools[poolID]; !exists {
			return "", false, nil, false
		}
		reads++
		if fail(reads) {
			return "", false, errors.New("503 pmxcfs read timeout"), true
		}
		return "", false, nil, false
	}
	return &reads
}

// failsUntilTheWayOut fails every confirming read, including the last one,
// which is made once the clock has reached deadline. Only the read after it,
// the one abandonLockCreate makes on the way out, answers.
func failsUntilTheWayOut(clk lockClock, deadline time.Time) func(int) bool {
	atDeadline := 0
	return func(int) bool {
		if clk.now().Before(deadline) {
			return true
		}
		atDeadline++
		return atDeadline == 1
	}
}

// sleepLog records the pauses a fixed test clock is asked for.
func sleepLog(start time.Time) (lockClock, *[]time.Duration) {
	cur := start
	var slept []time.Duration
	return lockClock{
		now: func() time.Time { return cur },
		sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			cur = cur.Add(d)
			return nil
		},
	}, &slept
}

// TestConfirm_ReadRetriesUntilItAnswers covers a transient failure of the read
// that confirms a fresh create. The pass reads again on the poll cadence and
// takes the lock once a read answers, instead of failing at once and leaving
// its own sentinel standing for a whole TTL.
func TestConfirm_ReadRetriesUntilItAnswers(t *testing.T) {
	f := newFakeLockPools()
	reads := confirmReads(f, func(read int) bool { return read <= 2 })
	clk, slept := sleepLog(time.Unix(1000, 0))
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 30*time.Second, clk)
	if err != nil || h == nil {
		t.Fatalf("a transient read failure lost the lock: %v", err)
	}
	if *reads != 3 || len(*slept) != 2 {
		t.Fatalf("want two failed reads and two poll waits before the answer, got reads=%d waits=%v", *reads, *slept)
	}
	for _, d := range *slept {
		if d < clusterLockPollInterval || d >= 2*clusterLockPollInterval {
			t.Fatalf("a retry waited %v, off the poll cadence", d)
		}
	}
	if f.deleteN != 0 || !strings.Contains(f.pools["bosh-lock-web"], "owner=me ") {
		t.Fatalf("the acquired sentinel was disturbed: deletes=%d sentinel=%q", f.deleteN, f.pools["bosh-lock-web"])
	}
}

// TestConfirm_SecondPassRetriesThatPass covers the pass shape for a lock that
// takes the grace. The first pass confirms the claim, the second pass's reads
// fail, and a retry of that same pass answers. The acquire takes the lock
// without starting over and without a second grace pause, and the handle
// carries the claim that answering read returned.
func TestConfirm_SecondPassRetriesThatPass(t *testing.T) {
	defer SetClusterLockGraceForTest(clusterLockDefaultGrace)()
	f := newFakeLockPools()
	reads := 0
	f.getFn = func(poolID string) (string, bool, error, bool) {
		stored, exists := f.pools[poolID]
		if !exists {
			return "", false, nil, false
		}
		reads++
		switch reads {
		case 1:
			return "", false, nil, false
		case 2, 3:
			return "", false, errors.New("503 pmxcfs read timeout"), true
		}
		return stored + "\n", true, nil, true
	}
	clk, slept := sleepLog(time.Unix(1000, 0))
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 30*time.Second, clk, WithCreateGrace())
	if err != nil || h == nil {
		t.Fatalf("the second pass's transient failure lost the lock: %v", err)
	}
	if f.createN != 1 || reads != 4 {
		t.Fatalf("want one create and four reads, got creates=%d reads=%d", f.createN, reads)
	}
	graces := 0
	for _, d := range *slept {
		if d == clusterLockDefaultGrace {
			graces++
		}
	}
	if graces != 1 || len(*slept) != 3 {
		t.Fatalf("want one grace pause and two poll waits, got %v", *slept)
	}
	if want := f.pools["bosh-lock-web"] + "\n"; h.claim != want {
		t.Fatalf("handle claim = %q, want the claim the answering read returned %q", h.claim, want)
	}
}

// TestConfirm_UnknownStateAtTheDeadline covers reads that never answer. The
// acquire cannot tell whether it holds the lock, so it returns
// ErrClusterLockStateUnknown with no handle, retriable, once its own deadline
// passes. No read proved the sentinel ours, so it is left standing.
func TestConfirm_UnknownStateAtTheDeadline(t *testing.T) {
	f := newFakeLockPools()
	confirmReads(f, func(int) bool { return true })
	clk, _ := sleepLog(time.Unix(1000, 0))
	start := clk.now()
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 3*time.Second, clk)
	if h != nil || !errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want no handle and ErrClusterLockStateUnknown, got handle=%v err=%v", h != nil, err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || errors.Is(err, ErrClusterLockTimeout) {
		t.Fatalf("the unknown state must be retriable and distinct from a timeout: %v", err)
	}
	if got := clk.now().Sub(start); got != 3*time.Second {
		t.Fatalf("the retries ran %v, want exactly the acquire's 3s wait", got)
	}
	if f.deleteN != 0 {
		t.Fatalf("a sentinel no read could attribute to us was deleted")
	}
}

// TestConfirm_AbandonDeletesAProvenOwnSentinel covers the way out. The
// confirming reads fail until the deadline, and the read on the way out
// answers with our claim, so the acquire deletes its own sentinel rather than
// block every other acquirer for a whole TTL. The delete carries the comment
// that read returned, which here differs from the one we sent, so a guarded
// delete compares two reads.
func TestConfirm_AbandonDeletesAProvenOwnSentinel(t *testing.T) {
	f := newFakeLockPools()
	f.normalize = func(comment string) string { return comment + "\n" }
	clk, _ := sleepLog(time.Unix(1000, 0))
	confirmReads(f, failsUntilTheWayOut(clk, clk.now().Add(3*time.Second)))
	f.deleteHook = func(ctx context.Context, _, stored string) {
		if expected, ok := ExpectedLockClaim(ctx); !ok || expected != stored {
			t.Errorf("the delete expected claim %q (set=%v), but the sentinel holds %q", expected, ok, stored)
		}
	}
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 3*time.Second, clk)
	if h != nil || !errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want no handle and ErrClusterLockStateUnknown, got handle=%v err=%v", h != nil, err)
	}
	if f.deleteN != 1 {
		t.Fatalf("want one delete of our proven sentinel, got %d", f.deleteN)
	}
	if left, ok := f.pools["bosh-lock-web"]; ok {
		t.Fatalf("our own proven sentinel was left behind: %q", left)
	}
}

// TestConfirm_AbandonLeavesAClaimItCannotProve covers the way out when the
// read does not prove the sentinel ours. Another owner's claim, a read that
// still fails, and a claim of ours too close to its expiry are all left
// standing, and no delete is attempted.
func TestConfirm_AbandonLeavesAClaimItCannotProve(t *testing.T) {
	other := encodeLockComment("S1@h/2-b-1", time.Unix(99_999, 0))
	for name, atDeadline := range map[string]func(f *fakeLockPools, id string) (string, bool, error, bool){
		"another owner's claim": func(f *fakeLockPools, id string) (string, bool, error, bool) {
			f.pools[id] = other
			return "", false, nil, false
		},
		"the read still fails": func(*fakeLockPools, string) (string, bool, error, bool) {
			return "", false, errors.New("503 pmxcfs read timeout"), true
		},
		"our claim about to expire": func(*fakeLockPools, string) (string, bool, error, bool) {
			return encodeLockComment("me", time.Unix(1004, 0)), true, nil, true
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeLockPools()
			clk, _ := sleepLog(time.Unix(1000, 0))
			failing := failsUntilTheWayOut(clk, clk.now().Add(3*time.Second))
			f.getFn = func(id string) (string, bool, error, bool) {
				if _, exists := f.pools[id]; !exists {
					return "", false, nil, false
				}
				if failing(0) {
					return "", false, errors.New("503 pmxcfs read timeout"), true
				}
				return atDeadline(f, id)
			}
			before := ""
			f.deleteHook = func(_ context.Context, _, stored string) { before = stored }
			h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", time.Minute, 3*time.Second, clk)
			if h != nil || !errors.Is(err, ErrClusterLockStateUnknown) {
				t.Fatalf("want no handle and ErrClusterLockStateUnknown, got handle=%v err=%v", h != nil, err)
			}
			if f.deleteN != 0 {
				t.Fatalf("a delete was attempted on a sentinel holding %q", before)
			}
			if _, ok := f.pools["bosh-lock-web"]; !ok {
				t.Fatal("the sentinel was removed")
			}
			if name == "another owner's claim" && f.pools["bosh-lock-web"] != other {
				t.Fatalf("another owner's claim was disturbed: %q", f.pools["bosh-lock-web"])
			}
		})
	}
}

// TestConfirm_UnknownStateDoesNotRunTheWindow covers the parker window. Every
// read after the create fails, so the acquire cannot tell whether it holds the
// lock. The window must not run unserialized next to a sentinel that may be
// ours, and the call fails with the unknown state instead.
func TestConfirm_UnknownStateDoesNotRunTheWindow(t *testing.T) {
	f := newFakeLockPools()
	confirmReads(f, func(int) bool { return true })
	ran := false
	ctx := withTestParkerLockTimeouts(context.Background(), 90*time.Second, time.Millisecond)
	err := withParkerProtectionLock(ctx, &parkerLockClient{pools: f}, nil, 90000, "unpark", func(context.Context) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("the window ran unserialized next to a sentinel that may be ours")
	}
	if !errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want ErrClusterLockStateUnknown, got %v", err)
	}
}
