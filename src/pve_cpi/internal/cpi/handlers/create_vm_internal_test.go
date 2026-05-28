package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
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

// --------------------------------------------------------------------------
// resolveVMShape — vmStorageType field
// --------------------------------------------------------------------------

// shapeTestClusterStorage is a minimal clusterstorage.Service stub that returns
// a fixed list of storage entries from ListStorage. All other Service methods
// panic — they are not reached by resolveVMShape/lookupVMStorageType.
type shapeTestClusterStorage struct {
	entries []map[string]any
	err     error
}

func (s *shapeTestClusterStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	resp := make(sdkclusterstorage.ListStorageResponse, 0, len(s.entries))
	for _, e := range s.entries {
		raw, _ := json.Marshal(e)
		resp = append(resp, raw)
	}
	return &resp, nil
}
func (s *shapeTestClusterStorage) CreateStorage(_ context.Context, _ *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("shapeTestClusterStorage.CreateStorage: not expected")
}
func (s *shapeTestClusterStorage) DeleteStorage(_ context.Context, _ string) error {
	panic("shapeTestClusterStorage.DeleteStorage: not expected")
}
func (s *shapeTestClusterStorage) GetStorage(_ context.Context, _ string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("shapeTestClusterStorage.GetStorage: not expected")
}
func (s *shapeTestClusterStorage) UpdateStorage(_ context.Context, _ string, _ *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("shapeTestClusterStorage.UpdateStorage: not expected")
}

// compile-time check: shapeTestClusterStorage satisfies clusterstorage.Service.
var _ sdkclusterstorage.Service = (*shapeTestClusterStorage)(nil)

// shapeTestPVEClient implements pve.Client with a configurable ClusterStorage.
// All other service methods panic on call — they are not reached by resolveVMShape.
type shapeTestPVEClient struct {
	clusterStorageSvc sdkclusterstorage.Service
}

func (c *shapeTestPVEClient) QEMU() sdkqemu.Service { panic("shapeTestPVEClient.QEMU: not expected") }
func (c *shapeTestPVEClient) Storage() sdkstorage.Service {
	panic("shapeTestPVEClient.Storage: not expected")
}
func (c *shapeTestPVEClient) CloudInit() sdkcloudinit.Service {
	panic("shapeTestPVEClient.CloudInit: not expected")
}
func (c *shapeTestPVEClient) Tasks() sdktasks.Service {
	panic("shapeTestPVEClient.Tasks: not expected")
}
func (c *shapeTestPVEClient) Nodes() sdknodes.Service {
	panic("shapeTestPVEClient.Nodes: not expected")
}
func (c *shapeTestPVEClient) Cluster() sdkcluster.Service {
	panic("shapeTestPVEClient.Cluster: not expected")
}
func (c *shapeTestPVEClient) ClusterStorage() sdkclusterstorage.Service { return c.clusterStorageSvc }
func (c *shapeTestPVEClient) Pools() pve.PoolService {
	panic("shapeTestPVEClient.Pools: not expected")
}

// compile-time check: shapeTestPVEClient satisfies pve.Client.
var _ pve.Client = (*shapeTestPVEClient)(nil)

// minimalParsedArgs returns a *createVMParsedArgs with the stem fields needed
// by resolveVMShape. stemcellStorage is used as the fallback vmStorage when
// deps.Config.VMStorage is empty.
func minimalParsedArgs(stemcellStorage string) *createVMParsedArgs {
	return &createVMParsedArgs{
		agentID:      "agent-test",
		stemcellCID:  stemcellStorage + ":import/bosh-stemcell-test-1.0.qcow2",
		rawCID:       stemcellStorage + ":import/bosh-stemcell-test-1.0.qcow2",
		stemcellStor: stemcellStorage,
		cloudProps: createVMCloudProps{
			TargetNode: "pve",
		},
		networks: map[string]createVMNetworkSpec{},
		env:      map[string]any{},
	}
}

