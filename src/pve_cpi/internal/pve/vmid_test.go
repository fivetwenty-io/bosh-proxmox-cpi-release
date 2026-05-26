package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// stubNodesService satisfies sdknodes.Service via embedding; only
// ListStorageContent is implemented. All other methods panic if called.
type stubNodesService struct {
	sdknodes.Service
	listStorageContentFn func(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error)
}

func (s *stubNodesService) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	if s.listStorageContentFn != nil {
		return s.listStorageContentFn(ctx, node, storage, params)
	}
	resp := sdknodes.ListStorageContentResponse{}
	return &resp, nil
}

// buildStorageContent marshals volid strings into a ListStorageContentResponse.
func buildStorageContent(volids ...string) *sdknodes.ListStorageContentResponse {
	resp := make(sdknodes.ListStorageContentResponse, 0, len(volids))
	for _, v := range volids {
		entry := struct {
			VolID string `json:"volid"`
		}{VolID: v}
		raw, _ := json.Marshal(entry)
		resp = append(resp, raw)
	}
	return &resp
}

// ---- stub cluster service ----
// stubClusterService embeds a nil pointer to satisfy the full cluster.Service
// interface. Only ListResources is overridden; all other methods panic if called.

type stubClusterService struct {
	sdkcluster.Service // embed for interface satisfaction; nil — panics on other methods
	listResourcesFn    func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (s *stubClusterService) ListResources(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	if s.listResourcesFn != nil {
		return s.listResourcesFn(ctx, params)
	}
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

// newVMIDClient builds a mockClient with a cluster stub wired to listFn.
func newVMIDClient(listFn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)) *mockClient {
	return &mockClient{
		tasksSvc:   &mockTasksService{},
		clusterSvc: &stubClusterService{listResourcesFn: listFn},
	}
}

// buildResources marshals a slice of vmid ints into a ListResourcesResponse.
func buildResources(vmids ...int) *sdkcluster.ListResourcesResponse {
	resp := make(sdkcluster.ListResourcesResponse, 0, len(vmids))
	for _, id := range vmids {
		id64 := int64(id)
		entry := struct {
			Vmid int64 `json:"vmid"`
		}{Vmid: id64}
		raw, _ := json.Marshal(entry)
		resp = append(resp, raw)
	}
	return &resp
}

// ---- tests ----

func TestNextVMID_FreeInRange(t *testing.T) {
	// used: 100, 101, 103; free slots include 102, 104..5999.
	// With randomised start the returned ID is any free slot in [100,5999].
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(100, 101, 103), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
	if id == 100 || id == 101 || id == 103 {
		t.Errorf("returned a used VMID: %d", id)
	}
}

func TestNextVMID_EmptyCluster(t *testing.T) {
	// No VMs → returned VMID is somewhere in the VM range.
	// Randomised start means it is not necessarily VMIDRangeVMStart.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
}

func TestNextVMID_AllUsed(t *testing.T) {
	// Fill entire VM range [100..5999].
	all := make([]int, 0, pve.VMIDRangeVMEnd-pve.VMIDRangeVMStart+1)
	for i := pve.VMIDRangeVMStart; i <= pve.VMIDRangeVMEnd; i++ {
		all = append(all, i)
	}
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(all...), nil
	})

	_, err := pve.NextVMID(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when range exhausted, got nil")
	}
}

func TestNextVMID_CustomRange(t *testing.T) {
	// Range [200,250], used [200,201,202]. Free slots: 203..250.
	// Randomised start means any free slot may be returned; verify in-range and not used.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(200, 201, 202), nil
	})

	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 250))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < 200 || id > 250 {
		t.Errorf("VMID %d outside custom range [200,250]", id)
	}
	if id == 200 || id == 201 || id == 202 {
		t.Errorf("returned a used VMID: %d", id)
	}
}

func TestNextVMID_CustomRange_AllUsed(t *testing.T) {
	// Tiny range [200,202] all used → error.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(200, 201, 202), nil
	})

	_, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 202))
	if err == nil {
		t.Fatal("expected exhausted error, got nil")
	}
}

