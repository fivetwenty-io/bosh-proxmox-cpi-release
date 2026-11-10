package endpointcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// tempMaxAge is how old a temporary file must be before the sweep removes
	// it. A write takes milliseconds, so a temporary file this old is what a
	// crash between create and rename left behind.
	tempMaxAge = time.Hour
	// fileMaxAge is how long a cache or lock file can go unmodified before the
	// sweep removes it. An endpoint list or CA change leaves such files
	// orphaned.
	fileMaxAge = 30 * 24 * time.Hour
)

// cacheFileName matches the names the cache makes for a key: an optional
// prefix of another kind of file, the sanitized host, a hyphen, 16 hex
// characters of the key digest, and one of the key file, lock, and discover
// lock suffixes. It is the shape Key.FileName, Key.LockName, and
// Key.DiscoverLockName produce, with a known file prefix in front of it for a
// file that is not the endpoint cache's.
var cacheFileName = regexp.MustCompile(
	`^((?:` + strings.Join(quotedPrefixes(), "|") + `)?[a-z0-9][a-z0-9.-]{0,62}-[0-9a-f]{16})` +
		`(\.json|\.lock|\.discover\.lock)$`)

// quotedPrefixes returns the known file prefixes as regular expression text.
func quotedPrefixes() []string {
	out := make([]string, 0, len(knownFilePrefixes))
	for _, p := range knownFilePrefixes {
		out = append(out, regexp.QuoteMeta(p))
	}
	return out
}

// SweepResult reports what a sweep did.
type SweepResult struct {
	// Removed are the names of the files the sweep removed, in name order.
	Removed []string
	// Err joins the failures of the files the sweep could not stat or remove.
	// A failure on one file does not stop the sweep, and a file that
	// disappeared in the meantime is not a failure.
	Err error
}

// Sweep removes what the cache left behind in the directory root names. It
// removes temporary files, which start with privatefile.TempPrefix, that
// nobody has modified for an hour, and key files, lock files, and discover lock
// files that nobody has modified for 30 days. A lock file's age is the newer of
// its own modification time and its key file's, so a lock in daily use or
// beside a fresh key file stays.
//
// It looks only at the directory's top level and never descends, and it
// removes only names the cache itself makes, so a directory that aliases
// another tree cannot lose that tree's files. It removes only regular files
// that the effective user owns, and it never removes secret or secret.lock
// whatever their age. keep lists names it leaves alone whatever their age,
// such as the lock the caller holds. It writes no log line.
func Sweep(root *os.Root, now time.Time, keep ...string) SweepResult {
	return endpointTexts.sweepWith(root, now, keep, ownedByEffectiveUser, "")
}

// sweepWith is Sweep with the ownership test replaced, so a test can show
// that a file another user owns stays, and with its error texts naming the
// store that calls it. A non-empty prefix limits the sweep to key files and
// lock files that start with it, plus temporary files, which carry no kind.
// Another kind's names are then never candidates, even for the same key.
func (t *texts) sweepWith(
	root *os.Root, now time.Time, keep []string, owned func(fs.FileInfo) bool, prefix string,
) SweepResult {
	var result SweepResult
	names, err := t.listNames(root)
	if err != nil {
		result.Err = err
		return result
	}
	infos := make(map[string]fs.FileInfo, len(names))
	var errs []error
	statted := func(name string) (fs.FileInfo, bool) {
		if info, ok := infos[name]; ok {
			return info, true
		}
		info, err := root.Lstat(name)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: stat %s: %w", t.name, name, err))
			}
			return nil, false
		}
		infos[name] = info
		return info, true
	}

	for _, name := range names {
		maxAge, ok := sweepAge(name, keep)
		if !ok || !inSweepScope(name, prefix) {
			continue
		}
		info, ok := statted(name)
		if !ok || !info.Mode().IsRegular() || !owned(info) {
			continue
		}
		modified := info.ModTime()
		if base, isLock := lockKeyFile(name); isLock {
			if keyInfo, ok := statted(base); ok && keyInfo.Mode().IsRegular() && keyInfo.ModTime().After(modified) {
				modified = keyInfo.ModTime()
			}
		}
		if now.Sub(modified) <= maxAge {
			continue
		}
		if err := root.Remove(name); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: remove %s: %w", t.name, name, err))
			}
			continue
		}
		result.Removed = append(result.Removed, name)
	}
	result.Err = errors.Join(errs...)
	return result
}

// listNames returns the names in the directory root names, sorted.
func (t *texts) listNames(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("%s: open directory to sweep: %w", t.name, err)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("%s: list directory to sweep: %w", t.name, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names, nil
}

// sweepAge returns how old the cache file called name must be to be removed,
// and false for a name the sweep never touches: the directory secret and its
// lock, a name in keep, and any name the cache does not make.
func sweepAge(name string, keep []string) (time.Duration, bool) {
	switch {
	case name == secretFile || name == secretLockFile:
		return 0, false
	case slices.Contains(keep, name):
		return 0, false
	case strings.HasPrefix(name, privatefile.TempPrefix):
		return tempMaxAge, true
	case cacheFileName.MatchString(name):
		return fileMaxAge, true
	}
	return 0, false
}

// inSweepScope reports whether a sweep limited to the file kind that starts its
// names with prefix may remove name. The empty prefix limits nothing. A kind's
// names are told apart by the prefix alone. The prefix holds a character
// sanitizeHost never emits, so no endpoint file for any host starts with it.
func inSweepScope(name, prefix string) bool {
	return prefix == "" || strings.HasPrefix(name, prefix) || strings.HasPrefix(name, privatefile.TempPrefix)
}

// lockKeyFile returns the key file that belongs to the lock file or discover
// lock file called name, and false for any other name.
func lockKeyFile(name string) (string, bool) {
	match := cacheFileName.FindStringSubmatch(name)
	if len(match) != 3 || match[2] == fileSuffix {
		return "", false
	}
	return match[1] + fileSuffix, true
}

// ownedByEffectiveUser reports whether the effective user owns the file.
func ownedByEffectiveUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Geteuid())
}
