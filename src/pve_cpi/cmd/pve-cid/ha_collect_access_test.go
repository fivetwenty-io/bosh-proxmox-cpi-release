package main

import (
	"errors"
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
		cfg.EndpointCacheDir = filepath.Join(t.TempDir(), "absent")
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
		cfg.EndpointCacheDir = filepath.Join(t.TempDir(), "absent")
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
	missing := filepath.Join(t.TempDir(), "absent")
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
		r := collect(t, cfg, blocking("/var/vcap/data"))
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

// The directories above a path are the ones it names and the ones it resolves
// to, because a link in the path is followed with the access of the user who
// opens it, and they come from the root down.
func TestAncestorDirsFollowALinkInThePath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(base, "real", "deep")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	dirs := ancestorDirs(filepath.Join(link, "cache"))
	if dirs[0] != "/" {
		t.Errorf("first ancestor %q, want the root", dirs[0])
	}
	for _, want := range []string{base, link, filepath.Join(base, "real"), deep} {
		found := false
		for _, d := range dirs {
			found = found || d == want
		}
		if !found {
			t.Errorf("ancestors %v lack %s", dirs, want)
		}
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			t.Errorf("ancestors %v repeat %s", dirs, d)
		}
		seen[d] = true
	}
}
