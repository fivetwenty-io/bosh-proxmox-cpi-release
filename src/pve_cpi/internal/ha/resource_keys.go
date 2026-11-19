package ha

import (
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// The pve-manager releases that bring the optional keys of an HA resource.
// The failback key came with pve-ha-manager 5.0.2, and every PVE 9.0 release
// depends on a later one. The auto-rebalance key came with pve-ha-manager
// 5.2.2, and pve-manager 9.1.14 is the first release that depends on a
// pve-ha-manager with it. PVE parses resources.cfg with a schema that refuses
// any key it does not know, so a write that names a key one node lacks fails.
const (
	failbackKeyMajor      = 9
	failbackKeyMinor      = 0
	failbackKeyPatch      = 0
	autoRebalanceKeyMajor = 9
	autoRebalanceKeyMinor = 1
	autoRebalanceKeyPatch = 14
)

// ResourceKeys says which optional keys every online node's release accepts
// on an HA resource. The zero value is a release the CPI could not read, and
// it names neither key.
type ResourceKeys struct {
	Failback      bool
	AutoRebalance bool
}

// ResourceKeys returns the optional resource keys of the lowest release among
// the profile's online nodes. A profile that could not read every online
// node's release names neither key.
func (p *Profile) ResourceKeys() ResourceKeys {
	if p == nil {
		return ResourceKeys{}
	}
	v, ok := parseVersion(p.MinVersion)
	if !ok {
		return ResourceKeys{}
	}
	return ResourceKeys{
		Failback:      v.AtLeastPatch(failbackKeyMajor, failbackKeyMinor, failbackKeyPatch),
		AutoRebalance: v.AtLeastPatch(autoRebalanceKeyMajor, autoRebalanceKeyMinor, autoRebalanceKeyPatch),
	}
}

// AutoRebalanceOf reads the auto-rebalance flag of an HA resource. Where the
// release has the key, an unset flag is PVE's default of on. Where it does
// not, no resource rebalances, so only a flag that reads on counts, which
// keeps an unset flag from making a VM a DLB VM on a release without the
// dynamic load balancer.
func (k ResourceKeys) AutoRebalanceOf(v *sdkclient.PVEBool) bool {
	if !k.AutoRebalance {
		return v != nil && v.Bool()
	}
	return AutoRebalanceOf(v)
}

// sendsFailback reports whether a write of failback want names the key. A
// release with the key always gets it. Otherwise the write names it only to
// turn failback off, because leaving the key out means PVE's default of on,
// and a write PVE refuses fails the call, where a silent default would leave
// failback on.
func (k ResourceKeys) sendsFailback(want bool) bool {
	return k.Failback || !want
}
