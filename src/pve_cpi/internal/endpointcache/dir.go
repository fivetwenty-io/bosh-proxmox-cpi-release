package endpointcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// Layer names which of the cache directory's layers a Choice came from.
type Layer string

const (
	// LayerOverride is the directory the operator named with
	// pve.endpoint_cache_dir. It wins over every other layer, and the CPI never
	// creates it.
	LayerOverride Layer = "override"
	// LayerJournal is the endpoint-cache directory beside the allocation
	// journal directory. The CPI uses it only when it already exists, because
	// provision-journal creates it, and never creates it at runtime.
	LayerJournal Layer = "journal"
	// LayerTemporary is pve_cpi-endpoint-cache.<euid> under the CPI's temporary
	// directory. The CPI creates it at runtime, one component deep.
	LayerTemporary Layer = "temporary"
)

// JournalSiblingName is the base name of the cache directory beside the
// journal directory.
const JournalSiblingName = "endpoint-cache"

// temporaryName is the base name of the temporary layer's directory before the
// effective user ID is appended.
const temporaryName = "pve_cpi-endpoint-cache."

// Options carries what the layers need to choose a directory.
type Options struct {
	// Override is the value of pve.endpoint_cache_dir, empty when it is unset.
	Override string
	// JournalDir is the value of pve.storage_allocation_journal_dir, empty when
	// it is unset. The journal layer looks for endpoint-cache beside it.
	JournalDir string
	// DisplayName is what the refusal texts call the cache the caller is
	// choosing a directory for, such as HAProfileName. Empty means the endpoint
	// cache, and a name that is not letters, digits, spaces, and hyphens reads
	// as empty.
	DisplayName string
	// Guard, when set, vets the path a layer is about to create. See PathGuard.
	// It is a pointer, so Options stays comparable.
	Guard PathGuard
}

// PathGuard rules out a path before a layer creates it. A caller sets one to
// keep a layer from leaving a directory behind that it cannot use, such as one
// that overlaps the allocation journal. BeforeCreate is called with the path
// the layer is about to create, before the layer creates anything, and an error
// refuses the layer with that error as the cause. It is called again, with the
// same path, when the directory already exists, so it must not assume the path
// is missing. Implementations are pointers, which keeps Options comparable.
type PathGuard interface {
	BeforeCreate(path string) error
}

// Refusal is the reason a layer gave no cache directory. Err carries the cause,
// which unwraps to a *privatefile.UnsafePathError whenever the directory itself
// was refused, whether by the private checks, by an unclean override path, or by
// an owner without read, write, and search access, so a caller can tell an owner
// mismatch from a mode or file type refusal with errors.As and errors.Is. A
// directory that cannot be created, and a relative temporary directory, carry
// the operating system error or a plain description instead.
type Refusal struct {
	Layer Layer
	Path  string
	Err   error
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("cache directory %s (%s layer): %v", r.Path, r.Layer, r.Err)
}

func (r *Refusal) Unwrap() error { return r.Err }

// Choice is the outcome of choosing the cache directory. Layer and Path say
// which layer applied and the directory it named. Root is the opened private
// directory, or nil when the layer refused it, in which case Err holds the
// reason and there is no cache for this process. A caller that gets a nil Root
// does not try another layer. Notes holds one line for each layer that
// declined before this one answered, such as the missing directory beside the
// journal. The package writes no log line, so the caller logs them, at Debug,
// because a layer that declines is working as designed. A caller hands Root to
// OpenStore, which owns it from then on, and closes the Store instead of the
// Choice.
type Choice struct {
	Layer Layer
	Path  string
	Root  *os.Root
	Err   *Refusal
	Notes []string
}

// Usable reports whether the choice holds an opened, private directory.
func (c Choice) Usable() bool { return c.Root != nil }

// Close releases the opened directory. It is safe to call on a refused choice.
func (c Choice) Close() error {
	if c.Root == nil {
		return nil
	}
	return c.Root.Close()
}

// layer inspects the options and either declines, with applies false so the
// next layer is considered, or answers with a Choice. A layer that declines for
// a reason worth a log line puts it in the Err of the Choice it returns, and
// resolveWith moves it into the Notes of the choice that answers. A layer that
// applies and refuses its directory ends the search, because falling through
// to a looser layer would hide a directory that someone else may control.
type layer func(opts Options) (choice Choice, applies bool)

// layers lists the layers in the order they are considered. The first one that
// applies wins. The temporary layer always applies, so it stays last.
var layers = []layer{
	overrideLayer,
	journalLayer,
	temporaryLayer,
}

