package config

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/hooks"
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

	// StemcellReplicateLocal enables per-node template replication when stemcell
	// storage is node-local (dir, lvm, lvmthin) and the cluster has more than one
	// node. When true, create_stemcell builds a template VM on every candidate
	// cluster node by uploading the qcow2 independently to each node's local
	// storage, then calling ensureTemplateVM per node. Each replica carries the
	// tag "bosh-stemcell-node-<node>" in addition to the shared content tag
	// "bosh-stemcell-sha-<sha8>". delete_stemcell removes all replicas across all
	// cluster nodes (best-effort; a single-node failure is logged and skipped
	// rather than aborting).
	//
	// Default false. When false, the existing behavior applies: local storage on a
	// multi-node cluster is rejected at create_stemcell time with a clear error
	// directing the operator to use shared storage. Setting this to true opts the
	// operator into the replication strategy as an alternative to shared storage.
	// validate-only-when-set; omit from ERB when false.
	StemcellReplicateLocal bool `json:"stemcell_replicate_local,omitempty"`

	// CloneMode controls the clone type used by create_vm when cloning a
	// stemcell template. Values: "auto" (default), "linked", "full".
	// "auto": linked clone for snapshot-capable backends (dir, nfs, cifs,
	// zfspool, lvmthin, rbd, cephfs); full clone for lvm-thick (linked
	// not supported). "linked": force linked clone; error on lvm-thick.
	// "full": force full clone on all backends.
	// ApplyDefaults treats empty string as "auto". omit from ERB when empty.
	CloneMode string `json:"clone_mode,omitempty"`

	// Placement is an optional nested block that controls availability-aware
	// node selection and anti-affinity at create_vm time. When nil (field absent
	// from JSON), all placement behavior defaults to safe defaults via accessors.
	// When present, sub-fields follow the pointer-optional pattern: nil = absent
	// = use default; explicit value overrides.
	Placement *PlacementConfig `json:"placement,omitempty"`

	// EnsureNoIPConflicts, when true (default), causes create_vm to verify that
	// no existing VM on the candidate node already holds the same static IP before
	// provisioning. Prevents duplicate-IP collisions on static networks. Set
	// false only for dynamic (DHCP) networks where IP pre-assignment is not
	// meaningful. Pointer-typed so nil (absent from JSON) is distinguishable from
	// an explicit false. Use EnsureNoIPConflictsEnabled() to obtain the effective
	// bool. Validate-only-when-set; omit from ERB output when nil.
	EnsureNoIPConflicts *bool `json:"ensure_no_ip_conflicts,omitempty"`

	// IPConflictProbe selects the active IP-conflict probe mode. When empty or
	// "off" (default), no active probe runs and behavior is byte-identical to
	// prior releases. When "agent", create_vm additionally calls the QEMU guest
	// agent on each running VM to collect dynamically assigned IPs and checks
	// them against the target IPs before provisioning. This detects DHCP-assigned
	// addresses that the static-config scan (EnsureNoIPConflicts) cannot see.
	// The probe is fail-open: a guest agent error is logged at debug level and
	// that guest is skipped. Only valid when EnsureNoIPConflicts is also enabled
	// (or defaulting to enabled). Enum: ""|"off"|"agent". Validation rejects any
	// other value. Use IPConflictProbeMode() and ActiveIPProbeEnabled() for the
	// effective values.
	IPConflictProbe string `json:"ip_conflict_probe,omitempty"`

	// VMFirewall sets PVE's per-NIC firewall flag (firewall=1) on every NIC of
	// newly created VMs. When nil/false (default), NICs are created without the
	// flag — byte-identical to prior releases. A per-NIC network cloud property
	// (cloud_properties.firewall: true|false) overrides this global default for
	// that NIC. The NIC flag alone does not activate packet filtering: PVE also
	// requires the VM-level firewall to be enabled, which the security_groups
	// cloud property does. Pointer-typed so an explicit false survives JSON
	// omission. Use VMFirewallEnabled() for the effective bool. Omit from ERB
	// when nil; emit only when true.
	VMFirewall *bool `json:"vm_firewall,omitempty"`

	// Hooks names the built-in dispatch middleware hooks to activate, applied in
	// listed order. Each name must resolve in the hook registry
	// (internal/cpi/hooks; currently "audit_log"). When empty (default),
	// dispatch runs with no middleware — byte-identical to prior releases and
	// zero per-call overhead. Validated against the registry: an unknown name
	// fails config validation at startup. Use HooksValue() to read.
	Hooks []string `json:"hooks,omitempty"`

	// HealthCheck holds opt-in post-create VM health babysitting configuration.
	// When nil (the default), or when Enabled is absent or *false, no agent
	// ping is performed after the start task completes — behavior is byte-identical
	// to prior releases. When Enabled is *true, create_vm polls the guest QEMU
	// agent via POST /agent/ping until the agent answers or the deadline expires.
	// On failure, VM status diagnostics are folded into the error before the
	// standard rollback cleans up the VM.
	// Pointer-typed so a fully-absent block (nil) is cheap to detect. Accessors
	// handle nil safely; Validate runs only when the block is present and enabled.
	HealthCheck *HealthCheckConfig `json:"health_check,omitempty"`

	// Retry groups operator-tunable retry/backoff curves. Every sub-policy is
	// optional; an absent policy (or absent field within one) falls back to the
	// constant the CPI used before this block existed, so an unset retry block
	// is byte-identical behavior. Pointer-typed for cheap absence detection.
	Retry *RetryConfig `json:"retry,omitempty"`

	// OperationTimeout is the opt-in per-method deadline envelope. When nil, or
	// when Enabled is absent or *false, no context deadline wraps handler
	// dispatch — behavior is identical to prior releases. When Enabled is *true,
	// each handler runs under a context.WithTimeout sized by its method class so
	// a pathological retry/poll combination converts into a retriable timeout
	// the Director can act on rather than an un-cancellable hang holding a queue
	// slot. Pointer-typed so a fully-absent block is cheap to detect.
	OperationTimeout *OperationTimeoutConfig `json:"operation_timeout,omitempty"`

	// VMTypes maps operator-named VM-type profiles to default cloud_properties.
	// A create_vm call selects a profile via cloud_properties.vm_type="<name>".
	// Profile values sit BELOW per-call cloud_properties and ABOVE global config
	// in resolution precedence. Empty/absent = no profiles (today's behavior).
	VMTypes map[string]TypeProfile `json:"vm_types,omitempty"`

	// DiskTypes maps operator-named disk-type profiles to default cloud_properties.
	// Selected via cloud_properties.disk_type="<name>"; higher precedence than VMTypes.
	DiskTypes map[string]TypeProfile `json:"disk_types,omitempty"`

	// StorageTiers maps a tier label (e.g. "fast") to selection criteria matched
	// against live PVE /storage. cloud_properties.storage_tier="<name>" resolves to
	// a concrete pool when no explicit pool is set. Empty/absent = feature off.
	StorageTiers map[string]StorageTierCriteria `json:"storage_tiers,omitempty"`

	// SecurityGroups is an optional GLOBAL default list of PVE firewall groups
	// attached to every VM lacking a per-call/profile security_groups override.
	// Empty/absent = no global default (today's behavior).
	SecurityGroups []string `json:"security_groups,omitempty"`

	// DiskPerformance holds optional global per-disk performance option defaults.
	// Applied when a create_disk/create_vm cloud_properties (or vm_type/disk_type
	// profile) does not set the corresponding option. Pointer-typed so a nil block
	// (absent from JSON) emits nothing and preserves byte-identical behavior.
	// validate-only-when-set; omit from ERB when nil.
	DiskPerformance *DiskPerformanceDefaults `json:"disk_performance,omitempty"`

	// Stemcell holds optional stemcell provenance and orphan-pruning knobs.
	// Pointer-typed so a nil block (absent from JSON) emits nothing and preserves
	// byte-identical behavior. validate-only-when-set; omit from ERB when nil.
	// Distinct from the scalar StemcellStorage/StemcellTemplateNode/etc. fields
	// which remain untouched.
	Stemcell *StemcellProvenanceConfig `json:"stemcell,omitempty"`
}

