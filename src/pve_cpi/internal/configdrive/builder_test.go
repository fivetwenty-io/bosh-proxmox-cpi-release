package configdrive_test

import (
	"io"
	"os"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/configdrive"
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

	// Files that mirror the settings.json payload.
	for _, name := range []string{
		"/openstack/latest/user_data",
		"/ec2/latest/user-data",
		"/ec2/latest/meta-data.json",
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

	// /openstack/latest/meta_data.json carries a minimal stub (not the payload).
	mf, err := iso.OpenFile("/openstack/latest/meta_data.json", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile /openstack/latest/meta_data.json: %v", err)
	}
	mb, err := io.ReadAll(mf)
	if err != nil {
		t.Fatalf("read meta_data.json: %v", err)
	}
	if len(mb) == 0 {
		t.Error("/openstack/latest/meta_data.json is empty; want minimal JSON stub")
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
