package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
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
// migrations are mechanical. Each wraps the corresponding slog.* helper and
// returns a Field (alias for slog.Attr) ready to pass to Logger methods.

// String returns a Field carrying a string value under the given key.
func String(key, val string) Field { return slog.String(key, val) }

// Int returns a Field carrying an int value under the given key.
func Int(key string, val int) Field { return slog.Int(key, val) }

// Int64 returns a Field carrying an int64 value under the given key.
func Int64(key string, val int64) Field { return slog.Int64(key, val) }

// Float64 returns a Field carrying a float64 value under the given key.
func Float64(key string, val float64) Field { return slog.Float64(key, val) }

// Bool returns a Field carrying a bool value under the given key.
func Bool(key string, val bool) Field { return slog.Bool(key, val) }

// Any returns a Field carrying an arbitrary value under the given key.
func Any(key string, val any) Field { return slog.Any(key, val) }

// Err returns a Field carrying the error message under the "error" key with
// any URL credentials scrubbed (userinfo and sensitive query parameters).
// Every call site that logs an error goes through this scrubbing: an error
// message can originate from a guest-controlled or PVE-returned value that
// embeds a token-bearing URL (e.g. a storage API endpoint with a presigned
// query parameter), so there is no safe unscrubbed variant. Err and
// ErrScrubbed are equivalent; Err delegates to it.
func Err(err error) Field { return ErrScrubbed(err) }

// URL returns a Field carrying a URL-shaped string with embedded credentials
// masked (userinfo and sensitive query parameters such as presigned
// signatures). Always use this instead of String for operator-supplied URLs —
// image_url, source_url, endpoints — so a credential-bearing URL never
// reaches a log sink verbatim.
func URL(key, raw string) Field { return slog.String(key, ScrubMessage(raw)) }

// ErrScrubbed returns a Field carrying the error message under the "error" key
// with any URL credentials scrubbed (userinfo and sensitive query parameters).
// Err is identical (it delegates here); ErrScrubbed remains exported as a
// separate name so a call site can still signal, for a reader, that it
// specifically expects a credential-bearing value at that point.
func ErrScrubbed(err error) Field {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", scrubURLString(err.Error()))
}

// NewLogger constructs a JSON-encoded, leveled Logger writing to sink.
// level must be one of: debug, info, warn, error.
// When sink is nil, os.Stderr is used (preserves stdout for JSON-RPC).
func NewLogger(level string, sink io.Writer) (*Logger, error) {
	return NewLoggerWithHandlers(level, sink)
}

// NewLoggerWithHandlers constructs a JSON-encoded, leveled Logger writing to
// sink, additionally fanning every log record out to each handler in extra.
// level and sink behave exactly as in NewLogger (nil sink defaults to
// os.Stderr). With zero extra handlers, this returns a Logger backed
// directly by the sink's JSON handler (no wrapping), so NewLogger's output is
// byte-identical to before extra handlers existed.
//
// The configured level governs every destination: a record below level never
// reaches the sink or any extra handler, so a secondary destination sees the
// same records the sink does (an extra handler may still impose a stricter
// filter of its own via Enabled, but never a looser one).
//
// Each extra handler is isolated from the others and from the sink: a panic
// or an error returned by any extra handler's Enabled or Handle call is
// swallowed and never propagates to the caller, and never prevents the sink
// (or any other extra handler) from receiving the record. This package never
// imports an OTel package itself; callers construct OTel-backed slog.Handler
// values (e.g. via otelslog) and pass them in as extra.
func NewLoggerWithHandlers(level string, sink io.Writer, extra ...slog.Handler) (*Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		sink = os.Stderr
	}
	sinkHandler := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: lvl})
	var h slog.Handler = sinkHandler
	if len(extra) > 0 {
		h = &multiHandler{sink: sinkHandler, extras: extra, level: lvl}
	}
	return &Logger{Logger: slog.New(h), ctx: context.Background()}, nil
}

// multiHandler implements slog.Handler by fanning each record out to a
// canonical sink handler plus zero or more extra handlers. The sink is
// authoritative: its Enabled/Handle results and errors behave exactly as if
// it were used alone. Extra handlers are best-effort — used to bridge
// records to secondary destinations (e.g. an OTel logs handler) without
// letting that destination's failures affect the primary sink.
type multiHandler struct {
	sink   slog.Handler
	extras []slog.Handler
	level  slog.Level
}

