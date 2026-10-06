package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// A disk call such as attach_disk can stop after it plans a configuration
// write on the disk's holder and before that write is sent or answered, such
// as the write of the holder's provenance entry for the disk. A process that
// stops there leaves the disk's record planned or observed, with a planned
// lifecycle configuration write that no readback settles, because the write
// records nothing a later read could match. Every disk call then refuses the
// record, and so does a plain adopt.
//
// An attested adopt settles such a step without changing PVE. The operator
// attests that the previous writer is fenced and that its PVE tasks have
// settled, so the write can no longer land. A fresh readback then has to show
// the disk where the write left it, which is the volume the record names,
// carrying the disk's serial, in a drive slot of the VM the step targets.
// Adopt checks the record's ownership against that readback before it saves
// the settlement, so a refusal leaves the journal as it was. It then saves the
// step observed, with a record that stopped mid-call moved to
// reconciliation_required in the same save, and adopts the record the way it
// adopts any other. A later disk call writes any provenance entry the holder
// still lacks, under the disk's allocation lock.
//
// When adopt stops between the two saves on a record that a stopped call left
// planned or observed, the record stays in reconciliation_required with a
// reason that names the settled steps, and a rerun of the attested adopt
// repeats their readback and finishes the adoption. A record that was already
// in reconciliation_required keeps its own reason, so a rerun refuses it, and
// in every case the next attach_disk, detach_disk, or delete_vm writes the
// missing entry and heals the record.

// adoptSettledKinds are the configuration writes adopt settles from a
// readback, which are the ones attach_disk, detach_disk, and delete_disk plan
// on a disk's holder.
var adoptSettledKinds = map[string]bool{
	"lifecycle_attach_disk_Nodes_UpdateQemuConfig": true,
	"lifecycle_detach_disk_Nodes_UpdateQemuConfig": true,
	"lifecycle_delete_disk_Nodes_UpdateQemuConfig": true,
}

// adoptSettledConfigStep is the evidence adopt keeps in its verification for
// one configuration write it settled: the step, and the holder, slot, volume,
// and serial its readback found.
type adoptSettledConfigStep struct {
	StepID string `json:"step_id"`
	Kind   string `json:"kind"`
	Node   string `json:"node"`
	VMID   int    `json:"vmid"`
	Slot   string `json:"slot"`
	Volume string `json:"volume"`
	Serial string `json:"serial"`
}

// adoptSettlement is what an attested adopt settles once its readback holds.
// record is the disk's record with the settled steps observed, and moved to
// reconciliation_required when stalled names the disk call that stopped
// while the record was planned or observed. steps is the evidence for each
// settled step. Adopt saves record only after its audit gate and ownership
// check pass.
type adoptSettlement struct {
	record  aj.Record
	steps   []adoptSettledConfigStep
	stalled string
}

// view returns the record adopt checks: the settled record when there is a
// settlement, and record otherwise.
func (s *adoptSettlement) view(record aj.Record) aj.Record {
	if s == nil {
		return record
	}
	return s.record
}

// settledHolderKey marks a context whose disk resolution runs
// proveSettledHolder for a holder without the disk's provenance entry. Its
// value is the settledHolder that adopt read back.
type settledHolderKey struct{}

// settledHolder is the VM whose drive slot adopt read back before it settled
// configuration writes on it, and the IDs of the steps it is settling, which
// the journal still holds planned until adopt saves.
type settledHolder struct {
	node  string
	vmid  int
	steps map[string]bool
}

// withSettledHolderProof returns ctx marked so that a disk resolution made
// with it accepts the disk on the holder settlement read back without the
// holder's provenance entry, once proveSettledHolder's proofs hold.
func withSettledHolderProof(ctx context.Context, settlement *adoptSettlement) context.Context {
	expected := settledHolder{node: settlement.steps[0].Node, vmid: settlement.steps[0].VMID, steps: map[string]bool{}}
	for _, step := range settlement.steps {
		expected.steps[step.StepID] = true
	}
	return withHolderHeal(context.WithValue(ctx, settledHolderKey{}, expected), holderHealProve)
}

