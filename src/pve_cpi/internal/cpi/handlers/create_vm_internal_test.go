package handlers

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// TestSortedNetworkNames_NoDefault_Deterministic verifies that sortedNetworkNames
// returns a stable alphabetical order when "default" is absent. Go map iteration
// is randomised, so the function is called 10 times; all results must be identical.
// This is the regression test for bug B8.
func TestSortedNetworkNames_NoDefault_Deterministic(t *testing.T) {
	t.Parallel()

	networks := map[string]createVMNetworkSpec{
		"zebra": {},
		"alpha": {},
		"mango": {},
	}
	want := []string{"alpha", "mango", "zebra"}

	for i := 0; i < 10; i++ {
		got := sortedNetworkNames(networks)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %v, want %v", i, got, want)
		}
	}
}

// TestSortedNetworkNames_WithDefault verifies that "default" is placed first and
// the remaining names are sorted alphabetically.
func TestSortedNetworkNames_WithDefault(t *testing.T) {
	t.Parallel()

	networks := map[string]createVMNetworkSpec{
		"zebra":   {},
		"default": {},
		"alpha":   {},
	}
	want := []string{"default", "alpha", "zebra"}

	for i := 0; i < 10; i++ {
		got := sortedNetworkNames(networks)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %v, want %v", i, got, want)
		}
	}
}

