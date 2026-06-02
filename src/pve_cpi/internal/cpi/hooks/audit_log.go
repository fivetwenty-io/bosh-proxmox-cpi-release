// Package hooks provides built-in dispatch middleware hooks for the CPI and a
// name registry that config validation and main.go wiring resolve against.
//
// A hook implements cpi.Hook (see internal/cpi/middleware.go). Hooks are opt-in
// via the pve.hooks manifest property; when none are configured, dispatch runs
// with no middleware and zero per-call overhead.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// auditStartKey is the unexported context key under which AuditLogHook.Before
// stashes the call start time for AuditLogHook.After to read. Using a context
// value (rather than a field on the hook) keeps a single hook instance safe for
// concurrent dispatch.
type auditStartKey struct{}

// AuditLogHook logs each CPI method call with its duration and outcome. It does
// not log argument content, so request payloads (which may carry credentials or
// other sensitive values) never reach the log. It observes only: After returns
// the handler's result and error unchanged.
type AuditLogHook struct {
	logger *log.Logger
}

// NewAuditLogHook constructs an AuditLogHook bound to logger. It matches the
// hooks.Registry constructor signature.
func NewAuditLogHook(logger *log.Logger) cpi.Hook {
	return &AuditLogHook{logger: logger}
}

var _ cpi.Hook = (*AuditLogHook)(nil)

// Before records the call start time in the returned context.
func (h *AuditLogHook) Before(ctx context.Context, _ string, _ []json.RawMessage, _ jsonrpc.Context) context.Context {
	return context.WithValue(ctx, auditStartKey{}, time.Now())
}

// After emits one Info line: method, duration_ms, outcome (ok|error), and the
// CPI error type on the error path. The result and error are returned unchanged.
func (h *AuditLogHook) After(ctx context.Context, method string, result any, err error) (any, error) {
	durationMS := 0.0
	if start, ok := ctx.Value(auditStartKey{}).(time.Time); ok {
		durationMS = float64(time.Since(start).Microseconds()) / 1000.0
	}

	if err != nil {
		errorType := "unknown"
		var cpiErr *cpierrors.Error
		if errors.As(err, &cpiErr) {
			errorType = string(cpiErr.Type())
		}
		h.logger.Info("cpi_audit",
			log.String("method", method),
			log.Float64("duration_ms", durationMS),
			log.String("outcome", "error"),
			log.String("error_type", errorType),
		)
		return result, err
	}

	h.logger.Info("cpi_audit",
		log.String("method", method),
		log.Float64("duration_ms", durationMS),
		log.String("outcome", "ok"),
	)
	return result, err
}
