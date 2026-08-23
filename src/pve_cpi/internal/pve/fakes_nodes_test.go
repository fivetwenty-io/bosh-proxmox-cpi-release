// fakes_nodes_test.go — fakeNodesService, shared by tracing_test.go's Nodes
// exemplar test and by the Nodes full-matrix tests.
package pve

import (
	"context"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// fakeNodesService embeds the real generated interface (nil) and overrides
// only the single method each test needs — the same embedding idiom
// tracedNodesService itself uses, so no 25-method hand-written stub is
// required. Each *Fn field is nil until a test wires it; calling a method
// whose Fn is nil panics on the nil-func-call (an unwired method used by
// mistake fails loudly instead of silently returning zero values).
type fakeNodesService struct {
	nodes.Service

	createNetworkFn                     func(ctx context.Context, node string, params *nodes.CreateNetworkParams) error
	updateNetworkFn                     func(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error)
	listQemuFn                          func(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error)
	deleteQemuFn                        func(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error)
	createQemuAgentExecFn               func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentExecParams) (*nodes.CreateQemuAgentExecResponse, error)
	listQemuAgentExecStatusFn           func(ctx context.Context, node string, vmid string, params *nodes.ListQemuAgentExecStatusParams) (*nodes.ListQemuAgentExecStatusResponse, error)
	listQemuAgentNetworkGetInterfacesFn func(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error)
	createQemuAgentPingFn               func(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentPingResponse, error)
	createQemuCloneFn                   func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error)
	updateQemuConfigFn                  func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error
	createQemuMoveDiskFn                func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error)
	listQemuStatusCurrentFn             func(ctx context.Context, node string, vmid string) (*nodes.ListQemuStatusCurrentResponse, error)
	createQemuStatusRebootFn            func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error)
	createQemuTemplateFn                func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTemplateParams) (*nodes.CreateQemuTemplateResponse, error)
	updateQemuUnlinkFn                  func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) error
	listStorageFn                       func(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error)
	listStorageContentFn                func(ctx context.Context, node string, storageName string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error)
	createStorageDownloadURLFn          func(ctx context.Context, node string, storageName string, params *nodes.CreateStorageDownloadUrlParams) (*nodes.CreateStorageDownloadUrlResponse, error)
	listVersionFn                       func(ctx context.Context, node string) (*nodes.ListVersionResponse, error)
	listNetworkFn                       func(ctx context.Context, node string, params *nodes.ListNetworkParams) (*nodes.ListNetworkResponse, error)
	deleteNetwork2Fn                    func(ctx context.Context, node string, iface string) error
	listHardwarePciFn                   func(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error)
	createQemuFirewallIpsetFn           func(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallIpsetParams) error
	createQemuFirewallIpset2Fn          func(ctx context.Context, node string, vmid string, name string, params *nodes.CreateQemuFirewallIpset2Params) error
	createQemuFirewallRulesFn           func(ctx context.Context, node, vmid string, params *nodes.CreateQemuFirewallRulesParams) error
	updateQemuFirewallOptionsFn         func(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuFirewallOptionsParams) error
}

func (f *fakeNodesService) CreateQemuMoveDisk(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	return f.createQemuMoveDiskFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) CreateNetwork(ctx context.Context, node string, params *nodes.CreateNetworkParams) error {
	return f.createNetworkFn(ctx, node, params)
}

func (f *fakeNodesService) UpdateNetwork(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error) {
	return f.updateNetworkFn(ctx, node, params)
}

func (f *fakeNodesService) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	return f.listQemuFn(ctx, node, params)
}

func (f *fakeNodesService) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	return f.deleteQemuFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) CreateQemuAgentExec(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentExecParams) (*nodes.CreateQemuAgentExecResponse, error) {
	return f.createQemuAgentExecFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) ListQemuAgentExecStatus(ctx context.Context, node string, vmid string, params *nodes.ListQemuAgentExecStatusParams) (*nodes.ListQemuAgentExecStatusResponse, error) {
	return f.listQemuAgentExecStatusFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) ListQemuAgentNetworkGetInterfaces(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
	return f.listQemuAgentNetworkGetInterfacesFn(ctx, node, vmid)
}

func (f *fakeNodesService) CreateQemuAgentPing(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentPingResponse, error) {
	return f.createQemuAgentPingFn(ctx, node, vmid)
}

func (f *fakeNodesService) CreateQemuClone(ctx context.Context, node string, vmid string, params *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
	return f.createQemuCloneFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error {
	return f.updateQemuConfigFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) ListQemuStatusCurrent(ctx context.Context, node string, vmid string) (*nodes.ListQemuStatusCurrentResponse, error) {
	return f.listQemuStatusCurrentFn(ctx, node, vmid)
}

func (f *fakeNodesService) CreateQemuStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
	return f.createQemuStatusRebootFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) CreateQemuTemplate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTemplateParams) (*nodes.CreateQemuTemplateResponse, error) {
	return f.createQemuTemplateFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) UpdateQemuUnlink(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) error {
	return f.updateQemuUnlinkFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	return f.listStorageFn(ctx, node, params)
}

func (f *fakeNodesService) ListStorageContent(ctx context.Context, node string, storageName string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	return f.listStorageContentFn(ctx, node, storageName, params)
}

//nolint:revive // method name must match the SDK interface (SDK uses Url not URL)
func (f *fakeNodesService) CreateStorageDownloadUrl(ctx context.Context, node string, storageName string, params *nodes.CreateStorageDownloadUrlParams) (*nodes.CreateStorageDownloadUrlResponse, error) {
	return f.createStorageDownloadURLFn(ctx, node, storageName, params)
}

func (f *fakeNodesService) ListVersion(ctx context.Context, node string) (*nodes.ListVersionResponse, error) {
	return f.listVersionFn(ctx, node)
}

func (f *fakeNodesService) ListNetwork(ctx context.Context, node string, params *nodes.ListNetworkParams) (*nodes.ListNetworkResponse, error) {
	return f.listNetworkFn(ctx, node, params)
}

func (f *fakeNodesService) DeleteNetwork2(ctx context.Context, node string, iface string) error {
	return f.deleteNetwork2Fn(ctx, node, iface)
}

func (f *fakeNodesService) ListHardwarePci(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error) {
	return f.listHardwarePciFn(ctx, node, params)
}

func (f *fakeNodesService) CreateQemuFirewallIpset(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallIpsetParams) error {
	return f.createQemuFirewallIpsetFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) CreateQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, params *nodes.CreateQemuFirewallIpset2Params) error {
	return f.createQemuFirewallIpset2Fn(ctx, node, vmid, name, params)
}

func (f *fakeNodesService) CreateQemuFirewallRules(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallRulesParams) error {
	return f.createQemuFirewallRulesFn(ctx, node, vmid, params)
}

func (f *fakeNodesService) UpdateQemuFirewallOptions(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuFirewallOptionsParams) error {
	return f.updateQemuFirewallOptionsFn(ctx, node, vmid, params)
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (f *fakeNodesService) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	empty := nodes.ListNodesResponse{}
	return &empty, nil
}
