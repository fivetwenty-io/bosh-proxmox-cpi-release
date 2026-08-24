// Package pve white-box tests for assertNoFingerprintPinningWithRouting
// (F-04): TLS fingerprint pinning is incompatible with direct-to-node
// upload routing, since the SDK rejects any per-request host-overridden
// request outright once pinning is enabled. Uses package pve (not pve_test)
// to construct sdkclient.Options with the pinning fields directly.
package pve

import (
	"crypto/x509"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func TestAssertNoFingerprintPinningWithRouting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		opts            sdkclient.Options
		routingPossible bool
		wantErr         bool
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
			err := assertNoFingerprintPinningWithRouting(tc.opts, tc.routingPossible)
			if tc.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}
