package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// --------------------------------------------------------------------------
// fakeStorageSvc — implements sdkstorage.Service for ConfigDrive tests.
// --------------------------------------------------------------------------

type fakeStorageSvc struct {
	uploadFn               func(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error)
	deleteVolumeIfExistsFn func(ctx context.Context, node, storage, volume string) (bool, error)
	deleteFn               func(ctx context.Context, node, storage, volume string) error

	uploadCalls               []string // filename arg
	deleteVolumeIfExistsCalls []string // volume arg
	deleteCalls               []string // volume arg
}

func (f *fakeStorageSvc) CreateVolume(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
	panic("fakeStorageSvc.CreateVolume: not expected")
}

func (f *fakeStorageSvc) DeleteVolume(ctx context.Context, node, storage, volume string) error {
	f.deleteCalls = append(f.deleteCalls, volume)
	if f.deleteFn != nil {
		return f.deleteFn(ctx, node, storage, volume)
	}
	return nil
}

func (f *fakeStorageSvc) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	f.deleteVolumeIfExistsCalls = append(f.deleteVolumeIfExistsCalls, volume)
	if f.deleteVolumeIfExistsFn != nil {
		return f.deleteVolumeIfExistsFn(ctx, node, storage, volume)
	}
	// Default: volume existed and was deleted.
	return true, nil
}

func (f *fakeStorageSvc) Exists(_ context.Context, _, _, _ string) (bool, error) {
	panic("fakeStorageSvc.Exists: not expected")
}

func (f *fakeStorageSvc) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	f.uploadCalls = append(f.uploadCalls, filename)
	if f.uploadFn != nil {
		return f.uploadFn(ctx, node, storage, content, filename, body)
	}
	return "", nil
}

// Compile-time check.
var _ sdkstorage.Service = (*fakeStorageSvc)(nil)

// --------------------------------------------------------------------------
// fakeNodesSvc — implements the subset of sdknodes.Service used by ConfigDrive.
// --------------------------------------------------------------------------

type fakeNodesSvc struct {
	sdknodes.Service  // embed nil — panics on unexpected calls
	updateConfigFn    func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	updateConfigCalls []updateConfigCall
}

type updateConfigCall struct {
	node   string
	vmid   string
	params *sdknodes.UpdateQemuConfigParams
}

func (f *fakeNodesSvc) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	f.updateConfigCalls = append(f.updateConfigCalls, updateConfigCall{node, vmid, params})
	if f.updateConfigFn != nil {
		return f.updateConfigFn(ctx, node, vmid, params)
	}
	return nil
}

// --------------------------------------------------------------------------
// fakePVEClient — implements pve.Client for ConfigDrive tests.
// --------------------------------------------------------------------------

type fakePVEClient struct {
	pve.Client // embed nil — panics on unexpected methods
	storageSvc *fakeStorageSvc
	nodesSvc   *fakeNodesSvc
}

func (f *fakePVEClient) Storage() sdkstorage.Service               { return f.storageSvc }
func (f *fakePVEClient) Nodes() sdknodes.Service                   { return f.nodesSvc }
func (f *fakePVEClient) ClusterStorage() sdkclusterstorage.Service { return nil }

// newISOAgent builds a ConfigDrive backed by the provided fake services.
// If storageSvc or nodesSvc is nil, a no-op default is used.
func newISOAgent(storageSvc *fakeStorageSvc, nodesSvc *fakeNodesSvc) *ConfigDrive {
	if storageSvc == nil {
		storageSvc = &fakeStorageSvc{}
	}
	if nodesSvc == nil {
		nodesSvc = &fakeNodesSvc{}
	}
	client := &fakePVEClient{storageSvc: storageSvc, nodesSvc: nodesSvc}
	return newConfigDriveForTest(client, "local", log.NewNopLogger())
}

func baseISOConfig() AgentConfig {
	return AgentConfig{
		AgentID: "agent-iso-1",
		VM:      VMSpec{Name: "vm-200", ID: "200"},
		Networks: map[string]NetworkSpec{
			"default": {Type: "manual", IP: "10.0.0.5", Netmask: "255.255.255.0"},
		},
		Disks: DisksSpec{System: "/dev/sda", Ephemeral: "/dev/sdb"},
		Env:   map[string]any{},
		MBus:  "nats://mbus:4222",
		Blobstore: BlobstoreSpec{
			Provider: "dav",
			Options:  map[string]any{"endpoint": "https://10.0.0.1:25250"},
		},
		NTP: []string{"0.pool.ntp.org"},
	}
}

