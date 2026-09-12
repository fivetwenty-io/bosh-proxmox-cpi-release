package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cleanupManagedVMInfrastructure disposes only subnets whose creation has exact
// observed journal evidence. Shared subnets remain in use and receive a durable
// retention disposition. HA rule membership is disposed atomically by HA purge.
func cleanupManagedVMInfrastructure(ctx context.Context, deps Deps, journal *aj.Journal, handle *aj.Handle, node string, vmid int) error {
	record := handle.Record()
	routes, err := managedVMRouteCleanupIdentities(journal, record)
	if err != nil {
		return err
	}
	if len(routes) == 0 {
		return nil
	}
	if node == "" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			return err
		}
		node = plan.Node
	}
	if node == "" || vmid <= 0 {
		return fmt.Errorf("subnet cleanup lacks recorded VM coordinates")
	}
	shared, err := managedVMRouteUsers(ctx, deps, journal, record.Namespace, vmid)
	if err != nil {
		return err
	}
	target := aj.Target{Node: node, VMID: vmid}
	for _, identity := range routes {
		holders := shared[advertisedRouteTag(identity.VNet, identity.CIDR)]
		if len(holders) > 0 {
			if err := recordManagedVMSharedRoute(handle, target, identity, holders); err != nil {
				return err
			}
			continue
		}
		if err := deleteManagedVMRoute(ctx, deps, handle, target, identity); err != nil {
			return err
		}
	}
	return nil
}

func managedVMRouteUsers(ctx context.Context, deps Deps, journal *aj.Journal, namespace string, vmid int) (map[string][]string, error) {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, deps.Log(ctx))
	if err != nil {
		return nil, err
	}
	shared := map[string][]string{}
	for _, guest := range guests {
		if guest.VMID == vmid {
			continue
		}
		cfg, err := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			return nil, fmt.Errorf("route user configuration is unreadable")
		}
		tags, _ := pve.ConfigString(cfg, "tags")
		refs := parseAdvertisedRouteTags(tags)
		if len(refs) == 0 {
			continue
		}
		description, _ := pve.ConfigString(cfg, "description")
		marker, found, err := pve.ParseStorageAllocationMarker(description)
		if err != nil {
			return nil, err
		}
		holder := "vm:" + strconv.Itoa(guest.VMID)
		if found && marker.Namespace == namespace && marker.Kind == "vm" {
			owner, err := journal.Inspect(marker.AllocationID)
			if err != nil {
				return nil, err
			}
			if owner.Namespace != namespace || owner.Kind != "vm" || marker.AgentSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(owner.AgentID))) || !managedVMRouteOwnerMatches(owner, guest.VMID) {
				return nil, fmt.Errorf("route user allocation authority differs from its VM")
			}
			holder = "allocation:" + marker.AllocationID
		}
		for _, ref := range refs {
			shared[ref.tag] = append(shared[ref.tag], holder)
		}
	}
	for tag := range shared {
		slices.Sort(shared[tag])
		shared[tag] = slices.Compact(shared[tag])
	}
	return shared, nil
}

func managedVMRouteOwnerMatches(record aj.Record, vmid int) bool {
	if record.State == aj.Deleted || record.State == aj.Cleaned || record.State == aj.VMDeletedRetained {
		return false
	}
	if record.CID != "" {
		return record.CID == strconv.Itoa(vmid)
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && !step.Target.External && step.Target.VMID == vmid && !strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") {
			return true
		}
	}
	return false
}

func recordManagedVMSharedRoute(handle *aj.Handle, target aj.Target, identity managedVMRouteIdentity, holders []string) error {
	fields, err := managedEvidenceObject(identity)
	if err != nil {
		return err
	}
	fields["resources"] = strings.Join(holders, ",")
	parameters, err := aj.MutationParameters(fields)
	if err != nil {
		return err
	}
	step, err := storageMutationIntent(handle, "vm.route.retained_shared", target, nil, parameters)
	if err != nil {
		return err
	}
	return storageMutationObserved(handle, step, nil, false)
}

