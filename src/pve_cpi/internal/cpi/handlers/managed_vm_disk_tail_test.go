package handlers

// These rows run create_vm through its real entry with a parked disk in
// disk_cids whose parker entry names 777 as the VM it left, and 777 still
// carries the disk's allocation entry. The pre-attach runs the detach tail on
// 777 before it moves the disk off the parker.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// tailCreateVMClient is the create_vm disk client with 777's reads and
// description writes under the test's control.
type tailCreateVMClient struct {
	*createVMDiskClient
	journal *aj.Journal
	// readErr, when set, is the answer to the first read of 777's pending
	// view after each admission of the disk's attach, which is the tail's.
	// create_vm's own audit reads and the completion audit's reads of 777
	// are served as usual.
	readErr error
	// failedReads counts the reads of 777 that got readErr, and failedFor
	// holds the admissions they failed for.
	failedReads int
	failedFor   map[string]bool
	// writeErr, when set, is the answer to every description write to 777.
	writeErr error
	// writes counts the description writes 777 gets.
	writes int
}

func (c *tailCreateVMClient) Nodes() nodes.Service {
	return tailCreateVMNodes{Service: c.createVMDiskClient.Nodes(), c: c}
}

type tailCreateVMNodes struct {
	nodes.Service
	c *tailCreateVMClient
}

func (n tailCreateVMNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	if admission := n.c.attachAdmission(); vmid == "777" && n.c.readErr != nil && admission != "" && !n.c.failedFor[admission] {
		n.c.failedFor[admission] = true
		n.c.failedReads++
		return nil, n.c.readErr
	}
	return n.Service.ListQemuPending(ctx, node, vmid)
}

// attachAdmission names the admission of the disk's attach in flight, by the
// time its record was saved, or returns "" when no attach is in flight.
func (c *tailCreateVMClient) attachAdmission() string {
	records, err := c.journal.List()
	if err != nil {
		return ""
	}
	for i := range records {
		if records[i].Kind == "disk" && records[i].Reason == "lifecycle attach_disk admitted; completion pending" {
			return records[i].UpdatedAt.String()
		}
	}
	return ""
}

func (n tailCreateVMNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	if vmid == "777" && p.Description != nil {
		n.c.writes++
		if n.c.writeErr != nil {
			return n.c.writeErr
		}
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
}

// createVMTailFixture is createVMDiskFixture with the disk's parker entry
// naming 777 as its source and 777 carrying the allocation entry the parker
// carries, which is the state an earlier release leaves after a park.
func createVMTailFixture(t *testing.T) (Deps, *tailCreateVMClient, *aj.Journal, string, int) {
	t.Helper()
	deps, inner, journal, cid, parker := createVMDiskFixture(t, newLockContention(t), false)
	cfg := inner.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var parked map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &parked); err != nil || len(parked) != 1 {
		t.Fatalf("parker %d's parked entries = %s (%v), want one", parker, raw["bosh_parked_disks"], err)
	}
	for key := range parked {
		parked[key]["source_vm_cid"] = "777"
	}
	encoded, err := json.Marshal(parked)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	bare, meta, err := decodeDiskCID(context.Background(), deps, "create_vm", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(context.Background(), deps, "create_vm", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.allocation == nil || rd.holder == nil || rd.holder.VMID != parker {
		t.Fatalf("the disk resolved to %+v, want a managed disk on parker %d", rd.holder, parker)
	}
	inner.state.configs[777] = map[string]any{"name": "w777", "digest": "1"}
	client := &tailCreateVMClient{createVMDiskClient: inner, journal: journal, failedFor: map[string]bool{}}
	deps.PVE = client
	if err := pve.WriteDiskAllocationProvenance(context.Background(), client, "n1", 777, rd.sentinelKey(), rd.allocation.provenance); err != nil {
		t.Fatalf("seed 777's allocation entry: %v", err)
	}
	client.writes = 0
	return deps, client, journal, cid, parker
}

// parkerDiskSlots counts the disk slots parker holds.
func parkerDiskSlots(client *tailCreateVMClient, parker int) int {
	count := 0
	for key := range client.state.configs[parker] {
		if isDiskOptionKey(key) {
			count++
		}
	}
	return count
}

// has777Entry reports whether 777 still carries an allocation entry.
func has777Entry(t *testing.T, client *tailCreateVMClient) bool {
	t.Helper()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(client.state.configs[777]))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries) > 0
}

// TestCreateVMTailReadFailureReturnsTheDisk is a create_vm whose pre-attach
// can't read 777. The tail sent nothing, so the attach returns the disk on its
// parker and create_vm rolls the attempt back and fails retriably, with no VM
// generation left for reconciliation. Once 777 reads again, the retry removes
// 777's entry and attaches the disk.
func TestCreateVMTailReadFailureReturnsTheDisk(t *testing.T) {
	t.Parallel()
	deps, client, journal, cid, parker := createVMTailFixture(t)
	slots := parkerDiskSlots(client, parker)
	client.readErr = errors.New("connection reset reading 777")
	args := createVMArgs(t, cid)

	_, err := createVM(t.Context(), deps, args)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error, got %v", err)
	}
	if client.failedReads == 0 || client.writes != 0 {
		t.Fatalf("the tail's reads of 777 failed %d times and 777 got %d description writes, want failed reads and no writes", client.failedReads, client.writes)
	}
	if client.creates == 0 || client.destroys != client.creates {
		t.Fatalf("the failed attempts were not rolled back: creates=%d destroys=%d", client.creates, client.destroys)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if records[i].Kind == "vm" && records[i].State == aj.ReconciliationRequired {
			t.Fatalf("a VM generation was left for reconciliation: %s", records[i].Reason)
		}
	}
	assertCreateVMDiskReturned(t, journal)
	if after := parkerDiskSlots(client, parker); after != slots {
		t.Fatalf("parker %d holds %d disk slots, want the %d it held before", parker, after, slots)
	}
	if !has777Entry(t, client) {
		t.Fatal("777's entry went missing although the tail could not read 777")
	}

	client.readErr = nil
	if _, err := createVM(t.Context(), deps, args); err != nil {
		t.Fatalf("the Director's create_vm retry: %v", err)
	}
	if has777Entry(t, client) {
		t.Fatal("777 still carries the disk's allocation entry after the retry")
	}
	assertCreateVMDiskRecordsSettled(t, journal)
}

// TestCreateVMTailUnansweredWriteStillPoisons is a create_vm whose pre-attach
// sends the removal to 777 and gets no answer. The write may have landed, so
// the disk's record is left for reconciliation and the error isn't retriable.
func TestCreateVMTailUnansweredWriteStillPoisons(t *testing.T) {
	t.Parallel()
	deps, client, journal, cid, _ := createVMTailFixture(t)
	client.writeErr = errors.New("connection reset by peer")

	_, err := createVM(t.Context(), deps, createVMArgs(t, cid))
	if err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a non-retriable error after an unanswered write, got %v", err)
	}
	if client.writes == 0 {
		t.Fatal("the tail never sent its write to 777")
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	disks := 0
	for i := range records {
		if records[i].Kind != "disk" {
			continue
		}
		disks++
		if records[i].State != aj.ReconciliationRequired {
			t.Fatalf("the disk allocation is %s after an unanswered write, want %s", records[i].State, aj.ReconciliationRequired)
		}
	}
	if disks != 1 {
		t.Fatalf("found %d disk records, want 1", disks)
	}
}
