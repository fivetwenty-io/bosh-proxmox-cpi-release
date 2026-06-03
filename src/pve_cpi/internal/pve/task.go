// Task polling and await helpers for CPI action handlers.
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
// Returns a *cpierrors.Error with the following retriable classification:
//   - upid is empty — non-retriable CloudError (programming error).
//   - node is empty — non-retriable CloudError (programming error).
//   - ctx is nil — non-retriable CloudError (programming error).
//   - ctx cancelled / deadline exceeded — retriable RetriableCloudError; the
//     BOSH director re-issues the CPI action with a new VMID.
//   - transient poll fault (5xx, connection error, network timeout) — retriable
//     RetriableCloudError via WrapError classification.
//   - nil status — non-retriable CloudError (SDK contract violation).
//   - empty exit status — non-retriable CloudError (PVE never wrote outcome).
//   - non-OK exit status — non-retriable CloudError (permanent task failure).
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

	// Resolve the poll cadence from the process-wide (operator-configurable)
	// defaults. WithPollInterval still overrides the base interval per call.
	pollIntervalMs, pollMaxIntervalMs, pollJitterPct := taskPollDefaults()
	ao := &awaitOptions{
		pollIntervalMs: pollIntervalMs,
		maxWaitSeconds: defaultMaxWaitSeconds,
	}
	for _, opt := range opts {
		opt(ao)
	}

	// The maximum poll interval must never fall below the (possibly overridden)
	// base interval, or the SDK would back off to a value smaller than its
	// starting point.
	maxIntervalMs := pollMaxIntervalMs
	if maxIntervalMs < ao.pollIntervalMs {
		maxIntervalMs = ao.pollIntervalMs
	}

	waitOpts := &sdktasks.WaitOptions{
		TimeoutSeconds:    ao.maxWaitSeconds,
		IntervalMillis:    ao.pollIntervalMs,
		MaxIntervalMillis: maxIntervalMs,
		Backoff:           false,
		JitterPct:         pollJitterPct,
	}

	status, err := c.Tasks().Wait(ctx, node, upid, waitOpts)
	if err != nil {
		// ctx cancellation / deadline: retriable so the director re-issues the
		// CPI action. The in-flight PVE task is orphaned; the director supplies
		// a fresh VMID on retry and the VMID range-scan prevents collision.
		if ctx.Err() != nil {
			return cpierrors.WrapAs(ctx.Err(), cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AwaitTask %s: context cancelled", upid))
		}
		// Route through WrapError: 5xx, ConnectionError, TimeoutError, net.Error
		// Timeout all surface as retriable; permanent SDK errors stay non-retriable.
		return wrapPollError(err, upid)
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

// wrapPollError maps a task poll SDK error to the appropriate CPI error type.
//
// 404 errors are wrapped directly via cpierrors.Wrap (non-retriable, SDK chain
// preserved) so callers can still detect them with pve.IsNotFound — a not-found
// task UPID is a permanent condition, not a transient one.
//
// All other errors route through WrapError for standard retriability
// classification: 5xx → retriable, ConnectionError/TimeoutError/net.Timeout →
// retriable, 4xx non-404 → non-retriable, unknown → non-retriable.
func wrapPollError(err error, upid string) error {
	label := fmt.Sprintf("AwaitTask %s: poll failed", upid)
	if IsNotFound(err) {
		// Preserve SDK error chain so handlers can detect not-found via IsNotFound.
		return cpierrors.Wrap(err, label)
	}
	mapped := WrapError(err)
	return cpierrors.Wrap(mapped, label)
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
