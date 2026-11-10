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
	// LayerTemporary is pve_cpi-endpoint-cache.<euid> under the CPI's temporary
	// directory. The CPI creates it at runtime, one component deep.
	LayerTemporary Layer = "temporary"
)

// temporaryName is the base name of the temporary layer's directory before the
// effective user ID is appended.
const temporaryName = "pve_cpi-endpoint-cache."

// Options carries what the layers need to choose a directory.
type Options struct {
	// Override is the value of pve.endpoint_cache_dir, empty when it is unset.
	Override string
	// DisplayName is what the refusal texts call the cache the caller is
	// choosing a directory for, such as HAProfileName. Empty means the endpoint
	// cache, and a name that is not letters, digits, spaces, and hyphens reads
	// as empty.
	DisplayName string
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
// does not try another layer. A caller hands Root to OpenStore, which owns it
// from then on, and closes the Store instead of the Choice.
type Choice struct {
	Layer Layer
	Path  string
	Root  *os.Root
	Err   *Refusal
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
// next layer is considered, or answers with a Choice. A layer that applies and
// refuses its directory ends the search, because falling through to a looser
// layer would hide a directory that someone else may control.
type layer func(opts Options) (choice Choice, applies bool)

// layers lists the layers in the order they are considered. The first one that
// applies wins. The layer beside the journal belongs between the override and
// the temporary layer, and the temporary layer always applies, so it stays
// last.
var layers = []layer{
	overrideLayer,
	temporaryLayer,
}

// Resolve chooses the cache directory. It writes no log line, because its
// caller decides the level for the layer chosen and for any refusal.
func Resolve(opts Options) Choice { return resolveWith(layers, opts) }

// resolveWith runs the layers in order. Every layer but the last may decline,
// and the last answers with whatever it returns, so the result always names a
// layer. The list must not be empty.
func resolveWith(list []layer, opts Options) Choice {
	last := len(list) - 1
	for _, l := range list[:last] {
		if choice, applies := l(opts); applies {
			return choice
		}
	}
	choice, _ := list[last](opts)
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

// temporaryLayer uses pve_cpi-endpoint-cache.<euid> under os.TempDir().
func temporaryLayer(opts Options) (Choice, bool) {
	return textsFor(opts.DisplayName).temporaryLayerIn(os.TempDir(), os.Mkdir)
}

// temporaryLayerIn creates only the last component, with mkdir at mode 0700, so
// a missing temporary directory is a refusal and never a tree of new
// directories. The euid suffix keeps processes that run as different users
// apart. A relative temporary directory is refused before anything is created,
// because it would put the cache under the working directory.
func (t *texts) temporaryLayerIn(tempDir string, mkdir func(string, os.FileMode) error) (Choice, bool) {
	path := filepath.Join(tempDir, temporaryName+strconv.Itoa(os.Geteuid()))
	choice := Choice{Layer: LayerTemporary, Path: path}
	if !filepath.IsAbs(tempDir) {
		choice.Err = &Refusal{Layer: LayerTemporary, Path: path, Err: fmt.Errorf("temporary directory %q must be absolute", tempDir)}
		return choice, true
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
	return os.Chmod(path, 0o700) // #nosec G302 -- a directory needs owner search; this is the private mode, not a widening
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