// proveSettledHolder is the identity check for a holder without the disk's
// provenance entry when ctx comes from withSettledHolderProof. The holder has
// to be the VM that adopt read back, no transfer of the disk may be in
// flight, every step of the record has to be settled apart from the ones
// adopt is settling, and the proofs that healUnrecordedHolder makes before it
// writes have to hold. It writes nothing, so the holder still lacks the entry
// afterward, and the next call that changes the disk writes it under the
// disk's allocation lock.
func proveSettledHolder(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, shared bool, cfg map[string]any) error {
	if rd.holder == nil || rd.stableID == "" || cfg == nil {
		return cpierrors.Cloud("%s, because the disk has no holder with a stable identity to check", renamedHolderAudit)
	}
	expected, ok := ctx.Value(settledHolderKey{}).(settledHolder)
	if !ok || rd.holder.Node != expected.node || rd.holder.VMID != expected.vmid {
		return holderNotRecordedRefusal(rd, fmt.Sprintf(", and it is not VM %d on node %s, which adopt read back", expected.vmid, expected.node))
	}
	if rd.intent != nil {
		return holderNotRecordedRefusal(rd, fmt.Sprintf(", and a transfer of the disk to parker %d is still in flight", rd.intent.ParkerVMID))
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State == aj.Observed || storageDecisionClosedAttemptStepSettled(record, *step) || expected.steps[step.ID] {
			continue
		}
		return holderNotRecordedRefusal(rd, ", and "+unsettledStepName(*step)+" in the disk's record")
	}
	return unrecordedHolderProof(ctx, deps, rd, record, shared, cfg)
}

// adoptableRecord reports whether adopt accepts record for decision: a record
// in ready_to_return, or a disk in reconciliation_required, whose CID is the
// one the decision expects.
func adoptableRecord(record aj.Record, decision StorageAllocationDecision) bool {
	returned := record.State == aj.ReadyToReturn || record.Kind == allocationKindDisk && record.State == aj.ReconciliationRequired
	return returned && record.CID != "" && record.CID == decision.ExpectedCID
}

// adoptSettleableRecord reports whether adopt can settle configuration writes
// on record for decision. That takes a disk record with the CID the decision
// expects that adopt accepts as it is, or one that a disk call left planned or
// observed when it stopped, which stalled then names.
func adoptSettleableRecord(record aj.Record, decision StorageAllocationDecision) (stalled string, ok bool) {
	if record.Kind != allocationKindDisk {
		return "", false
	}
	if adoptableRecord(record, decision) {
		return "", true
	}
	if record.CID == "" || record.CID != decision.ExpectedCID || record.State != aj.Planned && record.State != aj.Observed {
		return "", false
	}
	return lifecycleOperation(record.Reason)
}

// adoptStalledReason is the reason adopt saves on a record that operation
// left planned or observed when it moves the record to
// reconciliation_required, naming the steps it settled.
// adoptStalledSettlement reads it back.
func adoptStalledReason(operation string, steps []string) string {
	writes := "write "
	if len(steps) > 1 {
		writes = "writes "
	}
	return adoptStalledPrefix(operation) + writes + joinWithOxfordComma(steps) + adoptStalledSuffix
}

// adoptStalledSuffix ends every reason adoptStalledReason writes.
const adoptStalledSuffix = " from a readback under attestation"

// adoptStalledPrefix begins the reason adoptStalledReason writes for
// operation.
func adoptStalledPrefix(operation string) string {
	return "lifecycle " + operation + " stopped; adopt settled its configuration "
}

