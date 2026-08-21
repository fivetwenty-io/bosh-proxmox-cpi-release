package config

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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
	//
	// The ConfigDrive ISO is attached as a CD-ROM on scsi30 for the VM's whole
	// life (see internal/agent/configdrive.go), not only at boot. PVE refuses
	// to live-migrate a VM whose CD-ROM volume sits on non-shared storage, and
	// HA recovery on another node fails at start because the ISO file does not
	// exist there — silently defeating placement.dlb, pin_az_via_ha_rules, and
	// anti_affinity.use_ha_rules. Use a shared pool (rbd, nfs, cifs, glusterfs,
	// cephfs) whenever any of those HA-driven features is active. See
	// RequireSharedISOForHA and ISOStorageFollowVMStorage.
	ISOStorage string `json:"iso_storage,omitempty"`

	// RequireSharedISOForHA escalates the config-drive ISO migration-safety
	// Warn (emitted by create_vm whenever the VM is HA-registered under DLB,
	// AZ node-affinity pinning, or anti-affinity HA rules while ISOStorage is
	// not a shared pool) to a non-retriable CloudError, failing create_vm
	// instead of only warning. Default false (nil → warn-only, byte-identical
	// to prior releases). Use RequireSharedISOForHAEnabled() for the effective
	// bool.
	RequireSharedISOForHA *bool `json:"require_shared_iso_for_ha,omitempty"`

	// ISOStorageFollowVMStorage, when true, resolves the ConfigDrive ISO pool
	// to VMStorage instead of the ISOStorage default, provided VMStorage
	// advertises PVE content type `iso` and is shared. This is evaluated once
	// at CPI process startup (see agent.ResolveISOStorage), before any create_vm
	// call, because BOSH renders the "local" spec default for iso_storage into
	// the config JSON whether or not the operator set it explicitly — the CPI
	// cannot distinguish "unset" from "explicitly local" any other way. As a
	// result this flag treats ISOStorage=="local" (the literal spec default) as
	// the "unset" sentinel: an operator who explicitly types iso_storage: local
	// while this flag is active (its default state) gets VMStorage-following
	// behavior instead of a literal local pool. Set iso_storage to any other
	// value to pin a literal pool that this flag will never override. When
	// VMStorage lacks `iso` content, is not shared, or cannot be resolved, the
	// CPI falls back to ISOStorage unchanged and logs a Warn (fail-open).
	//
	// Default TRUE (nil → true): a zero-config deployment now lands the
	// ConfigDrive ISO on the same (typically shared) pool as the VM's disks
	// instead of the historically node-local "local" default, so HA/DLB/
	// migration are not silently defeated by an unshared ISO pool out of the
	// box. Explicit *false opts out (ISOStorage always
	// used as configured). Pointer-typed tri-state so JSON absence (nil, →
	// true) is distinguishable from an explicit *false. Use
	// ISOStorageFollowVMStorageEnabled() for the effective bool.
	ISOStorageFollowVMStorage *bool `json:"iso_storage_follow_vm_storage,omitempty"`

	// Network
	NetworkBridge string `json:"network_bridge"`

	// NetworkMode selects create_network/delete_network behavior.
	// "sdn" — PVE SDN vnet lifecycle (requires SDN enabled on the cluster).
	// "bridge" — Linux bridge lifecycle via nodes API.
	// "auto" — use SDN if cloud_properties.zone or config SDNZone is set;
	//           fall back to bridge otherwise (legacy heuristic, opt-in).
	// Defaults to "bridge" (simple-first: plain Linux bridges need no SDN
	// stack; SDN is a one-line opt-in).
	NetworkMode string `json:"network_mode,omitempty"`

	// SDNZone is the default PVE SDN zone name for vnet creation. Operators may
	// override per-call via cloud_properties.zone. When empty and auto-manage is
	// enabled, the CPI uses the turnkey zone name "bosh"; when empty and
	// auto-manage is disabled, the zone must be supplied in cloud_properties.
	SDNZone string `json:"sdn_zone,omitempty"`

	// SDNZoneType is the PVE zone type used when the CPI creates the zone itself
	// (auto-manage enabled and zone absent). Valid values: simple, vlan, qinq,
	// vxlan, evpn. Defaults to "vxlan" — a cluster-wide overlay with no fabric
	// prerequisites. "vlan" is also turnkey (created with NetworkBridge as the
	// underlay; its vnets span nodes once the fabric trunks the VLANs).
	// "qinq" is never auto-created (pre-existing zones only). "evpn" is never
	// auto-created: the operator must pre-create the zone and its BGP
	// controller; the CPI then manages only vnets and subnets inside it.
	SDNZoneType string `json:"sdn_zone_type,omitempty"`

	// SDNAutoManageZone controls whether the CPI may create and delete the zone.
	// When enabled, create_network creates the zone (type SDNZoneType) if absent,
	// and delete_network deletes the zone if: it is not pinned by SDNZone, it is
	// not an EVPN zone, and it has no remaining vnets. Pointer so JSON omission
	// (nil) is distinguishable from explicit false; use SDNAutoManageZoneEnabled()
	// for the effective bool. Defaults to true (turnkey zone ownership).
	SDNAutoManageZone *bool `json:"sdn_auto_manage_zone,omitempty"`

	// SDNVxlanPeers is an optional explicit list of VXLAN peer IPs used when the
	// CPI creates a vxlan zone. Empty (the default) derives the peer list from
	// the cluster membership (/cluster/status node IPs). Set it when tunnel
	// traffic must ride a dedicated underlay network whose addresses differ from
	// the management IPs.
	SDNVxlanPeers []string `json:"sdn_vxlan_peers,omitempty"`

	// SDNVNIRangeStart/End bound the band from which the CPI auto-allocates
	// vnet tags (VNIs for vxlan/evpn, 802.1Q VLAN IDs for vlan/qinq) for zone
	// types that require one. 0 defaults to 5000/5999 — or 2000/2999 when
	// SDNZoneType is vlan/qinq, keeping the band inside the 4094 VLAN ID cap
	// (an explicit band beyond the cap fails validation). Per-network override
	// via cloud_properties.vnet_tag.
	SDNVNIRangeStart int `json:"sdn_vni_range_start,omitempty"`
	SDNVNIRangeEnd   int `json:"sdn_vni_range_end,omitempty"`

	// SDNZoneMTU overrides the MTU on CPI-created zones. Unset (nil) lets PVE
	// derive it from the underlay (e.g. 1450 on a 1500 underlay for vxlan).
	SDNZoneMTU *int64 `json:"sdn_zone_mtu,omitempty"`

	// TLS — pointer so JSON omission (nil) is distinguishable from explicit false.
	// Use VerifySSLValue() to obtain the effective bool.
	VerifySSL *bool `json:"verify_ssl,omitempty"`

	// PVECACertPEM is an optional PEM-encoded CA certificate (or chain) that
	// replaces the system trust pool when verifying the PVE API TLS certificate.
	// Use when the PVE host presents a certificate signed by a private CA that
	// the host trust store does not already include. When empty (the default),
	// TLS verification uses the system trust pool — behavior is byte-identical to
	// prior releases. Ignored when VerifySSL is false.
	PVECACertPEM string `json:"pve_ca_cert,omitempty"`

	// RejectTLSDowngradeOverrides hardens the per-request pve_* context
	// override mechanism (config.ApplyContextOverrides / handlers.Deps.
	// WithRequestOverrides): when true, a request whose context carries
	// pve_verify_ssl=false against a job-level config that itself verifies
	// (VerifySSLValue() true) is rejected with a non-retriable CloudError
	// instead of the default warn-and-proceed behavior. A base config that
	// already has verify_ssl=false is not a downgrade and is never rejected
	// by this knob. Pointer so JSON omission (nil) is distinguishable from
	// explicit false; use RejectTLSDowngradeOverridesEnabled() for the
	// effective bool. Default false (nil): behavior is byte-identical to
	// every prior release — the downgrade is still logged at Warn, never
	// rejected. Omit from ERB when unset; emit only when true.
	RejectTLSDowngradeOverrides *bool `json:"reject_tls_downgrade_overrides,omitempty"`

	// OperatorID is an optional label appended to the User-Agent header on all
	// PVE API requests as "pid-<value>". Use it to attribute CPI traffic in PVE
	// access logs when multiple BOSH directors share a single PVE cluster.
	// When empty (the default), the User-Agent is "BOSH-PVE-CPI/<version>"
	// with no suffix — byte-identical to prior releases.
	OperatorID string `json:"operator_id,omitempty"`

	// Agent
	AgentMode    string `json:"agent_mode"`
	VMDiskFormat string `json:"vm_disk_format,omitempty"`
	LogLevel     string `json:"log_level,omitempty"`

	// CPUType is the emulated CPU type/model PVE writes to the new VM's
	// "cpu" config key (e.g. "host", "x86-64-v2-AES"). Empty is filled by
	// ApplyDefaults with DefaultCPUType ("host" — full physical CPU feature
	// set, maximum guest performance; clusters mixing CPU generations that
	// need live migration should set a portable model such as
	// "x86-64-v2-AES" instead). The sentinel value
	// CPUTypePVEDefault ("pve-default") restores the legacy behavior: the
	// CPI writes no cpu key at all and PVE falls back to kvm64. Per-VM
	// cloud_properties.cpu_type — resolved through the same
	// call/disk_type/vm_type layered resolver as other create_vm knobs —
	// takes precedence over this global default when both are set.
	// cloud_properties.pve_config.cpu is a separate raw escape hatch applied
	// after this value in the create_vm sequence, so it always wins as the
	// final write when both are set. Use CPUTypeValue() for the effective,
	// whitespace-trimmed, sentinel-resolved string. PVE validates the model
	// name itself; the CPI passes the value through verbatim.
	CPUType string `json:"cpu_type,omitempty"`

	// Balloon controls the PVE "balloon" config key written to every new VM.
	// Empty is filled by ApplyDefaults with DefaultBalloon ("0" — balloon
	// device disabled): BOSH sizes VMs deterministically from the manifest,
	// and auto-ballooning reclaims guest memory beneath the Director's
	// assumptions. A positive integer (MiB) enables PVE auto-ballooning with
	// that floor; the CPI fails fast when it exceeds the VM's memory. The
	// sentinel BalloonPVEDefault ("pve-default") makes the CPI write no
	// balloon key at all, restoring PVE's own default (device enabled,
	// balloon = memory). Per-VM cloud_properties.balloon — resolved through
	// the same call/disk_type/vm_type layered resolver as other create_vm
	// knobs — takes precedence over this global value; the sentinel works at
	// either layer. Use BalloonValue() for the effective, whitespace-trimmed,
	// sentinel-resolved string.
	Balloon string `json:"balloon,omitempty"`

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

	// ResizeWaitForConvergence, when true, makes resize_disk poll the VM config
	// after the PVE resize task completes until the reported disk size reaches
	// the requested size, before returning. On asynchronous storage backends
	// (Ceph RBD, LVM-thin) the size metadata can lag the task completion, so a
	// follow-on operation may otherwise observe the old size. The poll is
	// best-effort: if the size has not converged within the bound it logs a
	// warning and returns success, never blocking the director. Default false
	// (nil → disabled), so behavior is byte-identical. Use
	// ResizeWaitForConvergenceEnabled().
	ResizeWaitForConvergence *bool `json:"resize_wait_for_convergence,omitempty"`

	// ResizeConvergenceTimeoutSec bounds the resize_wait_for_convergence poll.
	// The poll uses this independent budget (not the operation_timeout envelope)
	// so it is bounded even when that envelope is disabled. <= 0 (default)
	// resolves to 120 seconds. Negative values are rejected at validation. Use
	// ResizeConvergenceTimeoutSecValue().
	ResizeConvergenceTimeoutSec int `json:"resize_convergence_timeout_sec,omitempty"`

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

	// StemcellFetchBlockPrivateNetworks, when true, makes the https and
	// bosh+blobstore stemcell fetchers refuse any connection whose target
	// resolves to a private/loopback/link-local address, re-checked on every
	// dial so redirect hops and DNS rebinding are covered. Default false —
	// private mirrors on RFC1918 space are a legitimate deployment shape.
	// Enable when the mirror is public and redirect chains should never
	// re-enter the private network.
	StemcellFetchBlockPrivateNetworks bool `json:"stemcell_fetch_block_private_networks,omitempty"`

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

	// StemcellTemplatePool is an optional PVE resource pool name assigned to
	// newly created template VMs. Empty (default) means no pool assignment.
	// Create-if-missing: the CPI creates the pool before the first template
	// VM is assigned to it, tagging it with a "managed by bosh-pve-cpi"
	// provenance comment (see VMPool doc for the shared rationale and the
	// distinct-from-VMPool ACL boundary this preserves).
	// validate-only-when-set: must not equal VMPool (see validateVMPool),
	// must not contain '/', must match the PVE poolid charset, and must not
	// carry the cluster-lock sentinel namespace prefix ("bosh-lock-") — see
	// validateStemcellTemplatePool. Any other non-empty string is accepted.
	StemcellTemplatePool string `json:"stemcell_template_pool,omitempty"`

	// VMPool is an optional PVE resource pool name assigned to every VM
	// create_vm provisions, on both the import path (CreateQemuParams.Pool)
	// and the clone path (CreateQemuCloneParams.Pool), and is subject to the
	// call > vm_type > vm_pool_template > global precedence pipeline (see
	// resolvePoolName in internal/cpi/handlers). Empty (default) means no
	// pool assignment — byte-identical to every release before this property
	// existed. Setting it lets an operator scope the CPI's VM.* ACL grants to
	// /pool/<name> instead of cluster-wide /vms, shrinking the blast radius
	// of a compromised CPI token on a shared cluster — see
	// docs/pve-api-permissions.md for the reduced ACL table.
	//
	// Both VMPool and StemcellTemplatePool are now create-if-missing: the CPI
	// creates the resolved pool before the first VM or template lands in it,
	// tagging it with a "managed by bosh-pve-cpi" provenance comment. Their
	// spec/ERB defaults are distinct ("bosh" and "bosh-templates"
	// respectively) so the ACL boundary between workload VMs and shared
	// stemcell templates holds even for an operator who sets nothing;
	// setting either to "" explicitly opts that pool back out (no
	// assignment, no auto-creation).
	//
	// validate-only-when-set: must not equal StemcellTemplatePool, must not
	// contain '/', must match the PVE poolid charset, and must not carry the
	// cluster-lock sentinel namespace prefix ("bosh-lock-") — see
	// validateVMPool. Any other non-empty string is accepted.
	VMPool string `json:"vm_pool,omitempty"`

	// VMPoolTemplate is a director-level pool-name template rendered at
	// create_vm time when neither the call-level nor the vm_type-level
	// cloud_properties.pool is set (precedence position 3, above the global
	// VMPool default). Supported variables: "{prefix}" (VMPrefix),
	// "{director}", "{deployment}", and "{instance_group}"; any other
	// "{...}" token is a validation error. The rendered name is sanitized
	// (repeated separators collapsed, leading/trailing '-' trimmed) and must
	// still pass the same flat-name and charset rules as VMPool; a render
	// that collapses to "" falls through to the global VMPool default.
	// Empty disables this layer entirely (the Go zero value); the release
	// default is "bosh-{director}-{deployment}" (job spec), so a director
	// gets per-deployment pools unless the operator sets the property to ""
	// explicitly. On a create-env path the director token is underivable and
	// the deployment token falls back to CreateEnvDeployment, so the default
	// renders "bosh-create-env".
	// validate-only-when-set; omit from ERB when empty.
	VMPoolTemplate string `json:"vm_pool_template,omitempty"`

	// DestroyUnreferencedDisks opts in to passing DestroyUnreferencedDisks=true
	// to PVE's DeleteQemu on every non-retain delete_vm (sync path, fast path,
	// and the fast-path straggler sweep). PVE's own semantics: "free every
	// volume that (a) is not referenced in the destroyed VM's config AND (b)
	// has a VMID matching the VM being destroyed" — a storage-wide scan by
	// VMID, not a config-scoped one.
	//
	// Default FALSE. On storage dedicated to a single PVE cluster this is
	// safe and desirable: it sweeps up orphaned own-VMID volumes (e.g. a
	// disk left behind by an interrupted create) that the config-scoped
	// guards (guardUnusedVolumes, detachForeignActiveDisks) never touch,
	// because those only see volumes CURRENTLY referenced by this VM's
	// config. It is UNSAFE the moment storage is shared with a second,
	// independent PVE cluster (a second BOSH-Proxmox AZ pointed at the same
	// NFS/dir export, same storage ID): the destroyed VM's VMID band can
	// overlap the other cluster's VMID band (see WithStorageScan's doc
	// comment for the allocation-time half of this hazard), and this flag
	// would free the OTHER cluster's live disks — they are unreferenced from
	// THIS cluster's view and share the VMID by construction. Nothing in the
	// delete path can distinguish "orphaned by this cluster" from "owned by
	// another cluster sharing storage": both look identical (unreferenced,
	// matching VMID) from here.
	//
	// Enable only when the configured pve.vm_storage/disk_storage/iso_storage
	// pools are NOT shared with any other independent PVE cluster or non-CPI
	// tooling that allocates VMIDs in the same range. On shared storage,
	// leave this false and rely on disjoint per-CPI VMID banding instead
	// (see docs/multi-cluster.md, a later phase) — orphaned own-cluster
	// volumes accumulate and are visible to scripts/disk-audit rather than
	// being swept automatically.
	//
	// No validation; any bool value is accepted. omit from ERB when false.
	DestroyUnreferencedDisks bool `json:"destroy_unreferenced_disks,omitempty"`

	// PoolReapEmpty enables an empty-pool reaper at delete_vm: when the
	// destroyed VM's pool membership (captured before destroy) is a
	// CPI-managed pool (provenance comment "managed by bosh-pve-cpi") that
	// PVE reports empty after the destroy, the CPI deletes it. An
	// operator-created pool without the provenance comment is never reaped,
	// and the static VMPool and StemcellTemplatePool are refused by name
	// regardless of comment (see reapEmptyPoolIfManaged). The release
	// default is true (the ERB always emits the key), so per-deployment
	// pools created by the vm_pool_template default are reaped with their
	// deployments; the Go zero value of a raw config without the key remains
	// false. The fast-path delete never reaps, so some empty pools persist
	// until a later synchronous delete. No validation; any bool value is
	// accepted.
	PoolReapEmpty bool `json:"pool_reap_empty,omitempty"`

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

	// StemcellStrategy selects how create_vm materializes a VM root disk from
	// a stemcell CID.
	// "template" — clone the per-cluster cache template (CoW-fast; the cache
	//              is built eagerly by create_stemcell and rebuilt on demand).
	// "import"   — import-from the stemcell qcow2 directly (full copy per VM;
	//              slower but template-independent).
	// Per-VM cloud_properties.stemcell_strategy overrides this global.
	// Defaults to "template".
	StemcellStrategy string `json:"stemcell_strategy,omitempty"`

	// ReplicaAdoptTimeoutSec bounds the adopt-and-wait on a racing concurrent
	// template-replica clone. When two CPI invocations independently decide a node
	// needs a per-node stemcell replica, both can pass the settled-only existence
	// check while a winner is still building and then clone, producing a duplicate
	// half-built replica template. When this is > 0, the replica build first probes
	// for an in-flight winner (a VM carrying the replica tags but not yet frozen /
	// still clone-locked) and, finding one, waits up to this many seconds for it to
	// become a settled template and adopts it instead of building a duplicate; a
	// winner that never settles within the bound yields a retriable error so the
	// director re-drives. Default 0 disables the adopt path entirely, leaving the
	// build behaviour byte-identical. validate-only-when-set; omit from ERB when 0.
	ReplicaAdoptTimeoutSec int `json:"replica_adopt_timeout_sec,omitempty"`

	// CloneMode controls the clone type used by create_vm when cloning a
	// stemcell template. Values: "auto" (default), "linked", "full".
	// "auto": linked clone for snapshot-capable backends (dir, nfs, cifs,
	// zfspool, lvmthin, rbd, cephfs); full clone for lvm-thick (linked
	// not supported). "linked": force linked clone; error on lvm-thick.
	// "full": force full clone on all backends.
	// ApplyDefaults treats empty string as "auto". omit from ERB when empty.
	CloneMode string `json:"clone_mode,omitempty"`

	// RootDiskBus selects the PVE bus the root (system) disk is created on.
	// "virtio" (default when empty): root disk lands on virtio0 — byte-identical
	// to every release before this property existed. "scsi": root disk lands on
	// scsi0 under the same virtio-scsi controller as persistent disks, which
	// unlocks TRIM (discard) and ssd auto-resolution on the root disk itself —
	// both are unavailable on virtio-blk. Use RootDiskBusValue()/RootDiskUsesSCSI();
	// empty maps to "virtio", never mutated by ApplyDefaults. validate-only-when-set;
	// omit from ERB when empty.
	//
	// The clone path (the dominant path — every VM created from a
	// "template:<vmid>" stemcell CID clones a pre-built template) requires the
	// source template to carry a root disk on the same bus this resolves to.
	// A stemcell template is built once and reused by sha8-tag content match;
	// flipping this setting does not retroactively rebuild existing templates.
	// create_vm detects a bus mismatch between the resolved setting and the
	// template's actual root disk key and fails fast with a non-retriable error
	// naming the conflict, rather than silently producing a virtio root under a
	// "scsi" setting (or vice versa). Re-run create_stemcell for the affected
	// stemcell(s) after changing this value so new templates are built on the
	// matching bus. See docs/configuration.md.
	RootDiskBus string `json:"root_disk_bus,omitempty"`

	// Placement is an optional nested block that controls availability-aware
	// node selection and anti-affinity at create_vm time. When nil (field absent
	// from JSON), all placement behavior defaults to safe defaults via accessors.
	// When present, sub-fields follow the pointer-optional pattern: nil = absent
	// = use default; explicit value overrides.
	Placement *PlacementConfig `json:"placement,omitempty"`

	// Storage is an optional nested block controlling storage-pool capacity
	// guards that apply across create_vm placement, create_disk, resize_disk,
	// and snapshot_disk — not only create_vm placement, hence its own top-level
	// block rather than living under Placement. When nil (field absent from
	// JSON), all storage-capacity behavior is byte-identical to prior releases.
	Storage *StorageConfig `json:"storage,omitempty"`

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

	// DiskDeleteStateGuard selects whether delete_disk first checks the lock
	// state of the VM that owns the target volume. When empty or "on" (the
	// default), delete_disk resolves the owning guest (from the managed
	// volid) and, if that guest holds a destructive/in-flight lock (backup,
	// clone, migrate, snapshot, rollback, create), defers the delete with a
	// retriable error so the BOSH Director re-drives it after the operation
	// completes — this closes the race window against nightly vzdump/PBS backups
	// and other in-flight operations. Set "off" to restore the earlier unguarded
	// behavior (no owner lookup). The guard is best-effort and fails open on any
	// resolution uncertainty, so the worst case of leaving it on its new default
	// is a delayed delete during a backup window, never a hard failure. Enum:
	// ""|"off"|"on". Use DiskDeleteStateGuardEnabled() for the effective value.
	DiskDeleteStateGuard string `json:"disk_delete_state_guard,omitempty"`

	// DetachedDiskStrategy selects the lifecycle strategy for persistent disks
	// in the detached state (i.e. after detach_disk and before the next
	// attach_disk or delete_disk). Valid values:
	//
	//   ""       — same as "parked" (the default)
	//   "parked" — detached disks are attached to a dedicated parker VM
	//              (bosh-parker-<n>) in an active scsi slot (scsi0..30) with
	//              protection=1 and onboot=0. The parker VM is never started.
	//              Provides PVE-side ownership visibility and accident protection
	//              at the cost of slightly higher op counts per detach/attach.
	//              Default.
	//   "free"   — opt out of parking: newly detached disks float as un-attached
	//              volumes on PVE storage. PVE has no first-class volume object,
	//              so the disk is visible only via its synthetic-VMID container
	//              VM's config. Risk: administrators may delete the container VM
	//              thinking it is unused. The parker band still resolves and the
	//              holder scans still run, so disks parked while "parked" was in
	//              effect unpark on their next attach_disk or delete_disk.
	//
	// Use DetachedDiskStrategyValue() for the effective normalized value.
	// Use DetachedDiskParkedEnabled() to gate parker logic.
	DetachedDiskStrategy string `json:"detached_disk_strategy,omitempty"`

	// ParkedAnchorStrict controls the anchor-missing invariant of the parked
	// strategy. A disk whose CID envelope promises a parker anchor
	// (created under "parked") should always be held by a parker VM while
	// detached; when the holder scan finds no holder at all, or a parker the
	// scan identified vanishes before it can be read or unparked, the anchor
	// is missing — a parker VM was deleted out-of-band. Strict (nil or true,
	// the default) refuses the operation with a Cloud error naming the
	// recovery; false restores the permissive behavior of treating the volume
	// as free-floating, for labs that intentionally delete parkers. Disks
	// without the envelope promise (legacy, or created under "free") are
	// always permissive — their anchor was never promised.
	// Use ParkedAnchorStrictValue() for the effective value. Omit from ERB
	// when unset.
	ParkedAnchorStrict *bool `json:"parked_anchor_strict,omitempty"`

	// DiskMigration selects whether attach_disk may move a persistent disk
	// between cluster nodes when the disk and the target VM are not
	// co-located. Valid values:
	//
	//   ""          — same as "on_attach" (the default)
	//   "on_attach" — when a stable-ID disk sits on one node and the VM runs
	//                 on another, attach_disk isolates the disk onto a fresh
	//                 single-purpose mover parker VM on the disk's node,
	//                 offline-migrates the never-started mover to the VM's
	//                 node through the PVE migrate API (a metadata move on
	//                 shared storage, a volume copy on node-local storage),
	//                 attaches from the mover, and destroys the now-empty
	//                 mover. Default: a stranded disk moves instead of
	//                 erroring.
	//   "off"       — restore the hard cross-node errors: attach_disk refuses
	//                 a local-backend disk on another node and a parked disk
	//                 whose parker sits on another node, naming this knob in
	//                 the message.
	//
	// Legacy disks (no stable ID in the CID) are never migrated regardless of
	// this setting: the migration renames the volume, and a legacy CID is the
	// volume name. Disks on an OFFLINE node cannot migrate either — PVE needs
	// the source node online to run the migration task.
	//
	// Use DiskMigrationValue() for the effective normalized value and
	// DiskMigrationOnAttachEnabled() to gate the migration path.
	DiskMigration string `json:"disk_migration,omitempty"`

	// DiskCIDCompression is retained for compatibility and no longer gates
	// anything: create_disk always falls back to the pvz- compressed disk CID
	// format when the standard pvd- envelope would exceed 255 characters (the
	// varchar(255) disk_cid column of MySQL-backed Directors). CIDs that fit
	// are emitted as pvd- unchanged, and decode accepts every format
	// unconditionally. No validation needed (any bool is valid); omit from ERB
	// when false.
	DiskCIDCompression bool `json:"disk_cid_compression,omitempty"`

	// ParkedDiskVMIDRangeStart is the inclusive lower bound of the VMID range
	// reserved for parker VMs (bosh-parker-<n>). Parker VMs occupy this band;
	// each parker VM holds up to 31 parked disk volumes in scsi0..30 slots.
	// ApplyDefaults sets to 90000 when zero, under both strategies: under
	// "parked" the band is where parker VMs are allocated, and under "free" it
	// is read-only, letting the holder scans recognize and unpark disks parked
	// earlier. When set explicitly under "parked", the band must not overlap
	// the VM range, the persistent-disk range, or the stemcell-template range.
	// Omit from ERB when zero.
	ParkedDiskVMIDRangeStart int `json:"parked_disk_vmid_range_start,omitempty"`

	// ParkedDiskVMIDRangeEnd is the inclusive upper bound of the VMID range for
	// parker VMs. Must be > ParkedDiskVMIDRangeStart. ApplyDefaults sets to
	// 90999 when zero (companion to ParkedDiskVMIDRangeStart default fill).
	// Under "parked", must not overlap any other VMID band. Omit from ERB when
	// zero.
	ParkedDiskVMIDRangeEnd int `json:"parked_disk_vmid_range_end,omitempty"`

	// parkedDefaultBandCollision records why the DEFAULTED parked strategy is
	// not in force for this config: it names the configured VMID band the
	// built-in parker band 90000-90999 would have overlapped. Unexported and
	// untagged, so it is neither decoded from nor emitted to cpi.json; Load
	// reads it to warn, and it survives the shallow copy ApplyContextOverrides
	// makes. Empty means no collision was detected.
	parkedDefaultBandCollision string

	// parkerStartDefaulted and parkerEndDefaulted record, per bound, that
	// ApplyDefaults rather than the operator put the value in the range field.
	// Same unexported/untagged treatment as parkedDefaultBandCollision.
	// ApplyContextOverrides needs them to tell a defaulted bound from one an
	// operator typed: only a defaulted bound may be cleared when a per-entry
	// override arrives, and only a fully defaulted band may stand down when an
	// entry moves another band on top of it.
	//
	// Per bound rather than per band, because the two are set independently. A
	// job-level config naming only a start gets its end derived here; if that
	// derived end survived into an entry that names its own start, the entry
	// would silently inherit a partner it never described -- the very thing
	// effectiveParkerBand's same-width rule exists to prevent.
	parkerStartDefaulted bool
	parkerEndDefaulted   bool

	// NetworkResolveRetries bounds eventual-consistency polling for freshly
	// created SDN networks. A newly applied SDN vnet is not immediately usable
	// cluster-wide: the data-plane realization (ifupdown2 reload, pmxcfs
	// propagation) is asynchronous and per-node — SDN state propagates over
	// inter-node SSH, so one broken node leaves changes silently pending
	// cluster-wide while the apply task still reports success. When > 0,
	// create_network polls the running cluster SDN config until the new vnet
	// converges, and create_vm confirms each SDN-managed NIC bridge is present
	// on the target node before attaching, classifying a not-yet-present bridge
	// as retriable so the BOSH Director re-drives rather than booting a NIC into
	// a bridge that does not yet exist. Both gates are scoped to SDN vnets only
	// (external/static bridges such as vmbr0 are never gated) and fail open on
	// SDN-membership lookup errors, so the worst case of leaving this enabled is
	// a bounded retriable delay, never a false block on a legitimate bridge.
	// *int (not int) because this must distinguish "left unset"
	// (nil → defaults to 30, ~30s at the 1s poll cadence — enabled by default)
	// from "explicitly set to 0" (disables both gates, restoring the
	// earlier ungated behavior). Validate >= 0 when set. Use
	// NetworkResolveRetriesValue()/NetworkResolveEnabled().
	NetworkResolveRetries *int `json:"network_resolve_retries,omitempty"`

	// NetworkResolveTimeoutSec is the companion absolute bound on the SDN
	// eventual-consistency poll described on NetworkResolveRetries: the poll stops
	// once this many seconds have elapsed even if the retry budget is not yet
	// spent. Only meaningful when NetworkResolveRetries > 0; 0 resolves to 60s.
	// Validate >= 0 when set. Use NetworkResolveTimeoutSecValue().
	NetworkResolveTimeoutSec int `json:"network_resolve_timeout_sec,omitempty"`

	// DiskPerfInvariantMode controls enforcement of creation-time disk-performance
	// invariants at attach_disk time. The structural options cache, iothread, and
	// ssd are baked into the disk CID at create_disk time (§7.9). On re-attach the
	// CPI merges global disk_performance defaults over the CID-recorded options; if
	// global config has since introduced a structural option the disk did not have
	// at creation, the disk's runtime profile would silently diverge from its
	// recorded one. This knob governs that case:
	//
	//	enforce (default) — reject the attach with a non-retriable CloudError
	//	warn              — log the divergence and proceed with the merged options
	//	off               — skip the check entirely
	//
	// Throttle options (mbps_*, iops_*) and discard are NOT invariants — PVE can
	// change them on a live device — so they are never enforced. The check is a
	// no-op for any disk whose CID carries no performance options (bare/legacy
	// CIDs), so behavior is byte-identical unless §7.9 options were recorded.
	// Enum: ""|"enforce"|"warn"|"off"; empty resolves to "enforce". Use
	// DiskPerfInvariantModeValue() for the effective value.
	DiskPerfInvariantMode string `json:"disk_perf_invariant_mode,omitempty"`

	// EphemeralDiskMinRatio is the opt-in floor for a dedicated ephemeral disk's
	// size relative to VM RAM: when set and create_vm provisions a dedicated
	// ephemeral disk, the disk must be at least ratio × RAM (GiB). The BOSH agent
	// lays a RAM-sized swap file plus /var/vcap/data on the ephemeral disk, so an
	// ephemeral disk below ~2× RAM cannot satisfy the agent's own layout. The
	// conventional value is 2. 0 (the default) disables the check entirely, so
	// behavior is byte-identical when unset. The check is also a no-op when no
	// dedicated ephemeral disk is requested (the agent carves ephemeral storage
	// from the grown root disk). See EphemeralDiskMinMode for enforce vs warn.
	EphemeralDiskMinRatio float64 `json:"ephemeral_disk_min_ratio,omitempty"`

	// EphemeralDiskMinMode selects the action when the EphemeralDiskMinRatio
	// invariant is violated: enforce (default) → reject create_vm with a
	// non-retriable CloudError naming the deficit; warn → log and proceed. It has
	// no effect unless EphemeralDiskMinRatio is set. Enum: ""|"enforce"|"warn";
	// empty resolves to "enforce". Use EphemeralDiskMinModeValue() for the
	// effective value.
	EphemeralDiskMinMode string `json:"ephemeral_disk_min_mode,omitempty"`

	// ClusterLockMode selects the cross-process cluster mutex used to serialize
	// the read-modify-write on a shared HA anti-affinity rule. Two concurrent
	// create_vm invocations for the same instance group both read the old member
	// set and recreate the rule (PVE rules have no partial edit); the last writer
	// wins, silently dropping a member. When "pool", the CPI acquires a sentinel
	// resource pool (POST /pools is pmxcfs-serialized, create-or-fail) keyed on the
	// group name around that RMW. When empty or "off" (default) no lock is taken
	// and behavior is byte-identical to prior releases. Enum: ""|"off"|"pool".
	// Use ClusterLockMode()/ClusterLockEnabled().
	ClusterLock string `json:"cluster_lock_mode,omitempty"`

	// ClusterLockTimeoutSec bounds how long the anti-affinity RMW waits to acquire
	// the cluster lock before returning a retriable error (the BOSH director then
	// re-drives the operation). It also serves as the lock's TTL: a holder whose
	// recorded expiry has passed is treated as crashed and its lock is stolen. Only
	// meaningful when ClusterLockMode is "pool"; 0 resolves to 60s. Validate >= 0
	// when set. Use ClusterLockTimeoutSecValue().
	ClusterLockTimeoutSec int `json:"cluster_lock_timeout_sec,omitempty"`

	// AntiAffinityVerify enables a read-after-write check: after recreating a
	// bosh-aa-<group> rule the CPI re-lists the HA rules and asserts the target
	// VMID is present in the rule's members. A concurrent writer that dropped the
	// member surfaces as a retriable error rather than silent loss of spread.
	// Pointer-typed so an explicit false survives JSON omission; nil/false
	// (default) is byte-identical. Use AntiAffinityVerifyEnabled(). Omit from ERB
	// when nil; emit only when true.
	AntiAffinityVerify *bool `json:"antiaffinity_verify,omitempty"`

	// TaskPollAdaptive enables progress-aware adaptive task polling (§7.28). When
	// true, AwaitTask derives the poll interval from a PVE task's reported
	// progress (clamped 1–10s) for long operations (clone, move-disk), falling
	// back to the fixed task_poll cadence when progress is absent. Default false
	// (nil → disabled): polling is byte-identical to prior releases. Use
	// TaskPollAdaptiveEnabled().
	TaskPollAdaptive *bool `json:"task_poll_adaptive,omitempty"`

	// RedactLogs enables a debug-level redacted request/response trace at the
	// dispatcher boundary (§7.41). When true, each CPI call's argument tree and
	// result are logged at debug level with credentials masked by
	// log.RedactSecrets (mbus URL userinfo, blobstore secret_access_key/password,
	// registry credentials, and any other sensitive-named key). When nil/false
	// (default), no payload trace is emitted and logging is byte-identical to
	// prior releases. Pointer-typed so an explicit false survives JSON omission.
	// Use RedactLogsEnabled() for the effective bool. Omit from ERB when nil;
	// emit only when true.
	RedactLogs *bool `json:"redact_logs,omitempty"`

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

	// LBRegister configures the lb_register hook (HAProxy Data Plane API target).
	// Required only when "lb_register" is listed in Hooks; nil otherwise. The
	// config struct lives in internal/cpi/hooks to avoid an import cycle
	// (internal/config already imports internal/cpi/hooks for name validation).
	LBRegister *hooks.LBRegisterConfig `json:"lb_register,omitempty"`

	// ExternalCommand configures the external_command hook (allowlisted host
	// command). Required only when "external_command" is listed in Hooks; nil
	// otherwise.
	ExternalCommand *hooks.ExternalCommandConfig `json:"external_command,omitempty"`

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

	// Debug holds optional diagnostic knobs that change CPI behavior to aid
	// post-mortem investigation. Nil (the default) means all diagnostic modes
	// are off — behavior is byte-identical to prior releases. Use the typed
	// accessors (e.g. KeepFailedVMsEnabled).
	Debug *DebugConfig `json:"debug,omitempty"`

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

	// MaxInflightPerNode caps the number of concurrently outstanding mutating PVE
	// calls per node. When 0 (the default), no cap is applied and behavior is
	// byte-identical to prior releases. When > 0, a per-node bounded semaphore
	// limits concurrent outstanding calls in the five mutating handlers
	// (create_vm, delete_vm, create_disk, attach_disk, create_stemcell).
	// The semaphore is process-scoped and sized on first acquisition for a given
	// node; the limit is process-stable (restart to resize). Must be >= 0; negative
	// values are rejected at config validation. validate-only-when-set;
	// omit from ERB when zero.
	MaxInflightPerNode int `json:"max_inflight_per_node,omitempty"`

	// PVEDialTimeoutSec bounds the TCP dial step of every PVE API HTTP request.
	// 0 (default) leaves the transport at the SDK default (no explicit dial
	// timeout). When > 0, the TCP dial is cancelled if it has not completed within
	// this many seconds. Valid range: >= 0; negative values are rejected.
	// validate-only-when-set; omit from ERB when zero.
	PVEDialTimeoutSec int `json:"pve_api_dial_timeout_sec,omitempty"`

	// PVETLSHandshakeTimeoutSec bounds the TLS handshake step of every PVE API
	// HTTPS request. 0 (default) leaves the transport at the SDK default (no
	// explicit handshake timeout). When > 0, the handshake is cancelled if it
	// has not completed within this many seconds. Useful on high-latency paths.
	// validate-only-when-set; omit from ERB when zero.
	PVETLSHandshakeTimeoutSec int `json:"pve_api_tls_handshake_timeout_sec,omitempty"`

	// PVEMaxIdleConnsPerHost sets the maximum number of idle (keep-alive)
	// connections per PVE host in the transport pool. 0 (default) falls back to
	// the SDK default (KeepAlive value). Values > 0 cap the pool. Higher values
	// reduce connection-setup latency under burst load; lower values conserve
	// file descriptors on constrained CPI hosts. validate-only-when-set;
	// omit from ERB when zero.
	PVEMaxIdleConnsPerHost int `json:"pve_api_max_idle_conns_per_host,omitempty"`

	// PVEIdleConnTimeoutSec bounds how long an idle keep-alive connection stays
	// in the transport pool before being closed. 0 (default) leaves the transport
	// at the SDK default. Shorter values free sockets sooner on clusters with
	// infrequent CPI activity; longer values retain warmed connections across
	// calls. validate-only-when-set; omit from ERB when zero.
	PVEIdleConnTimeoutSec int `json:"pve_api_idle_conn_timeout_sec,omitempty"`

	// PVETCPKeepAliveSec sets the TCP keep-alive probe interval for PVE API
	// connections. 0 (default) leaves the transport at the Go default. A positive
	// value enables periodic TCP keep-alive probes at this interval in seconds,
	// which helps detect silently-dropped connections on stateful firewalls between
	// the CPI host and the PVE API. validate-only-when-set; omit from ERB when zero.
	PVETCPKeepAliveSec int `json:"pve_api_tcp_keepalive_sec,omitempty"`

	// StrictConfigValidation enables fail-fast config validation. When nil or
	// *false (the default), unknown top-level keys produce a Warn log and
	// inconsistent cross-field combinations are tolerated — byte-identical to
	// prior releases. When *true, the same conditions become hard CloudErrors
	// that abort startup. Pointer-typed so nil (field absent from JSON) is
	// distinguishable from an explicit false. Use StrictConfigValidationEnabled()
	// to obtain the effective bool. Validate-only-when-set; omit from ERB when nil.
	StrictConfigValidation *bool `json:"strict_config_validation,omitempty"`

	// FastPathDelete enables opt-in fast-path delete for delete_vm and delete_disk.
	// When nil or *false (the default), delete_vm and delete_disk issue the destroy
	// and await the PVE task until the resource is confirmed gone — fully synchronous
	// and consistent. When *true, the handlers tag the VM "bosh-deleting" (best-effort,
	// fail-open), issue the destroy call, and return without polling the task's terminal
	// state. This eliminates the queue-slot hazard described in §7.15 for the delete
	// path at the cost of eventual consistency: a subsequent has_vm/has_disk may briefly
	// still see the resource until the async destroy completes. The §7.13 orphan-GC sweep
	// reaps any VM whose async destroy never completes.
	//
	// Default false (nil → false via FastPathDeleteEnabled). Pointer-typed so nil (absent
	// from JSON) is distinguishable from an explicit false. Validate-only-when-set; omit
	// from ERB when nil.
	FastPathDelete *bool `json:"fast_path_delete,omitempty"`

	// StemcellReplicationConcurrency controls how many nodes receive a stemcell
	// replica upload concurrently inside replicateStemcellToNodes. Only meaningful
	// when StemcellReplicateLocal is true. Values:
	//
	//   0 or absent — defaults to 1 (serial, byte-identical to prior releases).
	//   1           — serial: one node at a time, deterministic order.
	//   N > 1       — up to N nodes replicated in parallel; each node's semantics
	//                 (idempotent skip, best-effort failure, in-flight gate) are
	//                 preserved independently per goroutine.
	//
	// Negative values and values > 64 are rejected at config validation.
	// Use StemcellReplicationConcurrencyValue() to obtain the effective worker count.
	// validate-only-when-set; omit from ERB when zero (serial default).
	StemcellReplicationConcurrency int `json:"stemcell_replication_concurrency,omitempty"`

	// Encrypted is the global opt-in for encrypted-storage disk placement (§7.49).
	// When *true, create_disk and ephemeral disk creation restrict storage-tier
	// selection to tiers that have Encrypted:*true in config.StorageTiers. A
	// per-call cloud_properties.encrypted overrides this global (per-call > global).
	// When nil or *false (default), no encrypted filter is applied and behavior is
	// byte-identical to prior releases. Pointer-typed so nil (absent from JSON) is
	// distinguishable from an explicit false. Use EncryptedEnabled() for the
	// effective bool. validate-only-when-set; omit from ERB when nil.
	Encrypted *bool `json:"encrypted,omitempty"`

	// Metrics holds the opt-in per-RPC metrics file configuration. When nil (the
	// default), or when Enabled is false, the MetricsHook is never registered and
	// adds no dispatch-path overhead — byte-identical to prior releases. When
	// Enabled is true, each CPI RPC appends one JSON-line sample (ts, method,
	// duration_ms, outcome, request_id) to FilePath. Write failures are logged at
	// Warn level and never fail the CPI call. FilePath is required when Enabled is
	// true; config validation fails with a clear error when absent.
	// The config struct lives in internal/cpi/hooks (not here) to avoid the
	// import cycle: this package already imports internal/cpi/hooks for hook-name
	// validation, so this package must not import a type that would cause hooks to
	// import back here. validate-only-when-set; omit from ERB when nil/disabled.
	Metrics *hooks.MetricsConfig `json:"metrics,omitempty"`

	// OTel holds the opt-in OpenTelemetry tracing configuration. Value-typed
	// (not pointer) because OTel.Enabled's default is false: a zero-value block
	// is unambiguous with "tracing off", so there is no nil-vs-explicit-false
	// case to disambiguate (unlike blocks such as Placement whose default is
	// true). Zero value emits zero exporter setup, zero network activity, and
	// zero validation errors — byte-identical to prior releases.
	// validate-only-when-set; omit from ERB when disabled. Use OTelEnabled()
	// for the effective bool.
	OTel OTelConfig `json:"otel"`
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
// (nil). Encrypted, when non-nil, restricts selection to tiers that match the
// encrypted predicate (see §7.49). At least one of Types, Shared, or Encrypted
// must be set.
type StorageTierCriteria struct {
	Types     []string `json:"types,omitempty"`
	Shared    *bool    `json:"shared,omitempty"`
	Encrypted *bool    `json:"encrypted,omitempty"`
}

