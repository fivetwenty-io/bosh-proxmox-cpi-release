package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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
// The settler only reads the parker. It runs only while the caller holds the
// record's journal lock, so the request that planned the step has finished or
// died. Another request may still be inside a protection window on the same
// parker for another disk, with the flag cleared on purpose, so the settler
// reads the parker only while it holds the parker's lock, which every window
// takes, and it takes that lock once per parker for every step on it. Its one
// write to PVE is that lock's sentinel pool. A lock that another request holds
// for the whole wait leaves the steps planned with a retriable refusal that
// gives no qm set command (parkerLockBusyOr).
//
// The order is the journal lock first and then the parker's lock, the order
// every disk operation that opens a window under its record already takes.
// Nothing takes a journal lock while it holds a parker's lock, and nothing
// calls the settler from inside a window, so the order can't invert.

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
// parentheses, and after, when set, finishes the sentence behind it. lock,
// when set, is the failure to take the parker's lock that kept the settler
// from reading the parker at all, and a gap that carries it is retriable
// (parkerLockBusyOr). A lock that another request held for the whole wait is
// said in the CPI's own words, and every other lock failure is also the
// gap's cause, so its text renders what failed (parkerLockGap).
type protectionSettlementGap struct {
	text  string
	cause error
	after string
	lock  error
}

func (g *protectionSettlementGap) Error() string {
	if g.cause != nil {
		return g.text + " (" + pve.DescribeAuditError(g.cause) + ")" + g.after
	}
	return g.text + g.after
}

func (g *protectionSettlementGap) Unwrap() error {
	if g.cause != nil {
		return g.cause
	}
	return g.lock
}

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

// settleProtectionLockPurpose names the settler in the owner of the parker
// lock claims it takes.
const settleProtectionLockPurpose = "settle_protection"

// protectionResult is what the settler concluded for one protection step: a
// reading for a step it settles, or the gap that keeps it planned.
type protectionResult struct {
	reading protectionReading
	err     error
}

// readParkerProtection decides, for every protection step on parker vmid,
// whether the parker leaves anything unprotected. It reads the parker only
// while it holds the parker's lock, the one every protection window on that
// parker takes, because a window clears the flag on purpose and a read inside
// one would judge a parker that another request is still working on. It takes
// the lock once for all of steps, so the journal lock its caller holds waits
// on each parker at most once. The lock is named for the VMID alone, so it is
// the same lock on whichever node the parker is found. It waits for the lock
// as long as a journal-managed disk operation does (managedLockWaitContext),
// and when the lock stays held, or can't be taken, every step stays planned
// with a gap that says so and gives no qm set command.
//
// Under the lock it reads the parker on each step's recorded node first, once
// per node. When that read answers that the VM is missing, it looks the VMID
// up across the cluster through pve.FindVMAuthoritative and reads the config
// wherever the VMID lives now. When the read fails any other way,
// readPastRecordedNode decides. When the cluster proves the VMID gone, the
// lock is released first, and the steps settle only if the disk is accounted
// for without the parker. The results line up with steps. It writes nothing to
// PVE but the lock's sentinel.
func readParkerProtection(ctx context.Context, deps Deps, record aj.Record, vmid int, steps []aj.Step) []protectionResult {
	results := make([]protectionResult, len(steps))
	every := func(err error) []protectionResult {
		for i := range results {
			results[i] = protectionResult{err: err}
		}
		return results
	}
	client := unguardedPVE(deps.PVE)
	if client.QEMU() == nil {
		return every(&protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", vmid)})
	}
	type nodeReading struct {
		reading protectionReading
		gone    bool
		err     error
	}
	readings := map[string]nodeReading{}
	logger := deps.Log(ctx)
	lockErr := pve.RunUnderParkerLock(managedLockWaitContext(ctx), client, logger, vmid, settleProtectionLockPurpose,
		func(lockedCtx context.Context) error {
			check := pve.ParkerLockCheck(vmid, pve.ParkerLockUnserialized(lockedCtx))
			for i := range steps {
				node := steps[i].Target.Node
				if _, read := readings[node]; read {
					continue
				}
				reading, gone, err := readParkerUnderLock(lockedCtx, client, vmid, node, check)
				readings[node] = nodeReading{reading: reading, gone: gone, err: err}
			}
			return nil
		})
	if lockErr != nil {
		gap := parkerLockGap(vmid, lockErr)
		logger.Warn("left the parker protection restore planned because "+gap.Error(),
			log.Int("vmid", vmid),
			log.String("lock", pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid))),
			log.Bool("held_by_another_operation", parkerLockHeld(lockErr)),
			log.Int("steps", len(steps)),
			log.Err(lockErr))
		return every(gap)
	}
	var gone *protectionResult
	for i := range steps {
		read := readings[steps[i].Target.Node]
		if !read.gone {
			results[i] = protectionResult{reading: read.reading, err: read.err}
			continue
		}
		if gone == nil {
			// The disk's resolution reads every VM that may hold it and needs
			// nothing from the parker, which is gone, so it runs once, after
			// the lock is released and never inside a window's deadline.
			reading, err := goneParkerReading(ctx, deps, record, vmid)
			gone = &protectionResult{reading: reading, err: err}
		}
		results[i] = *gone
	}
	return results
}

