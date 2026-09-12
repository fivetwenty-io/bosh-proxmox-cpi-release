package allocationjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Version identifies the journal record schema.
const Version = 1
const maxRecordBytes = 8 << 20

// Journal errors distinguish invalid evidence, lost authority, and unresolved outcomes.
var (
	ErrNotInitialized         = errors.New("journal authority is not initialized; historical provenance audit required")
	ErrConflict               = errors.New("allocation intent conflicts with active generation")
	ErrReconciliationRequired = errors.New("allocation requires reconciliation")
	ErrAuthority              = errors.New("journal authority mismatch or recovery required")
	ErrCorrupt                = errors.New("invalid journal evidence")
	ErrClosed                 = errors.New("journal allocation handle is closed")
)

// State records allocation progress and resource disposition.
type State string

// Allocation states retain mutation evidence through cleanup.
const (
	Planned                State = "planned"
	Submitted              State = "submitted"
	Observed               State = "observed"
	ReadyToReturn          State = "ready_to_return"
	ReconciliationRequired State = "reconciliation_required"
	Adopted                State = "adopted"
	Deleted                State = "deleted"
	Cleaned                State = "cleaned"
	VMDeletedRetained      State = "vm_deleted_retained"
)

// Intent separates caller identity from mutable global policy. Plan contains
// only versioned, nonsecret planner data. Neither fingerprint changes on resume.
type Intent struct {
	IntentFingerprint       string `json:"intent_fingerprint"`
	PolicyFingerprint       string `json:"policy_fingerprint"`
	FrozenInputsFingerprint string `json:"frozen_inputs_fingerprint,omitempty"`
	PlanVersion             int    `json:"plan_version"`
	// The executor must reject unsupported planner payload versions before resume.
	Plan json.RawMessage `json:"plan"`
}

// Target identifies the physical resource affected by a planned mutation.
type Target struct {
	// External records preservation work for another allocation, never ownership.
	External       bool   `json:"external,omitempty"`
	VirtualBytes   uint64 `json:"virtual_bytes,omitempty"`
	Node           string `json:"node"`
	Storage        string `json:"storage,omitempty"`
	Backing        string `json:"backing,omitempty"`
	VMID           int    `json:"vmid,omitempty"`
	IntendedVolume string `json:"intended_volume,omitempty"`
}

// Charge preserves planned capacity and its acquired/outstanding partition.
type Charge struct {
	Backing          string `json:"backing"`
	Domain           string `json:"domain,omitempty"`
	PlannedBytes     int64  `json:"planned_bytes"`
	AcquiredBytes    int64  `json:"acquired_bytes"`
	OutstandingBytes int64  `json:"outstanding_bytes"`
}

