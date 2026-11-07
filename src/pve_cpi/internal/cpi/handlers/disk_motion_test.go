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
	"time"

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

	// missingIs404 makes a node that holds no config answer 404 instead of the
	// 500 that PVE gives, which is how a node record that has gone looks.
	missingIs404 bool
	// haState is the hastate the index row carries.
	haState string

	// deleted records the volumes the storage service was asked to delete.
	deleted []string

	// haServiceReads counts reads of any HA source.
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
	storageSvc := &mockStorageService{
		deleteVolumeFn: func(_ context.Context, _, _, volume string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deleted = append(f.deleted, volume)
			return nil
		},
		deleteVolumeIfExistsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.deleted = append(f.deleted, volume)
			return true, nil
		},
	}
	f.deps = testDepsFoundVMWithStorage(101, svc, nodesSvc, nil, &mockAgentService{}, storageSvc)
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
			if f.haState != "" {
				row["hastate"] = f.haState
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
	// The disk lives on shared storage, so a VM that moved still reaches it.
	mc.clusterStorageSvc = &mockClusterStorage{storageName: "local-lvm", storageType: "rbd", shared: true}
	return f
}

// withHAMode sets pve.ha.mode for the fixture's deps.
func (f *diskMotionFixture) withHAMode(mode string) {
	choice := config.HAChoice(mode)
	f.deps.Config.HA = &config.HAConfig{Mode: &choice}
}

// haReadCountingCluster counts every read of an HA source: the HA service list,
// the HA status rows, and the CRM's manager view. With pve.ha.mode off none may
// happen.
type haReadCountingCluster struct {
	*mockClusterSvc
	reads *atomic.Int32
}

// haSceneReadCounter counts the same reads in front of any cluster fake.
type haSceneReadCounter struct {
	cluster.Service
	reads *atomic.Int32
}

func (c *haSceneReadCounter) ListHaStatusCurrent(ctx context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	c.reads.Add(1)
	return c.Service.ListHaStatusCurrent(ctx)
}

func (c *haSceneReadCounter) ListHaStatusManagerStatus(
	ctx context.Context,
) (*cluster.ListHaStatusManagerStatusResponse, error) {
	c.reads.Add(1)
	return c.Service.ListHaStatusManagerStatus(ctx)
}

func (c *haSceneReadCounter) ListHaResources(
	ctx context.Context, params *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	c.reads.Add(1)
	return c.Service.ListHaResources(ctx, params)
}

// ListHaStatusManagerStatus counts the read of the CRM's manager view.
func (c *haReadCountingCluster) ListHaStatusManagerStatus(
	ctx context.Context,
) (*cluster.ListHaStatusManagerStatusResponse, error) {
	c.reads.Add(1)
	return handlers.AbsenceIdleCRM(), nil
}

func (c *haReadCountingCluster) ListHaStatusCurrent(
	ctx context.Context,
) (*cluster.ListHaStatusCurrentResponse, error) {
	c.reads.Add(1)
	return c.mockClusterSvc.ListHaStatusCurrent(ctx)
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
	if f.missingIs404 {
		return notFoundAPIErr()
	}
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

// TestHandleResizeDisk_MoveAfterTheWaitIsFollowedWithHAOffAndAFencedRow follows
// a VM that moved after the first wait while the row shows an HA state of fence.
// With pve.ha.mode off the settle inside the mutation ignores that state too, so
// the resize lands on the new node.
func TestHandleResizeDisk_MoveAfterTheWaitIsFollowedWithHAOffAndAFencedRow(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.haState = "fence"
	f.beforeWrite = moveOnFirst("resize")

	if err := f.resize(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "resize", motionNode1, motionNode2)
	if got := f.haServiceReads.Load(); got != 0 {
		t.Fatalf("read an HA source %d times with pve.ha.mode off", got)
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

// TestHandleSetDiskMetadata_NotFoundOnTheWriteWithTheVMStillListedIsRetriable
// answers 404 to every metadata write while the VM is present. The 404 speaks
// for the node that gave it, so the call is a retriable error, never a VM that
// is gone.
func TestHandleSetDiskMetadata_NotFoundOnTheWriteWithTheVMStillListedIsRetriable(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(string, string, int) (error, bool) { return notFoundAPIErr(), false }

	err := f.setDiskMetadata(settleTestCtx())
	requireHARetriable(t, err)
	requireNotGone(t, err)
}

// TestHandleSetDiskMetadata_NotFoundOnTheWriteWithTheVMGoneIsVMNotFound makes
// the VM vanish as the first write arrives, so every member answers 404 and the
// absence proof finds it nowhere.
func TestHandleSetDiskMetadata_NotFoundOnTheWriteWithTheVMGoneIsVMNotFound(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.missingIs404 = true
	f.beforeWrite = func(f *diskMotionFixture, _, _ string, _ int) { f.configNode = "nowhere" }

	err := f.setDiskMetadata(settleTestCtx())
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeVMNotFound {
		t.Fatalf("want VMNotFound once every member agrees the VM is gone, got %v", err)
	}
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

// TestHandleDetachDisk_FailingClusterScanFailsClosedWithinTheDiskBudget keeps
// the cluster scan failing from the start. The detach has written nothing, so
// it gives up with a retriable error after the disk handlers' scan budget, not
// after the minute a lock earns, and it sends no write.
func TestHandleDetachDisk_FailingClusterScanFailsClosedWithinTheDiskBudget(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	mc := f.deps.PVE.(*mockPVEClient)
	inner := mc.clusterSvc.(*haReadCountingCluster).mockClusterSvc
	inner.listResourcesFn = func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return nil, errors.New("list resources: connection refused")
	}
	var mu sync.Mutex
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	ctx := pve.WithVMSettleClockForTest(context.Background(),
		func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return ctx.Err()
		})

	err := f.detach(ctx)
	requireHARetriable(t, err, "connection refused")
	mu.Lock()
	waited := now.Sub(start)
	mu.Unlock()
	if waited > 15*time.Second {
		t.Fatalf("the failing scan held the detach for %v, want at most 15s", waited)
	}
	f.requireWrites(t, "config")
}

func TestHandleAttachDisk_DeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.attach(settleTestCtx()), motionNode2)
	f.requireWrites(t, "drive")
	f.requireWrites(t, "config")
}

