package handlers

// A detach of a journal-managed disk ends by removing the source VM's
// allocation provenance once the disk has left it. A transfer that lands
// through a resume, after a snapshot deferral or a failed first transfer, used
// to skip that tail, so the source VM kept an entry that made a later
// delete_disk refuse for good. These rows run through the real handlers on the
// flow fake, with 777 carrying the entry an attach writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// tailWrites watches the description writes that drop the disk's allocation
// entry from 777, and can make the first of them meet a changed config.
type tailWrites struct {
	token string
	// removals counts the writes PVE applied that dropped the entry.
	removals int
	// conflictOnce makes the next such write find 777 changed by another
	// writer after its digest was read, so PVE refuses it unchanged.
	conflictOnce bool
	// conflictAlways makes every such write meet a changed config.
	conflictAlways bool
	conflicts      int
	// failWith, when set, is the answer every such write gets in place of
	// PVE applying it.
	failWith error
	// afterSourceRead, when set, runs after each read of 777's pending view
	// has been served.
	afterSourceRead func()
	// configRead, when set, runs before each read of 777's config is served,
	// and an error it returns is the read's answer in place of the config.
	configRead func() error
	// identityRead, when set, runs before each read of the cluster's identity
	// is served, and an error it returns is the read's answer in place of the
	// identity.
	identityRead func() error
}

// tailWritePVE is the stranded-disk client with 777's description writes
// watched by tailWrites.
type tailWritePVE struct {
	legacyDestroyPVE
	w *tailWrites
}

func (c tailWritePVE) Nodes() nodes.Service {
	return tailWriteNodes{Service: c.legacyDestroyPVE.Nodes(), c: c.lifecycleFlowPVE, w: c.w}
}

type tailWriteNodes struct {
	nodes.Service
	c *lifecycleFlowPVE
	w *tailWrites
}

// tailHookKey is the context key for a hook that the tail fakes run on each
// read of 777 and each description write to it.
type tailHookKey struct{}

// tailEvent is what a tail hook sees, a read of 777 that has been served, a
// description write to 777 that has arrived and that PVE hasn't looked at, or
// a description write to 777 that has landed.
type tailEvent string

const (
	tailRead    tailEvent = "read"
	tailWrite   tailEvent = "write"
	tailWritten tailEvent = "written"
)

// withTailHook returns ctx carrying hook. The fakes run it after each read of
// 777 is served, as each description write to 777 arrives, and after each one
// lands, so the hook can change 777 the way another writer does between a
// read and a write, or between a write and the read after it. PVE refuses a
// write whose digest the change made stale, with its own text.
func withTailHook(ctx context.Context, hook func(tailEvent)) context.Context {
	return context.WithValue(ctx, tailHookKey{}, hook)
}

// runTailHook runs ctx's tail hook, if it carries one, for an event on vmid.
func runTailHook(ctx context.Context, vmid int, event tailEvent) {
	if hook, ok := ctx.Value(tailHookKey{}).(func(tailEvent)); ok && vmid == 777 {
		hook(event)
	}
}

func (c tailWritePVE) QEMU() qemu.Service {
	return tailWriteQEMU{Service: c.legacyDestroyPVE.QEMU(), w: c.w}
}

type tailWriteQEMU struct {
	qemu.Service
	w *tailWrites
}

func (q tailWriteQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if vmid == 777 && q.w.configRead != nil {
		if err := q.w.configRead(); err != nil {
			return nil, err
		}
	}
	cfg, err := q.Service.Config(ctx, node, vmid)
	runTailHook(ctx, vmid, tailRead)
	return cfg, err
}

func (n tailWriteNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil || vmid != 777 || p.Description == nil {
		return n.Service.UpdateQemuConfig(ctx, node, vmidText, p)
	}
	if _, ok := ctx.Value(tailHookKey{}).(func(tailEvent)); ok {
		runTailHook(ctx, vmid, tailWrite)
		if current, _ := pve.ConfigString(n.c.state.configs[777], "digest"); p.Digest != nil && *p.Digest != current {
			n.w.conflicts++
			body, _ := json.Marshal(map[string]string{"message": "checksum mismatch (file change by other user?)\n"})
			return sdkerrors.ParseAPIError(500, body)
		}
		if err := n.updateDescription(ctx, node, vmidText, p); err != nil {
			return err
		}
		runTailHook(ctx, vmid, tailWritten)
		return nil
	}
	return n.updateDescription(ctx, node, vmidText, p)
}

// updateDescription is UpdateQemuConfig for a description write to 777.
func (n tailWriteNodes) updateDescription(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	before := allocationEntryNamed(pve.DescriptionFromConfig(n.c.state.configs[777]), n.w.token)
	after := allocationEntryNamed(*p.Description, n.w.token)
	if !before || after {
		return n.Service.UpdateQemuConfig(ctx, node, vmidText, p)
	}
	if n.w.failWith != nil {
		return n.w.failWith
	}
	if n.w.conflictOnce || n.w.conflictAlways {
		n.w.conflictOnce = false
		n.w.conflicts++
		// Another writer changes 777 between the read and this write, and
		// PVE's update_vm_api refuses the write before it changes anything.
		n.c.generation++
		n.c.state.configs[777]["digest"] = fmt.Sprint(n.c.generation + 100)
		body, _ := json.Marshal(map[string]string{"message": "checksum mismatch (file change by other user?)\n"})
		return sdkerrors.ParseAPIError(500, body)
	}
	if err := n.Service.UpdateQemuConfig(ctx, node, vmidText, p); err != nil {
		return err
	}
	n.w.removals++
	return nil
}

func (n tailWriteNodes) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	if n.w.identityRead != nil {
		if err := n.w.identityRead(); err != nil {
			return nil, err
		}
	}
	return n.Service.ListCertificatesInfo(ctx, node)
}

func (n tailWriteNodes) ListQemuPending(ctx context.Context, node, vmidText string) (*nodes.ListQemuPendingResponse, error) {
	resp, err := n.Service.ListQemuPending(ctx, node, vmidText)
	if vmidText == "777" && n.w.afterSourceRead != nil {
		n.w.afterSourceRead()
	}
	if vmid, convErr := strconv.Atoi(vmidText); convErr == nil {
		runTailHook(ctx, vmid, tailRead)
	}
	return resp, err
}

func allocationEntryNamed(description, key string) bool {
	entries, err := pve.ParseDiskAllocationProvenance(description)
	if err != nil {
		return false
	}
	_, found := entries[key]
	return found
}

// buildTailDisk is buildUnownedDisk's managed shape on shared storage, with
// 777 carrying the allocation entry an attach writes and its description
// writes watched.
func buildTailDisk(t *testing.T) (*strandedDisk, string, *tailWrites) {
	t.Helper()
	s, id, _ := buildUnownedDisk(t, true, false)
	w := &tailWrites{token: s.token}
	s.deps.PVE = tailWritePVE{legacyDestroyPVE: s.deps.PVE.(legacyDestroyPVE), w: w}
	s.seedSourceEntry(t, 777, id)
	return s, id, w
}

