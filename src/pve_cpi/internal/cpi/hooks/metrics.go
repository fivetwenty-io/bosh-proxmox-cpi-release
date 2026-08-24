package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// metricsStartKey is the unexported context key under which MetricsHook.Before
// stashes the call start time for MetricsHook.After to read. Using a context
// value keeps a single hook instance safe for concurrent dispatch.
type metricsStartKey struct{}

// metricsRequestIDKey carries the request_id from Before into After so the
// After path does not need to re-receive the jsonrpc.Context.
type metricsRequestIDKey struct{}

// MetricsConfig holds the opt-in metrics-file configuration. Field JSON tags
// match the manifest sub-block pve.metrics. This struct lives in the hooks
// package (not internal/config) to avoid the import cycle: internal/config
// already imports internal/cpi/hooks for hook-name validation, so this package
// must not import internal/config.
type MetricsConfig struct {
	// Enabled turns the hook on. When false (the default), the MetricsHook is
	// never registered and adds no dispatch-path overhead.
	Enabled bool `json:"enabled"`
	// FilePath is the absolute path to the metrics file. The hook appends one
	// JSON line per CPI RPC. Required when Enabled is true; construction fails
	// with a clear error when empty.
	FilePath string `json:"file_path"`
}

// metricsSample is the JSON structure written per CPI call.
type metricsSample struct {
	TS         string  `json:"ts"`
	Method     string  `json:"method"`
	DurationMS float64 `json:"duration_ms"`
	Outcome    string  `json:"outcome"`
	RequestID  string  `json:"request_id"`
}

// MetricsHook appends one JSON-line sample per CPI RPC to a configured file.
// Writes are best-effort: a write failure is logged at Warn level and never
// fails the CPI call. The file is opened, written, and closed per call (atomic
// line append < PIPE_BUF; no file descriptor held across calls).
type MetricsHook struct {
	filePath string
	logger   *log.Logger
	now      func() time.Time // injectable for tests; nil → time.Now
}

// NewMetricsHook constructs a MetricsHook from Deps.Metrics. Returns an error
// when Enabled is true but FilePath is empty, mirroring the fail-fast posture
// of lb_register and external_command.
//
// Callers (main.go) gate on cfg.Metrics != nil && cfg.Metrics.Enabled before
// registering, so this constructor is only called in the enabled path.
//
// The returned cpi.Hook is a *MetricsHook. Tests may type-assert to access
// SetClock.
func NewMetricsHook(d Deps) (cpi.Hook, error) {
	if d.Metrics == nil {
		return nil, fmt.Errorf("metrics hook: Deps.Metrics is nil")
	}
	if strings.TrimSpace(d.Metrics.FilePath) == "" {
		return nil, fmt.Errorf("metrics hook: file_path is required when metrics is enabled")
	}
	return &MetricsHook{
		filePath: d.Metrics.FilePath,
		logger:   d.Logger,
	}, nil
}

var _ cpi.Hook = (*MetricsHook)(nil)

// Before records the call start time and request_id in the returned context.
func (h *MetricsHook) Before(ctx context.Context, _ string, _ []json.RawMessage, reqCtx jsonrpc.Context) context.Context {
	ctx = context.WithValue(ctx, metricsStartKey{}, h.nowTime())
	ctx = context.WithValue(ctx, metricsRequestIDKey{}, reqCtx.RequestID)
	return ctx
}

// After appends one JSON-line sample to the configured file. The result and
// error are returned unchanged. Write failures are logged at Warn level and
// never propagated to the caller.
func (h *MetricsHook) After(ctx context.Context, method string, result any, err error) (any, error) {
	now := h.nowTime()
	durationMS := 0.0
	if start, ok := ctx.Value(metricsStartKey{}).(time.Time); ok {
		durationMS = float64(now.Sub(start).Microseconds()) / 1000.0
	}

	outcome := "ok"
	if err != nil {
		outcome = "error"
	}

	requestID, _ := ctx.Value(metricsRequestIDKey{}).(string)

	sample := metricsSample{
		TS:         now.UTC().Format(time.RFC3339Nano),
		Method:     method,
		DurationMS: durationMS,
		Outcome:    outcome,
		RequestID:  requestID,
	}

	raw, encErr := json.Marshal(sample)
	if encErr != nil {
		h.logger.Warn("metrics hook: marshal failed", log.Err(encErr))
		return result, err
	}

	// Open, write line, close — one syscall-group per call for atomic append.
	f, openErr := os.OpenFile(h.filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644) // #nosec G302 -- 0644 so operators can read the metrics file; path is operator-configured, not user-supplied
	if openErr != nil {
		h.logger.Warn("metrics hook: open failed", log.String("path", h.filePath), log.Err(openErr))
		return result, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			h.logger.Warn("metrics hook: close failed", log.String("path", h.filePath), log.Err(closeErr))
		}
	}()

	line := make([]byte, len(raw)+1)
	copy(line, raw)
	line[len(raw)] = '\n'
	if _, writeErr := f.Write(line); writeErr != nil {
		h.logger.Warn("metrics hook: write failed", log.String("path", h.filePath), log.Err(writeErr))
	}

	return result, err
}

// SetClock replaces the hook's time source. Used by tests to inject a
// deterministic clock and assert duration values without wall-clock flakiness.
func (h *MetricsHook) SetClock(fn func() time.Time) {
	h.now = fn
}

// nowTime returns the current time via the injectable clock, falling back to
// time.Now when no clock is injected. Keeping this as a method (not inlining
// time.Now) lets tests inject a deterministic clock without a sync.Mutex.
func (h *MetricsHook) nowTime() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}
