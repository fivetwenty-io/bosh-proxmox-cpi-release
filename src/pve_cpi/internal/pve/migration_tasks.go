package pve

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// MigrationTaskReader lists the migration tasks of one VM on a node. The
// replicate policy reads it to tell a planned migration of a VM from a
// failover, and to wait for a migration in flight.
type MigrationTaskReader interface {
	MigrationTasks(ctx context.Context, node string, vmid int, since time.Time) ([]MigrationTask, error)
}

// MigrationTask is one row of a node's task list for a qmigrate task. A task
// that is still running has no end time and no final status.
type MigrationTask struct {
	UPID      string `json:"upid"`
	Node      string `json:"node"`
	Type      string `json:"type"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartTime int64  `json:"starttime"`
	EndTime   int64  `json:"endtime"`
}

// Running reports whether the task may still run. PVE reports a task in the
// active list as RUNNING until it archives it, whether or not it has ended,
// so a row with no end time or no final status counts as running.
func (t MigrationTask) Running() bool {
	return t.EndTime <= 0 || t.Status == "" || t.Status == "RUNNING"
}

// Succeeded reports whether the task ended with PVE's OK status.
func (t MigrationTask) Succeeded() bool { return t.EndTime > 0 && t.Status == "OK" }

// End returns the time the task ended, and the zero time for a task that is
// still running.
func (t MigrationTask) End() time.Time {
	if t.EndTime <= 0 {
		return time.Time{}
	}
	return time.Unix(t.EndTime, 0)
}

// The read the migration task listing names in an AuditVisibilityReadError.
const auditReadMigrationTasks = "migration tasks"

// migrationTaskLimit bounds one listing. A listing that comes back full may
// have hidden a row, and it is refused.
const migrationTaskLimit = 1000

// MigrationTasks lists the qmigrate tasks of vmid on node that started at or
// after since, the running ones and the archived ones alike. PVE names a
// qmigrate task by the VMID alone, so its vmid filter finds every one. PVE
// shows another user's tasks only to a caller with Sys.Audit on the node
// (pve-manager Tasks.pm:184 and :190), and an HA migration runs as root@pam,
// so the listing first proves that grant and refuses without it, because a
// listing that could hide a task proves nothing about it.
func (c *sdkClient) MigrationTasks(ctx context.Context, node string, vmid int, since time.Time) ([]MigrationTask, error) {
	if c == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	return observeMigrationTasks(ctx, c.auditReader, node, vmid, since)
}

func observeMigrationTasks(
	ctx context.Context, reader auditPermissionGetter, node string, vmid int, since time.Time,
) ([]MigrationTask, error) {
	if ctx == nil || reader == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	if node == "" || len(node) > 255 || !cleanupTaskNodePattern.MatchString(node) || vmid <= 0 {
		return nil, &AuditVisibilityReadError{Read: auditReadMigrationTasks, Path: "/nodes", Malformed: true,
			text: "migration task node or VM identity malformed"}
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
	query := map[string]interface{}{
		"source": "all", "typefilter": "qmigrate", "vmid": vmid, "start": 0, "limit": migrationTaskLimit,
	}
	if !since.IsZero() {
		query["since"] = since.Unix()
	}
	raw, err := reader.GetCtx(ctx, path, query)
	if err != nil {
		return nil, &AuditVisibilityReadError{Read: auditReadMigrationTasks, Path: path, Cause: err,
			text: "migration task listing unavailable"}
	}
	malformed := &AuditVisibilityReadError{Read: auditReadMigrationTasks, Path: path, Malformed: true,
		text: "migration task listing malformed"}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, malformed
	}
	var tasks []MigrationTask
	if err := json.Unmarshal(encoded, &tasks); err != nil || tasks == nil || len(tasks) >= migrationTaskLimit {
		return nil, malformed
	}
	id := strconv.Itoa(vmid)
	for _, task := range tasks {
		if !cleanupTaskUPIDPattern.MatchString(task.UPID) || task.Type != "qmigrate" || task.ID != id ||
			task.Node != "" && task.Node != node {
			return nil, malformed
		}
	}
	return tasks, nil
}
