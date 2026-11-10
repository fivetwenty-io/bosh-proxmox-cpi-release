package endpointcache

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// fileVersion is the cache file's envelope schema version. It versions on
	// its own schedule, independent of the journal's.
	fileVersion = 1
	// maxFileBytes bounds a cache file on read and on write.
	maxFileBytes = 64 << 10
	// integrityKey is the envelope field that holds the payload's HMAC.
	integrityKey = "hmac_sha256"
	// writeLockWait bounds how long a writer waits for the key's lock, as the
	// journal's entry binding does. A write is one small read and one atomic
	// write, so the wait runs out only when another writer is badly stalled,
	// and then this process skips its write rather than delaying the call.
	writeLockWait = time.Second
)

// ErrFileInvalid means a cache file is oversized, malformed, fails its HMAC,
// carries an unknown field, or holds a key other than the reader's. A reader
// treats the file as missing, and the next successful write replaces it.
var ErrFileInvalid = privatefile.Sentinel("endpoint cache: invalid file", privatefile.ErrCorrupt)

// ErrKeyMismatch means a cache file's HMAC is valid but its payload names a
// different key, such as another endpoint list that shares the credential and
// the file name. It matches ErrFileInvalid.
var ErrKeyMismatch = privatefile.Sentinel("endpoint cache: file key does not match", ErrFileInvalid)

// ErrUnsupportedVersion means a cache file carries an envelope version this
// release does not read. It matches ErrFileInvalid, so the file is treated as
// missing and rebuilt, which a cache can afford.
var ErrUnsupportedVersion = privatefile.Sentinel("endpoint cache: unsupported file version", ErrFileInvalid)

// errLockBusy means the key's lock stayed busy past writeLockWait.
var errLockBusy = errors.New("endpoint cache: cache file lock is busy")

// StoreOptions are the caller's choices for a Store.
type StoreOptions struct {
	// Logger receives the line for a skipped write. Nil discards it.
	Logger *log.Logger
	// ExplicitOn is whether the configured discovery mode is "on", which
	// raises the skipped-write line from Debug to Warn.
	ExplicitOn bool
}

// Store reads and writes the cache file of one key in one cache directory.
// Each file is an envelope whose integrity field is an HMAC-SHA256 keyed from
// the credential and the directory secret, so someone who can write the
// directory but does not hold the current credential cannot forge a file, and
// rotating the credential invalidates every file. A Store holds no key
// material in a printable field. Read and Update are safe for concurrent use,
// and Close must not run alongside them.
type Store struct {
	root       *os.Root
	key        Key
	name       string
	policy     privatefile.Policy
	ops        privatefile.Ops
	lockWait   time.Duration
	logger     *log.Logger
	explicitOn bool
}

// OpenStore opens the cache directory dir, which must be absolute, a real
// directory, owned by the effective user, and closed to group and other. It
// reads the directory secret, creating it under secret.lock when it is
// missing, and derives the key for cfg and the HMAC key for its file from the
// credential, which is the entry's token secret or password. Any error means
// the caller runs without a cache. The caller closes the Store.
func OpenStore(ctx context.Context, dir string, cfg KeyConfig, credential string, opts StoreOptions) (*Store, error) {
	if credential == "" {
		return nil, fmt.Errorf("%w: no credential", ErrInvalidKey)
	}
	if _, err := normalizeEndpoints(cfg.Endpoints); err != nil {
		return nil, err
	}
	root, err := cachePrivatePolicy.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	secret, _, err := loadSecret(ctx, root, secretLockWait)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	key, macKey, err := deriveKeys(cfg, secret, credential)
	clear(secret)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}
	policy := cachePrivatePolicy
	policy.Corrupt = ErrFileInvalid
	policy.MaxBytes = maxFileBytes
	policy.IntegrityKey = integrityKey
	policy.Integrity = hmacIntegrity(macKey)
	return &Store{
		root:   root,
		key:    key,
		name:   key.FileName(),
		policy: policy,
		// Cache writes skip fsync. The HMAC turns a file torn by a crash into
		// "no cache", and the next discovery rebuilds it.
		ops:        privatefile.NoSyncOps(),
		lockWait:   writeLockWait,
		logger:     logger,
		explicitOn: opts.ExplicitOn,
	}, nil
}

// hmacIntegrity returns the envelope integrity function for key: the lowercase
// hex HMAC-SHA256 of the payload. The envelope reader compares it with the
// stored value in constant time.
func hmacIntegrity(key keyMaterial) func([]byte) string {
	return func(payload []byte) string {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(payload)
		return hex.EncodeToString(mac.Sum(nil))
	}
}

// Key returns the store's key, which every payload it reads or writes must
// hold.
func (s *Store) Key() Key {
	key := s.key
	key.Endpoints = append([]string(nil), s.key.Endpoints...)
	return key
}

