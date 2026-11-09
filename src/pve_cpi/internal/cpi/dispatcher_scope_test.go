package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// scopeProbe is a handler that records the affinity scope on its context and
// registers a release function that counts how often it ran.
type scopeProbe struct {
	scope          *endpointpool.Scope
	releasedInside bool
	releaseRuns    atomic.Int32
	ranBeforeEnd   atomic.Bool
	finished       atomic.Bool
	run            func(ctx context.Context) (any, error)
}

func (p *scopeProbe) Handle(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	p.scope = endpointpool.ScopeFromContext(ctx)
	if p.scope != nil {
		p.releasedInside = p.scope.Released()
		p.scope.OnRelease(func() {
			p.releaseRuns.Add(1)
			if !p.finished.Load() {
				p.ranBeforeEnd.Store(true)
			}
		})
	}
	defer p.finished.Store(true)
	if p.run == nil {
		return "ok", nil
	}
	return p.run(ctx)
}

func scopeRequest(method string) *jsonrpc.Request {
	return &jsonrpc.Request{
		Method:     method,
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "scope-req"},
		APIVersion: 2,
	}
}

func TestDispatcher_HandlerContextCarriesLiveScope(t *testing.T) {
	t.Parallel()

	probe := &scopeProbe{}
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", probe)

	resp := d.Handle(context.Background(), scopeRequest("create_vm"))
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	if probe.scope == nil {
		t.Fatal("handler context carries no affinity scope")
	}
	if probe.releasedInside {
		t.Error("scope was already released while the handler ran")
	}
	if !probe.scope.Released() {
		t.Error("scope was not released when the call returned")
	}
	if got := probe.releaseRuns.Load(); got != 1 {
		t.Errorf("release functions ran %d times; want 1", got)
	}
	if probe.ranBeforeEnd.Load() {
		t.Error("release ran before the handler returned")
	}
}

func TestDispatcher_ScopeReleasedWhenHandlerErrors(t *testing.T) {
	t.Parallel()

	probe := &scopeProbe{run: func(context.Context) (any, error) { return nil, errors.New("handler failed") }}
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", probe)

	resp := d.Handle(context.Background(), scopeRequest("create_vm"))
	if resp.Error == nil {
		t.Fatal("expected an error response")
	}
	if !probe.scope.Released() || probe.releaseRuns.Load() != 1 || probe.ranBeforeEnd.Load() {
		t.Errorf("released=%v runs=%d early=%v; want released once after the handler",
			probe.scope.Released(), probe.releaseRuns.Load(), probe.ranBeforeEnd.Load())
	}
}

func TestDispatcher_ScopeReleasedWhenHandlerPanics(t *testing.T) {
	t.Parallel()

	probe := &scopeProbe{run: func(context.Context) (any, error) { panic("handler blew up") }}
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", probe)

	resp := d.Handle(context.Background(), scopeRequest("create_vm"))
	if resp.Error == nil {
		t.Fatal("expected an error response after the panic")
	}
	if !probe.scope.Released() || probe.releaseRuns.Load() != 1 {
		t.Errorf("released=%v runs=%d; want released once", probe.scope.Released(), probe.releaseRuns.Load())
	}
}

func TestDispatcher_ScopeReleasedWhenDeadlineFires(t *testing.T) {
	t.Parallel()

	probe := &scopeProbe{run: func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	d := cpi.NewDispatcherWithOptions(nopLogger(),
		cpi.WithMethodTimeouts(func(string) time.Duration { return 20 * time.Millisecond }))
	mustRegister(t, d, "create_vm", probe)

	resp := d.Handle(context.Background(), scopeRequest("create_vm"))
	if resp.Error == nil {
		t.Fatal("expected a timeout error response")
	}
	if probe.scope == nil || !probe.scope.Released() || probe.releaseRuns.Load() != 1 {
		t.Errorf("scope not released exactly once after the deadline: %+v", probe.scope)
	}
}

func TestDispatcher_EachCallGetsItsOwnScope(t *testing.T) {
	t.Parallel()

	first, second := &scopeProbe{}, &scopeProbe{}
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, "create_vm", first)
	mustRegister(t, d, "delete_vm", second)

	d.Handle(context.Background(), scopeRequest("create_vm"))
	d.Handle(context.Background(), scopeRequest("delete_vm"))
	if first.scope == nil || second.scope == nil {
		t.Fatal("a handler context carried no scope")
	}
	if first.scope == second.scope {
		t.Error("two calls shared one affinity scope")
	}
}
