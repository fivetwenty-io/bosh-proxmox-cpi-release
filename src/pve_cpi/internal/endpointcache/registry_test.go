package endpointcache

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// drainSlack is how far past its bound a drain may return before a test
// calls it late. It is generous because the race detector and a busy runner
// slow every goroutine handoff.
const drainSlack = 2 * time.Second

// drainGuard is how long a test waits for a drain that should end for a
// reason other than its bound before it fails by name. It keeps a broken rule
// from showing up as a package timeout.
const drainGuard = 5 * time.Second

// guardedDrain runs drain on its own goroutine and fails the test, naming the
// rule that what describes, if it does not return within drainGuard.
func guardedDrain(t *testing.T, what string, drain func() DrainResult) DrainResult {
	t.Helper()
	done := make(chan DrainResult, 1)
	go func() { done <- drain() }()
	select {
	case result := <-done:
		return result
	case <-time.After(drainGuard):
		t.Fatalf("%s: Drain was still waiting after %s", what, drainGuard)
		return DrainResult{}
	}
}

// drainWithin is guardedDrain for r.Drain.
func drainWithin(t *testing.T, what string, ctx context.Context, r *Registry, wait time.Duration) DrainResult {
	t.Helper()
	return guardedDrain(t, what, func() DrainResult { return r.Drain(ctx, wait) })
}

// stuckWorker is a worker that ignores cancellation until the test releases
// it, standing in for a write blocked in a system call.
type stuckWorker struct {
	ctx     chan context.Context
	release chan struct{}
	done    chan struct{}
}

// startStuck starts a stuck worker named name on r, waits until it runs, and
// releases it when the test ends so it never outlives the test.
func startStuck(t *testing.T, r *Registry, name string) *stuckWorker {
	t.Helper()
	return startStuckMode(t, r, name, config.DiscoveryModeAuto)
}

// startStuckMode is startStuck for a worker whose cluster config has the
// given discovery mode.
func startStuckMode(t *testing.T, r *Registry, name string, mode config.DiscoveryMode) *stuckWorker {
	t.Helper()
	w := &stuckWorker{ctx: make(chan context.Context, 1), release: make(chan struct{}), done: make(chan struct{})}
	err := r.Go(name, mode, func(ctx context.Context) {
		defer close(w.done)
		w.ctx <- ctx
		<-w.release
	})
	if err != nil {
		t.Fatalf("Go(%s): %v", name, err)
	}
	t.Cleanup(w.finish)
	return w
}

// workerContext returns the context the worker received.
func (w *stuckWorker) workerContext(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-w.ctx:
		w.ctx <- ctx
		return ctx
	case <-time.After(10 * time.Second):
		t.Fatal("stuck worker never started")
		return nil
	}
}

// finish releases the worker, once, and waits for it to return.
func (w *stuckWorker) finish() {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
	<-w.done
}

// timedDrain runs r.Drain under the guard and returns its result and how long
// it took.
func timedDrain(
	t *testing.T,
	what string,
	ctx context.Context,
	r *Registry,
	wait time.Duration,
) (DrainResult, time.Duration) {
	t.Helper()
	start := time.Now()
	result := drainWithin(t, what, ctx, r, wait)
	return result, time.Since(start)
}

func TestDrainWaitIsThreeSeconds(t *testing.T) {
	t.Parallel()
	if DrainWait != 3*time.Second {
		t.Fatalf("DrainWait = %s, want 3s", DrainWait)
	}
}

// A call that started no worker pays nothing: the drain starts no timer and
// returns at once whatever its bound.
func TestDrainWithNoWorkersReturnsAtOnce(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	result, took := timedDrain(t, "a drain with no workers returns at once", context.Background(), r, time.Hour)
	if took > drainSlack {
		t.Fatalf("empty drain took %s", took)
	}
	if result.Started != 0 || len(result.Cancelled) != 0 || len(result.Panics) != 0 {
		t.Fatalf("empty drain result = %+v, want nothing", result)
	}
}

