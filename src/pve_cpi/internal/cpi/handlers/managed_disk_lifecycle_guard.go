package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"maps"
	"math"
	"strconv"
	"strings"
)

const (
	managedDiskServicePool    = "Pool"
	managedDiskServiceStorage = "Storage"
	managedDiskBusVirtio      = "virtio"
)

type managedDiskMutationObservation struct {
	preAbsent  bool
	copyLocal  bool
	node       string
	vmid       int
	before     map[string]any
	fields     map[string]any
	targetNode string
	targetVMID int
	targetSlot string
	targetGiB  int
	charges    bool
}

type managedDiskLifecycleGuard struct {
	lifecycle    *managedDiskLifecycle
	observations map[string]managedDiskMutationObservation
	created      map[int]bool
}

func newManagedDiskLifecycleGuard(m *managedDiskLifecycle) (*ManagedAllocationGuard, error) {
	state := &managedDiskLifecycleGuard{lifecycle: m, observations: map[string]managedDiskMutationObservation{}, created: map[int]bool{}}
	return NewManagedAllocationGuard(m.deps.PVE, ManagedAllocationHooks{Before: state.before, After: state.after, Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
		return m.session.Uncertain(call.Service + "." + call.Method)
	}})
}
func lifecycleMutationFields(params any) (map[string]any, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("invalid lifecycle mutation parameters")
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid lifecycle mutation parameters")
	}
	return fields, nil
}
func lifecycleInt(value any) (int, error) {
	n, err := strconv.Atoi(fmt.Sprint(value))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("lifecycle mutation lacks positive target identity")
	}
	return n, nil
}
func (g *managedDiskLifecycleGuard) before(ctx context.Context, call ManagedAllocationMutation) (string, error) {
	m := g.lifecycle
	if m.requestContext == nil || m.requestContext.Err() != nil {
		return "", fmt.Errorf("managed lifecycle request cancelled before mutation")
	}
	node, _ := call.Args[resourceTypeNode].(string)
	if node == "" {
		node = m.diskNode()
	}
	observation := managedDiskMutationObservation{node: node}
	storage, _, err := pve.ParseDiskCID(m.disk.volid)
	if err != nil {
		return "", err
	}
	backing, err := managedDiskActualBacking(ctx, m.deps, storage)
	if err != nil || backing != m.diskBacking() {
		return "", fmt.Errorf("managed disk backing changed before mutation")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, m.deps.PVE.Nodes(), []string{node})
	if err != nil || identity.ID() != m.handle.Record().ClusterID {
		return "", fmt.Errorf("managed disk cluster continuity changed before mutation")
	}
	key := call.Service + "." + call.Method
	if m.external {
		switch key {
		case "Pool.CreatePool", "Pool.DeletePool", "QEMU.Create", "QEMU.AttachDisk", "Nodes.UpdateQemuConfig", "Nodes.CreateQemuMoveDisk", "Nodes.CreateQemuMigrate", "Nodes.DeleteQemu":
		default:
			if !m.ownedRetention || !strings.HasPrefix(key, "Storage.DeleteVolume") {
				return "", fmt.Errorf("external disk preservation cannot perform this mutation")
			}
		}
	}
	if call.Service == managedDiskServicePool {
		pool, _ := call.Args["poolID"].(string)
		if !strings.HasPrefix(pool, "bosh-lock-") || (call.Method != "CreatePool" && call.Method != "DeletePool") {
			return "", fmt.Errorf("unrelated pool mutation in disk lifecycle")
		}
	} else if call.Service != managedDiskServiceStorage {
		value := call.Args["vmid"]
		if key == "QEMU.Create" {
			params, ok := call.Args["params"].(map[string]any)
			if !ok {
				return "", fmt.Errorf("invalid managed holder creation")
			}
			value = params["vmid"]
		}
		observation.vmid, err = lifecycleInt(value)
		if err != nil {
			return "", err
		}
		if key != "QEMU.Create" {
			observation.before, err = m.deps.PVE.QEMU().Config(ctx, node, observation.vmid)
			if err != nil || observation.before == nil {
				return "", fmt.Errorf("cannot verify lifecycle holder before mutation")
			}
		}
	}
	var charges []inv.ChargeRecord
	switch key {
	case "Pool.CreatePool", "Pool.DeletePool":
	case "QEMU.Create":
		err = g.prepareHolder(call, &observation, backing)
	case "QEMU.AttachDisk":
		err = g.prepareAttachment(call, &observation)
	case "QEMU.DetachDisk":
		return "", fmt.Errorf("managed detach must expose individual config mutation boundaries")
	case "QEMU.ResizeDisk":
		charges, err = g.prepareResize(ctx, call, &observation, storage, backing)
	case "QEMU.Snapshot", "QEMU.DeleteSnapshot":
		err = g.prepareSnapshot(&observation)
	case "Nodes.UpdateQemuConfig":
		err = g.prepareConfig(call, &observation)
	case "Nodes.CreateQemuMoveDisk":
		err = g.prepareMove(ctx, call, &observation)
	case "Nodes.CreateQemuMigrate":
		charges, err = g.prepareMigration(ctx, call, &observation, storage, backing)
	case "Storage.DeleteVolume", "Storage.DeleteVolumeAsync", "Storage.DeleteVolumeIfExists", "Storage.DeleteVolumeIfExistsAsync":
		err = g.prepareDeletion(ctx, call, &observation, storage)
	case "Nodes.DeleteQemu":
		err = g.prepareHolderDeletion(&observation)
	default:
		return "", fmt.Errorf("unexpected disk lifecycle mutation %s", key)
	}
	if err != nil {
		return "", err
	}

	target := aj.Target{External: m.external && !m.ownedRetention, VirtualBytes: m.retainedBytes, Node: node, Storage: storage, Backing: backing, VMID: observation.vmid, IntendedVolume: m.disk.volid}
	if observation.targetNode != "" {
		target.Node = observation.targetNode
	}
	step, err := storageMutationIntent(m.handle, "lifecycle_"+m.session.operation+"_"+call.Service+"_"+call.Method, target, charges)
	if err != nil {
		return "", err
	}
	g.observations[step] = observation
	return step, nil
}

