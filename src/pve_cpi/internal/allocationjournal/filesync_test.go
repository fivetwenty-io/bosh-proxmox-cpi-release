package allocationjournal

import (
	"os"
	"testing"
)

// TestDefaultOpsSyncFileAndDirectoryOnEveryWrite pins the durability contract
// of the default ops: a journal write syncs the file and then its directory,
// unless a test has turned syncing off with SetFileSyncForTest. It is the only
// test in this package that touches that switch, and it runs sequentially and
// restores both the switch and the hooks, so every other durability test here
// runs with real syncs.
func TestDefaultOpsSyncFileAndDirectoryOnEveryWrite(t *testing.T) {
	if fileSyncOff.Load() {
		t.Fatal("file sync is off by default; the journal must sync unless a test turns it off")
	}
	var fileSyncs, dirSyncs int
	origFile, origDir := syncFileHook, syncDirHook
	syncFileHook = func(f *os.File) error { fileSyncs++; return origFile(f) }
	syncDirHook = func(r *os.Root) error { dirSyncs++; return origDir(r) }
	t.Cleanup(func() { syncFileHook, syncDirHook = origFile, origDir })

	j, _ := fixture(t)
	if fileSyncs == 0 || dirSyncs == 0 {
		t.Fatalf("Initialize synced %d files and %d directories; want both above zero", fileSyncs, dirSyncs)
	}

	write := func(name string) {
		t.Helper()
		fileSyncs, dirSyncs = 0, 0
		if err := atomicJSON(j.root, name, map[string]string{"probe": name}, j.ops); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := readJSON(j.root, name, &got); err != nil {
			t.Fatal(err)
		}
		if got["probe"] != name {
			t.Fatalf("%s holds %v after the write", name, got)
		}
	}

	write("default.json")
	if fileSyncs != 1 || dirSyncs != 1 {
		t.Fatalf("a default write synced %d files and %d directories; want 1 and 1", fileSyncs, dirSyncs)
	}

	restore := SetFileSyncForTest(false)
	t.Cleanup(restore) // a failed write must not leave the switch off; restore is idempotent
	write("off.json")
	restore()
	if fileSyncs != 0 || dirSyncs != 0 {
		t.Fatalf("a write with sync off synced %d files and %d directories; want 0 and 0", fileSyncs, dirSyncs)
	}

	write("restored.json")
	if fileSyncs != 1 || dirSyncs != 1 {
		t.Fatalf("a write after restore synced %d files and %d directories; want 1 and 1", fileSyncs, dirSyncs)
	}
}
