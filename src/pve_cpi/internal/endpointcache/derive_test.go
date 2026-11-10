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
		{"fully qualified seed", "pve1.dc1.example.com:8006", "pve2", "pve2.dc1.example.com:8006"},
		{"fully qualified seed keeps its port", "pve1.dc1.example.com:9006", "pve2", "pve2.dc1.example.com:9006"},
		{"seed port written with a leading zero", "pve1.dc1.example.com:09006", "pve2", "pve2.dc1.example.com:9006"},
		{"two-label seed under a registrable domain", "pve1.example.com:8006", "pve2", "pve2.example.com:8006"},
		{"domain two labels deep", "pve1.dc1.example.com:8006", "pve9", "pve9.dc1.example.com:8006"},
		{"seed under a registrable ICANN second-level domain", "pve1.example.co.uk:8006", "pve2", "pve2.example.co.uk:8006"},
		{"seed host is upper case", "PVE1.DC1.Example.COM:8006", "pve2", "pve2.dc1.example.com:8006"},
		{"seed host carries a trailing dot", "pve1.dc1.example.com.:8006", "pve2", "pve2.dc1.example.com:8006"},
		{"seed under a dynamic zone's customer label", "pve1.mylab.github.io:8006", "pve2", "pve2.mylab.github.io:8006"},
		{"seed under a wildcard rule's exception", "pve1.city.kawasaki.jp:8006", "pve2", "pve2.city.kawasaki.jp:8006"},
		{"seed under a registrable domain beside a private suffix", "pve1.platform.sh:8006", "pve2", "pve2.platform.sh:8006"},
		{"seed under a wildcard rule on its registrable label", "pve1.www.ck:8006", "pve2", "pve2.www.ck:8006"},
		{"node keeps its own spelling", "pve1.example.com:8006", "PVE-2", "PVE-2.example.com:8006"},
		{"bare seed", "pve1:8006", "pve2", "pve2:8006"},
		{"bare seed keeps its port", "pve1:8443", "pve2", "pve2:8443"},
		{"IPv4 seed", "192.0.2.10:8006", "pve2", "pve2:8006"},
		{"IPv4 seed keeps its port", "192.0.2.10:9006", "pve2", "pve2:9006"},
		{"bracketed IPv6 seed", "[fd00::1]:8006", "pve2", "pve2:8006"},
		{"bracketed IPv6 seed keeps its port", "[fd00::1]:9006", "pve2", "pve2:9006"},
		{"node name of one character", "pve1.example.com:8006", "n", "n.example.com:8006"},
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
		{"seed whose domain co.uk is the suffix", "example.co.uk:8006"},
		{"seed whose domain co.uk is the suffix, other port", "example.co.uk:9006"},
		{"seed one label under com", "pve1.com:8006"},
		{"seed one label under a two-label ICANN suffix", "pve1.co.uk:8006"},
		{"private suffix dynamic zone", "pve1.github.io:8006"},
		{"private suffix with another port", "pve1.github.io:9006"},
		{"another private suffix", "pve1.herokuapp.com:8006"},
		{"single label domain lan", "pve1.lan:8006"},
		{"single label domain internal", "pve1.internal:8006"},
		{"single label domain in upper case", "PVE1.LAN:8006"},
		{"unlisted top-level label", "pve1.notatld:8006"},
		{"domain is the parent of a wildcard under uk", "pve1.sch.uk:8006"},
		{"domain is the parent of a wildcard under jp", "pve1.kawasaki.jp:8006"},
		{"domain is the parent of a wildcard under br", "pve1.nom.br:8006"},
		{"domain is the parent of a wildcard under amazonaws", "pve1.compute.amazonaws.com:8006"},
		{"domain is the parent of a wildcard under another amazonaws zone", "pve1.compute-1.amazonaws.com:8006"},
		{"domain is the parent of a wildcard under run.app", "pve1.run.app:8006"},
		{"wildcard parent with upper case", "PVE1.SCH.UK:8006"},
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

func TestNewDeriverRefusesAWildcardParentOnce(t *testing.T) {
	t.Parallel()
	// The refusal comes from NewDeriver, so a caller with many nodes learns it
	// before it derives any of them.
	d, err := NewDeriver("pve1.sch.uk:8006")
	if !errors.Is(err, ErrPublicSuffix) || d != nil {
		t.Fatalf("NewDeriver = %v, %v; want ErrPublicSuffix and no deriver", d, err)
	}
}

