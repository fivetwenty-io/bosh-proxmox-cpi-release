package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// -----------------------------------------------------------------------
// Request types
// -----------------------------------------------------------------------

// Request is the decoded BOSH CPI JSON-RPC request envelope.
type Request struct {
	// Method is the CPI method name (e.g. "create_vm", "delete_vm").
	Method string `json:"method"`

	// Arguments holds the ordered positional arguments for Method.
	// Each element is a raw JSON value; callers unmarshal individual elements.
	Arguments []json.RawMessage `json:"arguments"`

	// Context carries director metadata and optional VM/stemcell API version hints.
	Context Context `json:"context"`

	// APIVersion is the CPI API version requested by the Director.
	// Absent (zero) for v1 calls.
	APIVersion int `json:"api_version,omitempty"`
}

// Context is the per-request BOSH Director context carried inside a Request.
type Context struct {
	// DirectorUUID is the unique ID of the BOSH Director issuing the request.
	DirectorUUID string `json:"director_uuid,omitempty"`

	// RequestID is the opaque request trace ID assigned by the Director.
	RequestID string `json:"request_id,omitempty"`

	// VM carries optional VM-level hints such as the stemcell API version.
	// Present only for methods that operate on VMs.
	VM map[string]any `json:"vm,omitempty"`

	// Stemcell carries optional stemcell-level hints.
	Stemcell map[string]any `json:"stemcell,omitempty"`

	// Extra holds any unrecognised context keys for forward compatibility —
	// notably the pve_* connection/routing overrides BOSH's cpi-config
	// feature merges into context.properties for a director entry that
	// targets a non-default PVE cluster (see config.ApplyContextOverrides
	// and handlers.Deps.WithRequestOverrides). Populated by Context's custom
	// UnmarshalJSON (below); tag "-" only prevents the standard encoding/json
	// machinery from separately trying to (un)marshal a field named "Extra"
	// literally — UnmarshalJSON fills it directly from the raw object.
	Extra map[string]any `json:"-"`
}

// knownContextFields lists the Context JSON keys decoded into typed fields
// above. UnmarshalJSON (below) routes every OTHER top-level key of the
// context object into Extra instead of silently discarding it.
var knownContextFields = map[string]struct{}{
	"director_uuid": {},
	"request_id":    {},
	"vm":            {},
	"stemcell":      {},
}

// UnmarshalJSON decodes a Context, populating the typed fields exactly as
// plain struct-tag decoding would, and additionally capturing every
// top-level JSON key NOT in knownContextFields into Extra as a generic
// decoded value (string, float64, bool, map[string]any, []any, or nil —
// standard encoding/json-into-any shapes).
//
// This exists because a Context previously silently discarded any context
// key it did not itself declare (e.g. the director_uuid/request_id/vm/
// stemcell above) — including the pve_* per-request routing properties
// BOSH's cpi-config feature merges into context.properties when a director
// runs multiple named CPI entries against distinct PVE clusters, all backed
// by this one CPI binary. Without Extra capture those keys never reach
// config.ApplyContextOverrides, and every dispatched request silently runs
// against whichever cluster this process happened to be launched with.
//
// Failure modes:
//   - data is not a JSON object (or is malformed JSON) → wrapped decode error.
//   - A known field's value has the wrong JSON type (e.g. "vm" is a string,
//     not an object) → wrapped decode error, matching plain struct decoding.
//   - An extra key's raw value itself is malformed JSON → that single key is
//     skipped from Extra rather than failing the whole Context decode; this
//     can only happen if the outer json.Unmarshal already partially decoded
//     the object into map[string]json.RawMessage, which by construction only
//     contains syntactically valid JSON fragments, so this branch is
//     defensive and not expected to be reachable in practice.
//
// data may be nil or the literal "null"; both decode to a zero Context.
func (c *Context) UnmarshalJSON(data []byte) error {
	// contextAlias has the same fields/tags as Context but not Context's own
	// UnmarshalJSON, so decoding into it uses plain reflection-based decoding
	// (avoiding infinite recursion) while still filling DirectorUUID/
	// RequestID/VM/Stemcell exactly as before this method existed.
	type contextAlias Context
	var a contextAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("jsonrpc: decode context: %w", err)
	}
	*c = Context(a)
	c.Extra = nil

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// data was valid JSON (the decode above succeeded) but not an object
		// (e.g. context: null decodes raw as a nil map with no error, but
		// context: "oops" would fail here); mirror the same error the typed
		// decode above would already have produced for a non-object context.
		return fmt.Errorf("jsonrpc: decode context: %w", err)
	}
	for key, val := range raw {
		if _, known := knownContextFields[key]; known {
			continue
		}
		var decoded any
		if err := json.Unmarshal(val, &decoded); err != nil {
			// Defensive only — see doc comment. Skip rather than fail the
			// whole Context decode over one unparsable extra key.
			continue
		}
		if c.Extra == nil {
			c.Extra = make(map[string]any, len(raw))
		}
		c.Extra[key] = decoded
	}
	return nil
}

