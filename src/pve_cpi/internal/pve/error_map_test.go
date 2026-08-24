package pve_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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

func TestIsCloneToNonSharedStorage(t *testing.T) {
	t.Parallel()
	// Exact live shape: PVE rejects a cross-node clone whose destination
	// storage is node-local. The SDK surfaced it with code 0, which the
	// transient-transport classifier would otherwise match — the real
	// failure behind another "exhausted VMID allocation" message.
	err := makeAPIErr(500, "can't clone to non-shared storage 'local-lvm-data'")
	if !pve.IsCloneToNonSharedStorage(err) {
		t.Errorf("destination-not-shared rejection should match, got false; err=%v", err)
	}
	if pve.IsCloneToNonSharedStorage(nil) {
		t.Error("nil should not match")
	}
	if pve.IsCloneToNonSharedStorage(makeAPIErr(500, "storage 'zfs-0' is not online")) {
		t.Error("unrelated 500 should not match")
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

// source: live 3-node PVE cluster with shared RBD under concurrent clones —
// PVE's cfs cluster lock on the storage, not the file lock: mass VM creation
// from one template makes qmclone tasks fail with exactly this prose.
func TestIsStorageLockTimeout_CfsLockRequestTimeout(t *testing.T) {
	t.Parallel()
	err := errors.New("task failed: clone failed: cfs-lock 'storage-rbd' error: got lock request timeout")
	if !pve.IsStorageLockTimeout(err) {
		t.Error("cfs-lock request timeout message should match")
	}
}

func TestIsStorageLockTimeout_CfsLockOtherError(t *testing.T) {
	t.Parallel()
	err := errors.New("cfs-lock 'storage-rbd' error: no quorum")
	if pve.IsStorageLockTimeout(err) {
		t.Error("a non-timeout cfs-lock error must not classify as lock timeout")
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
// IsClusterNotQuorate
// ---------------------------------------------------------------------------

func TestIsClusterNotQuorate_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsClusterNotQuorate(nil) {
		t.Error("nil error should not be cluster-not-quorate")
	}
}

// source: PVE pmxcfs (cfs-lock write path) — observed message shape.
func TestIsClusterNotQuorate_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"not_quorate_phrase", "error writing config, cfs-lock failed - not quorate", true},
		{"not_quorate_mixed_case", "Error Writing Config, Cfs-Lock Failed - Not Quorate", true},
		{"no_quorum_phrase", "cluster not ready - no quorum on node pve02", true},
		{"no_quorum_mixed_case", "Cluster Not Ready - No Quorum On Node Pve02", true},
		{"5xx_wrapping_not_quorate", "task failed: unable to update VM 100 config: not quorate", true},
		{"unrelated_message", "VM 131 already exists", false},
		{"unrelated_lock_timeout", "can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout", false},
		{"partial_word_quorum_only", "quorum device configured", false},
		{"empty_string", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pve.IsClusterNotQuorate(errors.New(tc.msg))
			if got != tc.want {
				t.Errorf("IsClusterNotQuorate(%q) = %v; want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestWrapError_ClusterNotQuorate_PlainText_IsRetriableWithHint(t *testing.T) {
	t.Parallel()
	err := errors.New("task failed: unable to update VM 100 config: cfs-lock failed - not quorate")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("cluster-not-quorate should map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, "cluster has lost quorum") {
		t.Errorf("expected operator hint 'cluster has lost quorum' in message, got %q", msg)
	}
	if !strings.Contains(msg, "pvecm status") {
		t.Errorf("expected operator hint to name `pvecm status`, got %q", msg)
	}
	if !strings.Contains(msg, "not quorate") {
		t.Errorf("expected original PVE message preserved, got %q", msg)
	}
}

// TestWrapError_ClusterNotQuorate_5xxAPIError_IsRetriableWithHint verifies the
// quorum-specific check runs BEFORE the generic 5xx APIError branch, so a
// quorum error that PVE happens to wrap in a 5xx HTTP response still gets the
// operator-actionable hint instead of the anonymous "PVE server error"
// message.
func TestWrapError_ClusterNotQuorate_5xxAPIError_IsRetriableWithHint(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, "cfs-lock failed - not quorate")
	wrapped := pve.WrapError(err)
	if wrapped == nil {
		t.Fatal("WrapError returned nil")
	}
	if !cpiErrIsRetriable(t, wrapped) {
		t.Errorf("quorum 5xx should map to RetriableCloudError; got %T %v", wrapped, wrapped)
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, "cluster has lost quorum") {
		t.Errorf("expected quorum-specific hint on a 5xx-wrapped quorum error, got %q", msg)
	}
	if strings.Contains(msg, "PVE server error") {
		t.Errorf("quorum error must not fall through to the generic 5xx message, got %q", msg)
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

func TestWrapError_CauseTextAppearsExactlyOnce(t *testing.T) {
	t.Parallel()
	// Wrap messages must not embed the cause text: Error() appends the chained
	// cause after a colon, so an embedded copy prints it twice (the
	// "failed to upload ... EOF: failed to upload ... EOF" shape).
	cause := fmt.Errorf("failed to upload configdrive iso: %w", io.EOF)
	wrapped := pve.WrapError(cause)
	if got := strings.Count(wrapped.Error(), "EOF"); got != 1 {
		t.Errorf("cause text should appear exactly once, got %d in %q", got, wrapped.Error())
	}
	if got := strings.Count(wrapped.Error(), "failed to upload configdrive iso"); got != 1 {
		t.Errorf("cause prefix should appear exactly once, got %d in %q", got, wrapped.Error())
	}
	if !pve.IsTransientTransport(wrapped) {
		t.Errorf("de-doubled wrap must stay transient-transport classifiable; err=%v", wrapped)
	}
}

func TestIsBaseVolumeInUse_WrappedCPIError(t *testing.T) {
	t.Parallel()
	// A CPI CloudError wrapping the PVE message must also classify correctly.
	inner := errors.New("volume 'data:vm-200-disk-0' is still in use by linked clone")
	wrapped := cpierrors.Wrap(inner, "PVE error")
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

func TestIsHotUnplugBusy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"real PVE message", errors.New("API request failed: parameter error: Parameter verification failed. (code: 0, errors: scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?)"), true},
		{"mixed case", errors.New("SCSI2: Hotplug Problem - error on hot-unplugging device 'virtioscsi2' - Still Busy In Guest?"), true},
		{"hotplug without busy", errors.New("scsi1: hotplug problem - unsupported configuration"), false},
		{"busy without hotplug", errors.New("device busy in guest"), false},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.IsHotUnplugBusy(tc.err); got != tc.want {
				t.Errorf("IsHotUnplugBusy(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// source: qemu-server PVE::API2::Qemu destroy_vm ("VM $vmid is running - destroy failed").
func TestIsVMRunningDestroyFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"bare with vmid", "VM 7801 is running - destroy failed", true},
		{"bare without vmid", "VM is running - destroy failed", true},
		{"api wrapped", `API request failed: VM 7801 is running - destroy failed`, true},
		{"http context wrapped", `nodes.DeleteQemu: failed to execute DELETE request to "/nodes/lab-pve-cpi-2/qemu/7801" with context: API request failed: VM 7801 is running - destroy failed`, true},
		{"mixed case", "vm 12 IS RUNNING - Destroy Failed", true},
		{"running but not destroy", "VM 7801 is running", false},
		{"config locked is distinct", "VM is locked (clone)", false},
		{"unrelated", "storage 'nfs-images' is not online", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.IsVMRunningDestroyFailure(errors.New(tc.msg)); got != tc.want {
				t.Errorf("IsVMRunningDestroyFailure(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsVMRunningDestroyFailure_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsVMRunningDestroyFailure(nil) {
		t.Error("nil error should not match")
	}
}

// ---------------------------------------------------------------------------
// IsPoolNotEmpty / IsPoolNotFound
// ---------------------------------------------------------------------------

func TestIsPoolNotEmpty_LiveText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"live text", "pool 'bosh' is not empty (contains VM 99098)", true},
		{"mixed case", "Pool 'bosh' Is Not Empty (contains VM 99098)", true},
		{"missing pool word", "resource is not empty", false},
		{"unrelated", "storage 'nfs-images' is not online", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := makeAPIErr(500, tc.msg)
			if got := pve.IsPoolNotEmpty(err); got != tc.want {
				t.Errorf("IsPoolNotEmpty(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsPoolNotEmpty_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsPoolNotEmpty(nil) {
		t.Error("nil error should not match")
	}
}

func TestIsPoolNotFound_LiveText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"live text", "delete pool failed: pool 'x' does not exist", true},
		// The read shape: GET /pools/{poolid} for a pool that does not exist
		// yet answers 500 + this text, not 404. Observed on PVE 9.2.6.
		{"live read text", "API request failed: pool 'v2-templates' does not exist\n (code: 0)", true},
		{"mixed case", "Delete Pool Failed: Pool 'x' Does Not Exist", true},
		{"unrelated no pool word", "storage does not exist", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := makeAPIErr(500, tc.msg)
			if got := pve.IsPoolNotFound(err); got != tc.want {
				t.Errorf("IsPoolNotFound(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsPoolNotFound_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsPoolNotFound(nil) {
		t.Error("nil error should not match")
	}
}

// ---------------------------------------------------------------------------
// IsVolumeFormatUnknown — permanent PVE 500 that must not be retried
// ---------------------------------------------------------------------------

// liveVolumeSizeInfoNoFormat is the verbatim PVE 9.2 body observed on a
// has_disk probe for a directory-style volid on file storage. Pinned exactly
// (including the trailing newline and "(code: 0)" suffix the SDK appends) so a
// future PVE wording change fails this test rather than silently reverting the
// classifier to "transient".
const liveVolumeSizeInfoNoFormat = "volume_size_info on 'nfs-images:9999/vm-9999-disk-0.qcow2' failed - no format\n (code: 0)"

func TestIsVolumeFormatUnknown_LiveText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"live text", liveVolumeSizeInfoNoFormat, true},
		{"mixed case", "VOLUME_SIZE_INFO on 'x' FAILED - NO FORMAT", true},
		// Conservative: neither half alone is enough.
		{"format word without volume_size_info", "unsupported format 'raw' for storage", false},
		{"no format without volume_size_info", "backup failed - no format", false},
		{"volume_size_info other failure", "volume_size_info on 'x' failed - permission denied", false},
		{"unrelated 500", "internal server error", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := makeAPIErr(500, tc.msg)
			if got := pve.IsVolumeFormatUnknown(err); got != tc.want {
				t.Errorf("IsVolumeFormatUnknown(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

func TestIsVolumeFormatUnknown_Nil(t *testing.T) {
	t.Parallel()
	if pve.IsVolumeFormatUnknown(nil) {
		t.Error("nil error should not match")
	}
}

// TestIsTransientTransport_VolumeFormatUnknownIsPermanent pins the fix for the
// live regression: this shape is an HTTP 500, so the blanket 5xx rule in
// IsTransientTransport classified it transient and the retry helpers burned the
// full 8-attempt budget (~29.9s) before surfacing the identical error.
func TestIsTransientTransport_VolumeFormatUnknownIsPermanent(t *testing.T) {
	t.Parallel()
	err := makeAPIErr(500, liveVolumeSizeInfoNoFormat)
	if pve.IsTransientTransport(err) {
		t.Error("volume_size_info ... no format is a permanent request-shaped error, must not be transient")
	}
	if pve.IsPVEPushback(err) {
		t.Error("volume_size_info ... no format must not be classified as pushback either")
	}
	// A plain 500 stays transient — the exclusion must be shape-specific.
	if !pve.IsTransientTransport(makeAPIErr(500, "internal server error")) {
		t.Error("an unrelated 500 must still be transient")
	}
}

// TestWrapError_VolumeFormatUnknown_NotRetriable proves the same shape surfaces
// to the director as a permanent CloudError rather than a RetriableCloudError.
func TestWrapError_VolumeFormatUnknown_NotRetriable(t *testing.T) {
	t.Parallel()
	got := pve.WrapError(makeAPIErr(500, liveVolumeSizeInfoNoFormat))
	var cpiErr *cpierrors.Error
	if !errors.As(got, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", got, got)
	}
	if cpiErr.Type() != cpierrors.TypeCloud {
		t.Errorf("want TypeCloud got %q", cpiErr.Type())
	}
	if cpiErr.OkToRetry() {
		t.Error("a malformed-volid 500 is permanent; ok_to_retry must be false")
	}
}

// TestRetryOnTransient_DoesNotRetryVolumeFormatUnknown asserts the retry helper
// returns on the first attempt instead of burning the transient budget.
func TestRetryOnTransient_DoesNotRetryVolumeFormatUnknown(t *testing.T) {
	t.Parallel()
	calls := 0
	err := pve.RetryOnTransient(context.Background(), nil, "has_disk_exists", 8, func() error {
		calls++
		return makeAPIErr(500, liveVolumeSizeInfoNoFormat)
	})
	if err == nil {
		t.Fatal("expected the error to surface")
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 attempt (permanent error), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// WrapConfigReadError / WrapMutationError
// ---------------------------------------------------------------------------

// TestWrapConfigReadError covers the classifier the volume-holder scans use. Its
// default is WrapError's -- permanent -- because a config read's failure shapes
// are enumerable; what it adds is that a server fault stays retriable even when
// it arrives as a bare code the generic mapper does not recognize.
func TestWrapConfigReadError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		retriable bool
	}{
		{"nil", nil, false},
		{"403 is a grant to add, not a fault to re-drive", makeAPIErr(403, "Permission check failed"), false},
		{"404 is permanent", makeAPIErr(404, "not found"), false},
		{"500 is a server fault", makeAPIErr(500, "internal error"), true},
		{"596 transport shape", errors.New("pveproxy backend gone (code: 596)"), true},
		{"unrecognized prose stays permanent", errors.New("something we do not model"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pve.WrapConfigReadError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("nil in, nil out; got %v", got)
				}
				return
			}
			if cpiErrIsRetriable(t, got) != tc.retriable {
				t.Errorf("retriable = %v, want %v (err: %v)", !tc.retriable, tc.retriable, got)
			}
		})
	}
}

// TestWrapMutationError covers the classifier the parker's attaches, detaches,
// protection writes, and task awaits use. Its default is the INVERSE of
// WrapError's: retriable, because PVE reports transient conditions as prose no
// classifier models, and a park that failed permanently on one of those leaves
// the disk free-floating -- the state parking exists to prevent. Only shapes
// that are a verdict about the request stay permanent.
func TestWrapMutationError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		retriable bool
	}{
		{"nil", nil, false},
		{"403 is a verdict", makeAPIErr(403, "Permission check failed"), false},
		{"400 is a verdict", makeAPIErr(400, "parameter verification failed"), false},
		{"404 is a verdict", makeAPIErr(404, "not found"), false},
		{"401 is a verdict", makeAPIErr(401, "authentication failure"), false},
		{"429 is pushback, not a verdict", makeAPIErr(429, "too many requests"), true},
		{"500 is a server fault", makeAPIErr(500, "internal error"), true},
		{
			"config gone is a verdict",
			errors.New("Configuration file 'nodes/pve1/qemu-server/90000.conf' does not exist"),
			false,
		},
		{"unrecognized prose is retriable", errors.New("something we do not model"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pve.WrapMutationError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("nil in, nil out; got %v", got)
				}
				return
			}
			if cpiErrIsRetriable(t, got) != tc.retriable {
				t.Errorf("retriable = %v, want %v (err: %v)", !tc.retriable, tc.retriable, got)
			}
		})
	}
}

// TestWrapMutationError_PmxcfsConfigMissing_ChainsCause is the F7 regression:
// WrapMutationError's pmxcfs-config-gone branch used to build its result with
// cpierrors.Cloud("...: %s", err.Error()), which embeds the cause as flat
// text rather than chaining it -- Cloud sets no cause, so Unwrap returns nil
// and downstream errors.Is / errors.As stop working on the result. This pins
// that the SDK sentinel is still reachable through the chain (errors.Is)
// after the wrap, and that the message text is unchanged (Error() appends
// the chained cause exactly once, producing the same string the embedded
// format used to).
func TestWrapMutationError_PmxcfsConfigMissing_ChainsCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	cause := fmt.Errorf("Configuration file 'nodes/pve1/qemu-server/90000.conf' does not exist: %w", sentinel)
	got := pve.WrapMutationError(cause)
	if !errors.Is(got, sentinel) {
		t.Errorf("WrapMutationError must chain the cause (errors.Is must reach the sentinel); err=%v", got)
	}
	if strings.Count(got.Error(), "boom") != 1 {
		t.Errorf("cause text should appear exactly once, got %d in %q", strings.Count(got.Error(), "boom"), got.Error())
	}
	if cpiErrIsRetriable(t, got) {
		t.Errorf("pmxcfs-config-gone must stay non-retriable through WrapMutationError; err=%v", got)
	}
}

// TestWrapError_VolumeFormatUnknown_ChainsCause is the F7 regression for
// WrapError's IsVolumeFormatUnknown branch: same embedded-cause defect as
// WrapMutationError's pmxcfs branch, on the sibling classifier.
func TestWrapError_VolumeFormatUnknown_ChainsCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	cause := fmt.Errorf("volume_size_info on 'nfs-images:9999/vm-9999-disk-0.qcow2' failed - no format: %w", sentinel)
	got := pve.WrapError(cause)
	if !errors.Is(got, sentinel) {
		t.Errorf("WrapError must chain the cause (errors.Is must reach the sentinel); err=%v", got)
	}
	if strings.Count(got.Error(), "boom") != 1 {
		t.Errorf("cause text should appear exactly once, got %d in %q", strings.Count(got.Error(), "boom"), got.Error())
	}
	if cpiErrIsRetriable(t, got) {
		t.Errorf("volume-format-unknown must stay non-retriable; err=%v", got)
	}
}

// ---------------------------------------------------------------------------
// IsTransportConnectionDrop
// ---------------------------------------------------------------------------

// sdkUploadEOFError reproduces the exact chain the SDK surfaces when pveproxy
// drops a connection mid-request: url.Error(io.EOF) wrapped by the transport
// retry loop's "request failed after N attempt(s)" and the upload path's two
// prose layers, every layer with %w.
func sdkUploadEOFError(inner error) error {
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://pve01.example.io:8006/api2/json/nodes/pve01/storage/local/upload",
		Err: inner,
	}
	wrapped := fmt.Errorf("request failed after 1 attempt(s): %w", uerr)
	wrapped = fmt.Errorf("HTTP upload to %q failed for file %q: %w",
		"/nodes/pve01/storage/local/upload", "vm-2706-config.iso", wrapped)
	return fmt.Errorf("failed to upload file %q to path %q: %w",
		"vm-2706-config.iso", "/nodes/pve01/storage/local/upload", wrapped)
}

