package pve

import (
	"strings"
	"testing"
	"time"
)

const adviceMarker = "every VMID band is at its default"

// adviceRefresh builds a cache over entries with the advice option for bands,
// drives one refresh through Get, and returns what it logged.
func adviceRefresh(t *testing.T, bands VMIDBands, entries []map[string]any) string {
	t.Helper()
	ctx, buf := testLoggerCtx(t)
	cache := NewStorageInfoCache(&fakeLister{entries: entries}, time.Minute, WithSharedExportAdvice(bands))
	_, _ = cache.Get(ctx, "anything")
	return buf.String()
}

func TestSharedExportAdvice(t *testing.T) {
	nfs := map[string]any{"storage": "nfs-shared", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/exports/pve", "content": "images,iso"}
	nfsBackups := map[string]any{"storage": "nfs-backup", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/exports/bk", "content": "backup"}
	nfsDisabled := map[string]any{"storage": "nfs-off", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/exports/off", "content": "images", "disable": 1}
	cifsISO := map[string]any{"storage": "cifs-iso", "type": "cifs", "shared": 1, "server": "fs1", "share": "iso", "content": "iso"}
	sharedDir := map[string]any{"storage": "dir-shared", "type": "dir", "shared": 1, "path": "/mnt/pve/shared", "content": "images"}
	localDir := map[string]any{"storage": "local", "type": "dir", "path": "/var/lib/vz", "content": "iso,images"}
	lvm := map[string]any{"storage": "local-lvm", "type": "lvmthin", "content": "images"}
	moved := DefaultVMIDBands()
	moved.Disk = VMIDBand{Start: 40000, End: 49999}

	tests := []struct {
		name        string
		bands       VMIDBands
		entries     []map[string]any
		wantAdvice  bool
		wantStorage string
	}{
		{name: "default bands on nfs", bands: DefaultVMIDBands(), entries: []map[string]any{lvm, nfs}, wantAdvice: true, wantStorage: "nfs-shared"},
		{name: "default bands on a cifs ISO share", bands: DefaultVMIDBands(), entries: []map[string]any{cifsISO}, wantAdvice: true, wantStorage: "cifs-iso"},
		{name: "default bands on a shared dir", bands: DefaultVMIDBands(), entries: []map[string]any{sharedDir}, wantAdvice: true, wantStorage: "dir-shared"},
		{name: "one band set explicitly", bands: moved, entries: []map[string]any{nfs}},
		{name: "no network file storage", bands: DefaultVMIDBands(), entries: []map[string]any{lvm, localDir}},
		{name: "nfs with backups only", bands: DefaultVMIDBands(), entries: []map[string]any{nfsBackups}},
		{name: "disabled nfs", bands: DefaultVMIDBands(), entries: []map[string]any{nfsDisabled}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer ResetSharedExportAdviceOnceForTest()()
			defer ResetDuplicateBackingWarnOnceForTest()()
			out := adviceRefresh(t, tc.bands, tc.entries)
			if got := strings.Contains(out, adviceMarker); got != tc.wantAdvice {
				t.Fatalf("advice logged = %v, want %v:\n%s", got, tc.wantAdvice, out)
			}
			if !tc.wantAdvice {
				return
			}
			for _, want := range []string{"pve-cid ha --cpi-config", "next-id", `"storage":"` + tc.wantStorage + `"`, `"level":"INFO"`} {
				if !strings.Contains(out, want) {
					t.Errorf("advice lacks %s:\n%s", want, out)
				}
			}
		})
	}
}

func TestSharedExportAdviceOncePerProcess(t *testing.T) {
	defer ResetSharedExportAdviceOnceForTest()()
	defer ResetDuplicateBackingWarnOnceForTest()()
	nfs := []map[string]any{{"storage": "nfs-shared", "type": "nfs", "shared": 1, "server": "s", "export": "/e", "content": "images"}}
	first := adviceRefresh(t, DefaultVMIDBands(), nfs)
	second := adviceRefresh(t, DefaultVMIDBands(), nfs)
	if !strings.Contains(first, adviceMarker) {
		t.Fatalf("first refresh gave no advice:\n%s", first)
	}
	if strings.Contains(second, adviceMarker) {
		t.Errorf("advice logged again on a second cache's refresh:\n%s", second)
	}
}

func TestSharedExportAdviceStaysArmedUntilItFires(t *testing.T) {
	defer ResetSharedExportAdviceOnceForTest()()
	defer ResetDuplicateBackingWarnOnceForTest()()
	local := []map[string]any{{"storage": "local-lvm", "type": "lvmthin", "content": "images"}}
	nfs := []map[string]any{{"storage": "nfs-shared", "type": "nfs", "shared": 1, "server": "s", "export": "/e", "content": "images"}}
	if out := adviceRefresh(t, DefaultVMIDBands(), local); strings.Contains(out, adviceMarker) {
		t.Fatalf("advice logged for a cluster with no network file storage:\n%s", out)
	}
	if out := adviceRefresh(t, DefaultVMIDBands(), nfs); !strings.Contains(out, adviceMarker) {
		t.Errorf("a refresh that found nothing used up the gate:\n%s", out)
	}
}

func TestStorageInfoCacheWithoutAdviceLogsNone(t *testing.T) {
	defer ResetSharedExportAdviceOnceForTest()()
	defer ResetDuplicateBackingWarnOnceForTest()()
	ctx, buf := testLoggerCtx(t)
	entries := []map[string]any{{"storage": "nfs-shared", "type": "nfs", "shared": 1, "server": "s", "export": "/e", "content": "images"}}
	cache := NewStorageInfoCache(&fakeLister{entries: entries}, time.Minute)
	_, _ = cache.Get(ctx, "nfs-shared")
	if strings.Contains(buf.String(), adviceMarker) {
		t.Errorf("a cache built without the option gave advice:\n%s", buf.String())
	}
}
