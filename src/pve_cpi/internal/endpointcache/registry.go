package endpointcache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// DrainWait bounds how long the exit drain waits for background workers
// before it cancels the ones still running. A call waits that long only when
// it started a discovery refresh or wrote a mark or a hint just before it
// ended, and a call that started no worker does not wait at all.
const DrainWait = 3 * time.Second

// ErrRegistryClosed means a worker was refused because the registry's drain
// has started, so nothing would wait for the worker or cancel it before the
// process exits. The caller skips the work, and the cache misses one write.
var ErrRegistryClosed = errors.New("endpoint cache: background work refused because the exit drain has started")

// processRegistry holds the background workers of this process. The registry
// is process-wide because the client factory's signature is shared with the
// tests, so the pool cannot be handed a registry, and the CPI runs one call
// per process.
var processRegistry = NewRegistry()

// Go starts fn as a background worker in the process registry, as
// Registry.Go describes.
func Go(name string, mode config.DiscoveryMode, fn func(ctx context.Context)) error {
	return processRegistry.Go(name, mode, fn)
}

// Drain waits for the process registry's workers and cancels the rest, as
// Registry.Drain describes.
func Drain(ctx context.Context, wait time.Duration) DrainResult {
	return processRegistry.Drain(ctx, wait)
}

// Open reopens the process registry after a drain, as Registry.Open
// describes.
func Open() { processRegistry.Open() }

// Registry tracks background workers, such as a discovery refresh, a down-mark
// write, or a cold-start hint write, so the process can wait for them briefly
// before it exits and cancel the ones that run too long. A Registry is open
// when it is created. Its first Drain closes it, and from then on Go refuses
// every worker until Open starts the next run, so a worker never runs without
// a drain that waits for it or cancels it. A Registry is safe for concurrent
// use, and starting a worker costs one goroutine and no timer.
type Registry struct {
	mu  sync.Mutex
	run *workerRun
}

// workerRun is the state of one run of a Registry, from Open to the end of its
// drain. A worker belongs to the run it registered in, so a worker still
// running after its run was drained cannot hold up or join a later run.
type workerRun struct {
	// ctx is every worker's context. A drain cancels it when it gives up, and
	// a drain that finds it cancelled, or sees it cancelled while it waits,
	// returns at once. So drains of one run never add up to more than one
	// bound, and a later drain, such as the last one after a signal ended the
	// first, never waits again.
	ctx    context.Context
	cancel context.CancelFunc
	// closed is set when the first drain starts, and Go refuses workers
	// from then on.
	closed bool
	// idle is closed once the run is closed and has no running worker.
	idle    chan struct{}
	workers map[uint64]worker
	nextID  uint64
	started int
	panics  []WorkerPanic
}

// worker is a running worker as its run records it.
type worker struct {
	name string
	// mode is the configured discovery mode of the cluster config that
	// started the worker, which decides the level of its cancellation.
	mode config.DiscoveryMode
}

// WorkerPanic records a worker that panicked. The registry recovers the panic,
// so a defect in background work never ends the process or changes its exit
// code, and the drain hands the record to the caller to log.
type WorkerPanic struct {
	// Name is the name the worker was started with.
	Name string
	// Value is the value the worker panicked with.
	Value any
	// Stack is the worker's stack at the panic.
	Stack []byte
}

// DrainResult reports what a drain found.
type DrainResult struct {
	// Started counts the workers the run accepted, including ones that had
	// finished before the drain began. Zero means the call started no
	// background work and the drain did not wait.
	Started int
	// Waited is how long the drain took.
	Waited time.Duration
	// Cancelled names the workers still running when the drain gave up, in
	// sorted order. Each was cancelled and is expected to write nothing, and
	// the drain did not wait for it to return.
	Cancelled []string
	// CancelledLevel is the level for the line that reports Cancelled: the
	// highest Level among the cancelled workers, each judged by the
	// configured mode of the cluster config that started it. It is Debug when
	// no worker was cancelled.
	CancelledLevel slog.Level
	// Panics holds the workers that panicked and that no earlier drain
	// reported.
	Panics []WorkerPanic
}

// NewRegistry returns an open Registry with no workers.
func NewRegistry() *Registry {
	return &Registry{run: newWorkerRun()}
}

// newWorkerRun returns an open run with no workers.
func newWorkerRun() *workerRun {
	ctx, cancel := context.WithCancel(context.Background())
	return &workerRun{ctx: ctx, cancel: cancel, idle: make(chan struct{}), workers: map[uint64]worker{}}
}

