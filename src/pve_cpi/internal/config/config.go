// Package config loads and validates the BOSH CPI configuration JSON passed
// by the BOSH director at startup. It applies defaults for all optional fields
// and returns a fully populated, validated CPIConfig.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// CPIConfig holds all configuration loaded from the JSON config file.
// Fields map 1:1 to BOSH job properties in jobs/pve_cpi/spec.
// Tabs for indentation, 120-col max per CLAUDE.md.
type CPIConfig struct {
	// PVE connection
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	APIToken string `json:"api_token,omitempty"`
	Realm    string `json:"realm,omitempty"`
	Node     string `json:"node,omitempty"`

	// Storage
	VMStorage   string `json:"vm_storage"`
	DiskStorage string `json:"disk_storage"`
	// StemcellStorage receives qcow2 uploads and is referenced via
	// "<storage>:import/<filename>" in scsi0's `import-from=`. PVE only
	// allows uploads to file-based storages (dir/nfs/cifs/glusterfs/cephfs);
	// block storages (lvm, lvmthin, zfspool, rbd) cannot accept qcow2 uploads.
	StemcellStorage string `json:"stemcell_storage"`
	// ISOStorage is the PVE storage (must support content type `iso`,
	// i.e. dir/nfs/cifs) used to hold the per-VM ConfigDrive ISO that
	// boots the BOSH agent. Block storages (lvm/lvmthin/zfspool) cannot
	// hold ISO files. Defaults to "local".
	ISOStorage string `json:"iso_storage,omitempty"`

	// Network
	NetworkBridge string `json:"network_bridge"`

	// NetworkMode selects create_network/delete_network behavior.
	// "sdn" — PVE SDN vnet lifecycle (requires SDN enabled on the cluster).
	// "bridge" — Linux bridge lifecycle via nodes API.
	// "auto" — use SDN if cloud_properties.zone or config SDNZone is set;
	//           fall back to bridge otherwise.
	// Defaults to "auto".
	NetworkMode string `json:"network_mode,omitempty"`

	// SDNZone is the default PVE SDN zone name for vnet creation. Operators may
	// override per-call via cloud_properties.zone. When empty and NetworkMode
	// requires SDN, the zone must be supplied in cloud_properties.
	SDNZone string `json:"sdn_zone,omitempty"`

	// SDNZoneType is the PVE zone type used when the CPI creates the zone itself
	// (auto-manage enabled and zone absent). Valid values: simple, vlan, qinq,
	// vxlan, evpn. Defaults to "simple".
	SDNZoneType string `json:"sdn_zone_type,omitempty"`

	// SDNAutoManageZone controls whether the CPI may create and delete the zone.
	// When true, create_network creates the zone (type SDNZoneType) if absent,
	// and delete_network deletes the zone if: it is not pinned by SDNZone, and
	// it has no remaining vnets. Default false (operator manages zones; CPI
	// manages only vnets).
	SDNAutoManageZone bool `json:"sdn_auto_manage_zone,omitempty"`

	// TLS — pointer so JSON omission (nil) is distinguishable from explicit false.
	// Use VerifySSLValue() to obtain the effective bool.
	VerifySSL *bool `json:"verify_ssl,omitempty"`

	// Agent
	AgentMode    string `json:"agent_mode"`
	VMDiskFormat string `json:"vm_disk_format,omitempty"`
	LogLevel     string `json:"log_level,omitempty"`

	// Hotplug is the PVE `hotplug` flag baked into every new VM. Comma-list of
	// "network,disk,cpu,memory,usb,cloudinit"; "0" disables hotplug entirely.
	// Defaults to "network,disk,cpu,memory" so memory + CPU can be resized
	// live via qm set without rebooting the guest. Per-VM cloud_properties
	// can override (cloud_properties.hotplug) for stemcells that misbehave
	// on memory hot-add. Pointer-typed so an explicit empty / "0" value is
	// preserved through ApplyDefaults.
	Hotplug *string `json:"hotplug,omitempty"`

	// NUMA controls whether new VMs are created with NUMA enabled (numa=1).
	// PVE requires numa=1 at create time for memory hotplug to allocate DIMM
	// slots; without it, memory hot-add silently no-ops. Defaults to true.
	// Per-VM override via cloud_properties.numa. Pointer-typed so an explicit
	// false survives ApplyDefaults.
	NUMA *bool `json:"numa,omitempty"`

	// VMID allocation
	VMIDRangeStart int `json:"vmid_range_start,omitempty"`
	// VMIDRangeEnd is the inclusive upper bound of the VMID range for VM
	// allocation. VMs are allocated in [VMIDRangeStart, VMIDRangeEnd].
	// Defaults to 5999. Must be > VMIDRangeStart and <= 9999.
	// Persistent disks use synthetic VMIDs 9000-9999 (unaffected by this field).
	VMIDRangeEnd int `json:"vmid_range_end,omitempty"`
	// VMIDAllocAttempts is the maximum number of retries for VMID-conflict
	// recovery in create_vm / create_disk. ≤0 → use the handler default (5).
	// Cross-process VMID collisions surface as PVE 500 "already exists"
	// errors; the retry loop allocates a fresh VMID and re-attempts.
	VMIDAllocAttempts int `json:"vmid_alloc_attempts,omitempty"`

	// AllowDiskOpsWithSnapshots bypasses the snapshot pre-flight guard in
	// attach_disk, detach_disk, and resize_disk when true. Use only for
	// emergency disk recovery; snapshot state will be inconsistent after
	// the operation. Default false (guard active).
	AllowDiskOpsWithSnapshots bool `json:"allow_disk_ops_with_snapshots,omitempty"`

	// RequireSnapshotCheckPass controls behavior when the snapshot pre-flight
	// check itself fails (cannot reach PVE to list snapshots). When true, the
	// disk operation aborts. Default false: on check failure the CPI logs a
	// warning and proceeds (fail-open).
	RequireSnapshotCheckPass bool `json:"require_snapshot_check_pass,omitempty"`

	// Registry (required only when agent_mode == "registry")
	RegistryEndpoint string `json:"registry_endpoint,omitempty"`
	RegistryUser     string `json:"registry_user,omitempty"`
	RegistryPassword string `json:"registry_password,omitempty"`

	// AgentMBus is the URL the BOSH agent should bind/listen on inside the VM
	// (e.g. https://mbus:pw@0.0.0.0:6868). Sourced from
	// cloud_provider.properties.agent.mbus during `bosh create-env` because
	// bosh-init does not pass it via the per-call env argument. The CPI uses
	// it as the settings.json top-level `mbus` field when env doesn't carry
	// one already.
	AgentMBus string `json:"agent_mbus,omitempty"`

	// AgentBlobstore mirrors AgentMBus: the blobstore agent settings.json
	// should advertise. Populated from cloud_provider.properties.agent.blobstore.
	AgentBlobstore map[string]any `json:"agent_blobstore,omitempty"`

	// VMPrefix is prepended to every CPI-provisioned VM's PVE name. When set
	// (e.g. "cpi") VM names are formed as "<prefix>-<deployment>-<job>-<index>"
	// so deployments sharing a PVE cluster are easy to filter in the UI. When
	// empty, the prefix is omitted and the name format is
	// "<deployment>-<job>-<index>".
	VMPrefix string `json:"vm_prefix,omitempty"`

	// CreateEnvDeployment is the synthetic deployment name used in VM names
	// for VMs created via `bosh create-env`. bosh-init does not pass a
	// deployment in env, so a stable placeholder is required for the
	// "<deployment>" segment. Defaults to "create-env" so a director booted
	// via create-env shows up as "<prefix>-create-env-bosh-0".
	CreateEnvDeployment string `json:"create_env_deployment,omitempty"`

	// RebootMode selects the reboot_vm strategy. "soft" (default) issues a
	// graceful ACPI shutdown/reboot and falls back to a hard reset after
	// RebootTimeout seconds if the guest has not powered off. "hard" issues an
	// immediate /status/reset without a graceful-wait phase.
	RebootMode string `json:"reboot_mode,omitempty"`

	// RebootTimeout is the number of seconds the soft reboot path waits for a
	// graceful ACPI shutdown before falling back to a hard reset. Ignored when
	// RebootMode is "hard". Valid range 1–3600; defaults to 60.
	RebootTimeout int `json:"reboot_timeout,omitempty"`
}

