package handlers

// A stable-ID disk whose volume isn't named for the VM holding it loses every
// reference when its slot is deleted, because PVE keeps an unused entry only
// for a volume the VM owns (vm_is_volid_owner in vmconfig_register_unused_drive).
// The transfer to a parker now branches on what PVE left after the slot
// delete, and for such a volume it attaches it to the parker by config edit.
// These rows run through the real handlers with PVE-faithful demotion.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// unownedVolume is the probe's volume, named for VMID 9005, which is no guest
// in these rows.
const unownedVolume = "a:9005/vm-9005-disk-0.raw"

// buildUnownedDisk puts one stable-ID disk whose volume 777 doesn't own on
// 777's scsi1. A legacy disk uses unownedVolume, which is the state the probe's
// cross-node config-edit attach left on 777. A managed disk keeps the flow
// fixture's journal-managed volume, whose name carries the disk-band VMID 123.
// local puts the volumes on node-local storage. A second legacy disk whose
// volume 778 doesn't own sits on 778, for the rows that need an unrelated park
// on the same parker.
func buildUnownedDisk(t *testing.T, managed, local bool) (s *strandedDisk, id, otherCID string) {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	if local {
		client.localStorage = true
		client.volumeNodes = map[string]string{}
	}
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	s = &strandedDisk{client: client, recorder: recorder, journal: journal, managed: managed}
	legacyDisk := func(vmid int, volume string) (string, string) {
		token, err := pve.GenerateDiskStableID()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
		if err != nil {
			t.Fatal(err)
		}
		client.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
		if local {
			client.volumeNodes[volume] = "n1"
		}
		if client.state.configs[vmid] == nil {
			client.state.configs[vmid] = map[string]any{"name": fmt.Sprintf("w%d", vmid), "digest": "1"}
		}
		client.state.configs[vmid]["scsi1"] = volume + ",serial=" + token + ",size=5G"
		return encoded, token
	}
	if managed {
		record, err := journal.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		s.cid, s.token = cid, record.DiskToken
		if local {
			client.volumeNodes[strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]] = "n1"
		}
	} else {
		original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
		delete(client.state.volumes, original)
		delete(client.state.configs[777], "scsi1")
		deps.Config.StoragePlacementNamespace = ""
		deps.Config.StorageAllocationJournalDir = ""
		s.cid, s.token = legacyDisk(777, unownedVolume)
	}
	otherCID, _ = legacyDisk(778, "a:9006/vm-9006-disk-0.raw")
	if local {
		deps.Resolver = pve.NewBackendResolver(deps.PVE, nil, "n1")
	}
	s.deps = deps
	s.stranded = strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	return s, id, otherCID
}

// requireParkedUnderItsOwnName checks that the disk sits on one parker slot
// with its serial, still under the name it had on 777, and that 777 names it
// nowhere.
func (s *strandedDisk) requireParkedUnderItsOwnName(t *testing.T) {
	t.Helper()
	s.requireSingleHolder(t, true)
	for slot, volume := range s.serialHolders() {
		if volume != s.stranded {
			t.Fatalf("the parker slot %s names %s, want the volume's own name %s", slot, volume, s.stranded)
		}
	}
	for key, value := range s.client.state.configs[777] {
		if isDiskOptionKey(key) && strings.Split(fmt.Sprint(value), ",")[0] == s.stranded {
			t.Fatalf("777 still names %s on %s", s.stranded, key)
		}
	}
}

// TestUnownedDiskParksOnTheFirstDetach covers the probe's shape on shared and
// node-local storage, and a journal-managed disk with its usual disk-band
// name. Before the fix the first detach_disk failed, because the transfer
// expected an unused entry that PVE never keeps for a volume the VM doesn't
// own. Now it succeeds, with one reference on a parker and no retry, and the
// managed disk's record ends ready to return with no poisoned guard.
func TestUnownedDiskParksOnTheFirstDetach(t *testing.T) {
	for _, shape := range []struct {
		name           string
		managed, local bool
	}{
		{"legacy/shared", false, false},
		{"legacy/local", false, true},
		{"managed/shared", true, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, _ := buildUnownedDisk(t, shape.managed, shape.local)
			if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
				t.Fatalf("first detach_disk of a volume 777 doesn't own: %v", err)
			}
			s.requireParkedUnderItsOwnName(t)
			if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
				t.Fatalf("777 holds unused entries %v", unused)
			}
			if shape.managed {
				record, err := s.journal.Inspect(id)
				if err != nil {
					t.Fatal(err)
				}
				if record.State != aj.ReadyToReturn {
					t.Fatalf("managed record state = %s, want %s", record.State, aj.ReadyToReturn)
				}
			}
		})
	}
}

