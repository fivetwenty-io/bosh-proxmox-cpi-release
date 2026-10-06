package handlers

// A journal-managed operation writes description records, such as a parker's
// bosh_parked_disks record, through its guard. The writer builds each write
// from a read and sends that read's digest, and PVE checks the digest under
// the VM's lock before it writes. These rows cover the two races that used to
// send the allocation to reconciliation although nothing was lost: another
// writer changing the VM before PVE looked at our write, so PVE refused it,
// and another writer adding its own record after ours landed and before the
// guard read the VM back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

const (
	raceOurKey     = "bpd-ours"
	raceForeignKey = "bpd-foreign"
)

// descriptionRace says what another writer does to 777 around the next write
// the fake receives for it.
type descriptionRace struct {
	t *testing.T
	// refuseNext makes another writer add its record just before PVE looks
	// at the next write, so PVE refuses that write with its checksum text.
	refuseNext bool
	// foreignAfterNext makes another writer add its record right after the
	// next write lands.
	foreignAfterNext bool
	// dropOursNext makes the next write land without our record, which
	// stands for a write that did not apply as it was sent.
	dropOursNext bool
	refused      int
}

// descriptionRacePVE is the flow fake with 777's config writes raced as
// descriptionRace says.
type descriptionRacePVE struct {
	*lifecycleFlowPVE
	race *descriptionRace
}

func (c descriptionRacePVE) Nodes() nodes.Service {
	return descriptionRaceNodes{lifecycleFlowNodes: c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes), c: c.lifecycleFlowPVE, race: c.race}
}

type descriptionRaceNodes struct {
	lifecycleFlowNodes
	c    *lifecycleFlowPVE
	race *descriptionRace
}

func (n descriptionRaceNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	if vmidText != "777" {
		return n.lifecycleFlowNodes.UpdateQemuConfig(ctx, node, vmidText, p)
	}
	if n.race.refuseNext {
		n.race.refuseNext = false
		n.race.refused++
		raceEditRecords(n.race.t, n.c, func(records map[string]json.RawMessage) {
			records[raceForeignKey] = raceRecord(n.race.t, "local-lvm:vm-90000-disk-9")
		})
		return configChecksumMismatch(777)
	}
	if err := n.lifecycleFlowNodes.UpdateQemuConfig(ctx, node, vmidText, p); err != nil {
		return err
	}
	if n.race.foreignAfterNext {
		n.race.foreignAfterNext = false
		raceEditRecords(n.race.t, n.c, func(records map[string]json.RawMessage) {
			records[raceForeignKey] = raceRecord(n.race.t, "local-lvm:vm-90000-disk-9")
		})
	}
	if n.race.dropOursNext {
		n.race.dropOursNext = false
		raceEditRecords(n.race.t, n.c, func(records map[string]json.RawMessage) {
			delete(records, raceOurKey)
		})
	}
	return nil
}

