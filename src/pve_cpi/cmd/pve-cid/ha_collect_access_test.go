package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// otherUser is a user the test process is not, standing in for another account
// on the host.
func otherUser() int { return os.Geteuid() + 1 }

// A temporary directory named for the doctor's user and owned by another user
// is refused by the CPI that runs as the doctor's user. Its name carries that
// uid, so judging it again as its owner would look for a different directory
// and find none. With no journal to name the CPI's user, the doctor reports
// the refusal, and a missing override that falls through to it says the record
// has no place to go.
func TestCollectReportsATemporaryDirectoryAnotherUserOwns(t *testing.T) {
	doctor := otherUser()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	path := filepath.Join(tmp, "pve_cpi-endpoint-cache."+itoa(doctor))
	makeDir(t, path)

	t.Run("no override", func(t *testing.T) {
		r := cacheFindingsAs(haCfg("auto", "none"), doctor)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, path, "is refused", "dead-node handover cannot start",
			"carries uid "+itoa(doctor), "another user owns it")
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "confirm the owner") {
			t.Errorf("a uid-named directory is not the CPI's because of who owns it: %q", got)
		}
	})

	t.Run("missing override", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(t.TempDir(), "no-parent", "absent")
		r := cacheFindingsAs(cfg, doctor)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, cfg.EndpointCacheDir, "does not exist",
			"cannot be used either", "dead-node handover cannot start")
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "sits in") {
			t.Errorf("the record does not sit in a directory the CPI refuses: %q", got)
		}
	})

	t.Run("a directory its own user owns passes", func(t *testing.T) {
		makeDir(t, filepath.Join(tmp, "pve_cpi-endpoint-cache."+itoa(os.Geteuid())))
		wantNone(t, cacheFindingsAs(haCfg("auto", "none"), os.Geteuid()), haKindCacheDirectory)
	})

	t.Run("a missing override that falls through to a directory that passes", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(t.TempDir(), "no-parent", "absent")
		want(t, cacheFindingsAs(cfg, os.Geteuid()), haKindCacheDirectory, haSeverityAdvice, cfg.EndpointCacheDir,
			"sits in the temporary cache directory")
	})
}

// The doctor opens the cache directory with its own credentials, so when it
// judges as the CPI's user it also asks whether that user can search every
// directory above it, because the CPI gets permission denied at the first one
// it cannot. The test names the directory through the judge's search, since
// the test process cannot reach a directory it has no access to.
func TestCollectReportsADirectoryTheCPIsUserCannotReach(t *testing.T) {
	doctor := otherDoctor(t)
	t.Setenv("TMPDIR", t.TempDir())
	journal, sibling := journalCacheLayout(t, 0o700)
	makeDir(t, journal)
	override := filepath.Join(t.TempDir(), "cache")
	makeDir(t, override)
	// The parent is missing too, so the CPI does not create the override.
	missing := filepath.Join(t.TempDir(), "no-parent", "absent")
	cpiUser := os.Geteuid()

	collect := func(t *testing.T, cfg *config.CPIConfig, search func(string, int) (*searchBlock, error)) *haReport {
		t.Helper()
		c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}}}
		j := c.cacheJudge()
		j.ancestors = search
		c.cacheFindingsAs(j)
		return c.rep
	}
	blocking := func(dir string) func(string, int) (*searchBlock, error) {
		return func(string, int) (*searchBlock, error) {
			return &searchBlock{dir: dir, owner: 0, group: 0, mode: 0o700}, nil
		}
	}
	reachable := func(string, int) (*searchBlock, error) { return nil, nil }

	t.Run("journal sibling", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		wantNone(t, collect(t, cfg, reachable), haKindCacheDirectory)
		r := collect(t, cfg, blocking("/var/vcap/data/pve_cpi"))
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "sits below /var/vcap/data/pve_cpi",
			"uid "+itoa(cpiUser)+" cannot search", "owned by uid 0 with mode 0700", "permission denied",
			"dead-node handover cannot start")
	})

	t.Run("override", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = override
		r := collect(t, cfg, blocking("/var/vcap/data"))
		want(t, r, haKindCacheDirectory, haSeverityAdvice, "pve.endpoint_cache_dir sits below /var/vcap/data",
			"permission denied")
	})

	t.Run("fallback of a missing override", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		cfg.EndpointCacheDir = missing
		onlySibling := func(path string, _ int) (*searchBlock, error) {
			if path != sibling {
				return nil, nil
			}
			return &searchBlock{dir: "/var/vcap/data", owner: 0, group: 0, mode: 0o700}, nil
		}
		r := collect(t, cfg, onlySibling)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, missing, "does not exist", "beside the allocation journal",
			"sits below /var/vcap/data", "permission denied")
	})

	t.Run("a search that cannot tell", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		r := collect(t, cfg, func(string, int) (*searchBlock, error) { return nil, errors.New("no such account") })
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "was not checked for search access",
			"no such account")
	})

	t.Run("the doctor's own user is not searched again", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = journal
		c := &haCollector{cfg: cfg, euid: cpiUser, rep: &haReport{Findings: []haFinding{}}}
		j := c.cacheJudge()
		j.ancestors = func(string, int) (*searchBlock, error) {
			t.Error("the doctor already opened the path with the CPI's own credentials")
			return nil, nil
		}
		c.cacheFindingsAs(j)
		wantNone(t, c.rep, haKindCacheDirectory)
	})
}

