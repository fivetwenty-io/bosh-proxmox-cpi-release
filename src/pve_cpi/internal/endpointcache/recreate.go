package endpointcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrCreatedOnFirstUse marks the refusal that Probe gives for a directory that
// does not exist yet and that the CPI creates the first time it chooses it.
// The refusal also matches fs.ErrNotExist, because the directory is not there.
var ErrCreatedOnFirstUse = errors.New("the CPI creates it on first use")

// firstUse is the cause of that refusal. It keeps the text Probe has always
// printed for an absent temporary directory.
type firstUse struct{ path string }

func (e *firstUse) Error() string {
	return fmt.Sprintf("the directory %s does not exist yet, and the CPI creates it on first use: %v",
		e.path, fs.ErrNotExist)
}

func (e *firstUse) Unwrap() []error { return []error{fs.ErrNotExist, ErrCreatedOnFirstUse} }

// NotCreatedError is the cause a layer gives when its directory is missing and
// the CPI declines to create it. It matches fs.ErrNotExist, because the
// directory is still missing, so a caller that falls back from a missing
// override still does. It also unwraps to Reason, which says why the CPI
// declined, so pve-cid ha and the log lines can name it.
type NotCreatedError struct {
	Path   string
	Reason error
}

func (e *NotCreatedError) Error() string {
	return fmt.Sprintf("%s does not exist, and the CPI does not create it because %v", e.Path, e.Reason)
}

func (e *NotCreatedError) Unwrap() []error { return []error{fs.ErrNotExist, e.Reason} }

// mountKey names the filesystem and the mount a directory sits on. Two
// directories with the same key persist together. On Linux the key carries the
// mount ID as well as the device, because a bind mount keeps the device of the
// filesystem it comes from, so the device alone cannot tell a directory that
// BPM mounted into the worker from the worker's private filesystem when both
// live on the same disk.
type mountKey struct {
	dev   uint64
	mount string
}

// creationHooks are the system calls a layer makes to create its missing
// directory. Only tests replace them, to fake a lost race or two mounts.
type creationHooks struct {
	mkdir func(path string, perm os.FileMode) error
	// mount returns the mount key of path. follow says whether a symbolic link
	// in the last component is followed.
	mount func(path string, follow bool) (mountKey, error)
}

// systemHooks are the hooks every production caller uses.
var systemHooks = creationHooks{mkdir: os.Mkdir, mount: mountOf}

func (opts Options) creation() creationHooks {
	if opts.hooks == nil {
		return systemHooks
	}
	h := *opts.hooks
	if h.mkdir == nil {
		h.mkdir = systemHooks.mkdir
	}
	if h.mount == nil {
		h.mount = systemHooks.mount
	}
	return h
}

// anchor is a directory the CPI already relies on to persist. A layer creates
// its missing directory only when the new directory's parent sits on the same
// mount as its anchor, so the new directory persists exactly as long as the
// anchor does.
type anchor struct {
	path string
	// what says in words which directory the anchor is.
	what string
}

// journalAnchor is the anchor of a missing directory beside the allocation
// journal, which is the journal directory itself.
func journalAnchor(journal string) anchor {
	return anchor{path: journal, what: "the allocation journal directory"}
}

// overrideAnchor is the anchor of a missing pve.endpoint_cache_dir. When the
// journal directory is set it is the anchor, because the operator has already
// put it on durable storage. Otherwise the anchor is the temporary directory,
// because the layers after the override end there, so a directory on its
// mount is as durable as the one the HA recovery episode record would fall
// back to, and the operator's directory is not created anywhere less durable.
func overrideAnchor(opts Options) anchor {
	if opts.JournalDir != "" {
		return journalAnchor(filepath.Clean(opts.JournalDir))
	}
	return anchor{path: os.TempDir(), what: "the temporary directory"}
}

