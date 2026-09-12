package storageinventory

import (
	"fmt"
	"strings"
	"time"
)

// RestoreLedger reconstructs a journal's immutable charges without admitting
// already-consumed bytes as new allocations. Every acquired record needs a new
// sealed task/ownership proof; a later fresh snapshot must include that proof's
// completion barrier before its outstanding debit can retire. Callers obtain
// proofs with MarkCompletion, then Refresh the same collector before restoring.
// Submitted but unacquired records always remain outstanding.
func RestoreLedger(snapshot *Snapshot, records []ChargeRecord, proofs map[string]CompletionEvidence, now time.Time) (Ledger, error) {
	if snapshot == nil || !snapshot.Fresh(now) {
		return Ledger{}, fmt.Errorf("ledger recovery requires a fresh inventory")
	}
	ledger := NewLedger()
	usedProofs := map[string]bool{}
	for rangeIndex21 := range records {
		charge := records[rangeIndex21].Charge
		if strings.TrimSpace(charge.ID) == "" || strings.TrimSpace(charge.Role) == "" || charge.Bytes == 0 || records[rangeIndex21].CapacityKey == "" {
			return Ledger{}, fmt.Errorf("invalid recovered charge identity")
		}
		if _, exists := ledger.entries[charge.ID]; exists {
			return Ledger{}, fmt.Errorf("duplicate recovered charge %q", charge.ID)
		}
		if _, err := CombineLimits(charge.Limits); err != nil {
			return Ledger{}, err
		}
		if err := sameChargeTarget(snapshot, records[rangeIndex21]); err != nil {
			return Ledger{}, err
		}
		entry := ledgerEntry{record: records[rangeIndex21]}
		entry.record.Reflected = false
		if records[rangeIndex21].Acquired {
			proof, ok := proofs[charge.ID]
			if !ok || !records[rangeIndex21].Submitted || records[rangeIndex21].VolumeID == "" {
				return Ledger{}, fmt.Errorf("acquired recovery requires fresh completion proof")
			}
			expected := records[rangeIndex21]
			expected.Acquired = false
			expected.Reflected = false
			expected.Retained = false
			if proof.stamp.Generation == 0 || proof.record != expected {
				return Ledger{}, fmt.Errorf("recovered completion does not match immutable charge %q", charge.ID)
			}
			entry.completion = proof
			usedProofs[charge.ID] = true
		} else if records[rangeIndex21].VolumeID != "" || records[rangeIndex21].Reflected || records[rangeIndex21].Retained {
			return Ledger{}, fmt.Errorf("unacquired recovery carries invalid ownership flags")
		}
		ledger.entries[charge.ID] = entry
	}
	if len(usedProofs) != len(proofs) {
		return Ledger{}, fmt.Errorf("recovery includes unrelated completion proof")
	}
	var err error
	ledger, err = ledger.Refresh(snapshot, now)
	if err != nil {
		return Ledger{}, err
	}
	// All outstanding portions are present before admission, so shared domains
	// account for every member once and acquired reflected bytes are not doubled.
	for recordIndex := range ledger.Records() {
		record := ledger.Records()[recordIndex]
		if _, err = ledger.Candidate(snapshot, record.Charge.Node, record.Charge.StorageID, 0, record.Charge.Limits); err != nil {
			return Ledger{}, err
		}
	}
	return ledger, nil
}