// Work that finishes inside the bound runs to the end with a live context,
// and the drain returns as soon as it does rather than at the bound.
func TestDrainWaitsForWorkThatFinishes(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var sawCancel atomic.Bool
	release := make(chan struct{})
	if err := r.Go("discovery", config.DiscoveryModeAuto, func(ctx context.Context) {
		<-release
		sawCancel.Store(ctx.Err() != nil)
	}); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, func() { close(release) })

	result, took := timedDrain(t, "a drain returns once its work finishes", context.Background(), r, time.Hour)
	if took > drainSlack {
		t.Fatalf("drain took %s for work that finished after 50ms", took)
	}
	if sawCancel.Load() {
		t.Fatal("worker saw its context cancelled before it finished")
	}
	if result.Started != 1 || len(result.Cancelled) != 0 {
		t.Fatalf("result = %+v, want one started and none cancelled", result)
	}
}

// The drain stops at its bound even when the worker ignores cancellation, and
// it cancels the worker's context when it gives up.
func TestDrainStopsAtBoundAndCancels(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	stuck := startStuck(t, r, "down mark")
	ctx := stuck.workerContext(t)

	const bound = 150 * time.Millisecond
	result, took := timedDrain(t, "a drain stops at its bound", context.Background(), r, bound)
	if took < bound {
		t.Fatalf("drain returned after %s, before its %s bound", took, bound)
	}
	if took > bound+drainSlack {
		t.Fatalf("drain took %s, past its %s bound", took, bound)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("worker context after the drain: %v, want canceled", ctx.Err())
	}
	if !slices.Equal(result.Cancelled, []string{"down mark"}) || result.Started != 1 {
		t.Fatalf("result = %+v, want the down mark cancelled", result)
	}
	if result.Waited < bound {
		t.Fatalf("Waited = %s, want at least %s", result.Waited, bound)
	}
}

// A worker that honors its context returns once the drain cancels it. The
// drain must still list that worker, so the test holds the drain right after
// its cancel until the worker has returned and then gives the worker's removal
// time to run. The removal needs the registry's lock, which the drain holds
// there, so it cannot finish first. A drain that cancelled before it took the
// lock would lose the worker from the list every time.
func TestDrainCancelsCooperativeWorker(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	returned := make(chan struct{})
	got := make(chan error, 1)
	r.afterCancel = func() {
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Error("cooperative worker never saw the cancellation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := r.Go("discovery", config.DiscoveryModeAuto, func(ctx context.Context) {
		<-ctx.Done()
		got <- ctx.Err()
		close(returned)
	}); err != nil {
		t.Fatal(err)
	}
	result := drainWithin(t, "a drain stops at its bound", context.Background(), r, 20*time.Millisecond)
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want [discovery]", result.Cancelled)
	}
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("worker context error = %v, want canceled", err)
	}
}

// A zero or negative bound, and an ended context, cancel at once.
func TestDrainEndsEarly(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		wait time.Duration
	}{
		{name: "zero bound", ctx: context.Background(), wait: 0},
		{name: "negative bound", ctx: context.Background(), wait: -time.Second},
		{name: "ended context", ctx: ended, wait: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			stuck := startStuck(t, r, "discovery")
			ctx := stuck.workerContext(t)
			result, took := timedDrain(t, "a drain with "+tc.name+" cancels at once", tc.ctx, r, tc.wait)
			if took > drainSlack {
				t.Fatalf("drain took %s", took)
			}
			if ctx.Err() == nil || !slices.Equal(result.Cancelled, []string{"discovery"}) {
				t.Fatalf("worker context %v, result %+v; want the worker cancelled", ctx.Err(), result)
			}
		})
	}
}

