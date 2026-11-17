package pve

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// MigrationTaskReader lists the migration tasks and the start tasks of one VM
// on a node. The replicate policy reads them to tell a planned migration of a
// VM from a failover, and to wait for a migration in flight.
type MigrationTaskReader interface {
	MigrationTasks(ctx context.Context, node string, vmid int, since time.Time) ([]VMTask, error)
	StartTasks(ctx context.Context, node string, vmid int, since time.Time) ([]VMTask, error)
}

// VMTask is one row of a node's task list for one VM, such as a qmigrate or
// a qmstart task. A task that is still running has no end time and no final
// status.
type VMTask struct {
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
func (t VMTask) Running() bool {
	return t.EndTime <= 0 || t.Status == "" || t.Status == "RUNNING"
}

// Succeeded reports whether the task ended in success. PVE ends a task that
// logged warnings with the status WARNINGS: n and counts it as a success
// (pve-common PVE::Tools upid_normalize_status_type), so a migration that
// warned about a leftover cloud-init drive still moved the VM.
func (t VMTask) Succeeded() bool {
	return t.EndTime > 0 && (t.Status == "OK" || taskWarningsStatus.MatchString(t.Status))
}

// taskWarningsStatus matches the status PVE gives a task that ended in success
// after it logged warnings.
var taskWarningsStatus = regexp.MustCompile(`^WARNINGS: \d+$`)

// End returns the time the task ended, and the zero time for a task that is
// still running.
func (t VMTask) End() time.Time {
	if t.EndTime <= 0 {
		return time.Time{}
	}
	return time.Unix(t.EndTime, 0)
}

// The reads the task listings name in an AuditVisibilityReadError.
const (
	auditReadMigrationTasks = "migration tasks"
	auditReadStartTasks     = "start tasks"
)

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
func (c *sdkClient) MigrationTasks(ctx context.Context, node string, vmid int, since time.Time) ([]VMTask, error) {
	if c == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	return observeVMTasks(ctx, c.auditReader, vmTaskListing{taskType: "qmigrate", read: auditReadMigrationTasks,
		what: "migration"}, node, vmid, since)
}

// StartTasks lists the qmstart tasks of vmid on node that started at or after
// since, with the same Sys.Audit proof and the same refusals as
// MigrationTasks. PVE's vm_start runs every start in a qmstart worker
// (qemu-server API2/Qemu.pm vm_start), and an HA recovery starts the VM
// through it as root@pam (pve-ha-manager Resources/PVEVM.pm start). The
// target of a live migration starts the incoming VM with qm start
// --migratedfrom (QemuMigrate.pm phase2_start_local_cluster), which runs the
// same worker, so the target may list one qmstart task for each live
// migration it received.
func (c *sdkClient) StartTasks(ctx context.Context, node string, vmid int, since time.Time) ([]VMTask, error) {
	if c == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	return observeVMTasks(ctx, c.auditReader, vmTaskListing{taskType: "qmstart", read: auditReadStartTasks,
		what: "start"}, node, vmid, since)
}

// vmTaskListing names one task type a VM's task listing filters for, the read
// its errors name, and the word its error texts use.
type vmTaskListing struct {
	taskType, read, what string
}

func observeVMTasks(
	ctx context.Context, reader auditPermissionGetter, listing vmTaskListing, node string, vmid int,
	since time.Time,
) ([]VMTask, error) {
	if ctx == nil || reader == nil {
		return nil, ErrAuditVisibilityReaderUnavailable
	}
	if node == "" || len(node) > 255 || !cleanupTaskNodePattern.MatchString(node) || vmid <= 0 {
		return nil, &AuditVisibilityReadError{Read: listing.read, Path: "/nodes", Malformed: true,
			text: listing.what + " task node or VM identity malformed"}
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
		"source": "all", "typefilter": listing.taskType, "vmid": vmid, "start": 0, "limit": migrationTaskLimit,
	}
	if !since.IsZero() {
		query["since"] = since.Unix()
	}
	raw, err := reader.GetCtx(ctx, path, query)
	if err != nil {
		return nil, &AuditVisibilityReadError{Read: listing.read, Path: path, Cause: err,
			text: listing.what + " task listing unavailable"}
	}
	malformed := &AuditVisibilityReadError{Read: listing.read, Path: path, Malformed: true,
		text: listing.what + " task listing malformed"}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, malformed
	}
	var tasks []VMTask
	if err := json.Unmarshal(encoded, &tasks); err != nil || tasks == nil || len(tasks) >= migrationTaskLimit {
		return nil, malformed
	}
	id := strconv.Itoa(vmid)
	for _, task := range tasks {
		if !cleanupTaskUPIDPattern.MatchString(task.UPID) || task.Type != listing.taskType || task.ID != id ||
			task.Node != "" && task.Node != node {
			return nil, malformed
		}
	}
	return tasks, nil
}
