package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// retainManagedEphemeralForVMDelete reassigns a proven VM-owned ephemeral
// volume to a parker. The caller holds and finishes the VM allocation; the
// returned volume and all intermediate ownership evidence remain in that record.
func retainManagedEphemeralForVMDelete(ctx context.Context, deps Deps, handle *aj.Handle, node string, vmid int, volume string) (retained string, operationErr error) {
	if handle == nil || handle.Record().Kind != "vm" {
		return "", fmt.Errorf("ephemeral retention requires VM allocation ownership")
	}
	state := handle.Record().State
	if state != aj.Planned && state != aj.Observed {
		return "", fmt.Errorf("VM allocation cannot admit ephemeral retention")
	}
	disk, backing, virtualBytes, err := prepareManagedEphemeralRetention(ctx, deps, handle, node, vmid, volume)
	if err != nil {
		return "", err
	}
	token, cid := disk.stableID, disk.diskCID
	session := &storageLifecycle{handle: handle, operation: "delete_vm_retain_ephemeral"}
	lifecycle := &managedDiskLifecycle{external: true, ownedRetention: true, retainedBytes: virtualBytes, externalNode: node, externalBacking: backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return "", err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = &managedDiskLifecycleClient{Client: guard.Client(), lifecycle: lifecycle}
	defer func() {
		operationErr = errors.Join(operationErr, guard.Err())
		if operationErr != nil {
			deps.recordStorageReconciliation(ctx, "required")
			operationErr = errors.Join(operationErr, session.Uncertain("ephemeral retention incomplete"))
		}
	}()
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", err
	}
	volumes, err := managedVMConfigVolumes(cfg)
	if err != nil {
		return "", err
	}
	slot := ""
	for key, actual := range volumes {
		if actual != volume {
			continue
		}
		if slot != "" || strings.HasPrefix(key, "unused") {
			return "", fmt.Errorf("ephemeral retention requires one active volume slot")
		}
		slot = key
	}
	if slot == "" {
		return "", fmt.Errorf("ephemeral retention volume moved before mutation")
	}
	value, _ := pve.ConfigString(cfg, slot)
	if prior, ok := pve.StableIDFromDriveOptStr(value); ok && prior != token {
		return "", fmt.Errorf("ephemeral volume has conflicting stable identity")
	}
	for _, option := range strings.Split(value, ",")[1:] {
		if strings.HasPrefix(option, "serial=") {
			return "", fmt.Errorf("ephemeral retention cannot replace an existing serial")
		}
	}
	value += ",serial=" + token
	params := &nodes.UpdateQemuConfigParams{}
	if err := setManagedRetentionDrive(params, slot, value); err != nil {
		return "", err
	}
	if err := local.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), params); err != nil {
		return "", err
	}
	pve.UpdateAttachedDiskCID(ctx, local.PVE, local.Log(ctx), node, vmid, token, cid)
	if err := guard.Err(); err != nil {
		return "", err
	}
	if err := handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, disk); err != nil {
		return "", err
	}
	if err := verifyLegacyDiskPreservation(ctx, deps, disk); err != nil {
		return "", err
	}
	return lifecycle.disk.volid, nil
}

func setManagedRetentionDrive(params *nodes.UpdateQemuConfigParams, slot, value string) error {
	for _, prefix := range []string{"scsi", managedDiskBusVirtio, "sata", "ide"} {
		if !strings.HasPrefix(slot, prefix) {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(slot, prefix))
		if err != nil || index < 0 {
			return fmt.Errorf("invalid ephemeral disk slot")
		}
		field := map[int]string{index: value}
		switch prefix {
		case "scsi":
			params.Scsi = field
		case managedDiskBusVirtio:
			params.Virtio = field
		case "sata":
			params.Sata = field
		case "ide":
			params.Ide = field
		}
		return nil
	}
	return fmt.Errorf("unsupported ephemeral retention disk slot")
}

// managedVMRetainedTarget returns the exact receiving parker established by
// the retention guard, without treating that parker as the original guest.
func managedVMRetainedTarget(record aj.Record, volume string, originalVMID int) (aj.Target, error) {
	var result aj.Target
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if !strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") || step.Target.External || step.State != aj.Observed || step.Target.IntendedVolume != volume || step.Target.VMID == originalVMID || step.Target.VMID <= 0 {
			continue
		}
		if result.VMID != 0 && result != step.Target {
			return aj.Target{}, fmt.Errorf("ambiguous ephemeral retention target")
		}
		result = step.Target
	}
	if result.VMID == 0 || result.Storage == "" || result.Backing == "" {
		return aj.Target{}, fmt.Errorf("ephemeral retention target is not recorded")
	}
	return result, nil
}

func prepareManagedEphemeralRetention(ctx context.Context, deps Deps, handle *aj.Handle, node string, vmid int, volume string) (resolvedDisk, string, uint64, error) {
	record := handle.Record()
	owned := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.External || step.Target.Node != node || step.Target.VMID != vmid {
			continue
		}
		for _, actual := range step.VolIDs {
			if actual == volume {
				owned = true
			}
		}
	}
	if !owned {
		return resolvedDisk{}, "", 0, fmt.Errorf("ephemeral retention requires exact recorded VM volume ownership")
	}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return resolvedDisk{}, "", 0, err
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return resolvedDisk{}, "", 0, err
	}
	owned = false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.External || step.Target.Node != node || step.Target.VMID != vmid || step.Target.Storage != storage || step.Target.Backing != backing {
			continue
		}
		for _, actual := range step.VolIDs {
			if actual == volume {
				owned = true
			}
		}
	}
	if !owned {
		return resolvedDisk{}, "", 0, fmt.Errorf("ephemeral retention backing differs from recorded ownership")
	}
	present, err := observeManagedDiskVolume(ctx, deps, node, volume, nil)
	if err != nil || !present {
		return resolvedDisk{}, "", 0, fmt.Errorf("ephemeral retention volume is not observed")
	}
	_, bareVolume, _ := pve.ParseDiskCID(volume)
	content, contentErr := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bareVolume)
	if contentErr != nil || content == nil || content.Size <= 0 {
		return resolvedDisk{}, "", 0, fmt.Errorf("ephemeral retention size unavailable")
	}
	sum := sha256.Sum256([]byte("vm-ephemeral-retention\x00" + record.ID))
	token := "bpd-" + hex.EncodeToString(sum[:8])
	identity, err := pve.ResolveDiskIdentity(ctx, deps.PVE, deps.Log(ctx), storage+":bosh-retention-probe-"+record.ID, token, parkerReadConfigFor(deps))
	if err != nil {
		return resolvedDisk{}, "", 0, err
	}
	if identity.Holder.Found || identity.Intent != nil {
		return resolvedDisk{}, "", 0, fmt.Errorf("ephemeral retention token already identifies a live resource; audit recorded retention")
	}
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
	if err != nil {
		return resolvedDisk{}, "", 0, err
	}
	disk := resolvedDisk{diskCID: cid, birth: volume, volid: volume, meta: &pve.DiskCIDMeta{ID: token}, stableID: token, holder: &pve.DiskHolder{Found: true, Node: node, VMID: vmid}}

	return disk, backing, uint64(content.Size), nil
}
