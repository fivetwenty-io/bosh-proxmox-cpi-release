package http

import (
	"encoding/json"
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
//   - json.Number     → its exact source text (no float rounding/exponent)
//   - float64/float32 → decimal notation, never scientific (e.g. 1048576 not 1.048576e+06)
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

	val, refVal, ok := derefNonNil(val)
	if !ok {
		return // nil pointer: key omitted
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

	case json.Number, float64, float32:
		// Decimal digits only: PVE rejects scientific notation, so 1048576
		// must stay "1048576", never "1.048576e+06" (see encodeSingleValue).
		dst.Add(key, encodeSingleValue(val))

		return

	case time.Time:
		dst.Add(key, strconv.FormatInt(typedVal.Unix(), 10))

		return

	case map[string]interface{}:
		dst.Add(key, encodeNestedMap(typedVal))

		return

	case OptionString:
		dst.Add(key, (&typedVal).Encode())

		return

	case *OptionString:
		dst.Add(key, typedVal.Encode())

		return

	case IndexedSlice:
		typedVal.addTo(dst, key)

		return
	}

	// Slice handling via reflection (covers []string, []int, []any, etc.).
	if refVal.Kind() == reflect.Slice {
		addSliceParam(dst, key, refVal)

		return
	}

	// Default: Sprintf.
	dst.Add(key, fmt.Sprintf("%v", val))
}

// addSliceParam adds one repeated entry per slice element, each stringified via
// encodeSingleValue (covers []string, []int, []any, etc.).
func addSliceParam(dst url.Values, key string, refVal reflect.Value) {
	for i := range refVal.Len() {
		dst.Add(key, encodeSingleValue(refVal.Index(i).Interface()))
	}
}

