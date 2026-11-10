package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// vmServer answers the reads has_vm makes for VM 100, which carries the
// bosh-cpi tag, and counts the requests it served.
func vmServer(t *testing.T) (host string, port int, requests *atomic.Int32) {
	t.Helper()
	requests = new(atomic.Int32)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster/resources"):
			_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve-b",` +
				`"status":"running","tags":"bosh-cpi"}]}`))
		case strings.HasSuffix(r.URL.Path, "/qemu/100/pending"):
			_, _ = w.Write([]byte(`{"data":[{"key":"tags","value":"bosh-cpi"}]}`))
		case strings.HasSuffix(r.URL.Path, "/qemu/100/config"):
			_, _ = w.Write([]byte(`{"data":{"tags":"bosh-cpi"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), port, requests
}

// hasVMStderr runs has_vm at the default log level, routed by the request
// context to the cluster the extra context keys name, and returns what the
// call wrote to stderr.
func hasVMStderr(t *testing.T, context string) string {
	t.Helper()
	cfgPath := writeOTelWiringConfigAtLevel(t, "info", "")
	request := `{"method":"has_vm","arguments":["100"],"context":{"request_id":"hv-1",` + context +
		`},"api_version":2}` + "\n"
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{"--config", cfgPath}, strings.NewReader(request), &stdout, &stderr, runOptions{})
	if code != 0 {
		t.Fatalf("runWithArgs exit %d: %s", code, stderr.String())
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	if resp.Error != nil || resp.Result != true {
		t.Fatalf("has_vm = %+v (error %+v), want true", resp.Result, resp.Error)
	}
	return stderr.String()
}

// endpointLines returns the messages of the lines the endpoint pool and the
// cache write, which are the ones that begin with "pve endpoint", "endpoint
// cache", or "api endpoint discovery off". The discovery worker's own result
// lines are the worker's business and are left out.
func endpointLines(t *testing.T, stderr string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		for _, prefix := range []string{"pve endpoint", "endpoint cache", "api endpoint discovery off"} {
			if strings.HasPrefix(rec.Msg, prefix) {
				out = append(out, rec.Msg)
			}
		}
	}
	return out
}

// A token client with one host runs with discovery on by default. At the
// default log level the call writes none of the pool and cache lines, and a
// failover is the one event that adds a line.
func TestDefaultLevelCallLogsNoNewLineUnlessAFailoverHappens(t *testing.T) {
	host, port, requests := vmServer(t)
	token := `"pve_user":"root","pve_api_token":"root@pam!b=secret","pve_verify_ssl":false`

	one := hasVMStderr(t, fmt.Sprintf(`"pve_host":%q,"pve_port":%d,%s`, host, port, token))
	if lines := endpointLines(t, one); len(lines) != 0 {
		t.Errorf("a default-level call with discovery on logged %q", lines)
	}
	if requests.Load() == 0 {
		t.Fatal("the cluster saw no request, so the call proved nothing")
	}

	live := net.JoinHostPort(host, strconv.Itoa(port))
	dead := net.JoinHostPort(host, strconv.Itoa(closedPort(t)))
	failed := hasVMStderr(t, fmt.Sprintf(`"pve_host":%q,"pve_port":%d,"pve_endpoints":[%q,%q],%s,`+
		`"pve_api_endpoint_discovery":"on"`, host, closedPortOf(t, dead), dead, live, token))
	lines := endpointLines(t, failed)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "pve endpoint failover:") {
		t.Errorf("a call that failed over logged %q, want the one failover line", lines)
	}
}

// stderrLines returns the level and message of every line the call wrote to
// stderr, in order. The time, the request id, and the other fields change from
// call to call and are left out.
func stderrLines(t *testing.T, stderr string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stderr line %q is not JSON: %v", line, err)
		}
		out = append(out, rec.Level+" "+rec.Msg)
	}
	return out
}

// The default log level shows a call with discovery on exactly what it shows
// with discovery off, except for the one line the worker writes when its first
// discovery finds nothing to cache. The second call, against the file the first
// one left, shows nothing more than the baseline.
func TestDefaultLevelStderrWithDiscoveryMatchesTheDiscoveryOffBaseline(t *testing.T) {
	host, port, requests := vmServer(t)
	token := `"pve_user":"root","pve_api_token":"root@pam!b=secret","pve_verify_ssl":false`
	endpoint := fmt.Sprintf(`"pve_host":%q,"pve_port":%d,%s`, host, port, token)

	baseline := stderrLines(t, hasVMStderr(t, endpoint+`,"pve_api_endpoint_discovery":"off"`))
	cold := stderrLines(t, hasVMStderr(t, endpoint))
	warm := stderrLines(t, hasVMStderr(t, endpoint))
	if requests.Load() == 0 {
		t.Fatal("the cluster saw no request, so the calls proved nothing")
	}

	const refresh = "INFO api endpoint discovery: no result through "
	var extra, rest []string
	for _, line := range cold {
		if strings.HasPrefix(line, refresh) {
			extra = append(extra, line)
		} else {
			rest = append(rest, line)
		}
	}
	if len(extra) != 1 {
		t.Errorf("the cold call wrote %d worker result lines %q, want the one for its first discovery",
			len(extra), extra)
	}
	if !slices.Equal(rest, baseline) {
		t.Errorf("the cold call wrote %q besides the worker's result, want the baseline %q", rest, baseline)
	}
	if !slices.Equal(warm, baseline) {
		t.Errorf("the warm call wrote %q, want the baseline %q", warm, baseline)
	}
}

// closedPortOf returns the port of a host:port address.
func closedPortOf(t *testing.T, address string) int {
	t.Helper()
	_, text, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(text)
	if err != nil {
		t.Fatal(err)
	}
	return port
}
