package agent

import "context"

// Agent abstracts the agent bootstrap strategy.
// Implementations: ConfigDrive, NoAgent.
type Agent interface {
	// Configure writes agent settings to the VM (ConfigDrive ISO).
	// Called by create_vm AFTER VM is created but BEFORE it is started.
	Configure(ctx context.Context, node string, vmid int, cfg AgentConfig) error

	// Remove cleans up agent-side artifacts on VM deletion (or on
	// create_vm rollback).
	//   - ConfigDrive: deletes the ConfigDrive ISO volume from the
	//                  storage pool (404-tolerant).
	//   - NoAgent:     no-op.
	Remove(ctx context.Context, node string, vmid int) error
}

// AgentConfig is the complete agent bootstrap payload.
// Maps to the settings.json / registry record format defined by BOSH agent.
//
// nolint:revive // "AgentConfig" intentionally stutters with the package name;
// the package is imported as `agent` and the symbol's full name (agent.AgentConfig)
// is widely referenced across the codebase. Renaming is a breaking change.
type AgentConfig struct {
	AgentID   string
	Networks  map[string]NetworkSpec
	Disks     DisksSpec
	Env       map[string]any
	MBus      string
	Blobstore BlobstoreSpec
	VM        VMSpec
	NTP       []string
}

// DisksSpec describes the disk layout written into the agent settings payload,
// identifying the system, ephemeral, and persistent disk bus indices by their
// virtio slot numbers (as strings) so the BOSH agent can locate each device.
type DisksSpec struct {
	// System is the bus index (string) of the root disk; the stemcell's
	// agent.json DevicePathResolutionType=virtio resolves "0" to /dev/vda.
	System string `json:"system"`
	// Ephemeral is the bus index (string) of the ephemeral disk, or empty
	// when no dedicated ephemeral disk is attached (the agent then carves
	// the ephemeral partition out of the root disk).
	Ephemeral  string            `json:"ephemeral"`
	Persistent map[string]string `json:"persistent"` // disk_cid → bus index; empty at create time
}

// VMSpec carries the VM identity fields (name and VMID) embedded in the agent
// settings payload so the BOSH agent can self-identify after first boot.
type VMSpec struct {
	Name string `json:"name"` // "vm-{vmid}"
	ID   string `json:"id"`   // vmid as string
}

// NetworkSpec describes a single network interface entry in the agent settings
// payload, including addressing, gateway, DNS resolvers, and default-route flags.
type NetworkSpec struct {
	Type    string   `json:"type"`
	IP      string   `json:"ip"`
	Netmask string   `json:"netmask"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
	Default []string `json:"default"`
}

// BlobstoreSpec identifies the blobstore provider and its configuration options
// as required by the BOSH agent settings payload.
type BlobstoreSpec struct {
	Provider string         `json:"provider"`
	Options  map[string]any `json:"options"`
}
