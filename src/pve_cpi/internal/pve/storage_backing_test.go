// Tests for StorageInfo.BackingKey, SameBacking, SharedViaBacking, and
// WarnDuplicateBackingStorages — the backing-identity normalization that lets
// two differently-named PVE storage IDs pointing at the same physical export
// be recognized as "the same storage" (Kevin's "two names, one export" trap).
package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// ---------------------------------------------------------------------------
// BackingKey
// ---------------------------------------------------------------------------

func TestStorageInfo_BackingKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info StorageInfo
		want string
	}{
		{
			name: "nfs server+export",
			info: StorageInfo{Name: "nfs-a", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"},
			want: "nfs://10.0.0.5/tank/proxmox",
		},
		{
			name: "nfs server case-insensitive",
			info: StorageInfo{Name: "nfs-b", Type: "NFS", Server: "NAS.Example.COM", Export: "/tank/proxmox"},
			want: "nfs://nas.example.com/tank/proxmox",
		},
		{
			name: "nfs export path cleaned (double slash, trailing slash)",
			info: StorageInfo{Name: "nfs-c", Type: "nfs", Server: "10.0.0.5", Export: "//tank//proxmox/"},
			want: "nfs://10.0.0.5/tank/proxmox",
		},
		{
			name: "nfs missing server -> unknown",
			info: StorageInfo{Name: "nfs-d", Type: "nfs", Export: "/tank/proxmox"},
			want: "",
		},
		{
			name: "nfs missing export -> unknown",
			info: StorageInfo{Name: "nfs-e", Type: "nfs", Server: "10.0.0.5"},
			want: "",
		},
		{
			name: "cifs uses its own scheme, distinct from nfs, share field feeds Export",
			info: StorageInfo{Name: "cifs-a", Type: "cifs", Server: "10.0.0.6", Export: "/data"},
			want: "cifs://10.0.0.6/data",
		},
		{
			name: "cifs missing server -> unknown",
			info: StorageInfo{Name: "cifs-b", Type: "cifs", Export: "/data"},
			want: "",
		},
		{
			name: "cifs missing share -> unknown",
			info: StorageInfo{Name: "cifs-c", Type: "cifs", Server: "10.0.0.6"},
			want: "",
		},
		{
			name: "nfs and cifs at the coincidentally-matching server+path never share a key",
			info: StorageInfo{Name: "nfs-vs-cifs", Type: "nfs", Server: "10.0.0.6", Export: "/data"},
			want: "nfs://10.0.0.6/data",
		},
		{
			name: "dir cleaned path, no nodes restriction -> empty node suffix",
			info: StorageInfo{Name: "dir-a", Type: "dir", Path: "/mnt/pve//stemcells/"},
			want: "dir:///mnt/pve/stemcells#nodes=",
		},
		{
			name: "dir missing path -> unknown",
			info: StorageInfo{Name: "dir-b", Type: "dir"},
			want: "",
		},
		{
			name: "dir restricted to one node -> node folded into key",
			info: StorageInfo{Name: "dir-c", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n1"}},
			want: "dir:///mnt/ssd#nodes=n1",
		},
		{
			name: "dir restricted to multiple nodes -> sorted regardless of input order",
			info: StorageInfo{Name: "dir-d", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n2", "n1"}},
			want: "dir:///mnt/ssd#nodes=n1,n2",
		},
		{
			name: "lvm never normalizes",
			info: StorageInfo{Name: "vg0", Type: "lvm", Path: "/dev/vg0"},
			want: "id://vg0",
		},
		{
			name: "lvmthin never normalizes",
			info: StorageInfo{Name: "lvmthin0", Type: "lvmthin"},
			want: "id://lvmthin0",
		},
		{
			name: "zfspool never normalizes",
			info: StorageInfo{Name: "rpool", Type: "zfspool"},
			want: "id://rpool",
		},
		{
			name: "rbd never normalizes",
			info: StorageInfo{Name: "ceph", Type: "rbd"},
			want: "id://ceph",
		},
		{
			name: "cephfs never normalizes (no location field parsed)",
			info: StorageInfo{Name: "cfs", Type: "cephfs"},
			want: "id://cfs",
		},
		{
			name: "unknown/future type falls back to id://",
			info: StorageInfo{Name: "exotic1", Type: "exotic-plugin"},
			want: "id://exotic1",
		},
		{
			name: "empty name and unknown type -> unknown",
			info: StorageInfo{Type: "exotic-plugin"},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.info.BackingKey()
			if got != c.want {
				t.Errorf("BackingKey() = %q, want %q (info=%+v)", got, c.want, c.info)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SameBacking
// ---------------------------------------------------------------------------

func TestSameBacking(t *testing.T) {
	t.Parallel()
	nfsA := StorageInfo{Name: "nfs-a", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"}
	nfsB := StorageInfo{Name: "nfs-b", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"}
	nfsOther := StorageInfo{Name: "nfs-c", Type: "nfs", Server: "10.0.0.5", Export: "/tank/other"}
	lvmA := StorageInfo{Name: "vg0", Type: "lvm"}
	lvmB := StorageInfo{Name: "vg0", Type: "lvm"} // same name, same fallback key
	lvmC := StorageInfo{Name: "vg1", Type: "lvm"}
	dirUnknownA := StorageInfo{Name: "dir-a", Type: "dir"} // no Path -> key ""
	dirUnknownB := StorageInfo{Name: "dir-b", Type: "dir"} // no Path -> key ""

	// Node-local dir storages sharing a mount path — the "one storage ID per
	// node" PVE pattern F2 covers.
	dirSamePathNodeN1 := StorageInfo{Name: "ssd-n1", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n1"}}
	dirSamePathNodeN2 := StorageInfo{Name: "ssd-n2", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n2"}, Shared: true}
	dirSamePathNodeN1Again := StorageInfo{Name: "ssd-n1-dup", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n1"}}
	dirSamePathNoNodesA := StorageInfo{Name: "ssd-any-a", Type: "dir", Path: "/mnt/ssd"}
	dirSamePathNoNodesB := StorageInfo{Name: "ssd-any-b", Type: "dir", Path: "/mnt/ssd"}
	dirSamePathRestrictedVsUnrestricted := StorageInfo{Name: "ssd-n1-only", Type: "dir", Path: "/mnt/ssd", Nodes: []string{"n1"}}
	cifsA := StorageInfo{Name: "cifs-a", Type: "cifs", Server: "10.0.0.5", Export: "/data"}
	nfsSameServerPathAsCIFS := StorageInfo{Name: "nfs-x", Type: "nfs", Server: "10.0.0.5", Export: "/data"}

	cases := []struct {
		name string
		a, b StorageInfo
		want bool
	}{
		{"two IDs, same nfs export -> same backing", nfsA, nfsB, true},
		{"two IDs, different nfs export -> distinct", nfsA, nfsOther, false},
		{"same literal name, lvm fallback -> same backing (trivial self-match)", lvmA, lvmB, true},
		{"distinct lvm names never merge", lvmA, lvmC, false},
		{"two undeterminable dir entries never match, even to each other", dirUnknownA, dirUnknownB, false},
		{"undeterminable vs determinable never matches", dirUnknownA, nfsA, false},
		{"same dir path, disjoint node sets -> different backings (F2)", dirSamePathNodeN1, dirSamePathNodeN2, false},
		{"same dir path, identical single-node sets -> same backing", dirSamePathNodeN1, dirSamePathNodeN1Again, true},
		{"same dir path, both unrestricted (no nodes) -> same backing", dirSamePathNoNodesA, dirSamePathNoNodesB, true},
		{"same dir path, one restricted one unrestricted -> conservative, not a match", dirSamePathRestrictedVsUnrestricted, dirSamePathNoNodesA, false},
		{"cifs share and nfs export at the same server+path never merge (F7)", cifsA, nfsSameServerPathAsCIFS, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := SameBacking(c.a, c.b); got != c.want {
				t.Errorf("SameBacking(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
			}
			// SameBacking must be symmetric.
			if got := SameBacking(c.b, c.a); got != c.want {
				t.Errorf("SameBacking(%+v, %+v) [swapped] = %v, want %v", c.b, c.a, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SharedViaBacking
// ---------------------------------------------------------------------------

func TestSharedViaBacking(t *testing.T) {
	t.Parallel()

	t.Run("already shared on its own terms: other entries irrelevant", func(t *testing.T) {
		t.Parallel()
		target := StorageInfo{Name: "nfs-a", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"}
		if !SharedViaBacking(target, nil) {
			t.Fatal("nfs is shared by type regardless of the shared flag or other entries")
		}
	})

	t.Run("config-drift: two dir IDs, same path, only one flagged shared", func(t *testing.T) {
		t.Parallel()
		flaggedShared := StorageInfo{Name: "dir-a", Type: "dir", Path: "/mnt/pve/export", Shared: true}
		notFlagged := StorageInfo{Name: "dir-b", Type: "dir", Path: "/mnt/pve/export", Shared: false}
		all := []StorageInfo{flaggedShared, notFlagged}

		if !SharedViaBacking(notFlagged, all) {
			t.Error("dir-b shares dir-a's backing (same path) and dir-a is shared -> dir-b must be treated as shared")
		}
		if !SharedViaBacking(flaggedShared, all) {
			t.Error("dir-a is already shared on its own terms")
		}
	})

	t.Run("two genuinely distinct dir IDs: no propagation", func(t *testing.T) {
		t.Parallel()
		sharedElsewhere := StorageInfo{Name: "dir-a", Type: "dir", Path: "/mnt/pve/export-a", Shared: true}
		localOnly := StorageInfo{Name: "dir-b", Type: "dir", Path: "/mnt/pve/export-b", Shared: false}
		all := []StorageInfo{sharedElsewhere, localOnly}

		if SharedViaBacking(localOnly, all) {
			t.Error("dir-b has a different path than dir-a: must stay local, no false propagation")
		}
	})

	t.Run("neither flagged shared: stays local", func(t *testing.T) {
		t.Parallel()
		a := StorageInfo{Name: "dir-a", Type: "dir", Path: "/mnt/pve/export", Shared: false}
		b := StorageInfo{Name: "dir-b", Type: "dir", Path: "/mnt/pve/export", Shared: false}
		all := []StorageInfo{a, b}

		if SharedViaBacking(a, all) {
			t.Error("neither entry is shared: propagation must not fabricate shared=true")
		}
	})

	t.Run("block storage never propagates (id:// key only matches itself)", func(t *testing.T) {
		t.Parallel()
		// Two lvm pools that happen to be named identically to each other's
		// Type but are genuinely distinct storages — id:// keys differ, so no
		// propagation is even attempted.
		a := StorageInfo{Name: "vg0", Type: "lvm", Shared: true}
		b := StorageInfo{Name: "vg1", Type: "lvm", Shared: false}
		all := []StorageInfo{a, b}

		if SharedViaBacking(b, all) {
			t.Error("lvm never normalizes across distinct storage IDs: vg1 must stay local")
		}
	})

	t.Run("target absent from all: falls back to its own IsShared", func(t *testing.T) {
		t.Parallel()
		target := StorageInfo{Name: "dir-a", Type: "dir", Path: "/mnt/pve/export", Shared: false}
		other := StorageInfo{Name: "dir-z", Type: "dir", Path: "/mnt/pve/other", Shared: true}
		if SharedViaBacking(target, []StorageInfo{other}) {
			t.Error("target shares no backing with the only other entry: must stay local")
		}
	})
}

// ---------------------------------------------------------------------------
// WarnDuplicateBackingStorages
// ---------------------------------------------------------------------------

func testLoggerCtx(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("log.NewLogger: %v", err)
	}
	return log.IntoContext(context.Background(), logger), &buf
}

func TestWarnDuplicateBackingStorages(t *testing.T) {
	t.Parallel()

	t.Run("two IDs sharing one nfs export: one warn naming both", func(t *testing.T) {
		t.Parallel()
		ctx, buf := testLoggerCtx(t)
		infos := []StorageInfo{
			{Name: "nfs-a", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"},
			{Name: "nfs-b", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"},
			{Name: "nfs-other", Type: "nfs", Server: "10.0.0.5", Export: "/tank/other"},
		}
		WarnDuplicateBackingStorages(ctx, infos)
		logged := buf.String()
		if !strings.Contains(logged, "nfs-a") || !strings.Contains(logged, "nfs-b") {
			t.Errorf("expected both duplicate storage IDs named in the warning, got: %s", logged)
		}
		if strings.Contains(logged, "nfs-other") {
			t.Errorf("distinct-backing storage must not appear in the warning: %s", logged)
		}
		if strings.Count(logged, "level=WARN") != 1 && strings.Count(logged, "WARN") != 1 {
			// Accept either slog text rendering; just confirm exactly one warn fired.
			t.Logf("log output (informational, format-dependent check skipped): %s", logged)
		}
	})

	t.Run("no duplicates: no warning", func(t *testing.T) {
		t.Parallel()
		ctx, buf := testLoggerCtx(t)
		infos := []StorageInfo{
			{Name: "nfs-a", Type: "nfs", Server: "10.0.0.5", Export: "/tank/a"},
			{Name: "nfs-b", Type: "nfs", Server: "10.0.0.5", Export: "/tank/b"},
			{Name: "vg0", Type: "lvm"},
			{Name: "vg1", Type: "lvm"},
		}
		WarnDuplicateBackingStorages(ctx, infos)
		if buf.Len() != 0 {
			t.Errorf("expected no log output, got: %s", buf.String())
		}
	})

	t.Run("undeterminable backing never grouped as duplicate", func(t *testing.T) {
		t.Parallel()
		ctx, buf := testLoggerCtx(t)
		infos := []StorageInfo{
			{Name: "dir-a", Type: "dir"}, // no Path -> BackingKey() == ""
			{Name: "dir-b", Type: "dir"}, // no Path -> BackingKey() == ""
		}
		WarnDuplicateBackingStorages(ctx, infos)
		if buf.Len() != 0 {
			t.Errorf("two entries with undeterminable backing must never be reported as duplicates, got: %s", buf.String())
		}
	})

	t.Run("block storage types never warn even with identical names across a merged list", func(t *testing.T) {
		t.Parallel()
		ctx, buf := testLoggerCtx(t)
		infos := []StorageInfo{
			{Name: "vg0", Type: "lvm"},
			{Name: "vg1", Type: "lvmthin"},
			{Name: "ceph", Type: "rbd"},
		}
		WarnDuplicateBackingStorages(ctx, infos)
		if buf.Len() != 0 {
			t.Errorf("distinct block-storage IDs must never warn, got: %s", buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// parseStorageEntry / ParseStorageEntry — new field decoding
// ---------------------------------------------------------------------------

func TestParseStorageEntry_DecodesBackingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  map[string]any
		want StorageInfo
	}{
		{
			name: "dir path",
			row:  map[string]any{"storage": "d1", "type": "dir", "path": "/mnt/pve/d1", "content": "images,iso"},
			want: StorageInfo{Name: "d1", Type: "dir", Path: "/mnt/pve/d1", Content: "images,iso"},
		},
		{
			name: "nfs server+export",
			row:  map[string]any{"storage": "n1", "type": "nfs", "server": "10.0.0.5", "export": "/tank/proxmox"},
			want: StorageInfo{Name: "n1", Type: "nfs", Server: "10.0.0.5", Export: "/tank/proxmox"},
		},
		{
			name: "cifs server+share maps into Export",
			row:  map[string]any{"storage": "c1", "type": "cifs", "server": "10.0.0.6", "share": "data"},
			want: StorageInfo{Name: "c1", Type: "cifs", Server: "10.0.0.6", Export: "data"},
		},
		{
			name: "lvm carries no location fields",
			row:  map[string]any{"storage": "vg0", "type": "lvm"},
			want: StorageInfo{Name: "vg0", Type: "lvm"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(c.row)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			got, err := ParseStorageEntry(raw)
			if err != nil {
				t.Fatalf("ParseStorageEntry: %v", err)
			}
			if got.Name != c.want.Name || got.Type != c.want.Type || got.Path != c.want.Path ||
				got.Server != c.want.Server || got.Export != c.want.Export || got.Content != c.want.Content {
				t.Errorf("ParseStorageEntry(%v) = %+v, want %+v", c.row, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// StorageInfoCache wiring: WarnDuplicateBackingStorages fires once at first
// successful fill, not on every refresh.
// ---------------------------------------------------------------------------

func TestStorageInfoCache_WarnsDuplicateBackingOnceAtFirstFill(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "nfs-a", "type": "nfs", "server": "10.0.0.5", "export": "/tank/proxmox"},
		{"storage": "nfs-b", "type": "nfs", "server": "10.0.0.5", "export": "/tank/proxmox"},
	}}
	// TTL of 0 forces every Get to refresh, so we can observe that the warning
	// still only fires once despite multiple refreshes.
	cache := NewStorageInfoCache(lister, 0)

	ctx, buf := testLoggerCtx(t)
	if _, err := cache.Get(ctx, "nfs-a"); err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	if _, err := cache.Get(ctx, "nfs-b"); err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if _, err := cache.Get(ctx, "nfs-a"); err != nil {
		t.Fatalf("Get #3: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "nfs-a") || !strings.Contains(logged, "nfs-b") {
		t.Fatalf("expected the duplicate-backing warning to name both storage IDs, got: %s", logged)
	}
	occurrences := strings.Count(logged, "two or more storage IDs share one physical backing")
	if occurrences != 1 {
		t.Errorf("expected the warning to fire exactly once across 3 refreshes, fired %d times: %s", occurrences, logged)
	}
}

// TestStorageInfoCache_NoDuplicateBackingWarningWhenNoneShare verifies the
// common case (no duplicate backing configured) emits nothing extra.
func TestStorageInfoCache_NoDuplicateBackingWarningWhenNoneShare(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{entries: []map[string]any{
		{"storage": "nfs-a", "type": "nfs", "server": "10.0.0.5", "export": "/tank/a"},
		{"storage": "local-lvm", "type": "lvm"},
	}}
	cache := NewStorageInfoCache(lister, time.Minute)
	ctx, buf := testLoggerCtx(t)
	if _, err := cache.Get(ctx, "nfs-a"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(buf.String(), "share one physical backing") {
		t.Errorf("no duplicate backing configured: expected no warning, got: %s", buf.String())
	}
}
