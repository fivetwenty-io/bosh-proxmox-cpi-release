package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// These tests drive the disk handlers through the handler while the VM that
// holds the disk moves, locks, or sits on a node that cannot answer. The
// fixture is VM 101 holding one legacy disk, and a node that is not the VM's
// answers every call about the VM with config missing, the way PVE does, so a
// call sent to a stale node fails here as it would there.

const (
	motionDiskVolid = "local-lvm:vm-9001-disk-0"
	motionNode1     = "pve-node1"
	motionNode2     = "pve-node2"
)

// diskMotionFixture is VM 101 and the one disk it holds.
type diskMotionFixture struct {
	mu   sync.Mutex
	node string
	// configNode is where the VM's config really is when it differs from
	// the node the cluster index names, which is how a stale index row looks.
	configNode string
	lock       string
	offline    map[string]bool
	reads      int
	onRead     func(f *diskMotionFixture, reads int)
	// beforeWrite runs under the lock at the start of every write, with the
	// operation name, the node it was sent to, and how many writes of that
	// operation came before it. A test moves the VM here.
	beforeWrite func(f *diskMotionFixture, op, node string, call int)
	// writeFn answers a write after beforeWrite: the error it returns, and
	// whether the write took effect.
	writeFn func(op, node string, call int) (err error, landed bool)

	sizeGiB     int
	optStr      string
	snaps       []string
	description string
	calls       []string
	counts      map[string]int

	// haServiceReads counts reads of the HA service list.
	haServiceReads atomic.Int32

	deps handlers.Deps
}

func newDiskMotionFixture(t *testing.T, node string) *diskMotionFixture {
	t.Helper()
	f := &diskMotionFixture{node: node, offline: map[string]bool{}, counts: map[string]int{}, sizeGiB: 10}
	f.optStr = motionDiskVolid
	svc := &diskMotionQEMU{f: f}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, at, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.begin("config", at); err != nil {
				return err
			}
			err, landed := f.finish("config", at)
			if landed && params.Description != nil {
				f.description = *params.Description
			}
			if landed && params.Delete != nil && *params.Delete == "scsi1" {
				f.optStr = ""
			}
			return err
		},
	}
	f.deps = testDepsFoundVM(101, svc, nodesSvc, nil, &mockAgentService{})
	// A detached disk is left free, so no test needs a parker VM.
	f.deps.Config.DetachedDiskStrategy = "free"
	mc := f.deps.PVE.(*mockPVEClient)
	inner := &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads++
			if f.onRead != nil {
				f.onRead(f, f.reads)
			}
			row := map[string]any{"vmid": 101, "node": f.node, "type": "qemu", "tags": "bosh-cpi"}
			if f.lock != "" {
				row["lock"] = f.lock
			}
			raw, _ := json.Marshal(row)
			return &cluster.ListResourcesResponse{raw}, nil
		},
		listConfigNodesFn: func(context.Context) (*cluster.ListConfigNodesResponse, error) {
			return &cluster.ListConfigNodesResponse{
				json.RawMessage(`{"name":"pve-node1","node":"pve-node1"}`),
				json.RawMessage(`{"name":"pve-node2","node":"pve-node2"}`),
			}, nil
		},
		listStatusFn: func(context.Context) (*cluster.ListStatusResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			members := []string{}
			for _, n := range []string{motionNode1, motionNode2} {
				if !f.offline[n] {
					members = append(members, n)
				}
			}
			return handlers.AbsenceOnlineStatus(members...), nil
		},
	}
	mc.clusterSvc = &haReadCountingCluster{mockClusterSvc: inner, reads: &f.haServiceReads}
	return f
}

// haReadCountingCluster counts the reads of the HA service list, which is the
// read of a VM's HA state. The node health check reads the CRM's manager view
// and the cluster status, and neither is counted.
type haReadCountingCluster struct {
	*mockClusterSvc
	reads *atomic.Int32
}

func (c *haReadCountingCluster) ListHaResources(
	ctx context.Context, params *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	c.reads.Add(1)
	return c.mockClusterSvc.ListHaResources(ctx, params)
}

// home is the node whose config holds the VM.
func (f *diskMotionFixture) home() string {
	if f.configNode != "" {
		return f.configNode
	}
	return f.node
}

