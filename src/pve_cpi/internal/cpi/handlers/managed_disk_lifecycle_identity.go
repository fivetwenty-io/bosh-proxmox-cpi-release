package handlers

import (
	"context"
	"errors"
	"reflect"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

type managedDiskIdentity struct {
	terminalAbsent bool
	absent         bool
	record         aj.Record
	provenance     pve.DiskAllocationProvenance
	// shared reports whether the disk's storage is shared. Every move the
	// CPI makes keeps the volume on its storage, so the answer is the same
	// for each name the record's moves landed. Only the identity check's
	// claim-only resume reads it, to scope a later move that took a landing
	// away.
	shared bool
}

func resolveManagedDiskIdentity(ctx context.Context, deps Deps, op string, rd resolvedDisk) (resolvedDisk, error) {
	locator, id, managed := pve.ParseAllocationVolumeID(rd.birth)
	if !managed {
		if strings.Contains(rd.birth, "-bosh-") && strings.Contains(rd.birth, "-alloc-") {
			return resolvedDisk{}, cpierrors.Cloud("malformed managed disk allocation identity; audit required")
		}
		markerID, namespace, found, err := managedDiskMarkerIdentity(ctx, deps, rd)
		if err != nil {
			return resolvedDisk{}, err
		}
		if !found {
			return rd, nil
		}
		id = markerID
		locator = pve.AllocationNamespaceLocator(namespace)
	}
	if deps.Config == nil || deps.Config.StoragePlacementNamespace == "" || locator != pve.AllocationNamespaceLocator(deps.Config.StoragePlacementNamespace) {
		return resolvedDisk{}, cpierrors.Cloud("managed disk namespace differs from configured authority; audit required")
	}
	node := deps.Config.Node
	if rd.holder != nil {
		node = rd.holder.Node
	} else if rd.intent != nil {
		node = rd.intent.ParkerNode
	}
	journal, err := openStorageAllocationJournal(ctx, deps, []string{node})
	if err != nil {
		return resolvedDisk{}, cpierrors.Cloud("managed disk journal authority unavailable; audit required")
	}
	record, inspectErr := journal.Inspect(id)
	err = errors.Join(inspectErr, journal.Close())
	if err != nil {
		return resolvedDisk{}, cpierrors.Cloud("managed disk allocation record unavailable; audit required")
	}
	if record.Kind != "disk" || record.Namespace != deps.Config.StoragePlacementNamespace || rd.stableID != "" && record.DiskToken != rd.stableID {
		return resolvedDisk{}, cpierrors.Cloud("managed disk allocation identity conflicts; audit required")
	}
	if rd.stableID == "" {
		// A bare historical CID still carries the full allocation UUID. Recover
		// its immutable token from the journal before locating or moving it.
		metadata := pve.DiskCIDMeta{}
		if rd.meta != nil {
			metadata = *rd.meta
		}
		metadata.ID = record.DiskToken
		return resolveDiskForOp(ctx, deps, op, rd.diskCID, rd.birth, &metadata)
	}
	return resolveManagedDiskRecord(ctx, deps, op, rd, record, node)
}
func resolveManagedDiskRecord(ctx context.Context, deps Deps, op string, rd resolvedDisk, record aj.Record, node string) (resolvedDisk, error) {
	id := record.ID
	storage, _, err := pve.ParseDiskCID(rd.volid)
	if err != nil {
		return resolvedDisk{}, err
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		if rd.holder != nil || rd.intent != nil || len(rd.unused) > 0 {
			return resolvedDisk{}, cpierrors.Cloud("terminal managed disk still has ownership provenance; audit required")
		}
		exists, err := managedVolumePresent(ctx, deps, node, rd.volid)
		if err != nil || exists {
			return resolvedDisk{}, cpierrors.Cloud("terminal managed disk absence cannot be verified; audit required")
		}
		rd.allocation = &managedDiskIdentity{record: record, terminalAbsent: true}
		return rd, nil
	}
	definition, err := managedDiskActualDefinition(ctx, deps, storage)
	if err != nil {
		return resolvedDisk{}, err
	}
	backing := definition.BackingKey()
	if backing == "" {
		return resolvedDisk{}, cpierrors.Cloud("managed disk backing identity unavailable")
	}
	if err := validateManagedDiskJournalIdentity(record, rd.birth, rd.volid, backing); err != nil {
		return resolvedDisk{}, err
	}
	exists := false
	if rd.holder == nil && rd.intent == nil {
		node, exists, err = observeManagedFreeDisk(ctx, deps, record, rd.volid, backing, rd.meta, node)
	} else {
		exists, err = observeManagedDiskVolume(ctx, deps, node, rd.volid, rd.meta)
	}
	if err != nil {
		return resolvedDisk{}, err
	}

	provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: id, AllocationNamespace: record.Namespace, Node: node, Volid: rd.volid, Backing: backing}
	if !exists && rd.holder == nil && rd.intent != nil {
		return resumeBeforeManagedIdentity(ctx, deps, op, rd, record, provenance, definition.IsShared())
	}
	if !exists && (rd.holder != nil || rd.intent != nil) {
		return resolvedDisk{}, cpierrors.Cloud("managed disk ownership provenance references a missing volume; audit required")
	}
	if rd.holder != nil {
		cfg, err := deps.PVE.QEMU().Config(ctx, rd.holder.Node, rd.holder.VMID)
		if err != nil || cfg == nil {
			return resolvedDisk{}, cpierrors.Cloud("managed disk holder cannot be verified; audit required")
		}
		actual, found, err := pve.FindDiskAllocationProvenance(pve.DescriptionFromConfig(cfg), rd.sentinelKey())
		if err != nil {
			return resolvedDisk{}, err
		}
		if found {
			if actual.AllocationID != id || actual.AllocationNamespace != record.Namespace || actual.Volid != rd.volid || actual.Backing != "" && actual.Backing != backing {
				return resolvedDisk{}, cpierrors.Cloud("managed disk holder provenance conflicts; audit required")
			}
		} else if rd.volid != rd.birth {
			return resolvedDisk{}, cpierrors.Cloud("renamed managed disk lacks full ownership provenance; audit required")
		}
	} else if rd.intent != nil {
		if rd.intent.AllocationID != id || rd.intent.AllocationNamespace != record.Namespace || rd.intent.AllocationBacking != "" && rd.intent.AllocationBacking != backing {
			return resolvedDisk{}, cpierrors.Cloud("managed disk transfer provenance conflicts; audit required")
		}
	}
	rd.allocation = &managedDiskIdentity{record: record, provenance: provenance, absent: !exists}
	return rd, nil
}

