package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestManagedMarkerDescriptionPVEReadback(t *testing.T) {
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{
		Version: 1, Namespace: "certification", Kind: "vm",
		AllocationID: "50f14b51-3c47-4def-a9b2-a9e1bfa60d79", AgentSHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	params := &sdknodes.UpdateQemuConfigParams{Description: &marker}
	for _, tc := range []struct {
		name     string
		readback string
		reject   bool
	}{
		{name: "PVE omits final newline", readback: strings.TrimSuffix(marker, "\n")},
		{name: "exact marker", readback: marker},
		{name: "different allocation", readback: strings.ReplaceAll(marker, "50f14b51", "60f14b51"), reject: true},
		{name: "different namespace", readback: strings.ReplaceAll(marker, "certification", "foreign"), reject: true},
		{name: "different surrounding text", readback: "foreign notes" + marker, reject: true},
		{name: "interior line removed", readback: strings.Replace(marker, "]\n{", "]{", 1), reject: true},
		{name: "leading newline removed", readback: strings.TrimPrefix(marker, "\n"), reject: true},
		{name: "trailing space", readback: strings.TrimSuffix(marker, "\n") + " ", reject: true},
		{name: "marker removed", readback: "", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := managedConfigFieldsMatch(map[string]any{"description": tc.readback}, params); (err != nil) != tc.reject {
				t.Fatalf("unexpected marker readback verification: %v", err)
			}
		})
	}
	if err := managedConfigFieldsMatch(map[string]any{}, params); err == nil {
		t.Fatal("missing nonempty marker accepted")
	}
	if err := managedConfigFieldsMatch(map[string]any{"name": "guest"}, map[string]any{"name": "guest\n"}); err == nil {
		t.Fatal("description normalization weakened another field")
	}
	for _, tc := range []struct{ requested, actual string }{
		{"operator notes\n", "operator notes"},
		{marker + "\n", marker},
		{strings.TrimSuffix(marker, "\n") + "\r\n", strings.TrimSuffix(marker, "\n")},
		{strings.ReplaceAll(marker, "certification", ""), strings.TrimSuffix(strings.ReplaceAll(marker, "certification", ""), "\n")},
	} {
		if err := managedConfigFieldsMatch(map[string]any{"description": tc.actual}, map[string]any{"description": tc.requested}); err == nil {
			t.Fatal("unvalidated or broader whitespace normalization accepted")
		}
	}
}
