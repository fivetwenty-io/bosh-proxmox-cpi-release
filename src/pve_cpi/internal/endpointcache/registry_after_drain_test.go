package endpointcache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A function registered with AfterDrain runs once the drain has ended, after a
// worker the drain waited for has returned.
func TestAfterDrainRunsAfterTheWorkersReturn(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var workerDone, releasedAfterWorker atomic.Bool
	if !r.AfterDrain(func() { releasedAfterWorker.Store(workerDone.Load()) }) {
		t.Fatal("AfterDrain on an open registry = false, want true")
	}
	if err := r.Go("work", "", func(context.Context) {
		time.Sleep(20 * time.Millisecond)
		workerDone.Store(true)
	}); err != nil {
		t.Fatalf("Go: %v", err)
	}
	r.Drain(context.Background(), time.Second)
	if !releasedAfterWorker.Load() {
		t.Error("the release ran before the worker returned, or did not run")
	}
}

// A drain that gives up on a worker still runs the release, and the release
// runs once however many drains the run sees.
func TestAfterDrainRunsOnceWhenTheDrainGivesUp(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var runs atomic.Int32
	r.AfterDrain(func() { runs.Add(1) })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	if err := r.Go("stuck", "", func(context.Context) { <-block }); err != nil {
		t.Fatalf("Go: %v", err)
	}
	first := r.Drain(context.Background(), 10*time.Millisecond)
	if len(first.Cancelled) != 1 {
		t.Fatalf("Cancelled = %v, want the stuck worker", first.Cancelled)
	}
	r.Drain(context.Background(), 10*time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Errorf("release ran %d times, want 1", got)
	}
}

// Once a drain has closed the run, AfterDrain refuses the function and never
// runs it, and a nil function is refused too.
func TestAfterDrainRefusesAClosedRunAndNil(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if r.AfterDrain(nil) {
		t.Error("AfterDrain(nil) = true, want false")
	}
	r.Drain(context.Background(), 0)
	var ran atomic.Bool
	if r.AfterDrain(func() { ran.Store(true) }) {
		t.Error("AfterDrain on a drained run = true, want false")
	}
	r.Drain(context.Background(), 0)
	if ran.Load() {
		t.Error("a refused function ran")
	}
	r.Open()
	if !r.AfterDrain(func() {}) {
		t.Error("AfterDrain after Open = false, want true")
	}
}

// A panic in one release is reported with the worker panics and does not stop
// the releases after it.
func TestAfterDrainRecoversAPanicAndRunsTheRest(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	var second atomic.Bool
	r.AfterDrain(func() { panic("release broke") })
	r.AfterDrain(func() { second.Store(true) })
	result := r.Drain(context.Background(), 0)
	if !second.Load() {
		t.Error("the release after the panicking one did not run")
	}
	if len(result.Panics) != 1 || result.Panics[0].Name != releaseName || result.Panics[0].Value != "release broke" {
		t.Errorf("Panics = %+v, want the release panic named %q", result.Panics, releaseName)
	}
}

// The process-wide AfterDrain uses the process registry.
func TestProcessAfterDrainUsesTheProcessRegistry(t *testing.T) {
	var ran atomic.Bool
	Open()
	if !AfterDrain(func() { ran.Store(true) }) {
		t.Fatal("AfterDrain = false on an open process registry")
	}
	Drain(context.Background(), 0)
	Open()
	if !ran.Load() {
		t.Error("the process drain did not run the release")
	}
}
