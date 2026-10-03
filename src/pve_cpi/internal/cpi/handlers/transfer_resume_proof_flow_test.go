package handlers

// A detach-side transfer whose move lands before its serial write leaves a
// parker-named volume with no serial, while the parker's record keeps the
// pre-move name. Another disk can then take that name on the source, and its
// own unfinished detach leaves it on an unused entry there. These tests build
// that state through the real handlers and the fake's own move, and they check
// that every handler whose resume could move the other disk refuses instead,
// with the other disk left where it is.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// reusedNameState is a stranded legacy disk whose interrupted resume landed it
// on its parker with no serial, and, when reused is set, a second disk that
// took the freed name on the source's unused entry.
type reusedNameState struct {
	*strandedDisk
	parker       int
	recordedSlot string
	landedSlot   string
	landed       string
	unusedKey    string
	source       map[string]any
	moves        int
}

// strandLegacyDisk strands legacy disk A on 777's unused entry through a real
// detach whose move fails, the way buildStrandedDisk does for its birth-named
// shared shape, but with no later park, so A's recorded slot stays free.
func strandLegacyDisk(t *testing.T) *strandedDisk {
	t.Helper()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	delete(client.state.volumes, original)
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	volume := "a:777/vm-777-disk-0.raw"
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
	s := &strandedDisk{deps: deps, client: client, recorder: recorder, cid: cid, token: token}

	client.moveErr = errors.New("move_disk: storage migration failed")
	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch), deps, "777", cid); err == nil {
		t.Fatal("the transfer whose move fails reported success")
	}
	client.moveErr = nil
	for _, unused := range pve.FindUnusedDiskEntries(client.state.configs[777]) {
		s.stranded = unused
	}
	if s.stranded != volume || s.intentParker() == 0 {
		t.Fatalf("the failed transfer left 777 %v and a record on parker %d, want %s on an unused entry and a record", client.state.configs[777], s.intentParker(), volume)
	}
	return s
}

// buildReusedNameState strands disk A on 777's unused entry through the real
// detach, then moves A's volume onto its parker the way PVE does, renamed for
// the parker and with no serial. onRecordedSlot picks the record's slot for the
// landing over another free slot of the parker. reused then puts a volume
// under A's old name back on 777's unused entry, which is disk B.
func buildReusedNameState(t *testing.T, onRecordedSlot, reused bool) *reusedNameState {
	t.Helper()
	s := &reusedNameState{strandedDisk: strandLegacyDisk(t)}
	s.parker = s.intentParker()
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(s.client.state.configs[s.parker]))
	var records map[string]struct {
		Slot  string `json:"slot"`
		Volid string `json:"volid"`
	}
	if err := json.Unmarshal(raw["bosh_parked_disks"], &records); err != nil {
		t.Fatalf("read parker %d's records: %v", s.parker, err)
	}
	record := records[s.token]
	if record.Slot == "" || record.Volid != s.stranded {
		t.Fatalf("parker %d's record of %s is %+v, want a slot and volid %s", s.parker, s.token, record, s.stranded)
	}
	s.recordedSlot = record.Slot
	for key, value := range s.client.state.configs[777] {
		if strings.HasPrefix(key, "unused") && fmt.Sprint(value) == s.stranded {
			s.unusedKey = key
		}
	}
	s.landedSlot = s.recordedSlot
	if !onRecordedSlot {
		parkerCfg := s.client.state.configs[s.parker]
		for i := 1; s.landedSlot == s.recordedSlot; i++ {
			if _, taken := parkerCfg[fmt.Sprintf("scsi%d", i)]; !taken && fmt.Sprintf("scsi%d", i) != s.recordedSlot {
				s.landedSlot = fmt.Sprintf("scsi%d", i)
			}
		}
	}
	if _, taken := s.client.state.configs[s.parker][s.landedSlot]; taken {
		t.Fatalf("parker %d's slot %s is taken: %v", s.parker, s.landedSlot, s.client.state.configs[s.parker])
	}
	if err := s.client.reassignVolume(777, s.unusedKey, s.parker, s.landedSlot); err != nil {
		t.Fatalf("land A on parker %d: %v", s.parker, err)
	}
	s.landed = fmt.Sprint(s.client.state.configs[s.parker][s.landedSlot])
	if reused {
		s.client.state.volumes[s.stranded] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
		s.client.state.configs[777][s.unusedKey] = s.stranded
	}
	s.source = maps.Clone(s.client.state.configs[777])
	s.moves = s.client.moveCalls
	return s
}

