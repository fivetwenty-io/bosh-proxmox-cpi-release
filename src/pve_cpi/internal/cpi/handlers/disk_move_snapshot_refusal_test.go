package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// snapshotRefusalFixture is the journal-managed disk of digestManagedFixture
// with a snapshot of VM 777 naming its volume, and with the detach allowed to
// go ahead despite the snapshot, so its move reaches PVE.
func snapshotRefusalFixture(t *testing.T) digestManaged {
	t.Helper()
	f := digestManagedFixture(t)
	slot := ""
	for key, value := range f.client.state.configs[777] {
		if text, ok := value.(string); ok && strings.HasPrefix(text, f.volume+",") {
			slot = key
		}
	}
	if slot == "" {
		t.Fatalf("VM 777 holds %s on no slot", f.volume)
	}
	f.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "keep"}}}
	f.client.snapshotConfigs = map[int]map[string]map[string]any{777: {"keep": {slot: f.volume + ",size=5G"}}}
	f.deps.Config.AllowDiskOpsWithSnapshots = true
	return f
}

// assertMovePoisoned requires that a refused move marked the record for
// reconciliation at the move itself and left the volume in place.
func assertMovePoisoned(t *testing.T, f digestManaged, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("detach succeeded although its move was refused")
	}
	record, inspectErr := f.journal.Inspect(f.id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if record.State != aj.ReconciliationRequired || !strings.Contains(record.Reason, "Nodes.CreateQemuMoveDisk") {
		t.Fatalf("the refused move was settled: err=%v state=%s reason=%q", err, record.State, record.Reason)
	}
	if f.client.state.volumes[f.volume] == nil {
		t.Fatalf("volume %s is gone", f.volume)
	}
}

// TestManagedMoveSnapshotRefusalSettlesStep has PVE refuse a managed detach's
// move in the request because a snapshot still names the volume. Every move
// step closes as refused, in the Observed state with only the pre-move volume
// and no UPID. The guard stays usable, and the refusal reaches the detach with
// PVE's text, so the detach defers the park. The volume stays on 777's unused
// entry.
func TestManagedMoveSnapshotRefusalSettlesStep(t *testing.T) {
	f := snapshotRefusalFixture(t)
	client, journal := f.client, f.journal
	client.moveSnapshotRefusal = true
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach under the snapshot refusal did not defer the park: %v", err)
	}
	if client.moveCalls == 0 {
		t.Fatal("the detach sent no move")
	}
	record, err := journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || record.Reason != "" {
		t.Fatalf("the refused move left the record %s: %q", record.State, record.Reason)
	}
	moves := 0
	for _, step := range record.Steps {
		if step.Attempt != record.ActiveAttempt() || !strings.HasSuffix(step.Kind, "detach_disk_Nodes_CreateQemuMoveDisk") {
			continue
		}
		moves++
		if step.State != aj.Observed || step.UPID != "" || len(step.VolIDs) != 1 || step.VolIDs[0] != f.volume {
			t.Fatalf("a refused move did not settle with the pre-move volume: %+v", step)
		}
	}
	if moves != client.moveCalls {
		t.Fatalf("%d move POSTs but %d settled move steps", client.moveCalls, moves)
	}
	if unusedHolding(client, f.volume) == "" || client.state.volumes[f.volume] == nil {
		t.Fatal("the refused move did not leave the volume on 777's unused entry")
	}
	// The deferral takes the attached CID off 777, and the detach reaches it
	// only when the refusal it gets back still carries PVE's text.
	if description, _ := client.state.configs[777]["description"].(string); strings.Contains(description, "bosh_attached_disks") {
		t.Fatalf("the detach did not defer the park: %s", description)
	}
}

