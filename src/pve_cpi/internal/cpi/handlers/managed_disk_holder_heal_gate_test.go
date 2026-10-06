package handlers

// These tests cover where the identity check writes the provenance entry that
// a holder is missing, and how attach_disk settles its own provenance write
// when PVE answers it with an error. Only a call that changes the disk writes
// the missing entry, from inside the disk's lifecycle under the allocation
// journal's lock, and only while every step of the disk's record is settled
// and no transfer of the disk is in flight. The lifecycle settles by readback
// the steps it can before the heal decides, so a step that a readback settles
// doesn't hold the disk back, and a step it can't settle is reported with what
// the operator does next. A provenance write PVE answered with an error is
// read back once, so a write that landed counts as written, and a write that
// changed nothing, or that PVE refused only because the digest moved, is sent
// again.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// holderWrites watches the description writes that reach VM 777. It counts
// every one, the attach's own provenance writes, the heal's writes, and the
// heal's writes made while a lifecycle holds the disk's allocation journal
// lock. It can also fail the attach's own provenance write with a PVE error.
type holderWrites struct {
	mu                    sync.Mutex
	descriptions          int
	provenance            int
	heals, healsUnderLock int
	// journal and id, when set, name the disk's allocation, so a heal's
	// write counts as under the lock only when the journal's lock for id
	// can't be taken in shared mode as the write arrives.
	journal *aj.Journal
	id      string
	// faults is how many of the attach's own provenance writes get an
	// error, and fired counts them. apply decides whether PVE applies such a
	// write before it answers, and change, when set, runs instead to change
	// 777 the way another writer would. The error is refuse when it is set
	// and a 500 that says "got timeout" otherwise.
	faults, fired int
	apply         bool
	change        func()
	refuse        error
	// beforeHeal, when set, runs as the heal's write arrives, before PVE
	// checks its digest.
	beforeHeal func()
}

func (w *holderWrites) counts() (descriptions, provenance, heals, healsUnderLock, fired int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.descriptions, w.provenance, w.heals, w.healsUnderLock, w.fired
}

// holderWritesPVE is the lock-contention flow cluster with holderWrites on
// its configuration writes.
type holderWritesPVE struct {
	contendedFlowPVE
	writes *holderWrites
}

func (c holderWritesPVE) Nodes() nodes.Service {
	return holderWritesNodes{Service: c.contendedFlowPVE.Nodes(), writes: c.writes}
}

type holderWritesNodes struct {
	nodes.Service
	writes *holderWrites
}

func (n holderWritesNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	if vmid != "777" || p == nil || p.Description == nil {
		return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
	}
	stack := string(debug.Stack())
	heal := strings.Contains(stack, "healUnrecordedHolder")
	own := !heal && strings.Contains(stack, "writeManagedDiskHolder")
	w := n.writes
	locked := heal && w.lifecycleHoldsLock()
	w.mu.Lock()
	w.descriptions++
	if heal {
		w.heals++
		if locked {
			w.healsUnderLock++
		}
	}
	if own {
		w.provenance++
	}
	fault := own && w.fired < w.faults
	if fault {
		w.fired++
	}
	beforeHeal, change, apply, refuse := w.beforeHeal, w.change, w.apply, w.refuse
	w.mu.Unlock()
	if heal && beforeHeal != nil {
		beforeHeal()
	}
	if !fault {
		return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
	}
	switch {
	case change != nil:
		change()
	case apply:
		if err := n.Service.UpdateQemuConfig(ctx, node, vmid, p); err != nil {
			return err
		}
	}
	if refuse != nil {
		return refuse
	}
	return &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
}

// lifecycleHoldsLock reports whether a lifecycle holds the allocation
// journal's lock for the watched disk now, which is when the journal refuses
// a shared hold that never waits.
func (w *holderWrites) lifecycleHoldsLock() bool {
	if w.journal == nil {
		return false
	}
	release, held, err := w.journal.TryHoldShared(w.id)
	if err != nil {
		return false
	}
	if held {
		_ = release()
		return false
	}
	return true
}

