package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func managedEvidenceObject(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("evidence encoding failed")
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("evidence is not an object")
	}
	return fields, nil
}
func managedEvidenceListMatch(raw any, fields map[string]any) error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var rows []map[string]any
	if err := json.Unmarshal(encoded, &rows); err != nil {
		return fmt.Errorf("malformed policy evidence")
	}
	matched := 0
	for _, row := range rows {
		if managedConfigFieldsMatch(row, fields) == nil {
			matched++
		}
	}
	if matched != 1 {
		return fmt.Errorf("policy readback is missing or ambiguous")
	}
	return nil
}
func (m *managedVMAllocation) observePolicyMutation(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
	method := call.Service + "." + call.Method
	if call.Service == "Pool" {
		return m.observePoolMutation(ctx, call, result)
	}
	if method == "Cluster.DeleteHaRules" {
		return m.observeDeletedHARule(ctx, call)
	}
	if method == "Cluster.CreateSdnVnetsSubnets" || method == "Cluster.UpdateSdn" {
		return m.observeSDNMutation(ctx, call, step, result)
	}
	params, err := managedEvidenceObject(call.Args["params"])
	if err != nil {
		return err
	}
	node, id := m.shape.node, strconv.Itoa(m.vmid)
	switch method {
	case "Cluster.CreateHaResources", "Cluster.UpdateHaResources":
		sid := "vm:" + id
		if value, ok := params["sid"].(string); ok {
			sid = value
		}
		if value, ok := call.Args["sid"].(string); ok {
			sid = value
		}
		if sid != "vm:"+id && sid != id {
			return fmt.Errorf("HA resource differs from planned VM")
		}
		response, err := m.deps.PVE.Cluster().GetHaResources(ctx, sid)
		if err != nil {
			return err
		}
		fields, err := managedEvidenceObject(response)
		if err != nil {
			return err
		}
		return managedConfigFieldsMatch(fields, params)
	case "Cluster.CreateHaRules":
		response, err := m.deps.PVE.Cluster().ListHaRules(ctx, nil)
		if err != nil {
			return err
		}
		return managedEvidenceListMatch(response, params)
	case "Nodes.CreateQemuFirewallIpset":
		response, err := m.deps.PVE.Nodes().ListQemuFirewallIpset(ctx, node, id)
		if err != nil {
			return err
		}
		return managedEvidenceListMatch(response, params)
	case "Nodes.CreateQemuFirewallIpset2":
		name, _ := call.Args["name"].(string)
		cidr, _ := params["cidr"].(string)
		response, err := m.deps.PVE.Nodes().GetQemuFirewallIpset2(ctx, node, id, name, cidr)
		if err != nil {
			return err
		}
		fields, err := managedEvidenceObject(response)
		if err != nil {
			return err
		}
		return managedConfigFieldsMatch(fields, params)
	case "Nodes.CreateQemuFirewallRules":
		response, err := m.deps.PVE.Nodes().ListQemuFirewallRules(ctx, node, id)
		if err != nil {
			return err
		}
		return managedEvidenceListMatch(response, params)
	case "Nodes.UpdateQemuFirewallOptions":
		response, err := m.deps.PVE.Nodes().ListQemuFirewallOptions(ctx, node, id)
		if err != nil {
			return err
		}
		fields, err := managedEvidenceObject(response)
		if err != nil {
			return err
		}
		return managedConfigFieldsMatch(fields, params)
	case "Nodes.CreateQemuAgentExec":
		return m.observeGuestExecution(ctx, result)
	default:
		return fmt.Errorf("mutation evidence unavailable for %s", method)
	}
}

func (m *managedVMAllocation) observePoolMutation(ctx context.Context, call ManagedAllocationMutation, result any) error {
	pool, _ := call.Args["poolID"].(string)
	if call.Method == "CreatePool" {
		comment, found, err := m.deps.PVE.Pools().GetPoolComment(ctx, pool)
		if err != nil {
			return err
		}
		existing, _ := result.(managedExistingPool)
		concurrent := existing.poolID != "" && existing.poolID == pool
		if !found || (!concurrent && comment != call.Args["comment"]) {
			return fmt.Errorf("resource pool creation not observed")
		}
		return nil
	}
	found, err := m.deps.PVE.Pools().PoolHasVM(ctx, pool, int64(m.vmid))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("resource pool membership not observed")
	}
	return nil
}

func (m *managedVMAllocation) observeGuestExecution(ctx context.Context, result any) error {
	node, id := m.shape.node, strconv.Itoa(m.vmid)
	response, ok := result.(*nodes.CreateQemuAgentExecResponse)
	if !ok || response == nil || response.Pid <= 0 {
		return fmt.Errorf("guest execution did not return a process identity")
	}
	bounded, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		status, err := m.deps.PVE.Nodes().ListQemuAgentExecStatus(bounded, node, id, &nodes.ListQemuAgentExecStatusParams{Pid: int64(response.Pid)})
		if err != nil {
			return err
		}
		if status == nil {
			return fmt.Errorf("guest execution readback missing")
		}
		if bool(status.Exited) {
			if status.Exitcode == nil || *status.Exitcode != 0 || status.Signal != nil {
				return fmt.Errorf("guest execution did not complete successfully")
			}
			return nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return bounded.Err()
		case <-timer.C:
		}
	}
}