// adoptStalledSettlement reads the operation and the settled steps from a
// reason that adoptStalledReason wrote, and reports false for any other
// reason.
func adoptStalledSettlement(reason string) (operation string, steps []string, ok bool) {
	rest, ok := strings.CutPrefix(reason, "lifecycle ")
	if !ok {
		return "", nil, false
	}
	operation, _, ok = strings.Cut(rest, " stopped; ")
	if !ok || operation == "" || strings.ContainsAny(operation, " ,") {
		return "", nil, false
	}
	rest, ok = strings.CutSuffix(strings.TrimPrefix(reason, adoptStalledPrefix(operation)), adoptStalledSuffix)
	if !ok {
		return "", nil, false
	}
	_, list, _ := strings.Cut(rest, " ")
	for _, field := range strings.FieldsFunc(list, func(r rune) bool { return r == ' ' || r == ',' }) {
		if field != "and" {
			steps = append(steps, field)
		}
	}
	if len(steps) == 0 || adoptStalledReason(operation, steps) != reason {
		return "", nil, false
	}
	return operation, steps, true
}

// adoptStalledKind is the kind of the configuration write that operation
// plans on a disk's holder, which is the only write adopt settles on a record
// that operation left planned or observed.
func adoptStalledKind(operation string) string {
	return "lifecycle_" + operation + "_Nodes_UpdateQemuConfig"
}

// adoptRecordStateClause is the clause adopt's refusal adds when it can't
// settle a write on the record as it stands.
const adoptRecordStateClause = "; adopt settles this write only on a disk record with the exact CID that is in reconciliation_required, or that the disk call which planned the write left planned or observed when it stopped"

// adoptMissingAttestations names the storage-journal flags that adopt needs
// to settle a configuration write and decision lacks, always in the same
// order.
func adoptMissingAttestations(decision StorageAllocationDecision) []string {
	var missing []string
	if !decision.PreviousWriterFenced {
		missing = append(missing, "--previous-writer-fenced")
	}
	if !decision.RemoteTasksSettled {
		missing = append(missing, "--remote-tasks-settled")
	}
	return missing
}

// adoptConfigStepAdmission reports whether adopt can settle step from a
// readback, which takes a planned configuration write of attach_disk,
// detach_disk, or delete_disk in the record's active attempt that has no
// task, charges nothing, records no volume, targets a VM, and isn't a parker
// protection write. When adopt can't settle step, reason says which condition
// failed, and it is empty for a step that isn't a configuration write at all,
// whose refusal stays as it was.
func adoptConfigStepAdmission(record aj.Record, step aj.Step) (admitted bool, reason string) {
	switch {
	case !strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig"):
		return false, ""
	case IsParkerProtectionStep(record, step):
		return false, "; adopt doesn't settle a parker protection write from the disk's readback, because that write settles only when the parker reads back protected"
	case !adoptSettledKinds[step.Kind]:
		return false, "; adopt settles only a configuration write that attach_disk, detach_disk, or delete_disk planned"
	case step.UPID != "":
		return false, "; it has a PVE task, and adopt settles only a configuration write that has none"
	case len(step.Charges) != 0:
		return false, "; it charges capacity, and adopt settles only a configuration write that charges nothing"
	case !cleanupConfigStep(step, record):
		return false, "; adopt settles only a planned configuration write of the disk record's active attempt that targets a VM and records no volume"
	}
	return true, ""
}

