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
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// ParseDiskCID
// ---------------------------------------------------------------------------

func TestParseDiskCID_Valid(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	s, v, err := pve.ParseDiskCID("local-lvm:vm-200-disk-0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != "local-lvm" || v != "vm-200-disk-0" {
		t.Errorf("got (%q, %q)", s, v)
	}
}

func TestParseDiskCID_NoColon(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseDiskCID("broken")
	if err == nil {
		t.Fatal("expected error for CID without colon")
	}
}

func TestParseDiskCID_Empty(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseDiskCID("")
	if err == nil {
		t.Fatal("expected error for empty CID")
	}
}

func TestParseDiskCID_EmptyStorage(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseDiskCID(":volume")
	if err == nil {
		t.Fatal("expected error for empty storage part")
	}
}

func TestParseDiskCID_EmptyVolume(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseDiskCID("storage:")
	if err == nil {
		t.Fatal("expected error for empty volume part")
	}
}

func TestParseDiskCID_MultipleColons(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	got := pve.FormatDiskCID("local", "vol")
	if got != "local:vol" {
		t.Errorf("want %q got %q", "local:vol", got)
	}
}

func TestFormatDiskCID_RoundTrip(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, _, err := pve.ParseSnapshotCID("nocobon")
	if err == nil {
		t.Fatal("expected error for snapshot CID without colon")
	}
}

func TestParseSnapshotCID_Empty(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseSnapshotCID("")
	if err == nil {
		t.Fatal("expected error for empty snapshot CID")
	}
}

func TestParseSnapshotCID_EmptyVMCID(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseSnapshotCID(":snapname")
	if err == nil {
		t.Fatal("expected error for empty vm_cid part")
	}
}

func TestParseSnapshotCID_EmptySnapName(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseSnapshotCID("vm-100:")
	if err == nil {
		t.Fatal("expected error for empty snap_name part")
	}
}

// ---------------------------------------------------------------------------
// FormatSnapshotCID
// ---------------------------------------------------------------------------

func TestFormatSnapshotCID(t *testing.T) {
	t.Parallel()
	got := pve.FormatSnapshotCID("vm-100", "my-snap")
	if got != "vm-100:my-snap" {
		t.Errorf("want %q got %q", "vm-100:my-snap", got)
	}
}

func TestFormatSnapshotCID_RoundTrip(t *testing.T) {
	t.Parallel()
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
	configCfg map[string]any
	configErr error
}

func (m *diskMockQEMUService) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return m.configCfg, m.configErr
}
func (m *diskMockQEMUService) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("Create not expected")
}
func (m *diskMockQEMUService) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
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
func (m *diskMockQEMUService) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
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
func (m *diskMockQEMUService) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("Snapshot not expected")
}
func (m *diskMockQEMUService) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("DeleteSnapshot not expected")
}
func (m *diskMockQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
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
func (c *diskMockClient) Pools() pve.PoolService                 { return nil }

func newMockClientWithConfig(cfg map[string]any, err error) pve.Client {
	return &diskMockClient{
		qemuSvc: &diskMockQEMUService{configCfg: cfg, configErr: err},
	}
}

// ---------------------------------------------------------------------------
// ResolveDiskID tests
// ---------------------------------------------------------------------------

func TestResolveDiskID_Found(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{
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
	t.Parallel()
	cfg := map[string]any{
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
	t.Parallel()
	cfg := map[string]any{
		"scsi0": "local-lvm:vm-100-disk-0",
	}
	c := newMockClientWithConfig(cfg, nil)

	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "local:does-not-exist")
	if err == nil {
		t.Fatal("expected error when volid not in config")
	}
}

func TestResolveDiskID_ConfigError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "", 100, "local:disk")
	if err == nil {
		t.Fatal("expected error for empty node")
	}
}

func TestResolveDiskID_InvalidVMID(t *testing.T) {
	t.Parallel()
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 0, "local:disk")
	if err == nil {
		t.Fatal("expected error for zero vmid")
	}
}

func TestResolveDiskID_EmptyVolID(t *testing.T) {
	t.Parallel()
	c := newMockClientWithConfig(nil, nil)
	_, err := pve.ResolveDiskID(context.Background(), c, "pve1", 100, "")
	if err == nil {
		t.Fatal("expected error for empty volid")
	}
}

