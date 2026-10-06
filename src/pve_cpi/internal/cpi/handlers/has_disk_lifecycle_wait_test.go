package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// hasDiskResult is one has_disk answer.
type hasDiskResult struct {
	present any
	err     error
}

// hasDiskOverlap is a has_disk call that runs alongside another call on the
// same disk. It starts from inside that call's hook, and it stops once its
// first answer has refused and it waits for the disk's lifecycle, or once it
// has answered without waiting.
type hasDiskOverlap struct {
	// refusal is the first answer's refusal, and waited reports whether
	// has_disk started waiting at all.
	refusal error
	waited  bool
	// fakeBefore, fakeAfter, journalBefore, and journalAfter are the fake's
	// writes and the journal's files before has_disk started and when it
	// began to wait.
	fakeBefore, fakeAfter       string
	journalBefore, journalAfter map[string][]byte
	release                     chan struct{}
	result                      chan hasDiskResult
	answered                    *hasDiskResult
}

// startHasDiskOverlap runs has_disk on m's disk and returns once has_disk
// waits for the disk's lifecycle or has answered. The fake isn't safe for
// concurrent use, so the overlapping call stays inside its hook until then,
// and has_disk asks again only after finish, which a row calls once the
// overlapping call has returned.
func startHasDiskOverlap(t *testing.T, ctx context.Context, f digestManaged) *hasDiskOverlap {
	t.Helper()
	o := &hasDiskOverlap{release: make(chan struct{}), result: make(chan hasDiskResult, 1)}
	o.fakeBefore = flowFakeWrites(t, f.client)
	o.journalBefore = journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	waiting := make(chan error, 1)
	seam := hasDiskWaitSeam{
		limit:   30 * time.Second,
		waiting: func(refusal error) { waiting <- refusal },
		held:    func() { <-o.release },
	}
	args := []json.RawMessage{planJSON(t, f.cid)}
	go func() {
		present, err := HandleHasDisk(f.deps).Handle(withHasDiskWaitForTest(ctx, seam), args, jsonrpc.Context{})
		o.result <- hasDiskResult{present, err}
	}()
	select {
	case o.refusal = <-waiting:
		o.waited = true
	case r := <-o.result:
		o.answered = &r
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk neither answered nor waited")
	}
	o.fakeAfter = flowFakeWrites(t, f.client)
	o.journalAfter = journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	return o
}

// finish lets has_disk ask again, and returns its answer.
func (o *hasDiskOverlap) finish(t *testing.T) (any, error) {
	t.Helper()
	close(o.release)
	if o.answered != nil {
		return o.answered.present, o.answered.err
	}
	select {
	case r := <-o.result:
		return r.present, r.err
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk never answered after the overlapping call returned")
		return nil, nil
	}
}

// requireWaitedQuietly checks that has_disk refused first, with a refusal
// that starts with prefix, and that it wrote nothing before it waited.
func (o *hasDiskOverlap) requireWaitedQuietly(t *testing.T, where, prefix string) {
	t.Helper()
	if !o.waited {
		t.Fatalf("%s: has_disk answered %v, %v without waiting for the lifecycle", where, o.answered.present, o.answered.err)
	}
	if !strings.HasPrefix(o.refusal.Error(), prefix) {
		t.Errorf("%s: has_disk's first answer refused with %v, want a refusal starting %q", where, o.refusal, prefix)
	}
	if o.fakeAfter != o.fakeBefore {
		t.Errorf("%s: has_disk wrote to PVE before it waited\nbefore %s\nafter  %s", where, o.fakeBefore, o.fakeAfter)
	}
	if !reflect.DeepEqual(o.journalAfter, o.journalBefore) {
		t.Errorf("%s: has_disk changed the journal before it waited", where)
	}
}

// TestOverlappingMoveAnswerWaitsForTheDetach covers a has_disk that overlaps
// a detach whose move has landed and whose step isn't observed yet. has_disk
// sees the parker's transfer record name a volume that's gone, and before it
// answers, it waits for the detach to let go of the disk's record. It then
// asks again and finds the disk on its parker, without writing anything.
func TestOverlappingMoveAnswerWaitsForTheDetach(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	var overlap *hasDiskOverlap
	f.client.afterMove = func() {
		if overlap == nil {
			overlap = startHasDiskOverlap(t, digestCtx(), f)
		}
	}
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach_disk: %v", err)
	}
	if overlap == nil {
		t.Fatal("detach_disk sent no move, so has_disk never overlapped it")
	}
	overlap.requireWaitedQuietly(t, "has_disk during the detach's move", missingVolumeRefusal)
	if present, err := overlap.finish(t); err != nil || present != true {
		t.Errorf("has_disk during the detach's move = %v, %v, want true once the detach finished", present, err)
	}
}

