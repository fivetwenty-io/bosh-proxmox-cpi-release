package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// digestLegacyFixture is the flow fixture with its journal configuration
// removed and VM 777 holding a legacy stable-ID disk on scsi1, parked on
// detach, under a name 777 owns. See digestOwnedByVM.
func digestLegacyFixture(t *testing.T) (Deps, *lifecycleFlowPVE, string, string) {
	t.Helper()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	delete(client.state.volumes, old)
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	volume := "a:123/vm-123-disk-0.raw"
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
	return deps, client, cid, digestOwnedByVM(t, deps, client, cid)
}

// digestManaged is the flow fixture's journal-managed disk, with its
// allocation ID, its CID, and the volume VM 777 holds it under.
type digestManaged struct {
	deps    Deps
	client  *lifecycleFlowPVE
	journal *aj.Journal
	id      string
	cid     string
	volume  string
}

// digestManagedFixture is the flow fixture's journal-managed disk on VM 777,
// parked on detach, under a name 777 owns. See digestOwnedByVM.
func digestManagedFixture(t *testing.T) digestManaged {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	volume := digestOwnedByVM(t, deps, client, cid)
	return digestManaged{deps: deps, client: client, journal: journal, id: id, cid: cid, volume: volume}
}

// digestOwnedByVM gives VM 777 the disk under a name 777 owns, which is the
// only shape a detach moves with move_disk. A volume 777 doesn't own leaves
// no unused entry when its slot is deleted, so the detach parks it by config
// edit. The attach that follows brings it back with move_disk, which renames
// it for 777, the way any disk a parker hands back is named. The move
// counters start again from zero, and it returns the volume's new name.
func digestOwnedByVM(t *testing.T, deps Deps, client *lifecycleFlowPVE, cid string) string {
	t.Helper()
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("park the disk by config edit: %v", err)
	}
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("move the disk back to 777: %v", err)
	}
	if client.moveCalls != 1 {
		t.Fatalf("want one move POST while 777 took ownership, got %d", client.moveCalls)
	}
	client.moveCalls, client.moveDigests = 0, nil
	value, _ := pve.ConfigString(client.state.configs[777], "scsi1")
	volume := strings.Split(value, ",")[0]
	if !fakeVolumeOwnedBy(volume, 777) {
		t.Fatalf("777's scsi1 holds %q, want a volume named for 777", volume)
	}
	return volume
}

// digestCtx takes the backoff out of the move's retry loop, so a row whose
// move POST loses its response retries at once.
func digestCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

func digestDetach(t *testing.T, deps Deps, cid string) error {
	t.Helper()
	_, err := HandleDetachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	return err
}

func digestAttach(t *testing.T, deps Deps, cid string) error {
	t.Helper()
	_, err := HandleAttachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	return err
}

// changeConfig models another writer changing VM 777's configuration, which
// gives it a new digest.
func changeConfig(client *lifecycleFlowPVE) {
	client.generation++
	client.state.configs[777]["digest"] = fmt.Sprint(client.generation + 100)
}

// unusedHolding returns the unused key of VM 777 that names volume.
func unusedHolding(client *lifecycleFlowPVE, volume string) string {
	for key, value := range pve.FindUnusedDiskEntries(client.state.configs[777]) {
		if value == volume {
			return key
		}
	}
	return ""
}

// moveStep returns the move step of the record's active attempt and fails
// when that attempt still has a planned or submitted step.
func moveStep(t *testing.T, journal *aj.Journal, id string) aj.Step {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	var move aj.Step
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && (step.State == aj.Planned || step.State == aj.Submitted) {
			t.Fatalf("record %s still has an unsettled step: %s (%s) is %s", record.State, step.ID, step.Kind, step.State)
		}
		if step.Attempt == record.ActiveAttempt() && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
			move = *step
		}
	}
	if move.ID == "" {
		t.Fatalf("record has no move step: %+v", record.Steps)
	}
	return move
}

