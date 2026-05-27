package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/registry"
)

// testLogger returns a no-op production logger suitable for unit tests.
func testLogger() *log.Logger {
	return log.NewNopLogger()
}

// newRegistryAgent builds a RegistryAgent pointed at srv.
func newRegistryAgent(t *testing.T, srv *httptest.Server) *agent.RegistryAgent {
	t.Helper()
	c := registry.NewClient(srv.URL, "user", "secret")
	return agent.NewRegistryAgent(c, testLogger())
}

// --------------------------------------------------------------------------
// Configure
// --------------------------------------------------------------------------

func TestConfigure_Put(t *testing.T) {
	t.Parallel()
	type captured struct {
		method  string
		path    string
		rawBody []byte
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)

	cfg := agent.AgentConfig{
		AgentID: "agent-abc",
		VM:      agent.VMSpec{Name: "vm-100", ID: "100"},
		Networks: map[string]agent.NetworkSpec{
			"default": {
				Type:    "manual",
				IP:      "10.0.0.5",
				Netmask: "255.255.255.0",
				Gateway: "10.0.0.1",
				DNS:     []string{"8.8.8.8"},
				Default: []string{"dns", "gateway"},
			},
		},
		Disks: agent.DisksSpec{
			System:    "/dev/sda",
			Ephemeral: "/dev/sdb",
		},
		Env:  map[string]any{"bosh": map[string]any{"password": "secret"}},
		MBus: "nats://host:4222",
		Blobstore: agent.BlobstoreSpec{
			Provider: "dav",
			Options:  map[string]any{"endpoint": "http://blobstore"},
		},
		NTP: []string{"0.pool.ntp.org"},
	}

	err := ra.Configure(context.Background(), "node1", 100, cfg)
	if err != nil {
		t.Fatalf("Configure returned unexpected error: %v", err)
	}

	if got.method != http.MethodPut {
		t.Errorf("method: got %q, want PUT", got.method)
	}
	if got.path != "/instances/100/settings" {
		t.Errorf("path: got %q, want /instances/100/settings", got.path)
	}

	// Envelope must contain "settings" key whose value is a JSON string.
	var envelope map[string]string
	if err := json.Unmarshal(got.rawBody, &envelope); err != nil {
		t.Fatalf("body is not a valid JSON envelope: %v — body: %s", err, got.rawBody)
	}
	settingsStr, ok := envelope["settings"]
	if !ok {
		t.Fatal("envelope missing 'settings' key")
	}

	// Parse the inner settings JSON.
	var settings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsStr), &settings); err != nil {
		t.Fatalf("settings value is not valid JSON: %v", err)
	}

	// Verify networks key present.
	if _, ok := settings["networks"]; !ok {
		t.Error("settings missing 'networks' key")
	}
	// Verify agent_id.
	var agentID string
	if err := json.Unmarshal(settings["agent_id"], &agentID); err != nil {
		t.Fatalf("agent_id unmarshal: %v", err)
	}
	if agentID != "agent-abc" {
		t.Errorf("agent_id: got %q, want agent-abc", agentID)
	}
}

func TestConfigure_RegistryError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	err := ra.Configure(context.Background(), "node1", 100, agent.AgentConfig{})
	if err == nil {
		t.Fatal("expected error from registry 500, got nil")
	}
	if !strings.Contains(err.Error(), "configure") && !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention configure or 500: %v", err)
	}
}

func TestConfigure_NilPersistentMap(t *testing.T) {
	t.Parallel()
	// Ensure nil Disks.Persistent is serialised as {} not null.
	var settingsStr string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &env)
		settingsStr = env["settings"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	cfg := agent.AgentConfig{AgentID: "x", Disks: agent.DisksSpec{}} // Persistent is nil

	if err := ra.Configure(context.Background(), "n1", 1, cfg); err != nil {
		t.Fatalf("Configure error: %v", err)
	}

	var inner map[string]json.RawMessage
	_ = json.Unmarshal([]byte(settingsStr), &inner)
	var disks agent.DisksSpec
	_ = json.Unmarshal(inner["disks"], &disks)
	if disks.Persistent == nil {
		t.Error("persistent map should be {} not null")
	}
}

