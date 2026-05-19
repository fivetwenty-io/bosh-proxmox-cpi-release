package log

import (
	"context"
	"log/slog"
	"sync"
)

// Level aliases for callers that want to set thresholds without importing slog.
var (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Entry is a captured log record. Used by tests that need to inspect emitted output.
type Entry struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// Observer collects log entries emitted by a Logger built via NewObservedLogger.
// It is safe for concurrent use.
type Observer struct {
	mu      sync.Mutex
	entries []Entry
}

// All returns a snapshot of every entry recorded so far.
func (o *Observer) All() []Entry {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Entry, len(o.entries))
	copy(out, o.entries)
	return out
}

// Len returns the number of recorded entries.
func (o *Observer) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.entries)
}

// observerHandler implements slog.Handler by appending each record to its Observer.
type observerHandler struct {
	minLevel slog.Level
	parent   []slog.Attr
	obs      *Observer
}

func (h *observerHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.minLevel
}

func (h *observerHandler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Level: r.Level, Message: r.Message, Attrs: make(map[string]any, len(h.parent)+r.NumAttrs())}
	for _, a := range h.parent {
		e.Attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs[a.Key] = a.Value.Any()
		return true
	})
	h.obs.mu.Lock()
	h.obs.entries = append(h.obs.entries, e)
	h.obs.mu.Unlock()
	return nil
}

func (h *observerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.parent)+len(attrs))
	merged = append(merged, h.parent...)
	merged = append(merged, attrs...)
	return &observerHandler{minLevel: h.minLevel, parent: merged, obs: h.obs}
}

func (h *observerHandler) WithGroup(_ string) slog.Handler {
	// Groups are flattened — observer ignores them so attribute keys remain stable for tests.
	return h
}

// NewObservedLogger returns a Logger that records every emitted entry into the
// returned Observer. Entries below level are dropped.
func NewObservedLogger(level slog.Level) (*Logger, *Observer) {
	obs := &Observer{}
	h := &observerHandler{minLevel: level, obs: obs}
	return &Logger{Logger: slog.New(h)}, obs
}
