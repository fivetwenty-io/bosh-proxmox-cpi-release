package endpointcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// envelopeOnDisk is the cache file as JSON, for tests that edit it by hand.
type envelopeOnDisk struct {
	Version int             `json:"version"`
	HMAC    string          `json:"hmac_sha256"`
	Payload json.RawMessage `json:"payload"`
}

func readEnvelopeFile(t *testing.T, path string) envelopeOnDisk {
	t.Helper()
	var env envelopeOnDisk
	if err := json.Unmarshal(readStoreFile(t, path), &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func writeEnvelopeFile(t *testing.T, path string, env any) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, path, data, 0o600)
}

// signedFile writes payload to the store's file with a valid HMAC, the way a
// writer holding the credential would.
func signedFile(t *testing.T, s *Store, payload any) {
	t.Helper()
	if err := s.policy.WriteEnvelope(s.root, s.name, fileVersion, payload, s.ops); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	if !regexp.MustCompile(`^pve1\.dc1\.example\.com-[0-9a-f]{16}\.json$`).MatchString(s.FileName()) {
		t.Errorf("file name %q has the wrong shape", s.FileName())
	}
	var missing storeTestPayload
	if err := s.Read(&missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read before any write: %v, want fs.ErrNotExist", err)
	}
	storePut(t, s, "first")

	var got storeTestPayload
	if err := s.Read(&got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Note != "first" || !got.Key.Equal(s.Key()) {
		t.Errorf("Read returned %+v", got)
	}
	path := filepath.Join(dir, s.FileName())
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 {
		t.Errorf("cache file mode %v, want 0600", info.Mode())
	}
	env := readEnvelopeFile(t, path)
	if env.Version != 1 || len(env.HMAC) != 64 {
		t.Errorf("envelope version %d with a %d-character HMAC", env.Version, len(env.HMAC))
	}
	if !bytes.HasPrefix(readStoreFile(t, path), []byte(`{"version":1,"hmac_sha256":"`)) {
		t.Errorf("envelope fields are not version, hmac_sha256, payload: %s", readStoreFile(t, path))
	}

	// A second store on the same directory and credential reads the file.
	other, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var again storeTestPayload
	if err := other.Read(&again); err != nil || again.Note != "first" {
		t.Fatalf("second store Read: %+v, %v", again, err)
	}
	want := []string{secretFile, secretLockFile, s.FileName(), s.Key().LockName()}
	slices.Sort(want)
	if got := storeDirNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("directory holds %v, want %v and no temporary file", got, want)
	}
}

func TestStoreRefusesATamperedFile(t *testing.T) {
	cases := map[string]func(env *envelopeOnDisk){
		"payload changed": func(env *envelopeOnDisk) {
			env.Payload = json.RawMessage(strings.Replace(string(env.Payload), `"note":"signed"`, `"note":"forged"`, 1))
		},
		"hmac changed": func(env *envelopeOnDisk) {
			b := []byte(env.HMAC)
			b[0] ^= 1
			env.HMAC = string(b)
		},
		"hmac empty": func(env *envelopeOnDisk) { env.HMAC = "" },
		"plain sha256 in place of the hmac": func(env *envelopeOnDisk) {
			env.HMAC = privatefile.SHA256Hex(env.Payload)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			dir := newStoreDir(t)
			s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			storePut(t, s, "signed")
			path := filepath.Join(dir, s.FileName())
			env := readEnvelopeFile(t, path)
			tamper(&env)
			writeEnvelopeFile(t, path, env)
			var got storeTestPayload
			if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) {
				t.Fatalf("Read: %v, want ErrFileInvalid", err)
			}
			if got.Note != "" {
				t.Errorf("Read filled the payload from a refused file: %+v", got)
			}
		})
	}
}

