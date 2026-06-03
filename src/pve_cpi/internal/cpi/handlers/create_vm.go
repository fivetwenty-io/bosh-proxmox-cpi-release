package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	mrand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
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
	Disk          int    `json:"disk"`
	NetworkBridge string `json:"network_bridge"` // per-VM bridge override
	NetworkModel  string `json:"network_model"`  // virtio|e1000 etc.
	// Hotplug overrides the CPI default for this VM (config.Hotplug).
	// Pointer-typed so the caller can distinguish "not set" (use config
	// default) from "set to empty string" (currently treated the same:
	// fall back to default). Use "0" to explicitly disable hotplug.
	Hotplug *string `json:"hotplug,omitempty"`
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
}

// createVMNetworkSpec mirrors the BOSH v2 network spec shape.
type createVMNetworkSpec struct {
	Type            string         `json:"type"`
	IP              string         `json:"ip"`
	Netmask         string         `json:"netmask"`
	Gateway         string         `json:"gateway"`
	DNS             []string       `json:"dns"`
	Default         []string       `json:"default"` // ["dns","gateway"]
	CloudProperties map[string]any `json:"cloud_properties"`
	MAC             string         `json:"mac,omitempty"` // filled in response
}

// createVMParsedArgs holds the validated, unmarshalled create_vm arguments.
type createVMParsedArgs struct {
	agentID      string
	stemcellCID  string // original (may have "light:" prefix)
	rawCID       string // stripped of "light:" prefix
	stemcellStor string // storage component of rawCID
	cloudProps   createVMCloudProps
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
}

// HandleCreateVM returns a cpi.Handler that implements the BOSH CPI create_vm method.
//
// Arguments (positional, all required):
//
//	[0] agent_id      string
//	[1] stemcell_cid  string — CID returned by create_stemcell. Supported formats:
//	                    "template:<vmid>"           — new template CID; VM is created by cloning the template.
//	                    "<storage>:import/<file>"   — pre-upgrade volume CID; the CPI checks for a matching
//	                                                  template by sha8 tag and clones it if found, otherwise
//	                                                  falls back to import-from= (slow path).
//	                    "light:<storage>:import/<file>" — pre-upgrade light CID; same opportunistic logic as above.
//	[2] cloud_props   map     (cores, sockets, memory, vm_disk_format, target_node, ...)
//	[3] networks      map[name]NetworkSpec
//	[4] disk_cids     []string  (persistent disks to pre-attach)
//	[5] env           map[string]any
//
// Returns v2 2-tuple: [vm_cid_string, networks_map_with_mac_addresses].
//
// Rollback: if any step after VM creation fails, the VM is stopped (best-effort)
// and destroyed (purge=true) before the error is returned.
func HandleCreateVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return createVM(ctx, deps, args)
	})
}

