package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// mustRegister calls d.Register and fails the test on error. All names in tests
// are canonical; an error here indicates a test-authoring mistake.
func mustRegister(t *testing.T, d *cpi.Dispatcher, method string, h cpi.Handler) {
	t.Helper()
	if err := d.Register(method, h); err != nil {
		t.Fatalf("Register(%q): unexpected error: %v", method, err)
	}
}

// nopLogger returns a no-op log.Logger suitable for tests that do not assert log output.
func nopLogger() *log.Logger { return log.NewNopLogger() }

// makeReq builds a minimal *jsonrpc.Request for the given method.
func makeReq(method string) *jsonrpc.Request {
	return &jsonrpc.Request{
		Method:     method,
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "test-req-1"},
		APIVersion: 2,
	}
}

// --------------------------------------------------------------------------
// TestNewDispatcher_PreRegisters22
// --------------------------------------------------------------------------

func TestNewDispatcher_PreRegisters22(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	methods := cpi.Methods()

	if len(methods) != 22 {
		t.Fatalf("Methods() returned %d entries; want 22", len(methods))
	}

	for _, m := range methods {
		resp := d.Handle(context.Background(), makeReq(m))
		if resp == nil {
			t.Fatalf("Handle(%q) returned nil response", m)
		}
		if resp.Error == nil {
			t.Errorf("Handle(%q): expected NotImplemented error, got nil error (result=%v)", m, resp.Result)
			continue
		}
		if resp.Error.Type != string(cpierrors.TypeNotImplemented) {
			t.Errorf("Handle(%q): error type = %q; want %q", m, resp.Error.Type, cpierrors.TypeNotImplemented)
		}
	}
}

// --------------------------------------------------------------------------
// TestRegister_Overrides
// --------------------------------------------------------------------------

func TestRegister_Overrides(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	called := false
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		called = true
		return map[string]string{"api_version": "2"}, nil
	}))

	resp := d.Handle(context.Background(), makeReq("info"))
	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if !called {
		t.Error("custom handler was not called after Register")
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}

// --------------------------------------------------------------------------
// TestHandle_UnknownMethod
// --------------------------------------------------------------------------

func TestHandle_UnknownMethod(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	resp := d.Handle(context.Background(), makeReq("nonsense"))

	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("unknown-method error should not be retriable")
	}
}

// --------------------------------------------------------------------------
// TestHandle_HandlerReturnsResult
// --------------------------------------------------------------------------

func TestHandle_HandlerReturnsResult(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	want := map[string]string{"stemcell_id": "sc-abc123"}
	mustRegister(t, d, "create_stemcell", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return want, nil
	}))

	resp := d.Handle(context.Background(), makeReq("create_stemcell"))
	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	raw, ok := resp.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("Result is %T; want json.RawMessage", resp.Result)
	}

	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["stemcell_id"] != want["stemcell_id"] {
		t.Errorf("stemcell_id = %q; want %q", got["stemcell_id"], want["stemcell_id"])
	}
}

// --------------------------------------------------------------------------
// TestHandle_HandlerReturnsError
// --------------------------------------------------------------------------

func TestHandle_HandlerReturnsError(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	vmCID := "vm-missing-001"
	mustRegister(t, d, "delete_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, cpierrors.VMNotFound(vmCID)
	}))

	resp := d.Handle(context.Background(), makeReq("delete_vm"))
	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected error response; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeVMNotFound) {
		t.Errorf("error type = %q; want %q", resp.Error.Type, cpierrors.TypeVMNotFound)
	}
	if resp.Error.OkToRetry {
		t.Error("VMNotFound should not be retriable")
	}
}

// --------------------------------------------------------------------------
// TestHandle_HandlerReturnsPlainError
// --------------------------------------------------------------------------

func TestHandle_HandlerReturnsPlainError(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	mustRegister(t, d, "has_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, errors.New("pve api timeout")
	}))

	resp := d.Handle(context.Background(), makeReq("has_vm"))
	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected error response; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("plain error should be wrapped as CloudError; got %q", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("wrapped plain error should not be retriable")
	}
}

// --------------------------------------------------------------------------
// TestHandle_NotImplementedDefault
// --------------------------------------------------------------------------

func TestHandle_NotImplementedDefault(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	resp := d.Handle(context.Background(), makeReq("info"))

	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected NotImplemented error; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeNotImplemented) {
		t.Errorf("error type = %q; want NotImplemented", resp.Error.Type)
	}
}

// --------------------------------------------------------------------------
// TestMethods_Count22
// --------------------------------------------------------------------------

