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
	"fmt"
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
	if got.client != deps.PVE {
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

// parkedDeps is the parked-strategy deps for the two funnels that reach the
// park without touching PVE first. The client is wired even though neither
// funnel calls it, so that assertSweptOnce can compare what the sweep was
// handed against what deps holds.
func parkedDeps() Deps {
	return Deps{
		Config: parkerCfgTestConfig(),
		PVE:    &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: &parkerCfgNodes{}},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// parkFreshDisk (create_disk)
// ---------------------------------------------------------------------------

func TestParkFreshDisk_SweepsThePoolAfterThePark(t *testing.T) {
	_ = captureParkDisk(t, nil)
	calls := captureParkerPoolSweep(t)

	deps := parkedDeps()
	if err := parkFreshDisk(context.Background(), deps, parkerCfgJobNode,
		"pvd-abc", parkerCfgVolid, "stable-id"); err != nil {
		t.Fatalf("parkFreshDisk: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "parkFreshDisk", parkerCfgJobNode, deps)
}

func TestParkFreshDisk_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := parkedDeps()
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

	deps := parkedDeps()
	if err := parkAfterDetach(context.Background(), deps, "100", "pvd-abc",
		parkerCfgVolid, parkerCfgJobNode, nil); err != nil {
		t.Fatalf("parkAfterDetach: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "parkAfterDetach", parkerCfgJobNode, deps)
}

func TestParkAfterDetach_SkipsTheSweepWhenTheParkFails(t *testing.T) {
	_ = captureParkDisk(t, errors.New("simulated park failure"))
	calls := captureParkerPoolSweep(t)

	deps := parkedDeps()
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
//
// The parker carries the prefix tag transferFunnelDeps configures, because a
// park reuses only the parkers of its own prefix. Without it this parker reads
// as a legacy "bosh" one, the transfer builds a second parker beside it, and
// the sweep these tests watch would be watching the wrong guest.
func transferFunnelClient(volid string) *idFakeClient {
	return newIDFakeClient(map[int]map[string]any{
		700: {"scsi1": volid + ",serial=" + idTestToken + ",size=10G"},
		90000: {
			"tags":       "bosh-cpi;bosh-parker;" + pve.ParkerPrefixTagPrefix + parkerCfgPrefix,
			"protection": true,
		},
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

func TestDetachForeignActiveDisks_SweepsOncePerCallNotOncePerDisk(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	// Both disks transfer to a parker on the same node, so one sweep covers
	// them. A sweep per disk would repeat a pool ensure and a node listing for
	// an outcome the first one already reached.
	const secondToken = "bpd-aabbccdd00112244"
	c := newIDFakeClient(map[int]map[string]any{
		700: {
			"scsi1": "data:vm-777-disk-1,serial=" + idTestToken + ",size=10G",
			"scsi2": "data:vm-778-disk-1,serial=" + secondToken + ",size=10G",
		},
		// The prefix tag is here for the reason transferFunnelClient carries
		// one, which is that a park reuses only the parkers of its own prefix.
		90000: {
			"tags":       "bosh-cpi;bosh-parker;" + pve.ParkerPrefixTagPrefix + parkerCfgPrefix,
			"protection": true,
		},
	})
	deps := transferFunnelDeps(c)

	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700,
		deps.Log(context.Background())); err != nil {
		t.Fatalf("detachForeignActiveDisks: unexpected error: %v", err)
	}

	assertSweptOnce(t, *calls, "detachForeignActiveDisks", "pve1", deps)
}

func TestDetachForeignActiveDisks_SkipsTheSweepWhenNothingTransferred(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	// A legacy foreign disk carries no serial, so it is plain-detached and no
	// parker is involved. Nothing changed about pool membership, so nothing
	// sweeps.
	c := newIDFakeClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-777-disk-1,size=10G"},
	})
	deps := transferFunnelDeps(c)

	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700,
		deps.Log(context.Background())); err != nil {
		t.Fatalf("detachForeignActiveDisks: unexpected error: %v", err)
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
// The two resume sites, which converge a transfer an interruption left in
// flight. The parker the interrupted transfer created is already on the node
// and already outside the pool, so a resume that lands the disk has to sweep
// for the same reason a transfer does.
// ---------------------------------------------------------------------------

// captureResumeDiskTransfer swaps the resume seam for one that records the
// config it was handed, fails with resumeErr when that is set, and otherwise
// runs converge, which is how a case moves the fake into the state a real
// resume would have left. A nil converge leaves the fake alone.
func captureResumeDiskTransfer(t *testing.T, resumeErr error, converge func()) *[]pve.ParkerConfig {
	t.Helper()
	calls := &[]pve.ParkerConfig{}
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		_ context.Context, _ pve.Client, _ *log.Logger, intent pve.DiskTransferIntent,
		_ string, cfg pve.ParkerConfig, _ pve.ParkContext,
	) (string, error) {
		*calls = append(*calls, cfg)
		if resumeErr != nil {
			return "", resumeErr
		}
		if converge != nil {
			converge()
		}
		return intent.Volid, nil
	}))
	return calls
}

// midTransferDisk is the disk resumeTransferIfNeeded converges, carrying an
// intent whose parker sits on a node other than the one the request is aimed
// at, so the case can tell the two apart.
func midTransferDisk(volid string) resolvedDisk {
	return resolvedDisk{
		diskCID:  "pvd-abc",
		birth:    volid,
		volid:    volid,
		meta:     &pve.DiskCIDMeta{ID: idTestToken},
		stableID: idTestToken,
		intent: &pve.DiskTransferIntent{
			ParkerVMID: 90000, ParkerNode: "pve2", Slot: "scsi0", Volid: volid, SourceVMCID: "700",
		},
	}
}

func TestResumeTransferIfNeeded_SweepsTheParkersOwnNodeAfterTheResume(t *testing.T) {
	calls := captureParkerPoolSweep(t)
	resumes := captureResumeDiskTransfer(t, nil, nil)

	const volid = "data:vm-700-disk-1"
	deps := transferFunnelDeps(transferFunnelClient(volid))

	if _, err := resumeTransferIfNeeded(context.Background(), deps, "detach_disk", midTransferDisk(volid)); err != nil {
		t.Fatalf("resumeTransferIfNeeded: unexpected error: %v", err)
	}

	if len(*resumes) != 1 {
		t.Fatalf("resumes = %d, want exactly 1", len(*resumes))
	}
	// The node here is the parker's, from the intent record. The job-level node
	// this deps carries is pve1, so a sweep that read deps.Config.Node instead
	// would sweep a node the parker is not on.
	assertSweptOnce(t, *calls, "resumeTransferIfNeeded", "pve2", deps)
}

func TestResumeTransferIfNeeded_SkipsTheSweepWhenTheResumeFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)
	_ = captureResumeDiskTransfer(t, errors.New("simulated resume failure"), nil)

	const volid = "data:vm-700-disk-1"
	deps := transferFunnelDeps(transferFunnelClient(volid))

	if _, err := resumeTransferIfNeeded(context.Background(), deps, "detach_disk", midTransferDisk(volid)); err == nil {
		t.Fatal("resumeTransferIfNeeded: want the resume's failure, got nil")
	}

	assertNotSwept(t, *calls, "resumeTransferIfNeeded")
}

// retentionResumeFixture puts the retention fake into the state an interrupted
// transfer leaves behind. The volume is off the source VM, the source VM's
// description still carries its CID, and the parker holds the record the
// resolver reads as a transfer intent. It returns the converge that stands in
// for the real resume, which attaches the volume to the parker slot the record
// names and writes the disk's serial onto it.
func retentionResumeFixture(t *testing.T, volid string) (Deps, func()) {
	t.Helper()
	c, deps := retentionFixture(t, volid)
	cid := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken})
	c.mu.Lock()
	delete(c.configs[700], "scsi1")
	c.configs[90000]["description"] = fmt.Sprintf(
		`<!--BOSH:{"bosh_parked_disks":{%q:{"disk_cid":%q,"parked_at":"2026-08-20T00:00:00Z",`+
			`"node":"pve1","volid":%q,"slot":"scsi0","source_vm_cid":"700"}}}-->`,
		idTestToken, cid, volid)
	c.mu.Unlock()
	converge := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.configs[90000]["scsi0"] = volid + ",serial=" + idTestToken + ",size=10G"
	}
	return deps, converge
}

