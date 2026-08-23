// Package handlers: internal tests for the single-shared-template stemcell
// topology: a node-local qcow2 staging pool on a multi-node cluster is
// acceptable when the template-disk pool (vm_storage) is shared, because the
// one cache template's disk clones to any node via the cross-node Target=
// redirect. Also covers the early rejection of a block-only (rbd/lvm)
// staging pool, which cannot hold qcow2 files at all.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// sstClusterStorage serves a configurable set of storage entries so a test
// can classify the staging pool and the vm_storage pool differently.
type sstClusterStorage struct {
	sdkclusterstorage.Service
	entries []map[string]any
}

func (m *sstClusterStorage) ListStorage(
	_ context.Context, _ *sdkclusterstorage.ListStorageParams,
) (*sdkclusterstorage.ListStorageResponse, error) {
	resp := make(sdkclusterstorage.ListStorageResponse, 0, len(m.entries))
	for _, e := range m.entries {
		raw, _ := json.Marshal(e)
		resp = append(resp, raw)
	}
	return &resp, nil
}

// sstLocalResolver classifies every storage as a node-local backend, driving
// validateStemcellStorageShared into its local-storage branch.
type sstLocalResolver struct{}

func (r *sstLocalResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return &sstLocalBackend{}, nil
}

type sstLocalBackend struct{}

func (b *sstLocalBackend) Kind() pve.BackendKind { return pve.BackendLocal }
func (b *sstLocalBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return "pve01", nil
}
func (b *sstLocalBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return "pve01", nil
}

func sstDeps(cfg *config.CPIConfig, storageEntries []map[string]any) Deps {
	const nodeCount = 3 // multi-node: the shared-storage check only fires above one node
	return Deps{
		Config: cfg,
		PVE: &wbMockClient{
			clusterStorageSvc: &sstClusterStorage{entries: storageEntries},
			clusterSvc:        &wbMockCluster{nodeCount: nodeCount},
		},
		Resolver: &sstLocalResolver{},
		Logger:   log.NewNopLogger(),
	}
}

func sstEntries(vmStorageType string, vmStorageShared bool) []map[string]any {
	shared := 0
	if vmStorageShared {
		shared = 1
	}
	return []map[string]any{
		{"storage": "local", "type": "dir", "shared": 0, "nodes": ""},
		{"storage": "vmpool", "type": vmStorageType, "shared": shared, "nodes": ""},
	}
}

func sstConfig() *config.CPIConfig {
	return &config.CPIConfig{
		Node:             "pve01",
		StemcellStorage:  "local",
		VMStorage:        "vmpool",
		StemcellStrategy: config.StemcellStrategyTemplate,
	}
}

// TestValidateStemcellStorageShared_SharedVMStorage_Allowed: local staging on
// a 3-node cluster is fine when vm_storage is shared (rbd): the single
// template's disk clones cross-node, no replication needed.
func TestValidateStemcellStorageShared_SharedVMStorage_Allowed(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), sstEntries("rbd", true))

	if err := validateStemcellStorageShared(context.Background(), deps, "local"); err != nil {
		t.Fatalf("expected local staging accepted with shared vm_storage, got: %v", err)
	}
}

// TestValidateStemcellStorageShared_LocalVMStorage_StillRejected: with a
// node-local vm_storage the template disk cannot clone cross-node, so the
// rejection must stand.
func TestValidateStemcellStorageShared_LocalVMStorage_StillRejected(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), sstEntries("lvmthin", false))

	err := validateStemcellStorageShared(context.Background(), deps, "local")
	if err == nil {
		t.Fatal("expected rejection with local vm_storage on a multi-node cluster, got nil")
	}
}

// TestValidateStemcellStorageShared_ImportStrategy_StillRejected: the import
// strategy reads the qcow2 from every VM's own node at create_vm, so a local
// staging pool must still be rejected even with shared vm_storage.
func TestValidateStemcellStorageShared_ImportStrategy_StillRejected(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellStrategy = config.StemcellStrategyImport
	deps := sstDeps(cfg, sstEntries("rbd", true))

	err := validateStemcellStorageShared(context.Background(), deps, "local")
	if err == nil {
		t.Fatal("expected rejection under stemcell_strategy=import, got nil")
	}
}

