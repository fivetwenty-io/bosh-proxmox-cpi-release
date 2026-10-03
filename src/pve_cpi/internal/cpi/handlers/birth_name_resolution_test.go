package handlers

// An unmanaged stable-ID disk A keeps its birth volume name in its CID, but a
// move renames A's volume, so another volume B can later take that name. These
// tests put B at A's birth name and call every disk handler with A's CID. A
// handler finds A by its serial or its parker's transfer record. When nothing
// but an entry it can't prove holds A names A's birth volume, the handler
// refuses A, so nothing it does lands on B.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// birthNameCollision is A's birth volume, which B holds in these tests.
const birthNameCollision = "a:9001/vm-9001-disk-0.raw"

// noteAttachedDisk records cid under token in cfg's attached-disk note, the
// note attach_disk leaves on the VM a disk is attached to.
func noteAttachedDisk(t *testing.T, cfg map[string]any, token, cid string) {
	t.Helper()
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	disks := map[string]string{}
	if existing, ok := raw["bosh_attached_disks"]; ok {
		if err := json.Unmarshal(existing, &disks); err != nil {
			t.Fatal(err)
		}
	}
	disks[token] = cid
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_attached_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
}

// dropAttachedDiskNotes removes every attached-disk note from cfg.
func dropAttachedDiskNotes(t *testing.T, cfg map[string]any) {
	t.Helper()
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	delete(raw, "bosh_attached_disks")
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
}

// legacyBirthNameFixture is the flow fixture turned legacy, with its managed
// disk removed, parking turned on, and VMs 777 and 888 on node n1.
func legacyBirthNameFixture(t *testing.T) *strandedDisk {
	t.Helper()
	deps, client, journal, _, _ := lifecycleFlowFixture(t)
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	deps.Config.DetachedDiskStrategy = "parked"
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	delete(client.state.volumes, original)
	delete(client.state.configs[777], "scsi1")
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	return &strandedDisk{deps: deps, client: client, recorder: recorder, journal: journal}
}

// mintStableDisk gives s an unmanaged stable-ID disk whose CID names birth.
func (s *strandedDisk) mintStableDisk(t *testing.T, birth string) {
	t.Helper()
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID(birth, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	s.cid, s.token = cid, token
}

// buildBirthNameCollision gives VM 777's scsi1 a serial-less volume B at the
// birth name of disk A, which no slot, unused entry, or parker record names.
func buildBirthNameCollision(t *testing.T) *strandedDisk {
	t.Helper()
	s := legacyBirthNameFixture(t)
	s.mintStableDisk(t, birthNameCollision)
	s.client.state.volumes[birthNameCollision] = &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
	s.client.state.configs[777]["scsi1"] = birthNameCollision + ",size=3G"
	return s
}

// clusterState captures every config, descriptions and tags included, and
// every volume, so a row can prove a refused call wrote nothing at all.
func (s *strandedDisk) clusterState(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Configs any
		Volumes any
	}{s.client.state.configs, s.client.state.volumes})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// requireBirthNameRefused fails unless err is the permanent refusal naming
// A's serial, its birth volume, and holder, and nothing in the cluster moved,
// grew, was snapshotted, deleted, or destroyed.
func (s *strandedDisk) requireBirthNameRefused(t *testing.T, call string, err error, before string, holder string) {
	t.Helper()
	var typed *cpierrors.Error
	if err == nil || !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("%s = %v, want a permanent CloudError", call, err)
	}
	for _, want := range []string{"no slot carries the disk's serial " + s.token, "its birth volume " + birthNameCollision, holder} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s = %v\nwant it to contain %q", call, err, want)
		}
	}
	if after := s.clusterState(t); after != before {
		t.Fatalf("%s wrote to the cluster:\nbefore %s\nafter  %s", call, before, after)
	}
	c := s.client
	if c.resizeCalls != 0 || len(c.snapshots) != 0 || c.deletes != 0 || c.moves != 0 || c.moveCalls != 0 || len(s.recorder.submissions) != 0 {
		t.Fatalf("%s resized %d, snapshotted %d, deleted %d, moved %d, and destroyed %d", call,
			c.resizeCalls, len(c.snapshots), c.deletes, c.moves, len(s.recorder.submissions))
	}
}

