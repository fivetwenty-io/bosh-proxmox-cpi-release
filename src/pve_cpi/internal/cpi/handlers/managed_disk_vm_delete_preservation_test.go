package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

func TestManagedVMDeletePreservesManagedDiskAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	client.state.configs[777]["scsi0"] = "a:777/vm-777-disk-0.raw,size=10G"
	owned := map[string]bool{"a:777/vm-777-disk-0.raw": true}
	if err := detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, owned, nil); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || record.ID != id || client.moves != 1 {
		t.Fatalf("disk preservation lost identity: state=%s moves=%d", record.State, client.moves)
	}
	if _, ok := client.state.configs[777]["scsi1"]; ok {
		t.Fatal("persistent disk remains attached")
	}
	if _, ok := client.state.configs[777]["scsi0"]; !ok {
		t.Fatal("VM-owned root was changed")
	}
	if err := detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, owned, nil); err != nil {
		t.Fatal(err)
	}
	if client.moves != 1 {
		t.Fatal("repeat preservation moved disk again")
	}
}

func TestManagedVMDeleteUnknownVolumePreventsAllPreservation(t *testing.T) {
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	client.state.configs[777]["scsi2"] = "a:777/vm-777-disk-9.raw,size=1G"
	if err := detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, nil, nil); err == nil {
		t.Fatal("unknown volume accepted")
	}
	if client.moves != 0 {
		t.Fatal("mutated before completing all disk identity checks")
	}
}

func TestManagedVMDeleteVolumeEnumerationFailsClosed(t *testing.T) {
	cases := []map[string]any{
		{"scsi0": 32}, {"efidisk0": "/dev/disk/by-id/foreign"}, {"unused0": ""},
	}
	for _, config := range cases {
		if _, err := managedVMConfigVolumes(config); err == nil {
			t.Fatalf("invalid reference accepted: %v", config)
		}
	}
	result, err := managedVMConfigVolumes(map[string]any{"ide2": "none,media=cdrom", "tpmstate0": "a:777/vm-777-disk-1.raw", "unused0": "a:777/vm-777-disk-2.raw"})
	if err != nil || len(result) != 2 {
		t.Fatalf("volume enumeration: %v %v", result, err)
	}
}

func TestManagedVMDeletePreservesLegacyStableDiskWithExternalEvidence(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:123/vm-123-disk-0.raw"
	token, err := pve.GenerateDiskStableID()
	if err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",serial=" + token + ",size=5G"
	pve.UpdateAttachedDiskCID(context.Background(), client, deps.Log(context.Background()), "n1", 777, token, cid)
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}
	vm := handle.Record()
	vm.State = aj.Observed
	vm.CID = "777"
	if err := handle.Save(vm); err != nil {
		t.Fatal(err)
	}
	if err := detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, nil, handle); err != nil {
		t.Fatal(err)
	}
	after := handle.Record()
	if after.State != aj.Observed || len(after.Steps) == 0 || client.moves != 1 {
		t.Fatalf("external preservation incomplete: %+v", after)
	}
	for _, step := range after.Steps[1:] {
		if !step.Target.External || step.State != aj.Observed {
			t.Fatalf("foreign resource acquired VM ownership or lost evidence: %+v", step)
		}
	}
	birth, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := resolveDiskForOp(context.Background(), deps, "test", cid, birth, meta)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := attachExistingDiskToManagedVM(context.Background(), deps, handle, disk, "n1", 777)
	if err != nil {
		t.Fatal(err)
	}
	if slot == "" || client.moves != 2 {
		t.Fatal("legacy parker was not safely reattached")
	}
	for _, cfg := range client.state.configs {
		entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("legacy disk gained fabricated managed allocation marker")
		}
	}
}

func TestManagedVMDeletePreservesPreStableCIDWithoutDestructiveSweep(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		t.Run(map[bool]string{true: "unlinked", false: "owned-retained"}[foreign], func(t *testing.T) {
			testPreStablePreservation(t, foreign)
		})
	}
}

