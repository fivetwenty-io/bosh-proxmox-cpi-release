package pve

// FindVMAuthoritative contract tests. The function backs every handler branch
// that concludes "this VM does not exist", so the pins here are the
// destructive-branch safety net: absence only from an all-nodes-clean sweep,
// any probe failure retriable, and index hits served without probing.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// findVMTestClient is backendTestClient plus a QEMU service, the two handles
// FindVMAuthoritative touches.
type findVMTestClient struct {
	backendTestClient
	qemuSvc qemu.Service
}

func (c *findVMTestClient) QEMU() qemu.Service { return c.qemuSvc }

// findVMConfigNodes builds a ListConfigNodes response naming the given nodes.
func findVMConfigNodes(names ...string) func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	return func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		resp := make(sdkcluster.ListConfigNodesResponse, 0, len(names))
		for _, name := range names {
			raw, _ := json.Marshal(map[string]string{"name": name})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
}

// emptyListResources reports an empty cluster index: the lag window where a
// live guest is not yet visible.
func emptyFindVMListResources(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	empty := sdkcluster.ListResourcesResponse{}
	return &empty, nil
}

func fastBackoffCtx() context.Context {
	return WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
}

func TestFindVMAuthoritative_ClusterHit_NoProbes(t *testing.T) {
	t.Parallel()
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
					return clusterResp(map[string]any{
						"vmid": 4101, "node": "pve-node2", "tags": "bosh-vm;bosh-dir-abc", "type": "qemu",
					}), nil
				},
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, vmid int) (map[string]any, error) {
				t.Errorf("index hit must be served without probing, got Config(%s, %d)", node, vmid)
				return nil, errors.New("unexpected probe")
			},
		},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4101)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || loc.Node != "pve-node2" || loc.Tags != "bosh-vm;bosh-dir-abc" {
		t.Errorf("loc = %+v; want Found on pve-node2 with the index row's tags", loc)
	}
}

func TestFindVMAuthoritative_IndexMiss_ProbeFindsVM(t *testing.T) {
	t.Parallel()
	var probed []string
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn:        emptyFindVMListResources,
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
				probed = append(probed, node)
				if node != "pve-node2" {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				return map[string]any{"tags": "bosh-vm;bosh-deleting"}, nil
			},
		},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4102)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || loc.Node != "pve-node2" {
		t.Fatalf("loc = %+v; want the probe hit on pve-node2", loc)
	}
	if loc.Tags != "bosh-vm;bosh-deleting" {
		t.Errorf("Tags = %q; want the authoritative config's tag string", loc.Tags)
	}
	if len(probed) != 2 {
		t.Errorf("probed %v; want both nodes visited (clean 404 then hit)", probed)
	}
}

func TestFindVMAuthoritative_IndexMiss_AllNodesCleanAbsent(t *testing.T) {
	t.Parallel()
	var probed []string
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn:        emptyFindVMListResources,
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
				probed = append(probed, node)
				if node == "pve-node1" {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				// pmxcfs's config-missing surface: same proven-absent meaning as a 404.
				return nil, &sdkerrors.APIError{
					HTTPCode: 500,
					Message:  "Configuration file 'nodes/pve-node2/qemu-server/4103.conf' does not exist",
				}
			},
		},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4103)
	if err != nil {
		t.Fatalf("all-clean sweep must not error, got: %v", err)
	}
	if loc.Found {
		t.Errorf("loc = %+v; want Found=false, absence proven on every node", loc)
	}
	if len(probed) != 2 {
		t.Errorf("probed %v; want both nodes swept before concluding absence", probed)
	}
}

func TestFindVMAuthoritative_IndexMiss_ProbeErrorNoHit_Retriable(t *testing.T) {
	t.Parallel()
	var probed []string
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn:        emptyFindVMListResources,
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
				probed = append(probed, node)
				if node == "pve-node1" {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "pvedaemon worker cycling"}
			},
		},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4104)
	if err == nil {
		t.Fatal("a probe failure with no hit must error (the erroring node is the likeliest holder); got success")
	}
	if loc.Found {
		t.Errorf("loc = %+v; a failed sweep must not report a location", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("error = %v; want retriable so the Director re-drives once the node answers", err)
	}
	if len(probed) < 2 {
		t.Errorf("probed %v; the sweep must still attempt every node", probed)
	}
}

func TestFindVMAuthoritative_IndexMiss_EnumerationFails_Retriable(t *testing.T) {
	t.Parallel()
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn: emptyFindVMListResources,
				configNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
					return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "cluster config unavailable"}
				},
			},
		},
		qemuSvc: &fakeQEMUService{},
	}

	loc, err := FindVMAuthoritative(fastBackoffCtx(), c, 4105)
	if err == nil {
		t.Fatal("node-enumeration failure leaves absence unproven; want an error, got success")
	}
	if loc.Found {
		t.Errorf("loc = %+v; want no location on enumeration failure", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("error = %v; want retriable", err)
	}
}

func TestFindVMAuthoritative_IndexMiss_ZeroNodes_Retriable(t *testing.T) {
	t.Parallel()
	c := &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn:        emptyFindVMListResources,
				configNodesFn: findVMConfigNodes(),
			},
		},
		qemuSvc: &fakeQEMUService{},
	}

	_, err := FindVMAuthoritative(fastBackoffCtx(), c, 4106)
	if err == nil {
		t.Fatal("an empty membership list proves nothing; want an error, got success")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("error = %v; want retriable", err)
	}
}

func TestFindVMAuthoritative_MockContract_NilClusterAndBadVMID(t *testing.T) {
	t.Parallel()
	loc, err := FindVMAuthoritative(context.Background(), nil, 4107)
	if err != nil || loc.Found {
		t.Errorf("nil client: loc=%+v err=%v; want quiet not-found (mock contract)", loc, err)
	}
	c := &findVMTestClient{}
	loc, err = FindVMAuthoritative(context.Background(), c, 4107)
	if err != nil || loc.Found {
		t.Errorf("nil cluster service: loc=%+v err=%v; want quiet not-found", loc, err)
	}
	c = &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{}}}
	loc, err = FindVMAuthoritative(context.Background(), c, 0)
	if err != nil || loc.Found {
		t.Errorf("vmid 0: loc=%+v err=%v; want quiet not-found", loc, err)
	}
}

func TestListClusterConfigNodes_ParsesNamesSkipsMalformed(t *testing.T) {
	t.Parallel()
	c := &backendTestClient{
		clusterSvc: &fakeCluster{
			configNodesFn: func(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
				resp := sdkcluster.ListConfigNodesResponse{
					json.RawMessage(`{"name": "pve-node1", "nodeid": 1}`),
					json.RawMessage(`not json`),
					json.RawMessage(`{"nodeid": 3}`),
					json.RawMessage(`{"name": "pve-node2"}`),
				}
				return &resp, nil
			},
		},
	}
	nodes, err := ListClusterConfigNodes(fastBackoffCtx(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 2 || nodes[0] != "pve-node1" || nodes[1] != "pve-node2" {
		t.Errorf("nodes = %v; want the two named members, malformed rows skipped", nodes)
	}
}
