package pve

// The VM ownership guard reads the template flag and resource type from the
// same answer that located the guest. These pins cover both legs: the index
// row (including an LXC row and PVE's numeric template flag) and the
// config-probe hit on an index miss.

import (
	"context"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func TestFindVMAuthoritative_IndexRowCarriesTemplateAndType(t *testing.T) {
	t.Parallel()
	rows := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return clusterResp(
			map[string]any{"vmid": 4201, "node": "pve-node1", "type": "qemu", "template": 1, "tags": "bosh-stemcell"},
			map[string]any{"vmid": 4202, "node": "pve-node2", "type": "lxc", "tags": ""},
			map[string]any{"vmid": 4203, "node": "pve-node1", "type": "qemu", "tags": "bosh-cpi"},
		), nil
	}
	c := &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{listFn: rows}}}

	cases := []struct {
		vmid     int
		template bool
		typ      string
	}{
		{4201, true, "qemu"},
		{4202, false, "lxc"},
		{4203, false, "qemu"},
	}
	for _, tc := range cases {
		loc, err := FindVMAuthoritative(fastBackoffCtx(), c, tc.vmid)
		if err != nil {
			t.Fatalf("vmid %d: unexpected error: %v", tc.vmid, err)
		}
		if !loc.Found || loc.Template != tc.template || loc.Type != tc.typ || loc.FromConfig {
			t.Errorf("vmid %d: loc = %+v; want Found, Template=%v, Type=%q, and not FromConfig", tc.vmid, loc, tc.template, tc.typ)
		}
	}

	node, tags, found, err := FindVMViaCluster(fastBackoffCtx(), c, 4201)
	if err != nil || !found || node != "pve-node1" || tags != "bosh-stemcell" {
		t.Errorf("FindVMViaCluster = (%q, %q, %v, %v); want the unchanged node/tags answer", node, tags, found, err)
	}
}

func TestFindVMAuthoritative_ProbeHitReadsTemplateFlag(t *testing.T) {
	t.Parallel()
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn:        emptyFindVMListResources,
				configNodesFn: findVMConfigNodes("pve-node1"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
				if vmid == 4205 {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				return map[string]any{"tags": "bosh-cpi", "template": float64(1)}, nil
			},
		},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4204)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || !loc.Template || loc.Type != "qemu" || !loc.FromConfig {
		t.Errorf("loc = %+v; want the probe hit to report Template=true, Type=qemu, and FromConfig", loc)
	}
}

// TestFindVMByConfigProbe_SkipsTheIndex pins the re-locate the ownership guard
// runs after a config read on the row's node answers not-found. The stale row
// is never consulted, every member's config is probed, absence is proven only
// when each answers not-found, and a client without a cluster service proves
// nothing.
func TestFindVMByConfigProbe_SkipsTheIndex(t *testing.T) {
	t.Parallel()
	staleRow := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return clusterResp(map[string]any{"vmid": 4210, "node": "pve-node1", "type": "qemu", "tags": "bosh-cpi"}), nil
	}
	notFound := &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	client := func(configFn func(context.Context, string, int) (map[string]any, error)) *findVMTestClient {
		return &findVMTestClient{
			backendTestClient: backendTestClient{clusterSvc: &fakeCluster{
				listFn:        staleRow,
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			}},
			qemuSvc: &fakeQEMUService{configFn: configFn},
		}
	}

	t.Run("absent on every member", func(t *testing.T) {
		t.Parallel()
		loc, err := FindVMByConfigProbe(fastBackoffCtx(), client(func(context.Context, string, int) (map[string]any, error) {
			return nil, notFound
		}), 4210)
		if err != nil || loc.Found {
			t.Fatalf("loc=%+v err=%v; want proven absence despite the index row", loc, err)
		}
	})

	t.Run("hit on another member", func(t *testing.T) {
		t.Parallel()
		loc, err := FindVMByConfigProbe(fastBackoffCtx(), client(func(_ context.Context, node string, _ int) (map[string]any, error) {
			if node == "pve-node2" {
				return map[string]any{"tags": "bosh-cpi"}, nil
			}
			return nil, notFound
		}), 4210)
		if err != nil || !loc.Found || loc.Node != "pve-node2" || !loc.FromConfig || loc.Tags != "bosh-cpi" {
			t.Fatalf("loc=%+v err=%v; want a FromConfig hit on pve-node2", loc, err)
		}
	})

	t.Run("no cluster service", func(t *testing.T) {
		t.Parallel()
		c := &findVMTestClient{qemuSvc: &fakeQEMUService{}}
		if _, err := FindVMByConfigProbe(fastBackoffCtx(), c, 4210); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err = %v; want a retriable error without a cluster service", err)
		}
	})
}

func TestConfigFlag(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{"a": float64(1), "b": float64(0), "c": true, "d": "1", "e": "0", "f": nil, "g": "yes"}
	want := map[string]bool{"a": true, "b": false, "c": true, "d": true, "e": false, "f": false, "g": false, "missing": false}
	for key, w := range want {
		if got := ConfigFlag(cfg, key); got != w {
			t.Errorf("ConfigFlag(%q) = %v; want %v", key, got, w)
		}
	}
}
