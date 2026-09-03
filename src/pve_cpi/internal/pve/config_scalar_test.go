package pve

import (
	"encoding/json"
	"testing"
)

func TestConfigString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		m       map[string]any
		key     string
		wantVal string
		wantOK  bool
	}{
		{
			name:    "string value",
			m:       map[string]any{"tags": "web,prod"},
			key:     "tags",
			wantVal: "web,prod",
			wantOK:  true,
		},
		{
			name:    "integral float64 renders without decimal point",
			m:       map[string]any{"name": float64(123)},
			key:     "name",
			wantVal: "123",
			wantOK:  true,
		},
		{
			name:    "non-integral float64 renders with fraction",
			m:       map[string]any{"description": 1.5},
			key:     "description",
			wantVal: "1.5",
			wantOK:  true,
		},
		{
			name:    "large float64 renders without exponent",
			m:       map[string]any{"size": 1e15},
			key:     "size",
			wantVal: "1000000000000000",
			wantOK:  true,
		},
		{
			name:    "bool true renders 1",
			m:       map[string]any{"template": true},
			key:     "template",
			wantVal: "1",
			wantOK:  true,
		},
		{
			name:    "bool false renders 0",
			m:       map[string]any{"template": false},
			key:     "template",
			wantVal: "0",
			wantOK:  true,
		},
		{
			name:    "nil value is absent",
			m:       map[string]any{"lock": nil},
			key:     "lock",
			wantVal: "",
			wantOK:  false,
		},
		{
			name:    "absent key",
			m:       map[string]any{"tags": "x"},
			key:     "missing",
			wantVal: "",
			wantOK:  false,
		},
		{
			name:    "map value is not a scalar",
			m:       map[string]any{"net0": map[string]any{"model": "virtio"}},
			key:     "net0",
			wantVal: "",
			wantOK:  false,
		},
		{
			name:    "slice value is not a scalar",
			m:       map[string]any{"tags": []any{"a", "b"}},
			key:     "tags",
			wantVal: "",
			wantOK:  false,
		},
		{
			name:    "json.Number value",
			m:       map[string]any{"vmid": json.Number("999")},
			key:     "vmid",
			wantVal: "999",
			wantOK:  true,
		},
		{
			name:    "int value (defensive)",
			m:       map[string]any{"cores": 4},
			key:     "cores",
			wantVal: "4",
			wantOK:  true,
		},
		{
			name:    "int64 value (defensive)",
			m:       map[string]any{"memory": int64(8192)},
			key:     "memory",
			wantVal: "8192",
			wantOK:  true,
		},
		{
			name:    "empty string value stays present",
			m:       map[string]any{"tags": ""},
			key:     "tags",
			wantVal: "",
			wantOK:  true,
		},
		{
			name:    "nil map",
			m:       nil,
			key:     "tags",
			wantVal: "",
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVal, gotOK := ConfigString(tc.m, tc.key)
			if gotOK != tc.wantOK {
				t.Fatalf("ConfigString(%v, %q) ok = %v, want %v", tc.m, tc.key, gotOK, tc.wantOK)
			}
			if gotVal != tc.wantVal {
				t.Fatalf("ConfigString(%v, %q) val = %q, want %q", tc.m, tc.key, gotVal, tc.wantVal)
			}
		})
	}
}

func TestConfigStringValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		v       any
		wantVal string
		wantOK  bool
	}{
		{name: "string", v: "hello", wantVal: "hello", wantOK: true},
		{name: "integral float64", v: float64(42), wantVal: "42", wantOK: true},
		{name: "non-integral float64", v: 3.14, wantVal: "3.14", wantOK: true},
		{name: "negative float64", v: float64(-7), wantVal: "-7", wantOK: true},
		{name: "zero float64", v: float64(0), wantVal: "0", wantOK: true},
		{name: "bool true", v: true, wantVal: "1", wantOK: true},
		{name: "bool false", v: false, wantVal: "0", wantOK: true},
		{name: "nil", v: nil, wantVal: "", wantOK: false},
		{name: "json.Number", v: json.Number("1234567890123456"), wantVal: "1234567890123456", wantOK: true},
		{name: "int", v: 7, wantVal: "7", wantOK: true},
		{name: "int64", v: int64(9223372036854775807), wantVal: "9223372036854775807", wantOK: true},
		{name: "map is not scalar", v: map[string]any{"a": 1}, wantVal: "", wantOK: false},
		{name: "slice is not scalar", v: []any{1, 2, 3}, wantVal: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVal, gotOK := ConfigStringValue(tc.v)
			if gotOK != tc.wantOK {
				t.Fatalf("ConfigStringValue(%v) ok = %v, want %v", tc.v, gotOK, tc.wantOK)
			}
			if gotVal != tc.wantVal {
				t.Fatalf("ConfigStringValue(%v) val = %q, want %q", tc.v, gotVal, tc.wantVal)
			}
		})
	}
}