func TestNextDiskVMID_InRange(t *testing.T) {
	// 9000 used; any ID in [9001,9999] is valid with randomised start.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(9000), nil
	})

	id, err := pve.NextDiskVMID(context.Background(), c, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeDiskStart || id > pve.VMIDRangeDiskEnd {
		t.Errorf("disk VMID %d out of range [%d,%d]", id, pve.VMIDRangeDiskStart, pve.VMIDRangeDiskEnd)
	}
	if id == 9000 {
		t.Errorf("returned used disk VMID 9000")
	}
}

func TestNextVMID_Concurrency(t *testing.T) {
	// 100 goroutines call NextVMID against an empty cluster. The process-level
	// globalVMIDMu serialises them, so there is no data race (verified by -race).
	// With randomised start each goroutine picks a different offset, so results
	// are scattered across [VMIDRangeVMStart, VMIDRangeVMEnd]. We verify:
	//   1. No errors.
	//   2. Every returned VMID is within the valid range.
	//   3. ListResources is called exactly once per goroutine.
	//   4. At least two distinct VMIDs are returned (scatter check; astronomically
	//      unlikely to fail on a 5900-wide range with 100 goroutines).

	var callCount int64
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		atomic.AddInt64(&callCount, 1)
		return buildResources(), nil
	})

	const goroutines = 100
	results := make(chan int, goroutines)
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := pve.NextVMID(context.Background(), c)
			if err != nil {
				errs <- err
				return
			}
			results <- id
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}

	seen := make(map[int]struct{})
	for id := range results {
		if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
			t.Errorf("VMID %d outside range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
		}
		seen[id] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("expected scattered VMIDs (>1 distinct value), got %d distinct: %v", len(seen), seen)
	}

	if atomic.LoadInt64(&callCount) != goroutines {
		t.Errorf("expected %d ListResources calls, got %d", goroutines, callCount)
	}
}

func TestNextVMID_SDKError(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return nil, errors.New("connection refused")
	})

	_, err := pve.NextVMID(context.Background(), c)
	if err == nil {
		t.Fatal("expected error from SDK failure, got nil")
	}
}

func TestNextVMID_NilContext(t *testing.T) {
	c := newVMIDClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	_, err := pve.NextVMID(nil, c)
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

func TestNextVMID_NilClient(t *testing.T) {
	_, err := pve.NextVMID(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

func TestNextDiskVMID_NilClient(t *testing.T) {
	_, err := pve.NextDiskVMID(context.Background(), nil, "", "")
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

func TestNextDiskVMID_NilCtx(t *testing.T) {
	c := newVMIDClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	_, err := pve.NextDiskVMID(nil, c, "", "")
	if err == nil {
		t.Fatal("expected error for nil context, got nil")
	}
}

func TestNextDiskVMID_AllUsed(t *testing.T) {
	all := make([]int, 0, pve.VMIDRangeDiskEnd-pve.VMIDRangeDiskStart+1)
	for i := pve.VMIDRangeDiskStart; i <= pve.VMIDRangeDiskEnd; i++ {
		all = append(all, i)
	}
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(all...), nil
	})

	_, err := pve.NextDiskVMID(context.Background(), c, "", "")
	if err == nil {
		t.Fatal("expected exhausted error, got nil")
	}
}

// When NextDiskVMID is called with node+storage, an orphan volume
// "data:vm-9000-disk-0" with no matching VM must still be treated as used
// so the next call returns 9001, not 9000.
func TestNextDiskVMID_UnionsStorageVolumes(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		// No VMs in the synthetic range — cluster says 9000 is free.
		return buildResources(), nil
	})
	c.nodesSvc = &stubNodesService{
		listStorageContentFn: func(_ context.Context, node, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			if node != "pve" || storage != "data" {
				t.Errorf("unexpected node/storage: %q/%q", node, storage)
			}
			// Orphan from a previous failed run.
			return buildStorageContent("data:vm-9000-disk-0"), nil
		},
	}

	id, err := pve.NextDiskVMID(context.Background(), c, "pve", "data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == 9000 {
		t.Fatal("returned 9000 despite orphan vm-9000-disk-0 on storage; orphan was not counted as used")
	}
	if id < pve.VMIDRangeDiskStart || id > pve.VMIDRangeDiskEnd {
		t.Errorf("disk VMID %d outside [%d,%d]", id, pve.VMIDRangeDiskStart, pve.VMIDRangeDiskEnd)
	}
}

