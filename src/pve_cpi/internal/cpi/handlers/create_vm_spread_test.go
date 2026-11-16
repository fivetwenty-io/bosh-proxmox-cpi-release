package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
)

// spreadEnv is the env of a member of the web instance group.
func spreadEnv() map[string]any {
	return map[string]any{
		"bosh": map[string]any{
			"group":  "director-d-web",
			"groups": []any{"director", "d", "web", "director-d", "director-d-web"},
		},
	}
}

func spreadArgs() []json.RawMessage {
	return mkArgs("spread-agent", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{},
		spreadEnv(),
	)
}

// spreadRecord is a claim record in a new directory that only this user can
// open, with a probe that reads the web group's live claims.
type spreadRecord struct {
	store *spreadclaim.Store
	key   string
}

func newSpreadRecord(t *testing.T, deps *handlers.Deps) *spreadRecord {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &spreadclaim.Store{Dir: endpointcache.DirOptions{Override: dir}, Scope: "lab"}
	deps.SpreadClaims = func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return store }
	return &spreadRecord{store: store, key: handlers.SpreadClaimKey(*deps, spreadEnv())}
}

// counts returns the live claims per node. It records nothing itself.
func (r *spreadRecord) counts() (map[string]int, error) {
	return r.countsWithTagged(nil)
}

// countsWithTagged returns the live claims per node that a sibling counts
// when its facts already count the VMIDs that tagged reports from their
// member tag.
func (r *spreadRecord) countsWithTagged(tagged func(vmid int) bool) (map[string]int, error) {
	c, err := r.store.NewClaim(r.key, time.Now().Add(time.Hour))
	if err != nil {
		return nil, err
	}
	var seen map[string]int
	_, err = c.PickExcept(context.Background(), tagged, func(m map[string]int) string {
		seen = maps.Clone(m)
		return ""
	})
	return seen, err
}

// claimsSeen records the claims the record held each time PVE was asked to
// create the VM.
type claimsSeen struct {
	mu   sync.Mutex
	seen []map[string]int
	errs []error
}

func (c *claimsSeen) observe(r *spreadRecord) {
	c.observeTagged(r, nil)
}

// observeTagged is observe for a sibling whose facts count the VMIDs that
// tagged reports.
func (c *claimsSeen) observeTagged(r *spreadRecord, tagged func(vmid int) bool) {
	m, err := r.countsWithTagged(tagged)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, m)
	if err != nil {
		c.errs = append(c.errs, err)
	}
}

func spreadAntiAffinityOn(c *config.CPIConfig) {
	on := true
	if c.Placement == nil {
		c.Placement = &config.PlacementConfig{}
	}
	c.Placement.AntiAffinity = &config.AntiAffinityConfig{Enabled: &on}
}

func assertNoClaims(t *testing.T, r *spreadRecord) {
	t.Helper()
	got, err := r.counts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("claims after the create returned = %v; want none", got)
	}
}

// TestCreateVM_SpreadClaimHeldUntilTheCreateReturns checks that a create
// holds the claim on the node it picked until create_vm returns, so a sibling
// that read the member tags before the tag landed still counts it. Once the
// create has allocated its VM, the claim names the VMID, and a sibling whose
// facts already count that VM from its tag leaves the claim out.
func TestCreateVM_SpreadClaimHeldUntilTheCreateReturns(t *testing.T) {
	t.Parallel()
	var (
		rec    *spreadRecord
		seen   claimsSeen
		tagged claimsSeen
	)
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			seen.observe(rec)
			return "UPID:" + node + ":create:ok", nil
		},
		startFn: func(_ context.Context, node string, vmid int) (string, error) {
			seen.observe(rec)
			tagged.observeTagged(rec, func(v int) bool { return v == vmid })
			return "UPID:" + node + ":start:ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusTwoNodes(), emptyListResources, &vmMockAgent{},
		spreadAntiAffinityOn)
	rec = newSpreadRecord(t, &deps)

	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), spreadArgs(), mkCtx("spread-single")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := errors.Join(append(seen.errs, tagged.errs...)...); err != nil {
		t.Fatal(err)
	}
	// The build and the start both see the claim on pve1.
	want := map[string]int{"pve1": 1}
	if len(seen.seen) != 2 || !maps.Equal(seen.seen[0], want) || !maps.Equal(seen.seen[1], want) {
		t.Fatalf("claims at the build and the start = %v; want %v at both", seen.seen, want)
	}
	// A sibling that counts the new VM from its tag leaves the claim out.
	if len(tagged.seen) != 1 || len(tagged.seen[0]) != 0 {
		t.Fatalf("claims a sibling that counts the tagged VM sees = %v; want none", tagged.seen)
	}
	assertNoClaims(t, rec)
}

