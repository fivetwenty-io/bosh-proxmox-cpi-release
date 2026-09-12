package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

type managedVMAllocation struct {
	deps                     Deps
	parsed                   *createVMParsedArgs
	shape                    *createVMShape
	prepared                 *managedVMPlan
	handle                   *aj.Handle
	vmid                     int
	marker                   string
	guard                    *ManagedAllocationGuard
	rootCreated, needsMarker bool
	volumes                  map[string]string
	chargeSteps              map[string]string
	chargeIndices            map[string]int
}

func (m *managedVMAllocation) revalidate(ctx context.Context) error {
	cfg := m.deps.Config
	snapshot, ledger, err := RevalidateStorageAllocationPlan(ctx, m.prepared.collector, m.prepared.inventory, m.prepared.inventory, m.prepared.selection, m.prepared.plan, m.prepared.ledger, time.Now, StoragePlanInfrastructurePolicy{OriginalISOStorage: cfg.OriginalISOStorage(), FollowRoot: cfg.ISOStorageFollowVMStorageEnabled(), RequireShared: cfg.RequireSharedISOForHAEnabled()})
	if err != nil {
		return err
	}
	m.prepared.inventory = snapshot
	m.prepared.ledger = ledger
	return nil
}
func (m *managedVMAllocation) newGuard() error {
	if m.guard != nil {
		return m.guard.Err()
	}
	guard, err := NewManagedAllocationGuard(m.deps.PVE, ManagedAllocationHooks{Before: m.beforeMutation, After: m.afterMutation, Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
		return storageAllocationUncertain(m.handle, "VM "+call.Service+"."+call.Method)
	}})
	if err != nil {
		return err
	}
	m.guard = guard
	return nil
}
func (m *managedVMAllocation) beforeMutation(ctx context.Context, call ManagedAllocationMutation) (string, error) {
	if err := m.revalidate(ctx); err != nil {
		return "", err
	}
	method := call.Service + "." + call.Method
	target := aj.Target{Node: m.shape.node, VMID: m.vmid}
	var role string
	switch method {
	case managedVMCallCreate, managedVMCallClone:
		if m.rootCreated {
			return "", fmt.Errorf("VM root creation already submitted")
		}
		role = storageRoleRoot
	case managedVMCallCreateVolume:
		role = storageRoleEphemeral
	case managedVMCallUpload:
		role = storageRoleISO
	case "QEMU.ResizeDisk", managedVMCallResize, managedVMCallAttach, managedVMCallDetach, managedVMCallStart, managedVMCallUpdateConfig, "Nodes.CreateQemuAgentExec", "Nodes.CreateQemuFirewallIpset", "Nodes.CreateQemuFirewallIpset2", "Nodes.CreateQemuFirewallRules", "Nodes.UpdateQemuFirewallOptions", "Cluster.CreateHaResources", "Cluster.UpdateHaResources", "Cluster.CreateHaRules", "Cluster.DeleteHaRules", "Cluster.CreateSdnVnetsSubnets", "Cluster.UpdateSdn", "Pool.AddVM", "Pool.MoveVMToPool", "Pool.CreatePool":
	default:
		return "", fmt.Errorf("unplanned VM allocation mutation %s", method)
	}
	if node, ok := call.Args["node"].(string); ok && node != "" && method != managedVMCallClone && node != m.shape.node {
		return "", fmt.Errorf("VM mutation changed node")
	}
	if value, ok := call.Args["vmid"]; ok && method != managedVMCallClone && fmt.Sprint(value) != strconv.Itoa(m.vmid) {
		return "", fmt.Errorf("VM mutation changed guest identity")
	}
	if err := m.validateMutationTarget(call, role); err != nil {
		return "", err
	}
	if m.rootCreated && !m.needsMarker {
		cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
		if err != nil {
			return "", err
		}
		if err := m.verifyMarker(cfg); err != nil {
			return "", err
		}
	}
	if m.needsMarker && method != managedVMCallUpdateConfig {
		return "", fmt.Errorf("clone provenance must be established before further mutation")
	}
	charges, err := m.prepareMutationCharges(ctx, method, role, &target)
	if err != nil {
		return "", err
	}
	parameters, err := m.infrastructureMutationParameters(ctx, call)
	if err != nil {
		return "", err
	}
	if method == managedVMCallCreateVolume {
		parameters, err = aj.MutationParameters(map[string]any{"version": 1, "kind": "ephemeral_birth", "absence_verified": true})
		if err != nil {
			return "", err
		}
	}
	step, err := storageMutationIntent(m.handle, "vm."+method, target, charges, parameters)
	if err != nil {
		return "", err
	}
	if m.prepared.iterator != nil {
		m.prepared.iterator.MarkSubmitted()
	}
	for index := range charges {
		charge := &charges[index]
		m.chargeSteps[charge.Charge.ID] = step
		m.chargeIndices[charge.Charge.ID] = index
	}
	return step, nil
}

