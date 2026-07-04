package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// mustLogger creates a logger at the given level writing to buf. Fatal on error.
func mustLogger(t *testing.T, level string, buf *bytes.Buffer) *log.Logger {
	t.Helper()
	l, err := log.NewLogger(level, buf)
	if err != nil {
		t.Fatalf("NewLogger(%q): %v", level, err)
	}
	return l
}

// TestNewLogger_Levels verifies each valid level constructs without error.
func TestNewLogger_Levels(t *testing.T) {
	t.Parallel()
	levels := []string{"debug", "info", "warn", "error"}
	for _, lvl := range levels {
		t.Run(lvl, func(t *testing.T) {
			l, err := log.NewLogger(lvl, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("NewLogger(%q): unexpected error: %v", lvl, err)
			}
			if l == nil {
				t.Fatal("NewLogger returned nil logger")
			}
		})
	}
}

// TestNewLogger_InvalidLevel ensures unrecognized levels return an error.
func TestNewLogger_InvalidLevel(t *testing.T) {
	t.Parallel()
	cases := []string{"", "trace", "WARN", "verbose", "fatal"}
	for _, lvl := range cases {
		t.Run(lvl, func(t *testing.T) {
			_, err := log.NewLogger(lvl, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("NewLogger(%q): expected error, got nil", lvl)
			}
		})
	}
}

// TestNewLogger_WritesToSink confirms log output reaches the provided writer.
func TestNewLogger_WritesToSink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l.Info("hello world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("expected 'hello world' in output, got: %s", buf.String())
	}
}

// TestNewLogger_JSONOutput confirms output is valid JSON.
func TestNewLogger_JSONOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l.Debug("json check", log.String("key", "val"))

	line := strings.TrimSpace(buf.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, line)
	}
	if m["msg"] != "json check" {
		t.Fatalf("msg field mismatch: %v", m["msg"])
	}
	if m["key"] != "val" {
		t.Fatalf("key field mismatch: %v", m["key"])
	}
}

// TestNewLogger_LevelFiltering confirms that messages below the configured
// level are suppressed.
func TestNewLogger_LevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := log.NewLogger("warn", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l.Debug("should not appear")
	l.Info("should not appear either")
	if buf.Len() != 0 {
		t.Fatalf("expected no output at warn level for debug/info, got: %s", buf.String())
	}
}

// TestNewLogger_NilSinkDefaultsToStderr ensures nil sink is accepted (uses os.Stderr).
func TestNewLogger_NilSinkDefaultsToStderr(t *testing.T) {
	t.Parallel()
	l, err := log.NewLogger("info", nil)
	if err != nil {
		t.Fatalf("NewLogger with nil sink: %v", err)
	}
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

// TestWithRequestID round-trips the request ID through context.
func TestWithRequestID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ctx = log.WithRequestID(ctx, "req-abc-123")

	var buf bytes.Buffer
	l, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l2 := l.WithContext(ctx)
	l2.Info("request scoped")

	out := buf.String()
	if !strings.Contains(out, "req-abc-123") {
		t.Fatalf("request_id not present in output: %s", out)
	}
}

// TestWithMethod round-trips the method name through context.
func TestWithMethod(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ctx = log.WithMethod(ctx, "create_vm")

	var buf bytes.Buffer
	l, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	l2 := l.WithContext(ctx)
	l2.Info("method scoped")

	out := buf.String()
	if !strings.Contains(out, "create_vm") {
		t.Fatalf("method not present in output: %s", out)
	}
}

// TestWithContext_BothValues confirms both request_id and method appear together.
func TestWithContext_BothValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ctx = log.WithRequestID(ctx, "req-999")
	ctx = log.WithMethod(ctx, "delete_vm")

	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l.WithContext(ctx).Info("both fields")

	out := buf.String()
	if !strings.Contains(out, "req-999") || !strings.Contains(out, "delete_vm") {
		t.Fatalf("expected both fields in output: %s", out)
	}
}

// TestWithContext_EmptyCtx confirms WithContext on empty context returns same logger
// (no extra fields, no panic).
func TestWithContext_EmptyCtx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var buf bytes.Buffer
	l := mustLogger(t, "info", &buf)
	l2 := l.WithContext(ctx)
	if l2 == nil {
		t.Fatal("WithContext returned nil")
	}
	l2.Info("empty ctx")

	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if _, ok := m["request_id"]; ok {
		t.Fatal("request_id should not appear for empty ctx")
	}
	if _, ok := m["method"]; ok {
		t.Fatal("method should not appear for empty ctx")
	}
}

// TestFromContext_NoLogger ensures FromContext returns a nop logger (no panic)
// when no logger is stored in ctx.
func TestFromContext_NoLogger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := log.FromContext(ctx)
	if l == nil {
		t.Fatal("FromContext returned nil; expected nop logger")
	}
	l.Info("from empty context")
}

// TestFromContext_Roundtrip confirms a stored logger is retrieved correctly.
func TestFromContext_Roundtrip(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := mustLogger(t, "info", &buf)

	ctx := log.IntoContext(context.Background(), l)
	l2 := log.FromContext(ctx)
	if l2 == nil {
		t.Fatal("FromContext returned nil after IntoContext")
	}
	l2.Info("roundtrip")

	if !strings.Contains(buf.String(), "roundtrip") {
		t.Fatalf("expected roundtrip message in output: %s", buf.String())
	}
}

