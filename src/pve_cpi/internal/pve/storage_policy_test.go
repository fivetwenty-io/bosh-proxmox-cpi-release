package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// stubPolicyDeps implements PolicyDeps for unit tests. When err is non-nil,
// both StorageInfo and ClusterNodeCount return that error unconditionally so
// individual error-path tests don't need two separate stub types.
type stubPolicyDeps struct {
	storages       map[string]StorageInfo
	size           int
	storageErr     error
	clusterSizeErr error
}

func (s *stubPolicyDeps) StorageInfo(_ context.Context, name string) (StorageInfo, error) {
	if s.storageErr != nil {
		return StorageInfo{}, s.storageErr
	}
	info, ok := s.storages[name]
	if !ok {
		return StorageInfo{}, fmt.Errorf("storage %q not found", name)
	}
	return info, nil
}

func (s *stubPolicyDeps) ClusterNodeCount(_ context.Context) (int, error) {
	if s.clusterSizeErr != nil {
		return 0, s.clusterSizeErr
	}
	return s.size, nil
}

// ---- IsBlockStorage helper tests ----------------------------------------

func TestIsBlockStorage(t *testing.T) {
	t.Parallel()
	trueTypes := []string{"lvm", "lvmthin", "zfspool", "rbd", "LVM", "LVMTHIN", "ZFSPool", "RBD"}
	for _, typ := range trueTypes {
		if !IsBlockStorage(typ) {
			t.Errorf("IsBlockStorage(%q) = false, want true", typ)
		}
	}

	falseTypes := []string{"dir", "nfs", "cifs", "cephfs", "glusterfs", "btrfs", "pbs", ""}
	for _, typ := range falseTypes {
		if IsBlockStorage(typ) {
			t.Errorf("IsBlockStorage(%q) = true, want false", typ)
		}
	}
}

// ---- ValidateLightStemcellStorage tests ---------------------------------

// helper builds a minimal stub with a single named storage.
func singleStorageStub(name, typ string, shared bool, clusterSize int) *stubPolicyDeps {
	return &stubPolicyDeps{
		storages: map[string]StorageInfo{
			name: {Name: name, Type: typ, Shared: shared},
		},
		size: clusterSize,
	}
}

// Test 1: block storage (lvm) rejected even on single-node cluster.
func TestValidateLightStemcellStorage_BlockReject_LVM(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("vg0", "lvm", false, 1)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "vg0", "")
	if err == nil {
		t.Fatal("expected error for lvm storage, got nil")
	}
	if !strings.Contains(err.Error(), "block-only") {
		t.Errorf("error %q should mention block-only", err.Error())
	}
}

// Test 2: block storage (rbd) rejected even though PVE marks it shared by type.
// Rule 1 (block) must trump rule 3 (shared).
func TestValidateLightStemcellStorage_BlockReject_RBD(t *testing.T) {
	t.Parallel()
	// rbd: Shared flag false but IsShared() would return true via type heuristic —
	// rule 1 must fire first.
	deps := singleStorageStub("ceph", "rbd", false, 3)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "ceph", "pve2")
	if err == nil {
		t.Fatal("expected error for rbd storage, got nil")
	}
	if !strings.Contains(err.Error(), "block-only") {
		t.Errorf("error %q should mention block-only", err.Error())
	}
	if !strings.Contains(err.Error(), "rbd") {
		t.Errorf("error %q should name the type rbd", err.Error())
	}
}

// Test 3: lvmthin on single-node rejected (block rule beats single-node rule).
func TestValidateLightStemcellStorage_BlockReject_LVMThin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("lvtp", "lvmthin", false, 1)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "lvtp", "")
	if err == nil {
		t.Fatal("expected error for lvmthin, got nil")
	}
	if !strings.Contains(err.Error(), "block-only") {
		t.Errorf("error %q should mention block-only", err.Error())
	}
}

// Test 4: single-node cluster, dir storage, no node hint → accept, chosenNode="".
func TestValidateLightStemcellStorage_SingleNode_NoHint(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 1)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty string", node)
	}
}

