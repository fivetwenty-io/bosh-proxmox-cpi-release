package config

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
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

	// StemcellStagingDir is an optional absolute path that scopes all stemcell
	// file reads/writes to a single directory root using Go 1.24+ os.Root.
	// When empty (the default), os.Open/os.Create are called on paths as-is
	// (byte-identical behavior to prior releases). When set, director-supplied
	// image paths must reside under this directory; out-of-root paths are
	// rejected at runtime. Defense-in-depth against unexpected stemcell paths.
	StemcellStagingDir string `json:"stemcell_staging_dir,omitempty"`
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

	// PVECACertPEM is an optional PEM-encoded CA certificate (or chain) that
	// replaces the system trust pool when verifying the PVE API TLS certificate.
	// Use when the PVE host presents a certificate signed by a private CA that
	// the host trust store does not already include. When empty (the default),
	// TLS verification uses the system trust pool — behavior is byte-identical to
	// prior releases. Ignored when VerifySSL is false. Symmetric to
	// RegistryCACertPEM / registry.ca_cert.
	PVECACertPEM string `json:"pve_ca_cert,omitempty"`

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
	// Defaults to 8999. Must be > VMIDRangeStart and <= 8999 (the disk range
	// begins at 9000). Persistent disks use synthetic VMIDs 9000-29999
	// (unaffected by this field).
	VMIDRangeEnd int `json:"vmid_range_end,omitempty"`
	// VMIDAllocAttempts is the maximum number of retries for VMID-conflict
	// recovery in create_vm / create_disk. ≤0 → use the handler default (5).
	// Cross-process VMID collisions surface as PVE 500 "already exists"
	// errors; the retry loop allocates a fresh VMID and re-attempts.
	VMIDAllocAttempts int `json:"vmid_alloc_attempts,omitempty"`

	// DiskVMIDRangeStart is the inclusive lower bound of the synthetic VMID
	// range for persistent-disk containers (create_disk). ApplyDefaults sets
	// to 9000 when 0 (mirrors pve.VMIDRangeDiskStart). Must not overlap the VM
	// range or the template range. validate-only-when-set; omit from ERB when zero.
	DiskVMIDRangeStart int `json:"disk_vmid_range_start,omitempty"`

	// DiskVMIDRangeEnd is the inclusive upper bound of the synthetic VMID range
	// for persistent-disk containers. Must be > DiskVMIDRangeStart.
	// ApplyDefaults sets to 29999 when 0 (mirrors pve.VMIDRangeDiskEnd).
	// validate-only-when-set; omit from ERB when zero.
	DiskVMIDRangeEnd int `json:"disk_vmid_range_end,omitempty"`

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

	// RegistryAllowInsecure relaxes the default https:// requirement on the
	// registry endpoint. When false (default), Validate rejects any non-https
	// scheme so credentials are never transmitted in cleartext. When true and
	// the endpoint is http://, Validate emits a single warning log and proceeds.
	RegistryAllowInsecure bool `json:"registry_allow_insecure,omitempty"`

	// RegistryCACertPEM is an optional PEM-encoded CA certificate (or chain)
	// appended to the host's system trust pool when building the registry
	// HTTP client. Use when the registry presents a certificate signed by a
	// private CA the host trust store does not already include. Ignored when
	// empty (system trust pool used unmodified).
	RegistryCACertPEM string `json:"registry_ca_cert,omitempty"`

	// RegistryAllowedHosts is an optional list of host patterns that restrict
	// which hosts the registry HTTP client is permitted to contact. Each entry
	// is either an exact host (e.g. "registry.example.com") or a wildcard
	// prefix pattern (e.g. "*.example.com"). When non-empty, the registry
	// client rejects any request whose resolved host does not match at least
	// one entry. Empty (default) disables host-allow-list filtering; the
	// configuredHost invariant and disabled redirects still apply regardless.
	// Defense-in-depth against SSRF via host mutation.
	RegistryAllowedHosts []string `json:"registry_allowed_hosts,omitempty" yaml:"registry_allowed_hosts,omitempty"`

	// RegistryAllowPrivateIP disables the private/loopback IP rejection guard
	// on the registry endpoint when true. Default nil (treated as false): the
	// registry client rejects endpoints whose IP address is private (RFC1918),
	// loopback (127/8, ::1), link-local (169.254/16, fe80::/10), or unspecified
	// (0.0.0.0, ::). Set to true only for lab/test deployments where the
	// registry is intentionally on a private network (e.g. 192.168.x.x).
	// Pointer-typed so nil (field absent from JSON) is distinguishable from an
	// explicit false. Use RegistryAllowPrivateIPValue() to obtain the
	// effective bool. Validate-only-when-set; omit from ERB output when nil.
	RegistryAllowPrivateIP *bool `json:"registry_allow_private_ip,omitempty"`

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

	// FetchCredentialDefaults holds URL-prefix → auth-payload mappings used by
	// the stemcell_fetch package when no per-stemcell auth is provided in
	// cloud_properties. The longest URLPrefix match wins. Empty slice (default)
	// means all stemcell fetches are unauthenticated unless
	// cloud_properties.image_url_auth supplies per-stemcell credentials.
	FetchCredentialDefaults []FetchCredentialDefault `json:"fetch_credential_defaults,omitempty"`

	// StemcellFetchDialTimeoutSec bounds the TCP dial step of every
	// stemcell-fetch HTTP request (https and bosh+blobstore sources). 0 (default)
	// applies the built-in 30s default. Valid range: 1-3600 seconds.
	StemcellFetchDialTimeoutSec int `json:"stemcell_fetch_dial_timeout_sec,omitempty"`

	// StemcellFetchTLSHandshakeTimeoutSec bounds the TLS handshake step of every
	// stemcell-fetch HTTPS request. 0 (default) applies the built-in 15s default.
	// Valid range: 1-3600 seconds.
	StemcellFetchTLSHandshakeTimeoutSec int `json:"stemcell_fetch_tls_handshake_timeout_sec,omitempty"`

	// StemcellFetchResponseHeaderTimeoutSec bounds the wait for the response
	// headers after a stemcell-fetch request is sent. Guards against slow-loris
	// drips on the response-header phase. 0 (default) applies the built-in 120s
	// default. Valid range: 1-3600 seconds.
	StemcellFetchResponseHeaderTimeoutSec int `json:"stemcell_fetch_response_header_timeout_sec,omitempty"`

	// StemcellFetchIdleConnTimeoutSec bounds how long an idle keep-alive
	// connection stays in the connection pool before being closed. 0 (default)
	// applies the built-in 90s default. Valid range: 1-3600 seconds.
	StemcellFetchIdleConnTimeoutSec int `json:"stemcell_fetch_idle_conn_timeout_sec,omitempty"`

	// StemcellTemplateVMIDRangeStart is the inclusive lower bound of the VMID
	// range for template VMs created by create_stemcell. Templates occupy a
	// dedicated band above the persistent-disk range so they never collide with
	// the VM range (VMIDRangeStart..VMIDRangeEnd) or the disk range 9000-29999.
	// ApplyDefaults sets to 30000 when 0 (mirrors pve.VMIDRangeTemplateStart).
	// validate-only-when-set; omit from ERB when zero.
	StemcellTemplateVMIDRangeStart int `json:"stemcell_template_vmid_range_start,omitempty"`

	// StemcellTemplateVMIDRangeEnd is the inclusive upper bound of the VMID
	// range for template VMs. Must be > StemcellTemplateVMIDRangeStart.
	// ApplyDefaults sets to 30999 when 0 (mirrors pve.VMIDRangeTemplateEnd).
	// validate-only-when-set; omit from ERB when zero.
	StemcellTemplateVMIDRangeEnd int `json:"stemcell_template_vmid_range_end,omitempty"`

	// StemcellTemplatePool is an optional PVE resource pool name to assign
	// newly created template VMs. Empty (default) means no pool assignment.
	// validate-only-when-set: any non-empty string is accepted; PVE validates
	// pool existence at assignment time.
	StemcellTemplatePool string `json:"stemcell_template_pool,omitempty"`

	// StemcellTemplateNode is the PVE node on which template VMs are created.
	// Empty (default) falls back to Node at the callsite. Useful when
	// stemcell_storage is on shared storage but template creation should be
	// pinned to one node. validate-only-when-set.
	StemcellTemplateNode string `json:"stemcell_template_node,omitempty"`

	// CloneMode controls the clone type used by create_vm when cloning a
	// stemcell template. Values: "auto" (default), "linked", "full".
	// "auto": linked clone for snapshot-capable backends (dir, nfs, cifs,
	// zfspool, lvmthin, rbd, cephfs); full clone for lvm-thick (linked
	// not supported). "linked": force linked clone; error on lvm-thick.
	// "full": force full clone on all backends.
	// ApplyDefaults treats empty string as "auto". omit from ERB when empty.
	CloneMode string `json:"clone_mode,omitempty"`
}

