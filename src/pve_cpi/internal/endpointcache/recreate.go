package endpointcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// namesUser is true when the anchor's owner is the user the CPI runs as
	// whoever that owner is, which holds for the journal directory, because
	// pre-start provisions it for that user. Otherwise the anchor is the
	// temporary directory, whose owner names that user only when it is not
	// root, as the Director's /var/vcap/data/director/tmp belongs to vcap and
	// /tmp belongs to root. Only the user the anchor names may create the
	// directory.
	namesUser bool
}

// journalAnchor is the anchor of a missing directory beside the allocation
// journal, which is the journal directory itself.
func journalAnchor(journal string) anchor {
	return anchor{path: journal, what: "the allocation journal directory", namesUser: true}
}

// overrideAnchor is the anchor of a missing pve.endpoint_cache_dir. When the
// journal directory is set it is the anchor, because the operator has already
// put it on durable storage. Otherwise the anchor is the temporary directory,
// because the layers after the override end there, so a directory on its
// mount is as durable as the one the HA recovery episode record would fall
// back to, and the operator's directory is not created anywhere less durable.
// The temporary directory's owner then names the CPI's user, unless it is
// root.
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
// one of these holds. The process runs as the user the CPI runs as, which is
// the owner of the journal directory when the anchor is the journal, and the
// owner of the temporary directory when no journal is set and that owner is
// not root. Otherwise a run as another user, such as an operator's sudo, would
// leave a directory that every later call of the CPI refuses, where before it
// would have left the directory missing and the record would have fallen back. The parent exists, is not a symbolic
// link, belongs to the CPI's user, grants its owner write and search access,
// and grants no group or other write access. Every directory that resolving
// the parent looks a name up in belongs to root or to the CPI's user and
// grants no group or other write access unless it is sticky, which is the rule
// cpi provision-journal applies to the directories above the journal. The
// parent is on the same mount as the anchor, because under BPM a directory
// outside every mounted volume lands in the worker's private filesystem and
// vanishes on the next call. The guard, which Resolve fronts with its journal
// check, accepts the path. A directory that another worker creates first
// answers EEXIST and goes on to the private checks, as the temporary layer's
// does. A creation that fails for any other reason declines. A directory this
// process created takes the process's group, which is the group cpi
// provision-journal gives it, and one that then fails the private checks is
// removed and declined, so the layer behaves as it did before. Only when the
// removal fails too does the layer refuse it.
func (t *texts) createMissing(opts Options, choice Choice, a anchor, create bool) (Choice, error) {
	hooks := opts.creation()
	parent := filepath.Dir(choice.Path)
	if err := t.creatorIsTheCPIsUser(a); err != nil {
		return choice, err
	}
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
		// A parent with the setgid bit, or a filesystem mounted with grpid,
		// would hand the directory the parent's group, which provisioning
		// warns about on every pre-start. A failure here leaves that group,
		// and the private checks below still decide whether the CPI uses it.
		_ = os.Lchown(choice.Path, -1, os.Getegid())
		if err := t.secureCreated(choice.Path); err != nil {
			if rmErr := syscall.Rmdir(choice.Path); rmErr != nil {
				choice.Err = &Refusal{Layer: choice.Layer, Path: choice.Path, Err: errors.Join(err,
					fmt.Errorf("removing the directory this call created failed: %w", rmErr))}
				return choice, nil
			}
			return choice, fmt.Errorf("the directory it made failed the checks, so it removed it again: %w", err)
		}
		choice.Created = true
	case errors.Is(err, os.ErrExist):
		// Another worker created it first, so the private checks decide.
	default:
		return choice, fmt.Errorf("creating it failed: %w", err)
	}
	return t.open(choice), nil
}

// creatorIsTheCPIsUser returns nil when the user this process runs as, or the
// user a probe judges as, is the user the CPI runs as, and otherwise a reason
// that names both. That user is the anchor's owner. With the journal as the
// anchor, that holds whoever owns it. With the temporary directory as the
// anchor, it holds only when the owner is not root, because the CPI does not
// run as root, so a temporary directory such as /tmp names no user, and root
// is refused whatever the anchor, because a directory root made would belong
// to root and be refused by every call of the CPI.
func (t *texts) creatorIsTheCPIsUser(a anchor) error {
	uid := t.ownerUID()
	if !a.namesUser && uid == 0 {
		return errors.New("it would be made by root, and with no allocation journal directory to name the " +
			"user the CPI runs as, a directory owned by root could be one the CPI refuses on every later call")
	}
	info, err := os.Stat(a.path)
	if err != nil {
		return fmt.Errorf("%s %s, whose owner is the user the CPI runs as, could not be examined: %w",
			a.what, a.path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s %s has no ownership metadata to name the user the CPI runs as", a.what, a.path)
	}
	if !a.namesUser && st.Uid == 0 {
		return fmt.Errorf("%s %s belongs to root, so with no allocation journal directory nothing names the "+
			"user the CPI runs as, and a directory that uid %d made could be one the CPI refuses on every later "+
			"call", a.what, a.path, uid)
	}
	if int64(st.Uid) != int64(uid) {
		return fmt.Errorf("%s %s belongs to uid %d, the user the CPI runs as, and it would be made by uid %d, "+
			"so the CPI would refuse it on every later call", a.what, a.path, st.Uid, uid)
	}
	return nil
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

// maxLinks is how many symbolic links trustedAncestors follows while it
// resolves a parent, which matches the limit Linux puts on a path lookup.
const maxLinks = 40

// trustedAncestors returns nil when every directory that resolving parent
// looks a name up in belongs to root or to uid and grants no group or other
// write access unless it is sticky. It resolves the path one name at a time,
// as the kernel does, so a directory that holds a symbolic link on the way is
// checked as well as the directories the link leads to. Any directory that
// fails could be changed by another user between these checks and the mkdir,
// which would move the new directory somewhere the checks never saw. The
// parent itself is creatableParent's to judge.
func trustedAncestors(parent string, uid int) error {
	if !filepath.IsAbs(parent) {
		return fmt.Errorf("its parent directory %q is not an absolute path", parent)
	}
	dir, rest, links := string(filepath.Separator), parent, 0
	for {
		var name string
		name, rest = nextName(rest)
		switch name {
		case "":
			return nil
		case ".":
			continue
		case "..":
			// dir holds no symbolic link, so its lexical parent is its real one.
			dir = filepath.Dir(dir)
			continue
		}
		if err := trustedDirectory(dir, uid); err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("the directory %s above it could not be examined: %w", path, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			dir = path
			continue
		}
		links++
		if links > maxLinks {
			return fmt.Errorf("resolving its parent directory %s follows more than %d symbolic links", parent,
				maxLinks)
		}
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("the symbolic link %s above it could not be read: %w", path, err)
		}
		if filepath.IsAbs(target) {
			dir = string(filepath.Separator)
		}
		rest = target + string(filepath.Separator) + rest
	}
}

// nextName splits the first name off a slash-separated path and returns it
// with what follows. It returns an empty name when nothing is left.
func nextName(path string) (name, rest string) {
	path = strings.TrimLeft(path, string(filepath.Separator))
	name, rest, _ = strings.Cut(path, string(filepath.Separator))
	return name, rest
}

// trustedDirectory applies trustedAncestors' rule to one directory, which
// holds no symbolic link on its path.
func trustedDirectory(dir string, uid int) error {
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
	return nil
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
