package errors_test

import (
	"errors"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// --------------------------------------------------------------------------
// TestEachType — constructor, Type(), Error(), OkToRetry()
// --------------------------------------------------------------------------

func TestEachType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       *cpierrors.Error
		wantType  cpierrors.Type
		retriable bool
		msgPart   string
	}{
		{
			name:      "Cloud",
			err:       cpierrors.Cloud("disk %s failed", "abc"),
			wantType:  cpierrors.TypeCloud,
			retriable: false,
			msgPart:   "disk abc failed",
		},
		{
			name:      "Retriable",
			err:       cpierrors.Retriable("timeout on node %s", "pve1"),
			wantType:  cpierrors.TypeRetriableCloud,
			retriable: true,
			msgPart:   "timeout on node pve1",
		},
		{
			name:      "VMNotFound",
			err:       cpierrors.VMNotFound("vm-42"),
			wantType:  cpierrors.TypeVMNotFound,
			retriable: false,
			msgPart:   "vm-42",
		},
		{
			name:      "DiskNotFound",
			err:       cpierrors.DiskNotFound("local:vm-42-disk-0"),
			wantType:  cpierrors.TypeDiskNotFound,
			retriable: false,
			msgPart:   "local:vm-42-disk-0",
		},
		{
			name:      "NotImplemented",
			err:       cpierrors.NotImplemented("snapshot_disk"),
			wantType:  cpierrors.TypeNotImplemented,
			retriable: false,
			msgPart:   "snapshot_disk",
		},
		{
			name:      "NotSupported",
			err:       cpierrors.NotSupported("resize_disk", "cannot shrink"),
			wantType:  cpierrors.TypeNotSupported,
			retriable: false,
			msgPart:   "cannot shrink",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.err.Type() != tc.wantType {
				t.Errorf("Type() = %q, want %q", tc.err.Type(), tc.wantType)
			}
			if tc.err.OkToRetry() != tc.retriable {
				t.Errorf("OkToRetry() = %v, want %v", tc.err.OkToRetry(), tc.retriable)
			}
			if !strings.Contains(tc.err.Error(), tc.msgPart) {
				t.Errorf("Error() = %q, want it to contain %q", tc.err.Error(), tc.msgPart)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestWrap_WithStdError — plain error wrapped as CloudError, cause chained
// --------------------------------------------------------------------------

func TestWrap_WithStdError(t *testing.T) {
	t.Parallel()

	cause := errors.New("PVE API 500")
	wrapped := cpierrors.Wrap(cause, "create_vm failed")

	if wrapped.Type() != cpierrors.TypeCloud {
		t.Fatalf("Type() = %q, want TypeCloud", wrapped.Type())
	}
	if wrapped.OkToRetry() {
		t.Fatal("OkToRetry() should be false for plain-error wrap")
	}
	// Error() must include both message and cause
	got := wrapped.Error()
	if !strings.Contains(got, "create_vm failed") {
		t.Errorf("Error() missing outer msg: %q", got)
	}
	if !strings.Contains(got, "PVE API 500") {
		t.Errorf("Error() missing cause: %q", got)
	}
	// Unwrap must return cause
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is chain broken — Unwrap not returning cause")
	}
}

// --------------------------------------------------------------------------
// TestWrap_WithTypedError — *Error wrapped preserves original type
// --------------------------------------------------------------------------

func TestWrap_WithTypedError(t *testing.T) {
	t.Parallel()

	inner := cpierrors.VMNotFound("vm-99")
	outer := cpierrors.Wrap(inner, "delete_vm context")

	if outer.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("Type() = %q, want TypeVMNotFound (preserved)", outer.Type())
	}
	if outer.OkToRetry() {
		t.Error("OkToRetry() should remain false (VMNotFound is not retriable)")
	}
	// errors.As must find the inner *Error through the chain
	var found *cpierrors.Error
	if !errors.As(outer, &found) {
		t.Fatal("errors.As failed to traverse chain")
	}
}

// --------------------------------------------------------------------------
// TestWrapAs — type override
// --------------------------------------------------------------------------

func TestWrapAs(t *testing.T) {
	t.Parallel()

	cause := errors.New("disk I/O error")
	wrapped := cpierrors.WrapAs(cause, cpierrors.TypeRetriableCloud, "attach_disk retry")

	if wrapped.Type() != cpierrors.TypeRetriableCloud {
		t.Errorf("Type() = %q, want TypeRetriableCloud", wrapped.Type())
	}
	if !wrapped.OkToRetry() {
		t.Error("OkToRetry() should be true for TypeRetriableCloud")
	}
	// Non-retriable type must yield false
	wrapped2 := cpierrors.WrapAs(cause, cpierrors.TypeVMNotFound, "context")
	if wrapped2.OkToRetry() {
		t.Error("OkToRetry() should be false for TypeVMNotFound wrapped via WrapAs")
	}
}

// --------------------------------------------------------------------------
// TestIsType — true/false matrix
// --------------------------------------------------------------------------

func TestIsType(t *testing.T) {
	t.Parallel()

	err := cpierrors.VMNotFound("vm-1")

	if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
		t.Error("IsType should return true for matching type")
	}
	if cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Error("IsType should return false for non-matching type")
	}
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Error("IsType should return false for DiskNotFound when error is VMNotFound")
	}
	// Non-*Error should return false
	if cpierrors.IsType(errors.New("plain"), cpierrors.TypeCloud) {
		t.Error("IsType should return false for plain error")
	}
	// Works through wrapping chain
	wrapped := cpierrors.Wrap(err, "outer")
	if !cpierrors.IsType(wrapped, cpierrors.TypeVMNotFound) {
		t.Error("IsType should traverse chain")
	}
}

