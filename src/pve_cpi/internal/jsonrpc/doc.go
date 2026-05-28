// Package jsonrpc encodes and decodes BOSH CPI JSON-RPC request/response envelopes.
//
// Wire format (one JSON object per process invocation, no framing):
//
//	Request:  {"method":"...","arguments":[...],"context":{...},"api_version":2}
//	Success:  {"result":<value>,"error":null,"log":"..."}
//	Error:    {"result":null,"error":{"type":"...","message":"...","ok_to_retry":<bool>},"log":"..."}
//
// Invariants enforced by this package:
//   - result is null when error is set; error is null when result is set.
//   - log is always present (empty string when no log output).
//   - Single-line JSON output (no indent); BOSH Director expects one JSON object per line.
package jsonrpc