// TestHasDiskWaitsOutAnAttachLanding covers a has_disk that overlaps an
// attach_disk whose move off the parker has landed on VM 777, before the
// attach writes the disk's provenance on 777. has_disk waits for the attach
// and then finds the disk attached.
func TestHasDiskWaitsOutAnAttachLanding(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	var overlap *hasDiskOverlap
	f.client.afterMove = func() {
		if overlap == nil {
			overlap = startHasDiskOverlap(t, digestCtx(), f)
		}
	}
	if err := digestAttach(t, f.deps, f.cid); err != nil {
		t.Fatalf("attach_disk: %v", err)
	}
	if overlap == nil {
		t.Fatal("attach_disk sent no move, so has_disk never overlapped it")
	}
	overlap.requireWaitedQuietly(t, "has_disk during the attach's landing", "managed disk")
	if present, err := overlap.finish(t); err != nil || present != true {
		t.Errorf("has_disk during the attach's landing = %v, %v, want true once the attach finished", present, err)
	}
}

// TestHasDiskWaitsOutAnUpdateDiskResume covers update_disk, which renames a
// disk only when it resumes a park that a refused detach left on 777's unused
// entry. A has_disk that overlaps that resume's move waits for update_disk
// and then finds the disk on its parker.
func TestHasDiskWaitsOutAnUpdateDiskResume(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	f.client.changeSourceBeforeMove = true
	if err := digestDetach(t, f.deps, f.cid); err == nil {
		t.Fatal("want the detach's digest refusal")
	}
	if unusedHolding(f.client, f.volume) == "" {
		t.Fatalf("the refused detach left no unused entry: %v", f.client.state.configs[777])
	}
	var overlap *hasDiskOverlap
	f.client.afterMove = func() {
		if overlap == nil {
			overlap = startHasDiskOverlap(t, digestCtx(), f)
		}
	}
	if _, err := HandleUpdateDisk(f.deps).Handle(digestCtx(), overlayArgs(t, f.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("update_disk: %v", err)
	}
	if overlap == nil {
		t.Fatal("update_disk sent no move, so has_disk never overlapped it")
	}
	overlap.requireWaitedQuietly(t, "has_disk during update_disk's resume", missingVolumeRefusal)
	if present, err := overlap.finish(t); err != nil || present != true {
		t.Errorf("has_disk during update_disk's resume = %v, %v, want true once update_disk finished", present, err)
	}
}

// TestHasDiskWaitsOutAParkedDelete covers a has_disk that overlaps a
// delete_disk of a parked disk after PVE has destroyed the volume and before
// the delete settles its record. has_disk waits for the delete and then
// reports the disk gone.
func TestHasDiskWaitsOutAParkedDelete(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	var overlap *hasDiskOverlap
	f.client.onVolumeDeleted = func() {
		if overlap == nil {
			overlap = startHasDiskOverlap(t, digestCtx(), f)
		}
	}
	if err := deleteDiskAt(t, digestCtx(), f.deps, f.cid); err != nil {
		t.Fatalf("delete_disk: %v", err)
	}
	if overlap == nil {
		t.Fatal("delete_disk destroyed no volume, so has_disk never overlapped it")
	}
	overlap.requireWaitedQuietly(t, "has_disk during the delete", "managed disk")
	if present, err := overlap.finish(t); err != nil || present != false {
		t.Errorf("has_disk during the delete = %v, %v, want false once the delete finished", present, err)
	}
}

// holdRecord takes the disk's record lock and saves the reason a lifecycle
// saves when it starts. It returns a function that saves the record again,
// the way a lifecycle does when it ends, and lets the lock go. A record leaves
// reconciliation only with fresh audit evidence, which the row has no need
// for, so the record stays in reconciliation with its reason cleared.
func holdRecord(t *testing.T, f digestManaged, operation string) func() {
	t.Helper()
	handle, err := f.journal.Acquire(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "lifecycle " + operation + " admitted; completion pending"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = handle.Close()
		}
	})
	return func() {
		t.Helper()
		released = true
		record := handle.Record()
		record.Reason = "lifecycle " + operation + " ended; audit pending"
		if err := handle.Save(record); err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// hideVolumeOnce makes the next content read of the disk's volume find it
// gone, the way a read finds a volume a lifecycle renamed after the identity
// scan named it. It returns a function that puts the volume back.
func hideVolumeOnce(t *testing.T, f digestManaged) func() {
	t.Helper()
	volume, ok := f.client.state.volumes[f.volume]
	if !ok {
		t.Fatalf("the fake holds no volume %s", f.volume)
	}
	hidden := false
	f.client.onContentRead = func(read string) {
		if read == f.volume && !hidden {
			hidden = true
			delete(f.client.state.volumes, f.volume)
		}
	}
	return func() {
		f.client.onContentRead = nil
		f.client.state.volumes[f.volume] = volume
	}
}

// TestHasDiskWaitsOutARenameBetweenScanAndRead covers a lifecycle that
// renames an attached disk between has_disk's identity scan and its read of
// the volume, so the read finds the name 777 carries gone. has_disk waits for
// the lifecycle to let go of the disk's record, asks again, and finds the
// disk.
func TestHasDiskWaitsOutARenameBetweenScanAndRead(t *testing.T) {
	f := digestManagedFixture(t)
	finish := holdRecord(t, f, "update_disk")
	restore := hideVolumeOnce(t, f)
	overlap := startHasDiskOverlap(t, digestCtx(), f)
	// The row hid the volume itself, so the fake is read again once the
	// volume is back, while has_disk still waits.
	restore()
	overlap.fakeAfter = flowFakeWrites(t, f.client)
	overlap.requireWaitedQuietly(t, "has_disk during the rename", "managed disk ownership provenance references a missing volume")
	finish()
	if present, err := overlap.finish(t); err != nil || present != true {
		t.Errorf("has_disk during the rename = %v, %v, want true once the lifecycle finished", present, err)
	}
}

// TestHasDiskWaitTimesOutRetriably covers a lifecycle that holds the disk's
// record for longer than has_disk may wait. has_disk gives up with a
// retriable error that names the lifecycle's operation and says to rerun the
// cloud check, and it writes nothing.
func TestHasDiskWaitTimesOutRetriably(t *testing.T) {
	f := digestManagedFixture(t)
	holdRecord(t, f, "detach_disk")
	restore := hideVolumeOnce(t, f)
	fakeBefore := flowFakeWrites(t, f.client)
	journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 100 * time.Millisecond})
	start := time.Now()
	_, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if waited := time.Since(start); waited < 100*time.Millisecond || waited > 10*time.Second {
		t.Errorf("has_disk returned after %s, want it to wait its 100ms limit", waited)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk: error %v, want a retriable error", err)
	}
	requireText(t, err, "has_disk's expired wait", []string{"lifecycle detach_disk", f.cid, "rerun the cloud check once that operation completes"})
	restore()
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("has_disk changed the journal")
	}
}

