package endpointcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestOptionsForCarriesTheOverrideAndTheJournalDirectory(t *testing.T) {
	cfg := &config.CPIConfig{
		EndpointCacheDir:            "/var/vcap/store/director/pve_cpi-endpoint-cache",
		StorageAllocationJournalDir: "/var/vcap/store/director/pve_cpi-journal",
	}
	got := OptionsFor(cfg)
	if got.Override != cfg.EndpointCacheDir || got.JournalDir != cfg.StorageAllocationJournalDir {
		t.Errorf("OptionsFor = %+v, want the override and the journal directory", got)
	}
	if opts := got.Options(); opts != (Options{Override: cfg.EndpointCacheDir}) {
		t.Errorf("Options = %+v, want the override", opts)
	}
	if empty := OptionsFor(nil); empty != (DirOptions{}) {
		t.Errorf("OptionsFor(nil) = %+v, want the zero options", empty)
	}
	if empty := OptionsFor(&config.CPIConfig{}); empty != (DirOptions{}) {
		t.Errorf("OptionsFor(empty config) = %+v, want the zero options", empty)
	}
}

func TestDirOptionsResolveChoosesLikeResolve(t *testing.T) {
	dir := newStoreDir(t)
	choice := OptionsFor(&config.CPIConfig{EndpointCacheDir: dir}).Resolve()
	if !choice.Usable() || choice.Layer != LayerOverride || choice.Path != dir {
		t.Fatalf("Resolve = %+v, want the override directory", choice)
	}
	if err := choice.Close(); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "absent")
	refused := OptionsFor(&config.CPIConfig{EndpointCacheDir: missing}).Resolve()
	if refused.Usable() || refused.Layer != LayerOverride {
		t.Fatalf("a missing override resolved to %+v, want a refusal by the override layer", refused)
	}
}

func TestDirOptionsWithoutOverrideReachesTheLayersAfterIt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	opts := OptionsFor(&config.CPIConfig{
		EndpointCacheDir:            filepath.Join(t.TempDir(), "absent"),
		StorageAllocationJournalDir: "/journal",
	})
	without := opts.WithoutOverride()
	if without.Override != "" || without.JournalDir != "/journal" {
		t.Errorf("WithoutOverride = %+v, want only the override cleared", without)
	}
	if opts.Override == "" {
		t.Error("WithoutOverride changed the receiver")
	}
	choice := without.Resolve()
	if !choice.Usable() || choice.Layer != LayerTemporary {
		t.Fatalf("Resolve without the override = %+v, want the temporary layer", choice)
	}
	if err := choice.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDirOptionsNamedCarriesTheDisplayNameIntoTheRefusalText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- the test makes the directory unsafe on purpose
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- the umask may have narrowed it
		t.Fatal(err)
	}
	plain := DirOptions{Override: dir}
	named := plain.Named(HAProfileName)
	if plain.Options().DisplayName != "" || named.Options().DisplayName != HAProfileName {
		t.Fatalf("Options() = %+v and %+v, want the name carried only when set", plain.Options(), named.Options())
	}
	if text := plain.Resolve().Err.Error(); !strings.Contains(text, "endpoint cache: unsafe permissions") {
		t.Errorf("unnamed refusal %q, want the endpoint cache", text)
	}
	if text := named.Resolve().Err.Error(); !strings.Contains(text, "HA profile cache: unsafe permissions") ||
		strings.Contains(text, "endpoint cache") {
		t.Errorf("named refusal %q, want the HA profile cache", text)
	}
	if without := named.WithoutOverride(); without.DisplayName != HAProfileName {
		t.Errorf("WithoutOverride dropped the name: %+v", without)
	}
}

// recordingGuard is a PathGuard that records the paths it was asked about and
// answers with err.
type recordingGuard struct {
	err   error
	paths []string
}

func (g *recordingGuard) BeforeCreate(path string) error {
	g.paths = append(g.paths, path)
	return g.err
}

