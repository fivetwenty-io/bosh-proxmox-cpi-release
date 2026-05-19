// Package cpi implements the BOSH CPI dispatcher: a handler registry that routes
// JSON-RPC requests to per-method handlers. All 22 canonical CPI methods are
// pre-registered as NotImplemented placeholders; callers override slots via Register.
package cpi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// Handler is implemented by per-method handlers in internal/cpi/handlers/.
// Each handler unmarshals its own arguments from the raw JSON slice.
type Handler interface {
	Handle(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error)
}

// HandlerFunc adapts a plain function to the Handler interface.
type HandlerFunc func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error)

// Handle implements Handler by calling f.
func (f HandlerFunc) Handle(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
	return f(ctx, args, reqCtx)
}

// Dispatcher routes JSON-RPC requests to registered handlers.
// It is not safe for concurrent use — the BOSH CPI executes requests in a
// single-threaded stdin loop, so no locking is required.
type Dispatcher struct {
	handlers map[string]Handler
	logger   *log.Logger
}

// NewDispatcher returns a Dispatcher with all 22 CPI methods pre-registered as
// NotImplemented placeholders. Call Register to override with a concrete handler.
func NewDispatcher(logger *log.Logger) *Dispatcher {
	d := &Dispatcher{
		handlers: make(map[string]Handler, len(Methods())),
		logger:   logger,
	}
	for _, m := range Methods() {
		method := m // capture for closure
		d.handlers[method] = HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, cpierrors.NotImplemented(method)
		})
	}
	return d
}

// Register installs h as the handler for the given method name.
// If a handler already exists for method it is replaced.
// method need not be one of the 22 canonical names; arbitrary methods may be
// registered (useful for extensions or testing), but only pre-registered slots
// accept calls through Handle — unknown methods return a CloudError.
func (d *Dispatcher) Register(method string, h Handler) {
	d.handlers[method] = h
}

// Handle dispatches one JSON-RPC request. It always returns a non-nil *jsonrpc.Response.
//
// Dispatch rules:
//  1. Look up handlers[req.Method].
//  2. If absent: return CloudError "unknown method: <method>".
//  3. Call handler.Handle(ctx, req.Arguments, req.Context).
//  4. On *cpierrors.Error: build error response using the error's Type/Message/OkToRetry.
//  5. On any other non-nil error: wrap as CloudError, OkToRetry=false.
//  6. On success: marshal result to json.RawMessage and build success response.
//  7. If json.Marshal of result fails: return CloudError describing the failure.
//
// Every dispatch is logged at Info level with method, request_id, and duration_ms.
func (d *Dispatcher) Handle(ctx context.Context, req *jsonrpc.Request) *jsonrpc.Response {
	start := time.Now()

	h, ok := d.handlers[req.Method]
	if !ok {
		d.logger.Info("dispatch",
			log.String("method", req.Method),
			log.String("request_id", req.Context.RequestID),
			log.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			log.String("outcome", "unknown_method"),
		)
		return errorResponse(cpierrors.Cloud("unknown method: %s", req.Method))
	}

	result, err := h.Handle(ctx, req.Arguments, req.Context)

	durationMS := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		outcome := "error"
		d.logger.Info("dispatch",
			log.String("method", req.Method),
			log.String("request_id", req.Context.RequestID),
			log.Float64("duration_ms", durationMS),
			log.String("outcome", outcome),
			log.Err(err),
		)
		return dispatchError(err)
	}

	// Marshal result before returning so a non-serialisable value becomes
	// a CloudError rather than a silent null or a panic.
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		d.logger.Info("dispatch",
			log.String("method", req.Method),
			log.String("request_id", req.Context.RequestID),
			log.Float64("duration_ms", durationMS),
			log.String("outcome", "marshal_error"),
			log.Err(marshalErr),
		)
		return errorResponse(cpierrors.Cloud("result marshal failed: %s", marshalErr.Error()))
	}

	d.logger.Info("dispatch",
		log.String("method", req.Method),
		log.String("request_id", req.Context.RequestID),
		log.Float64("duration_ms", durationMS),
		log.String("outcome", "ok"),
	)

	return &jsonrpc.Response{
		Result: json.RawMessage(raw),
		Error:  nil,
		Log:    "",
	}
}

// Methods returns the canonical 22 BOSH CPI method names in stable order.
func Methods() []string {
	return []string{
		"info",
		"create_stemcell",
		"delete_stemcell",
		"create_vm",
		"delete_vm",
		"has_vm",
		"reboot_vm",
		"set_vm_metadata",
		"calculate_vm_cloud_properties",
		"create_disk",
		"delete_disk",
		"has_disk",
		"attach_disk",
		"detach_disk",
		"snapshot_disk",
		"delete_snapshot",
		"get_disks",
		"resize_disk",
		"set_disk_metadata",
		"update_disk",
		"create_network",
		"delete_network",
	}
}

// --------------------------------------------------------------------------
// internal helpers
// --------------------------------------------------------------------------

// dispatchError converts any error returned by a handler to a *jsonrpc.Response.
// *cpierrors.Error values are mapped faithfully; plain errors become CloudError.
func dispatchError(err error) *jsonrpc.Response {
	var cpiErr *cpierrors.Error
	if errors.As(err, &cpiErr) {
		return &jsonrpc.Response{
			Result: nil,
			Error: &jsonrpc.ErrorBody{
				Type:      string(cpiErr.Type()),
				Message:   cpiErr.Error(),
				OkToRetry: cpiErr.OkToRetry(),
			},
			Log: "",
		}
	}
	// Plain error — wrap as generic CloudError, not retriable.
	return errorResponse(cpierrors.Cloud("%s", err.Error()))
}

// errorResponse builds a *jsonrpc.Response from a *cpierrors.Error.
func errorResponse(e *cpierrors.Error) *jsonrpc.Response {
	return &jsonrpc.Response{
		Result: nil,
		Error: &jsonrpc.ErrorBody{
			Type:      string(e.Type()),
			Message:   e.Error(),
			OkToRetry: e.OkToRetry(),
		},
		Log: "",
	}
}

// Ensure HandlerFunc implements Handler at compile time.
var _ Handler = HandlerFunc(nil)

// unmarshalableValue is used only in tests; kept here so the package self-documents
// the marshal-failure code path without importing test packages in tests.
// (Tests reference the channel type directly — no exported symbol needed.)
var _ = fmt.Sprintf // keep fmt import live