// TestHasDiskWaitLimitKeepsTimeForTheSecondLook checks the wait's limit and
// the second look's end. With no deadline the wait is 120 seconds and the
// look ends 10 seconds after it. With a deadline the look ends at the
// deadline and the wait stops 10 seconds before it, so the wait is zero or
// less when 10 seconds or less remain.
func TestHasDiskWaitLimitKeepsTimeForTheSecondLook(t *testing.T) {
	start := time.Now()
	if wait, end := hasDiskWaitLimit(context.Background(), start); wait != 120*time.Second || !end.Equal(start.Add(130*time.Second)) {
		t.Errorf("without a deadline: wait %s, look ends %s after the start, want 120s and 130s", wait, end.Sub(start))
	}
	for _, tc := range []struct {
		remaining, wait time.Duration
	}{
		{120 * time.Second, 110 * time.Second},
		{30 * time.Second, 20 * time.Second},
		{10 * time.Second, 0},
		{5 * time.Second, -5 * time.Second},
		{-time.Second, -11 * time.Second},
	} {
		ctx, cancel := context.WithDeadline(context.Background(), start.Add(tc.remaining))
		wait, end := hasDiskWaitLimit(ctx, start)
		cancel()
		if wait != tc.wait || !end.Equal(start.Add(tc.remaining)) {
			t.Errorf("with %s left: wait %s, look ends %s after the start, want %s and %s", tc.remaining, wait, end.Sub(start), tc.wait, tc.remaining)
		}
	}
	ctx := withHasDiskWaitForTest(context.Background(), hasDiskWaitSeam{limit: time.Second})
	if wait, _ := hasDiskWaitLimit(ctx, start); wait != time.Second {
		t.Errorf("with a shorter test limit: wait %s, want 1s", wait)
	}
}

// TestHasDiskDoesNotWaitOnASettledDisk checks that has_disk answers at once
// for a disk its first answer settles, even while a lifecycle holds the
// disk's record.
func TestHasDiskDoesNotWaitOnASettledDisk(t *testing.T) {
	f := digestManagedFixture(t)
	finish := holdRecord(t, f, "set_disk_metadata")
	defer finish()
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) { waited = true }})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err != nil || present != true || waited {
		t.Errorf("has_disk = %v, %v, waited %v, want true at once", present, err, waited)
	}
}

