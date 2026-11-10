package handlers_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestDeadNodeDelete_AMissingCacheDirectoryIsWarnedAboutOncePerCall runs the
// whole delete of a VM on a dead node while pve.endpoint_cache_dir does not
// exist. The HA gate observes the recovery episode, and the dead-node
// completion that follows it observes the same episode again, so the call reads
// the record twice. The operator reads one line about the directory.
func TestDeadNodeDelete_AMissingCacheDirectoryIsWarnedAboutOncePerCall(t *testing.T) {
	f := newDeadNodeDelete(t)
	tmp, err := os.MkdirTemp("", "dead-node-episode-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	missing := filepath.Join(t.TempDir(), "absent")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)

	// An earlier retry, an hour ago, started the episode's window in the
	// temporary layer, which is where a missing override sends the record.
	earlier := &ha.EpisodeStore{
		Dir: endpointcache.DirOptions{Override: missing},
		Now: func() time.Time { return time.Now().Add(-time.Hour) },
	}
	if _, err := earlier.Observe("vm:101", "uid-101"); err != nil {
		t.Fatal(err)
	}
	f.episodes.Dir = endpointcache.DirOptions{Override: missing}
	f.episodes.Log = logger

	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete once the window has expired: %v", err)
	}
	f.requireCompletedOn("pve-node1")

	warned := 0
	for _, e := range logs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "pve.endpoint_cache_dir does not exist") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("the call logged %d missing-directory warnings, want one", warned)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the override directory must never be created: %v", err)
	}
}