// missing is the answer of a node that holds no config for the VM.
func (f *diskMotionFixture) missing(node string) error {
	return makeAPIErr(500, fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/101.conf' does not exist", node))
}

// begin records a write and runs the test's hook. It returns config missing
// when the node is not the VM's, and it needs f.mu held.
func (f *diskMotionFixture) begin(op, node string) error {
	f.counts[op]++
	f.calls = append(f.calls, op+"@"+node)
	if f.beforeWrite != nil {
		f.beforeWrite(f, op, node, f.counts[op])
	}
	if node != f.home() {
		return f.missing(node)
	}
	return nil
}

// finish asks the test how the write ended and needs f.mu held.
func (f *diskMotionFixture) finish(op, node string) (error, bool) {
	if f.writeFn != nil {
		return f.writeFn(op, node, f.counts[op])
	}
	return nil, true
}

func (f *diskMotionFixture) config(node string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if node != f.home() {
		return nil, f.missing(node)
	}
	cfg := map[string]any{
		"scsi0":       "local-lvm:vm-101-disk-0,size=8G",
		"description": f.description,
		"tags":        "bosh-cpi",
	}
	if f.optStr != "" {
		cfg["scsi1"] = fmt.Sprintf("%s,size=%dG", f.optStr, f.sizeGiB)
	}
	return cfg, nil
}

func (f *diskMotionFixture) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// writesTo returns the recorded writes of op, as the nodes they went to.
func (f *diskMotionFixture) writesTo(op string) []string {
	var out []string
	for _, c := range f.callList() {
		if o, n, ok := strings.Cut(c, "@"); ok && o == op {
			out = append(out, n)
		}
	}
	return out
}

func (f *diskMotionFixture) requireWrites(t *testing.T, op string, want ...string) {
	t.Helper()
	got := f.writesTo(op)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s writes = %v, want %v (all calls %v)", op, got, want, f.callList())
	}
}

// moveOnFirst returns a hook that moves the VM to node2 as the first write of
// op arrives.
func moveOnFirst(op string) func(f *diskMotionFixture, got, node string, call int) {
	return func(f *diskMotionFixture, got, _ string, call int) {
		if got == op && call == 1 {
			f.node = motionNode2
		}
	}
}

// movedByWait leaves the VM locked on node1 until the fourth index read, then
// lands it on node2.
func (f *diskMotionFixture) migratesDuringTheWait() {
	f.lock = "migrate"
	f.onRead = func(f *diskMotionFixture, reads int) {
		if reads >= 4 {
			f.node, f.lock = motionNode2, ""
		}
	}
}

// diskMotionQEMU is the qemu service of the fixture.
type diskMotionQEMU struct {
	qemu.Service
	f *diskMotionFixture
}

func (q *diskMotionQEMU) Config(_ context.Context, node string, _ int) (map[string]any, error) {
	return q.f.config(node)
}

func (q *diskMotionQEMU) ResizeDisk(_ context.Context, node string, _ int, _ string, deltaGiB int) (string, error) {
	f := q.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("resize", node); err != nil {
		return "", err
	}
	err, landed := f.finish("resize", node)
	if landed {
		f.sizeGiB += deltaGiB
	}
	return "", err
}

func (q *diskMotionQEMU) Snapshot(_ context.Context, node string, _ int, name string, _ map[string]any) (string, error) {
	f := q.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("snapshot", node); err != nil {
		return "", err
	}
	if slices.Contains(f.snaps, name) {
		return "", makeAPIErr(500, fmt.Sprintf("snapshot name '%s' already exists", name))
	}
	err, landed := f.finish("snapshot", node)
	if landed {
		f.snaps = append(f.snaps, name)
	}
	return "", err
}

func (q *diskMotionQEMU) DeleteSnapshot(_ context.Context, node string, _ int, name string) error {
	f := q.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("delsnap", node); err != nil {
		return err
	}
	err, landed := f.finish("delsnap", node)
	if landed {
		kept := f.snaps[:0:0]
		for _, s := range f.snaps {
			if s != name {
				kept = append(kept, s)
			}
		}
		f.snaps = kept
	}
	return err
}

func (q *diskMotionQEMU) ListSnapshots(_ context.Context, node string, _ int) ([]map[string]any, error) {
	f := q.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if node != f.home() {
		return nil, f.missing(node)
	}
	out := []map[string]any{{"name": "current"}}
	for _, s := range f.snaps {
		out = append(out, map[string]any{"name": s})
	}
	return out, nil
}

