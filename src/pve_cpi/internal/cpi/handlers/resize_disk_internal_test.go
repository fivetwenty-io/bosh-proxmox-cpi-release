package handlers

import (
	"strings"
	"testing"
)

// TestParseDiskSizeGiB_Units exercises the unit-handling branches of
// parseDiskSizeGiB across K/M/G/T/P (case-insensitive), unit-less bytes,
// and an explicit rejection of unknown trailing units.
func TestParseDiskSizeGiB_Units(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		optStr  string
		want    int
		wantErr bool
	}{
		// 1024 KiB = 1 MiB; ceil-to-GiB = 1.
		{"1024K", "vol,size=1024K", 1, false},
		// Lowercase suffix accepted.
		{"1024k_lower", "vol,size=1024k", 1, false},
		// 1024 MiB = 1 GiB exact.
		{"1024M", "vol,size=1024M", 1, false},
		// 1 GiB exact.
		{"1G", "vol,size=1G", 1, false},
		// 1 TiB = 1024 GiB.
		{"1T", "vol,size=1T", 1024, false},
		// 1 PiB = 1024 TiB = 1048576 GiB.
		{"1P", "vol,size=1P", 1024 * 1024, false},
		// Unit-less digits → bytes; 1 GiB exactly.
		{"unitless_bytes", "vol,size=1073741824", 1, false},
		// Sub-GiB byte count rounds up to 1 GiB (ceiling).
		{"small_bytes_ceil", "vol,size=1", 1, false},
		// Unknown unit suffix → error.
		{"100xyz", "vol,size=100xyz", 0, true},
		// Empty value → error.
		{"empty_value", "vol,size=", 0, true},
		// Missing numeric part (lone suffix) → error.
		{"lone_unit", "vol,size=G", 0, true},
		// No size= segment at all → error.
		{"no_size_segment", "vol,cache=writeback", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDiskSizeGiB(tc.optStr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (value=%d)", tc.optStr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.optStr, err)
			}
			if got != tc.want {
				t.Errorf("parseDiskSizeGiB(%q) = %d, want %d", tc.optStr, got, tc.want)
			}
		})
	}
}

// TestParseDiskSizeGiB_NegativeRejected confirms a negative numeric value is
// surfaced as an error rather than silently truncated. PVE does not emit
// negative sizes; this is a defense-in-depth guard against config corruption.
func TestParseDiskSizeGiB_NegativeRejected(t *testing.T) {
	t.Parallel()
	_, err := parseDiskSizeGiB("vol,size=-1G")
	if err == nil {
		t.Fatal("expected error for negative size, got nil")
	}
	if !strings.Contains(err.Error(), "non-negative") && !strings.Contains(err.Error(), "parse") {
		t.Errorf("error string should mention non-negative or parse failure; got %v", err)
	}
}
