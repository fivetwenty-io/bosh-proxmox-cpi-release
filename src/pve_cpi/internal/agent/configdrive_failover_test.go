package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// These tests drive the configdrive upload over real sockets, through a client
// built from a resolved multi-endpoint config so every request runs through the
// endpoint pool's RoundTripper. The target node pve2 has no entry in the pool.
// When a test routes the upload there, the routed address is a server outside
// the pool. None of them touches a live cluster.
//
// Releasing a call's affinity scope clears the scopeless tethers of every pool
// in the process, so these tests are all serial.

const (
	cdfNode      = "pve2"
	cdfFilename  = "vm-100-cloudinit.iso"
	cdfUploadURL = "/nodes/pve2/storage/local/upload"
	cdfContent   = "/nodes/pve2/storage/local/content/"
	cdfUPID      = "UPID:pve2:00002345:00000000:66aabbcc:imgcopy:local:root@pam:"
	cdfStatusURL = "/nodes/pve2/tasks/" + cdfUPID + "/status"
	cdfUploadPOS = "POST " + cdfUploadURL
)

func cdfLab(t *testing.T) *pvetest.Leaf {
	t.Helper()
	return pvetest.NewCA(t).NewLeaf(t, pvetest.LeafOptions{})
}

// cdfClosedAddr returns the address of a loopback port that refuses
// connections. It binds a port, closes it, and hands out the address, so the
// port is free again from then on. This file must stay serial, because a
// parallel test could bind the port in that window. No test here calls
// t.Parallel.
func cdfClosedAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

func cdfClient(t *testing.T, addrs ...string) pve.Client {
	t.Helper()
	host, portText, err := net.SplitHostPort(addrs[0])
	if err != nil {
		t.Fatalf("split %q: %v", addrs[0], err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port of %q: %v", addrs[0], err)
	}
	verify := false
	c, err := pve.NewClientWithTracer(&config.CPIConfig{
		Host:      host,
		Port:      port,
		Endpoints: addrs,
		APIToken:  "root@pam!test=tok-failover",
		VerifySSL: &verify,
	}, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	return c
}

// cdfCallDeadline bounds every call a wire test makes, so that a regression in
// the poll phase fails the test in seconds and does not wait for the task
// deadline, which is hundreds of seconds.
const cdfCallDeadline = 10 * time.Second

// cdfCtx returns a call context with its own affinity scope released when the
// test ends, a zero backoff, a one-millisecond SDK retry delay, and a deadline
// of cdfCallDeadline.
func cdfCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, release := endpointpool.WithAffinityScope(context.Background())
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(ctx, cdfCallDeadline)
	t.Cleanup(cancel)
	ctx = pve.WithTestBackoff(ctx, func(int) time.Duration { return 0 })
	return sdkclient.WithRetryDelay(ctx, time.Millisecond)
}

// cdfMutations lists every mutation the server read as "METHOD path", in
// arrival order and without the API prefix.
func cdfMutations(s *pvetest.Server) []string {
	seen := s.Seen()
	out := make([]string, 0, len(seen))
	for _, req := range seen {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		}
		out = append(out, req.Method+" "+strings.TrimPrefix(req.Path, pvetest.APIPrefix))
	}
	return out
}

// cdfUploads counts the uploads among the mutations in got.
func cdfUploads(got []string) int {
	n := 0
	for _, g := range got {
		if g == cdfUploadPOS {
			n++
		}
	}
	return n
}

func cdfNoMutations(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := cdfMutations(s); len(got) != 0 {
		t.Errorf("%s read mutations %v, want none", name, got)
	}
}

// cdfOnlyUploadAndSweep fails the test when got holds anything but the upload
// and the content deletes of the pre-retry sweep.
func cdfOnlyUploadAndSweep(t *testing.T, name string, got []string) {
	t.Helper()
	for _, m := range got {
		if m != cdfUploadPOS && !strings.HasPrefix(m, "DELETE "+cdfContent) {
			t.Errorf("%s read %q, want only the upload and the pre-retry sweep", name, m)
		}
	}
}

