package pve

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// adoptFakeNodes is a minimal nodes.Service whose ListQemu is driven by a
// per-call function so a test can model a replica that starts clone-locked and
// later settles. Embedding nodes.Service means every unused method is present
// (and panics if unexpectedly invoked).
type adoptFakeNodes struct {
	nodes.Service
	mu    sync.Mutex
	calls int
	fn    func(call int) (*nodes.ListQemuResponse, error)
}

func (f *adoptFakeNodes) ListQemu(_ context.Context, _ string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(n)
}

func (f *adoptFakeNodes) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// adoptFakeClient embeds Client (nil) so it satisfies the interface; only
// Nodes() is overridden, which is all the adopt path touches.
type adoptFakeClient struct {
	Client
	nodes nodes.Service
}

func (f *adoptFakeClient) Nodes() nodes.Service { return f.nodes }

// listFromRaw builds a *nodes.ListQemuResponse from raw JSON object strings,
// reproducing PVE's wire shape (integer template flag, lock string) verbatim.
func listFromRaw(objs ...string) *nodes.ListQemuResponse {
	resp := make(nodes.ListQemuResponse, 0, len(objs))
	for _, o := range objs {
		resp = append(resp, json.RawMessage(o))
	}
	return &resp
}

func adoptClient(fn func(call int) (*nodes.ListQemuResponse, error)) (*adoptFakeClient, *adoptFakeNodes) {
	n := &adoptFakeNodes{fn: fn}
	return &adoptFakeClient{nodes: n}, n
}

const (
	adoptSHA  = "abc12345"
	adoptNode = "pve2"
)

// adoptCtxFakeNodes is like adoptFakeNodes but passes the caller context to fn,
// allowing tests to capture and inspect the context passed to ListQemu.
type adoptCtxFakeNodes struct {
	nodes.Service
	mu          sync.Mutex
	calls       int
	capturedCtx context.Context
	fn          func(call int, ctx context.Context) (*nodes.ListQemuResponse, error)
}

func (f *adoptCtxFakeNodes) ListQemu(ctx context.Context, _ string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(n, ctx)
}

// adoptCtxFakeClient exposes adoptCtxFakeNodes via Nodes().
type adoptCtxFakeClient struct {
	Client
	nodesvc *adoptCtxFakeNodes
}

func (c *adoptCtxFakeClient) Nodes() nodes.Service { return c.nodesvc }

func adoptShaTag() string { return "bosh-stemcell-sha-" + adoptSHA }

// adoptCacheTag is the generation marker every replica this CPI builds carries
// from creation — including while it is still mid-build, which is what
// findReplicaCandidate looks for. Fixtures need it or the generation gate
// hides them.
const adoptCacheTag = "bosh-stemcell-cache"

// adoptReplicaTags is the full tag string of a replica this CPI built: cache
// marker, content sha, and the per-node replica tag.
func adoptReplicaTags() string {
	return adoptCacheTag + ";" + adoptShaTag() + ";" + adoptNodeTag()
}
func adoptNodeTag() string { return ReplicaNodeTagForNode(adoptNode) }

// ---- findReplicaCandidate ----

func TestFindReplicaCandidate_InFlightCloneLocked(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40001,"template":0,"lock":"clone","tags":"` + tags + `"}`,
		), nil
	})
	cand, found, err := findReplicaCandidate(context.Background(), c, adoptNode, adoptSHA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for an in-flight tagged replica")
	}
	if cand.VMID != 40001 {
		t.Errorf("VMID = %d; want 40001", cand.VMID)
	}
	if cand.Template {
		t.Error("Template should be false for a not-yet-frozen replica")
	}
	if cand.Lock != "clone" {
		t.Errorf("Lock = %q; want %q", cand.Lock, "clone")
	}
	if cand.settled() {
		t.Error("a clone-locked, unfrozen candidate must not be settled")
	}
}

func TestFindReplicaCandidate_SettledTemplate(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40002,"template":1,"tags":"` + tags + `"}`,
		), nil
	})
	cand, found, err := findReplicaCandidate(context.Background(), c, adoptNode, adoptSHA)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v; want true,nil", found, err)
	}
	if !cand.settled() {
		t.Errorf("a frozen, unlocked template must be settled: %+v", cand)
	}
}

func TestFindReplicaCandidate_RequiresBothTags(t *testing.T) {
	t.Parallel()
	// sha tag present but NO per-node tag → not a replica for this node.
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40003,"template":1,"tags":"` + adoptCacheTag + ";" + adoptShaTag() + `"}`,
		), nil
	})
	_, found, err := findReplicaCandidate(context.Background(), c, adoptNode, adoptSHA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("a primary (no node tag) must not match the per-node replica scan")
	}
}

func TestFindReplicaCandidate_LowestVMIDWins(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	// Two candidates of equal settled-ness (both in-flight clone-locked): the
	// lowest VMID wins, matching ResolveTemplateVMIDForNode's tie-break.
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40010,"template":0,"lock":"clone","tags":"`+tags+`"}`,
			`{"vmid":40005,"template":0,"lock":"clone","tags":"`+tags+`"}`,
		), nil
	})
	cand, found, err := findReplicaCandidate(context.Background(), c, adoptNode, adoptSHA)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cand.VMID != 40005 {
		t.Errorf("VMID = %d; want lowest 40005", cand.VMID)
	}
}