func (q *diskMotionQEMU) AttachDisk(
	_ context.Context, node string, _ int, volid string, _ string, opts *qemu.AttachOpts,
) (string, error) {
	f := q.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("drive", node); err != nil {
		return "", err
	}
	err, landed := f.finish("drive", node)
	if landed {
		f.optStr = volid
	}
	if opts != nil && opts.DiskID != "" {
		return opts.DiskID, err
	}
	return "scsi2", err
}

// requireNotGone fails a test when err says the VM or the disk is gone.
func requireNotGone(t *testing.T, err error) {
	t.Helper()
	var typed *cpierrors.Error
	if errors.As(err, &typed) && (typed.Type() == cpierrors.TypeVMNotFound || typed.Type() == cpierrors.TypeDiskNotFound) {
		t.Fatalf("a VM that is still listed must not be reported gone: %v", err)
	}
}

// motionResizeMB is the size every motion test resizes the fixture disk to.
const motionResizeMB = 20480

func (f *diskMotionFixture) resize(ctx context.Context) error {
	_, err := handlers.HandleResizeDisk(f.deps).Handle(ctx, marshalArgs(motionDiskCIDRaw, motionResizeMB), jsonrpc.Context{})
	return err
}

// motionDiskCIDRaw is the envelope CID of the fixture's disk.
var motionDiskCIDRaw = func() string {
	cid, err := pve.EncodeDiskCID(motionDiskVolid, nil)
	if err != nil {
		panic(err)
	}
	return cid
}()

// motionOtherDiskCIDRaw is a second disk, free-floating, that attach_disk puts
// on the fixture's VM.
var motionOtherDiskCIDRaw = func() string {
	cid, err := pve.EncodeDiskCID("local-lvm:vm-9002-disk-0", nil)
	if err != nil {
		panic(err)
	}
	return cid
}()

// ---------------------------------------------------------------------------
// resize_disk
// ---------------------------------------------------------------------------

func TestHandleResizeDisk_WaitsOutMigrationAndResizesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if err := f.resize(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "resize", motionNode2)
	if f.sizeGiB != 20 {
		t.Fatalf("size = %d GiB, want 20", f.sizeGiB)
	}
}

func TestHandleResizeDisk_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("resize")

	if err := f.resize(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "resize", motionNode1, motionNode2)
	if f.sizeGiB != 20 {
		t.Fatalf("size = %d GiB, want 20 (grown once)", f.sizeGiB)
	}
}

func TestHandleResizeDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.resize(settleTestCtx()), motionNode2)
	f.requireWrites(t, "resize")
}

func TestHandleResizeDisk_LostAnswerThatLandedIsNotResent(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(_, _ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), true
		}
		return nil, true
	}

	if err := f.resize(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "resize", motionNode1)
	if f.sizeGiB != 20 {
		t.Fatalf("size = %d GiB, want 20", f.sizeGiB)
	}
}

// ---------------------------------------------------------------------------
// snapshot_disk
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) snapshot(ctx context.Context) (any, error) {
	return handlers.HandleSnapshotDisk(f.deps).Handle(ctx, marshalArgs(motionDiskCIDRaw), jsonrpc.Context{})
}

func TestHandleSnapshotDisk_WaitsOutMigrationAndSnapshotsOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if _, err := f.snapshot(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "snapshot", motionNode2)
}

func TestHandleSnapshotDisk_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("snapshot")

	if _, err := f.snapshot(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "snapshot", motionNode1, motionNode2)
	if len(f.snaps) != 1 {
		t.Fatalf("snapshots = %v, want exactly one", f.snaps)
	}
}

func TestHandleSnapshotDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	_, err := f.snapshot(settleTestCtx())
	requireHARetriable(t, err, motionNode2)
	f.requireWrites(t, "snapshot")
}

func TestHandleSnapshotDisk_LostAnswerThatLandedIsNotTakenTwice(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(_, _ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), true
		}
		return nil, true
	}

	if _, err := f.snapshot(fastRetryCtx(settleTestCtx())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The handler's own replay loop sends the same name again, PVE refuses it
	// as already existing, and the listing shows the first send landed.
	f.requireWrites(t, "snapshot", motionNode1, motionNode1)
	if len(f.snaps) != 1 {
		t.Fatalf("snapshots = %v, want exactly one", f.snaps)
	}
}

