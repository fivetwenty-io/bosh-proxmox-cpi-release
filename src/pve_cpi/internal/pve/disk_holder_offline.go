package pve

import (
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// DiskHolderOfflineRefusal is the cause of the refusal the disk holder scan
// returns when it found no guest holding the disk among the nodes it could
// read, and it left out nodes the quorate cluster reports offline. Those
// nodes' guests were never read, so the scan cannot say the disk is
// unattached. Nodes lists them, in the order the scan left them out.
//
// The scan returns it wrapped in a retriable CPI error whose text names the
// disk, so errors.As finds it, and the classification and the text stay those
// of the refusal.
type DiskHolderOfflineRefusal struct {
	Volid string
	Nodes []string
}

// Error names the offline nodes and tells the operator the operation can be
// retried once they are back, because nothing else will clear the refusal.
func (e *DiskHolderOfflineRefusal) Error() string {
	nodes := strings.Join(e.Nodes, ",")
	return fmt.Sprintf("not found among the reachable guests, but node(s) %s were excluded as offline; "+
		"cannot prove the disk is unattached, so retry the operation once node(s) %s are back in the cluster",
		nodes, nodes)
}

// diskHolderOfflineRefusal builds the scan's refusal for volid with the nodes
// it left out.
func diskHolderOfflineRefusal(volid string, nodes []string) error {
	cause := &DiskHolderOfflineRefusal{Volid: volid, Nodes: append([]string(nil), nodes...)}
	return cpierrors.WrapAs(cause, cpierrors.TypeRetriableCloud, fmt.Sprintf("disk %q", volid))
}