func isDiskOptionKey(key string) bool {
	for _, prefix := range []string{"scsi", "sata", "ide", managedDiskBusVirtio, "unused"} {
		if strings.HasPrefix(key, prefix) {
			if _, err := strconv.Atoi(strings.TrimPrefix(key, prefix)); err == nil {
				return true
			}
		}
	}
	return false
}
func lifecycleConfigHasVolume(config map[string]any, volume string) bool {
	for key := range config {
		if !isDiskOptionKey(key) {
			continue
		}
		value, _ := pve.ConfigString(config, key)
		if strings.Split(value, ",")[0] == volume {
			return true
		}
	}
	return false
}
func lifecycleConfigHasAnyVolume(config map[string]any) bool {
	for key := range config {
		if !isDiskOptionKey(key) {
			continue
		}
		value, _ := pve.ConfigString(config, key)
		bare := strings.Split(value, ",")[0]
		if bare != "" && bare != "none" && bare != "cdrom" {
			return true
		}
	}
	return false
}
func lifecycleValidateConfigMutation(before map[string]any, fields map[string]any, volume string) error {
	for key, value := range fields {
		if key == pveConfigKeyDescription || key == "protection" || key == "digest" {
			continue
		}
		if key == "delete" {
			for _, field := range strings.Split(fmt.Sprint(value), ",") {
				existing, _ := pve.ConfigString(before, field)
				if !isDiskOptionKey(field) || strings.Split(existing, ",")[0] != volume {
					return fmt.Errorf("configuration deletion affects another resource")
				}
			}
			continue
		}
		if isDiskOptionKey(key) && strings.Split(fmt.Sprint(value), ",")[0] == volume {
			continue
		}
		return fmt.Errorf("configuration mutation affects an unrelated field")
	}
	return nil
}

