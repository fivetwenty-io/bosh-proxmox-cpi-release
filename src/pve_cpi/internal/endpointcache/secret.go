package endpointcache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// secretFile holds the directory secret, and secretLockFile serializes its
	// creation and replacement. The sweep never removes either.
	secretFile     = "secret"
	secretLockFile = "secret.lock"
	// secretBytes is the exact length of the directory secret.
	secretBytes = 32
	// secretLockWait bounds how long a process waits for another process that
	// is creating or replacing the secret. Writing it is one small write, so
	// the wait runs out only on a badly stalled filesystem, and the process
	// then runs without a cache rather than delaying the call.
	secretLockWait = time.Second

	// redactedMarker is what every formatting of key material prints.
	redactedMarker = "[redacted]"
)

// errSecretLength means the directory secret passed the private-file checks
// but is not exactly 32 bytes long. loadSecret replaces such a secret, so the
// error never leaves this package.
var errSecretLength = privatefile.Sentinel("endpoint cache: directory secret has the wrong length",
	privatefile.ErrCorrupt)

// ErrSecretBusy means another process held secret.lock for longer than the
// bounded wait. Nothing was created, and the caller runs without a cache.
var ErrSecretBusy = errors.New("endpoint cache: directory secret lock is busy")

// secretPolicy reads and writes the directory secret. Its size limit is the
// exact secret length, so WriteFile refuses anything longer and ReadFile
// refuses an oversized secret as errSecretLength.
var secretPolicy = func() privatefile.Policy {
	p := cachePrivatePolicy
	p.Corrupt = errSecretLength
	p.MaxBytes = secretBytes
	return p
}()

// secretOps are the steps the secret is written with. They are the durable
// ones, unlike the cache files'. A torn cache file is rebuilt by the next
// discovery, but every file name comes from the secret, so a crash right after
// an unsynced rename would orphan every file each time it happened.
var secretOps = privatefile.DefaultOps()

// keyMaterial holds secret bytes: the directory secret or a key derived from
// it. Every way fmt, slog, or a text encoder can print it prints "[redacted]",
// so the bytes cannot reach a log line by accident.
type keyMaterial []byte

// String returns the redaction marker.
func (keyMaterial) String() string { return redactedMarker }

// GoString returns the redaction marker.
func (keyMaterial) GoString() string { return redactedMarker }

// Format prints the redaction marker for every verb.
func (keyMaterial) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedMarker) }

// LogValue returns the redaction marker.
func (keyMaterial) LogValue() slog.Value { return slog.StringValue(redactedMarker) }

// MarshalText returns the redaction marker.
func (keyMaterial) MarshalText() ([]byte, error) { return []byte(redactedMarker), nil }

// secretOutcome says what loadSecret did to the secret file.
type secretOutcome int

const (
	// secretRead means the secret was already there and valid.
	secretRead secretOutcome = iota
	// secretCreated means the secret was missing and this call wrote it.
	secretCreated
	// secretReplaced means the secret had the wrong length and this call
	// replaced it.
	secretReplaced
)

// loadSecret returns the directory secret of r. When the secret is missing,
// or passes the private-file checks with the wrong length, the first process
// to see that takes an exclusive lock on secret.lock, reads the secret again,
// and writes 32 new bytes through the private temporary file and rename with
// ops, while every other process reads what it wrote. Replacing a short or
// long secret orphans every cache file in the directory, which costs one
// discovery, while refusing it would turn the cache off for good, and anyone
// who can write the directory could already delete the secret instead.
//
// A secret with the wrong owner, mode, type, or link count is never replaced,
// because someone else may control it. Its error names the secret's path, and
// removing that file brings the cache back.
func loadSecret(ctx context.Context, r *os.Root, wait time.Duration, ops privatefile.Ops) (
	keyMaterial, secretOutcome, error,
) {
	secret, err := readSecret(r)
	if err == nil || !replaceable(err) {
		return secret, secretRead, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	lock, err := waitHeldLock(waitCtx, r, secretLockFile)
	if err != nil {
		return nil, secretRead, lockWaitError(ctx, err, ErrSecretBusy)
	}
	secret, outcome, err := writeSecretLocked(r, ops)
	if closeErr := lock.Close(); closeErr != nil {
		clear(secret)
		return nil, secretRead, errors.Join(err, fmt.Errorf("%s: release directory secret lock: %w", cacheLabel, closeErr))
	}
	return secret, outcome, err
}

// writeSecretLocked reads the secret again under secret.lock, because another
// process may have written it while this one waited, and writes a new one
// when it is still missing or the wrong length.
func writeSecretLocked(r *os.Root, ops privatefile.Ops) (keyMaterial, secretOutcome, error) {
	secret, err := readSecret(r)
	if err == nil || !replaceable(err) {
		return secret, secretRead, err
	}
	outcome := secretCreated
	if errors.Is(err, errSecretLength) {
		outcome = secretReplaced
	}
	secret = make(keyMaterial, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, secretRead, fmt.Errorf("%s: generate directory secret: %w", cacheLabel, err)
	}
	if err := secretPolicy.WriteFile(r, secretFile, secret, ops); err != nil {
		clear(secret)
		return nil, secretRead, fmt.Errorf("%s: write directory secret %s: %w", cacheLabel, secretPath(r), err)
	}
	return secret, outcome, nil
}

// replaceable reports whether a readSecret error leaves the secret to be
// written, because it is missing or its only fault is its length.
func replaceable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSecretLength)
}

// readSecret reads the directory secret through the private-file checks, which
// refuse another owner, group or other access, a file that is not regular,
// a symlink, and a second link. An absent secret comes back as an error that
// matches fs.ErrNotExist, and a secret of the wrong length as one that matches
// errSecretLength. Every other error names the secret's path.
func readSecret(r *os.Root) (keyMaterial, error) {
	data, err := secretPolicy.ReadFile(r, secretFile, secretBytes)
	switch {
	case err == nil && len(data) != secretBytes:
		return nil, fmt.Errorf("%w: %d bytes, want %d", errSecretLength, len(data), secretBytes)
	case err == nil:
		return keyMaterial(data), nil
	case replaceable(err):
		return nil, err
	}
	return nil, fmt.Errorf("%s: refusing directory secret %s: %w", cacheLabel, secretPath(r), err)
}

// secretPath is the secret's full path, which is not sensitive and tells an
// operator which file to remove.
func secretPath(r *os.Root) string { return filepath.Join(r.Name(), secretFile) }
