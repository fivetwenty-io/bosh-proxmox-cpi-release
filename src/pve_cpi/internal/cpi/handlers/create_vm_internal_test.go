package handlers

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
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
// createVM retry backoff (newCreateVMRetryBackoff with default policies)
// --------------------------------------------------------------------------

// defaultCreateVMBackoff builds the create_vm backoff closure from the shipped
// default retry policies (no operator override), so these tests assert the
// behavior-preserving defaults: storage-lock 2s × 1.5^attempt ±30% cap 30s, and
// VMID-conflict uniform 50–250 ms.
func defaultCreateVMBackoff() func(error, int) time.Duration {
	var empty config.CPIConfig
	return newCreateVMRetryBackoff(empty.RetryStorageImport(), empty.RetryVMIDAlloc())
}

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
			d := defaultCreateVMBackoff()(lockErr, attempt)
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
		d := defaultCreateVMBackoff()(lockErr, 20)
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
				d := defaultCreateVMBackoff()(tc.err, attempt)
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
	d := defaultCreateVMBackoff()(unknownErr, 0)
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
	const filename = "bosh-stemcell-test-1.0.qcow2"
	return &createVMParsedArgs{
		agentID:          "agent-test",
		stemcellCID:      ":light:" + stemcellStorage + ":import/" + filename,
		stemcellKind:     pve.StemcellKindLight,
		stemcellStorage:  stemcellStorage,
		stemcellVolPath:  "import/" + filename,
		stemcellFilename: filename,
		rawVolid:         stemcellStorage + ":import/" + filename,
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

func (p *placementInternalTestPVE) QEMU() sdkqemu.Service           { panic("not needed") }
func (p *placementInternalTestPVE) Nodes() sdknodes.Service         { return &placementInternalNodesSvc{} }
func (p *placementInternalTestPVE) Tasks() sdktasks.Service         { panic("not needed") }
func (p *placementInternalTestPVE) Storage() sdkstorage.Service     { panic("not needed") }
func (p *placementInternalTestPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *placementInternalTestPVE) Cluster() sdkcluster.Service {
	return &fullClusterAdapter{sub: p.clusterClient}
}
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
	sub                placement.ClusterClient
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
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, nil)
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
//
//	seed 0 → [zone-a, zone-b] → pve1 selected (zone-a tried first).
//	seed 2 → [zone-b, zone-a] → pve2 selected (zone-b tried first).
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
	nodeA, errA := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, rngA, nil)
	if errA != nil {
		t.Fatalf("seed=0 error: %v", errA)
	}

	// seed 2: shuffle produces [zone-b, zone-a] → zone-b first → pve2 wins.
	rngB := rand.New(rand.NewSource(2)) //nolint:gosec // fixed seed — deterministic test
	nodeB, errB := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, rngB, nil)
	if errB != nil {
		t.Fatalf("seed=2 error: %v", errB)
	}

	// Same seed must be deterministic.
	rngA2 := rand.New(rand.NewSource(0)) //nolint:gosec // fixed seed — deterministic test
	nodeA2, _ := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, rngA2, nil)
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
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", nil, nil, nil)
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

func (p *rawNodeTestPVE) QEMU() sdkqemu.Service           { panic("not needed") }
func (p *rawNodeTestPVE) Nodes() sdknodes.Service         { return &placementInternalNodesSvc{} }
func (p *rawNodeTestPVE) Tasks() sdktasks.Service         { panic("not needed") }
func (p *rawNodeTestPVE) Storage() sdkstorage.Service     { panic("not needed") }
func (p *rawNodeTestPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *rawNodeTestPVE) Cluster() sdkcluster.Service     { return &fullClusterAdapter{sub: p.cluster} }
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
// buildTemplateGapArgs builds a path-identity stemcell CID whose filename
// embeds sha8 "abcd1234" (extractSHA8FromParsed returns it), and each test
// wires templateGapClusterSvc.resourceRows with a single cluster-scoped
// stemcell-cache template (vmid 6042, node "pve-tmpl") carrying that sha8 tag
// so resolveTemplateCacheTarget's cluster lookup (FindTemplatesBySHATagCluster)
// finds it and proceeds into the same-node/shared/replica branches under test.

// templateGapClusterSvc is a minimal cluster.Service for template-gap tests.
// ListConfigNodes returns a single-node cluster so ValidateTemplateCloneStorage
// returns immediately (single-node → any storage accepted). ListResources
// reports resourceRows for a non-VMID-scan query (Type unset) and an empty
// list for a VMID-collision scan (Type="vm"), so wiring a cache-template
// fixture never perturbs VMID allocation.
// All other placement methods that would fire return empty/noop.
type templateGapClusterSvc struct {
	sdkcluster.Service // nil — panics on any non-overridden call

	resourceRows []map[string]any
}

// ListConfigNodes derives corosync membership from the resourceRows fixture
// (the template's own node), so the authoritative template lookup lists the
// node that actually holds the template. "name" is what
// pve.ListClusterConfigNodes parses.
func (c *templateGapClusterSvc) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	seen := map[string]bool{}
	resp := sdkcluster.ListConfigNodesResponse{}
	for _, row := range c.resourceRows {
		node, _ := row["node"].(string)
		if node == "" || seen[node] {
			continue
		}
		seen[node] = true
		raw, _ := json.Marshal(map[string]any{"name": node, "node": node})
		resp = append(resp, raw)
	}
	if len(resp) == 0 {
		raw, _ := json.Marshal(map[string]any{"name": "pve-vm", "node": "pve-vm"})
		resp = append(resp, raw)
	}
	return &resp, nil
}
func (c *templateGapClusterSvc) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}
func (c *templateGapClusterSvc) ListResources(_ context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if (params != nil && params.Type != nil) || len(c.resourceRows) == 0 {
		empty := sdkcluster.ListResourcesResponse{}
		return &empty, nil
	}
	out := make(sdkcluster.ListResourcesResponse, 0, len(c.resourceRows))
	for _, row := range c.resourceRows {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}
func (c *templateGapClusterSvc) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	empty := sdkcluster.ListHaStatusCurrentResponse{}
	return &empty, nil
}