// birthNameCalls are the handlers that act on a disk by its CID, each called
// with A's CID.
var birthNameCalls = []struct {
	name string
	call func(t *testing.T, s *strandedDisk) error
}{
	{"attach_disk to the VM holding B", func(t *testing.T, s *strandedDisk) error {
		return attachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	}},
	{"attach_disk to another VM", func(t *testing.T, s *strandedDisk) error {
		return attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	}},
	{"detach_disk", func(t *testing.T, s *strandedDisk) error {
		return detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	}},
	{"resize_disk", func(t *testing.T, s *strandedDisk) error {
		return callHandler(t, HandleResizeDisk(s.deps), s.cid, 6144)
	}},
	{"update_disk", func(t *testing.T, s *strandedDisk) error {
		return callHandler(t, HandleUpdateDisk(s.deps), s.cid, map[string]any{"cache": "writeback", "size": 6144})
	}},
	{"snapshot_disk", func(t *testing.T, s *strandedDisk) error {
		return callHandler(t, HandleSnapshotDisk(s.deps), s.cid, map[string]any{})
	}},
	{"set_disk_metadata", func(t *testing.T, s *strandedDisk) error {
		return callHandler(t, HandleSetDiskMetadata(s.deps), s.cid, map[string]any{"director": "x"})
	}},
	{"delete_disk", func(t *testing.T, s *strandedDisk) error {
		return callHandler(t, HandleDeleteDisk(s.deps), s.cid)
	}},
}

// TestBirthNameHandlersRefuseAndNeverWriteB calls each handler with A's CID
// while B sits serial-less at A's birth name on 777's scsi1. Before the fix
// the resolver returned 777's slot as A, so attach_disk rewrote B's drive with
// A's serial, detach_disk moved B to a parker, resize_disk and update_disk
// grew B, snapshot_disk snapshotted 777, set_disk_metadata wrote A's record
// into 777's description, and delete_disk didn't give this refusal.
func TestBirthNameHandlersRefuseAndNeverWriteB(t *testing.T) {
	for _, tc := range birthNameCalls {
		t.Run(tc.name, func(t *testing.T) {
			s := buildBirthNameCollision(t)
			before := s.clusterState(t)
			err := tc.call(t, s)
			s.requireBirthNameRefused(t, tc.name, err, before, "slot scsi1 of VM 777 on node n1 with no serial")
		})
	}
}

// TestBirthNameHasDiskReportsTheDiskMissing shows has_disk answering false for
// A while B holds its birth name. Before the fix it answered true from B's
// slot, which hid A's loss from bosh cck.
func TestBirthNameHasDiskReportsTheDiskMissing(t *testing.T) {
	s := buildBirthNameCollision(t)
	before := s.clusterState(t)
	result, err := HandleHasDisk(s.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, s.cid)}, jsonrpc.Context{})
	if err != nil || result != false {
		t.Fatalf("has_disk = %v, %v; want false", result, err)
	}
	if after := s.clusterState(t); after != before {
		t.Fatalf("has_disk wrote to the cluster:\nbefore %s\nafter  %s", before, after)
	}
}

// TestBirthNameForeignSerialRefusesEveryHandler gives B its own serial. Before
// the fix the resolver matched B's slot by name and never read the serial.
func TestBirthNameForeignSerialRefusesEveryHandler(t *testing.T) {
	for _, tc := range birthNameCalls {
		t.Run(tc.name, func(t *testing.T) {
			s := buildBirthNameCollision(t)
			s.client.state.configs[777]["scsi1"] = birthNameCollision + ",serial=bpd-8899aabbccddeeff,size=3G"
			before := s.clusterState(t)
			err := tc.call(t, s)
			s.requireBirthNameRefused(t, tc.name, err, before, "slot scsi1 of VM 777 on node n1 with serial bpd-8899aabbccddeeff")
		})
	}
}

