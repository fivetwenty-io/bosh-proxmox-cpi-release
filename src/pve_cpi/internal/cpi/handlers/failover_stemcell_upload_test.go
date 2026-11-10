package handlers

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// These tests drive uploadStemcellImage over real sockets. The target node
// pve2 has no entry in the endpoint pool. Its own address, when a test routes
// the upload there, is a server outside the pool.

const (
	fwUploadNode = "pve2"
	fwUploadPath = "/nodes/pve2/storage/stemcells/upload"
	fwContentDir = "/nodes/pve2/storage/stemcells/content/"
	fwUPID       = "UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	fwStatusPath = "/nodes/pve2/tasks/" + fwUPID + "/status"
	fwUploadPOST = "POST " + fwUploadPath
)

// fwUploadRoutes answers an upload with onUpload, a pre-retry sweep with 200,
// and the upload task's status with a finished task.
func fwUploadRoutes(onUpload http.HandlerFunc) *pvetest.Routes {
	routes := pvetest.NewRoutes()
	routes.HandleFunc(http.MethodPost, fwUploadPath, onUpload)
	routes.JSON(http.MethodDelete, fwContentDir, http.StatusOK, nil)
	routes.JSON(http.MethodGet, fwStatusPath, http.StatusOK, map[string]string{"status": "stopped", "exitstatus": "OK"})
	return routes
}

// fwAnswerUPID reads the upload body and answers the task identifier.
func fwAnswerUPID(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":"` + fwUPID + `"}`))
}

// fwRouteTo returns the resolver that sends uploads for pve2 straight to addr.
func fwRouteTo(addr string) *pve.NodeEndpointResolver {
	return pve.NewNodeEndpointResolver(nil, map[string]string{fwUploadNode: addr}, "pve1.example.com", false,
		log.NewNopLogger())
}

func fwStemcellDeps(client pve.Client, resolver *pve.NodeEndpointResolver) Deps {
	return Deps{PVE: client, Logger: log.NewNopLogger(), NodeEndpoints: resolver}
}

// fwUploadAttempts bounds the loop's attempts so a test that expects every
// attempt to fail ends quickly.
func fwUploadAttempts(t *testing.T, n int) {
	t.Helper()
	t.Cleanup(pve.SetStorageUploadRetryForTest(n))
}

