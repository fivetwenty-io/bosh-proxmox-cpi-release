package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// ParseDiskCID
// ---------------------------------------------------------------------------

func TestParseDiskCID_Valid(t *testing.T) {
	storage, volume, err := pve.ParseDiskCID("local:vm-100-disk-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storage != "local" {
		t.Errorf("storage: want %q got %q", "local", storage)
	}
	if volume != "vm-100-disk-1" {
		t.Errorf("volume: want %q got %q", "vm-100-disk-1", volume)
	}
}

func TestParseDiskCID_ValidLocalLVM(t *testing.T) {
	s, v, err := pve.ParseDiskCID("local-lvm:vm-200-disk-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != "local-lvm" || v != "vm-200-disk-0" {
		t.Errorf("got (%q, %q)", s, v)
	}
}

func TestParseDiskCID_NoColon(t *testing.T) {
	_, _, err := pve.ParseDiskCID("broken")
	if err == nil {
		t.Fatal("expected error for CID without colon")
	}
}

func TestParseDiskCID_Empty(t *testing.T) {
	_, _, err := pve.ParseDiskCID("")
	if err == nil {
		t.Fatal("expected error for empty CID")
	}
}

func TestParseDiskCID_EmptyStorage(t *testing.T) {
	_, _, err := pve.ParseDiskCID(":volume")
	if err == nil {
		t.Fatal("expected error for empty storage part")
	}
}

func TestParseDiskCID_EmptyVolume(t *testing.T) {
	_, _, err := pve.ParseDiskCID("storage:")
	if err == nil {
		t.Fatal("expected error for empty volume part")
	}
}

func TestParseDiskCID_MultipleColons(t *testing.T) {
	// Only first colon is the split point; remainder is volume.
	s, v, err := pve.ParseDiskCID("local:vm-100:extra")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != "local" || v != "vm-100:extra" {
		t.Errorf("got (%q, %q)", s, v)
	}
}

// ---------------------------------------------------------------------------
// FormatDiskCID
// ---------------------------------------------------------------------------

func TestFormatDiskCID(t *testing.T) {
	got := pve.FormatDiskCID("local", "vol")
	if got != "local:vol" {
		t.Errorf("want %q got %q", "local:vol", got)
	}
}

func TestFormatDiskCID_RoundTrip(t *testing.T) {
	original := "local-lvm:vm-999-disk-3"
	s, v, err := pve.ParseDiskCID(original)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pve.FormatDiskCID(s, v) != original {
		t.Errorf("round-trip failed: got %q", pve.FormatDiskCID(s, v))
	}
}

// ---------------------------------------------------------------------------
// ParseSnapshotCID
// ---------------------------------------------------------------------------

func TestParseSnapshotCID_Valid(t *testing.T) {
	vmCID, snapName, err := pve.ParseSnapshotCID("vm-100:snap1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmCID != "vm-100" {
		t.Errorf("vmCID: want %q got %q", "vm-100", vmCID)
	}
	if snapName != "snap1" {
		t.Errorf("snapName: want %q got %q", "snap1", snapName)
	}
}

func TestParseSnapshotCID_NoColon(t *testing.T) {
	_, _, err := pve.ParseSnapshotCID("nocobon")
	if err == nil {
		t.Fatal("expected error for snapshot CID without colon")
	}
}

func TestParseSnapshotCID_Empty(t *testing.T) {
	_, _, err := pve.ParseSnapshotCID("")
	if err == nil {
		t.Fatal("expected error for empty snapshot CID")
	}
}

func TestParseSnapshotCID_EmptyVMCID(t *testing.T) {
	_, _, err := pve.ParseSnapshotCID(":snapname")
	if err == nil {
		t.Fatal("expected error for empty vm_cid part")
	}
}

func TestParseSnapshotCID_EmptySnapName(t *testing.T) {
	_, _, err := pve.ParseSnapshotCID("vm-100:")
	if err == nil {
		t.Fatal("expected error for empty snap_name part")
	}
}

// ---------------------------------------------------------------------------
// FormatSnapshotCID
// ---------------------------------------------------------------------------