// FetchCredentialDefault maps a URL prefix to a JSON auth payload understood
// by stemcell_fetch.parseAuth. URLPrefix is compared against the raw image_url
// string; the entry with the longest matching prefix wins.
//
// Auth is a raw JSON object with a mandatory "type" field (basic|bearer|s3|oci).
// Example:
//
//	{"url_prefix":"https://harbor.corp/stemcells/","auth":{"type":"basic","username":"robot","password":"s3cr3t"}}
type FetchCredentialDefault struct {
	URLPrefix string          `json:"url_prefix"`
	Auth      json.RawMessage `json:"auth"`
}

// knownConfigFields is the set of JSON field names declared on CPIConfig.
// Built once at init time via reflection so it stays in sync with struct changes.
var knownConfigFields = func() map[string]struct{} {
	t := reflect.TypeOf(CPIConfig{})
	m := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// Strip options like ",omitempty".
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			m[name] = struct{}{}
		}
	}
	return m
}()

// insertionSort sorts a string slice in place.
func insertionSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// warnUnknownFields decodes raw into a flat map, finds keys absent from
// knownConfigFields, and emits a single Warn entry listing them.
// Uses a stderr logger so the warning surfaces even before the application
// logger is fully initialized. Unknown fields are ignored, not rejected, to
// preserve forward-compatibility when the director sends fields added by
// future CPI versions.
func warnUnknownFields(raw []byte) {
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		return // malformed JSON is handled by the main decode path
	}
	var unknown []string
	for k := range flat {
		if _, known := knownConfigFields[k]; !known {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return
	}
	insertionSort(unknown)
	logger, err := log.NewLogger("warn", os.Stderr)
	if err != nil {
		return
	}
	logger.Warn("config: unknown fields ignored (forward-compat)",
		log.String("fields", strings.Join(unknown, ", ")),
	)
}

