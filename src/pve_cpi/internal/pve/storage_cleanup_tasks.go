package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// StorageTaskSettlementReader supplies read-only evidence for explicit cleanup.
// It does not establish resource ownership or authorize a pending mutation.
type StorageTaskSettlementReader interface {
	ObserveStorageTaskSettlement(context.Context, []string, []string) (StorageTaskSettlementEvidence, error)
}

// StorageTaskSettlementEvidence records the scope and observation interval of
// a successful task scan. It is separate from resource ownership evidence.
type StorageTaskSettlementEvidence struct {
	Version                int                  `json:"version"`
	StartedAt              time.Time            `json:"started_at"`
	CompletedAt            time.Time            `json:"completed_at"`
	Nodes                  []string             `json:"nodes"`
	Tasks                  []StorageSettledTask `json:"tasks"`
	TaskVisibilityVerified bool                 `json:"task_visibility_verified"`
	ActiveTasksEmpty       bool                 `json:"active_tasks_empty"`
}

// StorageSettledTask identifies one recorded task whose terminal status was
// read from its source node during the observation interval.
type StorageSettledTask struct {
	UPID       string `json:"upid"`
	Node       string `json:"node"`
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
	StartTime  int64  `json:"starttime,omitempty"`
	EndTime    int64  `json:"endtime,omitempty"`
}

var cleanupTaskNodePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?$`)
var cleanupTaskUPIDPattern = regexp.MustCompile(`^UPID:([a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?):[0-9A-Fa-f]{8}:[0-9A-Fa-f]{8,9}:[0-9A-Fa-f]{8}:[^:\s/]+:[^:\s/]*:[^:\s/]+:$`)

// ObserveStorageCleanupTasks requires fresh, complete task evidence from the
// client and checks its scope before an explicit cleanup can retain it.
func ObserveStorageCleanupTasks(ctx context.Context, client Client, nodes, upids []string) (StorageTaskSettlementEvidence, error) {
	if ctx == nil {
		return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup task observation requires context")
	}
	nodes, upids, err := canonicalCleanupTaskInputs(nodes, upids)
	if err != nil {
		return StorageTaskSettlementEvidence{}, err
	}
	reader, ok := client.(StorageTaskSettlementReader)
	if !ok {
		return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup task observation unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	started := time.Now().UTC()
	proof, err := reader.ObserveStorageTaskSettlement(ctx, slices.Clone(nodes), slices.Clone(upids))
	if err != nil {
		return StorageTaskSettlementEvidence{}, err
	}
	if ctx.Err() != nil {
		return StorageTaskSettlementEvidence{}, ctx.Err()
	}
	if err := validateCleanupTaskEvidence(proof, nodes, upids, started, time.Now().UTC(), cleanupFailedAllocationTasks(ctx)); err != nil {
		return StorageTaskSettlementEvidence{}, err
	}
	return proof, nil
}

func (c *sdkClient) ObserveStorageTaskSettlement(ctx context.Context, nodes, upids []string) (StorageTaskSettlementEvidence, error) {
	if c == nil {
		return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup task observation client unavailable")
	}
	return observeStorageCleanupTasks(ctx, c.auditReader, nodes, upids)
}

func canonicalCleanupTaskInputs(nodes, upids []string) ([]string, []string, error) {
	if len(nodes) == 0 || len(nodes) > 128 || len(upids) > 4096 {
		return nil, nil, fmt.Errorf("storage cleanup task observation inputs unavailable or exceed limits")
	}
	nodes = slices.Clone(nodes)
	upids = slices.Clone(upids)
	slices.Sort(nodes)
	slices.Sort(upids)
	nodes = slices.Compact(nodes)
	upids = slices.Compact(upids)
	for _, node := range nodes {
		if len(node) > 255 || !cleanupTaskNodePattern.MatchString(node) {
			return nil, nil, fmt.Errorf("storage cleanup task node identity malformed")
		}
	}
	for _, upid := range upids {
		match := cleanupTaskUPIDPattern.FindStringSubmatch(upid)
		if len(upid) > 1024 || len(match) != 2 || !slices.Contains(nodes, match[1]) {
			return nil, nil, fmt.Errorf("storage cleanup task identity is malformed or outside observed nodes")
		}
	}
	return nodes, upids, nil
}

func validateCleanupTaskEvidence(proof StorageTaskSettlementEvidence, nodes, upids []string, started, completed time.Time, allowed ...map[string]bool) error {
	if proof.Version != 1 || !proof.TaskVisibilityVerified || !proof.ActiveTasksEmpty || !slices.Equal(proof.Nodes, nodes) || proof.Tasks == nil || len(proof.Tasks) != len(upids) || proof.StartedAt.Before(started) || proof.CompletedAt.After(completed) || proof.CompletedAt.Before(proof.StartedAt) {
		return fmt.Errorf("storage cleanup task evidence is incomplete, stale, or outside requested scope")
	}
	var failed map[string]bool
	if len(allowed) == 1 {
		failed = allowed[0]
	}
	for index, task := range proof.Tasks {
		if task.UPID != upids[index] || task.Node != nodeFromUPID(upids[index]) || task.Status != taskStatusStopped || !cleanupTaskExitAccepted(task, failed) {
			return fmt.Errorf("storage cleanup task evidence does not prove every recorded task")
		}
		if strings.Split(task.UPID, ":")[5] == "imgcopy" && !cleanupUploadTaskIntervalValid(task, proof.CompletedAt) {
			return fmt.Errorf("storage cleanup upload task interval is unproven")
		}
	}
	return nil
}

func observeStorageCleanupTasks(ctx context.Context, reader auditPermissionGetter, nodes, upids []string) (StorageTaskSettlementEvidence, error) {
	if ctx == nil || reader == nil {
		return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup task observation inputs unavailable")
	}
	nodes, upids, err := canonicalCleanupTaskInputs(nodes, upids)
	if err != nil {
		return StorageTaskSettlementEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	proof := StorageTaskSettlementEvidence{Version: 1, StartedAt: time.Now().UTC(), Nodes: nodes, Tasks: []StorageSettledTask{}}
	for _, node := range nodes {
		permissions, err := auditPermissionsAt(ctx, reader, "/nodes/"+node)
		if err != nil {
			return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup task visibility unavailable")
		}
		if _, ok := permissions["Sys.Audit"]; !ok {
			return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup requires Sys.Audit on every observed node")
		}
	}
	proof.TaskVisibilityVerified = true
	for _, upid := range upids {
		task, err := observeSettledStorageTask(ctx, reader, upid)
		if err != nil {
			return StorageTaskSettlementEvidence{}, err
		}
		proof.Tasks = append(proof.Tasks, task)
	}
	for _, node := range nodes {
		if err := observeNoActiveStorageTasks(ctx, reader, node); err != nil {
			return StorageTaskSettlementEvidence{}, err
		}
	}
	proof.ActiveTasksEmpty = true
	proof.CompletedAt = time.Now().UTC()
	return proof, nil
}

func observeSettledStorageTask(ctx context.Context, reader auditPermissionGetter, upid string) (StorageSettledTask, error) {
	node := strings.Split(upid, ":")[1]
	path := "/nodes/" + url.PathEscape(node) + "/tasks/" + url.PathEscape(upid) + "/status"
	raw, err := reader.GetCtx(ctx, path, nil)
	if err != nil || raw == nil {
		return StorageSettledTask{}, fmt.Errorf("storage cleanup recorded task status unavailable")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return StorageSettledTask{}, fmt.Errorf("storage cleanup recorded task status malformed")
	}
	var task StorageSettledTask
	if err := json.Unmarshal(encoded, &task); err != nil || task.UPID != upid || task.Node != node || task.Status != taskStatusStopped || !cleanupTaskExitAccepted(task, cleanupFailedAllocationTasks(ctx)) {
		return StorageSettledTask{}, fmt.Errorf("storage cleanup recorded task identity or successful completion unproven")
	}
	if strings.Split(upid, ":")[5] == "imgcopy" {
		if err := observeCleanupUploadTaskInterval(ctx, reader, &task, encoded); err != nil {
			return StorageSettledTask{}, err
		}
	}
	return task, nil
}

func observeNoActiveStorageTasks(ctx context.Context, reader auditPermissionGetter, node string) error {
	// Any returned task is enough to refuse cleanup, so limit=1 is complete
	// for this predicate. No user, VM, type, status, or time filters are used.
	raw, err := reader.GetCtx(ctx, "/nodes/"+url.PathEscape(node)+"/tasks", map[string]interface{}{"source": "active", "start": 0, "limit": 1})
	if err != nil || raw == nil {
		return fmt.Errorf("storage cleanup active task listing unavailable")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("storage cleanup active task listing malformed")
	}
	var active []json.RawMessage
	if err := json.Unmarshal(encoded, &active); err != nil || active == nil {
		return fmt.Errorf("storage cleanup active task listing is not an array")
	}
	if len(active) != 0 {
		return fmt.Errorf("storage cleanup requires empty active task lists on every observed node")
	}
	return nil
}

type cleanupFailedAllocationTasksKey struct{}

func cleanupFailedAllocationTasks(ctx context.Context) map[string]bool {
	value, _ := ctx.Value(cleanupFailedAllocationTasksKey{}).(map[string]bool)
	return value
}
func cleanupTaskExitAccepted(task StorageSettledTask, allowed map[string]bool) bool {
	return task.ExitStatus == "OK" || allowed[task.UPID] && strings.TrimSpace(task.ExitStatus) != "" && len(task.ExitStatus) <= 4096
}

// ObserveStorageCleanupAllocationTasks permits terminal failure only for the
// explicitly identified root allocation tasks. Resource ownership or complete
// absence must be independently proved by the caller; deletion and upload tasks
// retain the success requirement of ObserveStorageCleanupTasks.
func ObserveStorageCleanupAllocationTasks(ctx context.Context, client Client, nodes, upids, allocationUPIDs []string) (StorageTaskSettlementEvidence, error) {
	if ctx == nil {
		return StorageTaskSettlementEvidence{}, fmt.Errorf("storage cleanup requires context")
	}
	allowed := map[string]bool{}
	for _, upid := range allocationUPIDs {
		parts := strings.Split(upid, ":")
		if !slices.Contains(upids, upid) || len(parts) != 9 || (parts[5] != "qmcreate" && parts[5] != "qmclone") || parts[6] == "" {
			return StorageTaskSettlementEvidence{}, fmt.Errorf("failed task allowance is not an exact root allocation task")
		}
		vmid, err := strconv.Atoi(parts[6])
		if err != nil || vmid <= 0 || strconv.Itoa(vmid) != parts[6] {
			return StorageTaskSettlementEvidence{}, fmt.Errorf("root allocation task VM identity malformed")
		}
		allowed[upid] = true
	}
	return ObserveStorageCleanupTasks(context.WithValue(ctx, cleanupFailedAllocationTasksKey{}, allowed), client, nodes, upids)
}
