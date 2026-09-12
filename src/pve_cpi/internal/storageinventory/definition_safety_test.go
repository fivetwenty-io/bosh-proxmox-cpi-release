package storageinventory

import (
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"testing"
)

func TestSameDefinitionSafetyMembershipAndShared(t *testing.T) {
	original := pve.StorageInfo{Name: "local", Type: "dir", Path: "/srv/images", Nodes: []string{"n1", "n2"}, Content: "images,iso"}
	for _, tc := range []struct {
		name   string
		nodes  []string
		shared bool
		want   bool
	}{
		{"reordered nodes", []string{"n2", "n1"}, false, true},
		{"replaced node", []string{"n1", "n3"}, false, false},
		{"shared changed", []string{"n1", "n2"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := original
			current.Nodes = tc.nodes
			current.Shared = tc.shared
			firstNode := current.Nodes[0]
			if got := SameDefinitionSafety(original, current); got != tc.want {
				t.Fatalf("equal=%v, want %v", got, tc.want)
			}
			if original.Nodes[0] != "n1" || original.Nodes[1] != "n2" {
				t.Fatal("comparison mutated original nodes")
			}
			if current.Nodes[0] != firstNode {
				t.Fatal("comparison mutated current nodes")
			}
		})
	}
}
