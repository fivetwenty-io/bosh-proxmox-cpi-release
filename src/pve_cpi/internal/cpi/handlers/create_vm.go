package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// newCreateVMRetryBackoff builds the backoff function used between
// AllocateWithRetry attempts in create_vm from the operator's retry policies.
// VMID conflicts only need a brief jitter to decorrelate herds across
// concurrent CPI processes (the vmid_alloc curve: uniform in [BaseMs,CapMs]);
// storage lock-file timeouts mean PVE is serialising imports against a busy
// storage and retrying immediately wins us nothing — back off exponentially
// (the storage_import curve: BaseMs × 1.5^attempt, ±JitterPct, capped at
// CapMs). With unset config both curves resolve to the constants the CPI
// shipped with (2s × 1.5, cap 30s, ±30%; and 50–250ms), so the default
// behavior is byte-identical to prior releases.
func newCreateVMRetryBackoff(storage, vmid config.EffectiveRetryPolicy) func(err error, attempt int) time.Duration {
	storageBase := time.Duration(storage.BaseMs) * time.Millisecond
	storageCap := time.Duration(storage.CapMs) * time.Millisecond
	vmidBase := time.Duration(vmid.BaseMs) * time.Millisecond
	vmidSpan := time.Duration(vmid.CapMs-vmid.BaseMs) * time.Millisecond
	jitterPct := int64(storage.JitterPct)

	return func(err error, attempt int) time.Duration {
		if pve.IsStorageLockTimeout(err) {
			// Exponential storageBase × 1.5^attempt, capped at storageCap.
			factor := 1.0
			for i := 0; i < attempt; i++ {
				factor *= 1.5
			}
			d := time.Duration(float64(storageBase) * factor)
			if d > storageCap {
				d = storageCap
			}
			// Jitter ±jitterPct: d - d*p/100 + rand(d*2*p/100).
			if jitterPct > 0 && d > 0 {
				span := int64(d) * 2 * jitterPct / 100
				if span > 0 {
					jitter := time.Duration(mrand.Int64N(span)) // #nosec G404 -- retry jitter; non-cryptographic
					return d - time.Duration(int64(d)*jitterPct/100) + jitter
				}
			}
			return d
		}
		// VMID conflict (or anything else flagged retryable): uniform draw in
		// [vmidBase, vmidBase+vmidSpan].
		if vmidSpan > 0 {
			return vmidBase + time.Duration(mrand.Int64N(int64(vmidSpan))) // #nosec G404 -- retry jitter; non-cryptographic
		}
		return vmidBase
	}
}

// defaultStemcellDiskGiB is the minimum root-disk size assumed when
// cloud_properties.disk is absent or zero. Stemcells are always at least
// this large; PVE refuses to shrink below the imported image size anyway.
const defaultStemcellDiskGiB = 5

// defaultNetworkBridge is the PVE Linux bridge used when neither the CPI
// config nor the network cloud_properties specifies an explicit bridge.
const defaultNetworkBridge = "vmbr0"

// metadataKeyName is the BOSH metadata map key that carries the VM name
// (e.g. "<job>/<id>"). Defined as a constant to avoid repeated string
// literals across the handlers package.
const metadataKeyName = "name"

const metadataKeyVMID = "vmid"

// defaultNetworkName is the BOSH reserved name for the primary network.
// sortedNetworkNames always places it first when present.
const defaultNetworkName = "default"

// nicTypeDynamic is the BOSH network spec type for a dynamically-assigned
// (DHCP) NIC. Distinct from crsModeDynamic (a PVE CRS placement mode) which
// shares the literal value but belongs to an unrelated domain.
const nicTypeDynamic = "dynamic"

// nicTypeManual is the BOSH network spec type for a statically-assigned NIC.
// Compared case-insensitively against spec.Type throughout the handler.
const nicTypeManual = "manual"

// nicTypeVIP is the BOSH network spec type for a virtual/floating IP. It is
// routing-level only: no PVE NIC address is configured for it.
const nicTypeVIP = "vip"

// nicCPKeyBridge, nicCPKeyModel, nicCPKeyFirewall, nicCPKeyVLAN, and
// nicCPKeyMTU are the cloud_properties map keys used in both per-NIC network
// specs and VM-level network_defaults (§7.34). Defined as constants to
// satisfy goconst (>3 occurrences across the package) and to make the key
// contract explicit. vlan (int, 1-4094) appends a plain 802.1Q tag= to the
// NIC's net{i} string for an operator-managed trunk bridge — no SDN
// involvement. mtu (int, 1 = inherit bridge MTU, or 576-65520) is
// virtio-model-only and wins over the automatic vnet-derived mtu=1
// inheritance below.
const (
	nicCPKeyBridge   = "bridge"
	nicCPKeyModel    = "model"
	nicCPKeyFirewall = "firewall"
	nicCPKeyVLAN     = "vlan"
	nicCPKeyMTU      = "mtu"
)

// diskKeyVirtio0 is the PVE VM config key for the primary root disk under the
// default pve.root_disk_bus (unset/"virtio"). Used across create_vm,
// create_stemcell, and get_disks to avoid repeated literals.
const diskKeyVirtio0 = "virtio0"

// diskKeyScsi0 is the PVE VM config key for the primary root disk when
// pve.root_disk_bus is set to "scsi". See rootDiskKey.
const diskKeyScsi0 = "scsi0"

// rootDiskKey returns the PVE VM config key the root (system) disk is
// created under: "virtio0" (default) or "scsi0" when
// cfg.RootDiskUsesSCSI() is true. Both create_vm and create_stemcell use
// this so a template's root disk always lands under the same key a
// subsequent create_vm clone of it expects.
func rootDiskKey(cfg *config.CPIConfig) string {
	if cfg.RootDiskUsesSCSI() {
		return diskKeyScsi0
	}
	return diskKeyVirtio0
}

// rootDiskBusName returns the filterDiskPerfForBus bus label matching
// rootDiskKey: "virtio" (drops "ssd" — virtio-blk has no rotation-rate flag)
// or "scsi" (keeps "ssd", enabling discard/ssd auto-resolution on a root
// disk that lives on the same virtio-scsi controller as persistent disks).
func rootDiskBusName(cfg *config.CPIConfig) string {
	if cfg.RootDiskUsesSCSI() {
		return "scsi"
	}
	return "virtio"
}

// createVMCloudProps holds the fields we care about from Args[2].
type createVMCloudProps struct {
	// CPU is the total vCPU count using the vSphere CPI convention. When
	// set and Cores is unset, the VM is created with Cores=CPU, Sockets=1
	// (PVE's cores-per-socket sums to total vCPUs at runtime).
	CPU                 int    `json:"cpu"`
	Cores               int    `json:"cores"`
	Sockets             int    `json:"sockets"`
	Memory              int    `json:"memory"`         // MiB
	RAM                 int    `json:"ram"`            // MiB (alias for memory)
	VMDiskFormat        string `json:"vm_disk_format"` // qcow2|raw|vmdk
	TargetNode          string `json:"target_node"`
	EphemeralDiskSizeMB int    `json:"ephemeral_disk_size_mb"`
	// Disk is the requested root disk size in MiB. BOSH puts this on
	// resource_pools/.../cloud_properties.disk. Stemcell base is typically
	// 5 GiB; the size= directive in the virtio0 import-from param sets the
	// root disk to max(defaultStemcellDiskGiB, ceil(Disk/1024)) GiB so
	// the agent has room to carve an ephemeral partition (CreatePartitionIfNoEphemeralDisk).
	Disk int `json:"disk"`
	// RootDiskSize is the requested root disk size in MiB. Takes precedence
	// over Disk when both are set. Zero = use Disk field or defaultStemcellDiskGiB.
	RootDiskSize int `json:"root_disk_size"`
	// EphemeralStoragePool is the PVE storage pool for the dedicated ephemeral disk.
	// Opt-in; empty = falls back to cfg.VMStorage. Layered resolver key
	// "ephemeral_storage_pool" also feeds this (resolver wins over struct field).
	EphemeralStoragePool string `json:"ephemeral_storage_pool"`
	NetworkBridge        string `json:"network_bridge"` // per-VM bridge override
	NetworkModel         string `json:"network_model"`  // virtio|e1000 etc.
	// Hotplug overrides the CPI default for this VM (config.Hotplug).
	// Pointer-typed so the caller can distinguish "not set" (use config
	// default) from "set to empty string" (currently treated the same:
	// fall back to default). Use "0" to explicitly disable hotplug.
	Hotplug *string `json:"hotplug,omitempty"`
	// CPUHotplug controls the "cpu" token in the PVE hotplug string.
	// true → ensure "cpu" token present; false → remove "cpu" token;
	// nil (default) → no change, byte-identical to pre-feature behavior.
	// Applied as a post-merge overlay after the base hotplug string is
	// resolved via Hotplug / profile / config precedence.
	CPUHotplug *bool `json:"cpu_hotplug,omitempty"`
	// MemoryHotplug controls the "memory" token in the PVE hotplug string.
	// true → ensure "memory" token present AND forces numaEnabled=true
	// (PVE requires numa=1 for memory hotplug to allocate DIMM slots);
	// false → remove "memory" token; nil (default) → no change.
	// When true, overrides any explicit cp.NUMA=false — memory hotplug
	// cannot function without NUMA enabled.
	MemoryHotplug *bool `json:"memory_hotplug,omitempty"`
	// NUMA overrides the CPI default (config.NUMA). PVE requires numa=1
	// at create time for memory hotplug to allocate DIMM slots; setting
	// false here disables NUMA for the new VM regardless of the global
	// default. Pointer-typed so explicit false survives JSON omission.
	NUMA *bool `json:"numa,omitempty"`
	// Tags is an operator-supplied map applied to the new VM's PVE tags
	// field as sanitized "<key>--<value>" entries. The BOSH-reserved
	// director/deployment/job triple is not known at create_vm time
	// (BOSH supplies it via set_vm_metadata after the agent settles), so
	// these entries are purely custom. set_vm_metadata preserves them
	// across re-syncs.
	Tags map[string]string `json:"tags"`
	// PVEConfig is an optional map of PVE VM config key=value pairs applied
	// post-clone via a single UpdateQemuConfig call. Only keys in the
	// allowlist (machine, bios, cpu) are accepted; CPI-managed keys (cores,
	// memory, sockets, netN, scsiN, ideN, virtioN, boot, name, tags,
	// hotplug, numa, smbios1, agent, onboot, tablet, vmgenid, description,
	// ostype) and "args" are rejected non-retriable. Values containing shell
	// metacharacters (;&|$`<>) are also rejected. Nil or empty map = no API
	// call (byte-identical to prior behavior).
	PVEConfig map[string]string `json:"pve_config,omitempty"`
	// AvailabilityZone restricts node-scoring to the nodes declared in
	// config.placement.az_map[availability_zone]. When set and the AZ key
	// is absent from the map, create_vm returns a CloudError (operator
	// misconfiguration). When empty, all online nodes are candidates.
	// Singular form; takes precedence over AvailabilityZones when both set.
	AvailabilityZone string `json:"availability_zone,omitempty"`
	// AvailabilityZones lists AZ names in preference order. When set and
	// AvailabilityZone (singular) is empty, placement iterates these AZs in
	// order (with optional shuffle via placement.az_shuffle), advancing to
	// the next AZ when the current one yields no viable candidates after
	// filter. Mutually exclusive with AvailabilityZone; singular wins when
	// both are present. Requires config.placement.az_map entries for each AZ.
	AvailabilityZones []string `json:"availability_zones,omitempty"`
	// SecurityGroups names PVE cluster firewall groups
	// (/cluster/firewall/groups) to attach to the new VM. Each group must
	// already exist in PVE; the CPI references group content but never creates
	// or modifies it. After attaching the groups the CPI enables the VM-level
	// firewall so the rules take effect. A missing group is a non-retriable
	// CloudError. Empty (default) means no firewall API calls are made.
	SecurityGroups []string `json:"security_groups,omitempty"`
	// NetworkDefaults is an optional map of NIC attribute overrides that apply
	// to every NIC created by this VM, regardless of the per-NIC
	// spec.cloud_properties. It is the final override layer in the precedence
	// chain (highest priority):
	//
	//   NetworkDefaults[key] > per-NIC spec.CloudProperties[key] > resolver default
	//
	// Supported keys: "bridge" (string), "model" (string), "firewall" (bool),
	// "vlan" (int, 1-4094 — plain 802.1Q tag=, no SDN involvement), "mtu"
	// (int, 1 = inherit bridge MTU or 576-65520, virtio-model-only).
	// Unknown keys are ignored gracefully — this is a cloud_property map, not
	// CPI config, so strict validation does not apply here.
	// Absent map or absent key → unchanged (byte-identical to pre-override behavior).
	NetworkDefaults map[string]any `json:"network_defaults,omitempty"`
	// Encrypted is the per-VM opt-in for encrypted-storage placement on the
	// ephemeral disk (§7.49). When *true, ephemeral disk storage-tier selection
	// is restricted to tiers marked Encrypted:*true in config.StorageTiers.
	// Overrides the global CPIConfig.Encrypted (per-call > global > off). When
	// nil, the global setting applies. Pointer-typed; absent JSON key leaves nil.
	Encrypted *bool `json:"encrypted,omitempty"`
	// RetainEphemeralOnDelete opts the VM's ephemeral disk out of purge when
	// delete_vm is called. When *true, create_vm stamps the tag tagRetainEphemeral
	// ("bosh-retain-ephemeral") onto the VM. The tag survives set_vm_metadata's
	// tag RMW (not in reservedBoshTagPrefixes). On delete_vm both paths check for
	// this tag: when present, the ephemeral disk slot (volid containing
	// "vm-<vmid>-ephemeral-") is unlinked (force=false → unusedN), the unusedN
	// config entry is then removed without freeing storage, and the volid is WARN-
	// logged for operator recovery. DeleteQemu proceeds after unlink and does not
	// see any reference to the ephemeral volume, so the backing storage survives.
	// Nil → byte-identical (no tag, no unlink, ephemeral is destroyed with the VM).
	RetainEphemeralOnDelete *bool `json:"retain_ephemeral_on_delete,omitempty"`
	// PCIPassthroughs lists host PCI devices to pass through to the VM.
	// Each entry carries a PCI address (e.g. "0000:01:00.0") that must be
	// present on the placement target node; placement filters nodes to those
	// advertising all requested devices via /nodes/{node}/hardware/pci.
	// After clone, hostpci0..N are set on the VM via UpdateQemuConfig.
	// A strict single-node HA pin is applied automatically to block live
	// migration (PCI passthrough is incompatible with migration).
	// Operator responsibility: IOMMU group resolution is not performed by
	// the CPI; ensure the host has IOMMU enabled and that the device is not
	// shared across IOMMU groups unless the operator has verified group
	// isolation. Nil or empty → byte-identical (no PCI filter, no hostpciN
	// config, no strict pin).
	PCIPassthroughs []PCIPassthrough `json:"pci_passthroughs,omitempty"`
	// StemcellStrategy overrides the global pve.stemcell_strategy for this VM's
	// stemcell resolution. "" (default) defers to the global config value
	// (itself defaulting to "template"). "template" clones the per-cluster
	// stemcell cache template; "import" imports the stemcell qcow2 directly
	// into the VM's root disk. Any other value is a validation error at parse
	// time, before any PVE mutation.
	StemcellStrategy string `json:"stemcell_strategy,omitempty"`
	// AdvertisedRoutes lists SDN vnet subnets to create for this VM
	// post-clone. Each entry names a vnet and a destination CIDR; the CPI
	// calls CreateSdnVnetsSubnets then applySDN to commit the subnet to the
	// FRR-managed logical-router fabric. Intended for router/NAT VMs that
	// need the fabric to know about the prefixes they forward. Nil or empty
	// → no SDN subnet calls (byte-identical). Route injection requires an
	// EVPN zone (a non-EVPN zone accepts the subnet but injects no route —
	// the CPI warns and continues) and SDN write permissions. SDN is
	// eventually consistent; applySDN await covers
	// convergence. Subnets created before a rollback-triggering failure are
	// removed on a best-effort basis; if removal fails, a warning names the
	// leftover subnet for operator cleanup.
	AdvertisedRoutes []AdvertisedRoute `json:"advertised_routes,omitempty"`
}

