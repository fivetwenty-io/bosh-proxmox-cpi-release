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
// in-flight operation. It is opt-in (callers gate on the feature knob) and fails
// open on any resolution uncertainty so it can never convert a guard hiccup into
// a delete failure. A disk that is not attached to any VM — the normal state at
// delete time, after BOSH has detached it — has no guest to interrogate and is
// allowed straight through.
package pve

import (
	"context"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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
	lock, _ := cfg["lock"].(string)
	return lock, false, nil
}

// GuardDiskDeleteState reports whether the disk identified by volid may be
// deleted right now, given the lock state of the VM it is attached to.
//
// It returns a TypeRetriableCloud error when the attached guest holds a
// destructive/in-flight lock (so the BOSH Director re-drives delete_disk after
// the operation completes), and nil in every other case — including when the
// disk is attached to no VM, the guest is gone, the guest is unlocked, the lock
// is a non-destructive value, or attachment could not be resolved. The guard is
// intentionally best-effort: it adds safety when it can conclude and never
// blocks a delete when it cannot.
//
// fallbackNode is the configured default node, used by FindVMByDiskVolid only
// for cluster rows that omit the node field (rare in modern PVE).
func GuardDiskDeleteState(ctx context.Context, c Client, fallbackNode, volid string) error {
	if c == nil || volid == "" {
		return nil
	}

	// Resolve the VM the disk is CURRENTLY ATTACHED to by scanning VM configs
	// for the volid. An error here means the disk is attached to no VM (the
	// normal pre-delete state) or that resolution failed (best-effort): either
	// way there is no in-flight guest to guard against, so allow the delete.
	vmid, node, err := FindVMByDiskVolid(ctx, c, fallbackNode, volid)
	if err != nil {
		return nil
	}

	lock, gone, lockErr := readGuestLock(ctx, c, node, vmid)
	if lockErr != nil || gone {
		// Config read failed (fail open) or the guest vanished mid-check
		// (idempotent): allow the delete to proceed.
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