// --------------------------------------------------------------------------
// Remove
// --------------------------------------------------------------------------

func TestRemove_Delete(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	if err := ra.Remove(context.Background(), "node1", 100); err != nil {
		t.Fatalf("Remove returned unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	if gotPath != "/instances/100/settings" {
		t.Errorf("path: got %q, want /instances/100/settings", gotPath)
	}
}

func TestRemove_404(t *testing.T) {
	t.Parallel()
	// 404 from registry Delete must propagate as nil (idempotent).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	if err := ra.Remove(context.Background(), "node1", 100); err != nil {
		t.Fatalf("Remove with 404 should return nil (idempotent), got: %v", err)
	}
}

// --------------------------------------------------------------------------
// UpdateDiskHints
// --------------------------------------------------------------------------

// TestUpdateDiskHints_GetThenPut verifies the GET → merge → PUT flow.
func TestUpdateDiskHints_GetThenPut(t *testing.T) {
	t.Parallel()
	// Existing settings stored in registry.
	existingSettings := map[string]any{
		"agent_id": "agent-xyz",
		"vm":       map[string]any{"name": "vm-100", "id": "100"},
		"networks": map[string]any{},
		"disks": map[string]any{
			"system":     "/dev/sda",
			"ephemeral":  "/dev/sdb",
			"persistent": map[string]string{"old-disk": "/dev/sdc"},
		},
		"env":       map[string]any{},
		"mbus":      "nats://host:4222",
		"blobstore": map[string]any{"provider": "dav", "options": map[string]any{}},
		"ntp":       []string{},
	}
	existingJSON, _ := json.Marshal(existingSettings)
	envelope := map[string]string{"settings": string(existingJSON)}
	envelopeBody, _ := json.Marshal(envelope)

	var putBody []byte
	requestCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(envelopeBody) //nolint:errcheck // test HTTP handler — write errors not actionable in handler context
		case http.MethodPut:
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)

	hints := []agent.DiskHint{
		{DiskCID: "new-disk", DevicePath: "/dev/sdd"},
	}
	if err := ra.UpdateDiskHints(context.Background(), 100, hints); err != nil {
		t.Fatalf("UpdateDiskHints returned unexpected error: %v", err)
	}

	if requestCount < 2 {
		t.Errorf("expected at least 2 requests (GET + PUT), got %d", requestCount)
	}

	// Decode PUT body and verify persistent map contains both old and new entries.
	var putEnvelope map[string]string
	if err := json.Unmarshal(putBody, &putEnvelope); err != nil {
		t.Fatalf("PUT body is not valid JSON envelope: %v", err)
	}
	var putSettings map[string]json.RawMessage
	if err := json.Unmarshal([]byte(putEnvelope["settings"]), &putSettings); err != nil {
		t.Fatalf("PUT settings is not valid JSON: %v", err)
	}

	var disks agent.DisksSpec
	if err := json.Unmarshal(putSettings["disks"], &disks); err != nil {
		t.Fatalf("disks field unmarshal: %v", err)
	}

	if disks.Persistent["old-disk"] != "/dev/sdc" {
		t.Errorf("old-disk: got %q, want /dev/sdc", disks.Persistent["old-disk"])
	}
	if disks.Persistent["new-disk"] != "/dev/sdd" {
		t.Errorf("new-disk: got %q, want /dev/sdd", disks.Persistent["new-disk"])
	}
}

// TestUpdateDiskHints_404OnGet verifies that a 404 from GET returns an error.
func TestUpdateDiskHints_404OnGet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	err := ra.UpdateDiskHints(context.Background(), 100, []agent.DiskHint{
		{DiskCID: "disk-1", DevicePath: "/dev/sdc"},
	})
	if err == nil {
		t.Fatal("expected error on 404 GET, got nil")
	}
}