// The routed fault the narrowing is about must be a read fault that the
// wrapper marks as possibly sent. Without the marker the same fault is
// fallback-eligible at the POST, so a test of the narrowing proves nothing
// when the fault is some other shape.
func TestStemcellUploadFailoverPremiseResetAfterBodyIsAMarkedReadFault(t *testing.T) {
	lab := newFWLab(t)
	direct := lab.live(t, fwUploadRoutes(fwDropAfterBody))
	a := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	client := fwClient(t, a.Addr, b.Addr)

	_, err := client.Storage().Upload(pve.UploadContext(fwCtx(t), direct.Addr), fwUploadNode, "stemcells", "import",
		"img.qcow2", strings.NewReader("qcow2-bytes"))
	if err == nil {
		t.Fatal("the upload succeeded against a server that resets the connection")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "read" {
		t.Fatalf("fault = %v, want a read fault", err)
	}
	if !endpointpool.IsMaybeSent(err) {
		t.Fatalf("fault = %v, want the wrapper's maybe-sent marker", err)
	}
	if pve.IsRoutedHandshakeFailure(err) {
		t.Fatal("a marked read fault must not count as a routed handshake failure")
	}
	if !pve.UploadFallbackEligible(err, pve.UploadPollPhase) {
		t.Fatal("the same fault at the poll phase must be fallback-eligible, because it only re-awaits the task")
	}
	if pve.UploadFallbackEligible(err, pve.UploadPostPhase) {
		t.Fatal("the same fault at the POST phase must not be fallback-eligible")
	}
}

// A routed upload that was dropped after its body started goes back to the
// direct host and never to the proxied path. The pool's servers read no
// mutation at all, because even the pre-retry sweep follows the pin.
func TestStemcellUploadRoutedDropAfterBodyRetriesOnTheDirectHost(t *testing.T) {
	fwFastPolling(t)
	lab := newFWLab(t)
	direct := lab.live(t, fwUploadRoutes(fwFirstThen(fwDropAfterBody, fwAnswerUPID)))
	a := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	deps := fwStemcellDeps(fwClient(t, a.Addr, b.Addr), fwRouteTo(direct.Addr))

	err := uploadStemcellImage(fwCtx(t), deps, fwUploadNode, "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	got := fwMutations(direct)
	if n := fwCount(got, fwUploadPOST); n != 2 {
		t.Errorf("the direct host read %d uploads, want 2 (%v)", n, got)
	}
	fwNoMutations(t, "the first pool server", a)
	fwNoMutations(t, "the second pool server", b)
	if _, ok := deps.NodeEndpoints.HostFor(fwCtx(t), fwUploadNode); !ok {
		t.Error("the direct route was marked failed, so the loop fell back")
	}
}

// A routed upload whose dial never completes falls back to the proxied path in
// the same attempt, because nothing was sent, and the pool's active server
// reads the one upload.
func TestStemcellUploadRoutedConnectFailureFallsBackToTheProxiedPath(t *testing.T) {
	fwFastPolling(t)
	lab := newFWLab(t)
	cases := map[string]func(*testing.T) string{
		"refused dial":    fwClosedAddr,
		"handshake reset": func(t *testing.T) string { return pvetest.NewReset(t).Addr },
	}
	for name, direct := range cases {
		t.Run(name, func(t *testing.T) {
			a := lab.live(t, fwUploadRoutes(fwAnswerUPID))
			b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
			deps := fwStemcellDeps(fwClient(t, a.Addr, b.Addr), fwRouteTo(direct(t)))

			err := uploadStemcellImage(fwCtx(t), deps, fwUploadNode, "stemcells", "img.qcow2", urImageFile(t), "")
			if err != nil {
				t.Fatalf("uploadStemcellImage: %v", err)
			}
			// A reset handshake counts as a dropped connection to the loop, which
			// sweeps the target name before it uploads again, so the sweep may
			// precede the upload. Nothing else is a mutation.
			got := fwMutations(a)
			if n := fwCount(got, fwUploadPOST); n != 1 {
				t.Errorf("the first pool server read %d uploads, want 1 (%v)", n, got)
			}
			for _, m := range got {
				if m != fwUploadPOST && !strings.HasPrefix(m, "DELETE "+fwContentDir) {
					t.Errorf("the first pool server read %q, want only the upload and the pre-upload sweep", m)
				}
			}
			fwNoMutations(t, "the second pool server", b)
			if host, ok := deps.NodeEndpoints.HostFor(fwCtx(t), fwUploadNode); ok {
				t.Errorf("the direct route %q is still in use, want it marked failed", host)
			}
		})
	}
}

// A fault from polling the finished upload's task falls back at once, and the
// loop awaits the same task through the pool. The upload is never sent again.
func TestStemcellUploadRoutedPollFaultFallsBackAndAwaitsTheSameTask(t *testing.T) {
	fwFastPolling(t)
	lab := newFWLab(t)
	directRoutes := fwUploadRoutes(fwAnswerUPID)
	directRoutes.HandleFunc(http.MethodGet, fwStatusPath, fwDropAfterBody)
	direct := lab.live(t, directRoutes)
	a := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	deps := fwStemcellDeps(fwClient(t, a.Addr, b.Addr), fwRouteTo(direct.Addr))

	err := uploadStemcellImage(fwCtx(t), deps, fwUploadNode, "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if got := fwMutations(direct); len(got) != 1 || got[0] != fwUploadPOST {
		t.Errorf("the direct host read %v, want the one upload", got)
	}
	fwNoMutations(t, "the first pool server", a)
	fwNoMutations(t, "the second pool server", b)
	if fwGets(a) == 0 {
		t.Error("the first pool server never read the task status, so the await did not fall back")
	}
	if host, ok := deps.NodeEndpoints.HostFor(fwCtx(t), fwUploadNode); ok {
		t.Errorf("the direct route %q is still in use, want it marked failed", host)
	}
}

// With no direct route, an upload that is dropped after its body started
// stays tethered to the server that got it. Once that server is gone, every
// later attempt fails there, and the other server reads no upload.
func TestStemcellUploadMaybeSentStaysOnItsServer(t *testing.T) {
	fwUploadAttempts(t, 3)
	lab := newFWLab(t)
	var a *pvetest.Server
	a = lab.live(t, fwUploadRoutes(func(w http.ResponseWriter, r *http.Request) {
		defer a.StopListening()
		fwDropAfterBody(w, r)
	}))
	b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	deps := fwStemcellDeps(fwClient(t, a.Addr, b.Addr), nil)

	err := uploadStemcellImage(fwCtx(t), deps, fwUploadNode, "stemcells", "img.qcow2", urImageFile(t), "")
	if err == nil {
		t.Fatal("uploadStemcellImage succeeded, so a retry reached a server that never got the first upload")
	}
	if got := fwCount(fwMutations(a), fwUploadPOST); got != 1 {
		t.Errorf("the first server read %d uploads, want 1", got)
	}
	if got := fwCount(fwMutations(b), fwUploadPOST); got != 0 {
		t.Errorf("the second server read %d uploads, want 0", got)
	}
	// The pre-retry sweep is deliberately outside the upload's tether family. Every
	// entry hands the content delete to the node that owns the storage, so the
	// entry it goes through changes neither which node deletes the file nor how
	// the delete races the first upload's task, and the sweep still clears the
	// partial file when the tethered entry is down. The uploads bullet in the
	// spec gives that reasoning. So the sweep's delete may reach the second
	// server and nothing else may.
	for _, m := range fwMutations(b) {
		if !strings.HasPrefix(m, "DELETE "+fwContentDir) {
			t.Errorf("the second server read %q, want at most the untethered sweep delete", m)
		}
	}
	if errors.Is(err, endpointpool.ErrTetherConflict) {
		t.Errorf("the failure is a tether conflict: %v", err)
	}
}

// An upload whose attempt fails in the connect phase releases its reservation
// and advances the active endpoint, so the loop's next attempt reaches the
// second server, which reads the one upload.
func TestStemcellUploadConnectFailureAdvancesToTheNextEndpoint(t *testing.T) {
	fwFastPolling(t)
	lab := newFWLab(t)
	dead := fwClosedAddr(t)
	b := lab.live(t, fwUploadRoutes(fwAnswerUPID))
	deps := fwStemcellDeps(fwClient(t, dead, b.Addr), nil)

	err := uploadStemcellImage(fwCtx(t), deps, fwUploadNode, "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if got := fwMutations(b); len(got) != 1 || got[0] != fwUploadPOST {
		t.Errorf("the second server read %v, want one upload", got)
	}
}