// createVM is the implementation body — separated for testability.
func createVM(
	ctx context.Context,
	deps Deps,
	args []json.RawMessage,
) (result any, retErr error) {
	logger := deps.Logger

	// -----------------------------------------------------------------------
	// 1. Parse + validate arguments
	// -----------------------------------------------------------------------
	parsed, err := parseCreateVMArgs(args)
	if err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 2–3. Resolve node and VM-shape parameters.
	// -----------------------------------------------------------------------
	shape, err := resolveVMShape(ctx, deps, parsed)
	if err != nil {
		return nil, err
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

	// Arm rollback for stages 4b–8: any failure after this point destroys
	// the winning VM.
	vmCreated := true
	defer func() {
		if retErr != nil && vmCreated {
			cleanupVM(contextWithoutCancel(ctx), deps, shape.node, vmid, logger)
		}
	}()

	logger.Info("create_vm: vm created and disk imported",
		log.Int(metadataKeyVMID, vmid),
		log.String("stemcell_cid", parsed.stemcellCID),
		log.String("storage", shape.vmStorage),
		log.Int("root_disk_gib", shape.rootDiskGiB),
	)

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
	netNames, err := configureNICs(ctx, deps, logger, parsed, shape, vmid)
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
	// 7. Build AgentConfig and call agent.Configure
	// -----------------------------------------------------------------------
	if err := configureAgent(ctx, deps, logger, parsed, shape, vmid, vmName); err != nil {
		return nil, err
	}

	// -----------------------------------------------------------------------
	// 8. Start VM + read back VM config to extract assigned MAC addresses
	// -----------------------------------------------------------------------
	responseNetworks, err := startVMAndReadConfig(ctx, deps, logger, parsed, shape, vmid, netNames)
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
	if len(effectiveGroups) > 0 {
		if fwErr := applySecurityGroups(ctx, deps, shape.node, vmid, effectiveGroups, logger); fwErr != nil {
			return nil, fwErr
		}
	} else {
		firewallEnabled, fwFlagErr := resolveEffectiveFirewall(parsed.cloudPropsMap, deps.Config)
		if fwFlagErr != nil {
			return nil, fwFlagErr
		}
		if firewallEnabled {
			if fwErr := enableVMFirewall(ctx, deps, shape.node, vmid, logger); fwErr != nil {
				return nil, fwErr
			}
		}
	}

	// -----------------------------------------------------------------------
	// 9. PVE HA anti-affinity membership (opt-in: anti_affinity.use_ha_rules).
	//
	// Best-effort and non-fatal: HA being unconfigured, or any rule-write
	// failure, is logged as a warning and never fails create_vm (scheduler-soft
	// spreading remains in effect via the scoring done at node selection).
	// -----------------------------------------------------------------------
	if deps.Config.AntiAffinityUseHaRulesEnabled() {
		if groupKey := sanitizeTagValue(instanceGroupName(parsed.env)); groupKey != "" {
			if aaErr := ensureAntiAffinityMembership(ctx, deps, groupKey, vmid, logger); aaErr != nil {
				logger.Warn("create_vm: HA anti-affinity membership not fully applied (non-fatal)",
					log.Int(metadataKeyVMID, vmid), log.String("group", groupKey), log.Err(aaErr))
			}
		}
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
	// 11. Post-create health gate (opt-in: health_check.enabled).
	//
	// When enabled, poll the QEMU guest agent until it answers or the deadline
	// expires. On failure, diagnostics from ListQemuStatusCurrent are folded
	// into the error before the standard rollback defer fires and destroys the
	// VM. Default off — behavior is byte-identical to prior releases when the
	// block is absent or Enabled is false.
	// -----------------------------------------------------------------------
	if deps.Config.HealthCheckEnabled() {
		if hcErr := waitUntilAgentReady(ctx, deps, shape.node, vmid, logger); hcErr != nil {
			return nil, hcErr
		}
	}

	vmCID := strconv.Itoa(vmid)
	return []any{vmCID, responseNetworks}, nil
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
	// Strip the "light:" prefix if present. Light stemcell CIDs are
	// "light:<storage>:import/<file>"; PVE's import-from= accepts only the
	// underlying "<storage>:import/<file>" volid. The light: marker exists so
	// delete_stemcell can recognize and no-op these CIDs without consulting
	// any external state.
	rawCID := pve.StripLightPrefix(stemcellCID)
	// stemcellStor is used as a fallback VMStorage when deps.Config.VMStorage
	// is empty (see VMStorage resolution below). For template CIDs ("template:<vmid>")
	// there is no storage component — stemcellStor is left empty and VMStorage
	// must be set in config or cloud_properties.
	var stemcellStor string
	if pve.IsTemplateStemcellCID(stemcellCID) {
		// Template CIDs carry only a VMID; validate the format now so errors
		// surface at parse time rather than at candidate-allocation time.
		if _, err := pve.ParseTemplateStemcellCID(stemcellCID); err != nil {
			return nil, cpierrors.Cloud("create_vm: invalid template stemcell_cid %q: %s", stemcellCID, err.Error())
		}
		// stemcellStor stays "" — VMStorage from config or resolveVMShapeStorage
		// must supply the target storage. Template clones carry no import-from path.
	} else {
		// Old-form CID: "light:<storage>:import/<file>" or "<storage>:import/<file>".
		// ParseStemcellCID guarantees a non-empty storage component when err == nil.
		var err error
		stemcellStor, _, err = pve.ParseStemcellCID(rawCID)
		if err != nil {
			return nil, cpierrors.Cloud("create_vm: invalid stemcell_cid %q: %s", stemcellCID, err.Error())
		}
	}

	var cloudProps createVMCloudProps
	if err := json.Unmarshal(args[2], &cloudProps); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse cloud_properties: %s", err.Error())
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

	var diskCIDs []string
	if err := json.Unmarshal(args[4], &diskCIDs); err != nil {
		return nil, cpierrors.Cloud("create_vm: parse disk_cids: %s", err.Error())
	}

	// Disk slot layout:
	//   virtio0       system disk (stemcell-imported root; see create flow below).
	//   scsi1..scsi28 persistent disks at create_vm time + dynamic attach_disk.
	//   scsi30        ConfigDrive CD-ROM (see agent.configDriveSlot); scsi29 headroom.
	// scsi0 is intentionally left unused so AttachDisk's free-slot search
	// (which starts at scsi1) and the agent's expectations stay simple.
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
		agentID:       agentID,
		stemcellCID:   stemcellCID,
		rawCID:        rawCID,
		stemcellStor:  stemcellStor,
		cloudProps:    cloudProps,
		cloudPropsMap: cloudPropsMap,
		networks:      networks,
		diskCIDs:      diskCIDs,
		env:           env,
	}, nil
}

// resolveVMShape derives the createVMShape from deps.Config + parsed args.
// Returns cpierrors.CloudError if the target node cannot be determined.
// vmStorageType is populated via a best-effort cluster storage list lookup;
// on failure (PVE unavailable, ClusterStorage not wired) the field is left ""
// so IsLinkedCloneSupported treats it as linked-capable (permissive default).
func resolveVMShape(ctx context.Context, deps Deps, parsed *createVMParsedArgs) (*createVMShape, error) {
	cp := parsed.cloudProps

	// Anti-affinity group tag (Tier 2, scheduler-soft spreading). Only computed
	// when anti-affinity is enabled; otherwise the scorer ignores group membership
	// and behavior is identical to Tier 1.
	groupTag := antiAffinityGroupTag(deps.Config, parsed.env)

	node, err := resolveTargetNode(ctx, deps, cp, groupTag, parsed.diskCIDs, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}

	rangeStart, maxAttempts := resolveVMIDAllocParams(deps.Config)
	// Build a tier-resolver closure so resolveVMShapeStorage can call
	// resolveStorageTier without its signature carrying ctx/Deps directly.
	// The closure is only invoked when cloud_properties.storage_tier is set
	// in the resolver layers; nil ClusterStorage falls through to config fallback.
	var tierFnForVM vmStorageTierFn
	if deps.PVE != nil && deps.PVE.ClusterStorage() != nil {
		lister := deps.PVE.ClusterStorage()
		cfg := deps.Config
		tierFnForVM = func(tier string) (string, error) {
			return resolveStorageTier(ctx, lister, cfg, tier)
		}
	}
	vmStorage, vmDiskFormat, rootDiskGiB, err := resolveVMShapeStorage(deps.Config, parsed, tierFnForVM)
	if err != nil {
		return nil, err
	}
	cores, sockets, memMiB := resolveVMShapeCPUMem(cp)
	hotplug, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(deps.Config, cp, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}

	// Operator-supplied tags only. The BOSH-managed director/deployment/job
	// triple is added later by set_vm_metadata.
	initialTags := mergeTagList(nil, buildCustomTags(cp.Tags), maxTagLength)
	initialName := resolveVMShapeInitialName(deps.Config, parsed)

	// Best-effort: populate vmStorageType for the clone-mode decision in
	// cloneFromTemplate. A lookup error leaves the field "" which
	// IsLinkedCloneSupported treats as linked-capable (permissive).
	vmStorageType := lookupVMStorageType(ctx, deps, vmStorage)

	return &createVMShape{
		node:          node,
		vmStorage:     vmStorage,
		vmStorageType: vmStorageType,
		vmDiskFormat:  vmDiskFormat,
		rootDiskGiB:   rootDiskGiB,
		cores:         cores,
		sockets:       sockets,
		memMiB:        memMiB,
		hotplug:       hotplug,
		numaEnabled:   numaEnabled,
		initialTags:   initialTags,
		rangeStart:    rangeStart,
		maxAttempts:   maxAttempts,
		initialName:   initialName,
		cloudPropsMap: parsed.cloudPropsMap,
	}, nil
}

// resolveTargetNode determines which PVE node the new VM will land on.
//
// Decision tree (evaluated in order):
//  1. cp.TargetNode != "" → operator override; skip scoring entirely (backward compat).
//  2. deps.Config.PlacementEnabled() == true → live placement scoring:
//     a. Build AZ order: singular availability_zone → single-element list (backward
//     compat). Plural availability_zones → iterate in operator order (shuffle if
//     placement.az_shuffle is true). Append config.placement.az_fallback_order.
//     b. GatherNodeFacts once (cluster-wide). ExcludeMaintenanceNodes wired from
//     config default (true).
//     c. For each AZ: resolve candidate set, Filter+Score+Pick. Advance to next
//     AZ on empty-after-filter. Return chosen node on first viable AZ.
//     d. No viable AZ: classify rejection causes. Transient causes →
//     cpierrors.Retriable. Permanent (bad AZ name) → cpierrors.Cloud.
//     e. After all AZs exhausted, fall back to config.node with a warning.
//  3. deps.Config.PlacementEnabled() == false → deps.Config.Node (legacy behavior).
//  4. All paths: if the resolved node is still "" → CloudError.
//
// diskCIDs carries the persistent disk CIDs passed to create_vm. When non-empty,
// disk fault-domain constraints are derived before placement runs:
//   - local-storage disks pin the VM to the disk's home node (hard constraint).
//   - shared-storage disks with an AZ label constrain the AZ order.
//   - bare legacy CIDs (no metadata) impose no constraint.
//
// groupTag, when non-empty, is the anti-affinity tag (e.g. "job--diego-cell")
// that activates scheduler-soft same-group spreading.
//
// rng is injected for deterministic shuffle in tests; pass nil for production
// (a fresh rand source is created from the current time).
//
//nolint:gocognit // Multi-AZ loop + maintenance + retryability; inherent complexity.
func resolveTargetNode(ctx context.Context, deps Deps, cp createVMCloudProps, groupTag string, diskCIDs []string, cloudPropsMap map[string]any) (string, error) {
	return resolveTargetNodeWithRNG(ctx, deps, cp, groupTag, diskCIDs, nil, cloudPropsMap)
}

// resolveTargetNodeWithRNG is the testable implementation of resolveTargetNode.
// rng controls AZ shuffle order; pass nil for production (non-deterministic).
// cloudPropsMap is the raw cloud_properties map used to build the layered resolver
// for per-call placement weight overrides and AZ resolution via vm_type profiles.
// Pass nil to skip resolver-based overrides (existing behavior preserved byte-identically).
//
//nolint:gocognit // Multi-AZ loop + maintenance + retryability; inherent complexity.
func resolveTargetNodeWithRNG(
	ctx context.Context,
	deps Deps,
	cp createVMCloudProps,
	groupTag string,
	diskCIDs []string,
	rng *rand.Rand,
	cloudPropsMap map[string]any,
) (string, error) {
	// Nil-guard the logger: internal unit tests that call resolveVMShape directly
	// may leave deps.Logger unset. Use a nop logger in that case so logging calls
	// are safe without requiring all callers to set a logger.
	logger := deps.Logger
	if logger == nil {
		logger = log.NewNopLogger()
	}

	// Derive hard fault-domain constraints from persistent disk CIDs before any
	// placement decision. Bare legacy CIDs (no metadata) impose no constraint.
	// Backend resolution uses the static resolver when Deps.Resolver is unset, so
	// this step is safe in both production and test environments.
	diskConstraints, dcErr := deriveDiskFaultConstraints(ctx, deps, diskCIDs)
	if dcErr != nil {
		return "", dcErr
	}

	// Branch 1: operator pin — no scoring.
	// If a local disk's node is known, validate consistency with the operator override:
	// pinning to a conflicting node would leave the disk unreachable.
	if cp.TargetNode != "" {
		if diskConstraints.requiredLocalNode != "" && diskConstraints.requiredLocalNode != cp.TargetNode {
			return "", cpierrors.Cloud(
				"create_vm: cloud_properties.target_node=%q conflicts with local disk placement constraint (disk node=%q); "+
					"set target_node=%q or move the disk to shared storage",
				cp.TargetNode, diskConstraints.requiredLocalNode, diskConstraints.requiredLocalNode,
			)
		}
		logger.Debug("create_vm: node selection: operator override via target_node",
			log.String("node", cp.TargetNode),
		)
		return cp.TargetNode, nil
	}

	// Build the layered resolver for per-call placement weight and AZ overrides.
	// A nil or empty cloudPropsMap produces a call-only resolver with no profile layers;
	// all lookups return not-found and behavior is byte-identical to the pre-resolver path.
	// An unknown vm_type/disk_type selector returns a CloudError immediately.
	var cpResolver *layeredResolver
	if deps.Config != nil {
		var resolverErr error
		cpResolver, resolverErr = newLayeredResolver(cloudPropsMap, deps.Config)
		if resolverErr != nil {
			return "", resolverErr
		}
	}

	// Branch 2: live placement scoring.
	// Skip when deps.PVE is nil (unit test minimal setup) — fall through to Branch 3.
	if deps.Config.PlacementEnabled() && deps.PVE != nil {
		// Build the AZ iteration order.
		// Singular availability_zone (backward compat) → single-element list,
		// no multi-AZ fallback behavior.
		azOrder := buildAZOrder(cp, deps.Config, rng, cpResolver)

		// Apply shared-disk AZ constraint: if disks declare required AZs and the
		// VM's AZ order is empty, constrain placement to the disk AZs. If the VM's
		// AZ order is set and required AZs are a subset, keep the intersection in
		// their original order. If required AZs are not a subset of the VM's AZ
		// order, return a clear non-retriable error.
		if len(diskConstraints.requiredAZs) > 0 {
			azOrder, dcErr = applyDiskAZConstraint(azOrder, diskConstraints.requiredAZs)
			if dcErr != nil {
				return "", dcErr
			}
		}

		// Pre-validate AZs: any unknown AZ name is a permanent misconfiguration.
		// This check runs before GatherNodeFacts to preserve the existing behavior
		// that unknown-AZ errors surface without making any cluster API calls.
		// DLB sentinel AZs are silently skipped (not an error).
		for _, az := range azOrder {
			_, ok := deps.Config.AZCandidates(az)
			if !ok && (az != deps.Config.DLBAZName() || deps.Config.DLBAZName() == "") {
				return "", cpierrors.Cloud(
					"create_vm: availability_zone %q is not defined in placement.az_map; "+
						"add the AZ to config.placement.az_map or remove availability_zone from cloud_properties",
					az,
				)
			}
		}

		// Gather live cluster facts once before the AZ loop.
		// ExcludeMaintenanceNodes defaults true; MaintenanceNodeTags defaults ["maintenance"].
		storageName := deps.Config.VMStorage
		excludeMaintenance := deps.Config.ExcludeMaintenanceNodesEnabled()
		facts, gatherErr := placement.GatherNodeFacts(ctx,
			deps.PVE.Cluster(),
			deps.PVE.Nodes(),
			logger,
			placement.GatherOptions{
				StorageName:             storageName,
				GroupTag:                groupTag,
				ExcludeMaintenanceNodes: excludeMaintenance,
				MaintenanceNodeTags:     deps.Config.MaintenanceNodeTagsValue(),
			},
		)
		if gatherErr != nil {
			// GatherNodeFacts returns a fatal error only when ListStatus fails.
			// Wrap and propagate — director will retry create_vm.
			return "", cpierrors.Wrap(pve.WrapError(gatherErr),
				"create_vm: placement: gather node facts")
		}

		w := deps.Config.EffectiveWeights()
		weights := placement.Weights{
			Mem:        w.Mem,
			Storage:    w.Storage,
			CPU:        w.CPU,
			GuestCount: w.GuestCount,
		}
		// Per-call cloud_properties weight overrides (opt-in, no global mutation).
		// Only axes with an explicit value in the resolver override the config axis;
		// absent keys leave the config value intact. AntiAffinity is config-only.
		if cpResolver != nil {
			if f, found := cpResolver.Float("placement_weight_mem"); found {
				weights.Mem = f
			}
			if f, found := cpResolver.Float("placement_weight_storage"); found {
				weights.Storage = f
			}
			if f, found := cpResolver.Float("placement_weight_cpu"); found {
				weights.CPU = f
			}
			if f, found := cpResolver.Float("placement_weight_guest_count"); found {
				weights.GuestCount = f
			}
		}
		if groupTag != "" {
			weights.AntiAffinity = placement.DefaultWeights().AntiAffinity
		}

		// Local-disk node pin: if all local disks share one node, force the
		// candidate set to that single node for all AZ iterations. This is checked
		// after GatherNodeFacts so we can report whether the node is
		// offline/maintenance rather than returning a generic "no candidates" error.
		localPin := diskConstraints.requiredLocalNode
		if localPin != "" {
			// Search facts slice for the pinned node name.
			var pinnedFact *placement.NodeFacts
			for i := range facts {
				if facts[i].Node == localPin {
					pinnedFact = &facts[i]
					break
				}
			}
			if pinnedFact == nil {
				return "", cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is not reachable in the cluster (offline, removed, or unknown); "+
						"ensure the disk's home node is online before creating the VM",
					localPin,
				)
			}
			if pinnedFact.InMaintenance {
				return "", cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is currently in maintenance; wait for maintenance to complete or "+
						"migrate the disk to a different node",
					localPin,
				)
			}
			if !pinnedFact.Online {
				return "", cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is offline; bring the node online before creating the VM",
					localPin,
				)
			}
			logger.Debug("create_vm: node selection: local disk pins node",
				log.String("node", localPin),
			)
			return localPin, nil
		}

		// AZ loop. When azOrder is empty (no AZ set at all), run a single
		// iteration with no candidate restriction (all nodes).
		allRejections := make(map[string]string)

		if len(azOrder) == 0 {
			// No AZ constraint: all nodes are candidates.
			req := placement.Request{
				ExcludeMaintenanceNodes: excludeMaintenance,
			}
			pass, rejections := placement.Filter(facts, req)
			mergeRejections(allRejections, rejections)
			logFilterRejections(logger, rejections, "")
			if chosen := scoreAndPick(pass, weights, logger, ""); chosen != "" {
				return chosen, nil
			}
		} else {
			for _, az := range azOrder {
				candidateSet, skipSilently := resolveAZCandidatesValidated(az, deps.Config, logger)
				if skipSilently {
					// DLB sentinel AZ: skip scoring, no error (pre-validation already passed).
					continue
				}

				req := placement.Request{
					CandidateNodes:          candidateSet,
					ExcludeMaintenanceNodes: excludeMaintenance,
				}
				pass, rejections := placement.Filter(facts, req)
				mergeRejections(allRejections, rejections)
				logFilterRejections(logger, rejections, az)

				if chosen := scoreAndPick(pass, weights, logger, az); chosen != "" {
					return chosen, nil
				}
				logger.Debug("create_vm: placement: AZ exhausted, trying next",
					log.String("az", az),
				)
			}
		}

		// All AZs exhausted (or no-AZ single pass had no candidates).
		fallback := deps.Config.Node
		logger.Warn("create_vm: placement: no viable candidates; falling back to config.node",
			log.String("fallback", fallback),
		)
		if fallback == "" {
			if classifyFilterResult(allRejections) {
				return "", cpierrors.Retriable(
					"create_vm: no viable placement candidates (transient); "+
						"all nodes rejected: %s",
					formatRejections(allRejections),
				)
			}
			return "", cpierrors.Cloud(
				"create_vm: no viable placement candidates; "+
					"all nodes rejected: %s",
				formatRejections(allRejections),
			)
		}
		return fallback, nil
	}

	// Branch 3: placement disabled or PVE nil — legacy static node.
	// When a local disk's home node is known, the VM must land there regardless
	// of placement being disabled. This ensures co-location even in single-node
	// static configs where config.node and the disk's node should agree; if they
	// conflict we surface a clear error rather than silently creating an inaccessible VM.
	if diskConstraints.requiredLocalNode != "" {
		logger.Debug("create_vm: node selection: local disk pins node (placement disabled)",
			log.String("node", diskConstraints.requiredLocalNode),
		)
		return diskConstraints.requiredLocalNode, nil
	}

	node := deps.Config.Node
	if node == "" {
		return "", cpierrors.Cloud(
			"create_vm: target node not set in cloud_properties.target_node or config.node",
		)
	}
	logger.Debug("create_vm: node selection: placement disabled, using config.node",
		log.String("node", node),
	)
	return node, nil
}

// applyDiskAZConstraint reconciles the VM's AZ order with the AZs required by
// shared-storage persistent disk CIDs.
//
// Rules (all non-retriable — disk AZ conflicts are operator configuration errors):
//   - VM AZ order empty: return the sorted required AZ list (constrain to disk AZs).
//   - VM AZ order non-empty: return only the AZs present in both the VM order and
//     requiredAZs, in the VM's original order (intersection). If the intersection
//     is empty, return a CloudError: the VM's AZ configuration is incompatible with
//     the disk's AZ requirement.
func applyDiskAZConstraint(azOrder []string, requiredAZs map[string]struct{}) ([]string, error) {
	if len(requiredAZs) == 0 {
		return azOrder, nil
	}

	if len(azOrder) == 0 {
		// No VM AZ preference: constrain to disk AZs in sorted order for determinism.
		result := make([]string, 0, len(requiredAZs))
		for az := range requiredAZs {
			result = append(result, az)
		}
		sort.Strings(result)
		return result, nil
	}

	// Intersect: keep VM AZ order but drop AZs not in requiredAZs.
	result := make([]string, 0, len(azOrder))
	for _, az := range azOrder {
		if _, required := requiredAZs[az]; required {
			result = append(result, az)
		}
	}
	if len(result) == 0 {
		reqList := make([]string, 0, len(requiredAZs))
		for az := range requiredAZs {
			reqList = append(reqList, az)
		}
		sort.Strings(reqList)
		return nil, cpierrors.Cloud(
			"create_vm: VM availability_zone(s) %v do not include the AZ(s) required "+
				"by persistent disk(s): %v; update cloud_properties.availability_zone(s) "+
				"to include a matching AZ, or move the disk(s) to shared storage without an AZ label",
			azOrder, reqList,
		)
	}
	return result, nil
}

