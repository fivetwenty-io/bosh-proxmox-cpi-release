package handlers

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func (m *managedVMAllocation) validateMutationTarget(call ManagedAllocationMutation, role string) error {
	method := call.Service + "." + call.Method
	if role != "" {
		if err := m.validateStorageMutationTarget(call, role); err != nil {
			return err
		}
	}
	if method == managedVMCallResize {
		params, ok := call.Args["params"].(*nodes.UpdateQemuResizeParams)
		target, found := managedVMRoleTarget(m.prepared.plan, storageRoleRoot)
		if !ok || !found || params.Disk != m.shape.rootDiskKey || params.Size != fmt.Sprintf("%dG", target.VirtualBytes/(1<<30)) {
			return fmt.Errorf("root expansion differs from frozen plan")
		}
	}
	if method == managedVMCallUpdateConfig {
		params, ok := call.Args["params"].(*nodes.UpdateQemuConfigParams)
		if !ok || params == nil {
			return fmt.Errorf("VM configuration mutation is malformed")
		}
		if params.Description != nil {
			marker, found, err := pve.ParseStorageAllocationMarker(*params.Description)
			expected, _, expectedErr := pve.ParseStorageAllocationMarker(m.marker)
			if err != nil || expectedErr != nil || !found || marker != expected {
				return fmt.Errorf("VM configuration would replace allocation provenance")
			}
		}
		if params.Delete != nil {
			for _, field := range strings.Split(*params.Delete, ",") {
				if field == pveConfigKeyDescription || field == m.shape.rootDiskKey {
					return fmt.Errorf("VM configuration would delete allocation identity or root")
				}
			}
		}
	}
	return nil
}

func (m *managedVMAllocation) validateStorageMutationTarget(call ManagedAllocationMutation, role string) error {
	method := call.Service + "." + call.Method
	target, ok := managedVMRoleTarget(m.prepared.plan, role)
	if !ok {
		return fmt.Errorf("allocation role is not planned")
	}
	switch method {
	case managedVMCallCreate:
		params, ok := call.Args["params"].(map[string]any)
		if !ok || fmt.Sprint(params["vmid"]) != strconv.Itoa(m.vmid) || params[pveConfigKeyDescription] != m.marker {
			return fmt.Errorf("root create identity differs from journal")
		}
		drive, ok := params[m.shape.rootDiskKey].(string)
		if !ok || target.Source == nil || !strings.HasPrefix(drive, target.StorageID+":0,import-from="+target.Source.VolumeID+",") {
			return fmt.Errorf("root import source or destination differs from plan")
		}
	case managedVMCallClone:
		params, ok := call.Args["params"].(*nodes.CreateQemuCloneParams)
		if !ok || params == nil || target.Source == nil || params.Newid != int64(m.vmid) || params.Description == nil || *params.Description != m.marker || call.Args["node"] != target.Source.Node || fmt.Sprint(call.Args["vmid"]) != strconv.Itoa(target.Source.TemplateVMID) {
			return fmt.Errorf("clone source or destination differs from plan")
		}
		full := target.Mechanism == storageMechanismFullClone
		if params.Full == nil || *params.Full != full {
			return fmt.Errorf("clone mechanism differs from plan")
		}
		if full && (params.Storage == nil || *params.Storage != target.StorageID) {
			return fmt.Errorf("clone storage differs from plan")
		}
	case managedVMCallCreateVolume:
		definition, found := m.prepared.plan.Definitions[target.StorageID]
		format, err := pve.EphemeralVolumeFormat(definition.Type, m.shape.vmDiskFormat)
		if err != nil {
			return err
		}
		name, _, err := pve.ManagedEphemeralVolumeName(definition.Type, format, m.vmid, m.handle.Record().Namespace, m.handle.Record().ID)
		if !found || err != nil {
			return fmt.Errorf("ephemeral naming lacks a supported frozen storage definition")
		}
		size, ok := call.Args["sizeGiB"].(int)
		if !ok || size <= 0 || uint64(size)*(1<<30) != target.VirtualBytes || call.Args["storageName"] != target.StorageID || call.Args["format"] != format || call.Args["name"] != name {
			return fmt.Errorf("ephemeral allocation differs from plan")
		}
	case managedVMCallUpload:
		if call.Args["storageName"] != target.StorageID || call.Args["content"] != storageRoleISO || call.Args["filename"] != fmt.Sprintf("vm-%d-config.iso", m.vmid) {
			return fmt.Errorf("ISO upload differs from plan")
		}
	}
	return nil
}