// digestRefusal is PVE's answer to a configuration write whose digest no
// longer matches the VM's configuration.
func digestRefusal() error {
	body, _ := json.Marshal(map[string]string{"message": "checksum mismatch (file change by other user?)\n"})
	return sdkerrors.ParseAPIError(500, body)
}

// watchedParkedDisk returns a parked disk whose cluster reports to writes.
func watchedParkedDisk(t *testing.T, writes *holderWrites) *parkedFlowDisk {
	t.Helper()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	disk.deps.PVE = holderWritesPVE{contendedFlowPVE: disk.deps.PVE.(contendedFlowPVE), writes: writes}
	return disk
}

// unrecordedHolderDisk returns a parked disk that an attach moved onto VM 777
// and then left without 777's provenance entry, because every try at that
// write failed before it was sent. Its cluster reports to the returned
// holderWrites from then on.
func unrecordedHolderDisk(t *testing.T) (*parkedFlowDisk, *holderWrites) {
	t.Helper()
	locks := newLockContention(t)
	disk := newParkedFlowDisk(t, locks)
	locks.reset()
	flow := disk.deps.PVE.(contendedFlowPVE)
	fault := &provenanceWriteFault{limit: -1, fail: func(context.Context) error { return errAdmissionReadRefused }}
	withProvenanceFault(disk, fault)
	if err := disk.attach(noBackoff(t.Context())); err == nil {
		t.Fatal("attach_disk succeeded although no provenance write was admitted")
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although no write was sent: %v", held)
	}
	writes := &holderWrites{journal: disk.journal, id: disk.id}
	disk.deps.PVE = holderWritesPVE{contendedFlowPVE: flow, writes: writes}
	return disk, writes
}

// requireNotRecorded fails unless err is the retriable refusal for a holder
// that lacks the disk's entry.
func requireNotRecorded(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted the disk although 777 lacks its provenance entry", what)
	}
	if !strings.Contains(err.Error(), "without its provenance entry") || strings.Contains(err.Error(), "audit required") {
		t.Fatalf("%s err = %v, want the retriable refusal for a missing provenance entry", what, err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("%s err = %v, want it retriable", what, err)
	}
}

// TestUnrecordedHolderReadsWriteNothing runs has_disk and a plain resolution
// of the disk, the kind create_vm's placement planning makes, against a
// holder that lacks the disk's entry. Neither writes to the holder, and each
// gets the retriable refusal.
func TestUnrecordedHolderReadsWriteNothing(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)

	out, err := HandleHasDisk(disk.deps).Handle(t.Context(), []json.RawMessage{json.RawMessage(fmt.Sprintf("%q", disk.cid))}, jsonrpc.Context{})
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("has_disk wrote 777's notes %d times, want never", descriptions)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("has_disk wrote 777's provenance for the disk: %v", held)
	}
	requireNotRecorded(t, fmt.Sprintf("has_disk (answer %v)", out), err)

	bare, meta, err := pve.ParseEncodedDiskCID(disk.cid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveDiskForOp(t.Context(), disk.deps, "create_vm", disk.cid, bare, meta)
	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("a plain resolution wrote 777's notes %d times, want never", descriptions)
	}
	requireNotRecorded(t, "a plain resolution", err)
}

// TestManagedAttachHealsTheHolderUnderItsAllocationLock retries the attach
// on a holder that lacks the disk's entry. The entry is written once, from
// the lifecycle's own resolution under the allocation journal's lock, and
// the attach completes.
func TestManagedAttachHealsTheHolderUnderItsAllocationLock(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the retried attach failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "retried", disk.record(t))
}