// raceRecord is a young parked-disk record naming volid.
func raceRecord(t *testing.T, volid string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{
		"disk_cid": volid, "volid": volid, "node": "n1", "parked_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// withRaceRecords returns desc with its parked-disk records edited by edit.
func withRaceRecords(t *testing.T, desc string, edit func(map[string]json.RawMessage)) string {
	t.Helper()
	nonBOSH, raw := pve.ParseSentinel(desc)
	records := map[string]json.RawMessage{}
	if encoded, ok := raw["bosh_parked_disks"]; ok {
		if err := json.Unmarshal(encoded, &records); err != nil {
			t.Fatal(err)
		}
	}
	edit(records)
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	out, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// raceEditRecords is another writer's description write to 777, which moves
// the digest on.
func raceEditRecords(t *testing.T, c *lifecycleFlowPVE, edit func(map[string]json.RawMessage)) {
	t.Helper()
	cfg := c.state.configs[777]
	cfg["description"] = withRaceRecords(t, pve.DescriptionFromConfig(cfg), edit)
	c.generation++
	cfg["digest"] = fmt.Sprint(c.generation + 100)
}

// raceRecords returns 777's parked-disk records.
func raceRecords(t *testing.T, c *lifecycleFlowPVE) map[string]json.RawMessage {
	t.Helper()
	records := map[string]json.RawMessage{}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(c.state.configs[777]))
	if encoded, ok := raw["bosh_parked_disks"]; ok {
		if err := json.Unmarshal(encoded, &records); err != nil {
			t.Fatal(err)
		}
	}
	return records
}

// raceFixture opens a managed detach_disk lifecycle on the digest fixture's
// disk, with 777's writes raced through the returned descriptionRace.
func raceFixture(t *testing.T) (Deps, *managedDiskLifecycle, digestManaged, *descriptionRace) {
	t.Helper()
	f := digestManagedFixture(t)
	race := &descriptionRace{t: t}
	f.deps.PVE = descriptionRacePVE{lifecycleFlowPVE: f.client, race: race}
	bare, meta, err := decodeDiskCID(context.Background(), f.deps, "test", f.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), f.deps, "test", f.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(context.Background(), f.deps, rd, "detach_disk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.finish(context.Background(), errors.New("the row is over"), false) })
	return local, lifecycle, f, race
}

// raceWrite reads 777 through the guarded client and writes our record onto
// that read, with the read's digest when withDigest is set, plus whatever
// extra adds to the parameters.
func raceWrite(t *testing.T, local Deps, withDigest bool, extra func(*nodes.UpdateQemuConfigParams)) error {
	t.Helper()
	cfg, err := local.PVE.QEMU().Config(context.Background(), "n1", 777)
	if err != nil {
		t.Fatal(err)
	}
	desc := withRaceRecords(t, pve.DescriptionFromConfig(cfg), func(records map[string]json.RawMessage) {
		records[raceOurKey] = raceRecord(t, "local-lvm:vm-90000-disk-5")
	})
	params := &nodes.UpdateQemuConfigParams{Description: &desc}
	if withDigest {
		digest, _ := pve.ConfigString(cfg, "digest")
		params.Digest = &digest
	}
	if extra != nil {
		extra(params)
	}
	return local.PVE.Nodes().UpdateQemuConfig(context.Background(), "n1", "777", params)
}

// TestManagedDescriptionWrite_SettlesPVEsDigestRefusal is a guarded record
// write that PVE refuses because another writer changed 777 after our read.
// The refusal shows nothing was written, so the guard stays usable, and the
// writer's second round lands from a fresh read with the other writer's
// record intact.
func TestManagedDescriptionWrite_SettlesPVEsDigestRefusal(t *testing.T) {
	local, lifecycle, f, race := raceFixture(t)
	_, observedBefore := countSteps(t, f.journal, f.id, "_Nodes_UpdateQemuConfig")
	race.refuseNext = true

	err := raceWrite(t, local, true, nil)
	if !pve.IsConfigDigestRefusal(err) {
		t.Fatalf("first write: err = %v, want PVE's digest refusal", err)
	}
	if guardErr := lifecycle.guard.Err(); guardErr != nil {
		t.Fatalf("the guard was poisoned by a refusal that changed nothing: %v", guardErr)
	}
	if err := raceWrite(t, local, true, nil); err != nil {
		t.Fatalf("second write from a fresh read: %v", err)
	}
	if guardErr := lifecycle.guard.Err(); guardErr != nil {
		t.Fatalf("the guard was poisoned by the write that landed: %v", guardErr)
	}
	records := raceRecords(t, f.client)
	if _, ok := records[raceOurKey]; !ok {
		t.Errorf("our record did not land; records %v", records)
	}
	if _, ok := records[raceForeignKey]; !ok {
		t.Errorf("the other writer's record was overwritten; records %v", records)
	}
	if planned, observed := countSteps(t, f.journal, f.id, "_Nodes_UpdateQemuConfig"); planned != 0 || observed-observedBefore != 2 {
		t.Errorf("planned=%d observed=%d (from %d) config steps on 777, want both writes observed", planned, observed, observedBefore)
	}
	if record, inspectErr := f.journal.Inspect(f.id); inspectErr != nil || record.State == aj.ReconciliationRequired {
		t.Errorf("allocation state = %v (%v), want it out of reconciliation", record.State, inspectErr)
	}
}

// TestManagedDescriptionWrite_LeavesOtherRefusalsUncertain covers the
// refusals the guard can't settle from the refusal alone: a description
// write whose caller sent no digest of its own, so the digest PVE refused is
// one only the guard read, and a write that changes a disk key too.
func TestManagedDescriptionWrite_LeavesOtherRefusalsUncertain(t *testing.T) {
	cases := map[string]struct {
		withDigest bool
		extra      func(*nodes.UpdateQemuConfigParams)
	}{
		"no caller digest": {withDigest: false},
		"a disk key changes too": {withDigest: true, extra: func(p *nodes.UpdateQemuConfigParams) {
			slot := "scsi1"
			p.Delete = &slot
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			local, lifecycle, _, race := raceFixture(t)
			race.refuseNext = true

			err := raceWrite(t, local, tc.withDigest, tc.extra)
			if err == nil || race.refused != 1 {
				t.Fatalf("err = %v refused = %d, want PVE to have refused the write", err, race.refused)
			}
			if lifecycle.guard.Err() == nil {
				t.Error("the guard stayed usable after a refusal it can't settle")
			}
		})
	}
}

// TestManagedDescriptionWrite_IgnoresAnotherWritersRecordBeforeTheReadback
// is a guarded record write that lands, after which another writer adds its
// own record before the guard reads 777 back. Our record reads back as we
// wrote it, so the guard observes the write and stays usable.
func TestManagedDescriptionWrite_IgnoresAnotherWritersRecordBeforeTheReadback(t *testing.T) {
	local, lifecycle, f, race := raceFixture(t)
	_, observedBefore := countSteps(t, f.journal, f.id, "_Nodes_UpdateQemuConfig")
	race.foreignAfterNext = true

	if err := raceWrite(t, local, true, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if guardErr := lifecycle.guard.Err(); guardErr != nil {
		t.Fatalf("the guard was poisoned by another writer's record: %v", guardErr)
	}
	records := raceRecords(t, f.client)
	if _, ok := records[raceForeignKey]; !ok {
		t.Fatalf("the other writer's record is missing; records %v", records)
	}
	if planned, observed := countSteps(t, f.journal, f.id, "_Nodes_UpdateQemuConfig"); planned != 0 || observed-observedBefore != 1 {
		t.Errorf("planned=%d observed=%d (from %d) config steps on 777, want the write observed", planned, observed, observedBefore)
	}
}

// TestManagedDescriptionWrite_PoisonsWhenOurRecordDidNotLand is the control.
// A write whose own record doesn't read back as sent is still an outcome the
// guard can't explain, so the guard is poisoned.
func TestManagedDescriptionWrite_PoisonsWhenOurRecordDidNotLand(t *testing.T) {
	local, lifecycle, _, race := raceFixture(t)
	race.dropOursNext = true

	_ = raceWrite(t, local, true, nil)
	if lifecycle.guard.Err() == nil {
		t.Fatal("the guard stayed usable although our record did not read back")
	}
}

// parkReadbackRequest is the managed create_disk request whose park After
// hook reads parker 90000 back through p.
func parkReadbackRequest(p *parkSettlePVE) *managedDiskRequest {
	return &managedDiskRequest{deps: Deps{PVE: p}, plan: &StorageAllocationPlan{Node: parkSettleNode}, token: "bpd-ours"}
}

// parkRecordWrite is a guarded description write of our record onto the
// parker's current description, as the park's provenance write sends it.
func parkRecordWrite(t *testing.T, p *parkSettlePVE) (ManagedAllocationMutation, *sdkParams) {
	t.Helper()
	p.mu.Lock()
	desc := withRaceRecords(t, pve.DescriptionFromConfig(p.cfg), func(records map[string]json.RawMessage) {
		records["bpd-ours"] = raceRecord(t, "local-lvm:vm-90000-disk-5")
	})
	digest, _ := pve.ConfigString(p.cfg, "digest")
	p.mu.Unlock()
	params := &sdkParams{Description: &desc, Digest: &digest}
	call := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig",
		Args: map[string]any{resourceTypeNode: parkSettleNode, metadataKeyVMID: fmt.Sprint(parkSettleParker), managedArgumentParams: params}}
	return call, params
}

type sdkParams = nodes.UpdateQemuConfigParams

// TestObserveParkMutation_IgnoresAnotherHoldersRecordBeforeTheReadback is
// the managed park's After hook after its provenance write landed and another
// holder wrote its own record before the readback. Our record reads back as
// we wrote it, so the park observes the write instead of sending the
// allocation to reconciliation.
func TestObserveParkMutation_IgnoresAnotherHoldersRecordBeforeTheReadback(t *testing.T) {
	t.Parallel()

	p := newParkSettlePVE(t, map[string]any{})
	call, params := parkRecordWrite(t, p)
	if err := p.Nodes().UpdateQemuConfig(t.Context(), parkSettleNode, fmt.Sprint(parkSettleParker), params); err != nil {
		t.Fatalf("our write: %v", err)
	}
	p.mu.Lock()
	p.otherRequestAdds(t, "bpd-theirs", "local-lvm:vm-90000-disk-7")
	p.mu.Unlock()

	if err := parkReadbackRequest(p).observeParkMutation(t.Context(), nil, call, "step", nil, "local-lvm:vm-90000-disk-5"); err != nil {
		t.Fatalf("the park readback refused another holder's record: %v", err)
	}
}

// TestObserveParkMutation_RefusesOurRecordReadingBackDifferently is the
// control. When our own record doesn't read back as we wrote it, the park
// can't explain the outcome and still fails its readback.
func TestObserveParkMutation_RefusesOurRecordReadingBackDifferently(t *testing.T) {
	t.Parallel()

	p := newParkSettlePVE(t, map[string]any{})
	call, params := parkRecordWrite(t, p)
	if err := p.Nodes().UpdateQemuConfig(t.Context(), parkSettleNode, fmt.Sprint(parkSettleParker), params); err != nil {
		t.Fatalf("our write: %v", err)
	}
	p.mu.Lock()
	p.otherRequestAdds(t, "bpd-ours", "local-lvm:vm-90000-disk-8")
	p.mu.Unlock()

	err := parkReadbackRequest(p).observeParkMutation(t.Context(), nil, call, "step", nil, "local-lvm:vm-90000-disk-5")
	if err == nil || !strings.Contains(err.Error(), "readback mismatch") {
		t.Fatalf("err = %v, want a readback mismatch for our own record", err)
	}
}