// assertMoveNotPoisoned requires that a refused move left the guard usable.
// The guard's failure path marks the record for reconciliation at the move
// itself and says so in the request's error, and a settled refusal does
// neither: the record's reason names only the operation that did not
// complete.
func assertMoveNotPoisoned(t *testing.T, journal *aj.Journal, id string, err error) {
	t.Helper()
	const poisoned = "Nodes.CreateQemuMoveDisk"
	if strings.Contains(err.Error(), "reconciliation at lifecycle detach_disk "+poisoned) || strings.Contains(err.Error(), "managed lifecycle task outcome requires reconciliation") {
		t.Fatalf("the refused move poisoned the guard: %v", err)
	}
	record, inspectErr := journal.Inspect(id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if strings.Contains(record.Reason, poisoned) {
		t.Fatalf("the record was marked for reconciliation at the move: %q", record.Reason)
	}
}

// TestLegacyLateMoveRefusedAfterSourceChange loses the response to a legacy
// detach's move POST. PVE forked the task anyway, and the task runs only
// after another writer has changed the source VM's configuration. The digest
// the POST carried makes the task refuse instead of moving the volume.
func TestLegacyLateMoveRefusedAfterSourceChange(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	client.loseMoveResponse = true
	if err := digestDetach(t, deps, cid); err == nil {
		t.Fatal("detach succeeded although its move response was lost")
	}
	client.loseMoveResponse = false
	if len(client.lostMoves) != 1 || unusedHolding(client, volume) == "" {
		t.Fatalf("the lost move did not leave the volume on 777's unused entry: lost=%d cfg=%v", len(client.lostMoves), client.state.configs[777])
	}
	changeConfig(client)
	outcome := client.runLostMove(0)
	if !strings.HasPrefix(outcome, "refused: VM 777: detected modified configuration") {
		t.Fatalf("the late task was not refused by its digest: %s", outcome)
	}
	if unusedHolding(client, volume) == "" || client.state.volumes[volume] == nil {
		t.Fatal("the refused late task moved the volume")
	}
}

// TestLegacyTransferOutLateMoveRefusedAfterSourceChange loses the response to
// the move that takes a parked legacy disk back to its VM. The transfer puts
// the parker's protection back after the failed move, which changes the
// parker's configuration, so the late task finds its source digest stale.
func TestLegacyTransferOutLateMoveRefusedAfterSourceChange(t *testing.T) {
	deps, client, cid, _ := digestLegacyFixture(t)
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach: %v", err)
	}
	client.loseMoveResponse = true
	if err := digestAttach(t, deps, cid); err == nil {
		t.Fatal("attach succeeded although its move response was lost")
	}
	client.loseMoveResponse = false
	if len(client.lostMoves) != 1 {
		t.Fatalf("want one lost move, got %d", len(client.lostMoves))
	}
	parker := client.lostMoves[0].source
	outcome := client.runLostMove(0)
	if !strings.HasPrefix(outcome, fmt.Sprintf("refused: VM %d: detected modified configuration", parker)) {
		t.Fatalf("the late task was not refused by its digest: %s", outcome)
	}
	if _, held := client.state.configs[parker][client.lostMoves[0].disk]; !held {
		t.Fatal("the refused late task moved the volume off the parker")
	}
}

// TestLegacyMoveDigestRefusalIsCleanAndNotRetried has another writer change
// the source VM between the CPI's read and PVE's check. PVE refuses in the
// request, the CPI sends no second POST, and the refusal says the volume
// stayed where it was.
func TestLegacyMoveDigestRefusalIsCleanAndNotRetried(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	client.changeSourceBeforeMove = true
	err := digestDetach(t, deps, cid)
	if err == nil || !errors.Is(err, pve.ErrMoveDiskDigestRefused) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable digest refusal, got %v", err)
	}
	if client.moveCalls != 1 {
		t.Fatalf("the refused move was posted %d times, want once", client.moveCalls)
	}
	if unusedHolding(client, volume) == "" || client.state.volumes[volume] == nil {
		t.Fatal("the refused move did not leave the volume on 777's unused entry")
	}
}

