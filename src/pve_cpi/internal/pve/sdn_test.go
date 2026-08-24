package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Test plumbing — minimal Cluster service stub with per-method hooks.
// ---------------------------------------------------------------------------

// sdnFakeCluster embeds cluster.Service so any method not overridden here
// panics on call — that surfaces accidental SDK widening during tests.
type sdnFakeCluster struct {
	sdkcluster.Service

	deleteZoneFn func(ctx context.Context, zone string, params *sdkcluster.DeleteSdnZonesParams) error
	listZonesFn  func(ctx context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error)
	getZoneFn    func(ctx context.Context, zone string, params *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error)
	deleteVnetFn func(ctx context.Context, vnet string, params *sdkcluster.DeleteSdnVnetsParams) error
	listVnetsFn  func(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error)
	getVnetFn    func(ctx context.Context, vnet string, params *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error)
	deleteSubFn  func(ctx context.Context, vnet, subnet string, params *sdkcluster.DeleteSdnVnetsSubnetsParams) error
	listSubsFn   func(ctx context.Context, vnet string, params *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error)
}

func (f *sdnFakeCluster) DeleteSdnZones(ctx context.Context, zone string, params *sdkcluster.DeleteSdnZonesParams) error {
	return f.deleteZoneFn(ctx, zone, params)
}
func (f *sdnFakeCluster) ListSdnZones(ctx context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
	return f.listZonesFn(ctx, params)
}
func (f *sdnFakeCluster) GetSdnZones(ctx context.Context, zone string, params *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
	return f.getZoneFn(ctx, zone, params)
}
func (f *sdnFakeCluster) DeleteSdnVnets(ctx context.Context, vnet string, params *sdkcluster.DeleteSdnVnetsParams) error {
	return f.deleteVnetFn(ctx, vnet, params)
}
func (f *sdnFakeCluster) ListSdnVnets(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	return f.listVnetsFn(ctx, params)
}
func (f *sdnFakeCluster) GetSdnVnets(ctx context.Context, vnet string, params *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
	return f.getVnetFn(ctx, vnet, params)
}
func (f *sdnFakeCluster) DeleteSdnVnetsSubnets(ctx context.Context, vnet, subnet string, params *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
	return f.deleteSubFn(ctx, vnet, subnet, params)
}
func (f *sdnFakeCluster) ListSdnVnetsSubnets(ctx context.Context, vnet string, params *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
	return f.listSubsFn(ctx, vnet, params)
}

// sdnClient satisfies pve.Client and exposes only the Cluster service.
type sdnClient struct {
	clusterSvc sdkcluster.Service
}

func (c *sdnClient) QEMU() sdkqemu.Service                     { return nil }
func (c *sdnClient) Storage() sdkstorage.Service               { return nil }
func (c *sdnClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *sdnClient) Tasks() sdktasks.Service                   { return nil }
func (c *sdnClient) Nodes() sdknodes.Service                   { return nil }
func (c *sdnClient) Cluster() sdkcluster.Service               { return c.clusterSvc }
func (c *sdnClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *sdnClient) Pools() pve.PoolService                    { return nil }

// newSDNClient builds a pve.Client backed by the supplied fake.
func newSDNClient(f *sdnFakeCluster) pve.Client {
	return &sdnClient{clusterSvc: f}
}

// rawRows packages JSON-encoded rows as the cluster service does.
func rawRows(t *testing.T, rows ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// rawObj marshals a single object and returns *json.RawMessage as a Get
// response (json.RawMessage type alias).
func rawObj(t *testing.T, v any) *json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal obj: %v", err)
	}
	rm := json.RawMessage(b)
	return &rm
}

// isRetriable reports whether err carries a TypeRetriableCloud classification.
func isRetriable(t *testing.T, err error) bool {
	t.Helper()
	var e *cpierrors.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.OkToRetry()
}

// ---------------------------------------------------------------------------
// DeleteSDNZone
// ---------------------------------------------------------------------------

func TestDeleteSDNZone_HappyPath(t *testing.T) {
	t.Parallel()
	var gotZone string
	fake := &sdnFakeCluster{
		deleteZoneFn: func(_ context.Context, zone string, _ *sdkcluster.DeleteSdnZonesParams) error {
			gotZone = zone
			return nil
		},
	}
	c := newSDNClient(fake)
	if err := pve.DeleteSDNZone(context.Background(), c, "zcpi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotZone != "zcpi" {
		t.Errorf("zone: want zcpi got %q", gotZone)
	}
}

