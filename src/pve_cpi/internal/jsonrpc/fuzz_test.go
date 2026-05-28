package jsonrpc

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzDecode feeds arbitrary byte slices to Decode and asserts no panic occurs.
//
// Decode is the first function called on every request from the BOSH Director,
// making it the highest-exposure parse surface in this binary. The invariant is
// simple: Decode must never panic regardless of input shape. Errors are allowed
// and expected on garbage or incomplete inputs.
//
// Seed corpus covers:
//   - Real happy-path request (from testdata/request.json)
//   - Minimal valid method-only object
//   - Empty bytes (EOF before first byte)
//   - Empty JSON object {}
//   - JSON null
//   - JSON array [] (wrong top-level type)
//   - Deeply nested array [[[[[[1]]]]]] (stack pressure)
//   - Truncated object (syntactically incomplete)
//   - Over-deep nesting (~1000 levels, stack-depth pressure)
//   - Very long string value (1 MiB of 'a', allocation pressure)
//   - Lone surrogate unicode escape \uD800 (encoding edge case)
//   - Object with method key present but all other fields absent
//   - Duplicate keys (last-wins per Go JSON decoder; must not panic)
//   - Binary garbage (non-UTF-8 bytes)
func FuzzDecode(f *testing.F) {
	// --- seed: real happy-path request ---
	f.Add([]byte(`{"method":"create_vm","arguments":["agent-id-1","stemcell-cid-1",{"instance_type":"standard"},{"network0":{"type":"dynamic"}},[],{"bosh":{"password":"secret"}}],"context":{"director_uuid":"550e8400-e29b-41d4-a716-446655440000","request_id":"cpi-abc123","vm":{"stemcell":{"api_version":2}}},"api_version":2}`))

	// --- seed: minimal valid ---
	f.Add([]byte(`{"method":"info"}`))

	// --- seed: empty bytes (EOF) ---
	f.Add([]byte{})

	// --- seed: empty JSON object ---
	f.Add([]byte(`{}`))

	// --- seed: JSON null ---
	f.Add([]byte(`null`))

	// --- seed: JSON array (wrong top-level type) ---
	f.Add([]byte(`[]`))

	// --- seed: large nested array (stack pressure) ---
	f.Add([]byte(`[[[[[[1]]]]]]`))

	// --- seed: truncated object (syntactically incomplete) ---
	f.Add([]byte(`{"method":"info"`))

	// --- seed: over-deep nesting (~1000 levels) ---
	f.Add([]byte(strings.Repeat("[", 1000) + "1" + strings.Repeat("]", 1000)))

	// --- seed: very long string value (1 MiB of 'a', allocation pressure) ---
	longVal := `{"method":"` + strings.Repeat("a", 1<<20) + `"}`
	f.Add([]byte(longVal))

	// --- seed: lone surrogate unicode escape ---
	f.Add([]byte(`{"method":"\uD800"}`))

	// --- seed: method key present, all others absent ---
	f.Add([]byte(`{"method":"delete_vm"}`))

	// --- seed: duplicate keys (last-wins per encoding/json) ---
	f.Add([]byte(`{"method":"a","method":"b"}`))

	// --- seed: binary garbage (non-UTF-8 bytes) ---
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01, 0x7b, 0x7d})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Invariant: Decode must never panic on any input.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %q: %v", data, r)
			}
		}()

		// Decode either returns an error (acceptable for arbitrary input)
		// or returns a non-nil *Request with a non-empty Method field.
		// Both outcomes are valid; only a panic is a failure.
		req, err := Decode(bytes.NewReader(data))
		if err != nil {
			// Error is acceptable for arbitrary/malformed input.
			return
		}
		// If no error, Decode's own postcondition holds: Method must be non-empty.
		// This mirrors the check inside Decode itself; violation here would
		// indicate a bug in that postcondition guard.
		if req == nil {
			t.Errorf("Decode returned nil req with nil err for input %q", data)
			return
		}
		if req.Method == "" {
			t.Errorf("Decode returned empty Method with nil err for input %q", data)
		}
	})
}
