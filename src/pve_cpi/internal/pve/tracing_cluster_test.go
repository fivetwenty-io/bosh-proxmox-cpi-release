// tracing_cluster_test.go — success+error span-assertion matrix for all 24
// tracedClusterService methods. None of these signatures
// carry a vmid parameter, so every case also asserts the exported span's
// attribute set matches the inventory exactly (no stray pve.vmid or other
// attribute leaks in).
package pve

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

const (
	clusterTestZone   = "sdn-zone-1"
	clusterTestVnet   = "vnet-1"
	clusterTestSubnet = "subnet-1"
	clusterTestHaSid  = "ha-sid-1"
)

// clusterMethodCase describes one tracedClusterService method under test.
// wire configures the fake's *Fn field for the given injected error (nil
// means the success path); invoke calls the traced method with fixed
// argument values matching what wire asserts against.
type clusterMethodCase struct {
	name      string
	wantSpan  string
	wantAttrs map[string]string
	wire      func(t *testing.T, f *fakeClusterService, injectErr error)
	invoke    func(traced *tracedClusterService) error
}

//nolint:gocognit,gocyclo // Flat test-case table, one entry per traced method; the complexity score is repetition of an identical wire/invoke shape, not branching logic.
func clusterMethodCases() []clusterMethodCase {
	return []clusterMethodCase{
		{
			name:     "ListResources",
			wantSpan: "pve.cluster.list_resources",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.listResourcesFn = func(_ context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
					if params == nil {
						t.Error("ListResources called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListResourcesResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListResources(context.Background(), &cluster.ListResourcesParams{})
				return err
			},
		},
		{
			name:     "ListStatus",
			wantSpan: "pve.cluster.list_status",
			wire: func(_ *testing.T, f *fakeClusterService, injectErr error) {
				f.listStatusFn = func(context.Context) (*cluster.ListStatusResponse, error) {
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListStatusResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListStatus(context.Background())
				return err
			},
		},
		{
			name:     "ListFirewallGroups",
			wantSpan: "pve.cluster.list_firewall_groups",
			wire: func(_ *testing.T, f *fakeClusterService, injectErr error) {
				f.listFirewallGroupsFn = func(context.Context) (*cluster.ListFirewallGroupsResponse, error) {
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListFirewallGroupsResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListFirewallGroups(context.Background())
				return err
			},
		},
		{
			name:     "ListConfigNodes",
			wantSpan: "pve.cluster.list_config_nodes",
			wire: func(_ *testing.T, f *fakeClusterService, injectErr error) {
				f.listConfigNodesFn = func(context.Context) (*cluster.ListConfigNodesResponse, error) {
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListConfigNodesResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListConfigNodes(context.Background())
				return err
			},
		},
		{
			name:     "ListSdnZones",
			wantSpan: "pve.cluster.list_sdn_zones",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.listSdnZonesFn = func(_ context.Context, params *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error) {
					if params == nil {
						t.Error("ListSdnZones called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListSdnZonesResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListSdnZones(context.Background(), &cluster.ListSdnZonesParams{})
				return err
			},
		},
		{
			name:      "GetSdnZones",
			wantSpan:  "pve.cluster.get_sdn_zones",
			wantAttrs: map[string]string{"pve.sdn_zone": clusterTestZone},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.getSdnZonesFn = func(_ context.Context, zone string, params *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error) {
					if zone != clusterTestZone {
						t.Errorf("GetSdnZones called with zone=%q, want %q", zone, clusterTestZone)
					}
					if params == nil {
						t.Error("GetSdnZones called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.GetSdnZonesResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.GetSdnZones(context.Background(), clusterTestZone, &cluster.GetSdnZonesParams{})
				return err
			},
		},
		{
			name:      "DeleteSdnZones",
			wantSpan:  "pve.cluster.delete_sdn_zones",
			wantAttrs: map[string]string{"pve.sdn_zone": clusterTestZone},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.deleteSdnZonesFn = func(_ context.Context, zone string, params *cluster.DeleteSdnZonesParams) error {
					if zone != clusterTestZone {
						t.Errorf("DeleteSdnZones called with zone=%q, want %q", zone, clusterTestZone)
					}
					if params == nil {
						t.Error("DeleteSdnZones called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.DeleteSdnZones(context.Background(), clusterTestZone, &cluster.DeleteSdnZonesParams{})
			},
		},
		{
			name:     "ListSdnVnets",
			wantSpan: "pve.cluster.list_sdn_vnets",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.listSdnVnetsFn = func(_ context.Context, params *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
					if params == nil {
						t.Error("ListSdnVnets called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListSdnVnetsResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListSdnVnets(context.Background(), &cluster.ListSdnVnetsParams{})
				return err
			},
		},
		{
			name:      "GetSdnVnets",
			wantSpan:  "pve.cluster.get_sdn_vnets",
			wantAttrs: map[string]string{"pve.sdn_vnet": clusterTestVnet},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.getSdnVnetsFn = func(_ context.Context, vnet string, params *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error) {
					if vnet != clusterTestVnet {
						t.Errorf("GetSdnVnets called with vnet=%q, want %q", vnet, clusterTestVnet)
					}
					if params == nil {
						t.Error("GetSdnVnets called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.GetSdnVnetsResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.GetSdnVnets(context.Background(), clusterTestVnet, &cluster.GetSdnVnetsParams{})
				return err
			},
		},
		{
			name:     "CreateSdnVnets",
			wantSpan: "pve.cluster.create_sdn_vnets",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.createSdnVnetsFn = func(_ context.Context, params *cluster.CreateSdnVnetsParams) error {
					if params == nil {
						t.Error("CreateSdnVnets called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.CreateSdnVnets(context.Background(), &cluster.CreateSdnVnetsParams{})
			},
		},
		{
			name:      "DeleteSdnVnets",
			wantSpan:  "pve.cluster.delete_sdn_vnets",
			wantAttrs: map[string]string{"pve.sdn_vnet": clusterTestVnet},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.deleteSdnVnetsFn = func(_ context.Context, vnet string, params *cluster.DeleteSdnVnetsParams) error {
					if vnet != clusterTestVnet {
						t.Errorf("DeleteSdnVnets called with vnet=%q, want %q", vnet, clusterTestVnet)
					}
					if params == nil {
						t.Error("DeleteSdnVnets called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.DeleteSdnVnets(context.Background(), clusterTestVnet, &cluster.DeleteSdnVnetsParams{})
			},
		},
		{
			name:      "ListSdnVnetsSubnets",
			wantSpan:  "pve.cluster.list_sdn_vnets_subnets",
			wantAttrs: map[string]string{"pve.sdn_vnet": clusterTestVnet},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.listSdnVnetsSubnetsFn = func(_ context.Context, vnet string, params *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
					if vnet != clusterTestVnet {
						t.Errorf("ListSdnVnetsSubnets called with vnet=%q, want %q", vnet, clusterTestVnet)
					}
					if params == nil {
						t.Error("ListSdnVnetsSubnets called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListSdnVnetsSubnetsResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListSdnVnetsSubnets(context.Background(), clusterTestVnet, &cluster.ListSdnVnetsSubnetsParams{})
				return err
			},
		},
		{
			name:      "CreateSdnVnetsSubnets",
			wantSpan:  "pve.cluster.create_sdn_vnets_subnets",
			wantAttrs: map[string]string{"pve.sdn_vnet": clusterTestVnet},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.createSdnVnetsSubnetsFn = func(_ context.Context, vnet string, params *cluster.CreateSdnVnetsSubnetsParams) error {
					if vnet != clusterTestVnet {
						t.Errorf("CreateSdnVnetsSubnets called with vnet=%q, want %q", vnet, clusterTestVnet)
					}
					if params == nil {
						t.Error("CreateSdnVnetsSubnets called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.CreateSdnVnetsSubnets(context.Background(), clusterTestVnet, &cluster.CreateSdnVnetsSubnetsParams{})
			},
		},
		{
			name:     "DeleteSdnVnetsSubnets",
			wantSpan: "pve.cluster.delete_sdn_vnets_subnets",
			wantAttrs: map[string]string{
				"pve.sdn_vnet":   clusterTestVnet,
				"pve.sdn_subnet": clusterTestSubnet,
			},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.deleteSdnVnetsSubnetsFn = func(_ context.Context, vnet string, subnet string, params *cluster.DeleteSdnVnetsSubnetsParams) error {
					if vnet != clusterTestVnet {
						t.Errorf("DeleteSdnVnetsSubnets called with vnet=%q, want %q", vnet, clusterTestVnet)
					}
					if subnet != clusterTestSubnet {
						t.Errorf("DeleteSdnVnetsSubnets called with subnet=%q, want %q", subnet, clusterTestSubnet)
					}
					if params == nil {
						t.Error("DeleteSdnVnetsSubnets called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.DeleteSdnVnetsSubnets(context.Background(), clusterTestVnet, clusterTestSubnet, &cluster.DeleteSdnVnetsSubnetsParams{})
			},
		},
		{
			name:     "UpdateSdn",
			wantSpan: "pve.cluster.update_sdn",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.updateSdnFn = func(_ context.Context, params *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
					if params == nil {
						t.Error("UpdateSdn called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.UpdateSdnResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.UpdateSdn(context.Background(), &cluster.UpdateSdnParams{})
				return err
			},
		},
		{
			name:     "CreateHaResources",
			wantSpan: "pve.cluster.create_ha_resources",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.createHaResourcesFn = func(_ context.Context, params *cluster.CreateHaResourcesParams) error {
					if params == nil {
						t.Error("CreateHaResources called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.CreateHaResources(context.Background(), &cluster.CreateHaResourcesParams{})
			},
		},
		{
			name:      "UpdateHaResources",
			wantSpan:  "pve.cluster.update_ha_resources",
			wantAttrs: map[string]string{"pve.ha_sid": clusterTestHaSid},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.updateHaResourcesFn = func(_ context.Context, sid string, params *cluster.UpdateHaResourcesParams) error {
					if sid != clusterTestHaSid {
						t.Errorf("UpdateHaResources called with sid=%q, want %q", sid, clusterTestHaSid)
					}
					if params == nil {
						t.Error("UpdateHaResources called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.UpdateHaResources(context.Background(), clusterTestHaSid, &cluster.UpdateHaResourcesParams{})
			},
		},
		{
			name:      "DeleteHaResources",
			wantSpan:  "pve.cluster.delete_ha_resources",
			wantAttrs: map[string]string{"pve.ha_sid": clusterTestHaSid},
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.deleteHaResourcesFn = func(_ context.Context, sid string, params *cluster.DeleteHaResourcesParams) error {
					if sid != clusterTestHaSid {
						t.Errorf("DeleteHaResources called with sid=%q, want %q", sid, clusterTestHaSid)
					}
					if params == nil {
						t.Error("DeleteHaResources called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.DeleteHaResources(context.Background(), clusterTestHaSid, &cluster.DeleteHaResourcesParams{})
			},
		},
		{
			name:     "ListHaRules",
			wantSpan: "pve.cluster.list_ha_rules",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.listHaRulesFn = func(_ context.Context, params *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
					if params == nil {
						t.Error("ListHaRules called with nil params")
					}
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListHaRulesResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListHaRules(context.Background(), &cluster.ListHaRulesParams{})
				return err
			},
		},
		{
			name:     "CreateHaRules",
			wantSpan: "pve.cluster.create_ha_rules",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.createHaRulesFn = func(_ context.Context, params *cluster.CreateHaRulesParams) error {
					if params == nil {
						t.Error("CreateHaRules called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.CreateHaRules(context.Background(), &cluster.CreateHaRulesParams{})
			},
		},
		{
			name:     "ListOptions",
			wantSpan: "pve.cluster.list_options",
			wire: func(_ *testing.T, f *fakeClusterService, injectErr error) {
				f.listOptionsFn = func(context.Context) (*cluster.ListOptionsResponse, error) {
					if injectErr != nil {
						return nil, injectErr
					}
					return &cluster.ListOptionsResponse{}, nil
				}
			},
			invoke: func(traced *tracedClusterService) error {
				_, err := traced.ListOptions(context.Background())
				return err
			},
		},
		{
			name:     "UpdateOptions",
			wantSpan: "pve.cluster.update_options",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.updateOptionsFn = func(_ context.Context, params *cluster.UpdateOptionsParams) error {
					if params == nil {
						t.Error("UpdateOptions called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.UpdateOptions(context.Background(), &cluster.UpdateOptionsParams{})
			},
		},
		{
			name:     "CreateSdnZones",
			wantSpan: "pve.cluster.create_sdn_zones",
			wire: func(t *testing.T, f *fakeClusterService, injectErr error) {
				f.createSdnZonesFn = func(_ context.Context, params *cluster.CreateSdnZonesParams) error {
					if params == nil {
						t.Error("CreateSdnZones called with nil params")
					}
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.CreateSdnZones(context.Background(), &cluster.CreateSdnZonesParams{})
			},
		},
		{
			name:      "DeleteHaRules",
			wantSpan:  "pve.cluster.delete_ha_rules",
			wantAttrs: map[string]string{"pve.ha_rule": clusterTestHaSid},
			wire: func(_ *testing.T, f *fakeClusterService, injectErr error) {
				f.deleteHaRulesFn = func(context.Context, string) error {
					return injectErr
				}
			},
			invoke: func(traced *tracedClusterService) error {
				return traced.DeleteHaRules(context.Background(), clusterTestHaSid)
			},
		},
	}
}

// spanAttrMap flattens a recorded span's attribute set into a plain
// string-keyed map for exact-match comparison against wantAttrs.
func spanAttrMap(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}

func attrMapsEqual(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestTracedClusterService_MethodMatrix(t *testing.T) {
	t.Parallel()

	for _, tc := range clusterMethodCases() {
		tc := tc

		t.Run(tc.name+"_Success", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			fake := &fakeClusterService{}
			tc.wire(t, fake, nil)
			traced := &tracedClusterService{Service: fake, tracer: tracer}

			if err := tc.invoke(traced); err != nil {
				t.Fatalf("%s returned err=%v, want nil", tc.name, err)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code == codes.Error {
				t.Errorf("success span carries Error status: %+v", span.Status)
			}
			gotAttrs := spanAttrMap(span.Attributes)
			if !attrMapsEqual(gotAttrs, tc.wantAttrs) {
				t.Errorf("span attributes = %v, want %v", gotAttrs, tc.wantAttrs)
			}
		})

		t.Run(tc.name+"_Error", func(t *testing.T) {
			t.Parallel()
			tracer, exporter := newTestTracer(t)
			fake := &fakeClusterService{}
			wantErr := errors.New(tc.name + " failed: upstream rejected request")
			tc.wire(t, fake, wantErr)
			traced := &tracedClusterService{Service: fake, tracer: tracer}

			err := tc.invoke(traced)
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s returned err=%v, want %v", tc.name, err, wantErr)
			}

			spans := exporter.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("got %d exported spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name != tc.wantSpan {
				t.Fatalf("span name = %q, want %q", span.Name, tc.wantSpan)
			}
			if span.Status.Code != codes.Error {
				t.Fatalf("error span status code = %v, want Error", span.Status.Code)
			}
			wantDescription := log.ScrubMessage(wantErr.Error())
			if span.Status.Description != wantDescription {
				t.Errorf("span status description = %q, want %q", span.Status.Description, wantDescription)
			}
			if len(span.Events) == 0 {
				t.Error("expected span.RecordError to add an exception event, got none")
			}
			gotAttrs := spanAttrMap(span.Attributes)
			if !attrMapsEqual(gotAttrs, tc.wantAttrs) {
				t.Errorf("span attributes = %v, want %v", gotAttrs, tc.wantAttrs)
			}
		})
	}
}