// buildAZOrder constructs the ordered AZ list for a placement attempt.
//
// Priority (highest to lowest):
//  1. cp.AvailabilityZone (singular, per-call) — backward compat; returns 1-elem slice, no fallback.
//  2. cp.AvailabilityZones (plural, per-call) — iterate in operator order.
//  3. resolver.String("availability_zone") — singular from profile; same semantics as #1.
//  4. resolver.StringSlice("availability_zones") — plural from profile; feeds multi-AZ path.
//  5. cfg.AZFallbackOrderValue() — config-level fallback appended after any plural list.
//
// Steps 3–4 are only consulted when both per-call fields are absent (empty).
// Pass a nil resolver to skip profile-based AZ resolution entirely (byte-identical to the
// pre-resolver behavior).
func buildAZOrder(cp createVMCloudProps, cfg *config.CPIConfig, rng *rand.Rand, resolver *layeredResolver) []string {
	// Singular per-call takes precedence — backward compat, no multi-AZ behavior.
	if cp.AvailabilityZone != "" {
		return []string{cp.AvailabilityZone}
	}

	// Plural per-call list is the starting point for multi-AZ behavior.
	startList := cp.AvailabilityZones

	// When the per-call fields are both absent, consult the resolver for profile-supplied AZ.
	if len(startList) == 0 && resolver != nil {
		if az, found := resolver.String("availability_zone"); found {
			// Singular profile AZ: same backward-compat semantics as cp.AvailabilityZone.
			return []string{az}
		}
		if azs, found := resolver.StringSlice("availability_zones"); found {
			// Plural profile AZ list: use as starting point for multi-AZ + fallback logic.
			startList = azs
		}
	}

	if len(startList) == 0 && len(cfg.AZFallbackOrderValue()) == 0 {
		return nil // no AZ constraint
	}

	// Start from the resolved list (copy to avoid mutating caller's slice or resolver output).
	order := make([]string, len(startList))
	copy(order, startList)

	if cfg.AZShuffleEnabled() && len(order) > 1 {
		if rng == nil {
			rng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // shuffle; non-cryptographic
		}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}

	// Append fallback AZs not already in the list.
	inOrder := make(map[string]struct{}, len(order))
	for _, az := range order {
		inOrder[az] = struct{}{}
	}
	for _, az := range cfg.AZFallbackOrderValue() {
		if _, already := inOrder[az]; !already {
			order = append(order, az)
			inOrder[az] = struct{}{}
		}
	}
	return order
}

// diskFaultConstraints carries the hard placement constraints derived from
// persistent disk CIDs before VM placement runs.
//
// requiredLocalNode, when non-empty, is the single PVE node that all
// local-storage disks share. The VM must land on this node.
//
// requiredAZs, when non-empty, is the set of AZ labels from shared-storage
// disks whose CID metadata carries an AZ. The VM's AZ order must intersect
// this set when an AZ is configured; if not, placement is constrained to only
// those AZs.
type diskFaultConstraints struct {
	// requiredLocalNode is set when one or more local-storage disks have a node
	// recorded in their CID metadata. Empty means no local-node pin.
	requiredLocalNode string
	// requiredAZs collects AZ labels from shared-storage disks. Empty means
	// no AZ constraint from persistent disks.
	requiredAZs map[string]struct{}
}