func TestResolveDiskID_EmptyConfig(t *testing.T) {
	t.Parallel()
	c := newMockClientWithConfig(map[string]any{}, nil)
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
func (c *diskClusterClient) Pools() pve.PoolService                 { return nil }

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
	cfg map[string]any
}

func (q *diskFakeQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return q.cfg, nil
}

// diskFakeQEMUFn is a minimal qemu.Service whose Config is driven by a function
// so tests can return per-VMID configs or errors.
type diskFakeQEMUFn struct {
	qemu.Service
	fn func(node string, vmid int) (map[string]any, error)
}

func (q *diskFakeQEMUFn) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	return q.fn(node, vmid)
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
	t.Parallel()
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
			cfg: map[string]any{
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
// FindVMByDiskVolid — per-VM Config error discrimination
// ---------------------------------------------------------------------------

// TestFindVMByDiskVolid_TransientConfigError_ReturnsRetriable verifies that a
// transient Config error on a VM mid-scan is returned as a retriable error, not
// silently swallowed. If the transient VM actually holds the disk, swallowing the
// error would produce a false "disk not attached to any VM" result.
func TestFindVMByDiskVolid_TransientConfigError_ReturnsRetriable(t *testing.T) {
	t.Parallel()
	volid := "local-lvm:vm-301-disk-0"

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(
					map[string]any{"vmid": int64(301), "node": "pve-01"},
				), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{
			fn: func(_ string, vmid int) (map[string]any, error) {
				// Transient shape: "(code: 596)" is detected by IsTransientTransport.
				return nil, errors.New("pveproxy backend gone (code: 596)")
			},
		},
	}

	_, _, err := pve.FindVMByDiskVolid(context.Background(), c, "pve-default", volid)
	if err == nil {
		t.Fatal("expected retriable error for transient Config failure; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient Config error must produce TypeRetriableCloud; got: %v", err)
	}
}

// TestFindVMByDiskVolid_NotFoundConfigError_SkippedScanContinues verifies that a
// not-found Config error (deleted/template VM) is silently skipped and the scan
// continues to find the disk on a later VM in the list.
func TestFindVMByDiskVolid_NotFoundConfigError_SkippedScanContinues(t *testing.T) {
	t.Parallel()
	volid := "local-lvm:vm-402-disk-0"

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(
					map[string]any{"vmid": int64(401), "node": "pve-01"}, // will 404
					map[string]any{"vmid": int64(402), "node": "pve-01"}, // holds the disk
				), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{
			fn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == 401 {
					// Simulate a 404 / not-found for a deleted VM.
					return nil, sdkerrors.ErrNotFound
				}
				return map[string]any{"scsi0": volid}, nil
			},
		},
	}

	gotVMID, gotNode, err := pve.FindVMByDiskVolid(context.Background(), c, "pve-default", volid)
	if err != nil {
		t.Fatalf("not-found Config error must be skipped; scan must find the disk; got: %v", err)
	}
	if gotVMID != 402 {
		t.Errorf("vmid: want 402, got %d", gotVMID)
	}
	if gotNode != "pve-01" {
		t.Errorf("node: want pve-01, got %s", gotNode)
	}
}

// ---------------------------------------------------------------------------
// EncodeDiskCID / ParseEncodedDiskCID
// ---------------------------------------------------------------------------

func TestEncodeDiskCID_NilMeta(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"
	if got := pve.EncodeDiskCID(bare, nil); got != bare {
		t.Errorf("nil meta: want %q unchanged, got %q", bare, got)
	}
}

func TestEncodeDiskCID_ZeroMeta(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"
	meta := &pve.DiskCIDMeta{}
	if got := pve.EncodeDiskCID(bare, meta); got != bare {
		t.Errorf("zero meta: want %q unchanged, got %q", bare, got)
	}
}

func TestParseEncodedDiskCID_Bare(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"
	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(bare)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta != nil {
		t.Errorf("meta: want nil, got %+v", gotMeta)
	}
}

func TestParseEncodedDiskCID_Empty(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseEncodedDiskCID("")
	if err == nil {
		t.Fatal("expected error for empty CID")
	}
}

func TestEncodeParseDiskCID_RoundTripFullMeta(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-9003-disk-0"
	meta := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1"}
	encoded := pve.EncodeDiskCID(bare, meta)

	if encoded == bare {
		t.Fatal("encoded CID should differ from bare when meta is non-empty")
	}

	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta == nil {
		t.Fatal("meta: want non-nil")
	}
	if gotMeta.Pool != "local-lvm" {
		t.Errorf("Pool: want %q, got %q", "local-lvm", gotMeta.Pool)
	}
	if gotMeta.Node != "pve1" {
		t.Errorf("Node: want %q, got %q", "pve1", gotMeta.Node)
	}
	if gotMeta.AZ != "z1" {
		t.Errorf("AZ: want %q, got %q", "z1", gotMeta.AZ)
	}
}