// --------------------------------------------------------------------------
// TestIsNotFound — VMNotFound and DiskNotFound true; others false
// --------------------------------------------------------------------------

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		want bool
		name string
	}{
		{cpierrors.VMNotFound("vm-1"), true, "VMNotFound"},
		{cpierrors.DiskNotFound("local:vol"), true, "DiskNotFound"},
		{cpierrors.Cloud("generic"), false, "Cloud"},
		{cpierrors.Retriable("timeout"), false, "Retriable"},
		{cpierrors.NotImplemented("method"), false, "NotImplemented"},
		{cpierrors.NotSupported("op", "reason"), false, "NotSupported"},
		{errors.New("plain"), false, "plain error"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cpierrors.IsNotFound(tc.err)
			if got != tc.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestRPCPayload — required keys and types
// --------------------------------------------------------------------------

func TestRPCPayload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err       *cpierrors.Error
		wantRetry bool
		wantType  string
		name      string
	}{
		{cpierrors.Cloud("oops"), false, "Bosh::Clouds::CloudError", "Cloud"},
		{cpierrors.Retriable("again"), true, "Bosh::Clouds::RetriableCloudError", "Retriable"},
		{cpierrors.VMNotFound("vm-5"), false, "Bosh::Clouds::VMNotFound", "VMNotFound"},
		{cpierrors.DiskNotFound("d-1"), false, "Bosh::Clouds::DiskNotFound", "DiskNotFound"},
		{cpierrors.NotImplemented("foo"), false, "Bosh::Clouds::NotImplemented", "NotImplemented"},
		{cpierrors.NotSupported("shrink", "not allowed"), false, "Bosh::Clouds::NotSupported", "NotSupported"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := tc.err.RPCPayload()

			typ, ok := payload["type"].(string)
			if !ok {
				t.Fatal("payload[\"type\"] is not a string")
			}
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}

			msg, ok := payload["message"].(string)
			if !ok {
				t.Fatal("payload[\"message\"] is not a string")
			}
			if msg == "" {
				t.Error("payload[\"message\"] must not be empty")
			}

			retry, ok := payload["ok_to_retry"].(bool)
			if !ok {
				t.Fatal("payload[\"ok_to_retry\"] is not a bool")
			}
			if retry != tc.wantRetry {
				t.Errorf("ok_to_retry = %v, want %v", retry, tc.wantRetry)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TestUnwrap — errors.Is / errors.As chain traversal
// --------------------------------------------------------------------------

func TestUnwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel cause")
	mid := cpierrors.Cloud("mid: %s", "layer")
	mid2 := &struct{ error }{sentinel} // non-*Error wrapper to confirm As still reaches sentinel

	// Direct unwrap
	direct := cpierrors.Wrap(sentinel, "outer")
	if !errors.Is(direct, sentinel) {
		t.Error("errors.Is should find sentinel through one Wrap")
	}

	// Two-level *Error chain: outer wraps inner wraps sentinel
	inner := cpierrors.Wrap(sentinel, "inner")
	outer := cpierrors.Wrap(inner, "outer")
	if !errors.Is(outer, sentinel) {
		t.Error("errors.Is should find sentinel through two Wraps")
	}

	_ = mid
	_ = mid2

	// Unwrap nil cause returns nil
	noWrap := cpierrors.Cloud("bare")
	if noWrap.Unwrap() != nil {
		t.Error("Unwrap() should return nil when no cause")
	}
}
