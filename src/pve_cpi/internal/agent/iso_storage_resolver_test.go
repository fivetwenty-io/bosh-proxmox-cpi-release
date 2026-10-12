package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func sharedISOStorageSvc() *fakeClusterStorageSvc {
	return &fakeClusterStorageSvc{entries: []fakeClusterStorageEntry{
		{storage: "vm-storage", stype: "nfs", shared: true, content: "images,iso"},
	}}
}

// Building the resolver and the agent that holds it must not touch PVE. The
// lookup runs once, on first use, and the answer is remembered.
func TestISOStorageResolver_LookupIsLazyAndMemoized(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	storageSvc := sharedISOStorageSvc()
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	resolver := NewISOStorageResolver(cfg, pveClient, log.NewNopLogger())
	if _, err := NewAgentWithISOResolver(cfg, pveClient, nil, resolver, log.NewNopLogger()); err != nil {
		t.Fatalf("NewAgentWithISOResolver: %v", err)
	}
	if storageSvc.listCalls != 0 {
		t.Fatalf("construction issued %d ListStorage calls, want 0", storageSvc.listCalls)
	}

	for range 2 {
		if got := resolver.Resolve(context.Background()); got != "vm-storage" {
			t.Fatalf("Resolve() = %q, want vm-storage", got)
		}
	}
	if storageSvc.listCalls != 1 {
		t.Errorf("ListStorage calls = %d, want 1 (memoized)", storageSvc.listCalls)
	}
}

// The resolver works on a snapshot, so resolving never rewrites the caller's
// operator selection.
func TestISOStorageResolver_DoesNotMutateConfig(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	cfg.CaptureISOStoragePolicy()
	resolver := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: sharedISOStorageSvc()}, log.NewNopLogger())

	if got := resolver.Resolve(context.Background()); got != "vm-storage" {
		t.Fatalf("Resolve() = %q, want vm-storage", got)
	}
	if cfg.ISOStorage != "local" || cfg.OriginalISOStorage() != "local" {
		t.Errorf("config mutated: ISOStorage=%q original=%q, want local/local", cfg.ISOStorage, cfg.OriginalISOStorage())
	}
}

// Operator pins and opt-outs resolve with zero PVE calls, as before.
func TestISOStorageResolver_PinAndOptOutIssueNoCalls(t *testing.T) {
	t.Parallel()
	pinned := resolveISOTestCfg("vm-storage", true)
	pinned.ISOStorage = "fixed-iso"
	optOut := resolveISOTestCfg("vm-storage", false)

	for name, tc := range map[string]struct{ cfg, want string }{"pinned": {"pinned", "fixed-iso"}, "opt out": {"optOut", "local"}} {
		cfg := pinned
		if tc.cfg == "optOut" {
			cfg = optOut
		}
		storageSvc := sharedISOStorageSvc()
		r := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: storageSvc}, log.NewNopLogger())
		if got := r.Resolve(context.Background()); got != tc.want {
			t.Errorf("%s: Resolve() = %q, want %q", name, got, tc.want)
		}
		if storageSvc.listCalls != 0 {
			t.Errorf("%s: ListStorage calls = %d, want 0", name, storageSvc.listCalls)
		}
	}
}

func TestISOStorageResolver_NilReceiverReturnsEmpty(t *testing.T) {
	t.Parallel()
	var r *ISOStorageResolver
	if got := r.Resolve(context.Background()); got != "" {
		t.Errorf("nil resolver: Resolve() = %q, want empty", got)
	}
}

// A ConfigDrive built with a resolver targets the resolved pool, and keeps its
// configured fallback when no resolver is attached.
func TestConfigDrive_StorageNameFollowsResolver(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	storageSvc := sharedISOStorageSvc()
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	drive := newConfigDriveForTest(pveClient, "local", log.NewNopLogger())
	if got := drive.storageName(context.Background()); got != "local" {
		t.Fatalf("no resolver: storageName() = %q, want local", got)
	}
	drive.isoResolver = NewISOStorageResolver(cfg, pveClient, log.NewNopLogger())
	if storageSvc.listCalls != 0 {
		t.Fatalf("attaching the resolver issued %d calls, want 0", storageSvc.listCalls)
	}
	if got := drive.storageName(context.Background()); got != "vm-storage" {
		t.Errorf("with resolver: storageName() = %q, want vm-storage", got)
	}
}

// A lookup that failed is not remembered: the failing call returns the
// configured fallback and the next call follows vm_storage.
func TestISOStorageResolver_FailedLookupIsRetried(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	storageSvc := sharedISOStorageSvc()
	storageSvc.failFirst = 1
	resolver := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: storageSvc}, log.NewNopLogger())

	if got := resolver.Resolve(context.Background()); got != "local" {
		t.Fatalf("failed lookup: Resolve() = %q, want the fallback local", got)
	}
	if got := resolver.Resolve(context.Background()); got != "vm-storage" {
		t.Fatalf("retry: Resolve() = %q, want vm-storage", got)
	}
	if got := resolver.Resolve(context.Background()); got != "vm-storage" {
		t.Fatalf("memoized: Resolve() = %q, want vm-storage", got)
	}
	if storageSvc.listCalls != 2 {
		t.Errorf("ListStorage calls = %d, want 2 (one failure, one success, then memoized)", storageSvc.listCalls)
	}
}

// A call whose context has already ended answers with the fallback and leaves
// the question open for the next caller.
func TestISOStorageResolver_EndedContextIsNotRemembered(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	resolver := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: sharedISOStorageSvc()}, log.NewNopLogger())

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if got := resolver.Resolve(ended); got != "local" {
		t.Fatalf("ended ctx: Resolve() = %q, want the fallback local", got)
	}
	if got := resolver.Resolve(context.Background()); got != "vm-storage" {
		t.Errorf("later call: Resolve() = %q, want vm-storage", got)
	}
}

// A vm_storage that is absent from the index is a conclusive answer and is
// remembered, so a missing pool does not cost a lookup per call.
func TestISOStorageResolver_NotFoundIsRemembered(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("other-storage", true)
	storageSvc := sharedISOStorageSvc()
	resolver := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: storageSvc}, log.NewNopLogger())

	for range 3 {
		if got := resolver.Resolve(context.Background()); got != "local" {
			t.Fatalf("Resolve() = %q, want local", got)
		}
	}
	if storageSvc.listCalls != 1 {
		t.Errorf("ListStorage calls = %d, want 1", storageSvc.listCalls)
	}
}

// Many goroutines asking at once share one lookup. Run under -race.
func TestISOStorageResolver_ConcurrentResolveSingleLookup(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	storageSvc := sharedISOStorageSvc()
	resolver := NewISOStorageResolver(cfg, &fakePVEClient{clusterStorageSvc: storageSvc}, log.NewNopLogger())

	var wg sync.WaitGroup
	results := make([]string, 16)
	for i := range results {
		wg.Go(func() { results[i] = resolver.Resolve(context.Background()) })
	}
	wg.Wait()
	for i, got := range results {
		if got != "vm-storage" {
			t.Errorf("goroutine %d: Resolve() = %q, want vm-storage", i, got)
		}
	}
	if storageSvc.listCalls != 1 {
		t.Errorf("ListStorage calls = %d, want 1", storageSvc.listCalls)
	}
}
