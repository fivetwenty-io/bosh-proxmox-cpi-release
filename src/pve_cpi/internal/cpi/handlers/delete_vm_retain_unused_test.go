package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// legacyEphemeralForms are VM 777's ephemeral volume as block storage and as
// file-backed storage name it.
var legacyEphemeralForms = []string{"a:vm-777-ephemeral-0", legacyFileEphemeral}

func TestLegacyFastPathDeleteVMRetainsEphemeralInUnusedSlot(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, journal := legacyRetainVolumeFixture(t, volume, "unused0")
			fast := true
			deps.Config.FastPathDelete = &fast
			before := journalRecords(t, journal)
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
				t.Fatalf("fast-path delete_vm refused the unused-slot volume its own VM holds: %v", err)
			}
			assertLegacyRetained(t, deps, client, recorder, volume)
			if skiplock := recorder.guestDestroys()[0].params.Skiplock; skiplock == nil || !*skiplock {
				t.Fatalf("the fast path's destroy did not carry skiplock: %v", skiplock)
			}
			assertJournalUntouched(t, journal, before)
		})
	}
}

// TestLegacyStragglerSweepRetainsEphemeralInUnusedSlot covers a fast-path
// delete that stamped bosh-deleting and then stopped with the ephemeral
// volume on an unused slot. The sweep used to skip such a straggler on every
// pass and leave it tagged forever.
func TestLegacyStragglerSweepRetainsEphemeralInUnusedSlot(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, journal := legacyRetainVolumeFixture(t, volume, "unused0")
			client.state.configs[777]["tags"] = tagRetainEphemeral + ";" + tagDeletingVM
			before := journalRecords(t, journal)
			sweepFastDeleteStragglers(context.Background(), deps, deps.Logger)
			assertLegacyRetained(t, deps, client, recorder, volume)
			if skiplock := recorder.guestDestroys()[0].params.Skiplock; skiplock == nil || !*skiplock {
				t.Fatalf("the sweep's destroy did not carry skiplock: %v", skiplock)
			}
			assertJournalUntouched(t, journal, before)
		})
	}
}

// dropParkerProvenance clears every parker's description, which removes the
// records the next provenance write would collect once they outlive the
// grace window.
func dropParkerProvenance(client *lifecycleFlowPVE) {
	for vmid, cfg := range client.state.configs {
		tags, _ := pve.ConfigString(cfg, jsonKeyTags)
		if vmid != 777 && tagsContain(tags, pve.ParkerTag) {
			cfg["description"] = ""
		}
	}
}

// TestLegacyDeleteVMRerunRetainsEphemeralAfterIntentCollected stops the first
// delete's transfer after it deleted the source slot, which leaves the volume
// on an unused slot, and then drops the transfer's intent record the way the
// parker's provenance collection does after its grace window. The rerun finds
// no holder and no intent, and it must still park the volume.
func TestLegacyDeleteVMRerunRetainsEphemeralAfterIntentCollected(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, journal := legacyRetainVolumeFixture(t, volume, "scsi1")
			before := journalRecords(t, journal)
			client.moveErr = errors.New("transfer refused")
			args := []json.RawMessage{planJSON(t, "777")}
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
				t.Fatal("delete_vm succeeded although its transfer failed")
			}
			if len(recorder.submissions) != 0 || client.state.configs[777]["unused0"] != volume {
				t.Fatalf("the stopped transfer did not leave the volume on unused0: destroys=%d unused0=%v", len(recorder.submissions), client.state.configs[777]["unused0"])
			}
			dropParkerProvenance(client)
			client.moveErr = nil
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
				t.Fatalf("rerun delete_vm after the intent was collected: %v", err)
			}
			assertLegacyRetained(t, deps, client, recorder, volume)
			if client.moves != 1 {
				t.Fatalf("retention moved the volume %d times, want once", client.moves)
			}
			assertJournalUntouched(t, journal, before)
		})
	}
}

// TestLegacyUnusedSlotTransferFailingAgainResumesThroughIntent fails the
// transfer that the unused-slot rule starts. The transfer writes its intent
// record before the move, so the next rerun must resolve that intent and
// resume onto the recorded parker with one move, rather than run the rule a
// second time.
func TestLegacyUnusedSlotTransferFailingAgainResumesThroughIntent(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, journal := legacyRetainVolumeFixture(t, volume, "unused0")
			before := journalRecords(t, journal)
			client.moveErr = errors.New("transfer refused")
			args := []json.RawMessage{planJSON(t, "777")}
			_, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{})
			if err == nil || !strings.Contains(err.Error(), "transfer refused") {
				t.Fatalf("the unused-slot transfer did not reach its move: %v", err)
			}
			if len(recorder.submissions) != 0 || client.moves != 0 || client.state.configs[777]["unused0"] != volume {
				t.Fatalf("the failed transfer changed the guest: destroys=%d moves=%d", len(recorder.submissions), client.moves)
			}
			cid := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(client.state.configs[777]))[volume]
			birth, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
			if err != nil || birth != volume {
				t.Fatalf("the source CID was not recorded: cid=%q err=%v", cid, err)
			}
			identity, err := resolveDiskForOp(context.Background(), deps, "test", cid, volume, meta)
			if err != nil || identity.intent == nil || identity.holder != nil {
				t.Fatalf("the failed transfer left no resumable intent: intent=%+v holder=%+v err=%v", identity.intent, identity.holder, err)
			}
			parker := identity.intent.ParkerVMID
			client.moveErr = nil
			if _, err := HandleDeleteVM(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
				t.Fatalf("rerun delete_vm: %v", err)
			}
			assertLegacyRetained(t, deps, client, recorder, volume)
			if client.moves != 1 {
				t.Fatalf("the resume moved the volume %d times, want once", client.moves)
			}
			holder := ""
			for key, value := range client.state.configs[parker] {
				if text, _ := value.(string); isDiskOptionKey(key) && strings.Contains(text, "serial="+meta.ID) {
					holder = key
				}
			}
			if holder == "" {
				t.Fatalf("the volume did not land on parker %d, the one its intent recorded", parker)
			}
			assertJournalUntouched(t, journal, before)
		})
	}
}

