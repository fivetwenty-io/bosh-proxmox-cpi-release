package pve

import (
	"strings"
	"testing"
)

func TestStorageAllocationMarkerRoundTripAndAmbiguity(t *testing.T) {
	t.Parallel()
	m := StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "director", AllocationID: "12345678-1234-4234-8234-123456789abc", AgentSHA256: strings.Repeat("a", 64)}
	encoded, err := FormatStorageAllocationMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := ParseStorageAllocationMarker("operator notes" + encoded + "more notes")
	if err != nil || !found || got != m {
		t.Fatal("marker did not round trip")
	}
	if _, found, err := ParseStorageAllocationMarker("operator notes"); err != nil || found {
		t.Fatal("unmarked legacy description rejected")
	}
	for _, bad := range []string{encoded + encoded, strings.Replace(encoded, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(encoded, `"version":1`, `"version":2`, 1), strings.Replace(encoded, `"kind":"vm"`, `"kind":"vm","secret":"response-secret"`, 1), storageAllocationMarkerEnd + storageAllocationMarkerStart, strings.Replace(encoded, m.AllocationID, "invalid", 1)} {
		if _, _, err := ParseStorageAllocationMarker(bad); err == nil {
			t.Fatal("unsafe provenance accepted")
		} else if strings.Contains(err.Error(), "response-secret") {
			t.Fatal("raw provenance leaked")
		}
	}
}
