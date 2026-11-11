package handlers

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// temporaryEpisodeDir points the temporary cache layer at a fresh directory and
// returns the directory the layer will make inside it.
func temporaryEpisodeDir(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "ha-episodes-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	return filepath.Join(tmp, "pve_cpi-endpoint-cache."+strconv.Itoa(os.Geteuid()))
}

func TestHAEpisodesStoreFollowsTheConfiguredCacheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Config: &config.CPIConfig{EndpointCacheDir: dir}}

	if _, err := haEpisodes(deps, nil).Observe("vm:101", "uid-a"); err != nil {
		t.Fatalf("observe through the default store: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ha.EpisodeFileName)); err != nil {
		t.Errorf("the record must sit in pve.endpoint_cache_dir: %v", err)
	}

	explicit := &ha.EpisodeStore{}
	if got := haEpisodes(Deps{HAEpisodes: explicit}, nil); got != explicit {
		t.Error("a store set on the deps must be used as is")
	}
}

func TestHAEpisodesStoreCarriesTheLoggerAndTheJournalDirectory(t *testing.T) {
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	missing := filepath.Join(t.TempDir(), "absent")
	temporaryEpisodeDir(t)
	deps := Deps{Config: &config.CPIConfig{EndpointCacheDir: missing, StorageAllocationJournalDir: "/journal"}}

	store := haEpisodes(deps, logger)
	if store.Dir != endpointcache.OptionsFor(deps.Config) {
		t.Errorf("Dir = %+v, want what OptionsFor builds", store.Dir)
	}
	if _, err := store.Observe("vm:101", "uid-a"); err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, e := range logs.All() {
		warned = warned || (e.Level == slog.LevelWarn && e.Attrs["path"] == missing)
	}
	if !warned {
		t.Error("the store must log through the logger the handler passed")
	}
}

// TestRecoveryGraceWindowRunsOutAcrossCallsWithAMissingOverride shows the
// reason for the fall-back. Each delete retry builds a new store, and the
// window only expires if the record outlives the call.
func TestRecoveryGraceWindowRunsOutAcrossCallsWithAMissingOverride(t *testing.T) {
	temporary := temporaryEpisodeDir(t)
	missing := filepath.Join(t.TempDir(), "absent")
	motionSec := 600
	deps := Deps{Config: &config.CPIConfig{
		EndpointCacheDir: missing,
		HA:               &config.HAConfig{MotionWaitSec: &motionSec},
	}}
	st := &pve.HAServiceState{SID: "vm:101", VMID: 101, UID: "uid-a", Node: "pve2"}
	logger, logs := log.NewObservedLogger(slog.LevelDebug)

	// An earlier retry, twenty minutes ago, started the window.
	earlier := &ha.EpisodeStore{
		Dir: endpointcache.DirOptions{Override: missing},
		Now: func() time.Time { return time.Now().Add(-20 * time.Minute) },
	}
	if _, err := earlier.Observe(st.SID, st.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(temporary, ha.EpisodeFileName)); err != nil {
		t.Fatalf("the earlier retry's record must be in the temporary layer: %v", err)
	}

	err := haRecoveryGraceError(deps, "delete_vm", st, "the node is fenced", logger)
	if err == nil || !strings.Contains(err.Error(), "grace window expired") {
		t.Fatalf("a window started twenty minutes ago must be expired, got %v", err)
	}
	if !okToRetryCPIError(err) {
		t.Errorf("the deferral must stay retriable: %v", err)
	}
	var missingWarn, expiredWarn bool
	for _, e := range logs.All() {
		if e.Level != slog.LevelWarn {
			continue
		}
		missingWarn = missingWarn || (strings.Contains(e.Message, "does not exist") && e.Attrs["path"] == missing)
		expiredWarn = expiredWarn || strings.Contains(e.Message, "grace window expired")
	}
	if !missingWarn || !expiredWarn {
		t.Errorf(
			"want the missing-directory warning and the expiry warning, got missing=%v expired=%v",
			missingWarn,
			expiredWarn,
		)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the override directory must never be created: %v", statErr)
	}
}
