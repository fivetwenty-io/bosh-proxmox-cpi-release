package handlers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestManagedVMDeleteAbsenceConvergencePreservesMutationHistory(t *testing.T) {
	for _, mode := range []string{"converges", "permission uncertainty", "listing uncertainty", "deadline", "cancelled", "cancel during proof"} {
		t.Run(mode, func(t *testing.T) { testVMDeleteConvergence(t, mode) })
	}
}
func testVMDeleteConvergence(t *testing.T, mode string) {
	t.Helper()
	deps, j, _, record := deleteManagedFixture(t)
	h, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	admitVMDeleteObservationTest(t, deps, j, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls, mutations := 0, 0
	var reasons []string
	original := h.Record().Steps
	err = managedVMDeleteTask(ctx, deps, h, "volume", aj.Target{Node: "pve1", Storage: "a", IntendedVolume: cleanupTestISO}, func() (any, error) { mutations++; return "UPID:pve1:test", nil }, func() error {
		probeCtx := ctx
		if mode == "deadline" {
			var stop context.CancelFunc
			probeCtx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer stop()
		}
		if mode == "cancelled" {
			cancel()
		}
		return pollManagedVMVolumeAbsence(probeCtx, time.Microsecond, func(context.Context) (bool, error) {
			calls++
			if mode == "cancel during proof" {
				cancel()
				return false, nil
			}
			if mode == "permission uncertainty" {
				if calls == 2 {
					cancel()
				}
				return false, errors.New("managed volume content visibility unproven")
			}
			if mode == "listing uncertainty" {
				if calls == 2 {
					cancel()
				}
				return false, errors.New("managed volume content listing unavailable")
			}
			return calls < 3, nil
		}, func(reason string) { reasons = append(reasons, reason) })
	})
	got := h.Record()
	if mutations != 1 {
		t.Fatalf("mutation count %d: %v", mutations, err)
	}
	if !reflect.DeepEqual(original, got.Steps[:len(original)]) {
		t.Fatal("earlier history changed")
	}
	last := got.Steps[len(got.Steps)-1]
	if mode == "converges" {
		if err != nil || calls != 3 || last.State != aj.Observed {
			t.Fatalf("convergence failed: %v calls %d state %s", err, calls, last.State)
		}
		if !reflect.DeepEqual(reasons, []string{"still_present", "still_present"}) {
			t.Fatal("missing observation diagnostics")
		}
	} else {
		if err == nil || last.State != aj.Submitted || last.UPID != "UPID:pve1:test" {
			t.Fatal("uncertain absence settled mutation")
		}
		if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline lost")
		}
		if mode != "deadline" && !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost")
		}
	}

}
func TestManagedVMAbsenceDiagnosticReasonsAreBounded(t *testing.T) {
	for _, tc := range []struct{ message, want string }{
		{"managed volume content listing unavailable", "listing_unavailable"},
		{"managed volume content listing malformed", "listing_malformed"},
		{"managed volume content listing target mismatch", "listing_target_mismatch"},
		{"managed volume content visibility proof unavailable", "visibility_unavailable"},
		{"managed volume content visibility unproven", "visibility_unproven"},
		{"https://user:secret@host/volume?token=private", "observation_unavailable"},
	} {
		got := managedVMVolumeAbsenceReason(false, errors.New(tc.message))
		if got != tc.want || strings.Contains(got, "secret") {
			t.Fatal("unsafe diagnostic reason")
		}
	}
}

func admitVMDeleteObservationTest(t *testing.T, deps Deps, j *aj.Journal, h *aj.Handle) {
	t.Helper()
	active := h.Record()
	active.State = aj.ReconciliationRequired
	active.Reason = "delete admitted"
	if err := h.Save(active); err != nil {
		t.Fatal(err)
	}
	audit, err := AuditStorageAllocations(t.Context(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	proof, err := storageAllocationVerification(audit, map[string]any{"operation": "test delete admission"})
	if err != nil {
		t.Fatal(err)
	}
	proof.OwnershipVerified = true
	active = h.Record()
	active.State = aj.Observed
	active.Reason = ""
	active.Verifications = append(active.Verifications, proof)
	if err := h.Save(active); err != nil {
		t.Fatal(err)
	}
}

type absenceDiagnosticClient struct{ pve.Client }
type absenceDiagnosticNodes struct{ nodes.Service }

func (*absenceDiagnosticClient) Nodes() nodes.Service { return &absenceDiagnosticNodes{} }
func (*absenceDiagnosticNodes) ListStorageContent(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	return nil, &sdkerrors.APIError{HTTPCode: 503, Message: "https://user:secret@host/private"}
}
func TestManagedVMAbsenceDiagnosticPreservesTypedListingCause(t *testing.T) {
	present, err := pve.ObserveStorageVolumePresence(t.Context(), &absenceDiagnosticClient{}, "pve1", "nfs:iso/vm-100-config.iso")
	if err == nil || present {
		t.Fatal("failed listing proved absence")
	}
	if got := managedVMVolumeAbsenceReason(present, err); got != "listing_http_503" {
		t.Fatalf("reason=%q", got)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("backend response leaked")
	}
}
