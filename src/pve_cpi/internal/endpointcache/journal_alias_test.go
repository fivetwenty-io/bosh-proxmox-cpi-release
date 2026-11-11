package endpointcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// journalBeside makes a journal directory and returns it with the path of the
// endpoint-cache directory beside it, which it does not create.
func journalBeside(t *testing.T) (journal, sibling string) {
	t.Helper()
	parent := t.TempDir()
	journal = filepath.Join(parent, "allocations")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	return journal, filepath.Join(parent, "endpoint-cache")
}

// The directory beside the journal is not the journal and holds no journal, so
// the alias check that guards every layer lets it through, whether the sibling
// exists yet or not.
func TestAliasesJournalLetsTheSiblingPass(t *testing.T) {
	journal, sibling := journalBeside(t)
	aliased, err := AliasesJournal(sibling, journal)
	if err != nil || aliased {
		t.Fatalf("a missing sibling: aliased %v, err %v, want neither", aliased, err)
	}
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	aliased, err = AliasesJournal(sibling, journal)
	if err != nil || aliased {
		t.Fatalf("an existing sibling: aliased %v, err %v, want neither", aliased, err)
	}
	// A journal that is a symlink to a directory elsewhere, and a parent that is
	// reached through a link, leave the sibling beside the link and still apart.
	target := filepath.Join(t.TempDir(), "journal-target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(sibling), "journal-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	aliased, err = AliasesJournal(sibling, link)
	if err != nil || aliased {
		t.Fatalf("a symlinked journal: aliased %v, err %v, want neither", aliased, err)
	}
	via := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(filepath.Dir(sibling), via); err != nil {
		t.Fatal(err)
	}
	aliased, err = AliasesJournal(filepath.Join(via, "endpoint-cache"), journal)
	if err != nil || aliased {
		t.Fatalf("a sibling reached through a link: aliased %v, err %v, want neither", aliased, err)
	}
	// The directory inside the journal is still refused, so the check is live.
	inside := filepath.Join(journal, "endpoint-cache")
	if aliased, err = AliasesJournal(inside, journal); err != nil || !aliased {
		t.Fatalf("a directory inside the journal: aliased %v, err %v, want aliased", aliased, err)
	}
}

func TestAliasesJournal(t *testing.T) {
	// The test directory is resolved first, so no symlink sits in any path
	// below unless a case puts it there. macOS keeps its temporary directory
	// behind a symlink and Linux does not, and the cases must mean the same on
	// both.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	journal := mk("journal")
	inside := mk("journal", "cache")
	sibling := mk("sibling")
	parent := mk("outer")
	nestedJournal := mk("outer", "journal")
	link := filepath.Join(root, "link")
	if err := os.Symlink(journal, link); err != nil {
		t.Fatal(err)
	}
	linkedAncestor := filepath.Join(root, "ancestor")
	if err := os.Symlink(root, linkedAncestor); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dirLikeFile := filepath.Join(file, "sub")
	linkedNested := filepath.Join(root, "nested-link")
	if err := os.Symlink(nestedJournal, linkedNested); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "parent-link")
	if err := os.Symlink(parent, linkedParent); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		cache   string
		journal string
		want    bool
		wantErr bool
	}{
		{"no journal", sibling, "", false, false},
		{"the same directory", journal, journal, true, false},
		{"inside the journal", inside, journal, true, false},
		{"a sibling", sibling, journal, false, false},
		{"a journal that does not exist yet", sibling, filepath.Join(root, "later"), false, false},
		{"a cache that contains the journal", parent, nestedJournal, true, false},
		{"a cache that contains a journal that does not exist yet", parent, filepath.Join(parent, "later"), true,
			false},
		{"a cache that contains a journal below a missing directory", parent,
			filepath.Join(parent, "missing", "deeper", "journal"), true, false},
		{"a cache beside a journal that does not exist yet", sibling, filepath.Join(parent, "later"), false, false},
		{"a cache that contains the journal through a symlink", parent, linkedNested, true, false},
		{"a cache reached through a symlink that contains the journal", linkedParent, nestedJournal, true, false},
		{"a symlinked ancestor above a containing cache", filepath.Join(linkedAncestor, "outer"),
			filepath.Join(linkedAncestor, "outer", "journal"), true, false},
		{"a symlinked ancestor on the journal of a containing cache", parent,
			filepath.Join(linkedAncestor, "outer", "journal"), true, false},
		{"a journal that does not exist yet below a symlink to the cache", parent, filepath.Join(linkedParent, "later"),
			true, false},
		{"a journal below missing directories under a symlink to the cache", parent,
			filepath.Join(linkedParent, "missing", "deeper", "journal"), true, false},
		{"a journal that does not exist yet below a symlink to another directory", sibling,
			filepath.Join(linkedParent, "later"), false, false},
		{"a journal that does not exist yet below a symlinked ancestor of the cache", journal,
			filepath.Join(linkedAncestor, "journal", "later"), true, false},
		{"a journal that does not exist yet beside a cache under a symlinked ancestor", inside,
			filepath.Join(linkedAncestor, "journal", "later"), false, false},
		{"a journal that does not exist yet below a symlinked ancestor of a sibling", sibling,
			filepath.Join(linkedAncestor, "outer", "later"), false, false},
		{"a cache that does not exist yet beside the journal", filepath.Join(root, "new-cache"), journal, false, false},
		{"a cache that does not exist yet inside the journal", filepath.Join(journal, "new-cache"), journal, true,
			false},
		{"a cache below missing directories inside the journal", filepath.Join(journal, "a", "b", "new-cache"), journal,
			true, false},
		{"a cache that does not exist yet inside the journal through a symlink", filepath.Join(link, "new-cache"),
			journal, true, false},
		{"a cache that does not exist yet inside a symlinked ancestor of the journal",
			filepath.Join(linkedAncestor, "journal", "new-cache"), journal, true, false},
		{"a cache that does not exist yet beside a journal that does not exist yet", filepath.Join(root, "m1"),
			filepath.Join(root, "m2"), false, false},
		{"a cache that does not exist yet and a journal below it", filepath.Join(root, "m1"),
			filepath.Join(root, "m1", "journal"), true, false},
		{"a cache that does not exist yet below a journal that does not exist yet", filepath.Join(root, "m1", "cache"),
			filepath.Join(root, "m1"), true, false},
		{"a cache that does not exist yet reached through a symlink to a journal that does not exist yet",
			filepath.Join(linkedParent, "m1", "cache"), filepath.Join(parent, "m1"), true, false},
		{"a cache that cannot be examined", dirLikeFile, journal, false, true},
		{"the filesystem root as the cache", string(filepath.Separator), journal, true, false},
		{"the journal reached through a symlink", journal, link, true, false},
		{"the cache reached through a symlink", link, journal, true, false},
		{"a cache inside a symlinked journal", inside, link, true, false},
		{"a symlinked ancestor of the cache", filepath.Join(linkedAncestor, "journal", "cache"), journal, true, false},
		{"a symlinked ancestor of the journal", inside, filepath.Join(linkedAncestor, "journal"), true, false},
		{"a journal with trailing space", inside, journal + " ", true, false},
		{"a journal that cannot be examined", sibling, dirLikeFile, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AliasesJournal(tc.cache, tc.journal)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("AliasesJournal(%q, %q) = %v, %v; want %v, error %v", tc.cache, tc.journal, got, err,
					tc.want, tc.wantErr)
			}
		})
	}
}