// DiskPerformanceDefaults holds optional global PVE per-disk performance option
// defaults applied when a create_disk/create_vm cloud_properties (or vm_type/
// disk_type profile) does not set the corresponding option. All fields optional;
// a nil block (default) preserves each field's own built-in default when the
// resolver and this block both leave it unset — see
// internal/cpi/handlers/disk_performance.go for the effective defaults:
// Iothread and VirtioSCSISingle default true; Discard and SSD default to a
// computed per-disk TRIM-capability auto-resolution (see pve.IsTrimCapable),
// not a fixed constant; every other field defaults to its Go zero value
// (unset/no limit).
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
	// AIO selects the PVE AsyncIO backend for every disk created by this CPI:
	// "native", "io_uring", or "threads". Empty (default) omits the key and
	// leaves PVE's own default in effect. Overridden per disk by
	// cloud_properties.aio. Structural (invariant-tracked on re-attach, same
	// as cache/iothread/ssd) — see diskPerfInvariantKeys.
	AIO string `json:"aio,omitempty"`
}

// StemcellProvenanceConfig holds optional stemcell orphan-pruning knobs.
// Provenance itself is no longer a knob: every stemcell template is stamped
// unconditionally with tags and notes JSON (name/version/sha256/kind/cid/
// director refs), and director identity comes from the JSON-RPC request
// context (Deps.RequestDirectorUUID), not config. All fields are optional; a
// nil block (default) emits nothing. Accessors on *CPIConfig handle nil
// blocks safely so callers never dereference this pointer directly.
type StemcellProvenanceConfig struct {
	// PruneOrphans enables pruning of stemcell templates tagged by the
	// requesting director that no longer have a referencing linked clone.
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

	// Pushback governs the backoff used when PVE returns HTTP 429 or a
	// worker-busy / lock-acquire-timeout signal. The PushbackBackoff curve
	// (5s/60s) is longer than StorageLock (2s/30s) to match PVE's slower
	// worker-pool drain. max_attempts caps retries; 0 = class default.
	// Defaults: max_attempts per-handler default, base_ms 5000, cap_ms 60000.
	Pushback *RetryPolicy `json:"pushback,omitempty"`

	// Transient governs the attempt budget for transport-layer retries
	// (RetryOnTransient and friends): pvedaemon worker recycling, connection
	// refusals, timeouts. Only max_attempts is consulted; the backoff curve is
	// the fixed TransientBackoff (1s..15s, ±30% jitter), tuned to pvedaemon's
	// sub-second worker restart, and base_ms/cap_ms/jitter_pct are ignored.
	// Default: max_attempts 0 → pve.DefaultTransientMaxAttempts (8).
	Transient *RetryPolicy `json:"transient,omitempty"`

	// StorageLock governs the exponential backoff used between attempts inside
	// RetryOnTransientOrLock when PVE responds with a "got timeout waiting for
	// worker" or "storage locked" signal. This is a
	// shorter curve than StorageImport because it guards the inner per-operation
	// lock, not the full import pipeline. max_attempts overrides
	// pve.DefaultStorageLockMaxAttempts (10) when > 0; 0 defers to that constant.
	// Defaults: max_attempts 0 (→ DefaultStorageLockMaxAttempts), base_ms 2000,
	// cap_ms 30000, jitter_pct 30.
	StorageLock *RetryPolicy `json:"storage_lock,omitempty"`

	// DiskMigrate governs the cross-node offline migration attach_disk runs
	// when pve.disk_migration resolves to "on_attach" and a persistent disk
	// must follow its VM to another node. Only two fields are consulted:
	// max_attempts bounds transient retries of the migrate request itself
	// (0 → pve.DefaultDiskMigrateMaxAttempts, 4), and cap_ms is the
	// wall-clock budget in milliseconds for awaiting the PVE migrate task
	// (0 → 1800000, 30 minutes). A volume copy that outlives the budget
	// surfaces as a retriable error while the copy continues server-side;
	// the Director's retried attach re-enters the migration and completes
	// once it lands. base_ms and jitter_pct are ignored (the task-poll
	// cadence comes from retry.task_poll).
	DiskMigrate *RetryPolicy `json:"disk_migrate,omitempty"`
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

// OTelConfig holds the opt-in OpenTelemetry configuration for the traces,
// logs, and metrics signals. Trace fields are honored only when Enabled is
// true; logs/metrics fields are honored only when LogsEnabled/MetricsEnabled
// are true respectively (validate-only-when-set, independent per signal). A
// zero-value block is equivalent to all signals off and adds zero overhead —
// no exporter is constructed and no network dial is attempted. Protocol
// selects OTLP http/protobuf or OTLP gRPC uniformly across whichever signals
// are enabled.
type OTelConfig struct {
	// Enabled turns on tracing. Default false. Plain bool (not pointer)
	// because the default IS false, so there is no unset-vs-explicit-false
	// ambiguity to resolve (unlike fields whose default is true, e.g.
	// PlacementConfig.Enabled).
	Enabled bool `json:"enabled,omitempty"`

	// ExporterEndpoint is the OTLP http/protobuf collector endpoint
	// (host:port or full URL). Required when Enabled is true. No default is
	// applied — there is no sane collector address to assume.
	ExporterEndpoint string `json:"exporter_endpoint,omitempty"`

	// Insecure disables TLS on the exporter connection. Default false
	// (TLS on).
	Insecure bool `json:"insecure,omitempty"`

	// ServiceName is the OTel resource service.name attribute. Empty means
	// "use default"; ApplyDefaults fills "bosh-pve-cpi" when Enabled is true
	// and this field is empty. An explicit non-empty override is preserved.
	ServiceName string `json:"service_name,omitempty"`

	// SampleRatio is the trace sampling ratio, valid range [0.0, 1.0]. Zero
	// means "use default" (mirrors the repo's existing float64-field
	// convention, e.g. EphemeralDiskMinRatio: zero is indistinguishable from
	// unset, so an explicit sample ratio of exactly 0.0 — "sample nothing" —
	// is not an expressible configuration). ApplyDefaults fills 1.0 when
	// Enabled is true and this field is zero.
	SampleRatio float64 `json:"sample_ratio,omitempty"`

	// ExportTimeoutMs bounds the force-flush deadline applied to the OTel
	// TracerProvider Shutdown call on every CPI process exit path. Zero
	// means "use default"; ApplyDefaults fills 5000 when Enabled is true and
	// this field is zero.
	ExportTimeoutMs int `json:"export_timeout_ms,omitempty"`

	// Protocol selects the OTLP exporter wire protocol ("http" or "grpc"),
	// applied uniformly to whichever signals (traces/logs/metrics) are
	// enabled. Empty means "use default"; ApplyDefaults fills "http" when
	// any signal is enabled and this field is empty.
	Protocol string `json:"protocol,omitempty"`

	// LogsEnabled turns on the OTel logs signal. Default false. Independent
	// of Enabled (traces) — a deployment may export logs without traces.
	LogsEnabled bool `json:"logs_enabled,omitempty"`

	// MetricsEnabled turns on the OTel metrics signal. Default false.
	// Independent of Enabled (traces) and LogsEnabled.
	MetricsEnabled bool `json:"metrics_enabled,omitempty"`

	// LogsExporterEndpoint is the OTLP collector endpoint for the logs
	// signal. Empty means "use default"; ApplyDefaults fills it from
	// ExporterEndpoint when LogsEnabled is true and this field is empty.
	LogsExporterEndpoint string `json:"logs_exporter_endpoint,omitempty"`

	// MetricsExporterEndpoint is the OTLP collector endpoint for the
	// metrics signal. Empty means "use default"; ApplyDefaults fills it
	// from ExporterEndpoint when MetricsEnabled is true and this field is
	// empty.
	MetricsExporterEndpoint string `json:"metrics_exporter_endpoint,omitempty"`
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

	// PinAZViaHARules, when true and an AZMap is set, makes create_vm write a PVE
	// HA node-affinity rule binding the VM to its AZ's node set after scoring, so
	// the AZ placement is durable across HA failover and DLB rebalance (scoring
	// alone only pins at birth). delete_vm removes the rule. Default false
	// (opt-in); nil/absent means no HA pin and zero regression. Use
	// HANodeAffinityPinEnabled().
	PinAZViaHARules *bool `json:"pin_az_via_ha_rules,omitempty"`

	// PinAZStrict controls the strictness of the node-affinity rule created by
	// PinAZViaHARules. Default true (nil → true): a strict rule is a hard AZ
	// guarantee — HA will not relocate the VM off its AZ node set even if every
	// AZ node is down (durability of locality over availability). Set false for a
	// non-strict (preferred) pin that lets HA relocate off-AZ on total AZ
	// failure. Use PinAZStrict().
	PinAZStrict *bool `json:"pin_az_strict,omitempty"`

	// FallbackMax controls post-selection fallback placement. When a clone or VM
	// start fails transiently after node selection, the CPI cleans up the failed
	// attempt and retries on the next-ranked candidate from the same scored set,
	// up to FallbackMax alternates (so at most 1+FallbackMax total attempts).
	//
	// Default 0 (nil or zero): disabled. The handler makes a single attempt on
	// the winner and propagates any error without fallback — byte-identical to
	// pre-feature behavior. Recommended operational value: 2.
	//
	// Valid range: 0–5. Negative values and values >5 are rejected by Validate.
	// Fallback only applies when placement scoring is active (PlacementEnabled).
	// Transient vs permanent failure classification uses the same pve classifiers
	// as the existing intra-attempt retry (IsTransientTransport, IsStorageLockTimeout,
	// IsVMIDConflict for clone; IsTransientTransport for start). Permanent errors
	// (IsCloneSourceMissing, any non-transient error) surface immediately without
	// consuming alternates. Use PlacementFallbackMaxValue() for the effective int.
	FallbackMax *int `json:"fallback_max,omitempty"`

	// ReserveStorageHeadroom enables the storage-capacity hard placement filter
	// (§7.58). Default false (nil or absent → false). When true, create_vm
	// computes a RequiredStorageBytes floor for each placement.Request:
	//
	//   floor = rootDiskBytes + ephemeralDiskBytes (when on the same pool) +
	//           headroomBytes
	//
	// where headroomBytes = StorageHeadroomMB (MiB→bytes) plus VM RAM
	// (MiB→bytes) as a swap reservation when a dedicated ephemeral disk is
	// present (mirroring vSphere's max-swapfile term). When StorageHeadroomMB is
	// unset, 1 GiB is used as the floor margin (matches vSphere DISK_HEADROOM).
	//
	// Only storage counts that land on the VM storage pool (deps.Config.VMStorage)
	// are included; if the ephemeral disk resolves to a different pool those bytes
	// are excluded from the filter. The filter is fail-open: when a node's storage
	// facts are unavailable (TotalStorageBytes == 0), that node is NOT rejected.
	//
	// Default false (opt-in): when nil or absent, RequiredStorageBytes stays 0 and
	// placement is byte-identical to prior releases. Use
	// ReserveStorageHeadroomEnabled() for the effective bool.
	ReserveStorageHeadroom *bool `json:"reserve_storage_headroom,omitempty"`

	// StorageHeadroomMB is the extra margin in MiB added on top of the computed
	// disk footprint when ReserveStorageHeadroom is true. Acts as the vSphere
	// DISK_HEADROOM equivalent: an absolute safety buffer beyond the raw disk
	// sizes. When 0 (nil or absent), defaults to 1024 MiB (1 GiB). Negative
	// values are rejected by Validate. Only meaningful when
	// ReserveStorageHeadroom is true. Use StorageHeadroomMBValue() for the
	// effective int (always ≥ 0 when valid).
	StorageHeadroomMB *int `json:"storage_headroom_mb,omitempty"`

	// MemorySignal selects which live memory signal the placement scorer's Mem
	// axis uses to rank candidate nodes:
	//
	//   "reserved" (default) — the Mem axis uses reservedFree =
	//     (TotalMemBytes − CommittedMemBytes) / TotalMemBytes, clamped to
	//     [0,1]. CommittedMemBytes sums each resident QEMU guest's configured
	//     memory (maxmem), regardless of run state, on the node. This tracks
	//     memory reservations rather than actual host memory in use: a
	//     sequence of freshly-booted BOSH VMs touches only a fraction of its
	//     reserved RAM, so the legacy resident-memory signal barely moves
	//     during a sequential deploy and the deterministic scorer keeps
	//     picking the same node. The reserved signal drops by a full guest's
	//     reservation on every create, so sequential creates fan out across
	//     equal nodes instead of stacking on one.
	//
	//   "resident" — legacy signal: freeMemBytes = Maxmem − Mem from PVE
	//     cluster status (actual host memory currently in use), no clamping.
	//     Byte-identical to pre-feature releases; opt out to this value only
	//     if the reserved signal misbehaves for a workload that deliberately
	//     overcommits guest memory (e.g. ballooning tuned to reclaim idle RAM
	//     aggressively, where committed memory persistently overstates real
	//     pressure).
	//
	// Comparison is case-insensitive with surrounding whitespace trimmed.
	// Empty (default), or any value other than "reserved"/"resident",
	// resolves to "reserved" — this is a fail-open fallback, not a validation
	// error, so a typo never blocks a deploy; it silently keeps the
	// protective new default instead of falling back to the vulnerable
	// legacy behavior. Use MemorySignalValue() for the effective, normalized
	// value.
	MemorySignal string `json:"memory_signal,omitempty"`
}

// StorageConfig holds storage-pool capacity guard knobs that apply across
// create_vm placement, create_disk, resize_disk, and snapshot_disk.
//
// Motivation: copy-on-write pools (qcow2/thin-LVM/ZFS) degrade progressively
// as they fill — noticeably from roughly 50% utilization, badly by roughly
// 80%, and a pool that fills completely can take days to recover. Ceph
// separately enforces nearfull/backfill-full/full watermarks at 85/90/95% by
// default; a CPI-side ceiling set below those watermarks (e.g. 80%) gives an
// early, CPI-level signal before Ceph's own thresholds engage, which matters
// most on small clusters where Ceph's defaults leave little headroom. This is
// independent of, and composes with, the absolute-bytes headroom filter
// (Placement.ReserveStorageHeadroom): the pct ceiling is a proportional
// early-warning band, the headroom filter is a fixed-margin floor.
type StorageConfig struct {
	// MaxUtilizationPct is the ceiling on projected storage-pool utilization,
	// as a percentage of pool capacity (0-100). Zero (nil or absent) disables
	// the gate entirely — byte-identical to prior releases. When positive,
	// four evaluation points check pool utilization AFTER accounting for the
	// operation's disk footprint:
	//
	//   - create_vm placement: candidate nodes whose target vm_storage pool
	//     would exceed the ceiling after adding the VM's disk footprint are
	//     rejected from placement (enforce mode) or logged (warn mode).
	//   - create_disk: the resolved disk pool is checked before allocation.
	//   - resize_disk: the resize delta is checked before the resize call.
	//   - snapshot_disk: Warn-only regardless of MaxUtilizationMode — snapshot
	//     growth is unbounded and cannot be estimated ahead of time, so this
	//     evaluation point only warns when the pool is ALREADY above the
	//     ceiling; it never blocks a snapshot.
	//
	// Computation reuses the storage status the CPI already fetches
	// (used/total bytes from GET /nodes/<node>/storage). When storage facts
	// cannot be determined (API error, pool not found, or pool inactive), the
	// gate fails open (proceeds) and logs a Warn — consistent with the
	// existing absolute-bytes headroom filter's fail-open behavior.
	//
	// Recommended operational value: 80 (stays below Ceph's 85% nearfull
	// watermark with margin, and sits at the edge of the CoW "badly degraded"
	// band). Validate rejects values outside [0, 100]. Use
	// MaxUtilizationPctValue() for the effective int.
	MaxUtilizationPct *int `json:"max_utilization_pct,omitempty"`

	// MaxUtilizationMode selects the enforcement behavior when
	// MaxUtilizationPct is exceeded. Only consulted when MaxUtilizationPct >
	// 0. Enum: ""|"enforce"|"warn"; empty resolves to "enforce".
	//
	//   enforce (default) — create_vm placement rejects the candidate node;
	//                        create_disk and resize_disk return a RETRIABLE
	//                        CloudError naming the pool, current projected
	//                        pct, and the ceiling (capacity can be freed, so
	//                        the director should re-drive rather than wedge
	//                        on a permanent error).
	//   warn              — the same facts are logged at Warn and the
	//                        operation proceeds unblocked.
	//
	// snapshot_disk ignores this setting: it is always Warn-only, regardless
	// of MaxUtilizationMode, because snapshot growth cannot be estimated and
	// blocking snapshots on a capacity guess would be unsound.
	//
	// Use MaxUtilizationEnforce() for the effective bool.
	MaxUtilizationMode string `json:"max_utilization_mode,omitempty"`
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

// DebugConfig holds optional diagnostic knobs. All fields default to off when
// the block or the field is absent, preserving production behavior.
type DebugConfig struct {
	// KeepFailedVMs, when *true, suppresses the create_vm rollback that destroys
	// a VM after a mid-creation failure. Instead the VM is tagged
	// "bosh-create-failed" (plus the deployment/job derived from env) and an
	// error naming the VMID and node is returned, leaving the VM intact for
	// post-mortem. Default false (opt-in). This is destructive of the normal
	// no-orphan guarantee, so it is intended for debugging, not production.
	KeepFailedVMs *bool `json:"keep_failed_vms,omitempty"`
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

	// ExpectedAgentSHA256 is the expected hex SHA-256 of the BOSH agent binary
	// (/var/vcap/bosh/bin/bosh-agent) inside the booted guest. When non-empty
	// and Enabled is true, create_vm runs sha256sum via the QEMU guest agent
	// after the ping succeeds and fails (triggering rollback) only on a CONFIRMED
	// digest mismatch; any inability to verify (guest-agent error, unparseable
	// output) is fail-open. Empty (default) disables the assertion. Must be 64
	// hex characters when set. Use HealthCheckExpectedAgentSHA256().
	ExpectedAgentSHA256 string `json:"expected_agent_sha256,omitempty"`
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

// unknownConfigKeys decodes raw into a flat map and returns the sorted list of
// top-level keys absent from knownConfigFields. Returns nil when the JSON is
// malformed (the main decode path handles that error) or when all keys are
// known. Shared by warnUnknownFields (always-on warn) and the strict-mode
// unknown-key check (hard error when strict is on).
func unknownConfigKeys(raw []byte) []string {
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil
	}
	var unknown []string
	for k := range flat {
		if _, known := knownConfigFields[k]; !known {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	insertionSort(unknown)
	return unknown
}

// rootPamIdentityLiteral is the exact user@realm string PVE requires for the
// skiplock parameter fast_path_delete's destroy calls rely on to reclaim
// locked/running VMs (see internal/cpi/handlers/delete_vm.go and
// vm_lock_recovery.go). Duplicated from internal/pve/identity.go's
// rootPamIdentity rather than imported: internal/pve already imports
// internal/config, so the reverse import would cycle.
const rootPamIdentityLiteral = "root@pam"

// warnFastPathDeleteNonRootIdentity logs a one-shot Warn at config load time
// when fast_path_delete is enabled and the configured PVE identity is not
// exactly the root@pam superuser authenticated via password — the only
// identity PVE honors the skiplock parameter for. Uses the same ad-hoc
// stderr-logger pattern as warnUnknownFields (config load happens before the
// application logger exists, since the logger's own level comes from this
// same config), except the sink is an explicit parameter so tests can
// capture output; production passes os.Stderr (see Load).
//
// API-token authentication ALWAYS warns, even when the token is owned by
// root@pam (e.g. "root@pam!bosh-cpi"): PVE's skiplock check compares the
// full authenticated-user identity, which for a token request always carries
// the "!<token-id>" suffix and therefore never equals the literal "root@pam"
// PVE requires. This does not reuse pve.IsRootPamIdentity — importing it
// would cycle (see rootPamIdentityLiteral above).
//
// Warn, never error: an operator may know something this check cannot see
// (e.g. a proxied auth layer that ultimately reaches PVE as root@pam).
// fast_path_delete stays enabled either way; a wrong prediction here costs
// nothing beyond a needless log line, while a correct one saves a confusing
// PVE-side rejection discovered only at delete time.
func warnFastPathDeleteNonRootIdentity(cfg *CPIConfig, out io.Writer) {
	if !cfg.FastPathDeleteEnabled() {
		return
	}

	var identity string
	switch {
	case cfg.APIToken != "":
		// Log only the "<user>@<realm>!<token-id>" portion — never the
		// "=<uuid>" secret suffix.
		idPart, _, _ := strings.Cut(cfg.APIToken, "=")
		identity = idPart
	case cfg.Password != "":
		identity = cfg.User
		if !strings.Contains(identity, "@") {
			realm := cfg.Realm
			if realm == "" {
				realm = "pam"
			}
			identity += "@" + realm
		}
		if identity == rootPamIdentityLiteral {
			return
		}
	default:
		// Neither auth method configured: validateAuth (elsewhere in this
		// same load) already accumulates a hard error for this case: nothing
		// this diagnostic can usefully add.
		return
	}

	logger, err := log.NewLogger("warn", out)
	if err != nil {
		return
	}
	logger.Warn("config: fast_path_delete is enabled but the configured PVE identity is not the root@pam superuser; "+
		"PVE only honors the skiplock parameter fast_path_delete's destroy calls rely on for that exact identity — "+
		"delete_vm/delete_disk will fall back to PVE's own rejection when a locked or still-running VM needs skiplock "+
		"to be destroyed under this identity",
		log.String("identity", identity),
	)
}

// applyParkerBandDefaults fills each parker bound with its built-in default
// when that bound is zero, matching effectiveParkerBand. The fill is
// unconditional across strategies: under "parked" the band is where new parker
// VMs are allocated, and under "free" it is read-only — it lets the holder
// scans recognize disks parked earlier (bosh-parker tag inside the band) and
// unpark them on the next attach_disk or delete_disk, instead of refusing them
// as stranded. detached_disk_strategy decides whether newly detached disks get
// parked and whether a parker-band overlap is a hard load error (Validate
// checks the overlap only under "parked"). Call it only after the VM, disk,
// and template bands have been defaulted.
//
// It also carries the upgrade guard. When the strategy was DEFAULTED rather
// than asked for, and no band was configured, the built-in band can collide
// with a band the operator widened long before parking existed — a vmid_range
// of 40000-200000 swallows 90000-90999. Validate rejects that overlap, and
// because the CPI is exec'd once per JSON-RPC request the rejection is not a
// deploy-time error: it is every subsequent CPI call failing, with the worst
// case landing mid create-env after the old Director VM has already been
// stopped. Turning a config that worked yesterday into an outage is not an
// acceptable price for a changed default, so the parked default stands down for
// that load and records why. An explicitly requested "parked", or an explicitly
// configured band, still gets the hard error: there the operator asked for the
// thing that collides. The band is filled even when the default stands down:
// overlap validation only runs under "parked", parker classification also
// requires the bosh-parker tag, and a filled band keeps drain working for any
// disk parked before the collision existed.
//
// The stand-down is recorded in parkedDefaultBandCollision rather than written into
// DetachedDiskStrategy, so it stays a decision the CPI took and can be taken
// again -- differently -- for a cpi-config entry whose own bands leave the
// parker band free. See reevaluateParkedDefaultAfterOverrides.
func (c *CPIConfig) applyParkerBandDefaults() {
	strategyDefaulted := strings.TrimSpace(c.DetachedDiskStrategy) == ""
	startUnset := c.ParkedDiskVMIDRangeStart == 0
	endUnset := c.ParkedDiskVMIDRangeEnd == 0
	bandUnset := startUnset && endUnset
	// Recorded before the fill: a bound nobody set is a bound nobody set,
	// whether or not the default it serves is in force. A cpi-config entry
	// re-decides them together.
	c.parkerStartDefaulted = startUnset
	c.parkerEndDefaulted = endUnset
	if strategyDefaulted && bandUnset {
		if collision := c.defaultParkerBandCollision(); collision != "" {
			c.parkedDefaultBandCollision = collision
		}
	}
	// One rule for filling a missing bound, shared with the accessors, so a
	// config that has been through ApplyDefaults and one that has not resolve to
	// the same band.
	c.ParkedDiskVMIDRangeStart, c.ParkedDiskVMIDRangeEnd = c.effectiveParkerBand()
}

// reevaluateParkedDefaultAfterOverrides re-decides the parked default against a
// config a cpi-config entry has just rewritten.
//
// ApplyDefaults decides once, against the job-level bands, whether the built-in
// parker band collides with another band. An entry can invalidate that
// decision in either direction. It can widen vmid_range, disk_vmid_range, or
// stemcell_template_vmid_range over 90000-90999, creating a collision that
// would then reject every request routed to that entry -- and because the CPI
// is exec'd per request, that rejection is an outage for one cluster rather
// than a deploy-time authoring error. It can also go the other way, narrowing
// the bands so a collision the job-level config had no longer exists, in which
// case the entry deserves the default it would have had on its own.
//
// Call it with the band already cleared of anything ApplyDefaults filled in (see
// clearDefaultedParkerBand), so a band still standing here is one somebody typed
// -- this entry, or the job-level config it inherits from. Either way it is
// honored as written: it is a statement about VMID topology, and the stand-down
// has nothing to say about it. (A job-level band the entry did not touch can
// therefore still produce a parker-overlap error attributed to a band the entry
// never named. That is unchanged from before the parked default, where the same
// band was validated the same way.)
//
// Undecided bounds always refill with the built-in band, collision or not,
// matching applyParkerBandDefaults: the stand-down governs only the strategy
// default, while the band stays in force read-only so disks parked earlier
// keep draining through the entry's holder scans.
func (c *CPIConfig) reevaluateParkedDefaultAfterOverrides() {
	if c.ParkedDiskVMIDRangeStart != 0 || c.ParkedDiskVMIDRangeEnd != 0 {
		// The entry named a bound. Whatever the job-level config concluded about
		// the built-in band does not apply to it; the accessors derive whatever
		// partner a single named bound needs, at the same width.
		c.parkerStartDefaulted = false
		c.parkerEndDefaulted = false
		c.parkedDefaultBandCollision = ""
		return
	}
	if !c.parkerStartDefaulted && !c.parkerEndDefaulted {
		// No bound was defaulted and none is set: nothing to re-decide.
		return
	}
	c.parkedDefaultBandCollision = ""
	// The stand-down governs the DEFAULT only. An entry that named a strategy
	// made the decision itself: under "parked" a colliding band is the error the
	// entry asked for, and under "free" no parker is ever created.
	if strings.TrimSpace(c.DetachedDiskStrategy) == "" {
		if collision := c.defaultParkerBandCollision(); collision != "" {
			c.parkedDefaultBandCollision = collision
		}
	}
	c.ParkedDiskVMIDRangeStart = defaultParkerVMIDStart
	c.ParkedDiskVMIDRangeEnd = defaultParkerVMIDEnd
}

// clearDefaultedParkerBand removes a parker band that ApplyDefaults, not the
// operator, put there. ApplyContextOverrides calls it before applying an entry's
// keys so the entry starts from the shape the job-level config had before
// defaulting: otherwise an entry that sets one bound inherits the other from a
// default, and an entry that sets neither cannot be told from one that named the
// built-in band on purpose.
func (c *CPIConfig) clearDefaultedParkerBand() {
	// Per bound: a job-level config that named only a start had its end derived
	// by ApplyDefaults, and carrying that derived end into an entry that names
	// its own start hands the entry a band it never described. Clearing only
	// what was defaulted leaves anything an operator typed exactly as typed.
	if c.parkerStartDefaulted {
		c.ParkedDiskVMIDRangeStart = 0
	}
	if c.parkerEndDefaulted {
		c.ParkedDiskVMIDRangeEnd = 0
	}
}

// ParkedDefaultStoodDown reports the VMID band that made the defaulted parked
// strategy stand down, or "" when parking is in force. Callers use it to
// surface the decision; nothing in the CPI branches on it.
func (c *CPIConfig) ParkedDefaultStoodDown() string {
	if c == nil {
		return ""
	}
	return c.parkedDefaultBandCollision
}

// defaultParkerBandCollision reports which configured VMID band the built-in
// parker band 90000-90999 would overlap, as a human-readable clause, or "" when
// it fits. Call it only after the VM, disk, and template bands have been
// defaulted — it compares against whatever those fields currently hold.
func (c *CPIConfig) defaultParkerBandCollision() string {
	type band struct {
		name       string
		start, end int
	}
	for _, b := range []band{
		{"vmid_range", c.VMIDRangeStart, c.VMIDRangeEnd},
		{"disk_vmid_range", c.DiskVMIDRangeStart, c.DiskVMIDRangeEnd},
		{"stemcell_template_vmid_range", c.StemcellTemplateVMIDRangeStart, c.StemcellTemplateVMIDRangeEnd},
	} {
		if b.start >= b.end {
			continue
		}
		if rangesOverlap(b.start, b.end, defaultParkerVMIDStart, defaultParkerVMIDEnd) {
			return fmt.Sprintf("%s [%d,%d]", b.name, b.start, b.end)
		}
	}
	return ""
}

// warnParkedDefaultBandCollision announces that the defaulted parked strategy
// was stood down for this load because the built-in band collides with a band
// the operator configured. Silent when ApplyDefaults recorded no collision.
//
// The message names the remedy rather than the symptom: an operator who wants
// parking on this cluster has to pick a band that fits their VMID layout, and
// one who does not can pin detached_disk_strategy to "free" to stop the notice.
func warnParkedDefaultBandCollision(cfg *CPIConfig, out io.Writer) {
	if cfg.parkedDefaultBandCollision == "" {
		return
	}
	logger, err := log.NewLogger("warn", out)
	if err != nil {
		return
	}
	logger.Warn("config: the default detached_disk_strategy \"parked\" is standing down for this load: "+
		"its built-in parker band [90000,90999] overlaps the configured "+cfg.parkedDefaultBandCollision+". "+
		"Detached persistent disks stay free-floating for as long as that is true. The band itself remains in "+
		"force read-only, so any disk parked before the overlap existed is still recognized and unparked on its "+
		"next attach_disk or delete_disk. To park disks here, set parked_disk_vmid_range_start/end to a "+
		"1000-wide window that does not overlap any other VMID band. To silence this notice, set "+
		"detached_disk_strategy to \"free\" explicitly",
		log.String("colliding_range", cfg.parkedDefaultBandCollision),
	)
}

// warnUnknownFields decodes raw into a flat map, finds keys absent from
// knownConfigFields, and emits a single Warn entry listing them.
// Uses a stderr logger so the warning surfaces even before the application
// logger is fully initialized. Unknown fields are ignored, not rejected, to
// preserve forward-compatibility when the director sends fields added by
// future CPI versions.
func warnUnknownFields(raw []byte) {
	unknown := unknownConfigKeys(raw)
	if len(unknown) == 0 {
		return
	}
	logger, err := log.NewLogger("warn", os.Stderr)
	if err != nil {
		return
	}
	logger.Warn("config: unknown fields ignored (forward-compat)",
		log.String("fields", strings.Join(unknown, ", ")),
	)
}

// validateStrictUnknownKeys appends an error to errs for each unknown
// top-level key found in raw. Keys with the prefix "registry_" are always
// rejected with a migration error (the BOSH registry was removed), regardless
// of strict mode. Other unknown keys produce an error only when
// strict_config_validation is enabled.
func (c *CPIConfig) validateStrictUnknownKeys(raw []byte, errs *[]string) {
	strict := c.StrictConfigValidationEnabled()
	for _, k := range unknownConfigKeys(raw) {
		if strings.HasPrefix(k, "registry_") {
			*errs = append(*errs, fmt.Sprintf("config key %q is no longer supported (the BOSH registry was removed)", k))
			continue
		}
		if strict {
			*errs = append(*errs, fmt.Sprintf("unknown config key %q (strict_config_validation=true)", k))
		}
	}
}

// MaxConfigBytes caps the CPI configuration JSON at 1 MiB. Realistic BOSH
// CPI configs are a few KiB at most; this is defense-in-depth against a
// malformed or attacker-controlled config that would otherwise drive an
// unbounded io.ReadAll allocation.
const MaxConfigBytes = 1 << 20

// DefaultCPUType is the CPU model ApplyDefaults writes into CPUType when the
// operator leaves pve.cpu_type unset. "host" passes the physical CPU's full
// feature set through to the guest — the best-performing choice, and safe on
// the homogeneous clusters typical of BOSH deployments. Clusters that mix CPU
// generations and rely on live migration should override with a portable
// model such as "x86-64-v2-AES" (PVE's own create-wizard default), since a
// host-typed guest can crash when migrated to a node with a different CPU.
const DefaultCPUType = "host"

// CPUTypePVEDefault is the sentinel operators set (globally via pve.cpu_type
// or per instance group via cloud_properties.cpu_type) to make the CPI write
// no "cpu" key at all, so PVE falls back to its own API default (kvm64).
const CPUTypePVEDefault = "pve-default"

// DefaultBalloon is the balloon value ApplyDefaults writes into Balloon when
// the operator leaves pve.balloon unset. "0" disables the balloon device:
// BOSH sizes VMs deterministically from the manifest, and PVE's default
// auto-ballooning would reclaim guest memory beneath the Director's
// assumptions.
const DefaultBalloon = "0"

// BalloonPVEDefault is the sentinel operators set (globally via pve.balloon
// or per instance group via cloud_properties.balloon) to leave no "balloon"
// key on the VM, so PVE keeps its own default (balloon device enabled,
// balloon = memory). The import path writes nothing; the clone path actively
// deletes the balloon=0 the clone inherits from the stemcell template.
const BalloonPVEDefault = "pve-default"

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

	// Collect strict unknown-key errors here (needs raw bytes) alongside the
	// standard Validate pass. Both accumulate into the same error list so the
	// operator sees all violations in one shot.
	var strictErrs []string
	cfg.validateStrictUnknownKeys(raw, &strictErrs)
	warnFastPathDeleteNonRootIdentity(&cfg, os.Stderr)
	warnParkedDefaultBandCollision(&cfg, os.Stderr)
	if err := cfg.ValidateWithLogger(nil); err != nil {
		if len(strictErrs) == 0 {
			return nil, err
		}
		// Merge strict unknown-key errors into the existing validation error.
		return nil, cpierrors.Cloud("config validation failed: %s; %s",
			strings.TrimPrefix(err.Error(), "config validation failed: "),
			strings.Join(strictErrs, "; "),
		)
	}
	if len(strictErrs) > 0 {
		return nil, cpierrors.Cloud("config validation failed: %s", strings.Join(strictErrs, "; "))
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

// SDNAutoManageZoneEnabled returns the effective zone auto-management bool.
// nil (field absent from JSON) → true (turnkey by default).
// *false → false (operator explicitly owns zones).
// *true  → true.
func (c *CPIConfig) SDNAutoManageZoneEnabled() bool {
	if c.SDNAutoManageZone == nil {
		return true
	}
	return *c.SDNAutoManageZone
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
		c.AgentMode = AgentModeCloudInit
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
	if strings.TrimSpace(c.CPUType) == "" {
		c.CPUType = DefaultCPUType
	}
	if strings.TrimSpace(c.Balloon) == "" {
		c.Balloon = DefaultBalloon
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
	// Parker band, and the upgrade guard around it. Must run after the VM, disk,
	// and template bands above are filled: it compares against them.
	c.applyParkerBandDefaults()
	if c.CloneMode == "" {
		c.CloneMode = CloneModeAuto
	}
	if c.NetworkMode == "" {
		// Simple-first default: a plain pre-existing Linux bridge
		// (managed: false, cloud_properties.bridge) needs zero SDN
		// prerequisites and zero CPI-side provisioning, so it is the
		// right zero-config behavior for the common shared-storage,
		// operator-managed-fabric deployment. SDN remains fully
		// supported as a one-line opt-in (network_mode: sdn) or per
		// network (cloud_properties naming a zone or vnet).
		c.NetworkMode = NetworkModeBridge
	}
	if c.SDNZoneType == "" {
		c.SDNZoneType = "vxlan"
	}
	if c.StemcellStrategy == "" {
		// Clone-from-cache is the fast path: the per-cluster cache template is
		// built once at upload time, so every create_vm pays seconds (CoW
		// clone) instead of a full qcow2 import.
		c.StemcellStrategy = StemcellStrategyTemplate
	}
	// VNI auto-allocation band. Filled unconditionally (unlike the parker band)
	// because the band is always meaningful once a tagged zone type is in play.
	// For 802.1Q-capped zone types (vlan, qinq) the default band must sit
	// inside the 4094 VLAN ID cap — the VXLAN-oriented 5000..5999 default
	// would make every auto-allocated tag fail at create_network time. An
	// operator-set band always wins (and is cap-validated in
	// validateSDNFields).
	if c.SDNVNIRangeStart == 0 && c.SDNVNIRangeEnd == 0 && sdnZoneTypeIsVLANCapped(c.SDNZoneType) {
		c.SDNVNIRangeStart = 2000
		c.SDNVNIRangeEnd = 2999
	}
	if c.SDNVNIRangeStart == 0 {
		c.SDNVNIRangeStart = 5000
	}
	if c.SDNVNIRangeEnd == 0 {
		c.SDNVNIRangeEnd = 5999
	}
	c.applyPlacementDefaults()
	c.applyOTelDefaults()
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

// applyOTelDefaults fills zero-value OTel fields when the relevant signal is
// enabled. A no-op for a field group whose signal is disabled, so a fully
// disabled block (all three signals false) is never touched and stays
// byte-identical to a fresh zero value.
func (c *CPIConfig) applyOTelDefaults() {
	anyEnabled := c.OTel.Enabled || c.OTel.LogsEnabled || c.OTel.MetricsEnabled
	if anyEnabled && c.OTel.Protocol == "" {
		c.OTel.Protocol = "http"
	}
	if c.OTel.Enabled {
		if c.OTel.ServiceName == "" {
			c.OTel.ServiceName = "bosh-pve-cpi"
		}
		if c.OTel.SampleRatio == 0 {
			c.OTel.SampleRatio = 1.0
		}
		if c.OTel.ExportTimeoutMs == 0 {
			c.OTel.ExportTimeoutMs = 5000
		}
	}
	if c.OTel.LogsEnabled && c.OTel.LogsExporterEndpoint == "" {
		c.OTel.LogsExporterEndpoint = c.OTel.ExporterEndpoint
	}
	if c.OTel.MetricsEnabled && c.OTel.MetricsExporterEndpoint == "" {
		c.OTel.MetricsExporterEndpoint = c.OTel.ExporterEndpoint
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

// CPUTypeValue returns the effective global CPU type/model, trimmed of
// surrounding whitespace. The CPUTypePVEDefault sentinel resolves to "" —
// callers only emit a "cpu" key when the result is non-empty, so the sentinel
// means "write no cpu key; let PVE fall back to its kvm64 API default". On a
// config that went through ApplyDefaults an empty CPUType has already been
// filled with DefaultCPUType; "" here only survives for nil or never-defaulted
// configs.
func (c *CPIConfig) CPUTypeValue() string {
	if c == nil {
		return ""
	}
	v := strings.TrimSpace(c.CPUType)
	if v == CPUTypePVEDefault {
		return ""
	}
	return v
}

// BalloonValue returns the effective global balloon setting, trimmed of
// surrounding whitespace. Empty (nil receiver or never-defaulted config)
// resolves to "0" — ballooning disabled — so the default holds even for
// configs that never went through ApplyDefaults. The BalloonPVEDefault
// sentinel resolves to "": callers only emit a "balloon" key when the result
// is non-empty, so the sentinel means "write no balloon key; let PVE keep its
// own default" (device enabled, balloon = memory).
func (c *CPIConfig) BalloonValue() string {
	if c == nil {
		return DefaultBalloon
	}
	v := strings.TrimSpace(c.Balloon)
	if v == "" {
		return DefaultBalloon
	}
	if v == BalloonPVEDefault {
		return ""
	}
	return v
}

// NUMAValue returns the effective NUMA toggle, defaulting to true (memory
// hotplug requires numa=1 at create time) when the pointer is nil.
func (c *CPIConfig) NUMAValue() bool {
	if c.NUMA == nil {
		return true
	}
	return *c.NUMA
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

// HANodeAffinityPinEnabled reports whether create_vm should write a PVE HA
// node-affinity rule binding the VM to its AZ node set. Requires a non-nil
// Placement, an explicit *true PinAZViaHARules, and a non-empty AZMap (a pin is
// meaningless without AZ-to-node mappings). Default false (opt-in).
func (c *CPIConfig) HANodeAffinityPinEnabled() bool {
	if c == nil || c.Placement == nil || c.Placement.PinAZViaHARules == nil || !*c.Placement.PinAZViaHARules {
		return false
	}
	return len(c.Placement.AZMap) > 0
}

// PinAZStrict reports whether the node-affinity rule is strict (hard AZ
// guarantee). Defaults to true: nil/absent PinAZStrict returns true. Only an
// explicit *false returns false (non-strict, preferred pin).
func (c *CPIConfig) PinAZStrict() bool {
	if c == nil || c.Placement == nil || c.Placement.PinAZStrict == nil {
		return true
	}
	return *c.Placement.PinAZStrict
}

// PlacementFallbackMaxValue returns the maximum number of alternate nodes to
// try after a transient create or start failure on the initially selected node.
//
// Returns 0 when: c is nil, Placement is nil, or FallbackMax is nil or zero.
// 0 means the fallback path is fully disabled — behavior is byte-identical to
// pre-feature releases. Positive values enable the post-selection fallback loop.
func (c *CPIConfig) PlacementFallbackMaxValue() int {
	if c == nil || c.Placement == nil || c.Placement.FallbackMax == nil {
		return 0
	}
	return *c.Placement.FallbackMax
}

// ReserveStorageHeadroomEnabled reports whether the storage-capacity hard
// placement filter is active. Default false (opt-in): nil Placement block,
// nil ReserveStorageHeadroom field, or explicit *false all return false.
// Only an explicit *true returns true.
func (c *CPIConfig) ReserveStorageHeadroomEnabled() bool {
	if c == nil || c.Placement == nil || c.Placement.ReserveStorageHeadroom == nil {
		return false
	}
	return *c.Placement.ReserveStorageHeadroom
}

// StorageHeadroomMBValue returns the configured extra storage-headroom margin
// in MiB. When StorageHeadroomMB is nil or 0, returns 1024 (1 GiB — matches
// vSphere DISK_HEADROOM default). Only meaningful when
// ReserveStorageHeadroomEnabled() is true.
func (c *CPIConfig) StorageHeadroomMBValue() int {
	const defaultHeadroomMiB = 1024 // 1 GiB, mirrors vSphere DISK_HEADROOM
	if c == nil || c.Placement == nil || c.Placement.StorageHeadroomMB == nil || *c.Placement.StorageHeadroomMB == 0 {
		return defaultHeadroomMiB
	}
	return *c.Placement.StorageHeadroomMB
}

// MemorySignalValue returns the effective placement memory signal:
// "reserved" (the placement.MemorySignalReserved constant) or "resident"
// (placement.MemorySignalResident). Defaults to "reserved" when Placement is
// nil, MemorySignal is empty, or MemorySignal (after case-insensitive,
// whitespace-trimmed comparison) is neither "reserved" nor "resident" — an
// unrecognized value silently falls back to the protective default rather
// than erroring, so a config typo never blocks a deploy. Callers copy this
// string directly into placement.Weights.MemorySignal when building the
// scorer's weights (the same pattern used for EffectiveWeights()).
func (c *CPIConfig) MemorySignalValue() string {
	if c == nil || c.Placement == nil {
		return memorySignalReserved
	}
	if strings.ToLower(strings.TrimSpace(c.Placement.MemorySignal)) == memorySignalResident {
		return memorySignalResident
	}
	return memorySignalReserved
}

// MaxUtilizationPctValue returns the effective storage-pool utilization
// ceiling percentage. Zero (nil Storage block, nil field, or an explicit 0)
// means the gate is disabled — the byte-identical, zero-behavior-change
// default. A positive return activates the four evaluation points documented
// on StorageConfig.MaxUtilizationPct.
func (c *CPIConfig) MaxUtilizationPctValue() int {
	if c == nil || c.Storage == nil || c.Storage.MaxUtilizationPct == nil {
		return 0
	}
	return *c.Storage.MaxUtilizationPct
}

// MaxUtilizationEnforce reports whether a storage-utilization-ceiling
// violation should be enforced (reject/error) rather than only logged. The
// mode is normalized case-insensitively; empty or unrecognized values resolve
// to true (enforce is the default). Only meaningful when
// MaxUtilizationPctValue() > 0; callers must check that first. Ignored
// entirely by snapshot_disk, which is always Warn-only regardless of mode.
func (c *CPIConfig) MaxUtilizationEnforce() bool {
	if c == nil || c.Storage == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(c.Storage.MaxUtilizationMode)) != enumValueWarn
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

// ClusterLockMode returns the normalized cross-process cluster lock mode.
// Empty or absent maps to "off"; the value is lowercased and trimmed.
// Valid return values: "off", "pool".
func (c *CPIConfig) ClusterLockMode() string {
	if c == nil {
		return enumValueOff
	}
	v := strings.ToLower(strings.TrimSpace(c.ClusterLock))
	if v == "" {
		return enumValueOff
	}
	return v
}

// ClusterLockEnabled reports whether a cross-process cluster lock is active
// (mode == "pool"). Default off → byte-identical behavior.
func (c *CPIConfig) ClusterLockEnabled() bool {
	return c.ClusterLockMode() == "pool"
}

// diskBusVirtio and diskBusSCSI are the two valid pve.root_disk_bus values.
const (
	diskBusVirtio = "virtio"
	diskBusSCSI   = "scsi"
)

// RootDiskBusValue returns the normalized root-disk bus: "virtio" (default)
// or "scsi". Empty/absent maps to "virtio" — byte-identical to every release
// before this property existed.
func (c *CPIConfig) RootDiskBusValue() string {
	if c == nil {
		return diskBusVirtio
	}
	v := strings.ToLower(strings.TrimSpace(c.RootDiskBus))
	if v == "" {
		return diskBusVirtio
	}
	return v
}

// RootDiskUsesSCSI reports whether the root disk is created on scsi0 instead
// of virtio0 (RootDiskBusValue() == "scsi").
func (c *CPIConfig) RootDiskUsesSCSI() bool {
	return c.RootDiskBusValue() == diskBusSCSI
}

// ReplicaAdoptTimeoutSecValue returns the configured adopt-and-wait timeout in
// seconds for a racing concurrent template-replica clone. A value <= 0 (the
// default) means the adopt path is disabled and replica builds behave
// byte-identically. Callers gate the adopt probe on this being > 0.
func (c *CPIConfig) ReplicaAdoptTimeoutSecValue() int {
	if c == nil || c.ReplicaAdoptTimeoutSec <= 0 {
		return 0
	}
	return c.ReplicaAdoptTimeoutSec
}

// ReplicaAdoptEnabled reports whether adopt-and-wait on a racing replica clone
// is active (timeout > 0). Default off → byte-identical behavior.
func (c *CPIConfig) ReplicaAdoptEnabled() bool {
	return c.ReplicaAdoptTimeoutSecValue() > 0
}

// ClusterLockTimeoutSecValue returns the effective lock acquire timeout / TTL in
// seconds. 0 (unset) resolves to the conventional 60s default.
func (c *CPIConfig) ClusterLockTimeoutSecValue() int {
	if c == nil || c.ClusterLockTimeoutSec <= 0 {
		return 60
	}
	return c.ClusterLockTimeoutSec
}

// AntiAffinityVerifyEnabled returns the effective read-after-write verify
// toggle. nil/false (default) → false (byte-identical). *true → true.
func (c *CPIConfig) AntiAffinityVerifyEnabled() bool {
	if c == nil || c.AntiAffinityVerify == nil {
		return false
	}
	return *c.AntiAffinityVerify
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
		return enumValueOff
	}
	v := strings.ToLower(strings.TrimSpace(c.IPConflictProbe))
	if v == "" {
		return enumValueOff
	}
	return v
}

// DiskPerfInvariantModeValue returns the effective disk-performance invariant
// enforcement mode, normalized to lower case and trimmed. Empty (the default)
// resolves to "enforce". See the DiskPerfInvariantMode field doc for semantics.
func (c *CPIConfig) DiskPerfInvariantModeValue() string {
	if c == nil {
		return "enforce"
	}
	v := strings.ToLower(strings.TrimSpace(c.DiskPerfInvariantMode))
	if v == "" {
		return "enforce"
	}
	return v
}

// TaskPollAdaptiveEnabled reports whether progress-aware adaptive task polling
// is active. Nil (default) → false.
func (c *CPIConfig) TaskPollAdaptiveEnabled() bool {
	return c != nil && c.TaskPollAdaptive != nil && *c.TaskPollAdaptive
}

// RedactLogsEnabled reports whether the redacted request/response dispatcher
// trace is enabled (§7.41). Nil receiver or unset field → false (no trace).
func (c *CPIConfig) RedactLogsEnabled() bool {
	return c != nil && c.RedactLogs != nil && *c.RedactLogs
}

// ResizeWaitForConvergenceEnabled reports whether resize_disk should poll for
// post-resize size convergence. Nil (default) → false.
func (c *CPIConfig) ResizeWaitForConvergenceEnabled() bool {
	return c != nil && c.ResizeWaitForConvergence != nil && *c.ResizeWaitForConvergence
}

// RejectTLSDowngradeOverridesEnabled reports whether a per-request context
// override that downgrades TLS verification (pve_verify_ssl=false against a
// verifying base config) must be rejected rather than merely warned about.
// Nil receiver or unset field → false (warn-only, byte-identical to every
// release before this knob existed).
func (c *CPIConfig) RejectTLSDowngradeOverridesEnabled() bool {
	return c != nil && c.RejectTLSDowngradeOverrides != nil && *c.RejectTLSDowngradeOverrides
}

// ResizeConvergenceTimeoutSecValue returns the effective convergence poll budget
// in seconds. A nil receiver or a non-positive configured value resolves to the
// 120-second default.
func (c *CPIConfig) ResizeConvergenceTimeoutSecValue() int {
	const defaultSec = 120
	if c == nil || c.ResizeConvergenceTimeoutSec <= 0 {
		return defaultSec
	}
	return c.ResizeConvergenceTimeoutSec
}

// ActiveIPProbeEnabled reports whether the guest-agent IP fan-out probe is
// active. Returns true only when IPConflictProbeMode() == "agent".
func (c *CPIConfig) ActiveIPProbeEnabled() bool {
	return c.IPConflictProbeMode() == "agent"
}

// enumValueOff is the shared "off" literal used by the opt-in enum knobs.
const enumValueOff = "off"

// enumValueWarn is the shared "warn" literal used by the enforce/warn enum
// knobs (e.g. storage.max_utilization_mode).
const enumValueWarn = "warn"

// memorySignalReserved and memorySignalResident are the two valid values for
// placement.memory_signal. They intentionally match the string values of
// placement.MemorySignalReserved and placement.MemorySignalResident without
// importing that package (internal/config avoids importing sibling internal
// packages beyond internal/cpi/hooks to keep the dependency graph acyclic;
// see internal/cpi/hooks/deps.go). MemorySignalValue() copies whichever of
// these two literals it resolves to directly into placement.Weights.MemorySignal
// at the handler layer, where the two packages actually meet.
const (
	memorySignalReserved = "reserved"
	memorySignalResident = "resident"
)

// DiskDeleteStateGuardEnabled reports whether delete_disk should check the
// owning VM's lock state before deleting. Default true: nil
// config, empty string, or "on" all resolve to enabled. Only an explicit
// "off" disables the lookup and restores the earlier byte-identical
// behavior. Any other value is rejected at config validation time
// (validateDiskDeleteStateGuardEnum), so by the time this accessor runs in
// production the field is guaranteed to be one of ""|"off"|"on".
func (c *CPIConfig) DiskDeleteStateGuardEnabled() bool {
	if c == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(c.DiskDeleteStateGuard)) != enumValueOff
}

// DetachedDiskStrategyParked and DetachedDiskStrategyFree are the two valid
// detached_disk_strategy values. "parked" is the default an empty setting
// resolves to; "free" is the opt-out that restores free-floating volumes.
const (
	DetachedDiskStrategyParked = "parked"
	DetachedDiskStrategyFree   = "free"
)

// DetachedDiskStrategyValue returns the effective detached-disk lifecycle
// strategy, normalized to lower case and trimmed. Empty or absent resolves to
// "parked", the safe default: a detached disk stays owned by a protected parker
// VM instead of floating as an unattached volume. Operators who want the older
// behavior set "free" explicitly. Valid return values: "parked", "free".
//
// A nil receiver means no configuration is loaded at all, so it resolves to
// "free" to keep every parker code path inert (matching the nil guard in
// ParkedStrategyActive) rather than driving parker calls off a nil config.
func (c *CPIConfig) DetachedDiskStrategyValue() string {
	if c == nil {
		return DetachedDiskStrategyFree
	}
	v := strings.ToLower(strings.TrimSpace(c.DetachedDiskStrategy))
	if v == "" {
		if c.parkedDefaultBandCollision != "" {
			// The default would be parked, but its built-in band collides with
			// a band this config already uses. See applyParkerBandDefaults. The
			// collision string is the single record of that decision: a
			// companion boolean would let a future edit set one without the
			// other, leaving a config that reports parked while warning that it
			// stood down.
			return DetachedDiskStrategyFree
		}
		return DetachedDiskStrategyParked
	}
	return v
}

// DetachedDiskParkedEnabled reports whether the "parked" detached-disk strategy
// is active. Returns true when DetachedDiskStrategyValue() == "parked", which
// includes the unset default.
func (c *CPIConfig) DetachedDiskParkedEnabled() bool {
	return c.DetachedDiskStrategyValue() == DetachedDiskStrategyParked
}

// DiskMigrationOnAttach and DiskMigrationOff are the two valid disk_migration
// values. "on_attach" is the default an empty setting resolves to; "off" is
// the opt-out that restores the hard cross-node errors.
const (
	DiskMigrationOnAttach = "on_attach"
	DiskMigrationOff      = "off"
)

// DiskMigrationValue returns the effective cross-node disk-migration mode,
// normalized to lower case and trimmed. Empty or absent resolves to
// "on_attach", the default: a stranded disk moves instead of erroring. Valid
// return values: "on_attach", "off".
//
// A nil receiver means no configuration is loaded at all, so it resolves to
// "off" to keep the migration path inert (matching the nil guard rationale in
// DetachedDiskStrategyValue) rather than driving mover creation off a nil
// config.
func (c *CPIConfig) DiskMigrationValue() string {
	if c == nil {
		return DiskMigrationOff
	}
	v := strings.ToLower(strings.TrimSpace(c.DiskMigration))
	if v == "" {
		return DiskMigrationOnAttach
	}
	return v
}

// DiskMigrationOnAttachEnabled reports whether attach_disk may migrate a
// stranded disk cross-node. Returns true when DiskMigrationValue() ==
// "on_attach", which includes the unset default.
func (c *CPIConfig) DiskMigrationOnAttachEnabled() bool {
	return c.DiskMigrationValue() == DiskMigrationOnAttach
}

// ParkedAnchorStrictValue resolves pve.parked_anchor_strict: unset (nil) means
// strict, the default. See the ParkedAnchorStrict field comment for what the
// invariant covers.
func (c *CPIConfig) ParkedAnchorStrictValue() bool {
	if c == nil || c.ParkedAnchorStrict == nil {
		return true
	}
	return *c.ParkedAnchorStrict
}

// ParkedStrategyActive reports whether any parker-related behavior should be
// applied. Returns true when any of the following is set:
//   - DetachedDiskParkedEnabled() (strategy="parked")
//   - ParkedDiskVMIDRangeStart != 0 (raw field — a parker band is in force)
//   - ParkedDiskVMIDRangeEnd != 0 (raw field — a parker band is in force)
//
// ApplyDefaults fills the band under both strategies, so on any defaulted
// config this is true: the parker read paths (holder scans, unpark) always
// run, and only strategy="parked" ever creates a parker. False for a nil
// receiver, a config that never went through ApplyDefaults, or a cpi-config
// entry that explicitly zeroes both bounds over a job-level named band — the
// raw fields stay 0 there, while the effective accessors still resolve the
// built-in band. Gate behavior on DetachedDiskParkedEnabled or on the
// effective accessors, not on this.
func (c *CPIConfig) ParkedStrategyActive() bool {
	if c == nil {
		return false
	}
	return c.DetachedDiskParkedEnabled() || c.ParkedDiskVMIDRangeStart != 0 || c.ParkedDiskVMIDRangeEnd != 0
}

// ParkedDiskVMIDRangeStartValue returns the effective parker-band lower bound.
// Returns 0 only on a nil receiver; every loaded config resolves a band.
//
// The built-in fallback matters because ApplyDefaults is not the only way a
// config reaches the handlers: ApplyContextOverrides merges per-CPI overrides
// onto an already-defaulted base and validates the result WITHOUT re-running
// ApplyDefaults, and both parker range keys are overridable — a cpi-config
// entry carrying the spec's documented 0 for them zeroes a band the default
// strategy still uses. Falling back to the band ApplyDefaults would have filled
// keeps that from reaching pve.ParkDisk as an "invalid VMID range [0, 0]"
// config error, which the detach path wraps as retriable and the Director then
// retries forever.
func (c *CPIConfig) ParkedDiskVMIDRangeStartValue() int {
	if c == nil {
		return 0
	}
	start, _ := c.effectiveParkerBand()
	return start
}

// ParkedDiskVMIDRangeEndValue returns the effective parker-band upper bound,
// with the same zero-band fallback as ParkedDiskVMIDRangeStartValue.
func (c *CPIConfig) ParkedDiskVMIDRangeEndValue() int {
	if c == nil {
		return 0
	}
	_, end := c.effectiveParkerBand()
	return end
}

// effectiveParkerBand returns the parker band the parked strategy actually
// uses, filling a bound the operator left at zero.
//
// The fill is per-bound rather than per-pair because a cpi-config entry
// overrides one key at a time. jobs/pve_cpi/spec documents 0 as the default for
// both range properties, so an operator who moves only the upper bound for one
// cluster ("parked_disk_vmid_range_end: 90499") leaves the lower bound at that
// documented 0 — and a pair-wise fallback would then hand pve.ParkDisk the band
// [0, 90499], which Validate rejects as below the PVE reserved floor. Every
// request against that entry would fail permanently, naming a bound the
// operator never meaningfully set. validateVMIDBands checks these same
// effective values so the two can never disagree.
//
// The missing bound comes from the built-in band when that produces a window no
// wider than the built-in one, which is the case the property documentation
// describes: narrowing 90000-90999 from one side. Otherwise it is derived from
// the bound the operator DID name, keeping the same width. Reaching for the
// built-in bound unconditionally would answer "parked_disk_vmid_range_start:
// 50000" with the band [50000,90999] — 41,000 VMIDs of an operator's space
// claimed by a config that names one of them, validated against nothing, since
// only the three configured bands are ever compared. A band the operator did
// not describe should not be wider than the one they did.
//
// The fill applies under both strategies. Under "free" the band is read-only:
// no parker VM is ever allocated in it, but the holder scans use it to
// recognize disks parked earlier and unpark them on the next attach_disk or
// delete_disk, instead of refusing them as stranded. A nil receiver is the only
// state with no band at all.
// minVMID is the first VMID PVE lets a guest take; 1-99 are reserved.
const minVMID = 100

func (c *CPIConfig) effectiveParkerBand() (int, int) {
	start, end := c.ParkedDiskVMIDRangeStart, c.ParkedDiskVMIDRangeEnd
	const width = defaultParkerVMIDEnd - defaultParkerVMIDStart
	switch {
	case start == 0 && end == 0:
		return defaultParkerVMIDStart, defaultParkerVMIDEnd
	case start == 0:
		if end > defaultParkerVMIDStart && end-defaultParkerVMIDStart <= width {
			return defaultParkerVMIDStart, end
		}
		// Derived at the same width, but never below the first VMID PVE allows.
		// Without the floor, an end of 100 derives a start of -899 and the bound
		// error names a number the operator never wrote, which reads as a CPI
		// defect rather than as "that end is too low to carry a band".
		if derived := end - width; derived >= minVMID {
			return derived, end
		}
		return minVMID, end
	case end == 0:
		if start < defaultParkerVMIDEnd && defaultParkerVMIDEnd-start <= width {
			return start, defaultParkerVMIDEnd
		}
		return start, start + width
	}
	return start, end
}

// validateIPConflictProbeEnum appends an error when ip_conflict_probe is set to
// a value other than off|agent. Empty (the default) is valid.
// validateBalloon rejects a non-empty Balloon that is neither the pve-default
// sentinel nor a non-negative integer (MiB). Empty is valid — ApplyDefaults
// fills it with DefaultBalloon ("0").
func (c *CPIConfig) validateBalloon(errs *[]string) {
	v := strings.TrimSpace(c.Balloon)
	if v == "" || v == BalloonPVEDefault {
		return
	}
	if n, err := strconv.Atoi(v); err != nil || n < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"balloon must be a non-negative integer (MiB) or %q, got %q",
			BalloonPVEDefault, c.Balloon,
		))
	}
}

func (c *CPIConfig) validateIPConflictProbeEnum(errs *[]string) {
	if c.IPConflictProbe == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.IPConflictProbe)) {
	case enumValueOff, "agent":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"ip_conflict_probe must be one of off|agent (or empty for default off), got %q",
			c.IPConflictProbe,
		))
	}
}

// validateEphemeralDiskMinModeEnum appends an error when ephemeral_disk_min_mode
// is set to a value other than enforce|warn. Empty (the default) is valid and
// resolves to enforce.
func (c *CPIConfig) validateEphemeralDiskMinModeEnum(errs *[]string) {
	if c.EphemeralDiskMinMode == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.EphemeralDiskMinMode)) {
	case "enforce", "warn":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"ephemeral_disk_min_mode must be one of enforce|warn (or empty for default enforce), got %q",
			c.EphemeralDiskMinMode,
		))
	}
}