// TestManagedAttachHealRefusedByTheDigestPin changes 777 just before the
// heal's write reaches PVE. The write carries the digest of the configuration
// the heal checked, so PVE refuses it, the attach fails retriably without an
// audit, and 777 gains no entry. The next attach writes it. Both heal writes
// arrive while the allocation journal's lock is held, which the fake checks
// by trying the lock itself.
func TestManagedAttachHealRefusedByTheDigestPin(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	writes.beforeHeal = func() { disk.client.state.configs[777]["digest"] = "changed-by-another-writer" }

	err := disk.attach(t.Context())
	if err == nil {
		t.Fatal("the attach succeeded although PVE refused the heal's write")
	}
	if !strings.Contains(err.Error(), "writing the entry failed") || strings.Contains(err.Error(), "audit required") || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want the retriable refusal for a heal write that failed", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal sent %d writes, %d of them under the allocation lock, want one under it", heals, underLock)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although PVE refused the write: %v", held)
	}

	writes.beforeHeal = nil
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the attach after the refused heal failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 2 || underLock != 2 {
		t.Fatalf("the heal sent %d writes, %d of them under the allocation lock, want two under it", heals, underLock)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "healed", disk.record(t))
}

// TestHealUnrecordedHolderWaitsForASettledRecord hands the heal a disk whose
// every proof holds, first with a step of its record left planned and then
// with a transfer to a parker in flight. Each time the heal writes nothing
// and gives the retriable refusal. With neither, it writes the provenance
// entry.
func TestHealUnrecordedHolderWaitsForASettledRecord(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	ctx := t.Context()
	bare, meta, err := pve.ParseEncodedDiskCID(disk.cid)
	if err != nil {
		t.Fatal(err)
	}
	cfg := maps.Clone(disk.client.state.configs[777])
	volid := ""
	for _, value := range qemu.ParseDisks(cfg) {
		if serial, ok := pve.StableIDFromDriveOptStr(value); ok && serial == meta.ID {
			volid = strings.Split(value, ",")[0]
		}
	}
	if volid == "" {
		t.Fatalf("777 carries no slot with the disk's serial: %v", cfg)
	}
	storage, _, err := pve.ParseDiskCID(volid)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := managedDiskActualDefinition(ctx, disk.deps, storage)
	if err != nil {
		t.Fatal(err)
	}
	record := disk.record(t)
	rd := resolvedDisk{diskCID: disk.cid, birth: bare, volid: volid, stableID: meta.ID, meta: meta, holder: &pve.DiskHolder{Found: true, VMID: 777, Node: "n1"}}
	provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: record.ID, AllocationNamespace: record.Namespace, Node: "n1", Volid: volid, Backing: definition.BackingKey()}

	planned := record
	planned.Steps = append(append([]aj.Step(nil), record.Steps...), aj.Step{ID: "attempt-0-step-99", Attempt: record.ActiveAttempt(), Kind: "lifecycle_attach_disk_Nodes_UpdateQemuConfig", State: aj.Planned, Target: aj.Target{Node: "n1", VMID: 777}})
	err = healUnrecordedHolder(ctx, disk.deps, rd, planned, definition.IsShared(), provenance, cfg)
	requireNotRecorded(t, "the heal with a planned step", err)

	moving := rd
	moving.intent = &pve.DiskTransferIntent{AllocationID: record.ID, AllocationNamespace: record.Namespace, ParkerVMID: disk.parker, ParkerNode: "n1", Volid: volid}
	err = healUnrecordedHolder(ctx, disk.deps, moving, record, definition.IsShared(), provenance, cfg)
	requireNotRecorded(t, "the heal with a transfer in flight", err)

	if descriptions, _, _, _, _ := writes.counts(); descriptions != 0 {
		t.Fatalf("the heal wrote 777's notes %d times while the record was unsettled, want never", descriptions)
	}

	if err := healUnrecordedHolder(ctx, disk.deps, rd, record, definition.IsShared(), provenance, cfg); err != nil {
		t.Fatalf("the heal refused a settled record: %v", err)
	}
	if held := holderProvenanceEntries(t, disk); len(held) != 1 || held[0].Volid != volid || held[0].AllocationID != record.ID {
		t.Fatalf("777 records the disk as %v after the heal, want %s once", held, volid)
	}
}