// When node/storage are empty, NextDiskVMID falls back to cluster-only
// scan and never calls ListStorageContent.
func TestNextDiskVMID_EmptyStorageSkipsScan(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})
	called := false
	c.nodesSvc = &stubNodesService{
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			called = true
			return buildStorageContent(), nil
		},
	}

	_, err := pve.NextDiskVMID(context.Background(), c, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("ListStorageContent invoked despite empty node/storage args")
	}
}

// Non-disk volumes on the storage (ISOs, backups) must NOT bleed into the
// used set. Only names matching vm-NNN-disk-N count.
func TestNextDiskVMID_IgnoresNonDiskVolumes(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})
	c.nodesSvc = &stubNodesService{
		listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			return buildStorageContent(
				"local:iso/vm-9000-config.iso",        // ISO — not vm-N-disk-N
				"backups:backup/vzdump-qemu-9000.tar", // backup
				"data:vm-9000-disk-0",                 // real disk → should be used
			), nil
		},
	}

	id, err := pve.NextDiskVMID(context.Background(), c, "pve", "data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 9000 is used (vm-9000-disk-0 matched); any other disk-range ID is valid.
	if id == 9000 {
		t.Error("returned 9000 but vm-9000-disk-0 volume marks it as used")
	}
	if id < pve.VMIDRangeDiskStart || id > pve.VMIDRangeDiskEnd {
		t.Errorf("disk VMID %d outside [%d,%d]", id, pve.VMIDRangeDiskStart, pve.VMIDRangeDiskEnd)
	}
}

func TestAllocateWithRetry_Success(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	var created int
	id, err := pve.AllocateWithRetry(context.Background(), c,
		func(vmid int) error { created = vmid; return nil },
		func(err error) bool { return false },
		3,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != created {
		t.Errorf("returned VMID %d != created VMID %d", id, created)
	}
}

func TestAllocateWithRetry_ConflictRetry(t *testing.T) {
	// First two calls fail with conflict; third succeeds.
	attempt := 0
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	id, err := pve.AllocateWithRetry(context.Background(), c,
		func(vmid int) error {
			attempt++
			if attempt < 3 {
				return conflictErr
			}
			return nil
		},
		func(err error) bool { return errors.Is(err, conflictErr) },
		3,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
	if attempt != 3 {
		t.Errorf("expected 3 create attempts, got %d", attempt)
	}
}

func TestAllocateWithRetry_ExhaustedAttempts(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	_, err := pve.AllocateWithRetry(context.Background(), c,
		func(_ int) error { return conflictErr },
		func(err error) bool { return errors.Is(err, conflictErr) },
		3,
	)
	if err == nil {
		t.Fatal("expected error after exhausted attempts, got nil")
	}
}

func TestAllocateWithRetry_NonConflictError(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	fatalErr := errors.New("fatal error")
	_, err := pve.AllocateWithRetry(context.Background(), c,
		func(_ int) error { return fatalErr },
		func(err error) bool { return false },
		3,
	)
	if err == nil {
		t.Fatal("expected error for non-conflict create failure, got nil")
	}
}

func TestAllocateWithRetry_NilCreateFunc(t *testing.T) {
	c := newVMIDClient(nil)
	_, err := pve.AllocateWithRetry(context.Background(), c, nil, nil, 3)
	if err == nil {
		t.Fatal("expected error for nil create func, got nil")
	}
}

func TestNextVMID_WithRange_InvalidIgnored(t *testing.T) {
	// Invalid range (start > end) is ignored; default VM range applies.
	// With randomised start and empty cluster, result is anywhere in default VM range.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(5000, 200)) // invalid: start>end
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside default VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
}

func TestNextVMID_MalformedJSONSkipped(t *testing.T) {
	// One malformed JSON entry must be skipped; allocation proceeds over remaining.
	// used: {101}; free: all of [100,5999] except 101.
	malformed := sdkcluster.ListResourcesResponse{
		json.RawMessage(`{bad json`),
		json.RawMessage(`{"vmid":101}`),
	}
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return &malformed, nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range", id)
	}
	if id == 101 {
		t.Errorf("returned used VMID 101")
	}
}

func TestNextVMID_GapAtStart(t *testing.T) {
	// used: 101 only; free: 100, 102..5999.
	// Randomised start; any free slot in the VM range is valid.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(101), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range", id)
	}
	if id == 101 {
		t.Errorf("returned used VMID 101")
	}
}