// validateDetachedDiskStrategyEnum appends an error when detached_disk_strategy
// is set to a value other than free|parked. Empty (the default) is valid and
// resolves to "parked".
func (c *CPIConfig) validateDetachedDiskStrategyEnum(errs *[]string) {
	if c.DetachedDiskStrategy == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.DetachedDiskStrategy)) {
	case DetachedDiskStrategyFree, DetachedDiskStrategyParked:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"detached_disk_strategy must be one of free|parked (or empty for default parked), got %q",
			c.DetachedDiskStrategy,
		))
	}
}

// validateDiskMigrationEnum appends an error when disk_migration is set to a
// value other than on_attach|off. Empty (the default) is valid and resolves
// to "on_attach".
func (c *CPIConfig) validateDiskMigrationEnum(errs *[]string) {
	if c.DiskMigration == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.DiskMigration)) {
	case DiskMigrationOnAttach, DiskMigrationOff:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"disk_migration must be one of on_attach|off (or empty for default on_attach), got %q",
			c.DiskMigration,
		))
	}
}

// validateRootDiskBusEnum appends an error when root_disk_bus is set to a
// value other than virtio|scsi. Empty (the default) is valid and resolves to
// "virtio". Extracted from validateEnumFields to keep its cognitive
// complexity under the project threshold.
func (c *CPIConfig) validateRootDiskBusEnum(errs *[]string) {
	if c.RootDiskBus == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.RootDiskBus)) {
	case diskBusVirtio, diskBusSCSI:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"root_disk_bus must be one of virtio|scsi (or empty for default virtio), got %q",
			c.RootDiskBus,
		))
	}
}