// MaxConfigBytes caps the CPI configuration JSON at 1 MiB. Realistic BOSH
// CPI configs are a few KiB at most; this is defense-in-depth against a
// malformed or attacker-controlled config that would otherwise drive an
// unbounded io.ReadAll allocation.
const MaxConfigBytes = 1 << 20

// Load decodes CPIConfig from r, applies defaults, then validates.
// Unknown JSON fields are logged at Warn level and ignored to preserve
// forward-compatibility with future BOSH director versions.
// Returns a CloudError on read failure, decode failure, validation failure,
// or when the input exceeds MaxConfigBytes.
func Load(r io.Reader) (*CPIConfig, error) {
	// Buffer the input so we can decode twice: once into a raw map for unknown-
	// field detection, then into CPIConfig for the actual value population.
	// Read one extra byte to distinguish "exactly at the cap" from "exceeded the cap".
	raw, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil {
		return nil, cpierrors.Cloud("config: read: %s", err.Error())
	}
	if int64(len(raw)) > MaxConfigBytes {
		return nil, cpierrors.Cloud("config: input exceeds %d bytes", MaxConfigBytes)
	}
	warnUnknownFields(raw)

	var cfg CPIConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
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
	f, err := os.Open(path) // #nosec G304 -- path is operator-supplied CLI arg; trust boundary
	if err != nil {
		return nil, cpierrors.Cloud("config: open %s: %s", path, err.Error())
	}
	defer func() { _ = f.Close() }()
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
		c.VMIDRangeEnd = 8999
	}
	// Persistent-disk VMID range. config cannot import internal/pve (cycle), so
	// the constants are inlined with comments referencing pve.VMIDRangeDisk*.
	if c.DiskVMIDRangeStart == 0 {
		c.DiskVMIDRangeStart = 9000 // pve.VMIDRangeDiskStart
	}
	if c.DiskVMIDRangeEnd == 0 {
		c.DiskVMIDRangeEnd = 29999 // pve.VMIDRangeDiskEnd
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
	if c.StemcellFetchDialTimeoutSec <= 0 {
		c.StemcellFetchDialTimeoutSec = 30
	}
	if c.StemcellFetchTLSHandshakeTimeoutSec <= 0 {
		c.StemcellFetchTLSHandshakeTimeoutSec = 15
	}
	if c.StemcellFetchResponseHeaderTimeoutSec <= 0 {
		c.StemcellFetchResponseHeaderTimeoutSec = 120
	}
	if c.StemcellFetchIdleConnTimeoutSec <= 0 {
		c.StemcellFetchIdleConnTimeoutSec = 90
	}
	// Template VMID range defaults to a dedicated band above the persistent-disk
	// range (30000-30999). Because this band sits above the disk range, it cannot
	// collide with any VM range up to the disk floor, so no adaptive derivation is
	// needed. config cannot import internal/pve (cycle), so the constants are
	// inlined with comments referencing pve.VMIDRangeTemplateStart/End.
	if c.StemcellTemplateVMIDRangeStart == 0 {
		c.StemcellTemplateVMIDRangeStart = 30000 // pve.VMIDRangeTemplateStart
	}
	if c.StemcellTemplateVMIDRangeEnd == 0 {
		c.StemcellTemplateVMIDRangeEnd = 30999 // pve.VMIDRangeTemplateEnd
	}
	if c.CloneMode == "" {
		c.CloneMode = "auto"
	}
	if c.NetworkMode == "" {
		c.NetworkMode = "auto"
	}
	if c.SDNZoneType == "" {
		c.SDNZoneType = "simple"
	}
	// Authentication precedence: when both password and api_token are present
	// (e.g. a kit renders a placeholder password alongside a real api_token
	// because credhub entombment rejects empty values), the api_token wins —
	// clear the password so downstream code uses tokens.
	//
	// This mutation lives in ApplyDefaults rather than Validate because
	// Validate's contract is read-only: callers may invoke Validate repeatedly
	// for diagnostics without side effects. Load() runs ApplyDefaults before
	// Validate, so the merged-credential path is normalized before the
	// validation pass observes it.
	if c.Password != "" && c.APIToken != "" {
		c.Password = ""
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

// RegistryAllowPrivateIPValue returns the effective allow-private-IP toggle.
// nil (field absent from JSON) → false (guard active, private IPs rejected).
// *true  → true (guard disabled, private IPs permitted — lab/test only).
// *false → false (guard active, identical to nil but explicit).
func (c *CPIConfig) RegistryAllowPrivateIPValue() bool {
	if c.RegistryAllowPrivateIP == nil {
		return false
	}
	return *c.RegistryAllowPrivateIP
}

// Validate checks all required fields and enum constraints.
// Returns a CloudError whose message lists every violation, separated by "; ".
//
// Validate may emit warning log entries (registry_allow_insecure and
// registry_allow_private_ip opt-in paths). Warnings are written to a
// stderr-backed slog logger; callers who need to capture them for assertions
// should use ValidateWithLogger instead.
func (c *CPIConfig) Validate() error {
	return c.ValidateWithLogger(nil)
}

// ValidateWithLogger is identical to Validate, but routes any warning entries
// to logger instead of the default stderr fallback. A nil logger uses the
// default fallback (matching the legacy Validate behavior).
func (c *CPIConfig) ValidateWithLogger(logger *log.Logger) error {
	var errs []string
	c.validateRequiredFields(&errs)
	c.validateAuth(&errs)
	c.validateEnumFields(&errs)
	c.validateRanges(&errs)
	c.validateRegistryConfig(&errs, logger)
	if len(errs) > 0 {
		return cpierrors.Cloud("config validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// validateRequiredFields appends an error for each required scalar field that
// is absent. Covers host, user, vm_storage, disk_storage, and network_bridge.
func (c *CPIConfig) validateRequiredFields(errs *[]string) {
	if c.Host == "" {
		*errs = append(*errs, "host is required")
	}
	if c.User == "" {
		*errs = append(*errs, "user is required")
	}
	if c.VMStorage == "" {
		*errs = append(*errs, "vm_storage is required")
	}
	if c.DiskStorage == "" {
		*errs = append(*errs, "disk_storage is required")
	}
	if c.NetworkBridge == "" {
		*errs = append(*errs, "network_bridge is required")
	}
}

// validateAuth appends an error when neither password nor api_token is set.
// ApplyDefaults normalizes the "both supplied" case (token wins, password
// cleared) so Validate sees the merged result here without mutating c.
func (c *CPIConfig) validateAuth(errs *[]string) {
	if c.Password == "" && c.APIToken == "" {
		*errs = append(*errs, "one of password or api_token is required")
	}
}

// validateEnumFields appends an error for each enum-bounded field whose value
// is not in the declared set. Covers agent_mode, vm_disk_format, log_level,
// network_mode, sdn_zone_type (conditional on network_mode), reboot_mode,
// stemcell_staging_dir path constraints, and the pve_ca_cert PEM check.
func (c *CPIConfig) validateEnumFields(errs *[]string) {
	// AgentMode enum.
	switch c.AgentMode {
	case "cloudinit", "registry", "noagent":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"agent_mode must be one of cloudinit|registry|noagent, got %q", c.AgentMode,
		))
	}

	// VMDiskFormat enum.
	switch c.VMDiskFormat {
	case "qcow2", "raw", "vmdk":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"vm_disk_format must be one of qcow2|raw|vmdk, got %q", c.VMDiskFormat,
		))
	}

	// LogLevel enum.
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"log_level must be one of debug|info|warn|error, got %q", c.LogLevel,
		))
	}

	// RebootMode enum.
	switch c.RebootMode {
	case "soft", "hard":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			`reboot_mode must be one of soft|hard, got %q`, c.RebootMode,
		))
	}

	// NetworkMode enum.
	switch c.NetworkMode {
	case "sdn", "bridge", "auto":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"network_mode must be one of sdn|bridge|auto, got %q", c.NetworkMode,
		))
	}

	// SDNZoneType enum — only validated when the SDN path is reachable.
	if c.NetworkMode == "sdn" || c.NetworkMode == "auto" {
		switch c.SDNZoneType {
		case "simple", "vlan", "qinq", "vxlan", "evpn":
			// valid
		default:
			*errs = append(*errs, fmt.Sprintf(
				"sdn_zone_type must be one of simple|vlan|qinq|vxlan|evpn, got %q", c.SDNZoneType,
			))
		}
	}

	// CloneMode enum: validate only when non-empty (ApplyDefaults sets "auto" when absent;
	// "auto" and "linked" and "full" are the only valid values).
	if c.CloneMode != "" {
		switch c.CloneMode {
		case "auto", "linked", "full":
			// valid
		default:
			*errs = append(*errs, fmt.Sprintf(
				"clone_mode must be one of auto|linked|full, got %q", c.CloneMode,
			))
		}
	}

	// StemcellStagingDir: when set, must be an absolute path to an existing directory.
	if c.StemcellStagingDir != "" {
		if !strings.HasPrefix(c.StemcellStagingDir, "/") {
			*errs = append(*errs, fmt.Sprintf(
				"stemcell_staging_dir %q must be an absolute path", c.StemcellStagingDir))
		} else {
			fi, statErr := os.Stat(c.StemcellStagingDir)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					*errs = append(*errs, fmt.Sprintf(
						"stemcell_staging_dir %q does not exist", c.StemcellStagingDir))
				} else {
					*errs = append(*errs, fmt.Sprintf(
						"stemcell_staging_dir %q cannot be stat'd: %s", c.StemcellStagingDir, statErr.Error()))
				}
			} else if !fi.IsDir() {
				*errs = append(*errs, fmt.Sprintf(
					"stemcell_staging_dir %q is not a directory", c.StemcellStagingDir))
			}
		}
	}

	// PVECACertPEM: when non-empty AND verify_ssl=true, the PEM must parse to at
	// least one valid certificate. Malformed PEM at startup is rejected so the
	// operator learns immediately rather than encountering TLS errors at runtime.
	// When verify_ssl=false the CA cert is ignored (insecure-skip-verify wins).
	if c.PVECACertPEM != "" && c.VerifySSLValue() {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.PVECACertPEM)) {
			*errs = append(*errs, "pve_ca_cert: no valid PEM certificates parsed from value")
		}
	}
}

