package cpi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
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
	// methodTimeout resolves the per-method deadline budget. nil (the default)
	// means no deadline wraps handler execution — context flows through
	// unchanged, identical to prior releases. A non-nil resolver that returns
	// 0 for a method also means "no deadline for this method". Populated via
	// WithMethodTimeouts. The dispatcher takes a plain func rather than a config
	// value so it stays decoupled from the config package.
	methodTimeout func(method string) time.Duration
	// requestTrace, when true, makes Handle emit a debug-level trace of each
	// request's arguments and the handler's result, with credentials masked by
	// log.RedactSecrets. false (the default) emits nothing extra — the log
	// stream is byte-identical to prior releases. A plain bool keeps the
	// dispatcher decoupled from the config package; main wires it from
	// pve.redact_logs via WithRequestTrace.
	requestTrace bool
	// durationRecorder, when non-nil, is invoked exactly once per dispatched
	// request with the request's ctx, method, final outcome, and elapsed
	// duration in milliseconds. "Final" means after every reclassification
	// Handle itself performs on the outcome a handler (and its hooks)
	// originally returned — in particular the per-method timeout rewrite
	// (still recorded with the same outcome the wrapped handler observed, not
	// a distinct "timeout" value) and a result that fails json.Marshal
	// (recorded as "marshal_error", a case no hook running inside the handler
	// call can ever observe), plus a recovered handler panic (recorded as
	// "error", matching the error response the Director receives). Requests
	// rejected before a handler runs — nil request, non-canonical or
	// unregistered method — are never recorded. The ctx passed is always
	// Handle's own request
	// ctx (never callCtx, the possibly-timeout-wrapped context passed to the
	// handler) so a recorder reading span/exemplar data from it is unaffected
	// by the per-method deadline envelope; a ctx already canceled or expired
	// by the time a branch records (e.g. the timeout and marshal_error
	// branches) is expected and fine, since the recorder only reads from it,
	// never dials out. nil (the default) means Handle makes no extra call, so
	// an unconfigured dispatcher pays zero overhead. Populated via
	// WithDurationRecorder.
	durationRecorder func(ctx context.Context, method, outcome string, durationMs float64)

	// transientClassifier, when non-nil, is consulted by dispatchError's
	// plain-error fallback: an error it reports as transient surfaces as a
	// retriable CloudError instead of the permanent generic wrap. Populated
	// via WithTransientClassifier (production wires pve.IsTransientTransport;
	// the dispatcher cannot import internal/pve itself, since internal/pve
	// depends on internal/config, which depends on internal/cpi/hooks, which
	// depends on this package). nil (the default) keeps the fallback
	// permanent for every plain error.
	transientClassifier func(error) bool
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

// WithMethodTimeouts returns an option that installs a per-method deadline
// resolver. For each dispatched request the resolver is consulted with the
// method name; a positive duration wraps the handler in a context.WithTimeout
// of that size, and a zero (or a nil resolver) leaves the context unwrapped.
// When the deadline fires before the handler returns, Handle converts the
// resulting error into a retriable CloudError so the Director retries the
// operation rather than treating a wedged call as a permanent failure.
func WithMethodTimeouts(resolver func(method string) time.Duration) func(*Dispatcher) {
	return func(d *Dispatcher) {
		d.methodTimeout = resolver
	}
}

// WithRequestTrace returns an option that toggles the redacted request/response
// trace. When enabled, Handle emits a debug-level "cpi request" record (the
// argument tree) before invoking the handler and a "cpi response" record (the
// result tree) after a successful call, each passed through log.RedactSecrets so
// mbus, blobstore, and registry credentials are masked. Disabled (the default)
// adds no log records and no per-call work — byte-identical to prior releases.
func WithRequestTrace(enabled bool) func(*Dispatcher) {
	return func(d *Dispatcher) {
		d.requestTrace = enabled
	}
}

