package allocationjournal

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
)

// AttemptVersion identifies the frozen retry-plan schema.
const AttemptVersion = 1

// AttemptPlan freezes one ranking, including its seed and observations in Plan.
// FrozenInputsFingerprint binds the original resolved membership and constraints;
// a new ranking may refresh observations but must not admit new members.
// Executors must validate their own supported PlanVersion before using Plan.
type AttemptPlan struct {
	Version                 int             `json:"version"`
	PolicyFingerprint       string          `json:"policy_fingerprint"`
	FrozenInputsFingerprint string          `json:"frozen_inputs_fingerprint,omitempty"`
	PlanVersion             int             `json:"plan_version"`
	Plan                    json.RawMessage `json:"plan"`
}

// AttemptVerification is external evidence covering all historic targets of this
// attempt, including side effects whose submission response was never recorded.
// NoSubmissionVerified must come from authoritative reconciliation: absent UPID
// alone is not evidence. OutcomesKnown excludes running or unknown tasks that
// could still create resources after the absence observation.
type AttemptVerification struct {
	Verification
	OutcomesKnown        bool `json:"outcomes_known"`
	NoSubmissionVerified bool `json:"no_submission_verified"`
	RetainedArtifacts    bool `json:"retained_artifacts"`
}

// Attempt keeps the plan and complete cleanup evidence permanently. Closing an
// attempt does not close its allocation or permit another VM generation.
type Attempt struct {
	Number     int                  `json:"number"`
	Plan       AttemptPlan          `json:"plan"`
	Completion *AttemptVerification `json:"completion,omitempty"`
	Admission  *AttemptVerification `json:"admission,omitempty"`
}

func initialPlan(i Intent) AttemptPlan {
	return AttemptPlan{Version: AttemptVersion, PolicyFingerprint: i.PolicyFingerprint, FrozenInputsFingerprint: i.FrozenInputsFingerprint, PlanVersion: i.PlanVersion, Plan: append(json.RawMessage(nil), i.Plan...)}
}

// ActiveAttempt returns zero for a legacy record with an implicit initial plan.
func (r Record) ActiveAttempt() int {
	if len(r.Attempts) == 0 {
		return 0
	}
	return len(r.Attempts) - 1
}

// ActivePlan returns a defensive copy. Intent.Plan is always the original plan
// and must not be used as the resume target after another attempt begins.
func (r Record) ActivePlan() AttemptPlan {
	if len(r.Attempts) == 0 {
		return initialPlan(r.Intent)
	}
	p := r.Attempts[len(r.Attempts)-1].Plan
	p.Plan = append(json.RawMessage(nil), p.Plan...)
	return p
}
func cloneAttempts(a []Attempt) []Attempt {
	a = append([]Attempt(nil), a...)
	for i := range a {
		a[i].Plan.Plan = append(json.RawMessage(nil), a[i].Plan.Plan...)
		if a[i].Completion != nil {
			v := *a[i].Completion
			a[i].Completion = &v
		}
		if a[i].Admission != nil {
			v := *a[i].Admission
			a[i].Admission = &v
		}
	}
	return a
}
func attemptClosed(r Record) bool {
	return len(r.Attempts) > 0 && r.Attempts[len(r.Attempts)-1].Completion != nil
}
func activeStepCount(r Record) int {
	n := 0
	for sIndex := range r.Steps {
		s := r.Steps[sIndex]
		if s.Attempt == r.ActiveAttempt() {
			n++
		}
	}
	return n
}
func validateAttemptProof(r Record, number int, v AttemptVerification) error {
	if !validateVerification(v.Verification) || !v.AbsenceVerified || !v.ArtifactDispositionVerified || v.RetainedArtifacts || (!v.OutcomesKnown && !v.NoSubmissionVerified) {
		return fmt.Errorf("%w: complete absence, artifact disposition and settled submission outcomes required", ErrReconciliationRequired)
	}
	if v.NoSubmissionVerified {
		for sIndex := range r.Steps {
			s := r.Steps[sIndex]
			if s.Attempt != number {
				continue
			}
			if s.UPID != "" || len(s.VolIDs) > 0 || s.State == Submitted || s.State == Observed {
				return fmt.Errorf("%w: no-submission proof conflicts with recorded mutation", ErrConflict)
			}
			for _, c := range s.Charges {
				if c.AcquiredBytes > 0 {
					return fmt.Errorf("%w: no-submission proof conflicts with acquired bytes", ErrConflict)
				}
			}
		}
	}
	return nil
}
func validateAttempts(r Record) error {
	for i, a := range r.Attempts {
		p := a.Plan
		if a.Number != i || p.Version != AttemptVersion || p.PolicyFingerprint != r.Intent.PolicyFingerprint || p.FrozenInputsFingerprint != r.Intent.FrozenInputsFingerprint || validateIntent(Intent{IntentFingerprint: r.Intent.IntentFingerprint, PolicyFingerprint: p.PolicyFingerprint, FrozenInputsFingerprint: p.FrozenInputsFingerprint, PlanVersion: p.PlanVersion, Plan: p.Plan}) != nil {
			return fmt.Errorf("%w: invalid attempt plan/version/boundary", ErrCorrupt)
		}
		if i == 0 && !reflect.DeepEqual(p, initialPlan(r.Intent)) {
			return fmt.Errorf("%w: initial plan changed", ErrCorrupt)
		}
		if i == 0 && a.Admission != nil {
			return fmt.Errorf("%w: initial attempt has retry admission", ErrCorrupt)
		}
		if i > 0 {
			if a.Admission == nil {
				return fmt.Errorf("%w: retry lacks admission audit", ErrCorrupt)
			}
			if err := validateAttemptProof(r, i-1, *a.Admission); err != nil {
				return err
			}
			if !slices.Contains(r.Verifications, a.Admission.Verification) {
				return fmt.Errorf("%w: retry admission audit missing", ErrCorrupt)
			}
		}
		if i > 0 && !validFingerprint(p.FrozenInputsFingerprint) {
			return fmt.Errorf("%w: retry lacks frozen membership", ErrCorrupt)
		}
		if i < len(r.Attempts)-1 && a.Completion == nil {
			return fmt.Errorf("%w: prior attempt remains open", ErrCorrupt)
		}
		if a.Completion != nil {
			if err := validateAttemptProof(r, i, *a.Completion); err != nil {
				return err
			}
			if !slices.Contains(r.Verifications, a.Completion.Verification) {
				return fmt.Errorf("%w: missing attempt audit", ErrCorrupt)
			}
		}
	}
	return validateAttemptStepBounds(r)
}