// prepareIntendedVolume freezes deterministic file identities before the mutation
// intent is saved. A planned name alone does not establish volume ownership.
func (m *managedVMAllocation) prepareIntendedVolume(target *aj.Target, role string) error {
	switch role {
	case storageRoleEphemeral:
		definition := m.prepared.plan.Definitions[target.Storage]
		_, suffix, err := pve.ManagedEphemeralVolumeName(definition.Type, m.shape.vmDiskFormat, m.vmid, m.handle.Record().Namespace, m.handle.Record().ID)
		if err != nil {
			return err
		}
		target.IntendedVolume = target.Storage + ":" + suffix
	case storageRoleISO:
		target.IntendedVolume = fmt.Sprintf("%s:iso/vm-%d-config.iso", target.Storage, m.vmid)
	}
	return nil
}

func (m *managedVMAllocation) verifyMarker(cfg map[string]any) error {
	description, _ := pve.ConfigString(cfg, "description")
	marker, found, err := pve.ParseStorageAllocationMarker(description)
	if err != nil || !found {
		return fmt.Errorf("VM allocation provenance missing")
	}
	expected, _, err := pve.ParseStorageAllocationMarker(m.marker)
	if err != nil || marker != expected {
		return fmt.Errorf("VM allocation provenance differs")
	}
	return nil
}
func (m *managedVMAllocation) afterMutation(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
	method := call.Service + "." + call.Method
	switch method {
	case managedVMCallCreate, managedVMCallClone, "QEMU.ResizeDisk", managedVMCallResize, managedVMCallStart, managedVMCallUpload, "Nodes.CreateQemuAgentExec":
		// Agent exec returns a PID, not a task; its exact readback is handled by
		// the guest-execution observer below.
		if method != "Nodes.CreateQemuAgentExec" {
			upid, err := managedMutationUPID(result)
			if err != nil {
				return err
			}
			if err := storageMutationSubmitted(m.handle, step, upid); err != nil {
				return err
			}
			node, _ := call.Args["node"].(string)
			if err := pve.AwaitTask(ctx, m.deps.PVE, node, upid, pve.WithMaxWait(pve.StemcellMaxWait)); err != nil {
				return err
			}
		}
	}
	var volumes []string
	switch method {
	case managedVMCallCreate, managedVMCallClone:
		return m.observeCreatedRoot(ctx, method, step)
	case "QEMU.ResizeDisk", managedVMCallResize:
		target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleRoot)
		volume := m.volumes[storageRoleRoot]
		if _, err := m.observeTargetVolume(ctx, target, volume, target.VirtualBytes); err != nil {
			return err
		}
		if err := m.acquireCharge(ctx, "root_growth", volume); err != nil {
			return err
		}
	case managedVMCallCreateVolume:
		return m.observeEphemeralCreation(ctx, call, step, result)
	case managedVMCallUpload:
		return m.observeISOUpload(ctx, call, step, result)
	case managedVMCallUpdateConfig:
		cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
		if err != nil {
			return err
		}
		if err := managedConfigFieldsMatch(cfg, call.Args["params"]); err != nil {
			return err
		}
		if err := m.verifyMarker(cfg); err != nil {
			return err
		}
		m.needsMarker = false
	case managedVMCallAttach:
		cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
		if err != nil {
			return err
		}
		slot, ok := result.(string)
		if !ok {
			return fmt.Errorf("attachment lacks device identity")
		}
		volume, _ := call.Args["volid"].(string)
		if err := managedAttachedVolume(cfg, slot, strings.Split(volume, ",")[0], ""); err != nil {
			return err
		}
	case managedVMCallDetach:
		cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
		if err != nil {
			return err
		}
		slot, _ := call.Args["diskID"].(string)
		if _, found := cfg[slot]; found {
			return fmt.Errorf("detached disk still attached")
		}
	case managedVMCallStart:
		status, err := m.deps.PVE.QEMU().Status(ctx, m.shape.node, m.vmid)
		if err != nil {
			return err
		}
		if status["status"] != "running" {
			return fmt.Errorf("VM start not observed")
		}
	default:
		if err := m.observePolicyMutation(ctx, call, step, result); err != nil {
			return err
		}
	}
	return storageMutationObserved(m.handle, step, volumes, false)
}
func (m *managedVMAllocation) observeTargetVolume(ctx context.Context, target StoragePlanTarget, volume string, minimum uint64) (uint64, error) {
	storage, bare, err := pve.ParseDiskCID(volume)
	if err != nil || storage != target.StorageID {
		return 0, fmt.Errorf("actual volume differs from frozen target")
	}
	response, err := m.deps.PVE.Nodes().GetStorageContent(ctx, target.Node, storage, bare)
	if err != nil {
		return 0, err
	}
	if response == nil || response.Size <= 0 || uint64(response.Size) < minimum {
		return 0, fmt.Errorf("actual volume size not proven")
	}
	if target.Role == storageRoleISO {
		if (response.Format != "raw" && response.Format != storageRoleISO) || uint64(response.Size) != target.VirtualBytes {
			return 0, fmt.Errorf("actual ISO format or exact size differs from frozen artifact")
		}
		if _, err := pve.ObserveStorageISOContent(ctx, m.deps.PVE, target.Node, volume, target.VirtualBytes); err != nil {
			return 0, err
		}
	} else if response.Format != "qcow2" && response.Format != "raw" {
		return 0, fmt.Errorf("actual disk format is unsupported")
	}
	return uint64(response.Size), nil
}
func (m *managedVMAllocation) acquireCharge(_ context.Context, id, volume string) error {
	var record inv.ChargeRecord
	found := false
	ledgerRecords := m.prepared.ledger.Records()
	for i := range ledgerRecords {
		candidate := &ledgerRecords[i]
		if candidate.Charge.ID == id {
			record = *candidate
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	if record.Acquired {
		return nil
	}
	proof, err := m.prepared.collector.MarkCompletion(record, volume, true, true)
	if err != nil {
		return err
	}
	ledger, err := m.prepared.ledger.Acquire(id, proof)
	if err != nil {
		return err
	}
	m.prepared.ledger = ledger
	entry := m.handle.Record()
	stepID := m.chargeSteps[id]

	for i := range entry.Steps {
		if entry.Steps[i].ID != stepID {
			continue
		}
		index, ok := m.chargeIndices[id]
		if !ok || index < 0 || index >= len(entry.Steps[i].Charges) {
			return fmt.Errorf("allocated charge index is absent")
		}
		charge := &entry.Steps[i].Charges[index]
		if charge.Backing != record.CapacityKey || charge.PlannedBytes <= 0 || uint64(charge.PlannedBytes) != record.Charge.Bytes {
			return fmt.Errorf("allocated charge identity changed")
		}
		charge.AcquiredBytes = charge.PlannedBytes
		charge.OutstandingBytes = 0
		entry.Steps[i].State = aj.Observed
		entry.State = aj.Observed
		return m.handle.Save(entry)
	}
	return fmt.Errorf("allocated charge has no durable intent")
}

func (m *managedVMAllocation) observeCreatedRoot(ctx context.Context, method, step string) error {
	cfg, err := m.deps.PVE.QEMU().Config(ctx, m.shape.node, m.vmid)
	if err != nil {
		return err
	}
	root, ok := pve.ConfigString(cfg, m.shape.rootDiskKey)
	if !ok {
		return fmt.Errorf("created VM root missing")
	}
	volume := strings.Split(root, ",")[0]
	target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleRoot)
	actualSize, err := m.observeTargetVolume(ctx, target, volume, target.Source.VirtualBytes)
	if err != nil {
		return err
	}
	if method == managedVMCallCreate {
		if err := m.verifyMarker(cfg); err != nil {
			return err
		}
	} else {
		m.needsMarker = true
	}
	m.rootCreated = true
	m.volumes[storageRoleRoot] = volume
	auxiliaries, err := m.observeRootAuxiliary(ctx, cfg, target)
	if err != nil {
		return err
	}
	volumes := make([]string, 0, 1+len(auxiliaries))
	volumes = append(volumes, volume)
	volumes = append(volumes, auxiliaries...)
	if err := storageMutationObserved(m.handle, step, volumes, false); err != nil {
		return err
	}
	if len(auxiliaries) > 0 {
		if err := m.acquireCharge(ctx, "root_auxiliary", auxiliaries[0]); err != nil {
			return err
		}
	}

	if err := m.acquireCharge(ctx, "root_base", volume); err != nil {
		return err
	}
	if actualSize >= target.VirtualBytes {
		if err := m.acquireCharge(ctx, "root_growth", volume); err != nil {
			return err
		}
	}
	return nil
}

func (m *managedVMAllocation) observeEphemeralCreation(ctx context.Context, _ ManagedAllocationMutation, step string, result any) error {
	volume, ok := result.(string)
	if !ok || volume == "" {
		return fmt.Errorf("ephemeral create lacks actual volume")
	}
	target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleEphemeral)
	if _, err := m.observeTargetVolume(ctx, target, volume, target.VirtualBytes); err != nil {
		return err
	}
	m.volumes[storageRoleEphemeral] = volume
	volumes := []string{volume}
	if err := storageMutationObserved(m.handle, step, volumes, false); err != nil {
		return err
	}
	if err := m.acquireCharge(ctx, storageRoleEphemeral, volume); err != nil {
		return err
	}
	return nil
}

