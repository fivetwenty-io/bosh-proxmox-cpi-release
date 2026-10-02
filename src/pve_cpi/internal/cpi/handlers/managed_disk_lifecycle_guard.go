package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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
	preAbsent bool
	copyLocal bool
	node      string
	vmid      int
	before    map[string]any
	// pendingDeletes names the keys of the holder whose delete PVE recorded
	// as pending when the guard read it before a config write. Their current
	// values are in before too, because the config endpoint hides them while
	// the running guest still has the disk.
	pendingDeletes map[string]bool
	fields         map[string]any
	targetNode     string
	targetVMID     int
	targetSlot     string
	targetGiB      int
	charges        bool
	// ontoParker records that a move's receiving VM is a parker and not a
	// mover. Only such a move settles a snapshot refusal.
	ontoParker bool
	// notesRemoval records that a description-only write carries its
	// caller's digest and only removes notes from the description the guard
	// read, the way pve.RemoveDescriptionNotes builds it.
	notesRemoval bool
}

type managedDiskLifecycleGuard struct {
	lifecycle    *managedDiskLifecycle
	observations map[string]managedDiskMutationObservation
	created      map[int]bool
	// pendingDeleteSettled records that a delete this operation sent stayed
	// pending and was settled as not applied, so the revert that follows it is
	// the pending-delete helper's own.
	pendingDeleteSettled bool
}

