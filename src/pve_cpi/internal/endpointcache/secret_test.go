package endpointcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

func openStoreRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := cachePrivatePolicy.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestLoadSecretCreatesThirtyTwoPrivateBytesOnce(t *testing.T) {
	dir := newStoreDir(t)
	root := openStoreRoot(t, dir)
	first, outcome, err := endpointTexts.loadSecret(context.Background(), root, time.Second, secretOps)
	if err != nil || outcome != secretCreated {
		t.Fatalf("first load: outcome %v, err %v", outcome, err)
	}
	info, err := os.Lstat(filepath.Join(dir, secretFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 || info.Size() != secretBytes {
		t.Errorf("secret has mode %v and %d bytes, want 0600 and %d", info.Mode(), info.Size(), secretBytes)
	}
	if bytes.Equal(first, make([]byte, secretBytes)) {
		t.Error("secret is all zero bytes")
	}
	second, outcome, err := endpointTexts.loadSecret(context.Background(), root, time.Second, secretOps)
	if err != nil || outcome != secretRead {
		t.Fatalf("second load: outcome %v, err %v", outcome, err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second load returned a different secret")
	}
	if got, want := storeDirNames(t, dir), []string{secretFile, secretLockFile}; !slices.Equal(got, want) {
		t.Errorf("directory holds %v, want %v and no temporary file", got, want)
	}
}

// TestSecretIsWrittenDurably shows that the ops OpenStore writes the secret
// with sync the file and the directory for real, and that both syncs run when
// OpenStore creates a secret.
func TestSecretIsWrittenDurably(t *testing.T) {
	// A real sync fails on a closed file and a closed root, where a sync that
	// does nothing would report success.
	closedFile, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secretOps.SyncFile(closedFile); err == nil {
		t.Error("the secret's file sync does nothing")
	}
	closedRoot, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := closedRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secretOps.SyncDir(closedRoot); err == nil {
		t.Error("the secret's directory sync does nothing")
	}

	prev := secretOps
	t.Cleanup(func() { secretOps = prev })
	var steps []string
	secretOps = privatefile.Ops{
		Write: func(f *os.File, b []byte) (int, error) { steps = append(steps, "write"); return prev.Write(f, b) },
		SyncFile: func(f *os.File) error {
			steps = append(steps, "syncFile")
			return prev.SyncFile(f)
		},
		Rename: func(r *os.Root, a, b string) error { steps = append(steps, "rename"); return prev.Rename(r, a, b) },
		SyncDir: func(r *os.Root) error {
			steps = append(steps, "syncDir")
			return prev.SyncDir(r)
		},
	}
	dir := newStoreDir(t)
	openTestStore(t, dir, storeTestCredential, StoreOptions{})
	if got := strings.Join(steps, ","); got != "write,syncFile,rename,syncDir" {
		t.Fatalf("creating the secret ran %s, want write, syncFile, rename, syncDir", got)
	}
	steps = nil
	openTestStore(t, dir, storeTestCredential, StoreOptions{})
	if len(steps) != 0 {
		t.Errorf("reading an existing secret ran %v", steps)
	}
}

// raceSecret runs rounds of two child processes that load the directory
// secret of a fresh directory at the same moment, after prepare sets each
// directory up. It returns, per round, how many children reported each
// outcome, and checks that both derived the identifier the stored secret
// gives.
func raceSecret(t *testing.T, rounds int, prepare func(dir string)) []map[string]int {
	t.Helper()
	dirs := make([]string, rounds)
	racers := make([][]*storeChild, rounds)
	for round := range rounds {
		dirs[round] = newStoreDir(t)
		prepare(dirs[round])
		racers[round] = []*storeChild{
			startStoreChild(t, storeChildSecret, dirs[round]),
			startStoreChild(t, storeChildSecret, dirs[round]),
		}
	}
	// Every round's two racers start together, so the rounds pay for one
	// child start-up between them rather than one each.
	for round := range rounds {
		for _, c := range racers[round] {
			if got := c.line(t); got != "ready" {
				t.Fatalf("round %d: child said %q, want ready", round, got)
			}
		}
		for _, c := range racers[round] {
			c.release(t)
		}
	}
	outcomes := make([]map[string]int, rounds)
	for round := range rounds {
		outcomes[round] = map[string]int{}
		ids := map[string]bool{}
		for _, c := range racers[round] {
			verb, id, ok := strings.Cut(c.line(t), " ")
			if !ok || !slices.Contains([]string{"read", "created", "replaced"}, verb) {
				t.Fatalf("round %d: child said %q %q", round, verb, id)
			}
			outcomes[round][verb]++
			ids[id] = true
			c.finish(t)
		}
		if len(ids) != 1 {
			t.Fatalf("round %d: the racers derived %d identifiers, want 1", round, len(ids))
		}
		if got, want := storeDirNames(t, dirs[round]), []string{secretFile, secretLockFile}; !slices.Equal(got, want) {
			t.Errorf("round %d: directory holds %v, want %v", round, got, want)
		}
		secret, again, err := endpointTexts.loadSecret(
			context.Background(),
			openStoreRoot(t, dirs[round]),
			time.Second,
			secretOps,
		)
		if err != nil || again != secretRead {
			t.Fatalf("round %d: reload outcome %v, err %v", round, again, err)
		}
		key, _ := mustDerive(t, storeTestConfig(), secret, storeTestCredential)
		if !ids[key.CredentialID] {
			t.Errorf("round %d: the stored secret derives %s, not what the racers derived", round, key.CredentialID)
		}
	}
	return outcomes
}

// TestSecretRaceCreatesItOnce runs two processes that find the secret missing
// at the same moment. Exactly one creates it, and both derive the same
// credential identifier from it.
func TestSecretRaceCreatesItOnce(t *testing.T) {
	for round, got := range raceSecret(t, 5, func(string) {}) {
		if got["created"] != 1 || got["read"] != 1 {
			t.Errorf("round %d: outcomes %v, want one created and one read", round, got)
		}
	}
}

// TestSecretRaceReplacesItOnce runs two processes that find a short secret at
// the same moment. Exactly one replaces it, the other reads the replacement
// under the lock, and both derive the same credential identifier.
func TestSecretRaceReplacesItOnce(t *testing.T) {
	short := bytes.Repeat([]byte{0xcd}, 5)
	outcomes := raceSecret(t, 5, func(dir string) { writeStoreFile(t, filepath.Join(dir, secretFile), short, 0o600) })
	for round, got := range outcomes {
		if got["replaced"] != 1 || got["read"] != 1 {
			t.Errorf("round %d: outcomes %v, want one replaced and one read", round, got)
		}
	}
}

// TestWrongLengthSecretIsReplaced shows that a private secret of any length but
// 32 is replaced with 32 new bytes, that the store reports the replacement once
// with the secret's path, and that a file written under the old secret is no
// longer found.
func TestWrongLengthSecretIsReplaced(t *testing.T) {
	for _, size := range []int{0, 1, 31, 33, 64, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := newStoreDir(t)
			old := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			storePut(t, old, "under the old secret")
			oldFile := old.FileName()
			bad := bytes.Repeat([]byte{0xab}, size)
			writeStoreFile(t, filepath.Join(dir, secretFile), bad, 0o600)

			s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			path, replaced := s.SecretReplaced()
			if !replaced || path != filepath.Join(dir, secretFile) {
				t.Fatalf("SecretReplaced = %q, %v, want the secret's path and true", path, replaced)
			}
			secret := readStoreFile(t, filepath.Join(dir, secretFile))
			if len(secret) != secretBytes || bytes.Equal(secret, bad) {
				t.Fatalf("secret after the replacement holds %d bytes", len(secret))
			}
			info, err := os.Lstat(filepath.Join(dir, secretFile))
			if err != nil || info.Mode() != 0o600 {
				t.Fatalf("replaced secret: %v, %v, want mode 0600", info, err)
			}
			if s.FileName() == oldFile {
				t.Error("the replaced secret kept the old file name")
			}
			var got storeTestPayload
			if err := s.Read(&got); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Read after the replacement: %v, want fs.ErrNotExist", err)
			}
			for _, name := range storeDirNames(t, dir) {
				if strings.HasPrefix(name, privatefile.TempPrefix) {
					t.Errorf("temporary file %s left behind", name)
				}
			}
			again := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			if _, replaced := again.SecretReplaced(); replaced || again.FileName() != s.FileName() {
				t.Errorf("a second store replaced %v and names %s, want no replacement and %s",
					replaced, again.FileName(), s.FileName())
			}
		})
	}
}

