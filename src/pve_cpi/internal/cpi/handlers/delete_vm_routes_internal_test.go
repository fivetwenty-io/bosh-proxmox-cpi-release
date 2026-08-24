// Package handlers internal tests for advertised-route provenance tags and
// the delete_vm SDN subnet cleanup.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// ---------------------------------------------------------------------------
// provenance tag unit tests
// ---------------------------------------------------------------------------

func TestAdvertisedRouteTag_FormatAndRoundTrip(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")

	if !regexp.MustCompile(`^advrt-vnet1-[0-9a-f]{8}$`).MatchString(tag) {
		t.Fatalf("tag %q does not match advrt-<vnet>-<hash8>", tag)
	}
	if len(tag) > 23 {
		t.Errorf("tag %q exceeds 23 bytes", tag)
	}
	// Determinism + parse round-trip.
	if tag != advertisedRouteTag("vnet1", "10.64.0.0/16") {
		t.Error("tag must be deterministic")
	}
	refs := parseAdvertisedRouteTags("bosh-cpi;" + tag + ";director--d1")
	if len(refs) != 1 {
		t.Fatalf("parse: want 1 ref, got %d", len(refs))
	}
	if refs[0].vnet != "vnet1" || refs[0].hash8 != advrtHash8("vnet1", "10.64.0.0/16") {
		t.Errorf("parse: got %+v", refs[0])
	}
}

func TestAdvertisedRouteTag_DistinctPerRoute(t *testing.T) {
	t.Parallel()
	a := advertisedRouteTag("vnet1", "10.64.0.0/16")
	b := advertisedRouteTag("vnet1", "10.65.0.0/16")
	c := advertisedRouteTag("vnet2", "10.64.0.0/16")
	if a == b || a == c || b == c {
		t.Errorf("tags must differ per (vnet,cidr): %q %q %q", a, b, c)
	}
}

func TestAdvertisedRouteTag_IPv6(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "fd00:10::/64")
	if !regexp.MustCompile(`^advrt-vnet1-[0-9a-f]{8}$`).MatchString(tag) {
		t.Fatalf("IPv6 tag %q does not match the grammar", tag)
	}
}

func TestAdvertisedRouteTags_SortedDeduped(t *testing.T) {
	t.Parallel()
	routes := []AdvertisedRoute{
		{VNet: "zz", Destination: "10.2.0.0/16"},
		{VNet: "aa", Destination: "10.1.0.0/16"},
		{VNet: "zz", Destination: "10.2.0.0/16"}, // duplicate
	}
	tags := advertisedRouteTags(routes)
	if len(tags) != 2 {
		t.Fatalf("want 2 deduped tags, got %d: %v", len(tags), tags)
	}
	if tags[0] > tags[1] {
		t.Errorf("tags must be sorted: %v", tags)
	}
	if advertisedRouteTags(nil) != nil {
		t.Error("nil routes must yield nil tags")
	}
}

func TestParseAdvertisedRouteTags_MalformedSkipped(t *testing.T) {
	t.Parallel()
	refs := parseAdvertisedRouteTags("advrt-;advrt-x;advrt-vnet1-short;bosh-cpi;;advrt-vnet1-0123abcd")
	if len(refs) != 1 {
		t.Fatalf("want only the well-formed ref, got %d: %+v", len(refs), refs)
	}
	if refs[0].vnet != "vnet1" || refs[0].hash8 != "0123abcd" {
		t.Errorf("ref: got %+v", refs[0])
	}
	if parseAdvertisedRouteTags("") != nil {
		t.Error("empty tag string must yield nil")
	}
}

// TestAdvrtTag_TruncationDropsDetectably proves the create_vm warn
// predicate: when the 255-byte tag cap truncates the merged list, a dropped
// advrt tag is absent from the merged string (strings.Contains detects it).
func TestAdvrtTag_TruncationDropsDetectably(t *testing.T) {
	t.Parallel()
	filler := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		filler = append(filler, fmt.Sprintf("filler-%02d-xxxxxxxx", i))
	}
	advrt := advertisedRouteTag("vnet1", "10.64.0.0/16")
	merged := mergeTagList([]string{ownershipTag}, append(filler, advrt), maxTagLength)
	if len(merged) > maxTagLength {
		t.Fatalf("merged tag string exceeds the cap: %d bytes", len(merged))
	}
	if strings.Contains(merged, advrt) {
		t.Fatal("test setup: advrt tag was expected to be dropped by the cap")
	}
}

// ---------------------------------------------------------------------------
// cleanupAdvertisedRoutes tests
// ---------------------------------------------------------------------------

// advrtClusterStub implements the four cluster methods the cleanup touches.
type advrtClusterStub struct {
	sdkcluster.Service

	resources     []json.RawMessage
	resourcesErr  error
	listResCalls  int
	subnetsByVnet map[string][]json.RawMessage
	subnetsErr    error
	deleted       []string // "<vnet>/<subnetID>"
	deleteErr     error
	applyCalls    int
}

func (s *advrtClusterStub) ListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	s.listResCalls++
	if s.resourcesErr != nil {
		return nil, s.resourcesErr
	}
	resp := sdkcluster.ListResourcesResponse(s.resources)
	return &resp, nil
}

// ListConfigNodes reports the single test node "pve1" (every fixture row
// lives there) so pve.ListGuestsAuthoritative has a non-empty membership.
func (s *advrtClusterStub) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	raw, _ := json.Marshal(map[string]any{"name": "pve1"})
	resp := sdkcluster.ListConfigNodesResponse{raw}
	return &resp, nil
}

