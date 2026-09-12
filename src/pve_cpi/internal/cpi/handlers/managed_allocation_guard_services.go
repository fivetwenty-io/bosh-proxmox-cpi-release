// Audited CPI mutation service adapters. Maintain alongside production call sites.
package handlers

import (
	"context"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"io"
)

type managedQEMUService struct {
	qemu.Service
	guard *ManagedAllocationGuard
}

func (c *managedAllocationClient) QEMU() qemu.Service {
	return &managedQEMUService{Service: c.Client.QEMU(), guard: c.guard}
}

type managedNodesService struct {
	nodes.Service
	guard *ManagedAllocationGuard
}

func (c *managedAllocationClient) Nodes() nodes.Service {
	return &managedNodesService{Service: c.Client.Nodes(), guard: c.guard}
}

type managedStorageService struct {
	storage.Service
	guard *ManagedAllocationGuard
}

func (c *managedAllocationClient) Storage() storage.Service {
	return &managedStorageService{Service: c.Client.Storage(), guard: c.guard}
}

type managedClusterService struct {
	cluster.Service
	guard *ManagedAllocationGuard
}

func (c *managedAllocationClient) Cluster() cluster.Service {
	return &managedClusterService{Service: c.Client.Cluster(), guard: c.guard}
}

type managedPoolService struct {
	pve.PoolService
	guard *ManagedAllocationGuard
}

func (c *managedAllocationClient) Pools() pve.PoolService {
	return &managedPoolService{PoolService: c.Client.Pools(), guard: c.guard}
}
func (t *managedQEMUService) Create(ctx context.Context, node string, params map[string]interface{}) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Create", Args: map[string]any{resourceTypeNode: node, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Create(ctx, node, params)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) Start(ctx context.Context, node string, vmid int) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Start", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Start(ctx, node, vmid)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) Stop(ctx context.Context, node string, vmid int) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Stop", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Stop(ctx, node, vmid)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) Reset(ctx context.Context, node string, vmid int) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Reset", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Reset(ctx, node, vmid)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) AttachDisk(ctx context.Context, node string, vmid int, volid string, bus string, opts *qemu.AttachOpts) (diskID string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "AttachDisk", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, "volid": volid, "bus": bus, "opts": opts}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	diskID, err = t.Service.AttachDisk(ctx, node, vmid, volid, bus, opts)
	err = t.guard.finish(ctx, m, token, diskID, err)
	return
}
func (t *managedQEMUService) DetachDisk(ctx context.Context, node string, vmid int, diskID string) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "DetachDisk", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, "diskID": diskID}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DetachDisk(ctx, node, vmid, diskID)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedQEMUService) ResizeDisk(ctx context.Context, node string, vmid int, diskID string, sizeGiB int) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "ResizeDisk", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, "diskID": diskID, "sizeGiB": sizeGiB}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.ResizeDisk(ctx, node, vmid, diskID, sizeGiB)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) Snapshot(ctx context.Context, node string, vmid int, name string, opts map[string]interface{}) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Snapshot", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, metadataKeyName: name, "opts": opts}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Snapshot(ctx, node, vmid, name, opts)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedQEMUService) DeleteSnapshot(ctx context.Context, node string, vmid int, name string) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "DeleteSnapshot", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, metadataKeyName: name}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteSnapshot(ctx, node, vmid, name)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedStorageService) CreateVolume(ctx context.Context, node, storageName string, sizeGiB int, format string, vmid int, name string) (volid string, err error) {
	m := ManagedAllocationMutation{Service: managedDiskServiceStorage, Method: "CreateVolume", Args: map[string]any{resourceTypeNode: node, managedArgumentStorageName: storageName, "sizeGiB": sizeGiB, "format": format, metadataKeyVMID: vmid, metadataKeyName: name}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	volid, err = t.Service.CreateVolume(ctx, node, storageName, sizeGiB, format, vmid, name)
	err = t.guard.finish(ctx, m, token, volid, err)
	return
}
func (t *managedStorageService) DeleteVolumeAsync(ctx context.Context, node, storageName, volume string) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedDiskServiceStorage, Method: "DeleteVolumeAsync", Args: map[string]any{resourceTypeNode: node, managedArgumentStorageName: storageName, "volume": volume}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.DeleteVolumeAsync(ctx, node, storageName, volume)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedStorageService) DeleteVolumeIfExists(ctx context.Context, node, storageName, volume string) (bool, error) {
	existed, _, err := t.DeleteVolumeIfExistsAsync(ctx, node, storageName, volume)
	return existed, err
}