// TestUnsafeSecretIsRefusedAndKept covers a secret whose mode, type, or link
// count is wrong. Each disables the cache with an error that names the
// secret's path, and none is replaced, even when its length is wrong too.
func TestUnsafeSecretIsRefusedAndKept(t *testing.T) {
	good := bytes.Repeat([]byte{0x11}, secretBytes)
	cases := map[string]struct {
		setup func(t *testing.T, dir string)
		want  error
		text  string
	}{
		"group readable": {
			setup: func(t *testing.T, dir string) { writeStoreFile(t, filepath.Join(dir, secretFile), good, 0o640) },
			want:  privatefile.ErrUnsafeMode,
		},
		"group readable and short": {
			setup: func(t *testing.T, dir string) {
				writeStoreFile(
					t,
					filepath.Join(dir, secretFile),
					good[:5],
					0o640,
				)
			},
			want: privatefile.ErrUnsafeMode,
		},
		"symlink to a valid secret": {
			setup: func(t *testing.T, dir string) {
				target := filepath.Join(t.TempDir(), "elsewhere")
				writeStoreFile(t, target, good, 0o600)
				if err := os.Symlink(target, filepath.Join(dir, secretFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeFileType,
		},
		"dangling symlink": {
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink(filepath.Join(dir, "planted"), filepath.Join(dir, secretFile)); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeFileType,
		},
		"directory": {
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, secretFile), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: privatefile.ErrUnsafeFileType,
		},
		"second link": {
			setup: func(t *testing.T, dir string) {
				writeStoreFile(t, filepath.Join(dir, secretFile), good, 0o600)
				if err := os.Link(filepath.Join(dir, secretFile), filepath.Join(dir, "copy")); err != nil {
					t.Fatal(err)
				}
			},
			text: "multiply linked",
		},
		"second link and long": {
			setup: func(t *testing.T, dir string) {
				writeStoreFile(t, filepath.Join(dir, secretFile), append(good, good...), 0o600)
				if err := os.Link(filepath.Join(dir, secretFile), filepath.Join(dir, "copy")); err != nil {
					t.Fatal(err)
				}
			},
			text: "multiply linked",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := newStoreDir(t)
			tc.setup(t, dir)
			before := storeDirNames(t, dir)
			beforeInfo, err := os.Lstat(filepath.Join(dir, secretFile))
			if err != nil {
				t.Fatal(err)
			}
			_, err = openStoreOn(t, dir, storeTestCredential, StoreOptions{})
			switch {
			case err == nil:
				t.Fatal("OpenStore accepted an unsafe secret")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("OpenStore: %v, want %v", err, tc.want)
			case tc.text != "" && !strings.Contains(err.Error(), tc.text):
				t.Fatalf("OpenStore: %v, want %q", err, tc.text)
			case !strings.Contains(err.Error(), "refusing directory secret "+filepath.Join(dir, secretFile)+":"):
				t.Fatalf("OpenStore: %v, want the secret's path", err)
			}
			if after := storeDirNames(t, dir); !slices.Equal(after, before) {
				t.Errorf("directory changed from %v to %v", before, after)
			}
			afterInfo, err := os.Lstat(filepath.Join(dir, secretFile))
			if err != nil || !os.SameFile(beforeInfo, afterInfo) {
				t.Errorf("the secret's name now points somewhere else: %v", err)
			}
		})
	}
}

// TestSecretWithAnotherOwnerIsRefused needs root to give a file another owner.
// Without root it shows the same refusal on the directory, which a non-root
// user can meet in "/".
func TestSecretWithAnotherOwnerIsRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		_, err := OpenStore(
			context.Background(),
			openSystemRoot(t),
			storeTestConfig(),
			storeTestCredential,
			StoreOptions{},
		)
		if !errors.Is(err, privatefile.ErrUnsafeOwnership) || !errors.Is(err, errCacheOwnership) {
			t.Fatalf("OpenStore on a directory another user owns: %v, want the ownership refusal", err)
		}
		return
	}
	dir := newStoreDir(t)
	good := bytes.Repeat([]byte{0x22}, secretBytes)
	writeStoreFile(t, filepath.Join(dir, secretFile), good, 0o600)
	if err := os.Lchown(filepath.Join(dir, secretFile), 1, 1); err != nil {
		t.Fatal(err)
	}
	_, err := openStoreOn(t, dir, storeTestCredential, StoreOptions{})
	if !errors.Is(err, privatefile.ErrUnsafeOwnership) ||
		!strings.Contains(err.Error(), filepath.Join(dir, secretFile)) {
		t.Fatalf("OpenStore: %v, want the ownership refusal naming the secret", err)
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), good) {
		t.Error("the secret was replaced")
	}
	// A short secret with another owner is refused the same way, not replaced.
	writeStoreFile(t, filepath.Join(dir, secretFile), good[:7], 0o600)
	if err := os.Lchown(filepath.Join(dir, secretFile), 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := openStoreOn(
		t,
		dir,
		storeTestCredential,
		StoreOptions{},
	); !errors.Is(err, privatefile.ErrUnsafeOwnership) {
		t.Fatalf("OpenStore on a short secret with another owner: %v, want the ownership refusal", err)
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), good[:7]) {
		t.Error("the short secret with another owner was replaced")
	}
}

