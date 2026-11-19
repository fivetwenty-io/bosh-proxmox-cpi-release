package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// haPinRuleNamePrefix is the start of the name the CPI gives a VM's or a
// parker's node-affinity pin, bosh-na-<vmid>.
const haPinRuleNamePrefix = "bosh-na-"

// WithHAStateScan makes NextVMID also treat as used every VMID the cluster's
// HA state names: the guest of every HA resource, every resource an HA rule
// lists, and the VMID in the name of every bosh-na-<vmid> pin. A VM or parker
// whose config is gone can leave its resource or its pin behind, and a guest
// created at that VMID would inherit them, so an allocator that must never
// adopt HA state it did not write asks for this scan. A failed read fails the
// allocation rather than allocate blind to the HA state.
func WithHAStateScan() AllocOption {
	return func(o *allocOpts) {
		o.haStateScan = true
	}
}

// listHAStateVMIDs returns the VMIDs the cluster's HA resources and rules
// name, as WithHAStateScan describes.
func listHAStateVMIDs(ctx context.Context, c Client) (map[int]struct{}, error) {
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("NextVMID: the HA state scan has no cluster client")
	}
	used := map[int]struct{}{}
	addSID := func(sid string) {
		kind, id, ok := strings.Cut(strings.TrimSpace(sid), ":")
		if !ok || (kind != "vm" && kind != "ct") {
			return
		}
		if vmid, err := strconv.Atoi(id); err == nil && vmid > 0 {
			used[vmid] = struct{}{}
		}
	}

	var resources *sdkcluster.ListHaResourcesResponse
	err := RetryOnTransient(ctx, nil, "vmid_ha_resources_scan", 0, func() error {
		var inner error
		resources, inner = svc.ListHaResources(ctx, nil)
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "NextVMID: list the HA resources")
	}
	if resources == nil {
		return nil, cpierrors.Retriable("NextVMID: list the HA resources: no rows returned")
	}
	for i, raw := range *resources {
		var row struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, cpierrors.Cloud("NextVMID: HA resources row %d does not parse: %v", i, err)
		}
		addSID(row.SID)
	}

	var rules *sdkcluster.ListHaRulesResponse
	err = RetryOnTransient(ctx, nil, "vmid_ha_rules_scan", 0, func() error {
		var inner error
		rules, inner = svc.ListHaRules(ctx, &sdkcluster.ListHaRulesParams{})
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "NextVMID: list the HA rules")
	}
	if rules == nil {
		return nil, cpierrors.Retriable("NextVMID: list the HA rules: no rows returned")
	}
	for i, raw := range *rules {
		var row struct {
			Rule      string          `json:"rule"`
			Resources json.RawMessage `json:"resources"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, cpierrors.Cloud("NextVMID: HA rules row %d does not parse: %v", i, err)
		}
		if id, ok := strings.CutPrefix(row.Rule, haPinRuleNamePrefix); ok {
			if vmid, err := strconv.Atoi(id); err == nil && vmid > 0 {
				used[vmid] = struct{}{}
			}
		}
		sids, err := haRuleResourceSIDs(row.Resources)
		if err != nil {
			return nil, cpierrors.Cloud("NextVMID: HA rule %q lists resources that do not parse: %v", row.Rule, err)
		}
		for _, sid := range sids {
			addSID(sid)
		}
	}
	return used, nil
}

// haRuleResourceSIDs reads a rule's resources field, which PVE sends as one
// comma-separated string and a client may see as an array of strings. An
// absent or null field lists none.
func haRuleResourceSIDs(raw json.RawMessage) ([]string, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	var list []string
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		for _, sid := range strings.Split(v, ",") {
			if sid = strings.TrimSpace(sid); sid != "" {
				list = append(list, sid)
			}
		}
	case []any:
		for _, item := range v {
			sid, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("a rule resource is %T, not a string", item)
			}
			list = append(list, sid)
		}
	default:
		return nil, fmt.Errorf("the rule resources field is %T, not a string or a list", value)
	}
	return list, nil
}
