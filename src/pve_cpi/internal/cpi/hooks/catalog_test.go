package hooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/hooks"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	l, err := log.NewLogger("debug", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return l
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// --- notes_audit ---------------------------------------------------------

type fakeAnnotator struct {
	calls  int
	vmid   int
	notes  string
	failed bool
}

func (f *fakeAnnotator) AnnotateNotes(_ context.Context, vmid int, notes string) error {
	f.calls++
	f.vmid = vmid
	f.notes = notes
	if f.failed {
		return errors.New("boom")
	}
	return nil
}

func createVMArgs(t *testing.T, env map[string]any, networks map[string]any) []json.RawMessage {
	t.Helper()
	return []json.RawMessage{
		raw(t, "agent-id"), raw(t, "stemcell"), raw(t, map[string]any{}),
		raw(t, networks), raw(t, []string{}), raw(t, env),
	}
}

func TestNotesAudit_WritesNotesOnSuccess(t *testing.T) {
	ann := &fakeAnnotator{}
	h := hooks.NewNotesAuditHook(hooks.Deps{Logger: testLogger(t), Annotator: ann})
	env := map[string]any{"bosh": map[string]any{"group": "d-dep-router", "id": "abc"}}
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, env, nil), jsonrpc.Context{})
	res, err := h.After(ctx, "create_vm", []any{"100", map[string]any{}}, nil)
	if err != nil {
		t.Fatalf("After err: %v", err)
	}
	if _, ok := res.([]any); !ok {
		t.Fatalf("result mutated: %#v", res)
	}
	if ann.calls != 1 || ann.vmid != 100 {
		t.Fatalf("annotator calls=%d vmid=%d; want 1/100", ann.calls, ann.vmid)
	}
	if !strings.Contains(ann.notes, "group=d-dep-router") || !strings.Contains(ann.notes, "id=abc") {
		t.Errorf("notes %q missing bosh identity", ann.notes)
	}
}

func TestNotesAudit_NoCallOnError(t *testing.T) {
	ann := &fakeAnnotator{}
	h := hooks.NewNotesAuditHook(hooks.Deps{Logger: testLogger(t), Annotator: ann})
	env := map[string]any{"bosh": map[string]any{"group": "g"}}
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, env, nil), jsonrpc.Context{})
	if _, err := h.After(ctx, "create_vm", nil, errors.New("handler failed")); err == nil {
		t.Fatal("expected handler error to pass through")
	}
	if ann.calls != 0 {
		t.Errorf("annotator must not run on error path; calls=%d", ann.calls)
	}
}

func TestNotesAudit_BestEffortSwallowsAnnotatorError(t *testing.T) {
	ann := &fakeAnnotator{failed: true}
	h := hooks.NewNotesAuditHook(hooks.Deps{Logger: testLogger(t), Annotator: ann})
	env := map[string]any{"bosh": map[string]any{"group": "g"}}
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, env, nil), jsonrpc.Context{})
	_, err := h.After(ctx, "create_vm", []any{"7", map[string]any{}}, nil)
	if err != nil {
		t.Fatalf("annotator failure must be swallowed; got %v", err)
	}
}

func TestNotesAudit_NilAnnotatorNoPanic(t *testing.T) {
	h := hooks.NewNotesAuditHook(hooks.Deps{Logger: testLogger(t)})
	env := map[string]any{"bosh": map[string]any{"group": "g"}}
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, env, nil), jsonrpc.Context{})
	if _, err := h.After(ctx, "create_vm", []any{"7", nil}, nil); err != nil {
		t.Fatalf("nil annotator must no-op; got %v", err)
	}
}

// --- lb_register ---------------------------------------------------------

func TestLBRegister_InertWhenNoConfig(t *testing.T) {
	h := hooks.NewLBRegisterHook(hooks.Deps{Logger: testLogger(t)})
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, nil, map[string]any{
		"default": map[string]any{"ip": "10.0.0.5"},
	}), jsonrpc.Context{})
	res, err := h.After(ctx, "create_vm", []any{"100", map[string]any{}}, nil)
	if err != nil {
		t.Fatalf("inert hook must pass through; got %v", err)
	}
	if _, ok := res.([]any); !ok {
		t.Fatalf("inert hook mutated result: %#v", res)
	}
}

