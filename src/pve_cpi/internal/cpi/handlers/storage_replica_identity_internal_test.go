package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestSelectStemcellAnchor_SkipsStorageReplica pins that a storage-set
// replica never anchors a stemcell's reference set, even with the lowest
// VMID, and that it lands in the sweep set instead.
func TestSelectStemcellAnchor_SkipsStorageReplica(t *testing.T) {
	t.Parallel()
	refs := []pve.TemplateRef{
		{VMID: 30009, Node: "n2", Tags: stemcellCacheTag + ";bosh-stemcell-sha-abcd1234;" + pve.ReplicaStorageTagForStorage("ns_2")},
		{VMID: 30010, Node: "n1", Tags: stemcellCacheTag + ";bosh-stemcell-sha-abcd1234"},
		{VMID: 30011, Node: "n2", Tags: stemcellCacheTag + ";bosh-stemcell-sha-abcd1234;" + pve.ReplicaNodeTagForNode("n2")},
	}
	anchor, sweep, ok := selectStemcellAnchor(refs)
	if !ok || anchor.VMID != 30010 {
		t.Fatalf("want anchor 30010, got %+v ok=%v", anchor, ok)
	}
	if len(sweep) != 2 || sweep[0].VMID != 30009 || sweep[1].VMID != 30011 {
		t.Fatalf("want both replicas in the sweep set, got %+v", sweep)
	}
	_, _, ok = selectStemcellAnchor(refs[:1])
	if ok {
		t.Fatal("a lone storage replica must not anchor")
	}
}

// TestEnsureTemplateVM_SHATagDedup_SkipsStorageReplicaAnchor is the
// storage-replica twin of TestEnsureTemplateVM_SHATagDedup_SkipsReplicaAnchor:
// a lower-VMID storage replica must never satisfy the create-side dedup
// lookup, because registering the director reference against it would
// consult a fossil ref set.
func TestEnsureTemplateVM_SHATagDedup_SkipsStorageReplicaAnchor(t *testing.T) {
	t.Parallel()

	const primaryVMID = int64(30500)
	const primaryNode = "pve-node1"
	const replicaVMID = int64(30100) // lower than primary
	const sha8 = "891b3b74"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"

	var createCalled bool
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		wbMockNodes: wbMockNodes{
			listStorageFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				entry, _ := json.Marshal(map[string]any{"volid": "nfs:import/noble.qcow2"})
				resp := sdknodes.ListStorageContentResponse{entry}
				return &resp, nil
			},
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(replicaVMID, primaryNode, "bosh-stemcell-ubuntu-noble-1-364-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8+";"+pve.ReplicaStorageTagForStorage("ns_2")),
				clusterResourceQemuTemplate(primaryVMID, primaryNode, "bosh-stemcell-ubuntu-noble-1-364-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}

	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.364"}
	stemcellCID := pve.BuildHeavyStemcellCID("nfs", "noble.qcow2")
	vmid, node, err := ensureTemplateVM(context.Background(), deps, "pve-node1", "nfs", "noble.qcow2", fullSHA,
		"", pve.StemcellKindHeavy, stemcellCID, "test-director", cp, "")
	if err != nil {
		t.Fatalf("ensureTemplateVM returned error: %v", err)
	}
	if vmid != primaryVMID || node != primaryNode {
		t.Errorf("vmid/node = %d/%q; want primary %d/%q", vmid, node, primaryVMID, primaryNode)
	}
	if createCalled {
		t.Error("QEMU.Create must NOT be called: the primary already satisfies the dedup lookup")
	}
}
