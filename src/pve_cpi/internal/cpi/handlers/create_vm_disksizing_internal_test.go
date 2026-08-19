package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// --------------------------------------------------------------------------
// Minimal mock types local to this file
// --------------------------------------------------------------------------

// diskSizingQEMU implements sdkqemu.Service with configurable Config, ResizeDisk, AttachDisk.
type diskSizingQEMU struct {
	configFn     func(ctx context.Context, node string, vmid int) (map[string]any, error)
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	attachDiskFn func(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error)
}

func (q *diskSizingQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.configFn != nil {
		return q.configFn(ctx, node, vmid)
	}
	return map[string]any{}, nil
}
func (q *diskSizingQEMU) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if q.resizeDiskFn != nil {
		return q.resizeDiskFn(ctx, node, vmid, diskID, sizeGiB)
	}
	panic("diskSizingQEMU.ResizeDisk: not expected in this test")
}
func (q *diskSizingQEMU) AttachDisk(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error) {
	if q.attachDiskFn != nil {
		return q.attachDiskFn(ctx, node, vmid, volid, bus, opts)
	}
	panic("diskSizingQEMU.AttachDisk: not expected in this test")
}

// Unimplemented stubs — panic on unexpected calls.
func (q *diskSizingQEMU) Create(_ context.Context, _ string, _ map[string]any) (string, error) {
	panic("diskSizingQEMU.Create: not expected")
}
func (q *diskSizingQEMU) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("diskSizingQEMU.Start: not expected")
}
func (q *diskSizingQEMU) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("diskSizingQEMU.Stop: not expected")
}
func (q *diskSizingQEMU) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("diskSizingQEMU.Clone: not expected")
}
func (q *diskSizingQEMU) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("diskSizingQEMU.Status: not expected")
}
func (q *diskSizingQEMU) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("diskSizingQEMU.Reset: not expected")
}
func (q *diskSizingQEMU) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("diskSizingQEMU.Template: not expected")
}
func (q *diskSizingQEMU) DetachDisk(_ context.Context, _ string, _ int, _ string) error {
	panic("diskSizingQEMU.DetachDisk: not expected")
}
func (q *diskSizingQEMU) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("diskSizingQEMU.Snapshot: not expected")
}
func (q *diskSizingQEMU) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("diskSizingQEMU.DeleteSnapshot: not expected")
}
func (q *diskSizingQEMU) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("diskSizingQEMU.ListSnapshots: not expected")
}
func (q *diskSizingQEMU) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("diskSizingQEMU.RollbackSnapshot: not expected")
}

var _ sdkqemu.Service = (*diskSizingQEMU)(nil)

// diskSizingStorage implements sdkstorage.Service with configurable CreateVolume + DeleteVolumeAsync.
type diskSizingStorage struct {
	createVolumeFn      func(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error)
	deleteVolumeAsyncFn func(ctx context.Context, node, storage, volume string) (string, error)
}

func (s *diskSizingStorage) CreateVolume(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
	if s.createVolumeFn != nil {
		return s.createVolumeFn(ctx, node, storage, sizeGiB, format, vmid, name)
	}
	return fmt.Sprintf("%s:vm-%d-%s", storage, vmid, name), nil
}

// DeleteVolume is not expected to be called by attachEphemeralDisk's rollback
// path: cleanup must use the async+await variant (see cleanupVol in
// create_vm.go) so a queued imgdel task cannot race a same-name re-upload.
func (s *diskSizingStorage) DeleteVolume(_ context.Context, _, _, _ string) error {
	panic("diskSizingStorage.DeleteVolume: not expected; rollback must use DeleteVolumeAsync")
}
func (s *diskSizingStorage) DeleteVolumeAsync(ctx context.Context, node, storage, volume string) (string, error) {
	if s.deleteVolumeAsyncFn != nil {
		return s.deleteVolumeAsyncFn(ctx, node, storage, volume)
	}
	return "", nil
}
func (s *diskSizingStorage) DeleteVolumeIfExists(_ context.Context, _, _, _ string) (bool, error) {
	panic("diskSizingStorage.DeleteVolumeIfExists: not expected")
}
func (s *diskSizingStorage) DeleteVolumeIfExistsAsync(_ context.Context, _, _, _ string) (bool, string, error) {
	panic("diskSizingStorage.DeleteVolumeIfExistsAsync: not expected")
}
func (s *diskSizingStorage) Exists(_ context.Context, _, _, _ string) (bool, error) {
	panic("diskSizingStorage.Exists: not expected")
}
func (s *diskSizingStorage) Upload(_ context.Context, _, _, _, _ string, _ io.Reader) (string, error) {
	panic("diskSizingStorage.Upload: not expected")
}