// TypeProfile is a named bundle of default cloud_properties applied by the
// layered resolver. CloudProperties is free-form (BOSH passes a flat merged
// dict; the CPI cannot validate keys at load time), so no key validation here.
type TypeProfile struct {
	CloudProperties map[string]any `json:"cloud_properties,omitempty"`
}

// StorageTierCriteria selects PVE storages by attribute. Types lists allowed
// PVE storage type strings (lvm, lvmthin, zfspool, dir, nfs, cifs, rbd, cephfs,
// btrfs, glusterfs, pbs). Shared requires shared (true) / local (false) / any
// (nil). At least one of Types or Shared must be set.
type StorageTierCriteria struct {
	Types  []string `json:"types,omitempty"`
	Shared *bool    `json:"shared,omitempty"`
}

// DiskPerformanceDefaults holds optional global PVE per-disk performance option
// defaults applied when a create_disk/create_vm cloud_properties (or vm_type/
// disk_type profile) does not set the corresponding option. All fields optional;
// a nil block (default) emits nothing and preserves byte-identical behavior.
type DiskPerformanceDefaults struct {
	Iothread         *bool    `json:"iothread,omitempty"`
	Cache            string   `json:"cache,omitempty"`
	Discard          *bool    `json:"discard,omitempty"`
	SSD              *bool    `json:"ssd,omitempty"`
	MBpsRd           *float64 `json:"mbps_rd,omitempty"`
	MBpsWr           *float64 `json:"mbps_wr,omitempty"`
	IOPSRd           *int     `json:"iops_rd,omitempty"`
	IOPSWr           *int     `json:"iops_wr,omitempty"`
	VirtioSCSISingle *bool    `json:"virtio_scsi_single,omitempty"`
}

// StemcellProvenanceConfig holds optional stemcell provenance tracking and
// orphan-pruning knobs. All fields are optional; a nil block (default) emits
// nothing and preserves byte-identical behavior. Accessors on *CPIConfig handle
// nil blocks safely so callers never dereference this pointer directly.
type StemcellProvenanceConfig struct {
	// Provenance enables tagging stemcell templates with BOSH director metadata
	// so provenance can be verified at upload and delete time. Default false
	// (nil or absent → false via StemcellProvenanceEnabled accessor).
	Provenance *bool `json:"provenance,omitempty"`

	// DirectorID is an optional human-readable identifier for the BOSH director
	// that owns the stemcell templates. Used in provenance tags when Provenance
	// is enabled. Empty string is valid (feature off). Must contain at least one
	// alphanumeric or hyphen character when non-empty.
	DirectorID string `json:"director_id,omitempty"`

	// PruneOrphans enables pruning of stemcell templates whose associated
	// director is no longer active. Requires Provenance=true to be meaningful.
	// Default false (nil or absent → false via StemcellOrphanPruneEnabled accessor).
	PruneOrphans *bool `json:"prune_orphans,omitempty"`

	// PruneDryRun controls whether orphan pruning logs but does not delete.
	// When true, pruning actions are logged without performing any deletions.
	// Default false (nil or absent → false via StemcellOrphanPruneDryRun accessor).
	PruneDryRun *bool `json:"prune_dry_run,omitempty"`
}

// RetryConfig holds the operator-tunable retry/backoff policies. Each field is
// optional and behavior-preserving when unset (the accessors return the
// constants the CPI shipped with). Only policies that map to an existing
// backoff seam are exposed — there is deliberately no generic "default" policy
// because there is no generic curve to attach it to.
type RetryConfig struct {
	// StorageImport governs the exponential backoff used between create_vm /
	// create_disk allocation attempts when PVE is serialising imports under a
	// storage lock. Defaults: max_attempts per-handler (vm 10 / disk pkg
	// default), base_ms 2000, cap_ms 30000, jitter_pct 30.
	StorageImport *RetryPolicy `json:"storage_import,omitempty"`

	// VMIDAlloc governs the brief uniform jitter between VMID-conflict retries
	// and the allocation attempt budget. Defaults: max_attempts falls back to
	// vmid_alloc_attempts then the per-handler default, base_ms 50, cap_ms 250,
	// jitter_pct unused (uniform in [base_ms,cap_ms]).
	VMIDAlloc *RetryPolicy `json:"vmid_alloc,omitempty"`

	// TaskPoll governs PVE task polling cadence in AwaitTask. base_ms is the
	// poll interval, cap_ms the maximum interval, jitter_pct the per-poll
	// jitter. max_attempts is not applicable (polling is wall-clock bounded by
	// WithMaxWait and the operation-timeout envelope) and is ignored. Defaults:
	// base_ms 2000, cap_ms 10000, jitter_pct 10.
	TaskPoll *RetryPolicy `json:"task_poll,omitempty"`
}

// RetryPolicy is the parameter bag shared by every retry class. Each consuming
// curve interprets the fields in the way documented on its RetryConfig field;
// not every field is meaningful for every class. Zero in any field means "use
// the built-in default for this class" — never "zero attempts" or "zero delay"
// — so a partially-specified policy still composes with the shipped constants.
type RetryPolicy struct {
	// MaxAttempts caps the number of attempts. Zero means "use the class
	// default". Ignored by TaskPoll.
	MaxAttempts int `json:"max_attempts,omitempty"`

	// BaseMs is the base delay (StorageImport/VMIDAlloc) or poll interval
	// (TaskPoll) in milliseconds. Zero means "use the class default".
	BaseMs int `json:"base_ms,omitempty"`

	// CapMs is the maximum delay (StorageImport/VMIDAlloc) or maximum poll
	// interval (TaskPoll) in milliseconds. Zero means "use the class default".
	CapMs int `json:"cap_ms,omitempty"`

	// JitterPct is the +/- jitter percentage applied to the delay. Zero means
	// "use the class default". Unused by VMIDAlloc (uniform draw).
	JitterPct int `json:"jitter_pct,omitempty"`
}

// OperationTimeoutConfig holds the opt-in per-method deadline envelope. All
// fields are honored only when Enabled is *true (validate-only-when-set). An
// absent block (nil pointer) is equivalent to Enabled=*false and adds zero
// overhead to dispatch.
type OperationTimeoutConfig struct {
	// Enabled turns on the per-method context deadline. Default false (opt-in);
	// nil or absent JSON key is treated as false by OperationTimeoutEnabled().
	Enabled *bool `json:"enabled,omitempty"`

	// CreateSec is the deadline for create_* methods. Zero maps to 1800 s.
	CreateSec int `json:"create_sec,omitempty"`

	// DeleteSec is the deadline for delete_* methods. Zero maps to 900 s.
	DeleteSec int `json:"delete_sec,omitempty"`

	// QuerySec is the deadline for read-only methods (info, has_*, get_disks,
	// calculate_vm_cloud_properties). Zero maps to 120 s.
	QuerySec int `json:"query_sec,omitempty"`

	// DefaultSec is the deadline for every other mutating method (reboot_vm,
	// attach/detach/resize/snapshot_disk, set_*_metadata, update_disk). Zero
	// maps to 600 s.
	DefaultSec int `json:"default_sec,omitempty"`
}

