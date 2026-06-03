package handlers

import (
	"context"
	stderrors "errors"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
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

// ---------------------------------------------------------------------------
// classifyFilterResult
// ---------------------------------------------------------------------------

func TestClassifyFilterResult_AllTransient_Retriable(t *testing.T) {
	t.Parallel()
	rejections := map[string]string{
		"pve1": "node offline",
		"pve2": "node in maintenance",
		"pve3": "insufficient CPU",
		"pve4": "insufficient free memory",
	}
	if !classifyFilterResult(rejections) {
		t.Error("classifyFilterResult: expected retriable=true for all-transient rejections")
	}
}

func TestClassifyFilterResult_AnyPermanent_NotRetriable(t *testing.T) {
	t.Parallel()
	// "unknown reason" is not a recognised transient cause → permanent → not retriable.
	rejections := map[string]string{
		"pve1": "node offline",
		"pve2": "unknown rejection reason",
	}
	if classifyFilterResult(rejections) {
		t.Error("classifyFilterResult: expected retriable=false when any rejection is a permanent cause")
	}
}

// TestClassifyFilterResult_NotInCandidateSet_Neutral verifies that
// "not in candidate node set" is treated as neutral (retriable), not permanent.
// In multi-AZ merges, out-of-AZ healthy nodes contribute this reason; it must
// not poison the retriability classification when the actual failure is
// transient (e.g. the only in-AZ candidate is temporarily offline).
func TestClassifyFilterResult_NotInCandidateSet_Neutral(t *testing.T) {
	t.Parallel()

	// Pure candidate-set rejections → retriable.
	onlyCandidateSet := map[string]string{
		"pve2": "not in candidate node set",
		"pve3": "not in candidate node set",
	}
	if !classifyFilterResult(onlyCandidateSet) {
		t.Error("classifyFilterResult: expected retriable=true for pure candidate-set rejections")
	}

	// Transient + candidate-set mix → retriable (the cross-AZ poisoning case).
	mixedTransientAndCandidateSet := map[string]string{
		"pve1": "node offline",
		"pve2": "not in candidate node set",
	}
	if !classifyFilterResult(mixedTransientAndCandidateSet) {
		t.Error("classifyFilterResult: in-AZ offline + out-of-AZ candidate-set must be retriable")
	}
}

func TestClassifyFilterResult_EmptyRejections_Retriable(t *testing.T) {
	t.Parallel()
	if !classifyFilterResult(nil) {
		t.Error("classifyFilterResult: expected retriable=true for nil/empty rejections")
	}
	if !classifyFilterResult(map[string]string{}) {
		t.Error("classifyFilterResult: expected retriable=true for empty map")
	}
}

// ---------------------------------------------------------------------------
// resolveTargetNodeWithRNG — multi-AZ and retryability
// ---------------------------------------------------------------------------

// placementInternalTestCluster is a minimal placement.ClusterClient for
// resolveTargetNodeWithRNG tests in the internal package.
type placementInternalTestCluster struct {
	nodes []map[string]any
}

func (c *placementInternalTestCluster) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	resp := make(sdkcluster.ListStatusResponse, 0, len(c.nodes))
	for _, n := range c.nodes {
		raw, _ := json.Marshal(n)
		resp = append(resp, raw)
	}
	return &resp, nil
}

func (c *placementInternalTestCluster) ListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

func (c *placementInternalTestCluster) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

var _ placement.ClusterClient = (*placementInternalTestCluster)(nil)

// placementInternalTestPVE wraps the test cluster + nodes clients.
// Cluster() returns a full cluster.Service wrapper around the subset client.
type placementInternalTestPVE struct {
	clusterClient *placementInternalTestCluster
}

func (p *placementInternalTestPVE) QEMU() sdkqemu.Service         { panic("not needed") }
func (p *placementInternalTestPVE) Nodes() sdknodes.Service       { return &placementInternalNodesSvc{} }
func (p *placementInternalTestPVE) Tasks() sdktasks.Service        { panic("not needed") }
func (p *placementInternalTestPVE) Storage() sdkstorage.Service    { panic("not needed") }
func (p *placementInternalTestPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *placementInternalTestPVE) Cluster() sdkcluster.Service    { return &fullClusterAdapter{sub: p.clusterClient} }
func (p *placementInternalTestPVE) ClusterStorage() sdkclusterstorage.Service {
	return &localStorageSvc{}
}
func (p *placementInternalTestPVE) Pools() pve.PoolService { return nil }