// TestManagedMoveSnapshotLookalikesPoison is a guard row. Each error carries
// wording close to PVE's snapshot refusal but isn't PVE's answer with its
// whole sentence, so none of them settles and each poisons the guard.
func TestManagedMoveSnapshotLookalikesPoison(t *testing.T) {
	answer := func(code int, message string) error {
		return sdkerrors.ParseAPIError(code, []byte(fmt.Sprintf(`{"message":%q}`, message)))
	}
	for name, moveErr := range map[string]error{
		"other snapshot answer":      answer(500, "you can't move a disk with snapshots and delete the source\n"),
		"sentence without an answer": errors.New("Can't move disk used by a snapshot to another VM"),
		"sentence through a gateway": answer(502, "Can't move disk used by a snapshot to another VM\n"),
		"lowercase sentence":         answer(500, "can't move disk used by a snapshot to another vm\n"),
	} {
		t.Run(name, func(t *testing.T) {
			f := snapshotRefusalFixture(t)
			f.client.moveErr = moveErr
			assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
		})
	}
}

// snapshotRefusalSentence is the whole message PVE's reassign check dies with
// when anything still names the volume, without its newline.
const snapshotRefusalSentence = "Can't move disk used by a snapshot to another VM"

// detachMoveSteps returns the move steps of the record's active attempt.
func detachMoveSteps(record aj.Record) []aj.Step {
	var moves []aj.Step
	for i := range record.Steps {
		if step := record.Steps[i]; step.Attempt == record.ActiveAttempt() && strings.HasSuffix(step.Kind, "detach_disk_Nodes_CreateQemuMoveDisk") {
			moves = append(moves, step)
		}
	}
	return moves
}

// TestManagedMoveSnapshotRefusalSendsOnePOST has PVE refuse a managed
// detach's move in the request because a snapshot still names the volume. The
// move's retry loop stops at the refusal, so the detach sends one move POST
// and the journal holds one move step, which closes as refused in the Observed
// state.
func TestManagedMoveSnapshotRefusalSendsOnePOST(t *testing.T) {
	f := snapshotRefusalFixture(t)
	f.client.moveSnapshotRefusal = true
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach under the snapshot refusal did not defer the park: %v", err)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	moves := detachMoveSteps(record)
	if f.client.moveCalls != 1 || len(moves) != 1 {
		t.Fatalf("want one move POST and one move step, got %d POSTs and %d steps", f.client.moveCalls, len(moves))
	}
	if moves[0].State != aj.Observed {
		t.Fatalf("refused move step state = %s, want %s", moves[0].State, aj.Observed)
	}
}

// TestManagedMoveTaskSnapshotRefusalSettlesStep has PVE accept a managed
// detach's move and the task exit with the snapshot refusal, because the
// task's own check under the configuration locks found the volume still in
// use. The move step closes as refused, in the Observed state with only the
// pre-move volume and the UPID the guard recorded, and the detach sends no
// second POST.
func TestManagedMoveTaskSnapshotRefusalSettlesStep(t *testing.T) {
	f := snapshotRefusalFixture(t)
	f.client.moveTaskExit = snapshotRefusalSentence
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach under the task's snapshot refusal: %v", err)
	}
	upid := ""
	for task, exit := range f.client.failedTasks {
		if exit == snapshotRefusalSentence {
			upid = task
		}
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	moves := detachMoveSteps(record)
	if f.client.moveCalls != 1 || len(moves) != 1 {
		t.Fatalf("want one move POST and one move step, got %d POSTs and %d steps", f.client.moveCalls, len(moves))
	}
	step := moves[0]
	if step.State != aj.Observed || upid == "" || step.UPID != upid || len(step.VolIDs) != 1 || step.VolIDs[0] != f.volume {
		t.Fatalf("the refused task did not settle with its UPID %q and the pre-move volume: %+v", upid, step)
	}
}

