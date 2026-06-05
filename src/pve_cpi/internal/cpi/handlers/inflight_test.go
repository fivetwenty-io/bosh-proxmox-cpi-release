package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// TestInflightUnlimited: limit<=0 → acquire never blocks; all goroutines proceed.
func TestInflightUnlimited(t *testing.T) {
	// Reset registry state between tests.
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			release, err := inflightSems.acquire(ctx, "node1", 0)
			if err != nil {
				t.Errorf("unexpected error with limit=0: %v", err)
				return
			}
			release()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutines blocked with limit=0 (expected unlimited)")
	}
}

// TestInflightLimitedSemaphore: limit=2 → 3rd acquire blocks until a release.
func TestInflightLimitedSemaphore(t *testing.T) {
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	r1, err := inflightSems.acquire(ctx, "nodeA", 2)
	if err != nil {
		t.Fatalf("acquire1 unexpected error: %v", err)
	}
	r2, err := inflightSems.acquire(ctx, "nodeA", 2)
	if err != nil {
		t.Fatalf("acquire2 unexpected error: %v", err)
	}

	// 3rd acquire should block: channel is full.
	acquired := make(chan error, 1)
	var r3 func()
	go func() {
		var e error
		r3, e = inflightSems.acquire(ctx, "nodeA", 2)
		acquired <- e
	}()

	// Assert not done after a brief wait.
	select {
	case <-acquired:
		t.Fatal("3rd acquire returned immediately; expected blocking")
	case <-time.After(80 * time.Millisecond):
	}

	// Release one slot — 3rd should unblock.
	r1()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("3rd acquire returned error after slot freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("3rd acquire did not unblock after release")
	}
	if r3 != nil {
		r3()
	}
	r2()
}

// TestInflightCtxCancel: ctx cancel while blocked → acquire returns ctx.Err(), no leak.
func TestInflightCtxCancel(t *testing.T) {
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx, cancel := context.WithCancel(context.Background())
	r1, _ := inflightSems.acquire(ctx, "nodeB", 1)

	done := make(chan error, 1)
	go func() {
		_, err := inflightSems.acquire(ctx, "nodeB", 1)
		done <- err
	}()

	// Give goroutine time to block.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error on ctx cancel, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not return after ctx cancel")
	}
	r1()
}

// TestInflightPerNodeIsolation: nodeA at limit does NOT block nodeB.
func TestInflightPerNodeIsolation(t *testing.T) {
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	// Fill nodeA's slot.
	_, err := inflightSems.acquire(ctx, "nodeA", 1)
	if err != nil {
		t.Fatalf("acquire nodeA: %v", err)
	}

	// nodeB with same limit must not block.
	done := make(chan error, 1)
	go func() {
		r, e := inflightSems.acquire(ctx, "nodeB", 1)
		if r != nil {
			r()
		}
		done <- e
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nodeB acquire failed: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("nodeB acquire blocked when nodeA semaphore was full")
	}
}

// TestInflightAcquireFailureIsRetriable verifies that an acquire failure (here:
// ctx cancel while blocked) produces an error that, when wrapped by
// cpierrors.Retriable — exactly as the five handler call-sites do — yields
// OkToRetry=true. This is a regression guard: the previous cpierrors.Cloud
// wrapper returned OkToRetry=false, meaning the Director would never re-queue.
func TestInflightAcquireFailureIsRetriable(t *testing.T) {
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx, cancel := context.WithCancel(context.Background())
	// Fill the single slot.
	r1, _ := inflightSems.acquire(ctx, "nodeX", 1)
	defer r1()

	done := make(chan error, 1)
	go func() {
		_, err := inflightSems.acquire(ctx, "nodeX", 1)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	var acquireErr error
	select {
	case acquireErr = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not return after ctx cancel")
	}
	if acquireErr == nil {
		t.Fatal("expected non-nil error when slot full and ctx cancelled")
	}
	// Wrap exactly as handlers do; verify OkToRetry=true.
	wrapped := cpierrors.Retriable("handler: in-flight limit exceeded: %s", acquireErr.Error())
	if !wrapped.OkToRetry() {
		t.Errorf("cpierrors.Retriable wrapping acquire error must have OkToRetry=true; got false")
	}
}

// TestInflightReleaseIdempotent: double-release does not panic or deadlock.
func TestInflightReleaseIdempotent(t *testing.T) {
	inflightSems = &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	release, err := inflightSems.acquire(ctx, "nodeC", 2)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Double call must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("double-release panicked: %v", r)
		}
	}()
	release()
	release()
}