// WithDurationRecorder returns an option that installs a callback Handle calls
// exactly once per dispatched request, after the request's final outcome is
// known, with the request's ctx, the method, the outcome, and elapsed
// duration in milliseconds. This is the sole seam by which a metrics backend
// observes per-action duration — no hook can substitute for it, because a
// hook's After runs inside the wrapped handler call, before Handle's own
// post-handler steps (the timeout rewrite and the json.Marshal of the result)
// have run, and so a hook can never see a marshal failure and would
// misreport a since-rewritten timeout as the handler's original (pre-rewrite)
// outcome. ctx is always Handle's own request ctx, letting a recorder read
// span/exemplar data from it (e.g. to correlate the metric point with the
// root span); it may already be canceled or expired at some call sites
// (timeout, marshal_error) — that is expected, since the recorder is expected
// only to read from ctx, never to dial out with it. The recorder is a plain
// func rather than an OTel type so the dispatcher stays decoupled from the
// metrics package. The default (no option, or an explicit nil recorder) means
// Handle performs no extra call — byte-identical to prior releases. The
// recorder must not block or fail the dispatched action: it runs inline on
// the dispatch path, so a slow or panicking recorder would itself delay or
// crash request handling; callers are responsible for making their callback
// fast and panic-free.
func WithDurationRecorder(recorder func(ctx context.Context, method, outcome string, durationMs float64)) func(*Dispatcher) {
	return func(d *Dispatcher) {
		d.durationRecorder = recorder
	}
}

