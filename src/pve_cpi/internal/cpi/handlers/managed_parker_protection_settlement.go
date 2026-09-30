package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
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
//     record placed or moved the disk on that VM, and the VM's live config
//     still carries the bosh-parker tag.
//   - The readback shows protection on. When it is off or absent, the step
//     stays planned and the refusal names the qm set command that puts it
//     back. When the config cannot be read, or the parker is gone, the step
//     stays planned too.
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

// isParkerProtectionStep reports whether step is a planned protection-only
// configuration write of the active attempt, read from its recorded
// parameters.
func isParkerProtectionStep(record aj.Record, step aj.Step) bool {
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
// through pve.DescribeAuditError.
type protectionSettlementGap struct {
	text  string
	cause error
}

func (g *protectionSettlementGap) Error() string {
	if g.cause != nil {
		return g.text + " (" + pve.DescribeAuditError(g.cause) + ")"
	}
	return g.text
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

// readParkerProtection reads the parker a protection step targets and
// reports nil when it is a parker with protection on. Every other answer is a
// gap that leaves the step planned.
func readParkerProtection(ctx context.Context, client pve.Client, step aj.Step) error {
	vmid := step.Target.VMID
	qemu := unguardedPVE(client).QEMU()
	if qemu == nil {
		return &protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", vmid)}
	}
	cfg, err := qemu.Config(ctx, step.Target.Node, vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return &protectionSettlementGap{text: fmt.Sprintf("parker %d no longer exists on node %s", vmid, step.Target.Node), cause: err}
		}
		return &protectionSettlementGap{text: fmt.Sprintf("the config of parker %d could not be read", vmid), cause: err}
	}
	tags, _ := pve.ConfigString(cfg, "tags")
	if !pve.TagsMarkParker(tags) {
		return &protectionSettlementGap{text: fmt.Sprintf("VM %d no longer carries the %s tag", vmid, pve.ParkerTag)}
	}
	if value, _ := pve.ConfigString(cfg, pveConfigKeyProtection); value != "1" {
		return &protectionSettlementGap{text: fmt.Sprintf(
			"protection is off on parker %d; run qm set %d --protection 1 on node %s, then retry", vmid, vmid, step.Target.Node)}
	}
	return nil
}

// settlePlannedProtectionSteps settles every planned protection step of
// handle's active attempt whose parker reads back protected, and adds a gap to
// reasons for each one it leaves planned. It runs after settlePlannedLockSteps
// and takes that function's results, so a failure there passes through
// untouched. A settled step is recorded observed, with the disk volume it
// names, and the record's own state is left alone.
func settlePlannedProtectionSteps(ctx context.Context, client pve.Client, handle *aj.Handle, reasons map[string]error, err error) (map[string]error, error) {
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
	settled := false
	for i := range record.Steps {
		step := &record.Steps[i]
		if !isParkerProtectionStep(record, *step) {
			continue
		}
		if !recordNamesParker(record, step.Target.VMID) {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("the record names no parker at VM %d", step.Target.VMID)})
			continue
		}
		if client == nil {
			gap(step.ID, &protectionSettlementGap{text: fmt.Sprintf("no PVE client is available to read parker %d", step.Target.VMID)})
			continue
		}
		if readErr := readParkerProtection(ctx, client, *step); readErr != nil {
			gap(step.ID, readErr)
			continue
		}
		step.State = aj.Observed
		if step.Target.IntendedVolume != "" && !containsString(step.VolIDs, step.Target.IntendedVolume) {
			step.VolIDs = append(step.VolIDs, step.Target.IntendedVolume)
		}
		settled = true
	}
	if settled {
		if saveErr := handle.Save(record); saveErr != nil {
			return nil, saveErr
		}
	}
	return reasons, nil
}
