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

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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
	// ConfiguredMode is the configured discovery mode, from
	// CPIConfig.DiscoveryModeValue and not the effective mode. Under
	// DiscoveryModeOn the skipped-write line is a Warn, and under every other
	// mode a Debug, as Level decides.
	ConfiguredMode config.DiscoveryMode
	// FilePrefix, Label, and Version select another kind of file that shares
	// the directory, such as the HA profile cache. All three empty selects the
	// endpoint cache file, with the same names, label, and version as before.
	// FilePrefix must be one the sweep knows, such as HAProfilePrefix, and it
	// starts the file's name and its lock names. Label starts the HKDF info of
	// the file's HMAC key, so a file of one kind never verifies as another's,
	// and it must be a lowercase label other than the endpoint cache's. Version
	// is the envelope schema version, and zero means 1. A prefix needs a label
	// and a label needs a prefix. The payload must carry the store's key in its
	// "key" member, as the endpoint payload does.
	FilePrefix string
	Label      string
	Version    int
	// DisplayName is what the store's messages call its file, its lock files,
	// its directory secret, and its directory, such as HAProfileName. Empty
	// means "endpoint cache", which keeps every message of the endpoint store
	// as it has always read. The endpoint cache file keeps that name, so a
	// DisplayName needs a FilePrefix to say anything else. It must be words of
	// letters and digits at most 64 characters long.
	DisplayName string
}

// Store reads and writes the cache file of one key in one cache directory.
// Each file is an envelope whose integrity field is an HMAC-SHA256 keyed from
// the credential and the directory secret, so someone who can write the
// directory but does not hold the current credential cannot forge a file, and
// rotating the credential invalidates every file. A Store holds no key
// material in a printable field. Read, Update, and TryLockDiscovery are safe
// for concurrent use, and Close must not run alongside them.
type Store struct {
	root           *os.Root
	key            Key
	kind           fileKind
	name           string
	policy         privatefile.Policy
	ops            privatefile.Ops
	lockWait       time.Duration
	logger         *log.Logger
	configuredMode config.DiscoveryMode
	secretReplaced bool

	// text names the store in its messages and holds the causes they print. The
	// endpoint cache's are the package's own sentinels, and another kind of
	// store's print its name and match them under errors.Is.
	text *texts
}

// OpenStore opens the store for cfg on root, the cache directory that Resolve
// chose and opened, which Choice.Root holds. The Store owns root from the call
// on, whether OpenStore succeeds or fails: it closes root on every error and
// in Close, and the caller neither uses nor closes root, or its Choice, again.
// Because every file is reached through root, renaming or replacing the
// directory's path afterwards cannot redirect the store.
//
// OpenStore checks the opened directory again through the same private checks
// Resolve makes, so a root opened some other way meets the same refusals. It
// reads the directory secret, writing a new one under secret.lock when it is
// missing or has the wrong length, and derives the key for cfg and the HMAC
// key for its file from cfg.User and the credential, which is the entry's
// token secret or password. Any error means the caller runs without a cache.
// When the secret had the wrong length and this call replaced it,
// SecretReplaced reports so, and the caller logs that once at Warn whatever
// the mode. The caller closes the Store. StoreOptions.FilePrefix, Label, and
// Version select another kind of file in the same directory, such as the HA
// profile cache, and an invalid choice is refused with ErrInvalidFileKind.
func OpenStore(ctx context.Context, root *os.Root, cfg KeyConfig, credential string, opts StoreOptions) (*Store, error) {
	kind, err := kindFor(opts)
	if err != nil {
		if root != nil {
			err = errors.Join(err, root.Close())
		}
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("%s: no cache directory", kind.name)
	}
	return openStoreOfKind(ctx, root, cfg, credential, opts, kind)
}

