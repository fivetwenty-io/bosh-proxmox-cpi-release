package safeexec_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	safeexec "github.com/fivetwenty-io/bosh-pve-cpi/internal/exec"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

func nopLogger() *log.Logger { return log.NewNopLogger() }

// skipIfMissing skips the test if binary does not exist on this platform.
func skipIfMissing(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary %q not present: %v", p, err)
		}
	}
}

// TestEmptyAllowlist verifies rule 1: empty allowlist → error, no exec.
func TestEmptyAllowlist(t *testing.T) {
	r := safeexec.New(nil, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), "/bin/echo", []string{"hi"}, nil)
	if err == nil {
		t.Fatal("expected error for empty allowlist, got nil")
	}
	if !strings.Contains(err.Error(), "no allowlist configured") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestRelativePath verifies rule 2: relative path → error.
func TestRelativePath(t *testing.T) {
	r := safeexec.New([]string{"/bin/echo"}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), "echo", nil, nil)
	if err == nil {
		t.Fatal("expected error for relative path, got nil")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestNotAllowlisted verifies rule 3: absolute path not in allowlist → error.
func TestNotAllowlisted(t *testing.T) {
	skipIfMissing(t, "/bin/echo")
	r := safeexec.New([]string{"/bin/true"}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), "/bin/echo", []string{"hi"}, nil)
	if err == nil {
		t.Fatal("expected error for non-allowlisted path, got nil")
	}
	if !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestNoShellInterpretation verifies rule 4: shell metacharacters in args are inert.
// Output must contain the literal strings, proving no shell interpretation.
func TestNoShellInterpretation(t *testing.T) {
	echoPath := "/bin/echo"
	skipIfMissing(t, echoPath)

	r := safeexec.New([]string{echoPath}, nil, 0, nopLogger())
	out, err := r.Run(context.Background(), echoPath, []string{"$(whoami)", "; rm -rf /"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "$(whoami)") {
		t.Errorf("expected literal $(whoami) in output, got: %q", out)
	}
	if !strings.Contains(out, "; rm -rf /") {
		t.Errorf("expected literal '; rm -rf /' in output, got: %q", out)
	}
}

// TestEnvScrub verifies rule 5: SECRET_X not passed; passlisted var and extraEnv var present.
func TestEnvScrub(t *testing.T) {
	envPath := "/usr/bin/env"
	skipIfMissing(t, envPath)

	// Inject a secret that must NOT appear in child env.
	t.Setenv("SECRET_X", "supersecret")
	// Inject a passlisted var that MUST appear.
	t.Setenv("SAFE_VAR", "safevalue")

	r := safeexec.New(
		[]string{envPath},
		[]string{"SAFE_VAR"}, // EnvPasslist includes SAFE_VAR, excludes SECRET_X
		0,
		nopLogger(),
	)
	extraEnv := map[string]string{"CPI_METHOD": "create_vm"}
	out, err := r.Run(context.Background(), envPath, nil, extraEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(out, "SECRET_X") {
		t.Errorf("SECRET_X leaked into child env; output: %q", out)
	}
	if !strings.Contains(out, "SAFE_VAR=safevalue") {
		t.Errorf("SAFE_VAR not found in child env; output: %q", out)
	}
	if !strings.Contains(out, "CPI_METHOD=create_vm") {
		t.Errorf("CPI_METHOD extraEnv not found in child env; output: %q", out)
	}
}

// TestTimeout verifies rule 6: deadline exceeded for a long-running process.
func TestTimeout(t *testing.T) {
	sleepPath := "/bin/sleep"
	skipIfMissing(t, sleepPath)

	r := safeexec.New([]string{sleepPath}, nil, 150*time.Millisecond, nopLogger())
	start := time.Now()
	_, err := r.Run(context.Background(), sleepPath, []string{"10"}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out' in error, got: %v", err)
	}
	// Must finish well under the sleep duration; generous bound of 3s for slow CI.
	if elapsed > 3*time.Second {
		t.Errorf("timeout did not interrupt sleep quickly enough: elapsed=%v", elapsed)
	}
}

// TestNonZeroExit verifies rule 7: non-zero exit → error with exit code.
func TestNonZeroExit(t *testing.T) {
	// Try /usr/bin/false then /bin/false.
	falsePath := ""
	for _, p := range []string{"/usr/bin/false", "/bin/false"} {
		if _, err := os.Stat(p); err == nil {
			falsePath = p
			break
		}
	}
	if falsePath == "" {
		t.Skip("neither /usr/bin/false nor /bin/false present")
	}

	r := safeexec.New([]string{falsePath}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), falsePath, nil, nil)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	// Error must mention an exit code.
	if !strings.Contains(err.Error(), "exited") {
		t.Fatalf("expected 'exited' in error, got: %v", err)
	}
}

// TestAllowlistCleanedPath verifies filepath.Clean normalization is applied to
// both the allowlist entry and the requested path before comparison.
func TestAllowlistCleanedPath(t *testing.T) {
	echoPath := "/bin/echo"
	skipIfMissing(t, echoPath)

	// Allowlist uses an unclean path; request uses a different unclean path.
	// Both clean to /bin/echo — should be allowed.
	r := safeexec.New([]string{"/bin/../bin/echo"}, nil, 0, nopLogger())
	out, err := r.Run(context.Background(), "/bin/./echo", []string{"clean"}, nil)
	if err != nil {
		t.Fatalf("expected allowlist match after Clean, got error: %v", err)
	}
	if !strings.Contains(out, "clean") {
		t.Errorf("unexpected output: %q", out)
	}
}

// TestNilExtraEnv verifies nil extraEnv does not panic.
func TestNilExtraEnv(t *testing.T) {
	echoPath := "/bin/echo"
	skipIfMissing(t, echoPath)

	r := safeexec.New([]string{echoPath}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), echoPath, []string{"ok"}, nil)
	if err != nil {
		t.Fatalf("nil extraEnv caused error: %v", err)
	}
}

// TestNilLogger verifies Logger=nil does not panic.
func TestNilLogger(t *testing.T) {
	echoPath := "/bin/echo"
	skipIfMissing(t, echoPath)

	r := safeexec.New([]string{echoPath}, nil, 0, nil)
	_, err := r.Run(context.Background(), echoPath, []string{"ok"}, nil)
	if err != nil {
		t.Fatalf("nil logger caused error: %v", err)
	}
}

// TestCallerContextCancel verifies that cancelling the caller's context
// also stops the child process (context propagation).
func TestCallerContextCancel(t *testing.T) {
	sleepPath := "/bin/sleep"
	skipIfMissing(t, sleepPath)

	ctx, cancel := context.WithCancel(context.Background())
	r := safeexec.New([]string{sleepPath}, nil, 0, nopLogger())

	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, sleepPath, []string{"10"}, nil)
		done <- err
	}()

	// Cancel after a brief delay.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after context cancel, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel within 3s")
	}
}

