package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// legacyFileEphemeral is the volume ID create_vm gives VM 777's ephemeral
// disk on the file-backed storage "a", where the name sits below the VMID's
// directory.
const legacyFileEphemeral = "a:777/vm-777-ephemeral-0.raw"

// TestFindEphemeralActiveDisksMatchesEveryProducedName feeds the matcher the
// volume ID that pve.EphemeralVolumeName produces for every storage type and
// format it accepts, so a change to how ephemeral volumes are named breaks
// this test before it can hide a disk from retention.
func TestFindEphemeralActiveDisksMatchesEveryProducedName(t *testing.T) {
	t.Parallel()
	types := []string{
		pve.StorageTypeDir, pve.StorageTypeLVM, pve.StorageTypeLVMThin, pve.StorageTypeRBD, pve.StorageTypeCephFS, pve.StorageTypeNFS,
		pve.StorageTypeCIFS, pve.StorageTypeGlusterFS, pve.StorageTypeZFSPool, pve.StorageTypePBS, pve.StorageTypeBTRFS,
	}
	produced := map[bool]int{}
	for _, storageType := range types {
		for _, format := range []string{"raw", "qcow2", "vmdk"} {
			_, suffix, err := pve.EphemeralVolumeName(storageType, format, 777)
			if err != nil {
				continue
			}
			produced[pve.StorageUsesFileVolumes(storageType)]++
			volume := "e:" + suffix
			found := findEphemeralActiveDisks(map[string]any{"scsi1": volume + ",size=5G"}, 777)
			if len(found) != 1 || found["scsi1"] != volume {
				t.Errorf("%s/%s: matcher missed the produced volume %q: %v", storageType, format, volume, found)
			}
			for _, foreign := range []int{77, 778, 7777} {
				_, other, err := pve.EphemeralVolumeName(storageType, format, foreign)
				if err != nil {
					t.Fatal(err)
				}
				if found := findEphemeralActiveDisks(map[string]any{"scsi1": "e:" + other + ",size=5G"}, 777); len(found) != 0 {
					t.Errorf("%s/%s: VM 777 claimed VM %d's ephemeral volume: %v", storageType, format, foreign, found)
				}
			}
		}
	}
	if produced[true] == 0 || produced[false] == 0 {
		t.Fatalf("the producer named no file-backed or no block-backed volume: %v", produced)
	}
	for _, volume := range []string{
		"e:777/vm-777-disk-0.raw", "e:vm-777-disk-0", "e:777/vm-777-cloudinit.qcow2", "e:vm-777-cloudinit",
		"e:777/base-777-ephemeral-0.raw", "none", "vm-777-ephemeral-0", "/dev/pve/vm-777-ephemeral-0",
	} {
		if found := findEphemeralActiveDisks(map[string]any{"scsi1": volume + ",size=5G"}, 777); len(found) != 0 {
			t.Errorf("non-ephemeral volume %q matched: %v", volume, found)
		}
	}
}

// legacyDestroySubmission is one DeleteQemu the CPI sent, with every guest's
// configuration and every volume as they stood when it went out.
type legacyDestroySubmission struct {
	node, vmid string
	params     nodes.DeleteQemuParams
	configs    map[int]map[string]any
	volumes    map[string]bool
}

type legacyDestroyRecorder struct {
	submissions []legacyDestroySubmission
}

// guestDestroys returns the submissions that destroyed the fixture's guest,
// VM 777.
func (r *legacyDestroyRecorder) guestDestroys() []legacyDestroySubmission {
	var out []legacyDestroySubmission
	for _, submission := range r.submissions {
		if submission.vmid == "777" {
			out = append(out, submission)
		}
	}
	return out
}

// legacyDestroyPVE records each DeleteQemu before it destroys the guest
// together with every volume its configuration still references, the way qm
// destroy does. The flow fake on its own refuses to destroy a guest that
// holds a volume, which would hide the loss these tests look for.
type legacyDestroyPVE struct {
	*lifecycleFlowPVE
	recorder *legacyDestroyRecorder
}