// readSettingsFromISO opens the ISO at path, reads /ec2/latest/user-data, and
// unmarshals it into settingsJSON. Also returns the volume label.
func readSettingsFromISO(t *testing.T, path string) (settingsJSON, string) {
	t.Helper()
	d, err := diskfs.Open(path)
	if err != nil {
		t.Fatalf("diskfs.Open(%s): %v", path, err)
	}
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatalf("GetFilesystem: %v", err)
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		t.Fatalf("not iso9660: %T", fs)
	}
	label := iso.Label()
	f, err := iso.OpenFile("/ec2/latest/user-data", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile user-data: %v", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read user-data: %v", err)
	}
	var s settingsJSON
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("unmarshal settings: %v\nraw=%q", err, string(data))
	}
	return s, label
}

func TestConfigDrive_Configure_BuildsISOWithCorrectLayout(t *testing.T) {
	t.Parallel()

	var uploadedPath string
	storageSvc := &fakeStorageSvc{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			tmp, err := os.CreateTemp("", "uploaded-*.iso")
			if err != nil {
				return "", err
			}
			defer tmp.Close()
			if _, err := io.Copy(tmp, body); err != nil {
				return "", err
			}
			uploadedPath = tmp.Name()
			return "", nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	defer func() {
		if uploadedPath != "" {
			_ = os.Remove(uploadedPath)
		}
	}()

	if err := a.Configure(context.Background(), "pve1", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if uploadedPath == "" {
		t.Fatal("upload not invoked")
	}
	s, label := readSettingsFromISO(t, uploadedPath)
	cleaned := strings.ToLower(strings.TrimSpace(strings.Trim(label, "\x00")))
	if cleaned != "config-2" {
		t.Errorf("volume label = %q, want config-2", cleaned)
	}
	if s.AgentID != "agent-iso-1" {
		t.Errorf("settings.agent_id = %q, want agent-iso-1", s.AgentID)
	}
	if s.MBus != "nats://mbus:4222" {
		t.Errorf("settings.mbus = %q, want nats://mbus:4222", s.MBus)
	}
}

func TestConfigDrive_Configure_AttachesAsScsi30Cdrom(t *testing.T) {
	t.Parallel()

	nodesSvc := &fakeNodesSvc{}
	a := newISOAgent(nil, nodesSvc)
	if err := a.Configure(context.Background(), "pve1", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if len(nodesSvc.updateConfigCalls) != 1 {
		t.Fatalf("expected 1 UpdateQemuConfig call, got %d", len(nodesSvc.updateConfigCalls))
	}
	call := nodesSvc.updateConfigCalls[0]
	if call.node != "pve1" {
		t.Errorf("node = %q, want pve1", call.node)
	}
	if call.vmid != "200" {
		t.Errorf("vmid = %q, want 200", call.vmid)
	}
	val, ok := call.params.Scsi[configDriveSlotIndex]
	if !ok {
		t.Fatalf("scsi[%d] missing from UpdateQemuConfig params", configDriveSlotIndex)
	}
	if !strings.Contains(val, "local:iso/vm-200-config.iso") {
		t.Errorf("scsi30 value = %q, missing local:iso/vm-200-config.iso", val)
	}
	if !strings.Contains(val, "media=cdrom") {
		t.Errorf("scsi30 value = %q, missing media=cdrom", val)
	}
}

func TestConfigDrive_Configure_DeletesLocalTempOnSuccess(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "iso-temp-*")
	defer os.RemoveAll(tempDir)
	t.Setenv("TMPDIR", tempDir)

	a := newISOAgent(nil, nil)
	if err := a.Configure(context.Background(), "pve1", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	entries, _ := os.ReadDir(tempDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bosh-cpi-configdrive-") {
			t.Errorf("temp file %q was not cleaned up", e.Name())
		}
	}
}

func TestConfigDrive_Configure_DeletesLocalTempOnUploadFailure(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "iso-temp-*")
	defer os.RemoveAll(tempDir)
	t.Setenv("TMPDIR", tempDir)

	storageSvc := &fakeStorageSvc{
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			return "", errors.New("upload boom")
		},
	}
	a := newISOAgent(storageSvc, nil)
	err := a.Configure(context.Background(), "pve1", 200, baseISOConfig())
	if err == nil {
		t.Fatal("expected error from failed upload")
	}

	entries, _ := os.ReadDir(tempDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bosh-cpi-configdrive-") {
			t.Errorf("temp file %q leaked after failed upload", e.Name())
		}
	}
}

