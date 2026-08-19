package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// floatBitSize is the bit size passed to strconv float routines: PVE numeric
// fields are 64-bit floats.
const floatBitSize = 64

// PVEFloat is a floating-point number that tolerates the JSON encodings the
// Proxmox VE API uses for numeric values. As with booleans, the Perl-based API
// is inconsistent about how it renders numbers in responses: a value documented
// as a number may arrive as a JSON number (1.5) or as a JSON string ("1.5", or
// "" for an absent value). The pressure-stall (PSI) metrics on container and VM
// status are a concrete example — they are documented as numbers but emitted as
// strings — which makes a plain *float64 fail to decode real payloads.
//
// PVEFloat decodes both forms and marshals back out as a native JSON number, so
// downstream consumers (including JSON/YAML output) see a clean number.
type PVEFloat float64

// Float returns the underlying float64 value.
func (f PVEFloat) Float() float64 { return float64(f) }

// MarshalJSON renders the value as a native JSON number. Non-finite values
// (which are not representable in JSON) marshal as 0.
func (f PVEFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("0"), nil
	}

	return strconv.AppendFloat(nil, v, 'g', -1, floatBitSize), nil
}

// UnmarshalJSON accepts the numeric and string encodings PVE emits.
//
//   - number: decoded directly
//   - string: parsed as a float; "" parses to 0; an unparseable string is 0
//   - null:   leaves the value unchanged
func (f *PVEFloat) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}

	// JSON string: decode then parse the inner token.
	if data[0] == '"' {
		var s string

		err := json.Unmarshal(data, &s)
		if err != nil {
			return fmt.Errorf("pve float: decode string: %w", err)
		}

		*f = PVEFloat(parseLooseFloat(s))

		return nil
	}

	// JSON number.
	var n float64

	err := json.Unmarshal(data, &n)
	if err != nil {
		return fmt.Errorf("pve float: unsupported JSON value %s: %w", string(data), err)
	}

	*f = PVEFloat(n)

	return nil
}

// parseLooseFloat interprets a PVE numeric-ish string. Empty and unparseable
// strings yield 0 so callers never see a decode error for a stray value.
func parseLooseFloat(s string) float64 {
	trimmed := trimASCIISpaceLower(s)
	if trimmed == "" {
		return 0
	}

	n, err := strconv.ParseFloat(trimmed, floatBitSize)
	if err == nil {
		return n
	}

	return 0
}
