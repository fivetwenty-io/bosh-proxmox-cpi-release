package handlers

import (
	"context"
	"time"
)

// resizeConvergencePollIntervalDefault is the wait between size-convergence
// polls in resize_disk (§7.27). 2s is long enough not to add poll pressure to a
// busy cluster, and short enough to converge promptly on a healthy backend.
const resizeConvergencePollIntervalDefault = 2 * time.Second

// resizeConvergencePollKey carries a test's shorter poll on the request context.
type resizeConvergencePollKey struct{}

// WithResizeConvergencePollForTest returns a context whose size-convergence
// polls wait d instead of resizeConvergencePollIntervalDefault. A non-positive
// d leaves ctx as it is. It rides the context rather than a package variable,
// so tests that set it can run in parallel. Production code never calls it; it
// mirrors pve.WithTestBackoff.
func WithResizeConvergencePollForTest(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, resizeConvergencePollKey{}, d)
}

// resizeConvergencePollInterval returns the poll ctx carries when a test set
// one, and resizeConvergencePollIntervalDefault otherwise.
func resizeConvergencePollInterval(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(resizeConvergencePollKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return resizeConvergencePollIntervalDefault
}