func (g *managedDiskLifecycleGuard) after(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
	m := g.lifecycle
	observation, ok := g.observations[step]
	if !ok {
		return fmt.Errorf("missing lifecycle mutation observation")
	}
	key := call.Service + "." + call.Method
	async := key == "QEMU.Create" || key == "QEMU.ResizeDisk" || key == "QEMU.Snapshot" || key == "QEMU.DeleteSnapshot" || key == "Nodes.CreateQemuMoveDisk" || key == "Nodes.CreateQemuMigrate" || key == "Nodes.DeleteQemu" || strings.HasSuffix(call.Method, "Async")
	if key == "Storage.DeleteVolumeIfExistsAsync" {
		values, ok := result.([]any)
		if !ok || len(values) != 2 {
			return fmt.Errorf("invalid conditional deletion result")
		}
		existed, ok := values[0].(bool)
		if !ok {
			return fmt.Errorf("invalid conditional deletion verdict")
		}
		if !existed && observation.preAbsent {
			async = false
		}
		result = values[1]
	}
	if async {
		upid, err := managedMutationUPID(result)
		if err != nil {
			return err
		}
		if err := m.session.Submitted(step, upid); err != nil {
			return err
		}
		if err := pve.AwaitTask(ctx, m.deps.PVE, observation.node, upid); err != nil {
			return fmt.Errorf("managed lifecycle task outcome requires reconciliation")
		}
	}
	volumes, err := g.observeResult(ctx, call, observation, result)
	if err != nil {
		return err
	}
	if err := storageMutationObserved(m.handle, step, volumes, observation.charges); err != nil {
		return err
	}
	delete(g.observations, step)
	return nil
}

func (m *managedDiskLifecycle) diskNode() string {
	if m.external {
		return m.externalNode
	}
	return m.disk.allocation.provenance.Node
}
func (m *managedDiskLifecycle) diskBacking() string {
	if m.external {
		return m.externalBacking
	}
	return m.disk.allocation.provenance.Backing
}

func (g *managedDiskLifecycleGuard) prepareHolder(call ManagedAllocationMutation, observation *managedDiskMutationObservation, backing string) (err error) {
	m := g.lifecycle
	node := observation.node
	params, ok := call.Args["params"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid holder creation parameters")
	}
	provenance := map[string]any{"disk_cid": m.disk.diskCID, "volid": m.disk.volid, resourceTypeNode: node}
	if !m.external {
		provenance["allocation_id"] = m.handle.Record().ID
		provenance["allocation_namespace"] = m.handle.Record().Namespace
		provenance["allocation_backing"] = backing
	}
	entry, err := json.Marshal(map[string]any{m.disk.stableID: provenance})
	if err != nil {
		return err
	}
	description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": entry})
	if err != nil {
		return err
	}
	params[pveConfigKeyDescription] = description
	for field := range params {
		if isDiskOptionKey(field) {
			return fmt.Errorf("holder creation cannot allocate an additional disk")
		}
	}
	observation.fields = params

	return nil
}

func (g *managedDiskLifecycleGuard) prepareAttachment(call ManagedAllocationMutation, observation *managedDiskMutationObservation) (err error) {
	m := g.lifecycle
	volume, _ := call.Args["volid"].(string)
	if strings.Split(volume, ",")[0] != m.disk.volid {
		return fmt.Errorf("attachment refers to another managed volume")
	}
	opts, ok := call.Args["opts"].(*qemu.AttachOpts)
	if !ok || opts == nil {
		return fmt.Errorf("managed attachment requires explicit options")
	}
	if opts.DiskID == "" {
		bus, _ := call.Args["bus"].(string)
		opts.DiskID = fmt.Sprintf("%s%d", bus, qemu.NextIndexForBus(observation.before, bus))
	}
	if old, exists := pve.ConfigString(observation.before, opts.DiskID); exists && strings.Split(old, ",")[0] != m.disk.volid {
		return fmt.Errorf("attachment slot is occupied by another volume")
	}
	digest, ok := pve.ConfigString(observation.before, "digest")
	if !ok || digest == "" {
		return fmt.Errorf("managed attachment requires config generation digest")
	}
	if opts.Extra == nil {
		opts.Extra = map[string]any{}
	}
	if err := lifecycleValidateConfigMutation(observation.before, opts.Extra, m.disk.volid); err != nil {
		return err
	}
	opts.Extra["digest"] = digest
	observation.fields = map[string]any{opts.DiskID: volume}

	return nil
}

