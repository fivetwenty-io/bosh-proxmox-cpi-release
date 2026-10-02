package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// attachMovedDisk attaches the fixture's managed disk to VM 777 on n1, which
// writes provenance naming n1, and then moves VM 777 to n2 outside BOSH.
func attachMovedDisk(t *testing.T, deps Deps, client *lifecycleFlowPVE, cid string) {
	t.Helper()
	delete(client.state.configs[777], "scsi1")
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	if entry := heldDiskProvenance(t, client, 777); entry.Node != "n1" {
		t.Fatalf("attach recorded provenance on %s, want n1", entry.Node)
	}
	deps.Config.DetachedDiskStrategy = "parked"
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[777] = "n2"
}

// heldDiskProvenance returns the one managed disk provenance entry vmid holds.
func heldDiskProvenance(t *testing.T, client *lifecycleFlowPVE, vmid int) pve.DiskAllocationProvenance {
	t.Helper()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(client.state.configs[vmid]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("VM %d provenance = %+v, %v; want one entry", vmid, entries, err)
	}
	for _, entry := range entries {
		return entry
	}
	return pve.DiskAllocationProvenance{}
}

// heldDiskVolume returns the volume VM 777 holds in a disk slot, or "".
func heldDiskVolume(client *lifecycleFlowPVE) string {
	for key, value := range client.state.configs[777] {
		if text, ok := value.(string); ok && isDiskOptionKey(key) && strings.Contains(text, ":") {
			return strings.Split(text, ",")[0]
		}
	}
	return ""
}

// TestManagedDiskDetachHealsProvenanceOfHolderMovedOnSharedStorage moves the
// holder of a shared managed disk to n2. Detach rewrites the provenance on n2
// before the transfer, so the removal after it matches and the detach
// completes in one call, parking the volume by config edit under its own name.
func TestManagedDiskDetachHealsProvenanceOfHolderMovedOnSharedStorage(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	attachMovedDisk(t, deps, client, cid)
	before, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("detach from a holder moved on shared storage failed: %v", err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	// The detach lifecycle journals its own mutations, including the heal,
	// but no earlier step is rewritten to name the new node.
	if len(record.Steps) < len(before.Steps) || !reflect.DeepEqual(record.Steps[:len(before.Steps)], before.Steps) {
		t.Fatal("detach rewrote recorded steps")
	}
	// The managed volume carries the disk band's VMID, which 777 doesn't own,
	// so PVE keeps no unused entry for it and the park is a config edit.
	if record.State != aj.ReadyToReturn || client.moves != 0 || heldDiskVolume(client) != "" {
		t.Fatalf("detach incomplete: state=%s moves=%d held=%q", record.State, client.moves, heldDiskVolume(client))
	}
	if entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(client.state.configs[777])); err != nil || len(entries) != 0 {
		t.Fatalf("former holder kept provenance: %+v %v", entries, err)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	requireConfigEditPark(t, client, record.DiskToken, bare)
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.holder == nil || !resolved.holder.IsParker || resolved.holder.Node != "n2" {
		t.Fatalf("disk not parked on the holder's node: %+v", resolved.holder)
	}
}

// TestManagedDiskDetachRefusesHolderMovedWithLocalStorage moves the holder
// and its node-local managed disk to n2. The audit accepts no move, so detach
// refuses before the transfer and the disk stays attached with its provenance.
func TestManagedDiskDetachRefusesHolderMovedWithLocalStorage(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	birth := relocateFixtureDiskToLocalStorage(t, deps, client, journal, id, cid)
	attachMovedDisk(t, deps, client, cid)
	client.volumeNodes[birth] = "n2"
	_, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	if message := directorMessage(err); !strings.Contains(message, "detach refused before transfer: disk allocation "+id) || !strings.Contains(message, "held by VM 777 on n2, provenance names n1") {
		t.Fatalf("local move not refused before transfer: %v", err)
	}
	// The refusal carries the audit's own reason for refusing the move.
	if message := directorMessage(err); !strings.Contains(message, "accepted no move on shared storage (") || !strings.Contains(message, "not a move: volume "+birth+" is node-local); the disk stays attached") {
		t.Fatalf("refusal lacks the audit's reason: %q", message)
	}
	if client.moves != 0 || heldDiskVolume(client) != birth {
		t.Fatalf("refused detach moved the disk: moves=%d held=%q", client.moves, heldDiskVolume(client))
	}
	if entry := heldDiskProvenance(t, client, 777); entry.Node != "n1" {
		t.Fatalf("refused detach rewrote provenance to %s", entry.Node)
	}
}

// TestManagedDiskDetachStopsWhenTheProvenanceHealFails fails the provenance
// rewrite. The detach stops before the transfer, so the disk is still
// attached and nothing is half done.
func TestManagedDiskDetachStopsWhenTheProvenanceHealFails(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	attachMovedDisk(t, deps, client, cid)
	volume := heldDiskVolume(client)
	client.descriptionErr = errors.New("description write lost")
	_, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{})
	if err == nil || !strings.Contains(directorMessage(err), "could not be moved to n2 before transfer") {
		t.Fatalf("failed heal not reported: %v", err)
	}
	if client.moves != 0 || heldDiskVolume(client) != volume {
		t.Fatalf("failed heal still transferred the disk: moves=%d held=%q", client.moves, heldDiskVolume(client))
	}
	if entry := heldDiskProvenance(t, client, 777); entry.Node != "n1" {
		t.Fatalf("failed heal changed provenance to %s", entry.Node)
	}
}