func (c legacyDestroyPVE) Nodes() nodes.Service {
	return legacyDestroyNodes{volumeDestroyingNodes: volumeDestroyingPVE{c.lifecycleFlowPVE}.Nodes().(volumeDestroyingNodes), recorder: c.recorder}
}

// QEMU accepts the fast path's fire-and-forget stop, which the flow fake
// does not model.
func (c legacyDestroyPVE) QEMU() qemu.Service {
	return legacyDestroyQEMU{Service: c.lifecycleFlowPVE.QEMU()}
}

type legacyDestroyQEMU struct{ qemu.Service }

// Cluster answers the HA and node-affinity cleanup that every legacy delete
// runs with an empty HA configuration, which the flow fake does not model.
func (c legacyDestroyPVE) Cluster() cluster.Service {
	return legacyDestroyCluster{Service: c.lifecycleFlowPVE.Cluster()}
}

type legacyDestroyCluster struct{ cluster.Service }

func (legacyDestroyCluster) DeleteHaRules(context.Context, string) error { return nil }
func (legacyDestroyCluster) DeleteHaResources(context.Context, string, *cluster.DeleteHaResourcesParams) error {
	return nil
}

func (legacyDestroyQEMU) Stop(context.Context, string, int) (string, error) {
	return "UPID:n1:stop", nil
}

type legacyDestroyNodes struct {
	volumeDestroyingNodes
	recorder *legacyDestroyRecorder
}

func (n legacyDestroyNodes) DeleteQemu(ctx context.Context, node, vmid string, p *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	submission := legacyDestroySubmission{node: node, vmid: vmid, configs: map[int]map[string]any{}, volumes: map[string]bool{}}
	if p != nil {
		submission.params = *p
	}
	for id, cfg := range n.c.state.configs {
		submission.configs[id] = maps.Clone(cfg)
	}
	for volume := range n.c.state.volumes {
		submission.volumes[volume] = true
	}
	n.recorder.submissions = append(n.recorder.submissions, submission)
	return n.volumeDestroyingNodes.DeleteQemu(ctx, node, vmid, p)
}

// legacyDestroyAgent accepts the agent cleanup that follows a destroy.
type legacyDestroyAgent struct{}

func (legacyDestroyAgent) Configure(context.Context, string, int, agent.AgentConfig) error {
	return nil
}
func (legacyDestroyAgent) Remove(context.Context, string, int) error { return nil }

// legacyRetainFileFixture is legacyRetainFlowFixture with the ephemeral disk
// named the way file-backed storage names it and held on scsi1. Its client
// records each destroy, and the journal it returns is the fixture's own,
// which the legacy path must never touch.
func legacyRetainFileFixture(t *testing.T) (Deps, *lifecycleFlowPVE, *legacyDestroyRecorder, *aj.Journal) {
	t.Helper()
	return legacyRetainVolumeFixture(t, legacyFileEphemeral, "scsi1")
}

// legacyRetainVolumeFixture is legacyRetainFileFixture for any ephemeral
// volume ID held on any slot, so a test can hold the block form or use an
// unused slot.
func legacyRetainVolumeFixture(t *testing.T, volume, slot string) (Deps, *lifecycleFlowPVE, *legacyDestroyRecorder, *aj.Journal) {
	t.Helper()
	deps, client, journal, _, _ := lifecycleFlowFixture(t)
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.volumes[volume] = client.state.volumes[original]
	delete(client.state.volumes, original)
	delete(client.state.configs[777], "scsi1")
	client.state.configs[777][slot] = volume
	if !strings.HasPrefix(slot, "unused") {
		client.state.configs[777][slot] = volume + ",size=5G"
	}
	client.state.configs[777]["tags"] = tagRetainEphemeral
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	recorder := &legacyDestroyRecorder{}
	deps.PVE = legacyDestroyPVE{lifecycleFlowPVE: client, recorder: recorder}
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	return deps, client, recorder, journal
}

