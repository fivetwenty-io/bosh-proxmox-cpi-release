package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"strings"
)

func cleanupPendingHAPurge(step aj.Step, record aj.Record) bool {
	if record.Kind != "vm" || step.Attempt != record.ActiveAttempt() || step.State != aj.Planned || step.UPID != "" || len(step.Charges) != 0 || len(step.VolIDs) != 0 || step.Target.External || step.Target.Node == "" || step.Target.VMID <= 0 || step.Target.Storage != "" || step.Target.Backing != "" || step.Target.IntendedVolume != "" {
		return false
	}
	var fields map[string]any
	if json.Unmarshal(step.Parameters, &fields) != nil || fields["version"] != float64(1) {
		return false
	}
	sid := haResourceSid(step.Target.VMID)
	switch step.Kind {
	case "vm.delete.ha":
		return len(fields) == 3 && fields["kind"] == "ha_resource_purge" && fields["resources"] == sid
	case "vm.ha.purge_rule":
		rule, ok := fields["rule"].(string)
		if !ok || strings.TrimSpace(rule) == "" || fields["kind"] != "ha_rule_purge" {
			return false
		}
		original, ok := fields["original_resources"].(string)
		if !ok {
			return false
		}
		desired, ok := fields["desired_resources"].(string)
		if !ok {
			return false
		}
		encoded, _ := json.Marshal(original)
		members := parseHaResources(encoded)
		if _, ok := members[sid]; !ok {
			return false
		}
		delete(members, sid)
		return desired == sidsCSV(members)
	}
	return false
}

func observeCleanupHAPurge(ctx context.Context, deps Deps, record aj.Record, step aj.Step) error {
	present, err := managedVMHAResourcePresent(ctx, deps, haResourceSid(step.Target.VMID))
	if err != nil || present {
		return fmt.Errorf("unknown HA purge has not been independently observed")
	}
	before := map[string]map[string]any{}
	for index := range record.Steps {
		old := &record.Steps[index]
		if old.Attempt != step.Attempt || old.Target != step.Target || old.Kind != "vm.ha.purge_rule" {
			continue
		}
		candidate := *old
		if candidate.State == aj.Observed {
			candidate.State = aj.Planned
		}
		if !cleanupPendingHAPurge(candidate, record) {
			return fmt.Errorf("HA purge rule evidence malformed")
		}
		var fields map[string]any
		if json.Unmarshal(old.Parameters, &fields) != nil {
			return fmt.Errorf("HA purge rule evidence unreadable")
		}
		name, ok := fields["rule"].(string)
		if !ok || name == "" || before[name] != nil {
			return fmt.Errorf("HA purge rule evidence ambiguous")
		}
		fields["resources"] = fields["original_resources"]
		before[name] = fields
	}
	return observeManagedVMHAPurge(ctx, deps, step.Target.VMID, before)
}