func TestHandleAttachDisk_WaitsOutMigrationAndAttachesOnNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()

	if err := f.attach(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "drive", motionNode2)
}

func TestHandleAttachDisk_MoveAfterTheWaitIsFollowed(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.beforeWrite = moveOnFirst("drive")

	if err := f.attach(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "drive", motionNode1, motionNode2)
}

func TestHandleAttachDisk_LostAnswerThatLandedIsNotResent(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.writeFn = func(_, _ string, call int) (error, bool) {
		if call == 1 {
			return fmt.Errorf("read tcp 10.0.0.1:44312->10.0.0.2:8006: %w", syscall.ECONNRESET), true
		}
		return nil, true
	}

	if err := f.attach(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.requireWrites(t, "drive", motionNode1)
}

func TestHandleDeleteDisk_HolderOnDeadNodeDefersWithoutWrite(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	f.offline[motionNode2] = true

	requireHARetriable(t, f.deleteDisk(settleTestCtx()), motionNode2)
	f.requireWrites(t, "config")
	f.requireWrites(t, "drive")
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v from storage, want nothing while the holder cannot answer", f.deleted)
	}
}

// holderMigratesAfterTheScan keeps the holder locked for migration until the
// settle has polled the index a few times past the holder scan, then lands it
// on node2. The holder scan reads the index three times first.
func (f *diskMotionFixture) holderMigratesAfterTheScan() {
	f.lock = "migrate"
	f.onRead = func(f *diskMotionFixture, reads int) {
		if reads >= 6 {
			f.node, f.lock = motionNode2, ""
		}
	}
}

func TestHandleDeleteDisk_WaitsOutTheHolderMigrationBeforeDeleting(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.holderMigratesAfterTheScan()

	if err := f.deleteDisk(settleTestCtx()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.lock != "" || f.node != motionNode2 {
		t.Fatalf("the delete returned while the holder was still locked on %s (lock %q)", f.node, f.lock)
	}
	if f.reads < 6 {
		t.Fatalf("index reads = %d, want the settle to have polled until the migration ended", f.reads)
	}
}

func TestHandleDeleteDisk_HolderThatKeepsMigratingDefersWithoutDeleting(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.lock = "migrate"

	err := f.deleteDisk(settleTestCtx())
	requireHARetriable(t, err, "migrate")
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v from storage while the holder was mid-migration", f.deleted)
	}
}

func TestHandleDeleteDisk_HolderBeingRecoveredDefersWithoutDeleting(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.haState = "recovery"
	f.withHAMode(config.HAModeAuto)

	err := f.deleteDisk(settleTestCtx())
	requireHARetriable(t, err, "recovery")
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v from storage while the holder was in recovery", f.deleted)
	}
}

// ---------------------------------------------------------------------------
// the interrupted transfer's identities
// ---------------------------------------------------------------------------