// TestStoreRefusesAFileSignedWithARotatedCredential shows that a new
// credential uses a different file, and that the old file moved under the new
// name fails its HMAC.
func TestStoreRefusesAFileSignedWithARotatedCredential(t *testing.T) {
	dir := newStoreDir(t)
	old := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, old, "old credential")
	rotated := openTestStore(t, dir, storeTestCredential+"-rotated", StoreOptions{})
	if rotated.FileName() == old.FileName() {
		t.Fatal("a rotated credential uses the same file name")
	}
	var got storeTestPayload
	if err := rotated.Read(&got); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read with the rotated credential: %v, want fs.ErrNotExist", err)
	}
	// Planted under the new name, with the payload's key rewritten too, the
	// old file still fails, because only the old credential can sign it.
	env := readEnvelopeFile(t, filepath.Join(dir, old.FileName()))
	var payload storeTestPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Key = rotated.Key()
	env.Payload, _ = json.Marshal(payload)
	writeEnvelopeFile(t, filepath.Join(dir, rotated.FileName()), env)
	if err := rotated.Read(&got); !errors.Is(err, ErrFileInvalid) || errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Read of a file signed with the old credential: %v, want the HMAC refusal", err)
	}
}

func TestStoreRefusesUnknownFields(t *testing.T) {
	type extended struct {
		Key   Key    `json:"key"`
		Note  string `json:"note"`
		Extra string `json:"extra"`
	}
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	signedFile(t, s, extended{Key: s.Key(), Note: "x", Extra: "planted"})
	var got storeTestPayload
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Read with an unknown payload field: %v, want ErrFileInvalid for the unknown field", err)
	}

	storePut(t, s, "clean")
	path := filepath.Join(dir, s.FileName())
	env := readEnvelopeFile(t, path)
	writeEnvelopeFile(t, path, map[string]any{
		"version": env.Version, "hmac_sha256": env.HMAC, "payload": env.Payload, "comment": "planted",
	})
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read with an unknown envelope field: %v, want ErrFileInvalid", err)
	}
	writeEnvelopeFile(t, path, map[string]any{"version": env.Version, "sha256": env.HMAC, "payload": env.Payload})
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read with the journal's integrity field: %v, want ErrFileInvalid", err)
	}
	writeStoreFile(t, path, append(readStoreFile(t, path), []byte(` {}`)...), 0o600)
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read with trailing data: %v, want ErrFileInvalid", err)
	}
}

// TestStoreRefusesAWrongKey covers a file the credential signed for another
// key, such as another endpoint list that shares the file name, and a payload
// with no key at all.
func TestStoreRefusesAWrongKey(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	wrong := s.Key()
	wrong.Endpoints = []string{"pve9.elsewhere.example.com:8006"}
	signedFile(t, s, storeTestPayload{Key: wrong, Note: "other cluster"})
	var got storeTestPayload
	err := s.Read(&got)
	if !errors.Is(err, ErrKeyMismatch) || !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read of another key's file: %v, want ErrKeyMismatch", err)
	}
	if got.Note != "" {
		t.Errorf("Read filled the payload from another key's file: %+v", got)
	}
	signedFile(t, s, map[string]string{"note": "no key"})
	if err := s.Read(&got); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Read of a payload without a key: %v, want ErrKeyMismatch", err)
	}
	signedFile(t, s, []string{"not", "an", "object"})
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read of a payload that is not an object: %v, want ErrFileInvalid", err)
	}

	// Update treats such a file as missing and writes its own.
	var seen *storeTestPayload
	written, err := Update(context.Background(), s, func(current *storeTestPayload) (*storeTestPayload, error) {
		seen = current
		return &storeTestPayload{Key: s.Key(), Note: "own"}, nil
	})
	if err != nil || !written || seen != nil {
		t.Fatalf("Update over another key's file: written %v, err %v, current %+v", written, err, seen)
	}
	// A payload for another key is never written.
	before := readStoreFile(t, filepath.Join(dir, s.FileName()))
	written, err = Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: wrong, Note: "misdirected"}, nil
	})
	if !errors.Is(err, ErrKeyMismatch) || written {
		t.Fatalf("Update with another key: written %v, err %v, want ErrKeyMismatch", written, err)
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, s.FileName())), before) {
		t.Error("a payload for another key replaced the file")
	}
}

func TestStoreRefusesAnUnsupportedVersion(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	if err := s.policy.WriteEnvelope(s.root, s.name, fileVersion+1, storeTestPayload{Key: s.Key()}, s.ops); err != nil {
		t.Fatal(err)
	}
	var got storeTestPayload
	if err := s.Read(&got); !errors.Is(err, ErrUnsupportedVersion) || !errors.Is(err, ErrFileInvalid) {
		t.Fatalf("Read of a newer version: %v, want ErrUnsupportedVersion", err)
	}
}

