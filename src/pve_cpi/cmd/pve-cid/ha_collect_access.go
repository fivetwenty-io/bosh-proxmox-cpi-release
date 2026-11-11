package main

import (
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

// ancestorBlock names the first directory above path that uid cannot search,
// or returns nil when uid can search every one. The CPI opens the cache
// directory with its own credentials, so it needs search access on each
// directory above it, and the doctor opens the same path with its own. The
// ancestors are the ones the path names and the ones it resolves to, because
// a symbolic link in the path is followed with the access of the user who
// opens it. groups tells which groups uid belongs to.
func ancestorBlock(path string, uid int, groups func(uid int) ([]int, error)) (*searchBlock, error) {
	return firstUnsearchable(ancestorDirs(path), uid, groups)
}

// ancestorDirs lists the directories above path, from the root down, as the
// path spells them and as it resolves them, without repeats.
func ancestorDirs(path string) []string {
	parent := filepath.Dir(filepath.Clean(path))
	dirs := prefixes(parent)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		seen := make(map[string]bool, len(dirs))
		for _, d := range dirs {
			seen[d] = true
		}
		for _, d := range prefixes(resolved) {
			if !seen[d] {
				dirs = append(dirs, d)
			}
		}
	}
	return dirs
}

// prefixes returns dir and each directory above it, from the root down.
func prefixes(dir string) []string {
	var out []string
	for d := dir; ; d = filepath.Dir(d) {
		out = append([]string{d}, out...)
		if d == filepath.Dir(d) {
			return out
		}
	}
}

// firstUnsearchable returns the first of dirs that uid cannot search. A
// directory the doctor cannot examine, or one whose answer depends on a group
// membership it cannot read, is returned as an error.
func firstUnsearchable(dirs []string, uid int, groups func(uid int) ([]int, error)) (*searchBlock, error) {
	if uid == 0 {
		return nil, nil
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok {
			continue
		}
		block := searchBlock{dir: dir, owner: int(st.Uid), group: int(st.Gid), mode: info.Mode().Perm()}
		can, err := block.searchableBy(uid, groups)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		if !can {
			return &block, nil
		}
	}
	return nil, nil
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
