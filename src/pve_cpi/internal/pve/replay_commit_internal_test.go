// replay_commit_internal_test.go: white-box tests for the replay-after-commit
// tolerances in the mover primitives: createMoverVM adopting the VM its own
// committed-then-dropped create left behind (and refusing to adopt on a
// first-attempt conflict), and moveDiskToVM converting a replayed reassignment
// failure into success when the probe proves the move already landed.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// rpClient is a stateful single-node fake: VM configs live in configs, the
// cluster listing is derived from them (so NextVMID excludes committed VMs),
// and the storage scan is empty. Create and CreateQemuMoveDisk delegate to
// per-test closures so each test scripts its own commit-then-drop sequence.
type rpClient struct {
	parkerLockClient

	mu           sync.Mutex
	node         string
	configs      map[int]map[string]any
	deletedVMs   []int
	createFn     func(vmid int, params map[string]any) (string, error)
	moveDiskFn   func(srcVMID int, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error)
	waitFn       func(call int, upid string) (*sdktasks.Status, error)
	migrateFn    func(call int) (*sdknodes.CreateQemuMigrateResponse, error)
	createCalls  int
	moveCalls    int
	waitCalls    int
	migrateCalls int
}

// rpVMIDKey is the "vmid" request/response field name.
const rpVMIDKey = "vmid"

func newRPClient() *rpClient {
	return &rpClient{node: "pve1", configs: map[int]map[string]any{}}
}

func (c *rpClient) QEMU() qemu.Service      { return &rpQEMU{c: c} }
func (c *rpClient) Nodes() sdknodes.Service { return &rpNodes{c: c} }
func (c *rpClient) Cluster() sdkcluster.Service {
	return &rpCluster{c: c}
}
func (c *rpClient) Tasks() sdktasks.Service { return &rpTasks{c: c} }

type rpTasks struct {
	sdktasks.Service
	c *rpClient
}

func (t *rpTasks) Wait(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.c.waitCalls++
	if t.c.waitFn != nil {
		return t.c.waitFn(t.c.waitCalls, upid)
	}
	return &sdktasks.Status{ExitStatus: "OK"}, nil
}