// validateDiskDeleteStateGuardEnum appends an error when disk_delete_state_guard
// is set to a value other than off|on. Empty (the default) is valid.
func (c *CPIConfig) validateDiskDeleteStateGuardEnum(errs *[]string) {
	if c.DiskDeleteStateGuard == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.DiskDeleteStateGuard)) {
	case enumValueOff, "on":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"disk_delete_state_guard must be one of off|on (or empty for default on), got %q",
			c.DiskDeleteStateGuard,
		))
	}
}

// NetworkResolveRetriesValue returns the configured SDN eventual-consistency
// poll retry count. Default 30: a nil receiver or a nil field
// (the property left entirely unset) resolves to 30. An explicit 0 disables
// both gates (returns 0), restoring the earlier ungated behavior. A negative
// explicit value also resolves to 0 defensively (config validation rejects
// negative values at load time, so this is a belt-and-suspenders guard for
// manually constructed CPIConfig values in tests).
func (c *CPIConfig) NetworkResolveRetriesValue() int {
	if c == nil || c.NetworkResolveRetries == nil {
		return 30
	}
	if *c.NetworkResolveRetries < 0 {
		return 0
	}
	return *c.NetworkResolveRetries
}

// NetworkResolveEnabled reports whether the SDN convergence gates run.
func (c *CPIConfig) NetworkResolveEnabled() bool {
	return c.NetworkResolveRetriesValue() > 0
}

