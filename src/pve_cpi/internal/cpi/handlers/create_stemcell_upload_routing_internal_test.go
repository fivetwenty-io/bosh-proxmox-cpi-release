// Package handlers: white-box tests for uploadStemcellImage's direct-to-node
// routing: a Deps.NodeEndpoints resolver pins the upload POST (and its
// awaits/sweeps) to the target node's own pveproxy, and a TLS certificate
// verification failure on the pinned dial falls back to the configured
// endpoint within the same retry loop.
package handlers

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"sync"
	"testing"

	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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

// routeResolver builds a resolver with one explicit entry and discovery off;
// the client is never consulted for explicit entries, so nil is safe.
func routeResolver(node, host string) *pve.NodeEndpointResolver {
	return pve.NewNodeEndpointResolver(nil,
		map[string]string{node: host}, "pve1.example.com", false, log.NewNopLogger())
}

func TestUploadStemcellImage_PinsUploadToMappedNode(t *testing.T) {
	t.Parallel()

	storage := &routeStorage{uploadFn: func(int) (string, error) { return "", nil }}
	deps := urDeps(storage)
	deps.NodeEndpoints = routeResolver("pve2", "pve2.example.com")

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
	deps.NodeEndpoints = routeResolver("pve2", "pve2.example.com")

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
	deps.NodeEndpoints = routeResolver("pve2", "10.0.0.2")

	err := uploadStemcellImage(urCtx(), deps, "pve2", "stemcells", "img.qcow2", urImageFile(t), "")
	if err != nil {
		t.Fatalf("uploadStemcellImage must succeed via the endpoint fallback: %v", err)
	}
	if len(storage.hosts) != 2 || storage.hosts[0] != "10.0.0.2" || storage.hosts[1] != "" {
		t.Errorf("upload ctx hosts = %v, want [10.0.0.2 \"\"]", storage.hosts)
	}
}