// firstUnsearchable applies the permission bits the way the kernel does, for a
// user the directory does not belong to.
func TestFirstUnsearchableAppliesTheKernelsRule(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "above")
	makeDir(t, dir)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restores the test's own directory
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	owner, group := int(st.Uid), int(st.Gid)
	stranger := owner + 1
	member := func(gids ...int) func(int) ([]int, error) {
		return func(int) ([]int, error) { return gids, nil }
	}
	broken := func(int) ([]int, error) { return nil, errors.New("no such account") }

	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		uid     int
		groups  func(int) ([]int, error)
		blocked bool
		fails   bool
	}{
		{"other bit grants a stranger", 0o701, stranger, member(), false, false},
		{"no bits stop a stranger", 0o700, stranger, member(), true, false},
		{"group bit grants a member", 0o710, stranger, member(group), false, false},
		{"group bit does not grant a stranger", 0o710, stranger, member(), true, false},
		{"a member is judged by the group bits alone", 0o701, stranger, member(group), true, false},
		{"unreadable groups matter when the bits differ", 0o710, stranger, broken, false, true},
		{"unreadable groups do not matter when the bits agree", 0o711, stranger, broken, false, false},
		{"unreadable groups do not matter when no bit grants", 0o700, stranger, broken, true, false},
		{"owner bit grants the owner", 0o700, owner, member(), false, false},
		{"owner bit that is missing stops the owner", 0o611, owner, member(), true, false},
		{"root searches anything", 0o000, 0, member(), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chmod(dir, tc.mode); err != nil { //nolint:gosec // the mode is the test's input
				t.Fatal(err)
			}
			block, err := firstUnsearchable([]string{dir}, tc.uid, tc.groups)
			if (err != nil) != tc.fails {
				t.Fatalf("error = %v, want failure %v", err, tc.fails)
			}
			if (block != nil) != tc.blocked {
				t.Fatalf("block = %+v, want blocked %v", block, tc.blocked)
			}
			if block != nil && (block.dir != dir || block.owner != owner || block.mode != tc.mode) {
				t.Errorf("block = %+v, want %s owned by %d with mode %04o", block, dir, owner, tc.mode)
			}
		})
	}
}

// The directories a lookup searches are the ones it passes through, from the
// root down, and a link in the path adds the directories its target passes
// through. The link itself is read, not searched.
func TestSearchedDirsFollowALinkInThePath(t *testing.T) {
	base := realTemp(t)
	deep := filepath.Join(base, "real", "deep")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	dirs, err := searchedDirs(filepath.Join(link, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if dirs[0] != "/" {
		t.Errorf("first directory %q, want the root", dirs[0])
	}
	wantDirs(t, dirs, base, filepath.Join(base, "real"), deep)
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Errorf("directories %v repeat %s", dirs, d)
		}
		seen[d] = true
	}
}