// TestValidateStemcellStorageShared_UnknownVMStorage_StillRejected: when the
// vm_storage pool cannot be classified the relaxation must not apply
// (fail-closed): the operator keeps the actionable rejection.
func TestValidateStemcellStorageShared_UnknownVMStorage_StillRejected(t *testing.T) {
	t.Parallel()
	// Storage listing omits the vm_storage pool entirely.
	deps := sstDeps(sstConfig(), []map[string]any{
		{"storage": "local", "type": "dir", "shared": 0, "nodes": ""},
	})

	err := validateStemcellStorageShared(context.Background(), deps, "local")
	if err == nil {
		t.Fatal("expected rejection when vm_storage sharedness is unknown, got nil")
	}
}

// TestValidateStemcellStorageShared_ReplicateLocal_Unchanged: the opt-in
// replication path keeps accepting local staging regardless of vm_storage.
func TestValidateStemcellStorageShared_ReplicateLocal_Unchanged(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellReplicateLocal = true
	deps := sstDeps(cfg, sstEntries("lvmthin", false))

	if err := validateStemcellStorageShared(context.Background(), deps, "local"); err != nil {
		t.Fatalf("expected replication opt-in to keep accepting local staging, got: %v", err)
	}
}

// TestResolveStemcellStorageAndNode_BlockOnlyStaging_Rejected: a block-only
// pool (rbd) holds VM disk images, never qcow2 files; pointing
// stemcell_storage at one (directly, or via the vm_storage fallback) must
// fail fast with an actionable message instead of an opaque upload error.
func TestResolveStemcellStorageAndNode_BlockOnlyStaging_Rejected(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Node:             "pve01",
		StemcellStorage:  "",
		VMStorage:        "rbdpool",
		StemcellStrategy: config.StemcellStrategyTemplate,
	}
	deps := sstDeps(cfg, []map[string]any{
		{"storage": "rbdpool", "type": "rbd", "shared": 1, "nodes": ""},
	})

	_, _, err := resolveStemcellStorageAndNode(context.Background(), deps)
	if err == nil {
		t.Fatal("expected block-only staging pool rejected, got nil")
	}
	if !strings.Contains(err.Error(), "file-capable") {
		t.Errorf("expected actionable file-capable guidance in error, got: %v", err)
	}
}

// TestResolveStemcellStorageAndNode_BlockOnlyStaging_NamesVMStorage: when the
// rejected staging pool and vm_storage are DIFFERENT pools, the remediation
// must name the vm_storage pool, not repeat the pool it just rejected.
func TestResolveStemcellStorageAndNode_BlockOnlyStaging_NamesVMStorage(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Node:             "pve01",
		StemcellStorage:  "local-lvm",
		VMStorage:        "cephfs-pool",
		StemcellStrategy: config.StemcellStrategyTemplate,
	}
	deps := sstDeps(cfg, []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin", "shared": 0, "nodes": ""},
		{"storage": "cephfs-pool", "type": "cephfs", "shared": 1, "nodes": ""},
	})

	_, _, err := resolveStemcellStorageAndNode(context.Background(), deps)
	if err == nil {
		t.Fatal("expected block-only staging pool rejected, got nil")
	}
	if !strings.Contains(err.Error(), `keep vm_storage on "cephfs-pool"`) {
		t.Errorf("expected remediation to name vm_storage pool, got: %v", err)
	}
	if strings.Contains(err.Error(), `keep vm_storage on "local-lvm"`) {
		t.Errorf("remediation must not name the rejected pool as vm_storage, got: %v", err)
	}
	if !strings.Contains(err.Error(), "node-local pool works") {
		t.Errorf("expected the template-strategy topology hint, got: %v", err)
	}
}