// createVMNetworkSpec mirrors the BOSH v2 network spec shape.
type createVMNetworkSpec struct {
	Type            string         `json:"type"`
	IP              string         `json:"ip"`
	Netmask         string         `json:"netmask"`
	Gateway         string         `json:"gateway"`
	DNS             []string       `json:"dns"`
	Default         []string       `json:"default"`         // ["dns","gateway"]
	Range           string         `json:"range,omitempty"` // CIDR for static-IP containment validation
	CloudProperties map[string]any `json:"cloud_properties"`
	MAC             string         `json:"mac,omitempty"` // filled in response
	// NicGroup names the group of networks that share a single NIC. Networks
	// carrying the same non-empty value are configured onto one PVE net{N}
	// (the dual-stack case: one IPv4 and one IPv6 network on one interface).
	// Empty — the default and the only shape a director without nic_group
	// support emits — means this network gets a NIC of its own.
	//
	// The value is opaque: only equality between networks matters. Directors
	// and IaaS templates write it both ways (`nic_group: 1` in the upstream
	// BATS templates, `nic_group: nic0` here), so it decodes from a JSON
	// string or a JSON number.
	NicGroup jsonScalarString `json:"nic_group,omitempty"`
}

// jsonScalarString is a string that also accepts a JSON number, so a numeric
// value in a manifest cannot fail the whole create_vm at unmarshal time.
type jsonScalarString string

func (v *jsonScalarString) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "" || trimmed == "null":
		*v = ""
		return nil
	case trimmed[0] == '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = jsonScalarString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("expected a string or a number, got %s", trimmed)
	}
	*v = jsonScalarString(n.String())
	return nil
}

// createVMParsedArgs holds the validated, unmarshalled create_vm arguments.
type createVMParsedArgs struct {
	agentID string
	// stemcellCID is the original path-identity CID as received
	// (":light:<storage>:import/<file>" or ":heavy:<storage>:import/<file>").
	stemcellCID string
	// stemcellKind is the decoded CID family (StemcellKindLight/StemcellKindHeavy).
	// Purely informational here — create_vm treats both kinds identically;
	// delete_stemcell and pve-cid are what act on the distinction.
	stemcellKind pve.StemcellKind
	// stemcellStorage is the PVE storage pool name decoded from the CID. Used
	// both as the import-from= source storage and, when deps.Config.VMStorage
	// is empty, as the fallback destination vm_storage (see
	// resolveVMShapeStorage).
	stemcellStorage string
	// stemcellVolPath is the "import/<file>" tail decoded from the CID — the
	// PVE volume path component (without the storage prefix).
	stemcellVolPath string
	// stemcellFilename is the bare qcow2 filename (stemcellVolPath with the
	// "import/" prefix stripped), used for sha8 extraction and the
	// pre-import existence check.
	stemcellFilename string
	// rawVolid is "<storage>:import/<file>" — the bare PVE volid PVE's
	// import-from= parameter consumes. Derived once from stemcellStorage +
	// stemcellVolPath.
	rawVolid   string
	cloudProps createVMCloudProps
	// cloudPropsMap is the raw map decoded from args[2] (same JSON as
	// cloudProps). It carries the vm_type / disk_type selector keys and any
	// extra cloud_properties (e.g. storage_pool) that are not modelled as
	// struct fields, and feeds the layered resolver in resolveVMShapeStorage.
	cloudPropsMap map[string]any
	networks      map[string]createVMNetworkSpec
	diskCIDs      []string
	env           map[string]any
}

// createVMShape holds the resolved VM-shape parameters derived from
// createVMParsedArgs + Deps.Config.
type createVMShape struct {
	node      string
	vmStorage string
	// vmStorageType is the PVE storage type string for vmStorage (e.g. "dir",
	// "nfs", "lvm", "lvmthin", "zfspool"). Used by cloneFromTemplate
	// to select linked vs full clone via pve.IsLinkedCloneSupported. Empty when
	// the cluster storage list is unavailable; IsLinkedCloneSupported treats ""
	// as linked-capable (permissive default).
	vmStorageType string
	vmDiskFormat  string
	rootDiskGiB   int
	cores         int
	sockets       int
	memMiB        int
	hotplug       string
	numaEnabled   bool
	initialTags   string
	rangeStart    int
	maxAttempts   int
	initialName   string
	// cloudPropsMap is the raw cloud_properties map from the parsed args. It is
	// carried forward so cloneFromTemplate can consult it for clone_mode resolution
	// via the layered resolver (vm_type profile may override config.CloneMode).
	cloudPropsMap map[string]any
	// rootDiskPerfOpts holds the resolved PVE per-disk performance options for
	// the root disk (rootDiskKey). Derived once in resolveVMShape via
	// resolveDiskPerfOptions + filterDiskPerfForBus(rootDiskBusName(cfg)). Empty
	// map when no options are set (byte-identical path: nothing appended to the
	// root disk string).
	rootDiskPerfOpts map[string]string
	// rootDiskKey is the PVE VM config key the root disk is created under:
	// "virtio0" (default) or "scsi0" (pve.root_disk_bus=scsi). Resolved once via
	// the package-level rootDiskKey(cfg) function. Both import-path
	// (createParams) and clone-path (template-inherited key, verified against
	// this value) use it.
	rootDiskKey string
	// scsihw is the resolved SCSI controller model. Defaults to "virtio-scsi-pci";
	// set to "virtio-scsi-single" only when virtio_scsi_single is opted in via
	// the layered resolver. Both import-path (createParams) and clone-path
	// (UpdateQemuConfig) use this value.
	scsihw string
	// ephemeralDiskGiB is the size of the dedicated ephemeral disk in GiB.
	// Zero means no dedicated ephemeral disk (default: agent carves from root).
	ephemeralDiskGiB int
	// ephemeralStorage is the PVE storage pool for the ephemeral disk.
	// Only meaningful when ephemeralDiskGiB > 0.
	ephemeralStorage string
	// cpuType is the resolved emulated CPU type/model to write on the new VM
	// (e.g. "host"). Resolved once via resolveVMShapeCPUType:
	// cloud_properties.cpu_type (call/disk_type/vm_type layered resolver) >
	// pve.cpu_type global value (ApplyDefaults fills "host") > "" (no cpu key
	// written at all — only reachable via the "pve-default" sentinel). Both
	// import-path (createParams) and clone-path (UpdateQemuConfig) use this
	// value; cloud_properties.pve_config.cpu is a separate, later write that
	// always wins as the final override.
	cpuType string
	// balloonMiB is the resolved PVE "balloon" value for the new VM. Resolved
	// once via resolveVMShapeBalloon: cloud_properties.balloon (call/disk_type/
	// vm_type layered resolver) > pve.balloon global value (default "0" —
	// ballooning disabled). Nil is the "pve-default" sentinel — PVE keeps its
	// own default (device enabled, balloon = memory): the import path writes
	// no balloon key, and the clone path actively DELETEs the key the clone
	// inherited from the template (which carries balloon=0). Validated
	// ≤ memMiB in buildVMShapeForNode (fail-fast, before any PVE API call).
	balloonMiB *int
	// vmPool is the resolved PVE resource pool name assigned to this VM at
	// create/clone time. Resolved once in buildVMShapeForNode via
	// resolvePoolName, applying the full precedence pipeline (plan §0/D-04):
	//
	//  1. call-level cloud_properties.pool (highest)
	//  2. vm_type profile cloud_properties.pool
	//  3. pve.vm_pool_template rendered with {prefix}/{director}/{deployment}/
	//     {instance_group}
	//  4. pve.vm_pool (global default)
	//
	// Empty (every layer empty) means no pool assignment — no "pool" key or
	// field is ever set, byte-identical to every release before this
	// property existed. The resolved pool is create-if-missing (see
	// ensureResolvedPool) before either create path consumes it.
	vmPool string
	// vmPoolComment is the provenance comment recorded on vmPool when it is
	// created by ensureResolvedPool (pve.PoolProvenance, optionally suffixed
	// with the director name extracted from env.bosh.group). Empty whenever
	// vmPool is empty — no pool is created, so no comment is needed.
	vmPoolComment string
	// vmPoolLayer is the pipeline layer that produced vmPool (a
	// pve.PoolLayer* constant), persisted in the bosh_pool description
	// sentinel so set_vm_metadata's reconciler knows whether the pool was a
	// template render (movable) or an explicit choice (never moved). Empty
	// whenever vmPool is empty.
	vmPoolLayer string
	// vmPoolDirector, vmPoolDeployment, and vmPoolInstanceGrp are the
	// env-derived template tokens (poolTemplateTokensFromEnv) persisted
	// alongside vmPoolLayer, making the reconciler's re-render a pure
	// function of stored create-time inputs. All empty whenever vmPool is
	// empty; individually empty when underivable (e.g. director on a
	// create-env path).
	vmPoolDirector    string
	vmPoolDeployment  string
	vmPoolInstanceGrp string
}

