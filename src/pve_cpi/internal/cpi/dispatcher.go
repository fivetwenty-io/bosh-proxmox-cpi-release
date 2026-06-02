package cpi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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
// It is safe for concurrent use: Register holds a write lock and Handle holds
// a read lock. BOSH dispatches sequentially over stdin so locking is not
// required in production, but concurrent tests and future extensions are safe.
type Dispatcher struct {
	mu           sync.RWMutex
	handlers     map[string]Handler
	allowedNames map[string]struct{}
	logger       *log.Logger
	// hooks is the middleware chain applied to each handler at Register time.
	// nil (the default, and what NewDispatcher produces) means Register installs
	// handlers verbatim — identical call stack and zero overhead versus prior
	// releases. Populated via WithHooks through NewDispatcherWithOptions.
	hooks []Hook
}

// NewDispatcher returns a Dispatcher with all 22 CPI methods pre-registered as
// NotImplemented placeholders. Call Register to override with a concrete handler.
// The canonical allow-list is built once from Methods() at construction time for
// O(1) lookup on every Register and Handle call.
func NewDispatcher(logger *log.Logger) *Dispatcher {
	methods := Methods()
	allowed := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		allowed[m] = struct{}{}
	}

	d := &Dispatcher{
		handlers:     make(map[string]Handler, len(methods)),
		allowedNames: allowed,
		logger:       logger,
	}
	for _, m := range methods {
		method := m // capture for closure
		d.handlers[method] = HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, cpierrors.NotImplemented(method)
		})
	}
	return d
}

// NewDispatcherWithOptions returns a Dispatcher configured by the supplied
// options. It is equivalent to NewDispatcher(logger) with each option applied
// in order before any handler is registered, so a WithHooks option installed
// here is honored by every subsequent Register call. With no options it behaves
// exactly like NewDispatcher.
func NewDispatcherWithOptions(logger *log.Logger, opts ...func(*Dispatcher)) *Dispatcher {
	d := NewDispatcher(logger)
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// WithHooks returns an option that sets the dispatcher's middleware chain. The
// hooks apply to every handler registered after the option runs (i.e. every
// RegisterAll handler). Passing no hooks leaves the chain empty, which is the
// same as not using the option at all.
func WithHooks(hooks ...Hook) func(*Dispatcher) {
	return func(d *Dispatcher) {
		d.hooks = hooks
	}
}

// Register installs h as the handler for the given method name.
// If a handler already exists for method it is replaced.
// Register returns an error when method is not a canonical CPI method name.
// Canonical names are the 22 methods returned by Methods().
// Register is safe to call concurrently with other Register or Handle calls.
func (d *Dispatcher) Register(method string, h Handler) error {
	if _, ok := d.allowedNames[method]; !ok {
		methods := Methods()
		return fmt.Errorf("register: unknown CPI method %q; canonical set: %v", method, methods)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// WrapHandler returns h unchanged when d.hooks is empty, so an unhooked
	// dispatcher installs the handler verbatim.
	d.handlers[method] = WrapHandler(method, h, d.hooks)
	return nil
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

	// Reject non-canonical method names before consulting the handlers map.
	// This is the allow-list guard: even if a handler were somehow installed
	// for a non-canonical name (impossible via Register but defensive), the
	// Director never receives a response for it.
	if _, allowed := d.allowedNames[req.Method]; !allowed {
		d.logger.Info("dispatch",
			log.String("method", req.Method),
			log.String("request_id", req.Context.RequestID),
			log.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			log.String("outcome", "method_not_found"),
		)
		return errorResponse(cpierrors.Cloud("method not found: %s", req.Method))
	}

	d.mu.RLock()
	h, ok := d.handlers[req.Method]
	d.mu.RUnlock()
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