// TestResolveStemcellStorageAndNode_BlockOnlyStaging_ImportStrategy_NoHint:
// under stemcell_strategy=import a node-local staging pool never works, so
// the topology hint must be absent from the block-only rejection.
func TestResolveStemcellStorageAndNode_BlockOnlyStaging_ImportStrategy_NoHint(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		Node:             "pve01",
		StemcellStorage:  "local-lvm",
		VMStorage:        "cephfs-pool",
		StemcellStrategy: config.StemcellStrategyImport,
	}
	deps := sstDeps(cfg, []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin", "shared": 0, "nodes": ""},
		{"storage": "cephfs-pool", "type": "cephfs", "shared": 1, "nodes": ""},
	})

	_, _, err := resolveStemcellStorageAndNode(context.Background(), deps)
	if err == nil {
		t.Fatal("expected block-only staging pool rejected, got nil")
	}
	if strings.Contains(err.Error(), "node-local pool works") {
		t.Errorf("topology hint must be absent under stemcell_strategy=import, got: %v", err)
	}
}

// TestResolveStemcellStorageAndNode_TemplateNodeMismatch_LocalStaging_Rejected:
// the node-local-staging relaxation must not accept a stemcell_template_node
// that cannot read the staged qcow2 (the template build would fail opaquely
// at QEMU create time on the wrong node).
func TestResolveStemcellStorageAndNode_TemplateNodeMismatch_LocalStaging_Rejected(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellTemplateNode = "pve02"
	deps := sstDeps(cfg, sstEntries("rbd", true))

	_, _, err := resolveStemcellStorageAndNode(context.Background(), deps)
	if err == nil {
		t.Fatal("expected stemcell_template_node mismatch rejected with node-local staging, got nil")
	}
	if !strings.Contains(err.Error(), "stemcell_template_node") {
		t.Errorf("expected error to name stemcell_template_node, got: %v", err)
	}
}

// TestResolveStemcellStorageAndNode_TemplateNodeMatchesStaging_Accepted: a
// stemcell_template_node equal to the staging node reads the qcow2 fine.
func TestResolveStemcellStorageAndNode_TemplateNodeMatchesStaging_Accepted(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellTemplateNode = "pve01"
	deps := sstDeps(cfg, sstEntries("rbd", true))

	node, storage, err := resolveStemcellStorageAndNode(context.Background(), deps)
	if err != nil {
		t.Fatalf("expected matching stemcell_template_node accepted, got: %v", err)
	}
	if node != "pve01" || storage != "local" {
		t.Errorf("unexpected resolution: node=%q storage=%q", node, storage)
	}
}

// TestValidateTemplateNodeReachesStaging_ReplicateLocal_Skipped: replication
// copies the qcow2 to every node, so a mismatched template node keeps its
// pre-existing behavior (no new rejection).
func TestValidateTemplateNodeReachesStaging_ReplicateLocal_Skipped(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellTemplateNode = "pve02"
	cfg.StemcellReplicateLocal = true
	deps := sstDeps(cfg, sstEntries("rbd", true))

	if err := validateTemplateNodeReachesStaging(context.Background(), deps, "local", "pve01"); err != nil {
		t.Fatalf("expected replicate_local to skip the template-node guard, got: %v", err)
	}
}

// TestValidateTemplateNodeReachesStaging_SharedStaging_Accepted: a shared
// staging pool is readable from every node; any template node works.
func TestValidateTemplateNodeReachesStaging_SharedStaging_Accepted(t *testing.T) {
	t.Parallel()
	cfg := sstConfig()
	cfg.StemcellStorage = "nfs-pool"
	cfg.StemcellTemplateNode = "pve02"
	deps := sstDeps(cfg, []map[string]any{
		{"storage": "nfs-pool", "type": "nfs", "shared": 1, "nodes": ""},
		{"storage": "vmpool", "type": "rbd", "shared": 1, "nodes": ""},
	})

	if err := validateTemplateNodeReachesStaging(context.Background(), deps, "nfs-pool", "pve01"); err != nil {
		t.Fatalf("expected shared staging to accept any template node, got: %v", err)
	}
}

