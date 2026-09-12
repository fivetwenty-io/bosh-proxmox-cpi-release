package allocationjournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DurabilityError means the caller must not submit another mutation. Atomic
// rename may already have committed, so reopening and reconciliation are required.
type DurabilityError struct{ Err error }

func (e *DurabilityError) Error() string {
	return "journal durability failure; reconcile before mutation: " + e.Err.Error()
}
func (e *DurabilityError) Unwrap() error { return e.Err }

type diskEnvelope struct {
	Version int             `json:"version"`
	SHA256  string          `json:"sha256"`
	Payload json.RawMessage `json:"payload"`
}
type fileOps struct {
	write    func(*os.File, []byte) (int, error)
	syncFile func(*os.File) error
	rename   func(*os.Root, string, string) error
	syncDir  func(*os.Root) error
}

func defaultFileOps() fileOps {
	return fileOps{
		write:    func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		syncFile: func(f *os.File) error { return f.Sync() },
		rename:   func(r *os.Root, a, b string) error { return r.Rename(a, b) },
		syncDir:  syncDirectory,
	}
}
func syncDirectory(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func pathKey(value string) string { s := sha256.Sum256([]byte(value)); return hex.EncodeToString(s[:]) }
func validateNamespace(namespace string) error {
	if !nonblank(namespace) || namespace != strings.TrimSpace(namespace) || len(namespace) > 1024 || strings.ContainsAny(namespace, "/\\\x00") || namespace == "." || namespace == ".." {
		return fmt.Errorf("journal: invalid namespace")
	}
	return nil
}
func privateInfo(info os.FileInfo, directory bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 || info.IsDir() != directory || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("journal: unsafe ownership, permissions, or file type")
	}
	if !directory && st.Nlink != 1 {
		return fmt.Errorf("journal: multiply linked file rejected")
	}
	return nil
}
func openPrivateRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("journal: directory must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("journal: symlink directory rejected")
	}
	if err := privateInfo(info, true); err != nil {
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
		return nil, errors.Join(fmt.Errorf("journal: directory changed while opening"), statErr, closeErr, r.Close())
	}
	return r, nil
}

// Native openat is intentional: os.Root.OpenFile resolves relative symlinks
// itself on some platforms before applying O_NOFOLLOW to the final open.
func openPrivate(r *os.Root, name string, flags int) (*os.File, error) {
	return openPrivateWith(r, name, flags, func(fd int, name string, flags int) (int, error) { return unix.Openat(fd, name, flags, 0o600) })
}
func openPrivateWith(r *os.Root, name string, flags int, open func(int, string, int) (int, error)) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("journal: private file name must be a basename")
	}
	prior, err := r.Lstat(name)
	if err == nil {
		if err := privateInfo(prior, false); err != nil {
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
		err = privateInfo(info, false)
	}
	if err == nil && prior != nil && !os.SameFile(prior, info) {
		err = fmt.Errorf("%w: private file changed during open", ErrReconciliationRequired)
	}
	if err == nil && flags&os.O_TRUNC != 0 {
		err = f.Truncate(0)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func strictDecode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
func readJSON(r *os.Root, name string, out any) error {
	f, err := openPrivate(r, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return fmt.Errorf("%w: oversized file", ErrCorrupt)
	}
	var env diskEnvelope
	if err = strictDecode(data, &env); err != nil {
		return fmt.Errorf("%w: malformed envelope: %w", ErrCorrupt, err)
	}
	sum := sha256.Sum256(env.Payload)
	if env.Version != Version || env.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: envelope version/checksum", ErrCorrupt)
	}
	if err = strictDecode(env.Payload, out); err != nil {
		return fmt.Errorf("%w: malformed payload: %w", ErrCorrupt, err)
	}
	return nil
}
func atomicJSON(r *os.Root, name string, value any, ops fileOps) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = &DurabilityError{Err: retErr}
		}
	}()
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	data, err := json.Marshal(diskEnvelope{Version: Version, SHA256: hex.EncodeToString(sum[:]), Payload: payload})
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return fmt.Errorf("journal: record exceeds size limit")
	}
	if info, err := r.Lstat(name); err == nil {
		if err := privateInfo(info, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := NewAllocationID()
	if err != nil {
		return err
	}
	temp := ".tmp-" + id
	f, err := openPrivate(r, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	closed := false
	renamed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, f.Close())
		}
		if !renamed {
			retErr = errors.Join(retErr, r.Remove(temp))
		}
	}()
	n, err := ops.write(f, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := ops.syncFile(f); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := ops.rename(r, temp, name); err != nil {
		return err
	}
	renamed = true
	return ops.syncDir(r)
}

type fileLock struct{ file *os.File }

func tryLock(r *os.Root, name string) (*fileLock, bool, error) {
	f, err := openPrivate(r, name, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, false, f.Close()
	}
	if err != nil {
		return nil, false, errors.Join(err, f.Close())
	}
	return &fileLock{file: f}, true, nil
}
func (l *fileLock) close() error {
	return errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
}
func waitLock(ctx context.Context, r *os.Root, name string) (*fileLock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, ok, err := tryLock(r, name)
		if err != nil {
			return nil, err
		}
		if ok {
			return l, nil
		}
		if err := pause(ctx); err != nil {
			return nil, err
		}
	}
}
func pause(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