// A signal that arrives during the wait ends it, so a signalled process
// cancels its background work and exits at once.
func TestDrainContextEndingDuringWait(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	result, took := timedDrain(t, "a drain ends when its context does", ctx, r, time.Hour)
	if took > drainSlack {
		t.Fatalf("drain took %s after its context ended", took)
	}
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want [discovery]", result.Cancelled)
	}
}

// A nil context waits for the bound alone.
func TestDrainNilContext(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	// A caller that holds no context passes a nil interface value.
	var none context.Context
	result, took := timedDrain(t, "a drain with a nil context waits for its bound", none, r, 50*time.Millisecond)
	if took < 50*time.Millisecond || took > 50*time.Millisecond+drainSlack {
		t.Fatalf("drain with a nil context took %s, want about 50ms", took)
	}
	if len(result.Cancelled) != 1 {
		t.Fatalf("Cancelled = %v, want one worker", result.Cancelled)
	}
}

// A worker offered once the drain has started is refused and never runs, both
// during the wait and after the drain returns, until Open starts a new run.
func TestGoRefusedOnceDrainStarts(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	stuck := startStuck(t, r, "discovery")
	stuck.workerContext(t)

	drained := make(chan DrainResult, 1)
	go func() { drained <- r.Drain(context.Background(), time.Hour) }()

	waitClosed(t, r)
	var ranRefused atomic.Bool
	refused := func(context.Context) { ranRefused.Store(true) }
	if err := r.Go("late mark", config.DiscoveryModeAuto, refused); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go while the drain waited: %v, want ErrRegistryClosed", err)
	}

	stuck.finish()
	var result DrainResult
	select {
	case result = <-drained:
	case <-time.After(drainGuard):
		t.Fatalf("a drain returns once its last worker does: Drain was still waiting after %s", drainGuard)
	}
	if len(result.Cancelled) != 0 {
		t.Fatalf("Cancelled = %v after every worker finished", result.Cancelled)
	}
	if err := r.Go("after drain", config.DiscoveryModeAuto, refused); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go after the drain returned: %v, want ErrRegistryClosed", err)
	}
	if ranRefused.Load() {
		t.Fatal("a refused worker ran")
	}

	r.Open()
	ran := make(chan struct{})
	if err := r.Go("next run", config.DiscoveryModeAuto, func(context.Context) { close(ran) }); err != nil {
		t.Fatalf("Go after Open: %v", err)
	}
	drainWithin(t, "a drain of the reopened run returns once its worker does", context.Background(), r, time.Hour)
	select {
	case <-ran:
	default:
		t.Fatal("the drain of the new run returned before its worker ran")
	}
}

// A drain after one that gave up waits for nothing, so two drains of one run
// never add up to more than one bound.
func TestSecondDrainWaitsForNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	drainWithin(t, "a drain stops at its bound", context.Background(), r, 20*time.Millisecond)
	result, took := timedDrain(t, "a drain after a cancelled run waits for nothing", context.Background(), r, time.Hour)
	if took > drainSlack {
		t.Fatalf("second drain took %s", took)
	}
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("second drain Cancelled = %v, want the worker still listed", result.Cancelled)
	}
}

// Drains that overlap share the first drain's bound, so a later drain with a
// longer bound still ends when the first one does.
func TestOverlappingDrainsShareBound(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	const bound = 200 * time.Millisecond
	start := time.Now()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		r.Drain(context.Background(), bound)
	}()
	waitClosed(t, r)
	drainWithin(t, "a drain shares the first drain's bound", context.Background(), r, time.Hour)
	select {
	case <-firstDone:
	case <-time.After(drainGuard):
		t.Fatalf("the first drain was still waiting after %s", drainGuard)
	}
	if took := time.Since(start); took > bound+drainSlack {
		t.Fatalf("overlapping drains took %s, past the first drain's %s bound", took, bound)
	}
}

