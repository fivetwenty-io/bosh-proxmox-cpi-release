package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"regexp"
	"sort"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

var managedVMVolumeSlot = regexp.MustCompile(`^(scsi|virtio|sata|ide|unused|efidisk|tpmstate)\d+$`)

// detachManagedPersistentForVMDelete preserves independently identified disks
// outside the VM allocation's exact owned volume set. The caller holds the VM
// generation lock and supplies its original services. Each managed disk acquires
// its own allocation lock. Unknown volumes prevent every preservation mutation.
//
// It runs after delete_vm's stop, and it reads what the destroy would take
// from both of PVE's views. A crash or a kill can leave a stopped VM with a
// slot whose delete is pending, which the config endpoint hides, while
// destroy_vm frees owned drives from the current config. A slot whose pending
// value names another volume is refused retriably, as on the legacy path,
// because the stop before this read has had its chance to apply the change.
func detachManagedPersistentForVMDelete(ctx context.Context, deps Deps, node string, vmid int, ownedVolumes map[string]bool, handle *aj.Handle) error {
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		return fmt.Errorf("read VM disks before preservation: %w", err)
	}
	if err := refusePendingDriveReplacement("delete_vm", strconv.Itoa(vmid), holding); err != nil {
		return err
	}
	disks, err := managedVMPreservationCandidates(ctx, deps, node, vmid, holding.Config, ownedVolumes)
	if err != nil {
		return err
	}
	for i := range disks {
		if err := detachManagedPersistentForVMDeleteOne(ctx, deps, node, vmid, disks[i], handle); err != nil {
			return err
		}
	}
	holding, err = pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		return fmt.Errorf("read VM after persistent disk preservation: %w", err)
	}
	if err := refusePendingDriveReplacement("delete_vm", strconv.Itoa(vmid), holding); err != nil {
		return err
	}
	volumes, err := managedVMConfigVolumes(holding.Config)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if !ownedVolumes[volume] {
			return fmt.Errorf("VM still references a volume outside its recorded allocation")
		}
	}
	return nil
}

func managedVMConfigVolumes(config map[string]any) (map[string]string, error) {
	volumes := map[string]string{}
	for slot, raw := range config {
		if !managedVMVolumeSlot.MatchString(slot) {
			continue
		}
		value, ok := pve.ConfigStringValue(raw)
		if !ok || value == "" {
			return nil, fmt.Errorf("invalid VM volume entry")
		}
		volume := strings.Split(value, ",")[0]
		if volume == "none" || volume == "cdrom" {
			continue
		}
		if _, _, err := pve.ParseDiskCID(volume); err != nil {
			return nil, fmt.Errorf("unrecognized VM volume reference")
		}
		volumes[slot] = volume
	}
	return volumes, nil
}

func managedVMPreservationCandidates(ctx context.Context, deps Deps, node string, vmid int, config map[string]any, owned map[string]bool) ([]resolvedDisk, error) {
	volumes, err := managedVMConfigVolumes(config)
	if err != nil {
		return nil, err
	}
	description, _ := pve.ConfigString(config, "description")
	provenance, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return nil, err
	}
	_, sentinel := pve.ParseSentinel(description)
	recorded := map[string]string{}
	if raw, exists := sentinel["bosh_attached_disks"]; exists {
		if err := json.Unmarshal(raw, &recorded); err != nil || recorded == nil {
			return nil, fmt.Errorf("invalid recorded persistent disk CIDs")
		}
	}
	slots := make([]string, 0, len(volumes))
	for slot := range volumes {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	seen := map[string]bool{}
	var result []resolvedDisk
	for _, slot := range slots {
		volume := volumes[slot]
		if seen[volume] {
			return nil, fmt.Errorf("ambiguous duplicate VM volume reference")
		}
		seen[volume] = true
		if owned[volume] {
			continue
		}
		disk, err := managedVMPreservationCandidate(ctx, deps, node, vmid, config, slot, volume, provenance, recorded)
		if err != nil {
			return nil, err
		}
		result = append(result, disk)
	}
	return result, nil
}