// HandleCreateVM returns a cpi.Handler that implements the BOSH CPI create_vm method.
//
// Arguments (positional, all required):
//
//	[0] agent_id      string
//	[1] stemcell_cid  string — CID returned by create_stemcell. Path-identity grammar only:
//	                    ":light:<storage>:import/<file>" — operator-managed qcow2 (the CPI never deletes it).
//	                    ":heavy:<storage>:import/<file>" — CPI-uploaded qcow2.
//	                    The qcow2's content sha8 (embedded in <file>) is looked up against the per-cluster
//	                    stemcell-cache templates create_stemcell maintains; strategy=template (default) clones
//	                    a cache hit and falls back to strategy=import on a cache miss or unextractable sha8.
//	                    See resolveStemcellStrategy / attemptStemcellTemplateClone / attemptStemcellImport.
//	[2] cloud_props   map     (cores, sockets, memory, vm_disk_format, target_node, stemcell_strategy, ...)
//	[3] networks      map[name]NetworkSpec
//	[4] disk_cids     []string  (persistent disks to pre-attach)
//	[5] env           map[string]any
//
// Returns v2 2-tuple: [vm_cid_string, networks_map_with_mac_addresses].
//
// Rollback: if any step after VM creation fails, the VM is stopped (best-effort)
// and destroyed (purge=true) before the error is returned.
func HandleCreateVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		return createVM(ctx, deps, args)
	})
}