// TestManagedMoveDigestRefusalSettlesStep refuses a managed detach's move in
// the request. The move step settles as observed with the pre-move volume,
// the guard is not poisoned, and the rerun parks the disk.
func TestManagedMoveDigestRefusalSettlesStep(t *testing.T) {
	f := digestManagedFixture(t)
	deps, client, journal, id, cid, volume := f.deps, f.client, f.journal, f.id, f.cid, f.volume
	client.changeSourceBeforeMove = true
	err := digestDetach(t, deps, cid)
	if err == nil || !errors.Is(err, pve.ErrMoveDiskDigestRefused) {
		t.Fatalf("want the digest refusal, got %v", err)
	}
	assertMoveNotPoisoned(t, journal, id, err)
	move := moveStep(t, journal, id)
	if move.State != aj.Observed || move.UPID != "" || !containsString(move.VolIDs, volume) {
		t.Fatalf("the refused move did not settle with the pre-move volume: %+v", move)
	}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("rerun detach after the refusal: %v", err)
	}
	if record, _ := journal.Inspect(id); record.State != aj.ReadyToReturn {
		t.Fatalf("rerun left the record %s", record.State)
	}
}

// TestManagedMoveTaskDigestRefusalSettlesStep has PVE accept a managed move,
// return its UPID, and then refuse inside the task on the digest check. The
// step goes from submitted to observed with the pre-move volume, the guard is
// not poisoned, and the record is left with no planned or submitted step.
func TestManagedMoveTaskDigestRefusalSettlesStep(t *testing.T) {
	f := digestManagedFixture(t)
	deps, client, journal, id, cid, volume := f.deps, f.client, f.journal, f.id, f.cid, f.volume
	client.moveTaskRefusal = true
	err := digestDetach(t, deps, cid)
	if err == nil || !errors.Is(err, pve.ErrMoveDiskDigestRefused) {
		t.Fatalf("want the digest refusal, got %v", err)
	}
	assertMoveNotPoisoned(t, journal, id, err)
	move := moveStep(t, journal, id)
	if move.State != aj.Observed || move.UPID == "" || !containsString(move.VolIDs, volume) {
		t.Fatalf("the refused task did not settle with its UPID and the pre-move volume: %+v", move)
	}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("rerun detach after the refusal: %v", err)
	}
}

// TestManagedLateMoveRefusedByDigest is a guard row. Managed moves carried
// digests before this change, so a late task after a source change is
// refused with or without it.
func TestManagedLateMoveRefusedByDigest(t *testing.T) {
	f := digestManagedFixture(t)
	deps, client, cid := f.deps, f.client, f.cid
	client.loseMoveResponse = true
	if err := digestDetach(t, deps, cid); err == nil {
		t.Fatal("detach succeeded although its move response was lost")
	}
	client.loseMoveResponse = false
	if len(client.lostMoves) != 1 {
		t.Fatalf("want one lost move, got %d", len(client.lostMoves))
	}
	changeConfig(client)
	if outcome := client.runLostMove(0); !strings.HasPrefix(outcome, "refused: VM 777: detected modified configuration") {
		t.Fatalf("the late managed task was not refused by its digest: %s", outcome)
	}
}

// TestManagedDroppedMovePoisonsBeforeASecondPOST drops the response to a
// managed detach's move POST with a transport fault the move's retry loop
// retries. The guard poisons on the unanswered POST, and its error ends the
// loop, so the call sends no second POST. A rerun of the detach is refused
// before it sends anything, so no second task can follow the dropped one.
func TestManagedDroppedMovePoisonsBeforeASecondPOST(t *testing.T) {
	f := digestManagedFixture(t)
	deps, client, journal, id, cid := f.deps, f.client, f.journal, f.id, f.cid
	client.dropMoveResponses = 2
	if err := digestDetach(t, deps, cid); err == nil {
		t.Fatal("detach succeeded although its move response was dropped")
	}
	if client.moveCalls != 1 || len(client.lostMoves) != 1 {
		t.Fatalf("want one POST after the dropped response, got %d POSTs and %d forked tasks", client.moveCalls, len(client.lostMoves))
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.Reason, "Nodes.CreateQemuMoveDisk") {
		t.Fatalf("the dropped POST did not poison the guard: %s %q", record.State, record.Reason)
	}
	if err := digestDetach(t, deps, cid); err == nil {
		t.Fatal("rerun detach succeeded after the dropped POST poisoned the guard")
	}
	if client.moveCalls != 1 {
		t.Fatalf("the rerun sent a second move POST, %d in all", client.moveCalls)
	}
}

