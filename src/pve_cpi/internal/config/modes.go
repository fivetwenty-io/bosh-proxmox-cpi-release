package config

// The CPI's three mode enums share the literal "auto" but resolve it with
// unrelated rules, so each family gets its own named constants. This package
// owns the enums because it validates them (validateEnumFields) and defaults
// them (ApplyDefaults); handlers and cmd/cpi reference these names instead of
// re-spelling the literals.

// AgentMode values (pve.agent_mode). "auto" behaves as cloudinit for the
// primary boot agent — see BootAgentMode, the single owner of that rule.
const (
	// AgentModeAuto defers the choice: the boot agent is cloudinit, and
	// per-request logic may still select noagent behavior where documented.
	AgentModeAuto = "auto"
	// AgentModeCloudInit boots VMs with a cloud-init configdrive ISO.
	AgentModeCloudInit = "cloudinit"
	// AgentModeNoAgent skips BOSH agent bootstrapping entirely.
	AgentModeNoAgent = "noagent"
)

// NetworkMode values (pve.network_mode).
const (
	// NetworkModeAuto selects SDN when a zone or vnet is resolvable, bridge
	// otherwise (legacy heuristic).
	NetworkModeAuto = "auto"
	// NetworkModeSDN forces the SDN vnet path.
	NetworkModeSDN = "sdn"
	// NetworkModeBridge forces classic Linux-bridge NICs; an explicit zone or
	// vnet in cloud_properties still takes the SDN path.
	NetworkModeBridge = "bridge"
)

// StemcellStrategy values (pve.stemcell_strategy; per-VM override via
// cloud_properties.stemcell_strategy).
const (
	// StemcellStrategyTemplate clones the per-cluster stemcell cache template
	// (CoW-fast; cache built eagerly by create_stemcell).
	StemcellStrategyTemplate = "template"
	// StemcellStrategyImport imports the stemcell qcow2 directly into the VM
	// root disk (full copy per VM; template-independent).
	StemcellStrategyImport = "import"
)

// CloneMode values (pve.clone_mode).
const (
	// CloneModeAuto picks linked clones on snapshot-capable backends and full
	// clones elsewhere.
	CloneModeAuto = "auto"
	// CloneModeLinked forces linked clones.
	CloneModeLinked = "linked"
	// CloneModeFull forces full clones.
	CloneModeFull = "full"
)

// BootAgentMode returns the agent mode the primary boot agent must be built
// with: AgentModeAuto resolves to AgentModeCloudInit, every other mode passes
// through. Both process boot (cmd/cpi) and per-request override bundles
// (handlers.WithRequestOverrides) build their boot agent from this method so
// the two paths cannot diverge.
func (c *CPIConfig) BootAgentMode() string {
	if c.AgentMode == AgentModeAuto {
		return AgentModeCloudInit
	}
	return c.AgentMode
}