func (t *rpTasks) GetStatus(_ context.Context, _, upid string) (*sdktasks.Status, error) {
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

type rpQEMU struct {
	qemu.Service
	c *rpClient
}

func (q *rpQEMU) Create(_ context.Context, _ string, params map[string]any) (string, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	q.c.createCalls++
	vmid, _ := params[rpVMIDKey].(int)
	return q.c.createFn(vmid, params)
}

func (q *rpQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg, ok := q.c.configs[vmid]
	if !ok || node != q.c.node {
		return nil, fmt.Errorf("rpClient: no config for vmid %d on node %s: %w", vmid, node, sdkerrors.ErrNotFound)
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

type rpNodes struct {
	sdknodes.Service
	c *rpClient
}

func (n *rpNodes) ListStorageContent(
	_ context.Context, _ string, _ string, _ *sdknodes.ListStorageContentParams,
) (*sdknodes.ListStorageContentResponse, error) {
	empty := sdknodes.ListStorageContentResponse{}
	return &empty, nil
}

// ListQemu derives the node's listing from the committed configs, mirroring
// ListResources, so the allocator's authoritative leg sees the same state.
func (n *rpNodes) ListQemu(
	_ context.Context, node string, _ *sdknodes.ListQemuParams,
) (*sdknodes.ListQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	entries := make([]json.RawMessage, 0, len(n.c.configs))
	if node == n.c.node {
		for vmid := range n.c.configs {
			raw, _ := json.Marshal(map[string]any{rpVMIDKey: vmid})
			entries = append(entries, raw)
		}
	}
	resp := sdknodes.ListQemuResponse(entries)
	return &resp, nil
}

func (n *rpNodes) CreateQemuMoveDisk(
	_ context.Context, _ string, vmidStr string, params *sdknodes.CreateQemuMoveDiskParams,
) (*sdknodes.CreateQemuMoveDiskResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	n.c.moveCalls++
	srcVMID, _ := strconv.Atoi(vmidStr)
	return n.c.moveDiskFn(srcVMID, params)
}

func (n *rpNodes) CreateQemuMigrate(
	_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuMigrateParams,
) (*sdknodes.CreateQemuMigrateResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	n.c.migrateCalls++
	if n.c.migrateFn != nil {
		return n.c.migrateFn(n.c.migrateCalls)
	}
	resp := sdknodes.CreateQemuMigrateResponse(`""`)
	return &resp, nil
}

func (n *rpNodes) UpdateQemuConfig(
	_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams,
) error {
	return nil
}

func (n *rpNodes) DeleteQemu(
	_ context.Context, _ string, vmidStr string, _ *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	delete(n.c.configs, vmid)
	n.c.deletedVMs = append(n.c.deletedVMs, vmid)
	resp := json.RawMessage(`""`)
	return &resp, nil
}

type rpCluster struct {
	sdkcluster.Service
	c *rpClient
}

func (cl *rpCluster) ListResources(
	_ context.Context, _ *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	cl.c.mu.Lock()
	defer cl.c.mu.Unlock()
	entries := make([]json.RawMessage, 0, len(cl.c.configs))
	for vmid := range cl.c.configs {
		raw, _ := json.Marshal(map[string]any{rpVMIDKey: vmid, "node": cl.c.node, "type": "qemu"})
		entries = append(entries, raw)
	}
	resp := sdkcluster.ListResourcesResponse(entries)
	return &resp, nil
}

// ListConfigNodes reports the fake's single node as the cluster membership.
func (cl *rpCluster) ListConfigNodes(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	raw, _ := json.Marshal(map[string]any{"name": cl.c.node})
	resp := sdkcluster.ListConfigNodesResponse{raw}
	return &resp, nil
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (cl *rpCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// rpCommitVM registers vmid in the fake's state the way PVE's side of a
// committed create would: config carries the tags and description (the
// per-attempt marker) the create sent.
func rpCommitVM(c *rpClient, vmid int, params map[string]any) {
	cfg := map[string]any{"name": parkerVMName(vmid)}
	if tags, ok := params[cfgKeyTags].(string); ok {
		cfg[cfgKeyTags] = tags
	}
	if desc, ok := params["description"].(string); ok {
		cfg["description"] = desc
	}
	c.configs[vmid] = cfg
}

func rpCtx() context.Context {
	return WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestCreateMoverVM_AdoptsOwnCommittedCreateAfterDrop scripts the replay:
// attempt 1 commits the mover server-side but its response is dropped
// mid-flight; the in-loop retry then conflicts against our own VM. The
// conflict must resolve by adopting the committed mover, not by regenerating
// a fresh VMID (which would leak a protection=1 orphan) and never by
// destroying anything.
func TestCreateMoverVM_AdoptsOwnCommittedCreateAfterDrop(t *testing.T) {
	t.Parallel()
	c := newRPClient()
	var committedVMID int
	c.createFn = func(vmid int, params map[string]any) (string, error) {
		if c.createCalls == 1 {
			committedVMID = vmid
			rpCommitVM(c, vmid, params)
			return "", fmt.Errorf("Post \"/nodes/pve1/qemu\": %w", io.EOF)
		}
		return "", fmt.Errorf("500 unable to create VM: VM %d already exists on node 'pve1'", vmid)
	}

	vmid, err := createMoverVM(rpCtx(), c, log.NewNopLogger(), "pve1", dmBand())
	if err != nil {
		t.Fatalf("expected the dropped-then-conflicting create to adopt the committed mover, got: %v", err)
	}
	if vmid != committedVMID {
		t.Errorf("adopted VMID: want the committed mover %d, got %d", committedVMID, vmid)
	}
	if c.createCalls != 2 {
		t.Errorf("Create submits: want 2 (dropped attempt + replay), got %d", c.createCalls)
	}
	if len(c.configs) != 1 {
		t.Errorf("exactly one mover must exist after adoption, got %d", len(c.configs))
	}
	if len(c.deletedVMs) != 0 {
		t.Errorf("adoption must never destroy anything, got destroys of %v", c.deletedVMs)
	}
}

// TestCreateMoverVM_FinalAttemptDropAdoptsCommittedMover covers the drop
// landing on the FINAL retry attempt: the create commits server-side but
// every response is dropped, so the retry budget exhausts with the drop
// itself as the error (never a VMID conflict). The commit question is the
// same, and the probe must adopt the committed mover instead of leaving it
// behind as a protection=1 orphan with no reclaim path.
func TestCreateMoverVM_FinalAttemptDropAdoptsCommittedMover(t *testing.T) {
	t.Parallel()
	c := newRPClient()
	var committedVMID int
	c.createFn = func(vmid int, params map[string]any) (string, error) {
		if c.createCalls == 1 {
			committedVMID = vmid
			rpCommitVM(c, vmid, params)
		}
		return "", fmt.Errorf("Post \"/nodes/pve1/qemu\": %w", io.EOF)
	}

	vmid, err := createMoverVM(rpCtx(), c, log.NewNopLogger(), "pve1", dmBand())
	if err != nil {
		t.Fatalf("expected the exhausted-drops create to adopt the committed mover, got: %v", err)
	}
	if vmid != committedVMID {
		t.Errorf("adopted VMID: want the committed mover %d, got %d", committedVMID, vmid)
	}
	if len(c.configs) != 1 {
		t.Errorf("exactly one mover must exist after adoption, got %d", len(c.configs))
	}
	if len(c.deletedVMs) != 0 {
		t.Errorf("adoption must never destroy anything, got destroys of %v", c.deletedVMs)
	}
}

// TestCreateMoverVM_FirstAttemptConflictRegenerates covers the guard on the
// adoption: a conflict with NO dropped attempt observed means another CPI won
// the VMID race. Even when the winner's config looks exactly like an empty
// mover, the loser must regenerate a fresh VMID and leave the winner alone.
func TestCreateMoverVM_FirstAttemptConflictRegenerates(t *testing.T) {
	t.Parallel()
	c := newRPClient()
	var conflictVMID int
	c.createFn = func(vmid int, params map[string]any) (string, error) {
		if c.createCalls == 1 {
			// The other CPI's mover materializes at this VMID: adoptable in
			// shape (both tags, no disks), but not ours (no drop happened).
			conflictVMID = vmid
			rpCommitVM(c, vmid, params)
			return "", fmt.Errorf("500 unable to create VM: VM %d already exists on node 'pve1'", vmid)
		}
		rpCommitVM(c, vmid, params)
		return "", nil
	}

	vmid, err := createMoverVM(rpCtx(), c, log.NewNopLogger(), "pve1", dmBand())
	if err != nil {
		t.Fatalf("expected regeneration to succeed, got: %v", err)
	}
	if vmid == conflictVMID {
		t.Errorf("a first-attempt conflict must regenerate, not adopt VMID %d", conflictVMID)
	}
	if _, winnerIntact := c.configs[conflictVMID]; !winnerIntact {
		t.Error("the conflicting winner's VM must be left untouched")
	}
	if len(c.deletedVMs) != 0 {
		t.Errorf("nothing may be destroyed on a first-attempt conflict, got destroys of %v", c.deletedVMs)
	}
}

// TestCreateMoverVM_DroppedThenForeignWinnerRegenerates covers the marker
// half of the adoption gate: our attempt drops, but a concurrent CPI wins the
// VMID before our replay. The winner's mover is identically shaped (parker
// and mover tags, empty), so only the per-attempt description marker can tell
// it apart, and the conflict must regenerate without touching the winner.
func TestCreateMoverVM_DroppedThenForeignWinnerRegenerates(t *testing.T) {
	t.Parallel()
	c := newRPClient()
	var conflictVMID int
	c.createFn = func(vmid int, params map[string]any) (string, error) {
		if c.createCalls == 1 {
			// Our POST drops; the peer's create lands at this VMID first,
			// carrying the peer's own attempt marker.
			conflictVMID = vmid
			rpCommitVM(c, vmid, params)
			c.configs[vmid]["description"] = moverAttemptDescPrefix + "bpd-feedbeeffeedbeef"
			return "", fmt.Errorf("Post \"/nodes/pve1/qemu\": %w", io.EOF)
		}
		if vmid == conflictVMID {
			return "", fmt.Errorf("500 unable to create VM: VM %d already exists on node 'pve1'", vmid)
		}
		rpCommitVM(c, vmid, params)
		return "", nil
	}

	vmid, err := createMoverVM(rpCtx(), c, log.NewNopLogger(), "pve1", dmBand())
	if err != nil {
		t.Fatalf("expected regeneration to succeed, got: %v", err)
	}
	if vmid == conflictVMID {
		t.Errorf("a conflicting VM without this call's attempt marker must not be adopted (VMID %d)", conflictVMID)
	}
	if desc, _ := c.configs[conflictVMID]["description"].(string); desc != moverAttemptDescPrefix+"bpd-feedbeeffeedbeef" {
		t.Error("the foreign winner's VM must be left untouched")
	}
	if len(c.deletedVMs) != 0 {
		t.Errorf("nothing may be destroyed, got destroys of %v", c.deletedVMs)
	}
}

// TestMoveDiskToVM_TaskBodyFailureStaysPermanent pins the classification
// split: a move task that fails with a verdict (task body error) must NOT be
// reported ok_to_retry=true to the Director. The await moved inside the
// retry loop; its errors must still route through the non-retriable text
// fallback, not the mutation wrapper's retriable default.
func TestMoveDiskToVM_TaskBodyFailureStaysPermanent(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-152-disk-0"
	c := newRPClient()
	c.configs[152] = map[string]any{"scsi1": volid}
	c.configs[90003] = map[string]any{cfgKeyTags: CpiOwnershipTag + ";" + ParkerTag}
	c.moveDiskFn = func(_ int, _ *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
		resp := json.RawMessage(`"UPID:pve1:0000C0DE:00112233:66aabbcc:qmmove:152:root@pam:"`)
		return &resp, nil
	}
	c.waitFn = func(_ int, _ string) (*sdktasks.Status, error) {
		// The SDK's own resolved-task failure shape, as Tasks().Wait returns it.
		return nil, fmt.Errorf("task failed: %s", "storage does not support this operation")
	}

	err := moveDiskToVM(rpCtx(), c, log.NewNopLogger(), "pve1", 152, "scsi1", 90003, "scsi2")
	if err == nil {
		t.Fatal("a failed move task must surface an error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.OkToRetry() {
		t.Errorf("a task-body verdict must stay non-retriable, got ok_to_retry=true: %v", err)
	}
	if !strings.Contains(err.Error(), "move_disk task for") {
		t.Errorf("await failures must keep the task-shaped message, got: %v", err)
	}
}

// TestMoveDiskToVM_ReplayLandedConvertsToSuccess scripts a move_disk whose
// first POST commits the reassignment but drops the response; the replay then
// fails ("no such disk": the source slot is already empty). The two-sided
// landed probe (target slot occupied AND source slot empty) must convert that
// failure into success.
func TestMoveDiskToVM_ReplayLandedConvertsToSuccess(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-150-disk-0"
	c := newRPClient()
	c.configs[150] = map[string]any{"scsi1": volid + ",serial=bpd-cafe"}
	c.configs[90001] = map[string]any{cfgKeyTags: CpiOwnershipTag + ";" + ParkerTag}
	c.moveDiskFn = func(srcVMID int, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
		if c.moveCalls == 1 {
			// Commit: the drive moves to the target slot, then the response drops.
			c.configs[90001][*params.TargetDisk] = c.configs[150][params.Disk]
			delete(c.configs[150], params.Disk)
			return nil, fmt.Errorf("Post \"/nodes/pve1/qemu/%d/move_disk\": %w", srcVMID, io.EOF)
		}
		return nil, fmt.Errorf("500 disk '%s' does not exist", params.Disk)
	}

	err := moveDiskToVM(rpCtx(), c, log.NewNopLogger(), "pve1", 150, "scsi1", 90001, "scsi2")
	if err != nil {
		t.Fatalf("landed replay must be success, got: %v", err)
	}
	if c.moveCalls != 2 {
		t.Errorf("move submits: want 2 (dropped attempt + replay), got %d", c.moveCalls)
	}
	if _, held := slotBareVolid(c.configs[90001], "scsi2"); !held {
		t.Error("target slot must hold the moved volume")
	}
	if _, held := slotBareVolid(c.configs[150], "scsi1"); held {
		t.Error("source slot must be empty after the landed move")
	}
}

// TestMoveDiskToVM_ReplayNotLandedSurfacesError is the control: the first
// POST drops WITHOUT committing, and the replay's failure must surface:
// the probe sees the source still holding the disk, so nothing may be
// papered over.
func TestMoveDiskToVM_ReplayNotLandedSurfacesError(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-151-disk-0"
	c := newRPClient()
	c.configs[151] = map[string]any{"scsi1": volid}
	c.configs[90002] = map[string]any{cfgKeyTags: CpiOwnershipTag + ";" + ParkerTag}
	permanent := errors.New("500 storage 'data' does not support content-type 'images'")
	c.moveDiskFn = func(srcVMID int, _ *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
		if c.moveCalls == 1 {
			return nil, fmt.Errorf("Post \"/nodes/pve1/qemu/%d/move_disk\": %w", srcVMID, io.EOF)
		}
		return nil, permanent
	}

	err := moveDiskToVM(rpCtx(), c, log.NewNopLogger(), "pve1", 151, "scsi1", 90002, "scsi2")
	if err == nil {
		t.Fatal("an uncommitted move's replay failure must surface, got success")
	}
	if !errors.Is(err, permanent) && !containsErrText(err, "does not support content-type") {
		t.Errorf("surfaced error must be the replay's own failure, got: %v", err)
	}
	if _, held := slotBareVolid(c.configs[151], "scsi1"); !held {
		t.Error("source slot must still hold the volume")
	}
}

func containsErrText(err error, substr string) bool {
	return err != nil && substr != "" && strings.Contains(err.Error(), substr)
}
