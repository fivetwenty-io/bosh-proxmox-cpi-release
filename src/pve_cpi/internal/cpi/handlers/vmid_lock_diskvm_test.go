package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkclusterapi "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ==========================================================================
// Shared helpers for disk-metadata and delete-vm lock tests
// ==========================================================================

// lockTestPVEClient is a pve.Client for tests that need recording pool
// service alongside specific QEMU / Nodes / Cluster services.
type lockTestPVEClient struct {
	qemuSvc    sdkqemu.Service
	nodesSvc   sdknodes.Service
	clusterSvc sdkclusterapi.Service
	poolsSvc   pve.PoolService
}

var _ pve.Client = (*lockTestPVEClient)(nil)

func (c *lockTestPVEClient) QEMU() sdkqemu.Service                     { return c.qemuSvc }
func (c *lockTestPVEClient) Nodes() sdknodes.Service                   { return c.nodesSvc }
func (c *lockTestPVEClient) Cluster() sdkclusterapi.Service            { return c.clusterSvc }
func (c *lockTestPVEClient) Pools() pve.PoolService                    { return c.poolsSvc }
func (c *lockTestPVEClient) Storage() sdkstorage.Service               { return nil }
func (c *lockTestPVEClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *lockTestPVEClient) Tasks() sdktasks.Service                   { return nil }
func (c *lockTestPVEClient) ClusterStorage() sdkclusterstorage.Service { return nil }

// ldmQEMU is a minimal qemu.Service for lock tests; records Config calls.
type ldmQEMU struct {
	sdkqemu.Service
	diskCID  string   // returned as scsi0 in Config
	events   *[]string
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error)
	stopFn   func(ctx context.Context, node string, vmid int) (string, error)
}

func (q *ldmQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.events != nil {
		*q.events = append(*q.events, "qemu-config")
	}
	if q.configFn != nil {
		return q.configFn(ctx, node, vmid)
	}
	out := map[string]any{"tags": "existing-tag"}
	if q.diskCID != "" {
		out["scsi0"] = q.diskCID + ",size=10G"
	}
	return out, nil
}

func (q *ldmQEMU) Stop(ctx context.Context, node string, vmid int) (string, error) {
	if q.stopFn != nil {
		return q.stopFn(ctx, node, vmid)
	}
	return "upid-stop", nil
}

// ldmNodes is a minimal nodes.Service for lock tests; records UpdateQemuConfig calls.
type ldmNodes struct {
	sdknodes.Service
	events   *[]string
	updateFn func(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	deleteFn func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
}

func (n *ldmNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if n.events != nil {
		*n.events = append(*n.events, "update-config")
	}
	if n.updateFn != nil {
		return n.updateFn(ctx, node, vmid, params)
	}
	return nil
}

func (n *ldmNodes) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	if n.deleteFn != nil {
		return n.deleteFn(ctx, node, vmid, params)
	}
	return &sdknodes.DeleteQemuResponse{}, nil
}

// ldmCluster is a minimal cluster.Service for lock tests.
type ldmCluster struct {
	sdkclusterapi.Service
	resp *sdkclusterapi.ListResourcesResponse
}

func (c *ldmCluster) ListResources(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
	if c.resp == nil {
		return &sdkclusterapi.ListResourcesResponse{}, nil
	}
	return c.resp, nil
}

// ldmClusterResources builds a one-VM ListResourcesResponse.
//
//nolint:unparam // node param is kept for call-site clarity; always lockTestNode in this file
func ldmClusterResources(vmid int64, node string) *sdkclusterapi.ListResourcesResponse {
	type entry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Type string `json:"type"`
	}
	raw, _ := json.Marshal(entry{VMID: vmid, Node: node, Type: "qemu"})
	return &sdkclusterapi.ListResourcesResponse{raw}
}

const lockTestNode = "pve-node1"

// ==========================================================================
// set_disk_metadata lock tests
// ==========================================================================