// TestResolveVMShape_StorageTypeLookup verifies that resolveVMShape populates
// shape.vmStorageType from the cluster storage list when ClusterStorage is wired.
func TestResolveVMShape_StorageTypeLookup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		configStor  string // deps.Config.VMStorage
		storEntries []map[string]any
		wantType    string
	}{
		{
			name:       "dir_storage_resolved",
			configStor: "local",
			storEntries: []map[string]any{
				{"storage": "local", "type": "dir"},
				{"storage": "local-lvm", "type": "lvm"},
			},
			wantType: "dir",
		},
		{
			name:       "lvm_storage_resolved",
			configStor: "local-lvm",
			storEntries: []map[string]any{
				{"storage": "local", "type": "dir"},
				{"storage": "local-lvm", "type": "lvm"},
			},
			wantType: "lvm",
		},
		{
			name:       "nfs_storage_resolved",
			configStor: "nfs-store",
			storEntries: []map[string]any{
				{"storage": "nfs-store", "type": "nfs"},
			},
			wantType: "nfs",
		},
		{
			name:       "storage_not_in_index_returns_empty",
			configStor: "missing-store",
			storEntries: []map[string]any{
				{"storage": "other", "type": "dir"},
			},
			wantType: "",
		},
		{
			name:        "empty_index_returns_empty",
			configStor:  "local",
			storEntries: []map[string]any{},
			wantType:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deps := Deps{
				Config: &config.CPIConfig{
					Node:           "pve",
					VMStorage:      tc.configStor,
					VMIDRangeStart: 100,
				},
				PVE: &shapeTestPVEClient{
					clusterStorageSvc: &shapeTestClusterStorage{entries: tc.storEntries},
				},
			}

			shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
			if err != nil {
				t.Fatalf("resolveVMShape returned error: %v", err)
			}
			if shape.vmStorageType != tc.wantType {
				t.Errorf("vmStorageType = %q; want %q", shape.vmStorageType, tc.wantType)
			}
			if shape.vmStorage != tc.configStor {
				t.Errorf("vmStorage = %q; want %q (config storage)", shape.vmStorage, tc.configStor)
			}
		})
	}
}

// TestResolveVMShape_StorageTypeNilClusterStorage verifies that resolveVMShape
// succeeds with vmStorageType="" when ClusterStorage() returns nil (e.g. test
// mocks that don't wire cluster storage).
func TestResolveVMShape_StorageTypeNilClusterStorage(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil, // not wired — simulates old mocks
		},
	}

	shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
	if err != nil {
		t.Fatalf("resolveVMShape returned error: %v", err)
	}
	if shape.vmStorageType != "" {
		t.Errorf("vmStorageType = %q; want empty string when ClusterStorage is nil", shape.vmStorageType)
	}
}

// TestResolveVMShape_StorageTypeLookupError verifies that a ClusterStorage
// ListStorage error leaves vmStorageType="" rather than propagating an error.
// create_vm must not fail due to a storage-type lookup that does not affect the
// import path.
func TestResolveVMShape_StorageTypeLookupError(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{
				err: fmt.Errorf("PVE /storage: connection refused"),
			},
		},
	}

	shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
	if err != nil {
		t.Fatalf("resolveVMShape must not return error on storage-type lookup failure; got: %v", err)
	}
	if shape.vmStorageType != "" {
		t.Errorf("vmStorageType = %q; want empty string on lookup error", shape.vmStorageType)
	}
}

// TestResolveVMShape_StorageTypeFallbackToStemcellStorage verifies that when
// Config.VMStorage is empty the vmStorage falls back to the stemcell's storage,
// and vmStorageType is looked up for that fallback storage.
func TestResolveVMShape_StorageTypeFallbackToStemcellStorage(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "", // not set — falls back to stemcell storage
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{
				entries: []map[string]any{
					{"storage": "stemcell-store", "type": "nfs"},
				},
			},
		},
	}

	parsed := minimalParsedArgs("stemcell-store")
	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape returned error: %v", err)
	}
	if shape.vmStorage != "stemcell-store" {
		t.Errorf("vmStorage = %q; want stemcell-store (stemcell fallback)", shape.vmStorage)
	}
	if shape.vmStorageType != "nfs" {
		t.Errorf("vmStorageType = %q; want nfs (looked up for fallback storage)", shape.vmStorageType)
	}
}

