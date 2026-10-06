package handlers

// A stable-ID disk whose detach-side transfer deleted its slot and then failed
// to move the volume sits on the guest's unused entry with no serial. The
// parker's transfer record is then the only link from the disk's CID to the
// volume. These tests build that state through the real handlers, age the
// record past the collector's grace window with the provenance clock, and let
// an unrelated park write to the same parker an hour later, which is the write
// that used to collect it.

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
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// strandedEpoch is when the failing transfer runs. The collecting park runs
// two hours later, past the one-hour grace window.
var strandedEpoch = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

func atProvenanceTime(at time.Time) context.Context {
	return pve.WithProvenanceClock(context.Background(), func() time.Time { return at })
}

// strandedDisk is one built stranded state.
type strandedDisk struct {
	deps     Deps
	client   *lifecycleFlowPVE
	recorder *legacyDestroyRecorder
	journal  *aj.Journal
	cid      string
	token    string
	stranded string
	managed  bool
}

// buildStrandedDisk runs the real handlers to strand one stable-ID disk on
// guest 777 and then parks a second disk on the same parker two hours later.
//
// managed picks a journal-managed disk over a legacy one. renamed first parks
// the disk and attaches it back, so the volume is named for 777; otherwise it
// keeps its birth name, which for a legacy disk is the name 777 owns. local puts
// every volume on node-local storage.
//
// A managed disk can't be built with its birth name. The journal fixes that
// name on the disk band's VMID, which 777 doesn't own, so PVE keeps no unused
// entry for it, and a failing move is never attempted.
func buildStrandedDisk(t *testing.T, managed, renamed, local bool) *strandedDisk {
	t.Helper()
	if managed && !renamed {
		t.Fatal("a managed disk's birth name is never owned by 777, so it can't be stranded on an unused entry")
	}
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
	s := &strandedDisk{client: client, recorder: recorder, journal: journal, managed: managed}

	var otherCID string
	if managed {
		record, err := journal.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		s.cid, s.token = cid, record.DiskToken
		other := journalLifecycleFlowDisk(t, journal, client.state, true, false)
		client.state.configs[778] = map[string]any{"name": "w778", "digest": "1", "scsi1": other.volume + ",serial=" + other.token + ",size=5G"}
		otherCID = other.cid
	} else {
		original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
		delete(client.state.volumes, original)
		deps.Config.StoragePlacementNamespace = ""
		deps.Config.StorageAllocationJournalDir = ""
		legacyDisk := func(vmid, owner int) (string, string) {
			token, err := pve.GenerateDiskStableID()
			if err != nil {
				t.Fatal(err)
			}
			volume := fmt.Sprintf("a:%d/vm-%d-disk-0.raw", owner, owner)
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
		s.cid, s.token = legacyDisk(777, 777)
		otherCID, _ = legacyDisk(778, 124)
	}
	if local {
		deps.Resolver = pve.NewBackendResolver(deps.PVE, nil, "n1")
	}
	s.deps = deps

	before := atProvenanceTime(strandedEpoch)
	if renamed {
		if err := detachDiskAt(t, before, deps, "777", s.cid); err != nil {
			t.Fatalf("park the disk: %v", err)
		}
		if err := attachDiskAt(t, before, deps, "777", s.cid); err != nil {
			t.Fatalf("attach it back to 777: %v", err)
		}
	}
	client.moveErr = errors.New("move_disk: storage migration failed")
	if err := detachDiskAt(t, before, deps, "777", s.cid); err == nil {
		t.Fatal("the transfer whose move fails reported success")
	}
	client.moveErr = nil
	for _, volume := range pve.FindUnusedDiskEntries(client.state.configs[777]) {
		s.stranded = volume
	}
	if s.stranded == "" {
		t.Fatalf("the failed transfer left no unused entry on 777: %v", client.state.configs[777])
	}
	if s.intentParker() == 0 {
		t.Fatal("the failed transfer left no record on any parker")
	}

	if err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(2*time.Hour)), deps, "778", otherCID); err != nil {
		t.Fatalf("park an unrelated disk on the same parker two hours later: %v", err)
	}
	recorder.submissions = nil
	return s
}

func detachDiskAt(t *testing.T, ctx context.Context, deps Deps, vmCID, diskCID string) error {
	t.Helper()
	_, err := HandleDetachDisk(deps).Handle(ctx, []json.RawMessage{planJSON(t, vmCID), planJSON(t, diskCID)}, jsonrpc.Context{})
	return err
}

