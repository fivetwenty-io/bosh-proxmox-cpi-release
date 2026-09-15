// Create-time provenance write: the single read-modify-write that stamps every
// record create_vm knows about onto a freshly created guest's description
// sentinel. Today that is the pool resolution (bosh_pool) and the stemcell the
// guest booted from (bosh_stemcell); the codecs themselves live in
// pool_membership.go and vm_stemcell.go.
//
// Why one function rather than one call per record: create_vm runs this on the
// hot path of every VM a deploy creates, and each separate writer would cost
// its own config read and its own UpdateQemuConfig. Merging both keys into one
// read and one write keeps a several-hundred-VM deploy from paying twice for
// provenance nobody reads until something goes wrong.
package pve

import (
	"context"
	"fmt"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// UpdateVMCreateProvenance merges pm and sc into the VM's description sentinel
// and writes the result back with a single UpdateQemuConfig. A nil record is
// skipped rather than deleted, so a caller that knows only one of the two
// leaves the other untouched. Nothing is read or written when both are nil.
//
// Best-effort, on the same terms as UpdatePoolMembership: every failure is
// logged at WARN and the function returns without an error. These records are
// advisory, consumed by set_vm_metadata's pool reconciler and by operators
// auditing the cluster, so losing the write degrades an answer rather than the
// create_vm the caller asked for.
func UpdateVMCreateProvenance(
	ctx context.Context,
	c Client,
	logger *log.Logger,
	node string,
	vmid int,
	pm *PoolMembership,
	sc *VMStemcell,
) {
	if c == nil || node == "" || vmid <= 0 || (pm == nil && sc == nil) {
		return
	}

	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if logger != nil {
			logger.Warn("create provenance: config fetch failed — pool and stemcell not recorded",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.Err(err),
			)
		}
		return
	}

	desc := DescriptionFromConfig(vmCfg)
	if pm != nil {
		desc, err = SetPoolMembershipOnDescription(desc, pm)
		if err != nil {
			if logger != nil {
				logger.Warn("create provenance: pool sentinel render failed — nothing recorded",
					log.Int("vmid", vmid), log.String("pool", pm.Name), log.Err(err))
			}
			return
		}
	}
	if sc != nil {
		desc, err = SetVMStemcellOnDescription(desc, sc)
		if err != nil {
			if logger != nil {
				logger.Warn("create provenance: stemcell sentinel render failed — nothing recorded",
					log.Int("vmid", vmid), log.String("stemcell", sc.Label), log.Err(err))
			}
			return
		}
	}

	vmidStr := fmt.Sprintf("%d", vmid)
	if updErr := c.Nodes().UpdateQemuConfig(ctx, node, vmidStr,
		&sdknodes.UpdateQemuConfigParams{Description: &desc}); updErr != nil {
		if logger != nil {
			logger.Warn("create provenance: description write failed — pool and stemcell not recorded",
				log.Int("vmid", vmid),
				log.Err(updErr),
			)
		}
	}
}