func TestFormatSnapshotCID(t *testing.T) {
	got := pve.FormatSnapshotCID("vm-100", "my-snap")
	if got != "vm-100:my-snap" {
		t.Errorf("want %q got %q", "vm-100:my-snap", got)
	}
}

func TestFormatSnapshotCID_RoundTrip(t *testing.T) {
	original := "vm-42:pre-deploy"
	v, s, err := pve.ParseSnapshotCID(original)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pve.FormatSnapshotCID(v, s) != original {
		t.Errorf("round-trip failed")
	}
}

// ---------------------------------------------------------------------------
// ResolveDiskID — mock infrastructure
// ---------------------------------------------------------------------------

// diskMockQEMUService implements qemu.Service with a configurable Config response.
// All methods except Config panic if called, enforcing that ResolveDiskID only
// calls Config.
type diskMockQEMUService struct {
	configCfg map[string]interface{}
	configErr error
}

func (m *diskMockQEMUService) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	return m.configCfg, m.configErr
}
func (m *diskMockQEMUService) Create(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	panic("Create not expected")
}
func (m *diskMockQEMUService) Status(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	panic("Status not expected")
}
func (m *diskMockQEMUService) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("Start not expected")
}
func (m *diskMockQEMUService) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("Stop not expected")
}
func (m *diskMockQEMUService) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("Reset not expected")
}
func (m *diskMockQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]interface{}) (string, error) {
	panic("Clone not expected")
}
func (m *diskMockQEMUService) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("Template not expected")
}
func (m *diskMockQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	panic("AttachDisk not expected")
}
func (m *diskMockQEMUService) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("DetachDisk not expected")
}
func (m *diskMockQEMUService) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("ResizeDisk not expected")
}
func (m *diskMockQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]interface{}) (string, error) {
	panic("Snapshot not expected")
}
func (m *diskMockQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("DeleteSnapshot not expected")
}
func (m *diskMockQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	panic("ListSnapshots not expected")
}
func (m *diskMockQEMUService) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("RollbackSnapshot not expected")
}

// diskMockClient satisfies pve.Client and routes QEMU() to the disk mock service.
type diskMockClient struct {
	qemuSvc qemu.Service
}

func (c *diskMockClient) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *diskMockClient) Storage() storage.Service               { return nil }
func (c *diskMockClient) CloudInit() cloudinit.Service           { return nil }
func (c *diskMockClient) Tasks() tasks.Service                   { return nil }
func (c *diskMockClient) Nodes() nodes.Service                   { return nil }
func (c *diskMockClient) Cluster() cluster.Service               { return nil }
func (c *diskMockClient) ClusterStorage() clusterstorage.Service { return nil }

func newMockClientWithConfig(cfg map[string]interface{}, err error) pve.Client {
	return &diskMockClient{
		qemuSvc: &diskMockQEMUService{configCfg: cfg, configErr: err},
	}
}

// ---------------------------------------------------------------------------
// ResolveDiskID tests
// ---------------------------------------------------------------------------

func TestResolveDiskID_Found(t *testing.T) {
	cfg := map[string]interface{}{
		"scsi0": "local-lvm:vm-100-disk-0",
		"scsi1": "local:vm-100-disk-1",
		"ide2":  "local:cloudinit",
		"name":  "test-vm",
	}
	c := newMockClientWithConfig(cfg, nil)

	diskID, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local:vm-100-disk-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diskID != "scsi1" {
		t.Errorf("want %q got %q", "scsi1", diskID)
	}
}

func TestResolveDiskID_FoundScsi0(t *testing.T) {
	cfg := map[string]interface{}{
		"scsi0": "local-lvm:vm-100-disk-0",
	}
	c := newMockClientWithConfig(cfg, nil)

	diskID, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local-lvm:vm-100-disk-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diskID != "scsi0" {
		t.Errorf("want %q got %q", "scsi0", diskID)
	}
}

func TestResolveDiskID_NotFound(t *testing.T) {
	cfg := map[string]interface{}{
		"scsi0": "local-lvm:vm-100-disk-0",
	}
	c := newMockClientWithConfig(cfg, nil)

	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local:does-not-exist")
	if err == nil {
		t.Fatal("expected error when volid not in config")
	}
}

