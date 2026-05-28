package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// buildState guards the cached binary path across the test process.
var buildState struct {
	mu  sync.Mutex
	bin string // non-empty after a successful build
	err error  // set only for genuine compile errors; nil on success or before first attempt
}

// buildOnce returns the path to the compiled CPI binary, building it on first
// call and caching a successful result for subsequent calls.
//
// Error classification:
//   - Genuine compile errors (non-zero go build exit, syntax/type errors) are
//     cached and returned on every subsequent call — the binary is broken and
//     retrying won't help.
//   - Transient errors (MkdirTemp failure, exec.ErrNotFound) are retried once;
//     if the retry also fails, the error is returned but NOT cached so the next
//     caller can try again.
//
// t is used for t.Fatalf on genuine build failures; for other errors the caller
// decides.
func buildOnce(t *testing.T) (string, error) {
	t.Helper()

	buildState.mu.Lock()
	defer buildState.mu.Unlock()

	// Successful build already cached.
	if buildState.bin != "" {
		return buildState.bin, nil
	}

	// Genuine compile error cached from a prior attempt — no point retrying.
	if buildState.err != nil {
		return "", buildState.err
	}

	bin, err := attemptBuild()
	if err == nil {
		buildState.bin = bin
		return bin, nil
	}

	if isTransientBuildErr(err) {
		// One retry for transient errors (e.g. tmp dir creation race, PATH glitch).
		bin, retryErr := attemptBuild()
		if retryErr == nil {
			buildState.bin = bin
			return bin, nil
		}
		// Retry also failed; do not cache — leave room for the next caller.
		return "", retryErr
	}

	// Genuine compile error: cache so subsequent callers skip the slow build.
	buildState.err = err
	return "", err
}

// attemptBuild runs go build once and returns the binary path on success.
func attemptBuild() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	dir, err := os.MkdirTemp("", "bosh-pve-cpi-bin-*")
	if err != nil {
		return "", fmt.Errorf("MkdirTemp: %w", err)
	}

	bin := filepath.Join(dir, "cpi")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", bin, "./cmd/cpi")
	cmd.Dir = repoRoot
	out, buildErr := cmd.CombinedOutput()
	if buildErr != nil {
		return "", fmt.Errorf("go build: %w\n%s", buildErr, out)
	}
	return bin, nil
}

// isTransientBuildErr reports whether err looks like a transient infrastructure
// failure rather than a genuine compile error. Heuristic: exec.ErrNotFound means
// "go" binary is absent; os.ErrNotExist on MkdirTemp means the OS tmp dir is gone.
// A go build exit-code failure with output is treated as a genuine compile error.
func isTransientBuildErr(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// MkdirTemp wraps os-level errors; detect by message prefix.
	if strings.HasPrefix(err.Error(), "MkdirTemp:") {
		return true
	}
	return false
}

// makeExecCmd creates an exec.Cmd for running the binary. Used by binary tests
// to avoid repeating ctx/path boilerplate.
func makeExecCmd(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}

// TestMain removes the buildOnce binary's parent temp directory after the test
// process exits. The binary is built into os.MkdirTemp("", "bosh-pve-cpi-bin-*")
// and reused across every test in this package (see buildOnce); t.TempDir is
// not usable because its lifetime is bound to a single *testing.T.
func TestMain(m *testing.M) {
	code := m.Run()
	buildState.mu.Lock()
	bin := buildState.bin
	buildState.mu.Unlock()
	if bin != "" {
		_ = os.RemoveAll(filepath.Dir(bin))
	}
	os.Exit(code)
}
