package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

type lifecycleFlowPVE struct {
	localStorage  bool
	volumeNodes   map[string]string
	foreignUnlink bool
	moveErr       error
	vmNodes       map[int]string
	migrations    int
	deletes       int
	visibilityErr error
	generation    int
	moves         int
	managedDiskTestPVE
	resizeCalls int
	dropResize  bool
	snapshots   []map[string]any
	// descriptionErr fails every configuration update that rewrites a VM
	// description, before it takes effect.
	descriptionErr error
	// visibilityErrAfterDelete becomes visibilityErr once a volume is
	// deleted, so only the audits after a deletion lose their visibility.
	visibilityErrAfterDelete error
	// onVolumeDeleted, when set, runs once a volume is deleted, so a row can
	// change a VM between the deletion and the audit that follows it.
	onVolumeDeleted func()
	// offlineNodes are the cluster members ListStatus reports offline.
	offlineNodes map[string]bool
	// vmSnapshots, when set, answers ListSnapshots per VM instead of
	// snapshots, which every VM shares.
	vmSnapshots map[int][]map[string]any
	// snapshotConfigs serves each snapshot's configuration per VM and name,
	// and snapshotErr fails every snapshot listing and configuration read.
	snapshotConfigs map[int]map[string]map[string]any
	snapshotErr     error
	// pending, when set, is PVE's pending section. A slot delete on a
	// running VM whose hotplug setting lacks disk stays pending, one whose
	// guest holds the device stays pending and fails busy, and a revert drops
	// the pending delete. The pending endpoint reports each held key with its
	// delete flag. Without it every delete applies at once, as on a stopped
	// VM.
	pending *fakePendingModel
	// afterMove, when set, runs after each move_disk lands, so a row can
	// change a VM while an operation is between its steps.
	afterMove func()
	// unlinkedVolumes are volumes a bus-slot delete leaves without an
	// unusedN entry, the way PVE drops a volume the VM doesn't own, while
	// every other delete keeps the fake's usual demotion.
	unlinkedVolumes map[string]bool
	// afterConfigWrite, when set, runs after each config write the fake
	// applies, with the VM it wrote, and onContentRead runs before each
	// single-volume content read, with the volume. Both let a row change
	// a VM while an operation is between its steps.
	afterConfigWrite func(vmid int)
	onContentRead    func(volume string)
	// beforeConfigWrite, when set, runs as each config write arrives, before
	// the fake checks its digest, so a row can change the VM under a writer
	// that has already read it.
	beforeConfigWrite func(vmid int)
	// unlisted hides VMs from the guest listings, the way a listing that
	// hasn't caught up with a guest does, while their configs still answer.
	unlisted map[int]bool
	// moveCalls counts every move POST the fake received.
	moveCalls int
	// moveDigests records the source and target digests each move POST
	// carried, empty when the POST sent none.
	moveDigests [][2]string
	// changeSourceBeforeMove makes the next move POST see its source
	// configuration changed by another writer after the caller read it.
	changeSourceBeforeMove bool
	// moveTaskRefusal makes the next move start a task that refuses on its
	// digest check inside the configuration locks, the check at Qemu.pm:5031.
	moveTaskRefusal bool
	// moveSnapshotRefusal makes every move POST that passes the digest check
	// refuse in the request with PVE's answer for a volume a snapshot still
	// names, the check at Qemu.pm:5060, before any task forks.
	moveSnapshotRefusal bool
	// snapshotAnswers counts the move POSTs refused that way.
	snapshotAnswers int
	// moveTaskExit, when set, makes the next move start a task that exits
	// with this status before its rename, so nothing moves.
	moveTaskExit string
	// moveTaskUnreadable makes the next move start a task that moves nothing
	// and whose status every read fails to get, over a dropped connection.
	moveTaskUnreadable bool
	unreadableTasks    map[string]bool
	// loseMoveResponse makes every move pass PVE's request checks, record the
	// task PVE would fork in lostMoves, and answer with a lost response. It
	// models a lost response that ends the call, because the move's retry
	// loop doesn't retry its plain error, so no second POST goes out.
	loseMoveResponse bool
	lostMoves        []lostMove
	// failedTasks maps a UPID to the exit status its task reports.
	failedTasks map[string]string
	// dropMoveResponses makes that many of the next move POSTs pass PVE's
	// request checks, record the task PVE would fork in lostMoves, and fail
	// with a dropped connection. It models a transport fault that the move's
	// retry loop retries, so the call goes on to a second attempt.
	dropMoveResponses int
	// dropMoveStatus, when set, makes each dropped POST fail with that HTTP
	// status and no text from PVE instead of a dropped connection. It models
	// a status such as 597, which pveproxy relays from its own client when
	// the response breaks off after the backend forked the task, and which
	// the move's retry loop retries as a server error.
	dropMoveStatus int
	// deferMoveTasks makes every move that passes PVE's request checks record
	// its task in lostMoves and answer with a UPID. The task runs only when
	// the CPI awaits it, after beforeMoveTask, so a row can land an earlier
	// task between a POST and its await. deferredOutcomes records what each
	// such task did, in the order the tasks ran.
	deferMoveTasks   bool
	beforeMoveTask   func()
	deferredMoves    map[string]int
	deferredOutcomes []string
	// beforeMoveCheck, when set, runs before PVE's request checks on every
	// move POST, with the POST's number and parameters.
	beforeMoveCheck func(call int, p *nodes.CreateQemuMoveDiskParams)
	// onConfigRead, when set, runs before every config read, and an error it
	// returns fails that read.
	onConfigRead func(vmid int) error
	// activeMoveTasks is each node's active qmmove task list, and
	// activeMoveTaskErr fails every listing. onActiveMoveTasks runs before
	// each listing with its node, and moveTaskListings records the nodes
	// listed, in order.
	activeMoveTasks   map[string][]pve.ActiveTask
	activeMoveTaskErr error
	onActiveMoveTasks func(node string)
	moveTaskListings  []string
	// destroyed records every volume PVE destroyed, in order, whether a
	// storage delete or the delete of an unused entry its VM owns took it.
	destroyed []string
}