// TestUnownedDiskCrashBetweenSlotDeleteAndParkerAttach covers a transfer cut
// off after the slot delete and before the parker attach. No guest names the
// volume, so the parker's record is its only link. The record survives a
// collecting park two hours later, and the retried detach_disk resumes it to
// one reference. Before the fix the record was collected, because the keep
// rule held a record only while its source named the volume.
func TestUnownedDiskCrashBetweenSlotDeleteAndParkerAttach(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, otherCID := buildUnownedDisk(t, false, false)
	s.client.state.parkErr = errors.New("parker attach failed")
	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch), s.deps, "777", s.cid); err == nil {
		t.Fatal("the transfer whose parker attach fails reported success")
	}
	s.client.state.parkErr = nil
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("after the failed attach %s is still referenced by %v, want no reference", s.stranded, refs)
	}
	if s.intentParker() == 0 {
		t.Fatal("the failed transfer left no record on any parker")
	}

	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(2*time.Hour)), s.deps, "778", otherCID); err != nil {
		t.Fatalf("park an unrelated disk on the same parker two hours later: %v", err)
	}
	if s.intentParker() == 0 {
		t.Fatalf("the transfer record for %s was collected while no guest names %s", s.token, s.stranded)
	}
	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(3*time.Hour)), s.deps, "777", s.cid); err != nil {
		t.Fatalf("the retried detach_disk failed: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
}

// withSnapshot gives 777 one snapshot, before, whose config names volume on
// scsi1, or names nothing when volume is empty, and lets disk operations go
// ahead while 777 has snapshots.
func (s *strandedDisk) withSnapshot(volume string) {
	s.deps.Config.AllowDiskOpsWithSnapshots = true
	s.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "current"}, {"name": "before"}}}
	cfg := map[string]any{"name": "w777"}
	if volume != "" {
		cfg["scsi1"] = volume + ",size=5G"
	}
	s.client.snapshotConfigs = map[int]map[string]map[string]any{777: {"before": cfg}}
}

// TestUnownedDiskSnapshotDefersThePark covers a snapshot of 777 that still
// names the volume. A config-edit attach has no snapshot check, so without one
// the parker would take a volume that a rollback puts back on 777. The first
// detach_disk succeeds with the park deferred and no parker slot, the way it
// does for an owned volume whose move PVE refuses, and the first call after
// the snapshot is deleted parks the disk with one reference.
func TestUnownedDiskSnapshotDefersThePark(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.withSnapshot(s.stranded)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk with a snapshot naming the volume: %v", err)
	}
	if holders := s.serialHolders(); len(holders) != 0 {
		t.Fatalf("slots carrying the serial while a snapshot names the volume: %v, want none", holders)
	}
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("%s is referenced by %v, want the park deferred with no reference", s.stranded, refs)
	}

	s.client.vmSnapshots, s.client.snapshotConfigs = nil, nil
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk after the snapshot was deleted: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
}

// TestUnownedDiskSnapshotThatDoesNotNameTheVolume covers a snapshot of 777
// that names other volumes only. It doesn't block the park, which happens at
// once.
func TestUnownedDiskSnapshotThatDoesNotNameTheVolume(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.withSnapshot("a:777/vm-777-disk-0.raw")
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk with a snapshot that doesn't name the volume: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
}

// TestUnownedDiskSnapshotReadErrorAttachesNothing covers a snapshot listing
// that fails while the transfer weighs the config-edit attach. Going ahead on
// an unknown risks a second reference, so the transfer attaches nothing and
// detach_disk fails retriably.
func TestUnownedDiskSnapshotReadErrorAttachesNothing(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.deps.Config.AllowDiskOpsWithSnapshots = true
	s.client.snapshotErr = errors.New("snapshot listing failed")
	// detach_disk reads the snapshots before the transfer too, and
	// require_snapshot_check_pass off lets it go ahead on a failed read, so
	// the failure reaches the transfer's own check.
	s.deps.Config.RequireSnapshotCheckPass = false
	err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	requireRetriable(t, err, "detach_disk")
	requireText(t, err, "detach_disk", []string{"snapshots of source vm 777", "nothing was attached"})
	if holders := s.serialHolders(); len(holders) != 0 {
		t.Fatalf("slots carrying the serial after a snapshot read error: %v, want none", holders)
	}
}