// relocateFixtureDiskToLocalStorage turns the fixture's shared storage into a
// node-local one and records an audited relocation there, the way
// TestManagedDiskLocalMigrationChargesDestinationAfterSetRemoval does, so the
// journal accepts the disk on local storage. It returns the disk's volume.
func relocateFixtureDiskToLocalStorage(t *testing.T, deps Deps, client *lifecycleFlowPVE, journal *aj.Journal, id, cid string) string {
	t.Helper()
	birth, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	pool, _, err := pve.ParseDiskCID(birth)
	if err != nil {
		t.Fatal(err)
	}
	client.localStorage = true
	client.volumeNodes = map[string]string{birth: "n1"}
	definition, err := managedDiskActualDefinition(context.Background(), deps, pool)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	session, err := beginStorageLifecycle(handle, "audited_external_relocation", lifecycleProof(t, "external-relocation-start", false))
	if err != nil {
		t.Fatal(err)
	}
	step, err := session.Intent("relocation", aj.Target{Node: "n1", VMID: 777, Storage: pool, Backing: definition.BackingKey(), IntendedVolume: birth})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Observed(step, []string{birth}); err != nil {
		t.Fatal(err)
	}
	if err := session.Finish(lifecycleProof(t, "external-relocation-complete", false), false); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	deps.Resolver = pve.NewBackendResolver(client, nil, "n1")
	return birth
}

// parkThenMoveParker parks the fixture's disk from VM 777 on n1 and then moves
// the parker to n2, the way a bulk migrate during a patch cycle does. The
// parked entry keeps naming n1. With renamed false the disk parks by config
// edit under its disk-band name, and with renamed true it runs the park,
// attach, park cycle, so it sits on the parker under a name the parker owns. It
// returns the parker's VMID.
func parkThenMoveParker(t *testing.T, deps Deps, client *lifecycleFlowPVE, cid string, renamed bool) int {
	t.Helper()
	deps.Config.DetachedDiskStrategy = "parked"
	if renamed {
		parkRenameCycle(t, context.Background(), deps, client, cid)
	} else if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	holder := resolvedFixtureHolder(t, deps, cid)
	if holder == nil || !holder.IsParker || holder.Node != "n1" {
		t.Fatalf("disk not parked on n1: %+v", holder)
	}
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[holder.VMID] = "n2"
	return holder.VMID
}

// parkNames are the two ways a managed disk sits on a parker. One park leaves
// it under its disk-band name, and the park, attach, park cycle renames it for
// the parker, so the audit then meets steps that name the volume's old names.
var parkNames = []struct {
	name    string
	renamed bool
}{{"disk-band name", false}, {"parker-owned name", true}}

func resolvedFixtureHolder(t *testing.T, deps Deps, cid string) *pve.DiskHolder {
	t.Helper()
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.holder
}

