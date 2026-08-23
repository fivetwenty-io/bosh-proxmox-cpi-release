package handlers_test

// Cluster-index-lag defenses around the stemcell cache template. PVE's
// /cluster/resources index can trail a just-frozen template by minutes on a
// loaded cluster; these tests pin the authoritative per-node fallbacks that
// keep delete_stemcell and create_vm from mistaking that lag for absence.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// lagRawTemplateItem builds one raw per-node ListQemu entry in PVE's wire
// shape (integer boolean template flag).
func lagRawTemplateItem(vmid int64, name, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"vmid":     vmid,
		"name":     name,
		"tags":     tags,
		"template": 1,
	})
	return raw
}

// emptyClusterResources reports an empty cluster index for every query: the
// lag window where a just-frozen template is not yet visible.
func emptyClusterResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

// lagConfigNodes builds a ListConfigNodes response naming the given nodes.
func lagConfigNodes(names ...string) func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	return func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		resp := make(sdkcluster.ListConfigNodesResponse, 0, len(names))
		for _, name := range names {
			raw, _ := json.Marshal(map[string]string{"name": name})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
}

// TestDeleteStemcell_ClusterIndexLag_NodeSweepFindsTemplate reproduces the
// live-observed orphaning: the cluster lookup misses the just-frozen
// template, and without the authoritative per-node sweep delete_stemcell
// took the no-template branch, deleting the qcow2 while leaving the live
// template behind. The template sits on a node that is NOT the configured
// one, so this also pins that the sweep covers every cluster node rather
// than guessing the template's home (create_stemcell can legitimately build
// elsewhere: owning-node retarget, node pin, or adoption). The with-template
// branch must run: deregister the last ref and destroy the template.
func TestDeleteStemcell_ClusterIndexLag_NodeSweepFindsTemplate(t *testing.T) {
	t.Parallel()

	const templateVMID = int64(7031)
	const otherNode = "pve-node2" // not Config.Node, not stemcell_template_node

	var destroyed []string
	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != otherNode {
				empty := sdknodes.ListQemuResponse{}
				return &empty, nil
			}
			resp := sdknodes.ListQemuResponse{
				lagRawTemplateItem(templateVMID, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
			}
			return &resp, nil
		},
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyed = append(destroyed, node+":"+vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn:   emptyClusterResources,
		listConfigNodesFn: lagConfigNodes(vmNode, otherNode),
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{
		qemuSvc: qemuSvc, nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc,
	})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	if _, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := fmt.Sprintf("%s:%d", otherNode, templateVMID)
	if len(destroyed) != 1 || destroyed[0] != want {
		t.Errorf("destroyed = %v; want exactly [%q]: the per-node sweep must route the delete"+
			" through the with-template branch", destroyed, want)
	}
	if storageSvc.deleteVolumeIfExistsCalls == 0 {
		t.Error("last-ref heavy delete must still remove the staging qcow2")
	}
}