// TestLightStemcellPolicyOpts: the light-path relaxation mirrors the heavy
// path exactly: option present only under a non-import strategy with a
// positively shared vm_storage.
func TestLightStemcellPolicyOpts(t *testing.T) {
	t.Parallel()

	if got := lightStemcellPolicyOpts(context.Background(), sstDeps(sstConfig(), sstEntries("rbd", true))); len(got) != 1 {
		t.Errorf("shared vm_storage + template strategy: expected 1 option, got %d", len(got))
	}

	importCfg := sstConfig()
	importCfg.StemcellStrategy = config.StemcellStrategyImport
	if got := lightStemcellPolicyOpts(context.Background(), sstDeps(importCfg, sstEntries("rbd", true))); len(got) != 0 {
		t.Errorf("import strategy: expected no options, got %d", len(got))
	}

	unknownDeps := sstDeps(sstConfig(), []map[string]any{
		{"storage": "local", "type": "dir", "shared": 0, "nodes": ""},
	})
	if got := lightStemcellPolicyOpts(context.Background(), unknownDeps); len(got) != 0 {
		t.Errorf("unknown vm_storage classification: expected no options (fail-closed), got %d", len(got))
	}
}

// sstNamedClusterFn backs wbMockCluster.listConfigNodesFn with caller-named
// nodes; listClusterNodes reads the "name" field of each entry.
func sstNamedClusterFn(names ...string) func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	return func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		resp := make(sdkcluster.ListConfigNodesResponse, 0, len(names))
		for _, n := range names {
			raw, _ := json.Marshal(map[string]string{"name": n})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
}

// sstContentFn serves FindStemcellByFilename: the named node reports the
// given volid under content type "import"; every other node reports nothing.
func sstContentFn(nodeWithFile, volid string) func(context.Context, string, string, *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	return func(_ context.Context, node, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		resp := sdknodes.ListStorageContentResponse{}
		if node == nodeWithFile {
			raw, _ := json.Marshal(map[string]any{"volid": volid})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
}

// TestStemcellQcow2MissingError_NodeLocalStaging_FoundElsewhere: with the
// qcow2 present only on another node's copy of a node-local staging pool,
// the confirmed-absence error must explain the topology (import cannot run
// here; the cache template is the supported route) and be retriable, because
// the usual cause is cluster-index lag hiding a live template.
func TestStemcellQcow2MissingError_NodeLocalStaging_FoundElsewhere(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), sstEntries("rbd", true))
	client := deps.PVE.(*wbMockClient)
	client.clusterSvc.(*wbMockCluster).listConfigNodesFn = sstNamedClusterFn("pve01", "pve02", "pve03")
	client.nodesSvc = &countingNodesService{
		listStorageContentFn: sstContentFn("pve02", "local:import/sc-abcd1234.qcow2"),
	}
	parsed := &createVMParsedArgs{
		stemcellStorage:  "local",
		stemcellVolPath:  "import/sc-abcd1234.qcow2",
		stemcellFilename: "sc-abcd1234.qcow2",
	}

	err := stemcellQcow2MissingError(context.Background(), deps, log.NewNopLogger(), "pve01", parsed)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), `exists only on node "pve02"`) {
		t.Errorf("expected the topology error naming the staging node, got: %v", err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Errorf("expected a retriable error, got: %#v", err)
	}
}

// TestStemcellQcow2MissingError_NotFoundAnywhere_Generic: when the qcow2 is
// genuinely gone from every node, the generic re-upload guidance stands.
func TestStemcellQcow2MissingError_NotFoundAnywhere_Generic(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), sstEntries("rbd", true))
	client := deps.PVE.(*wbMockClient)
	client.clusterSvc.(*wbMockCluster).listConfigNodesFn = sstNamedClusterFn("pve01", "pve02", "pve03")
	client.nodesSvc = &countingNodesService{
		listStorageContentFn: sstContentFn("", ""),
	}
	parsed := &createVMParsedArgs{
		stemcellStorage:  "local",
		stemcellVolPath:  "import/sc-abcd1234.qcow2",
		stemcellFilename: "sc-abcd1234.qcow2",
	}

	err := stemcellQcow2MissingError(context.Background(), deps, log.NewNopLogger(), "pve01", parsed)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "re-upload the stemcell") {
		t.Errorf("expected the generic re-upload guidance, got: %v", err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("expected a non-retriable generic error, got: %#v", err)
	}
}

