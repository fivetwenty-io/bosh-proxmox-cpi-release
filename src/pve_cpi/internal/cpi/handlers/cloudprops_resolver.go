package handlers

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// layeredResolver walks an ordered list of cloud_properties maps and returns
// the first matching value for a given key (or alias set). Layer order is:
//
//  1. Per-call cloud_properties (highest precedence)
//  2. disk_type profile CloudProperties (if selector present in call)
//  3. vm_type profile CloudProperties (lowest profile precedence)
//
// Callers apply global config defaults after the resolver returns not-found.
type layeredResolver struct {
	layers []map[string]any
}

// newLayeredResolver constructs a layeredResolver from the per-call cloud_properties
// map and CPI config. It reads callCP["vm_type"] and callCP["disk_type"] as string
// selectors. A non-empty selector that does not name a profile in cfg.VMTypes or
// cfg.DiskTypes returns a non-retriable CloudError (operator misconfiguration).
// A selector value present but not a string also returns a CloudError. nil callCP
// is treated as an empty map.
//
// Layer order appended: [callCP, diskType.CloudProperties, vmType.CloudProperties].
// Only profiles that resolved are appended (absent selector = layer omitted).
func newLayeredResolver(callCP map[string]any, cfg *config.CPIConfig) (*layeredResolver, error) {
	if callCP == nil {
		callCP = map[string]any{}
	}

	layers := make([]map[string]any, 0, 3)
	// Call layer is always first.
	layers = append(layers, callCP)

	// Resolve vm_type selector.
	vmTypeLayer, err := resolveProfileLayer(callCP, "vm_type", cfg.VMTypes)
	if err != nil {
		return nil, err
	}

	// Resolve disk_type selector.
	diskTypeLayer, err := resolveProfileLayer(callCP, "disk_type", cfg.DiskTypes)
	if err != nil {
		return nil, err
	}

	// Append in precedence order: disk_type before vm_type.
	if diskTypeLayer != nil {
		layers = append(layers, diskTypeLayer)
	}
	if vmTypeLayer != nil {
		layers = append(layers, vmTypeLayer)
	}

	return &layeredResolver{layers: layers}, nil
}

// resolveProfileLayer reads selectorKey from callCP, looks the selector up in
// profiles, and returns the matching profile's CloudProperties map (may be nil
// if the profile has no cloud_properties). Returns (nil, nil) when the selector
// is absent from callCP. Returns a CloudError when:
//   - the selector value is present but not a string
//   - the selector names a profile absent from profiles
func resolveProfileLayer(callCP map[string]any, selectorKey string, profiles map[string]config.TypeProfile) (map[string]any, error) {
	raw, present := callCP[selectorKey]
	if !present {
		return nil, nil
	}
	name, ok := raw.(string)
	if !ok {
		return nil, cpierrors.Cloud(
			"cloud_properties.%s must be a string selector, got %T (%v)",
			selectorKey, raw, raw,
		)
	}
	if name == "" {
		// Empty string selector: no profile selected, skip layer.
		return nil, nil
	}
	profile, found := profiles[name]
	if !found {
		return nil, cpierrors.Cloud(
			"cloud_properties.%s %q: unknown profile (not declared in cpi config %s map)",
			selectorKey, name, selectorKey+"s",
		)
	}
	// Profile with nil CloudProperties is valid; return an empty map so callers
	// can still tell the profile resolved without crashing on nil lookup.
	if profile.CloudProperties == nil {
		return map[string]any{}, nil
	}
	return profile.CloudProperties, nil
}

// String walks layers in order. Within each layer it tries each key left-to-right.
// The first layer that contains any of the keys with a non-empty, non-whitespace
// string value returns (trimmedValue, true). Non-string values in a layer are
// skipped (key treated as absent for this method). Whitespace-only strings are
// skipped. Returns ("", false) when no layer yields a result.
func (r *layeredResolver) String(keys ...string) (string, bool) {
	for _, layer := range r.layers {
		for _, key := range keys {
			v, present := layer[key]
			if !present {
				continue
			}
			s, isStr := v.(string)
			if !isStr {
				// Non-string: skip this key in this layer; try next key.
				continue
			}
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				// Whitespace-only: treat as absent; try next key.
				continue
			}
			return trimmed, true
		}
		// No key in this layer returned a value; fall through to next layer.
	}
	return "", false
}

