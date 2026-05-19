package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

func TestHandleInfo_ReturnsAPIVersion2(t *testing.T) {
	t.Parallel()

	deps := handlers.Deps{Logger: log.NewNopLogger()}
	h := handlers.HandleInfo(deps)

	result, err := h.Handle(context.Background(), nil, jsonrpc.Context{RequestID: "test-info-1"})
	if err != nil {
		t.Fatalf("HandleInfo: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("HandleInfo: result is nil")
	}

	// Round-trip through JSON to inspect fields generically.
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("HandleInfo: cannot marshal result: %v", marshalErr)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("HandleInfo: cannot unmarshal result map: %v", err)
	}

	// api_version must be 2.
	var apiVersion int
	if err := json.Unmarshal(m["api_version"], &apiVersion); err != nil {
		t.Fatalf("api_version field missing or invalid: %v", err)
	}
	if apiVersion != 2 {
		t.Errorf("api_version = %d; want 2", apiVersion)
	}

	// stemcell_formats must be a non-empty list containing the expected entries.
	var formats []string
	if err := json.Unmarshal(m["stemcell_formats"], &formats); err != nil {
		t.Fatalf("stemcell_formats field missing or invalid: %v", err)
	}
	if len(formats) == 0 {
		t.Fatal("stemcell_formats is empty; want at least one entry")
	}

	expected := map[string]bool{
		"openstack-qcow2": false,
		"openstack-raw":   false,
		"pve-qcow2":       false,
		"general-qcow2":   false,
		"general-raw":     false,
	}
	for _, f := range formats {
		if _, ok := expected[f]; ok {
			expected[f] = true
		} else {
			t.Errorf("unexpected stemcell format %q", f)
		}
	}
	for f, seen := range expected {
		if !seen {
			t.Errorf("stemcell format %q not present in result", f)
		}
	}
}

func TestHandleInfo_NoArgsRequired(t *testing.T) {
	t.Parallel()

	deps := handlers.Deps{Logger: log.NewNopLogger()}
	h := handlers.HandleInfo(deps)

	// Call with empty args — must not error.
	result, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("HandleInfo with empty args: unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("HandleInfo with empty args: result is nil")
	}
}

func TestHandleInfo_Idempotent(t *testing.T) {
	t.Parallel()

	deps := handlers.Deps{Logger: log.NewNopLogger()}
	h := handlers.HandleInfo(deps)

	for i := range 3 {
		result, err := h.Handle(context.Background(), nil, jsonrpc.Context{RequestID: "req"})
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if result == nil {
			t.Fatalf("call %d: nil result", i)
		}
	}
}
