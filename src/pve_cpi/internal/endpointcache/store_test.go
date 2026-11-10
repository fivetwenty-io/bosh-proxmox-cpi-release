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

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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
	other, err := openStoreOn(t, dir, storeTestCredential, StoreOptions{})
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
		_, err := OpenStore(context.Background(), openSystemRoot(t), storeTestConfig(), storeTestCredential, StoreOptions{})
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
// Warn line under a configured "on", and goes through once the lock is free.
func TestStoreSkipsTheWriteWhileAnotherProcessHoldsTheLock(t *testing.T) {
	for _, tc := range []struct {
		mode  config.DiscoveryMode
		level string
	}{{config.DiscoveryModeAuto, "DEBUG"}, {config.DiscoveryModeOn, "WARN"}} {
		t.Run(tc.level, func(t *testing.T) {
			dir := newStoreDir(t)
			logger, observed := log.NewObservedLogger(log.LevelDebug)
			s := openTestStore(t, dir, storeTestCredential, StoreOptions{Logger: logger, ConfiguredMode: tc.mode})
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

// A context that ends while merge runs writes nothing: Update returns the
// context's error, and the file and the directory stay as they were.
func TestStoreUpdateWritesNothingWhenContextEndsDuringMerge(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, s, "old")
	before := storeDirNames(t, dir)
	raw, err := os.ReadFile(filepath.Join(dir, s.FileName()))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	written, err := Update(ctx, s, func(*storeTestPayload) (*storeTestPayload, error) {
		cancel()
		return &storeTestPayload{Key: s.Key(), Note: "late"}, nil
	})
	if written || !errors.Is(err, context.Canceled) {
		t.Fatalf("Update with a context that ended in merge: written %v, err %v, want canceled", written, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, s.FileName()))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("the file changed after the context ended: %v", err)
	}
	if names := storeDirNames(t, dir); !slices.Equal(names, before) {
		t.Fatalf("directory holds %v, want %v", names, before)
	}
	var got storeTestPayload
	if err := s.Read(&got); err != nil || got.Note != "old" {
		t.Fatalf("stored payload = %+v, %v; want the old note", got, err)
	}
}

// lockHeldContext reports itself cancelled once the store's lock is held
// elsewhere, so a test can end a context at the moment Update has just taken
// the lock. The lock wait runs on a child context and never calls Err on this
// one, so the first call is Update's own check after the lock is held.
type lockHeldContext struct {
	context.Context
	t *testing.T
	s *Store
}

func (c lockHeldContext) Err() error {
	lock, free, err := endpointTexts.tryHeldLock(context.Background(), c.s.root, c.s.key.LockName())
	if err != nil {
		c.t.Errorf("probe the lock: %v", err)
		return nil
	}
	if free {
		if err := lock.Close(); err != nil {
			c.t.Errorf("release the probe lock: %v", err)
		}
		return nil
	}
	return context.Canceled
}

// A context that ends while Update holds the lock stops it before it reads
// the file or calls merge, and nothing is written.
func TestStoreUpdateStopsWhenContextEndsWhileLockHeld(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, s, "old")
	raw, err := os.ReadFile(filepath.Join(dir, s.FileName()))
	if err != nil {
		t.Fatal(err)
	}

	called := false
	ctx := lockHeldContext{Context: context.Background(), t: t, s: s}
	written, err := Update(ctx, s, func(*storeTestPayload) (*storeTestPayload, error) {
		called = true
		return &storeTestPayload{Key: s.Key(), Note: "late"}, nil
	})
	if written || called || !errors.Is(err, context.Canceled) {
		t.Fatalf("Update with a context that ended under the lock: written %v, merge called %v, err %v", written, called, err)
	}
	if errors.Is(err, ErrLockWaitAbandoned) {
		t.Fatalf("the lock wait reported the ended context, so the check under the lock never ran: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, s.FileName()))
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatalf("the file changed after the context ended: %v", err)
	}
	// The lock was released, so the next update goes through.
	storePut(t, s, "after")
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
	// requireClosed checks that OpenStore closed a root it refused, since the
	// store owns the root from the call on.
	requireClosed := func(t *testing.T, root *os.Root) {
		t.Helper()
		if _, err := root.Stat("."); err == nil {
			t.Error("OpenStore left a refused root open")
		}
	}
	t.Run("no root", func(t *testing.T) {
		if _, err := OpenStore(context.Background(), nil, storeTestConfig(), storeTestCredential, StoreOptions{}); err == nil {
			t.Fatal("OpenStore accepted a nil root")
		}
	})
	t.Run("open directory", func(t *testing.T) {
		dir := newStoreDir(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = OpenStore(context.Background(), root, storeTestConfig(), storeTestCredential, StoreOptions{})
		if !errors.Is(err, privatefile.ErrUnsafeMode) || !errors.Is(err, errCacheMode) {
			t.Fatalf("OpenStore: %v, want the mode refusal in the cache's wording", err)
		}
		requireClosed(t, root)
		if got := storeDirNames(t, dir); len(got) != 0 {
			t.Errorf("a refused directory gained %v", got)
		}
	})
	t.Run("owner without write access", func(t *testing.T) {
		dir := newStoreDir(t)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		_, err = OpenStore(context.Background(), root, storeTestConfig(), storeTestCredential, StoreOptions{})
		var unsafe *privatefile.UnsafePathError
		if !errors.As(err, &unsafe) || !errors.Is(err, privatefile.ErrUnsafeMode) ||
			!strings.Contains(err.Error(), "owner needs read, write, and search access") {
			t.Fatalf("OpenStore: %v, want the owner access refusal", err)
		}
		requireClosed(t, root)
	})
	for name, tc := range map[string]struct {
		cfg        KeyConfig
		credential string
	}{
		"no credential":    {storeTestConfig(), ""},
		"no user":          {KeyConfig{Endpoints: storeTestConfig().Endpoints}, storeTestCredential},
		"no endpoints":     {KeyConfig{User: storeTestUser}, storeTestCredential},
		"endpoint no port": {KeyConfig{Endpoints: []string{"pve1.example.com"}, User: storeTestUser}, storeTestCredential},
	} {
		t.Run(name, func(t *testing.T) {
			dir := newStoreDir(t)
			root := storeTestRoot(t, dir)
			if _, err := OpenStore(context.Background(), root, tc.cfg, tc.credential, StoreOptions{}); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("OpenStore: %v, want ErrInvalidKey", err)
			}
			requireClosed(t, root)
			if got := storeDirNames(t, dir); len(got) != 0 {
				t.Errorf("a refused key created %v", got)
			}
		})
	}
}

// TestStoreKeepsItsDirectoryAfterARename renames the cache directory after
// OpenStore and puts a new private directory at the old path. The store keeps
// reading and writing the directory it opened.
func TestStoreKeepsItsDirectoryAfterARename(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, s, "before")
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	storePut(t, s, "after")
	if got := storeDirNames(t, dir); len(got) != 0 {
		t.Fatalf("the new directory at the old path gained %v", got)
	}
	var got storeTestPayload
	if err := s.Read(&got); err != nil || got.Note != "after" {
		t.Fatalf("Read after the rename: %+v, %v", got, err)
	}
	reopened := openTestStore(t, moved, storeTestCredential, StoreOptions{})
	if err := reopened.Read(&got); err != nil || got.Note != "after" {
		t.Fatalf("Read in the moved directory: %+v, %v, want the write made after the rename", got, err)
	}
	if path, _ := s.SecretReplaced(); path != "" {
		t.Errorf("SecretReplaced named %q for a secret that was created, not replaced", path)
	}
}

// TestStoreRefusesAFileMovedFromAnotherKey moves a file between two keys that
// share the credential and the directory. The HMAC key depends on the key
// digest, so the moved file fails its HMAC before the key check runs, even
// with its payload's key rewritten.
func TestStoreRefusesAFileMovedFromAnotherKey(t *testing.T) {
	dir := newStoreDir(t)
	a := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	cfg := storeTestConfig()
	cfg.Endpoints = []string{"pve9.elsewhere.example.com:8006"}
	b, err := OpenStore(context.Background(), storeTestRoot(t, dir), cfg, storeTestCredential, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	storePut(t, a, "from a")
	moved := readStoreFile(t, filepath.Join(dir, a.FileName()))
	writeStoreFile(t, filepath.Join(dir, b.FileName()), moved, 0o600)
	var got storeTestPayload
	if err := b.Read(&got); !errors.Is(err, ErrFileInvalid) || errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Read of a moved file: %v, want the HMAC refusal", err)
	}
	env := readEnvelopeFile(t, filepath.Join(dir, b.FileName()))
	var payload storeTestPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Key = b.Key()
	env.Payload, _ = json.Marshal(payload)
	writeEnvelopeFile(t, filepath.Join(dir, b.FileName()), env)
	if err := b.Read(&got); !errors.Is(err, ErrFileInvalid) || errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Read of a moved file with its key rewritten: %v, want the HMAC refusal", err)
	}
}

