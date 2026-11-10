package endpointcache

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainSlack is how far past its bound a drain may return before a test
// calls it late. It is generous because the race detector and a busy runner
// slow every goroutine handoff.
const drainSlack = 2 * time.Second

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
	w := &stuckWorker{ctx: make(chan context.Context, 1), release: make(chan struct{}), done: make(chan struct{})}
	err := r.Go(name, func(ctx context.Context) {
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

// timedDrain runs r.Drain and returns its result and how long it took.
func timedDrain(ctx context.Context, r *Registry, wait time.Duration) (DrainResult, time.Duration) {
	start := time.Now()
	result := r.Drain(ctx, wait)
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
	result, took := timedDrain(context.Background(), r, time.Hour)
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
	if err := r.Go("discovery", func(ctx context.Context) {
		<-release
		sawCancel.Store(ctx.Err() != nil)
	}); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, func() { close(release) })

	result, took := timedDrain(context.Background(), r, time.Hour)
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
	result, took := timedDrain(context.Background(), r, bound)
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

// A worker that honors its context returns once the drain cancels it.
func TestDrainCancelsCooperativeWorker(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	got := make(chan error, 1)
	if err := r.Go("discovery", func(ctx context.Context) {
		<-ctx.Done()
		got <- ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	result := r.Drain(context.Background(), 20*time.Millisecond)
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want [discovery]", result.Cancelled)
	}
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("worker context error = %v, want canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cooperative worker never saw the cancellation")
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
			result, took := timedDrain(tc.ctx, r, tc.wait)
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
	result, took := timedDrain(ctx, r, time.Hour)
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
	result, took := timedDrain(none, r, 50*time.Millisecond)
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
	if err := r.Go("late mark", refused); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go while the drain waited: %v, want ErrRegistryClosed", err)
	}

	stuck.finish()
	result := <-drained
	if len(result.Cancelled) != 0 {
		t.Fatalf("Cancelled = %v after every worker finished", result.Cancelled)
	}
	if err := r.Go("after drain", refused); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go after the drain returned: %v, want ErrRegistryClosed", err)
	}
	if ranRefused.Load() {
		t.Fatal("a refused worker ran")
	}

	r.Open()
	ran := make(chan struct{})
	if err := r.Go("next run", func(context.Context) { close(ran) }); err != nil {
		t.Fatalf("Go after Open: %v", err)
	}
	r.Drain(context.Background(), time.Hour)
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
	r.Drain(context.Background(), 20*time.Millisecond)
	result, took := timedDrain(context.Background(), r, time.Hour)
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
	r.Drain(context.Background(), time.Hour)
	<-firstDone
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
	result, took := timedDrain(context.Background(), r, bound)
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

// waitClosed waits until a drain of r has started, which Go reports by
// refusing a worker.
func waitClosed(t *testing.T, r *Registry) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := r.Go("probe", func(context.Context) {})
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
	r.Drain(context.Background(), 10*time.Millisecond)
	r.Open()
	result, took := timedDrain(context.Background(), r, time.Hour)
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
	result := r.Drain(context.Background(), 10*time.Millisecond)
	if !slices.Equal(result.Cancelled, []string{"discovery"}) {
		t.Fatalf("Cancelled = %v, want the worker started before Open", result.Cancelled)
	}
	stuck.finish()
}

func TestGoRefusesBadArguments(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Go("", func(context.Context) {}); err == nil {
		t.Error("Go accepted an empty name")
	}
	if err := r.Go("discovery", nil); err == nil {
		t.Error("Go accepted a nil function")
	}
	if result := r.Drain(context.Background(), time.Hour); result.Started != 0 {
		t.Errorf("refused workers counted as started: %+v", result)
	}
}

// A panicking worker is recovered, reported once with its stack, and does not
// disturb the other workers.
func TestWorkerPanicIsRecoveredAndReported(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Go("discovery", func(context.Context) { panic("cache payload defect") }); err != nil {
		t.Fatal(err)
	}
	var finished atomic.Bool
	if err := r.Go("down mark", func(context.Context) { finished.Store(true) }); err != nil {
		t.Fatal(err)
	}
	result := r.Drain(context.Background(), time.Hour)
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
	if again := r.Drain(context.Background(), time.Hour); len(again.Panics) != 0 {
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
				err := r.Go("discovery", func(context.Context) {
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
	result := r.Drain(context.Background(), 30*time.Second)
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
	if err := Go("discovery", func(context.Context) { close(ran) }); err != nil {
		t.Fatalf("Go: %v", err)
	}
	result := Drain(context.Background(), time.Hour)
	<-ran
	if result.Started != 1 {
		t.Fatalf("Started = %d, want 1", result.Started)
	}
	if err := Go("late", func(context.Context) {}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("Go after Drain: %v, want ErrRegistryClosed", err)
	}
	Open()
	if result := Drain(context.Background(), time.Hour); result.Started != 0 {
		t.Fatalf("reopened registry Started = %d, want 0", result.Started)
	}
}

// Workers, cancelled ones included, leave no goroutine behind once they
// return, and the drain itself starts none.
func TestRegistryLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	r := NewRegistry()
	for range 20 {
		if err := r.Go("down mark", func(ctx context.Context) { <-ctx.Done() }); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	for range 5 {
		if err := r.Go("stuck", func(context.Context) { <-release }); err != nil {
			t.Fatal(err)
		}
	}
	result := r.Drain(context.Background(), 20*time.Millisecond)
	if len(result.Cancelled) == 0 {
		t.Fatal("no worker was still running at the bound")
	}
	close(release)
	r.Open()
	r.Drain(context.Background(), time.Hour)

	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
