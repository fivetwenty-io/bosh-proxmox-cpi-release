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

// ---- IsLinkedCloneSupported tests ---------------------------------------

func TestIsLinkedCloneSupported(t *testing.T) {
	t.Parallel()

	cases := []struct {
		storageType string
		want        bool
	}{
		// File-backed and CoW-capable backends → true.
		{StorageTypeNFS, true},
		{StorageTypeCIFS, true},
		{StorageTypeZFSPool, true},
		{StorageTypeLVMThin, true},
		{StorageTypeRBD, true},
		{StorageTypeCephFS, true},
		{"dir", true},

		// Thick LVM: the sole backend without linked-clone support → false.
		{StorageTypeLVM, false},

		// Empty / unknown types: permissive default → true.
		{"", true},
		{"unknown-type", true},

		// Case-insensitive: upper-case LVM still returns false.
		{"LVM", false},
		{"Lvm", false},

		// Case-insensitive: upper-case lvmthin must still return true.
		{"LVMTHIN", true},
	}

	for _, tc := range cases {
		t.Run(tc.storageType, func(t *testing.T) {
			t.Parallel()
			got := IsLinkedCloneSupported(tc.storageType)
			if got != tc.want {
				t.Errorf("IsLinkedCloneSupported(%q) = %v, want %v", tc.storageType, got, tc.want)
			}
		})
	}
}

// ---- ValidateLightStemcellStorage tests ---------------------------------

// helper builds a minimal stub with a single named storage.
// shared is always false in this suite; StorageInfo.Shared defaults to false.
func singleStorageStub(name, typ string, clusterSize int) *stubPolicyDeps {
	return &stubPolicyDeps{
		storages: map[string]StorageInfo{
			name: {Name: name, Type: typ, Shared: false},
		},
		size: clusterSize,
	}
}

// Test 1: block storage (lvm) rejected even on single-node cluster.
func TestValidateLightStemcellStorage_BlockReject_LVM(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("vg0", "lvm", 1)
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
	deps := singleStorageStub("ceph", "rbd", 3)
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
	deps := singleStorageStub("lvtp", "lvmthin", 1)
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
	deps := singleStorageStub("local", "dir", 1)
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
	deps := singleStorageStub("local", "dir", 1)
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
	deps := singleStorageStub("nfs1", "nfs", 3)
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
	deps := singleStorageStub("cfs", "cephfs", 5)
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
	deps := singleStorageStub("local", "dir", 3)
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
	deps := singleStorageStub("local", "dir", 3)
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

// Test 10b: multi-node + local dir + NO node, but the caller vouches that
// placement no longer depends on the upload node (single-shared-template
// topology) → accepted, empty chosenNode so the caller falls back to its
// configured node.
func TestValidateLightStemcellStorage_MultiNode_LocalNoPin_UnpinnedOptionAccepts(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 3)
	node, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "",
		WithUnpinnedLocalAccepted())
	if err != nil {
		t.Fatalf("unexpected error with WithUnpinnedLocalAccepted: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty (caller resolves)", node)
	}
}

// Test 10c: the option relaxes ONLY rule 5 — block-only storage is still
// rejected with it set (rule 1 evaluates first).
func TestValidateLightStemcellStorage_UnpinnedOption_BlockStillRejected(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("vg0", "lvm", 3)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "vg0", "",
		WithUnpinnedLocalAccepted())
	if err == nil {
		t.Fatal("expected block-only rejection to survive WithUnpinnedLocalAccepted")
	}
	if !strings.Contains(err.Error(), "block-only") {
		t.Errorf("error %q should mention block-only", err.Error())
	}
}

// Test 11: multi-node + zfspool (block) + node pinned → still rejected (rule 1 first).
func TestValidateLightStemcellStorage_BlockReject_ZFSPool(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("rpool", "zfspool", 1)
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
	deps := singleStorageStub("gfs", "glusterfs", 4)
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
	deps := singleStorageStub("local", "dir", 2)
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
	deps := singleStorageStub("local", "dir", 2)
	_, err := ValidateLightStemcellStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected rejection for two-node cluster local storage without pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
}

// ---- ValidateTemplateCloneStorage tests ---------------------------------

// TC-01: single-node cluster → accept any backend, no pin needed.
func TestValidateTemplateCloneStorage_SingleNode_NoPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 1)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty string", node)
	}
}

// TC-02: single-node cluster + node hint → hint returned.
func TestValidateTemplateCloneStorage_SingleNode_WithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 1)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// TC-03: multi-node + shared storage (nfs) → accept, no pin required.
func TestValidateTemplateCloneStorage_MultiNode_SharedNFS_NoPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("nfs1", "nfs", 3)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "nfs1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty", node)
	}
}