// TestDeleteStemcell_ClusterIndexLag_NodeSweepProbeErrors_NoTemplateBranch
// verifies the sweep fails closed: when every node's probe errors and no
// ref-anchor was found, delete_stemcell returns a retriable error rather
// than entering the no-template branch — that branch deletes the qcow2, and
// concluding "no template" from a sweep that could not actually see the
// nodes is the original production bug reachable through its own fix's
// error path. Nothing may be destroyed and the qcow2 delete must not run.
// The sweep must still have attempted every enumerated node (a sweep that
// never probes would pass the destruction assertions trivially).
func TestDeleteStemcell_ClusterIndexLag_NodeSweepProbeErrors_NoTemplateBranch(t *testing.T) {
	t.Parallel()

	var probed []string
	var destroyed []string
	nodesSvc := &stemcellMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			probed = append(probed, node)
			return nil, fmt.Errorf("node listing unavailable")
		},
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyed = append(destroyed, node+":"+vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	clusterSvc := &stemcellMockCluster{
		listResourcesFn:   emptyClusterResources,
		listConfigNodesFn: lagConfigNodes(vmNode, "pve-node2"),
	}
	storageSvc := &deleteStemcellMockStorage{}

	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{
		nodesSvc: nodesSvc, clusterSvc: clusterSvc, storageSvc: storageSvc,
	})
	h := handlers.HandleDeleteStemcell(deps)

	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	_, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"})
	if err == nil {
		t.Fatal("expected a retriable error when every probe fails and no anchor was found; got success")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("incomplete-sweep error = %v; want retriable (the Director re-drives once PVE recovers)", err)
	}
	if len(probed) != 2 {
		t.Errorf("sweep probed %v; want both enumerated nodes attempted despite errors", probed)
	}
	if len(destroyed) != 0 {
		t.Errorf("nothing may be destroyed on an incomplete sweep, got %v", destroyed)
	}
	if storageSvc.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("qcow2 delete ran %d time(s) on an incomplete sweep; the volume must be left in place",
			storageSvc.deleteVolumeIfExistsCalls)
	}
}

// TestCreateVM_TemplateCacheMiss_HomeNodeListingFindsTemplate_Clones covers
// the cross-node half of the settled re-check: the VM places on a node other
// than the template's home, the cluster index lags past the whole re-check
// budget, and the placement-node probe cannot see the template. The
// home-node probe must find it so the VM clones cross-node instead of dying
// on the import fallback.
func TestCreateVM_TemplateCacheMiss_HomeNodeListingFindsTemplate_Clones(t *testing.T) {
	defer handlers.SetTemplateCacheRecheckDelay(0)()

	const homeNode = "pve"       // config.Node, where create_stemcell froze the template
	const placementNode = "pve9" // where the VM is pinned
	const templateVMID = int64(6042)

	cloneCalled := false
	var cloneNode, cloneSourceVMID string

	n := &vmMockNodes{
		listQemuFn: func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			if node != homeNode {
				empty := sdknodes.ListQemuResponse{}
				return &empty, nil
			}
			resp := sdknodes.ListQemuResponse{
				lagRawTemplateItem(templateVMID, "bosh-stemcell-ubuntu-jammy-1-438",
					"bosh-stemcell-cache;bosh-stemcell-sha-"+oldCIDSHA8),
			}
			return &resp, nil
		},
		createQemuCloneFn: func(_ context.Context, node, vmid string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			cloneCalled = true
			cloneNode = node
			cloneSourceVMID = vmid
			return cloneUPIDResponse(), nil
		},
	}
	q := &vmMockQEMU{}
	a := &vmMockAgent{}
	c := &vmMockCluster{listResourcesFn: emptyListResources}

	deps := buildVMDepsForOldCIDLookup(q, n, c, a)
	deps.Config.Node = homeNode
	// Shared vm_storage and a second node: the cross-node Target= clone is
	// only legal when the storage is reachable from both sides.
	deps.PVE = &mockPVEClient{
		qemuSvc:    q,
		nodesSvc:   n,
		clusterSvc: withConfigNodes(c, 2),
		tasksSvc:   deps.PVE.Tasks(),
		clusterStorageSvc: &mockClusterStorage{
			storageName: storageName,
			storageType: "nfs",
			shared:      true,
		},
	}
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-homenode-probe", testStemcellCIDWithSHA,
		map[string]any{"cores": 1, "memory": 512, "target_node": placementNode},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("homenode-probe")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cloneCalled {
		t.Fatal("the home-node probe found the template; create_vm must clone from it, not import")
	}
	if cloneNode != homeNode {
		t.Errorf("clone issued against node %q; want the template's home node %q", cloneNode, homeNode)
	}
	if cloneSourceVMID != fmt.Sprintf("%d", templateVMID) {
		t.Errorf("clone source vmid = %q; want %d", cloneSourceVMID, templateVMID)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("QEMU.Create (import path) must not be called, got %d calls", len(q.createCalls))
	}
}
