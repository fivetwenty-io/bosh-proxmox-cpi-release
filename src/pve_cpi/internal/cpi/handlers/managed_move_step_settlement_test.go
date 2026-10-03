package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// pastQuietPeriod is a settlement clock offset past the quiet period, and
// insideQuietPeriod is one inside it.
const (
	pastQuietPeriod   = 11 * time.Minute
	insideQuietPeriod = 5 * time.Minute
)

// settleAt returns a request context whose move settlement reads the wall
// clock shifted by offset, with the move retry loop's backoff taken out.
func settleAt(offset time.Duration) context.Context {
	return WithSettlementClockForTest(digestCtx(), func() time.Time { return time.Now().Add(offset) })
}

// settleAtTime returns a request context whose move settlement reads the
// fixed time at, with the move retry loop's backoff taken out.
func settleAtTime(at time.Time) context.Context {
	return WithSettlementClockForTest(digestCtx(), func() time.Time { return at })
}

// unfiredMove is a managed disk whose move POST failed without an answer,
// which left the record in reconciliation_required with the move step
// planned and no UPID.
type unfiredMove struct {
	digestManaged
	// step is the planned move step, and before is the record as the failed
	// POST left it.
	step   aj.Step
	before aj.Record
	// source is the VM the move would have taken the disk from.
	source int
	token  string
	// deletes, destroyed, and volumes are the fake's delete count, the
	// number of volumes it had destroyed, and its volume count when the move
	// failed.
	deletes, destroyed, volumes int
}

// errLostMove is the failure every unfired row gives its move POST. It is
// not an answer from PVE, so the guard can't know whether a task forked.
var errLostMove = errors.New("move_disk: connection reset before PVE answered")

// failMove runs op with every move POST failing without an answer, and
// returns the unfired move it leaves behind. With lose set, PVE forks the
// task before the answer is lost, so the move can still land, through
// runLostMove(0), while the step stays planned.
func failMove(t *testing.T, f digestManaged, kind string, lose bool, op func() error) *unfiredMove {
	t.Helper()
	if lose {
		f.client.loseMoveResponse = true
	} else {
		f.client.moveErr = errLostMove
	}
	if err := op(); err == nil {
		t.Fatal("the operation succeeded although its move POST failed")
	}
	f.client.moveErr, f.client.loseMoveResponse = nil, false
	if lose && len(f.client.lostMoves) != 1 {
		t.Fatalf("want one forked task, got %d", len(f.client.lostMoves))
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("the failed move left the record %s", record.State)
	}
	var step aj.Step
	for i := range record.Steps {
		s := record.Steps[i]
		if s.Attempt == record.ActiveAttempt() && s.State == aj.Planned && strings.HasSuffix(s.Kind, "_Nodes_CreateQemuMoveDisk") {
			step = s
		}
	}
	if step.ID == "" || step.Kind != "lifecycle_"+kind+"_Nodes_CreateQemuMoveDisk" || step.UPID != "" {
		t.Fatalf("want a planned, UPID-less %s move step, got %+v", kind, record.Steps)
	}
	return &unfiredMove{digestManaged: f, step: step, before: record, source: step.Target.VMID, token: record.DiskToken, deletes: f.client.deletes, destroyed: len(f.client.destroyed), volumes: len(f.client.state.volumes)}
}

// unfiredDetach is a detach whose move to the parker never answered. The
// volume is on 777's unused entry, and the parker holds the transfer record.
func unfiredDetach(t *testing.T, lose bool) *unfiredMove {
	t.Helper()
	f := digestManagedFixture(t)
	return failMove(t, f, "detach_disk", lose, func() error { return digestDetach(t, f.deps, f.cid) })
}

// unfiredResume is a detach whose move PVE refused on its digest, which
// settles inline and leaves the volume on 777's unused entry with the
// transfer record kept, followed by an operation whose resume of the park
// sends a move that never answers. The step's kind names that operation, and
// its source is still 777.
func unfiredResume(t *testing.T, lose bool, kind string, op func(f digestManaged) error) *unfiredMove {
	t.Helper()
	f := digestManagedFixture(t)
	f.client.changeSourceBeforeMove = true
	if err := digestDetach(t, f.deps, f.cid); !errors.Is(err, pve.ErrMoveDiskDigestRefused) {
		t.Fatalf("want the detach's digest refusal, got %v", err)
	}
	if unusedHolding(f.client, f.volume) == "" {
		t.Fatalf("the refused detach did not leave %s on 777's unused entry: %v", f.volume, f.client.state.configs[777])
	}
	return failMove(t, f, kind, lose, func() error { return op(f) })
}

// unfiredAttachFromParker is a parked disk whose attach back to 777 sent a
// move that never answered. The move's source is the parker, and the disk is
// still on the parker's bus slot with its serial.
func unfiredAttachFromParker(t *testing.T, lose bool) *unfiredMove {
	t.Helper()
	f := digestManagedFixture(t)
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	m := failMove(t, f, "attach_disk", lose, func() error { return digestAttach(t, f.deps, f.cid) })
	if !isParkerVM(f.client, m.source) {
		t.Fatalf("the attach's move source %d is not a parker", m.source)
	}
	return m
}

func isParkerVM(client *lifecycleFlowPVE, vmid int) bool {
	tags, _ := client.state.configs[vmid]["tags"].(string)
	return pve.TagsMarkParker(tags)
}

// deleteDiskAt runs delete_disk on the disk.
func deleteDiskAt(t *testing.T, ctx context.Context, deps Deps, diskCID string) error {
	t.Helper()
	_, err := HandleDeleteDisk(deps).Handle(ctx, []json.RawMessage{planJSON(t, diskCID)}, jsonrpc.Context{})
	return err
}

