// Package privatefile holds the filesystem rules for files that only the
// running user may read or write. It checks ownership, mode, file type, and
// link count; opens files without following symlinks; reads and writes a
// versioned JSON envelope whose integrity check the caller chooses; and takes
// advisory file locks. The allocation journal uses it with its plain checksum
// and every fsync, and other stores can use it with a keyed check and no
// fsync.
package privatefile

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// Unsafe path causes. An UnsafePathError wraps exactly one of them, so a
// caller can tell an ownership mismatch, which a rerun as the owner fixes,
// from a mode or file type that only the operator can repair. A caller that
// wants its own wording gives its Policy sentinels made with Sentinel.
var (
	ErrUnsafeOwnership = errors.New("private file: unsafe ownership")
	ErrUnsafeMode      = errors.New("private file: unsafe permissions")
	ErrUnsafeFileType  = errors.New("private file: unsafe file type")
)

// ErrChanged is the default error for a file whose inode was replaced between
// the lstat and the fstat inside an open. A rerun of the open normally sees
// the new file.
var ErrChanged = errors.New("private file replaced")

// ErrCorrupt is the default error for an envelope that is oversized,
// malformed, of the wrong version, or fails its integrity check.
var ErrCorrupt = errors.New("invalid private file")

type sentinel struct {
	text string
	base error
}

func (s *sentinel) Error() string { return s.text }
func (s *sentinel) Unwrap() error { return s.base }

// Sentinel returns an error that prints text and matches base under
// errors.Is. A caller uses it to give one of this package's causes its own
// wording while every check here still recognizes the cause.
func Sentinel(text string, base error) error { return &sentinel{text: text, base: base} }

// SHA256Hex is the plain integrity function: the lowercase hex SHA-256 of the
// payload.
func SHA256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Policy holds what differs between the stores that share these helpers. The
// zero value is not usable, because a store must name its size limit and its
// integrity function.
type Policy struct {
	// Label prefixes the fixed error texts, as in "<Label>: directory must be
	// absolute". An empty label reads "private file".
	Label string
	// Ownership, Mode, and FileType are the causes an UnsafePathError carries.
	// A nil one uses the package's own error of the same name. A caller's should
	// match it under errors.Is, which Sentinel guarantees, and one that does not
	// is joined with it when PrivateInfo reports it.
	Ownership, Mode, FileType error
	// Changed is wrapped around the error for a file replaced during an open,
	// and a read retries on it. Nil uses ErrChanged.
	Changed error
	// Corrupt is wrapped around every envelope read failure that blames the
	// file's content. Nil uses ErrCorrupt.
	Corrupt error
	// MaxBytes is the largest envelope a read accepts and a write produces.
	MaxBytes int
	// IntegrityKey is the JSON key that holds the integrity string in the
	// envelope, such as "sha256" or "hmac_sha256".
	IntegrityKey string
	// Integrity computes the integrity string of a payload. It is a plain
	// checksum for one store and a keyed MAC for another.
	Integrity func(payload []byte) string
	// NewID returns a unique name fragment for a temporary file. Nil uses 128
	// random bits in hex.
	NewID func() (string, error)
	// EUID returns the user ID that must own a private path. Nil uses the
	// process's effective user ID, which is the only value production code
	// sets. A test sets it to show that a caller's checks refuse a path that
	// another user owns, which an ordinary user cannot create.
	EUID func() int
}

var integrityKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkSize refuses a Policy with no size limit.
func (p Policy) checkSize() error {
	if p.MaxBytes <= 0 {
		return fmt.Errorf("%s: size limit not set", p.label())
	}
	return nil
}

// check refuses a Policy that cannot read or write an envelope.
func (p Policy) check() error {
	if err := p.checkSize(); err != nil {
		return err
	}
	switch {
	case p.Integrity == nil:
		return fmt.Errorf("%s: integrity function not set", p.label())
	case !integrityKeyPattern.MatchString(p.IntegrityKey) || p.IntegrityKey == "version" || p.IntegrityKey == "payload":
		return fmt.Errorf("%s: invalid integrity key %q", p.label(), p.IntegrityKey)
	}
	return nil
}

func (p Policy) label() string {
	if p.Label == "" {
		return "private file"
	}
	return p.Label
}

func (p Policy) ownership() error { return matching(p.Ownership, ErrUnsafeOwnership) }
func (p Policy) mode() error      { return matching(p.Mode, ErrUnsafeMode) }
func (p Policy) fileType() error  { return matching(p.FileType, ErrUnsafeFileType) }
func (p Policy) changed() error   { return firstError(p.Changed, ErrChanged) }
func (p Policy) corrupt() error   { return firstError(p.Corrupt, ErrCorrupt) }

func (p Policy) newID() (string, error) {
	if p.NewID != nil {
		return p.NewID()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("%s: generate temporary name: %w", p.label(), err)
	}
	return hex.EncodeToString(b[:]), nil
}

// joined prints cause's text and matches both cause and base under errors.Is.
type joined struct {
	cause, base error
}

func (j *joined) Error() string   { return j.cause.Error() }
func (j *joined) Unwrap() []error { return []error{j.cause, j.base} }

// matching returns cause, or base when cause is nil. A cause that does not
// already match base under errors.Is is joined with it, so a Policy built
// without Sentinel is still classified correctly and keeps its own wording.
func matching(cause, base error) error {
	switch {
	case cause == nil:
		return base
	case errors.Is(cause, base):
		return cause
	}
	return &joined{cause: cause, base: base}
}

func firstError(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