// openSystemRoot opens "/", which another user owns in a non-root run, as a
// root OpenStore must refuse.
func openSystemRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot("/")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSecretLockHeldByAnotherProcess shows that a process that cannot take
// secret.lock within its wait creates nothing and reports the lock busy, and
// that a caller whose context ends reports that instead.
func TestSecretLockHeldByAnotherProcess(t *testing.T) {
	dir := newStoreDir(t)
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+secretLockFile)
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	root := openStoreRoot(t, dir)
	if _, _, err := endpointTexts.loadSecret(
		context.Background(),
		root,
		100*time.Millisecond,
		secretOps,
	); !errors.Is(err, ErrSecretBusy) {
		t.Fatalf("loadSecret under a held lock: %v, want ErrSecretBusy", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := endpointTexts.loadSecret(ctx, root, time.Second, secretOps)
	if !errors.Is(err, ErrLockWaitAbandoned) || !errors.Is(err, context.Canceled) {
		t.Fatalf("loadSecret with an ended context: %v, want ErrLockWaitAbandoned and context.Canceled", err)
	}
	if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretLockFile}) {
		t.Errorf("directory holds %v, want only the lock", got)
	}
	// A short secret waits for the same lock before it is replaced.
	short := []byte("short")
	writeStoreFile(t, filepath.Join(dir, secretFile), short, 0o600)
	if _, _, err := endpointTexts.loadSecret(
		context.Background(),
		root,
		100*time.Millisecond,
		secretOps,
	); !errors.Is(err, ErrSecretBusy) {
		t.Fatalf("loadSecret of a short secret under a held lock: %v, want ErrSecretBusy", err)
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), short) {
		t.Error("the short secret was replaced without the lock")
	}
	holder.finish(t)
	if _, outcome, err := endpointTexts.loadSecret(
		context.Background(),
		root,
		time.Second,
		secretOps,
	); err != nil || outcome != secretReplaced {
		t.Fatalf("loadSecret after release: outcome %v, err %v", outcome, err)
	}
}