// TestStoreRefusesUnsafeFiles covers a cache file whose mode, type, or link
// count is wrong. Read refuses it, and Update refuses to read or replace it.
func TestStoreRefusesUnsafeFiles(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, dir, name string)
		want  error
		text  string
	}{
		"group readable": {
			setup: func(t *testing.T, dir, name string) {
				if err := os.Chmod(filepath.Join(dir, name), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeMode,
		},
		"symlink to a signed file": {
			setup: func(t *testing.T, dir, name string) {
				target := filepath.Join(t.TempDir(), "signed.json")
				writeStoreFile(t, target, readStoreFile(t, filepath.Join(dir, name)), 0o600)
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeFileType,
		},
		"directory": {
			setup: func(t *testing.T, dir, name string) {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeFileType,
		},
		"second link": {
			setup: func(t *testing.T, dir, name string) {
				if err := os.Link(filepath.Join(dir, name), filepath.Join(dir, "planted")); err != nil {
					t.Fatal(err)
				}
			},
			text: "multiply linked",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := newStoreDir(t)
			s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			storePut(t, s, "signed")
			tc.setup(t, dir, s.FileName())
			before, err := os.Lstat(filepath.Join(dir, s.FileName()))
			if err != nil {
				t.Fatal(err)
			}
			check := func(what string, err error) {
				t.Helper()
				switch {
				case err == nil:
					t.Fatalf("%s accepted an unsafe file", what)
				case tc.want != nil && !errors.Is(err, tc.want):
					t.Fatalf("%s: %v, want %v", what, err, tc.want)
				case tc.text != "" && !strings.Contains(err.Error(), tc.text):
					t.Fatalf("%s: %v, want %q", what, err, tc.text)
				case errors.Is(err, ErrFileInvalid):
					t.Fatalf("%s: %v is classed as an invalid file, which a writer would replace", what, err)
				}
			}
			var got storeTestPayload
			check("Read", s.Read(&got))
			called := false
			written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
				called = true
				return &storeTestPayload{Key: s.Key()}, nil
			})
			check("Update", err)
			if written || called {
				t.Errorf("Update wrote %v and called merge %v for an unsafe file", written, called)
			}
			after, err := os.Lstat(filepath.Join(dir, s.FileName()))
			if err != nil || !os.SameFile(before, after) {
				t.Errorf("the unsafe file was replaced: %v", err)
			}
		})
	}
}

// TestStoreRefusesAnUnsafeLockFile shows that a key lock file that fails the
// private checks stops the write with that refusal rather than a skip.
func TestStoreRefusesAnUnsafeLockFile(t *testing.T) {
	dir := newStoreDir(t)
	logger, observed := log.NewObservedLogger(log.LevelDebug)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{Logger: logger})
	writeStoreFile(t, filepath.Join(dir, s.Key().LockName()), nil, 0o640)
	written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: s.Key()}, nil
	})
	if !errors.Is(err, privatefile.ErrUnsafeMode) || written {
		t.Fatalf("Update with an open lock file: written %v, err %v, want the mode refusal", written, err)
	}
	if observed.Len() != 0 {
		t.Errorf("an unsafe lock file logged the skip line: %+v", observed.All())
	}
}

// TestStoreRefusesAnotherOwner needs root to give a file another owner.
// Without root it shows the refusal on a directory another user owns.
func TestStoreRefusesAnotherOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		_, err := OpenStore(context.Background(), "/", storeTestConfig(), storeTestCredential, StoreOptions{})
		if !errors.Is(err, privatefile.ErrUnsafeOwnership) {
			t.Fatalf("OpenStore on a directory another user owns: %v, want the ownership refusal", err)
		}
		return
	}
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, s, "signed")
	if err := os.Lchown(filepath.Join(dir, s.FileName()), 1, 1); err != nil {
		t.Fatal(err)
	}
	var got storeTestPayload
	if err := s.Read(&got); !errors.Is(err, privatefile.ErrUnsafeOwnership) {
		t.Fatalf("Read: %v, want the ownership refusal", err)
	}
	if _, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: s.Key()}, nil
	}); !errors.Is(err, privatefile.ErrUnsafeOwnership) {
		t.Fatalf("Update: %v, want the ownership refusal", err)
	}
}

