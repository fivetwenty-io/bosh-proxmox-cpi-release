package endpointcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// declineText is the reason declineCreation gives.
const declineText = "the test declines to create it"

// declineCreation makes every layer decline to create its missing directory,
// for the tests whose subject is what happens after a layer declines.
var declineCreation = &creationHooks{mount: func(string, bool) (mountKey, error) {
	return mountKey{}, errors.New(declineText)
}}

// twoMounts reports anchor on one mount and everything else on another, so a
// test can fake a journal directory that BPM mounted by itself.
func twoMounts(anchor string) *creationHooks {
	return &creationHooks{mount: func(path string, _ bool) (mountKey, error) {
		if path == anchor {
			return mountKey{dev: 1, mount: "volume"}, nil
		}
		return mountKey{dev: 2, mount: "private"}, nil
	}}
}

// requireNotCreated checks that choice is a refusal or a note whose cause is a
// NotCreatedError for path that names reason, and that path is still absent.
func requireNotCreated(t *testing.T, refusal *Refusal, path, reason string) {
	t.Helper()
	if refusal == nil {
		t.Fatal("no refusal")
	}
	var notCreated *NotCreatedError
	if !errors.As(refusal, &notCreated) || notCreated.Path != path {
		t.Fatalf("refusal %v, want a NotCreatedError for %s", refusal, path)
	}
	if !errors.Is(refusal, fs.ErrNotExist) {
		t.Errorf("refusal %v does not match fs.ErrNotExist", refusal)
	}
	if !strings.Contains(refusal.Error(), reason) {
		t.Errorf("refusal %q does not name %q", refusal.Error(), reason)
	}
	requireAbsent(t, path, "a declined directory was created")
}

func TestAMissingOverrideIsCreatedOnTheJournalsMount(t *testing.T) {
	temporaryParent(t)
	journal, _ := journalSibling(t)
	makePrivateDirectory(t, journal)
	override := filepath.Join(filepath.Dir(journal), "cache")
	// mkdir at 0700 under this mask leaves 0500, so only the chmod after the
	// creation gives the owner the write access the checks require.
	setUmask(t, 0o277)
	choice := resolve(t, Options{Override: override, JournalDir: journal})
	if choice.Layer != LayerOverride || choice.Path != override || !choice.Created {
		t.Fatalf("choice = %s %s created %v, want the created override", choice.Layer, choice.Path, choice.Created)
	}
	requireWritable(t, choice)
	requireMode(t, override, 0o700)
}

func TestAMissingOverrideWithNoJournalIsCreatedOnTheTemporaryDirectorysMount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("with no journal, root never creates the override")
	}
	parent := temporaryParent(t)
	base := filepath.Join(parent, "operator")
	makePrivateDirectory(t, base)
	override := filepath.Join(base, "cache")
	choice := resolve(t, Options{Override: override})
	if choice.Layer != LayerOverride || !choice.Created {
		t.Fatalf("choice = %s created %v err %v, want the created override", choice.Layer, choice.Created,
			choice.Err)
	}
	requireWritable(t, choice)
	requireAbsent(t, temporaryPath(parent), "the temporary layer was used")
}

func TestAMissingSiblingIsCreatedBesideTheJournal(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	setUmask(t, 0o277)
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Layer != LayerJournal || choice.Path != sibling || !choice.Created {
		t.Fatalf("choice = %s %s created %v err %v, want the created sibling", choice.Layer, choice.Path,
			choice.Created, choice.Err)
	}
	requireWritable(t, choice)
	requireMode(t, sibling, 0o700)
	if len(choice.Notes) != 0 || len(choice.Declined) != 0 {
		t.Errorf("notes %v declined %v, want none", choice.Notes, choice.Declined)
	}
	requireAbsent(t, temporaryPath(parent), "the temporary layer was used")

	again := resolve(t, Options{JournalDir: journal})
	if !again.Usable() || again.Created {
		t.Errorf("second choice usable %v created %v, want the existing sibling reused", again.Usable(),
			again.Created)
	}
}

// When the journal directory is a mount point of its own, as it is when BPM
// mounts only the journal into the worker, its parent is the worker's private
// filesystem, so the sibling is not created there and the temporary layer
// answers as it did before.
func TestASiblingWhoseParentIsOnAnotherMountIsNotCreated(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	choice := resolve(t, Options{JournalDir: journal, hooks: twoMounts(journal)})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) || !choice.Usable() {
		t.Fatalf("choice = %s %s usable %v, want the temporary layer", choice.Layer, choice.Path, choice.Usable())
	}
	if len(choice.Declined) != 1 {
		t.Fatalf("declined = %v, want the sibling", choice.Declined)
	}
	requireNotCreated(t, choice.Declined[0], sibling, "is not on the same mount as the allocation journal")
}

