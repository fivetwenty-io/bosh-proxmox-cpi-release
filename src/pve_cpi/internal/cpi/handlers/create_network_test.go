package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

func TestHandleCreateNetwork_NotImplemented(t *testing.T) {
	h := handlers.HandleCreateNetwork(handlers.Deps{})
	spec := map[string]any{"type": "manual", "subnets": []any{}}
	raw, _ := json.Marshal(spec)

	result, err := h.Handle(context.Background(), []json.RawMessage{raw}, jsonrpc.Context{})

	if result != nil {
		t.Fatalf("expected nil result, got %v", result)
	}
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeNotImplemented {
		t.Errorf("expected type %q, got %q", cpierrors.TypeNotImplemented, cpiErr.Type())
	}
	if cpiErr.OkToRetry() {
		t.Error("NotImplemented must not be ok_to_retry")
	}
}

func TestHandleCreateNetwork_NullSpec(t *testing.T) {
	// null network_spec is valid JSON and allowed by spec — still NotImplemented.
	h := handlers.HandleCreateNetwork(handlers.Deps{})

	result, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage("null")}, jsonrpc.Context{})

	if result != nil {
		t.Fatalf("expected nil result, got %v", result)
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T", err)
	}
	if cpiErr.Type() != cpierrors.TypeNotImplemented {
		t.Errorf("expected NotImplemented, got %q", cpiErr.Type())
	}
}

func TestHandleCreateNetwork_MissingArg(t *testing.T) {
	h := handlers.HandleCreateNetwork(handlers.Deps{})

	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing argument")
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T", err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError for missing arg, got %q", cpiErr.Type())
	}
}

func TestHandleCreateNetwork_InvalidJSON(t *testing.T) {
	h := handlers.HandleCreateNetwork(handlers.Deps{})

	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`not-json`)}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T", err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError for invalid JSON, got %q", cpiErr.Type())
	}
}

func TestHandleCreateNetwork_ErrorTypeString(t *testing.T) {
	// Verify the canonical BOSH type string used by the Director.
	h := handlers.HandleCreateNetwork(handlers.Deps{})
	raw, _ := json.Marshal(map[string]any{})

	_, err := h.Handle(context.Background(), []json.RawMessage{raw}, jsonrpc.Context{})

	cpiErr := err.(*cpierrors.Error)
	const want = "Bosh::Clouds::NotImplemented"
	if string(cpiErr.Type()) != want {
		t.Errorf("type string: got %q, want %q", cpiErr.Type(), want)
	}
}