var _ sdkstorage.Service = (*diskSizingStorage)(nil)

// diskSizingTasks implements sdktasks.Service with configurable Wait, used to
// verify attachEphemeralDisk's rollback awaits the DeleteVolumeAsync UPID
// (pve.AwaitTaskWithLogger → Tasks().Wait) rather than discarding it. Absent a
// waitFn, Wait reports the task as immediately successful so tests that do not
// care about await behavior are unaffected.
type diskSizingTasks struct {
	sdktasks.Service
	waitFn func(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error)
}

func (t *diskSizingTasks) Wait(ctx context.Context, node, upid string, opts *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	if t.waitFn != nil {
		return t.waitFn(ctx, node, upid, opts)
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

var _ sdktasks.Service = (*diskSizingTasks)(nil)

// diskSizingPVE implements pve.Client; QEMU() and optionally Storage()/Tasks() are live.
type diskSizingPVE struct {
	qemu    sdkqemu.Service
	storage sdkstorage.Service
	tasks   sdktasks.Service
}

func (p *diskSizingPVE) QEMU() sdkqemu.Service { return p.qemu }
func (p *diskSizingPVE) Storage() sdkstorage.Service {
	if p.storage != nil {
		return p.storage
	}
	panic("diskSizingPVE.Storage: not expected")
}
func (p *diskSizingPVE) CloudInit() sdkcloudinit.Service {
	panic("diskSizingPVE.CloudInit: not expected")
}
func (p *diskSizingPVE) Tasks() sdktasks.Service {
	if p.tasks != nil {
		return p.tasks
	}
	return &diskSizingTasks{}
}
func (p *diskSizingPVE) Nodes() sdknodes.Service     { panic("diskSizingPVE.Nodes: not expected") }
func (p *diskSizingPVE) Cluster() sdkcluster.Service { panic("diskSizingPVE.Cluster: not expected") }
func (p *diskSizingPVE) ClusterStorage() sdkclusterstorage.Service {
	panic("diskSizingPVE.ClusterStorage: not expected")
}
func (p *diskSizingPVE) Pools() pve.PoolService { panic("diskSizingPVE.Pools: not expected") }

var _ pve.Client = (*diskSizingPVE)(nil)

// buildDiskSizingDeps builds a Deps with the given configFn and optional resizeDiskFn.
func buildDiskSizingDeps(
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error),
	resizeDiskFn func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error),
) Deps {
	return Deps{
		Config: &config.CPIConfig{VMStorage: "local-lvm"},
		PVE: &diskSizingPVE{
			qemu: &diskSizingQEMU{
				configFn:     configFn,
				resizeDiskFn: resizeDiskFn,
			},
		},
		Logger: log.NewNopLogger(),
	}
}

// buildEphemeralDeps builds a Deps with wired QEMU (configFn + attachDiskFn) and Storage.
func buildEphemeralDeps(
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error),
	attachDiskFn func(ctx context.Context, node string, vmid int, volid, bus string, opts *sdkqemu.AttachOpts) (string, error),
	stor *diskSizingStorage,
) Deps {
	return Deps{
		Config: &config.CPIConfig{VMStorage: "local-lvm"},
		PVE: &diskSizingPVE{
			qemu: &diskSizingQEMU{
				configFn:     configFn,
				attachDiskFn: attachDiskFn,
			},
			storage: stor,
		},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// readRootDiskSizeGiB tests
// --------------------------------------------------------------------------

// TestReadRootDiskSizeGiB_ReturnsActualSize verifies the happy path: virtio0
// present with a parseable size= directive returns the correct GiB value.
func TestReadRootDiskSizeGiB_ReturnsActualSize(t *testing.T) {
	t.Parallel()

	deps := buildDiskSizingDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				diskKeyVirtio0: "local-lvm:vm-100-disk-0,size=7G",
				"net0":         "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			}, nil
		},
		nil,
	)

	got, err := readRootDiskSizeGiB(context.Background(), deps, "pve", 100, diskKeyVirtio0)
	if err != nil {
		t.Fatalf("readRootDiskSizeGiB unexpected error: %v", err)
	}
	if got != 7 {
		t.Errorf("readRootDiskSizeGiB = %d; want 7", got)
	}
}