// NetworkResolveTimeoutSecValue returns the effective absolute poll bound in
// seconds. nil receiver or a non-positive value → 60.
func (c *CPIConfig) NetworkResolveTimeoutSecValue() int {
	if c == nil || c.NetworkResolveTimeoutSec <= 0 {
		return 60
	}
	return c.NetworkResolveTimeoutSec
}

// EphemeralDiskMinRatioValue returns the effective ephemeral-disk minimum-size
// ratio. A nil receiver or a non-positive value resolves to 0, which disables
// the invariant (byte-identical behavior).
func (c *CPIConfig) EphemeralDiskMinRatioValue() float64 {
	if c == nil || c.EphemeralDiskMinRatio <= 0 {
		return 0
	}
	return c.EphemeralDiskMinRatio
}

// EphemeralDiskMinModeValue returns the effective enforcement mode for the
// ephemeral-disk minimum-size invariant, normalized to lower case and trimmed.
// Empty (the default) resolves to "enforce". See the EphemeralDiskMinMode field
// doc for semantics.
func (c *CPIConfig) EphemeralDiskMinModeValue() string {
	if c == nil {
		return "enforce"
	}
	v := strings.ToLower(strings.TrimSpace(c.EphemeralDiskMinMode))
	if v == "" {
		return "enforce"
	}
	return v
}