// ---------------------------------------------------------------------------
// delete_snapshot
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) deleteSnapshot(ctx context.Context) error {
	_, err := handlers.HandleDeleteSnapshot(f.deps).Handle(ctx, marshalArgs(pve.FormatSnapshotCID("101", "snap1")), jsonrpc.Context{})
	return err
}

func TestHandleDeleteSnapshot_WaitsOutMigrationAndDeletesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.snaps = []string{"snap1"}
	f.migratesDuringTheWait()

	if err := f.deleteSnapshot(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "delsnap", motionNode2)
	if len(f.snaps) != 0 {
		t.Fatalf("snapshots = %v, want none", f.snaps)
	}
}

func TestHandleDeleteSnapshot_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.snaps = []string{"snap1"}
	f.beforeWrite = moveOnFirst("delsnap")

	if err := f.deleteSnapshot(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "delsnap", motionNode1, motionNode2)
}

func TestHandleDeleteSnapshot_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.snaps = []string{"snap1"}
	f.offline[motionNode2] = true

	requireHARetriable(t, f.deleteSnapshot(settleTestCtx()), motionNode2)
	f.requireWrites(t, "delsnap")
}

// TestHandleDeleteSnapshot_NotFoundWithSnapshotStillListedIsRetriable answers
// 404 to the delete while the VM is present and still lists the snapshot. The
// snapshot is not gone, so the call is a retriable error and never success.
func TestHandleDeleteSnapshot_NotFoundWithSnapshotStillListedIsRetriable(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.snaps = []string{"snap1"}
	f.writeFn = func(string, string, int) (error, bool) { return notFoundAPIErr(), false }

	err := f.deleteSnapshot(settleTestCtx())
	requireHARetriable(t, err)
	requireNotGone(t, err)
}

// TestHandleDeleteSnapshot_NotFoundWithSnapshotUnlistedIsGone answers 404 to the
// delete while the VM is present and no longer lists the snapshot, which is the
// goal state.
func TestHandleDeleteSnapshot_NotFoundWithSnapshotUnlistedIsGone(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(string, string, int) (error, bool) { return notFoundAPIErr(), false }

	if err := f.deleteSnapshot(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestHandleDeleteSnapshot_MissingVMIsGoneOnlyThroughTheAbsenceProof lists the
// VM in no index row and holds its config on no node. Every member agrees it is
// gone, so the delete is an idempotent success. A config that sits on a node the
// index does not name keeps the VM alive, and the call must not report success.
func TestHandleDeleteSnapshot_MissingVMIsGoneOnlyThroughTheAbsenceProof(t *testing.T) {
	t.Parallel()
	t.Run("proven absent", func(t *testing.T) {
		t.Parallel()
		f := newDiskMotionFixture(t, motionNode1)
		f.configNode = "nowhere"
		f.onRead = func(f *diskMotionFixture, _ int) { f.node = "nowhere" }
		mc := f.deps.PVE.(*mockPVEClient)
		inner := mc.clusterSvc.(*haReadCountingCluster).mockClusterSvc
		inner.listResourcesFn = func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &cluster.ListResourcesResponse{}, nil
		}
		if err := f.deleteSnapshot(settleTestCtx()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		f.requireWrites(t, "delsnap")
	})
	t.Run("config on a node the index misses", func(t *testing.T) {
		t.Parallel()
		f := newDiskMotionFixture(t, motionNode1)
		f.snaps = []string{"snap1"}
		f.configNode = motionNode2
		mc := f.deps.PVE.(*mockPVEClient)
		inner := mc.clusterSvc.(*haReadCountingCluster).mockClusterSvc
		inner.listResourcesFn = func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return &cluster.ListResourcesResponse{}, nil
		}
		if err := f.deleteSnapshot(settleTestCtx()); err != nil {
			requireNotGone(t, err)
		}
		if len(f.snaps) != 0 {
			t.Fatalf("the VM is alive on node2, so its snapshot must be deleted there, got %v", f.snaps)
		}
		f.requireWrites(t, "delsnap", motionNode2)
	})
}

// ---------------------------------------------------------------------------
// update_disk
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) update(ctx context.Context) error {
	spec := map[string]any{"iothread": true}
	_, err := handlers.HandleUpdateDisk(f.deps).Handle(ctx, marshalArgs(motionDiskCIDRaw, spec), jsonrpc.Context{})
	return err
}

