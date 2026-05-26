// Package configdrive internal tests — exercises unexported helpers that cannot
// be reached from the external _test package.
package configdrive

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/filesystem"
)

// --------------------------------------------------------------------------
// memFile — in-memory filesystem.File.
// --------------------------------------------------------------------------

type memFile struct {
	mu     sync.Mutex
	buf    []byte
	pos    int
	closed bool
	name   string
}

func newMemFile(name string) *memFile { return &memFile{name: name} }

func (m *memFile) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pos >= len(m.buf) {
		return 0, io.EOF
	}
	n := copy(p, m.buf[m.pos:])
	m.pos += n
	return n, nil
}

func (m *memFile) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = append(m.buf, p...)
	m.pos = len(m.buf)
	return len(p), nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch whence {
	case io.SeekStart:
		m.pos = int(offset)
	case io.SeekCurrent:
		m.pos += int(offset)
	case io.SeekEnd:
		m.pos = len(m.buf) + int(offset)
	}
	if m.pos < 0 {
		m.pos = 0
	}
	return int64(m.pos), nil
}

func (m *memFile) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

// fs.File interface — Stat and ReadDir required.
func (m *memFile) Stat() (fs.FileInfo, error) {
	return &memFileInfo{name: m.name, size: int64(len(m.buf))}, nil
}

func (m *memFile) ReadDir(_ int) ([]fs.DirEntry, error) { return nil, nil }

// memFileInfo implements fs.FileInfo.
type memFileInfo struct {
	name string
	size int64
}

func (i *memFileInfo) Name() string       { return i.name }
func (i *memFileInfo) Size() int64        { return i.size }
func (i *memFileInfo) Mode() os.FileMode  { return 0o644 }
func (i *memFileInfo) ModTime() time.Time { return time.Time{} }
func (i *memFileInfo) IsDir() bool        { return false }
func (i *memFileInfo) Sys() any           { return nil }

// --------------------------------------------------------------------------
// trackingFS — minimal filesystem.FileSystem that records open + close.
// Unimplemented methods panic — only Mkdir and OpenFile are exercised by
// writeFiles.
// --------------------------------------------------------------------------

type trackingFS struct {
	filesystem.FileSystem // nil embed — panics on unexpected calls
	mu                    sync.Mutex
	opened                []*memFile
}

func newTrackingFS() *trackingFS { return &trackingFS{} }

func (t *trackingFS) Mkdir(_ string) error { return nil }

func (t *trackingFS) OpenFile(name string, _ int) (filesystem.File, error) {
	f := newMemFile(name)
	t.mu.Lock()
	t.opened = append(t.opened, f)
	t.mu.Unlock()
	return f, nil
}

// allClosed returns true when every file opened via OpenFile has been closed,
// along with a slice of names of any unclosed handles.
func (t *trackingFS) allClosed() (bool, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var leaked []string
	for _, f := range t.opened {
		f.mu.Lock()
		closed := f.closed
		name := f.name
		f.mu.Unlock()
		if !closed {
			leaked = append(leaked, name)
		}
	}
	return len(leaked) == 0, leaked
}

// --------------------------------------------------------------------------
// TestBuild_ClosesFiles — verifies writeFiles closes every file it opens.
// --------------------------------------------------------------------------

// TestBuild_ClosesFiles verifies that writeFiles closes every file handle it
// opens via OpenFile, on both success and normal execution paths. Uses a
// cross-platform in-memory fake filesystem — no /proc/self/fd dependency.
func TestBuild_ClosesFiles(t *testing.T) {
	t.Parallel()

	tfs := newTrackingFS()
	payload := []byte(`{"agent_id":"test-close"}`)

	if err := writeFiles(tfs, payload); err != nil {
		t.Fatalf("writeFiles: %v", err)
	}

	// writeFiles opens exactly 4 files: openstack user_data, openstack meta_data.json,
	// ec2 user-data, ec2 meta-data.json.
	if len(tfs.opened) != 4 {
		t.Errorf("expected 4 OpenFile calls, got %d", len(tfs.opened))
	}

	ok, leaked := tfs.allClosed()
	if !ok {
		t.Errorf("file handle(s) not closed after writeFiles: %v", leaked)
	}
}

// --------------------------------------------------------------------------
// writeFiles error-path coverage — Mkdir, OpenFile, Write failures.
// Each test wraps trackingFS with a failure injector and asserts:
//   - the specific error is returned (wrapped, message identifies the failing path)
//   - no file handle leaks (every successfully-opened file was closed)
// --------------------------------------------------------------------------

// injectFS wraps trackingFS and lets a test fail Mkdir or OpenFile for a
// particular path, or fail Write on the next OpenFile call.
type injectFS struct {
	*trackingFS
	mkdirFailOn string // exact path; non-empty enables failure
	openFailOn  string
	writeFailOn string
	mkdirErr    error
	openErr     error
	writeErr    error
}

func (i *injectFS) Mkdir(path string) error {
	if i.mkdirFailOn != "" && path == i.mkdirFailOn {
		return i.mkdirErr
	}
	return i.trackingFS.Mkdir(path)
}