// createVM is the implementation body — separated for testability.
func createVM(
	ctx context.Context,
	deps Deps,
	args []json.RawMessage,
) (result any, retErr error) {
	logger := deps.Log(ctx)

	// -----------------------------------------------------------------------
	// 1. Parse + validate arguments
	// -----------------------------------------------------------------------
	parsed, err := parseCreateVMArgs(args)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 1b. Early VIP validation — fail-fast before any VM mutation.
	// Malformed allowed_address_pairs entries (bad IPs, non-string types) are
	// operator errors that must be surfaced before the VM is created, consistent
	// with static-IP-in-range validation. Any PVE-API failure is deferred to step 8c
	// where it is handled best-effort (fail-open).
	// -----------------------------------------------------------------------
	if err := validateVIPAllowedAddressPairs(parsed.networks); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 2–3. Resolve node and VM-shape parameters.
	// -----------------------------------------------------------------------

	// Post-selection fallback (opt-in: placement.fallback_max > 0).
	// When disabled (default 0), the single-shot path below runs byte-identical
	// to pre-feature behavior — no extra cluster calls, no behavior change.
	// When enabled, a separate path resolves the ranked candidate list and loops
	// over alternates on transient clone or start failure.
	fallbackMax := 0
	if deps.Config != nil {
		fallbackMax = deps.Config.PlacementFallbackMaxValue()
	}

	if fallbackMax > 0 {
		return createVMWithFallback(ctx, deps, logger, parsed, fallbackMax, &retErr)
	}

	// -----------------------------------------------------------------------
	// Single-shot path (fallback disabled, default) — byte-identical to
	// pre-feature behavior.
	// -----------------------------------------------------------------------
	shape, err := resolveVMShape(ctx, deps, parsed)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 3b. Per-node in-flight gate (opt-in; limit=0 → unlimited, no gating).
	// The release is registered twice (see the second defer below the
	// rollback arm); acquire's release is sync.Once-guarded so the double
	// registration is safe.
	// -----------------------------------------------------------------------
	var inflightRelease func()
	if deps.Config != nil {
		var inflightErr error
		inflightRelease, inflightErr = deps.Inflight.acquire(ctx, shape.node, deps.Config.MaxInflightPerNodeLimit())
		if inflightErr != nil {
			return nil, cpierrors.Retriable("create_vm: in-flight limit exceeded or context cancelled on node %s: %s", shape.node, inflightErr.Error())
		}
		defer inflightRelease()
	}

	// -----------------------------------------------------------------------
	// 4. Allocate VMID + create VM via retry-on-conflict.
	//    PVE may reject POST /qemu with HTTP 500 "VM N already exists" when a
	//    concurrent CPI process wins the same VMID. AllocateWithRetry picks a
	//    fresh VMID after backoff and re-attempts. The retry callback also
	//    rolls back per-attempt failures whose UPID-await surfaces a
	//    non-conflict error (PVE accepted the POST but the task itself failed).
	// -----------------------------------------------------------------------
	vmid, err := allocateVM(ctx, deps, logger, parsed, shape)
	if err != nil {
		return nil, cpierrors.Wrap(err, "create_vm: allocate+create VM")
	}

	vmName := shape.initialName
	if vmName == "" {
		vmName = fmt.Sprintf("vm-%d", vmid)
	}

	// Arm rollback for stages 4b–8: any failure after this point destroys the
	// winning VM. See rollbackOnExit for the error-path and panic-path handling.
	vmCreated := true
	defer rollbackOnExit(ctx, deps, shape.node, vmid, parsed.env, logger, &vmCreated, &retErr)
	// Registered AFTER the rollback arm so it runs FIRST (defer is LIFO): the
	// per-node in-flight slot must be released before the rollback runs, not
	// after — a slow rollback otherwise pins the slot for its whole duration
	// and starves concurrent create_vm/attach_disk/delete_vm on the node. The
	// earlier registration above still covers returns before this point;
	// sync.Once inside the release makes the pair safe.
	if inflightRelease != nil {
		defer inflightRelease()
	}

	// Register a middleware-level rollback so that post-hooks (After callbacks
	// in WrapHandler) can trigger cleanup when they flip a nil handler error
	// to non-nil. fireRollback is idempotent and only fires when handlerErr==nil,
	// so this never double-fires with the defer above (which guards on retErr!=nil).
	// Honors keep_failed_vms: a post-hook failure preserves+tags the VM instead
	// of destroying it, matching the handler-failure path.
	cpi.RegisterRollback(ctx, func(c context.Context) {
		disposeFailedVM(c, deps, shape.node, vmid, parsed.env, logger)
	})

	logger.Info("create_vm: vm created and disk imported",
		log.Int(metadataKeyVMID, vmid),
		log.String("stemcell_cid", parsed.stemcellCID),
		log.String("storage", shape.vmStorage),
		log.Int("root_disk_gib", shape.rootDiskGiB),
	)

	// Record the pool-resolution provenance now that the VM exists (the
	// clone path's post-clone description clear already ran inside the
	// allocate attempt). Best-effort — see persistPoolMembership.
	persistPoolMembership(ctx, deps, logger, shape, vmid)

	// -----------------------------------------------------------------------
	// 4b. Grow virtio0 to the requested root disk size.
	//
	// PVE silently ignores the `size=<N>G` directive on the import-from
	// scsi/virtio param when the source image is smaller than N — the new
	// volume keeps the source image's size (~5 GiB for BOSH stemcells).
	// Without an explicit resize, the BOSH agent's bootstrap fails at
	// "Setting up ephemeral disk: Insufficient remaining disk space"
	// (CreatePartitionIfNoEphemeralDisk=true in the stemcell's agent.json
	// requires free space at the end of the root disk).
	// -----------------------------------------------------------------------
	if err := resizeRootDisk(ctx, deps, logger, shape, vmid); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 5. Configure NICs from networks map
	// -----------------------------------------------------------------------
	nicPlan, err := configureNICs(ctx, deps, logger, parsed, shape, vmid)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 5b–5c. IP-conflict pre-flight (static scan + optional agent probe).
	// -----------------------------------------------------------------------
	if err := runIPConflictChecks(ctx, deps, logger, parsed, vmid); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 6. Attach persistent disks (disk_cids pre-attach)
	// -----------------------------------------------------------------------
	if err := attachPersistentDisks(ctx, deps, logger, parsed, shape, vmid); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 6.5. Create and attach dedicated ephemeral disk (opt-in).
	//      When ephemeral_disk_size_mb is unset (zero), this is a no-op —
	//      byte-identical to pre-feature behavior (agent carves ephemeral
	//      from root disk via CreatePartitionIfNoEphemeralDisk).
	// -----------------------------------------------------------------------
	ephemeralDevPath, err := attachEphemeralDisk(ctx, deps, logger, shape, vmid)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 7. Build AgentConfig and call agent.Configure
	// -----------------------------------------------------------------------
	if err := configureAgent(ctx, deps, logger, parsed, shape, vmid, vmName, ephemeralDevPath); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 8. Start VM + read back VM config to extract assigned MAC addresses
	// -----------------------------------------------------------------------
	responseNetworks, err := startVMAndReadConfig(ctx, deps, logger, parsed, shape, vmid, nicPlan)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 8b. Firewall: resolve and apply security groups and VM-level firewall flag.
	//
	// Precedence for security_groups: per-call > vm_type/disk_type profile >
	// config global default. Precedence for firewall flag: per-call cloud_properties
	// > profile > config.VMFirewall.
	//
	// Behavior:
	//   - effective groups non-empty  => applySecurityGroups (attaches rules +
	//     enables firewall); the flag is NOT checked here to prevent double-enable.
	//   - no groups but firewall flag true => enableVMFirewall (standalone enable).
	//   - no groups and flag false/unset => no firewall API calls (byte-identical
	//     to prior behavior when VMFirewall is nil and no groups are set).
	//
	// Any error triggers the create_vm rollback (the VM is destroyed) so a
	// half-firewalled VM is never left behind.
	// -----------------------------------------------------------------------
	effectiveGroups, groupErr := resolveEffectiveSecurityGroups(
		parsed.cloudPropsMap, deps.Config, parsed.cloudProps.SecurityGroups)
	if groupErr != nil {
		return nil, groupErr
	}
	firewallEnabled, fwFlagErr := resolveEffectiveFirewall(parsed.cloudPropsMap, deps.Config)
	if fwFlagErr != nil {
		return nil, fwFlagErr
	}
	// One-time datacenter firewall master-switch probe (§1.4): whenever this VM
	// requests any firewall-affecting feature, verify — once per process — that
	// the cluster-wide master switch is actually enabled, since every API call
	// below succeeds regardless of it and a disabled master switch means none of
	// the rules programmed here filter any traffic. Best-effort and fail-open;
	// never blocks or fails create_vm. See probeFirewallMasterSwitch.
	if firewallFeatureInPlay(effectiveGroups, firewallEnabled, parsed.networks) {
		probeFirewallMasterSwitch(ctx, deps, logger)
	}
	if len(effectiveGroups) > 0 {
		if fwErr := applySecurityGroups(ctx, deps, shape.node, vmid, effectiveGroups, logger); fwErr != nil {
			return nil, fwErr
		}
	} else if firewallEnabled {
		if fwErr := enableVMFirewall(ctx, deps, shape.node, vmid, logger); fwErr != nil {
			return nil, fwErr
		}
	}

	// -----------------------------------------------------------------------
	// 8c. VIP allowed-address-pairs: seed per-NIC ipfilter ipsets and enable the
	// VM-level ipfilter option when any NIC declares allowed_address_pairs.
	//
	// Best-effort and non-fatal: PVE API failures are logged as warnings and
	// leave ipfilter OFF rather than risk locking out the VM. The safety guard in
	// applyVIPAllowedAddressPairs ensures ipfilter is never enabled unless every
	// firewalled NIC ipset is fully seeded. Unset manifest = byte-identical (no
	// PVE calls). Entry format errors were rejected at step 1b before VM mutation.
	//
	// NICs with ip_forwarding=true are excluded from ipset seeding: a router VM
	// forwards traffic not in its own ipset, so ipfilter on those NICs would
	// silently drop forwarded packets. applyVIPAllowedAddressPairs receives the
	// full network map; per-NIC exclusion is enforced inside that function.
	// -----------------------------------------------------------------------
	if vipErr := applyVIPAllowedAddressPairs(ctx, deps, shape.node, vmid, parsed.networks, logger); vipErr != nil {
		logger.Warn("create_vm: VIP ipfilter not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.Err(vipErr))
	}

	// -----------------------------------------------------------------------
	// 8d. ip_forwarding per-NIC: for any NIC with cloud_properties.ip_forwarding=true,
	// set firewall=0 on that NIC via UpdateQemuConfig. Router/NAT VMs need this so
	// forwarded packets (not addressed to the NIC's own IP) are not dropped by the
	// PVE per-NIC firewall. Nil/unset → no API calls (byte-identical).
	// Errors propagate and trigger rollback so a half-configured router NIC is
	// never left behind.
	// -----------------------------------------------------------------------
	if fwdErr := applyIPForwarding(ctx, deps, shape.node, vmid, parsed.networks, logger); fwdErr != nil {
		return nil, fwdErr
	}

	// -----------------------------------------------------------------------
	// 8e. Advertised routes: for each entry in cloud_properties.advertised_routes,
	// create an SDN vnet subnet then call applySDN to commit the OVN logical-router
	// route. Empty list → no API calls (byte-identical). Errors propagate and
	// trigger rollback; subnets injected before a failure are removed best-effort.
	// -----------------------------------------------------------------------
	if routeErr := applyAdvertisedRoutes(ctx, deps, shape.node, vmid, parsed.cloudProps.AdvertisedRoutes, logger); routeErr != nil {
		return nil, routeErr
	}

	// -----------------------------------------------------------------------
	// 9. PVE HA anti-affinity membership (opt-in: anti_affinity.use_ha_rules).
	//
	// Errors propagate: a TypeRetriableCloud error (e.g. lock-timeout) is
	// returned so the director re-drives. A non-retriable failure (e.g. HA
	// not configured) is also returned — callers must treat it accordingly.
	// Scheduler-soft spreading via the node scorer remains in effect
	// regardless.
	// -----------------------------------------------------------------------
	if aaErr := applyAntiAffinityMembership(ctx, deps, vmid, parsed.env, logger); aaErr != nil {
		return nil, aaErr
	}

	// -----------------------------------------------------------------------
	// 9b. AZ node-affinity HA pin (opt-in: placement.pin_az_via_ha_rules).
	//
	// After scoring placed the VM on a node within its AZ, write a PVE HA
	// node-affinity rule binding it to the AZ node set, so the AZ placement is
	// durable across HA failover and DLB rebalance (scoring alone only pins at
	// birth). TypeRetriableCloud (lock-timeout, verify failure) is returned so
	// the director re-drives rather than silently losing the pin guarantee.
	// Non-retriable failures also propagate.
	// -----------------------------------------------------------------------
	if naErr := applyAZNodeAffinityPin(ctx, deps, vmid, parsed.cloudProps, shape.node, logger); naErr != nil {
		return nil, naErr
	}

	// -----------------------------------------------------------------------
	// 9c. PCI strict node-affinity pin (automatic when pci_passthroughs set).
	//
	// PCI passthrough is incompatible with live migration. When the VM has any
	// passthrough devices, a strict single-node HA pin is applied so the HA
	// manager cannot relocate the VM to a node that may lack the device.
	// Empty pci_passthroughs → no-op (byte-identical). TypeRetriableCloud
	// errors propagate; generic HA-API failures are logged as non-fatal.
	// -----------------------------------------------------------------------
	if pciPinErr := applyPCINodeAffinityPin(ctx, deps, vmid, shape.node, parsed.cloudProps.PCIPassthroughs, logger); pciPinErr != nil {
		return nil, pciPinErr
	}

	// -----------------------------------------------------------------------
	// 10. PVE Dynamic Load Balancer membership (opt-in: placement.dlb).
	//
	// Best-effort and non-fatal. When the VM is DLB-eligible (master flag or
	// sentinel AZ) it is registered as a PVE HA resource with auto-rebalance so
	// the 9.2 CRS dynamic load balancer places and rebalances it. All guards
	// (PVE>=9.2, multi-node, shared storage) live in ensureDLBMembership; any
	// failure is logged and never fails create_vm.
	// -----------------------------------------------------------------------
	if deps.Config.DLBEligibleForAZ(parsed.cloudProps.AvailabilityZone) {
		if dlbErr := ensureDLBMembership(ctx, deps, vmid, parsed.cloudProps.AvailabilityZone, logger); dlbErr != nil {
			logger.Warn("create_vm: DLB membership not fully applied (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.Err(dlbErr))
		}
	}

	// -----------------------------------------------------------------------
	// 10b. Config-drive ISO migration-safety check. Runs whenever this VM was
	// (or was eligible to be) HA-registered by any path above — DLB, AZ
	// node-affinity pin, or anti-affinity HA rules: a non-shared iso_storage
	// pool means the scsi30 CD-ROM cannot follow the VM across live migration
	// or HA recovery, silently defeating all three features. Warns by default;
	// escalates to a CloudError (failing create_vm, triggering rollback) when
	// require_shared_iso_for_ha is true.
	// -----------------------------------------------------------------------
	if isoErr := checkISOStorageForHA(ctx, deps, vmid, parsed.cloudProps, shape.node, parsed.env, logger); isoErr != nil {
		return nil, isoErr
	}

	// -----------------------------------------------------------------------
	// 11. Post-create health gate (opt-in: health_check.enabled).
	//
	// When enabled, poll the QEMU guest agent until it answers or the deadline
	// expires. On failure, diagnostics from ListQemuStatusCurrent are folded
	// into the error before the standard rollback defer fires and destroys the
	// VM. Default off — behavior is byte-identical to prior releases when the
	// block is absent or Enabled is false.
	// -----------------------------------------------------------------------
	if deps.Config.HealthCheckEnabled() {
		if hcErr := runHealthGate(ctx, deps, shape.node, vmid, logger); hcErr != nil {
			return nil, hcErr
		}
	}

	vmCID := strconv.Itoa(vmid)
	return []any{vmCID, responseNetworks}, nil
}

// createVMWithFallback runs the post-selection fallback loop for create_vm.
// Called only when PlacementFallbackMaxValue() > 0.
//
// Invariants:
//   - Intermediate (non-final) failed attempts are fully purged via cleanupVM
//     before the next candidate is tried. No orphan VMs are left.
//   - keep_failed_vms tagging and rollbackOnExit apply only to the FINAL outcome
//     (success or terminal-fail). Intermediate failures are silently cleaned up.
//   - Fallback only activates on transient clone errors (IsTransientTransport,
//     IsStorageLockTimeout) or transient start errors (IsTransientTransport).
//     Permanent errors surface immediately without consuming alternates.
//     (Clone-source-missing never surfaces here at all: the template path
//     falls back to strategy=import inside attemptStemcellTemplateClone.)
//   - The in-flight semaphore from step 3b is NOT acquired here to keep the
//     fallback loop simple; the semaphore protects per-node concurrency and is
//     best applied to the final committed node rather than each attempt node.
//
// ptrToRefParam: retErr is a pointer to createVM's named return value so
// rollbackOnExit and RegisterRollback can observe the final error at defer-time.
// This is the same pattern as createVM itself; gocritic's suggestion (*error →
// non-pointer) would break the caller's named-return observation.
//
//nolint:gocognit,gocritic // Fallback loop + rollback + final-side-effects; inherent complexity.
func createVMWithFallback(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	fallbackMax int,
	retErr *error, //nolint:gocritic // ptrToRefParam: same named-return pointer pattern as createVM
) (any, error) {
	shape, alternates, err := resolveVMShapeWithAlternates(ctx, deps, parsed, fallbackMax)
	if err != nil {
		return nil, err
	}

	// Build the ordered list of candidates: [winner, alternates...].
	candidates := make([]string, 0, 1+len(alternates))
	candidates = append(candidates, shape.node)
	candidates = append(candidates, alternates...)

	var lastErr error
	for attemptIdx, candidateNode := range candidates {
		isLast := attemptIdx == len(candidates)-1

		vmid, responseNetworks, allocErr, startErr, otherErr := buildAndStartVMAttempt(
			ctx, deps, logger, parsed, shape, candidateNode)

		// Determine whether to fall back or commit.
		var attemptErr error
		var shouldFallback bool
		switch {
		case allocErr != nil:
			attemptErr = allocErr
			shouldFallback = isTransientAllocateError(allocErr)
		case otherErr != nil:
			// Resize, NICs, disks, ephemeral, agent — non-fallback errors.
			attemptErr = otherErr
			shouldFallback = false
		case startErr != nil:
			attemptErr = startErr
			shouldFallback = isTransientStartError(startErr)
		}

		if attemptErr != nil {
			// Clean up this attempt's partial VM (if any was created).
			if vmid != 0 {
				cleanupVMDetached(ctx, deps, candidateNode, vmid, parsed.env, logger)
			}

			if !shouldFallback || isLast {
				// Permanent error OR exhausted candidates — propagate last error.
				lastErr = attemptErr
				break
			}

			// Transient and alternates remain — log and advance.
			logger.Warn("create_vm: fallback: transient error on candidate, trying next",
				log.String("node", candidateNode),
				log.Int("attempt", attemptIdx+1),
				log.Int("remaining", len(candidates)-attemptIdx-1),
				log.Err(attemptErr),
			)
			lastErr = attemptErr
			continue
		}

		// Success on this candidate. Arm rollback for post-start steps 8b-11.
		// shape.node is set to the winning candidateNode for all subsequent calls.
		winningNode := candidateNode
		winningVMID := vmid
		vmCreated := true
		defer rollbackOnExit(ctx, deps, winningNode, winningVMID, parsed.env, logger, &vmCreated, retErr) //nolint:gocritic // deferInLoop: intentional — defer fires at function return, not loop-end; only one candidate succeeds
		cpi.RegisterRollback(ctx, func(c context.Context) {
			disposeFailedVM(c, deps, winningNode, winningVMID, parsed.env, logger)
		})

		logger.Info("create_vm: vm created and disk imported",
			log.Int(metadataKeyVMID, winningVMID),
			log.String("stemcell_cid", parsed.stemcellCID),
			log.String("storage", shape.vmStorage),
			log.Int("root_disk_gib", shape.rootDiskGiB),
			log.String("node", winningNode),
			log.Int("attempt", attemptIdx+1),
		)

		// Build a node-specific shape copy for post-start steps that read shape.node.
		winShape := *shape
		winShape.node = winningNode

		// -----------------------------------------------------------------------
		// 8b. Firewall
		// -----------------------------------------------------------------------
		effectiveGroups, groupErr := resolveEffectiveSecurityGroups(
			parsed.cloudPropsMap, deps.Config, parsed.cloudProps.SecurityGroups)
		if groupErr != nil {
			return nil, groupErr
		}
		firewallEnabled, fwFlagErr := resolveEffectiveFirewall(parsed.cloudPropsMap, deps.Config)
		if fwFlagErr != nil {
			return nil, fwFlagErr
		}
		// One-time datacenter firewall master-switch probe (§1.4) — see the
		// step-8b comment in createVM for the full rationale.
		if firewallFeatureInPlay(effectiveGroups, firewallEnabled, parsed.networks) {
			probeFirewallMasterSwitch(ctx, deps, logger)
		}
		if len(effectiveGroups) > 0 {
			if fwErr := applySecurityGroups(ctx, deps, winShape.node, winningVMID, effectiveGroups, logger); fwErr != nil {
				return nil, fwErr
			}
		} else if firewallEnabled {
			if fwErr := enableVMFirewall(ctx, deps, winShape.node, winningVMID, logger); fwErr != nil {
				return nil, fwErr
			}
		}

		// -----------------------------------------------------------------------
		// 8c. VIP ipfilter
		// -----------------------------------------------------------------------
		if vipErr := applyVIPAllowedAddressPairs(ctx, deps, winShape.node, winningVMID, parsed.networks, logger); vipErr != nil {
			logger.Warn("create_vm: VIP ipfilter not fully applied (non-fatal)",
				log.Int(metadataKeyVMID, winningVMID), log.Err(vipErr))
		}

		// -----------------------------------------------------------------------
		// 8d. ip_forwarding per-NIC: disable per-NIC firewall for router/NAT NICs.
		// -----------------------------------------------------------------------
		if fwdErr := applyIPForwarding(ctx, deps, winShape.node, winningVMID, parsed.networks, logger); fwdErr != nil {
			return nil, fwdErr
		}

		// -----------------------------------------------------------------------
		// 8e. Advertised routes: inject OVN SDN subnets for router VMs.
		// -----------------------------------------------------------------------
		if routeErr := applyAdvertisedRoutes(ctx, deps, winShape.node, winningVMID, parsed.cloudProps.AdvertisedRoutes, logger); routeErr != nil {
			return nil, routeErr
		}

		// -----------------------------------------------------------------------
		// 9. HA anti-affinity — errors propagate (retriable ones re-drive).
		// -----------------------------------------------------------------------
		if aaErr := applyAntiAffinityMembership(ctx, deps, winningVMID, parsed.env, logger); aaErr != nil {
			return nil, aaErr
		}

		// -----------------------------------------------------------------------
		// 9b. AZ node-affinity HA pin — errors propagate (retriable ones re-drive).
		// -----------------------------------------------------------------------
		if naErr := applyAZNodeAffinityPin(ctx, deps, winningVMID, parsed.cloudProps, winShape.node, logger); naErr != nil {
			return nil, naErr
		}

		// -----------------------------------------------------------------------
		// 9c. PCI strict node-affinity pin — errors propagate (retriable ones
		// re-drive). No-op when pci_passthroughs is empty.
		// -----------------------------------------------------------------------
		if pciPinErr := applyPCINodeAffinityPin(ctx, deps, winningVMID, winShape.node, parsed.cloudProps.PCIPassthroughs, logger); pciPinErr != nil {
			return nil, pciPinErr
		}

		// -----------------------------------------------------------------------
		// 10. DLB membership
		// -----------------------------------------------------------------------
		if deps.Config.DLBEligibleForAZ(parsed.cloudProps.AvailabilityZone) {
			if dlbErr := ensureDLBMembership(ctx, deps, winningVMID, parsed.cloudProps.AvailabilityZone, logger); dlbErr != nil {
				logger.Warn("create_vm: DLB membership not fully applied (non-fatal)",
					log.Int(metadataKeyVMID, winningVMID), log.Err(dlbErr))
			}
		}

		// -----------------------------------------------------------------------
		// 10b. Config-drive ISO migration-safety check — see the step-10b comment
		// in createVM for the full rationale.
		// -----------------------------------------------------------------------
		if isoErr := checkISOStorageForHA(ctx, deps, winningVMID, parsed.cloudProps, winShape.node, parsed.env, logger); isoErr != nil {
			return nil, isoErr
		}

		// -----------------------------------------------------------------------
		// 11. Health gate
		// -----------------------------------------------------------------------
		if deps.Config.HealthCheckEnabled() {
			if hcErr := runHealthGate(ctx, deps, winShape.node, winningVMID, logger); hcErr != nil {
				return nil, hcErr
			}
		}

		vmCID := strconv.Itoa(winningVMID)
		return []any{vmCID, responseNetworks}, nil
	}

	// All candidates exhausted (or single permanent-fail).
	return nil, lastErr
}

// parseCreateVMArgs validates and unmarshals the 6-element create_vm JSON args array.
// Returns cpierrors.CloudError on any validation failure.
func parseCreateVMArgs(args []json.RawMessage) (*createVMParsedArgs, error) {
	if len(args) < 6 {
		return nil, cpierrors.Cloud("create_vm: expected 6 arguments, got %d", len(args))
	}

	var agentID string
	if err := json.Unmarshal(args[0], &agentID); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse agent_id: %s", err.Error())
	}
	if agentID == "" {
		return nil, cpierrors.Cloud("create_vm: agent_id must not be empty")
	}

	var stemcellCID string
	if err := json.Unmarshal(args[1], &stemcellCID); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse stemcell_cid: %s", err.Error())
	}
	// Path-identity grammar only: ":light:<storage>:import/<file>" or
	// ":heavy:<storage>:import/<file>". Every retired form (legacy
	// "template:<vmid>", old "light:..."/bare "<storage>:import/<file>",
	// integer CIDs) is rejected here — ParseStemcellPathCID's error text
	// explains the expected grammar.
	stemcellKind, stemcellStorage, stemcellVolPath, err := pve.ParseStemcellPathCID(stemcellCID)
	if err != nil {
		return nil, cpierrors.Cloud("create_vm: invalid stemcell_cid: %s", err.Error())
	}
	// ParseStemcellPathCID guarantees stemcellVolPath starts with "import/".
	stemcellFilename := strings.TrimPrefix(stemcellVolPath, "import/")
	// rawVolid is the bare "<storage>:import/<file>" volid PVE's import-from=
	// parameter consumes.
	rawVolid := stemcellStorage + ":" + stemcellVolPath

	var cloudProps createVMCloudProps
	if err := json.Unmarshal(args[2], &cloudProps); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse cloud_properties: %s", err.Error())
	}
	// Validate pve_config before any VM is created. A bad key or value is a
	// manifest error that must surface pre-clone so no orphan VM is produced.
	if err := validatePVEConfig(cloudProps.PVEConfig); err != nil {
		return nil, err
	}
	// Validate pci_passthroughs address format pre-clone so a malformed address
	// never reaches the PVE API, consistent with the pve_config pre-validation
	// pattern.
	if err := validatePCIPassthroughs(cloudProps.PCIPassthroughs); err != nil {
		return nil, err
	}
	// Validate advertised_routes CIDR destinations and vnet names pre-clone so
	// malformed entries never reach the SDN API and no orphan VM is produced.
	if err := validateAdvertisedRoutes(cloudProps.AdvertisedRoutes); err != nil {
		return nil, err
	}
	// Validate the per-VM stemcell_strategy override pre-clone. Empty defers
	// to the global config default; any other value must be "template" or
	// "import".
	if err := validateStemcellStrategyCloudProp(cloudProps.StemcellStrategy); err != nil {
		return nil, err
	}

	// Also decode into a raw map so the layered resolver can access keys not
	// modelled in createVMCloudProps (e.g. storage_pool, vm_type, disk_type).
	// null args[2] unmarshals to a nil map; treat nil as empty (no overrides).
	var cloudPropsMap map[string]any
	if err := json.Unmarshal(args[2], &cloudPropsMap); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse cloud_properties (raw): %s", err.Error())
	}
	if cloudPropsMap == nil {
		cloudPropsMap = map[string]any{}
	}

	var networks map[string]createVMNetworkSpec
	if err := json.Unmarshal(args[3], &networks); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse networks: %s", err.Error())
	}
	// Static-IP containment: validate each manual network's IP against its
	// declared range CIDR before any PVE resources are allocated. This is a
	// manifest error that will not resolve on retry, so a non-retriable
	// CloudError is returned immediately.
	if err := validateNetworkContainment(networks); err != nil {
		return nil, err
	}

	var diskCIDs []string
	if err := json.Unmarshal(args[4], &diskCIDs); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse disk_cids: %s", err.Error())
	}

	// Disk slot layout:
	//   virtio0 (default) or scsi0 (pve.root_disk_bus=scsi)
	//                 system disk (stemcell-imported root; see create flow below,
	//                 and rootDiskKey for bus selection).
	//   scsi1..scsi28 persistent disks at create_vm time + dynamic attach_disk.
	//   scsi30        ConfigDrive CD-ROM (see agent.configDriveSlot); scsi29 headroom.
	// scsi0 is always reserved for the root disk (used or not): AttachDisk's
	// free-slot search starts at scsi1 unconditionally, so persistent-disk slot
	// allocation is identical regardless of which bus the root disk is on —
	// there is no slot collision to manage when root_disk_bus=scsi puts the
	// root disk on the slot persistent disks already skip.
	const maxPersistentDisksAtCreate = 28
	if len(diskCIDs) > maxPersistentDisksAtCreate {
		return nil, cpierrors.Cloud(
			"create_vm: too many persistent disks at create time (%d); CPI reserves scsi29 (headroom) and scsi30 (cloud-init drive)",
			len(diskCIDs))
	}

	var env map[string]any
	if err := json.Unmarshal(args[5], &env); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse env: %s", err.Error())
	}

	return &createVMParsedArgs{
		agentID:          agentID,
		stemcellCID:      stemcellCID,
		stemcellKind:     stemcellKind,
		stemcellStorage:  stemcellStorage,
		stemcellVolPath:  stemcellVolPath,
		stemcellFilename: stemcellFilename,
		rawVolid:         rawVolid,
		cloudProps:       cloudProps,
		cloudPropsMap:    cloudPropsMap,
		networks:         networks,
		diskCIDs:         diskCIDs,
		env:              env,
	}, nil
}