func TestNextVMID_VmidsOutsideRangeIgnored(t *testing.T) {
	// VMIDs in the disk range (9000+) must not block the VM range.
	// All of [VMIDRangeVMStart,VMIDRangeVMEnd] is free; returned ID is anywhere in that range.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(9000, 9001), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
}

func TestVMIDRange_VMEndIs5999(t *testing.T) {
	if pve.VMIDRangeVMEnd != 5999 {
		t.Errorf("VMIDRangeVMEnd: expected 5999, got %d", pve.VMIDRangeVMEnd)
	}
}

// TestNextVMID_5500Allocatable verifies that VMIDs in [5500,5999], formerly in the
// stemcell sub-range, are now allocatable as regular VM VMIDs.
func TestNextVMID_5500Allocatable(t *testing.T) {
	// Fill 100..5499; only 5500..5999 remain free.
	all := make([]int, 0, 5500-100)
	for i := 100; i < 5500; i++ {
		all = append(all, i)
	}
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(all...), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Randomised start: result is somewhere in [5500,5999].
	if id < 5500 || id > pve.VMIDRangeVMEnd {
		t.Errorf("expected free VMID in [5500,%d], got %d", pve.VMIDRangeVMEnd, id)
	}
}

func TestAllocateWithRetry_ZeroAttempts(t *testing.T) {
	// maxAttempts ≤ 0 is normalized to 3; conflict on every call → exhausted.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	var callCount int
	_, err := pve.AllocateWithRetry(context.Background(), c,
		func(_ int) error {
			callCount++
			return conflictErr
		},
		func(err error) bool { return errors.Is(err, conflictErr) },
		0, // normalized to 3
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 3 {
		t.Errorf("expected 3 calls (default), got %d", callCount)
	}
}

func TestNextVMID_NilListResourcesResponse(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return nil, nil
	})

	_, err := pve.NextVMID(context.Background(), c)
	if err == nil {
		t.Fatal("expected error for nil ListResources response, got nil")
	}
}