func attachDiskAt(t *testing.T, ctx context.Context, deps Deps, vmCID, diskCID string) error {
	t.Helper()
	_, err := HandleAttachDisk(deps).Handle(ctx, []json.RawMessage{planJSON(t, vmCID), planJSON(t, diskCID)}, jsonrpc.Context{})
	return err
}

// intentParker returns the parker whose record carries the disk's stable ID,
// or 0 when none does.
func (s *strandedDisk) intentParker() int {
	for vmid, cfg := range s.client.state.configs {
		tags, _ := cfg["tags"].(string)
		if !strings.Contains(tags, pve.ParkerTag) {
			continue
		}
		_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
		var disks map[string]json.RawMessage
		if json.Unmarshal(raw["bosh_parked_disks"], &disks) == nil {
			if _, ok := disks[s.token]; ok {
				return vmid
			}
		}
	}
	return 0
}

// references lists every config key that names volume.
func (s *strandedDisk) references(volume string) []string {
	var out []string
	for vmid, cfg := range s.client.state.configs {
		for key, value := range cfg {
			if isDiskOptionKey(key) && strings.Split(fmt.Sprint(value), ",")[0] == volume {
				out = append(out, fmt.Sprintf("%d.%s", vmid, key))
			}
		}
	}
	sort.Strings(out)
	return out
}

// serialHolders lists every slot that carries the disk's serial, with the
// volume it names.
func (s *strandedDisk) serialHolders() map[string]string {
	out := map[string]string{}
	for vmid, cfg := range s.client.state.configs {
		for key, value := range cfg {
			text, _ := value.(string)
			if isDiskOptionKey(key) && strings.Contains(text, "serial="+s.token) {
				out[fmt.Sprintf("%d.%s", vmid, key)] = strings.Split(text, ",")[0]
			}
		}
	}
	return out
}

// requireSingleHolder fails unless exactly one slot carries the serial, on a
// VM whose identity holds, naming a volume that exists and that nothing else
// references. It returns the holding VMID.
func (s *strandedDisk) requireSingleHolder(t *testing.T, onParker bool) int {
	t.Helper()
	holders := s.serialHolders()
	if len(holders) != 1 {
		t.Fatalf("slots carrying serial %s: %v, want exactly one", s.token, holders)
	}
	for slot, volume := range holders {
		var vmid int
		_, _ = fmt.Sscanf(slot, "%d.", &vmid)
		tags, _ := s.client.state.configs[vmid]["tags"].(string)
		if onParker != strings.Contains(tags, pve.ParkerTag) {
			t.Fatalf("serial %s is on VM %d (tags %q), want on a parker=%t", s.token, vmid, tags, onParker)
		}
		if s.client.state.volumes[volume] == nil {
			t.Fatalf("serial %s names %s, which does not exist", s.token, volume)
		}
		if refs := s.references(volume); len(refs) != 1 {
			t.Fatalf("%s is referenced by %v, want one reference", volume, refs)
		}
		return vmid
	}
	return 0
}

// strandedShapes are the legacy shapes the probe found: renamed or birth-named,
// on shared or node-local storage. A birth-named legacy disk is named for 777,
// the VMID the probe's disk was created under.
var strandedShapes = []struct {
	name           string
	renamed, local bool
}{
	{"renamed/shared", true, false},
	{"renamed/local", true, true},
	{"birth/shared", false, false},
	{"birth/local", false, true},
}

// TestStrandedTransferRecordSurvivesTheCollectingPark is A1. Before the keep
// rule every shape fails: the park two hours later collected the record. The
// managed disk has only its renamed shapes, because a managed disk's birth name
// is never owned by 777 and so can't be stranded.
func TestStrandedTransferRecordSurvivesTheCollectingPark(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, shape := range strandedShapes {
			if managed && (shape.local || !shape.renamed) {
				continue
			}
			t.Run(fmt.Sprintf("managed=%t/%s", managed, shape.name), func(t *testing.T) {
				s := buildStrandedDisk(t, managed, shape.renamed, shape.local)
				if s.intentParker() == 0 {
					t.Fatalf("the transfer record for %s was collected while VM 777 still names %s on %v", s.token, s.stranded, s.references(s.stranded))
				}
			})
		}
	}
}