// PlacementConfig holds all availability-aware node-selection knobs.
// The struct is embedded as a pointer in CPIConfig so that a fully-absent
// placement block (nil) is cheap to detect and preserves zero-regression for
// existing configurations.
type PlacementConfig struct {
	// Enabled controls whether live node scoring runs at create_vm time.
	// Default true (fully protective). When false, create_vm falls
	// back to Config.Node (existing behavior). Pointer-typed so nil is
	// treated as the default rather than explicit false.
	Enabled *bool `json:"enabled,omitempty"`

	// Weights tunes the node-scoring formula. When nil, ApplyDefaults
	// fills each axis with a sane default. Individual nil sub-fields also
	// defer to defaults. Pointer-typed so an absent weights block never
	// forces materialization of the parent Placement block.
	Weights *PlacementWeights `json:"weights,omitempty"`

	// AZMap maps availability-zone names to the list of PVE node names that
	// serve that AZ. When create_vm receives a cloud_properties.availability_zone
	// that is present in this map, the candidate set is restricted to those
	// nodes. An AZ name present in cloud_properties but absent from the map
	// yields a CloudError (explicit misconfiguration, not silent fall-through).
	// When empty or nil, all online nodes are candidates (default, zero regression).
	AZMap map[string][]string `json:"az_map,omitempty"`

	// AntiAffinity controls same-instance-group spreading. Pointer-typed so a
	// fully-absent block (nil) means "off" with zero regression. When present
	// and Enabled, the node scorer penalizes nodes already hosting members of
	// the VM's BOSH instance group (scheduler-soft spreading). UseHaRules adds
	// PVE-enforced negative HA affinity rules on top (opt-in within opt-in).
	AntiAffinity *AntiAffinityConfig `json:"anti_affinity,omitempty"`

	// DLB holds the PVE 9.2+ Dynamic Load Balancer integration knobs. Pointer-
	// typed so a fully-absent block (nil) means DLB is off with zero regression
	// for existing configurations. Use DLBExplicitlyEnabled(), DLBAZName(), and
	// related accessors rather than reading this pointer directly.
	DLB *DLBConfig `json:"dlb,omitempty"`

	// ExcludeMaintenanceNodes controls whether nodes in a PVE HA maintenance or
	// error state are excluded from placement candidates at create_vm time.
	// Default true (protective): when nil or absent, the accessor returns true so
	// VMs are not placed on degraded nodes unless the operator explicitly opts out.
	// Set to false only when the HA API is unavailable or when placing on
	// maintenance nodes is intentional.
	ExcludeMaintenanceNodes *bool `json:"exclude_maintenance_nodes,omitempty"`

	// MaintenanceNodeTags is the list of PVE node tags that indicate a node is in
	// maintenance and should be excluded from placement. A node carrying any tag in
	// this list is excluded when ExcludeMaintenanceNodes is true (or defaults to
	// true). Default ["maintenance"]. Empty slice disables tag-based detection
	// while still honoring HA-status-based detection.
	MaintenanceNodeTags []string `json:"maintenance_node_tags,omitempty"`

	// AZFallbackOrder is an ordered list of AZ names appended as fallback after
	// cloud_properties.availability_zones are exhausted. Useful for a cluster-wide
	// "prefer AZ-a then AZ-b" policy without per-VM cloud_properties. Empty (default)
	// means no config-level fallback chain; placement uses only AZ candidates from
	// cloud_properties.
	AZFallbackOrder []string `json:"az_fallback_order,omitempty"`

	// AZShuffle, when true, randomizes the AZ order within availability_zones before
	// scoring, breaking ordering ties with a random draw. Default false
	// (deterministic: preference order preserved). Pointer-typed so nil (absent) is
	// distinguishable from explicit false. Use AZShuffleEnabled() for the effective
	// bool.
	AZShuffle *bool `json:"az_shuffle,omitempty"`
}

// AntiAffinityConfig holds the Tier-2 same-group spreading knobs.
// Both fields are pointer-typed so nil and explicit false are both "off",
// and an absent sub-key defers to the documented default.
type AntiAffinityConfig struct {
	// Enabled turns on scheduler-soft spreading: the node scorer subtracts a
	// per-same-group-member penalty so members of one BOSH instance group are
	// distributed across nodes at create time. Default false (opt-in). This is
	// advisory only — under resource pressure two members may still co-locate.
	Enabled *bool `json:"enabled,omitempty"`

	// UseHaRules additionally registers each VM as a PVE HA resource and
	// maintains a cluster-level negative resource-affinity rule keyed on the
	// instance group, so PVE enforces spreading at the hypervisor level (not
	// just at create time). Default false. Has no effect unless Enabled is true.
	// Note: HA-managed resources interact with the BOSH resurrector; enable
	// only when PVE-level enforcement is desired.
	UseHaRules *bool `json:"use_ha_rules,omitempty"`

	// Strict controls whether the PVE HA negative resource-affinity rule is
	// set to strict mode. Default false (soft/advisory). When true, PVE refuses
	// to start or migrate a VM onto a node that already hosts another member of
	// the same HA group — this provides hard enforcement at the cost of reduced
	// scheduler flexibility. WARNING: strict=true on a small cluster (two or
	// three nodes) can prevent PVE from evacuating a faulting node because no
	// other node satisfies every strict negative rule simultaneously. Enable
	// only when the cluster is large enough to absorb the constraint. Has no
	// effect unless UseHaRules is also true.
	Strict *bool `json:"strict,omitempty"`
}

// DLBConfig holds the PVE 9.2+ Dynamic Load Balancer integration knobs.
// All fields are pointer-typed so nil means "use the documented default" and
// an absent placement.dlb block (nil DLB pointer on PlacementConfig) means
// DLB is fully off with zero regression on existing configurations.
// Defaults live in the accessor methods — ApplyDefaults does NOT materialize
// this block or any of its fields.
type DLBConfig struct {
	// Enabled is the master override that marks all VMs for DLB registration
	// regardless of their availability_zone. Default false (opt-in). When true,
	// every VM created by this CPI instance (except Director/bootstrap, which
	// use a separate cpi.json) is registered as a PVE HA resource with
	// auto-rebalance=1 so CRS/DLB places and continuously rebalances it.
	// Setting Enabled=true does not conflict with operator AZ topologies: the
	// CPI still restricts initial placement to the AZ candidate set when
	// AZMap is configured; DLB rebalances within the allowed nodes afterward.
	Enabled *bool `json:"enabled,omitempty"`

	// AZName is the sentinel availability-zone name that triggers DLB
	// registration for a VM. Default "dlb". When a BOSH cloud_properties block
	// sets availability_zone to this value, the CPI treats the VM as
	// DLB-delegated: it skips its own node-scoring AZ restriction (any online
	// node is a candidate) and registers the VM for DLB. Setting AZName to an
	// explicit empty string ("") disables the sentinel mechanism entirely so
	// only the master Enabled flag can trigger DLB. A nil pointer returns the
	// default "dlb"; a non-nil pointer to "" is honored as "disabled".
	AZName *string `json:"az_name,omitempty"`

	// ManageClusterCRS controls whether the CPI actively ensures the PVE
	// cluster CRS (Cluster Resource Scheduler) setting is configured for
	// dynamic load balancing. Default false. When false (default), the CPI
	// reads /cluster/options and logs a clear warning if crs is not set to
	// "ha=dynamic,...", directing the operator to run:
	//   pvesh set /cluster/options --crs ha=dynamic,...
	// When true, the CPI calls UpdateOptions to set crs=dynamic+auto-rebalance
	// automatically. Setting this to true writes a cluster-wide option that
	// affects all HA guests, so it is an explicit opt-in rather than the
	// default.
	ManageClusterCRS *bool `json:"manage_cluster_crs,omitempty"`

	// RequireSharedStorage guards DLB registration against VMs whose root disk
	// or persistent disk resides on node-local storage. Default true. When true,
	// a VM on local storage (dir, lvm, lvmthin, zfspool) is silently
	// skipped for DLB registration with a debug log entry — registering such a
	// VM for HA/DLB would cause PVE to attempt live-migration of a non-shared
	// disk, which fails. Set to false only when all VMs are guaranteed to use
	// shared storage (rbd, nfs, cifs, glusterfs, cephfs) and the storage type
	// cannot be determined from the PVE API at create time.
	RequireSharedStorage *bool `json:"require_shared_storage,omitempty"`
}

