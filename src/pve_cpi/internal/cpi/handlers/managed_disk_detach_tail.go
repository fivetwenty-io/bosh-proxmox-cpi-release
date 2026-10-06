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
// caller reads the VM again. It wraps pve.ErrConfigDigestStale, so a writer in
// the pve package, such as a parker provenance write, sees the same refusal it
// would get from PVE and retries from a fresh read.
var errManagedDescriptionDigestStale = fmt.Errorf("managed config generation changed before a description write: %w", pve.ErrConfigDigestStale)

// errManagedTailReadFailed marks the refusal of the detach tail's removal write
// when one of the lifecycle guard's own reads before that write returned an
// error. The guard sends nothing until its reads answer, so the guard's begin
// method hands the refusal back as not attempted and leaves the guard usable.
// The tail then marks the error as not sent, and the lifecycle's finish returns
// the disk's allocation unchanged for the next call.
var errManagedTailReadFailed = errors.New("managed lifecycle read failed before the detach tail's removal")

// detachTailRemovalKey is the context key the detach tail sets on its removal
// write. The guard can't tell that a description write only removes notes until
// its own read of the VM answers, so when that read fails, the key is all the
// guard has to show that the write is the tail's removal.
type detachTailRemovalKey struct{}

// withDetachTailRemoval returns ctx marked as carrying the detach tail's
// removal write.
func withDetachTailRemoval(ctx context.Context) context.Context {
	return context.WithValue(ctx, detachTailRemovalKey{}, true)
}