func TestConfigDrive_Configure_MBusFallback(t *testing.T) {
	t.Parallel()

	var uploadedPath string
	storageSvc := &fakeStorageSvc{
		uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
			tmp, err := os.CreateTemp("", "uploaded-*.iso")
			if err != nil {
				return "", err
			}
			defer tmp.Close()
			if _, err := io.Copy(tmp, body); err != nil {
				return "", err
			}
			uploadedPath = tmp.Name()
			return "", nil
		},
	}

	a := newISOAgent(storageSvc, nil)
	defer func() {
		if uploadedPath != "" {
			_ = os.Remove(uploadedPath)
		}
	}()

	cfg := baseISOConfig()
	cfg.MBus = ""
	cfg.Blobstore = BlobstoreSpec{
		Provider: "dav",
		Options:  map[string]any{"endpoint": "https://10.0.0.42:25250"},
	}

	if err := a.Configure(context.Background(), "pve1", 200, cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	s, _ := readSettingsFromISO(t, uploadedPath)
	if s.MBus != "nats://10.0.0.42:4222" {
		t.Errorf("settings.mbus = %q, want fallback nats://10.0.0.42:4222", s.MBus)
	}
}

func TestConfigDrive_Remove_DeletesISOFromStorage(t *testing.T) {
	t.Parallel()

	storageSvc := &fakeStorageSvc{}
	a := newISOAgent(storageSvc, nil)
	if err := a.Remove(context.Background(), "pve1", 200); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if len(storageSvc.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteVolume call, got %d", len(storageSvc.deleteCalls))
	}
	if storageSvc.deleteCalls[0] != "local:iso/vm-200-config.iso" {
		t.Errorf("DeleteVolume volume = %q, want local:iso/vm-200-config.iso", storageSvc.deleteCalls[0])
	}
}