// cdfDropAfterBody reads the whole request and resets the connection with no
// response, so the client's read fails after every byte it sent.
func cdfDropAfterBody(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, isTLS := conn.(*tls.Conn); isTLS {
		conn = tc.NetConn()
	}
	if tcp, isTCP := conn.(*net.TCPConn); isTCP {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func cdfAnswerUPID(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":"` + cdfUPID + `"}`))
}

func cdfFirstThen(first, then http.HandlerFunc) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first(w, r)
			return
		}
		then(w, r)
	}
}

// cdfRoutes answers an upload with onUpload, the pre-retry sweep with an empty
// success, and the upload task's status with a finished task.
func cdfRoutes(onUpload http.HandlerFunc) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodPost, cdfUploadURL, onUpload)
	routes.JSON(http.MethodDelete, cdfContent, http.StatusOK, nil)
	routes.JSON(http.MethodGet, cdfStatusURL, http.StatusOK, map[string]string{"status": "stopped", "exitstatus": "OK"})
	return routes
}

func cdfISO(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), cdfFilename)
	if err := os.WriteFile(p, []byte("iso9660-bytes"), 0o600); err != nil {
		t.Fatalf("write iso: %v", err)
	}
	return p
}

// cdfAgent returns the agent over client, with its uploads for pve2 routed to
// direct, or unrouted when direct is empty.
func cdfAgent(client pve.Client, direct string) *ConfigDrive {
	cd := newConfigDriveForTest(client, "local", log.NewNopLogger())
	if direct != "" {
		cd.nodeEndpoints = pve.NewNodeEndpointResolver(nil, map[string]string{cdfNode: direct}, "pve1.example.com",
			false, log.NewNopLogger())
	}
	return cd
}

// A routed upload that was dropped after its body started goes back to the
// direct host, and the pool's servers read no mutation at all, because even
// the pre-retry sweep follows the pin.
func TestConfigDriveUploadRoutedDropAfterBodyRetriesOnTheDirectHost(t *testing.T) {
	t.Cleanup(pve.SetTaskPollingForTest(1, 1, 0))
	leaf := cdfLab(t)
	direct := pvetest.NewLive(t, leaf, cdfRoutes(cdfFirstThen(cdfDropAfterBody, cdfAnswerUPID)))
	a := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	b := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	cd := cdfAgent(cdfClient(t, a.Addr, b.Addr), direct.Addr)

	if err := cd.uploadISO(cdfCtx(t), cdfNode, cdfISO(t), cdfFilename); err != nil {
		t.Fatalf("uploadISO: %v", err)
	}
	got := cdfMutations(direct)
	if n := cdfUploads(got); n != 2 {
		t.Errorf("the direct host read %d uploads, want 2 (%v)", n, got)
	}
	cdfNoMutations(t, "the first pool server", a)
	cdfNoMutations(t, "the second pool server", b)
	if _, ok := cd.nodeEndpoints.HostFor(cdfCtx(t), cdfNode); !ok {
		t.Error("the direct route was marked failed, so the loop fell back")
	}
}

// A routed upload whose dial or handshake never completes falls back to the
// proxied path, because nothing was sent, and the pool's active server reads
// the one upload.
func TestConfigDriveUploadRoutedConnectFailureFallsBackToTheProxiedPath(t *testing.T) {
	t.Cleanup(pve.SetTaskPollingForTest(1, 1, 0))
	leaf := cdfLab(t)
	cases := map[string]func(*testing.T) string{
		"refused dial":    cdfClosedAddr,
		"handshake reset": func(t *testing.T) string { return pvetest.NewReset(t).Addr },
	}
	for name, direct := range cases {
		t.Run(name, func(t *testing.T) {
			a := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
			b := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
			cd := cdfAgent(cdfClient(t, a.Addr, b.Addr), direct(t))

			if err := cd.uploadISO(cdfCtx(t), cdfNode, cdfISO(t), cdfFilename); err != nil {
				t.Fatalf("uploadISO: %v", err)
			}
			got := cdfMutations(a)
			if n := cdfUploads(got); n != 1 {
				t.Errorf("the first pool server read %d uploads, want 1 (%v)", n, got)
			}
			cdfOnlyUploadAndSweep(t, "the first pool server", got)
			cdfNoMutations(t, "the second pool server", b)
			if host, ok := cd.nodeEndpoints.HostFor(cdfCtx(t), cdfNode); ok {
				t.Errorf("the direct route %q is still in use, want it marked failed", host)
			}
		})
	}
}