func TestDeleteSDNZone_NotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		deleteZoneFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			return makeAPIErr(404, "no such zone")
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNZone(context.Background(), c, "ghost")
	if err == nil {
		t.Fatal("expected error for missing zone")
	}
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Errorf("404 → expected ErrSDNNotFound, got %v", err)
	}
}

func TestDeleteSDNZone_EmptyZone(t *testing.T) {
	t.Parallel()
	c := newSDNClient(&sdnFakeCluster{})
	if err := pve.DeleteSDNZone(context.Background(), c, ""); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestDeleteSDNZone_ServerErrorRetriable(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		deleteZoneFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnZonesParams) error {
			return makeAPIErr(503, "busy")
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNZone(context.Background(), c, "zcpi")
	if !isRetriable(t, err) {
		t.Errorf("5xx → expected retriable, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListSDNZones + GetSDNZone
// ---------------------------------------------------------------------------

func TestListSDNZones_DecodesRows(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		listZonesFn: func(_ context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
			if params == nil || params.Pending == nil || !*params.Pending {
				t.Errorf("expected pending=true on list")
			}
			rows := rawRows(t,
				map[string]any{"zone": "zcpi1", "type": "simple", "bridge": "vmbr0"},
				map[string]any{"zone": "zcpi2", "type": "vlan", "bridge": "vmbr1"},
			)
			resp := sdkcluster.ListSdnZonesResponse(rows)
			return &resp, nil
		},
	}
	c := newSDNClient(fake)
	zones, err := pve.ListSDNZones(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(zones) != 2 {
		t.Fatalf("want 2 zones got %d", len(zones))
	}
	if zones[0].Zone != "zcpi1" || zones[0].Type != "simple" || zones[0].Bridge != "vmbr0" {
		t.Errorf("zone[0] mismatch: %+v", zones[0])
	}
	if zones[1].Zone != "zcpi2" || zones[1].Type != "vlan" {
		t.Errorf("zone[1] mismatch: %+v", zones[1])
	}
}

func TestListSDNZones_Empty(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		listZonesFn: func(_ context.Context, _ *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
			resp := sdkcluster.ListSdnZonesResponse{}
			return &resp, nil
		},
	}
	c := newSDNClient(fake)
	zones, err := pve.ListSDNZones(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if zones == nil || len(zones) != 0 {
		t.Errorf("want empty non-nil slice, got %v", zones)
	}
}

func TestGetSDNZone_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		getZoneFn: func(_ context.Context, zone string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			if zone != "zcpi" {
				t.Errorf("zone arg: want zcpi got %q", zone)
			}
			return rawObj(t, map[string]any{"type": "simple", "bridge": "vmbr0"}), nil
		},
	}
	c := newSDNClient(fake)
	z, err := pve.GetSDNZone(context.Background(), c, "zcpi")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if z.Zone != "zcpi" {
		t.Errorf("want Zone back-filled to zcpi, got %q", z.Zone)
	}
	if z.Type != "simple" {
		t.Errorf("Type: want simple got %q", z.Type)
	}
	if z.Bridge != "vmbr0" {
		t.Errorf("Bridge: want vmbr0 got %q", z.Bridge)
	}
}

func TestGetSDNZone_NotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		getZoneFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnZonesParams) (*sdkcluster.GetSdnZonesResponse, error) {
			return nil, makeAPIErr(404, "nope")
		},
	}
	c := newSDNClient(fake)
	_, err := pve.GetSDNZone(context.Background(), c, "ghost")
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Fatalf("404 → expected ErrSDNNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateSDNVnet / DeleteSDNVnet / List / Get
// ---------------------------------------------------------------------------

func TestDeleteSDNVnet_NotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		deleteVnetFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return makeAPIErr(404, "gone")
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNVnet(context.Background(), c, "v")
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Errorf("want ErrSDNNotFound, got %v", err)
	}
}

