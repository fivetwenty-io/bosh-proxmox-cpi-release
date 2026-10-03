package handlers

// A legacy stable-ID disk whose birth name carries its guest's own VMID can be
// stranded on that guest's unused entry. Its transfer to a parker deletes the
// slot, PVE keeps the owned volume as an unused entry, the move fails, and
// before 0.9.0 the parker's transfer record was collected an hour later. These
// tests build that state through the real handlers with PVE-faithful
// demotion, then remove the record the way an earlier release did. A birth
// name the guest doesn't own leaves no unused entry, and its transfer stops
// only when the parker attach after the slot delete fails.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

// strandedRunbookPointer is the pointer every stranded-disk refusal ends with.
const strandedRunbookPointer = `see "delete_disk refuses a disk stranded on an unused entry" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// buildBirthStrand strands one legacy stable-ID disk on guest 777 through the
// real detach_disk. owner is the VMID the disk's birth name carries: 777 is the
// owned birth shape, whose move fails, and any other VMID gives PVE no reason
// to keep an unused entry, so the transfer takes the config-edit attach, and
// that attach fails instead. With keepRecord false the parker's transfer record
// is removed afterwards, the way 0.5.1 through 0.8.0 collected it.
func buildBirthStrand(t *testing.T, owner int, local, keepRecord bool) *strandedDisk {
	t.Helper()
	deps, client, journal, _, _ := lifecycleFlowFixture(t)
	if local {
		client.localStorage = true
		client.volumeNodes = map[string]string{}
	}
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

	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	birth := fmt.Sprintf("a:%d/vm-%d-disk-0.raw", owner, owner)
	cid, err := pve.EncodeDiskCID(birth, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[birth] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	if local {
		client.volumeNodes[birth] = "n1"
		deps.Resolver = pve.NewBackendResolver(deps.PVE, nil, "n1")
	}
	client.state.configs[777]["scsi1"] = birth + ",serial=" + token + ",size=5G"
	noteAttachedDisk(t, client.state.configs[777], token, cid)
	s := &strandedDisk{deps: deps, client: client, recorder: recorder, journal: journal, cid: cid, token: token}

	if owner == 777 {
		client.moveErr = errors.New("move_disk: storage migration failed")
	} else {
		client.state.parkErr = errors.New("parker attach failed")
	}
	if err := detachDiskAt(t, context.Background(), deps, "777", cid); err == nil {
		t.Fatal("the transfer whose last step fails reported success")
	}
	client.moveErr, client.state.parkErr = nil, nil
	for _, volume := range pve.FindUnusedDiskEntries(client.state.configs[777]) {
		s.stranded = volume
	}
	if owner == 777 && s.stranded != birth {
		t.Fatalf("the failed transfer left %q on 777's unused entry, want %s: %v", s.stranded, birth, client.state.configs[777])
	}
	if owner != 777 && s.stranded != "" {
		t.Fatalf("PVE keeps no unused entry for a volume 777 doesn't own, but 777 has %s", s.stranded)
	}
	if s.intentParker() == 0 {
		t.Fatal("the failed transfer left no record on any parker")
	}
	if !keepRecord {
		s.dropRecord(t)
	}
	recorder.submissions = nil
	return s
}

// dropRecord removes the disk's transfer record from its parker.
func (s *strandedDisk) dropRecord(t *testing.T) {
	t.Helper()
	parker := s.intentParker()
	cfg := s.client.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]json.RawMessage
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	delete(disks, s.token)
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
	if s.intentParker() != 0 {
		t.Fatal("the transfer record is still on the parker")
	}
}

// snapshot captures every config key that names a disk and every volume, so
// a row can prove a refusal changed nothing.
func (s *strandedDisk) snapshot() string {
	var keys []string
	for vmid, cfg := range s.client.state.configs {
		for key, value := range cfg {
			if isDiskOptionKey(key) {
				keys = append(keys, fmt.Sprintf("%d.%s=%v", vmid, key, value))
			}
		}
	}
	for volume := range s.client.state.volumes {
		keys = append(keys, "volume "+volume)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// requireUnchanged fails when anything moved since before was taken, or when
// a volume was deleted or a guest destroyed.
func (s *strandedDisk) requireUnchanged(t *testing.T, before string, deletes int) {
	t.Helper()
	if after := s.snapshot(); after != before {
		t.Fatalf("a refused call changed the cluster:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if s.client.deletes != deletes || len(s.recorder.submissions) != 0 {
		t.Fatalf("a refused call deleted %d volumes and destroyed %d guests", s.client.deletes-deletes, len(s.recorder.submissions))
	}
}

func callHandler(t *testing.T, h Handler, args ...any) error {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(args))
	for _, arg := range args {
		raw = append(raw, planJSON(t, arg))
	}
	_, err := h.Handle(context.Background(), raw, jsonrpc.Context{})
	return err
}

var birthStrandStorages = []struct {
	name  string
	local bool
}{{"shared", false}, {"local", true}}

// TestStrandedOwnedBirthDetachParksTheDisk is F1 and F2. Before the guard,
// detach_disk found no holder and parked the birth volume by config edit, so
// a parker slot and 777's unused entry both named it.
func TestStrandedOwnedBirthDetachParksTheDisk(t *testing.T) {
	for _, storage := range birthStrandStorages {
		t.Run(storage.name, func(t *testing.T) {
			s := buildBirthStrand(t, 777, storage.local, false)
			deletes := s.client.deletes
			if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
				t.Fatalf("detach_disk(777): %v", err)
			}
			s.requireSingleHolder(t, true)
			if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
				t.Fatalf("777 still holds unused entries %v after detach_disk succeeded", unused)
			}
			if s.client.deletes != deletes {
				t.Fatal("detach_disk deleted a volume")
			}
		})
	}
}

// TestStrandedOwnedBirthAttachMovesTheDiskOffTheStrandedVM is F3 and F4.
// Before the guard, attach_disk(888) attached the birth volume by config edit
// while 777's unused entry still named it.
func TestStrandedOwnedBirthAttachMovesTheDiskOffTheStrandedVM(t *testing.T) {
	for _, storage := range birthStrandStorages {
		t.Run(storage.name, func(t *testing.T) {
			s := buildBirthStrand(t, 777, storage.local, false)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
				t.Fatalf("attach_disk(888): %v", err)
			}
			if vmid := s.requireSingleHolder(t, false); vmid != 888 {
				t.Fatalf("the disk landed on VM %d, want 888", vmid)
			}
			if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
				t.Fatalf("777 still holds unused entries %v after the attach", unused)
			}
		})
	}
}

// TestStrandedOwnedBirthDeleteDiskRefuses is F5 and F6. Before the guard,
// delete_disk deleted the volume while 777's unused entry still named it.
func TestStrandedOwnedBirthDeleteDiskRefuses(t *testing.T) {
	for _, storage := range birthStrandStorages {
		t.Run(storage.name, func(t *testing.T) {
			s := buildBirthStrand(t, 777, storage.local, false)
			before, deletes := s.snapshot(), s.client.deletes
			err := callHandler(t, HandleDeleteDisk(s.deps), s.cid)
			want := fmt.Sprintf("delete_disk: refusing to delete disk %s, because VM 777 on node n1 still names its volume %s as unused0, "+
				"and no other configuration does. Removing that entry by hand makes PVE free the volume. Nothing was deleted; %s",
				s.cid, s.stranded, strandedRunbookPointer)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("delete_disk = %v\nwant it to contain %q", err, want)
			}
			s.requireUnchanged(t, before, deletes)
		})
	}
}

// TestStrandedOwnedBirthKeptRecordResumes is C1. With the transfer record in
// place, the retried detach resumes the transfer, as it did before the guard.
func TestStrandedOwnedBirthKeptRecordResumes(t *testing.T) {
	s := buildBirthStrand(t, 777, false, true)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk(777): %v", err)
	}
	s.requireSingleHolder(t, true)
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 still holds unused entries %v", unused)
	}
}

// TestUnownedBirthLeavesNothingToGuard is C2. A birth name 777 doesn't own
// leaves no unused entry, so the guard never matches, and the disk behaves as
// a free-floating one does, before the guard and after it.
func TestUnownedBirthLeavesNothingToGuard(t *testing.T) {
	t.Run("detach", func(t *testing.T) {
		s := buildBirthStrand(t, 123, false, false)
		if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
			t.Fatalf("detach_disk(777): %v", err)
		}
		s.requireSingleHolder(t, true)
	})
	t.Run("delete", func(t *testing.T) {
		s := buildBirthStrand(t, 123, false, false)
		deletes := s.client.deletes
		if err := callHandler(t, HandleDeleteDisk(s.deps), s.cid); err != nil {
			t.Fatalf("delete_disk: %v", err)
		}
		if s.client.deletes != deletes+1 {
			t.Fatalf("delete_disk deleted %d volumes, want 1", s.client.deletes-deletes)
		}
	})
}

// TestRenamedStrandIsNotTheGuardsToMatch is C3. A volume renamed for 777 no
// longer carries the CID's birth name, so the guard can't match it, and
// delete_disk goes on exactly as before: it deletes the missing birth name and
// leaves the renamed volume on 777's unused entry.
func TestRenamedStrandIsNotTheGuardsToMatch(t *testing.T) {
	s := buildBirthStrand(t, 123, false, true)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	if err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach it back to 777: %v", err)
	}
	s.client.moveErr = errors.New("move_disk: storage migration failed")
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err == nil {
		t.Fatal("the transfer whose move fails reported success")
	}
	s.client.moveErr = nil
	for _, volume := range pve.FindUnusedDiskEntries(s.client.state.configs[777]) {
		s.stranded = volume
	}
	if !pve.VolumeNamedForVM(s.stranded, 777) {
		t.Fatalf("the renamed volume %q isn't named for 777", s.stranded)
	}
	s.dropRecord(t)
	if err := callHandler(t, HandleDeleteDisk(s.deps), s.cid); err != nil {
		t.Fatalf("delete_disk: %v", err)
	}
	if s.client.state.volumes[s.stranded] == nil || len(s.references(s.stranded)) != 1 {
		t.Fatalf("the renamed volume %s moved: present=%t refs=%v", s.stranded, s.client.state.volumes[s.stranded] != nil, s.references(s.stranded))
	}
}

// TestStrandedAmbiguousEntriesRefuseEverywhere is N1. Two guests' unused
// entries name the birth volume, so no handler can tell which one should hold
// it. Before the guard, each call parked, attached, or deleted the volume.
func TestStrandedAmbiguousEntriesRefuseEverywhere(t *testing.T) {
	for _, call := range []string{"detach_disk", "attach_disk", "delete_disk"} {
		t.Run(call, func(t *testing.T) {
			s := buildBirthStrand(t, 777, false, false)
			s.client.state.configs[778] = map[string]any{"name": "w778", "digest": "1", "unused0": s.stranded}
			// Both guests hold the disk's note, so each entry is a candidate.
			noteAttachedDisk(t, s.client.state.configs[778], s.token, s.cid)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			before, deletes := s.snapshot(), s.client.deletes
			err := strandedCall(t, s, call)
			if err == nil || !strings.Contains(err.Error(), "can't be tied to one guest outside the parker band: unused entry unused0 of VM 777 on node n1, unused entry unused0 of VM 778 on node n1") {
				t.Fatalf("%s = %v, want the ambiguous refusal naming both entries", call, err)
			}
			s.requireUnchanged(t, before, deletes)
		})
	}
}

// TestStrandedOnParkerBandRefusesAsAmbiguous puts the only entry on a VM in
// the parker band, where an entry is an unpark's leftover rather than a
// stranded transfer, so every handler refuses it as ambiguous.
func TestStrandedOnParkerBandRefusesAsAmbiguous(t *testing.T) {
	for _, call := range []string{"detach_disk", "attach_disk", "delete_disk"} {
		t.Run(call, func(t *testing.T) {
			s := buildBirthStrand(t, 777, false, false)
			delete(s.client.state.configs[777], "unused0")
			s.client.state.configs[90500] = map[string]any{"name": "w90500", "digest": "1", "unused0": s.stranded}
			// 90500 holds the disk's note, so its entry reaches the parker-band
			// check rather than the refusal of an entry nothing ties to the disk.
			noteAttachedDisk(t, s.client.state.configs[90500], s.token, s.cid)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			before, deletes := s.snapshot(), s.client.deletes
			err := strandedCall(t, s, call)
			if err == nil || !strings.Contains(err.Error(), "can't be tied to one guest outside the parker band: unused entry unused0 of VM 90500 on node n1") {
				t.Fatalf("%s = %v, want the ambiguous refusal", call, err)
			}
			s.requireUnchanged(t, before, deletes)
		})
	}
}

func strandedCall(t *testing.T, s *strandedDisk, call string) error {
	t.Helper()
	switch call {
	case "detach_disk":
		return detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	case "attach_disk":
		return attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	default:
		return callHandler(t, HandleDeleteDisk(s.deps), s.cid)
	}
}

// TestStrandedOnAnotherVMDetachRefuses is N2. A detach from a VM that doesn't
// hold the entry can't move it. Before the guard, detach_disk(778) parked the
// birth volume while 777's unused entry still named it.
func TestStrandedOnAnotherVMDetachRefuses(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[778] = map[string]any{"name": "w778", "digest": "1"}
	before, deletes := s.snapshot(), s.client.deletes
	err := detachDiskAt(t, context.Background(), s.deps, "778", s.cid)
	want := fmt.Sprintf("detach_disk: refusing to detach disk %s from VM 778, because its volume %s sits on unused entry unused0 of VM 777 on node n1, "+
		"and no slot carries it. Removing that entry by hand makes PVE free the volume. Nothing was changed; %s", s.cid, s.stranded, strandedRunbookPointer)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("detach_disk(778) = %v\nwant it to contain %q", err, want)
	}
	s.requireUnchanged(t, before, deletes)
}

// TestStrandedOnTargetAttachLandsOnce is N3. PVE drops an unused entry when
// the same volume is attached again, so the ordinary attach to 777 leaves one
// reference.
func TestStrandedOnTargetAttachLandsOnce(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	if err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach_disk(777): %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 777 {
		t.Fatalf("the disk landed on VM %d, want 777", vmid)
	}
}

// TestStrandedManagedCreateVMPathRefuses is N4. The create_vm attach paths of
// a journal-managed VM don't move a disk off another VM. Before the guard, the
// attach guard planned an ordinary attach of the birth volume.
func TestStrandedManagedCreateVMPathRefuses(t *testing.T) {
	for _, op := range []string{"create_vm.attach_existing", "create_vm.attach_disk"} {
		t.Run(op, func(t *testing.T) {
			s := buildBirthStrand(t, 777, false, false)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			bare, meta, err := decodeDiskCID(context.Background(), s.deps, op, s.cid)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := resolveDiskForOp(context.Background(), s.deps, op, s.cid, bare, meta)
			if err != nil {
				t.Fatal(err)
			}
			before, deletes := s.snapshot(), s.client.deletes
			_, err = guardAndUnparkBeforeAttach(context.Background(), s.deps, op, &rd, "n1", 888)
			want := fmt.Sprintf("%s: refusing to attach disk %s to VM 888, because its volume %s sits on unused entry unused0 of VM 777 on node n1 and no slot carries it. "+
				"This path doesn't move a disk off another VM, so nothing was attached.", op, s.cid, s.stranded)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("attach guard = %v\nwant it to contain %q", err, want)
			}
			s.requireUnchanged(t, before, deletes)
		})
	}
}

// TestStrandedDiskWithAnOfflineNodeFailsRetriable shows that the guard never
// acts on a partial cluster. With node n2 offline, the scan can't prove that
// no slot there carries the disk, so every call fails retriable and nothing
// changes, as it did before the guard.
func TestStrandedDiskWithAnOfflineNodeFailsRetriable(t *testing.T) {
	for _, call := range []string{"detach_disk", "attach_disk", "delete_disk"} {
		t.Run(call, func(t *testing.T) {
			s := buildBirthStrand(t, 777, false, false)
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			s.client.offlineNodes = map[string]bool{"n2": true}
			before, deletes := s.snapshot(), s.client.deletes
			err := strandedCall(t, s, call)
			if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("%s = %v, want a retriable error", call, err)
			}
			s.requireUnchanged(t, before, deletes)
		})
	}
}

// probeCountingPVE counts the storage-content reads a handler makes.
type probeCountingPVE struct {
	legacyDestroyPVE
	probes *int
}

func (c probeCountingPVE) Nodes() nodes.Service {
	return probeCountingNodes{Service: c.legacyDestroyPVE.Nodes(), probes: c.probes}
}

type probeCountingNodes struct {
	nodes.Service
	probes *int
}

func (n probeCountingNodes) GetStorageContent(ctx context.Context, node, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	*n.probes++
	return n.Service.GetStorageContent(ctx, node, pool, volume)
}

func (c probeCountingPVE) Storage() storage.Service {
	return probeCountingStorage{Service: c.legacyDestroyPVE.Storage(), probes: c.probes}
}

type probeCountingStorage struct {
	storage.Service
	probes *int
}

func (s probeCountingStorage) Exists(ctx context.Context, node, pool, volume string) (bool, error) {
	*s.probes++
	return s.Service.Exists(ctx, node, pool, volume)
}

func (n probeCountingNodes) ListStorageContent(ctx context.Context, node, pool string, p *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	*n.probes++
	return n.Service.ListStorageContent(ctx, node, pool, p)
}

// TestStrandedDiskHasDiskAnswersFromTheEntry shows has_disk answering true
// from the unused entry, the way it does for a holder or an intent. Before
// the guard it reached the same answer through a storage probe.
func TestStrandedDiskHasDiskAnswersFromTheEntry(t *testing.T) {
	s := buildBirthStrand(t, 777, true, false)
	probes := 0
	s.deps.PVE = probeCountingPVE{legacyDestroyPVE: s.deps.PVE.(legacyDestroyPVE), probes: &probes}
	s.deps.Resolver = pve.NewBackendResolver(s.deps.PVE, nil, "n1")
	result, err := HandleHasDisk(s.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, s.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Fatalf("has_disk = %v, %v; want true", result, err)
	}
	if probes != 0 {
		t.Fatalf("has_disk probed storage %d times for a disk an unused entry names", probes)
	}
}

// TestStrandedDiskResizeAndSnapshotRefuse shows resize_disk and snapshot_disk
// refusing with the entry named. Before the guard each failed on its own
// active-only lookup with no word about the entry.
func TestStrandedDiskResizeAndSnapshotRefuse(t *testing.T) {
	for _, op := range []string{"resize_disk", "snapshot_disk"} {
		t.Run(op, func(t *testing.T) {
			s := buildBirthStrand(t, 777, false, false)
			before, deletes := s.snapshot(), s.client.deletes
			var err error
			if op == "resize_disk" {
				err = callHandler(t, HandleResizeDisk(s.deps), s.cid, 6144)
			} else {
				err = callHandler(t, HandleSnapshotDisk(s.deps), s.cid, map[string]any{})
			}
			want := fmt.Sprintf("%s: refusing disk %s, because its volume %s sits on unused entry unused0 of VM 777 on node n1 and no slot carries it. Nothing was changed; %s",
				op, s.cid, s.stranded, strandedRunbookPointer)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s = %v\nwant it to contain %q", op, err, want)
			}
			s.requireUnchanged(t, before, deletes)
			if s.client.resizeCalls != 0 || len(s.client.snapshots) != 0 {
				t.Fatal("a refused call resized or snapshotted")
			}
		})
	}
}

// managedStrand moves the flow fixture's journal-managed disk from 777's
// scsi1 to its unused0, the state a failed managed transfer leaves once its
// planned move step is settled. The managed rows can't build that through
// the handlers until the settlement exists, so they seed it, along with the
// attached-disk note attach_disk left on 777 under the disk's stable ID.
func managedStrand(t *testing.T, client *lifecycleFlowPVE, cid string) string {
	t.Helper()
	value := client.state.configs[777]["scsi1"].(string)
	token, ok := pve.StableIDFromDriveOptStr(value)
	if !ok {
		t.Fatalf("777's scsi1 %q carries no stable ID", value)
	}
	noteAttachedDisk(t, client.state.configs[777], token, cid)
	volume := strings.Split(value, ",")[0]
	delete(client.state.configs[777], "scsi1")
	client.state.configs[777]["unused0"] = volume
	return volume
}

func requireRecordUnchanged(t *testing.T, journal *aj.Journal, id string, before aj.Record) {
	t.Helper()
	after, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.Steps) != len(before.Steps) {
		t.Fatalf("the record moved from %s with %d steps to %s with %d steps", before.State, len(before.Steps), after.State, len(after.Steps))
	}
}

// TestStrandedManagedDiskRefusesBeforeItsLifecycle covers attach_disk and
// resize_disk on a journal-managed disk stranded on 777. Both refuse before
// the lifecycle opens, so the record stays as it was. Before the guard, the
// attach landed the volume on 888 while 777 still named it, and the resize
// failed after admission and left the record needing reconciliation.
func TestStrandedManagedDiskRefusesBeforeItsLifecycle(t *testing.T) {
	for _, op := range []string{"attach_disk", "resize_disk"} {
		t.Run(op, func(t *testing.T) {
			deps, client, journal, id, cid := lifecycleFlowFixture(t)
			volume := managedStrand(t, client, cid)
			client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			before, err := journal.Inspect(id)
			if err != nil {
				t.Fatal(err)
			}
			var want string
			if op == "attach_disk" {
				err = attachDiskAt(t, context.Background(), deps, "888", cid)
				want = fmt.Sprintf("attach_disk: refusing to attach journal-managed disk %s to VM 888, because its volume %s sits on unused entry unused0 of VM 777 on node n1", cid, volume)
			} else {
				err = callHandler(t, HandleResizeDisk(deps), cid, 6144)
				want = fmt.Sprintf("resize_disk: refusing disk %s, because its volume %s sits on unused entry unused0 of VM 777 on node n1", cid, volume)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s = %v\nwant it to contain %q", op, err, want)
			}
			requireRecordUnchanged(t, journal, id, before)
			if refs := pve.FindUnusedDiskEntries(client.state.configs[777]); refs["unused0"] != volume {
				t.Fatalf("777's unused entry moved: %v", client.state.configs[777])
			}
			if disks := qemu.ParseDisks(client.state.configs[888]); len(disks) != 0 || client.resizeCalls != 0 {
				t.Fatalf("a refused call attached %v or resized %d times", disks, client.resizeCalls)
			}
		})
	}
}

// TestStrandedManagedDiskPlacementRefuses is the managed placement row. A
// create_vm that brings the disk can't be planned around an unused entry,
// the same way it can't around an interrupted transfer.
func TestStrandedManagedDiskPlacementRefuses(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	managedStrand(t, client, cid)
	_, err := ObserveStorageExistingVolumes(context.Background(), deps, []string{cid})
	if err == nil || !strings.Contains(err.Error(), "existing disk is stranded on an unused entry: "+cid) {
		t.Fatalf("placement = %v, want the stranded plan error", err)
	}
}

// TestStrandedManagedDiskExplicitCleanupRefuses is the explicit cleanup row.
// Before the guard only an attached holder refused, so cleanup deleted the
// volume while 777's unused entry still named it.
func TestStrandedManagedDiskExplicitCleanupRefuses(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, false)
	managedStrand(t, client, cid)
	h, err := journal.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := cleanupManagedDiskAllocation(context.Background(), deps, journal, h); err == nil || !strings.Contains(err.Error(), "cleanup requires detached or parked persistent disk") {
		t.Fatalf("explicit cleanup = %v, want the detached-or-parked refusal", err)
	}
	if client.deletes != 0 {
		t.Fatal("explicit cleanup deleted the stranded volume")
	}
}

// TestStrandedManagedDiskCleanupPrecheckRefuses is the storage cleanup
// precheck row. Before the guard it admitted the cleanup.
func TestStrandedManagedDiskCleanupPrecheckRefuses(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	managedStrand(t, client, cid)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	report := StorageAllocationAudit{Evidence: []StorageAllocationEvidence{{AllocationID: id, Kind: allocationKindDisk}}}
	if _, err := storageCleanupDiskOwnership(context.Background(), deps, record, report, nil); err == nil || !strings.Contains(err.Error(), "disk cleanup requires managed detach before deleting an attached disk") {
		t.Fatalf("cleanup precheck = %v, want the managed detach refusal", err)
	}
}

// TestStrandedManagedDiskDeleteSubmissionRefuses is the delete pre-submission
// row. The disk is free when delete_disk is admitted, and an unused entry
// names it by the time the delete goes out. Before the guard, the last check
// looked only for a holder or an intent and let the delete through.
func TestStrandedManagedDiskDeleteSubmissionRefuses(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	volume := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	delete(client.state.configs[777], "scsi1")
	rd, err := resolveDeleteDiskCID(context.Background(), deps, cid)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(context.Background(), deps, rd, "delete_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("admission: %v", err)
	}
	client.state.configs[777]["unused0"] = volume
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		t.Fatal(err)
	}
	deleteErr := deleteDiskVolume(context.Background(), local, cid, volume, storage, "n1")
	_ = lifecycle.finish(context.Background(), deleteErr, true)
	if deleteErr == nil || client.deletes != 0 {
		t.Fatalf("the delete went out while 777's unused entry named the volume: err=%v deletes=%d", deleteErr, client.deletes)
	}
}

// TestStrandedManagedTerminalRecordRefuses is the terminal record row. After
// a completed delete, an unused entry that still names the volume is
// ownership the record can't explain. Before the guard, delete_disk read the
// record as already deleted and returned success.
func TestStrandedManagedTerminalRecordRefuses(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	value := client.state.configs[777]["scsi1"].(string)
	volume := strings.Split(value, ",")[0]
	delete(client.state.configs[777], "scsi1")
	if err := callHandler(t, HandleDeleteDisk(deps), cid); err != nil {
		t.Fatalf("delete_disk: %v", err)
	}
	// 777 keeps the disk's note, so the entry resolves as the disk and the
	// terminal record is what refuses it.
	token, _ := pve.StableIDFromDriveOptStr(value)
	noteAttachedDisk(t, client.state.configs[777], token, cid)
	client.state.configs[777]["unused0"] = volume
	if err := callHandler(t, HandleDeleteDisk(deps), cid); err == nil || !strings.Contains(err.Error(), "terminal managed disk still has ownership provenance") {
		t.Fatalf("delete_disk after the delete = %v, want the terminal ownership refusal", err)
	}
}

// TestStrandedManagedTerminalRecordWithoutTheNoteNeedsAnAudit is the terminal
// record row with 777's note gone. Nothing then proves the unused entry holds
// the disk rather than another volume at its birth name, so delete_disk asks
// for an audit before the terminal record is read, and deletes nothing. Before
// the fix the entry resolved as the disk and the terminal record refused it.
func TestStrandedManagedTerminalRecordWithoutTheNoteNeedsAnAudit(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	value := client.state.configs[777]["scsi1"].(string)
	volume := strings.Split(value, ",")[0]
	delete(client.state.configs[777], "scsi1")
	if err := callHandler(t, HandleDeleteDisk(deps), cid); err != nil {
		t.Fatalf("delete_disk: %v", err)
	}
	dropAttachedDiskNotes(t, client.state.configs[777])
	client.state.configs[777]["unused0"] = volume
	deletes := client.deletes
	err := callHandler(t, HandleDeleteDisk(deps), cid)
	token, _ := pve.StableIDFromDriveOptStr(value)
	var typed *cpierrors.Error
	if err == nil || !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("delete_disk after the delete = %v, want a permanent CloudError", err)
	}
	for _, want := range []string{
		"no slot carries the disk's serial " + token,
		"unused entry unused0 of VM 777",
		"Nothing acts on the volume until an operator confirms whose it is",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("delete_disk after the delete = %v\nwant it to contain %q", err, want)
		}
	}
	if client.deletes != deletes {
		t.Fatalf("delete_disk deleted %d volumes, want none", client.deletes-deletes)
	}
}

// TestManagedBirthCountsAnUnusedEntryAsACollision is the collision row. A
// fresh token whose would-be volume an unused entry already names is a
// collision, as a holder or an intent is. Before the guard the create went on.
func TestManagedBirthCountsAnUnusedEntryAsACollision(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	m.deps.Config.DetachedDiskStrategy = "parked"
	m.deps.Config.DiskVMIDRangeStart = 20000
	m.deps.Config.DiskVMIDRangeEnd = 20001
	if state.configs == nil {
		state.configs = map[int]map[string]any{}
	}
	state.configs[555] = map[string]any{"name": "w555", "digest": "1"}
	// The allocator draws either VMID in the band, so an unused entry names
	// the volume each draw would create.
	for i, vmid := range []int{20000, 20001} {
		name, err := pve.AllocationVolumeName(vmid, m.plan.Namespace, m.id, m.format)
		if err != nil {
			t.Fatal(err)
		}
		state.configs[555][fmt.Sprintf("unused%d", i)] = fmt.Sprintf("%s:%d/%s", m.plan.Targets[0].StorageID, vmid, name)
	}
	if _, err := m.executeAttempt(t.Context(), h); err == nil || !strings.Contains(err.Error(), "shortened token collision") {
		t.Fatalf("executeAttempt = %v, want the collision refusal", err)
	}
	if len(state.created) != 0 {
		t.Fatal("the collision created a volume")
	}
}

// TestStrandedDiskRunbookHeadingExists pins the heading the stranded-disk
// refusals quote to a heading that exists in docs/troubleshooting.md.
func TestStrandedDiskRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.TrimSuffix(strings.TrimPrefix(strandedRunbookPointer, `see "`), `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`)
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == heading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", heading)
}