// unfiredMoveShapes are every way an unfired move step is built: the detach
// itself, the resume an attach_disk or a delete_disk sends for a park the
// detach left on 777's unused entry, and an attach whose move leaves a
// parker.
var unfiredMoveShapes = []struct {
	name  string
	build func(t *testing.T, lose bool) *unfiredMove
}{
	{"detach", unfiredDetach},
	{"attach-resume", func(t *testing.T, lose bool) *unfiredMove {
		return unfiredResume(t, lose, "attach_disk", func(f digestManaged) error { return digestAttach(t, f.deps, f.cid) })
	}},
	{"delete-resume", func(t *testing.T, lose bool) *unfiredMove {
		return unfiredResume(t, lose, "delete_disk", func(f digestManaged) error { return deleteDiskAt(t, digestCtx(), f.deps, f.cid) })
	}},
	{"attach-side", unfiredAttachFromParker},
}

// requireRecordUnchanged fails unless the record reads back exactly as
// before, its update time included.
func (m *unfiredMove) requireRecordUnchanged(t *testing.T) {
	t.Helper()
	record, err := m.journal.Inspect(m.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record, m.before) {
		t.Fatalf("the record changed:\nbefore %+v\nafter  %+v", m.before, record)
	}
}

// requireOnlyMoveSettled fails unless every step the record held before is
// unchanged except the move step, which is observed with no UPID and with
// IntendedVolume appended to its volumes. With exact set, the record must
// also hold no new step and differ in nothing else but its update time.
func (m *unfiredMove) requireOnlyMoveSettled(t *testing.T, exact bool) aj.Record {
	t.Helper()
	record, err := m.journal.Inspect(m.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Steps) < len(m.before.Steps) {
		t.Fatalf("the record lost steps: %d before, %d after", len(m.before.Steps), len(record.Steps))
	}
	for i := range m.before.Steps {
		was, now := m.before.Steps[i], record.Steps[i]
		if was.ID != m.step.ID {
			if !reflect.DeepEqual(now, was) {
				t.Fatalf("step %s changed:\nbefore %+v\nafter  %+v", was.ID, was, now)
			}
			continue
		}
		want := was
		want.State = aj.Observed
		want.VolIDs = append(append([]string{}, was.VolIDs...), was.Target.IntendedVolume)
		if containsString(was.VolIDs, was.Target.IntendedVolume) {
			want.VolIDs = was.VolIDs
		}
		if !reflect.DeepEqual(now, want) {
			t.Fatalf("move step %s did not settle as never started:\nwant %+v\ngot  %+v", was.ID, want, now)
		}
	}
	if exact {
		rest, wasRest := record, m.before
		rest.Steps, wasRest.Steps = nil, nil
		rest.UpdatedAt, wasRest.UpdatedAt = time.Time{}, time.Time{}
		if len(record.Steps) != len(m.before.Steps) || !reflect.DeepEqual(rest, wasRest) {
			t.Fatalf("the settlement changed more than the move step:\nbefore %+v\nafter  %+v", m.before, record)
		}
	}
	return record
}

// requireNothingDeleted fails when any volume was deleted or destroyed since
// the move failed, or when the disk no longer has exactly one slot carrying its serial
// on a volume that exists, unless the disk sits on an unused entry under the
// volume the move step names.
func (m *unfiredMove) requireNothingDeleted(t *testing.T) {
	t.Helper()
	m.requireVolumesKept(t)
	holders := map[string]string{}
	for vmid, cfg := range m.client.state.configs {
		for key, value := range cfg {
			text, _ := value.(string)
			if isDiskOptionKey(key) && strings.Contains(text, "serial="+m.token) {
				holders[fmt.Sprintf("%d.%s", vmid, key)] = strings.Split(text, ",")[0]
			}
		}
	}
	if len(holders) == 0 {
		if m.client.state.volumes[m.step.Target.IntendedVolume] == nil {
			t.Fatalf("no slot carries serial %s, and %s is gone", m.token, m.step.Target.IntendedVolume)
		}
		return
	}
	if len(holders) != 1 {
		t.Fatalf("slots carrying serial %s: %v, want one", m.token, holders)
	}
	for slot, volume := range holders {
		if m.client.state.volumes[volume] == nil {
			t.Fatalf("%s carries serial %s on %s, which does not exist", slot, m.token, volume)
		}
	}
}

// requireVolumesKept fails when any volume was deleted or destroyed since the
// move failed.
func (m *unfiredMove) requireVolumesKept(t *testing.T) {
	t.Helper()
	if m.client.deletes != m.deletes || len(m.client.state.volumes) != m.volumes {
		t.Fatalf("volumes were deleted: %d delete calls, and %d volumes where there were %d", m.client.deletes-m.deletes, len(m.client.state.volumes), m.volumes)
	}
}

// TestManagedDetachSettlesUnfiredMoveStep reruns detach_disk past the quiet
// period on every shape of unfired move. The settlement settles the step, and
// the detach then parks the disk. Before this change the rerun refused on the
// planned step forever.
func TestManagedDetachSettlesUnfiredMoveStep(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			if err := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid); err != nil {
				t.Fatalf("detach_disk after the quiet period: %v", err)
			}
			m.requireOnlyMoveSettled(t, false)
			m.requireNothingDeleted(t)
			if record, _ := m.journal.Inspect(m.id); record.State != aj.ReadyToReturn {
				t.Fatalf("the detach left the record %s", record.State)
			}
		})
	}
}

// TestManagedAttachElsewhereSettlesUnfiredMoveStep reruns the Director's next
// attach, to another VM, past the quiet period. The settlement settles the
// step, and the attach then moves the disk to 888.
func TestManagedAttachElsewhereSettlesUnfiredMoveStep(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			m.client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
			if err := attachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "888", m.cid); err != nil {
				t.Fatalf("attach_disk to 888 after the quiet period: %v", err)
			}
			m.requireOnlyMoveSettled(t, false)
			m.requireNothingDeleted(t)
			holder := ""
			for key, value := range m.client.state.configs[888] {
				if text, _ := value.(string); isDiskOptionKey(key) && strings.Contains(text, "serial="+m.token) {
					holder = key
				}
			}
			if holder == "" {
				t.Fatalf("888 does not carry serial %s: %v", m.token, m.client.state.configs[888])
			}
		})
	}
}

