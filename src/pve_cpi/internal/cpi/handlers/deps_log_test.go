package handlers_test

// deps_log_test.go verifies handlers.Deps.Log(ctx), the seam every handler
// logging call site goes through instead of deps.Logger directly.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// TestDeps_Log_CtxLoggerTakesPrecedence verifies Deps.Log(ctx) returns the
// ctx-carried logger over deps.Logger when ctx has one.
func TestDeps_Log_CtxLoggerTakesPrecedence(t *testing.T) {
	t.Parallel()
	var depsBuf, ctxBuf bytes.Buffer
	depsLogger, err := log.NewLogger("info", &depsBuf)
	if err != nil {
		t.Fatalf("NewLogger (deps): %v", err)
	}
	ctxLogger, err := log.NewLogger("info", &ctxBuf)
	if err != nil {
		t.Fatalf("NewLogger (ctx): %v", err)
	}

	deps := handlers.Deps{Logger: depsLogger}
	ctx := log.IntoContext(context.Background(), ctxLogger)

	deps.Log(ctx).Info("routed")

	if !strings.Contains(ctxBuf.String(), "routed") {
		t.Errorf("expected message routed to ctx logger, got ctxBuf=%q", ctxBuf.String())
	}
	if strings.Contains(depsBuf.String(), "routed") {
		t.Errorf("message unexpectedly routed to deps.Logger, got depsBuf=%q", depsBuf.String())
	}
}

// TestDeps_Log_NoCtxLogger_FallsBackToDepsLogger verifies Deps.Log(ctx) uses
// deps.Logger when ctx carries no logger — the fallback every existing
// handler unit test (which builds ctx via context.Background()) relies on
// to keep passing unchanged.
func TestDeps_Log_NoCtxLogger_FallsBackToDepsLogger(t *testing.T) {
	t.Parallel()
	var depsBuf bytes.Buffer
	depsLogger, err := log.NewLogger("info", &depsBuf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	deps := handlers.Deps{Logger: depsLogger}
	deps.Log(context.Background()).Info("fallback")

	if !strings.Contains(depsBuf.String(), "fallback") {
		t.Errorf("expected message routed to deps.Logger, got depsBuf=%q", depsBuf.String())
	}
}

// TestDeps_Log_NilLoggerAndNoCtxLogger_NeverPanics verifies Deps.Log(ctx) is
// safe to call — and to log through — even when both deps.Logger and the ctx
// logger are absent (e.g. a Deps{} zero-value literal in a handler test).
func TestDeps_Log_NilLoggerAndNoCtxLogger_NeverPanics(t *testing.T) {
	t.Parallel()
	deps := handlers.Deps{}
	l := deps.Log(context.Background())
	if l == nil {
		t.Fatal("Deps.Log returned nil logger")
	}
	l.Info("must not panic") // exercises the nop-logger fallback path
}