// A missing override whose parent is on another mount from the journal is not
// created, and its refusal still matches fs.ErrNotExist, so the HA recovery
// episode record falls back from it as it always has.
func TestAnOverrideWhoseParentIsOnAnotherMountIsNotCreated(t *testing.T) {
	temporaryParent(t)
	journal, _ := journalSibling(t)
	makePrivateDirectory(t, journal)
	override := filepath.Join(t.TempDir(), "cache")
	choice := resolve(t, Options{Override: override, JournalDir: journal, hooks: twoMounts(journal)})
	if choice.Layer != LayerOverride || choice.Usable() {
		t.Fatalf("choice = %s usable %v, want a refusal by the override", choice.Layer, choice.Usable())
	}
	requireNotCreated(t, choice.Err, override, "might not persist")

	noJournal := resolve(t, Options{Override: override, hooks: twoMounts(os.TempDir())})
	reason := "the temporary directory"
	if os.Geteuid() == 0 {
		reason = "it would be made by root"
	}
	requireNotCreated(t, noJournal.Err, override, reason)
}

func TestAMissingDirectoryIsNotCreatedWhenItsParentIsUnsuitable(t *testing.T) {
	temporaryParent(t)
	other := os.Geteuid() + 1
	cases := map[string]struct {
		layout func(t *testing.T, base string) string
		judge  *judgement
		reason string
		root   bool
		// noJournal leaves the journal unset, so the anchor names no user
		// and the parent's own owner is what the case tests.
		noJournal bool
	}{
		"missing parent": {
			layout: func(_ *testing.T, base string) string { return filepath.Join(base, "absent") },
			reason: "does not exist",
		},
		"symlinked parent": {
			layout: func(t *testing.T, base string) string {
				real := filepath.Join(base, "real")
				makePrivateDirectory(t, real)
				link := filepath.Join(base, "link")
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			reason: "is a symbolic link",
		},
		"parent owned by another user": {
			layout: func(t *testing.T, base string) string {
				dir := filepath.Join(base, "theirs")
				makePrivateDirectory(t, dir)
				return dir
			},
			judge:     &judgement{set: true, uid: other},
			reason:    "and the CPI runs as uid " + strconv.Itoa(other),
			noJournal: true,
		},
		"group-writable parent": {
			layout: func(t *testing.T, base string) string {
				dir := filepath.Join(base, "shared")
				makePrivateDirectory(t, dir)
				if err := os.Chmod(dir, 0o770); err != nil { // #nosec G302 -- the test opens the mode on purpose
					t.Fatal(err)
				}
				return dir
			},
			reason: "lets a group or others write",
		},
		"parent without owner write": {
			layout: func(t *testing.T, base string) string {
				dir := filepath.Join(base, "readonly")
				makePrivateDirectory(t, dir)
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- restores the test's directory
				return dir
			},
			reason: "does not give its owner write",
			root:   true,
		},
		"world-writable directory above the parent": {
			layout: func(t *testing.T, base string) string {
				open := filepath.Join(base, "open")
				makePrivateDirectory(t, open)
				if err := os.Chmod(open, 0o777); err != nil { // #nosec G302 -- the test opens the mode on purpose
					t.Fatal(err)
				}
				dir := filepath.Join(open, "mine")
				makePrivateDirectory(t, dir)
				return dir
			},
			reason: "lets a group or others replace",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.root && os.Geteuid() == 0 {
				t.Skip("root writes in a directory whatever its owner bits say")
			}
			base := t.TempDir()
			journal := filepath.Join(base, "allocations")
			makePrivateDirectory(t, journal)
			parent := tc.layout(t, base)
			override := filepath.Join(parent, "cache")
			opts := Options{Override: override, JournalDir: journal}
			if tc.noJournal {
				opts.JournalDir = ""
			}
			if tc.judge != nil {
				opts.judge = *tc.judge
			}
			choice := resolve(t, opts)
			if choice.Layer != LayerOverride || choice.Usable() {
				t.Fatalf("choice = %s usable %v, want a refusal by the override", choice.Layer, choice.Usable())
			}
			requireNotCreated(t, choice.Err, override, tc.reason)
		})
	}
}

