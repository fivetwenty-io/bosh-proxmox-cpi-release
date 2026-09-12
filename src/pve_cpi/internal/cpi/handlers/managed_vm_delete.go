package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// cleanupManagedVMAttempt returns fresh disposition evidence without closing the
// generation. The caller may close the attempt, admit a retry, or tombstone it.
func cleanupManagedVMAttempt(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle) (aj.Verification, error) {
	return disposeManagedVM(ctx, deps, journal, handle, false)
}

func disposeManagedVM(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle, retain bool) (proof aj.Verification, retErr error) {
	if handle == nil || journal == nil {
		return proof, fmt.Errorf("VM cleanup requires held journal authority")
	}
	record := handle.Record()
	if record.State == aj.VMDeletedRetained {
		return disposeManagedRetainedVM(ctx, deps, journal, handle)
	}
	if record.Kind != "vm" {
		return proof, fmt.Errorf("VM cleanup requires a VM allocation")
	}
	if err := storageCleanupSettled(ctx, record); err != nil {
		return proof, err
	}
	clusterNodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return proof, err
	}
	audit, err := AuditStorageAllocations(ctx, deps, journal, clusterNodes)
	if err != nil {
		return proof, err
	}
	if !audit.Complete || !audit.VMScanComplete || len(audit.Issues) != 0 || len(audit.Conflicts) != 0 {
		return proof, fmt.Errorf("VM cleanup requires complete conflict-free historical visibility")
	}
	node, vmid, owned, err := managedVMDisposalIdentity(record, audit)
	if err != nil {
		return proof, err
	}
	if err := includePendingCleanupVolumes(ctx, deps, journal, record, owned); err != nil {
		return proof, err
	}
	// An absent marker never authorizes destroying a guest at its old numeric ID.
	if vmid > 0 {
		location, e := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if e != nil {
			return proof, e
		}
		if location.Found && location.Node != node {
			return proof, fmt.Errorf("VM cleanup identity lacks exact live provenance")
		}
	}
	admission, err := storageAllocationVerification(audit, map[string]any{"operation": "VM cleanup admission", allocationEvidenceIDField: record.ID})
	if err != nil {
		return proof, err
	}
	admission.OwnershipVerified = node != ""
	admission.VMAbsenceVerified = node == ""
	admission.AbsenceVerified = node == ""
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID == record.ID {
			admission.AbsenceVerified = false
			admission.OwnershipVerified = true
		}
	}
	if record.State == aj.ReadyToReturn || record.State == aj.Adopted {
		record.State = aj.ReconciliationRequired
		record.Reason = "VM deletion admitted; resource disposition pending"
		if err := handle.Save(record); err != nil {
			return proof, err
		}
		record = handle.Record()
	}
	if record.State != aj.VMDeletedRetained {
		record.State = aj.Observed
	}
	record.Reason = ""
	record.Verifications = append(record.Verifications, admission)
	if err := handle.Save(record); err != nil {
		return proof, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, storageAllocationUncertain(handle, "VM cleanup"))
		}
	}()
	if err := cleanupManagedVMInfrastructure(ctx, deps, journal, handle, node, vmid); err != nil {
		return proof, storageCleanupFailure("vm_infrastructure", err)
	}
	retainedTargets, err := deleteManagedVMGuest(ctx, deps, handle, record, node, vmid, owned, retain)
	if err != nil {
		return proof, err
	}

	if err := deleteManagedVMOrphanVolumes(ctx, deps, journal, handle, record, clusterNodes, owned, retainedTargets); err != nil {
		return proof, err
	}

	audit, err = AuditStorageAllocations(ctx, deps, journal, clusterNodes)
	if err != nil {
		return proof, err
	}
	return managedVMDispositionProof(audit, record, vmid, retainedTargets)
}

