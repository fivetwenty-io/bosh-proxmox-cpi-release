package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestQuorumLossReasonNamesTheEntryNode checks that the reason a quorum gate
// gives names the node that answered, and that a status which marks no node
// local falls back to the generic reason.
func TestQuorumLossReasonNamesTheEntryNode(t *testing.T) {
	t.Parallel()
	named := quorumLossReason(pve.ClusterQuorum{Reported: true, EntryNode: "pve3"})
	if want := "the entry node pve3 does not report the cluster quorate"; named != want {
		t.Errorf("reason = %q, want %q", named, want)
	}
	if got := quorumLossReason(pve.ClusterQuorum{Reported: true}); got != pve.ReasonNotQuorate {
		t.Errorf("reason without an entry node = %q, want %q", got, pve.ReasonNotQuorate)
	}
}
