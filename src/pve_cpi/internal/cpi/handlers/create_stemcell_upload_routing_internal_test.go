// Package handlers: white-box tests for uploadStemcellImage's direct-to-node
// routing: a Deps.NodeEndpoints resolver pins the upload POST (and its
// awaits/sweeps) to the target node's own pveproxy, and a TLS certificate
// verification failure on the pinned dial falls back to the configured
// endpoint within the same retry loop.
package handlers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// routeStorage records the CPI upload-host context stamp per Upload call.
type routeStorage struct {
	sdkstorage.Service

	mu       sync.Mutex
	hosts    []string
	uploadFn func(call int) (string, error)
}

func (s *routeStorage) Upload(ctx context.Context, _, _, _, _ string, body io.Reader) (string, error) {
	_, _ = io.ReadAll(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = append(s.hosts, pve.UploadHostFromContext(ctx))
	return s.uploadFn(len(s.hosts))
}

// routeResolver builds a resolver mapping node pve2 to host, discovery off;
// the client is never consulted for explicit entries, so nil is safe.
func routeResolver(host string) *pve.NodeEndpointResolver {
	return pve.NewNodeEndpointResolver(nil,
		map[string]string{"pve2": host}, "pve1.example.com", false, log.NewNopLogger())
}

func TestUploadStemcellImage_PinsUploadToMappedNode(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{uploadFn: func(int) (string, error) { return "", nil }}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("pve2.example.com")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if len(storage.hosts) != 1 || storage.hosts[0] != "pve2.example.com" {
		t.Errorf("upload ctx hosts = %v, want [pve2.example.com]", storage.hosts)
	}
}

func TestUploadStemcellImage_UnmappedNodeStaysUnpinned(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{uploadFn: func(int) (string, error) { return "", nil }}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("pve2.example.com")

	err := uploadStemcellImage(urCtx(), deps, "pve3", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if len(storage.hosts) != 1 || storage.hosts[0] != "" {
		t.Errorf("upload ctx hosts = %v, want one un-pinned call", storage.hosts)
	}
}

func TestUploadStemcellImage_NilResolverUnpinned(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{uploadFn: func(int) (string, error) { return "", nil }}
	deps := urDeps(storage)

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if len(storage.hosts) != 1 || storage.hosts[0] != "" {
		t.Errorf("upload ctx hosts = %v, want one un-pinned call", storage.hosts)
	}
}

// routeTasks records the CPI upload-host context stamp per Wait call and
// scripts outcomes by 1-based call count.
type routeTasks struct {
	sdktasks.Service
	mu     sync.Mutex
	hosts  []string
	waitFn func(call int, upid string) (*sdktasks.Status, error)
}

func (s *routeTasks) Wait(ctx context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = append(s.hosts, pve.UploadHostFromContext(ctx))
	if s.waitFn != nil {
		return s.waitFn(len(s.hosts), upid)
	}
	return &sdktasks.Status{ExitStatus: "OK"}, nil
}

func TestUploadStemcellImage_PinsAwaitToMappedNode(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	storage := &routeStorage{uploadFn: func(int) (string, error) { return upid, nil }}
	tasks := &routeTasks{}
	deps := urDepsWithTasks(storage, tasks)
	deps.NodeEndpoints = routeResolver("pve2.example.com")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage: %v", err)
	}
	if len(storage.hosts) != 1 || storage.hosts[0] != "pve2.example.com" {
		t.Errorf("upload ctx hosts = %v, want [pve2.example.com]", storage.hosts)
	}
	if len(tasks.hosts) != 1 || tasks.hosts[0] != "pve2.example.com" {
		t.Errorf("await ctx hosts = %v, want [pve2.example.com] (the UPID await rides the pinned ctx)", tasks.hosts)
	}
}

func TestUploadStemcellImage_TLSFailureAfterPostResolvesPriorTask(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve2:00001234:00000000:66aabbcc:imgcopy:local:root@pam:"
	storage := &routeStorage{uploadFn: func(int) (string, error) { return upid, nil }}
	tasks := &routeTasks{
		waitFn: func(call int, _ string) (*sdktasks.Status, error) {
			if call == 1 {
				return nil, fmt.Errorf("Get %q: %w",
					"https://10.0.0.2:8006/api2/json/nodes/pve2/tasks",
					x509.HostnameError{Certificate: &x509.Certificate{}, Host: "10.0.0.2"})
			}
			return &sdktasks.Status{ExitStatus: "OK"}, nil
		},
	}
	deps := urDepsWithTasks(storage, tasks)
	deps.NodeEndpoints = routeResolver("10.0.0.2")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the prior-task await: %v", err)
	}
	// The POST went through before the pinned await failed, so the fallback
	// re-run must resolve the pending task un-pinned, never re-POST.
	if len(storage.hosts) != 1 || storage.hosts[0] != "10.0.0.2" {
		t.Errorf("upload ctx hosts = %v, want the single pinned POST", storage.hosts)
	}
	if len(tasks.hosts) != 2 || tasks.hosts[0] != "10.0.0.2" || tasks.hosts[1] != "" {
		t.Errorf("await ctx hosts = %v, want [10.0.0.2 \"\"]", tasks.hosts)
	}
}

