package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func (m *managedDiskRequest) park(ctx context.Context, handle *aj.Handle, cid, volume string) error {
	cfg := parkerWriteConfigFor(m.deps)
	cfg.DiskStorage = m.plan.Targets[0].StorageID
	pctx := pve.ParkContext{DiskCID: cid, StableID: m.token, Opts: m.opts, AllocationID: m.id, AllocationNamespace: m.plan.Namespace, AllocationBacking: m.plan.Targets[0].BackingKey}
	guard, err := NewManagedAllocationGuard(m.deps.PVE, ManagedAllocationHooks{
		Before: func(ctx context.Context, call ManagedAllocationMutation) (string, error) {
			if err := m.revalidate(ctx); err != nil {
				return "", err
			}
			if err := m.observeVolume(ctx, volume); err != nil {
				return "", err
			}
			node, vmid, err := managedDiskParkIdentity(call, m.plan.Node)
			if err != nil {
				return "", err
			}
			switch call.Service + "." + call.Method {
			case "QEMU.Create":
				params, ok := call.Args["params"].(map[string]any)
				if !ok {
					return "", fmt.Errorf("invalid parker creation parameters")
				}
				entry, err := json.Marshal(map[string]any{m.token: map[string]any{"disk_cid": cid, "allocation_id": m.id, "allocation_namespace": m.plan.Namespace, "allocation_backing": m.plan.Targets[0].BackingKey, "volid": volume, resourceTypeNode: node}})
				if err != nil {
					return "", err
				}
				description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": entry})
				if err != nil {
					return "", err
				}
				params[pveConfigKeyDescription] = description
			case "QEMU.AttachDisk":
				v, ok := call.Args["volid"].(string)
				if !ok || strings.Split(v, ",")[0] != volume {
					return "", fmt.Errorf("parker attempted to attach an unplanned volume")
				}
			case "Nodes.UpdateQemuConfig":
				if _, err := managedDiskParkUpdateFields(call.Args["params"]); err != nil {
					return "", err
				}
			case "Pool.CreatePool", "Pool.DeletePool":
				pool, _ := call.Args["poolID"].(string)
				if !strings.HasPrefix(pool, "bosh-lock-") {
					return "", fmt.Errorf("unexpected parker pool mutation")
				}
			default:
				return "", fmt.Errorf("unplanned parker mutation %s.%s", call.Service, call.Method)
			}
			return storageMutationIntent(handle, "park_"+call.Service+"_"+call.Method, aj.Target{Node: node, VMID: vmid, Storage: m.plan.Targets[0].StorageID, Backing: m.plan.Targets[0].BackingKey, IntendedVolume: volume}, nil)
		},
		After: func(ctx context.Context, call ManagedAllocationMutation, step string, result any) error {
			if err := m.observeParkMutation(ctx, handle, call, step, result, volume); err != nil {
				return err
			}
			return storageMutationObserved(handle, step, nil, false)
		},
		Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
			return storageAllocationUncertain(handle, "parker "+call.Service+"."+call.Method)
		},
	})
	if err != nil {
		return err
	}
	if err := pve.ParkDisk(ctx, guard.Client(), m.deps.Log(ctx), m.plan.Node, volume, cfg, pctx); err != nil {
		return err
	}
	if err := guard.Err(); err != nil {
		return err
	}
	return pve.VerifyAllocationParked(ctx, m.deps.PVE, m.deps.Log(ctx), volume, m.token, m.plan.Namespace, m.id, cfg)
}

func managedDiskParkIdentity(call ManagedAllocationMutation, defaultNode string) (string, int, error) {
	node, _ := call.Args[resourceTypeNode].(string)
	if node == "" {
		node = defaultNode
	}
	value := call.Args[metadataKeyVMID]
	if call.Method == "Create" {
		if p, ok := call.Args["params"].(map[string]any); ok {
			value = p[metadataKeyVMID]
		}
	}
	if value == nil && call.Service == "Pool" {
		return node, 0, nil
	}
	vmid, err := strconv.Atoi(fmt.Sprint(value))
	if err != nil || vmid <= 0 {
		return "", 0, fmt.Errorf("parker mutation lacks exact VMID")
	}
	return node, vmid, nil
}

func managedDiskParkUpdateFields(params any) (map[string]any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty parker update")
	}
	for key := range fields {
		if key != pveConfigKeyDescription && key != "protection" && key != "digest" {
			return nil, fmt.Errorf("unplanned parker configuration field %q", key)
		}
	}
	return fields, nil
}

func (m *managedDiskRequest) observeParkMutation(ctx context.Context, handle *aj.Handle, call ManagedAllocationMutation, step string, result any, volume string) error {
	node, vmid, err := managedDiskParkIdentity(call, m.plan.Node)
	if err != nil {
		return err
	}
	if call.Service == "Pool" {
		return m.observeParkPool(ctx, call)
	}
	if call.Service == managedServiceQEMU && call.Method == "Create" {
		upid, ok := result.(string)
		if !ok || upid == "" {
			return fmt.Errorf("parker creation returned no task identity")
		}
		if err := storageMutationSubmitted(handle, step, upid); err != nil {
			return err
		}
		if err := pve.AwaitTask(ctx, m.deps.PVE, node, upid); err != nil {
			return err
		}
	}
	config, err := m.deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return err
	}
	if config == nil {
		return fmt.Errorf("nil parker mutation readback")
	}
	if call.Method == "AttachDisk" {
		slot, ok := result.(string)
		if !ok || slot == "" {
			return fmt.Errorf("parker attach returned no disk slot")
		}
		drive, ok := pve.ConfigString(config, slot)
		token, has := pve.StableIDFromDriveOptStr(drive)
		if !ok || strings.Split(drive, ",")[0] != volume || !has || token != m.token {
			return fmt.Errorf("parker attachment readback mismatch")
		}
		return nil
	}
	var fields map[string]any
	if call.Method == "Create" {
		var ok bool
		fields, ok = call.Args["params"].(map[string]any)
		if !ok || fields == nil {
			return fmt.Errorf("parker create parameters are invalid")
		}
	} else {
		fields, err = managedDiskParkUpdateFields(call.Args["params"])
		if err != nil {
			return err
		}
	}
	for key, want := range fields {
		if key == metadataKeyVMID || key == "digest" {
			continue
		}
		got, ok := config[key]
		if !ok && (key == "onboot" && managedDiskScalar(want) == "0") {
			continue
		}
		if !ok || managedDiskScalar(got) != managedDiskScalar(want) {
			return fmt.Errorf("parker %s readback mismatch", key)
		}
	}
	return nil
}

func managedDiskScalar(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "1"
		}
		return "0"
	}
	return fmt.Sprint(v)
}

func (m *managedDiskRequest) observeParkPool(ctx context.Context, call ManagedAllocationMutation) error {
	pool, _ := call.Args["poolID"].(string)
	comment, found, err := m.deps.PVE.Pools().GetPoolComment(ctx, pool)
	if err != nil {
		return err
	}
	if call.Method == "CreatePool" {
		want, _ := call.Args["comment"].(string)
		if !found || comment != want {
			return fmt.Errorf("parker lock ownership readback mismatch")
		}
	} else if found {
		return fmt.Errorf("parker lock deletion not observed")
	}
	return nil
}
