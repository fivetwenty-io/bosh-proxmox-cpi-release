package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func cleanupUploadTaskIntervalValid(task StorageSettledTask, observed time.Time) bool {
	parts := strings.Split(task.UPID, ":")
	if len(parts) != 9 || parts[5] != "imgcopy" || parts[6] != "" {
		return false
	}
	started, err := strconv.ParseInt(parts[4], 16, 64)
	return err == nil && started > 0 && task.StartTime == started && task.EndTime >= started && task.EndTime <= observed.Unix()
}

func observeCleanupUploadTaskInterval(ctx context.Context, reader auditPermissionGetter, task *StorageSettledTask, status []byte) error {
	var identity struct {
		Type      string  `json:"type"`
		ID        *string `json:"id"`
		StartTime int64   `json:"starttime"`
	}
	if json.Unmarshal(status, &identity) != nil || identity.Type != "imgcopy" || identity.ID == nil || *identity.ID != "" {
		return fmt.Errorf("storage cleanup upload task status identity differs")
	}
	task.StartTime = identity.StartTime
	task.EndTime = identity.StartTime
	if !cleanupUploadTaskIntervalValid(*task, time.Now()) {
		return fmt.Errorf("storage cleanup upload task start is unproven")
	}
	end, err := observeCleanupUploadTaskHistory(ctx, reader, *task)
	if err != nil {
		return err
	}
	task.EndTime = end
	return nil
}

// PVE task status omits endtime. The task archive supplies it. Its since/until
// filters are inclusive start times, so scan every page for the exact second
// and worker type, rejecting ambiguity or an incomplete bounded observation.
func observeCleanupUploadTaskHistory(ctx context.Context, reader auditPermissionGetter, task StorageSettledTask) (int64, error) {
	const pageSize = 100
	const maxPages = 128
	seen := make(map[string]bool)
	var end int64
	found := false
	for page := 0; page < maxPages; page++ {
		raw, err := reader.GetCtx(ctx, "/nodes/"+url.PathEscape(task.Node)+"/tasks", map[string]interface{}{
			"source": "all", "start": page * pageSize, "limit": pageSize,
			"since": task.StartTime, "until": task.StartTime, "typefilter": "imgcopy",
		})
		if err != nil || raw == nil {
			return 0, fmt.Errorf("storage cleanup upload task history unavailable")
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return 0, fmt.Errorf("storage cleanup upload task history malformed")
		}
		var rows []json.RawMessage
		if json.Unmarshal(encoded, &rows) != nil || rows == nil || len(rows) > pageSize {
			return 0, fmt.Errorf("storage cleanup upload task history is not a bounded array")
		}
		for _, row := range rows {
			var item struct {
				UPID string `json:"upid"`
			}
			if json.Unmarshal(row, &item) != nil || item.UPID == "" || seen[item.UPID] {
				return 0, fmt.Errorf("storage cleanup upload task history is malformed or duplicated")
			}
			seen[item.UPID] = true
			if item.UPID != task.UPID {
				continue
			}
			end, err = cleanupUploadHistoryEnd(row, task)
			if err != nil {
				return 0, err
			}
			found = true
		}
		if len(rows) < pageSize {
			if !found {
				return 0, fmt.Errorf("storage cleanup upload task history is missing")
			}
			return end, nil
		}
	}
	return 0, fmt.Errorf("storage cleanup upload task history exceeded observation limit")
}

func cleanupUploadHistoryEnd(raw []byte, task StorageSettledTask) (int64, error) {
	var row struct {
		UPID      string  `json:"upid"`
		Node      string  `json:"node"`
		Type      string  `json:"type"`
		ID        *string `json:"id"`
		Status    string  `json:"status"`
		StartTime int64   `json:"starttime"`
		EndTime   int64   `json:"endtime"`
	}
	if json.Unmarshal(raw, &row) != nil || row.UPID != task.UPID || row.Node != task.Node || row.Type != "imgcopy" || row.ID == nil || *row.ID != "" || row.Status != "OK" || row.StartTime != task.StartTime {
		return 0, fmt.Errorf("storage cleanup upload task history identity differs")
	}
	task.EndTime = row.EndTime
	if !cleanupUploadTaskIntervalValid(task, time.Now()) {
		return 0, fmt.Errorf("storage cleanup upload task interval is invalid")
	}
	return row.EndTime, nil
}

// CorroboratesUploadTimestamp checks the ISO timestamp against one task interval.
// The settlement evidence must first pass the fresh task identity validation.
func (proof *StorageTaskSettlementEvidence) CorroboratesUploadTimestamp(upid string, ctime int64) bool {
	if proof == nil {
		return false
	}
	matches := 0
	for _, task := range proof.Tasks {
		if task.UPID == upid {
			if task.StartTime <= 0 || task.EndTime < task.StartTime || ctime < task.StartTime || ctime > task.EndTime {
				return false
			}
			matches++
		}
	}
	return matches == 1
}
