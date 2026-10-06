package handlers

// These rows cover the detach tail where a parked disk leaves the parker or
// is deleted. attach_disk and create_vm's pre-attach run the tail before they
// move the disk off the parker, a parker lock timeout that follows the tail's
// write in delete_disk stays clean, a read with no digest sends no write, and a
// refused write settles only while 777 still carries every note it meant to
// remove.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// countMoveSteps counts the record's disk move steps, on any VM.
func countMoveSteps(t *testing.T, journal *aj.Journal, id string) int {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i := range record.Steps {
		if strings.HasSuffix(record.Steps[i].Kind, "_Nodes_CreateQemuMoveDisk") {
			count++
		}
	}
	return count
}

// TestDetachTailRunsBeforeAttachLeavesTheParker is a latent record whose disk
// attach_disk takes off the parker onto 888. Every write the tail sends to 777
// meets a changed config, so the attach fails retriably before it moves the
// disk, which stays parked with 777's entry in place. Once 777 settles down,
// the retried attach removes the entry and then moves the disk to 888.
func TestDetachTailRunsBeforeAttachLeavesTheParker(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	parker := s.requireSingleHolder(t, true)
	moves := countMoveSteps(t, s.journal, id)
	w.conflictAlways = true
	err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	requireRetriable(t, err, "attach_disk whose tail can't remove 777's entry")
	if w.conflicts != 3 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want three refused writes and no removal", w.conflicts, w.removals)
	}
	if after := countMoveSteps(t, s.journal, id); after != moves {
		t.Fatalf("the failed attach journaled %d moves, want none", after-moves)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the disk moved to VM %d although the tail failed, want it on parker %d", vmid, parker)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although every write was refused")
	}
	if s.hasEntry(888) {
		t.Fatal("888 carries the disk's entry although the attach never moved it")
	}
	// Every refused write is settled and nothing else touched the disk, so
	// the lifecycle returns the record the way a success would.
	s.requireRecord(t, id, aj.ReadyToReturn)

	w.conflictAlways = false
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("the retried attach_disk: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("the disk landed on VM %d, want 888", vmid)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
	s.requireSourceClean(t)
	if !s.hasEntry(888) {
		t.Fatal("888 carries no allocation entry for the disk it holds")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailRunsBeforeCreateVMLeavesTheParker is the same latent record
// handed to create_vm's managed pre-attach for 888. The pre-attach shares
// attach_disk's guard, so a tail that can't remove 777's entry fails it
// retriably before the transfer, with the disk still parked and its record
// returned. The retried pre-attach removes the entry and then moves the disk.
func TestDetachTailRunsBeforeCreateVMLeavesTheParker(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	parker := s.requireSingleHolder(t, true)
	moves := countMoveSteps(t, s.journal, id)
	resolve := func() resolvedDisk {
		t.Helper()
		bare, meta, err := decodeDiskCID(context.Background(), s.deps, "create_vm", s.cid)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := resolveDiskForOp(context.Background(), s.deps, "create_vm", s.cid, bare, meta)
		if err != nil {
			t.Fatal(err)
		}
		return rd
	}
	w.conflictAlways = true
	_, err := attachManagedPersistentDisk(context.Background(), s.deps, "888", "n1", 888, resolve())
	requireRetriable(t, err, "create_vm pre-attach whose tail can't remove 777's entry")
	if w.conflicts != 3 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want three refused writes and no removal", w.conflicts, w.removals)
	}
	if after := countMoveSteps(t, s.journal, id); after != moves {
		t.Fatalf("the failed pre-attach journaled %d moves, want none", after-moves)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the disk moved to VM %d although the tail failed, want it on parker %d", vmid, parker)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although every write was refused")
	}
	// Every refused write is settled and nothing else touched the disk, so
	// the lifecycle returns the record the way a success would.
	s.requireRecord(t, id, aj.ReadyToReturn)

	w.conflictAlways = false
	if _, err := attachManagedPersistentDisk(context.Background(), s.deps, "888", "n1", 888, resolve()); err != nil {
		t.Fatalf("the retried pre-attach: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("the disk landed on VM %d, want 888", vmid)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// tailLockedPVE is the tail fixture with every bosh-lock- sentinel kept in a
// shared store, so a test can hold the parker's lock for another request.
type tailLockedPVE struct {
	tailWritePVE
	locks *lockContention
}

func (c tailLockedPVE) Pools() pve.PoolService {
	return contendedPools{PoolService: c.tailWritePVE.Pools(), locks: c.locks}
}

// TestDetachTailThenParkerLockTimeoutStaysClean is a latent record whose
// delete_disk removes 777's entry and then waits out the parker's lock, which
// another request holds. The tail's write changed only 777's notes, so the
// timeout comes back retriable with the record returned and the volume in
// place, and the rerun deletes the disk once the lock is free.
func TestDetachTailThenParkerLockTimeoutStaysClean(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	locks := newLockContention(t)
	s.deps.PVE = tailLockedPVE{tailWritePVE: s.deps.PVE.(tailWritePVE), locks: locks}
	parker := s.requireSingleHolder(t, true)
	plantHeldParkerLock(locks, parker)

	err := deleteDiskAt(t, shortenManagedLockWait(context.Background(), testManagedLockWait), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk that times out on the parker lock after the tail")
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("err = %v, want the parker lock timeout", err)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once before the wait", w.removals)
	}
	s.requireSourceClean(t)
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although it never took the parker lock", s.stranded)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)

	locks.reset()
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk once the parker lock is free: %v", err)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want the rerun to find nothing left", w.removals)
	}
	s.requireRecord(t, id, aj.Deleted)
	if s.client.state.volumes[s.stranded] != nil {
		t.Fatalf("%s survived delete_disk", s.stranded)
	}
}

// noDigestTailPVE serves 777's pending view without its digest row while
// drop is set, and counts the description writes 777 gets meanwhile.
type noDigestTailPVE struct {
	tailWritePVE
	drop   *bool
	writes *int
}

func (c noDigestTailPVE) Nodes() nodes.Service {
	return noDigestTailNodes{Service: c.tailWritePVE.Nodes(), drop: c.drop, writes: c.writes}
}

type noDigestTailNodes struct {
	nodes.Service
	drop   *bool
	writes *int
}

func (n noDigestTailNodes) ListQemuPending(ctx context.Context, node, vmidText string) (*nodes.ListQemuPendingResponse, error) {
	resp, err := n.Service.ListQemuPending(ctx, node, vmidText)
	if err != nil || resp == nil || vmidText != "777" || !*n.drop {
		return resp, err
	}
	rows := nodes.ListQemuPendingResponse{}
	for _, raw := range *resp {
		var item struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Key == "digest" {
			continue
		}
		rows = append(rows, raw)
	}
	return &rows, nil
}

func (n noDigestTailNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	if vmid, err := strconv.Atoi(vmidText); err == nil && vmid == 777 && p.Description != nil && *n.drop {
		*n.writes++
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmidText, p)
}

// TestDetachTailReadWithoutDigestSendsNothing is a latent record whose
// delete_disk reads 777's pending view without a digest. The tail has nothing
// to pin its write to, so it refuses retriably, sends no description write,
// and leaves the volume and 777's entry in place. Once the read carries the
// digest again, the rerun deletes the disk.
func TestDetachTailReadWithoutDigestSendsNothing(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	drop, writes := true, 0
	s.deps.PVE = noDigestTailPVE{tailWritePVE: s.deps.PVE.(tailWritePVE), drop: &drop, writes: &writes}

	err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk whose tail read carries no digest")
	if writes != 0 || w.removals != 0 {
		t.Fatalf("777 got %d description writes and %d removals, want none", writes, w.removals)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although no write was sent")
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail refused", s.stranded)
	}

	drop = false
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk once the read carries a digest: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// seedTailNotes files the attached-disk entry and overlay an attach writes for
// the disk onto 777, beside the allocation entry the latent record keeps.
func (s *strandedDisk) seedTailNotes(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// UpdateAttachedDiskCID returns no error, because an attach treats the
	// note as best effort, so the check below that both carriers file the
	// disk is what fails the row when the seed write doesn't land.
	pve.UpdateAttachedDiskCID(ctx, s.client, nil, "n1", 777, s.token, s.cid)
	if err := pve.SetVMDiskOptOverlay(ctx, s.client, "n1", 777, s.token, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(s.client.state.configs[777]))
	for _, carrier := range []string{"bosh_attached_disks", pve.DiskOptOverlaysSentinelKey} {
		if !strings.Contains(string(raw[carrier]), s.token) {
			t.Fatalf("777's %s doesn't file the disk: %s", carrier, raw[carrier])
		}
	}
}

// changeTailNotes is another writer changing 777 after the tail read it and
// before PVE looks at the tail's write. It moves 777's digest on, and when
// dropNotes is set it also removes the disk's attached-disk entry and overlay
// while leaving its allocation entry.
func (s *strandedDisk) changeTailNotes(t *testing.T, dropNotes bool) {
	t.Helper()
	if dropNotes {
		s.dropTailNotes(t)
	}
	s.client.generation++
	s.client.state.configs[777]["digest"] = fmt.Sprint(s.client.generation + 100)
}

// dropTailNotes removes the disk's attached-disk entry and overlay from 777
// and leaves its allocation entry and 777's digest as they are.
func (s *strandedDisk) dropTailNotes(t *testing.T) {
	t.Helper()
	cfg := s.client.state.configs[777]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	for _, carrier := range []string{"bosh_attached_disks", pve.DiskOptOverlaysSentinelKey} {
		var notes map[string]json.RawMessage
		if err := json.Unmarshal(raw[carrier], &notes); err != nil {
			t.Fatal(err)
		}
		delete(notes, s.token)
		if len(notes) == 0 {
			delete(raw, carrier)
			continue
		}
		encoded, err := json.Marshal(notes)
		if err != nil {
			t.Fatal(err)
		}
		raw[carrier] = encoded
	}
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
}

// refuseFirstTailWrite returns a context whose tail hook has another writer
// change 777 just before PVE looks at the first description write that
// delete_disk's lifecycle sends to it, so PVE refuses that write.
func (s *strandedDisk) refuseFirstTailWrite(t *testing.T, id string, dropNotes bool) (context.Context, *int) {
	t.Helper()
	changed := 0
	return withTailHook(context.Background(), func(event tailEvent) {
		if event == tailWrite && changed == 0 && s.deleteAdmitted(id) {
			changed++
			s.changeTailNotes(t, dropNotes)
		}
	}), &changed
}

// TestDetachTailRefusalWithEveryNoteKeptSettles is a control. PVE refuses the
// tail's write for a stale digest, and 777 still carries the allocation entry,
// the attached-disk entry, and the overlay the write meant to remove. The
// refusal is settled, the tail removes all three from a fresh read, and
// delete_disk ends Deleted.
func TestDetachTailRefusalWithEveryNoteKeptSettles(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	s.seedTailNotes(t)
	ctx, changed := s.refuseFirstTailWrite(t, id, false)
	if err := deleteDiskAt(t, ctx, s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk whose first tail write PVE refused: %v", err)
	}
	if *changed != 1 || w.conflicts != 1 || w.removals != 1 {
		t.Fatalf("changed=%d conflicts=%d removals=%d, want one refused write and one removal", *changed, w.conflicts, w.removals)
	}
	if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 0 {
		t.Fatalf("%d of 777's config steps were left planned, want the refusal settled", planned)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailRefusalWithNotesGoneStillSettles is a refused tail write
// after which 777 still carries the allocation entry but no longer carries the
// attached-disk entry or the overlay the write meant to remove. The tail built
// its write from a read at the digest it sent, and PVE checks that digest under
// 777's lock before it writes, so the refusal alone shows the write changed
// nothing, whatever another writer removed since. The refusal is settled, the
// tail removes the entry that is left from a fresh read, and delete_disk ends
// Deleted.
func TestDetachTailRefusalWithNotesGoneStillSettles(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	s.seedTailNotes(t)
	ctx, changed := s.refuseFirstTailWrite(t, id, true)
	if err := deleteDiskAt(t, ctx, s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk whose refused write's notes were gone on the readback: %v", err)
	}
	if *changed != 1 || w.conflicts != 1 || w.removals != 1 {
		t.Fatalf("changed=%d conflicts=%d removals=%d, want one refused write and one removal", *changed, w.conflicts, w.removals)
	}
	if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 0 {
		t.Fatalf("%d of 777's config steps were left planned, want the refusal settled", planned)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// configSteps counts the record's config steps on any VM, planned or
// observed.
func configSteps(t *testing.T, journal *aj.Journal, id string) int {
	t.Helper()
	planned, observed := countSteps(t, journal, id, "_Nodes_UpdateQemuConfig")
	return planned + observed
}

// pveAnswer is the error the SDK returns for a PVE answer with status code
// and message.
func pveAnswer(code int, message string) error {
	body, _ := json.Marshal(map[string]string{"message": message})
	return sdkerrors.ParseAPIError(code, body)
}

// TestDetachTailLockedSourceSendsNothing is a latent record whose delete_disk
// finds a lock key on 777, as a backup leaves it. PVE would refuse the write,
// so the tail refuses retriably before it journals or sends one, the record is
// returned, and the volume and 777's entry stay. Once the lock is gone, the
// rerun deletes the disk.
func TestDetachTailLockedSourceSendsNothing(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	parker := s.requireSingleHolder(t, true)
	steps := configSteps(t, s.journal, id)
	s.client.state.configs[777]["lock"] = "backup"

	err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk while 777 is locked")
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
	}
	if w.conflicts != 0 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want no write sent to 777", w.conflicts, w.removals)
	}
	if after := configSteps(t, s.journal, id); after != steps {
		t.Fatalf("the refused delete_disk journaled %d config steps, want none", after-steps)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although no write was sent")
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail refused", s.stranded)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)

	delete(s.client.state.configs[777], "lock")
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk once 777 is unlocked: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailAnsweredRefusalWithEveryNoteKeptSettles is a latent record
// whose delete_disk gets an answered refusal for the tail's write, either a
// 403 or a 500 saying 777 is locked. 777 still carries every note the write
// meant to remove, so the refusal is settled, the record is returned, and the
// volume stays. Once PVE takes the write, the rerun deletes the disk.
func TestDetachTailAnsweredRefusalWithEveryNoteKeptSettles(t *testing.T) {
	for name, answer := range map[string]error{
		"forbidden": pveAnswer(403, "Permission check failed (/vms/777, VM.Config.Options)"),
		"locked":    pveAnswer(500, "VM 777 is locked (backup)"),
	} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildLatentTailDisk(t)
			parker := s.requireSingleHolder(t, true)
			s.seedTailNotes(t)
			w.failWith = answer

			err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
			requireRetriable(t, err, "delete_disk whose tail write PVE refused")
			if vmid := s.requireSingleHolder(t, true); vmid != parker {
				t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
			}
			if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 0 {
				t.Fatalf("%d of 777's config steps were left planned, want the refusal settled", planned)
			}
			if !s.hasEntry(777) || w.removals != 0 {
				t.Fatalf("777's entry present=%t removals=%d, want it left in place", s.hasEntry(777), w.removals)
			}
			if s.client.state.volumes[s.stranded] == nil {
				t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
			}
			s.requireRecord(t, id, aj.ReadyToReturn)

			w.failWith = nil
			if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
				t.Fatalf("delete_disk once PVE takes the write: %v", err)
			}
			s.requireRecord(t, id, aj.Deleted)
			s.requireSourceClean(t)
		})
	}
}

// TestDetachTailAnsweredRefusalWithNoteGoneStaysUnsettled is a latent record
// whose delete_disk gets a 403 for the tail's write, after another writer
// removed the disk's attached-disk entry and overlay from 777 without moving
// its digest on. The readback can't show that the refused write changed
// nothing, so the refusal isn't settled, the record is left for
// reconciliation, and the volume stays.
func TestDetachTailAnsweredRefusalWithNoteGoneStaysUnsettled(t *testing.T) {
	// The helper asserts the row, and the marker test reads the error it
	// returns.
	_ = answeredRefusalWithNoteGone(t)
}

// answeredRefusalWithNoteGone runs that row's delete_disk, checks what the
// row asserts, and returns the call's error.
func answeredRefusalWithNoteGone(t *testing.T) error {
	t.Helper()
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	s.seedTailNotes(t)
	w.failWith = pveAnswer(403, "Permission check failed (/vms/777, VM.Config.Options)")
	dropped := 0
	ctx := withTailHook(context.Background(), func(event tailEvent) {
		if event == tailWrite && dropped == 0 && s.deleteAdmitted(id) {
			dropped++
			s.dropTailNotes(t)
		}
	})

	err := deleteDiskAt(t, ctx, s.deps, s.cid)
	requirePermanent(t, err, "delete_disk whose refused write's notes were gone on the readback")
	if dropped != 1 || w.removals != 0 {
		t.Fatalf("dropped=%d removals=%d, want the notes dropped once and no removal", dropped, w.removals)
	}
	if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 1 {
		t.Fatalf("%d of 777's config steps were left planned, want the refused write unsettled", planned)
	}
	s.requireRecord(t, id, aj.ReconciliationRequired)
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although PVE refused the write")
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
	}
	return err
}

// TestDetachTailUnansweredRefusalStillPoisonsDelete is a latent record whose
// delete_disk gets an answer for the tail's write that may hide a write that
// landed, a 502 from a gateway or a 500 that isn't a lock refusal. The guard
// is poisoned, the write's step stays planned, the record is left for
// reconciliation, and the volume stays.
func TestDetachTailUnansweredRefusalStillPoisonsDelete(t *testing.T) {
	for name, answer := range map[string]error{
		"gateway":   pveAnswer(502, "Bad Gateway"),
		"other 500": pveAnswer(500, "unable to write config file"),
	} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildLatentTailDisk(t)
			s.seedTailNotes(t)
			w.failWith = answer

			err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
			requirePermanent(t, err, "delete_disk whose tail write went unanswered")
			if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 1 {
				t.Fatalf("%d of 777's config steps were left planned, want the unanswered write left planned", planned)
			}
			s.requireRecord(t, id, aj.ReconciliationRequired)
			if s.client.state.volumes[s.stranded] == nil {
				t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
			}
		})
	}
}

