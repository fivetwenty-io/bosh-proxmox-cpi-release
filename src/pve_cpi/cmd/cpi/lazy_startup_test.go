package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// dialCounter listens on a loopback port and counts the connections made to
// it, standing in for a job-level PVE host that must never be contacted. It
// accepts and immediately closes each connection so a stray dial fails fast
// instead of hanging.
type dialCounter struct {
	ln    net.Listener
	count atomic.Int32
	wg    sync.WaitGroup
}

func newDialCounter(t *testing.T) *dialCounter {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &dialCounter{ln: ln}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			d.count.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		d.wg.Wait()
	})
	return d
}

func (d *dialCounter) port() int { return d.ln.Addr().(*net.TCPAddr).Port } //nolint:forcetypeassert // loopback TCP listener

func runRequest(t *testing.T, cfgPath, request string) jsonrpc.Response {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithArgs([]string{"--config", cfgPath}, strings.NewReader(request+"\n"), &stdout, &stderr, runOptions{})
	if code != 0 {
		t.Fatalf("runWithArgs exit %d: %s", code, stderr.String())
	}
	var resp jsonrpc.Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	return resp
}

// `info` answers from constants. Password auth, the default pool checks, and
// the follow-vm-storage lookup are all on for this config, and none of them may
// reach the job-level host before or during the call.
func TestInfoTouchesNoPVEAPI(t *testing.T) {
	job := newDialCounter(t)
	cfgPath := writeOTelWiringConfig(t, fmt.Sprintf(`,"port":%d`, job.port()))

	resp := runRequest(t, cfgPath, `{"method":"info","arguments":[],"context":{"request_id":"info-1"},"api_version":2}`)
	if resp.Error != nil {
		t.Fatalf("info returned an error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok || result["api_version"] == nil {
		t.Fatalf("info result = %#v, want a map carrying api_version", resp.Result)
	}
	if got := job.count.Load(); got != 0 {
		t.Errorf("job-level host was dialed %d times, want 0", got)
	}
}

// A request routed by cpi_config to cluster B must not depend on the job
// cluster at all: the job host stays untouched while B answers.
func TestRoutedRequestIgnoresJobCluster(t *testing.T) {
	job := newDialCounter(t)
	var bRequests atomic.Int32
	clusterB := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/cluster/resources") {
			_, _ = w.Write([]byte(`{"data":[{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve-b","status":"running"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(clusterB.Close)
	bURL, err := url.Parse(clusterB.URL)
	if err != nil {
		t.Fatal(err)
	}
	bPort, err := strconv.Atoi(bURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	cfgPath := writeOTelWiringConfig(t, fmt.Sprintf(`,"port":%d`, job.port()))
	request := fmt.Sprintf(`{"method":"has_vm","arguments":["100"],"context":{"request_id":"hv-1","pve_host":%q,"pve_port":%d,`+
		`"pve_user":"root","pve_api_token":"root@pam!b=secret","pve_verify_ssl":false},"api_version":2}`, bURL.Hostname(), bPort)

	resp := runRequest(t, cfgPath, request)
	if resp.Error != nil {
		t.Fatalf("has_vm returned an error: %+v", resp.Error)
	}
	if resp.Result != true {
		t.Fatalf("has_vm result = %#v, want true", resp.Result)
	}
	if bRequests.Load() == 0 {
		t.Error("cluster B saw no requests; the call did not reach the targeted cluster")
	}
	if got := job.count.Load(); got != 0 {
		t.Errorf("job-level host was dialed %d times, want 0", got)
	}
}