var _ pve.Client = (*placementInternalTestPVE)(nil)

// fullClusterAdapter wraps a placement.ClusterClient subset as a full cluster.Service.
// It embeds sdkcluster.Service (nil) to satisfy the full interface via nil-pointer panic
// for any method not overridden. Only the three methods used by placement are forwarded.
type fullClusterAdapter struct {
	sdkcluster.Service // nil — panics on any non-overridden call
	sub placement.ClusterClient
}

func (a *fullClusterAdapter) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	return a.sub.ListStatus(ctx)
}
func (a *fullClusterAdapter) ListResources(ctx context.Context, p *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	return a.sub.ListResources(ctx, p)
}
func (a *fullClusterAdapter) ListHaStatusCurrent(ctx context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	return a.sub.ListHaStatusCurrent(ctx)
}

var _ sdkcluster.Service = (*fullClusterAdapter)(nil)

// placementInternalNodesSvc satisfies sdknodes.Service with ListStorage wired.
// All other methods panic.
type placementInternalNodesSvc struct {
	sdknodes.Service // nil — panics on non-overridden calls
}

func (s *placementInternalNodesSvc) ListStorage(_ context.Context, _ string, params *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
	storageName := "local-lvm"
	if params != nil && params.Storage != nil {
		storageName = *params.Storage
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": storageName, "active": 1, "enabled": 1,
		"content": "images", "avail": int64(10 * 1024 * 1024 * 1024), "total": int64(100 * 1024 * 1024 * 1024),
	})
	resp := sdknodes.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}

// localStorageSvc reports the test storage as non-shared (local).
type localStorageSvc struct{}

func (s *localStorageSvc) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	raw, _ := json.Marshal(map[string]any{"storage": "local-lvm", "type": "lvm", "shared": 0})
	resp := sdkclusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}
func (s *localStorageSvc) CreateStorage(_ context.Context, _ *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("not needed")
}
func (s *localStorageSvc) DeleteStorage(_ context.Context, _ string) error { panic("not needed") }
func (s *localStorageSvc) GetStorage(_ context.Context, _ string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("not needed")
}
func (s *localStorageSvc) UpdateStorage(_ context.Context, _ string, _ *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("not needed")
}

var _ sdkclusterstorage.Service = (*localStorageSvc)(nil)

// onlineNode builds a cluster status map for an online node.
func onlineNode(name string) map[string]any {
	return map[string]any{
		"type": "node", "name": name, "online": 1,
		"maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024),
		"mem": int64(2 * 1024 * 1024 * 1024), "cpu": 0.1,
	}
}

// buildPlacementDeps builds Deps wired for resolveTargetNodeWithRNG tests.
func buildPlacementDeps(nodes []map[string]any, cfgFn func(*config.CPIConfig)) Deps {
	cfg := &config.CPIConfig{
		Host:          "pve.test",
		Node:          "",
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr0",
	}
	if cfgFn != nil {
		cfgFn(cfg)
	}
	return Deps{
		Config: cfg,
		PVE: &placementInternalTestPVE{
			clusterClient: &placementInternalTestCluster{nodes: nodes},
		},
		Logger: log.NewNopLogger(),
	}
}

func TestResolveTargetNodeWithRNG_SingleAZ_BackwardCompat(t *testing.T) {
	t.Parallel()
	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1 (only candidate in zone-a); got %q", node)
	}
}

func TestResolveTargetNodeWithRNG_MultiAZ_FirstAZViable(t *testing.T) {
	t.Parallel()
	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
	})
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a", "zone-b"}}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1 (first viable AZ); got %q", node)
	}
}

func TestResolveTargetNodeWithRNG_MultiAZ_FirstExhausted_FallsToSecond(t *testing.T) {
	t.Parallel()
	// pve1 is offline → zone-a exhausted; pve2 is online → zone-b selected.
	deps := buildPlacementDeps([]map[string]any{
		{"type": "node", "name": "pve1", "online": 0, "maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0},
		onlineNode("pve2"),
	}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
	})
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a", "zone-b"}}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("expected pve2 (zone-a exhausted, zone-b fallback); got %q", node)
	}
}

