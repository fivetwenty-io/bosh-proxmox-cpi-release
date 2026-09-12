package allocationjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// RetainAudit stores bounded canonical nonsecret evidence in the private journal
// base directory before authority enrollment or a recovery transition. Its SHA256
// becomes Enrollment.AuditID; atomic replacement uses the journal's fsync rules.
func RetainAudit(directory string, report any) (id string, retErr error) {
	id, payload, err := VerificationEvidence(report)
	if err != nil {
		return "", err
	}
	root, err := openPrivateRoot(directory)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	name := "audit-" + id + ".json"
	var existing json.RawMessage
	if err = readJSON(root, name, &existing); err == nil {
		if string(existing) != payload {
			return "", fmt.Errorf("%w: retained audit identity disagrees with payload", ErrCorrupt)
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := atomicJSON(root, name, json.RawMessage(payload), defaultFileOps()); err != nil {
		return "", err
	}
	return id, nil
}
