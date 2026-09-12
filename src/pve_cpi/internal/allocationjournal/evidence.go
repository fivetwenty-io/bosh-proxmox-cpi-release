package allocationjournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// MaxEvidenceBytes bounds each canonical durable verification payload.
const MaxEvidenceBytes = 1 << 20

// VerificationEvidence returns canonical, bounded object JSON and its SHA256
// identity. The caller must supply only nonsecret audit evidence and omit
// recursive journal records. Persist both returned strings in Verification.
func VerificationEvidence(report any) (evidenceID, evidenceJSON string, err error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return "", "", err
	}
	canonical, err := canonicalEvidence(raw)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), string(canonical), nil
}
func canonicalEvidence(raw []byte) ([]byte, error) {
	if len(raw) > MaxEvidenceBytes {
		return nil, ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var report map[string]any
	if err := d.Decode(&report); err != nil {
		return nil, err
	}
	if report == nil {
		return nil, ErrCorrupt
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxEvidenceBytes {
		return nil, ErrCorrupt
	}
	return encoded, nil
}
func validateEvidencePayload(v Verification) bool {
	if v.EvidenceJSON == "" {
		return true
	}
	canonical, err := canonicalEvidence([]byte(v.EvidenceJSON))
	if err != nil || string(canonical) != v.EvidenceJSON {
		return false
	}
	sum := sha256.Sum256(canonical)
	return v.EvidenceID == hex.EncodeToString(sum[:])
}
