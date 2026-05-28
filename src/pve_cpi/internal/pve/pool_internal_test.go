// pool_internal_test.go — white-box tests for sdkPoolService.AddVM.
// Uses package pve (internal) so sdkPoolService can be constructed directly.
package pve

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
	sdkmetrics "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/metrics"
)

// fakeRawClient is a minimal sdkclient.Client implementation for unit tests.
// All methods panic unless explicitly overridden — this forces the test to
// control exactly which SDK calls fire. Only PutCtx is relevant for AddVM.
//
// Why not embed sdkclient.Client directly: the interface has ~20 methods;
// embedding and calling an unimplemented method would nil-deref, making
// test failures confusing. Explicit panics give a clear "unexpected call"
// signal.
type fakeRawClient struct {
	putCtxFn func(ctx context.Context, path string, params map[string]interface{}) (interface{}, error)
}

// PutCtx is the only SDK method AddVM exercises after input validation.
func (f *fakeRawClient) PutCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	if f.putCtxFn != nil {
		return f.putCtxFn(ctx, path, params)
	}
	return nil, nil
}

// The remaining sdkclient.Client methods are intentionally unimplemented.
// A test that accidentally triggers one of these will panic with a clear message.

func (f *fakeRawClient) Get(_ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: Get not implemented in this test")
}
func (f *fakeRawClient) GetRaw(_ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: GetRaw not implemented in this test")
}
func (f *fakeRawClient) Post(_ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: Post not implemented in this test")
}
func (f *fakeRawClient) PostRaw(_ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: PostRaw not implemented in this test")
}
func (f *fakeRawClient) Put(_ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: Put not implemented in this test")
}
func (f *fakeRawClient) PutRaw(_ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: PutRaw not implemented in this test")
}
func (f *fakeRawClient) Delete(_ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: Delete not implemented in this test")
}
func (f *fakeRawClient) DeleteRaw(_ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: DeleteRaw not implemented in this test")
}
func (f *fakeRawClient) GetCtx(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: GetCtx not implemented in this test")
}
func (f *fakeRawClient) GetRawCtx(_ context.Context, _ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: GetRawCtx not implemented in this test")
}
func (f *fakeRawClient) PostCtx(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: PostCtx not implemented in this test")
}
func (f *fakeRawClient) PostRawCtx(_ context.Context, _ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: PostRawCtx not implemented in this test")
}
func (f *fakeRawClient) PutRawCtx(_ context.Context, _ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: PutRawCtx not implemented in this test")
}
func (f *fakeRawClient) DeleteCtx(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
	panic("fakeRawClient: DeleteCtx not implemented in this test")
}
func (f *fakeRawClient) DeleteRawCtx(_ context.Context, _ string, _ map[string]interface{}) (*sdkclient.Response, error) {
	panic("fakeRawClient: DeleteRawCtx not implemented in this test")
}
func (f *fakeRawClient) UploadCtx(_ context.Context, _ string, _ map[string]string, _, _ string, _ io.Reader) (*sdkclient.Response, error) {
	panic("fakeRawClient: UploadCtx not implemented in this test")
}
func (f *fakeRawClient) Login() error  { panic("fakeRawClient: Login not implemented in this test") }
func (f *fakeRawClient) Logout() error { panic("fakeRawClient: Logout not implemented in this test") }
func (f *fakeRawClient) UpdateTicket(_ string) {
	panic("fakeRawClient: UpdateTicket not implemented in this test")
}
func (f *fakeRawClient) UpdateCSRFToken(_ string) {
	panic("fakeRawClient: UpdateCSRFToken not implemented in this test")
}
func (f *fakeRawClient) SetTimeout(_ time.Duration) {
	panic("fakeRawClient: SetTimeout not implemented in this test")
}
func (f *fakeRawClient) SetKeepAlive(_ int) {
	panic("fakeRawClient: SetKeepAlive not implemented in this test")
}
func (f *fakeRawClient) SetLogger(_ sdkclient.Logger) {
	panic("fakeRawClient: SetLogger not implemented in this test")
}
func (f *fakeRawClient) SetLogConfig(_ sdkclient.LogConfig) {
	panic("fakeRawClient: SetLogConfig not implemented in this test")
}
func (f *fakeRawClient) AddLogHook(_ sdkclient.Hook) {
	panic("fakeRawClient: AddLogHook not implemented in this test")
}
func (f *fakeRawClient) GetLogConfig() sdkclient.LogConfig {
	panic("fakeRawClient: GetLogConfig not implemented in this test")
}
func (f *fakeRawClient) SetMetrics(_ *sdkmetrics.DefaultMetrics) {
	panic("fakeRawClient: SetMetrics not implemented in this test")
}
func (f *fakeRawClient) SetTFAHandler(_ sdkclient.TFAHandler) {
	panic("fakeRawClient: SetTFAHandler not implemented in this test")
}
func (f *fakeRawClient) InvalidateCache(_ string) int {
	panic("fakeRawClient: InvalidateCache not implemented in this test")
}
func (f *fakeRawClient) ClearCache() {
	panic("fakeRawClient: ClearCache not implemented in this test")
}
func (f *fakeRawClient) CacheStats() *sdkclient.CacheStats {
	panic("fakeRawClient: CacheStats not implemented in this test")
}