// HealthCheckConfig holds opt-in post-create VM health babysitting knobs.
// All fields are checked only when Enabled is *true; validate-only-when-set.
// An absent block (nil pointer on CPIConfig.HealthCheck) is equivalent to
// Enabled=*false and produces zero overhead at create_vm time.
type HealthCheckConfig struct {
	// Enabled turns on post-create agent ping polling. Default false (opt-in).
	// nil pointer or absent JSON key is treated as false by HealthCheckEnabled().
	Enabled *bool `json:"enabled,omitempty"`

	// TimeoutSec is the maximum time in seconds to wait for the guest agent to
	// respond before giving up, gathering diagnostics, and returning an error.
	// Must be 0–3600 when Enabled is true. Zero (absent or explicit 0) maps to
	// the built-in default of 300 s via HealthCheckTimeoutSec(). Negative values
	// are rejected. validate-only-when-set: ignored entirely when Enabled is
	// false or nil.
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// IntervalSec is the pause in seconds between successive agent ping attempts.
	// Must be 0–3600 when Enabled is true. Zero means no sleep between retries
	// (fast test mode). Default 5 s via HealthCheckIntervalSec().
	// validate-only-when-set.
	IntervalSec int `json:"interval_sec,omitempty"`
}

// PlacementWeights controls how each scoring axis contributes to the final
// per-node score at create_vm time. Higher weight = stronger preference.
// All fields are float64 ≥ 0; negative values are rejected by Validate.
// ApplyDefaults fills zero-value fields with documented sane defaults.
type PlacementWeights struct {
	// Mem weights free-memory headroom. Default 1.0 (highest priority axis:
	// prefer the node with the most available RAM).
	Mem float64 `json:"mem,omitempty"`
	// Storage weights free-storage fraction on the target pool. Default 0.5.
	Storage float64 `json:"storage,omitempty"`
	// CPU weights free-CPU headroom (max_cpu − used_cpu fraction). Default 0.5.
	CPU float64 `json:"cpu,omitempty"`
	// GuestCount weights inverse guest count (prefer emptier nodes). Default 0.3.
	GuestCount float64 `json:"guest_count,omitempty"`
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
	c.applyPlacementDefaults()
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

// applyPlacementDefaults fills zero-value Placement.Weights axes when the
// Placement block is present. Never materializes a nil Placement pointer.
// Extracted from ApplyDefaults to reduce its cognitive complexity.
func (c *CPIConfig) applyPlacementDefaults() {
	// Only when the block is present. Never materialize a nil Placement to
	// avoid phantom JSON output. Accessors handle nil → default.
	// EnsureNoIPConflicts: nil means absent → default true via accessor; no
	// field mutation needed (accessor handles nil safely).
	if c.Placement == nil {
		return
	}
	// Enabled: nil → default true via accessor; no mutation needed.
	// Weights: if block is present, fill any zero axes so consumers see
	// documented defaults even when inspecting Weights directly (not via
	// EffectiveWeights). Only fill when Weights block is present.
	if c.Placement.Weights == nil {
		return
	}
	if c.Placement.Weights.Mem == 0 {
		c.Placement.Weights.Mem = 1.0
	}
	if c.Placement.Weights.Storage == 0 {
		c.Placement.Weights.Storage = 0.5
	}
	if c.Placement.Weights.CPU == 0 {
		c.Placement.Weights.CPU = 0.5
	}
	if c.Placement.Weights.GuestCount == 0 {
		c.Placement.Weights.GuestCount = 0.3
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

// PlacementEnabled returns the effective placement-scoring toggle.
// nil Placement block OR nil Placement.Enabled → true (fully protective default).
// Explicit *false → false (operator opt-out).
func (c *CPIConfig) PlacementEnabled() bool {
	if c.Placement == nil || c.Placement.Enabled == nil {
		return true
	}
	return *c.Placement.Enabled
}

// EffectiveWeights returns PlacementWeights with all zero-value axes filled to
// their sane defaults. Callers always get a non-nil, fully-populated struct:
//   - Mem        → 1.0
//   - Storage    → 0.5
//   - CPU        → 0.5
//   - GuestCount → 0.3
//
// Explicit operator-supplied values are preserved (ApplyDefaults already ran).
func (c *CPIConfig) EffectiveWeights() PlacementWeights {
	var w PlacementWeights
	if c.Placement != nil && c.Placement.Weights != nil {
		w = *c.Placement.Weights
	}
	if w.Mem == 0 {
		w.Mem = 1.0
	}
	if w.Storage == 0 {
		w.Storage = 0.5
	}
	if w.CPU == 0 {
		w.CPU = 0.5
	}
	if w.GuestCount == 0 {
		w.GuestCount = 0.3
	}
	return w
}

// AntiAffinityEnabled reports whether scheduler-soft same-group spreading is
// active. Default false (nil Placement, nil AntiAffinity, or nil Enabled).
func (c *CPIConfig) AntiAffinityEnabled() bool {
	if c.Placement == nil || c.Placement.AntiAffinity == nil || c.Placement.AntiAffinity.Enabled == nil {
		return false
	}
	return *c.Placement.AntiAffinity.Enabled
}

// AntiAffinityUseHaRulesEnabled reports whether PVE HA negative-affinity rules
// should be maintained on top of scheduler-soft spreading. Default false. HA
// rule maintenance only runs when anti-affinity is also enabled, so this
// returns false unless both AntiAffinityEnabled() and UseHaRules are true.
func (c *CPIConfig) AntiAffinityUseHaRulesEnabled() bool {
	if !c.AntiAffinityEnabled() {
		return false
	}
	aa := c.Placement.AntiAffinity
	return aa.UseHaRules != nil && *aa.UseHaRules
}

// AZCandidates returns the node list for az and true when az is a known key in
// Placement.AZMap. Returns nil, false when: Placement is nil, AZMap is empty,
// or az is not in the map. Callers treat (nil, false) as "all online nodes".
func (c *CPIConfig) AZCandidates(az string) ([]string, bool) {
	if c.Placement == nil || len(c.Placement.AZMap) == 0 || az == "" {
		return nil, false
	}
	nodes, ok := c.Placement.AZMap[az]
	return nodes, ok
}

// ExcludeMaintenanceNodesEnabled returns the effective maintenance-node exclusion
// toggle. nil Placement block OR nil ExcludeMaintenanceNodes field → true
// (default-on: protective). Explicit *false → false (operator opt-out).
func (c *CPIConfig) ExcludeMaintenanceNodesEnabled() bool {
	if c.Placement == nil || c.Placement.ExcludeMaintenanceNodes == nil {
		return true
	}
	return *c.Placement.ExcludeMaintenanceNodes
}

// MaintenanceNodeTagsValue returns the effective list of operator maintenance
// tags. When the field is nil or empty, returns ["maintenance"] (the default).
func (c *CPIConfig) MaintenanceNodeTagsValue() []string {
	if c.Placement == nil || len(c.Placement.MaintenanceNodeTags) == 0 {
		return []string{"maintenance"}
	}
	return c.Placement.MaintenanceNodeTags
}

// AZFallbackOrderValue returns the operator-configured AZ fallback chain.
// Returns nil when Placement is nil or the field is empty.
func (c *CPIConfig) AZFallbackOrderValue() []string {
	if c.Placement == nil {
		return nil
	}
	return c.Placement.AZFallbackOrder
}

// AZShuffleEnabled reports whether AZ ordering randomization is on.
// Default false (nil Placement, nil AZShuffle, or *false).
func (c *CPIConfig) AZShuffleEnabled() bool {
	if c.Placement == nil || c.Placement.AZShuffle == nil {
		return false
	}
	return *c.Placement.AZShuffle
}

// DLBExplicitlyEnabled reports whether the master DLB override is set to true.
// Returns false when: c is nil, Placement is nil, Placement.DLB is nil, or
// DLB.Enabled is nil or *false. Only an explicit *true returns true.
// Use this to check whether ALL VMs (regardless of AZ) should be DLB-registered.
func (c *CPIConfig) DLBExplicitlyEnabled() bool {
	if c == nil || c.Placement == nil || c.Placement.DLB == nil || c.Placement.DLB.Enabled == nil {
		return false
	}
	return *c.Placement.DLB.Enabled
}

// DLBAZName returns the sentinel availability-zone name used to trigger DLB
// registration. When Placement.DLB is nil or DLB.AZName is nil (absent from
// JSON), returns "dlb" (the built-in default). When DLB.AZName is a non-nil
// pointer to an empty string (""), returns "" — the caller interprets "" as
// "sentinel disabled". Explicit non-empty strings are returned verbatim.
func (c *CPIConfig) DLBAZName() string {
	if c == nil || c.Placement == nil || c.Placement.DLB == nil || c.Placement.DLB.AZName == nil {
		return "dlb"
	}
	return *c.Placement.DLB.AZName
}

// DLBManageClusterCRS reports whether the CPI should actively manage the PVE
// cluster CRS setting. Default false (nil DLB block, nil field, or *false).
// When true, the CPI calls UpdateOptions to set crs=dynamic+auto-rebalance.
func (c *CPIConfig) DLBManageClusterCRS() bool {
	if c == nil || c.Placement == nil || c.Placement.DLB == nil || c.Placement.DLB.ManageClusterCRS == nil {
		return false
	}
	return *c.Placement.DLB.ManageClusterCRS
}

// DLBRequireSharedStorage reports whether DLB registration must be skipped for
// VMs on node-local storage. Default true (protective: nil DLB block or nil
// field returns true so local-storage VMs are never accidentally DLB-registered).
// Set RequireSharedStorage=false only when all VMs are guaranteed shared-storage.
func (c *CPIConfig) DLBRequireSharedStorage() bool {
	if c == nil || c.Placement == nil || c.Placement.DLB == nil || c.Placement.DLB.RequireSharedStorage == nil {
		return true
	}
	return *c.Placement.DLB.RequireSharedStorage
}

// DLBEligibleForAZ reports whether a VM with the given availability_zone string
// qualifies for DLB registration. Returns true when either:
//   - The master flag is on (DLBExplicitlyEnabled() == true), OR
//   - The sentinel AZ name is non-empty AND az matches it exactly.
//
// A nil or empty az argument never matches the sentinel (requires exact string
// equality). When both the master flag is off and the sentinel is disabled
// (DLBAZName() == ""), DLBEligibleForAZ always returns false.
func (c *CPIConfig) DLBEligibleForAZ(az string) bool {
	if c.DLBExplicitlyEnabled() {
		return true
	}
	sentinel := c.DLBAZName()
	return sentinel != "" && az == sentinel
}

// DLBConfigured reports whether the DLB block is present and carries at least
// one active configuration (master flag true OR a non-empty sentinel AZ name).
// Used to gate delete-time HA cleanup: when DLBConfigured() returns true, the
// delete_vm path broadens its HA deregistration check so DLB-registered VMs
// are cleaned up even when anti_affinity.use_ha_rules is false.
// Returns false when: Placement is nil, DLB is nil, master flag is false/nil,
// and AZName is nil or "". An explicit AZName="" (sentinel disabled) also
// returns false because no VMs could have been DLB-registered via the sentinel.
func (c *CPIConfig) DLBConfigured() bool {
	if c == nil || c.Placement == nil || c.Placement.DLB == nil {
		return false
	}
	return c.DLBExplicitlyEnabled() || c.DLBAZName() != ""
}

// AntiAffinityStrict reports whether PVE HA negative-affinity rules should be
// created in strict mode. Default false (nil Placement, nil AntiAffinity, or
// nil Strict pointer). When true, PVE enforces hard node-separation for HA
// group members; see AntiAffinityConfig.Strict for the small-cluster hazard.
func (c *CPIConfig) AntiAffinityStrict() bool {
	if c == nil || c.Placement == nil || c.Placement.AntiAffinity == nil || c.Placement.AntiAffinity.Strict == nil {
		return false
	}
	return *c.Placement.AntiAffinity.Strict
}

// EnsureNoIPConflictsEnabled returns the effective IP-conflict guard toggle.
// nil (field absent from JSON) → true (protective default for static networks).
// *false → false (operator opt-out, e.g. pure-DHCP networks).
// *true  → true.
func (c *CPIConfig) EnsureNoIPConflictsEnabled() bool {
	if c.EnsureNoIPConflicts == nil {
		return true
	}
	return *c.EnsureNoIPConflicts
}

// IPConflictProbeMode returns the normalized active IP-probe mode.
// Empty or absent maps to "off"; the value is lowercased and trimmed.
// Valid return values: "off", "agent".
func (c *CPIConfig) IPConflictProbeMode() string {
	if c == nil {
		return "off"
	}
	v := strings.ToLower(strings.TrimSpace(c.IPConflictProbe))
	if v == "" {
		return "off"
	}
	return v
}

// ActiveIPProbeEnabled reports whether the guest-agent IP fan-out probe is
// active. Returns true only when IPConflictProbeMode() == "agent".
func (c *CPIConfig) ActiveIPProbeEnabled() bool {
	return c.IPConflictProbeMode() == "agent"
}

// VMFirewallEnabled returns the effective global per-NIC firewall default.
// nil (field absent from JSON) → false (no behavior change versus prior
// releases). *false → false; *true → true.
func (c *CPIConfig) VMFirewallEnabled() bool {
	return c.VMFirewall != nil && *c.VMFirewall
}

// HooksValue returns the configured hook names in order, or nil when none are
// set.
func (c *CPIConfig) HooksValue() []string {
	return c.Hooks
}

// HealthCheckEnabled reports whether the post-create agent ping loop is active.
// Returns false when HealthCheck is nil, Enabled is nil, or Enabled is *false.
// Only an explicit *true returns true.
func (c *CPIConfig) HealthCheckEnabled() bool {
	if c == nil || c.HealthCheck == nil || c.HealthCheck.Enabled == nil {
		return false
	}
	return *c.HealthCheck.Enabled
}

// HealthCheckTimeoutSec returns the effective agent-ping deadline in seconds.
// Returns 300 when the block is absent, Enabled is false, or TimeoutSec is zero
// (zero means "use built-in default"). Callers should gate on
// HealthCheckEnabled() before using this value.
func (c *CPIConfig) HealthCheckTimeoutSec() int {
	const defaultHealthCheckTimeout = 300
	if c == nil || c.HealthCheck == nil || c.HealthCheck.TimeoutSec <= 0 {
		return defaultHealthCheckTimeout
	}
	return c.HealthCheck.TimeoutSec
}

// HealthCheckIntervalSec returns the effective sleep duration in seconds between
// successive agent ping attempts. Returns 5 when the block is absent or
// IntervalSec is zero. Zero is valid at runtime (no sleep); the accessor only
// returns the default when the field has not been explicitly configured.
func (c *CPIConfig) HealthCheckIntervalSec() int {
	const defaultHealthCheckInterval = 5
	if c == nil || c.HealthCheck == nil {
		return defaultHealthCheckInterval
	}
	// Zero is a valid explicit setting (no sleep between retries). Return the
	// default only when the block is absent (handled above). An explicitly
	// set IntervalSec of 0 is returned as-is.
	return c.HealthCheck.IntervalSec
}

// EffectiveRetryPolicy is a fully-resolved retry policy: every field carries a
// concrete value (the operator override when set, otherwise the class default).
// Consumers convert BaseMs/CapMs to durations as needed. This keeps the config
// package free of a time dependency while giving callers a single resolved view.
type EffectiveRetryPolicy struct {
	MaxAttempts int
	BaseMs      int
	CapMs       int
	JitterPct   int
}

// Class defaults for the retry policies. These are exactly the constants the
// CPI used before the retry config block existed, so an unset policy resolves
// to byte-identical behavior.
const (
	defaultStorageImportBaseMs    = 2000
	defaultStorageImportCapMs     = 30000
	defaultStorageImportJitterPct = 30

	defaultVMIDAllocBaseMs = 50
	defaultVMIDAllocCapMs  = 250

	defaultTaskPollBaseMs    = 2000
	defaultTaskPollCapMs     = 10000
	defaultTaskPollJitterPct = 10
)

// retryPolicyOrNil returns the named sub-policy, or nil when the retry block or
// the sub-policy is absent. Centralizes the nil-guard the accessors share.
func (c *CPIConfig) retryPolicyOf(pick func(*RetryConfig) *RetryPolicy) *RetryPolicy {
	if c == nil || c.Retry == nil {
		return nil
	}
	return pick(c.Retry)
}

// resolveField returns override when it is > 0, otherwise def. Used field-wise
// so a partially-specified policy fills only the fields the operator set.
func resolveField(override, def int) int {
	if override > 0 {
		return override
	}
	return def
}

// RetryStorageImport returns the resolved storage-import backoff policy.
// MaxAttempts is left at 0 when the operator has not set it, signaling callers
// to keep their own per-handler default (vm 10 / disk package default).
func (c *CPIConfig) RetryStorageImport() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.StorageImport })
	out := EffectiveRetryPolicy{
		BaseMs:    defaultStorageImportBaseMs,
		CapMs:     defaultStorageImportCapMs,
		JitterPct: defaultStorageImportJitterPct,
	}
	if p != nil {
		out.MaxAttempts = p.MaxAttempts // 0 → caller default
		out.BaseMs = resolveField(p.BaseMs, defaultStorageImportBaseMs)
		out.CapMs = resolveField(p.CapMs, defaultStorageImportCapMs)
		out.JitterPct = resolveField(p.JitterPct, defaultStorageImportJitterPct)
	}
	return out
}