// TestManagedDeleteDiskSettlesUnfiredMoveStep reruns delete_disk past the
// quiet period. The settlement settles the step, and the delete then removes
// the disk's own volume and nothing else. A disk the detach left on 777's
// unused entry keeps 777's provenance after the delete, which ends the
// delete in reconciliation exactly as it does when no move was lost, so the
// row accepts that answer and checks that it names nothing but 777's
// provenance.
func TestManagedDeleteDiskSettlesUnfiredMoveStep(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			err := deleteDiskAt(t, settleAt(pastQuietPeriod), m.deps, m.cid)
			record := m.requireOnlyMoveSettled(t, false)
			switch {
			case err == nil && record.State == aj.Deleted:
			case err != nil && strings.Contains(err.Error(), "managed disk artifact or provenance remains") && record.State == aj.ReconciliationRequired:
				description, _ := m.client.state.configs[777]["description"].(string)
				if !strings.Contains(description, m.token) {
					t.Fatalf("the delete refused although 777 no longer holds the disk's provenance: %v", err)
				}
			default:
				t.Fatalf("delete_disk after the quiet period answered %v and left the record %s", err, record.State)
			}
			if m.client.deletes > m.deletes+1 || len(m.client.state.volumes) != m.volumes-1 {
				t.Fatalf("want exactly the disk's own volume deleted, got %d delete calls and %d volumes from %d", m.client.deletes-m.deletes, len(m.client.state.volumes), m.volumes)
			}
			for vmid, cfg := range m.client.state.configs {
				for key, value := range cfg {
					if text, _ := value.(string); isDiskOptionKey(key) && strings.Contains(text, "serial="+m.token) {
						t.Fatalf("%d.%s still carries the deleted disk's serial", vmid, key)
					}
				}
			}
		})
	}
}

// storageDecisionRows are the operator's journal decisions that run the lock
// step settlement before their own rules.
// done is the state a decision that goes ahead leaves, and deletes says
// whether it may remove the disk.
var storageDecisionRows = []struct {
	name    string
	done    aj.State
	deletes bool
	run     func(ctx context.Context, m *unfiredMove) error
}{
	{"adopt", aj.Adopted, false, func(ctx context.Context, m *unfiredMove) error {
		_, err := ApplyStorageAllocationDecision(ctx, m.deps, m.journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: m.id, ExpectedCID: m.cid, DecisionID: "unfired-move-adoption"})
		return err
	}},
	{"finalize-cleanup", aj.Cleaned, false, func(ctx context.Context, m *unfiredMove) error {
		_, err := ApplyStorageAllocationDecision(ctx, m.deps, m.journal, []string{"n1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: m.id, DecisionID: "unfired-move-finalize"})
		return err
	}},
	{"cleanup", aj.Cleaned, true, func(ctx context.Context, m *unfiredMove) error {
		_, err := CleanupStorageAllocation(ctx, m.deps, m.journal, []string{"n1"}, cleanupAttestedDecision(m.id))
		return err
	}},
}

// TestStorageJournalDecisionSettlesUnfiredMoveStep runs each journal decision
// past the quiet period. The settlement settles the step, and the decision
// then answers on its own rules, never on the planned step. A decision that
// refuses leaves nothing changed but the step, and one that goes ahead leaves
// its own end state. A cleanup that goes ahead deletes exactly one volume,
// the disk's own, which is the volume the move step names.
func TestStorageJournalDecisionSettlesUnfiredMoveStep(t *testing.T) {
	for _, decision := range storageDecisionRows {
		for _, shape := range unfiredMoveShapes {
			t.Run(decision.name+"/"+shape.name, func(t *testing.T) {
				m := shape.build(t, false)
				err := decision.run(settleAt(pastQuietPeriod), m)
				if err != nil {
					if strings.Contains(err.Error(), "unsettled mutation evidence") || strings.Contains(err.Error(), "never started") {
						t.Fatalf("the decision refused on the move step: %v", err)
					}
					m.requireOnlyMoveSettled(t, true)
					m.requireNothingDeleted(t)
					return
				}
				if record := m.requireOnlyMoveSettled(t, false); record.State != decision.done {
					t.Fatalf("%s went ahead and left the record %s", decision.name, record.State)
				}
				if !decision.deletes {
					m.requireNothingDeleted(t)
					return
				}
				volume := m.step.Target.IntendedVolume
				if destroyed := m.client.destroyed[m.destroyed:]; len(destroyed) != 1 || destroyed[0] != volume || len(m.client.state.volumes) != m.volumes-1 {
					t.Fatalf("want exactly %s deleted, got %v deleted and %d volumes from %d", volume, destroyed, len(m.client.state.volumes), m.volumes)
				}
			})
		}
	}
}

// reattach is the operator's documented recovery for a disk whose transfer
// record was collected. It puts the volume back on a free bus slot of 777
// with its serial, and PVE drops the unused entry that named it.
func (m *unfiredMove) reattach(t *testing.T) {
	t.Helper()
	cfg := m.client.state.configs[777]
	unused := unusedHolding(m.client, m.step.Target.IntendedVolume)
	if unused == "" {
		t.Fatalf("777 has no unused entry naming %s: %v", m.step.Target.IntendedVolume, cfg)
	}
	if _, taken := cfg["scsi1"]; taken {
		t.Fatalf("777's scsi1 is not free: %v", cfg)
	}
	delete(cfg, unused)
	cfg["scsi1"] = m.step.Target.IntendedVolume + ",serial=" + m.token
	changeConfig(m.client)
}

// TestManagedDetachSettlesMoveStepAfterReattach builds the record a release
// before 0.9.0 left, whose parker collected the transfer record an hour after
// the detach's move was lost. After the operator's reattach, the source VM is
// the one holder of the disk's serial, on a bus slot naming the volume, and
// the rerun detach settles the step and parks the disk with one reference.
// Before the reattach, delete_disk's absence finalization runs the
// settlement, which refuses on the step because no parker keeps the transfer
// record, and the delete deletes nothing. 777's description entries are
// stripped for that call, so the refusal can't rest on 777's provenance.
func TestManagedDetachSettlesMoveStepAfterReattach(t *testing.T) {
	for _, deleteFirst := range []bool{false, true} {
		name := "reattach"
		if deleteFirst {
			name = "delete-refuses-then-reattach"
		}
		t.Run(name, func(t *testing.T) {
			m := unfiredDetach(t, false)
			(&strandedDisk{client: m.client, token: m.token}).dropRecord(t)
			// Without the parker's record, nothing ties the disk to the
			// volume on 777's unused entry, so detach_disk refuses before the
			// settlement runs, and only the reattach lets it through.
			if err := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid); err == nil || !strings.Contains(err.Error(), "terminal managed disk cannot be mutated") {
				t.Fatalf("want detach_disk refused before the reattach, got %v", err)
			}
			m.requireRecordUnchanged(t)
			m.requireNothingDeleted(t)
			if deleteFirst {
				// Admission finds no live volume under the disk's name, so
				// delete_disk goes to the absence finalization, which runs
				// the settlement first. An unused entry carries no serial, and
				// with the transfer record gone nothing shows that the volume
				// on it is still this disk, so the settlement refuses on the
				// step.
				description := m.client.state.configs[777]["description"]
				stripDiskEntries(t, m.client, 777)
				var refusal error
				m.requireRefusedBy(t, func() error {
					refusal = deleteDiskAt(t, settleAt(pastQuietPeriod), m.deps, m.cid)
					return refusal
				}, "no parker keeps the record of the disk's transfer from VM 777")
				requireCloudErrorNotRetried(t, refusal)
				m.client.state.configs[777]["description"] = description
				m.requireNothingDeleted(t)
				if unusedHolding(m.client, m.step.Target.IntendedVolume) == "" {
					t.Fatalf("the refused delete moved %s off 777's unused entry", m.step.Target.IntendedVolume)
				}
				if record, _ := m.journal.Inspect(m.id); record.State == aj.Deleted {
					t.Fatal("the refused delete finalized the record as deleted")
				}
			}
			m.reattach(t)
			if err := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid); err != nil {
				t.Fatalf("detach_disk after the reattach: %v", err)
			}
			m.requireOnlyMoveSettled(t, false)
			m.requireNothingDeleted(t)
			if record, _ := m.journal.Inspect(m.id); record.State != aj.ReadyToReturn {
				t.Fatalf("the detach left the record %s", record.State)
			}
			if holder := unusedHolding(m.client, m.step.Target.IntendedVolume); holder != "" {
				t.Fatalf("777 still names the old volume on %s", holder)
			}
		})
	}
}

