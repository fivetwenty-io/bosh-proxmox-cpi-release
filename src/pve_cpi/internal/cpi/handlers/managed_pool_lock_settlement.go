package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A planned lock step is the journal's record of a guarded bosh-lock- sentinel
// create or delete whose outcome was never read back. Releases before the
// guard learned to settle PVE's lock refusals left such steps behind whenever
// two requests contended for one parker, and every readmission then refused the
// record as unsettled.
//
// A sentinel is an empty, TTL-bounded lock marker. It is never an allocation
// resource and it never holds a guest or a volume, so no outcome of its create
// or delete can change anything the allocation owns. That is why such a step
// can be settled after the fact by a fresh, exact readback, where any other
// planned step needs the full reconciliation it has always needed.
//
// Two more facts carry the rule.
//
//   - Settlement runs only while the caller holds the record's journal lock.
//     The request that planned the step held that same lock for its whole
//     operation, so it has finished or died. No live writer is still waiting on
//     the step.
//   - A claim that the dead request did stamp is harmless. It expires with its
//     TTL, and the next acquirer steals it exactly as it steals any crashed
//     holder's claim. Settlement never deletes a sentinel. A delete from here
//     would run outside the lock and could remove a live holder's or a
//     stealer's fresh claim, which is the cascade the lock's owner checks exist
//     to prevent.
//
// So the readback settles the step whenever it answers exactly, whether the
// sentinel is gone or holds any claim, ours or not. It leaves the step planned
// when a read fails or answers loosely, and when the record names no sentinel
// the step could have meant. The readback proves that PVE answered exactly for
// the sentinel, not what the step did. The ownerless reasoning above it is what
// makes that enough. A read that fails leaves nothing behind, so the call that
// hit it refuses, and the next call on the record reads again. By then, the
// transport's own retries have already been spent.

// lockStepKinds are the step kinds a guard writes for a sentinel mutation. The
// lifecycle and parker guards admit a Pool mutation only for a bosh-lock-
// sentinel.
//
// A VM record's pool steps are listed too, although a step does not record
// which pool it meant. Its CreatePool is either the deployment pool's ensure or
// the anti-affinity lock's sentinel, and its DeletePool can only be that
// sentinel, because the VM guard refuses every other pool delete. The
// allocation owns neither pool. The sentinel is a TTL-bounded marker like any
// other, and the deployment pool is shared infrastructure that cleanup never
// deletes, so no outcome of either step changes anything the record owns. The
// sentinel's name comes from the group the record froze in
// VMExecution.PoolInstanceGroup. A VM pool step is admitted only in the shape
// the VM guard writes, which is a target naming just a node and a VMID with
// nothing else recorded, so a step that carries anything more is never settled
// here.
func isLockStep(step aj.Step) bool {
	if step.Kind == "vm.Pool.CreatePool" || step.Kind == "vm.Pool.DeletePool" {
		bare := aj.Target{Node: step.Target.Node, VMID: step.Target.VMID}
		return step.Target == bare && bare.Node != "" && bare.VMID > 0 && step.UPID == "" && len(step.VolIDs) == 0 && len(step.Charges) == 0 && len(step.Parameters) == 0
	}
	if !strings.HasSuffix(step.Kind, "_Pool_CreatePool") && !strings.HasSuffix(step.Kind, "_Pool_DeletePool") {
		return false
	}
	return strings.HasPrefix(step.Kind, "lifecycle_") || strings.HasPrefix(step.Kind, "park_")
}

// lockStepSentinels names every sentinel a lock step in record could have
// meant. The disk guards only ever lock a parker's protection window or a
// VMID, and both are keyed "vm-<vmid>", so the candidates are the sentinels of
// every VM the active attempt's steps target. A VM record adds its instance
// group's anti-affinity sentinel, rebuilt exactly as acquireAntiAffinityLock
// names it, whenever the record froze a group.
//
// A VM record's pool step never touched its vm-<vmid> sentinel. That read is
// kept as a probe that PVE is answering exactly, and for a record that froze
// no group it is the only read, because the sentinel such a step meant belongs
// to a group the record never stored. A step records no pool of its own, so
// the rule reads every candidate and settles only when each one answers
// exactly.
func lockStepSentinels(record aj.Record) []string {
	seen := map[int]bool{}
	var sentinels []string
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.Target.VMID <= 0 || seen[step.Target.VMID] {
			continue
		}
		seen[step.Target.VMID] = true
		sentinels = append(sentinels, pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", step.Target.VMID)))
	}
	if record.Kind == "vm" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			// Without its plan the record cannot say which group it locked,
			// so it names no sentinel at all.
			return nil
		}
		if plan.VMExecution != nil {
			if group := sanitizeTagValue(plan.VMExecution.PoolInstanceGroup); group != "" {
				sentinels = append(sentinels, pve.ClusterLockPoolName(antiAffinityLockPrefix+group))
			}
		}
	}
	sort.Strings(sentinels)
	return sentinels
}

// lockSettlementGap says why settlement left a lock step planned. Its text is
// the CPI's own, and any PVE error behind it is only ever rendered through
// pve.DescribeAuditError, so a refusal that repeats it carries no raw text.
type lockSettlementGap struct {
	text, sentinel string
	cause          error
}