// RetryVMIDAlloc returns the resolved VMID-conflict retry policy. MaxAttempts
// precedence: retry.vmid_alloc.max_attempts (>0) wins; otherwise 0 is returned
// so the caller falls back to vmid_alloc_attempts then its per-handler default.
// JitterPct is unused (the curve draws uniformly in [BaseMs,CapMs]).
func (c *CPIConfig) RetryVMIDAlloc() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.VMIDAlloc })
	out := EffectiveRetryPolicy{
		BaseMs: defaultVMIDAllocBaseMs,
		CapMs:  defaultVMIDAllocCapMs,
	}
	if p != nil {
		out.MaxAttempts = p.MaxAttempts // 0 → caller default
		out.BaseMs = resolveField(p.BaseMs, defaultVMIDAllocBaseMs)
		out.CapMs = resolveField(p.CapMs, defaultVMIDAllocCapMs)
	}
	return out
}

// RetryTaskPoll returns the resolved task-poll cadence. BaseMs is the poll
// interval, CapMs the maximum interval, JitterPct the per-poll jitter.
// MaxAttempts is not applicable to polling and is always 0.
func (c *CPIConfig) RetryTaskPoll() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.TaskPoll })
	out := EffectiveRetryPolicy{
		BaseMs:    defaultTaskPollBaseMs,
		CapMs:     defaultTaskPollCapMs,
		JitterPct: defaultTaskPollJitterPct,
	}
	if p != nil {
		out.BaseMs = resolveField(p.BaseMs, defaultTaskPollBaseMs)
		out.CapMs = resolveField(p.CapMs, defaultTaskPollCapMs)
		out.JitterPct = resolveField(p.JitterPct, defaultTaskPollJitterPct)
	}
	return out
}