// TestStoreRefusesAFileSignedForAnotherVersion signs a file with the HMAC key
// for the next schema version and labels it with the current one. The
// version is part of the key's derivation, so the relabeled file fails.
func TestStoreRefusesAFileSignedForAnotherVersion(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	secret := keyMaterial(readStoreFile(t, filepath.Join(dir, secretFile)))
	key, current, err := deriveKeysAt(storeTestConfig(), secret, storeTestCredential, fileVersion)
	if err != nil {
		t.Fatal(err)
	}
	nextKey, next, err := deriveKeysAt(storeTestConfig(), secret, storeTestCredential, fileVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	if !nextKey.Equal(key) || bytes.Equal(next, current) {
		t.Fatal("another version derived the same HMAC key, or a different file key")
	}
	other := s.policy
	other.Integrity = hmacIntegrity(next)
	if err := other.WriteEnvelope(s.root, s.name, fileVersion, storeTestPayload{Key: s.Key(), Note: "v2"}, s.ops); err != nil {
		t.Fatal(err)
	}
	var got storeTestPayload
	if err := s.Read(&got); !errors.Is(err, ErrFileInvalid) || got.Note != "" {
		t.Fatalf("Read of a file signed for another version: %+v, %v, want ErrFileInvalid", got, err)
	}
}

// TestLocksUpdateTheirTimes ages the key's lock file and its discover lock
// file by 40 days, then takes each lock. Taking it brings the times to now, so
// a lock in use never looks abandoned.
func TestLocksUpdateTheirTimes(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	old := time.Now().Add(-40 * 24 * time.Hour)
	age := func(name string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			writeStoreFile(t, path, nil, 0o600)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	requireFresh := func(name string) {
		t.Helper()
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if since := time.Since(info.ModTime()); since > time.Minute {
			t.Errorf("%s was modified %v ago after it was locked, want now", name, since)
		}
	}
	age(s.Key().LockName())
	storePut(t, s, "touch")
	requireFresh(s.Key().LockName())

	age(s.Key().DiscoverLockName())
	lock, held, err := s.TryLockDiscovery(context.Background())
	if err != nil || !held {
		t.Fatalf("TryLockDiscovery: held %v, err %v", held, err)
	}
	requireFresh(s.Key().DiscoverLockName())
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoveryLockIsExclusiveAcrossProcesses holds the discover lock in a
// child process. TryLockDiscovery reports it held elsewhere without waiting,
// and takes it once the child lets go.
func TestDiscoveryLockIsExclusiveAcrossProcesses(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+s.Key().DiscoverLockName())
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	start := time.Now()
	if lock, held, err := s.TryLockDiscovery(context.Background()); err != nil || held || lock != nil {
		t.Fatalf("TryLockDiscovery under a held lock: %v, %v, %v", lock, held, err)
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("TryLockDiscovery waited %v for a held lock", waited)
	}
	holder.finish(t)
	lock, held, err := s.TryLockDiscovery(context.Background())
	if err != nil || !held {
		t.Fatalf("TryLockDiscovery after release: held %v, err %v", held, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, filepath.Join(dir, s.Key().DiscoverLockName()), nil, 0o644)
	if _, _, err := s.TryLockDiscovery(context.Background()); !errors.Is(err, privatefile.ErrUnsafeMode) {
		t.Fatalf("TryLockDiscovery on an open lock file: %v, want the mode refusal", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TryLockDiscovery(context.Background()); err == nil {
		t.Error("TryLockDiscovery on a closed store succeeded")
	}
}

// replaceLockFile removes the lock file and creates a new one at the name, the
// way a sweep and a second process could while the lock is held.
func replaceLockFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, filepath.Join(dir, name), nil, 0o600)
}

// tryLockThen takes the lock with TryLock and runs after on each lock it
// holds, counting the takes.
func tryLockThen(root *os.Root, name string, takes *int, after func(*privatefile.Lock)) func() (*privatefile.Lock, bool, error) {
	return func() (*privatefile.Lock, bool, error) {
		*takes++
		lock, held, err := cachePrivatePolicy.TryLock(root, name)
		if held {
			after(lock)
		}
		return lock, held, err
	}
}

// TestHeldLockRetakesALockFileReplacedWhileHeld removes or replaces the lock
// file after the lock on it is taken. A lock on an inode the name no longer
// reaches is never kept.
func TestHeldLockRetakesALockFileReplacedWhileHeld(t *testing.T) {
	const name = "k.lock"
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, dir string)
	}{
		{"replaced once", func(t *testing.T, dir string) { replaceLockFile(t, dir, name) }},
		{"removed", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newStoreDir(t)
			root := openStoreRoot(t, dir)
			takes := 0
			lock, held, err := endpointTexts.heldLock(root, name, tryLockThen(root, name, &takes, func(*privatefile.Lock) {
				if takes == 1 {
					tc.change(t, dir)
				}
			}))
			if err != nil || !held || takes != 2 {
				t.Fatalf("heldLock: held %v, err %v after %d takes, want the lock on the second", held, err, takes)
			}
			defer func() { _ = lock.Close() }()
			locked, err := lock.File.Stat()
			if err != nil {
				t.Fatal(err)
			}
			named, err := os.Lstat(filepath.Join(dir, name))
			if err != nil || !os.SameFile(locked, named) {
				t.Fatalf("the kept lock is not on the file the name reaches: %v", err)
			}
			// The inode the name reaches is the one locked, so a second taker is refused.
			if _, held, err := cachePrivatePolicy.TryLock(root, name); err != nil || held {
				t.Fatalf("a second TryLock: held %v, err %v, want the lock busy", held, err)
			}
		})
	}
}

