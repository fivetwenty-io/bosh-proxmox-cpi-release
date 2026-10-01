package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestCreateVMRetrySettlesTheCutOffRestore takes create_vm's route for a disk
// in disk_cids, the managed persistent attach, after a cut-off restore. With
// protection off it is refused with the qm set text; once the parker reads
// protected, it settles the restore step before readmission and attaches.
func TestCreateVMRetrySettlesTheCutOffRestore(t *testing.T) {
	t.Parallel()
	c := newCutOffRestore(t, 200*time.Millisecond)
	step := c.restoreStep(t)
	bare, meta, err := decodeDiskCID(c.ctx, c.deps, "create_vm", c.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(c.ctx, c.deps, "create_vm", c.cid, bare, meta)
	if err != nil {
		t.Fatalf("the disk the cut-off attach left on VM 777 does not resolve: %v", err)
	}

	c.setProtection(false)
	if _, err := attachManagedPersistentDisk(c.ctx, c.deps, "777", "n1", 777, rd); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("run qm set %d --protection 1", c.parker)) {
		t.Fatalf("create_vm attach with protection off = %v, want a refusal naming qm set", err)
	}

	c.setProtection(true)
	if _, err := attachManagedPersistentDisk(c.ctx, c.deps, "777", "n1", 777, rd); err != nil {
		t.Fatalf("create_vm attach after protection was put back: %v", err)
	}
	record := c.record(t)
	if got := stepState(t, record, step.ID); got != aj.Observed {
		t.Fatalf("restore step %s left %s after the create_vm retry", step.ID, got)
	}
	assertReturnedRecord(t, "create_vm retry", record)
}

// guardProbeClient serves one nodes service to a guard under test.
type guardProbeClient struct {
	pve.Client
	nodes nodes.Service
}

func (c guardProbeClient) Nodes() nodes.Service { return c.nodes }

// guardProbeNodes answers every configuration write with err, or blocks until
// the write's context ends when block is set.
type guardProbeNodes struct {
	nodes.Service
	err   error
	block bool
}

func (n guardProbeNodes) UpdateQemuConfig(ctx context.Context, _, _ string, _ *nodes.UpdateQemuConfigParams) error {
	if n.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return n.err
}

// TestGuardFinishesProtectionWritesWithoutLocking drives the allocation
// guard's finish directly. A protection-only write that PVE refused is
// observed through After and a cut-off one is left alone, and in both the
// guard stays usable. Every other failure still locks the guard and records
// uncertainty exactly as before: a non-protection write, a protection write
// carrying another field, a protection write that failed in transport before
// any deadline, and any write when the hooks did not opt in.
func TestGuardFinishesProtectionWritesWithoutLocking(t *testing.T) {
	t.Parallel()
	on, off := true, false
	description := "notes"
	refused := &sdkerrors.APIError{HTTPCode: 500, Message: "unable to update VM 90000"}
	cases := []struct {
		name       string
		params     *nodes.UpdateQemuConfigParams
		probe      guardProbeNodes
		optIn      bool
		wantLocked bool
		wantAfter  bool
	}{
		{"refused protection restore", &nodes.UpdateQemuConfigParams{Protection: &on}, guardProbeNodes{err: refused}, true, false, true},
		{"refused protection clear", &nodes.UpdateQemuConfigParams{Protection: &off}, guardProbeNodes{err: refused}, true, false, true},
		{"cut-off protection restore", &nodes.UpdateQemuConfigParams{Protection: &on}, guardProbeNodes{block: true}, true, false, false},
		{"failed description write", &nodes.UpdateQemuConfigParams{Description: &description}, guardProbeNodes{err: refused}, true, true, false},
		{"protection with another field", &nodes.UpdateQemuConfigParams{Protection: &on, Description: &description}, guardProbeNodes{err: refused}, true, true, false},
		{"protection transport failure", &nodes.UpdateQemuConfigParams{Protection: &on}, guardProbeNodes{err: errors.New("connection reset by peer")}, true, false, false},
		{"description transport failure", &nodes.UpdateQemuConfigParams{Description: &description}, guardProbeNodes{err: errors.New("connection reset by peer")}, true, true, false},
		{"cut-off description write", &nodes.UpdateQemuConfigParams{Description: &description}, guardProbeNodes{block: true}, true, true, false},
		{"guard that did not opt in", &nodes.UpdateQemuConfigParams{Protection: &on}, guardProbeNodes{err: refused}, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var afterResults []any
			failed := 0
			guard, err := NewManagedAllocationGuard(guardProbeClient{nodes: tc.probe}, ManagedAllocationHooks{
				Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "step", nil },
				After: func(_ context.Context, _ ManagedAllocationMutation, _ string, result any) error {
					afterResults = append(afterResults, result)
					return nil
				},
				Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
					failed++
					return errors.New("uncertain")
				},
				SettleProtectionWrites: tc.optIn,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			writeErr := guard.Client().Nodes().UpdateQemuConfig(ctx, "n1", "90000", tc.params)
			if writeErr == nil {
				t.Fatal("the failed write returned no error")
			}
			if locked := guard.Err() != nil; locked != tc.wantLocked {
				t.Fatalf("guard locked = %t (err %v), want %t", locked, guard.Err(), tc.wantLocked)
			}
			if tc.wantLocked != (failed == 1) {
				t.Fatalf("Failed ran %d times, want it exactly when the guard locks", failed)
			}
			if gotAfter := len(afterResults) == 1; gotAfter != tc.wantAfter {
				t.Fatalf("After ran %d times, want %t", len(afterResults), tc.wantAfter)
			}
			if tc.wantAfter {
				if _, ok := afterResults[0].(managedProtectionWriteRefusal); !ok {
					t.Fatalf("After received %T, want the refusal proof", afterResults[0])
				}
			}
			want := tc.probe.err
			if tc.probe.block {
				want = context.DeadlineExceeded
			}
			if !tc.wantLocked && !errors.Is(writeErr, want) {
				t.Fatalf("the guard replaced the write's own error %v with %v", want, writeErr)
			}
		})
	}
}

