package allocationjournal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func retentionProof(t *testing.T, r Record) Verification {
	t.Helper()
	target := r.Steps[0].Target
	target.IntendedVolume = r.Steps[0].VolIDs[0]
	id, payload, err := VerificationEvidence(struct {
		VMRetentionEvidence
		ObservedAt time.Time `json:"observed_at"`
	}{VMRetentionEvidence{VMID: 101, RetainedArtifacts: []Target{target}}, time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}
}

func TestVMRetentionClosesGenerationWithoutClosingResources(t *testing.T) {
	j, _ := fixture(t)
	h := acquireVM(t, j)
	observed(t, h)
	r := h.Record()
	oldID := r.ID
	r.State = VMDeletedRetained
	r.Verifications = append(r.Verifications, retentionProof(t, r))
	save(t, h, r)
	closeHandle(t, h)
	if _, found, err := j.InspectVMContext(context.Background(), "agent"); err != nil || found {
		t.Fatalf("retained generation remains active: found=%v err=%v", found, err)
	}
	next := acquireVM(t, j)
	if next.Record().ID == oldID || next.Resumed {
		t.Fatal("retained allocation resumed as new VM")
	}
	newID := next.Record().ID
	closeHandle(t, next)
	retained, err := j.Acquire(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	r = retained.Record()
	r.Verifications = append(r.Verifications, retentionProof(t, r))
	r.Steps = append(r.Steps, Step{ID: "retained-delete", Kind: "delete-volume", Target: r.Steps[0].Target, State: Planned})
	r.Reason = "cleanup response unknown"
	save(t, retained, r)
	for _, state := range []State{Planned, Observed, ReadyToReturn, ReconciliationRequired, Adopted} {
		changed := retained.Record()
		changed.State = state
		if err := retained.Save(changed); err == nil {
			t.Fatalf("retained VM reactivated as %s", state)
		}
	}
	r = retained.Record()
	r.Steps[1].State = Observed
	save(t, retained, r)
	r = retained.Record()
	r.State = Cleaned
	proofID, proofJSON, proofErr := VerificationEvidence(map[string]any{"operation": "retained_cleanup", "complete_absence": true})
	if proofErr != nil {
		t.Fatal(proofErr)
	}
	r.Verifications = append(r.Verifications, Verification{EvidenceID: proofID, EvidenceJSON: proofJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
	save(t, retained, r)
	closeHandle(t, retained)
	active, found, err := j.InspectVMContext(context.Background(), "agent")
	if err != nil || !found || active.ID != newID {
		t.Fatalf("cleanup reopened or displaced generation: %+v %v %v", active, found, err)
	}
}

func TestVMRetentionRejectsUnprovenOrForeignArtifacts(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Record)
	}{
		{"missing durable proof", func(r *Record) { r.Verifications[0].EvidenceJSON = "" }},
		{"whole allocation absence", func(r *Record) { r.Verifications[0].AbsenceVerified = true }},
		{"no VM absence", func(r *Record) { r.Verifications[0].VMAbsenceVerified = false }},
		{"no disposition", func(r *Record) { r.Verifications[0].ArtifactDispositionVerified = false }},
		{"unsettled mutation", func(r *Record) {
			r.Steps = append(r.Steps, Step{ID: "pending", Kind: "delete", Target: r.Steps[0].Target, State: Planned})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := fixture(t)
			h := acquireVM(t, j)
			defer closeHandle(t, h)
			observed(t, h)
			r := h.Record()
			r.State = VMDeletedRetained
			r.Verifications = []Verification{retentionProof(t, r)}
			tc.change(&r)
			if err := h.Save(r); err == nil {
				t.Fatal("invalid retention accepted")
			}
		})
	}
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign backing", true: "external target"}[external], func(t *testing.T) {
			j, _ := fixture(t)
			h := acquireVM(t, j)
			defer closeHandle(t, h)
			observed(t, h)
			r := h.Record()
			r.State = VMDeletedRetained
			target := r.Steps[0].Target
			target.IntendedVolume = r.Steps[0].VolIDs[0]
			if external {
				target.External = true
			} else {
				target.Backing = "foreign"
			}
			id, payload, err := VerificationEvidence(VMRetentionEvidence{VMID: 101, RetainedArtifacts: []Target{target}})
			if err != nil {
				t.Fatal(err)
			}
			r.Verifications = []Verification{{EvidenceID: id, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}}
			if err := h.Save(r); err == nil {
				t.Fatal("foreign retention accepted")
			}
		})
	}
}

func TestVMRetentionPreventsClusterRebindAndSurvivesIndexRecovery(t *testing.T) {
	j, dir := fixture(t)
	h := acquireVM(t, j)
	observed(t, h)
	r := h.Record()
	r.State = VMDeletedRetained
	r.Verifications = []Verification{retentionProof(t, r)}
	save(t, h, r)
	id := r.ID
	closeHandle(t, h)
	e := enrollment()
	e.ClusterID = "different-cluster"
	e.AuditID = "fresh-audit"
	if err := j.RecoverAuthority(context.Background(), e); err == nil {
		t.Fatal("retained live resources permitted cluster rebind")
	}
	e = enrollment()
	e.AuditID = "recover-retention"
	if err := j.RecoverIndex(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "namespace", e.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	next := acquireVM(t, reopened)
	if next.Record().ID == id {
		t.Fatal("index repair reactivated retained allocation")
	}
	closeHandle(t, next)
	if _, err := reopened.Inspect(id); err != nil {
		t.Fatal(err)
	}
}

func TestVMRetentionOnlyAcceptsVMRecords(t *testing.T) {
	j, _ := fixture(t)
	id, err := NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.CreateDisk(context.Background(), id, intent())
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(t, h)
	observed(t, h)
	r := h.Record()
	r.State = VMDeletedRetained
	r.Verifications = []Verification{retentionProof(t, r)}
	if err := h.Save(r); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("disk retention state: %v", err)
	}
}