// OperationTimeoutEnabled reports whether the per-method deadline envelope is
// active. Returns false when the block is nil, Enabled is nil, or Enabled is
// *false. Only an explicit *true returns true.
func (c *CPIConfig) OperationTimeoutEnabled() bool {
	if c == nil || c.OperationTimeout == nil || c.OperationTimeout.Enabled == nil {
		return false
	}
	return *c.OperationTimeout.Enabled
}

// Operation-timeout class defaults in seconds. Generous so the envelope only
// fires on a genuinely wedged operation, never on a slow-but-progressing one.
const (
	defaultOpTimeoutCreateSec  = 1800
	defaultOpTimeoutDeleteSec  = 900
	defaultOpTimeoutQuerySec   = 120
	defaultOpTimeoutDefaultSec = 600
)

// OperationTimeoutCreateSec returns the effective create-class deadline. Callers
// should gate on OperationTimeoutEnabled() first.
func (c *CPIConfig) OperationTimeoutCreateSec() int {
	if c == nil || c.OperationTimeout == nil || c.OperationTimeout.CreateSec <= 0 {
		return defaultOpTimeoutCreateSec
	}
	return c.OperationTimeout.CreateSec
}

// OperationTimeoutDeleteSec returns the effective delete-class deadline.
func (c *CPIConfig) OperationTimeoutDeleteSec() int {
	if c == nil || c.OperationTimeout == nil || c.OperationTimeout.DeleteSec <= 0 {
		return defaultOpTimeoutDeleteSec
	}
	return c.OperationTimeout.DeleteSec
}

// OperationTimeoutQuerySec returns the effective query-class deadline.
func (c *CPIConfig) OperationTimeoutQuerySec() int {
	if c == nil || c.OperationTimeout == nil || c.OperationTimeout.QuerySec <= 0 {
		return defaultOpTimeoutQuerySec
	}
	return c.OperationTimeout.QuerySec
}

// OperationTimeoutDefaultSec returns the effective deadline for every method not
// in the create/delete/query classes.
func (c *CPIConfig) OperationTimeoutDefaultSec() int {
	if c == nil || c.OperationTimeout == nil || c.OperationTimeout.DefaultSec <= 0 {
		return defaultOpTimeoutDefaultSec
	}
	return c.OperationTimeout.DefaultSec
}

// StemcellProvenanceEnabled reports whether stemcell provenance tracking is
// active. Returns false when the block is nil, Provenance is nil, or Provenance
// is *false. Only an explicit *true returns true.
func (c *CPIConfig) StemcellProvenanceEnabled() bool {
	if c == nil || c.Stemcell == nil || c.Stemcell.Provenance == nil {
		return false
	}
	return *c.Stemcell.Provenance
}