// TestDetachTailRefusedOnParkedDetachReturnsTheDisk is a latent record whose
// rerun detach_disk finds the disk already parked and 777 locked. The tail
// refuses before it sends anything, so the call comes back retriable with the
// record returned and the disk on its parker. Once the lock is gone, the next
// detach_disk removes 777's entry.
func TestDetachTailRefusedOnParkedDetachReturnsTheDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	parker := s.requireSingleHolder(t, true)
	s.client.state.configs[777]["lock"] = "backup"

	err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	requireRetriable(t, err, "detach_disk of a parked disk while 777 is locked")
	if w.conflicts != 0 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want no write sent to 777", w.conflicts, w.removals)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the disk moved to VM %d, want it on parker %d", vmid, parker)
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("%s went missing although the tail refused", s.stranded)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although no write was sent")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)

	delete(s.client.state.configs[777], "lock")
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk once 777 is unlocked: %v", err)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailRefusedAfterMoveStaysForReconciliation is a detach that moves
// the disk to the parker and then has every removal of 777's entry refused for
// a stale digest. The call changed the disk, so the refusal doesn't hand the
// allocation back, and the record is left for reconciliation with the disk on
// the parker. The call's error is retriable, and once the refusals stop, the
// Director's retry of the same detach_disk reads 777 again, removes the entry,
// and settles the record.
func TestDetachTailRefusedAfterMoveStaysForReconciliation(t *testing.T) {
	// The helper asserts the row, and the marker test reads the error it
	// returns.
	_ = refusedAfterMove(t)
}

// refusedAfterMove runs that row's detach_disk and its rerun, checks what the
// row asserts, and returns the first call's error.
func refusedAfterMove(t *testing.T) error {
	t.Helper()
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	w.conflictAlways = true

	err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	if err == nil {
		t.Fatal("detach_disk succeeded although every removal was refused")
	}
	requireRetriable(t, err, "detach_disk whose removal was refused after it moved the disk")
	if w.conflicts != 3 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want three refused writes and no removal", w.conflicts, w.removals)
	}
	parker := s.requireSingleHolder(t, true)
	s.requireRecord(t, id, aj.ReconciliationRequired)

	// The refusal was answered, so the record left no planned step, and the
	// Director's retry of the same call finds the disk parked and finishes the
	// tail.
	w.conflictAlways = false
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("the rerun detach_disk once the refusals stopped: %v", err)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once by the rerun", w.removals)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Fatalf("the rerun moved the disk to VM %d, want it still on parker %d", vmid, parker)
	}
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
	return err
}