// assertLegacyFileRetained requires exactly one destroy of VM 777, sent only
// after the ephemeral volume had left the guest for a parker slot that
// carries the volume's serial, and while the guest's description still
// recorded the source CID that names the volume and that serial.
func assertLegacyFileRetained(t *testing.T, deps Deps, client *lifecycleFlowPVE, recorder *legacyDestroyRecorder) {
	t.Helper()
	destroys := recorder.guestDestroys()
	if len(destroys) != 1 {
		t.Fatalf("VM 777 was destroyed %d times, want once", len(destroys))
	}
	submitted := destroys[0]
	if keep := submitted.params.DestroyUnreferencedDisks; keep == nil || *keep {
		t.Fatalf("the retain-path destroy did not force destroy-unreferenced-disks off: %v", keep)
	}
	guest := submitted.configs[777]
	for key, value := range guest {
		if isDiskOptionKey(key) && strings.Contains(fmt.Sprint(value), "vm-777-ephemeral-0") {
			t.Fatalf("DeleteQemu for VM 777 was submitted while %s still held %v, so the destroy frees the ephemeral volume", key, value)
		}
	}
	cid := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(guest))[legacyFileEphemeral]
	birth, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil || birth != legacyFileEphemeral || meta == nil || meta.ID == "" {
		t.Fatalf("guest 777 did not record the source CID when its destroy was submitted: cid=%q birth=%q err=%v", cid, birth, err)
	}
	holders := 0
	for vmid, cfg := range submitted.configs {
		for key, value := range cfg {
			text, _ := value.(string)
			if !isDiskOptionKey(key) || !strings.Contains(text, "serial="+meta.ID) {
				continue
			}
			holders++
			tags, _ := pve.ConfigString(cfg, jsonKeyTags)
			volume := strings.Split(text, ",")[0]
			if vmid == 777 || !tagsContain(tags, pve.ParkerTag) || strings.HasPrefix(key, "unused") || !submitted.volumes[volume] {
				t.Fatalf("serial %s was not on a parker's active slot when the destroy went out: VM %d %s=%s tags=%q present=%t", meta.ID, vmid, key, text, tags, submitted.volumes[volume])
			}
			if after := client.state.volumes[volume]; after == nil || after.Size != 5<<30 {
				t.Fatalf("parked volume %s did not survive the destroy with its contents", volume)
			}
		}
	}
	if holders != 1 {
		t.Fatalf("%d slots carried serial %s when the destroy went out, want one parker slot", holders, meta.ID)
	}
	if client.state.configs[777] != nil {
		t.Fatal("VM 777 survived its destroy")
	}
}

// assertJournalUntouched requires the fixture's allocation journal to read
// back exactly as it did before the legacy delete ran.
func assertJournalUntouched(t *testing.T, journal *aj.Journal, before []aj.Record) {
	t.Helper()
	after, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the legacy delete changed the allocation journal:\nbefore %+v\nafter  %+v", before, after)
	}
}

func journalRecords(t *testing.T, journal *aj.Journal) []aj.Record {
	t.Helper()
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestLegacyDeleteVMRetainsFileBackedEphemeral(t *testing.T) {
	deps, client, recorder, journal := legacyRetainFileFixture(t)
	before := journalRecords(t, journal)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_vm: %v", err)
	}
	assertLegacyFileRetained(t, deps, client, recorder)
	if client.moves != 1 {
		t.Fatalf("retention moved the volume %d times, want once", client.moves)
	}
	assertJournalUntouched(t, journal, before)
}

func TestLegacyFastPathDeleteVMRetainsFileBackedEphemeral(t *testing.T) {
	deps, client, recorder, journal := legacyRetainFileFixture(t)
	fast := true
	deps.Config.FastPathDelete = &fast
	before := journalRecords(t, journal)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatalf("fast-path delete_vm: %v", err)
	}
	assertLegacyFileRetained(t, deps, client, recorder)
	if skiplock := recorder.guestDestroys()[0].params.Skiplock; skiplock == nil || !*skiplock {
		t.Fatalf("the fast path's destroy did not carry skiplock: %v", skiplock)
	}
	assertJournalUntouched(t, journal, before)
}

