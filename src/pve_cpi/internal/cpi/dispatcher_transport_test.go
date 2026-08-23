package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// TestDispatchError_RawTransientTransportIsRetriable is the last-resort
// classification test for the dispatcher's plain-error fallback. Every live
// handler path classifies before returning, so no raw transport error should
// reach dispatchError today; this is defense in depth for the path that a
// future handler regression would take. A raw *url.Error carrying io.EOF (a
// mid-request connection drop) must surface with ok_to_retry=true, not as the
// permanent generic CloudError the fallback used to mint. The wire type is
// CloudError, not RetriableCloudError: the Director's known-error list has no
// RetriableCloudError entry, and for a method without a Director-side retry
// loop CloudError plus the flag is the closest legal encoding.
func TestDispatchError_RawTransientTransportIsRetriable(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
	rawDrop := &url.Error{Op: "Post", URL: "https://pve:8006/api2/json/nodes", Err: io.EOF}
	mustRegister(t, d, "has_vm", cpi.HandlerFunc(
		func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, rawDrop
		}))

	resp := d.Handle(context.Background(), makeReq("has_vm"))
	if resp == nil || resp.Error == nil {
		t.Fatal("expected an error response")
	}

	if !resp.Error.OkToRetry {
		t.Errorf("raw transient transport error surfaced with ok_to_retry=false: %+v", resp.Error)
	}

	if resp.Error.Type != string(cpierrors.TypeCloud) {
		t.Errorf("error type = %q, want %q", resp.Error.Type, string(cpierrors.TypeCloud))
	}
}

// TestDispatchError_RawTransientTransportOnCreateVM pins the create_vm arm of
// the same fallback: the one method the Director retries needs the
// VMCreationFailed wire type for ok_to_retry to reach its create step.
func TestDispatchError_RawTransientTransportOnCreateVM(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
	rawDrop := &url.Error{Op: "Post", URL: "https://pve:8006/api2/json/nodes", Err: io.EOF}
	mustRegister(t, d, "create_vm", cpi.HandlerFunc(
		func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, rawDrop
		}))

	resp := d.Handle(context.Background(), makeReq("create_vm"))
	if resp == nil || resp.Error == nil {
		t.Fatal("expected an error response")
	}

	if !resp.Error.OkToRetry {
		t.Errorf("raw transient transport error surfaced with ok_to_retry=false: %+v", resp.Error)
	}

	if resp.Error.Type != string(cpierrors.TypeVMCreationFailed) {
		t.Errorf("error type = %q, want %q", resp.Error.Type, string(cpierrors.TypeVMCreationFailed))
	}
}

// TestDispatchError_RawPlainErrorStaysPermanent pins the existing fallback
// for a plain error with no transport signal: still a non-retriable
// CloudError.
func TestDispatchError_RawPlainErrorStaysPermanent(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
	mustRegister(t, d, "has_disk", cpi.HandlerFunc(
		func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, errors.New("handler forgot to classify this")
		}))

	resp := d.Handle(context.Background(), makeReq("has_disk"))
	if resp == nil || resp.Error == nil {
		t.Fatal("expected an error response")
	}

	if resp.Error.OkToRetry {
		t.Errorf("plain error surfaced with ok_to_retry=true: %+v", resp.Error)
	}
}
