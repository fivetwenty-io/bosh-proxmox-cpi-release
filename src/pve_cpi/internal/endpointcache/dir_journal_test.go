package endpointcache

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// journalSibling returns a journal directory path and the endpoint-cache path
// beside it, both under a fresh parent. Neither exists yet.
func journalSibling(t *testing.T) (journal, sibling string) {
	t.Helper()
	parent := t.TempDir()
	return filepath.Join(parent, "allocations"), filepath.Join(parent, JournalSiblingName)
}

func makePrivateDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestJournalLayerIsChosenWhenTheSiblingExists(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	makePrivateDirectory(t, sibling)
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Layer != LayerJournal || choice.Path != sibling {
		t.Fatalf("choice = %s %s, want %s %s", choice.Layer, choice.Path, LayerJournal, sibling)
	}
	requireWritable(t, choice)
	if len(choice.Notes) != 0 {
		t.Errorf("notes = %v, want none for a usable sibling", choice.Notes)
	}
	if _, err := os.Lstat(filepath.Join(journal, JournalSiblingName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory appeared inside the journal: %v", err)
	}
}

func TestJournalLayerReadsTheJournalFromTheSiblingOfACleanedPath(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	makePrivateDirectory(t, sibling)
	// A trailing slash must not move the sibling inside the journal.
	choice := resolve(t, Options{JournalDir: journal + string(filepath.Separator)})
	if choice.Layer != LayerJournal || choice.Path != sibling {
		t.Fatalf("choice = %s %s, want %s %s", choice.Layer, choice.Path, LayerJournal, sibling)
	}
}

func TestJournalLayerIsUsedWhenTheJournalDirectoryDoesNotExist(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, sibling)
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Layer != LayerJournal || choice.Path != sibling || !choice.Usable() {
		t.Fatalf("choice = %s %s usable %v, want the sibling", choice.Layer, choice.Path, choice.Usable())
	}
}

func TestOverrideWinsOverTheJournalLayer(t *testing.T) {
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, sibling)
	override := privateDirectory(t)
	choice := resolve(t, Options{Override: override, JournalDir: journal})
	if choice.Layer != LayerOverride || choice.Path != override {
		t.Fatalf("choice = %s %s, want the override", choice.Layer, choice.Path)
	}
	// A refused override ends the search, so the sibling is not tried.
	missing := filepath.Join(t.TempDir(), "absent")
	refused := resolve(t, Options{Override: missing, JournalDir: journal})
	if refused.Layer != LayerOverride || refused.Usable() {
		t.Fatalf("choice = %s usable %v, want a refusal by the override", refused.Layer, refused.Usable())
	}
}

func TestMissingSiblingFallsThroughToTheTemporaryLayerWithANote(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	choice := resolve(t, Options{JournalDir: journal, hooks: declineCreation})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) || !choice.Usable() {
		t.Fatalf("choice = %s %s usable %v, want the temporary layer", choice.Layer, choice.Path, choice.Usable())
	}
	if len(choice.Notes) != 1 {
		t.Fatalf("notes = %q, want one", choice.Notes)
	}
	for _, want := range []string{sibling, "journal layer", "missing", "next layer", "does not create it because",
		declineText} {
		if !strings.Contains(choice.Notes[0], want) {
			t.Errorf("note %q lacks %q", choice.Notes[0], want)
		}
	}
	requireAbsent(t, sibling, "the layer created the sibling at runtime")
	if len(choice.Declined) != 1 || choice.Declined[0].Layer != LayerJournal {
		t.Fatalf("declined = %+v, want the journal layer's refusal", choice.Declined)
	}
	var notCreated *NotCreatedError
	if !errors.As(choice.Declined[0], &notCreated) || notCreated.Path != sibling {
		t.Errorf("declined cause %v, want a NotCreatedError for %s", choice.Declined[0].Err, sibling)
	}
}

func TestMissingSiblingAndJournalFallThroughWithoutCreatingEither(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Layer != LayerTemporary {
		t.Fatalf("layer = %s, want %s", choice.Layer, LayerTemporary)
	}
	requireAbsent(t, sibling, "the layer created the sibling at runtime")
	requireAbsent(t, journal, "the layer created the journal directory")
}