// TestBirthNameWithAnOfflineNodeFailsRetriable keeps the offline-node answer
// ahead of the birth-name refusal. With node n2 offline the scan can't prove
// that no slot there carries A's serial, so the calls fail retriably, as they
// did before the fix.
func TestBirthNameWithAnOfflineNodeFailsRetriable(t *testing.T) {
	for _, tc := range birthNameCalls {
		t.Run(tc.name, func(t *testing.T) {
			s := buildBirthNameCollision(t)
			s.client.offlineNodes = map[string]bool{"n2": true}
			before := s.clusterState(t)
			err := tc.call(t, s)
			if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("%s = %v, want a retriable error", tc.name, err)
			}
			if after := s.clusterState(t); after != before {
				t.Fatalf("%s wrote to the cluster:\nbefore %s\nafter  %s", tc.name, before, after)
			}
		})
	}
}

// buildBirthNameOnParker parks another stable-ID disk C through the real
// handlers, so a parker exists, and then puts B on a second slot of that
// parker with no serial, the way a legacy disk parked by a config edit would
// sit.
func buildBirthNameOnParker(t *testing.T) *strandedDisk {
	t.Helper()
	s := legacyBirthNameFixture(t)
	other := "a:555/vm-555-disk-0.raw"
	s.mintStableDisk(t, other)
	s.client.state.volumes[other] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	if err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach disk C: %v", err)
	}
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("park disk C: %v", err)
	}
	parker := s.requireSingleHolder(t, true)
	s.client.state.volumes[birthNameCollision] = &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
	s.client.state.configs[parker]["scsi9"] = birthNameCollision + ",size=3G"
	s.mintStableDisk(t, birthNameCollision)
	s.client.moves, s.client.moveCalls, s.client.deletes = 0, 0, 0
	return s
}

// TestBirthNameOnAParkerIsNeverExposed puts B on a parker with no serial.
// Before the fix the resolver returned the parker as A's holder, so
// attach_disk and create_vm's attach unparked B onto the target as A.
func TestBirthNameOnAParkerIsNeverExposed(t *testing.T) {
	t.Run("attach_disk", func(t *testing.T) {
		s := buildBirthNameOnParker(t)
		before := s.clusterState(t)
		err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
		s.requireBirthNameRefused(t, "attach_disk(888)", err, before, "slot scsi9 of VM ")
	})
	for _, op := range []string{"create_vm.attach_existing", "create_vm.attach_disk"} {
		t.Run(op, func(t *testing.T) {
			s := buildBirthNameOnParker(t)
			before := s.clusterState(t)
			bare, meta, err := decodeDiskCID(context.Background(), s.deps, op, s.cid)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := resolveDiskForOp(context.Background(), s.deps, op, s.cid, bare, meta)
			if err == nil {
				_, err = guardAndUnparkBeforeAttach(context.Background(), s.deps, op, &rd, "n1", 888)
			}
			s.requireBirthNameRefused(t, op, err, before, "slot scsi9 of VM ")
		})
	}
}

// TestBirthNameUnusedEntryWithoutTheNoteRefuses strands A on 777's unused
// entry and then removes 777's attached-disk note. Nothing then ties the entry
// to A rather than to another disk, so attach_disk refuses. Before the fix the
// stranded route moved the entry's volume onto 888 as A.
func TestBirthNameUnusedEntryWithoutTheNoteRefuses(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	dropAttachedDiskNotes(t, s.client.state.configs[777])
	s.client.moves, s.client.moveCalls, s.client.deletes = 0, 0, 0
	before := s.clusterState(t)
	err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	var typed *cpierrors.Error
	if err == nil || !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("attach_disk(888) = %v, want a permanent CloudError", err)
	}
	want := "unused entry unused0 of VM 777 on node n1, whose description holds no note for the disk"
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "its birth volume "+s.stranded) {
		t.Fatalf("attach_disk(888) = %v\nwant it to contain %q", err, want)
	}
	if after := s.clusterState(t); after != before {
		t.Fatalf("attach_disk(888) wrote to the cluster:\nbefore %s\nafter  %s", before, after)
	}
	if s.client.moves != 0 || s.client.deletes != 0 {
		t.Fatal("the refused attach moved or deleted a volume")
	}
}