func (g *managedDiskLifecycleGuard) prepareResize(ctx context.Context, call ManagedAllocationMutation, observation *managedDiskMutationObservation, storage string, backing string) (charges []inv.ChargeRecord, err error) {
	m := g.lifecycle
	node := observation.node
	key := call.Service + "." + call.Method
	slot, _ := call.Args["diskID"].(string)
	if err := managedAttachedVolume(observation.before, slot, m.disk.volid, m.disk.stableID); err != nil {
		return nil, err
	}
	if key == "QEMU.ResizeDisk" {
		drive, _ := pve.ConfigString(observation.before, slot)
		size, err := parseDiskSizeGiB(drive)
		if err != nil {
			return nil, err
		}
		delta, err := lifecycleInt(call.Args["sizeGiB"])
		if err != nil || size <= 0 || delta <= 0 || delta > math.MaxInt-size {
			return nil, fmt.Errorf("invalid resize capacity delta")
		}
		observation.targetGiB = size + delta
		if delta <= 0 || delta > math.MaxInt64/(1<<30) {
			return nil, fmt.Errorf("resize charge overflow")
		}
		charges, err = managedDiskLifecycleCapacity(ctx, m.deps, m.handle.Record(), node, storage, backing, uint64(delta)<<30)
		if err != nil {
			return nil, err
		}
		observation.charges = true
	}

	return charges, nil
}

func (g *managedDiskLifecycleGuard) prepareSnapshot(observation *managedDiskMutationObservation) (err error) {
	m := g.lifecycle
	if !lifecycleConfigHasVolume(observation.before, m.disk.volid) {
		return fmt.Errorf("snapshot holder no longer owns managed disk")
	}

	return nil
}

func (g *managedDiskLifecycleGuard) prepareConfig(call ManagedAllocationMutation, observation *managedDiskMutationObservation) (err error) {
	m := g.lifecycle
	observation.fields, err = lifecycleMutationFields(call.Args["params"])
	if err != nil {
		return err
	}
	if err := lifecycleValidateConfigMutation(observation.before, observation.fields, m.disk.volid); err != nil {
		return err
	}
	params, ok := call.Args["params"].(*sdknodes.UpdateQemuConfigParams)
	if !ok || params == nil {
		return fmt.Errorf("managed config mutation requires typed parameters")
	}
	digest, ok := pve.ConfigString(observation.before, "digest")
	if !ok || digest == "" {
		return fmt.Errorf("managed config mutation requires generation digest")
	}
	if params.Digest != nil && *params.Digest != digest {
		return fmt.Errorf("managed config generation changed")
	}
	params.Digest = &digest

	return nil
}

func (g *managedDiskLifecycleGuard) prepareMove(ctx context.Context, call ManagedAllocationMutation, observation *managedDiskMutationObservation) (err error) {
	m := g.lifecycle
	node := observation.node
	observation.fields, err = lifecycleMutationFields(call.Args["params"])
	if err != nil {
		return err
	}
	slot, _ := observation.fields["disk"].(string)
	value, _ := pve.ConfigString(observation.before, slot)
	if strings.Split(value, ",")[0] != m.disk.volid {
		return fmt.Errorf("move source no longer owns managed disk")
	}
	observation.targetVMID, err = lifecycleInt(observation.fields["target-vmid"])
	if err != nil {
		return err
	}
	observation.targetSlot, _ = observation.fields["target-disk"].(string)
	if observation.targetSlot == "" {
		return fmt.Errorf("move requires exact receiving slot")
	}
	target, err := m.deps.PVE.QEMU().Config(ctx, node, observation.targetVMID)
	if err != nil || target == nil {
		return fmt.Errorf("move receiver cannot be verified")
	}
	if value, ok := pve.ConfigString(target, observation.targetSlot); ok && value != "" {
		return fmt.Errorf("move receiver slot already occupied")
	}
	params, ok := call.Args["params"].(*sdknodes.CreateQemuMoveDiskParams)
	if !ok || params == nil {
		return fmt.Errorf("managed move requires typed parameters")
	}
	digest, sourceOK := pve.ConfigString(observation.before, "digest")
	targetDigest, targetOK := pve.ConfigString(target, "digest")
	if !sourceOK || !targetOK || digest == "" || targetDigest == "" {
		return fmt.Errorf("managed move requires both configuration generation digests")
	}
	params.Digest = &digest
	params.TargetDigest = &targetDigest

	return nil
}

