package handlers

// These rows cover what the identity check reads from a disk's allocation
// record before its claim-only resume runs, which is the first step that
// isn't settled and the moves the record observed.

import (
	"reflect"
	"slices"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestUnsettledRecordStepSkipsAClosedAttemptsProvenSteps covers a record whose
// first attempt was closed with a complete proof that its steps left nothing
// behind, and whose second attempt is active. A step of the closed attempt
// that never reached observed is settled by that proof, so the identity check
// doesn't refuse the disk over it. A step of the active attempt that isn't
// observed is still unsettled, and so is a closed attempt's step when the
// attempt's proof isn't complete.
func TestUnsettledRecordStepSkipsAClosedAttemptsProvenSteps(t *testing.T) {
	complete := &aj.AttemptVerification{
		Verification:         aj.Verification{Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true},
		NoSubmissionVerified: true,
	}
	partial := &aj.AttemptVerification{Verification: aj.Verification{Complete: true}}
	closedStep := aj.Step{Attempt: 0, ID: "attempt-0-step-1", Kind: "create_disk_Nodes_CreateStorageContent", State: aj.Planned}
	activeStep := aj.Step{Attempt: 1, ID: "attempt-1-step-2", Kind: "lifecycle_detach_disk_QEMU_AttachDisk", State: aj.Planned}
	observed := aj.Step{Attempt: 1, ID: "attempt-1-step-1", Kind: "create_disk_Nodes_CreateStorageContent", State: aj.Observed}
	for _, tc := range []struct {
		name  string
		proof *aj.AttemptVerification
		steps []aj.Step
		want  string
	}{
		{"closed attempt proven", complete, []aj.Step{closedStep, observed}, ""},
		{"active attempt unsettled", complete, []aj.Step{closedStep, observed, activeStep}, activeStep.ID},
		{"closed attempt unproven", partial, []aj.Step{closedStep, observed}, closedStep.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := aj.Record{Attempts: []aj.Attempt{{Number: 0, Completion: tc.proof}, {Number: 1}}, Steps: tc.steps}
			step, found := unsettledRecordStep(record)
			if got := map[bool]string{true: step.ID}[found]; got != tc.want {
				t.Fatalf("unsettled step = %q (found %v), want %q", step.ID, found, tc.want)
			}
		})
	}
}

// TestRecordedLandingsListsObservedMoves covers the moves the identity check
// passes to its claim-only resume. Only an observed move step that names both
// the volume it started with and the one it landed counts, and the landing
// names the VM the move ran on as its source.
func TestRecordedLandingsListsObservedMoves(t *testing.T) {
	const kind = "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk"
	record := aj.Record{Steps: []aj.Step{
		{ID: "landed", Kind: kind, State: aj.Observed, Target: aj.Target{VMID: 777}, VolIDs: []string{"a:vm-777-disk-1", "a:vm-90000-disk-3"}},
		{ID: "planned", Kind: kind, State: aj.Planned, Target: aj.Target{VMID: 777}, VolIDs: []string{"a:vm-777-disk-2", "a:vm-90000-disk-4"}},
		{ID: "no landing", Kind: kind, State: aj.Observed, Target: aj.Target{VMID: 777}, VolIDs: []string{"a:vm-777-disk-5"}},
		{ID: "not a move", Kind: "lifecycle_detach_disk_QEMU_AttachDisk", State: aj.Observed, Target: aj.Target{VMID: 90000}, VolIDs: []string{"a:vm-90000-disk-3", "a:vm-90000-disk-6"}},
	}}
	want := []pve.RecordedLanding{{SourceVMID: 777, From: "a:vm-777-disk-1", To: "a:vm-90000-disk-3"}}
	if got := recordedLandings(record, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded landings = %+v, want %+v", got, want)
	}
}

// TestRecordedLandingsLeaveOutALandingAMoveTookAway covers the later moves
// that take a landing away. A later move counts only when PVE landed the
// volume under another name, and only when it ran on the landing's node or
// the storage is shared. The VM it ran on doesn't matter.
func TestRecordedLandingsLeaveOutALandingAMoveTookAway(t *testing.T) {
	const kind = "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk"
	const from, landed = "a:vm-777-disk-1", "a:vm-90000-disk-3"
	landing := aj.Step{ID: "landed", Kind: kind, State: aj.Observed, Target: aj.Target{Node: "pve1", VMID: 777}, VolIDs: []string{from, landed}}
	want := pve.RecordedLanding{SourceVMID: 777, From: from, To: landed}
	for _, tc := range []struct {
		name   string
		later  aj.Step
		shared bool
		kept   bool
	}{
		{"moved off the parker", aj.Step{Target: aj.Target{Node: "pve1", VMID: 90000}, VolIDs: []string{landed, "a:vm-777-disk-2"}}, false, false},
		{"moved off another VM", aj.Step{Target: aj.Target{Node: "pve1", VMID: 778}, VolIDs: []string{landed, "a:vm-90001-disk-4"}}, false, false},
		{"refused", aj.Step{Target: aj.Target{Node: "pve1", VMID: 90000}, VolIDs: []string{landed}}, false, true},
		{"landed under the same name", aj.Step{Target: aj.Target{Node: "pve1", VMID: 90000}, VolIDs: []string{landed, landed}}, false, true},
		{"another node's volume", aj.Step{Target: aj.Target{Node: "pve2", VMID: 90000}, VolIDs: []string{landed, "a:vm-777-disk-2"}}, false, true},
		{"another node on shared storage", aj.Step{Target: aj.Target{Node: "pve2", VMID: 90000}, VolIDs: []string{landed, "a:vm-777-disk-2"}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			later := tc.later
			later.ID, later.Kind, later.State = "later", kind, aj.Observed
			got := recordedLandings(aj.Record{Steps: []aj.Step{landing, later}}, tc.shared)
			if kept := slices.Contains(got, want); kept != tc.kept {
				t.Fatalf("recorded landings = %+v, want the landing %+v kept %v", got, want, tc.kept)
			}
		})
	}
}