// TestStrandedLegacyDetachResumesToOneParkedReference is A2 and A3. Before the
// keep rule each shape fails differently: node-local renamed reports success
// with the volume still on 777, shared renamed refuses or parks a volume that
// does not exist, and birth-named parks the volume while 777 still names it.
func TestStrandedLegacyDetachResumesToOneParkedReference(t *testing.T) {
	for _, shape := range strandedShapes {
		t.Run(shape.name, func(t *testing.T) {
			s := buildStrandedDisk(t, false, shape.renamed, shape.local)
			later := atProvenanceTime(strandedEpoch.Add(3 * time.Hour))
			if err := detachDiskAt(t, later, s.deps, "777", s.cid); err != nil {
				t.Fatalf("A2: the retried detach_disk failed: %v", err)
			}
			s.requireSingleHolder(t, true)
			if unused := pve.FindUnusedDiskEntries(s.client.state.configs[777]); len(unused) != 0 {
				t.Fatalf("A2: 777 still holds unused entries %v after detach_disk succeeded", unused)
			}

			if _, err := HandleDeleteVM(s.deps).Handle(later, []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
				t.Fatalf("A3: delete_vm(777): %v", err)
			}
			for _, destroy := range s.recorder.guestDestroys() {
				for key, value := range destroy.configs[777] {
					if isDiskOptionKey(key) {
						t.Fatalf("A3: VM 777 was destroyed while %s still named %v", key, value)
					}
				}
			}
			s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
			if err := attachDiskAt(t, later, s.deps, "888", s.cid); err != nil {
				t.Fatalf("A3: attach_disk(888): %v", err)
			}
			if vmid := s.requireSingleHolder(t, false); vmid != 888 {
				t.Fatalf("A3: the disk landed on VM %d, want 888", vmid)
			}
		})
	}
}

// TestStrandedManagedDetachRefusesAndKeepsItsRecord is A4. A managed disk stays
// refused at the planned move step, and the record that leads back to the
// volume survives for the settlement to use. Before the keep rule the record
// was collected, and the refusal said the disk was terminal instead of naming
// the step. Only the renamed shape exists for a managed disk, because its birth
// name is never owned by 777.
func TestStrandedManagedDetachRefusesAndKeepsItsRecord(t *testing.T) {
	s := buildStrandedDisk(t, true, true, false)
	err := detachDiskAt(t, atProvenanceTime(strandedEpoch.Add(3*time.Hour)), s.deps, "777", s.cid)
	if err == nil || !strings.Contains(err.Error(), "lifecycle_detach_disk_Nodes_CreateQemuMoveDisk") {
		t.Fatalf("detach_disk = %v, want the refusal that names the planned move step", err)
	}
	if s.client.state.volumes[s.stranded] == nil || !strings.Contains(strings.Join(s.references(s.stranded), " "), "777.unused") {
		t.Fatalf("the volume %s left 777's unused entry: present=%t refs=%v", s.stranded, s.client.state.volumes[s.stranded] != nil, s.references(s.stranded))
	}
	if s.intentParker() == 0 {
		t.Fatal("the transfer record was collected")
	}
}

// TestDeleteVMUnusedSlotRefusalPointsAtTheRecovery pins the refusal text. Before
// this change it ended at "verify pve.disk_storage configuration", and the
// operator's obvious next step, removing the unused entry, deletes a volume
// named for the VM. With the parker's record in place delete_vm finishes the
// transfer itself, so the row removes the record to reach the refusal.
func TestDeleteVMUnusedSlotRefusalPointsAtTheRecovery(t *testing.T) {
	s := buildStrandedDisk(t, false, true, true)
	s.dropTransferRecord(t)
	_, err := HandleDeleteVM(s.deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("delete_vm destroyed a VM that holds a stranded volume on an unused entry")
	}
	want := fmt.Sprintf("delete_vm: refusing to destroy VM 777 -- persistent volumes still attached as unused slots: [unused0=%s] "+
		"(call detach_disk first or verify pve.disk_storage configuration; "+
		"if detach_disk succeeds and the slot stays, do not remove the slot or destroy the VM by hand, because PVE then deletes a volume named for the VM; "+
		`see "delete_vm refuses to destroy VM with attached unused disks" in docs/troubleshooting.md of bosh-proxmox-cpi-release)`, s.stranded)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal = %q\nwant it to contain %q", err.Error(), want)
	}
	if len(s.recorder.submissions) != 0 {
		t.Fatalf("delete_vm submitted %d destroys", len(s.recorder.submissions))
	}
}

// TestUnusedSlotRecoveryRunbookHeadingExists pins the heading the refusal
// quotes to a heading that exists in docs/troubleshooting.md.
func TestUnusedSlotRecoveryRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.TrimSuffix(strings.TrimPrefix(unusedSlotRecoveryRunbook, `see "`), `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`)
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == heading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", heading)
}
