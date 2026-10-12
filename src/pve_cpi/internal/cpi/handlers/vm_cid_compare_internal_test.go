package handlers

import (
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// TestVMCIDComparisonsByVMID checks that the journal comparisons that used to
// compare a stored CID with strconv.Itoa(vmid) compare by VMID, so a stamped
// CID in a record still matches its VM and a different VM still does not.
func TestVMCIDComparisonsByVMID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		cid  string
		vmid int
		want bool
	}{
		{"101", 101, true},
		{"101-0a1b2c3d-4e5f6a7b", 101, true},
		{"102-0a1b2c3d-4e5f6a7b", 101, false},
		{"102", 101, false},
		{"junk", 101, false},
	} {
		record := aj.Record{State: aj.ReadyToReturn, CID: tc.cid}
		if got := managedVMRouteOwnerMatches(record, tc.vmid); got != tc.want {
			t.Errorf("managedVMRouteOwnerMatches(CID %q, vmid %d) = %v, want %v", tc.cid, tc.vmid, got, tc.want)
		}
	}
}