// sourceEntry is the allocation entry 777 carries for the disk once an attach
// has written it.
func (s *strandedDisk) sourceEntry(t *testing.T, id string) pve.DiskAllocationProvenance {
	t.Helper()
	storage, _, err := pve.ParseDiskCID(s.stranded)
	if err != nil {
		t.Fatal(err)
	}
	backing, err := managedDiskActualBacking(context.Background(), s.deps, storage)
	if err != nil {
		t.Fatal(err)
	}
	return pve.DiskAllocationProvenance{Version: 1, AllocationID: id, AllocationNamespace: lifecycleFlowNamespace, Volid: s.stranded, Node: "n1", Backing: backing}
}

// seedSourceEntry writes the disk's allocation entry onto vmid the way an
// attach does, beneath the watch.
func (s *strandedDisk) seedSourceEntry(t *testing.T, vmid int, id string) {
	t.Helper()
	if err := pve.WriteDiskAllocationProvenance(context.Background(), s.client, "n1", vmid, s.token, s.sourceEntry(t, id)); err != nil {
		t.Fatalf("seed the allocation entry on %d: %v", vmid, err)
	}
}

// hasEntry reports whether vmid carries any allocation entry for the disk.
func (s *strandedDisk) hasEntry(vmid int) bool {
	cfg := s.client.state.configs[vmid]
	return cfg != nil && allocationEntryNamed(pve.DescriptionFromConfig(cfg), s.token)
}

// requireSourceClean checks that 777 names the disk in none of its notes.
func (s *strandedDisk) requireSourceClean(t *testing.T) {
	t.Helper()
	if s.hasEntry(777) {
		t.Fatal("777 still carries the disk's allocation entry")
	}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(s.client.state.configs[777]))
	for _, carrier := range []string{"bosh_attached_disks", pve.DiskOptOverlaysSentinelKey} {
		if strings.Contains(string(raw[carrier]), s.token) || strings.Contains(string(raw[carrier]), s.stranded) {
			t.Fatalf("777's %s still names the disk: %s", carrier, raw[carrier])
		}
	}
}

func (s *strandedDisk) requireRecord(t *testing.T, id string, want aj.State) {
	t.Helper()
	record, err := s.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != want {
		t.Fatalf("record state = %s (%s), want %s", record.State, record.Reason, want)
	}
}

// deferBySnapshot runs the first detach under a snapshot of 777 that names
// the volume, which leaves the park deferred, and then deletes the snapshot.
func (s *strandedDisk) deferBySnapshot(t *testing.T, id string) {
	t.Helper()
	s.withSnapshot(s.stranded)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk with a snapshot naming the volume: %v", err)
	}
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("%s is referenced by %v, want the park deferred", s.stranded, refs)
	}
	if !s.hasEntry(777) {
		t.Fatal("the deferral dropped 777's allocation entry while the disk had not landed")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	s.client.vmSnapshots, s.client.snapshotConfigs = nil, nil
}

// TestDetachTailDeferredParkThenDeleteDisk is the snapshot deferral followed
// by delete_disk once the snapshot is gone. The resume lands the disk on a
// parker and removes 777's entry, so the delete proves absence and the record
// ends Deleted.
func TestDetachTailDeferredParkThenDeleteDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk after the snapshot was deleted: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
	if s.client.state.volumes[s.stranded] != nil {
		t.Fatalf("%s survived delete_disk", s.stranded)
	}
}

// TestDetachTailDeferredParkRetriedDetach is a deferred detach retried after
// the snapshot is gone. The retry lands the park and leaves 777 naming the
// disk nowhere.
func TestDetachTailDeferredParkRetriedDetach(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk after the snapshot was deleted: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
}

