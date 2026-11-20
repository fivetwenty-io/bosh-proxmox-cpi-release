package handlers

import (
	"encoding/json"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// haAntiAffinityCloudProp is the cloud property that names the anti-affinity
// pattern of a VM's instance group.
const haAntiAffinityCloudProp = "ha_anti_affinity"

// resolveAntiAffinityPattern reads the ha_anti_affinity cloud property from
// the call's cloud_properties, then the disk_type profile, then the vm_type
// profile, and returns the pattern's canonical form, or "" when no layer sets
// it. A value that is not a valid pattern fails the create before any VM
// exists, because it is a manifest error that no retry fixes.
func resolveAntiAffinityPattern(cloudProps map[string]any, cfg *config.CPIConfig) (string, error) {
	r, err := newLayeredResolver(cloudProps, cfg)
	if err != nil {
		return "", err
	}
	v, ok := r.Raw(haAntiAffinityCloudProp)
	if !ok {
		return "", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", cpierrors.Cloud("create_vm: cloud property %s: %s", haAntiAffinityCloudProp, err.Error())
	}
	p, err := aapattern.DecodeJSON(raw)
	if err != nil {
		return "", cpierrors.Cloud("create_vm: cloud property %s: %s", haAntiAffinityCloudProp, err.Error())
	}
	return p.Canonical(), nil
}