// TestHandleSetDiskMetadata_LockAcquiredBeforeRead verifies the per-VMID cluster
// lock is acquired before the QEMU.Config read and released after UpdateQemuConfig.
func TestHandleSetDiskMetadata_LockAcquiredBeforeRead(t *testing.T) {
	t.Parallel()

	const diskCID = "local-lvm:vm-100-disk-0"
	const vmid = int64(100)

	events := []string{}
	pools := newRecordingPoolService(&events)
	expectedPool := fmt.Sprintf("bosh-lock-vm-%d", vmid)

	deps := handlers.Deps{
		Config: testConfig(),
		Logger: log.NewNopLogger(),
		Agent:  &mockAgentService{},
		PVE: &lockTestPVEClient{
			qemuSvc:    &ldmQEMU{diskCID: diskCID, events: &events},
			nodesSvc:   &ldmNodes{events: &events},
			clusterSvc: &ldmCluster{resp: ldmClusterResources(vmid, lockTestNode)},
			poolsSvc:   pools,
		},
	}

	h := handlers.HandleSetDiskMetadata(deps)
	_, err := h.Handle(context.Background(),
		marshalArgs(diskCID, map[string]any{"instance_id": "vm-abc"}),
		jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The handler makes two separate Config calls:
	//   1. During findVMsHostingDisk (the scan phase) — before the lock.
	//   2. Inside persistMetadata (the RMW phase) — must be inside the lock.
	// Assert: lock acquire precedes the SECOND (RMW) Config call.
	acquireIdx := -1
	for i, ev := range events {
		if ev == "create:"+expectedPool && acquireIdx == -1 {
			acquireIdx = i
		}
	}
	if acquireIdx == -1 {
		t.Fatalf("lock was never acquired; events=%v", events)
	}

	// Find the RMW Config call — the one that happens after the lock acquire.
	rmwReadIdx := -1
	for i := acquireIdx + 1; i < len(events); i++ {
		if events[i] == "qemu-config" {
			rmwReadIdx = i
			break
		}
	}
	if rmwReadIdx == -1 {
		t.Fatalf("no qemu-config call found after lock acquire; events=%v", events)
	}

	// Update must happen after the RMW read, and lock release must follow update.
	updateIdx, releaseIdx := -1, -1
	for i, ev := range events {
		if ev == "update-config" && updateIdx == -1 {
			updateIdx = i
		}
		if ev == "delete:"+expectedPool {
			releaseIdx = i
		}
	}
	if releaseIdx == -1 {
		t.Fatalf("lock was never released; events=%v", events)
	}
	if updateIdx == -1 {
		t.Fatalf("update-config was never called; events=%v", events)
	}
	if updateIdx <= rmwReadIdx {
		t.Errorf("update(%d) must follow RMW read(%d); events=%v", updateIdx, rmwReadIdx, events)
	}
	if releaseIdx <= updateIdx {
		t.Errorf("lock release(%d) must follow update(%d); events=%v", releaseIdx, updateIdx, events)
	}
}

// TestHandleSetDiskMetadata_LockAcquireFailureRetriable verifies a pool service
// error surfaces as a retriable error and no config update is written.
func TestHandleSetDiskMetadata_LockAcquireFailureRetriable(t *testing.T) {
	t.Parallel()

	const diskCID = "local-lvm:vm-100-disk-0"
	const vmid = int64(100)

	pools := newRecordingPoolService(nil)
	pools.createErr = fmt.Errorf("pmxcfs unavailable")

	var updateCalled bool
	deps := handlers.Deps{
		Config: testConfig(),
		Logger: log.NewNopLogger(),
		Agent:  &mockAgentService{},
		PVE: &lockTestPVEClient{
			qemuSvc: &ldmQEMU{diskCID: diskCID},
			nodesSvc: &ldmNodes{
				updateFn: func(_ context.Context, _, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
					updateCalled = true
					return nil
				},
			},
			clusterSvc: &ldmCluster{resp: ldmClusterResources(vmid, lockTestNode)},
			poolsSvc:   pools,
		},
	}

	h := handlers.HandleSetDiskMetadata(deps)
	_, err := h.Handle(context.Background(),
		marshalArgs(diskCID, map[string]any{"instance_id": "vm-abc"}),
		jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected retriable error when lock cannot be acquired")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("lock-acquire failure must be retriable; got %v", err)
	}
	if updateCalled {
		t.Error("UpdateQemuConfig must not be called when lock acquisition fails")
	}
}

// ==========================================================================
// delete_vm stampDeletingTag lock tests
// ==========================================================================

// fastPathDepsWithPools builds Deps with fast_path_delete enabled and an explicit
// pool service. Uses testDepsFoundVMWithPools for the VMID lock + cluster wiring.
func fastPathDepsWithPools(
	vmid int,
	qemuSvc *mockQEMUService,
	nodesSvc *mockNodesService,
	pools pve.PoolService,
) handlers.Deps {
	deps := testDepsFoundVMWithPools(vmid, qemuSvc, nodesSvc, pools)
	enabled := true
	deps.Config.FastPathDelete = &enabled
	return deps
}

// TestDeleteVM_StampDeletingTag_LockAcquiredBeforeRead verifies the per-VMID
// cluster lock is acquired before the Config read for the bosh-deleting stamp.
// Uses fast_path_delete=true so the test does not need a task service.
func TestDeleteVM_StampDeletingTag_LockAcquiredBeforeRead(t *testing.T) {
	t.Parallel()

	const vmid = 101
	events := []string{}
	pools := newRecordingPoolService(&events)
	expectedPool := fmt.Sprintf("bosh-lock-vm-%d", vmid)

	var gotTags string
	deps := fastPathDepsWithPools(
		vmid,
		&mockQEMUService{
			configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
				events = append(events, "qemu-config")
				return map[string]any{"tags": "existing-tag"}, nil
			},
			stopFn: func(_ context.Context, _ string, _ int) (string, error) {
				return "upid-stop", nil
			},
		},
		&mockNodesService{
			updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				events = append(events, "update-config")
				if params.Tags != nil {
					gotTags = *params.Tags
				}
				return nil
			},
			deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
				return &sdknodes.DeleteQemuResponse{}, nil
			},
		},
		pools,
	)

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(),
		marshalArgs(fmt.Sprintf("%d", vmid)),
		jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotTags, "bosh-deleting") {
		t.Errorf("bosh-deleting tag must be written; got tags=%q", gotTags)
	}

	// Lock acquire must precede the Config read for the stamp.
	acquireIdx, readIdx := -1, -1
	for i, ev := range events {
		if ev == "create:"+expectedPool && acquireIdx == -1 {
			acquireIdx = i
		}
		if ev == "qemu-config" && readIdx == -1 {
			readIdx = i
		}
	}
	if acquireIdx == -1 {
		t.Fatalf("lock never acquired; events=%v", events)
	}
	if readIdx == -1 {
		t.Fatalf("qemu-config never read; events=%v", events)
	}
	if acquireIdx >= readIdx {
		t.Errorf("lock acquire(%d) must precede qemu-config read(%d); events=%v", acquireIdx, readIdx, events)
	}
}

// TestDeleteVM_StampDeletingTag_NilPoolsFallback verifies stampDeletingTag still
// writes bosh-deleting when the pool service is nil (lock fails → best-effort
// unlocked path). Uses fast_path_delete=true.
func TestDeleteVM_StampDeletingTag_NilPoolsFallback(t *testing.T) {
	t.Parallel()

	const vmid = 101
	var gotTags string
	deps := fastPathDepsWithPools(
		vmid,
		&mockQEMUService{
			configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
				return map[string]any{"tags": "existing-tag"}, nil
			},
			stopFn: func(_ context.Context, _ string, _ int) (string, error) {
				return "upid-stop", nil
			},
		},
		&mockNodesService{
			updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				if params.Tags != nil {
					gotTags = *params.Tags
				}
				return nil
			},
			deleteQemuFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
				return &sdknodes.DeleteQemuResponse{}, nil
			},
		},
		nil, // nil pool service → lock fails → best-effort unlocked write
	)

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(),
		marshalArgs(fmt.Sprintf("%d", vmid)),
		jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotTags, "bosh-deleting") {
		t.Errorf("bosh-deleting must be written even when lock unavailable; got tags=%q", gotTags)
	}
}
