package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

func (m *managedVMAllocation) restoreAcquiredCharges(ctx context.Context, observed *managedVMObservation) error {
	records := slices.Clone(m.prepared.plan.Charges)
	slices.SortFunc(records, func(a, b inv.ChargeRecord) int { return strings.Compare(a.Charge.ID, b.Charge.ID) })
	proofs := map[string]inv.CompletionEvidence{}
	journalRecord := m.handle.Record()
	if observed != nil {
		root, ok := pve.ConfigString(observed.Config, m.shape.rootDiskKey)
		if !ok {
			return fmt.Errorf("recorded root device is absent")
		}
		m.volumes[storageRoleRoot] = strings.Split(root, ",")[0]
	}
	changed := false
	for si := range journalRecord.Steps {
		step := &journalRecord.Steps[si]
		if step.Attempt != journalRecord.ActiveAttempt() {
			continue
		}
		role := managedVMAllocationStepRole(step.Kind)
		if role == "" {
			continue
		}
		if step.State != aj.Observed {
			return fmt.Errorf("allocation mutation lacks observed completion")
		}
		if len(step.VolIDs) == 0 || role != storageRoleRoot && len(step.VolIDs) != 1 {
			return fmt.Errorf("allocation mutation lacks unique actual volume")
		}
		volume := step.VolIDs[0]
		if role == storageRoleRoot {
			volume = m.volumes[storageRoleRoot]
		}
		m.volumes[role] = volume
		target, ok := managedVMRoleTarget(m.prepared.plan, role)
		if !ok {
			return fmt.Errorf("recorded mutation role is absent from frozen plan")
		}

		auxiliaries, err := m.restoreRoleArtifacts(ctx, observed, step, role, target, volume)
		if err != nil {
			return err
		}
		size, err := m.observeTargetVolume(ctx, target, volume, 1)
		if err != nil {
			return err
		}
		indices := []int{}
		for i := range records {
			if records[i].Charge.Role == role {
				indices = append(indices, i)
			}
		}
		if len(indices) != len(step.Charges) {
			return fmt.Errorf("recorded role charges differ from frozen plan")
		}
		acquired, err := m.restoreRoleCharges(step, records, indices, target, size, volume, auxiliaries, proofs)
		if err != nil {
			return err
		}
		changed = changed || acquired

	}
	snapshot := m.prepared.inventory
	if len(proofs) > 0 {
		var err error
		snapshot, err = m.prepared.collector.Refresh(ctx, snapshot)
		if err != nil {
			return err
		}
	}
	ledger, err := inv.RestoreLedger(snapshot, records, proofs, time.Now())
	if err != nil {
		return err
	}
	if changed {
		if err := m.handle.Save(journalRecord); err != nil {
			return err
		}
	}
	m.prepared.inventory = snapshot
	m.prepared.ledger = ledger
	return nil
}

func (m *managedVMAllocation) restoreRoleCharges(step *aj.Step, records []inv.ChargeRecord, indices []int, target StoragePlanTarget, size uint64, volume string, auxiliaries []string, proofs map[string]inv.CompletionEvidence) (bool, error) {
	changed := false
	for j, index := range indices {
		record := &records[index]
		old := &step.Charges[j]
		if old.Backing != record.CapacityKey || old.Domain != record.DomainKey || old.PlannedBytes <= 0 || uint64(old.PlannedBytes) != record.Charge.Bytes {
			return false, fmt.Errorf("recorded charge identity differs from frozen plan")
		}
		record.Submitted = true
		m.chargeSteps[record.Charge.ID] = step.ID
		m.chargeIndices[record.Charge.ID] = j
		acquired := false
		switch record.Charge.ID {
		case "root_base":
			acquired = size >= target.Source.VirtualBytes
		case "root_growth":
			acquired = size >= target.VirtualBytes
		case "root_auxiliary":
			acquired = len(auxiliaries) > 0
		case storageRoleEphemeral:
			acquired = size >= target.VirtualBytes
		case storageRoleISO:
			acquired = size == target.VirtualBytes
		default:
			return false, fmt.Errorf("recorded allocation charge has no independent completion proof")
		}
		if !acquired {
			continue
		}
		proofVolume := volume
		if record.Charge.ID == "root_auxiliary" {
			proofVolume = auxiliaries[0]
		}
		proof, err := m.prepared.collector.MarkCompletion(*record, proofVolume, true, true)
		if err != nil {
			return false, err
		}
		proofs[record.Charge.ID] = proof
		record.Acquired = true
		record.VolumeID = proofVolume
		if old.AcquiredBytes != old.PlannedBytes {
			old.AcquiredBytes = old.PlannedBytes
			old.OutstandingBytes = 0
			changed = true
		}
	}
	return changed, nil
}

func (m *managedVMAllocation) restoreRoleArtifacts(ctx context.Context, observed *managedVMObservation, step *aj.Step, role string, target StoragePlanTarget, volume string) ([]string, error) {
	if role != storageRoleRoot {
		return nil, nil
	}
	if observed == nil {
		return nil, fmt.Errorf("root restoration lacks independent VM observation")
	}
	auxiliaries, err := m.observeRootAuxiliary(ctx, observed.Config, target)
	if err != nil {
		return nil, err
	}
	expected := append([]string{volume}, auxiliaries...)
	recorded := slices.Clone(step.VolIDs)
	slices.Sort(expected)
	slices.Sort(recorded)
	if !slices.Equal(expected, recorded) {
		return nil, fmt.Errorf("root artifact history differs from actual clone layout")
	}
	return auxiliaries, nil
}

func managedVMAllocationStepRole(kind string) string {
	switch kind {
	case managedVMStepCreate, managedVMStepClone:
		return storageRoleRoot
	case managedVMStepCreateVolume:
		return storageRoleEphemeral
	case managedVMStepUpload:
		return storageRoleISO
	default:
		return ""
	}
}
