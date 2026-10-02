package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// detachTailAttempts bounds how many times the detach tail reads the source VM
// again after its removal met a configuration that changed under it.
const detachTailAttempts = 3

// errManagedDescriptionDigestStale marks a description-only configuration
// write that the lifecycle guard refused because the digest it carried no
// longer matched the guard's own read. Nothing was journaled or sent, so begin
// hands the refusal back as not attempted and leaves the guard usable, and the
// caller reads the VM again.
var errManagedDescriptionDigestStale = errors.New("managed config generation changed before a description write")

// finishDetachTail removes the allocation entry, the attached-disk entry, and
// the drive-option overlay that a managed disk's source VM still carries once
// the disk has landed on a parker. A detach that finishes its move at once
// removes them itself, and this finishes the job for a move that landed some
// other way, through a resume or before a release that removed them. rd is the
// disk after the landing, so its volid is the landed one.
//
// It runs only inside the disk's lifecycle, under the allocation journal's lock
// and the lifecycle guard, and never takes the source VM's lock. A source VM
// that's gone, or that no longer carries the entry, leaves nothing to do.
func finishDetachTail(ctx context.Context, deps Deps, op string, rd resolvedDisk, sourceVMID int) error {
	if _, managed := deps.PVE.(*managedDiskLifecycleClient); !managed || rd.allocation == nil || sourceVMID <= 0 {
		return nil
	}
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, sourceVMID)
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: locate VM %d to remove disk %s's allocation entry", op, sourceVMID, rd.diskCID))
	}
	if !location.Found {
		return nil
	}
	return finishDetachTailOn(ctx, deps, op, rd, location.Node, sourceVMID)
}

// finishDetachTailOn is finishDetachTail for a source VM whose node is known.
// A removal that meets a changed configuration, either in the guard's own read
// or in PVE's answer, starts again from a fresh read, a bounded number of
// times. Every other failure comes back retriable unless it already carries a
// class, so the next call heals what this one could not.
func finishDetachTailOn(ctx context.Context, deps Deps, op string, rd resolvedDisk, node string, sourceVMID int) error {
	managed, ok := deps.PVE.(*managedDiskLifecycleClient)
	if !ok || rd.allocation == nil || sourceVMID <= 0 {
		return nil
	}
	for attempt := 1; ; attempt++ {
		err := removeDetachTailEntry(ctx, deps, managed.lifecycle.handle.Record(), op, rd, node, sourceVMID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errManagedDescriptionDigestStale) && !pve.IsConfigDigestRefusal(err) {
			return retriableUnlessPermanent(err, fmt.Sprintf("%s: remove disk %s's allocation entry from VM %d", op, rd.diskCID, sourceVMID))
		}
		if attempt == detachTailAttempts {
			return cpierrors.Retriable("%s: VM %d's configuration changed during each of %d attempts to remove disk %s's allocation entry",
				op, sourceVMID, detachTailAttempts, rd.diskCID)
		}
	}
}