// TestManagedOwnedDetachDefersOnTaskSnapshotRefusal detaches a disk named for
// its VM while a snapshot names it, and the refusal comes back as the move
// task's exit status. The detach defers the park, the record ends ready to
// return, and the volume stays on 777's unused entry.
func TestManagedOwnedDetachDefersOnTaskSnapshotRefusal(t *testing.T) {
	f := snapshotRefusalFixture(t)
	if !fakeVolumeOwnedBy(f.volume, 777) {
		t.Fatalf("%s isn't named for 777", f.volume)
	}
	f.client.moveTaskExit = snapshotRefusalSentence
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach under the task's snapshot refusal did not defer the park: %v", err)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || record.Reason != "" {
		t.Fatalf("the refused task left the record %s: %q", record.State, record.Reason)
	}
	if unusedHolding(f.client, f.volume) == "" || f.client.state.volumes[f.volume] == nil {
		t.Fatal("the refused task did not leave the volume on 777's unused entry")
	}
	if description, _ := f.client.state.configs[777]["description"].(string); strings.Contains(description, "bosh_attached_disks") {
		t.Fatalf("the detach did not defer the park: %s", description)
	}
}

// TestManagedMoveTaskOtherFailurePoisons is a guard row. The move task exits
// with a status that isn't PVE's snapshot refusal word for word, so the step
// stays uncertain and the guard is poisoned.
func TestManagedMoveTaskOtherFailurePoisons(t *testing.T) {
	for name, exit := range map[string]string{
		"unrelated failure": "storage migration failed: unable to rename volume",
		"longer status":     snapshotRefusalSentence + " (snapshot 'keep')",
		"lowercase status":  strings.ToLower(snapshotRefusalSentence),
	} {
		t.Run(name, func(t *testing.T) {
			f := snapshotRefusalFixture(t)
			f.client.moveTaskExit = exit
			assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
		})
	}
}

// TestManagedMoveTaskUnreadableStatusPoisons is a guard row. PVE accepts the
// move, and no read of its task's status gets an answer, so nothing shows how
// the task ended and the guard is poisoned.
func TestManagedMoveTaskUnreadableStatusPoisons(t *testing.T) {
	f := snapshotRefusalFixture(t)
	f.client.moveTaskUnreadable = true
	assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
}