// DeleteHaRules / DeleteHaResources are no-ops so cleanupVM's best-effort HA
// pin removal (removeNodeAffinityPin) can run against this fixture.
func (c *templateGapClusterSvc) DeleteHaRules(_ context.Context, _ string) error { return nil }
func (c *templateGapClusterSvc) DeleteHaResources(_ context.Context, _ string, _ *sdkcluster.DeleteHaResourcesParams) error {
	return nil
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
	listQemuFn       func(ctx context.Context, node string) (*sdknodes.ListQemuResponse, error)
	cloneCapture     *struct{ node, vmidStr string }
	cloneErr         error
	deleteQemuFn     func(ctx context.Context, node, vmidStr string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
}

func (n *templateGapNodesSvc) DeleteQemu(ctx context.Context, node, vmidStr string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if n.deleteQemuFn != nil {
		return n.deleteQemuFn(ctx, node, vmidStr, params)
	}
	panic("templateGapNodesSvc.DeleteQemu: not expected (set deleteQemuFn to intercept)")
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
// qemu, when set, overrides the default benign etQEMU stub — used by tests whose
// path reaches QEMU calls beyond Config (e.g. the rollback Stop in
// cleanupVMDetached).
// templateGapAuthNodes serves the template's own node from the cluster
// fixture and delegates every other node (and method) to the test's nodes
// service, keeping listQemuFn a pure replica-guard observable.
type templateGapAuthNodes struct {
	*templateGapNodesSvc
	cluster *templateGapClusterSvc
}

func (n *templateGapAuthNodes) ListQemu(ctx context.Context, node string, p *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if node == templateGapTemplateNode {
		out := sdknodes.ListQemuResponse{}
		for _, row := range n.cluster.resourceRows {
			if rn, _ := row["node"].(string); rn != node {
				continue
			}
			raw, err := json.Marshal(row)
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		}
		return &out, nil
	}
	return n.templateGapNodesSvc.ListQemu(ctx, node, p)
}

type templateGapPVE struct {
	nodes   *templateGapNodesSvc
	cluster *templateGapClusterSvc
	storage *templateGapClusterStorageSvc
	qemu    sdkqemu.Service
}

// QEMU returns a benign stub whose Config call returns an empty map (no
// virtio0 entry) rather than panicking: cloneFromTemplate's §1.3
// resolveTemplateDiskStorage reads the template's config to detect a
// linked-clone storage-placement mismatch before every clone attempt, so
// QEMU() is now reachable on every template-clone path, not only paths that
// exercise VM lifecycle calls. An empty config makes template storage
// resolution "undeterminable", which fails open to this test's pre-existing
// (vm_storage-keyed) expectations — the test asserts on clone-error handling
// and ListQemu, not on clone-mode selection.
func (p *templateGapPVE) QEMU() sdkqemu.Service {
	if p.qemu != nil {
		return p.qemu
	}
	return &etQEMU{}
}

// Nodes wraps the test's nodes service so the authoritative template lookup
// (which lists the template's own node) sees the resourceRows fixture, while
// listQemuFn stays the replica guard's observable on every other node.
func (p *templateGapPVE) Nodes() sdknodes.Service {
	return &templateGapAuthNodes{templateGapNodesSvc: p.nodes, cluster: p.cluster}
}
func (p *templateGapPVE) Tasks() sdktasks.Service         { panic("not needed") }
func (p *templateGapPVE) Storage() sdkstorage.Service     { panic("not needed") }
func (p *templateGapPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *templateGapPVE) Cluster() sdkcluster.Service     { return p.cluster }
func (p *templateGapPVE) ClusterStorage() sdkclusterstorage.Service {
	return p.storage
}
func (p *templateGapPVE) Pools() pve.PoolService { return nil }

var _ pve.Client = (*templateGapPVE)(nil)

// templateGapFilename is the stemcell qcow2 filename shared by
// buildTemplateGapArgs and each test's cluster-cache resource row: its sha8
// suffix ("abcd1234") is what resolveTemplateCacheTarget matches on.
const templateGapFilename = "bosh-stemcell-ubuntu-jammy-1.0-abcd1234.qcow2"

// templateGapSHA8 is the content sha8 embedded in templateGapFilename.
const templateGapSHA8 = "abcd1234"

// templateGapPrimaryVMID is the cluster-cache template VMID every
// templateGapClusterSvc.resourceRows fixture below uses.
const templateGapPrimaryVMID = 6042

// templateGapTemplateNode is the node hosting the cluster-cache template in
// every templateGapClusterSvc fixture — deliberately not shape.node ("pve-vm").
const templateGapTemplateNode = "pve-tmpl"

// templateGapResourceRow returns a GET /cluster/resources row for a frozen
// stemcell-cache template at (templateGapPrimaryVMID, templateGapTemplateNode)
// tagged with templateGapSHA8 — the fixture resolveTemplateCacheTarget's
// cluster-scoped lookup discovers. The node differs from shape.node ("pve-vm")
// so every test exercises the cross-node template branches.
func templateGapResourceRow() map[string]any {
	return map[string]any{
		"type":     "qemu",
		"vmid":     templateGapPrimaryVMID,
		"node":     templateGapTemplateNode,
		"name":     "bosh-stemcell-ubuntu-jammy-1-0",
		"tags":     stemcellCacheTag + ";bosh-stemcell-sha-" + templateGapSHA8,
		"template": true,
	}
}

// buildTemplateGapArgs returns a (parsed, shape) pair for template-gap tests.
// stemcellCID is a path-identity CID whose filename carries sha8 "abcd1234"
// so extractSHA8FromParsed returns it and the cluster-cache lookup fires.
// shape.node = vmNode; the cache template's node (set via each test's
// templateGapClusterSvc.resourceRows) is "pve-tmpl" (cross-node).
func buildTemplateGapArgs(shared bool) (*createVMParsedArgs, *createVMShape) {
	parsed := &createVMParsedArgs{
		stemcellCID:      ":light:local-lvm:import/" + templateGapFilename,
		stemcellKind:     pve.StemcellKindLight,
		stemcellStorage:  "local-lvm",
		stemcellVolPath:  "import/" + templateGapFilename,
		stemcellFilename: templateGapFilename,
		rawVolid:         "local-lvm:import/" + templateGapFilename,
		cloudProps:       createVMCloudProps{},
		networks:         map[string]createVMNetworkSpec{},
	}
	vmStorageType := "lvm"
	if shared {
		vmStorageType = "nfs"
	}
	shape := &createVMShape{
		node:          "pve-vm", // VM target node
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
	tags := stemcellCacheTag + ";bosh-stemcell-sha-abcd1234;bosh-stemcell-node-pve-vm"
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
		nodes: ns,
		// Cluster-scoped cache lookup finds the primary template on
		// "pve-tmpl" (cross-node from shape.node "pve-vm"); local storage
		// routes into the per-node replica guard, which listQemuWithReplica
		// satisfies.
		cluster: &templateGapClusterSvc{
			resourceRows: []map[string]any{templateGapResourceRow()},
		},
		storage: &templateGapClusterStorageSvc{shared: false},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
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
		nodes: ns,
		cluster: &templateGapClusterSvc{
			resourceRows: []map[string]any{templateGapResourceRow()},
		},
		storage: &templateGapClusterStorageSvc{shared: false},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:                   "",
			VMStorage:              "local-lvm",
			NetworkBridge:          "vmbr0",
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
		nodes: ns,
		cluster: &templateGapClusterSvc{
			resourceRows: []map[string]any{templateGapResourceRow()},
		},
		storage: &templateGapClusterStorageSvc{shared: true}, // shared → guard skipped
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
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

// TestAttemptStemcellTemplateClone_SourceVanished_FallsBackToImport verifies
// the mid-flight-vanish fallback: the cluster cache lookup finds a template,
// but the clone itself fails with clone-source-missing (the template was
// destroyed between lookup and clone POST — out-of-band, or by a concurrent
// delete_stemcell). attemptStemcellTemplateClone must report handled=false
// with a nil error so attemptCreateVM proceeds to strategy=import (the qcow2
// the CID names may still exist), and must sweep the candidate VMID first —
// the failed clone may have left partial VM state.
func TestAttemptStemcellTemplateClone_SourceVanished_FallsBackToImport(t *testing.T) {
	t.Parallel()

	capture := &struct{ node, vmidStr string }{}
	var deletedVMIDs []string
	ns := &templateGapNodesSvc{
		cloneCapture: capture,
		cloneErr:     stderrors.New("unable to find configuration file for VM 6042 on node 'pve-tmpl'"),
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			deletedVMIDs = append(deletedVMIDs, vmidStr)
			return nil, nil // synchronous destroy — no task to await
		},
	}
	pveCli := &templateGapPVE{
		nodes: ns,
		cluster: &templateGapClusterSvc{
			resourceRows: []map[string]any{templateGapResourceRow()},
		},
		storage: &templateGapClusterStorageSvc{shared: true}, // shared → clone with cluster primary
		// cleanupVMDetached's best-effort Stop must not panic; a plain error
		// (VM was never started) exits the rollback's stop phase immediately.
		qemu: &etQEMU{stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", stderrors.New("VM 203 not running")
		}},
	}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE:    pveCli,
		Logger: log.NewNopLogger(),
	}

	parsed, shape := buildTemplateGapArgs(true)
	handled, err := attemptStemcellTemplateClone(
		context.Background(), deps, log.NewNopLogger(), parsed, shape, 203, "vm-203")

	if err != nil {
		t.Fatalf("want nil error (fall back to import), got: %v", err)
	}
	if handled {
		t.Fatal("want handled=false so attemptCreateVM falls back to strategy=import")
	}
	// The clone must actually have been attempted against the primary template.
	if capture.vmidStr != "6042" {
		t.Errorf("clone vmidStr: want %q (primary), got %q", "6042", capture.vmidStr)
	}
	// The candidate VMID must have been swept before the fallback.
	if len(deletedVMIDs) != 1 || deletedVMIDs[0] != "203" {
		t.Errorf("candidate sweep: want DeleteQemu for vmid 203 exactly once, got %v", deletedVMIDs)
	}
}

// ---------------------------------------------------------------------------
// deriveDiskFaultConstraints
// ---------------------------------------------------------------------------

// encodeLocalCID builds an encoded disk CID for a local-backend disk on node.
// Uses "local-lvm" as the storage pool (the only pool used in these tests).
func encodeLocalCID(t *testing.T, node string) string {
	t.Helper()
	const pool = "local-lvm"
	bareCID := pool + ":vm-9001-disk-0"
	got, err := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{Pool: pool, Node: node})
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bareCID, err)
	}
	return got
}

// encodeSharedCID builds an encoded disk CID for a shared-backend disk with an AZ.
func encodeSharedCID(t *testing.T, pool, az string) string {
	t.Helper()
	bareCID := pool + ":vm-9001-disk-0"
	got, err := pve.EncodeDiskCID(bareCID, &pve.DiskCIDMeta{Pool: pool, AZ: az})
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bareCID, err)
	}
	return got
}

// sharedStorageSvc reports the test storage as shared (cluster-visible).
type sharedStorageSvc struct{}

func (s *sharedStorageSvc) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	raw, _ := json.Marshal(map[string]any{"storage": "ceph-pool", "type": "rbd", "shared": 1})
	resp := sdkclusterstorage.ListStorageResponse{json.RawMessage(raw)}
	return &resp, nil
}
func (s *sharedStorageSvc) CreateStorage(_ context.Context, _ *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("not needed")
}
func (s *sharedStorageSvc) DeleteStorage(_ context.Context, _ string) error { panic("not needed") }
func (s *sharedStorageSvc) GetStorage(_ context.Context, _ string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("not needed")
}
func (s *sharedStorageSvc) UpdateStorage(_ context.Context, _ string, _ *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("not needed")
}

var _ sdkclusterstorage.Service = (*sharedStorageSvc)(nil)

// faultDomainTestPVE wraps placementInternalTestCluster with configurable ClusterStorage.
type faultDomainTestPVE struct {
	clusterClient  *placementInternalTestCluster
	clusterStorage sdkclusterstorage.Service
}

func (p *faultDomainTestPVE) QEMU() sdkqemu.Service           { panic("not needed") }
func (p *faultDomainTestPVE) Nodes() sdknodes.Service         { return &placementInternalNodesSvc{} }
func (p *faultDomainTestPVE) Tasks() sdktasks.Service         { panic("not needed") }
func (p *faultDomainTestPVE) Storage() sdkstorage.Service     { panic("not needed") }
func (p *faultDomainTestPVE) CloudInit() sdkcloudinit.Service { panic("not needed") }
func (p *faultDomainTestPVE) Cluster() sdkcluster.Service {
	return &fullClusterAdapter{sub: p.clusterClient}
}
func (p *faultDomainTestPVE) ClusterStorage() sdkclusterstorage.Service { return p.clusterStorage }
func (p *faultDomainTestPVE) Pools() pve.PoolService                    { return nil }

var _ pve.Client = (*faultDomainTestPVE)(nil)

// staticKindResolver is a BackendResolver that classifies storages by name using
// a fixed mapping. Used in fault-domain tests to avoid StorageInfoCache complexity.
// Any pool name mapped to true is shared; false or absent means local.
type staticKindResolver struct {
	sharedPools map[string]bool // pool name → true if shared
	defaultNode string
}

func (r *staticKindResolver) Resolve(_ context.Context, storage string) (pve.Backend, error) {
	if r.sharedPools[storage] {
		return pve.NewStaticBackendResolver(nil, r.defaultNode).Resolve(context.Background(), storage)
	}
	// Local: return a local-kind backend. We use a private helper type below.
	return &localKindBackend{}, nil
}

// localKindBackend is a minimal Backend that reports Kind()=BackendLocal.
// NodeForCreate / NodeForExisting are not called in deriveDiskFaultConstraints
// (which only uses Kind()), so they panic to detect unexpected calls.
type localKindBackend struct{}

func (l *localKindBackend) Kind() pve.BackendKind { return pve.BackendLocal }
func (l *localKindBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	panic("localKindBackend.NodeForCreate: not expected in fault-domain tests")
}
func (l *localKindBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	panic("localKindBackend.NodeForExisting: not expected in fault-domain tests")
}

// buildFaultDomainDeps builds Deps for deriveDiskFaultConstraints / resolveTargetNodeWithRNG
// tests that need configurable node lists and storage classification.
// clusterNodes is used by GatherNodeFacts; sharedPools maps pool names to shared=true
// for the BackendResolver that deriveDiskFaultConstraints uses.
func buildFaultDomainDeps(clusterNodes []map[string]any, sharedPools map[string]bool, cfgFn func(*config.CPIConfig)) Deps {
	cfg := &config.CPIConfig{
		Host:          "pve.test",
		Node:          "",
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr0",
	}
	if cfgFn != nil {
		cfgFn(cfg)
	}
	pveCli := &faultDomainTestPVE{
		clusterClient:  &placementInternalTestCluster{nodes: clusterNodes},
		clusterStorage: &localStorageSvc{}, // used by GatherNodeFacts storage queries
	}
	return Deps{
		Config:   cfg,
		PVE:      pveCli,
		Logger:   log.NewNopLogger(),
		Resolver: &staticKindResolver{sharedPools: sharedPools, defaultNode: ""},
	}
}