// Step intent must be saved before submitting its API operation. Earlier steps
// remain in the record when a later step starts; UPIDs and volids are retained.
type Step struct {
	// Parameters contains bounded concrete nonsecret infrastructure mutation fields.
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Attempt    int             `json:"attempt,omitempty"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Target     Target          `json:"target"`
	State      State           `json:"state"`
	UPID       string          `json:"upid,omitempty"`
	VolIDs     []string        `json:"volids,omitempty"`
	Charges    []Charge        `json:"charges,omitempty"`
}

// Verification is an attestation supplied by the external PVE reconciliation
// adapter or operator. EvidenceID refers to retained audit evidence, never a
// request payload. Complete requires historical targets, not current membership.
type Verification struct {
	EvidenceID                  string `json:"evidence_id"`
	EvidenceJSON                string `json:"evidence_json,omitempty"`
	Complete                    bool   `json:"complete"`
	OwnershipVerified           bool   `json:"ownership_verified"`
	VMAbsenceVerified           bool   `json:"vm_absence_verified,omitempty"`
	AbsenceVerified             bool   `json:"absence_verified"`
	ArtifactDispositionVerified bool   `json:"artifact_disposition_verified"`
}

// Record retains immutable allocation intent and append-only mutation evidence.
type Record struct {
	Version        int            `json:"version"`
	ID             string         `json:"id"`
	Namespace      string         `json:"namespace"`
	ClusterID      string         `json:"cluster_id"`
	AuthorityEpoch string         `json:"authority_epoch"`
	Kind           string         `json:"kind"`
	AgentID        string         `json:"agent_id,omitempty"`
	DiskToken      string         `json:"disk_token,omitempty"`
	Intent         Intent         `json:"intent"`
	Attempts       []Attempt      `json:"attempts,omitempty"`
	State          State          `json:"state"`
	Steps          []Step         `json:"steps,omitempty"`
	CID            string         `json:"cid,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	Verifications  []Verification `json:"verifications,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// Enrollment is explicit operator evidence, not automatic discovery. A complete
// historical audit must include allocations outside current storage membership.
// PreviousWriterFenced attests external fencing; local locks cannot prove it.
type Enrollment struct {
	ClusterID               string   `json:"cluster_id"`
	AuthorityID             string   `json:"authority_id"`
	AuditID                 string   `json:"audit_id"`
	CompleteHistoricalAudit bool     `json:"complete_historical_audit"`
	PreviousWriterFenced    bool     `json:"previous_writer_fenced"`
	ProvenanceIDs           []string `json:"provenance_ids,omitempty"`
}

type authority struct {
	Version    int        `json:"version"`
	Namespace  string     `json:"namespace"`
	Epoch      string     `json:"epoch"`
	Enrollment Enrollment `json:"enrollment"`
}

type index struct {
	Version   int               `json:"version"`
	ActiveVMs map[string]string `json:"active_vms"`
}

func nonblank(s string) bool        { return strings.TrimSpace(s) != "" }
func terminal(s State) bool         { return s == Deleted || s == Cleaned }
func generationClosed(s State) bool { return terminal(s) || s == VMDeletedRetained }
func validState(s State) bool {
	switch s {
	case Planned, Submitted, Observed, ReadyToReturn, ReconciliationRequired, Adopted, Deleted, Cleaned, VMDeletedRetained:
		return true
	}
	return false
}
func validFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func validateIntent(i Intent) error {
	if i.FrozenInputsFingerprint != "" && !validFingerprint(i.FrozenInputsFingerprint) {
		return fmt.Errorf("%w: invalid frozen input fingerprint", ErrCorrupt)
	}
	if !validFingerprint(i.IntentFingerprint) || !validFingerprint(i.PolicyFingerprint) || i.PlanVersion < 1 || !json.Valid(i.Plan) || string(i.Plan) == "null" {
		return fmt.Errorf("%w: invalid fingerprints or versioned plan", ErrCorrupt)
	}
	return nil
}
func validateVerification(v Verification) bool {
	return nonblank(v.EvidenceID) && v.Complete && validateEvidencePayload(v)
}
func validateRecord(r Record) error {
	if r.Version != Version || !allocationIDPattern.MatchString(r.ID) || !nonblank(r.Namespace) || !nonblank(r.ClusterID) || !allocationIDPattern.MatchString(r.AuthorityEpoch) || !validState(r.State) || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("%w: invalid record identity/version/state", ErrCorrupt)
	}
	if err := validateIntent(r.Intent); err != nil {
		return err
	}
	switch r.Kind {
	case "vm":
		if !nonblank(r.AgentID) || r.DiskToken != "" {
			return fmt.Errorf("%w: VM identity", ErrCorrupt)
		}
	case "disk":
		token, err := DiskCorrelationToken(r.ID)
		if err != nil || token != r.DiskToken || r.AgentID != "" {
			return fmt.Errorf("%w: disk identity", ErrCorrupt)
		}
	default:
		return fmt.Errorf("%w: allocation kind", ErrCorrupt)
	}
	if err := validateAttempts(r); err != nil {
		return err
	}
	if err := validateRecordSteps(r); err != nil {
		return err
	}
	return validateRecordDisposition(r)
}

func validateRecordSteps(r Record) error {
	seen := map[string]bool{}
	for sIndex := range r.Steps {
		s := r.Steps[sIndex]
		if !nonblank(s.ID) || seen[s.ID] || !nonblank(s.Kind) || !nonblank(s.Target.Node) || s.Target.VMID < 0 || (s.State != Planned && s.State != Submitted && s.State != Observed && s.State != ReconciliationRequired) {
			return fmt.Errorf("%w: invalid mutation step", ErrCorrupt)
		}
		seen[s.ID] = true
		if err := validateMutationParameters(s.Parameters); err != nil {
			return err
		}
		if s.State == Submitted && !nonblank(s.UPID) {
			return fmt.Errorf("%w: submitted step lacks UPID", ErrCorrupt)
		}
		for _, c := range s.Charges {
			if !nonblank(c.Backing) || c.PlannedBytes < 0 || c.AcquiredBytes < 0 || c.OutstandingBytes < 0 || c.AcquiredBytes > math.MaxInt64-c.OutstandingBytes || c.AcquiredBytes+c.OutstandingBytes != c.PlannedBytes {
				return fmt.Errorf("%w: invalid charge", ErrCorrupt)
			}
		}
		for _, id := range s.VolIDs {
			if !nonblank(id) {
				return fmt.Errorf("%w: blank volid", ErrCorrupt)
			}
		}
	}
	for _, v := range r.Verifications {
		if !validateVerification(v) {
			return fmt.Errorf("%w: incomplete verification", ErrCorrupt)
		}
	}

	return nil
}

