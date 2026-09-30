package allocationjournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubFileInfo lets a test present ownership the test process cannot create.
// CI runs as root, so a real file owned by another user is not portable.
type stubFileInfo struct {
	mode os.FileMode
	sys  any
}

func (s stubFileInfo) Name() string       { return "stub" }
func (s stubFileInfo) Size() int64        { return 0 }
func (s stubFileInfo) Mode() os.FileMode  { return s.mode }
func (s stubFileInfo) ModTime() time.Time { return time.Time{} }
func (s stubFileInfo) IsDir() bool        { return s.mode.IsDir() }
func (s stubFileInfo) Sys() any           { return s.sys }

func TestPrivateInfoNamesEachUnsafeCause(t *testing.T) {
	euid := os.Geteuid()
	other := uint32(euid + 1000) // #nosec G115 -- test UIDs are small and positive
	own := uint32(euid)          // #nosec G115 -- the effective UID is never negative
	for _, tc := range []struct {
		name      string
		info      os.FileInfo
		directory bool
		cause     error
		uid       int
		text      string
	}{
		{"owned by another user", stubFileInfo{mode: os.ModeDir | 0o700, sys: &syscall.Stat_t{Uid: other, Nlink: 1}}, true, ErrUnsafeOwnership, int(other), "is owned by uid"},
		{"no ownership metadata", stubFileInfo{mode: os.ModeDir | 0o700, sys: nil}, true, ErrUnsafeOwnership, -1, "has no ownership metadata"},
		{"group readable directory", stubFileInfo{mode: os.ModeDir | 0o750, sys: &syscall.Stat_t{Uid: own, Nlink: 1}}, true, ErrUnsafeMode, euid, "has mode 0750"},
		{"file where a directory belongs", stubFileInfo{mode: 0o600, sys: &syscall.Stat_t{Uid: own, Nlink: 1}}, true, ErrUnsafeFileType, euid, "is not a directory"},
		{"directory where a file belongs", stubFileInfo{mode: os.ModeDir | 0o700, sys: &syscall.Stat_t{Uid: own, Nlink: 1}}, false, ErrUnsafeFileType, euid, "is not a regular file"},
		{"socket where a file belongs", stubFileInfo{mode: os.ModeSocket | 0o600, sys: &syscall.Stat_t{Uid: own, Nlink: 1}}, false, ErrUnsafeFileType, euid, "is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := privateInfo("/journal/path", tc.info, tc.directory)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("error %v does not wrap %v", err, tc.cause)
			}
			var unsafe *UnsafePathError
			if !errors.As(err, &unsafe) {
				t.Fatalf("error %T is not an UnsafePathError", err)
			}
			if unsafe.Path != "/journal/path" || unsafe.UID != tc.uid || unsafe.EUID != euid {
				t.Fatalf("unsafe path fields = %+v", unsafe)
			}
			if !strings.Contains(err.Error(), "/journal/path") || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("error text %q does not name the path and %q", err.Error(), tc.text)
			}
		})
	}
	if err := privateInfo("/journal/path", stubFileInfo{mode: os.ModeDir | 0o700, sys: &syscall.Stat_t{Uid: own, Nlink: 1}}, true); err != nil {
		t.Fatalf("a private directory this process owns was refused: %v", err)
	}
}

func TestInspectEnrollmentReportsTheUnsafePath(t *testing.T) {
	_, dir := fixture(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := InspectEnrollment(dir, "namespace")
	var unsafe *UnsafePathError
	if !errors.Is(err, ErrUnsafeMode) || !errors.As(err, &unsafe) || unsafe.Path != dir {
		t.Fatalf("widened journal directory reported as %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	namespaceDir := filepath.Join(dir, pathKey("namespace"))
	if err := os.Chmod(namespaceDir, 0o711); err != nil {
		t.Fatal(err)
	}
	_, err = InspectEnrollment(dir, "namespace")
	if !errors.Is(err, ErrUnsafeMode) || !errors.As(err, &unsafe) || unsafe.Path != namespaceDir {
		t.Fatalf("widened namespace directory reported as %v", err)
	}
	if err := os.Chmod(namespaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authority := filepath.Join(namespaceDir, authorityFile)
	if err := os.Chmod(authority, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = InspectEnrollment(dir, "namespace")
	if !errors.Is(err, ErrUnsafeMode) || !errors.As(err, &unsafe) || unsafe.Path != authority {
		t.Fatalf("widened authority file reported as %v", err)
	}
}
