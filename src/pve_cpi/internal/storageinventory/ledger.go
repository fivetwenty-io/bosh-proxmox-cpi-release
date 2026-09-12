package storageinventory

import (
	"fmt"
	"math/bits"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

// Limits is the effective reserve and ceiling from a selected role, its global
// boundary, and existing hard gates. Zero ceiling means no additional limit.
type Limits struct {
	ReserveBytes      uint64
	MaxUtilizationPct int
}

// CombineLimits retains the maximum reserve and minimum nonzero ceiling.
func CombineLimits(limits ...Limits) (Limits, error) {
	var result Limits
	for _, limit := range limits {
		if limit.MaxUtilizationPct < 0 || limit.MaxUtilizationPct > 100 {
			return Limits{}, fmt.Errorf("invalid utilization ceiling %d", limit.MaxUtilizationPct)
		}
		result.ReserveBytes = max(result.ReserveBytes, limit.ReserveBytes)
		if limit.MaxUtilizationPct != 0 && (result.MaxUtilizationPct == 0 || limit.MaxUtilizationPct < result.MaxUtilizationPct) {
			result.MaxUtilizationPct = limit.MaxUtilizationPct
		}
	}
	return result, nil
}

func add(a, b uint64) (uint64, error) {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return 0, fmt.Errorf("byte sum overflow")
	}
	return sum, nil
}
func multiply(a, b uint64) (uint64, error) {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return 0, fmt.Errorf("byte multiplication overflow")
	}
	return lo, nil
}

// RoundBytes rounds upward without overflowing, using the actual backend unit.
func RoundBytes(value, alignment uint64) (uint64, error) {
	if alignment == 0 {
		return 0, fmt.Errorf("alignment must be positive")
	}
	if remainder := value % alignment; remainder != 0 {
		return add(value, alignment-remainder)
	}
	return value, nil
}

// Charge identifies one new role/auxiliary allocation. Existing physical usage
// already present in observations must never be introduced as a new charge.
type Charge struct {
	ID, Role, Node, StorageID string
	Bytes                     uint64
	Limits                    Limits
}

// ChargeRecord retains actual capacity ownership throughout submission,
// acquisition, reflection in live statistics, and retention after failure.
type ChargeRecord struct {
	Charge                                   Charge
	CapacityKey, DomainKey, VolumeID         string
	Submitted, Acquired, Reflected, Retained bool
}

// ChargeTotals distinguishes plans, all acquired bytes, and acquired/submitted
// bytes not yet reflected in capacity observations. Planned bytes are separate
// from OutstandingBytes; both are debited during admission.
type ChargeTotals struct{ PlannedBytes, AcquiredBytes, OutstandingBytes uint64 }

// CompletionEvidence is sealed by Collector.MarkCompletion after actual task
// completion and owned-volume readback. Arbitrary external generation numbers
// cannot be substituted into this proof.
type CompletionEvidence struct {
	record ChargeRecord
	stamp  ObservationStamp
}
type ledgerEntry struct {
	record     ChargeRecord
	completion CompletionEvidence
}

// Ledger is immutable and request-local. Copying it is cheap; state-changing
// methods clone its private entries before modification.
type Ledger struct{ entries map[string]ledgerEntry }

// NewLedger creates an empty immutable capacity ledger.
func NewLedger() Ledger { return Ledger{entries: map[string]ledgerEntry{}} }
func (l Ledger) clone() Ledger {
	next := NewLedger()
	for id := range l.entries {
		next.entries[id] = l.entries[id]
	}
	return next
}

// Records returns deterministic value copies suitable for plan diagnostics.
func (l Ledger) Records() []ChargeRecord {
	result := make([]ChargeRecord, 0, len(l.entries))
	for rangeIndex111 := range l.entries {
		result = append(result, l.entries[rangeIndex111].record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Charge.ID < result[j].Charge.ID })
	return result
}

// HasRetained reports whether unresolved allocations still consume capacity.
func (l Ledger) HasRetained() bool {
	for rangeIndex119 := range l.entries {
		if l.entries[rangeIndex119].record.Retained {
			return true
		}
	}
	return false
}

