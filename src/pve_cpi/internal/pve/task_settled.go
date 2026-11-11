package pve

import (
	"context"
	"fmt"
	"strconv"

	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ConfirmGuestTaskOK reads task upid back from node once and returns nil only
// when the UPID names a task of type kind for VM vmid that ran on node, and
// PVE reports it stopped with exit status exactly OK and no warnings. It
// doesn't wait for the task. A caller uses it after AwaitTask, which also
// accepts a task that ended with warnings, when only a clean exit proves the
// task's effect.
func ConfirmGuestTaskOK(ctx context.Context, c Client, node, upid, kind string, vmid int) error {
	task, err := sdktasks.ParseUPID(upid)
	if err != nil {
		return fmt.Errorf("task %q is not a UPID: %w", upid, err)
	}
	if task.Node != node || task.Type != kind || task.ID != strconv.Itoa(vmid) {
		return fmt.Errorf("task %s is not a %s task of VM %d on node %s", upid, kind, vmid, node)
	}
	status, err := c.Tasks().GetStatus(ctx, node, upid)
	if err != nil {
		return fmt.Errorf("read task %s on node %s: %w", upid, node, err)
	}
	if status == nil || status.UpID != upid {
		return fmt.Errorf("task %s on node %s read back as another task", upid, node)
	}
	if status.Status != "stopped" || status.ExitStatus != "OK" || status.Warned {
		return fmt.Errorf("task %s on node %s reports status %q, exit status %q, warned %t",
			upid, node, status.Status, status.ExitStatus, status.Warned)
	}
	return nil
}