func TestSiblingRefusalsGiveNoCacheAndDoNotFallThrough(t *testing.T) {
	cases := []struct {
		name  string
		make  func(t *testing.T, sibling string)
		cause error
	}{
		{"group bits", func(t *testing.T, sibling string) {
			makePrivateDirectory(t, sibling)
			if err := os.Chmod(sibling, 0o770); err != nil {
				t.Fatal(err)
			}
		}, privatefile.ErrUnsafeMode},
		{"other bits", func(t *testing.T, sibling string) {
			makePrivateDirectory(t, sibling)
			if err := os.Chmod(sibling, 0o705); err != nil {
				t.Fatal(err)
			}
		}, privatefile.ErrUnsafeMode},
		{"owner without search access", func(t *testing.T, sibling string) {
			makePrivateDirectory(t, sibling)
			if err := os.Chmod(sibling, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(sibling, 0o700) })
		}, privatefile.ErrUnsafeMode},
		{"symlink to a private directory", func(t *testing.T, sibling string) {
			if err := os.Symlink(privateDirectory(t), sibling); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"dangling symlink", func(t *testing.T, sibling string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), sibling); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"regular file", func(t *testing.T, sibling string) {
			if err := os.WriteFile(sibling, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, privatefile.ErrUnsafeFileType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := temporaryParent(t)
			journal, sibling := journalSibling(t)
			c.make(t, sibling)
			choice := resolve(t, Options{JournalDir: journal})
			if choice.Layer != LayerJournal || choice.Path != sibling {
				t.Fatalf("choice = %s %s, want a refusal by the journal layer at %s",
					choice.Layer, choice.Path, sibling)
			}
			if c.cause != nil {
				requireUnsafe(t, choice, c.cause)
			} else if choice.Usable() || choice.Err == nil || !strings.Contains(choice.Err.Error(), "symlink") {
				t.Fatalf("a symlinked sibling gave %+v, want a symlink refusal", choice)
			}
			requireAbsent(t, temporaryPath(parent), "a refused sibling fell through to the temporary layer")
		})
	}
}

func TestSiblingRefusesWrongOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only root can create a directory that belongs to another user")
	}
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, sibling)
	if err := os.Chown(sibling, foreignUID, -1); err != nil {
		t.Skipf("cannot give a directory to uid %d: %v", foreignUID, err)
	}
	choice := resolve(t, Options{JournalDir: journal})
	requireUnsafe(t, choice, privatefile.ErrUnsafeOwnership)
	if choice.Layer != LayerJournal || choice.Path != sibling {
		t.Fatalf("choice = %s %s, want a refusal by the journal layer", choice.Layer, choice.Path)
	}
	requireAbsent(t, temporaryPath(parent), "a refused sibling fell through to the temporary layer")
}

// Another user's directory cannot be made without root, so the policy's owner
// is moved instead: the process's own sibling then belongs to someone else. The
// sibling must be refused for its owner, with no fall-through, exactly as an
// override directory is, because both reach the private checks through the same
// policy. This runs for every user, so a sibling that skipped the owner check
// fails here.
func TestSiblingRefusesAnOwnerThatIsNotTheEffectiveUser(t *testing.T) {
	for _, name := range []string{"", HAProfileName} {
		t.Run("display name "+name, func(t *testing.T) {
			parent := temporaryParent(t)
			journal, sibling := journalSibling(t)
			makePrivateDirectory(t, sibling)
			override := privateDirectory(t)
			// textsFor copies cachePrivatePolicy for a named cache and
			// endpointTexts holds its own copy, so both move.
			previousBase, previousTexts := cachePrivatePolicy.EUID, endpointTexts.policy.EUID
			someoneElse := func() int { return os.Geteuid() + 1 }
			cachePrivatePolicy.EUID, endpointTexts.policy.EUID = someoneElse, someoneElse
			t.Cleanup(func() { cachePrivatePolicy.EUID, endpointTexts.policy.EUID = previousBase, previousTexts })

			// The override layer shows the policy refuses the owner at all.
			requireUnsafe(t, resolve(t, Options{Override: override, DisplayName: name}), privatefile.ErrUnsafeOwnership)

			choice := resolve(t, Options{JournalDir: journal, DisplayName: name})
			requireUnsafe(t, choice, privatefile.ErrUnsafeOwnership)
			if choice.Layer != LayerJournal || choice.Path != sibling {
				t.Fatalf("choice = %s %s, want a refusal by the journal layer at %s",
					choice.Layer, choice.Path, sibling)
			}
			requireAbsent(t, temporaryPath(parent), "a refused sibling fell through to the temporary layer")
		})
	}
}

func TestSiblingUnderASymlinkedAncestorIsFoundThroughTheConfiguredPath(t *testing.T) {
	temporaryParent(t)
	target := t.TempDir()
	linked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	makePrivateDirectory(t, filepath.Join(target, JournalSiblingName))
	// The private checks look at the final component, as the journal's do, so an
	// ancestor link that the operator configured still reaches the sibling.
	sibling := filepath.Join(linked, JournalSiblingName)
	choice := resolve(t, Options{JournalDir: filepath.Join(linked, "allocations")})
	if choice.Layer != LayerJournal || choice.Path != sibling || !choice.Usable() {
		t.Fatalf("choice = %s %s usable %v, want the sibling under the configured path %s", choice.Layer,
			choice.Path, choice.Usable(), sibling)
	}
}