// WithTransientClassifier returns an option that installs the predicate the
// plain-error fallback in dispatchError consults before minting a permanent
// CloudError. It exists as an injected seam purely for layering: the
// production wiring passes pve.IsTransientTransport, which this package
// cannot import directly (see the transientClassifier field comment). A nil
// classifier (the default) keeps every unclassified plain error permanent.
func WithTransientClassifier(classifier func(error) bool) func(*Dispatcher) {
	return func(d *Dispatcher) {
		d.transientClassifier = classifier
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
//  8. If the handler panics: recover, emit a non-retriable CloudError with method
//     context and request_id, log the stack trace at error level.
//
// Every dispatch is logged at Info level with method, request_id, and duration_ms.
//
// Every log line below also carries trace_id/span_id (via requestFields,
// which appends log.SpanFields(ctx)) when ctx carries a valid OTel span —
// e.g. the root span cmd/cpi's runCPI starts before calling Handle. d.logger
// is the fixed process-startup logger (not derived from ctx via WithContext),
// so appending SpanFields never duplicates the method/request_id fields each
// line already attaches explicitly.
func (d *Dispatcher) Handle(ctx context.Context, req *jsonrpc.Request) (resp *jsonrpc.Response) {
	start := time.Now()

	// Recover from any panic in the handler. A panic means the handler hit an
	// unrecoverable condition (e.g., nil-deref). Convert it to a non-retriable
	// CloudError so the Director receives a structured response and the process
	// stays alive to serve subsequent requests.
	//
	// Nil-req guard: if req is nil (should not happen in production, but is
	// theoretically reachable via a bug in runCPI or tests), reading req.Method /
	// req.Context inside the defer would itself panic — unrecovered. Extract the
	// safe values before the defer so the closure captures copies.
	method := "unknown"
	requestID := ""
	if req != nil {
		method = req.Method
		requestID = req.Context.RequestID
	}
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			d.logger.Error("handler panic recovered", requestFields(ctx, method, requestID,
				log.Any("panic", r),
				log.String("stack", string(stack)),
			)...)
			resp = errorResponse(cpierrors.Cloud("panic in %s [request_id=%s]: %v", method, requestID, r))
			// The Director receives an error response for this request, and the
			// root span records it as a failure; record the histogram the same
			// way so a panicking handler cannot make the two disagree.
			d.recordDuration(ctx, method, "error", float64(time.Since(start).Microseconds())/1000.0)
		}
	}()

	// After the safe-extract above, a nil req would panic on req.Method below.
	// Return a typed error immediately rather than letting the recover fire for
	// what is effectively a caller bug.
	if req == nil {
		return errorResponse(cpierrors.Cloud("dispatcher: nil request"))
	}

	// Reject non-canonical method names before consulting the handlers map.
	// This is the allow-list guard: even if a handler were somehow installed
	// for a non-canonical name (impossible via Register but defensive), the
	// Director never receives a response for it.
	if _, allowed := d.allowedNames[req.Method]; !allowed {
		d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
			log.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			log.String("outcome", "method_not_found"),
		)...)
		return errorResponse(cpierrors.Cloud("method not found: %s", req.Method))
	}

	d.mu.RLock()
	h, ok := d.handlers[req.Method]
	d.mu.RUnlock()
	if !ok {
		d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
			log.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000.0),
			log.String("outcome", "unknown_method"),
		)...)
		return errorResponse(cpierrors.Cloud("unknown method: %s", req.Method))
	}

	// Per-method deadline envelope (opt-in). When a resolver is installed and
	// returns a positive budget, wrap the handler context so a wedged retry or
	// poll loop cannot hold the request indefinitely. The handler's own retry
	// loops and task polls already observe ctx.Done(), so this composes without
	// any handler change. A zero budget (or nil resolver) leaves ctx untouched.
	callCtx := ctx
	var budget time.Duration
	if d.methodTimeout != nil {
		if budget = d.methodTimeout(req.Method); budget > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, budget)
			defer cancel()
		}
	}

	d.traceRequest(ctx, req.Method, requestID, req.Arguments)

	result, err := h.Handle(callCtx, req.Arguments, req.Context)

	// If our deadline fired before the handler returned, translate whatever the
	// handler reported into a retriable timeout so the Director gets a clear,
	// actionable signal. Only do this when the handler actually errored: a
	// handler that returned success just as the deadline elapsed genuinely
	// succeeded and its result must not be clobbered. Parent (signal) shutdown
	// is deliberately excluded — that is process shutdown, not a per-operation
	// budget overrun. The ctx.Err()==nil guard closes the razor-thin race where
	// the parent is cancelled at the same instant the child deadline fires: in
	// that window callCtx.Err() may report DeadlineExceeded even though the real
	// cause was shutdown, so we additionally require the parent to be live.
	if err != nil && budget > 0 && callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		durationMS := float64(time.Since(start).Microseconds()) / 1000.0
		d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
			log.Float64("duration_ms", durationMS),
			log.String("outcome", "timeout"),
			log.ErrScrubbed(err),
		)...)
		// Recorded as "error", not "timeout": this mirrors what the wrapped
		// handler's hooks already observed inside h.Handle above (the
		// handler's own non-nil error), before this rewrite ran. The duration
		// metric's outcome vocabulary is success/error/marshal_error only;
		// the "timeout" string is a dispatch-log-only distinction.
		d.recordDuration(ctx, req.Method, "error", durationMS)
		return d.dispatchError(req.Method, cpierrors.Retriable(
			"operation %s exceeded its %s deadline [request_id=%s]; aborted and may be retried",
			req.Method, budget, requestID))
	}

	durationMS := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		outcome := "error"
		d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
			log.Float64("duration_ms", durationMS),
			log.String("outcome", outcome),
			log.ErrScrubbed(err),
		)...)
		d.recordDuration(ctx, req.Method, outcome, durationMS)
		return d.dispatchError(req.Method, err)
	}

	// Marshal result before returning so a non-serialisable value becomes
	// a CloudError rather than a silent null or a panic.
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
			log.Float64("duration_ms", durationMS),
			log.String("outcome", "marshal_error"),
			log.Err(marshalErr),
		)...)
		d.recordDuration(ctx, req.Method, "marshal_error", durationMS)
		return errorResponse(cpierrors.Cloud("result marshal failed: %s", marshalErr.Error()))
	}

	d.traceResponse(ctx, req.Method, requestID, result)

	d.logger.Info("dispatch", requestFields(ctx, req.Method, req.Context.RequestID,
		log.Float64("duration_ms", durationMS),
		log.String("outcome", "ok"),
	)...)
	d.recordDuration(ctx, req.Method, "success", durationMS)

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

// methodClass partitions the canonical CPI methods into the four budget
// classes the operation-timeout envelope sizes deadlines by. Any method not
// listed in create/delete/query falls into the default class. Keeping this map
// beside Methods() means a new method is a compile-adjacent edit, not a hunt
// across packages.
var methodClass = map[string]string{
	// create class
	"create_stemcell": "create",
	"create_vm":       "create",
	"create_disk":     "create",
	"create_network":  "create",
	// delete class
	"delete_stemcell": "delete",
	"delete_vm":       "delete",
	"delete_disk":     "delete",
	"delete_snapshot": "delete",
	"delete_network":  "delete",
	// query class (read-only / cheap)
	"info":                          "query",
	"has_vm":                        "query",
	"has_disk":                      "query",
	"get_disks":                     "query",
	"calculate_vm_cloud_properties": "query",
	// everything else (reboot_vm, set_vm_metadata, attach_disk, detach_disk,
	// snapshot_disk, resize_disk, set_disk_metadata, update_disk) → default.
}