// rangesOverlap reports whether [s1,e1] and [s2,e2] overlap (inclusive on both ends).
func rangesOverlap(s1, e1, s2, e2 int) bool {
	return s1 <= e2 && s2 <= e1
}

// validateRanges appends an error for each numeric field outside its valid range.
// Covers port (1–65535), reboot_timeout (1–3600 s), the three VMID allocation
// bands (delegated to validateVMIDBands), and the stemcell-fetch timeouts.
func (c *CPIConfig) validateRanges(errs *[]string) {
	// Port range.
	if c.Port <= 0 || c.Port >= 65536 {
		*errs = append(*errs, fmt.Sprintf("port must be 1–65535, got %d", c.Port))
	}

	// RebootTimeout range: 1–3600 seconds.
	if c.RebootTimeout < 1 || c.RebootTimeout > 3600 {
		*errs = append(*errs, fmt.Sprintf(
			"reboot_timeout must be 1-3600 seconds, got %d", c.RebootTimeout,
		))
	}

	c.validateVMIDBands(errs)

	// Stemcell-fetch transport timeouts: 1–3600 seconds when ApplyDefaults has
	// run. The fields are int seconds so the JSON shape stays human-friendly;
	// the conversion to time.Duration happens at the call site.
	c.appendStemcellFetchTimeoutErrors(errs)
}

