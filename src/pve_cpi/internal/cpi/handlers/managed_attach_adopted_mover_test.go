package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// newAdoptedMoverDisk is newParkedRestoreDisk with the parker turned into a
// mover that an earlier request created. A mover is never a park target, so
// the tag goes on after the disk is parked. The tests that use it mean nothing
// unless the attach takes the mover path, so it fails the test unless the
// holder carries the mover tag and the plan transfers the disk off the mover
// with destroyMover set. Both checks only read, which the fixture's write
// generation confirms. It returns the disk and the buffer the request logs to,
// with the parker's recorded writes cleared.
func newAdoptedMoverDisk(t *testing.T, timeout time.Duration) (*cutOffRestore, *bytes.Buffer) {
	t.Helper()
	c := newParkedRestoreDisk(t, timeout)
	logged := &bytes.Buffer{}
	logger, err := log.NewLogger("debug", logged)
	if err != nil {
		t.Fatal(err)
	}
	c.deps.Logger = logger
	c.client.state.configs[c.parker]["tags"] = "bosh-parker;" + pve.DiskMoverTag

	generation := c.client.generation
	bare, meta, err := decodeDiskCID(c.ctx, c.deps, "attach_disk", c.cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(c.ctx, c.deps, "attach_disk", c.cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rd.holder == nil || rd.holder.VMID != c.parker || !rd.holder.IsParker {
		t.Fatalf("the disk's holder is %+v, want parker %d", rd.holder, c.parker)
	}
	if !pve.TagsMarkDiskMover(rd.holder.Tags) {
		t.Fatalf("the holder's tags %q do not carry the mover tag; the attach would not take the mover path", rd.holder.Tags)
	}
	plan, err := guardAndUnparkBeforeAttach(c.ctx, c.deps, "attach_disk", &rd, "n1", 777)
	if err != nil {
		t.Fatalf("plan the attach: %v", err)
	}
	if !plan.viaTransfer || plan.parker.VMID != c.parker {
		t.Fatalf("the plan does not transfer the disk off mover %d: %+v", c.parker, plan)
	}
	if !plan.destroyMover {
		t.Fatalf("the plan does not set destroyMover for mover %d; the attach would not reach the mover's destroy", c.parker)
	}
	if c.client.generation != generation {
		t.Fatal("planning the attach wrote to PVE")
	}
	c.hung.forgetParkerWrites()
	return c, logged
}

// logMoverLines repeats, scrubbed, every logged line that mentions a mover,
// so a failing run shows what the attach decided about it.
func logMoverLines(t *testing.T, logged *bytes.Buffer) {
	t.Helper()
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "mover") {
			t.Logf("log: %s", log.ScrubMessage(line))
		}
	}
}