// Enabled reports whether any destination is enabled for level. The
// configured level is a floor for every destination — a record below it is
// dropped outright, so an extra handler whose own Enabled is permissive
// (OTel bridge handlers report enabled at all levels) cannot receive records
// the sink's level setting suppresses. Handle re-checks each handler's
// Enabled before dispatching, so a handler with a stricter filter still does
// not receive the record.
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < m.level {
		return false
	}
	if m.sink.Enabled(ctx, level) {
		return true
	}
	for _, e := range m.extras {
		if e.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle dispatches r to the sink handler and to every extra handler that
// reports itself enabled for r's level. Only the sink handler's error is
// returned to the caller; an extra handler's error or panic is swallowed
// (fail-open) so a broken secondary destination never blocks or corrupts the
// canonical log stream. Each handler receives its own clone of r since
// slog.Handler implementations may retain or mutate the record they're
// given.
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < m.level {
		return nil
	}
	var sinkErr error
	if m.sink.Enabled(ctx, r.Level) {
		sinkErr = m.sink.Handle(ctx, r.Clone())
	}
	for _, e := range m.extras {
		dispatchToExtraHandler(ctx, e, r)
	}
	return sinkErr
}

// dispatchToExtraHandler calls h.Enabled/Handle for r, recovering from any
// panic and discarding any returned error. This is the sole point where an
// extra handler's failure is contained; callers of multiHandler.Handle never
// see it.
func dispatchToExtraHandler(ctx context.Context, h slog.Handler, r slog.Record) {
	defer func() {
		_ = recover()
	}()
	if !h.Enabled(ctx, r.Level) {
		return
	}
	_ = h.Handle(ctx, r.Clone())
}

// WithAttrs returns a new multiHandler with attrs applied to the sink and to
// every extra handler, so attributes attached via Logger.With reach all
// destinations rather than only the sink.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newExtras := make([]slog.Handler, len(m.extras))
	for i, e := range m.extras {
		newExtras[i] = e.WithAttrs(attrs)
	}
	return &multiHandler{sink: m.sink.WithAttrs(attrs), extras: newExtras, level: m.level}
}

// WithGroup returns a new multiHandler with the group applied to the sink
// and to every extra handler, mirroring WithAttrs.
func (m *multiHandler) WithGroup(name string) slog.Handler {
	newExtras := make([]slog.Handler, len(m.extras))
	for i, e := range m.extras {
		newExtras[i] = e.WithGroup(name)
	}
	return &multiHandler{sink: m.sink.WithGroup(name), extras: newExtras, level: m.level}
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

// SpanFields returns trace_id/span_id fields extracted from an OTel span in
// ctx, or nil when ctx carries no span or an invalid SpanContext (zero fields
// added, zero behavior change when tracing is inactive). WithContext uses this
// internally; it is also exported for callers that build their own field list
// explicitly rather than deriving a whole *Logger from ctx — e.g.
// cpi.Dispatcher's per-request log lines, which already attach their own
// method/request_id fields and must not gain a second copy of those two keys
// by going through WithContext/FromContext.
func SpanFields(ctx context.Context) []Field {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return []Field{
			String("trace_id", sc.TraceID().String()),
			String("span_id", sc.SpanID().String()),
		}
	}
	return nil
}

// WithContext returns a new Logger that:
//   - stores ctx so Debug/Info/Warn/Error pass it to slog.LogAttrs (enabling
//     trace/span propagation in slog handlers that inspect the context), and
//   - extracts request_id and method from ctx and attaches them as log fields
//     when present, and
//   - extracts trace_id/span_id from an OTel span in ctx (via the stable
//     go.opentelemetry.io/otel/trace API) and attaches them as log fields
//     when the span context is valid; a ctx with no span, or an invalid/zero
//     SpanContext, adds nothing (zero behavior change when tracing inactive).
//
// Existing callers that discard the returned Logger are unaffected: the stored
// context only influences log calls made on the returned instance.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	fields := make([]Field, 0, 4)
	if reqID, ok := ctx.Value(ctxKeyRequestID).(string); ok && reqID != "" {
		fields = append(fields, String("request_id", reqID))
	}
	if method, ok := ctx.Value(ctxKeyMethod).(string); ok && method != "" {
		fields = append(fields, String("method", method))
	}
	fields = append(fields, SpanFields(ctx)...)
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

// FromContextOr returns the Logger stored in ctx by IntoContext, or fallback
// when ctx carries none. Unlike FromContext — which falls back to a silent
// NewNopLogger, a contract several internal/pve call sites rely on to mean
// "no per-request logger in scope, log nothing" — FromContextOr lets the
// caller name an explicit, non-nop fallback (e.g. a handler's startup-time
// Deps.Logger) so a log call is never silently swallowed just because ctx
// happens to lack the per-request logger (e.g. a handler unit test that
// builds ctx via context.Background()). Callers must pass a non-nil
// fallback; the only production caller (handlers.Deps.Log) guarantees this.
func FromContextOr(ctx context.Context, fallback *Logger) *Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*Logger); ok && l != nil {
		return l
	}
	return fallback
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
