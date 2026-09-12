package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func managedVMHAResourcePresent(ctx context.Context, deps Deps, sid string) (bool, error) {
	rows, err := deps.PVE.Cluster().ListHaResources(ctx, nil)
	if err != nil || rows == nil || *rows == nil {
		return false, fmt.Errorf("HA resource membership unavailable")
	}
	seen := map[string]bool{}
	for _, raw := range *rows {
		var row struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return false, fmt.Errorf("HA resource membership malformed")
		}
		if !managedHAResourceSID(row.SID) || seen[row.SID] {
			return false, fmt.Errorf("HA resource membership identity malformed or duplicated")
		}
		seen[row.SID] = true
	}
	return seen[sid], nil
}

func managedHAResourceSID(sid string) bool {
	kind, id, ok := strings.Cut(sid, ":")
	vmid, err := strconv.Atoi(id)
	return ok && (kind == "vm" || kind == "ct") && err == nil && vmid > 0 && strconv.Itoa(vmid) == id
}
func managedVMHARuleRows(ctx context.Context, deps Deps) (map[string]map[string]any, error) {
	rows, err := deps.PVE.Cluster().ListHaRules(ctx, nil)
	if err != nil || rows == nil || *rows == nil {
		return nil, fmt.Errorf("HA rule membership unavailable")
	}
	result := map[string]map[string]any{}
	for _, raw := range *rows {
		fields, err := managedEvidenceObject(raw)
		if err != nil {
			return nil, err
		}
		name, nameOK := fields["rule"].(string)
		resources, resourcesOK := fields["resources"].(string)
		if !nameOK || name == "" || strings.TrimSpace(name) != name || result[name] != nil || !resourcesOK || resources == "" {
			return nil, fmt.Errorf("HA rule identity or resource membership malformed")
		}
		members := map[string]bool{}
		for _, sid := range strings.Split(resources, ",") {
			sid = strings.TrimSpace(sid)
			if !managedHAResourceSID(sid) || members[sid] {
				return nil, fmt.Errorf("HA rule resource identity malformed or duplicated")
			}
			members[sid] = true
		}
		result[name] = fields
	}
	return result, nil
}