// abandonStepOnParkedVolume leaves a parked disk's record needing
// reconciliation with an unsettled step of the given kind that targets the
// parked volume, as a call that died partway would, and then removes that
// volume from PVE. The step names the VM the park moved the disk from, or,
// when onParker is set, the parker that owns the parked volume.
func abandonStepOnParkedVolume(t *testing.T, f digestManaged, kind string, onParker bool) {
	t.Helper()
	handle, err := f.journal.Acquire(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "an answer from PVE was lost"
	var landed aj.Step
	for i := range record.Steps {
		if observedMoveStep(&record.Steps[i]) {
			landed = record.Steps[i]
		}
	}
	if len(landed.VolIDs) < 2 {
		t.Fatalf("the park recorded no landed move: %+v", record.Steps)
	}
	parked := landed.VolIDs[len(landed.VolIDs)-1]
	target := landed.Target
	target.IntendedVolume = parked
	if onParker {
		parker, ok := pve.EmbeddedDiskVMID(parked)
		if !ok || parker == landed.Target.VMID {
			t.Fatalf("the parked volume %s isn't named for a parker", parked)
		}
		target.VMID = parker
	}
	record.Steps = append(record.Steps, aj.Step{ID: "abandoned", Kind: kind, State: aj.Planned, Attempt: record.ActiveAttempt(), Target: target})
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	delete(f.client.state.volumes, parked)
}

// TestHasDiskKeepsRefusingAnAbandonedDelete covers a parked disk whose
// volume is gone while its record keeps a step that no live call holds. When
// that step is a delete, the disk may be gone, so has_disk keeps refusing it
// for audit. A configuration write on the parker that owns the volume counts
// as a delete too, because a detach deletes unused entries through such
// writes, and PVE destroys a volume whose unused entry its owner loses. When
// the step is a move, or a configuration write on a VM that doesn't own the
// volume, the parker still carries the disk's identity, so has_disk reports
// it present, and the other disk calls keep refusing it.
func TestHasDiskKeepsRefusingAnAbandonedDelete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		onParker bool
		present  bool
	}{
		{"delete", "lifecycle_delete_disk_Storage_DeleteVolumeAsync", false, false},
		{"move", "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk", false, true},
		{"config write on the owning parker", "lifecycle_detach_disk_Nodes_UpdateQemuConfig", true, false},
		{"config write on another VM", "lifecycle_detach_disk_Nodes_UpdateQemuConfig", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			f := digestManagedFixture(t)
			if err := digestDetach(t, f.deps, f.cid); err != nil {
				t.Fatalf("park the disk: %v", err)
			}
			abandonStepOnParkedVolume(t, f, tc.kind, tc.onParker)
			fakeBefore := flowFakeWrites(t, f.client)
			present, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			if after := flowFakeWrites(t, f.client); after != fakeBefore {
				t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
			}
			if tc.present {
				if err != nil || present != true {
					t.Errorf("has_disk on an abandoned %s = %v, %v, want true", tc.name, present, err)
				}
				if err := digestAttach(t, f.deps, f.cid); err == nil || !strings.Contains(err.Error(), missingVolumeRefusal) {
					t.Errorf("attach_disk on an abandoned %s: error %v, want the missing-volume refusal for audit", tc.name, err)
				}
				return
			}
			if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.Contains(err.Error(), missingVolumeRefusal) {
				t.Fatalf("has_disk on an abandoned %s = %v, %v, want the permanent missing-volume refusal for audit", tc.name, present, err)
			}
		})
	}
}

// TestHasDiskKeepsRefusingAConflictingRecord covers a lost move that left
// its step unsettled, after which the parker's transfer record came to name
// another allocation. That record contradicts the journal, so has_disk keeps
// refusing the disk for audit after its wait, rather than report it present.
func TestHasDiskKeepsRefusingAConflictingRecord(t *testing.T) {
	captureParkerPoolSweep(t)
	m := unfiredDetach(t, true)
	if outcome := m.client.runLostMove(0); !strings.HasPrefix(outcome, "moved ") {
		t.Fatalf("the lost move didn't land: %s", outcome)
	}
	parker := 0
	for vmid := range m.client.state.configs {
		if isParkerVM(m.client, vmid) {
			parker = vmid
		}
	}
	if parker == 0 {
		t.Fatal("the lost move left no parker")
	}
	editParkedRecord(t, m.digestManaged, parker, m.token, func(entry map[string]any) {
		entry["allocation_backing"] = "another-backing"
	})
	_, err := HandleHasDisk(m.deps).Handle(settleAt(pastQuietPeriod), []json.RawMessage{planJSON(t, m.cid)}, jsonrpc.Context{})
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.Contains(err.Error(), "provenance conflicts; audit required") {
		t.Fatalf("has_disk on a conflicting transfer record: error %v, want the permanent conflict refusal for audit", err)
	}
}

// TestHasDiskLooksAgainAfterAnUnchangedRecord covers a lifecycle that renamed
// the disk and settled its record between has_disk's identity scan and its
// read of the record. The record has nothing left to change, so it reads
// the same after the wait, but the first answer rests on the name the scan
// saw before the rename. has_disk looks at the disk again under the shared
// hold and finds it.
func TestHasDiskLooksAgainAfterAnUnchangedRecord(t *testing.T) {
	f := digestManagedFixture(t)
	restore := hideVolumeOnce(t, f)
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) { waited = true; restore() }})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !waited {
		t.Fatalf("has_disk = %v, %v without waiting, want it to refuse first and wait", present, err)
	}
	if err != nil || present != true {
		t.Errorf("has_disk after an unchanged record = %v, %v, want true from the second look", present, err)
	}
}