// A drain that overlaps a longer one still ends at its own, shorter bound.
func TestOverlappingDrainKeepsItsOwnShorterBound(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	stuck := startStuck(t, r, "discovery")
	firstDone := make(chan DrainResult, 1)
	go func() { firstDone <- r.Drain(context.Background(), time.Hour) }()
	waitClosed(t, r)

	const bound = 50 * time.Millisecond
	result, took := timedDrain(t, "a drain keeps its own shorter bound", context.Background(), r, bound)
	if took < bound || took > bound+drainSlack {
		t.Fatalf("overlapping drain took %s, want about its own %s bound", took, bound)
	}
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want the stuck worker", result.Cancelled)
	}
	stuck.finish()
	select {
	case <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the first drain did not return once the worker did")
	}
}

// A drain whose context ended gave up early, and a later drain of the run
// waits for nothing, however long its own bound is. This is the last drain of
// a signalled process, which must not wait out the bound the first drain
// skipped.
func TestDrainAfterEndedContextDrainWaitsForNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	drainWithin(t, "a drain with an ended context cancels at once", ended, r, time.Hour)

	result, took := timedDrain(t, "a drain after a drain whose context ended waits for nothing",
		context.Background(), r, time.Hour)
	if took > drainSlack {
		t.Fatalf("later drain took %s after the first drain gave up", took)
	}
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("later drain Cancelled = %v, want the worker still listed", result.Cancelled)
	}
}

// Once a drain gives up, a longer drain of the same run returns too, although
// the worker still runs and its own bound has not passed.
func TestShorterDrainEndsLongerDrain(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	longDone := make(chan DrainResult, 1)
	go func() { longDone <- r.Drain(context.Background(), time.Hour) }()
	waitClosed(t, r)

	const bound = 50 * time.Millisecond
	drainWithin(t, "a drain stops at its bound", context.Background(), r, bound)
	select {
	case result := <-longDone:
		if !slices.Equal(result.Cancelled, []string{"discovery"}) {
			t.Fatalf("longer drain Cancelled = %v, want the worker", result.Cancelled)
		}
	case <-time.After(drainGuard):
		t.Fatalf("a drain keeps waiting after another drain of its run gave up: still waiting after %s", drainGuard)
	}
}

// The level of the cancelled-work line is the highest among the cancelled
// workers, each judged by its own cluster config's mode, and a worker that
// finished does not count.
func TestCancelledLevelFollowsWorkerModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		modes []config.DiscoveryMode
		done  []config.DiscoveryMode
		want  slog.Level
	}{
		{name: "no worker", want: slog.LevelDebug},
		{name: "one auto", modes: []config.DiscoveryMode{config.DiscoveryModeAuto}, want: slog.LevelDebug},
		{name: "one unset", modes: []config.DiscoveryMode{""}, want: slog.LevelDebug},
		{name: "one off", modes: []config.DiscoveryMode{config.DiscoveryModeOff}, want: slog.LevelDebug},
		{name: "one on", modes: []config.DiscoveryMode{config.DiscoveryModeOn}, want: slog.LevelWarn},
		{
			name:  "auto beside on",
			modes: []config.DiscoveryMode{config.DiscoveryModeAuto, config.DiscoveryModeOn},
			want:  slog.LevelWarn,
		},
		{
			name:  "on finished, auto cancelled",
			modes: []config.DiscoveryMode{config.DiscoveryModeAuto},
			done:  []config.DiscoveryMode{config.DiscoveryModeOn},
			want:  slog.LevelDebug,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			for _, mode := range tc.modes {
				startStuckMode(t, r, "discovery", mode)
			}
			finished := make(chan struct{}, len(tc.done))
			for _, mode := range tc.done {
				if err := r.Go("down mark", mode, func(context.Context) { finished <- struct{}{} }); err != nil {
					t.Fatal(err)
				}
				<-finished
			}
			waitRunning(t, r, len(tc.modes))
			result := drainWithin(t, "a drain stops at its bound", context.Background(), r, 20*time.Millisecond)
			if result.CancelledLevel != tc.want {
				t.Fatalf(
					"CancelledLevel = %v, want %v for %d cancelled workers",
					result.CancelledLevel,
					tc.want,
					len(result.Cancelled),
				)
			}
			if len(result.Cancelled) != len(tc.modes) {
				t.Fatalf("Cancelled = %v, want %d workers", result.Cancelled, len(tc.modes))
			}
		})
	}
}