// TestStemcellQcow2MissingError_SharedStaging_Generic: on a shared staging
// pool the cross-node probe is pointless (one copy, visible everywhere); the
// generic guidance stands without any per-node scans.
func TestStemcellQcow2MissingError_SharedStaging_Generic(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), []map[string]any{
		{"storage": "local", "type": "nfs", "shared": 1, "nodes": ""},
		{"storage": "vmpool", "type": "rbd", "shared": 1, "nodes": ""},
	})
	parsed := &createVMParsedArgs{
		stemcellStorage:  "local",
		stemcellVolPath:  "import/sc-abcd1234.qcow2",
		stemcellFilename: "sc-abcd1234.qcow2",
	}

	err := stemcellQcow2MissingError(context.Background(), deps, log.NewNopLogger(), "pve01", parsed)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "re-upload the stemcell") {
		t.Errorf("expected the generic re-upload guidance, got: %v", err)
	}
}

// sstRecordingStorage records DeleteVolumeIfExists calls by node.
type sstRecordingStorage struct {
	replicationMockStorage
	mu      sync.Mutex
	deletes []string
}

func (m *sstRecordingStorage) DeleteVolumeIfExists(_ context.Context, node, _, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, node)
	return true, nil
}

// TestSweepHeavyQcow2OtherNodes_LocalStaging_Sweeps: with a node-local (or
// unclassifiable) staging pool, delete_stemcell's sweep must visit every
// cluster node not already covered, catching per-node copies the replica
// list cannot name (second CPI entry uploads, migrated template anchors).
func TestSweepHeavyQcow2OtherNodes_LocalStaging_Sweeps(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), sstEntries("rbd", true))
	client := deps.PVE.(*wbMockClient)
	client.clusterSvc.(*wbMockCluster).listConfigNodesFn = sstNamedClusterFn("pve01", "pve02", "pve03")
	rec := &sstRecordingStorage{}
	client.storageSvc = rec

	sweepHeavyQcow2OtherNodesBestEffort(context.Background(), deps, log.NewNopLogger(),
		"local", "import/sc.qcow2", map[string]bool{"pve01": true})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deletes) != 2 {
		t.Fatalf("expected deletes on the 2 unswept nodes, got %v", rec.deletes)
	}
	got := map[string]bool{}
	for _, n := range rec.deletes {
		got[n] = true
	}
	if !got["pve02"] || !got["pve03"] {
		t.Errorf("expected deletes on pve02 and pve03, got %v", rec.deletes)
	}
}

// TestSweepHeavyQcow2OtherNodes_SharedStaging_NoSweep: a positively shared
// staging pool has exactly one copy; the primary delete already removed it
// and the sweep must not spend per-node API calls.
func TestSweepHeavyQcow2OtherNodes_SharedStaging_NoSweep(t *testing.T) {
	t.Parallel()
	deps := sstDeps(sstConfig(), []map[string]any{
		{"storage": "local", "type": "nfs", "shared": 1, "nodes": ""},
		{"storage": "vmpool", "type": "rbd", "shared": 1, "nodes": ""},
	})
	client := deps.PVE.(*wbMockClient)
	client.clusterSvc.(*wbMockCluster).listConfigNodesFn = sstNamedClusterFn("pve01", "pve02", "pve03")
	rec := &sstRecordingStorage{}
	client.storageSvc = rec

	sweepHeavyQcow2OtherNodesBestEffort(context.Background(), deps, log.NewNopLogger(),
		"local", "import/sc.qcow2", map[string]bool{"pve01": true})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deletes) != 0 {
		t.Errorf("expected no sweep on shared staging, got deletes on %v", rec.deletes)
	}
}
