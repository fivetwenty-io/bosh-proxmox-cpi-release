package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedVMRetentionStepPrefix starts the kind of every step a VM's ephemeral
// retention writes through its lifecycle guard.
const managedVMRetentionStepPrefix = "lifecycle_delete_vm_retain_ephemeral_"

// managedVMRetentionToken is the stable ID a VM allocation's ephemeral
// retention writes as the volume's serial. It is derived from the record ID,
// so the serial names this allocation and nothing else.
func managedVMRetentionToken(recordID string) string {
	sum := sha256.Sum256([]byte("vm-ephemeral-retention\x00" + recordID))
	return "bpd-" + hex.EncodeToString(sum[:8])
}

// managedVMRetentionStarted reports whether the record holds an observed
// retention config write aimed at the guest's own VMID. The first such write
// is the serial write, so the retention started on this guest. A step aimed
// at a parker never counts.
func managedVMRetentionStarted(record aj.Record, vmid int) bool {
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.Attempt == record.ActiveAttempt() && step.Kind == managedVMRetentionStepPrefix+"Nodes_UpdateQemuConfig" && step.State == aj.Observed && !step.Target.External && vmid > 0 && step.Target.VMID == vmid {
			return true
		}
	}
	return false
}

// managedVMConfigCarriesSerial reports whether any volume drive in config
// carries serial as its stable ID.
func managedVMConfigCarriesSerial(config map[string]any, serial string) bool {
	for key, value := range config {
		if !managedVMVolumeDevice(key) {
			continue
		}
		drive, ok := pve.ConfigStringValue(value)
		if !ok {
			continue
		}
		if id, has := pve.StableIDFromDriveOptStr(drive); has && id == serial {
			return true
		}
	}
	return false
}

// managedVMDisposalRetains decides whether a disposal keeps the guest's
// ephemeral volume. A create_vm rollback never does. A deletion does when its
// caller asked, when the retention already started, which is an observed
// serial write aimed at the guest or our token on one of its drives, or when
// delete_vm would have retained, which is a CID the Director received and the
// retain tag on the live guest. guest is the live guest's config, or nil when
// there is no live guest.
func managedVMDisposalRetains(disposal managedVMDisposal, requested bool, record aj.Record, vmid int, guest map[string]any) bool {
	if disposal != managedVMDeletion {
		return false
	}
	if requested || managedVMRetentionStarted(record, vmid) {
		return true
	}
	if guest == nil {
		return false
	}
	if managedVMConfigCarriesSerial(guest, managedVMRetentionToken(record.ID)) {
		return true
	}
	tags, _ := pve.ConfigString(guest, jsonKeyTags)
	return record.CID != "" && tagsContain(tags, tagRetainEphemeral)
}

// managedVMRetentionParkers groups the targets of the observed retention
// steps that name a parker rather than the guest, by parker VMID, in VMID
// order.
func managedVMRetentionParkers(record aj.Record, guestVMID int) ([]int, map[int][]aj.Target) {
	targets := map[int][]aj.Target{}
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.Attempt != record.ActiveAttempt() || !strings.HasPrefix(step.Kind, managedVMRetentionStepPrefix) || step.State != aj.Observed || step.Target.External || step.Target.VMID <= 0 || step.Target.VMID == guestVMID || step.Target.IntendedVolume == "" {
			continue
		}
		known := false
		for _, target := range targets[step.Target.VMID] {
			known = known || target == step.Target
		}
		if !known {
			targets[step.Target.VMID] = append(targets[step.Target.VMID], step.Target)
		}
	}
	order := make([]int, 0, len(targets))
	for vmid := range targets {
		order = append(order, vmid)
	}
	sort.Ints(order)
	return order, targets
}

// managedVMRetentionVolumes returns every volume the record's ephemeral
// binding and retention steps name, under either its birth or landed name.
func managedVMRetentionVolumes(record aj.Record) map[string]bool {
	volumes := map[string]bool{}
	for index := range record.Steps {
		step := &record.Steps[index]
		if !strings.HasPrefix(step.Kind, managedVMRetentionStepPrefix) && !strings.HasPrefix(step.Kind, "vm.ephemeral.") {
			continue
		}
		if step.Target.IntendedVolume != "" {
			volumes[step.Target.IntendedVolume] = true
		}
		for _, volume := range step.VolIDs {
			volumes[volume] = true
		}
	}
	return volumes
}