func TestResolveDiskID_ConfigError(t *testing.T) {
	configErr := errors.New("API unreachable")
	c := newMockClientWithConfig(nil, configErr)

	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local:some-vol")
	if err == nil {
		t.Fatal("expected error when Config returns error")
	}
	if !errors.Is(err, configErr) {
		t.Errorf("want cause %v, got %v", configErr, err)
	}
}

func TestResolveDiskID_EmptyNode(t *testing.T) {
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "", 100, "local:disk")
	if err == nil {
		t.Fatal("expected error for empty node")
	}
}

func TestResolveDiskID_InvalidVMID(t *testing.T) {
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 0, "local:disk")
	if err == nil {
		t.Fatal("expected error for zero vmid")
	}
}

func TestResolveDiskID_EmptyVolID(t *testing.T) {
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "")
	if err == nil {
		t.Fatal("expected error for empty volid")
	}
}

func TestResolveDiskID_EmptyConfig(t *testing.T) {
	c := newMockClientWithConfig(map[string]interface{}{}, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local:vm-100-disk-0")
	if err == nil {
		t.Fatal("expected error when config is empty and volid not found")
	}
}

// ---------------------------------------------------------------------------
// FindVMByDiskVolid — transient retry test infrastructure
// ---------------------------------------------------------------------------

// diskClusterClient is a pve.Client variant that exposes both a QEMU service
// (for Config calls) and a Cluster service (for ListResources calls).
type diskClusterClient struct {
	qemuSvc    qemu.Service
	clusterSvc cluster.Service
}

func (c *diskClusterClient) QEMU() qemu.Service                     { return c.qemuSvc }
func (c *diskClusterClient) Storage() storage.Service               { return nil }
func (c *diskClusterClient) CloudInit() cloudinit.Service           { return nil }
func (c *diskClusterClient) Tasks() tasks.Service                   { return nil }
func (c *diskClusterClient) Nodes() nodes.Service                   { return nil }
func (c *diskClusterClient) Cluster() cluster.Service               { return c.clusterSvc }
func (c *diskClusterClient) ClusterStorage() clusterstorage.Service { return nil }

// diskFakeCluster is a minimal cluster.Service that delegates ListResources
// to an injected function.
type diskFakeCluster struct {
	cluster.Service
	listFn func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
}

func (f *diskFakeCluster) ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	return f.listFn(ctx, params)
}

// diskFakeQEMU is a minimal qemu.Service that returns a canned config for any VM.
type diskFakeQEMU struct {
	qemu.Service
	cfg map[string]interface{}
}

func (q *diskFakeQEMU) Config(_ context.Context, _ string, _ int) (map[string]interface{}, error) {
	return q.cfg, nil
}

// diskClusterResp builds a cluster.ListResourcesResponse from typed rows.
func diskClusterResp(rows ...map[string]any) *cluster.ListResourcesResponse {
	out := make(cluster.ListResourcesResponse, 0, len(rows))
	for _, r := range rows {
		b, _ := json.Marshal(r)
		out = append(out, b)
	}
	return &out
}

// ---------------------------------------------------------------------------
// TestFindVMByDiskVolid_TransientThenSuccess
// ---------------------------------------------------------------------------

func TestFindVMByDiskVolid_TransientThenSuccess(t *testing.T) {
	// ListResources errors once with a transient signal, then succeeds.
	// FindVMByDiskVolid must retry and return the correct (vmid, node).
	var listCalls int
	volid := "local-lvm:vm-200-disk-0"

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				listCalls++
				if listCalls == 1 {
					// Transient shape: "(code: 596)" is detected by IsTransientTransport.
					return nil, errors.New("pveproxy backend gone (code: 596)")
				}
				return diskClusterResp(
					map[string]any{"vmid": int64(200), "node": "pve-01"},
				), nil
			},
		},
		qemuSvc: &diskFakeQEMU{
			cfg: map[string]interface{}{
				"scsi0": volid,
			},
		},
	}

	vmid, node, err := pve.FindVMByDiskVolid(context.Background(), c, "pve-default", volid)
	if err != nil {
		t.Fatalf("expected success after transient retry, got: %v", err)
	}
	if vmid != 200 {
		t.Errorf("vmid: want 200, got %d", vmid)
	}
	if node != "pve-01" {
		t.Errorf("node: want pve-01, got %s", node)
	}
	if listCalls < 2 {
		t.Errorf("expected at least 2 ListResources calls (1 transient + 1 success), got %d", listCalls)
	}
}