// TestManagedDiskParkerMovedOnSharedStorageStaysUsable walks every reader of
// a parker's node after a bulk migrate moved the parker. Admission accepts
// the move without touching the journal, attach takes the disk from the
// parker on its new node, and a later detach parks it on that node again.
func TestManagedDiskParkerMovedOnSharedStorageStaysUsable(t *testing.T) {
	for _, park := range parkNames {
		t.Run(park.name, func(t *testing.T) {
			parkerMovedOnSharedStorageStaysUsable(t, park.renamed)
		})
	}
}

func parkerMovedOnSharedStorageStaysUsable(t *testing.T, renamed bool) {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	parker := parkThenMoveParker(t, deps, client, cid, renamed)

	files := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	report, err := admitStorageAllocation(context.Background(), deps, journal, []string{"n1", "n2"})
	if err != nil {
		t.Fatalf("admission refused a parker moved on shared storage: %v", err)
	}
	if !report.observedMove(storageMoveKindParker, id, parker, "n2") {
		t.Fatalf("parker move not observed: %+v", report.ObservedMoves)
	}
	if !equalFiles(files, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("admission changed the journal")
	}

	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("attach from a moved parker failed: %v", err)
	}
	if client.migrations != 0 {
		t.Fatalf("attach migrated a parker that already runs on the target's node: %d", client.migrations)
	}
	if entry := heldDiskProvenance(t, client, 888); entry.Node != "n2" || entry.AllocationID != id {
		t.Fatalf("attach recorded %+v, want allocation %s on n2", entry, id)
	}

	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("re-park on the moved parker's node failed: %v", err)
	}
	holder := resolvedFixtureHolder(t, deps, cid)
	if holder == nil || !holder.IsParker || holder.Node != "n2" {
		t.Fatalf("disk not parked again on n2: %+v", holder)
	}
	if holder.VMID != parker {
		t.Fatalf("detach parked on VM %d instead of reusing moved parker %d", holder.VMID, parker)
	}
	record, err := journal.Inspect(id)
	if err != nil || record.State != aj.ReadyToReturn {
		t.Fatalf("disk record after re-park: %+v %v", record.State, err)
	}
}

// TestManagedDiskDeletesDiskFromParkerMovedOnSharedStorage covers parker
// cleanup: delete_disk removes a disk whose parker a bulk migrate moved.
func TestManagedDiskDeletesDiskFromParkerMovedOnSharedStorage(t *testing.T) {
	for _, park := range parkNames {
		t.Run(park.name, func(t *testing.T) {
			deps, client, journal, id, cid := lifecycleFlowFixture(t)
			parkThenMoveParker(t, deps, client, cid, park.renamed)
			if _, err := HandleDeleteDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
				t.Fatalf("delete_disk from a moved parker failed: %v", err)
			}
			record, err := journal.Inspect(id)
			if err != nil || record.State != aj.Deleted {
				t.Fatalf("disk record after delete: %+v %v", record.State, err)
			}
			if len(client.state.volumes) != 0 {
				t.Fatalf("delete left volumes behind: %v", client.state.volumes)
			}
		})
	}
}

func equalFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, content := range a {
		if other, ok := b[path]; !ok || other != content {
			return false
		}
	}
	return true
}

type configFailQEMU struct{ lifecycleFlowQEMU }

func (configFailQEMU) Config(context.Context, string, int) (map[string]any, error) {
	return nil, sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to read VM config"}`))
}

// configFailPVE fails every VM configuration read.
type configFailPVE struct{ *lifecycleFlowPVE }

func (c configFailPVE) QEMU() qemu.Service {
	return configFailQEMU{c.lifecycleFlowPVE.QEMU().(lifecycleFlowQEMU)}
}

// Nodes serves the pending endpoint from the same failing config read.
func (c configFailPVE) Nodes() nodes.Service {
	n := c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes)
	n.cfg = c.QEMU().Config
	return n
}

// TestManagedDiskProvenanceHealNamesAFailedConfigRead fails the holder's
// configuration read that the heal makes before a transfer. The refusal
// carries the classified read failure.
func TestManagedDiskProvenanceHealNamesAFailedConfigRead(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	attachMovedDisk(t, deps, client, cid)
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	deps.PVE = configFailPVE{client}
	err = healMovedDiskProvenance(context.Background(), deps, "n2", 777, resolved)
	if message := directorMessage(err); message != "managed disk holder provenance cannot be read before transfer: VM 777 on n2: HTTP 500: unable to read VM config; audit required" {
		t.Fatalf("heal refusal = %q", message)
	}
}