// derefNonNil unwraps pointer chains to the underlying value. The bool is false
// when a nil pointer is encountered (the caller should omit the key); otherwise
// it returns the dereferenced value and its reflect.Value.
func derefNonNil(val interface{}) (interface{}, reflect.Value, bool) {
	refVal := reflect.ValueOf(val)
	for refVal.Kind() == reflect.Pointer {
		if refVal.IsNil() {
			return nil, refVal, false
		}

		refVal = refVal.Elem()
		val = refVal.Interface()
	}

	return val, refVal, true
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

	case json.Number:
		return typedVal.String()

	case float64:
		return strconv.FormatFloat(typedVal, 'f', -1, 64)

	case float32:
		return strconv.FormatFloat(float64(typedVal), 'f', -1, 32)

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

// KV is a single key/value entry within an ordered Proxmox option-string.
//
// A KV with an empty Key is a bare positional token (the leading value of a
// Proxmox option-string, such as the storage:size in a disk spec
// "local-lvm:32,size=64G" or the model in a NIC spec "virtio,bridge=vmbr0").
// A KV with a non-empty Key is emitted as "Key=Value".
type KV struct {
	// Key is the option name. Empty means this entry is a bare positional token.
	Key string

	// Value is the option value. It is stringified with the same rules as
	// encodeSingleValue (bool → "1"/"0", time.Time → Unix seconds, etc.).
	Value interface{}
}

// OptionString is an ordered Proxmox option-string. Unlike map[string]interface{},
// which sorts keys lexicographically and can only emit "key=value" pairs,
// OptionString preserves insertion order and supports bare positional leading
// tokens. This is required for PVE specs whose first element is positional, for
// example a disk ("local-lvm:32,size=64G,ssd=1") or a NIC
// ("virtio,bridge=vmbr0,firewall=1").
//
// Use OptionString (not map[string]interface{}) whenever element order matters
// or a positional leading token is present. The map[string]interface{} path is
// retained for order-insensitive option-strings only.
//
// Construct one directly or via OptionStringOf:
//
//	NewOptionString().Positional("local-lvm:32").Set("size", "64G").Set("ssd", true)
//	  → "local-lvm:32,size=64G,ssd=1"
type OptionString struct {
	entries []KV
}

// NewOptionString returns an empty OptionString ready for chained building.
func NewOptionString() *OptionString {
	return &OptionString{}
}

// OptionStringOf builds an OptionString from an ordered slice of KV entries.
// Order is preserved exactly as given.
func OptionStringOf(entries ...KV) *OptionString {
	out := &OptionString{entries: make([]KV, 0, len(entries))}
	out.entries = append(out.entries, entries...)

	return out
}

// Positional appends a bare positional token (no key). Returns the receiver so
// calls can be chained. Typically used once for the leading token, but PVE
// permits multiple positional tokens, so this method does not enforce a limit.
func (o *OptionString) Positional(value interface{}) *OptionString {
	o.entries = append(o.entries, KV{Key: "", Value: value})

	return o
}

// Set appends a "key=value" entry, preserving insertion order. Returns the
// receiver so calls can be chained. An empty key is treated as a positional
// token to avoid emitting a malformed leading "=value".
func (o *OptionString) Set(key string, value interface{}) *OptionString {
	o.entries = append(o.entries, KV{Key: key, Value: value})

	return o
}

// Len reports the number of entries in the option-string.
func (o *OptionString) Len() int {
	return len(o.entries)
}

// Encode serialises the option-string in insertion order using Proxmox rules:
// bare positional tokens emit their value alone, keyed entries emit "key=value",
// and all values are stringified via encodeSingleValue (bool → "1"/"0", etc.).
// Entries are joined with commas. An empty OptionString encodes to "".
func (o *OptionString) Encode() string {
	if len(o.entries) == 0 {
		return ""
	}

	parts := make([]string, 0, len(o.entries))

	for _, entry := range o.entries {
		valStr := encodeSingleValue(entry.Value)
		if entry.Key == "" {
			parts = append(parts, valStr)

			continue
		}

		parts = append(parts, entry.Key+"="+valStr)
	}

	return strings.Join(parts, ",")
}

// String implements fmt.Stringer, returning the encoded option-string.
func (o *OptionString) String() string {
	return o.Encode()
}

// ArrayMode selects how a slice value is encoded into url.Values.
type ArrayMode int

const (
	// ArrayRepeated emits one repeated entry per element (key=a&key=b&key=c).
	// This is the default behaviour for plain slices passed to encodeParams.
	ArrayRepeated ArrayMode = iota

	// ArrayIndexed emits one indexed entry per element (key0=a&key1=b&key2=c),
	// matching the convention used by many Proxmox endpoints (e.g. ipset, acl,
	// and bulk-style list parameters).
	ArrayIndexed
)

// IndexedSlice wraps a slice of values together with an explicit ArrayMode so a
// caller can choose indexed-key encoding (key0,key1,...) instead of the default
// repeated-key encoding (key=a&key=b). Pass an IndexedSlice as a parameter value
// to select the mode explicitly; a plain slice continues to use ArrayRepeated.
//
//	IndexedSliceOf(ArrayIndexed, "a", "b") under key "ip" → ip0=a, ip1=b
//	IndexedSliceOf(ArrayRepeated, "a", "b") under key "ip" → ip=a, ip=b
type IndexedSlice struct {
	mode   ArrayMode
	values []interface{}
}

// IndexedSliceOf builds an IndexedSlice from the given mode and elements.
func IndexedSliceOf(mode ArrayMode, values ...interface{}) IndexedSlice {
	out := IndexedSlice{mode: mode, values: make([]interface{}, 0, len(values))}
	out.values = append(out.values, values...)

	return out
}

// Mode reports the configured ArrayMode.
func (s IndexedSlice) Mode() ArrayMode {
	return s.mode
}

// Len reports the number of elements.
func (s IndexedSlice) Len() int {
	return len(s.values)
}

// addTo writes the slice into dst under key using the configured mode. Each
// element is stringified via encodeSingleValue. An empty slice adds nothing.
func (s IndexedSlice) addTo(dst url.Values, key string) {
	for i, v := range s.values {
		valStr := encodeSingleValue(v)
		if s.mode == ArrayIndexed {
			dst.Add(key+strconv.Itoa(i), valStr)

			continue
		}

		dst.Add(key, valStr)
	}
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
