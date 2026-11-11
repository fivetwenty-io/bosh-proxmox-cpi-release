package endpointcache

import (
	"bytes"
	"context"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// These tests use the process registry, so none of them runs in parallel.

func resetProcessRegistry(t *testing.T) {
	t.Helper()
	Drain(context.Background(), time.Second)
	Open()
	t.Cleanup(func() {
		Drain(context.Background(), time.Second)
		Open()
	})
}

func TestDrainWaitForUsesTheDefaultForZeroAndLess(t *testing.T) {
	for _, tc := range []struct {
		in, want time.Duration
	}{
		{0, DrainWait}, {-time.Second, DrainWait}, {time.Millisecond, time.Millisecond}, {time.Hour, time.Hour},
	} {
		if got := DrainWaitFor(tc.in); got != tc.want {
			t.Errorf("DrainWaitFor(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestDrainAtExitWaitsForWorkThatFinishesInTime(t *testing.T) {
	resetProcessRegistry(t)
	var done atomic.Bool
	if err := Go("slow write", config.DiscoveryModeAuto, func(ctx context.Context) {
		select {
		case <-time.After(100 * time.Millisecond):
			done.Store(true)
		case <-ctx.Done():
		}
	}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	DrainAtExit("tool", &stderr, 5*time.Second)
	if !done.Load() {
		t.Error("the drain returned before the worker finished its write")
	}
	if !Draining() {
		t.Error("the registry is still open after the drain")
	}
	if stderr.Len() != 0 {
		t.Errorf("a clean drain wrote %q to stderr", stderr.String())
	}
}

// Every command passes DrainAtExit no wait of its own, so the drain applies the
// default. A worker that needs a fraction of the default bound finishes first,
// whether the wait is zero or negative. A drain that took the zero as its wait
// would cancel the worker at once.
func TestDrainAtExitAppliesTheDefaultBoundToAZeroOrNegativeWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetProcessRegistry(t)
			var done atomic.Bool
			if err := Go("slow write", config.DiscoveryModeAuto, func(ctx context.Context) {
				select {
				case <-time.After(300 * time.Millisecond):
					done.Store(true)
				case <-ctx.Done():
				}
			}); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			DrainAtExit("tool", &stderr, tc.wait)
			if !done.Load() {
				t.Errorf("a drain with a wait of %s cancelled a write that needed 300ms, want the default bound of %s",
					tc.wait, DrainWait)
			}
			if stderr.Len() != 0 {
				t.Errorf("a clean drain wrote %q to stderr", stderr.String())
			}
		})
	}
}

func TestDrainAtExitCancelsWorkThatRunsPastTheBound(t *testing.T) {
	resetProcessRegistry(t)
	cancelled := make(chan struct{})
	if err := Go("stuck write", config.DiscoveryModeAuto, func(ctx context.Context) {
		<-ctx.Done()
		close(cancelled)
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	DrainAtExit("tool", &bytes.Buffer{}, 50*time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the drain took %s with a 50ms bound", took)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the stuck worker was never cancelled")
	}
}

func TestDrainAtExitStartedNoWorkReturnsAtOnce(t *testing.T) {
	resetProcessRegistry(t)
	start := time.Now()
	DrainAtExit("tool", &bytes.Buffer{}, time.Hour)
	if took := time.Since(start); took > time.Second {
		t.Errorf("a drain with no worker took %s", took)
	}
}

func TestDrainAtExitReportsAPanickedWorkerOnStderr(t *testing.T) {
	resetProcessRegistry(t)
	panicking := func(context.Context) { panic("the write blew up") }
	if err := Go("bad write", config.DiscoveryModeAuto, panicking); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	DrainAtExit("tool", &stderr, 5*time.Second)
	out := stderr.String()
	for _, want := range []string{"background worker panicked", "bad write", "the write blew up"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to contain %q", out, want)
		}
	}
}

// exitStuckWorker registers a worker that runs until the drain cancels it.
func exitStuckWorker(t *testing.T, name string, mode config.DiscoveryMode) {
	t.Helper()
	if err := Go(name, mode, func(ctx context.Context) { <-ctx.Done() }); err != nil {
		t.Fatal(err)
	}
}

// A worker the drain cancels under an explicit on is one Warn line on stderr
// that names the worker, because the operator asked for a cache the command
// did not finish writing.
func TestDrainAtExitWarnsOnceAboutACancelledWorkerUnderOn(t *testing.T) {
	resetProcessRegistry(t)
	exitStuckWorker(t, "stuck refresh", config.DiscoveryModeOn)
	var stderr bytes.Buffer
	DrainAtExit("tool", &stderr, 50*time.Millisecond)
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr = %q, want exactly one line", stderr.String())
	}
	for _, want := range []string{
		`"level":"WARN"`, "cancelled background work still running at exit", "stuck refresh",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q does not contain %q", lines[0], want)
		}
	}
}

// Under auto the cancellation is a Debug line, which a command that logs at
// Warn does not write.
func TestDrainAtExitWritesNothingAboutACancelledWorkerUnderAuto(t *testing.T) {
	resetProcessRegistry(t)
	exitStuckWorker(t, "stuck refresh", config.DiscoveryModeAuto)
	var stderr bytes.Buffer
	DrainAtExit("tool", &stderr, 50*time.Millisecond)
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// A worker cancelled under on and a worker that panicked each get their line,
// the cancellation at Warn and the panic at Error.
func TestDrainAtExitReportsAPanicAndACancellationTogether(t *testing.T) {
	resetProcessRegistry(t)
	panicking := func(context.Context) { panic("the write blew up") }
	if err := Go("bad write", config.DiscoveryModeAuto, panicking); err != nil {
		t.Fatal(err)
	}
	exitStuckWorker(t, "stuck refresh", config.DiscoveryModeOn)
	var stderr bytes.Buffer
	DrainAtExit("tool", &stderr, 100*time.Millisecond)
	out := stderr.String()
	for _, want := range []string{
		`"level":"ERROR"`, "background worker panicked", "bad write", "the write blew up",
		`"level":"WARN"`, "cancelled background work still running at exit", "stuck refresh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to contain %q", out, want)
		}
	}
}

// A SIGTERM during the wait ends it at once. The process still returns to its
// caller, who computes the exit code, and the worker is cancelled.
func TestDrainAtExitEndsOnASignal(t *testing.T) {
	resetProcessRegistry(t)
	held := make(chan os.Signal, 1)
	notifyTerm(t, held)
	cancelled := make(chan struct{})
	if err := Go("stuck write", config.DiscoveryModeAuto, func(ctx context.Context) {
		<-ctx.Done()
		close(cancelled)
	}); err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() {
		DrainAtExit("tool", &bytes.Buffer{}, time.Hour)
		close(returned)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !Draining() {
		if time.Now().After(deadline) {
			t.Fatal("the drain never started")
		}
		time.Sleep(time.Millisecond)
	}
	// The handler is installed before the drain closes the registry, so it is
	// in place by now.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGTERM did not end the drain")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("the signal did not cancel the worker")
	}
}

// notifyTerm keeps a handler for SIGTERM registered until the test ends, so a
// signal that lands after the drain removed its own handler, as it would when
// the rule under test breaks, cannot kill the test binary.
func notifyTerm(t *testing.T, ch chan os.Signal) {
	t.Helper()
	signal.Notify(ch, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(ch) })
}