// TestSettleTransferIdentities_ParkerThatMovedResumesOnItsNewNode records the
// parker on node1 while the index shows it on node2. The resume must use node2,
// and the journal's record stays as it was written.
func TestSettleTransferIdentities_ParkerThatMovedResumesOnItsNewNode(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode2)
	intent := &pve.DiskTransferIntent{ParkerVMID: 101, ParkerNode: motionNode1}

	node, err := handlers.SettleTransferIdentitiesForTest(settleTestCtx(), f.deps, "attach_disk", intent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != motionNode2 {
		t.Fatalf("parker node = %q, want %q", node, motionNode2)
	}
	if intent.ParkerNode != motionNode1 {
		t.Fatalf("the recorded intent changed to %q; the settle works on a copy", intent.ParkerNode)
	}
}

func TestSettleTransferIdentities_ParkerMigratingIsWaitedOut(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	f.migratesDuringTheWait()
	intent := &pve.DiskTransferIntent{ParkerVMID: 101, ParkerNode: motionNode1}

	node, err := handlers.SettleTransferIdentitiesForTest(settleTestCtx(), f.deps, "attach_disk", intent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != motionNode2 {
		t.Fatalf("parker node = %q, want %q after the migration", node, motionNode2)
	}
}

// TestSettleTransferIdentities_SourceAndParkerOnDifferentNodesIsRetriable
// refuses the resume when PVE could not move the disk between the two VMs.
func TestSettleTransferIdentities_SourceAndParkerOnDifferentNodesIsRetriable(t *testing.T) {
	t.Parallel()
	f := newDiskMotionFixture(t, motionNode1)
	mc := f.deps.PVE.(*mockPVEClient)
	inner := mc.clusterSvc.(*haReadCountingCluster).mockClusterSvc
	inner.listResourcesFn = func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return &cluster.ListResourcesResponse{
			json.RawMessage(`{"vmid":101,"node":"pve-node1","type":"qemu","tags":"bosh-cpi"}`),
			json.RawMessage(`{"vmid":9100,"node":"pve-node2","type":"qemu","tags":"bosh-cpi"}`),
		}, nil
	}
	intent := &pve.DiskTransferIntent{ParkerVMID: 9100, ParkerNode: motionNode2, SourceVMCID: "101"}

	_, err := handlers.SettleTransferIdentitiesForTest(settleTestCtx(), f.deps, "detach_disk", intent)
	requireHARetriable(t, err, motionNode1, motionNode2, "one node")
}

// ---------------------------------------------------------------------------
// HA mode and the method table
// ---------------------------------------------------------------------------

// TestDiskHandlersReadNoHAServiceStateWhenHAIsOff runs every disk handler of the
// fixture with pve.ha.mode off against a VM whose index row says it is in
// fence. None of them may read any HA source (the service list, the status rows,
// or the CRM's manager view), and none may defer on the row's HA state.
func TestDiskHandlersReadNoHAServiceStateWhenHAIsOff(t *testing.T) {
	t.Parallel()
	for name, run := range diskMotionRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDiskMotionFixture(t, motionNode1)
			if name == "delete_snapshot" {
				f.snaps = []string{"snap1"}
			}
			f.haState = "fence"
			f.withHAMode(config.HAModeOff)
			if err := run(f, fastRetryCtx(settleTestCtx())); err != nil {
				t.Fatalf("a VM in HA state fence must not defer the call when pve.ha.mode is off: %v", err)
			}
			if got := f.haServiceReads.Load(); got != 0 {
				t.Fatalf("%s read an HA source %d times with pve.ha.mode off", name, got)
			}
		})
	}
}

// TestDiskHandlersDeferOnAnHAStateWhenHAIsOn is the control for the test above.
// With pve.ha.mode auto the same row defers every handler that waits on the VM,
// so the off-mode test passes because the mode is off and not because the
// fixture never shows an HA state.
func TestDiskHandlersDeferOnAnHAStateWhenHAIsOn(t *testing.T) {
	t.Parallel()
	for name, run := range diskMotionRuns() {
		if name == "get_disks" {
			// get_disks reads the VM's config and defers on a node that cannot
			// answer, not on the HA state of the VM.
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDiskMotionFixture(t, motionNode1)
			if name == "delete_snapshot" {
				f.snaps = []string{"snap1"}
			}
			f.haState = "fence"
			f.withHAMode(config.HAModeAuto)
			requireHARetriable(t, run(f, fastRetryCtx(settleTestCtx())), "fence")
		})
	}
}

