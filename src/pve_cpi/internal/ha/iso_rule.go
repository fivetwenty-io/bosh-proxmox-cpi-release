package ha

import "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"

// LocalISORule says when DecideVM fails a call because the VM's config-drive
// ISO sits on node-local storage, rather than take the VM out of full HA with
// a warning. A caller that must settle the ISO's storage before the VM exists,
// such as a storage plan, reads the rule here, so it asks for a shared ISO in
// exactly the cases the decision made after the VM exists would refuse a
// node-local one.
//
// The rule assumes the decision reaches the VM's volumes, which it does on a
// cluster whose profile lets the CPI write HA state, for a VM inside its own
// pin, and with enough placeable nodes in its CPU migration domain. A caller
// that cannot see those facts before the VM exists therefore asks for a shared
// ISO for a VM that the decision might still leave out of HA, which never lets
// a VM that PVE HA could start on another node keep a node-local ISO.
type LocalISORule struct {
	// Always is true when a node-local ISO fails the call whatever storage the
	// VM's disks are on. That is the case under pve.ha.storage_topology
	// shared, which refuses every node-local volume, and under the replicate
	// local policy whenever the ISO escalates, because a VM with a node-local
	// disk reaches replicate, and replication never copies the ISO, while a VM
	// whose disks are all shared would otherwise get full HA.
	Always bool

	// SharedDisks is true when a node-local ISO fails the call of a VM whose
	// disks are all on shared storage, because the ISO alone keeps that VM
	// from full HA. Under pve.ha.storage_topology local no disk reads as
	// shared, so no VM would otherwise get full HA and the field is false.
	SharedDisks bool
}

// Fails reports whether a node-local ISO fails the call of a VM. disksShared
// says that every disk of the VM is on storage that PVE reports as shared.
func (r LocalISORule) Fails(disksShared bool) bool {
	return r.Always || r.SharedDisks && disksShared
}

// Active reports whether a node-local ISO fails the call of any VM.
func (r LocalISORule) Active() bool {
	return r.Always || r.SharedDisks
}

// LocalISORuleFor returns the rule DecideVM applies under cfg when pve.ha.mode
// is mode. Mode off makes no decision, so it refuses no ISO, and neither does
// a nil config.
func LocalISORuleFor(cfg *config.CPIConfig, mode string) LocalISORule {
	if cfg == nil || mode != config.HAModeAuto && mode != config.HAModeManaged {
		return LocalISORule{}
	}
	escalates := localISOEscalates(cfg, mode == config.HAModeManaged)
	topology := cfg.HAStorageTopologyValue()
	return LocalISORule{
		Always: topology == config.HAStorageTopologyShared ||
			escalates && cfg.HALocalPolicyValue() == config.HALocalPolicyReplicate,
		SharedDisks: escalates && topology != config.HAStorageTopologyLocal,
	}
}

// localISOEscalates reports whether a node-local ISO fails the call of a VM
// that would otherwise get full HA, or that the replicate policy covers,
// instead of only downgrading the VM with a warning. Managed mode always
// fails it, and pve.require_shared_iso_for_ha makes auto mode fail it too.
func localISOEscalates(cfg *config.CPIConfig, managed bool) bool {
	return managed || cfg.RequireSharedISOForHAEnabled()
}
