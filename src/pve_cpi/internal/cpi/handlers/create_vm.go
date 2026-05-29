package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// createVMRetryBackoff selects the sleep duration between AllocateWithRetry
// attempts in create_vm. VMID conflicts only need a brief jitter to
// decorrelate herds across concurrent CPI processes; storage lock-file
// timeouts mean PVE is serialising imports against a busy storage and
// retrying immediately wins us nothing — back off seconds, exponentially,
// up to 30s.
func createVMRetryBackoff(err error, attempt int) time.Duration {
	if pve.IsStorageLockTimeout(err) {
		// Exponential base 2s × 1.5^attempt with ±30% jitter, capped at 30s.
		base := 2 * time.Second
		factor := 1.0
		for i := 0; i < attempt; i++ {
			factor *= 1.5
		}
		d := time.Duration(float64(base) * factor)
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		// Jitter ±30%.
		jitter := time.Duration(mrand.Int64N(int64(d) * 6 / 10)) // #nosec G404 -- VMID jitter; non-cryptographic
		return d - d*3/10 + jitter
	}
	// VMID conflict (or anything else flagged retryable): uniform 50-250 ms.
	return 50*time.Millisecond + time.Duration(mrand.Int64N(int64(200*time.Millisecond))) // #nosec G404 -- retry jitter; non-cryptographic
}

// defaultStemcellDiskGiB is the minimum root-disk size assumed when
// cloud_properties.disk is absent or zero. Stemcells are always at least
// this large; PVE refuses to shrink below the imported image size anyway.
const defaultStemcellDiskGiB = 5

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
	networks     map[string]createVMNetworkSpec
	diskCIDs     []string
	env          map[string]any
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
		log.Int("vmid", vmid),
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
		agentID:      agentID,
		stemcellCID:  stemcellCID,
		rawCID:       rawCID,
		stemcellStor: stemcellStor,
		cloudProps:   cloudProps,
		networks:     networks,
		diskCIDs:     diskCIDs,
		env:          env,
	}, nil
}

// resolveVMShape derives the createVMShape from deps.Config + parsed args.
// Returns cpierrors.CloudError if the target node cannot be determined.
// vmStorageType is populated via a best-effort cluster storage list lookup;
// on failure (PVE unavailable, ClusterStorage not wired) the field is left ""
// so IsLinkedCloneSupported treats it as linked-capable (permissive default).
func resolveVMShape(ctx context.Context, deps Deps, parsed *createVMParsedArgs) (*createVMShape, error) {
	cp := parsed.cloudProps

	node := cp.TargetNode
	if node == "" {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("create_vm: target node not set in cloud_properties.target_node or config.node")
	}

	rangeStart, maxAttempts := resolveVMIDAllocParams(deps.Config)
	vmStorage, vmDiskFormat, rootDiskGiB := resolveVMShapeStorage(deps.Config, parsed)
	cores, sockets, memMiB := resolveVMShapeCPUMem(cp)
	hotplug, numaEnabled := resolveVMShapeHotplugNUMA(deps.Config, cp)

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
	}, nil
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
func resolveVMIDAllocParams(cfg *config.CPIConfig) (rangeStart, maxAttempts int) {
	rangeStart = cfg.VMIDRangeStart
	if rangeStart < 100 {
		rangeStart = pve.VMIDRangeVMStart
	}
	maxAttempts = cfg.VMIDAllocAttempts
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	return rangeStart, maxAttempts
}

