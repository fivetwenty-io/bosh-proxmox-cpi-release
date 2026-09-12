package pve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const storageAllocationMarkerStart = "[bosh_storage_allocation]"
const storageAllocationMarkerEnd = "[/bosh_storage_allocation]"

var storageAllocationUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var storageAllocationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// StorageAllocationMarker is remote corroboration of a journal allocation.
// A marker is not permission to delete: cluster, target and actual volumes must
// agree with retained journal evidence and a complete ownership audit.
type StorageAllocationMarker struct {
	Version      int    `json:"version"`
	Namespace    string `json:"namespace"`
	AllocationID string `json:"allocation_id"`
	AgentSHA256  string `json:"agent_sha256"`
	Kind         string `json:"kind"`
}

func validateStorageAllocationMarker(m StorageAllocationMarker) error {
	if m.Version != 1 || m.Kind != "vm" || !storageAllocationUUID.MatchString(m.AllocationID) || !storageAllocationDigest.MatchString(m.AgentSHA256) || strings.TrimSpace(m.Namespace) != m.Namespace || m.Namespace == "" || len(m.Namespace) > 1024 || strings.ContainsAny(m.Namespace, "/\\\x00\r\n") || m.Namespace == "." || m.Namespace == ".." {
		return fmt.Errorf("invalid storage allocation provenance")
	}
	return nil
}

// FormatStorageAllocationMarker encodes validated allocation ownership facts.
func FormatStorageAllocationMarker(m StorageAllocationMarker) (string, error) {
	if err := validateStorageAllocationMarker(m); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("cannot encode storage allocation provenance")
	}
	return "\n" + storageAllocationMarkerStart + "\n" + string(encoded) + "\n" + storageAllocationMarkerEnd + "\n", nil
}

// ParseStorageAllocationMarker reads one strict allocation marker and rejects ambiguity.
func ParseStorageAllocationMarker(description string) (StorageAllocationMarker, bool, error) {
	var marker StorageAllocationMarker
	starts, ends := strings.Count(description, storageAllocationMarkerStart), strings.Count(description, storageAllocationMarkerEnd)
	if starts == 0 && ends == 0 {
		return marker, false, nil
	}
	if starts != 1 || ends != 1 {
		return marker, false, fmt.Errorf("ambiguous storage allocation provenance")
	}
	start := strings.Index(description, storageAllocationMarkerStart) + len(storageAllocationMarkerStart)
	end := strings.Index(description, storageAllocationMarkerEnd)
	if end < start {
		return marker, false, fmt.Errorf("malformed storage allocation provenance")
	}
	payload := description[start:end]
	// Reject repeated fields rather than trusting the last value of ambiguous JSON.
	d := json.NewDecoder(strings.NewReader(payload))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return marker, false, fmt.Errorf("malformed storage allocation provenance")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return marker, false, fmt.Errorf("malformed storage allocation provenance")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return marker, false, fmt.Errorf("ambiguous storage allocation provenance")
		}
		seen[key] = true
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return marker, false, fmt.Errorf("malformed storage allocation provenance")
		}
	}
	strict := json.NewDecoder(bytes.NewBufferString(payload))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&marker); err != nil {
		return marker, false, fmt.Errorf("malformed storage allocation provenance")
	}
	if err := strict.Decode(new(any)); err != io.EOF {
		return marker, false, fmt.Errorf("malformed storage allocation provenance")
	}
	if err := validateStorageAllocationMarker(marker); err != nil {
		return marker, false, err
	}
	return marker, true, nil
}
