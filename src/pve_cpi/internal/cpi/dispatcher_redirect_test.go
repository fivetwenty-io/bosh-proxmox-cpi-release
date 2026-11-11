package cpi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// Many handler paths label a PVE failure retriable, because it is usually a
// dropped connection. A refused redirect comes back the same on every retry,
// so the dispatcher sends it with ok_to_retry false, whichever layer refused
// it and whatever wrapping the handler added. For create_vm that keeps the
// Director from spending max_vm_create_tries on it.
func TestDispatchError_RefusedRedirectIsNeverRetriable(t *testing.T) {
	t.Parallel()

	sdkRefusal := &sdkclient.RedirectError{Status: http.StatusFound, From: "https://pve1:8006/api2/json/nodes"}
	cpiRefusal := sdkclient.Permanent(fmt.Errorf("%w from pve1:8006", endpointpool.ErrRedirectRefused))
	loginRefusal := fmt.Errorf("auto-login failed: %w",
		&sdkerrors.ConnectionError{Message: "login request failed", Cause: sdkRefusal})
	refusals := map[string]error{
		"the SDK's refusal":    fmt.Errorf("request failed after 1 attempt(s), not retried: %w", sdkRefusal),
		"the CPI's refusal":    fmt.Errorf("request failed after 1 attempt(s), not retried: %w", cpiRefusal),
		"a login refusal":      loginRefusal,
		"a mapped refusal":     pve.WrapError(loginRefusal),
		"a joined refusal":     errors.Join(errors.New("await the task"), sdkRefusal),
		"a doubly wrapped one": fmt.Errorf("delete_vm: %w", fmt.Errorf("list the pool: %w", sdkRefusal)),
	}
	for name, refusal := range refusals {
		for _, method := range []string{"create_vm", "delete_vm"} {
			t.Run(name+", "+method, func(t *testing.T) {
				t.Parallel()
				d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
				mustRegister(t, d, method, cpi.HandlerFunc(
					func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
						return nil, cpierrors.WrapAs(refusal, cpierrors.TypeRetriableCloud, method+": read PVE")
					}))
				resp := d.Handle(context.Background(), makeReq(method))
				if resp == nil || resp.Error == nil {
					t.Fatal("expected an error response")
				}
				if resp.Error.OkToRetry {
					t.Errorf("a refused redirect surfaced with ok_to_retry=true: %+v", resp.Error)
				}
				if resp.Error.Type != string(cpierrors.TypeCloud) {
					t.Errorf("error type = %q, want %q", resp.Error.Type, string(cpierrors.TypeCloud))
				}
				if !strings.Contains(resp.Error.Message, "redirect") || !strings.Contains(resp.Error.Message, method) {
					t.Errorf("message %q should name the redirect and keep the handler's context", resp.Error.Message)
				}
			})
		}
	}

	// A refusal a handler already made permanent keeps its type, and a raw
	// one, which no handler classified, is a plain permanent CloudError.
	keep := map[string]error{
		"a disk that is not there": cpierrors.WrapAs(sdkRefusal, cpierrors.TypeDiskNotFound, "disk"),
		"a raw refusal":            sdkRefusal,
	}
	wantType := map[string]cpierrors.Type{
		"a disk that is not there": cpierrors.TypeDiskNotFound,
		"a raw refusal":            cpierrors.TypeCloud,
	}
	for name, err := range keep {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
			mustRegister(t, d, "has_disk", cpi.HandlerFunc(
				func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
					return nil, err
				}))
			resp := d.Handle(context.Background(), makeReq("has_disk"))
			if resp == nil || resp.Error == nil {
				t.Fatal("expected an error response")
			}
			if resp.Error.OkToRetry || resp.Error.Type != string(wantType[name]) {
				t.Errorf("got %+v, want type %q with ok_to_retry=false", resp.Error, wantType[name])
			}
		})
	}
}

// A retriable error that carries no redirect keeps its retry flag.
func TestDispatchError_RetriableWithoutRedirectStaysRetriable(t *testing.T) {
	t.Parallel()

	d := cpi.NewDispatcherWithOptions(nopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
	mustRegister(t, d, "create_vm", cpi.HandlerFunc(
		func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			return nil, cpierrors.WrapAs(errors.New("connection reset by peer"), cpierrors.TypeRetriableCloud,
				"create_vm: read PVE")
		}))
	resp := d.Handle(context.Background(), makeReq("create_vm"))
	if resp == nil || resp.Error == nil {
		t.Fatal("expected an error response")
	}
	if !resp.Error.OkToRetry || resp.Error.Type != string(cpierrors.TypeVMCreationFailed) {
		t.Errorf("got %+v, want VMCreationFailed with ok_to_retry=true", resp.Error)
	}
}