func TestFindReplicaCandidate_PrefersSettledOverLowerVMIDOrphan(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	// A crashed-mid-build orphan at a LOW VMID (unfrozen, no lock, never settles)
	// coexists with a genuine settled template at a HIGHER VMID. The scan must
	// prefer the settled one, not the lower-VMID orphan — otherwise adopt would
	// wait out its whole timeout against an artifact that never freezes.
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40070,"template":0,"tags":"`+tags+`"}`, // orphan, lower VMID, unsettled
			`{"vmid":40090,"template":1,"tags":"`+tags+`"}`, // settled, higher VMID
		), nil
	})
	cand, found, err := findReplicaCandidate(context.Background(), c, adoptNode, adoptSHA)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if cand.VMID != 40090 || !cand.settled() {
		t.Errorf("must prefer the settled higher-VMID replica; got %+v", cand)
	}
}

func TestAdoptReplicaTemplate_AdoptsSettledDespiteLowerVMIDOrphan(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(
			`{"vmid":40071,"template":0,"tags":"`+tags+`"}`,
			`{"vmid":40091,"template":1,"tags":"`+tags+`"}`,
		), nil
	})
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 30*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !adopted || vmid != 40091 {
		t.Fatalf("must adopt the settled replica despite a lower-VMID orphan: got (%d,%v)", vmid, adopted)
	}
}

func TestFindReplicaCandidate_EmptySHANoError(t *testing.T) {
	t.Parallel()
	c, n := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		t.Fatal("ListQemu must not be called for empty sha8")
		return nil, nil
	})
	_, found, err := findReplicaCandidate(context.Background(), c, adoptNode, "")
	if err != nil || found {
		t.Fatalf("empty sha8: found=%v err=%v; want false,nil", found, err)
	}
	if n.callCount() != 0 {
		t.Errorf("ListQemu calls = %d; want 0", n.callCount())
	}
}

// ---- AdoptReplicaTemplate (exported wrapper) ----

// TestAdoptReplicaTemplate_ExportedWrapper_NoCandidate_NotAdopted exercises
// the exported AdoptReplicaTemplate entry point (adoptReplicaTemplate wrapped
// with defaultLockClock()) rather than the unexported clock-injectable form
// every other test in this file uses. The no-candidate path returns
// immediately without sleeping, so the real clock is safe to use here.
func TestAdoptReplicaTemplate_ExportedWrapper_NoCandidate_NotAdopted(t *testing.T) {
	t.Parallel()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(), nil // empty list: no winner
	})
	vmid, adopted, err := AdoptReplicaTemplate(context.Background(), c, adoptNode, adoptSHA, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adopted || vmid != 0 {
		t.Fatalf("no candidate must yield (0,false): got (%d,%v)", vmid, adopted)
	}
}

// TestAdoptReplicaTemplate_ExportedWrapper_SettledImmediately_Adopts exercises
// the exported wrapper's settled-on-first-probe path, which also returns
// without sleeping and is safe against the real clock.
func TestAdoptReplicaTemplate_ExportedWrapper_SettledImmediately_Adopts(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, n := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(`{"vmid":40091,"template":1,"tags":"` + tags + `"}`), nil
	})
	vmid, adopted, err := AdoptReplicaTemplate(context.Background(), c, adoptNode, adoptSHA, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !adopted || vmid != 40091 {
		t.Fatalf("must adopt the settled replica: got (%d,%v)", vmid, adopted)
	}
	if n.callCount() != 1 {
		t.Errorf("ListQemu calls = %d; want 1 (single probe, no wait loop for a settled candidate)", n.callCount())
	}
}

// ---- AdoptReplicaTemplate ----

func TestAdoptReplicaTemplate_NoCandidate_NotAdopted(t *testing.T) {
	t.Parallel()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(), nil // empty list: no winner
	})
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 30*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adopted || vmid != 0 {
		t.Fatalf("no candidate must yield (0,false): got (%d,%v)", vmid, adopted)
	}
}

func TestAdoptReplicaTemplate_SettledImmediately_Adopts(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, n := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(`{"vmid":40020,"template":1,"tags":"` + tags + `"}`), nil
	})
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 30*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !adopted || vmid != 40020 {
		t.Fatalf("settled replica must be adopted: got (%d,%v)", vmid, adopted)
	}
	if n.callCount() != 1 {
		t.Errorf("a settled replica should need exactly one poll; got %d", n.callCount())
	}
}

func TestAdoptReplicaTemplate_WaitsForLockClear_ThenAdopts(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, n := adoptClient(func(call int) (*nodes.ListQemuResponse, error) {
		if call < 3 {
			// First two polls: winner is still cloning.
			return listFromRaw(`{"vmid":40030,"template":0,"lock":"clone","tags":"` + tags + `"}`), nil
		}
		// Third poll: frozen and unlocked.
		return listFromRaw(`{"vmid":40030,"template":1,"tags":"` + tags + `"}`), nil
	})
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 60*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !adopted || vmid != 40030 {
		t.Fatalf("must adopt the winner after its lock clears: got (%d,%v)", vmid, adopted)
	}
	if n.callCount() < 3 {
		t.Errorf("expected at least 3 polls (two locked + one settled); got %d", n.callCount())
	}
}

func TestAdoptReplicaTemplate_Timeout_Retriable(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, _ := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		// Never settles: stays clone-locked forever.
		return listFromRaw(`{"vmid":40040,"template":0,"lock":"clone","tags":"` + tags + `"}`), nil
	})
	// step 20s per sleep, timeout 30s → second deadline check trips.
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 30*time.Second,
		fixedClock(time.Unix(1000, 0), 20*time.Second))
	if err == nil {
		t.Fatal("expected a timeout error when the winner never settles")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("adopt timeout must be TypeRetriableCloud so the director re-drives; got %v", err)
	}
	if adopted || vmid != 0 {
		t.Fatalf("timeout must not report adoption: got (%d,%v)", vmid, adopted)
	}
}

func TestAdoptReplicaTemplate_InFlightVanishes_NotAdopted(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, _ := adoptClient(func(call int) (*nodes.ListQemuResponse, error) {
		if call == 1 {
			return listFromRaw(`{"vmid":40050,"template":0,"lock":"clone","tags":"` + tags + `"}`), nil
		}
		// Winner's build failed and rolled back: candidate gone.
		return listFromRaw(), nil
	})
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 60*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("a vanished in-flight candidate is not an error: %v", err)
	}
	if adopted || vmid != 0 {
		t.Fatalf("vanished candidate must yield (0,false) so the caller builds: got (%d,%v)", vmid, adopted)
	}
}

// TestAdoptReplicaTemplate_LoopContextHasDeadline verifies that findReplicaCandidate
// calls inside the adopt-wait loop receive a context with a deadline derived from
// the adoption timeout, so a hung ListQemu cannot stall past that deadline.
func TestAdoptReplicaTemplate_LoopContextHasDeadline(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()

	acn := &adoptCtxFakeNodes{}
	acn.fn = func(call int, ctx context.Context) (*nodes.ListQemuResponse, error) {
		if call >= 2 {
			acn.mu.Lock()
			acn.capturedCtx = ctx
			acn.mu.Unlock()
		}
		if call == 1 {
			// Initial probe: in-flight clone-locked.
			return listFromRaw(`{"vmid":40080,"template":0,"lock":"clone","tags":"` + tags + `"}`), nil
		}
		// First loop iteration: settled.
		return listFromRaw(`{"vmid":40080,"template":1,"tags":"` + tags + `"}`), nil
	}
	cc := &adoptCtxFakeClient{nodesvc: acn}

	_, adopted, err := adoptReplicaTemplate(
		context.Background(), cc, adoptNode, adoptSHA, 30*time.Second,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !adopted {
		t.Fatal("expected adoption after lock clears")
	}

	acn.mu.Lock()
	loopCtx := acn.capturedCtx
	acn.mu.Unlock()

	if loopCtx == nil {
		t.Fatal("loop context was never captured — loop may not have run")
	}
	if _, hasDeadline := loopCtx.Deadline(); !hasDeadline {
		t.Error("context passed to findReplicaCandidate inside the loop must have a deadline derived from the adoption timeout")
	}
}

func TestAdoptReplicaTemplate_DisabledTimeout_InFlight_NotAdopted(t *testing.T) {
	t.Parallel()
	tags := adoptReplicaTags()
	c, n := adoptClient(func(int) (*nodes.ListQemuResponse, error) {
		return listFromRaw(`{"vmid":40060,"template":0,"lock":"clone","tags":"` + tags + `"}`), nil
	})
	// timeout=0: do not wait. An in-flight candidate is not adopted; caller falls
	// back to its legacy build path (byte-identical when the knob is off).
	vmid, adopted, err := adoptReplicaTemplate(
		context.Background(), c, adoptNode, adoptSHA, 0,
		fixedClock(time.Unix(1000, 0), time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adopted || vmid != 0 {
		t.Fatalf("timeout=0 in-flight must not adopt: got (%d,%v)", vmid, adopted)
	}
	if n.callCount() != 1 {
		t.Errorf("timeout=0 must poll exactly once (no wait loop); got %d", n.callCount())
	}
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (f *adoptFakeNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	empty := nodes.ListNodesResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (f *adoptCtxFakeNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	empty := nodes.ListNodesResponse{}
	return &empty, nil
}