// requireCloudErrorNotRetried fails unless err carries a *cpierrors.Error of
// type CloudError with ok_to_retry false. Before move steps were settled,
// delete_disk refused this state with a plain error from
// storageLifecycleSettled, which the dispatcher sent as a CloudError with
// ok_to_retry false. The typed error states that answer on purpose instead of
// leaving it to how the dispatcher handles a plain error.
func requireCloudErrorNotRetried(t *testing.T, err error) {
	t.Helper()
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("want a *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeCloud || cpiErr.OkToRetry() {
		t.Fatalf("want a CloudError the Director doesn't retry, got %s with ok_to_retry %t: %v", cpiErr.Type(), cpiErr.OkToRetry(), err)
	}
}

// stripDiskEntries removes the disk entries, bosh_attached_disks and
// bosh_disk_allocations, from the description of VM vmid.
func stripDiskEntries(t *testing.T, client *lifecycleFlowPVE, vmid int) {
	t.Helper()
	cfg := client.state.configs[vmid]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	if raw["bosh_attached_disks"] == nil && raw["bosh_disk_allocations"] == nil {
		t.Fatalf("VM %d's description holds no disk entries: %v", vmid, cfg["description"])
	}
	delete(raw, "bosh_attached_disks")
	delete(raw, "bosh_disk_allocations")
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
}

// requireRefused reruns detach_disk at ctx and fails unless it refuses on the
// move step with a gap containing want, changes nothing in the record, and
// deletes nothing.
func (m *unfiredMove) requireRefused(t *testing.T, ctx context.Context, want string) {
	t.Helper()
	m.requireRefusedBy(t, func() error { return detachDiskAt(t, ctx, m.deps, "777", m.cid) }, want)
	m.requireNothingDeleted(t)
}

// requireRefusedBy runs a call that runs the settlement and fails unless it
// refuses on the move step with a gap containing want, changes nothing in the
// record, and deletes or destroys no volume.
func (m *unfiredMove) requireRefusedBy(t *testing.T, run func() error, want string) {
	t.Helper()
	err := run()
	if err == nil {
		t.Fatal("the call settled the move step")
	}
	if text := err.Error(); !strings.Contains(text, "its move could not be settled as never started because ") || !strings.Contains(text, want) {
		t.Fatalf("want a refusal on the move step naming %q, got %v", want, err)
	}
	m.requireRecordUnchanged(t)
	m.requireVolumesKept(t)
}

// sourceKey returns the disk key of the move's source that names the volume.
func (m *unfiredMove) sourceKey(t *testing.T) string {
	t.Helper()
	for key, value := range m.client.state.configs[m.source] {
		if text, _ := value.(string); isDiskOptionKey(key) && strings.Split(text, ",")[0] == m.step.Target.IntendedVolume {
			return key
		}
	}
	t.Fatalf("VM %d names %s on no disk key: %v", m.source, m.step.Target.IntendedVolume, m.client.state.configs[m.source])
	return ""
}

// activeTask is a qmmove row of a node's active task list, with an ID in the
// form PVE gives it.
func activeTask(node, id string) pve.ActiveTask {
	return pve.ActiveTask{UPID: "UPID:" + node + ":0004D2A1:03504636:6AA1786A:qmmove:" + id + ":bosh@pve!cpi:", Node: node, Type: "qmmove", ID: id, Status: "RUNNING"}
}