// managedVMParkerHoldsRetention reads a parker back and reports whether it
// holds anything of this retention. That is a drive carrying our token as its
// serial or holding a volume the record names, or a bosh_parked_disks entry
// for our token that names a slot. A slotless entry is the stamp the guard
// writes when it creates a holder, which proves nothing.
//
// A parker that reads back gone holds nothing of ours. Gone is what the pve
// package itself calls it, a not-found answer or pmxcfs's missing config
// file. If such a parker took our volume with it, the guest no longer holds
// the volume with our serial either, so resume admission refuses. Any other
// read error refuses here.
func managedVMParkerHoldsRetention(ctx context.Context, deps Deps, node string, vmid int, token string, volumes map[string]bool) (bool, error) {
	config, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
		return false, nil
	}
	if err != nil || config == nil {
		return false, storageRefusal(managedVMRetentionParkerUnreadable)
	}
	if managedVMConfigCarriesSerial(config, token) {
		return true, nil
	}
	held, err := managedVMConfigVolumes(config)
	if err != nil {
		return false, err
	}
	for _, volume := range held {
		if volumes[volume] {
			return true, nil
		}
	}
	return managedVMParkerEntrySlotted(config, token)
}

// managedVMRetentionParkerUnreadable refuses a retention whose recorded parker
// could not be read back for any reason other than being gone.
const managedVMRetentionParkerUnreadable = "a retention parker the record names could not be read"