// TestBirthNameStrandedDiskWithItsNoteRecovers is the stranded route as it
// works today. A sits on 777's unused entry and 777 still holds A's note, so
// attach_disk moves A off 777 and onto 888.
func TestBirthNameStrandedDiskWithItsNoteRecovers(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	if pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(s.client.state.configs[777]))[s.token] != s.cid {
		t.Fatal("777 lost A's attached-disk note")
	}
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach_disk(888): %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("A landed on VM %d, want 888", vmid)
	}
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 still holds unused entries %v after the attach", unused)
	}
}

// TestBirthNameOrdinaryCycleNeverTouchesB attaches A to 777, parks it, which
// renames A's volume, and then lets B take A's birth name on 778 with no
// serial. Resolution keeps finding A on its parker by serial, and A goes back
// to 777 and away again without B's slot or volume changing. Before the fix
// the resolver returned 778 whenever the listing reached it first, and the
// loop makes that all but certain to show.
func TestBirthNameOrdinaryCycleNeverTouchesB(t *testing.T) {
	s := legacyBirthNameFixture(t)
	birth := "a:777/vm-777-disk-0.raw"
	s.mintStableDisk(t, birth)
	s.client.state.volumes[birth] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	ctx := context.Background()
	if err := attachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach_disk(777): %v", err)
	}
	if err := detachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk(777): %v", err)
	}
	parker := s.requireSingleHolder(t, true)
	if s.client.state.volumes[birth] != nil {
		t.Fatalf("the park kept A's birth name %s, want the volume renamed for the parker", birth)
	}

	b := &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
	s.client.state.volumes[birth] = b
	s.client.state.configs[778] = map[string]any{"name": "w778", "digest": "1", "scsi1": birth + ",size=3G"}
	requireB := func(step string) {
		t.Helper()
		if s.client.state.configs[778]["scsi1"] != birth+",size=3G" || s.client.state.volumes[birth] != b || b.Size != 3<<30 {
			t.Fatalf("%s changed B: 778.scsi1=%v volume=%+v", step, s.client.state.configs[778]["scsi1"], s.client.state.volumes[birth])
		}
		if refs := s.references(birth); len(refs) != 1 || refs[0] != "778.scsi1" {
			t.Fatalf("after %s B's volume is referenced by %v, want 778.scsi1 alone", step, refs)
		}
	}

	bare, meta, err := decodeDiskCID(ctx, s.deps, "attach_disk", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 32 {
		rd, err := resolveDiskForOp(ctx, s.deps, "attach_disk", s.cid, bare, meta)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if rd.holder == nil || rd.holder.VMID != parker || !rd.holder.IsParker || rd.volid == birth {
			t.Fatalf("call %d resolved A to %+v on %s, want parker %d by serial", i, rd.holder, rd.volid, parker)
		}
	}
	if err := attachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("reattach to 777: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 777 {
		t.Fatalf("A landed on VM %d, want 777", vmid)
	}
	requireB("the reattach")
	if err := detachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach again: %v", err)
	}
	s.requireSingleHolder(t, true)
	requireB("the second detach")
}

