// pool_internal_test.go — white-box tests for sdkPoolService.AddVM and
// GetPoolComment.
// Uses package pve (internal) so sdkPoolService can be constructed directly.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/pools"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// fakePoolsService implements pools.Service for unit tests.
// Only UpdatePools2 is wired; all other methods panic on call.
type fakePoolsService struct {
	updatePools2Fn func(ctx context.Context, poolid string, params *pools.UpdatePools2Params) error
	getPoolsFn     func(ctx context.Context, poolid string, params *pools.GetPoolsParams) (*pools.GetPoolsResponse, error)
}

func (f *fakePoolsService) UpdatePools2(ctx context.Context, poolid string, params *pools.UpdatePools2Params) error {
	if f.updatePools2Fn != nil {
		return f.updatePools2Fn(ctx, poolid, params)
	}
	return nil
}

func (f *fakePoolsService) ListPools(_ context.Context, _ *pools.ListPoolsParams) (*pools.ListPoolsResponse, error) {
	panic("fakePoolsService: ListPools unexpected call")
}

func (f *fakePoolsService) CreatePools(_ context.Context, _ *pools.CreatePoolsParams) error {
	panic("fakePoolsService: CreatePools unexpected call")
}

func (f *fakePoolsService) DeletePools(_ context.Context, _ *pools.DeletePoolsParams) error {
	panic("fakePoolsService: DeletePools unexpected call")
}

func (f *fakePoolsService) GetPools(ctx context.Context, poolid string, params *pools.GetPoolsParams) (*pools.GetPoolsResponse, error) {
	if f.getPoolsFn != nil {
		return f.getPoolsFn(ctx, poolid, params)
	}
	panic("fakePoolsService: GetPools unexpected call")
}

func (f *fakePoolsService) UpdatePools(_ context.Context, _ *pools.UpdatePoolsParams) error {
	panic("fakePoolsService: UpdatePools unexpected call")
}

func (f *fakePoolsService) DeletePools2(_ context.Context, _ string) error {
	panic("fakePoolsService: DeletePools2 unexpected call")
}

// Compile-time check.
var _ pools.Service = (*fakePoolsService)(nil)

// TestSDKPoolService_AddVM_EmptyPoolID verifies that an empty poolID returns
// a validation error without calling UpdatePools2.
func TestSDKPoolService_AddVM_EmptyPoolID(t *testing.T) {
	t.Parallel()

	var called bool
	svc := &sdkPoolService{
		svc: &fakePoolsService{
			updatePools2Fn: func(_ context.Context, _ string, _ *pools.UpdatePools2Params) error {
				called = true
				return nil
			},
		},
	}

	err := svc.AddVM(context.Background(), "", 100)
	if err == nil {
		t.Fatal("expected error for empty poolID; got nil")
	}
	if !strings.Contains(err.Error(), "poolID") {
		t.Errorf("error %q does not mention poolID", err.Error())
	}
	if called {
		t.Error("UpdatePools2 must NOT be called when poolID is empty")
	}
}

// TestSDKPoolService_AddVM_NegativeVMID verifies that vmid <= 0 returns a
// validation error without calling UpdatePools2.
func TestSDKPoolService_AddVM_NegativeVMID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		vmid int64
	}{
		{"zero", 0},
		{"negative", -1},
		{"large negative", -9999},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var called bool
			svc := &sdkPoolService{
				svc: &fakePoolsService{
					updatePools2Fn: func(_ context.Context, _ string, _ *pools.UpdatePools2Params) error {
						called = true
						return nil
					},
				},
			}

			err := svc.AddVM(context.Background(), "bosh-pool", tc.vmid)
			if err == nil {
				t.Fatalf("vmid=%d: expected error; got nil", tc.vmid)
			}
			if !strings.Contains(err.Error(), "vmid") {
				t.Errorf("vmid=%d: error %q does not mention vmid", tc.vmid, err.Error())
			}
			if called {
				t.Errorf("vmid=%d: UpdatePools2 must NOT be called on invalid vmid", tc.vmid)
			}
		})
	}
}