// planAdoptConfigSettlement works out, for an adopt decision, whether adopt
// can settle every unsettled step of record. It can when each step is a
// configuration write that adoptConfigStepAdmission admits, the decision
// attests both that the previous writer is fenced and that its PVE tasks
// settled, adoptSettleableRecord accepts the record, and a fresh readback
// finds the disk's volume, with its serial, in a drive slot of the VM each
// step targets. It saves nothing and returns the settlement for adopt to save
// once its other checks pass.
//
// When any condition fails, or the decision isn't an adopt, it settles
// nothing and returns, per step, the clause that adopt's refusal adds after
// naming that step. A step that isn't a configuration write gets no clause,
// so its refusal is the one it had before.
func planAdoptConfigSettlement(ctx context.Context, deps Deps, record aj.Record, decision StorageAllocationDecision) (*adoptSettlement, map[string]string) {
	reasons := map[string]string{}
	if decision.Action != decisionActionAdopt {
		return nil, reasons
	}
	var candidates []int
	var blocked *aj.Step
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State == aj.Observed || storageDecisionClosedAttemptStepSettled(record, *step) {
			continue
		}
		admitted, reason := adoptConfigStepAdmission(record, *step)
		if !admitted {
			if reason != "" {
				reasons[step.ID] = reason
			}
			if blocked == nil {
				blocked = step
			}
			continue
		}
		candidates = append(candidates, i)
	}
	if len(candidates) == 0 {
		return nil, reasons
	}
	refuseAll := func(reason string) (*adoptSettlement, map[string]string) {
		for _, i := range candidates {
			reasons[record.Steps[i].ID] = reason
		}
		return nil, reasons
	}
	if blocked != nil {
		return refuseAll(fmt.Sprintf("; adopt settles this write only together with every other unsettled step, and it can't settle step %s (%s)", blocked.ID, blocked.Kind))
	}
	if missing := adoptMissingAttestations(decision); len(missing) > 0 {
		return refuseAll("; adopt settles this write from a readback only when the decision attests that the previous writer is fenced and that its PVE tasks have settled, so rerun adopt with " + joinWithOxfordComma(missing) + " once both hold")
	}
	stalled, ok := adoptSettleableRecord(record, decision)
	if !ok {
		return refuseAll(adoptRecordStateClause)
	}
	for _, i := range candidates {
		if stalled != "" && record.Steps[i].Kind != adoptStalledKind(stalled) {
			return refuseAll(adoptRecordStateClause)
		}
	}
	rd, err := resolveDeleteDiskCID(withHolderHeal(ctx, holderHealDefer), deps, record.CID)
	if err != nil {
		for _, i := range candidates {
			reasons[record.Steps[i].ID] = adoptUnresolvedReason(ctx, deps, record, record.Steps[i], err)
		}
		return nil, reasons
	}
	settled := make([]adoptSettledConfigStep, 0, len(candidates))
	for _, i := range candidates {
		step := record.Steps[i]
		evidence, reason := adoptConfigReadback(ctx, deps, rd, record, step)
		if reason != "" {
			return refuseAll(fmt.Sprintf("; adopt settles this write only together with every other unsettled step, and its readback of step %s failed%s", step.ID, reason))
		}
		settled = append(settled, evidence)
	}
	next := record
	next.Steps = slices.Clone(record.Steps)
	ids := make([]string, 0, len(candidates))
	for _, i := range candidates {
		step := &next.Steps[i]
		step.State = aj.Observed
		if !containsString(step.VolIDs, rd.volid) {
			step.VolIDs = append(slices.Clone(step.VolIDs), rd.volid)
		}
		ids = append(ids, step.ID)
	}
	if stalled != "" {
		next.State = aj.ReconciliationRequired
		next.Reason = adoptStalledReason(stalled, ids)
	}
	return &adoptSettlement{record: next, steps: settled, stalled: stalled}, reasons
}

// saveAdoptSettlement saves the settlement adopt planned, which writes only
// the journal, and logs each step it settled.
func saveAdoptSettlement(ctx context.Context, deps Deps, handle *aj.Handle, settlement *adoptSettlement) error {
	if err := handle.Save(settlement.record); err != nil {
		return refusedSettlementSave("configuration write", settlement.settledSteps(), err)
	}
	for _, evidence := range settlement.steps {
		deps.Log(ctx).Warn("storage-journal adopt settled a planned configuration write from a readback of the disk's holder",
			log.String("allocation_id", settlement.record.ID),
			log.String("step", evidence.StepID),
			log.String("kind", evidence.Kind),
			log.String("node", evidence.Node),
			log.Int("vmid", evidence.VMID),
			log.String("slot", evidence.Slot),
			log.String("volid", evidence.Volume),
			log.String("stalled_operation", settlement.stalled),
		)
	}
	return nil
}