// Int walks layers in order, trying each key within each layer. Returns the first
// parseable integer value. Accepted types: int, int64, float64 (JSON default number
// type), json.Number, and numeric string (via strconv.Atoi). Returns (0, false) when
// no layer yields a parseable integer. Returns (0, true) when an explicit 0 is found.
func (r *layeredResolver) Int(keys ...string) (int, bool) {
	for _, layer := range r.layers {
		for _, key := range keys {
			v, present := layer[key]
			if !present {
				continue
			}
			n, ok := coerceInt(v)
			if !ok {
				// Value present but not parseable as int: skip to next key.
				continue
			}
			return n, true
		}
	}
	return 0, false
}

// coerceInt converts v to an int if possible. Accepts: int, int64, float64,
// json.Number, and strings parseable by strconv.Atoi. Returns (0, false) for
// all other types or unparseable strings.
func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := strconv.Atoi(n.String())
		if err != nil {
			// Try via float in case the number has a decimal part (e.g. "2.0").
			f, ferr := strconv.ParseFloat(n.String(), 64)
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return i, true
	case string:
		trimmed := strings.TrimSpace(n)
		i, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// Float walks layers in order, trying each key within each layer. Returns the first
// parseable float64 value. Accepted types: float64 (JSON default number type), int,
// int64, json.Number, and numeric strings (via strconv.ParseFloat). Returns (0, false)
// when no layer yields a parseable float.
func (r *layeredResolver) Float(keys ...string) (float64, bool) {
	for _, layer := range r.layers {
		for _, key := range keys {
			v, present := layer[key]
			if !present {
				continue
			}
			f, ok := coerceFloat(v)
			if !ok {
				// Value present but not parseable as float: skip to next key.
				continue
			}
			return f, true
		}
	}
	return 0, false
}

// coerceFloat converts v to a float64 if possible. Accepts: float64, int, int64,
// json.Number, and strings parseable by strconv.ParseFloat. Returns (0, false) for
// all other types or unparseable strings.
func coerceFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		trimmed := strings.TrimSpace(n)
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// Bool walks layers in order, trying each key within each layer. Returns the first
// parseable boolean value. Accepted types:
//   - bool: returned as-is; explicit false returns (false, true)
//   - float64 / int: 1 → true, 0 → false; other values → skip
//   - string (trimmed, case-insensitive): "true"/"1" → true, "false"/"0" → false
//
// Returns (false, false) when no layer yields a parseable bool.
func (r *layeredResolver) Bool(keys ...string) (bool, bool) {
	for _, layer := range r.layers {
		for _, key := range keys {
			v, present := layer[key]
			if !present {
				continue
			}
			b, ok := coerceBool(v)
			if !ok {
				// Value present but not parseable as bool: skip to next key.
				continue
			}
			return b, true
		}
	}
	return false, false
}

// coerceBool converts v to a bool if possible. Returns (false, false) for
// unrecognized types or unparseable strings.
func coerceBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case float64:
		switch b {
		case 1:
			return true, true
		case 0:
			return false, true
		default:
			return false, false
		}
	case int:
		switch b {
		case 1:
			return true, true
		case 0:
			return false, true
		default:
			return false, false
		}
	case int64:
		switch b {
		case 1:
			return true, true
		case 0:
			return false, true
		default:
			return false, false
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1":
			return true, true
		case "false", "0":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

// StringSlice walks layers in order, trying each key within each layer. Returns the
// first layer/key combination that yields a non-empty filtered string slice. Accepted
// source types:
//   - []string: used directly; empty elements skipped
//   - []any: elements that are non-empty strings (after TrimSpace) are collected; others skipped
//   - string: a non-empty (after TrimSpace) single string becomes a 1-element slice
//
// Empty result after filtering (all elements whitespace/empty or wrong type) is
// treated as not found; the next key/layer is tried. Returns (nil, false) when no
// layer/key yields a result.
func (r *layeredResolver) StringSlice(keys ...string) ([]string, bool) {
	for _, layer := range r.layers {
		for _, key := range keys {
			v, present := layer[key]
			if !present {
				continue
			}
			ss, ok := coerceStringSlice(v)
			if !ok || len(ss) == 0 {
				continue
			}
			return ss, true
		}
	}
	return nil, false
}

// coerceStringSlice converts v to a []string. Returns (nil, false) for unrecognized
// types or empty results after filtering.
func coerceStringSlice(v any) ([]string, bool) {
	switch s := v.(type) {
	case []string:
		out := make([]string, 0, len(s))
		for _, elem := range s {
			if t := strings.TrimSpace(elem); t != "" {
				out = append(out, t)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true

	case []any:
		out := make([]string, 0, len(s))
		for _, elem := range s {
			str, isStr := elem.(string)
			if !isStr {
				continue
			}
			if t := strings.TrimSpace(str); t != "" {
				out = append(out, t)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true

	case string:
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return nil, false
		}
		return []string{trimmed}, true

	default:
		return nil, false
	}
}