// resolveVMShapeStorage returns the target VM storage, disk format, and root
// disk size in GiB. VMStorage falls back to the stemcell's own storage so the
// import stays on-node when no override is configured. Disk format defaults to
// qcow2. Root disk size is the max of cloud_properties.disk (MiB, rounded up to
// GiB) and defaultStemcellDiskGiB; PVE enforces a no-shrink rule so a smaller
// request is silently ignored, hence the floor.
func resolveVMShapeStorage(cfg *config.CPIConfig, parsed *createVMParsedArgs) (vmStorage, vmDiskFormat string, rootDiskGiB int) {
	cp := parsed.cloudProps

	vmStorage = cfg.VMStorage
	if vmStorage == "" {
		vmStorage = parsed.stemcellStor
	}

	vmDiskFormat = cp.VMDiskFormat
	if vmDiskFormat == "" {
		vmDiskFormat = diskFormatQCOW2
	}

	rootDiskGiB = defaultStemcellDiskGiB
	if cp.Disk > 0 {
		requestedGiB := (cp.Disk + 1023) / 1024
		if requestedGiB > rootDiskGiB {
			rootDiskGiB = requestedGiB
		}
	}
	return vmStorage, vmDiskFormat, rootDiskGiB
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
// cloud_properties → config → built-in default. Memory hotplug needs both
// numa=1 and "memory" in hotplug at create time; operators can override
// per-vm_type for stemcells that misbehave on memory hot-add.
func resolveVMShapeHotplugNUMA(cfg *config.CPIConfig, cp createVMCloudProps) (hotplug string, numaEnabled bool) {
	hotplug = cfg.HotplugValue()
	if cp.Hotplug != nil {
		hotplug = *cp.Hotplug
	}
	numaEnabled = cfg.NUMAValue()
	if cp.NUMA != nil {
		numaEnabled = *cp.NUMA
	}
	return hotplug, numaEnabled
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
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	vmid, err := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateVM(ctx, deps, logger, parsed, shape, candidate)
		},
		isRetryable,
		shape.maxAttempts,
		pve.WithRange(shape.rangeStart, deps.Config.VMIDRangeEnd),
		pve.WithBackoffFunc(createVMRetryBackoff),
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

		cloneErr := cloneFromTemplate(ctx, deps, logger, shape, candidate, candidateName, templateNode, templateVMID)
		if cloneErr != nil {
			// Classify for retry: VMID conflicts and transient transport faults are
			// retryable — they use the same retry classification as the import path.
			return handleCloneError(ctx, deps, logger, shape.node, candidate, cloneErr)
		}

		logger.Info("create_vm: vm cloned from template",
			log.Int("vmid_attempted", candidate),
			log.Int64("template_vmid", templateVMID),
			log.String("template_node", templateNode),
		)
		return nil
	}

	// --- Old-form CID: opportunistic template lookup before import-from ---
	//
	// D-07: if an existing template carries a matching sha tag, clone it
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
		"vmid":    candidate,
		"name":    candidateName,
		"memory":  shape.memMiB,
		"cores":   shape.cores,
		"ostype":  osTypeLinux26,
		"scsihw":  "virtio-scsi-pci",
		"virtio0": virtio0Val,
		"boot":    "order=virtio0",
		"agent":   "enabled=1",
		"hotplug": shape.hotplug,
		"onboot":  0,
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
	mode := deps.Config.CloneMode
	if mode == "" {
		mode = "auto"
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
			return cpierrors.Cloud(
				"create_vm: cross-node clone: cannot look up storage %q to determine if Target is safe: %s",
				shape.vmStorage, infoErr.Error())
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
	memStr := strconv.Itoa(shape.memMiB)
	cores64 := int64(shape.cores)
	sockets64 := int64(shape.sockets)
	resourceParams := &sdknodes.UpdateQemuConfigParams{
		Memory:  &memStr,
		Cores:   &cores64,
		Sockets: &sockets64,
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
		log.Int("vmid", vmid),
		log.Int("delta_gib", growGiB),
		log.Int("final_gib", shape.rootDiskGiB),
	)
	return nil
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

	// bridge and model defaults
	defaultBridge := deps.Config.NetworkBridge
	if defaultBridge == "" {
		defaultBridge = "vmbr0"
	}
	if parsed.cloudProps.NetworkBridge != "" {
		defaultBridge = parsed.cloudProps.NetworkBridge
	}
	defaultModel := "virtio"
	if parsed.cloudProps.NetworkModel != "" {
		defaultModel = parsed.cloudProps.NetworkModel
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
		// form (e.g. "data:vm-9003-disk-0"). The disk_cid is already in
		// that form, so pass it through verbatim. Stripping the storage
		// prefix produces a bare volname that PVE rejects with
		// "scsi0.file: invalid format - unable to parse volume ID ...".
		// ParseDiskCID is still used to validate the shape.
		if _, _, parseErr := pve.ParseDiskCID(diskCID); parseErr != nil {
			return cpierrors.Cloud("create_vm: parse disk_cid %q: %s", diskCID, parseErr.Error())
		}
		diskID, err := deps.PVE.QEMU().AttachDisk(ctx, shape.node, vmid, diskCID, "scsi", nil)
		if err != nil {
			return cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("create_vm: attach disk %q to vmid=%d: %s", diskCID, vmid, err.Error()))
		}
		logger.Info("create_vm: attached persistent disk",
			log.Int("vmid", vmid),
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

	logger.Info("create_vm: VM started", log.Int("vmid", vmid))

	// -----------------------------------------------------------------------
	// 9. Read back VM config to extract assigned MAC addresses
	// -----------------------------------------------------------------------
	vmCfg, err := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
	if err != nil {
		// Non-fatal: return networks without MAC rather than rolling back
		logger.Warn("create_vm: could not read VM config for MAC extraction",
			log.Int("vmid", vmid),
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
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int("vmid", vmid))

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
			logger.Warn("create_vm: rollback stop task failed", log.Int("vmid", vmid), log.Err(awaitErr))
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
			logger.Info("create_vm: rollback delete -- VM already gone (idempotent)", log.Int("vmid", vmid))
		} else {
			logger.Error("create_vm: rollback delete failed", log.Int("vmid", vmid), log.Err(delErr))
		}
	} else {
		// Await the destroy task so PVE fully releases the VMID before we return.
		// An empty UPID means synchronous completion; skip await in that case.
		if delResp != nil {
			delUPID, upidErr := pve.UPIDFromRaw(*delResp)
			if upidErr != nil {
				logger.Warn("create_vm: cannot parse UPID from rollback delete response -- skipping await",
					log.Int("vmid", vmid), log.Err(upidErr))
			} else if delUPID != "" {
				if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, delUPID, logger); awaitErr != nil {
					if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
						logger.Info("create_vm: rollback destroy await -- VM already gone (idempotent)",
							log.Int("vmid", vmid))
					} else {
						logger.Error("create_vm: rollback destroy await failed",
							log.Int("vmid", vmid), log.Err(awaitErr))
					}
				}
			}
		}
		logger.Info("create_vm: rollback complete", log.Int("vmid", vmid))
	}

	// Remove any agent-side artifacts (e.g. the ConfigDrive ISO uploaded by
	// the configdrive agent). VM purge removes referenced disk volumes
	// but does not touch independent content uploaded with content=iso, so
	// the ISO must be deleted via the agent. Order matters: purge first, so
	// the CD-ROM reference is gone before the underlying volume is removed.
	if deps.Agent != nil {
		if remErr := deps.Agent.Remove(ctx, node, vmid); remErr != nil {
			logger.Warn("create_vm: rollback agent remove failed",
				log.Int("vmid", vmid), log.Err(remErr))
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
		if s, _ := inst["name"].(string); s != "" {
			return s
		}
	}
	return ""
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
