package handlers

import "errors"

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

// StorageAllocationDecisionFailure returns a bounded stage identifier. When an
// audit gate refused, it appends the gate's summary, whose findings are built
// only from identifiers and classified error descriptions. Backend response
// text, credentials, and resource payloads are never included.
func StorageAllocationDecisionFailure(err error) string {
	class := "identity_or_audit_evidence"
	var stage *storageCleanupStageError
	if errors.As(err, &stage) {
		class = "cleanup_" + stage.stage
	}
	var gate *storageAuditGateFailure
	if errors.As(err, &gate) {
		return class + ": " + gate.summary
	}
	return class
}