func TestDeleteSDNVnet_Server5xxRetriable(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		deleteVnetFn: func(_ context.Context, _ string, _ *sdkcluster.DeleteSdnVnetsParams) error {
			return makeAPIErr(500, "kapow")
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNVnet(context.Background(), c, "v")
	if !isRetriable(t, err) {
		t.Errorf("5xx → expected retriable, got %v", err)
	}
}

func TestListSDNVnets_DecodesRows(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		listVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
			rows := rawRows(t,
				map[string]any{"vnet": "v1", "zone": "z1", "tag": 100},
				map[string]any{"vnet": "v2", "zone": "z2"},
			)
			resp := sdkcluster.ListSdnVnetsResponse(rows)
			return &resp, nil
		},
	}
	c := newSDNClient(fake)
	vs, err := pve.ListSDNVnets(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("want 2 got %d", len(vs))
	}
	if vs[0].Vnet != "v1" || vs[0].Zone != "z1" || vs[0].Tag != 100 {
		t.Errorf("vnet[0] mismatch: %+v", vs[0])
	}
	if vs[1].Zone != "z2" {
		t.Errorf("vnet[1].Zone want z2 got %q", vs[1].Zone)
	}
}

func TestGetSDNVnet_ExposesZone(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		getVnetFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return rawObj(t, map[string]any{"zone": "zparent", "tag": 50}), nil
		},
	}
	c := newSDNClient(fake)
	v, err := pve.GetSDNVnet(context.Background(), c, "vcpi")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if v.Vnet != "vcpi" {
		t.Errorf("Vnet back-fill want vcpi got %q", v.Vnet)
	}
	if v.Zone != "zparent" {
		t.Errorf("Zone want zparent got %q", v.Zone)
	}
}

func TestGetSDNVnet_NotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		getVnetFn: func(_ context.Context, _ string, _ *sdkcluster.GetSdnVnetsParams) (*sdkcluster.GetSdnVnetsResponse, error) {
			return nil, makeAPIErr(404, "no vnet")
		},
	}
	c := newSDNClient(fake)
	_, err := pve.GetSDNVnet(context.Background(), c, "v")
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Fatalf("404 → ErrSDNNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateSDNVnetSubnet / DeleteSDNVnetSubnet / ListSDNVnetSubnets
// ---------------------------------------------------------------------------

func TestDeleteSDNVnetSubnet_HappyPath(t *testing.T) {
	t.Parallel()
	var gotVnet, gotSub string
	fake := &sdnFakeCluster{
		deleteSubFn: func(_ context.Context, vnet, sub string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			gotVnet = vnet
			gotSub = sub
			return nil
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNVnetSubnet(context.Background(), c, "vcpi", "10.0.0.0/24")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotVnet != "vcpi" || gotSub != "10.0.0.0/24" {
		t.Errorf("args: want (vcpi, 10.0.0.0/24) got (%q, %q)", gotVnet, gotSub)
	}
}

func TestDeleteSDNVnetSubnet_NotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		deleteSubFn: func(_ context.Context, _, _ string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
			return makeAPIErr(404, "no subnet")
		},
	}
	c := newSDNClient(fake)
	err := pve.DeleteSDNVnetSubnet(context.Background(), c, "v", "10.0.0.0/24")
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Errorf("want ErrSDNNotFound, got %v", err)
	}
}

func TestListSDNVnetSubnets_DecodesAndBackfillsVnet(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		listSubsFn: func(_ context.Context, vnet string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			if vnet != "vcpi" {
				t.Errorf("vnet arg want vcpi got %q", vnet)
			}
			rows := rawRows(t,
				map[string]any{"subnet": "10.0.0.0/24", "gateway": "10.0.0.1", "type": "subnet"},
			)
			resp := sdkcluster.ListSdnVnetsSubnetsResponse(rows)
			return &resp, nil
		},
	}
	c := newSDNClient(fake)
	subs, err := pve.ListSDNVnetSubnets(context.Background(), c, "vcpi")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("want 1 got %d", len(subs))
	}
	if subs[0].Subnet != "10.0.0.0/24" {
		t.Errorf("Subnet: %q", subs[0].Subnet)
	}
	if subs[0].Vnet != "vcpi" {
		t.Errorf("Vnet back-fill want vcpi got %q", subs[0].Vnet)
	}
}

func TestListSDNVnetSubnets_VnetNotFound(t *testing.T) {
	t.Parallel()
	fake := &sdnFakeCluster{
		listSubsFn: func(_ context.Context, _ string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
			return nil, makeAPIErr(404, "vnet gone")
		},
	}
	c := newSDNClient(fake)
	_, err := pve.ListSDNVnetSubnets(context.Background(), c, "ghost")
	if !errors.Is(err, pve.ErrSDNNotFound) {
		t.Errorf("404 on list → ErrSDNNotFound, got %v", err)
	}
}