// createMissing decides whether the layer of choice may create its missing
// directory and, when create is true, creates it. It returns a non-nil
// declined error, with choice unchanged, when the CPI must not create the
// directory, and the caller then behaves as it did for a missing directory
// before run-time creation existed. Otherwise it returns the choice the layer
// answers with. When create is false, which is Probe, that choice is a refusal
// that matches ErrCreatedOnFirstUse and nothing is created.
//
// The CPI creates only the last component, at mode 0700, and only when every
// one of these holds. The parent exists, is not a symbolic link, belongs to the
// CPI's user, grants its owner write and search access, and grants no group or
// other write access. Every directory above the parent belongs to root or to
// the CPI's user and grants no group or other write access unless it is
// sticky, which is the rule cpi provision-journal applies to the directories
// above the journal. The parent is on the same mount as the anchor, because
// under BPM a directory outside every mounted volume lands in the worker's
// private filesystem and vanishes on the next call. The guard, which Resolve
// fronts with its journal check, accepts the path. A directory that another
// worker creates first answers EEXIST and goes on to the private checks, as
// the temporary layer's does. A creation that fails for any other reason
// declines, and a directory that this process created but that fails the
// check after the creation refuses the layer.
func (t *texts) createMissing(opts Options, choice Choice, a anchor, create bool) (Choice, error) {
	hooks := opts.creation()
	parent := filepath.Dir(choice.Path)
	if err := t.creatableParent(parent); err != nil {
		return choice, err
	}
	if err := sameMount(parent, a, hooks); err != nil {
		return choice, err
	}
	if opts.Guard != nil {
		if err := opts.Guard.BeforeCreate(choice.Path); err != nil {
			return choice, fmt.Errorf("the path was refused before anything was created: %w", err)
		}
	}
	if !create {
		choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: &firstUse{path: choice.Path}}
		return choice, nil
	}
	err := hooks.mkdir(choice.Path, 0o700)
	switch {
	case err == nil:
		if err := t.secureCreated(choice.Path); err != nil {
			choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: err}
			return choice, nil
		}
		choice.Created = true
	case errors.Is(err, os.ErrExist):
		// Another worker created it first, so the private checks decide.
	default:
		return choice, fmt.Errorf("creating it failed: %w", err)
	}
	return t.open(choice), nil
}

// creatableParent returns nil when the CPI's user may create a directory in
// parent, and otherwise a reason that names what failed.
func (t *texts) creatableParent(parent string) error {
	uid := t.ownerUID()
	info, err := os.Lstat(parent)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("its parent directory %s does not exist", parent)
	case err != nil:
		return fmt.Errorf("its parent directory %s could not be examined: %w", parent, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("its parent directory %s is a symbolic link", parent)
	case !info.IsDir():
		return fmt.Errorf("its parent directory %s is not a directory", parent)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("its parent directory %s has no ownership metadata", parent)
	}
	perm := info.Mode().Perm()
	switch {
	case int64(st.Uid) != int64(uid):
		return fmt.Errorf("its parent directory %s is owned by uid %d, and the CPI runs as uid %d",
			parent, st.Uid, uid)
	case perm&0o022 != 0:
		return fmt.Errorf("its parent directory %s has mode %04o, which lets a group or others write in it",
			parent, perm)
	case uid != 0 && perm&0o300 != 0o300:
		return fmt.Errorf("its parent directory %s has mode %04o, which does not give its owner write and "+
			"search access", parent, perm)
	}
	return trustedAncestors(parent, uid)
}

// trustedAncestors returns nil when every directory above parent, after
// symbolic links are resolved, belongs to root or to uid and grants no group
// or other write access unless it is sticky. Any other directory could be
// replaced by another user, which would move the new directory somewhere the
// checks never saw.
func trustedAncestors(parent string, uid int) error {
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("the directories above %s could not be resolved: %w", parent, err)
	}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("the directory %s above it could not be examined: %w", dir, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		perm := info.Mode().Perm()
		switch {
		case !ok || !info.IsDir():
			return fmt.Errorf("the directory %s above it is not a directory with ownership metadata", dir)
		case st.Uid != 0 && int64(st.Uid) != int64(uid):
			return fmt.Errorf("the directory %s above it is owned by uid %d, which is neither root nor the "+
				"CPI's user, uid %d", dir, st.Uid, uid)
		case perm&0o022 != 0 && info.Mode()&fs.ModeSticky == 0:
			return fmt.Errorf("the directory %s above it has mode %04o, which lets a group or others replace "+
				"what it holds", dir, perm)
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

// sameMount returns nil when parent sits on the same mount as the anchor, and
// otherwise a reason that names the two directories.
func sameMount(parent string, a anchor, hooks creationHooks) error {
	if !filepath.IsAbs(a.path) {
		return fmt.Errorf("%s %q is not an absolute path, so nothing shows that a new directory would persist",
			a.what, a.path)
	}
	want, err := hooks.mount(a.path, true)
	if err != nil {
		return fmt.Errorf("%s %s, which shows where a directory persists, could not be examined: %w",
			a.what, a.path, err)
	}
	got, err := hooks.mount(parent, false)
	if err != nil {
		return fmt.Errorf("the mount of its parent directory %s could not be read: %w", parent, err)
	}
	if got != want {
		return fmt.Errorf("its parent directory %s is not on the same mount as %s %s, so a directory made "+
			"there might not persist from one call to the next", parent, a.what, a.path)
	}
	return nil
}
