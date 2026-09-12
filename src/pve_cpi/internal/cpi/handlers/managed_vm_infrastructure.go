package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

const managedVMRouteKind = "route"
const managedVMHARuleKind = "ha_rule"

type managedVMRouteIdentity struct {
	Version          int    `json:"version"`
	Kind             string `json:"kind"`
	VNet             string `json:"vnet"`
	Subnet           string `json:"subnet"`
	CIDR             string `json:"cidr"`
	SourceAllocation string `json:"source_allocation,omitempty"`
	SourceStep       string `json:"source_step,omitempty"`
}

func (m *managedVMAllocation) infrastructureMutationParameters(ctx context.Context, call ManagedAllocationMutation) (json.RawMessage, error) {
	switch call.Service + "." + call.Method {
	case "Cluster.CreateSdnVnetsSubnets":
		params, ok := call.Args["params"].(*cluster.CreateSdnVnetsSubnetsParams)
		vnet, _ := call.Args["vnet"].(string)
		if !ok || params == nil || vnet == "" {
			return nil, fmt.Errorf("subnet creation lacks concrete identity")
		}
		identity, err := resolveManagedVMRoute(ctx, m.deps, AdvertisedRoute{VNet: vnet, Destination: params.Subnet})
		if err != nil {
			return nil, err
		}
		exists, err := observeManagedVMRoute(ctx, m.deps, identity, false)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("subnet was not absent before its creation intent")
		}
		return aj.MutationParameters(identity)
	case "Cluster.CreateHaRules":
		params, ok := call.Args["params"].(*cluster.CreateHaRulesParams)
		if !ok || params == nil {
			return nil, fmt.Errorf("HA rule creation lacks concrete identity")
		}
		fields, err := managedEvidenceObject(params)
		if err != nil {
			return nil, err
		}
		resources := strings.Split(params.Resources, ",")
		if !slices.Contains(resources, haResourceSid(m.vmid)) {
			return nil, fmt.Errorf("HA rule does not include the recorded VM")
		}
		existing, err := readManagedVMHARule(ctx, m.deps, params.Rule)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, fmt.Errorf("HA rule was not absent before its creation intent")
		}
		fields["version"] = 1
		fields["kind"] = managedVMHARuleKind
		return aj.MutationParameters(fields)
	default:
		return nil, nil
	}
}

func resolveManagedVMRoute(ctx context.Context, deps Deps, route AdvertisedRoute) (managedVMRouteIdentity, error) {
	vnet, err := pve.GetSDNVnet(ctx, deps.PVE, route.VNet)
	if err != nil {
		return managedVMRouteIdentity{}, err
	}
	if vnet == nil || vnet.Zone == "" {
		return managedVMRouteIdentity{}, fmt.Errorf("route vnet zone cannot be observed")
	}
	return managedVMRouteIdentity{Version: 1, Kind: managedVMRouteKind, VNet: route.VNet, Subnet: vnet.Zone + "-" + strings.ReplaceAll(route.Destination, "/", "-"), CIDR: route.Destination}, nil
}

func observeManagedVMRoute(ctx context.Context, deps Deps, identity managedVMRouteIdentity, running bool) (bool, error) {
	rows, err := deps.PVE.Cluster().ListSdnVnetsSubnets(ctx, identity.VNet, &cluster.ListSdnVnetsSubnetsParams{Running: &running})
	if err != nil {
		return false, err
	}
	if rows == nil {
		return false, fmt.Errorf("route readback unavailable")
	}
	count := 0
	for _, raw := range *rows {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return false, fmt.Errorf("malformed subnet readback")
		}
		if fields["subnet"] != identity.Subnet {
			continue
		}
		if fields["vnet"] != identity.VNet || fields["type"] != "subnet" {
			return false, fmt.Errorf("subnet readback differs from recorded identity")
		}
		count++
	}
	if count > 1 {
		return false, fmt.Errorf("ambiguous route readback")
	}
	return count == 1, nil
}

func readManagedVMHARule(ctx context.Context, deps Deps, name string) (map[string]any, error) {
	rows, err := managedVMHARuleRows(ctx, deps)
	if err != nil {
		return nil, err
	}
	return rows[name], nil
}

func managedVMRecordedRoute(record aj.Record, stepID string) (managedVMRouteIdentity, error) {
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.ID != stepID || step.Attempt != record.ActiveAttempt() {
			continue
		}
		var identity managedVMRouteIdentity
		if err := json.Unmarshal(step.Parameters, &identity); err != nil {
			return identity, fmt.Errorf("subnet intent lacks readable identity")
		}
		if identity.Version != 1 || identity.Kind != managedVMRouteKind || identity.VNet == "" || identity.Subnet == "" || identity.CIDR == "" {
			return identity, fmt.Errorf("subnet intent lacks exact identity")
		}
		return identity, nil
	}
	return managedVMRouteIdentity{}, fmt.Errorf("subnet mutation intent is absent")
}
