// Package exec (safeexec) provides a security-hardened external-command runner
// for the BOSH CPI external_command hook surface. Safety envelope is the
// primary feature: allowlist enforcement, no shell, env scrubbing, timeout.
package safeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// Runner executes external commands within a strict safety envelope.
//
// Allowlist   - absolute paths of binaries permitted to run; empty = inert (no exec).
// EnvPasslist - os environment variable names forwarded to child; all others stripped.
// Timeout     - per-call deadline; <=0 means no deadline (caller context governs).
// Logger      - structured debug output; nil is safe (no-op).
//
// Allowlist entries are resolved via filepath.EvalSymlinks once, at the first
// Run call (lazy sync.Once), and cached for the lifetime of the Runner. A
// post-construction symlink swap therefore does not alter the effective
// allowlist. Entries that fail to resolve at compile time are silently skipped
// (no match possible) — same behavior as the previous per-call resolution.
type Runner struct {
	Allowlist   []string
	EnvPasslist []string
	Timeout     time.Duration
	Logger      *log.Logger

	// canonOnce guards a single call to compileAllowlist.
	canonOnce sync.Once
	// canonAllowlist holds the resolved real paths of Allowlist entries.
	// Populated by compileAllowlist on first Run call; read-only afterwards.
	canonAllowlist []string
}

// New constructs a Runner. All fields are set; none are optional here.
func New(allowlist []string, envPasslist []string, timeout time.Duration, logger *log.Logger) *Runner {
	return &Runner{
		Allowlist:   allowlist,
		EnvPasslist: envPasslist,
		Timeout:     timeout,
		Logger:      logger,
	}
}

// Run executes path with args under the safety envelope.
//
// Security rules enforced (each independently tested):
//  1. Empty allowlist → error; no exec attempted.
//  2. Non-absolute path → error.
//  3. Path not in allowlist (canonical real-path match via EvalSymlinks) → error.
//  4. No shell: exec.CommandContext(ctx, path, args...) directly; args are argv, not shell input.
//  5. Env scrub: child inherits ONLY EnvPasslist hits + extraEnv; os.Environ() never forwarded.
//  6. Timeout: context.WithTimeout wraps ctx when r.Timeout>0; deadline kills the process group.
//  7. Non-zero exit: error includes exit code and stderr tail.
//
// Allowlist matching uses filepath.EvalSymlinks to resolve both the requested
// path and each allowlist entry to their canonical real paths before comparing.
// If EvalSymlinks fails (path does not exist or is unresolvable) the call is
// rejected with an error — there is NO fallback to lexical matching (fail closed).
//
// Residual TOCTOU note: the canonicality check and the exec are not atomic.
// A root-race could swap the file between the EvalSymlinks call and exec.
// Operator requirement: allowlisted binaries must be root-owned and not writable
// by untrusted users. This runner provides defense-in-depth, not a sandbox.
//
// stdout is returned as a string. stderr is captured for error reporting only.
func (r *Runner) Run(ctx context.Context, path string, args []string, extraEnv map[string]string) (stdout string, err error) {
	// Rule 1: empty allowlist → inert, never exec.
	if len(r.Allowlist) == 0 {
		return "", errors.New("external_command: no allowlist configured (hook inert)")
	}

	// Rule 2: must be absolute (cheap pre-check before EvalSymlinks).
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("external_command: path %q is not absolute", path)
	}

	// Rule 3: canonical real-path allowlist check.
	//
	// EvalSymlinks is authoritative: if the path does not exist or any symlink
	// in the chain is unresolvable, reject immediately (fail closed).
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("external_command: path %q cannot be resolved (not found or broken symlink): %w", path, err)
	}
	realPath = filepath.Clean(realPath)

	if !r.isAllowlisted(realPath) {
		return "", fmt.Errorf("external_command: resolved path %q (from %q) is not allowlisted", realPath, path)
	}

	// Rule 6: apply per-call timeout if configured.
	runCtx := ctx
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	// Rule 4: no shell — direct exec, args are discrete argv entries.
	// #nosec G204 — path is allowlist-validated via EvalSymlinks canonicalization above;
	// args are passed as argv (not shell input), so shell-injection is not possible.
	cmd := exec.CommandContext(runCtx, realPath, args...)

	// Place the child in its own process group so a timeout/cancel kills the
	// entire process tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Kill the whole process group (negative PID) when the context is done, so
	// descendants do not linger. cmd.Cancel is invoked by CommandContext only
	// after Start has set cmd.Process, from a single exec-owned goroutine, so it
	// reads cmd.Process race-free — unlike a hand-rolled watchdog that races the
	// Start that writes cmd.Process.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Stdout/Stderr are bytes.Buffers, so os/exec copies them through pipes
	// and Wait blocks until every writer of the pipe closes it. A child that
	// leaves the process group (setsid/nohup inside the hook script) survives
	// the group SIGKILL while holding the inherited stdout descriptor, which
	// would block cmd.Run past the deadline indefinitely. WaitDelay abandons
	// the pipe copy shortly after Cancel so a detached grandchild cannot
	// wedge the CPI request the hook runs inside.
	cmd.WaitDelay = 5 * time.Second

	// Rule 5: build env from scratch; never inherit os.Environ().
	cmd.Env = r.buildEnv(extraEnv)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Args are operator-configured and static, but they are a plausible place
	// to pass a token or a credential-bearing URL — scrub each before logging,
	// consistent with the env treatment beside it (key names only).
	scrubbedArgs := make([]string, len(args))
	for i, a := range args {
		scrubbedArgs[i] = log.ScrubMessage(a)
	}
	r.debugf("exec: running %q args=%v env_keys=%v", realPath, scrubbedArgs, r.envKeys(cmd.Env))

	runErr := cmd.Run()
	stdoutStr := stdoutBuf.String()
	stderrStr := stderrBuf.String()

	if runErr != nil {
		// Rule 6: surface deadline distinctly.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("external_command: %q timed out after %v: %w", realPath, r.Timeout, context.DeadlineExceeded)
		}
		// Rule 7: include exit code and stderr tail.
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		stderrTailStr := stderrTail(stderrStr, 512)
		return "", fmt.Errorf("external_command: %q exited %d: %s", realPath, exitCode, stderrTailStr)
	}

	r.debugf("exec: %q succeeded stdout_len=%d", realPath, len(stdoutStr))
	return stdoutStr, nil
}