func validateRecordDisposition(r Record) error {
	if r.State == Submitted {
		found := false
		for sIndex := range r.Steps {
			s := r.Steps[sIndex]
			found = found || s.Attempt == r.ActiveAttempt() && s.State == Submitted
		}
		if !found {
			return fmt.Errorf("%w: missing submitted step", ErrCorrupt)
		}
	}
	if r.State == Observed && len(r.Steps) == 0 {
		return fmt.Errorf("%w: no observed resources", ErrCorrupt)
	}
	if r.State == ReadyToReturn || r.State == Adopted {
		if !nonblank(r.CID) || len(r.CID) > 255 || activeStepCount(r) == 0 || attemptClosed(r) {
			return fmt.Errorf("%w: result lacks CID/resources", ErrCorrupt)
		}
		for sIndex := range r.Steps {
			s := r.Steps[sIndex]
			if s.Attempt != r.ActiveAttempt() {
				continue
			}
			if s.State != Observed {
				return fmt.Errorf("%w: incomplete result step", ErrCorrupt)
			}
		}
	}
	if r.State == VMDeletedRetained {
		if err := validateVMRetention(r); err != nil {
			return err
		}
	}
	if r.State == ReconciliationRequired && !nonblank(r.Reason) {
		return fmt.Errorf("%w: reconciliation lacks reason", ErrCorrupt)
	}
	if terminal(r.State) || r.State == Adopted {
		if len(r.Verifications) == 0 {
			return fmt.Errorf("%w: terminal/adoption evidence required", ErrCorrupt)
		}
		v := r.Verifications[len(r.Verifications)-1]
		if terminal(r.State) && (!v.AbsenceVerified || !v.ArtifactDispositionVerified) || r.State == Adopted && !v.OwnershipVerified {
			return fmt.Errorf("%w: insufficient lifecycle evidence", ErrCorrupt)
		}
	}
	return nil
}
func validTransition(from, to State) bool {
	if terminal(from) {
		return from == to
	}
	if from == VMDeletedRetained {
		return to == VMDeletedRetained || to == Deleted || to == Cleaned
	}
	if to == VMDeletedRetained {
		return from == Observed || from == ReadyToReturn || from == Adopted || from == ReconciliationRequired
	}
	if from == to || to == ReconciliationRequired || to == Deleted || to == Cleaned {
		return true
	}
	switch from {
	case Planned:
		return to == Submitted || to == Observed
	case Submitted:
		return to == Observed
	case Observed:
		return to == Planned || to == Submitted || to == ReadyToReturn
	case ReadyToReturn:
		return to == Adopted
	case ReconciliationRequired:
		return to == Planned || to == Submitted || to == Observed || to == ReadyToReturn || to == Adopted
	}
	return false
}
func validateUpdate(old, next Record) error {
	a, b := old, next
	a.State = b.State
	a.Steps = b.Steps
	a.CID = b.CID
	a.Reason = b.Reason
	a.Verifications = b.Verifications
	a.UpdatedAt = b.UpdatedAt
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("%w: immutable allocation identity, intent or plan changed", ErrConflict)
	}
	if !validTransition(old.State, next.State) {
		return fmt.Errorf("%w: invalid transition %s to %s", ErrCorrupt, old.State, next.State)
	}
	if terminal(old.State) && !reflect.DeepEqual(old, next) {
		return fmt.Errorf("%w: terminal evidence is immutable", ErrConflict)
	}
	if old.CID != "" && old.CID != next.CID {
		return fmt.Errorf("%w: CID changed", ErrConflict)
	}
	if len(next.Verifications) < len(old.Verifications) || !slices.Equal(next.Verifications[:len(old.Verifications)], old.Verifications) {
		return fmt.Errorf("%w: verification evidence removed", ErrConflict)
	}
	if old.State == ReconciliationRequired && next.State != ReconciliationRequired && len(next.Verifications) == len(old.Verifications) {
		return ErrReconciliationRequired
	}
	if err := validateRetentionTransition(old, next); err != nil {
		return err
	}
	if err := validateStepHistory(old, next); err != nil {
		return err
	}
	return validateRecord(next)
}