// Two workers can race to create the same directory. The one that loses gets
// EEXIST and goes on to the private checks, which accept the directory the
// winner made and refuse one that someone else made unsafe.
func TestALayerThatLosesTheRaceToCreateGoesOnToTheChecks(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	lose := func(mode os.FileMode) *creationHooks {
		return &creationHooks{mkdir: func(path string, _ os.FileMode) error {
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(path, mode); err != nil {
				return err
			}
			return &os.PathError{Op: "mkdir", Path: path, Err: syscall.EEXIST}
		}}
	}
	choice := resolve(t, Options{JournalDir: journal, hooks: lose(0o700)})
	if choice.Layer != LayerJournal || choice.Created {
		t.Fatalf("choice = %s created %v, want the sibling another worker created", choice.Layer, choice.Created)
	}
	requireWritable(t, choice)
	_ = choice.Close()

	if err := os.RemoveAll(sibling); err != nil {
		t.Fatal(err)
	}
	wide := resolve(t, Options{JournalDir: journal, hooks: lose(0o755)})
	if wide.Layer != LayerJournal || errors.Is(wide.Err, fs.ErrNotExist) {
		t.Fatalf("choice = %s err %v, want the sibling refused, not missing", wide.Layer, wide.Err)
	}
	requireUnsafe(t, wide, errCacheMode)
}

// A creation that fails for any reason but EEXIST declines, so the layer
// behaves as it did for a missing directory.
func TestAFailedCreationDeclines(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	fail := &creationHooks{mkdir: func(path string, _ os.FileMode) error {
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.EROFS}
	}}
	choice := resolve(t, Options{JournalDir: journal, hooks: fail})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) {
		t.Fatalf("choice = %s %s, want the temporary layer", choice.Layer, choice.Path)
	}
	requireNotCreated(t, choice.Declined[0], sibling, "creating it failed")
	if !errors.Is(choice.Declined[0], syscall.EROFS) {
		t.Errorf("declined %v, want the mkdir error", choice.Declined[0])
	}
}

// The journal check and the caller's guard run before the CPI creates
// anything, so a path either refuses is declined and nothing is left behind.
func TestTheGuardsRunBeforeAMissingDirectoryIsCreated(t *testing.T) {
	temporaryParent(t)
	journal, _ := journalSibling(t)
	makePrivateDirectory(t, journal)

	inside := filepath.Join(journal, "cache")
	choice := resolve(t, Options{Override: inside, JournalDir: journal})
	requireNotCreated(t, choice.Err, inside, "refused before anything was created")
	if !errors.Is(choice.Err, ErrJournalOverlap) {
		t.Errorf("refusal %v, want ErrJournalOverlap", choice.Err)
	}

	override := filepath.Join(filepath.Dir(journal), "cache")
	guard := &refusingGuard{err: errors.New("the caller refuses it")}
	refused := resolve(t, Options{Override: override, JournalDir: journal, Guard: guard})
	requireNotCreated(t, refused.Err, override, "the caller refuses it")
	if len(guard.paths) != 1 || guard.paths[0] != override {
		t.Errorf("guard asked about %v, want %s once", guard.paths, override)
	}
}

// refusingGuard refuses every path it is asked about and records them.
type refusingGuard struct {
	err   error
	paths []string
}

func (g *refusingGuard) BeforeCreate(path string) error {
	g.paths = append(g.paths, path)
	return g.err
}

// Probe answers what Resolve would do for a missing directory without creating
// it, and names the reason when Resolve would decline.
func TestProbeReportsWhetherAMissingDirectoryWouldBeCreated(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)

	probe := Probe(Options{JournalDir: journal})
	if probe.Layer != LayerJournal || probe.Path != sibling || probe.Usable() ||
		!errors.Is(probe.Err, ErrCreatedOnFirstUse) || !errors.Is(probe.Err, fs.ErrNotExist) {
		t.Fatalf("probe = %s %s err %v, want the sibling created on first use", probe.Layer, probe.Path, probe.Err)
	}
	requireAbsent(t, sibling, "the probe created the sibling")

	override := filepath.Join(filepath.Dir(journal), "cache")
	probe = Probe(Options{Override: override, JournalDir: journal})
	if probe.Layer != LayerOverride || !errors.Is(probe.Err, ErrCreatedOnFirstUse) {
		t.Fatalf("probe = %s err %v, want the override created on first use", probe.Layer, probe.Err)
	}
	requireAbsent(t, override, "the probe created the override")

	other := os.Geteuid() + 1
	judged := ProbeAs(Options{Override: override, JournalDir: journal}, other)
	requireNotCreated(t, judged.Err, override, "it would be made by uid "+strconv.Itoa(other))
	if errors.Is(judged.Err, ErrCreatedOnFirstUse) {
		t.Errorf("a declined probe matches ErrCreatedOnFirstUse: %v", judged.Err)
	}

	declined := Probe(Options{JournalDir: journal, hooks: twoMounts(journal)})
	if declined.Layer != LayerTemporary || len(declined.Declined) != 1 {
		t.Fatalf("probe = %s declined %v, want the temporary layer after the sibling", declined.Layer,
			declined.Declined)
	}
	requireNotCreated(t, declined.Declined[0], sibling, "not on the same mount")
	if !errors.Is(declined.Err, ErrCreatedOnFirstUse) {
		t.Errorf("the absent temporary directory's refusal %v does not match ErrCreatedOnFirstUse", declined.Err)
	}
	requireAbsent(t, temporaryPath(parent), "the probe created the temporary directory")
}

