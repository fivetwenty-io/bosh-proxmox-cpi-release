// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

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

// HandleCreateVM returns a cpi.Handler that implements the BOSH CPI create_vm method.
//
// Arguments (positional, all required):
//
//	[0] agent_id      string
//	[1] stemcell_cid  string  ("<storage>:import/<filename>" volid format)
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
	stemcellStorage, _, err := pve.ParseStemcellCID(stemcellCID)
	if err != nil {
		return nil, cpierrors.Cloud("create_vm: invalid stemcell_cid %q: %s", stemcellCID, err.Error())
	}
	// stemcellStorage is used as a fallback VMStorage when deps.Config.VMStorage is empty;
	// it must be non-empty (guaranteed by ParseStemcellCID returning a non-empty storage).
	_ = stemcellStorage

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

	// -----------------------------------------------------------------------
	// 2. Resolve node from cloud_properties or config default
	// -----------------------------------------------------------------------
	node := cloudProps.TargetNode
	if node == "" {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("create_vm: target node not set in cloud_properties.target_node or config.node")
	}

	// -----------------------------------------------------------------------
	// 3. Resolve VM-shape parameters used by every allocation attempt.
	// -----------------------------------------------------------------------
	rangeStart := deps.Config.VMIDRangeStart
	if rangeStart < 100 {
		rangeStart = pve.VMIDRangeVMStart
	}
	maxAttempts := deps.Config.VMIDAllocAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	// Resolve disk format: prefer cloud_properties.vm_disk_format; fall back to "qcow2".
	vmDiskFormat := cloudProps.VMDiskFormat
	if vmDiskFormat == "" {
		vmDiskFormat = "qcow2"
	}

	// Resolve target storage: prefer config VMStorage; fall back to the stemcell's
	// own storage so the import stays on-node when no override is configured.
	vmStorage := deps.Config.VMStorage
	if vmStorage == "" {
		vmStorage = stemcellStorage
	}

	// Compute root disk size in GiB. cloud_properties.disk is in MiB; round up.
	// Minimum is defaultStemcellDiskGiB so we never request a size smaller than
	// the imported image (PVE enforces a no-shrink rule — the import itself would
	// succeed but the explicit size= directive would be ignored or error).
	rootDiskGiB := defaultStemcellDiskGiB
	if cloudProps.Disk > 0 {
		requestedGiB := (cloudProps.Disk + 1023) / 1024
		if requestedGiB > rootDiskGiB {
			rootDiskGiB = requestedGiB
		}
	}

	// cloud_properties supports two conventions:
	//   - vSphere CPI style: cpu = total vCPU count (cores × sockets).
	//   - PVE-native: cores/sockets explicit.
	// Explicit cores/sockets win when present; otherwise fall back to cpu
	// as cores with a single socket. Default is 1 vCPU.
	cores := cloudProps.Cores
	if cores <= 0 && cloudProps.CPU > 0 {
		cores = cloudProps.CPU
	}
	if cores <= 0 {
		cores = 1
	}
	sockets := cloudProps.Sockets
	if sockets <= 0 {
		sockets = 1
	}
	memMiB := cloudProps.Memory
	if memMiB <= 0 {
		memMiB = 512
	}

	// -----------------------------------------------------------------------
	// 4. Allocate VMID + create VM via retry-on-conflict.
	//    PVE may reject POST /qemu with HTTP 500 "VM N already exists" when a
	//    concurrent CPI process wins the same VMID. AllocateWithRetry picks a
	//    fresh VMID after backoff and re-attempts. The retry callback also
	//    rolls back per-attempt failures whose UPID-await surfaces a
	//    non-conflict error (PVE accepted the POST but the task itself failed).
	// -----------------------------------------------------------------------
	var createUPID string
	vmid, err := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			virtio0Val := fmt.Sprintf("%s:0,import-from=%s,format=%s,size=%dG",
				vmStorage, stemcellCID, vmDiskFormat, rootDiskGiB)
			candidateName := fmt.Sprintf("vm-%d", candidate)

			createParams := map[string]interface{}{
				"vmid":    candidate,
				"name":    candidateName,
				"memory":  memMiB,
				"cores":   cores,
				"ostype":  "l26",
				"scsihw":  "virtio-scsi-pci",
				"virtio0": virtio0Val,
				"boot":    "order=virtio0",
				"agent":   "enabled=1",
				"hotplug": "network,disk,cpu",
				"onboot":  0,
			}
			if sockets > 1 {
				createParams["sockets"] = sockets
			}

			upid, cerr := deps.PVE.QEMU().Create(ctx, node, createParams)
			if cerr != nil {
				if pve.IsVMIDConflict(cerr) {
					logger.Info("create_vm: vmid conflict, retrying",
						log.Int("vmid_attempted", candidate),
					)
				}
				return cerr
			}

			if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger,
				pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
				if pve.IsVMIDConflict(werr) {
					logger.Info("create_vm: vmid conflict on await, retrying",
						log.Int("vmid_attempted", candidate),
					)
					return werr
				}
				// Non-conflict failure after Create succeeded: the VM may
				// have been partially registered. Roll back this attempt
				// before propagating so the next retry (which won't run)
				// or the caller sees a clean slate.
				cleanupVM(contextWithoutCancel(ctx), deps, node, candidate, logger)
				return werr
			}

			createUPID = upid
			return nil
		},
		pve.IsVMIDConflict,
		maxAttempts,
		pve.WithRange(rangeStart, pve.VMIDRangeVMEnd),
	)
	if err != nil {
		return nil, cpierrors.Wrap(err, "create_vm: allocate+create VM")
	}
	_ = createUPID

	vmName := fmt.Sprintf("vm-%d", vmid)

	// Arm rollback for stages 4b–8: any failure after this point destroys
	// the winning VM.
	vmCreated := true
	defer func() {
		if retErr != nil && vmCreated {
			cleanupVM(contextWithoutCancel(ctx), deps, node, vmid, logger)
		}
	}()

	logger.Info("create_vm: vm created and disk imported",
		log.Int("vmid", vmid),
		log.String("stemcell_cid", stemcellCID),
		log.String("storage", vmStorage),
		log.Int("root_disk_gib", rootDiskGiB),
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
	growGiB := rootDiskGiB - defaultStemcellDiskGiB
	if growGiB > 0 {
		resizeUPID, rerr := deps.PVE.QEMU().ResizeDisk(ctx, node, vmid, "virtio0", growGiB)
		if rerr != nil {
			return nil, cpierrors.Cloud("create_vm: resize virtio0 vmid=%d +%dG: %s", vmid, growGiB, rerr.Error())
		}
		if resizeUPID != "" {
			if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, resizeUPID, logger); werr != nil {
				return nil, cpierrors.Wrap(werr, fmt.Sprintf("create_vm: await resize vmid=%d", vmid))
			}
		}
		logger.Info("create_vm: grew virtio0",
			log.Int("vmid", vmid),
			log.Int("delta_gib", growGiB),
			log.Int("final_gib", rootDiskGiB),
		)
	}

	// -----------------------------------------------------------------------
	// 5. Configure NICs from networks map
	// -----------------------------------------------------------------------
	// Build an ordered list of network names for deterministic NIC assignment.
	netNames := sortedNetworkNames(networks)

	// bridge and model defaults
	defaultBridge := deps.Config.NetworkBridge
	if defaultBridge == "" {
		defaultBridge = "vmbr0"
	}
	if cloudProps.NetworkBridge != "" {
		defaultBridge = cloudProps.NetworkBridge
	}
	defaultModel := "virtio"
	if cloudProps.NetworkModel != "" {
		defaultModel = cloudProps.NetworkModel
	}

	// Build net map[int]string and ipconfig map[int]string for UpdateQemuConfigParams
	netMap := make(map[int]string, len(netNames))
	ipconfigMap := make(map[int]string, len(netNames))
	var nameservers []string
	firstNS := true

	for i, name := range netNames {
		spec := networks[name]

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

	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), nicParams); err != nil {
		return nil, cpierrors.Cloud("create_vm: configure NICs vmid=%d: %s", vmid, err.Error())
	}

	// -----------------------------------------------------------------------
	// 6. Attach persistent disks (disk_cids pre-attach)
	// -----------------------------------------------------------------------
	for _, diskCID := range diskCIDs {
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
			return nil, cpierrors.Cloud("create_vm: parse disk_cid %q: %s", diskCID, parseErr.Error())
		}
		diskID, err := deps.PVE.QEMU().AttachDisk(ctx, node, vmid, diskCID, "scsi", nil)
		if err != nil {
			return nil, cpierrors.Cloud("create_vm: attach disk %q to vmid=%d: %s", diskCID, vmid, err.Error())
		}
		logger.Info("create_vm: attached persistent disk",
			log.Int("vmid", vmid),
			log.String("disk_cid", diskCID),
			log.String("disk_id", diskID),
		)
	}

	// -----------------------------------------------------------------------
	// 7. Build AgentConfig and call agent.Configure
	// -----------------------------------------------------------------------
	agentNetworks := buildAgentNetworks(networks)
	mbus, blobstore := extractMBusAndBlobstore(env)
	// create-env (bosh-init) does not put the agent mbus URL in env; it
	// lives in CPI config as cloud_provider.properties.agent.mbus. Fall back
	// to the configured value when env didn't carry one.
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
		AgentID:  agentID,
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
		Env:       env,
		MBus:      mbus,
		Blobstore: blobstore,
		VM: agent.VMSpec{
			Name: vmName,
			ID:   strconv.Itoa(vmid),
		},
	}

	if err := deps.Agent.Configure(ctx, node, vmid, agentCfg); err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("create_vm: agent configure vmid=%d", vmid))
	}

	// -----------------------------------------------------------------------
	// 8. Start VM
	// -----------------------------------------------------------------------
	startUPID, err := deps.PVE.QEMU().Start(ctx, node, vmid)
	if err != nil {
		return nil, cpierrors.Cloud("create_vm: start vmid=%d: %s", vmid, err.Error())
	}

	if err := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, startUPID, logger); err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("create_vm: await start task vmid=%d", vmid))
	}

	logger.Info("create_vm: VM started", log.Int("vmid", vmid))

	// -----------------------------------------------------------------------
	// 9. Read back VM config to extract assigned MAC addresses
	// -----------------------------------------------------------------------
	vmCfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		// Non-fatal: return networks without MAC rather than rolling back
		logger.Warn("create_vm: could not read VM config for MAC extraction",
			log.Int("vmid", vmid),
			log.Err(err),
		)
		vmCfg = map[string]interface{}{}
	}

	responseNetworks := buildResponseNetworks(networks, netNames, vmCfg)

	vmCID := strconv.Itoa(vmid)
	return []any{vmCID, responseNetworks}, nil
}