func TestHandleUpdateDisk_WaitsOutMigrationAndWritesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if err := f.update(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode2)
	f.requireWrites(t, "drive", motionNode2)
}

func TestHandleUpdateDisk_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("config")

	if err := f.update(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode1, motionNode2)
	f.requireWrites(t, "drive", motionNode2)
}

func TestHandleUpdateDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.update(settleTestCtx()), motionNode2)
	f.requireWrites(t, "config")
	f.requireWrites(t, "drive")
}

// ---------------------------------------------------------------------------
// get_disks
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) getDisks(ctx context.Context) (any, error) {
	return handlers.HandleGetDisks(f.deps).Handle(ctx, marshalArgs("101"), jsonrpc.Context{})
}

func TestHandleGetDisks_WaitsOutMigration(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	got, err := f.getDisks(settleTestCtx())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cids, ok := got.([]string); !ok || len(cids) != 1 {
		t.Fatalf("disks = %v, want one", got)
	}
}

func TestHandleGetDisks_DeadNodeIsRetriable(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	_, err := f.getDisks(settleTestCtx())
	requireHARetriable(t, err, motionNode2)
}

// TestHandleGetDisks_StaleIndexIsFollowedToTheConfig reads the config on the
// node the index names, which answers config missing because the VM moved, and
// follows the VM to the node that holds it.
func TestHandleGetDisks_StaleIndexIsFollowedToTheConfig(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.configNode = motionNode2

	got, err := f.getDisks(settleTestCtx())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cids, ok := got.([]string); !ok || len(cids) != 1 {
		t.Fatalf("disks = %v, want one", got)
	}
}

// TestHandleGetDisks_ConfigMissingEverywhereIsVMNotFound is the absence proof:
// the index names a node, and no member holds the config.
func TestHandleGetDisks_ConfigMissingEverywhereIsVMNotFound(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.configNode = "nowhere"

	_, err := f.getDisks(settleTestCtx())
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeVMNotFound {
		t.Fatalf("want VMNotFound once every member agrees, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// set_disk_metadata
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) setDiskMetadata(ctx context.Context) error {
	md := map[string]any{"note": "kept"}
	_, err := handlers.HandleSetDiskMetadata(f.deps).Handle(ctx, marshalArgs(motionDiskCIDRaw, md), jsonrpc.Context{})
	return err
}

func TestHandleSetDiskMetadata_WaitsOutMigrationAndWritesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if err := f.setDiskMetadata(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode2)
	if !strings.Contains(f.description, "kept") {
		t.Fatalf("description %q does not hold the metadata", f.description)
	}
}

func TestHandleSetDiskMetadata_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("config")

	if err := f.setDiskMetadata(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode1, motionNode2)
}

func TestHandleSetDiskMetadata_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.setDiskMetadata(settleTestCtx()), motionNode2)
	f.requireWrites(t, "config")
}

func TestHandleSetDiskMetadata_LostAnswerThatLandedIsNotResent(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(_, _ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), true
		}
		return nil, true
	}

	if err := f.setDiskMetadata(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode1)
}

func TestHandleSetDiskMetadata_LostAnswerThatDidNotLandIsResent(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(_, _ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), false
		}
		return nil, true
	}

	if err := f.setDiskMetadata(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "config", motionNode1, motionNode1)
	if !strings.Contains(f.description, "kept") {
		t.Fatalf("description %q does not hold the metadata", f.description)
	}
}

// ---------------------------------------------------------------------------
// detach_disk, attach_disk, delete_disk
// ---------------------------------------------------------------------------

func (f *diskMotionFixture) detach(ctx context.Context) error {
	_, err := handlers.HandleDetachDisk(f.deps).Handle(ctx, marshalArgs("101", motionDiskCIDRaw), jsonrpc.Context{})
	return err
}

func (f *diskMotionFixture) attach(ctx context.Context) error {
	_, err := handlers.HandleAttachDisk(f.deps).Handle(ctx, marshalArgs("101", motionOtherDiskCIDRaw), jsonrpc.Context{})
	return err
}

func (f *diskMotionFixture) deleteDisk(ctx context.Context) error {
	_, err := handlers.HandleDeleteDisk(f.deps).Handle(ctx, marshalArgs(motionDiskCIDRaw), jsonrpc.Context{})
	return err
}

func TestHandleDetachDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.detach(settleTestCtx()), motionNode2)
	f.requireWrites(t, "config")
}

func TestHandleDetachDisk_WaitsOutMigrationAndDetachesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if err := f.detach(fastRetryCtx(settleTestCtx())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, n := range f.writesTo("config") {
		if n != motionNode2 {
			t.Fatalf("config writes = %v, want every one on %s", f.writesTo("config"), motionNode2)
		}
	}
	if len(f.writesTo("config")) == 0 {
		t.Fatal("the detach wrote no config")
	}
}

func TestHandleDetachDisk_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("config")

	if err := f.detach(fastRetryCtx(settleTestCtx())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	writes := f.writesTo("config")
	if len(writes) < 2 || writes[0] != motionNode1 || writes[len(writes)-1] != motionNode2 {
		t.Fatalf("config writes = %v, want a refused send to %s and a landed one on %s", writes, motionNode1, motionNode2)
	}
}

func TestHandleAttachDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.attach(settleTestCtx()), motionNode2)
	f.requireWrites(t, "drive")
	f.requireWrites(t, "config")
}

func TestHandleDeleteDisk_HolderOnDeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.deleteDisk(settleTestCtx()), motionNode2)
	f.requireWrites(t, "config")
	f.requireWrites(t, "drive")
}

// ---------------------------------------------------------------------------
// HA mode and the method table
// ---------------------------------------------------------------------------

// TestDiskHandlersReadNoHAServiceStateWhenHAIsOff runs each handler of the
// fixture with pve.ha.mode off and checks that none of them reads a VM's HA
// state, and that a VM in no HA service is not deferred on an HA view.
func TestDiskHandlersReadNoHAServiceStateWhenHAIsOff(t *testing.T) {
	t.Parallel()
	for name, run := range diskMotionRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDiskMotionFixture(t, motionNode1)
			if name == "delete_snapshot" {
				f.snaps = []string{"snap1"}
			}
			f.deps.Config.HA = nil
			if err := run(f, fastRetryCtx(settleTestCtx())); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := f.haServiceReads.Load(); got != 0 {
				t.Fatalf("%s read the HA service list %d times with pve.ha.mode off", name, got)
			}
		})
	}
}

// diskMotionRuns are the handlers the fixture drives, by method name, each
// against a healthy VM.
func diskMotionRuns() map[string]func(f *diskMotionFixture, ctx context.Context) error {
	return map[string]func(f *diskMotionFixture, ctx context.Context) error{
		"resize_disk":       func(f *diskMotionFixture, ctx context.Context) error { return f.resize(ctx) },
		"snapshot_disk":     func(f *diskMotionFixture, ctx context.Context) error { _, err := f.snapshot(ctx); return err },
		"delete_snapshot":   func(f *diskMotionFixture, ctx context.Context) error { return f.deleteSnapshot(ctx) },
		"update_disk":       func(f *diskMotionFixture, ctx context.Context) error { return f.update(ctx) },
		"get_disks":         func(f *diskMotionFixture, ctx context.Context) error { _, err := f.getDisks(ctx); return err },
		"set_disk_metadata": func(f *diskMotionFixture, ctx context.Context) error { return f.setDiskMetadata(ctx) },
		"detach_disk":       func(f *diskMotionFixture, ctx context.Context) error { return f.detach(ctx) },
	}
}

// diskMethodsWithoutAMotionWait are the CPI methods that are not disk handlers
// acting on a VM's config, or that act on no VM at all, and the reason each
// needs no settle of a disk's holder. The table covers the same 22 methods as
// the dispatcher's, so a method added without a decision here fails the test.
var diskMethodsWithoutAMotionWait = map[string]string{
	"info":                          "no VM",
	"create_stemcell":               "no VM",
	"delete_stemcell":               "no VM",
	"create_vm":                     "VM lifecycle handler with its own settle",
	"delete_vm":                     "VM lifecycle handler with its own settle",
	"has_vm":                        "read of the VM index",
	"reboot_vm":                     "VM lifecycle handler with its own settle",
	"set_vm_metadata":               "VM lifecycle handler with its own settle",
	"calculate_vm_cloud_properties": "no VM",
	"create_disk":                   "allocates a volume on a placeable node and holds no VM",
	"has_disk":                      "read of storage",
	"create_network":                "no VM",
	"delete_network":                "no VM",
}