// TestReadRootDiskSizeGiB_MissingKey_Fallback verifies that a config map without
// a virtio0 key returns defaultStemcellDiskGiB (5).
func TestReadRootDiskSizeGiB_MissingKey_Fallback(t *testing.T) {
	t.Parallel()

	deps := buildDiskSizingDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"net0": "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
			}, nil
		},
		nil,
	)

	got, err := readRootDiskSizeGiB(context.Background(), deps, "pve", 100, diskKeyVirtio0)
	if err != nil {
		t.Fatalf("readRootDiskSizeGiB unexpected error: %v", err)
	}
	if got != defaultStemcellDiskGiB {
		t.Errorf("readRootDiskSizeGiB (missing key) = %d; want %d (defaultStemcellDiskGiB)", got, defaultStemcellDiskGiB)
	}
}

// TestReadRootDiskSizeGiB_ConfigError_Propagates verifies that a Config call
// returning an error is propagated (NOT swallowed as a fallback to 5). A
// transient read failure on a non-5-GiB template would otherwise fabricate the
// wrong resize delta; the caller surfaces this as a retriable error instead.
func TestReadRootDiskSizeGiB_ConfigError_Propagates(t *testing.T) {
	t.Parallel()

	deps := buildDiskSizingDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return nil, errors.New("PVE API unreachable")
		},
		nil,
	)

	_, err := readRootDiskSizeGiB(context.Background(), deps, "pve", 100, diskKeyVirtio0)
	if err == nil {
		t.Fatal("readRootDiskSizeGiB (config error): expected error, got nil")
	}
}

// TestReadRootDiskSizeGiB_ParseError_Fallback verifies that an unparseable
// virtio0 option string causes fallback to defaultStemcellDiskGiB.
func TestReadRootDiskSizeGiB_ParseError_Fallback(t *testing.T) {
	t.Parallel()

	deps := buildDiskSizingDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				diskKeyVirtio0: "bad-format-no-size-directive",
			}, nil
		},
		nil,
	)

	got, err := readRootDiskSizeGiB(context.Background(), deps, "pve", 100, diskKeyVirtio0)
	if err != nil {
		t.Fatalf("readRootDiskSizeGiB unexpected error: %v", err)
	}
	if got != defaultStemcellDiskGiB {
		t.Errorf("readRootDiskSizeGiB (parse error) = %d; want %d (defaultStemcellDiskGiB)", got, defaultStemcellDiskGiB)
	}
}

// --------------------------------------------------------------------------
// resizeRootDisk tests
// --------------------------------------------------------------------------

// buildResizeDeps builds Deps for resizeRootDisk tests with configurable
// actual virtio0 size and a recording resizeDiskFn.
func buildResizeDeps(
	actualSizeGiB int,
	resizeCalls *[]int,
) Deps {
	return buildDiskSizingDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				diskKeyVirtio0: "local-lvm:vm-100-disk-0,size=" + itoa(actualSizeGiB) + "G",
			}, nil
		},
		func(_ context.Context, _ string, _ int, _ string, sizeGiB int) (string, error) {
			if resizeCalls != nil {
				*resizeCalls = append(*resizeCalls, sizeGiB)
			}
			return "", nil
		},
	)
}