// TestDetachTailDeferredParkAttachedElsewhere is attach_disk to 888 resuming
// the deferred park. Afterwards only 888 carries the disk's entry.
func TestDetachTailDeferredParkAttachedElsewhere(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	s.client.state.configs[888] = map[string]any{"name": "w888", "digest": "1"}
	if err := attachDiskAt(t, context.Background(), s.deps, "888", s.cid); err != nil {
		t.Fatalf("attach_disk(888) of the deferred disk: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 888 {
		t.Fatalf("the disk landed on VM %d, want 888", vmid)
	}
	s.requireSourceClean(t)
	if !s.hasEntry(888) {
		t.Fatal("888 carries no allocation entry for the disk it holds")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailRetryAfterFailedTransfer is a first transfer that fails
// retriably after the slot delete, because the snapshot listing it reads
// before the config-edit attach fails, and that attaches nothing. The
// Director's retry is readmitted, lands the disk, and removes 777's entry.
func TestDetachTailRetryAfterFailedTransfer(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildTailDisk(t)
	s.deps.Config.AllowDiskOpsWithSnapshots = true
	s.deps.Config.RequireSnapshotCheckPass = false
	s.client.snapshotErr = errors.New("snapshot listing failed")
	err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	requireRetriable(t, err, "detach_disk whose transfer fails after the slot delete")
	if refs := s.references(s.stranded); len(refs) != 0 {
		t.Fatalf("after the failed transfer %s is referenced by %v, want no reference", s.stranded, refs)
	}
	s.client.snapshotErr = nil
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("the retried detach_disk: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// buildLatentTailDisk is the state releases from 0.6.0 through 0.8.x leave.
// The record is ready to return, the disk has landed on a parker with no
// intent left, and 777 still carries the disk's entry.
func buildLatentTailDisk(t *testing.T) (*strandedDisk, string, *tailWrites) {
	t.Helper()
	s, id, w := buildTailDisk(t)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.seedSourceEntry(t, 777, id)
	if s.intentParker() == 0 {
		t.Fatal("the landed park left no parker record")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	w.removals = 0
	return s, id, w
}

// TestDetachTailLatentRecordDeletes is the 0.8.x latent shape. Its first
// delete_disk finds the source VM in the parker's landed entry, removes 777's
// entry, and then deletes the volume, so the record ends Deleted.
func TestDetachTailLatentRecordDeletes(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk of the latent record: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
	if s.client.state.volumes[s.stranded] != nil {
		t.Fatalf("%s survived delete_disk", s.stranded)
	}
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
}

// TestDetachTailDigestConflictRetries is a tail whose first write meets a
// config another writer changed after the tail read it. PVE refuses the write
// unchanged, and the tail reads 777 again and removes the entry.
func TestDetachTailDigestConflictRetries(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	w.conflictOnce = true
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk whose tail meets a changed config: %v", err)
	}
	if w.conflicts != 1 || w.removals != 1 {
		t.Fatalf("conflicts=%d removals=%d, want one refused write and one removal", w.conflicts, w.removals)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailFailureKeepsTheVolume is a latent record whose tail can't read
// 777. delete_disk fails retriably before anything deletes the volume, with
// the record returned because the tail sent nothing, and the next delete_disk
// heals the entry and deletes it.
func TestDetachTailFailureKeepsTheVolume(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	// 777 answers every read but the first one after delete_disk's lifecycle
	// is admitted, which is the tail's. The completion audit's reads of 777
	// are served, because a failed audit read is its own uncertainty.
	failed := false
	s.client.onConfigRead = func(vmid int) error {
		record, err := s.journal.Inspect(id)
		if vmid == 777 && !failed && err == nil && record.Reason == "lifecycle delete_disk admitted; completion pending" {
			failed = true
			return errors.New("connection reset reading 777")
		}
		return nil
	}
	err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk with an unreadable source")
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail failed", s.stranded)
	}
	s.requireSingleHolder(t, true)
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although the tail could not read 777")
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	s.client.onConfigRead = nil
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("the retried delete_disk: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailNormalDetachRemovesOnce is a control. A detach that transfers
// the disk at once removes 777's entry exactly once, and a repeat detach
// writes nothing more.
func TestDetachTailNormalDetachRemovesOnce(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk: %v", err)
	}
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("repeat detach_disk: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
	if w.removals != 1 {
		t.Fatalf("777's entry was removed %d times, want once", w.removals)
	}
}

// TestDetachTailSourceGone is a control. When 777 is gone, the tail has
// nothing to remove, and delete_disk of the latent record ends Deleted.
func TestDetachTailSourceGone(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	delete(s.client.state.configs, 777)
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk with 777 gone: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
}

// TestDetachTailMismatchedEntryStays is a control. An entry on 777 that names
// another node isn't the entry the disk left, so a repeat detach succeeds and
// leaves it, and delete_disk meets the deletion proof's refusal as before.
func TestDetachTailMismatchedEntryStays(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	entry := s.sourceEntry(t, id)
	entry.Node = "n2"
	s.requireMismatchStays(t, id, entry)
}

// requireMismatchStays writes entry onto 777 and checks that the tail leaves
// it there. A repeat detach succeeds, delete_disk fails, and the record isn't
// Deleted.
func (s *strandedDisk) requireMismatchStays(t *testing.T, id string, entry pve.DiskAllocationProvenance) {
	t.Helper()
	if err := pve.WriteDiskAllocationProvenance(context.Background(), s.client, "n1", 777, s.token, entry); err != nil {
		t.Fatal(err)
	}
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("repeat detach_disk with a mismatched entry on 777: %v", err)
	}
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err == nil {
		t.Fatal("delete_disk certified absence while 777 carries a mismatched entry for the allocation")
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(s.client.state.configs[777]))
	if err != nil {
		t.Fatal(err)
	}
	if entries[s.token] != entry {
		t.Fatalf("777's entry = %+v, want the mismatched entry left as it was", entries[s.token])
	}
	record, err := s.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.Deleted {
		t.Fatal("the record ended Deleted while 777 still carries an entry for it")
	}
}

// TestDetachTailOtherHolderStillRefuses is a control. When a VM other than
// the source still carries the allocation's entry, the tail leaves it alone,
// and deletionProof still refuses.
func TestDetachTailOtherHolderStillRefuses(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	s.seedSourceEntry(t, 778, id)
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err == nil {
		t.Fatal("delete_disk certified absence while 778 carries the allocation's entry")
	}
	if !s.hasEntry(778) {
		t.Fatal("778's entry was removed")
	}
	record, err := s.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.Deleted {
		t.Fatal("the record ended Deleted while 778 carries an entry for it")
	}
}

// countSteps counts the record's steps whose kind ends with suffix and that
// target 777.
func countSteps(t *testing.T, journal *aj.Journal, id, suffix string) (planned, observed int) {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if !strings.HasSuffix(step.Kind, suffix) || step.Target.VMID != 777 {
			continue
		}
		switch step.State {
		case aj.Planned:
			planned++
		case aj.Observed:
			observed++
		}
	}
	return planned, observed
}

// TestDetachTailStaleDigestBeforeSend is a latent record whose source VM
// changes between the tail's read and the guard's. The guard refuses the write
// before journaling or sending it and stays usable, and the tail reads 777
// again and removes the entry, so delete_disk ends Deleted.
func TestDetachTailStaleDigestBeforeSend(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	_, observedBefore := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig")
	changed := 0
	w.afterSourceRead = func() {
		record, err := s.journal.Inspect(id)
		if changed == 0 && err == nil && record.Reason == "lifecycle delete_disk admitted; completion pending" {
			changed++
			s.client.generation++
			s.client.state.configs[777]["digest"] = fmt.Sprint(s.client.generation + 100)
		}
	}
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk whose tail read went stale before the write: %v", err)
	}
	if changed != 1 || w.removals != 1 || w.conflicts != 0 {
		t.Fatalf("changed=%d removals=%d conflicts=%d, want one change before the send, one removal, and nothing refused by PVE", changed, w.removals, w.conflicts)
	}
	planned, observed := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig")
	if planned != 0 || observed != observedBefore+1 {
		t.Fatalf("777's config steps: planned=%d observed=%d, want none planned and only the sent write observed (%d before)", planned, observed, observedBefore)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailExhaustedRetryKeepsTheVolume is a latent record whose source
// VM changes before every write the tail sends. After three refused writes
// delete_disk returns retriable with the volume and 777's entry in place, and
// every refused write is settled. Once 777 settles down, the next delete_disk
// ends Deleted.
func TestDetachTailExhaustedRetryKeepsTheVolume(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	w.conflictAlways = true
	err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk whose source VM keeps changing")
	if w.conflicts != 3 || w.removals != 0 {
		t.Fatalf("conflicts=%d removals=%d, want three refused writes and no removal", w.conflicts, w.removals)
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s although the tail never finished", s.stranded)
	}
	if !s.hasEntry(777) {
		t.Fatal("777's entry went missing although every write was refused")
	}
	if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 0 {
		t.Fatalf("%d refused writes were left planned, want each settled", planned)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	w.conflictAlways = false
	if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
		t.Fatalf("the retried delete_disk: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailRefusedOnFirstDetach is a detach that moves the disk at once
// and whose removal of 777's entry PVE refuses for a stale digest. The refusal
// is settled, the removal is sent again from a fresh read, and the record goes
// back ready to return instead of needing reconciliation.
func TestDetachTailRefusedOnFirstDetach(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	w.conflictOnce = true
	if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk whose removal meets a changed config: %v", err)
	}
	if w.conflicts != 1 || w.removals != 1 {
		t.Fatalf("conflicts=%d removals=%d, want one refused write and one removal", w.conflicts, w.removals)
	}
	s.requireParkedUnderItsOwnName(t)
	s.requireSourceClean(t)
	s.requireRecord(t, id, aj.ReadyToReturn)
}

// TestDetachTailUnansweredWriteStillPoisons is a guard row. A removal whose
// answer may hide a write that landed, because the connection dropped or a
// gateway answered for PVE, is uncertain as before. The guard is poisoned and
// the write's step stays planned, even when the gateway's body carries PVE's
// refusal text.
func TestDetachTailUnansweredWriteStillPoisons(t *testing.T) {
	refusal, _ := json.Marshal(map[string]string{"message": "checksum mismatch (file change by other user?)\n"})
	for name, answer := range map[string]error{
		"connection reset": errors.New("connection reset by peer"),
		"gateway":          sdkerrors.ParseAPIError(502, refusal),
	} {
		t.Run(name, func(t *testing.T) {
			captureParkerPoolSweep(t)
			s, id, w := buildTailDisk(t)
			w.failWith = answer
			if err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid); err == nil {
				t.Fatal("detach_disk succeeded although its removal went unanswered")
			}
			if planned, _ := countSteps(t, s.journal, id, "_Nodes_UpdateQemuConfig"); planned != 1 {
				t.Fatalf("%d planned config steps on 777, want the unanswered write left planned", planned)
			}
			record, err := s.journal.Inspect(id)
			if err != nil {
				t.Fatal(err)
			}
			if record.State == aj.ReadyToReturn || record.State == aj.Deleted {
				t.Fatalf("record state = %s after an unanswered write, want it left for reconciliation", record.State)
			}
		})
	}
}

// TestDetachTailStillNamedRefuses is a latent record whose source VM names the
// landed volume again. The tail refuses retriably before it removes anything,
// and delete_disk leaves the volume alone.
func TestDetachTailStillNamedRefuses(t *testing.T) {
	captureParkerPoolSweep(t)
	s, _, w := buildLatentTailDisk(t)
	s.client.state.configs[777]["unused5"] = s.stranded
	err := deleteDiskAt(t, context.Background(), s.deps, s.cid)
	requireRetriable(t, err, "delete_disk while 777 names the volume")
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("delete_disk deleted %s while 777 names it", s.stranded)
	}
	if !s.hasEntry(777) || w.removals != 0 {
		t.Fatalf("777's entry present=%t removals=%d, want it left in place", s.hasEntry(777), w.removals)
	}
}

// TestDetachTailNoMoveStepLeavesEntry is a record with no observed move,
// which is how a disk parked by config edit looks, and how an older journal
// might. An entry naming a volid other than the landed one can't be tied to
// the disk, so the tail leaves it, and delete_disk refuses as before.
func TestDetachTailNoMoveStepLeavesEntry(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, _ := buildLatentTailDisk(t)
	if planned, observed := countSteps(t, s.journal, id, "_Nodes_CreateQemuMoveDisk"); planned+observed != 0 {
		t.Fatalf("the record holds %d move steps, want none", planned+observed)
	}
	entry := s.sourceEntry(t, id)
	storage, _, err := pve.ParseDiskCID(s.stranded)
	if err != nil {
		t.Fatal(err)
	}
	entry.Volid = storage + ":vm-777-disk-7"
	s.requireMismatchStays(t, id, entry)
}

// tailManaged is digestManagedFixture with 777's allocation entry for the
// disk as the attach wrote it, before any detach.
func tailManaged(t *testing.T) (digestManaged, string, pve.DiskAllocationProvenance) {
	t.Helper()
	return tailEntry(t, digestManagedFixture(t))
}

// tailManagedOnDiskZero is tailManaged with the disk named vm-777-disk-0 on
// 777, which is the only name a legacy create_disk gives a volume. PVE gives
// the first disk it moves to 777 that name.
func tailManagedOnDiskZero(t *testing.T) (digestManaged, string, pve.DiskAllocationProvenance) {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	client.moves = -1
	volume := digestOwnedByVM(t, deps, client, cid)
	if !strings.HasSuffix(volume, ":777/vm-777-disk-0.raw") {
		t.Fatalf("777 holds the disk as %s, want vm-777-disk-0", volume)
	}
	return tailEntry(t, digestManaged{deps: deps, client: client, journal: journal, id: id, cid: cid, volume: volume})
}

// tailEntry returns f with the key and value of 777's allocation entry for
// its disk, which must be 777's only entry.
func tailEntry(t *testing.T, f digestManaged) (digestManaged, string, pve.DiskAllocationProvenance) {
	t.Helper()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(f.client.state.configs[777]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("777 carries entries %v (%v), want the disk's alone", entries, err)
	}
	for key, entry := range entries {
		if entry.Volid != f.volume {
			t.Fatalf("777's entry names %s, want %s", entry.Volid, f.volume)
		}
		return f, key, entry
	}
	panic("unreachable")
}

// parkRenamed detaches the managed disk with move_disk, which renames it for
// the parker, and then writes 777's entry back as the attach wrote it. That is
// the state a detach that moved the disk without reaching its tail leaves.
func parkRenamed(t *testing.T, f digestManaged, key string, entry pve.DiskAllocationProvenance) {
	t.Helper()
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("park the disk with move_disk: %v", err)
	}
	if f.client.state.volumes[f.volume] != nil {
		t.Fatalf("%s kept its name, want the move to rename it", f.volume)
	}
	if _, observed := countSteps(t, f.journal, f.id, "_Nodes_CreateQemuMoveDisk"); observed == 0 {
		t.Fatal("the record holds no observed move off 777")
	}
	if err := pve.WriteDiskAllocationProvenance(context.Background(), f.client, "n1", 777, key, entry); err != nil {
		t.Fatal(err)
	}
}

// TestDetachTailRenamedLandingHeals is a disk whose move renamed it, with
// 777's entry still naming the pre-move volid. The record's own observed move
// names that volid as its source, so delete_disk removes the entry and ends
// Deleted.
func TestDetachTailRenamedLandingHeals(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk of the renamed disk: %v", err)
	}
	if allocationEntryNamed(pve.DescriptionFromConfig(f.client.state.configs[777]), key) {
		t.Fatal("777 still carries the renamed disk's allocation entry")
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Deleted {
		t.Fatalf("record state = %s (%s), want deleted", record.State, record.Reason)
	}
}

// TestDetachTailUnrelatedVolidLeft is a renamed disk whose 777 entry names a
// volid that neither the landing nor the record's move names. The tail
// leaves it, a repeat detach succeeds, and delete_disk refuses as before.
func TestDetachTailUnrelatedVolidLeft(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	storage, _, err := pve.ParseDiskCID(f.volume)
	if err != nil {
		t.Fatal(err)
	}
	entry.Volid = storage + ":vm-777-disk-7"
	if err := pve.WriteDiskAllocationProvenance(context.Background(), f.client, "n1", 777, key, entry); err != nil {
		t.Fatal(err)
	}
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("repeat detach_disk with an unrelated entry on 777: %v", err)
	}
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err == nil {
		t.Fatal("delete_disk certified absence while 777 carries an unrelated entry for the allocation")
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(f.client.state.configs[777]))
	if err != nil {
		t.Fatal(err)
	}
	if entries[key] != entry {
		t.Fatalf("777's entry = %+v, want the unrelated entry left as it was", entries[key])
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.Deleted {
		t.Fatal("the record ended Deleted while 777 still carries an entry for it")
	}
}

// renamedLanding returns the volid the record's observed move off 777 landed
// the disk as, and checks that the move renamed it.
func renamedLanding(t *testing.T, f digestManaged) string {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed || !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") {
			continue
		}
		if step.Target.VMID != 777 || len(step.VolIDs) < 2 || step.VolIDs[0] != f.volume {
			continue
		}
		landed := step.VolIDs[len(step.VolIDs)-1]
		if landed == f.volume || step.Target.IntendedVolume != f.volume {
			t.Fatalf("the move off 777 intended %s and landed %s, want a rename of %s", step.Target.IntendedVolume, landed, f.volume)
		}
		return landed
	}
	t.Fatal("the record holds no observed move off 777")
	return ""
}

// notesFiledUnder returns the raw JSON of every allocation entry, attached-disk
// entry, and drive-option overlay that cfg's description files under keys.
func notesFiledUnder(t *testing.T, cfg map[string]any, keys ...string) map[string]string {
	t.Helper()
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	notes := map[string]string{}
	for _, carrier := range []string{"bosh_disk_allocations", "bosh_attached_disks", pve.DiskOptOverlaysSentinelKey} {
		if len(raw[carrier]) == 0 {
			continue
		}
		var filed map[string]json.RawMessage
		if err := json.Unmarshal(raw[carrier], &filed); err != nil {
			t.Fatalf("777's %s: %v", carrier, err)
		}
		for _, key := range keys {
			if note, found := filed[key]; found {
				notes[carrier+"/"+key] = string(note)
			}
		}
	}
	return notes
}

// TestDetachTailRenamedStillNamesLanded is a renamed disk whose source VM
// names the landed volid again. The source still holds the volume, so
// delete_disk refuses retriably, and nothing is written to 777.
func TestDetachTailRenamedStillNamesLanded(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	f.client.state.configs[777]["unused5"] = landed
	before := pve.DescriptionFromConfig(f.client.state.configs[777])
	_, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	requireRetriable(t, err, "delete_disk while 777 names the landed volume")
	if after := pve.DescriptionFromConfig(f.client.state.configs[777]); after != before {
		t.Fatalf("777's description changed while it names the landed volume:\nbefore %s\nafter  %s", before, after)
	}
	if f.client.state.volumes[landed] == nil {
		t.Fatalf("delete_disk deleted %s while 777 names it", landed)
	}
}

// oldNameHolding returns what 777 keeps for the disk on the renamed disk's
// old name. That's its notes filed under serial or the old name, the slot that
// holds the old name, and the volume's storage entry.
func oldNameHolding(t *testing.T, f digestManaged, serial string) map[string]string {
	t.Helper()
	cfg := f.client.state.configs[777]
	held := notesFiledUnder(t, cfg, serial, f.volume)
	for key, value := range cfg {
		if text, ok := value.(string); ok && strings.HasPrefix(text, f.volume+",") {
			held["slot/"+key] = text
		}
	}
	if info := f.client.state.volumes[f.volume]; info != nil {
		held["volume"] = fmt.Sprintf("%+v", *info)
	}
	return held
}

// holdOldName gives the renamed disk's old name to another disk, which 777
// holds on scsi3 with its own serial and the notes an attach writes for it,
// filed under allocation id in namespace. It returns what 777 keeps for that
// disk.
func holdOldName(t *testing.T, f digestManaged, entry pve.DiskAllocationProvenance, serial, id, namespace string) map[string]string {
	t.Helper()
	ctx := context.Background()
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["scsi3"] = f.volume + ",serial=" + serial
	other := entry
	other.AllocationID = id
	other.AllocationNamespace = namespace
	if err := pve.WriteDiskAllocationProvenance(ctx, f.client, "n1", 777, serial, other); err != nil {
		t.Fatal(err)
	}
	pve.UpdateAttachedDiskCID(ctx, f.client, nil, "n1", 777, f.volume, "other-disk-cid")
	if err := pve.SetVMDiskOptOverlay(ctx, f.client, "n1", 777, f.volume, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	return requireOldNameHeld(t, f, serial)
}

// requireOldNameHeld checks that 777 keeps the other disk's allocation entry,
// attached-disk entry, overlay, slot, and volume, and returns them.
func requireOldNameHeld(t *testing.T, f digestManaged, serial string) map[string]string {
	t.Helper()
	held := oldNameHolding(t, f, serial)
	if len(held) != 5 {
		t.Fatalf("777 keeps %v for the disk on %s, want its allocation entry, attached-disk entry, overlay, slot, and volume", held, f.volume)
	}
	return held
}

// ownDiskOnOldName journals another disk of this Director and brings it to
// 777 the way a disk type change in the same deployment does, so PVE gives it
// the renamed disk's old name. Spread placement picks a disk's storage from
// its allocation ID, so it journals disks until one lands on the old name's
// storage. The disk's options then go into 777's overlay under its serial,
// the way an attach with disk options files them.
func ownDiskOnOldName(t *testing.T, f digestManaged) lifecycleFlowDisk {
	t.Helper()
	storage, _, err := pve.ParseDiskCID(f.volume)
	if err != nil {
		t.Fatal(err)
	}
	var other lifecycleFlowDisk
	for attempt := 0; attempt < 32 && other.storage != storage; attempt++ {
		other = journalLifecycleFlowDisk(t, f.journal, f.client.state, true, false)
	}
	if other.storage != storage {
		t.Fatalf("no journaled disk was placed on %s", storage)
	}
	if err := digestAttach(t, f.deps, other.cid); err != nil {
		t.Fatalf("attach the other disk under its created name: %v", err)
	}
	if err := digestDetach(t, f.deps, other.cid); err != nil {
		t.Fatalf("park the other disk by config edit: %v", err)
	}
	// PVE names a disk it moves to 777 with 777's lowest free disk number,
	// which the renamed disk gave up.
	f.client.moves = 0
	if err := digestAttach(t, f.deps, other.cid); err != nil {
		t.Fatalf("move the other disk to 777: %v", err)
	}
	if err := pve.SetVMDiskOptOverlay(context.Background(), f.client, "n1", 777, other.token, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	return other
}

// requireOldNameHolderKept checks that 777 keeps everything it held for the
// disk on the old name byte for byte, that the renamed disk's notes are gone
// from 777, and that its record ended Deleted.
func requireOldNameHolderKept(t *testing.T, f digestManaged, key, serial string, before map[string]string) {
	t.Helper()
	if notes := notesFiledUnder(t, f.client.state.configs[777], key); len(notes) != 0 {
		t.Fatalf("777 still files %v under the renamed disk's serial", notes)
	}
	after := oldNameHolding(t, f, serial)
	if len(after) != len(before) {
		t.Fatalf("777 keeps %v for the disk on the old name, want %v", after, before)
	}
	for held, was := range before {
		if after[held] != was {
			t.Fatalf("the other disk's %s = %s, want %s", held, after[held], was)
		}
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Deleted {
		t.Fatalf("record state = %s (%s), want deleted", record.State, record.Reason)
	}
}

// recordBytes returns the journal record of allocation id as JSON.
func recordBytes(t *testing.T, journal *aj.Journal, id string) string {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestDetachTailRenamedOldNameHeldByOwnDisk is a renamed disk whose old name
// PVE has since given to another disk of this Director, as after a disk type
// change in the same deployment. That disk has its own journal record, so the
// audit knows its allocation and the tail runs. delete_disk ends Deleted, and
// the other disk's notes, slot, volume, and record stay byte for byte as they
// were.
func TestDetachTailRenamedOldNameHeldByOwnDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	renamedLanding(t, f)
	other := ownDiskOnOldName(t, f)
	before := requireOldNameHeld(t, f, other.token)
	if value := before["bosh_disk_allocations/"+other.token]; !strings.Contains(value, `"allocation_namespace":"`+lifecycleFlowNamespace+`"`) || !strings.Contains(value, other.id) {
		t.Fatalf("the other disk's allocation entry = %s, want allocation %s in namespace %s", value, other.id, lifecycleFlowNamespace)
	}
	recordBefore := recordBytes(t, f.journal, other.id)
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk with this Director's other disk on the old name: %v", err)
	}
	requireOldNameHolderKept(t, f, key, other.token, before)
	if recordAfter := recordBytes(t, f.journal, other.id); recordAfter != recordBefore {
		t.Fatalf("the other disk's record changed:\nbefore %s\nafter  %s", recordBefore, recordAfter)
	}
}

// TestDetachTailRenamedOldNameHeldByAnotherDirector is a renamed disk whose
// old name PVE has since given to a disk of another Director. The old name is
// no longer ours, so the tail goes on, and delete_disk ends Deleted with the
// other disk's notes byte for byte as they were.
func TestDetachTailRenamedOldNameHeldByAnotherDirector(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	renamedLanding(t, f)
	const otherSerial = "bpd-9999888877776666"
	before := holdOldName(t, f, entry, otherSerial, "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d", "other-director")
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk with another Director's disk on the old name: %v", err)
	}
	requireOldNameHolderKept(t, f, key, otherSerial, before)
}

// TestDetachTailRenamedOldNameHeldByLegacyDisk is a renamed disk whose old
// name, vm-777-disk-0, a legacy disk of this Director now holds. A legacy
// create_disk always names its volume disk-0, and that state comes only from
// v0.6.0 or from attaches an earlier release made. A legacy disk has no
// stable ID and no allocation entry, so 777 files its attached-disk entry and
// overlay under its birth volid, which is the old name. The tail
// removes only the renamed disk's allocation entry and leaves the legacy
// disk's notes, slot, and volume byte for byte as they were. delete_disk then
// destroys the renamed disk's landed volume, which is the data the Director
// asked to delete. The move that renamed the disk is observed, so the
// completion audit no longer counts the legacy disk's serial-less volume under
// the old name as the renamed disk's artifact, and the record ends Deleted.
func TestDetachTailRenamedOldNameHeldByLegacyDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManagedOnDiskZero(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	ctx := context.Background()
	f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	f.client.state.configs[777]["scsi3"] = f.volume + ",size=1G"
	pve.UpdateAttachedDiskCID(ctx, f.client, nil, "n1", 777, f.volume, "legacy-disk-cid")
	if err := pve.SetVMDiskOptOverlay(ctx, f.client, "n1", 777, f.volume, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	held := oldNameHolding(t, f, "")
	if len(held) != 4 {
		t.Fatalf("777 keeps %v for the legacy disk on %s, want its attached-disk entry, overlay, slot, and volume", held, f.volume)
	}
	before := captureRenamedNameState(t, f)
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk with a legacy disk on the old name: %v", err)
	}
	if notes := notesFiledUnder(t, f.client.state.configs[777], key); len(notes) != 0 {
		t.Fatalf("777 still files %v under the renamed disk's serial", notes)
	}
	if after := oldNameHolding(t, f, ""); fmt.Sprint(after) != fmt.Sprint(held) {
		t.Fatalf("777 keeps %v for the legacy disk on the old name, want %v", after, held)
	}
	requireRenamedDiskDeletedAlone(t, f, key, landed, before)
}

// descriptionNotes returns every note 777's description files, keyed by its
// carrier and key, with its raw JSON.
func descriptionNotes(t *testing.T, cfg map[string]any) map[string]string {
	t.Helper()
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	notes := map[string]string{}
	for carrier, value := range raw {
		var filed map[string]json.RawMessage
		if err := json.Unmarshal(value, &filed); err != nil {
			notes[carrier] = string(value)
			continue
		}
		for key, note := range filed {
			notes[carrier+"/"+key] = string(note)
		}
	}
	return notes
}

// configWithoutNotes returns 777's config apart from its description and
// digest, printed with its keys sorted.
func configWithoutNotes(cfg map[string]any) string {
	rest := map[string]any{}
	for key, value := range cfg {
		if key != "description" && key != "digest" {
			rest[key] = value
		}
	}
	return fmt.Sprint(rest)
}

// volumeNames returns the volids storage lists, sorted.
func volumeNames(f digestManaged) []string {
	names := make([]string, 0, len(f.client.state.volumes))
	for volid := range f.client.state.volumes {
		names = append(names, volid)
	}
	sort.Strings(names)
	return names
}

// TestDetachTailRenamedOldNameUnknownAllocationRefuses is a renamed disk whose
// old name another disk holds under an allocation in this Director's
// namespace that the journal doesn't know. The tail rests on the renamed
// disk's own proofs, so it removes that disk's allocation entry from 777 and
// nothing else. delete_disk then destroys the renamed disk's landed volume on
// the parker, and the completion audit refuses on the unknown allocation, as
// it does for any delete_disk while our namespace holds one. Once the unknown
// allocation is resolved, a second delete_disk leaves 777 alone, destroys
// nothing more, and ends Deleted.
//
// Losing the landed volume on the first pass isn't data loss. The Director
// asked to delete this disk, and the landed volume is this disk's data, so
// destroying it is what the call is for. The tail doesn't change which volume
// delete_disk picks. It only removes the stale entry on 777 that kept the
// deletion proof refusing, and the audit's refusal afterwards is about the
// other disk.
func TestDetachTailRenamedOldNameUnknownAllocationRefuses(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManaged(t)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	const unknown, serial = "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d", "bpd-9999888877776666"
	held := holdOldName(t, f, entry, serial, unknown, lifecycleFlowNamespace)
	cfg := f.client.state.configs[777]
	notesBefore, restBefore := descriptionNotes(t, cfg), configWithoutNotes(cfg)
	volumesBefore := volumeNames(f)
	_, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err == nil || !strings.Contains(err.Error(), "delete_disk refused: 1 audit conflict") || !strings.Contains(err.Error(), "carries unknown allocation "+unknown) {
		t.Fatalf("delete_disk with an unknown allocation on the old name: err = %v, want the completion audit's refusal", err)
	}
	requirePermanent(t, err, "the completion audit's refusal")
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("the completion audit's refusal has type %v, want %s", err, cpierrors.TypeCloud)
	}
	var destroyed []string
	for _, volid := range volumesBefore {
		if f.client.state.volumes[volid] == nil {
			destroyed = append(destroyed, volid)
		}
	}
	if len(destroyed) != 1 || destroyed[0] != landed || len(volumeNames(f)) != len(volumesBefore)-1 {
		t.Fatalf("delete_disk destroyed %v and left %v, want only the landed volume %s destroyed", destroyed, volumeNames(f), landed)
	}
	notesAfter := descriptionNotes(t, cfg)
	gone := "bosh_disk_allocations/" + key
	if _, found := notesAfter[gone]; found {
		t.Fatal("777 still carries the renamed disk's allocation entry")
	}
	for note, was := range notesBefore {
		if note != gone && notesAfter[note] != was {
			t.Fatalf("777's %s = %q, want %s left as it was", note, notesAfter[note], was)
		}
	}
	if len(notesAfter) != len(notesBefore)-1 {
		t.Fatalf("777's notes went from %v to %v, want only %s gone", notesBefore, notesAfter, gone)
	}
	if rest := configWithoutNotes(cfg); rest != restBefore {
		t.Fatalf("777's config changed beyond its notes:\nbefore %s\nafter  %s", restBefore, rest)
	}
	if after := oldNameHolding(t, f, serial); fmt.Sprint(after) != fmt.Sprint(held) {
		t.Fatalf("777 keeps %v for the disk on the old name, want %v", after, held)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("record state = %s (%s), want %s", record.State, record.Reason, aj.ReconciliationRequired)
	}

	// The operator resolves the unknown allocation, and the Director retries.
	other := entry
	other.AllocationID = unknown
	other.AllocationNamespace = lifecycleFlowNamespace
	if err := pve.RemoveDiskAllocationEntry(context.Background(), f.client, "n1", 777, serial, other, f.client.state.configs[777], nil, nil); err != nil {
		t.Fatal(err)
	}
	resolved, volumesResolved := fmt.Sprint(cfg), fmt.Sprint(volumeNames(f))
	if _, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_disk once the unknown allocation is resolved: %v", err)
	}
	if after := fmt.Sprint(cfg); after != resolved {
		t.Fatalf("the second delete_disk wrote to 777:\nbefore %s\nafter  %s", resolved, after)
	}
	if after := fmt.Sprint(volumeNames(f)); after != volumesResolved {
		t.Fatalf("the second delete_disk left volumes %s, want %s", after, volumesResolved)
	}
	if record, err = f.journal.Inspect(f.id); err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Deleted {
		t.Fatalf("record state = %s (%s), want deleted", record.State, record.Reason)
	}
}

// TestDetachTailUnrenamedStillNamedRefuses is a disk that landed under its own
// name, which 777 names again on an unused entry. With no rename, the name is
// still the disk's own, so a repeat detach refuses retriably and writes
// nothing to 777.
func TestDetachTailUnrenamedStillNamedRefuses(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildLatentTailDisk(t)
	if planned, observed := countSteps(t, s.journal, id, "_Nodes_CreateQemuMoveDisk"); planned+observed != 0 {
		t.Fatalf("the record holds %d move steps, want none", planned+observed)
	}
	s.client.state.configs[777]["unused6"] = s.stranded
	before := pve.DescriptionFromConfig(s.client.state.configs[777])
	err := detachDiskAt(t, context.Background(), s.deps, "777", s.cid)
	requireRetriable(t, err, "detach_disk while 777 names the volume")
	if after := pve.DescriptionFromConfig(s.client.state.configs[777]); after != before || w.removals != 0 {
		t.Fatalf("777 was written while it names the volume (removals %d):\nbefore %s\nafter  %s", w.removals, before, after)
	}
	if s.client.state.volumes[s.stranded] == nil {
		t.Fatalf("detach_disk lost %s", s.stranded)
	}
}

// seedOtherTailDisk journals a second disk of this Director and writes its
// allocation entry onto 777, the way two disks orphaned from one VM leave it.
// It also files the attached-disk entry and overlay an attach writes for the
// first disk under its serial, so every note the tail removes is there to
// remove. It returns the second disk's serial.
func (s *strandedDisk) seedOtherTailDisk(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	other := journalLifecycleFlowDisk(t, s.journal, s.client.state, true, false)
	backing, err := managedDiskActualBacking(ctx, s.deps, other.storage)
	if err != nil {
		t.Fatal(err)
	}
	entry := pve.DiskAllocationProvenance{Version: 1, AllocationID: other.id, AllocationNamespace: lifecycleFlowNamespace, Volid: other.volume, Node: "n1", Backing: backing}
	if err := pve.WriteDiskAllocationProvenance(ctx, s.client, "n1", 777, other.token, entry); err != nil {
		t.Fatal(err)
	}
	pve.UpdateAttachedDiskCID(ctx, s.client, nil, "n1", 777, s.token, s.cid)
	if err := pve.SetVMDiskOptOverlay(ctx, s.client, "n1", 777, s.token, map[string]string{"cache": "none"}); err != nil {
		t.Fatal(err)
	}
	return other.token
}

// removeOtherTailEntry is the second disk's own tail landing on 777 from
// another goroutine. It removes that disk's allocation entry with a write PVE
// applies, which moves 777's digest on.
func (s *strandedDisk) removeOtherTailEntry(otherToken string) error {
	done := make(chan error)
	go func() {
		nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(s.client.state.configs[777]))
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw["bosh_disk_allocations"], &entries); err != nil {
			done <- err
			return
		}
		delete(entries, otherToken)
		encoded, err := json.Marshal(entries)
		if err != nil {
			done <- err
			return
		}
		raw["bosh_disk_allocations"] = encoded
		description, err := pve.RenderSentinel(nonBOSH, raw)
		if err != nil {
			done <- err
			return
		}
		done <- s.client.Nodes().UpdateQemuConfig(context.Background(), "n1", "777", &nodes.UpdateQemuConfigParams{Description: &description})
	}()
	return <-done
}

// deleteAdmitted reports whether the record of allocation id shows delete_disk's
// lifecycle admitted and not yet complete.
func (s *strandedDisk) deleteAdmitted(id string) bool {
	record, err := s.journal.Inspect(id)
	return err == nil && record.Reason == "lifecycle delete_disk admitted; completion pending"
}

// TestDetachTailConcurrentTailKeepsOtherRemoval is a latent record whose
// delete_disk races the tail of a second disk orphaned from 777. The second
// disk's tail removes its own entry from 777 while the first tail runs. A first
// run records the order of 777's reads and writes once delete_disk's lifecycle
// is admitted, which is where the tail runs. Then, for each description write,
// one race lands right after the read that write is built from, before the
// guard reads 777 again, another lands as the write reaches PVE, and a third
// lands after the write and before the read that follows it. Wherever it
// lands, the second disk's removal must survive. When it lands before the
// guard's read, the guard refuses the stale write before it sends it, and when
// it lands as the write arrives, PVE refuses the write as a checksum mismatch.
// Either way the first delete_disk succeeds, or it refuses retriably and
// succeeds when it runs again. When it lands after the write, the first
// delete_disk succeeds with nothing left to settle.
func TestDetachTailConcurrentTailKeepsOtherRemoval(t *testing.T) {
	captureParkerPoolSweep(t)
	events := recordConcurrentTailEvents(t)
	for _, r := range concurrentTailRaces(t, events) {
		t.Run(r.name, func(t *testing.T) {
			runConcurrentTailRace(t, events, r)
		})
	}
}

// concurrentTailRace is one place the second disk's tail lands, the index of
// the event it follows among 777's events once delete_disk is admitted.
type concurrentTailRace struct {
	name string
	at   int
}

// recordConcurrentTailEvents runs delete_disk with no race and returns 777's
// events from the point its lifecycle is admitted.
func recordConcurrentTailEvents(t *testing.T) []tailEvent {
	t.Helper()
	var events []tailEvent
	s, id, _ := buildLatentTailDisk(t)
	s.seedOtherTailDisk(t)
	ctx := withTailHook(context.Background(), func(event tailEvent) {
		if s.deleteAdmitted(id) {
			events = append(events, event)
		}
	})
	if err := deleteDiskAt(t, ctx, s.deps, s.cid); err != nil {
		t.Fatalf("delete_disk with no race: %v", err)
	}
	s.requireRecord(t, id, aj.Deleted)
	return events
}

// concurrentTailRaces returns three races for each description write in
// events. Each write must follow the read it's built from and the guard's
// read, and be followed by its landing.
func concurrentTailRaces(t *testing.T, events []tailEvent) []concurrentTailRace {
	t.Helper()
	var races []concurrentTailRace
	for i, event := range events {
		if event != tailWrite {
			continue
		}
		write := len(races)/3 + 1
		if i < 2 || events[i-1] != tailRead || events[i-2] != tailRead || i+1 == len(events) || events[i+1] != tailWritten {
			t.Fatalf("777's events %v don't show write %d built from a read, guarded by another, and landing", events, write)
		}
		races = append(races,
			concurrentTailRace{fmt.Sprintf("after the read write %d is built from", write), i - 2},
			concurrentTailRace{fmt.Sprintf("as write %d arrives", write), i},
			concurrentTailRace{fmt.Sprintf("after write %d lands", write), i + 1})
	}
	if len(races) == 0 {
		t.Fatalf("delete_disk sent no description write to 777, events %v", events)
	}
	return races
}

// runConcurrentTailRace runs delete_disk with the second disk's tail landing
// at r, and checks that its removal survives and that delete_disk ends Deleted.
func runConcurrentTailRace(t *testing.T, events []tailEvent, r concurrentTailRace) {
	t.Helper()
	s, id, w := buildLatentTailDisk(t)
	otherToken := s.seedOtherTailDisk(t)
	seen := 0
	raced := false
	var raceErr error
	ctx := withTailHook(context.Background(), func(event tailEvent) {
		if !s.deleteAdmitted(id) {
			return
		}
		at := seen
		seen++
		if at != r.at {
			return
		}
		raced = true
		if event != events[r.at] {
			raceErr = fmt.Errorf("event %d is a %s, want a %s", r.at, event, events[r.at])
			return
		}
		raceErr = s.removeOtherTailEntry(otherToken)
	})
	err := deleteDiskAt(t, ctx, s.deps, s.cid)
	if !raced || raceErr != nil {
		t.Fatalf("the second disk's tail ran %v: %v", raced, raceErr)
	}
	if allocationEntryNamed(pve.DescriptionFromConfig(s.client.state.configs[777]), otherToken) {
		t.Fatal("777 carries the second disk's allocation entry again after its tail removed it")
	}
	switch landed := events[r.at] == tailWritten; {
	case landed && err != nil:
		t.Fatalf("delete_disk whose write landed before the second disk's tail: %v", err)
	case events[r.at] == tailWrite && w.conflicts == 0:
		t.Fatalf("PVE refused no write that arrived after the second disk's tail (err = %v)", err)
	case err != nil:
		requireRetriable(t, err, "delete_disk that met the second disk's tail")
		if err := deleteDiskAt(t, context.Background(), s.deps, s.cid); err != nil {
			t.Fatalf("delete_disk run again after the race: %v", err)
		}
		if allocationEntryNamed(pve.DescriptionFromConfig(s.client.state.configs[777]), otherToken) {
			t.Fatal("777 carries the second disk's allocation entry again after the rerun")
		}
	}
	s.requireRecord(t, id, aj.Deleted)
	s.requireSourceClean(t)
}

// TestDetachTailBirthOnHeldOldNameKeepsHolder is a renamed disk whose CID
// carries vm-777-disk-0 as its birth name, which the CID of a disk that the
// journal manages through its allocation marker can carry. Another Director's
// disk now holds that name on 777's unused3, with its attached-disk entry and
// overlay filed under it. A slot would make the identity scan resolve the CID
// to that disk, so the unused entry is the shape that reaches the tail. The
// tail must hand that name to no removal, so every write to 777 keeps the
// other disk's notes, and its unused entry, volume, and notes come through
// byte for byte. The renamed disk's own notes still leave 777.
func TestDetachTailBirthOnHeldOldNameKeepsHolder(t *testing.T) {
	captureParkerPoolSweep(t)
	f, key, entry := tailManagedOnDiskZero(t)
	parkRenamed(t, f, key, entry)
	renamedLanding(t, f)
	const otherSerial = "bpd-9999888877776666"
	holdOldName(t, f, entry, otherSerial, "0b5f3c2e-4d6a-4f1b-9c8d-7e6f5a4b3c2d", "other-director")
	delete(f.client.state.configs[777], "scsi3")
	f.client.state.configs[777]["unused3"] = f.volume
	notesBefore := notesFiledUnder(t, f.client.state.configs[777], otherSerial, f.volume)
	if len(notesBefore) != 3 {
		t.Fatalf("777 files %v for the other disk, want its allocation entry, attached-disk entry, and overlay", notesBefore)
	}
	configBefore := configWithoutNotes(f.client.state.configs[777])
	volumeBefore := fmt.Sprintf("%+v", *f.client.state.volumes[f.volume])
	_, meta, err := pve.ParseEncodedDiskCID(f.cid)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID(f.volume, meta)
	if err != nil {
		t.Fatal(err)
	}
	var touched []string
	f.client.afterConfigWrite = func(vmid int) {
		if vmid != 777 {
			return
		}
		if notes := notesFiledUnder(t, f.client.state.configs[777], otherSerial, f.volume); fmt.Sprint(notes) != fmt.Sprint(notesBefore) {
			touched = append(touched, fmt.Sprint(notes))
		}
	}
	_, deleteErr := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
	f.client.afterConfigWrite = nil
	if len(touched) != 0 {
		t.Fatalf("delete_disk (err = %v) wrote 777 with the other disk's notes as %v, want %v", deleteErr, touched, notesBefore)
	}
	if notes := notesFiledUnder(t, f.client.state.configs[777], otherSerial, f.volume); fmt.Sprint(notes) != fmt.Sprint(notesBefore) {
		t.Fatalf("777 files %v for the other disk, want %v", notes, notesBefore)
	}
	if after := configWithoutNotes(f.client.state.configs[777]); after != configBefore {
		t.Fatalf("777's config changed apart from its notes:\nbefore %s\nafter  %s", configBefore, after)
	}
	if info := f.client.state.volumes[f.volume]; info == nil || fmt.Sprintf("%+v", *info) != volumeBefore {
		t.Fatalf("the other disk's volume %s = %v, want %s", f.volume, info, volumeBefore)
	}
	if notes := notesFiledUnder(t, f.client.state.configs[777], key); len(notes) != 0 {
		t.Fatalf("777 still files %v under the renamed disk's serial (delete_disk err = %v)", notes, deleteErr)
	}
	// The completion audit counts the other disk's volume, which carries the
	// birth name, as the renamed disk's artifact, the same limit the legacy row
	// shows, so the record waits for reconciliation.
	if deleteErr == nil || !strings.Contains(deleteErr.Error(), "managed disk artifact or provenance remains; reconciliation required") {
		t.Fatalf("delete_disk with the birth name held: err = %v, want the completion audit's refusal", deleteErr)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("record state = %s (%s), want %s", record.State, record.Reason, aj.ReconciliationRequired)
	}
}

// TestDetachTailNotRunByUpdateDisk pins that update_disk stays out of the
// tail. It resumes an unfinished transfer inside the disk's lifecycle, but it
// skips the tail even there, so a parked disk whose source VM still carries
// its allocation entry keeps that entry and 777's description through
// update_disk.
func TestDetachTailNotRunByUpdateDisk(t *testing.T) {
	captureParkerPoolSweep(t)
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (*strandedDisk, *tailWrites)
	}{
		{"parked", func(t *testing.T) (*strandedDisk, *tailWrites) {
			s, _, w := buildLatentTailDisk(t)
			return s, w
		}},
		{"park deferred by a snapshot", func(t *testing.T) (*strandedDisk, *tailWrites) {
			s, id, w := buildTailDisk(t)
			s.deferBySnapshot(t, id)
			return s, w
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, w := tc.build(t)
			before := pve.DescriptionFromConfig(s.client.state.configs[777])
			_, err := HandleUpdateDisk(s.deps).Handle(context.Background(), overlayArgs(t, s.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
			if !s.hasEntry(777) {
				t.Fatalf("update_disk (err = %v) removed 777's allocation entry", err)
			}
			if after := pve.DescriptionFromConfig(s.client.state.configs[777]); after != before {
				t.Fatalf("update_disk (err = %v) changed 777's description:\nbefore %s\nafter  %s", err, before, after)
			}
			if w.removals != 0 {
				t.Fatalf("update_disk removed 777's entry %d times, want none", w.removals)
			}
		})
	}
}
