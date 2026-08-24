package hooks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/hooks"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// newTestMetricsHook constructs a MetricsHook wired to a temp file and
// a no-op logger. Returns the hook and the file path.
func newTestMetricsHook(t *testing.T) (*hooks.MetricsHook, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")

	var logBuf nopWriter
	logger, err := log.NewLogger("warn", &logBuf)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	h, err := hooks.NewMetricsHook(hooks.Deps{
		Logger:  logger,
		Metrics: &hooks.MetricsConfig{Enabled: true, FilePath: path},
	})
	if err != nil {
		t.Fatalf("NewMetricsHook: %v", err)
	}
	return h.(*hooks.MetricsHook), path
}

// nopWriter satisfies io.Writer for log.NewLogger without allocating a bytes.Buffer.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// readLines returns all non-empty lines from a file.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open metrics file: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close metrics file: %v", err)
		}
	}()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan metrics file: %v", err)
	}
	return lines
}

// decodeSample parses a metrics line into a map.
func decodeSample(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode sample %q: %v", line, err)
	}
	return m
}

func TestMetricsHook_SampleValidJSON(t *testing.T) {
	h, path := newTestMetricsHook(t)

	reqCtx := jsonrpc.Context{RequestID: "cpi-test-001"}
	ctx := h.Before(context.Background(), "create_vm", nil, reqCtx)
	res, err := h.After(ctx, "create_vm", "vm-123", nil)
	if res != "vm-123" || err != nil {
		t.Errorf("After must return result/err unchanged; got %v / %v", res, err)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line in metrics file; got %d", len(lines))
	}

	m := decodeSample(t, lines[0])

	// All required fields present.
	for _, field := range []string{"ts", "method", "duration_ms", "outcome", "request_id"} {
		if _, ok := m[field]; !ok {
			t.Errorf("sample missing field %q; sample: %s", field, lines[0])
		}
	}
	if m["method"] != "create_vm" {
		t.Errorf("method = %v; want create_vm", m["method"])
	}
	if m["outcome"] != "ok" {
		t.Errorf("outcome = %v; want ok", m["outcome"])
	}
	if m["request_id"] != "cpi-test-001" {
		t.Errorf("request_id = %v; want cpi-test-001", m["request_id"])
	}
	if _, ok := m["ts"].(string); !ok {
		t.Errorf("ts must be a string; got %T", m["ts"])
	}
}

func TestMetricsHook_OutcomeError(t *testing.T) {
	h, path := newTestMetricsHook(t)

	reqCtx := jsonrpc.Context{RequestID: "cpi-test-err"}
	ctx := h.Before(context.Background(), "delete_vm", nil, reqCtx)
	wantErr := errors.New("delete failed")
	res, gotErr := h.After(ctx, "delete_vm", nil, wantErr)
	if !errors.Is(gotErr, wantErr) {
		t.Errorf("After must return error unchanged; got %v", gotErr)
	}
	if res != nil {
		t.Errorf("result should remain nil; got %v", res)
	}

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line; got %d", len(lines))
	}
	m := decodeSample(t, lines[0])
	if m["outcome"] != "error" {
		t.Errorf("outcome = %v; want error", m["outcome"])
	}
}

func TestMetricsHook_DurationNonNegative(t *testing.T) {
	h, path := newTestMetricsHook(t)

	// Inject a fixed clock to avoid wall-clock flakiness.
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(42 * time.Millisecond)
	call := 0
	h.SetClock(func() time.Time {
		call++
		if call == 1 {
			return t0
		}
		return t1
	})

	ctx := h.Before(context.Background(), "create_vm", nil, jsonrpc.Context{})
	_, _ = h.After(ctx, "create_vm", nil, nil)

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line; got %d", len(lines))
	}
	m := decodeSample(t, lines[0])
	dur, ok := m["duration_ms"].(float64)
	if !ok {
		t.Fatalf("duration_ms is not float64; got %T", m["duration_ms"])
	}
	if dur < 0 {
		t.Errorf("duration_ms = %f; must be >= 0", dur)
	}
	// With injected clock: t1-t0 = 42 ms → duration_ms should be ~42.
	if dur < 40 || dur > 50 {
		t.Errorf("duration_ms = %f; expected ~42 with injected clock", dur)
	}
}

func TestMetricsHook_AppendTwoCallsTwoLines(t *testing.T) {
	h, path := newTestMetricsHook(t)

	for i, method := range []string{"create_vm", "delete_vm"} {
		ctx := h.Before(context.Background(), method, nil, jsonrpc.Context{RequestID: "req-" + method})
		_, _ = h.After(ctx, method, nil, nil)
		lines := readLines(t, path)
		if len(lines) != i+1 {
			t.Fatalf("after call %d expected %d lines; got %d", i+1, i+1, len(lines))
		}
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines total; got %d", len(lines))
	}
	m0 := decodeSample(t, lines[0])
	m1 := decodeSample(t, lines[1])
	if m0["method"] != "create_vm" {
		t.Errorf("line 0 method = %v; want create_vm", m0["method"])
	}
	if m1["method"] != "delete_vm" {
		t.Errorf("line 1 method = %v; want delete_vm", m1["method"])
	}
}

func TestMetricsHook_WriteFailureDoesNotReturnError(t *testing.T) {
	dir := t.TempDir()
	// Create a directory at the metrics path — writes to it will fail.
	badPath := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(badPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var warnBuf nopWriter
	logger, _ := log.NewLogger("warn", &warnBuf)

	h, err := hooks.NewMetricsHook(hooks.Deps{
		Logger:  logger,
		Metrics: &hooks.MetricsConfig{Enabled: true, FilePath: badPath},
	})
	if err != nil {
		t.Fatalf("NewMetricsHook: %v", err)
	}

	ctx := h.Before(context.Background(), "create_vm", nil, jsonrpc.Context{})
	// Write will fail because badPath is a directory, but After must NOT return an error.
	_, gotErr := h.After(ctx, "create_vm", "vm-id", nil)
	if gotErr != nil {
		t.Errorf("write failure must not return an error to caller; got %v", gotErr)
	}
}

func TestNewMetricsHook_ValidationErrors(t *testing.T) {
	logger, _ := log.NewLogger("warn", &nopWriter{})

	// Nil Deps.Metrics must fail.
	if _, err := hooks.NewMetricsHook(hooks.Deps{Logger: logger}); err == nil {
		t.Error("expected error when Deps.Metrics is nil")
	}

	// Enabled but empty file_path must fail.
	_, err := hooks.NewMetricsHook(hooks.Deps{
		Logger:  logger,
		Metrics: &hooks.MetricsConfig{Enabled: true, FilePath: ""},
	})
	if err == nil {
		t.Error("expected error when file_path is empty")
	}

	// Enabled but whitespace-only file_path must fail.
	_, err = hooks.NewMetricsHook(hooks.Deps{
		Logger:  logger,
		Metrics: &hooks.MetricsConfig{Enabled: true, FilePath: "   "},
	})
	if err == nil {
		t.Error("expected error when file_path is whitespace only")
	}
}