// itoa converts an int to a decimal string for test helpers.
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// TestResizeRootDisk_GrowCorrectDelta verifies that when the template is 5 GiB
// and shape requests 10 GiB, ResizeDisk is called with +5.
func TestResizeRootDisk_GrowCorrectDelta(t *testing.T) {
	t.Parallel()

	var resizeCalls []int
	deps := buildResizeDeps(5, &resizeCalls)
	shape := &createVMShape{
		node:        "pve",
		rootDiskGiB: 10,
		maxAttempts: 1,
		rootDiskKey: diskKeyVirtio0,
	}

	err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 100)
	if err != nil {
		t.Fatalf("resizeRootDisk returned error: %v", err)
	}
	if len(resizeCalls) != 1 {
		t.Fatalf("ResizeDisk called %d times; want 1", len(resizeCalls))
	}
	if resizeCalls[0] != 5 {
		t.Errorf("ResizeDisk delta = %d; want 5", resizeCalls[0])
	}
}

// TestResizeRootDisk_GrowTemplate3G verifies the bug-fix: when the template is
// 3 GiB (not the hardcoded 5 GiB default) and shape requests 10 GiB,
// ResizeDisk is called with +7, not +5.
func TestResizeRootDisk_GrowTemplate3G(t *testing.T) {
	t.Parallel()

	var resizeCalls []int
	deps := buildResizeDeps(3, &resizeCalls)
	shape := &createVMShape{
		node:        "pve",
		rootDiskGiB: 10,
		maxAttempts: 1,
		rootDiskKey: diskKeyVirtio0,
	}

	err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 100)
	if err != nil {
		t.Fatalf("resizeRootDisk returned error: %v", err)
	}
	if len(resizeCalls) != 1 {
		t.Fatalf("ResizeDisk called %d times; want 1", len(resizeCalls))
	}
	if resizeCalls[0] != 7 {
		t.Errorf("ResizeDisk delta = %d; want 7 (bug fix: actual=3G not 5G)", resizeCalls[0])
	}
}

// TestResizeRootDisk_NoOp_Equal verifies that when requested size equals the
// actual template size, ResizeDisk is NOT called and nil is returned.
func TestResizeRootDisk_NoOp_Equal(t *testing.T) {
	t.Parallel()

	var resizeCalls []int
	deps := buildResizeDeps(10, &resizeCalls)
	shape := &createVMShape{
		node:        "pve",
		rootDiskGiB: 10,
		maxAttempts: 1,
		rootDiskKey: diskKeyVirtio0,
	}

	err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 100)
	if err != nil {
		t.Fatalf("resizeRootDisk returned error: %v", err)
	}
	if len(resizeCalls) != 0 {
		t.Errorf("ResizeDisk called %d times; want 0 (no-op equal)", len(resizeCalls))
	}
}

// TestResizeRootDisk_ShrinkReject verifies that requesting a size smaller than
// the actual template size returns a Cloud error and ResizeDisk is NOT called.
func TestResizeRootDisk_ShrinkReject(t *testing.T) {
	t.Parallel()

	var resizeCalls []int
	deps := buildResizeDeps(10, &resizeCalls)
	shape := &createVMShape{
		node:        "pve",
		rootDiskGiB: 5,
		maxAttempts: 1,
		rootDiskKey: diskKeyVirtio0,
	}

	err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 100)
	if err == nil {
		t.Fatal("expected error for shrink request, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected Cloud error for shrink; got: %v", err)
	}
	if !strings.Contains(err.Error(), "shrink not supported") {
		t.Errorf("error message missing 'shrink not supported': %v", err)
	}
	if len(resizeCalls) != 0 {
		t.Errorf("ResizeDisk called %d times; want 0 (shrink must not resize)", len(resizeCalls))
	}
}

// --------------------------------------------------------------------------
// resolveVMShapeStorage root_disk_size tests
// --------------------------------------------------------------------------

// TestResolveVMShapeStorage_RootDiskSize_MiB verifies that root_disk_size=10240
// (MiB) resolves to rootDiskGiB=10.
func TestResolveVMShapeStorage_RootDiskSize_MiB(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})
	parsed.cloudProps.RootDiskSize = 10240

	_, _, rootDiskGiB, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rootDiskGiB != 10 {
		t.Errorf("rootDiskGiB = %d; want 10 (root_disk_size=10240 MiB)", rootDiskGiB)
	}
}

