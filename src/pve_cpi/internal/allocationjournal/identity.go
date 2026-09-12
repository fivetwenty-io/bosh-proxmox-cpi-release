// Package allocationjournal records allocation intent and uncertain outcomes durably.
// It never contacts PVE: callers must verify ownership and historical provenance
// before resuming mutations or returning an existing CID. Local locks coordinate
// one directory only; authority relocation requires external writer fencing.
package allocationjournal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

var allocationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// NewAllocationID returns a cryptographically random version 4 UUID.
func NewAllocationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("journal: generate allocation UUID: %w", err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// DiskCorrelationToken preserves the existing 20-character parked disk ID.
// This shortened locator is not sufficient evidence of allocation ownership.
func DiskCorrelationToken(id string) (string, error) {
	if !allocationIDPattern.MatchString(id) {
		return "", fmt.Errorf("journal: invalid allocation UUID")
	}
	sum := sha256.Sum256([]byte("bosh-proxmox-cpi/disk-correlation/v1\x00" + id))
	return "bpd-" + hex.EncodeToString(sum[:8]), nil
}

// Fingerprint hashes canonical encoding/json output. The caller must supply
// normalized nonsecret fields only; this function does not redact input.
func Fingerprint(nonsecret any) (string, error) {
	data, err := json.Marshal(nonsecret)
	if err != nil {
		return "", fmt.Errorf("journal: fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