// settledSteps returns the steps of the settled record that adopt settled.
func (s *adoptSettlement) settledSteps() []aj.Step {
	steps := make([]aj.Step, 0, len(s.steps))
	for _, evidence := range s.steps {
		for i := range s.record.Steps {
			if s.record.Steps[i].ID == evidence.StepID {
				steps = append(steps, s.record.Steps[i])
			}
		}
	}
	return steps
}

// adoptionAfterSettlementSaveError is adopt's error when it saved the
// settlement and then failed to save the adoption with cause. The settled
// steps stay observed and the record stays in reconciliation_required, so it
// says so and names the disk calls that go on from there. When the
// settlement saved adoptStalledReason, a rerun of the attested adopt also
// finishes the adoption, so it names that too.
func adoptionAfterSettlementSaveError(settlement *adoptSettlement, cause error) error {
	rerun := ""
	if settlement.stalled != "" {
		rerun = "; a rerun of this adopt with --previous-writer-fenced and --remote-tasks-settled reads the VM back again and finishes the adoption"
	}
	return errors.Join(storageRefusalf("adopt settled %s from its readback of VM %d on node %s and saved the record in reconciliation_required, and then it could not save the adoption; the disk stays where the readback found it, and the next attach_disk, detach_disk, or delete_vm of the disk writes the holder's provenance entry under the disk's allocation lock and goes on%s",
		settledStepNames(settlement.steps), settlement.steps[0].VMID, settlement.steps[0].Node, rerun), storageDecisionSourceError(cause))
}

// settledStepNames names each step in steps with its kind, as in
// "step attempt-0-step-4 (lifecycle_attach_disk_Nodes_UpdateQemuConfig)".
func settledStepNames(steps []adoptSettledConfigStep) string {
	names := make([]string, 0, len(steps))
	for _, evidence := range steps {
		names = append(names, fmt.Sprintf("step %s (%s)", evidence.StepID, evidence.Kind))
	}
	return joinWithOxfordComma(names)
}

// adoptConfigReadback reads back the VM that step targets and checks that rd,
// the disk as a fresh resolution found it, sits there. The disk has to
// resolve to record's allocation with no transfer in flight, its holder has
// to be the VM and node the step targets and not a parker, the volume has to
// be the one the step names when it names one, and a drive slot in the VM's
// configuration, read again here, has to name the volume with the disk's
// serial. It returns the evidence, or the clause adopt's refusal adds when a
// check fails.
func adoptConfigReadback(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, step aj.Step) (adoptSettledConfigStep, string) {
	target := step.Target
	switch {
	case rd.allocation == nil || rd.allocation.record.ID != record.ID:
		return adoptSettledConfigStep{}, "; adopt read the disk back, and it doesn't resolve to this allocation"
	case rd.intent != nil:
		return adoptSettledConfigStep{}, fmt.Sprintf("; adopt read the disk back and found a transfer of it to parker %d still in flight", rd.intent.ParkerVMID)
	case rd.holder == nil || rd.stableID == "":
		return adoptSettledConfigStep{}, fmt.Sprintf("; adopt read the disk back and found it in no drive slot, where the step targets VM %d on node %s", target.VMID, target.Node)
	case rd.holder.Node != target.Node || rd.holder.VMID != target.VMID:
		return adoptSettledConfigStep{}, fmt.Sprintf("; adopt read the disk back and found it on VM %d on node %s, where the step targets VM %d on node %s", rd.holder.VMID, rd.holder.Node, target.VMID, target.Node)
	case rd.holder.IsParker || pve.TagsMarkParker(rd.holder.Tags):
		return adoptSettledConfigStep{}, fmt.Sprintf("; the step targets VM %d, which is a parker, and adopt settles only a write on a workload VM", target.VMID)
	case target.IntendedVolume != "" && target.IntendedVolume != rd.volid:
		return adoptSettledConfigStep{}, fmt.Sprintf("; the step names volume %s, and adopt read the disk back as %s", target.IntendedVolume, rd.volid)
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, target.Node, target.VMID)
	if err != nil || cfg == nil {
		description := "it returned no configuration"
		if err != nil {
			description = pve.DescribeAuditError(err)
		}
		return adoptSettledConfigStep{}, fmt.Sprintf("; adopt could not read VM %d on node %s back to settle it (%s)", target.VMID, target.Node, description)
	}
	slot, ok := holderSlotWithDisk(cfg, rd.volid, rd.stableID)
	if !ok {
		return adoptSettledConfigStep{}, fmt.Sprintf("; adopt read VM %d on node %s back, and no drive slot there names %s with the disk's serial %s", target.VMID, target.Node, rd.volid, rd.stableID)
	}
	return adoptSettledConfigStep{StepID: step.ID, Kind: step.Kind, Node: target.Node, VMID: target.VMID, Slot: slot, Volume: rd.volid, Serial: rd.stableID}, ""
}