// ---------------------------------------------------------------------------
// FindUnusedDiskEntries
// ---------------------------------------------------------------------------

func TestFindUnusedDiskEntries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cfg      map[string]interface{}
		wantKeys []string          // expected slot keys in result
		wantVals map[string]string // expected slot→bare-volid mapping
		wantLen  int               // expected result length
	}{
		{
			name:    "empty config map returns empty result",
			cfg:     map[string]interface{}{},
			wantLen: 0,
		},
		{
			name: "one unused0 entry parsed correctly",
			cfg: map[string]interface{}{
				"unused0": "local-lvm:vm-100-disk-0,size=10G",
			},
			wantLen:  1,
			wantKeys: []string{"unused0"},
			wantVals: map[string]string{"unused0": "local-lvm:vm-100-disk-0"},
		},
		{
			name: "multiple unusedN entries all returned in slot-keyed map",
			cfg: map[string]interface{}{
				"unused0": "local-lvm:vm-100-disk-0",
				"unused1": "local-lvm:vm-100-disk-1",
				"unused2": "data:vm-200-disk-0",
			},
			wantLen: 3,
			wantVals: map[string]string{
				"unused0": "local-lvm:vm-100-disk-0",
				"unused1": "local-lvm:vm-100-disk-1",
				"unused2": "data:vm-200-disk-0",
			},
		},
		{
			name: "mixed unused and non-unused keys returns only unused entries",
			cfg: map[string]interface{}{
				"scsi0":   "local-lvm:vm-100-disk-0",
				"net0":    "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
				"unused0": "local-lvm:vm-100-disk-1",
				"name":    "my-vm",
				"ide2":    "local:cloudinit",
			},
			wantLen:  1,
			wantVals: map[string]string{"unused0": "local-lvm:vm-100-disk-1"},
		},
		{
			name: "unused0 with extra comma-separated options strips options suffix",
			cfg: map[string]interface{}{
				"unused0": "local-lvm:vm-100-disk-0,iothread=1,cache=writeback",
			},
			wantLen:  1,
			wantVals: map[string]string{"unused0": "local-lvm:vm-100-disk-0"},
		},
		{
			name: "malformed unused entry without storage colon is skipped (bare value kept)",
			cfg: map[string]interface{}{
				// PVE normally always emits "storage:volid"; a value with no colon
				// is unexpected but FindUnusedDiskEntries must not panic. The bare
				// string (no comma suffix) is stored as-is under the slot key.
				"unused0": "garbage_no_colon",
			},
			// The function stores the bare value; no error is possible since
			// FindUnusedDiskEntries has no error return. Callers that need the
			// storage prefix (ParseDiskCID) will error on their own.
			wantLen:  1,
			wantVals: map[string]string{"unused0": "garbage_no_colon"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pve.FindUnusedDiskEntries(tc.cfg)

			if len(got) != tc.wantLen {
				t.Errorf("len: want %d, got %d; result=%v", tc.wantLen, len(got), got)
			}
			for slot, wantVolid := range tc.wantVals {
				gotVolid, ok := got[slot]
				if !ok {
					t.Errorf("slot %q missing from result; result=%v", slot, got)
					continue
				}
				if gotVolid != wantVolid {
					t.Errorf("slot %q: want volid %q, got %q", slot, wantVolid, gotVolid)
				}
			}
			// No extra keys beyond expected.
			for slot := range got {
				if _, expected := tc.wantVals[slot]; !expected && tc.wantLen > 0 {
					// wantVals might be nil (wantLen==0 case) — only fail when
					// wantVals is populated and the slot is truly unexpected.
					if tc.wantVals != nil {
						t.Errorf("unexpected slot %q in result", slot)
					}
				}
			}
		})
	}
}
