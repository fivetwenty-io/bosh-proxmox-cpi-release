package pve_test

import (
	"errors"
	"net"
	"testing"

	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// makeAPIErr builds an SDK error via ParseAPIError (which sets the sentinel and
// HTTPCode correctly) so errors.Is(err, sdkerrors.ErrNotFound) etc. work.
func makeAPIErr(httpCode int, msg string) error {
	body := []byte(`{"message":"` + msg + `","code":` + itoa(httpCode) + `}`)
	return sdkerrors.ParseAPIError(httpCode, body)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// fakeNetError satisfies net.Error with configurable Timeout/Temporary.
type fakeNetError struct {
	timeout   bool
	temporary bool
}

func (e *fakeNetError) Error() string   { return "fake net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return e.temporary }

// Ensure fakeNetError satisfies net.Error at compile time.
var _ net.Error = (*fakeNetError)(nil)

// ---------------------------------------------------------------------------
// WrapError
// ---------------------------------------------------------------------------

func TestWrapError_Nil(t *testing.T) {
	if got := pve.WrapError(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapError_404(t *testing.T) {
	err := makeAPIErr(404, "not found")
	got := pve.WrapError(err)
	if got == nil {
		t.Fatal("expected non-nil error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", got, got)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("want TypeCloud got %q", cpiErr.Type())
	}
	if cpiErr.OkToRetry() {
		t.Error("404 should not be retriable")
	}
}

func TestWrapError_5xx(t *testing.T) {
	err := makeAPIErr(500, "internal server error")
	got := pve.WrapError(err)
	if got == nil {
		t.Fatal("expected non-nil error")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", got, got)
	}
	if cpiErr.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("want TypeRetriableCloud got %q", cpiErr.Type())
	}
	if !cpiErr.OkToRetry() {
		t.Error("5xx should be retriable")
	}
}

func TestWrapError_503(t *testing.T) {
	err := makeAPIErr(503, "service unavailable")
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if !cpiErr.OkToRetry() {
		t.Error("503 should be retriable")
	}
}

func TestWrapError_4xxNon404(t *testing.T) {
	for _, code := range []int{400, 403, 409, 422} {
		err := makeAPIErr(code, "client error")
		got := pve.WrapError(err)
		var cpiErr *cpierrors.Error
		if !errors.As(got, &cpiErr) {
			t.Fatalf("code %d: expected *cpierrors.Error, got %T", code, got)
		}
		if cpiErr.Type() != cpierrors.TypeCloud {
			t.Errorf("code %d: want TypeCloud got %q", code, cpiErr.Type())
		}
		if cpiErr.OkToRetry() {
			t.Errorf("code %d: should not be retriable", code)
		}
	}
}

func TestWrapError_Timeout_NetError(t *testing.T) {
	err := &fakeNetError{timeout: true}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if !cpiErr.OkToRetry() {
		t.Error("network timeout should be retriable")
	}
}

func TestWrapError_NonTimeout_NetError(t *testing.T) {
	// net.Error that is not a timeout should fall through as non-retriable CloudError.
	err := &fakeNetError{timeout: false}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	// Non-timeout net.Error is a generic error → CloudError, non-retriable.
	if cpiErr.OkToRetry() {
		t.Error("non-timeout net.Error should not be retriable")
	}
}

func TestWrapError_ConnectionError(t *testing.T) {
	err := &sdkerrors.ConnectionError{
		Host:    "pve.example.com",
		Port:    8006,
		Message: "refused",
	}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if !cpiErr.OkToRetry() {
		t.Error("ConnectionError should be retriable")
	}
}

func TestWrapError_SDKTimeoutError(t *testing.T) {
	err := &sdkerrors.TimeoutError{
		Operation: "get-config",
		Duration:  "30s",
	}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if !cpiErr.OkToRetry() {
		t.Error("TimeoutError should be retriable")
	}
}

func TestWrapError_Generic(t *testing.T) {
	err := errors.New("some untyped error")
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.OkToRetry() {
		t.Error("generic error should not be retriable")
	}
}

// ---------------------------------------------------------------------------
// WrapNotFoundVM
// ---------------------------------------------------------------------------

func TestWrapNotFoundVM_Nil(t *testing.T) {
	if got := pve.WrapNotFoundVM(nil, "vm-100"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapNotFoundVM_404(t *testing.T) {
	err := makeAPIErr(404, "vm not found")
	got := pve.WrapNotFoundVM(err, "vm-100")
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("want TypeVMNotFound got %q", cpiErr.Type())
	}
	// Ensure vm CID is embedded in message.
	if cpiErr.Error() == "" {
		t.Error("error message must not be empty")
	}
}

func TestWrapNotFoundVM_Other(t *testing.T) {
	err := makeAPIErr(500, "server error")
	got := pve.WrapNotFoundVM(err, "vm-100")
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	// 500 passes through WrapError → retriable.
	if !cpiErr.OkToRetry() {
		t.Error("5xx passed through WrapNotFoundVM should be retriable")
	}
}

// ---------------------------------------------------------------------------
// WrapNotFoundDisk
// ---------------------------------------------------------------------------

func TestWrapNotFoundDisk_Nil(t *testing.T) {
	if got := pve.WrapNotFoundDisk(nil, "local:vol"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapNotFoundDisk_404(t *testing.T) {
	err := makeAPIErr(404, "disk not found")
	got := pve.WrapNotFoundDisk(err, "local:vm-100-disk-1")
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.Type() != cpierrors.TypeDiskNotFound {
		t.Errorf("want TypeDiskNotFound got %q", cpiErr.Type())
	}
}

func TestWrapNotFoundDisk_Other(t *testing.T) {
	err := errors.New("generic error")
	got := pve.WrapNotFoundDisk(err, "local:disk")
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("want TypeCloud got %q", cpiErr.Type())
	}
}

// ---------------------------------------------------------------------------
// IsNotFound
// ---------------------------------------------------------------------------

func TestIsNotFound_NilFalse(t *testing.T) {
	if pve.IsNotFound(nil) {
		t.Error("nil should not be not-found")
	}
}

func TestIsNotFound_SDK404True(t *testing.T) {
	err := makeAPIErr(404, "not found")
	if !pve.IsNotFound(err) {
		t.Errorf("SDK 404 should be IsNotFound=true, got false; err=%v", err)
	}
}

func TestIsNotFound_SDK500False(t *testing.T) {
	err := makeAPIErr(500, "server error")
	if pve.IsNotFound(err) {
		t.Error("SDK 500 should not be not-found")
	}
}

func TestIsNotFound_VMNotFoundTrue(t *testing.T) {
	err := cpierrors.VMNotFound("vm-100")
	if !pve.IsNotFound(err) {
		t.Error("VMNotFound should be IsNotFound=true")
	}
}

func TestIsNotFound_DiskNotFoundTrue(t *testing.T) {
	err := cpierrors.DiskNotFound("local:disk")
	if !pve.IsNotFound(err) {
		t.Error("DiskNotFound should be IsNotFound=true")
	}
}

func TestIsNotFound_GenericFalse(t *testing.T) {
	err := errors.New("something else")
	if pve.IsNotFound(err) {
		t.Error("generic error should not be not-found")
	}
}

// ---------------------------------------------------------------------------
// IsVMIDConflict
// ---------------------------------------------------------------------------

func TestIsVMIDConflict_Nil(t *testing.T) {
	if pve.IsVMIDConflict(nil) {
		t.Error("nil should not be a VMID conflict")
	}
}

func TestIsVMIDConflict_HTTP409(t *testing.T) {
	err := makeAPIErr(409, "vmid is in use")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("HTTP 409 APIError should be VMID conflict, got false; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP500AlreadyExists(t *testing.T) {
	err := makeAPIErr(500, "unable to create VM 113 - VM 113 already exists on node 'pve'")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("500 with 'already exists' should be VMID conflict, got false; err=%v", err)
	}
}

func TestIsVMIDConflict_MixedCase(t *testing.T) {
	err := makeAPIErr(500, "Volume Already Exists")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("mixed-case 'Already Exists' should be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP500Unrelated(t *testing.T) {
	err := makeAPIErr(500, "lvm thin pool out of space")
	if pve.IsVMIDConflict(err) {
		t.Errorf("unrelated 500 should not be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP404(t *testing.T) {
	err := makeAPIErr(404, "vm not found")
	if pve.IsVMIDConflict(err) {
		t.Errorf("404 should not be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_PlainAlreadyExists(t *testing.T) {
	err := errors.New("already exists")
	if !pve.IsVMIDConflict(err) {
		t.Error("plain error with 'already exists' should be VMID conflict")
	}
}
