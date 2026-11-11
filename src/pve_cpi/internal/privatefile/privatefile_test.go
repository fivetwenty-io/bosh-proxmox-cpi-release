package privatefile

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func keyedPolicy(key []byte, maxBytes int) Policy {
	return Policy{
		Label:        "cache",
		MaxBytes:     maxBytes,
		IntegrityKey: "hmac_sha256",
		Integrity: func(payload []byte) string {
			mac := hmac.New(sha256.New, key)
			mac.Write(payload)
			return hex.EncodeToString(mac.Sum(nil))
		},
	}
}

func plainPolicy(maxBytes int) Policy {
	return Policy{MaxBytes: maxBytes, IntegrityKey: "sha256", Integrity: SHA256Hex}
}

func privateRoot(t *testing.T, p Policy) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := p.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

func TestKeyedEnvelopeRoundTrip(t *testing.T) {
	p := keyedPolicy([]byte("k1"), 1<<16)
	root, dir := privateRoot(t, p)
	want := sample{Name: "pve1", Count: 3}
	if err := p.WriteEnvelope(root, "entry.json", 7, want, NoSyncOps()); err != nil {
		t.Fatalf("WriteEnvelope: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 {
		t.Fatalf("file fields = %v, %v; want version, hmac_sha256, payload", fields, err)
	}
	if !strings.HasPrefix(string(raw), `{"version":7,"hmac_sha256":"`) || !strings.HasSuffix(string(raw), `,"payload":{"name":"pve1","count":3}}`) {
		t.Fatalf("file = %s; want fields in version, hmac_sha256, payload order", raw)
	}
	env, err := p.ReadEnvelope(root, "entry.json")
	if err != nil {
		t.Fatalf("ReadEnvelope: %v", err)
	}
	var got sample
	if err := p.DecodeEnvelope(env, 7, &got); err != nil || got != want {
		t.Fatalf("DecodeEnvelope = %+v, %v; want %+v", got, err, want)
	}
	if info, err := os.Stat(filepath.Join(dir, "entry.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v; want 0600", info, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestKeyedEnvelopeRejectsWrongKeyVersionAndTamper(t *testing.T) {
	p := keyedPolicy([]byte("k1"), 1<<16)
	root, dir := privateRoot(t, p)
	if err := p.WriteEnvelope(root, "entry.json", 1, sample{Name: "a"}, NoSyncOps()); err != nil {
		t.Fatal(err)
	}
	env, err := p.ReadEnvelope(root, "entry.json")
	if err != nil {
		t.Fatal(err)
	}
	var out sample
	other := keyedPolicy([]byte("k2"), 1<<16)
	if err := other.DecodeEnvelope(env, 1, &out); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key = %v; want ErrCorrupt", err)
	}
	if err := p.DecodeEnvelope(env, 2, &out); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong version = %v; want ErrCorrupt", err)
	}
	plain := plainPolicy(1 << 16)
	if err := plain.DecodeEnvelope(env, 1, &out); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("plain checksum on a keyed file = %v; want ErrCorrupt", err)
	}
	env.Payload = json.RawMessage(`{"name":"b"}`)
	if err := p.DecodeEnvelope(env, 1, &out); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered payload = %v; want ErrCorrupt", err)
	}
	// A plain-checksum reader refuses the keyed key name as an unknown field.
	if _, err := plain.ReadEnvelope(root, "entry.json"); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "malformed envelope") {
		t.Fatalf("plain reader on keyed file = %v; want malformed envelope", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unknown.json"), []byte(`{"version":1,"hmac_sha256":"","payload":{},"extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadEnvelope(root, "unknown.json"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown field = %v; want ErrCorrupt", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trailing.json"), []byte(`{"version":1,"hmac_sha256":"","payload":{}} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadEnvelope(root, "trailing.json"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing data = %v; want ErrCorrupt", err)
	}
	var out2 struct {
		Name string `json:"name"`
	}
	good := Envelope{Version: 1, Integrity: p.Integrity([]byte(`{"name":"a","extra":1}`)), Payload: json.RawMessage(`{"name":"a","extra":1}`)}
	if err := p.DecodeEnvelope(good, 1, &out2); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "malformed payload") {
		t.Fatalf("payload with an unknown field = %v; want malformed payload", err)
	}
}

func TestSizeParameterBoundsReadAndWrite(t *testing.T) {
	small := plainPolicy(128)
	root, dir := privateRoot(t, small)
	if err := small.WriteEnvelope(root, "ok.json", 1, sample{Name: "x"}, NoSyncOps()); err != nil {
		t.Fatalf("a record under the limit: %v", err)
	}
	big := sample{Name: strings.Repeat("n", 200)}
	err := small.WriteEnvelope(root, "big.json", 1, big, NoSyncOps())
	if err == nil || err.Error() != "private file: record exceeds size limit" {
		t.Fatalf("oversized write = %v; want the size error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "big.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized write left a file: %v", statErr)
	}
	// The same record fits under a larger limit, and the smaller limit then
	// refuses to read it.
	large := plainPolicy(1 << 12)
	if err := large.WriteEnvelope(root, "big.json", 1, big, NoSyncOps()); err != nil {
		t.Fatalf("a record under the larger limit: %v", err)
	}
	if _, err := small.ReadEnvelope(root, "big.json"); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "oversized file") {
		t.Fatalf("oversized read = %v; want ErrCorrupt oversized file", err)
	}
	if _, err := large.ReadEnvelope(root, "big.json"); err != nil {
		t.Fatalf("read under the larger limit: %v", err)
	}
}

func TestPolicyMustBeComplete(t *testing.T) {
	cases := map[string]Policy{
		"no size limit":    {IntegrityKey: "sha256", Integrity: SHA256Hex},
		"no integrity":     {MaxBytes: 10, IntegrityKey: "sha256"},
		"no key":           {MaxBytes: 10, Integrity: SHA256Hex},
		"reserved key":     {MaxBytes: 10, IntegrityKey: "payload", Integrity: SHA256Hex},
		"key needing json": {MaxBytes: 10, IntegrityKey: `a"b`, Integrity: SHA256Hex},
	}
	root, _ := privateRoot(t, plainPolicy(10))
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := p.WriteEnvelope(root, "x.json", 1, 1, NoSyncOps()); err == nil {
				t.Error("WriteEnvelope accepted the policy")
			}
			if _, err := p.ReadEnvelope(root, "x.json"); err == nil {
				t.Error("ReadEnvelope accepted the policy")
			}
			if err := p.DecodeEnvelope(Envelope{}, 1, new(int)); err == nil {
				t.Error("DecodeEnvelope accepted the policy")
			}
		})
	}
}

