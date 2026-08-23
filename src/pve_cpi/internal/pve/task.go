// Task polling and await helpers for CPI action handlers.
package pve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

const (
	defaultPollIntervalMs = 2000 // 2 s
	defaultMaxWaitSeconds = 300  // 5 min
)

// taskStatusGetter mirrors the subset of the vendored tasks.Service that
// adaptive polling depends on. GetStatus was upstreamed in v3.2.7. The
// compile-time assertion below ensures the interface stays satisfied across
// future vendor refreshes.
type taskStatusGetter interface {
	GetStatus(ctx context.Context, node, upid string) (*sdktasks.Status, error)
}

var _ taskStatusGetter = sdktasks.Service(nil)

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
// Returns nil when the task exits with status "OK" or a "WARNINGS: N"
// status (completed with warnings).
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
//   - non-OK, non-WARNINGS exit status — non-retriable CloudError (permanent
//     task failure).
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

	// pveproxy may execute a proxied request on the node handling the API
	// connection rather than the node addressed in the URL (observed with
	// storage uploads to a shared storage in a multi-node cluster: POST to
	// /nodes/B/storage/X/upload over a connection to node A runs on A and
	// returns "UPID:A:..."). The task status endpoint rejects a UPID whose
	// embedded node differs from the URL's node segment ("upid: no such
	// task"), so the UPID's own node is authoritative for polling.
	if n := nodeFromUPID(upid); n != "" && n != node {
		node = n
	}

	// §7.28: when progress-aware adaptive polling is enabled, run the CPI-owned
	// loop. It is byte-equivalent in cadence for tasks that report no progress
	// (falls back to the fixed interval) and tightens polling for long ops
	// (clone, move-disk) that do report progress.
	if adaptivePollEnabled.Load() {
		return awaitTaskAdaptive(ctx, c, node, upid, opts...)
	}

	// Resolve the poll cadence from the process-wide (operator-configurable)
	// defaults.
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
		// Empty exit status means the SDK's TimeoutSeconds window elapsed
		// before PVE wrote a terminal ExitStatus field — the underlying PVE
		// task is still running. This is a polling timeout, not a permanent
		// task failure. Return a retriable error so the BOSH director
		// re-issues the CPI action (with a fresh VMID if applicable) rather
		// than holding a queue slot or treating a live task as failed.
		return cpierrors.WrapAs(
			fmt.Errorf("task %s: empty exit status (poll timeout — task still running)", upid),
			cpierrors.TypeRetriableCloud,
			fmt.Sprintf("task %s: empty exit status (poll timeout — task still running)", upid),
		)
	}
	if status.Warned {
		// "WARNINGS: N" exit: the task completed its work but logged WARN
		// lines -- e.g. qmdestroy removed the VM yet could not remove one
		// disk under storage-lock contention ("Could not remove disk ...,
		// check manually"). Failing here would fail an operation that
		// actually succeeded; any leftover volume is an audit concern
		// (scripts/disk-audit), not a task failure. Mirrors the adaptive
		// path's classifyTaskExit.
		return nil
	}
	if exit != "OK" && exit != "ok" {
		return cpierrors.Cloud("task %s failed: exit status %q", upid, exit)
	}

	return nil
}

// adaptiveTaskPollBounds are the clamp on the progress-derived poll interval.
const (
	adaptivePollMinInterval = 1 * time.Second
	adaptivePollMaxInterval = 10 * time.Second
)

// adaptiveTaskInterval derives the next poll interval from how long the task has
// been running and its reported progress, using vSphere's estimator: the
// projected remaining time (elapsed/progress − elapsed) divided by 5, clamped to
// [1s, 10s]. PVE reports progress in [0,1]; a value in (1,100] (some operations
// report a percentage) is normalized by /100. When progress is non-positive (no
// estimate available) the fixed fallback interval is returned instead.
func adaptiveTaskInterval(elapsed time.Duration, progress float64, fallback time.Duration) time.Duration {
	if progress > 1 {
		progress /= 100
	}
	if progress <= 0 {
		return fallback
	}
	if progress > 1 {
		progress = 1
	}
	eta := time.Duration(float64(elapsed) / progress)
	next := (eta - elapsed) / 5
	if next < adaptivePollMinInterval {
		return adaptivePollMinInterval
	}
	if next > adaptivePollMaxInterval {
		return adaptivePollMaxInterval
	}
	return next
}

