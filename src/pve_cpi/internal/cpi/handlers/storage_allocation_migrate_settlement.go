package handlers

import (
	"context"
	"errors"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	decisionActionSettleMigration = "settle-migration"
	migrateStepKindPrefix         = "lifecycle_"
	migrateStepKindSuffix         = "_Nodes_CreateQemuMigrate"
	maxMigrationStepIDLength      = 256
)

// newDecisionDenyGuard wraps client in a guard that refuses every PVE
// mutation, so an allocation decision can only read PVE. It returns the guard
// and the refusal it answers each mutation with. A denied mutation poisons
// the guard, so a caller that checks Err before it saves never records a
// decision that something on its path tried to write PVE under.
func newDecisionDenyGuard(client pve.Client) (*ManagedAllocationGuard, error, error) {
	denied := storageRefusal("allocation decisions cannot mutate PVE")
	guard, err := NewManagedAllocationGuard(client, ManagedAllocationHooks{
		Before: func(context.Context, ManagedAllocationMutation) (string, error) { return "", denied },
		After:  func(context.Context, ManagedAllocationMutation, string, any) error { return denied },
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error { return denied },
	})
	if err != nil {
		return nil, nil, err
	}
	return guard, denied, nil
}

// SettleStorageAllocationMigration marks one submitted cross-node migration
// step of a disk record observed, once PVE proves that the step's migration
// finished and left the disk where the step says. It is the operator's way
// out of a record that a migration's unobserved outcome left in
// reconciliation_required, such as a migration whose source node answered
// the guard's readback with an error that didn't say the config was gone.
//
// It never writes PVE. It reads the step's qmigrate task on the node the
// task's UPID names and requires it stopped with exit status OK, then proves
// the move with the same readback the guard uses: the source no longer has
// the mover's config, the cluster's config probe places the mover on the
// step's target alone, the target holds no migrate lock, the disk's serial is
// on exactly one slot there, whose backing is the step's and whose volume is
// the one the step recorded, and that volume is in its storage's listing on
// the target. Only then does it save the step observed. The record stays in
// reconciliation_required with a reason that names the decision, and the
// Director's next call on the disk resumes from it, because lifecycle
// admission requires every step of the active attempt observed, not a
// returned record.
//
// A copy-local migration, which carries a capacity charge, is refused, and
// so is a record with any other unsettled step. Both attestations are
// required, because the decision's evidence is only as good as the operator's
// word that no other writer and no other task is still acting on the disk.
func SettleStorageAllocationMigration(
	ctx context.Context, deps Deps, journal *aj.Journal, nodes []string, decision StorageAllocationDecision,
) (result aj.Record, retErr error) {
	defer func() {
		if retErr != nil {
			deps.recordStorageReconciliation(ctx, "rejected")
		}
	}()
	if ctx == nil || deps.PVE == nil {
		return result, storageRefusal("a journal and bounded nonsecret decision reference are required")
	}
	raw := unguardedPVE(deps.PVE)
	guard, denied, err := newDecisionDenyGuard(deps.PVE)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	deps.PVE = guard.Client()
	return settleStorageAllocationMigration(ctx, deps, raw, guard, denied, journal, nodes, decision)
}

// settleStorageAllocationMigration is SettleStorageAllocationMigration with
// the guard already in place: deps.PVE is guard's client, which refuses every
// write, and raw is the client beneath it, which serves the task readback
// that the guard's client doesn't forward.
func settleStorageAllocationMigration(
	ctx context.Context, deps Deps, raw pve.Client, guard *ManagedAllocationGuard, denied error,
	journal *aj.Journal, nodes []string, decision StorageAllocationDecision,
) (result aj.Record, retErr error) {
	source, vmid, err := admitMigrationSettlement(ctx, deps, raw, guard, journal, decision)
	if err != nil {
		return result, err
	}
	// Inspect before requesting ownership so an absent or corrupt record does
	// not create a new lock file. Acquire repeats the read under ownership.
	if _, err := journal.Inspect(decision.AllocationID); err != nil {
		return result, storageDecisionSourceError(err)
	}
	handle, err := journal.Acquire(ctx, decision.AllocationID)
	if err != nil {
		return result, storageDecisionSourceError(err)
	}
	defer func() { retErr = errors.Join(retErr, storageDecisionSourceError(handle.Close())) }()
	return settleMigrationUnderHandle(ctx, deps, raw, guard, denied, journal, handle, nodes, decision, source, vmid)
}