func detachManagedPersistentForVMDeleteOne(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk, handle *aj.Handle) (operationErr error) {
	// Both preservations below take the parker lock through a guarded client,
	// so both get the managed wait, the legacy one included.
	ctx = managedLockWaitContext(ctx)
	if disk.allocation == nil {
		return preserveLegacyDiskForVMDelete(ctx, deps, node, vmid, disk, handle)
	}
	local, lifecycle, err := managedDiskOperation(ctx, deps, disk, "delete_vm.preserve_disk")
	if err != nil {
		return protectionPendingPreservation(vmid, err)
	}
	if lifecycle == nil {
		return fmt.Errorf("persistent disk preservation did not acquire allocation ownership")
	}
	defer func() { operationErr = lifecycle.finish(ctx, operationErr, false) }()
	current := lifecycle.disk
	if current.holder == nil || current.holder.Node != node || current.holder.VMID != vmid || current.volid != disk.volid {
		return fmt.Errorf("persistent disk ownership changed before preservation")
	}
	return handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, current)
}

// preserveLegacyDiskForVMDelete borrows the already admitted VM handle. It
// never closes or terminalizes that allocation, and marks every disk step
// external so its actual volume lineage cannot authorize VM-owned deletion.
func preserveLegacyDiskForVMDelete(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk, handle *aj.Handle) error {
	if handle == nil || handle.Record().Kind != "vm" || handle.Record().State != aj.Observed {
		return fmt.Errorf("legacy preservation requires an admitted VM journal handle")
	}
	storage, _, err := pve.ParseDiskCID(disk.volid)
	if err != nil {
		return err
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return err
	}
	present, err := observeManagedDiskVolume(ctx, deps, node, disk.volid, disk.meta)
	if err != nil || !present {
		return fmt.Errorf("legacy preservation requires observed live volume")
	}
	session := &storageLifecycle{handle: handle, operation: "delete_vm_preserve_legacy"}
	lifecycle := &managedDiskLifecycle{external: true, externalNode: node, externalBacking: backing, requestContext: ctx, deps: deps, disk: disk, handle: handle, session: session}
	guard, err := newManagedDiskLifecycleGuard(lifecycle)
	if err != nil {
		return err
	}
	lifecycle.guard = guard
	local := deps
	local.PVE = wrapManagedDiskClient(guard, lifecycle)
	current, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_legacy", disk.diskCID, disk.birth, disk.meta)
	if err == nil && disk.stableID == "" {
		current = disk
	}
	if err == nil && (current.holder == nil || current.holder.Node != node || current.holder.VMID != vmid || current.volid != disk.volid || current.intent != nil) {
		err = fmt.Errorf("legacy disk ownership changed before preservation")
	}
	if err == nil {
		if disk.stableID == "" {
			err = unlinkLegacyPersistentForVMDelete(ctx, local, node, vmid, current)
		} else {
			err = handleDetachStableID(ctx, local, strconv.Itoa(vmid), vmid, current)
		}
	}
	err = errors.Join(err, guard.Err())
	if err != nil && lifecycle.cleanLockTimeout(err) {
		// The parker lock wait ran out before this preservation changed the
		// disk, and every step on the VM's record is observed. The disk is
		// where it was, so nothing here is uncertain, and delete_vm's own
		// cleanup rule reads the marker.
		return &diskReturnedAfterLockTimeout{err: err}
	}
	if err == nil {
		if disk.stableID != "" {
			err = verifyLegacyDiskPreservation(ctx, deps, disk)
		} else {
			present, readErr := observeManagedDiskVolume(ctx, deps, node, disk.volid, disk.meta)
			if readErr != nil || !present {
				err = fmt.Errorf("legacy unlinked volume not observed")
			}
		}
	}
	if err != nil {
		deps.recordStorageReconciliation(ctx, "required")
		return errors.Join(err, session.Uncertain("legacy disk preservation incomplete"))
	}
	return nil
}

