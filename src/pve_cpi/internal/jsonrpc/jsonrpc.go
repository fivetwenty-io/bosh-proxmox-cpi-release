package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
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

	// ClashingKeys names the Director-owned context keys a cpi_config
	// property overwrote with a value the CPI cannot use: "vm" or "stemcell"
	// when the value is not an object, and "vm.<key>" for each key of a vm
	// object other than "stemcell". The CPI ignores those values. The
	// handlers log the names so the clash is visible. Populated by
	// UnmarshalJSON.
	ClashingKeys []string `json:"-"`
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

// decodeDirectorMap decodes the value of a Director-owned object key. A value
// that is not an object is ignored and recorded in c.ClashingKeys, because it
// can only have come from a cpi_config property of the same name.
func (c *Context) decodeDirectorMap(name string, raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		c.ClashingKeys = append(c.ClashingKeys, name)
		return nil
	}
	return m
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
//   - A scalar field's value has the wrong JSON type (e.g. "request_id" is a
//     number) → wrapped decode error, matching plain struct decoding.
//   - "vm" or "stemcell" is not an object, or "vm" holds a key other than
//     "stemcell" → the value is ignored and its name recorded in
//     ClashingKeys, since only a cpi_config property of that name can cause
//     it and failing the request would punish a harmless entry.
//   - An extra key's raw value itself is malformed JSON → that single key is
//     skipped from Extra rather than failing the whole Context decode; this
//     can only happen if the outer json.Unmarshal already partially decoded
//     the object into map[string]json.RawMessage, which by construction only
//     contains syntactically valid JSON fragments, so this branch is
//     defensive and not expected to be reachable in practice.
//
// data may be nil or the literal "null"; both decode to a zero Context.
func (c *Context) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// data is not a JSON object (for example context: "oops"), or is
		// malformed JSON.
		return fmt.Errorf("jsonrpc: decode context: %w", err)
	}
	// The scalar fields decode through an alias holding only them. The vm and
	// stemcell fields decode separately below, because a cpi_config property
	// named vm or stemcell replaces the Director's own key with whatever the
	// operator wrote.
	var scalars struct {
		DirectorUUID string `json:"director_uuid,omitempty"`
		RequestID    string `json:"request_id,omitempty"`
	}
	if err := json.Unmarshal(data, &scalars); err != nil {
		return fmt.Errorf("jsonrpc: decode context: %w", err)
	}
	*c = Context{DirectorUUID: scalars.DirectorUUID, RequestID: scalars.RequestID}
	c.VM = c.decodeDirectorMap("vm", raw["vm"])
	c.Stemcell = c.decodeDirectorMap("stemcell", raw["stemcell"])
	for key := range c.VM {
		if key != "stemcell" {
			// Only vm.stemcell is a Director field. Another key came from a
			// property named vm, and nothing reads it.
			c.ClashingKeys = append(c.ClashingKeys, "vm."+key)
			delete(c.VM, key)
		}
	}
	sort.Strings(c.ClashingKeys)

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