// VMFirewallEnabled returns the effective global per-NIC firewall default.
// nil (field absent from JSON) → false (no behavior change versus prior
// releases). *false → false; *true → true.
func (c *CPIConfig) VMFirewallEnabled() bool {
	return c.VMFirewall != nil && *c.VMFirewall
}

// EncryptedEnabled returns the effective global encrypted-storage preference (§7.49).
// nil (absent from JSON) → false (byte-identical, no filter applied).
// *false → false; *true → true.
func (c *CPIConfig) EncryptedEnabled() bool {
	return c.Encrypted != nil && *c.Encrypted
}

// RequireSharedISOForHAEnabled returns the effective require_shared_iso_for_ha
// setting. nil (field absent from JSON) → false (warn-only, byte-identical to
// prior releases). *false → false; *true → true (escalates the config-drive
// ISO migration-safety Warn to a CloudError). See the ISOStorage field doc.
func (c *CPIConfig) RequireSharedISOForHAEnabled() bool {
	return c != nil && c.RequireSharedISOForHA != nil && *c.RequireSharedISOForHA
}

// ISOStorageFollowVMStorageEnabled returns the effective
// iso_storage_follow_vm_storage setting. nil (field absent from JSON) → TRUE
// (the default: ISO follows vm_storage when eligible). *false → false
// (opt-out: ISOStorage used as configured); *true → true (explicit, same as default).
// See the ISOStorageFollowVMStorage field doc for the resolution algorithm.
func (c *CPIConfig) ISOStorageFollowVMStorageEnabled() bool {
	if c == nil {
		return false
	}
	if c.ISOStorageFollowVMStorage == nil {
		return true
	}
	return *c.ISOStorageFollowVMStorage
}

// HooksValue returns the configured hook names in order, or nil when none are
// set.
func (c *CPIConfig) HooksValue() []string {
	return c.Hooks
}

// LBRegisterConfig returns the lb_register hook configuration, or nil when no
// block is set. The pointer is shared with the hook constructor in main.go.
func (c *CPIConfig) LBRegisterConfig() *hooks.LBRegisterConfig {
	if c == nil {
		return nil
	}
	return c.LBRegister
}

// ExternalCommandConfig returns the external_command hook configuration, or nil
// when no block is set.
func (c *CPIConfig) ExternalCommandConfig() *hooks.ExternalCommandConfig {
	if c == nil {
		return nil
	}
	return c.ExternalCommand
}

// MetricsConfig returns the metrics hook configuration, or nil when no block
// is set or Enabled is false.
func (c *CPIConfig) MetricsConfig() *hooks.MetricsConfig {
	if c == nil || c.Metrics == nil || !c.Metrics.Enabled {
		return nil
	}
	return c.Metrics
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

// HealthCheckExpectedAgentSHA256 returns the configured expected agent-binary
// SHA-256, normalized to lower case, or "" when unset. Empty disables the
// §7.29 checksum assertion.
func (c *CPIConfig) HealthCheckExpectedAgentSHA256() string {
	if c == nil || c.HealthCheck == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.HealthCheck.ExpectedAgentSHA256))
}

// KeepFailedVMsEnabled reports whether the create_vm keep-failed diagnostic mode
// is active. Returns false when Debug is nil, KeepFailedVMs is nil, or it is
// *false. Only an explicit *true returns true.
func (c *CPIConfig) KeepFailedVMsEnabled() bool {
	if c == nil || c.Debug == nil || c.Debug.KeepFailedVMs == nil {
		return false
	}
	return *c.Debug.KeepFailedVMs
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

	defaultPushbackBaseMs = 5000
	defaultPushbackCapMs  = 60000

	defaultStorageLockBaseMs    = 2000
	defaultStorageLockCapMs     = 30000
	defaultStorageLockJitterPct = 30 // matches StorageLockBackoff shipped curve (±30% jitter)

	// defaultDiskMigrateCapMs is the wall-clock budget for awaiting a
	// cross-node disk-migration task: 30 minutes, sized for a node-local
	// volume copy rather than the metadata move shared storage needs.
	defaultDiskMigrateCapMs = 1800000
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

// RetryPushback returns the resolved pushback (HTTP 429 / worker-busy) backoff
// policy. BaseMs is the initial delay, CapMs the ceiling, MaxAttempts the
// retry budget (0 → caller chooses its own default).
func (c *CPIConfig) RetryPushback() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.Pushback })
	out := EffectiveRetryPolicy{
		BaseMs: defaultPushbackBaseMs,
		CapMs:  defaultPushbackCapMs,
	}
	if p != nil {
		out.MaxAttempts = p.MaxAttempts // 0 → caller default
		out.BaseMs = resolveField(p.BaseMs, defaultPushbackBaseMs)
		out.CapMs = resolveField(p.CapMs, defaultPushbackCapMs)
		out.JitterPct = resolveField(p.JitterPct, 0) // not used by PushbackBackoff today
	}
	return out
}

// RetryStorageLock returns the resolved storage-lock backoff policy used
// inside RetryOnTransientOrLock. MaxAttempts 0 means callers should fall back
// to pve.DefaultStorageLockMaxAttempts (the constant the CPI shipped with).
// Defaults: base_ms 2000, cap_ms 30000, jitter_pct 30.
func (c *CPIConfig) RetryStorageLock() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.StorageLock })
	out := EffectiveRetryPolicy{
		BaseMs:    defaultStorageLockBaseMs,
		CapMs:     defaultStorageLockCapMs,
		JitterPct: defaultStorageLockJitterPct,
	}
	if p != nil {
		out.MaxAttempts = p.MaxAttempts // 0 → caller falls back to DefaultStorageLockMaxAttempts
		out.BaseMs = resolveField(p.BaseMs, defaultStorageLockBaseMs)
		out.CapMs = resolveField(p.CapMs, defaultStorageLockCapMs)
		out.JitterPct = resolveField(p.JitterPct, defaultStorageLockJitterPct)
	}
	return out
}

// RetryDiskMigrate returns the resolved cross-node disk-migration policy.
// MaxAttempts bounds transient retries of the migrate request and is left at
// 0 when the operator has not set it, signaling callers to fall back to
// pve.DefaultDiskMigrateMaxAttempts. CapMs is the wall-clock budget in
// milliseconds for awaiting the PVE migrate task (default 30 minutes).
// BaseMs and JitterPct are not consulted by this class and are always 0.
func (c *CPIConfig) RetryDiskMigrate() EffectiveRetryPolicy {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.DiskMigrate })
	out := EffectiveRetryPolicy{
		CapMs: defaultDiskMigrateCapMs,
	}
	if p != nil {
		out.MaxAttempts = p.MaxAttempts // 0 → caller falls back to DefaultDiskMigrateMaxAttempts
		out.CapMs = resolveField(p.CapMs, defaultDiskMigrateCapMs)
	}
	return out
}

// RetryTransientMaxAttempts returns the operator's override for the
// transport-layer transient retry budget (RetryOnTransient and friends), or 0
// when unset so callers keep pve.DefaultTransientMaxAttempts. Only
// max_attempts is meaningful for this class; the backoff curve is the fixed
// TransientBackoff (see the RetryConfig.Transient doc comment).
func (c *CPIConfig) RetryTransientMaxAttempts() int {
	p := c.retryPolicyOf(func(r *RetryConfig) *RetryPolicy { return r.Transient })
	if p == nil {
		return 0
	}
	return p.MaxAttempts
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

// OTelEnabled reports whether OTel tracing is turned on. A nil receiver
// returns false (matches the nil-safe accessor convention used throughout
// this package).
func (c *CPIConfig) OTelEnabled() bool {
	if c == nil {
		return false
	}
	return c.OTel.Enabled
}

// OTelLogsEnabled reports whether the OTel logs signal is turned on. A nil
// receiver returns false (matches the nil-safe accessor convention used
// throughout this package).
func (c *CPIConfig) OTelLogsEnabled() bool {
	if c == nil {
		return false
	}
	return c.OTel.LogsEnabled
}

// OTelMetricsEnabled reports whether the OTel metrics signal is turned on. A
// nil receiver returns false (matches the nil-safe accessor convention used
// throughout this package).
func (c *CPIConfig) OTelMetricsEnabled() bool {
	if c == nil {
		return false
	}
	return c.OTel.MetricsEnabled
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

// MaxInflightPerNodeLimit returns the configured per-node in-flight cap.
// Returns 0 (unlimited) when the field is absent or zero, preserving
// byte-identical behavior for existing configurations.
func (c *CPIConfig) MaxInflightPerNodeLimit() int {
	if c == nil {
		return 0
	}
	return c.MaxInflightPerNode
}

// StrictConfigValidationEnabled reports whether strict config validation is
// active. Returns false when: c is nil, StrictConfigValidation is nil, or
// StrictConfigValidation is *false. Only an explicit *true returns true.
// When false, unknown keys warn and cross-field inconsistencies are tolerated
// (byte-identical to prior releases). When true, both become hard CloudErrors.
func (c *CPIConfig) StrictConfigValidationEnabled() bool {
	if c == nil || c.StrictConfigValidation == nil {
		return false
	}
	return *c.StrictConfigValidation
}

// FastPathDeleteEnabled reports whether opt-in fast-path delete is active.
// When false (nil/absent/explicit *false), delete_vm and delete_disk await
// the PVE task until the resource is confirmed gone — fully synchronous.
// When true, the handlers tag-and-return without polling terminal state
// (eventual consistency; pairs with §7.13 orphan-GC to reap leftovers).
// Default false (nil → false).
func (c *CPIConfig) FastPathDeleteEnabled() bool {
	return c != nil && c.FastPathDelete != nil && *c.FastPathDelete
}

// StemcellReplicationConcurrencyValue returns the effective worker count for
// parallel stemcell replication. 0 or absent resolves to 1 (serial, byte-identical
// to prior releases). Valid positive values are returned as-is.
// Negative values and values > 64 are rejected at config validation, so this
// accessor assumes a validated config and clamps only 0 to 1.
func (c *CPIConfig) StemcellReplicationConcurrencyValue() int {
	if c == nil || c.StemcellReplicationConcurrency <= 0 {
		return 1
	}
	return c.StemcellReplicationConcurrency
}

// Validate checks all required fields and enum constraints.
// Returns a CloudError whose message lists every violation, separated by "; ".
func (c *CPIConfig) Validate() error {
	return c.ValidateWithLogger(nil)
}

// ValidateWithLogger is identical to Validate, but accepts a logger parameter
// for any warning entries. A nil logger uses the default stderr fallback.
// The logger parameter is retained for API compatibility; no registry warnings
// are emitted since the registry was removed.
func (c *CPIConfig) ValidateWithLogger(_ *log.Logger) error {
	var errs []string
	c.validateRequiredFields(&errs)
	c.validateAuth(&errs)
	c.validateEnumFields(&errs)
	c.validateSDNFields(&errs)
	c.validateRanges(&errs)
	c.validatePlacement(&errs)
	c.validateStorage(&errs)
	c.validateHooks(&errs)
	c.validateMetrics(&errs)
	c.validateHealthCheck(&errs)
	c.validateRetry(&errs)
	c.validateOperationTimeout(&errs)
	c.validateOTel(&errs)
	c.validateStorageTiers(&errs)
	c.validateDiskPerformance(&errs)
	c.validateVMPool(&errs)
	c.validateStemcellTemplatePool(&errs)
	c.validateVMPoolTemplate(&errs)
	// Cross-field strict checks. No raw bytes needed; struct fields are read
	// directly. Appended after all other validators so existing error order is
	// preserved and strict errors group at the end.
	c.validateStrictCrossFields(&errs)
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
	c.validateAgentModeEnum(errs)
	c.validateVMDiskFormatEnum(errs)
	c.validateLogLevelEnum(errs)
	c.validateRebootModeEnum(errs)

	// NetworkMode enum, plus the SDNZoneType enum which is only validated
	// when the SDN path is reachable.
	c.validateNetworkModeAndZoneEnum(errs)

	// CloneMode enum: validate only when non-empty (ApplyDefaults sets "auto" when absent;
	// "auto" and "linked" and "full" are the only valid values).
	c.validateCloneModeEnum(errs)

	// StemcellStrategy enum: validate only when non-empty (ApplyDefaults sets
	// "template" when absent).
	c.validateStemcellStrategyEnum(errs)

	// Balloon: validate only when non-empty.
	c.validateBalloon(errs)

	// IPConflictProbe enum: validate only when non-empty.
	c.validateIPConflictProbeEnum(errs)

	// DiskDeleteStateGuard enum: validate only when non-empty.
	c.validateDiskDeleteStateGuardEnum(errs)

	// DetachedDiskStrategy enum: validate only when non-empty.
	c.validateDetachedDiskStrategyEnum(errs)

	// DiskMigration enum: validate only when non-empty.
	c.validateDiskMigrationEnum(errs)

	// DiskPerfInvariantMode enum: validate only when non-empty.
	c.validateDiskPerfInvariantModeEnum(errs)

	// ReplicaAdoptTimeoutSec: 0 disables the adopt path; negative is invalid.
	c.validateReplicaAdoptTimeoutSecField(errs)

	// ClusterLock mode enum: validate only when non-empty.
	c.validateClusterLockModeEnum(errs)

	// RootDiskBus enum: validate only when non-empty.
	c.validateRootDiskBusEnum(errs)

	// ClusterLockTimeoutSec, NetworkResolveRetries, NetworkResolveTimeoutSec,
	// and EphemeralDiskMinRatio: independent "must be >= 0" numeric bounds.
	c.validateNetworkTimingFields(errs)

	// EphemeralDiskMinMode enum: validate only when non-empty.
	c.validateEphemeralDiskMinModeEnum(errs)

	// StemcellStagingDir: when set, must be an absolute path to an existing directory.
	c.validateStemcellStagingDirField(errs)

	// ResizeConvergenceTimeoutSec: negative is invalid (0 → default). An overly
	// long budget is allowed (operator's choice); only nonsense is rejected.
	c.validateResizeConvergenceTimeoutSecField(errs)

	// PVECACertPEM: when non-empty AND verify_ssl=true, the PEM must parse to at
	// least one valid certificate.
	c.validatePVECACertPEMField(errs)
}

// validateAgentModeEnum appends an error when agent_mode is not one of
// cloudinit|noagent|auto. The removed "registry" value gets a dedicated
// message pointing at cloudinit since the BOSH registry was deprecated
// upstream.
func (c *CPIConfig) validateAgentModeEnum(errs *[]string) {
	switch c.AgentMode {
	case AgentModeCloudInit, AgentModeNoAgent, AgentModeAuto:
		// valid
	case "registry":
		*errs = append(*errs, `agent_mode "registry" is no longer supported (the BOSH registry was deprecated upstream); set agent_mode to "cloudinit"`)
	default:
		*errs = append(*errs, fmt.Sprintf(
			"agent_mode must be one of cloudinit|noagent|auto, got %q", c.AgentMode,
		))
	}
}

// validateVMDiskFormatEnum appends an error when vm_disk_format is not one
// of qcow2|raw|vmdk.
func (c *CPIConfig) validateVMDiskFormatEnum(errs *[]string) {
	switch c.VMDiskFormat {
	case "qcow2", "raw", "vmdk":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"vm_disk_format must be one of qcow2|raw|vmdk, got %q", c.VMDiskFormat,
		))
	}
}