// The mount lookup tells two mounts apart and puts two directories on one
// filesystem on the same mount.
func TestMountOfTellsMountsApart(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "child")
	makePrivateDirectory(t, child)
	a, err := mountOf(base, true)
	if err != nil {
		skipOffLinux(t, "the mount of %s could not be read: %v", base, err)
	}
	b, err := mountOf(child, false)
	if err != nil || a != b {
		t.Fatalf("mountOf(%s) = %+v, mountOf(%s) = %+v (%v), want the same mount", base, a, child, b, err)
	}
	dev, err := mountOf("/dev", true)
	if err != nil {
		skipOffLinux(t, "the mount of /dev could not be read: %v", err)
	}
	root, err := mountOf("/", true)
	if err != nil || dev == root {
		t.Fatalf("mountOf(/dev) = %+v, mountOf(/) = %+v (%v), want two mounts", dev, root, err)
	}
}

// skipOffLinux skips the test with the message, except on Linux, where it
// fails instead. There the CPI needs statx mount IDs to create anything, so a
// kernel or a seccomp profile that withholds them must not pass unseen.
func skipOffLinux(t *testing.T, format string, args ...any) {
	t.Helper()
	if runtime.GOOS == "linux" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// asAnotherOwner returns a judgement as a user that does not own journal. When
// the test runs as root it gives journal to another user and judges as root,
// which is the operator's sudo the rule exists for. Otherwise it judges as root
// while the test's own user owns journal.
func asAnotherOwner(t *testing.T, journal string) judgement {
	t.Helper()
	if os.Geteuid() == 0 {
		if err := os.Chown(journal, 54321, 54321); err != nil {
			t.Fatal(err)
		}
	}
	return judgement{set: true, uid: 0}
}

// A process that is not the journal directory's owner, such as an operator's
// sudo of a CPI command, creates neither directory, so it never leaves one
// that the CPI's own user would refuse. The override still matches
// fs.ErrNotExist, so the HA recovery episode record falls back from it.
func TestOnlyTheJournalsOwnerCreatesAMissingDirectory(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	judge := asAnotherOwner(t, journal)

	choice := resolve(t, Options{JournalDir: journal, judge: judge})
	if choice.Layer != LayerTemporary || filepath.Dir(choice.Path) != parent || len(choice.Declined) != 1 {
		t.Fatalf("choice = %s %s declined %v, want the temporary layer after the sibling", choice.Layer,
			choice.Path, choice.Declined)
	}
	requireNotCreated(t, choice.Declined[0], sibling, "the user the CPI runs as, and it would be made by uid 0")

	override := filepath.Join(filepath.Dir(journal), "cache")
	refused := resolve(t, Options{Override: override, JournalDir: journal, judge: judge})
	requireNotCreated(t, refused.Err, override, "it would be made by uid 0")
}

// With no journal nothing names the CPI's user, so root creates no override.
func TestRootCreatesNoOverrideWithoutAJournal(t *testing.T) {
	parent := temporaryParent(t)
	base := filepath.Join(parent, "operator")
	makePrivateDirectory(t, base)
	override := filepath.Join(base, "cache")
	choice := resolve(t, Options{Override: override, judge: judgement{set: true, uid: 0}})
	requireNotCreated(t, choice.Err, override, "it would be made by root")
}

// A directory this call made that the private checks then refuse, as on a
// filesystem that forces its own mode, is removed, so the layer declines as it
// did for a missing directory instead of refusing it on every later call. A
// directory that cannot be removed is refused.
func TestACreatedDirectoryThatFailsTheChecksIsRemoved(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	forced := func(fill bool) *creationHooks {
		return &creationHooks{mkdir: func(path string, perm os.FileMode) error {
			if err := os.Mkdir(path, perm); err != nil {
				return err
			}
			if fill {
				if err := os.WriteFile(filepath.Join(path, "held"), nil, 0o600); err != nil {
					return err
				}
			}
			return os.Chmod(path, 0o750) // #nosec G302 -- the test fakes a filesystem that forces the mode
		}}
	}
	choice := resolve(t, Options{JournalDir: journal, hooks: forced(false)})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) || len(choice.Declined) != 1 {
		t.Fatalf("choice = %s %s declined %v, want the temporary layer after the sibling", choice.Layer,
			choice.Path, choice.Declined)
	}
	requireNotCreated(t, choice.Declined[0], sibling, "so it removed it again")

	kept := resolve(t, Options{JournalDir: journal, hooks: forced(true)})
	if kept.Layer != LayerJournal || kept.Usable() || kept.Err == nil ||
		!strings.Contains(kept.Err.Error(), "removing the directory this call created failed") {
		t.Fatalf("choice = %s usable %v err %v, want the sibling refused", kept.Layer, kept.Usable(), kept.Err)
	}
}