// WithPlanned first admits the full combined ledger, then returns a new ledger.
// It does not allocate a VMID, submit a PVE task, or reserve global NAS space.
func (l Ledger) WithPlanned(s *Snapshot, charge Charge) (Ledger, error) {
	if strings.TrimSpace(charge.ID) == "" || strings.TrimSpace(charge.Role) == "" || charge.Bytes == 0 {
		return Ledger{}, fmt.Errorf("charge requires identity, role, and positive bytes")
	}
	if _, exists := l.entries[charge.ID]; exists {
		return Ledger{}, fmt.Errorf("duplicate charge %q", charge.ID)
	}
	if l.HasRetained() {
		return Ledger{}, fmt.Errorf("retained allocation blocks replacement planning")
	}
	candidate, err := l.Candidate(s, charge.Node, charge.StorageID, charge.Bytes, charge.Limits)
	if err != nil {
		return Ledger{}, err
	}
	next := l.clone()
	next.entries[charge.ID] = ledgerEntry{record: ChargeRecord{Charge: charge, CapacityKey: candidate.BackingKey, DomainKey: candidate.DomainKey}}
	return next, nil
}

// Candidate constructs a pure strategy value with all existing request-local
// charges included. Nonshared storage uses the node-specific CapacityKey.
func (l Ledger) Candidate(s *Snapshot, node, id string, allocationBytes uint64, limit Limits) (storageplacement.EligibleCandidate, error) {
	if s == nil {
		return storageplacement.EligibleCandidate{}, fmt.Errorf("candidate requires inventory")
	}
	if l.HasRetained() {
		return storageplacement.EligibleCandidate{}, fmt.Errorf("retained allocation blocks replacement planning")
	}
	pair, exists := s.Pair(node, id)
	if !exists || pair.Reason != "" {
		return storageplacement.EligibleCandidate{}, fmt.Errorf("storage %q on node %q is unavailable: %s", id, node, pair.Reason)
	}
	b, exists := s.Backing(pair.CapacityKey)
	if !exists {
		return storageplacement.EligibleCandidate{}, fmt.Errorf("backing %q lacks usable facts", pair.CapacityKey)
	}
	memberLimits, err := CombineLimits(limit)
	if err != nil {
		return storageplacement.EligibleCandidate{}, err
	}
	domainLimits := memberLimits
	var memberOutstanding, domainOutstanding uint64
	domainKey := s.DomainForStorage(id)
	for rangeIndex172 := range l.entries {
		r := l.entries[rangeIndex172].record
		if err := sameChargeTarget(s, r); err != nil {
			return storageplacement.EligibleCandidate{}, err
		}
		if r.Reflected && !reflectedIn(s, l.entries[rangeIndex172], s.observedAt) {
			return storageplacement.EligibleCandidate{}, fmt.Errorf("snapshot predates reflected charge %q", r.Charge.ID)
		}
		// Reflected acquired resources still contribute policy restrictions, but
		// their physical bytes are already present in the new observation.
		if r.CapacityKey == pair.CapacityKey {
			memberLimits, memberOutstanding, err = includeChargeBudget(memberLimits, memberOutstanding, r)
			if err != nil {
				return storageplacement.EligibleCandidate{}, err
			}
		}
		if domainKey != "" && r.DomainKey == domainKey {
			domainLimits, domainOutstanding, err = includeChargeBudget(domainLimits, domainOutstanding, r)
			if err != nil {
				return storageplacement.EligibleCandidate{}, err
			}
		}
	}
	candidate := storageplacement.EligibleCandidate{StorageID: id, BackingKey: pair.CapacityKey, DomainKey: domainKey, Member: storageplacement.CapacityBudget{TotalBytes: b.TotalBytes, AvailableBytes: b.AvailableBytes, ReserveBytes: memberLimits.ReserveBytes, MaxUtilizationPct: memberLimits.MaxUtilizationPct, OutstandingBytes: memberOutstanding, AllocationBytes: allocationBytes}}
	if _, err := storageplacement.ProjectBudget(candidate.Member); err != nil {
		return storageplacement.EligibleCandidate{}, fmt.Errorf("member %s admission: %w", id, err)
	}
	if domainKey != "" {
		d, ok := s.Domain(domainKey)
		if !ok || d.Reason != "" {
			return storageplacement.EligibleCandidate{}, fmt.Errorf("capacity domain %q is unavailable: %s", domainKey, d.Reason)
		}
		candidate.Domain = storageplacement.CapacityBudget{TotalBytes: d.TotalBytes, AvailableBytes: d.AvailableBytes, ReserveBytes: domainLimits.ReserveBytes, MaxUtilizationPct: domainLimits.MaxUtilizationPct, OutstandingBytes: domainOutstanding, AllocationBytes: allocationBytes}
		if _, err := storageplacement.ProjectBudget(candidate.Domain); err != nil {
			return storageplacement.EligibleCandidate{}, fmt.Errorf("domain %s admission: %w", domainKey, err)
		}
	}
	return candidate, nil
}

