package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// baseDepsForHas builds Deps for has_disk tests.
func baseDepsForHas(t *testing.T, storageSvc *mockStorageService) handlers.Deps {
	t.Helper()
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "pve1",
			DiskStorage: "local-lvm",
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// has_disk tests
// ---------------------------------------------------------------------------

func TestHandleHasDisk_Exists(t *testing.T) {
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, node, storage, volume string) (bool, error) {
			if node != "pve1" {
				t.Errorf("unexpected node %q", node)
			}
			if storage != "local-lvm" {
				t.Errorf("unexpected storage %q", storage)
			}
			if volume != "vm-9001-disk-0" {
				t.Errorf("unexpected volume %q", volume)
			}
			return true, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestHandleHasDisk_NotExists(t *testing.T) {
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, nil
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9999-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if exists {
		t.Error("expected exists=false")
	}
}

func TestHandleHasDisk_SDKNotFoundError_ReturnsFalse(t *testing.T) {
	// If SDK Exists returns a not-found error (unusual but defensive), CPI returns false.
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, &sdkerrors.APIError{
				Message:  "volume not found",
				HTTPCode: 404,
			}
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("expected nil error for not-found, got: %v", err)
	}
	exists, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool result, got %T", result)
	}
	if exists {
		t.Error("expected exists=false for not-found error")
	}
}

func TestHandleHasDisk_SDKError_Propagated(t *testing.T) {
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, errors.New("storage backend unavailable")
		},
	}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error to be propagated from Exists failure")
	}
}

func TestHandleHasDisk_MalformedCID_NoColon(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("malformed-disk-cid"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for malformed disk_cid with no colon")
	}
}

func TestHandleHasDisk_EmptyCID(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(""),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for empty disk_cid")
	}
}

func TestHandleHasDisk_TooFewArgs(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing disk_cid argument")
	}
}

func TestHandleHasDisk_MissingNode(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:        "",
			DiskStorage: "local-lvm",
		},
		PVE:    newHandlerMockClient(storageSvc, nil),
		Logger: log.NewNopLogger(),
	}

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("local-lvm:vm-9001-disk-0"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing node")
	}
}

func TestHandleHasDisk_EmptyStoragePart(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(":volume"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty storage part")
	}
}

func TestHandleHasDisk_EmptyVolumePart(t *testing.T) {
	storageSvc := &mockStorageService{}
	deps := baseDepsForHas(t, storageSvc)

	h := handlers.HandleHasDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal("storage:"),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for disk_cid with empty volume part")
	}
}