// openStoreOfKind is OpenStore for a file kind that kindFor has already
// accepted or that a test supplies. Its root is not nil, because OpenStore
// checks that first and the test that calls it passes an opened directory.
func openStoreOfKind(
	ctx context.Context, root *os.Root, cfg KeyConfig, credential string, opts StoreOptions, kind fileKind,
) (*Store, error) {
	if err := checkKeyInputs(cfg, credential); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	text := textsFor(kind.name)
	if err := text.checkOpenedDirectory(root); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	secret, outcome, err := text.loadSecret(ctx, root, secretLockWait, secretOps)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	key, macKey, err := deriveKeysFor(cfg, secret, credential, kind)
	clear(secret)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}
	policy := text.policy
	policy.Corrupt = text.invalid
	policy.MaxBytes = maxFileBytes
	policy.IntegrityKey = integrityKey
	policy.Integrity = hmacIntegrity(macKey)
	return &Store{
		root:   root,
		key:    key,
		kind:   kind,
		name:   kind.fileName(key),
		policy: policy,
		// Cache writes skip fsync. The HMAC turns a file torn by a crash into
		// "no cache", and the next discovery rebuilds it.
		ops:            privatefile.NoSyncOps(),
		lockWait:       writeLockWait,
		logger:         logger,
		configuredMode: opts.ConfiguredMode,
		secretReplaced: outcome == secretReplaced,
		text:           text,
	}, nil
}

// checkKeyInputs refuses a config or credential that cannot form a key before
// the directory is touched, so a refused key creates no secret.
func checkKeyInputs(cfg KeyConfig, credential string) error {
	switch {
	case cfg.User == "":
		return fmt.Errorf("%w: no user or token ID", ErrInvalidKey)
	case credential == "":
		return fmt.Errorf("%w: no credential", ErrInvalidKey)
	}
	_, err := normalizeEndpoints(cfg.Endpoints)
	return err
}

// SecretReplaced reports whether OpenStore replaced a directory secret whose
// only fault was its length, and returns the secret's full path for the
// caller's Warn line. Every cache file written under the old secret is now
// orphaned and costs one discovery to rebuild.
func (s *Store) SecretReplaced() (path string, replaced bool) {
	if s == nil || s.root == nil || !s.secretReplaced {
		return "", false
	}
	return secretPath(s.root), true
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

// label is what the store's messages call its file.
func (s *Store) label() string { return s.texts().name }

// texts returns the store's message texts, and the endpoint cache's for a nil
// or zero store.
func (s *Store) texts() *texts {
	if s == nil || s.text == nil {
		return endpointTexts
	}
	return s.text
}

// Sweep removes what the cache left behind in the store's directory, as the
// package's Sweep does, and leaves the store's own file and lock alone. A store
// of another kind than the endpoint cache's removes only the files that carry
// its own prefix, and the stale temporary files, so it never removes an
// endpoint file or an endpoint lock, even for its own key. A writer calls it
// from the merge function it hands to Update, where it holds that lock, which
// is the cadence the endpoint cache's UpdatePayload sweeps at.
func (s *Store) Sweep(now time.Time) SweepResult {
	if s == nil || s.root == nil {
		return SweepResult{Err: fmt.Errorf("%s: store is closed", s.label())}
	}
	keep := []string{s.kind.lockName(s.key), s.kind.discoverLockName(s.key), s.name}
	return s.texts().sweepWith(s.root, now, keep, ownedByEffectiveUser, s.kind.prefix)
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
		return fmt.Errorf("%s: store is closed", s.label())
	}
	if v := reflect.ValueOf(out); !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("%s: read needs a non-nil pointer, got %T", s.label(), out)
	}
	env, err := s.policy.ReadEnvelope(s.root, s.name)
	if err != nil {
		return err
	}
	if env.Version != s.kind.version {
		return fmt.Errorf("%w: version %d", s.texts().unsupported, env.Version)
	}
	var payload json.RawMessage
	if err := s.policy.DecodeEnvelope(env, s.kind.version, &payload); err != nil {
		return err
	}
	if err := s.checkKey(payload); err != nil {
		return err
	}
	if err := privatefile.StrictDecode(payload, out); err != nil {
		return fmt.Errorf("%w: malformed payload: %w", s.texts().invalid, err)
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
		return fmt.Errorf("%w: malformed payload: %w", s.texts().invalid, err)
	}
	if head.Key == nil || !head.Key.Equal(s.key) {
		return s.texts().keyMismatch
	}
	return nil
}

