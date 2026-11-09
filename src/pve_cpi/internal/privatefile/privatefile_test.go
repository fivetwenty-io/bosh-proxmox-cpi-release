package privatefile

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
	if err := p.WriteEnvelope(root, "entry.json", 7, want, DefaultOps(false)); err != nil {
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
	if err := p.WriteEnvelope(root, "entry.json", 1, sample{Name: "a"}, DefaultOps(false)); err != nil {
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
	if err := small.WriteEnvelope(root, "ok.json", 1, sample{Name: "x"}, DefaultOps(false)); err != nil {
		t.Fatalf("a record under the limit: %v", err)
	}
	big := sample{Name: strings.Repeat("n", 200)}
	err := small.WriteEnvelope(root, "big.json", 1, big, DefaultOps(false))
	if err == nil || err.Error() != "private file: record exceeds size limit" {
		t.Fatalf("oversized write = %v; want the size error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "big.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized write left a file: %v", statErr)
	}
	// The same record fits under a larger limit, and the smaller limit then
	// refuses to read it.
	large := plainPolicy(1 << 12)
	if err := large.WriteEnvelope(root, "big.json", 1, big, DefaultOps(false)); err != nil {
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
			if err := p.WriteEnvelope(root, "x.json", 1, 1, DefaultOps(false)); err == nil {
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
	if err := DefaultOps(true).SyncFile(closed); err == nil {
		t.Error("a durable file sync on a closed file reported success, so it did not reach the file")
	}
	if err := DefaultOps(true).SyncDir(closedRoot); err == nil {
		t.Error("a durable directory sync on a closed root reported success, so it did not reach the directory")
	}
	if err := DefaultOps(false).SyncFile(closed); err != nil {
		t.Errorf("a non-durable file sync = %v; want a no-op", err)
	}
	if err := DefaultOps(false).SyncDir(closedRoot); err != nil {
		t.Errorf("a non-durable directory sync = %v; want a no-op", err)
	}

	p := plainPolicy(1 << 10)
	for _, durable := range []bool{true, false} {
		ops := DefaultOps(durable)
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
			ops := DefaultOps(false)
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
	dirSyncErr := DefaultOps(false)
	dirSyncErr.SyncDir = func(*os.Root) error { return boom }
	if err := p.WriteEnvelope(root, "f.json", 1, sample{}, dirSyncErr); !errors.Is(err, boom) {
		t.Fatalf("directory sync failure = %v; want the injected error", err)
	}
	if err := p.WriteEnvelope(root, "g.json", 1, make(chan int), DefaultOps(false)); err == nil {
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
	if err := p.WriteEnvelope(root, "r.json", 1, sample{Name: "a"}, DefaultOps(false)); err != nil {
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
	if err := p.WriteEnvelope(root, "x.json", 1, sample{}, DefaultOps(false)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("WriteEnvelope with a failing id source = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("files left behind: %v", left)
	}
}