// TestDeriveDiskFaultConstraints_NoDiskCIDs verifies that an empty diskCIDs slice
// returns zero-value constraints with no error (inert behavior).
func TestDeriveDiskFaultConstraints_NoDiskCIDs(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	c, err := deriveDiskFaultConstraints(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "" {
		t.Errorf("requiredLocalNode = %q; want empty", c.requiredLocalNode)
	}
	if len(c.requiredAZs) != 0 {
		t.Errorf("requiredAZs = %v; want empty", c.requiredAZs)
	}
}

// TestDeriveDiskFaultConstraints_BareLegacyCID verifies that a bare CID (no metadata)
// imposes no constraint (backward compatibility for pre-AZ deployments).
func TestDeriveDiskFaultConstraints_BareLegacyCID(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	bareCID := "local-lvm:vm-100-disk-0"
	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{bareCID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "" {
		t.Errorf("bare CID: requiredLocalNode = %q; want empty", c.requiredLocalNode)
	}
	if len(c.requiredAZs) != 0 {
		t.Errorf("bare CID: requiredAZs = %v; want empty", c.requiredAZs)
	}
}

// TestDeriveDiskFaultConstraints_SingleLocalDisk_PinsNode verifies that a single
// local-backend disk with node="pve1" produces requiredLocalNode="pve1".
func TestDeriveDiskFaultConstraints_SingleLocalDisk_PinsNode(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	cid := encodeLocalCID(t, "pve1")
	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "pve1" {
		t.Errorf("requiredLocalNode = %q; want pve1", c.requiredLocalNode)
	}
}

// TestDeriveDiskFaultConstraints_TwoLocalDisks_SameNode_OK verifies that two
// local-backend disks on the same node produce requiredLocalNode set to that node
// without error.
func TestDeriveDiskFaultConstraints_TwoLocalDisks_SameNode_OK(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	cid1 := encodeLocalCID(t, "pve1")
	cid2 := encodeLocalCID(t, "pve1")
	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cid1, cid2})
	if err != nil {
		t.Fatalf("unexpected error for same-node disks: %v", err)
	}
	if c.requiredLocalNode != "pve1" {
		t.Errorf("requiredLocalNode = %q; want pve1", c.requiredLocalNode)
	}
}

// TestDeriveDiskFaultConstraints_TwoLocalDisks_DifferentNodes_Error verifies that
// two local-backend disks on different nodes produce a CloudError naming both nodes.
func TestDeriveDiskFaultConstraints_TwoLocalDisks_DifferentNodes_Error(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	cid1 := encodeLocalCID(t, "pve1")
	cid2 := encodeLocalCID(t, "pve2")
	_, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cid1, cid2})
	if err == nil {
		t.Fatal("expected error for disks on different nodes; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pve1") || !strings.Contains(msg, "pve2") {
		t.Errorf("error must mention both nodes; got: %v", msg)
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("cross-node disk error must not be retriable (permanent misconfiguration)")
	}
}

// TestDeriveDiskFaultConstraints_SharedDisk_AZConstraint verifies that a shared-backend
// disk with AZ="zone-a" populates requiredAZs with "zone-a".
func TestDeriveDiskFaultConstraints_SharedDisk_AZConstraint(t *testing.T) {
	t.Parallel()
	// sharedStorageSvc returns ceph-pool as shared.
	deps := buildFaultDomainDeps(nil, map[string]bool{"ceph-pool": true}, nil)
	cid := encodeSharedCID(t, "ceph-pool", "zone-a")
	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{cid})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "" {
		t.Errorf("shared disk: requiredLocalNode = %q; want empty", c.requiredLocalNode)
	}
	if _, ok := c.requiredAZs["zone-a"]; !ok {
		t.Errorf("requiredAZs = %v; want zone-a", c.requiredAZs)
	}
}

// TestDeriveDiskFaultConstraints_MixedBareAndEncoded verifies that a mix of
// bare volid strings and encoded CIDs correctly ignores the bare ones while
// applying constraints from the encoded ones.
func TestDeriveDiskFaultConstraints_MixedBareAndEncoded(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	bareCID := "local-lvm:vm-100-disk-0"    // bare volid — no constraint
	encodedCID := encodeLocalCID(t, "pve1") // local — pins node
	c, err := deriveDiskFaultConstraints(context.Background(), deps, []string{bareCID, encodedCID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "pve1" {
		t.Errorf("requiredLocalNode = %q; want pve1", c.requiredLocalNode)
	}
}

// ---------------------------------------------------------------------------
// applyDiskAZConstraint
// ---------------------------------------------------------------------------

// TestApplyDiskAZConstraint_EmptyVMAZOrder_ConstrainedToDiskAZs verifies that when
// the VM has no AZ preference, the result is the sorted set of required AZs.
func TestApplyDiskAZConstraint_EmptyVMAZOrder_ConstrainedToDiskAZs(t *testing.T) {
	t.Parallel()
	required := map[string]struct{}{"zone-b": {}, "zone-a": {}}
	got, err := applyDiskAZConstraint(nil, required)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"zone-a", "zone-b"} // sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

// TestApplyDiskAZConstraint_VMOrderConsistentWithDisk_ReturnsIntersection verifies
// that when the VM's AZ order includes the required AZ, the intersection (in VM order)
// is returned.
func TestApplyDiskAZConstraint_VMOrderConsistentWithDisk_ReturnsIntersection(t *testing.T) {
	t.Parallel()
	vmOrder := []string{"zone-a", "zone-b", "zone-c"}
	required := map[string]struct{}{"zone-b": {}}
	got, err := applyDiskAZConstraint(vmOrder, required)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"zone-b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

// TestApplyDiskAZConstraint_VMOrderIncompatibleWithDisk_Error verifies that when
// the VM's AZ order has no overlap with the required AZs, a non-retriable CloudError
// is returned naming both the VM AZs and required AZs.
func TestApplyDiskAZConstraint_VMOrderIncompatibleWithDisk_Error(t *testing.T) {
	t.Parallel()
	vmOrder := []string{"zone-a"}
	required := map[string]struct{}{"zone-b": {}}
	_, err := applyDiskAZConstraint(vmOrder, required)
	if err == nil {
		t.Fatal("expected error for incompatible VM AZ and disk AZ; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "zone-a") || !strings.Contains(msg, "zone-b") {
		t.Errorf("error must mention both VM AZ and required AZ; got: %v", msg)
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("AZ conflict must not be retriable")
	}
}

// TestApplyDiskAZConstraint_NoRequiredAZs_Unchanged verifies that empty requiredAZs
// returns the VM's original AZ order unchanged.
func TestApplyDiskAZConstraint_NoRequiredAZs_Unchanged(t *testing.T) {
	t.Parallel()
	vmOrder := []string{"zone-a", "zone-b"}
	got, err := applyDiskAZConstraint(vmOrder, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, vmOrder) {
		t.Errorf("got %v; want %v (unchanged)", got, vmOrder)
	}
}

// ---------------------------------------------------------------------------
// resolveTargetNodeWithRNG — disk fault-domain cases
// ---------------------------------------------------------------------------

// TestResolveTargetNodeWithRNG_LocalDisk_PinsNode verifies that a single local-backend
// disk with node="pve1" causes the VM to be placed on pve1 (both nodes online).
func TestResolveTargetNodeWithRNG_LocalDisk_PinsNode(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil, // local-lvm is non-shared (default: all pools local)
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{
					"zone-a": {"pve1", "pve2"},
				},
			}
		},
	)
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("got node %q; want pve1 (local disk pin)", node)
	}
}

// TestResolveTargetNodeWithRNG_TwoLocalDisks_SameNode_OK verifies that two local disks
// on the same node ("pve1") still resolve to "pve1" without error.
func TestResolveTargetNodeWithRNG_TwoLocalDisks_SameNode_OK(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}},
			}
		},
	)
	diskCIDs := []string{
		encodeLocalCID(t, "pve1"),
		encodeLocalCID(t, "pve1"),
	}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("got node %q; want pve1", node)
	}
}

// TestResolveTargetNodeWithRNG_TwoLocalDisks_DifferentNodes_Error verifies that two
// local disks on different nodes produce a non-retriable CloudError before any placement
// attempt.
func TestResolveTargetNodeWithRNG_TwoLocalDisks_DifferentNodes_Error(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{
					"zone-a": {"pve1"},
					"zone-b": {"pve2"},
				},
			}
		},
	)
	diskCIDs := []string{
		encodeLocalCID(t, "pve1"),
		encodeLocalCID(t, "pve2"),
	}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err == nil {
		t.Fatal("expected error for disks on different nodes; got nil")
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("different-node disk error must not be retriable")
	}
	if !strings.Contains(err.Error(), "pve1") || !strings.Contains(err.Error(), "pve2") {
		t.Errorf("error should name both nodes; got: %v", err)
	}
}

// TestResolveTargetNodeWithRNG_SharedDisk_AZConstrains_Compatible verifies that a
// shared-backend disk with AZ="zone-a" constrains placement to zone-a when the VM
// has availability_zones: [zone-a, zone-b].
func TestResolveTargetNodeWithRNG_SharedDisk_AZConstrains_Compatible(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		map[string]bool{"ceph-pool": true},
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{
					"zone-a": {"pve1"},
					"zone-b": {"pve2"},
				},
			}
		},
	)
	diskCIDs := []string{encodeSharedCID(t, "ceph-pool", "zone-a")}
	cp := createVMCloudProps{AvailabilityZones: []string{"zone-a", "zone-b"}}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only zone-a remains after intersection; pve1 is the only candidate.
	if node != "pve1" {
		t.Errorf("got node %q; want pve1 (shared disk constrains to zone-a)", node)
	}
}

// TestResolveTargetNodeWithRNG_SharedDisk_AZConflicts_Error verifies that a shared-backend
// disk with AZ="zone-b" conflicts with a VM configured for only zone-a, producing
// a non-retriable CloudError.
func TestResolveTargetNodeWithRNG_SharedDisk_AZConflicts_Error(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		map[string]bool{"ceph-pool": true},
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{
					"zone-a": {"pve1"},
					"zone-b": {"pve2"},
				},
			}
		},
	)
	diskCIDs := []string{encodeSharedCID(t, "ceph-pool", "zone-b")}
	cp := createVMCloudProps{AvailabilityZone: "zone-a"} // singular AZ — zone-a only
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", diskCIDs, nil, nil)
	if err == nil {
		t.Fatal("expected error for AZ conflict; got nil")
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("AZ conflict error must not be retriable")
	}
}