// requireNothingMoved fails unless no move ran, no slot carries A's serial,
// and 777's configuration is what the build left.
func (s *reusedNameState) requireNothingMoved(t *testing.T) {
	t.Helper()
	if s.client.moveCalls != s.moves {
		t.Errorf("the resume posted %d move(s), want none", s.client.moveCalls-s.moves)
	}
	if holders := s.serialHolders(); len(holders) != 0 {
		t.Errorf("slots carrying A's serial %s: %v, want none", s.token, holders)
	}
	if !reflect.DeepEqual(s.client.state.configs[777], s.source) {
		t.Errorf("777's configuration changed:\n got %v\nwant %v", s.client.state.configs[777], s.source)
	}
	if got := fmt.Sprint(s.client.state.configs[s.parker][s.landedSlot]); got != s.landed {
		t.Errorf("parker %d's %s is %q, want the landing %q left as it was", s.parker, s.landedSlot, got, s.landed)
	}
}

// requireProofRefusal fails unless err is the resume proof's refusal, which no
// retry can change.
func requireProofRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Error("the handler succeeded, want the resume's refusal")
		return
	}
	if !strings.Contains(err.Error(), "can't prove which volume is this disk") || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the resume's proof refusal naming %q", err, want)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Errorf("err = %v, want a Cloud error no retry changes", err)
	}
}

// TestResumeProofRefusesTheReusedNameInEveryHandler drives update_disk,
// detach_disk, and attach_disk on A with its landing off the recorded slot and
// B on 777's unused entry under A's old name. Before the proof each one moved
// B onto the parker and wrote A's serial on it.
func TestResumeProofRefusesTheReusedNameInEveryHandler(t *testing.T) {
	later := atProvenanceTime(strandedEpoch.Add(3 * time.Hour))
	for _, row := range []struct {
		op  string
		run func(t *testing.T, s *reusedNameState) error
	}{
		{"update_disk", func(t *testing.T, s *reusedNameState) error {
			_, err := HandleUpdateDisk(s.deps).Handle(later, overlayArgs(t, s.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
			return err
		}},
		{"detach_disk", func(t *testing.T, s *reusedNameState) error {
			return detachDiskAt(t, later, s.deps, "777", s.cid)
		}},
		{"attach_disk", func(t *testing.T, s *reusedNameState) error {
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			return attachDiskAt(t, later, s.deps, "888", s.cid)
		}},
	} {
		t.Run(row.op, func(t *testing.T) {
			s := buildReusedNameState(t, false, true)
			err := row.run(t, s)
			requireProofRefusal(t, err, fmt.Sprintf("while the record names slot %q", s.recordedSlot))
			s.requireNothingMoved(t)
			if row.op == "attach_disk" && len(s.client.state.configs[888]) != 2 {
				t.Errorf("888's configuration changed: %v", s.client.state.configs[888])
			}
		})
	}
}

// TestResumeProofClaimsTheLandingOnItsRecordedSlot is the same state with the
// landing on the slot the record names. The detach claims A's own landing and
// leaves B on 777's unused entry. Before the proof it moved B instead.
func TestResumeProofClaimsTheLandingOnItsRecordedSlot(t *testing.T) {
	s := buildReusedNameState(t, true, true)
	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(3*time.Hour)), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk: %v", err)
	}
	if s.client.moveCalls != s.moves {
		t.Errorf("the resume posted %d move(s), want none", s.client.moveCalls-s.moves)
	}
	want := map[string]string{fmt.Sprintf("%d.%s", s.parker, s.recordedSlot): s.landed}
	if holders := s.serialHolders(); !reflect.DeepEqual(holders, want) {
		t.Errorf("slots carrying A's serial %s: %v, want %v", s.token, holders, want)
	}
	if got := fmt.Sprint(s.client.state.configs[777][s.unusedKey]); got != s.stranded {
		t.Errorf("777's %s is %q, want B's %q left in place", s.unusedKey, got, s.stranded)
	}
}

// TestResumeProofControlInterruptedDetachStillResumes is the control. The
// detach's move never ran and nothing took A's name, so the retried detach
// moves A off 777's unused entry, lands it on the recorded slot, and writes
// A's serial there. It passes before and after the proof.
func TestResumeProofControlInterruptedDetachStillResumes(t *testing.T) {
	s := strandLegacyDisk(t)
	parker := s.intentParker()
	moves := s.client.moveCalls
	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(3*time.Hour)), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk: %v", err)
	}
	if s.client.moveCalls != moves+1 {
		t.Errorf("the resume posted %d move(s), want one", s.client.moveCalls-moves)
	}
	if vmid := s.requireSingleHolder(t, true); vmid != parker {
		t.Errorf("A's serial is on VM %d, want parker %d", vmid, parker)
	}
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Errorf("777 still holds unused entries %v", unused)
	}
}

