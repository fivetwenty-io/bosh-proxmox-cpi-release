package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// retainManagedEphemeralForVMDelete reassigns a proven VM-owned ephemeral
// volume to a parker. The caller holds and finishes the VM allocation; the
// returned volume and all intermediate ownership evidence remain in that record.
// moved reports that the caller's delete audit accepted a move of this VM to
// node on shared storage, so steps recorded on another node still own volume.
func retainManagedEphemeralForVMDelete(ctx context.Context, deps Deps, handle *aj.Handle, node string, vmid int, volume string, moved bool) (retained string, operationErr error) {
	if handle == nil || handle.Record().Kind != "vm" {
		return "", fmt.Errorf("ephemeral retention requires VM allocation ownership")
	}
	state := handle.Record().State
	if state != aj.Planned && state != aj.Observed {
		return "", fmt.Errorf("VM allocation cannot admit ephemeral retention")
	}
	disk, backing, virtualBytes, resumed, err := prepareManagedEphemeralRetention(ctx, deps, handle, node, vmid, volume, moved)
	if err != nil {
		return "", err
	}
	// The read runs after delete_vm's stop and takes both of PVE's views, so
	// a slot whose delete a crash left pending is still the ephemeral
	// volume's slot. The serial write below cancels that pending delete on
	// the stopped VM, and the transfer then moves the volume off it. A
	// pending drive replacement is refused before the retention opens, so
	// the refusal changes nothing and leaves the record resumable.
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		return "", err
	}
	if err := refusePendingDriveReplacement("delete_vm", strconv.Itoa(vmid), holding); err != nil {
		return "", err
	}
	cfg := holding.Config
	token, cid := disk.stableID, disk.diskCID
	start := len(handle.Record().Steps)
	session := &storageLifecycle{handle: handle, operation: "delete_vm_retain_ephemeral"}
	lifecycle := &managedDiskLifecycle{external: true, ownedRetention: true, retainedBytes: virtualBytes, externalNode: node, externalBacking: backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return "", err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = wrapManagedDiskClient(guard, lifecycle)
	ctx = managedLockWaitContext(ctx)
	// check is set once the serial is on the drive. A lock wait that runs out
	// after that and changed nothing a resume can't accept hands the record
	// back observed with the retriable timeout, as finish does for a disk.
	var check *managedRetentionTimeoutCheck
	defer func() {
		guardErr := guard.Err()
		operationErr = errors.Join(operationErr, guardErr)
		if operationErr == nil {
			return
		}
		if check.returned(ctx, deps, handle.Record(), operationErr, guardErr) {
			operationErr = &diskReturnedAfterLockTimeout{err: operationErr}
			return
		}
		deps.recordStorageReconciliation(ctx, "required")
		operationErr = errors.Join(operationErr, session.Uncertain("ephemeral retention incomplete"))
	}()
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
	serials, ours := 0, false
	for _, option := range strings.Split(value, ",")[1:] {
		if strings.HasPrefix(option, "serial=") {
			serials++
			ours = ours || option == "serial="+token
		}
	}
	// A resumed retention finds its own serial, which an earlier attempt
	// wrote, and keeps it. Any other serial is refused.
	switch {
	case serials > 0 && (!resumed || serials != 1 || !ours):
		return "", fmt.Errorf("ephemeral retention cannot replace an existing serial")
	case !resumed:
		value += ",serial=" + token
		params := &nodes.UpdateQemuConfigParams{}
		if err := setManagedRetentionDrive(params, slot, value); err != nil {
			return "", err
		}
		if err := local.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), params); err != nil {
			return "", err
		}
	}
	check = &managedRetentionTimeoutCheck{start: start, node: node, vmid: vmid, slot: slot, written: value, volume: volume, cid: cid, token: token}
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