// A path spelled in another letter case reaches the same directory on a case
// folding file system, and only the identity check sees it.
func TestAliasesJournalSeesADirectoryUnderAnotherSpelling(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "Journal")
	cache := filepath.Join(journal, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	respelled := filepath.Join(root, "jOURNAL")
	if _, err := os.Stat(respelled); err != nil {
		t.Skip("this file system distinguishes letter case")
	}
	got, err := AliasesJournal(cache, respelled)
	if err != nil || !got {
		t.Errorf("AliasesJournal under another spelling = %v, %v, want true", got, err)
	}
}

// resolveLayout builds a resolved test root with a private journal directory
// and a private directory inside it, and an alias link to the root.
func resolveLayout(t *testing.T) (root, journal, inner, alias string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	journal = filepath.Join(root, "state", "journal")
	inner = filepath.Join(journal, "inner")
	for _, dir := range []string{filepath.Join(root, "state"), journal, inner} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias = filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "state"), alias); err != nil {
		t.Fatal(err)
	}
	return root, journal, inner, alias
}

// Resolve refuses a directory that overlaps the journal for every caller, after
// the layer opened it, and leaves no open handle on the journal's tree.
func TestResolveRefusesAnOpenedDirectoryThatOverlapsTheJournal(t *testing.T) {
	_, journal, inner, alias := resolveLayout(t)
	cases := []struct {
		name     string
		override string
		journal  string
	}{
		{"the same directory", journal, journal},
		{"the same directory through a symlinked ancestor", filepath.Join(alias, "journal"), journal},
		{"inside the journal", inner, journal},
		{"inside the journal through a symlinked ancestor", filepath.Join(alias, "journal", "inner"), journal},
		{"containing the journal", filepath.Dir(journal), journal},
		{"containing a journal named through a symlinked ancestor", filepath.Dir(journal),
			filepath.Join(alias, "journal")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := filepath.Clean(tc.override)
			choice := Resolve(Options{Override: override, JournalDir: tc.journal})
			if choice.Usable() || choice.Root != nil {
				_ = choice.Close()
				t.Fatalf("the choice for %s is usable, want it refused", override)
			}
			if choice.Layer != LayerOverride || choice.Path != override {
				t.Errorf("layer %q path %q, want the override layer and its path", choice.Layer, choice.Path)
			}
			if choice.Err == nil || !errors.Is(choice.Err, ErrJournalOverlap) {
				t.Errorf("err %v, want it to wrap ErrJournalOverlap", choice.Err)
			}
		})
	}
}

