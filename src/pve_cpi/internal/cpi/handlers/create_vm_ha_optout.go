package handlers

import (
	"fmt"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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

// storagePlanRequiresSharedISO reports whether pve.require_shared_iso_for_ha
// makes a storage plan put the VM's config-drive ISO on shared storage. A VM
// whose ha cloud property kept it out of HA never runs under PVE HA, so the
// setting asks nothing of it. That matches the plain create path, where
// checkISOStorageForHA finds no HA registration feature for such a VM and the
// HA decision never runs. Every other VM keeps the rule the plan has always
// applied, and a call with no parsed arguments keeps it too.
func storagePlanRequiresSharedISO(cfg *config.CPIConfig, parsed *createVMParsedArgs) bool {
	return cfg.RequireSharedISOForHAEnabled() && (parsed == nil || !parsed.cloudProps.haOff)
}
