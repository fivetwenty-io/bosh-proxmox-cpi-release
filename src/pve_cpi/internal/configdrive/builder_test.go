package configdrive_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
)

func TestBuild_RoundTrip(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"agent_id":"abc","vm":{"name":"vm-1","id":"1"}}`)
	path, cleanup, err := configdrive.Build(payload)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer cleanup()

	d, err := diskfs.Open(path)
	if err != nil {
		t.Fatalf("diskfs.Open: %v", err)
	}
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatalf("GetFilesystem: %v", err)
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		t.Fatalf("not iso9660: %T", fs)
	}

	label := strings.ToLower(strings.TrimSpace(strings.Trim(iso.Label(), "\x00")))
	if label != "config-2" {
		t.Errorf("volume label = %q, want config-2", label)
	}

	// Files that carry the raw settings.json payload.
	for _, name := range []string{
		"/openstack/latest/user_data",
		"/ec2/latest/user-data",
	} {
		f, err := iso.OpenFile(name, os.O_RDONLY)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", name, err)
		}
		got, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(payload) {
			t.Errorf("%s content mismatch:\n got=%q\nwant=%q", name, got, payload)
		}
	}

	// Both meta_data files must carry the minimal stub, not the payload.
	// /openstack/latest/meta_data.json — OpenStack datasource stub.
	// /ec2/latest/meta-data.json       — EC2 datasource stub (fixed: was payload).
	for _, name := range []string{
		"/openstack/latest/meta_data.json",
		"/ec2/latest/meta-data.json",
	} {
		mf, err := iso.OpenFile(name, os.O_RDONLY)
		if err != nil {
			t.Fatalf("OpenFile %s: %v", name, err)
		}
		mb, err := io.ReadAll(mf)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(mb) == 0 {
			t.Errorf("%s is empty; want minimal JSON stub", name)
		}
		if string(mb) == string(payload) {
			t.Errorf("%s contains full settings payload; want metadata stub only", name)
		}
	}
}

func TestBuild_CleanupRemovesFile(t *testing.T) {
	t.Parallel()
	path, cleanup, err := configdrive.Build([]byte("x"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected iso to exist before cleanup, stat err: %v", statErr)
	}
	cleanup()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected iso removed after cleanup, stat err: %v", statErr)
	}
}

// TestBuild_UsesTempDir verifies that Build places the ISO inside a
// process-owned temp directory (not directly in /tmp with a predictable name),
// and that the temp directory is fully removed by the cleanup function.
func TestBuild_UsesTempDir(t *testing.T) {
	t.Parallel()

	path, cleanup, err := configdrive.Build([]byte(`{"agent_id":"test"}`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The ISO must be named "configdrive.iso" and live inside a temp directory,
	// not directly in /tmp as a bare *.iso file.
	base := filepath.Base(path)
	if base != "configdrive.iso" {
		t.Errorf("ISO filename = %q, want %q", base, "configdrive.iso")
	}

	dir := filepath.Dir(path)
	// The parent directory must not be /tmp itself — it must be a unique subdir.
	tmpRoot := os.TempDir()
	if dir == tmpRoot {
		t.Errorf("ISO placed directly in %s; expected a unique subdirectory", tmpRoot)
	}

	// The temp directory must exist before cleanup.
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("expected temp dir to exist before cleanup, stat err: %v", statErr)
	}

	cleanup()

	// Both the ISO and its parent temp directory must be gone after cleanup.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("ISO still exists after cleanup: stat err: %v", statErr)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("temp dir still exists after cleanup: stat err: %v", statErr)
	}
}
