package endpointcache

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// temporaryParent points os.TempDir at a fresh directory and returns it.
func temporaryParent(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	return parent
}

func temporaryPath(parent string) string {
	return filepath.Join(parent, "pve_cpi-endpoint-cache."+strconv.Itoa(os.Geteuid()))
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func resolve(t *testing.T, opts Options) Choice {
	t.Helper()
	choice := Resolve(opts)
	t.Cleanup(func() { _ = choice.Close() })
	return choice
}

func requireAbsent(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: %v", what, err)
	}
}

func requireUnsafe(t *testing.T, choice Choice, cause error) {
	t.Helper()
	if choice.Usable() {
		t.Fatalf("choice is usable, want a refusal: %+v", choice)
	}
	if choice.Err == nil {
		t.Fatal("refused choice has no reason")
	}
	var unsafe *privatefile.UnsafePathError
	if !errors.As(choice.Err, &unsafe) {
		t.Fatalf("reason %v is not an UnsafePathError", choice.Err)
	}
	if !errors.Is(choice.Err, cause) {
		t.Fatalf("reason %v does not match %v", choice.Err, cause)
	}
}

func TestOverrideIsUsedWhenSet(t *testing.T) {
	parent := temporaryParent(t)
	dir := privateDirectory(t)
	choice := resolve(t, Options{Override: dir})
	if !choice.Usable() || choice.Err != nil {
		t.Fatalf("override refused: %v", choice.Err)
	}
	if choice.Layer != LayerOverride || choice.Path != dir {
		t.Fatalf("choice = %s %s, want %s %s", choice.Layer, choice.Path, LayerOverride, dir)
	}
	if choice.Root.Name() != dir {
		t.Fatalf("root = %s, want %s", choice.Root.Name(), dir)
	}
	requireAbsent(t, temporaryPath(parent), "the temporary directory was created beside an override")
}

func TestTemporaryLayerIsUsedWithoutOverride(t *testing.T) {
	parent := temporaryParent(t)
	choice := resolve(t, Options{})
	if !choice.Usable() || choice.Err != nil {
		t.Fatalf("temporary layer refused: %v", choice.Err)
	}
	want := temporaryPath(parent)
	if choice.Layer != LayerTemporary || choice.Path != want {
		t.Fatalf("choice = %s %s, want %s %s", choice.Layer, choice.Path, LayerTemporary, want)
	}
	info, err := os.Lstat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created %v, want a directory at mode 0700", info.Mode())
	}
}

func TestTemporaryLayerReusesItsDirectory(t *testing.T) {
	parent := temporaryParent(t)
	first := resolve(t, Options{})
	if err := os.WriteFile(filepath.Join(temporaryPath(parent), "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := resolve(t, Options{})
	if !first.Usable() || !second.Usable() {
		t.Fatalf("refused: %v, %v", first.Err, second.Err)
	}
	if _, err := second.Root.Stat("marker"); err != nil {
		t.Fatalf("second resolve did not reuse the directory: %v", err)
	}
}

func TestTemporaryLayerCreatesOnlyTheLastComponent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "deeper")
	t.Setenv("TMPDIR", missing)
	choice := resolve(t, Options{})
	if choice.Usable() {
		t.Fatal("temporary layer was usable under a missing parent")
	}
	if choice.Layer != LayerTemporary || !errors.Is(choice.Err, os.ErrNotExist) {
		t.Fatalf("layer = %s, reason = %v, want a missing-parent refusal", choice.Layer, choice.Err)
	}
	requireAbsent(t, filepath.Dir(missing), "an intermediate directory was created")
}

func TestTemporaryLayerRefusesAnUncreatableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	t.Setenv("TMPDIR", parent)
	choice := resolve(t, Options{})
	if choice.Usable() || !errors.Is(choice.Err, os.ErrPermission) {
		t.Fatalf("usable = %v, reason = %v, want a permission refusal", choice.Usable(), choice.Err)
	}
}

func TestTemporaryLayerRefusesWideMode(t *testing.T) {
	dir := temporaryPath(temporaryParent(t))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{})
	requireUnsafe(t, choice, privatefile.ErrUnsafeMode)
	if choice.Layer != LayerTemporary || choice.Path != dir {
		t.Fatalf("choice = %s %s", choice.Layer, choice.Path)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %v, the layer must not repair a directory it refuses", info.Mode().Perm())
	}
}