// StemcellOrphanPruneEnabled reports whether stemcell orphan pruning is active.
// Returns false when the block is nil, PruneOrphans is nil, or PruneOrphans is
// *false. Only an explicit *true returns true.
func (c *CPIConfig) StemcellOrphanPruneEnabled() bool {
	if c == nil || c.Stemcell == nil || c.Stemcell.PruneOrphans == nil {
		return false
	}
	return *c.Stemcell.PruneOrphans
}

// StemcellOrphanPruneDryRun reports whether orphan pruning runs in dry-run mode
// (log only, no deletions). Returns false when the block is nil, PruneDryRun is
// nil, or PruneDryRun is *false. Only an explicit *true returns true.
func (c *CPIConfig) StemcellOrphanPruneDryRun() bool {
	if c == nil || c.Stemcell == nil || c.Stemcell.PruneDryRun == nil {
		return false
	}
	return *c.Stemcell.PruneDryRun
}

// StemcellDirectorID returns the configured director identifier for provenance
// tagging. Returns empty string when the block is nil or DirectorID is unset.
func (c *CPIConfig) StemcellDirectorID() string {
	if c == nil || c.Stemcell == nil {
		return ""
	}
	return c.Stemcell.DirectorID
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
	c.validatePlacement(&errs)
	c.validateHooks(&errs)
	c.validateHealthCheck(&errs)
	c.validateRetry(&errs)
	c.validateOperationTimeout(&errs)
	c.validateStorageTiers(&errs)
	c.validateDiskPerformance(&errs)
	c.validateStemcell(&errs)
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

	// IPConflictProbe enum: validate only when non-empty.
	if c.IPConflictProbe != "" {
		switch strings.ToLower(strings.TrimSpace(c.IPConflictProbe)) {
		case "off", "agent":
			// valid
		default:
			*errs = append(*errs, fmt.Sprintf(
				"ip_conflict_probe must be one of off|agent (or empty for default off), got %q",
				c.IPConflictProbe,
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

// validatePlacement validates the optional Placement block. Skipped entirely
// when Placement is nil (validate-only-when-set). Checks:
//   - AZMap: every AZ name is non-empty (always true by map key semantics),
//     every node name list is non-empty, every node name is non-empty.
//   - Weights (when present): all axes ≥ 0 (negative weights invert scoring).
//   - DLB (when present): see validateDLB.
//
// No PVE API calls are made; node names are not checked for cluster membership.
func (c *CPIConfig) validatePlacement(errs *[]string) {
	if c.Placement == nil {
		return
	}
	// AZMap: each AZ must have at least one non-empty node name.
	for az, nodes := range c.Placement.AZMap {
		if len(nodes) == 0 {
			*errs = append(*errs, fmt.Sprintf(
				"placement.az_map[%q] must contain at least one node name", az,
			))
			continue
		}
		for i, n := range nodes {
			if n == "" {
				*errs = append(*errs, fmt.Sprintf(
					"placement.az_map[%q][%d] must not be an empty string", az, i,
				))
			}
		}
	}
	// Weights: negative values invert the scoring formula and are rejected.
	if c.Placement.Weights != nil {
		w := c.Placement.Weights
		checkWeight := func(name string, v float64) {
			if v < 0 {
				*errs = append(*errs, fmt.Sprintf(
					"placement.weights.%s must be ≥ 0, got %g", name, v,
				))
			}
		}
		checkWeight("mem", w.Mem)
		checkWeight("storage", w.Storage)
		checkWeight("cpu", w.CPU)
		checkWeight("guest_count", w.GuestCount)
	}
	// DLB: validate when the sub-block is present.
	c.validateDLB(errs)
}

// validateHooks appends an error for each configured hook name that does not
// resolve in the built-in hook registry. Empty Hooks is valid (no middleware).
func (c *CPIConfig) validateHooks(errs *[]string) {
	for _, name := range c.Hooks {
		if !hooks.Known(name) {
			*errs = append(*errs, fmt.Sprintf(
				"unknown hook %q; known hooks: %s", name, strings.Join(hooks.Names(), ", "),
			))
		}
	}
}

// validateDLB validates the optional Placement.DLB sub-block. Skipped when
// Placement.DLB is nil (validate-only-when-set). All DLBConfig fields are
// optional *bool or *string with no enum or range constraints — the only
// invariant enforced here is that, when Enabled is explicitly false and
// AZName is explicitly "" (sentinel disabled), at least one of them must be
// non-nil for the block to be meaningful. This is advisory only (a warning
// path rather than a hard error) so the operator is not blocked by a
// vacuously-empty dlb block. No error is appended for an all-nil DLB block;
// the block is simply inert.
func (c *CPIConfig) validateDLB(_ *[]string) {
	if c.Placement == nil || c.Placement.DLB == nil {
		return
	}
	// No numeric fields; no enum fields; AZName accepts any string including "".
	// The Strict field on AntiAffinityConfig is a *bool — Go's type system
	// guarantees the pointer target is a valid bool, so no additional check is
	// needed. Nothing further to validate.
}

// validateHealthCheck validates the optional HealthCheck block.
// Skipped entirely when HealthCheck is nil or Enabled is not *true
// (validate-only-when-set). When enabled, TimeoutSec must be 0–3600 and
// IntervalSec must be 0–3600. TimeoutSec == 0 is accepted and maps to the
// built-in default of 300 s via HealthCheckTimeoutSec(). Negative values
// are rejected.
func (c *CPIConfig) validateHealthCheck(errs *[]string) {
	if !c.HealthCheckEnabled() {
		return
	}
	hc := c.HealthCheck
	if hc.TimeoutSec < 0 || hc.TimeoutSec > 3600 {
		*errs = append(*errs, fmt.Sprintf(
			"health_check.timeout_sec must be 0-3600 when enabled, got %d", hc.TimeoutSec,
		))
	}
	if hc.IntervalSec < 0 || hc.IntervalSec > 3600 {
		*errs = append(*errs, fmt.Sprintf(
			"health_check.interval_sec must be 0-3600 when enabled, got %d", hc.IntervalSec,
		))
	}
}

// validateRetry rejects negative or contradictory retry-policy values. Zero is
// always valid (means "use the class default"). Only fields the operator
// actually set are present, so validation is cheap and never fires for an unset
// block.
//
// The cap-vs-base ordering is checked on the EFFECTIVE (resolved) values, not
// the raw fields: an operator who sets only cap_ms leaves base_ms at 0, which
// resolves to the class default base. Comparing raw fields would skip the check
// (raw base 0 fails the >0 guard) and let an effective cap smaller than the
// effective base through, producing a degenerate near-zero backoff. eff returns
// the resolved (base, cap) the runtime curve actually uses.
func (c *CPIConfig) validateRetry(errs *[]string) {
	if c == nil || c.Retry == nil {
		return
	}
	checkRaw := func(name string, p *RetryPolicy) {
		if p == nil {
			return
		}
		if p.MaxAttempts < 0 {
			*errs = append(*errs, fmt.Sprintf("retry.%s.max_attempts must be >= 0, got %d", name, p.MaxAttempts))
		}
		if p.BaseMs < 0 {
			*errs = append(*errs, fmt.Sprintf("retry.%s.base_ms must be >= 0, got %d", name, p.BaseMs))
		}
		if p.CapMs < 0 {
			*errs = append(*errs, fmt.Sprintf("retry.%s.cap_ms must be >= 0, got %d", name, p.CapMs))
		}
		if p.JitterPct < 0 || p.JitterPct > 100 {
			*errs = append(*errs, fmt.Sprintf("retry.%s.jitter_pct must be 0-100, got %d", name, p.JitterPct))
		}
	}
	checkRaw("storage_import", c.Retry.StorageImport)
	checkRaw("vmid_alloc", c.Retry.VMIDAlloc)
	checkRaw("task_poll", c.Retry.TaskPoll)

	// Effective cap >= base. Only meaningful when the operator set at least one
	// of the two fields for that class (an entirely-absent policy resolves to
	// the shipped defaults, which always satisfy cap >= base).
	checkEffective := func(name string, p *RetryPolicy, eff EffectiveRetryPolicy) {
		if p == nil || (p.BaseMs == 0 && p.CapMs == 0) {
			return
		}
		if eff.CapMs < eff.BaseMs {
			*errs = append(*errs, fmt.Sprintf(
				"retry.%s: effective cap_ms (%d) must be >= effective base_ms (%d)",
				name, eff.CapMs, eff.BaseMs))
		}
	}
	checkEffective("storage_import", c.Retry.StorageImport, c.RetryStorageImport())
	checkEffective("vmid_alloc", c.Retry.VMIDAlloc, c.RetryVMIDAlloc())
	checkEffective("task_poll", c.Retry.TaskPoll, c.RetryTaskPoll())
}

// validateOperationTimeout rejects out-of-range per-class deadlines. Honored
// only when the envelope is enabled (validate-only-when-set); zero is valid and
// maps to the class default. The upper bound (24h) is a sanity ceiling that
// still leaves the envelope able to bound any realistic operation.
func (c *CPIConfig) validateOperationTimeout(errs *[]string) {
	if !c.OperationTimeoutEnabled() {
		return
	}
	const maxSec = 86400
	ot := c.OperationTimeout
	checkSec := func(name string, v int) {
		if v < 0 || v > maxSec {
			*errs = append(*errs, fmt.Sprintf(
				"operation_timeout.%s must be 0-%d when enabled, got %d", name, maxSec, v))
		}
	}
	checkSec("create_sec", ot.CreateSec)
	checkSec("delete_sec", ot.DeleteSec)
	checkSec("query_sec", ot.QuerySec)
	checkSec("default_sec", ot.DefaultSec)
}

// knownDiskCacheModes is the set of PVE per-disk cache mode strings accepted in
// DiskPerformanceDefaults.Cache. Mirrors the values accepted by the PVE qemu
// disk bus configuration; an empty string means "no override" and is valid.
var knownDiskCacheModes = map[string]struct{}{
	"none":         {},
	"writethrough": {},
	"writeback":    {},
	"unsafe":       {},
	"directsync":   {},
}

// IsKnownDiskCacheMode reports whether mode is a PVE per-disk cache mode the CPI
// accepts. An empty string ("no override") is reported as valid. Exported so the
// handlers package can validate call-time cache values against the single
// authoritative set without duplicating the literals.
func IsKnownDiskCacheMode(mode string) bool {
	if mode == "" {
		return true
	}
	_, ok := knownDiskCacheModes[mode]
	return ok
}

// validateDiskPerformance validates the optional DiskPerformance block.
// Skipped entirely when DiskPerformance is nil (validate-only-when-set).
// Rules enforced when the block is present:
//   - Cache non-empty and not in {none,writethrough,writeback,unsafe,directsync} → error.
//   - MBpsRd/MBpsWr non-nil and < 0 → error.
//   - IOPSRd/IOPSWr non-nil and < 0 → error.
//
// Boolean fields (Iothread, Discard, SSD, VirtioSCSISingle) are *bool with no
// further constraints; any non-nil *bool is valid.
func (c *CPIConfig) validateDiskPerformance(errs *[]string) {
	if c.DiskPerformance == nil {
		return
	}
	dp := c.DiskPerformance
	if dp.Cache != "" {
		if _, ok := knownDiskCacheModes[dp.Cache]; !ok {
			*errs = append(*errs, fmt.Sprintf(
				"disk_performance.cache must be one of none|writethrough|writeback|unsafe|directsync, got %q",
				dp.Cache,
			))
		}
	}
	if dp.MBpsRd != nil && *dp.MBpsRd < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"disk_performance.mbps_rd must be >= 0, got %g", *dp.MBpsRd,
		))
	}
	if dp.MBpsWr != nil && *dp.MBpsWr < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"disk_performance.mbps_wr must be >= 0, got %g", *dp.MBpsWr,
		))
	}
	if dp.IOPSRd != nil && *dp.IOPSRd < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"disk_performance.iops_rd must be >= 0, got %d", *dp.IOPSRd,
		))
	}
	if dp.IOPSWr != nil && *dp.IOPSWr < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"disk_performance.iops_wr must be >= 0, got %d", *dp.IOPSWr,
		))
	}
}