func managedVMDeleteTask(ctx context.Context, deps Deps, handle *aj.Handle, kind string, target aj.Target, submit func() (any, error), observe func() error) error {
	step, err := storageMutationIntent(handle, "vm.delete."+kind, target, nil)
	if err != nil {
		return err
	}
	result, err := submit()
	if err != nil {
		return err
	}
	upid, err := managedMutationUPID(result)
	if err != nil {
		return err
	}
	if err := storageMutationSubmitted(handle, step, upid); err != nil {
		return err
	}
	if err := pve.AwaitTask(ctx, deps.PVE, target.Node, upid, pve.WithMaxWait(pve.StemcellMaxWait)); err != nil {
		return err
	}
	if err := observe(); err != nil {
		return err
	}
	return storageMutationObserved(handle, step, nil, false)
}

func managedVMDeleteHA(ctx context.Context, deps Deps, handle *aj.Handle, node string, vmid int) error {
	if err := storageCleanupSettled(ctx, handle.Record()); err != nil {
		return err
	}
	sid := haResourceSid(vmid)
	before, err := managedVMHARulesForResource(ctx, deps, vmid)
	if err != nil {
		return err
	}
	present, err := managedVMHAResourcePresent(ctx, deps, sid)
	if err != nil {
		return err
	}
	if !present {
		if len(before) != 0 {
			return fmt.Errorf("HA resource absent while its rule membership remains")
		}
		return nil
	}
	if err != nil {
		return err
	}
	resource, err := deps.PVE.Cluster().GetHaResources(ctx, sid)
	if err != nil {
		return err
	}
	if resource == nil || resource.Sid != sid {
		return fmt.Errorf("HA resource identity is unreadable or differs from the recorded VM")
	}
	parameters, err := aj.MutationParameters(map[string]any{"version": 1, "kind": "ha_resource_purge", "resources": sid})
	if err != nil {
		return err
	}
	ruleSteps, err := recordManagedVMHAPurgeRules(handle, aj.Target{Node: node, VMID: vmid}, before)
	if err != nil {
		return err
	}
	step, err := storageMutationIntent(handle, "vm.delete.ha", aj.Target{Node: node, VMID: vmid}, nil, parameters)
	if err != nil {
		return err
	}
	purge := true
	if err := deps.PVE.Cluster().DeleteHaResources(ctx, sid, &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
		return err
	}
	present, err = managedVMHAResourcePresent(ctx, deps, sid)
	if err != nil || present {
		return fmt.Errorf("HA deregistration not observed")
	}
	if err := observeManagedVMHAPurge(ctx, deps, vmid, before); err != nil {
		return err
	}
	for _, ruleStep := range ruleSteps {
		if err := storageMutationObserved(handle, ruleStep, nil, false); err != nil {
			return err
		}
	}
	return storageMutationObserved(handle, step, nil, false)
}

// deleteManagedVMIfRecorded intercepts historical CIDs before legacy fast-delete
// paths. Current set membership is irrelevant to deleting an existing resource.
func deleteManagedVMIfRecorded(ctx context.Context, deps Deps, cid string, vmid int) (handled bool, retErr error) {
	if deps.Config == nil || deps.Config.StoragePlacementNamespace == "" && deps.Config.StorageAllocationJournalDir == "" {
		return false, nil
	}
	clusterNodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return true, err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, clusterNodes)
	if err != nil {
		return true, err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	records, err := journal.List()
	if err != nil {
		return true, err
	}
	selected, err := managedVMRecordForCID(records, cid)
	if err != nil {
		return true, err
	}
	if selected == nil {
		location, e := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if e != nil {
			return true, e
		}
		if location.Found {
			cfg, e := deps.PVE.QEMU().Config(ctx, location.Node, vmid)
			if e != nil {
				return true, e
			}
			_, found, e := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(cfg))
			if e != nil || found {
				return true, fmt.Errorf("VM allocation provenance has no returned journal CID; explicit cleanup required")
			}
		}
		return false, nil
	}
	handle, err := journal.Acquire(ctx, selected.ID)
	if err != nil {
		return true, err
	}
	defer func() { retErr = errors.Join(retErr, handle.Close()) }()
	record := handle.Record()
	if record.State == aj.VMDeletedRetained || record.State == aj.Deleted || record.State == aj.Cleaned {
		location, e := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if e != nil {
			return true, e
		}
		if location.Found {
			return true, fmt.Errorf("retired VM identity is present; audit required")
		}
		return true, nil
	}
	retain := false
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return true, err
	}
	if location.Found {
		cfg, e := deps.PVE.QEMU().Config(ctx, location.Node, vmid)
		if e != nil {
			return true, e
		}
		tags, _ := pve.ConfigString(cfg, "tags")
		retain = tagsContain(tags, tagRetainEphemeral)
	}
	proof, err := disposeManagedVM(ctx, deps, journal, handle, retain)
	if err != nil {
		return true, err
	}
	record = handle.Record()
	record.Verifications = append(record.Verifications, proof)
	record.Reason = ""
	if proof.AbsenceVerified {
		record.State = aj.Deleted
	} else {
		record.State = aj.VMDeletedRetained
	}
	if err := handle.Save(record); err != nil {
		return true, err
	}
	// Managed ISO disposition is part of the recorded resource audit above.
	// The current Agent may point at a different node or storage than the
	// frozen allocation and cannot authorize another deletion here.
	return true, nil
}