// resolveVMIDAllocParams returns the VMID range start and per-create allocation
// retry budget. maxAttempts defaults to 10 so a parallel CF deploy (many
// simultaneous stemcell imports against the same PVE storage) can survive
// transient per-storage lockfile timeouts in addition to VMID races. Each
// lock-timeout retry waits seconds, not ms, so 10 is still bounded.
//
// Attempt-budget precedence (first set wins):
//  1. retry.storage_lock.max_attempts  — dedicated storage-lock budget (primary)
//  2. retry.storage_import.max_attempts — legacy fallback (pre-storage_lock deployments)
//  3. retry.vmid_alloc.max_attempts
//  4. vmid_alloc_attempts
//  5. built-in default 10
//
// The create_vm allocation loop handles both storage-lock and VMID-conflict
// retries in one budget; the lock retries dominate (seconds vs ms).
func resolveVMIDAllocParams(cfg *config.CPIConfig) (rangeStart, maxAttempts int) {
	rangeStart = cfg.VMIDRangeStart
	if rangeStart < 100 {
		rangeStart = pve.VMIDRangeVMStart
	}
	switch {
	case cfg.RetryStorageLock().MaxAttempts > 0:
		maxAttempts = cfg.RetryStorageLock().MaxAttempts
	case cfg.RetryStorageImport().MaxAttempts > 0:
		maxAttempts = cfg.RetryStorageImport().MaxAttempts
	case cfg.RetryVMIDAlloc().MaxAttempts > 0:
		maxAttempts = cfg.RetryVMIDAlloc().MaxAttempts
	case cfg.VMIDAllocAttempts > 0:
		maxAttempts = cfg.VMIDAllocAttempts
	default:
		maxAttempts = 10
	}
	return rangeStart, maxAttempts
}