// awaitTaskAdaptive is the §7.28 progress-aware poll loop. It mirrors AwaitTask's
// terminal/error classification, with ONE deliberate divergence: AwaitTask can
// observe ExitStatus == "" only while the task is still running (it returns a
// retriable poll-timeout for it), whereas this loop reaches classification
// only after Status == "stopped", where an empty ExitStatus is treated as
// success (see classifyTaskExit). The two paths never classify the same
// observable state differently — they observe different states.
//   - terminal "stopped" with exit OK/ok/"" or a WARNINGS status → nil
//   - terminal "stopped" with any other exit → non-retriable CloudError
//   - poll deadline exceeded while still running → retriable (task still running)
//   - ctx cancelled → retriable
//   - not-found task → non-retriable (preserves IsNotFound)
//   - other transient read errors → retried until the deadline
//
// The per-iteration sleep is the progress-derived interval (adaptiveTaskInterval)
// with the fixed cadence as fallback; the WithTestBackoff seam overrides it so
// tests poll instantly.
func awaitTaskAdaptive(ctx context.Context, c Client, node, upid string, opts ...AwaitOption) error {
	intervalMs, _, _ := taskPollDefaults()
	ao := &awaitOptions{pollIntervalMs: intervalMs, maxWaitSeconds: defaultMaxWaitSeconds}
	for _, opt := range opts {
		opt(ao)
	}
	fallback := time.Duration(ao.pollIntervalMs) * time.Millisecond

	actx, cancel := context.WithTimeout(ctx, time.Duration(ao.maxWaitSeconds)*time.Second)
	defer cancel()

	start := time.Now()
	for attempt := 0; ; attempt++ {
		status, err := c.Tasks().GetStatus(actx, node, upid)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return cpierrors.WrapAs(ctx.Err(), cpierrors.TypeRetriableCloud,
					fmt.Sprintf("AwaitTask %s: context cancelled", upid))
			}
			if actx.Err() != nil {
				return cpierrors.WrapAs(
					fmt.Errorf("task %s: poll timeout — task still running", upid),
					cpierrors.TypeRetriableCloud,
					fmt.Sprintf("task %s: poll timeout — task still running", upid))
			}
			if IsNotFound(err) {
				return wrapPollError(err, upid)
			}
			// Mirror the non-adaptive path: route through wrapPollError to classify.
			// Transient faults (5xx, ConnectionError, TimeoutError, net.Timeout,
			// pushback phrases, storage-lock, pmxcfs race) remain retriable and fall
			// through to the sleep-and-retry path. Permanent faults (4xx non-404,
			// generic unknown) are returned immediately so they are not misclassified
			// as a retriable poll-timeout when actx eventually fires.
			if !IsTransientTransport(err) && !IsStorageLockTimeout(err) &&
				!IsPmxcfsConfigMissing(err) && !IsPVEPushback(err) {
				return wrapPollError(err, upid)
			}
			// Transient read fault: fall through and retry until the deadline.
		case status == nil:
			return cpierrors.Cloud("AwaitTask %s: nil status returned from task service", upid)
		case status.Status == taskStatusStopped:
			return classifyTaskExit(upid, status.ExitStatus, status.Warned)
		}

		var d time.Duration
		if status != nil {
			d = adaptiveTaskInterval(time.Since(start), status.Progress, fallback)
		} else {
			d = fallback
		}
		if override := backoffFromCtx(ctx); override != nil {
			d = override(attempt)
		}

		select {
		case <-ctx.Done():
			return cpierrors.WrapAs(ctx.Err(), cpierrors.TypeRetriableCloud,
				fmt.Sprintf("AwaitTask %s: context cancelled", upid))
		case <-actx.Done():
			return cpierrors.WrapAs(
				fmt.Errorf("task %s: poll timeout — task still running", upid),
				cpierrors.TypeRetriableCloud,
				fmt.Sprintf("task %s: poll timeout — task still running", upid))
		case <-time.After(d):
		}
	}
}

// nodeFromUPID extracts the node name embedded in a PVE UPID
// ("UPID:<node>:<pid_hex>:..."). Returns "" when the string does not carry
// a parseable node segment.
func nodeFromUPID(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" {
		return ""
	}
	return parts[1]
}

// taskStatusStopped is the PVE task status string for a finished task.
const taskStatusStopped = "stopped"

// classifyTaskExit maps a terminal (Status == "stopped") task's exit status
// to the CPI result: OK/ok or a WARNINGS status (warned) is success; anything
// else is a non-retriable failure.
//
// An empty exit status on a STOPPED task is also accepted as success — an
// explicit choice, not an oversight: some PVE task types report stopped with
// no exit string, and failing them would break genuinely-completed
// operations. This is the one classification AwaitTask never faces (it only
// sees an empty ExitStatus while the task is still running, which it maps to
// a retriable poll-timeout).
func classifyTaskExit(upid, exit string, warned bool) error {
	if exit == "" {
		return nil
	}
	if warned || exit == "OK" || exit == "ok" {
		return nil
	}
	return cpierrors.Cloud("task %s failed: exit status %q", upid, exit)
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
	// AwaitTask polls the UPID's embedded node when it differs from the
	// caller's (pveproxy may run a proxied request on the connection-handling
	// node; see the note there). Log the node actually polled, not the
	// caller's, so the log line matches the request PVE serves — plus a
	// dedicated entry when the override fires, since that mismatch is the
	// first clue in cross-node task confusion.
	effectiveNode := node
	if n := nodeFromUPID(upid); n != "" && n != node {
		effectiveNode = n
		if logger != nil {
			logger.Debug("pve: task node overridden by UPID",
				log.String("upid", upid),
				log.String("caller_node", node),
				log.String("effective_node", effectiveNode))
		}
	}
	if logger != nil {
		logger.Debug("pve: awaiting task", log.String("upid", upid), log.String("node", effectiveNode))
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
