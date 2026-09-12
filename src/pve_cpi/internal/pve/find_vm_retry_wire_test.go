package pve_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestFindVMAuthoritativeWireMissingDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name, message      string
		status             int
		recover            bool
		wantCalls          int
		wantErr, wantFound bool
	}{
		{name: "missing config", message: "Configuration file 'nodes/pve1/qemu-server/123.conf' does not exist", status: 500, wantCalls: 1},
		{name: "not found", message: "not found", status: 404, wantCalls: 1},
		{name: "forbidden", message: "permission denied", status: 403, wantCalls: 1, wantErr: true},
		{name: "transient recovers", message: "internal server error", status: 500, recover: true, wantCalls: 2, wantFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) })
			mux.HandleFunc("/api2/json/cluster/config/nodes", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[{"name":"pve1"}]}`)) })
			mux.HandleFunc("/api2/json/cluster/status", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"type":"cluster","quorate":1},{"type":"node","name":"pve1","online":1}]}`))
			})
			mux.HandleFunc("/api2/json/nodes/pve1/qemu/123/config", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				n := calls.Add(1)
				if tc.recover && n > 1 {
					_, _ = w.Write([]byte(`{"data":{"name":"found","tags":"owned"}}`))
					return
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": tc.message})
			})
			client := newPoolStubClient(t, mux)
			ctx := pve.WithTestBackoff(t.Context(), func(int) time.Duration { return 0 })
			location, err := pve.FindVMAuthoritative(ctx, client, 123)
			if (err != nil) != tc.wantErr || location.Found != tc.wantFound {
				t.Fatalf("location=%+v err=%v", location, err)
			}
			if int(calls.Load()) != tc.wantCalls {
				t.Fatalf("config requests=%d want=%d", calls.Load(), tc.wantCalls)
			}
		})
	}
}
