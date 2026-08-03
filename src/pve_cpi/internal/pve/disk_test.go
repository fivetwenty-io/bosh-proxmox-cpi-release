package pve_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
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

// mustEncodeDiskCID calls pve.EncodeDiskCID and fails the test on error. Every
// call site in this file passes a non-empty bareCID, so an error here always
// indicates a real regression, not an expected failure path (those are
// covered by TestEncodeDiskCID_EmptyBareCID).
func mustEncodeDiskCID(t *testing.T, bareCID string, meta *pve.DiskCIDMeta) string {
	t.Helper()
	got, err := pve.EncodeDiskCID(bareCID, meta)
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): unexpected error: %v", bareCID, err)
	}
	return got
}

// mustEncodeDiskCIDCompressed is the EncodeDiskCIDCompressed counterpart of
// mustEncodeDiskCID.
func mustEncodeDiskCIDCompressed(t *testing.T, bareCID string, meta *pve.DiskCIDMeta) string {
	t.Helper()
	got, err := pve.EncodeDiskCIDCompressed(bareCID, meta)
	if err != nil {
		t.Fatalf("EncodeDiskCIDCompressed(%q): unexpected error: %v", bareCID, err)
	}
	return got
}

// TestEncodeDiskCID_EmptyBareCIDErrors verifies round-trip totality: encoding
// an empty bare CID is a programming error in the caller and must be rejected
// rather than silently producing an envelope that decodes to an empty volid.
func TestEncodeDiskCID_EmptyBareCIDErrors(t *testing.T) {
	t.Parallel()
	if _, err := pve.EncodeDiskCID("", nil); err == nil {
		t.Fatal("expected error for empty bareCID")
	}
	if _, err := pve.EncodeDiskCID("", &pve.DiskCIDMeta{Pool: "local"}); err == nil {
		t.Fatal("expected error for empty bareCID even with non-nil meta")
	}
}

// TestEncodeDiskCIDCompressed_EmptyBareCIDErrors is the EncodeDiskCIDCompressed
// counterpart of TestEncodeDiskCID_EmptyBareCIDErrors.
func TestEncodeDiskCIDCompressed_EmptyBareCIDErrors(t *testing.T) {
	t.Parallel()
	if _, err := pve.EncodeDiskCIDCompressed("", nil); err == nil {
		t.Fatal("expected error for empty bareCID")
	}
}

// TestParseEncodedDiskCID_UnknownPrefixHardErrors verifies that any CID
// lacking the pvd- or pvz- envelope prefix — garbage, a random string, or a
// prefix that merely resembles the envelope markers — is a hard parse error.
func TestParseEncodedDiskCID_UnknownPrefixHardErrors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"garbage",
		"xyz-abc123",
		"pv-abc123",   // one character short of "pvd-"/"pvz-"
		"pvda-abc123", // superset, not an exact prefix match issue but still no valid decode
		"   ",
	}
	for _, cid := range cases {
		if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
			t.Errorf("ParseEncodedDiskCID(%q): expected error, got success", cid)
		}
	}
}

func TestEncodeDiskCID_NilMeta(t *testing.T) {
	t.Parallel()
	// Even without meta the CID is wrapped: path-form bare volids contain "/"
	// which breaks the Director's /disks/<cid>/attachments route.
	bare := "local-lvm:vm-100-disk-0"
	got := mustEncodeDiskCID(t, bare, nil)
	if got == bare {
		t.Fatalf("nil meta: want wrapped CID, got bare %q", got)
	}
	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(got)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta != nil {
		t.Errorf("meta: want nil, got %+v", gotMeta)
	}
}

func TestEncodeDiskCID_ZeroMeta(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"
	meta := &pve.DiskCIDMeta{}
	got := mustEncodeDiskCID(t, bare, meta)
	if got == bare {
		t.Fatalf("zero meta: want wrapped CID, got bare %q", got)
	}
	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(got)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta != nil {
		t.Errorf("meta: want nil (all-zero meta is omitted), got %+v", gotMeta)
	}
}