// lostMove is a move task PVE forked for a POST whose response was lost.
type lostMove struct {
	source, target       int
	disk, slot           string
	digest, targetDigest *string
}

// moveDigestRefusal is PVE's answer to a move whose digest no longer matches
// the configuration of vmid: assert_if_modified's text behind the move
// handler's "VM <vmid>: " prefix, as an HTTP 500.
func moveDigestRefusal(vmid int) error {
	body, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("VM %d: detected modified configuration - file changed by other user? Try again.\n", vmid)})
	return sdkerrors.ParseAPIError(500, body)
}

// moveSnapshotRefusal is PVE's answer to a move whose volume a snapshot or
// another drive key of the source still names, as an HTTP 500.
func moveSnapshotRefusal() error {
	return sdkerrors.ParseAPIError(500, []byte(`{"message":"Can't move disk used by a snapshot to another VM\n"}`))
}

// moveDigestMismatch reports which side of a move no longer matches the digest
// the POST carried, or 0 when both match or none was sent.
func (c *lifecycleFlowPVE) moveDigestMismatch(sourceID, targetID int, digest, targetDigest *string) int {
	if digest != nil && *digest != c.state.configs[sourceID]["digest"] {
		return sourceID
	}
	if targetDigest != nil && *targetDigest != c.state.configs[targetID]["digest"] {
		return targetID
	}
	return 0
}

// runLostMove plays the task of lost move i the way PVE's worker runs it,
// with the digest check (Qemu.pm:5031) and the disk key check (:5040) made
// against the configurations as they are now. It reports what the task did.
func (c *lifecycleFlowPVE) runLostMove(i int) string {
	lost := c.lostMoves[i]
	if vmid := c.moveDigestMismatch(lost.source, lost.target, lost.digest, lost.targetDigest); vmid != 0 {
		return fmt.Sprintf("refused: VM %d: detected modified configuration - file changed by other user? Try again.", vmid)
	}
	if _, exists := c.state.configs[lost.source][lost.disk]; !exists {
		return fmt.Sprintf("refused: Disk '%s' for VM '%d' does not exist", lost.disk, lost.source)
	}
	if err := c.reassignVolume(lost.source, lost.disk, lost.target, lost.slot); err != nil {
		return "failed: " + err.Error()
	}
	return fmt.Sprintf("moved vm%d.%s to vm%d.%s", lost.source, lost.disk, lost.target, lost.slot)
}

// Tasks reports the exit status of every task failedTasks names, and success
// for every other task.
func (c *lifecycleFlowPVE) Tasks() tasks.Service {
	return lifecycleFlowTasks{c: c}
}

type lifecycleFlowTasks struct {
	diskSizingTasks
	c *lifecycleFlowPVE
}

func (t lifecycleFlowTasks) Wait(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
	if t.c.unreadableTasks[upid] {
		return nil, &sdkerrors.ConnectionError{Host: "n1", Port: 8006, Message: "connection reset by peer"}
	}
	if exit, failed := t.c.failedTasks[upid]; failed {
		return nil, fmt.Errorf("task failed: %s", exit)
	}
	if i, deferred := t.c.deferredMoves[upid]; deferred {
		delete(t.c.deferredMoves, upid)
		if t.c.beforeMoveTask != nil {
			t.c.beforeMoveTask()
		}
		outcome := t.c.runLostMove(i)
		t.c.deferredOutcomes = append(t.c.deferredOutcomes, outcome)
		if !strings.HasPrefix(outcome, "moved ") {
			return nil, fmt.Errorf("task failed: %s", strings.TrimPrefix(outcome, "refused: "))
		}
	}
	return t.diskSizingTasks.Wait(ctx, node, upid, opts)
}

func (c *lifecycleFlowPVE) Nodes() nodes.Service {
	return lifecycleFlowNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, c: c}
}
func (c *lifecycleFlowPVE) QEMU() qemu.Service {
	return lifecycleFlowQEMU{managedDiskTestQEMU: managedDiskTestQEMU{state: c.state}, c: c}
}