// TestMoveSettlementWaitsForQuietPeriod reruns detach_disk inside the quiet
// period on every shape. The step stays planned, the refusal names the time
// the quiet period ends, and the record is unchanged.
func TestMoveSettlementWaitsForQuietPeriod(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			ends := m.before.UpdatedAt.Add(moveSettlementQuietPeriod).UTC().Format(time.RFC3339)
			m.requireRefused(t, settleAt(insideQuietPeriod), "its quiet period ends at "+ends)
			if len(m.client.moveTaskListings) != 0 {
				t.Fatalf("the settlement listed tasks inside the quiet period: %v", m.client.moveTaskListings)
			}
		})
	}
}

// TestMoveSettlementQuietPeriodBoundary reruns detach_disk one second before
// the quiet period ends and again exactly when it ends. The first refuses and
// changes nothing, and the second settles the step.
func TestMoveSettlementQuietPeriodBoundary(t *testing.T) {
	m := unfiredDetach(t, false)
	ends := m.before.UpdatedAt.Add(moveSettlementQuietPeriod)
	m.requireRefused(t, settleAtTime(ends.Add(-time.Second)), "its quiet period ends at "+ends.UTC().Format(time.RFC3339))
	if len(m.client.moveTaskListings) != 0 {
		t.Fatalf("the settlement listed tasks inside the quiet period: %v", m.client.moveTaskListings)
	}
	if err := detachDiskAt(t, settleAtTime(ends), m.deps, "777", m.cid); err != nil {
		t.Fatalf("detach_disk when the quiet period ends: %v", err)
	}
	m.requireOnlyMoveSettled(t, false)
	m.requireNothingDeleted(t)
}

// recordFile returns the path of the record's file in the journal.
func (m *unfiredMove) recordFile(t *testing.T) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(m.deps.Config.StorageAllocationJournalDir, "*", "allocation-"+m.id+".json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one record file for %s, got %v (%v)", m.id, matches, err)
	}
	return matches[0]
}

// rewriteRecord writes record over the record's file, the way a record a
// release left behind reads back, without the journal's save gate.
func (m *unfiredMove) rewriteRecord(t *testing.T, record aj.Record) {
	t.Helper()
	path := m.recordFile(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	encoded, err := json.Marshal(map[string]any{"version": aj.Version, "sha256": hex.EncodeToString(sum[:]), "payload": json.RawMessage(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if m.before, err = m.journal.Inspect(m.id); err != nil {
		t.Fatalf("the rewritten record does not read back: %v", err)
	}
}

// sentinelReadingPVE answers every lock sentinel read the way PVE answers for
// a sentinel that no longer exists, so a planned lock step settles.
type sentinelReadingPVE struct {
	*lifecycleFlowPVE
	t *testing.T
}

func (c sentinelReadingPVE) Pools() pve.PoolService {
	return sentinelReadingPools{PoolService: c.lifecycleFlowPVE.Pools(), t: c.t}
}

type sentinelReadingPools struct {
	pve.PoolService
	t *testing.T
}

func (p sentinelReadingPools) ReadPoolComment(_ context.Context, id string) (string, error) {
	return "", livePoolVerdict(p.t, "pool '"+id+"' does not exist")
}

// TestMoveSettlementQuietPeriodRestartsOnceForLockSettlement gives an unfired
// detach a planned lock step beside its move step, with a last save twenty
// minutes ago. The first rerun settles the lock step, and that save restarts
// the move's quiet period, so the move waits. A rerun inside the new period
// waits again and saves nothing, which is the bound, and a rerun after it
// settles the move.
func TestMoveSettlementQuietPeriodRestartsOnceForLockSettlement(t *testing.T) {
	m := unfiredDetach(t, false)
	m.deps.PVE = sentinelReadingPVE{lifecycleFlowPVE: m.client, t: t}
	record := m.before
	record.Steps = append(record.Steps, aj.Step{ID: fmt.Sprintf("attempt-%d-step-%d", record.ActiveAttempt(), len(record.Steps)), Attempt: record.ActiveAttempt(), Kind: "lifecycle_detach_disk_Pool_DeletePool", Target: aj.Target{Node: "n1", VMID: m.source}, State: aj.Planned})
	record.CreatedAt = record.CreatedAt.Add(-20 * time.Minute)
	record.UpdatedAt = record.UpdatedAt.Add(-20 * time.Minute)
	m.rewriteRecord(t, record)
	lock := record.Steps[len(record.Steps)-1]

	err := detachDiskAt(t, digestCtx(), m.deps, "777", m.cid)
	if err == nil || !strings.Contains(err.Error(), "its quiet period ends at ") {
		t.Fatalf("want the move to wait after the lock settlement, got %v", err)
	}
	afterLock, inspectErr := m.journal.Inspect(m.id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if got := afterLock.Steps[len(afterLock.Steps)-1]; got.ID != lock.ID || got.State != aj.Observed {
		t.Fatalf("the lock step did not settle: %+v", got)
	}
	if !afterLock.UpdatedAt.After(m.before.UpdatedAt.Add(moveSettlementQuietPeriod)) {
		t.Fatalf("the lock settlement did not save: %s", afterLock.UpdatedAt)
	}
	if ends := afterLock.UpdatedAt.Add(moveSettlementQuietPeriod).UTC().Format(time.RFC3339); !strings.Contains(err.Error(), ends) {
		t.Fatalf("want the restarted period to end at %s, got %v", ends, err)
	}
	m.before = afterLock

	m.requireRefused(t, settleAt(insideQuietPeriod), "its quiet period ends at ")

	if err := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid); err != nil {
		t.Fatalf("detach_disk after the restarted quiet period: %v", err)
	}
	m.requireOnlyMoveSettled(t, false)
	m.requireNothingDeleted(t)
}

// TestMoveSettlementRefusesRunningMoveTask lists a move task of the source in
// the step node's active list. The settlement refuses, naming the task.
func TestMoveSettlementRefusesRunningMoveTask(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			task := activeTask("n1", fmt.Sprintf("%d-%s>90777-scsi0", m.source, m.sourceKey(t)))
			m.client.activeMoveTasks = map[string][]pve.ActiveTask{"n1": {task}}
			m.requireRefused(t, settleAt(pastQuietPeriod), "move task "+task.UPID+" is still active on node n1")
		})
	}
}

// TestMoveSettlementTaskMatchIgnoresPVEVmidFilter checks the task match on
// IDs PVE's own vmid filter would miss or confuse. A move to another VM has
// the ID "<vmid>-<disk>><target>-<disk>", which PVE's filter never matches,
// and a VMID that merely shares the source's digits must not match.
func TestMoveSettlementTaskMatchIgnoresPVEVmidFilter(t *testing.T) {
	for _, row := range []struct {
		id      string
		matches bool
	}{
		{"777-unused0>90030-scsi0", true},
		{"777", true},
		{"7770-unused0>9-scsi0", false},
		{"77-unused0>9-scsi0", false},
		{"1777-scsi1>777-scsi0", false},
	} {
		t.Run(row.id, func(t *testing.T) {
			m := unfiredDetach(t, false)
			task := activeTask("n1", row.id)
			m.client.activeMoveTasks = map[string][]pve.ActiveTask{"n1": {task}}
			if row.matches {
				m.requireRefused(t, settleAt(pastQuietPeriod), "move task "+task.UPID+" is still active on node n1")
				return
			}
			if err := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid); err != nil {
				t.Fatalf("a task of another VM blocked the settlement: %v", err)
			}
			m.requireOnlyMoveSettled(t, false)
			m.requireNothingDeleted(t)
		})
	}
}