func TestResolveTargetNodeWithRNG_MultiAZ_AllExhausted_RetriableError(t *testing.T) {
	t.Parallel()
	// Both nodes offline → all AZs exhausted → retriable error (no config.Node fallback).
	deps := buildPlacementDeps([]map[string]any{
		{"type": "node", "name": "pve1", "online": 0, "maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0},
		{"type": "node", "name": "pve2", "online": 0, "maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0},
	}, func(c *config.CPIConfig) {
		c.Node = "" // no fallback
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
	})
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a", "zone-b"}}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err == nil {
		t.Fatal("expected error for all-AZs-exhausted")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error; got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("expected retriable error; got non-retriable: %v", err)
	}
}

// TestResolveTargetNodeWithRNG_InAZOffline_OutAZOnline_Retriable reproduces the
// cross-AZ retriability poison: zone-a has pve1 (offline, transient), pve2 is
// online but outside zone-a. allRejections merges pve1=offline + pve2=not-in-
// candidate-node-set. The error must be retriable, not permanent.
func TestResolveTargetNodeWithRNG_InAZOffline_OutAZOnline_Retriable(t *testing.T) {
	t.Parallel()
	deps := buildPlacementDeps([]map[string]any{
		{"type": "node", "name": "pve1", "online": 0, "maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0},
		onlineNode("pve2"),
	}, func(c *config.CPIConfig) {
		c.Node = "" // no config.Node fallback
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"}, // pve1 is offline; pve2 not in zone-a
			},
		}
	})
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a"}}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err == nil {
		t.Fatal("expected error when all in-AZ candidates are offline")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error; got %T: %v", err, err)
	}
	if !cpiErr.OkToRetry() {
		t.Errorf("in-AZ offline + out-of-AZ healthy node: error must be retriable (transient); got non-retriable: %v", err)
	}
}

func TestResolveTargetNodeWithRNG_UnknownAZ_PermanentError(t *testing.T) {
	t.Parallel()
	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-unknown"}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil)
	if err == nil {
		t.Fatal("expected error for unknown AZ")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error; got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("expected non-retriable error for unknown AZ (misconfiguration)")
	}
	if !strings.Contains(err.Error(), "zone-unknown") {
		t.Errorf("error should mention the AZ name; got: %v", err)
	}
}

// TestResolveTargetNodeWithRNG_AZShuffle_SeedControlsOrder verifies that the
// injected rng seed deterministically controls which AZ is tried first, and
// therefore which node is selected. Two viable AZs, each with a distinct
// healthy node; different seeds produce different first-AZ choices → different
// selected nodes. This proves the shuffle actually reorders, not just stabilises.
//
// Empirically (rand.New(rand.NewSource(N)).Shuffle on ["zone-a","zone-b"]):
//   seed 0 → [zone-a, zone-b] → pve1 selected (zone-a tried first).
//   seed 2 → [zone-b, zone-a] → pve2 selected (zone-b tried first).
func TestResolveTargetNodeWithRNG_AZShuffle_SeedControlsOrder(t *testing.T) {
	t.Parallel()
	tr := true
	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Node = "" // no fallback so the picked AZ winner is the final answer
		c.Placement = &config.PlacementConfig{
			AZShuffle: &tr,
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
	})
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a", "zone-b"}}

	// seed 0: shuffle keeps [zone-a, zone-b] → zone-a first → pve1 wins.
	rngA := rand.New(rand.NewSource(0)) //nolint:gosec // fixed seed — deterministic test
	nodeA, errA := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", rngA)
	if errA != nil {
		t.Fatalf("seed=0 error: %v", errA)
	}

	// seed 2: shuffle produces [zone-b, zone-a] → zone-b first → pve2 wins.
	rngB := rand.New(rand.NewSource(2)) //nolint:gosec // fixed seed — deterministic test
	nodeB, errB := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", rngB)
	if errB != nil {
		t.Fatalf("seed=2 error: %v", errB)
	}

	// Same seed must be deterministic.
	rngA2 := rand.New(rand.NewSource(0)) //nolint:gosec // fixed seed — deterministic test
	nodeA2, _ := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", rngA2)
	if nodeA != nodeA2 {
		t.Errorf("same seed must be deterministic; seed=0 got %q then %q", nodeA, nodeA2)
	}

	// Different seeds must produce different first-AZ picks (and thus different nodes).
	if nodeA == nodeB {
		t.Errorf("seed=0 and seed=2 should pick different nodes (proving reorder); both got %q", nodeA)
	}
	if nodeA != "pve1" {
		t.Errorf("seed=0 → zone-a first → expected pve1; got %q", nodeA)
	}
	if nodeB != "pve2" {
		t.Errorf("seed=2 → zone-b first → expected pve2; got %q", nodeB)
	}
}