// prepareManagedEphemeralRetention proves the VM owns volume and returns the
// disk the retention moves. resumed reports that an earlier attempt already
// wrote this record's serial onto the volume's drive. That holds when the
// token resolves to exactly this guest, node, and volume with no transfer
// intent, and the record holds that attempt's observed serial write. Any other
// live holder of the token is refused.
func prepareManagedEphemeralRetention(ctx context.Context, deps Deps, handle *aj.Handle, node string, vmid int, volume string, moved bool) (resolvedDisk, string, uint64, bool, error) {
	record := handle.Record()
	// A step owns the volume on node, or on its recorded node when the delete
	// audit accepted the VM's move to node.
	onNode := func(step *aj.Step) bool { return step.Target.Node == node || moved }
	owned := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.External || !onNode(step) || step.Target.VMID != vmid {
			continue
		}
		for _, actual := range step.VolIDs {
			if actual == volume {
				owned = true
			}
		}
	}
	if !owned {
		return resolvedDisk{}, "", 0, false, fmt.Errorf("ephemeral retention requires exact recorded VM volume ownership")
	}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return resolvedDisk{}, "", 0, false, err
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return resolvedDisk{}, "", 0, false, err
	}
	owned = false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.External || !onNode(step) || step.Target.VMID != vmid || step.Target.Storage != storage || step.Target.Backing != backing {
			continue
		}
		for _, actual := range step.VolIDs {
			if actual == volume {
				owned = true
			}
		}
	}
	if !owned {
		return resolvedDisk{}, "", 0, false, fmt.Errorf("ephemeral retention backing differs from recorded ownership")
	}
	// The token is resolved before the volume is looked for, because a volume
	// that already moved under our token is no longer where the record put
	// it, and the refusal should say where it went.
	token := managedVMRetentionToken(record.ID)
	identity, err := pve.ResolveDiskIdentity(ctx, deps.PVE, deps.Log(ctx), storage+":bosh-retention-probe-"+record.ID, token, parkerReadConfigFor(deps))
	if copied, ok := pve.IsDiskIdentityCopied(err); ok {
		// A token that more than one guest or record carries identifies a
		// live resource as surely as one that a single holder carries, so the
		// retention refuses it the same way. The log keeps where each one is.
		deps.Log(ctx).Warn("ephemeral retention: the retention token already identifies a live resource in more than one place",
			log.String("stable_id", copied.StableID),
			log.Err(err),
		)
		return resolvedDisk{}, "", 0, false, storageRefusal("ephemeral retention token already identifies a live resource; audit recorded retention")
	}
	if err != nil {
		return resolvedDisk{}, "", 0, false, err
	}
	resumed := identity.Holder.Found && identity.Intent == nil && !identity.Holder.IsParker && identity.Holder.VMID == vmid &&
		identity.Holder.Node == node && identity.Volid == volume && managedVMRetentionStarted(record, vmid)
	if !resumed && (identity.Holder.Found || identity.Intent != nil) {
		return resolvedDisk{}, "", 0, false, storageRefusal("ephemeral retention token already identifies a live resource; audit recorded retention")
	}
	present, err := observeManagedDiskVolume(ctx, deps, node, volume, nil)
	if err != nil || !present {
		return resolvedDisk{}, "", 0, false, fmt.Errorf("ephemeral retention volume is not observed")
	}
	_, bareVolume, _ := pve.ParseDiskCID(volume)
	content, contentErr := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bareVolume)
	if contentErr != nil || content == nil || content.Size <= 0 {
		return resolvedDisk{}, "", 0, false, fmt.Errorf("ephemeral retention size unavailable")
	}
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
	if err != nil {
		return resolvedDisk{}, "", 0, false, err
	}
	disk := resolvedDisk{diskCID: cid, birth: volume, volid: volume, meta: &pve.DiskCIDMeta{ID: token}, stableID: token, holder: &pve.DiskHolder{Found: true, Node: node, VMID: vmid}}

	return disk, backing, uint64(content.Size), resumed, nil
}
