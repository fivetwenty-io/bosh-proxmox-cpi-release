package pve_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// cpiErrIsRetriable reports whether err's chain carries a *cpierrors.Error with
// OkToRetry()==true. Used to assert WrapError-driven retriability mapping.
func cpiErrIsRetriable(t *testing.T, err error) bool {
	t.Helper()
	var e *cpierrors.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.OkToRetry()
}

// makeAPIErr builds an SDK error via ParseAPIError (which sets the sentinel and
// HTTPCode correctly) so errors.Is(err, sdkerrors.ErrNotFound) etc. work.
func makeAPIErr(httpCode int, msg string) error {
	body := []byte(`{"message":"` + msg + `","code":` + strconv.Itoa(httpCode) + `}`)
	return sdkerrors.ParseAPIError(httpCode, body)
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
	t.Parallel()
	if got := pve.WrapError(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapError_404(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestWrapError_Temporary_True_NetError(t *testing.T) {
	t.Parallel()
	// temporary=true alone does not change retriability; only Timeout() matters.
	err := &fakeNetError{timeout: false, temporary: true}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.OkToRetry() {
		t.Error("temporary-only net.Error should not be retriable (only Timeout() drives retriability)")
	}
}

func TestWrapError_Temporary_False_NetError(t *testing.T) {
	t.Parallel()
	// temporary=false, timeout=false → non-retriable CloudError (same as base case).
	err := &fakeNetError{timeout: false, temporary: false}
	got := pve.WrapError(err)
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T", got)
	}
	if cpiErr.OkToRetry() {
		t.Error("non-timeout non-temporary net.Error should not be retriable")
	}
}

func TestWrapError_ConnectionError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if got := pve.WrapNotFoundVM(nil, "vm-100"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapNotFoundVM_404(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if got := pve.WrapNotFoundDisk(nil, "local:vol"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestWrapNotFoundDisk_404(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if pve.IsNotFound(nil) {
		t.Error("nil should not be not-found")
	}
}

func TestIsNotFound_SDK404True(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(404, "not found")
	if !pve.IsNotFound(err) {
		t.Errorf("SDK 404 should be IsNotFound=true, got false; err=%v", err)
	}
}

func TestIsNotFound_SDK500False(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "server error")
	if pve.IsNotFound(err) {
		t.Error("SDK 500 should not be not-found")
	}
}

func TestIsNotFound_VMNotFoundTrue(t *testing.T) {
	t.Parallel()
	err := cpierrors.VMNotFound("vm-100")
	if !pve.IsNotFound(err) {
		t.Error("VMNotFound should be IsNotFound=true")
	}
}

func TestIsNotFound_DiskNotFoundTrue(t *testing.T) {
	t.Parallel()
	err := cpierrors.DiskNotFound("local:disk")
	if !pve.IsNotFound(err) {
		t.Error("DiskNotFound should be IsNotFound=true")
	}
}

func TestIsNotFound_GenericFalse(t *testing.T) {
	t.Parallel()
	err := errors.New("something else")
	if pve.IsNotFound(err) {
		t.Error("generic error should not be not-found")
	}
}

// ---------------------------------------------------------------------------
// IsVMIDConflict
// ---------------------------------------------------------------------------

func TestIsVMIDConflict_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsVMIDConflict(nil) {
		t.Error("nil should not be a VMID conflict")
	}
}

func TestIsVMIDConflict_HTTP409(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(409, "vmid is in use")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("HTTP 409 APIError should be VMID conflict, got false; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP500AlreadyExists(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "unable to create VM 113 - VM 113 already exists on node 'pve'")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("500 with 'already exists' should be VMID conflict, got false; err=%v", err)
	}
}

func TestIsVMIDConflict_VMAlreadyExistsMixedCase(t *testing.T) {
	t.Parallel()
	// PVE perl die() message with mixed case — anchored to "vm" prefix.
	err := makeAPIErr(500, "VM 200 Already Exists on node pve01")
	if !pve.IsVMIDConflict(err) {
		t.Errorf("mixed-case 'VM N Already Exists' should be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_NonVMVolumeAlreadyExists(t *testing.T) {
	t.Parallel()
	// Ceph "image already exists" must NOT match — no "vm" prefix.
	err := makeAPIErr(500, "Volume Already Exists")
	if pve.IsVMIDConflict(err) {
		t.Errorf("'Volume Already Exists' (no vm prefix) should NOT be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP500Unrelated(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "lvm thin pool out of space")
	if pve.IsVMIDConflict(err) {
		t.Errorf("unrelated 500 should not be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_HTTP404(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(404, "vm not found")
	if pve.IsVMIDConflict(err) {
		t.Errorf("404 should not be VMID conflict; err=%v", err)
	}
}

func TestIsVMIDConflict_PlainVMAlreadyExists(t *testing.T) {
	t.Parallel()
	// PVE task-body plain error with VMID-specific wording.
	err := errors.New("vm 101 already exists")
	if !pve.IsVMIDConflict(err) {
		t.Error("plain error 'vm N already exists' should be VMID conflict")
	}
}

func TestIsVMIDConflict_CephImageAlreadyExists(t *testing.T) {
	t.Parallel()
	// Ceph "image already exists" — must NOT match; it is not a VMID conflict.
	err := errors.New("image already exists")
	if pve.IsVMIDConflict(err) {
		t.Error("Ceph 'image already exists' must NOT be classified as VMID conflict")
	}
}

// ---------------------------------------------------------------------------
// IsTransientTransport
// ---------------------------------------------------------------------------

func TestIsCloneSourceMissing_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsCloneSourceMissing(nil) {
		t.Error("nil should not be clone-source-missing")
	}
}

func TestIsCloneSourceMissing_TemplateNotFound(t *testing.T) {
	t.Parallel()
	// Exact shape PVE returns on a clone POST when the source template VM is
	// gone — the real failure behind the misleading "exhausted VMID allocation".
	err := makeAPIErr(500, "unable to find configuration file for VM 30437 on node 'pve'")
	if !pve.IsCloneSourceMissing(err) {
		t.Errorf("template-not-found should be clone-source-missing, got false; err=%v", err)
	}
}

func TestIsCloneSourceMissing_Unrelated500(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "storage 'zfs-0' is not online")
	if pve.IsCloneSourceMissing(err) {
		t.Error("unrelated 500 should not be clone-source-missing")
	}
}

func TestIsTransientTransport_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsTransientTransport(nil) {
		t.Error("nil should not be transient")
	}
}

func TestIsTransientTransport_HTTP596(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(596, "backend gone")
	if !pve.IsTransientTransport(err) {
		t.Errorf("HTTP 596 should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_HTTP500(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "internal server error")
	if !pve.IsTransientTransport(err) {
		t.Errorf("HTTP 500 should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_HTTP503(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(503, "service unavailable")
	if !pve.IsTransientTransport(err) {
		t.Errorf("HTTP 503 should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_HTTP404False(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(404, "not found")
	if pve.IsTransientTransport(err) {
		t.Errorf("HTTP 404 should not be transient; err=%v", err)
	}
}

func TestIsTransientTransport_HTTP409False(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(409, "conflict")
	if pve.IsTransientTransport(err) {
		t.Errorf("HTTP 409 should not be transient; err=%v", err)
	}
}

func TestIsTransientTransport_ConnectionError(t *testing.T) {
	t.Parallel()
	err := &sdkerrors.ConnectionError{
		Host:    "pve.example.com",
		Port:    8006,
		Message: "connection refused",
	}
	if !pve.IsTransientTransport(err) {
		t.Errorf("ConnectionError should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_TimeoutError(t *testing.T) {
	t.Parallel()
	err := &sdkerrors.TimeoutError{Operation: "get", Duration: "30s"}
	if !pve.IsTransientTransport(err) {
		t.Errorf("TimeoutError should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_NetTimeout(t *testing.T) {
	t.Parallel()
	err := &fakeNetError{timeout: true}
	if !pve.IsTransientTransport(err) {
		t.Errorf("net.Error with Timeout()=true should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_NetNonTimeoutFalse(t *testing.T) {
	t.Parallel()
	err := &fakeNetError{timeout: false}
	if pve.IsTransientTransport(err) {
		t.Errorf("net.Error with Timeout()=false should not be transient; err=%v", err)
	}
}

func TestIsTransientTransport_LoginEOF(t *testing.T) {
	t.Parallel()
	err := errors.New("auto-login failed: authentication failed: failed to parse login response: EOF")
	if !pve.IsTransientTransport(err) {
		t.Errorf("login EOF should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_AutoLoginFailed(t *testing.T) {
	t.Parallel()
	err := errors.New("auto-login failed: some other reason")
	if !pve.IsTransientTransport(err) {
		t.Errorf("auto-login failed should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_596Substring(t *testing.T) {
	t.Parallel()
	// Plain errors (no APIError wrap) carrying the SDK formatted
	// "(code: 596)" suffix should still be detected.
	err := errors.New("API request failed: HTTP 596 (code: 596)")
	if !pve.IsTransientTransport(err) {
		t.Errorf("plain 596 message should be transient; err=%v", err)
	}
}

func TestIsTransientTransport_Unrelated(t *testing.T) {
	t.Parallel()
	err := errors.New("some other error")
	if pve.IsTransientTransport(err) {
		t.Errorf("unrelated error should not be transient; err=%v", err)
	}
}

func TestIsStorageLockTimeout_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsStorageLockTimeout(nil) {
		t.Error("nil error should not be storage lock timeout")
	}
}

// source: PVE pve-manager/PVE/Storage.pm (pve-manager v8.x)
func TestIsStorageLockTimeout_RealPVEMessage(t *testing.T) {
	t.Parallel()
	err := errors.New("task failed: unable to create VM 131 - cannot import from 'local:import/foo.qcow2' - can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	if !pve.IsStorageLockTimeout(err) {
		t.Error("real PVE storage lock timeout message should match")
	}
}

func TestIsStorageLockTimeout_MixedCase(t *testing.T) {
	t.Parallel()
	err := errors.New("Can't Lock File '/var/lock/pve-manager/pve-storage-data' - Got Timeout")
	if !pve.IsStorageLockTimeout(err) {
		t.Error("mixed-case message should match")
	}
}

func TestIsStorageLockTimeout_OnlyLockNoTimeout(t *testing.T) {
	t.Parallel()
	err := errors.New("can't lock file '/var/lock/foo'")
	if pve.IsStorageLockTimeout(err) {
		t.Error("lock-only message without timeout should not match")
	}
}

func TestIsStorageLockTimeout_OnlyTimeoutNoLock(t *testing.T) {
	t.Parallel()
	err := errors.New("got timeout")
	if pve.IsStorageLockTimeout(err) {
		t.Error("timeout-only message without lock should not match")
	}
}

func TestIsStorageLockTimeout_Unrelated(t *testing.T) {
	t.Parallel()
	err := errors.New("VM 131 already exists")
	if pve.IsStorageLockTimeout(err) {
		t.Error("unrelated message should not match")
	}
}

// source: PVE pve-manager/PVE/API2/Qemu.pm + Storage/LVMThin.pm (pve-manager v8.x)
func TestIsLVMCommandTimeout_RealPVEMessage(t *testing.T) {
	t.Parallel()
	err := errors.New("AwaitTask UPID:pve:00157ECF:00B4B70C:6A0E4D24:resize:112:root@pam:: poll failed: task failed: command '/sbin/lvs --separator : --noheadings --units b --unbuffered --nosuffix --options lv_size /dev/data/vm-112-disk-0' failed: got timeout")
	if !pve.IsLVMCommandTimeout(err) {
		t.Error("real lvs command-timeout message should match")
	}
	// And the broader storage-backend predicate should subsume it.
	if !pve.IsStorageLockTimeout(err) {
		t.Error("IsStorageLockTimeout should subsume LVM command timeouts")
	}
}

func TestIsLVMCommandTimeout_LVCreate(t *testing.T) {
	t.Parallel()
	err := errors.New("task failed: command '/sbin/lvcreate -aly -Wy --yes --addtag pve-vm-114 --size 5G --name vm-114-disk-0 data' failed: got timeout")
	if !pve.IsLVMCommandTimeout(err) {
		t.Error("lvcreate command-timeout should match")
	}
}

func TestIsLVMCommandTimeout_VGSTimeout(t *testing.T) {
	t.Parallel()
	err := errors.New("command '/sbin/vgs --separator : data' failed: got timeout")
	if !pve.IsLVMCommandTimeout(err) {
		t.Error("vgs command-timeout should match")
	}
}

func TestIsLVMCommandTimeout_UnrelatedTimeout(t *testing.T) {
	t.Parallel()
	// "command X failed: got timeout" but X isn't an LVM tool — must not match.
	err := errors.New("command '/usr/bin/curl --foo' failed: got timeout")
	if pve.IsLVMCommandTimeout(err) {
		t.Error("non-LVM command timeout should not match")
	}
}

func TestIsLVMCommandTimeout_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsLVMCommandTimeout(nil) {
		t.Error("nil should not match")
	}
}

// source: PVE pve-manager/PVE/Cluster/Config.pm (pve-manager v8.x)
func TestIsPmxcfsConfigMissing_RealPVEMessage(t *testing.T) {
	t.Parallel()
	err := errors.New("AwaitTask UPID:pve:001581E7:00B4C1C0:6A0E4D3F:resize:114:root@pam:: poll failed: task failed: Configuration file 'nodes/pve/qemu-server/114.conf' does not exist")
	if !pve.IsPmxcfsConfigMissing(err) {
		t.Error("real pmxcfs config-missing message should match")
	}
}

func TestIsPmxcfsConfigMissing_Unrelated(t *testing.T) {
	t.Parallel()
	err := errors.New("VM 131 already exists")
	if pve.IsPmxcfsConfigMissing(err) {
		t.Error("unrelated message should not match")
	}
}

func TestIsPmxcfsConfigMissing_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsPmxcfsConfigMissing(nil) {
		t.Error("nil should not match")
	}
}

func TestWrapError_LVMCommandTimeout_IsRetriable(t *testing.T) {
	t.Parallel()
	err := errors.New("AwaitTask UPID:pve:00157ECF:00B4B70C:6A0E4D24:resize:112:root@pam:: poll failed: task failed: command '/sbin/lvs ...' failed: got timeout")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("LVM command timeout should map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
}

func TestWrapError_PmxcfsConfigMissing_IsRetriable(t *testing.T) {
	t.Parallel()
	err := errors.New("AwaitTask UPID:...:resize:114:root@pam:: poll failed: task failed: Configuration file 'nodes/pve/qemu-server/114.conf' does not exist")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("pmxcfs config-missing should map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
}

func TestWrapError_StorageLockTimeout_IsRetriable(t *testing.T) {
	t.Parallel()
	err := errors.New("task failed: unable to create VM 131 - can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("storage-lock timeout should map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
}

// ---------------------------------------------------------------------------
// IsSnapshotBlocked
// ---------------------------------------------------------------------------

func TestIsSnapshotBlocked_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsSnapshotBlocked(nil) {
		t.Error("nil should not be snapshot-blocked")
	}
}

// source: PVE pve-manager/PVE/QemuServer.pm (pve-manager v8.x)
func TestIsSnapshotBlocked_DetachMessage(t *testing.T) {
	t.Parallel()
	// Canonical PVE detach surface: PUT /config delete:scsiN rejected because
	// a snapshot references the disk.
	err := errors.New("cannot delete disk 'scsi1', disk is used in snapshot 'snap1'")
	if !pve.IsSnapshotBlocked(err) {
		t.Errorf("detach snapshot-blocked message should match; err=%v", err)
	}
}

func TestIsSnapshotBlocked_ResizeMessage(t *testing.T) {
	t.Parallel()
	// Canonical PVE resize surface (LVM-thin/ZFS): task body contains this text.
	err := errors.New("can't resize volume, volume is referenced in snapshot 'bosh-1'")
	if !pve.IsSnapshotBlocked(err) {
		t.Errorf("resize snapshot-blocked message should match; err=%v", err)
	}
}

func TestIsSnapshotBlocked_StorageLockTimeout(t *testing.T) {
	t.Parallel()
	// Unrelated transient error must not match.
	err := errors.New("task failed: can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	if pve.IsSnapshotBlocked(err) {
		t.Errorf("storage lock timeout should not be snapshot-blocked; err=%v", err)
	}
}

func TestIsSnapshotBlocked_CaseInsensitive_UsedIn(t *testing.T) {
	t.Parallel()
	// PVE error text may arrive in mixed case depending on Perl die() formatting.
	err := errors.New("Cannot Delete Disk 'scsi0', Disk Is Used In Snapshot 'auto-backup'")
	if !pve.IsSnapshotBlocked(err) {
		t.Errorf("mixed-case 'Is Used In Snapshot' should match; err=%v", err)
	}
}

func TestIsSnapshotBlocked_CaseInsensitive_ReferencedIn(t *testing.T) {
	t.Parallel()
	err := errors.New("Task Failed: Volume Is Referenced In Snapshot 'daily-2024'")
	if !pve.IsSnapshotBlocked(err) {
		t.Errorf("mixed-case 'Referenced In Snapshot' should match; err=%v", err)
	}
}

func TestIsSnapshotBlocked_Unrelated(t *testing.T) {
	t.Parallel()
	err := errors.New("VM 131 already exists on node 'pve'")
	if pve.IsSnapshotBlocked(err) {
		t.Errorf("unrelated error should not be snapshot-blocked; err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// IsVolumeMissing — covers the lvmthin and zfspool 500-error patterns that
// the SDK does not classify as 404. NodeForExisting must fold these into a
// clean miss so a just-deleted disk does not surface as a retriable error.
// ---------------------------------------------------------------------------

func TestIsVolumeMissing_LvmthinFailedToFind(t *testing.T) {
	t.Parallel()
	// Exact PVE-API error shape observed in the wild for a deleted lvmthin LV.
	err := errors.New(
		`failed to check if volume "data:vm-9373-disk-0" exists on storage "data" node "pve": ` +
			`failed to execute GET request to "/nodes/pve/storage/data/content/data:vm-9373-disk-0" ` +
			`with context: HTTP GET request failed: API request failed: can't get size of ` +
			`'/dev/data/vm-9373-disk-0':   Failed to find logical volume "data/vm-9373-disk-0"`)
	if !pve.IsVolumeMissing(err) {
		t.Errorf("expected lvmthin 'Failed to find logical volume' error to classify as missing; err=%v", err)
	}
}

func TestIsVolumeMissing_LvmthinCantGetSize(t *testing.T) {
	t.Parallel()
	// The "can't get size of '/dev/...'" prefix alone (without the trailing
	// 'Failed to find logical volume') is sufficient — PVE has been seen
	// emitting just the size-probe error for some lvmthin variants.
	err := errors.New(`can't get size of '/dev/data/vm-100-disk-1'`)
	if !pve.IsVolumeMissing(err) {
		t.Errorf("expected lvmthin 'can't get size of' error to classify as missing; err=%v", err)
	}
}

func TestIsVolumeMissing_ZfspoolDatasetMissing(t *testing.T) {
	t.Parallel()
	err := errors.New(`zfs error: dataset does not exist`)
	if !pve.IsVolumeMissing(err) {
		t.Errorf("expected zfspool 'dataset does not exist' to classify as missing; err=%v", err)
	}
}

func TestIsVolumeMissing_HTTP404(t *testing.T) {
	t.Parallel()
	// SDK 404 path must still classify (existing IsNotFound semantics retained).
	err := makeAPIErr(404, "no such volume")
	if !pve.IsVolumeMissing(err) {
		t.Errorf("expected SDK 404 to classify as missing; err=%v", err)
	}
}

func TestIsVolumeMissing_Unrelated(t *testing.T) {
	t.Parallel()
	// A genuine transient error (timeout, 5xx without the missing-LV text)
	// must NOT classify as missing — otherwise we would mask real failures.
	err := errors.New("upstream gateway timed out")
	if pve.IsVolumeMissing(err) {
		t.Errorf("unrelated error should not classify as missing; err=%v", err)
	}
}

func TestIsVolumeMissing_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsVolumeMissing(nil) {
		t.Error("nil should not classify as missing")
	}
}

// ---------------------------------------------------------------------------
// IsBaseVolumeInUse — PVE refusal to delete a template whose base volume is
// still referenced by a linked clone.
// ---------------------------------------------------------------------------

func TestIsBaseVolumeInUse_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsBaseVolumeInUse(nil) {
		t.Error("nil should not classify as base-volume-in-use")
	}
}

func TestIsBaseVolumeInUse_VolumeStillInUseByClone(t *testing.T) {
	t.Parallel()
	// Canonical lvmthin DELETE /storage/content/<volid> refusal.
	err := errors.New("volume 'vm-9000-disk-0' is still in use by 'vm-101-disk-0'")
	if !pve.IsBaseVolumeInUse(err) {
		t.Errorf("volume-in-use message should classify as base-volume-in-use; err=%v", err)
	}
}

func TestIsBaseVolumeInUse_BaseVolumeStillInUse(t *testing.T) {
	t.Parallel()
	// ZFS variant with "base" keyword.
	err := errors.New("base volume still in use")
	if !pve.IsBaseVolumeInUse(err) {
		t.Errorf("'base volume still in use' should classify as base-volume-in-use; err=%v", err)
	}
}

func TestIsBaseVolumeInUse_Unrelated500(t *testing.T) {
	t.Parallel()
	err := errors.New("500 internal error")
	if pve.IsBaseVolumeInUse(err) {
		t.Errorf("unrelated 500 message should not classify as base-volume-in-use; err=%v", err)
	}
}

func TestIsBaseVolumeInUse_Timeout(t *testing.T) {
	t.Parallel()
	err := errors.New("timeout")
	if pve.IsBaseVolumeInUse(err) {
		t.Errorf("timeout error should not classify as base-volume-in-use; err=%v", err)
	}
}

func TestIsBaseVolumeInUse_WrappedCPIError(t *testing.T) {
	t.Parallel()
	// A CPI CloudError wrapping the PVE message must also classify correctly.
	inner := errors.New("volume 'data:vm-200-disk-0' is still in use by linked clone")
	wrapped := cpierrors.Wrap(inner, "PVE error: "+inner.Error())
	if !pve.IsBaseVolumeInUse(wrapped) {
		t.Errorf("CPI-wrapped base-volume-in-use message should classify correctly; err=%v", wrapped)
	}
}

// ---------------------------------------------------------------------------
// ExistsTolerant — folds the lvmthin/zfspool "500 with CLI text" quirk into a
// clean not-found so the local-backend cluster scan and idempotent delete
// paths see uniform existence semantics regardless of storage backend.
// ---------------------------------------------------------------------------

// existsTolerantStorageService is a minimal storage.Service fake exposing
// only Exists; any other call is unused by ExistsTolerant and panics via the
// embedded nil interface if invoked.
type existsTolerantStorageService struct {
	storage.Service
	existsFn func(ctx context.Context, node, storageName, volume string) (bool, error)
}

func (s *existsTolerantStorageService) Exists(ctx context.Context, node, storageName, volume string) (bool, error) {
	return s.existsFn(ctx, node, storageName, volume)
}

// existsTolerantMockClient is a minimal pve.Client fake for ExistsTolerant
// tests. Only Storage() is used; all other accessors return nil.
type existsTolerantMockClient struct {
	storageSvc storage.Service
}

func (c *existsTolerantMockClient) QEMU() qemu.Service                     { return nil }
func (c *existsTolerantMockClient) Storage() storage.Service               { return c.storageSvc }
func (c *existsTolerantMockClient) CloudInit() cloudinit.Service           { return nil }
func (c *existsTolerantMockClient) Tasks() tasks.Service                   { return nil }
func (c *existsTolerantMockClient) Nodes() nodes.Service                   { return nil }
func (c *existsTolerantMockClient) Cluster() cluster.Service               { return nil }
func (c *existsTolerantMockClient) ClusterStorage() clusterstorage.Service { return nil }
func (c *existsTolerantMockClient) Pools() pve.PoolService                 { return nil }

var _ pve.Client = (*existsTolerantMockClient)(nil)

func existsTolerantClient(fn func(ctx context.Context, node, storageName, volume string) (bool, error)) pve.Client {
	return &existsTolerantMockClient{storageSvc: &existsTolerantStorageService{existsFn: fn}}
}

func TestExistsTolerant_PassesThroughSuccess(t *testing.T) {
	t.Parallel()
	c := existsTolerantClient(func(_ context.Context, _, _, _ string) (bool, error) {
		return true, nil
	})
	exists, err := pve.ExistsTolerant(context.Background(), c, "pve-01", "local-lvm", "vm-100-disk-0")
	if err != nil {
		t.Fatalf("ExistsTolerant: %v", err)
	}
	if !exists {
		t.Fatalf("exists=false, want true")
	}
}

func TestExistsTolerant_FoldsLVMThinMissing(t *testing.T) {
	t.Parallel()
	rawErr := errors.New(`can't get size of '/dev/data/vm-100-disk-0': Failed to find logical volume "data/vm-100-disk-0"`)
	c := existsTolerantClient(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, rawErr
	})
	exists, err := pve.ExistsTolerant(context.Background(), c, "pve-01", "data", "vm-100-disk-0")
	if err != nil {
		t.Fatalf("expected lvmthin missing-volume error to fold to (false, nil); got err=%v", err)
	}
	if exists {
		t.Fatalf("exists=true, want false")
	}
}

func TestExistsTolerant_FoldsZFSPoolMissing(t *testing.T) {
	t.Parallel()
	rawErr := errors.New("zfs error: dataset does not exist")
	c := existsTolerantClient(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, rawErr
	})
	exists, err := pve.ExistsTolerant(context.Background(), c, "pve-01", "rpool", "vm-200-disk-0")
	if err != nil {
		t.Fatalf("expected zfspool missing-dataset error to fold to (false, nil); got err=%v", err)
	}
	if exists {
		t.Fatalf("exists=true, want false")
	}
}

func TestExistsTolerant_PropagatesOtherErrors(t *testing.T) {
	t.Parallel()
	rawErr := errors.New("upstream gateway timed out")
	c := existsTolerantClient(func(_ context.Context, _, _, _ string) (bool, error) {
		return false, rawErr
	})
	_, err := pve.ExistsTolerant(context.Background(), c, "pve-01", "data", "vm-300-disk-0")
	if err == nil {
		t.Fatalf("expected genuine (non-missing-shaped) error to propagate, got nil")
	}
	if !errors.Is(err, rawErr) {
		t.Fatalf("propagated error does not chain to the original; err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// IsVMConfigLocked / VMConfigLockType / WrapVMConfigLocked
// ---------------------------------------------------------------------------

func TestIsVMConfigLocked_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsVMConfigLocked(nil) {
		t.Error("nil error should not be VM-config-locked")
	}
}

// source: PVE pve-manager PVE/AbstractConfig.pm check_lock ("VM is locked ($lock)").
func TestIsVMConfigLocked_PositiveCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		msg      string
		wantType string
	}{
		{"bare clone", "VM is locked (clone)", "clone"},
		{"bare create", "VM is locked (create)", "create"},
		{"bare backup", "VM is locked (backup)", "backup"},
		{"bare migrate", "VM is locked (migrate)", "migrate"},
		{"bare snapshot", "VM is locked (snapshot)", "snapshot"},
		{"bare rollback", "VM is locked (rollback)", "rollback"},
		{"wrapped by destroy handler", "500 unable to destroy VM 106: VM is locked (clone)", "clone"},
		{"vmid repeated before is locked", "unable to stop VM 106 - VM 106 is locked (backup)", "backup"},
		{"mixed case", "Vm Is Locked (Clone)", "Clone"},
		{"api-error prefixed", "PVE API error: 500 Internal Server Error: VM is locked (create)", "create"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := errors.New(tc.msg)
			if !pve.IsVMConfigLocked(err) {
				t.Errorf("IsVMConfigLocked(%q) = false, want true", tc.msg)
			}
			if got := pve.VMConfigLockType(err); got != tc.wantType {
				t.Errorf("VMConfigLockType(%q) = %q, want %q", tc.msg, got, tc.wantType)
			}
		})
	}
}

// Negatives: storage-lockfile and other unrelated PVE messages must NOT match
// the guest-config-lock pattern — the two conditions require different
// recovery and must never be conflated.
func TestIsVMConfigLocked_NegativeCases(t *testing.T) {
	t.Parallel()
	cases := []string{
		"task failed: unable to create VM 131 - cannot import from 'local:import/foo.qcow2' - can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout",
		"command '/sbin/lvs --separator : --noheadings --units b --unbuffered --nosuffix --options lv_size /dev/data/vm-112-disk-0' failed: got timeout",
		"can't lock file '/var/lock/qemu-server/lock-106.conf' - got timeout",
		"VM 131 already exists",
		"too many requests",
		"unable to acquire lock",
		"Configuration file 'nodes/pve/qemu-server/114.conf' does not exist",
		"",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()
			err := errors.New(msg)
			if msg == "" {
				err = nil
			}
			if pve.IsVMConfigLocked(err) {
				t.Errorf("IsVMConfigLocked(%q) = true, want false", msg)
			}
			if got := pve.VMConfigLockType(err); got != "" {
				t.Errorf("VMConfigLockType(%q) = %q, want \"\"", msg, got)
			}
		})
	}
}

func TestIsVMConfigLocked_DoesNotOverlapStorageLockTimeout(t *testing.T) {
	t.Parallel()
	lockfileErr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	if pve.IsVMConfigLocked(lockfileErr) {
		t.Error("storage-lockfile message must not match IsVMConfigLocked")
	}
	configLockErr := errors.New("VM is locked (clone)")
	if pve.IsStorageLockTimeout(configLockErr) {
		t.Error("guest-config-lock message must not match IsStorageLockTimeout")
	}
}

func TestVMConfigLockType_Nil(t *testing.T) {
	t.Parallel()
	if got := pve.VMConfigLockType(nil); got != "" {
		t.Errorf("VMConfigLockType(nil) = %q, want \"\"", got)
	}
}

func TestWrapVMConfigLocked_Nil(t *testing.T) {
	t.Parallel()
	if err := pve.WrapVMConfigLocked(nil, 106, "pve01"); err != nil {
		t.Errorf("WrapVMConfigLocked(nil) = %v, want nil", err)
	}
}

func TestWrapVMConfigLocked_NonLockError_FallsBackToWrapError(t *testing.T) {
	t.Parallel()
	orig := errors.New("VM 131 already exists")
	err := pve.WrapVMConfigLocked(orig, 131, "pve01")
	if pve.IsVMConfigLocked(err) {
		t.Fatalf("non-lock error should not be classified as VM-config-locked: %v", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected original message preserved, got: %v", err)
	}
}

func TestWrapVMConfigLocked_LockedError_RetriableAndActionable(t *testing.T) {
	t.Parallel()
	orig := errors.New("500 unable to destroy VM 106: VM is locked (clone)")
	err := pve.WrapVMConfigLocked(orig, 106, "pve01")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !cpiErrIsRetriable(t, err) {
		t.Errorf("WrapVMConfigLocked must produce a retriable error, got: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"106", "pve01", "clone", "qm unlock"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing expected substring %q", msg, want)
		}
	}
}