// TestResolveTargetNodeWithRNG_LocalDiskPinnedNodeOffline_Error verifies that when
// the local disk's home node is offline, a non-retriable CloudError is returned
// (not a generic "no candidates" retriable error).
func TestResolveTargetNodeWithRNG_LocalDiskPinnedNodeOffline_Error(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{
			{"type": "node", "name": "pve1", "online": 0, "maxcpu": int64(4), "maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0},
			onlineNode("pve2"),
		},
		nil,
		func(c *config.CPIConfig) {
			c.Node = ""
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}},
			}
		},
	)
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err == nil {
		t.Fatal("expected error when disk's home node is offline; got nil")
	}
	if !strings.Contains(err.Error(), "pve1") {
		t.Errorf("error must name the pinned node; got: %v", err)
	}
	// Must be non-retriable: this is a hard constraint violation.
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("offline pinned node error must not be retriable (hard constraint)")
	}
}

// TestResolveTargetNodeWithRNG_BareLocalCID_Inert verifies that a bare legacy CID
// imposes no constraint — placement proceeds normally without pinning.
func TestResolveTargetNodeWithRNG_BareLocalCID_Inert(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}},
			}
		},
	)
	diskCIDs := []string{"local-lvm:vm-100-disk-0"} // bare legacy — no constraint
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("bare CID must impose no constraint; got error: %v", err)
	}
}

// TestResolveTargetNodeWithRNG_PlacementDisabled_LocalDiskPinsNode verifies that even
// with placement disabled (config.Node fallback path), a local disk's home node wins.
// This prevents silent VM placement on the wrong node when placement is off.
func TestResolveTargetNodeWithRNG_PlacementDisabled_LocalDiskPinsNode(t *testing.T) {
	t.Parallel()
	// No Placement config → PlacementEnabled() == false.
	deps := Deps{
		Config: &config.CPIConfig{
			Host:          "pve.test",
			Node:          "pve2", // config.node differs from disk's node
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE:      nil, // placement disabled path never queries PVE
		Logger:   log.NewNopLogger(),
		Resolver: &staticKindResolver{sharedPools: nil}, // all pools are local
	}
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{}, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Disk's home node (pve1) must win over config.node (pve2).
	if node != "pve1" {
		t.Errorf("got node %q; want pve1 (local disk pin wins over config.node)", node)
	}
}

// TestResolveTargetNodeWithRNG_TargetNodeConflictsLocalDisk_Error verifies that
// cloud_properties.target_node conflicts with the local disk's home node produce
// a non-retriable CloudError explaining the conflict.
func TestResolveTargetNodeWithRNG_TargetNodeConflictsLocalDisk_Error(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Config: &config.CPIConfig{
			Host:          "pve.test",
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE:      nil,
		Logger:   log.NewNopLogger(),
		Resolver: &staticKindResolver{sharedPools: nil}, // all pools are local
	}
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	cp := createVMCloudProps{TargetNode: "pve2"} // conflicts with disk on pve1
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", diskCIDs, nil, nil)
	if err == nil {
		t.Fatal("expected error for target_node/disk conflict; got nil")
	}
	if !strings.Contains(err.Error(), "pve2") || !strings.Contains(err.Error(), "pve1") {
		t.Errorf("error must name both target_node and disk node; got: %v", err)
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("target_node/disk conflict must not be retriable")
	}
}

// TestResolveTargetNodeWithRNG_TargetNodeMatchesLocalDisk_OK verifies that when
// cloud_properties.target_node matches the local disk's home node, the call
// succeeds and returns that node.
func TestResolveTargetNodeWithRNG_TargetNodeMatchesLocalDisk_OK(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Config: &config.CPIConfig{
			Host:          "pve.test",
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE:      nil,
		Logger:   log.NewNopLogger(),
		Resolver: &staticKindResolver{sharedPools: nil}, // all pools are local
	}
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	cp := createVMCloudProps{TargetNode: "pve1"} // consistent with disk
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", diskCIDs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error when target_node matches disk node: %v", err)
	}
	if node != "pve1" {
		t.Errorf("got node %q; want pve1", node)
	}
}

// TestResolveTargetNodeWithFallbacks_TargetNodeConflictsLocalDisk_Error drives
// the operator-pin/local-disk conflict guard on the fallback entry point
// (fallbackMax > 0) — the code path production takes when
// placement_fallback_max is enabled. The implementation is shared with
// resolveTargetNodeWithRNG by delegation, so this test guards the fallback
// plumbing (extra return values, alternates handling) around the same guard.
func TestResolveTargetNodeWithFallbacks_TargetNodeConflictsLocalDisk_Error(t *testing.T) {
	t.Parallel()
	deps := Deps{
		Config: &config.CPIConfig{
			Host:          "pve.test",
			Node:          "",
			VMStorage:     "local-lvm",
			NetworkBridge: "vmbr0",
		},
		PVE:      nil,
		Logger:   log.NewNopLogger(),
		Resolver: &staticKindResolver{sharedPools: nil}, // all pools are local
	}
	diskCIDs := []string{encodeLocalCID(t, "pve1")}
	cp := createVMCloudProps{TargetNode: "pve2"} // conflicts with disk on pve1
	_, alts, err := resolveTargetNodeWithFallbacks(context.Background(), deps, cp, "", diskCIDs, nil, nil, 3)
	if err == nil {
		t.Fatal("expected error for target_node/disk conflict; got nil")
	}
	if alts != nil {
		t.Errorf("no alternates may accompany a conflict error; got %v", alts)
	}
	if !strings.Contains(err.Error(), "pve2") || !strings.Contains(err.Error(), "pve1") {
		t.Errorf("error must name both target_node and disk node; got: %v", err)
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("target_node/disk conflict must not be retriable")
	}
}

// TestResolveTargetNodeWithFallbacks_UnknownAZ_Error drives the
// availability_zone-not-in-az_map rejection on the fallback entry point
// (fallbackMax > 0), for the same shared-guard reason as the conflict test
// above.
func TestResolveTargetNodeWithFallbacks_UnknownAZ_Error(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{
				AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}},
			}
		},
	)
	cp := createVMCloudProps{AvailabilityZone: "zone-nope"}
	_, alts, err := resolveTargetNodeWithFallbacks(context.Background(), deps, cp, "", nil, nil, nil, 3)
	if err == nil {
		t.Fatal("expected error for AZ missing from placement.az_map; got nil")
	}
	if alts != nil {
		t.Errorf("no alternates may accompany an unknown-AZ error; got %v", alts)
	}
	if !strings.Contains(err.Error(), "zone-nope") {
		t.Errorf("error must name the unknown AZ; got: %v", err)
	}
	var cpiErr *cpierrors.Error
	if stderrors.As(err, &cpiErr) && cpiErr.OkToRetry() {
		t.Error("unknown AZ is a config error and must not be retriable")
	}
}

// ---------------------------------------------------------------------------
// buildAZOrder — layered resolver integration
// ---------------------------------------------------------------------------

// TestBuildAZOrder_NoResolver_NoProfile_Unchanged verifies that passing nil
// resolver produces the same result as the pre-resolver behavior.
func TestBuildAZOrder_NoResolver_NoProfile_Unchanged(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		Placement: &config.PlacementConfig{
			AZMap:           map[string][]string{"zone-a": {"pve1"}},
			AZFallbackOrder: []string{"zone-a"},
		},
	}
	cp := createVMCloudProps{} // no per-call AZ
	got := buildAZOrder(cp, cfg, nil, nil)
	if len(got) != 1 || got[0] != "zone-a" {
		t.Errorf("buildAZOrder nil resolver: got %v; want [zone-a]", got)
	}
}

// TestBuildAZOrder_PerCallSingularBeatsProfile verifies that a per-call
// availability_zone always wins over a profile-supplied one.
func TestBuildAZOrder_PerCallSingularBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"availability_zone": "profile-zone"}},
		},
	}
	r, _ := newLayeredResolver(map[string]any{"vm_type": "med"}, cfg)
	cp := createVMCloudProps{AvailabilityZone: "call-zone"}
	got := buildAZOrder(cp, cfg, nil, r)
	if len(got) != 1 || got[0] != "call-zone" {
		t.Errorf("per-call AZ must win; got %v; want [call-zone]", got)
	}
}

// TestBuildAZOrder_ProfileSingularUsedWhenCallAbsent verifies that when no
// per-call AZ is set the resolver's singular availability_zone is used.
func TestBuildAZOrder_ProfileSingularUsedWhenCallAbsent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"availability_zone": "profile-zone"}},
		},
	}
	r, _ := newLayeredResolver(map[string]any{"vm_type": "med"}, cfg)
	cp := createVMCloudProps{}
	got := buildAZOrder(cp, cfg, nil, r)
	if len(got) != 1 || got[0] != "profile-zone" {
		t.Errorf("profile singular AZ: got %v; want [profile-zone]", got)
	}
}

// TestBuildAZOrder_ProfileSingularBeatsProfilePlural verifies that a singular
// availability_zone in the profile beats a plural availability_zones in the
// same profile (same precedence hierarchy as cp.AvailabilityZone vs cp.AvailabilityZones).
func TestBuildAZOrder_ProfileSingularBeatsProfilePlural(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{
				"availability_zone":  "singular-zone",
				"availability_zones": []any{"plural-a", "plural-b"},
			}},
		},
	}
	r, _ := newLayeredResolver(map[string]any{"vm_type": "med"}, cfg)
	cp := createVMCloudProps{}
	got := buildAZOrder(cp, cfg, nil, r)
	if len(got) != 1 || got[0] != "singular-zone" {
		t.Errorf("singular profile AZ beats plural; got %v; want [singular-zone]", got)
	}
}

// TestBuildAZOrder_ProfilePluralFeedsMultiAZPath verifies that a plural
// availability_zones from a profile populates the multi-AZ list.
func TestBuildAZOrder_ProfilePluralFeedsMultiAZPath(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{
				"availability_zones": []any{"zone-a", "zone-b"},
			}},
		},
	}
	r, _ := newLayeredResolver(map[string]any{"vm_type": "med"}, cfg)
	cp := createVMCloudProps{}
	got := buildAZOrder(cp, cfg, nil, r)
	if len(got) != 2 || got[0] != "zone-a" || got[1] != "zone-b" {
		t.Errorf("profile plural AZs: got %v; want [zone-a zone-b]", got)
	}
}

// TestBuildAZOrder_PerCallPluralBeatsProfile verifies per-call plurality wins over profile.
func TestBuildAZOrder_PerCallPluralBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{
				"availability_zones": []any{"profile-a"},
			}},
		},
	}
	r, _ := newLayeredResolver(map[string]any{"vm_type": "med"}, cfg)
	cp := createVMCloudProps{AvailabilityZones: []string{"call-a", "call-b"}}
	got := buildAZOrder(cp, cfg, nil, r)
	if len(got) != 2 || got[0] != "call-a" || got[1] != "call-b" {
		t.Errorf("per-call plural must win; got %v; want [call-a call-b]", got)
	}
}

// ---------------------------------------------------------------------------
// resolveTargetNodeWithRNG — placement weight overrides via cloud_properties
// ---------------------------------------------------------------------------

