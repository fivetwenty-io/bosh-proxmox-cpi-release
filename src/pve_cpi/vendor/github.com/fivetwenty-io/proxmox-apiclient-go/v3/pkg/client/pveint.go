package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// intBitSize is the bit size passed to strconv integer routines: PVE integer
// fields are 64-bit.
const intBitSize = 64

// decimalBase is the base passed to strconv integer routines.
const decimalBase = 10

// PVEInt is an integer that tolerates the JSON encodings the Proxmox VE API
// uses for integer values. As with booleans and floats, the Perl-based API is
// inconsistent about how it renders integers in responses: a value documented
// as an integer may arrive as a JSON number (5) or as a JSON string ("5", or
// "" for an absent value). The firewall rule position on
// /cluster/firewall/groups/{group}/{pos} is a concrete example — it is
// documented as an integer but emitted as a string — which makes a plain
// int64 fail to decode real payloads.
//
// PVEInt decodes both forms and marshals back out as a native JSON number, so
// downstream consumers (including JSON/YAML output) see a clean integer.
type PVEInt int64

// Int returns the underlying int64 value.
func (i PVEInt) Int() int64 { return int64(i) }

// MarshalJSON renders the value as a native JSON number.
func (i PVEInt) MarshalJSON() ([]byte, error) {
	return strconv.AppendInt(nil, int64(i), decimalBase), nil
}

// UnmarshalJSON accepts the numeric and string encodings PVE emits.
//
//   - integer: decoded directly
//   - number:  a fractional number is truncated toward zero
//   - string:  parsed as an integer; "" parses to 0; an unparseable string is 0
//   - null:    leaves the value unchanged
func (i *PVEInt) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}

	// JSON string: decode then parse the inner token.
	if data[0] == '"' {
		var s string

		err := json.Unmarshal(data, &s)
		if err != nil {
			return fmt.Errorf("pve int: decode string: %w", err)
		}

		*i = PVEInt(parseLooseInt(s))

		return nil
	}

	// JSON integer.
	var n int64

	err := json.Unmarshal(data, &n)
	if err == nil {
		*i = PVEInt(n)

		return nil
	}

	// JSON number with a fractional part: truncate toward zero.
	var f float64

	err = json.Unmarshal(data, &f)
	if err != nil {
		return fmt.Errorf("pve int: unsupported JSON value %s: %w", string(data), err)
	}

	*i = PVEInt(int64(f))

	return nil
}

// parseLooseInt interprets a PVE integer-ish string. Empty and unparseable
// strings yield 0 so callers never see a decode error for a stray value.
// Fractional strings truncate toward zero.
func parseLooseInt(s string) int64 {
	trimmed := trimASCIISpaceLower(s)
	if trimmed == "" {
		return 0
	}

	n, err := strconv.ParseInt(trimmed, decimalBase, intBitSize)
	if err == nil {
		return n
	}

	f, err := strconv.ParseFloat(trimmed, floatBitSize)
	if err == nil {
		return int64(f)
	}

	return 0
}
