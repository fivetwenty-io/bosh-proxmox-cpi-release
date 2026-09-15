package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"math"
	"slices"
	"strings"
)

func cleanupPendingVMAllocation(step aj.Step, record aj.Record) bool {
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.Target.External || step.Target.VMID <= 0 || len(step.VolIDs) != 0 {
		return false
	}
	births := 0
	for index := range record.Steps {
		candidate := &record.Steps[index]
		if candidate.Attempt == record.ActiveAttempt() && candidate.Kind == step.Kind {
			births++
		}
	}
	if births != 1 {
		return false
	}
	role := ""
	switch step.Kind {
	case "vm." + managedVMCallCreate, "vm." + managedVMCallClone:
		if !cleanupRootTaskIdentity(step, record) {
			return false
		}
		role = storageRoleRoot
	case "vm." + managedVMCallCreateVolume:
		if step.State != aj.Planned || step.UPID != "" {
			return false
		}
		var birth struct {
			Version int    `json:"version"`
			Kind    string `json:"kind"`
			Absent  bool   `json:"absence_verified"`
		}
		if json.Unmarshal(step.Parameters, &birth) != nil || birth.Version != 1 || birth.Kind != "ephemeral_birth" || !birth.Absent {
			return false
		}
		role = storageRoleEphemeral
	default:
		return false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil || plan.VMExecution == nil {
		return false
	}
	target, ok := managedVMRoleTarget(plan, role)
	if !ok || target.Node != step.Target.Node || target.StorageID != step.Target.Storage || target.BackingKey != step.Target.Backing {
		return false
	}
	if role == storageRoleEphemeral {
		definition, ok := plan.Definitions[target.StorageID]
		if !ok {
			return false
		}
		_, suffix, err := pve.ManagedEphemeralVolumeName(definition.Type, plan.VMExecution.DiskFormat, step.Target.VMID, record.Namespace, record.ID)
		if err != nil || step.Target.IntendedVolume != target.StorageID+":"+suffix {
			return false
		}
	} else if step.Target.IntendedVolume != "" {
		return false
	}
	var expected []aj.Charge
	for index := range plan.Charges {
		charge := &plan.Charges[index]
		if charge.Charge.Role == role {
			if charge.Charge.Bytes > math.MaxInt64 {
				return false
			}
			expected = append(expected, aj.Charge{Backing: charge.CapacityKey, Domain: charge.DomainKey, PlannedBytes: int64(charge.Charge.Bytes), OutstandingBytes: int64(charge.Charge.Bytes)})
		}
	}
	if len(expected) != len(step.Charges) {
		return false
	}
	for _, charge := range step.Charges {
		index := slices.Index(expected, charge)
		if index < 0 {
			return false
		}
		expected = append(expected[:index], expected[index+1:]...)
	}
	return true
}

// The task identifies submission completion; a fresh full VM marker and concrete
// storage/device readback establish ownership separately. A filename or task ID
// alone never grants cleanup authority.
func observeCleanupVMAllocation(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, step aj.Step) (aj.Verification, []string, error) {
	nodes, err := clusterNodeNames(ctx, deps)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	audit, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil || !audit.Complete || !audit.VMScanComplete || len(audit.Issues) > 0 || len(audit.Conflicts) > 0 {
		return aj.Verification{}, nil, fmt.Errorf("unknown VM allocation requires complete historical visibility")
	}
	node, vmid, _, err := managedVMDisposalIdentity(record, audit)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	definition, ok := plan.Definitions[step.Target.Storage]
	if !ok {
		return aj.Verification{}, nil, fmt.Errorf("pending VM allocation lacks frozen backing")
	}
	if err := verifyManagedVMCleanupDefinition(ctx, deps, step.Target, definition); err != nil {
		return aj.Verification{}, nil, err
	}
	if node == "" {
		return observeAbsentCleanupVMAllocation(ctx, deps, record, step, nodes, vmid, audit, plan, definition)
	}
	if node != step.Target.Node || vmid != step.Target.VMID {
		return aj.Verification{}, nil, fmt.Errorf("pending VM allocation moved from exact target")
	}
	observed, err := observeManagedVMRecord(ctx, deps, journal, record)
	if err != nil {
		return aj.Verification{}, nil, err
	}
	var volumes []string
	if step.Kind == "vm."+managedVMCallCreateVolume {
		volume := step.Target.IntendedVolume
		present, err := managedVolumePresent(ctx, deps, node, volume)
		if err != nil {
			return aj.Verification{}, nil, err
		}
		if present {
			target, _ := managedVMRoleTarget(plan, storageRoleEphemeral)
			m := managedVMAllocation{deps: deps}
			size, err := m.observeTargetVolume(ctx, target, volume, target.VirtualBytes)
			if err != nil || size != target.VirtualBytes {
				return aj.Verification{}, nil, fmt.Errorf("ephemeral allocation content differs from frozen size")
			}
			if _, err := managedVMVerifyCleanupVolume(ctx, deps, step.Target, definition, true); err != nil {
				return aj.Verification{}, nil, err
			}
			volumes = append(volumes, volume)
		}
	} else {
		target, _ := managedVMRoleTarget(plan, storageRoleRoot)
		drive, ok := pve.ConfigString(observed.Config, plan.VMExecution.RootDevice)
		if !ok {
			references, err := managedVMConfigVolumes(observed.Config)
			if err != nil || len(references) != 0 {
				return aj.Verification{}, nil, fmt.Errorf("missing root has other guest disk references")
			}
			if err := observePlannedVMStorageAbsence(ctx, deps, record, nodes, vmid); err != nil {
				return aj.Verification{}, nil, err
			}
			return observed.Verification, nil, nil
		}
		if !ok || strings.Contains(drive, "media=cdrom") || target.Source == nil {
			return aj.Verification{}, nil, fmt.Errorf("pending root device is unavailable")
		}
		volume := strings.Split(drive, ",")[0]
		if volume == target.Source.VolumeID {
			return aj.Verification{}, nil, fmt.Errorf("pending root still references source image")
		}
		owner, ok := pve.EmbeddedDiskVMID(volume)
		if !ok || owner != vmid {
			return aj.Verification{}, nil, fmt.Errorf("pending root volume belongs to another VM")
		}
		m := managedVMAllocation{deps: deps, shape: &createVMShape{rootDiskKey: plan.VMExecution.RootDevice}}
		size, err := m.observeTargetVolume(ctx, target, volume, target.Source.VirtualBytes)
		if err != nil || size != target.Source.VirtualBytes {
			return aj.Verification{}, nil, fmt.Errorf("pending root size differs from submitted source")
		}
		auxiliary, err := m.observeRootAuxiliary(ctx, observed.Config, target)
		if err != nil {
			return aj.Verification{}, nil, err
		}
		volumes = append([]string{volume}, auxiliary...)
	}
	if err := verifyCleanupAllocationReferences(ctx, deps, observed.Node, observed.VMID, definition, volumes); err != nil {
		return aj.Verification{}, nil, err
	}
	return observed.Verification, volumes, nil
}

func verifyCleanupAllocationReferences(ctx context.Context, deps Deps, node string, vmid int, definition pve.StorageInfo, volumes []string) error {
	guests, skipped, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
	if err != nil || len(skipped) > 0 {
		return fmt.Errorf("pending allocation reference scan incomplete")
	}
	for _, guest := range guests {
		if guest.Node == node && guest.VMID == vmid {
			continue
		}
		cfg, err := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
		if err != nil {
			return err
		}
		references, err := managedVMConfigVolumes(cfg)
		if err != nil {
			return err
		}
		for _, volume := range references {
			if slices.Contains(volumes, volume) && (definition.IsShared() || guest.Node == node) {
				return fmt.Errorf("pending allocation artifact is referenced by another VM")
			}
		}
	}
	return nil
}

// Previously retained admission links the exact full-UUID volume to the owned VM
// before destruction. Fresh absence, backing, size and reference checks remain
// mandatory; this historical evidence alone never authorizes a mutation.
func cleanupPriorVMVolumeOwnership(record aj.Record, step aj.Step, volume string) bool {
	for _, verification := range record.Verifications {
		if !verification.Complete || !verification.OwnershipVerified {
			continue
		}
		var payload struct {
			Operation  string             `json:"operation"`
			Settlement *cleanupSettlement `json:"cleanup_settlement"`
		}
		if json.Unmarshal([]byte(verification.EvidenceJSON), &payload) != nil || payload.Operation != "explicit_cleanup_admission" || payload.Settlement == nil {
			continue
		}
		proof := payload.Settlement
		if proof.AllocationID != record.ID || proof.Attempt != record.ActiveAttempt() || proof.PendingVMAllocation == nil || proof.PendingVMAllocation.ID != step.ID || !slices.Contains(proof.PendingVMVolumes, volume) {
			continue
		}
		original := step
		for index := range record.Steps {
			candidate := &record.Steps[index]
			if candidate.ID == step.ID {
				original = *candidate
				break
			}
		}
		hash, err := aj.Fingerprint(original)
		if err == nil && proof.Steps[step.ID] == hash {
			return true
		}
	}
	return false
}

func observeAbsentCleanupVMAllocation(ctx context.Context, deps Deps, record aj.Record, step aj.Step, nodes []string, vmid int, audit StorageAllocationAudit, plan *StorageAllocationPlan, definition pve.StorageInfo) (aj.Verification, []string, error) {
	if step.Kind == "vm."+managedVMCallCreateVolume && cleanupPriorVMVolumeOwnership(record, step, step.Target.IntendedVolume) {
		volume := step.Target.IntendedVolume
		for _, evidence := range audit.Evidence {
			if evidence.AllocationID == record.ID && evidence.VolumeID != volume {
				return aj.Verification{}, nil, fmt.Errorf("absent VM has unrelated allocation artifacts")
			}
		}
		if _, err := observePlannedVMAbsence(ctx, deps, record, nodes, vmid, volume); err != nil {
			return aj.Verification{}, nil, err
		}
		present, err := managedVMVerifyCleanupVolume(ctx, deps, step.Target, definition, true)
		if err != nil {
			return aj.Verification{}, nil, err
		}
		if !present {
			return aj.Verification{Complete: true, AbsenceVerified: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}, nil, nil
		}
		target, _ := managedVMRoleTarget(plan, storageRoleEphemeral)
		m := managedVMAllocation{deps: deps}
		size, err := m.observeTargetVolume(ctx, target, volume, target.VirtualBytes)
		if err != nil || size != target.VirtualBytes {
			return aj.Verification{}, nil, fmt.Errorf("orphan ephemeral content differs from frozen size")
		}
		return aj.Verification{Complete: true, OwnershipVerified: true, VMAbsenceVerified: true}, []string{volume}, nil
	}
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID == record.ID {
			return aj.Verification{}, nil, fmt.Errorf("absent VM still has allocation artifacts")
		}
	}
	// Root names are selected by PVE. Check the entire VMID on every historical
	// backing through the existing disposal absence observer before closing.
	proof, err := observePlannedVMAbsence(ctx, deps, record, nodes, vmid)
	return proof, nil, err
}
