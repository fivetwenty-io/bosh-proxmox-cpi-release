package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

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

func TestNextVMID_LowestAvailable(t *testing.T) {
	// used: 100, 101, 103 → first free is 102
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(100, 101, 103), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 102 {
		t.Errorf("expected VMID 102, got %d", id)
	}
}

func TestNextVMID_EmptyCluster(t *testing.T) {
	// No VMs → lowest is rangeStart (100).
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != pve.VMIDRangeVMStart {
		t.Errorf("expected %d, got %d", pve.VMIDRangeVMStart, id)
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
	// Range [200,250], used [200,201,202] → expect 203.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(200, 201, 202), nil
	})

	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 250))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 203 {
		t.Errorf("expected 203, got %d", id)
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
	if id != 9001 {
		t.Errorf("expected 9001, got %d", id)
	}
}

func TestNextVMID_Concurrency(t *testing.T) {
	// 100 goroutines call NextVMID with a shared cluster list that starts empty.
	// The globalVMIDMu means each goroutine serialises; with an empty list and range
	// [100,4999] all 100 goroutines will return 100 unless the list changes between calls.
	// We track call count and verify no data race (run with -race).
	//
	// Because we use a process-global mutex, all goroutines will call ListResources
	// serially. The mock always returns an empty set; every goroutine gets 100.
	// That is correct per the documented contract: cross-process races are handled by
	// PVE conflict → caller retry; within-process the mutex prevents data races.

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
	for id := range results {
		// All should be 100 (empty cluster, lowest start).
		if id != pve.VMIDRangeVMStart {
			t.Errorf("expected %d, got %d", pve.VMIDRangeVMStart, id)
		}
	}

	if atomic.LoadInt64(&callCount) != goroutines {
		t.Errorf("expected %d ListResources calls (one per goroutine), got %d", goroutines, callCount)
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
	if id != 9001 {
		t.Errorf("expected 9001, got %d", id)
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
	if id != 9001 {
		t.Errorf("expected 9001 (9000 used by disk volume), got %d", id)
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
	if id != pve.VMIDRangeVMStart {
		t.Errorf("expected %d, got %d", pve.VMIDRangeVMStart, id)
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
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})

	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(5000, 200)) // invalid: start>end
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With invalid range ignored, default VMIDRangeVMStart applies.
	if id != pve.VMIDRangeVMStart {
		t.Errorf("expected default %d, got %d", pve.VMIDRangeVMStart, id)
	}
}

func TestNextVMID_MalformedJSONSkipped(t *testing.T) {
	// One malformed JSON entry in ListResources; should be skipped, not error.
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
	// 101 used, 100 is free.
	if id != 100 {
		t.Errorf("expected 100, got %d", id)
	}
}

func TestNextVMID_GapAtStart(t *testing.T) {
	// used: 101 only; first free is 100 (rangeStart).
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(101), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 100 {
		t.Errorf("expected 100, got %d", id)
	}
}

func TestNextVMID_VmidsOutsideRangeIgnored(t *testing.T) {
	// VMIDs in the disk range (9000+) must not block the VM range.
	c := newVMIDClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(9000, 9001), nil
	})

	id, err := pve.NextVMID(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != pve.VMIDRangeVMStart {
		t.Errorf("expected %d, got %d", pve.VMIDRangeVMStart, id)
	}
}

func TestVMIDRange_VMEndIs5999(t *testing.T) {
	if pve.VMIDRangeVMEnd != 5999 {
		t.Errorf("VMIDRangeVMEnd: expected 5999, got %d", pve.VMIDRangeVMEnd)
	}
}

// TestNextVMID_5500Allocatable verifies that VMID 5500, which was formerly in the
// stemcell sub-range [5000,5999], is now allocatable as a regular VM VMID.
func TestNextVMID_5500Allocatable(t *testing.T) {
	// Fill 100..5499; 5500 must be the first free VMID.
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
	if id != 5500 {
		t.Errorf("expected 5500 (first free after filling 100-5499), got %d", id)
	}
	if id < pve.VMIDRangeVMStart || id > pve.VMIDRangeVMEnd {
		t.Errorf("VMID %d outside VM range [%d,%d]", id, pve.VMIDRangeVMStart, pve.VMIDRangeVMEnd)
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

func TestNextVMID_VmidNullField(t *testing.T) {
	// JSON entry where vmid is null — should be skipped (pointer nil).
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
	// 100 is used; next is 101.
	if id != 101 {
		t.Errorf("expected 101, got %d", id)
	}
}
