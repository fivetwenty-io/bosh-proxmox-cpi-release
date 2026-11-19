package ha

import (
	"strings"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// DLBRecordKey opens the token Stage appends to the comment of a VM's HA
// resource to record whether the CPI registered the VM as a DLB VM: 1 for a
// DLB VM and 0 for any other. The CPI reads membership from this record
// rather than from the resource's auto-rebalance flag, because an operator can
// change the flag in PVE's GUI, and a re-registration would then take the
// operator's choice for the VM's placement class (see InDLB).
const DLBRecordKey = "bosh-dlb="

// WithDLBRecord returns comment with the token that records dlb, replacing
// any record it carried. The rest of the comment, which starts with the
// ownership marker, is kept in order.
func WithDLBRecord(comment string, dlb bool) string {
	value := "0"
	if dlb {
		value = "1"
	}
	fields := strings.Fields(comment)
	kept := fields[:0]
	for _, f := range fields {
		if !strings.HasPrefix(f, DLBRecordKey) {
			kept = append(kept, f)
		}
	}
	return strings.Join(append(kept, DLBRecordKey+value), " ")
}

// DLBRecord returns the DLB membership comment records, and false when it
// carries no record or one whose value is neither 1 nor 0.
func DLBRecord(comment *string) (dlb, ok bool) {
	if comment == nil {
		return false, false
	}
	for _, f := range strings.Fields(*comment) {
		if v, found := strings.CutPrefix(f, DLBRecordKey); found {
			switch v {
			case "1":
				return true, true
			case "0":
				return false, true
			}
			return false, false
		}
	}
	return false, false
}

// InDLB reports whether the CPI counts the VM whose HA resource carries
// comment and the auto-rebalance flag flag as a DLB VM. The record Stage
// wrote decides, and a resource without one, which an earlier release wrote,
// falls back to the flag as the release reads it (see AutoRebalanceOf).
func (k ResourceKeys) InDLB(comment *string, flag *sdkclient.PVEBool) bool {
	if dlb, ok := DLBRecord(comment); ok {
		return dlb
	}
	return k.AutoRebalanceOf(flag)
}