// TestHasDiskWaitsBeforeAnsweringFalse covers a disk on its way between two
// guests while has_disk's scan reads them, so no read finds a guest that
// carries it and the first look finds it absent. A lifecycle holds the
// disk's record, so has_disk waits for it before it answers, and the second
// look finds the disk where the lifecycle left it.
func TestHasDiskWaitsBeforeAnsweringFalse(t *testing.T) {
	f := digestManagedFixture(t)
	finish := holdRecord(t, f, "attach_disk")
	slot := f.client.state.configs[777]["scsi1"]
	delete(f.client.state.configs[777], "scsi1")
	waiting := make(chan error, 1)
	release := make(chan struct{})
	seam := hasDiskWaitSeam{
		limit:   30 * time.Second,
		waiting: func(refusal error) { waiting <- refusal },
		held:    func() { <-release },
	}
	result := make(chan hasDiskResult, 1)
	go func() {
		present, err := HandleHasDisk(f.deps).Handle(withHasDiskWaitForTest(digestCtx(), seam), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
		result <- hasDiskResult{present, err}
	}()
	select {
	case refusal := <-waiting:
		if refusal != nil {
			t.Errorf("has_disk's first look refused with %v, want an absent answer", refusal)
		}
	case r := <-result:
		f.client.state.configs[777]["scsi1"] = slot
		finish()
		t.Fatalf("has_disk = %v, %v without waiting for the lifecycle that holds the disk's record", r.present, r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk neither answered nor waited")
	}
	f.client.state.configs[777]["scsi1"] = slot
	finish()
	close(release)
	select {
	case r := <-result:
		if r.err != nil || r.present != true {
			t.Errorf("has_disk after the lifecycle finished = %v, %v, want true", r.present, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk never answered after the lifecycle finished")
	}
}

// TestHasDiskKeepsTimeForTheSecondLook covers a request with too little time
// left for has_disk to wait and still look at the disk again, while a
// lifecycle holds the disk's record. has_disk doesn't wait at all, and it
// returns its retriable wait error at once, naming that lifecycle.
func TestHasDiskKeepsTimeForTheSecondLook(t *testing.T) {
	f := digestManagedFixture(t)
	holdRecord(t, f, "detach_disk")
	restore := hideVolumeOnce(t, f)
	defer restore()
	ctx, cancel := context.WithTimeout(digestCtx(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("has_disk returned after %s, want it to return at once", waited)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk: error %v, want a retriable error", err)
	}
	requireText(t, err, "has_disk without time for a second look", []string{"lifecycle detach_disk", f.cid, "too little of the request's time is left", "rerun the cloud check once that operation completes"})
}

// TestHasDiskSkipsASecondLookWithoutTime covers a wait for a lifecycle that
// ends with less than the second look's share of the request left. has_disk
// doesn't start the second look, and it returns its retriable wait error,
// which names the lifecycle it waited for.
func TestHasDiskSkipsASecondLookWithoutTime(t *testing.T) {
	f := digestManagedFixture(t)
	finish := holdRecord(t, f, "detach_disk")
	restore := hideVolumeOnce(t, f)
	defer restore()
	ctx, cancel := context.WithTimeout(digestCtx(), hasDiskSecondLookFloor+time.Second)
	defer cancel()
	waiting := make(chan struct{})
	ctx = withHasDiskWaitForTest(ctx, hasDiskWaitSeam{waiting: func(error) { close(waiting) }, held: func() { time.Sleep(time.Second) }})
	result := make(chan hasDiskResult, 1)
	go func() {
		present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
		result <- hasDiskResult{present, err}
	}()
	select {
	case <-waiting:
	case r := <-result:
		finish()
		t.Fatalf("has_disk = %v, %v without waiting for the lifecycle", r.present, r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk never started to wait")
	}
	time.Sleep(200 * time.Millisecond)
	finish()
	var r hasDiskResult
	select {
	case r = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk never answered")
	}
	if r.err == nil || !isTypedCPIError(r.err) || !okToRetryCPIError(r.err) {
		t.Fatalf("has_disk: %v, %v, want a retriable error", r.present, r.err)
	}
	requireText(t, r.err, "has_disk without time for a second look", []string{"lifecycle detach_disk", f.cid, "too little of the request's time is left", "rerun the cloud check once that operation completes"})
}

// TestHasDiskLooksAgainOnAShortBudget covers a request with less time left
// than the second look's floor, while no lifecycle holds the disk's record.
// has_disk has nothing to wait for, so it takes the record's lock at once and
// looks again on the time it has, and the second look finds the disk.
func TestHasDiskLooksAgainOnAShortBudget(t *testing.T) {
	f := digestManagedFixture(t)
	restore := hideVolumeOnce(t, f)
	defer restore()
	ctx, cancel := context.WithTimeout(digestCtx(), 3*time.Second)
	defer cancel()
	waited := false
	ctx = withHasDiskWaitForTest(ctx, hasDiskWaitSeam{waiting: func(error) { waited = true; restore() }})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !waited {
		t.Fatalf("has_disk = %v, %v without a second look, want it to refuse first and look again", present, err)
	}
	if err != nil || present != true {
		t.Errorf("has_disk on a 3s budget with nothing held = %v, %v, want true from the second look", present, err)
	}
}

// TestHasDiskSecondLookEndsWithTheRequest covers a request that is cancelled
// while has_disk's second look reads the cluster, with no lifecycle holding
// the disk's record. has_disk returns a retriable error that says the
// request ended before it could look again, and names no operation, because
// none was running.
func TestHasDiskSecondLookEndsWithTheRequest(t *testing.T) {
	f := digestManagedFixture(t)
	restore := hideVolumeOnce(t, f)
	defer restore()
	ctx, cancel := context.WithCancel(digestCtx())
	defer cancel()
	ctx = withHasDiskWaitForTest(ctx, hasDiskWaitSeam{
		waiting: func(error) { restore() },
		held: func() {
			f.client.onConfigRead = func(int) error {
				cancel()
				return context.Canceled
			}
		},
	})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk cancelled during its second look = %v, %v, want a retriable error", present, err)
	}
	requireText(t, err, "has_disk cancelled during its second look",
		[]string{f.cid, "no other operation held", "the request ended before has_disk could look at the disk again", "rerun the cloud check"},
		"lifecycle ", "another disk operation")
}

// TestHasDiskSecondLookStopsBeforeTheDeadline covers a second look still
// reading the cluster as the request's deadline nears. has_disk stops the
// look a second early and returns its own retriable error while the request
// is still live, so the dispatcher passes that error on instead of replacing
// it with its generic timeout. No lifecycle holds the disk's record, so the
// second look starts at once on a budget shorter than its floor.
func TestHasDiskSecondLookStopsBeforeTheDeadline(t *testing.T) {
	f := digestManagedFixture(t)
	restore := hideVolumeOnce(t, f)
	defer restore()
	request, cancel := context.WithTimeout(digestCtx(), 2*time.Second)
	defer cancel()
	ctx := withHasDiskWaitForTest(request, hasDiskWaitSeam{
		waiting: func(error) { restore() },
		held: func() {
			f.client.onConfigReadContext = func(ctx context.Context, _ int) error {
				<-ctx.Done()
				return ctx.Err()
			}
		},
	})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if ended := request.Err(); ended != nil {
		t.Errorf("has_disk returned after the request ended (%v), so the dispatcher's timeout would replace its error", ended)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk whose second look ran into the deadline = %v, %v, want a retriable error", present, err)
	}
	requireText(t, err, "has_disk whose second look ran into the deadline", []string{f.cid, "the request ended before has_disk could look at the disk again"})
}

// TestHasDiskCancelledWaitIsRetriable covers a request that is cancelled
// while has_disk waits for a lifecycle. has_disk returns its retriable wait
// error with the time it waited, never the first look's permanent refusal.
func TestHasDiskCancelledWaitIsRetriable(t *testing.T) {
	f := digestManagedFixture(t)
	holdRecord(t, f, "detach_disk")
	restore := hideVolumeOnce(t, f)
	defer restore()
	ctx, cancel := context.WithCancel(digestCtx())
	defer cancel()
	ctx = withHasDiskWaitForTest(ctx, hasDiskWaitSeam{limit: 30 * time.Second, waiting: func(error) { time.AfterFunc(100*time.Millisecond, cancel) }})
	start := time.Now()
	_, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("has_disk returned %s after the request was cancelled, want it to stop at once", waited)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk: error %v, want a retriable error", err)
	}
	requireText(t, err, "has_disk's cancelled wait", []string{"lifecycle detach_disk", f.cid, "the request ended", "rerun the cloud check once that operation completes"})
}

// unsettleAttachLanding rewrites the disk's record the way a call that died
// after the attach's move landed on 777 leaves it. The move step keeps its
// task and stays unsettled, the steps after it are gone, and the record
// needs reconciliation. edit, when set, changes the move step further.
func unsettleAttachLanding(t *testing.T, f digestManaged, edit func(*aj.Step)) {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	landing := -1
	for i := range record.Steps {
		step := &record.Steps[i]
		if observedMoveStep(step) && step.Target.VMID != 777 {
			landing = i
		}
	}
	if landing < 0 {
		t.Fatalf("the attach recorded no landed move onto 777: %+v", record.Steps)
	}
	record.Steps = record.Steps[:landing+1]
	step := &record.Steps[landing]
	// The fake answers this move with a short UPID, so the step gets the one
	// PVE gives a move to another VM, which names both VMs and slots.
	step.UPID = fmt.Sprintf("UPID:%s:000573C0:03504636:6AA1786A:qmmove:%d-scsi1>777-scsi1:root@pam:", step.Target.Node, step.Target.VMID)
	step.State = aj.Submitted
	step.VolIDs = nil
	if edit != nil {
		edit(step)
	}
	record.State = aj.ReconciliationRequired
	record.Reason = "an answer from PVE was lost"
	rewriteJournalRecord(t, f.deps, f.id, record)
}

// TestHasDiskReportsAnUnsettledAttachLanding covers an attach that died after
// its move landed the disk on 777 and before it observed the move. 777
// carries the disk under a name no step of the record names. The record's
// unsettled move names 777 as its target, so the name is the landing and
// not a contradiction, and has_disk reports the disk present while the other
// disk calls keep refusing it. A move whose task names another VM, or a move
// with no task, explains nothing, so has_disk keeps refusing the disk for
// audit.
func TestHasDiskReportsAnUnsettledAttachLanding(t *testing.T) {
	const conflict = "managed disk physical backing or volume conflicts with journal; audit required"
	for _, tc := range []struct {
		name    string
		edit    func(*aj.Step)
		present bool
	}{
		{"move onto the holder", nil, true},
		{"move onto another VM", func(step *aj.Step) { step.UPID = strings.Replace(step.UPID, ">777-", ">999-", 1) }, false},
		{"move with no task", func(step *aj.Step) { step.State, step.UPID = aj.Planned, "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			unsettleAttachLanding(t, f, tc.edit)
			fakeBefore := flowFakeWrites(t, f.client)
			present, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			if after := flowFakeWrites(t, f.client); after != fakeBefore {
				t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
			}
			if tc.present {
				if err != nil || present != true {
					t.Fatalf("has_disk on an unsettled attach landing = %v, %v, want true", present, err)
				}
				if err := digestDetach(t, f.deps, f.cid); err == nil || !strings.Contains(err.Error(), conflict) {
					t.Errorf("detach_disk on an unsettled attach landing: error %v, want the refusal for audit", err)
				}
				return
			}
			if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.Contains(err.Error(), conflict) {
				t.Fatalf("has_disk on a %s = %v, %v, want the permanent conflict refusal for audit", tc.name, present, err)
			}
		})
	}
}

// TestHasDiskWaitsThroughTheHoldersNode covers a disk whose holder runs on
// another node than the configured one, after the configured node stops
// answering. has_disk's first look reads the journal through the holder's
// node, and its wait and second look do the same, so it finds the disk.
func TestHasDiskWaitsThroughTheHoldersNode(t *testing.T) {
	f := digestManagedFixture(t)
	if f.client.vmNodes == nil {
		f.client.vmNodes = map[int]string{}
	}
	f.client.vmNodes[777] = "n2"
	volume, ok := f.client.state.volumes[f.volume]
	if !ok {
		t.Fatalf("the fake holds no volume %s", f.volume)
	}
	hidden := false
	f.client.onContentRead = func(read string) {
		if read == f.volume && !hidden {
			hidden = true
			delete(f.client.state.volumes, f.volume)
			f.client.certificateErr = map[string]error{"n1": &sdkerrors.ConnectionError{Host: "n1", Port: 8006, Message: "connection refused"}}
		}
	}
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) {
		waited = true
		f.client.onContentRead = nil
		f.client.state.volumes[f.volume] = volume
	}})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !hidden {
		t.Fatal("has_disk never read the disk's volume on the holder's node")
	}
	if !waited {
		t.Fatalf("has_disk = %v, %v without waiting, want it to wait through the holder's node", present, err)
	}
	if err != nil || present != true {
		t.Errorf("has_disk with the holder on n2 and n1 unreachable = %v, %v, want true", present, err)
	}
}

// refuseCertificates makes every certificate read of n1 fail, the way a node
// that stops answering does, so the journal can't be opened through it.
func refuseCertificates(f digestManaged) {
	f.client.certificateErr = map[string]error{"n1": &sdkerrors.ConnectionError{Host: "n1", Port: 8006, Message: "connection refused"}}
}

// TestHasDiskFalseAnswerNeedsTheJournal covers a disk on its way between two
// guests, which the first look finds absent, after which the journal can't
// be opened to check for the lifecycle that holds its record. has_disk can't
// show that no lifecycle holds it, so it returns a retriable error instead
// of false.
func TestHasDiskFalseAnswerNeedsTheJournal(t *testing.T) {
	f := digestManagedFixture(t)
	slot := f.client.state.configs[777]["scsi1"]
	delete(f.client.state.configs[777], "scsi1")
	defer func() { f.client.state.configs[777]["scsi1"] = slot }()
	waited := false
	ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(refusal error) {
		waited = true
		if refusal != nil {
			t.Errorf("has_disk's first look refused with %v, want an absent answer", refusal)
		}
		refuseCertificates(f)
	}})
	present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !waited {
		t.Fatalf("has_disk = %v, %v without checking for a lifecycle", present, err)
	}
	if err == nil || !isTypedCPIError(err) || !okToRetryCPIError(err) {
		t.Fatalf("has_disk with the journal unreachable after an absent first look = %v, %v, want a retriable error", present, err)
	}
	requireText(t, err, "has_disk with the journal unreachable", []string{f.cid, "couldn't check whether another operation holds", "rerun the cloud check once the allocation journal can be read"})
}

