package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// observeManagedRetainedEphemeral validates a recorded retainer without taking
// mutation ownership or interpreting a missing volume as cleanup completion.
func observeManagedRetainedEphemeral(ctx context.Context, deps Deps, handle *aj.Handle, target aj.Target) error {
	_, err := resolveManagedRetainedEphemeral(ctx, deps, handle, target)
	return err
}

func resolveManagedRetainedEphemeral(ctx context.Context, deps Deps, handle *aj.Handle, target aj.Target) (resolvedDisk, error) {
	if handle == nil || handle.Record().Kind != "vm" || target.External || target.IntendedVolume == "" {
		return resolvedDisk{}, fmt.Errorf("retained resource requires VM allocation ownership")
	}
	record := handle.Record()
	recorded := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") && step.State == aj.Observed && step.Target == target {
			recorded = true
		}
	}
	if !recorded {
		return resolvedDisk{}, fmt.Errorf("retention target has no settled ownership evidence")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), []string{target.Node})
	if err != nil || identity.ID() != record.ClusterID {
		return resolvedDisk{}, fmt.Errorf("retention cluster continuity differs")
	}
	backing, err := managedDiskActualBacking(ctx, deps, target.Storage)
	if err != nil || backing != target.Backing {
		return resolvedDisk{}, fmt.Errorf("retention backing differs")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, target.Node, target.VMID)
	if err != nil {
		return resolvedDisk{}, err
	}
	if _, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(cfg)); err != nil {
		return resolvedDisk{}, err
	}
	_, sentinel := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var entries map[string]struct {
		DiskCID string `json:"disk_cid"`
		Volid   string `json:"volid"`
		Node    string `json:"node"`
		Slot    string `json:"slot"`
	}
	if err := json.Unmarshal(sentinel["bosh_parked_disks"], &entries); err != nil {
		return resolvedDisk{}, fmt.Errorf("retained parker provenance unavailable")
	}
	sum := sha256.Sum256([]byte("vm-ephemeral-retention\x00" + record.ID))
	token := "bpd-" + hex.EncodeToString(sum[:8])
	entry, ok := entries[token]
	if !ok || entry.Volid != target.IntendedVolume || entry.Node != target.Node || entry.Slot == "" {
		return resolvedDisk{}, fmt.Errorf("retained parker identity differs")
	}
	birth, meta, err := decodeDiskCID(ctx, deps, "retained_ephemeral", entry.DiskCID)
	if err != nil || meta == nil || meta.ID != token {
		return resolvedDisk{}, fmt.Errorf("retained CID identity differs")
	}
	birthRecorded := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if !step.Target.External && strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") && step.Target.IntendedVolume == birth {
			birthRecorded = true
		}
	}
	if !birthRecorded {
		return resolvedDisk{}, fmt.Errorf("retained CID birth has no allocation evidence")
	}
	disk, err := resolveDiskForOp(ctx, deps, "retained_ephemeral", entry.DiskCID, birth, meta)
	if err != nil {
		return resolvedDisk{}, err
	}
	if disk.intent != nil || disk.volid != target.IntendedVolume || disk.holder == nil || !disk.holder.IsParker || disk.holder.Node != target.Node || disk.holder.VMID != target.VMID || disk.holder.Slot != entry.Slot {
		return resolvedDisk{}, fmt.Errorf("retained resource moved away from its recorded target")
	}
	if err := verifyLegacyDiskPreservation(ctx, deps, disk); err != nil {
		return resolvedDisk{}, err
	}
	return disk, nil
}

// cleanupManagedRetainedEphemeral borrows the permanently closed VM generation.
// It deletes only its proven retained E resource. The caller independently
// audits all historical resources and records the final cleanup decision.
func cleanupManagedRetainedEphemeral(ctx context.Context, deps Deps, handle *aj.Handle, target aj.Target) (operationErr error) {
	if handle == nil || handle.Record().State != aj.VMDeletedRetained {
		return fmt.Errorf("retained cleanup requires closed retained VM generation")
	}
	if err := storageCleanupSettled(ctx, handle.Record()); err != nil {
		return err
	}
	disk, err := resolveManagedRetainedEphemeral(ctx, deps, handle, target)
	if err != nil {
		return err
	}
	evidenceID, payload, err := aj.VerificationEvidence(map[string]any{"observed_at": time.Now().UTC(), "scope": "retained_ephemeral_target_ownership", "allocation_id": handle.Record().ID, "target": target})
	if err != nil {
		return err
	}
	record := handle.Record()
	record.Verifications = append(record.Verifications, aj.Verification{EvidenceID: evidenceID, EvidenceJSON: payload, Complete: true, OwnershipVerified: true})
	if err := handle.Save(record); err != nil {
		return err
	}
	session := &storageLifecycle{handle: handle, operation: "delete_disk"}
	lifecycle := &managedDiskLifecycle{external: true, ownedRetention: true, retainedBytes: target.VirtualBytes, externalNode: target.Node, externalBacking: target.Backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = wrapManagedDiskClient(guard, lifecycle)
	defer func() {
		operationErr = errors.Join(operationErr, guard.Err())
		if operationErr != nil {
			deps.recordStorageReconciliation(ctx, "required")
			operationErr = errors.Join(operationErr, session.Uncertain("retained ephemeral cleanup incomplete"))
		}
	}()
	if err := pve.DeleteParkedOwnedDisk(ctx, local.PVE, local.Log(ctx), target.Node, target.VMID, target.IntendedVolume, parkerWriteConfigFor(local)); err != nil {
		return err
	}
	absent, err := volumeAbsentFromStorage(ctx, deps, target.Node, target.IntendedVolume)
	if err != nil || !absent {
		return fmt.Errorf("retained volume deletion is not observed")
	}
	return nil
}
