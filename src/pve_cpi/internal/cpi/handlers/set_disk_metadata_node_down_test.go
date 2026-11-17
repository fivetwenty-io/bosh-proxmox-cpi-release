package handlers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// diskMetaDownNode is the member a node-down fixture takes offline.
const diskMetaDownNode = "pve-down"

// diskMetaNodeDownFixture builds a two-member cluster in which testNode is up
// and diskMetaDownNode is down. The quorate cluster reports the down member
// offline, and every listing of it answers the 595 a proxied request to an
// unreachable node gets. holderNode names the node whose guest has the disk
// in its config. The other node's guest has an unrelated disk.
func diskMetaNodeDownFixture(t *testing.T, holderNode string) (*diskMetaNodesMock, *diskMetaClientMock) {
	t.Helper()
	const otherVMID = int64(300)
	onDown := otherVMID
	onUp := testVMID
	if holderNode == diskMetaDownNode {
		onDown, onUp = testVMID, otherVMID
	}
	clusterSvc := &diskMetaClusterSvc{
		resp: clusterResourcesWithVMs(
			struct {
				vmid int64
				node string
			}{onUp, testNode},
			struct {
				vmid int64
				node string
			}{onDown, diskMetaDownNode},
		),
		statusFn: func() (*sdkclusterapi.ListStatusResponse, error) {
			resp := sdkclusterapi.ListStatusResponse{
				json.RawMessage(`{"type":"cluster","name":"lab","quorate":1}`),
				json.RawMessage(`{"type":"node","name":"` + testNode + `","online":1}`),
				json.RawMessage(`{"type":"node","name":"` + diskMetaDownNode + `","online":0}`),
			}
			return &resp, nil
		},
	}
	nodesSvc := &diskMetaNodesMock{
		failNodes: map[string]error{diskMetaDownNode: makeSdkAPIErr(595, "no route to host")},
	}
	configs := map[string]map[string]any{
		diskKey(testNode, int(onUp)): {"scsi0": "local-lvm:vm-300-disk-7"},
	}
	if holderNode == testNode {
		configs[diskKey(testNode, int(onUp))] = vmConfigWithDisk(testDiskCID, "")
	}
	return nodesSvc, buildDiskMetaPVE(clusterSvc, configs, nodesSvc)
}

// nodeDownCtx disables retry backoff, so a fixture that does retry the down
// member fails fast instead of sleeping out the curve.
func nodeDownCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestHandleSetDiskMetadata_DownNodeDoesNotBlockHolderElsewhere: with one
// member offline, the metadata still lands on the VM that holds the disk on
// an online member, and the offline member is never listed, so the call
// neither stalls on its retries nor fails.
func TestHandleSetDiskMetadata_DownNodeDoesNotBlockHolderElsewhere(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, testNode)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	meta := map[string]any{"deployment": "cf", "instance_id": "vm-abc123"}
	if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, meta), jsonrpc.Context{}); err != nil {
		t.Fatalf("a down member must not fail a call whose disk is held elsewhere, got: %v", err)
	}
	if nodesSvc.capturedDesc == nil || !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Fatal("the metadata must be written to the holder on the online member")
	}
	if got := nodesSvc.listCalls[diskMetaDownNode]; got != 0 {
		t.Fatalf("the member the cluster reports offline must not be listed, got %d listing attempts", got)
	}
}

// TestHandleSetDiskMetadata_DiskOnDownNodeFailsRetriable: when the only VM
// that could hold the disk is on the offline member, the call lists that
// member once more with the strict retry budget, which is what rides out a
// short blip, and when it still doesn't answer the call fails retriable,
// names the member, and writes nothing. It never reports the disk as not
// attached, which would drop the metadata without an error.
func TestHandleSetDiskMetadata_DiskOnDownNodeFailsRetriable(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, diskMetaDownNode)
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	_, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("a disk that may be on the offline member must fail the call, not read as not attached")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the down-target error must be retriable, got: %v", err)
	}
	if !strings.Contains(err.Error(), diskMetaDownNode) {
		t.Fatalf("the error must name the offline member, got: %v", err)
	}
	if nodesSvc.capturedDesc != nil || nodesSvc.capturedTags != nil {
		t.Fatal("nothing may be written when the holder can't be found")
	}
	if got := nodesSvc.listCalls[diskMetaDownNode]; got != pve.DefaultTransientMaxAttempts {
		t.Fatalf("the offline member must get one strict listing with the full budget, got %d listing attempts", got)
	}
}

