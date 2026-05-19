package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

func TestHandleDeleteNetwork_NotImplemented(t *testing.T) {
	h := handlers.HandleDeleteNetwork(handlers.Deps{})
	cid, _ := json.Marshal("net-abc123")

	result, err := h.Handle(context.Background(), []json.RawMessage{cid}, jsonrpc.Context{})

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

func TestHandleDeleteNetwork_MissingArg(t *testing.T) {
	h := handlers.HandleDeleteNetwork(handlers.Deps{})

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

func TestHandleDeleteNetwork_NonStringCID(t *testing.T) {
	h := handlers.HandleDeleteNetwork(handlers.Deps{})

	_, err := h.Handle(context.Background(), []json.RawMessage{json.RawMessage(`12345`)}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for non-string CID")
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T", err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError, got %q", cpiErr.Type())
	}
}

func TestHandleDeleteNetwork_EmptyCID(t *testing.T) {
	h := handlers.HandleDeleteNetwork(handlers.Deps{})
	cid, _ := json.Marshal("   ")

	_, err := h.Handle(context.Background(), []json.RawMessage{cid}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for blank CID")
	}
	cpiErr, ok := err.(*cpierrors.Error)
	if !ok {
		t.Fatalf("expected *cpierrors.Error, got %T", err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("expected CloudError for empty CID, got %q", cpiErr.Type())
	}
}

func TestHandleDeleteNetwork_InvalidJSON(t *testing.T) {
	h := handlers.HandleDeleteNetwork(handlers.Deps{})

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

func TestHandleDeleteNetwork_ErrorTypeString(t *testing.T) {
	// Verify the canonical BOSH type string used by the Director.
	h := handlers.HandleDeleteNetwork(handlers.Deps{})
	cid, _ := json.Marshal("net-xyz")

	_, err := h.Handle(context.Background(), []json.RawMessage{cid}, jsonrpc.Context{})

	cpiErr := err.(*cpierrors.Error)
	const want = "Bosh::Clouds::NotImplemented"
	if string(cpiErr.Type()) != want {
		t.Errorf("type string: got %q, want %q", cpiErr.Type(), want)
	}
}
