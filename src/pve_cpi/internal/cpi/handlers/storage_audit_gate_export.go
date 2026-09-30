package handlers

import (
	"context"
	"errors"
)

// StorageAuditFindingSummary returns the finding summary that an audit gate
// requiring everything would refuse report with: the counts, up to three
// findings, and how many more there are. It returns "" when report passes
// that gate. The storage-journal CLI prints it under a refusal, so an operator
// sees the same summary a refused CPI call carries, without the rerun
// command that the operator has just run.
func StorageAuditFindingSummary(report StorageAllocationAudit) string {
	err := storageAuditGateError(context.Background(), Deps{}, "storage-journal", report, storageAuditGateAll)
	var gate *storageAuditGateFailure
	if errors.As(err, &gate) {
		return gate.summary
	}
	return ""
}