func TestJournalNamedEndpointCacheSkipsTheLayer(t *testing.T) {
	parent := temporaryParent(t)
	root := t.TempDir()
	journal := filepath.Join(root, JournalSiblingName)
	makePrivateDirectory(t, journal)
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) || !choice.Usable() {
		t.Fatalf("choice = %s %s, want the temporary layer", choice.Layer, choice.Path)
	}
	if len(choice.Notes) != 1 || !strings.Contains(choice.Notes[0], JournalSiblingName) ||
		!strings.Contains(choice.Notes[0], "journal layer") {
		t.Errorf("notes = %q, want one that names the collision", choice.Notes)
	}
	// The journal itself is never mistaken for the cache.
	if choice.Path == journal {
		t.Error("the journal directory was chosen as the cache")
	}
	if names := storeDirNames(t, journal); len(names) != 0 {
		t.Errorf("the journal directory holds %v, want nothing written", names)
	}
}

func TestJournalLayerDeclinesWithoutAUsableJournalPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		journal string
		notes   int
		reason  string
	}{
		{"unset", "", 0, ""},
		{"relative", "var/vcap/store/pve_cpi/allocations", 1, "not an absolute path below the root"},
		{"filesystem root", "/", 1, "not an absolute path below the root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := temporaryParent(t)
			choice := resolve(t, Options{JournalDir: tc.journal})
			if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) {
				t.Fatalf("choice = %s %s, want the temporary layer", choice.Layer, choice.Path)
			}
			if len(choice.Notes) != tc.notes {
				t.Fatalf("notes = %q, want %d", choice.Notes, tc.notes)
			}
			if tc.notes == 1 && !strings.Contains(choice.Notes[0], tc.reason) {
				t.Errorf("note %q lacks %q, so the layer declined for another reason", choice.Notes[0], tc.reason)
			}
		})
	}
}

// A relative journal directory has no place to look beside, even when the
// working directory holds a directory shaped like the sibling. The layer
// declines it, so the working directory never becomes a cache.
func TestJournalLayerDeclinesARelativeJournalEvenWhenTheSiblingExistsUnderTheWorkingDirectory(t *testing.T) {
	parent := temporaryParent(t)
	work := t.TempDir()
	relativeSibling := filepath.Join("var", "vcap", "store", "pve_cpi", JournalSiblingName)
	if err := os.MkdirAll(filepath.Join(work, relativeSibling), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	choice := resolve(t, Options{JournalDir: filepath.Join("var", "vcap", "store", "pve_cpi", "allocations")})
	if choice.Layer != LayerTemporary || choice.Path != temporaryPath(parent) || !choice.Usable() {
		t.Fatalf("choice = %s %s usable %v (%v), want the temporary layer",
			choice.Layer, choice.Path, choice.Usable(), choice.Err)
	}
	if len(choice.Notes) != 1 || !strings.Contains(choice.Notes[0], "not an absolute path below the root") {
		t.Errorf("notes = %q, want one that says the journal is not absolute", choice.Notes)
	}
}

// A stat error other than "does not exist" leaves the sibling's absence
// unproven, so the layer refuses it and the temporary layer is never tried.
// These cases need no permission bits, so they also run as root, and they ask
// the layer itself, with no guard in front of it, so widening the check to
// treat every stat error as an absent sibling fails here.
func TestUnprovenAbsenceOfTheSiblingRefusesTheLayer(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, base string)
		cause syscall.Errno
	}{
		{"an ancestor that is a regular file", func(t *testing.T, base string) {
			if err := os.WriteFile(base, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, syscall.ENOTDIR},
		{"an ancestor that is a symlink loop", func(t *testing.T, base string) {
			if err := os.Symlink(base, base); err != nil {
				t.Fatal(err)
			}
		}, syscall.ELOOP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := temporaryParent(t)
			base := filepath.Join(t.TempDir(), "ancestor")
			c.build(t, base)
			choice := resolve(t, Options{JournalDir: filepath.Join(base, "allocations")})
			sibling := filepath.Join(base, JournalSiblingName)
			if choice.Layer != LayerJournal || choice.Path != sibling {
				t.Fatalf("choice = %s %s, want a refusal by the journal layer at %s",
					choice.Layer, choice.Path, sibling)
			}
			if choice.Root != nil || choice.Usable() {
				t.Error("the refused layer holds an opened directory")
			}
			if choice.Err == nil || !errors.Is(choice.Err, c.cause) {
				t.Errorf("err = %v, want it to wrap %v", choice.Err, c.cause)
			}
			if errors.Is(choice.Err, os.ErrNotExist) {
				t.Errorf("err = %v must not read as an absent directory", choice.Err)
			}
			if len(choice.Notes) != 0 {
				t.Errorf("notes = %q, want none, because the layer did not decline", choice.Notes)
			}
			requireAbsent(t, temporaryPath(parent), "an unprovable sibling fell through to the temporary layer")
		})
	}
}