func (t *managedStorageService) DeleteVolumeIfExistsAsync(ctx context.Context, node, storageName, volume string) (existed bool, upid string, err error) {
	m := ManagedAllocationMutation{Service: managedDiskServiceStorage, Method: "DeleteVolumeIfExistsAsync", Args: map[string]any{resourceTypeNode: node, managedArgumentStorageName: storageName, "volume": volume}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	existed, upid, err = t.Service.DeleteVolumeIfExistsAsync(ctx, node, storageName, volume)
	err = t.guard.finish(ctx, m, token, []any{existed, upid}, err)
	return
}
func (t *managedStorageService) Upload(ctx context.Context, node, storageName, content, filename string, body io.Reader) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedDiskServiceStorage, Method: "Upload", Args: map[string]any{resourceTypeNode: node, managedArgumentStorageName: storageName, "content": content, "filename": filename, "body": body}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Upload(ctx, node, storageName, content, filename, body)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}
func (t *managedNodesService) CreateNetwork(ctx context.Context, node string, params *nodes.CreateNetworkParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateNetwork", Args: map[string]any{resourceTypeNode: node, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateNetwork(ctx, node, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) UpdateNetwork(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (resp *nodes.UpdateNetworkResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateNetwork", Args: map[string]any{resourceTypeNode: node, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.UpdateNetwork(ctx, node, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (resp *nodes.DeleteQemuResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "DeleteQemu", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.DeleteQemu(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) CreateQemuAgentExec(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentExecParams) (resp *nodes.CreateQemuAgentExecResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuAgentExec", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuAgentExec(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) CreateQemuClone(ctx context.Context, node string, vmid string, params *nodes.CreateQemuCloneParams) (resp *nodes.CreateQemuCloneResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuClone", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuClone(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.UpdateQemuConfig(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) CreateQemuStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (resp *nodes.CreateQemuStatusRebootResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuStatusReboot", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuStatusReboot(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) CreateQemuTemplate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTemplateParams) (resp *nodes.CreateQemuTemplateResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuTemplate", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuTemplate(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) UpdateQemuUnlink(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuUnlink", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.UpdateQemuUnlink(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}

//nolint:revive // Method spelling is required by the upstream nodes.Service interface.
func (t *managedNodesService) CreateStorageDownloadUrl(ctx context.Context, node string, storageName string, params *nodes.CreateStorageDownloadUrlParams) (resp *nodes.CreateStorageDownloadUrlResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateStorageDownloadUrl", Args: map[string]any{resourceTypeNode: node, managedArgumentStorageName: storageName, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateStorageDownloadUrl(ctx, node, storageName, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedNodesService) DeleteNetwork2(ctx context.Context, node string, iface string) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "DeleteNetwork2", Args: map[string]any{resourceTypeNode: node, "iface": iface}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteNetwork2(ctx, node, iface)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) CreateQemuFirewallIpset(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallIpsetParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuFirewallIpset", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateQemuFirewallIpset(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) CreateQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, params *nodes.CreateQemuFirewallIpset2Params) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuFirewallIpset2", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, metadataKeyName: name, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateQemuFirewallIpset2(ctx, node, vmid, name, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) CreateQemuFirewallRules(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallRulesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuFirewallRules", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateQemuFirewallRules(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedNodesService) UpdateQemuFirewallOptions(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuFirewallOptionsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuFirewallOptions", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.UpdateQemuFirewallOptions(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) CreateSdnZones(ctx context.Context, params *cluster.CreateSdnZonesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "CreateSdnZones", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateSdnZones(ctx, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) DeleteSdnZones(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "DeleteSdnZones", Args: map[string]any{"zone": zone, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteSdnZones(ctx, zone, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) CreateSdnVnets(ctx context.Context, params *cluster.CreateSdnVnetsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "CreateSdnVnets", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateSdnVnets(ctx, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) DeleteSdnVnets(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "DeleteSdnVnets", Args: map[string]any{managedArgumentVnet: vnet, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteSdnVnets(ctx, vnet, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) CreateSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "CreateSdnVnetsSubnets", Args: map[string]any{managedArgumentVnet: vnet, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateSdnVnetsSubnets(ctx, vnet, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) DeleteSdnVnetsSubnets(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "DeleteSdnVnetsSubnets", Args: map[string]any{managedArgumentVnet: vnet, "subnet": subnet, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteSdnVnetsSubnets(ctx, vnet, subnet, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) UpdateSdn(ctx context.Context, params *cluster.UpdateSdnParams) (resp *cluster.UpdateSdnResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "UpdateSdn", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.UpdateSdn(ctx, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
func (t *managedClusterService) CreateHaResources(ctx context.Context, params *cluster.CreateHaResourcesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "CreateHaResources", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateHaResources(ctx, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) UpdateHaResources(ctx context.Context, sid string, params *cluster.UpdateHaResourcesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "UpdateHaResources", Args: map[string]any{"sid": sid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.UpdateHaResources(ctx, sid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) DeleteHaResources(ctx context.Context, sid string, params *cluster.DeleteHaResourcesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "DeleteHaResources", Args: map[string]any{"sid": sid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteHaResources(ctx, sid, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) CreateHaRules(ctx context.Context, params *cluster.CreateHaRulesParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "CreateHaRules", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.CreateHaRules(ctx, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) DeleteHaRules(ctx context.Context, rule string) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "DeleteHaRules", Args: map[string]any{"rule": rule}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.DeleteHaRules(ctx, rule)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedClusterService) UpdateOptions(ctx context.Context, params *cluster.UpdateOptionsParams) (err error) {
	m := ManagedAllocationMutation{Service: managedServiceCluster, Method: "UpdateOptions", Args: map[string]any{managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.Service.UpdateOptions(ctx, params)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedPoolService) AddVM(ctx context.Context, poolID string, vmid int64) (err error) {
	m := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "AddVM", Args: map[string]any{managedArgumentPoolID: poolID, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.PoolService.AddVM(ctx, poolID, vmid)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedPoolService) MoveVMToPool(ctx context.Context, poolID string, vmid int64) (err error) {
	m := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "MoveVMToPool", Args: map[string]any{managedArgumentPoolID: poolID, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.PoolService.MoveVMToPool(ctx, poolID, vmid)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedPoolService) CreatePool(ctx context.Context, poolID, comment string) (err error) {
	m := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "CreatePool", Args: map[string]any{managedArgumentPoolID: poolID, "comment": comment}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.PoolService.CreatePool(ctx, poolID, comment)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}
func (t *managedPoolService) DeletePool(ctx context.Context, poolID string) (err error) {
	m := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "DeletePool", Args: map[string]any{managedArgumentPoolID: poolID}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	err = t.PoolService.DeletePool(ctx, poolID)
	err = t.guard.finish(ctx, m, token, nil, err)
	return
}

func (t *managedQEMUService) Clone(ctx context.Context, node string, vmid int, params map[string]interface{}) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Clone", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Clone(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}

func (t *managedQEMUService) Template(ctx context.Context, node string, vmid int) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "Template", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.Template(ctx, node, vmid)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}

func (t *managedQEMUService) RollbackSnapshot(ctx context.Context, node string, vmid int, name string) (upid string, err error) {
	m := ManagedAllocationMutation{Service: managedServiceQEMU, Method: "RollbackSnapshot", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, metadataKeyName: name}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	upid, err = t.Service.RollbackSnapshot(ctx, node, vmid, name)
	err = t.guard.finish(ctx, m, token, upid, err)
	return
}

// DeleteVolume retains the asynchronous deletion task through the guarded path.
func (t *managedStorageService) DeleteVolume(ctx context.Context, node, storageName, volume string) error {
	_, err := t.DeleteVolumeAsync(ctx, node, storageName, volume)
	return err
}

func (t *managedNodesService) CreateQemuMoveDisk(ctx context.Context, node, vmid string, params *nodes.CreateQemuMoveDiskParams) (resp *nodes.CreateQemuMoveDiskResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuMoveDisk", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuMoveDisk(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}

func (t *managedNodesService) CreateQemuMigrate(ctx context.Context, node, vmid string, params *nodes.CreateQemuMigrateParams) (resp *nodes.CreateQemuMigrateResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuMigrate", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuMigrate(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}

func (t *managedNodesService) CreateQemuStatusStop(ctx context.Context, node, vmid string, params *nodes.CreateQemuStatusStopParams) (resp *nodes.CreateQemuStatusStopResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuStatusStop", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.CreateQemuStatusStop(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}

func (t *managedNodesService) UpdateQemuResize(ctx context.Context, node, vmid string, params *nodes.UpdateQemuResizeParams) (resp *nodes.UpdateQemuResizeResponse, err error) {
	m := ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuResize", Args: map[string]any{resourceTypeNode: node, metadataKeyVMID: vmid, managedArgumentParams: params}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return
	}
	defer t.guard.end(ctx, m, token)
	resp, err = t.Service.UpdateQemuResize(ctx, node, vmid, params)
	err = t.guard.finish(ctx, m, token, resp, err)
	return
}
