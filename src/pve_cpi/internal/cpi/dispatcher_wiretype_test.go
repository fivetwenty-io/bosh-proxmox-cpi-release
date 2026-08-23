package cpi_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// These tests pin the dispatch boundary's wire-type translation against the
// Director's known-error list (KNOWN_ERRORS in bosh-director's
// clouds/external_cpi.rb). A type outside that list makes the Director raise
// "Unknown CPI error" and discard ok_to_retry, so every type the dispatcher
// can emit must be on it. The internal taxonomy is wider than the list on
// purpose; the translation is the single point where the two meet.

// dispatchTypedError registers a handler on method that returns err, invokes
// it, and returns the response's error body.
func dispatchTypedError(t *testing.T, method string, err error) *jsonrpc.ErrorBody {
	t.Helper()
	d := cpi.NewDispatcher(nopLogger())
	mustRegister(t, d, method, cpi.HandlerFunc(
		func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, err
		}))
	resp := d.Handle(context.Background(), makeReq(method))
	if resp == nil || resp.Error == nil {
		t.Fatal("expected an error response")
	}
	return resp.Error
}

// TestWireType_RetriableOnCreateVM: a retriable error escaping create_vm must
// cross the wire as VMCreationFailed with ok_to_retry=true. That is the only
// encoding that engages the Director's own create retry loop
// (max_vm_create_tries); RetriableCloudError is an abstract base class on the
// Director side and fails the deployment as an unknown CPI error.
func TestWireType_RetriableOnCreateVM(t *testing.T) {
	t.Parallel()
	body := dispatchTypedError(t, "create_vm",
		cpierrors.Retriable("upload configdrive iso: PVE connection dropped mid-request"))
	if body.Type != string(cpierrors.TypeVMCreationFailed) {
		t.Errorf("type = %q, want %q", body.Type, cpierrors.TypeVMCreationFailed)
	}
	if !body.OkToRetry {
		t.Error("ok_to_retry = false, want true")
	}
}

// TestWireType_RetriableOnOtherMethods: methods without a Director-side retry
// loop encode retriable errors as CloudError, keeping ok_to_retry set. The
// Director has no behavior to attach to the flag there, but the type is on
// its known list, so the message lands in the task record instead of an
// "Unknown CPI error" wrapper.
func TestWireType_RetriableOnOtherMethods(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"attach_disk", "delete_vm", "create_disk", "has_vm"} {
		body := dispatchTypedError(t, method, cpierrors.Retriable("lock timeout"))
		if body.Type != string(cpierrors.TypeCloud) {
			t.Errorf("%s: type = %q, want %q", method, body.Type, cpierrors.TypeCloud)
		}
		if !body.OkToRetry {
			t.Errorf("%s: ok_to_retry = false, want true", method)
		}
	}
}

// TestWireType_DetachedDiskBecomesDiskNotAttached: the internal DetachedDisk
// type translates to the Director's name for the same condition.
func TestWireType_DetachedDiskBecomesDiskNotAttached(t *testing.T) {
	t.Parallel()
	body := dispatchTypedError(t, "update_disk",
		cpierrors.DetachedDisk("disk %s is not attached", "disk-1"))
	if body.Type != string(cpierrors.TypeDiskNotAttached) {
		t.Errorf("type = %q, want %q", body.Type, cpierrors.TypeDiskNotAttached)
	}
	if body.OkToRetry {
		t.Error("ok_to_retry = true, want false")
	}
}

// TestWireType_InternalDiagnosticsBecomeCloudError: the stemcell validation
// types and SnapshotBlocked are internal diagnostics; on the wire they are
// plain non-retriable CloudErrors.
func TestWireType_InternalDiagnosticsBecomeCloudError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *cpierrors.Error
	}{
		{"snapshot_blocked", cpierrors.SnapshotBlocked("vm has active snapshots")},
		{"stemcell_extract_cap", cpierrors.StemcellExtractCap("declared sizes exceed cap")},
		{"stemcell_magic_mismatch", cpierrors.StemcellMagicMismatch("unknown image magic")},
		{"stemcell_no_candidate", cpierrors.StemcellNoCandidate("no disk image in tarball")},
		{"stemcell_escaped_root", cpierrors.StemcellEscapedRoot("path escapes staging root")},
		{"stemcell_invalid_tar", cpierrors.StemcellInvalidTar("negative entry size")},
	}
	for _, tc := range cases {
		body := dispatchTypedError(t, "create_stemcell", tc.err)
		if body.Type != string(cpierrors.TypeCloud) {
			t.Errorf("%s: type = %q, want %q", tc.name, body.Type, cpierrors.TypeCloud)
		}
		if body.OkToRetry {
			t.Errorf("%s: ok_to_retry = true, want false", tc.name)
		}
	}
}

// TestWireType_KnownTypesPassThrough: types already on the Director's list
// cross the wire unchanged.
func TestWireType_KnownTypesPassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		err    *cpierrors.Error
		want   cpierrors.Type
	}{
		{"has_vm", cpierrors.VMNotFound("vm-1"), cpierrors.TypeVMNotFound},
		{"attach_disk", cpierrors.DiskNotFound("disk-1"), cpierrors.TypeDiskNotFound},
		{"resize_disk", cpierrors.NotSupported("shrink", "disks only grow"), cpierrors.TypeNotSupported},
		{"delete_vm", cpierrors.Cloud("boom"), cpierrors.TypeCloud},
	}
	for _, tc := range cases {
		body := dispatchTypedError(t, tc.method, tc.err)
		if body.Type != string(tc.want) {
			t.Errorf("%s: type = %q, want %q", tc.method, body.Type, tc.want)
		}
	}
}
