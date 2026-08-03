package log

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

func TestFieldConstructors_AllTypes(t *testing.T) {
	t.Parallel()
	if got := Int64("k", 42); got.Key != "k" || got.Value.Int64() != 42 {
		t.Fatalf("Int64: got %+v", got)
	}
	if got := Float64("k", 3.14); got.Key != "k" || got.Value.Float64() != 3.14 {
		t.Fatalf("Float64: got %+v", got)
	}
	if got := Any("k", "v"); got.Key != "k" || got.Value.Any() != "v" {
		t.Fatalf("Any: got %+v", got)
	}
	want := errors.New("boom")
	if got := Err(want); got.Key != "error" || got.Value.Any() != want.Error() {
		t.Fatalf("Err: got %+v", got)
	}
}

func TestObserver_CapturesEntries(t *testing.T) {
	t.Parallel()
	l, obs := NewObservedLogger(slog.LevelDebug)
	l.Info("hello", String("k", "v"), Int("n", 7))
	l.Warn("watch", Bool("b", true))

	entries := obs.All()
	if len(entries) != 2 {
		t.Fatalf("len=%d, want 2", len(entries))
	}
	if obs.Len() != 2 {
		t.Fatalf("Len=%d, want 2", obs.Len())
	}

	if entries[0].Level != slog.LevelInfo || entries[0].Message != "hello" {
		t.Fatalf("entry[0]: %+v", entries[0])
	}
	if entries[0].Attrs["k"] != "v" || entries[0].Attrs["n"] != int64(7) {
		t.Fatalf("entry[0].Attrs: %+v", entries[0].Attrs)
	}
	if entries[1].Level != slog.LevelWarn || entries[1].Attrs["b"] != true {
		t.Fatalf("entry[1]: %+v", entries[1])
	}
}

func TestObserver_LevelFilter(t *testing.T) {
	t.Parallel()
	l, obs := NewObservedLogger(slog.LevelWarn)
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")

	if obs.Len() != 2 {
		t.Fatalf("Len=%d, want 2 (warn+error only)", obs.Len())
	}
	if obs.All()[0].Message != "w" || obs.All()[1].Message != "e" {
		t.Fatalf("entries: %+v", obs.All())
	}
}

func TestObserver_WithAttrsCarriesParent(t *testing.T) {
	t.Parallel()
	l, obs := NewObservedLogger(slog.LevelDebug)
	child := l.With(String("scope", "test"))
	child.Info("msg", Int("n", 1))

	got := obs.All()[0]
	if got.Attrs["scope"] != "test" || got.Attrs["n"] != int64(1) {
		t.Fatalf("attrs: %+v", got.Attrs)
	}
}

func TestObserver_WithGroupIsTransparent(t *testing.T) {
	t.Parallel()
	obs := &Observer{}
	h := &observerHandler{minLevel: slog.LevelDebug, obs: obs}
	grouped := h.WithGroup("ignored")
	l := &Logger{Logger: slog.New(grouped)}
	l.Info("msg", String("k", "v"))

	if obs.Len() != 1 || obs.All()[0].Attrs["k"] != "v" {
		t.Fatalf("WithGroup should be transparent; got %+v", obs.All())
	}
}

func TestObserver_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	l, obs := NewObservedLogger(slog.LevelDebug)
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			l.Info("msg", Int("i", i))
		}(i)
	}
	wg.Wait()
	if obs.Len() != n {
		t.Fatalf("Len=%d, want %d", obs.Len(), n)
	}
}

func TestEnabledThreshold(t *testing.T) {
	t.Parallel()
	h := &observerHandler{minLevel: slog.LevelWarn, obs: &Observer{}}
	if h.Enabled(context.TODO(), slog.LevelInfo) {
		t.Fatal("info should be disabled when min is warn")
	}
	if !h.Enabled(context.TODO(), slog.LevelError) {
		t.Fatal("error should be enabled when min is warn")
	}
}

func TestLevelAliases(t *testing.T) {
	t.Parallel()
	if LevelDebug != slog.LevelDebug || LevelInfo != slog.LevelInfo ||
		LevelWarn != slog.LevelWarn || LevelError != slog.LevelError {
		t.Fatal("level aliases drifted from slog")
	}
}
