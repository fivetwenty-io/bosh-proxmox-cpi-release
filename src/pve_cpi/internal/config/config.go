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
	VMStorage       string `json:"vm_storage"`
	DiskStorage     string `json:"disk_storage"`
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

	// TLS — pointer so JSON omission (nil) is distinguishable from explicit false.
	// Use VerifySSLValue() to obtain the effective bool.
	VerifySSL *bool `json:"verify_ssl,omitempty"`

	// Agent
	AgentMode    string `json:"agent_mode"`
	VMDiskFormat string `json:"vm_disk_format,omitempty"`
	LogLevel     string `json:"log_level,omitempty"`

	// VMID allocation
	VMIDRangeStart int `json:"vmid_range_start,omitempty"`

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
	// StemcellStorage defaults to VMStorage when not specified.
	if c.StemcellStorage == "" {
		c.StemcellStorage = c.VMStorage
	}
	if c.ISOStorage == "" {
		c.ISOStorage = "local"
	}
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

	// Authentication: exactly one of password or api_token must be set.
	hasPassword := c.Password != ""
	hasToken := c.APIToken != ""
	switch {
	case !hasPassword && !hasToken:
		errs = append(errs, "one of password or api_token is required")
	case hasPassword && hasToken:
		errs = append(errs, "password and api_token are mutually exclusive; provide only one")
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