// deriveDiskFaultConstraints inspects each disk CID and builds the set of
// placement constraints it implies. Bare legacy CIDs (no metadata) are silently
// skipped to preserve backward compatibility.
//
// Errors returned:
//   - Two or more local-storage disks on different nodes → cpierrors.Cloud.
//   - Backend resolution failure → cpierrors.Wrap (unexpected; safe to retry).
//
// The ctx is used only for backend Resolve calls (cached in production).
func deriveDiskFaultConstraints(ctx context.Context, deps Deps, diskCIDs []string) (diskFaultConstraints, error) {
	var c diskFaultConstraints
	if len(diskCIDs) == 0 {
		return c, nil
	}

	resolver := backendResolverOrDefault(deps)
	localNodes := make(map[string]struct{}) // unique local nodes seen

	for _, cid := range diskCIDs {
		if cid == "" {
			continue
		}
		_, meta, err := pve.ParseEncodedDiskCID(cid)
		if err != nil || meta == nil {
			// Bare legacy CID or parse failure: impose no constraint.
			// Parse errors on bare CIDs are not possible (ParseEncodedDiskCID
			// returns err only when "|" present but suffix malformed); the
			// caller already validated CIDs at parse time.
			continue
		}

		// Determine backend kind for this disk's pool.
		pool := meta.Pool
		if pool == "" {
			// Pool absent from meta but node/AZ may still be set (e.g. CID
			// written by an older CPI version that set Node/AZ without Pool).
			// Derive the pool from the bare CID so the node/AZ constraint is
			// not silently dropped. Fail closed: if ParseDiskCID cannot extract
			// a storage prefix, skip with no constraint (cannot classify).
			if meta.Node == "" && meta.AZ == "" {
				// Truly empty meta — legacy upgrade path, no constraint.
				continue
			}
			bareCID, _, parseErr := pve.ParseEncodedDiskCID(cid)
			if parseErr != nil {
				continue
			}
			derivedPool, _, parseErr := pve.ParseDiskCID(bareCID)
			if parseErr != nil {
				// Bare CID malformed; cannot classify. Skip — fail closed.
				continue
			}
			pool = derivedPool
		}

		backend, resolveErr := resolver.Resolve(ctx, pool)
		if resolveErr != nil {
			return diskFaultConstraints{}, cpierrors.Wrap(resolveErr,
				"create_vm: fault-domain: cannot resolve backend for disk pool "+pool)
		}

		if backend.Kind() == pve.BackendLocal {
			if meta.Node != "" {
				localNodes[meta.Node] = struct{}{}
			}
		} else {
			// Shared backend: AZ constraint only.
			if meta.AZ != "" {
				if c.requiredAZs == nil {
					c.requiredAZs = make(map[string]struct{})
				}
				c.requiredAZs[meta.AZ] = struct{}{}
			}
		}
	}

	// Validate local-node set: all local disks must share one node.
	if len(localNodes) > 1 {
		nodes := make([]string, 0, len(localNodes))
		for n := range localNodes {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		return diskFaultConstraints{}, cpierrors.Cloud(
			"create_vm: persistent disks are pinned to different local nodes %v — "+
				"local-storage disks cannot span nodes; ensure all persistent disks "+
				"reside on the same PVE node or use shared storage",
			nodes,
		)
	}
	if len(localNodes) == 1 {
		for n := range localNodes {
			c.requiredLocalNode = n
		}
	}

	return c, nil
}

// resolveAZCandidatesValidated looks up the node list for az in the AZ map.
// Called only after pre-validation confirmed all AZ names are known; unknown
// names are not expected here. Returns (nil, true) for the DLB sentinel AZ.
// Returns (nodes, false) for a valid AZ.
func resolveAZCandidatesValidated(az string, cfg *config.CPIConfig, logger *log.Logger) (candidates []string, skipSilently bool) {
	nodes, ok := cfg.AZCandidates(az)
	if ok {
		logger.Debug("create_vm: node selection: AZ candidate set",
			log.String("az", az),
			log.String("candidates", strings.Join(nodes, ",")),
		)
		return nodes, false
	}
	// DLB sentinel: skip scoring for this AZ.
	if az == cfg.DLBAZName() && cfg.DLBAZName() != "" {
		logger.Debug("create_vm: node selection: DLB sentinel AZ — candidates = all online nodes",
			log.String("az", az),
		)
		return nil, true
	}
	// Should not reach here after pre-validation; treat as skip.
	return nil, true
}

// scoreAndPick scores the passed nodes and picks the best. Returns "" when
// pass is empty.
func scoreAndPick(pass []placement.NodeFacts, weights placement.Weights, logger *log.Logger, az string) string {
	if len(pass) == 0 {
		return ""
	}
	scored := placement.Score(pass, weights, nil)
	chosen := placement.Pick(scored, nil)
	if chosen != "" {
		logger.Info("create_vm: node selection: placement scoring chose node",
			log.String("node", chosen),
			log.String("az", az),
		)
	}
	return chosen
}

// logFilterRejections emits a Debug entry for each rejection.
func logFilterRejections(logger *log.Logger, rejections map[string]string, az string) {
	for n, reason := range rejections {
		logger.Debug("create_vm: placement: node filtered",
			log.String("node", n),
			log.String("reason", reason),
			log.String("az", az),
		)
	}
}

// mergeRejections merges src into dst, keeping existing entries (first rejection wins).
func mergeRejections(dst, src map[string]string) {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// classifyFilterResult returns true when all rejection reasons are transient
// (node may come back without operator intervention). Returns true on empty
// rejections (cluster temporarily unreachable → retriable).
// Returns false when any rejection is a permanent misconfiguration.
func classifyFilterResult(rejections map[string]string) (retriable bool) {
	if len(rejections) == 0 {
		return true // no facts = cluster may be temporarily unreachable
	}
	for _, reason := range rejections {
		if !isTransientRejectionReason(reason) {
			return false
		}
	}
	return true
}

// isTransientRejectionReason reports whether a single rejection reason string
// corresponds to a transient condition that may clear without operator action.
//
// "not in candidate node set" is a scope constraint, not a node-health signal.
// It is neutral — it does not indicate a permanent misconfiguration. Returning
// true here ensures it never prevents retriability when other nodes are offline
// (a transient cause). A pure "all nodes outside candidate set" result is still
// retriable because the cluster topology may change (node added to AZ, config
// reload).
func isTransientRejectionReason(reason string) bool {
	switch reason {
	case "node offline", "node in maintenance", "insufficient CPU", "insufficient free memory",
		"not in candidate node set":
		return true
	}
	return false
}

// formatRejections returns a compact human-readable summary of a rejection map.
func formatRejections(rejections map[string]string) string {
	if len(rejections) == 0 {
		return "(no candidates available)"
	}
	parts := make([]string, 0, len(rejections))
	for node, reason := range rejections {
		parts = append(parts, node+": "+reason)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// collectStaticIPsForConflictCheck extracts the bare IP addresses from the
// parsed network specs that carry a static (manual, non-DHCP) assignment.
// Dynamic (type=="dynamic") and VIP networks are skipped.
//
// collectStaticIPsForConflictCheck groups static IPs by their bridge so that
// the caller can call detectIPConflict once per bridge with the correct NIC
// filter, preventing conflicts on any bridge from being silently missed.
//
// Returns a map[bridge][]IP. The default bridge (cloud_properties.network_bridge
// → config.NetworkBridge → "vmbr0") applies to networks that do not specify an
// explicit bridge override. Networks of type dynamic/vip or with empty/DHCP IPs
// are skipped. An empty map means no static IPs were found; callers must check
// len(result) > 0 before calling detectIPConflict.
func collectStaticIPsForConflictCheck(parsed *createVMParsedArgs, cfg *config.CPIConfig) map[string][]string {
	// Resolve the default bridge using the same layered logic as configureNICs.
	// Errors from an unknown vm_type selector are suppressed here: this is a
	// pre-flight check and the main create_vm path will surface the error later.
	defaultBridge, _, _ := resolveVMNICDefaultsWithError(cfg, parsed.cloudProps, parsed.cloudPropsMap)

	result := make(map[string][]string)
	for netName := range parsed.networks {
		spec := parsed.networks[netName]
		switch strings.ToLower(spec.Type) {
		case "manual":
			if spec.IP == "" || strings.EqualFold(spec.IP, "dhcp") {
				continue
			}
			// Use per-network bridge override when present; otherwise the VM default.
			bridge := defaultBridge
			if b, ok := spec.CloudProperties["bridge"].(string); ok && b != "" {
				bridge = b
			}
			result[bridge] = append(result[bridge], spec.IP)
		default:
			// dynamic, vip, "" → skip
		}
	}
	return result
}

// runIPConflictChecks runs the static ipconfig{N} scan (step 5b) and, when
// enabled, the guest-agent active probe (step 5c). Returns nil when
// EnsureNoIPConflictsEnabled is false. The vmid argument is the newly created
// VM so its own ipconfig entries are excluded from conflict detection.
func runIPConflictChecks(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, vmid int) error {
	if !deps.Config.EnsureNoIPConflictsEnabled() {
		return nil
	}

	// 5b. Static ipconfig{N} scan — DHCP/dynamic addresses are not visible here.
	ipsByBridge := collectStaticIPsForConflictCheck(parsed, deps.Config)
	for bridge, ips := range ipsByBridge {
		// Pass vmid as excludeVMID so the newly created VM's own ipconfig
		// entries are not treated as a conflict against itself.
		conflict, conflictErr := detectIPConflict(ctx, deps, ips, bridge, vmid)
		if conflictErr != nil {
			return cpierrors.Wrap(conflictErr, "create_vm: IP-conflict pre-flight")
		}
		if conflict != nil {
			return IPConflictCloudError(conflict, bridge)
		}
	}

	// 5c. Active IP probe via guest agent (opt-in: ip_conflict_probe=agent).
	//
	// Extends the static-config scan with a live fan-out to running VM guest
	// agents, detecting DHCP-assigned and dynamically configured addresses
	// that do not appear in ipconfig{N} keys. Fail-open per guest: an
	// unreachable agent is logged and skipped, never blocking provisioning.
	if deps.Config.ActiveIPProbeEnabled() {
		var allTargetIPs []string
		for _, ips := range ipsByBridge {
			allTargetIPs = append(allTargetIPs, ips...)
		}
		if probeErr := probeGuestAgentIPConflict(ctx, deps, logger, allTargetIPs); probeErr != nil {
			return cpierrors.Wrap(probeErr, "create_vm: active IP probe")
		}
	}
	return nil
}

// lookupVMStorageType fetches the PVE storage type for storageName by listing
// the cluster storage index. Returns "" on any error — callers treat "" as
// linked-clone-capable (permissive). This is intentionally best-effort: the
// create_vm flow must not fail on a storage-lookup error that does not affect
// the import path; the clone-mode decision downstream uses "" → linked safely.
//
// ClusterStorage() == nil (e.g. test mocks that don't wire it) is the expected
// case in unit tests; the function returns "" without logging to keep test
// output clean.
func lookupVMStorageType(ctx context.Context, deps Deps, storageName string) string {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil || storageName == "" {
		return ""
	}
	resp, err := deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil || resp == nil {
		return ""
	}
	for _, raw := range *resp {
		var entry struct {
			Storage string `json:"storage"`
			Type    string `json:"type"`
		}
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			continue
		}
		if entry.Storage == storageName {
			return entry.Type
		}
	}
	return ""
}

// resolveVMIDAllocParams returns the VMID range start and per-create allocation
// retry budget. maxAttempts defaults to 10 so a parallel CF deploy (many
// simultaneous stemcell imports against the same PVE storage) can survive
// transient per-storage lockfile timeouts in addition to VMID races. Each
// lock-timeout retry waits seconds, not ms, so 10 is still bounded.
//
// Attempt-budget precedence (first set wins): retry.storage_import.max_attempts,
// retry.vmid_alloc.max_attempts, vmid_alloc_attempts, then the built-in 10. The
// create_vm allocation loop handles both storage-lock and VMID-conflict retries
// in one budget, and the lock retries dominate (seconds vs ms), so the
// storage_import override is consulted first.
func resolveVMIDAllocParams(cfg *config.CPIConfig) (rangeStart, maxAttempts int) {
	rangeStart = cfg.VMIDRangeStart
	if rangeStart < 100 {
		rangeStart = pve.VMIDRangeVMStart
	}
	switch {
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

// vmStorageTierFn resolves a named storage tier to a concrete pool name.
// Passed as an optional parameter to resolveVMShapeStorage so the function
// can stay testable in the internal package without requiring ctx or Deps.
// Production callers supply a closure over ctx + deps; internal unit tests
// omit the parameter entirely (nil = tier resolution skipped).
type vmStorageTierFn func(tier string) (string, error)

// resolveVMShapeStorage returns the target VM storage, disk format, root disk
// size in GiB, and an error. The resolver checks for storage_pool and
// vm_disk_format through the layered resolver (call cloud_properties →
// disk_type profile → vm_type profile), then falls back to existing config /
// struct-field / default logic. Returns a CloudError if the vm_type or disk_type
// selector names an unknown profile. Behavior is byte-identical to the
// pre-resolver path when no profiles or storage_pool keys are present.
//
// The optional tierFn parameter enables storage_tier resolution. When nil (the
// default for internal tests and callers that do not need tier resolution),
// storage_tier in cloud_properties is silently ignored and the existing
// fallback chain applies: config.VMStorage → stemcell storage.
func resolveVMShapeStorage(cfg *config.CPIConfig, parsed *createVMParsedArgs, tierFn ...vmStorageTierFn) (vmStorage, vmDiskFormat string, rootDiskGiB int, retErr error) {
	cp := parsed.cloudProps

	// Build a layered resolver from the raw cloud_properties map. This resolves
	// vm_type / disk_type selectors and sets up precedence-ordered layers.
	// A nil cloudPropsMap (e.g. old callers / unit tests) is treated as empty.
	r, err := newLayeredResolver(parsed.cloudPropsMap, cfg)
	if err != nil {
		return "", "", 0, err
	}

	// Extract the tier resolver (nil when not provided).
	var resolveTier vmStorageTierFn
	if len(tierFn) > 0 {
		resolveTier = tierFn[0]
	}

	// Storage pool resolution: explicit pool → storage_tier (if tierFn wired) → config → stemcell fallback.
	if pool, ok := r.String("storage_pool"); ok {
		vmStorage = pool
	} else if tier, hasTier := r.String("storage_tier"); hasTier && resolveTier != nil {
		resolved, tierErr := resolveTier(tier)
		if tierErr != nil {
			return "", "", 0, tierErr
		}
		vmStorage = resolved
	} else {
		vmStorage = cfg.VMStorage
		if vmStorage == "" {
			vmStorage = parsed.stemcellStor
		}
	}

	// Disk format: resolver wins (handles both "vm_disk_format" key in call
	// layer and profile layers) → struct field from JSON unmarshal → qcow2.
	// The struct field cp.VMDiskFormat is already populated from args[2] by
	// the standard unmarshal in parseCreateVMArgs, so we only consult it when
	// the resolver finds nothing in any layer.
	if df, ok := r.String("vm_disk_format", "disk_format"); ok {
		vmDiskFormat = df
	} else if cp.VMDiskFormat != "" {
		vmDiskFormat = cp.VMDiskFormat
	} else {
		vmDiskFormat = diskFormatQCOW2
	}

	rootDiskGiB = defaultStemcellDiskGiB
	if cp.Disk > 0 {
		requestedGiB := (cp.Disk + 1023) / 1024
		if requestedGiB > rootDiskGiB {
			rootDiskGiB = requestedGiB
		}
	}
	return vmStorage, vmDiskFormat, rootDiskGiB, nil
}

// resolveVMShapeCPUMem returns cores, sockets, and memory (MiB) honoring two
// cloud_properties conventions: vSphere-style (cpu = total vCPU count) and
// PVE-native (cores/sockets explicit). Explicit cores/sockets win when present;
// otherwise cp.CPU becomes cores with a single socket. Defaults are 1 vCPU and
// 512 MiB.
func resolveVMShapeCPUMem(cp createVMCloudProps) (cores, sockets, memMiB int) {
	cores = cp.Cores
	if cores <= 0 && cp.CPU > 0 {
		cores = cp.CPU
	}
	if cores <= 0 {
		cores = 1
	}
	sockets = cp.Sockets
	if sockets <= 0 {
		sockets = 1
	}
	memMiB = cp.Memory
	if memMiB <= 0 {
		memMiB = cp.RAM
	}
	if memMiB <= 0 {
		memMiB = 512
	}
	return cores, sockets, memMiB
}

// resolveVMShapeHotplugNUMA resolves hotplug + numa using
// cloud_properties → vm_type/disk_type profile → config → built-in default.
// Memory hotplug needs both numa=1 and "memory" in hotplug at create time;
// operators can override per-vm_type for stemcells that misbehave on hot-add.
//
// Hotplug precedence (pointer semantics preserved):
//  1. cp.Hotplug != nil → use *cp.Hotplug (includes explicit "" to disable)
//  2. profile layer via r.String("hotplug") (disk_type then vm_type)
//  3. config.HotplugValue()
//
// NUMA precedence:
//  1. cp.NUMA != nil → use *cp.NUMA (includes explicit false)
//  2. profile layer via r.Bool("numa") (explicit false honored)
//  3. config.NUMAValue()
//
// Panics on resolver error — callers should use resolveVMShapeHotplugNUMAWithError
// when the error must propagate. resolveVMShape uses resolveVMShapeHotplugNUMAWithError
// directly so unknown-selector errors surface as CloudErrors.
func resolveVMShapeHotplugNUMA(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (hotplug string, numaEnabled bool) {
	h, n, err := resolveVMShapeHotplugNUMAWithError(cfg, cp, cpMap)
	if err != nil {
		// Should not reach here: resolveVMShape validates the selector before
		// calling this. Panic makes any regression visible immediately.
		panic("resolveVMShapeHotplugNUMA: unexpected resolver error: " + err.Error())
	}
	return h, n
}

// resolveVMShapeHotplugNUMAWithError is the error-returning variant used by
// resolveVMShape and tests. It returns a CloudError when an unknown vm_type or
// disk_type selector is present in cpMap.
func resolveVMShapeHotplugNUMAWithError(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (hotplug string, numaEnabled bool, err error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", false, err
	}

	// Hotplug: call struct pointer wins (includes explicit "").
	// The call layer IS already in r (cpMap layer 0), but cp.Hotplug is a typed
	// struct pointer — using r.String would drop an explicit "" (empty is skipped
	// by r.String). Keep the struct-pointer check as the authoritative call gate.
	hotplug = cfg.HotplugValue()
	if cp.Hotplug != nil {
		hotplug = *cp.Hotplug
	} else if v, ok := r.String("hotplug"); ok {
		// Profiles only: the call layer's "hotplug" key (if any) was already
		// covered by cp.Hotplug above — this branch reads disk_type/vm_type layers.
		// r.String skips empty strings, so a profile "" is also treated as absent
		// (consistent with explicit-value semantics; only cp.Hotplug carries the
		// disable-via-empty-string meaning).
		hotplug = v
	}

	// NUMA: call struct pointer wins (includes explicit false).
	numaEnabled = cfg.NUMAValue()
	if cp.NUMA != nil {
		numaEnabled = *cp.NUMA
	} else if b, ok := r.Bool("numa"); ok {
		numaEnabled = b
	}

	return hotplug, numaEnabled, nil
}

// resolveVMShapeInitialName composes the initial PVE VM name from env.bosh
// fields + Config so the PVE UI shows deployment + instance-group immediately
// on come-online instead of the placeholder "vm-<vmid>". Director-mode deploys
// carry director + deployment + job in env.bosh.group; `bosh create-env` paths
// have no deployment, so Config.CreateEnvDeployment (default "create-env")
// fills that segment. set_vm_metadata later refines this to
// "<prefix>-<deployment>-<job>-<index>" once the index is known.
func resolveVMShapeInitialName(cfg *config.CPIConfig, parsed *createVMParsedArgs) string {
	initialJobName := extractJobNameFromEnv(parsed.env)
	initialDeployment := extractDeploymentFromEnv(parsed.env, initialJobName)
	if initialDeployment == "" {
		initialDeployment = cfg.CreateEnvDeployment
	}
	if initialJobName == "" {
		// create-env path: env has no group/groups. Fall back to the BOSH
		// instance-group baked into cloud_provider.template.name when it is
		// detectable from env.bosh.instance.name; otherwise leave blank and
		// let the "vm-<vmid>" placeholder stand.
		initialJobName = extractInstanceNameFromEnv(parsed.env)
	}
	return composeVMName(cfg.VMPrefix, initialDeployment, initialJobName, "")
}

// allocateVM runs AllocateWithRetry: picks a free VMID, calls QEMU.Create,
// awaits the import task. On conflict or transient errors it retries up to
// shape.maxAttempts times. Returns the winning vmid.
func allocateVM(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
) (int, error) {
	isRetryable := func(e error) bool {
		// A missing clone source (stemcell template removed out-of-band) is
		// permanent: it surfaces as a 5xx that IsTransientTransport would match,
		// but retrying with a fresh VMID cannot help. Short-circuit so the real
		// cause propagates instead of "exhausted VMID allocation".
		if pve.IsCloneSourceMissing(e) {
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
		pve.WithBackoffFunc(newCreateVMRetryBackoff(
			deps.Config.RetryStorageImport(), deps.Config.RetryVMIDAlloc())),
	)
	return vmid, err
}

// extractSHA8FromFilename extracts the 8-hex-char content sha from a stemcell
// qcow2 filename produced by BuildStemcellFilename.
//
// Format: bosh-stemcell-<name>-<version>-<sha8>.qcow2
// The sha8 is the last "-"-delimited segment before ".qcow2". Because <name>
// and <version> themselves may contain hyphens, the function takes the segment
// between the final "-" and the ".qcow2" suffix rather than splitting on the
// third hyphen.
//
// Returns ("", false) when:
//   - filename does not end with ".qcow2"
//   - there is no "-" before ".qcow2"
//   - the candidate sha8 is not exactly 8 ASCII hex characters
//
// The caller treats ("", false) as "skip lookup, fall back to import-from".
func extractSHA8FromFilename(filename string) (sha8 string, ok bool) {
	const suffix = ".qcow2"
	if !strings.HasSuffix(filename, suffix) {
		return "", false
	}
	base := filename[:len(filename)-len(suffix)]
	lastDash := strings.LastIndexByte(base, '-')
	if lastDash < 0 {
		return "", false
	}
	candidate := base[lastDash+1:]
	if len(candidate) != 8 {
		return "", false
	}
	for _, c := range candidate {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return strings.ToLower(candidate), true
}

// extractSHA8FromFilenameInCID extracts the sha8 from the filename embedded in
// a raw stemcell CID of the form "<storage>:import/<filename>".
//
// It calls ParseStemcellCID to get the volumePath ("import/<filename>"),
// strips the "import/" prefix to obtain the bare filename, then delegates to
// extractSHA8FromFilename. Returns ("", false) on any parse error or when the
// filename does not match the expected pattern — callers skip the lookup.
func extractSHA8FromFilenameInCID(rawCID string) (sha8 string, ok bool) {
	_, volumePath, err := pve.ParseStemcellCID(rawCID)
	if err != nil {
		return "", false
	}
	const importPrefix = "import/"
	if !strings.HasPrefix(volumePath, importPrefix) {
		return "", false
	}
	filename := volumePath[len(importPrefix):]
	return extractSHA8FromFilename(filename)
}

// needsReplicaCheck reports whether the template-gap guard should run for the
// given vmStorage. The guard is needed when storage is local (not shared across
// the cluster): on a multi-node cluster, a local-storage template is only
// accessible on the node that holds it. When storage information is unavailable
// (lookup error or nil PVE client), returns false (fail-open: skip guard).
func needsReplicaCheck(ctx context.Context, deps Deps, vmStorage string) bool {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil || vmStorage == "" {
		return false
	}
	resp, err := deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil || resp == nil {
		return false
	}
	for _, raw := range *resp {
		var entry struct {
			Storage string `json:"storage"`
			Type    string `json:"type"`
			Shared  int    `json:"shared"` // PVE integer bool: 1 = shared
		}
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			continue
		}
		if entry.Storage != vmStorage {
			continue
		}
		// Shared storage: no guard needed (template accessible from any node).
		if entry.Shared == 1 {
			return false
		}
		// Local storage: guard needed.
		return true
	}
	// Storage not found in index: fail-open (skip guard).
	return false
}

// extractSHA8FromTemplateCIDContext extracts the sha8 digest from the parsed
// args when the stemcell CID is a template CID. Template CIDs carry no filename
// (they are "template:<vmid>"), so the sha8 must come from a previous lookup
// context. For now we use the raw CID filename when present (old-form CIDs
// carry sha8 in the filename). Returns ("", false) when the sha8 cannot be
// determined — the caller skips the replica lookup in that case.
func extractSHA8FromTemplateCIDContext(parsed *createVMParsedArgs) (sha8 string, ok bool) {
	// Template CIDs (template:<vmid>) do not embed a sha8. The sha8 is available
	// from the raw CID if the operator is using an old-form CID at the same time
	// (not the case for pure template CIDs). Return not-found so the guard is
	// skipped for pure template CIDs without a raw CID fallback.
	if parsed.rawCID == "" {
		return "", false
	}
	return extractSHA8FromFilenameInCID(parsed.rawCID)
}

// attemptCreateVM builds the create params for one VMID candidate, then either
// clones from a template (when stemcellCID is a "template:<vmid>" CID) or calls
// QEMU.Create with import-from= (old-form CID). On retryable failures it logs
// and optionally cleans up the candidate VMID before returning the error so
// AllocateWithRetry can retry with a fresh candidate.
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

	// --- Template-clone path ---
	if pve.IsTemplateStemcellCID(parsed.stemcellCID) {
		templateVMID, err := pve.ParseTemplateStemcellCID(parsed.stemcellCID)
		if err != nil {
			// Should never happen: parsing was validated in parseCreateVMArgs.
			return cpierrors.Wrap(err, "create_vm: parse template CID")
		}

		// Compute the node that hosts the template. StemcellTemplateNode, when
		// set, is where create_stemcell built the template; fall back to the
		// general config.Node. The clone task is submitted to templateNode;
		// Target= redirects the resulting VM to shape.node (cross-node shared).
		templateNode := deps.Config.StemcellTemplateNode
		if templateNode == "" {
			templateNode = deps.Config.Node
		}

		// Template-gap guard: when the chosen VM node differs from the template
		// node and the stemcell storage is local (not shared), the template is
		// not accessible from shape.node. Check for a per-node replica first;
		// if none exists, return a clear actionable error instead of letting the
		// clone task fail with an opaque PVE error.
		effectiveTemplateVMID := templateVMID
		effectiveTemplateNode := templateNode
		if shape.node != "" && shape.node != templateNode {
			if needsReplicaCheck(ctx, deps, shape.vmStorage) {
				// Extract sha8 from the stemcell CID to look up a replica.
				sha8, hasSHA := extractSHA8FromTemplateCIDContext(parsed)
				if hasSHA && sha8 != "" {
					replicaVMID, found, lookupErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, shape.node, sha8)
					switch {
					case lookupErr != nil:
						logger.Warn("create_vm: template replica lookup failed (continuing with primary)",
							log.String("node", shape.node),
							log.String("sha8", sha8),
							log.Err(lookupErr),
						)
					case found:
						logger.Info("create_vm: using per-node template replica",
							log.String("node", shape.node),
							log.Int("replica_vmid", replicaVMID),
							log.String("sha8", sha8),
						)
						effectiveTemplateVMID = int64(replicaVMID)
						effectiveTemplateNode = shape.node
					case !deps.Config.StemcellReplicateLocal:
						// No replica found and replication is disabled — fail fast with
						// an actionable message rather than letting the clone fail opaquely.
						return cpierrors.Cloud(
							"create_vm: stemcell template (vmid=%d sha8=%s) is not present on node %q "+
								"and stemcell_replicate_local is disabled; "+
								"either enable stemcell_replicate_local to allow per-node replication, "+
								"or use shared storage for the stemcell pool (%s)",
							templateVMID, sha8, shape.node, shape.vmStorage,
						)
					}
				}
			}
		}

		cloneErr := cloneFromTemplate(ctx, deps, logger, shape, candidate, candidateName, effectiveTemplateNode, effectiveTemplateVMID)
		if cloneErr != nil {
			// Classify for retry: VMID conflicts and transient transport faults are
			// retryable — they use the same retry classification as the import path.
			return handleCloneError(ctx, deps, logger, shape.node, candidate, cloneErr)
		}

		logger.Info("create_vm: vm cloned from template",
			log.Int("vmid_attempted", candidate),
			log.Int64("template_vmid", effectiveTemplateVMID),
			log.String("template_node", effectiveTemplateNode),
		)
		return nil
	}

	// --- Old-form CID: opportunistic template lookup before import-from ---
	//
	// If an existing template carries a matching sha tag, clone it
	// (fast path). If not found or lookup fails, fall through to import-from
	// (slow but correct). create_vm NEVER builds a template here — read-only
	// lookup only.
	//
	// The sha8 lives in the filename: bosh-stemcell-<name>-<version>-<sha8>.qcow2.
	// We extract it from the last "-"-separated segment before ".qcow2". If the
	// filename does not match that pattern (pre-upgrade or custom stems), skip
	// lookup silently.
	if sha8, ok := extractSHA8FromFilenameInCID(parsed.rawCID); ok {
		templateNode := deps.Config.StemcellTemplateNode
		if templateNode == "" {
			templateNode = deps.Config.Node
		}
		templateVMID, found, lookupErr := pve.FindTemplateBySHATag(ctx, deps.PVE, templateNode, sha8)
		if lookupErr != nil {
			// Lookup failure is non-fatal: log and fall through to import-from.
			// Do NOT fail create_vm on a read-only lookup error — the safe path
			// (import-from) is always available.
			logger.Warn("create_vm: template SHA lookup failed, falling back to import-from",
				log.String("sha8", sha8),
				log.String("template_node", templateNode),
				log.Err(lookupErr),
			)
		} else if found {
			// A pre-built template matches this stemcell content — clone it.
			logger.Info("create_vm: opportunistic template found by sha tag, cloning",
				log.String("sha8", sha8),
				log.Int64("template_vmid", templateVMID),
				log.String("template_node", templateNode),
			)
			cloneErr := cloneFromTemplate(ctx, deps, logger, shape, candidate, candidateName, templateNode, templateVMID)
			if cloneErr != nil {
				return handleCloneError(ctx, deps, logger, shape.node, candidate, cloneErr)
			}
			logger.Info("create_vm: vm cloned from opportunistic template",
				log.Int("vmid_attempted", candidate),
				log.Int64("template_vmid", templateVMID),
				log.String("template_node", templateNode),
			)
			return nil
		}
		// !found → fall through to import-from below.
	}

	// --- Import-from path (old-form CID: light: or plain <storage>:import/<file>) ---
	virtio0Val := fmt.Sprintf("%s:0,import-from=%s,format=%s,size=%dG",
		shape.vmStorage, parsed.rawCID, shape.vmDiskFormat, shape.rootDiskGiB)

	createParams := map[string]any{
		metadataKeyVMID: candidate,
		metadataKeyName: candidateName,
		"memory":        shape.memMiB,
		"cores":         shape.cores,
		"ostype":        osTypeLinux26,
		"scsihw":        "virtio-scsi-pci",
		"virtio0":       virtio0Val,
		"boot":          "order=virtio0",
		"agent":         "enabled=1",
		"hotplug":       shape.hotplug,
		"onboot":        0,
	}
	if shape.numaEnabled {
		createParams["numa"] = 1
	}
	if shape.sockets > 1 {
		createParams["sockets"] = shape.sockets
	}
	if shape.initialTags != "" {
		createParams["tags"] = shape.initialTags
	}

	upid, cerr := deps.PVE.QEMU().Create(ctx, shape.node, createParams)
	if cerr != nil {
		return handleCreateError(ctx, deps, logger, shape.node, candidate, cerr)
	}

	if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, upid, logger,
		pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
		return handleAwaitError(ctx, deps, logger, shape.node, candidate, werr)
	}

	logger.Info("create_vm: vm disk imported",
		log.Int("vmid_attempted", candidate),
		log.String("upid", upid),
	)
	return nil
}

// handleCloneError classifies a cloneFromTemplate error and logs appropriately.
// VMID conflicts and transient transport faults are retryable (same semantics as
// handleCreateError). Storage-lock timeouts from the clone task are also retried.
// Local-storage cross-node violations are NOT retryable — they are
// configuration errors that must propagate immediately.
func handleCloneError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	cerr error,
) error {
	switch {
	case pve.IsCloneSourceMissing(cerr):
		// Permanent: the clone source template VM is gone on the node (stemcell
		// template removed out-of-band). Retrying a fresh VMID cannot help —
		// surface the real cause instead of "exhausted VMID allocation".
		logger.Error("create_vm: clone source template missing, not retrying",
			log.Int("vmid_attempted", candidate),
			log.String("error", cerr.Error()),
		)
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
	case pve.IsVMIDConflict(cerr):
		logger.Info("create_vm: vmid conflict on clone, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsStorageLockTimeout(cerr):
		logger.Info("create_vm: storage lock timeout on clone, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsTransientTransport(cerr):
		// Clone POST may or may not have committed — sweep the candidate VMID
		// before retrying so the cluster list is clean.
		logger.Info("create_vm: transient transport fault on clone, retrying",
			log.Int("vmid_attempted", candidate),
			log.String("error", cerr.Error()),
		)
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
	default:
		// Non-retryable error (e.g. local-storage cross-node violation,
		// template not found, or other PVE fatal). Clean up any partial VM
		// state and propagate — AllocateWithRetry will not retry.
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
	}
	return cerr
}

// cloneFromTemplate clones templateVMID (on templateNode) into the candidate
// VMID, selecting linked vs full clone per clone_mode config and the storage
// backend capability reported by IsLinkedCloneSupported. It also enforces the
// cross-node placement policy via ValidateTemplateCloneStorage and sets
// params.Target when the template node differs from the desired VM node and
// the storage is confirmed shared.
//
// Clone-mode selection:
//   - "linked" — forced linked; error if storage does not support it.
//   - "full"   — forced full; Storage and Format are set on params.
//   - "auto"/"" — linked when supported, full otherwise.
//
// Storage and Format are only set on full-clone params (SDK requirement).
// Target is set only for cross-node clones on shared storage. Cross-node
// clones on local storage return an actionable error.
//
// The returned upid identifies the async PVE clone task; the caller must await
// it. An empty upid means PVE completed synchronously.
func cloneFromTemplate(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	candidate int,
	candidateName string,
	templateNode string,
	templateVMID int64,
) error {
	// Clone mode: call cloud_properties.clone_mode → vm_type profile → config → "auto".
	// shape carries the resolved cloudPropsMap from the parsed args.
	mode, err := resolveCloneMode(deps.Config, shape.cloudPropsMap)
	if err != nil {
		return err
	}

	linkedOK := pve.IsLinkedCloneSupported(shape.vmStorageType)

	var full *bool
	switch mode {
	case "linked":
		if !linkedOK {
			return cpierrors.Cloud(
				"create_vm: clone_mode=linked but storage %q (type %q) does not support linked clones;"+
					" use clone_mode=auto or clone_mode=full, or switch to a snapshot-capable storage backend",
				shape.vmStorage, shape.vmStorageType,
			)
		}
		// full remains nil → linked clone.
	case "full":
		t := true
		full = &t
	default: // "auto"
		if !linkedOK {
			t := true
			full = &t
		} else if shape.vmStorageType == "" {
			// Storage type lookup failed or returned empty; IsLinkedCloneSupported
			// treats unknown type as linked-capable (permissive default). Log at
			// debug so a PVE rejection of a linked clone is diagnosable even when
			// the storage type could not be determined at clone time.
			logger.Debug("create_vm: clone_mode=auto: storage type unknown, assuming linked-clone support",
				log.String("vm_storage", shape.vmStorage),
			)
		}
		// Otherwise full remains nil → linked clone.
	}

	newid := int64(candidate)
	params := &sdknodes.CreateQemuCloneParams{
		Newid: newid,
		Name:  &candidateName,
		Full:  full,
	}

	// Set Storage and Format only for full clones. The SDK validates that
	// these fields are absent on linked clones; setting them on a nil-Full
	// (linked) request triggers a PVE API error.
	if full != nil && *full {
		params.Storage = &shape.vmStorage
		params.Format = &shape.vmDiskFormat
	}

	// Cross-node Target= enforcement.
	//
	// The clone task is submitted to templateNode. When templateNode != shape.node
	// (BOSH's desired VM node), PVE must move the resulting VM to shape.node.
	// PVE supports cross-node placement via params.Target ONLY on shared storage.
	// Local storage (dir, lvm, lvmthin, zfspool) cannot cross nodes — PVE rejects
	// Target on local-storage clones with a hard error.
	//
	// Topology matrix:
	//   single-node (≤1)          → accept any storage; templateNode==shape.node; no Target.
	//   multi-node + shared       → accept; set Target when templateNode != shape.node.
	//   multi-node + local + pin  → operator must pin to the template node; if
	//       shape.node != templateNode the configuration is wrong — return error.
	//   multi-node + local + no pin → ValidateTemplateCloneStorage rejects (rule 4).
	policyDeps := newHandlerPolicyDeps(deps)
	_, policyErr := pve.ValidateTemplateCloneStorage(ctx, policyDeps, shape.vmStorage, shape.node)
	if policyErr != nil {
		return policyErr
	}

	// After policy validation, enforce the local-storage cross-node constraint:
	// if templateNode and shape.node differ AND storage is not shared, PVE cannot
	// clone across nodes — the operator must fix this by
	// pinning the VM node to match the template node or switching to shared storage.
	if templateNode != shape.node {
		storageInfo, infoErr := policyDeps.StorageInfo(ctx, shape.vmStorage)
		if infoErr != nil {
			return cpierrors.Wrap(infoErr,
				"create_vm: cross-node clone: cannot look up storage "+shape.vmStorage+" to determine if Target is safe")
		}
		if !storageInfo.IsShared() {
			return cpierrors.Cloud(
				"create_vm: cross-node clone rejected: template is on node %q but VM is targeted to node %q;"+
					" storage %q is local (not shared) — PVE cannot cross-node clone local storage;"+
					" set cloud_properties.node to match the template node (%q),"+
					" or use shared storage",
				templateNode, shape.node, shape.vmStorage, templateNode)
		}
		// Shared storage confirmed: set Target so PVE lands the clone on shape.node.
		targetNode := shape.node
		params.Target = &targetNode
	}

	logger.Info("create_vm: cloning template",
		log.Int("template_vmid", int(templateVMID)),
		log.String("template_node", templateNode),
		log.Int("new_vmid", candidate),
		log.String("clone_mode", mode),
		log.Bool("full_clone", full != nil && *full),
	)

	upid, cloneErr := pve.CloneQemuVM(ctx, deps.PVE, templateNode, templateVMID, params)
	if cloneErr != nil {
		return cpierrors.Wrap(cloneErr, fmt.Sprintf(
			"create_vm: clone template vmid=%d → new vmid=%d", templateVMID, candidate))
	}

	if upid != "" {
		if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, templateNode, upid, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
			return cpierrors.Wrap(werr, fmt.Sprintf(
				"create_vm: await clone task template vmid=%d → new vmid=%d", templateVMID, candidate))
		}
	}

	logger.Info("create_vm: template clone complete",
		log.Int("template_vmid", int(templateVMID)),
		log.Int("new_vmid", candidate),
	)

	// The clone inherits the template's minimal resources (templates are created
	// with PVE defaults: 512 MiB RAM, 1 core). Apply the requested CPU/memory
	// shape to the cloned VM — the import-from path sets these in CreateQemuParams
	// at create time, but a clone must set them explicitly or the VM boots
	// undersized (e.g. a 512 MiB director that never reaches "running").
	//
	// Also re-enable the QEMU guest agent channel. The stemcell template is
	// created with agent=enabled=0 (a frozen template needs no agent), and a
	// clone inherits that. Without overriding it here every cloned VM has the
	// agent channel disabled, so `qm guest exec`/QGA cannot reach the guest —
	// which removes the only out-of-band path to a VM whose bosh-agent has
	// wedged (the import-from path sets agent=enabled=1 at create time).
	memStr := strconv.Itoa(shape.memMiB)
	cores64 := int64(shape.cores)
	sockets64 := int64(shape.sockets)
	agentEnabled := "enabled=1"
	resourceParams := &sdknodes.UpdateQemuConfigParams{
		Memory:  &memStr,
		Cores:   &cores64,
		Sockets: &sockets64,
		Agent:   &agentEnabled,
	}
	if cfgErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, shape.node, strconv.Itoa(candidate), resourceParams); cfgErr != nil {
		return cpierrors.Wrap(pve.WrapError(cfgErr), fmt.Sprintf(
			"create_vm: apply cpu/memory to cloned vmid=%d: %s", candidate, cfgErr.Error()))
	}
	logger.Info("create_vm: applied cpu/memory to cloned vm",
		log.Int("new_vmid", candidate),
		log.Int("cores", shape.cores),
		log.Int("sockets", shape.sockets),
		log.Int("memory_mib", shape.memMiB),
	)

	return nil
}

// handleCreateError classifies a QEMU.Create error and logs the appropriate
// message. It cleans up transient-transport failures (where the POST may have
// committed) and returns the original error so AllocateWithRetry can retry.
func handleCreateError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	cerr error,
) error {
	// Classify mutually exclusively so VMID conflicts don't
	// trigger the transient-transport cleanup path: a "VM N
	// already exists" 500 satisfies both predicates, but
	// destroying the conflicting VMID would wipe another
	// process's in-flight VM.
	switch {
	case pve.IsVMIDConflict(cerr):
		logger.Info("create_vm: vmid conflict, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsStorageLockTimeout(cerr):
		logger.Info("create_vm: storage lock timeout on create, retrying",
			log.Int("vmid_attempted", candidate),
		)
	case pve.IsTransientTransport(cerr):
		// HTTP 596 or auth-EOF: pvedaemon worker cycled
		// mid-request. POST may or may not have committed
		// the VM. Sweep this VMID before the next attempt
		// so the cluster list is clean.
		logger.Info("create_vm: transient transport fault on create, retrying",
			log.Int("vmid_attempted", candidate),
			log.String("error", cerr.Error()),
		)
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
	}
	return cerr
}

// handleAwaitError classifies an AwaitTask error after QEMU.Create succeeded.
// It cleans up partially registered VMIDs for retriable errors and returns the
// original error so AllocateWithRetry can retry.
func handleAwaitError(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	werr error,
) error {
	if pve.IsVMIDConflict(werr) {
		logger.Info("create_vm: vmid conflict on await, retrying",
			log.Int("vmid_attempted", candidate),
		)
		return werr
	}
	if pve.IsStorageLockTimeout(werr) {
		logger.Info("create_vm: storage lock timeout on import, retrying",
			log.Int("vmid_attempted", candidate),
		)
		// PVE rolled back its own qmcreate task — but the
		// VMID may still be registered with the partial
		// state. Clean up before the next attempt.
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
		return werr
	}
	if pve.IsTransientTransport(werr) {
		logger.Info("create_vm: transient transport fault on await, retrying",
			log.Int("vmid_attempted", candidate),
			log.String("error", werr.Error()),
		)
		// The qmcreate task itself may still be running on
		// PVE — we only lost the await connection. Clean
		// up the VMID so a fresh attempt has a clean slate.
		cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
		return werr
	}
	// Non-conflict failure after Create succeeded: the VM may
	// have been partially registered. Roll back this attempt
	// before propagating so the next retry (which won't run)
	// or the caller sees a clean slate.
	cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
	return werr
}

// resizeRootDisk grows virtio0 by the delta between rootDiskGiB and the
// imported stemcell base size. It is a no-op when no growth is needed.
//
// PVE silently ignores the `size=<N>G` directive on the import-from
// scsi/virtio param when the source image is smaller than N — the new
// volume keeps the source image's size (~5 GiB for BOSH stemcells).
// Without an explicit resize, the BOSH agent's bootstrap fails at
// "Setting up ephemeral disk: Insufficient remaining disk space"
// (CreatePartitionIfNoEphemeralDisk=true in the stemcell's agent.json
// requires free space at the end of the root disk).
func resizeRootDisk(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	shape *createVMShape,
	vmid int,
) error {
	growGiB := shape.rootDiskGiB - defaultStemcellDiskGiB
	if growGiB <= 0 {
		return nil
	}
	// PVE's `qm resize` runs `qemu-img resize` under the per-storage
	// lockfile (/var/lock/pve-manager/pve-storage-<name>). Under a
	// concurrent CF deploy this contends with parallel stemcell imports
	// and other resizes and surfaces as "can't lock file ... got timeout"
	// in the task log. Retry the whole submit+await with seconds-scale
	// backoff against the lock holder finishing.
	rerr := pve.RetryOnTransientOrLock(ctx, logger, "resize_virtio0", shape.maxAttempts, func() error {
		upid, e := deps.PVE.QEMU().ResizeDisk(ctx, shape.node, vmid, "virtio0", growGiB)
		if e != nil {
			return e
		}
		if upid == "" {
			return nil
		}
		return pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, upid, logger)
	})
	if rerr != nil {
		// Route through WrapError so task-level transients (LVM command
		// timeouts under VG contention, pmxcfs sync races where the just-
		// created conf is briefly absent) surface as RetriableCloudError
		// — director re-issues create_vm with a fresh VMID instead of
		// failing the deploy.
		return cpierrors.Wrap(pve.WrapError(rerr),
			fmt.Sprintf("create_vm: resize virtio0 vmid=%d +%dG", vmid, growGiB))
	}
	logger.Info("create_vm: grew virtio0",
		log.Int(metadataKeyVMID, vmid),
		log.Int("delta_gib", growGiB),
		log.Int("final_gib", shape.rootDiskGiB),
	)
	return nil
}

// resolveVMNICDefaults resolves the VM-level NIC bridge and model defaults using
// the layered resolver. Precedence for bridge:
//  1. cp.NetworkBridge (call struct field, non-empty wins)
//  2. profile layers via r.String("network_bridge")
//  3. config.NetworkBridge
//  4. defaultNetworkBridge ("vmbr0")
//
// Precedence for model:
//  1. cp.NetworkModel (call struct field, non-empty wins)
//  2. profile layers via r.String("network_model")
//  3. built-in default "virtio"
//
// Per-NIC spec.CloudProperties["bridge"] / ["model"] overrides sit above these
// VM-level defaults and are applied in configureNICs after this call.
//
// Panics on resolver error — callers requiring error propagation use
// resolveVMNICDefaultsWithError.
func resolveVMNICDefaults(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (bridge, model string) {
	b, m, err := resolveVMNICDefaultsWithError(cfg, cp, cpMap)
	if err != nil {
		panic("resolveVMNICDefaults: unexpected resolver error: " + err.Error())
	}
	return b, m
}

// resolveVMNICDefaultsWithError is the error-returning variant of resolveVMNICDefaults.
// Returns a CloudError when cpMap contains an unknown vm_type or disk_type selector.
func resolveVMNICDefaultsWithError(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (bridge, model string, err error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", "", err
	}

	// Bridge resolution: call struct field (non-empty) → profile → config → constant.
	bridge = cfg.NetworkBridge
	if bridge == "" {
		bridge = defaultNetworkBridge
	}
	if cp.NetworkBridge != "" {
		bridge = cp.NetworkBridge
	} else if v, ok := r.String("network_bridge"); ok {
		// Profile layers only; call layer is already covered by cp.NetworkBridge above.
		// r.String reads all layers in order — call layer first — but since we only
		// land here when cp.NetworkBridge is empty, any non-empty "network_bridge" in
		// the call map would be redundant with the struct field. Profile wins over
		// config when the struct field is empty.
		bridge = v
	}

	// Model resolution: call struct field (non-empty) → profile → built-in "virtio".
	model = "virtio"
	if cp.NetworkModel != "" {
		model = cp.NetworkModel
	} else if v, ok := r.String("network_model"); ok {
		model = v
	}

	return bridge, model, nil
}

// resolveCloneMode returns the effective clone_mode by consulting the layered
// resolver (call cloud_properties → profile layers) then falling back to
// config.CloneMode. An empty config.CloneMode defaults to "auto".
// Returns a CloudError when cpMap contains an unknown vm_type or disk_type selector.
func resolveCloneMode(cfg *config.CPIConfig, cpMap map[string]any) (string, error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", err
	}
	if v, ok := r.String("clone_mode"); ok {
		return v, nil
	}
	mode := cfg.CloneMode
	if mode == "" {
		mode = "auto"
	}
	return mode, nil
}

// configureNICs builds and applies the NIC configuration for the new VM from
// the networks map. Returns the ordered list of network names (used later for
// MAC extraction) and any error.
func configureNICs(
	ctx context.Context,
	deps Deps,
	_ *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
) ([]string, error) {
	// Build an ordered list of network names for deterministic NIC assignment.
	netNames := sortedNetworkNames(parsed.networks)

	// VM-level bridge and model defaults via layered resolver.
	// Per-NIC spec.CloudProperties["bridge"]/["model"] overrides are applied below.
	defaultBridge, defaultModel, err := resolveVMNICDefaultsWithError(deps.Config, parsed.cloudProps, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}

	// Build net map[int]string and ipconfig map[int]string for UpdateQemuConfigParams
	netMap := make(map[int]string, len(netNames))
	ipconfigMap := make(map[int]string, len(netNames))
	var nameservers []string
	firstNS := true

	for i, name := range netNames {
		spec := parsed.networks[name]

		// NIC bridge from cloud_properties within the network spec
		bridge := defaultBridge
		if cp, ok := spec.CloudProperties["bridge"].(string); ok && cp != "" {
			bridge = cp
		}
		model := defaultModel
		if cp, ok := spec.CloudProperties["model"].(string); ok && cp != "" {
			model = cp
		}

		// net0 = "virtio,bridge=vmbr0" (no MAC — PVE assigns one)
		netMap[i] = fmt.Sprintf("%s,bridge=%s", model, bridge)

		// Per-NIC firewall flag. The network cloud_property "firewall" (bool)
		// overrides the global cfg.VMFirewallEnabled() default for this NIC.
		// firewall=1 on the NIC alone does not activate filtering — the
		// VM-level firewall must also be enabled (see applySecurityGroups).
		nicFirewall := deps.Config.VMFirewallEnabled()
		if cp, ok := spec.CloudProperties["firewall"].(bool); ok {
			nicFirewall = cp
		}
		if nicFirewall {
			netMap[i] += ",firewall=1"
		}

		// ipconfig: dynamic → dhcp; manual → ip=<cidr>,gw=<gw>
		switch strings.ToLower(spec.Type) {
		case "dynamic", "":
			ipconfigMap[i] = "ip=dhcp"
		case "manual":
			if spec.IP != "" {
				cidr := ipToCIDR(spec.IP, spec.Netmask)
				cfg := "ip=" + cidr
				if spec.Gateway != "" {
					cfg += ",gw=" + spec.Gateway
				}
				ipconfigMap[i] = cfg
			} else {
				ipconfigMap[i] = "ip=dhcp"
			}
		case "vip":
			// VIP networks are routing-level, no ipconfig needed
		}

		// Collect DNS servers from all specs (first spec's DNS takes precedence)
		if firstNS && len(spec.DNS) > 0 {
			nameservers = spec.DNS
			firstNS = false
		}
	}

	nicParams := &sdknodes.UpdateQemuConfigParams{
		Net:      netMap,
		Ipconfig: ipconfigMap,
	}
	if len(nameservers) > 0 {
		ns := strings.Join(nameservers, " ")
		nicParams.Nameserver = &ns
	}

	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, shape.node, strconv.Itoa(vmid), nicParams); err != nil {
		return nil, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("create_vm: configure NICs vmid=%d: %s", vmid, err.Error()))
	}

	return netNames, nil
}