// Load decodes CPIConfig from r, applies defaults, then validates.
// Returns a CloudError on decode failure or validation failure.
func Load(r io.Reader) (*CPIConfig, error) {
	var cfg CPIConfig
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, cpierrors.Cloud("config: decode failed: %s", err.Error())
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadFile opens path and delegates to Load.
// Returns a CloudError on open failure, decode failure, or validation failure.
func LoadFile(path string) (*CPIConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, cpierrors.Cloud("config: open %s: %s", path, err.Error())
	}
	defer f.Close()
	return Load(f)
}

// VerifySSLValue returns the effective TLS-verification bool.
// nil (field absent from JSON) → true (secure by default).
// *false → false (caller explicitly disabled verification).
// *true  → true.
func (c *CPIConfig) VerifySSLValue() bool {
	if c.VerifySSL == nil {
		return true
	}
	return *c.VerifySSL
}

// ApplyDefaults sets zero-value optional fields to their documented defaults.
// Callers must invoke this before Validate when constructing a CPIConfig manually.
func (c *CPIConfig) ApplyDefaults() {
	if c.Port == 0 {
		c.Port = 8006
	}
	if c.Realm == "" {
		c.Realm = "pam"
	}
	// VerifySSL is a *bool. nil means the field was absent from JSON → default true.
	// An explicit false is preserved — ApplyDefaults never overwrites a set value.
	if c.VerifySSL == nil {
		t := true
		c.VerifySSL = &t
	}
	if c.AgentMode == "" {
		c.AgentMode = "cloudinit"
	}
	if c.VMDiskFormat == "" {
		c.VMDiskFormat = "qcow2"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.VMIDRangeStart == 0 {
		c.VMIDRangeStart = 100
	}
	if c.VMIDRangeEnd == 0 {
		c.VMIDRangeEnd = 5999
	}
	// StemcellStorage defaults to VMStorage when not specified.
	if c.StemcellStorage == "" {
		c.StemcellStorage = c.VMStorage
	}
	if c.ISOStorage == "" {
		c.ISOStorage = "local"
	}
	if c.Hotplug == nil {
		s := "network,disk,cpu,memory"
		c.Hotplug = &s
	}
	if c.NUMA == nil {
		t := true
		c.NUMA = &t
	}
	if c.CreateEnvDeployment == "" {
		c.CreateEnvDeployment = "create-env"
	}
	if c.RebootMode == "" {
		c.RebootMode = "soft"
	}
	if c.RebootTimeout <= 0 {
		c.RebootTimeout = 60
	}
	if c.NetworkMode == "" {
		c.NetworkMode = "auto"
	}
	if c.SDNZoneType == "" {
		c.SDNZoneType = "simple"
	}
}

// RebootModeValue returns the effective reboot mode, defaulting to "soft" when
// the field is empty (e.g. config constructed manually without ApplyDefaults).
func (c *CPIConfig) RebootModeValue() string {
	if c.RebootMode == "" {
		return "soft"
	}
	return c.RebootMode
}

// RebootTimeoutValue returns the effective reboot timeout in seconds, defaulting
// to 60 when the field is zero or negative (e.g. config constructed manually
// without ApplyDefaults).
func (c *CPIConfig) RebootTimeoutValue() int {
	if c.RebootTimeout <= 0 {
		return 60
	}
	return c.RebootTimeout
}

// HotplugValue returns the effective hotplug flag, falling back to the
// CPI default ("network,disk,cpu,memory") when the pointer is nil.
func (c *CPIConfig) HotplugValue() string {
	if c.Hotplug == nil {
		return "network,disk,cpu,memory"
	}
	return *c.Hotplug
}

// NUMAValue returns the effective NUMA toggle, defaulting to true (memory
// hotplug requires numa=1 at create time) when the pointer is nil.
func (c *CPIConfig) NUMAValue() bool {
	if c.NUMA == nil {
		return true
	}
	return *c.NUMA
}

// Validate checks all required fields and enum constraints.
// Returns a CloudError whose message lists every violation, separated by "; ".
func (c *CPIConfig) Validate() error {
	var errs []string

	// Required scalar fields.
	if c.Host == "" {
		errs = append(errs, "host is required")
	}
	if c.User == "" {
		errs = append(errs, "user is required")
	}
	if c.VMStorage == "" {
		errs = append(errs, "vm_storage is required")
	}
	if c.DiskStorage == "" {
		errs = append(errs, "disk_storage is required")
	}
	if c.NetworkBridge == "" {
		errs = append(errs, "network_bridge is required")
	}

	// Authentication: at least one of password or api_token must be set.
	// When both are present (e.g. kit renders a placeholder password alongside
	// a real api_token because credhub entombment rejects empty values), the
	// api_token wins — clear the password so downstream code uses tokens.
	hasPassword := c.Password != ""
	hasToken := c.APIToken != ""
	if hasPassword && hasToken {
		c.Password = ""
		hasPassword = false
	}
	if !hasPassword && !hasToken {
		errs = append(errs, "one of password or api_token is required")
	}

	// Port range.
	if c.Port <= 0 || c.Port >= 65536 {
		errs = append(errs, fmt.Sprintf("port must be 1–65535, got %d", c.Port))
	}

	// AgentMode enum.
	switch c.AgentMode {
	case "cloudinit", "registry", "noagent":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			"agent_mode must be one of cloudinit|registry|noagent, got %q", c.AgentMode,
		))
	}

	// VMDiskFormat enum.
	switch c.VMDiskFormat {
	case "qcow2", "raw", "vmdk":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			"vm_disk_format must be one of qcow2|raw|vmdk, got %q", c.VMDiskFormat,
		))
	}

	// LogLevel enum.
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			"log_level must be one of debug|info|warn|error, got %q", c.LogLevel,
		))
	}

	// VMIDRangeStart: PVE reserves 0–99.
	if c.VMIDRangeStart < 100 {
		errs = append(errs, fmt.Sprintf(
			"vmid_range_start must be ≥100 (PVE reserved range), got %d", c.VMIDRangeStart,
		))
	}

	// VMIDRangeEnd: must be strictly greater than VMIDRangeStart and within PVE
	// VM VMID space (max 9999; disk range 9000-9999 is separate).
	if c.VMIDRangeEnd <= c.VMIDRangeStart {
		errs = append(errs, fmt.Sprintf(
			"vmid_range_end must be > vmid_range_start (%d), got %d", c.VMIDRangeStart, c.VMIDRangeEnd,
		))
	} else if c.VMIDRangeEnd > 9999 {
		errs = append(errs, fmt.Sprintf(
			"vmid_range_end must be ≤9999, got %d", c.VMIDRangeEnd,
		))
	}

	// RebootMode enum.
	switch c.RebootMode {
	case "soft", "hard":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			`reboot_mode must be one of soft|hard, got %q`, c.RebootMode,
		))
	}

	// RebootTimeout range: 1–3600 seconds.
	if c.RebootTimeout < 1 || c.RebootTimeout > 3600 {
		errs = append(errs, fmt.Sprintf(
			"reboot_timeout must be 1-3600 seconds, got %d", c.RebootTimeout,
		))
	}

	// NetworkMode enum.
	switch c.NetworkMode {
	case "sdn", "bridge", "auto":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			"network_mode must be one of sdn|bridge|auto, got %q", c.NetworkMode,
		))
	}

	// SDNZoneType enum — only validated when the SDN path is reachable.
	if c.NetworkMode == "sdn" || c.NetworkMode == "auto" {
		switch c.SDNZoneType {
		case "simple", "vlan", "qinq", "vxlan", "evpn":
			// valid
		default:
			errs = append(errs, fmt.Sprintf(
				"sdn_zone_type must be one of simple|vlan|qinq|vxlan|evpn, got %q", c.SDNZoneType,
			))
		}
	}

	// Registry mode requires endpoint + credentials.
	if c.AgentMode == "registry" {
		if c.RegistryEndpoint == "" {
			errs = append(errs, "registry_endpoint is required when agent_mode=registry")
		}
		if c.RegistryUser == "" {
			errs = append(errs, "registry_user is required when agent_mode=registry")
		}
		if c.RegistryPassword == "" {
			errs = append(errs, "registry_password is required when agent_mode=registry")
		}
	}

	if len(errs) > 0 {
		return cpierrors.Cloud("config validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