// validateLogLevelEnum appends an error when log_level is not one of
// debug|info|warn|error.
func (c *CPIConfig) validateLogLevelEnum(errs *[]string) {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"log_level must be one of debug|info|warn|error, got %q", c.LogLevel,
		))
	}
}

// validateRebootModeEnum appends an error when reboot_mode is not one of
// soft|hard.
func (c *CPIConfig) validateRebootModeEnum(errs *[]string) {
	switch c.RebootMode {
	case "soft", "hard":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			`reboot_mode must be one of soft|hard, got %q`, c.RebootMode,
		))
	}
}

// validateNetworkModeAndZoneEnum appends an error when network_mode is not
// one of sdn|bridge|auto, and — only when the SDN path is reachable
// (network_mode sdn or auto) — when sdn_zone_type is not one of
// simple|vlan|qinq|vxlan|evpn.
func (c *CPIConfig) validateNetworkModeAndZoneEnum(errs *[]string) {
	switch c.NetworkMode {
	case NetworkModeSDN, NetworkModeBridge, NetworkModeAuto:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"network_mode must be one of sdn|bridge|auto, got %q", c.NetworkMode,
		))
	}

	if c.NetworkMode == NetworkModeSDN || c.NetworkMode == NetworkModeAuto {
		switch c.SDNZoneType {
		case "simple", "vlan", "qinq", "vxlan", "evpn":
			// valid
		default:
			*errs = append(*errs, fmt.Sprintf(
				"sdn_zone_type must be one of simple|vlan|qinq|vxlan|evpn, got %q", c.SDNZoneType,
			))
		}
	}
}

// validateCloneModeEnum appends an error when clone_mode is set to a value
// other than auto|linked|full. Empty (the default; ApplyDefaults fills
// "auto") is valid.
func (c *CPIConfig) validateCloneModeEnum(errs *[]string) {
	if c.CloneMode == "" {
		return
	}
	switch c.CloneMode {
	case CloneModeAuto, CloneModeLinked, CloneModeFull:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"clone_mode must be one of auto|linked|full, got %q", c.CloneMode,
		))
	}
}

// validateStemcellStrategyEnum appends an error when stemcell_strategy is
// set to a value other than template|import. Empty (the default;
// ApplyDefaults fills "template") is valid.
func (c *CPIConfig) validateStemcellStrategyEnum(errs *[]string) {
	if c.StemcellStrategy == "" {
		return
	}
	switch c.StemcellStrategy {
	case StemcellStrategyTemplate, StemcellStrategyImport:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"stemcell_strategy must be one of template|import, got %q", c.StemcellStrategy,
		))
	}
}

// validateDiskPerfInvariantModeEnum appends an error when
// disk_perf_invariant_mode is set to a value other than enforce|warn|off.
// Empty (the default) is valid and resolves to enforce.
func (c *CPIConfig) validateDiskPerfInvariantModeEnum(errs *[]string) {
	if c.DiskPerfInvariantMode == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.DiskPerfInvariantMode)) {
	case "enforce", "warn", enumValueOff:
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"disk_perf_invariant_mode must be one of enforce|warn|off (or empty for default enforce), got %q",
			c.DiskPerfInvariantMode,
		))
	}
}

// validateReplicaAdoptTimeoutSecField appends an error when
// replica_adopt_timeout_sec is negative. 0 disables the adopt path.
func (c *CPIConfig) validateReplicaAdoptTimeoutSecField(errs *[]string) {
	if c.ReplicaAdoptTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"replica_adopt_timeout_sec must be >= 0 (0 disables adopt-and-wait), got %d",
			c.ReplicaAdoptTimeoutSec,
		))
	}
}

// validateClusterLockModeEnum appends an error when cluster_lock_mode is
// set to a value other than off|pool. Empty (the default) is valid.
func (c *CPIConfig) validateClusterLockModeEnum(errs *[]string) {
	if c.ClusterLock == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(c.ClusterLock)) {
	case enumValueOff, "pool":
		// valid
	default:
		*errs = append(*errs, fmt.Sprintf(
			"cluster_lock_mode must be one of off|pool (or empty for default off), got %q",
			c.ClusterLock,
		))
	}
}

// validateNetworkTimingFields appends an error for each of
// cluster_lock_timeout_sec, network_resolve_retries,
// network_resolve_timeout_sec, and ephemeral_disk_min_ratio that carries an
// out-of-range value. Grouped together since each is an independent
// "must be >= 0" numeric bound with no cross-field interaction.
func (c *CPIConfig) validateNetworkTimingFields(errs *[]string) {
	// ClusterLockTimeoutSec: 0 resolves to the 60s default; negative is invalid.
	if c.ClusterLockTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"cluster_lock_timeout_sec must be >= 0 (0 means default 60s), got %d",
			c.ClusterLockTimeoutSec,
		))
	}

	// NetworkResolveRetries: unset resolves to the default (30); an
	// explicit 0 disables the SDN convergence gates; negative is invalid.
	if c.NetworkResolveRetries != nil && *c.NetworkResolveRetries < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"network_resolve_retries must be >= 0 (0 disables SDN convergence polling), got %d",
			*c.NetworkResolveRetries,
		))
	}

	// NetworkResolveTimeoutSec: 0 resolves to the 60s default; negative is invalid.
	if c.NetworkResolveTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"network_resolve_timeout_sec must be >= 0 (0 means default 60s), got %d",
			c.NetworkResolveTimeoutSec,
		))
	}

	// EphemeralDiskMinRatio: 0 disables the invariant; negative is invalid.
	if c.EphemeralDiskMinRatio < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"ephemeral_disk_min_ratio must be >= 0 (0 disables the check), got %v",
			c.EphemeralDiskMinRatio,
		))
	}
}

// validateStemcellStagingDirField appends an error when
// stemcell_staging_dir is set but is not an absolute path to an existing
// directory. Empty (the default) is valid.
func (c *CPIConfig) validateStemcellStagingDirField(errs *[]string) {
	if c.StemcellStagingDir == "" {
		return
	}
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

// validateResizeConvergenceTimeoutSecField appends an error when
// resize_convergence_timeout_sec is negative. 0 resolves to the default
// 120s; an overly long budget is allowed (operator's choice) — only
// nonsense is rejected.
func (c *CPIConfig) validateResizeConvergenceTimeoutSecField(errs *[]string) {
	if c.ResizeConvergenceTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"resize_convergence_timeout_sec must be >= 0 (0 means default 120s), got %d",
			c.ResizeConvergenceTimeoutSec,
		))
	}
}

// validatePVECACertPEMField appends an error when pve_ca_cert is set and
// verify_ssl is true but the PEM does not parse to at least one valid
// certificate. Malformed PEM at startup is rejected so the operator learns
// immediately rather than encountering TLS errors at runtime. When
// verify_ssl=false the CA cert is ignored (insecure-skip-verify wins).
func (c *CPIConfig) validatePVECACertPEMField(errs *[]string) {
	if c.PVECACertPEM != "" && c.VerifySSLValue() {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.PVECACertPEM)) {
			*errs = append(*errs, "pve_ca_cert: no valid PEM certificates parsed from value")
		}
	}
}

// validateSDNFields appends an error for each malformed SDN overlay field:
// VXLAN peer IPs, the VNI auto-allocation band, and the zone MTU override.
//
// Design contract (SDN-only validation must not leak into bridge
// mode, but must not go silent on a genuine operator typo either): every
// check below is gated on "the field is explicitly set", NOT on
// c.NetworkMode. A config migrated to network_mode: bridge that still
// carries a stale/malformed sdn_* field left over from an SDN deployment
// gets flagged just as loudly as it would under sdn/auto — the operator
// set it, so catching the typo beats silently ignoring it (matching the
// zone-type enum and 4094-cap checks' own precedent for what "reachable"
// means). Only the 4094 VLAN-ID cap sub-check is mode-gated on top of that:
// its correct value depends on the EFFECTIVE zone type, which is only
// resolvable once the SDN path actually runs (bridge mode never resolves a
// zone at all), so a stale band under bridge mode is left unenforced by
// design — see sdnReachable below.
//
// None of these gates fire on defaults alone: ApplyDefaults' own band fill
// (5000..5999, or 2000..2999 for vlan/qinq) is always self-consistent, so a
// zero-config deployment in any mode never trips this function.
func (c *CPIConfig) validateSDNFields(errs *[]string) {
	sdnReachable := c.NetworkMode == NetworkModeSDN || c.NetworkMode == NetworkModeAuto

	// VXLAN peer IPs — each entry must parse as an IP address. An empty list
	// (the default, and the only state a pure bridge-mode deployment ever
	// reaches) is always a no-op; a non-empty list is validated in every
	// mode because a non-empty value can only come from the operator.
	if sdnReachable || len(c.SDNVxlanPeers) > 0 {
		for _, peer := range c.SDNVxlanPeers {
			if net.ParseIP(peer) == nil {
				*errs = append(*errs, fmt.Sprintf(
					"sdn_vxlan_peers entry %q is not a valid IP address", peer,
				))
			}
		}
	}

	// VNI band — full 24-bit VNI space; the vlan/qinq 4094 cap is enforced at
	// allocation time where the effective zone type is known.
	if c.SDNVNIRangeStart != 0 || c.SDNVNIRangeEnd != 0 {
		if c.SDNVNIRangeStart < 1 || c.SDNVNIRangeEnd > 16777215 || c.SDNVNIRangeStart > c.SDNVNIRangeEnd {
			*errs = append(*errs, fmt.Sprintf(
				"sdn_vni_range must satisfy 1 <= start <= end <= 16777215, got %d..%d",
				c.SDNVNIRangeStart, c.SDNVNIRangeEnd,
			))
		}
		// For 802.1Q-capped zone types the whole band must sit inside the
		// 4094 VLAN ID cap — fail at Load time rather than on the first
		// auto-allocated tag inside create_network. The allocation-time clamp
		// stays as backstop for pre-existing zones whose effective type
		// differs from the configured one. Only enforced when the SDN path is
		// reachable (mode sdn/auto), mirroring the zone-type enum validation:
		// under network_mode bridge, sdn_zone_type is inert and stale SDN
		// fields left over from a migrated config must not reject the load.
		if sdnReachable && sdnZoneTypeIsVLANCapped(c.SDNZoneType) && c.SDNVNIRangeEnd > 4094 {
			*errs = append(*errs, fmt.Sprintf(
				"sdn_vni_range %d..%d exceeds the 4094 VLAN ID cap for sdn_zone_type %q",
				c.SDNVNIRangeStart, c.SDNVNIRangeEnd, c.SDNZoneType,
			))
		}
	}

	// Zone MTU — sane Ethernet/jumbo bounds when set. A nil pointer (the
	// default) is always a no-op; a set value is validated in every mode —
	// like the peer list, a non-nil value can only come from the operator.
	if c.SDNZoneMTU != nil && (*c.SDNZoneMTU < 576 || *c.SDNZoneMTU > 65520) {
		*errs = append(*errs, fmt.Sprintf(
			"sdn_zone_mtu must be within 576..65520, got %d", *c.SDNZoneMTU,
		))
	}
}

// sdnZoneTypeIsVLANCapped reports whether the zone type carries 802.1Q VLAN
// IDs as vnet tags, capping them at 4094 (vlan, qinq). Untagged (simple) and
// 24-bit-VNI types (vxlan, evpn) are not capped.
func sdnZoneTypeIsVLANCapped(zoneType string) bool {
	return zoneType == "vlan" || zoneType == "qinq"
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

	// max_inflight_per_node: 0 = unlimited (no gating, byte-identical). Negative
	// is always invalid since it would produce a zero-capacity channel at runtime.
	if c.MaxInflightPerNode < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"max_inflight_per_node must be >= 0, got %d", c.MaxInflightPerNode))
	}

	// stemcell_replication_concurrency: 0 = serial default (resolved to 1).
	// Negative is invalid. Values > 64 are rejected as an unreasonable cap (a
	// PVE cluster rarely exceeds 32 nodes; the concurrency limit cannot exceed
	// the node count in practice, but the config cannot know that at load time,
	// so 64 is chosen as a sane guard).
	if c.StemcellReplicationConcurrency < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell_replication_concurrency must be >= 0, got %d", c.StemcellReplicationConcurrency))
	} else if c.StemcellReplicationConcurrency > 64 {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell_replication_concurrency must be <= 64, got %d", c.StemcellReplicationConcurrency))
	}

	// PVE API transport tuning: 0 = SDK default (byte-identical). Negative values
	// are never valid (they would pass nonsensical negative durations to the
	// transport); reject them at config load time.
	if c.PVEDialTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"pve_api_dial_timeout_sec must be >= 0, got %d", c.PVEDialTimeoutSec))
	}
	if c.PVETLSHandshakeTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"pve_api_tls_handshake_timeout_sec must be >= 0, got %d", c.PVETLSHandshakeTimeoutSec))
	}
	if c.PVEMaxIdleConnsPerHost < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"pve_api_max_idle_conns_per_host must be >= 0, got %d", c.PVEMaxIdleConnsPerHost))
	}
	if c.PVEIdleConnTimeoutSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"pve_api_idle_conn_timeout_sec must be >= 0, got %d", c.PVEIdleConnTimeoutSec))
	}
	if c.PVETCPKeepAliveSec < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"pve_api_tcp_keepalive_sec must be >= 0, got %d", c.PVETCPKeepAliveSec))
	}
}

// Default VMID band bounds, inlined because config cannot import internal/pve
// (cycle). They mirror pve.VMIDRangeDiskStart/End and pve.VMIDRangeTemplate*.
const (
	defaultDiskVMIDStart     = 9000
	defaultDiskVMIDEnd       = 29999
	defaultTemplateVMIDStart = 30000
	defaultTemplateVMIDEnd   = 30999
	defaultParkerVMIDStart   = 90000
	defaultParkerVMIDEnd     = 90999
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
		if start < minVMID {
			*errs = append(*errs, fmt.Sprintf(
				"%s_start must be ≥%d (PVE reserved range), got %d", name, minVMID, start))
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
			"persistent disk VMID range [%d,%d] overlaps VM VMID range [%d,%d]; "+
				"adjust disk_vmid_range_start/_end or vmid_range_start/_end so the bands are disjoint",
			diskStart, diskEnd, c.VMIDRangeStart, c.VMIDRangeEnd))
	}
	if vmOK && tOK && rangesOverlap(c.VMIDRangeStart, c.VMIDRangeEnd, tStart, tEnd) {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell template VMID range [%d,%d] overlaps VM VMID range [%d,%d]; "+
				"adjust stemcell_template_vmid_range_start/_end or vmid_range_start/_end so the bands are disjoint",
			tStart, tEnd, c.VMIDRangeStart, c.VMIDRangeEnd))
	}
	if diskOK && tOK && rangesOverlap(diskStart, diskEnd, tStart, tEnd) {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell template VMID range [%d,%d] overlaps persistent disk range [%d,%d]; "+
				"adjust stemcell_template_vmid_range_start/_end or disk_vmid_range_start/_end so the bands are disjoint",
			tStart, tEnd, diskStart, diskEnd))
	}

	// Parker band validation. The band is always in force (ApplyDefaults fills
	// it under both strategies), so bounds-check it unconditionally. A zero
	// bound means ApplyDefaults has not run on this value yet, so validate what
	// effectiveParkerBand would fill rather than reporting a bogus zero-bound
	// error (same treatment the disk and template bands get above). Validating
	// the same effective values the accessors hand to pve.ParkDisk is what
	// keeps a config that loads from failing at request time.
	pStart, pEnd := c.effectiveParkerBand()
	checkBounds("parked_disk_vmid_range", pStart, pEnd)
	// The overlap checks exist to stop the CPI allocating a parker VMID that a
	// VM, disk, or template will also claim. Only the parked strategy allocates
	// one. Under "free" the band is read-only -- it keeps the unpark probes
	// running for disks parked before the opt-out -- and every parker
	// classification also requires the bosh-parker tag, so an overlap there
	// cannot mistake a workload VM for a parker. Rejecting it anyway would fail
	// every request routed to a cpi-config entry that opted out precisely
	// because its VMID layout has no room for a parker band.
	if c.DetachedDiskParkedEnabled() {
		pOK := pStart < pEnd
		if vmOK && pOK && rangesOverlap(c.VMIDRangeStart, c.VMIDRangeEnd, pStart, pEnd) {
			*errs = append(*errs, fmt.Sprintf(
				"parker VMID range [%d,%d] overlaps VM VMID range [%d,%d]; "+
					"adjust parked_disk_vmid_range_start/_end or vmid_range_start/_end so the bands are disjoint",
				pStart, pEnd, c.VMIDRangeStart, c.VMIDRangeEnd))
		}
		if diskOK && pOK && rangesOverlap(diskStart, diskEnd, pStart, pEnd) {
			*errs = append(*errs, fmt.Sprintf(
				"parker VMID range [%d,%d] overlaps persistent disk range [%d,%d]; "+
					"adjust parked_disk_vmid_range_start/_end or disk_vmid_range_start/_end so the bands are disjoint",
				pStart, pEnd, diskStart, diskEnd))
		}
		if tOK && pOK && rangesOverlap(tStart, tEnd, pStart, pEnd) {
			*errs = append(*errs, fmt.Sprintf(
				"parker VMID range [%d,%d] overlaps stemcell template range [%d,%d]; "+
					"adjust parked_disk_vmid_range_start/_end or stemcell_template_vmid_range_start/_end so the bands are disjoint",
				pStart, pEnd, tStart, tEnd))
		}
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
	// Node-affinity HA pin: cross-field rules.
	c.validateHANodeAffinityPin(errs)
	// FallbackMax: must be in [0, 5].
	const placementFallbackMaxCap = 5
	if c.Placement.FallbackMax != nil {
		v := *c.Placement.FallbackMax
		if v < 0 {
			*errs = append(*errs, fmt.Sprintf(
				"placement.fallback_max must be >= 0, got %d", v,
			))
		} else if v > placementFallbackMaxCap {
			*errs = append(*errs, fmt.Sprintf(
				"placement.fallback_max must be <= %d, got %d", placementFallbackMaxCap, v,
			))
		}
	}
	// StorageHeadroomMB: must be >= 0 when set.
	if c.Placement.StorageHeadroomMB != nil && *c.Placement.StorageHeadroomMB < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"placement.storage_headroom_mb must be >= 0, got %d", *c.Placement.StorageHeadroomMB,
		))
	}
}