// Update reads, merges, and writes the store's file under an exclusive lock on
// the key's lock file. It waits at most one second for the lock. When the wait
// runs out, it skips the write, logs at Debug, or at Warn under a configured
// "on", and returns false with no error, without calling merge. A lock is kept
// only while the lock file's name still reaches the locked inode, and taking
// it updates the lock file's times, as heldLock describes.
//
// Under the lock, merge receives the current payload, or nil when the file is
// absent or invalid, and returns the payload to write, or nil to write
// nothing. The payload's key must equal the store's key. The write replaces
// the file atomically through a temporary file and a rename, without fsync.
// written reports whether the file was replaced.
//
// The caller's context is checked once the lock is held and again after merge
// returns, so a caller whose context has ended, such as a worker the exit
// drain cancelled, writes nothing. A check that finds the context ended
// returns its error, wrapped.
//
// An error comes back when the caller's context ends, whether during the
// wait, before merge, or after it, when the lock file or the cache file fails
// the private-file checks, when the lock file keeps being replaced, which is
// ErrLockReplaced, when merge fails, or when the write fails. A failed write
// leaves the previous file or none, never a torn one. The caller logs these
// errors, because the store logs only the skipped write.
func Update[T any](ctx context.Context, s *Store, merge func(current *T) (*T, error)) (written bool, err error) {
	if s == nil || s.root == nil {
		return false, fmt.Errorf("%s: store is closed", s.label())
	}
	if merge == nil {
		return false, fmt.Errorf("%s: update needs a merge function", s.label())
	}
	lock, err := s.lock(ctx)
	if errors.Is(err, errLockBusy) {
		s.logger.LogAttrs(ctx, Level(s.configuredMode), s.label()+" lock wait expired; skipping the cache write",
			log.String("file", s.name), log.String("wait", s.lockWait.String()))
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: release cache file lock: %w", s.label(), closeErr))
		}
	}()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("%s: update abandoned with the caller's context: %w", s.label(), ctxErr)
	}

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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("%s: write abandoned with the caller's context: %w", s.label(), ctxErr)
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
	lock, err := s.texts().waitHeldLock(waitCtx, s.root, s.kind.lockName(s.key))
	if err != nil {
		return nil, s.texts().lockWaitError(ctx, err, s.texts().lockBusy)
	}
	return lock, nil
}

// TryLockDiscovery takes the key's discover lock without waiting, so only one
// process discovers for a key at a time. held is false, with no error, when
// another process holds it, and the caller skips its discovery. As with the
// write lock, a lock is kept only while the discover lock file's name still
// reaches the locked inode, and taking it updates the file's times. An error
// comes back when the store is closed, when the lock file fails the
// private-file checks, when it keeps being replaced, which is ErrLockReplaced,
// or when ctx ends during the short retry of a lock file being created. The
// caller closes a held lock when discovery ends.
func (s *Store) TryLockDiscovery(ctx context.Context) (lock *privatefile.Lock, held bool, err error) {
	if s == nil || s.root == nil {
		return nil, false, fmt.Errorf("%s: store is closed", s.label())
	}
	return s.texts().tryHeldLock(ctx, s.root, s.kind.discoverLockName(s.key))
}

// write marshals value, refuses it unless its key is the store's, and replaces
// the file with it. The caller holds the key's lock.
func (s *Store) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s: marshal payload: %w", s.label(), err)
	}
	if err := s.checkKey(payload); err != nil {
		return fmt.Errorf("%s: refusing to write a payload for another key: %w", s.label(), err)
	}
	return s.policy.WriteEnvelope(s.root, s.name, s.kind.version, json.RawMessage(payload), s.ops)
}