// The same options with no journal directory open the directory, so the refusal
// above comes from the journal check and nothing else.
func TestResolveUsesAnOverlappingDirectoryWhenNoJournalIsConfigured(t *testing.T) {
	_, journal, _, _ := resolveLayout(t)
	choice := Resolve(Options{Override: journal})
	defer func() { _ = choice.Close() }()
	if !choice.Usable() {
		t.Fatalf("the directory was refused with no journal configured: %v", choice.Err)
	}
}

// A path that cannot be compared with the journal is refused, because the check
// fails closed.
func TestResolveRefusesADirectoryItCannotCompareWithTheJournal(t *testing.T) {
	root, _, inner, _ := resolveLayout(t)
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	choice := Resolve(Options{Override: inner, JournalDir: loop})
	if choice.Usable() {
		_ = choice.Close()
		t.Fatal("the directory is usable although the journal could not be examined")
	}
	if choice.Err == nil || errors.Is(choice.Err, ErrJournalOverlap) ||
		!strings.Contains(choice.Err.Error(), "could not be compared with the allocation journal directory") {
		t.Errorf("err %v, want the comparison failure", choice.Err)
	}
}

// The temporary layer asks before it creates its directory, so a temporary
// directory inside the journal leaves nothing behind, and the caller's own
// guard is still asked about every path the journal check lets through.
func TestResolveChecksTheJournalBeforeTheTemporaryLayerCreatesItsDirectory(t *testing.T) {
	root, journal, _, alias := resolveLayout(t)
	tmp := filepath.Join(journal, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(alias, "journal", "tmp"))
	guard := &recordingGuard{}
	choice := Resolve(Options{JournalDir: journal, Guard: guard})
	if choice.Usable() {
		_ = choice.Close()
		t.Fatal("the temporary directory inside the journal is usable")
	}
	if choice.Layer != LayerTemporary || !errors.Is(choice.Err, ErrJournalOverlap) {
		t.Errorf("layer %q err %v, want a temporary layer refusal that wraps ErrJournalOverlap", choice.Layer,
			choice.Err)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("the journal's tmp holds %v (err %v), want nothing created", entries, err)
	}
	if len(guard.paths) != 0 {
		t.Errorf("the caller's guard was asked about %v after the journal check refused", guard.paths)
	}

	other := filepath.Join(root, "other-tmp")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", other)
	choice = Resolve(Options{JournalDir: journal, Guard: guard})
	defer func() { _ = choice.Close() }()
	if !choice.Usable() || len(guard.paths) != 1 {
		t.Errorf("usable %v, guard paths %v, want a usable directory the caller's guard was asked about once",
			choice.Usable(), guard.paths)
	}
}
