// Package log provides a slog-backed structured logger for the BOSH PVE CPI.
//
// CPI protocol uses stdout for JSON-RPC; all log output targets stderr (or a
// caller-supplied io.Writer) to avoid corrupting the protocol stream.
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// ctxKey is an unexported type for context keys in this package.
// Prevents collision with keys from other packages.
type ctxKey int

const (
	ctxKeyLogger    ctxKey = iota
	ctxKeyRequestID ctxKey = iota
	ctxKeyMethod    ctxKey = iota
)

// Field is an alias for slog.Attr so call sites can use short helpers like
// log.String / log.Int without importing slog directly.
type Field = slog.Attr

// Logger wraps slog.Logger with package-specific helpers.
//
// The ctx field carries a context set via WithContext; Debug/Info/Warn/Error
// pass it to LogAttrs so that slog handlers can extract trace/span values.
// Default is context.Background() when no context has been set.
type Logger struct {
	*slog.Logger
	ctx context.Context
}

// Field constructors mirror the names previously provided by zap so call-site
// migrations are mechanical.
func String(key, val string) Field          { return slog.String(key, val) }
func Int(key string, val int) Field         { return slog.Int(key, val) }
func Int64(key string, val int64) Field     { return slog.Int64(key, val) }
func Float64(key string, val float64) Field { return slog.Float64(key, val) }
func Bool(key string, val bool) Field       { return slog.Bool(key, val) }
func Any(key string, val any) Field         { return slog.Any(key, val) }

// Err is a convenience for slog.Any("error", err); slog has no Error field helper.
func Err(err error) Field { return slog.Any("error", err) }

// NewLogger constructs a JSON-encoded, leveled Logger writing to sink.
// level must be one of: debug, info, warn, error.
// When sink is nil, os.Stderr is used (preserves stdout for JSON-RPC).
func NewLogger(level string, sink io.Writer) (*Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		sink = os.Stderr
	}
	h := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: lvl})
	return &Logger{Logger: slog.New(h), ctx: context.Background()}, nil
}

// NewNopLogger returns a Logger that discards all output.
func NewNopLogger() *Logger {
	return &Logger{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), ctx: context.Background()}
}

// parseLevel converts a string level to slog.Level.
// Returns an error for any unrecognized value so callers fail fast.
func parseLevel(level string) (slog.Level, error) {
	switch level {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("log: unrecognized level %q; must be debug|info|warn|error", level)
	}
}

// WithRequestID stores a CPI request ID in ctx for later extraction by WithContext.
func WithRequestID(ctx context.Context, reqID string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, reqID)
}

// WithMethod stores a CPI method name in ctx for later extraction by WithContext.
func WithMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, ctxKeyMethod, method)
}

// WithContext returns a new Logger that:
//   - stores ctx so Debug/Info/Warn/Error pass it to slog.LogAttrs (enabling
//     trace/span propagation in slog handlers that inspect the context), and
//   - extracts request_id and method from ctx and attaches them as log fields
//     when present.
//
// Existing callers that discard the returned Logger are unaffected: the stored
// context only influences log calls made on the returned instance.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	fields := make([]Field, 0, 2)
	if reqID, ok := ctx.Value(ctxKeyRequestID).(string); ok && reqID != "" {
		fields = append(fields, String("request_id", reqID))
	}
	if method, ok := ctx.Value(ctxKeyMethod).(string); ok && method != "" {
		fields = append(fields, String("method", method))
	}
	var next *Logger
	if len(fields) == 0 {
		next = &Logger{Logger: l.Logger, ctx: ctx}
	} else {
		next = l.With(fields...)
		next.ctx = ctx
	}
	return next
}

// With returns a new Logger with the given fields attached.
// The stored context is propagated to the new instance.
func (l *Logger) With(fields ...Field) *Logger {
	return &Logger{Logger: l.Logger.With(attrsToArgs(fields)...), ctx: l.ctx}
}

// WithFields is an alias for With kept for source compatibility.
func (l *Logger) WithFields(fields ...Field) *Logger {
	return l.With(fields...)
}

func attrsToArgs(fields []Field) []any {
	out := make([]any, len(fields))
	for i, f := range fields {
		out[i] = f
	}
	return out
}

// FromContext returns the Logger stored in ctx by IntoContext.
// If none is found, returns NewNopLogger so callers never receive nil.
func FromContext(ctx context.Context) *Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*Logger); ok && l != nil {
		return l
	}
	return NewNopLogger()
}

// IntoContext stores l in ctx and returns the updated context.
// Downstream callers retrieve it via FromContext.
func IntoContext(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// logCtx returns the context to pass to LogAttrs.
// Falls back to context.Background() when l.ctx is nil (e.g. a Logger
// constructed directly via struct literal without calling NewLogger).
func (l *Logger) logCtx() context.Context {
	if l.ctx != nil {
		return l.ctx
	}
	return context.Background()
}

// Debug logs at debug level using the stored context (set via WithContext).
func (l *Logger) Debug(msg string, fields ...Field) {
	l.LogAttrs(l.logCtx(), slog.LevelDebug, msg, fields...)
}

// Info logs at info level using the stored context (set via WithContext).
func (l *Logger) Info(msg string, fields ...Field) {
	l.LogAttrs(l.logCtx(), slog.LevelInfo, msg, fields...)
}

// Warn logs at warn level using the stored context (set via WithContext).
func (l *Logger) Warn(msg string, fields ...Field) {
	l.LogAttrs(l.logCtx(), slog.LevelWarn, msg, fields...)
}

// Error logs at error level using the stored context (set via WithContext).
func (l *Logger) Error(msg string, fields ...Field) {
	l.LogAttrs(l.logCtx(), slog.LevelError, msg, fields...)
}

// Sync is a no-op; slog handlers flush per write. Retained for API symmetry
// so callers that previously invoked logger.Sync() keep compiling.
func (l *Logger) Sync() error { return nil }