func TestLBRegister_RegistersAndDeregistersViaDPA(t *testing.T) {
	var mu sync.Mutex
	var gotMethods []string
	var gotPaths []string
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotMethods = append(gotMethods, r.Method)
		gotPaths = append(gotPaths, r.URL.Path)
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&createBody)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	h := hooks.NewLBRegisterHook(hooks.Deps{
		Logger: testLogger(t),
		LBRegister: &hooks.LBRegisterConfig{
			Endpoint: srv.URL, Backend: "cf-routers", Port: 8080,
			AllowPrivateIP: true, TimeoutMS: 2000,
		},
	})

	// create_vm registers the VM as server "vm-100" at the static IP.
	ctx := h.Before(context.Background(), "create_vm", createVMArgs(t, nil, map[string]any{
		"default": map[string]any{"ip": "10.0.0.5"},
	}), jsonrpc.Context{})
	if _, err := h.After(ctx, "create_vm", []any{"100", map[string]any{}}, nil); err != nil {
		t.Fatalf("register After err: %v", err)
	}
	// delete_vm deregisters "vm-100".
	dctx := h.Before(context.Background(), "delete_vm", []json.RawMessage{raw(t, "100")}, jsonrpc.Context{})
	if _, err := h.After(dctx, "delete_vm", nil, nil); err != nil {
		t.Fatalf("deregister After err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotMethods) != 2 {
		t.Fatalf("expected 2 DPA calls (POST then DELETE); got %v %v", gotMethods, gotPaths)
	}
	if gotMethods[0] != http.MethodPost || gotMethods[1] != http.MethodDelete {
		t.Errorf("DPA call sequence = %v; want [POST DELETE]", gotMethods)
	}
	if name, _ := createBody["name"].(string); name != "vm-100" {
		t.Errorf("registered server name = %q; want vm-100 (body=%v)", name, createBody)
	}
	if addr, _ := createBody["address"].(string); addr != "10.0.0.5" {
		t.Errorf("registered address = %q; want 10.0.0.5", addr)
	}
	if !strings.Contains(gotPaths[0], "cf-routers") || !strings.Contains(gotPaths[1], "vm-100") {
		t.Errorf("paths = %v; want backend cf-routers + server vm-100", gotPaths)
	}
}

func TestLBRegister_RollbackDeregistersOnPostHookFailure(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	lbHook := hooks.NewLBRegisterHook(hooks.Deps{
		Logger: testLogger(t),
		LBRegister: &hooks.LBRegisterConfig{
			Endpoint: srv.URL, Backend: "cf-routers", Port: 8080,
			AllowPrivateIP: true, TimeoutMS: 2000,
		},
	})
	// A second post-hook flips the successful create to a failure, which must
	// drive the dispatch rollback and unwind the LB registration.
	failHook := cpi.HookFunc{
		AfterFn: func(_ context.Context, m string, r any, _ error) (any, error) {
			if m == "create_vm" {
				return r, errors.New("later post-hook failed")
			}
			return r, nil
		},
	}
	handler := cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return []any{"100", map[string]any{}}, nil
	})
	// Order matters: After fires in reverse, so lbHook (listed last) runs its
	// After first — it registers while err is still nil — then failHook flips the
	// error, driving the rollback that must deregister.
	wrapped := cpi.WrapHandler("create_vm", handler, []cpi.Hook{failHook, lbHook})

	args := createVMArgs(t, nil, map[string]any{"default": map[string]any{"ip": "10.0.0.5"}})
	_, err := wrapped.Handle(context.Background(), args, jsonrpc.Context{})
	if err == nil {
		t.Fatal("post-hook failure must propagate")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodDelete {
		t.Errorf("expected register POST then rollback-deregister DELETE; got %v", methods)
	}
}

// --- external_command ----------------------------------------------------

func TestExternalCommand_InertWhenEmptyAllowlist(t *testing.T) {
	h := hooks.NewExternalCommandHook(hooks.Deps{
		Logger:          testLogger(t),
		ExternalCommand: &hooks.ExternalCommandConfig{Command: "/bin/echo"},
	})
	res, err := h.After(context.Background(), "create_vm", []any{"5", nil}, nil)
	if err != nil {
		t.Fatalf("inert hook must pass through; got %v", err)
	}
	if _, ok := res.([]any); !ok {
		t.Fatalf("inert hook mutated result: %#v", res)
	}
}

func TestExternalCommand_RunsOnConfiguredMethod(t *testing.T) {
	const bin = "/bin/echo"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s missing: %v", bin, err)
	}
	var buf bytes.Buffer
	logger, _ := log.NewLogger("debug", &buf)
	h := hooks.NewExternalCommandHook(hooks.Deps{
		Logger: logger,
		ExternalCommand: &hooks.ExternalCommandConfig{
			Command: bin, Args: []string{"hook-ran"}, Allowlist: []string{bin},
			TimeoutMS: 2000, Methods: []string{"create_vm"},
		},
	})
	// Matching method runs.
	if _, err := h.After(context.Background(), "create_vm", []any{"42", nil}, nil); err != nil {
		t.Fatalf("After err: %v", err)
	}
	if !strings.Contains(buf.String(), "external_command: ran") {
		t.Errorf("expected run log; got %q", buf.String())
	}
	// Non-matching method is skipped (no run log for delete_vm).
	buf.Reset()
	if _, err := h.After(context.Background(), "delete_vm", nil, nil); err != nil {
		t.Fatalf("After err: %v", err)
	}
	if strings.Contains(buf.String(), "external_command: ran") {
		t.Errorf("delete_vm must not run (not in methods); got %q", buf.String())
	}
}

func TestExternalCommand_NoRunOnError(t *testing.T) {
	const bin = "/bin/echo"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s missing: %v", bin, err)
	}
	var buf bytes.Buffer
	logger, _ := log.NewLogger("debug", &buf)
	h := hooks.NewExternalCommandHook(hooks.Deps{
		Logger: logger,
		ExternalCommand: &hooks.ExternalCommandConfig{
			Command: bin, Allowlist: []string{bin}, TimeoutMS: 2000,
		},
	})
	if _, err := h.After(context.Background(), "create_vm", nil, errors.New("failed")); err == nil {
		t.Fatal("handler error must pass through")
	}
	if strings.Contains(buf.String(), "external_command: ran") {
		t.Errorf("must not run on error path; got %q", buf.String())
	}
}