func TestAddressChecksEveryDerivedHost(t *testing.T) {
	t.Parallel()
	// A hand-built Deriver skips NewDeriver's early check, so Address has to
	// refuse a host that leaves the seed's registrable domain on its own.
	d := &Deriver{domain: "sch.uk", registrable: "sch.uk", port: 8006}
	got, err := d.Address("pve2")
	if !errors.Is(err, ErrPublicSuffix) || got != "" {
		t.Fatalf("Address = %q, %v; want ErrPublicSuffix", got, err)
	}
	d = &Deriver{domain: "dc1.example.com", registrable: "example.com", port: 8006}
	if got, err := d.Address("PVE-2"); err != nil || got != "PVE-2.dc1.example.com:8006" {
		t.Fatalf("Address = %q, %v", got, err)
	}
}

func TestDeriveAddressAcceptsDC1ExampleCom(t *testing.T) {
	t.Parallel()
	got, err := DeriveAddress("pve1.dc1.example.com:8006", "pve2")
	if err != nil || got != "pve2.dc1.example.com:8006" {
		t.Fatalf("DeriveAddress = %q, %v", got, err)
	}
}

func TestDeriveAddressBareSeedsSkipThePublicSuffixCheck(t *testing.T) {
	t.Parallel()
	// A bare name has no domain to check, and an IP literal has none either, so
	// neither can take the public_suffix outcome.
	for _, seed := range []string{"lan:8006", "com:8006", "github:8006", "10.0.0.1:8006", "[::1]:8006", "[fd00::1]:8006"} {
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
		{"ip6-localhost", ErrNodeNameAmbiguous},
		{"IP6-Loopback", ErrNodeNameAmbiguous},
		{"127", ErrNodeNameAmbiguous},
		{"0x7f000001", ErrNodeNameAmbiguous},
	} {
		for _, seed := range []string{"pve1.dc1.example.com:8006", "pve1:8006", "192.0.2.10:8006"} {
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
		"empty":                       "",
		"scheme":                      "https://pve1.example.com",
		"path":                        "pve1.example.com/api2",
		"user info":                   "user@pve1.example.com",
		"whitespace":                  " pve1.example.com:8006",
		"port out of range":           "pve1.example.com:70000",
		"bracketed without a port":    "[fd00::1]",
		"empty label":                 "pve1..example.com:8006",
		"leading dot":                 ".example.com:8006",
		"two trailing dots":           "pve1.example.com..:8006",
		"no port on a name":           "pve1.dc1.example.com",
		"no port on a bare name":      "pve1",
		"no port on an IPv4 literal":  "192.0.2.10",
		"no port on an IPv6 literal":  "fd00::1",
		"lone dot":                    ".",
		"lone dot with a port":        ".:8006",
		"numeric single label":        "10.1:8006",
		"numeric two-label":           "127.0.1:8006",
		"numeric three-label":         "1.2.3:8006",
		"numeric four-label":          "0x7f.0.0.1:8006",
		"numeric last label":          "pve1.123:8006",
		"numeric with a trailing dot": "1.2.3.:8006",
	} {
		got, err := DeriveAddress(seed, "pve2")
		if !errors.Is(err, ErrSeedEndpoint) || got != "" {
			t.Errorf("%s: DeriveAddress(%q) = %q, %v; want ErrSeedEndpoint", name, seed, got, err)
		}
	}
}

func TestDeriveAddressAcceptsALastLabelWithDigitsAndLetters(t *testing.T) {
	t.Parallel()
	// Only an all-digit last label is refused. A name such as pve1.example.com
	// with digits inside its labels derives as usual.
	for _, seed := range []string{"pve1.dc1.example.com:8006", "pve1.example.xn--p1ai:8006", "pve1.123.example.com:8006"} {
		if _, err := DeriveAddress(seed, "pve2"); err != nil {
			t.Errorf("DeriveAddress(%q) error = %v", seed, err)
		}
	}
}

func TestDeriveAddressRefusesANameOverTheDNSLimit(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("b", 63)
	seed := "pve1." + label + "." + label + "." + label + ".com:8006"
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
	d, err := NewDeriver("pve1.lan:8006")
	if !errors.Is(err, ErrPublicSuffix) || d != nil {
		t.Fatalf("NewDeriver = %v, %v; want ErrPublicSuffix and no deriver", d, err)
	}
}

func TestDeriveErrorsDoNotEchoTheSeed(t *testing.T) {
	t.Parallel()
	for _, seed := range []string{"https://user:hunter2@pve1.example.com", "pve1.lan:8006", "pve1.sch.uk:8006"} {
		_, err := DeriveAddress(seed, "pve2")
		if err == nil {
			t.Fatalf("DeriveAddress(%q) succeeded", seed)
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "pve1") {
			t.Errorf("error %q echoes the seed", err)
		}
	}
}