// -----------------------------------------------------------------------
// Response types
// -----------------------------------------------------------------------

// Response is the BOSH CPI JSON-RPC response envelope.
//
// Exactly one of Result or Error is non-null per the BOSH spec.
type Response struct {
	// Result holds the method return value; null when Error is set.
	Result any `json:"result"`

	// Error holds the structured error; null when Result is set.
	Error *ErrorBody `json:"error"`

	// Log contains debug/audit output (stack traces, etc.).
	// Always present; empty string when no output.
	Log string `json:"log"`
}

// ErrorBody is the structured error payload inside a Response.
type ErrorBody struct {
	// Type is the canonical BOSH CPI error class name (e.g. "Bosh::Clouds::CloudError").
	Type string `json:"type"`

	// Message is a human-readable description of the error.
	Message string `json:"message"`

	// OkToRetry instructs the Director to retry the request with identical
	// arguments when true.
	OkToRetry bool `json:"ok_to_retry"`
}

// -----------------------------------------------------------------------
// EncodeSuccess
// -----------------------------------------------------------------------

// EncodeSuccess writes a success response envelope to w.
//
// The envelope has the form: {"result":<result>,"error":null,"log":"<log>"}
//
// result may be nil or any JSON-serialisable value; nil marshals to JSON null,
// which is correct for void CPI methods.
//
// Failure modes:
//   - w is nil → error returned; nothing written.
//   - result is not JSON-serialisable → wrapped json encode error.
//   - I/O error writing to w → wrapped error.
//
// Output is a single line of JSON terminated by a newline, as required by the
// BOSH Director's response parser.
func EncodeSuccess(w io.Writer, result any, log string) error {
	if w == nil {
		return errors.New("jsonrpc: EncodeSuccess called with nil writer")
	}

	resp := Response{
		Result: result,
		Error:  nil,
		Log:    log,
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("jsonrpc: encode success response: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------
// EncodeError
// -----------------------------------------------------------------------

// EncodeError writes an error response envelope to w.
//
// The envelope has the form:
//
//	{"result":null,"error":{"type":"<errType>","message":"<msg>","ok_to_retry":<okToRetry>},"log":"<log>"}
//
// errType must be a canonical BOSH CPI error type string (e.g.
// "Bosh::Clouds::CloudError"). An empty errType is replaced with
// "Bosh::Clouds::CloudError" so the Director always receives a valid envelope.
//
// Failure modes:
//   - w is nil → error returned; nothing written.
//   - I/O error writing to w → wrapped error.
//
// Output is a single line of JSON terminated by a newline.
func EncodeError(w io.Writer, errType, msg string, okToRetry bool, log string) error {
	if w == nil {
		return errors.New("jsonrpc: EncodeError called with nil writer")
	}

	if errType == "" {
		errType = "Bosh::Clouds::CloudError"
	}

	resp := Response{
		Result: nil,
		Error: &ErrorBody{
			Type:      errType,
			Message:   msg,
			OkToRetry: okToRetry,
		},
		Log: log,
	}

	enc := json.NewEncoder(w)
	if err := enc.Encode(resp); err != nil {
		return fmt.Errorf("jsonrpc: encode error response: %w", err)
	}
	return nil
}
