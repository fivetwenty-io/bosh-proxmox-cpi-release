package storageinventory

import "fmt"

// PolicyError identifies a deterministic inventory policy rejection without
// requiring callers to inspect untrusted API error text.
type PolicyError struct {
	Code   string
	detail string
}

const (
	// PolicyMissingStorage identifies explicitly referenced, absent storage IDs.
	PolicyMissingStorage = "missing_storage_ids"
	// PolicyBackingAliases identifies duplicate physical backings in one set.
	PolicyBackingAliases = "backing_aliases"
	// PolicyOverlappingSets identifies overlapping persistent and VM backings.
	PolicyOverlappingSets = "overlapping_sets"
)

func (e *PolicyError) Error() string { return e.detail }

func policyError(code, format string, args ...any) error {
	return &PolicyError{Code: code, detail: fmt.Sprintf(format, args...)}
}
