package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// legacyCloneDeps builds the scalar (non-set-managed) clone path's
// dependencies over the template-gap fakes. storage.shared true makes
// needsReplicaCheck false, so resolveTemplateCacheTarget takes the refs[0]
// fallback, which is the branch a storage replica must never win.
func legacyCloneDeps(rows ...map[string]any) Deps {
	return Deps{
		Config: &config.CPIConfig{Node: "pve-vm", VMStorage: "local-lvm"},
		PVE: &templateGapPVE{
			nodes:   &templateGapNodesSvc{},
			cluster: &templateGapClusterSvc{resourceRows: rows},
			storage: &templateGapClusterStorageSvc{shared: true},
		},
		Logger: log.NewNopLogger(),
	}
}

func legacyTemplateRow(vmid int64, tags string) map[string]any {
	return map[string]any{
		"type": "qemu", "vmid": vmid, "node": templateGapTemplateNode,
		"name": "bosh-stemcell-ubuntu-jammy-1-0", "tags": tags, "template": true,
	}
}

// TestResolveTemplateCacheTarget_SkipsStorageReplicaWhenPrimaryExists pins
// that the scalar path never clones from a storage-set replica while the
// primary exists: the replica's disk lives on a set member, so cloning it
// onto vm_storage downgrades to a full clone under clone_mode auto and
// fails outright under clone_mode linked.
func TestResolveTemplateCacheTarget_SkipsStorageReplicaWhenPrimaryExists(t *testing.T) {
	t.Parallel()
	deps := legacyCloneDeps(
		legacyTemplateRow(30009, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8+";bosh-stemcell-storage-ns-2"),
		legacyTemplateRow(30010, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8),
	)
	shape := &createVMShape{node: "pve-vm", vmStorage: "local-lvm"}
	vmid, node, found, err := resolveTemplateCacheTarget(context.Background(), deps, log.NewNopLogger(), shape, templateGapSHA8)
	if err != nil || !found || vmid != 30010 || node != templateGapTemplateNode {
		t.Fatalf("want primary 30010 on %q, got vmid=%d node=%q found=%v err=%v", templateGapTemplateNode, vmid, node, found, err)
	}
}

// TestResolveTemplateCacheTarget_UsesStorageReplicaWhenPrimaryGone keeps the
// path working when only replicas remain: a full clone is the fallback this
// path already accepts for a missing template, and it beats no VM at all.
func TestResolveTemplateCacheTarget_UsesStorageReplicaWhenPrimaryGone(t *testing.T) {
	t.Parallel()
	deps := legacyCloneDeps(
		legacyTemplateRow(30009, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8+";bosh-stemcell-storage-ns-2"),
	)
	shape := &createVMShape{node: "pve-vm", vmStorage: "local-lvm"}
	vmid, _, found, err := resolveTemplateCacheTarget(context.Background(), deps, log.NewNopLogger(), shape, templateGapSHA8)
	if err != nil || !found || vmid != 30009 {
		t.Fatalf("want replica 30009 as the fallback, got vmid=%d found=%v err=%v", vmid, found, err)
	}
}

// A template whose only copy sits on a node the cluster reports down is no
// clone source. The lookup reports a miss, so the caller imports instead of
// addressing a clone to a dead node.
func TestResolveTemplateCacheTarget_OnlyTemplateOnDeadNodeIsAMiss(t *testing.T) {
	t.Parallel()
	deps := legacyCloneDeps(
		legacyTemplateRow(30010, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8),
	)
	deps.PVE.(*templateGapPVE).cluster.offline = map[string]bool{templateGapTemplateNode: true}
	shape := &createVMShape{node: "pve-vm", vmStorage: "local-lvm"}
	vmid, node, found, err := resolveTemplateCacheTarget(context.Background(), deps, log.NewNopLogger(), shape, templateGapSHA8)
	if err != nil || found {
		t.Fatalf("a template on a dead node must be a miss, got vmid=%d node=%q found=%v err=%v", vmid, node, found, err)
	}
}

// With two copies, the one on the live node is chosen even though the dead
// node's copy has the lower VMID.
func TestResolveTemplateCacheTarget_SkipsTemplateOnDeadNode(t *testing.T) {
	t.Parallel()
	dead := legacyTemplateRow(30001, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8)
	dead["node"] = "pve-dead"
	live := legacyTemplateRow(30010, stemcellCacheTag+";bosh-stemcell-sha-"+templateGapSHA8)
	deps := legacyCloneDeps(dead, live)
	deps.PVE.(*templateGapPVE).cluster.offline = map[string]bool{"pve-dead": true}
	shape := &createVMShape{node: "pve-vm", vmStorage: "local-lvm"}
	vmid, node, found, err := resolveTemplateCacheTarget(context.Background(), deps, log.NewNopLogger(), shape, templateGapSHA8)
	if err != nil || !found || vmid != 30010 || node != templateGapTemplateNode {
		t.Fatalf("want the live node's 30010 on %q, got vmid=%d node=%q found=%v err=%v",
			templateGapTemplateNode, vmid, node, found, err)
	}
}