// Default VMID band bounds, inlined because config cannot import internal/pve
// (cycle). They mirror pve.VMIDRangeDiskStart/End and pve.VMIDRangeTemplate*.
const (
	defaultDiskVMIDStart     = 9000
	defaultDiskVMIDEnd       = 29999
	defaultTemplateVMIDStart = 30000
	defaultTemplateVMIDEnd   = 30999
)

// validateVMIDBands validates the three VMID allocation bands (VM, persistent
// disk, stemcell template) and rejects any pairwise overlap.
//
// The VM range is always validated from its raw fields (ApplyDefaults fills it;
// tests set it explicitly). The disk and template ranges are validated only
// when explicitly set — both-zero means the caller skipped ApplyDefaults, so
// the runtime defaults apply and there is nothing operator-supplied to reject.
//
// Overlap checks use *effective* ranges: an unset disk or template range falls
// back to its default so the VM range is always cross-checked against the bands
// that will exist at runtime. There is no hard VM-range ceiling — the VM range
// may grow as far as the operator wants, provided it does not collide with the
// (possibly relocated) disk or template band.
func (c *CPIConfig) validateVMIDBands(errs *[]string) {
	const maxVMID = 999999999 // PVE maximum VMID

	checkBounds := func(name string, start, end int) {
		if start < 100 {
			*errs = append(*errs, fmt.Sprintf(
				"%s_start must be ≥100 (PVE reserved range), got %d", name, start))
		}
		if end > maxVMID {
			*errs = append(*errs, fmt.Sprintf(
				"%s_end must be ≤%d (PVE maximum VMID), got %d", name, maxVMID, end))
		}
		if end <= start {
			*errs = append(*errs, fmt.Sprintf(
				"%s_end must be > %s_start (%d), got %d", name, name, start, end))
		}
	}

	// VM range: always validated from raw fields.
	checkBounds("vmid_range", c.VMIDRangeStart, c.VMIDRangeEnd)

	// Disk and template ranges: bounds-checked only when explicitly set.
	if c.DiskVMIDRangeStart != 0 || c.DiskVMIDRangeEnd != 0 {
		checkBounds("disk_vmid_range", c.DiskVMIDRangeStart, c.DiskVMIDRangeEnd)
	}
	if c.StemcellTemplateVMIDRangeStart != 0 || c.StemcellTemplateVMIDRangeEnd != 0 {
		checkBounds("stemcell_template_vmid_range", c.StemcellTemplateVMIDRangeStart, c.StemcellTemplateVMIDRangeEnd)
	}

	// Effective ranges for overlap checks (unset disk/template → defaults).
	diskStart, diskEnd := c.DiskVMIDRangeStart, c.DiskVMIDRangeEnd
	if diskStart == 0 && diskEnd == 0 {
		diskStart, diskEnd = defaultDiskVMIDStart, defaultDiskVMIDEnd
	}
	tStart, tEnd := c.StemcellTemplateVMIDRangeStart, c.StemcellTemplateVMIDRangeEnd
	if tStart == 0 && tEnd == 0 {
		tStart, tEnd = defaultTemplateVMIDStart, defaultTemplateVMIDEnd
	}

	// Pairwise overlaps — only when each range is well-formed, so a bounds
	// error above does not spawn a confusing second overlap error.
	vmOK := c.VMIDRangeStart < c.VMIDRangeEnd
	diskOK := diskStart < diskEnd
	tOK := tStart < tEnd
	if vmOK && diskOK && rangesOverlap(c.VMIDRangeStart, c.VMIDRangeEnd, diskStart, diskEnd) {
		*errs = append(*errs, fmt.Sprintf(
			"persistent disk VMID range [%d,%d] overlaps VM VMID range [%d,%d]",
			diskStart, diskEnd, c.VMIDRangeStart, c.VMIDRangeEnd))
	}
	if vmOK && tOK && rangesOverlap(c.VMIDRangeStart, c.VMIDRangeEnd, tStart, tEnd) {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell template VMID range [%d,%d] overlaps VM VMID range [%d,%d]",
			tStart, tEnd, c.VMIDRangeStart, c.VMIDRangeEnd))
	}
	if diskOK && tOK && rangesOverlap(diskStart, diskEnd, tStart, tEnd) {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell template VMID range [%d,%d] overlaps persistent disk range [%d,%d]",
			tStart, tEnd, diskStart, diskEnd))
	}
}