// buildBirthNameUnfinishedTransfer parks A off 777 through the real handlers
// and then puts the parker back in the state a crash leaves between the move
// and the serial write. The landed slot loses A's serial, and the record names
// A's volume by its name before the move, which is A's birth name. B then
// takes that birth name on 778's scsi1 with no serial.
func buildBirthNameUnfinishedTransfer(t *testing.T) (*strandedDisk, string, *nodes.GetStorageContentResponse) {
	t.Helper()
	s := legacyBirthNameFixture(t)
	birth := "a:777/vm-777-disk-0.raw"
	s.mintStableDisk(t, birth)
	s.client.state.volumes[birth] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	ctx := context.Background()
	if err := attachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach_disk(777): %v", err)
	}
	if err := detachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk(777): %v", err)
	}
	parker := s.requireSingleHolder(t, true)
	cfg := s.client.state.configs[parker]
	for slot, value := range s.serialHolders() {
		key := strings.TrimPrefix(slot, strconv.Itoa(parker)+".")
		cfg[key] = strings.Replace(cfg[key].(string), ",serial="+s.token, "", 1)
		if !strings.HasPrefix(value, "a:") {
			t.Fatalf("parker slot %s holds %q", slot, value)
		}
	}
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if disks[s.token] == nil || disks[s.token]["slot"] == "" {
		t.Fatalf("the park left no record with a slot for A: %v", disks)
	}
	disks[s.token]["volid"] = birth
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
	if len(s.serialHolders()) != 0 || s.intentParker() != parker {
		t.Fatalf("the crash state still has serial holders %v or lost the record", s.serialHolders())
	}

	b := &nodes.GetStorageContentResponse{Size: 3 << 30, Format: "raw"}
	s.client.state.volumes[birth] = b
	s.client.state.configs[778] = map[string]any{"name": "w778", "digest": "1", "scsi1": birth + ",size=3G"}
	s.client.moves, s.client.moveCalls, s.client.deletes = 0, 0, 0
	return s, birth, b
}

