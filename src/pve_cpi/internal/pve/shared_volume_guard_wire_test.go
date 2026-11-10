// Wire-level tests that the client every handler uses carries the
// shared-volume guards: a foreign-owner volume delete on shared storage never
// reaches PVE, and a destroy that asks for the unreferenced-disk sweep on a
// node that sees shared storage reaches PVE with the sweep off.
package pve_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// guardWireStub serves a cluster whose one images storage is an NFS export,
// and records every request it is sent.
type guardWireStub struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   []string
	forms  []string
}

func newGuardWireStub(t *testing.T) *guardWireStub {
	t.Helper()
	s := &guardWireStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.forms = append(s.forms, r.Form.Encode())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/storage":
			_, _ = w.Write([]byte(`{"data":[{"storage":"nfs-shared","type":"nfs","shared":1,"server":"10.0.0.5","export":"/exports/pve","content":"images,iso"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve1/storage":
			_, _ = w.Write([]byte(`{"data":[{"storage":"nfs-shared","type":"nfs","shared":1,"content":"images,iso","enabled":1,"active":1}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":"UPID:pve1:00000001:00000001:00000001:qmdestroy:150:root@pam:"}`))
		}
	})
	s.server = httptest.NewTLSServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *guardWireStub) requests() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...), append([]string(nil), s.forms...)
}

func guardWireCfg(t *testing.T, s *guardWireStub) *config.CPIConfig {
	t.Helper()
	host, port := hostPort(t, s.server.URL)
	return &config.CPIConfig{
		Host:                 host,
		Port:                 port,
		APIToken:             "root@pam!test=tok-wire",
		APIEndpointDiscovery: config.DiscoveryModeOff,
		VerifySSL:            boolPtr(false),
	}
}

func TestClientGuardsVolumeDeletesOnSharedStorage(t *testing.T) {
	t.Parallel()
	for name, withTracer := range map[string]bool{"untraced": false, "traced": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newGuardWireStub(t)
			var c pve.Client
			var err error
			if withTracer {
				c, err = pve.NewClientWithTracer(guardWireCfg(t, stub), log.NewNopLogger(), noop.NewTracerProvider().Tracer("t"))
			} else {
				c, err = pve.NewClientWithTracer(guardWireCfg(t, stub), log.NewNopLogger(), nil)
			}
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = c.Storage().DeleteVolumeAsync(context.Background(), "pve1", "nfs-shared", "nfs-shared:50000/vm-50000-disk-0.qcow2")
			if err == nil || !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Fatalf("foreign-owner delete returned %v, want a permanent refusal", err)
			}
			seen, _ := stub.requests()
			for _, req := range seen {
				if strings.HasPrefix(req, "DELETE") {
					t.Fatalf("the refused delete reached PVE: %v", seen)
				}
			}
			if len(seen) != 1 || seen[0] != "GET /api2/json/storage" {
				t.Errorf("requests = %v, want only the storage index read", seen)
			}
		})
	}
}

func TestClientTurnsTheSweepOffOnSharedStorage(t *testing.T) {
	defer pve.ResetSweepSuppressedWarnOnceForTest()()
	stub := newGuardWireStub(t)
	c, err := pve.NewClientWithTracer(guardWireCfg(t, stub), log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sweep, purge := true, true
	if _, err := c.Nodes().DeleteQemu(context.Background(), "pve1", "150",
		&sdknodes.DeleteQemuParams{DestroyUnreferencedDisks: &sweep, Purge: &purge}); err != nil {
		t.Fatalf("DeleteQemu: %v", err)
	}
	seen, forms := stub.requests()
	for i, req := range seen {
		if req != "DELETE /api2/json/nodes/pve1/qemu/150" {
			continue
		}
		if got := forms[i]; !containsParam(got, "destroy-unreferenced-disks=0") {
			t.Fatalf("destroy sent with %q, want destroy-unreferenced-disks=0", got)
		}
		return
	}
	t.Fatalf("no destroy reached PVE: %v", seen)
}

func containsParam(encoded, param string) bool {
	return slices.Contains(strings.Split(encoded, "&"), param)
}