// TestHasDiskRefusalWhenTheJournalFails covers a first look that refuses the
// disk, after which the journal can't be opened to wait for its lifecycle.
// While the request runs, has_disk keeps the first refusal. When the request
// has ended, has_disk returns a retriable error instead, because the first
// refusal may rest on a lifecycle it never got to wait for.
func TestHasDiskRefusalWhenTheJournalFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
	}{
		{name: "request running"},
		{name: "request ended", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			volume, ok := f.client.state.volumes[f.volume]
			if !ok {
				t.Fatalf("the fake holds no volume %s", f.volume)
			}
			ctx, cancel := context.WithCancel(digestCtx())
			defer cancel()
			hidden := false
			f.client.onContentRead = func(read string) {
				if read == f.volume && !hidden {
					hidden = true
					delete(f.client.state.volumes, f.volume)
					refuseCertificates(f)
					if tc.cancelled {
						cancel()
					}
				}
			}
			defer func() {
				f.client.onContentRead = nil
				f.client.state.volumes[f.volume] = volume
			}()
			present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			if !hidden {
				t.Fatal("has_disk never read the disk's volume")
			}
			if err == nil || !isTypedCPIError(err) {
				t.Fatalf("has_disk = %v, %v, want a typed error", present, err)
			}
			if !tc.cancelled {
				if okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), "managed disk ownership provenance references a missing volume") {
					t.Fatalf("has_disk with the journal unreachable = %v, want the first look's permanent refusal", err)
				}
				return
			}
			if !okToRetryCPIError(err) {
				t.Fatalf("has_disk after the request ended = %v, want a retriable error", err)
			}
			requireText(t, err, "has_disk after the request ended", []string{f.cid, "the request ended before has_disk could check whether another operation holds", "rerun the cloud check"})
		})
	}
}