func (g *managedDiskLifecycleGuard) prepareMigration(ctx context.Context, call ManagedAllocationMutation, observation *managedDiskMutationObservation, storage string, backing string) (charges []inv.ChargeRecord, err error) {
	m := g.lifecycle
	node := observation.node
	observation.fields, err = lifecycleMutationFields(call.Args["params"])
	if err != nil {
		return nil, err
	}
	observation.targetNode, _ = observation.fields["target"].(string)
	if observation.targetNode == "" || observation.targetNode == node {
		return nil, fmt.Errorf("migration requires distinct target node")
	}
	for field := range observation.before {
		if !isDiskOptionKey(field) {
			continue
		}
		value, _ := pve.ConfigString(observation.before, field)
		bare := strings.Split(value, ",")[0]
		if bare != "" && bare != "none" && bare != "cdrom" && bare != m.disk.volid {
			return nil, fmt.Errorf("managed disk migration would move another resource")
		}
	}
	if !lifecycleConfigHasVolume(observation.before, m.disk.volid) {
		return nil, fmt.Errorf("migration holder lacks managed disk")
	}
	continuity, err := pve.ObserveStorageClusterIdentity(ctx, m.deps.PVE.Nodes(), []string{observation.targetNode})
	if err != nil || continuity.ID() != m.handle.Record().ClusterID {
		return nil, fmt.Errorf("migration target cluster continuity differs")
	}
	definition, err := managedDiskActualDefinition(ctx, m.deps, storage)
	if err != nil {
		return nil, err
	}
	observation.copyLocal = !definition.IsShared()
	if observation.copyLocal {
		if fmt.Sprint(observation.fields["targetstorage"]) != "1" {
			return nil, fmt.Errorf("migration cannot select a different physical storage")
		}
		_, contentName, parseErr := pve.ParseDiskCID(m.disk.volid)
		if parseErr != nil {
			return nil, fmt.Errorf("migration source volume is invalid")
		}
		content, contentErr := m.deps.PVE.Nodes().GetStorageContent(ctx, node, storage, contentName)
		if contentErr != nil || content == nil || content.Size <= 0 {
			return nil, fmt.Errorf("migration source size unavailable")
		}
		bytes := uint64(content.Size)
		charges, err = managedDiskLifecycleCapacity(ctx, m.deps, m.handle.Record(), observation.targetNode, storage, backing, bytes)
		if err != nil {
			return nil, err
		}
		observation.charges = true
	}

	return charges, nil
}

func (g *managedDiskLifecycleGuard) prepareDeletion(ctx context.Context, call ManagedAllocationMutation, observation *managedDiskMutationObservation, storage string) (err error) {
	m := g.lifecycle
	node := observation.node
	actualStorage, _ := call.Args["storageName"].(string)
	volume, _ := call.Args["volume"].(string)
	if actualStorage != storage || volume != m.disk.volid {
		return fmt.Errorf("delete refers to an unrelated volume")
	}
	current, identityErr := resolveDiskForOp(ctx, m.deps, "delete_disk_pre_submission", m.disk.diskCID, m.disk.birth, m.disk.meta)
	if identityErr != nil || current.intent != nil || current.holder != nil {
		return fmt.Errorf("storage deletion requires a volume with no remaining guest references")
	}
	exists, err := managedVolumePresent(ctx, m.deps, node, volume)
	if err != nil {
		return fmt.Errorf("cannot verify delete target before submission")
	}
	observation.preAbsent = !exists

	return nil
}

func (g *managedDiskLifecycleGuard) prepareHolderDeletion(observation *managedDiskMutationObservation) (err error) {
	if !g.created[observation.vmid] || lifecycleConfigHasAnyVolume(observation.before) {
		return fmt.Errorf("cleanup cannot delete a foreign or nonempty holder")
	}

	return nil
}