func verifyLegacyDiskPreservation(ctx context.Context, deps Deps, disk resolvedDisk) error {
	current, err := resolveDiskForOp(ctx, deps, "delete_vm.preserve_legacy_complete", disk.diskCID, disk.birth, disk.meta)
	if err != nil {
		return err
	}
	if current.holder == nil || !current.holder.IsParker || current.intent != nil {
		return fmt.Errorf("legacy disk preservation has no committed parker")
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, current.holder.Node, current.holder.VMID)
	if err != nil {
		return err
	}
	_, records := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var entries map[string]struct {
		DiskCID string `json:"disk_cid"`
		Volid   string `json:"volid"`
		Node    string `json:"node"`
		Slot    string `json:"slot"`
	}
	if err := json.Unmarshal(records["bosh_parked_disks"], &entries); err != nil {
		return fmt.Errorf("legacy parker provenance unreadable")
	}
	entry, ok := entries[disk.stableID]
	if !ok || entry.DiskCID != disk.diskCID || entry.Volid != current.volid || entry.Node != current.holder.Node || entry.Slot == "" {
		return fmt.Errorf("legacy parker provenance differs from preserved identity")
	}
	if err := managedAttachedVolume(cfg, entry.Slot, current.volid, disk.stableID); err != nil {
		return err
	}
	present, err := observeManagedDiskVolume(ctx, deps, current.holder.Node, current.volid, disk.meta)
	if err != nil || !present {
		return fmt.Errorf("legacy parked volume not observed")
	}
	return nil
}

// An active non-force config delete cannot free an ordinary disk. If PVE
// registers an unused entry, the VM owns its physical volume: deleting that
// entry would free it, so preserve it and stop instead of sweeping.
//
// Both reads take both of PVE's views, so a slot a crash left with its delete
// pending still counts as the disk's slot, and the delete sent to it applies
// at once on the stopped VM.
func unlinkLegacyPersistentForVMDelete(ctx context.Context, deps Deps, node string, vmid int, disk resolvedDisk) error {
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		return err
	}
	if err := refusePendingDriveReplacement("delete_vm", strconv.Itoa(vmid), holding); err != nil {
		return err
	}
	cfg := holding.Config
	volumes, err := managedVMConfigVolumes(cfg)
	if err != nil {
		return err
	}
	slot := ""
	for key, volume := range volumes {
		if volume != disk.volid {
			continue
		}
		if slot != "" || strings.HasPrefix(key, "unused") {
			return fmt.Errorf("legacy disk cannot be safely unlinked from this VM")
		}
		slot = key
	}
	if slot == "" {
		return fmt.Errorf("legacy disk moved before preservation")
	}
	if err := managedDeleteSlot(ctx, deps.PVE, node, vmid, slot, cfg); err != nil {
		return err
	}
	after, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		return err
	}
	if err := refusePendingDriveReplacement("delete_vm", strconv.Itoa(vmid), after); err != nil {
		return err
	}
	remaining, err := managedVMConfigVolumes(after.Config)
	if err != nil {
		return err
	}
	for _, volume := range remaining {
		if volume == disk.volid {
			return fmt.Errorf("legacy disk remains VM-owned; safe preservation requires reconciliation")
		}
	}
	return nil
}

