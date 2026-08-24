package pve_test

import (
	"context"
	"errors"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// guardFakeQEMU is a qemu.Service whose Config returns a canned map (the
// attached VM's config, including its disk slots and lock) or an error. The
// same config answers every VMID, which is sufficient because the guard tests
// place a single VM in the cluster.
type guardFakeQEMU struct {
	qemu.Service
	cfg map[string]any
	err error
}

func (q *guardFakeQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	if q.err != nil {
		return nil, q.err
	}
	return q.cfg, nil
}

// guardAttachedClient wires a cluster that lists a single VM (vmid on node) and
// a QEMU service whose config attaches volid at scsi0 with the given lock. This
// is the realistic topology: the disk-name VMID is a placeholder, and the disk
// is attached to a different, real VM whose lock is what the guard must read.
func guardAttachedClient(volid, lock, node string) pve.Client {
	cfg := map[string]any{"scsi0": volid}
	if lock != "" {
		cfg["lock"] = lock
	}
	// 9002 is the real attached VM — deliberately distinct from the placeholder
	// VMID baked into the volid name (9001), proving the guard resolves by
	// attachment, not by name.
	const attachedVMID = 9002
	return &diskClusterClient{
		qemuSvc: &guardFakeQEMU{cfg: cfg},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(attachedVMID), "node": node}), nil
			},
		},
	}
}

// guardUnattachedClient lists a VM whose config does NOT reference volid, so the
// disk resolves as attached to no VM.
func guardUnattachedClient() pve.Client {
	return &diskClusterClient{
		qemuSvc: &guardFakeQEMU{cfg: map[string]any{"scsi0": "other:vm-1-disk-0", "lock": "migrate"}},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(4242), "node": "pve-01"}), nil
			},
		},
	}
}

// guardClientListErr wires a cluster service that always errors, to exercise
// the fail-open path when attachment resolution cannot complete.
func guardClientListErr(listErr error) pve.Client {
	return &diskClusterClient{
		qemuSvc: &guardFakeQEMU{},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return nil, listErr
			},
		},
	}
}

const guardVolid = "local-lvm:vm-9001-disk-0"

func TestGuardDiskDeleteState_NotAttached_Proceeds(t *testing.T) {
	t.Parallel()
	// The disk is attached to no VM (the normal pre-delete state): nothing to
	// guard, even though some unrelated VM is mid-migrate.
	if err := pve.GuardDiskDeleteState(context.Background(), guardUnattachedClient(), guardVolid); err != nil {
		t.Errorf("not attached: want proceed (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_AttachedUnlocked_Proceeds(t *testing.T) {
	t.Parallel()
	c := guardAttachedClient(guardVolid, "", "pve-01")
	if err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid); err != nil {
		t.Errorf("attached unlocked: want proceed (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_AttachedDestructiveLock_Retriable(t *testing.T) {
	t.Parallel()
	for _, lock := range []string{"backup", "clone", "migrate", "snapshot", "rollback", "create"} {
		c := guardAttachedClient(guardVolid, lock, "pve-02")
		err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid)
		if err == nil {
			t.Fatalf("lock=%q: want error, got nil", lock)
		}
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Errorf("lock=%q: want retriable-cloud, got %v", lock, err)
		}
	}
}

func TestGuardDiskDeleteState_AttachedNonDestructiveLock_Proceeds(t *testing.T) {
	t.Parallel()
	// A lock outside the destructive set (e.g. "suspended") must not block the
	// delete — the guard only fires on known data-mutating locks.
	c := guardAttachedClient(guardVolid, "suspended", "pve-01")
	if err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid); err != nil {
		t.Errorf("non-destructive lock: want proceed (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_AttachedDirStyleVolid_Retriable(t *testing.T) {
	t.Parallel()
	// dir-style volid embeds the placeholder VMID in a subpath; the guard must
	// still match it against the attached VM's disk slot and apply the lock check.
	const dirVolid = "local:9001/vm-9001-disk-0.raw"
	c := guardAttachedClient(dirVolid, "snapshot", "pve-01")
	err := pve.GuardDiskDeleteState(context.Background(), c, dirVolid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("dir-style volid: want retriable-cloud, got %v", err)
	}
}

func TestGuardDiskDeleteState_AttachedVMConfig404_Proceeds(t *testing.T) {
	t.Parallel()
	// FindVMByDiskVolid skips VMs whose config 404s (deleted concurrently), so
	// a not-found during resolution surfaces as "not attached" → proceed.
	c := &diskClusterClient{
		qemuSvc: &guardFakeQEMU{err: &sdkerrors.APIError{HTTPCode: 404, Message: "VM not found"}},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(9002), "node": "pve-01"}), nil
			},
		},
	}
	if err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid); err != nil {
		t.Errorf("config 404 during resolution: want proceed (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_PermanentResolutionError_FailsOpen(t *testing.T) {
	t.Parallel()
	// Cluster listing fails with a permanent (non-retriable) error: the guard is
	// best-effort and must fail open rather than turn a permanent guard fault
	// into a permanent delete failure.
	c := guardClientListErr(errors.New("permission denied"))
	if err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid); err != nil {
		t.Errorf("permanent resolution error: want fail-open (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_TransientConfigErrorDuringScan_Retriable(t *testing.T) {
	t.Parallel()
	// A transient per-VM Config fault during holder resolution leaves the
	// holder state unknown: the guard must defer the delete as retriable, not
	// wave it through as "not attached".
	c := &diskClusterClient{
		qemuSvc: &guardFakeQEMU{err: &sdkerrors.APIError{HTTPCode: 500, Message: "pvedaemon worker cycling"}},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(9002), "node": "pve-01"}), nil
			},
		},
	}
	err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient config error during scan: want retriable-cloud, got %v", err)
	}
}