// unmarkedDiskCID returns a CID for f's disk under a birth name that isn't an
// allocation name, with token as its stable ID. Such a disk's allocation is
// known only from its holder's description or its parker's transfer record.
func unmarkedDiskCID(t *testing.T, f digestManaged, token string) string {
	t.Helper()
	storage, _, ok := strings.Cut(f.volume, ":")
	if !ok {
		t.Fatalf("volume %s names no storage", f.volume)
	}
	cid, err := pve.EncodeDiskCID(storage+":vm-777-disk-9", &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

// unmarkedFixture is the flow fixture's disk on VM 777 with no description,
// so no holder of it carries an allocation marker.
func unmarkedFixture(t *testing.T) (digestManaged, string) {
	t.Helper()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	deps.Logger = log.NewNopLogger()
	value, _ := pve.ConfigString(client.state.configs[777], "scsi1")
	f := digestManaged{deps: deps, client: client, journal: journal, id: id, volume: strings.Split(value, ",")[0]}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	return f, record.DiskToken
}

// TestHasDiskWaitsForAnUnmarkedDiskInFlight covers a disk whose CID carries
// no allocation, on its way between two guests while has_disk's scan reads
// them. No guest carries it, and its birth name is gone, but the allocation
// record that names its token is held by a lifecycle. has_disk waits for
// that lifecycle and looks again, and the second look finds the disk.
func TestHasDiskWaitsForAnUnmarkedDiskInFlight(t *testing.T) {
	f, token := unmarkedFixture(t)
	cid := unmarkedDiskCID(t, f, token)
	finish := holdRecord(t, f, "attach_disk")
	slot := f.client.state.configs[777]["scsi1"]
	delete(f.client.state.configs[777], "scsi1")
	waiting := make(chan error, 1)
	release := make(chan struct{})
	seam := hasDiskWaitSeam{
		limit:   30 * time.Second,
		waiting: func(refusal error) { waiting <- refusal },
		held:    func() { <-release },
	}
	result := make(chan hasDiskResult, 1)
	go func() {
		present, err := HandleHasDisk(f.deps).Handle(withHasDiskWaitForTest(digestCtx(), seam), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
		result <- hasDiskResult{present, err}
	}()
	select {
	case <-waiting:
	case r := <-result:
		f.client.state.configs[777]["scsi1"] = slot
		finish()
		t.Fatalf("has_disk = %v, %v without waiting for the lifecycle that holds the disk's record", r.present, r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk neither answered nor waited")
	}
	f.client.state.configs[777]["scsi1"] = slot
	finish()
	close(release)
	select {
	case r := <-result:
		if r.err != nil || r.present != true {
			t.Errorf("has_disk after the lifecycle finished = %v, %v, want true", r.present, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("has_disk never answered after the lifecycle finished")
	}
}

// TestHasDiskAnswersAnUnmarkedDiskAtOnce covers a disk whose CID carries no
// allocation, whose birth name is gone, and which no guest carries, when no
// allocation record that isn't terminal names its token. has_disk answers
// false at once, as it always has.
func TestHasDiskAnswersAnUnmarkedDiskAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f digestManaged, token string) string
	}{
		{name: "no record names the token", setup: func(t *testing.T, f digestManaged, _ string) string {
			return "bpd-00000000000000aa"
		}},
		{name: "the record is terminal", setup: func(t *testing.T, f digestManaged, token string) string {
			handle, err := f.journal.Acquire(t.Context(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			record := handle.Record()
			record.State = aj.Deleted
			record.Verifications = append(record.Verifications, aj.Verification{EvidenceID: "delete-audit", Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true})
			if err := handle.Save(record); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			return token
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token := unmarkedFixture(t)
			delete(f.client.state.configs[777], "scsi1")
			cid := unmarkedDiskCID(t, f, tc.setup(t, f, token))
			waited := false
			ctx := withHasDiskWaitForTest(digestCtx(), hasDiskWaitSeam{limit: 5 * time.Second, waiting: func(error) { waited = true }})
			start := time.Now()
			present, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
			if err != nil || present != false {
				t.Fatalf("has_disk = %v, %v, want false", present, err)
			}
			if waited {
				t.Error("has_disk waited for a lifecycle, want it to answer at once")
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("has_disk took %s, want it to answer at once", took)
			}
		})
	}
}
