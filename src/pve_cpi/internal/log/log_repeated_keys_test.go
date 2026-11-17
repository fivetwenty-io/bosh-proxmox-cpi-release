package log_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// lineKeys returns the top-level keys of one JSON log line in order, with
// every repeat, which a map decode would hide.
func lineKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("line %q does not open an object: %v", line, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
	}
	return keys
}

// repeatedKeys returns the keys a JSON line carries more than once.
func repeatedKeys(t *testing.T, line string) []string {
	t.Helper()
	seen := map[string]int{}
	var out []string
	for _, k := range lineKeys(t, line) {
		seen[k]++
		if seen[k] == 2 {
			out = append(out, k)
		}
	}
	return out
}

func newJSONLogger(t *testing.T) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	return m
}

func TestWithLayersDoNotRepeatAKey(t *testing.T) {
	t.Parallel()
	l, buf := newJSONLogger(t)
	// The handler, the HA helper, and the replica policy each attach the VMID
	// and the node they know, and the call names the VMID again.
	inner := l.With(log.Int("vmid", 210578)).
		With(log.Int("vmid", 210578), log.String("node", "n0")).
		With(log.Int("vmid", 210578), log.String("node", "n0"), log.Any("volids", []string{"a", "b"}))
	inner.Info("widen", log.Int("vmid", 210578), log.Any("volids", []string{"a", "b"}), log.String("job", "1-0"))
	line := strings.TrimSpace(buf.String())
	if rep := repeatedKeys(t, line); len(rep) != 0 {
		t.Fatalf("line %s repeats %v", line, rep)
	}
	m := decodeLine(t, line)
	if m["vmid"] != float64(210578) || m["node"] != "n0" || m["job"] != "1-0" {
		t.Fatalf("line %s lost a value", line)
	}
}

func TestWithKeepsADifferentValueUnderADistinctKey(t *testing.T) {
	t.Parallel()
	l, buf := newJSONLogger(t)
	l.With(log.Int("vmid", 100), log.String("node", "n1")).
		With(log.Int("vmid", 9001)).
		Warn("parker", log.Int("vmid", 9002), log.Int("vmid", 100), log.String("node", "n2"), log.Int("vmid", 9001))
	line := strings.TrimSpace(buf.String())
	if rep := repeatedKeys(t, line); len(rep) != 0 {
		t.Fatalf("line %s repeats %v", line, rep)
	}
	m := decodeLine(t, line)
	want := map[string]any{
		"vmid": float64(100), "vmid_2": float64(9001), "vmid_3": float64(9002), "node": "n1", "node_2": "n2",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("line %s: %s = %v, want %v", line, k, m[k], v)
		}
	}
	if _, extra := m["vmid_4"]; extra {
		t.Errorf("line %s: a value already held got another key", line)
	}
}

func TestWithContextKeepsTheHeldFields(t *testing.T) {
	t.Parallel()
	l, buf := newJSONLogger(t)
	held := l.With(log.Int("vmid", 7))
	held.WithContext(t.Context()).Info("ctx", log.Int("vmid", 7))
	line := strings.TrimSpace(buf.String())
	if rep := repeatedKeys(t, line); len(rep) != 0 {
		t.Fatalf("line %s repeats %v", line, rep)
	}
}