// NewMethodTimeoutResolver returns a per-method deadline resolver suitable for
// WithMethodTimeouts. It classifies each method into create/delete/query and
// returns the matching budget; any other (or unknown) method gets def. A class
// duration of 0 (or any negative value, which is normalized to 0) disables the
// envelope for that class (the resolver returns 0, which WithMethodTimeouts
// treats as "do not wrap"). The four arguments are typically built from the
// operator's operation_timeout config.
func NewMethodTimeoutResolver(create, del, query, def time.Duration) func(string) time.Duration {
	// Normalize negatives to 0 ("disabled") so a bad config value can never
	// produce a context.WithTimeout with a negative (already-expired) deadline.
	nonNeg := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		return d
	}
	create, del, query, def = nonNeg(create), nonNeg(del), nonNeg(query), nonNeg(def)
	return func(method string) time.Duration {
		switch methodClass[method] {
		case "create":
			return create
		case "delete":
			return del
		case "query":
			return query
		default:
			return def
		}
	}
}

// --------------------------------------------------------------------------
// internal helpers
// --------------------------------------------------------------------------

// requestFields builds the field list for one dispatcher log line: method and
// request_id (explicit, always present), trace_id/span_id from ctx when a
// valid OTel span is attached (via log.SpanFields — nil/empty when tracing is
// inactive, so byte-identical output to prior releases in that case), then
// any call-specific extra fields (duration_ms, outcome, error, ...).
func requestFields(ctx context.Context, method, requestID string, extra ...log.Field) []log.Field {
	fields := make([]log.Field, 0, 4+len(extra))
	fields = append(fields, log.String("method", method), log.String("request_id", requestID))
	fields = append(fields, log.SpanFields(ctx)...)
	fields = append(fields, extra...)
	return fields
}

// traceRequest emits a debug-level, credential-masked trace of a request's
// argument tree. It is a no-op unless the request trace is enabled, so a
// dispatcher without WithRequestTrace produces byte-identical log output. Each
// raw argument is decoded to a generic tree and passed through
// log.RedactSecrets; an argument that fails to decode is logged as an opaque
// placeholder rather than its raw bytes, so a malformed payload can never leak
// an unredacted credential.
func (d *Dispatcher) traceRequest(ctx context.Context, method, requestID string, args []json.RawMessage) {
	if !d.requestTrace {
		return
	}
	redacted := make([]any, len(args))
	for i, raw := range args {
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			redacted[i] = "<unparsable argument>"
			continue
		}
		redacted[i] = log.RedactSecrets(tree)
	}
	d.logger.Debug("cpi request", requestFields(ctx, method, requestID,
		log.Any("arguments", redacted),
	)...)
}

// traceResponse emits a debug-level, credential-masked trace of a handler's
// result tree. Like traceRequest it is a no-op unless the trace is enabled. The
// result is round-tripped through JSON so that a typed struct is normalized to
// the same generic tree shape RedactSecrets operates on; a result that fails to
// marshal is logged as an opaque placeholder.
func (d *Dispatcher) traceResponse(ctx context.Context, method, requestID string, result any) {
	if !d.requestTrace {
		return
	}
	var tree any
	if raw, err := json.Marshal(result); err != nil {
		tree = "<unserializable result>"
	} else if err := json.Unmarshal(raw, &tree); err != nil {
		tree = "<unparsable result>"
	}
	d.logger.Debug("cpi response", requestFields(ctx, method, requestID,
		log.Any("result", log.RedactSecrets(tree)),
	)...)
}

// recordDuration calls d.durationRecorder when one is installed, centralizing
// the nil-guard so every Handle call site is a single line. ctx is always the
// caller's request ctx (see WithDurationRecorder). A dispatcher built without
// WithDurationRecorder (the default) makes this a no-op check with no further
// cost.
func (d *Dispatcher) recordDuration(ctx context.Context, method, outcome string, durationMS float64) {
	if d.durationRecorder != nil {
		d.durationRecorder(ctx, method, outcome, durationMS)
	}
}