// TestManagedAttachProvenanceWriteLandedDespiteAnError has PVE apply the
// holder's provenance write and answer it with a 500. The readback finds the
// entry, so the attach counts it as written, sends it only once, and returns
// the allocation with every step settled.
func TestManagedAttachProvenanceWriteLandedDespiteAnError(t *testing.T) {
	t.Parallel()
	writes := &holderWrites{faults: 1, apply: true}
	disk := watchedParkedDisk(t, writes)

	if err := disk.attach(noBackoff(t.Context())); err != nil {
		t.Fatalf("attach_disk failed although its provenance write landed: %v", err)
	}
	if _, provenance, _, _, fired := writes.counts(); fired != 1 || provenance != 1 {
		t.Fatalf("the provenance write was sent %d times with %d errors, want once with one", provenance, fired)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "attached", disk.record(t))
}

// TestManagedAttachProvenanceWriteNotLandedIsSentAgain has PVE answer the
// holder's provenance write with a 500 without applying it. The readback
// finds 777 unchanged, so the attach sends the write again in the same call,
// and the second write lands.
func TestManagedAttachProvenanceWriteNotLandedIsSentAgain(t *testing.T) {
	t.Parallel()
	writes := &holderWrites{faults: 1}
	disk := watchedParkedDisk(t, writes)

	if err := disk.attach(noBackoff(t.Context())); err != nil {
		t.Fatalf("attach_disk failed although its provenance write could be sent again: %v", err)
	}
	if _, provenance, _, _, fired := writes.counts(); fired != 1 || provenance != 2 {
		t.Fatalf("the provenance write was sent %d times with %d errors, want twice with one", provenance, fired)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "attached", disk.record(t))
}

// TestManagedAttachProvenanceWriteOnAChangedHolderNeedsAnAudit has PVE answer
// the holder's provenance write with an error while another writer changes
// 777, or leaves it alone. When 777 is unchanged, the readback proves PVE
// wrote nothing, so the attach sends the write again and completes. When the
// other writer changed 777, whether PVE answered with a 500 or refused the
// pinned digest, the attach can't tell whether its write landed or what the
// other writer did. It keeps the audit refusal, says what the readback found
// and what the operator does next, and leaves the allocation for
// reconciliation.
func TestManagedAttachProvenanceWriteOnAChangedHolderNeedsAnAudit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		refuse error
		change func(cfg map[string]any)
		answer string
		audit  bool
	}{
		{name: "a 500 on an unchanged holder is sent again", answer: "got timeout"},
		{
			name:   "a 500 while another writer changes the holder",
			change: func(cfg map[string]any) { cfg["digest"] = "changed-by-another-writer" },
			answer: "got timeout",
			audit:  true,
		},
		{
			name:   "a digest refusal after another writer changes a setting",
			refuse: digestRefusal(),
			change: func(cfg map[string]any) {
				cfg["digest"] = "changed-by-another-writer"
				cfg["memory"] = "8192"
			},
			answer: "checksum mismatch",
			audit:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writes := &holderWrites{faults: 1, refuse: tc.refuse}
			disk := watchedParkedDisk(t, writes)
			if tc.change != nil {
				writes.change = func() { tc.change(disk.client.state.configs[777]) }
			}

			err := disk.attach(noBackoff(t.Context()))
			_, provenance, _, _, fired := writes.counts()
			if fired != 1 {
				t.Fatalf("the provenance write got %d errors, want one", fired)
			}
			if !tc.audit {
				if err != nil {
					t.Fatalf("attach_disk failed although 777 was unchanged and the write could be sent again: %v", err)
				}
				if provenance != 2 {
					t.Fatalf("the provenance write was sent %d times, want twice", provenance)
				}
				requireHolderProvenance(t, disk)
				assertReturnedRecord(t, "attached", disk.record(t))
				return
			}
			if err == nil {
				t.Fatal("attach_disk succeeded although 777 changed under its provenance write")
			}
			for _, want := range []string{"audit required", tc.answer, "changed since the write was pinned", "changed-by-another-writer", "qm config 777", "Multi-storage allocation requires reconciliation"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to say %q", err, want)
				}
			}
			if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, want it not retriable", err)
			}
			if provenance != 1 {
				t.Fatalf("the provenance write was sent %d times, want once", provenance)
			}
			if held := holderProvenanceEntries(t, disk); len(held) != 0 {
				t.Fatalf("777 records the disk although the write did not land: %v", held)
			}
			if record := disk.record(t); record.State != aj.ReconciliationRequired {
				t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
			}
		})
	}
}