func TestUploadStemcellImage_DialFailureFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{}
	storage.uploadFn = func(call int) (string, error) {
		if call == 1 {
			return "", &sdkerrors.ConnectionError{
				Host: "10.0.0.2", Port: 8006, Message: "failed to establish a connection",
				Cause: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			}
		}
		return "", nil
	}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("10.0.0.2")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the endpoint fallback: %v", err)
	}
	if len(storage.hosts) != 2 || storage.hosts[0] != "10.0.0.2" || storage.hosts[1] != "" {
		t.Errorf("upload ctx hosts = %v, want [10.0.0.2 \"\"]", storage.hosts)
	}
	// The dead route is memoized for the rest of the process (the replication
	// fan-out's later uploads to this node skip the failing dial).
	if _, ok := deps.NodeEndpoints.HostFor(context.Background(), "pve2"); ok {
		t.Error("HostFor(pve2) after the fallback: expected the failed route to be memoized away")
	}
}

func TestUploadStemcellImage_TLSFailureFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{}
	storage.uploadFn = func(call int) (string, error) {
		if call == 1 {
			return "", fmt.Errorf("Post %q: %w",
				"https://10.0.0.2:8006/api2/json/nodes/pve2/storage/stemcells/upload",
				x509.HostnameError{Certificate: &x509.Certificate{}, Host: "10.0.0.2"})
		}
		return "", nil
	}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("10.0.0.2")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the endpoint fallback: %v", err)
	}
	if len(storage.hosts) != 2 || storage.hosts[0] != "10.0.0.2" || storage.hosts[1] != "" {
		t.Errorf("upload ctx hosts = %v, want [10.0.0.2 \"\"]", storage.hosts)
	}
}

// TestUploadStemcellImage_NonVerificationTLSHandshakeFallsBackToEndpoint is
// the F-01 regression: a TLS handshake failure that is NOT certificate
// verification (a non-TLS listener answering the routed port) must still
// degrade to the proxied path instead of failing create_stemcell
// permanently. Before the fix neither IsTLSCertVerificationFailure nor
// IsDirectDialFailure matched tls.RecordHeaderError.
func TestUploadStemcellImage_NonVerificationTLSHandshakeFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{}
	storage.uploadFn = func(call int) (string, error) {
		if call == 1 {
			return "", fmt.Errorf("Post %q: %w",
				"https://10.0.0.13:22/api2/json/nodes/pve2/storage/stemcells/upload",
				tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})
		}
		return "", nil
	}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("10.0.0.13:22")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the endpoint fallback: %v", err)
	}
	if len(storage.hosts) != 2 || storage.hosts[0] != "10.0.0.13:22" || storage.hosts[1] != "" {
		t.Errorf("upload ctx hosts = %v, want [10.0.0.13:22 \"\"]", storage.hosts)
	}
}

// TestUploadStemcellImage_HandshakePhaseReadTimeoutFallsBackToEndpoint is the
// F-02 regression: a read-phase net.OpError (the shape a stalled or reset
// TLS handshake produces once the TCP connect already succeeded) must be
// fallback-eligible on the FIRST attempt rather than retried pinned to the
// same dead route for the whole upload budget.
func TestUploadStemcellImage_HandshakePhaseReadTimeoutFallsBackToEndpoint(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{}
	storage.uploadFn = func(call int) (string, error) {
		if call == 1 {
			return "", &net.OpError{Op: "read", Net: "tcp", Err: stemcellRoutingTimeoutErr{}}
		}
		return "", nil
	}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("10.0.0.2")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the endpoint fallback: %v", err)
	}
	// Exactly 2 attempts: fall back on the FIRST pinned failure.
	if len(storage.hosts) != 2 || storage.hosts[0] != "10.0.0.2" || storage.hosts[1] != "" {
		t.Errorf("upload ctx hosts = %v, want [10.0.0.2 \"\"]", storage.hosts)
	}
}

// stemcellRoutingTimeoutErr implements net.Error with Timeout()==true, the
// shape net.OpError.Timeout() delegates to, without needing a real syscall
// error.
type stemcellRoutingTimeoutErr struct{}

func (stemcellRoutingTimeoutErr) Error() string   { return "i/o timeout" }
func (stemcellRoutingTimeoutErr) Timeout() bool   { return true }
func (stemcellRoutingTimeoutErr) Temporary() bool { return true }