// admitMigrationSettlement checks the decision before anything is read, and
// returns the node the task's UPID names, which is the migration's source,
// and the VM it names.
func admitMigrationSettlement(
	ctx context.Context, deps Deps, raw pve.Client, guard *ManagedAllocationGuard, journal *aj.Journal,
	decision StorageAllocationDecision,
) (string, int, error) {
	if ctx == nil || deps.Config == nil || deps.PVE == nil || raw == nil || guard == nil || journal == nil ||
		strings.TrimSpace(decision.DecisionID) == "" || len(decision.DecisionID) > 256 ||
		strings.ContainsAny(decision.DecisionID, "\r\n\x00") {
		return "", 0, storageRefusal("a journal and bounded nonsecret decision reference are required")
	}
	if decision.Action != decisionActionSettleMigration {
		return "", 0, storageRefusal("unsupported allocation decision")
	}
	if missing := adoptMissingAttestations(decision); len(missing) > 0 {
		return "", 0, storageRefusalf("settle-migration requires the operator's attestations that no earlier "+
			"writer and no remote task is still acting on the disk; this decision lacks %s",
			joinWithOxfordComma(missing))
	}
	if strings.TrimSpace(decision.ExpectedCID) == "" {
		return "", 0, storageRefusal("settle-migration requires --expected-cid, the disk's exact CID")
	}
	step := decision.MigrationStepID
	if strings.TrimSpace(step) == "" || len(step) > maxMigrationStepIDLength ||
		strings.ContainsFunc(step, isControlRune) {
		return "", 0, storageRefusal("settle-migration requires --step-id, the ID of the migration step to settle, " +
			"as one bounded line")
	}
	source, vmid, err := pve.MigrationTaskSource(decision.MigrationTaskUPID)
	if err != nil {
		return "", 0, storageRefusal("settle-migration requires --task-upid, the UPID of the step's qmigrate task: " +
			err.Error())
	}
	return source, vmid, nil
}

func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f }

// settleMigrationUnderHandle runs the settlement while handle holds the
// record's lock.
func settleMigrationUnderHandle(
	ctx context.Context, deps Deps, raw pve.Client, guard *ManagedAllocationGuard, denied error,
	journal *aj.Journal, handle *aj.Handle, nodes []string, decision StorageAllocationDecision,
	source string, vmid int,
) (aj.Record, error) {
	record := handle.Record()
	if record.Namespace != deps.Config.StoragePlacementNamespace {
		return aj.Record{}, storageRefusal("allocation decision namespace differs from journal authority")
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), nodes)
	if err != nil || identity.ID() != record.ClusterID {
		return aj.Record{}, storageRefusal("allocation decision requires verified live cluster continuity")
	}
	if err := settleableMigrationRecord(record, decision); err != nil {
		return aj.Record{}, err
	}
	// A lock, protection, or move step an earlier request left planned is
	// settled by readback first, the way every decision does. Those writes
	// touch only the journal.
	gaps, err := settlePlannedLockSteps(ctx, deps, handle)
	if err != nil {
		return aj.Record{}, storageDecisionSourceError(err)
	}
	record = handle.Record()
	if err := settleableMigrationRecord(record, decision); err != nil {
		return aj.Record{}, err
	}
	step, err := settleableMigrationStep(record, decision, source, vmid, nodes)
	if err != nil {
		return aj.Record{}, err
	}
	if text := otherUnsettledStepText(record, step.ID, gaps); text != "" {
		return aj.Record{}, storageRefusal("settle-migration settles only a record whose other steps are all " +
			"settled; " + text + "; reconcile that step first")
	}
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return aj.Record{}, storageDecisionSourceError(err)
	}
	if err := storageAuditGateError(ctx, deps, "migration settlement", report, storageAuditGateAll); err != nil {
		return aj.Record{}, err
	}
	task, err := pve.ObserveSettledMigrationTask(ctx, raw, step.UPID, vmid)
	if err != nil {
		return aj.Record{}, storageRefusal(err.Error())
	}
	slot, volume, err := observeSettledMigration(ctx, deps, record, step, source)
	if err != nil {
		return aj.Record{}, err
	}
	if guard.Err() != nil {
		return aj.Record{}, denied
	}
	next := handle.Record()
	if markStepObserved(&next, step.ID, []string{step.Target.IntendedVolume, volume}) == nil {
		return aj.Record{}, storageRefusalf("allocation %s has no step %s in its open attempt", record.ID, step.ID)
	}
	next.Reason = "migration settled by decision " + decision.DecisionID
	if err := handle.Save(next); err != nil {
		return aj.Record{}, refusedSettlementSave("migration", []aj.Step{step}, err)
	}
	deps.Log(ctx).Warn("storage-journal settle-migration settled a cross-node migration step from a readback of "+
		"its finished task and of the mover where the migration left it; the record stays in "+
		"reconciliation_required until the Director's next call on the disk resumes it",
		log.String("allocation_id", record.ID),
		log.String("decision_id", decision.DecisionID),
		log.String("step_id", step.ID),
		log.String("kind", step.Kind),
		log.String("source_node", source),
		log.String("target_node", step.Target.Node),
		log.Int("vmid", vmid),
		log.String("slot", slot),
		log.String("volume", volume),
		log.String("serial", record.DiskToken),
		log.String("upid", step.UPID),
		log.String("exit_status", task.ExitStatus),
	)
	return handle.Record(), nil
}

// settleableMigrationRecord refuses a record that isn't a disk in
// reconciliation_required with the decision's exact CID.
func settleableMigrationRecord(record aj.Record, decision StorageAllocationDecision) error {
	if record.Kind != allocationKindDisk || record.State != aj.ReconciliationRequired ||
		record.CID == "" || record.CID != decision.ExpectedCID {
		return storageRefusalf("settle-migration requires a disk record in %s with the exact CID; allocation %s "+
			"is a %s record in state %s", aj.ReconciliationRequired, record.ID, record.Kind, record.State)
	}
	return nil
}