// TestUnownedDiskResumeOfAnOldIntentWaitsForTheSnapshot covers the intent
// record 0.8.0 leaves when the first transfer of a volume the source doesn't
// own fails after the slot delete, with nothing naming the volume. A snapshot
// of the source still names it. The resume attaches nothing, and detach_disk
// reports the park as deferred.
func TestUnownedDiskResumeOfAnOldIntentWaitsForTheSnapshot(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.client.state.parkErr = errors.New("parker attach failed")
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err == nil {
		t.Fatal("the transfer whose parker attach fails reported success")
	}
	s.client.state.parkErr = nil
	if s.intentParker() == 0 || len(s.references(s.stranded)) != 0 {
		t.Fatalf("setup: want an intent record and no reference, have parker %d and references %v", s.intentParker(), s.references(s.stranded))
	}
	s.withSnapshot(s.stranded)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk through the resume with a snapshot naming the volume: %v", err)
	}
	if holders := s.serialHolders(); len(holders) != 0 {
		t.Fatalf("slots carrying the serial while a snapshot names the volume: %v, want none", holders)
	}
	if s.intentParker() == 0 {
		t.Fatal("the deferred park lost its record")
	}
}

// TestOwnedDiskStillTakesTheMovePath is a control. A volume named for 777
// leaves an unused entry when its slot is deleted, so the transfer moves it
// and the parker renames it, as before.
func TestOwnedDiskStillTakesTheMovePath(t *testing.T) {
	captureParkerPoolSweep(t)
	s := buildBirthStrandOwnedReady(t)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk of an owned volume: %v", err)
	}
	vmid := s.requireSingleHolder(t, true)
	for _, volume := range s.serialHolders() {
		if owner, ok := pve.EmbeddedDiskVMID(volume); !ok || owner != vmid {
			t.Fatalf("the parked volume is %s, want it renamed for parker %d", volume, vmid)
		}
	}
}

// buildBirthStrandOwnedReady is a legacy stable-ID disk named for 777 on 777's
// scsi1, before any detach.
func buildBirthStrandOwnedReady(t *testing.T) *strandedDisk {
	t.Helper()
	s, _, _ := buildUnownedDisk(t, false, false)
	owned := "a:777/vm-777-disk-0.raw"
	s.client.state.volumes[owned] = s.client.state.volumes[s.stranded]
	delete(s.client.state.volumes, s.stranded)
	cid, err := pve.EncodeDiskCID(owned, &pve.DiskCIDMeta{ID: s.token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	s.client.state.configs[777]["scsi1"] = owned + ",serial=" + s.token + ",size=5G"
	s.cid, s.stranded = cid, owned
	return s
}

// TestUnownedDiskSlotStillNamedStopsTheTransfer is a control. On a running VM
// whose hotplug setting lacks disk, the slot delete stays pending, so a slot
// still names the volume, and the transfer stops with the pending-delete error
// before either branch, attaching nothing.
func TestUnownedDiskSlotStillNamedStopsTheTransfer(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.client.state.configs[777]["hotplug"] = "network,usb"
	s.client.pending = newFakePendingModel()
	s.client.pending.run(777)
	err := detachDiskAt(t, pendingRowContext(), s.deps, "777", s.cid)
	requirePermanent(t, err, "detach_disk")
	requireText(t, err, "detach_disk", []string{"VM 777", `"network,usb"`, "scsi1"})
	for slot := range s.serialHolders() {
		if !strings.HasPrefix(slot, "777.") {
			t.Fatalf("the serial landed on %s, want it only on 777", slot)
		}
	}
}

// requireNothingOnAParker checks that no slot outside 777 carries the disk's
// serial and that no parker names its volume on any key.
func (s *strandedDisk) requireNothingOnAParker(t *testing.T) {
	t.Helper()
	for slot := range s.serialHolders() {
		if !strings.HasPrefix(slot, "777.") {
			t.Fatalf("the serial landed on %s, want nothing attached to a parker", slot)
		}
	}
	for vmid, cfg := range s.client.state.configs {
		tags, _ := cfg["tags"].(string)
		if !strings.Contains(tags, pve.ParkerTag) {
			continue
		}
		for key, value := range cfg {
			if isDiskOptionKey(key) && strings.Split(fmt.Sprint(value), ",")[0] == s.stranded {
				t.Fatalf("parker %d names %s on %s", vmid, s.stranded, key)
			}
		}
	}
}

// TestUnownedDiskAnotherKeyStillNamingTheVolumeStopsTheTransfer covers a
// second key of 777 that names the volume after the transfer deleted the slot
// carrying the serial. Something other than this transfer holds the volume
// there, so the transfer attaches nothing and fails retriably, naming that
// key. On the managed path that refusal comes after the guard admitted the
// intent write and the slot delete, so the record requires reconciliation.
func TestUnownedDiskAnotherKeyStillNamingTheVolumeStopsTheTransfer(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "managed"}[managed], func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, _ := buildUnownedDisk(t, managed, false)
			s.client.state.configs[777]["scsi2"] = s.stranded + ",size=5G"
			err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
			requireRetriable(t, err, "detach_disk")
			requireText(t, err, "detach_disk", []string{"source vm 777 still names", "scsi2", "after its slot delete"})
			s.requireNothingOnAParker(t)
			if managed {
				record, err := s.journal.Inspect(id)
				if err != nil {
					t.Fatal(err)
				}
				if record.State != aj.ReconciliationRequired {
					t.Fatalf("managed record state = %s, want %s", record.State, aj.ReconciliationRequired)
				}
			}
		})
	}
}