// validateStemcell validates the optional Stemcell block.
// Skipped entirely when Stemcell is nil (validate-only-when-set).
// Rules enforced when the block is present:
//   - DirectorID non-empty after TrimSpace must contain at least one
//     [A-Za-z0-9-] character; a value consisting entirely of non-word/non-hyphen
//     characters (e.g. "@@@") is rejected.
//
// Boolean fields (Provenance, PruneOrphans, PruneDryRun) are *bool with no
// further constraints; any non-nil *bool is valid.
func (c *CPIConfig) validateStemcell(errs *[]string) {
	if c.Stemcell == nil {
		return
	}
	id := strings.TrimSpace(c.Stemcell.DirectorID)
	if id != "" {
		if !stemcellDirectorIDRe.MatchString(id) {
			*errs = append(*errs, fmt.Sprintf(
				"stemcell.director_id must contain at least one alphanumeric or hyphen character, got %q",
				c.Stemcell.DirectorID,
			))
		}
	}
}

// stemcellDirectorIDRe matches any string that contains at least one
// alphanumeric character or hyphen. Used by validateStemcell.
var stemcellDirectorIDRe = regexp.MustCompile(`[A-Za-z0-9-]`)

// knownPVEStorageTypes is the exhaustive set of PVE storage plugin names
// accepted in StorageTierCriteria.Types. Hardcoded here to avoid an import
// cycle with internal/pve (which itself imports internal/config). The set
// mirrors pve/storage_types.go plus "dir" and "btrfs", which that file omits
// but which PVE exposes as valid storage types.
var knownPVEStorageTypes = map[string]struct{}{
	"lvm":       {},
	"lvmthin":   {},
	"zfspool":   {},
	"dir":       {},
	"nfs":       {},
	"cifs":      {},
	"rbd":       {},
	"cephfs":    {},
	"btrfs":     {},
	"glusterfs": {},
	"pbs":       {},
}

// validateStorageTiers validates every entry in StorageTiers. Skipped entirely
// when StorageTiers is nil or empty (validate-only-when-set). For each entry:
//   - At least one of Types or Shared must be set; an entry with neither is
//     a CloudError naming the tier.
//   - Every string in Types must be a known PVE storage type; unknown values
//     produce a CloudError naming the tier and the unknown type.
//
// VMTypes, DiskTypes, and SecurityGroups carry no structural constraints beyond
// what their Go types enforce, so they are not validated here.
func (c *CPIConfig) validateStorageTiers(errs *[]string) {
	for name, criteria := range c.StorageTiers {
		if len(criteria.Types) == 0 && criteria.Shared == nil {
			*errs = append(*errs, fmt.Sprintf(
				"storage_tiers[%s]: must set types or shared", name,
			))
			continue
		}
		for _, t := range criteria.Types {
			if _, ok := knownPVEStorageTypes[t]; !ok {
				*errs = append(*errs, fmt.Sprintf(
					"storage_tiers[%s]: unknown storage type %q; valid types: lvm, lvmthin, zfspool, dir, nfs, cifs, rbd, cephfs, btrfs, glusterfs, pbs",
					name, t,
				))
			}
		}
	}
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
