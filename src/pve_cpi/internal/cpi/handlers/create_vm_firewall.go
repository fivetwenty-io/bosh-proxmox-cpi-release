package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// fwGroupRuleType is the PVE firewall rule type used to reference a cluster
// firewall group from a VM: a rule with type=group and action=<group name>.
const fwGroupRuleType = "group"

// applySecurityGroups attaches each named PVE cluster firewall group to the VM
// as a group-type firewall rule and enables the VM-level firewall so the rules
// take effect. The named groups must already exist in PVE
// (/cluster/firewall/groups); the CPI references group content but never creates
// or modifies it.
//
// A requested group that does not exist is reported as a non-retriable
// CloudError (an operator misconfiguration the director should not retry). PVE
// API faults on the rule-create / firewall-enable calls are wrapped retriable
// via pve.WrapError. Every error path returns to create_vm, which rolls the VM
// back, so a partially-firewalled VM is never left behind.
//
// Called after VM start with the resolved node and vmid. A nil/empty groups
// slice is handled by the caller (no call is made), so this function assumes at
// least one group.
func applySecurityGroups(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	groups []string,
	logger *log.Logger,
) error {
	nodeSvc := deps.PVE.Nodes()
	vmidStr := strconv.Itoa(vmid)

	// 1. Validate every requested group exists before mutating the VM.
	existing, err := listFirewallGroupNames(ctx, deps)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), "create_vm: list firewall groups")
	}
	for _, g := range groups {
		if _, ok := existing[g]; !ok {
			return cpierrors.Cloud(
				"create_vm: PVE firewall group %q not found; create it in PVE before referencing it in security_groups", g,
			)
		}
	}

	// 2. Attach each group as a group-type rule on the VM. In the PVE firewall
	//    API a group reference is a rule with type=group and action=<group name>.
	for _, g := range groups {
		enable := int64(1)
		groupName := g
		if ruleErr := nodeSvc.CreateQemuFirewallRules(ctx, node, vmidStr, &sdknodes.CreateQemuFirewallRulesParams{
			Type:   fwGroupRuleType,
			Action: groupName,
			Enable: &enable,
		}); ruleErr != nil {
			return cpierrors.Wrap(pve.WrapError(ruleErr),
				fmt.Sprintf("create_vm: attach firewall group %q vmid=%d", g, vmid))
		}
	}

	// 3. Enable the VM-level firewall so the attached group rules filter traffic.
	enableFW := true
	if optErr := nodeSvc.UpdateQemuFirewallOptions(ctx, node, vmidStr, &sdknodes.UpdateQemuFirewallOptionsParams{
		Enable: &enableFW,
	}); optErr != nil {
		return cpierrors.Wrap(pve.WrapError(optErr),
			fmt.Sprintf("create_vm: enable VM firewall vmid=%d", vmid))
	}

	logger.Info("create_vm: applied firewall security groups",
		log.Int("vmid", vmid),
		log.Int("group_count", len(groups)),
	)
	return nil
}

// listFirewallGroupNames returns the set of PVE cluster firewall group names.
// The /cluster/firewall/groups response is an array of objects each carrying a
// "group" field; entries that fail to decode are skipped.
func listFirewallGroupNames(ctx context.Context, deps Deps) (map[string]struct{}, error) {
	resp, err := deps.PVE.Cluster().ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if resp == nil {
		return out, nil
	}
	for _, raw := range *resp {
		var entry struct {
			Group string `json:"group"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Group != "" {
			out[entry.Group] = struct{}{}
		}
	}
	return out, nil
}