// Verify the physical backing immediately before deletion and reject any live
// guest reference. A matching filename is never permission to delete a volume.
func managedVMVerifyCleanupVolume(ctx context.Context, deps Deps, target aj.Target, expected pve.StorageInfo, unattached bool) (bool, error) {
	if err := verifyManagedVMCleanupDefinition(ctx, deps, target, expected); err != nil {
		return false, err
	}
	storage, bare, err := pve.ParseDiskCID(target.IntendedVolume)
	if err != nil || storage != target.Storage {
		return false, fmt.Errorf("cleanup target identity is invalid")
	}
	present, err := managedVolumePresent(ctx, deps, target.Node, target.IntendedVolume)
	if err != nil || !present {
		return false, err
	}
	content, err := deps.PVE.Nodes().GetStorageContent(ctx, target.Node, storage, bare)
	if pve.IsNotFound(err) {
		return false, nil
	}
	if err != nil || content == nil || content.Size <= 0 {
		return false, fmt.Errorf("cleanup volume content is not proven")
	}
	if unattached {
		guests, skipped, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
		if err != nil || len(skipped) > 0 {
			return false, fmt.Errorf("cleanup volume reference scan incomplete")
		}
		for _, guest := range guests {
			cfg, e := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
			if e != nil {
				return false, e
			}
			volumes, e := managedVMConfigVolumes(cfg)
			if e != nil {
				return false, e
			}
			for _, volume := range volumes {
				if volume == target.IntendedVolume && (expected.IsShared() || guest.Node == target.Node) {
					return false, fmt.Errorf("cleanup volume remains referenced by a guest")
				}
			}
		}
	}
	return true, nil
}