// TestResolveVMShapeStorage_RootDiskSize_Precedence verifies that root_disk_size
// takes precedence over disk when both are set.
func TestResolveVMShapeStorage_RootDiskSize_Precedence(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})
	parsed.cloudProps.RootDiskSize = 10240 // 10 GiB
	parsed.cloudProps.Disk = 5120          // 5 GiB — must be ignored

	_, _, rootDiskGiB, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rootDiskGiB != 10 {
		t.Errorf("rootDiskGiB = %d; want 10 (root_disk_size wins over disk)", rootDiskGiB)
	}
}

// TestResolveVMShapeStorage_Disk_LegacyStillWorks verifies that the legacy disk
// field still works when root_disk_size is unset.
func TestResolveVMShapeStorage_Disk_LegacyStillWorks(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})
	parsed.cloudProps.Disk = 10240 // 10 GiB via legacy field

	_, _, rootDiskGiB, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rootDiskGiB != 10 {
		t.Errorf("rootDiskGiB = %d; want 10 (legacy disk=10240 MiB)", rootDiskGiB)
	}
}

// --------------------------------------------------------------------------
// resolveEphemeralShape tests
// --------------------------------------------------------------------------

// TestResolveEphemeralShape_Disabled_NoOp verifies that zero EphemeralDiskSizeMB
// returns (0, "", nil) — byte-identical to pre-feature behavior.
func TestResolveEphemeralShape_Disabled_NoOp(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	cp := createVMCloudProps{EphemeralDiskSizeMB: 0}

	gib, stor, err := resolveEphemeralShape(context.Background(), Deps{Config: cfg}, cp, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gib != 0 || stor != "" {
		t.Errorf("resolveEphemeralShape disabled = (%d, %q); want (0, \"\")", gib, stor)
	}
}

// TestResolveEphemeralShape_ExplicitPool verifies that EphemeralStoragePool
// struct field is used when set and no resolver layer overrides it.
func TestResolveEphemeralShape_ExplicitPool(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	cp := createVMCloudProps{
		EphemeralDiskSizeMB:  4096,
		EphemeralStoragePool: "fast",
	}

	gib, stor, err := resolveEphemeralShape(context.Background(), Deps{Config: cfg}, cp, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gib != 4 {
		t.Errorf("ephemeralDiskGiB = %d; want 4 (4096 MiB / 1024)", gib)
	}
	if stor != "fast" {
		t.Errorf("ephemeralStorage = %q; want \"fast\"", stor)
	}
}

// TestResolveEphemeralShape_FallbackVMStorage verifies fallback to cfg.VMStorage
// when no pool is set in the struct field or resolver.
func TestResolveEphemeralShape_FallbackVMStorage(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	cp := createVMCloudProps{EphemeralDiskSizeMB: 4096}

	gib, stor, err := resolveEphemeralShape(context.Background(), Deps{Config: cfg}, cp, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gib != 4 {
		t.Errorf("ephemeralDiskGiB = %d; want 4", gib)
	}
	if stor != "local-lvm" {
		t.Errorf("ephemeralStorage = %q; want cfg.VMStorage (local-lvm)", stor)
	}
}

// TestResolveEphemeralShape_RoundUp verifies ceil(MB/1024) rounding:
// 1025 MiB → 2 GiB.
func TestResolveEphemeralShape_RoundUp(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	cp := createVMCloudProps{EphemeralDiskSizeMB: 1025}

	gib, _, err := resolveEphemeralShape(context.Background(), Deps{Config: cfg}, cp, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gib != 2 {
		t.Errorf("ephemeralDiskGiB = %d; want 2 (ceil(1025/1024))", gib)
	}
}

// TestResolveEphemeralShape_1MB verifies that a 1 MiB request rounds up to 1 GiB.
func TestResolveEphemeralShape_1MB(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMStorage: "local-lvm"}
	cp := createVMCloudProps{EphemeralDiskSizeMB: 1}

	gib, _, err := resolveEphemeralShape(context.Background(), Deps{Config: cfg}, cp, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gib != 1 {
		t.Errorf("ephemeralDiskGiB = %d; want 1 (ceil(1/1024)=1)", gib)
	}
}

// --------------------------------------------------------------------------
// attachEphemeralDisk tests
// --------------------------------------------------------------------------

// TestAttachEphemeralDisk_Disabled_NoOp verifies that shape.ephemeralDiskGiB=0
// returns ("", nil) without calling CreateVolume.
func TestAttachEphemeralDisk_Disabled_NoOp(t *testing.T) {
	t.Parallel()

	called := false
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			called = true
			return "", nil
		},
	}
	deps := buildEphemeralDeps(nil, nil, stor)
	shape := &createVMShape{node: "pve", ephemeralDiskGiB: 0}

	devPath, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devPath != "" {
		t.Errorf("devPath = %q; want empty (no-op path)", devPath)
	}
	if called {
		t.Error("CreateVolume called on no-op path; must not be called when ephemeralDiskGiB=0")
	}
}

// TestAttachEphemeralDisk_Success verifies the happy path: CreateVolume returns
// a volid, Config returns an empty cfg (slot=1 free), AttachDisk returns "scsi1",
// and devPath is the expected by-id path.
func TestAttachEphemeralDisk_Success(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
	}
	deps := buildEphemeralDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// Empty config: no scsi slots taken → slot=1.
			return map[string]any{}, nil
		},
		func(_ context.Context, _ string, _ int, gotVolid, bus string, opts *sdkqemu.AttachOpts) (string, error) {
			if gotVolid != volid {
				return "", fmt.Errorf("AttachDisk: volid=%q, want %q", gotVolid, volid)
			}
			if bus != "scsi" {
				return "", fmt.Errorf("AttachDisk: bus=%q, want scsi", bus)
			}
			// The slot must be forced via opts to the computed non-zero index;
			// a nil opts (SDK assigns from scsi0) would put ephemeral on the
			// root-disk-colliding scsi0 slot.
			if opts == nil {
				return "", fmt.Errorf("AttachDisk: opts is nil; ephemeral slot must be forced (never scsi0)")
			}
			if opts.DiskID != "scsi1" {
				return "", fmt.Errorf("AttachDisk: opts.DiskID=%q, want scsi1 (empty config → floor slot)", opts.DiskID)
			}
			return opts.DiskID, nil
		},
		stor,
	)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	devPath, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1"
	if devPath != want {
		t.Errorf("devPath = %q; want %q", devPath, want)
	}
}

