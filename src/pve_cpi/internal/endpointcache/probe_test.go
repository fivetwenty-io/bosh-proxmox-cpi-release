package endpointcache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeCreatesNothingAndReportsAnAbsentTemporaryDirectory(t *testing.T) {
	parent := temporaryParent(t)

	choice := Probe(Options{})
	defer func() { _ = choice.Close() }()

	if choice.Usable() || choice.Layer != LayerTemporary || !errors.Is(choice.Err, os.ErrNotExist) {
		t.Fatalf("probe = layer %q usable %v err %v, want the temporary layer absent", choice.Layer, choice.Usable(), choice.Err)
	}
	requireAbsent(t, temporaryPath(parent), "the probe must not create the temporary directory")
}

func TestProbeOpensAnExistingTemporaryDirectory(t *testing.T) {
	parent := temporaryParent(t)
	if err := os.Mkdir(temporaryPath(parent), 0o700); err != nil {
		t.Fatal(err)
	}

	choice := Probe(Options{})
	defer func() { _ = choice.Close() }()

	if !choice.Usable() || choice.Layer != LayerTemporary {
		t.Fatalf("probe = layer %q err %v, want the usable temporary layer", choice.Layer, choice.Err)
	}
}

func TestProbeRefusesTheJournalSiblingAsResolveDoes(t *testing.T) {
	temporaryParent(t)
	base := t.TempDir()
	sibling := filepath.Join(base, JournalSiblingName)
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sibling, 0o755); err != nil { //nolint:gosec // the test makes the mode unsafe on purpose
		t.Fatal(err)
	}
	opts := Options{JournalDir: filepath.Join(base, "journal")}

	probe := Probe(opts)
	defer func() { _ = probe.Close() }()
	resolved := resolve(t, opts)

	if probe.Usable() || probe.Layer != LayerJournal || probe.Path != sibling {
		t.Fatalf("probe = layer %q path %q usable %v, want the refused sibling", probe.Layer, probe.Path, probe.Usable())
	}
	if resolved.Layer != probe.Layer || resolved.Path != probe.Path || resolved.Usable() {
		t.Fatalf("probe and resolve disagree: %q %q vs %q %q", probe.Layer, probe.Path, resolved.Layer, resolved.Path)
	}
}