// appendStemcellFetchTimeoutErrors validates each stemcell-fetch transport
// timeout is within 1–3600 seconds when explicitly set. Zero is accepted and
// is the signal for ApplyDefaults to fill in the built-in default. Negative
// values and values >3600 are rejected.
func (c *CPIConfig) appendStemcellFetchTimeoutErrors(errs *[]string) {
	check := func(name string, v int) {
		if v == 0 {
			return
		}
		if v < 1 || v > 3600 {
			*errs = append(*errs, fmt.Sprintf(
				"%s must be 1-3600 seconds when set, got %d", name, v,
			))
		}
	}
	check("stemcell_fetch_dial_timeout_sec", c.StemcellFetchDialTimeoutSec)
	check("stemcell_fetch_tls_handshake_timeout_sec", c.StemcellFetchTLSHandshakeTimeoutSec)
	check("stemcell_fetch_response_header_timeout_sec", c.StemcellFetchResponseHeaderTimeoutSec)
	check("stemcell_fetch_idle_conn_timeout_sec", c.StemcellFetchIdleConnTimeoutSec)
}

// validateRegistryConfig appends errors for registry-related constraints when
// agent_mode=registry. Checks endpoint presence, user presence, password presence,
// scheme guard (https required unless registry_allow_insecure=true), and
// registry_allowed_hosts format (host patterns only, no scheme or path).
func (c *CPIConfig) validateRegistryConfig(errs *[]string, logger *log.Logger) {
	if c.AgentMode != "registry" {
		return
	}

	if c.RegistryEndpoint == "" {
		*errs = append(*errs, "registry_endpoint is required when agent_mode=registry")
	}
	if c.RegistryUser == "" {
		*errs = append(*errs, "registry_user is required when agent_mode=registry")
	}
	if c.RegistryPassword == "" {
		*errs = append(*errs, "registry_password is required when agent_mode=registry")
	}

	// Scheme guard: refuse plaintext http:// (or any non-https scheme) unless
	// the operator has explicitly set registry_allow_insecure=true. Credentials
	// flow over this connection on every settings PUT/GET; default-deny matches
	// the verify_ssl=true default for the PVE connection.
	if c.RegistryEndpoint != "" {
		if msg := c.validateRegistryScheme(logger); msg != "" {
			*errs = append(*errs, msg)
		}
	}

	// registry_allow_private_ip opt-in warning: when the operator has explicitly
	// enabled the override, emit a single warning so the choice is visible in
	// logs. Nil and explicit false are silent (guard active is the safe default).
	if c.RegistryAllowPrivateIP != nil && *c.RegistryAllowPrivateIP {
		emitRegistryPrivateIPWarning(logger, c.RegistryEndpoint)
	}

	// registry_allowed_hosts: each entry must be a non-empty string with no
	// scheme or path component (host patterns only).
	for i, h := range c.RegistryAllowedHosts {
		if h == "" {
			*errs = append(*errs, fmt.Sprintf("registry_allowed_hosts[%d] must not be empty", i))
			continue
		}
		if strings.Contains(h, "://") {
			*errs = append(*errs, fmt.Sprintf(
				"registry_allowed_hosts[%d] %q must be a host pattern (no scheme; use e.g. \"host.example.com\" or \"*.example.com\")", i, h,
			))
			continue
		}
		if strings.Contains(h, "/") {
			*errs = append(*errs, fmt.Sprintf(
				"registry_allowed_hosts[%d] %q must be a host pattern (no path component)", i, h,
			))
		}
	}
}

