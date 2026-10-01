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

// StorageAllocationDecisionFailure returns a bounded stage identifier and a
// safe description of why the decision was refused. A refused audit gate
// contributes its summary and the runbook pointer, and a CPI-authored refusal
// its reason, both built only from identifiers and classified error
// descriptions. Only an audit refusal points at the audit runbook. Anything
// else is described by pve.DescribeAuditError, so backend response text,
// credentials, and resource payloads are never included.
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
	if err == nil {
		return class
	}
	return class + ": " + pve.DescribeAuditError(err)
}