// TestMoveSettlementRefusesWhenTaskListUnreadable fails the task listing. The
// settlement refuses, because a listing it can't read proves nothing.
func TestMoveSettlementRefusesWhenTaskListUnreadable(t *testing.T) {
	m := unfiredDetach(t, false)
	m.client.activeMoveTaskErr = errors.New("connection reset by peer")
	m.requireRefused(t, settleAt(pastQuietPeriod), "the active move tasks on node n1 could not be listed")
}

// TestMoveSettlementRefusesWithoutSysAudit has the listing refuse for want of
// Sys.Audit on the node, which hides other users' tasks. The settlement
// refuses with a gap naming the privilege and the path.
func TestMoveSettlementRefusesWithoutSysAudit(t *testing.T) {
	m := unfiredDetach(t, false)
	m.client.activeMoveTaskErr = &pve.AuditVisibilityError{Privilege: "Sys.Audit", Path: "/nodes/n1"}
	m.requireRefused(t, settleAt(pastQuietPeriod), "listing move tasks on node n1 needs Sys.Audit on /nodes/n1")
}

// finalizeCleanup runs the storage-journal finalize-cleanup decision, which
// runs the settlement right after it takes the record's lock.
func (m *unfiredMove) finalizeCleanup(ctx context.Context) error {
	_, err := ApplyStorageAllocationDecision(ctx, m.deps, m.journal, []string{"n1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: m.id, DecisionID: "unfired-move-finalize"})
	return err
}

// TestMoveSettlementRefusesLandedMove lands the task PVE forked for the lost
// POST before the rerun, on every shape. The source no longer names the
// volume, so the settlement refuses and leaves the step for the operator. A
// lifecycle call refuses on the disk's provenance before it reaches the
// settlement in this state, so the row runs the settlement through
// finalize-cleanup, which settles before it resolves anything.
func TestMoveSettlementRefusesLandedMove(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, true)
			key := m.sourceKey(t)
			if outcome := m.client.runLostMove(0); !strings.HasPrefix(outcome, "moved ") {
				t.Fatalf("the forked task did not land: %s", outcome)
			}
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) },
				fmt.Sprintf("no disk key of VM %d names volume %s", m.source, m.step.Target.IntendedVolume))
			if _, still := m.client.state.configs[m.source][key]; still {
				t.Fatalf("the landed task left %d.%s behind", m.source, key)
			}
		})
	}
}

// pendingShapes are the shapes whose source key the pending rows change: the
// detach's unused entry on 777, and the parker slot an attach moves from.
var pendingShapes = []struct {
	name  string
	build func(t *testing.T, lose bool) *unfiredMove
}{
	{"detach", unfiredDetach},
	{"attach-side", unfiredAttachFromParker},
}

// TestMoveSettlementRefusesWhenSourceNoLongerNamesVolume holds a pending
// change on the source key. A pending delete or a pending replacement means
// the source's applied value may not survive, so the settlement refuses. The
// detach shape's source key is an unused entry, for which the fake models no
// pending replacement, so only the attach-side shape's bus slot runs that
// subrow.
func TestMoveSettlementRefusesWhenSourceNoLongerNamesVolume(t *testing.T) {
	for _, shape := range pendingShapes {
		for _, change := range []string{"pending-delete", "pending-replacement"} {
			if shape.name == "detach" && change == "pending-replacement" {
				continue
			}
			t.Run(shape.name+"/"+change, func(t *testing.T) {
				m := shape.build(t, false)
				key := m.sourceKey(t)
				m.client.pending = newFakePendingModel()
				cfg := m.client.state.configs[m.source]
				want := fmt.Sprintf("VM %d has a pending delete of %s", m.source, key)
				if change == "pending-delete" {
					m.client.pending.holdDelete(m.source, cfg, key)
				} else {
					replacement := fmt.Sprintf("a:%d/vm-%d-disk-99.raw", m.source, m.source)
					m.client.state.volumes[replacement] = m.client.state.volumes[m.step.Target.IntendedVolume]
					m.volumes++
					m.client.pending.holdReplacement(m.source, cfg, key, replacement)
					want = fmt.Sprintf("VM %d has a pending replacement of %s", m.source, key)
				}
				m.requireRefused(t, settleAt(pastQuietPeriod), want)
			})
		}
	}
}