// removeDetachTailEntry makes one attempt at the tail. It reads the source VM
// in both views, and from that one read it builds a single description without
// the allocation entry, the attached-disk entries, and the drive-option
// overlays, which it sends with the read's digest. So a description that
// another disk's lifecycle changed since the read is refused rather than
// overwritten, and the tail succeeds only when that one write lands. An entry
// that names neither the landed volid nor the source volid of one of the
// record's own observed moves isn't provably this disk's, so it stays where it
// is and the call goes on, which leaves the deletion proof to refuse it as
// before. A source VM that still names the volume on a slot, an unused entry,
// or a pending change is refused, because the disk hasn't left it.
func removeDetachTailEntry(ctx context.Context, deps Deps, record aj.Record, op string, rd resolvedDisk, node string, sourceVMID int) error {
	views, err := pve.ReadQemuViews(ctx, deps.PVE, node, sourceVMID)
	if err != nil {
		return detachTailReadFailure(ctx, deps, err, sourceVMID)
	}
	config := views.Applied()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(config))
	if err != nil {
		return err
	}
	key := rd.sentinelKey()
	entry, found := entries[key]
	if !found {
		return nil
	}
	expected := rd.allocation.provenance
	expected.Volid = entry.Volid
	named, renamed := detachTailNamesDisk(record, entry, rd.volid, sourceVMID)
	if entry != expected || !named {
		deps.Log(ctx).Warn(op+": the source VM's allocation entry doesn't match the landed disk; leaving it in place",
			log.String("allocation_id", rd.allocation.record.ID),
			log.String("disk_cid", rd.diskCID),
			log.Int("vmid", sourceVMID),
			log.String("entry_volid", entry.Volid),
			log.String("landed_volid", rd.volid),
		)
		return nil
	}
	// A source that names the landed volid still holds the volume. So does a
	// source that names the volid the entry records, unless an observed move
	// landed the disk under another name, because then the name is no longer
	// ours and the held set below keeps it out of the removal.
	slots := views.SlotsNaming(rd.volid)
	if entry.Volid != rd.volid && !renamed {
		slots = append(slots, views.SlotsNaming(entry.Volid)...)
	}
	if len(slots) > 0 {
		return cpierrors.Retriable("VM %d still names disk %s's volume on %s", sourceVMID, rd.diskCID, strings.Join(slots, ", "))
	}
	// A name the source still holds on a slot or an unused entry belongs to
	// whatever disk sits there now, which after a rename can be another disk
	// that PVE gave the old name. We work out those names once, from this
	// read, and no removal below gets any of them. When the entry itself is
	// filed under such a name, we can't remove it without touching that disk's
	// records, so we leave it for the deletion proof to refuse.
	held := map[string]bool{}
	for _, name := range []string{key, rd.stableID, entry.Volid, rd.birth} {
		if name != "" && views.NamesVolume(name) {
			held[name] = true
		}
	}
	if held[key] {
		deps.Log(ctx).Warn(op+": the source VM holds a volume under the name its allocation entry is filed under; leaving it in place",
			log.String("allocation_id", rd.allocation.record.ID),
			log.String("disk_cid", rd.diskCID),
			log.Int("vmid", sourceVMID),
			log.String("entry_key", key),
		)
		return nil
	}
	attachedKeys := unheldNames(held, rd.stableID, entry.Volid)
	overlayKeys := unheldNames(held, rd.stableID, entry.Volid, rd.birth)
	return pve.RemoveDiskAllocationEntry(ctx, deps.PVE, node, sourceVMID, key, entry, config, attachedKeys, overlayKeys)
}

// detachTailReadFailure decides what a failed read of the source VM means. We
// count it as absence only when it has the shape that parkerConfigGone in
// pve/parker.go reads as a VM whose config is gone, and a fresh lookup then
// agrees that the VM isn't there. Any other failure, including a 404 for a VM
// the lookup still finds, comes back retriable, so the next call tries again.
func detachTailReadFailure(ctx context.Context, deps Deps, err error, sourceVMID int) error {
	if !pve.VMConfigGone(err) {
		return err
	}
	location, lookupErr := pve.FindVMAuthoritative(ctx, deps.PVE, sourceVMID)
	if lookupErr == nil && !location.Found {
		return nil
	}
	return cpierrors.WrapAs(errors.Join(err, lookupErr), cpierrors.TypeRetriableCloud,
		fmt.Sprintf("read VM %d, which still exists, to remove its allocation entry", sourceVMID))
}

// unheldNames returns the names that aren't empty and aren't in held.
func unheldNames(held map[string]bool, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" && !held[name] {
			out = append(out, name)
		}
	}
	return out
}

// detachTailNamesDisk reports whether entry names the disk that landed as
// landed, on the node the entry records. It does when it names the landed
// volid, or when one of record's observed moves took the disk off sourceVMID on
// that node under the volid the entry names and landed it as landed. renamed
// reports whether such a move landed the disk under a name other than the one
// it set out to move.
//
// We don't check step.Attempt, so a move step from any attempt counts. That
// fails safe, because a step matches only when its first volid is the one the
// entry names and its last volid is the landed one, whichever attempt wrote it.
func detachTailNamesDisk(record aj.Record, entry pve.DiskAllocationProvenance, landed string, sourceVMID int) (named, renamed bool) {
	named = entry.Volid == landed
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed || !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
			continue
		}
		if step.Target.VMID != sourceVMID || step.Target.Node != entry.Node || len(step.VolIDs) < 2 {
			continue
		}
		if step.VolIDs[0] != entry.Volid || step.VolIDs[len(step.VolIDs)-1] != landed {
			continue
		}
		named = true
		if step.Target.IntendedVolume != "" && step.Target.IntendedVolume != landed {
			renamed = true
		}
	}
	return named, renamed
}

