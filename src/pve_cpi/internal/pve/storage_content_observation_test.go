package pve_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestStorageContentWireRequiresArray(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		count      int
		reason     string
		wantErr    bool
	}{
		{name: "empty", body: `{"data":[]}`},
		{name: "image", body: `{"data":[{"volid":"nfs:20000/disk.qcow2","size":1073741824}]}`, count: 1},
		{name: "null", body: `{"data":null}`, wantErr: true, reason: "listing_data_missing"},
		{name: "missing data", body: `{}`, wantErr: true, reason: "listing_data_missing"},
		{name: "object", body: `{"data":{"volid":"nfs:20000/disk.qcow2"}}`, wantErr: true, reason: "listing_shape_invalid"},
		{name: "malformed", body: `broken`, wantErr: true},
		{name: "backend failure", body: `{"message":"private-backend-error"}`, status: 500, wantErr: true, reason: "listing_http_500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/nodes/pve1/storage/nfs/content", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Query().Get("content") != "images" {
					t.Errorf("request not preserved: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			client := newPoolStubClient(t, mux)
			content := "images"
			listing, err := client.Nodes().ListStorageContent(t.Context(), "pve1", "nfs", &nodes.ListStorageContentParams{Content: &content})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %+v %v", listing, err)
			}
			if err != nil {
				if tc.reason != "" && pve.StorageVolumeObservationReason(err) != tc.reason {
					t.Fatalf("reason = %q, want %q", pve.StorageVolumeObservationReason(err), tc.reason)
				}
				if strings.Contains(err.Error(), "private-backend-error") {
					t.Fatal("backend details leaked")
				}
				return
			}
			if listing == nil || *listing == nil || len(*listing) != tc.count {
				t.Fatalf("listing changed: %+v", listing)
			}
			if tc.count > 0 {
				var row map[string]any
				if json.Unmarshal((*listing)[0], &row) != nil || row["volid"] != "nfs:20000/disk.qcow2" {
					t.Fatal("volume identity changed")
				}
			}
		})
	}
}

func TestHAResourcesWireRequiresArray(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		count      int
		wantErr    bool
	}{
		{name: "empty", body: `{"data":[]}`},
		{name: "image", body: `{"data":[{"sid":"vm:20000","size":1073741824}]}`, count: 1},
		{name: "null", body: `{"data":null}`, wantErr: true},
		{name: "missing data", body: `{}`, wantErr: true},
		{name: "object", body: `{"data":{"sid":"vm:20000"}}`, wantErr: true},
		{name: "malformed", body: `broken`, wantErr: true},
		{name: "backend failure", body: `{"message":"private-backend-error"}`, status: 500, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/cluster/ha/resources", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Query().Get("type") != "vm" {
					t.Errorf("request not preserved: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			client := newPoolStubClient(t, mux)
			content := "vm"
			listing, err := client.Cluster().ListHaResources(t.Context(), &cluster.ListHaResourcesParams{Type: &content})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %+v %v", listing, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "private-backend-error") {
					t.Fatal("backend details leaked")
				}
				return
			}
			if listing == nil || len(*listing) != tc.count {
				t.Fatalf("listing changed: %+v", listing)
			}
			if tc.count > 0 {
				var row map[string]any
				if json.Unmarshal((*listing)[0], &row) != nil || row["sid"] != "vm:20000" {
					t.Fatal("HA identity changed")
				}
			}
		})
	}
}

func TestHARulesWireRequiresArray(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		count      int
		wantErr    bool
	}{
		{name: "empty", body: `{"data":[]}`},
		{name: "image", body: `{"data":[{"rule":"rule-20000","size":1073741824}]}`, count: 1},
		{name: "null", body: `{"data":null}`, wantErr: true},
		{name: "missing data", body: `{}`, wantErr: true},
		{name: "object", body: `{"data":{"rule":"rule-20000"}}`, wantErr: true},
		{name: "malformed", body: `broken`, wantErr: true},
		{name: "backend failure", body: `{"message":"private-backend-error"}`, status: 500, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api2/json/cluster/ha/rules", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Query().Get("type") != "vm" || r.URL.Query().Get("resource") != "vm:20000" {
					t.Errorf("request not preserved: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			client := newPoolStubClient(t, mux)
			content := "vm"
			resource := "vm:20000"
			listing, err := client.Cluster().ListHaRules(t.Context(), &cluster.ListHaRulesParams{Type: &content, Resource: &resource})
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %+v %v", listing, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "private-backend-error") {
					t.Fatal("backend details leaked")
				}
				return
			}
			if listing == nil || len(*listing) != tc.count {
				t.Fatalf("listing changed: %+v", listing)
			}
			if tc.count > 0 {
				var row map[string]any
				if json.Unmarshal((*listing)[0], &row) != nil || row["rule"] != "rule-20000" {
					t.Fatal("HA identity changed")
				}
			}
		})
	}
}