// Resolving the parent can pass through a directory that holds a symbolic
// link. That directory is checked too, because whoever may write in it can
// point the link somewhere else between the checks and the mkdir.
func TestADirectoryHoldingALinkAboveTheParentIsChecked(t *testing.T) {
	temporaryParent(t)
	base := t.TempDir()
	journal := filepath.Join(base, "allocations")
	makePrivateDirectory(t, journal)
	target := filepath.Join(base, "real")
	makePrivateDirectory(t, target)
	makePrivateDirectory(t, filepath.Join(target, "mine"))
	open := filepath.Join(base, "open")
	makePrivateDirectory(t, open)
	if err := os.Symlink(target, filepath.Join(open, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	override := filepath.Join(open, "link", "mine", "cache")
	choice := resolve(t, Options{Override: override, JournalDir: journal})
	requireNotCreated(t, choice.Err, override, filepath.Base(base)+"/open above it has mode 0777")

	// The same link held in a private directory is followed and accepted.
	if err := os.Chmod(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := trustedAncestors(filepath.Join(open, "link", "mine"), os.Geteuid()); err != nil {
		t.Errorf("a link in a private directory was refused: %v", err)
	}
}

// A sticky directory above the parent may let others write, as /tmp does,
// because no one but the owner can replace what it holds. A directory above
// the parent that a third user owns is refused.
func TestTheOwnersAndModesAboveTheParent(t *testing.T) {
	base := t.TempDir()
	sticky := filepath.Join(base, "sticky")
	makePrivateDirectory(t, sticky)
	if err := os.Chmod(sticky, 0o1777|os.ModeSticky); err != nil { // #nosec G302 -- the mode is the subject
		t.Fatal(err)
	}
	mine := filepath.Join(sticky, "mine")
	makePrivateDirectory(t, mine)
	if err := trustedAncestors(mine, os.Geteuid()); err != nil {
		t.Errorf("a sticky world-writable directory above the parent was refused: %v", err)
	}

	theirs := filepath.Join(base, "theirs")
	makePrivateDirectory(t, theirs)
	inside := filepath.Join(theirs, "inside")
	makePrivateDirectory(t, inside)
	third, judged := os.Geteuid(), os.Geteuid()+1
	if os.Geteuid() == 0 {
		third, judged = 54321, 4242
		if err := os.Chown(theirs, third, third); err != nil {
			t.Fatal(err)
		}
	}
	err := trustedAncestors(inside, judged)
	if err == nil || !strings.Contains(err.Error(), "owned by uid "+strconv.Itoa(third)) {
		t.Errorf("trustedAncestors = %v, want the directory owned by uid %d refused", err, third)
	}
}

// A directory the CPI makes takes the process's group even under a parent
// that would hand it another one, so cpi provision-journal accepts it.
func TestACreatedDirectoryTakesTheProcesssGroup(t *testing.T) {
	temporaryParent(t)
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getegid() {
			other = g
			break
		}
	}
	if other < 0 {
		t.Skip("the test user belongs to no group but its own, so the parent cannot take another")
	}
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	dir := filepath.Dir(journal)
	if err := os.Chown(dir, -1, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{JournalDir: journal})
	requireWritable(t, choice)
	info, err := os.Lstat(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if gid := info.Sys().(*syscall.Stat_t).Gid; int(gid) != os.Getegid() {
		t.Errorf("the created directory has gid %d, want the process's %d", gid, os.Getegid())
	}
	if info.Mode()&os.ModeSetgid != 0 {
		t.Errorf("the created directory kept the setgid bit: %v", info.Mode())
	}
}
