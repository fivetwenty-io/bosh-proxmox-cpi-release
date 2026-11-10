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
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// cacheLabel prefixes the fixed error texts of the private-file helpers.
	cacheLabel = "endpoint cache"

	// secretFile holds the directory secret, and secretLockFile serializes its
	// creation. The sweep never removes either.
	secretFile     = "secret"
	secretLockFile = "secret.lock"
	// secretBytes is the exact length of the directory secret.
	secretBytes = 32
	// secretLockWait bounds how long a process waits for another process that
	// is creating the secret. Creating it is one small write, so the wait runs
	// out only on a badly stalled filesystem, and the process then runs
	// without a cache rather than delaying the call.
	secretLockWait = time.Second

	// redactedMarker is what every formatting of key material prints.
	redactedMarker = "[redacted]"
)

// The unsafe path causes carry this package's wording and match the
// privatefile causes under errors.Is.
var (
	errCacheOwnership = privatefile.Sentinel("endpoint cache: unsafe ownership", privatefile.ErrUnsafeOwnership)
	errCacheMode      = privatefile.Sentinel("endpoint cache: unsafe permissions", privatefile.ErrUnsafeMode)
	errCacheFileType  = privatefile.Sentinel("endpoint cache: unsafe file type", privatefile.ErrUnsafeFileType)
)

// ErrSecretInvalid means the directory secret exists but is not exactly 32
// bytes long. The secret is never regenerated over a bad one, because a
// replaced secret would silently orphan every file and hide the damage, so the
// cache stays unusable until an operator removes the directory.
var ErrSecretInvalid = privatefile.Sentinel("endpoint cache: invalid directory secret", privatefile.ErrCorrupt)

// ErrSecretBusy means another process held secret.lock for longer than the
// bounded wait. Nothing was created, and the caller runs without a cache.
var ErrSecretBusy = errors.New("endpoint cache: directory secret lock is busy")

// ErrLockWaitAbandoned means the caller's context ended while a cache lock
// wait was running. The returned error also wraps the context's own error.
var ErrLockWaitAbandoned = errors.New("endpoint cache: lock wait ended with the caller's context")

// cachePrivatePolicy carries this package's label and unsafe path causes. It
// opens directories and lock files, and the secret and cache file policies
// extend it with their size limits and checks.
var cachePrivatePolicy = privatefile.Policy{
	Label:     cacheLabel,
	Ownership: errCacheOwnership,
	Mode:      errCacheMode,
	FileType:  errCacheFileType,
}

// secretPolicy reads and writes the directory secret. Its size limit is the
// exact secret length, so WriteFile refuses anything longer and ReadFile
// refuses an oversized secret as ErrSecretInvalid.
var secretPolicy = func() privatefile.Policy {
	p := cachePrivatePolicy
	p.Corrupt = ErrSecretInvalid
	p.MaxBytes = secretBytes
	return p
}()

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

// loadSecret returns the directory secret of r, creating it when it is
// missing. The first process that finds it missing creates it under an
// exclusive lock on secret.lock, through the private temporary file and
// rename, and every other process reads it. created reports whether this call
// wrote it. A secret that exists but fails the private-file checks or the
// length check is returned as an error and never replaced.
func loadSecret(ctx context.Context, r *os.Root, wait time.Duration) (secret keyMaterial, created bool, err error) {
	secret, err = readSecret(r)
	if !errors.Is(err, fs.ErrNotExist) {
		return secret, false, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	lock, err := waitCreatedLock(waitCtx, r, secretLockFile)
	if err != nil {
		return nil, false, lockWaitError(ctx, err, ErrSecretBusy)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			secret, created = nil, false
			err = errors.Join(err, fmt.Errorf("endpoint cache: release directory secret lock: %w", closeErr))
		}
	}()
	// Another process may have created the secret while this one waited.
	secret, err = readSecret(r)
	if !errors.Is(err, fs.ErrNotExist) {
		return secret, false, err
	}
	secret = make(keyMaterial, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, false, fmt.Errorf("endpoint cache: generate directory secret: %w", err)
	}
	// The secret is written durably, unlike the cache files. A torn cache file
	// is rebuilt by the next discovery, but a torn secret is never replaced, so
	// a crash right after an unsynced rename would disable the cache for good.
	if err := secretPolicy.WriteFile(r, secretFile, secret, privatefile.DefaultOps()); err != nil {
		return nil, false, fmt.Errorf("endpoint cache: create directory secret: %w", err)
	}
	return secret, true, nil
}

// readSecret reads the directory secret through the private-file checks, which
// refuse another owner, group or other access, a file that is not regular,
// a symlink, and a second link. An absent secret comes back as an error that
// matches fs.ErrNotExist.
func readSecret(r *os.Root) (keyMaterial, error) {
	data, err := secretPolicy.ReadFile(r, secretFile, secretBytes)
	if err != nil {
		return nil, err
	}
	if len(data) != secretBytes {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrSecretInvalid, len(data), secretBytes)
	}
	return keyMaterial(data), nil
}

// waitCreatedLock waits for the lock file name, which whichever process first
// needs it creates, so several processes can race to create it. macOS can fail
// the loser's O_CREATE open with ENOENT while the winner's create is in
// flight, and the file exists a moment later, so that error is retried a few
// times within the same deadline. A directory that is really gone keeps
// failing and the error is returned.
func waitCreatedLock(ctx context.Context, r *os.Root, name string) (*privatefile.Lock, error) {
	const createRaceRetries = 5
	for attempt := 0; ; attempt++ {
		lock, err := cachePrivatePolicy.WaitLock(ctx, r, name)
		if !errors.Is(err, fs.ErrNotExist) || attempt == createRaceRetries {
			return lock, err
		}
		if err := privatefile.Pause(ctx); err != nil {
			return nil, err
		}
	}
}

// lockWaitError classifies an error from a bounded lock wait. A wait that ended
// because the caller's own context ended reports ErrLockWaitAbandoned with the
// context's error, a wait that ran out its own bound reports busy, and any
// other error, such as an unsafe lock file, is returned as it is.
func lockWaitError(parent context.Context, err, busy error) error {
	waitEnded := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	switch parentErr := parent.Err(); {
	case waitEnded && parentErr != nil:
		return fmt.Errorf("%w: %w", ErrLockWaitAbandoned, parentErr)
	case waitEnded:
		return busy
	}
	return err
}
