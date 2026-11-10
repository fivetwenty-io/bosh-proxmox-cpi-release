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

// directoryPolicy supplies the owner, mode, file type, and symlink checks for
// the cache directory. It never reads or writes an envelope, so it names no
// size limit or integrity function.
var directoryPolicy = privatefile.Policy{Label: "endpoint cache"}

// Options carries what the layers need to choose a directory.
type Options struct {
	// Override is the value of pve.endpoint_cache_dir, empty when it is unset.
	Override string
}

// Refusal is the reason a layer gave no cache directory. Err carries the cause,
// which is a *privatefile.UnsafePathError when the private checks refused the
// directory, so a caller can tell an owner mismatch from a mode or file type
// refusal with errors.As and errors.Is.
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
// does not try another layer.
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
func Resolve(opts Options) Choice {
	for _, l := range layers {
		if choice, applies := l(opts); applies {
			return choice
		}
	}
	// The temporary layer always applies, so this is unreachable with the
	// layers above. Refuse rather than report a usable directory if a later
	// edit empties the list.
	return Choice{Err: &Refusal{Layer: LayerTemporary, Err: errors.New("no cache directory layer applies")}}
}

// overrideLayer uses the directory the operator named and nothing else. It
// opens the directory through the private checks and never creates it.
func overrideLayer(opts Options) (Choice, bool) {
	if opts.Override == "" {
		return Choice{}, false
	}
	choice := Choice{Layer: LayerOverride, Path: opts.Override}
	if filepath.Clean(opts.Override) != opts.Override {
		choice.Err = &Refusal{Layer: LayerOverride, Path: opts.Override, Err: errors.New("directory path is not clean")}
		return choice, true
	}
	return open(choice), true
}

// temporaryLayer uses pve_cpi-endpoint-cache.<euid> under os.TempDir(). It
// creates only the last component, with os.Mkdir at mode 0700, so a missing
// temporary directory is a refusal and never a tree of new directories. The
// euid suffix keeps processes that run as different users apart.
func temporaryLayer(Options) (Choice, bool) {
	path := filepath.Join(os.TempDir(), temporaryName+strconv.Itoa(os.Geteuid()))
	choice := Choice{Layer: LayerTemporary, Path: path}
	// A directory that already exists, whoever owns it, answers EEXIST here and
	// goes to the private checks. Another worker that wins the race to create it
	// gets the same answer.
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		choice.Err = &Refusal{Layer: LayerTemporary, Path: path, Err: err}
		return choice, true
	}
	return open(choice), true
}

// open fills in the choice by opening its path through the private-directory
// checks.
func open(choice Choice) Choice {
	root, err := directoryPolicy.OpenRoot(choice.Path)
	if err != nil {
		choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: err}
		return choice
	}
	choice.Root = root
	return choice
}