// TestCreateVM_SpreadClaimForAPinnedCreate checks that a create that
// target_node pins claims its node while PVE builds the VM, so the siblings
// that score meanwhile count it.
func TestCreateVM_SpreadClaimForAPinnedCreate(t *testing.T) {
	t.Parallel()
	var (
		rec  *spreadRecord
		seen claimsSeen
	)
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			seen.observe(rec)
			return "UPID:" + node + ":create:ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusTwoNodes(), emptyListResources, &vmMockAgent{},
		spreadAntiAffinityOn)
	rec = newSpreadRecord(t, &deps)
	args := mkArgs("spread-agent", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "target_node": "pve2"},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{},
		spreadEnv(),
	)

	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("spread-pinned")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := errors.Join(seen.errs...); err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"pve2": 1}; len(seen.seen) != 1 || !maps.Equal(seen.seen[0], want) {
		t.Fatalf("claims at the build = %v; want %v", seen.seen, want)
	}
	assertNoClaims(t, rec)
}

// TestCreateVM_SpreadClaimReleasedWhenTheCreateFails checks that a create
// whose build fails leaves no claim behind.
func TestCreateVM_SpreadClaimReleasedWhenTheCreateFails(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			return "", cloneSourceMissingErr()
		},
	}
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, listStatusTwoNodes(), emptyListResources, &vmMockAgent{},
		spreadAntiAffinityOn)
	rec := newSpreadRecord(t, &deps)

	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), spreadArgs(), mkCtx("spread-fail")); err == nil {
		t.Fatal("the create must fail")
	}
	assertNoClaims(t, rec)
}

// TestCreateVM_Fallback_SpreadClaimMovesToTheAlternate checks that a create
// that falls back to an alternate holds its claim there until create_vm
// returns, and holds nothing after.
func TestCreateVM_Fallback_SpreadClaimMovesToTheAlternate(t *testing.T) {
	t.Parallel()
	var (
		rec   *spreadRecord
		seen  claimsSeen
		calls int
	)
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			seen.observe(rec)
			calls++
			if calls == 1 {
				return "", transientConnErr("pvedaemon worker recycled")
			}
			return "UPID:" + node + ":create:ok", nil
		},
		startFn: func(_ context.Context, node string, _ int) (string, error) {
			seen.observe(rec)
			return "UPID:" + node + ":start:ok", nil
		},
	}
	deps := buildFallbackDeps(q, &vmMockNodes{}, &vmMockAgent{}, 2)
	spreadAntiAffinityOn(deps.Config)
	rec = newSpreadRecord(t, &deps)

	if _, err := handlers.HandleCreateVM(deps).Handle(context.Background(), spreadArgs(), mkCtx("spread-fallback")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := errors.Join(seen.errs...); err != nil {
		t.Fatal(err)
	}
	// The failed build on pve1 sees the claim there, and the alternate's
	// build and start see it on pve2.
	want := []map[string]int{{"pve1": 1}, {"pve2": 1}, {"pve2": 1}}
	if len(seen.seen) != len(want) {
		t.Fatalf("claims per build = %v; want %v", seen.seen, want)
	}
	for i := range want {
		if len(seen.seen[i]) != len(want[i]) || !maps.Equal(seen.seen[i], want[i]) {
			t.Fatalf("claims per build = %v; want %v", seen.seen, want)
		}
	}
	assertNoClaims(t, rec)
}