// holderRows place a second holder beside the shape's own. On the detach
// shape it is the only other holder, and on the attach-side shape it sits
// beside the tolerated parker slot. Each holder is tried applied and behind
// a pending delete, because a pending delete may never apply.
var holderRows = []string{"applied", "pending-delete"}

// TestMoveSettlementRefusesWhenSerialHeldElsewhere gives VM 888 a slot
// carrying the disk's serial. Another guest holds the disk, so the
// settlement refuses.
func TestMoveSettlementRefusesWhenSerialHeldElsewhere(t *testing.T) {
	for _, shape := range pendingShapes {
		for _, row := range holderRows {
			t.Run(shape.name+"/"+row, func(t *testing.T) {
				m := shape.build(t, false)
				m.client.state.configs[888] = map[string]any{"name": "other", "digest": "1", "scsi2": "a:888/vm-888-disk-9.raw,serial=" + m.token}
				if row == "pending-delete" {
					m.client.pending = newFakePendingModel()
					m.client.pending.holdDelete(888, m.client.state.configs[888], "scsi2")
				}
				m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, "VM 888 holds the disk's serial on scsi2")
			})
		}
	}
}

// TestMoveSettlementRefusesWhenParkerNamesVolume gives a second parker, at
// the first VMID from 90031 up that no guest uses, a slot naming the volume.
// A parker that names it may have taken the disk, so the settlement refuses.
// The last row renumbers the fixture's own parker to 90031 first, the
// collision a random allocation would sometimes make.
func TestMoveSettlementRefusesWhenParkerNamesVolume(t *testing.T) {
	for _, shape := range pendingShapes {
		for _, row := range holderRows {
			t.Run(shape.name+"/"+row, func(t *testing.T) {
				requireParkerNamingVolumeRefused(t, shape.build(t, false), row)
			})
		}
	}
	t.Run("real-parker-at-90031", func(t *testing.T) {
		m := unfiredDetach(t, false)
		moveRealParkerTo(t, m, 90031)
		requireParkerNamingVolumeRefused(t, m, "applied")
	})
}

// moveRealParkerTo renumbers the fixture's only parker to vmid, the way a
// random allocation could have placed it.
func moveRealParkerTo(t *testing.T, m *unfiredMove, vmid int) {
	t.Helper()
	parker := 0
	for id := range m.client.state.configs {
		if isParkerVM(m.client, id) {
			if parker != 0 {
				t.Fatalf("want one parker, got %d and %d", parker, id)
			}
			parker = id
		}
	}
	if parker == 0 {
		t.Fatal("the fixture has no parker")
	}
	if parker == vmid {
		return
	}
	if _, taken := m.client.state.configs[vmid]; taken {
		t.Fatalf("VM %d is already a guest", vmid)
	}
	m.client.state.configs[vmid] = m.client.state.configs[parker]
	delete(m.client.state.configs, parker)
	if node, ok := m.client.vmNodes[parker]; ok {
		m.client.vmNodes[vmid] = node
		delete(m.client.vmNodes, parker)
	}
}

// requireParkerNamingVolumeRefused gives an injected parker a slot naming the
// step's volume, applied or behind a pending delete, and requires the
// settlement to refuse on it. The injected parker takes the first VMID from
// 90031 up that no guest uses, because the fixture's own parker lands on a
// random VMID in the same range.
func requireParkerNamingVolumeRefused(t *testing.T, m *unfiredMove, row string) {
	t.Helper()
	volume := m.step.Target.IntendedVolume
	injected := 90031
	for {
		if _, taken := m.client.state.configs[injected]; !taken {
			break
		}
		injected++
	}
	m.client.state.configs[injected] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", injected), "tags": "bosh-cpi;" + pve.ParkerTag, "digest": "1", "scsi3": volume}
	if row == "pending-delete" {
		m.client.pending = newFakePendingModel()
		m.client.pending.holdDelete(injected, m.client.state.configs[injected], "scsi3")
	}
	m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, fmt.Sprintf("parker %d names volume %s on scsi3", injected, volume))
	m.requireNothingDeleted(t)
}

// TestMoveSettlementReadsBothNodesAfterMigration moves the source to n2 after
// the step named n1. The settlement lists the active tasks on both nodes, and
// a task on either one refuses.
func TestMoveSettlementReadsBothNodesAfterMigration(t *testing.T) {
	for _, node := range []string{"n1", "n2", ""} {
		name := "task-on-" + node
		if node == "" {
			name = "no-task"
		}
		t.Run(name, func(t *testing.T) {
			m := unfiredDetach(t, false)
			if m.step.Target.Node != "n1" {
				t.Fatalf("the step names node %s, want n1", m.step.Target.Node)
			}
			m.client.vmNodes[777] = "n2"
			if node != "" {
				task := activeTask(node, "777-"+m.sourceKey(t)+">90777-scsi0")
				m.client.activeMoveTasks = map[string][]pve.ActiveTask{node: {task}}
				m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, "move task "+task.UPID+" is still active on node "+node)
				m.requireNothingDeleted(t)
				return
			}
			if err := m.finalizeCleanup(settleAt(pastQuietPeriod)); err == nil {
				t.Fatal("finalize-cleanup finalized a disk that still exists")
			}
			if !reflect.DeepEqual(m.client.moveTaskListings, []string{"n1", "n2"}) {
				t.Fatalf("want the tasks of n1 and n2 listed, got %v", m.client.moveTaskListings)
			}
			m.requireOnlyMoveSettled(t, true)
			m.requireNothingDeleted(t)
		})
	}
}

