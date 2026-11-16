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
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
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
// that could hold the disk is on the offline member, the call fails
// retriable, names the member, and writes nothing. It neither reports the
// disk as not attached, which would drop the metadata without an error, nor
// spends the retry budget on the member first.
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
	if got := nodesSvc.listCalls[diskMetaDownNode]; got != 0 {
		t.Fatalf("the member the cluster reports offline must not be listed, got %d listing attempts", got)
	}
}
