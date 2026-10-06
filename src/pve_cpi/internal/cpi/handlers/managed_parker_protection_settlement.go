package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A parker's protection flag is cleared for the length of a protection window
// and put back at its end. The lifecycle guard records a protection-only write
// with parameters that say so, and it finishes a failed one without locking
// itself (classifyProtectionWriteFailure): a write PVE refused is observed,
// because it did not apply, and a write cut off before PVE answered stays
// planned, because nobody knows whether it applied. The operation then goes on
// to record where the disk is, and the lifecycle leaves the allocation
// reconciliation_required with that one step planned. Every later
// readmission, adopt included, runs the settler below before it judges the
// record.
//
// The flag never moves, renames, or deletes a volume, and it never changes
// which VM holds one, so no outcome of that write can change anything the
// allocation owns. What matters is only whether the parker is protected now.
// That is why such a step can be settled after the fact by reading the parker
// back, where every other planned configuration write needs the full
// reconciliation it has always needed.
//
// The rule is deliberately narrow.
//
//   - The step is a planned lifecycle Nodes.UpdateQemuConfig step of the active
//     attempt, and its recorded parameters say it wrote the protection flag and
//     nothing else. The lifecycle guard records those parameters only for a
//     write whose fields are exactly protection, with or without digest, so a
//     step with any other field, or with no recorded parameters at all, is
//     never settled here.
//   - The step's target is a parker the record names: another step of the
//     record placed or moved the disk on that VM.
//   - The VM at that VMID reads protected, on its recorded node or on another
//     node the cluster now places it on. A VM that reads protected leaves
//     nothing unprotected whoever it is, so it settles even when it doesn't
//     carry the bosh-parker tag, and even when it is a VM created later at the
//     same VMID. When protection is off, the step stays planned and the
//     refusal names the qm set command for the node that holds the VM now,
//     or, for a VM without the tag, the checks we make first.
//   - Or the cluster proves the VMID gone, and the disk is accounted for
//     without the parker: it is on another VM, or its volume reads absent and
//     the record observed a delete_disk write that reached it. A parker that
//     vanished while the record still placed the disk on it may have taken
//     the disk with it, so every other answer keeps the step planned.
//
// A recorded node that fails the read without saying the VM is missing, such
// as a node that is down, proves nothing about the parker, so the settler
// searches the cluster then too, and it follows only a VM the cluster places
// on another node. When the cluster can't be searched, places the VMID on a
// node whose config can't be read, or places it nowhere while the recorded
// node doesn't answer, the step stays planned too. A step settled on anything
// but a tagged, protected parker on its recorded node logs a warning that says
// what the CPI read instead, and the journal records it exactly as a plain
// readback, so no release that reads the record sees anything new.
//
// The settler only reads. It never writes to PVE, and it runs only while the
// caller holds the record's journal lock, so the request that planned the step
// has finished or died.

// The parameter kinds a protection-only configuration write records. The kind
// carries the value written instead of a "protection" field, because the
// journal's mutation-parameter allowlist would reject a new field, and a
// record carrying one could not be read by an earlier release, which would
// break rolling back to 0.8.0. Version and kind are on every release's list.
const (
	parkerProtectionOnKind  = "parker_protection_on"
	parkerProtectionOffKind = "parker_protection_off"
)

// The configuration and parameter field names the protection rule reads.
const (
	pveConfigKeyProtection = "protection"
	pveConfigKeyDigest     = "digest"
)

// isParkerProtectionParameters is the one definition of a protection-only
// write. It reads recorded step parameters and holds when they are exactly
// version 1 and a parker protection kind, with digest allowed beside them. The
// lifecycle guard's admission, the guard's finish, and the settler all decide
// through it, and nothing else may define a second rule.
func isParkerProtectionParameters(raw json.RawMessage) bool {
	var fields map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for key := range fields {
		if key != "version" && key != "kind" && key != pveConfigKeyDigest {
			return false
		}
	}
	kind, _ := fields["kind"].(string)
	return fields["version"] == float64(1) && (kind == parkerProtectionOnKind || kind == parkerProtectionOffKind)
}