// TestUpdateDiskHints_EmptyHints verifies no-op on empty hints list still
// round-trips correctly (GET + PUT with unchanged settings).
func TestUpdateDiskHints_EmptyHints(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"agent_id":  "x",
		"vm":        map[string]any{},
		"networks":  map[string]any{},
		"disks":     map[string]any{"system": "/dev/sda", "ephemeral": "/dev/sdb", "persistent": map[string]any{}},
		"env":       map[string]any{},
		"mbus":      "",
		"blobstore": map[string]any{},
		"ntp":       []string{},
	}
	existingJSON, _ := json.Marshal(existing)
	envelope := map[string]string{"settings": string(existingJSON)}
	envelopeBody, _ := json.Marshal(envelope)

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.Write(envelopeBody) //nolint:errcheck // test HTTP handler — write errors not actionable in handler context
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	if err := ra.UpdateDiskHints(context.Background(), 100, nil); err != nil {
		t.Fatalf("UpdateDiskHints with nil hints returned error: %v", err)
	}
	if requestCount != 2 {
		t.Errorf("expected exactly 2 requests, got %d", requestCount)
	}
}

// TestUpdateDiskHints_EmptyDevicePath verifies that an empty DevicePath removes the entry.
func TestUpdateDiskHints_EmptyDevicePath(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"disks": map[string]any{
			"system":    "/dev/sda",
			"ephemeral": "/dev/sdb",
			"persistent": map[string]string{
				"remove-me": "/dev/sdc",
				"keep-me":   "/dev/sdd",
			},
		},
	}
	existingJSON, _ := json.Marshal(existing)
	envelope := map[string]string{"settings": string(existingJSON)}
	envelopeBody, _ := json.Marshal(envelope)

	var putBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Write(envelopeBody) //nolint:errcheck // test HTTP handler — write errors not actionable in handler context
		case http.MethodPut:
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	hints := []agent.DiskHint{{DiskCID: "remove-me", DevicePath: ""}}
	if err := ra.UpdateDiskHints(context.Background(), 100, hints); err != nil {
		t.Fatalf("UpdateDiskHints returned error: %v", err)
	}

	var putEnvelope map[string]string
	_ = json.Unmarshal(putBody, &putEnvelope)
	var putSettings map[string]json.RawMessage
	_ = json.Unmarshal([]byte(putEnvelope["settings"]), &putSettings)
	var disks agent.DisksSpec
	_ = json.Unmarshal(putSettings["disks"], &disks)

	if _, found := disks.Persistent["remove-me"]; found {
		t.Error("remove-me should have been deleted from persistent map")
	}
	if disks.Persistent["keep-me"] != "/dev/sdd" {
		t.Errorf("keep-me: got %q, want /dev/sdd", disks.Persistent["keep-me"])
	}
}

// --------------------------------------------------------------------------
// MBus fallback
// --------------------------------------------------------------------------

// captureSettings posts to a fake registry server and returns the inner
// settings JSON sent by Configure.
func captureSettings(t *testing.T, cfg agent.AgentConfig) map[string]json.RawMessage {
	t.Helper()
	var settingsStr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &env)
		settingsStr = env["settings"]
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ra := newRegistryAgent(t, srv)
	if err := ra.Configure(context.Background(), "node1", 100, cfg); err != nil {
		t.Fatalf("Configure error: %v", err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settingsStr), &inner); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return inner
}

// TestRegistryAgent_Configure_MBusFallbackReturnsError verifies that Configure
// returns an error when MBus is empty and the blobstore endpoint would produce
// a credential-less NATS URL. Operators must supply mbus explicitly.
func TestRegistryAgent_Configure_MBusFallbackReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ra := newRegistryAgent(t, srv)
	cfg := agent.AgentConfig{
		AgentID: "x",
		MBus:    "",
		Blobstore: agent.BlobstoreSpec{
			Provider: "dav",
			Options:  map[string]any{"endpoint": "https://10.0.0.1:25250"},
		},
	}
	err := ra.Configure(context.Background(), "node1", 100, cfg)
	if err == nil {
		t.Fatal("expected error when mbus is empty and blobstore host is derivable; got nil")
	}
	if !strings.Contains(err.Error(), "mbus is empty") {
		t.Errorf("error should mention 'mbus is empty', got: %v", err)
	}
}

