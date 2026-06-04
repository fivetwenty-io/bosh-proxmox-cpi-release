package pve

import (
	"testing"
	"time"
)

// TestAdaptiveTaskInterval exercises the progress-derived poll-interval math and
// its clamp/fallback behavior (§7.28).
func TestAdaptiveTaskInterval(t *testing.T) {
	t.Parallel()
	const fallback = 2 * time.Second

	cases := []struct {
		name     string
		elapsed  time.Duration
		progress float64
		want     time.Duration
	}{
		{"no progress → fallback", 10 * time.Second, 0, fallback},
		{"negative progress → fallback", 10 * time.Second, -0.5, fallback},
		{"half done at 10s → 2s", 10 * time.Second, 0.5, 2 * time.Second},
		{"nearly done → clamp to min 1s", 9 * time.Second, 0.9, adaptivePollMinInterval},
		{"barely started → clamp to max 10s", 10 * time.Second, 0.1, adaptivePollMaxInterval},
		{"percentage form 50 → normalized 0.5 → 2s", 10 * time.Second, 50, 2 * time.Second},
		{"progress 1.0 (done) → min 1s", 10 * time.Second, 1.0, adaptivePollMinInterval},
		{"percentage form 10 → 0.1 → clamp max", 10 * time.Second, 10, adaptivePollMaxInterval},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := adaptiveTaskInterval(c.elapsed, c.progress, fallback)
			if got != c.want {
				t.Errorf("adaptiveTaskInterval(%v, %v): got %v, want %v", c.elapsed, c.progress, got, c.want)
			}
		})
	}
}

// TestClassifyTaskExit covers terminal exit-status classification shared by the
// adaptive loop.
func TestClassifyTaskExit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		exit    string
		warned  bool
		wantErr bool
	}{
		{"OK", "OK", false, false},
		{"lowercase ok", "ok", false, false},
		{"empty success", "", false, false},
		{"warnings success", "WARNINGS: 2", true, false},
		{"failure", "command failed", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := classifyTaskExit("UPID:x", c.exit, c.warned)
			if (err != nil) != c.wantErr {
				t.Errorf("classifyTaskExit(%q, warned=%v): err=%v, wantErr=%v", c.exit, c.warned, err, c.wantErr)
			}
		})
	}
}