func TestIsTransportConnectionDrop(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof mid-request", sdkUploadEOFError(io.EOF), true},
		{"unexpected eof", sdkUploadEOFError(io.ErrUnexpectedEOF), true},
		{"connection reset", sdkUploadEOFError(&net.OpError{
			Op: "write", Net: "tcp",
			Err: &os.SyscallError{Syscall: "write", Err: syscall.ECONNRESET},
		}), true},
		{"broken pipe", sdkUploadEOFError(&net.OpError{
			Op: "write", Net: "tcp",
			Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE},
		}), true},
		{"api 500 is not a drop", makeAPIErr(500, "internal error"), false},
		{"api 404 is not a drop", makeAPIErr(404, "not found"), false},
		{"plain prose is not a drop", errors.New("volume does not exist"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pve.IsTransportConnectionDrop(tc.err); got != tc.want {
				t.Errorf("IsTransportConnectionDrop = %v, want %v (err: %v)", got, tc.want, tc.err)
			}
		})
	}
}

// TestIsTransientTransport_ConnectionDrop pins the classification behind the
// live failure: a CF deploy's configdrive upload died with a mid-request EOF
// and surfaced as a non-retriable CloudError because the transient classifier
// only modeled dial failures and timeouts, not drops after the connection was
// established.
func TestIsTransientTransport_ConnectionDrop(t *testing.T) {
	t.Parallel()
	if !pve.IsTransientTransport(sdkUploadEOFError(io.EOF)) {
		t.Error("mid-request EOF must be transient")
	}
}

func TestWrapError_ConnectionDropRetriable(t *testing.T) {
	t.Parallel()
	got := pve.WrapError(sdkUploadEOFError(io.EOF))
	if !cpiErrIsRetriable(t, got) {
		t.Errorf("mid-request EOF must map to a retriable error, got: %v", got)
	}
}