// TestHeldLockGivesUpOnALockFileThatKeepsChanging replaces the lock file every
// time the lock is taken. heldLock releases each lock and stops with
// ErrLockReplaced once its retries run out.
func TestHeldLockGivesUpOnALockFileThatKeepsChanging(t *testing.T) {
	const name = "k.lock"
	dir := newStoreDir(t)
	root := openStoreRoot(t, dir)
	takes := 0
	var taken []*privatefile.Lock
	lock, held, err := endpointTexts.heldLock(root, name, tryLockThen(root, name, &takes, func(l *privatefile.Lock) {
		taken = append(taken, l)
		replaceLockFile(t, dir, name)
	}))
	if !errors.Is(err, ErrLockReplaced) || held || lock != nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("heldLock: %v, %v, %v, want ErrLockReplaced naming the file", lock, held, err)
	}
	if len(taken) != lockReplacedRetries+1 {
		t.Errorf("took the lock %d times, want %d", len(taken), lockReplacedRetries+1)
	}
	for i, l := range taken {
		if _, err := l.File.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("lock %d was not released: %v", i, err)
		}
	}
	lock, held, err = endpointTexts.heldLock(root, name, func() (*privatefile.Lock, bool, error) { return nil, false, nil })
	if lock != nil || held || err != nil {
		t.Fatalf("heldLock of a busy lock: %v, %v, %v", lock, held, err)
	}
}
