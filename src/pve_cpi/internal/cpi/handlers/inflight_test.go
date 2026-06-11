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
// Uses a local registry instance; does not touch the package-level inflightSems.
func TestInflightUnlimited(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			release, err := reg.acquire(ctx, "node1", 0)
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
// Uses a local registry instance; does not touch the package-level inflightSems.
func TestInflightLimitedSemaphore(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	r1, err := reg.acquire(ctx, "nodeA", 2)
	if err != nil {
		t.Fatalf("acquire1 unexpected error: %v", err)
	}
	r2, err := reg.acquire(ctx, "nodeA", 2)
	if err != nil {
		t.Fatalf("acquire2 unexpected error: %v", err)
	}

	// 3rd acquire should block: channel is full.
	acquired := make(chan error, 1)
	var r3 func()
	go func() {
		var e error
		r3, e = reg.acquire(ctx, "nodeA", 2)
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
// Uses a local registry instance; does not touch the package-level inflightSems.
// A ready channel replaces the prior time.Sleep to ensure the goroutine has
// reached the blocking select before cancel fires.
func TestInflightCtxCancel(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx, cancel := context.WithCancel(context.Background())
	r1, _ := reg.acquire(ctx, "nodeB", 1)

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		// Signal just before the blocking acquire so the test can cancel
		// only after this goroutine is guaranteed to be waiting on the semaphore.
		// acquire itself checks ctx.Done() in the same select, so signalling
		// immediately before the call is the tightest safe rendezvous point.
		close(ready)
		_, err := reg.acquire(ctx, "nodeB", 1)
		done <- err
	}()

	<-ready
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
// Uses a local registry instance; does not touch the package-level inflightSems.
func TestInflightPerNodeIsolation(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	// Fill nodeA's slot.
	_, err := reg.acquire(ctx, "nodeA", 1)
	if err != nil {
		t.Fatalf("acquire nodeA: %v", err)
	}

	// nodeB with same limit must not block.
	done := make(chan error, 1)
	go func() {
		r, e := reg.acquire(ctx, "nodeB", 1)
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
// Uses a local registry instance; does not touch the package-level inflightSems.
// A ready channel replaces the prior time.Sleep to ensure the goroutine is
// blocked on the semaphore before cancel fires.
func TestInflightAcquireFailureIsRetriable(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx, cancel := context.WithCancel(context.Background())
	// Fill the single slot.
	r1, _ := reg.acquire(ctx, "nodeX", 1)
	defer r1()

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(ready)
		_, err := reg.acquire(ctx, "nodeX", 1)
		done <- err
	}()

	<-ready
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
// Uses a local registry instance; does not touch the package-level inflightSems.
func TestInflightReleaseIdempotent(t *testing.T) {
	reg := &nodeInflightRegistry{m: map[string]chan struct{}{}}

	ctx := context.Background()
	release, err := reg.acquire(ctx, "nodeC", 2)
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