// TestMoveDigestTakenAfterLastWrite requires both digests on the move into a
// parker and on the move back out. Each transfer writes the parker before its
// move, the intent on the way in and the option bake and protection clear on
// the way out, and the fake refuses a digest that no longer matches.
func TestMoveDigestTakenAfterLastWrite(t *testing.T) {
	deps, client, cid, _ := digestLegacyFixture(t)
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(client.moveDigests) != 2 {
		t.Fatalf("want two move POSTs, got %d", len(client.moveDigests))
	}
	for i, digests := range client.moveDigests {
		if digests[0] == "" || digests[1] == "" {
			t.Fatalf("move %d carried digests %q", i, digests)
		}
	}
}

// configRefusingPVE answers every configuration update with refusal.
type configRefusingPVE struct {
	*lifecycleFlowPVE
	refusal func(vmid int) error
}

func (c configRefusingPVE) Nodes() nodes.Service {
	return configRefusingNodes{lifecycleFlowNodes: c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes), refusal: c.refusal}
}

type configRefusingNodes struct {
	lifecycleFlowNodes
	refusal func(vmid int) error
}

func (n configRefusingNodes) UpdateQemuConfig(_ context.Context, _ string, vmid string, _ *nodes.UpdateQemuConfigParams) error {
	var id int
	_, _ = fmt.Sscan(vmid, &id)
	return n.refusal(id)
}