// TestForeignHolderRefusalReadsNoHAStateWhenHAIsOff gives the refusal a holder
// in recovery with pve.ha.mode off. The refusal is the plain permanent one, and
// no HA source is read. A holder on a node that cluster status lists offline is
// still named.
func TestForeignHolderRefusalReadsNoHAStateWhenHAIsOff(t *testing.T) {
	t.Parallel()
	holder := pve.DiskHolder{Found: true, VMID: 101, Node: "pve-node1"}
	cases := []struct {
		name      string
		setup     func(s *handlers.HADeleteScene)
		retriable bool
		want      string
	}{
		{name: "holder in recovery",
			setup: func(s *handlers.HADeleteScene) { s.Manage(101, "pve-node1", "started", "recovery") },
			want:  "already attached to VM 101"},
		{name: "holder on a dead node", retriable: true,
			setup: func(s *handlers.HADeleteScene) { s.Place(101, "pve-node1"); s.MarkNodeDead("pve-node1") },
			want:  "node pve-node1 is not online"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scene := handlers.NewHADeleteScene()
			tc.setup(scene)
			deps := foreignHolderDeps(scene)
			choice := config.HAChoice(config.HAModeOff)
			deps.Config.HA = &config.HAConfig{Mode: &choice}
			var reads atomic.Int32
			client := deps.PVE.(*handlers.HAScenePermsClient).Client.(*mockPVEClient)
			client.clusterSvc = &haSceneReadCounter{Service: client.clusterSvc, reads: &reads}

			err := handlers.ForeignHolderErrorForTest(settleTestCtx(), deps, "attach_disk",
				"local-lvm:vm-9001-disk-0", 202, holder)
			var typed *cpierrors.Error
			if !errors.As(err, &typed) {
				t.Fatalf("want a CPI error, got %T %v", err, err)
			}
			if typed.OkToRetry() != tc.retriable || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("retriable = %v, error %q; want retriable %v naming %q", typed.OkToRetry(), err, tc.retriable, tc.want)
			}
			if strings.Contains(err.Error(), "HA state") {
				t.Fatalf("the refusal names an HA state with pve.ha.mode off: %v", err)
			}
			if got := reads.Load(); got != 0 {
				t.Fatalf("the refusal read an HA source %d times with pve.ha.mode off", got)
			}
		})
	}
}

// diskMotionRuns are the disk handlers the fixture drives, by method name, each
// against a healthy VM.
func diskMotionRuns() map[string]func(f *diskMotionFixture, ctx context.Context) error {
	return map[string]func(f *diskMotionFixture, ctx context.Context) error{
		"attach_disk":       func(f *diskMotionFixture, ctx context.Context) error { return f.attach(ctx) },
		"delete_disk":       func(f *diskMotionFixture, ctx context.Context) error { return f.deleteDisk(ctx) },
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
// needs no settle of a disk's holder. A method that cpi.Methods() registers
// must appear here or in diskMotionRuns, and the test names the one that does
// not.
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
// fixture. A method is either exempt with a reason, or it is run here against a
// VM on a node that cannot answer, and the run must come back a retriable error
// that names the node, with nothing written to the VM and nothing deleted from
// storage. A method in neither table, or in both, or a table entry for a method
// the dispatcher no longer registers, fails by name.
func TestEveryDiskMethodIsCoveredByAMotionTest(t *testing.T) {
	t.Parallel()
	runs := diskMotionRuns()
	registered := map[string]bool{}
	for _, m := range cpi.Methods() {
		registered[m] = true
		_, exempt := diskMethodsWithoutAMotionWait[m]
		_, run := runs[m]
		switch {
		case run && exempt:
			t.Errorf("%s is run by a motion test and listed as exempt", m)
		case !run && !exempt:
			t.Errorf("%s has no motion test and no exemption", m)
		}
	}
	for name := range runs {
		if !registered[name] {
			t.Errorf("%s has a motion test but cpi.Methods() does not register it", name)
		}
	}
	for name := range diskMethodsWithoutAMotionWait {
		if !registered[name] {
			t.Errorf("%s is listed as exempt but cpi.Methods() does not register it", name)
		}
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDiskMotionFixture(t, motionNode2)
			if name == "delete_snapshot" {
				f.snaps = []string{"snap1"}
			}
			f.offline[motionNode2] = true
			requireHARetriable(t, run(f, settleTestCtx()), motionNode2)
			for _, op := range []string{"config", "drive", "resize", "snapshot", "delsnap"} {
				f.requireWrites(t, op)
			}
			if len(f.deleted) != 0 {
				t.Fatalf("%s deleted %v from storage on a node that cannot answer", name, f.deleted)
			}
		})
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