// --------------------------------------------------------------------------
// extractSHA8FromFilename
// --------------------------------------------------------------------------

// TestExtractSHA8FromFilename verifies sha8 extraction from stemcell filenames.
// The sha8 is the last "-"-delimited segment before ".qcow2". Filenames with
// hyphens in the name or version segments must still extract the trailing sha8.
func TestExtractSHA8FromFilename(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		filename string
		wantSHA8 string
		wantOK   bool
	}{
		{
			name:     "simple name and version",
			filename: "bosh-stemcell-ubuntu-jammy-1.438-abc12345.qcow2",
			wantSHA8: "abc12345",
			wantOK:   true,
		},
		{
			name:     "name with hyphens",
			filename: "bosh-stemcell-ubuntu-jammy-noble-1.438-deadbeef.qcow2",
			wantSHA8: "deadbeef",
			wantOK:   true,
		},
		{
			name:     "version with hyphens",
			filename: "bosh-stemcell-centos-9-stream-1.0-456-cafe1234.qcow2",
			wantSHA8: "cafe1234",
			wantOK:   true,
		},
		{
			name:     "uppercase hex letters",
			filename: "bosh-stemcell-foo-bar-1.0-ABCDEF01.qcow2",
			wantSHA8: "abcdef01",
			wantOK:   true,
		},
		{
			name:     "mixed-case hex letters",
			filename: "bosh-stemcell-test-1.0-AbCd1234.qcow2",
			wantSHA8: "abcd1234",
			wantOK:   true,
		},
		{
			name:     "no .qcow2 suffix",
			filename: "bosh-stemcell-ubuntu-jammy-1.438-abc12345.raw",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "sha8 too short (7 chars)",
			filename: "bosh-stemcell-foo-1.0-abc1234.qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "sha8 too long (9 chars)",
			filename: "bosh-stemcell-foo-1.0-abc123456.qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "non-hex char in sha8 (g)",
			filename: "bosh-stemcell-foo-1.0-abcg1234.qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "last segment wrong length (14 chars not 8)",
			filename: "bosh-stemcell-foobababcdef12.qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "empty string",
			filename: "",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "only .qcow2",
			filename: ".qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSHA8, gotOK := extractSHA8FromFilename(tc.filename)
			if gotOK != tc.wantOK {
				t.Errorf("extractSHA8FromFilename(%q): ok=%v, want %v", tc.filename, gotOK, tc.wantOK)
			}
			if gotSHA8 != tc.wantSHA8 {
				t.Errorf("extractSHA8FromFilename(%q): sha8=%q, want %q", tc.filename, gotSHA8, tc.wantSHA8)
			}
		})
	}
}

// TestExtractSHA8FromFilenameInCID verifies sha8 extraction from raw CIDs.
func TestExtractSHA8FromFilenameInCID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rawCID   string
		wantSHA8 string
		wantOK   bool
	}{
		{
			name:     "valid CID with sha8",
			rawCID:   "local:import/bosh-stemcell-ubuntu-jammy-1.438-abc12345.qcow2",
			wantSHA8: "abc12345",
			wantOK:   true,
		},
		{
			name:     "light-stripped CID with sha8",
			rawCID:   "test-storage:import/bosh-stemcell-ubuntu-jammy-noble-1.438-deadbeef.qcow2",
			wantSHA8: "deadbeef",
			wantOK:   true,
		},
		{
			name:     "CID without .qcow2 suffix",
			rawCID:   "local:import/bosh-stemcell-ubuntu-1.0-abc12345.raw",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "invalid CID (no colon)",
			rawCID:   "notavolid",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "empty CID",
			rawCID:   "",
			wantSHA8: "",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSHA8, gotOK := extractSHA8FromFilenameInCID(tc.rawCID)
			if gotOK != tc.wantOK {
				t.Errorf("extractSHA8FromFilenameInCID(%q): ok=%v, want %v", tc.rawCID, gotOK, tc.wantOK)
			}
			if gotSHA8 != tc.wantSHA8 {
				t.Errorf("extractSHA8FromFilenameInCID(%q): sha8=%q, want %q", tc.rawCID, gotSHA8, tc.wantSHA8)
			}
		})
	}
}