// configChecksumMismatch is PVE's answer to a configuration update whose
// digest no longer matches (update_vm_api, Qemu.pm:2213), as an HTTP 500.
func configChecksumMismatch(int) error {
	return sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`))
}

// TestConfigUpdateDigestRefusalKeepsItsHandling is a guard row. A managed
// configuration update that PVE refuses on its digest is not a move, so the
// guard is poisoned and its step stays planned, as before. One subrow uses
// the text PVE sends for a configuration update, and the other uses the move
// text, which shows that the move classifier never applies to a config write.
func TestConfigUpdateDigestRefusalKeepsItsHandling(t *testing.T) {
	for name, refusal := range map[string]func(int) error{
		"config update text": configChecksumMismatch,
		"move text":          moveDigestRefusal,
	} {
		t.Run(name, func(t *testing.T) {
			configUpdateRefusalKeepsItsHandling(t, refusal)
		})
	}
}

func configUpdateRefusalKeepsItsHandling(t *testing.T, refusal func(int) error) {
	t.Helper()
	f := digestManagedFixture(t)
	deps, client, journal, id, cid, volume := f.deps, f.client, f.journal, f.id, f.cid, f.volume
	deps.PVE = configRefusingPVE{lifecycleFlowPVE: client, refusal: refusal}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(context.Background(), deps, rd, "detach_disk")
	if err != nil {
		t.Fatal(err)
	}
	slot := "scsi1"
	updateErr := local.PVE.Nodes().UpdateQemuConfig(context.Background(), "n1", "777", &nodes.UpdateQemuConfigParams{Delete: &slot})
	if updateErr == nil || lifecycle.guard.Err() == nil {
		t.Fatalf("a refused configuration update did not poison the guard: err=%v guard=%v", updateErr, lifecycle.guard.Err())
	}
	_ = lifecycle.finish(context.Background(), updateErr, false)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	planned := false
	for i := range record.Steps {
		if step := &record.Steps[i]; step.State == aj.Planned && strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig") {
			planned = true
		}
	}
	if !planned || client.state.volumes[volume] == nil {
		t.Fatalf("the refused update did not keep its existing handling: planned=%t record=%s", planned, record.State)
	}
}

// driveSerial returns the serial a drive value carries.
func driveSerial(t *testing.T, value any) string {
	t.Helper()
	for _, option := range strings.Split(fmt.Sprint(value), ",") {
		if serial, ok := strings.CutPrefix(option, "serial="); ok {
			return serial
		}
	}
	t.Fatalf("drive %v carries no serial", value)
	return ""
}

// assertParkedWithSerial requires that the dropped POST's task, lost move 0,
// is what parked the disk. Its parker slot holds the volume under the name
// the move gave it, with the disk's serial written back, and no key of 777
// names the old volume.
func assertParkedWithSerial(t *testing.T, client *lifecycleFlowPVE, volume, serial string) {
	t.Helper()
	moved := client.lostMoves[0]
	value, _ := pve.ConfigString(client.state.configs[moved.target], moved.slot)
	landed := strings.Split(value, ",")[0]
	if landed == "" || landed == volume || client.state.volumes[landed] == nil {
		t.Fatalf("parker %d slot %s holds %q, want the renamed volume", moved.target, moved.slot, value)
	}
	if !strings.Contains(value, ",serial="+serial) {
		t.Fatalf("parker %d slot %s holds %q without the disk's serial %s", moved.target, moved.slot, value, serial)
	}
	if unusedHolding(client, volume) != "" || client.state.volumes[volume] != nil {
		t.Fatalf("777 still names the old volume %s: %v", volume, client.state.configs[777])
	}
}

// TestLegacyDroppedMoveLandsBeforeRefusedTask drops the response to a legacy
// detach's move POST after PVE forked its task. The retry reads the same
// digests, because that task hasn't written anything yet, and its POST forks
// a second task. The first task commits before the second one runs, so the
// second refuses on its stale source digest. The readback finds that the move
// landed, so the detach succeeds with the disk on the parker and its serial
// written back.
func TestLegacyDroppedMoveLandsBeforeRefusedTask(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	serial := driveSerial(t, client.state.configs[777]["scsi1"])
	client.dropMoveResponses = 1
	client.deferMoveTasks = true
	first := ""
	client.beforeMoveTask = func() { first = client.runLostMove(0) }
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach after the dropped POST's task landed: %v", err)
	}
	if client.moveCalls != 2 || !strings.HasPrefix(first, "moved ") {
		t.Fatalf("want a second POST and the first task landing, got %d POSTs and %q", client.moveCalls, first)
	}
	if len(client.deferredOutcomes) != 1 || !strings.HasPrefix(client.deferredOutcomes[0], "refused: VM 777: detected modified configuration") {
		t.Fatalf("the second task was not refused on its digest: %q", client.deferredOutcomes)
	}
	assertParkedWithSerial(t, client, volume, serial)
}

// TestLegacyDroppedMoveLandsBeforeRefusedRequest is the same race, except that
// the first task commits before the second POST's request checks, so PVE
// refuses the second POST itself. The readback finds that the move landed,
// and the detach succeeds.
func TestLegacyDroppedMoveLandsBeforeRefusedRequest(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	serial := driveSerial(t, client.state.configs[777]["scsi1"])
	client.dropMoveResponses = 1
	first := ""
	client.beforeMoveCheck = func(call int, _ *nodes.CreateQemuMoveDiskParams) {
		if call == 2 {
			first = client.runLostMove(0)
		}
	}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach after the dropped POST's task landed: %v", err)
	}
	if client.moveCalls != 2 || !strings.HasPrefix(first, "moved ") {
		t.Fatalf("want a second POST and the first task landing, got %d POSTs and %q", client.moveCalls, first)
	}
	assertParkedWithSerial(t, client, volume, serial)
}

// TestLegacyMove597LandsBeforeRefusedRetry is the same race, except that the
// first POST comes back 597, which pveproxy relays when the response breaks
// off after pvedaemon forked the task. The 597 carries no answer from PVE, so
// it counts as unanswered. The first task commits before the retry's request
// checks, PVE refuses the retry on its digest, and the readback finds that
// the move landed, so the detach succeeds.
func TestLegacyMove597LandsBeforeRefusedRetry(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	serial := driveSerial(t, client.state.configs[777]["scsi1"])
	client.dropMoveResponses = 1
	client.dropMoveStatus = 597
	first := ""
	client.beforeMoveCheck = func(call int, _ *nodes.CreateQemuMoveDiskParams) {
		if call == 2 {
			first = client.runLostMove(0)
		}
	}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach after the 597 POST's task landed: %v", err)
	}
	if client.moveCalls != 2 || !strings.HasPrefix(first, "moved ") {
		t.Fatalf("want a second POST and the first task landing, got %d POSTs and %q", client.moveCalls, first)
	}
	assertParkedWithSerial(t, client, volume, serial)
}

// TestLegacyDroppedMoveLandsBeforeFailedDigestRead drops the response to a
// legacy detach's move POST, and its task lands before the retry reads the
// digests. That read fails with a permission error, which the loop doesn't
// retry. The readback finds that the move landed, so the detach succeeds
// without a second POST.
func TestLegacyDroppedMoveLandsBeforeFailedDigestRead(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	serial := driveSerial(t, client.state.configs[777]["scsi1"])
	client.dropMoveResponses = 1
	first := ""
	client.onConfigRead = func(vmid int) error {
		if vmid != 777 || client.moveCalls != 1 || first != "" {
			return nil
		}
		first = client.runLostMove(0)
		return sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/vms/777, VM.Audit)\n"}`))
	}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("detach after the dropped POST's task landed: %v", err)
	}
	if client.moveCalls != 1 || !strings.HasPrefix(first, "moved ") {
		t.Fatalf("want one POST and its task landing before the failed read, got %d POSTs and %q", client.moveCalls, first)
	}
	assertParkedWithSerial(t, client, volume, serial)
}