func TestDirOptionsWithGuardCarriesTheGuardIntoTheLayerOptions(t *testing.T) {
	guard := &recordingGuard{}
	plain := DirOptions{Override: "/cache"}
	guarded := plain.WithGuard(guard)
	if guarded.Options().Guard != PathGuard(guard) {
		t.Errorf("Options().Guard = %v, want the guard WithGuard set", guarded.Options().Guard)
	}
	if plain.Guard != nil || plain.Options().Guard != nil {
		t.Error("WithGuard changed the receiver")
	}
	if guarded.Override != "/cache" {
		t.Errorf("WithGuard = %+v, want the other options kept", guarded)
	}
}

// A guard that refuses the temporary layer's path stops the layer before it
// creates anything, and its error is the refusal's cause.
func TestTemporaryLayerGuardRefusesBeforeAnythingIsCreated(t *testing.T) {
	parent := t.TempDir()
	cause := errors.New("the guard says no")
	guard := &recordingGuard{err: cause}
	created := false
	choice, applies := endpointTexts.temporaryLayerGuarded(parent, func(string, os.FileMode) error {
		created = true
		return nil
	}, guard)
	if !applies || choice.Usable() || choice.Layer != LayerTemporary {
		t.Fatalf("applies = %v, usable = %v, layer = %s, want a refusal by the temporary layer", applies,
			choice.Usable(), choice.Layer)
	}
	if !errors.Is(choice.Err, cause) {
		t.Errorf("refusal = %v, want the guard's error as its cause", choice.Err)
	}
	if want := temporaryPath(parent); len(guard.paths) != 1 || guard.paths[0] != want || choice.Path != want {
		t.Errorf("guard saw %v and the choice names %q, want the one path %q", guard.paths, choice.Path, want)
	}
	if created {
		t.Error("the layer called mkdir after the guard refused")
	}
	requireAbsent(t, temporaryPath(parent), "the layer created its directory after the guard refused")
}

// A guard that accepts leaves the layer to create and open its directory, and
// it is asked again when the directory already exists.
func TestTemporaryLayerGuardAcceptsAndTheLayerCreatesTheDirectory(t *testing.T) {
	parent := t.TempDir()
	guard := &recordingGuard{}
	for range 2 {
		choice, _ := endpointTexts.temporaryLayerGuarded(parent, os.Mkdir, guard)
		requireWritable(t, choice)
		_ = choice.Close()
	}
	if want := temporaryPath(parent); len(guard.paths) != 2 || guard.paths[0] != want || guard.paths[1] != want {
		t.Errorf("guard saw %v, want %q twice, once for the new directory and once for the existing one", guard.paths,
			want)
	}
}

// A relative temporary directory is refused before the guard is asked, so the
// guard never sees a path that depends on the working directory.
func TestTemporaryLayerGuardIsNotAskedAboutARelativeTemporaryDirectory(t *testing.T) {
	guard := &recordingGuard{}
	choice, _ := endpointTexts.temporaryLayerGuarded("relative", os.Mkdir, guard)
	if choice.Usable() || choice.Err == nil {
		t.Fatalf("a relative temporary directory gave %+v, want a refusal", choice)
	}
	if len(guard.paths) != 0 {
		t.Errorf("guard saw %v, want it left out for a relative temporary directory", guard.paths)
	}
}

// Resolve passes the options' guard to the temporary layer, and the override
// layer, which creates nothing, never asks it.
func TestResolveAsksTheGuardOnlyForALayerThatCreatesItsDirectory(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cause := errors.New("the guard says no")
	guard := &recordingGuard{err: cause}
	choice := Resolve(Options{Guard: guard})
	if choice.Usable() || !errors.Is(choice.Err, cause) || choice.Layer != LayerTemporary {
		t.Fatalf("Resolve with a refusing guard = %+v, want the guard's refusal from the temporary layer", choice)
	}
	if len(guard.paths) != 1 {
		t.Errorf("guard saw %v, want the temporary layer's path once", guard.paths)
	}

	override := privateDirectory(t)
	guard.paths = nil
	choice = Resolve(Options{Override: override, Guard: guard})
	t.Cleanup(func() { _ = choice.Close() })
	if !choice.Usable() || choice.Layer != LayerOverride {
		t.Fatalf("Resolve with an override = %+v, want the override layer's directory", choice)
	}
	if len(guard.paths) != 0 {
		t.Errorf("guard saw %v, want the override layer to leave it alone", guard.paths)
	}
}