func (m *managedVMAllocation) observeISOUpload(ctx context.Context, call ManagedAllocationMutation, step string, _ any) error {
	target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleISO)
	filename, _ := call.Args["filename"].(string)
	volume := target.StorageID + ":iso/" + filename
	size, err := m.observeTargetVolume(ctx, target, volume, 1)
	if err != nil {
		return err
	}
	if size != target.VirtualBytes {
		return fmt.Errorf("uploaded ISO differs from frozen artifact size")
	}
	m.volumes[storageRoleISO] = volume
	volumes := []string{volume}
	if err := storageMutationObserved(m.handle, step, volumes, false); err != nil {
		return err
	}
	if err := m.acquireCharge(ctx, storageRoleISO, volume); err != nil {
		return err
	}
	return nil
}

func (m *managedVMAllocation) prepareMutationCharges(ctx context.Context, method, role string, target *aj.Target) ([]inv.ChargeRecord, error) {
	var charges []inv.ChargeRecord
	if role == "" {
		return charges, nil
	}
	selected, ok := managedVMRoleTarget(m.prepared.plan, role)
	if !ok {
		return nil, fmt.Errorf("mutation attempted an unplanned storage role")
	}
	target.Storage = selected.StorageID
	target.Backing = selected.BackingKey
	if err := m.prepareIntendedVolume(target, role); err != nil {
		return nil, err
	}
	if method == managedVMCallCreateVolume {
		definition, ok := m.prepared.plan.Definitions[target.Storage]
		if !ok {
			return nil, fmt.Errorf("ephemeral birth backing unavailable")
		}
		if err := verifyManagedVMCleanupDefinition(ctx, m.deps, *target, definition); err != nil {
			return nil, err
		}
		present, err := managedVolumePresent(ctx, m.deps, target.Node, target.IntendedVolume)
		if err != nil {
			return nil, err
		}
		if present {
			return nil, fmt.Errorf("ephemeral birth name already exists before submission")
		}
	}
	ledgerRecords := m.prepared.ledger.Records()
	for i := range ledgerRecords {
		record := &ledgerRecords[i]
		if record.Charge.Role == role {
			if record.Submitted {
				return nil, fmt.Errorf("storage role already submitted")
			}
			next, err := m.prepared.ledger.Submit(record.Charge.ID)
			if err != nil {
				return nil, err
			}
			m.prepared.ledger = next
			charges = append(charges, *record)
		}
	}
	return charges, nil
}