// TestLegacyDroppedMoveRefusalStandsWhenNothingMoved is a guard row. A legacy
// detach's move POST loses its response, and another writer changes the
// source before the second POST, which PVE refuses. The readback finds the
// volume still on the source's unused entry and the parker's slot empty, so
// the refusal stands, and the dropped POST's task refuses too when it runs.
func TestLegacyDroppedMoveRefusalStandsWhenNothingMoved(t *testing.T) {
	deps, client, cid, volume := digestLegacyFixture(t)
	client.dropMoveResponses = 1
	client.beforeMoveCheck = func(call int, _ *nodes.CreateQemuMoveDiskParams) {
		if call == 2 {
			changeConfig(client)
		}
	}
	err := digestDetach(t, deps, cid)
	if err == nil || !errors.Is(err, pve.ErrMoveDiskDigestRefused) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable digest refusal, got %v", err)
	}
	if client.moveCalls != 2 || unusedHolding(client, volume) == "" {
		t.Fatalf("want two POSTs and the volume still on 777's unused entry, got %d POSTs and %v", client.moveCalls, client.state.configs[777])
	}
	if outcome := client.runLostMove(0); !strings.HasPrefix(outcome, "refused: VM 777: detected modified configuration") {
		t.Fatalf("the dropped POST's task was not refused by its digest: %s", outcome)
	}
}

// TestManagedMoveRefusalWithPendingAddOnSlotPoisons is a guard row. PVE
// refuses a managed detach's move in the request, and by the readback the
// parker's receiving slot holds a pending add. The applied view shows the
// slot occupied, so the refusal doesn't settle and the guard is poisoned.
func TestManagedMoveRefusalWithPendingAddOnSlotPoisons(t *testing.T) {
	f := digestManagedFixture(t)
	deps, client, journal, id, cid := f.deps, f.client, f.journal, f.id, f.cid
	client.pending = newFakePendingModel()
	client.changeSourceBeforeMove = true
	client.beforeMoveCheck = func(_ int, p *nodes.CreateQemuMoveDiskParams) {
		target := int(*p.TargetVmid)
		client.pending.holdReplacement(target, client.state.configs[target], *p.TargetDisk, fmt.Sprintf("a:%d/vm-%d-disk-9.raw", target, target))
	}
	err := digestDetach(t, deps, cid)
	if err == nil {
		t.Fatal("detach succeeded although its move was refused")
	}
	record, inspectErr := journal.Inspect(id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if !strings.Contains(record.Reason, "Nodes.CreateQemuMoveDisk") {
		t.Fatalf("a refused move with a pending add on its slot did not poison the guard: err=%v reason=%q", err, record.Reason)
	}
}