// TestEveryDiskMethodIsCoveredByAMotionTest ties the CPI method table to the
// motion tests: a method is either exercised against a VM that moves or sits on
// a dead node, or it is listed with the reason it needs neither.
func TestEveryDiskMethodIsCoveredByAMotionTest(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{"attach_disk": true, "delete_disk": true}
	for name := range diskMotionRuns() {
		covered[name] = true
	}
	methods := cpi.Methods()
	if len(methods) != 22 {
		t.Fatalf("Methods() returned %d entries, want 22; classify the new method in this test", len(methods))
	}
	for _, m := range methods {
		_, exempt := diskMethodsWithoutAMotionWait[m]
		switch {
		case covered[m] && exempt:
			t.Errorf("%s is listed as covered and as exempt", m)
		case !covered[m] && !exempt:
			t.Errorf("%s has no motion test and no exemption", m)
		}
	}
}

// ---------------------------------------------------------------------------
// the foreign holder refusal
// ---------------------------------------------------------------------------

// foreignHolderDeps are deps over the HA scene, with VM 101 as the holder.
func foreignHolderDeps(scene *handlers.HADeleteScene) handlers.Deps {
	scene.IndexTags = "bosh-cpi"
	cfg := testConfig()
	haMode := config.HAChoice(config.HAModeAuto)
	cfg.HA = &config.HAConfig{Mode: &haMode}
	return handlers.Deps{
		Config: cfg,
		Logger: log.NewNopLogger(),
		PVE: &handlers.HAScenePermsClient{Scene: scene, Client: &mockPVEClient{
			qemuSvc:    &handlers.HASceneQEMU{Service: &mockQEMUService{}, Scene: scene},
			nodesSvc:   &handlers.HASceneNodes{Service: &mockNodesService{}, Scene: scene},
			storageSvc: &mockStorageService{},
			clusterSvc: &handlers.HASceneCluster{Service: &mockClusterSvc{}, Scene: scene, UseIndex: true},
			poolsSvc:   &noopPoolService{},
		}},
	}
}

// TestForeignHolderRefusalNamesAHolderThatIsBeingRecovered checks the refusal an
// attach gives a disk another VM holds. The refusal stands in every case,
// because the volume is still referenced. It is a retriable error that names
// the holder's state when the holder is in fence or recovery or on a node that
// is down, so a retry finds the disk where the recovery leaves it, and it is a
// permanent error for a healthy holder.
func TestForeignHolderRefusalNamesAHolderThatIsBeingRecovered(t *testing.T) {
	t.Parallel()
	holder := pve.DiskHolder{Found: true, VMID: 101, Node: "pve-node1"}
	cases := []struct {
		name      string
		setup     func(s *handlers.HADeleteScene)
		retriable bool
		want      []string
	}{
		{name: "healthy holder", setup: func(s *handlers.HADeleteScene) { s.Place(101, "pve-node1") },
			want: []string{"already attached to VM 101", "Detach it from VM 101 first"}},
		{name: "holder in recovery", retriable: true,
			setup: func(s *handlers.HADeleteScene) { s.Manage(101, "pve-node1", "started", "recovery") },
			want:  []string{"being recovered or is down", "HA state is recovery"}},
		{name: "holder in fence", retriable: true,
			setup: func(s *handlers.HADeleteScene) { s.Manage(101, "pve-node1", "started", "fence") },
			want:  []string{"HA state is fence"}},
		{name: "holder on a dead node", retriable: true,
			setup: func(s *handlers.HADeleteScene) { s.Place(101, "pve-node1"); s.MarkNodeDead("pve-node1") },
			want:  []string{"node pve-node1 is not online"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scene := handlers.NewHADeleteScene()
			tc.setup(scene)
			err := handlers.ForeignHolderErrorForTest(settleTestCtx(), foreignHolderDeps(scene), "attach_disk",
				"local-lvm:vm-9001-disk-0", 202, holder)
			if err == nil {
				t.Fatal("the attach of a held disk must be refused")
			}
			var typed *cpierrors.Error
			if !errors.As(err, &typed) {
				t.Fatalf("want a CPI error, got %T %v", err, err)
			}
			if typed.OkToRetry() != tc.retriable {
				t.Fatalf("retriable = %v, want %v: %v", typed.OkToRetry(), tc.retriable, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not name %q", err, w)
				}
			}
		})
	}
}
