package endpointcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// optionsNamed returns the directory options for dir whose refusals call the
// cache name.
func optionsNamed(dir, name string) Options {
	return Options{Override: dir, DisplayName: name}
}

// messageScenarios makes each failure that a lock, the directory secret, the
// directory, or the cache file can meet, in a directory of its own, and returns
// the text of every one. The directory and the store's file and lock names are
// replaced by <dir>, <file>, and <lock>, so the endpoint cache's texts can be
// compared with fixed text and another kind of store's with those texts under
// its own name. opts selects the kind of store and name is its display name.
func messageScenarios(t *testing.T, opts StoreOptions, name string) []string {
	t.Helper()
	var out []string
	var dir, file, lock string
	record := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error", what)
		}
		text := err.Error()
		if file != "" {
			text = strings.ReplaceAll(text, file, "<file>")
			text = strings.ReplaceAll(text, lock, "<lock>")
		}
		out = append(out, what+": "+strings.ReplaceAll(text, dir, "<dir>"))
	}
	resolveRoot := func(dir string) *os.Root {
		t.Helper()
		choice := Resolve(optionsNamed(dir, name))
		if !choice.Usable() {
			t.Fatalf("Resolve(%s): %v", dir, choice.Err)
		}
		return choice.Root
	}
	open := func(d string, o StoreOptions) (*Store, error) {
		t.Helper()
		return OpenStore(context.Background(), resolveRoot(d), storeTestConfig(), storeTestCredential, o)
	}
	fresh := func() {
		dir = newStoreDir(t)
		file, lock = "", ""
	}
	openAndPut := func() *Store {
		t.Helper()
		s, err := open(dir, opts)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		storePut(t, s, "x")
		file, lock = s.FileName(), s.kind.lockName(s.Key())
		return s
	}

	fresh()
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	record("directory open mode", Resolve(optionsNamed(dir, name)).Err)

	fresh()
	record("directory path not clean", Resolve(optionsNamed(dir+"/.", name)).Err)

	fresh()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	record("directory without owner access", Resolve(optionsNamed(dir, name)).Err)

	fresh()
	root := resolveRoot(dir)
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	_, err := OpenStore(context.Background(), root, storeTestConfig(), storeTestCredential, opts)
	record("opened directory open mode", err)

	_, err = OpenStore(context.Background(), nil, storeTestConfig(), storeTestCredential, opts)
	record("no directory", err)

	fresh()
	writeStoreFile(t, filepath.Join(dir, secretFile), make([]byte, secretBytes), 0o644)
	_, err = open(dir, opts)
	record("secret open mode", err)

	fresh()
	writeStoreFile(t, filepath.Join(dir, secretFile), []byte("short"), 0o600)
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+secretLockFile)
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	_, err = open(dir, opts)
	record("secret lock busy", err)
	holder.finish(t)

	fresh()
	s := openAndPut()
	writeStoreFile(t, filepath.Join(dir, lock), nil, 0o644)
	_, err = Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) { return nil, nil })
	record("lock file open mode", err)
	_, _, err = s.TryLockDiscovery(context.Background())
	// The discover lock is a different file and is unaffected, so this must
	// succeed and is not recorded.
	if err != nil {
		t.Fatalf("TryLockDiscovery: %v", err)
	}

	fresh()
	logger, observed := log.NewObservedLogger(log.LevelDebug)
	logged := opts
	logged.Logger = logger
	s, err = open(dir, logged)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	file, lock = s.FileName(), s.kind.lockName(s.Key())
	s.lockWait = 100 * time.Millisecond
	holder = startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+lock)
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	update := func(ctx context.Context) error {
		_, err := Update(ctx, s, func(*storeTestPayload) (*storeTestPayload, error) {
			return &storeTestPayload{Key: s.Key()}, nil
		})
		return err
	}
	if err := update(context.Background()); err != nil {
		t.Fatalf("Update under a held lock: %v", err)
	}
	for _, e := range observed.All() {
		out = append(out, "skipped write: "+e.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	record("lock wait abandoned", update(ctx))
	holder.finish(t)

	fresh()
	s = openAndPut()
	var got storeTestPayload
	writeStoreFile(t, filepath.Join(dir, file), []byte("not json"), 0o600)
	record("damaged file", s.Read(&got))
	other := s.Key()
	other.CredentialID = "0000000000000000"
	if err := s.policy.WriteEnvelope(s.guard.root, s.name, s.kind.version,
		storeTestPayload{Key: other}, s.ops); err != nil {
		t.Fatal(err)
	}
	record("file for another key", s.Read(&got))
	if err := s.policy.WriteEnvelope(s.guard.root, s.name, s.kind.version+1,
		storeTestPayload{Key: s.Key()}, s.ops); err != nil {
		t.Fatal(err)
	}
	record("newer file version", s.Read(&got))
	storePut(t, s, "again")
	if err := os.Chmod(filepath.Join(dir, file), 0o644); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	record("file open mode", s.Read(&got))

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	record("read on a closed store", s.Read(&got))
	_, err = Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) { return nil, nil })
	record("update on a closed store", err)
	_, _, err = s.TryLockDiscovery(context.Background())
	record("discovery lock on a closed store", err)
	record("sweep on a closed store", s.Sweep(time.Now()).Err)

	if len(out) == 0 {
		t.Fatal("no scenario ran")
	}
	return out
}
