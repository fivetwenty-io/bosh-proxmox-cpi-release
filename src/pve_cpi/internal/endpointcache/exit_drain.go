package endpointcache

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// DrainWaitFor returns wait, or DrainWait when wait is zero or less. Entry
// points that let a test shorten the bound pass their override through it.
func DrainWaitFor(wait time.Duration) time.Duration {
	if wait <= 0 {
		return DrainWait
	}
	return wait
}

// DrainAtExit is the last drain of a command that builds PVE clients. It waits
// for the process registry's workers for at most wait, or DrainWait when wait
// is zero, cancels the ones still running, and runs the releases registered
// with AfterDrain, which close the cache stores. A command opens the registry
// with Open when it starts and defers DrainAtExit, so a down mark or a
// discovery refresh that a client started just before the command ends
// finishes, or is cancelled, before the process exits.
//
// A SIGINT or SIGTERM during the wait ends the wait at once, and the command
// still exits with the code its caller computed, because the signal is caught
// here and not left to kill the process. command names the caller in the one
// line the drain writes when its logger fails. The command has no other logger,
// so the drain reports on stderr, through LogDrainResult at Warn. A panicked
// worker is an Error line there, and a cancelled worker is a Warn line only
// when it serves a cluster config with an explicit "on", because that is the
// level its own config's mode gives. A normal exit writes nothing, and a
// command that started no worker never waits.
func DrainAtExit(command string, stderr io.Writer, wait time.Duration) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result := Drain(ctx, DrainWaitFor(wait))
	if len(result.Panics) == 0 && len(result.Cancelled) == 0 {
		return
	}
	logger, err := log.NewLogger("warn", stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: background workers panicked (%d) or were cancelled (%d), and the logger "+
			"failed: %s\n", command, len(result.Panics), len(result.Cancelled), err)
		return
	}
	LogDrainResult(logger, result)
}

// LogDrainResult logs a drain's findings on logger. A panicked worker is an
// Error with its stack. Cancelled work is logged at the level the drain
// reports, a Debug line, or a Warn when a cancelled worker serves a cluster
// config with an explicit "on", because the operator asked for a cache this
// call did not finish writing. Work that finished in time is a Debug line, and
// a call that started no worker logs nothing.
func LogDrainResult(logger *log.Logger, result DrainResult) {
	for _, p := range result.Panics {
		logger.Error("endpoint cache: background worker panicked",
			log.String("worker", p.Name),
			log.Any("panic", p.Value),
			log.String("stack", string(p.Stack)),
		)
	}
	switch {
	case len(result.Cancelled) > 0:
		logger.LogAttrs(context.Background(), result.CancelledLevel,
			"endpoint cache: cancelled background work still running at exit",
			log.String("workers", strings.Join(result.Cancelled, ", ")),
			log.Int("started", result.Started),
			log.String("waited", result.Waited.Round(time.Millisecond).String()),
		)
	case result.Started > 0:
		logger.Debug("endpoint cache: background work finished before exit",
			log.Int("started", result.Started),
			log.String("waited", result.Waited.Round(time.Millisecond).String()),
		)
	}
}
