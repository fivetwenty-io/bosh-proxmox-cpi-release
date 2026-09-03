// Pre-delete state guard for delete_disk.
//
// Deleting a disk volume out from under a VM that is mid-backup, mid-clone,
// mid-migrate, mid-snapshot, mid-rollback, or still being created can corrupt
// the in-flight operation or leave PVE's storage layer inconsistent. PVE itself
// only takes a storage-level lock for the imgdel task, not a guest-level
// interlock, so a delete_disk that arrives while the attached guest holds a
// config lock is not rejected by the API — it races.
//
// GuardDiskDeleteState closes that window on a best-effort basis. The VMID baked
// into a managed volid name ("vm-<VMID>-disk-<N>") is only the allocation-time
// placeholder this CPI assigns at create_disk; BOSH later attaches the volume to
// a different guest without renaming it, so the lock that matters belongs to the
// guest the disk is CURRENTLY ATTACHED to. The guard finds that guest by
// scanning VM configs for the volid (FindVMByDiskVolid), reads its config lock,
// and asks the director to retry later when the lock indicates a destructive or
// in-flight operation. Callers gate on the feature knob (config.CPIConfig.
// DiskDeleteStateGuardEnabled), which defaults to enabled as of Phase 1 — set
// pve.disk_delete_state_guard: "off" to opt out and restore the pre-Phase-1
// no-lookup behavior. When resolution fails transiently (a network blip or
// 5xx mid-scan), the guard also
// defers the delete as retriable — an unknown holder state is exactly the
// condition the guard exists to protect against. Only permanent outcomes fail
// open: a disk that is not attached to any VM — the normal state at delete
// time, after BOSH has detached it — has no guest to interrogate and is
// allowed straight through, as is a permanent (non-retriable) resolution
// failure the director could never clear by retrying.
package pve

import (
	"context"
	"errors"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// destructiveDiskLocks is the set of PVE guest-config lock values during which
// the attached VM's disks must not be deleted: an in-flight backup, clone,
// migrate, snapshot, or rollback is actively reading or rewriting disk state,
// and "create" means the guest (and its disks) is still materialising. Other
// lock values (e.g. "suspended", "suspending") do not mutate disk contents and
// are treated as safe so the guard stays narrow.
var destructiveDiskLocks = map[string]struct{}{
	"backup":   {},
	"clone":    {},
	"migrate":  {},
	"snapshot": {},
	"rollback": {},
	"create":   {},
}

// isDestructiveDiskLock reports whether lock is one of the disk-mutating /
// in-flight states during which a disk delete must be deferred.
func isDestructiveDiskLock(lock string) bool {
	_, ok := destructiveDiskLocks[strings.ToLower(strings.TrimSpace(lock))]
	return ok
}

// isRetriableCPIError reports whether err carries a *cpierrors.Error anywhere
// in its chain whose type the BOSH Director may retry (ok_to_retry).
func isRetriableCPIError(err error) bool {
	var ce *cpierrors.Error
	return errors.As(err, &ce) && ce.OkToRetry()
}

// readGuestLock returns the config lock string of the guest vmid on node.
//
//   - (lock, false, nil) — config read succeeded; lock is "" when unlocked.
//   - ("", true, nil)    — the guest is gone (404); caller should proceed.
//   - ("", false, err)   — any other config-read error.
func readGuestLock(ctx context.Context, c Client, node string, vmid int) (string, bool, error) {
	cfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if IsNotFound(err) {
			return "", true, nil
		}
		return "", false, err
	}
	if cfg == nil {
		return "", false, nil
	}
	lock, _ := ConfigString(cfg, "lock")
	return lock, false, nil
}

// GuardDiskDeleteState reports whether the disk identified by volid may be
// deleted right now, given the lock state of the VM it is attached to.
//
// It returns a TypeRetriableCloud error when the attached guest holds a
// destructive/in-flight lock (so the BOSH Director re-drives delete_disk after
// the operation completes), and also when holder resolution or the lock read
// fails TRANSIENTLY — an unresolved holder state must defer the delete, not
// wave it through. It returns nil for every permanent outcome: the disk is
// attached to no VM, the guest is gone, the guest is unlocked, the lock is a
// non-destructive value, or resolution failed with a non-retriable error the
// director could never clear by retrying (the guard stays best-effort there
// rather than converting a permanent guard fault into a permanent delete
// failure).
func GuardDiskDeleteState(ctx context.Context, c Client, volid string) error {
	if c == nil || volid == "" {
		return nil
	}

	// Resolve the VM the disk is CURRENTLY ATTACHED to by scanning VM configs
	// for the volid. A retriable error means the scan hit a transient fault
	// and the holder state is unknown — defer the delete rather than risk
	// pulling a disk out from under an in-flight operation. A non-retriable
	// error means the disk is attached to no VM (the normal pre-delete state)
	// or resolution failed permanently: no in-flight guest to guard against,
	// so allow the delete.
	vmid, node, err := FindVMByDiskVolid(ctx, c, volid)
	if err != nil {
		if isRetriableCPIError(err) {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				"delete_disk: could not resolve disk holder; deferring delete")
		}
		return nil
	}

	lock, gone, lockErr := readGuestLock(ctx, c, node, vmid)
	if lockErr != nil {
		// readGuestLock returns the raw SDK error; classify it. Transient
		// faults defer the delete (the holder's lock state is unknown);
		// permanent read failures keep the guard best-effort and fail open.
		if wrapped := WrapError(lockErr); isRetriableCPIError(wrapped) {
			return cpierrors.WrapAs(wrapped, cpierrors.TypeRetriableCloud,
				"delete_disk: could not read holder lock state; deferring delete")
		}
		return nil
	}
	if gone {
		// Guest vanished mid-check: delete is idempotent, allow it.
		return nil
	}

	if isDestructiveDiskLock(lock) {
		return cpierrors.WrapAs(
			cpierrors.Cloud(
				"delete_disk: VM %d (node %q) holding disk %q is locked (%q); deferring delete",
				vmid, node, volid, lock),
			cpierrors.TypeRetriableCloud,
			"delete_disk: attached VM busy")
	}
	return nil
}