// TestManagedAttachProvenanceWriteRefusedForAMovedDigestIsSentAgain has PVE
// refuse the holder's provenance write because 777's digest moved, as the
// removal of an older snapshot can move it, while every setting of 777 stays
// the same. PVE's refusal means it wrote nothing, and the readback shows no
// other change, so the attach sends the write again in the same call, and the
// second write lands.
func TestManagedAttachProvenanceWriteRefusedForAMovedDigestIsSentAgain(t *testing.T) {
	t.Parallel()
	writes := &holderWrites{faults: 1, refuse: digestRefusal()}
	disk := watchedParkedDisk(t, writes)
	writes.change = func() { disk.client.state.configs[777]["digest"] = "moved-without-a-change" }

	if err := disk.attach(noBackoff(t.Context())); err != nil {
		t.Fatalf("attach_disk failed although PVE wrote nothing and 777 kept every setting: %v", err)
	}
	if _, provenance, _, _, fired := writes.counts(); fired != 1 || provenance != 2 {
		t.Fatalf("the provenance write was sent %d times with %d refusals, want twice with one", provenance, fired)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "attached", disk.record(t))
}

// cutOffHolderPVE is the cluster that can drop the parker's protection
// restore, with a storage listing that provenanceWriteFault can fail, so the
// holder's provenance write never gets sent.
type cutOffHolderPVE struct {
	*hungRestorePVE
	fault *provenanceWriteFault
}

func (c cutOffHolderPVE) ClusterStorage() clusterstorage.Service {
	return provenanceFaultStorage{Service: c.hungRestorePVE.ClusterStorage(), fault: c.fault}
}

// unsettledSteps lists the steps of the disk's record that aren't observed.
func unsettledSteps(t *testing.T, disk *parkedFlowDisk) []string {
	t.Helper()
	var out []string
	steps := disk.record(t).Steps
	for i := range steps {
		step := &steps[i]
		if step.State != aj.Observed {
			out = append(out, fmt.Sprintf("%s (%s) %s", step.ID, step.Kind, step.State))
		}
	}
	return out
}

// TestManagedAttachSettlesACutOffRestoreBeforeHealingTheHolder moves a parked
// disk onto VM 777 while every write that puts the parker's protection back
// fails in transport and every read before the holder's provenance write
// fails. The disk lands on 777 with a planned protection step in its record
// and no entry on 777. Once the outage ends, the retried attach settles the
// step by readback first, finds protection still off on the parker, and
// refuses with the qm set command that fixes it, writing nothing to 777. After
// the operator restores protection, the next attach settles the step, writes
// the missing entry, and completes.
func TestManagedAttachSettlesACutOffRestoreBeforeHealingTheHolder(t *testing.T) {
	t.Parallel()
	c := newParkedRestoreDisk(t, 5*time.Second)
	c.ctx = pve.WithTestBackoff(c.ctx, func(int) time.Duration { return 0 })
	c.hung.mu.Lock()
	c.hung.dropped = 1000
	c.hung.mu.Unlock()
	fault := &provenanceWriteFault{limit: -1, fail: func(context.Context) error { return errAdmissionReadRefused }}
	c.deps.PVE = cutOffHolderPVE{hungRestorePVE: c.hung, fault: fault}
	disk := &parkedFlowDisk{deps: c.deps, client: c.client, journal: c.journal, id: c.id, cid: c.cid, parker: c.parker}
	attach := func() error {
		_, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
		return err
	}

	if err := attach(); err == nil {
		t.Fatal("attach_disk succeeded although the restore and the provenance write both failed")
	}
	c.restoreStep(t)
	if held := holderProvenanceEntries(t, disk); len(held) != 0 {
		t.Fatalf("777 records the disk although no provenance write was sent: %v", held)
	}

	fault.clear()
	c.hung.mu.Lock()
	c.hung.dropped = 0
	c.hung.mu.Unlock()
	fix := fmt.Sprintf("qm set %d --protection 1", c.parker)
	for range 2 {
		err := attach()
		if err == nil || !strings.Contains(err.Error(), fix) || strings.Contains(err.Error(), "without its provenance entry") {
			t.Fatalf("retried attach err = %v, want the refusal that names %q", err, fix)
		}
		if held := holderProvenanceEntries(t, disk); len(held) != 0 {
			t.Fatalf("777 records the disk although protection is still off on the parker: %v", held)
		}
	}

	c.setProtection(true)
	if err := attach(); err != nil {
		t.Fatalf("attach_disk failed after the operator restored protection: %v", err)
	}
	if steps := unsettledSteps(t, disk); len(steps) != 0 {
		t.Fatalf("the record keeps unsettled steps %v after the attach", steps)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "healed", disk.record(t))
}