// TestKeyMaterialNeverPrints formats the directory secret, a derived key, and a
// Store every way fmt, slog, and encoding/json can, and checks that none of
// the bytes appear.
func TestKeyMaterialNeverPrints(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	secret := keyMaterial(readStoreFile(t, filepath.Join(dir, secretFile)))
	_, mac := mustDerive(t, storeTestConfig(), secret, storeTestCredential)

	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	for _, value := range []any{secret, mac, s, *s} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%d", "%q"} {
			fmt.Fprintf(&out, verb+"\n", value)
		}
		fmt.Fprintln(&out, value)
		logger.Info("value", "value", value)
	}
	for _, value := range []any{secret, mac} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(data)
	}
	text := strings.ToLower(out.String())
	for name, m := range map[string]keyMaterial{"directory secret": secret, "HMAC key": mac} {
		encoded := hex.EncodeToString(m)
		if strings.Contains(text, encoded) || strings.Contains(text, encoded[:16]) || bytes.Contains(out.Bytes(), m) {
			t.Errorf("the %s appeared in formatted output", name)
		}
		if strings.Contains(out.String(), fmt.Sprintf("%d", []byte(m))) {
			t.Errorf("the %s appeared as a byte list", name)
		}
	}
	if !strings.Contains(out.String(), redactedMarker) {
		t.Error("the redaction marker never appeared")
	}
}

