package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// vnetStubCluster satisfies cluster.Service via embedding; only ListSdnVnets
// is overridden. All other methods panic if called.
type vnetStubCluster struct {
	sdkcluster.Service
	listSdnVnetsFn func(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error)
}

func (s *vnetStubCluster) ListSdnVnets(ctx context.Context, params *sdkcluster.ListSdnVnetsParams) (*sdkcluster.ListSdnVnetsResponse, error) {
	return s.listSdnVnetsFn(ctx, params)
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

func TestNextVNI_SkipsUsedTags(t *testing.T) {
	t.Parallel()
	// Band [5000,5002]; 5000 and 5002 taken → only 5001 free.
	c := newVNIClient(vnetRow("bosh1", 5000), vnetRow("bosh2", 5002), vnetRow("untagged", 0))

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5002)
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

	vni, err := pve.NextVNI(context.Background(), c, 5000, 5999)
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

	_, err := pve.NextVNI(context.Background(), c, 5000, 5001)
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

	_, err := pve.NextVNI(context.Background(), c, 6000, 5000)
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

	_, err := pve.NextVNI(context.Background(), c, 5000, 5999)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestNextVNI_NilArgs(t *testing.T) {
	t.Parallel()
	if _, err := pve.NextVNI(context.Background(), nil, 5000, 5999); err == nil {
		t.Error("nil client: expected error")
	}
	//lint:ignore SA1012 deliberate nil-ctx contract check
	//nolint:staticcheck // deliberate nil-ctx contract check
	if _, err := pve.NextVNI(nil, newVNIClient(), 5000, 5999); err == nil {
		t.Error("nil ctx: expected error")
	}
}