func (g *lockSettlementGap) Error() string {
	text := g.text
	if g.sentinel != "" {
		text += " " + g.sentinel
	}
	if g.cause != nil {
		text += " (" + pve.DescribeAuditError(g.cause) + ")"
	}
	return text
}

func (g *lockSettlementGap) Unwrap() error { return g.cause }

// readLockSentinel reads one sentinel. It succeeds when the sentinel is
// present with any claim, or when PVE answers with its exact missing-pool
// verdict. Anything else is unreadable.
func readLockSentinel(ctx context.Context, pools pve.PoolService, sentinel string) error {
	reader, ok := pools.(pve.RawPoolCommentReader)
	if !ok {
		return &lockSettlementGap{text: "the pool service cannot read sentinel", sentinel: sentinel}
	}
	if _, err := reader.ReadPoolComment(ctx, sentinel); err != nil && !exactPoolReadMissing(err, sentinel) {
		return &lockSettlementGap{text: "PVE did not answer exactly for sentinel", sentinel: sentinel, cause: err}
	}
	return nil
}

// settlePlannedLockSteps settles every planned lock step of handle's active
// attempt when the sentinels those steps could have meant all read back
// exactly. It returns, per step it left planned, the reason it did. A settled
// step is recorded as the guard records one: observed, with the disk volume a
// lifecycle step names, and without touching the record's own state.
func settlePlannedLockSteps(ctx context.Context, client pve.Client, handle *aj.Handle) (gaps map[string]error, settleErr error) {
	defer func() { gaps, settleErr = settlePlannedProtectionSteps(ctx, client, handle, gaps, settleErr) }()
	if handle == nil {
		return nil, nil
	}
	record := handle.Record()
	if n := len(record.Attempts); n > 0 && record.Attempts[n-1].Completion != nil {
		// A closed attempt's evidence is immutable, and its completion proof
		// already settles every step it holds.
		return nil, nil
	}
	if record.Kind == "vm" && !managedVMWorkBegan(record) {
		// A VM record whose own work never began is left alone. Its create_vm
		// stopped between planning its first pool call and its first VM
		// write, and closing such a record is what an attested cleanup's
		// writer fencing exists for, so settlement must not become a way
		// around it.
		return nil, nil
	}
	var planned []int
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && step.State == aj.Planned && isLockStep(*step) {
			planned = append(planned, i)
		}
	}
	if len(planned) == 0 {
		return nil, nil
	}
	skipped := func(reason error) map[string]error {
		reasons := map[string]error{}
		for _, i := range planned {
			reasons[record.Steps[i].ID] = reason
		}
		return reasons
	}
	sentinels := lockStepSentinels(record)
	if len(sentinels) == 0 {
		return skipped(&lockSettlementGap{text: "the record names no sentinel the step could have meant"}), nil
	}
	if client == nil {
		return skipped(&lockSettlementGap{text: "no PVE client is available to read the sentinel"}), nil
	}
	pools := unguardedPVE(client).Pools()
	if pools == nil {
		return skipped(&lockSettlementGap{text: "no pool service is available to read the sentinel"}), nil
	}
	for _, sentinel := range sentinels {
		if err := readLockSentinel(ctx, pools, sentinel); err != nil {
			return skipped(err), nil
		}
	}
	for _, i := range planned {
		step := &record.Steps[i]
		step.State = aj.Observed
		if strings.HasPrefix(step.Kind, "lifecycle_") && step.Target.IntendedVolume != "" && !containsString(step.VolIDs, step.Target.IntendedVolume) {
			step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
		}
	}
	if err := handle.Save(record); err != nil {
		// The journal's error stays in the chain, and the text names the
		// steps this write was settling so a refusal says which ones.
		names := make([]string, 0, len(planned))
		for _, i := range planned {
			names = append(names, fmt.Sprintf("step %s (%s)", record.Steps[i].ID, record.Steps[i].Kind))
		}
		return nil, fmt.Errorf("settling lock %s: %w", strings.Join(names, ", "), err)
	}
	return nil, nil
}

// managedVMWorkBegan reports whether the active attempt of a VM record holds
// an observed step that is not a pool step, which shows that the VM's own work
// began.
func managedVMWorkBegan(record aj.Record) bool {
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && step.State == aj.Observed && !strings.HasPrefix(step.Kind, "vm.Pool.") {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// unsettledStepName names one step the way refusals do.
func unsettledStepName(step aj.Step) string {
	return fmt.Sprintf("step %s (%s) is %s", step.ID, step.Kind, step.State)
}

// unsettledStepText names the first step of record that is neither observed
// nor covered by a closed attempt's proof, with the reason settlement left it,
// in the safe form refusals use. It returns "" when every step is settled.
func unsettledStepText(record aj.Record, reasons map[string]error, settled func(aj.Step) bool) string {
	for i := range record.Steps {
		step := record.Steps[i]
		if step.State == aj.Observed || settled != nil && settled(step) {
			continue
		}
		text := unsettledStepName(step)
		var gap *lockSettlementGap
		if errors.As(reasons[step.ID], &gap) {
			text += "; its lock sentinel could not be settled because " + gap.Error()
		} else if extra := protectionSettlementText(reasons[step.ID]); extra != "" {
			text += extra
		} else if isLockStep(step) {
			text += "; its lock sentinel was not read"
		}
		return text
	}
	return ""
}