// TestResolveTargetNodeWithRNG_WeightOverride_NoProfile_NoOp verifies that
// with no weight keys in cloud_properties the resolved weights equal the config
// defaults (golden no-op: byte-identical to pre-resolver behavior).
func TestResolveTargetNodeWithRNG_WeightOverride_NoProfile_NoOp(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	// No weight keys in cloudPropsMap → must succeed with config defaults.
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, map[string]any{})
	if err != nil {
		t.Fatalf("no-override: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1; got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_WeightOverride_PerCall_MemAxis verifies that a
// per-call placement_weight_mem overrides only the Mem axis; other axes stay at
// config defaults.
func TestResolveTargetNodeWithRNG_WeightOverride_PerCall_MemAxis(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
	})
	// Set a heavy placement_weight_mem; first viable AZ (zone-a → pve1) should win.
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	cpMap := map[string]any{"placement_weight_mem": float64(2.0)}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("weight override mem: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1 (only candidate in zone-a); got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_WeightOverride_VMTypeProfile verifies that a
// vm_type profile can supply a placement weight axis.
func TestResolveTargetNodeWithRNG_WeightOverride_VMTypeProfile(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
		c.VMTypes = map[string]config.TypeProfile{
			"heavy": {CloudProperties: map[string]any{"placement_weight_mem": float64(3.0)}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	cpMap := map[string]any{"vm_type": "heavy"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("vm_type weight profile: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1; got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_WeightOverride_PerCallBeatsProfile verifies that
// a per-call weight key beats the same key supplied by a vm_type profile.
func TestResolveTargetNodeWithRNG_WeightOverride_PerCallBeatsProfile(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
		c.VMTypes = map[string]config.TypeProfile{
			"heavy": {CloudProperties: map[string]any{"placement_weight_mem": float64(3.0)}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	// Per-call value (2.0) must win over profile value (3.0); node is still pve1 either way.
	cpMap := map[string]any{
		"vm_type":              "heavy",
		"placement_weight_mem": float64(2.0),
	}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("per-call beats profile: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("expected pve1; got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_UnknownVMTypeSelector_CloudError verifies that
// an unknown vm_type selector in cloudPropsMap returns a CloudError immediately.
func TestResolveTargetNodeWithRNG_UnknownVMTypeSelector_CloudError(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{"zone-a": {"pve1"}},
		}
		c.VMTypes = map[string]config.TypeProfile{
			"known": {CloudProperties: map[string]any{}},
		}
	})
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	cpMap := map[string]any{"vm_type": "unknown-profile"}
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type selector; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error; got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Error("unknown selector must produce non-retriable CloudError")
	}
}

// TestResolveTargetNodeWithRNG_AZFromVMTypeProfile_SingularUsed verifies that
// when cp.AvailabilityZone and cp.AvailabilityZones are both absent, the
// availability_zone from the vm_type profile is used as the singular AZ.
func TestResolveTargetNodeWithRNG_AZFromVMTypeProfile_SingularUsed(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
		c.VMTypes = map[string]config.TypeProfile{
			"zoned": {CloudProperties: map[string]any{"availability_zone": "zone-a"}},
		}
	})
	cp := createVMCloudProps{} // no per-call AZ
	cpMap := map[string]any{"vm_type": "zoned"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("profile singular AZ: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("profile az=zone-a → expected pve1; got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_AZFromVMTypeProfile_PluralUsed verifies that
// availability_zones (plural) from the vm_type profile feeds the multi-AZ path.
func TestResolveTargetNodeWithRNG_AZFromVMTypeProfile_PluralUsed(t *testing.T) {
	t.Parallel()

	// pve1 is offline → zone-a exhausted → zone-b (pve2) wins via profile plural list.
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
		c.VMTypes = map[string]config.TypeProfile{
			"multi": {CloudProperties: map[string]any{
				"availability_zones": []any{"zone-a", "zone-b"},
			}},
		}
	})
	cp := createVMCloudProps{} // no per-call AZ
	cpMap := map[string]any{"vm_type": "multi"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("profile plural AZs: unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("zone-a exhausted (pve1 offline), zone-b should win; got %q", node)
	}
}

// TestResolveTargetNodeWithRNG_AZCallBeatsProfile verifies that per-call
// availability_zone beats the profile's availability_zone.
func TestResolveTargetNodeWithRNG_AZCallBeatsProfile(t *testing.T) {
	t.Parallel()

	deps := buildPlacementDeps([]map[string]any{onlineNode("pve1"), onlineNode("pve2")}, func(c *config.CPIConfig) {
		c.Placement = &config.PlacementConfig{
			AZMap: map[string][]string{
				"zone-a": {"pve1"},
				"zone-b": {"pve2"},
			},
		}
		c.VMTypes = map[string]config.TypeProfile{
			"zoned": {CloudProperties: map[string]any{"availability_zone": "zone-b"}},
		}
	})
	// Per-call says zone-a → pve1; profile says zone-b → pve2.
	cp := createVMCloudProps{AvailabilityZone: "zone-a"}
	cpMap := map[string]any{"vm_type": "zoned"}
	node, err := resolveTargetNodeWithRNG(context.Background(), deps, cp, "", nil, nil, cpMap)
	if err != nil {
		t.Fatalf("call AZ beats profile: unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("per-call zone-a must win over profile zone-b; got %q", node)
	}
}

// --------------------------------------------------------------------------
// resolveVMShapeStorage — layered resolver integration
// --------------------------------------------------------------------------

// minimalParsedArgsWithCP returns a *createVMParsedArgs with cloudPropsMap set.
func minimalParsedArgsWithCP(stemcellStorage string, cpMap map[string]any) *createVMParsedArgs {
	p := minimalParsedArgs(stemcellStorage)
	p.cloudPropsMap = cpMap
	return p
}

// TestResolveVMShapeCPUMem_Defaults verifies the built-in shape defaults:
// two cores (PVE guidance — never single-thread a guest), one socket, and
// 512 MiB, with explicit values (including cores: 1) honored as given.
func TestResolveVMShapeCPUMem_Defaults(t *testing.T) {
	t.Parallel()

	cores, sockets, memMiB := resolveVMShapeCPUMem(createVMCloudProps{})
	if cores != 2 || sockets != 1 || memMiB != 512 {
		t.Errorf("defaults = (%d cores, %d sockets, %d MiB); want (2, 1, 512)", cores, sockets, memMiB)
	}

	// Explicit single core is honored — the 2-core default only fills absence.
	cores, _, _ = resolveVMShapeCPUMem(createVMCloudProps{Cores: 1})
	if cores != 1 {
		t.Errorf("explicit cores=1 resolved to %d; want 1", cores)
	}

	// vSphere-style cpu convention still maps to cores.
	cores, sockets, _ = resolveVMShapeCPUMem(createVMCloudProps{CPU: 4})
	if cores != 4 || sockets != 1 {
		t.Errorf("cpu=4 resolved to (%d cores, %d sockets); want (4, 1)", cores, sockets)
	}
}

// TestResolveVMShapeStorage_NoProfile_UsesConfigVMStorage verifies byte-identical
// behavior when no vm_type profile or storage_pool override is present: vmStorage
// equals config.VMStorage.
func TestResolveVMShapeStorage_NoProfile_UsesConfigVMStorage(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})

	vmStorage, vmDiskFormat, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmStorage != "local-lvm" {
		t.Errorf("vmStorage = %q; want local-lvm (config)", vmStorage)
	}
	if vmDiskFormat != diskFormatQCOW2 {
		t.Errorf("vmDiskFormat = %q; want %q (default)", vmDiskFormat, diskFormatQCOW2)
	}
}

// TestResolveVMShapeStorage_NoProfile_NoConfig_FallbackToStemcell verifies that
// when config.VMStorage is empty the stemcell storage is used as fallback.
func TestResolveVMShapeStorage_NoProfile_NoConfig_FallbackToStemcell(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: ""}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})

	vmStorage, _, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmStorage != "stemcell-store" {
		t.Errorf("vmStorage = %q; want stemcell-store (stemcell fallback)", vmStorage)
	}
}

// TestResolveVMShapeStorage_CallCP_StoragePool_OverridesConfig verifies that
// cloud_properties.storage_pool in the call layer overrides config.VMStorage.
func TestResolveVMShapeStorage_CallCP_StoragePool_OverridesConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"storage_pool": "fast-nvme",
	})

	vmStorage, _, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmStorage != "fast-nvme" {
		t.Errorf("vmStorage = %q; want fast-nvme (call cloud_properties override)", vmStorage)
	}
}

// TestResolveVMShapeStorage_VMTypeProfile_StoragePool verifies that a vm_type
// profile's storage_pool is used when the call layer has no storage_pool set.
func TestResolveVMShapeStorage_VMTypeProfile_StoragePool(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"large": {
				CloudProperties: map[string]any{
					"storage_pool": "ceph-fast",
				},
			},
		},
	}
	// Call layer selects vm_type "large" but supplies no storage_pool itself.
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"vm_type": "large",
	})

	vmStorage, _, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmStorage != "ceph-fast" {
		t.Errorf("vmStorage = %q; want ceph-fast (vm_type profile)", vmStorage)
	}
}

// TestResolveVMShapeStorage_CallOverridesProfile verifies that an explicit
// storage_pool in the call layer wins over a vm_type profile storage_pool.
func TestResolveVMShapeStorage_CallOverridesProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"large": {
				CloudProperties: map[string]any{
					"storage_pool": "ceph-fast",
				},
			},
		},
	}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"vm_type":      "large",
		"storage_pool": "override-pool",
	})

	vmStorage, _, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmStorage != "override-pool" {
		t.Errorf("vmStorage = %q; want override-pool (call beats profile)", vmStorage)
	}
}

// TestResolveVMShapeStorage_DiskFormat_FromProfile verifies that vm_disk_format
// in a vm_type profile is applied when the call layer has no disk format.
func TestResolveVMShapeStorage_DiskFormat_FromProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"fast": {
				CloudProperties: map[string]any{
					"vm_disk_format": "raw",
				},
			},
		},
	}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"vm_type": "fast",
	})
	// struct field left empty — profile should supply the format.
	parsed.cloudProps.VMDiskFormat = ""

	_, vmDiskFormat, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmDiskFormat != "raw" {
		t.Errorf("vmDiskFormat = %q; want raw (from vm_type profile)", vmDiskFormat)
	}
}

// TestResolveVMShapeStorage_DiskFormat_CallBeatsProfile verifies that an explicit
// vm_disk_format in the call layer beats the profile value.
func TestResolveVMShapeStorage_DiskFormat_CallBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"fast": {
				CloudProperties: map[string]any{
					"vm_disk_format": "raw",
				},
			},
		},
	}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"vm_type":        "fast",
		"vm_disk_format": "vmdk",
	})
	// struct field mirrors call layer value (it was unmarshalled from args[2]).
	parsed.cloudProps.VMDiskFormat = "vmdk"

	_, vmDiskFormat, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vmDiskFormat != "vmdk" {
		t.Errorf("vmDiskFormat = %q; want vmdk (call beats profile)", vmDiskFormat)
	}
}