// A fault from polling the finished upload's task falls back at once, and the
// loop awaits the same task through the pool. The upload is never sent again.
func TestConfigDriveUploadRoutedPollFaultFallsBackAndAwaitsTheSameTask(t *testing.T) {
	t.Cleanup(pve.SetTaskPollingForTest(1, 1, 0))
	leaf := cdfLab(t)
	directRoutes := cdfRoutes(cdfAnswerUPID)
	directRoutes.HandleFunc(http.MethodGet, cdfStatusURL, cdfDropAfterBody)
	direct := pvetest.NewLive(t, leaf, directRoutes)
	a := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	b := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	cd := cdfAgent(cdfClient(t, a.Addr, b.Addr), direct.Addr)

	if err := cd.uploadISO(cdfCtx(t), cdfNode, cdfISO(t), cdfFilename); err != nil {
		t.Fatalf("uploadISO: %v", err)
	}
	if got := cdfMutations(direct); len(got) != 1 || got[0] != cdfUploadPOS {
		t.Errorf("the direct host read %v, want the one upload", got)
	}
	cdfNoMutations(t, "the first pool server", a)
	cdfNoMutations(t, "the second pool server", b)
	if a.Counts.Method(http.MethodGet) == 0 {
		t.Error("the first pool server never read the task status, so the await did not fall back")
	}
	if host, ok := cd.nodeEndpoints.HostFor(cdfCtx(t), cdfNode); ok {
		t.Errorf("the direct route %q is still in use, want it marked failed", host)
	}
}

// With no direct route, an upload that is dropped after its body started stays
// tethered to the server that got it. Once that server is gone, every later
// attempt fails there, and the other server reads no upload.
func TestConfigDriveUploadMaybeSentStaysOnItsServer(t *testing.T) {
	t.Cleanup(pve.SetStorageUploadRetryForTest(3))
	leaf := cdfLab(t)
	var a *pvetest.Server
	a = pvetest.NewLive(t, leaf, cdfRoutes(func(w http.ResponseWriter, r *http.Request) {
		defer a.StopListening()
		cdfDropAfterBody(w, r)
	}))
	b := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	cd := cdfAgent(cdfClient(t, a.Addr, b.Addr), "")

	err := cd.uploadISO(cdfCtx(t), cdfNode, cdfISO(t), cdfFilename)
	if err == nil {
		t.Fatal("uploadISO succeeded, so a retry reached a server that never got the first upload")
	}
	if got := cdfUploads(cdfMutations(a)); got != 1 {
		t.Errorf("the first server read %d uploads, want 1", got)
	}
	if got := cdfUploads(cdfMutations(b)); got != 0 {
		t.Errorf("the second server read %d uploads, want 0", got)
	}
	// The pre-retry sweep is deliberately outside the upload's tether family, because
	// every entry hands the content delete to the node that owns the storage, so
	// its delete may reach the second server and nothing else may. The uploads
	// bullet in the spec gives that reasoning.
	cdfOnlyUploadAndSweep(t, "the second server", cdfMutations(b))
	if errors.Is(err, endpointpool.ErrTetherConflict) {
		t.Errorf("the failure is a tether conflict: %v", err)
	}
}

// An upload whose attempt fails in the connect phase releases its reservation
// and advances the active endpoint, so the loop's next attempt reaches the
// second server, which reads the one upload.
func TestConfigDriveUploadConnectFailureAdvancesToTheNextEndpoint(t *testing.T) {
	t.Cleanup(pve.SetTaskPollingForTest(1, 1, 0))
	leaf := cdfLab(t)
	b := pvetest.NewLive(t, leaf, cdfRoutes(cdfAnswerUPID))
	cd := cdfAgent(cdfClient(t, cdfClosedAddr(t), b.Addr), "")

	if err := cd.uploadISO(cdfCtx(t), cdfNode, cdfISO(t), cdfFilename); err != nil {
		t.Fatalf("uploadISO: %v", err)
	}
	if got := cdfMutations(b); len(got) != 1 || got[0] != cdfUploadPOS {
		t.Errorf("the second server read %v, want one upload", got)
	}
}