// attachPersistentDisks attaches each disk CID in parsed.diskCIDs to the VM
// on the scsi bus.
func attachPersistentDisks(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
) error {
	for _, diskCID := range parsed.diskCIDs {
		if diskCID == "" {
			continue
		}
		// PVE disk config values are the canonical "<storage>:<volname>"
		// form (e.g. "data:vm-9003-disk-0"). Strip any encoded metadata suffix
		// before passing to AttachDisk; PVE rejects non-volid suffixes with
		// "scsi0.file: invalid format - unable to parse volume ID ...".
		bareDiskCID, _, decErr := pve.ParseEncodedDiskCID(diskCID)
		if decErr != nil {
			return cpierrors.Cloud("create_vm: parse disk_cid %q: %s", diskCID, decErr.Error())
		}
		if _, _, parseErr := pve.ParseDiskCID(bareDiskCID); parseErr != nil {
			return cpierrors.Cloud("create_vm: parse disk_cid %q: %s", diskCID, parseErr.Error())
		}
		diskID, err := deps.PVE.QEMU().AttachDisk(ctx, shape.node, vmid, bareDiskCID, "scsi", nil)
		if err != nil {
			return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("create_vm: attach disk %q to vmid=%d: %s", diskCID, vmid, err.Error()))
		}
		logger.Info("create_vm: attached persistent disk",
			log.Int(metadataKeyVMID, vmid),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
		)
	}
	return nil
}

