package pve

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ActiveMoveTaskReader lists the move tasks PVE still has in a node's active
// task list. The settlement of a planned move step reads it to prove that no
// task forked for the step's POST can still be running.
type ActiveMoveTaskReader interface {
	ActiveMoveTasks(ctx context.Context, node string) ([]ActiveTask, error)
}

// ActiveTask is one row of a node's active task list.
type ActiveTask struct {
	UPID   string `json:"upid"`
	Node   string `json:"node"`
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
}

// The read the move task listing names in an AuditVisibilityReadError.
const auditReadActiveMoveTasks = "active move tasks"

// activeMoveTaskLimit bounds one listing. PVE answers at most this many rows,
// so a listing that comes back full may have hidden one, and it is refused.
const activeMoveTaskLimit = 1000

// MoveTaskIDNamesVM reports whether a qmmove task's ID names vmid as the VM it
// moves a disk from. PVE gives a move to another storage the plain VMID as its
// ID (qemu-server Qemu.pm:5210) and a move to another VM the ID
// "<vmid>-<disk>><target-vmid>-<target-disk>" (Qemu.pm:5196-5199). PVE's own
// vmid filter compares the whole ID (pve-manager Tasks.pm:194), so it never
// returns the second kind, and callers match with this instead.
func MoveTaskIDNamesVM(id string, vmid int) bool {
	if vmid <= 0 {
		return false
	}
	own := strconv.Itoa(vmid)
	return id == own || strings.HasPrefix(id, own+"-")
}

// ActiveMoveTasks lists every qmmove task in node's active task list. PVE
// shows another user's tasks only to a caller with Sys.Audit on the node
// (pve-manager Tasks.pm:184 and :190), so the listing first proves that grant
// and refuses without it, because a listing that could hide a task proves
// nothing about it. PVE reports a task in the active list as RUNNING until it
// archives it, whether or not it has ended, so every row is returned.
func (c *sdkClient) ActiveMoveTasks(ctx context.Context, node string) ([]ActiveTask, error) {
	if c == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	return observeActiveMoveTasks(ctx, c.auditReader, node)
}

func observeActiveMoveTasks(ctx context.Context, reader auditPermissionGetter, node string) ([]ActiveTask, error) {
	if ctx == nil || reader == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	if node == "" || len(node) > 255 || !cleanupTaskNodePattern.MatchString(node) {
		return nil, &AuditVisibilityReadError{Read: auditReadActiveMoveTasks, Path: "/nodes", Malformed: true, text: "active move task node identity malformed"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	privilegePath := "/nodes/" + node
	permissions, err := auditPermissionsAt(ctx, reader, privilegePath)
	if err != nil {
		return nil, err
	}
	if _, ok := permissions["Sys.Audit"]; !ok {
		return nil, &AuditVisibilityError{Privilege: "Sys.Audit", Path: privilegePath}
	}
	path := "/nodes/" + url.PathEscape(node) + "/tasks"
	raw, err := reader.GetCtx(ctx, path, map[string]interface{}{"source": "active", "typefilter": "qmmove", "start": 0, "limit": activeMoveTaskLimit})
	if err != nil {
		return nil, &AuditVisibilityReadError{Read: auditReadActiveMoveTasks, Path: path, Cause: err, text: "active move task listing unavailable"}
	}
	malformed := &AuditVisibilityReadError{Read: auditReadActiveMoveTasks, Path: path, Malformed: true, text: "active move task listing malformed"}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, malformed
	}
	var tasks []ActiveTask
	if err := json.Unmarshal(encoded, &tasks); err != nil || tasks == nil || len(tasks) >= activeMoveTaskLimit {
		return nil, malformed
	}
	// A refusal names the UPID of a task that blocks it, so every UPID must
	// have PVE's own shape before it can reach a refusal's text.
	for _, task := range tasks {
		if !cleanupTaskUPIDPattern.MatchString(task.UPID) {
			return nil, malformed
		}
	}
	return tasks, nil
}
