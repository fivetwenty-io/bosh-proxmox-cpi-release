package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"strconv"
	"strings"
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
