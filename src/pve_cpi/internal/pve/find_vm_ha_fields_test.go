package pve

// VMLocation carries the lock, hastate, and status fields of the cluster scan
// row at no extra cost. A location that did not come from a scan row leaves
// them unknown, and FromScan is how a caller tells the two apart.

import (
	"context"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestFindVMAuthoritative_ScanRowCarriesLockHAStateAndStatus(t *testing.T) {
	t.Parallel()
	rows := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return clusterResp(
			map[string]any{
				"vmid": 5101, "node": "pve-node1", "type": "qemu", "lock": "migrate", "hastate": "started", "status": "running",
			},
			map[string]any{"vmid": 5102, "node": "pve-node2", "type": "qemu", "tags": "bosh-cpi"},
			map[string]any{"vmid": 5103, "node": "pve-node3", "type": "qemu", "hastate": "fence", "status": "unknown"},
		), nil
	}
	c := &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{listFn: rows}}}

	cases := []struct {
		vmid                       int
		lock, haState, status, tag string
	}{
		{5101, "migrate", "started", "running", ""},
		// A scan row that omits the fields is a real answer: no lock, no HA
		// resource, and a status the scan did not report.
		{5102, "", "", "", "bosh-cpi"},
		{5103, "", "fence", "unknown", ""},
	}
	for _, tc := range cases {
		loc, err := FindVMAuthoritative(fastBackoffCtx(), c, tc.vmid)
		if err != nil {
			t.Fatalf("vmid %d: %v", tc.vmid, err)
		}
		if !loc.Found || !loc.FromScan || loc.FromConfig {
			t.Errorf("vmid %d: loc = %+v; want a found scan row", tc.vmid, loc)
		}
		if loc.Lock != tc.lock || loc.HAState != tc.haState || loc.Status != tc.status || loc.Tags != tc.tag {
			t.Errorf("vmid %d: loc = %+v; want lock %q hastate %q status %q tags %q",
				tc.vmid, loc, tc.lock, tc.haState, tc.status, tc.tag)
		}
	}

	node, tags, found, err := FindVMViaCluster(fastBackoffCtx(), c, 5102)
	if err != nil || !found || node != "pve-node2" || tags != "bosh-cpi" {
		t.Errorf("FindVMViaCluster = (%q, %q, %v, %v); want the unchanged answer", node, tags, found, err)
	}
}

func TestFindVMLocation_ConfigProbeLeavesTheScanFieldsUnknown(t *testing.T) {
	t.Parallel()
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
					// A stale row for the VM sits in the index, but the caller
					// went to the config probe on purpose.
					return clusterResp(map[string]any{
						"vmid": 5104, "node": "pve-node1", "type": "qemu", "lock": "migrate", "hastate": "started",
					}), nil
				},
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
				if node == "pve-node1" {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				return map[string]any{"tags": "bosh-cpi", "lock": "backup"}, nil
			},
		},
	}

	loc, err := FindVMByConfigProbe(fastBackoffCtx(), c, 5104)
	if err != nil {
		t.Fatal(err)
	}
	if !loc.Found || loc.Node != "pve-node2" || !loc.FromConfig || loc.FromScan {
		t.Fatalf("loc = %+v; want a config-probe hit that is not from the scan", loc)
	}
	if loc.Lock != "" || loc.HAState != "" || loc.Status != "" {
		t.Errorf("loc = %+v; the probe must leave lock, hastate, and status unknown", loc)
	}

	// The index-miss fallback in FindVMAuthoritative is a config probe as well.
	c.clusterSvc = &fakeCluster{listFn: emptyFindVMListResources, configNodesFn: findVMConfigNodes("pve-node2")}
	loc, err = FindVMAuthoritative(fastBackoffCtx(), c, 5105)
	if err != nil || !loc.Found || loc.FromScan || loc.Lock != "" || loc.HAState != "" {
		t.Errorf("loc = %+v err = %v; want a probe hit with the scan fields unknown", loc, err)
	}

	// A hand-built location is not a scan row either.
	if (VMLocation{Node: "n", Found: true}).FromScan {
		t.Error("a zero-value FromScan means unknown")
	}
}
