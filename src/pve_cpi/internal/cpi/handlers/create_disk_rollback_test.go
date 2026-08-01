package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// rollbackCreatedVolume: direct unit tests. The deferred call site inside
// create_disk's HandleCreateDisk closure (create_disk.go ~line 450) is only
// reachable through a step that fails after attemptCreateVolume succeeds, and
// every step in that window (size-warning, tag application) is deliberately
// best-effort/warn-only in the current implementation — there is no
// non-panic path from HandleCreateDisk into rollbackCreatedVolume today.
// These tests exercise the production rollback function directly with a
// minimal fake pve.Client, asserting its actual DeleteVolumeAsync/await
// contract rather than reimplementing it.
// ---------------------------------------------------------------------------

// rollbackStorage is a minimal sdkstorage.Service fake recording
// DeleteVolumeAsync calls.
type rollbackStorage struct {
	sdkstorage.Service
	deleteAsyncCalls []struct{ node, storage, volume string }
	deleteAsyncUPID  string
	deleteAsyncErr   error
}

func (s *rollbackStorage) DeleteVolumeAsync(_ context.Context, node, storage, volume string) (string, error) {
	s.deleteAsyncCalls = append(s.deleteAsyncCalls, struct{ node, storage, volume string }{node, storage, volume})
	return s.deleteAsyncUPID, s.deleteAsyncErr
}

// rollbackTasks is a minimal sdktasks.Service fake recording Wait calls.
type rollbackTasks struct {
	sdktasks.Service
	waitCalls  []struct{ node, upid string }
	waitStatus *sdktasks.Status
	waitErr    error
}

func (t *rollbackTasks) Wait(_ context.Context, node, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	t.waitCalls = append(t.waitCalls, struct{ node, upid string }{node, upid})
	if t.waitStatus != nil {
		return t.waitStatus, t.waitErr
	}
	return &sdktasks.Status{ExitStatus: "OK"}, t.waitErr
}

// rollbackClient implements pve.Client, wiring only Storage()/Tasks() to
// fakes; every other service is unused by rollbackCreatedVolume and returns
// nil so an unintended call panics loudly.
type rollbackClient struct {
	storage sdkstorage.Service
	tasks   sdktasks.Service
}

func (c *rollbackClient) QEMU() sdkqemu.Service                     { return nil }
func (c *rollbackClient) Nodes() sdknodes.Service                   { return nil }
func (c *rollbackClient) Storage() sdkstorage.Service               { return c.storage }
func (c *rollbackClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *rollbackClient) Tasks() sdktasks.Service                   { return c.tasks }
func (c *rollbackClient) Cluster() sdkcluster.Service               { return nil }
func (c *rollbackClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *rollbackClient) Pools() pve.PoolService                    { return nil }

func TestRollbackCreatedVolume_DeletesAndAwaitsUPID(t *testing.T) {
	t.Parallel()

	const node = "pve1"
	const storagePool = "local-lvm"
	const canonicalVolID = "local-lvm:vm-9001-disk-0"
	const upid = "UPID:pve1:0001:0002:imgdel:local-lvm:root@pam:"

	stor := &rollbackStorage{deleteAsyncUPID: upid}
	tsk := &rollbackTasks{}
	deps := Deps{
		PVE:    &rollbackClient{storage: stor, tasks: tsk},
		Logger: log.NewNopLogger(),
	}

	rollbackCreatedVolume(context.Background(), deps, node, storagePool, canonicalVolID, log.NewNopLogger())

	if len(stor.deleteAsyncCalls) != 1 {
		t.Fatalf("DeleteVolumeAsync: want 1 call, got %d", len(stor.deleteAsyncCalls))
	}
	got := stor.deleteAsyncCalls[0]
	if got.node != node || got.storage != storagePool || got.volume != canonicalVolID {
		t.Errorf("DeleteVolumeAsync called with (%q,%q,%q); want (%q,%q,%q)",
			got.node, got.storage, got.volume, node, storagePool, canonicalVolID)
	}
	if len(tsk.waitCalls) != 1 {
		t.Fatalf("Tasks().Wait: want 1 call (await the returned UPID), got %d", len(tsk.waitCalls))
	}
	if tsk.waitCalls[0].upid != upid {
		t.Errorf("Tasks().Wait awaited upid %q; want %q", tsk.waitCalls[0].upid, upid)
	}
	if tsk.waitCalls[0].node != node {
		t.Errorf("Tasks().Wait node = %q; want %q", tsk.waitCalls[0].node, node)
	}
}

func TestRollbackCreatedVolume_SynchronousDelete_NoUPID_NoAwait(t *testing.T) {
	t.Parallel()

	// Empty UPID with nil error means PVE completed the delete synchronously
	// (see sdkstorage.Service.DeleteVolumeAsync doc); no task to await.
	stor := &rollbackStorage{deleteAsyncUPID: ""}
	tsk := &rollbackTasks{}
	deps := Deps{
		PVE:    &rollbackClient{storage: stor, tasks: tsk},
		Logger: log.NewNopLogger(),
	}

	rollbackCreatedVolume(context.Background(), deps, "pve1", "local-lvm", "local-lvm:vm-9002-disk-0", log.NewNopLogger())

	if len(stor.deleteAsyncCalls) != 1 {
		t.Fatalf("DeleteVolumeAsync: want 1 call, got %d", len(stor.deleteAsyncCalls))
	}
	if len(tsk.waitCalls) != 0 {
		t.Errorf("Tasks().Wait: want 0 calls when DeleteVolumeAsync returns no UPID, got %d", len(tsk.waitCalls))
	}
}

func TestRollbackCreatedVolume_DeleteError_NoAwait(t *testing.T) {
	t.Parallel()

	stor := &rollbackStorage{deleteAsyncErr: errors.New("simulated: transport failure")}
	tsk := &rollbackTasks{}
	deps := Deps{
		PVE:    &rollbackClient{storage: stor, tasks: tsk},
		Logger: log.NewNopLogger(),
	}

	rollbackCreatedVolume(context.Background(), deps, "pve1", "local-lvm", "local-lvm:vm-9003-disk-0", log.NewNopLogger())

	if len(stor.deleteAsyncCalls) != 1 {
		t.Fatalf("DeleteVolumeAsync: want 1 call, got %d", len(stor.deleteAsyncCalls))
	}
	if len(tsk.waitCalls) != 0 {
		t.Errorf("Tasks().Wait: want 0 calls when DeleteVolumeAsync itself errors, got %d", len(tsk.waitCalls))
	}
}

func TestRollbackCreatedVolume_AwaitError_DoesNotPanic(t *testing.T) {
	t.Parallel()

	const upid = "UPID:pve1:0001:0002:imgdel:local-lvm:root@pam:"
	stor := &rollbackStorage{deleteAsyncUPID: upid}
	tsk := &rollbackTasks{waitErr: errors.New("simulated: task poll failure")}
	deps := Deps{
		PVE:    &rollbackClient{storage: stor, tasks: tsk},
		Logger: log.NewNopLogger(),
	}

	// rollbackCreatedVolume is a best-effort void function; an await failure
	// must be logged (Warn), never propagated or panicked.
	rollbackCreatedVolume(context.Background(), deps, "pve1", "local-lvm", "local-lvm:vm-9004-disk-0", log.NewNopLogger())

	if len(tsk.waitCalls) != 1 {
		t.Fatalf("Tasks().Wait: want 1 call, got %d", len(tsk.waitCalls))
	}
}
