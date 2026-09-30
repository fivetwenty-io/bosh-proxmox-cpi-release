package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// unsettledRetainStep is the refusal a delete_vm retry meets when the first
// delete failed its ephemeral retention transfer and left that step planned.
const unsettledRetainStep = "cleanup has unresolved mutation evidence; step attempt-0-step-7 (lifecycle_delete_vm_retain_ephemeral_Nodes_CreateQemuMoveDisk) is planned"

// TestDeleteVMRetryNamesTheUnsettledStep fails a delete_vm's retention
// transfer and retries it. The retry refuses on the planned transfer step,
// and the error HandleDeleteVM returns names that step instead of the generic
// evidence line, for a VM that stayed on its node and for one that moved.
func TestDeleteVMRetryNamesTheUnsettledStep(t *testing.T) {
	for _, moved := range []bool{false, true} {
		deps, client, _, _ := deleteWindowFixture(t, moved)
		client.moveErr = errors.New("transfer refused")
		args := []json.RawMessage{planJSON(t, "777")}
		if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
			t.Fatalf("moved=%t: a failed retention transfer reported success", moved)
		}
		client.moveErr = nil
		_, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{})
		var refusal *storageRefusalError
		if !errors.As(err, &refusal) || err.Error() != unsettledRetainStep {
			t.Fatalf("moved=%t: retry error = %v, want %q", moved, err, unsettledRetainStep)
		}
		var generic *storageDecisionObservationError
		if errors.As(err, &generic) {
			t.Fatalf("moved=%t: the refusal is still wrapped in the generic evidence line", moved)
		}
	}
}

// TestDeleteVMRetryShowsTheDirectorTheUnsettledStep runs the same two
// deletes through the dispatcher and reads the error body the Director
// receives. The first delete reports that the allocation requires
// reconciliation, and the retry reports the step that refused it. Both are
// non-retriable CloudErrors, as they were when the retry carried the generic
// line.
func TestDeleteVMRetryShowsTheDirectorTheUnsettledStep(t *testing.T) {
	for _, moved := range []bool{false, true} {
		deps, client, _, vmID := deleteWindowFixture(t, moved)
		d := cpi.NewDispatcher(log.NewNopLogger())
		if err := d.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
			t.Fatal(err)
		}
		request := &jsonrpc.Request{Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")}}

		client.moveErr = errors.New("transfer refused")
		first := d.Handle(context.Background(), request)
		want := "allocation " + vmID + " requires reconciliation at VM cleanup; no alternate allocation was attempted"
		if first.Error == nil || first.Error.Type != string(cpierrors.TypeCloud) || first.Error.OkToRetry || first.Error.Message != want {
			t.Fatalf("moved=%t: first delete reached the Director as %+v, want %q", moved, first.Error, want)
		}

		client.moveErr = nil
		retry := d.Handle(context.Background(), request)
		if retry.Error == nil || retry.Error.Type != string(cpierrors.TypeCloud) || retry.Error.OkToRetry || retry.Error.Message != unsettledRetainStep {
			t.Fatalf("moved=%t: retry reached the Director as %+v, want %q", moved, retry.Error, unsettledRetainStep)
		}
		if strings.Contains(retry.Error.Message, "could not verify or persist evidence") {
			t.Fatalf("moved=%t: retry carries the generic evidence line", moved)
		}
	}
}

// TestManagedDeleteVMFailuresReachTheDirector sends one failure of each kind
// a journal-managed delete can end in through the production dispatcher and
// reads the error body the Director receives. A refusal the CPI wrote, an
// audit gate's findings, and a lock timeout keep their own text. A journal
// write that failed, a PVE answer, and a cancelled request keep the generic
// evidence line, and a transient PVE fault stays retriable. Every type and
// retry flag is the one the same failure carried behind the generic line.
func TestManagedDeleteVMFailuresReachTheDirector(t *testing.T) {
	const generic = "allocation decision could not verify or persist evidence"
	gate := cpierrors.WrapAs(&storageAuditGateFailure{summary: "1 audit conflict; VM 777 on n2, recorded n1 (node_mismatch)", hint: "run 'cpi storage-journal audit --summary' for the full report"}, cpierrors.TypeCloud, "VM cleanup refused")
	persistence := fmt.Errorf("settling lock step attempt-0-step-2 (vm.Pool.CreatePool): %w", &fs.PathError{Op: "rename", Path: "/var/vcap/store/pve_cpi/journal/a.json", Err: syscall.ENOSPC})
	for _, tc := range []struct {
		name      string
		err       error
		message   string
		okToRetry bool
	}{
		{"refusal naming a step", storageRefusal(unsettledRetainStep), unsettledRetainStep, false},
		{"refusal without a step", storageRefusal("retired VM identity is present; audit required"), "retired VM identity is present; audit required", false},
		{"audit gate", gate, gate.Error(), false},
		{"lock timeout", cpierrors.Retriable("cluster lock bosh-lock-vm-90001 acquire timed out"), "cluster lock bosh-lock-vm-90001 acquire timed out", true},
		{"journal persistence", persistence, generic, false},
		{"permanent PVE answer", fmt.Errorf("read config: %w", sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed"}`))), generic, false},
		{"transient PVE fault", fmt.Errorf("read config: %w", sdkerrors.ParseAPIError(503, []byte(`{"message":"proxy loop"}`))), "transient transport fault: " + generic, true},
		{"cancelled request", context.Canceled, generic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithTransientClassifier(pve.IsTransientTransport))
			failing := cpi.HandlerFunc(func(context.Context, []json.RawMessage, jsonrpc.Context) (any, error) {
				return nil, managedDeleteVMError(tc.err)
			})
			if err := d.Register("delete_vm", failing); err != nil {
				t.Fatal(err)
			}
			body := d.Handle(context.Background(), &jsonrpc.Request{Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")}}).Error
			if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry != tc.okToRetry || body.Message != tc.message {
				t.Fatalf("Director received %+v, want CloudError ok_to_retry=%t %q", body, tc.okToRetry, tc.message)
			}
		})
	}
	if managedDeleteVMError(nil) != nil {
		t.Fatal("a delete that succeeded reported an error")
	}
}