// unsettledMoveRunbook points the unsettled-move refusal at its runbook entry.
const unsettledMoveRunbook = `see "A disk call refuses while a move to a parker is unsettled" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// identityResumeKey marks a context whose disk resolution runs inside a
// resume that resumeBeforeManagedIdentity started.
type identityResumeKey struct{}

// resumeBeforeManagedIdentity handles a managed disk whose transfer to a
// parker stopped after the move renamed its volume, so the volume its transfer
// record names is gone. Nothing the identity check reads can say where the
// disk landed until the transfer finishes, so we finish it first and return
// the disk resolved again from the settled state.
//
// A transfer record that doesn't match the journal's allocation is refused
// before anything is written. A resume that can't run comes back retriable,
// and an error the resume already marks permanent stays permanent. A disk
// that still names a missing volume after its own resume is refused for audit,
// as it was before the resume ran.
//
// A record with a step that isn't settled, on any attempt, gets that same
// refusal before the resume runs, and the refusal names the step. A step that
// a closed attempt's completion proof covers counts as settled. So does a
// record that needs reconciliation. An unsettled move can land the volume
// under a name that no step of the record names, and any other unsettled
// step leaves the disk's state unknown, so the check would refuse the
// claimed disk anyway, after the resume had already written to the parker.
//
// The resume runs with the client the caller passed in. The check can't open
// the disk's lifecycle here, because the lifecycle's ownership proof refuses a
// missing volume, and the check already runs under the allocation journal's
// lock when a lifecycle resolves the disk again. So the resume is claim-only.
// It writes the parker's serial and the finalize, which are safe to repeat,
// and it writes them only under the parker's lock, on a transfer record that
// still matches, and on a landing an observed move of the record proves.
//
// Where the resume would apply a pending delete or attach by config edit, no
// later call gets past this check to do that write, so the refusal is the
// permanent audit refusal with the reason added. The same goes for a window
// that would run without the parker's lock. PVE refuses that lock to a token
// that lacks Pool.Allocate on bosh-lock-*, and no retry grants that privilege.
//
// Where the resume would move the volume, the source VM's config still names
// the old name that storage just said is gone, so a retry would only meet the
// same refusal. In that case, and when the transfer moved underneath the
// resume or no window applied, the check resolves the disk once more. A disk
// whose old name is back, or that has settled, is returned, and the next call
// that changes the disk, such as attach_disk or detach_disk, finishes a
// transfer that's still open. A transfer that changed again comes back
// retriable. An unchanged one gets the resume's answer, which for the move is
// the permanent audit refusal with the move's reason. That refusal also says
// that storage doesn't list the old name, so the source VM's entry for it may
// be dangling.
//
// shared reports whether the disk's storage is shared, and the claim-only
// resume uses it to scope the moves that took a landing away.
func resumeBeforeManagedIdentity(ctx context.Context, deps Deps, op string, rd resolvedDisk, record aj.Record, provenance pve.DiskAllocationProvenance, shared bool) (resolvedDisk, error) {
	if rd.intent.AllocationID != record.ID || rd.intent.AllocationNamespace != record.Namespace || rd.intent.AllocationBacking != "" && rd.intent.AllocationBacking != provenance.Backing {
		return resolvedDisk{}, cpierrors.Cloud("managed disk transfer provenance conflicts; audit required")
	}
	if ctx.Value(identityResumeKey{}) != nil {
		return resolvedDisk{}, cpierrors.Cloud(missingVolumeAudit)
	}
	if recheck, ok := ctx.Value(identityRecheckKey{}).(identityRecheck); ok {
		if sameTransfer(recheck.intent, *rd.intent) && reflect.DeepEqual(recheck.record, record) {
			return resolvedDisk{}, recheck.unchanged
		}
		return resolvedDisk{}, transferMovedUnderneath(rd)
	}
	if step, ok := unsettledRecordStep(record); ok {
		if strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
			return resolvedDisk{}, cpierrors.Cloud("%s, because move step %s of the disk's record isn't settled; %s", missingVolumeAudit, step.ID, unsettledMoveRunbook)
		}
		return resolvedDisk{}, cpierrors.Cloud("%s, because %s in the disk's record", missingVolumeAudit, unsettledStepName(step))
	}
	if record.State == aj.ReconciliationRequired {
		return resolvedDisk{}, cpierrors.Cloud("%s, because the disk's record needs reconciliation", missingVolumeAudit)
	}
	// The resume takes the allocation's identity from rd, so the landing
	// writes the journal's backing even when the transfer record left it out.
	rd.allocation = &managedDiskIdentity{record: record, provenance: provenance, absent: true, shared: shared}
	resumed, err := resumeTransfer(context.WithValue(ctx, identityResumeKey{}, true), deps, op, rd, true)
	if err == nil {
		return resumed, nil
	}
	refusal, ok := pve.AsClaimOnlyRefusal(err)
	if !ok {
		return resolvedDisk{}, err
	}
	switch refusal.Window {
	case pve.ClaimOnlyPendingDelete, pve.ClaimOnlyConfigEdit, pve.ClaimOnlyUnprovenLanding, pve.ClaimOnlyUnserialized:
		return resolvedDisk{}, cpierrors.Cloud("%s, because %s", missingVolumeAudit, refusal.Reason)
	case pve.ClaimOnlyMove, pve.ClaimOnlyIntentMoved, pve.ClaimOnlyNoWindow:
		unchanged := err
		switch refusal.Window {
		case pve.ClaimOnlyMove:
			unchanged = cpierrors.Cloud("%s, because %s, and storage doesn't list %s, so source vm %s's entry for it may be dangling",
				missingVolumeAudit, refusal.Reason, rd.intent.Volid, rd.intent.SourceVMCID)
		case pve.ClaimOnlyIntentMoved:
			unchanged = transferMovedUnderneath(rd)
		}
		recheck := identityRecheck{intent: *rd.intent, record: record, unchanged: unchanged}
		return resolveDiskForOp(context.WithValue(ctx, identityRecheckKey{}, recheck), deps, op, rd.diskCID, rd.birth, rd.meta)
	default:
		return resolvedDisk{}, err
	}
}

// missingVolumeAudit is the identity check's refusal for a disk whose
// ownership provenance names a volume that's gone.
const missingVolumeAudit = "managed disk ownership provenance references a missing volume; audit required"

// identityRecheckKey marks a context whose disk resolution runs again after
// the identity check's claim-only resume stopped at the move, found the
// transfer moved underneath it, or found no window to run. Its value is an
// identityRecheck.
type identityRecheckKey struct{}

// identityRecheck is what the identity check's second resolution compares.
// When the transfer record and the journal record both read as they did
// before the resume, the check returns unchanged instead of resuming again.
type identityRecheck struct {
	intent    pve.DiskTransferIntent
	record    aj.Record
	unchanged error
}

// sameTransfer reports whether two reads of a transfer record name the same
// parker, slot, volume, and source VM.
func sameTransfer(a, b pve.DiskTransferIntent) bool {
	return a.ParkerVMID == b.ParkerVMID && a.ParkerNode == b.ParkerNode && a.Slot == b.Slot && a.Volid == b.Volid && a.SourceVMCID == b.SourceVMCID
}

// transferMovedUnderneath is the retriable refusal for a transfer that
// another call changed while the identity check was resuming it.
func transferMovedUnderneath(rd resolvedDisk) error {
	return cpierrors.Retriable("managed disk %s: its transfer to parker vmid %d moved underneath the identity check; retry",
		rd.diskCID, rd.intent.ParkerVMID)
}

// unsettledRecordStep returns the first step of record, on any attempt, that
// isn't observed and that no closed attempt's completion proof covers.
func unsettledRecordStep(record aj.Record) (aj.Step, bool) {
	for i := range record.Steps {
		step := record.Steps[i]
		if step.State != aj.Observed && !storageDecisionClosedAttemptStepSettled(record, step) {
			return step, true
		}
	}
	return aj.Step{}, false
}

// recordedLandings lists the moves record observed, each from the volid the
// move started with to the volid it landed under, off the VM it moved from.
// A claim-only resume claims a parker slot only when one of them landed the
// slot's volume.
//
// A landing that a later observed move or migrate took away again is left
// out, because the disk no longer holds that name, and another volume may
// hold it now.
//
// shared reports whether the disk's storage is shared, which decides where
// that later move has to run for it to count.
func recordedLandings(record aj.Record, shared bool) []pve.RecordedLanding {
	var landings []pve.RecordedLanding
	for i := range record.Steps {
		step := &record.Steps[i]
		if !observedMoveStep(step) || len(step.VolIDs) < 2 {
			continue
		}
		landed := step.VolIDs[len(step.VolIDs)-1]
		if landingMovedAway(record.Steps[i+1:], landed, step.Target.Node, shared) {
			continue
		}
		landings = append(landings, pve.RecordedLanding{
			SourceVMID: step.Target.VMID,
			From:       step.VolIDs[0],
			To:         landed,
		})
	}
	return landings
}

// observedMoveStep reports whether step is a move that the record observed.
func observedMoveStep(step *aj.Step) bool {
	return step.State == aj.Observed && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk")
}

// observedMigrateStep reports whether step is a migrate that the record
// observed.
func observedMigrateStep(step *aj.Step) bool {
	return step.State == aj.Observed && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMigrate")
}

// landingMovedAway reports whether any observed move in later started from
// landed, the name an earlier move gave the volume. The steps in later are
// the ones the record observed after that earlier move, which ran on node.
//
// A move counts only when PVE landed the volume under another name, so it
// has to hold at least two volids, and its last one has to differ from
// landed. A move that PVE refused holds only the name it started from, and
// it leaves the volume where it was. A volid names one volume on a node, or
// across the cluster on shared storage, so a later move counts when it ran
// on node, or on any node when the storage is shared. The VM that move ran
// on doesn't matter, because attach_disk can attach a volume whose name
// carries one VM's number to another VM by a config edit.
//
// A migrate counts as well when the record observed it starting from landed,
// on whatever node it ran and under whatever name it landed. The VM that
// held the landing left its node then, so the landing is no longer there to
// claim. As with a move, a migrate step that holds only the name it started
// from records no landing, and it doesn't count.
func landingMovedAway(later []aj.Step, landed, node string, shared bool) bool {
	for i := range later {
		step := &later[i]
		if observedMigrateStep(step) && len(step.VolIDs) >= 2 && step.VolIDs[0] == landed {
			return true
		}
		if !observedMoveStep(step) || len(step.VolIDs) < 2 || step.VolIDs[0] != landed || step.VolIDs[len(step.VolIDs)-1] == landed {
			continue
		}
		if shared || step.Target.Node == node {
			return true
		}
	}
	return false
}

func managedDiskActualBacking(ctx context.Context, deps Deps, storage string) (string, error) {
	info, err := managedDiskActualDefinition(ctx, deps, storage)
	if err != nil {
		return "", err
	}
	backing := info.BackingKey()
	if backing == "" {
		return "", cpierrors.Cloud("managed disk backing identity unavailable")
	}
	return backing, nil
}
func managedDiskActualDefinition(ctx context.Context, deps Deps, storage string) (pve.StorageInfo, error) {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition unavailable")
	}
	all, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	if err != nil || all == nil || *all == nil {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition unavailable")
	}
	var found pve.StorageInfo
	for _, raw := range *all {
		info, err := pve.ParseStorageEntry(raw)
		if err != nil {
			return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition malformed")
		}
		if info.Name == storage {
			if found.Name != "" {
				return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition ambiguous")
			}
			found = info
		}
	}
	if found.Name == "" {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk actual storage is unavailable")
	}
	return found, nil
}
func managedDiskParkContext(rd resolvedDisk, pctx pve.ParkContext) pve.ParkContext {
	if rd.allocation != nil {
		pctx.AllocationID = rd.allocation.record.ID
		pctx.AllocationNamespace = rd.allocation.record.Namespace
		pctx.AllocationBacking = rd.allocation.provenance.Backing
	}
	return pctx
}
func writeManagedDiskHolder(ctx context.Context, deps Deps, rd resolvedDisk, node string, vmid int, volid string) error {
	if rd.allocation == nil {
		return nil
	}
	entry := rd.allocation.provenance
	entry.Node = node
	entry.Volid = volid
	if err := pve.WriteDiskAllocationProvenance(ctx, deps.PVE, node, vmid, rd.sentinelKey(), entry); err != nil {
		return cpierrors.Cloud("managed disk holder provenance was not verified; audit required")
	}
	return nil
}

// healMovedDiskProvenance runs before a detach transfers a managed disk to a
// parker. A holder that was moved between nodes outside BOSH still carries a
// provenance entry naming its old node, while rd carries the entry rebuilt on
// the node the holder runs on now, and the removal after the transfer
// compares against the rebuilt entry exactly. When the two differ in Node
// alone and a fresh audit accepts the disk's move on shared storage, the
// entry is rewritten on the holder's current node and the transfer proceeds.
// Without that move the detach refuses here, while the disk is still
// attached. Any other difference is left to the removal's own check.
func healMovedDiskProvenance(ctx context.Context, deps Deps, node string, vmid int, rd resolvedDisk) error {
	if rd.allocation == nil {
		return nil
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return cpierrors.Cloud("managed disk holder provenance cannot be read before transfer: VM %d on %s: %s; audit required", vmid, node, pve.DescribeAuditError(err))
	}
	if cfg == nil {
		return cpierrors.Cloud("managed disk holder provenance cannot be read before transfer: VM %d on %s returned no configuration; audit required", vmid, node)
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(cfg))
	if err != nil {
		return cpierrors.Cloud("managed disk holder provenance is malformed; audit required")
	}
	stored, found := entries[rd.sentinelKey()]
	current := rd.allocation.provenance
	relocated := stored
	relocated.Node = current.Node
	if !found || stored == current || relocated != current {
		return nil
	}
	// The audit only reads, so it runs beneath the lifecycle decorators the
	// way the parker sweep does; the rewrite below stays journaled.
	reader := deps
	reader.PVE = unguardedPVE(deps.PVE)
	journal, err := openStorageAllocationJournal(ctx, reader, []string{node})
	if err != nil {
		return err
	}
	audit, err := AuditStorageAllocations(ctx, reader, journal, []string{node})
	if closeErr := journal.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if !audit.observedMove(allocationKindDisk, current.AllocationID, vmid, node) {
		return cpierrors.Cloud("detach refused before transfer: disk allocation %s (volume %s) held by VM %d on %s, provenance names %s, and the allocation audit accepted no move on shared storage (%s); the disk stays attached", current.AllocationID, current.Volid, vmid, node, storageAuditField(stored.Node), storageAuditMoveRefusal(audit, current.AllocationID))
	}
	if err := pve.WriteDiskAllocationProvenance(ctx, deps.PVE, node, vmid, rd.sentinelKey(), current); err != nil {
		return cpierrors.Cloud("managed disk holder provenance could not be moved to %s before transfer; audit required", node)
	}
	deps.Log(ctx).Info("detach_disk: moved disk provenance to the holder's current node",
		log.String("allocation_id", current.AllocationID),
		log.String("volid", current.Volid),
		log.Int("vmid", vmid),
		log.String("recorded_node", storageAuditField(stored.Node)),
		log.String("node", node),
	)
	return nil
}

func verifyManagedDiskParked(ctx context.Context, deps Deps, rd resolvedDisk, volid string) error {
	if rd.allocation == nil {
		return nil
	}
	if err := pve.VerifyAllocationParked(ctx, deps.PVE, deps.Log(ctx), volid, rd.stableID, rd.allocation.record.Namespace, rd.allocation.record.ID, parkerReadConfigFor(deps)); err != nil {
		return cpierrors.Cloud("managed disk parker provenance requires reconciliation")
	}
	return nil
}

// A volume and its backing must be associated by the same durable step. Separate
// historical steps cannot be combined to invent ownership after a transfer.
func validateManagedDiskJournalIdentity(record aj.Record, birth, actual, backing string) error {
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return cpierrors.Cloud("terminal managed disk allocation still has a live resource; audit required")
	}
	birthRecorded, actualRecorded := false, false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		volumes := append([]string{step.Target.IntendedVolume}, step.VolIDs...)
		for _, volume := range volumes {
			birthRecorded = birthRecorded || volume == birth
			actualRecorded = actualRecorded || volume == actual && step.Target.Backing == backing
		}
	}
	if !birthRecorded || !actualRecorded {
		return cpierrors.Cloud("managed disk physical backing or volume conflicts with journal; audit required")
	}
	return nil
}

// observeManagedDiskVolume reads the exact storage-content endpoint. A filename,
// journal record, or dangling VM config entry alone never proves live ownership.
func observeManagedDiskVolume(ctx context.Context, deps Deps, node, volume string, meta *pve.DiskCIDMeta) (bool, error) {
	present, err := managedVolumePresent(ctx, deps, node, volume)
	if err != nil || !present {
		return false, err
	}
	storage, bare, err := pve.ParseDiskCID(volume)
	if err != nil {
		return false, err
	}
	info, err := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bare)
	if err != nil {
		if pve.IsNotFound(err) {
			return false, nil
		}
		return false, cpierrors.Cloud("managed disk exact volume observation failed")
	}
	if info == nil || info.Size <= 0 || info.Format == "" {
		return false, cpierrors.Cloud("managed disk exact volume observation malformed")
	}
	if meta != nil && meta.Format != "" && info.Format != meta.Format {
		return false, cpierrors.Cloud("managed disk format differs from recorded identity")
	}
	return true, nil
}

// Free disks on nonshared backings must be checked at their recorded physical
// nodes. The configured default node cannot certify absence on another node.
func observeManagedFreeDisk(ctx context.Context, deps Deps, record aj.Record, volume, backing string, meta *pve.DiskCIDMeta, fallback string) (string, bool, error) {
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return "", false, err
	}
	definition, err := managedDiskActualDefinition(ctx, deps, storage)
	if err != nil {
		return "", false, err
	}
	candidates := []string{}
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.Backing != backing || step.Target.Node == "" {
			continue
		}
		matches := step.Target.IntendedVolume == volume
		for _, id := range step.VolIDs {
			matches = matches || id == volume
		}
		if matches {
			seen := false
			for _, node := range candidates {
				seen = seen || node == step.Target.Node
			}
			if !seen {
				candidates = append(candidates, step.Target.Node)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = append(candidates, fallback)
	}
	if definition.IsShared() {
		candidates = candidates[len(candidates)-1:]
	}
	chosen := ""
	for _, node := range candidates {
		continuity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), []string{node})
		if err != nil || continuity.ID() != record.ClusterID {
			return "", false, cpierrors.Cloud("managed free disk cluster continuity differs")
		}
		exists, err := observeManagedDiskVolume(ctx, deps, node, volume, meta)
		if err != nil {
			return "", false, err
		}
		if exists {
			if chosen != "" {
				return "", false, cpierrors.Cloud("managed local disk has multiple retained copies; audit required")
			}
			chosen = node
		}
	}
	if chosen != "" {
		return chosen, true, nil
	}
	return candidates[len(candidates)-1], false, nil
}

// A known holder or transfer marker can activate managed ownership even when a
// caller supplies an older plain volume CID. Unmarked legacy disks incur no
// journal access, and absent holder information does not invent ownership.
func managedDiskMarkerIdentity(ctx context.Context, deps Deps, rd resolvedDisk) (string, string, bool, error) {
	if rd.intent != nil && (rd.intent.AllocationID != "" || rd.intent.AllocationNamespace != "") {
		if rd.intent.AllocationID == "" || rd.intent.AllocationNamespace == "" {
			return "", "", false, cpierrors.Cloud("incomplete managed transfer identity")
		}
		return rd.intent.AllocationID, rd.intent.AllocationNamespace, true, nil
	}
	if rd.holder == nil {
		return "", "", false, nil
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, rd.holder.Node, rd.holder.VMID)
	if err != nil || cfg == nil {
		return "", "", false, cpierrors.Cloud("disk holder provenance unavailable")
	}
	description := pve.DescriptionFromConfig(cfg)
	if !strings.Contains(description, "bosh_disk_allocations") && !strings.Contains(description, "allocation_id") {
		return "", "", false, nil
	}
	entry, found, err := pve.FindDiskAllocationProvenance(description, rd.sentinelKey())
	if err != nil {
		return "", "", false, err
	}
	if !found {
		return "", "", false, nil
	}
	return entry.AllocationID, entry.AllocationNamespace, true, nil
}