// Resolve chooses the cache directory. It writes no log line, because its
// caller decides the level for the layer chosen and for any refusal.
func Resolve(opts Options) Choice { return resolveWith(layers, opts) }

// resolveWith runs the layers in order. Every layer but the last may decline,
// and the last answers with whatever it returns, so the result always names a
// layer. The reasons that earlier layers gave for declining go into the
// answer's Notes. The list must not be empty.
func resolveWith(list []layer, opts Options) Choice {
	var notes []string
	last := len(list) - 1
	for _, l := range list[:last] {
		choice, applies := l(opts)
		if applies {
			choice.Notes = notes
			return choice
		}
		if choice.Err != nil {
			notes = append(notes, choice.Err.Error())
		}
	}
	choice, _ := list[last](opts)
	choice.Notes = notes
	return choice
}

// unsafeDirectory is the cause of a refusal that this package raises for a
// directory. It prints its own text and unwraps to the *privatefile.UnsafePathError
// that classifies it, so a caller reads it the way it reads the shared checks.
type unsafeDirectory struct {
	unsafe *privatefile.UnsafePathError
	text   string
}

func (e *unsafeDirectory) Error() string { return e.text }
func (e *unsafeDirectory) Unwrap() error { return e.unsafe }

// overrideLayer uses the directory the operator named and nothing else. It
// opens the directory through the private checks and never creates it. Config
// validation already requires the value to be absolute and clean, so a path
// that is not clean here means the validation was bypassed, and the layer
// refuses it instead of opening a directory the operator did not name.
func overrideLayer(opts Options) (Choice, bool) {
	if opts.Override == "" {
		return Choice{}, false
	}
	t := textsFor(opts.DisplayName)
	choice := Choice{Layer: LayerOverride, Path: opts.Override}
	if filepath.Clean(opts.Override) != opts.Override {
		// No inode was examined, so the owner is unknown.
		choice.Err = &Refusal{Layer: LayerOverride, Path: opts.Override, Err: &unsafeDirectory{
			unsafe: &privatefile.UnsafePathError{
				Path: opts.Override, UID: -1, EUID: os.Geteuid(), WantDirectory: true, Cause: t.fileType,
			},
			text: fmt.Sprintf("%v: %s is not a clean path", t.fileType, opts.Override),
		}}
		return choice, true
	}
	return t.open(choice), true
}

// journalLayer uses the endpoint-cache directory beside the journal directory,
// never inside it. It applies only when the journal directory is set and the
// directory exists. A missing directory declines with the reason in the Err of
// the declined choice, so the temporary layer is next. The layer never creates
// the directory, because under BPM a path outside every mounted volume lands in
// the worker's private filesystem, and a runtime mkdir could appear to work and
// lose the cache on every call. A directory that exists applies, and the
// private checks decide whether it is usable. A refusal there ends the search,
// because a sibling that someone else controls is the case the checks exist
// for. A journal directory that is not absolute, or is the filesystem root, has
// no sibling to look for, and a journal whose own base name is endpoint-cache
// would collide with the sibling's name, so the layer declines for both.
func journalLayer(opts Options) (Choice, bool) {
	if opts.JournalDir == "" {
		return Choice{}, false
	}
	journal := filepath.Clean(opts.JournalDir)
	declined := func(path string, cause error) (Choice, bool) {
		refusal := &Refusal{Layer: LayerJournal, Path: path, Err: cause}
		return Choice{Layer: LayerJournal, Path: path, Err: refusal}, false
	}
	if !filepath.IsAbs(journal) || journal == string(filepath.Separator) {
		return declined(opts.JournalDir, fmt.Errorf("the allocation journal directory %q is not an absolute path "+
			"below the root, so it has no directory beside it", opts.JournalDir))
	}
	if filepath.Base(journal) == JournalSiblingName {
		return declined(journal, fmt.Errorf(
			"the allocation journal directory %s is itself named %s, so the name beside it is not free",
			journal, JournalSiblingName))
	}
	t := textsFor(opts.DisplayName)
	choice := Choice{Layer: LayerJournal, Path: filepath.Join(filepath.Dir(journal), JournalSiblingName)}
	if _, err := os.Lstat(choice.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return declined(choice.Path, fmt.Errorf("the directory beside the allocation journal is missing, "+
				"so the next layer applies: %w", os.ErrNotExist))
		}
		// A directory that cannot be examined is not known to be absent, so the
		// layer refuses it and no cache is better than a guess.
		choice.Err = &Refusal{Layer: LayerJournal, Path: choice.Path, Err: err}
		return choice, true
	}
	return t.open(choice), true
}

