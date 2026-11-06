package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

func applyCreatedVMRoutes(ctx context.Context, deps Deps, parsed *createVMParsedArgs, shape *createVMShape, vmid int, logger *log.Logger) error {
	if parsed.storageRuntime == nil {
		return applyAdvertisedRoutes(ctx, deps, shape.node, vmid, parsed.cloudProps.AdvertisedRoutes, logger)
	}
	m := parsed.storageRuntime
	if len(parsed.cloudProps.AdvertisedRoutes) == 0 {
		return nil
	}
	warnNonEVPNRouteZones(ctx, deps, vmid, parsed.cloudProps.AdvertisedRoutes, logger)
	for _, route := range parsed.cloudProps.AdvertisedRoutes {
		found, err := m.observeRoute(ctx, route, false)
		if err != nil {
			return err
		}
		if !found {
			if err := createSDNSubnet(ctx, deps.PVE.Cluster(), route.VNet, route.Destination); err != nil {
				return err
			}
		}
	}
	_, err := deps.PVE.Cluster().UpdateSdn(ctx, nil)
	return err
}
func (m *managedVMAllocation) observeRoute(ctx context.Context, route AdvertisedRoute, running bool) (bool, error) {
	identity, err := resolveManagedVMRoute(ctx, m.deps, route)
	if err != nil {
		return false, err
	}
	return observeManagedVMRoute(ctx, m.deps, identity, running)
}

func (m *managedVMAllocation) observeSDNMutation(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
	if call.Method == "CreateSdnVnetsSubnets" {
		identity, err := managedVMRecordedRoute(m.handle.Record(), step)
		if err != nil {
			return err
		}
		found, err := observeManagedVMRoute(ctx, m.deps, identity, false)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("created subnet not observed")
		}
		return nil
	}
	raw, ok := result.(*json.RawMessage)
	if !ok {
		return fmt.Errorf("malformed SDN apply response")
	}
	if raw != nil && len(*raw) > 0 && string(*raw) != "null" && string(*raw) != `""` {
		upid, err := managedMutationUPID(raw)
		if err != nil {
			return err
		}
		if err := storageMutationSubmitted(m.handle, step, upid); err != nil {
			return err
		}
		parts := strings.Split(upid, ":")
		if len(parts) < 3 || parts[1] == "" {
			return fmt.Errorf("SDN task node not proven")
		}
		// The caller's guard owns the mutation intent; wait synchronously before its
		// observed transition and require running configuration for every route.
		if err := pve.AwaitTaskWithLogger(ctx, m.deps.PVE, parts[1], upid, m.deps.Log(ctx)); err != nil {
			return err
		}
	}
	for _, route := range m.parsed.cloudProps.AdvertisedRoutes {
		found, err := m.observeRoute(ctx, route, true)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("applied route not present in running configuration")
		}
	}
	return nil
}
func (m *managedVMAllocation) observeDeletedHARule(ctx context.Context, call ManagedAllocationMutation) error {
	rule, ok := call.Args["rule"].(string)
	if !ok || rule == "" {
		return fmt.Errorf("missing HA rule identity")
	}
	rows, err := m.deps.PVE.Cluster().ListHaRules(ctx, nil)
	if err != nil {
		return err
	}
	if rows == nil {
		return fmt.Errorf("HA rule absence unreadable")
	}
	for _, raw := range *rows {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if fields["rule"] == rule {
			return fmt.Errorf("HA rule still present for VM %s", strconv.Itoa(m.vmid))
		}
	}
	return nil
}

// observeUpdatedHARule proves a rule update landed. It reads the rules back and
// compares the updated rule, normalized the way the rule writer reads it, with
// the rule the update describes: its type, its resources, its comment, whether
// it is disabled, its affinity, and for a node-affinity rule its nodes, their
// priorities, and strict. A field the update deletes must read back unset.
func (m *managedVMAllocation) observeUpdatedHARule(ctx context.Context, call ManagedAllocationMutation) error {
	rule, _ := call.Args["rule"].(string)
	params, ok := call.Args[managedArgumentParams].(*cluster.UpdateHaRulesParams)
	if rule == "" || !ok || params == nil || params.Resources == nil {
		return fmt.Errorf("missing HA rule update identity")
	}
	want, err := expectedUpdatedHARule(rule, params)
	if err != nil {
		return err
	}
	response, err := m.deps.PVE.Cluster().ListHaRules(ctx, nil)
	if err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("HA rule update unreadable")
	}
	set, err := ha.ParseRuleList(response)
	if err != nil {
		return err
	}
	got := set.Find(rule)
	if got == nil {
		return fmt.Errorf("HA rule absent after its update for VM %s", strconv.Itoa(m.vmid))
	}
	if diff := ha.RuleDiff(want, got); len(diff) > 0 {
		return fmt.Errorf("HA rule update readback differs in %s", strings.Join(diff, ", "))
	}
	return nil
}

// expectedUpdatedHARule renders the rule an update describes as a list row and
// reads it back through the rule parser, so the comparison uses one
// normalization for both sides.
func expectedUpdatedHARule(rule string, params *cluster.UpdateHaRulesParams) (*ha.Rule, error) {
	row := map[string]any{"rule": rule, "type": params.Type, "resources": *params.Resources}
	if params.Comment != nil {
		row["comment"] = *params.Comment
	}
	if params.Affinity != nil {
		row["affinity"] = *params.Affinity
	}
	if params.Nodes != nil {
		row["nodes"] = *params.Nodes
	}
	if params.Strict != nil && *params.Strict {
		row["strict"] = 1
	}
	if params.Disable != nil && *params.Disable {
		row["disable"] = 1
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("cannot encode the HA rule update")
	}
	set, err := ha.ParseRuleList(&cluster.ListHaRulesResponse{raw})
	if err != nil {
		return nil, err
	}
	return set.Find(rule), nil
}
