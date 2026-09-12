package allocationjournal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestVerificationEvidenceCanonicalBoundedAndImmutable(t *testing.T) {
	id, payload, err := VerificationEvidence(map[string]any{"issues": []string{}, "complete": true, "vm_scan_complete": true, "vmid": json.Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	v := Verification{EvidenceID: id, EvidenceJSON: payload, Complete: true, OwnershipVerified: true}
	if !validateVerification(v) || !strings.Contains(payload, "9007199254740993") {
		t.Fatal("valid exact evidence rejected")
	}
	j, _ := fixture(t)
	h := acquireVM(t, j)
	defer closeHandle(t, h)
	r := h.Record()
	r.Verifications = append(r.Verifications, v)
	save(t, h, r)
	r = h.Record()
	r.Verifications[0].EvidenceJSON = `{"complete":false}`
	if err := h.Save(r); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical evidence changed: %v", err)
	}
	cases := []string{`[]`, `null`, `{}`, payload + " ", `{"x":1,"x":2}`, strings.Repeat("x", MaxEvidenceBytes+1)}
	for _, bad := range cases {
		altered := v
		altered.EvidenceJSON = bad
		if validateVerification(altered) {
			t.Fatalf("bad evidence accepted: %.80s", bad)
		}
	}
	altered := v
	altered.EvidenceID = strings.Repeat("0", 64)
	if validateVerification(altered) {
		t.Fatal("fingerprint tampering accepted")
	}
	if _, _, err := VerificationEvidence(map[string]any{"large": strings.Repeat("x", MaxEvidenceBytes)}); err == nil {
		t.Fatal("oversized report accepted")
	}
	if _, _, err := VerificationEvidence([]string{"not-object"}); err == nil {
		t.Fatal("non-object report accepted")
	}
	if !validateVerification(Verification{EvidenceID: "legacy-audit", Complete: true}) {
		t.Fatal("legacy evidence rejected")
	}
}