// adoptUnresolvedReason is the clause adopt's refusal adds for step when the
// disk didn't resolve from record's CID, with resolveErr as the cause. It
// reads the VM the step targets back once more, so the clause says whether a
// drive slot there still carries the disk's serial, the token its record
// keeps.
func adoptUnresolvedReason(ctx context.Context, deps Deps, record aj.Record, step aj.Step, resolveErr error) string {
	target := step.Target
	cfg, err := deps.PVE.QEMU().Config(ctx, target.Node, target.VMID)
	if err != nil || cfg == nil {
		description := "it returned no configuration"
		if err != nil {
			description = pve.DescribeAuditError(err)
		}
		return fmt.Sprintf("; adopt could not read VM %d on node %s back to settle it (%s)", target.VMID, target.Node, description)
	}
	if record.DiskToken != "" && !holderSlotCarriesSerial(cfg, record.DiskToken) {
		return fmt.Sprintf("; adopt read VM %d on node %s back, and no drive slot there carries the disk's serial %s", target.VMID, target.Node, record.DiskToken)
	}
	return "; adopt could not resolve the disk from its CID to settle it (" + pve.DescribeAuditError(resolveErr) + "), so run storage-journal audit to see where the disk is"
}

// holderSlotCarriesSerial reports whether a drive slot in cfg carries
// stableID as its serial, whatever volume it names.
func holderSlotCarriesSerial(cfg map[string]any, stableID string) bool {
	for _, value := range qemu.ParseDisks(cfg) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == stableID {
			return true
		}
	}
	return false
}

// unsettledDecisionText names the first unsettled step of record for a
// decision's refusal, with the reason settlement left it from gaps and, for a
// configuration write adopt didn't settle, the condition that failed from
// adoptReasons. It returns "" when every step is settled.
func unsettledDecisionText(record aj.Record, gaps map[string]error, adoptReasons map[string]string) string {
	text := unsettledStepText(record, gaps, func(step aj.Step) bool { return storageDecisionClosedAttemptStepSettled(record, step) })
	if step, ok := unsettledRecordStep(record); ok && text != "" {
		text += adoptReasons[step.ID]
	}
	return text
}

// adoptOwnershipContext returns the context adopt observes the disk's
// ownership with. A holder that adopt just read back for a settled
// configuration write may still lack the disk's provenance entry, which that
// write may have been about to record, so the observation then accepts it on
// the same proofs the next disk call makes before it writes the entry.
//
// The same holds for a record that an earlier attested adopt settled and
// then stopped before it saved the adoption, when the decision carries both
// attestations again. adoptResumedSettlement repeats the readback of each
// step that adopt settled, and the readback evidence it returns goes into the
// adoption's evidence. Every other record keeps the plain ownership check,
// which refuses a holder without the entry.
func adoptOwnershipContext(ctx context.Context, deps Deps, record aj.Record, decision StorageAllocationDecision, settlement *adoptSettlement) (context.Context, []adoptSettledConfigStep, error) {
	if settlement != nil {
		return withSettledHolderProof(ctx, settlement), nil, nil
	}
	resumed, err := adoptResumedSettlement(ctx, deps, record, decision)
	if err != nil || resumed == nil {
		return ctx, nil, err
	}
	return withSettledHolderProof(ctx, resumed), resumed.steps, nil
}