func disposeManagedRetainedVM(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle) (proof aj.Verification, retErr error) {
	record := handle.Record()
	if err := storageCleanupSettled(ctx, record); err != nil {
		return proof, err
	}
	var retention aj.VMRetentionEvidence
	for _, verification := range record.Verifications {
		var candidate aj.VMRetentionEvidence
		if json.Unmarshal([]byte(verification.EvidenceJSON), &candidate) == nil && len(candidate.RetainedArtifacts) > 0 {
			retention = candidate
		}
	}
	if retention.VMID <= 0 || len(retention.RetainedArtifacts) == 0 {
		return proof, fmt.Errorf("retained VM disposition evidence missing")
	}
	clusterNodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return proof, err
	}
	audit, err := AuditStorageAllocations(ctx, deps, journal, clusterNodes)
	if err != nil {
		return proof, err
	}
	if !audit.Complete || len(audit.Conflicts) > 0 {
		return proof, fmt.Errorf("retained cleanup requires complete historical visibility")
	}
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, retention.VMID)
	if err != nil {
		return proof, err
	}
	if location.Found {
		return proof, fmt.Errorf("retired VM identity is present")
	}
	admission, err := retainedCleanupDecisionAdmission(ctx, deps, record, audit, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "retained artifact cleanup admission"})
	if err != nil {
		return proof, err
	}
	record.Verifications = append(record.Verifications, admission)
	if err := handle.Save(record); err != nil {
		return proof, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, storageAllocationUncertain(handle, "retained VM cleanup"))
		}
	}()
	// Already observed cleanup remains idempotent; an unsettled step was rejected
	// above even if its physical artifact has disappeared.
	for _, target := range retention.RetainedArtifacts {
		present := false
		for _, evidence := range audit.Evidence {
			if evidence.AllocationID == record.ID && managedVMRetentionEvidenceMatches(record, target, evidence) {
				present = true
			}
		}
		if !present {
			continue
		}
		if err := observeManagedRetainedEphemeral(ctx, deps, handle, target); err != nil {
			return proof, err
		}
		if err := cleanupManagedRetainedEphemeral(ctx, deps, handle, target); err != nil {
			return proof, err
		}
	}
	audit, err = AuditStorageAllocations(ctx, deps, journal, clusterNodes)
	if err != nil {
		return proof, err
	}
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID == record.ID {
			return proof, fmt.Errorf("retained cleanup artifacts remain")
		}
	}
	proof, err = storageAllocationVerification(audit, map[string]any{"operation": "retained VM cleanup completion", allocationEvidenceIDField: record.ID, metadataKeyVMID: retention.VMID})
	if err != nil {
		return proof, err
	}
	proof.AbsenceVerified = true
	proof.VMAbsenceVerified = true
	proof.ArtifactDispositionVerified = true
	return proof, nil
}