// tailRemovalReadFailed builds the refusal that the guard's before hook gives
// the detach tail's removal when one of the guard's reads returned err. It's
// nil for a read that answered, for any other write, and for a request whose
// context has ended, because those keep the guard's usual handling. The read's
// cause goes into the text only, so no status in it can make the refusal look
// like PVE's answer to the write. The %v also flattens the class of a typed
// cause to retriable on purpose. A failed read before the write is a read we
// can safely repeat, because nothing has been sent.
func tailRemovalReadFailed(ctx context.Context, call ManagedAllocationMutation, what string, err error) error {
	if err == nil || ctx.Err() != nil || !descriptionOnlyConfigWrite(call) {
		return nil
	}
	if marked, _ := ctx.Value(detachTailRemovalKey{}).(bool); !marked {
		return nil
	}
	return cpierrors.WrapAs(errors.Join(fmt.Errorf("cannot read %s: %v", what, err), errManagedTailReadFailed), cpierrors.TypeRetriableCloud, //nolint:errorlint // The read's cause stays as text, so its status can't read as PVE refusing the removal.
		"managed lifecycle could not check the detach tail's removal before sending it")
}

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
	managed, ok := deps.PVE.(*managedDiskLifecycleClient)
	if !ok || rd.allocation == nil || sourceVMID <= 0 {
		return nil
	}
	location, err := pve.FindVMAuthoritative(ctx, deps.PVE, sourceVMID)
	if err != nil {
		return detachTailRefused(ctx, managed, retriableUnlessPermanent(err, fmt.Sprintf("%s: locate VM %d to remove disk %s's allocation entry", op, sourceVMID, rd.diskCID)))
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
//
// A failure that left the source VM as the tail found it is marked with
// detachTailNotSent, so finish can return the disk's allocation unchanged. That
// covers a refusal from removeDetachTailEntry before its write, including one
// that a failed guard read just before that write caused, retries that ran out
// after the guard settled every refusal, and a write PVE refused outright that
// the guard settled from a readback. A write the guard couldn't settle poisons
// the guard, and that failure never gets the marker.
func finishDetachTailOn(ctx context.Context, deps Deps, op string, rd resolvedDisk, node string, sourceVMID int) error {
	managed, ok := deps.PVE.(*managedDiskLifecycleClient)
	if !ok || rd.allocation == nil || sourceVMID <= 0 {
		return nil
	}
	for attempt := 1; ; attempt++ {
		err := removeDetachTailEntry(ctx, deps, managed, op, rd, node, sourceVMID)
		if err == nil {
			return nil
		}
		if isDetachTailNotSent(err) {
			return err
		}
		if !errors.Is(err, errManagedDescriptionDigestStale) && !pve.IsConfigDigestRefusal(err) {
			wrapped := retriableUnlessPermanent(err, fmt.Sprintf("%s: remove disk %s's allocation entry from VM %d", op, rd.diskCID, sourceVMID))
			if pve.IsAnsweredConfigRefusal(err) || errors.Is(err, errManagedTailReadFailed) {
				return detachTailRefused(ctx, managed, wrapped)
			}
			return wrapped
		}
		if attempt == detachTailAttempts {
			return detachTailRefused(ctx, managed, cpierrors.Retriable("%s: VM %d's configuration changed during each of %d attempts to remove disk %s's allocation entry",
				op, sourceVMID, detachTailAttempts, rd.diskCID))
		}
	}
}

// detachTailNotSent marks a detach tail failure that left the source VM as the
// tail found it. Either the tail stopped before it sent a write, or PVE
// refused the write and the lifecycle guard settled the refusal from a
// readback. finish reads the marker to return the disk's allocation the way a
// clean lock timeout returns it, so the retry readmits the record. The marker
// wraps the original error, so its CPI type stays visible.
type detachTailNotSent struct{ err error }

func (e *detachTailNotSent) Error() string { return e.err.Error() }
func (e *detachTailNotSent) Unwrap() error { return e.err }

// isDetachTailNotSent reports whether err carries that marker.
func isDetachTailNotSent(err error) bool {
	var refused *detachTailNotSent
	return errors.As(err, &refused)
}

// detachTailRefused marks err with detachTailNotSent while the request is
// still live and the lifecycle guard is still clean. An ended request or a
// poisoned guard leaves err as it is, because then something this operation
// sent may have an unknown outcome.
func detachTailRefused(ctx context.Context, managed *managedDiskLifecycleClient, err error) error {
	if err == nil || ctx.Err() != nil || managed == nil || managed.lifecycle == nil {
		return err
	}
	if guard := managed.lifecycle.guard; guard == nil || guard.Err() != nil {
		return err
	}
	return &detachTailNotSent{err: err}
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
// or a pending change is refused, because the disk hasn't left it. So is one
// whose config carries a lock key, because PVE would refuse the write while
// another task such as a backup holds the VM.
func removeDetachTailEntry(ctx context.Context, deps Deps, managed *managedDiskLifecycleClient, op string, rd resolvedDisk, node string, sourceVMID int) error {
	views, err := pve.ReadQemuViews(ctx, deps.PVE, node, sourceVMID)
	if err != nil {
		failure := detachTailReadFailure(ctx, deps, err, sourceVMID)
		if failure == nil || ctx.Err() != nil {
			return failure
		}
		return detachTailRefused(ctx, managed, retriableUnlessPermanent(failure,
			fmt.Sprintf("%s: read VM %d to remove disk %s's allocation entry", op, sourceVMID, rd.diskCID)))
	}
	record := managed.lifecycle.handle.Record()
	config := views.Applied()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(config))
	if err != nil {
		return err
	}
	// When the allocation entry is gone, we stop here and don't look for the
	// attached-disk note or the drive-option overlay. The removal at the end
	// of this function drops all three in one write that carries the digest of
	// a read, so an entry that's absent means the other two left with it.
	key := rd.sentinelKey()
	entry, found := entries[key]
	if !found {
		return nil
	}
	expected := rd.allocation.provenance
	expected.Volid = entry.Volid
	if entry != expected || !detachTailNamesDisk(record, entry, rd.volid, sourceVMID) {
		deps.Log(ctx).Warn(op+": the source VM's allocation entry doesn't match the landed disk; leaving it in place",
			log.String("allocation_id", rd.allocation.record.ID),
			log.String("disk_cid", rd.diskCID),
			log.Int("vmid", sourceVMID),
			log.String("entry_volid", entry.Volid),
			log.String("landed_volid", rd.volid),
		)
		return nil
	}
	// A source that names the landed volid still holds the volume. When an
	// entry records another volid, it matches the disk only through an
	// observed move that took the disk off that name and landed it under the
	// new one. The old name is then no longer ours, so the held set below
	// keeps it out of the removal.
	if slots := views.SlotsNaming(rd.volid); len(slots) > 0 {
		return detachTailRefused(ctx, managed, cpierrors.Retriable("VM %d still names disk %s's volume on %s", sourceVMID, rd.diskCID, strings.Join(slots, ", ")))
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
	// The write below is pinned to the digest of this read. qemu-server's
	// vm_pending in PVE/API2/Qemu.pm loads the config with load_config. In
	// that load, parse_vm_config in PVE/QemuServer.pm puts the file's digest into the
	// config as a top-level key, and config_with_pending_array in
	// pve-guest-common's PVE/GuestHelpers.pm then lists that key like any other
	// plain key. A read without a digest gives us nothing to pin the write to,
	// so we refuse rather than send a write that PVE wouldn't check.
	if digest, ok := pve.ConfigString(config, "digest"); !ok || digest == "" {
		return detachTailRefused(ctx, managed, cpierrors.Retriable("%s: the read of VM %d carries no digest to pin the removal of disk %s's allocation entry to",
			op, sourceVMID, rd.diskCID))
	}
	// qemu-server's update_vm_api refuses a config write while the config
	// carries a lock, unless the caller skips the lock check, which we never
	// do. A backup or another task that holds the VM is ordinary, so we wait
	// for the next call rather than send a write PVE would refuse.
	if lock, ok := pve.ConfigString(config, "lock"); ok && lock != "" {
		return detachTailRefused(ctx, managed, cpierrors.Retriable("%s: VM %d is locked (%s), so the removal of disk %s's allocation entry waits for the next call",
			op, sourceVMID, lock, rd.diskCID))
	}
	attachedKeys := unheldNames(held, rd.stableID, entry.Volid)
	overlayKeys := unheldNames(held, rd.stableID, entry.Volid, rd.birth)
	return pve.RemoveDiskAllocationEntry(withDetachTailRemoval(ctx), deps.PVE, node, sourceVMID, key, entry, config, attachedKeys, overlayKeys)
}

// detachTailReadFailure decides what a failed read of the source VM means. We
// count it as absence only when it has the shape that parkerConfigGone in
// pve/parker.go reads as a VM whose config is gone, and a fresh lookup then
// agrees that the VM isn't there. Any other failure, including a 500 that says
// the config file does not exist for a VM the lookup still finds, comes back
// retriable, so the next call tries again.
//
// We don't trust a read of that shape on its own, because the read goes to one
// node. When a VM's config now sits under another node, such as after a
// migration we haven't seen yet, the old node answers with a 500 that says the
// config file does not exist. parkerConfigGone reads that answer, and a 404, as
// a missing VM. Taking that as absence would skip the removal and leave the
// entry for the deletion proof to refuse after the volume is gone. The
// cluster-wide lookup tells a VM that's gone from one that moved.
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

// detachTailNamesDisk reports whether entry names the disk that landed under
// the volid that landed holds, on the node the entry records. It does when it
// names the landed volid, or when one of record's observed moves took the disk
// off sourceVMID on that node under the volid the entry names and landed it
// under the volid that landed holds.
//
// We don't check step.Attempt, so a move step from any attempt counts. That
// fails safe, because a step matches only when its first volid is the one the
// entry names and its last volid is the landed one, whichever attempt wrote it.
func detachTailNamesDisk(record aj.Record, entry pve.DiskAllocationProvenance, landed string, sourceVMID int) bool {
	if entry.Volid == landed {
		return true
	}
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
		return true
	}
	return false
}

// finishParkedDetachTail runs the detach tail for a disk already parked, with
// the source VM the parker's landed entry names. It is how a record that an
// earlier release left with its source VM's entry in place heals.
func finishParkedDetachTail(ctx context.Context, deps Deps, op string, rd resolvedDisk) error {
	managed, ok := deps.PVE.(*managedDiskLifecycleClient)
	if !ok || rd.allocation == nil || rd.holder == nil || !rd.holder.IsParker {
		return nil
	}
	source, found, err := pve.ReadParkerSourceVMID(ctx, deps.PVE, rd.holder.Node, rd.holder.VMID, rd.volid, rd.stableID)
	if err != nil {
		return detachTailRefused(ctx, managed, retriableUnlessPermanent(err, fmt.Sprintf("%s: read the source VM from parker %d's entry for disk %s", op, rd.holder.VMID, rd.diskCID)))
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

// callerSentDescriptionDigest reports whether fields, the parameters a
// config write carried when its caller handed it to the lifecycle guard and
// before the guard stamped its own digest, are exactly a description and the
// caller's digest.
func callerSentDescriptionDigest(fields map[string]any) bool {
	if len(fields) != 2 {
		return false
	}
	_, description := fields[pveConfigKeyDescription]
	digest, ok := fields["digest"].(string)
	return description && ok && digest != ""
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
// for a stale digest, or refused outright with a 4xx status or a 500 that says
// the VM is locked. qemu-server's update_vm_api checks the digest under the
// VM's lock before it writes anything, so the digest refusal changed nothing,
// and the 4xx and locked-500 refusals are answers PVE gives instead of a write.
//
// A digest refusal of a write whose caller sent its own digest settles at
// once, whatever the description adds or removes. Such a caller built the
// description from a read carrying that digest, the guard confirmed before
// sending that its own read carried the same one, and PVE's refusal under the
// lock says the config changed after both reads, so the refusal itself proves
// nothing was written. The caller, such as a parker provenance write, reads
// again and retries, which it can only do while the guard stays usable.
//
// Any other settled refusal needs more proof. The step is observed with no
// volume once a fresh read shows the VM still carrying every note the write
// meant to remove, each as it was before the write. Those notes are an
// allocation entry, an attached-disk entry, and a drive-option overlay.
// Anything else, including a write that changes a disk key, a digest refusal
// of a write whose digest only the guard stamped, and a failure PVE may not
// have answered, stays with the guard's usual handling.
func (g *managedDiskLifecycleGuard) settleRefusedDescription(ctx context.Context, call ManagedAllocationMutation, step string, writeErr error) bool {
	observation, ok := g.observations[step]
	if !ok || ctx.Err() != nil || !descriptionOnlyConfigWrite(call) {
		return false
	}
	if pve.IsConfigDigestRefusal(writeErr) && callerSentDescriptionDigest(observation.fields) {
		if err := storageMutationObserved(g.lifecycle.handle, step, nil, false); err != nil {
			return false
		}
		delete(g.observations, step)
		return true
	}
	if !pve.IsConfigDigestRefusal(writeErr) && !pve.IsAnsweredConfigRefusal(writeErr) {
		return false
	}
	written, ok := observation.fields[pveConfigKeyDescription].(string)
	if !ok {
		return false
	}
	before := pve.DescriptionFromConfig(observation.before)
	removed, ok := pve.DescriptionNotesRemoved(before, written)
	if !ok {
		return false
	}
	cfg, err := g.lifecycle.deps.PVE.QEMU().Config(ctx, observation.node, observation.vmid)
	if err != nil || cfg == nil {
		return false
	}
	if !pve.DescriptionNotesKept(before, pve.DescriptionFromConfig(cfg), removed) {
		return false
	}
	if err := storageMutationObserved(g.lifecycle.handle, step, nil, false); err != nil {
		return false
	}
	delete(g.observations, step)
	return true
}