func managedVMPreservationCandidate(ctx context.Context, deps Deps, node string, vmid int, config map[string]any, slot, volume string, provenance map[string]pve.DiskAllocationProvenance, recorded map[string]string) (resolvedDisk, error) {
	var err error
	value, _ := pve.ConfigString(config, slot)
	token, _ := pve.StableIDFromDriveOptStr(value)
	for key, entry := range provenance {
		if entry.Volid != volume {
			continue
		}
		if token != "" && token != key {
			return resolvedDisk{}, fmt.Errorf("conflicting persistent disk identity")
		}
		token = key
	}
	cid := recorded[token]
	if cid == "" {
		cid = recorded[volume]
	}
	// A disk the VM's notes don't record may still be one the allocation
	// journal knows, and its CID then comes from the journal (see
	// managedVMJournalDiskCID). The first resolution of a disk with a stable
	// identity defers a missing holder entry to the disk's lifecycle, which
	// writes the entry under the disk's allocation lock before it preserves
	// the disk, as attach_disk and detach_disk do. Without that, a retry of
	// delete_vm would meet the same refusal, and the Director sends no other
	// call for the disk of an instance it is deleting.
	resolveCtx := ctx
	if token != "" {
		resolveCtx = withHolderHeal(ctx, holderHealDefer)
	}
	if cid == "" && token != "" {
		journaled, lookupErr := managedVMJournalDiskCID(deps, vmid, volume, token)
		if lookupErr != nil {
			return resolvedDisk{}, lookupErr
		}
		if journaled != "" {
			cid = journaled
		}
	}
	if cid == "" {
		cid, err = pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
		if err != nil {
			return resolvedDisk{}, err
		}
	}
	birth, meta, err := decodeDiskCID(ctx, deps, "delete_vm.preserve_disk", cid)
	if err != nil {
		return resolvedDisk{}, err
	}
	if token != "" {
		if meta == nil {
			meta = &pve.DiskCIDMeta{ID: token}
		} else if meta.ID != token {
			return resolvedDisk{}, fmt.Errorf("recorded persistent disk CID contradicts live identity")
		}
	}
	disk, err := resolveDiskForOp(resolveCtx, deps, "delete_vm.preserve_disk", cid, birth, meta)
	if err != nil {
		return resolvedDisk{}, err
	}
	if disk.stableID == "" && disk.allocation == nil {
		if recorded[volume] == "" || birth != volume || strings.HasPrefix(slot, "unused") {
			return resolvedDisk{}, fmt.Errorf("legacy disk lacks safe active-volume preservation proof")
		}
		present, err := observeManagedDiskVolume(ctx, deps, node, volume, meta)
		if err != nil || !present {
			return resolvedDisk{}, fmt.Errorf("legacy persistent volume is not observed")
		}
		disk.holder = &pve.DiskHolder{Found: true, Node: node, VMID: vmid, Slot: slot}
	}
	_, holderRecorded := provenance[token]
	if err := managedVMCandidateOwnership(disk, node, vmid, volume, holderRecorded); err != nil {
		return resolvedDisk{}, err
	}
	if disk.allocation == nil && recorded[token] == "" && recorded[volume] == "" {
		return resolvedDisk{}, fmt.Errorf("legacy persistent disk lacks its recorded CPI CID")
	}
	return disk, nil
}

// managedVMCandidateOwnership checks that disk, resolved for the slot on VM
// vmid that names volume, is held by that VM on node and by nothing else. A
// disk with a transfer to a parker in flight fails the check.
//
// The current resolver never returns a found holder together with a transfer
// intent, because it reads the intent only when no slot carries the disk's
// serial, so the branch for a journal-managed disk that VM vmid holds without
// its provenance entry while a transfer is in flight is a guard in case that
// changes. It returns the retriable refusal the holder heal returns for the
// same state (see healUnrecordedHolder). A disk whose entry VM vmid does
// carry keeps the plain refusal.
func managedVMCandidateOwnership(disk resolvedDisk, node string, vmid int, volume string, holderRecorded bool) error {
	held := disk.holder != nil && disk.holder.Node == node && disk.holder.VMID == vmid && disk.volid == volume
	if held && disk.intent != nil && disk.allocation != nil && !holderRecorded {
		return holderNotRecordedRefusal(disk, transferInFlightReason(disk.intent))
	}
	if !held || disk.intent != nil {
		return fmt.Errorf("persistent disk has no unambiguous current ownership proof")
	}
	return nil
}

// journalDiskCIDUnread is managedVMJournalDiskCID's error when the allocation
// journal can't be read. Its text and CPI type are the retriable error it
// carries, and its type tells a journal-managed delete_vm that the
// preservation changed nothing (see isWholePreservationRefusal).
type journalDiskCIDUnread struct{ err error }

func (e *journalDiskCIDUnread) Error() string { return e.err.Error() }

func (e *journalDiskCIDUnread) Unwrap() error { return e.err }