// TestAttachEphemeralDisk_NextFreeSlot verifies the computed free slot flows
// through to AttachOpts: with scsi1 already occupied, the ephemeral disk lands
// on scsi2 (never scsi0, never a colliding slot).
func TestAttachEphemeralDisk_NextFreeSlot(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-102-ephemeral-0"
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
	}
	deps := buildEphemeralDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			// scsi1 taken by a persistent disk → next free is scsi2.
			return map[string]any{"scsi1": "local-lvm:vm-102-disk-1,size=10G"}, nil
		},
		func(_ context.Context, _ string, _ int, _, _ string, opts *sdkqemu.AttachOpts) (string, error) {
			if opts == nil || opts.DiskID != "scsi2" {
				return "", fmt.Errorf("AttachDisk: opts.DiskID=%v, want scsi2", opts)
			}
			return opts.DiskID, nil
		},
		stor,
	)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	devPath, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 102)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2"
	if devPath != want {
		t.Errorf("devPath = %q; want %q", devPath, want)
	}
}

// TestAttachEphemeralDisk_CreateFail_NoOrphan verifies that when CreateVolume
// fails, no DeleteVolumeAsync call is made (nothing was created to clean up).
func TestAttachEphemeralDisk_CreateFail_NoOrphan(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return "", errors.New("storage pool not found")
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalled = true
			return "", nil
		},
	}
	deps := buildEphemeralDeps(nil, nil, stor)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error from CreateVolume failure, got nil")
	}
	if deleteCalled {
		t.Error("DeleteVolumeAsync called after CreateVolume failure — no volume exists to delete")
	}
}

// TestAttachEphemeralDisk_AttachFail_OrphanCleanup verifies that when
// AttachDisk fails after CreateVolume succeeds, DeleteVolumeAsync is called
// with the created volid (not the synchronous DeleteVolume — see
// diskSizingStorage.DeleteVolume, which panics if invoked).
func TestAttachEphemeralDisk_AttachFail_OrphanCleanup(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	var deletedVolid string
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, volume string) (string, error) {
			deletedVolid = volume
			return "", nil
		},
	}
	deps := buildEphemeralDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
		func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
			return "", errors.New("disk attach failed")
		},
		stor,
	)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error from AttachDisk failure, got nil")
	}
	if deletedVolid == "" {
		t.Error("DeleteVolumeAsync not called after AttachDisk failure — orphan volume would be left behind")
	}
	if deletedVolid != volid {
		t.Errorf("DeleteVolumeAsync called with volid=%q; want %q", deletedVolid, volid)
	}
}