// FileName returns the name of the store's cache file in its directory.
func (s *Store) FileName() string { return s.name }

// Close closes the cache directory. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}

// Read reads the store's file and strictly decodes its payload into out, which
// must be a non-nil pointer. Readers take no lock: the read goes through the
// private-file checks and retries a file replaced by a concurrent rename. An
// absent file returns an error matching fs.ErrNotExist. A file that fails the
// envelope version, the HMAC, the key match, or the strict decode returns an
// error matching ErrFileInvalid, and the caller treats it as missing. A file
// with the wrong owner, mode, type, or link count, or a symlink, returns the
// private-file refusal, and the caller runs without a cache.
func (s *Store) Read(out any) error {
	if s == nil || s.root == nil {
		return fmt.Errorf("%s: store is closed", cacheLabel)
	}
	if v := reflect.ValueOf(out); !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("%s: read needs a non-nil pointer, got %T", cacheLabel, out)
	}
	env, err := s.policy.ReadEnvelope(s.root, s.name)
	if err != nil {
		return err
	}
	if env.Version != fileVersion {
		return fmt.Errorf("%w: version %d", ErrUnsupportedVersion, env.Version)
	}
	var payload json.RawMessage
	if err := s.policy.DecodeEnvelope(env, fileVersion, &payload); err != nil {
		return err
	}
	if err := s.checkKey(payload); err != nil {
		return err
	}
	if err := privatefile.StrictDecode(payload, out); err != nil {
		return fmt.Errorf("%w: malformed payload: %w", ErrFileInvalid, err)
	}
	return nil
}

// checkKey refuses a payload whose "key" member is missing or differs from the
// store's key.
func (s *Store) checkKey(payload json.RawMessage) error {
	var head struct {
		Key *Key `json:"key"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return fmt.Errorf("%w: malformed payload: %w", ErrFileInvalid, err)
	}
	if head.Key == nil || !head.Key.Equal(s.key) {
		return ErrKeyMismatch
	}
	return nil
}

// Update reads, merges, and writes the store's file under an exclusive lock on
// the key's lock file. It waits at most one second for the lock. When the wait
// runs out, it skips the write, logs at Debug, or at Warn under an explicit
// "on", and returns false with no error, without calling merge.
//
// Under the lock, merge receives the current payload, or nil when the file is
// absent or invalid, and returns the payload to write, or nil to write
// nothing. The payload's key must equal the store's key. The write replaces
// the file atomically through a temporary file and a rename, without fsync.
// written reports whether the file was replaced.
//
// An error comes back when the caller's context ends during the wait, when
// the lock file or the cache file fails the private-file checks, when merge
// fails, or when the write fails. A failed write leaves the previous file or
// none, never a torn one. The caller logs these errors, because the store logs
// only the skipped write.
func Update[T any](ctx context.Context, s *Store, merge func(current *T) (*T, error)) (written bool, err error) {
	if s == nil || s.root == nil {
		return false, fmt.Errorf("%s: store is closed", cacheLabel)
	}
	if merge == nil {
		return false, fmt.Errorf("%s: update needs a merge function", cacheLabel)
	}
	lock, err := s.lock(ctx)
	if errors.Is(err, errLockBusy) {
		s.logger.LogAttrs(ctx, Level(s.explicitOn), "endpoint cache lock wait expired; skipping the cache write",
			log.String("file", s.name), log.String("wait", s.lockWait.String()))
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: release cache file lock: %w", cacheLabel, closeErr))
		}
	}()

	var current *T
	var stored T
	switch readErr := s.Read(&stored); {
	case readErr == nil:
		current = &stored
	case errors.Is(readErr, fs.ErrNotExist), errors.Is(readErr, ErrFileInvalid):
		current = nil
	default:
		return false, readErr
	}
	next, err := merge(current)
	if err != nil || next == nil {
		return false, err
	}
	if err := s.write(next); err != nil {
		return false, err
	}
	return true, nil
}

// lock takes the key's lock within the store's bounded wait.
func (s *Store) lock(ctx context.Context) (*privatefile.Lock, error) {
	waitCtx, cancel := context.WithTimeout(ctx, s.lockWait)
	defer cancel()
	lock, err := waitCreatedLock(waitCtx, s.root, s.key.LockName())
	if err != nil {
		return nil, lockWaitError(ctx, err, errLockBusy)
	}
	return lock, nil
}

// write marshals value, refuses it unless its key is the store's, and replaces
// the file with it. The caller holds the key's lock.
func (s *Store) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: marshal payload: %w", cacheLabel, err)
	}
	if err := s.checkKey(payload); err != nil {
		return fmt.Errorf("%s: refusing to write a payload for another key: %w", cacheLabel, err)
	}
	return s.policy.WriteEnvelope(s.root, s.name, fileVersion, json.RawMessage(payload), s.ops)
}