func TestEncodeParseDiskCID_RoundTripPoolOnly(t *testing.T) {
	t.Parallel()
	bare := "data:vm-9003-disk-0"
	meta := &pve.DiskCIDMeta{Pool: "data"}
	encoded := pve.EncodeDiskCID(bare, meta)

	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta == nil {
		t.Fatal("meta: want non-nil")
	}
	if gotMeta.Pool != "data" {
		t.Errorf("Pool: want %q, got %q", "data", gotMeta.Pool)
	}
	if gotMeta.Node != "" {
		t.Errorf("Node: want empty, got %q", gotMeta.Node)
	}
	if gotMeta.AZ != "" {
		t.Errorf("AZ: want empty, got %q", gotMeta.AZ)
	}
}

func TestParseEncodedDiskCID_MalformedBase64(t *testing.T) {
	t.Parallel()
	cid := "local-lvm:vm-100-disk-0|!!!notbase64!!!"
	_, _, err := pve.ParseEncodedDiskCID(cid)
	if err == nil {
		t.Fatal("expected error for malformed base64 suffix")
	}
}

func TestParseEncodedDiskCID_MalformedJSON(t *testing.T) {
	t.Parallel()
	import64 := "bm90anNvbg==" // base64url of "notjson"
	cid := "local-lvm:vm-100-disk-0|" + import64
	_, _, err := pve.ParseEncodedDiskCID(cid)
	if err == nil {
		t.Fatal("expected error for base64-encoded non-JSON suffix")
	}
}

func TestParseEncodedDiskCID_EmptySuffix(t *testing.T) {
	t.Parallel()
	// Pipe present but no suffix is malformed.
	cid := "local-lvm:vm-100-disk-0|"
	_, _, err := pve.ParseEncodedDiskCID(cid)
	if err == nil {
		t.Fatal("expected error for empty suffix after pipe")
	}
}

func TestParseEncodedDiskCID_BaseStillParseable(t *testing.T) {
	t.Parallel()
	// Round-trip: encoded CID base must still pass ParseDiskCID.
	bare := "local-lvm:vm-9003-disk-0"
	meta := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve2", AZ: "az-a"}
	encoded := pve.EncodeDiskCID(bare, meta)

	gotBase, _, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	storage, volume, err2 := pve.ParseDiskCID(gotBase)
	if err2 != nil {
		t.Fatalf("ParseDiskCID on base: %v", err2)
	}
	if storage != "local-lvm" {
		t.Errorf("storage: want %q, got %q", "local-lvm", storage)
	}
	if volume != "vm-9003-disk-0" {
		t.Errorf("volume: want %q, got %q", "vm-9003-disk-0", volume)
	}
}

