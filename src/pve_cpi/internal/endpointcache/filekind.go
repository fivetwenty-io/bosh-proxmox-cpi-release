package endpointcache

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// endpointLabel is the HMAC label of the endpoint cache file. Another kind
	// of file must use a different one.
	endpointLabel = "pve-cpi endpoint-cache"

	// HAProfilePrefix is the file name prefix of the HA profile cache. The
	// file is named ha_profile-<sanitized host>-<digest>.json, and its lock
	// files carry the same prefix. The underscore is a character sanitizeHost
	// never emits, so no endpoint file name can start with this prefix.
	HAProfilePrefix = "ha_profile-"

	// maxFileLabel bounds a file kind's HMAC label.
	maxFileLabel = 64

	// defaultKindVersion is the envelope schema version of a file kind that
	// leaves StoreOptions.Version at zero. It is a literal 1 and does not follow
	// fileVersion, so a later bump of the endpoint cache's schema never bumps
	// another kind's by accident.
	defaultKindVersion = 1
)

// ErrInvalidFileKind means StoreOptions asked for a kind of cache file this
// package does not make. The caller runs without a cache.
var ErrInvalidFileKind = errors.New("endpoint cache: invalid file kind")

// knownFilePrefixes lists the name prefixes a file kind may use. The sweep
// knows exactly these, so a file of any other kind would never be removed.
var knownFilePrefixes = []string{HAProfilePrefix}

// filePrefixPattern is the shape of a file kind's name prefix, a lowercase
// token that ends in a hyphen.
var filePrefixPattern = regexp.MustCompile(`^[a-z0-9_]+(?:[.-][a-z0-9_]+)*-$`)

// validFilePrefix reports whether prefix is a fixed lowercase token ending in
// a hyphen that also holds a character sanitizeHost never emits. The second
// condition keeps an endpoint file for any host from starting with the prefix.
func validFilePrefix(prefix string) bool {
	if !filePrefixPattern.MatchString(prefix) {
		return false
	}
	return strings.IndexFunc(prefix, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-'
	}) >= 0
}

// fileLabelPattern is the shape of a file kind's HMAC label: lowercase words
// of letters and digits, joined by single spaces or hyphens.
var fileLabelPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[ -][a-z0-9]+)*$`)

// fileKind says which cache file a Store reads and writes and how its HMAC key
// is derived. The zero value is not usable, and endpointKind is the endpoint
// cache's.
type fileKind struct {
	// prefix starts the file's name and its lock names, and is empty for the
	// endpoint cache file.
	prefix string
	// name is what error and log text calls the file, from
	// StoreOptions.DisplayName. It is "endpoint cache" when that is empty.
	name string
	// label starts the HKDF info of the file's HMAC key.
	label string
	// version is the file's envelope schema version.
	version int
}

// endpointKind is the kind of the endpoint cache file.
var endpointKind = fileKind{name: cacheLabel, label: endpointLabel, version: fileVersion}

func (k fileKind) fileName(key Key) string         { return k.prefix + key.FileName() }
func (k fileKind) lockName(key Key) string         { return k.prefix + key.LockName() }
func (k fileKind) discoverLockName(key Key) string { return k.prefix + key.DiscoverLockName() }

// kindFor returns the file kind opts asks for. Leaving FilePrefix, Label, and
// Version all empty selects the endpoint cache file, exactly as before. A
// kind of another file needs both a prefix and a label, because a prefix with
// the endpoint label, or a label with the endpoint's file name, would let two
// kinds of file overwrite each other.
func kindFor(opts StoreOptions) (fileKind, error) {
	if !validDisplayName(opts.DisplayName) {
		return fileKind{}, fmt.Errorf("%w: display name %q is not words of at most %d characters",
			ErrInvalidFileKind, opts.DisplayName, maxDisplayName)
	}
	if opts.FilePrefix == "" && opts.Label == "" && opts.Version == 0 {
		if opts.DisplayName != "" && opts.DisplayName != cacheLabel {
			return fileKind{}, fmt.Errorf("%w: the endpoint cache file keeps the name %q", ErrInvalidFileKind, cacheLabel)
		}
		return endpointKind, nil
	}
	switch {
	case opts.FilePrefix == "":
		return fileKind{}, fmt.Errorf("%w: a label or version needs a file prefix", ErrInvalidFileKind)
	case !validFilePrefix(opts.FilePrefix) || !slices.Contains(knownFilePrefixes, opts.FilePrefix):
		return fileKind{}, fmt.Errorf("%w: file prefix %q is not one the sweep knows", ErrInvalidFileKind, opts.FilePrefix)
	case opts.Label == "":
		return fileKind{}, fmt.Errorf("%w: a file prefix needs a label", ErrInvalidFileKind)
	case len(opts.Label) > maxFileLabel || !fileLabelPattern.MatchString(opts.Label):
		return fileKind{}, fmt.Errorf("%w: label %q is not lowercase words of at most %d characters",
			ErrInvalidFileKind, opts.Label, maxFileLabel)
	case strings.HasPrefix(opts.Label, endpointLabel):
		return fileKind{}, fmt.Errorf("%w: label %q is the endpoint cache's", ErrInvalidFileKind, opts.Label)
	case opts.Version < 0:
		return fileKind{}, fmt.Errorf("%w: version %d is negative", ErrInvalidFileKind, opts.Version)
	}
	version := opts.Version
	if version == 0 {
		version = defaultKindVersion
	}
	name := opts.DisplayName
	if name == "" {
		name = cacheLabel
	}
	return fileKind{prefix: opts.FilePrefix, name: name, label: opts.Label, version: version}, nil
}