// configureAgent builds the agent.AgentConfig and calls deps.Agent.Configure.
func configureAgent(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
	vmName string,
) error {
	agentNetworks := buildAgentNetworks(parsed.networks)
	mbus, blobstore := extractMBusAndBlobstore(parsed.env)
	if bsRaw, ok := parsed.env["blobstore"].(map[string]any); ok && len(bsRaw) > 0 && blobstore.Provider == "" {
		logger.Warn("create_vm: env.blobstore.provider type assertion failed; configuring agent without blobstore",
			log.String("vm", vmName))
	}
	// In the modern (NATS-mTLS) BOSH director path the director-side
	// agent_settings env carries env.bosh.mbus.cert but NOT env.bosh.mbus.url
	// — the URL has to come from the CPI's job-level `properties.agent.mbus`
	// config (matches the pattern bosh-deployment uses for other CPIs, e.g.
	// virtualbox_cpi in misc/ipv6/bosh.yml). The same fallback handles
	// create-env (bosh-init), where env carries no mbus at all and the URL
	// lives in cloud_provider.properties.agent.mbus.
	if mbus == "" {
		mbus = deps.Config.AgentMBus
	}
	if blobstore.Provider == "" && len(deps.Config.AgentBlobstore) > 0 {
		if p, ok := deps.Config.AgentBlobstore["provider"].(string); ok {
			blobstore.Provider = p
		}
		if opts, ok := deps.Config.AgentBlobstore["options"].(map[string]any); ok {
			blobstore.Options = opts
		}
		if blobstore.Options == nil {
			blobstore.Options = map[string]any{}
		}
	}

	agentCfg := agent.AgentConfig{
		AgentID:  parsed.agentID,
		Networks: agentNetworks,
		Disks: agent.DisksSpec{
			// "/dev/sda" is the *form* the agent's mappedDevicePathResolver
			// expects: it strips the "/dev/sd" prefix and tries "/dev/xvd",
			// "/dev/vd", "/dev/sd" in turn, so a virtio0 root disk is found
			// as /dev/vda even though our PVE config never exposes /dev/sda.
			// A numeric index like "0" would route to idDevicePathResolver,
			// which globs /dev/disk/by-id/*0 — that file does not exist
			// unless we also set a matching `serial=` on the PVE disk.
			System: "/dev/sda",
			// Leave Ephemeral empty: we do not attach a second disk for
			// ephemeral storage. The agent honors
			// CreatePartitionIfNoEphemeralDisk=true (set in agent.json on
			// the stemcell) and carves the ephemeral partition out of the
			// root disk. Setting Ephemeral here would cause the agent's
			// DevicePathResolver to poll forever for a second disk to appear.
			Persistent: map[string]string{},
		},
		Env:       parsed.env,
		MBus:      mbus,
		Blobstore: blobstore,
		VM: agent.VMSpec{
			Name: vmName,
			ID:   strconv.Itoa(vmid),
		},
	}

	if err := deps.Agent.Configure(ctx, shape.node, vmid, agentCfg); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("create_vm: agent configure vmid=%d", vmid))
	}
	return nil
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
	netNames []string,
) (map[string]createVMNetworkSpec, error) {
	// -----------------------------------------------------------------------
	// 8. Start VM
	// -----------------------------------------------------------------------
	// Wrap in RetryOnTransient so a pvedaemon worker-recycle (HTTP 5xx /
	// "got no worker upid - start worker failed") under burst load is absorbed
	// in-process rather than surfacing as RetriableCloudError to the director.
	var startUPID string
	if err := pve.RetryOnTransient(ctx, logger, "create_vm.start", 0, func() error {
		var innerErr error
		startUPID, innerErr = deps.PVE.QEMU().Start(ctx, shape.node, vmid)
		return innerErr
	}); err != nil {
		return nil, cpierrors.Cloud("create_vm: start vmid=%d: %s", vmid, err.Error())
	}

	if err := pve.AwaitTaskWithLogger(ctx, deps.PVE, shape.node, startUPID, logger); err != nil {
		return nil, cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("create_vm: await start task vmid=%d", vmid))
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

	return buildResponseNetworks(parsed.networks, netNames, vmCfg), nil
}