// TestSymlinkBypassRejected is the security regression for FIX 1.
// allowlist=[realA], symlink L->realB (realB!=realA) → Run(L) must be rejected
// because EvalSymlinks(L)==realB which does not match allowlisted realA.
func TestSymlinkBypassRejected(t *testing.T) {
	echoPath := "/bin/echo"
	shPath := "/bin/sh"
	skipIfMissing(t, echoPath, shPath)

	dir := t.TempDir()
	linkPath := dir + "/mylink"
	// Symlink points to /bin/sh; allowlist contains only /bin/echo.
	if err := os.Symlink(shPath, linkPath); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	r := safeexec.New([]string{echoPath}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), linkPath, nil, nil)
	if err == nil {
		t.Fatal("expected rejection: symlink resolves to non-allowlisted target, got nil error")
	}
	if !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestSymlinkAllowedWhenTargetMatches verifies the complementary case:
// allowlist=[realA], symlink L->realA → Run(L) is allowed because both resolve equal.
func TestSymlinkAllowedWhenTargetMatches(t *testing.T) {
	echoPath := "/bin/echo"
	skipIfMissing(t, echoPath)

	dir := t.TempDir()
	linkPath := dir + "/echolink"
	if err := os.Symlink(echoPath, linkPath); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	// Allowlist the real /bin/echo; call via the symlink.
	r := safeexec.New([]string{echoPath}, nil, 0, nopLogger())
	out, err := r.Run(context.Background(), linkPath, []string{"symlink-ok"}, nil)
	if err != nil {
		t.Fatalf("expected success when symlink resolves to allowlisted target, got: %v", err)
	}
	if !strings.Contains(out, "symlink-ok") {
		t.Errorf("unexpected output: %q", out)
	}
}

// TestNonExistentPathRejected verifies fail-closed behaviour: a path that does
// not exist on disk is rejected with an error, never matched lexically.
func TestNonExistentPathRejected(t *testing.T) {
	ghost := "/no/such/binary/ever"
	r := safeexec.New([]string{ghost}, nil, 0, nopLogger())
	_, err := r.Run(context.Background(), ghost, nil, nil)
	if err == nil {
		t.Fatal("expected error for non-existent path, got nil")
	}
	// Must indicate resolution failure, not a successful allowlist check.
	if !strings.Contains(err.Error(), "cannot be resolved") {
		t.Fatalf("unexpected error text (expected 'cannot be resolved'): %v", err)
	}
}

// TestTimeoutKillsProcessGroup verifies that a timed-out sleep is interrupted
// well within the sleep duration, confirming the process-group kill path.
// (Duplicate of TestTimeout but explicitly names the process-group intent.)
func TestTimeoutKillsProcessGroup(t *testing.T) {
	sleepPath := "/bin/sleep"
	skipIfMissing(t, sleepPath)

	r := safeexec.New([]string{sleepPath}, nil, 200*time.Millisecond, nopLogger())
	start := time.Now()
	_, err := r.Run(context.Background(), sleepPath, []string{"60"}, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out' in error, got: %v", err)
	}
	// Must finish well under the sleep duration; generous bound for slow CI.
	if elapsed > 5*time.Second {
		t.Errorf("process group kill did not interrupt sleep quickly: elapsed=%v", elapsed)
	}
}
