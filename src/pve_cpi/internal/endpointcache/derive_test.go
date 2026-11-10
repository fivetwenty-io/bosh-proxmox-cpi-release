package endpointcache

import (
	"errors"
	"strings"
	"testing"
)

func TestDeriveAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed string
		node string
		want string
	}{
		{"fully qualified seed", "pve1.dc1.example.com", "pve2", "pve2.dc1.example.com:8006"},
		{"fully qualified seed keeps its port", "pve1.dc1.example.com:9006", "pve2", "pve2.dc1.example.com:9006"},
		{"two-label seed under a registrable domain", "pve1.example.com", "pve2", "pve2.example.com:8006"},
		{"domain two labels deep", "pve1.dc1.example.com", "pve9", "pve9.dc1.example.com:8006"},
		{"seed under a registrable ICANN second-level domain", "pve1.example.co.uk", "pve2", "pve2.example.co.uk:8006"},
		{"seed host is upper case", "PVE1.DC1.Example.COM", "pve2", "pve2.dc1.example.com:8006"},
		{"seed host carries a trailing dot", "pve1.dc1.example.com.", "pve2", "pve2.dc1.example.com:8006"},
		{"seed under a dynamic zone's customer label", "pve1.mylab.github.io", "pve2", "pve2.mylab.github.io:8006"},
		{"node keeps its own spelling", "pve1.example.com", "PVE-2", "PVE-2.example.com:8006"},
		{"bare seed", "pve1", "pve2", "pve2:8006"},
		{"bare seed keeps its port", "pve1:8443", "pve2", "pve2:8443"},
		{"IPv4 seed", "192.0.2.10", "pve2", "pve2:8006"},
		{"IPv4 seed keeps its port", "192.0.2.10:9006", "pve2", "pve2:9006"},
		{"bare IPv6 seed", "fd00::1", "pve2", "pve2:8006"},
		{"bracketed IPv6 seed keeps its port", "[fd00::1]:9006", "pve2", "pve2:9006"},
		{"node name of one character", "pve1.example.com", "n", "n.example.com:8006"},
	} {
		got, err := DeriveAddress(tc.seed, tc.node)
		if err != nil {
			t.Errorf("%s: DeriveAddress(%q, %q) error = %v", tc.name, tc.seed, tc.node, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: DeriveAddress(%q, %q) = %q, want %q", tc.name, tc.seed, tc.node, got, tc.want)
		}
	}
}

func TestDeriveAddressRefusesPublicSuffixDomains(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed string
	}{
		{"ICANN suffix itself", "example.co.uk"},
		{"ICANN suffix itself with a port", "example.co.uk:8006"},
		{"seed one label under com", "pve1.com"},
		{"seed one label under a two-label ICANN suffix", "pve1.co.uk"},
		{"private suffix dynamic zone", "pve1.github.io"},
		{"private suffix with a port", "pve1.github.io:9006"},
		{"another private suffix", "pve1.herokuapp.com"},
		{"single label domain lan", "pve1.lan"},
		{"single label domain internal", "pve1.internal"},
		{"single label domain in upper case", "PVE1.LAN"},
		{"unlisted top-level label", "pve1.notatld"},
		{"numeric single-label domain", "10.1"},
	} {
		got, err := DeriveAddress(tc.seed, "pve2")
		if !errors.Is(err, ErrPublicSuffix) {
			t.Errorf("%s: DeriveAddress(%q) = %q, %v; want ErrPublicSuffix", tc.name, tc.seed, got, err)
		}
		if got != "" {
			t.Errorf("%s: DeriveAddress(%q) returned address %q with a refusal", tc.name, tc.seed, got)
		}
	}
}

func TestDeriveAddressAcceptsDC1ExampleCom(t *testing.T) {
	t.Parallel()
	got, err := DeriveAddress("pve1.dc1.example.com", "pve2")
	if err != nil || got != "pve2.dc1.example.com:8006" {
		t.Fatalf("DeriveAddress = %q, %v", got, err)
	}
}