// TestParseEncodedDiskCID_BareVolidRejected verifies that a bare PVE volid
// (no pvd-/pvz- envelope prefix) is a hard parse error. Pre-release software
// carries no backward-compatibility requirement for the format emitted before
// the envelope was introduced.
func TestParseEncodedDiskCID_BareVolidRejected(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseEncodedDiskCID("local-lvm:vm-100-disk-0")
	if err == nil {
		t.Fatal("expected error for a bare (unenveloped) disk CID")
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
	encoded := mustEncodeDiskCID(t, bare, meta)

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
	encoded := mustEncodeDiskCID(t, bare, meta)

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

func TestParseEncodedDiskCID_BaseStillParseable(t *testing.T) {
	t.Parallel()
	// Round-trip: encoded CID base must still pass ParseDiskCID.
	bare := "local-lvm:vm-9003-disk-0"
	meta := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve2", AZ: "az-a"}
	encoded := mustEncodeDiskCID(t, bare, meta)

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

// ---------------------------------------------------------------------------
// pvd- envelope format (REST-path-safe CIDs)
// ---------------------------------------------------------------------------

// TestEncodeDiskCID_CharsetSafe verifies the emitted CID uses only characters
// safe in a URI path segment and in bosh CLI argument passthrough: no ":",
// "/", "|", "+", or "=" may appear, even for path-form volids with full
// per-disk performance options.
func TestEncodeDiskCID_CharsetSafe(t *testing.T) {
	t.Parallel()
	bare := "nfs-slow:9001/vm-9001-disk-0.qcow2"
	meta := &pve.DiskCIDMeta{
		Pool: "nfs-slow",
		Node: "pve1",
		AZ:   "z1",
		Opts: map[string]string{
			"iothread": "1",
			"cache":    "writeback",
			"discard":  "on",
			"ssd":      "1",
			"mbps_rd":  "100",
			"mbps_wr":  "100",
			"iops_rd":  "5000",
			"iops_wr":  "5000",
		},
	}
	got := mustEncodeDiskCID(t, bare, meta)
	if !strings.HasPrefix(got, "pvd-") {
		t.Fatalf("want pvd- prefix, got %q", got)
	}
	if strings.ContainsAny(got, ":/|+=") {
		t.Errorf("emitted CID contains REST-hostile characters: %q", got)
	}
}

// TestEncodeParseDiskCID_PathFormRoundTrip covers file/qcow storage where the
// PVE volid embeds "/" and "." — the exact shape that 404s the Director's
// /disks/<cid>/attachments route when emitted raw.
func TestEncodeParseDiskCID_PathFormRoundTrip(t *testing.T) {
	t.Parallel()
	bare := "local:9001/vm-9001-disk-0.qcow2"
	meta := &pve.DiskCIDMeta{Pool: "local", Node: "pve2"}
	encoded := mustEncodeDiskCID(t, bare, meta)

	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(encoded)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != bare {
		t.Errorf("base: want %q, got %q", bare, gotBase)
	}
	if gotMeta == nil || gotMeta.Pool != "local" || gotMeta.Node != "pve2" {
		t.Errorf("meta: want Pool=local Node=pve2, got %+v", gotMeta)
	}
}

// TestParseEncodedDiskCID_LegacyPipeSuffixRejected verifies that the wire
// format emitted by releases before the pvd- envelope ("<storage>:<volid>|
// <base64>") is now a hard parse error: this package carries no
// backward-compatibility requirement, and the legacy fallback path has been
// removed entirely.
func TestParseEncodedDiskCID_LegacyPipeSuffixRejected(t *testing.T) {
	t.Parallel()
	suffix := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"pool":"data","node":"pve1","az":"z1"}`),
	)
	cid := "data:vm-9897-disk-0|" + suffix
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for legacy pipe-suffixed CID")
	}
}

// TestParseEncodedDiskCID_PvdNamedStorageHardErrors covers a PVE storage
// literally named with a "pvd-" prefix: its bare CID starts with "pvd-" but
// contains ":" (outside the base64url alphabet). With the legacy fallback
// removed, this now surfaces as a hard parse error rather than being silently
// treated as a bare legacy CID — an edge case operators must avoid by not
// naming a PVE storage "pvd-…" or "pvz-…".
func TestParseEncodedDiskCID_PvdNamedStorageHardErrors(t *testing.T) {
	t.Parallel()
	if _, _, err := pve.ParseEncodedDiskCID("pvd-foo:vm-100-disk-0"); err == nil {
		t.Fatal("expected error for a pvd- prefixed CID whose payload is not valid base64url")
	}
}

func TestParseEncodedDiskCID_PvdEmptyPayload(t *testing.T) {
	t.Parallel()
	if _, _, err := pve.ParseEncodedDiskCID("pvd-"); err == nil {
		t.Fatal("expected error for empty pvd payload")
	}
}

func TestParseEncodedDiskCID_PvdBadBase64(t *testing.T) {
	t.Parallel()
	// No colon anywhere, so no legacy fallback applies: the CID was meant to
	// be a pvd envelope and its corruption must surface as an error.
	if _, _, err := pve.ParseEncodedDiskCID("pvd-!!!notbase64"); err == nil {
		t.Fatal("expected error for malformed pvd base64 payload")
	}
}

func TestParseEncodedDiskCID_PvdBadJSON(t *testing.T) {
	t.Parallel()
	cid := "pvd-" + base64.RawURLEncoding.EncodeToString([]byte("notjson"))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for pvd payload that is not JSON")
	}
}

func TestParseEncodedDiskCID_PvdEmptyVolid(t *testing.T) {
	t.Parallel()
	cid := "pvd-" + base64.RawURLEncoding.EncodeToString([]byte(`{"m":{"pool":"data"}}`))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for pvd envelope with empty volid")
	}
}

func TestParseEncodedDiskCID_PvdNullMeta(t *testing.T) {
	t.Parallel()
	cid := "pvd-" + base64.RawURLEncoding.EncodeToString([]byte(`{"v":"data:vm-1-disk-0","m":null}`))
	gotBase, gotMeta, err := pve.ParseEncodedDiskCID(cid)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBase != "data:vm-1-disk-0" {
		t.Errorf("base: want %q, got %q", "data:vm-1-disk-0", gotBase)
	}
	if gotMeta != nil {
		t.Errorf("meta: want nil, got %+v", gotMeta)
	}
}

// TestEncodeDiskCID_TypicalLength keeps the common-case CID comfortably under
// MySQL-backed Directors' 255-char disk_cid column.
func TestEncodeDiskCID_TypicalLength(t *testing.T) {
	t.Parallel()
	got := mustEncodeDiskCID(t,
		"local-lvm:vm-90001-disk-0",
		&pve.DiskCIDMeta{Pool: "local-lvm", Node: "lab-pmx-0", AZ: "z1"},
	)
	if len(got) > 255 {
		t.Errorf("typical CID length %d exceeds 255: %q", len(got), got)
	}
}

// ---------------------------------------------------------------------------
// pvz- compressed envelope (opt-in disk_cid_compression)

// bigDiskCIDMeta returns a meta whose pvd- encoding exceeds 255 characters:
// the shape that motivates the compressed format on MySQL-backed Directors.
func bigDiskCIDMeta() (string, *pve.DiskCIDMeta) {
	bare := "ceph-rbd-nvme-tier1:300/vm-300-disk-0.qcow2"
	return bare, &pve.DiskCIDMeta{
		Pool: "ceph-rbd-nvme-tier1",
		Node: "prod-pmx-node-07",
		AZ:   "az-rack-2",
		Opts: map[string]string{
			"iothread": "1",
			"cache":    "writeback",
			"discard":  "on",
			"ssd":      "1",
			"mbps_rd":  "120",
			"mbps_wr":  "120",
			"iops_rd":  "8000",
			"iops_wr":  "8000",
		},
	}
}

// gzipPayload gzips data and returns the "pvz-" CID form, for building decode
// fixtures without going through the production encoder.
func gzipPayload(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return "pvz-" + base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

// TestEncodeDiskCIDCompressed_SmallStaysPvd: compression must be conditional.
// A CID whose pvd- form fits in 255 characters is emitted uncompressed so the
// common case stays byte-identical to EncodeDiskCID and operator-inspectable.
func TestEncodeDiskCIDCompressed_SmallStaysPvd(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-90001-disk-0"
	meta := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "lab-pmx-0", AZ: "z1"}
	got := mustEncodeDiskCIDCompressed(t, bare, meta)
	want := mustEncodeDiskCID(t, bare, meta)
	if got != want {
		t.Errorf("small CID must match EncodeDiskCID output: got %q want %q", got, want)
	}
}

// TestEncodeDiskCIDCompressed_LargeEmitsPvz: when the pvd- form exceeds 255
// characters the encoder switches to the gzip envelope, the result fits the
// MySQL varchar(255) disk_cid column, and it round-trips bare CID and meta.
func TestEncodeDiskCIDCompressed_LargeEmitsPvz(t *testing.T) {
	t.Parallel()
	bare, meta := bigDiskCIDMeta()
	if plain := mustEncodeDiskCID(t, bare, meta); len(plain) <= 255 {
		t.Fatalf("test premise broken: pvd form is %d chars, need >255", len(plain))
	}
	got := mustEncodeDiskCIDCompressed(t, bare, meta)
	if !strings.HasPrefix(got, "pvz-") {
		t.Fatalf("want pvz- prefix, got %q", got)
	}
	if len(got) > 255 {
		t.Errorf("compressed CID length %d exceeds 255: %q", len(got), got)
	}
	gotBare, gotMeta, err := pve.ParseEncodedDiskCID(got)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBare != bare {
		t.Errorf("base: want %q, got %q", bare, gotBare)
	}
	if gotMeta == nil || gotMeta.Pool != meta.Pool || gotMeta.Node != meta.Node || gotMeta.AZ != meta.AZ {
		t.Fatalf("meta: want %+v, got %+v", meta, gotMeta)
	}
	if len(gotMeta.Opts) != len(meta.Opts) {
		t.Fatalf("opts: want %d entries, got %+v", len(meta.Opts), gotMeta.Opts)
	}
	for k, v := range meta.Opts {
		if gotMeta.Opts[k] != v {
			t.Errorf("opts[%q]: want %q, got %q", k, v, gotMeta.Opts[k])
		}
	}
}

// TestEncodeDiskCIDCompressed_CharsetSafe: the pvz- form must honor the same
// charset guarantee as pvd- — no ":", "/", "|", "+", or "=".
func TestEncodeDiskCIDCompressed_CharsetSafe(t *testing.T) {
	t.Parallel()
	bare, meta := bigDiskCIDMeta()
	got := mustEncodeDiskCIDCompressed(t, bare, meta)
	if strings.ContainsAny(got, ":/|+=") {
		t.Errorf("emitted CID contains REST-hostile characters: %q", got)
	}
}

// TestEncodeDiskCIDCompressed_ThresholdBoundary walks storage-name lengths to
// find the largest pvd- form at or under 255 chars and the smallest over it,
// then asserts the encoder switches format exactly there.
func TestEncodeDiskCIDCompressed_ThresholdBoundary(t *testing.T) {
	t.Parallel()
	build := func(n int) (string, *pve.DiskCIDMeta) {
		storage := strings.Repeat("s", n)
		return storage + ":vm-1-disk-0", &pve.DiskCIDMeta{Pool: storage}
	}
	lastFit, firstOver := -1, -1
	for n := 1; n <= 400; n++ {
		bare, meta := build(n)
		l := len(mustEncodeDiskCID(t, bare, meta))
		if l <= 255 {
			lastFit = n
		} else if firstOver == -1 {
			firstOver = n
			break
		}
	}
	if lastFit == -1 || firstOver == -1 {
		t.Fatalf("could not bracket the 255 boundary (lastFit=%d firstOver=%d)", lastFit, firstOver)
	}

	bare, meta := build(lastFit)
	if got := mustEncodeDiskCIDCompressed(t, bare, meta); !strings.HasPrefix(got, "pvd-") {
		t.Errorf("pvd form of %d chars fits; want pvd- output, got %q", len(mustEncodeDiskCID(t, bare, meta)), got)
	}
	bare, meta = build(firstOver)
	if got := mustEncodeDiskCIDCompressed(t, bare, meta); !strings.HasPrefix(got, "pvz-") {
		t.Errorf("pvd form of %d chars is over the limit; want pvz- output, got %q", len(mustEncodeDiskCID(t, bare, meta)), got)
	}
}

// TestEncodeDiskCIDCompressed_IncompressiblePrefersPlain: gzip inflates
// high-entropy payloads. When the compressed form would be no shorter than the
// plain form, the encoder must keep the plain pvd- CID (both are over the
// limit; the warn fires either way, and the shorter, inspectable form wins).
func TestEncodeDiskCIDCompressed_IncompressiblePrefersPlain(t *testing.T) {
	t.Parallel()
	// Fixed high-entropy strings (base64 of random bytes, generated once).
	storage := "Zq3xK9mWp2Lr8vTn5cYd1Bf7Hs4Ej6Ug0QaXwOiNkMzRlPyAoJhFbCtDeSvGxIu" +
		"VrEw2nT8mK5pL3qZ9dX7cB1fY4hS6jE0gU5aQ8wO2iN4kM6zR1lP3yA7oJ9hF2bC"
	bare := storage + ":vm-1-disk-0"
	meta := &pve.DiskCIDMeta{Pool: storage, Opts: map[string]string{
		"a": "Xk2mZ9qW", "b": "Tn5cYd1B", "c": "Hs4Ej6Ug", "d": "QaXwOiNk",
	}}
	plain := mustEncodeDiskCID(t, bare, meta)
	if len(plain) <= 255 {
		t.Fatalf("test premise broken: pvd form is %d chars, need >255", len(plain))
	}
	got := mustEncodeDiskCIDCompressed(t, bare, meta)
	if len(got) > len(plain) {
		t.Errorf("encoder emitted a longer CID than the plain form: %d > %d", len(got), len(plain))
	}
	if strings.HasPrefix(got, "pvz-") && len(got) >= len(plain) {
		t.Errorf("pvz form not shorter than pvd (%d vs %d); plain must win", len(got), len(plain))
	}
}

// TestParseEncodedDiskCID_PvzFrozenFixture pins the compressed wire format:
// this literal was generated by an independent gzip implementation (Python,
// mtime=0) and must decode forever, regardless of how Go's gzip encoder output
// evolves across versions.
func TestParseEncodedDiskCID_PvzFrozenFixture(t *testing.T) {
	t.Parallel()
	const fixture = "pvz-H4sIAAAAAAAC_6tWKlOyUsrJT07M0c0py7Uqy9U1NjDQTcksztY1UNJRylWyqlYqyM_PQValVFsLABA25Zc4AAAA"
	gotBare, gotMeta, err := pve.ParseEncodedDiskCID(fixture)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if gotBare != "local-lvm:vm-300-disk-0" {
		t.Errorf("base: want %q, got %q", "local-lvm:vm-300-disk-0", gotBare)
	}
	if gotMeta == nil || gotMeta.Pool != "local-lvm" {
		t.Errorf("meta: want Pool=local-lvm, got %+v", gotMeta)
	}
}

// TestParseEncodedDiskCID_PvzNamedStorageHardErrors mirrors the pvd- rule: a
// PVE storage literally named "pvz-…" produces a bare CID containing ":",
// which can never be base64url. With the legacy fallback removed, this is now
// a hard parse error.
func TestParseEncodedDiskCID_PvzNamedStorageHardErrors(t *testing.T) {
	t.Parallel()
	if _, _, err := pve.ParseEncodedDiskCID("pvz-foo:vm-100-disk-0"); err == nil {
		t.Fatal("expected error for a pvz- prefixed CID whose payload is not valid base64url")
	}
}

func TestParseEncodedDiskCID_PvzEmptyPayload(t *testing.T) {
	t.Parallel()
	if _, _, err := pve.ParseEncodedDiskCID("pvz-"); err == nil {
		t.Fatal("expected error for empty pvz payload")
	}
}

func TestParseEncodedDiskCID_PvzBadBase64(t *testing.T) {
	t.Parallel()
	if _, _, err := pve.ParseEncodedDiskCID("pvz-!!!notbase64"); err == nil {
		t.Fatal("expected error for malformed pvz base64 payload")
	}
}

func TestParseEncodedDiskCID_PvzNotGzip(t *testing.T) {
	t.Parallel()
	cid := "pvz-" + base64.RawURLEncoding.EncodeToString([]byte("plainbytesnotgzip"))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for pvz payload that is not a gzip stream")
	}
}

func TestParseEncodedDiskCID_PvzBadJSONInsideGzip(t *testing.T) {
	t.Parallel()
	cid := gzipPayload(t, []byte("notjson"))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for pvz payload whose gzip content is not JSON")
	}
}

func TestParseEncodedDiskCID_PvzEmptyVolid(t *testing.T) {
	t.Parallel()
	cid := gzipPayload(t, []byte(`{"m":{"pool":"data"}}`))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for pvz envelope with empty volid")
	}
}

// TestParseEncodedDiskCID_PvzDecompressionBombRejected: a hostile CID that
// gzips megabytes of data into a short payload must be rejected by the
// decompression size cap, not expanded in memory.
func TestParseEncodedDiskCID_PvzDecompressionBombRejected(t *testing.T) {
	t.Parallel()
	cid := gzipPayload(t, bytes.Repeat([]byte("0"), 10<<20))
	if _, _, err := pve.ParseEncodedDiskCID(cid); err == nil {
		t.Fatal("expected error for oversized decompressed envelope")
	}
}

// Table-driven tests cover all call-site wrapping patterns: ParseEncodedDiskCID
// strips the envelope, then ParseDiskCID on the resulting bare CID yields the
// expected storage/volume. Every case here is an envelope CID — bare
// (unenveloped) input is covered separately by
// TestParseEncodedDiskCID_BareVolidRejected since it is now a hard error.
func TestCallSiteWrapperBehaviour(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cid         string // always an encoded (pvd-/pvz-) CID
		wantStorage string
		wantVolume  string
	}{
		{
			name: "encoded full meta wraps to same base",
			cid: mustEncodeDiskCID(t,
				"local-lvm:vm-9003-disk-0",
				&pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1"},
			),
			wantStorage: "local-lvm",
			wantVolume:  "vm-9003-disk-0",
		},
		{
			name: "encoded pool-only meta wraps to same base",
			cid: mustEncodeDiskCID(t,
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
	encoded := mustEncodeDiskCID(t, bare, meta)

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
// Also verifies the zero-meta case omits the meta payload entirely, matching
// a nil-meta encode byte for byte.
func TestEncodeDiskCID_NilOptsIdentical(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"

	// Zero meta with nil Opts — meta is omitted; identical to nil-meta encode.
	metaNilOpts := &pve.DiskCIDMeta{}
	if got := mustEncodeDiskCID(t, bare, metaNilOpts); got != mustEncodeDiskCID(t, bare, nil) {
		t.Errorf("zero meta + nil Opts: want nil-meta encoding, got %q", got)
	}

	// Non-zero meta: encoding with explicit nil Opts must equal encoding
	// without the Opts field (omitempty drops it from JSON).
	metaWith := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1", Opts: nil}
	metaWithout := &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1", AZ: "z1"}
	if mustEncodeDiskCID(t, bare, metaWith) != mustEncodeDiskCID(t, bare, metaWithout) {
		t.Errorf("nil Opts must produce identical CID to omitted Opts field")
	}
}

// TestEncodeDiskCID_EmptyOptsIdentical proves an empty (non-nil) Opts map is
// treated as omitted by JSON omitempty, producing a CID identical to nil Opts.
func TestEncodeDiskCID_EmptyOptsIdentical(t *testing.T) {
	t.Parallel()
	bare := "local-lvm:vm-100-disk-0"

	// Zero meta + empty map — meta is omitted; identical to nil-meta encode.
	metaEmptyOpts := &pve.DiskCIDMeta{Opts: map[string]string{}}
	if got := mustEncodeDiskCID(t, bare, metaEmptyOpts); got != mustEncodeDiskCID(t, bare, nil) {
		t.Errorf("zero meta + empty Opts: want nil-meta encoding, got %q", got)
	}

	// Non-zero meta + empty map must equal non-zero meta + nil Opts.
	metaEmpty := &pve.DiskCIDMeta{Pool: "local-lvm", Opts: map[string]string{}}
	metaNil := &pve.DiskCIDMeta{Pool: "local-lvm"}
	if mustEncodeDiskCID(t, bare, metaEmpty) != mustEncodeDiskCID(t, bare, metaNil) {
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

// ---------------------------------------------------------------------------
// FindVMPoolViaCluster
// ---------------------------------------------------------------------------

// TestFindVMPoolViaCluster_ReturnsPool verifies the member, non-member, and
// absent-vmid cases from a single /cluster/resources scan.
func TestFindVMPoolViaCluster_ReturnsPool(t *testing.T) {
	t.Parallel()

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(
					map[string]any{"vmid": int64(101), "node": "pve-01", "pool": "bosh"},
					map[string]any{"vmid": int64(102), "node": "pve-01"}, // no pool field
				), nil
			},
		},
	}

	pool, found, err := pve.FindVMPoolViaCluster(context.Background(), c, 101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a row present in the scan")
	}
	if pool != "bosh" {
		t.Errorf("pool: want %q, got %q", "bosh", pool)
	}

	// Row present but without a pool field: found=true, pool="".
	pool, found, err = pve.FindVMPoolViaCluster(context.Background(), c, 102)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a row present without pool membership")
	}
	if pool != "" {
		t.Errorf("pool: want empty string for non-member VM, got %q", pool)
	}

	// vmid absent from the scan entirely: found=false, pool="".
	pool, found, err = pve.FindVMPoolViaCluster(context.Background(), c, 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a vmid absent from the scan")
	}
	if pool != "" {
		t.Errorf("pool: want empty string when not found, got %q", pool)
	}
}

// TestFindVMPoolViaCluster_NilClusterService verifies the nil-Cluster-service
// guard (unit-test mocks that don't wire a cluster service) reports
// not-found rather than panicking.
func TestFindVMPoolViaCluster_NilClusterService(t *testing.T) {
	t.Parallel()
	c := &diskClusterClient{clusterSvc: nil}
	pool, found, err := pve.FindVMPoolViaCluster(context.Background(), c, 101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found || pool != "" {
		t.Errorf("expected (\"\", false, nil) with nil cluster service, got (%q, %v, %v)", pool, found, err)
	}
}

// TestFindVMPoolViaCluster_TransportError verifies a permanent ListResources
// failure propagates as an error rather than being swallowed into a false
// not-found.
func TestFindVMPoolViaCluster_TransportError(t *testing.T) {
	t.Parallel()
	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return nil, errors.New("boom: permanent failure")
			},
		},
	}
	_, found, err := pve.FindVMPoolViaCluster(context.Background(), c, 101)
	if err == nil {
		t.Fatal("expected error on transport failure")
	}
	if found {
		t.Error("expected found=false on error")
	}
}