func TestDurabilityFlagControlsSyncs(t *testing.T) {
	root, dir := privateRoot(t, plainPolicy(1<<10))
	closed, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	closedRoot, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := closedRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := DefaultOps().SyncFile(closed); err == nil {
		t.Error("a durable file sync on a closed file reported success, so it did not reach the file")
	}
	if err := DefaultOps().SyncDir(closedRoot); err == nil {
		t.Error("a durable directory sync on a closed root reported success, so it did not reach the directory")
	}
	if err := NoSyncOps().SyncFile(closed); err != nil {
		t.Errorf("a non-durable file sync = %v; want a no-op", err)
	}
	if err := NoSyncOps().SyncDir(closedRoot); err != nil {
		t.Errorf("a non-durable directory sync = %v; want a no-op", err)
	}

	p := plainPolicy(1 << 10)
	for _, durable := range []bool{true, false} {
		ops := NoSyncOps()
		if durable {
			ops = DefaultOps()
		}
		fileSyncs, dirSyncs := 0, 0
		inner := ops
		ops.SyncFile = func(f *os.File) error { fileSyncs++; return inner.SyncFile(f) }
		ops.SyncDir = func(r *os.Root) error { dirSyncs++; return inner.SyncDir(r) }
		if err := p.WriteEnvelope(root, "w.json", 1, sample{}, ops); err != nil {
			t.Fatalf("durable=%v: %v", durable, err)
		}
		if fileSyncs != 1 || dirSyncs != 1 {
			t.Errorf("durable=%v: the write called %d file syncs and %d directory syncs; want one each", durable, fileSyncs, dirSyncs)
		}
	}
}