// assertLegacyUnusedRefused requires the ambiguous-ownership refusal with no
// move, no destroy, and the volume still on unused0 of VM 777.
func assertLegacyUnusedRefused(t *testing.T, err error, client *lifecycleFlowPVE, recorder *legacyDestroyRecorder, volume string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "retained ephemeral ownership is ambiguous") {
		t.Fatalf("retention did not refuse a volume it cannot attribute to VM 777: %v", err)
	}
	if len(recorder.submissions) != 0 || client.moves != 0 || client.state.volumes[volume] == nil || client.state.configs[777]["unused0"] != volume {
		t.Fatalf("the refusal changed the guest or its volume: destroys=%d moves=%d", len(recorder.submissions), client.moves)
	}
}

// TestLegacyDeleteVMRefusesUnusedSlotVolumeAnotherVMReferences gives VM 888 a
// reference to the volume as well, on an unused entry or on an active slot.
// Either one means VM 777 is not the volume's only holder.
func TestLegacyDeleteVMRefusesUnusedSlotVolumeAnotherVMReferences(t *testing.T) {
	for _, slot := range []string{"unused0", "scsi0"} {
		for _, volume := range legacyEphemeralForms {
			t.Run(slot+"/"+volume, func(t *testing.T) {
				deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "unused0")
				client.state.configs[888] = map[string]any{"name": "other", "digest": "1", slot: volume}
				_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
				assertLegacyUnusedRefused(t, err, client, recorder, volume)
				if client.state.configs[888][slot] != volume {
					t.Fatal("the refusal changed VM 888")
				}
			})
		}
	}
}

func TestLegacyDeleteVMRefusesEphemeralOnTwoUnusedSlots(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "unused0")
			client.state.configs[777]["unused1"] = volume
			_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			assertLegacyUnusedRefused(t, err, client, recorder, volume)
		})
	}
}

// TestLegacyRetentionRefusesForeignEphemeralOnUnusedSlot calls the retention
// directly, because the collection loops never pass a volume named for
// another VM. The rule's own name check must still refuse it.
func TestLegacyRetentionRefusesForeignEphemeralOnUnusedSlot(t *testing.T) {
	for _, volume := range []string{"a:vm-778-ephemeral-0", "a:778/vm-778-ephemeral-0.raw"} {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "unused0")
			err := retainLegacyEphemeralVolume(context.Background(), deps, "n1", "777", 777, volume, log.NewNopLogger())
			assertLegacyUnusedRefused(t, err, client, recorder, volume)
		})
	}
}

// unlistedNodePVE is legacyDestroyPVE on a cluster whose member n2 cannot be
// listed, and which /cluster/status reports offline, as a powered-off node
// would be.
type unlistedNodePVE struct{ legacyDestroyPVE }

func (c unlistedNodePVE) Nodes() nodes.Service {
	return unlistedNodeNodes{Service: c.legacyDestroyPVE.Nodes()}
}

func (c unlistedNodePVE) Cluster() cluster.Service {
	return unlistedNodeCluster{Service: c.legacyDestroyPVE.Cluster()}
}

type unlistedNodeNodes struct{ nodes.Service }

func (n unlistedNodeNodes) ListQemu(ctx context.Context, node string, p *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	if node == "n2" {
		return nil, errors.New("n2: connection refused (member is powered off)")
	}
	return n.Service.ListQemu(ctx, node, p)
}

type unlistedNodeCluster struct{ cluster.Service }

func (unlistedNodeCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := cluster.ListStatusResponse{json.RawMessage(`{"type":"cluster","quorate":1}`), json.RawMessage(`{"type":"node","name":"n1","online":1}`), json.RawMessage(`{"type":"node","name":"n2","online":0}`)}
	return &r, nil
}

// TestLegacyDeleteVMRefusesUnusedSlotVolumeWhenANodeIsExcluded runs the
// delete on a cluster with an unlisted member. The identity scan excludes the
// offline member, cannot prove the volume unattached, and fails retriable
// before the unused-slot rule runs, so nothing is moved or destroyed.
func TestLegacyDeleteVMRefusesUnusedSlotVolumeWhenANodeIsExcluded(t *testing.T) {
	for _, volume := range legacyEphemeralForms {
		t.Run(volume, func(t *testing.T) {
			deps, client, recorder, _ := legacyRetainVolumeFixture(t, volume, "unused0")
			deps.PVE = unlistedNodePVE{legacyDestroyPVE: deps.PVE.(legacyDestroyPVE)}
			_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
			t.Logf("PROBE excluded-node %s delete_vm error=%v", volume, err)
			if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("delete_vm did not fail retriable with a member unlisted: %v", err)
			}
			if len(recorder.submissions) != 0 || client.moves != 0 || client.state.configs[777]["unused0"] != volume {
				t.Fatalf("the refusal changed the guest: destroys=%d moves=%d", len(recorder.submissions), client.moves)
			}
		})
	}
}
