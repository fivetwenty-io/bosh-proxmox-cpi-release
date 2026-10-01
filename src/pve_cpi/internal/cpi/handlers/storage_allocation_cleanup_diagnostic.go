package handlers

import (
	"errors"
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

type storageCleanupStageError struct {
	stage string
	cause error
}

func (e *storageCleanupStageError) Error() string { return "allocation cleanup refused at " + e.stage }
func (e *storageCleanupStageError) Unwrap() error { return e.cause }
func storageCleanupFailure(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	var existing *storageCleanupStageError
	if errors.As(cause, &existing) {
		return cause
	}
	return &storageCleanupStageError{stage: stage, cause: cause}
}

// storageRefusalError is a refusal the CPI wrote itself from fixed text and
// identifiers, so no backend response or credential can reach its reason.
// StorageAllocationDecisionFailure prints the reason for the CLI.
type storageRefusalError struct{ reason string }

func (e *storageRefusalError) Error() string { return e.reason }

// storageRefusal returns a refusal whose reason is fixed text.
func storageRefusal(reason string) error { return &storageRefusalError{reason: reason} }

// storageRefusalf formats a refusal. Its arguments must be identifiers, such
// as allocation IDs, node names, and VMIDs, or a pve.DescribeAuditError
// description, and never an error or raw PVE text.
func storageRefusalf(format string, args ...any) error {
	return &storageRefusalError{reason: fmt.Sprintf(format, args...)}
}

// errRetainedParkerIdentity refuses a retained ephemeral volume whose parker
// no longer records it under the allocation's retention token, on the node
// and volume the record names.
var errRetainedParkerIdentity = errors.New("retained parker identity differs")

// storageFixedRefusals are refusals the CPI wrote as fixed errors rather than
// as storageRefusalError. They keep that plain type, so delete_vm still wraps
// them and shows them to the Director exactly as before, and
// StorageAllocationDecisionFailure prints their fixed text for the CLI.
var storageFixedRefusals = []error{errRetainedParkerIdentity}

// storageLockWaitReturned is what the CLI prints when a disk operation inside
// a decision ran out a parker lock wait before it moved the disk. The record
// stays where it was, so the same decision can run again.
const storageLockWaitReturned = "a parker lock wait ran out before the disk moved, so nothing was destroyed; run cleanup again once the lock is free"

// StorageAllocationDecisionFailure returns a bounded stage identifier and a
// safe description of why the decision was refused. A refused audit gate
// contributes its summary and the runbook pointer, and a CPI-authored refusal
// its reason, both built only from identifiers and classified error
// descriptions. Only an audit refusal points at the audit runbook. A
// settlement write the journal refused names the steps it was settling and
// the journal's class of failure, which can include the journal's own file
// path. That is safe because the storage-journal CLI, which runs on the
// journal's host, is this function's only production caller. A fixed refusal
// in storageFixedRefusals contributes its own text, and so does a parker lock
// wait that ran out before the disk moved. Anything else is described by
// pve.DescribeAuditError, so backend response text, credentials, and resource
// payloads are never included.
func StorageAllocationDecisionFailure(err error) string {
	class := "identity_or_audit_evidence"
	var stage *storageCleanupStageError
	if errors.As(err, &stage) {
		class = "cleanup_" + stage.stage
	}
	var gate *storageAuditGateFailure
	if errors.As(err, &gate) {
		return class + ": " + gate.summary + "; " + StorageAuditRunbook
	}
	var refusal *storageRefusalError
	if errors.As(err, &refusal) {
		return class + ": " + log.ScrubMessage(refusal.reason)
	}
	var settlement *settlementSaveError
	if errors.As(err, &settlement) {
		return class + ": " + log.ScrubMessage(settlement.description())
	}
	for _, fixed := range storageFixedRefusals {
		if errors.Is(err, fixed) {
			return class + ": " + fixed.Error()
		}
	}
	if isDiskReturnedAfterLockTimeout(err) {
		return class + ": " + storageLockWaitReturned
	}
	if err == nil {
		return class
	}
	return class + ": " + pve.DescribeAuditError(err)
}