// waitRunning waits until r's current run has exactly n workers running.
func waitRunning(t *testing.T, r *Registry, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		running := len(r.run.workers)
		r.mu.Unlock()
		if running == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run has %d workers running, want %d", running, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitClosed waits until a drain of r has started, which Go reports by
// refusing a worker.
func waitClosed(t *testing.T, r *Registry) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := r.Go("probe", config.DiscoveryModeAuto, func(context.Context) {})
		if errors.Is(err, ErrRegistryClosed) {
			return
		}
		if err != nil {
			t.Fatalf("Go: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never closed the registry")
		}
		time.Sleep(time.Millisecond)
	}
}

// Open leaves a drained run's stuck worker behind, so it cannot hold up the
// next run's drain.
func TestOpenStartsFreshRun(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	startStuck(t, r, "discovery")
	drainWithin(t, "a drain stops at its bound", context.Background(), r, 10*time.Millisecond)
	r.Open()
	result, took := timedDrain(
		t,
		"a drain of a fresh run ignores the drained run's worker",
		context.Background(),
		r,
		time.Hour,
	)
	if took > drainSlack {
		t.Fatalf("drain of the fresh run took %s", took)
	}
	if result.Started != 0 || len(result.Cancelled) != 0 {
		t.Fatalf("fresh run result = %+v, want nothing", result)
	}
}

// Open on an open registry keeps its workers, so the drain still waits for
// them.
func TestOpenOnOpenRegistryKeepsRun(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	stuck := startStuck(t, r, "discovery")
	r.Open()
	result := drainWithin(t, "a drain stops at its bound", context.Background(), r, 10*time.Millisecond)
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want the worker started before Open", result.Cancelled)
	}
	stuck.finish()
}

func TestGoRefusesBadArguments(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Go("", config.DiscoveryModeAuto, func(context.Context) {}); err == nil {
		t.Error("Go accepted an empty name")
	}
	if err := r.Go("discovery", config.DiscoveryModeAuto, nil); err == nil {
		t.Error("Go accepted a nil function")
	}
	if result := drainWithin(
		t,
		"a drain with no workers returns at once",
		context.Background(),
		r,
		time.Hour,
	); result.Started != 0 {
		t.Errorf("refused workers counted as started: %+v", result)
	}
}

// A panicking worker is recovered, reported once with its stack, and does not
// disturb the other workers.
func TestWorkerPanicIsRecoveredAndReported(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Go(
		"discovery",
		config.DiscoveryModeAuto,
		func(context.Context) { panic("cache payload defect") },
	); err != nil {
		t.Fatal(err)
	}
	var finished atomic.Bool
	if err := r.Go("down mark", config.DiscoveryModeAuto, func(context.Context) { finished.Store(true) }); err != nil {
		t.Fatal(err)
	}
	result := drainWithin(t, "a drain returns once its workers finish", context.Background(), r, time.Hour)
	if !finished.Load() {
		t.Fatal("the other worker did not finish")
	}
	if len(result.Panics) != 1 {
		t.Fatalf("Panics = %+v, want one", result.Panics)
	}
	p := result.Panics[0]
	if p.Name != "discovery" || p.Value != "cache payload defect" || len(p.Stack) == 0 {
		t.Fatalf("panic record = %q %v with %d stack bytes", p.Name, p.Value, len(p.Stack))
	}
	if again := drainWithin(
		t,
		"a second drain of a finished run returns at once",
		context.Background(),
		r,
		time.Hour,
	); len(again.Panics) != 0 {
		t.Fatalf("second drain reported the panic again: %+v", again.Panics)
	}
}