func deleteManagedVMRoute(ctx context.Context, deps Deps, handle *aj.Handle, target aj.Target, identity managedVMRouteIdentity) error {
	if err := storageCleanupSettled(ctx, handle.Record()); err != nil {
		return err
	}
	present, err := observeManagedVMRoute(ctx, deps, identity, false)
	if err != nil {
		return err
	}
	parameters, err := aj.MutationParameters(identity)
	if err != nil {
		return err
	}
	if present {
		if managedVMRouteWasDeleted(handle.Record(), identity) {
			return fmt.Errorf("disposed subnet identity has reappeared")
		}
		step, err := storageMutationIntent(handle, "vm.route.delete", target, nil, parameters)
		if err != nil {
			return err
		}
		if err := deps.PVE.Cluster().DeleteSdnVnetsSubnets(ctx, identity.VNet, identity.Subnet, nil); err != nil {
			return err
		}
		remaining, err := observeManagedVMRoute(ctx, deps, identity, false)
		if err != nil {
			return err
		}
		if remaining {
			return fmt.Errorf("subnet deletion was not observed")
		}
		if err := storageMutationObserved(handle, step, nil, false); err != nil {
			return err
		}
	}
	// A process can stop after deleting pending configuration but before apply.
	// The running-state check distinguishes that prefix from finished cleanup.
	running, err := observeManagedVMRoute(ctx, deps, identity, true)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	step, err := storageMutationIntent(handle, "vm.route.apply_delete", target, nil, parameters)
	if err != nil {
		return err
	}
	result, err := deps.PVE.Cluster().UpdateSdn(ctx, nil)
	if err != nil {
		return err
	}
	if err := awaitManagedVMSDN(ctx, deps, handle, step, result); err != nil {
		return err
	}
	remaining, err := observeManagedVMRoute(ctx, deps, identity, true)
	if err != nil {
		return err
	}
	if remaining {
		return fmt.Errorf("deleted subnet still present in running configuration")
	}
	return storageMutationObserved(handle, step, nil, false)
}

func awaitManagedVMSDN(ctx context.Context, deps Deps, handle *aj.Handle, step string, result *json.RawMessage) error {
	if result == nil || len(*result) == 0 || string(*result) == "null" || string(*result) == `""` {
		return nil
	}
	upid, err := managedMutationUPID(result)
	if err != nil {
		return err
	}
	if err := storageMutationSubmitted(handle, step, upid); err != nil {
		return err
	}
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[1] == "" {
		return fmt.Errorf("SDN task node not proven")
	}
	return pve.AwaitTaskWithLogger(ctx, deps.PVE, parts[1], upid, deps.Log(ctx))
}

func managedVMHARulesForResource(ctx context.Context, deps Deps, vmid int) (map[string]map[string]any, error) {
	rows, err := managedVMHARuleRows(ctx, deps)
	if err != nil {
		return nil, err
	}
	result := map[string]map[string]any{}
	for name, fields := range rows {
		encoded, err := json.Marshal(fields["resources"])
		if err != nil {
			return nil, err
		}
		if _, found := parseHaResources(encoded)[haResourceSid(vmid)]; found {
			result[name] = fields
		}
	}
	return result, nil
}

func observeManagedVMHAPurge(ctx context.Context, deps Deps, vmid int, before map[string]map[string]any) error {
	remaining, err := managedVMHARulesForResource(ctx, deps, vmid)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("HA purge left recorded VM membership")
	}
	for name, fields := range before {
		raw, err := json.Marshal(fields["resources"])
		if err != nil {
			return err
		}
		members := parseHaResources(raw)
		delete(members, haResourceSid(vmid))
		current, err := readManagedVMHARule(ctx, deps, name)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			if current != nil {
				return fmt.Errorf("HA purge left a sole-member rule")
			}
			continue
		}
		if current == nil {
			return fmt.Errorf("HA purge removed a shared rule")
		}
		raw, err = json.Marshal(current["resources"])
		if err != nil {
			return err
		}
		actual := parseHaResources(raw)
		for member := range members {
			if _, ok := actual[member]; !ok {
				return fmt.Errorf("HA purge removed another resource membership")
			}
		}
		// Removing this VM does not authorize changing shared rule policy.
		for _, key := range []string{"type", "affinity", "nodes", "strict", "disable", "comment"} {
			if fmt.Sprint(current[key]) != fmt.Sprint(fields[key]) {
				return fmt.Errorf("HA purge changed shared rule policy")
			}
		}
	}
	return nil
}

func recordManagedVMHAPurgeRules(handle *aj.Handle, target aj.Target, before map[string]map[string]any) ([]string, error) {
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	slices.Sort(names)
	steps := make([]string, 0, len(names))
	for _, name := range names {
		old := before[name]
		fields := map[string]any{"version": 1, "kind": "ha_rule_purge", "rule": name}
		for _, key := range []string{"type", "affinity", "nodes", "strict", "disable", "comment"} {
			if value, found := old[key]; found {
				fields[key] = value
			}
		}
		raw, err := json.Marshal(old["resources"])
		if err != nil {
			return nil, err
		}
		members := parseHaResources(raw)
		fields["original_resources"] = sidsCSV(members)
		delete(members, haResourceSid(target.VMID))
		fields["desired_resources"] = sidsCSV(members)
		parameters, err := aj.MutationParameters(fields)
		if err != nil {
			return nil, err
		}
		step, err := storageMutationIntent(handle, "vm.ha.purge_rule", target, nil, parameters)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}