func (s *advrtClusterStub) ListSdnVnetsSubnets(_ context.Context, vnet string, _ *sdkcluster.ListSdnVnetsSubnetsParams) (*sdkcluster.ListSdnVnetsSubnetsResponse, error) {
	if s.subnetsErr != nil {
		return nil, s.subnetsErr
	}
	resp := sdkcluster.ListSdnVnetsSubnetsResponse(s.subnetsByVnet[vnet])
	return &resp, nil
}

func (s *advrtClusterStub) DeleteSdnVnetsSubnets(_ context.Context, vnet string, subnet string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, vnet+"/"+subnet)
	return nil
}

func (s *advrtClusterStub) UpdateSdn(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
	s.applyCalls++
	return nil, nil
}

func advrtDeps(cl *advrtClusterStub) Deps {
	cfg := icMinConfig()
	return Deps{
		Config: cfg,
		PVE: &icPVEClient{
			clusterSvc: cl,
			nodesSvc: &icNodesService{listFn: func(ctx context.Context, p *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return cl.ListResources(ctx, p)
			}},
		},
		Logger: log.NewNopLogger(),
	}
}

// vmRow marshals a /cluster/resources vm row.
func vmRow(vmid int, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": "pve1", "tags": tags, "type": "qemu"})
	return raw
}

// subnetRow marshals one subnet row with the canonical PVE id and cidr.
func subnetRow(id, cidr string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"subnet": id, "cidr": cidr, "type": "subnet"})
	return raw
}

func TestCleanupAdvertisedRoutes_SoleOwner_DeletesAndApplies(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resources: []json.RawMessage{
			vmRow(100, "bosh-cpi;"+tag),
			vmRow(200, "bosh-cpi"), // other VM, no shared tag
		},
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {
				subnetRow("z1-10.64.0.0-16", "10.64.0.0/16"),
				subnetRow("z1-10.0.0.0-24", "10.0.0.0/24"), // unrelated — untouched
			},
		},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, "bosh-cpi;"+tag, log.NewNopLogger())

	if len(cl.deleted) != 1 || cl.deleted[0] != "vnet1/z1-10.64.0.0-16" {
		t.Errorf("deleted = %v, want exactly the matching subnet", cl.deleted)
	}
	if cl.applyCalls != 1 {
		t.Errorf("apply calls = %d, want 1", cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_SharedOwner_Skips(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resources: []json.RawMessage{
			vmRow(100, tag),
			vmRow(200, tag), // second router still alive shares the route
		},
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {subnetRow("z1-10.64.0.0-16", "10.64.0.0/16")},
		},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if len(cl.deleted) != 0 {
		t.Errorf("deleted = %v, want none (route shared)", cl.deleted)
	}
	if cl.applyCalls != 0 {
		t.Errorf("apply calls = %d, want 0", cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_RefcountError_FailOpen(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resourcesErr: errors.New("boom"),
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {subnetRow("z1-10.64.0.0-16", "10.64.0.0/16")},
		},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if len(cl.deleted) != 0 || cl.applyCalls != 0 {
		t.Errorf("refcount failure must delete nothing; deleted=%v applies=%d", cl.deleted, cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_SubnetAlreadyGone_Idempotent(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resources: []json.RawMessage{vmRow(100, tag)},
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {subnetRow("z1-10.0.0.0-24", "10.0.0.0/24")}, // no matching hash
		},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if len(cl.deleted) != 0 || cl.applyCalls != 0 {
		t.Errorf("gone subnet must be a no-op; deleted=%v applies=%d", cl.deleted, cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_VnetGone_Idempotent(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resources:  []json.RawMessage{vmRow(100, tag)},
		subnetsErr: &sdkerrors.APIError{HTTPCode: 404},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if len(cl.deleted) != 0 || cl.applyCalls != 0 {
		t.Errorf("vnet-gone must be a no-op; deleted=%v applies=%d", cl.deleted, cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_DeleteError_FailOpen(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "10.64.0.0/16")
	cl := &advrtClusterStub{
		resources: []json.RawMessage{vmRow(100, tag)},
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {subnetRow("z1-10.64.0.0-16", "10.64.0.0/16")},
		},
		deleteErr: errors.New("locked"),
	}

	// Must not panic and must not apply (nothing was deleted).
	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if cl.applyCalls != 0 {
		t.Errorf("apply calls = %d, want 0 when the delete failed", cl.applyCalls)
	}
}

func TestCleanupAdvertisedRoutes_NoTags_ZeroCalls(t *testing.T) {
	t.Parallel()
	cl := &advrtClusterStub{}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, "bosh-cpi;director--d1", log.NewNopLogger())

	if cl.listResCalls != 0 {
		t.Errorf("no advrt tags must make zero SDN/refcount calls; got %d", cl.listResCalls)
	}
}

func TestCleanupAdvertisedRoutes_IPv6RoundTrip(t *testing.T) {
	t.Parallel()
	tag := advertisedRouteTag("vnet1", "fd00:10::/64")
	cl := &advrtClusterStub{
		resources: []json.RawMessage{vmRow(100, tag)},
		subnetsByVnet: map[string][]json.RawMessage{
			"vnet1": {subnetRow("z1-fd00:10::-64", "fd00:10::/64")},
		},
	}

	cleanupAdvertisedRoutes(context.Background(), advrtDeps(cl), 100, tag, log.NewNopLogger())

	if len(cl.deleted) != 1 || cl.deleted[0] != "vnet1/z1-fd00:10::-64" {
		t.Errorf("IPv6 subnet must round-trip via hash match; deleted=%v", cl.deleted)
	}
	if cl.applyCalls != 1 {
		t.Errorf("apply calls = %d, want 1", cl.applyCalls)
	}
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (s *advrtClusterStub) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}