func TestManagedVMEphemeralRetentionPreservesRenamedOwnedVolume(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:777/vm-777-disk-9.raw"
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_ephemeral", aj.Target{Node: "n1", VMID: 777, Storage: "a", Backing: "nfs://nas/a", IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	retained, err := retainManagedEphemeralForVMDelete(context.Background(), deps, handle, "n1", 777, volume)
	if err != nil {
		t.Fatalf("%v steps=%+v config=%+v", err, handle.Record().Steps, client.state.configs)
	}
	target, targetErr := managedVMRetainedTarget(handle.Record(), retained, 777)
	if targetErr != nil || target.VMID == 777 || target.IntendedVolume != retained {
		t.Fatalf("retained target missing: %+v %v", target, targetErr)
	}
	if retained == volume || client.state.volumes[retained] == nil || client.moves != 1 {
		t.Fatalf("ephemeral retention failed: %s moves=%d", retained, client.moves)
	}
	for _, step := range handle.Record().Steps {
		if step.Target.External || step.State != aj.Observed {
			t.Fatalf("owned retention lost evidence: %+v", step)
		}
	}
	if _, ok := client.state.configs[777]["scsi1"]; ok {
		t.Fatal("retained volume still attached to deleting VM")
	}
	if target.VirtualBytes != 5<<30 {
		t.Fatal("retention lost actual virtual size")
	}
	if err := observeManagedRetainedEphemeral(context.Background(), deps, handle, target); err != nil {
		t.Fatal(err)
	}
	t.Run("moved_retainer_is_not_the_recorded_target", func(t *testing.T) {
		original := client.state.configs[target.VMID]
		moved := make(map[string]any, len(original))
		for key, value := range original {
			moved[key] = value
		}
		delete(original, "scsi0")
		client.state.configs[target.VMID+1] = moved
		t.Cleanup(func() {
			original["scsi0"] = moved["scsi0"]
			delete(client.state.configs, target.VMID+1)
		})
		if err := observeManagedRetainedEphemeral(context.Background(), deps, handle, target); err == nil {
			t.Fatal("a different retainer was accepted as the recorded target")
		}
	})
	delete(client.state.configs, 777)
	evidenceID, payload, err := aj.VerificationEvidence(aj.VMRetentionEvidence{VMID: 777, RetainedArtifacts: []aj.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	closed := handle.Record()
	closed.State = aj.VMDeletedRetained
	closed.Verifications = append(closed.Verifications, aj.Verification{EvidenceID: evidenceID, EvidenceJSON: payload, Complete: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true})
	if err := handle.Save(closed); err != nil {
		t.Fatal(err)
	}
	if err := cleanupManagedRetainedEphemeral(context.Background(), deps, handle, target); err != nil {
		t.Fatal(err)
	}
	if client.state.volumes[retained] != nil || handle.Record().State != aj.VMDeletedRetained {
		t.Fatal("retained cleanup deleted evidence or reopened VM generation")
	}
	for _, step := range handle.Record().Steps {
		if step.State != aj.Observed {
			t.Fatalf("retained cleanup unsettled: %+v", step)
		}
	}

}

func TestManagedVMNormalDeleteRetainsEphemeralAndClosesGeneration(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	prior, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:777/vm-777-ephemeral-0.raw"
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	frozenPlan, err := activeStorageAllocationPlan(prior)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := managedDiskActualDefinition(context.Background(), deps, "a")
	if err != nil {
		t.Fatal(err)
	}
	frozenPlan.Definitions["a"] = definition
	frozenPlan.AllocationKey = "vm-agent"
	intent := prior.Intent
	intent.Plan, err = json.Marshal(frozenPlan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	vmID := handle.Record().ID
	step, err := storageMutationIntent(handle, "vm.ephemeral.scsi1", aj.Target{Node: "n1", VMID: 777, Storage: "a", Backing: "nfs://nas/a", IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReadyToReturn
	record.CID = "777"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("vm-agent"))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: record.Namespace, AllocationID: vmID, AgentSHA256: hex.EncodeToString(hash[:])})
	if err != nil {
		t.Fatal(err)
	}
	client.state.configs[777]["description"] = marker
	client.state.configs[777]["tags"] = tagRetainEphemeral
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		report, auditErr := AuditStorageAllocations(context.Background(), deps, journal, []string{"n1", "n2"})
		t.Fatalf("%v audit=%+v auditerr=%v", err, report, auditErr)
	}
	after, err := journal.Inspect(vmID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.VMDeletedRetained || client.moves != 1 || client.state.configs[777] != nil {
		t.Fatalf("normal retain delete incomplete: state=%s moves=%d", after.State, client.moves)
	}
	if _, found, err := journal.InspectVM("vm-agent"); err != nil || found {
		t.Fatalf("retained VM generation remains active: %v %v", found, err)
	}
	fresh, err := journal.AcquireVM(context.Background(), "vm-agent", intent)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Record().ID == vmID {
		t.Fatal("retained generation reopened")
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

func testPreStablePreservation(t *testing.T, foreign bool) {
	t.Helper()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	volume := "a:777/vm-777-disk-9.raw"
	cid, err := pve.EncodeDiskCID(volume, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = client.state.volumes[old]
	delete(client.state.volumes, old)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	client.foreignUnlink = foreign
	pve.UpdateAttachedDiskCID(context.Background(), client, deps.Log(context.Background()), "n1", 777, volume, cid)
	handle, err := journal.AcquireVM(context.Background(), "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	step, err := storageMutationIntent(handle, "vm_create", aj.Target{Node: "n1", VMID: 777}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, nil, false); err != nil {
		t.Fatal(err)
	}
	err = detachManagedPersistentForVMDelete(context.Background(), deps, "n1", 777, nil, handle)
	if foreign && err != nil {
		t.Fatal(err)
	}
	if !foreign && err == nil {
		t.Fatal("VM-owned unused volume was treated as safely preserved")
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("physical volume deleted during preservation")
	}
	if client.moves != 0 || len(handle.Record().Steps) != 2 {
		t.Fatalf("unexpected rename or destructive sweep: %+v", handle.Record())
	}
	if foreign {
		birth, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := resolveDiskForOp(context.Background(), deps, "test", cid, birth, meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := attachExistingDiskToManagedVM(context.Background(), deps, handle, disk, "n1", 777); err != nil {
			t.Fatal(err)
		}
	}
	if !handle.Record().Steps[1].Target.External {
		t.Fatal("legacy volume gained VM allocation ownership")
	}
}
