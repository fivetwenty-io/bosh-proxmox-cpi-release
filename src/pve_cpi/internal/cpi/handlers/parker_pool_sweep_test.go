// parker_pool_sweep_test.go covers one rule. Every funnel that parks or
// transfers a disk runs the parker pool sweep once it has succeeded, and runs
// it not at all when it has failed.
//
// The sweep is best-effort and cosmetic, so a funnel that dropped the call
// would still park the disk, still return success, and still pass every
// assertion anybody would think to write about the park. Watching for the call
// is the only thing that catches it, which is why there is a case here per
// funnel rather than one case for the pair of helpers they share.
//
// These tests swap process-wide seams, so none of them may call t.Parallel.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// sweepCall is one recorded invocation of the pool sweep.
type sweepCall struct {
	client pve.Client
	node   string
	cfg    pve.ParkerConfig
}

// captureParkerPoolSweep swaps the sweep seam for one that records its
// arguments, and restores it when the test ends.
func captureParkerPoolSweep(t *testing.T) *[]sweepCall {
	t.Helper()
	calls := &[]sweepCall{}
	t.Cleanup(setPlaceParkersInPoolForTest(func(
		_ context.Context, c pve.Client, _ *log.Logger, node string, cfg pve.ParkerConfig,
	) error {
		*calls = append(*calls, sweepCall{client: c, node: node, cfg: cfg})
		return nil
	}))
	return calls
}

// assertSweptOnce checks that the funnel swept exactly once, on the node it
// parked on, carrying the configured pool, and holding the client from deps
// rather than one a guard handed it.
func assertSweptOnce(t *testing.T, calls []sweepCall, where, node string, deps Deps) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("%s: parker pool sweeps = %d, want exactly 1", where, len(calls))
	}
	got := calls[0]
	if got.node != node {
		t.Errorf("%s: swept node %q, want the node it parked on, %q", where, got.node, node)
	}
	if got.cfg.Pool != parkerCfgPool {
		t.Errorf("%s: swept pool %q, want %q", where, got.cfg.Pool, parkerCfgPool)
	}
	if deps.PVE != nil && got.client != deps.PVE {
		t.Errorf("%s: the sweep must run on the unguarded client from deps", where)
	}
}

// assertNotSwept checks that a funnel whose park failed left the pool alone.
func assertNotSwept(t *testing.T, calls []sweepCall, where string) {
	t.Helper()
	if len(calls) != 0 {
		t.Errorf("%s: a failed park must not sweep the pool, got %d sweeps", where, len(calls))
	}
}

// ---------------------------------------------------------------------------
// parkFreshDisk (create_disk)
// ---------------------------------------------------------------------------