func newManagedDiskLifecycleGuard(m *managedDiskLifecycle) (*ManagedAllocationGuard, error) {
	state := &managedDiskLifecycleGuard{lifecycle: m, observations: map[string]managedDiskMutationObservation{}, created: map[int]bool{}}
	m.holders = state
	return NewManagedAllocationGuard(m.deps.PVE, ManagedAllocationHooks{Before: state.before, After: state.after, Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
		return m.session.Uncertain(call.Service + "." + call.Method)
	}, SettleProtectionWrites: true, SettleFailedWrite: state.settleFailedWrite})
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
	if err := m.requestEndedRefusal(ctx, call); err != nil {
		return "", err
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
	backing, err := g.observeContinuity(ctx, call, node, storage)
	if err != nil {
		return "", err
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
		if err := g.readHolderState(ctx, call, key, node, storage, &observation); err != nil {
			return "", err
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
	// A protection-only configuration write records what it wrote, so a
	// restore cut off by its deadline can be settled later by reading the
	// parker back (settlePlannedProtectionSteps). Every other write records
	// no parameters, as before.
	step, err := storageMutationIntent(m.handle, "lifecycle_"+m.session.operation+"_"+call.Service+"_"+call.Method, target, charges, lifecycleStepParameters(key, observation.fields))
	if err != nil {
		return "", err
	}
	g.observations[step] = observation
	if lifecycleMutationTouchesDisk(call, observation) {
		// Only a sentinel create or delete, or a write of the drive-option
		// overlay note alone, leaves the disk and its holders alone. Anything
		// else admitted here may have moved, migrated, or rewritten the disk,
		// which a later lock timeout must not paper over.
		m.diskMutationAdmitted = true
	}
	return step, nil
}

// observeContinuity reads the disk's actual backing and the cluster's identity
// and checks both against the record, returning the backing. A protection
// restore whose read returns an error is not admitted, and it does not lock
// the guard either (see restoreChecksIncomplete). A read that finishes with an
// answer that disagrees still locks it, as it does for every other mutation.
func (g *managedDiskLifecycleGuard) observeContinuity(ctx context.Context, call ManagedAllocationMutation, node, storage string) (string, error) {
	m := g.lifecycle
	backing, err := managedDiskActualBacking(ctx, m.deps, storage)
	if err != nil && isProtectionRestore(call) {
		return "", g.restoreChecksIncomplete(call, node, storage, "managed disk backing")
	}
	if err != nil || backing != m.diskBacking() {
		return "", fmt.Errorf("managed disk backing changed before mutation")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, m.deps.PVE.Nodes(), []string{node})
	if err != nil && isProtectionRestore(call) {
		return "", g.restoreChecksIncomplete(call, node, storage, "managed disk cluster identity")
	}
	if err != nil || identity.ID() != m.handle.Record().ClusterID {
		return "", fmt.Errorf("managed disk cluster continuity changed before mutation")
	}
	return backing, nil
}

// readHolderState reads what the guard checks a mutation against on the VM it
// targets. Every mutation but a holder create reads the holder's config, and a
// config write that deletes or reverts a key also reads its pending view.
func (g *managedDiskLifecycleGuard) readHolderState(ctx context.Context, call ManagedAllocationMutation, key, node, storage string, observation *managedDiskMutationObservation) error {
	if key == "QEMU.Create" {
		return nil
	}
	before, err := g.readHolderConfig(ctx, call, node, storage, observation.vmid)
	if err != nil {
		return err
	}
	observation.before = before
	if key == managedVMCallUpdateConfig && configWriteDeletesOrReverts(call) {
		return g.addPendingDeletes(ctx, node, observation)
	}
	return nil
}

// readHolderConfig reads the configuration of the VM a mutation targets. As in
// observeContinuity, a protection restore whose read returns an error ends in
// restoreChecksIncomplete, and a read that returns nothing still locks the
// guard.
func (g *managedDiskLifecycleGuard) readHolderConfig(ctx context.Context, call ManagedAllocationMutation, node, storage string, vmid int) (map[string]any, error) {
	config, err := g.lifecycle.deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil && isProtectionRestore(call) {
		return nil, g.restoreChecksIncomplete(call, node, storage, "parker configuration")
	}
	if err != nil || config == nil {
		return nil, fmt.Errorf("cannot verify lifecycle holder before mutation")
	}
	return config, nil
}

// addPendingDeletes reads the holder's pending view before a config write and
// adds every key whose delete is pending to the observation, with its current
// value merged into before. A slot delete that PVE could only record as pending
// is then still the managed volume's slot when the delete is sent again, or
// when its revert is sent.
func (g *managedDiskLifecycleGuard) addPendingDeletes(ctx context.Context, node string, observation *managedDiskMutationObservation) error {
	views, err := pve.ReadQemuViews(ctx, g.lifecycle.deps.PVE, node, observation.vmid)
	if err != nil {
		return fmt.Errorf("cannot verify lifecycle holder pending changes before mutation")
	}
	current := views.Current()
	for key := range current {
		if !views.PendingDelete(key) {
			continue
		}
		if observation.pendingDeletes == nil {
			observation.pendingDeletes = map[string]bool{}
		}
		observation.pendingDeletes[key] = true
		if _, present := observation.before[key]; !present {
			observation.before[key] = current[key]
		}
	}
	return nil
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
	if _, refused := result.(managedProtectionWriteRefusal); refused {
		// PVE refused a protection-only write, so nothing changed and there is
		// nothing to read back. The guard hands the refusal here only after
		// classifyProtectionWriteFailure matched the write.
		if key != "Nodes.UpdateQemuConfig" || !isParkerProtectionParameters(parkerProtectionStepParameters(observation.fields)) {
			return fmt.Errorf("protection refusal names another mutation")
		}
		if err := storageMutationObserved(m.handle, step, nil, false); err != nil {
			return err
		}
		delete(g.observations, step)
		return nil
	}
	if _, refused := result.(managedMoveDigestRefusal); refused {
		if key != "Nodes.CreateQemuMoveDisk" {
			return fmt.Errorf("move refusal names another mutation")
		}
		return g.settleRefusedMove(ctx, step, observation)
	}
	if _, refused := result.(managedMoveSnapshotRefusal); refused {
		return g.settleSnapshotRefusedMove(ctx, key, step, observation)
	}
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
			if key == "Nodes.CreateQemuMoveDisk" && pve.IsMoveDigestRefusal(err) {
				// The move task checked both digests under the configuration
				// locks and refused before its rename, so nothing moved.
				return g.settleRefusedMove(ctx, step, observation)
			}
			if observation.ontoParker && pve.IsMoveSnapshotRefusalTaskExit(err) {
				// The move task found the volume still in use when it checked
				// again under the configuration locks, and it refused before
				// its rename, so nothing moved onto the parker.
				return g.settleSnapshotRefusedMove(ctx, key, step, observation)
			}
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
	if err := lifecycleValidateRevert(observation, m.disk.volid); err != nil {
		return err
	}
	if err := lifecycleValidateConfigMutation(observation.before, withoutRevert(observation.fields), m.disk.volid); err != nil {
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
		if descriptionOnlyConfigWrite(call) {
			return errManagedDescriptionDigestStale
		}
		return fmt.Errorf("managed config generation changed")
	}
	if params.Digest != nil && params.Description != nil && descriptionOnlyConfigWrite(call) {
		_, observation.notesRemoval = pve.DescriptionNotesRemoved(pve.DescriptionFromConfig(observation.before), *params.Description)
	}
	params.Digest = &digest

	return nil
}

// lifecycleValidateRevert admits a revert only of keys whose delete PVE
// recorded as pending and whose current value is the managed volume, which is
// the revert the pending-delete helper sends after one of our own slot deletes.
func lifecycleValidateRevert(observation *managedDiskMutationObservation, volume string) error {
	text, present := observation.fields["revert"]
	if !present {
		return nil
	}
	for _, key := range strings.Split(fmt.Sprint(text), ",") {
		existing, _ := pve.ConfigString(observation.before, key)
		if !isDiskOptionKey(key) || !observation.pendingDeletes[key] || strings.Split(existing, ",")[0] != volume {
			return fmt.Errorf("configuration revert affects another resource")
		}
	}
	return nil
}

// withoutRevert returns fields without the revert key, which
// lifecycleValidateRevert has already checked.
func withoutRevert(fields map[string]any) map[string]any {
	if _, present := fields["revert"]; !present {
		return fields
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		if key != "revert" {
			out[key] = value
		}
	}
	return out
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
	tags, _ := pve.ConfigString(target, "tags")
	observation.ontoParker = pve.IsParkerVM(observation.targetVMID, tags, parkerReadConfigFor(m.deps)) && !pve.TagsMarkDiskMover(tags)
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
	if identityErr != nil || current.intent != nil || current.holder != nil || len(current.unused) > 0 {
		return fmt.Errorf("storage deletion requires a volume with no remaining guest references")
	}
	exists, err := managedVolumePresent(ctx, m.deps, node, volume)
	if err != nil {
		return fmt.Errorf("cannot verify delete target before submission")
	}
	observation.preAbsent = !exists

	return nil
}

// createdHolder reports whether this operation created the VM with this VMID
// and read it back, which is what prepareHolderDeletion asks before it lets
// the operation delete a holder. It answers only who created the VM. Whether
// the VM is empty is left to the caller's own check and to
// prepareHolderDeletion.
//
// It reads created under the allocation guard's mutex. That can't deadlock
// only because the handler asks between guarded calls and never from inside a
// hook. begin takes the mutex when it admits a call and end releases it, and
// every hook runs while it is held, so a hook that asked would wait on itself.
func (g *managedDiskLifecycleGuard) createdHolder(vmid int) bool {
	guard := g.lifecycle.guard
	if guard == nil {
		return false
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return g.created[vmid]
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
		return g.observePool(ctx, call, result)
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
func (g *managedDiskLifecycleGuard) observePool(ctx context.Context, call ManagedAllocationMutation, result any) ([]string, error) {
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	if err := observeLockPoolMutation(ctx, g.lifecycle.deps.PVE.Pools(), call, result, "lifecycle"); err != nil {
		return nil, err
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
	if key == managedVMCallUpdateConfig && observation.notesRemoval {
		// PVE checks the digest before it writes, so a pinned removal that it
		// answered with success has landed. Any change to the description
		// since then is another writer's, so the guard doesn't read it back.
		// The caller checks that the notes it removed are gone, and a note
		// that is back is retriable rather than an unknown outcome.
		return volumes, nil
	}
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
		return g.observeConfigWrite(ctx, observation, cfg)
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
	if err := managedConfigFieldsMatch(cfg, withoutRevert(observation.fields)); err != nil {
		return nil, err
	}

	return volumes, nil
}

// observeConfigWrite observes a config write. A write that deletes or reverts
// a key is checked against the pending view too, which the config read back
// afterwards can't show.
//
// A delete PVE could only record as pending is missing from that config while
// the running guest still has the disk, so it is never observed as a delete.
// The write is settled with no volume instead, the way a refused protection
// write is, because PVE applied nothing the journal has to account for, and
// the pending-delete helper reverts it with a write of its own straight after.
// A revert is observed when its key is back in the config, naming the managed
// volume, with no pending delete left.
func (g *managedDiskLifecycleGuard) observeConfigWrite(ctx context.Context, observation managedDiskMutationObservation, cfg map[string]any) ([]string, error) {
	_, hasDelete := observation.fields["delete"]
	reverted, hasRevert := observation.fields["revert"]
	if !hasDelete && !hasRevert {
		return g.observeConfigFields(observation, cfg)
	}
	views, err := pve.ReadQemuViews(ctx, g.lifecycle.deps.PVE, observation.node, observation.vmid)
	if err != nil {
		return nil, fmt.Errorf("cannot read lifecycle mutation pending result")
	}
	if hasRevert {
		for _, key := range strings.Split(fmt.Sprint(reverted), ",") {
			value, _ := pve.ConfigString(cfg, key)
			if views.PendingDelete(key) || strings.Split(value, ",")[0] != g.lifecycle.disk.volid {
				return nil, fmt.Errorf("VM config revert not observed for %s", key)
			}
		}
	}
	// A write that carries a revert and no other field has nothing left for
	// the readback to match, and the revert check above has already observed
	// it. Every other write, an empty one included, goes through the matcher.
	if rest := withoutRevert(observation.fields); !hasRevert || len(rest) > 0 {
		err = managedConfigFieldsMatchPending(cfg, views.PendingDelete, rest)
	}
	if errors.Is(err, errManagedConfigDeletePending) {
		// Settled as not applied only when the pending delete is the whole
		// change, with each slot's current value still what the guard read
		// before the write.
		deleted := strings.Split(fmt.Sprint(observation.fields["delete"]), ",")
		if !pendingDeleteOnlyChange(observation.before, views, deleted) {
			return nil, fmt.Errorf("VM config deletion is pending alongside another change")
		}
		g.pendingDeleteSettled = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hasRevert && g.pendingDeleteSettled {
		g.lifecycle.pendingDeleteReverted = true
	}
	volumes := make([]string, 1, 2)
	volumes[0] = g.lifecycle.disk.volid
	return volumes, nil
}

// managedMoveDigestRefusal is the After result for a move PVE refused in the
// request because a configuration changed after its digest was read.
type managedMoveDigestRefusal struct{}

// managedMoveSnapshotRefusal is the After result for a move PVE refused in
// the request because a snapshot or another drive key still names the volume.
type managedMoveSnapshotRefusal struct{}

// settleSnapshotRefusedMove settles a move that PVE refused because a
// snapshot still names the volume. Only a move onto a parker settles, and any
// other move keeps the guard's failure path.
func (g *managedDiskLifecycleGuard) settleSnapshotRefusedMove(ctx context.Context, key, step string, observation managedDiskMutationObservation) error {
	if key != "Nodes.CreateQemuMoveDisk" {
		return fmt.Errorf("move refusal names another mutation")
	}
	if !observation.ontoParker {
		// A move off a parker goes on to a fallback that can edit the
		// volume onto the VM while a parker snapshot still names it, so
		// the refusal stays uncertain there.
		return fmt.Errorf("snapshot refusal settles only a move onto a parker")
	}
	return g.settleRefusedMove(ctx, step, observation)
}

// settleRefusedMove records a move that PVE refused before its rename as
// refused, once a readback of both views shows the source still naming the
// managed disk on the same key and the receiving slot still empty. The step
// goes to the journal's Observed state holding only the pre-move volume. A
// readback that shows anything else returns an error, and the guard treats the
// move as uncertain.
func (g *managedDiskLifecycleGuard) settleRefusedMove(ctx context.Context, step string, observation managedDiskMutationObservation) error {
	m := g.lifecycle
	sourceSlot, _ := observation.fields["disk"].(string)
	source, err := pve.ReadQemuViews(ctx, m.deps.PVE, observation.node, observation.vmid)
	if err != nil {
		return fmt.Errorf("refused move source readback unavailable")
	}
	if !pve.MoveSourceStillNames(source, sourceSlot, m.disk.volid) {
		return fmt.Errorf("refused move source no longer names managed disk")
	}
	target, err := pve.ReadQemuViews(ctx, m.deps.PVE, observation.node, observation.targetVMID)
	if err != nil {
		return fmt.Errorf("refused move receiver readback unavailable")
	}
	if !pve.MoveSlotEmpty(target, observation.targetSlot) {
		return fmt.Errorf("refused move receiving slot is occupied")
	}
	if err := storageMutationObserved(m.handle, step, []string{m.disk.volid}, false); err != nil {
		return err
	}
	delete(g.observations, step)
	return nil
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

// errManagedRequestEnded marks a lifecycle mutation that the guard refused
// because the request's own context had already ended. Nothing reached PVE, so
// begin hands the refusal back as not attempted and leaves the guard usable,
// and cleanLockTimeout can count the operation clean when nothing else was
// admitted and every step is settled.
var errManagedRequestEnded = errors.New("managed lifecycle request ended before mutation")

// errManagedRestoreChecksIncomplete marks a parker protection restore that
// before did not admit because one of its reads returned an error. before has
// already recorded the restore as a planned step, so begin hands the refusal
// back unchanged and leaves the guard usable.
var errManagedRestoreChecksIncomplete = errors.New("parker protection restore checks did not finish")

// managedRestoreChecksIncomplete is the refusal before gives a parker
// protection restore when one of its reads returned an error. Its text names
// the check that could not finish. It matches errManagedRestoreChecksIncomplete
// and pve.ErrMutationChecksIncomplete, and on purpose it matches neither the
// read's own error nor the context's. The retry loop around the restore would
// take either one for a transport fault and return the bare context error in
// its place, which would lose the fact that the restore was never sent.
type managedRestoreChecksIncomplete struct{ check string }

func (e *managedRestoreChecksIncomplete) Error() string {
	return e.check + " could not be checked before the parker protection restore"
}

func (e *managedRestoreChecksIncomplete) Unwrap() []error {
	return []error{errManagedRestoreChecksIncomplete, pve.ErrMutationChecksIncomplete}
}

// restoreChecksIncomplete ends the admission of a parker protection restore
// whose check could not finish, because one of before's reads returned an
// error. It doesn't matter whether PVE answered with a failure or the
// restore's deadline ended the read. The restore is not sent. Its intent is
// recorded as a planned protection-on step instead, so the next call reads the
// parker back before it takes the disk, and the refusal leaves the guard usable
// (see begin).
//
// Every error from those reads lands here on purpose, including a storage
// missing from the listing, a storage definition that is ambiguous or
// malformed, and a certificate answer that was rejected. None of those reaches
// a verdict on this disk, and treating them like a read that failed costs
// nothing, because the restore isn't sent and the planned step only causes a
// read-back. The attach's next guarded write and every later lifecycle open
// call the same helpers, so they still lock or refuse on that answer before
// anything mutates. Narrowing it to a smaller set of errors would buy no safety.
//
// The intent uses only what the lifecycle already holds. The backing is the
// one the record names, which is the backing every admitted write records,
// and the parameters are rendered from the call's own fields. Nothing comes
// from the read that failed. That is safe because isProtectionRestore has
// proved the write is protection-only and the write is never sent. A backing
// or a cluster that really changed still stops this operation's next guarded
// write, and every later lifecycle open, when they make their own reads.
//
// No observation is stored for the step, because only after reads one and
// after never runs for a write that was not sent. diskMutationAdmitted is left
// alone, because a protection write never touches the disk.
func (g *managedDiskLifecycleGuard) restoreChecksIncomplete(call ManagedAllocationMutation, node, storage, check string) error {
	m := g.lifecycle
	vmid, err := lifecycleInt(call.Args[metadataKeyVMID])
	if err != nil {
		return err
	}
	fields, err := lifecycleMutationFields(call.Args[managedArgumentParams])
	if err != nil {
		return err
	}
	key := call.Service + "." + call.Method
	target := aj.Target{External: m.external && !m.ownedRetention, VirtualBytes: m.retainedBytes, Node: node, Storage: storage, Backing: m.diskBacking(), VMID: vmid, IntendedVolume: m.disk.volid}
	if _, err := storageMutationIntent(m.handle, "lifecycle_"+m.session.operation+"_"+call.Service+"_"+call.Method, target, nil, lifecycleStepParameters(key, fields)); err != nil {
		return err
	}
	return &managedRestoreChecksIncomplete{check: check}
}

// managedRequestEnded is the refusal before gives a mutation on an ended
// request. It is retriable, because nothing changed and the Director may try
// again, and it carries both the context's own error and errManagedRequestEnded.
func managedRequestEnded(cause error) error {
	return cpierrors.WrapAs(errors.Join(cause, errManagedRequestEnded), cpierrors.TypeRetriableCloud,
		"managed lifecycle request ended before mutation")
}

// requestEndedRefusal refuses a mutation on a request whose context has
// ended, except the release of a lock sentinel or a parker protection restore
// on its own live context.
func (m *managedDiskLifecycle) requestEndedRefusal(ctx context.Context, call ManagedAllocationMutation) error {
	if m.requestContext == nil {
		return fmt.Errorf("managed lifecycle request cancelled before mutation")
	}
	cause := m.requestContext.Err()
	if cause == nil {
		return nil
	}
	if ctx.Err() == nil && (isLockSentinelDelete(call) || isProtectionRestore(call)) {
		return nil
	}
	return managedRequestEnded(cause)
}

// isLockSentinelDelete reports whether call deletes a bosh-lock- sentinel. A
// lock the request still holds has to be released after the request's context
// ends, or every other request waits out its TTL. That release runs on its own
// detached, bounded context, and before admits it while that context is live.
// The release deletes only a claim a fresh read proves ours.
func isLockSentinelDelete(call ManagedAllocationMutation) bool {
	pool, _ := call.Args[managedArgumentPoolID].(string)
	return call.Service == managedDiskServicePool && call.Method == "DeletePool" && isManagedLockPool(pool)
}

// isProtectionRestore reports whether call is a configuration write that only
// puts protection back, with or without a digest. A window that cleared a
// parker's protection has to put it back after the request's context ends, or
// the parker stays unprotected until the next window on it. That restore runs
// on its own detached, bounded context, and before admits it while that
// context is live. A write that clears protection or changes anything else is
// still refused.
func isProtectionRestore(call ManagedAllocationMutation) bool {
	if call.Service != managedServiceNodes || call.Method != "UpdateQemuConfig" {
		return false
	}
	fields, err := lifecycleMutationFields(call.Args[managedArgumentParams])
	if err != nil {
		return false
	}
	on, _ := fields[pveConfigKeyProtection].(bool)
	return on && isParkerProtectionParameters(parkerProtectionStepParameters(fields))
}

// settleFailedWrite settles a lifecycle slot delete that PVE answered with an
// error, when PVE recorded the delete as pending before the unplug failed
// busy. qemu-server writes the pending delete and then tries the unplug, so a
// busy guest leaves exactly that pending delete behind. The guard reads both
// views, and when the only change is a pending delete of the slots the write
// named, with each slot's current value still the managed volume, it settles
// the step as not applied and stays usable, so the helper's busy retries and
// its revert still pass. A description-only write that PVE refused for a
// stale digest settles the same way (see settleRefusedDescription). Any other
// readback, or a failed read, leaves the failure to poison the guard as before.
func (g *managedDiskLifecycleGuard) settleFailedWrite(ctx context.Context, call ManagedAllocationMutation, step string, writeErr error) bool {
	if g.settleRefusedDescription(ctx, call, step, writeErr) {
		return true
	}
	deleted := configWriteDeletedSlots(call)
	observation, ok := g.observations[step]
	if len(deleted) == 0 || !ok {
		return false
	}
	views, err := pve.ReadQemuViews(ctx, g.lifecycle.deps.PVE, observation.node, observation.vmid)
	if err != nil || !pendingDeleteOnlyChange(observation.before, views, deleted) {
		return false
	}
	current := views.Current()
	for _, slot := range deleted {
		value, _ := pve.ConfigString(current, slot)
		if strings.Split(value, ",")[0] != g.lifecycle.disk.volid {
			return false
		}
	}
	if err := storageMutationObserved(g.lifecycle.handle, step, nil, false); err != nil {
		return false
	}
	delete(g.observations, step)
	g.pendingDeleteSettled = true
	return true
}
