package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// stampedCIDFixture is VMID 101 in the stamped form a later release will emit.
// Production still emits bare CIDs; the conformance test runs with the stamped
// form from the start so the path through the built binary is proven before
// any CID changes format.
const stampedCIDFixture = "101-0a1b2c3d-4e5f6a7b"

var vmidPathSegment = regexp.MustCompile(`^/nodes/[^/]+/(?:qemu|lxc)/([^/]+)`)

// strictVMIDPVE is an httptest PVE that answers 400 for any request whose vmid
// path segment is not all digits, the way real PVE does, and records every
// such request. It serves one running VM, 101, on node pve1.
type strictVMIDPVE struct {
	server *httptest.Server

	mu         sync.Mutex
	pools      map[string]string
	badVMID    []string
	vmRequests []string
}

func newStrictVMIDPVE(t *testing.T) *strictVMIDPVE {
	t.Helper()
	f := &strictVMIDPVE{pools: map[string]string{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *strictVMIDPVE) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	w.Header().Set("Content-Type", "application/json")

	if m := vmidPathSegment.FindStringSubmatch(path); m != nil {
		f.mu.Lock()
		f.vmRequests = append(f.vmRequests, r.Method+" "+path)
		numeric := strings.Trim(m[1], "0123456789") == ""
		if !numeric {
			f.badVMID = append(f.badVMID, r.Method+" "+path)
		}
		f.mu.Unlock()
		if !numeric {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":   nil,
				"errors": map[string]string{"vmid": "value does not look like a valid VM ID"},
			})
			return
		}
	}

	if f.servePool(w, r, path) {
		return
	}

	var data any
	switch {
	case path == "/access/ticket":
		data = map[string]any{
			"ticket":              "PVE:root@pam:00000000::fake",
			"CSRFPreventionToken": "00000000:fake",
			"username":            "root@pam",
		}
	case path == "/version":
		data = map[string]any{"version": "8.4.1", "release": "8.4", "repoid": "fake"}
	case path == "/nodes":
		data = []any{map[string]any{"node": "pve1", "status": "online"}}
	case path == "/cluster/resources":
		data = []any{map[string]any{
			"id": "qemu/101", "type": "qemu", "vmid": 101, "node": "pve1", "status": "running",
			"name": "vm-101", "tags": "bosh-cpi",
		}}
	case path == "/cluster/status":
		data = []any{map[string]any{"type": "node", "name": "pve1", "online": 1}}
	case path == "/cluster/config/nodes":
		data = []any{map[string]any{"name": "pve1"}}
	case strings.HasSuffix(path, "/status/current"):
		data = map[string]any{"status": "running", "vmid": 101}
	case strings.HasSuffix(path, "/config") && r.Method == http.MethodGet:
		data = map[string]any{"name": "vm-101", "tags": "bosh-cpi", "description": ""}
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/qemu"):
		data = []any{map[string]any{"vmid": 101, "status": "running", "name": "vm-101", "tags": "bosh-cpi"}}
	case strings.Contains(path, "/tasks/"):
		data = map[string]any{"status": "stopped", "exitstatus": "OK"}
	case r.Method == http.MethodGet:
		data = []any{}
	default:
		data = "UPID:pve1:00000001:00000001:00000001:task:101:root@pam:"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// servePool keeps a small in-memory pool store, because the per-VMID lock the
// handlers take is a pool whose comment names the holder. It reports whether
// it answered the request.
func (f *strictVMIDPVE) servePool(w http.ResponseWriter, r *http.Request, path string) bool {
	if path != "/pools" && !strings.HasPrefix(path, "/pools/") {
		return false
	}
	_ = r.ParseForm()
	id := strings.TrimPrefix(strings.TrimPrefix(path, "/pools"), "/")
	if id == "" {
		id = r.Form.Get("poolid")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	comment, exists := f.pools[id]
	var data any
	switch r.Method {
	case http.MethodPost:
		if id != "" {
			f.pools[id] = r.Form.Get("comment")
		}
	case http.MethodPut:
		if exists && r.Form.Has("comment") {
			f.pools[id] = r.Form.Get("comment")
		}
	case http.MethodDelete:
		delete(f.pools, id)
	default:
		switch {
		case id == "":
			list := []any{}
			for pid := range f.pools {
				list = append(list, map[string]any{"poolid": pid})
			}
			data = list
		case !exists:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": nil})
			return true
		default:
			data = map[string]any{"comment": comment, "members": []any{}}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	return true
}

func (f *strictVMIDPVE) config(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := fmt.Sprintf(`{
  "host": %q,
  "port": %s,
  "user": "root",
  "password": "test-password",
  "vm_storage": "local-lvm",
  "disk_storage": "local-lvm",
  "stemcell_storage": "local",
  "network_bridge": "vmbr0",
  "node": "pve1",
  "log_level": "error",
  "verify_ssl": false,
  "retry": {"transient": {"max_attempts": 1}}
}`, u.Hostname(), u.Port())
	cfgFile := filepath.Join(t.TempDir(), "cpi.json")
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	return cfgFile
}

// TestBinary_StampedCIDNeverReachesPVEPath drives the VM-addressed methods
// through the built binary with a stamped CID against a PVE that answers 400
// for a non-numeric vmid path segment. A handler that passes the raw CID to
// PVE instead of its bare VMID lands in the fake's bad-vmid list.
func TestBinary_StampedCIDNeverReachesPVEPath(t *testing.T) {
	cases := []struct {
		method   string
		args     string
		mutation string
	}{
		{"has_vm", fmt.Sprintf(`[%q]`, stampedCIDFixture), ""},
		{"set_vm_metadata", fmt.Sprintf(`[%q,{"director":"d","deployment":"cf","job":"cell","index":"0","id":"abc"}]`, stampedCIDFixture), "PUT /nodes/pve1/qemu/101/config"},
		{"reboot_vm", fmt.Sprintf(`[%q]`, stampedCIDFixture), "POST /nodes/pve1/qemu/101/status/reboot"},
		{"delete_vm", fmt.Sprintf(`[%q]`, stampedCIDFixture), "DELETE /nodes/pve1/qemu/101"},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			fake := newStrictVMIDPVE(t)
			cfgFile := fake.config(t)
			req := fmt.Sprintf(`{"method":%q,"arguments":%s,"context":{"request_id":"cid-%s"},"api_version":2}`,
				tc.method, tc.args, tc.method) + "\n"
			stdout, code, err := runBinary(t, req, "--config", cfgFile)
			if err != nil {
				t.Fatalf("runBinary: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit %d; stdout=%q", code, stdout)
			}
			resp := parseSingleEnvelope(t, stdout)

			fake.mu.Lock()
			bad := append([]string(nil), fake.badVMID...)
			seen := append([]string(nil), fake.vmRequests...)
			fake.mu.Unlock()
			if resp.Error != nil {
				t.Fatalf("%s returned an envelope error: %s; requests: %v", tc.method, resp.Error.Message, seen)
			}
			if len(bad) > 0 {
				t.Errorf("raw CID reached a PVE vmid path: %v", bad)
			}
			if tc.mutation == "" {
				// has_vm answers from the cluster listing and makes no
				// VM-addressed call, so it must report the VM as present.
				if got, ok := resp.Result.(bool); !ok || !got {
					t.Errorf("%s result = %v, want true", tc.method, resp.Result)
				}
			} else if !slices.Contains(seen, tc.mutation) {
				t.Errorf("%s never made the mutating call %q; requests: %v", tc.method, tc.mutation, seen)
			}
		})
	}
}
