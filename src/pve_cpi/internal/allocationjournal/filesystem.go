package allocationjournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
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

// fileSyncOff is the switch behind SetFileSyncForTest. The zero value keeps
// every sync on. The default ops read it on each call, so it also governs a
// journal that was opened before the switch changed.
var fileSyncOff atomic.Bool

// syncFileHook and syncDirHook are the syncs the default ops perform. They
// are variables only so a test in this package can observe the calls.
var (
	syncFileHook = func(f *os.File) error { return f.Sync() }
	syncDirHook  = syncDirectory
)

// SetFileSyncForTest turns the default ops' file and directory syncs on or
// off for a test binary and returns a restore function. Off makes both syncs
// no-ops, and the write, rename, and error handling are unchanged. It exists
// so packages whose tests write many journal records need not wait on disk
// flushes, and production code never calls it.
//
//	defer allocationjournal.SetFileSyncForTest(false)()
func SetFileSyncForTest(enabled bool) func() {
	prev := fileSyncOff.Swap(!enabled)
	return func() { fileSyncOff.Store(prev) }
}

func defaultFileOps() fileOps {
	return fileOps{
		write: func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		syncFile: func(f *os.File) error {
			if fileSyncOff.Load() {
				return nil
			}
			return syncFileHook(f)
		},
		rename: func(r *os.Root, a, b string) error { return r.Rename(a, b) },
		syncDir: func(r *os.Root) error {
			if fileSyncOff.Load() {
				return nil
			}
			return syncDirHook(r)
		},
	}
}
func syncDirectory(r *os.Root) error { return privatefile.SyncDirectory(r) }
func pathKey(value string) string    { s := sha256.Sum256([]byte(value)); return hex.EncodeToString(s[:]) }
func validateNamespace(namespace string) error {
	if !nonblank(namespace) || namespace != strings.TrimSpace(namespace) || len(namespace) > 1024 || strings.ContainsAny(namespace, "/\\\x00") || namespace == "." || namespace == ".." {
		return fmt.Errorf("journal: invalid namespace")
	}
	return nil
}

// Unsafe path causes. An UnsafePathError wraps exactly one of them, so a
// caller can tell an ownership mismatch, which a rerun as the owner fixes,
// from a mode or file type that only the operator can repair. Each carries
// the journal's wording and still matches the shared package's cause.
var (
	ErrUnsafeOwnership = privatefile.Sentinel("journal: unsafe ownership", privatefile.ErrUnsafeOwnership)
	ErrUnsafeMode      = privatefile.Sentinel("journal: unsafe permissions", privatefile.ErrUnsafeMode)
	ErrUnsafeFileType  = privatefile.Sentinel("journal: unsafe file type", privatefile.ErrUnsafeFileType)
)

// UnsafePathError names a journal path that privateInfo refused. UID is the
// path's owner, or -1 when the platform reported no ownership metadata, and
// EUID is the effective UID the check compared it against. WantDirectory
// says which file type the check expected. Its text carries only the path,
// the two UIDs, and the mode, so the CLI may print it.
type UnsafePathError = privatefile.UnsafePathError

// privateFiles is the journal's private-file policy: its own wording and
// error classes, its record size cap, its plain SHA-256 checksum, and a
// temporary name drawn from the allocation UUID source.
var privateFiles = privatefile.Policy{
	Label:        "journal",
	Ownership:    ErrUnsafeOwnership,
	Mode:         ErrUnsafeMode,
	FileType:     ErrUnsafeFileType,
	Changed:      ErrReconciliationRequired,
	Corrupt:      ErrCorrupt,
	MaxBytes:     maxRecordBytes,
	IntegrityKey: "sha256",
	Integrity:    privatefile.SHA256Hex,
	NewID:        NewAllocationID,
}

func privateInfo(path string, info os.FileInfo, directory bool) error {
	return privateFiles.PrivateInfo(path, info, directory)
}
func openPrivateRoot(path string) (*os.Root, error) { return privateFiles.OpenRoot(path) }
func openPrivate(r *os.Root, name string, flags int) (*os.File, error) {
	return privateFiles.Open(r, name, flags)
}
func openPrivateWith(r *os.Root, name string, flags int, open func(int, string, int) (int, error)) (*os.File, error) {
	return privateFiles.OpenWith(r, name, flags, open)
}

func readJSON(r *os.Root, name string, out any) error {
	env, err := readEnvelope(r, name)
	if err != nil {
		return err
	}
	return decodeEnvelope(env, Version, out)
}

// readEnvelope opens name under the private-file rules, reads it within the
// record size limit, and decodes the outer envelope without judging its
// version, so a caller whose file versions on its own schedule can tell a
// newer file from a damaged one before it decides what to do.
func readEnvelope(r *os.Root, name string) (diskEnvelope, error) {
	env, err := privateFiles.ReadEnvelope(r, name)
	if err != nil {
		return diskEnvelope{}, err
	}
	return diskEnvelope{Version: env.Version, SHA256: env.Integrity, Payload: env.Payload}, nil
}

// decodeEnvelope checks env against the expected version and its checksum and
// strictly decodes the payload into out.
func decodeEnvelope(env diskEnvelope, version int, out any) error {
	return privateFiles.DecodeEnvelope(privatefile.Envelope{Version: env.Version, Integrity: env.SHA256, Payload: env.Payload}, version, out)
}

func atomicJSON(r *os.Root, name string, value any, ops fileOps) error {
	return atomicEnvelope(r, name, Version, value, ops)
}

// atomicEnvelope is atomicJSON with the envelope version chosen by the caller.
// Journal records, the index, and the authority always use Version; the entry
// binding sidecar uses its own schema version. Any failure may follow a
// committed rename, so it comes back as a DurabilityError.
func atomicEnvelope(r *os.Root, name string, version int, value any, ops fileOps) error {
	err := privateFiles.WriteEnvelope(r, name, version, value, privatefile.Ops{
		Write: ops.write, SyncFile: ops.syncFile, Rename: ops.rename, SyncDir: ops.syncDir,
	})
	if err != nil {
		return &DurabilityError{Err: err}
	}
	return nil
}

type fileLock struct{ file *os.File }

func tryLock(r *os.Root, name string) (*fileLock, bool, error) {
	l, ok, err := privateFiles.TryLock(r, name)
	if !ok || err != nil {
		return nil, false, err
	}
	return &fileLock{file: l.File}, true, nil
}
func (l *fileLock) close() error { return (&privatefile.Lock{File: l.file}).Close() }
func waitLock(ctx context.Context, r *os.Root, name string) (*fileLock, error) {
	l, err := privateFiles.WaitLock(ctx, r, name)
	if err != nil {
		return nil, err
	}
	return &fileLock{file: l.File}, nil
}
func pause(ctx context.Context) error { return privatefile.Pause(ctx) }
