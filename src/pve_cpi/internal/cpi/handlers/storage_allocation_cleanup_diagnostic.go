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

// StorageAllocationDecisionFailure returns a bounded stage identifier. Backend
// response text, credentials, and resource payloads are never included.
func StorageAllocationDecisionFailure(err error) string {
	var stage *storageCleanupStageError
	if errors.As(err, &stage) {
		return "cleanup_" + stage.stage
	}
	return "identity_or_audit_evidence"
}