func TestResolveTargetNodeWithRNG_MaintenanceNodeExcluded(t *testing.T) {
	t.Parallel()
	// pve1 carries maintenance tag; pve2 is clean.
	// ExcludeMaintenanceNodes defaults true (nil Placement field).
	type taggedNode struct {
		Type   string  `json:"type"`
		Name   string  `json:"name"`
		Online int     `json:"online"`
		Maxcpu int64   `json:"maxcpu"`
		Maxmem int64   `json:"maxmem"`
		Mem    int64   `json:"mem"`
		CPU    float64 `json:"cpu"`
		Tags   string  `json:"tags,omitempty"`
	}
	n1raw, _ := json.Marshal(taggedNode{
		Type: "node", Name: "pve1", Online: 1, Maxcpu: 4,
		Maxmem: 8 * 1024 * 1024 * 1024, Mem: 2 * 1024 * 1024 * 1024, CPU: 0.1,
		Tags: "maintenance",
	})
	n2raw, _ := json.Marshal(taggedNode{
		Type: "node", Name: "pve2", Online: 1, Maxcpu: 4,
		Maxmem: 8 * 1024 * 1024 * 1024, Mem: 2 * 1024 * 1024 * 1024, CPU: 0.1,
	})
	cluster := &rawNodeCluster{nodes: []json.RawMessage{n1raw, n2raw}}
	cfg := &config.CPIConfig{
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr0",
		Placement:     &config.PlacementConfig{}, // nil ExcludeMaintenanceNodes → true
	}
	deps := Deps{
		Config: cfg,
		PVE:    &rawNodeTestPVE{cluster},
		Logger: log.NewNopLogger(),
	}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("expected pve2 (pve1 excluded by maintenance tag); got %q", node)
	}
}

// rawNodeCluster is a placement.ClusterClient that returns pre-built raw JSON nodes.
type rawNodeCluster struct {
	nodes []json.RawMessage
}

func (c *rawNodeCluster) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	resp := sdkcluster.ListStatusResponse(c.nodes)
	return &resp, nil
}
func (c *rawNodeCluster) ListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}
func (c *rawNodeCluster) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

var _ placement.ClusterClient = (*rawNodeCluster)(nil)

// rawNodeTestPVE wraps rawNodeCluster as pve.Client.
type rawNodeTestPVE struct{ cluster *rawNodeCluster }

func (p *rawNodeTestPVE) QEMU() sdkqemu.Service         { panic("not needed") }
func (p *rawNodeTestPVE) Nodes() sdknodes.Service       { return &placementInternalNodesSvc{} }
func (p *rawNodeTestPVE) Tasks() sdktasks.Service        { panic("not needed") }
func (p *rawNodeTestPVE) Storage() sdkstorage.Service    { panic("not needed") }
func (p *rawNodeTestPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *rawNodeTestPVE) Cluster() sdkcluster.Service    { return &fullClusterAdapter{sub: p.cluster} }
func (p *rawNodeTestPVE) ClusterStorage() sdkclusterstorage.Service {
	return &localStorageSvc{}
}
func (p *rawNodeTestPVE) Pools() pve.PoolService { return nil }

var _ pve.Client = (*rawNodeTestPVE)(nil)

// ---------------------------------------------------------------------------
// Template-gap guard: attemptCreateVM replica-lookup branches
// ---------------------------------------------------------------------------
//
// These tests call attemptCreateVM directly to exercise the three code paths:
//   (a) local storage + replica found  → clone fired with replica VMID.
//   (b) local storage + not found + replicate disabled → Cloud error, no clone.
//   (c) shared storage                 → guard skipped, clone with primary VMID.
//
// "template:6042" is the stemcell CID (IsTemplateStemcellCID true).
// rawCID is set to a valid old-form CID carrying sha8 "abcd1234" so that
// extractSHA8FromTemplateCIDContext returns ("abcd1234", true) and the guard
// fires. This models the operator scenario described in the code comment for
// extractSHA8FromTemplateCIDContext.

