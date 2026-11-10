package endpointcache

import (
	"path/filepath"
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