// TestUnsafeSecretLockIsRefused shows that a lock file that fails the private
// checks is reported as it is, not as a busy lock, and creates no secret.
func TestUnsafeSecretLockIsRefused(t *testing.T) {
	dir := newStoreDir(t)
	writeStoreFile(t, filepath.Join(dir, secretLockFile), nil, 0o644)
	_, err := openStoreOn(t, dir, storeTestCredential, StoreOptions{})
	if !errors.Is(err, privatefile.ErrUnsafeMode) || errors.Is(err, ErrSecretBusy) {
		t.Fatalf("OpenStore with an open secret.lock: %v, want the mode refusal", err)
	}
	if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretLockFile}) {
		t.Errorf("directory holds %v, want only the lock", got)
	}
}

// TestSecretCreationFailureLeavesNoSecret makes the directory unwritable after
// the lock file exists, so the temporary file cannot be created. Root ignores
// the mode, so the test needs another user.
func TestSecretCreationFailureLeavesNoSecret(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory whatever its mode")
	}
	dir := newStoreDir(t)
	writeStoreFile(t, filepath.Join(dir, secretLockFile), nil, 0o600)
	root := openStoreRoot(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	secret, outcome, err := endpointTexts.loadSecret(context.Background(), root, time.Second, secretOps)
	if err == nil || outcome != secretRead || secret != nil ||
		!strings.Contains(err.Error(), "write directory secret "+filepath.Join(dir, secretFile)) {
		t.Fatalf("loadSecret in a read-only directory: outcome %v, err %v", outcome, err)
	}
	if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretLockFile}) {
		t.Errorf("directory holds %v, want only the lock", got)
	}
	// A short secret that cannot be replaced is left as it was.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStoreFile(t, filepath.Join(dir, secretFile), []byte("short"), 0o600)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, _, err := endpointTexts.loadSecret(context.Background(), root, time.Second, secretOps); err == nil {
		t.Fatal("loadSecret replaced a short secret in a read-only directory")
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), []byte("short")) {
		t.Error("a failed replacement changed the short secret")
	}
}

func TestMaterialMethodsRedact(t *testing.T) {
	m := storeTestSecret(0x42)
	text, err := m.MarshalText()
	if m.String() != redactedMarker || m.GoString() != redactedMarker || m.LogValue().String() != redactedMarker ||
		err != nil || string(text) != redactedMarker {
		t.Error("a keyMaterial method printed something other than the redaction marker")
	}
}