// Table-driven tests cover all call-site wrapping patterns: ParseEncodedDiskCID
// strips the suffix, then ParseDiskCID on the base yields the same storage/volume
// as parsing the bare CID directly. This verifies that the 10 call site wrappers
// produce identical results to legacy bare CID behaviour.
func TestCallSiteWrapperBehaviour(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cid         string // may be bare or encoded
		wantStorage string
		wantVolume  string
	}{
		{
			name:        "bare lvm volid",
			cid:         "local-lvm:vm-100-disk-0",
			wantStorage: "local-lvm",
			wantVolume:  "vm-100-disk-0",
		},
		{
			name:        "bare dir volid with subpath",
			cid:         "local:9001/vm-9001-disk-0.raw",
			wantStorage: "local",
			wantVolume:  "9001/vm-9001-disk-0.raw",
		},
		{
			name: "encoded full meta wraps to same base",
			cid: pve.EncodeDiskCID(
				"local-lvm:vm-9003-disk-0",
				&pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1"},
			),
			wantStorage: "local-lvm",
			wantVolume:  "vm-9003-disk-0",
		},
		{
			name: "encoded pool-only meta wraps to same base",
			cid: pve.EncodeDiskCID(
				"data:vm-200-disk-0",
				&pve.DiskCIDMeta{Pool: "data"},
			),
			wantStorage: "data",
			wantVolume:  "vm-200-disk-0",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base, _, err := pve.ParseEncodedDiskCID(tc.cid)
			if err != nil {
				t.Fatalf("ParseEncodedDiskCID: %v", err)
			}
			storage, volume, err2 := pve.ParseDiskCID(base)
			if err2 != nil {
				t.Fatalf("ParseDiskCID: %v", err2)
			}
			if storage != tc.wantStorage {
				t.Errorf("storage: want %q, got %q", tc.wantStorage, storage)
			}
			if volume != tc.wantVolume {
				t.Errorf("volume: want %q, got %q", tc.wantVolume, volume)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DiskCIDMeta.Opts — per-disk performance options round-trip
// ---------------------------------------------------------------------------

// TestEncodeParseDiskCID_OptsRoundTrip encodes a meta with Opts set and verifies
// ParseEncodedDiskCID returns a deep-equal Opts map.
func TestEncodeParseDiskCID_OptsRoundTrip(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-9003-disk-0"
	meta := &pve.DiskCIDMeta{
		Pool: "local-lvm",
		Node: "pve1",
		AZ:   "z1",
		Opts: map[string]string{
			"iothread": "1",
			"cache":    "writeback",
		},
	}
	encoded := pve.EncodeDiskCID(bare, meta)

	if encoded == bare {
		t.Fatal("encoded CID should differ from bare when meta is non-empty")
	}

	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta == nil {
		t.Fatal("meta: want non-nil")
	}
	if len(gotMeta.Opts) != 2 {
		t.Fatalf("Opts len: want 2, got %d (%v)", len(gotMeta.Opts), gotMeta.Opts)
	}
	if gotMeta.Opts["iothread"] != "1" {
		t.Errorf("Opts[iothread]: want %q, got %q", "1", gotMeta.Opts["iothread"])
	}
	if gotMeta.Opts["cache"] != "writeback" {
		t.Errorf("Opts[cache]: want %q, got %q", "writeback", gotMeta.Opts["cache"])
	}
}

// TestEncodeDiskCID_NilOptsIdentical proves that nil Opts produces a CID
// byte-identical to encoding the same meta without Opts (omitempty guarantee).
// Also verifies the zero-meta short-circuit still applies when all fields are
// zero-valued including Opts.
func TestEncodeDiskCID_NilOptsIdentical(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"

	// Zero meta with nil Opts — must return bare unchanged.
	metaNilOpts := &pve.DiskCIDMeta{}
	if got := pve.EncodeDiskCID(bare, metaNilOpts); got != bare {
		t.Errorf("zero meta + nil Opts: want bare %q, got %q", bare, got)
	}

	// Non-zero meta: encoding with explicit nil Opts must equal encoding
	// without the Opts field (omitempty drops it from JSON).
	metaWith := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1", Opts: nil}
	metaWithout := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1"}
	if pve.EncodeDiskCID(bare, metaWith) != pve.EncodeDiskCID(bare, metaWithout) {
		t.Errorf("nil Opts must produce identical CID to omitted Opts field")
	}
}

// TestEncodeDiskCID_EmptyOptsIdentical proves an empty (non-nil) Opts map is
// treated as omitted by JSON omitempty, producing a CID identical to nil Opts.
func TestEncodeDiskCID_EmptyOptsIdentical(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"

	// Zero meta + empty map must return bare unchanged (zero-meta guard).
	metaEmptyOpts := &pve.DiskCIDMeta{Opts: map[string]string{}}
	if got := pve.EncodeDiskCID(bare, metaEmptyOpts); got != bare {
		t.Errorf("zero meta + empty Opts: want bare %q, got %q", bare, got)
	}

	// Non-zero meta + empty map must equal non-zero meta + nil Opts.
	metaEmpty := &pve.DiskCIDMeta{Pool: "local-lvm", Opts: map[string]string{}}
	metaNil := &pve.DiskCIDMeta{Pool: "local-lvm"}
	if pve.EncodeDiskCID(bare, metaEmpty) != pve.EncodeDiskCID(bare, metaNil) {
		t.Errorf("empty Opts map must produce identical CID to nil Opts")
	}
}

// ---------------------------------------------------------------------------
// EmbeddedDiskVMID
// ---------------------------------------------------------------------------

func TestEmbeddedDiskVMID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		volid    string
		wantVMID int
		wantOK   bool
	}{
		{
			name:     "flat lvm volid with foreign vmid",
			volid:    "zfs-1:vm-15689-disk-0",
			wantVMID: 15689,
			wantOK:   true,
		},
		{
			name:     "flat lvm volid no options",
			volid:    "local-lvm:vm-100-disk-2",
			wantVMID: 100,
			wantOK:   true,
		},
		{
			name:     "path form dir volid",
			volid:    "dir:100/vm-100-disk-0.qcow2",
			wantVMID: 100,
			wantOK:   true,
		},
		{
			name:     "cloudinit volid returns false",
			volid:    "local-lvm:vm-100-cloudinit",
			wantVMID: 0,
			wantOK:   false,
		},
		{
			name:     "none (cdrom) returns false",
			volid:    "none",
			wantVMID: 0,
			wantOK:   false,
		},
		{
			name:     "iso volid returns false",
			volid:    "local:iso/x.iso",
			wantVMID: 0,
			wantOK:   false,
		},
		{
			name:     "empty string returns false",
			volid:    "",
			wantVMID: 0,
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVMID, gotOK := pve.EmbeddedDiskVMID(tc.volid)
			if gotOK != tc.wantOK {
				t.Errorf("EmbeddedDiskVMID(%q): ok=%v, want %v", tc.volid, gotOK, tc.wantOK)
			}
			if gotVMID != tc.wantVMID {
				t.Errorf("EmbeddedDiskVMID(%q): vmid=%d, want %d", tc.volid, gotVMID, tc.wantVMID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FindForeignActiveDisks
// ---------------------------------------------------------------------------

func TestFindForeignActiveDisks(t *testing.T) {
	t.Parallel()

	// ownerVMID=6031; virtio0 and scsi2 are owned disks; scsi1 is a foreign
	// persistent disk (vmid 15689); ide2 is cloudinit (no vm-N-disk-N label,
	// skipped); unused0 is NOT an active bus slot (skipped by ParseDisks).
	cfg := map[string]any{
		"virtio0": "zfs-1:vm-6031-disk-0",
		"scsi1":   "zfs-1:vm-15689-disk-0,size=128G",
		"scsi2":   "zfs-1:vm-6031-disk-1",
		"ide2":    "local-lvm:vm-6031-cloudinit",
		"unused0": "zfs-1:vm-9999-disk-0",
	}

	got := pve.FindForeignActiveDisks(cfg, 6031)

	if len(got) != 1 {
		t.Fatalf("FindForeignActiveDisks: want 1 entry, got %d: %v", len(got), got)
	}
	bareVolid, ok := got["scsi1"]
	if !ok {
		t.Fatalf("FindForeignActiveDisks: expected scsi1 in result; got %v", got)
	}
	const wantBare = "zfs-1:vm-15689-disk-0"
	if bareVolid != wantBare {
		t.Errorf("FindForeignActiveDisks scsi1: want %q, got %q", wantBare, bareVolid)
	}
	// Confirm unused0 NOT returned (it is not an active bus slot).
	if _, found := got["unused0"]; found {
		t.Error("FindForeignActiveDisks: unused0 must not appear in result (not an active bus slot)")
	}
}

// ---------------------------------------------------------------------------
// FindUnusedDiskEntries
// ---------------------------------------------------------------------------

func TestFindUnusedDiskEntries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cfg      map[string]any
		wantKeys []string          // expected slot keys in result
		wantVals map[string]string // expected slot→bare-volid mapping
		wantLen  int               // expected result length
	}{
		{
			name:    "empty config map returns empty result",
			cfg:     map[string]any{},
			wantLen: 0,
		},
		{
			name: "one unused0 entry parsed correctly",
			cfg: map[string]any{
				"unused0": "local-lvm:vm-100-disk-0,size=10G",
			},
			wantLen:  1,
			wantKeys: []string{"unused0"},
			wantVals: map[string]string{"unused0": "local-lvm:vm-100-disk-0"},
		},
		{
			name: "multiple unusedN entries all returned in slot-keyed map",
			cfg: map[string]any{
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
			cfg: map[string]any{
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
			cfg: map[string]any{
				"unused0": "local-lvm:vm-100-disk-0,iothread=1,cache=writeback",
			},
			wantLen:  1,
			wantVals: map[string]string{"unused0": "local-lvm:vm-100-disk-0"},
		},
		{
			name: "malformed unused entry without storage colon is skipped (bare value kept)",
			cfg: map[string]any{
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