// A chain of links loses nothing: the kernel searches the directories that
// every hop's target passes through, relative targets and parent references
// included, and the directory the first link sits in.
func TestSearchedDirsFollowAChainOfLinks(t *testing.T) {
	base := realTemp(t)
	for _, d := range []string{"a", "b", "c/d", "c/x"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../b/l2", filepath.Join(base, "a", "l1")); err != nil {
		t.Fatal(err)
	}
	// The target is a raw string, because filepath.Join would clean the parent
	// reference out of it.
	if err := os.Symlink(base+"/c/x/../d", filepath.Join(base, "b", "l2")); err != nil {
		t.Fatal(err)
	}
	dirs, err := searchedDirs(filepath.Join(base, "a", "l1", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	wantDirs(t, dirs, base, filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c"),
		filepath.Join(base, "c", "x"), filepath.Join(base, "c", "d"))
}

// A parent reference after a descent is looked up in the directory it leaves,
// so the kernel needs search access there even though the path climbs straight
// back out, in a link's target and in a path as spelled alike.
func TestSearchedDirsRecordTheDirectoryAParentReferenceLeaves(t *testing.T) {
	base := realTemp(t)
	for _, d := range []string{"a/sub", "b"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("sub/../../b", filepath.Join(base, "a", "l")); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(base, "a", "sub")
	for name, path := range map[string]string{
		"in a link target":               filepath.Join(base, "a", "l", "cache"),
		"in the spelled path":            base + "/a/sub/../../b/cache",
		"after a current-directory step": base + "/a/sub/./../../b/cache",
	} {
		dirs, err := searchedDirs(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, d := range dirs {
			found = found || d == sub
		}
		if !found {
			t.Errorf("%s: directories %v lack %s", name, dirs, sub)
		}
	}
}

// The kernel follows at most 40 links in one lookup, and the walk stops at the
// same count instead of looping, keeping the directories it found.
func TestSearchedDirsStopAtTheKernelsLinkLimit(t *testing.T) {
	chain := func(t *testing.T, links int) error {
		t.Helper()
		base := realTemp(t)
		target := filepath.Join(base, "end")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := links - 1; i >= 0; i-- {
			next := target
			if i < links-1 {
				next = filepath.Join(base, fmt.Sprintf("l%d", i+1))
			}
			if err := os.Symlink(next, filepath.Join(base, fmt.Sprintf("l%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		_, err := searchedDirs(filepath.Join(base, "l0", "cache"))
		return err
	}
	if err := chain(t, maxLinkHops); err != nil {
		t.Errorf("%d links: %v", maxLinkHops, err)
	}
	if err := chain(t, maxLinkHops+1); err == nil || !strings.Contains(err.Error(), "more than 40 symbolic links") {
		t.Errorf("%d links: error = %v, want the link limit", maxLinkHops+1, err)
	}

	base := realTemp(t)
	if err := os.Symlink(filepath.Join(base, "loop"), filepath.Join(base, "loop")); err != nil {
		t.Fatal(err)
	}
	dirs, err := searchedDirs(filepath.Join(base, "loop", "cache"))
	if err == nil {
		t.Fatal("a link to itself was followed without end")
	}
	wantDirs(t, dirs, base)
}

// A component that does not exist ends the walk, because the lookup ends
// there, so a directory that is not made yet is judged by the directories
// above the first one that is missing.
func TestSearchedDirsEndAtTheFirstMissingComponent(t *testing.T) {
	base := realTemp(t)
	dirs, err := searchedDirs(filepath.Join(base, "absent", "deeper", "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if last := dirs[len(dirs)-1]; last != base {
		t.Errorf("directories %v end at %s, want %s", dirs, last, base)
	}
}

// A definite block is reported even when another directory could not be
// examined or its answer turns on a group the doctor cannot read, because the
// CPI is refused at the block whatever the other answer is. Without a block,
// the first thing that could not be told is the error.
func TestFirstUnsearchableNamesABlockBehindAnUnknown(t *testing.T) {
	base := realTemp(t)
	early, late, open := filepath.Join(base, "early"), filepath.Join(base, "late"), filepath.Join(base, "open")
	for _, d := range []string{early, late, open} {
		makeDir(t, d)
	}
	t.Cleanup(func() { //nolint:gosec // restores the test's own directories
		_ = os.Chmod(early, 0o700)
		_ = os.Chmod(open, 0o700)
	})
	// early grants the group and not others, so the answer turns on a group lookup; open grants everyone.
	if err := os.Chmod(early, 0o750); err != nil { //nolint:gosec // the mode is the test's input
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o711); err != nil { //nolint:gosec // the mode is the test's input
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(base, &st); err != nil {
		t.Fatal(err)
	}
	stranger := int(st.Uid) + 1
	broken := func(int) ([]int, error) { return nil, errors.New("no such account") }
	missing := filepath.Join(base, "gone")

	block, err := firstUnsearchable([]string{early, missing, late}, stranger, broken)
	if err != nil || block == nil || block.dir != late {
		t.Fatalf("block = %+v, error = %v, want the block at %s", block, err, late)
	}
	block, err = firstUnsearchable([]string{early, missing, open}, stranger, broken)
	if block != nil || err == nil || !strings.Contains(err.Error(), "no such account") {
		t.Fatalf("block = %+v, error = %v, want the first thing that could not be told", block, err)
	}
	if block, err = firstUnsearchable([]string{missing, open}, stranger, broken); block != nil || err == nil {
		t.Fatalf("block = %+v, error = %v, want the missing directory as the error", block, err)
	}
}

// The walk keeps what it found before it stopped, so a block above a loop of
// links is named and not hidden behind the loop.
func TestAncestorBlockNamesABlockAboveALoop(t *testing.T) {
	base := realTemp(t)
	blocker := filepath.Join(base, "blocker")
	makeDir(t, blocker)
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(blocker, "loop"), filepath.Join(blocker, "loop")); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(base, &st); err != nil {
		t.Fatal(err)
	}
	nobody := func(int) ([]int, error) { return nil, nil }
	search := searchBelow(base, nobody)
	block, err := search(filepath.Join(blocker, "loop", "cache"), int(st.Uid)+1)
	if err != nil || block == nil || block.dir != blocker {
		t.Fatalf("block = %+v, error = %v, want the block at %s", block, err, blocker)
	}
	if _, err = search(filepath.Join(blocker, "loop", "cache"), int(st.Uid)); err == nil {
		t.Error("the loop is a failure the CPI meets too, and the walk must say it could not finish")
	}
}

// A pve.endpoint_cache_dir that does not exist is not necessarily a missing
// directory to the CPI. When a directory above it grants the CPI's user no
// search access, the CPI gets permission denied, which is no reason to fall
// back, so the doctor must not promise that the record goes to another
// directory. The same holds for the directory beside the allocation journal.
func TestCollectReportsAMissingDirectoryTheCPIsUserCannotLookFor(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	base := realTemp(t)
	closed := filepath.Join(base, "closed")
	makeDir(t, closed)
	if err := os.Chmod(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	doctor := os.Geteuid()
	cpi := doctor + 1
	nobody := func(int) ([]int, error) { return nil, nil }
	walk := searchBelow(base, nobody)
	collect := func(cfg *config.CPIConfig, search func(string, int) (*searchBlock, error)) *haReport {
		c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}}}
		j := cacheJudge{uid: cpi, known: true, doctor: doctor, ancestors: search}
		c.cacheFindingsAs(j)
		return c.rep
	}

	t.Run("override below a directory the CPI cannot search", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(closed, "cache", "ep")
		r := collect(cfg, walk)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, cfg.EndpointCacheDir, "does not exist as this check sees it",
			"sits below "+closed, "uid "+itoa(cpi)+" cannot search", "permission denied", "does not fall back")
		if n := len(findings(r, haKindCacheDirectory)); n != 1 {
			t.Errorf("%d findings, want only the one that names the block: %+v", n, r.Findings)
		}
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "sits in") {
			t.Errorf("the record does not go to a fallback the CPI never reaches: %q", got)
		}
	})

	t.Run("override the CPI's user can look for", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(base, "absent")
		want(t, collect(cfg, walk), haKindCacheDirectory, haSeverityAdvice, cfg.EndpointCacheDir, "does not exist",
			"sits in the temporary cache directory")
	})

	t.Run("override and the search cannot tell", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(closed, "cache")
		r := collect(cfg, func(string, int) (*searchBlock, error) { return nil, errors.New("no such account") })
		got := findings(r, haKindCacheDirectory)
		if len(got) != 2 {
			t.Fatalf("%d findings, want the doubt and the fallback: %+v", len(got), r.Findings)
		}
		doubt := []string{"could not tell whether uid " + itoa(cpi), "no such account", "does not fall back"}
		for _, text := range doubt {
			if !strings.Contains(got[0].Detail, text) {
				t.Errorf("finding %q lacks %q", got[0].Detail, text)
			}
		}
		if !strings.Contains(got[1].Detail, "sits in the temporary cache directory") {
			t.Errorf("the fallback finding stays: %q", got[1].Detail)
		}
	})

	t.Run("a run as the CPI's own user asks nothing", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.EndpointCacheDir = filepath.Join(closed, "cache")
		c := &haCollector{cfg: cfg, euid: doctor, rep: &haReport{Findings: []haFinding{}}}
		asked := func(string, int) (*searchBlock, error) {
			t.Error("the doctor already looked as the CPI's user")
			return nil, nil
		}
		c.cacheFindingsAs(cacheJudge{uid: doctor, known: true, doctor: doctor, ancestors: asked})
	})

	t.Run("journal sibling", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = filepath.Join(closed, "journal")
		sibling := filepath.Join(closed, "endpoint-cache")
		r := collect(cfg, walk)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "does not exist as this check sees it",
			"sits below "+closed, "permission denied", "temporary cache directory")
		if n := len(findings(r, haKindCacheDirectory)); n != 1 {
			t.Errorf("%d findings, want only the one that names the block: %+v", n, r.Findings)
		}
	})

	t.Run("missing override whose journal sibling is blocked", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = filepath.Join(closed, "journal")
		cfg.EndpointCacheDir = filepath.Join(base, "absent")
		r := collect(cfg, walk)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, cfg.EndpointCacheDir,
			"pve.endpoint_cache_dir does not exist, and", "beside the allocation journal",
			"sits below "+closed, "permission denied")
		if got := findings(r, haKindCacheDirectory)[0].Detail; strings.Contains(got, "sits in") {
			t.Errorf("the record does not go to a directory the CPI never reaches: %q", got)
		}
	})

	t.Run("journal sibling the CPI's user can look for", func(t *testing.T) {
		cfg := haCfg("auto", "none")
		cfg.StorageAllocationJournalDir = filepath.Join(base, "journal")
		sibling := filepath.Join(base, "endpoint-cache")
		r := collect(cfg, walk)
		want(t, r, haKindCacheDirectory, haSeverityAdvice, sibling, "does not create it because",
			"temporary cache directory")
		for _, f := range findings(r, haKindCacheDirectory) {
			if strings.Contains(f.Detail, "permission denied") {
				t.Errorf("the CPI's user can look for the directory, so nothing is denied: %+v", f)
			}
		}
	})
}

// searchBelow is ancestorBlock limited to the directories at or below root, so
// a test judges the directories it made and not the ones the host's temporary
// directory sits in, which may be closed to the user the test stands in for.
func searchBelow(root string, groups func(int) ([]int, error)) func(string, int) (*searchBlock, error) {
	return func(path string, uid int) (*searchBlock, error) {
		dirs, walkErr := searchedDirs(path)
		var below []string
		for _, d := range dirs {
			if d == root || strings.HasPrefix(d, root+string(filepath.Separator)) {
				below = append(below, d)
			}
		}
		block, err := firstUnsearchable(below, uid, groups)
		if block != nil || err != nil {
			return block, err
		}
		return nil, walkErr
	}
}

// realTemp returns a temporary directory with its links resolved, so the paths
// a test compares are the ones the walk produces.
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The mode comes from the host's umask unless the test sets it, and the
	// tests judge this directory as one that every user can search.
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the mode is the test's input
		t.Fatal(err)
	}
	return dir
}

// wantDirs fails the test unless dirs holds every directory in want.
func wantDirs(t *testing.T, dirs []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, d := range dirs {
			found = found || d == w
		}
		if !found {
			t.Errorf("directories %v lack %s", dirs, w)
		}
	}
}
