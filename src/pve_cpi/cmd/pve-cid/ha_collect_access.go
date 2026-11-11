package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// searchBlock names a directory on the way to a cache directory that the
// user the CPI runs as cannot search.
type searchBlock struct {
	// dir is the directory, owner and group are its owner and group, and mode
	// is its permission bits.
	dir          string
	owner, group int
	mode         fs.FileMode
}

// maxLinkHops is how many symbolic links the kernel follows while it resolves
// one path before it gives up with ELOOP.
const maxLinkHops = 40

// ancestorBlock names the first directory the resolution of path searches that
// uid cannot search, or returns nil when uid can search every one. The CPI
// opens the cache directory with its own credentials, so it needs search
// access on each directory its lookup passes through, and the doctor opens the
// same path with its own. A directory that does not exist yet ends the walk,
// as it ends the kernel's lookup. A definite block is returned even when
// another directory could not be examined, because the CPI is refused at the
// block either way, and an error is returned only when the walk found no block
// and could not examine everything. groups tells which groups uid belongs to.
func ancestorBlock(path string, uid int, groups func(uid int) ([]int, error)) (*searchBlock, error) {
	dirs, walkErr := searchedDirs(path)
	block, err := firstUnsearchable(dirs, uid, groups)
	switch {
	case block != nil:
		return block, nil
	case err != nil:
		return nil, err
	}
	return nil, walkErr
}

// searchedDirs lists, in the order the kernel meets them and without repeats,
// the directories whose search permission the lookup of path needs. It
// resolves the path one component at a time, so a symbolic link adds the
// directories its target passes through, a chain of links adds those of every
// hop, and the walk gives up after maxLinkHops links as the kernel does. A
// component that does not exist ends the walk without an error, because the
// lookup ends there too. The directories found before an error are returned
// with it.
func searchedDirs(path string) ([]string, error) {
	var dirs []string
	seen := map[string]bool{}
	cur, hops := string(filepath.Separator), 0
	pending := pathParts(path)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if name == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		if !seen[cur] {
			seen[cur] = true
			dirs = append(dirs, cur)
		}
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return dirs, nil
		case err != nil:
			return dirs, err
		case info.Mode()&fs.ModeSymlink != 0:
			hops++
			if hops > maxLinkHops {
				return dirs, fmt.Errorf("%s: more than %d symbolic links on the way to %s", next, maxLinkHops, path)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return dirs, err
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
			}
			pending = append(pathParts(target), pending...)
		case len(pending) > 0 && !info.IsDir():
			return dirs, fmt.Errorf("%s: %w", next, syscall.ENOTDIR)
		default:
			cur = next
		}
	}
	return dirs, nil
}

// pathParts splits path into the names a lookup meets, leaving out the empty
// and current-directory parts.
func pathParts(path string) []string {
	var parts []string
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// firstUnsearchable returns the first of dirs that uid cannot search. A
// directory the doctor cannot examine, or one whose answer depends on a group
// membership it cannot read, does not stop the walk, because a later directory
// can still block uid outright. The first such error is returned when no
// directory blocks.
func firstUnsearchable(dirs []string, uid int, groups func(uid int) ([]int, error)) (*searchBlock, error) {
	if uid == 0 {
		return nil, nil
	}
	var cannotTell error
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			cannotTell = firstError(cannotTell, err)
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok {
			continue
		}
		block := searchBlock{dir: dir, owner: int(st.Uid), group: int(st.Gid), mode: info.Mode().Perm()}
		can, err := block.searchableBy(uid, groups)
		if err != nil {
			cannotTell = firstError(cannotTell, fmt.Errorf("%s: %w", dir, err))
			continue
		}
		if !can {
			return &block, nil
		}
	}
	return nil, cannotTell
}

// firstError returns first when it is set and next otherwise.
func firstError(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// searchableBy applies the permission bits the way the kernel does. The owner
// is judged by the owner bits alone, a member of the directory's group by the
// group bits alone, and everyone else by the other bits. Access control lists
// are not read. An answer that turns on a group membership that cannot be
// read is an error.
func (b searchBlock) searchableBy(uid int, groups func(uid int) ([]int, error)) (bool, error) {
	const ownerX, groupX, otherX = 0o100, 0o010, 0o001
	if uid == b.owner {
		return b.mode&ownerX != 0, nil
	}
	if (b.mode&groupX != 0) == (b.mode&otherX != 0) {
		return b.mode&otherX != 0, nil
	}
	gids, err := groups(uid)
	if err != nil {
		return false, fmt.Errorf("the groups of uid %d could not be read: %w", uid, err)
	}
	for _, g := range gids {
		if g == b.group {
			return b.mode&groupX != 0, nil
		}
	}
	return b.mode&otherX != 0, nil
}

// groupsOf lists the groups the account with uid belongs to, its primary group
// included.
func groupsOf(uid int) ([]int, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	names, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	names = append(names, u.Gid)
	gids := make([]int, 0, len(names))
	for _, name := range names {
		if gid, err := strconv.Atoi(strings.TrimSpace(name)); err == nil {
			gids = append(gids, gid)
		}
	}
	return gids, nil
}