// TestAttachEphemeralDisk_OrphanCleanup_AwaitsDeleteUPID verifies that when
// DeleteVolumeAsync returns a non-empty UPID, cleanupVol awaits it via
// pve.AwaitTaskWithLogger (Tasks().Wait) instead of discarding it — the fix
// for the SDK's documented same-name re-upload race (M1).
func TestAttachEphemeralDisk_OrphanCleanup_AwaitsDeleteUPID(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	const upid = "UPID:pve:00001234:imgdel:local-lvm:vm-101-ephemeral-0:root@pam:"
	var waitedUPID string
	var waitedNode string
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			return upid, nil
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{VMStorage: "local-lvm"},
		PVE: &diskSizingPVE{
			qemu: &diskSizingQEMU{
				configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
					return map[string]any{}, nil
				},
				attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
					return "", errors.New("disk attach failed")
				},
			},
			storage: stor,
			tasks: &diskSizingTasks{
				waitFn: func(_ context.Context, node, gotUPID string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					waitedNode = node
					waitedUPID = gotUPID
					return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: gotUPID}, nil
				},
			},
		},
		Logger: log.NewNopLogger(),
	}
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error from AttachDisk failure, got nil")
	}
	if waitedUPID != upid {
		t.Errorf("Tasks().Wait upid = %q; want %q (imgdel UPID must be awaited, not discarded)", waitedUPID, upid)
	}
	if waitedNode != "pve" {
		t.Errorf("Tasks().Wait node = %q; want %q", waitedNode, "pve")
	}
}

// TestAttachEphemeralDisk_OrphanCleanup_AwaitFailureLoggedNotFatal verifies
// that a failure awaiting the DeleteVolumeAsync UPID is logged (best-effort
// cleanup) and does not replace or suppress the original AttachDisk error.
func TestAttachEphemeralDisk_OrphanCleanup_AwaitFailureLoggedNotFatal(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	const upid = "UPID:pve:00001234:imgdel:local-lvm:vm-101-ephemeral-0:root@pam:"
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			return upid, nil
		},
	}
	deps := Deps{
		Config: &config.CPIConfig{VMStorage: "local-lvm"},
		PVE: &diskSizingPVE{
			qemu: &diskSizingQEMU{
				configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
					return map[string]any{}, nil
				},
				attachDiskFn: func(_ context.Context, _ string, _ int, _, _ string, _ *sdkqemu.AttachOpts) (string, error) {
					return "", errors.New("disk attach failed")
				},
			},
			storage: stor,
			tasks: &diskSizingTasks{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return nil, errors.New("task poll transport error")
				},
			},
		},
		Logger: log.NewNopLogger(),
	}
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error from AttachDisk failure, got nil")
	}
	if !strings.Contains(err.Error(), "disk attach failed") {
		t.Errorf("expected original AttachDisk error to surface, got: %v", err)
	}
}

// TestAttachEphemeralDisk_ConfigReadFail_OrphanCleanup verifies that when
// Config read fails after CreateVolume succeeds, DeleteVolumeAsync is called.
func TestAttachEphemeralDisk_ConfigReadFail_OrphanCleanup(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	deleteCalled := false
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalled = true
			return "", nil
		},
	}
	deps := buildEphemeralDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return nil, errors.New("PVE API timeout")
		},
		nil,
		stor,
	)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error from Config failure, got nil")
	}
	if !deleteCalled {
		t.Error("DeleteVolumeAsync not called after Config read failure — orphan volume would be left behind")
	}
}