// --------------------------------------------------------------------------
// cleanupVM attempts to stop and purge a created VM on error. All errors are
// logged but suppressed so the original error propagates unmodified.
// --------------------------------------------------------------------------
func cleanupVM(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) {
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int(metadataKeyVMID, vmid))

	// Stop (best-effort; VM may not have started yet). Wrap in RetryOnTransient
	// so a pvedaemon worker-recycle during rollback doesn't bubble out — this
	// path is best-effort already and absorbing the transient keeps the
	// rollback graceful.
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "create_vm.cleanup.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	if stopErr == nil && stopUPID != "" {
		if awaitErr := pve.AwaitTask(ctx, deps.PVE, node, stopUPID); awaitErr != nil {
			logger.Warn("create_vm: rollback stop task failed", log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
		}
	}

	// Purge the VM
	purge := true
	destroyUnref := true
	delResp, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, strconv.Itoa(vmid), &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyUnref,
	})
	if delErr != nil {
		if pve.IsNotFound(delErr) || pve.IsPmxcfsConfigMissing(delErr) {
			logger.Info("create_vm: rollback delete -- VM already gone (idempotent)", log.Int(metadataKeyVMID, vmid))
		} else {
			logger.Error("create_vm: rollback delete failed", log.Int(metadataKeyVMID, vmid), log.Err(delErr))
		}
	} else {
		// Await the destroy task so PVE fully releases the VMID before we return.
		// An empty UPID means synchronous completion; skip await in that case.
		if delResp != nil {
			delUPID, upidErr := pve.UPIDFromRaw(*delResp)
			if upidErr != nil {
				logger.Warn("create_vm: cannot parse UPID from rollback delete response -- skipping await",
					log.Int(metadataKeyVMID, vmid), log.Err(upidErr))
			} else if delUPID != "" {
				if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, delUPID, logger); awaitErr != nil {
					if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
						logger.Info("create_vm: rollback destroy await -- VM already gone (idempotent)",
							log.Int(metadataKeyVMID, vmid))
					} else {
						logger.Error("create_vm: rollback destroy await failed",
							log.Int(metadataKeyVMID, vmid), log.Err(awaitErr))
					}
				}
			}
		}
		logger.Info("create_vm: rollback complete", log.Int(metadataKeyVMID, vmid))
	}

	// Remove any agent-side artifacts (e.g. the ConfigDrive ISO uploaded by
	// the configdrive agent). VM purge removes referenced disk volumes
	// but does not touch independent content uploaded with content=iso, so
	// the ISO must be deleted via the agent. Order matters: purge first, so
	// the CD-ROM reference is gone before the underlying volume is removed.
	if deps.Agent != nil {
		if remErr := deps.Agent.Remove(ctx, node, vmid); remErr != nil {
			logger.Warn("create_vm: rollback agent remove failed",
				log.Int(metadataKeyVMID, vmid), log.Err(remErr))
		}
	}
}

// --------------------------------------------------------------------------
// sortedNetworkNames returns network names in a deterministic order.
// "default" network (if present) is first; remaining names are alphabetical.
//
// The previous implementation iterated the tail of a pre-built slice that
// already had "default" at index 0, which meant the bubble-sort only ran
// over non-default names when "default" was present. When "default" was
// absent the slice had no guaranteed ordering because Go map iteration is
// randomised — bug B8. This implementation collects non-default names into
// a fresh slice, sorts them with sort.Strings (correct O(n log n)), then
// prepends "default" only if it exists.
// --------------------------------------------------------------------------
func sortedNetworkNames(networks map[string]createVMNetworkSpec) []string {
	names := make([]string, 0, len(networks))
	hasDefault := false
	for n := range networks {
		if n == "default" {
			hasDefault = true
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if hasDefault {
		return append([]string{"default"}, names...)
	}
	return names
}

// --------------------------------------------------------------------------
// ipToCIDR converts a dotted-decimal netmask to a prefix length and returns
// "ip/prefix". Falls back to "/32" if the netmask cannot be parsed.
// --------------------------------------------------------------------------
func ipToCIDR(ip, netmask string) string {
	prefix := netmaskToCIDR(netmask)
	return fmt.Sprintf("%s/%d", ip, prefix)
}

// netmaskToCIDR counts set bits in a dotted-decimal subnet mask.
func netmaskToCIDR(netmask string) int {
	if netmask == "" {
		return 32
	}
	parts := strings.Split(netmask, ".")
	if len(parts) != 4 {
		return 32
	}
	bits := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return 32
		}
		for b := 7; b >= 0; b-- {
			if (n>>uint(b))&1 == 1 {
				bits++
			}
		}
	}
	return bits
}

// --------------------------------------------------------------------------
// buildAgentNetworks converts CPI network specs to agent.NetworkSpec map.
// --------------------------------------------------------------------------
func buildAgentNetworks(networks map[string]createVMNetworkSpec) map[string]agent.NetworkSpec {
	out := make(map[string]agent.NetworkSpec, len(networks))
	for name := range networks {
		spec := networks[name]
		out[name] = agent.NetworkSpec{
			Type:    spec.Type,
			IP:      spec.IP,
			Netmask: spec.Netmask,
			Gateway: spec.Gateway,
			DNS:     spec.DNS,
			Default: spec.Default,
		}
	}
	return out
}

// --------------------------------------------------------------------------
// extractJobNameFromEnv returns the BOSH instance-group (job) name from the
// create_vm env, or "" if it cannot be derived.
//
// env["bosh"]["group"] is "<director>-<deployment>-<job>" and
// env["bosh"]["groups"] is an array containing all combinations of those
// three (including each one standalone). The bare job name is the shortest
// element G for which group has suffix "-G" (or group == G). Using the
// shortest such suffix avoids confusing "<deployment>-<job>" with "<job>"
// when the job name itself contains hyphens (e.g. "diego-api").
//
// Returns the raw job name; the caller must run sanitizeVMName before
// handing it to PVE.
// --------------------------------------------------------------------------
func extractJobNameFromEnv(env map[string]any) string {
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	group, _ := boshRaw["group"].(string)
	groupsRaw, _ := boshRaw["groups"].([]any)
	if group == "" || len(groupsRaw) == 0 {
		return ""
	}
	var best string
	for _, g := range groupsRaw {
		s, ok := g.(string)
		if !ok || s == "" || s == group {
			continue
		}
		if !strings.HasSuffix(group, "-"+s) {
			continue
		}
		if best == "" || len(s) < len(best) {
			best = s
		}
	}
	return best
}

