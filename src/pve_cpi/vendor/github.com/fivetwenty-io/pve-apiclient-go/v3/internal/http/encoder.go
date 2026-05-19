package http

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// encodeParams converts a map[string]any into url.Values using Proxmox-correct
// encoding rules:
//
//   - bool            → "1" / "0"
//   - []T (any slice) → repeated key entries, one per element (each element
//     recursively encoded via encodeSingleValue)
//   - map[string]any  → Proxmox comma-separated key=value string
//     (e.g. net0 → "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0")
//   - time.Time       → Unix seconds string (decimal, per Proxmox convention)
//   - pointer         → dereference; nil pointer → key omitted
//   - everything else → fmt.Sprintf("%v", val)
//
// The function is safe for all combinations; it never panics on unexpected types.
func encodeParams(params map[string]interface{}) url.Values {
	out := url.Values{}

	for key, val := range params {
		addEncodedParam(out, key, val)
	}

	return out
}

// addEncodedParam encodes a single key/value pair into dst, potentially adding
// multiple entries for slice values.
func addEncodedParam(dst url.Values, key string, val interface{}) {
	if val == nil {
		return
	}

	// Dereference pointer via reflection; omit nil pointers.
	refVal := reflect.ValueOf(val)
	for refVal.Kind() == reflect.Pointer {
		if refVal.IsNil() {
			return
		}

		refVal = refVal.Elem()
		val = refVal.Interface()
	}

	// Dispatch on concrete type first (fast path for common cases).
	switch typedVal := val.(type) {
	case bool:
		if typedVal {
			dst.Add(key, "1")
		} else {
			dst.Add(key, "0")
		}

		return

	case time.Time:
		dst.Add(key, strconv.FormatInt(typedVal.Unix(), 10))

		return

	case map[string]interface{}:
		dst.Add(key, encodeNestedMap(typedVal))

		return
	}

	// Slice handling via reflection (covers []string, []int, []any, etc.).
	if refVal.Kind() == reflect.Slice {
		for i := range refVal.Len() {
			elem := refVal.Index(i).Interface()
			dst.Add(key, encodeSingleValue(elem))
		}

		return
	}

	// Default: Sprintf.
	dst.Add(key, fmt.Sprintf("%v", val))
}

// encodeSingleValue converts a single non-slice, non-map value to a string.
// Used for slice element encoding.
func encodeSingleValue(val interface{}) string {
	if val == nil {
		return ""
	}

	switch typedVal := val.(type) {
	case bool:
		if typedVal {
			return "1"
		}

		return "0"

	case time.Time:
		return strconv.FormatInt(typedVal.Unix(), 10)

	case string:
		return typedVal
	}

	return fmt.Sprintf("%v", val)
}

// encodeNestedMap serialises map[string]any as Proxmox comma-separated
// key=value notation (e.g. "bridge=vmbr0,virtio=52:54:00:12:34:56").
// Values within the map are stringified with encodeSingleValue.
// If JSON marshalling of the whole map is requested by a higher-level caller,
// that caller should do so directly; this function is for inline Proxmox params.
func encodeNestedMap(nested map[string]interface{}) string {
	if len(nested) == 0 {
		return ""
	}

	// Collect sorted keys for deterministic output.
	keys := make([]string, 0, len(nested))
	for k := range nested {
		keys = append(keys, k)
	}

	// Sort keys lexicographically so output is stable across runs.
	sortStrings(keys)

	parts := make([]string, 0, len(nested))
	for _, k := range keys {
		parts = append(parts, k+"="+encodeSingleValue(nested[k]))
	}

	return strings.Join(parts, ",")
}

// sortStrings sorts a slice of strings in place using insertion sort.
// Used to avoid importing "sort" just for key ordering.
func sortStrings(strs []string) {
	for i := 1; i < len(strs); i++ {
		key := strs[i]
		idx := i - 1

		for idx >= 0 && strs[idx] > key {
			strs[idx+1] = strs[idx]
			idx--
		}

		strs[idx+1] = key
	}
}
