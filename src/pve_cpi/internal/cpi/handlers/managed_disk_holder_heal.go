package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// renamedHolderAudit is the identity check's refusal for a renamed managed
// disk whose holder carries no provenance entry for it, when the check can't
// prove the disk is the holder's and write the entry itself.
const renamedHolderAudit = "renamed managed disk lacks full ownership provenance; audit required"

// holderHealKey marks a context whose disk resolution does something other
// than refuse a renamed managed disk whose holder carries no provenance entry
// for it. Its value is a holderHeal.
type holderHealKey struct{}

// holderHeal says what a disk resolution does with a renamed managed disk
// whose holder carries no provenance entry for it.
type holderHeal int

const (
	// holderHealRefuse returns holderNotRecordedRefusal and writes nothing.
	// Every resolution does this unless its context asks for something else,
	// so has_disk, create_vm's placement planning, and the lifecycle guard's
	// check before a delete never write to the holder.
	holderHealRefuse holderHeal = iota
	// holderHealDefer returns the disk resolved without the entry. Only the
	// first resolution of a call that opens the disk's lifecycle next asks
	// for it, because the lifecycle resolves the disk again under the
	// allocation journal's lock and writes the entry there.
	holderHealDefer
	// holderHealWrite runs healUnrecordedHolder, which writes the entry when
	// it can prove the disk is the holder's. Only acquireManagedDiskLifecycle
	// asks for it, for its resolution under the allocation journal's lock.
	holderHealWrite
)

// withHolderHeal returns ctx marked so that a disk resolution made with it
// does what heal says with a holder that lacks the disk's entry.
func withHolderHeal(ctx context.Context, heal holderHeal) context.Context {
	return context.WithValue(ctx, holderHealKey{}, heal)
}

// holderHealFor returns what a disk resolution made with ctx does with a
// holder that lacks the disk's entry. A context that asks for nothing refuses.
func holderHealFor(ctx context.Context) holderHeal {
	if heal, ok := ctx.Value(holderHealKey{}).(holderHeal); ok {
		return heal
	}
	return holderHealRefuse
}

// holderNotRecorded is the identity check's refusal for a renamed managed
// disk whose holder carries no provenance entry for it, when the check
// doesn't write the entry. Its text and CPI type are the retriable error it
// carries.
type holderNotRecorded struct{ err error }

func (e *holderNotRecorded) Error() string { return e.err.Error() }
func (e *holderNotRecorded) Unwrap() error { return e.err }

// isHolderNotRecorded reports whether err is that refusal.
func isHolderNotRecorded(err error) bool {
	var refused *holderNotRecorded
	return errors.As(err, &refused)
}

// holderNotRecordedRefusal builds that refusal for rd, with reason added after
// the missing entry when the heal itself refused to write.
func holderNotRecordedRefusal(rd resolvedDisk, reason string) error {
	return &holderNotRecorded{err: cpierrors.Retriable("managed disk %s is attached to VM %d as %s without its provenance entry%s; retry the operation, and the next call that changes the disk, such as attach_disk or detach_disk, checks the disk again under its allocation lock and writes the entry",
		rd.diskCID, rd.holder.VMID, rd.volid, reason)}
}

// healUnrecordedHolder handles a managed disk whose volume was renamed onto
// its holder while the holder's provenance entry for it was never written.
// That happens when a call moves the disk and then stops before the entry's
// write is sent, because a read before the write failed or the request ended.
// Every later call would refuse the disk for audit, although the journal
// already proves it.
//
// The caller has checked the journal: the record names the volume on the
// disk's backing, under the disk's stable token. healUnrecordedHolder then
// proves the rest from cfg, the holder configuration the caller just read,
// and from the cluster:
//
//   - the holder is a workload VM and not a parker;
//   - a drive slot in cfg names the volume and carries the disk's stable ID as
//     its serial;
//   - the volume's name carries the holder's VMID, the name PVE gives a volume
//     it moves onto that VM;
//   - no other VM's notes claim the disk's key, except the parker the disk
//     came from, whose leftover parked entry names the volume a move in the
//     record landed onto the holder.
//
// When every proof holds, it writes provenance onto the holder, pinned to
// cfg's digest so PVE refuses the write if the holder changed after the
// checks, and reads it back. Only then does the disk count as resolved, so a
// missing entry is never accepted without being written in the same call.
//
// A proof that fails keeps the audit refusal, with the reason added. A read
// that fails, a write PVE refused because the holder changed, and a write
// whose answer never came back are retriable, because the next call makes the
// same checks again. The write is safe to repeat, because it writes the same
// entry and its readback proves the result.
//
// Only the resolution inside acquireManagedDiskLifecycle runs the heal (see
// holderHealWrite), so the write is made under the allocation journal's lock,
// with the client the lifecycle resolves with. Even there it writes nothing
// while a step of the record isn't settled or a transfer of the disk to a
// parker is in flight, because either one leaves the disk's name open to
// change. It returns holderNotRecordedRefusal instead, which is retriable.
// Before acquireManagedDiskLifecycle takes that answer, it settles by readback
// the steps a readback can settle and resolves the disk once more, so only a
// step that stays unsettled holds the heal back.
// The parker's leftover entry is left for the parker sweeps, as it was before
// the entry went missing.
func healUnrecordedHolder(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, shared bool, provenance pve.DiskAllocationProvenance, cfg map[string]any) error {
	if rd.holder == nil || rd.stableID == "" || cfg == nil {
		return cpierrors.Cloud("%s, because the disk has no holder with a stable identity to check", renamedHolderAudit)
	}
	if rd.intent != nil {
		return holderNotRecordedRefusal(rd, fmt.Sprintf(", and a transfer of the disk to parker %d is still in flight", rd.intent.ParkerVMID))
	}
	if step, ok := unsettledRecordStep(record); ok {
		return holderNotRecordedRefusal(rd, ", and "+unsettledStepName(step)+" in the disk's record")
	}
	holder := *rd.holder
	if holder.IsParker || pve.TagsMarkParker(holder.Tags) {
		return cpierrors.Cloud("%s, because its holder VM %d is a parker", renamedHolderAudit, holder.VMID)
	}
	if !holderSlotCarriesDisk(cfg, rd.volid, rd.stableID) {
		return cpierrors.Cloud("%s, because no drive slot on VM %d names %s with the disk's serial", renamedHolderAudit, holder.VMID, rd.volid)
	}
	if embedded, ok := pve.EmbeddedDiskVMID(rd.volid); !ok || embedded != holder.VMID {
		return cpierrors.Cloud("%s, because volume %s is not named for its holder VM %d", renamedHolderAudit, rd.volid, holder.VMID)
	}
	if err := otherHolderProvenanceClaim(ctx, deps, rd, record, shared); err != nil {
		return err
	}
	key := rd.sentinelKey()
	if err := pve.WriteDiskAllocationProvenanceOnto(ctx, deps.PVE, holder.Node, holder.VMID, key, provenance, cfg); err != nil {
		if errors.Is(err, pve.ErrProvenancePersistFailed) || errors.Is(err, pve.ErrProvenanceReadbackUnread) || ctx.Err() != nil {
			return cpierrors.Retriable("managed disk %s is attached to VM %d as %s without its provenance entry, and writing the entry failed (%s); retry the operation, which checks the disk again and writes the entry",
				rd.diskCID, holder.VMID, rd.volid, err.Error())
		}
		return cpierrors.Cloud("%s, because writing the entry onto VM %d failed: %s", renamedHolderAudit, holder.VMID, err.Error())
	}
	deps.Log(ctx).Warn("managed disk identity: wrote the provenance entry its holder was missing",
		log.String("disk_cid", rd.diskCID),
		log.String("allocation_id", provenance.AllocationID),
		log.String("volid", rd.volid),
		log.Int("vmid", holder.VMID),
		log.String("node", holder.Node),
	)
	return nil
}

