// Package handlers — internal tests for handlerPolicyDeps, the create_stemcell
// path's pve.PolicyDeps adapter. Covers the two properties the stemcell path
// previously lacked because it decoded /storage inline instead of going
// through pve.ParseStorageEntry: fully populated backing-identity fields, and
// the once-per-process duplicate-backing warning.
package handlers

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// policyDepsLoggerCtx returns a context carrying a logger that writes to the
// returned buffer, so tests can assert on what the stemcell path logged.
func policyDepsLoggerCtx(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("log.NewLogger: %v", err)
	}
	return log.IntoContext(context.Background(), logger), &buf
}

// TestHandlerPolicyDeps_StorageInfoPopulatesBackingIdentity pins the fix for
// the stemcell path returning StorageInfo with Server/Export/Path unset:
// BackingKey() was "" for every entry, so any consumer reaching for backing
// identity through PolicyDeps saw "unknown" — and two unrelated storages both
// keyed "" would compare equal to a naive consumer.
func TestHandlerPolicyDeps_StorageInfoPopulatesBackingIdentity(t *testing.T) {
	t.Parallel()
	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"nfs-a":   {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"nfs-b":   {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/other"},
		"dir-ssd": {storageType: "dir", path: "/mnt/ssd"},
	})
	pd := newHandlerPolicyDeps(deps)

	nfsA, err := pd.StorageInfo(context.Background(), "nfs-a")
	if err != nil {
		t.Fatalf("StorageInfo(nfs-a): %v", err)
	}
	if nfsA.Server != "10.0.0.5" || nfsA.Export != "/tank/proxmox" {
		t.Errorf("nfs backing fields not populated: %+v", nfsA)
	}

	dir, err := pd.StorageInfo(context.Background(), "dir-ssd")
	if err != nil {
		t.Fatalf("StorageInfo(dir-ssd): %v", err)
	}
	if dir.Path != "/mnt/ssd" {
		t.Errorf("dir Path not populated: %+v", dir)
	}

	nfsB, err := pd.StorageInfo(context.Background(), "nfs-b")
	if err != nil {
		t.Fatalf("StorageInfo(nfs-b): %v", err)
	}

	// The latent trap: with the fields unpopulated every key was "", so two
	// unrelated storages carried identical (empty) backing identity.
	for _, info := range []pve.StorageInfo{nfsA, nfsB, dir} {
		if info.BackingKey() == "" {
			t.Errorf("storage %q: BackingKey() is empty — backing identity unusable from the stemcell path", info.Name)
		}
	}
	if nfsA.BackingKey() == nfsB.BackingKey() {
		t.Errorf("two storages on different exports must not share a backing key (%q)", nfsA.BackingKey())
	}
	if pve.SameBacking(nfsA, nfsB) {
		t.Error("SameBacking must be false for different exports")
	}
}

// TestHandlerPolicyDeps_StorageInfoPreservesSharedAndNodes guards the fields
// the inline decoder did populate, so routing through pve.ParseStorageEntry
// does not regress them.
func TestHandlerPolicyDeps_StorageInfoPreservesSharedAndNodes(t *testing.T) {
	t.Parallel()
	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"local-lvm": {storageType: "lvm", shared: false},
		"ceph":      {storageType: "rbd", shared: true},
	})
	pd := newHandlerPolicyDeps(deps)

	lvm, err := pd.StorageInfo(context.Background(), "local-lvm")
	if err != nil {
		t.Fatalf("StorageInfo(local-lvm): %v", err)
	}
	if lvm.Name != "local-lvm" || lvm.Type != "lvm" || lvm.Shared {
		t.Errorf("local-lvm decoded wrong: %+v", lvm)
	}

	ceph, err := pd.StorageInfo(context.Background(), "ceph")
	if err != nil {
		t.Fatalf("StorageInfo(ceph): %v", err)
	}
	if !ceph.Shared {
		t.Errorf("ceph shared flag lost: %+v", ceph)
	}

	if _, err := pd.StorageInfo(context.Background(), "absent"); err == nil {
		t.Error("expected an error for a storage absent from the index")
	}
}

// TestHandlerPolicyDeps_WarnsDuplicateBackingOncePerProcess pins the other half
// of the fix: a create_stemcell-only workload (`bosh upload-stemcell` before any
// deploy) never touches StorageInfoCache, so before this the duplicate-backing
// warning could not fire at all on that path. It must fire, and it must stay
// once per process rather than once per call.
func TestHandlerPolicyDeps_WarnsDuplicateBackingOncePerProcess(t *testing.T) {
	defer pve.ResetDuplicateBackingWarnOnceForTest()()

	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"nfs-a":     {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"nfs-b":     {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/proxmox"},
		"local-lvm": {storageType: "lvm"},
	})
	pd := newHandlerPolicyDeps(deps)
	ctx, buf := policyDepsLoggerCtx(t)

	for i := 0; i < 3; i++ {
		if _, err := pd.StorageInfo(ctx, "nfs-a"); err != nil {
			t.Fatalf("StorageInfo call %d: %v", i+1, err)
		}
	}

	logged := buf.String()
	occurrences := strings.Count(logged, "two or more storage IDs share one physical backing")
	if occurrences != 1 {
		t.Fatalf("expected the duplicate-backing warning exactly once across 3 calls, got %d: %s", occurrences, logged)
	}
	if !strings.Contains(logged, "nfs-a") || !strings.Contains(logged, "nfs-b") {
		t.Errorf("expected both duplicate storage IDs named: %s", logged)
	}
	if strings.Contains(logged, "local-lvm") {
		t.Errorf("a distinct-backing storage must not appear in the warning: %s", logged)
	}
}

// TestHandlerPolicyDeps_NoWarningWhenNoDuplicateBacking keeps the common case
// silent.
func TestHandlerPolicyDeps_NoWarningWhenNoDuplicateBacking(t *testing.T) {
	defer pve.ResetDuplicateBackingWarnOnceForTest()()

	deps := storageLookupDeps(map[string]dlbStorageEntry{
		"nfs-a":     {storageType: "nfs", shared: true, server: "10.0.0.5", export: "/tank/a"},
		"local-lvm": {storageType: "lvm"},
	})
	pd := newHandlerPolicyDeps(deps)
	ctx, buf := policyDepsLoggerCtx(t)

	if _, err := pd.StorageInfo(ctx, "nfs-a"); err != nil {
		t.Fatalf("StorageInfo: %v", err)
	}
	if strings.Contains(buf.String(), "share one physical backing") {
		t.Errorf("no duplicate backing configured: expected no warning, got: %s", buf.String())
	}
}