// managedVMParkerEntrySlotted reports whether the parker's bosh_parked_disks
// entry for token names a slot.
func managedVMParkerEntrySlotted(config map[string]any, token string) (bool, error) {
	_, sentinel := pve.ParseSentinel(pve.DescriptionFromConfig(config))
	raw, ok := sentinel["bosh_parked_disks"]
	if !ok {
		return false, nil
	}
	var entries map[string]struct {
		Slot string `json:"slot"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false, fmt.Errorf("retention parker provenance is malformed")
	}
	entry, ok := entries[token]
	return ok && entry.Slot != "", nil
}

// managedVMTransferredRetention applies the transfer evidence rule to every
// parker an observed retention step names. It first reads the parker back,
// and a parker that is gone or holds nothing of this retention is passed
// over. A parker that holds something counts as the transfer only when its
// entry for our token names a slot and the drive at that slot holds the
// recorded volume with our serial, which resolveManagedRetainedEphemeral
// reads back. Anything in between refuses. It returns the one transferred
// target, nil when no parker holds the transfer, or a refusal.
func managedVMTransferredRetention(ctx context.Context, deps Deps, handle *aj.Handle, guestVMID int) (*aj.Target, error) {
	record := handle.Record()
	order, parkers := managedVMRetentionParkers(record, guestVMID)
	token := managedVMRetentionToken(record.ID)
	volumes := managedVMRetentionVolumes(record)
	var found *aj.Target
	for _, vmid := range order {
		targets := parkers[vmid]
		holds, err := managedVMParkerHoldsRetention(ctx, deps, targets[0].Node, vmid, token, volumes)
		if err != nil {
			return nil, err
		}
		if !holds {
			continue
		}
		var refusal error
		var transferred *aj.Target
		for index := range targets {
			_, err := resolveManagedRetainedEphemeral(ctx, deps, handle, targets[index])
			if err == nil {
				target := targets[index]
				transferred = &target
				break
			}
			if refusal == nil {
				refusal = err
			}
		}
		if transferred == nil {
			return nil, refusal
		}
		if found != nil {
			return nil, storageRefusal("ambiguous ephemeral retention target")
		}
		found = transferred
	}
	return found, nil
}

// managedRetentionTimeoutCheck holds what an ephemeral retention wrote before
// it waited for the parker lock, so that a timeout can be judged clean.
type managedRetentionTimeoutCheck struct {
	// start is the number of steps the record held when the retention began.
	start   int
	node    string
	vmid    int
	slot    string
	written string
	volume  string
	cid     string
	token   string
}

// returned reports whether a retention that failed with err changed nothing
// but what a resume accepts, so the record can go back to the Director as a
// retriable timeout instead of requiring reconciliation. It takes three of
// cleanLockTimeout's conditions as they are: a lock-wait error, a clean
// guard, and every step settled. The fourth, that no disk mutation was
// admitted, cannot hold, because the serial write, the CID description write,
// and the parker create are all admitted before the wait. So it proves which
// mutations were sent and reads back what each one left.
//
// The guard journals every mutation before it sends it, so the steps after
// start are everything that reached PVE. They may be at most two config writes
// aimed at the guest, at most one holder create aimed elsewhere, and lock
// sentinel writes. Anything else, such as a slot delete, a move, an attach, or
// a config write aimed at a parker, means the lock window was entered. The
// readback then requires the guest's slot to carry exactly the drive string
// the retention wrote, the volume to be present, the guest's CID entry for our
// token and allocation marker to be ours, every created holder to be empty
// with only a slotless entry for our token, and the token to resolve to the
// guest with no intent. Any read that fails makes the timeout unclean.
func (c *managedRetentionTimeoutCheck) returned(ctx context.Context, deps Deps, record aj.Record, err, guardErr error) bool {
	if c == nil || err == nil || guardErr != nil {
		return false
	}
	if !errors.Is(err, pve.ErrClusterLockTimeout) && !errors.Is(err, pve.ErrClusterLockStateUnknown) &&
		!errors.Is(err, pve.ErrClusterLockInterrupted) && !errors.Is(err, errManagedRequestEnded) {
		return false
	}
	if storageLifecycleSettled(record) != nil || c.start < 0 || c.start > len(record.Steps) {
		return false
	}
	guestWrites, creates := 0, []aj.Target{}
	for index := c.start; index < len(record.Steps); index++ {
		step := record.Steps[index]
		switch step.Kind {
		case managedVMRetentionStepPrefix + "Nodes_UpdateQemuConfig":
			if step.Target.VMID != c.vmid {
				return false
			}
			guestWrites++
		case managedVMRetentionStepPrefix + "QEMU_Create":
			if step.Target.VMID <= 0 || step.Target.VMID == c.vmid {
				return false
			}
			creates = append(creates, step.Target)
		case managedVMRetentionStepPrefix + "Pool_CreatePool", managedVMRetentionStepPrefix + "Pool_DeletePool":
			if step.Target.VMID != 0 {
				return false
			}
		default:
			return false
		}
	}
	if guestWrites > 2 || len(creates) > 1 {
		return false
	}
	if ctx.Err() != nil {
		detached, cancel := detachedContext(ctx, pve.ClusterLockCompletionAllowance)
		defer cancel()
		ctx = detached
	}
	return c.readBack(ctx, deps, record, creates)
}

func (c *managedRetentionTimeoutCheck) readBack(ctx context.Context, deps Deps, record aj.Record, creates []aj.Target) bool {
	config, err := deps.PVE.QEMU().Config(ctx, c.node, c.vmid)
	if err != nil || config == nil {
		return false
	}
	if drive, ok := pve.ConfigString(config, c.slot); !ok || drive != c.written {
		return false
	}
	volumes, err := managedVMConfigVolumes(config)
	if err != nil {
		return false
	}
	for slot, volume := range volumes {
		if volume == c.volume && slot != c.slot {
			return false
		}
	}
	present, err := observeManagedDiskVolume(ctx, deps, c.node, c.volume, nil)
	if err != nil || !present {
		return false
	}
	if pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(config))[c.token] != c.cid || !managedVMMarkerMatches(config, record) {
		return false
	}
	for _, created := range creates {
		holder, err := deps.PVE.QEMU().Config(ctx, created.Node, created.VMID)
		if err != nil || holder == nil || lifecycleConfigHasAnyVolume(holder) {
			return false
		}
		if slotted, err := managedVMParkerEntrySlotted(holder, c.token); err != nil || slotted {
			return false
		}
	}
	storage, _, err := pve.ParseDiskCID(c.volume)
	if err != nil {
		return false
	}
	identity, err := pve.ResolveDiskIdentity(ctx, deps.PVE, deps.Log(ctx), storage+":bosh-retention-probe-"+record.ID, c.token, parkerReadConfigFor(deps))
	return err == nil && identity.Intent == nil && identity.Volid == c.volume && identity.Holder.Found && !identity.Holder.IsParker &&
		identity.Holder.VMID == c.vmid && identity.Holder.Node == c.node
}