// validateHANodeAffinityPin enforces the cross-field rules for the
// pin_az_via_ha_rules option: it requires an AZMap to pin against, and it is
// incompatible with the DLB sentinel AZ (DLB intentionally un-pins guests, so a
// durable pin would fight the rebalancer).
func (c *CPIConfig) validateHANodeAffinityPin(errs *[]string) {
	if c.Placement == nil || c.Placement.PinAZViaHARules == nil || !*c.Placement.PinAZViaHARules {
		return
	}
	if len(c.Placement.AZMap) == 0 {
		*errs = append(*errs,
			"placement.pin_az_via_ha_rules requires a non-empty placement.az_map to pin against")
	}
	sentinel := c.DLBAZName()
	if sentinel != "" {
		if _, ok := c.Placement.AZMap[sentinel]; ok {
			*errs = append(*errs, fmt.Sprintf(
				"placement.pin_az_via_ha_rules is incompatible with the DLB sentinel AZ %q in az_map; "+
					"DLB intentionally un-pins guests", sentinel))
		}
	}
}

// validateStorage validates the optional Storage block. A nil block is valid
// (gate disabled, zero behavior change). MaxUtilizationPct must fall in
// [0, 100] when set; MaxUtilizationMode must be enforce|warn when non-empty,
// validated regardless of whether MaxUtilizationPct is currently positive so
// a typo is caught at startup even if the operator has not yet raised the
// ceiling above 0.
func (c *CPIConfig) validateStorage(errs *[]string) {
	if c.Storage == nil {
		return
	}
	if c.Storage.MaxUtilizationPct != nil {
		pct := *c.Storage.MaxUtilizationPct
		if pct < 0 || pct > 100 {
			*errs = append(*errs, fmt.Sprintf(
				"storage.max_utilization_pct must be between 0 and 100, got %d", pct,
			))
		}
	}
	if c.Storage.MaxUtilizationMode != "" {
		switch strings.ToLower(strings.TrimSpace(c.Storage.MaxUtilizationMode)) {
		case "enforce", enumValueWarn:
			// valid
		default:
			*errs = append(*errs, fmt.Sprintf(
				"storage.max_utilization_mode must be one of enforce|warn (or empty for default enforce), got %q",
				c.Storage.MaxUtilizationMode,
			))
		}
	}
}

// validateHooks appends an error for each configured hook name that does not
// resolve in the built-in hook registry. Empty Hooks is valid (no middleware).
func (c *CPIConfig) validateHooks(errs *[]string) {
	active := make(map[string]bool, len(c.Hooks))
	for _, name := range c.Hooks {
		if !hooks.Known(name) {
			*errs = append(*errs, fmt.Sprintf(
				"unknown hook %q; known hooks: %s", name, strings.Join(hooks.Names(), ", "),
			))
			continue
		}
		active[name] = true
	}
	if active["lb_register"] {
		c.validateLBRegister(errs)
	}
	if active["external_command"] {
		c.validateExternalCommand(errs)
	}
}

// validateLBRegister enforces that an active lb_register hook has the minimum
// HAProxy Data Plane API target it needs: an endpoint and a backend name.
func (c *CPIConfig) validateLBRegister(errs *[]string) {
	if c.LBRegister == nil {
		*errs = append(*errs, "hook \"lb_register\" is active but no lb_register block is configured")
		return
	}
	if strings.TrimSpace(c.LBRegister.Endpoint) == "" {
		*errs = append(*errs, "lb_register.endpoint is required when the lb_register hook is active")
	}
	if strings.TrimSpace(c.LBRegister.Backend) == "" {
		*errs = append(*errs, "lb_register.backend is required when the lb_register hook is active")
	}
}

// validateExternalCommand enforces the safety preconditions for an active
// external_command hook: a non-empty allowlist of absolute paths, a command,
// and the command being a member of the allowlist. The runtime runner re-checks
// these, but failing fast at config load surfaces a misconfiguration before any
// dispatch.
func (c *CPIConfig) validateExternalCommand(errs *[]string) {
	if c.ExternalCommand == nil {
		*errs = append(*errs, "hook \"external_command\" is active but no external_command block is configured")
		return
	}
	ec := c.ExternalCommand
	if len(ec.Allowlist) == 0 {
		*errs = append(*errs, "external_command.allowlist must be non-empty when the external_command hook is active")
	}
	for _, p := range ec.Allowlist {
		if !filepath.IsAbs(p) {
			*errs = append(*errs, fmt.Sprintf("external_command.allowlist entry %q must be an absolute path", p))
		}
	}
	if strings.TrimSpace(ec.Command) == "" {
		*errs = append(*errs, "external_command.command is required when the external_command hook is active")
		return
	}
	if !filepath.IsAbs(ec.Command) {
		*errs = append(*errs, fmt.Sprintf("external_command.command %q must be an absolute path", ec.Command))
	}
	inAllowlist := false
	for _, p := range ec.Allowlist {
		if filepath.Clean(p) == filepath.Clean(ec.Command) {
			inAllowlist = true
			break
		}
	}
	if !inAllowlist {
		*errs = append(*errs, fmt.Sprintf("external_command.command %q must be a member of external_command.allowlist", ec.Command))
	}
}

// validateMetrics enforces that when metrics is present and enabled,
// file_path is a non-empty string. When metrics is absent or disabled, this
// is a no-op (validate-only-when-set).
func (c *CPIConfig) validateMetrics(errs *[]string) {
	if c.Metrics == nil || !c.Metrics.Enabled {
		return
	}
	if strings.TrimSpace(c.Metrics.FilePath) == "" {
		*errs = append(*errs, "metrics.file_path is required when metrics.enabled is true")
	}
}

// validateDLB validates the optional Placement.DLB sub-block. Skipped when
// Placement.DLB is nil (validate-only-when-set). All DLBConfig fields are
// optional *bool or *string with no enum or range constraints.
//
// Rule (d): when DLB is not enabled (master flag off and sentinel AZ not
// configured), setting require_shared_storage explicitly is meaningless
// because no VMs are ever DLB-registered. Under strict_config_validation=true
// this is a hard error. Under strict off it is a no-op (byte-identical).
func (c *CPIConfig) validateDLB(errs *[]string) {
	if c.Placement == nil || c.Placement.DLB == nil {
		return
	}
	dlb := c.Placement.DLB
	// No numeric, enum, or string-range constraints beyond what Go types enforce.

	// Rule (d): require_shared_storage meaningful only when DLB is active.
	// "DLB active" = master Enabled=*true OR a non-empty sentinel AZName.
	// We check the raw Enabled field (not DLBExplicitlyEnabled) to distinguish
	// "explicitly false" from "nil/absent". An explicitly false Enabled with a
	// non-nil RequireSharedStorage that deviates from the default (true) is the
	// indicator of an inconsistent config.
	if !c.StrictConfigValidationEnabled() {
		return
	}
	dlbActive := (dlb.Enabled != nil && *dlb.Enabled) || (dlb.AZName != nil && *dlb.AZName != "")
	if !dlbActive && dlb.RequireSharedStorage != nil {
		*errs = append(*errs, "placement.dlb.require_shared_storage is only meaningful when DLB is enabled (strict_config_validation=true)")
	}
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
	if sha := strings.TrimSpace(hc.ExpectedAgentSHA256); sha != "" && !isHex64(sha) {
		*errs = append(*errs, fmt.Sprintf(
			"health_check.expected_agent_sha256 must be 64 hex characters, got %q", hc.ExpectedAgentSHA256,
		))
	}
}

// isHex64 reports whether s is exactly 64 hexadecimal characters (a SHA-256 hex
// digest), case-insensitive.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
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
	checkRaw("pushback", c.Retry.Pushback)
	checkRaw("transient", c.Retry.Transient)
	checkRaw("storage_lock", c.Retry.StorageLock)
	// disk_migrate consults only max_attempts and cap_ms (a wall-clock await
	// budget, not a backoff ceiling), so like transient it gets the raw-bounds
	// check without the cap-vs-base ordering check.
	checkRaw("disk_migrate", c.Retry.DiskMigrate)

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
	checkEffective("pushback", c.Retry.Pushback, c.RetryPushback())
	checkEffective("storage_lock", c.Retry.StorageLock, c.RetryStorageLock())
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

// validateOTel appends errors for the OTel block. Trace-specific checks run
// only when Enabled is true; logs/metrics checks run only when their signal
// is enabled (validate-only-when-set, independent per signal) — a fully
// disabled block never produces a validation error, preserving
// zero-behavior-change-when-unset.
func (c *CPIConfig) validateOTel(errs *[]string) {
	// Protocol must be a known value whenever any signal is enabled. The
	// logs/metrics signals also reject empty: they have no callers that
	// predate Protocol, so requiring it is safe. Trace-only (Enabled)
	// callers that invoke Validate directly without ApplyDefaults predate
	// Protocol and may still pass empty (the normal ApplyDefaults-then-
	// Validate flow fills "http" before Validate ever sees it) — but a
	// non-empty unknown value is a misconfiguration for every signal and
	// must fail fast rather than silently selecting the http exporter.
	if c.OTel.Protocol != "http" && c.OTel.Protocol != "grpc" {
		switch {
		case c.OTel.LogsEnabled || c.OTel.MetricsEnabled,
			c.OTel.Enabled && c.OTel.Protocol != "":
			*errs = append(*errs, fmt.Sprintf(
				"otel.protocol must be \"http\" or \"grpc\", got %q", c.OTel.Protocol))
		}
	}
	if c.OTel.Enabled {
		if c.OTel.ExporterEndpoint == "" {
			*errs = append(*errs, "otel.exporter_endpoint is required when otel.enabled is true")
		}
		if c.OTel.SampleRatio < 0 || c.OTel.SampleRatio > 1 {
			*errs = append(*errs, fmt.Sprintf(
				"otel.sample_ratio must be 0.0-1.0, got %v", c.OTel.SampleRatio))
		}
		if c.OTel.ExportTimeoutMs <= 0 {
			*errs = append(*errs, fmt.Sprintf(
				"otel.export_timeout_ms must be > 0, got %d", c.OTel.ExportTimeoutMs))
		}
		// ApplyDefaults fills ServiceName from empty when Enabled is true, so
		// this only fires for a caller that invokes Validate directly
		// (bypassing ApplyDefaults) with an explicit blank override.
		if c.OTel.ServiceName == "" {
			*errs = append(*errs, "otel.service_name must not be empty when otel.enabled is true")
		}
	}
	if c.OTel.LogsEnabled && c.OTel.LogsExporterEndpoint == "" {
		*errs = append(*errs, "otel.logs_exporter_endpoint is required when otel.logs_enabled is true")
	}
	if c.OTel.MetricsEnabled && c.OTel.MetricsExporterEndpoint == "" {
		*errs = append(*errs, "otel.metrics_exporter_endpoint is required when otel.metrics_enabled is true")
	}
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

// knownDiskAioModes is the set of PVE per-disk AsyncIO backend strings
// accepted in DiskPerformanceDefaults.AIO. Mirrors PVE's own qm.conf
// asyncio enum; an empty string means "no override" and is valid.
var knownDiskAioModes = map[string]struct{}{
	"native":   {},
	"io_uring": {},
	"threads":  {},
}

// IsKnownDiskAioMode reports whether mode is a PVE per-disk AsyncIO backend
// the CPI accepts. An empty string ("no override") is reported as valid.
// Exported so the handlers package can validate call-time aio values against
// the single authoritative set without duplicating the literals.
func IsKnownDiskAioMode(mode string) bool {
	if mode == "" {
		return true
	}
	_, ok := knownDiskAioModes[mode]
	return ok
}

// validateDiskPerformance validates the optional DiskPerformance block.
// Skipped entirely when DiskPerformance is nil (validate-only-when-set).
// Rules enforced when the block is present:
//   - Cache non-empty and not in {none,writethrough,writeback,unsafe,directsync} → error.
//   - MBpsRd/MBpsWr non-nil and < 0 → error.
//   - IOPSRd/IOPSWr non-nil and < 0 → error.
//   - AIO non-empty and not in {native,io_uring,threads} → error.
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
	if dp.AIO != "" {
		if _, ok := knownDiskAioModes[dp.AIO]; !ok {
			*errs = append(*errs, fmt.Sprintf(
				"disk_performance.aio must be one of native|io_uring|threads, got %q",
				dp.AIO,
			))
		}
	}
}

// clusterLockPoolPrefix mirrors internal/pve's own clusterLockPoolPrefix
// constant (see internal/pve/cluster_lock.go). internal/pve imports this
// config package, so the reverse import needed to reference that constant
// directly would create a cycle; the literal is duplicated here instead,
// with both copies documented as namespacing the same reserved space.
const clusterLockPoolPrefix = "bosh-lock-"

// poolIDCharsetRe matches the PVE poolid charset: letters, digits, '.', '_',
// '-'. Used to validate VMPool, StemcellTemplatePool, and (token-substituted)
// VMPoolTemplate — the CPI never fabricates a name PVE itself would reject.
var poolIDCharsetRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateFlatPoolName appends at most one error when name (the configured
// value of the given field, e.g. "vm_pool") is not flat (contains '/') or
// does not match the PVE poolid charset. A '/' already fails the charset
// check too, so the slash case is checked first to avoid a redundant second
// error for the same string. Shared by validateVMPool and
// validateStemcellTemplatePool so both fields report identically worded
// errors; callers own their own field-specific checks (equality,
// bosh-lock- prefix).
func validateFlatPoolName(field, name string, errs *[]string) {
	switch {
	case strings.Contains(name, "/"):
		*errs = append(*errs, fmt.Sprintf(
			"%s must not contain '/' (got %q); the CPI only manages flat pool names, not nested ids",
			field, name,
		))
	case !poolIDCharsetRe.MatchString(name):
		*errs = append(*errs, fmt.Sprintf(
			"%s contains characters invalid for a PVE poolid (allowed: letters, digits, '.', '_', '-'), got %q",
			field, name,
		))
	}
}

// validateVMPool rejects a VMPool value that collides with a pool namespace
// the CPI itself already owns, or that cannot be a valid flat PVE poolid:
//   - StemcellTemplatePool: assigning template VMs and regular VMs to the
//     same pool would let a create_vm-scoped ACL also touch stemcell
//     templates (and vice versa), defeating the point of a dedicated pool.
//     This equality check lives solely here — validateStemcellTemplatePool
//     does not repeat it — so the collision is reported exactly once.
//   - A '/' or a poolid-charset violation: the CPI only creates and assigns
//     flat pools (see resolvePoolName in internal/cpi/handlers).
//   - The cluster-lock sentinel namespace ("bosh-lock-" prefix, see
//     internal/pve/cluster_lock.go): a VM pool inside that namespace could
//     collide with a dynamically-named sentinel pool the CPI creates and
//     deletes on demand for anti-affinity locking, corrupting the mutex.
//
// Skipped entirely when VMPool is empty (validate-only-when-set).
func (c *CPIConfig) validateVMPool(errs *[]string) {
	if c.VMPool == "" {
		return
	}
	if c.VMPool == c.StemcellTemplatePool {
		*errs = append(*errs, fmt.Sprintf(
			"vm_pool must not equal stemcell_template_pool (both %q); use a distinct pool for VMs so their "+
				"ACL scope does not also cover stemcell templates",
			c.VMPool,
		))
	}
	validateFlatPoolName("vm_pool", c.VMPool, errs)
	if strings.HasPrefix(c.VMPool, clusterLockPoolPrefix) {
		*errs = append(*errs, fmt.Sprintf(
			"vm_pool must not start with %q (reserved for cluster-lock sentinel pools), got %q",
			clusterLockPoolPrefix, c.VMPool,
		))
	}
}

// validateStemcellTemplatePool rejects a StemcellTemplatePool value that
// cannot be a valid flat PVE poolid, or that collides with the cluster-lock
// sentinel namespace. It deliberately does NOT check equality against
// VMPool — that rule lives solely in validateVMPool (see its doc comment) —
// so a vm_pool/stemcell_template_pool collision is reported exactly once
// regardless of which field a config sets first.
//
// Skipped entirely when StemcellTemplatePool is empty (validate-only-when-set).
func (c *CPIConfig) validateStemcellTemplatePool(errs *[]string) {
	if c.StemcellTemplatePool == "" {
		return
	}
	validateFlatPoolName("stemcell_template_pool", c.StemcellTemplatePool, errs)
	if strings.HasPrefix(c.StemcellTemplatePool, clusterLockPoolPrefix) {
		*errs = append(*errs, fmt.Sprintf(
			"stemcell_template_pool must not start with %q (reserved for cluster-lock sentinel pools), got %q",
			clusterLockPoolPrefix, c.StemcellTemplatePool,
		))
	}
}

// poolTemplateTokenRe matches every "{...}" token in a VMPoolTemplate string
// so validateVMPoolTemplate can check each one against the allowed variable
// set.
var poolTemplateTokenRe = regexp.MustCompile(`\{[^{}]*\}`)

// allowedPoolTemplateTokens is the set of "{...}" variables resolvePoolName's
// renderPoolTemplate (internal/cpi/handlers/pool_resolver.go) knows how to
// substitute. Keep in sync with that function's substitution list.
var allowedPoolTemplateTokens = map[string]bool{
	"{prefix}":         true,
	"{director}":       true,
	"{deployment}":     true,
	"{instance_group}": true,
}

// validateVMPoolTemplate rejects a VMPoolTemplate containing an unrecognized
// "{...}" token, or a literal '/'. A template that renders a '/' can only
// ever produce a nested pool id, which the CPI refuses to create — flat
// names only, mirroring the VMPool and StemcellTemplatePool rules.
//
// Skipped entirely when VMPoolTemplate is empty (validate-only-when-set).
func (c *CPIConfig) validateVMPoolTemplate(errs *[]string) {
	if c.VMPoolTemplate == "" {
		return
	}
	if strings.Contains(c.VMPoolTemplate, "/") {
		*errs = append(*errs, fmt.Sprintf(
			"vm_pool_template must not contain '/' (got %q); a rendered '/' can only produce a nested pool id, "+
				"which the CPI refuses to create",
			c.VMPoolTemplate,
		))
	}
	for _, tok := range poolTemplateTokenRe.FindAllString(c.VMPoolTemplate, -1) {
		if !allowedPoolTemplateTokens[tok] {
			*errs = append(*errs, fmt.Sprintf(
				"vm_pool_template contains unknown variable %q in %q; allowed variables are "+
					"{prefix}, {director}, {deployment}, {instance_group}",
				tok, c.VMPoolTemplate,
			))
		}
	}
}

// validateStrictCrossFields checks cross-field consistency rules that are
// advisory (no-op) when strict_config_validation is off, and hard errors when
// it is on. Called from ValidateWithLogger after all other validators so
// existing error ordering is preserved. No raw bytes required — all checks
// read struct fields directly.
//
// Rules enforced when strict is on:
//
//	(b) use_ha_rules=true requires anti_affinity.enabled=true.
//	(c) network_mode=sdn requires sdn_zone != "" OR sdn_auto_manage_zone=true.
//	    network_mode=auto is exempt (auto falls back to bridge when no zone).
//
// Rule (d) (DLB require_shared_storage without DLB enabled) lives in
// validateDLB so it shares placement's nil-guard and is co-located with DLB
// logic.
func (c *CPIConfig) validateStrictCrossFields(errs *[]string) {
	if !c.StrictConfigValidationEnabled() {
		return
	}
	c.strictCheckUseHaRules(errs)
	c.strictCheckSDNZone(errs)
}

// strictCheckUseHaRules appends an error when use_ha_rules is explicitly true
// but anti_affinity.enabled is not true. Reads the raw UseHaRules field (not
// the guarded AntiAffinityUseHaRulesEnabled accessor, which silently returns
// false when anti-affinity is off).
func (c *CPIConfig) strictCheckUseHaRules(errs *[]string) {
	if c.Placement == nil || c.Placement.AntiAffinity == nil {
		return
	}
	aa := c.Placement.AntiAffinity
	if aa.UseHaRules == nil || !*aa.UseHaRules {
		return
	}
	if aa.Enabled == nil || !*aa.Enabled {
		*errs = append(*errs, "placement.anti_affinity.use_ha_rules=true requires placement.anti_affinity.enabled=true (strict_config_validation=true)")
	}
}

// strictCheckSDNZone appends an error when network_mode=sdn but neither
// sdn_zone nor sdn_auto_manage_zone satisfies the zone requirement.
// network_mode=auto is exempt because auto falls back to bridge when no zone.
func (c *CPIConfig) strictCheckSDNZone(errs *[]string) {
	if c.NetworkMode != NetworkModeSDN {
		return
	}
	if c.SDNZone != "" || c.SDNAutoManageZoneEnabled() {
		return
	}
	*errs = append(*errs, "network_mode=sdn requires sdn_zone or sdn_auto_manage_zone=true (strict_config_validation=true)")
}

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
		if len(criteria.Types) == 0 && criteria.Shared == nil && criteria.Encrypted == nil {
			*errs = append(*errs, fmt.Sprintf(
				"storage_tiers[%s]: must set at least one of types, shared, or encrypted", name,
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