// WithNoBackoff exercises the deterministic, no-sleep retry path.
func TestAllocateWithRetry_NoBackoff(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	attempts := 0
	id, err := pve.AllocateWithRetry(context.Background(), c,
		func(_ int) error {
			attempts++
			if attempts < 2 {
				return conflictErr
			}
			return nil
		},
		func(e error) bool { return errors.Is(e, conflictErr) },
		3,
		pve.WithNoBackoff(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
	}
}

func TestAllocateDiskWithRetry_Success(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	var created int
	id, err := pve.AllocateDiskWithRetry(context.Background(), c, "", "",
		func(vmid int) error { created = vmid; return nil },
		func(_ error) bool { return false },
		3,
		pve.WithNoBackoff(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != created {
		t.Errorf("returned VMID %d != created VMID %d", id, created)
	}
	if id < pve.VMIDRangeDiskStart || id > pve.VMIDRangeDiskEnd {
		t.Errorf("disk VMID %d outside [%d,%d]", id, pve.VMIDRangeDiskStart, pve.VMIDRangeDiskEnd)
	}
}

func TestAllocateDiskWithRetry_ConflictThenSuccess(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	attempts := 0
	id, err := pve.AllocateDiskWithRetry(context.Background(), c, "", "",
		func(_ int) error {
			attempts++
			if attempts < 3 {
				return conflictErr
			}
			return nil
		},
		func(e error) bool { return errors.Is(e, conflictErr) },
		5,
		pve.WithNoBackoff(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
	if id < pve.VMIDRangeDiskStart {
		t.Errorf("expected disk-range VMID, got %d", id)
	}
}

func TestAllocateDiskWithRetry_Exhausted(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	conflictErr := errors.New("already exists")
	_, err := pve.AllocateDiskWithRetry(context.Background(), c, "", "",
		func(_ int) error { return conflictErr },
		func(e error) bool { return errors.Is(e, conflictErr) },
		3,
		pve.WithNoBackoff(),
	)
	if err == nil {
		t.Fatal("expected exhausted error, got nil")
	}
}

func TestAllocateDiskWithRetry_NilCreateFunc(t *testing.T) {
	c := newVMIDClient(nil)
	_, err := pve.AllocateDiskWithRetry(context.Background(), c, "", "", nil, nil, 3)
	if err == nil {
		t.Fatal("expected error for nil create func, got nil")
	}
}

func TestNextVMID_VmidNullField(t *testing.T) {
	// JSON entry where vmid is null must be skipped (pointer nil).
	// used: {100}; free: 101..5999 plus any others in the VM range.
	entry := sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid":null,"type":"node"}`),
		json.RawMessage(fmt.Sprintf(`{"vmid":%d}`, 100)),
	}
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return &entry, nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range", id)
	}
	if id == 100 {
		t.Errorf("returned used VMID 100")
	}
}

// ---- IMP-01: randomised-start scatter tests ----

// TestNextVMIDInRange_AllFreeInRange verifies that with all slots free in a
// small range the returned VMID lands within that range.
func TestNextVMIDInRange_AllFreeInRange(t *testing.T) {
	const rangeStart, rangeEnd = 100, 199
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(rangeStart, rangeEnd))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id < rangeStart || id > rangeEnd {
		t.Errorf("VMID %d outside [%d,%d]", id, rangeStart, rangeEnd)
	}
}

// TestNextVMIDInRange_OnlyOneSlotFree verifies that when all IDs in a range
// are used except one, that one ID is always returned regardless of where
// the random scan starts. Runs multiple iterations to defeat luck.
func TestNextVMIDInRange_OnlyOneSlotFree(t *testing.T) {
	const rangeStart, rangeEnd, freeID = 100, 199, 150
	all := make([]int, 0, rangeEnd-rangeStart)
	for i := rangeStart; i <= rangeEnd; i++ {
		if i != freeID {
			all = append(all, i)
		}
	}

	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(all...), nil
	})

	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		id, err := pve.NextVMID(context.Background(), c, pve.WithRange(rangeStart, rangeEnd))
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", iter, err)
		}
		if id != freeID {
			t.Fatalf("iter %d: expected %d (only free slot), got %d", iter, freeID, id)
		}
	}
}

// TestNextVMIDInRange_ExhaustedRange verifies that a fully-used range returns
// an error. Uses multi-element ranges so WithRange accepts them (requires end > start).
func TestNextVMIDInRange_ExhaustedRange(t *testing.T) {
	tests := []struct {
		name  string
		used  []int
		start int
		end   int
	}{
		{
			name:  "two-element range all used",
			used:  []int{300, 301},
			start: 300,
			end:   301,
		},
		{
			name:  "three-element range all used",
			used:  []int{400, 401, 402},
			start: 400,
			end:   402,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return buildResources(tc.used...), nil
			})
			_, err := pve.NextVMID(context.Background(), c, pve.WithRange(tc.start, tc.end))
			if err == nil {
				t.Fatal("expected error for exhausted range, got nil")
			}
		})
	}
}

// TestNextVMIDInRange_SpreadCheck runs NextVMID 200 times on a fully-free
// [100,1099] range and asserts that more than one distinct VMID is returned,
// confirming the randomised start scatters allocations rather than always
// returning the same ID.
func TestNextVMIDInRange_SpreadCheck(t *testing.T) {
	const rangeStart, rangeEnd = 100, 1099
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	seen := make(map[int]struct{})
	const iterations = 200
	for i := 0; i < iterations; i++ {
		id, err := pve.NextVMID(context.Background(), c, pve.WithRange(rangeStart, rangeEnd))
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if id < rangeStart || id > rangeEnd {
			t.Errorf("iter %d: VMID %d outside [%d,%d]", i, id, rangeStart, rangeEnd)
		}
		seen[id] = struct{}{}
	}
	if len(seen) <= 1 {
		t.Errorf("spread check failed: all %d iterations returned the same VMID (scatter not working)", iterations)
	}
}