func (i *injectFS) OpenFile(name string, flag int) (filesystem.File, error) {
	if i.openFailOn != "" && name == i.openFailOn {
		return nil, i.openErr
	}
	f, err := i.trackingFS.OpenFile(name, flag)
	if err != nil {
		return nil, err
	}
	if i.writeFailOn != "" && name == i.writeFailOn {
		// Wrap the underlying memFile in a writer that always fails so
		// the caller sees the injected write error but Close still runs.
		mf := f.(*memFile)
		return &failWriteFile{memFile: mf, writeErr: i.writeErr}, nil
	}
	return f, nil
}

// failWriteFile returns writeErr from every Write call. Close, Read, Seek,
// Stat all delegate to the embedded memFile so close-tracking still works.
type failWriteFile struct {
	*memFile
	writeErr error
}

func (f *failWriteFile) Write(_ []byte) (int, error) {
	return 0, f.writeErr
}

// errSentinel is a unique error so error-message assertions can verify the
// wrap path used %w semantics (errors.Is must match).
type errSentinel struct{ msg string }

func (e *errSentinel) Error() string { return e.msg }

// TestBuild_WriteFilesError_MkdirFails verifies that a Mkdir failure on the
// first directory ("/openstack") returns a wrapped error naming the path and
// does not leak file handles (no OpenFile should have been called before
// the failure, so trackingFS.allClosed() trivially holds).
func TestBuild_WriteFilesError_MkdirFails(t *testing.T) {
	t.Parallel()

	sentinel := &errSentinel{msg: "injected: read-only filesystem"}
	tfs := newTrackingFS()
	ifs := &injectFS{
		trackingFS:  tfs,
		mkdirFailOn: "/openstack",
		mkdirErr:    sentinel,
	}

	err := writeFiles(ifs, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("expected error from injected Mkdir failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v should wrap sentinel via %%w", err)
	}
	if !strings.Contains(err.Error(), "/openstack") {
		t.Errorf("error %q should name the failing path", err.Error())
	}

	// Mkdir failed before any OpenFile; nothing to close, nothing should
	// have leaked. allClosed() returns true when no files were opened.
	if ok, leaked := tfs.allClosed(); !ok {
		t.Errorf("unexpected file handle leaks after Mkdir failure: %v", leaked)
	}
	if len(tfs.opened) != 0 {
		t.Errorf("expected 0 OpenFile calls before Mkdir failure, got %d", len(tfs.opened))
	}
}

// TestBuild_WriteFilesError_OpenFails verifies that an OpenFile failure on
// the first user_data write returns a wrapped error and leaves no leaked
// handles. The two preceding Mkdir calls (/openstack and /openstack/latest)
// must have run successfully, so trackingFS records 0 opened handles.
func TestBuild_WriteFilesError_OpenFails(t *testing.T) {
	t.Parallel()

	sentinel := &errSentinel{msg: "injected: permission denied"}
	tfs := newTrackingFS()
	ifs := &injectFS{
		trackingFS: tfs,
		openFailOn: "/openstack/latest/user_data",
		openErr:    sentinel,
	}

	err := writeFiles(ifs, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("expected error from injected OpenFile failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v should wrap sentinel via %%w", err)
	}
	if !strings.Contains(err.Error(), "/openstack/latest/user_data") {
		t.Errorf("error %q should name the failing path", err.Error())
	}

	// No file was successfully opened, so no handle could leak.
	if ok, leaked := tfs.allClosed(); !ok {
		t.Errorf("unexpected file handle leaks after OpenFile failure: %v", leaked)
	}
	if len(tfs.opened) != 0 {
		t.Errorf("expected 0 successful OpenFile calls, got %d", len(tfs.opened))
	}
}

// TestBuild_WriteFilesError_WriteFails verifies that a Write failure after a
// successful OpenFile (a) returns the wrapped write error and (b) still
// closes the underlying file handle — i.e. the deferred Close in writeISOFile
// runs even on the Write-error path. This is the fd-leak guard.
func TestBuild_WriteFilesError_WriteFails(t *testing.T) {
	t.Parallel()

	sentinel := &errSentinel{msg: "injected: disk full"}
	tfs := newTrackingFS()
	ifs := &injectFS{
		trackingFS:  tfs,
		writeFailOn: "/openstack/latest/user_data",
		writeErr:    sentinel,
	}

	err := writeFiles(ifs, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("expected error from injected Write failure, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v should wrap sentinel via %%w", err)
	}
	if !strings.Contains(err.Error(), "/openstack/latest/user_data") {
		t.Errorf("error %q should name the failing path", err.Error())
	}

	// Exactly one file should have been opened before Write failed.
	if len(tfs.opened) != 1 {
		t.Errorf("expected 1 OpenFile call before Write failure, got %d", len(tfs.opened))
	}
	// Critical assertion: the opened file MUST have been closed by the
	// deferred Close in writeISOFile, even though Write returned an error.
	if ok, leaked := tfs.allClosed(); !ok {
		t.Errorf("file handle leaked after Write failure: %v", leaked)
	}
}
