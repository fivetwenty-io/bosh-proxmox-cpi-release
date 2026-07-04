// fakes_qemu_test.go — fakeQEMUService, shared by tracing_test.go's QEMU
// exemplar tests and by the QEMU full-matrix tests.
package pve

import (
	"context"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// fakeQEMUService implements qemu.Service for tests. Every method the
// tracedQEMUService decorator overrides is wired via a settable *Fn field;
// leaving a field nil and calling the method panics, since no test here
// should reach an unwired method. Clone is the one exception (not
// overridden by the decorator) and keeps its own dedicated field.
type fakeQEMUService struct {
	configFn         func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	cloneFn          func(ctx context.Context, node string, vmid int, params map[string]interface{}) (string, error)
	createFn         func(ctx context.Context, node string, params map[string]interface{}) (string, error)
	statusFn         func(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	startFn          func(ctx context.Context, node string, vmid int) (string, error)
	stopFn           func(ctx context.Context, node string, vmid int) (string, error)
	resetFn          func(ctx context.Context, node string, vmid int) (string, error)
	attachDiskFn     func(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error)
	detachDiskFn     func(ctx context.Context, node string, vmid int, diskID string) error
	resizeDiskFn     func(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error)
	snapshotFn       func(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error)
	deleteSnapshotFn func(ctx context.Context, node string, vmid int, name string) error
	listSnapshotsFn  func(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error)
}

func (f *fakeQEMUService) Create(ctx context.Context, node string, params map[string]interface{}) (string, error) {
	if f.createFn != nil {
		return f.createFn(ctx, node, params)
	}
	panic("fakeQEMUService: Create not wired")
}

func (f *fakeQEMUService) Config(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if f.configFn != nil {
		return f.configFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: Config not wired")
}

func (f *fakeQEMUService) Status(ctx context.Context, node string, vmid int) (map[string]interface{}, error) {
	if f.statusFn != nil {
		return f.statusFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: Status not wired")
}

func (f *fakeQEMUService) Start(ctx context.Context, node string, vmid int) (string, error) {
	if f.startFn != nil {
		return f.startFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: Start not wired")
}

func (f *fakeQEMUService) Stop(ctx context.Context, node string, vmid int) (string, error) {
	if f.stopFn != nil {
		return f.stopFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: Stop not wired")
}

func (f *fakeQEMUService) Reset(ctx context.Context, node string, vmid int) (string, error) {
	if f.resetFn != nil {
		return f.resetFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: Reset not wired")
}

func (f *fakeQEMUService) Clone(ctx context.Context, node string, vmid int, params map[string]interface{}) (string, error) {
	if f.cloneFn != nil {
		return f.cloneFn(ctx, node, vmid, params)
	}
	panic("fakeQEMUService: Clone not wired")
}

func (f *fakeQEMUService) Template(context.Context, string, int) (string, error) {
	panic("fakeQEMUService: Template unexpected call")
}

func (f *fakeQEMUService) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (string, error) {
	if f.attachDiskFn != nil {
		return f.attachDiskFn(ctx, node, vmid, volid, bus, opts)
	}
	panic("fakeQEMUService: AttachDisk not wired")
}

func (f *fakeQEMUService) DetachDisk(ctx context.Context, node string, vmid int, diskID string) error {
	if f.detachDiskFn != nil {
		return f.detachDiskFn(ctx, node, vmid, diskID)
	}
	panic("fakeQEMUService: DetachDisk not wired")
}

func (f *fakeQEMUService) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (string, error) {
	if f.resizeDiskFn != nil {
		return f.resizeDiskFn(ctx, node, vmid, diskID, sizeGiB)
	}
	panic("fakeQEMUService: ResizeDisk not wired")
}

func (f *fakeQEMUService) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (string, error) {
	if f.snapshotFn != nil {
		return f.snapshotFn(ctx, node, vmid, name, opts)
	}
	panic("fakeQEMUService: Snapshot not wired")
}

func (f *fakeQEMUService) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) error {
	if f.deleteSnapshotFn != nil {
		return f.deleteSnapshotFn(ctx, node, vmid, name)
	}
	panic("fakeQEMUService: DeleteSnapshot not wired")
}

func (f *fakeQEMUService) ListSnapshots(ctx context.Context, node string, vmid int) ([]map[string]interface{}, error) {
	if f.listSnapshotsFn != nil {
		return f.listSnapshotsFn(ctx, node, vmid)
	}
	panic("fakeQEMUService: ListSnapshots not wired")
}

func (f *fakeQEMUService) RollbackSnapshot(context.Context, string, int, string) (string, error) {
	panic("fakeQEMUService: RollbackSnapshot unexpected call")
}