// isTransientAllocateError reports whether the error returned by allocateVM
// (after its internal per-VMID retry exhaustion) is a transient condition that
// post-selection fallback should treat as "try a different node". Only errors
// whose root cause may change when a different cluster node is chosen are
// returned true; VMID-conflict and similar VMID-only conditions are excluded
// because retrying on a fresh node with the same VMID pool is not guaranteed
// to resolve the conflict.
//
// The IsCloneSourceMissing guard is defense-in-depth: the template path
// normally intercepts a missing clone source and falls back to
// strategy=import, but the matcher is message-based ("unable to find
// configuration file for vm") and PVE can emit that text from other QEMU
// operations, where the condition is permanent — a node fallback would not
// help and would only delay surfacing the real error.
func isTransientAllocateError(err error) bool {
	if err == nil {
		return false
	}
	if pve.IsCloneSourceMissing(err) {
		return false
	}
	return pve.IsTransientTransport(err) || pve.IsStorageLockTimeout(err)
}

// isTransientStartError reports whether the error returned by
// startVMAndReadConfig is a transient condition that warrants a post-selection
// fallback to the next-ranked node.
//
// startVMAndReadConfig wraps start errors as cpierrors.Cloud strings, breaking
// the SDK error chain. To detect transient start failures, this function checks
// both the unwrapped chain (for raw SDK errors that escaped the RetryOnTransient
// wrapper) and the error message string (for errors whose chain was string-wrapped).
// It also checks if the wrapped cpierrors is retriable (WrapAs + TypeRetriableCloud).
func isTransientStartError(err error) bool {
	if err == nil {
		return false
	}
	// Check the SDK error chain first (raw transport error, not string-wrapped).
	if pve.IsTransientTransport(err) {
		return true
	}
	// cpierrors.Cloud wraps as TypeCloud (non-retriable), so OkToRetry() won't
	// help here. Fall back to message-string inspection as a last resort.
	// The string check covers the "create_vm: start vmid=N: <transport error>"
	// message shape produced by startVMAndReadConfig.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "auto-login failed") ||
		strings.Contains(msg, "failed to parse login response") ||
		strings.Contains(msg, "(code: 596)") ||
		strings.Contains(msg, "http 596") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "deadline exceeded")
}

// buildAndStartVMAttempt runs steps 4–8 (allocate, resize, NICs, disks,
// ephemeral, agent, start) on the given candidate node. On success it returns
// the allocated VMID and populated response-networks map.
// On any error it is the CALLER's responsibility to call cleanupVM when the
// VMID is non-zero (the VM was created but a later step failed).
//
// shape.node is overridden with candidateNode for this attempt; the shape
// struct is not mutated — a local copy is used.
//
// Three error outputs distinguish error categories for the fallback loop:
//   - allocErr: returned when allocateVM fails (eligible for fallback on transient).
//   - startErr: returned when startVMAndReadConfig fails (eligible for fallback on transient).
//   - otherErr: returned when any intermediate step (resize, NICs, etc.) fails
//     (permanent, no fallback).
func buildAndStartVMAttempt(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	candidateNode string,
) (vmid int, responseNetworks map[string]createVMNetworkSpec, allocErr error, startErr error, otherErr error) {
	// Use a node-overridden copy so we never mutate the caller's shape.
	nodeShape := *shape
	nodeShape.node = candidateNode

	vmid, err := allocateVMForFallback(ctx, deps, logger, parsed, &nodeShape)
	if err != nil {
		return 0, nil, cpierrors.Wrap(err, "create_vm: allocate+create VM"), nil, nil
	}

	// Record the pool-resolution provenance now that the VM exists (the
	// clone path's post-clone description clear already ran inside the
	// allocate attempt). Best-effort — see persistPoolMembership.
	persistPoolMembership(ctx, deps, logger, &nodeShape, vmid)

	vmName := nodeShape.initialName
	if vmName == "" {
		vmName = fmt.Sprintf("vm-%d", vmid)
	}

	if err := resizeRootDisk(ctx, deps, logger, &nodeShape, vmid); err != nil {
		return vmid, nil, nil, nil, err
	}

	nicPlan, err := configureNICs(ctx, deps, logger, parsed, &nodeShape, vmid)
	if err != nil {
		return vmid, nil, nil, nil, err
	}

	if err := runIPConflictChecks(ctx, deps, logger, parsed, vmid); err != nil {
		return vmid, nil, nil, nil, err
	}

	if err := attachPersistentDisks(ctx, deps, logger, parsed, &nodeShape, vmid); err != nil {
		return vmid, nil, nil, nil, err
	}

	ephemeralDevPath, err := attachEphemeralDisk(ctx, deps, logger, &nodeShape, vmid)
	if err != nil {
		return vmid, nil, nil, nil, err
	}

	if err := configureAgent(ctx, deps, logger, parsed, &nodeShape, vmid, vmName, ephemeralDevPath); err != nil {
		return vmid, nil, nil, nil, err
	}

	responseNetworks, err = startVMAndReadConfig(ctx, deps, logger, parsed, &nodeShape, vmid, nicPlan)
	if err != nil {
		return vmid, nil, nil, err, nil
	}

	return vmid, responseNetworks, nil, nil, nil
}

// allocateVM runs AllocateWithRetry: picks a free VMID, calls QEMU.Create,
// awaits the import task. On conflict or transient errors it retries up to
// shape.maxAttempts times. Returns the winning vmid.
//
// pve.WithStorageScan(shape.node, shape.vmStorage) is always passed: shape.node
// and shape.vmStorage are resolved (in buildVMShapeForNode / resolveTargetNode)
// from the EFFECTIVE per-request CPI config — i.e. already reflect any
// per-request pve_node/pve_vm_storage context override — so the storage scan
// targets the cluster this specific request is landing the VM on, which
// matters when that cluster shares its VM/images storage with another BOSH
// AZ's cluster (see pve.WithStorageScan's doc comment for the co-mingling
// risk this closes).
func allocateVM(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
) (int, error) {
	isRetryable := func(e error) bool {
		// Defense-in-depth: attemptStemcellTemplateClone intercepts a missing
		// clone source and falls back to import, so this error normally never
		// propagates here. But the matcher is message-based and the text can
		// surface from other QEMU operations as a 5xx that IsTransientTransport
		// would match; retrying with a fresh VMID cannot help. Short-circuit so
		// the real cause propagates instead of "exhausted VMID allocation".
		if pve.IsCloneSourceMissing(e) {
			return false
		}
		// Same short-circuit for PVE's cross-node clone rejection: it can
		// surface with an SDK classification IsTransientTransport matches,
		// but it is a permanent configuration condition.
		if pve.IsCloneToNonSharedStorage(e) {
			return false
		}
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	vmid, err := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateVM(ctx, deps, logger, parsed, shape, candidate)
		},
		isRetryable,
		shape.maxAttempts,
		pve.WithRange(shape.rangeStart, deps.Config.VMIDRangeEnd),
		pve.WithStorageScan(shape.node, shape.vmStorage),
		pve.WithExtraStorageScan(shape.node, isoStorageScanTarget(deps, shape.vmStorage)),
		pve.WithBackoffFunc(newCreateVMRetryBackoff(
			deps.Config.RetryStorageImport(), deps.Config.RetryVMIDAlloc())),
	)
	return vmid, err
}

// allocateVMForFallback is a variant of allocateVM used by the post-selection
// fallback path. It differs from allocateVM in one critical way: transient
// transport errors are NOT retried internally. Instead they propagate immediately
// so the fallback loop can decide whether to try the next candidate node.
//
// VMID conflicts and storage-lock timeouts are still retried internally (they
// are VMID-specific, not node-specific, and retrying with a fresh VMID on the
// same node is still correct). The clone-source-missing guard is the same
// defense-in-depth as allocateVM's: normally intercepted upstream by the
// import fallback, kept here for message-matcher spillover from other ops.
func allocateVMForFallback(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
) (int, error) {
	isRetryable := func(e error) bool {
		if pve.IsCloneSourceMissing(e) {
			return false
		}
		// Transient transport: NOT retried here — let the fallback loop handle it.
		if pve.IsTransientTransport(e) {
			return false
		}
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e)
	}

	vmid, err := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateVM(ctx, deps, logger, parsed, shape, candidate)
		},
		isRetryable,
		shape.maxAttempts,
		pve.WithRange(shape.rangeStart, deps.Config.VMIDRangeEnd),
		pve.WithStorageScan(shape.node, shape.vmStorage),
		pve.WithExtraStorageScan(shape.node, isoStorageScanTarget(deps, shape.vmStorage)),
		pve.WithBackoffFunc(newCreateVMRetryBackoff(
			deps.Config.RetryStorageImport(), deps.Config.RetryVMIDAlloc())),
	)
	return vmid, err
}

// attemptCreateVM builds the create params for one VMID candidate, then
// either clones from the stemcell's per-cluster cache template
// (strategy=template, the default) or calls QEMU.Create with import-from=
// (strategy=import, or the strategy=template fallback on a cache miss or
// unextractable sha8). On retryable failures it logs and optionally cleans
// up the candidate VMID before returning the error so AllocateWithRetry can
// retry with a fresh candidate.
func attemptCreateVM(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	candidate int,
) error {
	candidateName := shape.initialName
	if candidateName == "" {
		candidateName = fmt.Sprintf("vm-%d", candidate)
	}

	// PCI guard: every node-resolution outcome funnels through here, including
	// the static paths that never run the placement filter (operator
	// target_node, local-disk pin, config.node fallback, placement disabled).
	// Verify the chosen node before any clone/import so a missing device
	// produces a clear pre-mutation error instead of a VM that cannot start.
	// No-op when pci_passthroughs is absent.
	if pciErr := verifyPCIOnNode(ctx, deps, shape.node, parsed.cloudProps.PCIPassthroughs, logger); pciErr != nil {
		return pciErr
	}

	// --- Stemcell strategy dispatch ---
	//
	// strategy=template (default): clone the per-cluster stemcell-cache
	// template found by content sha8. A cache miss or an unextractable sha8
	// falls back to strategy=import — create_vm NEVER builds a template
	// itself (that stays create_stemcell's job; the cache is read-only from
	// here), so there is nothing new to roll back on a fallback.
	// strategy=import: import-from= the stemcell qcow2 directly.
	strategy := resolveStemcellStrategy(deps.Config, parsed)
	if strategy == config.StemcellStrategyTemplate {
		handled, cloneErr := attemptStemcellTemplateClone(ctx, deps, logger, parsed, shape, candidate, candidateName)
		if handled {
			return cloneErr
		}
	}
	return attemptStemcellImport(ctx, deps, logger, parsed, shape, candidate, candidateName)
}