type lifecycleFlowNodes struct {
	managedDiskTestNodes
	c *lifecycleFlowPVE
	// cfg, when set, is the config read of a client that wraps the flow
	// fake's QEMU service, which the pending endpoint then serves from.
	cfg func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (n lifecycleFlowNodes) ListStorageContent(ctx context.Context, node, pool string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	listing, err := n.managedDiskTestNodes.ListStorageContent(ctx, node, pool, params)
	if err != nil || !n.c.localStorage {
		return listing, err
	}
	local := nodes.ListStorageContentResponse{}
	for _, raw := range *listing {
		var row struct {
			Volid string `json:"volid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		if n.c.volumeNodes[row.Volid] == node {
			local = append(local, raw)
		}
	}
	return &local, nil
}

// ListQemuSnapshotConfig serves a snapshot's configuration from
// snapshotConfigs.
func (n lifecycleFlowNodes) ListQemuSnapshotConfig(_ context.Context, _, vmidText, name string) (*nodes.ListQemuSnapshotConfigResponse, error) {
	if n.c.snapshotErr != nil {
		return nil, n.c.snapshotErr
	}
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return nil, err
	}
	cfg, ok := n.c.snapshotConfigs[vmid][name]
	if !ok {
		return nil, fmt.Errorf("flow fake: vm %d has no snapshot %q", vmid, name)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	resp := nodes.ListQemuSnapshotConfigResponse(raw)
	return &resp, nil
}

func (n lifecycleFlowNodes) ListCertificatesInfo(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) {
	raw, _ := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("11:", 32), ":")})
	r := nodes.ListCertificatesInfoResponse{raw}
	return &r, nil
}

type lifecycleFlowQEMU struct {
	managedDiskTestQEMU
	c *lifecycleFlowPVE
}

// AttachDisk drops the unused entry that names the attached volume, the way
// write_vm_config does.
func (q lifecycleFlowQEMU) AttachDisk(ctx context.Context, node string, vmid int, volume, bus string, opts *qemu.AttachOpts) (string, error) {
	slot, err := q.managedDiskTestQEMU.AttachDisk(ctx, node, vmid, volume, bus, opts)
	if err == nil {
		dropUnusedNaming(q.c.state.configs[vmid], strings.Split(volume, ",")[0])
	}
	return slot, err
}

// DetachDisk does what the SDK's DetachDisk does against PVE. It deletes the
// slot through the fake's own config write, so on a running VM the pending
// model holds the delete the way qemu-server does, and then it removes every
// unusedN entry of the VM that names the slot's volume.
func (q lifecycleFlowQEMU) DetachDisk(ctx context.Context, node string, vmid int, slot string) error {
	svc := q.c.Nodes()
	value, _ := pve.ConfigString(q.c.state.configs[vmid], slot)
	volume := strings.Split(value, ",")[0]
	del := slot
	if err := svc.UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), &nodes.UpdateQemuConfigParams{Delete: &del}); err != nil {
		return err
	}
	for key, unused := range pve.FindUnusedDiskEntries(q.c.state.configs[vmid]) {
		if volume == "" || unused != volume {
			continue
		}
		swept := key
		if err := svc.UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), &nodes.UpdateQemuConfigParams{Delete: &swept}); err != nil {
			return err
		}
	}
	return nil
}

func (q lifecycleFlowQEMU) ListSnapshots(_ context.Context, _ string, vmid int) ([]map[string]any, error) {
	if q.c.snapshotErr != nil {
		return nil, q.c.snapshotErr
	}
	if q.c.vmSnapshots != nil {
		return q.c.vmSnapshots[vmid], nil
	}
	return q.c.snapshots, nil
}
func (q lifecycleFlowQEMU) ResizeDisk(_ context.Context, _ string, vmid int, slot string, delta int) (string, error) {
	q.c.resizeCalls++
	value := q.c.state.configs[vmid][slot].(string)
	old, err := parseDiskSizeGiB(value)
	if err != nil {
		return "", err
	}
	q.c.state.configs[vmid][slot] = strings.Replace(value, fmt.Sprintf("size=%dG", old), fmt.Sprintf("size=%dG", old+delta), 1)
	volume := strings.Split(value, ",")[0]
	q.c.state.volumes[volume].Size = sdk.PVEInt(old+delta) << 30
	if q.c.dropResize {
		return "", fmt.Errorf("connection lost after external side effect")
	}
	return "UPID:n1:resize", nil
}
func (q lifecycleFlowQEMU) Snapshot(_ context.Context, _ string, _ int, name string, _ map[string]any) (string, error) {
	q.c.snapshots = append(q.c.snapshots, map[string]any{"name": name})
	return "UPID:n1:snapshot", nil
}

func lifecycleFlowFixture(t *testing.T) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	return lifecycleFlowFixtureState(t, true)
}
func lifecycleFlowFixtureState(t *testing.T, returned bool, anchored ...bool) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	return lifecycleFlowFixtureWith(t, returned, len(anchored) > 0 && anchored[0], nil)
}

// lifecycleFlowFixtureWith is lifecycleFlowFixtureState with a hook that runs
// on the disk's creating handle just before it closes, so a test can leave the
// record the way a request that stopped there would.
func lifecycleFlowFixtureWith(t *testing.T, returned, anchored bool, beforeClose func(*aj.Handle)) (Deps, *lifecycleFlowPVE, *aj.Journal, string, string) {
	t.Helper()
	state := &managedDiskTestState{configs: map[int]map[string]any{}, volumes: map[string]*nodes.GetStorageContentResponse{}, pools: map[string]string{}}
	client := &lifecycleFlowPVE{managedDiskTestPVE: managedDiskTestPVE{state: state}}
	identity, err := pve.ObserveStorageClusterIdentity(context.Background(), client.Nodes(), []string{"n1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(context.Background(), dir, lifecycleFlowNamespace, aj.Enrollment{ClusterID: identity.ID(), AuthorityID: "authority", AuditID: "audit", PreviousWriterFenced: true, CompleteHistoricalAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	disk := journalLifecycleFlowDisk(t, journal, state, returned, anchored, beforeClose)
	state.configs[777] = map[string]any{"name": "workload", "digest": "1", "scsi1": disk.volume + ",serial=" + disk.token + ",size=5G"}
	// All original storage sets and role bindings have been removed.
	deps := Deps{PVE: client, Config: &config.CPIConfig{Node: "n1", DiskStorage: disk.storage, StoragePlacementNamespace: lifecycleFlowNamespace, StorageAllocationJournalDir: dir}}
	return deps, client, journal, disk.id, disk.cid
}

// lifecycleFlowNamespace is the placement namespace planFixture plans in, and
// so the one every flow fixture journal is initialized with.
const lifecycleFlowNamespace = "director"

// lifecycleFlowDisk is one journal-managed persistent disk in a flow fixture.
type lifecycleFlowDisk struct {
	id, cid, volume, token, storage string
}

// journalLifecycleFlowDisk plans a 5 GiB persistent disk the way create_disk
// does, journals its observed creation, and places its volume on storage. The
// volume starts without a holder, and the caller decides what holds it.
func journalLifecycleFlowDisk(t *testing.T, journal *aj.Journal, state *managedDiskTestState, returned, anchored bool, beforeClose ...func(*aj.Handle)) lifecycleFlowDisk {
	t.Helper()
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	request, _, _ := planFixture(t, func(_ *planFixtureSource, cfg *config.CPIConfig) {
		cfg.EphemeralStorageSet = ""
		cfg.PersistentStorageSet = "E"
	})
	selection, err := ResolveStoragePlacementSelectors(request.Selection.Policy, "create_disk", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	request.Selection = selection
	request.RootBytes = 0
	request.Sources = nil
	request.PersistentBytes = 5 << 30
	request.AllocationKey = id
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := iterator.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Namespace != lifecycleFlowNamespace {
		t.Fatalf("disk planned in namespace %q, want %q", plan.Namespace, lifecycleFlowNamespace)
	}
	intent, err := storageJournalIntent("create_disk", []json.RawMessage{planJSON(t, 5120)}, selection, request.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.CreateDisk(context.Background(), id, intent)
	if err != nil {
		t.Fatal(err)
	}
	name, err := pve.AllocationVolumeName(123, plan.Namespace, id, "raw")
	if err != nil {
		t.Fatal(err)
	}
	target := plan.Targets[0]
	volume := target.StorageID + ":123/" + name
	token := handle.Record().DiskToken
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: token, Format: "raw", Anchor: anchored})
	if err != nil {
		t.Fatal(err)
	}
	step, err := storageMutationIntent(handle, "create", aj.Target{Node: "n1", Storage: target.StorageID, Backing: target.BackingKey, VMID: 123, IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(handle, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	if returned {
		record.State = aj.ReadyToReturn
		record.CID = cid
	}
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	for _, hook := range beforeClose {
		if hook != nil {
			hook(handle)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	state.volumes[volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	return lifecycleFlowDisk{id: id, cid: cid, volume: volume, token: token, storage: target.StorageID}
}
func TestManagedDiskResizeAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	args := []json.RawMessage{planJSON(t, cid), planJSON(t, 6144)}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.resizeCalls != 1 || record.State != aj.ReadyToReturn || len(record.Steps) != 2 {
		t.Fatalf("incomplete lifecycle: calls=%d record=%+v", client.resizeCalls, record)
	}
	step := record.Steps[1]
	if step.UPID != "UPID:n1:resize" || len(step.Charges) != 1 || step.Charges[0].AcquiredBytes != 1<<30 {
		t.Fatalf("resize evidence lost: %+v", step)
	}
}
func TestManagedDiskDroppedResizeCannotReplay(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	client.dropResize = true
	args := []json.RawMessage{planJSON(t, cid), planJSON(t, 6144)}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("lost response accepted")
	}
	if _, err := HandleResizeDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("unknown old resize replay accepted")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.resizeCalls != 1 || record.State != aj.ReconciliationRequired || record.Steps[1].State != aj.Planned {
		t.Fatalf("unknown evidence replayed: calls=%d record=%+v", client.resizeCalls, record)
	}
}
func TestManagedDiskOwnershipRequiresActualVolume(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	for volume := range client.state.volumes {
		delete(client.state.volumes, volume)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "has_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDiskForOp(context.Background(), deps, "has_disk", cid, bare, meta); err == nil {
		t.Fatal("dangling VM entry claimed live disk ownership")
	}
	if client.resizeCalls != 0 {
		t.Fatal("read-only lookup submitted mutation")
	}
}

func (n lifecycleFlowNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return err
	}
	if n.c.beforeConfigWrite != nil {
		n.c.beforeConfigWrite(vmid)
	}
	cfg := n.c.state.configs[vmid]
	if p.Digest != nil && *p.Digest != cfg["digest"] {
		return fmt.Errorf("config generation conflict")
	}
	if p.Description != nil && n.c.descriptionErr != nil {
		return n.c.descriptionErr
	}
	if n.c.pending != nil {
		if handled, pendingErr := n.c.pending.update(vmid, cfg, p); handled {
			// PVE writes the pending section, so the digest moves on.
			n.c.generation++
			cfg["digest"] = fmt.Sprint(n.c.generation + 100)
			return pendingErr
		}
	}
	deletedVolume := ""
	if p.Delete != nil && strings.HasPrefix(*p.Delete, "unused") {
		value, _ := pve.ConfigString(cfg, *p.Delete)
		deletedVolume = strings.Split(value, ",")[0]
	}
	detachedVolume := ""
	if p.Delete != nil && !strings.HasPrefix(*p.Delete, "unused") {
		value, _ := pve.ConfigString(cfg, *p.Delete)
		detachedVolume = strings.Split(value, ",")[0]
	}
	fake := newIDFakeClient(n.c.state.configs)
	if err := (&idFakeNodes{c: fake}).UpdateQemuConfig(ctx, node, vmidText, p); err != nil {
		return err
	}
	if p.Tags != nil {
		// A tag write lands the way PVE applies it, so a row can see the
		// bosh-deleting tag delete_vm stamps.
		cfg["tags"] = *p.Tags
	}
	fields, err := lifecycleMutationFields(p)
	if err != nil {
		return err
	}
	for key, value := range fields {
		if isDiskOptionKey(key) {
			cfg[key] = value
			if !strings.HasPrefix(key, "unused") {
				dropUnusedNaming(cfg, strings.Split(fmt.Sprint(value), ",")[0])
			}
		}
	}
	// Deleting an unused entry destroys the volume when the VM owns it by
	// name, as qemu-server's update_vm_api does through try_deallocate_drive
	// and PVE::Storage::vdisk_free (src/PVE/API2/Qemu.pm line 2321 and
	// src/PVE/QemuServer.pm line 4908 at a7b4240b). The volume is recorded
	// in destroyed, but deletes counts only storage deletes.
	if fakeVolumeOwnedBy(deletedVolume, vmid) {
		n.c.destroyed = append(n.c.destroyed, deletedVolume)
		delete(n.c.state.volumes, deletedVolume)
		n.c.volumeDeleted()
	}
	if detachedVolume != "" && n.c.unlinkedVolumes[detachedVolume] {
		dropUnusedNaming(cfg, detachedVolume)
	}
	if n.c.foreignUnlink && p.Delete != nil && !strings.HasPrefix(*p.Delete, "unused") {
		for slot := range pve.FindUnusedDiskEntries(cfg) {
			delete(cfg, slot)
		}
	}
	n.c.generation++
	cfg["digest"] = fmt.Sprint(n.c.generation + 100)
	if n.c.afterConfigWrite != nil {
		n.c.afterConfigWrite(vmid)
	}
	return nil
}
func (n lifecycleFlowNodes) CreateQemuMoveDisk(_ context.Context, _ string, sourceText string, p *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	if n.c.moveErr != nil {
		return nil, n.c.moveErr
	}
	n.c.moveCalls++
	digests := [2]string{}
	if p.Digest != nil {
		digests[0] = *p.Digest
	}
	if p.TargetDigest != nil {
		digests[1] = *p.TargetDigest
	}
	n.c.moveDigests = append(n.c.moveDigests, digests)
	sourceID, _ := strconv.Atoi(sourceText)
	targetID := int(*p.TargetVmid)
	if n.c.beforeMoveCheck != nil {
		n.c.beforeMoveCheck(n.c.moveCalls, p)
	}
	if n.c.changeSourceBeforeMove {
		n.c.changeSourceBeforeMove = false
		n.c.generation++
		n.c.state.configs[sourceID]["digest"] = fmt.Sprint(n.c.generation + 100)
	}
	// PVE's request checks, before any fork (Qemu.pm:5191).
	if vmid := n.c.moveDigestMismatch(sourceID, targetID, p.Digest, p.TargetDigest); vmid != 0 {
		return nil, moveDigestRefusal(vmid)
	}
	if n.c.moveSnapshotRefusal {
		n.c.snapshotAnswers++
		return nil, moveSnapshotRefusal()
	}
	forked := lostMove{source: sourceID, target: targetID, disk: p.Disk, slot: *p.TargetDisk, digest: p.Digest, targetDigest: p.TargetDigest}
	if n.c.loseMoveResponse {
		n.c.lostMoves = append(n.c.lostMoves, forked)
		return nil, fmt.Errorf("move_disk response lost")
	}
	if n.c.dropMoveResponses > 0 {
		n.c.dropMoveResponses--
		n.c.lostMoves = append(n.c.lostMoves, forked)
		if n.c.dropMoveStatus != 0 {
			return nil, sdkerrors.ParseAPIError(n.c.dropMoveStatus, []byte(`{"data":null}`))
		}
		return nil, &sdkerrors.ConnectionError{Host: "n1", Port: 8006, Message: "connection reset by peer"}
	}
	if n.c.deferMoveTasks {
		n.c.lostMoves = append(n.c.lostMoves, forked)
		upid := fmt.Sprintf("UPID:n1:%08X:03504636:6AA1786A:qmmove:%d-%s>%d-%s:root@pam:", len(n.c.lostMoves), sourceID, p.Disk, targetID, *p.TargetDisk)
		if n.c.deferredMoves == nil {
			n.c.deferredMoves = map[string]int{}
		}
		n.c.deferredMoves[upid] = len(n.c.lostMoves) - 1
		raw := json.RawMessage(strconv.Quote(upid))
		return &raw, nil
	}
	if n.c.moveTaskRefusal {
		n.c.moveTaskRefusal = false
		upid := fmt.Sprintf("UPID:n1:000573BD:03504636:6AA1786A:qmmove:%d-%s>%d-%s:root@pam:", sourceID, p.Disk, targetID, *p.TargetDisk)
		if n.c.failedTasks == nil {
			n.c.failedTasks = map[string]string{}
		}
		n.c.failedTasks[upid] = fmt.Sprintf("VM %d: detected modified configuration - file changed by other user? Try again.", sourceID)
		raw := json.RawMessage(strconv.Quote(upid))
		return &raw, nil
	}
	if n.c.moveTaskUnreadable {
		n.c.moveTaskUnreadable = false
		upid := fmt.Sprintf("UPID:n1:000573BF:03504636:6AA1786A:qmmove:%d-%s>%d-%s:root@pam:", sourceID, p.Disk, targetID, *p.TargetDisk)
		if n.c.unreadableTasks == nil {
			n.c.unreadableTasks = map[string]bool{}
		}
		n.c.unreadableTasks[upid] = true
		raw := json.RawMessage(strconv.Quote(upid))
		return &raw, nil
	}
	if n.c.moveTaskExit != "" {
		upid := fmt.Sprintf("UPID:n1:000573BE:03504636:6AA1786A:qmmove:%d-%s>%d-%s:root@pam:", sourceID, p.Disk, targetID, *p.TargetDisk)
		if n.c.failedTasks == nil {
			n.c.failedTasks = map[string]string{}
		}
		n.c.failedTasks[upid] = n.c.moveTaskExit
		n.c.moveTaskExit = ""
		raw := json.RawMessage(strconv.Quote(upid))
		return &raw, nil
	}
	if err := n.c.reassignVolume(sourceID, p.Disk, targetID, *p.TargetDisk); err != nil {
		return nil, err
	}
	if n.c.afterMove != nil {
		n.c.afterMove()
	}
	raw := json.RawMessage(`"UPID:n1:move"`)
	return &raw, nil
}

// reassignVolume moves the volume on disk of sourceID to slot of targetID and
// renames it for its new owner, the way a completed reassign does.
func (c *lifecycleFlowPVE) reassignVolume(sourceID int, disk string, targetID int, slot string) error {
	source := c.state.configs[sourceID]
	target := c.state.configs[targetID]
	value, _ := pve.ConfigString(source, disk)
	old := strings.Split(value, ",")[0]
	storage, _, err := pve.ParseDiskCID(old)
	if err != nil {
		return err
	}
	info := c.state.volumes[old]
	if info == nil {
		return fmt.Errorf("source volume missing")
	}
	c.moves++
	landed := fmt.Sprintf("%s:%d/vm-%d-disk-%d.raw", storage, targetID, targetID, c.moves)
	opts := strings.TrimPrefix(value, old)
	if strings.HasPrefix(disk, "unused") {
		opts = ""
	}
	delete(source, disk)
	target[slot] = landed + opts
	delete(c.state.volumes, old)
	c.state.volumes[landed] = info
	if c.volumeNodes != nil {
		c.volumeNodes[landed] = c.volumeNodes[old]
		delete(c.volumeNodes, old)
	}
	c.generation++
	source["digest"] = fmt.Sprint(c.generation + 100)
	c.generation++
	target["digest"] = fmt.Sprint(c.generation + 100)
	return nil
}
func (q lifecycleFlowQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	upid, err := q.managedDiskTestQEMU.Create(ctx, node, params)
	if err == nil {
		id := params["vmid"].(int)
		q.c.state.configs[id]["digest"] = "1"
		if q.c.vmNodes == nil {
			q.c.vmNodes = map[int]string{}
		}
		q.c.vmNodes[id] = node
	}
	return upid, err
}
func TestManagedDiskSnapshotAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	if _, err := HandleSnapshotDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.snapshots) != 1 || record.State != aj.ReadyToReturn || record.Steps[1].UPID != "UPID:n1:snapshot" {
		t.Fatalf("snapshot lifecycle incomplete: %+v", record)
	}
}

// attachManagedDiskToFreshSlot removes the fixture's own attachment and attaches
// the disk to VM 777 through attach_disk, which writes the receiving
// provenance.
func attachManagedDiskToFreshSlot(t *testing.T, deps Deps, client *lifecycleFlowPVE, cid string) {
	t.Helper()
	delete(client.state.configs[777], "scsi1")
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(client.state.configs[777]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("receiving provenance absent: %v %v", entries, err)
	}
}

// TestManagedDiskAttachAndDetachAfterSetRemoval proves that allocation identity
// survives a rename. A managed disk keeps its CID name through its first park,
// so the row runs the park, attach, park cycle, where the detach renames the
// volume for the parker, and then resolves the disk by its CID.
func TestManagedDiskAttachAndDetachAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	attachManagedDiskToFreshSlot(t, deps, client, cid)
	parkRenameCycle(t, context.Background(), deps, client, cid)
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || client.moves != 2 {
		t.Fatalf("detach lifecycle incomplete: moves=%d record=%+v", client.moves, record)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.volid == bare || resolved.allocation == nil || resolved.allocation.record.ID != id {
		t.Fatal("rename lost full allocation identity")
	}
}

// TestManagedDiskConfigEditParkKeepsItsVolumeAndIdentity is the other half. The
// first detach of a managed disk moves nothing, because 777 doesn't own the
// disk band's name and PVE keeps no unused entry for it. The volume lands on
// one parker slot under its CID name, and the allocation ID still resolves.
func TestManagedDiskConfigEditParkKeepsItsVolumeAndIdentity(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	attachManagedDiskToFreshSlot(t, deps, client, cid)
	deps.Config.DetachedDiskStrategy = "parked"
	if err := detachDiskAt(t, context.Background(), deps, "777", cid); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReadyToReturn || client.moves != 0 {
		t.Fatalf("detach lifecycle incomplete: moves=%d record=%+v", client.moves, record)
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
	if resolved.volid != bare || resolved.allocation == nil || resolved.allocation.record.ID != id {
		t.Fatalf("config-edit park lost identity: volid=%s want %s allocation=%+v", resolved.volid, bare, resolved.allocation)
	}
}

func (c *lifecycleFlowPVE) StorageAuditVisibility(context.Context) error { return c.visibilityErr }

// ActiveMoveTasks serves activeMoveTasks for node, the way PVE's active task
// list answers a qmmove typefilter.
func (c *lifecycleFlowPVE) ActiveMoveTasks(_ context.Context, node string) ([]pve.ActiveTask, error) {
	c.moveTaskListings = append(c.moveTaskListings, node)
	if c.onActiveMoveTasks != nil {
		c.onActiveMoveTasks(node)
	}
	if c.activeMoveTaskErr != nil {
		return nil, c.activeMoveTaskErr
	}
	return append([]pve.ActiveTask{}, c.activeMoveTasks[node]...), nil
}

// volumeDeleted applies visibilityErrAfterDelete and runs onVolumeDeleted
// once any volume is deleted.
func (c *lifecycleFlowPVE) volumeDeleted() {
	if c.visibilityErrAfterDelete != nil {
		c.visibilityErr = c.visibilityErrAfterDelete
	}
	if c.onVolumeDeleted != nil {
		c.onVolumeDeleted()
	}
}
func (c *lifecycleFlowPVE) Storage() storage.Service {
	return lifecycleFlowStorage{managedDiskTestStorage: managedDiskTestStorage{state: c.state}, c: c}
}

type lifecycleFlowStorage struct {
	managedDiskTestStorage
	c *lifecycleFlowPVE
}

func (s lifecycleFlowStorage) DeleteVolumeAsync(_ context.Context, node, pool, volume string) (string, error) {
	s.c.deletes++
	s.c.volumeDeleted()
	s.c.destroyed = append(s.c.destroyed, volume)
	delete(s.c.state.volumes, volume)
	return fmt.Sprintf("UPID:%s:000573BD:03504636:6AA1786A:imgdel:123@%s:pmx@pve!pmx:", node, pool), nil
}
func TestManagedDiskDeleteRequiresCompleteAuditAndPreservesTombstone(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	client.visibilityErr = errors.New("restricted visibility")
	args := []json.RawMessage{planJSON(t, cid)}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("partial audit certified deletion")
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired || client.deletes != 1 {
		t.Fatalf("partial deletion state: %+v deletes=%d", record, client.deletes)
	}
	client.visibilityErr = nil
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil {
		t.Fatal("ordinary retry accepted unresolved deletion")
	}
	deps.PVE = &cleanupTaskClient{Client: deps.PVE}
	if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(id)); err != nil {
		t.Fatalf("explicit cleanup of successfully deleted disk: %v", err)
	}
	record, err = journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	wantUPID := fmt.Sprintf("UPID:n1:000573BD:03504636:6AA1786A:imgdel:123@%s:pmx@pve!pmx:", record.Steps[0].Target.Storage)
	if record.State != aj.Cleaned || client.deletes != 1 || record.Steps[1].UPID != wantUPID || record.Steps[1].State != aj.Submitted {
		t.Fatalf("verified deletion lost evidence: %+v deletes=%d", record, client.deletes)
	}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("idempotent tombstone delete: %v", err)
	}
	bare, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	client.state.volumes[bare] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	if _, err := HandleDeleteDisk(deps).Handle(context.Background(), args, jsonrpc.Context{}); err == nil || client.deletes != 1 {
		t.Fatal("terminal journal authorized deletion of new live resource")
	}
}
func TestManagedDiskBareCIDRecoversJournalToken(t *testing.T) {
	deps, _, journal, id, cid := lifecycleFlowFixture(t)
	bare, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "has_disk", bare, bare, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if rd.stableID != record.DiskToken || rd.holder == nil || rd.holder.VMID != 777 {
		t.Fatalf("bare CID lost stable ownership: %+v", rd)
	}
}

func (n lifecycleFlowNodes) GetStorageContent(ctx context.Context, node, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	if n.c.onContentRead != nil {
		n.c.onContentRead(pool + ":" + volume)
	}
	if _, exists := n.c.state.volumes[pool+":"+volume]; !exists || n.c.localStorage && n.c.volumeNodes[pool+":"+volume] != node {
		return nil, sdkerrors.ParseAPIError(404, []byte(`{"message":"volume not found"}`))
	}
	return n.managedDiskTestNodes.GetStorageContent(ctx, node, pool, volume)
}

func (c *lifecycleFlowPVE) vmNode(vmid int) string {
	if node := c.vmNodes[vmid]; node != "" {
		return node
	}
	return "n1"
}
func (q lifecycleFlowQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.c.onConfigRead != nil {
		if err := q.c.onConfigRead(vmid); err != nil {
			return nil, err
		}
	}
	if _, exists := q.c.state.configs[vmid]; !exists || q.c.vmNode(vmid) != node {
		return nil, sdkerrors.ParseAPIError(404, []byte(`{"message":"VM not found"}`))
	}
	return q.managedDiskTestQEMU.Config(ctx, node, vmid)
}
func (n lifecycleFlowNodes) ListQemu(_ context.Context, node string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	rows := nodes.ListQemuResponse{}
	for id, cfg := range n.c.state.configs {
		if n.c.vmNode(id) != node || n.c.unlisted[id] {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"vmid": id, "tags": cfg["tags"], "name": cfg["name"]})
		rows = append(rows, raw)
	}
	return &rows, nil
}
func (n lifecycleFlowNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	read := n.cfg
	if read == nil {
		read = n.c.QEMU().Config
	}
	if n.c.pending != nil {
		return n.c.pending.pendingRead(ctx, read, node, vmid)
	}
	return PendingFromConfigRead(ctx, read, node, vmid)
}
func (n lifecycleFlowNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	rows := nodes.ListNodesResponse{json.RawMessage(`{"node":"n1","status":"online"}`), json.RawMessage(`{"node":"n2","status":"online"}`)}
	return &rows, nil
}
func (c *lifecycleFlowPVE) Cluster() cluster.Service {
	return lifecycleFlowCluster{managedDiskTestCluster: managedDiskTestCluster{state: c.state}, c: c}
}

type lifecycleFlowCluster struct {
	managedDiskTestCluster
	c *lifecycleFlowPVE
}

func (c lifecycleFlowCluster) ListConfigNodes(context.Context) (*cluster.ListConfigNodesResponse, error) {
	r := cluster.ListConfigNodesResponse{json.RawMessage(`{"name":"n1"}`), json.RawMessage(`{"name":"n2"}`)}
	return &r, nil
}
func (c lifecycleFlowCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := make(cluster.ListStatusResponse, 0, 3)
	r = append(r, json.RawMessage(`{"type":"cluster","quorate":1}`))
	for _, node := range []string{"n1", "n2"} {
		online := 1
		if c.c.offlineNodes[node] {
			online = 0
		}
		r = append(r, json.RawMessage(fmt.Sprintf(`{"type":"node","name":%q,"online":%d}`, node, online)))
	}
	return &r, nil
}
func (c lifecycleFlowCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	r := make(cluster.ListResourcesResponse, 0, len(c.c.state.configs))
	for id, cfg := range c.c.state.configs {
		if c.c.unlisted[id] {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": id, "node": c.c.vmNode(id), "tags": cfg["tags"]})
		r = append(r, raw)
	}
	return &r, nil
}
func (n lifecycleFlowNodes) CreateQemuMigrate(_ context.Context, node, vmidText string, p *nodes.CreateQemuMigrateParams) (*nodes.CreateQemuMigrateResponse, error) {
	id, _ := strconv.Atoi(vmidText)
	if n.c.vmNode(id) != node {
		return nil, fmt.Errorf("migration source node changed")
	}
	if n.c.vmNodes == nil {
		n.c.vmNodes = map[int]string{}
	}
	n.c.vmNodes[id] = p.Target
	if n.c.localStorage {
		for _, value := range qemu.ParseDisks(n.c.state.configs[id]) {
			n.c.volumeNodes[strings.Split(value, ",")[0]] = p.Target
		}
	}
	n.c.migrations++
	raw := json.RawMessage(`"UPID:n1:migrate"`)
	return &raw, nil
}
func (n lifecycleFlowNodes) DeleteQemu(_ context.Context, node, vmidText string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	id, _ := strconv.Atoi(vmidText)
	if n.c.vmNode(id) != node {
		return nil, fmt.Errorf("delete node changed")
	}
	if lifecycleConfigHasAnyVolume(n.c.state.configs[id]) {
		return nil, fmt.Errorf("mover not empty")
	}
	delete(n.c.state.configs, id)
	delete(n.c.vmNodes, id)
	raw := json.RawMessage(`"UPID:n2:destroy"`)
	return &raw, nil
}

// TestManagedDiskSharedMigrationAfterSetRemoval attaches a parker-named managed
// disk on shared storage to a VM on another node, which needs the mover flow.
// The park, attach, park cycle gives the disk the parker's name first, because
// a vm-123 volume on shared storage reaches the other node by config edit and
// never needs a migration (see the pin row below).
func TestManagedDiskSharedMigrationAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DiskMigration = "on_attach"
	parkRenameCycle(t, context.Background(), deps, client, cid)
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.migrations != 1 || record.State != aj.ReadyToReturn {
		t.Fatalf("migration incomplete: count=%d record=%+v", client.migrations, record)
	}
	for _, step := range record.Steps {
		if strings.Contains(step.Kind, "CreateQemuMigrate") && len(step.Charges) != 0 {
			t.Fatal("shared metadata migration charged destination bytes")
		}
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.holder == nil || rd.holder.Node != "n2" || rd.holder.VMID != 888 || rd.allocation.record.ID != id {
		t.Fatalf("migration lost identity: %+v", rd)
	}
}

// TestManagedDiskOnSharedStorageReachesAnotherNodeByConfigEdit pins the fact the
// migration row relies on. A vm-123 disk on shared storage that one detach
// parked, with its name unchanged, attaches to a VM on n2 by config edit. No
// migration runs, the disk ends on 888 under its own name, and the parker no
// longer names it.
func TestManagedDiskOnSharedStorageReachesAnotherNodeByConfigEdit(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskMigration = "on_attach"
	if err := detachDiskAt(t, context.Background(), deps, "777", cid); err != nil {
		t.Fatal(err)
	}
	bare, meta, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	requireConfigEditPark(t, client, record.DiskToken, bare)
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if err := attachDiskAt(t, context.Background(), deps, "888", cid); err != nil {
		t.Fatal(err)
	}
	if client.migrations != 0 || client.moves != 0 {
		t.Fatalf("a vm-123 disk on shared storage migrated or moved: migrations=%d moves=%d", client.migrations, client.moves)
	}
	holders := map[string]string{}
	for vmid, cfg := range client.state.configs {
		for key, value := range cfg {
			text, _ := value.(string)
			if isDiskOptionKey(key) && strings.Split(text, ",")[0] == bare {
				holders[fmt.Sprintf("%d.%s", vmid, key)] = text
			}
		}
	}
	if len(holders) != 1 {
		t.Fatalf("%s is named by %v, want exactly one slot on VM 888", bare, holders)
	}
	for slot := range holders {
		if !strings.HasPrefix(slot, "888.") {
			t.Fatalf("%s is named by %s, want VM 888", bare, slot)
		}
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "test", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.holder == nil || rd.holder.Node != "n2" || rd.holder.VMID != 888 || rd.allocation.record.ID != id {
		t.Fatalf("config-edit attach lost identity: %+v", rd)
	}
}

// Status reports a VM the pending model runs as running, and every other VM
// as stopped.
func (q lifecycleFlowQEMU) Status(_ context.Context, _ string, vmid int) (map[string]any, error) {
	if q.c.pending != nil && q.c.pending.isRunning(vmid) {
		return map[string]any{"status": "running"}, nil
	}
	return map[string]any{"status": "stopped"}, nil
}

// Stop stops a VM the pending model runs and completes the stop at once, the
// way vm_stop_cleanup applies the VM's pending changes when a stop finishes, so
// the stop's task is already done when the caller awaits it.
func (q lifecycleFlowQEMU) Stop(_ context.Context, _ string, vmid int) (string, error) {
	if q.c.pending != nil {
		q.c.pending.issueStop(vmid)
		q.c.pending.completeStops(q.c.state.configs)
	}
	return "UPID:n1:stop", nil
}
func (c lifecycleFlowCluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	return nil, &sdkerrors.APIError{Code: 404, Message: "not found"}
}

func (c lifecycleFlowCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	r := cluster.ListHaRulesResponse{}
	return &r, nil
}

func (c *lifecycleFlowPVE) ClusterStorage() clusterstorage.Service {
	return lifecycleFlowDefinitions{c: c}
}

type lifecycleFlowDefinitions struct {
	managedDiskTestDefinitions
	c *lifecycleFlowPVE
}

func (d lifecycleFlowDefinitions) ListStorage(ctx context.Context, params *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	if !d.c.localStorage {
		return d.managedDiskTestDefinitions.ListStorage(ctx, params)
	}
	r := clusterstorage.ListStorageResponse{}
	for _, id := range []string{"a", "b"} {
		raw, err := json.Marshal(map[string]any{"storage": id, "type": "dir", "path": "/mnt/" + id, "shared": 0, "content": "images", "nodes": "n1,n2"})
		if err != nil {
			return nil, err
		}
		r = append(r, raw)
	}
	return &r, nil
}
func (s lifecycleFlowStorage) Exists(ctx context.Context, node, pool, volume string) (bool, error) {
	if s.c.localStorage {
		return s.c.state.volumes[volume] != nil && s.c.volumeNodes[volume] == node, nil
	}
	return s.managedDiskTestStorage.Exists(ctx, node, pool, volume)
}

func TestManagedDiskLocalMigrationChargesDestinationAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	birth, _, err := decodeDiskCID(context.Background(), deps, "test", cid)
	if err != nil {
		t.Fatal(err)
	}
	pool, _, err := pve.ParseDiskCID(birth)
	if err != nil {
		t.Fatal(err)
	}
	// Model a separately audited external relocation onto a local backing.
	// Its evidence is appended; the original NFS policy and birth stay immutable.
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
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskMigration = "on_attach"
	if _, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}
	if _, err := HandleAttachDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if strings.Contains(step.Kind, "CreateQemuMigrate") {
			found = true
			if step.Target.Node != "n2" || len(step.Charges) != 1 || step.Charges[0].AcquiredBytes != 5<<30 || step.Charges[0].OutstandingBytes != 0 {
				t.Fatalf("local copy lost destination accounting: %+v", step)
			}
		}
	}
	if !found || client.migrations != 1 || record.State != aj.ReadyToReturn {
		t.Fatalf("local migration incomplete: %+v", record)
	}
}

func (c lifecycleFlowCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	return &rows, nil
}
