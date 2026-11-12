package endpointcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	setUmask(t, 0o077)
	choice := resolve(t, Options{Override: override, JournalDir: journal})
	if choice.Layer != LayerOverride || choice.Path != override || !choice.Created {
		t.Fatalf("choice = %s %s created %v, want the created override", choice.Layer, choice.Path, choice.Created)
	}
	requireWritable(t, choice)
	requireMode(t, override, 0o700)
}

func TestAMissingOverrideWithNoJournalIsCreatedOnTheTemporaryDirectorysMount(t *testing.T) {
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
	requireNotCreated(t, noJournal.Err, override, "the temporary directory")
}

func TestAMissingDirectoryIsNotCreatedWhenItsParentIsUnsuitable(t *testing.T) {
	temporaryParent(t)
	other := os.Geteuid() + 1
	cases := map[string]struct {
		layout func(t *testing.T, base string) string
		judge  *judgement
		reason string
		root   bool
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
			judge:  &judgement{set: true, uid: other},
			reason: "and the CPI runs as uid " + strconv.Itoa(other),
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
	requireNotCreated(t, judged.Err, override, "the CPI runs as uid "+strconv.Itoa(other))
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
		t.Skipf("the mount of %s could not be read: %v", base, err)
	}
	b, err := mountOf(child, false)
	if err != nil || a != b {
		t.Fatalf("mountOf(%s) = %+v, mountOf(%s) = %+v (%v), want the same mount", base, a, child, b, err)
	}
	dev, err := mountOf("/dev", true)
	if err != nil {
		t.Skipf("the mount of /dev could not be read: %v", err)
	}
	root, err := mountOf("/", true)
	if err != nil || dev == root {
		t.Fatalf("mountOf(/dev) = %+v, mountOf(/) = %+v (%v), want two mounts", dev, root, err)
	}
}