// finishParkedDetachTail runs the detach tail for a disk already parked, with
// the source VM the parker's landed entry names. It is how a record that an
// earlier release left with its source VM's entry in place heals.
func finishParkedDetachTail(ctx context.Context, deps Deps, op string, rd resolvedDisk) error {
	if _, managed := deps.PVE.(*managedDiskLifecycleClient); !managed || rd.allocation == nil || rd.holder == nil || !rd.holder.IsParker {
		return nil
	}
	source, found, err := pve.ReadParkerSourceVMID(ctx, deps.PVE, rd.holder.Node, rd.holder.VMID, rd.volid, rd.stableID)
	if err != nil {
		return retriableUnlessPermanent(err, fmt.Sprintf("%s: read the source VM from parker %d's entry for disk %s", op, rd.holder.VMID, rd.diskCID))
	}
	if !found {
		return nil
	}
	return finishDetachTail(ctx, deps, op, rd, source)
}

// resumeAndFinishParkedTail resumes a transfer the disk has in flight and then
// runs the detach tail for the disk once it is parked, so the source VM's
// entries are gone before the caller goes on.
func resumeAndFinishParkedTail(ctx context.Context, deps Deps, op string, rd resolvedDisk) (resolvedDisk, error) {
	rd, err := resumeTransferIfNeeded(ctx, deps, op, rd)
	if err != nil {
		return resolvedDisk{}, err
	}
	if err := finishParkedDetachTail(ctx, deps, op, rd); err != nil {
		return resolvedDisk{}, err
	}
	return rd, nil
}

// intentSourceVMID returns the source VM a transfer intent names, if any.
func intentSourceVMID(intent *pve.DiskTransferIntent) (int, bool) {
	if intent == nil {
		return 0, false
	}
	vmid, err := strconv.Atoi(intent.SourceVMCID)
	if err != nil || vmid <= 0 {
		return 0, false
	}
	return vmid, true
}

// descriptionOnlyConfigWrite reports whether call is a configuration write
// whose parameters are exactly a description and a digest.
func descriptionOnlyConfigWrite(call ManagedAllocationMutation) bool {
	if call.Service != managedServiceNodes || call.Method != "UpdateQemuConfig" {
		return false
	}
	fields, err := lifecycleMutationFields(call.Args[managedArgumentParams])
	if err != nil || len(fields) != 2 {
		return false
	}
	_, description := fields[pveConfigKeyDescription]
	_, digest := fields["digest"]
	return description && digest
}

// settleRefusedDescription settles a description-only write that PVE refused
// for a stale digest. qemu-server's update_vm_api checks the digest under the
// VM's lock before it writes anything, so the refusal changed nothing. The
// step is observed with no volume once a fresh read shows the VM still
// carrying every allocation entry the write meant to remove. Anything else,
// including a refusal PVE may not have given, stays with the guard's usual
// handling.
func (g *managedDiskLifecycleGuard) settleRefusedDescription(ctx context.Context, call ManagedAllocationMutation, step string, writeErr error) bool {
	observation, ok := g.observations[step]
	if !ok || ctx.Err() != nil || !pve.IsConfigDigestRefusal(writeErr) || !descriptionOnlyConfigWrite(call) {
		return false
	}
	before, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(observation.before))
	if err != nil {
		return false
	}
	written, ok := observation.fields[pveConfigKeyDescription].(string)
	if !ok {
		return false
	}
	sent, err := pve.ParseDiskAllocationProvenance(written)
	if err != nil {
		return false
	}
	var removed []string
	for key := range before {
		if _, kept := sent[key]; !kept {
			removed = append(removed, key)
		}
	}
	if len(removed) == 0 {
		return false
	}
	cfg, err := g.lifecycle.deps.PVE.QEMU().Config(ctx, observation.node, observation.vmid)
	if err != nil || cfg == nil {
		return false
	}
	after, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(cfg))
	if err != nil {
		return false
	}
	for _, key := range removed {
		if entry, present := after[key]; !present || entry != before[key] {
			return false
		}
	}
	if err := storageMutationObserved(g.lifecycle.handle, step, nil, false); err != nil {
		return false
	}
	delete(g.observations, step)
	return true
}