func TestRetainLegacyEphemeralVolume_SweepsThePoolAfterTheResume(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	deps, converge := retentionResumeFixture(t, volid)
	resumes := captureResumeDiskTransfer(t, nil, converge)

	if err := retainLegacyEphemeralVolume(context.Background(), deps, "pve1", "700", 700, volid,
		deps.Log(context.Background())); err != nil {
		t.Fatalf("retainLegacyEphemeralVolume: unexpected error: %v", err)
	}

	if len(*resumes) != 1 {
		t.Fatalf("resumes = %d, want exactly 1", len(*resumes))
	}
	assertSweptOnce(t, *calls, "retainLegacyEphemeralVolume resume", "pve1", deps)
}

func TestRetainLegacyEphemeralVolume_SkipsTheSweepWhenTheResumeFails(t *testing.T) {
	calls := captureParkerPoolSweep(t)

	const volid = "data:vm-700-disk-1"
	deps, _ := retentionResumeFixture(t, volid)
	resumes := captureResumeDiskTransfer(t, errors.New("simulated resume failure"), nil)

	if err := retainLegacyEphemeralVolume(context.Background(), deps, "pve1", "700", 700, volid,
		deps.Log(context.Background())); err == nil {
		t.Fatal("retainLegacyEphemeralVolume: want the resume's failure, got nil")
	}

	if len(*resumes) != 1 {
		t.Fatalf("resumes = %d, want exactly 1 so the failure came from the resume itself", len(*resumes))
	}
	assertNotSwept(t, *calls, "retainLegacyEphemeralVolume resume")
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

// ---------------------------------------------------------------------------
// The guard scope, which is the rule the funnels above cannot enforce for
// themselves
// ---------------------------------------------------------------------------

// guardScopePools is a pool service that records every call and holds
// membership, so a test can read what the real sweep did once the funnel has
// returned.
type guardScopePools struct {
	pools   map[string]string
	members map[string]map[int64]bool
	calls   []string
}

func newGuardScopePools() *guardScopePools {
	return &guardScopePools{pools: map[string]string{}, members: map[string]map[int64]bool{}}
}

func (p *guardScopePools) CreatePool(_ context.Context, poolID, comment string) error {
	p.calls = append(p.calls, "create:"+poolID)
	p.pools[poolID] = comment
	return nil
}

func (p *guardScopePools) DeletePool(_ context.Context, poolID string) error {
	p.calls = append(p.calls, "delete:"+poolID)
	delete(p.pools, poolID)
	return nil
}

func (p *guardScopePools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.calls = append(p.calls, "comment:"+poolID)
	comment, found := p.pools[poolID]
	return comment, found, nil
}

func (p *guardScopePools) AddVM(_ context.Context, poolID string, vmid int64) error {
	p.calls = append(p.calls, "add:"+poolID)
	if p.members[poolID] == nil {
		p.members[poolID] = map[int64]bool{}
	}
	p.members[poolID][vmid] = true
	return nil
}

func (p *guardScopePools) MoveVMToPool(_ context.Context, poolID string, _ int64) error {
	p.calls = append(p.calls, "move:"+poolID)
	return nil
}

func (p *guardScopePools) PoolHasVM(_ context.Context, poolID string, vmid int64) (bool, error) {
	return p.members[poolID][vmid], nil
}

// guardScopeClient adds a pool service to a fake client that has none, so the
// real sweep has somewhere to put a parker. Everything else falls through to
// the client it wraps.
type guardScopeClient struct {
	pve.Client
	pools *guardScopePools
}

func (c *guardScopeClient) Pools() pve.PoolService { return c.pools }

// guardScopeNodes reports one parker on one node, which is the listing the
// sweep reads to find its candidates. Every other node answers empty, so the
// holder scans that run elsewhere in the funnel see what they saw before.
type guardScopeNodes struct {
	sdknodes.Service
	node string
	vmid int
}

func (n *guardScopeNodes) ListQemu(
	_ context.Context, node string, _ *sdknodes.ListQemuParams,
) (*sdknodes.ListQemuResponse, error) {
	if node != n.node {
		empty := sdknodes.ListQemuResponse{}
		return &empty, nil
	}
	// The prefix tag is what makes this parker one the configured prefix may
	// adopt and pool. A parker without it reads as a legacy "bosh" parker and
	// the sweep under test would pass it over.
	raw, err := json.Marshal(map[string]any{
		"vmid": n.vmid,
		"tags": "bosh-cpi;bosh-parker;" + pve.ParkerPrefixTagPrefix + parkerCfgPrefix,
	})
	if err != nil {
		return nil, err
	}
	resp := sdknodes.ListQemuResponse{raw}
	return &resp, nil
}

// guardScopeAdmission records every mutation a guard admits and applies the one
// rule the real hooks apply to pools, which is that a pool outside bosh-lock-
// is refused and the refusal poisons the allocation. It stands in for
// managedDiskLifecycleGuard.before, whose other arms need a whole allocation
// journal that has nothing to do with what these two cases are about.
type guardScopeAdmission struct {
	// seen records every mutation as "Service.Method" with the pool ID
	// appended for a pool mutation, which is what the pool assertions read.
	seen []string
	// unrelatedPools counts the pool mutations outside bosh-lock-, which are
	// the ones the real hook refuses.
	unrelatedPools int
}

func (a *guardScopeAdmission) hooks() ManagedAllocationHooks {
	return ManagedAllocationHooks{
		Before: func(_ context.Context, call ManagedAllocationMutation) (string, error) {
			key := call.Service + "." + call.Method
			if call.Service != managedDiskServicePool {
				a.seen = append(a.seen, key)
				return key, nil
			}
			pool, _ := call.Args[managedArgumentPoolID].(string)
			a.seen = append(a.seen, key+":"+pool)
			if !strings.HasPrefix(pool, "bosh-lock-") ||
				(call.Method != "CreatePool" && call.Method != "DeletePool") {
				a.unrelatedPools++
				return "", errors.New("unrelated pool mutation in disk lifecycle")
			}
			return key, nil
		},
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(_ context.Context, _ ManagedAllocationMutation, _ string, err error) error { return err },
	}
}

// guardedDepsFor wraps deps the way the managed detach and attach paths wrap
// their own, and returns the guard alongside so a test can read whether it was
// poisoned. It calls wrapManagedDiskClient rather than building the decorator
// chain of its own, so that a decorator added to the production chain reaches
// these cases too.
func guardedDepsFor(t *testing.T, deps Deps, admission *guardScopeAdmission) (Deps, *ManagedAllocationGuard) {
	t.Helper()
	guard, err := NewManagedAllocationGuard(deps.PVE, admission.hooks())
	if err != nil {
		t.Fatalf("build the allocation guard: %v", err)
	}
	guarded := deps
	guarded.PVE = wrapManagedDiskClient(guard, &managedDiskLifecycle{})
	return guarded, guard
}

func TestUnguardedPVE_WalksOutOfEveryAllocationGuardDecorator(t *testing.T) {
	base := &guardScopeClient{
		Client: &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: &parkerCfgNodes{}},
		pools:  newGuardScopePools(),
	}
	admission := &guardScopeAdmission{}
	guard, err := NewManagedAllocationGuard(base, admission.hooks())
	if err != nil {
		t.Fatalf("build the allocation guard: %v", err)
	}

	if got := unguardedPVE(base); got != pve.Client(base) {
		t.Errorf("a client no guard wraps must come back unchanged, got %T", got)
	}
	if got := unguardedPVE(guard.Client()); got != pve.Client(base) {
		t.Errorf("the guard's own decorator must unwrap to the client it guards, got %T", got)
	}
	// The chain comes from wrapManagedDiskClient, which is the one production
	// builds, so a decorator added there is unwrapped here or this case fails.
	lifecycle := wrapManagedDiskClient(guard, &managedDiskLifecycle{})
	if got := unguardedPVE(lifecycle); got != pve.Client(base) {
		t.Errorf("the lifecycle decorator must unwrap through the guard as well, got %T", got)
	}
}

