package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"strings"
)

// storageLifecycle retains the original allocation while a lifecycle operation
// changes its resources. External adapters own all observation and task waits.
type storageLifecycle struct {
	handle    *aj.Handle
	operation string
	adopted   bool
	closed    bool
}

func beginStorageLifecycle(handle *aj.Handle, operation string, ownership aj.Verification) (*storageLifecycle, error) {
	return beginStorageLifecycleMode(handle, operation, ownership, true)
}

func beginStorageLifecycleMode(handle *aj.Handle, operation string, ownership aj.Verification, requireReturned bool) (*storageLifecycle, error) {
	return beginStorageLifecycleContext(context.Background(), handle, operation, ownership, requireReturned)
}
func beginStorageLifecycleCleanup(ctx context.Context, handle *aj.Handle, operation string, ownership aj.Verification) (*storageLifecycle, error) {
	return beginStorageLifecycleContext(ctx, handle, operation, ownership, false)
}
func beginStorageLifecycleContext(ctx context.Context, handle *aj.Handle, operation string, ownership aj.Verification, requireReturned bool) (*storageLifecycle, error) {
	if handle == nil || strings.TrimSpace(operation) == "" {
		return nil, fmt.Errorf("lifecycle requires allocation ownership and operation")
	}
	record := handle.Record()
	if record.State == aj.Deleted || record.State == aj.Cleaned || requireReturned && record.CID == "" {
		return nil, fmt.Errorf("lifecycle requires a live returned allocation")
	}
	if err := storageLifecycleEvidence(record, ownership, false); err != nil {
		return nil, err
	}
	if err := storageCleanupSettled(ctx, record); err != nil {
		return nil, err
	}
	session := &storageLifecycle{handle: handle, operation: operation, adopted: record.State == aj.Adopted}
	record.State = aj.ReconciliationRequired
	record.Reason = "lifecycle " + operation + " admitted; completion pending"
	if err := handle.Save(record); err != nil {
		return nil, err
	}
	record = handle.Record()
	record.State = aj.Observed
	record.Verifications = append(record.Verifications, ownership)
	if err := handle.Save(record); err != nil {
		return nil, err
	}
	return session, nil
}
func storageLifecycleSettled(record aj.Record) error {
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Attempt == record.ActiveAttempt() && step.State != aj.Observed {
			return fmt.Errorf("lifecycle has unresolved mutation evidence; audit required")
		}
	}
	return nil
}
func storageLifecycleEvidence(record aj.Record, v aj.Verification, deleted bool) error {
	if !v.Complete || v.EvidenceID == "" || v.EvidenceJSON == "" {
		return fmt.Errorf("lifecycle requires a complete durable external audit")
	}
	id, payload, err := aj.VerificationEvidence(json.RawMessage(v.EvidenceJSON))
	if err != nil || id != v.EvidenceID || payload != v.EvidenceJSON {
		return fmt.Errorf("lifecycle audit payload does not match its fingerprint")
	}
	if deleted && (!v.AbsenceVerified || !v.ArtifactDispositionVerified) || !deleted && !v.OwnershipVerified {
		return fmt.Errorf("lifecycle audit does not prove required resource disposition")
	}
	for _, prior := range record.Verifications {
		if prior.EvidenceID == v.EvidenceID {
			return fmt.Errorf("lifecycle requires fresh external audit evidence")
		}
	}
	return nil
}
func (s *storageLifecycle) Intent(kind string, target aj.Target) (string, error) {
	if s == nil || s.closed {
		return "", fmt.Errorf("lifecycle session closed")
	}
	return storageMutationIntent(s.handle, "lifecycle_"+s.operation+"_"+kind, target, nil)
}
func (s *storageLifecycle) Submitted(step, upid string) error {
	if s == nil || s.closed {
		return fmt.Errorf("lifecycle session closed")
	}
	return storageMutationSubmitted(s.handle, step, upid)
}
func (s *storageLifecycle) Observed(step string, volumes []string) error {
	if s == nil || s.closed {
		return fmt.Errorf("lifecycle session closed")
	}
	return storageMutationObserved(s.handle, step, volumes, false)
}
func (s *storageLifecycle) Finish(v aj.Verification, deleted bool) error {
	if s == nil || s.closed {
		return fmt.Errorf("lifecycle session closed")
	}
	record := s.handle.Record()
	if err := storageLifecycleEvidence(record, v, deleted); err != nil {
		return err
	}
	if err := storageLifecycleSettled(record); err != nil {
		return err
	}
	record.Verifications = append(record.Verifications, v)
	record.Reason = ""
	if deleted {
		record.State = aj.Deleted
	} else {
		record.State = aj.ReadyToReturn
	}
	if err := s.handle.Save(record); err != nil {
		return err
	}
	if !deleted && s.adopted {
		record = s.handle.Record()
		record.State = aj.Adopted
		if err := s.handle.Save(record); err != nil {
			return err
		}
	}
	s.closed = true
	return nil
}
func (s *storageLifecycle) Uncertain(phase string) error {
	if s == nil || s.closed {
		return fmt.Errorf("lifecycle session closed")
	}
	s.closed = true
	return storageAllocationUncertain(s.handle, "lifecycle "+s.operation+" "+phase)
}
