// fakes_storage_test.go — fakeStorageService, shared by tracing_storage_test.go's
// storage decorator matrix tests.
package pve

import (
	"context"
	"io"
)

// fakeStorageService implements storage.Service for tests. Every method the
// tracedStorageService decorator overrides (CreateVolume, Exists,
// DeleteVolumeAsync, DeleteVolumeIfExists, DeleteVolumeIfExistsAsync, Upload)
// is wired through a settable closure field; DeleteVolume is not overridden
// by the decorator (and is forbidden at production call sites by the
// sync-DeleteVolume lint guard), so it panics if a test ever reaches it.
type fakeStorageService struct {
	createVolumeFn              func(ctx context.Context, node, storageName string, sizeGiB int, format string, vmid int, name string) (string, error)
	existsFn                    func(ctx context.Context, node, storageName, volume string) (bool, error)
	deleteVolumeAsyncFn         func(ctx context.Context, node, storageName, volume string) (string, error)
	deleteVolumeIfExistsFn      func(ctx context.Context, node, storageName, volume string) (bool, error)
	deleteVolumeIfExistsAsyncFn func(ctx context.Context, node, storageName, volume string) (bool, string, error)
	uploadFn                    func(ctx context.Context, node, storageName, content, filename string, body io.Reader) (string, error)
}

func (f *fakeStorageService) CreateVolume(ctx context.Context, node, storageName string, sizeGiB int, format string, vmid int, name string) (string, error) {
	if f.createVolumeFn != nil {
		return f.createVolumeFn(ctx, node, storageName, sizeGiB, format, vmid, name)
	}
	panic("fakeStorageService: CreateVolume not wired")
}

func (f *fakeStorageService) DeleteVolume(context.Context, string, string, string) error {
	panic("fakeStorageService: DeleteVolume unexpected call (sync DeleteVolume is forbidden at call sites)")
}

func (f *fakeStorageService) DeleteVolumeIfExists(ctx context.Context, node, storageName, volume string) (bool, error) {
	if f.deleteVolumeIfExistsFn != nil {
		return f.deleteVolumeIfExistsFn(ctx, node, storageName, volume)
	}
	panic("fakeStorageService: DeleteVolumeIfExists not wired")
}

func (f *fakeStorageService) DeleteVolumeAsync(ctx context.Context, node, storageName, volume string) (string, error) {
	if f.deleteVolumeAsyncFn != nil {
		return f.deleteVolumeAsyncFn(ctx, node, storageName, volume)
	}
	panic("fakeStorageService: DeleteVolumeAsync not wired")
}

func (f *fakeStorageService) DeleteVolumeIfExistsAsync(ctx context.Context, node, storageName, volume string) (bool, string, error) {
	if f.deleteVolumeIfExistsAsyncFn != nil {
		return f.deleteVolumeIfExistsAsyncFn(ctx, node, storageName, volume)
	}
	panic("fakeStorageService: DeleteVolumeIfExistsAsync not wired")
}

func (f *fakeStorageService) Exists(ctx context.Context, node, storageName, volume string) (bool, error) {
	if f.existsFn != nil {
		return f.existsFn(ctx, node, storageName, volume)
	}
	panic("fakeStorageService: Exists not wired")
}

func (f *fakeStorageService) Upload(ctx context.Context, node, storageName, content, filename string, body io.Reader) (string, error) {
	if f.uploadFn != nil {
		return f.uploadFn(ctx, node, storageName, content, filename, body)
	}
	panic("fakeStorageService: Upload not wired")
}