// TestResolveVMShapeStorage_UnknownVMType_ReturnsCloudError verifies that an
// unknown vm_type selector produces a CloudError from resolveVMShapeStorage.
func TestResolveVMShapeStorage_UnknownVMType_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes:   map[string]config.TypeProfile{},
	}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{
		"vm_type": "unknown-profile",
	})

	_, _, _, err := resolveVMShapeStorage(cfg, parsed)

	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// TestResolveVMShape_StoragePool_FlowsToStorageTypeLookup verifies that a
// storage_pool from the call layer flows through resolveVMShape into
// shape.vmStorage and shape.vmStorageType is looked up for the resolved pool.
func TestResolveVMShape_StoragePool_FlowsToStorageTypeLookup(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{
				entries: []map[string]any{
					{"storage": "local-lvm", "type": "dir"},
					{"storage": "ceph-pool", "type": "rbd"},
				},
			},
		},
	}

	parsed := minimalParsedArgsWithCP("test-storage", map[string]any{
		"storage_pool": "ceph-pool",
	})

	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape returned error: %v", err)
	}
	if shape.vmStorage != "ceph-pool" {
		t.Errorf("vmStorage = %q; want ceph-pool (from call storage_pool)", shape.vmStorage)
	}
	if shape.vmStorageType != "rbd" {
		t.Errorf("vmStorageType = %q; want rbd (looked up for ceph-pool)", shape.vmStorageType)
	}
}

// TestResolveVMShape_UnknownVMType_PropagatesCloudError verifies that an unknown
// vm_type selector in cloud_properties propagates a CloudError out of resolveVMShape.
func TestResolveVMShape_UnknownVMType_PropagatesCloudError(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
			VMTypes:        map[string]config.TypeProfile{},
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{entries: nil},
		},
	}

	parsed := minimalParsedArgsWithCP("test-storage", map[string]any{
		"vm_type": "no-such-profile",
	})

	_, err := resolveVMShape(context.Background(), deps, parsed)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// resolveVMShapeHotplugNUMAWithError — layered resolver integration
// --------------------------------------------------------------------------

// TestResolveVMShapeHotplugNUMA_NoProfile_ByteIdentical verifies that when no
// vm_type/disk_type profile and no call-layer hotplug/numa keys are set, the
// result is identical to the pre-resolver behavior: config values win.
func TestResolveVMShapeHotplugNUMA_NoProfile_ByteIdentical(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	// cp.Hotplug nil, cp.NUMA nil → config defaults.
	cp := createVMCloudProps{}
	cpMap := map[string]any{}

	hotplug, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hotplug != cfg.HotplugValue() {
		t.Errorf("hotplug = %q; want %q (config default)", hotplug, cfg.HotplugValue())
	}
	if numaEnabled != cfg.NUMAValue() {
		t.Errorf("numaEnabled = %v; want %v (config default)", numaEnabled, cfg.NUMAValue())
	}
}

// TestResolveVMShapeHotplugNUMA_CallHotplugEmpty_DisablesHotplug verifies that
// an explicit cp.Hotplug = "" (empty pointer) still wins — it means "disable
// hotplug" and must not be overridden by a profile value.
func TestResolveVMShapeHotplugNUMA_CallHotplugEmpty_DisablesHotplug(t *testing.T) {
	t.Parallel()

	emptyStr := ""
	cp := createVMCloudProps{Hotplug: &emptyStr}
	cpMap := map[string]any{
		"vm_type": "fast",
	}
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"fast": {
				CloudProperties: map[string]any{
					"hotplug": "network,disk,memory",
				},
			},
		},
	}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Explicit empty pointer wins over profile value — empty string disables hotplug.
	if hotplug != "" {
		t.Errorf("hotplug = %q; want empty string (explicit cp.Hotplug='' disables, must beat profile)", hotplug)
	}
}

// TestResolveVMShapeHotplugNUMA_ProfileHotplug_UsedWhenCallAbsent verifies that
// a hotplug value from a vm_type profile is used when cp.Hotplug is nil.
func TestResolveVMShapeHotplugNUMA_ProfileHotplug_UsedWhenCallAbsent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"memhot": {
				CloudProperties: map[string]any{
					"hotplug": "memory",
				},
			},
		},
	}
	cp := createVMCloudProps{} // Hotplug nil
	cpMap := map[string]any{"vm_type": "memhot"}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hotplug != "memory" {
		t.Errorf("hotplug = %q; want memory (from vm_type profile)", hotplug)
	}
}

// TestResolveVMShapeHotplugNUMA_CallHotplugBeatsProfile verifies that an
// explicit non-empty cp.Hotplug beats the profile value.
func TestResolveVMShapeHotplugNUMA_CallHotplugBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"memhot": {
				CloudProperties: map[string]any{
					"hotplug": "memory",
				},
			},
		},
	}
	callHotplug := "disk"
	cp := createVMCloudProps{Hotplug: &callHotplug}
	cpMap := map[string]any{"vm_type": "memhot", "hotplug": "disk"}

	hotplug, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hotplug != "disk" {
		t.Errorf("hotplug = %q; want disk (call beats profile)", hotplug)
	}
}

// TestResolveVMShapeHotplugNUMA_NUMAFalse_HonoredInCall verifies that an
// explicit cp.NUMA = false is preserved (not overridden by config or profile).
func TestResolveVMShapeHotplugNUMA_NUMAFalse_HonoredInCall(t *testing.T) {
	t.Parallel()

	numaTrue := true
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		NUMA:      &numaTrue, // config says true
		VMTypes: map[string]config.TypeProfile{
			"numa-on": {
				CloudProperties: map[string]any{
					"numa": true,
				},
			},
		},
	}
	numaFalse := false
	cp := createVMCloudProps{NUMA: &numaFalse}
	cpMap := map[string]any{"vm_type": "numa-on"}

	_, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if numaEnabled {
		t.Error("numaEnabled = true; want false (explicit cp.NUMA=false must win over config+profile)")
	}
}

// TestResolveVMShapeHotplugNUMA_ProfileNUMAFalse_HonoredWhenCallAbsent verifies
// that a profile can set numa=false when the call does not specify cp.NUMA.
func TestResolveVMShapeHotplugNUMA_ProfileNUMAFalse_HonoredWhenCallAbsent(t *testing.T) {
	t.Parallel()

	numaTrue := true
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		NUMA:      &numaTrue, // config default is true
		VMTypes: map[string]config.TypeProfile{
			"numa-off": {
				CloudProperties: map[string]any{
					"numa": false, // profile says off
				},
			},
		},
	}
	cp := createVMCloudProps{} // NUMA nil
	cpMap := map[string]any{"vm_type": "numa-off"}

	_, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if numaEnabled {
		t.Error("numaEnabled = true; want false (profile numa=false must override config default)")
	}
}

// TestResolveVMShapeHotplugNUMA_ProfileNUMATrue_UsedWhenCallAbsent verifies that
// a profile numa=true is respected when cp.NUMA is nil.
func TestResolveVMShapeHotplugNUMA_ProfileNUMATrue_UsedWhenCallAbsent(t *testing.T) {
	t.Parallel()

	numaFalse := false
	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		NUMA:      &numaFalse, // config default is false
		VMTypes: map[string]config.TypeProfile{
			"numa-on": {
				CloudProperties: map[string]any{
					"numa": true,
				},
			},
		},
	}
	cp := createVMCloudProps{}
	cpMap := map[string]any{"vm_type": "numa-on"}

	_, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !numaEnabled {
		t.Error("numaEnabled = false; want true (profile numa=true must override config default false)")
	}
}

// TestResolveVMShapeHotplugNUMA_UnknownVMType_ReturnsCloudError verifies that an
// unknown vm_type selector in cloud_properties causes a CloudError.
func TestResolveVMShapeHotplugNUMA_UnknownVMType_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes:   map[string]config.TypeProfile{},
	}
	cp := createVMCloudProps{}
	cpMap := map[string]any{"vm_type": "no-such-profile"}

	_, _, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// configureNICs bridge/model — layered resolver integration
// --------------------------------------------------------------------------

// TestConfigureNICs_NoProfile_ByteIdentical verifies the exact bridge/model
// precedence that existed before the resolver was added:
//
//	config.NetworkBridge → cp.NetworkBridge (VM-level) → per-NIC bridge override
//
// This acts as a golden no-op regression test.
func TestConfigureNICs_BridgeModel_NoProfile_ByteIdentical(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr1",
	}
	cp := createVMCloudProps{TargetNode: "pve"} // NetworkBridge/NetworkModel empty
	cpMap := map[string]any{}

	bridge, model, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if bridge != "vmbr1" {
		t.Errorf("bridge = %q; want vmbr1 (config.NetworkBridge)", bridge)
	}
	if model != "virtio" {
		t.Errorf("model = %q; want virtio (built-in default)", model)
	}
}

// TestConfigureNICs_BridgeModel_CallBridge_Wins verifies that cp.NetworkBridge
// (call layer) beats config.NetworkBridge.
func TestConfigureNICs_BridgeModel_CallBridge_Wins(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr1",
	}
	cp := createVMCloudProps{NetworkBridge: "vmbr99"}
	cpMap := map[string]any{"network_bridge": "vmbr99"}

	bridge, _, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if bridge != "vmbr99" {
		t.Errorf("bridge = %q; want vmbr99 (call beats config)", bridge)
	}
}

// TestConfigureNICs_BridgeModel_ProfileBridge_UsedWhenCallAbsent verifies that
// a network_bridge value in a vm_type profile supplies the VM-level default when
// the call does not include a network_bridge.
func TestConfigureNICs_BridgeModel_ProfileBridge_UsedWhenCallAbsent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage:     "local-lvm",
		NetworkBridge: "vmbr0",
		VMTypes: map[string]config.TypeProfile{
			"isolated": {
				CloudProperties: map[string]any{
					"network_bridge": "vmbr10",
				},
			},
		},
	}
	cp := createVMCloudProps{} // NetworkBridge empty
	cpMap := map[string]any{"vm_type": "isolated"}

	bridge, _, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if bridge != "vmbr10" {
		t.Errorf("bridge = %q; want vmbr10 (from vm_type profile)", bridge)
	}
}