func TestMethods_Count22(t *testing.T) {
	methods := cpi.Methods()

	if len(methods) != 22 {
		t.Fatalf("Methods() returned %d entries; want 22", len(methods))
	}

	canonical := map[string]bool{
		"info":                          true,
		"create_stemcell":               true,
		"delete_stemcell":               true,
		"create_vm":                     true,
		"delete_vm":                     true,
		"has_vm":                        true,
		"reboot_vm":                     true,
		"set_vm_metadata":               true,
		"calculate_vm_cloud_properties": true,
		"create_disk":                   true,
		"delete_disk":                   true,
		"has_disk":                      true,
		"attach_disk":                   true,
		"detach_disk":                   true,
		"snapshot_disk":                 true,
		"delete_snapshot":               true,
		"get_disks":                     true,
		"resize_disk":                   true,
		"set_disk_metadata":             true,
		"update_disk":                   true,
		"create_network":                true,
		"delete_network":                true,
	}

	seen := make(map[string]bool, len(methods))
	for _, m := range methods {
		if seen[m] {
			t.Errorf("duplicate method name: %q", m)
		}
		seen[m] = true
		if !canonical[m] {
			t.Errorf("unexpected method name: %q", m)
		}
	}
	for name := range canonical {
		if !seen[name] {
			t.Errorf("missing canonical method: %q", name)
		}
	}
}

// --------------------------------------------------------------------------
// --------------------------------------------------------------------------
// TestDispatcher_ConcurrentRegisterHandle_NoDataRace
//
// Runs Register and Handle concurrently across multiple goroutines to verify
// no data race occurs under the -race detector. This test is meaningful only
// with `go test -race`; without the race detector it simply confirms correct
// results are returned without panics.
// --------------------------------------------------------------------------

func TestDispatcher_ConcurrentRegisterHandle_NoDataRace(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	const goroutines = 20
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Writer goroutines: concurrently register handlers.
		writers := make(chan struct{}, goroutines)
		for i := 0; i < goroutines; i++ {
			go func(n int) {
				defer func() { writers <- struct{}{} }()
				method := "info"
				if n%2 == 0 {
					method = "create_stemcell"
				}
				if err := d.Register(method, cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
					return map[string]int{"n": n}, nil
				})); err != nil {
					t.Errorf("goroutine %d: Register(%q): unexpected error: %v", n, method, err)
				}
			}(i)
		}
		// Reader goroutines: concurrently call Handle.
		readers := make(chan struct{}, goroutines)
		for i := 0; i < goroutines; i++ {
			go func(n int) {
				defer func() { readers <- struct{}{} }()
				method := "has_vm"
				if n%3 == 0 {
					method = "info"
				}
				resp := d.Handle(context.Background(), makeReq(method))
				if resp == nil {
					t.Errorf("goroutine %d: Handle returned nil", n)
				}
			}(i)
		}
		for i := 0; i < goroutines; i++ {
			<-writers
		}
		for i := 0; i < goroutines; i++ {
			<-readers
		}
	}()
	<-done
}

// --------------------------------------------------------------------------
// TestRegister_RejectsUnknownMethod
// --------------------------------------------------------------------------

func TestRegister_RejectsUnknownMethod(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	err := d.Register("not_a_real_method", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, nil
	}))
	if err == nil {
		t.Fatal("Register: expected error for unknown method; got nil")
	}
	if !strings.Contains(err.Error(), "unknown CPI method") {
		t.Errorf("Register: error message = %q; want to contain \"unknown CPI method\"", err.Error())
	}
}

// --------------------------------------------------------------------------
// TestRegister_AcceptsCanonicalMethod
// --------------------------------------------------------------------------

func TestRegister_AcceptsCanonicalMethod(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	err := d.Register("create_vm", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("Register(\"create_vm\"): unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// TestHandle_UnknownMethodReturnsMethodNotFound
// --------------------------------------------------------------------------

func TestHandle_UnknownMethodReturnsMethodNotFound(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())
	resp := d.Handle(context.Background(), makeReq("bogus"))

	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected error response for unknown method; got nil error")
	}
	// BOSH CPI uses CloudError for method-not-found (JSON-RPC -32601 equivalent).
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError (method-not-found)", resp.Error.Type)
	}
	if resp.Error.OkToRetry {
		t.Error("method-not-found error must not be retriable")
	}
	if !strings.Contains(resp.Error.Message, "method not found") {
		t.Errorf("error message = %q; want to contain \"method not found\"", resp.Error.Message)
	}
}

// TestHandle_ResultMarshalError
// --------------------------------------------------------------------------

func TestHandle_ResultMarshalError(t *testing.T) {
	d := cpi.NewDispatcher(nopLogger())

	// Channels are not JSON-serialisable; json.Marshal returns an error.
	mustRegister(t, d, "info", cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return make(chan int), nil // non-marshalable
	}))

	resp := d.Handle(context.Background(), makeReq("info"))
	if resp == nil {
		t.Fatal("Handle returned nil")
	}
	if resp.Error == nil {
		t.Fatal("expected CloudError for non-marshalable result; got nil error")
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q; want CloudError", resp.Error.Type)
	}
}
