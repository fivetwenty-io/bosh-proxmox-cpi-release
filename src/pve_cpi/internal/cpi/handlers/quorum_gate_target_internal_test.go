package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestTargetNodeRefusal_GivesTheDefaultReasonWhenHealthGivesNone pins the
// reason the refusal of a pinned node names when the health entry carries
// none, for a node in maintenance and for one that only cannot take work, and
// that a placeable node is not refused.
func TestTargetNodeRefusal_GivesTheDefaultReasonWhenHealthGivesNone(t *testing.T) {
	t.Parallel()
	const def = "the node health check does not call it placeable"
	cases := []struct {
		name  string
		entry pve.NodeHealthEntry
		want  []string
	}{
		{"unplaceable", pve.NodeHealthEntry{Name: "pve2"}, []string{"cannot take a new VM (" + def + ")"}},
		{"maintenance", pve.NodeHealthEntry{Name: "pve2", Maintenance: true},
			[]string{"is in maintenance (" + def + ")"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := targetNodeRefusal("pve2", tc.entry)
			if err == nil {
				t.Fatal("an unplaceable target_node was not refused")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not contain %q", err, w)
				}
			}
		})
	}
	if err := targetNodeRefusal("pve2", pve.NodeHealthEntry{Name: "pve2", Placeable: true}); err != nil {
		t.Fatalf("a placeable target_node was refused: %v", err)
	}
}