// guardLockReadFailQEMU answers the first Config call (the holder scan) with a
// config that attaches the disk, then fails every subsequent call (the lock
// read) with lockErr.
type guardLockReadFailQEMU struct {
	qemu.Service
	cfg     map[string]any
	lockErr error
	calls   int
}

func (q *guardLockReadFailQEMU) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	q.calls++
	if q.calls == 1 {
		return q.cfg, nil
	}
	return nil, q.lockErr
}

// guardLockReadFailClient wires a single-VM cluster whose holder scan succeeds
// (disk attached at scsi0) and whose subsequent lock read fails with lockErr.
func guardLockReadFailClient(lockErr error) pve.Client {
	return &diskClusterClient{
		qemuSvc: &guardLockReadFailQEMU{cfg: map[string]any{"scsi0": guardVolid}, lockErr: lockErr},
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(9002), "node": "pve-01"}), nil
			},
		},
	}
}

func TestGuardDiskDeleteState_TransientLockReadError_Retriable(t *testing.T) {
	t.Parallel()
	// The holder resolved but its lock state could not be read due to a
	// transient fault: the guard must defer the delete as retriable.
	c := guardLockReadFailClient(&sdkerrors.TimeoutError{Operation: "qemu config", Duration: "5s"})
	err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient lock-read error: want retriable-cloud, got %v", err)
	}
}

func TestGuardDiskDeleteState_PermanentLockReadError_FailsOpen(t *testing.T) {
	t.Parallel()
	// A permanent lock-read failure (e.g. 403) keeps the guard best-effort:
	// retrying cannot clear it, so fail open and allow the delete.
	c := guardLockReadFailClient(&sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"})
	if err := pve.GuardDiskDeleteState(context.Background(), c, guardVolid); err != nil {
		t.Errorf("permanent lock-read error: want fail-open (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_EmptyVolid_Proceeds(t *testing.T) {
	t.Parallel()
	if err := pve.GuardDiskDeleteState(context.Background(), guardAttachedClient(guardVolid, "migrate", "pve-01"), ""); err != nil {
		t.Errorf("empty volid: want proceed (nil), got %v", err)
	}
}

func TestGuardDiskDeleteState_NilClient_FailsOpen(t *testing.T) {
	t.Parallel()
	if err := pve.GuardDiskDeleteState(context.Background(), nil, guardVolid); err != nil {
		t.Errorf("nil client: want fail-open (nil), got %v", err)
	}
}