// diskMetaStatus answers /cluster/status for the node-down fixture, with the
// down member online or offline as asked.
func diskMetaStatus(downOnline bool) (*sdkclusterapi.ListStatusResponse, error) {
	flag := "0"
	if downOnline {
		flag = "1"
	}
	resp := sdkclusterapi.ListStatusResponse{
		json.RawMessage(`{"type":"cluster","name":"lab","quorate":1}`),
		json.RawMessage(`{"type":"node","name":"` + testNode + `","online":1}`),
		json.RawMessage(`{"type":"node","name":"` + diskMetaDownNode + `","online":` + flag + `}`),
	}
	return &resp, nil
}

// TestHandleSetDiskMetadata_DiskOnBlippingNodeLandsAfterRelist: a corosync blip
// reports the holder's node offline while its VMs keep running. The scan
// leaves the node out and finds no holder, so it lists that node again with
// the strict retry budget, which rides out the blip, finds the holder there,
// and writes the metadata.
func TestHandleSetDiskMetadata_DiskOnBlippingNodeLandsAfterRelist(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, diskMetaDownNode)
	client.qemuSvc.configs[diskKey(diskMetaDownNode, int(testVMID))] = vmConfigWithDisk(testDiskCID, "")
	// The member reads offline until its first listing has been tried, and
	// the blip ends after that failed attempt.
	client.clusterSvc.(*diskMetaClusterSvc).statusFn = func() (*sdkclusterapi.ListStatusResponse, error) {
		return diskMetaStatus(nodesSvc.listCalls[diskMetaDownNode] > 0)
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration {
		delete(nodesSvc.failNodes, diskMetaDownNode)
		return 0
	})
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	if _, err := h.Handle(ctx, makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("a blip the strict listing rides out must not fail the call, got: %v", err)
	}
	if nodesSvc.capturedDesc == nil || !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Fatal("the metadata must be written to the holder on the member that came back")
	}
	if got := nodesSvc.listCalls[diskMetaDownNode]; got != 2 {
		t.Fatalf("the member must be listed again until it answers, got %d listing attempts, want 2", got)
	}
}

// TestHandleSetDiskMetadata_MemberGoingOfflineDuringListing: a member the status
// still reports online before the fan-out, and offline once its first listing
// has failed, is left out after that one attempt, and the metadata lands on
// the holder on the online member.
func TestHandleSetDiskMetadata_MemberGoingOfflineDuringListing(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, testNode)
	client.clusterSvc.(*diskMetaClusterSvc).statusFn = func() (*sdkclusterapi.ListStatusResponse, error) {
		return diskMetaStatus(nodesSvc.listCalls[diskMetaDownNode] == 0)
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("a member going offline mid-listing must not fail a call whose disk is held elsewhere, got: %v", err)
	}
	if nodesSvc.capturedDesc == nil || !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Fatal("the metadata must be written to the holder on the online member")
	}
	if got := nodesSvc.listCalls[diskMetaDownNode]; got != 1 {
		t.Fatalf("the member must be left out after its first failed listing, got %d listing attempts", got)
	}
}

// TestHandleSetDiskMetadata_NodeGoingOfflineDuringConfigReads: a member whose
// listing answered and that goes down before the scan reads its guest's
// config is left out once the cluster reports it offline, and the metadata
// lands on the holder on the online member.
func TestHandleSetDiskMetadata_NodeGoingOfflineDuringConfigReads(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, testNode)
	delete(nodesSvc.failNodes, diskMetaDownNode)
	client.qemuSvc.configErrs = map[string]error{diskKey(diskMetaDownNode, 300): makeSdkAPIErr(595, "no route to host")}
	client.clusterSvc.(*diskMetaClusterSvc).statusFn = func() (*sdkclusterapi.ListStatusResponse, error) {
		return diskMetaStatus(client.qemuSvc.configErrCalls == 0)
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("a member going offline during the config reads must not fail a call whose disk is held elsewhere, got: %v", err)
	}
	if nodesSvc.capturedDesc == nil || !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Fatal("the metadata must be written to the holder on the online member")
	}
	if got := client.qemuSvc.configErrCalls; got != 1 {
		t.Fatalf("the down member's guest must be read once, got %d reads", got)
	}
}

// TestHandleSetDiskMetadata_ConfigErrorOnOnlineNodeStillFails: a config read
// that fails on a member the cluster still reports online fails the call,
// because a fault on the VM that holds the disk must never read as "not
// attached".
func TestHandleSetDiskMetadata_ConfigErrorOnOnlineNodeStillFails(t *testing.T) {
	t.Parallel()
	nodesSvc, client := diskMetaNodeDownFixture(t, testNode)
	delete(nodesSvc.failNodes, diskMetaDownNode)
	client.qemuSvc.configErrs = map[string]error{diskKey(diskMetaDownNode, 300): makeSdkAPIErr(595, "no route to host")}
	client.clusterSvc.(*diskMetaClusterSvc).statusFn = func() (*sdkclusterapi.ListStatusResponse, error) {
		return diskMetaStatus(true)
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	_, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{})
	if err == nil || !strings.Contains(err.Error(), "Config error for vm 300") {
		t.Fatalf("want the config error on the online member, got: %v", err)
	}
	if nodesSvc.capturedDesc != nil {
		t.Fatal("nothing may be written when a config read failed on an online member")
	}
}