func TestConfigDrive_Remove_404IsSuccess(t *testing.T) {
	t.Parallel()

	storageSvc := &fakeStorageSvc{
		deleteFn: func(_ context.Context, _, _, _ string) error {
			// SDK's DeleteVolume already swallows 404 before returning nil.
			return nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	if err := a.Remove(context.Background(), "pve1", 200); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestConfigDrive_UpdateDiskHints_NoOp(t *testing.T) {
	t.Parallel()
	a := newISOAgent(nil, nil)
	if err := a.UpdateDiskHints(context.Background(), 200, []DiskHint{{DiskCID: "x", DevicePath: "/dev/sdc"}}); err != nil {
		t.Fatalf("UpdateDiskHints: %v", err)
	}
}

func TestConfigDrive_Configure_AttachFailureCleansUpUploadedISO(t *testing.T) {
	t.Parallel()

	storageSvc := &fakeStorageSvc{}
	nodesSvc := &fakeNodesSvc{
		updateConfigFn: func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return errors.New("attach boom")
		},
	}
	a := newISOAgent(storageSvc, nodesSvc)
	err := a.Configure(context.Background(), "pve1", 200, baseISOConfig())
	if err == nil {
		t.Fatal("expected error from attach failure")
	}
	// After attach fails, orphan cleanup calls DeleteVolume.
	if len(storageSvc.deleteCalls) != 1 {
		t.Errorf("expected 1 DeleteVolume call for orphan cleanup, got %d", len(storageSvc.deleteCalls))
	}
}

func TestConfigDrive_Remove_InvalidInputs(t *testing.T) {
	t.Parallel()
	a := newISOAgent(nil, nil)

	if err := a.Remove(nil, "pve1", 200); err == nil { //nolint:staticcheck
		t.Error("expected error for nil ctx")
	}
	if err := a.Remove(context.Background(), "", 200); err == nil {
		t.Error("expected error for empty node")
	}
	if err := a.Remove(context.Background(), "pve1", 0); err == nil {
		t.Error("expected error for zero vmid")
	}
}

func TestConfigDriveISOFilename(t *testing.T) {
	t.Parallel()
	got := configDriveISOFilename(200)
	if got != "vm-200-config.iso" {
		t.Errorf("filename = %q, want vm-200-config.iso", got)
	}
}

// TestConfigDrive_Configure_PreDeletesStaleVolume confirms that Configure
// issues a DeleteVolumeIfExists for any pre-existing orphan ISO at the target
// filename before uploading the fresh ISO.
func TestConfigDrive_Configure_PreDeletesStaleVolume(t *testing.T) {
	t.Parallel()

	var calls []string
	storageSvc := &fakeStorageSvc{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			calls = append(calls, "delete:"+volume)
			return true, nil
		},
		uploadFn: func(_ context.Context, _, _, _, filename string, _ io.Reader) (string, error) {
			calls = append(calls, "upload:"+filename)
			return "", nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	if err := a.Configure(context.Background(), "pve1", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(calls) < 2 || !strings.HasPrefix(calls[0], "delete:") || !strings.HasPrefix(calls[1], "upload:") {
		t.Fatalf("expected pre-delete before upload, got %v", calls)
	}
	if !strings.Contains(calls[0], "local:iso/vm-200-config.iso") {
		t.Errorf("pre-delete targeted %q, want it to reference vm-200-config.iso", calls[0])
	}
}

// TestConfigDrive_Configure_PreDelete404IsNotAnError verifies that a
// 404 from the pre-delete (volume not found) is silently tolerated and
// Configure still runs the upload.
func TestConfigDrive_Configure_PreDelete404IsNotAnError(t *testing.T) {
	t.Parallel()

	var uploadCalled bool
	storageSvc := &fakeStorageSvc{
		// DeleteVolumeIfExists returns (false, nil) when volume does not exist.
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, nil
		},
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalled = true
			return "", nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	if err := a.Configure(context.Background(), "pve1", 200, baseISOConfig()); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !uploadCalled {
		t.Error("expected upload to run after volume-not-found pre-delete")
	}
}

// TestConfigDrive_Configure_PreDeleteHardFailureSurfaces verifies that
// any non-404 error from the pre-delete is surfaced and the upload is
// not attempted.
func TestConfigDrive_Configure_PreDeleteHardFailureSurfaces(t *testing.T) {
	t.Parallel()

	var uploadCalled bool
	storageSvc := &fakeStorageSvc{
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, &pveerr.APIError{Message: "storage offline", HTTPCode: 500}
		},
		uploadFn: func(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
			uploadCalled = true
			return "", nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	err := a.Configure(context.Background(), "pve1", 200, baseISOConfig())
	if err == nil {
		t.Fatal("expected pre-delete hard failure to propagate")
	}
	if !strings.Contains(err.Error(), "pre-delete stale configdrive iso") {
		t.Errorf("expected pre-delete error context, got %v", err)
	}
	if uploadCalled {
		t.Error("upload must not run after a hard pre-delete failure")
	}
}

func TestConfigDrive_Configure_InvalidInputs(t *testing.T) {
	t.Parallel()
	a := newISOAgent(nil, nil)

	if err := a.Configure(nil, "pve1", 200, baseISOConfig()); err == nil { //nolint:staticcheck
		t.Error("expected error for nil ctx")
	}
	if err := a.Configure(context.Background(), "", 200, baseISOConfig()); err == nil {
		t.Error("expected error for empty node")
	}
	if err := a.Configure(context.Background(), "pve1", 0, baseISOConfig()); err == nil {
		t.Error("expected error for zero vmid")
	}
}

// --------------------------------------------------------------------------
// Typed Storage().DeleteVolume coverage for Remove().
// --------------------------------------------------------------------------

func TestConfigDrive_Remove_DeletesViaTypedStorageService(t *testing.T) {
	t.Parallel()
	storageSvc := &fakeStorageSvc{}
	a := newISOAgent(storageSvc, nil)
	if err := a.Remove(context.Background(), "pve1", 200); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(storageSvc.deleteCalls) != 1 {
		t.Fatalf("expected one DeleteVolume call, got %d", len(storageSvc.deleteCalls))
	}
	if storageSvc.deleteCalls[0] != "local:iso/vm-200-config.iso" {
		t.Errorf("DeleteVolume volume = %q, want local:iso/vm-200-config.iso", storageSvc.deleteCalls[0])
	}
}

// TestConfigDrive_Remove_TypedServiceTreats404AsSuccess documents the
// production contract: the SDK's storage.DeleteVolume swallows HTTP 404
// internally, so by the time the agent sees a return value it has already
// been normalized to nil.
func TestConfigDrive_Remove_TypedServiceTreats404AsSuccess(t *testing.T) {
	t.Parallel()
	storageSvc := &fakeStorageSvc{
		deleteFn: func(_ context.Context, _, _, _ string) error {
			return nil
		},
	}
	a := newISOAgent(storageSvc, nil)
	if err := a.Remove(context.Background(), "pve1", 200); err != nil {
		t.Fatalf("expected Remove to swallow 404 via typed service, got %v", err)
	}
	if len(storageSvc.deleteCalls) != 1 {
		t.Errorf("expected one DeleteVolume invocation, got %d", len(storageSvc.deleteCalls))
	}
}

func TestConfigDrive_Remove_TypedServiceErrorPropagates(t *testing.T) {
	t.Parallel()
	storageSvc := &fakeStorageSvc{
		deleteFn: func(_ context.Context, _, _, _ string) error {
			return &pveerr.APIError{Message: "storage offline", HTTPCode: 500}
		},
	}
	a := newISOAgent(storageSvc, nil)
	err := a.Remove(context.Background(), "pve1", 200)
	if err == nil {
		t.Fatal("expected 500 to propagate")
	}
	if !strings.Contains(err.Error(), "storage offline") {
		t.Errorf("expected upstream message in error chain, got %v", err)
	}
}