// Submit makes an imminent mutation's charge nonremovable until its outcome has
// been reconciled. Call before issuing the corresponding PVE request.
func (l Ledger) Submit(id string) (Ledger, error) {
	e, exists := l.entries[id]
	if !exists {
		return Ledger{}, fmt.Errorf("unknown charge %q", id)
	}
	if e.record.Submitted || e.record.Acquired {
		return Ledger{}, fmt.Errorf("charge %q is already submitted", id)
	}
	next := l.clone()
	e.record.Submitted = true
	next.entries[id] = e
	return next, nil
}

// MarkCompletion creates a barrier bound to the exact submitted charge. The
// executor must supply truthful completion and owned-volume readback results.
func (c *Collector) MarkCompletion(record ChargeRecord, volumeID string, taskComplete, volumeReadback bool) (CompletionEvidence, error) {
	if !taskComplete || !volumeReadback || strings.TrimSpace(volumeID) == "" {
		return CompletionEvidence{}, fmt.Errorf("completion requires successful task and owned-volume readback")
	}
	if !record.Submitted || record.Acquired || strings.TrimSpace(record.Charge.ID) == "" || record.Charge.Bytes == 0 || record.CapacityKey == "" {
		return CompletionEvidence{}, fmt.Errorf("completion requires a submitted charge")
	}
	stamp := c.begin()
	stamp.CompletedAt = stamp.StartedAt
	if stamp.StartedAt.IsZero() {
		return CompletionEvidence{}, fmt.Errorf("completion clock is invalid")
	}
	record.VolumeID = volumeID
	return CompletionEvidence{record: record, stamp: stamp}, nil
}

// Acquire preserves the charge until a later qualifying observation proves its
// physical usage is included. Completion alone never releases capacity.
func (l Ledger) Acquire(id string, evidence CompletionEvidence) (Ledger, error) {
	e, exists := l.entries[id]
	if !exists || !e.record.Submitted || e.record.Acquired {
		return Ledger{}, fmt.Errorf("charge %q is not awaiting completion", id)
	}
	expected := e.record
	expected.VolumeID = evidence.record.VolumeID
	if evidence.stamp.Generation == 0 || evidence.record.VolumeID == "" || expected != evidence.record {
		return Ledger{}, fmt.Errorf("completion does not match charge %q", id)
	}
	next := l.clone()
	e.record.Acquired = true
	e.record.VolumeID = evidence.record.VolumeID
	e.completion = evidence
	next.entries[id] = e
	return next, nil
}

// Retain records a failed allocation kept by policy and prevents replacement
// planning from treating cleanup as complete.
func (l Ledger) Retain(id string) (Ledger, error) {
	e, exists := l.entries[id]
	if !exists || !e.record.Acquired {
		return Ledger{}, fmt.Errorf("retention requires an acquired charge")
	}
	next := l.clone()
	e.record.Retained = true
	e.record.Reflected = false
	next.entries[id] = e
	return next, nil
}

