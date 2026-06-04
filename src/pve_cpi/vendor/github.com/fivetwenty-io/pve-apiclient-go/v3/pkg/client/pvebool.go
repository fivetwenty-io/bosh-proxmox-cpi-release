package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// PVEBool is a boolean that tolerates the several JSON encodings the Proxmox VE
// API uses for boolean values. The API is internally Perl and is inconsistent
// about how it renders booleans in responses: depending on the endpoint a
// boolean may arrive as a JSON boolean (true/false), a JSON number (1/0, and
// occasionally other non-zero integers), or a JSON string ("1"/"0", "true"/
// "false", "yes"/"no", or empty). A plain Go *bool fails to decode every form
// but the literal true/false, which makes typed get-by-id responses error on
// real payloads.
//
// PVEBool decodes all of those forms and marshals back out as a native JSON
// boolean, so downstream consumers (including JSON/YAML output) see a clean
// true/false.
type PVEBool bool

// Bool returns the underlying boolean value.
func (b PVEBool) Bool() bool { return bool(b) }

// MarshalJSON renders the value as a native JSON boolean (true/false).
func (b PVEBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

// UnmarshalJSON accepts the boolean, numeric, and string encodings PVE emits.
//
//   - boolean: true / false
//   - number:  0 -> false, any other number -> true
//   - string:  "", "0", "false", "no", "off" -> false (case-insensitive);
//     "1", "true", "yes", "on" -> true; other non-empty strings -> true
//   - null:    leaves the value unchanged (handled by encoding/json for
//     pointer fields, but tolerated here for completeness)
func (b *PVEBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}

	// JSON boolean.
	if bytes.Equal(data, []byte("true")) {
		*b = true
		return nil
	}
	if bytes.Equal(data, []byte("false")) {
		*b = false
		return nil
	}

	// JSON string: decode then interpret the inner token.
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("pve bool: decode string: %w", err)
		}
		*b = PVEBool(parseLooseBool(s))
		return nil
	}

	// JSON number: 0 is false, anything else is true.
	var n float64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("pve bool: unsupported JSON value %s: %w", string(data), err)
	}
	*b = PVEBool(n != 0)
	return nil
}

// parseLooseBool interprets a PVE boolean-ish string. Empty, "0", and the usual
// negative words are false; everything else is true. Numeric strings are honored
// (e.g. "2" -> true) so callers never see a decode error for a stray value.
func parseLooseBool(s string) bool {
	switch trimmed := trimASCIISpaceLower(s); trimmed {
	case "", "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	default:
		if n, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return n != 0
		}
		return true
	}
}

// trimASCIISpaceLower trims surrounding ASCII whitespace and lowercases ASCII
// letters without pulling in strings/unicode for this hot, tiny path.
func trimASCIISpaceLower(s string) string {
	start := 0
	end := len(s)
	for start < end && isASCIISpace(s[start]) {
		start++
	}
	for end > start && isASCIISpace(s[end-1]) {
		end--
	}
	out := make([]byte, end-start)
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i-start] = c
	}
	return string(out)
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
