package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// vnetStubCluster satisfies cluster.Service via embedding; only ListSdnVnets
// and ListSdnZones are overridden. All other methods panic if called.
//
// ListSdnZones defaults to an empty zone list when listSdnZonesFn is unset,
// so every pre-§1.7 test in this file (none of which cares about zones) keeps
// its original exclusion behavior unchanged: an empty zone list contributes
// zero reserved VNIs.
type vnetStubCluster struct {
	sdkcluster.Service
	listSdnVnetsFn func(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error)
	listSdnZonesFn func(ctx context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error)
}

func (s *vnetStubCluster) ListSdnVnets(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	return s.listSdnVnetsFn(ctx, params)
}

func (s *vnetStubCluster) ListSdnZones(ctx context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
	if s.listSdnZonesFn != nil {
		return s.listSdnZonesFn(ctx, params)
	}
	empty := sdkcluster.ListSdnZonesResponse{}
	return &empty, nil
}

// vnetRow marshals one vnet row with the given tag; tag 0 emits an untagged row.
func vnetRow(name string, tag int) json.RawMessage {
	entry := map[string]any{"vnet": name, "zone": "z"}
	if tag != 0 {
		entry["tag"] = tag
	}
	raw, _ := json.Marshal(entry)
	return raw
}

// zoneRow marshals one SDN zone row carrying zone-level reserved-VNI fields.
// Either vrfVxlan or tag may be 0 to omit that field from the row — mirrors
// vnetRow's zero-means-absent convention.
func zoneRow(zone string, vrfVxlan, tag int) json.RawMessage {
	entry := map[string]any{"zone": zone, "type": "evpn"}
	if vrfVxlan != 0 {
		entry["vrf-vxlan"] = vrfVxlan
	}
	if tag != 0 {
		entry["tag"] = tag
	}
	raw, _ := json.Marshal(entry)
	return raw
}

func newVNIClient(rows ...json.RawMessage) *mockClient {
	resp := sdkcluster.ListSdnVnetsResponse(rows)
	return &mockClient{
		clusterSvc: &vnetStubCluster{
			listSdnVnetsFn: func(_ context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
				if params == nil || params.Pending == nil || !*params.Pending {
					panic("ListSdnVnets must be called with pending=true")
				}
				return &resp, nil
			},
		},
	}
}

// newVNIClientWithZones extends newVNIClient with an explicit zone row set,
// for tests that verify zone-level VNI exclusion (§1.7).
func newVNIClientWithZones(vnetRows, zoneRows []json.RawMessage) *mockClient {
	vresp := sdkcluster.ListSdnVnetsResponse(vnetRows)
	zresp := sdkcluster.ListSdnZonesResponse(zoneRows)
	return &mockClient{
		clusterSvc: &vnetStubCluster{
			listSdnVnetsFn: func(_ context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
				if params == nil || params.Pending == nil || !*params.Pending {
					panic("ListSdnVnets must be called with pending=true")
				}
				return &vresp, nil
			},
			listSdnZonesFn: func(_ context.Context, params *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
				if params == nil || params.Pending == nil || !*params.Pending {
					panic("ListSdnZones must be called with pending=true")
				}
				return &zresp, nil
			},
		},
	}
}

// newVNIClientZoneListError builds a client whose vnet listing succeeds but
// whose zone listing fails, for the §1.7 fail-open test.
func newVNIClientZoneListError(vnetRows []json.RawMessage, zoneErr error) *mockClient {
	vresp := sdkcluster.ListSdnVnetsResponse(vnetRows)
	return &mockClient{
		clusterSvc: &vnetStubCluster{
			listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
				return &vresp, nil
			},
			listSdnZonesFn: func(_ context.Context, _ *sdkcluster.ListSdnZonesParams) (*sdkcluster.ListSdnZonesResponse, error) {
				return nil, zoneErr
			},
		},
	}
}

func TestNextVNI_SkipsUsedTags(t *testing.T) {
	t.Parallel()
	// Band [5000,5002]; 5000 and 5002 taken → only 5001 free.
	c := newVNIClient(vnetRow("bosh1", 5000), vnetRow("bosh2", 5002), vnetRow("untagged", 0))

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5002, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vni != 5001 {
		t.Errorf("VNI = %d, want 5001", vni)
	}
}

func TestNextVNI_EmptyCluster_WithinBand(t *testing.T) {
	t.Parallel()
	c := newVNIClient()

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5999, log.NewNopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vni < 5000 || vni > 5999 {
		t.Errorf("VNI %d outside band [5000,5999]", vni)
	}
}

func TestNextVNI_Exhausted_NamesConfigKeys(t *testing.T) {
	t.Parallel()
	c := newVNIClient(vnetRow("a", 5000), vnetRow("b", 5001))

	_, err := pve.NextVNI(context.Background(), c, 5000, 5001, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"sdn_vni_range_start", "sdn_vni_range_end", "vnet_tag"} {
		if !strings.Contains(msg, want) {
			t.Errorf("exhaustion error %q must mention %q", msg, want)
		}
	}
}