// TestStrandedManagedDiskDetachParksIt is the managed detach row. A
// journal-managed disk stranded on 777's unused entry goes through the
// lifecycle guard, and detach_disk(777) moves it off the entry onto a parker
// with one reference. The record goes back to ready_to_return, so the guard
// admitted every step. Before the guard, detach_disk parked the volume by
// config edit while 777's unused entry still named it.
func TestStrandedManagedDiskDetachParksIt(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	managedStrand(t, client, cid)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := detachDiskAt(t, context.Background(), deps, "777", cid); err != nil {
		t.Fatalf("detach_disk(777): %v", err)
	}
	s := &strandedDisk{deps: deps, client: client, journal: journal, cid: cid, token: record.DiskToken}
	s.requireSingleHolder(t, true)
	if unused := pve.FindUnusedDiskEntries(client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 still holds unused entries %v after detach_disk succeeded", unused)
	}
	after, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State == aj.ReconciliationRequired || after.State != aj.ReadyToReturn || after.Reason != "" {
		t.Fatalf("the record ended in %s (reason %q), want %s with no reason", after.State, after.Reason, aj.ReadyToReturn)
	}
	// A poisoned guard leaves its step planned. Every step this detach
	// journaled is observed, and the move off the unused entry is among them.
	moveObserved := false
	for _, step := range after.Steps[len(record.Steps):] {
		if step.State != aj.Observed {
			t.Fatalf("step %s (%s) is %s, want observed", step.ID, step.Kind, step.State)
		}
		if strings.Contains(step.Kind, "CreateQemuMoveDisk") {
			moveObserved = true
		}
	}
	if !moveObserved {
		t.Fatalf("no observed move step among the detach's steps: %+v", after.Steps[len(record.Steps):])
	}
}