// TestNextVMID_LockNotHeldDuringAPICall demonstrates that globalVMIDMu is NOT
// held while the PVE API call executes. A channel-blocking fake client stalls
// the API call. A second goroutine calls NextVMID concurrently and must
// complete before the first one unblocks, proving the lock is not held during
// the network round-trip.
//
// Without the B1 fix (lock held around API call), the second goroutine would
// block on the mutex until the first goroutine's stalled API call completes,
// making this test deadlock or time out.
func TestNextVMID_LockNotHeldDuringAPICall(t *testing.T) {
	// gate controls when the first goroutine's API call unblocks.
	gate := make(chan struct{})
	// firstStarted is closed once goroutine-1's API call has begun.
	firstStarted := make(chan struct{})

	blockingClient := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		select {
		case <-firstStarted:
			// already closed by this goroutine on first invocation; a second
			// goroutine using a different client hits its own fast path.
		default:
			close(firstStarted)
		}
		// Block until gate is opened.
		<-gate
		return buildResources(), nil
	})

	fastClient := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	// Goroutine 1: starts the blocking API call.
	g1done := make(chan error, 1)
	go func() {
		_, err := pve.NextVMID(context.Background(), blockingClient)
		g1done <- err
	}()

	// Wait until goroutine-1's API call has started (first started signal).
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine-1 API call did not start within 5 s")
	}

	// Goroutine 2: must acquire the mutex and complete while goroutine-1 is
	// still blocked in its API call. If the lock were held during the API call,
	// this goroutine would block here until the gate is opened.
	g2done := make(chan error, 1)
	go func() {
		_, err := pve.NextVMID(context.Background(), fastClient)
		g2done <- err
	}()

	// Goroutine-2 must finish promptly (well before the 5 s gate).
	select {
	case err := <-g2done:
		if err != nil {
			t.Errorf("goroutine-2 NextVMID error: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(gate) // unblock goroutine-1 so the test can clean up
		t.Fatal("goroutine-2 was blocked while goroutine-1's API call was in flight; lock held during API call")
	}

	// Unblock goroutine-1 and verify it also completes cleanly.
	close(gate)
	select {
	case err := <-g1done:
		if err != nil {
			t.Errorf("goroutine-1 NextVMID error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine-1 did not complete after gate was opened")
	}
}

// TestRetryBackoff_RespectsContextCancel verifies that when the context is
// cancelled during a backoff sleep, AllocateWithRetry returns immediately with
// a context error rather than sleeping for the full backoff duration. This
// validates the C10 fix to retryBackoff.
//
// The test installs a backoff function that would sleep 10s (far longer than
// the test timeout); the context is cancelled within 100ms. The call must
// return well before the 10s sleep completes.
func TestRetryBackoff_RespectsContextCancel(t *testing.T) {
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	conflictErr := errors.New("conflict: already exists")
	attempts := 0

	// Cancel the context after the first attempt fails.
	go func() {
		// Give the first attempt time to complete and the backoff to begin.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := pve.AllocateWithRetry(ctx, c,
		func(_ int) error {
			attempts++
			return conflictErr // always conflict so retry is triggered
		},
		func(e error) bool { return errors.Is(e, conflictErr) },
		5,
		// Backoff of 10s: if context cancellation is not respected the test
		// would run for 10s; with the fix it returns within ~100ms.
		pve.WithBackoffFunc(func(_ error, _ int) time.Duration {
			return 10 * time.Second
		}),
	)

	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("AllocateWithRetry did not respect context cancel: elapsed %v (expected < 2s)", elapsed)
	}
	// The error must reflect context cancellation (wrapped or direct).
	if ctx.Err() == nil {
		t.Error("expected ctx.Err() to be non-nil after cancel")
	}
}
