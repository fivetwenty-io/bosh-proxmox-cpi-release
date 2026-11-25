package handlers

import (
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// haCloudProp is the cloud property that keeps one VM out of PVE HA. Only
// false has an effect. Unset or true leaves the VM to the HA decision, and
// true never forces HA on a VM the decision would refuse.
const haCloudProp = "ha"

// haOffTag is the PVE tag create_vm writes on a VM whose ha cloud property
// kept it out of PVE HA, so the doctor and an operator can tell an opted-out
// VM from one the HA decision refused. It is written once into the VM's first
// tag list, and no operator tag may claim it.
const haOffTag = "bosh-ha-off"

// resolveCreateVMHAOptOut reads the ha cloud property from the call's
// cloud_properties, then the disk_type profile, then the vm_type profile, and
// marks the cloud properties when the VM opts out of PVE HA. A value that is
// not a boolean fails the create before any VM exists, because it is a
// manifest error that no retry fixes. The property is checked in every mode,
// but it takes a VM out of HA only when pve.ha.mode is not off, because under
// off no VM is in HA and the opt-out changes nothing.
func resolveCreateVMHAOptOut(parsed *createVMParsedArgs, cfg *config.CPIConfig) error {
	r, err := newLayeredResolver(parsed.cloudPropsMap, cfg)
	if err != nil {
		return err
	}
	v, ok := r.Raw(haCloudProp)
	if !ok {
		return nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return cpierrors.Cloud("create_vm: cloud property %s: must be a boolean, got %s", haCloudProp,
			describeHAValue(v))
	}
	parsed.cloudProps.haOff = !b && cfg.HAModeValue() != config.HAModeOff
	return nil
}

// describeHAValue names the type and value of a bad ha cloud property, so
// the error shows what the manifest set.
func describeHAValue(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("the string %q", x)
	case float64:
		return fmt.Sprintf("the number %v", x)
	case map[string]any:
		return "a map"
	case []any:
		return "a list"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// StoragePlanISOPolicy says when a storage plan must put the VM's config-drive
// ISO on shared storage. It restates the rule of the HA decision, which the
// plain create path applies after the VM exists, so the plan refuses a
// node-local ISO in the cases where that decision would fail the create and in
// no others. A zero policy asks nothing of the ISO.
type StoragePlanISOPolicy struct {
	// Live is false for a VM that PVE HA never manages, which is a VM whose ha
	// cloud property is false and any VM under pve.ha.mode off. Such a VM asks
	// nothing of its ISO.
	Live bool
	// Setting is pve.require_shared_iso_for_ha for a VM that may join PVE HA.
	// A VM with an HA registration feature, which the plan sees as HA nodes,
	// fails with a node-local ISO under it in every mode that writes HA state.
	Setting bool
	// Rule is when the HA decision fails a VM over a node-local ISO.
	Rule ha.LocalISORule
}

// Requires reports whether the ISO must sit on shared storage. haNodes is the
// number of nodes PVE HA could start the VM on, and disksShared says that
// every disk of the VM is on shared storage.
func (p StoragePlanISOPolicy) Requires(haNodes int, disksShared bool) bool {
	if !p.Live {
		return false
	}
	return p.Rule.Fails(disksShared) || p.Setting && haNodes > 0
}

// storagePlanISOPolicy returns the shared-ISO policy of a create. The rule
// comes from the ha package, so the plan and the decision cannot drift apart.
// A call with no parsed arguments keeps its VM in HA.
func storagePlanISOPolicy(cfg *config.CPIConfig, parsed *createVMParsedArgs) StoragePlanISOPolicy {
	if cfg == nil || parsed != nil && parsed.cloudProps.haOff {
		return StoragePlanISOPolicy{}
	}
	mode := cfg.HAModeValue()
	if mode == config.HAModeOff {
		return StoragePlanISOPolicy{}
	}
	return StoragePlanISOPolicy{
		Live: true, Setting: cfg.RequireSharedISOForHAEnabled(), Rule: ha.LocalISORuleFor(cfg, mode),
	}
}
