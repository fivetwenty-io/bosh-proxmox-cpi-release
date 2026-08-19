// fakes_cluster_test.go — fakeClusterService, shared by tracing_test.go's
// Cluster exemplar tests and by the Cluster full-matrix tests.
package pve

import (
	"context"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// fakeClusterService embeds the real generated interface (nil) and overrides
// only the methods each test needs — the same embedding idiom
// tracedClusterService itself uses, so no 60+-method hand-written stub is
// required.
type fakeClusterService struct {
	cluster.Service

	createSdnZonesFn func(ctx context.Context, params *cluster.CreateSdnZonesParams) error
	deleteHaRulesFn  func(ctx context.Context, rule string) error

	listResourcesFn         func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error)
	listStatusFn            func(ctx context.Context) (*cluster.ListStatusResponse, error)
	listFirewallGroupsFn    func(ctx context.Context) (*cluster.ListFirewallGroupsResponse, error)
	listConfigNodesFn       func(ctx context.Context) (*cluster.ListConfigNodesResponse, error)
	listSdnZonesFn          func(ctx context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error)
	getSdnZonesFn           func(ctx context.Context, zone string, params *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error)
	deleteSdnZonesFn        func(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) error
	listSdnVnetsFn          func(ctx context.Context, params *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error)
	getSdnVnetsFn           func(ctx context.Context, vnet string, params *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error)
	createSdnVnetsFn        func(ctx context.Context, params *cluster.CreateSdnVnetsParams) error
	deleteSdnVnetsFn        func(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) error
	listSdnVnetsSubnetsFn   func(ctx context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error)
	createSdnVnetsSubnetsFn func(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) error
	deleteSdnVnetsSubnetsFn func(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) error
	updateSdnFn             func(ctx context.Context, params *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error)
	createHaResourcesFn     func(ctx context.Context, params *cluster.CreateHaResourcesParams) error
	updateHaResourcesFn     func(ctx context.Context, sid string, params *cluster.UpdateHaResourcesParams) error
	deleteHaResourcesFn     func(ctx context.Context, sid string, params *cluster.DeleteHaResourcesParams) error
	listHaRulesFn           func(ctx context.Context, params *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error)
	createHaRulesFn         func(ctx context.Context, params *cluster.CreateHaRulesParams) error
	listOptionsFn           func(ctx context.Context) (*cluster.ListOptionsResponse, error)
	updateOptionsFn         func(ctx context.Context, params *cluster.UpdateOptionsParams) error
}

func (f *fakeClusterService) CreateSdnZones(ctx context.Context, params *cluster.CreateSdnZonesParams) error {
	return f.createSdnZonesFn(ctx, params)
}

func (f *fakeClusterService) DeleteHaRules(ctx context.Context, rule string) error {
	return f.deleteHaRulesFn(ctx, rule)
}

func (f *fakeClusterService) ListResources(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	return f.listResourcesFn(ctx, params)
}

func (f *fakeClusterService) ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error) {
	return f.listStatusFn(ctx)
}

func (f *fakeClusterService) ListFirewallGroups(ctx context.Context) (*cluster.ListFirewallGroupsResponse, error) {
	return f.listFirewallGroupsFn(ctx)
}

func (f *fakeClusterService) ListConfigNodes(ctx context.Context) (*cluster.ListConfigNodesResponse, error) {
	return f.listConfigNodesFn(ctx)
}

func (f *fakeClusterService) ListSdnZones(ctx context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error) {
	return f.listSdnZonesFn(ctx, params)
}

func (f *fakeClusterService) GetSdnZones(ctx context.Context, zone string, params *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error) {
	return f.getSdnZonesFn(ctx, zone, params)
}

func (f *fakeClusterService) DeleteSdnZones(ctx context.Context, zone string, params *cluster.DeleteSdnZonesParams) error {
	return f.deleteSdnZonesFn(ctx, zone, params)
}

func (f *fakeClusterService) ListSdnVnets(ctx context.Context, params *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	return f.listSdnVnetsFn(ctx, params)
}

func (f *fakeClusterService) GetSdnVnets(ctx context.Context, vnet string, params *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error) {
	return f.getSdnVnetsFn(ctx, vnet, params)
}

func (f *fakeClusterService) CreateSdnVnets(ctx context.Context, params *cluster.CreateSdnVnetsParams) error {
	return f.createSdnVnetsFn(ctx, params)
}

func (f *fakeClusterService) DeleteSdnVnets(ctx context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) error {
	return f.deleteSdnVnetsFn(ctx, vnet, params)
}

func (f *fakeClusterService) ListSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
	return f.listSdnVnetsSubnetsFn(ctx, vnet, params)
}

func (f *fakeClusterService) CreateSdnVnetsSubnets(ctx context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) error {
	return f.createSdnVnetsSubnetsFn(ctx, vnet, params)
}

func (f *fakeClusterService) DeleteSdnVnetsSubnets(ctx context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) error {
	return f.deleteSdnVnetsSubnetsFn(ctx, vnet, subnet, params)
}

func (f *fakeClusterService) UpdateSdn(ctx context.Context, params *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
	return f.updateSdnFn(ctx, params)
}

func (f *fakeClusterService) CreateHaResources(ctx context.Context, params *cluster.CreateHaResourcesParams) error {
	return f.createHaResourcesFn(ctx, params)
}

func (f *fakeClusterService) UpdateHaResources(ctx context.Context, sid string, params *cluster.UpdateHaResourcesParams) error {
	return f.updateHaResourcesFn(ctx, sid, params)
}

func (f *fakeClusterService) DeleteHaResources(ctx context.Context, sid string, params *cluster.DeleteHaResourcesParams) error {
	return f.deleteHaResourcesFn(ctx, sid, params)
}

func (f *fakeClusterService) ListHaRules(ctx context.Context, params *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	return f.listHaRulesFn(ctx, params)
}

func (f *fakeClusterService) CreateHaRules(ctx context.Context, params *cluster.CreateHaRulesParams) error {
	return f.createHaRulesFn(ctx, params)
}

func (f *fakeClusterService) ListOptions(ctx context.Context) (*cluster.ListOptionsResponse, error) {
	return f.listOptionsFn(ctx)
}

func (f *fakeClusterService) UpdateOptions(ctx context.Context, params *cluster.UpdateOptionsParams) error {
	return f.updateOptionsFn(ctx, params)
}
