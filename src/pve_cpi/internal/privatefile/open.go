package privatefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// UnsafePathError names a path that PrivateInfo refused. UID is the path's
// owner, or -1 when the platform reported no ownership metadata, and EUID is
// the effective UID the check compared it against. WantDirectory says which
// file type the check expected. Its text carries only the path, the two UIDs,
// and the mode, so a CLI may print it.
type UnsafePathError struct {
	Path          string
	UID, EUID     int
	Mode          os.FileMode
	WantDirectory bool
	Cause         error
}

func (e *UnsafePathError) Error() string {
	switch {
	case errors.Is(e.Cause, ErrUnsafeOwnership) && e.UID < 0:
		return fmt.Sprintf("%v: %s has no ownership metadata", e.Cause, e.Path)
	case errors.Is(e.Cause, ErrUnsafeOwnership):
		return fmt.Sprintf("%v: %s is owned by uid %d, not effective uid %d", e.Cause, e.Path, e.UID, e.EUID)
	case errors.Is(e.Cause, ErrUnsafeMode):
		return fmt.Sprintf("%v: %s has mode %04o and must grant no group or other access", e.Cause, e.Path, e.Mode.Perm())
	case e.WantDirectory:
		return fmt.Sprintf("%v: %s is not a directory", e.Cause, e.Path)
	default:
		return fmt.Sprintf("%v: %s is not a regular file", e.Cause, e.Path)
	}
}

func (e *UnsafePathError) Unwrap() error { return e.Cause }

// PrivateInfo refuses a path that another user owns, whose type is not the
// one expected, or that grants any group or other access, in that order of
// precedence. path is only reported; every check reads info.
func (p Policy) PrivateInfo(path string, info os.FileInfo, directory bool) error {
	euid := os.Geteuid()
	st, ok := info.Sys().(*syscall.Stat_t)
	unsafe := &UnsafePathError{Path: path, UID: -1, EUID: euid, Mode: info.Mode(), WantDirectory: directory}
	if ok {
		unsafe.UID = int(st.Uid)
	}
	switch {
	case !ok || int64(st.Uid) != int64(euid):
		unsafe.Cause = p.ownership()
	case info.IsDir() != directory || !directory && !info.Mode().IsRegular():
		unsafe.Cause = p.fileType()
	case info.Mode().Perm()&0o077 != 0:
		unsafe.Cause = p.mode()
	}
	if unsafe.Cause != nil {
		return unsafe
	}
	// Reject a second link, not a missing one. A link count of zero is what a
	// concurrent atomic rename looks like from the reader's side: the
	// victim inode is unlinked while the name still resolves to it, so an
	// lstat or an fstat a moment later sees a live name on a zero-link inode.
	// Measured on APFS that is about one lookup in five hundred under load,
	// and "!= 1" turned every one of them into a hard failure no caller
	// recognised. An inode nothing links to cannot be reached by name by any
	// new opener, so accepting it gives an attacker nothing, while nlink >= 2
	// stays refused and that is what the message has always claimed to catch.
	// Reading a doomed inode is handled where it belongs, by the SameFile
	// check in OpenWith and the retry in the envelope read.
	if !directory && st.Nlink > 1 {
		return fmt.Errorf("%s: multiply linked file rejected", p.label())
	}
	return nil
}

// OpenRoot opens the absolute directory path as a root after PrivateInfo
// accepts it and its opened inode matches the one checked.
func (p Policy) OpenRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s: directory must be absolute", p.label())
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: symlink directory rejected", p.label())
	}
	if err := p.PrivateInfo(path, info, true); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	// Check the opened inode as well, so replacement between Lstat and OpenRoot
	// cannot silently grant access to a different directory.
	f, err := r.Open(".")
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	opened, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(fmt.Errorf("%s: directory changed while opening", p.label()), statErr, closeErr, r.Close())
	}
	return r, nil
}

// Open opens the file name under r with the private-file checks. Native
// openat is intentional: os.Root.OpenFile resolves relative symlinks
// itself on some platforms before applying O_NOFOLLOW to the final open.
func (p Policy) Open(r *os.Root, name string, flags int) (*os.File, error) {
	return p.OpenWith(r, name, flags, openat)
}

// openat is the native openat call Open performs. It is a variable only so a
// test in this package can replace a file between the lstat and the open.
var openat = func(fd int, name string, flags int) (int, error) { return unix.Openat(fd, name, flags, 0o600) }

// OpenWith is Open with the openat call supplied by the caller, so a test can
// observe or replace it.
func (p Policy) OpenWith(r *os.Root, name string, flags int, open func(int, string, int) (int, error)) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("%s: private file name must be a basename", p.label())
	}
	prior, err := r.Lstat(name)
	if err == nil {
		if err := p.PrivateInfo(filepath.Join(r.Name(), name), prior, false); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	// Defer truncation until after the opened inode is validated.
	fd, openErr := open(int(dir.Fd()), name, (flags&^os.O_TRUNC)|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	closeErr := dir.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	f := os.NewFile(uintptr(fd), name)
	if closeErr != nil {
		return nil, errors.Join(closeErr, f.Close())
	}
	info, err := f.Stat()
	if err == nil {
		err = p.PrivateInfo(filepath.Join(r.Name(), name), info, false)
	}
	if err == nil && prior != nil && !os.SameFile(prior, info) {
		err = fmt.Errorf("%w: private file changed during open", p.changed())
	}
	if err == nil && flags&os.O_TRUNC != 0 {
		err = f.Truncate(0)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