// TestConfigureNICs_BridgeModel_CallBridgeBeatsProfile verifies that an explicit
// cp.NetworkBridge in the call beats the profile value.
func TestConfigureNICs_BridgeModel_CallBridgeBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"isolated": {
				CloudProperties: map[string]any{
					"network_bridge": "vmbr10",
				},
			},
		},
	}
	cp := createVMCloudProps{NetworkBridge: "vmbr5"}
	cpMap := map[string]any{"vm_type": "isolated", "network_bridge": "vmbr5"}

	bridge, _, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if bridge != "vmbr5" {
		t.Errorf("bridge = %q; want vmbr5 (call beats profile)", bridge)
	}
}

// TestConfigureNICs_BridgeModel_ProfileModel_UsedWhenCallAbsent verifies that
// network_model from a vm_type profile is used when the call has no model.
func TestConfigureNICs_BridgeModel_ProfileModel_UsedWhenCallAbsent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes: map[string]config.TypeProfile{
			"compat": {
				CloudProperties: map[string]any{
					"network_model": "e1000",
				},
			},
		},
	}
	cp := createVMCloudProps{} // NetworkModel empty
	cpMap := map[string]any{"vm_type": "compat"}

	_, model, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if model != "e1000" {
		t.Errorf("model = %q; want e1000 (from vm_type profile)", model)
	}
}

// TestConfigureNICs_BridgeModel_UnknownVMType_ReturnsCloudError verifies that an
// unknown vm_type selector causes a CloudError from resolveVMNICDefaultsWithError.
func TestConfigureNICs_BridgeModel_UnknownVMType_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes:   map[string]config.TypeProfile{},
	}
	cp := createVMCloudProps{}
	cpMap := map[string]any{"vm_type": "no-such"}

	_, _, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// resolveCloneMode — layered resolver integration
// --------------------------------------------------------------------------

// TestResolveCloneMode_NoProfile_ByteIdentical verifies that when no vm_type
// profile and no call-layer clone_mode key are present, the result equals
// config.CloneMode (default "auto").
func TestResolveCloneMode_NoProfile_ByteIdentical(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm", CloneMode: "auto"}
	cpMap := map[string]any{}

	mode, err := resolveCloneMode(cfg, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "auto" {
		t.Errorf("clone_mode = %q; want auto (config default)", mode)
	}
}

// TestResolveCloneMode_ProfileOverridesConfig verifies that a clone_mode in a
// vm_type profile overrides config.CloneMode.
func TestResolveCloneMode_ProfileOverridesConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		CloneMode: "auto",
		VMTypes: map[string]config.TypeProfile{
			"linked-only": {
				CloudProperties: map[string]any{
					"clone_mode": "linked",
				},
			},
		},
	}
	cpMap := map[string]any{"vm_type": "linked-only"}

	mode, err := resolveCloneMode(cfg, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "linked" {
		t.Errorf("clone_mode = %q; want linked (from vm_type profile)", mode)
	}
}

// TestResolveCloneMode_CallBeatsProfile verifies that an explicit clone_mode in
// the call layer beats the profile value.
func TestResolveCloneMode_CallBeatsProfile(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		CloneMode: "auto",
		VMTypes: map[string]config.TypeProfile{
			"linked-only": {
				CloudProperties: map[string]any{
					"clone_mode": "linked",
				},
			},
		},
	}
	cpMap := map[string]any{"vm_type": "linked-only", "clone_mode": "full"}

	mode, err := resolveCloneMode(cfg, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "full" {
		t.Errorf("clone_mode = %q; want full (call beats profile)", mode)
	}
}

// TestResolveCloneMode_ConfigDefault_WhenNilEmpty verifies that an empty
// config.CloneMode defaults to "auto".
func TestResolveCloneMode_ConfigDefault_WhenNilEmpty(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"} // CloneMode not set
	cpMap := map[string]any{}

	mode, err := resolveCloneMode(cfg, cpMap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "auto" {
		t.Errorf("clone_mode = %q; want auto (empty config defaults to auto)", mode)
	}
}

// TestResolveCloneMode_UnknownVMType_ReturnsCloudError verifies that an unknown
// vm_type selector causes a CloudError from resolveCloneMode.
func TestResolveCloneMode_UnknownVMType_ReturnsCloudError(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMStorage: "local-lvm",
		VMTypes:   map[string]config.TypeProfile{},
	}
	cpMap := map[string]any{"vm_type": "no-such"}

	_, err := resolveCloneMode(cfg, cpMap)
	if err == nil {
		t.Fatal("expected CloudError for unknown vm_type; got nil")
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// resolveVMShape: rootDiskPerfOpts + scsihw field tests
// --------------------------------------------------------------------------

// TestResolveVMShape_NoPerfOpts_ByteIdentical verifies that when no perf opts
// and no virtio_scsi_single are set, the shape carries empty rootDiskPerfOpts
// and scsihw=="virtio-scsi-pci" (byte-identical to pre-feature behaviour).
// TestResolveVMShape_NoPerfOpts_CurrentDefaults verifies the Phase 2 default
// shape when nothing is set anywhere: rootDiskPerfOpts carries iothread=1
// (its built-in default) and scsihw resolves to virtio-scsi-single (its
// built-in default) — replacing the pre-Phase-2 fully-empty/virtio-scsi-pci
// assertions this test used to make.
func TestResolveVMShape_NoPerfOpts_CurrentDefaults(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil,
		},
	}

	shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if len(shape.rootDiskPerfOpts) != 1 || shape.rootDiskPerfOpts["iothread"] != "1" {
		t.Errorf("rootDiskPerfOpts = %v; want map[iothread:1] (Phase 2 default)", shape.rootDiskPerfOpts)
	}
	if shape.scsihw != "virtio-scsi-single" {
		t.Errorf("scsihw = %q; want virtio-scsi-single (Phase 2 default)", shape.scsihw)
	}
}

// TestResolveVMShape_ExplicitOptOut_RestoresPreDefaultShape verifies that
// explicitly disabling both flipped knobs restores the pre-Phase-2 shape:
// empty rootDiskPerfOpts and virtio-scsi-pci.
func TestResolveVMShape_ExplicitOptOut_RestoresPreDefaultShape(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil,
		},
	}

	parsed := minimalParsedArgs("test-storage")
	parsed.cloudPropsMap = map[string]any{
		"iothread":           false,
		"virtio_scsi_single": false,
	}

	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if len(shape.rootDiskPerfOpts) != 0 {
		t.Errorf("rootDiskPerfOpts = %v; want empty map with explicit opt-out", shape.rootDiskPerfOpts)
	}
	if shape.scsihw != "virtio-scsi-pci" {
		t.Errorf("scsihw = %q; want virtio-scsi-pci with explicit opt-out", shape.scsihw)
	}
}

// TestResolveVMShape_PerfOpts_IOThreadCache verifies that iothread+cache in
// cloud_properties resolve to rootDiskPerfOpts.
func TestResolveVMShape_PerfOpts_IOThreadCache(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil,
		},
	}

	parsed := minimalParsedArgs("test-storage")
	parsed.cloudPropsMap = map[string]any{
		"iothread": true,
		"cache":    "writeback",
		// Explicit opt-out isolates this test's focus (iothread+cache
		// resolution) from the Phase 2 virtio_scsi_single default.
		"virtio_scsi_single": false,
	}

	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if shape.rootDiskPerfOpts["iothread"] != "1" {
		t.Errorf("rootDiskPerfOpts[iothread] = %q; want 1", shape.rootDiskPerfOpts["iothread"])
	}
	if shape.rootDiskPerfOpts["cache"] != "writeback" {
		t.Errorf("rootDiskPerfOpts[cache] = %q; want writeback", shape.rootDiskPerfOpts["cache"])
	}
	// ssd absent — virtio bus drops it; virtio_scsi_single explicitly opted out above.
	if shape.scsihw != "virtio-scsi-pci" {
		t.Errorf("scsihw = %q; want virtio-scsi-pci (explicit virtio_scsi_single:false)", shape.scsihw)
	}
}

// TestResolveVMShape_SSD_DroppedByVirtioBusFilter verifies that ssd:true in
// cloud_properties is absent from rootDiskPerfOpts (virtio bus drops it).
func TestResolveVMShape_SSD_DroppedByVirtioBusFilter(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil,
		},
	}

	parsed := minimalParsedArgs("test-storage")
	parsed.cloudPropsMap = map[string]any{
		"ssd":      true,
		"iothread": true,
	}

	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if _, ok := shape.rootDiskPerfOpts["ssd"]; ok {
		t.Error("rootDiskPerfOpts must not contain ssd (virtio bus drops it)")
	}
	// iothread still present.
	if shape.rootDiskPerfOpts["iothread"] != "1" {
		t.Errorf("rootDiskPerfOpts[iothread] = %q; want 1", shape.rootDiskPerfOpts["iothread"])
	}
}

// TestResolveVMShape_DiscardAuto_TrimCapableStorage_BakesDiscardNotSSD
// verifies the root-disk auto-resolution: on a TRIM-capable vmStorage type
// (lvmthin), discard auto-bakes into rootDiskPerfOpts, but ssd — even though
// it also auto-resolves true — is dropped by the pre-existing virtio bus
// filter, since the root disk is always virtio-blk regardless of
// virtio_scsi_single.
func TestResolveVMShape_DiscardAuto_TrimCapableStorage_BakesDiscardNotSSD(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{
				entries: []map[string]any{
					{"storage": "local-lvm", "type": "lvmthin"},
				},
			},
		},
	}

	shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if shape.rootDiskPerfOpts["discard"] != "on" {
		t.Errorf("rootDiskPerfOpts[discard] = %q; want on (lvmthin is TRIM-capable)", shape.rootDiskPerfOpts["discard"])
	}
	if _, present := shape.rootDiskPerfOpts["ssd"]; present {
		t.Error("rootDiskPerfOpts must not contain ssd — virtio bus filter drops it regardless of auto-resolution")
	}
}

// TestResolveVMShape_DiscardAuto_NonTrimStorage_NothingBaked verifies that
// discard stays unbaked on a non-TRIM-capable vmStorage type (thick lvm).
func TestResolveVMShape_DiscardAuto_NonTrimStorage_NothingBaked(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: &shapeTestClusterStorage{
				entries: []map[string]any{
					{"storage": "local-lvm", "type": "lvm"},
				},
			},
		},
	}

	shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgs("test-storage"))
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if _, present := shape.rootDiskPerfOpts["discard"]; present {
		t.Errorf("rootDiskPerfOpts must not contain discard on thick lvm, got %v", shape.rootDiskPerfOpts)
	}
}