// templateGapClusterSvc is a minimal cluster.Service for template-gap tests.
// ListConfigNodes returns a single-node cluster so ValidateTemplateCloneStorage
// returns immediately (single-node → any storage accepted).
// All placement methods that would fire return empty/noop.
type templateGapClusterSvc struct {
	sdkcluster.Service // nil — panics on any non-overridden call
}

func (c *templateGapClusterSvc) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	raw, _ := json.Marshal(map[string]any{"node": "pve-vm"})
	resp := sdkcluster.ListConfigNodesResponse{raw}
	return &resp, nil
}
func (c *templateGapClusterSvc) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}
func (c *templateGapClusterSvc) ListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}
func (c *templateGapClusterSvc) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

var _ sdkcluster.Service = (*templateGapClusterSvc)(nil)

// templateGapClusterStorageSvc reports storage with configurable shared flag.
type templateGapClusterStorageSvc struct {
	shared bool
}

func (s *templateGapClusterStorageSvc) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	sharedInt := 0
	if s.shared {
		sharedInt = 1
	}
	raw, _ := json.Marshal(map[string]any{
		"storage": "local-lvm",
		"type":    "lvm",
		"shared":  sharedInt,
	})
	resp := sdkclusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}
func (s *templateGapClusterStorageSvc) CreateStorage(_ context.Context, _ *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("not needed")
}
func (s *templateGapClusterStorageSvc) DeleteStorage(_ context.Context, _ string) error {
	panic("not needed")
}
func (s *templateGapClusterStorageSvc) GetStorage(_ context.Context, _ string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("not needed")
}
func (s *templateGapClusterStorageSvc) UpdateStorage(_ context.Context, _ string, _ *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("not needed")
}

var _ sdkclusterstorage.Service = (*templateGapClusterStorageSvc)(nil)

// templateGapNodesSvc implements nodes.Service for the template-gap tests.
// listQemuFn controls what ResolveTemplateVMIDForNode sees.
// cloneCapture records (node, vmidStr) from any CreateQemuClone call; the
// test can set cloneErr to stop the clone early without wiring the full tail.
type templateGapNodesSvc struct {
	sdknodes.Service // nil — panics on non-overridden calls
	listQemuFn   func(ctx context.Context, node string) (*sdknodes.ListQemuResponse, error)
	cloneCapture *struct{ node, vmidStr string }
	cloneErr     error
}

