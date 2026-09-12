package allocationjournal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func physicalTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestProvisionFreshRepeatedAndNoAuthority(t *testing.T) {
	base := physicalTemp(t)
	path := filepath.Join(base, "durable", "allocations")
	for i := 0; i < 2; i++ {
		if err := ProvisionDirectory(path, os.Geteuid(), os.Getegid()); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != 0700 || st.Uid != uint32(os.Geteuid()) || st.Gid != uint32(os.Getegid()) {
			t.Fatal("incorrect mode or ownership")
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("provisioning initialized journal authority")
	}
}
func TestProvisionRejectsUnsafePathsWithoutChangingExistingMode(t *testing.T) {
	base := physicalTemp(t)
	broad := filepath.Join(base, "broad")
	if err := os.Mkdir(broad, 0755); err != nil {
		t.Fatal(err)
	}
	readonly := filepath.Join(base, "readonly")
	if err := os.Mkdir(readonly, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(readonly, 0700); err != nil {
			t.Error(err)
		}
	}()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(broad, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "", "relative", base + "/../other", broad, readonly, file, link, filepath.Join(link, "child")} {
		if err := ProvisionDirectory(path, os.Geteuid(), os.Getegid()); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
	for p, mode := range map[string]os.FileMode{broad: 0755, readonly: 0500} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatal("existing permissions changed")
		}
	}
	if _, err := os.Stat(filepath.Join(broad, "child")); !os.IsNotExist(err) {
		t.Fatal("followed parent symlink")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "preserve" {
		t.Fatal("existing file changed")
	}
}
func TestProvisionRejectsUntrustedParent(t *testing.T) {
	base := physicalTemp(t)
	parent := filepath.Join(base, "writable")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionDirectory(filepath.Join(parent, "journal"), os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("untrusted parent accepted")
	}
	if _, err := os.Stat(filepath.Join(parent, "journal")); !os.IsNotExist(err) {
		t.Fatal("created under unsafe parent")
	}
}
func TestProvisionWrongOwnerAndConcurrentCalls(t *testing.T) {
	base := physicalTemp(t)
	path := filepath.Join(base, "journal")
	if err := ProvisionDirectory(path, -1, os.Getegid()); err == nil {
		t.Fatal("invalid UID accepted")
	}
	if os.Geteuid() != 0 {
		if err := ProvisionDirectory(path, 0, 0); err == nil {
			t.Fatal("unprivileged owner override accepted")
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- ProvisionDirectory(path, os.Geteuid(), os.Getegid()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProvisionComponent(info, os.Geteuid()+1, os.Getegid(), true); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("mismatched owner accepted")
	}
}