func TestStoreRefusesAnOversizedFile(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	big := strings.Repeat("x", maxFileBytes)
	written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: s.Key(), Note: big}, nil
	})
	if err == nil || written {
		t.Fatalf("Update with an oversized payload: written %v, err %v", written, err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, s.FileName())); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("an oversized payload left a file: %v", statErr)
	}

	writeStoreFile(t, filepath.Join(dir, s.FileName()), bytes.Repeat([]byte(" "), maxFileBytes+1), 0o600)
	var got storeTestPayload
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("Read of an oversized file: %v, want ErrFileInvalid", err)
	}
	storePut(t, s, "rebuilt")
	if err := s.Read(&got); err != nil || got.Note != "rebuilt" {
		t.Fatalf("Read after the rebuild: %+v, %v", got, err)
	}
}

// TestStoreSkipsTheWriteWhileAnotherProcessHoldsTheLock holds the key's lock
// in a child process. The write is skipped with a Debug line under auto and a
// Warn line under an explicit "on", and goes through once the lock is free.
func TestStoreSkipsTheWriteWhileAnotherProcessHoldsTheLock(t *testing.T) {
	for _, tc := range []struct {
		explicitOn bool
		level      string
	}{{false, "DEBUG"}, {true, "WARN"}} {
		t.Run(tc.level, func(t *testing.T) {
			dir := newStoreDir(t)
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			s := openTestStore(t, dir, storeTestCredential, StoreOptions{Logger: logger, ExplicitOn: tc.explicitOn})
			s.lockWait = 150 * time.Millisecond
			holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+s.Key().LockName())
			if got := holder.line(t); got != "locked" {
				t.Fatalf("child said %q, want locked", got)
			}
			called := false
			start := time.Now()
			written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
				called = true
				return &storeTestPayload{Key: s.Key()}, nil
			})
			if err != nil || written || called {
				t.Fatalf("Update under a held lock: written %v, err %v, merge called %v", written, err, called)
			}
			if waited := time.Since(start); waited < s.lockWait {
				t.Errorf("Update gave up after %v, before its %v wait", waited, s.lockWait)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, s.FileName())); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a skipped write left a file: %v", statErr)
			}
			entries := observed.All()
			if len(entries) != 1 {
				t.Fatalf("logged %d lines, want 1: %+v", len(entries), entries)
			}
			entry := entries[0]
			if entry.Level.String() != tc.level || !strings.Contains(entry.Message, "skipping the cache write") ||
				entry.Attrs["file"] != s.FileName() {
				t.Errorf("logged %+v, want a %s skip line naming the file", entry, tc.level)
			}

			holder.finish(t)
			storePut(t, s, "after release")
			if len(observed.All()) != 1 {
				t.Errorf("a successful write logged: %+v", observed.All())
			}
		})
	}
}

func TestStoreUpdateReportsAnEndedContext(t *testing.T) {
	dir := newStoreDir(t)
	logger, observed := log.NewObservedLogger(log.LevelDebug)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{Logger: logger})
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+s.Key().LockName())
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	written, err := Update(ctx, s, func(*storeTestPayload) (*storeTestPayload, error) { return &storeTestPayload{Key: s.Key()}, nil })
	if !errors.Is(err, ErrLockWaitAbandoned) || !errors.Is(err, context.DeadlineExceeded) || written {
		t.Fatalf("Update with an ended context: written %v, err %v, want ErrLockWaitAbandoned", written, err)
	}
	if observed.Len() != 0 {
		t.Errorf("an ended context logged the skip line: %+v", observed.All())
	}
	holder.finish(t)
}

