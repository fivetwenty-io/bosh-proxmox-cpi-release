package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// Preserve the existing optional VM tag attribution, with the same journal and
// poisoned-write boundary as the volume and parker mutations.
func (m *managedDiskRequest) applyTags(ctx context.Context, handle *aj.Handle, cid, volume string) error {
	if m.hint == "" || len(m.tags) == 0 {
		return nil
	}
	node, err := managedDiskHintNode(ctx, m.deps, m.hint)
	if err != nil {
		return err
	}
	guard, err := NewManagedAllocationGuard(m.deps.PVE, ManagedAllocationHooks{
		Before: func(ctx context.Context, call ManagedAllocationMutation) (string, error) {
			if call.Service != managedServiceNodes || call.Method != "UpdateQemuConfig" {
				return "", fmt.Errorf("unplanned disk metadata mutation")
			}
			current, err := managedDiskHintNode(ctx, m.deps, m.hint)
			if err != nil || current != node {
				return "", fmt.Errorf("hinted VM moved before metadata mutation")
			}
			if err := m.revalidate(ctx); err != nil {
				return "", err
			}
			if _, err := managedDiskMetadataFields(call.Args["params"]); err != nil {
				return "", err
			}
			n, vmid, err := managedDiskParkIdentity(call, node)
			if err != nil {
				return "", err
			}
			if n != node || fmt.Sprint(vmid) != m.hint {
				return "", fmt.Errorf("disk metadata targeted another VM")
			}
			return storageMutationIntent(handle, "persistent_vm_metadata", aj.Target{Node: n, VMID: vmid, Storage: m.plan.Targets[0].StorageID, Backing: m.plan.Targets[0].BackingKey, IntendedVolume: volume}, nil)
		},
		After: func(ctx context.Context, call ManagedAllocationMutation, step string, _ any) error {
			fields, err := managedDiskMetadataFields(call.Args["params"])
			if err != nil {
				return err
			}
			n, vmid, err := managedDiskParkIdentity(call, node)
			if err != nil {
				return err
			}
			config, err := m.deps.PVE.QEMU().Config(ctx, n, vmid)
			if err != nil {
				return err
			}
			for key, want := range fields {
				if key == "digest" {
					continue
				}
				if got, ok := config[key]; !ok || managedDiskScalar(got) != managedDiskScalar(want) {
					return fmt.Errorf("disk metadata readback mismatch")
				}
			}
			return storageMutationObserved(handle, step, nil, false)
		},
		Failed: func(context.Context, ManagedAllocationMutation, string, error) error {
			return storageAllocationUncertain(handle, "persistent VM metadata")
		},
	})
	if err != nil {
		return err
	}
	deps := m.deps
	deps.PVE = guard.Client()
	applyCreateDiskTags(ctx, deps, node, m.hint, cid, m.tags)
	return guard.Err()
}

func managedDiskMetadataFields(params any) (map[string]any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty disk metadata mutation")
	}
	for key := range fields {
		if key != pveConfigKeyDescription && key != "tags" && key != "digest" {
			return nil, fmt.Errorf("unplanned disk metadata field")
		}
	}
	return fields, nil
}