func (n *templateGapNodesSvc) ListQemu(ctx context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if n.listQemuFn != nil {
		return n.listQemuFn(ctx, node)
	}
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

func (n *templateGapNodesSvc) CreateQemuClone(_ context.Context, node, vmidStr string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
	if n.cloneCapture != nil {
		n.cloneCapture.node = node
		n.cloneCapture.vmidStr = vmidStr
	}
	if n.cloneErr != nil {
		return nil, n.cloneErr
	}
	panic("templateGapNodesSvc.CreateQemuClone: not expected (set cloneErr to intercept)")
}

// templateGapPVE wires the pieces needed by attemptCreateVM for template-gap tests.
type templateGapPVE struct {
	nodes   *templateGapNodesSvc
	cluster *templateGapClusterSvc
	storage *templateGapClusterStorageSvc
}

func (p *templateGapPVE) QEMU() sdkqemu.Service         { panic("not needed") }
func (p *templateGapPVE) Nodes() sdknodes.Service       { return p.nodes }
func (p *templateGapPVE) Tasks() sdktasks.Service        { panic("not needed") }
func (p *templateGapPVE) Storage() sdkstorage.Service    { panic("not needed") }
func (p *templateGapPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *templateGapPVE) Cluster() sdkcluster.Service    { return p.cluster }
func (p *templateGapPVE) ClusterStorage() sdkclusterstorage.Service {
	return p.storage
}
func (p *templateGapPVE) Pools() pve.PoolService { return nil }

var _ pve.Client = (*templateGapPVE)(nil)

// buildTemplateGapArgs returns a (parsed, shape) pair for template-gap tests.
// stemcellCID is "template:6042". rawCID carries sha8 "abcd1234" so the guard fires.
// shape.node = vmNode, templateNode = "pve-tmpl" (cross-node).
func buildTemplateGapArgs(shared bool) (*createVMParsedArgs, *createVMShape) {
	parsed := &createVMParsedArgs{
		stemcellCID: "template:6042",
		// rawCID carries sha8 "abcd1234" → extractSHA8FromTemplateCIDContext returns it.
		rawCID:    "local-lvm:import/bosh-stemcell-ubuntu-jammy-1.0-abcd1234.qcow2",
		cloudProps: createVMCloudProps{},
		networks:   map[string]createVMNetworkSpec{},
	}
	vmStorageType := "lvm"
	if shared {
		vmStorageType = "nfs"
	}
	shape := &createVMShape{
		node:          "pve-vm",   // VM target node
		vmStorage:     "local-lvm",
		vmStorageType: vmStorageType,
		vmDiskFormat:  "raw",
		rootDiskGiB:   10,
		cores:         1,
		memMiB:        512,
		rangeStart:    100,
		maxAttempts:   3,
	}
	return parsed, shape
}

// listQemuWithReplica returns a ListQemuResponse containing a VM tagged with
// sha "abcd1234" and node "pve-vm", VMID 9099. Used to simulate a found replica.
func listQemuWithReplica(_ context.Context, _ string) (*sdknodes.ListQemuResponse, error) {
	tr := true
	vmid := int64(9099)
	tags := "bosh-stemcell-sha-abcd1234;bosh-stemcell-node-pve-vm"
	raw, _ := json.Marshal(map[string]any{
		"vmid":     vmid,
		"template": tr,
		"tags":     tags,
		"name":     "replica-stemcell",
		"status":   "stopped",
	})
	resp := sdknodes.ListQemuResponse{json.RawMessage(raw)}
	return &resp, nil
}

// TestAttemptCreateVM_TemplateGap_ReplicaFound verifies that when local storage
// is in use and a per-node replica exists, attemptCreateVM clones from the
// replica VMID (9099) on the VM's node, not the primary VMID (6042).
//
// cloneErr is a VMID-conflict sentinel so handleCloneError skips cleanupVM
// (which would need QEMU wired). The capture fields prove the correct VMID/node.
func TestAttemptCreateVM_TemplateGap_ReplicaFound(t *testing.T) {
	t.Parallel()

	capture := &struct{ node, vmidStr string }{}
	// Use a VMID-conflict error: handleCloneError matches IsVMIDConflict → skips cleanupVM.
	cloneErr := fmt.Errorf("VM 200 already exists on node 'pve-vm'")
	ns := &templateGapNodesSvc{
		listQemuFn:   listQemuWithReplica,
		cloneCapture: capture,
		cloneErr:     cloneErr,
	}
	pveCli := &templateGapPVE{
		nodes:   ns,
		cluster: &templateGapClusterSvc{},
		storage: &templateGapClusterStorageSvc{shared: false},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                 "",
			StemcellTemplateNode: "pve-tmpl", // cross-node: template on pve-tmpl, VM on pve-vm
			VMStorage:            "local-lvm",
			NetworkBridge:        "vmbr0",
		},
		PVE:    pveCli,
		Logger: log.NewNopLogger(),
	}

	parsed, shape := buildTemplateGapArgs(false)
	err := attemptCreateVM(context.Background(), deps, log.NewNopLogger(), parsed, shape, 200)

	// Error must contain the clone sentinel.
	if err == nil {
		t.Fatal("expected clone sentinel error; got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected VMID-conflict sentinel; got: %v", err)
	}

	// Clone must have been called with the replica node and VMID, not the primary.
	if capture.node != "pve-vm" {
		t.Errorf("clone node: want %q, got %q", "pve-vm", capture.node)
	}
	if capture.vmidStr != "9099" {
		t.Errorf("clone vmidStr: want %q (replica), got %q (primary=6042 means guard missed)", "9099", capture.vmidStr)
	}
}