func TestJournalLayerCarriesARefusalInErrAndNoNote(t *testing.T) {
	// A refused sibling carries its reason in Err and no note, which leaves the
	// level of the log line to the caller.
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, sibling)
	if err := os.Chmod(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{JournalDir: journal})
	if choice.Err == nil || len(choice.Notes) != 0 {
		t.Errorf("err = %v, notes = %q, want the refusal in Err and no notes", choice.Err, choice.Notes)
	}
}

// The layer chooser writes no log line, because its caller sets the level. This
// keeps a logging import out of dir.go, which would let a future call slip a
// line in without a test noticing.
func TestTheDirectoryChooserImportsNoLogger(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "dir.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == "log" || strings.HasPrefix(path, "log/") || strings.HasSuffix(path, "/internal/log") ||
			strings.Contains(path, "zap") {
			t.Errorf("dir.go imports the logging package %s", path)
		}
	}
}

func TestJournalLayerUsesTheDisplayNameInItsRefusal(t *testing.T) {
	temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, sibling)
	if err := os.Chmod(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	text := resolve(t, Options{JournalDir: journal, DisplayName: HAProfileName}).Err.Error()
	if !strings.Contains(text, "HA profile cache: unsafe permissions") || !strings.Contains(text, "journal layer") {
		t.Errorf("refusal %q, want the HA profile cache and the journal layer", text)
	}
}

func TestResolveWithCollectsTheNotesOfDecliningLayers(t *testing.T) {
	declining := func(reason string) layer {
		return func(Options) (Choice, bool) {
			if reason == "" {
				return Choice{}, false
			}
			return Choice{Err: &Refusal{Layer: "x", Path: "/p", Err: errors.New(reason)}}, false
		}
	}
	answer := func(Options) (Choice, bool) { return Choice{Layer: "last"}, false }
	got := resolveWith([]layer{declining("one"), declining(""), declining("two"), answer}, Options{})
	if got.Layer != "last" || len(got.Notes) != 2 || !strings.Contains(got.Notes[0], "one") ||
		!strings.Contains(got.Notes[1], "two") {
		t.Fatalf("choice = %+v, want the notes of the two layers that gave a reason", got)
	}
	middle := func(Options) (Choice, bool) { return Choice{Layer: "mid"}, true }
	applied := resolveWith([]layer{declining("one"), middle, answer}, Options{})
	if applied.Layer != "mid" || len(applied.Notes) != 1 {
		t.Fatalf("choice = %+v, want the middle layer with one note", applied)
	}
}

// An HA profile cache and a recovery-episode record build their options with
// OptionsFor, so a journal directory in the config reaches the layer with no
// change of their own. The profile file lands in the sibling.
func TestHAProfileFileLandsBesideTheJournal(t *testing.T) {
	parent := temporaryParent(t)
	journal, sibling := journalSibling(t)
	makePrivateDirectory(t, journal)
	makePrivateDirectory(t, sibling)
	cfg := &config.CPIConfig{StorageAllocationJournalDir: journal}
	choice := OptionsFor(cfg).Named(HAProfileName).Resolve()
	if choice.Layer != LayerJournal || !choice.Usable() || choice.Path != sibling {
		t.Fatalf("choice = %s %s: %v, want the sibling", choice.Layer, choice.Path, choice.Err)
	}
	store, err := OpenStore(
		context.Background(), choice.Root, storeTestConfig(), storeTestCredential, haProfileOptions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	storePut(t, store, "profile")
	if !strings.HasPrefix(store.FileName(), HAProfilePrefix) {
		t.Fatalf("file %q lacks the HA profile prefix %q", store.FileName(), HAProfilePrefix)
	}
	if _, err := os.Lstat(filepath.Join(sibling, store.FileName())); err != nil {
		t.Errorf("the HA profile file is not in the sibling: %v; it holds %v", err, storeDirNames(t, sibling))
	}
	if names := storeDirNames(t, journal); len(names) != 0 {
		t.Errorf("the journal directory holds %v, want nothing written", names)
	}
	requireAbsent(t, temporaryPath(parent), "the temporary layer was created although the sibling was usable")
}