// TestResumeProofRefusesAnotherDisksPendingDeleteOnAStoppedSource drives
// detach_disk and attach_disk, the two handlers whose resume applies a found
// pending delete on a stopped source. The slot whose delete is pending names
// the recorded volume with another disk's serial. Each handler refuses before
// it applies that delete, so the delete stays pending and nothing lands on the
// parker. Before the proof the resume applied it and parked the other disk
// under this disk's serial.
func TestResumeProofRefusesAnotherDisksPendingDeleteOnAStoppedSource(t *testing.T) {
	const otherSerial = "bpd-99887766ffeeddcc"
	for _, row := range []struct {
		op  string
		run func(t *testing.T, deps Deps, c *idFakeClient, diskCID string) error
	}{
		{"detach_disk", func(t *testing.T, deps Deps, _ *idFakeClient, diskCID string) error {
			return handleDetachStableID(pendingRowContext(), deps, "700", 700, resolveTransferDisk(t, deps, diskCID))
		}},
		{"attach_disk", func(t *testing.T, deps Deps, c *idFakeClient, diskCID string) error {
			c.configs[701] = map[string]any{"name": "w701", "digest": "1"}
			return attachDiskAt(t, pendingRowContext(), deps, "701", diskCID)
		}},
	} {
		t.Run(row.op, func(t *testing.T) {
			deps, c, diskCID := pendingResumeFixture(t)
			held := fmt.Sprint(c.pending.deletes[700]["scsi1"])
			c.pending.deletes[700]["scsi1"] = strings.Replace(held, "serial="+idTestToken, "serial="+otherSerial, 1)
			c.pending.stop(700)
			reverts, deletes := len(c.pending.reverts), len(c.pending.deleteCalls)

			err := row.run(t, deps, c, diskCID)
			requireProofRefusal(t, err, fmt.Sprintf("scsi1 of source vm 700 names volume %s with another disk's serial %s", pendingTransferVolid, otherSerial))
			requireLeftAlone(t, c, reverts, deletes, row.op)
			if row.op == "attach_disk" && len(c.configs[701]) != 2 {
				t.Errorf("701's configuration changed: %v", c.configs[701])
			}
		})
	}
}