// RemovePlanned only releases an unsubmitted speculative choice.
func (l Ledger) RemovePlanned(id string) (Ledger, error) {
	e, exists := l.entries[id]
	if !exists {
		return Ledger{}, fmt.Errorf("unknown charge %q", id)
	}
	if e.record.Submitted || e.record.Acquired || e.record.Retained {
		return Ledger{}, fmt.Errorf("charge %q requires reconciliation before removal", id)
	}
	next := l.clone()
	delete(next.entries, id)
	return next, nil
}

// Refresh retires charges only when all contributing member/domain observations
// began after completion and remain fresh. Missing/older observations keep the
// charge conservative. A repointed target is an error, never a new identity.
func (l Ledger) Refresh(s *Snapshot, now time.Time) (Ledger, error) {
	if s == nil {
		return Ledger{}, fmt.Errorf("ledger refresh requires inventory")
	}
	next := l.clone()
	for id := range l.entries {
		e := l.entries[id]
		r := e.record
		if err := sameChargeTarget(s, r); err != nil {
			return Ledger{}, err
		}
		pair, exists := s.Pair(r.Charge.Node, r.Charge.StorageID)
		if exists && pair.CapacityKey != r.CapacityKey {
			return Ledger{}, fmt.Errorf("charge %q backing changed", id)
		}
		if !r.Acquired || r.Retained {
			continue
		}
		e.record.Reflected = exists && pair.Reason == "" && reflectedIn(s, e, now)
		next.entries[id] = e
	}
	return next, nil
}

func sameChargeTarget(s *Snapshot, r ChargeRecord) error {
	pair, exists := s.Pair(r.Charge.Node, r.Charge.StorageID)
	if !exists {
		return fmt.Errorf("charge %q target is absent from frozen inventory", r.Charge.ID)
	}
	if pair.CapacityKey != r.CapacityKey || s.DomainForStorage(r.Charge.StorageID) != r.DomainKey {
		return fmt.Errorf("charge %q backing or capacity domain changed", r.Charge.ID)
	}
	return nil
}

func reflectedIn(s *Snapshot, e ledgerEntry, now time.Time) bool {
	r := e.record
	b, exists := s.Backing(r.CapacityKey)
	if !exists {
		return false
	}
	reports := slices.Clone(b.Reports)
	if r.DomainKey != "" {
		d, exists := s.Domain(r.DomainKey)
		if !exists || d.Reason != "" {
			return false
		}
		reports = append(reports, d.Reports...)
	}
	if len(reports) == 0 {
		return false
	}
	for _, stamp := range reports {
		barrier := e.completion.stamp
		if stamp.Epoch != barrier.Epoch || stamp.Generation <= barrier.Generation || stamp.StartedAt.Before(barrier.CompletedAt) || !stampFresh(stamp, now, s.maxAge) {
			return false
		}
	}
	return true
}

// Totals reports one physical backing's planned/acquired/outstanding bytes.
func (l Ledger) Totals(key string) (ChargeTotals, error) {
	var totals ChargeTotals
	for rangeIndex372 := range l.entries {
		r := l.entries[rangeIndex372].record
		if r.CapacityKey != key {
			continue
		}
		var err error
		if r.Acquired {
			totals.AcquiredBytes, err = add(totals.AcquiredBytes, r.Charge.Bytes)
			if err != nil {
				return ChargeTotals{}, err
			}
		}
		if !r.Submitted {
			totals.PlannedBytes, err = add(totals.PlannedBytes, r.Charge.Bytes)
		} else if !r.Reflected {
			totals.OutstandingBytes, err = add(totals.OutstandingBytes, r.Charge.Bytes)
		}
		if err != nil {
			return ChargeTotals{}, err
		}
	}
	return totals, nil
}

func includeChargeBudget(limits Limits, outstanding uint64, record ChargeRecord) (Limits, uint64, error) {
	combined, err := CombineLimits(limits, record.Charge.Limits)
	if err != nil {
		return Limits{}, 0, err
	}
	if record.Reflected {
		return combined, outstanding, nil
	}
	total, err := add(outstanding, record.Charge.Bytes)
	return combined, total, err
}