func deleteManagedVMGuest(ctx context.Context, deps Deps, handle *aj.Handle, record aj.Record, node string, vmid int, owned map[string]bool, retain bool) ([]aj.Target, error) {
	var err error
	if node == "" {
		return managedVMRetentionTargets(ctx, deps, handle, record, node, vmid, false)
	}
	var retainedTargets []aj.Target

	if node != "" {
		if err := managedVMDeleteHA(ctx, deps, handle, node, vmid); err != nil {
			return nil, storageCleanupFailure("vm_ha", err)
		}
		status, e := deps.PVE.QEMU().Status(ctx, node, vmid)
		if e != nil {
			return nil, e
		}
		if status["status"] != "stopped" {
			err = managedVMDeleteTask(ctx, deps, handle, "stop", aj.Target{Node: node, VMID: vmid}, func() (any, error) { return deps.PVE.QEMU().Stop(ctx, node, vmid) }, func() error {
				status, e := deps.PVE.QEMU().Status(ctx, node, vmid)
				if e != nil {
					return e
				}
				if status["status"] != "stopped" {
					return fmt.Errorf("VM stop not observed")
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		if err := detachManagedPersistentForVMDelete(ctx, deps, node, vmid, owned, handle); err != nil {
			return nil, err
		}
		retainedTargets, err = managedVMRetentionTargets(ctx, deps, handle, record, node, vmid, retain)
		if err != nil {
			return nil, err
		}

		if err := verifyManagedVMDestroyDevices(ctx, deps, record, node, vmid, owned); err != nil {
			return nil, err
		}

		purge, sweep := true, false
		err = managedVMDeleteTask(ctx, deps, handle, "destroy", aj.Target{Node: node, VMID: vmid}, func() (any, error) {
			return deps.PVE.Nodes().DeleteQemu(ctx, node, strconv.Itoa(vmid), &nodes.DeleteQemuParams{Purge: &purge, DestroyUnreferencedDisks: &sweep})
		}, func() error {
			location, e := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
			if e != nil {
				return e
			}
			if location.Found {
				return fmt.Errorf("VM destruction not observed")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return retainedTargets, nil
}

func deleteManagedVMOrphanVolumes(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle, record aj.Record, clusterNodes []string, owned map[string]bool, retainedTargets []aj.Target) error {
	var audit StorageAllocationAudit
	var err error
	// VM destruction does not delete uploaded ISOs or allocated unattached disks.
	// Only exact journal-owned physical targets may be removed here.
	audit, err = AuditStorageAllocations(ctx, deps, journal, clusterNodes)
	if err != nil {
		return err
	}
	if !audit.Complete || len(audit.Conflicts) != 0 {
		return fmt.Errorf("post-destroy resource audit is incomplete")
	}
	seen := map[string]bool{}
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID != record.ID {
			continue
		}
		if evidence.VolumeID == "" {
			return fmt.Errorf("remaining cleanup artifact is not an exact owned volume")
		}
		kept := false
		for _, target := range retainedTargets {
			if managedVMRetentionEvidenceMatches(record, target, evidence) {
				kept = true
			}
		}
		if kept {
			continue
		}
		if !owned[evidence.VolumeID] {
			return fmt.Errorf("remaining cleanup volume lacks allocation ownership")
		}
		storage, _, e := pve.ParseDiskCID(evidence.VolumeID)
		if e != nil {
			return e
		}
		target := aj.Target{Node: evidence.Node, Storage: storage, IntendedVolume: evidence.VolumeID}
		plan, e := activeStorageAllocationPlan(record)
		if e != nil {
			return e
		}
		definition, ok := plan.Definitions[storage]
		if !ok {
			return fmt.Errorf("cleanup volume lacks frozen backing")
		}
		target.Backing = definition.BackingKey()
		key := evidence.Node + "/" + evidence.VolumeID
		if definition.IsShared() {
			key = target.Backing + "/" + evidence.VolumeID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		present, e := managedVMVerifyCleanupVolume(ctx, deps, target, definition, true)
		if e != nil {
			return e
		}
		if !present {
			continue
		}
		err = managedVMDeleteTask(ctx, deps, handle, "volume", target, func() (any, error) {
			return deps.PVE.Storage().DeleteVolumeAsync(ctx, evidence.Node, storage, evidence.VolumeID)
		}, func() error {
			return awaitManagedVMVolumeAbsence(ctx, deps, evidence.Node, evidence.VolumeID)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func managedVMDisposalIdentity(record aj.Record, audit StorageAllocationAudit) (string, int, map[string]bool, error) {
	vmid, node := 0, ""
	owned := map[string]bool{}
	for rangeIndex50 := range record.Steps {
		if record.Steps[rangeIndex50].Target.External || record.Steps[rangeIndex50].Attempt != record.ActiveAttempt() {
			continue
		}
		if record.Steps[rangeIndex50].Target.VMID > 0 && !strings.HasPrefix(record.Steps[rangeIndex50].Kind, "lifecycle_delete_vm_retain_ephemeral_") {
			if vmid != 0 && vmid != record.Steps[rangeIndex50].Target.VMID {
				return "", 0, nil, fmt.Errorf("VM cleanup targets disagree")
			}
			vmid = record.Steps[rangeIndex50].Target.VMID
		}
		for _, volume := range record.Steps[rangeIndex50].VolIDs {
			owned[volume] = true
		}
	}
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID == record.ID && evidence.Kind == "vm" && evidence.VolumeID == "" {
			if node != "" || evidence.VMID != vmid {
				return "", 0, nil, fmt.Errorf("VM cleanup provenance is ambiguous")
			}
			node = evidence.Node
		}
	}
	return node, vmid, owned, nil
}

func managedVMRetentionTargets(ctx context.Context, deps Deps, handle *aj.Handle, record aj.Record, node string, vmid int, retain bool) ([]aj.Target, error) {
	var retainedTargets []aj.Target
	// A completed transfer remains retained even if the process stopped before
	// recording VM destruction or before the caller received success.
	var previousRetained aj.Target
	for rangeIndex120 := range record.Steps {
		if record.Steps[rangeIndex120].Attempt == record.ActiveAttempt() && strings.HasPrefix(record.Steps[rangeIndex120].Kind, "lifecycle_delete_vm_retain_ephemeral_") && record.Steps[rangeIndex120].State == aj.Observed && record.Steps[rangeIndex120].Target.VMID != vmid && record.Steps[rangeIndex120].Target.VMID > 0 && record.Steps[rangeIndex120].Target.IntendedVolume != "" {
			previousRetained = record.Steps[rangeIndex120].Target
		}
	}
	if previousRetained.VMID > 0 {
		if err := observeManagedRetainedEphemeral(ctx, deps, handle, previousRetained); err != nil {
			return nil, err
		}
		retainedTargets = append(retainedTargets, previousRetained)
	}
	if retain && len(retainedTargets) == 0 {
		original := ""
		for rangeIndex159 := range record.Steps {
			if record.Steps[rangeIndex159].Attempt == record.ActiveAttempt() && strings.HasPrefix(record.Steps[rangeIndex159].Kind, "vm.ephemeral.") && len(record.Steps[rangeIndex159].VolIDs) == 1 {
				if original != "" {
					return nil, fmt.Errorf("multiple ephemeral retention bindings")
				}
				original = record.Steps[rangeIndex159].VolIDs[0]
			}
		}
		if original != "" {
			landed, e := retainManagedEphemeralForVMDelete(ctx, deps, handle, node, vmid, original)
			if e != nil {
				return nil, e
			}
			retainedTarget, e := managedVMRetainedTarget(handle.Record(), landed, vmid)
			if e != nil {
				return nil, e
			}
			if retainedTarget.VMID <= 0 || retainedTarget.Storage == "" || retainedTarget.Backing == "" {
				return nil, fmt.Errorf("retained ephemeral physical target is not proven")
			}
			retainedTargets = append(retainedTargets, retainedTarget)
		}
	}
	return retainedTargets, nil
}

func managedVMDispositionProof(audit StorageAllocationAudit, record aj.Record, vmid int, retainedTargets []aj.Target) (proof aj.Verification, err error) {
	for _, evidence := range audit.Evidence {
		if evidence.AllocationID != record.ID {
			continue
		}
		kept := false
		for _, target := range retainedTargets {
			if managedVMRetentionEvidenceMatches(record, target, evidence) {
				kept = true
			}
		}
		if !kept {
			return proof, fmt.Errorf("VM allocation artifacts remain")
		}
	}
	proof, err = storageAllocationVerification(audit, map[string]any{"operation": "VM cleanup completion", allocationEvidenceIDField: record.ID, metadataKeyVMID: vmid})
	if err != nil {
		return proof, err
	}
	if len(retainedTargets) > 0 {
		var payload map[string]any
		if err := json.Unmarshal([]byte(proof.EvidenceJSON), &payload); err != nil {
			return proof, err
		}
		payload[metadataKeyVMID] = vmid
		payload["retained_artifacts"] = retainedTargets
		proof.EvidenceID, proof.EvidenceJSON, err = aj.VerificationEvidence(payload)
		if err != nil {
			return proof, err
		}
	}
	proof.AbsenceVerified = len(retainedTargets) == 0
	proof.VMAbsenceVerified = true
	proof.ArtifactDispositionVerified = true
	return proof, nil
}

func verifyManagedVMDestroyDevices(ctx context.Context, deps Deps, record aj.Record, node string, vmid int, owned map[string]bool) error {
	cfg, e := deps.PVE.QEMU().Config(ctx, node, vmid)
	if e != nil {
		return e
	}
	for device, value := range cfg {
		if !managedVMVolumeDevice(device) {
			continue
		}
		drive, ok := pve.ConfigStringValue(value)
		if !ok {
			return fmt.Errorf("VM destruction device is malformed")
		}
		volume := strings.Split(drive, ",")[0]
		if strings.Contains(volume, ":") && !owned[volume] {
			return fmt.Errorf("VM destruction would include an unowned volume")
		}
	}
	marker, found, err := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(cfg))
	if err != nil || !found || marker.Kind != "vm" || marker.Namespace != record.Namespace || marker.AllocationID != record.ID || marker.AgentSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(record.AgentID))) {
		return fmt.Errorf("VM destruction provenance changed")
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return err
	}
	volumes, err := managedVMConfigVolumes(cfg)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		storage, _, err := pve.ParseDiskCID(volume)
		if err != nil {
			return err
		}
		def, ok := plan.Definitions[storage]
		if !ok {
			return fmt.Errorf("VM destruction backing lacks frozen definition")
		}
		present, err := managedVMVerifyCleanupVolume(ctx, deps, aj.Target{Node: node, Storage: storage, Backing: def.BackingKey(), IntendedVolume: volume}, def, false)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("VM destruction volume disappeared before submission")
		}
	}
	return nil
}

func managedVMRecordForCID(records []aj.Record, cid string) (*aj.Record, error) {
	var selected, terminalRecord *aj.Record
	for rangeIndex393 := range records {
		if records[rangeIndex393].Kind != "vm" || records[rangeIndex393].CID != cid {
			continue
		}
		if records[rangeIndex393].State == aj.Deleted || records[rangeIndex393].State == aj.Cleaned {
			copyOfRecord := records[rangeIndex393]
			terminalRecord = &copyOfRecord
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("VM CID has multiple live allocation records")
		}
		cloned := records[rangeIndex393]
		selected = &cloned
	}
	if selected == nil {
		return terminalRecord, nil
	}
	return selected, nil
}

func managedVMRetentionEvidenceMatches(record aj.Record, target aj.Target, evidence StorageAllocationEvidence) bool {
	if target.IntendedVolume != evidence.VolumeID {
		return false
	}
	if target.Node == evidence.Node {
		return true
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return false
	}
	definition, ok := plan.Definitions[target.Storage]
	return ok && definition.IsShared() && definition.BackingKey() == target.Backing
}

func verifyManagedVMCleanupDefinition(ctx context.Context, deps Deps, target aj.Target, expected pve.StorageInfo) error {
	response, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	if err != nil || response == nil || *response == nil {
		return fmt.Errorf("cleanup storage definition unavailable")
	}
	found := false
	for _, raw := range *response {
		definition, e := pve.ParseStorageEntry(raw)
		if e != nil {
			return fmt.Errorf("cleanup storage definition malformed")
		}
		if definition.Name != target.Storage {
			continue
		}
		if found {
			return fmt.Errorf("cleanup storage definition ambiguous")
		}
		found = true
		if definition.BackingKey() != target.Backing || definition.BackingKey() != expected.BackingKey() || definition.IsShared() != expected.IsShared() || len(definition.Nodes) > 0 && !slices.Contains(definition.Nodes, target.Node) {
			return fmt.Errorf("cleanup physical backing or shared scope changed")
		}
	}
	if !found {
		return fmt.Errorf("cleanup storage definition disappeared")
	}
	return nil
}

// A missing mutable marker cannot release a journal-owned VM to the legacy
// straggler sweep. Unreadable configured history defers the entire sweep.
func legacySweepJournalVMIDs(ctx context.Context, deps Deps) (protected map[int]bool, retErr error) {
	protected = map[int]bool{}
	if deps.Config == nil || deps.Config.StoragePlacementNamespace == "" && deps.Config.StorageAllocationJournalDir == "" {
		return protected, nil
	}
	clusterNodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return nil, err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, clusterNodes)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	records, err := journal.List()
	if err != nil {
		return nil, err
	}
	for index := range records {
		record := records[index]
		if record.Kind != "vm" {
			continue
		}
		if vmid, e := strconv.Atoi(record.CID); e == nil && vmid > 0 {
			protected[vmid] = true
		}
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if !step.Target.External && step.Target.VMID > 0 && !strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") {
				protected[step.Target.VMID] = true
			}
		}
	}
	return protected, nil
}

func includePendingCleanupVolumes(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record, owned map[string]bool) error {
	if settlement, ok := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement); ok && settlement.CompletedUpload != nil {
		present, e := observeCleanupUploadedISOState(ctx, deps, journal, record, *settlement.CompletedUpload)
		if e != nil {
			return e
		}
		if present {
			owned[settlement.CompletedUpload.IntendedVolume] = true
		}
	}

	if settlement, ok := ctx.Value(cleanupSettlementKey{}).(*cleanupSettlement); ok && settlement.PendingVMAllocation != nil {
		_, volumes, e := observeCleanupVMAllocation(ctx, deps, journal, record, *settlement.PendingVMAllocation)
		if e != nil {
			return e
		}
		for _, volume := range volumes {
			owned[volume] = true
		}
	}
	return nil
}