func TestWriteEnvelopeFailuresLeaveNoTempFile(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, dir := privateRoot(t, p)
	boom := errors.New("injected")
	cases := map[string]func(*Ops){
		"short write": func(o *Ops) { o.Write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:1]) } },
		"write error": func(o *Ops) { o.Write = func(*os.File, []byte) (int, error) { return 0, boom } },
		"file sync":   func(o *Ops) { o.SyncFile = func(*os.File) error { return boom } },
		"rename":      func(o *Ops) { o.Rename = func(*os.Root, string, string) error { return boom } },
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			ops := NoSyncOps()
			inject(&ops)
			if err := p.WriteEnvelope(root, "f.json", 1, sample{}, ops); err == nil {
				t.Fatal("WriteEnvelope succeeded")
			}
			if left, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(left) != 0 {
				t.Fatalf("temporary files left behind: %v", left)
			}
			if _, err := os.Stat(filepath.Join(dir, "f.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed write produced the target: %v", err)
			}
		})
	}
	dirSyncErr := NoSyncOps()
	dirSyncErr.SyncDir = func(*os.Root) error { return boom }
	if err := p.WriteEnvelope(root, "f.json", 1, sample{}, dirSyncErr); !errors.Is(err, boom) {
		t.Fatalf("directory sync failure = %v; want the injected error", err)
	}
	if err := p.WriteEnvelope(root, "g.json", 1, make(chan int), NoSyncOps()); err == nil {
		t.Fatal("an unmarshalable value was written")
	}
}

func TestNilOpsStepsDefaultToDurable(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, _ := privateRoot(t, p)
	syncs := 0
	ops := Ops{SyncFile: func(*os.File) error { syncs++; return nil }}
	if err := p.WriteEnvelope(root, "n.json", 1, sample{}, ops); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("file syncs = %d, want 1", syncs)
	}
	if _, err := p.ReadEnvelope(root, "n.json"); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateChecksAndWording(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, dir := privateRoot(t, p)
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := p.Open(root, "f", os.O_RDONLY)
	var unsafe *UnsafePathError
	if !errors.Is(err, ErrUnsafeMode) || !errors.As(err, &unsafe) || unsafe.Path != path {
		t.Fatalf("group-readable file = %v; want ErrUnsafeMode naming the path", err)
	}
	if !strings.HasPrefix(err.Error(), "private file: unsafe permissions: "+path+" has mode 0640") {
		t.Fatalf("default wording = %q", err)
	}

	custom := p
	custom.Label = "cache"
	custom.Mode = Sentinel("cache: unsafe permissions", ErrUnsafeMode)
	_, err = custom.Open(root, "f", os.O_RDONLY)
	if !errors.Is(err, ErrUnsafeMode) || !errors.Is(err, custom.Mode) || !strings.HasPrefix(err.Error(), "cache: unsafe permissions: ") {
		t.Fatalf("custom wording = %v; want the caller's text matching both causes", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "second")); err != nil {
		t.Fatal(err)
	}
	if _, err := custom.Open(root, "f", os.O_RDONLY); err == nil || err.Error() != "cache: multiply linked file rejected" {
		t.Fatalf("linked file = %v", err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Open(root, "link", os.O_RDONLY); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("symlink = %v; want ErrUnsafeFileType", err)
	}
	for _, name := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := custom.Open(root, name, os.O_RDONLY); err == nil || err.Error() != "cache: private file name must be a basename" {
			t.Errorf("name %q = %v", name, err)
		}
	}
	if _, err := p.Open(root, "absent", os.O_RDONLY); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent = %v; want not-exist", err)
	}
}

