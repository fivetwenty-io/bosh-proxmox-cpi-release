package endpointcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	first, created, err := loadSecret(context.Background(), root, time.Second)
	if err != nil || !created {
		t.Fatalf("first load: created %v, err %v", created, err)
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
	second, created, err := loadSecret(context.Background(), root, time.Second)
	if err != nil || created {
		t.Fatalf("second load: created %v, err %v", created, err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second load returned a different secret")
	}
	if got, want := storeDirNames(t, dir), []string{secretFile, secretLockFile}; !slices.Equal(got, want) {
		t.Errorf("directory holds %v, want %v and no temporary file", got, want)
	}
}

// TestSecretRaceCreatesItOnce runs two processes that find the secret missing
// at the same moment. Exactly one creates it, and both derive the same
// credential identifier from it.
func TestSecretRaceCreatesItOnce(t *testing.T) {
	// Every round's two racers start together, so the rounds pay for one child
	// start-up between them rather than one each.
	const rounds = 5
	dirs := make([]string, rounds)
	racers := make([][]*storeChild, rounds)
	for round := range rounds {
		dirs[round] = newStoreDir(t)
		racers[round] = []*storeChild{
			startStoreChild(t, storeChildSecret, dirs[round]),
			startStoreChild(t, storeChildSecret, dirs[round]),
		}
	}
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
	for round := range rounds {
		created := 0
		ids := map[string]bool{}
		for _, c := range racers[round] {
			verb, id, ok := strings.Cut(c.line(t), " ")
			if !ok || verb != "created" && verb != "read" {
				t.Fatalf("round %d: child said %q %q", round, verb, id)
			}
			if verb == "created" {
				created++
			}
			ids[id] = true
			c.finish(t)
		}
		if created != 1 || len(ids) != 1 {
			t.Fatalf("round %d: %d processes created the secret and they derived %d identifiers, want 1 and 1",
				round, created, len(ids))
		}
		if got, want := storeDirNames(t, dirs[round]), []string{secretFile, secretLockFile}; !slices.Equal(got, want) {
			t.Errorf("round %d: directory holds %v, want %v", round, got, want)
		}
		secret, again, err := loadSecret(context.Background(), openStoreRoot(t, dirs[round]), time.Second)
		if err != nil || again {
			t.Fatalf("round %d: reload created %v, err %v", round, again, err)
		}
		key, _ := mustDerive(t, storeTestConfig(), secret, storeTestCredential)
		if !ids[key.CredentialID] {
			t.Errorf("round %d: the stored secret derives %s, not what the racers derived", round, key.CredentialID)
		}
	}
}

// TestMalformedSecretDisablesTheCache shows that a secret of any length but 32
// makes OpenStore fail and is left exactly as it was.
func TestMalformedSecretDisablesTheCache(t *testing.T) {
	for _, size := range []int{0, 1, 31, 33, 64, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			dir := newStoreDir(t)
			bad := bytes.Repeat([]byte{0xab}, size)
			writeStoreFile(t, filepath.Join(dir, secretFile), bad, 0o600)
			s, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{})
			if !errors.Is(err, ErrSecretInvalid) || s != nil {
				t.Fatalf("OpenStore: %v, want ErrSecretInvalid", err)
			}
			if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), bad) {
				t.Error("the malformed secret was replaced")
			}
			if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretFile}) {
				t.Errorf("directory holds %v, want only the secret", got)
			}
		})
	}
}

// TestUnsafeSecretIsRefusedAndKept covers a secret whose mode, type, or link
// count is wrong. Each disables the cache, and none is replaced.
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
			_, err = OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{})
			switch {
			case err == nil:
				t.Fatal("OpenStore accepted an unsafe secret")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("OpenStore: %v, want %v", err, tc.want)
			case tc.text != "" && !strings.Contains(err.Error(), tc.text):
				t.Fatalf("OpenStore: %v, want %q", err, tc.text)
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
		_, err := OpenStore(context.Background(), "/", storeTestConfig(), storeTestCredential, StoreOptions{})
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
	_, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{})
	if !errors.Is(err, privatefile.ErrUnsafeOwnership) {
		t.Fatalf("OpenStore: %v, want the ownership refusal", err)
	}
	if !bytes.Equal(readStoreFile(t, filepath.Join(dir, secretFile)), good) {
		t.Error("the secret was replaced")
	}
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
	if _, _, err := loadSecret(context.Background(), root, 100*time.Millisecond); !errors.Is(err, ErrSecretBusy) {
		t.Fatalf("loadSecret under a held lock: %v, want ErrSecretBusy", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := loadSecret(ctx, root, time.Second)
	if !errors.Is(err, ErrLockWaitAbandoned) || !errors.Is(err, context.Canceled) {
		t.Fatalf("loadSecret with an ended context: %v, want ErrLockWaitAbandoned and context.Canceled", err)
	}
	if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretLockFile}) {
		t.Errorf("directory holds %v, want only the lock", got)
	}
	holder.finish(t)
	if _, created, err := loadSecret(context.Background(), root, time.Second); err != nil || !created {
		t.Fatalf("loadSecret after release: created %v, err %v", created, err)
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
	_, err := OpenStore(context.Background(), dir, storeTestConfig(), storeTestCredential, StoreOptions{})
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
	secret, created, err := loadSecret(context.Background(), root, time.Second)
	if err == nil || created || secret != nil || !strings.Contains(err.Error(), "create directory secret") {
		t.Fatalf("loadSecret in a read-only directory: created %v, err %v", created, err)
	}
	if got := storeDirNames(t, dir); !slices.Equal(got, []string{secretLockFile}) {
		t.Errorf("directory holds %v, want only the lock", got)
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