// Verify fakeRawClient satisfies sdkclient.Client at compile time.
var _ sdkclient.Client = (*fakeRawClient)(nil)

// TestSDKPoolService_AddVM_EmptyPoolID verifies that an empty poolID returns
// a validation error without calling PutCtx.
func TestSDKPoolService_AddVM_EmptyPoolID(t *testing.T) {
	t.Parallel()

	var putCalled bool
	svc := &sdkPoolService{
		raw: &fakeRawClient{
			putCtxFn: func(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
				putCalled = true
				return nil, nil
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
	if putCalled {
		t.Error("PutCtx must NOT be called when poolID is empty")
	}
}

// TestSDKPoolService_AddVM_NegativeVMID verifies that vmid <= 0 returns a
// validation error without calling PutCtx.
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

			var putCalled bool
			svc := &sdkPoolService{
				raw: &fakeRawClient{
					putCtxFn: func(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
						putCalled = true
						return nil, nil
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
			if putCalled {
				t.Errorf("vmid=%d: PutCtx must NOT be called on invalid vmid", tc.vmid)
			}
		})
	}
}

// TestSDKPoolService_AddVM_ValidCallsRawPut verifies that valid inputs reach
// PutCtx with the correct path ("/pools/<poolID>") and params (vms="<vmid>").
func TestSDKPoolService_AddVM_ValidCallsRawPut(t *testing.T) {
	t.Parallel()

	const poolID = "bosh-stemcells"
	const vmid = int64(7001)

	var capturedPath string
	var capturedParams map[string]interface{}

	svc := &sdkPoolService{
		raw: &fakeRawClient{
			putCtxFn: func(_ context.Context, path string, params map[string]interface{}) (interface{}, error) {
				capturedPath = path
				capturedParams = params
				return nil, nil
			},
		},
	}

	err := svc.AddVM(context.Background(), poolID, vmid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPath := "/pools/" + poolID
	if capturedPath != wantPath {
		t.Errorf("PutCtx path = %q; want %q", capturedPath, wantPath)
	}
	wantVMs := "7001"
	if got, ok := capturedParams["vms"].(string); !ok || got != wantVMs {
		t.Errorf("PutCtx params[vms] = %v; want %q", capturedParams["vms"], wantVMs)
	}
}

// TestSDKPoolService_AddVM_PutCtxError verifies that a PutCtx error is wrapped
// and returned to the caller (not swallowed).
func TestSDKPoolService_AddVM_PutCtxError(t *testing.T) {
	t.Parallel()

	rawErr := errors.New("PVE: pool bosh-stemcells not found (500)")
	svc := &sdkPoolService{
		raw: &fakeRawClient{
			putCtxFn: func(_ context.Context, _ string, _ map[string]interface{}) (interface{}, error) {
				return nil, rawErr
			},
		},
	}

	err := svc.AddVM(context.Background(), "bosh-stemcells", 7002)
	if err == nil {
		t.Fatal("expected error from PutCtx; got nil")
	}
	if !errors.Is(err, rawErr) && !strings.Contains(err.Error(), rawErr.Error()) {
		t.Errorf("error %q does not wrap or contain raw error %q", err.Error(), rawErr.Error())
	}
}
