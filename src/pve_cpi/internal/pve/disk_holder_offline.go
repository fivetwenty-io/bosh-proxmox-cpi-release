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

// CreatedVolume records that the request about to park a volume is the one
// that created it, moments earlier, and names that volume. Only
// VolumeCreatedByThisRequest makes one, and its zero value names nothing.
//
// It exists for one decision. A volume this request created can't be held by
// any guest, on any node, because no request could have attached it yet, so
// ParkDisk doesn't need the holder scan to prove that. When the scan leaves
// out a node the cluster reports offline and finds no holder anywhere else,
// ParkDisk parks a volume this value names instead of refusing. Every other
// volume, and every other caller of the scan, keeps the refusal.
type CreatedVolume struct {
	volid string
}

// VolumeCreatedByThisRequest returns the record for volid, which must be the
// volume the calling request's own create call just returned. create_disk
// makes it right after its create call succeeds and hands it to the park
// through ParkContext.Created. No other caller may make one, because the
// value tells ParkDisk to park the volume without proof that no guest on an
// offline node holds it.
func VolumeCreatedByThisRequest(volid string) CreatedVolume {
	return CreatedVolume{volid: volid}
}

// Volid returns the volume the record names, or "" for the zero value.
func (v CreatedVolume) Volid() string { return v.volid }

// names reports whether the record names bareVolid exactly. The zero value
// names no volume.
func (v CreatedVolume) names(bareVolid string) bool {
	return v.volid != "" && v.volid == bareVolid
}
