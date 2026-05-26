// Package pve provides task-await and VMID-allocation helpers used by CPI action handlers.
package pve

import (
	"context"
	"fmt"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

const (
	defaultPollIntervalMs = 2000 // 2 s
	defaultMaxWaitSeconds = 300  // 5 min
)

// StemcellMaxWait is the task timeout for stemcell upload + create_vm
// import-from operations, which may include qcow2->raw format conversion
// on LVM/ZFS storages.
const StemcellMaxWait = 600 * time.Second

// awaitOptions holds resolved configuration for AwaitTask.
type awaitOptions struct {
	pollIntervalMs int
	maxWaitSeconds int
}

// AwaitOption is a functional option for AwaitTask.
type AwaitOption func(*awaitOptions)

// WithPollInterval sets the poll interval passed to the SDK WaitOptions.
// Values ≤ 0 are ignored (default 2 s applies).
func WithPollInterval(d time.Duration) AwaitOption {
	return func(o *awaitOptions) {
		if d > 0 {
			o.pollIntervalMs = int(d.Milliseconds())
		}
	}
}

// WithMaxWait sets the maximum wait timeout passed to the SDK WaitOptions.
// Overrides the ctx deadline if shorter. Values ≤ 0 are ignored (default 5 min applies).
func WithMaxWait(d time.Duration) AwaitOption {
	return func(o *awaitOptions) {
		if d > 0 {
			o.maxWaitSeconds = int(d.Seconds())
		}
	}
}

// AwaitTask polls a PVE task identified by upid on the given node until it
// reaches terminal state or the deadline is exceeded. It wraps the SDK
// tasks.Service.Wait with CPI-standard defaults and error normalization.
//
// Returns nil when the task exits with status "OK".
// Returns a *cpierrors.Error (TypeCloud) when:
//   - upid is empty — validated before the SDK call.
//   - node is empty — validated before the SDK call.
//   - ctx is nil — validated before the SDK call.
//   - the task exits with a non-OK exit status.
//   - the SDK call itself fails (network, timeout, etc.).
//
// ctx cancellation is propagated through the SDK poller; when ctx is cancelled
// the function returns a *cpierrors.Error wrapping ctx.Err().
func AwaitTask(ctx context.Context, c Client, node, upid string, opts ...AwaitOption) error {
	if ctx == nil {
		return cpierrors.Cloud("AwaitTask: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("AwaitTask: client must not be nil")
	}
	if node == "" {
		return cpierrors.Cloud("AwaitTask: node must not be empty")
	}
	if upid == "" {
		return cpierrors.Cloud("AwaitTask: upid must not be empty")
	}

	ao := &awaitOptions{
		pollIntervalMs: defaultPollIntervalMs,
		maxWaitSeconds: defaultMaxWaitSeconds,
	}
	for _, opt := range opts {
		opt(ao)
	}

	waitOpts := &sdktasks.WaitOptions{
		TimeoutSeconds:    ao.maxWaitSeconds,
		IntervalMillis:    ao.pollIntervalMs,
		MaxIntervalMillis: ao.pollIntervalMs * 5,
		Backoff:           false,
		JitterPct:         10,
	}

	status, err := c.Tasks().Wait(ctx, node, upid, waitOpts)
	if err != nil {
		// ctx cancellation surfaces as context.DeadlineExceeded from the SDK poller.
		if ctx.Err() != nil {
			return cpierrors.Wrap(ctx.Err(), fmt.Sprintf("AwaitTask %s: context cancelled", upid))
		}
		return cpierrors.Wrap(err, fmt.Sprintf("AwaitTask %s: poll failed", upid))
	}

	if status == nil {
		return cpierrors.Cloud("AwaitTask %s: nil status returned from task service", upid)
	}

	exit := status.ExitStatus
	if exit == "" {
		// Empty exit status means the task poller returned before PVE wrote
		// the terminal ExitStatus field. Treating this as success silently
		// masks tasks that stalled or were killed without a recorded outcome.
		return cpierrors.Cloud("task %s: empty exit status (polling did not surface completion state)", upid)
	}
	if exit != "OK" && exit != "ok" {
		return cpierrors.Cloud("task %s failed: exit status %q", upid, exit)
	}

	return nil
}

// AwaitTaskWithLogger is like AwaitTask but emits debug/error log entries via the
// provided logger. Callers that do not need logging should use AwaitTask directly.
func AwaitTaskWithLogger(
	ctx context.Context, c Client, node, upid string, logger *log.Logger, opts ...AwaitOption,
) error {
	if logger != nil {
		logger.Debug("pve: awaiting task", log.String("upid", upid), log.String("node", node))
	}
	err := AwaitTask(ctx, c, node, upid, opts...)
	if err != nil {
		if logger != nil {
			logger.Error("pve: task failed", log.String("upid", upid), log.Err(err))
		}
		return err
	}
	if logger != nil {
		logger.Debug("pve: task completed OK", log.String("upid", upid))
	}
	return nil
}