// attemptStemcellTemplateClone attempts the strategy=template dispatch: find
// a cluster-scoped stemcell-cache template matching the stemcell's content
// sha8 and clone it.
//
// Returns handled=false (nil error) when the caller must fall back to
// strategy=import — the sha8 could not be extracted from the stemcell's
// filename, no cache template exists anywhere in the cluster
// (create_stemcell builds the cache; it may have been manually deleted), or
// the template vanished between the cache lookup and the clone attempt
// (clone-source-missing) — the qcow2 the CID names may still exist, so the
// import path gets its chance before the failure is declared permanent.
//
// Returns handled=true with a non-nil error for a real, actionable failure
// (a local-storage replica gap with replication disabled, or the clone
// attempt itself failing for any reason other than a missing source) that
// must propagate without an import fallback.
//
// Returns handled=true with a nil error when the clone — and its post-clone
// config (pve_config passthrough + PCI hostpciN) — succeeded.
func attemptStemcellTemplateClone(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	candidate int,
	candidateName string,
) (handled bool, err error) {
	sha8, hasSHA := extractSHA8FromParsed(parsed)
	if !hasSHA {
		logger.Warn("create_vm: stemcell filename sha8 unextractable; strategy=template falls back to import",
			log.String("stemcell_filename", parsed.stemcellFilename),
		)
		return false, nil
	}

	templateVMID, templateNode, found, resolveErr := resolveTemplateCacheTargetSettled(ctx, deps, logger, shape, sha8)
	if resolveErr != nil {
		return true, resolveErr
	}
	if !found {
		logger.Warn("create_vm: stemcell cache template still not visible for sha8 after re-checking;"+
			" falling back to the slower direct-import path (full copy instead of a clone)."+
			" Usual causes: PVE's /cluster/resources index has not caught up with a just-frozen"+
			" template, or the cache template create_stemcell built was deleted out of band",
			log.String("sha8", sha8),
			log.Int("recheck_attempts", templateCacheRecheckAttempts),
		)
		return false, nil
	}

	cloneErr := cloneFromTemplate(ctx, deps, logger, shape, candidate, candidateName, templateNode, templateVMID)
	if cloneErr != nil {
		if pve.IsCloneSourceMissing(cloneErr) {
			// The cache template vanished between resolveTemplateCacheTarget
			// and the clone POST (deleted out-of-band, or by a concurrent
			// delete_stemcell). The qcow2 the stemcell CID names may still
			// exist, so fall back to strategy=import — the same path a cache
			// miss takes. Sweep the candidate VMID first: the failed clone
			// may have left partial VM state behind.
			logger.Warn("create_vm: clone source template vanished mid-flight; falling back to direct import",
				log.Int("vmid_attempted", candidate),
				log.Int64("template_vmid", templateVMID),
				log.String("template_node", templateNode),
				log.String("sha8", sha8),
				log.ErrScrubbed(cloneErr),
			)
			sweepCandidateVMID(ctx, deps, shape.node, candidate, candidateName, nil, logger)
			return false, nil
		}
		// Classify for retry: VMID conflicts and transient transport faults are
		// retryable — they use the same retry classification as the import path.
		return true, handleCloneError(ctx, deps, logger, shape.node, candidate, candidateName, parsed.env, cloneErr)
	}

	logger.Info("create_vm: vm cloned from stemcell cache template",
		log.Int("vmid_attempted", candidate),
		log.Int64("template_vmid", templateVMID),
		log.String("template_node", templateNode),
		log.String("sha8", sha8),
	)
	// Apply post-clone config (pve_config passthrough + PCI hostpciN). Both
	// steps are no-ops when the respective cloud_properties are absent.
	return true, applyPostCloneConfig(ctx, deps, shape.node, candidate, parsed, logger)
}

// templateCacheRecheckAttempts bounds how many times
// resolveTemplateCacheTargetSettled looks for a cache template before
// conceding a genuine miss. Three attempts spaced by
// templateCacheRecheckDelay cover PVE's cluster-index lag with margin while
// costing a genuine miss under two seconds before it falls back to import.
const templateCacheRecheckAttempts = 3

// resolveTemplateCacheTargetSettled is resolveTemplateCacheTarget plus a
// bounded re-check that defeats template-visibility races.
//
// resolveTemplateCacheTarget originally read GET /cluster/resources, whose
// index trails a freshly frozen template by around a second. A BOSH director
// issues create_vm immediately after create_stemcell, so the first VM of
// every fresh-stemcell deploy hit that window: the lookup missed a template
// that had existed for well under a second, and the VM silently took the
// full-copy import path instead of the CoW clone. The cluster-scoped lookup
// has since moved to authoritative per-node listings (listClusterQemuTemplates
// via ListGuestsAuthoritative), which removes the index lag itself; the
// re-check below is retained because a template frozen mid-flight (the qm
// template task still running when the first lookup lands) is still invisible
// to any listing until the freeze commits, and the same settle loop covers
// that window.
//
// Two independent mechanisms close it, in this order per attempt:
//
//  1. The cluster-scoped lookup, which is the only one that can find a
//     template hosted on a node other than the placement target.
//  2. An authoritative per-node listing of the placement node
//     (pve.ResolveTemplateVMIDForNode → GET /nodes/<node>/qemu). That endpoint
//     reads the node's own guest config directly rather than the cluster
//     index, so it sees a just-frozen local template with no lag at all —
//     which resolves the single-node and same-node cases on the first attempt,
//     with no wait.
//  3. The same authoritative listing of the template's home node
//     (stemcell_template_node when set, the configured node otherwise) when
//     it differs from the placement node. Under the single-shared-template
//     topology the cache template create_stemcell just froze lives on the
//     staging node, not on the placement node, and clusters under load have
//     been observed lagging their /cluster/resources index by minutes: far
//     beyond the re-check budget. Probing the home node directly makes the
//     cross-node fresh-template case as lag-proof as the same-node case.
//
// Only when all miss does the attempt sleep and retry. A genuine absence
// (operator deleted the cache template) still falls through to found=false
// after the full budget, and the caller falls back to import as before.
// A ctx cancellation during the wait ends the re-check immediately and reports
// a miss — the import fallback is always a safe answer.
func resolveTemplateCacheTargetSettled(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	sha8 string,
) (templateVMID int64, templateNode string, found bool, err error) {
	for attempt := 1; attempt <= templateCacheRecheckAttempts; attempt++ {
		templateVMID, templateNode, found, err = resolveTemplateCacheTarget(ctx, deps, logger, shape, sha8)
		if err != nil || found {
			if found && attempt > 1 {
				logger.Info("create_vm: stemcell cache template became visible on re-check"+
					" (PVE cluster-resource index lag); cloning instead of importing",
					log.String("sha8", sha8),
					log.Int("attempt", attempt),
				)
			}
			return templateVMID, templateNode, found, err
		}

		// Authoritative per-node read: not served from the lagging cluster
		// index, so a template just frozen on this node is visible at once.
		if vmid, ok, probeErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, shape.node, sha8); probeErr != nil {
			logger.Warn("create_vm: authoritative per-node cache-template probe failed (continuing re-check)",
				log.String("node", shape.node),
				log.String("sha8", sha8),
				log.Err(probeErr),
			)
		} else if ok {
			logger.Info("create_vm: stemcell cache template found by authoritative per-node listing"+
				" after a cluster-index miss; cloning instead of importing",
				log.String("node", shape.node),
				log.Int("template_vmid", vmid),
				log.String("sha8", sha8),
				log.Int("attempt", attempt),
			)
			return int64(vmid), shape.node, true, nil
		}

		// Same authoritative read against the template's home node: a fresh
		// template built by create_stemcell lives there, and when the VM
		// places on a different node the probe above cannot see it while the
		// cluster index lags.
		if home := templateHomeNode(deps); home != "" && home != shape.node {
			if vmid, ok, probeErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, home, sha8); probeErr != nil {
				logger.Warn("create_vm: authoritative template-home-node cache-template probe failed (continuing re-check)",
					log.String("node", home),
					log.String("sha8", sha8),
					log.Err(probeErr),
				)
			} else if ok {
				logger.Info("create_vm: stemcell cache template found on its home node by authoritative"+
					" per-node listing after a cluster-index miss; cloning instead of importing",
					log.String("node", home),
					log.Int("template_vmid", vmid),
					log.String("sha8", sha8),
					log.Int("attempt", attempt),
				)
				return int64(vmid), home, true, nil
			}
		}

		if attempt == templateCacheRecheckAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return 0, "", false, nil
		case <-time.After(templateCacheRecheckDelay()):
		}
	}
	return 0, "", false, nil
}

// resolveTemplateCacheTarget resolves which cluster-scoped stemcell-cache
// template to clone for strategy=template.
//
// Selection order:
//  1. A template on shape.node — used directly, no cross-node concerns.
//  2. No template on shape.node, but the stemcell storage is shared across
//     the cluster (needsReplicaCheck reports false) — any cluster template
//     works; cloneFromTemplate handles the cross-node Target= redirect.
//  3. No template on shape.node and storage is local (needsReplicaCheck
//     reports true) — consult the per-node replica tag
//     (pve.ResolveTemplateVMIDForNode). A replica match wins. Otherwise,
//     when stemcell_replicate_local is disabled, an actionable error is
//     returned — replicating the pre-rewrite behavior at this branch point.
//     When enabled but no replica exists yet, falls through to the cluster
//     primary (lowest VMID) so cloneFromTemplate's own cross-node
//     local-storage guard produces the (more specific) rejection.
//
// Returns found=false (nil error) when no cache template exists anywhere in
// the cluster for sha8 — a true cache miss; the caller falls back to
// strategy=import. A non-nil error is a real actionable failure that must
// propagate, not fall back.
func resolveTemplateCacheTarget(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	sha8 string,
) (templateVMID int64, templateNode string, found bool, err error) {
	// Tolerant enumeration: this lookup's miss path falls open to import, so
	// a cluster member the quorate cluster reports offline must not fail (or
	// slow) every create_vm; a template living only on that member could not
	// be cloned from anyway.
	refs, listErr := pve.FindTemplatesBySHATagClusterTolerant(ctx, deps.PVE, sha8)
	if listErr != nil {
		// Lookup failure is non-fatal: log and fall through to import-from.
		// Do NOT fail create_vm on a read-only lookup error — the safe path
		// (import-from) is always available.
		logger.Warn("create_vm: cluster stemcell-cache template lookup failed, falling back to import-from",
			log.String("sha8", sha8),
			log.Err(listErr),
		)
		return 0, "", false, nil
	}
	if len(refs) == 0 {
		return 0, "", false, nil
	}

	// Prefer a template already on shape.node.
	for _, ref := range refs {
		if ref.Node == shape.node {
			return ref.VMID, ref.Node, true, nil
		}
	}

	// No template on shape.node. Shared stemcell storage: any cluster
	// template works — clone with a cross-node Target= redirect.
	if !needsReplicaCheck(ctx, deps, shape.vmStorage) {
		primary := refs[0]
		return primary.VMID, primary.Node, true, nil
	}

	// Local storage, no template on shape.node: consult the per-node replica
	// guard exactly as the pre-rewrite template-CID path did.
	replicaVMID, replicaFound, lookupErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, shape.node, sha8)
	if lookupErr != nil {
		logger.Warn("create_vm: template replica lookup failed (continuing with cluster primary)",
			log.String("node", shape.node),
			log.String("sha8", sha8),
			log.Err(lookupErr),
		)
		primary := refs[0]
		return primary.VMID, primary.Node, true, nil
	}
	if replicaFound {
		logger.Info("create_vm: using per-node template replica",
			log.String("node", shape.node),
			log.Int("replica_vmid", replicaVMID),
			log.String("sha8", sha8),
		)
		return int64(replicaVMID), shape.node, true, nil
	}
	if !deps.Config.StemcellReplicateLocal {
		// No replica found and replication is disabled — fail fast with an
		// actionable message rather than letting the clone fail opaquely.
		return 0, "", false, cpierrors.Cloud(
			"create_vm: stemcell cache template for sha8=%s not present on node %q "+
				"and stemcell storage %q is local; "+
				"either enable stemcell_replicate_local to allow per-node replication, "+
				"or use shared storage for the stemcell pool",
			sha8, shape.node, shape.vmStorage,
		)
	}
	// Replication enabled but no replica exists for this node yet: fall
	// through to the cluster primary (lowest VMID, deterministic) so
	// cloneFromTemplate's own cross-node local-storage guard produces the
	// rejection — mirrors the pre-rewrite behavior, where this same
	// configuration reached the clone attempt via the primary template.
	primary := refs[0]
	return primary.VMID, primary.Node, true, nil
}