// TestHandleSetDiskMetadata_WarnsThatOfflineNodesEscapedTheAmbiguityCheck: a
// holder found while a member is offline gets the metadata, and the call
// warns that the ambiguity check did not cover that member.
func TestHandleSetDiskMetadata_WarnsThatOfflineNodesEscapedTheAmbiguityCheck(t *testing.T) {
	t.Parallel()
	_, client := diskMetaNodeDownFixture(t, testNode)
	deps := makeDiskMetaDeps(client)
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps.Logger = logger
	h := handlers.HandleSetDiskMetadata(deps)

	if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "ambiguity check did not cover") && strings.Contains(fmtAttr(e.Attrs["offline_nodes"]), diskMetaDownNode) {
			return
		}
	}
	t.Fatalf("want a warning that names %s as outside the ambiguity check, got %+v", diskMetaDownNode, obs.All())
}

// fmtAttr renders a logged attribute for a substring check.
func fmtAttr(v any) string {
	s, _ := v.(string)
	return s
}

// TestHandleSetDiskMetadata_ParkedDiskWithNodeDownReadsAsNotAttached: a parker
// on an online member that holds the disk proves that no VM on the offline
// member holds it, so the call answers "not attached" without listing the
// offline member again, whether the listing row or only the config marks the
// parker.
func TestHandleSetDiskMetadata_ParkedDiskWithNodeDownReadsAsNotAttached(t *testing.T) {
	t.Parallel()
	for name, rowTags := range map[string]string{"row tags": "bosh-parker", "config tags only": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const parkerVMID = 900
			nodesSvc, client := diskMetaNodeDownFixture(t, testNode)
			rows := sdkclusterapi.ListResourcesResponse{
				json.RawMessage(`{"vmid":900,"node":"` + testNode + `","type":"qemu","tags":"` + rowTags + `"}`),
				json.RawMessage(`{"vmid":300,"node":"` + diskMetaDownNode + `","type":"qemu"}`),
			}
			client.clusterSvc.(*diskMetaClusterSvc).resp = &rows
			client.qemuSvc.configs = map[string]map[string]any{
				diskKey(testNode, parkerVMID): {"scsi1": testDiskCID, "tags": "bosh-parker"},
			}
			h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

			if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
				t.Fatalf("a disk a parker on an online member holds must read as not attached, got: %v", err)
			}
			if nodesSvc.capturedDesc != nil || nodesSvc.capturedTags != nil {
				t.Fatal("nothing may be written to a parker")
			}
			if got := nodesSvc.listCalls[diskMetaDownNode]; got != 0 {
				t.Fatalf("the parker settles the answer, so the offline member must not be listed, got %d attempts", got)
			}
		})
	}
}

// TestHandleSetDiskMetadata_PendingDeleteSlotStillHolds: a slot whose delete
// PVE could only record as pending is missing from the config endpoint's
// view, while the running guest still has the disk. The scan reads both
// views, so the VM still counts as the holder and gets the metadata.
func TestHandleSetDiskMetadata_PendingDeleteSlotStillHolds(t *testing.T) {
	t.Parallel()
	nodesSvc := &diskMetaNodesMock{}
	clusterSvc := &diskMetaClusterSvc{resp: clusterResourcesWithVM(testVMID, testNode)}
	client := buildDiskMetaPVE(clusterSvc, map[string]map[string]any{
		diskKey(testNode, int(testVMID)): {"name": "vm-100"},
	}, nodesSvc)
	nodesSvc.pending = map[string]*sdknodes.ListQemuPendingResponse{
		testNode + ":100": {
			json.RawMessage(`{"key":"name","value":"vm-100"}`),
			json.RawMessage(`{"key":"scsi0","value":"` + testDiskCID + `","delete":1}`),
		},
	}
	h := handlers.HandleSetDiskMetadata(makeDiskMetaDeps(client))

	if _, err := h.Handle(nodeDownCtx(), makeMetaArgs(t, testDiskCID, map[string]any{"deployment": "cf"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nodesSvc.capturedDesc == nil || !strings.Contains(*nodesSvc.capturedDesc, testDiskCID) {
		t.Fatal("a VM whose slot has a pending delete still holds the disk and must get the metadata")
	}
}