func TestParkFreshDisk_SweepsThePoolAfterThePark(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkFreshDisk(context.Background(), deps, parkerCfgJobNode,
		"pvd-abc", parkerCfgVolid, "stable-id"); err != nil {
		t.Fatalf("parkFreshDisk: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "parkFreshDisk", parkerCfgJobNode, deps)
}

func TestParkFreshDisk_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkFreshDisk(context.Background(), deps, parkerCfgJobNode,
		"pvd-abc", parkerCfgVolid, "stable-id"); err == nil {
		t.Fatal("parkFreshDisk: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "parkFreshDisk")
}

// ---------------------------------------------------------------------------
// parkAfterDetach (detach_disk)
// ---------------------------------------------------------------------------

func TestParkAfterDetach_SweepsThePoolAfterThePark(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkAfterDetach(context.Background(), deps, "100", "pvd-abc",
		parkerCfgVolid, parkerCfgJobNode, nil); err != nil {
		t.Fatalf("parkAfterDetach: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "parkAfterDetach", parkerCfgJobNode, deps)
}

func TestParkAfterDetach_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkAfterDetach(context.Background(), deps, "100", "pvd-abc",
		parkerCfgVolid, parkerCfgJobNode, nil); err == nil {
		t.Fatal("parkAfterDetach: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "parkAfterDetach")
}

// ---------------------------------------------------------------------------
// handleAlreadyDetachedParked (detach_disk retry)
// ---------------------------------------------------------------------------

// alreadyDetachedDeps builds the deps that funnel needs, which are an empty
// cluster so the holder scan reports the disk free-floating, and a backend that
// names the node the disk sits on.
func alreadyDetachedDeps() Deps {
	return Deps{
		Config:   parkerCfgTestConfig(),
		PVE:      &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: &parkerCfgNodes{}},
		Resolver: parkerCfgResolver{backend: parkerCfgBackend{node: "pve2"}},
		Logger:   log.NewNopLogger(),
	}
}

func TestHandleAlreadyDetachedParked_SweepsThePoolOnTheDisksOwnNode(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := alreadyDetachedDeps()
	if err := handleAlreadyDetachedParked(context.Background(), deps, "pvd-abc", parkerCfgVolid); err != nil {
		t.Fatalf("handleAlreadyDetachedParked: unexpected error: %v", err)
	}

	// The disk's own node, not the job-level one, because that is where the
	// park put the parker.
	assertSweptOnce(t, *calls, "handleAlreadyDetachedParked", "pve2", deps)
}

func TestHandleAlreadyDetachedParked_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := alreadyDetachedDeps()
	if err := handleAlreadyDetachedParked(context.Background(), deps, "pvd-abc", parkerCfgVolid); err == nil {
		t.Fatal("handleAlreadyDetachedParked: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "handleAlreadyDetachedParked")
}

// ---------------------------------------------------------------------------
// parkFreeFloatingStableID (detach_disk)
// ---------------------------------------------------------------------------

func TestParkFreeFloatingStableID_SweepsThePoolAfterThePark(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := alreadyDetachedDeps()
	rd := resolvedDisk{diskCID: "pvd-abc", birth: parkerCfgVolid, volid: parkerCfgVolid, stableID: "bpd-aabbccdd00112233"}
	if err := parkFreeFloatingStableID(context.Background(), deps, rd); err != nil {
		t.Fatalf("parkFreeFloatingStableID: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "parkFreeFloatingStableID", "pve2", deps)
}

func TestParkFreeFloatingStableID_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := alreadyDetachedDeps()
	rd := resolvedDisk{diskCID: "pvd-abc", birth: parkerCfgVolid, volid: parkerCfgVolid, stableID: "bpd-aabbccdd00112233"}
	if err := parkFreeFloatingStableID(context.Background(), deps, rd); err == nil {
		t.Fatal("parkFreeFloatingStableID: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "parkFreeFloatingStableID")
}

// ---------------------------------------------------------------------------
// parkFreeFloatingCrossNodeDisk (attach_disk migration)
// ---------------------------------------------------------------------------

// crossNodeBackend is a node-local backend, which is the shape that sends an
// attach through the pre-migration park.
type crossNodeBackend struct{ node string }

func (b crossNodeBackend) Kind() pve.BackendKind { return pve.BackendLocal }

func (b crossNodeBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return b.node, nil
}

func (b crossNodeBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return b.node, nil
}

type crossNodeResolver struct{ backend crossNodeBackend }

func (r crossNodeResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return r.backend, nil
}

func crossNodeDeps() Deps {
	return Deps{
		Config:   parkerCfgTestConfig(),
		PVE:      &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: &parkerCfgNodes{}},
		Resolver: crossNodeResolver{backend: crossNodeBackend{node: "pve2"}},
		Logger:   log.NewNopLogger(),
	}
}

func TestParkFreeFloatingCrossNodeDisk_SweepsThePoolOnTheDisksNode(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := crossNodeDeps()
	rd := resolvedDisk{diskCID: "pvd-abc", birth: parkerCfgVolid, volid: parkerCfgVolid, stableID: "bpd-aabbccdd00112233"}
	// The funnel goes on to re-resolve the holder, and the empty cluster here
	// reports the just-created parker as not yet visible, which is the
	// retriable answer that path gives. The park itself succeeded, and the
	// sweep follows the park, so that later verdict is a separate concern from
	// the one under test.
	_, _, err := parkFreeFloatingCrossNodeDisk(context.Background(), deps, "attach_disk",
		&rd, parkerCfgJobNode, parkerReadConfigFor(deps))
	if err == nil {
		t.Fatal("the holder re-resolve must report the parker as not yet visible here")
	}

	assertSweptOnce(t, *calls, "parkFreeFloatingCrossNodeDisk", "pve2", deps)
}

func TestParkFreeFloatingCrossNodeDisk_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := crossNodeDeps()
	rd := resolvedDisk{diskCID: "pvd-abc", birth: parkerCfgVolid, volid: parkerCfgVolid, stableID: "bpd-aabbccdd00112233"}
	_, _, err := parkFreeFloatingCrossNodeDisk(context.Background(), deps, "attach_disk",
		&rd, parkerCfgJobNode, parkerReadConfigFor(deps))
	if err == nil {
		t.Fatal("parkFreeFloatingCrossNodeDisk: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "parkFreeFloatingCrossNodeDisk")
}

// ---------------------------------------------------------------------------
// The three transfer funnels, which move a disk onto a parker by reassignment
// rather than parking a free-floating one. These drive the real transfer
// against the stateful single-node fake in disk_identity_internal_test.go, so
// the sweep is watched in the flow it actually runs in, and a failure is
// injected through the fake's one-shot move error.
// ---------------------------------------------------------------------------

// transferFunnelDeps wires the identity fake to a parked-strategy config that
// names a parker pool, so the sweep the funnel runs carries a pool to assert
// on.
func transferFunnelDeps(c *idFakeClient) Deps {
	deps := idTestDeps(c)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.ParkerPrefix = parkerCfgPrefix
	deps.Config.ParkerPool = "{prefix}-parker"
	deps.Logger = log.NewNopLogger()
	return deps
}

// transferFunnelClient is the fake both detach funnels below run against: one
// workload VM holding a stable-ID disk, and one parker ready to receive it.
func transferFunnelClient(volid string) *idFakeClient {
	return newIDFakeClient(map[int]map[string]any{
		700:   {"scsi1": volid + ",serial=" + idTestToken + ",size=10G"},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
}

// resolveTransferDisk resolves the disk the way the handler's own caller does.
func resolveTransferDisk(t *testing.T, deps Deps, diskCID string) resolvedDisk {
	t.Helper()
	ctx := context.Background()
	bare, meta, decErr := decodeDiskCID(ctx, deps, "detach_disk", diskCID)
	if decErr != nil {
		t.Fatalf("decode disk CID: %v", decErr)
	}
	rd, resolveErr := resolveDiskForOp(ctx, deps, "detach_disk", diskCID, bare, meta)
	if resolveErr != nil {
		t.Fatalf("resolve disk: %v", resolveErr)
	}
	return rd
}

// ---------------------------------------------------------------------------
// handleDetachStableID (detach_disk)
// ---------------------------------------------------------------------------

func TestHandleDetachStableID_SweepsThePoolAfterTheTransfer(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

	if err := handleDetachStableID(context.Background(), deps, "700", 700,
		resolveTransferDisk(t, deps, diskCID)); err != nil {
		t.Fatalf("handleDetachStableID: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "handleDetachStableID", "pve1", deps)
}

func TestHandleDetachStableID_SkipsTheSweepWhenTheTransferFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	rd := resolveTransferDisk(t, deps, diskCID)
	c.moveErr = errors.New("API request failed: storage is busy")

	if err := handleDetachStableID(context.Background(), deps, "700", 700, rd); err == nil {
		t.Fatal("handleDetachStableID: want the transfer's failure, got nil")
	}

	assertNotSwept(t, *calls, "handleDetachStableID")
}

// ---------------------------------------------------------------------------
// detachForeignActiveDisks (delete_vm fast-path retain)
// ---------------------------------------------------------------------------

func TestDetachForeignActiveDisks_SweepsThePoolAfterTheTransfer(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	// The volume is named for VM 777 while VM 700 holds it, which is what
	// makes it foreign, and the serial is what marks it persistent.
	const volid = "data:vm-777-disk-1"
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)

	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700,
		deps.Log(context.Background())); err != nil {
		t.Fatalf("detachForeignActiveDisks: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "detachForeignActiveDisks", "pve1", deps)
}

func TestDetachForeignActiveDisks_SkipsTheSweepWhenTheTransferFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-777-disk-1"
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)
	c.moveErr = errors.New("API request failed: storage is busy")

	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700,
		deps.Log(context.Background())); err == nil {
		t.Fatal("detachForeignActiveDisks: want the transfer's failure, got nil")
	}

	assertNotSwept(t, *calls, "detachForeignActiveDisks")
}

// ---------------------------------------------------------------------------
// retainLegacyEphemeralVolume (delete_vm ephemeral retention)
// ---------------------------------------------------------------------------

// retentionNodes adds the storage content listing that the retention funnel's
// closing proof reads, serving it from the same config store the rest of the
// fake works from. Every other node call delegates to the fake underneath.
type retentionNodes struct {
	sdknodes.Service
	c *idFakeClient
}

func (n *retentionNodes) ListStorageContent(
	_ context.Context, _ string, storage string, _ *sdknodes.ListStorageContentParams,
) (*sdknodes.ListStorageContentResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	resp := sdknodes.ListStorageContentResponse{}
	seen := map[string]bool{}
	for _, cfg := range n.c.configs {
		for _, value := range cfg {
			drive, ok := value.(string)
			if !ok || !strings.HasPrefix(drive, storage+":") {
				continue
			}
			volid := n.c.bareOf(drive)
			if seen[volid] {
				continue
			}
			seen[volid] = true
			raw, err := json.Marshal(map[string]any{"volid": volid, "content": "images"})
			if err != nil {
				return nil, err
			}
			resp = append(resp, raw)
		}
	}
	return &resp, nil
}

// GetStorageContent answers the exact-volume read that follows the listing,
// with a size and a format, which is all the proof asks of it.
func (n *retentionNodes) GetStorageContent(
	_ context.Context, _ string, _, _ string,
) (*sdknodes.GetStorageContentResponse, error) {
	return &sdknodes.GetStorageContentResponse{Format: "raw", Size: sdk.PVEInt(10 << 30)}, nil
}

// retentionClient is the identity fake with those two reads added.
type retentionClient struct{ *idFakeClient }

func (c *retentionClient) Nodes() sdknodes.Service {
	return &retentionNodes{Service: c.idFakeClient.Nodes(), c: c.idFakeClient}
}

// retentionFixture builds a VM holding a retained ephemeral volume whose CID
// is already recorded on the VM's own description, which is the state this
// funnel resumes from.
func retentionFixture(t *testing.T, volid string) (*idFakeClient, Deps) {
	t.Helper()
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)
	deps.PVE = &retentionClient{idFakeClient: c}
	cid := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken})
	pve.UpdateAttachedDiskCID(context.Background(), deps.PVE, deps.Log(context.Background()), "pve1", 700, volid, cid)
	return c, deps
}

func TestRetainLegacyEphemeralVolume_SweepsThePoolAfterTheTransfer(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	_, deps := retentionFixture(t, volid)

	if err := retainLegacyEphemeralVolume(context.Background(), deps, "pve1", "700", 700, volid,
		deps.Log(context.Background())); err != nil {
		t.Fatalf("retainLegacyEphemeralVolume: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "retainLegacyEphemeralVolume", "pve1", deps)
}

func TestRetainLegacyEphemeralVolume_SkipsTheSweepWhenTheTransferFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	c, deps := retentionFixture(t, volid)
	c.moveErr = errors.New("API request failed: storage is busy")

	if err := retainLegacyEphemeralVolume(context.Background(), deps, "pve1", "700", 700, volid,
		deps.Log(context.Background())); err == nil {
		t.Fatal("retainLegacyEphemeralVolume: want the transfer's failure, got nil")
	}

	assertNotSwept(t, *calls, "retainLegacyEphemeralVolume")
}

// ---------------------------------------------------------------------------
// The managed create_disk park, which is the funnel the allocation guard wraps
// ---------------------------------------------------------------------------

// managedParkFixture is the multi-storage park fixture with the parker pool
// left at the default the job spec ships. The fixture builds its config
// literally and never runs ApplyDefaults, so the template is written out here
// the way defaulting would have written it, and ParkerPoolValue renders it
// against the resolved prefix.
func managedParkFixture(t *testing.T) (*managedDiskRequest, *aj.Handle, *managedDiskTestState) {
	t.Helper()
	m, h, state := managedDiskFixture(t, "spread", false)
	m.deps.Config.DetachedDiskStrategy = "parked"
	m.deps.Config.ParkerPool = "{prefix}-parker"
	return m, h, state
}

func TestManagedDiskPark_SweepsThePoolAfterTheParkIsVerified(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	m, h, _ := managedParkFixture(t)
	if _, err := m.execute(t.Context(), h); err != nil {
		t.Fatalf("managed create_disk: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("managed park: parker pool sweeps = %d, want exactly 1", len(*calls))
	}
	got := (*calls)[0]
	if got.node != m.plan.Node {
		t.Errorf("managed park: swept node %q, want the planned node %q", got.node, m.plan.Node)
	}
	if got.cfg.Pool != "bosh-parker" {
		t.Errorf("managed park: swept pool %q, want the default %q", got.cfg.Pool, "bosh-parker")
	}
	if got.client != m.deps.PVE {
		t.Error("managed park: the sweep must run on the unguarded client, never on the guard's")
	}
}

func TestManagedDiskPark_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	m, h, state := managedParkFixture(t)
	state.parkErr = errors.New("lost attach response")
	if _, err := m.execute(t.Context(), h); err == nil {
		t.Fatal("managed create_disk: want the park's failure, got nil")
	}

	assertNotSwept(t, *calls, "managed park")
}

// TestManagedDiskPark_PoolPlacementRunsOutsideTheAllocationGuard is the guard
// test. The hook the managed park installs admits a pool mutation only for a
// bosh-lock- sentinel and refuses everything else, and a refusal poisons the
// allocation for good, so a placement moved inside ParkDisk shows up here as a
// failed create_disk and a record asking for reconciliation. The real sweep
// runs, which is the other half, and the parker ends up in the pool because
// the call went out on the unguarded client after the window closed.
func TestManagedDiskPark_PoolPlacementRunsOutsideTheAllocationGuard(t *testing.T) {
	m, h, state := managedParkFixture(t)

	result, err := m.execute(t.Context(), h)
	if err != nil {
		t.Fatalf("a pool call inside the guard poisons the allocation; create_disk failed with: %v", err)
	}
	if h.Record().State != aj.ReadyToReturn {
		t.Fatalf("allocation state = %v, want %v; a refused pool call inside the guard is what moves it",
			h.Record().State, aj.ReadyToReturn)
	}
	if _, ok := result.(string); !ok {
		t.Fatalf("create_disk returned %T, want the disk CID", result)
	}

	if len(state.configs) != 1 {
		t.Fatalf("expected exactly one created parker, got %d", len(state.configs))
	}
	for vmid := range state.configs {
		if !state.poolMembers["bosh-parker"][int64(vmid)] {
			t.Errorf("parker %d is not in %q; pool members = %v",
				vmid, "bosh-parker", state.poolMembers)
		}
	}
	if comment := state.pools["bosh-parker"]; !pve.IsCPIManagedPoolComment(comment) {
		t.Errorf("the parker pool's comment is %q, want the CPI provenance the empty-pool reaper reads",
			comment)
	}
}
