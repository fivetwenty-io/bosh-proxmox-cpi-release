package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestStorageJournalFollowsTheConfiguredRetryCurves runs the CLI with a
// config whose pushback curve is longer than the shipped one. A CPI with that
// config holds a parker's lock for longer, so the CLI has to wait that much
// longer for it and give cleanup the matching budget. The client factory runs
// after the config is loaded and before any lock work, so it sees the values
// cleanup would use. The test changes process-wide curves, so it does not run
// in parallel, and it puts the shipped curves back when it ends.
func TestStorageJournalFollowsTheConfiguredRetryCurves(t *testing.T) {
	t.Cleanup(pve.SetPushbackBackoffForTest(5000, 60000))
	t.Cleanup(pve.SetStorageLockBackoffForTest(2000, 30000, 30))
	shippedTTL := pve.ParkerProtectionLockTTLNow()
	shippedBudget := storageJournalBudget(storageJournalActionCleanup)

	cfg := minimalCfg()
	base := provisionTemp(t)
	dir := filepath.Join(base, "allocations")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.StorageAllocationJournalDir = dir
	cfg.StoragePlacementNamespace = "director"
	cfg.Retry = &config.RetryConfig{Pushback: &config.RetryPolicy{BaseMs: 20000, CapMs: 120000}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "cpi.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var ttl, budget time.Duration
	factory := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		ttl = pve.ParkerProtectionLockTTLNow()
		budget = storageJournalBudget(storageJournalActionCleanup)
		return nil, errors.New("this test has no PVE")
	}
	var stdout, stderr bytes.Buffer
	if code := runStorageJournal([]string{"audit", "--config", path}, &stdout, &stderr,
		runOptions{ClientFactory: factory}); code != 1 {
		t.Fatalf("exit code = %d, want 1 from the refused client; output %s%s", code, stdout.String(), stderr.String())
	}
	if ttl == 0 {
		t.Fatalf("the CLI never built its client; output %s", stderr.String())
	}
	if ttl <= shippedTTL {
		t.Fatalf("the CLI's parker lock TTL and wait stayed %s under a pushback curve that lengthens the CPI's", ttl)
	}
	if grew, want := budget-shippedBudget, 2*(ttl-shippedTTL); grew < want {
		t.Fatalf("the cleanup budget grew by %s, want at least %s for its wait and its own window", grew, want)
	}
}