// parkerProtectionStepParameters renders the parameters a configuration write
// with the given fields would record when it is protection-only: fields that
// are exactly protection, with or without digest. For every other write it
// returns nil, and the step records no parameters, as before. Callers decide
// with isParkerProtectionParameters on the result.
func parkerProtectionStepParameters(fields map[string]any) json.RawMessage {
	value, ok := fields[pveConfigKeyProtection].(bool)
	if !ok {
		return nil
	}
	for key := range fields {
		if key != pveConfigKeyProtection && key != pveConfigKeyDigest {
			return nil
		}
	}
	kind := parkerProtectionOffKind
	if value {
		kind = parkerProtectionOnKind
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "kind": kind})
	if err != nil {
		return nil
	}
	return raw
}

// lifecycleStepParameters returns the parameters a lifecycle step records for
// a guarded mutation: protection-only parameters for a configuration write
// the predicate accepts, and nil for every other mutation.
func lifecycleStepParameters(key string, fields map[string]any) json.RawMessage {
	if key != "Nodes.UpdateQemuConfig" {
		return nil
	}
	if candidate := parkerProtectionStepParameters(fields); isParkerProtectionParameters(candidate) {
		return candidate
	}
	return nil
}

// IsParkerProtectionStep reports whether step is a planned protection-only
// configuration write of the active attempt, read from its recorded
// parameters. It is exported so the audit summary in cmd/cpi decides through
// the same rule the settler does.
func IsParkerProtectionStep(record aj.Record, step aj.Step) bool {
	if step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.Target.VMID <= 0 {
		return false
	}
	if !strings.HasPrefix(step.Kind, "lifecycle_") || !strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig") {
		return false
	}
	return isParkerProtectionParameters(step.Parameters)
}

// protectionWriteFailure is how the allocation guard finishes a failed write
// when its hooks opt in with SettleProtectionWrites.
type protectionWriteFailure int

const (
	// protectionWriteOther is every failure the guard handles as it always
	// has: it locks the guard and records the allocation as uncertain.
	protectionWriteOther protectionWriteFailure = iota
	// protectionWriteRefused is a protection-only write PVE answered with a
	// failure. The write did not apply, so its outcome is known: the step is
	// observed and the guard stays usable.
	protectionWriteRefused
	// protectionWriteCutOff is a protection-only write that failed without an
	// answer from PVE: its context ended, the request was cancelled, or the
	// transport failed. Nobody knows whether it applied, so the step stays
	// planned for the settler, and the guard stays usable so the rest of the
	// operation can record where the disk is. What makes this safe is the
	// predicate, not the kind of failure: a protection-only write can never
	// change what the allocation owns, so locking the guard on an
	// unknown-outcome transport error would only reopen the same gap.
	protectionWriteCutOff
)

// managedProtectionWriteRefusal is the After result for a protection-only
// write PVE refused, the proof that nothing changed.
type managedProtectionWriteRefusal struct{}

// classifyProtectionWriteFailure decides which of the three a failed guarded
// write is. Only a Nodes.UpdateQemuConfig whose fields render protection-only
// parameters is ever refused or cut off; every other write, on every kind of
// failure, is protectionWriteOther. A protection-only write is refused when
// PVE answered it (pve.ProtectionWriteRefused) and cut off otherwise.
func classifyProtectionWriteFailure(ctx context.Context, m ManagedAllocationMutation, err error) protectionWriteFailure {
	if err == nil || m.Service != managedServiceNodes || m.Method != "UpdateQemuConfig" {
		return protectionWriteOther
	}
	fields, fieldsErr := lifecycleMutationFields(m.Args[managedArgumentParams])
	if fieldsErr != nil || !isParkerProtectionParameters(parkerProtectionStepParameters(fields)) {
		return protectionWriteOther
	}
	if ctx.Err() == nil && pve.ProtectionWriteRefused(err) {
		return protectionWriteRefused
	}
	return protectionWriteCutOff
}

// recordNamesParker reports whether another step of record placed or moved
// the disk on vmid, which is how a record names the parker a window ran on.
func recordNamesParker(record aj.Record, vmid int) bool {
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Target.VMID != vmid {
			continue
		}
		for _, suffix := range []string{"_QEMU_AttachDisk", "_QEMU_Create", "_Nodes_CreateQemuMoveDisk"} {
			if strings.HasSuffix(step.Kind, suffix) {
				return true
			}
		}
	}
	return false
}

