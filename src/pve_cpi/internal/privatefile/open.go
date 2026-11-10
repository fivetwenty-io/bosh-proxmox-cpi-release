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
	st, _ := info.Sys().(*syscall.Stat_t)
	return p.privateStat(path, st, info.Mode(), directory)
}

// privateStat is PrivateInfo on a stat result and its file mode. st is nil
// when the platform reported no ownership metadata.
func (p Policy) privateStat(path string, st *syscall.Stat_t, mode os.FileMode, directory bool) error {
	euid := os.Geteuid()
	unsafe := &UnsafePathError{Path: path, UID: -1, EUID: euid, Mode: mode, WantDirectory: directory}
	if st != nil {
		unsafe.UID = int(st.Uid)
	}
	switch {
	case st == nil || int64(st.Uid) != int64(euid):
		unsafe.Cause = p.ownership()
	case mode.IsDir() != directory || !directory && !mode.IsRegular():
		unsafe.Cause = p.fileType()
	case mode.Perm()&0o077 != 0:
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
	// Reading a doomed inode is handled where it belongs, by the inode
	// comparison in OpenWith and the retry in the envelope read.
	if !directory && st.Nlink > 1 {
		return fmt.Errorf("%s: multiply linked file rejected", p.label())
	}
	return nil
}

// fileModeOf converts a raw st_mode to the os.FileMode that os.Lstat reports
// for it, so a descriptor checked before it becomes an *os.File is judged and
// reported exactly as a named file is. It is generic because st_mode is a
// uint16 on darwin and a uint32 on linux.
func fileModeOf[M ~uint16 | ~uint32](m M) os.FileMode {
	raw := uint32(m)
	mode := os.FileMode(raw & 0o777)
	switch raw & syscall.S_IFMT {
	case syscall.S_IFREG:
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	case syscall.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	default:
		mode |= os.ModeIrregular
	}
	if raw&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if raw&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if raw&syscall.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
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
//
// The open adds O_NONBLOCK. Opening a FIFO for reading otherwise blocks until
// a writer appears, and the file type check only runs once the open returns,
// so a FIFO swapped in after the lstat could stall the caller for good. The
// descriptor is checked first and becomes an *os.File only after the checks
// pass and O_NONBLOCK is cleared, so the caller always gets a blocking file.
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
	fd, openErr := open(int(dir.Fd()), name, (flags&^os.O_TRUNC)|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK)
	closeErr := dir.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, unix.Close(fd))
	}
	if err := p.checkOpened(filepath.Join(r.Name(), name), fd, prior); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	f := os.NewFile(uintptr(fd), name)
	if flags&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	return f, nil
}

// checkOpened runs the private checks on the descriptor OpenWith opened,
// confirms that it is the inode prior described when the name existed at the
// lstat, and then clears O_NONBLOCK.
func (p Policy) checkOpened(path string, fd int, prior os.FileInfo) error {
	var st syscall.Stat_t
	if err := ignoringEINTR(func() error { return syscall.Fstat(fd, &st) }); err != nil {
		return &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if err := p.privateStat(path, &st, fileModeOf(st.Mode), false); err != nil {
		return err
	}
	if prior != nil && !sameInode(prior, &st) {
		return fmt.Errorf("%w: private file changed during open", p.changed())
	}
	return unix.SetNonblock(fd, false)
}

// sameInode reports whether info and st describe one inode, which is the
// comparison os.SameFile makes.
func sameInode(info os.FileInfo, st *syscall.Stat_t) bool {
	named, ok := info.Sys().(*syscall.Stat_t)
	return ok && named.Dev == st.Dev && named.Ino == st.Ino
}

// ignoringEINTR repeats call while it fails with EINTR, as the os package
// does around its own fstat.
func ignoringEINTR(call func() error) error {
	for {
		if err := call(); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
