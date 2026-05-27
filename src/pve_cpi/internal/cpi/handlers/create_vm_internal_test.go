package handlers

import (
	"reflect"
	"testing"
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
