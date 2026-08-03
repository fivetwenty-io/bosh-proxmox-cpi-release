package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// reservedContextKeys mirrors jsonrpc.knownContextFields (internal/jsonrpc/jsonrpc.go).
// Context.UnmarshalJSON routes every top-level context key NOT in this set into
// Context.Extra; a key from this set showing up in Extra would mean the typed
// field and the catch-all disagree on ownership, silently duplicating or
// dropping director_uuid/request_id/vm/stemcell handling downstream.
var reservedContextKeys = map[string]struct{}{
	"director_uuid": {},
	"request_id":    {},
	"vm":            {},
	"stemcell":      {},
}

// FuzzDecodeRequest feeds arbitrary byte slices to decodeRequest, the parse
// boundary for every JSON-RPC line the BOSH Director sends this CPI on stdin
// (readLoop calls it once per scanned line, see main.go:644). The function
// must never panic, and its two documented outcomes each carry their own
// contract:
//
//   - err == nil: req is non-nil, req.Method is non-empty (the only field
//     decodeRequest itself validates, main.go:701-703), req.Context.Extra
//     never contains a reserved context key (that would mean Context's
//     catch-all logic disagrees with its own typed-field routing), every
//     req.Arguments element is syntactically valid JSON, and req re-marshals
//     cleanly (proving no field holds a value encoding/json can decode but
//     not re-encode, e.g. invalid RawMessage bytes captured verbatim).
//
//   - err != nil: req is nil, and err.Error() is either exactly the
//     missing-method sentinel or carries the "jsonrpc: decode request: "
//     wrap prefix decodeRequest applies to every json.Decoder error
//     (main.go:698-699). readLoop's actual handling of that error (main.go:
//     644-651) is exercised too: wrap it the same way readLoop does into a
//     cpierrors.Cloud error and encode it with jsonrpc.EncodeError, then
//     decode the result back — it must always be a well-formed CloudError
//     envelope (Error set, Result null, Type/OkToRetry fixed) that the
//     Director's response parser can consume, regardless of what decErr's
//     message contains.
func FuzzDecodeRequest(f *testing.F) {
	// valid request: full realistic BOSH Director payload.
	f.Add([]byte(`{"method":"create_vm","arguments":["agent-id-1","stemcell-cid-1",{"instance_type":"standard"},{"network0":{"type":"dynamic"}},[],{"bosh":{"password":"secret"}}],"context":{"director_uuid":"550e8400-e29b-41d4-a716-446655440000","request_id":"cpi-abc123","vm":{"stemcell":{"api_version":2}}},"api_version":2}`))

	// valid request: minimal method-only object.
	f.Add([]byte(`{"method":"info"}`))

	// empty line: EOF before any byte.
	f.Add([]byte(``))

	// truncated JSON: syntactically incomplete object.
	f.Add([]byte(`{"method":"create_vm","arguments":[1,2`))

	// non-object JSON at top level.
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`"just a string"`))
	f.Add([]byte(`42`))
	f.Add([]byte(`null`))

	// wrong-type fields: method as number, arguments as object, context as string.
	f.Add([]byte(`{"method":123}`))
	f.Add([]byte(`{"method":"a","arguments":"not-an-array"}`))
	f.Add([]byte(`{"method":"a","context":"oops"}`))
	f.Add([]byte(`{"method":"a","context":123}`))

	// huge nested args: past encoding/json's internal max-depth guard
	// (~10000 levels) — decodeRequest must return a clean error, not
	// exhaust the goroutine stack.
	deepArgs := `{"method":"a","arguments":[` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `]}`
	f.Add([]byte(deepArgs))

	// duplicate keys: encoding/json applies last-value-wins; must not panic.
	f.Add([]byte(`{"method":"a","method":"b","method":"c"}`))

	// invalid UTF-8: both embedded in a JSON string value and as raw non-JSON bytes.
	f.Add([]byte("{\"method\":\"a\xff\xfeb\"}"))
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01, 0x7b, 0x7d})

	// null args: a present-but-null "arguments" field is a valid empty call
	// (encoding/json decodes JSON null into a nil []json.RawMessage with no
	// error), distinct from an absent field or a genuinely empty array.
	f.Add([]byte(`{"method":"a","arguments":null}`))

	// unknown method: decodeRequest validates only that Method is non-empty;
	// method-name legality is the dispatcher's concern, not this function's.
	f.Add([]byte(`{"method":"totally_bogus_method_xyz"}`))

	// oversized method name.
	f.Add([]byte(`{"method":"` + strings.Repeat("a", 1<<20) + `"}`))

	// context carrying both known and unrecognised keys, to exercise the
	// Extra catch-all directly.
	f.Add([]byte(`{"method":"a","context":{"director_uuid":"d","request_id":"r","vm":{"x":1},"stemcell":{"y":2},"pve_endpoint":"https://pve.example.com","pve_token":"secret=="}}`))

	// empty method after successful decode (explicit missing-field trigger).
	f.Add([]byte(`{"method":""}`))
	f.Add([]byte(`{"arguments":[]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := decodeRequest(data)

		if err == nil {
			assertDecodeSuccessInvariants(t, data, req)
			return
		}
		assertDecodeFailureInvariants(t, data, req, err)
	})
}

// assertDecodeSuccessInvariants checks every postcondition decodeRequest
// itself guarantees (or relies on jsonrpc.Context's UnmarshalJSON to
// guarantee) when it returns a nil error.
func assertDecodeSuccessInvariants(t *testing.T, data []byte, req *jsonrpc.Request) {
	t.Helper()

	if req == nil {
		t.Fatalf("decodeRequest(%q): nil req with nil err", data)
	}
	if req.Method == "" {
		t.Fatalf("decodeRequest(%q): empty Method with nil err (violates main.go:701-703 check)", data)
	}

	for i, arg := range req.Arguments {
		if !json.Valid(arg) {
			t.Fatalf("decodeRequest(%q): Arguments[%d] = %q is not valid JSON", data, i, arg)
		}
	}

	for key := range req.Context.Extra {
		if _, reserved := reservedContextKeys[key]; reserved {
			t.Fatalf("decodeRequest(%q): Context.Extra contains reserved key %q that should have decoded into its typed field instead", data, key)
		}
	}

	// The decoded request must itself be re-marshalable: every field
	// (including raw Arguments bytes and the Context.Extra map) that
	// encoding/json accepted on the way in must be representable on the
	// way back out.
	if _, marshalErr := json.Marshal(req); marshalErr != nil {
		t.Fatalf("decodeRequest(%q): successfully decoded request failed to re-marshal: %v", data, marshalErr)
	}
}

// assertDecodeFailureInvariants checks decodeRequest's own error-shape
// contract, then replicates readLoop's actual handling of a decode error
// (main.go:644-651) to confirm that handling always produces a well-formed
// CloudError JSON-RPC response envelope, regardless of what decErr says.
func assertDecodeFailureInvariants(t *testing.T, data []byte, req *jsonrpc.Request, err error) {
	t.Helper()

	if req != nil {
		t.Fatalf("decodeRequest(%q): non-nil req alongside non-nil err", data)
	}
	msg := err.Error()
	if msg == "" {
		t.Fatalf("decodeRequest(%q): empty error message", data)
	}

	const missingMethodMsg = `jsonrpc: request missing required field "method"`
	const decodeWrapPrefix = "jsonrpc: decode request: "
	if msg != missingMethodMsg && !strings.HasPrefix(msg, decodeWrapPrefix) {
		t.Fatalf("decodeRequest(%q): error %q matches neither the missing-method sentinel nor the decode-wrap prefix", data, msg)
	}

	// Replicate readLoop's handling of decErr (main.go:646-650) and confirm
	// the Director-facing envelope it produces is always well-formed.
	cpiErr := cpierrors.Cloud("request decode failed: %s", msg)
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Fatalf("decodeRequest(%q): wrapped CloudError has Type %q, want %q", data, cpiErr.Type(), cpierrors.TypeCloud)
	}
	if cpiErr.OkToRetry() {
		t.Fatalf("decodeRequest(%q): wrapped CloudError is retriable; a malformed request will never succeed on retry", data)
	}
	if !strings.Contains(cpiErr.Error(), msg) {
		t.Fatalf("decodeRequest(%q): wrapped CloudError message %q lost the original decode error %q", data, cpiErr.Error(), msg)
	}

	var buf bytes.Buffer
	if encErr := jsonrpc.EncodeError(&buf, string(cpiErr.Type()), cpiErr.Error(), cpiErr.OkToRetry(), ""); encErr != nil {
		t.Fatalf("decodeRequest(%q): EncodeError failed on the wrapped CloudError: %v", data, encErr)
	}

	var resp jsonrpc.Response
	if decErr := json.Unmarshal(buf.Bytes(), &resp); decErr != nil {
		t.Fatalf("decodeRequest(%q): EncodeError produced a response envelope the Director could not parse back: %v (envelope: %s)", data, decErr, buf.String())
	}
	if resp.Error == nil {
		t.Fatalf("decodeRequest(%q): encoded error envelope has nil Error field", data)
	}
	if resp.Result != nil {
		t.Fatalf("decodeRequest(%q): encoded error envelope has non-null Result %v alongside a non-nil Error, violating the exactly-one-of contract", data, resp.Result)
	}
	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Fatalf("decodeRequest(%q): encoded error envelope Type = %q, want %q", data, resp.Error.Type, cpierrors.TypeCloud)
	}
	if resp.Error.OkToRetry {
		t.Fatalf("decodeRequest(%q): encoded error envelope is marked retriable", data)
	}
}