func validateRetentionTransition(old, next Record) error {
	if old.State == VMDeletedRetained && terminal(next.State) {
		if len(next.Verifications) <= len(old.Verifications) {
			return fmt.Errorf("%w: fresh retained artifact absence required", ErrCorrupt)
		}
		v := next.Verifications[len(next.Verifications)-1]
		if v.EvidenceJSON == "" {
			return fmt.Errorf("%w: durable retained cleanup proof required", ErrCorrupt)
		}
		for _, prior := range old.Verifications {
			if prior.EvidenceID == v.EvidenceID {
				return fmt.Errorf("%w: retained cleanup proof reused", ErrCorrupt)
			}
		}
		for stepIndex := range next.Steps {
			step := next.Steps[stepIndex]
			if step.Attempt == next.ActiveAttempt() && step.State != Observed {
				return fmt.Errorf("%w: unsettled retained cleanup step", ErrCorrupt)
			}
		}
	}
	if next.State == VMDeletedRetained && old.State != VMDeletedRetained {
		if len(next.Verifications) <= len(old.Verifications) {
			return fmt.Errorf("%w: fresh VM retention proof required", ErrCorrupt)
		}
		v := next.Verifications[len(next.Verifications)-1]
		if !v.VMAbsenceVerified || !v.ArtifactDispositionVerified || v.AbsenceVerified || v.EvidenceJSON == "" {
			return fmt.Errorf("%w: fresh retention proof lacks VM disposition", ErrCorrupt)
		}
		for _, prior := range old.Verifications {
			if prior.EvidenceID == v.EvidenceID {
				return fmt.Errorf("%w: retention proof reused", ErrCorrupt)
			}
		}
		for stepIndex := range next.Steps {
			step := next.Steps[stepIndex]
			if step.Attempt == next.ActiveAttempt() && step.State != Observed {
				return fmt.Errorf("%w: unsettled VM deletion step", ErrCorrupt)
			}
		}
	}

	return nil
}

func validateStepHistory(old, next Record) error {
	if len(next.Steps) < len(old.Steps) {
		return fmt.Errorf("%w: mutation history removed", ErrConflict)
	}
	for i := range old.Steps {
		s := old.Steps[i]
		n := next.Steps[i]
		if (s.Attempt < old.ActiveAttempt() || attemptClosed(old)) && !reflect.DeepEqual(s, n) {
			return fmt.Errorf("%w: closed attempt evidence changed", ErrConflict)
		}
		if !bytes.Equal(s.Parameters, n.Parameters) || s.Attempt != n.Attempt || s.ID != n.ID || s.Kind != n.Kind || s.Target != n.Target || s.UPID != "" && s.UPID != n.UPID || len(n.VolIDs) < len(s.VolIDs) || !slices.Equal(n.VolIDs[:len(s.VolIDs)], s.VolIDs) {
			return fmt.Errorf("%w: mutation evidence changed", ErrConflict)
		}
		if len(s.Charges) != len(n.Charges) {
			return fmt.Errorf("%w: mutation charge history changed", ErrConflict)
		}
		for k, c := range s.Charges {
			nc := n.Charges[k]
			if c.Backing != nc.Backing || c.Domain != nc.Domain || c.PlannedBytes != nc.PlannedBytes || nc.AcquiredBytes < c.AcquiredBytes || nc.AcquiredBytes > c.AcquiredBytes && n.State != Observed {
				return fmt.Errorf("%w: charge identity, acquisition or planned total changed", ErrConflict)
			}
		}
		if !validTransition(s.State, n.State) || s.State == Observed && n.State != Observed {
			return fmt.Errorf("%w: observed step regressed", ErrConflict)
		}
	}
	for sIndex := range next.Steps[len(old.Steps):] {
		s := next.Steps[len(old.Steps):][sIndex]
		if s.Attempt != old.ActiveAttempt() || attemptClosed(old) || s.State != Planned || s.UPID != "" || len(s.VolIDs) != 0 {
			return fmt.Errorf("%w: new mutation step must first persist intent", ErrCorrupt)
		}
	}
	return nil
}

func cloneRecord(r Record) Record {
	r.Intent.Plan = append(json.RawMessage(nil), r.Intent.Plan...)
	r.Attempts = cloneAttempts(r.Attempts)
	r.Steps = append([]Step(nil), r.Steps...)
	for i := range r.Steps {
		r.Steps[i].Parameters = append(json.RawMessage(nil), r.Steps[i].Parameters...)
		r.Steps[i].VolIDs = append([]string(nil), r.Steps[i].VolIDs...)
		r.Steps[i].Charges = append([]Charge(nil), r.Steps[i].Charges...)
	}
	r.Verifications = append([]Verification(nil), r.Verifications...)
	return r
}