// unmanagedDetachFixture is a disk named for VM 700 on its scsi1, with a
// parker and no allocation journal, so its detach moves the disk without a
// guard.
func unmanagedDetachFixture(t *testing.T) (*idFakeClient, Deps, func() resolvedDisk) {
	t.Helper()
	const birth = "data:vm-9001-disk-0"
	c := newIDFakeClient(map[int]map[string]any{
		700:   {"scsi1": "data:vm-700-disk-1,serial=" + idTestToken + ",size=10G"},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	deps := idTestDeps(c)
	diskCID := overlayCID(t, birth, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	resolve := func() resolvedDisk {
		t.Helper()
		bare, meta, err := decodeDiskCID(digestCtx(), deps, "detach_disk", diskCID)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		rd, err := resolveDiskForOp(digestCtx(), deps, "detach_disk", diskCID, bare, meta)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return rd
	}
	return c, deps, resolve
}

// TestUnmanagedDetachSnapshotRefusalSendsOnePOST has PVE refuse every move of
// an unmanaged detach with its 500 for a volume a snapshot still names. The
// move's retry loop is the same one the managed detach uses, so it stops at
// the first refusal and the detach defers the park after one move POST.
func TestUnmanagedDetachSnapshotRefusalSendsOnePOST(t *testing.T) {
	c, deps, resolve := unmanagedDetachFixture(t)
	c.moveErr, c.keepMoveErr = moveSnapshotRefusal(), true
	if err := handleDetachStableID(digestCtx(), deps, "700", 700, resolve()); err != nil {
		t.Fatalf("detach under the snapshot refusal did not defer the park: %v", err)
	}
	if c.moveCalls != 1 {
		t.Fatalf("want one move POST, got %d", c.moveCalls)
	}
	if got, _ := c.configs[700]["unused0"].(string); got != "data:vm-700-disk-1" {
		t.Fatalf("unused0 = %q, want the demoted volume", got)
	}
}

// TestUnmanagedDetachOtherServerErrorRetries is a guard row. PVE answers every
// move of an unmanaged detach with a 500 whose text isn't a snapshot refusal,
// so the move's retry loop retries it as it always has, four POSTs in all.
func TestUnmanagedDetachOtherServerErrorRetries(t *testing.T) {
	c, deps, resolve := unmanagedDetachFixture(t)
	c.moveErr = sdkerrors.ParseAPIError(500, []byte(`{"message":"storage migration failed: unable to rename volume\n"}`))
	c.keepMoveErr = true
	if err := handleDetachStableID(digestCtx(), deps, "700", 700, resolve()); err == nil {
		t.Fatal("detach succeeded although every move failed")
	}
	if c.moveCalls != 4 {
		t.Fatalf("want the move retried to four POSTs, got %d", c.moveCalls)
	}
}

// TestManagedMoveSnapshotRefusalWithPendingAddOnSlotPoisons is a guard row.
// PVE refuses the move for a snapshot, and by the readback the parker's
// receiving slot holds a pending add. The readback can't show that nothing
// moved, so the refusal doesn't settle and the guard is poisoned.
func TestManagedMoveSnapshotRefusalWithPendingAddOnSlotPoisons(t *testing.T) {
	f := snapshotRefusalFixture(t)
	client := f.client
	client.pending = newFakePendingModel()
	client.moveSnapshotRefusal = true
	client.beforeMoveCheck = func(_ int, p *nodes.CreateQemuMoveDiskParams) {
		target := int(*p.TargetVmid)
		client.pending.holdReplacement(target, client.state.configs[target], *p.TargetDisk, fmt.Sprintf("a:%d/vm-%d-disk-9.raw", target, target))
	}
	assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
	if client.snapshotAnswers != 1 {
		t.Fatalf("want the move refused once for the snapshot, got %d", client.snapshotAnswers)
	}
}

// TestManagedMoveTaskSnapshotRefusalWithPendingAddOnSlotPoisons is a guard
// row. The move task exits with PVE's snapshot refusal, and by the readback
// the parker's receiving slot holds a pending add. The readback can't show
// that nothing moved, so the task's refusal doesn't settle and the guard is
// poisoned.
func TestManagedMoveTaskSnapshotRefusalWithPendingAddOnSlotPoisons(t *testing.T) {
	f := snapshotRefusalFixture(t)
	client := f.client
	client.pending = newFakePendingModel()
	client.moveTaskExit = snapshotRefusalSentence
	client.beforeMoveCheck = func(_ int, p *nodes.CreateQemuMoveDiskParams) {
		target := int(*p.TargetVmid)
		client.pending.holdReplacement(target, client.state.configs[target], *p.TargetDisk, fmt.Sprintf("a:%d/vm-%d-disk-9.raw", target, target))
	}
	assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
	exits := 0
	for _, exit := range client.failedTasks {
		if exit == snapshotRefusalSentence {
			exits++
		}
	}
	if exits != 1 {
		t.Fatalf("want one move task to exit with the snapshot refusal, got %d", exits)
	}
}

// TestManagedMoveSnapshotRefusalWithPendingSourceDeletePoisons is a guard row.
// PVE refuses the move for a snapshot, and by the readback the source key
// that named the volume has a pending delete. The source no longer shows the
// volume where it was, so the refusal doesn't settle and the guard is
// poisoned.
func TestManagedMoveSnapshotRefusalWithPendingSourceDeletePoisons(t *testing.T) {
	f := snapshotRefusalFixture(t)
	client := f.client
	client.pending = newFakePendingModel()
	client.moveSnapshotRefusal = true
	client.beforeMoveCheck = func(_ int, p *nodes.CreateQemuMoveDiskParams) {
		client.pending.holdDelete(777, client.state.configs[777], p.Disk)
	}
	assertMovePoisoned(t, f, digestDetach(t, f.deps, f.cid))
	if client.snapshotAnswers != 1 {
		t.Fatalf("want the move refused once for the snapshot, got %d", client.snapshotAnswers)
	}
}

// TestManagedMoveSnapshotRefusalAfterCancelPoisons is a guard row. The
// request's context ends while the move is in flight, and PVE's answer is
// still the snapshot refusal. An ended context proves nothing about the
// answer, so the refusal doesn't settle and the guard is poisoned.
func TestManagedMoveSnapshotRefusalAfterCancelPoisons(t *testing.T) {
	f := snapshotRefusalFixture(t)
	client := f.client
	client.moveSnapshotRefusal = true
	ctx, cancel := context.WithCancel(digestCtx())
	defer cancel()
	client.beforeMoveCheck = func(int, *nodes.CreateQemuMoveDiskParams) { cancel() }
	_, err := HandleDetachDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, "777"), planJSON(t, f.cid)}, jsonrpc.Context{})
	assertMovePoisoned(t, f, err)
	if client.snapshotAnswers != 1 {
		t.Fatalf("want the move refused once for the snapshot, got %d", client.snapshotAnswers)
	}
}