// Many goroutines register at once while a drain starts. Exactly the
// accepted workers run, each once, every refused one never runs, and the race
// detector sees no unsynchronized access.
func TestConcurrentRegistrationAndDrain(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	const goroutines, perGoroutine = 32, 50
	var acceptedIDs, ranIDs sync.Map
	var accepted atomic.Int64
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-startGate
			for i := range perGoroutine {
				id := g*perGoroutine + i
				err := r.Go("discovery", config.DiscoveryModeAuto, func(context.Context) {
					if _, again := ranIDs.LoadOrStore(id, true); again {
						t.Errorf("worker %d ran twice", id)
					}
				})
				switch {
				case err == nil:
					acceptedIDs.Store(id, true)
					accepted.Add(1)
				case !errors.Is(err, ErrRegistryClosed):
					t.Errorf("Go: %v", err)
				}
			}
		})
	}
	close(startGate)
	time.Sleep(time.Millisecond)
	result := drainWithin(
		t,
		"a drain returns once the accepted workers finish",
		context.Background(),
		r,
		30*time.Second,
	)
	wg.Wait()

	if int64(result.Started) != accepted.Load() {
		t.Fatalf("Started = %d, accepted = %d", result.Started, accepted.Load())
	}
	if len(result.Cancelled) != 0 {
		t.Fatalf("Cancelled = %d workers that finish at once", len(result.Cancelled))
	}
	// The drain waited for every accepted worker, so each has run by now.
	acceptedIDs.Range(func(id, _ any) bool {
		if _, ok := ranIDs.Load(id); !ok {
			t.Errorf("accepted worker %v never ran before the drain returned", id)
		}
		return true
	})
	ranIDs.Range(func(id, _ any) bool {
		if _, ok := acceptedIDs.Load(id); !ok {
			t.Errorf("refused worker %v ran", id)
		}
		return true
	})
}

// The process registry's functions reach one shared registry.
func TestProcessRegistry(t *testing.T) {
	Open()
	ran := make(chan struct{})
	if err := Go("discovery", config.DiscoveryModeAuto, func(context.Context) { close(ran) }); err != nil {
		t.Fatalf("Go: %v", err)
	}
	result := guardedDrain(t, "a drain returns once its worker does", func() DrainResult {
		return Drain(context.Background(), time.Hour)
	})
	<-ran
	if result.Started != 1 {
		t.Fatalf("Started = %d, want 1", result.Started)
	}
	if err := Go("late", config.DiscoveryModeAuto, func(context.Context) {}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go after Drain: %v, want ErrRegistryClosed", err)
	}
	Open()
	reopened := guardedDrain(t, "a drain of a reopened registry returns at once", func() DrainResult {
		return Drain(context.Background(), time.Hour)
	})
	if reopened.Started != 0 {
		t.Fatalf("reopened registry Started = %d, want 0", reopened.Started)
	}
}

// Workers, cancelled ones included, leave no goroutine behind once they
// return, and the drain itself starts none.
func TestRegistryLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	r := NewRegistry()
	for range 20 {
		if err := r.Go("down mark", config.DiscoveryModeAuto, func(ctx context.Context) { <-ctx.Done() }); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	for range 5 {
		if err := r.Go("stuck", config.DiscoveryModeAuto, func(context.Context) { <-release }); err != nil {
			t.Fatal(err)
		}
	}
	result := drainWithin(t, "a drain stops at its bound", context.Background(), r, 20*time.Millisecond)
	if len(result.Cancelled) == 0 {
		t.Fatal("no worker was still running at the bound")
	}
	close(release)
	r.Open()
	drainWithin(t, "a drain of a fresh run returns at once", context.Background(), r, time.Hour)

	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