// TestBirthNameUnfinishedTransferResumesFirst calls each handler with A's CID
// while A's transfer onto a parker stopped between its move and its serial
// write, and B sits at A's birth name, which the record names. Resolution
// returns the record, so every handler has to finish the transfer before it
// acts. Before the fix resize_disk grew B, snapshot_disk snapshotted 778, and
// set_disk_metadata wrote A's record into 778's description, because each of
// them acted on the record's volume name without the resume.
func TestBirthNameUnfinishedTransferResumesFirst(t *testing.T) {
	onParker := func(t *testing.T, s *strandedDisk) {
		t.Helper()
		s.requireSingleHolder(t, true)
	}
	for _, tc := range []struct {
		name  string
		call  func(t *testing.T, s *strandedDisk) error
		after func(t *testing.T, s *strandedDisk)
	}{
		{"attach_disk", func(t *testing.T, s *strandedDisk) error {
			return attachDiskAt(t, context.Background(), s.deps, "777", s.cid)
		}, func(t *testing.T, s *strandedDisk) {
			if vmid := s.requireSingleHolder(t, false); vmid != 777 {
				t.Fatalf("A landed on VM %d, want 777", vmid)
			}
		}},
		{"detach_disk", func(t *testing.T, s *strandedDisk) error {
			return detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
		}, onParker},
		{"resize_disk", birthNameCall(t, "resize_disk"), onParker},
		{"update_disk", birthNameCall(t, "update_disk"), onParker},
		{"snapshot_disk", birthNameCall(t, "snapshot_disk"), onParker},
		{"set_disk_metadata", birthNameCall(t, "set_disk_metadata"), onParker},
		{"delete_disk", birthNameCall(t, "delete_disk"), func(t *testing.T, s *strandedDisk) {
			if holders := s.serialHolders(); len(holders) != 0 {
				t.Fatalf("delete_disk left A's serial on %v", holders)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, birth, b := buildBirthNameUnfinishedTransfer(t)
			before778 := s.client.state.configs[778]["description"]
			err := tc.call(t, s)
			if s.client.state.configs[778]["scsi1"] != birth+",size=3G" || s.client.state.volumes[birth] != b || b.Size != 3<<30 {
				t.Fatalf("%s (err %v) changed B: 778.scsi1=%v volume=%+v", tc.name, err, s.client.state.configs[778]["scsi1"], s.client.state.volumes[birth])
			}
			if after := s.client.state.configs[778]["description"]; after != before778 {
				t.Fatalf("%s (err %v) wrote 778's description: %v", tc.name, err, after)
			}
			if len(s.client.snapshots) != 0 {
				t.Fatalf("%s (err %v) took snapshots %v, and the only VM it could reach is 778", tc.name, err, s.client.snapshots)
			}
			if refs := s.references(birth); len(refs) != 1 || refs[0] != "778.scsi1" {
				t.Fatalf("after %s (err %v) B's volume is referenced by %v, want 778.scsi1 alone", tc.name, err, refs)
			}
			tc.after(t, s)
		})
	}
}

// birthNameCall returns the call from birthNameCalls with the given name.
func birthNameCall(t *testing.T, name string) func(t *testing.T, s *strandedDisk) error {
	t.Helper()
	for _, tc := range birthNameCalls {
		if tc.name == name {
			return tc.call
		}
	}
	t.Fatalf("no birth-name call named %s", name)
	return nil
}

// TestBirthNameFreeFloatingDiskStillResolves is the control. A disk nothing
// names resolves to its birth volume and has_disk finds it there, before the
// fix and after it.
func TestBirthNameFreeFloatingDiskStillResolves(t *testing.T) {
	s := legacyBirthNameFixture(t)
	s.mintStableDisk(t, birthNameCollision)
	s.client.state.volumes[birthNameCollision] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	bare, meta, err := decodeDiskCID(context.Background(), s.deps, "attach_disk", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), s.deps, "attach_disk", s.cid, bare, meta)
	if err != nil || rd.holder != nil || rd.intent != nil || rd.volid != birthNameCollision {
		t.Fatalf("resolveDiskForOp = %+v, %v; want the birth volume with no holder", rd, err)
	}
	result, err := HandleHasDisk(s.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, s.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Fatalf("has_disk = %v, %v; want true", result, err)
	}
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach_disk(888): %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("A landed on VM %d, want 888", vmid)
	}
}

// requireRetentionRefused fails unless delete_vm refused 777's ephemeral
// volume as ambiguous and changed no disk entry or volume. Retention records
// the minted CID in 777's description before it resolves, as it always has,
// so the description is left out of the comparison.
func requireRetentionRefused(t *testing.T, err error, client *lifecycleFlowPVE, recorder *legacyDestroyRecorder, before string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "retained ephemeral ownership is ambiguous") {
		t.Fatalf("delete_vm = %v, want the ambiguous-ownership refusal", err)
	}
	if after := (&strandedDisk{client: client}).snapshot(); after != before {
		t.Fatalf("the refused delete_vm changed a disk entry or a volume:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if client.moves != 0 || client.deletes != 0 || len(recorder.submissions) != 0 {
		t.Fatalf("the refused delete_vm moved %d, deleted %d, and destroyed %d", client.moves, client.deletes, len(recorder.submissions))
	}
}

// TestBirthNameRetentionKeepsOnlyItsOwnSlot covers legacy ephemeral retention,
// which reads a serial-less slot off the VM it deletes and mints the volume's
// stable ID. The volume is retained when that slot is the only entry naming
// it. When another guest or a second slot of 777 names it too, delete_vm
// refuses and the other entry is never chosen. Before the fix the scan
// returned whichever slot it reached first, so the loop retained 777's slot
// on some runs while 888's slot named the same volume.
func TestBirthNameRetentionKeepsOnlyItsOwnSlot(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run("own slot/"+volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
				t.Fatalf("delete_vm: %v", err)
			}
			assertLegacyRetained(t, deps, client, recorder, volume)
		})
		for _, shape := range []struct {
			name string
			add  func(client *lifecycleFlowPVE)
		}{
			{"another guest", func(client *lifecycleFlowPVE) {
				client.state.configs[888] = map[string]any{"name": "other", "digest": "1", "scsi0": volume + ",size=5G"}
			}},
			{"a second slot", func(client *lifecycleFlowPVE) {
				client.state.configs[777]["scsi3"] = volume + ",size=5G"
			}},
		} {
			t.Run(shape.name+"/"+volume, func(t *testing.T) {
				for range 16 {
					deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
					shape.add(client)
					before := (&strandedDisk{client: client}).snapshot()
					_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
					requireRetentionRefused(t, err, client, recorder, before)
				}
			})
		}
	}
}