// compileAllowlist resolves every Allowlist entry via filepath.EvalSymlinks
// and stores the canonical paths in canonAllowlist. Called exactly once via
// canonOnce on the first Run invocation. Entries that fail EvalSymlinks are
// omitted (they cannot match any real path — same behavior as the previous
// per-call resolution). The result is immutable after this call returns.
func (r *Runner) compileAllowlist() {
	canon := make([]string, 0, len(r.Allowlist))
	for _, a := range r.Allowlist {
		realA, err := filepath.EvalSymlinks(a)
		if err != nil {
			// Entry does not resolve on disk; skip rather than match.
			continue
		}
		canon = append(canon, filepath.Clean(realA))
	}
	r.canonAllowlist = canon
}

// isAllowlisted returns true iff realPath (already EvalSymlinks+Clean resolved)
// matches the canonical real path of any allowlist entry. The allowlist is
// compiled (EvalSymlinks resolved) exactly once on the first call and cached;
// a post-construction symlink swap does not affect the effective allowlist.
func (r *Runner) isAllowlisted(realPath string) bool {
	r.canonOnce.Do(r.compileAllowlist)
	for _, canon := range r.canonAllowlist {
		if canon == realPath {
			return true
		}
	}
	return false
}

// buildEnv builds the child process environment from scratch.
// EnvPasslist entries present in os.Environ are included.
// extraEnv entries are appended (caller-supplied, e.g. CPI_VMID, CPI_METHOD).
// os.Environ() is never included wholesale.
func (r *Runner) buildEnv(extraEnv map[string]string) []string {
	env := make([]string, 0, len(r.EnvPasslist)+len(extraEnv))
	for _, name := range r.EnvPasslist {
		if val, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+val)
		}
	}
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	return env
}

// envKeys extracts variable names from a KEY=VALUE env slice for logging.
func (r *Runner) envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, e := range env {
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			keys = append(keys, e[:idx])
		}
	}
	return keys
}

// debugf logs at debug level when Logger is non-nil.
func (r *Runner) debugf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger.Debug(fmt.Sprintf(format, args...))
	}
}

// stderrTail returns the last n bytes of s, trimmed of leading/trailing whitespace.
func stderrTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