// TestSortedNetworkNames_OnlyDefault verifies single-element map with only "default".
func TestSortedNetworkNames_OnlyDefault(t *testing.T) {
	t.Parallel()

	networks := map[string]createVMNetworkSpec{
		"default": {},
	}
	want := []string{"default"}
	got := sortedNetworkNames(networks)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestSortedNetworkNames_Empty verifies empty map returns empty slice (not nil panic).
func TestSortedNetworkNames_Empty(t *testing.T) {
	t.Parallel()

	got := sortedNetworkNames(map[string]createVMNetworkSpec{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

// TestExtractMBusAndBlobstore_DirectorDeployFlatString covers the BOSH-director
// shape: env.bosh.mbus is a STRING (e.g. "nats://10.0.0.1:4222"). This is the
// shape used for all post-bootstrap deploys; bosh-init/create-env uses the
// nested object shape covered by TestExtractMBusAndBlobstore_CreateEnvObject.
func TestExtractMBusAndBlobstore_DirectorDeployFlatString(t *testing.T) {
	t.Parallel()

	env := map[string]any{
		"bosh": map[string]any{
			"mbus": "nats://10.0.0.1:4222",
			"blobstores": []any{
				map[string]any{
					"provider": "dav",
					"options": map[string]any{
						"endpoint": "https://10.0.0.1:25250",
						"user":     "agent",
						"password": "secret",
					},
				},
			},
		},
	}

	mbus, bs := extractMBusAndBlobstore(env)
	if mbus != "nats://10.0.0.1:4222" {
		t.Errorf("mbus = %q, want nats://10.0.0.1:4222", mbus)
	}
	if bs.Provider != "dav" {
		t.Errorf("blobstore.provider = %q, want dav", bs.Provider)
	}
	if endpoint, _ := bs.Options["endpoint"].(string); endpoint != "https://10.0.0.1:25250" {
		t.Errorf("blobstore.options.endpoint = %q, want https://10.0.0.1:25250", endpoint)
	}
}

// TestExtractMBusAndBlobstore_CreateEnvObject covers the bosh-init/create-env
// shape: env.bosh.mbus is an OBJECT with .url (plus TLS cert fields). The
// extractor picks .url.
func TestExtractMBusAndBlobstore_CreateEnvObject(t *testing.T) {
	t.Parallel()

	env := map[string]any{
		"bosh": map[string]any{
			"mbus": map[string]any{
				"url": "https://mbus:secret@0.0.0.0:6868",
				"cert": map[string]any{
					"ca": "-----BEGIN CERTIFICATE-----\n...",
				},
			},
		},
	}

	mbus, _ := extractMBusAndBlobstore(env)
	if mbus != "https://mbus:secret@0.0.0.0:6868" {
		t.Errorf("mbus = %q, want https://mbus:secret@0.0.0.0:6868", mbus)
	}
}

// TestExtractMBusAndBlobstore_LegacyTopLevel covers an out-of-band/legacy
// caller that puts env["mbus"] and env["blobstore"] at the top level (not
// under env.bosh). Accepted as a compatibility fallback.
func TestExtractMBusAndBlobstore_LegacyTopLevel(t *testing.T) {
	t.Parallel()

	env := map[string]any{
		"mbus": "nats://legacy:4222",
		"blobstore": map[string]any{
			"provider": "local",
			"options":  map[string]any{"path": "/var/blob"},
		},
	}

	mbus, bs := extractMBusAndBlobstore(env)
	if mbus != "nats://legacy:4222" {
		t.Errorf("mbus = %q, want nats://legacy:4222", mbus)
	}
	if bs.Provider != "local" {
		t.Errorf("blobstore.provider = %q, want local", bs.Provider)
	}
}

// --------------------------------------------------------------------------
// createVMRetryBackoff
// --------------------------------------------------------------------------

// TestCreateVMRetryBackoff_StorageLockTimeout verifies that a storage-lock
// timeout error produces a duration that grows with attempt count and stays
// within the [d×0.7, d×1.3] window (exponential base 2s × 1.5^attempt ±30%).
//
// N=100 samples are drawn per attempt to reduce single-draw boundary flakes.
// All 100 must fall within the window; if the RNG ever produces a value outside
// [0.69, 1.31]×expected, that is a real implementation bug, not a boundary quirk.
func TestCreateVMRetryBackoff_StorageLockTimeout(t *testing.T) {
	t.Parallel()

	const samples = 100
	lockErr := fmt.Errorf("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")

	for attempt := 0; attempt <= 5; attempt++ {
		// Compute expected base: 2s × 1.5^attempt, capped at 30s.
		base := 2 * time.Second
		factor := 1.0
		for i := 0; i < attempt; i++ {
			factor *= 1.5
		}
		expected := time.Duration(float64(base) * factor)
		if expected > 30*time.Second {
			expected = 30 * time.Second
		}

		// Allowed window: [expected×0.69, expected×1.31] (±31% — 1% slack on
		// the ±30% jitter window to absorb integer-truncation at small durations).
		lo := time.Duration(float64(expected) * 0.69)
		hi := time.Duration(float64(expected) * 1.31)

		for n := range samples {
			d := createVMRetryBackoff(lockErr, attempt)
			if d < lo || d > hi {
				t.Errorf("attempt=%d sample=%d: backoff=%v not in [%v, %v] (expected base %v)",
					attempt, n, d, lo, hi, expected)
			}
		}
	}
}

// TestCreateVMRetryBackoff_StorageLockTimeout_Cap verifies that the base
// duration is capped at 30s before jitter. The jittered output may be up to
// 30s×1.3 (capped base ±30%), but the base itself must not grow beyond 30s.
// We check that the result stays within [30s×0.7, 30s×1.3].
func TestCreateVMRetryBackoff_StorageLockTimeout_Cap(t *testing.T) {
	t.Parallel()

	lockErr := fmt.Errorf("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	// attempt=20 pushes the raw exponential far above 30s; the base is capped at 30s
	// before jitter, so the jittered output is in [30s×0.7, 30s×1.3].
	const samples = 100
	const cappedBase = 30 * time.Second
	lo := time.Duration(float64(cappedBase) * 0.69)
	hi := time.Duration(float64(cappedBase) * 1.31)

	for n := range samples {
		d := createVMRetryBackoff(lockErr, 20)
		if d < lo || d > hi {
			t.Errorf("attempt=20 (sample %d): backoff=%v not in [%v, %v]", n, d, lo, hi)
		}
	}
}

// TestCreateVMRetryBackoff_NonRetriable verifies that errors not matching
// IsStorageLockTimeout (auth failures, permission errors, opaque errors) fall
// through to the uniform 50–250 ms jitter branch. createVMRetryBackoff does
// not gate on retryability — that is the caller's responsibility — so any
// non-storage-lock error uses the short jitter window.
func TestCreateVMRetryBackoff_NonRetriable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"auth failure", fmt.Errorf("401 authentication failure")},
		{"permission denied", fmt.Errorf("permission denied")},
		{"storage full", fmt.Errorf("storage full")},
		{"vmid conflict", fmt.Errorf("VM 113 already exists on node 'pve'")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for attempt := 0; attempt <= 3; attempt++ {
				d := createVMRetryBackoff(tc.err, attempt)
				const lo = 50 * time.Millisecond
				const hi = 250 * time.Millisecond
				if d < lo || d > hi {
					t.Errorf("%s attempt=%d: backoff=%v not in [%v, %v]",
						tc.name, attempt, d, lo, hi)
				}
			}
		})
	}
}

// TestCreateVMRetryBackoff_UnknownErrorType verifies that an opaque error with
// no pve fingerprint also falls through to the 50–250 ms uniform jitter branch.
func TestCreateVMRetryBackoff_UnknownErrorType(t *testing.T) {
	t.Parallel()

	unknownErr := fmt.Errorf("some opaque error with no pve fingerprint whatsoever")
	d := createVMRetryBackoff(unknownErr, 0)
	const lo = 50 * time.Millisecond
	const hi = 250 * time.Millisecond
	if d < lo || d > hi {
		t.Errorf("unknown error type: backoff=%v not in [%v, %v]", d, lo, hi)
	}
}

// --------------------------------------------------------------------------
// normalizeOSType
// --------------------------------------------------------------------------

// TestNormalizeOSType covers all mapped BOSH/stemcell os_type values plus
// empty input and unknown inputs (which are returned verbatim for PVE
// validation).
func TestNormalizeOSType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		// Linux 2.6+ inputs → l26.
		{"linux", "l26"},
		{"ubuntu", "l26"},
		{"centos", "l26"},
		{"rhel", "l26"},
		{"debian", "l26"},
		{"fedora", "l26"},
		{"alpine", "l26"},
		{"l26", "l26"},
		// Linux 2.4 → l24.
		{"linux24", "l24"},
		{"l24", "l24"},
		// Windows variants.
		{"windows", "win10"},
		{"win", "win10"},
		{"win10", "win10"},
		{"win11", "win11"},
		{"win7", "win7"},
		{"win8", "win8"},
		// Solaris.
		{"solaris", "solaris"},
		// Pass-through: unknown values returned verbatim for PVE to validate.
		{"other-linux", "other-linux"},
		{"wvista", "wvista"},
		{"w2k8", "w2k8"},
		{"custom-os", "custom-os"},
		// Empty string returns empty string (verbatim pass-through).
		{"", ""},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("input=%q", tc.input), func(t *testing.T) {
			t.Parallel()
			got := normalizeOSType(tc.input)
			if got != tc.want {
				t.Errorf("normalizeOSType(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}