// TestManagedAttachKeepsAMoverAnEarlierRequestCreated attaches a
// journal-managed disk that sits on a mover already on the VM's node, left
// there by an earlier request. This request did not create the mover, so the
// allocation guard will not delete it. The attach has to land and return the
// disk with every step observed, and nothing it sends may reach the mover
// after the transfer has put the mover's protection back: no protection-off
// write and no delete.
func TestManagedAttachKeepsAMoverAnEarlierRequestCreated(t *testing.T) {
	t.Parallel()
	c, logged := newAdoptedMoverDisk(t, 5*time.Second)

	moves := c.client.moves
	_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	if c.client.moves != moves+1 {
		t.Fatalf("the attach made %d moves, want the one transfer off the mover", c.client.moves-moves)
	}
	writes := c.hung.landedProtectionWrites()
	t.Logf("protection writes that landed on mover %d, oldest first: %v", c.parker, writes)
	logMoverLines(t, logged)

	if attachErr != nil {
		t.Errorf("attach_disk failed after the disk landed: %s", log.ScrubMessage(attachErr.Error()))
	}
	record := c.record(t)
	if record.State != aj.ReadyToReturn {
		t.Errorf("allocation state = %s (reason %q), want %s", record.State, record.Reason, aj.ReadyToReturn)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.State != aj.Observed {
			t.Errorf("step %s (%s, vmid %d) is %s", step.ID, step.Kind, step.Target.VMID, step.State)
		}
	}

	// The transfer opens the mover's protection window and closes it. Any
	// protection-off write after that close is the destroy's.
	restored := -1
	for i, on := range writes {
		if on {
			restored = i
			break
		}
	}
	if restored < 0 {
		t.Errorf("the transfer never put protection back on mover %d", c.parker)
	}
	for _, on := range writes[restored+1:] {
		if !on {
			t.Errorf("a protection-off write reached mover %d after the transfer restored its protection", c.parker)
		}
	}
	if n := c.hung.deletesOfParker(); n != 0 {
		t.Errorf("%d deletes reached mover %d", n, c.parker)
	}
	kept, ok := c.client.state.configs[c.parker]
	if !ok {
		t.Fatalf("mover %d is gone", c.parker)
	}
	if v, _ := pve.ConfigString(kept, "protection"); v != "1" {
		t.Errorf("mover %d protection = %q, want it left on", c.parker, v)
	}
	if lifecycleConfigHasAnyVolume(kept) {
		t.Errorf("mover %d still holds a volume: %v", c.parker, kept)
	}
	slot, _ := pve.ConfigString(c.client.state.configs[777], "scsi1")
	if !strings.Contains(slot, ":777/vm-777-disk-") {
		t.Errorf("VM 777 scsi1 = %q, want the transferred disk", slot)
	}
	if !strings.Contains(logged.String(), "an earlier request created it") {
		t.Errorf("the attach logged no warning saying why it kept mover %d", c.parker)
	}
	if want := fmt.Sprintf("once this call has gone through and the mover holds no disks, run qm set %d --protection 0 and then qm destroy %d", c.parker, c.parker); !strings.Contains(logged.String(), want) {
		t.Errorf("the keep warning does not say to remove mover %d once this call has gone through", c.parker)
	}
}

// TestManagedAttachDestroysTheMoverItCreated is the journal-path cross-node
// attach whose request creates a fresh mover under its own guard. The guard
// created that mover, so it admits the delete, and the attach destroys the
// mover after the disk lands, exactly as it did before movers an earlier
// request created were kept. The attach succeeds, the record holds no planned
// or uncertain step, and the journal shows the mover's create and its delete
// on the same VMID.
func TestManagedAttachDestroysTheMoverItCreated(t *testing.T) {
	t.Parallel()
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Config.DiskMigration = "on_attach"
	ctx := pve.WithTestBackoff(t.Context(), func(int) time.Duration { return 0 })
	if _, err := HandleDetachDisk(deps).Handle(ctx, []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("setup detach into the parker: %v", err)
	}
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[888] = "n2"
	client.state.configs[888] = map[string]any{"name": "target", "digest": "1"}

	if _, err := HandleAttachDisk(deps).Handle(ctx, []json.RawMessage{planJSON(t, "888"), planJSON(t, cid)}, jsonrpc.Context{}); err != nil {
		t.Fatalf("attach_disk across nodes: %s", log.ScrubMessage(err.Error()))
	}
	if client.migrations != 1 {
		t.Fatalf("migrations = %d, want the one migration of a fresh mover", client.migrations)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	assertReturnedRecord(t, "cross-node attach", record)

	created := map[int]bool{}
	destroyed := 0
	for i := range record.Steps {
		step := &record.Steps[i]
		switch step.Kind {
		case "lifecycle_attach_disk_QEMU_Create":
			created[step.Target.VMID] = true
		case "lifecycle_attach_disk_Nodes_DeleteQemu":
			if !created[step.Target.VMID] {
				t.Errorf("the attach deleted VM %d, which it did not create", step.Target.VMID)
			}
			destroyed = step.Target.VMID
		}
	}
	if destroyed == 0 {
		t.Fatalf("the attach recorded no delete of the mover it created; its creates were %v", created)
	}
	if cfg, ok := client.state.configs[destroyed]; ok {
		t.Errorf("mover %d still exists after the attach: %v", destroyed, cfg)
	}
	for vmid, cfg := range client.state.configs {
		if tags, _ := pve.ConfigString(cfg, "tags"); pve.TagsMarkDiskMover(tags) {
			t.Errorf("VM %d still carries the mover tag after the attach", vmid)
		}
	}
}