// TC-04: multi-node + shared storage (nfs) + pin → pin returned.
func TestValidateTemplateCloneStorage_MultiNode_SharedNFS_WithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("nfs1", "nfs", 3)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "nfs1", "pve2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("chosenNode = %q, want pve2", node)
	}
}

// TC-05: multi-node + local dir + node pinned → accept, pin returned.
func TestValidateTemplateCloneStorage_MultiNode_LocalDir_WithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 3)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "pve2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("chosenNode = %q, want pve2", node)
	}
}

// TC-06: multi-node + local dir + NO pin → error with actionable message.
func TestValidateTemplateCloneStorage_MultiNode_LocalDir_NoPin_Reject(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 3)
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected rejection for local storage on multi-node without node pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q should mention cluster size", err.Error())
	}
	if !strings.Contains(err.Error(), "auto-migrate") {
		t.Errorf("error %q should mention auto-migrate", err.Error())
	}
}

// TC-07: block storage that is also shared (rbd) → accept (block is OK for clones,
// IsShared returns true for rbd by type).
func TestValidateTemplateCloneStorage_MultiNode_RBD_Shared_NoPin(t *testing.T) {
	t.Parallel()
	// rbd: IsShared() returns true via type heuristic even without Shared flag.
	deps := singleStorageStub("ceph", "rbd", 3)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "ceph", "")
	if err != nil {
		t.Fatalf("unexpected error for rbd (shared block): %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty", node)
	}
}

// TC-08: block + local (lvmthin) + multi-node + no pin → error (local rule applies).
func TestValidateTemplateCloneStorage_MultiNode_LVMThin_Local_NoPin_Reject(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("lvtp", "lvmthin", 3)
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "lvtp", "")
	if err == nil {
		t.Fatal("expected rejection for lvmthin local on multi-node without pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
}

// TC-09: block + local (lvmthin) + multi-node + pin → accept (pin satisfies constraint).
func TestValidateTemplateCloneStorage_MultiNode_LVMThin_Local_WithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("lvtp", "lvmthin", 3)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "lvtp", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// TC-10: empty storage name → immediate error before any deps call.
func TestValidateTemplateCloneStorage_EmptyStorageName(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDeps{size: 3}
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "", "")
	if err == nil {
		t.Fatal("expected error for empty storage name")
	}
	if !strings.Contains(err.Error(), "storage name required") {
		t.Errorf("error %q should mention 'storage name required'", err.Error())
	}
}

// TC-11: StorageInfo error → propagated.
func TestValidateTemplateCloneStorage_StorageInfoError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("pve api timeout")
	deps := &stubPolicyDeps{
		storages:   map[string]StorageInfo{},
		size:       3,
		storageErr: sentinel,
	}
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
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

// TC-12: ClusterNodeCount error → propagated.
func TestValidateTemplateCloneStorage_ClusterNodeCountError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("cluster unreachable")
	deps := &stubPolicyDeps{
		storages: map[string]StorageInfo{
			"local": {Name: "local", Type: "dir"},
		},
		size:           0,
		clusterSizeErr: sentinel,
	}
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
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

// TC-13: multi-node + shared-by-flag dir → accept.
func TestValidateTemplateCloneStorage_MultiNode_SharedByFlag(t *testing.T) {
	t.Parallel()
	deps := &stubPolicyDeps{
		storages: map[string]StorageInfo{
			"shared-dir": {Name: "shared-dir", Type: "dir", Shared: true},
		},
		size: 4,
	}
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "shared-dir", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// TC-14: two-node boundary + local + pin → accept.
func TestValidateTemplateCloneStorage_TwoNodeCluster_LocalWithPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 2)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve1" {
		t.Errorf("chosenNode = %q, want pve1", node)
	}
}

// TC-15: two-node boundary + local + no pin → reject.
func TestValidateTemplateCloneStorage_TwoNodeCluster_LocalNoPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("local", "dir", 2)
	_, err := ValidateTemplateCloneStorage(context.Background(), deps, "local", "")
	if err == nil {
		t.Fatal("expected rejection for two-node cluster without pin")
	}
	if !strings.Contains(err.Error(), "cloud_properties.node") {
		t.Errorf("error %q should mention cloud_properties.node", err.Error())
	}
}

// TC-16: cephfs (shared by type) multi-node → accept.
func TestValidateTemplateCloneStorage_MultiNode_CephFS_NoPin(t *testing.T) {
	t.Parallel()
	deps := singleStorageStub("cfs", "cephfs", 5)
	node, err := ValidateTemplateCloneStorage(context.Background(), deps, "cfs", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "" {
		t.Errorf("chosenNode = %q, want empty", node)
	}
}