// extractDeploymentFromEnv returns the BOSH deployment name from the
// create_vm env, or "" if it cannot be derived. Given env.bosh.group =
// "<director>-<deployment>-<job>" and the already-resolved job, the
// remainder ("<director>-<deployment>") has the deployment as the shortest
// suffix in env.bosh.groups that is neither the remainder itself nor the
// bare director. This mirrors extractJobNameFromEnv's "shortest matching
// suffix" rule so a deployment name containing hyphens still resolves
// correctly. Returns "" when env.bosh is absent, when group is empty, or
// when job is empty (the deployment cannot be located without first
// stripping the job suffix from group).
func extractDeploymentFromEnv(env map[string]any, job string) string {
	if job == "" {
		return ""
	}
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	group, _ := boshRaw["group"].(string)
	groupsRaw, _ := boshRaw["groups"].([]any)
	if group == "" || len(groupsRaw) == 0 {
		return ""
	}
	remainder := strings.TrimSuffix(group, "-"+job)
	if remainder == group || remainder == "" {
		return ""
	}
	var best string
	for _, g := range groupsRaw {
		s, ok := g.(string)
		if !ok || s == "" || s == remainder || s == group || s == job {
			continue
		}
		if !strings.HasSuffix(remainder, "-"+s) {
			continue
		}
		if best == "" || len(s) < len(best) {
			best = s
		}
	}
	return best
}

// extractInstanceNameFromEnv returns the BOSH instance-group name embedded
// in env.bosh.instance.name (the bosh-init / create-env env shape includes
// this even when env.bosh.group is absent). Returns "" when neither key is
// present.
func extractInstanceNameFromEnv(env map[string]any) string {
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	if inst, ok := boshRaw["instance"].(map[string]any); ok {
		if s, _ := inst[metadataKeyName].(string); s != "" {
			return s
		}
	}
	return ""
}

// instanceGroupName returns the BOSH instance-group name for the VM being
// created — the unit of anti-affinity spreading. It prefers the job suffix
// derived from env.bosh.group/groups (director deploys) and falls back to
// env.bosh.instance.name (create-env / bosh-init). Returns "" when neither is
// derivable.
func instanceGroupName(env map[string]any) string {
	if job := extractJobNameFromEnv(env); job != "" {
		return job
	}
	return extractInstanceNameFromEnv(env)
}

// antiAffinityGroupTag returns the PVE tag that marks membership in the VM's
// BOSH instance group, formed to match the tag scheme set_vm_metadata stamps
// ("job--<sanitized>"). It returns "" — disabling scheduler-soft spreading for
// this create — when anti-affinity is not enabled or the instance group cannot
// be derived from env.
func antiAffinityGroupTag(cfg *config.CPIConfig, env map[string]any) string {
	if !cfg.AntiAffinityEnabled() {
		return ""
	}
	group := sanitizeTagValue(instanceGroupName(env))
	if group == "" {
		return ""
	}
	return "job--" + group
}

// composeVMName builds the PVE VM name from prefix + deployment + job +
// optional index. Empty segments are dropped, so a metadata payload missing
// the deployment still yields "<prefix>-<job>-<index>" rather than a
// double-dash. Returns "" when no segment is populated; the caller then
// falls back to the "vm-<vmid>" placeholder.
func composeVMName(prefix, deployment, job, index string) string {
	parts := make([]string, 0, 4)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	if deployment != "" {
		parts = append(parts, deployment)
	}
	if job != "" {
		parts = append(parts, job)
	}
	if index != "" {
		parts = append(parts, index)
	}
	if len(parts) == 0 {
		return ""
	}
	return sanitizeVMName(strings.Join(parts, "-"))
}

// --------------------------------------------------------------------------
// extractMBusAndBlobstore pulls mbus and blobstore from the env map. BOSH
// uses three distinct env shapes depending on the caller:
//
//   - Director deploys (the common path): keys live under env["bosh"],
//     with env["bosh"]["mbus"] as a STRING (e.g. "nats://10.0.0.1:4222")
//     and env["bosh"]["blobstores"] as an array (first entry is the
//     director-side blobstore).
//   - create-env / bosh-init: keys also live under env["bosh"], but
//     env["bosh"]["mbus"] is an OBJECT with at least env["bosh"]["mbus"]["url"]
//     plus TLS cert fields. We extract .url.
//   - Legacy / out-of-band callers: top-level env["mbus"] (string) and
//     env["blobstore"] (object). Accepted as a fallback for compatibility.
//
// Tolerant: missing keys return zero values.
// --------------------------------------------------------------------------
func extractMBusAndBlobstore(env map[string]any) (string, agent.BlobstoreSpec) {
	mbus, _ := env["mbus"].(string)

	var bs agent.BlobstoreSpec
	if bsRaw, ok := env["blobstore"].(map[string]any); ok {
		bs.Provider, _ = bsRaw["provider"].(string)
		bs.Options, _ = bsRaw["options"].(map[string]any)
	}

	if boshRaw, ok := env["bosh"].(map[string]any); ok {
		if mbus == "" {
			// Director-deploy shape: env.bosh.mbus is a flat string.
			if s, ok := boshRaw["mbus"].(string); ok {
				mbus = s
			}
		}
		if mbus == "" {
			// create-env / bosh-init shape: env.bosh.mbus is an object
			// with .url (plus cert fields we forward via env elsewhere).
			if mbusRaw, ok := boshRaw["mbus"].(map[string]any); ok {
				if u, ok := mbusRaw["url"].(string); ok {
					mbus = u
				}
			}
		}
		if bs.Provider == "" {
			if blobs, ok := boshRaw["blobstores"].([]any); ok && len(blobs) > 0 {
				if b0, ok := blobs[0].(map[string]any); ok {
					bs.Provider, _ = b0["provider"].(string)
					bs.Options, _ = b0["options"].(map[string]any)
				}
			}
		}
	}

	if bs.Options == nil {
		bs.Options = map[string]any{}
	}
	return mbus, bs
}

// --------------------------------------------------------------------------
// buildResponseNetworks constructs the v2 response networks map.
// It copies input specs and fills in the MAC address read from PVE VM config.
// PVE stores NICs as "net0" → "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,...".
// --------------------------------------------------------------------------
func buildResponseNetworks(
	networks map[string]createVMNetworkSpec,
	orderedNames []string,
	vmCfg map[string]any,
) map[string]createVMNetworkSpec {
	// Build index → MAC lookup from VM config
	macByIndex := extractMACsFromConfig(vmCfg)

	out := make(map[string]createVMNetworkSpec, len(networks))
	for i, name := range orderedNames {
		spec := networks[name]
		if mac, ok := macByIndex[i]; ok {
			spec.MAC = mac
		}
		out[name] = spec
	}
	// Copy any names not in orderedNames (defensive)
	for name := range networks {
		if _, exists := out[name]; !exists {
			out[name] = networks[name]
		}
	}
	return out
}

// extractMACsFromConfig parses "net0", "net1", ... keys from PVE VM config and
// returns map[nicIndex]macAddress. PVE net value format:
//
//	"virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1"
//	  or
//	"virtio,bridge=vmbr0"   (no MAC, PVE assigns later)
func extractMACsFromConfig(cfg map[string]any) map[int]string {
	macs := make(map[int]string)
	for key, val := range cfg {
		if !strings.HasPrefix(key, "net") {
			continue
		}
		idxStr := strings.TrimPrefix(key, "net")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		valStr, ok := val.(string)
		if !ok {
			continue
		}
		mac := parseMACFromNetValue(valStr)
		if mac != "" {
			macs[idx] = mac
		}
	}
	return macs
}

// parseMACFromNetValue extracts the MAC from a PVE net value string.
// Format: "model=MAC,bridge=X,..." or "model,bridge=X,..."
// The MAC is the part after "=" in the first segment if it contains ":".
func parseMACFromNetValue(val string) string {
	segments := strings.Split(val, ",")
	if len(segments) == 0 {
		return ""
	}
	// First segment is either "model" or "model=MAC"
	first := segments[0]
	eqIdx := strings.Index(first, "=")
	if eqIdx < 0 {
		return ""
	}
	mac := first[eqIdx+1:]
	// Validate it looks like a MAC (contains ":")
	if strings.Count(mac, ":") == 5 {
		return strings.ToLower(mac)
	}
	return ""
}

// waitUntilAgentReady polls the QEMU guest agent via CreateQemuAgentPing until
// the agent responds or the deadline derived from health_check.timeout_sec expires.
//
// Behavior:
//   - A successful ping (nil error) returns nil immediately.
//   - A transient ping error (transport fault, connection refused, 5xx) is
//     retried after the effective poll interval.
//   - A permanent ping error (auth failure, 4xx non-transport) fails fast
//     without waiting for the deadline; diagnostics are still gathered.
//   - On deadline expiry or parent context cancellation, ListQemuStatusCurrent
//     is called to gather VM status diagnostics; the diagnostics are folded into
//     the returned error so the existing rollback defer has context.
//   - Parent context cancellation is honored: if ctx.Done() fires, the function
//     returns promptly regardless of the health-check deadline.
//   - The effective poll interval is max(configured interval, healthPollMinInterval)
//     so a configured value of 0 never produces a tight busy-loop in production.
//     The interval sleep is deadline-aware: both hcCtx.Done() and ctx.Done()
//     wake it early.
//
// Diagnostics source: VM status from ListQemuStatusCurrent only. There is no
// clean REST surface to retrieve arbitrary task-log lines without a UPID, so
// status-only enrichment is the intended and complete behavior.
//
// The returned error is a non-retriable CloudError. The existing create_vm
// rollback (cleanupVM defer) fires automatically because retErr != nil.
func waitUntilAgentReady(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	logger *log.Logger,
) error {
	timeoutSec := deps.Config.HealthCheckTimeoutSec()
	intervalSec := deps.Config.HealthCheckIntervalSec()
	vmidStr := strconv.Itoa(vmid)

	// Compute effective poll interval. The configured value of 0 is valid
	// ("no explicit preference") but must not produce a tight busy-loop in
	// production. Apply the package-level floor; tests may lower it to zero.
	effectiveInterval := time.Duration(intervalSec) * time.Second
	if floor := healthPollMinInterval(); effectiveInterval < floor {
		effectiveInterval = floor
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	hcCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	logger.Debug("create_vm: health gate: polling guest agent",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.Int("timeout_sec", timeoutSec),
	)

	for {
		// Respect parent context cancellation.
		select {
		case <-ctx.Done():
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		default:
		}

		_, pingErr := deps.PVE.Nodes().CreateQemuAgentPing(hcCtx, node, vmidStr)
		if pingErr == nil {
			logger.Debug("create_vm: health gate: guest agent ready",
				log.Int(metadataKeyVMID, vmid))
			return nil
		}

		// Check whether the health-check deadline or parent context expired.
		if hcCtx.Err() != nil {
			msg := fmt.Sprintf(
				"create_vm: health gate: timeout waiting for guest agent on vm %d after %ds",
				vmid, timeoutSec)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger, msg)
		}
		if ctx.Err() != nil {
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		}

		// Classify the ping error: transient faults are retried; permanent faults
		// (auth failures, 4xx non-transport responses) fail fast to avoid spinning
		// for the full timeout when the outcome is already determined.
		if !pve.IsTransientTransport(pingErr) {
			logger.Error("create_vm: health gate: permanent agent ping error, failing fast",
				log.Int(metadataKeyVMID, vmid),
				log.String("node", node),
				log.Err(pingErr),
			)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: permanent error pinging agent on vm %d: %v",
					vmid, pingErr))
		}

		logger.Debug("create_vm: health gate: agent ping failed (retrying)",
			log.Int(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Err(pingErr),
		)

		// Deadline-aware sleep. Both hcCtx.Done() and ctx.Done() wake the select
		// early so the deadline still bounds total wait time regardless of interval.
		select {
		case <-time.After(effectiveInterval):
		case <-ctx.Done():
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		case <-hcCtx.Done():
			msg := fmt.Sprintf(
				"create_vm: health gate: timeout waiting for guest agent on vm %d after %ds",
				vmid, timeoutSec)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger, msg)
		}
	}
}

// gatherHealthDiagnostics calls ListQemuStatusCurrent to enrich the error with
// VM state at the time of the health-gate failure. Task-log lines are not
// included: no clean REST surface exists for retrieving arbitrary log content
// without a UPID, so status-only enrichment is the complete intended behavior.
// On ListQemuStatusCurrent error the base message is returned without
// enrichment (best-effort). Always returns a non-retriable CloudError.
func gatherHealthDiagnostics(
	_ context.Context,
	deps Deps,
	node string,
	vmid int,
	vmidStr string,
	logger *log.Logger,
	baseMsg string,
) error {
	// Use a fresh context for the diagnostic scrape; the original context may
	// already be cancelled or at its deadline.
	diagCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, statusErr := deps.PVE.Nodes().ListQemuStatusCurrent(diagCtx, node, vmidStr)
	if statusErr != nil {
		logger.Warn("create_vm: health gate: could not gather VM status for diagnostics",
			log.Int(metadataKeyVMID, vmid), log.Err(statusErr))
		return cpierrors.Cloud("%s (diagnostics unavailable: %s)", baseMsg, statusErr.Error())
	}

	qmpStatus := ""
	if status.Qmpstatus != nil {
		qmpStatus = *status.Qmpstatus
	}
	logger.Error("create_vm: health gate failed",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.String("vm_status", status.Status),
		log.String("qmp_status", qmpStatus),
	)

	return cpierrors.Cloud("%s (vm_status=%s qmp_status=%s)",
		baseMsg, status.Status, qmpStatus)
}