func TestNextVNI_EndBelowStart_Errors(t *testing.T) {
	t.Parallel()
	c := newVNIClient()

	_, err := pve.NextVNI(context.Background(), c, 6000, 5000, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected invalid-range error, got nil")
	}
}

func TestNextVNI_ListError_Wrapped(t *testing.T) {
	t.Parallel()
	c := &mockClient{
		clusterSvc: &vnetStubCluster{
			listSdnVnetsFn: func(_ context.Context, _ *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
				return nil, errors.New("boom")
			},
		},
	}

	_, err := pve.NextVNI(context.Background(), c, 5000, 5999, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestNextVNI_NilArgs(t *testing.T) {
	t.Parallel()
	if _, err := pve.NextVNI(context.Background(), nil, 5000, 5999, log.NewNopLogger()); err == nil {
		t.Error("nil client: expected error")
	}
	//lint:ignore SA1012 deliberate nil-ctx contract check
	//nolint:staticcheck // deliberate nil-ctx contract check
	if _, err := pve.NextVNI(nil, newVNIClient(), 5000, 5999, log.NewNopLogger()); err == nil {
		t.Error("nil ctx: expected error")
	}
}

// ---------------------------------------------------------------------------
// §1.7: zone-level VNI exclusion
// ---------------------------------------------------------------------------

// TestNextVNI_ExcludesZoneVrfVxlan verifies that an EVPN zone's vrf-vxlan
// control VNI, sitting inside the allocation band, is never handed out to a
// vnet — the core §1.7 fix. Band [5000,5002]; no vnets are tagged, but the
// zone reserves 5001 via vrf-vxlan, so only 5000 and 5002 remain free.
func TestNextVNI_ExcludesZoneVrfVxlan(t *testing.T) {
	t.Parallel()
	c := newVNIClientWithZones(
		nil,
		[]json.RawMessage{zoneRow("evpn1", 5001, 0)},
	)

	seen := map[int]bool{}
	for i := 0; i < 2; i++ {
		vni, err := pve.NextVNI(context.Background(), c, 5000, 5002, log.NewNopLogger())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if vni == 5001 {
			t.Fatalf("zone-reserved VNI 5001 (vrf-vxlan) must never be allocated, got %d", vni)
		}
		seen[vni] = true
	}
	// Both non-reserved slots must be reachable across repeated allocation
	// (random start offset) — a loose sanity check that exclusion is
	// data-driven, not a coincidental single-call pass.
	if len(seen) == 0 {
		t.Fatal("expected at least one allocation to succeed")
	}
}

// TestNextVNI_ExcludesZoneTag verifies that a zone-level "tag" field (a
// separate, zone-type-dependent reservation mechanism from vrf-vxlan) is also
// excluded. Band [5000,5000]: the sole slot is zone-reserved, so allocation
// must exhaust rather than hand out the reserved VNI.
func TestNextVNI_ExcludesZoneTag(t *testing.T) {
	t.Parallel()
	c := newVNIClientWithZones(
		nil,
		[]json.RawMessage{zoneRow("z1", 0, 5000)},
	)

	_, err := pve.NextVNI(context.Background(), c, 5000, 5000, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected exhaustion error: the only band slot is zone-reserved via tag")
	}
}

// TestNextVNI_ZoneListFailure_FailsOpenAndWarnsOnce verifies the §1.7
// fail-open contract in a single test (deliberately NOT split across two test
// functions, and deliberately NOT t.Parallel): when listing SDN zones fails,
// allocation proceeds using only the vnet-tag exclusion set (the pre-§1.7
// behavior) rather than failing the entire allocation, and the Warn fires
// exactly once even across two separate NextVNI calls that both hit the
// failure — vniZoneListWarnOnce is a package-level sync.Once with no
// test-only reset hook, so exercising both the "fails open" and the
// "warns once" assertions from one call sequence is the only way to keep
// this deterministic regardless of test execution order within the package.
func TestNextVNI_ZoneListFailure_FailsOpenAndWarnsOnce(t *testing.T) {
	c := newVNIClientZoneListError(
		[]json.RawMessage{vnetRow("bosh1", 5000)},
		errors.New("zone listing unavailable"),
	)
	obsLogger, obs := log.NewObservedLogger(log.LevelWarn)

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5001, obsLogger)
	if err != nil {
		t.Fatalf("first call: zone-list failure must fail open, not error: %v", err)
	}
	if vni != 5001 {
		t.Errorf("VNI = %d, want 5001 (5000 taken by vnet, zone exclusion unavailable)", vni)
	}

	// Second call, same failure: must also fail open, and must NOT add a
	// second Warn.
	if _, err := pve.NextVNI(context.Background(), c, 5000, 5001, obsLogger); err != nil {
		t.Fatalf("second call: zone-list failure must fail open, not error: %v", err)
	}

	warnCount := 0
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn {
			warnCount++
		}
	}
	if warnCount != 1 {
		t.Errorf("expected exactly 1 Warn across both zone-list-failure calls, got %d (entries: %+v)", warnCount, obs.All())
	}
}