// countingProbeNodes counts the configuration writes that reach it and
// answers each one with success.
type countingProbeNodes struct {
	nodes.Service
	sent *int
}

func (n countingProbeNodes) UpdateQemuConfig(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
	*n.sent++
	return nil
}

// TestGuardBeginKeepsARestoreWhoseChecksDidNotFinish drives the allocation
// guard's begin with a Before that could not finish the checks for a parker
// protection restore. That Before has already recorded the restore as a
// planned step, so begin must hand its refusal back as it is, rather than as a
// write refused by an uncertain operation. It must not send the write, must
// not record uncertainty, and must leave the guard usable for the next write.
func TestGuardBeginKeepsARestoreWhoseChecksDidNotFinish(t *testing.T) {
	t.Parallel()
	on := true
	refusal := fmt.Errorf("parker configuration could not be checked before the parker protection restore: %w", errManagedRestoreChecksIncomplete)
	sent, befores, failed := 0, 0, 0
	guard, err := NewManagedAllocationGuard(guardProbeClient{nodes: countingProbeNodes{sent: &sent}}, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) {
			befores++
			if befores == 1 {
				return "", refusal
			}
			return "step", nil
		},
		After: func(context.Context, ManagedAllocationMutation, string, any) error { return nil },
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			failed++
			return errors.New("uncertain")
		},
		SettleProtectionWrites: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	params := &nodes.UpdateQemuConfigParams{Protection: &on}
	writeErr := guard.Client().Nodes().UpdateQemuConfig(t.Context(), "n1", "90000", params)
	if writeErr == nil || writeErr.Error() != refusal.Error() || errors.Is(writeErr, pve.ErrMutationNotAttempted) ||
		!errors.Is(writeErr, errManagedRestoreChecksIncomplete) {
		t.Fatalf("begin handed back %v, want the refusal %q unchanged", writeErr, refusal)
	}
	if guard.Err() != nil {
		t.Fatalf("the guard locked itself on a restore that Before recorded as planned: %v", guard.Err())
	}
	if sent != 0 || failed != 0 {
		t.Fatalf("the refused restore sent %d writes and recorded uncertainty %d times, want neither", sent, failed)
	}
	if err := guard.Client().Nodes().UpdateQemuConfig(t.Context(), "n1", "90000", params); err != nil {
		t.Fatalf("the next write after the refusal: %v", err)
	}
	if sent != 1 {
		t.Fatalf("the next write reached PVE %d times, want once", sent)
	}
}

// TestProtectionPredicateIsTheOnlyRule pins the one definition of a
// protection-only write that admission, finish, and the settler share.
func TestProtectionPredicateIsTheOnlyRule(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		`{"kind":"parker_protection_on","version":1}`:                true,
		`{"kind":"parker_protection_off","version":1}`:               true,
		`{"digest":"abc","kind":"parker_protection_on","version":1}`: true,
		`{"comment":"x","kind":"parker_protection_on","version":1}`:  false,
		`{"kind":"parker_protection_on","version":2}`:                false,
		`{"kind":"persistent_birth","version":1}`:                    false,
		`{"kind":"parker_protection_on"}`:                            false,
		``:                                                           false,
	} {
		if got := isParkerProtectionParameters(json.RawMessage(raw)); got != want {
			t.Errorf("isParkerProtectionParameters(%s) = %t, want %t", raw, got, want)
		}
	}
	for name, tc := range map[string]struct {
		fields map[string]any
		want   bool
	}{
		"restore":            {map[string]any{"protection": true}, true},
		"clear with digest":  {map[string]any{"protection": false, "digest": "abc"}, true},
		"with a description": {map[string]any{"protection": true, "description": "x"}, false},
		"no protection":      {map[string]any{"digest": "abc"}, false},
	} {
		if got := isParkerProtectionParameters(parkerProtectionStepParameters(tc.fields)); got != tc.want {
			t.Errorf("%s: rendered parameters satisfy the predicate = %t, want %t", name, got, tc.want)
		}
	}
}