// validateRegistryScheme parses RegistryEndpoint and applies the scheme guard.
// Returns an empty string on success, or a violation message suitable for
// appending to the validation error list. When the opt-in flag is set and the
// scheme is http://, a single warning is emitted to logger (or to a stderr
// fallback when logger is nil).
func (c *CPIConfig) validateRegistryScheme(logger *log.Logger) string {
	u, err := url.Parse(c.RegistryEndpoint)
	if err != nil {
		return fmt.Sprintf("registry_endpoint %q is not a valid URL: %s", c.RegistryEndpoint, err.Error())
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return fmt.Sprintf(
			"registry_endpoint %q is missing a scheme; expected https:// (or http:// with registry_allow_insecure=true)",
			c.RegistryEndpoint,
		)
	}
	if scheme == "https" {
		return ""
	}
	if !c.RegistryAllowInsecure {
		return fmt.Sprintf(
			"registry_endpoint scheme must be https (got %q); set registry_allow_insecure=true to permit plaintext",
			scheme,
		)
	}
	// Opt-in to insecure transport: only http:// is supported as the cleartext
	// alternative. Anything else (e.g. ftp, gopher) is rejected outright.
	if scheme != "http" {
		return fmt.Sprintf(
			"registry_endpoint scheme %q is not supported even with registry_allow_insecure=true (use https:// or http://)",
			scheme,
		)
	}
	emitRegistryInsecureWarning(logger, c.RegistryEndpoint)
	return ""
}