// TestWithFields chains additional structured fields onto a logger.
func TestWithFields(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l2 := l.WithFields(log.String("component", "pve"), log.Int("vmid", 100))
	l2.Info("with fields")

	out := buf.String()
	if !strings.Contains(out, "pve") || !strings.Contains(out, "100") {
		t.Fatalf("expected field values in output: %s", out)
	}
}

// TestFromContextOr_CtxLoggerPresent verifies FromContextOr returns the
// ctx-stored logger, not fallback, when ctx carries one.
func TestFromContextOr_CtxLoggerPresent(t *testing.T) {
	t.Parallel()
	var ctxBuf, fallbackBuf bytes.Buffer
	ctxLogger := mustLogger(t, "info", &ctxBuf)
	fallbackLogger := mustLogger(t, "info", &fallbackBuf)

	ctx := log.IntoContext(context.Background(), ctxLogger)
	got := log.FromContextOr(ctx, fallbackLogger)
	got.Info("routed message")

	if !strings.Contains(ctxBuf.String(), "routed message") {
		t.Errorf("expected message routed to ctx-stored logger, got ctxBuf=%q", ctxBuf.String())
	}
	if strings.Contains(fallbackBuf.String(), "routed message") {
		t.Errorf("message unexpectedly routed to fallback logger, got fallbackBuf=%q", fallbackBuf.String())
	}
}

// TestFromContextOr_NoCtxLogger_UsesFallback verifies FromContextOr returns
// fallback (not a silent nop) when ctx carries no logger — the behavior that
// distinguishes it from FromContext, whose contract is "always nop when
// absent".
func TestFromContextOr_NoCtxLogger_UsesFallback(t *testing.T) {
	t.Parallel()
	var fallbackBuf bytes.Buffer
	fallbackLogger := mustLogger(t, "info", &fallbackBuf)

	got := log.FromContextOr(context.Background(), fallbackLogger)
	got.Info("fallback message")

	if !strings.Contains(fallbackBuf.String(), "fallback message") {
		t.Errorf("expected message routed to fallback logger, got fallbackBuf=%q", fallbackBuf.String())
	}
}

// TestWithFields_Empty verifies that WithFields with no args returns a valid logger.
func TestWithFields_Empty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := mustLogger(t, "info", &buf)
	l2 := l.WithFields()
	if l2 == nil {
		t.Fatal("WithFields() returned nil")
	}
	l2.Info("no extra fields")
	if buf.Len() == 0 {
		t.Fatal("expected output from WithFields() logger")
	}
}

// TestNopLogger_NoOutput confirms NewNopLogger produces no output to any sink.
func TestNopLogger_NoOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := log.NewNopLogger()
	if l == nil {
		t.Fatal("NewNopLogger returned nil")
	}
	l.Debug("nop debug")
	l.Info("nop info")
	l.Warn("nop warn")
	l.Error("nop error")
	if buf.Len() != 0 {
		t.Fatalf("nop logger wrote to buffer: %s", buf.String())
	}
}

// TestLogger_AllLevelMethods exercises Debug/Info/Warn/Error methods without panic.
func TestLogger_AllLevelMethods(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l.Debug("debug msg", log.String("k", "v"))
	l.Info("info msg", log.Int("n", 1))
	l.Warn("warn msg")
	l.Error("error msg", log.Bool("ok", false))

	out := buf.String()
	for _, want := range []string{"debug msg", "info msg", "warn msg", "error msg"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output", want)
		}
	}
}

// TestErrScrubbed_ScrubsTokenBearingURL verifies that a token-bearing URL
// embedded in an error message is scrubbed when logged via ErrScrubbed.
func TestErrScrubbed_ScrubsTokenBearingURL(t *testing.T) {
	t.Parallel()

	// Simulate an error whose message embeds a presigned PVE URL with a token
	// query parameter — the form PVE can return in error responses.
	tokenErr := errors.New("storage request failed: https://pve.example.com:8006/api2/json/nodes/pve/storage?access_token=secret-pve-token-abc123&node=pve")

	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l.Debug("scrub test", log.ErrScrubbed(tokenErr))

	out := buf.String()
	if strings.Contains(out, "secret-pve-token-abc123") {
		t.Errorf("token leaked through ErrScrubbed: %s", out)
	}
	if !strings.Contains(out, log.RedactedPlaceholder) {
		t.Errorf("expected %q placeholder in output: %s", log.RedactedPlaceholder, out)
	}
	// Non-sensitive parts of the message must survive.
	if !strings.Contains(out, "storage request failed") {
		t.Errorf("non-sensitive message prefix must be preserved: %s", out)
	}
}

// TestErrScrubbed_NilError returns an empty string field without panic.
func TestErrScrubbed_NilError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l.Debug("nil err test", log.ErrScrubbed(nil))

	out := buf.String()
	if !strings.Contains(out, `"error":""`) && !strings.Contains(out, `"error": ""`) {
		t.Errorf("nil ErrScrubbed must produce empty error field: %s", out)
	}
}

// TestErrScrubbed_PlainErrorPassesThrough verifies a credential-free error message
// survives ErrScrubbed unchanged.
func TestErrScrubbed_PlainErrorPassesThrough(t *testing.T) {
	t.Parallel()

	plainErr := errors.New("connection refused")
	var buf bytes.Buffer
	l := mustLogger(t, "debug", &buf)
	l.Debug("plain err test", log.ErrScrubbed(plainErr))

	out := buf.String()
	if !strings.Contains(out, "connection refused") {
		t.Errorf("plain error message must survive ErrScrubbed: %s", out)
	}
}