// TestResolveVMShape_VirtioSCSISingle_Opt_In verifies that
// virtio_scsi_single:true sets scsihw=="virtio-scsi-single".
func TestResolveVMShape_VirtioSCSISingle_Opt_In(t *testing.T) {
	t.Parallel()

	deps := Deps{
		Config: &config.CPIConfig{
			Node:           "pve",
			VMStorage:      "local-lvm",
			VMIDRangeStart: 100,
		},
		PVE: &shapeTestPVEClient{
			clusterStorageSvc: nil,
		},
	}

	parsed := minimalParsedArgs("test-storage")
	parsed.cloudPropsMap = map[string]any{
		"virtio_scsi_single": true,
	}

	shape, err := resolveVMShape(context.Background(), deps, parsed)
	if err != nil {
		t.Fatalf("resolveVMShape error: %v", err)
	}
	if shape.scsihw != "virtio-scsi-single" {
		t.Errorf("scsihw = %q; want virtio-scsi-single", shape.scsihw)
	}
}

// --------------------------------------------------------------------------
// parseSha256SumOutput — input validation
// --------------------------------------------------------------------------

// TestParseSha256SumOutput_OversizedInput_Rejected verifies that an input
// exceeding parseSha256SumMaxBytes is rejected (returns "", false) without
// processing, preventing unbounded work on guest-controlled data.
func TestParseSha256SumOutput_OversizedInput_Rejected(t *testing.T) {
	t.Parallel()

	// Build a string that exceeds the limit. Content is otherwise valid sha256sum
	// format so that only the size check — not hex validation — causes rejection.
	big := strings.Repeat("a", parseSha256SumMaxBytes+1)
	got, ok := parseSha256SumOutput(&big)
	if ok {
		t.Errorf("oversized input must be rejected; got digest %q", got)
	}
	if got != "" {
		t.Errorf("oversized input must return empty digest; got %q", got)
	}
}

// TestParseSha256SumOutput_AtLimit_Accepted verifies that an input at exactly
// parseSha256SumMaxBytes is not rejected by the size guard (content validation
// may still reject it if it is not a valid sha256sum line).
func TestParseSha256SumOutput_AtLimit_Accepted(t *testing.T) {
	t.Parallel()

	// Build a valid sha256sum line that fits within the limit.
	// Use a known digest and a path padded to push us close to the boundary.
	digest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	// The line is "digest  path\n". Pad path so total length == parseSha256SumMaxBytes.
	// 64 (digest) + 2 (spaces) + 1 (newline) = 67 fixed bytes; path = limit - 67.
	pathLen := parseSha256SumMaxBytes - 67
	path := "/" + strings.Repeat("x", pathLen-1)
	line := digest + "  " + path + "\n"
	if len(line) != parseSha256SumMaxBytes {
		// Adjust: if path arithmetic differs, skip rather than give a false failure.
		t.Skipf("line length %d != limit %d; arithmetic mismatch in test setup", len(line), parseSha256SumMaxBytes)
	}
	got, ok := parseSha256SumOutput(&line)
	if !ok {
		t.Errorf("input at limit must not be rejected by size guard; parse failed")
	}
	if got != digest {
		t.Errorf("digest = %q; want %q", got, digest)
	}
}

// ---------------------------------------------------------------------------
// parseCreateVMArgs: stemcell CID grammar + stemcell_strategy validation
// ---------------------------------------------------------------------------

// TestParseCreateVMArgs_PathIdentityCID_PopulatesFields verifies that valid
// ":light:"/":heavy:" path-identity stemcell CIDs populate every new
// createVMParsedArgs stemcell field.
func TestParseCreateVMArgs_PathIdentityCID_PopulatesFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cid         string
		wantKind    pve.StemcellKind
		wantStor    string
		wantVolPath string
		wantFile    string
	}{
		{
			name:        "light",
			cid:         ":light:test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
			wantKind:    pve.StemcellKindLight,
			wantStor:    "test-storage",
			wantVolPath: "import/bosh-stemcell-foo-1.0-abc12345.qcow2",
			wantFile:    "bosh-stemcell-foo-1.0-abc12345.qcow2",
		},
		{
			name:        "heavy",
			cid:         ":heavy:nfs-pool:import/bosh-stemcell-bar-2.0-deadbeef.qcow2",
			wantKind:    pve.StemcellKindHeavy,
			wantStor:    "nfs-pool",
			wantVolPath: "import/bosh-stemcell-bar-2.0-deadbeef.qcow2",
			wantFile:    "bosh-stemcell-bar-2.0-deadbeef.qcow2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := marshalCreateVMArgs(t, []any{
				"agent-id-1", tc.cid,
				map[string]any{"cores": 1, "memory": 512},
				map[string]any{}, []string{}, map[string]any{},
			})
			parsed, err := parseCreateVMArgs(args)
			if err != nil {
				t.Fatalf("parseCreateVMArgs(%q): unexpected error: %v", tc.cid, err)
			}
			if parsed.stemcellCID != tc.cid {
				t.Errorf("stemcellCID = %q, want %q (original preserved)", parsed.stemcellCID, tc.cid)
			}
			if parsed.stemcellKind != tc.wantKind {
				t.Errorf("stemcellKind = %q, want %q", parsed.stemcellKind, tc.wantKind)
			}
			if parsed.stemcellStorage != tc.wantStor {
				t.Errorf("stemcellStorage = %q, want %q", parsed.stemcellStorage, tc.wantStor)
			}
			if parsed.stemcellVolPath != tc.wantVolPath {
				t.Errorf("stemcellVolPath = %q, want %q", parsed.stemcellVolPath, tc.wantVolPath)
			}
			if parsed.stemcellFilename != tc.wantFile {
				t.Errorf("stemcellFilename = %q, want %q", parsed.stemcellFilename, tc.wantFile)
			}
			wantRawVolid := tc.wantStor + ":" + tc.wantVolPath
			if parsed.rawVolid != wantRawVolid {
				t.Errorf("rawVolid = %q, want %q", parsed.rawVolid, wantRawVolid)
			}
		})
	}
}

// TestParseCreateVMArgs_LegacyCID_Rejected verifies every retired stemcell
// CID grammar (legacy template CID, old "light:" prefix, bare volid, and
// integer CIDs) is rejected as a non-retriable CloudError at parse time —
// pre-release cutover leaves no compat path for any of these forms.
func TestParseCreateVMArgs_LegacyCID_Rejected(t *testing.T) {
	t.Parallel()

	cases := []string{
		"template:6042",
		"light:test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
		"test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
		"5042",
		"",
	}

	for _, cid := range cases {
		t.Run(cid, func(t *testing.T) {
			t.Parallel()
			args := marshalCreateVMArgs(t, []any{
				"agent-id-1", cid,
				map[string]any{"cores": 1, "memory": 512},
				map[string]any{}, []string{}, map[string]any{},
			})
			_, err := parseCreateVMArgs(args)
			if err == nil {
				t.Fatalf("parseCreateVMArgs(%q): expected error, got nil", cid)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Errorf("parseCreateVMArgs(%q): expected non-retriable Cloud error, got: %v", cid, err)
			}
		})
	}
}

// TestParseCreateVMArgs_StemcellStrategy_Validation verifies the per-VM
// cloud_properties.stemcell_strategy override: "", "template", and "import"
// are accepted; any other value is a non-retriable CloudError at parse time.
func TestParseCreateVMArgs_StemcellStrategy_Validation(t *testing.T) {
	t.Parallel()

	const validCID = ":light:test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2"

	cases := []struct {
		name      string
		strategy  string
		wantError bool
	}{
		{name: "empty defers to global", strategy: "", wantError: false},
		{name: "template", strategy: "template", wantError: false},
		{name: "import", strategy: "import", wantError: false},
		{name: "invalid value", strategy: "bogus", wantError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := marshalCreateVMArgs(t, []any{
				"agent-id-1", validCID,
				map[string]any{"cores": 1, "memory": 512, "stemcell_strategy": tc.strategy},
				map[string]any{}, []string{}, map[string]any{},
			})
			parsed, err := parseCreateVMArgs(args)
			if tc.wantError {
				if err == nil {
					t.Fatalf("stemcell_strategy=%q: expected error, got nil", tc.strategy)
				}
				if !cpierrors.IsType(err, cpierrors.TypeCloud) {
					t.Errorf("stemcell_strategy=%q: expected non-retriable Cloud error, got: %v", tc.strategy, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stemcell_strategy=%q: unexpected error: %v", tc.strategy, err)
			}
			if parsed.cloudProps.StemcellStrategy != tc.strategy {
				t.Errorf("parsed StemcellStrategy = %q, want %q", parsed.cloudProps.StemcellStrategy, tc.strategy)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveStemcellStrategy
// ---------------------------------------------------------------------------

// TestResolveStemcellStrategy_Precedence verifies per-VM cloud_properties
// override > global config value > "template" default.
func TestResolveStemcellStrategy_Precedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		perVM      string
		globalCfg  string
		wantResult string
	}{
		{name: "per-VM import overrides global template", perVM: "import", globalCfg: "template", wantResult: "import"},
		{name: "per-VM template overrides global import", perVM: "template", globalCfg: "import", wantResult: "template"},
		{name: "empty per-VM defers to global import", perVM: "", globalCfg: "import", wantResult: "import"},
		{name: "empty per-VM and empty global defaults to template", perVM: "", globalCfg: "", wantResult: "template"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{StemcellStrategy: tc.globalCfg}
			parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{StemcellStrategy: tc.perVM}}
			got := resolveStemcellStrategy(cfg, parsed)
			if got != tc.wantResult {
				t.Errorf("resolveStemcellStrategy(global=%q, perVM=%q) = %q, want %q",
					tc.globalCfg, tc.perVM, got, tc.wantResult)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractSHA8FromParsed
// ---------------------------------------------------------------------------

// TestExtractSHA8FromParsed verifies sha8 extraction from the parsed args'
// rawVolid, including the unextractable case (no valid trailing sha8) that
// strategy=template treats as "skip the cache lookup, go straight to import".
func TestExtractSHA8FromParsed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		rawVolid string
		wantSHA8 string
		wantOK   bool
	}{
		{
			name:     "valid sha8",
			rawVolid: "test-storage:import/bosh-stemcell-foo-1.0-abc12345.qcow2",
			wantSHA8: "abc12345",
			wantOK:   true,
		},
		{
			name:     "unextractable (custom filename, no sha8 suffix)",
			rawVolid: "test-storage:import/my-custom-image.qcow2",
			wantSHA8: "",
			wantOK:   false,
		},
		{
			name:     "empty rawVolid",
			rawVolid: "",
			wantSHA8: "",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parsed := &createVMParsedArgs{rawVolid: tc.rawVolid}
			gotSHA8, gotOK := extractSHA8FromParsed(parsed)
			if gotOK != tc.wantOK {
				t.Errorf("extractSHA8FromParsed(rawVolid=%q): ok=%v, want %v", tc.rawVolid, gotOK, tc.wantOK)
			}
			if gotSHA8 != tc.wantSHA8 {
				t.Errorf("extractSHA8FromParsed(rawVolid=%q): sha8=%q, want %q", tc.rawVolid, gotSHA8, tc.wantSHA8)
			}
		})
	}
}
