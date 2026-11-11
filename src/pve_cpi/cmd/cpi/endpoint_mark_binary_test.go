package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// binaryRun is what one run of the built binary left behind.
type binaryRun struct {
	stdout string
	// lines are the level and message of each line on stderr.
	lines []string
	code  int
}

// runCPIBinary runs the built binary once with a has_vm request on stdin, the
// way the Director runs it, and waits for the process to exit.
func runCPIBinary(t *testing.T, bin, cfgFile string) binaryRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--config", cfgFile)
	cmd.Stdin = strings.NewReader(`{"method":"has_vm","arguments":["100"],"context":{"request_id":"mark-1"},` +
		`"api_version":2}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the binary failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	return binaryRun{stdout: stdout.String(), lines: stderrLines(t, stderr.String()), code: cmd.ProcessState.ExitCode()}
}

// has reports whether a line of the run starts with prefix, and returns it.
func (r binaryRun) has(prefix string) (string, bool) {
	for _, line := range r.lines {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

// TestADownMarkWrittenBeforeExitIsReadByTheNextProcess runs the built binary
// twice over one cache directory, against a list whose first endpoint refuses
// connections and whose second answers. The first process fails over, and the
// drains registered in main write the mark before it exits, so the file is on
// disk when the process is gone. The second process reads the mark, says that
// it starts on the live endpoint, and never dials the dead one first. A change
// that stopped the first process from writing the mark, or wrote it under a key
// the next process does not read, would leave the second run without the line.
// Whether the drain waits for a slow write is the drain tests' contract, which
// this test cannot observe without making the write slow.
func TestADownMarkWrittenBeforeExitIsReadByTheNextProcess(t *testing.T) {
	var liveHits atomic.Int32
	live := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		liveHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve1",` +
				`"status":"running","tags":"bosh-cpi"}]}`))
		case strings.HasSuffix(r.URL.Path, "/qemu/100/pending"):
			_, _ = w.Write([]byte(`{"data":[{"key":"tags","value":"bosh-cpi"}]}`))
		case strings.HasSuffix(r.URL.Path, "/qemu/100/config"):
			_, _ = w.Write([]byte(`{"data":{"tags":"bosh-cpi"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	}))
	t.Cleanup(live.Close)
	liveAddr := live.Listener.Addr().String()
	dead := net.JoinHostPort("127.0.0.1", fmt.Sprint(closedPort(t)))
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{
  "host": "127.0.0.1",
  "port": %d,
  "user": "root@pam",
  "api_token": "root@pam!cpi=0123456789abcdef",
  "endpoints": [%q, %q],
  "api_endpoint_discovery": "on",
  "endpoint_cache_dir": %q,
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "info",
  "verify_ssl": false,
  "pve_api_dial_timeout_sec": 1,
  "retry": {"transient": {"max_attempts": 1}}
}`, closedPortOf(t, dead), dead, liveAddr, dir)
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, err := buildOnce(t)
	if err != nil {
		t.Fatalf("buildOnce: %v", err)
	}

	first := runCPIBinary(t, bin, cfgFile)

	if first.code != 0 || !strings.Contains(first.stdout, `"result":true`) {
		t.Fatalf("first process: exit %d, stdout %s; want has_vm to answer true from the live endpoint",
			first.code, first.stdout)
	}
	if line, ok := first.has("WARN pve endpoint failover:"); !ok || !strings.Contains(line, dead) {
		t.Fatalf("first process lines %q, want the failover Warn that names the dead endpoint %s", first.lines, dead)
	}
	if _, ok := first.has("INFO pve endpoints: starting on"); ok {
		t.Errorf("the first process started away from the dead endpoint before anything marked it: %q", first.lines)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cache files after the first process = %v (%v), want the one the mark was written to", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !bytes.Contains(raw, []byte(dead)) {
		t.Fatalf("cache file %s does not carry the mark for %s: %s", files[0], dead, raw)
	}
	liveBefore := liveHits.Load()

	second := runCPIBinary(t, bin, cfgFile)

	if second.code != 0 || !strings.Contains(second.stdout, `"result":true`) {
		t.Fatalf("second process: exit %d, stdout %s; want has_vm to answer true", second.code, second.stdout)
	}
	want := "INFO pve endpoints: starting on " + liveAddr + " because " + dead + " was marked down"
	if line, ok := second.has(want); !ok {
		t.Errorf("second process lines %q, want one that starts %q", second.lines, want)
	} else if !strings.HasSuffix(line, " ago") {
		t.Errorf("the started-away line %q should end with the mark's age", line)
	}
	if _, ok := second.has("WARN pve endpoint failover:"); ok {
		t.Errorf("the second process failed over again, so it dialed the dead endpoint first: %q", second.lines)
	}
	if liveHits.Load() == liveBefore {
		t.Error("the live endpoint saw no request from the second process")
	}
}
