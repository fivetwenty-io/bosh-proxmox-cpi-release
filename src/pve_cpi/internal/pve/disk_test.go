package pve_test

import (
	"context"
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