func TestDeriveAddressBareSeedsSkipThePublicSuffixCheck(t *testing.T) {
	t.Parallel()
	// A bare name has no domain to check, and an IP literal has none either, so
	// neither can take the public_suffix outcome.
	for _, seed := range []string{"lan", "com", "github", "10.0.0.1", "::1", "[fd00::1]:8006"} {
		if _, err := DeriveAddress(seed, "pve2"); err != nil {
			t.Errorf("DeriveAddress(%q) error = %v", seed, err)
		}
	}
}

func TestDeriveAddressRejectsBadNodeNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		node string
		want error
	}{
		{"", ErrNodeNameFormat},
		{"api.attacker.example", ErrNodeNameFormat},
		{"-pve2", ErrNodeNameFormat},
		{strings.Repeat("a", 64), ErrNodeNameFormat},
		{"localhost", ErrNodeNameAmbiguous},
		{"127", ErrNodeNameAmbiguous},
		{"0x7f000001", ErrNodeNameAmbiguous},
	} {
		for _, seed := range []string{"pve1.dc1.example.com", "pve1", "192.0.2.10"} {
			got, err := DeriveAddress(seed, tc.node)
			if !errors.Is(err, tc.want) || got != "" {
				t.Errorf("DeriveAddress(%q, %q) = %q, %v; want %v", seed, tc.node, got, err, tc.want)
			}
		}
	}
}

func TestDeriveAddressRejectsBadSeeds(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]string{
		"empty":                    "",
		"scheme":                   "https://pve1.example.com",
		"path":                     "pve1.example.com/api2",
		"user info":                "user@pve1.example.com",
		"whitespace":               " pve1.example.com",
		"port out of range":        "pve1.example.com:70000",
		"bracketed without a port": "[fd00::1]",
		"empty label":              "pve1..example.com",
		"leading dot":              ".example.com",
		"two trailing dots":        "pve1.example.com..",
	} {
		got, err := DeriveAddress(seed, "pve2")
		if !errors.Is(err, ErrSeedEndpoint) || got != "" {
			t.Errorf("%s: DeriveAddress(%q) = %q, %v; want ErrSeedEndpoint", name, seed, got, err)
		}
	}
}

func TestDeriveAddressRefusesANameOverTheDNSLimit(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("b", 63)
	seed := "pve1." + label + "." + label + "." + label + ".com"
	got, err := DeriveAddress(seed, strings.Repeat("a", 63))
	if !errors.Is(err, ErrSeedEndpoint) || got != "" {
		t.Fatalf("DeriveAddress = %q, %v; want ErrSeedEndpoint", got, err)
	}
}

func TestDeriverReusesTheSeedForManyNodes(t *testing.T) {
	t.Parallel()
	d, err := NewDeriver("pve1.dc1.example.com:9006")
	if err != nil {
		t.Fatalf("NewDeriver error = %v", err)
	}
	for _, node := range []string{"pve2", "pve3", "pve4"} {
		got, err := d.Address(node)
		want := node + ".dc1.example.com:9006"
		if err != nil || got != want {
			t.Errorf("Address(%q) = %q, %v; want %q", node, got, err, want)
		}
	}
	if _, err := d.Address("bad.name"); !errors.Is(err, ErrNodeNameFormat) {
		t.Errorf("Address(bad.name) error = %v, want ErrNodeNameFormat", err)
	}
}

func TestNewDeriverReportsThePublicSuffixOnce(t *testing.T) {
	t.Parallel()
	d, err := NewDeriver("pve1.lan")
	if !errors.Is(err, ErrPublicSuffix) || d != nil {
		t.Fatalf("NewDeriver = %v, %v; want ErrPublicSuffix and no deriver", d, err)
	}
}

func TestDeriveErrorsDoNotEchoTheSeed(t *testing.T) {
	t.Parallel()
	for _, seed := range []string{"https://user:hunter2@pve1.example.com", "pve1.lan"} {
		_, err := DeriveAddress(seed, "pve2")
		if err == nil {
			t.Fatalf("DeriveAddress(%q) succeeded", seed)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pve1") {
			t.Errorf("error %q echoes the seed", err)
		}
	}
}