// twinParkedRecord gives the parker whose configuration is cfg a second
// unfinished record that copies from's record, under a new stable ID, a new
// disk CID, and a new slot, so both records name the same source VM and
// recorded volume. It returns the new record's ID.
func twinParkedRecord(t *testing.T, cfg map[string]any, from, slot string) string {
	t.Helper()
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if disks[from] == nil {
		t.Fatalf("the parker keeps no record for %s: %v", from, disks)
	}
	id, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	volid, _ := disks[from]["volid"].(string)
	cid, err := pve.EncodeDiskCID(volid, &pve.DiskCIDMeta{ID: id, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	twin := maps.Clone(disks[from])
	twin["disk_cid"], twin["slot"] = cid, slot
	disks[id] = twin
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	return id
}

// requireAuditRefusal fails unless err is the resume proof's permanent refusal
// for two records that name the same source VM and recorded volume, as the
// caller op returns it.
func requireAuditRefusal(t *testing.T, err error, op string) {
	t.Helper()
	requireProofRefusal(t, err, "names the same source vm")
	if err != nil && !strings.Contains(err.Error(), "(audit required)") {
		t.Errorf("%s: err = %v, want the refusal to ask for an audit", op, err)
	}
}

// TestResumeProofAuditRefusalReachesEveryCaller drives every handler that
// resumes a legacy disk's transfer, with A's landing on its recorded slot and
// a second unfinished record on the parker that names A's source VM and
// recorded volume. The resume refuses for audit, and each handler returns that
// refusal as the same permanent Cloud error, with nothing moved and no serial
// written. Before the proof each one claimed the landing as A's.
func TestResumeProofAuditRefusalReachesEveryCaller(t *testing.T) {
	later := atProvenanceTime(strandedEpoch.Add(3 * time.Hour))
	call := func(t *testing.T, h Handler, args ...any) error {
		t.Helper()
		raw := make([]json.RawMessage, 0, len(args))
		for _, arg := range args {
			raw = append(raw, planJSON(t, arg))
		}
		_, err := h.Handle(later, raw, jsonrpc.Context{})
		return err
	}
	for _, row := range []struct {
		op  string
		run func(t *testing.T, s *reusedNameState) error
	}{
		{"update_disk", func(t *testing.T, s *reusedNameState) error {
			_, err := HandleUpdateDisk(s.deps).Handle(later, overlayArgs(t, s.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
			return err
		}},
		{"detach_disk", func(t *testing.T, s *reusedNameState) error {
			return detachDiskAt(t, later, s.deps, "777", s.cid)
		}},
		{"attach_disk", func(t *testing.T, s *reusedNameState) error {
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			return attachDiskAt(t, later, s.deps, "888", s.cid)
		}},
		{"resize_disk", func(t *testing.T, s *reusedNameState) error {
			return call(t, HandleResizeDisk(s.deps), s.cid, 6144)
		}},
		{"snapshot_disk", func(t *testing.T, s *reusedNameState) error {
			return call(t, HandleSnapshotDisk(s.deps), s.cid, map[string]any{})
		}},
		{"set_disk_metadata", func(t *testing.T, s *reusedNameState) error {
			return call(t, HandleSetDiskMetadata(s.deps), s.cid, map[string]any{"director": "x"})
		}},
		{"delete_disk", func(t *testing.T, s *reusedNameState) error {
			return call(t, HandleDeleteDisk(s.deps), s.cid)
		}},
	} {
		t.Run(row.op, func(t *testing.T) {
			s := buildReusedNameState(t, true, false)
			twinParkedRecord(t, s.client.state.configs[s.parker], s.token, "scsi29")
			volumes := len(s.client.state.volumes)
			requireAuditRefusal(t, row.run(t, s), row.op)
			s.requireNothingMoved(t)
			if len(s.client.state.volumes) != volumes {
				t.Errorf("%s changed the volume count from %d to %d", row.op, volumes, len(s.client.state.volumes))
			}
			if row.op == "attach_disk" && len(s.client.state.configs[888]) != 2 {
				t.Errorf("888's configuration changed: %v", s.client.state.configs[888])
			}
		})
	}
}

// TestResumeProofAuditRefusalReachesTheIdentityCheck is the same refusal for a
// journal-managed disk, whose handlers reach the resume through the identity
// check's claim-only resume. has_disk reaches it that way and no other. The
// disk landed before its serial write, and its record observed the move, so
// the claim-only resume could claim the landing, but a second unfinished
// record names the same source VM and recorded volume. The identity check
// passes the resume's permanent refusal through as it is, and nothing is
// written to PVE or the journal. Before the proof each one claimed the landing.
func TestResumeProofAuditRefusalReachesTheIdentityCheck(t *testing.T) {
	captureParkerPoolSweep(t)
	call := func(t *testing.T, h Handler, args ...any) error {
		t.Helper()
		raw := make([]json.RawMessage, 0, len(args))
		for _, arg := range args {
			raw = append(raw, planJSON(t, arg))
		}
		_, err := h.Handle(digestCtx(), raw, jsonrpc.Context{})
		return err
	}
	for _, row := range []struct {
		op  string
		run func(t *testing.T, f digestManaged) error
	}{
		{"has_disk", func(t *testing.T, f digestManaged) error {
			return call(t, HandleHasDisk(f.deps), f.cid)
		}},
		{"update_disk", func(t *testing.T, f digestManaged) error {
			_, err := HandleUpdateDisk(f.deps).Handle(digestCtx(), overlayArgs(t, f.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
			return err
		}},
		{"detach_disk", func(t *testing.T, f digestManaged) error {
			return digestDetach(t, f.deps, f.cid)
		}},
		{"attach_disk", func(t *testing.T, f digestManaged) error {
			return digestAttach(t, f.deps, f.cid)
		}},
		{"resize_disk", func(t *testing.T, f digestManaged) error {
			return call(t, HandleResizeDisk(f.deps), f.cid, 6144)
		}},
		{"snapshot_disk", func(t *testing.T, f digestManaged) error {
			return call(t, HandleSnapshotDisk(f.deps), f.cid, map[string]any{})
		}},
		{"set_disk_metadata", func(t *testing.T, f digestManaged) error {
			return call(t, HandleSetDiskMetadata(f.deps), f.cid, map[string]any{"director": "x"})
		}},
		{"delete_disk", func(t *testing.T, f digestManaged) error {
			return call(t, HandleDeleteDisk(f.deps), f.cid)
		}},
	} {
		t.Run(row.op, func(t *testing.T) {
			f := digestManagedFixture(t)
			parker, token, _ := landedBeforeSerial(t, f)
			twinParkedRecord(t, f.client.state.configs[parker], token, "scsi29")
			writes := flowFakeWrites(t, f.client)
			journal := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
			requireAuditRefusal(t, row.run(t, f), row.op)
			requireTransferUnfinished(t, f, parker, token)
			if after := flowFakeWrites(t, f.client); after != writes {
				t.Errorf("%s wrote to PVE:\n got %s\nwant %s", row.op, after, writes)
			}
			if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journal) {
				t.Errorf("%s changed the allocation journal", row.op)
			}
		})
	}
}
