// Package pve white-box tests for assertNoFingerprintPinningWithRouting
// (F-04): TLS fingerprint pinning is incompatible with direct-to-node
// upload routing, since the SDK rejects any per-request host-overridden
// request outright once pinning is enabled. Uses package pve (not pve_test)
// to construct sdkclient.Options with the pinning fields directly.
package pve

import (
	"crypto/x509"
	"strings"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func TestAssertNoFingerprintPinningWithRouting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		opts        sdkclient.Options
		nodeRouting bool
		wantErr     bool
	}{
		{"no pinning, routing possible", sdkclient.Options{}, true, false},
		{"no pinning, routing impossible", sdkclient.Options{}, false, false},
		{
			"cached fingerprints + routing",
			sdkclient.Options{CachedFingerprints: map[string]bool{"aa:bb": true}},
			true, true,
		},
		{
			"cached fingerprints, routing impossible",
			sdkclient.Options{CachedFingerprints: map[string]bool{"aa:bb": true}},
			false, false,
		},
		{"manual verification + routing", sdkclient.Options{ManualVerification: true}, true, true},
		{"manual verification, routing impossible", sdkclient.Options{ManualVerification: true}, false, false},
		{
			"verify fingerprint callback + routing",
			sdkclient.Options{VerifyFingerprintCallback: func(*x509.Certificate) bool { return true }},
			true, true,
		},
		{
			"manual verify callback + routing",
			sdkclient.Options{ManualVerifyCallback: func(sdkclient.FingerprintVerificationRequest) bool { return true }},
			true, true,
		},
		{
			"fingerprint cache path + routing",
			sdkclient.Options{FingerprintCachePath: "/var/lib/pve-cpi/fingerprints.json"},
			true, true,
		},
		{
			"fingerprint cache path, routing impossible",
			sdkclient.Options{FingerprintCachePath: "/var/lib/pve-cpi/fingerprints.json"},
			false, false,
		},
		{
			"empty cached fingerprints map is not pinning",
			sdkclient.Options{CachedFingerprints: map[string]bool{}},
			true, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := assertNoFingerprintPinningWithRouting(tc.opts, tc.nodeRouting, false)
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

// The failover case has its own message. It names pve.endpoints, says that
// fingerprint pinning cannot combine with endpoint failover, and does not
// mention discovery or node routing, which are separate settings.
func TestAssertNoFingerprintPinningWithFailoverMessage(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]sdkclient.Options{
		"cached fingerprints":   {CachedFingerprints: map[string]bool{"aa:bb": true}},
		"manual verification":   {ManualVerification: true},
		"fingerprint callback":  {VerifyFingerprintCallback: func(*x509.Certificate) bool { return true }},
		"manual verify handler": {ManualVerifyCallback: func(sdkclient.FingerprintVerificationRequest) bool { return true }},
		"fingerprint cache":     {FingerprintCachePath: "/var/lib/pve-cpi/fingerprints.json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := assertNoFingerprintPinningWithRouting(opts, false, true)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			text := err.Error()
			for _, want := range []string{
				"pve.endpoints",
				"fingerprint pinning cannot combine with endpoint failover",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("message %q does not contain %q", text, want)
				}
			}
			for _, banned := range []string{"discovery", "node_endpoints", "upload"} {
				if strings.Contains(text, banned) {
					t.Errorf("message %q mentions %q", text, banned)
				}
			}
		})
	}
}

// Without failover the node-routing message is unchanged, and with no pinning
// the failover flag changes nothing.
func TestAssertNoFingerprintPinningRoutingMessageKeepsItsText(t *testing.T) {
	t.Parallel()
	pinned := sdkclient.Options{ManualVerification: true}
	err := assertNoFingerprintPinningWithRouting(pinned, true, false)
	if err == nil || !strings.Contains(err.Error(), "direct-to-node upload routing") {
		t.Fatalf("node routing refusal = %v, want the direct-to-node message", err)
	}
	if strings.Contains(err.Error(), "endpoint failover") {
		t.Errorf("node routing refusal mentions failover: %v", err)
	}
	if err := assertNoFingerprintPinningWithRouting(sdkclient.Options{}, true, true); err != nil {
		t.Errorf("failover without pinning = %v, want nil", err)
	}
	if err := assertNoFingerprintPinningWithRouting(pinned, false, false); err != nil {
		t.Errorf("pinning without routing possible = %v, want nil", err)
	}
}

// When both causes apply, one error names both, so the operator sees the
// failover cause and the node routing cause on the first failed start. Each
// cause keeps the text it has alone.
func TestAssertNoFingerprintPinningNamesBothCauses(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]sdkclient.Options{
		"cached fingerprints":   {CachedFingerprints: map[string]bool{"aa:bb": true}},
		"manual verification":   {ManualVerification: true},
		"fingerprint callback":  {VerifyFingerprintCallback: func(*x509.Certificate) bool { return true }},
		"manual verify handler": {ManualVerifyCallback: func(sdkclient.FingerprintVerificationRequest) bool { return true }},
		"fingerprint cache":     {FingerprintCachePath: "/var/lib/pve-cpi/fingerprints.json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			both := assertNoFingerprintPinningWithRouting(opts, true, true)
			failover := assertNoFingerprintPinningWithRouting(opts, false, true)
			node := assertNoFingerprintPinningWithRouting(opts, true, false)
			if both == nil || failover == nil || node == nil {
				t.Fatalf("refusals = %v, %v, %v, want all three", both, failover, node)
			}
			text := both.Error()
			if !strings.HasPrefix(text, "pve client init: ") {
				t.Errorf("combined message %q does not start with the init prefix", text)
			}
			for _, part := range []string{
				strings.TrimPrefix(failover.Error(), "pve client init: "),
				strings.TrimPrefix(node.Error(), "pve client init: "),
			} {
				if !strings.Contains(text, part) {
					t.Errorf("combined message %q does not contain %q", text, part)
				}
			}
			if strings.Index(text, "endpoint failover") > strings.Index(text, "direct-to-node upload routing") {
				t.Errorf("combined message %q names node routing before failover", text)
			}
		})
	}
}
