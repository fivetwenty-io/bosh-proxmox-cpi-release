package endpointcache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// storeTestCredential stands in for a token secret or a password.
const storeTestCredential = "token-secret-0123456789"

// storeTestUser stands in for the API token ID the credential belongs to.
const storeTestUser = "cpi@pve!cache"

// storeTestConfig is a two-endpoint configuration with the system trust store.
func storeTestConfig() KeyConfig {
	return KeyConfig{
		Endpoints: []string{"pve1.dc1.example.com:8006", "pve1b.dc1.example.com:8006"},
		VerifySSL: true,
		User:      storeTestUser,
	}
}

// storeTestPayload stands in for the cache payload, which carries the key and
// whatever the store's caller records.
type storeTestPayload struct {
	Key  Key    `json:"key"`
	Note string `json:"note"`
}

// newStoreDir returns a fresh private directory.
func newStoreDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// storeTestRoot opens dir the way the client does, through Resolve with dir
// as the override, and returns the root for OpenStore, which owns it.
func storeTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	choice := Resolve(Options{Override: dir})
	if !choice.Usable() {
		t.Fatalf("Resolve(%s): %v", dir, choice.Err)
	}
	return choice.Root
}

// openStoreOn opens a store on dir for storeTestConfig with credential.
func openStoreOn(t *testing.T, dir, credential string, opts StoreOptions) (*Store, error) {
	t.Helper()
	return OpenStore(context.Background(), storeTestRoot(t, dir), storeTestConfig(), credential, opts)
}

// openTestStore opens a store on dir for storeTestConfig with credential, and
// closes it when the test ends.
func openTestStore(t *testing.T, dir, credential string, opts StoreOptions) *Store {
	t.Helper()
	s, err := openStoreOn(t, dir, credential, opts)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// put writes a payload carrying note through Update.
func storePut(t *testing.T, s *Store, note string) {
	t.Helper()
	written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: s.Key(), Note: note}, nil
	})
	if err != nil || !written {
		t.Fatalf("Update: written %v, err %v", written, err)
	}
}

// storeDirNames returns the sorted names in dir.
func storeDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func readStoreFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test reads a file it created under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeStoreFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Child process modes. The parent runs this test binary again with one of
// these in ENDPOINT_CACHE_CHILD, so a lock or a race involves a real second
// process rather than a goroutine sharing this one's file table.
const (
	storeChildEnv     = "ENDPOINT_CACHE_CHILD"
	storeChildDirEnv  = "ENDPOINT_CACHE_DIRECTORY"
	storeChildLockEnv = "ENDPOINT_CACHE_LOCK"

	// storeChildSecret waits for its standard input to close, so every racer starts
	// together, then loads the directory secret and prints whether it read,
	// created, or replaced it, followed by the credential identifier derived
	// from it.
	storeChildSecret = "secret"
	// storeChildLock takes the lock named in ENDPOINT_CACHE_LOCK, prints "locked",
	// and holds it until its standard input closes.
	storeChildLock = "lock"
)

// TestEndpointCacheChild is the body of the child processes. It does nothing
// in an ordinary test run.
func TestEndpointCacheChild(t *testing.T) {
	mode := os.Getenv(storeChildEnv)
	if mode == "" {
		return
	}
	root, err := cachePrivatePolicy.OpenRoot(os.Getenv(storeChildDirEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	switch mode {
	case storeChildSecret:
		if _, err := fmt.Println("ready"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		secret, outcome, err := endpointTexts.loadSecret(context.Background(), root, 30*time.Second, secretOps)
		if err != nil {
			t.Fatal(err)
		}
		key, _, err := deriveKeys(storeTestConfig(), secret, storeTestCredential)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Println(secretOutcomeNames[outcome], key.CredentialID); err != nil {
			t.Fatal(err)
		}
	case storeChildLock:
		lock, held, err := cachePrivatePolicy.TryLock(root, os.Getenv(storeChildLockEnv))
		if err != nil || !held {
			t.Fatalf("child lock: held %v, err %v", held, err)
		}
		if _, err := fmt.Println("locked"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
}

// secretOutcomeNames are the words the secret child prints for each outcome.
var secretOutcomeNames = map[secretOutcome]string{secretRead: "read", secretCreated: "created", secretReplaced: "replaced"}

// child is a running child process.
type storeChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// startStoreChild runs this test binary as a child in mode on dir, with extra
// environment entries, and stops it when the test ends.
func startStoreChild(t *testing.T, mode, dir string, extra ...string) *storeChild {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEndpointCacheChild$") // #nosec G204 -- reruns this test binary
	cmd.Env = append(append(os.Environ(), storeChildEnv+"="+mode, storeChildDirEnv+"="+dir), extra...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &storeChild{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(func() {
		_ = c.stdin.Close()
		if c.cmd.ProcessState == nil {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
		}
	})
	return c
}

// line reads the child's next line of output without its newline.
func (c *storeChild) line(t *testing.T) string {
	t.Helper()
	line, err := c.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("read child output: %v (got %q)", err, line)
	}
	return strings.TrimSuffix(line, "\n")
}

// release closes the child's input, which starts a racer or frees a held
// lock. A second call does nothing.
func (c *storeChild) release(t *testing.T) {
	t.Helper()
	if err := c.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
}

// finish releases the child and waits for it to exit cleanly.
func (c *storeChild) finish(t *testing.T) {
	t.Helper()
	c.release(t)
	rest, err := io.ReadAll(c.stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, rest)
	}
}