// --------------------------------------------------------------------------
// cleanupVM attempts to stop and purge a created VM on error. All errors are
// logged but suppressed so the original error propagates unmodified.
// --------------------------------------------------------------------------
func cleanupVM(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) {
	logger.Warn("create_vm: rolling back, destroying created VM", log.Int("vmid", vmid))

	// Stop (best-effort; VM may not have started yet)
	stopUPID, stopErr := deps.PVE.QEMU().Stop(ctx, node, vmid)
	if stopErr == nil && stopUPID != "" {
		if awaitErr := pve.AwaitTask(ctx, deps.PVE, node, stopUPID); awaitErr != nil {
			logger.Warn("create_vm: rollback stop task failed", log.Int("vmid", vmid), log.Err(awaitErr))
		}
	}

	// Purge the VM
	purge := true
	destroyUnref := true
	_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, strconv.Itoa(vmid), &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyUnref,
	})
	if delErr != nil {
		logger.Error("create_vm: rollback delete failed", log.Int("vmid", vmid), log.Err(delErr))
	} else {
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
// --------------------------------------------------------------------------
func sortedNetworkNames(networks map[string]createVMNetworkSpec) []string {
	names := make([]string, 0, len(networks))
	for n := range networks {
		names = append(names, n)
	}
	// Stable sort: "default" first, then alphabetical.
	sorted := make([]string, 0, len(names))
	for _, n := range names {
		if n == "default" {
			sorted = append([]string{n}, sorted...)
		} else {
			sorted = append(sorted, n)
		}
	}
	// Sort the non-default portion alphabetically for full determinism.
	if len(sorted) > 1 {
		tail := sorted[1:]
		for i := 0; i < len(tail)-1; i++ {
			for j := i + 1; j < len(tail); j++ {
				if tail[i] > tail[j] {
					tail[i], tail[j] = tail[j], tail[i]
				}
			}
		}
	}
	return sorted
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
	for name, spec := range networks {
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
// extractMBusAndBlobstore pulls mbus and blobstore from the env map. BOSH
// uses two distinct env shapes depending on the caller:
//
//   - Director deploys: top-level keys env["mbus"] (string), env["blobstore"]
//     (object with provider/options).
//   - create-env / bosh-init: keys live under env["bosh"], with
//     env["bosh"]["mbus"]["url"] (string) and env["bosh"]["blobstores"]
//     (array; the first entry is the director-side blobstore).
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
	vmCfg map[string]interface{},
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
	for name, spec := range networks {
		if _, exists := out[name]; !exists {
			out[name] = spec
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
func extractMACsFromConfig(cfg map[string]interface{}) map[int]string {
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