func TestRegistryAgent_Configure_MBusExplicitWinsOverFallback(t *testing.T) {
	t.Parallel()
	cfg := agent.AgentConfig{
		AgentID: "x",
		MBus:    "nats://explicit:4222",
		Blobstore: agent.BlobstoreSpec{
			Provider: "dav",
			Options:  map[string]any{"endpoint": "https://10.0.0.1:25250"},
		},
	}
	settings := captureSettings(t, cfg)
	var mbus string
	_ = json.Unmarshal(settings["mbus"], &mbus)
	if mbus != "nats://explicit:4222" {
		t.Errorf("mbus = %q, want explicit value preserved", mbus)
	}
}

func TestRegistryAgent_Configure_MBusEmptyWhenNoBlobstore(t *testing.T) {
	t.Parallel()
	cfg := agent.AgentConfig{AgentID: "x"}
	settings := captureSettings(t, cfg)
	var mbus string
	_ = json.Unmarshal(settings["mbus"], &mbus)
	if mbus != "" {
		t.Errorf("mbus = %q, want empty (no synthesized 0.0.0.0)", mbus)
	}
}

// TestRegistryAgent_Configure_AppliesVMNameDefault verifies that when
// cfg.VM.Name is empty the shared buildSettings default ("vm-{vmid}")
// is applied — registry agent must not write VM.Name="" to the
// registry just because the caller omitted it.
func TestRegistryAgent_Configure_AppliesVMNameDefault(t *testing.T) {
	t.Parallel()
	cfg := agent.AgentConfig{AgentID: "x"} // VM.Name intentionally empty
	settings := captureSettings(t, cfg)
	var vm agent.VMSpec
	if err := json.Unmarshal(settings["vm"], &vm); err != nil {
		t.Fatalf("unmarshal vm: %v", err)
	}
	if vm.Name != "vm-100" {
		t.Errorf("vm.name = %q, want vm-100 (captureSettings uses vmid=100)", vm.Name)
	}
}

// TestRegistryAgent_Configure_AppliesVMIDDefault verifies that an empty
// cfg.VM.ID falls back to the vmid string passed to Configure.
func TestRegistryAgent_Configure_AppliesVMIDDefault(t *testing.T) {
	t.Parallel()
	cfg := agent.AgentConfig{AgentID: "x"} // VM.ID intentionally empty
	settings := captureSettings(t, cfg)
	var vm agent.VMSpec
	if err := json.Unmarshal(settings["vm"], &vm); err != nil {
		t.Fatalf("unmarshal vm: %v", err)
	}
	if vm.ID != "100" {
		t.Errorf("vm.id = %q, want \"100\"", vm.ID)
	}
}

// TestRegistryAgent_Configure_NetworksRenderAsEmptyObject confirms that
// when cfg.Networks is nil, the settings JSON contains "networks": {}
// (not "networks": null). Empty objects are what the BOSH agent expects;
// a null field is silently accepted but downstream tests assume {}.
func TestRegistryAgent_Configure_NetworksRenderAsEmptyObject(t *testing.T) {
	t.Parallel()
	cfg := agent.AgentConfig{AgentID: "x"} // Networks intentionally nil
	settings := captureSettings(t, cfg)
	raw, ok := settings["networks"]
	if !ok {
		t.Fatal("settings missing 'networks' key")
	}
	if string(raw) != "{}" {
		t.Errorf("networks JSON = %s, want {}", string(raw))
	}
	// ntp + env should likewise be non-null.
	if string(settings["ntp"]) != "[]" {
		t.Errorf("ntp JSON = %s, want []", string(settings["ntp"]))
	}
	if string(settings["env"]) != "{}" {
		t.Errorf("env JSON = %s, want {}", string(settings["env"]))
	}
}