// settleableMigrationStep returns the step the decision names, once it is a
// submitted cross-node migration of the active, open attempt whose UPID is
// the decision's, which names the step's VM and a source other than the
// step's target, with both nodes among nodes. A step that carries a charge is
// a copy-local migration, whose volumes PVE copied to the target's storage,
// and is refused.
func settleableMigrationStep(
	record aj.Record, decision StorageAllocationDecision, source string, vmid int, nodes []string,
) (aj.Step, error) {
	step, found := recordStep(record, decision.MigrationStepID)
	if !found {
		return aj.Step{}, storageRefusalf("allocation %s has no step %s", record.ID, decision.MigrationStepID)
	}
	op, cut := strings.CutPrefix(step.Kind, migrateStepKindPrefix)
	op, suffixed := strings.CutSuffix(op, migrateStepKindSuffix)
	if !cut || !suffixed || op == "" {
		return aj.Step{}, storageRefusalf("step %s (%s) is not a cross-node migration step", step.ID, step.Kind)
	}
	n := len(record.Attempts)
	if step.Attempt != record.ActiveAttempt() || n == 0 || record.Attempts[n-1].Completion != nil {
		return aj.Step{}, storageRefusalf("step %s is not in the allocation's open attempt", step.ID)
	}
	if step.State != aj.Submitted {
		return aj.Step{}, storageRefusalf("step %s (%s) is %s, not submitted; settle-migration settles only a "+
			"migration whose task PVE started", step.ID, step.Kind, step.State)
	}
	if step.UPID != decision.MigrationTaskUPID {
		return aj.Step{}, storageRefusalf("the decision's --task-upid does not match the UPID step %s recorded",
			step.ID)
	}
	if len(step.Charges) > 0 {
		return aj.Step{}, storageRefusalf("settling a copy-local migration is not supported in 0.9.7; step %s "+
			"carries a capacity charge on the target's storage", step.ID)
	}
	target := step.Target
	if target.External || target.VMID <= 0 || target.Node == "" || target.IntendedVolume == "" ||
		target.Backing == "" || record.DiskToken == "" {
		return aj.Step{}, storageRefusalf("step %s does not record the managed mover, node, volume, and backing "+
			"a settlement must observe", step.ID)
	}
	if vmid != target.VMID {
		return aj.Step{}, storageRefusalf("task %s names VM %d, but step %s migrates VM %d",
			step.UPID, vmid, step.ID, target.VMID)
	}
	if source == target.Node {
		return aj.Step{}, storageRefusalf("task %s ran on node %s, the step's target; a cross-node migration's "+
			"task runs on its source node", step.UPID, source)
	}
	if !containsString(nodes, source) || !containsString(nodes, target.Node) {
		return aj.Step{}, storageRefusalf("step %s migrates from node %s to node %s, and both must be among the "+
			"nodes this decision may read", step.ID, source, target.Node)
	}
	return step, nil
}

// otherUnsettledStepText names the first step of record other than id that is
// neither observed nor covered by a closed attempt's proof, with the reason
// settlement left it planned. It returns "" when there is none.
func otherUnsettledStepText(record aj.Record, id string, gaps map[string]error) string {
	return unsettledStepText(record, gaps, func(step aj.Step) bool {
		return step.ID == id || storageDecisionClosedAttemptStepSettled(record, step)
	})
}

// observeSettledMigration proves that step's migration left the disk on the
// step's target: the guard's migration readback must pass, the slot carrying
// the disk's serial must hold the volume the step recorded, and that volume
// must be in its storage's listing on the target. It returns the slot and the
// volume.
func observeSettledMigration(
	ctx context.Context, deps Deps, record aj.Record, step aj.Step, source string,
) (string, string, error) {
	target := step.Target
	slot, landed, err := observeMigratedHolder(ctx, deps, target.VMID, source, target.Node, record.DiskToken,
		target.Backing)
	if err != nil {
		// The readback's own clauses are fixed text. A config probe failure
		// is wrapped after a colon, and its cause can carry backend text, so
		// it stays out of the refusal.
		reason, _, _ := strings.Cut(err.Error(), ": ")
		return "", "", storageRefusalf("the migration of step %s is not observed: %s", step.ID, reason)
	}
	if landed != target.IntendedVolume {
		return "", "", storageRefusalf("the mover's slot %s on node %s carries volume %s, not volume %s that step "+
			"%s recorded", slot, target.Node, landed, target.IntendedVolume, step.ID)
	}
	present, err := managedVolumePresent(ctx, deps, target.Node, landed)
	if err != nil {
		return "", "", storageRefusalf("the content listing for volume %s on node %s failed with %s", landed,
			target.Node, pve.StorageVolumeObservationReason(err))
	}
	if !present {
		return "", "", storageRefusalf("volume %s is not in its storage's content listing on node %s", landed,
			target.Node)
	}
	return slot, landed, nil
}
