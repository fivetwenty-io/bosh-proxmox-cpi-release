package cpi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// blockingHandler blocks until its context is cancelled, then returns the
// context error wrapped as a generic (non-retriable) cloud error — exactly how
// a real handler whose retry/poll loop observes ctx.Done() would behave when a
// deadline fires.
type blockingHandler struct {
	observed chan struct{}
}

func (h blockingHandler) Handle(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	<-ctx.Done()
	if h.observed != nil {
		close(h.observed)
	}
	return nil, cpierrors.Cloud("handler aborted: %v", ctx.Err())
}

// fastHandler returns success immediately, ignoring any deadline.
type fastHandler struct{}

func (fastHandler) Handle(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	return map[string]string{"ok": "yes"}, nil
}

func req(method string) *jsonrpc.Request {
	return &jsonrpc.Request{
		Method:    method,
		Arguments: nil,
		Context:   jsonrpc.Context{RequestID: "req-timeout-test"},
	}
}

// TestNewMethodTimeoutResolver_Classification verifies each canonical method
// maps to the right budget class, and that an unknown method falls to default.
func TestNewMethodTimeoutResolver_Classification(t *testing.T) {
	t.Parallel()

	const (
		create = 30 * time.Minute
		del    = 15 * time.Minute
		query  = 2 * time.Minute
		def    = 10 * time.Minute
	)
	r := cpi.NewMethodTimeoutResolver(create, del, query, def)

	cases := map[string]time.Duration{
		"create_vm":                     create,
		"create_stemcell":               create,
		"create_disk":                   create,
		"create_network":                create,
		"delete_vm":                     del,
		"delete_stemcell":               del,
		"delete_disk":                   del,
		"delete_snapshot":               del,
		"delete_network":                del,
		"info":                          query,
		"has_vm":                        query,
		"has_disk":                      query,
		"get_disks":                     query,
		"calculate_vm_cloud_properties": query,
		"reboot_vm":                     def,
		"attach_disk":                   def,
		"detach_disk":                   def,
		"snapshot_disk":                 def,
		"resize_disk":                   def,
		"set_vm_metadata":               def,
		"set_disk_metadata":             def,
		"update_disk":                   def,
		"some_unknown_method":           def,
	}
	for method, want := range cases {
		if got := r(method); got != want {
			t.Errorf("method %q → %v, want %v", method, got, want)
		}
	}

	// Every canonical method must resolve to a positive budget (no method
	// silently falls through to 0 when all classes are non-zero).
	for _, m := range cpi.Methods() {
		if r(m) <= 0 {
			t.Errorf("canonical method %q resolved to non-positive budget", m)
		}
	}
}

// TestNewMethodTimeoutResolver_ZeroClassDisables verifies a 0 class duration
// disables the envelope for that class (resolver returns 0 → no wrap).
func TestNewMethodTimeoutResolver_ZeroClassDisables(t *testing.T) {
	t.Parallel()
	r := cpi.NewMethodTimeoutResolver(time.Minute, time.Minute, 0, time.Minute)
	if got := r("has_vm"); got != 0 {
		t.Errorf("query class with 0 budget → %v, want 0 (disabled)", got)
	}
}

// TestDispatcher_MethodTimeout_FiresRetriable verifies that when the per-method
// deadline elapses before the handler returns, the dispatcher returns a
// RETRIABLE CloudError naming the method, and that the handler actually
// observed the cancellation.
func TestDispatcher_MethodTimeout_FiresRetriable(t *testing.T) {
	t.Parallel()

	observed := make(chan struct{})
	resolver := func(method string) time.Duration {
		if method == "create_vm" {
			return 20 * time.Millisecond
		}
		return 0
	}
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithMethodTimeouts(resolver))
	mustRegister(t, d, "create_vm", blockingHandler{observed: observed})

	resp := d.Handle(context.Background(), req("create_vm"))

	if resp.Error == nil {
		t.Fatalf("expected error response, got success: %+v", resp)
	}
	if !resp.Error.OkToRetry {
		t.Errorf("timeout error must be retriable, got OkToRetry=false: %s", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "create_vm") ||
		!strings.Contains(resp.Error.Message, "deadline") {
		t.Errorf("message %q should name the method and mention the deadline", resp.Error.Message)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Error("handler did not observe context cancellation")
	}
}

// TestDispatcher_MethodTimeout_ZeroBudgetNoWrap verifies that a resolver
// returning 0 for a method leaves dispatch unwrapped: a handler that blocks
// briefly and then returns success still succeeds (no deadline fires).
func TestDispatcher_MethodTimeout_ZeroBudgetNoWrap(t *testing.T) {
	t.Parallel()

	resolver := func(string) time.Duration { return 0 } // never wrap
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithMethodTimeouts(resolver))
	mustRegister(t, d, "has_vm", fastHandler{})

	resp := d.Handle(context.Background(), req("has_vm"))
	if resp.Error != nil {
		t.Fatalf("expected success, got error: %s", resp.Error.Message)
	}
}

// TestDispatcher_MethodTimeout_FastHandlerSucceeds verifies that a handler that
// completes within budget returns its real result, not a timeout.
func TestDispatcher_MethodTimeout_FastHandlerSucceeds(t *testing.T) {
	t.Parallel()

	resolver := func(string) time.Duration { return time.Hour } // generous
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithMethodTimeouts(resolver))
	mustRegister(t, d, "create_vm", fastHandler{})

	resp := d.Handle(context.Background(), req("create_vm"))
	if resp.Error != nil {
		t.Fatalf("expected success within budget, got error: %s", resp.Error.Message)
	}
	raw, ok := resp.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("result is %T, want json.RawMessage", resp.Result)
	}
	if !strings.Contains(string(raw), "yes") {
		t.Errorf("result not preserved: %s", string(raw))
	}
}

// TestDispatcher_NoResolver_BehaviorUnchanged verifies that without the option
// installed the context flows through unchanged (no deadline) — the
// behavior-preserving default.
func TestDispatcher_NoResolver_BehaviorUnchanged(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", fastHandler{})

	resp := d.Handle(context.Background(), req("create_vm"))
	if resp.Error != nil {
		t.Fatalf("expected success, got error: %s", resp.Error.Message)
	}
}

// TestDispatcher_MethodTimeout_ParentCancelNotTimeout verifies that a handler
// erroring due to PARENT context cancellation (signal shutdown), not the
// per-method deadline, is NOT relabeled as a retriable timeout. The parent is
// cancelled while the per-method budget is large, so callCtx.Err() is
// context.Canceled, not DeadlineExceeded.
func TestDispatcher_MethodTimeout_ParentCancelNotTimeout(t *testing.T) {
	t.Parallel()

	resolver := func(string) time.Duration { return time.Hour } // budget far away
	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithMethodTimeouts(resolver))
	mustRegister(t, d, "create_vm", blockingHandler{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	resp := d.Handle(ctx, req("create_vm"))
	if resp.Error == nil {
		t.Fatalf("expected error, got success")
	}
	// Must NOT be relabeled as a retriable deadline timeout. The handler's own
	// (non-retriable) error must survive.
	if strings.Contains(resp.Error.Message, "deadline") {
		t.Errorf("parent cancellation should not be relabeled as a deadline timeout: %s", resp.Error.Message)
	}
}
