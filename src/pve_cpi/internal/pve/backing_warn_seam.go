package pve

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// duplicateBackingWarnOnce gates the duplicate-backing warning to a single
// firing per process, across EVERY path that builds a full StorageInfo index —
// StorageInfoCache.refresh (the deploy path) and create_stemcell's PolicyDeps
// adapter (the `bosh upload-stemcell` path, which never touches the cache).
// A process-wide gate rather than a per-cache one because the two paths would
// otherwise each warn about the same storage.cfg, and because storage.cfg
// duplicate-backing misconfiguration is static operator state: it does not
// flip on a routine TTL refresh, so re-warning would be process-lifetime log
// noise for a condition that cannot change without an operator edit and a CPI
// restart.
//
// Held in an atomic.Pointer rather than a plain sync.Once so
// ResetDuplicateBackingWarnOnceForTest can swap in a fresh gate race-safely.
// This mirrors the package-level seam pattern used for the backoff curves
// (pushback_seam.go, storage_lock_seam.go).
var duplicateBackingWarnOnce atomic.Pointer[sync.Once]

func init() {
	duplicateBackingWarnOnce.Store(&sync.Once{})
}

// WarnDuplicateBackingStoragesOnce runs WarnDuplicateBackingStorages at most
// once per process. Callers pass the FULL storage index they just decoded;
// passing a partial index would let whichever path happens to run first fix a
// misleading (incomplete) warning as the process's only one.
//
// Safe to call from any path that has just decoded /storage — it is pure
// logging over the caller's slice and never re-enters the caller.
func WarnDuplicateBackingStoragesOnce(ctx context.Context, infos []StorageInfo) {
	duplicateBackingWarnOnce.Load().Do(func() {
		WarnDuplicateBackingStorages(ctx, infos)
	})
}

// ResetDuplicateBackingWarnOnceForTest re-arms the process-wide gate for a test
// and returns a restore function. Exported (in a non-test file) so tests in
// other packages — the create_stemcell PolicyDeps path lives in
// internal/cpi/handlers — can drive the warning deterministically.
//
// Tests using this seam must NOT call t.Parallel: the gate is process-wide, so
// two parallel tests would race over which one gets the single firing.
//
//	defer pve.ResetDuplicateBackingWarnOnceForTest()()
func ResetDuplicateBackingWarnOnceForTest() func() {
	prev := duplicateBackingWarnOnce.Swap(&sync.Once{})
	return func() {
		duplicateBackingWarnOnce.Store(prev)
	}
}

// sharedExportAdviceGiven gates the shared-export advice line to one firing
// per process. The gate is consumed only when the line actually fires, so a
// refresh that finds no network file storage, or bands an operator already
// set, leaves it armed for a later refresh. Held in an atomic.Pointer so
// ResetSharedExportAdviceOnceForTest can swap in a fresh gate race-safely.
var sharedExportAdviceGiven atomic.Pointer[atomic.Bool]

func init() {
	sharedExportAdviceGiven.Store(&atomic.Bool{})
}

// AdviseSharedExportBandsOnce logs, at most once per process, the advice that
// a cluster with network file storage should give each cluster that mounts the
// same export its own VMID bands. It fires only when infos holds an enabled
// nfs, cifs, or shared dir storage that carries guest images or ISOs, and
// every band in bands is at its built-in value. It is pure logging over the
// caller's slice.
func AdviseSharedExportBandsOnce(ctx context.Context, infos []StorageInfo, bands VMIDBands) {
	if !bands.AllDefault() {
		return
	}
	storageName := sharedExportCandidate(infos)
	if storageName == "" {
		return
	}
	if !sharedExportAdviceGiven.Load().CompareAndSwap(false, true) {
		return
	}
	log.FromContext(ctx).Info("pve: this cluster has network file storage and every VMID band is at its default; "+
		"if another cluster mounts the same export, give each cluster disjoint vm, disk, stemcell template, "+
		"and parker bands and set the datacenter next-id range outside them, then run "+
		"`pve-cid ha --cpi-config` to check the pair",
		log.String("storage", storageName),
		log.String("bands", bands.String()))
}

// sharedExportCandidate returns the name of the first enabled network file
// storage in infos (by name, so the line is deterministic) that carries guest
// images or ISOs, or "" when there is none.
func sharedExportCandidate(infos []StorageInfo) string {
	best := ""
	for i := range infos {
		info := &infos[i]
		if info.Disabled {
			continue
		}
		switch info.Type {
		case "nfs", "cifs":
		case "dir":
			if !info.Shared {
				continue
			}
		default:
			continue
		}
		if !hasImagesContent(info.Content) && !hasContentType(info.Content, "iso") {
			continue
		}
		if best == "" || info.Name < best {
			best = info.Name
		}
	}
	return best
}

// ResetSharedExportAdviceOnceForTest re-arms the process-wide gate for the
// shared-export advice and returns a restore function. Tests using it must
// not call t.Parallel.
func ResetSharedExportAdviceOnceForTest() func() {
	prev := sharedExportAdviceGiven.Swap(&atomic.Bool{})
	return func() {
		sharedExportAdviceGiven.Store(prev)
	}
}
