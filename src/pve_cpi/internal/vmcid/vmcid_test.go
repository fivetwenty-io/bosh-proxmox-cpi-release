package vmcid

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	good := []struct {
		raw  string
		want VMCID
	}{
		{"1", VMCID{VMID: 1}},
		{"123", VMCID{VMID: 123}},
		{"999999999", VMCID{VMID: 999999999}},
		{"123-0a1b2c3d-4e5f6a7b", VMCID{VMID: 123, Cluster: "0a1b2c3d", Token: "4e5f6a7b"}},
		{"7-00000000-ffffffff", VMCID{VMID: 7, Cluster: "00000000", Token: "ffffffff"}},
	}
	for _, tc := range good {
		got, err := Parse(tc.raw)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}

	bad := []string{
		"", " ", "0", "-1", "+5", " 5", "5 ", "12a", "vm-12", "1e3", "١٢٣",
		"99999999999999999999999999",
		"123-", "123--", "123-0a1b2c3d", "123-0a1b2c3d-", "123--4e5f6a7b",
		"123-0A1B2C3D-4e5f6a7b", "123-0a1b2c3-4e5f6a7b", "123-0a1b2c3d-4e5f6a7b9",
		"123-0a1b2c3d-4e5f6a7b-1", "123-0a1b2cxx-4e5f6a7b", "0-0a1b2c3d-4e5f6a7b",
		"../123", "123/456", "123?x=1", "0123", "0101-0a1b2c3d-4e5f6a7b",
	}
	for _, raw := range bad {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", raw, got)
		}
	}
}

func TestPathIDIsAllDigits(t *testing.T) {
	for _, raw := range []string{"42", "42-0a1b2c3d-4e5f6a7b"} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if c.PathID() != "42" {
			t.Errorf("PathID(%q) = %q, want 42", raw, c.PathID())
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, raw := range []string{"42", "42-0a1b2c3d-4e5f6a7b"} {
		c, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if c.String() != raw {
			t.Errorf("String() = %q, want %q", c.String(), raw)
		}
		if c.Stamped() != strings.Contains(raw, "-") {
			t.Errorf("Stamped(%q) = %v", raw, c.Stamped())
		}
	}
}

func TestVMIDAndSameVM(t *testing.T) {
	if n, ok := VMID("42-0a1b2c3d-4e5f6a7b"); !ok || n != 42 {
		t.Errorf("VMID stamped = %d, %v", n, ok)
	}
	if _, ok := VMID("junk"); ok {
		t.Error("VMID(junk) ok")
	}
	if !SameVM("42", 42) || !SameVM("42-0a1b2c3d-4e5f6a7b", 42) {
		t.Error("SameVM missed a match")
	}
	if SameVM("43", 42) || SameVM("junk", 42) || SameVM("", 0) {
		t.Error("SameVM matched wrongly")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"1", "123-0a1b2c3d-4e5f6a7b", "", "-", "--", "9-a-b", "+1", "00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		c, err := Parse(raw)
		if err != nil {
			return
		}
		if c.VMID <= 0 {
			t.Fatalf("Parse(%q) gave VMID %d", raw, c.VMID)
		}
		for _, r := range c.PathID() {
			if r < '0' || r > '9' {
				t.Fatalf("PathID %q not all digits", c.PathID())
			}
		}
		again, err := Parse(c.String())
		if err != nil || again != c {
			t.Fatalf("round trip of %q failed: %+v %v", raw, again, err)
		}
	})
}