// parkerLockHeld reports whether lockErr says that another request held the
// parker's lock for the whole wait. A wait the request's deadline left no room
// for, and an acquire that could not tell who holds the lock, say nothing of
// the kind, even when a timeout comes with them.
func parkerLockHeld(lockErr error) bool {
	switch {
	case errors.Is(lockErr, pve.ErrClusterLockNoTimeToWait),
		errors.Is(lockErr, pve.ErrClusterLockStateUnknown),
		errors.Is(lockErr, pve.ErrClusterLockClaimTooShort),
		errors.Is(lockErr, pve.ErrClusterLockInterrupted):
		return false
	}
	return errors.Is(lockErr, pve.ErrClusterLockTimeout)
}

// parkerLockGap is the gap for steps whose parker lock the settler could not
// take. A lock that stayed held for the whole wait means another request is
// inside a window on the parker, where protection is off on purpose, so the
// gap says that operation is in progress and to retry once it ends. Every
// other acquire failure leaves the holder unknown, so the gap says what failed
// instead, with the lock error as its cause, and never claims that another
// operation holds the lock. Neither reads the parker, so neither gives a qm
// set command.
func parkerLockGap(vmid int, lockErr error) *protectionSettlementGap {
	lock := pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid))
	if parkerLockHeld(lockErr) {
		return &protectionSettlementGap{
			text:  fmt.Sprintf("another operation on parker %d is in progress and holds the parker's lock %s", vmid, lock),
			after: "; retry once that operation finishes",
			lock:  lockErr,
		}
	}
	gap := &protectionSettlementGap{
		text:  fmt.Sprintf("the parker's lock %s could not be taken to read parker %d, because %s", lock, vmid, parkerLockFailure(lockErr)),
		after: "; retry",
		lock:  lockErr,
	}
	if !parkerLockReasonSaysAll(lockErr) {
		gap.cause = lockErr
	}
	return gap
}

// parkerLockReasonSaysAll reports whether parkerLockFailure's reason already
// says everything about lockErr. The CPI raises these failures itself, so
// they carry no PVE answer to render, and the audit description of such an
// error would only say that it is unclassified.
func parkerLockReasonSaysAll(lockErr error) bool {
	return errors.Is(lockErr, pve.ErrClusterLockNoTimeToWait) ||
		errors.Is(lockErr, pve.ErrClusterLockClaimTooShort) ||
		errors.Is(lockErr, pve.ErrMutationNotAttempted)
}