// attemptStemcellImport imports the stemcell qcow2 directly into the VM's
// root disk via PVE's import-from= directive. Used for strategy=import and
// as the strategy=template fallback (unextractable sha8 or an empty cluster
// cache).
func attemptStemcellImport(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	candidate int,
	candidateName string,
) error {
	if existErr := verifyStemcellQcow2Exists(ctx, deps, logger, shape.node, parsed); existErr != nil {
		return existErr
	}

	rootDiskVal := fmt.Sprintf("%s:0,import-from=%s,format=%s,size=%dG",
		shape.vmStorage, parsed.rawVolid, shape.vmDiskFormat, shape.rootDiskGiB)
	// Append resolved per-disk performance options (iothread, cache, etc.) when
	// any are set. buildDiskOptStr treats the whole rootDiskVal string as the bare
	// volid prefix and appends ",key=value" pairs in deterministic alpha order.
	// When rootDiskPerfOpts is empty the value is unchanged (byte-identical path).
	if len(shape.rootDiskPerfOpts) > 0 {
		rootDiskVal = buildDiskOptStr(rootDiskVal, shape.rootDiskPerfOpts)
	}

	createParams := map[string]any{
		metadataKeyVMID:    candidate,
		metadataKeyName:    candidateName,
		pveConfigKeyMemory: shape.memMiB,
		"cores":            shape.cores,
		"ostype":           osTypeLinux26,
		"scsihw":           shape.scsihw,
		shape.rootDiskKey:  rootDiskVal,
		"boot":             "order=" + shape.rootDiskKey,
		"agent":            "enabled=1",
		"hotplug":          shape.hotplug,
		"onboot":           0,
		// Every BOSH VM is headless: the emulated USB tablet exists only to
		// smooth mouse tracking for an interactive VNC/SPICE console and costs
		// 2-3% CPU at scale for no benefit on a VM nobody looks at. No
		// cloud_properties override — this is unconditional on every VM.
		"tablet": 0,
		// Stemcells log the BOSH agent's console output to the serial console;
		// without a serial0 device that output has nowhere to go, which is a
		// direct hit to wedged-agent debuggability. "socket" is PVE's standard
		// virtual-console form for cloud-image guests (readable via `qm
		// terminal`). No cloud_properties knob for the default itself, but
		// cloud_properties.pve_config.serial0 (allowlisted) overrides it —
		// e.g. to redirect to a host device instead.
		pveConfigKeySerial0: "socket",
	}
	// The resolved pool (if any) must exist before it is handed to QEMU.Create
	// as the "pool" param below (applyOptionalCreateParams) — PVE rejects a
	// create referencing a non-existent pool. No-op when shape.vmPool == "".
	if err := ensureResolvedPool(ctx, deps, shape, logger); err != nil {
		return err
	}
	applyOptionalCreateParams(createParams, shape)

	upid, cerr := deps.PVE.QEMU().Create(ctx, shape.node, createParams)
	if cerr != nil {
		return handleCreateError(ctx, deps, logger, shape.node, candidate, candidateName, cerr)
	}

	if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, upid, logger,
		pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
		return handleAwaitError(ctx, deps, logger, shape.node, candidate, candidateName, werr)
	}

	logger.Info("create_vm: vm disk imported",
		log.Int("vmid_attempted", candidate),
		log.String("upid", upid),
	)
	// Apply post-clone config (pve_config passthrough + PCI hostpciN).
	return applyPostCloneConfig(ctx, deps, shape.node, candidate, parsed, logger)
}

// verifyStemcellQcow2Exists confirms the stemcell's qcow2 file is present on
// node's copy of parsed.stemcellStorage before an import-from= is submitted,
// producing a crisp, actionable error instead of an opaque PVE import
// failure.
//
// The existence check itself is best-effort: a lookup failure (API/transport
// error) is logged and the import proceeds — PVE surfaces its own error if
// the file is genuinely missing, and a lookup hiccup here must never hard-
// fail create_vm. Only a confirmed absence (a successful lookup that found no
// matching volid) is a hard failure.
func verifyStemcellQcow2Exists(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	parsed *createVMParsedArgs,
) error {
	volid, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, parsed.stemcellStorage, parsed.stemcellFilename)
	if findErr != nil {
		logger.Warn("create_vm: stemcell existence pre-check failed (continuing with import)",
			log.String("storage", parsed.stemcellStorage),
			log.String("filename", parsed.stemcellFilename),
			log.Err(findErr),
		)
		return nil
	}
	if volid == "" {
		return stemcellQcow2MissingError(ctx, deps, logger, node, parsed)
	}
	return nil
}

// stemcellQcow2MissingError builds the confirmed-absence error for
// verifyStemcellQcow2Exists, distinguishing two very different situations
// that look identical from the placement node:
//
//   - The qcow2 is genuinely gone (deleted, or never uploaded): the generic
//     "re-upload" guidance is correct.
//   - The qcow2 exists, but only on another node's copy of a node-local
//     staging pool (the single-shared-template topology: node-local
//     stemcell_storage + shared vm_storage). Direct import can never run
//     from this node; the cache template is the supported route, and this
//     code path is only reachable because that template was not found
//     (deleted out of band, or PVE's cluster index is lagging). The generic
//     "re-upload" guidance would be misleading: upload-stemcell --fix does
//     repair it, but by rebuilding the template, not by placing a qcow2
//     here.
//
// The cross-node probe is best-effort: enumeration or lookup failures fall
// back to the generic message. The topology error is retriable because the
// most common cause (index lag hiding a live template) heals on its own; a
// genuinely deleted template surfaces the same message, with the rebuild
// remedy, once the director's bounded retries are exhausted.
func stemcellQcow2MissingError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	parsed *createVMParsedArgs,
) error {
	generic := cpierrors.Cloud(
		"create_vm: stemcell qcow2 %s:%s not found — re-upload the stemcell (bosh upload-stemcell --fix)",
		parsed.stemcellStorage, parsed.stemcellVolPath,
	)
	if shared, known := stemcellStorageIsShared(ctx, deps, parsed.stemcellStorage); !known || shared {
		return generic
	}
	nodes, listErr := listClusterNodes(ctx, deps)
	if listErr != nil {
		logger.Warn("create_vm: cannot enumerate cluster nodes for cross-node stemcell probe (using generic error)",
			log.Err(listErr),
		)
		return generic
	}
	for _, n := range nodes {
		if n == node {
			continue
		}
		otherVolid, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, n, parsed.stemcellStorage, parsed.stemcellFilename)
		if findErr != nil {
			continue
		}
		if otherVolid != "" {
			// Under an explicit import strategy the cache template is
			// irrelevant and no retry can help: import always reads from the
			// VM's own node, and the qcow2 permanently lives elsewhere. Only
			// the template strategy's fallback has a self-healing cause
			// (cluster-index lag hiding a live template) worth retrying.
			if resolveStemcellStrategy(deps.Config, parsed) == config.StemcellStrategyImport {
				return cpierrors.Cloud(
					"create_vm: stemcell qcow2 %s:%s exists only on node %q's node-local storage and cannot be"+
						" imported from node %q; stemcell_strategy=import always reads from the VM's own node, so"+
						" drop the import override (letting the cache template serve this node via cross-node"+
						" clone) or pin the VM to node %q",
					parsed.stemcellStorage, parsed.stemcellVolPath, n, node, n,
				)
			}
			return cpierrors.Retriable(
				"create_vm: stemcell qcow2 %s:%s exists only on node %q's node-local storage and cannot be"+
					" imported from node %q; the stemcell cache template that normally serves this node via"+
					" cross-node clone was not found (deleted out of band, or PVE's cluster index is lagging);"+
					" retry the deploy, or run bosh upload-stemcell --fix to rebuild the cache template",
				parsed.stemcellStorage, parsed.stemcellVolPath, n, node,
			)
		}
	}
	return generic
}

// validateStemcellStrategyCloudProp validates the optional per-VM
// cloud_properties.stemcell_strategy override at parse time, before any PVE
// mutation. Empty is valid (defers to the global config default). Any
// non-empty value other than "template"/"import" is a manifest error.
func validateStemcellStrategyCloudProp(v string) error {
	switch v {
	case "", config.StemcellStrategyTemplate, config.StemcellStrategyImport:
		return nil
	default:
		return cpierrors.Cloud(
			"create_vm: cloud_properties.stemcell_strategy %q invalid; must be %q, %q, or omitted",
			v, config.StemcellStrategyTemplate, config.StemcellStrategyImport,
		)
	}
}

// resolveStemcellStrategy resolves the effective stemcell strategy: per-VM
// cloud_properties.stemcell_strategy (validated at parse time by
// validateStemcellStrategyCloudProp) takes precedence over the global
// pve.stemcell_strategy config value, itself defaulting to "template"
// (config.ApplyDefaults normally fills this; the nil/empty fallback here
// covers direct unit-test construction of *config.CPIConfig).
func resolveStemcellStrategy(cfg *config.CPIConfig, parsed *createVMParsedArgs) string {
	if parsed.cloudProps.StemcellStrategy != "" {
		return parsed.cloudProps.StemcellStrategy
	}
	if cfg != nil && cfg.StemcellStrategy != "" {
		return cfg.StemcellStrategy
	}
	return config.StemcellStrategyTemplate
}

// startVMAndReadConfig starts the VM, awaits the task, reads back PVE config
// to extract MAC addresses, and returns the populated response networks map.
func startVMAndReadConfig(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
	nicPlan []nicPlanEntry,
) (map[string]createVMNetworkSpec, error) {
	// -----------------------------------------------------------------------
	// 8. Start VM
	// -----------------------------------------------------------------------
	// Wrap in RetryOnTransient so a pvedaemon worker-recycle (HTTP 5xx /
	// "got no worker upid - start worker failed") under burst load is absorbed
	// in-process rather than surfacing as RetriableCloudError to the director.
	// A start whose first attempt committed but whose response was dropped
	// replays into an "already running" rejection; the goal state (a running
	// VM) is reached whoever reached it, so tolerate that outcome on both the
	// submit and the task await, falling back to a live status probe when the
	// rejection text does not match.
	var startUPID string
	startErr := pve.RetryOnTransient(ctx, logger, "create_vm.start", 0, func() error {
		var innerErr error
		startUPID, innerErr = deps.PVE.QEMU().Start(ctx, shape.node, vmid)
		return innerErr
	})
	switch {
	case startErr == nil:
		if err := pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, startUPID, logger); err != nil {
			if !vmAlreadyRunning(err) && !vmRunningNow(ctx, deps, logger, shape.node, vmid) {
				return nil, cpierrors.Wrap(pve.WrapErrorKeepingClass(err),
					fmt.Sprintf("create_vm: await start task vmid=%d", vmid))
			}
			logger.Info("create_vm: start task raced a prior committed start; VM already running",
				log.Int(metadataKeyVMID, vmid))
		}
	case vmAlreadyRunning(startErr) || vmRunningNow(ctx, deps, logger, shape.node, vmid):
		logger.Info("create_vm: start replay found the VM already running; goal state reached",
			log.Int(metadataKeyVMID, vmid))
	default:
		// WrapErrorKeepingClass: the exhausted retry error is the raw last
		// SDK error, and flattening it to a permanent Cloud made a transient
		// start failure non-retriable for the Director.
		return nil, cpierrors.Wrap(pve.WrapErrorKeepingClass(startErr),
			fmt.Sprintf("create_vm: start vmid=%d", vmid))
	}

	logger.Info("create_vm: VM started", log.Int(metadataKeyVMID, vmid))

	// -----------------------------------------------------------------------
	// 9. Read back VM config to extract assigned MAC addresses
	// -----------------------------------------------------------------------
	vmCfg, err := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
	if err != nil {
		// Non-fatal: return networks without MAC rather than rolling back
		logger.Warn("create_vm: could not read VM config for MAC extraction",
			log.Int(metadataKeyVMID, vmid),
			log.Err(err),
		)
		vmCfg = map[string]any{}
	}

	return buildResponseNetworks(parsed.networks, nicPlan, vmCfg), nil
}