// TestBirthNameRetentionRefusesASlotWithASerial covers 777's own slot taking
// another disk's serial while retention runs. delete_vm parks a slot that
// carries a serial before retention starts, so the serial arrives with the
// write that notes the minted CID, and the scan then reports 777's slot under
// that serial. delete_vm refuses the volume, because a slot with a serial
// belongs to the disk that serial names, and destroys nothing.
func TestBirthNameRetentionRefusesASlotWithASerial(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
			stamped := false
			client.afterConfigWrite = func(vmid int) {
				cfg := client.state.configs[vmid]
				if vmid == 777 && !stamped && pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg))[volume] != "" {
					stamped = true
					cfg["scsi2"] = volume + ",serial=bpd-0011223344556677,size=5G"
				}
			}
			_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			if err == nil || !strings.Contains(err.Error(), "retained ephemeral ownership is ambiguous") {
				t.Fatalf("delete_vm = %v, want the ambiguous-ownership refusal", err)
			}
			if client.moves != 0 || client.deletes != 0 || len(recorder.submissions) != 0 {
				t.Fatalf("the refused delete_vm moved %d, deleted %d, and destroyed %d", client.moves, client.deletes, len(recorder.submissions))
			}
			if slot, _ := client.state.configs[777]["scsi2"].(string); client.state.volumes[volume] == nil || strings.Split(slot, ",")[0] != volume {
				t.Fatalf("the refused delete_vm left scsi2=%q, want it still naming %s", slot, volume)
			}
		})
	}
}

// TestBirthNameRetentionKeepsItsPendingDeletedSlot covers a stopped VM whose
// own serial-less ephemeral slot has a pending delete. VM 777 already notes
// a CID for the volume, so retention writes no note, and the slot's delete
// stays pending, because on PVE a stopped VM applies its pending changes with
// the next config write. A VM reaches this state when an earlier delete_vm
// stopped after its note write, the slot's delete stayed pending while the VM
// ran, and the VM then stopped without that cleanup. The scan reports the
// slot from the current view, because the applied view leaves it out, and
// retention reads the slot from the same view, so delete_vm retains the
// volume. A read of the applied view alone would find nothing there and
// refuse the volume as ambiguous.
func TestBirthNameRetentionKeepsItsPendingDeletedSlot(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
			token, err := pve.GenerateDiskStableID()
			if err != nil {
				t.Fatal(err)
			}
			cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
			if err != nil {
				t.Fatal(err)
			}
			noteAttachedDisk(t, client.state.configs[777], volume, cid)
			client.pending = newFakePendingModel()
			client.pending.holdDelete(777, client.state.configs[777], "scsi2")
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
				t.Fatalf("delete_vm: %v", err)
			}
			assertLegacyRetained(t, deps, client, recorder, volume)
			guest := recorder.guestDestroys()[0].configs[777]
			if noted := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(guest))[volume]; noted != cid {
				t.Fatalf("777 noted %q when it was destroyed, want the CID it already held, %q", noted, cid)
			}
		})
	}
}