// plantPlannedRestore turns the last observed step that put the parker's
// protection back on into a planned step, the way a restore whose answer was
// lost leaves it, and marks the record for reconciliation as that loss does.
func plantPlannedRestore(t *testing.T, disk *parkedFlowDisk) {
	t.Helper()
	record := disk.record(t)
	planted := -1
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed || step.Target.VMID != disk.parker || !isParkerProtectionParameters(step.Parameters) {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(step.Parameters, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["kind"] == parkerProtectionOnKind {
			planted = i
		}
	}
	if planted < 0 {
		t.Fatalf("the record has no observed protection restore on parker %d: %+v", disk.parker, record.Steps)
	}
	record.Steps[planted].State = aj.Planned
	record.Steps[planted].VolIDs = nil
	if record.State != aj.ReconciliationRequired {
		record.State = aj.ReconciliationRequired
		record.Reason = "an answer from PVE was lost"
	}
	rewriteJournalRecord(t, disk.deps, disk.id, record)
}

// TestManagedAttachSettlesAPlannedRestoreBeforeHealingTheHolder retries the
// attach on a holder that lacks the disk's entry, with the parker's
// protection restore left planned although protection reads back on. The
// lifecycle settles the step by readback, the heal then writes the entry once
// under the allocation lock, and the attach completes with every step
// settled.
func TestManagedAttachSettlesAPlannedRestoreBeforeHealingTheHolder(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	plantPlannedRestore(t, disk)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the retried attach failed: %v", err)
	}
	if _, _, heals, underLock, _ := writes.counts(); heals != 1 || underLock != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, %d of them under the allocation lock, want once under it", heals, underLock)
	}
	if steps := unsettledSteps(t, disk); len(steps) != 0 {
		t.Fatalf("the record keeps unsettled steps %v after the attach", steps)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "healed", disk.record(t))
}

// TestManagedAttachSettlesAPlannedRestoreWithTheEntryPresent is the control
// for the test above. The holder already carries the disk's entry when the
// protection restore is left planned, so the attach settles the step and
// completes without a heal.
func TestManagedAttachSettlesAPlannedRestoreWithTheEntryPresent(t *testing.T) {
	t.Parallel()
	disk, writes := unrecordedHolderDisk(t)
	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the attach that heals the holder failed: %v", err)
	}
	plantPlannedRestore(t, disk)

	if err := disk.attach(t.Context()); err != nil {
		t.Fatalf("the attach with a planned restore failed: %v", err)
	}
	if _, _, heals, _, _ := writes.counts(); heals != 1 {
		t.Fatalf("the heal wrote 777's entry %d times, want only the first attach's write", heals)
	}
	if steps := unsettledSteps(t, disk); len(steps) != 0 {
		t.Fatalf("the record keeps unsettled steps %v after the attach", steps)
	}
	requireHolderProvenance(t, disk)
	assertReturnedRecord(t, "settled", disk.record(t))
}