// TestAttachEphemeralDisk_ScsiSlotExhausted verifies that when all scsi slots
// scsi1..28 are occupied, a CloudError is returned and DeleteVolumeAsync is
// called.
func TestAttachEphemeralDisk_ScsiSlotExhausted(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-101-ephemeral-0"
	deleteCalled := false
	stor := &diskSizingStorage{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, _ int, _ string) (string, error) {
			return volid, nil
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalled = true
			return "", nil
		},
	}

	// Build a VM config with scsi1..28 all occupied.
	occupiedCfg := map[string]any{}
	for i := 1; i <= 28; i++ {
		occupiedCfg[fmt.Sprintf("scsi%d", i)] = "local-lvm:vm-101-disk-" + itoa(i)
	}
	deps := buildEphemeralDeps(
		func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return occupiedCfg, nil
		},
		nil,
		stor,
	)
	shape := &createVMShape{
		node:             "pve",
		ephemeralDiskGiB: 4,
		ephemeralStorage: "local-lvm",
		vmDiskFormat:     "qcow2",
	}

	_, err := attachEphemeralDisk(context.Background(), deps, log.NewNopLogger(), shape, 101)
	if err == nil {
		t.Fatal("expected error for exhausted scsi slots, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected Cloud error for slot exhaustion; got: %v", err)
	}
	if !strings.Contains(err.Error(), "no free scsi slot") {
		t.Errorf("error missing 'no free scsi slot': %v", err)
	}
	if !deleteCalled {
		t.Error("DeleteVolumeAsync not called after slot exhaustion — orphan volume would be left behind")
	}
}

// --------------------------------------------------------------------------
// configureAgent ephemeralDevPath tests
// --------------------------------------------------------------------------

// TestConfigureAgent_EphemeralDevPath_Set verifies that a non-empty
// ephemeralDevPath is set on agentCfg.Disks.Ephemeral.
func TestConfigureAgent_EphemeralDevPath_Set(t *testing.T) {
	t.Parallel()
	testConfigureAgentEphemeralPath(t,
		"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2",
		"/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi2",
	)
}

// TestConfigureAgent_EphemeralDevPath_Empty verifies that an empty
// ephemeralDevPath leaves agentCfg.Disks.Ephemeral as empty string.
func TestConfigureAgent_EphemeralDevPath_Empty(t *testing.T) {
	t.Parallel()
	testConfigureAgentEphemeralPath(t, "", "")
}

// testConfigureAgentEphemeralPath calls configureAgent with the given
// ephemeralDevPath and asserts agentCfg.Disks.Ephemeral equals wantEphemeral.
func testConfigureAgentEphemeralPath(t *testing.T, inputPath, wantEphemeral string) {
	t.Helper()

	cfg := &config.CPIConfig{}
	cfg.ApplyDefaults()
	cfg.AgentMBus = "nats://mbus.test:4222"

	var gotEphemeral string
	fakeAgent := &dsEphemeralAgent{
		configureFn: func(_ context.Context, _ string, _ int, ac agent.AgentConfig) error {
			gotEphemeral = ac.Disks.Ephemeral
			return nil
		},
	}

	deps := Deps{
		Config: cfg,
		PVE:    &diskSizingPVE{qemu: &diskSizingQEMU{}},
		Agent:  fakeAgent,
		Logger: log.NewNopLogger(),
	}
	parsed := &createVMParsedArgs{
		agentID:  "agent-eph-1",
		networks: map[string]createVMNetworkSpec{"default": {Type: "manual", IP: "10.0.0.5"}},
		env:      map[string]any{},
	}
	shape := &createVMShape{node: "pve"}

	err := configureAgent(context.Background(), deps, log.NewNopLogger(), parsed, shape, 200, "vm-200", inputPath)
	if err != nil {
		t.Fatalf("configureAgent returned error: %v", err)
	}
	if gotEphemeral != wantEphemeral {
		t.Errorf("Disks.Ephemeral = %q; want %q", gotEphemeral, wantEphemeral)
	}
}

// dsEphemeralAgent implements agent.Agent for configureAgent ephemeral path tests.
type dsEphemeralAgent struct {
	configureFn func(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error
}

func (a *dsEphemeralAgent) Configure(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error {
	return a.configureFn(ctx, node, vmid, cfg)
}
func (a *dsEphemeralAgent) Remove(_ context.Context, _ string, _ int) error { return nil }

var _ agent.Agent = (*dsEphemeralAgent)(nil)
