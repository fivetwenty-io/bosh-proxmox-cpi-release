package allocationjournal

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrivateOpenRejectsSymlinksInAllModes(t *testing.T) {
	modes := []int{os.O_RDONLY, os.O_RDWR, os.O_WRONLY, os.O_RDWR | os.O_CREATE, os.O_WRONLY | os.O_CREATE | os.O_EXCL, os.O_WRONLY | os.O_TRUNC, os.O_WRONLY | os.O_APPEND}
	for _, mode := range modes {
		j, _ := fixture(t)
		if err := j.root.WriteFile("target", []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := j.root.Symlink("target", "link"); err != nil {
			t.Fatal(err)
		}
		f, err := openPrivate(j.root, "link", mode)
		if err == nil {
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			t.Fatalf("symlink accepted in mode %d", mode)
		}
		got, err := j.root.ReadFile("target")
		if err != nil || string(got) != "preserve" {
			t.Fatal("symlink target changed")
		}
	}
}
func TestPrivateOpenRejectsReplacementBeforeTruncation(t *testing.T) {
	for _, replacementSymlink := range []bool{false, true} {
		j, _ := fixture(t)
		if err := j.root.WriteFile("old", []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := j.root.WriteFile("replacement", []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := openPrivateWith(j.root, "old", os.O_WRONLY|os.O_TRUNC, func(fd int, name string, flags int) (int, error) {
			if replacementSymlink {
				if err := j.root.Remove("old"); err != nil {
					return -1, err
				}
				if err := j.root.Symlink("replacement", "old"); err != nil {
					return -1, err
				}
			} else {
				if err := j.root.Rename("replacement", "old"); err != nil {
					return -1, err
				}
			}
			return unix.Openat(fd, name, flags, 0600)
		})
		if err == nil {
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			t.Fatal("concurrent replacement accepted")
		}
		target := "old"
		if replacementSymlink {
			target = "replacement"
		}
		data, err := j.root.ReadFile(target)
		if err != nil || string(data) != "preserve" {
			t.Fatal("replacement inode was truncated")
		}
	}
}
func TestPrivateReadJSONRejectsSymlinkRecord(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	r := h.Record()
	closeHandle(t, h)
	if err := j.root.Rename(recordName(r.ID), "original-record"); err != nil {
		t.Fatal(err)
	}
	if err := j.root.Symlink("original-record", recordName(r.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Inspect(r.ID); err == nil {
		t.Fatal("record symlink accepted")
	}
	if _, _, err := tryLock(j.root, recordName(r.ID)); err == nil {
		t.Fatal("lock symlink accepted")
	}
	if f, err := openPrivate(j.root, "../outside", os.O_CREATE|os.O_RDWR); err == nil {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("basename restriction bypassed")
	}
	if _, err := openPrivate(j.root, "absent", os.O_RDONLY); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absence semantics: %v", err)
	}
}