// parkerLockFailure says in the CPI's own words why an acquire of the
// parker's lock failed without showing that another request holds it. The
// gap renders the error itself behind it.
func parkerLockFailure(lockErr error) string {
	switch {
	case errors.Is(lockErr, pve.ErrClusterLockNoTimeToWait):
		return "the request had no time left to wait for it"
	case errors.Is(lockErr, pve.ErrClusterLockInterrupted):
		return "the request ended while it waited for it"
	case errors.Is(lockErr, pve.ErrClusterLockClaimTooShort):
		return "the CPI confirmed its claim too late to use it"
	case errors.Is(lockErr, pve.ErrClusterLockStateUnknown):
		return "PVE never confirmed who holds it"
	case errors.Is(lockErr, pve.ErrMutationNotAttempted):
		return "the CPI did not send the lock's create to PVE"
	}
	return "its create or a read of it failed"
}

// readParkerUnderLock reads and judges the parker while the caller holds its
// lock. gone reports that the cluster proved the VMID gone, which the caller
// decides once the lock is released. check is the clause a refusal puts
// before its qm set command (pve.ParkerLockCheck).
func readParkerUnderLock(ctx context.Context, client pve.Client, vmid int, recorded, check string) (protectionReading, bool, error) {
	cfg, err := client.QEMU().Config(ctx, recorded, vmid)
	if err == nil {
		reading, judgeErr := judgeParkerConfig(cfg, vmid, recorded, recorded, check)
		return reading, false, judgeErr
	}
	if !pve.IsNotFound(err) && !pve.IsPmxcfsConfigMissing(err) {
		reading, pastErr := readPastRecordedNode(ctx, client, vmid, recorded, check, err)
		return reading, false, pastErr
	}
	missing := fmt.Sprintf("parker %d wasn't found on its recorded node %s, and the cluster could not be searched for it", vmid, recorded)
	const searchRetry = "; retry once every node answers"
	if client.Cluster() == nil {
		// FindVMAuthoritative answers not found without a cluster service,
		// which would read as proven absence here.
		return protectionReading{}, false, &protectionSettlementGap{text: missing, after: searchRetry}
	}
	location, findErr := pve.FindVMAuthoritative(ctx, client, vmid)
	if findErr != nil {
		return protectionReading{}, false, &protectionSettlementGap{text: missing, cause: findErr, after: searchRetry}
	}
	if !location.Found {
		return protectionReading{}, true, nil
	}
	reading, listedErr := readListedParker(ctx, client, vmid, recorded, location.Node, check)
	return reading, false, listedErr
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
func readPastRecordedNode(ctx context.Context, client pve.Client, vmid int, recorded, check string, readErr error) (protectionReading, error) {
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
	return readListedParker(ctx, client, vmid, recorded, location.Node, check)
}

// readListedParker reads and judges the VM at the parker's VMID on node, a
// node other than the recorded one where the cluster places that VMID now.
// The VM there may be the parker after a migration or a VM created later at
// the same VMID, and the reading says only what the config shows.
func readListedParker(ctx context.Context, client pve.Client, vmid int, recorded, node, check string) (protectionReading, error) {
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
	return judgeParkerConfig(cfg, vmid, recorded, node, check)
}

// judgeParkerConfig reads the tag and the protection flag from the config of
// the VM at the parker's VMID on node. A VM that reads protected leaves
// nothing unprotected whoever it is, so it settles the step even without the
// parker tag. A VM without the tag and without protection may be the parker
// with its tag stripped or a newcomer at the same VMID, and PVE reads can't
// tell which, so the refusal leaves that check to us. On a node other than
// the recorded one, even a tagged VM may be a parker the CPI created later at
// the same VMID, so the texts name the VM and its node and never say that the
// parker moved. The caller reads cfg under the parker's lock whenever PVE
// lets the CPI take it, so a flag that reads off is then no window of ours.
// A request that starts after the refusal may still open one, so every text
// that gives a qm set command puts check before it, the clause that has us
// confirm that no CPI operation holds the parker's lock (pve.ParkerLockCheck).
func judgeParkerConfig(cfg map[string]any, vmid int, recorded, node, check string) (protectionReading, error) {
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
				"so check with qm config %d whether it is still the parker; %sput protection back with "+
				"qm set %d --protection 1 on node %s, and retry", vmid, node, pve.ParkerTag, vmid, check, vmid, node)}
	case node != recorded && protected:
		return protectionReading{warning: fmt.Sprintf(
			"VM %d on node %s carries the %s tag and reads protected, and the parker's recorded node is %s",
			vmid, node, pve.ParkerTag, recorded)}, nil
	case node != recorded:
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf(
			"VM %d on node %s carries the %s tag and its protection is off, and the parker's recorded node is %s; "+
				"%srun qm set %d --protection 1 on node %s, then retry", vmid, node, pve.ParkerTag, recorded, check, vmid, node)}
	case !protected:
		return protectionReading{}, &protectionSettlementGap{text: fmt.Sprintf(
			"protection is off on parker %d; %srun qm set %d --protection 1 on node %s, then retry", vmid, check, vmid, node)}
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
func goneParkerReading(ctx context.Context, deps Deps, record aj.Record, vmid int) (protectionReading, error) {
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
// nothing more than a plain readback would. The steps are grouped by parker,
// in the order the record first names each one, and every step on a parker is
// settled under one acquire of that parker's lock, so a record that a run of
// cut-off restore tries left with several steps waits on each parker once.
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
	var parkers []int
	byParker := map[int][]int{}
	for i := range record.Steps {
		step := &record.Steps[i]
		if !IsParkerProtectionStep(record, *step) {
			continue
		}
		vmid := step.Target.VMID
		if !recordNamesParker(record, vmid) {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("the record names no parker at VM %d", vmid)})
			continue
		}
		if deps.PVE == nil {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", vmid)})
			continue
		}
		if _, named := byParker[vmid]; !named {
			parkers = append(parkers, vmid)
		}
		byParker[vmid] = append(byParker[vmid], i)
	}
	var settled []aj.Step
	var warnings []string
	for _, vmid := range parkers {
		indexes := byParker[vmid]
		steps := make([]aj.Step, len(indexes))
		for j, i := range indexes {
			steps[j] = record.Steps[i]
		}
		results := readParkerProtection(ctx, deps, record, vmid, steps)
		for j, i := range indexes {
			step := &record.Steps[i]
			if results[j].err != nil {
				gap(step.ID, results[j].err)
				continue
			}
			step.State = aj.Observed
			if step.Target.IntendedVolume != "" && !containsString(step.VolIDs, step.Target.IntendedVolume) {
				step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
			}
			settled = append(settled, *step)
			warnings = append(warnings, results[j].reading.warning)
		}
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

// parkerLockBusyOr returns refusal as a retriable error when every unsettled
// step of record's active attempt is a protection-only write the settler left
// planned because it could not take the parker's lock, and refusal unchanged
// otherwise. Nothing was read, so nothing is in doubt but timing, and the
// Director's retry settles the step once the lock is free. The wrapper says
// that another operation is in progress only when every one of those gaps is
// a lock that another request held for the whole wait, and otherwise says
// only that the lock could not be taken. The refusal's own type stays in the
// chain, so a caller that tells refusals apart by type still finds it.
func parkerLockBusyOr(record aj.Record, gaps map[string]error, refusal error) error {
	busy, held := false, true
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.State == aj.Observed {
			continue
		}
		var gap *protectionSettlementGap
		if !IsParkerProtectionStep(record, *step) || !errors.As(gaps[step.ID], &gap) || gap.lock == nil {
			return refusal
		}
		busy = true
		held = held && parkerLockHeld(gap.lock)
	}
	if !busy {
		return refusal
	}
	if !held {
		return cpierrors.WrapAs(refusal, cpierrors.TypeRetriableCloud, "the parker's lock could not be taken, so retry")
	}
	return cpierrors.WrapAs(refusal, cpierrors.TypeRetriableCloud, "another operation on the parker is in progress, so retry")
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