func TestLegacyStragglerSweepRetainsFileBackedEphemeral(t *testing.T) {
	deps, client, recorder, journal := legacyRetainFileFixture(t)
	client.state.configs[777]["tags"] = tagRetainEphemeral + ";" + tagDeletingVM
	before := journalRecords(t, journal)
	sweepFastDeleteStragglers(context.Background(), deps, deps.Logger)
	assertLegacyFileRetained(t, deps, client, recorder)
	if skiplock := recorder.guestDestroys()[0].params.Skiplock; skiplock == nil || !*skiplock {
		t.Fatalf("the sweep's destroy did not carry skiplock: %v", skiplock)
	}
	assertJournalUntouched(t, journal, before)
}

// TestLegacyDeleteVMKeepsEphemeralInUnusedSlot is a control that passes with
// and without the matcher fix, in the block and the file form. A detach can
// leave the ephemeral volume on an unused slot, and the retention loop over
// unused entries matches it there. The refusal below is the evidence of that
// match, because a volume the loops miss leaves retention with nothing to do,
// and the delete would go on to destroy the guest. The identity scan reads
// only active slots, and an unused entry carries no serial, so retention finds
// no holder and refuses before it changes or destroys anything.
func TestLegacyDeleteVMKeepsEphemeralInUnusedSlot(t *testing.T) {
	for _, volume := range []string{"a:vm-777-ephemeral-0", legacyFileEphemeral} {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, journal := legacyRetainVolumeFixture(t, volume, "unused0")
			before := journalRecords(t, journal)
			_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			t.Logf("PROBE unused-slot %s delete_vm error=%v destroys=%d moves=%d", volume, err, len(recorder.submissions), client.moves)
			if err == nil || !strings.Contains(err.Error(), "retained ephemeral ownership is ambiguous") {
				t.Fatalf("delete_vm did not refuse the unused-slot volume it cannot attribute: %v", err)
			}
			if len(recorder.submissions) != 0 || client.moves != 0 || client.state.volumes[volume] == nil || client.state.configs[777]["unused0"] != volume {
				t.Fatalf("refused delete changed the guest or its volume: destroys=%d moves=%d", len(recorder.submissions), client.moves)
			}
			assertJournalUntouched(t, journal, before)
		})
	}
}

// TestLegacyDeleteVMRerunRetainsFileBackedEphemeral fails the first delete's
// transfer, which must leave the guest and its volume alone, and requires the
// Director's rerun to park the volume once before it destroys the guest.
func TestLegacyDeleteVMRerunRetainsFileBackedEphemeral(t *testing.T) {
	deps, client, recorder, journal := legacyRetainFileFixture(t)
	before := journalRecords(t, journal)
	client.moveErr = errors.New("transfer refused")
	args := []json.RawMessage{planJSON(t, "777")}
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatalf("delete_vm succeeded although its transfer failed; destroys submitted=%d", len(recorder.guestDestroys()))
	}
	if len(recorder.submissions) != 0 || client.state.volumes[legacyFileEphemeral] == nil {
		t.Fatalf("failed retention destroyed the guest or its volume: destroys=%d", len(recorder.submissions))
	}
	client.moveErr = nil
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("rerun delete_vm: %v", err)
	}
	assertLegacyFileRetained(t, deps, client, recorder)
	if client.moves != 1 {
		t.Fatalf("rerun moved the volume %d times, want once", client.moves)
	}
	assertJournalUntouched(t, journal, before)
}

// TestLegacyRetentionRetryAfterFileBackedTransferMovesOnce runs retention a
// second time after it succeeded, as a delete_vm that failed after its
// retention would, and requires the second run to find the parked volume
// through the recorded source CID instead of moving anything again.
func TestLegacyRetentionRetryAfterFileBackedTransferMovesOnce(t *testing.T) {
	deps, client, _, _ := legacyRetainFileFixture(t)
	for attempt := range 2 {
		retained, err := detachRetainedEphemeralDisk(context.Background(), deps, "n1", "777", 777, log.NewNopLogger())
		if err != nil || !retained {
			t.Fatalf("attempt %d: retained=%t err=%v", attempt, retained, err)
		}
		if client.moves != 1 || client.state.volumes[legacyFileEphemeral] != nil {
			t.Fatalf("attempt %d: moves=%d, source volume still named for VM 777=%t", attempt, client.moves, client.state.volumes[legacyFileEphemeral] != nil)
		}
	}
}