// TestMoveSettlementLeavesSubmittedAndClosedStepsAlone checks that only a
// planned, UPID-less lifecycle move step of a disk record's active attempt,
// with no recorded parameters, no external target, and a full target, is
// ever judged. It then closes the active attempt with a completion audit and
// checks that the settlement judges none of its steps. Last, it has PVE
// refuse a move inside its task on the digest check while the inline
// readback fails, which leaves the step submitted with its UPID, and checks
// that the rerun leaves that step to the operator.
func TestMoveSettlementLeavesSubmittedAndClosedStepsAlone(t *testing.T) {
	m := unfiredDetach(t, false)
	if !isUnfiredMoveCandidate(m.before, m.step) {
		t.Fatalf("the unfired step is not a candidate: %+v", m.step)
	}
	for name, change := range map[string]func(r *aj.Record, s *aj.Step){
		"submitted":          func(_ *aj.Record, s *aj.Step) { s.State = aj.Submitted },
		"observed":           func(_ *aj.Record, s *aj.Step) { s.State = aj.Observed },
		"upid":               func(_ *aj.Record, s *aj.Step) { s.UPID = "UPID:n1:0004D2A1:03504636:6AA1786A:qmmove:777:bosh@pve!cpi:" },
		"earlier-attempt":    func(_ *aj.Record, s *aj.Step) { s.Attempt-- },
		"external":           func(_ *aj.Record, s *aj.Step) { s.Target.External = true },
		"vm-record":          func(r *aj.Record, _ *aj.Step) { r.Kind = "vm" },
		"parameters":         func(_ *aj.Record, s *aj.Step) { s.Parameters = json.RawMessage(`{"disk":"scsi1"}`) },
		"no-node":            func(_ *aj.Record, s *aj.Step) { s.Target.Node = "" },
		"no-source":          func(_ *aj.Record, s *aj.Step) { s.Target.VMID = 0 },
		"no-volume":          func(_ *aj.Record, s *aj.Step) { s.Target.IntendedVolume = "" },
		"park-kind":          func(_ *aj.Record, s *aj.Step) { s.Kind = "park_detach_disk_Nodes_CreateQemuMoveDisk" },
		"config-update-kind": func(_ *aj.Record, s *aj.Step) { s.Kind = "lifecycle_detach_disk_Nodes_UpdateQemuConfig" },
	} {
		record, step := m.before, m.step
		change(&record, &step)
		if isUnfiredMoveCandidate(record, step) {
			t.Errorf("%s: the step is judged as unfired", name)
		}
	}

	t.Run("closed-attempt", func(t *testing.T) {
		m := unfiredDetach(t, false)
		record := m.before
		audit := aj.AttemptVerification{
			Verification:  aj.Verification{EvidenceID: "closed-attempt-audit", Complete: true, OwnershipVerified: true, AbsenceVerified: true, ArtifactDispositionVerified: true},
			OutcomesKnown: true,
		}
		if len(record.Attempts) == 0 {
			t.Fatal("the record has no explicit attempt to close")
		}
		record.Attempts = append([]aj.Attempt{}, record.Attempts...)
		record.Attempts[len(record.Attempts)-1].Completion = &audit
		record.Verifications = append(append([]aj.Verification{}, record.Verifications...), audit.Verification)
		m.rewriteRecord(t, record)
		ctx := settleAt(pastQuietPeriod)
		handle, err := m.journal.Acquire(ctx, m.id)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = handle.Close() }()
		reasons, err := settlePlannedMoveSteps(ctx, m.client, handle, nil, nil)
		if err != nil || len(reasons) != 0 {
			t.Fatalf("the settlement judged a closed attempt: reasons %v, err %v", reasons, err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		m.requireRecordUnchanged(t)
		if len(m.client.moveTaskListings) != 0 {
			t.Fatalf("the settlement listed tasks for a closed attempt: %v", m.client.moveTaskListings)
		}
	})

	t.Run("task-digest-refusal-with-failed-readback", func(t *testing.T) {
		f := digestManagedFixture(t)
		f.client.moveTaskRefusal = true
		f.client.onConfigRead = func(int) error {
			if f.client.moveCalls > 0 {
				return errors.New("config read: connection reset by peer")
			}
			return nil
		}
		if err := digestDetach(t, f.deps, f.cid); err == nil {
			t.Fatal("the detach succeeded although its task refused")
		}
		f.client.onConfigRead = nil
		before, err := f.journal.Inspect(f.id)
		if err != nil {
			t.Fatal(err)
		}
		var submitted aj.Step
		for _, step := range before.Steps {
			if step.Attempt == before.ActiveAttempt() && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") && step.State == aj.Submitted && step.UPID != "" {
				submitted = step
			}
		}
		if submitted.ID == "" {
			t.Fatalf("want a submitted move step with its UPID, got %+v", before.Steps)
		}
		err = detachDiskAt(t, settleAt(pastQuietPeriod), f.deps, "777", f.cid)
		if err == nil || strings.Contains(err.Error(), "could not be settled as never started") {
			t.Fatalf("want the submitted step left to the operator untouched by the move settlement, got %v", err)
		}
		if after, _ := f.journal.Inspect(f.id); !reflect.DeepEqual(after, before) {
			t.Fatalf("the rerun changed the record:\nbefore %+v\nafter  %+v", before, after)
		}
		if len(f.client.moveTaskListings) != 0 {
			t.Fatalf("the settlement judged the submitted step: listed %v", f.client.moveTaskListings)
		}
	})
}

// TestMoveSettlementListsTasksBeforeTheProofs pins the read order. The task
// PVE forked for the lost POST is absent from the active list when the
// settlement lists it, and it lands right after that listing, before the
// proofs read. Reading the listing first makes the proofs see the landing and
// refuse. With the proofs read first, both reads would pass and the step
// would be settled as never started for a move that happened.
func TestMoveSettlementListsTasksBeforeTheProofs(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, true)
			landed := ""
			m.client.onActiveMoveTasks = func(string) {
				if landed == "" {
					landed = m.client.runLostMove(0)
				}
			}
			m.requireRefusedBy(t, func() error { return detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid) },
				fmt.Sprintf("no disk key of VM %d names volume %s", m.source, m.step.Target.IntendedVolume))
			if !strings.HasPrefix(landed, "moved ") {
				t.Fatalf("the forked task did not land after the listing: %q", landed)
			}
		})
	}
}