// TestStrandedAttachRunsTheSnapshotGuardOnTheStrandedVM is the snapshot row.
// Moving the disk off 777 detaches it from 777, so a snapshot there blocks the
// attach the way it blocks detach_disk. Before the guard, the attach put the
// volume on 888 while 777's unused entry still named it.
func TestStrandedAttachRunsTheSnapshotGuardOnTheStrandedVM(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	s.client.vmSnapshots = map[int][]map[string]any{777: {{"name": "before-upgrade"}}}
	before, deletes := s.snapshot(), s.client.deletes
	err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	want := fmt.Sprintf("attach_disk: refusing to attach disk %s to VM 888, because the attach first moves it off unused entry unused0 of VM 777 on node n1, "+
		"which detaches it from VM 777, and VM 777 (node n1) has 1 snapshot(s) [before-upgrade]", s.cid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeSnapshotBlocked) || !strings.Contains(err.Error(), want) {
		t.Fatalf("attach_disk(888) = %v\nwant a SnapshotBlocked refusal containing %q", err, want)
	}
	s.requireUnchanged(t, before, deletes)
}

// TestStrandedAttachRetriesAFailedMove is the attach retry row. A move off 777
// that fails leaves the transfer record behind, so attach_disk fails retriable,
// and the retry resumes the transfer and lands the disk on 888 with one
// reference. Before the guard, the first attach never moved the disk at all.
func TestStrandedAttachRetriesAFailedMove(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	s.client.moveErr = errors.New("move_disk: storage migration failed")
	err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("attach_disk(888) with a failing move = %v, want a retriable error", err)
	}
	s.client.moveErr = nil
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("retried attach_disk(888): %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("the disk landed on VM %d, want 888", vmid)
	}
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 still holds unused entries %v after the attach", unused)
	}
}

// TestStrandedPlainCreateVMMovesTheDiskOffTheStrandedVM is the plain create_vm
// row. The attach guard moves the disk off 777 onto a parker and returns the
// parked plan, which create_vm then carries out. Before the guard, it planned
// an ordinary attach of the volume 777 still named.
func TestStrandedPlainCreateVMMovesTheDiskOffTheStrandedVM(t *testing.T) {
	s := buildBirthStrand(t, 777, false, false)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	bare, meta, err := decodeDiskCID(context.Background(), s.deps, "create_vm", s.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), s.deps, "create_vm", s.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := guardAndUnparkBeforeAttach(context.Background(), s.deps, "create_vm", &rd, "n1", 888)
	if err != nil {
		t.Fatalf("attach guard for create_vm: %v", err)
	}
	if !plan.viaTransfer || !plan.parker.IsParker {
		t.Fatalf("the guard returned plan %+v, want the parked transfer plan", plan)
	}
	s.requireSingleHolder(t, true)
	if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
		t.Fatalf("777 still holds unused entries %v after the guard moved the disk", unused)
	}
}
