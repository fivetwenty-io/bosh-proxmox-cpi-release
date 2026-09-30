package handlers

import (
	"context"
	"time"
)

// healthPollMinIntervalDefault is the production floor for the health-check
// poll interval. An operator-supplied IntervalSec of 0 is clamped to it so the
// loop never becomes a tight busy-loop in production.
const healthPollMinIntervalDefault = 1 * time.Second

// healthPollMinIntervalKey carries a test's lower floor on the request context.
type healthPollMinIntervalKey struct{}

// WithHealthPollMinIntervalForTest returns a context whose health-check poll
// floor is d instead of healthPollMinIntervalDefault. A non-positive d leaves
// ctx as it is, so a test that wants fast polling passes a small positive
// floor. It rides the context rather than a package variable, so tests that
// set it can run in parallel. Production code never calls it; it mirrors
// pve.WithTestBackoff.
func WithHealthPollMinIntervalForTest(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, healthPollMinIntervalKey{}, d)
}

// healthPollMinInterval returns the floor ctx carries when a test set one, and
// healthPollMinIntervalDefault otherwise.
func healthPollMinInterval(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(healthPollMinIntervalKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return healthPollMinIntervalDefault
}
