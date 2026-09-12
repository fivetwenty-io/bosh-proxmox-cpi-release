package handlers

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// A handoff never turns an arbitrary preexisting route into an owned route. It
// retains the original immutable create proof and names each continuing user.
func managedVMRouteCleanupIdentities(journal *aj.Journal, record aj.Record) ([]managedVMRouteIdentity, error) {
	routes := map[string]managedVMRouteIdentity{}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt != record.ActiveAttempt() || step.Kind != "vm.Cluster.CreateSdnVnetsSubnets" {
			continue
		}
		if step.State != aj.Observed {
			return nil, fmt.Errorf("subnet cleanup cannot resolve an unobserved create")
		}
		identity, err := managedVMRecordedRoute(record, step.ID)
		if err != nil {
			return nil, err
		}
		identity.SourceAllocation = record.ID
		identity.SourceStep = step.ID
		routes[identity.VNet+"/"+identity.Subnet] = identity
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return nil, err
	}
	if plan.VMExecution == nil {
		return sortedManagedVMRoutes(routes), nil
	}
	wanted := map[string]bool{}
	for _, ref := range parseAdvertisedRouteTags(plan.VMExecution.Tags) {
		wanted[ref.tag] = true
	}
	if len(wanted) == 0 {
		return sortedManagedVMRoutes(routes), nil
	}
	records, err := journal.List()
	if err != nil {
		return nil, err
	}
	for i := range records {
		candidate := &records[i]
		for si := range candidate.Steps {
			step := &candidate.Steps[si]
			if step.Kind != "vm.route.retained_shared" || step.State != aj.Observed {
				continue
			}
			var fields map[string]any
			if err := json.Unmarshal(step.Parameters, &fields); err != nil {
				return nil, err
			}
			users, _ := fields["resources"].(string)
			if !slices.Contains(strings.Split(users, ","), "allocation:"+record.ID) {
				continue
			}
			var identity managedVMRouteIdentity
			if err := json.Unmarshal(step.Parameters, &identity); err != nil {
				return nil, err
			}
			if !wanted[advertisedRouteTag(identity.VNet, identity.CIDR)] {
				continue
			}
			if err := verifyManagedVMRouteOrigin(journal, identity); err != nil {
				return nil, err
			}
			key := identity.VNet + "/" + identity.Subnet
			if previous, ok := routes[key]; ok && (previous.SourceAllocation != identity.SourceAllocation || previous.SourceStep != identity.SourceStep) {
				return nil, fmt.Errorf("route creation ancestry is ambiguous")
			}
			routes[key] = identity
		}
	}
	return sortedManagedVMRoutes(routes), nil
}

func verifyManagedVMRouteOrigin(journal *aj.Journal, identity managedVMRouteIdentity) error {
	if identity.SourceAllocation == "" || identity.SourceStep == "" {
		return fmt.Errorf("route handoff lacks original creation proof")
	}
	records, err := journal.List()
	if err != nil {
		return err
	}
	for i := range records {
		if managedVMRouteWasDeleted(records[i], identity) {
			return fmt.Errorf("route handoff origin has a deletion disposition")
		}
	}
	origin, err := journal.Inspect(identity.SourceAllocation)
	if err != nil {
		return err
	}
	if origin.Kind != "vm" {
		return fmt.Errorf("route handoff origin is not a VM allocation")
	}
	found := false
	for i := range origin.Steps {
		step := &origin.Steps[i]
		if step.ID != identity.SourceStep {
			continue
		}
		if step.Kind != "vm.Cluster.CreateSdnVnetsSubnets" || step.State != aj.Observed {
			return fmt.Errorf("route handoff origin is not observed creation")
		}
		var created managedVMRouteIdentity
		if err := json.Unmarshal(step.Parameters, &created); err != nil {
			return err
		}
		if created.Version != 1 || created.Kind != managedVMRouteKind || created.VNet != identity.VNet || created.Subnet != identity.Subnet || created.CIDR != identity.CIDR {
			return fmt.Errorf("route handoff differs from original creation")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("route handoff creation proof is absent")
	}
	return nil
}

func sortedManagedVMRoutes(routes map[string]managedVMRouteIdentity) []managedVMRouteIdentity {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]managedVMRouteIdentity, 0, len(keys))
	for _, key := range keys {
		result = append(result, routes[key])
	}
	return result
}

// Any deletion intent ends transferable authority, even if its response was lost.
func managedVMRouteWasDeleted(record aj.Record, identity managedVMRouteIdentity) bool {
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Kind != "vm.route.delete" && step.Kind != "vm.route.apply_delete" {
			continue
		}
		var disposed managedVMRouteIdentity
		if json.Unmarshal(step.Parameters, &disposed) != nil {
			continue
		}
		if disposed.VNet != identity.VNet || disposed.Subnet != identity.Subnet {
			continue
		}
		if disposed.SourceAllocation == identity.SourceAllocation && disposed.SourceStep == identity.SourceStep {
			return true
		}
	}
	return false
}