// TestAttemptCreateVM_TemplateGap_NotFound_ReplicateDisabled verifies that when
// local storage is in use, no replica exists, and StemcellReplicateLocal is false,
// attemptCreateVM returns a clear Cloud error naming the node and storage without
// calling CreateQemuClone.
func TestAttemptCreateVM_TemplateGap_NotFound_ReplicateDisabled(t *testing.T) {
	t.Parallel()

	cloneCalled := false
	ns := &templateGapNodesSvc{
		listQemuFn: func(_ context.Context, _ string) (*sdknodes.ListQemuResponse, error) {
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil // no replica found
		},
		cloneCapture: nil,
		cloneErr:     fmt.Errorf("clone-must-not-be-called"),
	}
	// Override CreateQemuClone via cloneCapture sentinel: if clone is called the
	// test panics via the "not expected" path (cloneErr is set but that fires only
	// if capture is set; here we detect the call via cloneErr firing).
	_ = cloneCalled // checked via absence of error containing "clone-must-not-be-called"

	pveCli := &templateGapPVE{
		nodes:   ns,
		cluster: &templateGapClusterSvc{},
		storage: &templateGapClusterStorageSvc{shared: false},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                  "",
			StemcellTemplateNode:  "pve-tmpl",
			VMStorage:             "local-lvm",
			NetworkBridge:         "vmbr0",
			StemcellReplicateLocal: false,
		},
		PVE:    pveCli,
		Logger: log.NewNopLogger(),
	}

	parsed, shape := buildTemplateGapArgs(false)
	err := attemptCreateVM(context.Background(), deps, log.NewNopLogger(), parsed, shape, 201)

	if err == nil {
		t.Fatal("expected Cloud error for missing replica + replication disabled; got nil")
	}
	// Error must NOT be our clone sentinel (clone was not called).
	if strings.Contains(err.Error(), "clone-must-not-be-called") {
		t.Errorf("CreateQemuClone must not be called when no replica and replication disabled; got: %v", err)
	}
	// Error must mention the node and storage.
	if !strings.Contains(err.Error(), "pve-vm") {
		t.Errorf("error must name the target node; got: %v", err)
	}
	if !strings.Contains(err.Error(), "local-lvm") {
		t.Errorf("error must name the storage; got: %v", err)
	}
	// Must not be retriable — this is a configuration gap, not a transient failure.
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Errorf("missing-replica error must not be retriable; got OkToRetry=true")
	}
}

// TestAttemptCreateVM_TemplateGap_SharedStorage_NoGuard verifies that when
// storage is shared, needsReplicaCheck returns false and the guard is skipped:
// ListQemu is never called, and clone fires with the primary VMID (6042).
//
// cloneErr is a VMID-conflict sentinel so handleCloneError skips cleanupVM.
func TestAttemptCreateVM_TemplateGap_SharedStorage_NoGuard(t *testing.T) {
	t.Parallel()

	listQemuCalled := false
	capture := &struct{ node, vmidStr string }{}
	// VMID-conflict sentinel: handleCloneError skips cleanupVM (no QEMU needed).
	cloneErr := fmt.Errorf("VM 202 already exists on node 'pve-vm'")
	ns := &templateGapNodesSvc{
		listQemuFn: func(_ context.Context, _ string) (*sdknodes.ListQemuResponse, error) {
			listQemuCalled = true
			empty := sdknodes.ListQemuResponse{}
			return &empty, nil
		},
		cloneCapture: capture,
		cloneErr:     cloneErr,
	}
	pveCli := &templateGapPVE{
		nodes:   ns,
		cluster: &templateGapClusterSvc{},
		storage: &templateGapClusterStorageSvc{shared: true}, // shared → guard skipped
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                 "",
			StemcellTemplateNode: "pve-tmpl",
			VMStorage:            "local-lvm",
			NetworkBridge:        "vmbr0",
		},
		PVE:    pveCli,
		Logger: log.NewNopLogger(),
	}

	parsed, shape := buildTemplateGapArgs(true)
	err := attemptCreateVM(context.Background(), deps, log.NewNopLogger(), parsed, shape, 202)

	if err == nil {
		t.Fatal("expected clone sentinel error; got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected VMID-conflict sentinel; got: %v", err)
	}
	if listQemuCalled {
		t.Error("ListQemu must not be called on shared storage (guard must be skipped)")
	}
	// Clone must fire with the primary VMID (6042), not a replica.
	if capture.vmidStr != "6042" {
		t.Errorf("clone vmidStr: want %q (primary), got %q", "6042", capture.vmidStr)
	}
}