// Test 5: single-node cluster, dir storage, node hint provided → accept, hint returned.
func TestValidateLightStemcellStorage_SingleNode_WithHint(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 1)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// Test 6: multi-node cluster + nfs (shared by type) → accept, no node required.
func TestValidateLightStemcellStorage_MultiNode_SharedByType(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("nfs1", "nfs", false, 3)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "nfs1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// shared backend: caller resolves node later; chosenNode passes through (empty).
	if node != "" {
		t.Errorf("chosenNode = %q, want empty", node)
	}
}

// Test 7: multi-node cluster + dir storage marked shared via Shared flag → accept.
func TestValidateLightStemcellStorage_MultiNode_SharedByFlag(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDeps{
		storages: map[string]StorageInfo{
			"shared-dir": {Name: "shared-dir", Type: "dir", Shared: true},
		},
		size: 3,
	}
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "shared-dir", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// Test 8: multi-node cluster + cephfs (shared by type) → accept, node hint returned.
func TestValidateLightStemcellStorage_MultiNode_CephFS(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("cfs", "cephfs", false, 5)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "cfs", "pve3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve3" {
		t.Errorf("chosenNode = %q, want pve3", node)
	}
}

// Test 9: multi-node cluster + local dir + node pinned → accept, pin returned.
func TestValidateLightStemcellStorage_MultiNode_LocalWithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 3)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "pve2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("chosenNode = %q, want pve2", node)
	}
}

// Test 10: multi-node cluster + local dir + NO node → reject with actionable message.
func TestValidateLightStemcellStorage_MultiNode_LocalNoPin_Reject(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 3)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected rejection for local storage on multi-node without node pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q should mention cluster size", err.Error())
	}
}

// Test 11: multi-node + zfspool (block) + node pinned → still rejected (rule 1 first).
func TestValidateLightStemcellStorage_BlockReject_ZFSPool(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("rpool", "zfspool", false, 1)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "rpool", "pve1")
	if err == nil {
		t.Fatal("expected error for zfspool, got nil")
	}
	if !strings.Contains(err.Error(), "block-only") {
		t.Errorf("error %q should mention block-only", err.Error())
	}
}

// Test 12: empty storage name → immediate error before any deps call.
func TestValidateLightStemcellStorage_EmptyStorageName(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDeps{size: 1}
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "", "")
	if err == nil {
		t.Fatal("expected error for empty storage name")
	}
	if !strings.Contains(err.Error(), "storage name required") {
		t.Errorf("error %q should mention 'storage name required'", err.Error())
	}
}

// Test 13: StorageInfo lookup error → propagated as *cpierrors.Error.
func TestValidateLightStemcellStorage_StorageInfoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("pve api timeout")
	deps := &stubPolicyDeps{
		storages:   map[string]StorageInfo{},
		size:       1,
		storageErr: sentinel,
	}
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected error from StorageInfo, got nil")
	}
	if !strings.Contains(err.Error(), "lookup storage") {
		t.Errorf("error %q should mention 'lookup storage'", err.Error())
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Errorf("error %q should contain the original sentinel message", err.Error())
	}
}

// Test 14: ClusterNodeCount error → propagated as *cpierrors.Error.
func TestValidateLightStemcellStorage_ClusterNodeCountError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("cluster unreachable")
	deps := &stubPolicyDeps{
		storages: map[string]StorageInfo{
			"local": {Name: "local", Type: "dir"},
		},
		size:           0,
		clusterSizeErr: sentinel,
	}
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected error from ClusterNodeCount, got nil")
	}
	if !strings.Contains(err.Error(), "cluster node count") {
		t.Errorf("error %q should mention 'cluster node count'", err.Error())
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Errorf("error %q should contain the original sentinel message", err.Error())
	}
}

// Test 15: glusterfs on multi-node (shared by type) → accept.
func TestValidateLightStemcellStorage_MultiNode_Glusterfs(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("gfs", "glusterfs", false, 4)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "gfs", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty", node)
	}
}

// Test 16: cluster size exactly 2 (boundary) + local + pin → accept.
func TestValidateLightStemcellStorage_TwoNodeCluster_LocalWithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 2)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// Test 17: cluster size exactly 2 + local + no pin → reject.
func TestValidateLightStemcellStorage_TwoNodeCluster_LocalNoPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", false, 2)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected rejection for two-node cluster local storage without pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
}