// temporaryLayer uses pve_cpi-endpoint-cache.<euid> under os.TempDir().
func temporaryLayer(opts Options) (Choice, bool) {
	return textsFor(opts.DisplayName).temporaryLayerGuarded(os.TempDir(), os.Mkdir, opts.Guard)
}

// temporaryLayerIn creates only the last component, with mkdir at mode 0700, so
// a missing temporary directory is a refusal and never a tree of new
// directories. The euid suffix keeps processes that run as different users
// apart. A relative temporary directory is refused before anything is created,
// because it would put the cache under the working directory.
func (t *texts) temporaryLayerIn(tempDir string, mkdir func(string, os.FileMode) error) (Choice, bool) {
	return t.temporaryLayerGuarded(tempDir, mkdir, nil)
}

// temporaryLayerGuarded is temporaryLayerIn with a guard. When guard is not
// nil it runs after the temporary directory is known to be absolute and before
// anything is created, and an error from it refuses the layer.
func (t *texts) temporaryLayerGuarded(
	tempDir string, mkdir func(string, os.FileMode) error, guard PathGuard,
) (Choice, bool) {
	path := filepath.Join(tempDir, temporaryName+strconv.Itoa(os.Geteuid()))
	choice := Choice{Layer: LayerTemporary, Path: path}
	if !filepath.IsAbs(tempDir) {
		choice.Err = &Refusal{
			Layer: LayerTemporary,
			Path:  path,
			Err:   fmt.Errorf("temporary directory %q must be absolute", tempDir),
		}
		return choice, true
	}
	if guard != nil {
		if err := guard.BeforeCreate(path); err != nil {
			choice.Err = &Refusal{Layer: LayerTemporary, Path: path, Err: err}
			return choice, true
		}
	}
	// A directory that already exists, whoever owns it, answers EEXIST here and
	// goes to the private checks untouched. Another worker that wins the race to
	// create it gets the same answer.
	err := mkdir(path, 0o700)
	if err == nil {
		err = t.secureCreated(path)
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		choice.Err = &Refusal{Layer: LayerTemporary, Path: path, Err: err}
		return choice, true
	}
	return t.open(choice), true
}

// secureCreated sets a directory this process just created to exactly 0700,
// because the umask may have stripped owner bits from the requested mode. It
// confirms first that the name still holds a directory owned by the effective
// user, so a name swapped in after the creation is never changed. It is never
// called for a directory that already existed.
func (t *texts) secureCreated(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := t.policy.PrivateInfo(path, info, true); err != nil {
		return err
	}
	return os.Chmod(path, 0o700) // #nosec G302 -- a directory needs owner search; the private mode, not a widening
}

// open fills in the choice by opening its path through the private-directory
// checks. A directory that passes them but lacks owner read, write, or search
// access is refused with that reason, because the owner could not use it and the
// operating system would only say permission denied.
func (t *texts) open(choice Choice) Choice {
	if err := t.requireOwnerAccess(choice.Path); err != nil {
		choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: err}
		return choice
	}
	root, err := t.policy.OpenRoot(choice.Path)
	if err != nil {
		choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: err}
		return choice
	}
	choice.Root = root
	return choice
}

// requireOwnerAccess refuses an absolute path that names a private directory
// without owner read, write, and search. Anything else, including a path the
// private checks refuse, is left to OpenRoot to report.
func (t *texts) requireOwnerAccess(path string) error {
	if !filepath.IsAbs(path) {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || t.policy.PrivateInfo(path, info, true) != nil {
		return nil
	}
	return t.ownerAccess(path, info)
}

// ownerAccess refuses a directory whose owner lacks read, write, or search
// access, because the owner could not use it and the operating system would
// only say permission denied.
func (t *texts) ownerAccess(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o700 == 0o700 {
		return nil
	}
	return &unsafeDirectory{
		unsafe: &privatefile.UnsafePathError{
			Path: path, UID: os.Geteuid(), EUID: os.Geteuid(), Mode: info.Mode(), WantDirectory: true, Cause: t.mode,
		},
		text: fmt.Sprintf("%v: %s has mode %04o and its owner needs read, write, and search access",
			t.mode, path, info.Mode().Perm()),
	}
}

// checkOpenedDirectory applies the checks open makes to the directory r has
// open, reading it through r rather than by path, so a root opened some
// other way meets the same refusals and a rename of the path cannot change
// what is checked.
func (t *texts) checkOpenedDirectory(r *os.Root) error {
	info, err := r.Stat(".")
	if err != nil {
		return err
	}
	if err := t.policy.PrivateInfo(r.Name(), info, true); err != nil {
		return err
	}
	return t.ownerAccess(r.Name(), info)
}