// dispatchError converts any error returned by a handler to a *jsonrpc.Response.
// *cpierrors.Error values keep their type through wireErrorBody's translation;
// plain errors become CloudError. method is the JSON-RPC method the error came
// from, which wireErrorBody needs to pick the Director's retriable class.
//
// The message is scrubbed at this choke point — the single funnel every
// handler error passes through — because the Director persists it in its
// task/event records: an error embedding a presigned or userinfo-bearing URL
// (a failed stemcell fetch, for example) must not reach that sink verbatim.
// This mirrors what endRootSpanErr already does for the trace exporter.
func (d *Dispatcher) dispatchError(method string, err error) *jsonrpc.Response {
	var cpiErr *cpierrors.Error
	if errors.As(err, &cpiErr) {
		return &jsonrpc.Response{
			Result: nil,
			Error:  wireErrorBody(method, cpiErr),
			Log:    "",
		}
	}
	// Last-resort transport classification: no live handler path lets a raw
	// transport error reach this fallback today, but a future handler
	// regression must not convert a transient fault (a dropped connection, a
	// cycling pveproxy) into a permanent failure the Director will not retry.
	if d.transientClassifier != nil && d.transientClassifier(err) {
		return &jsonrpc.Response{
			Result: nil,
			Error: wireErrorBody(method, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				"transient transport fault")),
			Log: "",
		}
	}
	// Plain error — wrap as generic CloudError, not retriable.
	return errorResponse(cpierrors.Cloud("%s", err.Error()))
}

// wireErrorBody builds the JSON-RPC error body for e, translating the CPI's
// internal error taxonomy into the class names the Director recognizes. The
// Director resolves the wire type against a fixed allow-list (KNOWN_ERRORS in
// bosh-director's clouds/external_cpi.rb) and raises "Unknown CPI error" for
// any other string, discarding ok_to_retry with it, so an internal type that
// crossed the wire unmapped would turn a retriable fault into a hard
// deployment failure.
//
//   - TypeRetriableCloud is the internal retry marker. The Director knows
//     RetriableCloudError only as an abstract base class, never as a wire
//     type. For create_vm the subclass its create step acts on is
//     VMCreationFailed: with ok_to_retry set, the Director re-invokes
//     create_vm with identical arguments up to max_vm_create_tries. No other
//     method has a Director-side retry loop, so every other method maps to
//     CloudError with the ok_to_retry flag carried through unchanged.
//   - TypeDetachedDisk names the condition the Director calls
//     DiskNotAttached.
//   - The stemcell validation types and SnapshotBlocked are internal
//     diagnostics; on the wire they are ordinary non-retriable CloudErrors.
//
// The message is scrubbed here for the reason given on dispatchError.
func wireErrorBody(method string, e *cpierrors.Error) *jsonrpc.ErrorBody {
	typ := e.Type()
	switch typ {
	case cpierrors.TypeRetriableCloud:
		if method == "create_vm" {
			typ = cpierrors.TypeVMCreationFailed
		} else {
			typ = cpierrors.TypeCloud
		}
	case cpierrors.TypeDetachedDisk:
		typ = cpierrors.TypeDiskNotAttached
	case cpierrors.TypeSnapshotBlocked,
		cpierrors.TypeStemcellExtractCap,
		cpierrors.TypeStemcellMagicMismatch,
		cpierrors.TypeStemcellNoCandidate,
		cpierrors.TypeStemcellEscapedRoot,
		cpierrors.TypeStemcellInvalidTar:
		typ = cpierrors.TypeCloud
	}
	return &jsonrpc.ErrorBody{
		Type:      string(typ),
		Message:   log.ScrubMessage(e.Error()),
		OkToRetry: e.OkToRetry(),
	}
}

// errorResponse builds a *jsonrpc.Response from a *cpierrors.Error for the
// dispatcher's own pre-handler failures (nil request, unknown method, result
// marshal, panic recovery). Those are all minted as CloudError, which
// wireErrorBody passes through untouched, so no method context is needed.
func errorResponse(e *cpierrors.Error) *jsonrpc.Response {
	return &jsonrpc.Response{
		Result: nil,
		Error:  wireErrorBody("", e),
		Log:    "",
	}
}

// Ensure HandlerFunc implements Handler at compile time.
var _ Handler = HandlerFunc(nil)