func TestTemporaryLayerRefusesSymlink(t *testing.T) {
	parent := temporaryParent(t)
	if err := os.Symlink(privateDirectory(t), temporaryPath(parent)); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{})
	if choice.Usable() || choice.Err == nil {
		t.Fatal("symlinked temporary directory was accepted")
	}
	if choice.Layer != LayerTemporary {
		t.Fatalf("layer = %s, want %s", choice.Layer, LayerTemporary)
	}
}

func TestTemporaryLayerRefusesRegularFile(t *testing.T) {
	parent := temporaryParent(t)
	if err := os.WriteFile(temporaryPath(parent), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	requireUnsafe(t, resolve(t, Options{}), privatefile.ErrUnsafeFileType)
}

func TestOverrideRefusesWrongOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the directory the test uses as foreign")
	}
	// The filesystem root belongs to root, so it stands for a directory that
	// another user created first.
	choice := resolve(t, Options{Override: string(filepath.Separator)})
	requireUnsafe(t, choice, privatefile.ErrUnsafeOwnership)
	if choice.Layer != LayerOverride {
		t.Fatalf("layer = %s, want %s", choice.Layer, LayerOverride)
	}
}

func TestOverrideRefusesWrongMode(t *testing.T) {
	dir := privateDirectory(t)
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{Override: dir})
	requireUnsafe(t, choice, privatefile.ErrUnsafeMode)
	if choice.Layer != LayerOverride || choice.Path != dir {
		t.Fatalf("choice = %s %s", choice.Layer, choice.Path)
	}
}

func TestOverrideRefusesSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(privateDirectory(t), link); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{Override: link})
	if choice.Usable() || choice.Err == nil {
		t.Fatal("symlinked override was accepted")
	}
	if choice.Layer != LayerOverride {
		t.Fatalf("layer = %s, want %s", choice.Layer, LayerOverride)
	}
}

func TestOverrideRefusesAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	requireUnsafe(t, resolve(t, Options{Override: file}), privatefile.ErrUnsafeFileType)
}

func TestOverrideIsNeverCreated(t *testing.T) {
	parent := temporaryParent(t)
	missing := filepath.Join(t.TempDir(), "absent")
	choice := resolve(t, Options{Override: missing})
	if choice.Usable() || !errors.Is(choice.Err, os.ErrNotExist) {
		t.Fatalf("usable = %v, reason = %v, want a not-exist refusal", choice.Usable(), choice.Err)
	}
	requireAbsent(t, missing, "the override was created")
	requireAbsent(t, temporaryPath(parent), "a missing override fell through to the temporary layer")
}

func TestRefusedOverrideDoesNotFallThrough(t *testing.T) {
	parent := temporaryParent(t)
	dir := privateDirectory(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	choice := resolve(t, Options{Override: dir})
	if choice.Layer != LayerOverride || choice.Usable() {
		t.Fatalf("layer = %s, usable = %v", choice.Layer, choice.Usable())
	}
	requireAbsent(t, temporaryPath(parent), "a refused override fell through to the temporary layer")
}

func TestOverrideRefusesRelativeAndUncleanPaths(t *testing.T) {
	dir := privateDirectory(t)
	cases := map[string]string{
		"relative":       "relative/cache",
		"trailing slash": dir + "/",
		"dot dot":        filepath.Join(dir, "sub") + "/../",
		"double slash":   filepath.Dir(dir) + "//cache",
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			choice := resolve(t, Options{Override: override})
			if choice.Usable() || choice.Err == nil || choice.Layer != LayerOverride {
				t.Fatalf("override %q: usable %v, layer %s, reason %v", override, choice.Usable(), choice.Layer, choice.Err)
			}
		})
	}
}

func TestRefusalTextNamesLayerAndPath(t *testing.T) {
	dir := privateDirectory(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	text := resolve(t, Options{Override: dir}).Err.Error()
	for _, want := range []string{dir, "override layer", "0777"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reason %q lacks %q", text, want)
		}
	}
}

func TestCloseIsSafeOnEveryChoice(t *testing.T) {
	if err := (Choice{}).Close(); err != nil {
		t.Fatal(err)
	}
	choice := Resolve(Options{Override: privateDirectory(t)})
	if err := choice.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRefusesWhenNoLayerApplies(t *testing.T) {
	saved := layers
	layers = nil
	t.Cleanup(func() { layers = saved })
	choice := Resolve(Options{})
	if choice.Usable() || choice.Err == nil {
		t.Fatalf("empty layer list produced %+v", choice)
	}
}