// emitRegistryInsecureWarning logs the opt-in plaintext warning to logger.
// When logger is nil it builds a stderr-backed warn-level logger (matching the
// pattern used by warnUnknownFields) so the warning surfaces even when Validate
// is invoked before the application logger is constructed.
func emitRegistryInsecureWarning(logger *log.Logger, endpoint string) {
	target := logger
	if target == nil {
		fallback, err := log.NewLogger("warn", os.Stderr)
		if err != nil {
			return
		}
		target = fallback
	}
	target.Warn(
		"registry_allow_insecure=true; transmitting credentials over cleartext http",
		log.String("endpoint", redactEndpoint(endpoint)),
	)
}

// emitRegistryPrivateIPWarning logs the allow-private-ip opt-in warning to
// logger. When logger is nil it builds a stderr-backed warn-level logger
// (matching the pattern used by emitRegistryInsecureWarning) so the warning
// surfaces even when Validate is invoked before the application logger is
// constructed.
func emitRegistryPrivateIPWarning(logger *log.Logger, endpoint string) {
	target := logger
	if target == nil {
		fallback, err := log.NewLogger("warn", os.Stderr)
		if err != nil {
			return
		}
		target = fallback
	}
	target.Warn(
		"registry_allow_private_ip=true; private/loopback IP check disabled for registry endpoint",
		log.String("endpoint", redactEndpoint(endpoint)),
	)
}

// redactEndpoint strips userinfo from a URL so the endpoint can be logged
// without leaking embedded credentials. A parse failure returns the original
// string unchanged so the operator still sees something useful in logs.
func redactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	if u.User != nil {
		u.User = url.UserPassword("REDACTED", "REDACTED")
	}
	return u.String()
}