// transferRecords counts the parker records keyed by the disk's stable ID that
// name volume.
func (s *strandedDisk) transferRecords(t *testing.T, volume string) int {
	t.Helper()
	count := 0
	for _, cfg := range s.client.state.configs {
		tags, _ := cfg["tags"].(string)
		if !strings.Contains(tags, pve.ParkerTag) {
			continue
		}
		_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
		var disks map[string]struct {
			Volid string `json:"volid"`
		}
		if json.Unmarshal(raw["bosh_parked_disks"], &disks) != nil {
			continue
		}
		if entry, ok := disks[s.token]; ok && entry.Volid == volume {
			count++
		}
	}
	return count
}

// TestDeleteVMWithASnapshotNamingAnUnownedDisk covers delete_vm of a VM whose
// snapshot still names an unowned persistent disk with a stable ID. The first
// attempt deletes the slot, finds the snapshot, and refuses, naming it, with
// nothing destroyed. The transfer classes the snapshot refusal as a cloud
// error, so the refusal isn't retriable, the same as for an owned volume
// whose move PVE refuses. The disk is no longer on a slot of the VM, so a
// delete_vm run again finds nothing to preserve and destroys the VM, and the
// snapshot goes with it. PVE's destroy doesn't free a volume the VM doesn't own, so the
// volume survives free-floating, and the parker's record, which the keep rule
// holds, is still its one link. A later attach_disk resumes the transfer and
// attaches the disk with one reference. On c5a038de the first attempt failed
// at step 4 instead, and nothing about the snapshot was checked.
func TestDeleteVMWithASnapshotNamingAnUnownedDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, _ := buildUnownedDisk(t, false, false)
	s.withSnapshot(s.stranded)
	args := []json.RawMessage{planJSON(t, "777")}

	_, err := HandleDeleteVM(s.deps).Handle(context.Background(), args, jsonrpc.Context{})
	requirePermanent(t, err, "first delete_vm")
	requireText(t, err, "first delete_vm", []string{"retry resumes the transfer", `snapshot "before" of source vm 777`, "the park waits until that snapshot is deleted"})
	if destroys := s.recorder.guestDestroys(); len(destroys) != 0 {
		t.Fatalf("VM 777 was destroyed %d times on the first attempt", len(destroys))
	}
	if s.client.state.volumes[s.stranded] == nil || s.transferRecords(t, s.stranded) != 1 {
		t.Fatalf("after the first attempt: volume present=%t, records=%d, want the volume and one record", s.client.state.volumes[s.stranded] != nil, s.transferRecords(t, s.stranded))
	}

	if _, err := HandleDeleteVM(s.deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("retried delete_vm: %v", err)
	}
	if destroys := s.recorder.guestDestroys(); len(destroys) != 1 || s.client.state.configs[777] != nil {
		t.Fatalf("the retry destroyed VM 777 %d times, VM present=%t, want it destroyed once", len(destroys), s.client.state.configs[777] != nil)
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("the destroy freed %s", s.stranded)
	}
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("%s is referenced by %v, want it free-floating", s.stranded, refs)
	}
	if records := s.transferRecords(t, s.stranded); records != 1 {
		t.Fatalf("%d records link the CID to %s, want exactly one", records, s.stranded)
	}

	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach_disk(888) of the recoverable disk: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("the disk landed on VM %d, want 888", vmid)
	}
}
