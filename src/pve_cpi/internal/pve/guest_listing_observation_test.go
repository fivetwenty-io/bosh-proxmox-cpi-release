package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
	"net/http"
	"testing"
)

type guestWireCase struct {
	name, body string
	status     int
	wantErr    bool
	count      int
}

func TestGuestListingWireRequiresArray(t *testing.T) {
	for _, kind := range []string{"qemu", "lxc"} {
		for _, tc := range []guestWireCase{
			{name: "empty", body: `{"data":[]}`},
			{name: "guest", body: `{"data":[{"vmid":123,"name":"exact-guest"}]}`, count: 1},
			{name: "null", body: `{"data":null}`, wantErr: true},
			{name: "missing", body: `{}`, wantErr: true},
			{name: "object", body: `{"data":{"vmid":123}}`, wantErr: true},
			{name: "malformed", body: `broken`, wantErr: true},
			{name: "denied", body: `{"message":"denied"}`, status: 403, wantErr: true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				testGuestWireCase(t, kind, tc)
			})
		}
	}
}
func testGuestWireCase(t *testing.T, kind string, tc guestWireCase) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/nodes/pve1/"+kind, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("method changed")
		}
		if kind == "qemu" && r.URL.Query().Get("full") != "1" {
			t.Errorf("full parameter changed: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		if tc.status != 0 {
			w.WriteHeader(tc.status)
		}
		_, _ = w.Write([]byte(tc.body))
	})
	client := newPoolStubClient(t, mux)
	var rows []json.RawMessage
	var err error
	if kind == "qemu" {
		full := true
		result, e := client.Nodes().ListQemu(t.Context(), "pve1", &nodes.ListQemuParams{Full: &full})
		err = e
		if result != nil {
			rows = *result
		}
	} else {
		result, e := client.Nodes().ListLxc(t.Context(), "pve1")
		err = e
		if result != nil {
			rows = *result
		}
	}
	if (err != nil) != tc.wantErr {
		t.Fatalf("unexpected result: %v", err)
	}
	if tc.status == 403 {
		var apiErr *sdkerrors.PermissionError
		if !errors.As(err, &apiErr) || apiErr.HTTPCode != 403 {
			t.Fatal("typed API classification lost")
		}
	}
	if err != nil {
		return
	}
	if rows == nil || len(rows) != tc.count {
		t.Fatal("array identity lost")
	}
	if tc.count > 0 {
		var row map[string]any
		if json.Unmarshal(rows[0], &row) != nil || row["vmid"] != float64(123) || row["name"] != "exact-guest" {
			t.Fatal("guest identity changed")
		}
	}
}

func TestGuestListingPreservesCancellationAndFalseParameter(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/api2/json/nodes/pve1/qemu", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("full") != "0" {
			t.Error("explicit false lost")
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	client := newPoolStubClient(t, mux)
	full := false
	if _, err := client.Nodes().ListQemu(t.Context(), "pve1", &nodes.ListQemuParams{Full: &full}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Nodes().ListQemu(ctx, "pve1", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if _, err := client.Nodes().ListLxc(ctx, "pve1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if calls != 1 {
		t.Fatal("cancelled request reached server")
	}
}
