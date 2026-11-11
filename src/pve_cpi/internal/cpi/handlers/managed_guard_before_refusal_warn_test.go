package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// blockedWarnings returns the WARN lines in which the guard says it locked
// itself because its checks before a lifecycle mutation refused it.
func blockedWarnings(logged string) []string {
	var lines []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `"level":"WARN"`) &&
			strings.Contains(line, "requires reconciliation because the guard's checks before it refused it") {
			lines = append(lines, log.ScrubMessage(line))
		}
	}
	return lines
}

// TestManagedDiskBeforeRefusalWarnsWithTheReason runs the adopted mover's
// attach and moves the mover off the source out of band just before the
// migrate's admission, so the guard's read of the mover's config on the
// source answers that it is gone. That answer locks the guard before the
// migrate is sent. The guard logs one WARN with the reason, the call, the
// operation, and the allocation, in the shape of the WARN for an outcome it
// could not settle.
func TestManagedDiskBeforeRefusalWarnsWithTheReason(t *testing.T) {
	c := newAdoptedMoverCase(t)
	client, mover := c.client, c.mover
	moved := false
	client.onCertificateRead = func(string) error {
		if moved || len(client.protectionWrites) == 0 {
			return nil
		}
		last := client.protectionWrites[len(client.protectionWrites)-1]
		if last.vmid == mover && !last.on && last.node == "n2" {
			moved = true
			client.vmNodes[mover] = "n1"
		}
		return nil
	}

	_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx,
		[]json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{})
	if !moved {
		t.Fatalf("the attach never reached the migrate's admission; attach_disk = %v", attachErr)
	}
	if attachErr == nil {
		t.Fatal("attach_disk succeeded, want the locked guard's error")
	}
	if client.migrations != 0 {
		t.Fatalf("the attach sent %d migrations, want none", client.migrations)
	}
	lines := blockedWarnings(c.logged.String())
	if len(lines) != 1 {
		t.Fatalf("the guard logged %d WARNs that its checks refused a mutation, want one: %v", len(lines), lines)
	}
	for _, want := range []string{
		`"call":"Nodes.CreateQemuMigrate"`,
		`"operation":"attach_disk"`,
		`"allocation_id":"` + c.id + `"`,
		"cannot verify lifecycle holder before mutation",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the guard's WARN lacks %s: %s", want, lines[0])
		}
	}
}