// TestManagedAttachSnapshotRefusalOffParkerPoisons is a guard row. A managed
// disk parks by config edit under a name its parker doesn't own, so the next
// attach brings it back with move_disk, and PVE refuses that move because a
// snapshot still names the volume. The snapshot settlement covers only a
// move onto a parker, so this refusal poisons the guard as it always has. The
// attach edits no configuration onto 777 and destroys nothing, and the volume
// stays on the parker.
func TestManagedAttachSnapshotRefusalOffParkerPoisons(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	value, _ := pve.ConfigString(client.state.configs[777], "scsi1")
	volume := strings.Split(value, ",")[0]
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("park the disk by config edit: %v", err)
	}
	parker := 0
	for vmid, cfg := range client.state.configs {
		for key, held := range cfg {
			if text, ok := held.(string); ok && vmid != 777 && strings.HasPrefix(text, volume+",") && !strings.HasPrefix(key, "unused") {
				parker = vmid
			}
		}
	}
	if parker == 0 || fakeVolumeOwnedBy(volume, parker) {
		t.Fatalf("want %s parked by config edit under a name its parker doesn't own, parker %d", volume, parker)
	}
	vms := len(client.state.configs)
	client.moveCalls = 0
	client.moveSnapshotRefusal = true
	err := digestAttach(t, deps, cid)
	if err == nil {
		t.Fatal("attach succeeded although its move off the parker was refused")
	}
	if client.moveCalls == 0 || client.snapshotAnswers == 0 {
		t.Fatalf("the attach never reached the refused move: %d POSTs, %d refusals", client.moveCalls, client.snapshotAnswers)
	}
	record, inspectErr := journal.Inspect(id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if record.State != aj.ReconciliationRequired || !strings.Contains(record.Reason, "Nodes.CreateQemuMoveDisk") {
		t.Fatalf("the refused move off the parker was settled: err=%v state=%s reason=%q", err, record.State, record.Reason)
	}
	for key, held := range client.state.configs[777] {
		if text, ok := held.(string); ok && strings.Contains(text, volume) {
			t.Fatalf("the attach wrote %s onto 777's %s: %q", volume, key, text)
		}
	}
	if len(client.state.configs) != vms || client.state.configs[parker] == nil || client.state.volumes[volume] == nil {
		t.Fatalf("the attach destroyed a VM or the volume: %d VMs, want %d, parker %d present %t", len(client.state.configs), vms, parker, client.state.configs[parker] != nil)
	}
	held := false
	for _, text := range client.state.configs[parker] {
		if s, ok := text.(string); ok && strings.HasPrefix(s, volume+",") {
			held = true
		}
	}
	if !held {
		t.Fatalf("the volume left parker %d: %v", parker, client.state.configs[parker])
	}
}