// holderSlotCarriesDisk reports whether a drive slot in cfg names volid and
// carries stableID as its serial.
func holderSlotCarriesDisk(cfg map[string]any, volid, stableID string) bool {
	for _, value := range qemu.ParseDisks(cfg) {
		bare := value
		if comma := strings.IndexByte(value, ','); comma >= 0 {
			bare = value[:comma]
		}
		if bare != volid {
			continue
		}
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == stableID {
			return true
		}
	}
	return false
}

// otherHolderProvenanceClaim returns nil when no VM in the cluster other than
// rd's holder carries provenance for rd's key, apart from the leftover parked
// entry that leftoverParkedEntry accepts. A VM that carries one gets the audit
// refusal, and so do notes that name the key but don't parse, because they
// might claim it. A listing or a configuration read that fails is retriable.
func otherHolderProvenanceClaim(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, shared bool) error {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, deps.Log(ctx))
	if err != nil {
		return cpierrors.Retriable("managed disk %s lacks its provenance entry on VM %d, and the cluster's VMs could not be listed to prove no other VM claims it (%s); retry the operation",
			rd.diskCID, rd.holder.VMID, err.Error())
	}
	key := rd.sentinelKey()
	for _, guest := range guests {
		if guest.VMID == rd.holder.VMID {
			continue
		}
		cfg, err := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
		if err != nil || cfg == nil {
			reason := "returned no configuration"
			if err != nil {
				reason = err.Error()
			}
			return cpierrors.Retriable("managed disk %s lacks its provenance entry on VM %d, and VM %d on %s could not be read to prove it doesn't claim the disk (%s); retry the operation",
				rd.diskCID, rd.holder.VMID, guest.VMID, guest.Node, reason)
		}
		description := pve.DescriptionFromConfig(cfg)
		if !strings.Contains(description, key) {
			continue
		}
		entry, found, err := pve.FindDiskAllocationProvenance(description, key)
		if err != nil {
			return cpierrors.Cloud("%s, because VM %d names the disk in notes that don't parse", renamedHolderAudit, guest.VMID)
		}
		if !found {
			continue
		}
		if leftoverParkedEntry(deps, guest, description, key, entry, rd, record, shared) {
			continue
		}
		return identityConflict(cpierrors.Cloud("%s, because VM %d also carries provenance for the disk", renamedHolderAudit, guest.VMID))
	}
	return nil
}

// leftoverParkedEntry reports whether entry, which guest's notes carry for
// key, is what a transfer off a parker leaves behind when it stops before
// removing it. guest has to be a parker in the configured band. The entry has
// to come from the parker's parked-disk record alone, name the disk's
// allocation, and name the volume that a move the record observed took off
// that parker and landed as rd's volume.
func leftoverParkedEntry(deps Deps, guest pve.GuestRef, description, key string, entry pve.DiskAllocationProvenance, rd resolvedDisk, record aj.Record, shared bool) bool {
	if deps.Config == nil || !pve.IsParkerVM(guest.VMID, guest.Tags, parkerReadConfigFor(deps)) {
		return false
	}
	current, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return false
	}
	if _, inAllocations := current[key]; inAllocations {
		return false
	}
	if entry.AllocationID != record.ID || entry.AllocationNamespace != record.Namespace {
		return false
	}
	for _, landing := range recordedLandings(record, shared) {
		if landing.SourceVMID == guest.VMID && landing.From == entry.Volid && landing.To == rd.volid {
			return true
		}
	}
	return false
}