func TestOpenRootRefusals(t *testing.T) {
	p := Policy{Label: "cache"}
	if _, err := p.OpenRoot("relative"); err == nil || err.Error() != "cache: directory must be absolute" {
		t.Fatalf("relative = %v", err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenRoot(dir); !errors.Is(err, ErrUnsafeMode) {
		t.Fatalf("0755 directory = %v; want ErrUnsafeMode", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenRoot(link); err == nil || err.Error() != "cache: symlink directory rejected" {
		t.Fatalf("symlink = %v", err)
	}
	if _, err := p.OpenRoot(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing = %v; want not-exist", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenRoot(file); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("regular file = %v; want ErrUnsafeFileType", err)
	}
}

type stubInfo struct {
	mode os.FileMode
	sys  any
}

func (s stubInfo) Name() string       { return "stub" }
func (s stubInfo) Size() int64        { return 0 }
func (s stubInfo) Mode() os.FileMode  { return s.mode }
func (s stubInfo) ModTime() time.Time { return time.Time{} }
func (s stubInfo) IsDir() bool        { return s.mode.IsDir() }
func (s stubInfo) Sys() any           { return s.sys }

func TestPrivateInfoPrecedenceAndText(t *testing.T) {
	p := Policy{}
	own := uint32(os.Geteuid())
	other := own + 1
	cases := []struct {
		name      string
		info      stubInfo
		directory bool
		cause     error
		contains  string
	}{
		{"another owner", stubInfo{os.ModeDir | 0o700, &syscall.Stat_t{Uid: other, Nlink: 1}}, true, ErrUnsafeOwnership, "is owned by uid"},
		{"no metadata", stubInfo{os.ModeDir | 0o700, nil}, true, ErrUnsafeOwnership, "has no ownership metadata"},
		{"open directory", stubInfo{os.ModeDir | 0o750, &syscall.Stat_t{Uid: own, Nlink: 1}}, true, ErrUnsafeMode, "has mode 0750"},
		{"file for directory", stubInfo{0o600, &syscall.Stat_t{Uid: own, Nlink: 1}}, true, ErrUnsafeFileType, "is not a directory"},
		{"directory for file", stubInfo{os.ModeDir | 0o700, &syscall.Stat_t{Uid: own, Nlink: 1}}, false, ErrUnsafeFileType, "is not a regular file"},
		{"owner outranks type", stubInfo{os.ModeSocket | 0o777, &syscall.Stat_t{Uid: other, Nlink: 1}}, false, ErrUnsafeOwnership, "is owned by uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.PrivateInfo("/p", tc.info, tc.directory)
			var unsafe *UnsafePathError
			if !errors.Is(err, tc.cause) || !errors.As(err, &unsafe) || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("PrivateInfo = %v; want %v containing %q", err, tc.cause, tc.contains)
			}
		})
	}
	if err := p.PrivateInfo("/p", stubInfo{0o600, &syscall.Stat_t{Uid: own, Nlink: 2}}, false); err == nil {
		t.Error("a file with two links was accepted")
	}
	if err := p.PrivateInfo("/p", stubInfo{0o600, &syscall.Stat_t{Uid: own, Nlink: 0}}, false); err != nil {
		t.Errorf("an unlinked inode = %v; want accepted", err)
	}
}

func TestOpenWithUsesTheSuppliedOpenat(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, _ := privateRoot(t, p)
	if err := p.WriteEnvelope(root, "r.json", 1, sample{Name: "a"}, NoSyncOps()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	open := func(fd int, name string, flags int) (int, error) { calls++; return openat(fd, name, flags) }
	f, err := p.OpenWith(root, "r.json", os.O_RDONLY, open)
	if err != nil || calls != 1 {
		t.Fatalf("OpenWith = %v after %d calls", err, calls)
	}
	_ = f.Close()
	if _, err := p.OpenWith(root, "r.json", os.O_RDONLY, func(int, string, int) (int, error) { return -1, syscall.EIO }); !errors.Is(err, syscall.EIO) {
		t.Fatalf("openat failure = %v; want EIO", err)
	}
}

// TestOpenRefusesAFIFOSwappedInBeforeTheOpen puts a FIFO at the name between
// the lstat and the open. A blocking open would wait for a writer forever, so
// the refusal has to come back at once.
func TestOpenRefusesAFIFOSwappedInBeforeTheOpen(t *testing.T) {
	for name, existing := range map[string]bool{"absent at the lstat": false, "regular file at the lstat": true} {
		t.Run(name, func(t *testing.T) {
			p := plainPolicy(1 << 10)
			root, dir := privateRoot(t, p)
			if existing {
				if err := p.WriteEnvelope(root, "r.json", 1, sample{Name: "a"}, NoSyncOps()); err != nil {
					t.Fatal(err)
				}
			}
			fifo := filepath.Join(dir, "r.json")
			prev := openat
			t.Cleanup(func() { openat = prev })
			openat = func(fd int, name string, flags int) (int, error) {
				if name == "r.json" {
					if err := os.Remove(fifo); err != nil && !errors.Is(err, os.ErrNotExist) {
						return -1, err
					}
					if err := syscall.Mkfifo(fifo, 0o600); err != nil {
						return -1, err
					}
				}
				return prev(fd, name, flags)
			}
			done := make(chan error, 1)
			go func() {
				f, err := p.Open(root, "r.json", os.O_RDONLY)
				if f != nil {
					_ = f.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrUnsafeFileType) || !strings.Contains(err.Error(), "is not a regular file") {
					t.Fatalf("Open of a FIFO = %v; want the file type refusal", err)
				}
			case <-time.After(time.Second):
				// Open the FIFO for writing so the stuck reader returns and the
				// test can end.
				if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil { // #nosec G304 -- the test's own FIFO
					_ = w.Close()
				}
				t.Fatalf("Open of a FIFO swapped in before the open blocked for a writer, then returned %v", <-done)
			}
		})
	}
}

// TestOpenRefusesASymlinkPlantedAtAnAbsentName plants a symlink to a valid
// private file at a name the lstat found absent. Only O_NOFOLLOW stops the
// open from following it, because no earlier lstat result can be compared.
func TestOpenRefusesASymlinkPlantedAtAnAbsentName(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, dir := privateRoot(t, p)
	elsewhere, _ := privateRoot(t, p)
	if err := p.WriteEnvelope(elsewhere, "target.json", 1, sample{Name: "planted"}, NoSyncOps()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(elsewhere.Name(), "target.json")
	prev := openat
	t.Cleanup(func() { openat = prev })
	openat = func(fd int, name string, flags int) (int, error) {
		if name == "r.json" {
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				return -1, err
			}
		}
		return prev(fd, name, flags)
	}
	f, err := p.Open(root, "r.json", os.O_RDONLY)
	if err == nil {
		_ = f.Close()
		t.Fatal("Open followed a symlink planted after the lstat")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Open of a planted symlink = %v; want ELOOP from O_NOFOLLOW", err)
	}
}

// TestOpenReturnsABlockingFile shows that the O_NONBLOCK the open adds is
// gone from the file the caller gets.
func TestOpenReturnsABlockingFile(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, _ := privateRoot(t, p)
	f, err := p.Open(root, "r.json", os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var flagErr error
	if err := raw.Control(func(fd uintptr) { flags, flagErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil || flagErr != nil {
		t.Fatalf("F_GETFL: %v, %v", err, flagErr)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatal("Open returned a file with O_NONBLOCK still set")
	}
}

func TestFileModeOfMatchesLstat(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, dir := privateRoot(t, p)
	if err := p.WriteEnvelope(root, "file", 1, sample{}, NoSyncOps()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "sub"), 0o700|os.ModeSticky|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", "fifo", "link", "sub"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if got := fileModeOf(st.Mode); got != info.Mode() {
			t.Errorf("%s: fileModeOf = %v; os.Lstat says %v", name, got, info.Mode())
		}
	}
	cases := map[uint32]os.FileMode{
		syscall.S_IFSOCK | 0o600:                  os.ModeSocket | 0o600,
		syscall.S_IFCHR | 0o600:                   os.ModeDevice | os.ModeCharDevice | 0o600,
		syscall.S_IFBLK | 0o600:                   os.ModeDevice | 0o600,
		syscall.S_IFREG | syscall.S_ISUID | 0o700: os.ModeSetuid | 0o700,
		0o600: os.ModeIrregular | 0o600,
	}
	for raw, want := range cases {
		if got := fileModeOf(raw); got != want {
			t.Errorf("fileModeOf(%#o) = %v; want %v", raw, got, want)
		}
	}
}

func TestLockWait(t *testing.T) {
	p := plainPolicy(1 << 10)
	root, _ := privateRoot(t, p)
	held, ok, err := p.TryLock(root, "a.lock")
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	if second, ok, err := p.TryLock(root, "a.lock"); err != nil || ok || second != nil {
		t.Fatalf("second TryLock = %v, %v, %v; want a refusal without an error", second, ok, err)
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if l, err := p.WaitLock(short, root, "a.lock"); !errors.Is(err, context.DeadlineExceeded) || l != nil {
		t.Fatalf("WaitLock on a held lock = %v, %v; want the deadline", l, err)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("WaitLock returned after %v; want it to wait for the deadline", waited)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := p.WaitLock(done, root, "a.lock"); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitLock with a cancelled context = %v", err)
	}

	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = held.Close()
	}()
	long, cancelLong := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLong()
	l, err := p.WaitLock(long, root, "a.lock")
	if err != nil {
		t.Fatalf("WaitLock after release = %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	again, ok, err := p.TryLock(root, "a.lock")
	if err != nil || !ok {
		t.Fatalf("TryLock after Close = %v, %v", ok, err)
	}
	_ = again.Close()
	if _, _, err := p.TryLock(root, "../a.lock"); err == nil {
		t.Error("TryLock accepted a path outside the root")
	}
}

func TestNewIDDefaultsAndOverride(t *testing.T) {
	p := plainPolicy(1 << 10)
	a, err := p.newID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.newID()
	if len(a) != 32 || a == b {
		t.Fatalf("default ids = %q, %q; want distinct 32-character hex", a, b)
	}
	p.NewID = func() (string, error) { return "", io.ErrUnexpectedEOF }
	root, dir := privateRoot(t, p)
	if err := p.WriteEnvelope(root, "x.json", 1, sample{}, NoSyncOps()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("WriteEnvelope with a failing id source = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
}

func TestWriteFileAndReadFileRoundTripRawBytes(t *testing.T) {
	p := Policy{Label: "secret", MaxBytes: 32}
	root, dir := privateRoot(t, p)
	want := []byte("0123456789abcdef0123456789abcdef")
	var steps []string
	ops := NoSyncOps()
	ops.Write = func(f *os.File, b []byte) (int, error) { steps = append(steps, "write"); return f.Write(b) }
	ops.SyncFile = func(*os.File) error { steps = append(steps, "syncFile"); return nil }
	ops.Rename = func(r *os.Root, a, b string) error {
		if !strings.HasPrefix(a, TempPrefix) || b != "secret" {
			t.Errorf("rename %q to %q; want a %q file onto secret", a, b, TempPrefix)
		}
		steps = append(steps, "rename")
		return r.Rename(a, b)
	}
	ops.SyncDir = func(*os.Root) error { steps = append(steps, "syncDir"); return nil }
	if err := p.WriteFile(root, "secret", want, ops); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := strings.Join(steps, ","); got != "write,syncFile,rename,syncDir" {
		t.Fatalf("steps = %s; want write, syncFile, rename, syncDir", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "secret"))
	if err != nil || string(raw) != string(want) {
		t.Fatalf("file = %q, %v; want the exact bytes with no envelope", raw, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, TempPrefix+"*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	got, err := p.ReadFile(root, "secret", 32)
	if err != nil || string(got) != string(want) {
		t.Fatalf("ReadFile = %q, %v; want %q", got, err, want)
	}
	// A second write replaces the first.
	if err := p.WriteFile(root, "secret", []byte("short"), NoSyncOps()); err != nil {
		t.Fatalf("replacing WriteFile: %v", err)
	}
	if got, err := p.ReadFile(root, "secret", 32); err != nil || string(got) != "short" {
		t.Fatalf("ReadFile after replace = %q, %v", got, err)
	}
}

func TestWriteFileRefusals(t *testing.T) {
	p := Policy{MaxBytes: 8}
	root, dir := privateRoot(t, p)
	if err := p.WriteFile(root, "big", []byte("123456789"), NoSyncOps()); err == nil || err.Error() != "private file: record exceeds size limit" {
		t.Fatalf("oversized WriteFile = %v; want the size error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized WriteFile left a file: %v", err)
	}
	if err := (Policy{}).WriteFile(root, "x", []byte("a"), NoSyncOps()); err == nil || !strings.Contains(err.Error(), "size limit not set") {
		t.Fatalf("WriteFile with no size limit = %v", err)
	}
	if err := p.WriteFile(root, "../x", []byte("a"), NoSyncOps()); err == nil {
		t.Fatal("WriteFile accepted a name outside the root")
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteFile(root, "open", []byte("b"), NoSyncOps()); !errors.Is(err, ErrUnsafeMode) {
		t.Fatalf("WriteFile over an open file = %v; want ErrUnsafeMode", err)
	}
	if raw, _ := os.ReadFile(open); string(raw) != "a" {
		t.Fatalf("refused WriteFile changed the file to %q", raw)
	}
	boom := errors.New("injected")
	for name, inject := range map[string]func(*Ops){
		"short write": func(o *Ops) { o.Write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:1]) } },
		"file sync":   func(o *Ops) { o.SyncFile = func(*os.File) error { return boom } },
		"rename":      func(o *Ops) { o.Rename = func(*os.Root, string, string) error { return boom } },
	} {
		ops := NoSyncOps()
		inject(&ops)
		if err := p.WriteFile(root, "f", []byte("ab"), ops); err == nil {
			t.Fatalf("%s: WriteFile succeeded", name)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, TempPrefix+"*")); len(left) != 0 {
			t.Fatalf("%s: temporary files left behind: %v", name, left)
		}
	}
}

func TestReadFileRefusals(t *testing.T) {
	p := Policy{Label: "secret", MaxBytes: 64}
	root, dir := privateRoot(t, p)
	if err := p.WriteFile(root, "s", []byte("0123456789"), NoSyncOps()); err != nil {
		t.Fatal(err)
	}
	if got, err := p.ReadFile(root, "s", 10); err != nil || string(got) != "0123456789" {
		t.Fatalf("ReadFile at exactly the limit = %q, %v", got, err)
	}
	if _, err := p.ReadFile(root, "s", 9); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "oversized file") {
		t.Fatalf("ReadFile over the limit = %v; want ErrCorrupt oversized file", err)
	}
	if _, err := p.ReadFile(root, "s", 0); err == nil || !strings.Contains(err.Error(), "size limit not set") {
		t.Fatalf("ReadFile with no limit = %v", err)
	}
	if _, err := p.ReadFile(root, "absent", 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile of an absent file = %v; want not exist", err)
	}
	if err := os.Chmod(filepath.Join(dir, "s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadFile(root, "s", 10); !errors.Is(err, ErrUnsafeMode) {
		t.Fatalf("ReadFile of an open file = %v; want ErrUnsafeMode", err)
	}
}

func TestPrivateInfoJoinsAPlainCauseWithTheBase(t *testing.T) {
	own := uint32(os.Geteuid())
	plain := errors.New("custom words")
	p := Policy{Ownership: plain, Mode: plain, FileType: plain}
	cases := []struct {
		name     string
		info     stubInfo
		base     error
		contains string
	}{
		{"ownership", stubInfo{0o600, &syscall.Stat_t{Uid: own + 1, Nlink: 1}}, ErrUnsafeOwnership, "is owned by uid"},
		{"mode", stubInfo{0o640, &syscall.Stat_t{Uid: own, Nlink: 1}}, ErrUnsafeMode, "has mode 0640"},
		{"file type", stubInfo{os.ModeDir | 0o700, &syscall.Stat_t{Uid: own, Nlink: 1}}, ErrUnsafeFileType, "is not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.PrivateInfo("/p", tc.info, false)
			var unsafe *UnsafePathError
			if !errors.As(err, &unsafe) || !errors.Is(err, plain) || !errors.Is(err, tc.base) {
				t.Fatalf("PrivateInfo = %v; want the plain cause and %v both matching", err, tc.base)
			}
			if !strings.HasPrefix(err.Error(), "custom words: /p ") {
				t.Fatalf("PrivateInfo = %q; want the policy's own wording", err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("PrivateInfo = %q; want %q", err, tc.contains)
			}
		})
	}
	// A cause that already matches the base is used as it is.
	own1 := Sentinel("wording", ErrUnsafeMode)
	var rewrapped *joined
	if errors.As(matching(own1, ErrUnsafeMode), &rewrapped) {
		t.Fatal("matching rewrapped a cause that already matches")
	}
}

func TestReadRetriesAReplacedFileThenReturnsChanged(t *testing.T) {
	changedErr := errors.New("replaced")
	for name, tc := range map[string]struct {
		policy  Policy
		changed error
	}{
		"default cause": {plainPolicy(1 << 10), ErrChanged},
		"policy cause":  {Policy{MaxBytes: 1 << 10, IntegrityKey: "sha256", Integrity: SHA256Hex, Changed: changedErr}, changedErr},
	} {
		t.Run(name, func(t *testing.T) {
			p := tc.policy
			root, dir := privateRoot(t, p)
			if err := p.WriteEnvelope(root, "r.json", 1, sample{Name: "a"}, NoSyncOps()); err != nil {
				t.Fatal(err)
			}
			valid, err := os.ReadFile(filepath.Join(dir, "r.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Each open finds the name replaced by a new inode after the lstat.
			opens, replaceFor := 0, 0
			prev := openat
			t.Cleanup(func() { openat = prev })
			openat = func(fd int, name string, flags int) (int, error) {
				if name == "r.json" {
					opens++
					if opens <= replaceFor {
						if err := root.WriteFile("swap", valid, 0o600); err != nil {
							return -1, err
						}
						if err := root.Rename("swap", name); err != nil {
							return -1, err
						}
					}
				}
				return prev(fd, name, flags)
			}

			replaceFor = 1 << 20
			_, err = p.ReadEnvelope(root, "r.json")
			if !errors.Is(err, tc.changed) || !strings.Contains(err.Error(), "private file changed during open") {
				t.Fatalf("ReadEnvelope = %v; want the unchanged Changed error", err)
			}
			if opens != ReadRetries+1 {
				t.Fatalf("opens = %d; want the first try plus %d retries", opens, ReadRetries)
			}

			// A reader that loses the race once reads the new file.
			opens, replaceFor = 0, 1
			env, err := p.ReadEnvelope(root, "r.json")
			if err != nil || env.Version != 1 || opens != 2 {
				t.Fatalf("ReadEnvelope after one loss = %+v, %v after %d opens; want success on the second", env, err, opens)
			}
		})
	}
}

func TestEUIDHookSetsTheRequiredOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	own := os.Geteuid()
	custom := errors.New("custom ownership cause")
	cases := []struct {
		name   string
		euid   func() int
		want   error
		wantID int
		custom bool
	}{
		{"nil hook uses the process's user", nil, nil, own, false},
		{"hook naming the file's owner", func() int { return own }, nil, own, false},
		{"hook naming another user", func() int { return own + 1 }, ErrUnsafeOwnership, own + 1, false},
		{"hook naming another user with a custom cause", func() int { return own + 1 }, custom, own + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Policy{EUID: tc.euid}
			if tc.custom {
				p.Ownership = custom
			}
			err := p.PrivateInfo(path, info, false)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("PrivateInfo = %v; want accepted", err)
				}
				return
			}
			var unsafe *UnsafePathError
			if !errors.Is(err, tc.want) || !errors.As(err, &unsafe) {
				t.Fatalf("PrivateInfo = %v; want %v", err, tc.want)
			}
			if unsafe.EUID != tc.wantID || unsafe.UID != own {
				t.Errorf("EUID/UID = %d/%d; want %d/%d", unsafe.EUID, unsafe.UID, tc.wantID, own)
			}
		})
	}
}

func TestEUIDHookIsNeverSetByProductionCode(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		if setsEUIDHook(file) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("non-test sources set the Policy.EUID test hook: %v", offenders)
	}
}

// setsEUIDHook reports whether the file names an EUID field in a composite
// literal other than an UnsafePathError, which reports an EUID and has no
// hook, or assigns to a selector named EUID.
func setsEUIDHook(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if literalTypeName(node.Type) != "UnsafePathError" && literalNamesEUID(node) {
				found = true
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "EUID" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// literalNamesEUID reports whether a composite literal keys an element EUID.
func literalNamesEUID(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "EUID" {
				return true
			}
		}
	}
	return false
}

// literalTypeName is the name of a composite literal's type without its
// package qualifier, or empty for an anonymous or elided type.
func literalTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	}
	return ""
}