// TestBirthNameRetentionRefusesAConfigThatMovedOn runs retention's digest
// check through delete_vm. Retention writes the minted CID's note on 777 and
// reads the config back, and then 777's config changes again as the scan
// lists the guests, so the scan reports 777's slot with a digest retention
// never read. delete_vm refuses the volume and destroys nothing.
func TestBirthNameRetentionRefusesAConfigThatMovedOn(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
			changes := 0
			deps.PVE = configMovingPVE{legacyDestroyPVE: deps.PVE.(legacyDestroyPVE), onList: func() {
				changes++
				client.state.configs[777]["digest"] = "moved-" + strconv.Itoa(changes)
			}}
			before := (&strandedDisk{client: client}).snapshot()
			_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			if err == nil || !strings.Contains(err.Error(), "retained ephemeral config changed while its volume was resolved") {
				t.Fatalf("delete_vm = %v, want the changed-config refusal", err)
			}
			if after := (&strandedDisk{client: client}).snapshot(); after != before {
				t.Fatalf("the refused delete_vm changed a disk entry or a volume:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if client.moves != 0 || client.deletes != 0 || len(recorder.submissions) != 0 {
				t.Fatalf("the refused delete_vm moved %d, deleted %d, and destroyed %d", client.moves, client.deletes, len(recorder.submissions))
			}
		})
	}
}

// configMovingPVE runs onList each time a guest listing reaches a node, which
// the identity scan does before it reads any guest's config.
type configMovingPVE struct {
	legacyDestroyPVE
	onList func()
}

func (c configMovingPVE) Nodes() nodes.Service {
	return configMovingNodes{Service: c.legacyDestroyPVE.Nodes(), onList: c.onList}
}

type configMovingNodes struct {
	nodes.Service
	onList func()
}

func (n configMovingNodes) ListQemu(ctx context.Context, node string, p *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	n.onList()
	return n.Service.ListQemu(ctx, node, p)
}

// TestBirthNameRetentionPassesOtherAnswersThrough covers the two answers
// legacy retention must not take for its own slot. In the first, 777 already
// noted a CID for the volume, and that CID's serial sits on a slot of VM 888,
// so the resolver finds the serial there and retention refuses the volume as
// someone else's. In the second, node n2 is offline, and the scan's retriable
// answer comes back unchanged. Before the fix the name match on 777 won in
// both, and the volume was retained.
func TestBirthNameRetentionPassesOtherAnswersThrough(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run("a serial collision/"+volume, func(t *testing.T) {
			for range 16 {
				deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
				token, err := pve.GenerateDiskStableID()
				if err != nil {
					t.Fatal(err)
				}
				cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token})
				if err != nil {
					t.Fatal(err)
				}
				noteAttachedDisk(t, client.state.configs[777], volume, cid)
				client.state.configs[888] = map[string]any{"name": "other", "digest": "1", "scsi0": "a:vm-888-disk-7,serial=" + token + ",size=5G"}
				before := (&strandedDisk{client: client}).snapshot()
				_, err = HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
				requireRetentionRefused(t, err, client, recorder, before)
			}
		})
		t.Run("an offline node/"+volume, func(t *testing.T) {
			for range 16 {
				deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "scsi2")
				client.offlineNodes = map[string]bool{"n2": true}
				before := (&strandedDisk{client: client}).snapshot()
				_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
				var typed *cpierrors.Error
				if err == nil || !errors.As(err, &typed) || !typed.OkToRetry() ||
					!strings.Contains(err.Error(), "retain_ephemeral: resolve disk identity") || !strings.Contains(err.Error(), "excluded as offline") {
					t.Fatalf("delete_vm = %v, want the scan's retriable offline-node answer", err)
				}
				if after := (&strandedDisk{client: client}).snapshot(); after != before {
					t.Fatalf("delete_vm changed a disk entry or a volume:\nbefore:\n%s\nafter:\n%s", before, after)
				}
				if client.moves != 0 || client.deletes != 0 || len(recorder.submissions) != 0 {
					t.Fatalf("delete_vm moved %d, deleted %d, and destroyed %d", client.moves, client.deletes, len(recorder.submissions))
				}
			}
		})
	}
}
