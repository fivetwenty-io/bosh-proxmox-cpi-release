package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestManagedDiskMoverRestoreFallsBackToTheTargetUnderTheGuard runs the
// adopted mover's attach under the journal, with a migrate that the guard
// refuses before sending while the mover lands on the target anyway, and a
// config probe that fails on the target, so no read places the mover. The
// restore then tries the source and the target in turn. The guard reads the
// mover's config on the source first, and that read answers that the config
// is gone. The guard must neither send that write nor record a planned step
// for it, and the restore must move on and put protection back on the target.
func TestManagedDiskMoverRestoreFallsBackToTheTargetUnderTheGuard(t *testing.T) {
	c := newAdoptedMoverCase(t)
	client, mover := c.client, c.mover
	forbidden := &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"}
	landed := false
	client.onCertificateRead = func(string) error {
		if landed || len(client.protectionWrites) == 0 {
			return nil
		}
		last := client.protectionWrites[len(client.protectionWrites)-1]
		if last.vmid != mover || last.on || last.node != "n2" {
			return nil
		}
		// The protection-off write has landed on n2, so this read is the
		// migrate's admission. The mover reaches n1 without the migrate,
		// and the admission read fails, so the guard refuses the migrate
		// unsent and stays usable.
		landed = true
		client.vmNodes[mover] = "n1"
		return forbidden
	}
	// The first read of the mover on n1 after it lands there is the config
	// probe's, and it fails, so the probe can't place the mover either.
	probeFailed := false
	client.onNodeConfigRead = func(node string, vmid int) error {
		if landed && !probeFailed && node == "n1" && vmid == mover {
			probeFailed = true
			return forbidden
		}
		return nil
	}

	_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx,
		[]json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{})
	logMoverLines(t, c.logged)
	if !landed || !probeFailed {
		t.Fatalf("the attach never reached the migrate's admission and the probe; attach_disk = %v", attachErr)
	}
	if attachErr == nil {
		t.Fatalf("attach_disk succeeded, want the refused migrate's error")
	}
	if client.migrations != 0 {
		t.Fatalf("the attach sent %d migrations, want none", client.migrations)
	}

	var restores []lifecycleProtectionWrite
	for _, write := range client.protectionWrites {
		if write.vmid == mover && write.on {
			restores = append(restores, write)
		}
	}
	if len(restores) != 1 || restores[0].node != "n1" {
		t.Fatalf("protection restores on mover %d = %+v, want one on n1", mover, restores)
	}
	if client.vmNode(mover) != "n1" || !pve.ConfigFlag(client.state.configs[mover], "protection") {
		t.Fatalf("mover %d is on %s with protection %v, want n1 with protection on", mover,
			client.vmNode(mover), client.state.configs[mover]["protection"])
	}

	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range record.Steps {
		if step.Target.Node == "n2" && step.Target.VMID == mover && step.State == aj.Planned {
			t.Errorf("the journal kept a planned step for mover %d on n2, where it has no config: %+v", mover, step)
		}
	}
	for _, line := range strings.Split(c.logged.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "protection restore on mover") {
			t.Errorf("the restore warned that it did not succeed: %s", log.ScrubMessage(line))
		}
	}
}