func validateAttemptStepBounds(r Record) error {
	for sIndex := range r.Steps {
		s := r.Steps[sIndex]
		if s.Attempt < 0 || s.Attempt > r.ActiveAttempt() {
			return fmt.Errorf("%w: invalid step attempt", ErrCorrupt)
		}
	}
	if attemptClosed(r) && r.State != ReconciliationRequired && !terminal(r.State) {
		return fmt.Errorf("%w: closed attempt cannot mutate or return CID", ErrCorrupt)
	}
	return nil
}
func retryEligible(r Record) error {
	if r.CID != "" || r.State == ReadyToReturn || r.State == Adopted || generationClosed(r.State) {
		return fmt.Errorf("%w: allocation lifecycle forbids retry", ErrConflict)
	}
	if !validFingerprint(r.Intent.FrozenInputsFingerprint) {
		return fmt.Errorf("%w: original frozen membership missing", ErrConflict)
	}
	return nil
}
func attemptsOrInitial(r Record) []Attempt {
	if len(r.Attempts) == 0 {
		return []Attempt{{Number: 0, Plan: initialPlan(r.Intent)}}
	}
	return cloneAttempts(r.Attempts)
}

// CompleteAttempt persists a verified cleanup checkpoint. It preserves the
// allocation identity/index, all plans, and acquired resource history. A crash
// after this write still requires a fresh external audit before BeginAttempt.
func (h *Handle) CompleteAttempt(v AttemptVerification) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := cloneRecord(h.record)
	if err := retryEligible(r); err != nil {
		return err
	}
	if attemptClosed(r) {
		return fmt.Errorf("%w: attempt already closed", ErrConflict)
	}
	if err := validateAttemptProof(r, r.ActiveAttempt(), v); err != nil {
		return err
	}
	for _, old := range r.Verifications {
		if old.EvidenceID == v.EvidenceID {
			return fmt.Errorf("%w: a fresh audit evidence ID is required", ErrReconciliationRequired)
		}
	}
	r.Attempts = attemptsOrInitial(r)
	r.Attempts[len(r.Attempts)-1].Completion = &v
	r.Verifications = append(r.Verifications, v.Verification)
	r.State = ReconciliationRequired
	r.Reason = "attempt verified absent; a new audited plan is required"
	return h.saveLocked(r, true)
}

// BeginAttempt atomically freezes a new plan after fresh complete external
// verification. It may also close an open failed attempt in the same durable
// write. The caller must perform the audit while holding this Handle; do not
// reuse a cached proof from before a process restart. It never changes policy,
// resolved membership, UUID, disk token, or VM generation index.
func (h *Handle) BeginAttempt(p AttemptPlan, v AttemptVerification) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := cloneRecord(h.record)
	if err := retryEligible(r); err != nil {
		return err
	}
	if err := validateAttemptProof(r, r.ActiveAttempt(), v); err != nil {
		return err
	}
	for _, old := range r.Verifications {
		if old.EvidenceID == v.EvidenceID {
			return fmt.Errorf("%w: a fresh audit evidence ID is required", ErrReconciliationRequired)
		}
	}
	r.Attempts = attemptsOrInitial(r)
	if !attemptClosed(r) {
		r.Attempts[len(r.Attempts)-1].Completion = &v
	}
	r.Attempts = append(r.Attempts, Attempt{Number: len(r.Attempts), Plan: p, Admission: &v})
	r.Verifications = append(r.Verifications, v.Verification)
	r.State = Planned
	r.Reason = ""
	return h.saveLocked(r, true)
}

// Only the explicit transition methods may invoke this validator; Save retains
// the complete attempt slice as immutable. The validator keeps the exceptional
// record-state transition from weakening ordinary resource evidence rules.
func validateAttemptTransition(old, next Record) error {
	if err := retryEligible(old); err != nil {
		return err
	}
	if !reflect.DeepEqual(old.Steps, next.Steps) || len(next.Verifications) != len(old.Verifications)+1 {
		return ErrConflict
	}
	before := attemptsOrInitial(old)
	if len(next.Attempts) != len(before) && len(next.Attempts) != len(before)+1 {
		return ErrConflict
	}
	for i, a := range before {
		n := next.Attempts[i]
		if i == len(before)-1 && a.Completion == nil {
			a.Completion = n.Completion
			if a.Completion == nil {
				return ErrConflict
			}
		}
		if !reflect.DeepEqual(a, n) {
			return ErrConflict
		}
	}
	if len(next.Attempts) == len(before) {
		if next.State != ReconciliationRequired || attemptClosed(old) {
			return ErrConflict
		}
	} else if next.State != Planned || next.Attempts[len(next.Attempts)-1].Completion != nil {
		return ErrConflict
	}
	return validateAttempts(next)
}