// protectionSettlementGap says why settlement left a protection step planned.
// Its text is the CPI's own, and a PVE error behind it is only ever rendered
// through pve.DescribeAuditError. The rendered cause follows text in
// parentheses, and after, when set, finishes the sentence behind it.
type protectionSettlementGap struct {
	text  string
	cause error
	after string
}

func (g *protectionSettlementGap) Error() string {
	if g.cause != nil {
		return g.text + " (" + pve.DescribeAuditError(g.cause) + ")" + g.after
	}
	return g.text + g.after
}

func (g *protectionSettlementGap) Unwrap() error { return g.cause }

// protectionSettlementText is the clause a refusal adds for a protection step
// the settler left planned, or "" when reason is not one of its gaps.
func protectionSettlementText(reason error) string {
	var gap *protectionSettlementGap
	if !errors.As(reason, &gap) {
		return ""
	}
	return "; its parker protection write could not be settled because " + gap.Error()
}

// parkerAnchorRunbook is the guide section a refusal points to when a vanished
// parker may have taken the disk with it.
const parkerAnchorRunbook = `see "Parker anchor missing (parked disk with no holder)" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// protectionReading is what the settler learned about a protection step's
// parker when it settles the step. warning is empty for a tagged parker that
// read protected on its recorded node, and otherwise says what the CPI read
// instead, for the warning the settler logs.
type protectionReading struct {
	warning string
}

// readParkerProtection decides whether a protection step's parker leaves
// anything unprotected. It reads the parker on its recorded node first. When
// that read answers that the VM is missing, it looks the VMID up across the
// cluster through pve.FindVMAuthoritative and reads the config wherever the
// VMID lives now. When the cluster proves the VMID gone, the step settles
// only if the disk is accounted for without the parker. When the read fails
// any other way, readPastRecordedNode decides. It returns the reading for a
// step it settles, and a gap for every other answer. It only reads PVE and
// takes no lock.
func readParkerProtection(ctx context.Context, deps Deps, record aj.Record, step aj.Step) (protectionReading, error) {
	vmid, recorded := step.Target.VMID, step.Target.Node
	client := unguardedPVE(deps.PVE)
	qemu := client.QEMU()
	if qemu == nil {
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", vmid)}
	}
	cfg, err := qemu.Config(ctx, recorded, vmid)
	if err == nil {
		return judgeParkerConfig(cfg, vmid, recorded, recorded)
	}
	if !pve.IsNotFound(err) && !pve.IsPmxcfsConfigMissing(err) {
		return readPastRecordedNode(ctx, client, vmid, recorded, err)
	}
	missing := fmt.Sprintf("parker %d wasn't found on its recorded node %s, and the cluster could not be searched for it", vmid, recorded)
	const searchRetry = "; retry once every node answers"
	if client.Cluster() == nil {
		// FindVMAuthoritative answers not found without a cluster service,
		// which would read as proven absence here.
		return protectionReading{}, &protectionSettlementGap{text: missing, after: searchRetry}
	}
	location, findErr := pve.FindVMAuthoritative(ctx, client, vmid)
	if findErr != nil {
		return protectionReading{}, &protectionSettlementGap{text: missing, cause: findErr, after: searchRetry}
	}
	if !location.Found {
		return goneParkerReading(ctx, deps, record, step)
	}
	return readListedParker(ctx, client, vmid, recorded, location.Node)
}

// readPastRecordedNode decides a step whose recorded node failed the read of
// the parker's config without saying the VM is missing, the way a node that
// is down or unreachable fails it. That read proves nothing about the parker,
// which may have been migrated away before the node stopped answering. So the
// cluster is searched, and a VM the cluster places on another node is read
// and judged there, as for a parker that moved. Every other answer keeps the
// step planned and names the recorded node. The cluster's resource list keeps
// a down node's guests, so a parker still on that node is listed there, and a
// search that skips the node because the cluster reports it offline proves
// nothing about a parker on it, so even a VMID the cluster places nowhere is
// no proof that the parker is gone. A node removed from the cluster never
// answers again, and the refusal stands until we resolve the record.
func readPastRecordedNode(ctx context.Context, client pve.Client, vmid int, recorded string, readErr error) (protectionReading, error) {
	unreadable := fmt.Sprintf("the config of parker %d could not be read on its recorded node %s", vmid, recorded)
	retry := fmt.Sprintf("; retry once node %s answers", recorded)
	if client.Cluster() == nil {
		return protectionReading{}, &protectionSettlementGap{text: unreadable, cause: readErr, after: retry}
	}
	location, findErr := pve.FindVMAuthoritative(ctx, client, vmid)
	switch {
	case findErr != nil:
		return protectionReading{}, &protectionSettlementGap{text: unreadable, cause: readErr,
			after: ", and the cluster could not be searched for it either" + retry}
	case !location.Found || location.Node == recorded:
		return protectionReading{}, &protectionSettlementGap{text: unreadable, cause: readErr,
			after: ", and the cluster places it on no other node" + retry}
	}
	return readListedParker(ctx, client, vmid, recorded, location.Node)
}

// readListedParker reads and judges the VM at the parker's VMID on node, a
// node other than the recorded one where the cluster places that VMID now.
// The VM there may be the parker after a migration or a VM created later at
// the same VMID, and the reading says only what the config shows.
func readListedParker(ctx context.Context, client pve.Client, vmid int, recorded, node string) (protectionReading, error) {
	cfg, err := client.QEMU().Config(ctx, node, vmid)
	if err != nil && (pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err)) {
		// The cluster's resource list still places the VMID on a node whose
		// config reads missing, so the list lags a deletion or a migration.
		return protectionReading{}, &protectionSettlementGap{
			text:  fmt.Sprintf("the cluster lists VM %d on node %s, but its config there reads missing", vmid, node),
			cause: err,
			after: "; retry once the cluster's resource list catches up",
		}
	}
	if err != nil {
		return protectionReading{}, &protectionSettlementGap{
			text:  fmt.Sprintf("the cluster lists VM %d on node %s, but its config there could not be read", vmid, node),
			cause: err,
			after: fmt.Sprintf("; bring %s back and retry. When %s has been removed from the cluster, no retry settles the step and resolving the record is up to the operator, who first checks whether the disk's volume still exists, "+parkerAnchorRunbook, node, node),
		}
	}
	return judgeParkerConfig(cfg, vmid, recorded, node)
}

// judgeParkerConfig reads the tag and the protection flag from the config of
// the VM at the parker's VMID on node. A VM that reads protected leaves
// nothing unprotected whoever it is, so it settles the step even without the
// parker tag. A VM without the tag and without protection may be the parker
// with its tag stripped or a newcomer at the same VMID, and PVE reads can't
// tell which, so the refusal leaves that check to us. On a node other than
// the recorded one, even a tagged VM may be a parker the CPI created later at
// the same VMID, so the texts name the VM and its node and never say that the
// parker moved.
func judgeParkerConfig(cfg map[string]any, vmid int, recorded, node string) (protectionReading, error) {
	tags, _ := pve.ConfigString(cfg, "tags")
	tagged := pve.TagsMarkParker(tags)
	protected := false
	if value, _ := pve.ConfigString(cfg, pveConfigKeyProtection); value == "1" {
		protected = true
	}
	switch {
	case !tagged && protected:
		return protectionReading{warning: fmt.Sprintf(
			"VM %d on node %s doesn't carry the %s tag and reads protected", vmid, node, pve.ParkerTag)}, nil
	case !tagged:
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf(
			"VM %d on node %s no longer carries the %s tag and its protection is off; the CPI writes nothing to it, "+
				"so check with qm config %d whether it is still the parker, put protection back with "+
				"qm set %d --protection 1 on node %s, and retry", vmid, node, pve.ParkerTag, vmid, vmid, node)}
	case node != recorded && protected:
		return protectionReading{warning: fmt.Sprintf(
			"VM %d on node %s carries the %s tag and reads protected, and the parker's recorded node is %s",
			vmid, node, pve.ParkerTag, recorded)}, nil
	case node != recorded:
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf(
			"VM %d on node %s carries the %s tag and its protection is off, and the parker's recorded node is %s; "+
				"run qm set %d --protection 1 on node %s, then retry", vmid, node, pve.ParkerTag, recorded, vmid, node)}
	case !protected:
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf(
			"protection is off on parker %d; run qm set %d --protection 1 on node %s, then retry", vmid, vmid, node)}
	}
	return protectionReading{}, nil
}

// goneParkerReading decides a step whose parker the cluster proved gone. The
// flag protected nothing but the parker, so a gone parker leaves nothing
// unprotected, but a parker that vanished while the record still placed the
// disk on it may have taken the disk with it. The step settles only when the
// disk's own resolution accounts for it without the parker, in one of two
// shapes. Either the disk is on another VM, with no transfer pending, or its
// volume reads absent with no VM naming it, and the record observed a
// delete_disk write that reached it (recordDeletedDisk). The absence read
// carries the proof that the disk is gone, and the write only ties that
// absence to the Director's own request to delete the disk. Every other
// answer, a resolution error included, keeps the step planned.
//
// The resolution runs marked as a resume, so a disk whose transfer would need
// finishing is refused instead of resumed, and the settler still writes
// nothing to PVE.
func goneParkerReading(ctx context.Context, deps Deps, record aj.Record, step aj.Step) (protectionReading, error) {
	vmid := step.Target.VMID
	gone := fmt.Sprintf("parker %d is gone from the cluster", vmid)
	const mayHaveTaken = ", so the parker may have taken the disk with it; check that the volume still exists before anything else, and " + parkerAnchorRunbook
	if record.Kind != allocationKindDisk || record.CID == "" {
		return protectionReading{}, &protectionSettlementGap{text: gone + ", and the record names no disk to look for" + mayHaveTaken}
	}
	rd, err := resolveDeleteDiskCID(context.WithValue(ctx, identityResumeKey{}, true), deps, record.CID)
	if err != nil {
		return protectionReading{}, &protectionSettlementGap{text: gone + ", and the disk could not be located", cause: err, after: mayHaveTaken}
	}
	if rd.allocation != nil && rd.allocation.record.ID == record.ID && rd.intent == nil {
		if rd.holder != nil && rd.holder.VMID != vmid && !rd.allocation.absent {
			return protectionReading{warning: fmt.Sprintf("%s and the disk is on VM %d", gone, rd.holder.VMID)}, nil
		}
		if rd.holder == nil && rd.allocation.absent && len(rd.unused) == 0 {
			if deleted := recordDeletedDisk(record, vmid); deleted != "" {
				return protectionReading{warning: fmt.Sprintf("%s, the disk's volume reads absent, and %s", gone, deleted)}, nil
			}
		}
	}
	return protectionReading{}, &protectionSettlementGap{text: gone + ", and the disk is on no VM and the record observed no delete of it" + mayHaveTaken}
}

// recordDeletedDisk reports what the active attempt of record observed of a
// delete_disk that reached the disk, or "" when it observed nothing of the
// kind. delete_disk deletes a disk in one of two ways, and the record shows
// each one differently.
//
//   - A storage delete records an observed Storage.DeleteVolume step.
//   - A disk parked under a name that embeds the parker's VMID is deleted
//     through the parker's config. Inside the window that cleared the
//     parker's protection, delete_disk detaches the slot and then sweeps the
//     unusedN entry, and on a volume the parker owns, that sweep is the
//     deallocation. An observed configuration write of that window proves
//     only that PVE answered the detach or the sweep, not that the sweep ran,
//     because a detach whose sweep never ran leaves the volume on an unusedN
//     entry that destroying the parker frees.
//
// Neither shape proves on its own that the volume is gone. The caller's
// absence read does that, and this only ties the absence to a delete the
// Director asked for.
func recordDeletedDisk(record aj.Record, parker int) string {
	window := false
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.State != aj.Observed {
			continue
		}
		if strings.HasPrefix(step.Kind, "lifecycle_delete_disk_Storage_DeleteVolume") {
			return "the record observed its storage delete"
		}
		if step.Kind != "lifecycle_delete_disk_Nodes_UpdateQemuConfig" || step.Target.VMID != parker {
			continue
		}
		switch {
		case parkerProtectionKind(step.Parameters) == parkerProtectionOffKind:
			window = true
		case parkerProtectionKind(step.Parameters) != "":
			window = false
		case window && len(step.Parameters) == 0:
			if embedded, ok := pve.EmbeddedDiskVMID(step.Target.IntendedVolume); ok && embedded == parker {
				return "the record observed a delete_disk write on the parker for that volume"
			}
		}
	}
	return ""
}

// parkerProtectionKind returns the kind recorded protection-only parameters
// carry, or "" for parameters that aren't protection-only.
func parkerProtectionKind(raw json.RawMessage) string {
	if !isParkerProtectionParameters(raw) {
		return ""
	}
	var fields struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	return fields.Kind
}

// settlePlannedProtectionSteps settles every planned protection step of
// handle's active attempt whose parker leaves nothing unprotected, and adds a
// gap to reasons for each one it leaves planned. It runs after
// settlePlannedLockSteps and takes that function's results, so a failure
// there passes through untouched. A settled step is recorded observed, with
// the disk volume it names, and the record's own state is left alone. A step
// settled on anything but a tagged, protected parker on its recorded node logs
// a warning that says what the CPI read instead, and the journal records
// nothing more than a plain readback would.
func settlePlannedProtectionSteps(ctx context.Context, deps Deps, handle *aj.Handle, reasons map[string]error, err error) (map[string]error, error) {
	if err != nil || handle == nil {
		return reasons, err
	}
	record := handle.Record()
	if n := len(record.Attempts); n > 0 && record.Attempts[n-1].Completion != nil {
		return reasons, nil
	}
	gap := func(id string, reason error) {
		if reasons == nil {
			reasons = map[string]error{}
		}
		reasons[id] = reason
	}
	var settled []aj.Step
	var warnings []string
	for i := range record.Steps {
		step := &record.Steps[i]
		if !IsParkerProtectionStep(record, *step) {
			continue
		}
		if !recordNamesParker(record, step.Target.VMID) {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("the record names no parker at VM %d", step.Target.VMID)})
			continue
		}
		if deps.PVE == nil {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", step.Target.VMID)})
			continue
		}
		reading, readErr := readParkerProtection(ctx, deps, record, *step)
		if readErr != nil {
			gap(step.ID, readErr)
			continue
		}
		step.State = aj.Observed
		if step.Target.IntendedVolume != "" && !containsString(step.VolIDs, step.Target.IntendedVolume) {
			step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
		}
		settled = append(settled, *step)
		warnings = append(warnings, reading.warning)
	}
	if len(settled) > 0 {
		if saveErr := handle.Save(record); saveErr != nil {
			return nil, refusedSettlementSave("parker protection", settled, saveErr)
		}
	}
	for i, warning := range warnings {
		if warning == "" {
			continue
		}
		step := settled[i]
		deps.Log(ctx).Warn(fmt.Sprintf("settled the parker protection restore of step %s because %s", step.ID, warning),
			log.String("allocation", record.ID),
			log.String("step", step.ID),
			log.Int("vmid", step.Target.VMID),
			log.String("recorded_node", step.Target.Node))
	}
	return reasons, nil
}

// protectionPendingRefusal is a readmission refusal whose only cause is a
// parker protection write the settler could not settle yet. Nothing about the
// disk itself is in doubt: it is where its record says, and the refusal lifts
// as soon as the parker reads back protected. Its text is the refusal's own.
type protectionPendingRefusal struct{ err error }

func (e *protectionPendingRefusal) Error() string { return e.err.Error() }

func (e *protectionPendingRefusal) Unwrap() error { return e.err }

// protectionPendingOr returns refusal marked as a protectionPendingRefusal
// when every unsettled step of record's active attempt is a protection-only
// write the settler left planned, and refusal unchanged otherwise.
func protectionPendingOr(record aj.Record, gaps map[string]error, refusal error) error {
	pending := false
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.State == aj.Observed {
			continue
		}
		var gap *protectionSettlementGap
		if !IsParkerProtectionStep(record, *step) || !errors.As(gaps[step.ID], &gap) {
			return refusal
		}
		pending = true
	}
	if !pending {
		return refusal
	}
	return &protectionPendingRefusal{err: refusal}
}