// TestHandleDetachStableID_PlacesThePoolOutsideTheLifecycleGuard drives the
// detach funnel with the deps shape managedDiskOperation hands it, which is a
// guarded client the funnel cannot tell from an unguarded one. The real sweep
// runs. A sweep that went through the guard would be refused as an unrelated
// pool mutation, the refusal would poison the allocation, and the detach would
// fail after the disk had already moved onto the parker.
func TestHandleDetachStableID_PlacesThePoolOutsideTheLifecycleGuard(t *testing.T) {
	const volid = "data:vm-700-disk-1"
	c := transferFunnelClient(volid)
	deps := transferFunnelDeps(c)
	pools := newGuardScopePools()
	deps.PVE = &guardScopeClient{Client: c, pools: pools}
	diskCID := overlayCID(t, volid, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	rd := resolveTransferDisk(t, deps, diskCID)

	admission := &guardScopeAdmission{}
	guarded, guard := guardedDepsFor(t, deps, admission)

	if err := handleDetachStableID(context.Background(), guarded, "700", 700, rd); err != nil {
		t.Fatalf("the detach must succeed; a pool call inside the guard is what fails it: %v", err)
	}
	if err := guard.Err(); err != nil {
		t.Fatalf("the allocation guard is poisoned: %v", err)
	}
	if admission.unrelatedPools != 0 {
		t.Errorf("the guard saw %d pool mutations outside bosh-lock-, want 0; it saw %v",
			admission.unrelatedPools, admission.seen)
	}
	if !pools.members[parkerCfgPool][90000] {
		t.Errorf("the parker must be in %q once the detach returns, got %v",
			parkerCfgPool, pools.members)
	}
}

// TestParkFreeFloatingCrossNodeDisk_PlacesThePoolOutsideTheLifecycleGuard is
// the attach-side half of the case above. attach_disk shadows its own deps with
// the guarded copy before guardAndUnparkBeforeAttach reaches this funnel, so
// the funnel parks and then sweeps holding a client it did not choose.
//
// The funnel's own return is not the assertion here. Its holder re-resolve
// reports the just-created parker as not yet visible against this fixture,
// which the sweep case above already documents. What this case reads is the
// guard, which must be clean, and the pool, which must hold the parker.
func TestParkFreeFloatingCrossNodeDisk_PlacesThePoolOutsideTheLifecycleGuard(t *testing.T) {
	_ = captureParkDisk(t, nil)

	deps := crossNodeDeps()
	pools := newGuardScopePools()
	deps.PVE = &guardScopeClient{
		Client: &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: &guardScopeNodes{node: "pve2", vmid: 90000}},
		pools:  pools,
	}

	admission := &guardScopeAdmission{}
	guarded, guard := guardedDepsFor(t, deps, admission)

	rd := resolvedDisk{diskCID: "pvd-abc", birth: parkerCfgVolid, volid: parkerCfgVolid, stableID: "bpd-aabbccdd00112233"}
	_, _, _ = parkFreeFloatingCrossNodeDisk(context.Background(), guarded, "attach_disk",
		&rd, parkerCfgJobNode, parkerReadConfigFor(guarded))

	if err := guard.Err(); err != nil {
		t.Fatalf("the allocation guard is poisoned: %v", err)
	}
	if admission.unrelatedPools != 0 {
		t.Errorf("the guard saw %d pool mutations outside bosh-lock-, want 0; it saw %v",
			admission.unrelatedPools, admission.seen)
	}
	if !pools.members[parkerCfgPool][90000] {
		t.Errorf("the parker must be in %q once the funnel returns, got %v",
			parkerCfgPool, pools.members)
	}
}