// isWholePreservationRefusal reports whether err is, and is only, one of the
// refusals delete_vm's preservation returns before it changes a disk. Those
// are the holder heal's refusal for a disk whose holder lacks its provenance
// entry, the failed journal read that looks up such a disk's CID, and the
// lifecycle's refusal while a parker protection write of the disk's record
// waits to be settled (see protectionPendingPreservation).
func isWholePreservationRefusal(err error) bool {
	return isWholeRefusal(err, func(err error) bool {
		switch err.(type) { //nolint:errorlint // isWholeRefusal unwraps one error at a time and hands each one here.
		case *holderNotRecorded, *journalDiskCIDUnread, *protectionPendingRefusal:
			return true
		}
		return false
	})
}

// isWholeRefusal reports whether err is, and is only, an error that match
// accepts. It follows single-error unwrapping and a join that holds one
// error, which is how the disk's lifecycle hands back a refusal after it
// closes the journal cleanly. It gives up at a join of several errors,
// because a refusal joined with another failure, such as a journal close that
// failed or the reconciliation a preservation records, isn't a refusal that
// changed nothing.
func isWholeRefusal(err error, match func(error) bool) bool {
	for err != nil {
		if match(err) {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok { //nolint:errorlint // The walk unwraps one error at a time itself, so it can stop at a join of several errors, which errors.As would search through.
			errs := joined.Unwrap()
			if len(errs) != 1 {
				return false
			}
			err = errs[0]
			continue
		}
		err = errors.Unwrap(err)
	}
	return false
}

// protectionPendingPreservation returns err as a retriable error when the
// disk's lifecycle refused the disk before it changed anything, and the only
// reason is a parker protection write of the disk's record that the settler
// left planned (protectionPendingOr). The disk is where its record says, so a
// retry settles the step by readback once the parker reads back protected,
// which happens when another operation releases the parker's lock or when we
// run the qm set command the refusal names. A refusal that parkerLockBusyOr
// already made retriable, and every other error, is returned unchanged.
func protectionPendingPreservation(vmid int, err error) error {
	pending := isWholeRefusal(err, func(err error) bool {
		_, ok := err.(*protectionPendingRefusal) //nolint:errorlint // isWholeRefusal unwraps one error at a time and hands each one here.
		return ok
	})
	if !pending || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		return err
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("a persistent disk stays on VM %d until the parker protection write in the disk's record is settled, so retry once the parker reads back protected", vmid))
}

// managedVMJournalDiskCID returns the CID that the allocation journal records
// for the disk whose stable token is token, or "" when no live disk record
// names the token. delete_vm needs it for a disk that attach_disk moved onto
// the VM and then left without the VM's provenance entry and without the
// disk's CID in the VM's notes, because the holder write failed. A CID built
// from the volume's new name resolves that disk as a legacy disk with no
// allocation, which every retry refuses, and the Director sends no
// attach_disk or detach_disk that would write the entry for an instance it is
// deleting. The journal's CID resolves it as the managed disk it is.
//
// A deployment without an enrolled journal, a token that no disk record
// names, and a terminal record find nothing, so those disks keep the refusal
// they had. A journal that is enrolled but can't be read is retriable,
// because nothing then shows that the journal doesn't know the disk.
func managedVMJournalDiskCID(deps Deps, vmid int, volume, token string) (string, error) {
	if deps.Config == nil {
		return "", nil
	}
	directory := strings.TrimSpace(deps.Config.StorageAllocationJournalDir)
	namespace := strings.TrimSpace(deps.Config.StoragePlacementNamespace)
	if directory == "" || namespace == "" {
		return "", nil
	}
	record, found, err := inspectAllocationJournalDiskToken(directory, namespace, token)
	if err != nil {
		return "", &journalDiskCIDUnread{err: cpierrors.Retriable("delete_vm: couldn't read the allocation journal to find the CID of persistent disk %s on VM %d (%v); retry delete_vm once the journal can be read", volume, vmid, err)}
	}
	if !found || record.Kind != allocationKindDisk || record.DiskToken != token || record.State == aj.Deleted || record.State == aj.Cleaned {
		return "", nil
	}
	return record.CID, nil
}
