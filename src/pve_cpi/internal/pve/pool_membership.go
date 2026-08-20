// Pool-membership provenance: records how a workload VM's resource pool was
// resolved at create_vm time, on the VM's description sentinel (distinct
// top-level key from bosh_attached_disks/bosh_parked_disks so the codecs
// coexist — see sentinel.go).
//
// Why this exists: set_vm_metadata reconciles a VM's pool against the
// director-level pve.vm_pool_template. Re-deriving the template tokens from
// live metadata on every call could oscillate when the metadata map and the
// original create_vm env disagree; persisting the create-time inputs
// (director, deployment, instance_group) plus the winning resolution layer
// makes the re-render a pure function of stored state, so reconciliation is
// idempotent. Only layer "template" VMs are ever moved — a call-level,
// vm_type, or static pool choice is an operator decision the CPI must not
// override.
package pve

import (
	"context"
	"encoding/json"
	"fmt"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// Pool-resolution layer names persisted in the bosh_pool sentinel. These are
// wire values: changing one silently orphans every VM whose sentinel carries
// the old string (its layer no longer matches, so reconciliation skips it).
const (
	// PoolLayerCall: call-level cloud_properties.pool won.
	PoolLayerCall = "call"
	// PoolLayerVMType: the vm_type profile's cloud_properties.pool won.
	PoolLayerVMType = "vm_type"
	// PoolLayerTemplate: the rendered pve.vm_pool_template won. Only VMs on
	// this layer are reconciled/moved by set_vm_metadata.
	PoolLayerTemplate = "template"
	// PoolLayerStatic: the global pve.vm_pool default won.
	PoolLayerStatic = "static"
)

// poolMembershipSentinelKey is the top-level sentinel JSON key holding the
// create-time pool resolution record on a workload VM's description.
const poolMembershipSentinelKey = "bosh_pool"

// PoolMembership is the persisted pool-resolution record. Name is the pool
// the VM was assigned to at create time (updated after a reconciliation
// move); Layer is one of the PoolLayer* constants; the three token fields
// are the create-time template inputs (empty when underivable, e.g. director
// on a create-env path).
type PoolMembership struct {
	Name          string `json:"name,omitempty"`
	Layer         string `json:"layer,omitempty"`
	Director      string `json:"director,omitempty"`
	Deployment    string `json:"deployment,omitempty"`
	InstanceGroup string `json:"instance_group,omitempty"`
}

// GetPoolMembership parses a VM description and returns its bosh_pool record.
// Absent sentinel, absent key, or corrupted JSON all yield (nil, false) —
// callers treat such a VM as legacy (pre-provenance) and fall back to the
// adoption rules in set_vm_metadata's reconciler.
func GetPoolMembership(desc string) (*PoolMembership, bool) {
	_, raw := ParseSentinel(desc)
	rawPM, ok := raw[poolMembershipSentinelKey]
	if !ok {
		return nil, false
	}
	pm := &PoolMembership{}
	if err := json.Unmarshal(rawPM, pm); err != nil {
		return nil, false
	}
	if pm.Name == "" && pm.Layer == "" {
		return nil, false
	}
	return pm, true
}

// SetPoolMembershipOnDescription returns desc with its bosh_pool sentinel key
// replaced by pm, preserving the nonBOSH text and every other sentinel key.
// A nil pm deletes the key.
func SetPoolMembershipOnDescription(desc string, pm *PoolMembership) (string, error) {
	nonBOSH, raw := ParseSentinel(desc)
	if pm == nil {
		delete(raw, poolMembershipSentinelKey)
	} else {
		b, err := json.Marshal(pm)
		if err != nil {
			return "", err
		}
		raw[poolMembershipSentinelKey] = json.RawMessage(b)
	}
	return RenderSentinel(nonBOSH, raw)
}

// UpdatePoolMembership merges pm into the VM's description sentinel and
// writes it back via UpdateQemuConfig.
//
// Best-effort: any failure is logged at WARN and the function returns
// without error. The record is advisory provenance consumed only by
// set_vm_metadata's pool reconciler; losing this write degrades that VM to
// the legacy-adoption rules (moved only out of the static pve.vm_pool), not
// a correctness failure for the caller.
func UpdatePoolMembership(ctx context.Context, c Client, logger *log.Logger, node string, vmid int, pm *PoolMembership) {
	if c == nil || node == "" || vmid <= 0 || pm == nil {
		return
	}

	vmCfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		if logger != nil {
			logger.Warn("pool membership: config fetch failed — provenance not recorded",
				log.Int("vmid", vmid),
				log.String("node", node),
				log.String("pool", pm.Name),
				log.Err(err),
			)
		}
		return
	}

	newDesc, renderErr := SetPoolMembershipOnDescription(DescriptionFromConfig(vmCfg), pm)
	if renderErr != nil {
		if logger != nil {
			logger.Warn("pool membership: sentinel render failed — provenance not recorded",
				log.Int("vmid", vmid),
				log.String("pool", pm.Name),
				log.Err(renderErr),
			)
		}
		return
	}

	vmidStr := fmt.Sprintf("%d", vmid)
	if updErr := c.Nodes().UpdateQemuConfig(ctx, node, vmidStr,
		&sdknodes.UpdateQemuConfigParams{Description: &newDesc}); updErr != nil {
		if logger != nil {
			logger.Warn("pool membership: description write failed — provenance not recorded",
				log.Int("vmid", vmid),
				log.String("pool", pm.Name),
				log.Err(updErr),
			)
		}
	}
}
