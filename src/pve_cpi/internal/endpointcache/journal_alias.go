package endpointcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrJournalOverlap is the cause of a refusal for a cache directory that is the
// allocation journal directory, sits inside its tree, or contains it, judged
// after symlink resolution. Every cache that chooses its directory through
// Resolve meets it, because the check lives in Resolve and not in a caller.
var ErrJournalOverlap = errors.New(
	"the directory is the allocation journal directory, sits inside it, or contains it")

// journalOverlap returns nil when path leaves the allocation journal directory
// alone, ErrJournalOverlap when it overlaps the journal, and an error that
// wraps the cause when the two cannot be compared, because the check fails
// closed. An empty journal directory overlaps nothing.
func journalOverlap(path, journal string) error {
	aliased, err := AliasesJournal(path, journal)
	switch {
	case err != nil:
		return fmt.Errorf("the directory could not be compared with the allocation journal directory: %w", err)
	case aliased:
		return ErrJournalOverlap
	}
	return nil
}

// journalGuard is the PathGuard Resolve puts in front of the layers that create
// their directory, so a directory that overlaps the allocation journal is
// refused before anything is created and a refused layer leaves nothing behind.
// It then asks next, the guard the caller set, when there is one.
type journalGuard struct {
	journal string
	next    PathGuard
}

var _ PathGuard = (*journalGuard)(nil)

// BeforeCreate refuses a path that overlaps the journal directory, and
// otherwise returns what the caller's own guard says.
func (g *journalGuard) BeforeCreate(path string) error {
	if err := journalOverlap(path, g.journal); err != nil {
		return err
	}
	if g.next != nil {
		return g.next.BeforeCreate(path)
	}
	return nil
}

// guardJournal returns the guard Resolve hands the layers: one that checks the
// journal directory first and then next. A caller with no journal directory
// keeps its own guard unchanged.
func guardJournal(journal string, next PathGuard) PathGuard {
	if journal == "" {
		return next
	}
	return &journalGuard{journal: journal, next: next}
}

// refuseJournalOverlap checks the directory a layer opened against the journal
// directory, because that check also covers a directory the layer did not
// create and one that changed after the first check. A choice that overlaps the
// journal, or that cannot be compared with it, is closed and becomes a refusal
// of the same layer, so no caller holds an open handle to the journal's tree. A
// refused choice passes through as it is.
func refuseJournalOverlap(choice Choice, journal string) Choice {
	if !choice.Usable() || journal == "" {
		return choice
	}
	err := journalOverlap(choice.Path, journal)
	if err == nil {
		return choice
	}
	_ = choice.Close()
	choice.Root = nil
	choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: err}
	return choice
}

// AliasesJournal reports whether the cache directory overlaps the
// allocation journal directory, which is when the cache directory is the
// journal directory, sits inside its tree, or contains it. The check resolves
// symlinks in both paths, and it compares the directories themselves, and
// every ancestor of each, with os.SameFile, so a link or a bind in the middle
// of either path cannot hide the overlap in either direction. Either
// directory may not exist yet, because the temporary layer asks before it
// creates its directory. A directory that does not exist has no identity to
// compare, so it is compared by its resolved path, which is the only way it can
// overlap an existing directory from below, and a missing cache directory
// contains nothing. An empty journal directory overlaps nothing. A path that
// cannot be examined for any reason but being absent is an error, and the
// caller treats the directory as unusable, because the check must fail closed.
func AliasesJournal(cachePath, journal string) (bool, error) {
	candidates := []string{journal}
	if trimmed := strings.TrimSpace(journal); trimmed != journal {
		candidates = append(candidates, trimmed)
	}
	var journals []string
	for _, c := range candidates {
		if c != "" {
			journals = append(journals, c)
		}
	}
	if len(journals) == 0 {
		return false, nil
	}
	cacheReal, cacheInfo, err := resolveDirectory(cachePath)
	if err != nil {
		return false, fmt.Errorf("resolve the cache directory: %w", err)
	}
	for _, j := range journals {
		journalReal, journalInfo, err := resolveDirectory(j)
		if err != nil {
			return false, fmt.Errorf("resolve the allocation journal directory: %w", err)
		}
		overlap, err := directoriesOverlap(cacheReal, cacheInfo, journalReal, journalInfo)
		if err != nil {
			return false, fmt.Errorf("compare the cache directory with the allocation journal directory: %w", err)
		}
		if overlap {
			return true, nil
		}
	}
	return false, nil
}

// directoriesOverlap reports whether one of two resolved directories is the
// other or lies below it. A nil info means the directory does not exist yet.
// Existing directories are compared by file identity along the ancestors of
// each path. A directory that is missing is compared by path against the other
// one, and by the identity of its nearest existing ancestor when the other one
// exists.
func directoriesOverlap(cachePath string, cacheInfo os.FileInfo, journalPath string, journalInfo os.FileInfo,
) (bool, error) {
	switch {
	case cacheInfo == nil && journalInfo == nil:
		return pathWithin(journalPath, cachePath) || pathWithin(cachePath, journalPath), nil
	case journalInfo == nil:
		// The journal can only lie below the cache directory.
		return pathWithin(cachePath, journalPath), nil
	case cacheInfo == nil:
		// A cache directory that is not there contains nothing, and it sits inside the
		// journal when the directory it would be created in does.
		existing, err := nearestExisting(cachePath)
		if err != nil {
			return false, err
		}
		return ancestorIs(existing, journalInfo)
	}
	inside, err := ancestorIs(cachePath, journalInfo)
	if err != nil || inside {
		return inside, err
	}
	return ancestorIs(journalPath, cacheInfo)
}

// nearestExisting returns dir, or the nearest ancestor of dir that exists.
func nearestExisting(dir string) (string, error) {
	for {
		_, err := os.Stat(dir)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		dir = parent
	}
}

// resolveDirectory returns the absolute path of dir with its symlinks
// resolved, and its file info. A directory that does not exist yet is resolved
// through its nearest existing ancestor, and its info is nil, because nothing
// can be the same file as a directory that is not there. Any other failure is
// returned.
func resolveDirectory(dir string) (string, os.FileInfo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return "", nil, statErr
		}
		return resolved, info, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", nil, err
	}
	resolvedParent, _, parentErr := resolveDirectory(parent)
	if parentErr != nil {
		return "", nil, parentErr
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil, nil
}

// ancestorIs reports whether dir, or any directory above it, is the same file
// as target.
func ancestorIs(dir string, target os.FileInfo) (bool, error) {
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		if os.SameFile(info, target) {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

// pathWithin reports whether the absolute path child equals root or lies below
// it, comparing whole path elements so /a/bc is not inside /a/b.
func pathWithin(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