func (g *managedDiskLifecycleGuard) observeResult(ctx context.Context, call ManagedAllocationMutation, observation managedDiskMutationObservation, result any) ([]string, error) {
	key := call.Service + "." + call.Method
	switch {
	case call.Service == managedDiskServicePool:
		return g.observePool(ctx, call)
	case call.Service == managedDiskServiceStorage:
		return g.observeStorage(ctx, call, observation)
	case key == "Nodes.CreateQemuMigrate":
		return g.observeMigration(ctx, observation)
	case key == "Nodes.DeleteQemu":
		return g.observeHolderDeletion(ctx, observation)
	default:
		return g.observeConfigResult(ctx, call, observation, result)
	}
}
func (g *managedDiskLifecycleGuard) observePool(ctx context.Context, call ManagedAllocationMutation) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	pool, _ := call.Args["poolID"].(string)
	comment, found, err := m.deps.PVE.Pools().GetPoolComment(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("cannot verify lifecycle lock mutation")
	}
	if call.Method == "CreatePool" {
		want, _ := call.Args["comment"].(string)
		if !found || comment != want {
			return nil, fmt.Errorf("lifecycle lock readback mismatch")
		}
	} else if found {
		return nil, fmt.Errorf("lifecycle lock deletion not observed")
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeStorage(ctx context.Context, call ManagedAllocationMutation, observation managedDiskMutationObservation) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	volume, _ := call.Args["volume"].(string)
	exists, err := managedVolumePresent(ctx, m.deps, observation.node, volume)
	if err != nil || exists {
		return nil, fmt.Errorf("managed disk deletion is not observed")
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeMigration(ctx context.Context, observation managedDiskMutationObservation) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	source, sourceErr := m.deps.PVE.QEMU().Config(ctx, observation.node, observation.vmid)
	if sourceErr == nil || !pve.IsNotFound(sourceErr) || source != nil {
		return nil, fmt.Errorf("migration source holder absence not observed")
	}
	target, err := m.deps.PVE.QEMU().Config(ctx, observation.targetNode, observation.vmid)
	if err != nil || target == nil {
		return nil, fmt.Errorf("migration target holder unavailable")
	}
	landed := ""
	for field := range target {
		if !isDiskOptionKey(field) {
			continue
		}
		value, _ := pve.ConfigString(target, field)
		token, found := pve.StableIDFromDriveOptStr(value)
		if found && token == m.disk.stableID {
			if landed != "" {
				return nil, fmt.Errorf("migration target disk identity ambiguous")
			}
			landed = strings.Split(value, ",")[0]
		}
	}
	storage, _, err := pve.ParseDiskCID(landed)
	if err != nil {
		return nil, fmt.Errorf("migration target disk identity missing")
	}
	backing, err := managedDiskActualBacking(ctx, m.deps, storage)
	if err != nil || backing != m.diskBacking() {
		return nil, fmt.Errorf("migration target backing differs")
	}
	exists, err := managedVolumePresent(ctx, m.deps, observation.targetNode, landed)
	if err != nil || !exists {
		return nil, fmt.Errorf("migration target volume absent")
	}
	if observation.copyLocal {
		exists, err := managedVolumePresent(ctx, m.deps, observation.node, m.disk.volid)
		if err != nil || exists {
			return nil, fmt.Errorf("migration source volume disposition unknown")
		}
	}
	volumes = append(volumes, landed)
	m.disk.volid = landed
	if m.external {
		m.externalNode = observation.targetNode
	} else {
		m.disk.allocation.provenance.Node = observation.targetNode
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeHolderDeletion(ctx context.Context, observation managedDiskMutationObservation) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	cfg, err := m.deps.PVE.QEMU().Config(ctx, observation.node, observation.vmid)
	if err == nil || !pve.IsNotFound(err) || cfg != nil {
		return nil, fmt.Errorf("temporary holder deletion is not observed")
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeConfigResult(ctx context.Context, call ManagedAllocationMutation, observation managedDiskMutationObservation, result any) ([]string, error) {
	m := g.lifecycle
	key := call.Service + "." + call.Method
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	cfg, err := m.deps.PVE.QEMU().Config(ctx, observation.node, observation.vmid)
	if err != nil || cfg == nil {
		return nil, fmt.Errorf("cannot read lifecycle mutation result")
	}

	switch key {
	case "QEMU.Create":
		return g.observeHolder(observation, cfg)
	case "QEMU.AttachDisk":
		return g.observeAttachment(observation, result, cfg)
	case "QEMU.DetachDisk":
		return g.observeDetach(ctx, call, observation, cfg)
	case "QEMU.ResizeDisk":
		return g.observeResize(call, observation, cfg)
	case "QEMU.Snapshot", "QEMU.DeleteSnapshot":
		return g.observeSnapshot(ctx, call, observation)
	case "Nodes.UpdateQemuConfig":
		return g.observeConfigFields(observation, cfg)
	case "Nodes.CreateQemuMoveDisk":
		return g.observeMove(ctx, observation, cfg)
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeHolder(observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	fields := maps.Clone(observation.fields)
	delete(fields, "vmid")
	if _, present := cfg["onboot"]; !present && managedDiskScalar(fields["onboot"]) == "0" {
		delete(fields, "onboot")
	}
	if err := managedConfigFieldsMatch(cfg, fields); err != nil {
		return nil, err
	}
	g.created[observation.vmid] = true

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeAttachment(observation managedDiskMutationObservation, result any, cfg map[string]any) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	slot, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("attachment did not return its slot")
	}
	if err := managedAttachedVolume(cfg, slot, m.disk.volid, m.disk.stableID); err != nil {
		return nil, err
	}
	if err := managedConfigFieldsMatch(cfg, observation.fields); err != nil {
		return nil, err
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeDetach(ctx context.Context, call ManagedAllocationMutation, observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	slot, _ := call.Args["diskID"].(string)
	if _, exists := cfg[slot]; exists {
		return nil, fmt.Errorf("disk detach not observed")
	}
	if m.session.operation != "delete_disk" {
		exists, err := managedVolumePresent(ctx, m.deps, observation.node, m.disk.volid)
		if err != nil || !exists {
			return nil, fmt.Errorf("detached managed disk was not retained")
		}
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeResize(call ManagedAllocationMutation, observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	slot, _ := call.Args["diskID"].(string)
	if err := managedAttachedVolume(cfg, slot, m.disk.volid, m.disk.stableID); err != nil {
		return nil, err
	}
	value, _ := pve.ConfigString(cfg, slot)
	size, err := parseDiskSizeGiB(value)
	if err != nil || size < observation.targetGiB {
		return nil, fmt.Errorf("resize target size not observed")
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeSnapshot(ctx context.Context, call ManagedAllocationMutation, observation managedDiskMutationObservation) ([]string, error) {
	m := g.lifecycle
	key := call.Service + "." + call.Method
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	name, _ := call.Args["name"].(string)
	entries, err := m.deps.PVE.QEMU().ListSnapshots(ctx, observation.node, observation.vmid)
	if err != nil {
		return nil, fmt.Errorf("snapshot readback unavailable")
	}
	found := false
	for _, entry := range entries {
		actual, _ := pve.ConfigString(entry, "name")
		if actual == name {
			state, _ := pve.ConfigString(entry, "snapstate")
			if state != "" {
				return nil, fmt.Errorf("snapshot outcome unsettled")
			}
			found = true
		}
	}
	if found != (key == "QEMU.Snapshot") {
		return nil, fmt.Errorf("snapshot mutation not observed")
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeConfigFields(observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	if err := managedConfigFieldsMatch(cfg, observation.fields); err != nil {
		return nil, err
	}

	return volumes, nil
}

func (g *managedDiskLifecycleGuard) observeMove(ctx context.Context, observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	m := g.lifecycle
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	sourceSlot, _ := observation.fields["disk"].(string)
	if _, exists := cfg[sourceSlot]; exists {
		return nil, fmt.Errorf("move source slot still occupied")
	}
	target, err := m.deps.PVE.QEMU().Config(ctx, observation.node, observation.targetVMID)
	if err != nil || target == nil {
		return nil, fmt.Errorf("move receiver readback unavailable")
	}
	value, _ := pve.ConfigString(target, observation.targetSlot)
	landed := strings.Split(value, ",")[0]
	storage, _, err := pve.ParseDiskCID(landed)
	if err != nil {
		return nil, fmt.Errorf("move receiving slot lacks volume")
	}
	backing, err := managedDiskActualBacking(ctx, m.deps, storage)
	if err != nil || backing != m.diskBacking() {
		return nil, fmt.Errorf("move changed physical storage backing")
	}
	volumes = append(volumes, landed)
	m.disk.volid = landed
	return volumes, nil
}
