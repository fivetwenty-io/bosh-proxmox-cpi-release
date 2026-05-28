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

	// Extra holds any unrecognised context keys for forward compatibility.
	// Fields stored here are not round-tripped through JSON (tag "-").
	Extra map[string]any `json:"-"`
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
// Decode
// -----------------------------------------------------------------------

// Decode reads exactly one JSON-RPC request from r.
//
// Failure modes:
//   - I/O error reading from r → wrapped error.
//   - Malformed JSON → wrapped json decode error.
//   - Missing or empty "method" field → fmt.Errorf with descriptive message.
//   - Empty input (io.EOF before any bytes) → wrapped error.
//
// Decode does not use DisallowUnknownFields so that forward-compatible context
// extensions added by future Director versions are silently ignored. Callers
// that need strict parsing should layer their own decoder on top.
func Decode(r io.Reader) (*Request, error) {
	if r == nil {
		return nil, errors.New("jsonrpc: Decode called with nil reader")
	}

	dec := json.NewDecoder(r)
	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("jsonrpc: decode request: %w", err)
	}

	if req.Method == "" {
		return nil, errors.New("jsonrpc: request missing required field \"method\"")
	}

	return &req, nil
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