// TestSDKPoolService_AddVM_ValidCallsUpdatePools2 verifies that valid inputs
// reach UpdatePools2 with the correct poolid and vms="<vmid>".
func TestSDKPoolService_AddVM_ValidCallsUpdatePools2(t *testing.T) {
	t.Parallel()

	const poolID = "bosh-stemcells"
	const vmid = int64(7001)

	var capturedPoolID string
	var capturedParams *pools.UpdatePools2Params

	svc := &sdkPoolService{
		svc: &fakePoolsService{
			updatePools2Fn: func(_ context.Context, pid string, params *pools.UpdatePools2Params) error {
				capturedPoolID = pid
				capturedParams = params
				return nil
			},
		},
	}

	err := svc.AddVM(context.Background(), poolID, vmid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedPoolID != poolID {
		t.Errorf("UpdatePools2 poolid = %q; want %q", capturedPoolID, poolID)
	}
	if capturedParams == nil {
		t.Fatal("UpdatePools2 params is nil")
	}
	if capturedParams.Vms == nil {
		t.Fatal("UpdatePools2 params.Vms is nil")
	}
	const wantVMs = "7001"
	if *capturedParams.Vms != wantVMs {
		t.Errorf("UpdatePools2 params.Vms = %q; want %q", *capturedParams.Vms, wantVMs)
	}
}

// TestSDKPoolService_AddVM_UpdatePools2Error verifies that an UpdatePools2 error
// is wrapped and returned to the caller (not swallowed).
func TestSDKPoolService_AddVM_UpdatePools2Error(t *testing.T) {
	t.Parallel()

	rawErr := errors.New("PVE: pool bosh-stemcells not found (500)")
	svc := &sdkPoolService{
		svc: &fakePoolsService{
			updatePools2Fn: func(_ context.Context, _ string, _ *pools.UpdatePools2Params) error {
				return rawErr
			},
		},
	}

	err := svc.AddVM(context.Background(), "bosh-stemcells", 7002)
	if err == nil {
		t.Fatal("expected error from UpdatePools2; got nil")
	}
	if !errors.Is(err, rawErr) && !strings.Contains(err.Error(), rawErr.Error()) {
		t.Errorf("error %q does not wrap or contain raw error %q", err.Error(), rawErr.Error())
	}
}

// TestSDKPoolService_GetPoolComment_LiveNotFoundTextIsAbsent covers the live
// PVE 9.2 not-found shape for a pool read: HTTP 500 with the body text
// "pool 'x' does not exist", never a 404. GetPoolComment must map it to
// (found=false, err=nil) — a pool the CPI has not created yet is the normal
// pre-lazy-creation state, and reporting it as an error made every first boot
// against the shipped default pools log a spurious transient warning.
func TestSDKPoolService_GetPoolComment_LiveNotFoundTextIsAbsent(t *testing.T) {
	t.Parallel()

	svc := &sdkPoolService{
		svc: &fakePoolsService{
			getPoolsFn: func(_ context.Context, _ string, _ *pools.GetPoolsParams) (*pools.GetPoolsResponse, error) {
				return nil, sdkerrors.ParseAPIError(500,
					[]byte(`{"message":"pool 'v2-templates' does not exist","code":0}`))
			},
		},
	}

	comment, found, err := svc.GetPoolComment(context.Background(), "v2-templates")
	if err != nil {
		t.Fatalf("a pool that does not exist must not surface as an error: %v", err)
	}
	if found {
		t.Error("found = true; want false for a pool that does not exist")
	}
	if comment != "" {
		t.Errorf("comment = %q; want empty", comment)
	}
}

// TestSDKPoolService_GetPoolComment_UnrelatedErrorPropagates keeps the mapping
// fail-closed: only a positively identified not-found becomes absent. Any
// other fault must still reach the caller so a genuine API problem is not
// mistaken for a missing pool.
func TestSDKPoolService_GetPoolComment_UnrelatedErrorPropagates(t *testing.T) {
	t.Parallel()

	svc := &sdkPoolService{
		svc: &fakePoolsService{
			getPoolsFn: func(_ context.Context, _ string, _ *pools.GetPoolsParams) (*pools.GetPoolsResponse, error) {
				return nil, sdkerrors.ParseAPIError(500,
					[]byte(`{"message":"storage 'x' does not exist","code":0}`))
			},
		},
	}

	if _, _, err := svc.GetPoolComment(context.Background(), "bosh"); err == nil {
		t.Fatal("an unrelated 500 must propagate, not be treated as an absent pool")
	}
}
