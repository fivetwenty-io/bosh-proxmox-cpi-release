package endpointcache

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNodeNameAccepts(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"pve1",
		"a",
		"A",
		"pve-1",
		"Node-A-b",
		"a1",
		"0xzz",
		"0x7f-1",
		"1a",
		"a-1",
		"localhost1",
		"mylocalhost",
		"local-host",
		strings.Repeat("a", 63),
		"a" + strings.Repeat("-", 61) + "a",
	} {
		if err := ValidateNodeName(name); err != nil {
			t.Errorf("ValidateNodeName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateNodeNameRejectsBadFormat(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"empty":             "",
		"dot":               "pve1.example.com",
		"leading dot":       ".pve1",
		"trailing dot":      "pve1.",
		"leading hyphen":    "-pve1",
		"trailing hyphen":   "pve1-",
		"only hyphen":       "-",
		"64 characters":     strings.Repeat("a", 64),
		"underscore":        "pve_1",
		"space":             "pve 1",
		"leading space":     " pve1",
		"trailing newline":  "pve1\n",
		"slash":             "pve1/..",
		"colon":             "pve1:8006",
		"at sign":           "user@pve1",
		"non-ASCII letter":  "pvé",
		"kelvin sign":       "K1",
		"fullwidth digit":   "pve１",
		"NUL":               "pve\x001",
		"bracket":           "[::1]",
		"percent":           "pve%31",
		"query":             "pve1?x",
		"long and dotted":   strings.Repeat("a", 70) + ".example.com",
		"all hyphens inner": "--",
	} {
		err := ValidateNodeName(input)
		if !errors.Is(err, ErrNodeNameFormat) {
			t.Errorf("%s: ValidateNodeName(%q) = %v, want ErrNodeNameFormat", name, input, err)
		}
		if errors.Is(err, ErrNodeNameAmbiguous) {
			t.Errorf("%s: ValidateNodeName(%q) matched both tiers", name, input)
		}
	}
}

func TestValidateNodeNameRefusesAmbiguousNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"1",
		"0",
		"127",
		"0177",
		"2130706433",
		strings.Repeat("9", 63),
		"0x7f000001",
		"0X7F000001",
		"0xdeadBEEF",
		"0x0",
		"0x",
		"0X",
		"localhost",
		"LOCALHOST",
		"LocalHost",
		"localHOST",
	} {
		err := ValidateNodeName(name)
		if !errors.Is(err, ErrNodeNameAmbiguous) {
			t.Errorf("ValidateNodeName(%q) = %v, want ErrNodeNameAmbiguous", name, err)
		}
		if errors.Is(err, ErrNodeNameFormat) {
			t.Errorf("ValidateNodeName(%q) matched both tiers", name)
		}
	}
}

func TestValidateNodeNameFormatWinsOverAmbiguity(t *testing.T) {
	t.Parallel()
	// A dotted numeric name fails the grammar first, so the caller rejects the
	// whole result instead of dropping one node.
	for _, name := range []string{"127.0.0.1", "localhost.", "0x7f.1", strings.Repeat("1", 64)} {
		if err := ValidateNodeName(name); !errors.Is(err, ErrNodeNameFormat) {
			t.Errorf("ValidateNodeName(%q) = %v, want ErrNodeNameFormat", name, err)
		}
	}
}

func TestValidateNodeNameErrorDoesNotEchoTheName(t *testing.T) {
	t.Parallel()
	err := ValidateNodeName("secret.token@evil.example")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "evil") {
		t.Errorf("error %q echoes the rejected name", err)
	}
}

func TestValidNodeName(t *testing.T) {
	t.Parallel()
	if !ValidNodeName("pve1") {
		t.Error("pve1 should be valid")
	}
	for _, name := range []string{"", "a.b", "localhost", "123", "0x1f", "-a"} {
		if ValidNodeName(name) {
			t.Errorf("ValidNodeName(%q) = true", name)
		}
	}
}

func TestNodeNameFormatMatchesStorageIdentityGrammar(t *testing.T) {
	t.Parallel()
	// The helper applies the grammar the storage identity code uses, plus the
	// 63-character cap. Names within the cap must agree on the format tier.
	for _, name := range []string{"a", "a-b", "-a", "a-", "a.b", "a_b", "1", "A9-z", ""} {
		got := !errors.Is(ValidateNodeName(name), ErrNodeNameFormat)
		want := referenceNodeFormat(name)
		if got != want {
			t.Errorf("format verdict for %q = %v, reference %v", name, got, want)
		}
	}
}

// referenceNodeFormat restates PVE's pve-node pattern independently of the
// implementation, so the grammar test does not compare the code with itself.
func referenceNodeFormat(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && (c != '-' || i == 0 || i == len(name)-1) {
			return false
		}
	}
	return true
}