// Open starts a new run when the current one has been drained, so a process
// that serves more than one call, such as a test binary, gets an open
// registry for each call. It has no effect on an open registry. A worker left
// running by the drained run stays in that run, which is already cancelled.
// Every Open must be followed by a Drain, because the workers of the new run
// are waited for or cancelled only by a drain.
func (r *Registry) Open() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run.closed {
		r.run = newWorkerRun()
	}
}

// Go starts fn on its own goroutine as a worker of the current run. name
// identifies the worker in the drain's result, for example "discovery" or
// "down mark", and must not be empty. mode is the configured discovery mode
// of the cluster config the worker serves, from CPIConfig.DiscoveryModeValue
// and not the effective mode, and it sets the level at which the drain
// reports the worker if it has to cancel it.
//
// fn receives the run's context, which the drain cancels when its wait runs
// out. fn must pass that context to every lock wait and request it makes. A
// write through Update after the cancel is refused by the store, and a write
// already under way finishes through the temp-file-and-rename path or not at
// all, so the cache never holds a torn file. A worker that ignores
// cancellation does not hold up the drain, and the process exit ends it.
//
// A panic in fn is recovered and reported by the next drain. Go returns
// ErrRegistryClosed, without running fn, once the run's drain has started.
// It also refuses an empty name or a nil fn.
func (r *Registry) Go(name string, mode config.DiscoveryMode, fn func(ctx context.Context)) error {
	switch {
	case name == "":
		return fmt.Errorf("%s: background worker needs a name", cacheLabel)
	case fn == nil:
		return fmt.Errorf("%s: background worker %q has no function", cacheLabel, name)
	}
	r.mu.Lock()
	run := r.run
	if run.closed {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrRegistryClosed, name)
	}
	id := run.nextID
	run.nextID++
	run.workers[id] = worker{name: name, mode: mode}
	run.started++
	r.mu.Unlock()

	go r.work(run, id, name, fn)
	return nil
}

// work runs one worker, recovers a panic from it, and removes it from its run.
func (r *Registry) work(run *workerRun, id uint64, name string, fn func(ctx context.Context)) {
	defer func() {
		recovered := recover()
		var stack []byte
		if recovered != nil {
			stack = debug.Stack()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if recovered != nil {
			run.panics = append(run.panics, WorkerPanic{Name: name, Value: recovered, Stack: stack})
		}
		delete(run.workers, id)
		if run.closed && len(run.workers) == 0 {
			close(run.idle)
		}
	}()
	fn(run.ctx)
}

// Drain closes the current run, waits for its workers to return, and then
// cancels the run's context. The wait ends when every worker has returned,
// when wait has passed since this drain started, when any other drain of the
// run gives up, or when ctx ends, whichever comes first, so a drain never
// blocks past its bound, even for a worker that ignores cancellation. A drain
// with no running worker returns at once. A nil ctx waits for wait alone, and
// a wait of zero or less cancels at once.
//
// The first drain closes the run, and Go refuses workers from then on. Once
// any drain of the run gives up, every drain of the run returns, so a longer
// drain never outlasts a shorter one that overlaps it, and a drain after the
// run was cancelled waits for nothing. Two drains of one run therefore never
// add up to more than one bound. A drain never changes anything about the
// call's result, and the caller decides how to log what it reports.
func (r *Registry) Drain(ctx context.Context, wait time.Duration) DrainResult {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	wait = max(wait, 0)

	r.mu.Lock()
	run := r.run
	if !run.closed {
		run.closed = true
		if len(run.workers) == 0 {
			close(run.idle)
		}
	}
	r.mu.Unlock()

	awaitIdle(ctx, run.ctx, run.idle, wait)
	run.cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	result := DrainResult{Started: run.started, Waited: time.Since(start), Panics: run.panics}
	run.panics = nil
	result.CancelledLevel = Level(config.DiscoveryModeAuto)
	for _, w := range run.workers {
		result.Cancelled = append(result.Cancelled, w.name)
		result.CancelledLevel = max(result.CancelledLevel, Level(w.mode))
	}
	slices.Sort(result.Cancelled)
	return result
}

// awaitIdle waits for idle to close, for at most limit, or until ctx or runCtx
// ends. runCtx is the run's context, which another drain cancels when it
// gives up. It starts no timer when idle is already closed or limit has
// passed.
func awaitIdle(ctx, runCtx context.Context, idle <-chan struct{}, limit time.Duration) {
	select {
	case <-idle:
		return
	default:
	}
	if limit <= 0 || ctx.Err() != nil || runCtx.Err() != nil {
		return
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	case <-ctx.Done():
	case <-runCtx.Done():
	}
}