func TestStoreUpdateMergeContract(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	var seen []*storeTestPayload
	merge := func(next *storeTestPayload, err error) func(*storeTestPayload) (*storeTestPayload, error) {
		return func(current *storeTestPayload) (*storeTestPayload, error) {
			seen = append(seen, current)
			return next, err
		}
	}
	if written, err := Update(context.Background(), s, merge(nil, nil)); err != nil || written {
		t.Fatalf("merge returning nil: written %v, err %v", written, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, s.FileName())); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("merge returning nil left a file: %v", err)
	}
	if written, err := Update(context.Background(), s, merge(&storeTestPayload{Key: s.Key(), Note: "one"}, nil)); err != nil || !written {
		t.Fatalf("first write: written %v, err %v", written, err)
	}
	failure := errors.New("merge failed")
	if written, err := Update(context.Background(), s, merge(&storeTestPayload{Key: s.Key(), Note: "two"}, failure)); !errors.Is(err, failure) || written {
		t.Fatalf("failing merge: written %v, err %v", written, err)
	}
	if len(seen) != 3 || seen[0] != nil || seen[1] != nil || seen[2] == nil || seen[2].Note != "one" {
		t.Fatalf("merge saw %+v, want nil, nil, then the stored payload", seen)
	}
	var got storeTestPayload
	if err := s.Read(&got); err != nil || got.Note != "one" {
		t.Fatalf("a failing merge changed the file: %+v, %v", got, err)
	}

	path := filepath.Join(dir, s.FileName())
	writeStoreFile(t, path, []byte("{torn"), 0o600)
	if written, err := Update(context.Background(), s, merge(&storeTestPayload{Key: s.Key(), Note: "three"}, nil)); err != nil || !written {
		t.Fatalf("write over a torn file: written %v, err %v", written, err)
	}
	if seen[3] != nil {
		t.Errorf("merge received %+v for a torn file, want nil", seen[3])
	}

	if _, err := Update[storeTestPayload](context.Background(), s, nil); err == nil {
		t.Error("Update accepted a nil merge")
	}
	if err := s.Read(got); err == nil {
		t.Error("Read accepted a non-pointer")
	}
	if err := s.Read(nil); err == nil {
		t.Error("Read accepted nil")
	}
	var nilPayload *storeTestPayload
	if err := s.Read(nilPayload); err == nil {
		t.Error("Read accepted a nil pointer")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := s.Read(&got); err == nil {
		t.Error("Read on a closed store succeeded")
	}
	if _, err := Update(context.Background(), s, merge(nil, nil)); err == nil {
		t.Error("Update on a closed store succeeded")
	}
	var none *Store
	if err := none.Close(); err != nil {
		t.Errorf("Close on a nil store: %v", err)
	}
}

// TestFileNameDiffersAcrossDirectories shows that one credential and one
// endpoint configuration name different files in two cache directories,
// because each directory has its own secret.
func TestFileNameDiffersAcrossDirectories(t *testing.T) {
	a := openTestStore(t, newStoreDir(t), storeTestCredential, StoreOptions{})
	b := openTestStore(t, newStoreDir(t), storeTestCredential, StoreOptions{})
	if a.FileName() == b.FileName() || a.Key().CredentialID == b.Key().CredentialID {
		t.Fatalf("two directories share the file name %s", a.FileName())
	}
	if !strings.HasPrefix(a.FileName(), "pve1.dc1.example.com-") || !strings.HasPrefix(b.FileName(), "pve1.dc1.example.com-") {
		t.Errorf("file names %s and %s lost the host", a.FileName(), b.FileName())
	}
}

func TestOpenStoreRefusals(t *testing.T) {
	t.Run("relative directory", func(t *testing.T) {
		if _, err := OpenStore(context.Background(), "cache", storeTestConfig(), storeTestCredential, StoreOptions{}); err == nil {
			t.Fatal("OpenStore accepted a relative directory")
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		if _, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{}); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("OpenStore: %v, want fs.ErrNotExist", err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Error("OpenStore created the directory")
		}
	})
	t.Run("open directory", func(t *testing.T) {
		dir := newStoreDir(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{}); !errors.Is(err, privatefile.ErrUnsafeMode) {
			t.Fatalf("OpenStore: %v, want the mode refusal", err)
		}
	})
	t.Run("symlinked directory", func(t *testing.T) {
		dir := newStoreDir(t)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStore(context.Background(), link, storeTestConfig(), storeTestCredential, StoreOptions{}); err == nil {
			t.Fatal("OpenStore accepted a symlinked directory")
		}
	})
	for name, tc := range map[string]struct {
		cfg        KeyConfig
		credential string
	}{
		"no credential":    {storeTestConfig(), ""},
		"no endpoints":     {KeyConfig{}, storeTestCredential},
		"endpoint no port": {KeyConfig{Endpoints: []string{"pve1.example.com"}}, storeTestCredential},
	} {
		t.Run(name, func(t *testing.T) {
			dir := newStoreDir(t)
			if _, err := OpenStore(context.Background(), dir, tc.cfg, tc.credential, StoreOptions{}); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("OpenStore: %v, want ErrInvalidKey", err)
			}
			if got := storeDirNames(t, dir); len(got) != 0 {
				t.Errorf("a refused key created %v", got)
			}
		})
	}
}