// adoptResumedSettlement reads back the steps that an earlier attested adopt
// settled on record when it stopped before it saved the adoption. That record
// is a disk in reconciliation_required whose reason adoptStalledReason wrote.
// It returns nil for any other record, or when decision isn't an adopt that
// attests both that the previous writer is fenced and that its PVE tasks have
// settled. Each step the reason names has to be an observed configuration
// write of the stopped operation in the record's active attempt, with no
// task, no charge, and a VM target, that isn't a parker protection write, and
// its readback has to hold as it did when adopt settled it. When either
// fails, it refuses with the condition that failed.
func adoptResumedSettlement(ctx context.Context, deps Deps, record aj.Record, decision StorageAllocationDecision) (*adoptSettlement, error) {
	if decision.Action != decisionActionAdopt || record.Kind != allocationKindDisk || record.State != aj.ReconciliationRequired || len(adoptMissingAttestations(decision)) > 0 {
		return nil, nil
	}
	stalled, ids, ok := adoptStalledSettlement(record.Reason)
	if !ok {
		return nil, nil
	}
	steps := make([]aj.Step, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		step, found := recordStep(record, id)
		if !found || !adoptResumableStep(record, step, stalled) {
			return nil, storageRefusalf("an earlier attested adopt of the stopped %s saved the record in reconciliation_required, and adopt can't finish that adoption, because the record no longer holds step %s as an observed %s write with no task; run storage-journal audit and investigate the record before the next disk call",
				stalled, id, adoptStalledKind(stalled))
		}
		steps = append(steps, step)
		names = append(names, fmt.Sprintf("step %s (%s)", step.ID, step.Kind))
	}
	refuse := func(reason string) error {
		return storageRefusalf("an earlier attested adopt settled %s of the stopped %s and saved the record in reconciliation_required, and adopt can't finish that adoption%s; run storage-journal audit to see where the disk is, and the next attach_disk, detach_disk, or delete_vm of the disk checks it again under its allocation lock",
			joinWithOxfordComma(names), stalled, reason)
	}
	rd, err := resolveDeleteDiskCID(withHolderHeal(ctx, holderHealDefer), deps, record.CID)
	if err != nil {
		return nil, refuse(adoptUnresolvedReason(ctx, deps, record, steps[0], err))
	}
	evidence := make([]adoptSettledConfigStep, 0, len(steps))
	for i := range steps {
		settled, reason := adoptConfigReadback(ctx, deps, rd, record, steps[i])
		if reason != "" {
			return nil, refuse(fmt.Sprintf(", because its readback of step %s failed%s", steps[i].ID, reason))
		}
		evidence = append(evidence, settled)
	}
	return &adoptSettlement{record: record, steps: evidence, stalled: stalled}, nil
}

// adoptResumableStep reports whether step is a configuration write that an
// attested adopt settled on record for the stopped operation: an observed
// write of that operation's kind in the record's active attempt, with no
// task, no charge, and a VM target, that isn't a parker protection write.
func adoptResumableStep(record aj.Record, step aj.Step, operation string) bool {
	return step.State == aj.Observed && step.Kind == adoptStalledKind(operation) && step.Attempt == record.ActiveAttempt() &&
		step.UPID == "" && len(step.Charges) == 0 && step.Target.VMID > 0 && step.Target.Node != "" && !step.Target.External &&
		!IsParkerProtectionStep(record, step)
}

// recordStep returns the step of record with id.
func recordStep(record aj.Record, id string) (aj.Step, bool) {
	for i := range record.Steps {
		if record.Steps[i].ID == id {
			return record.Steps[i], true
		}
	}
	return aj.Step{}, false
}
