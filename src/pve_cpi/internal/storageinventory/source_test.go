package storageinventory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type readClient struct {
	pve.Client
	cluster clusterstorage.Service
	node    nodes.Service
}

func (c readClient) ClusterStorage() clusterstorage.Service { return c.cluster }
func (c readClient) Nodes() nodes.Service                   { return c.node }

type clusterReader struct {
	clusterstorage.Service
	response *clusterstorage.ListStorageResponse
	err      error
}

func (c clusterReader) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	return c.response, c.err
}

type nodeReader struct {
	nodes.Service
	response *nodes.ListStorageResponse
	err      error
	name     *string
}

func (n nodeReader) ListStorage(_ context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	if n.name != nil {
		*n.name = node
	}
	if params != nil {
		return nil, errors.New("unexpected per-storage filter")
	}
	return n.response, n.err
}

func TestPVESourcePreservesReadErrorsAndResponseShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	failure := errors.New("transport refused")
	for _, read := range []func() (any, error){func() (any, error) { return (PVESource{}).Definitions(ctx) }, func() (any, error) { return (PVESource{}).Statuses(ctx, "n1") }} {
		if _, err := read(); err == nil {
			t.Fatal("nil client accepted")
		}
	}
	for _, tc := range []struct {
		name    string
		cluster *clusterstorage.ListStorageResponse
		node    *nodes.ListStorageResponse
		err     error
	}{
		{name: "nil response"},
		{name: "nil arrays", cluster: new(clusterstorage.ListStorageResponse), node: new(nodes.ListStorageResponse)},
		{name: "transport", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := PVESource{Client: readClient{cluster: clusterReader{response: tc.cluster, err: tc.err}, node: nodeReader{response: tc.node, err: tc.err}}}
			_, definitionErr := source.Definitions(ctx)
			_, statusErr := source.Statuses(ctx, "n1")
			if definitionErr == nil || statusErr == nil {
				t.Fatal("failed observation accepted")
			}
			if tc.err != nil && (!errors.Is(definitionErr, failure) || !errors.Is(statusErr, failure)) {
				t.Fatal("transport error identity lost")
			}
		})
	}
	clusterResponse := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"a"}`)}
	nodeResponse := nodes.ListStorageResponse{json.RawMessage(`{"storage":"a","active":1}`)}
	var seenNode string
	source := PVESource{Client: readClient{cluster: clusterReader{response: &clusterResponse}, node: nodeReader{response: &nodeResponse, name: &seenNode}}}
	defs, err := source.Definitions(ctx)
	if err != nil || len(defs) != 1 {
		t.Fatalf("definitions: %v %v", defs, err)
	}
	statuses, err := source.Statuses(ctx, "n-test")
	if err != nil || len(statuses) != 1 || seenNode != "n-test" {
		t.Fatalf("statuses: %v %v %s", statuses, err, seenNode)
	}
	emptyDefinitions := clusterstorage.ListStorageResponse{}
	emptyStatuses := nodes.ListStorageResponse{}
	source = PVESource{Client: readClient{cluster: clusterReader{response: &emptyDefinitions}, node: nodeReader{response: &emptyStatuses}}}
	if got, err := source.Definitions(ctx); err != nil || got == nil {
		t.Fatal("empty array confused with nil response")
	}
	if got, err := source.Statuses(ctx, "n1"); err != nil || got == nil {
		t.Fatal("empty status array confused with nil response")
	}
}

func TestCollectorRejectsMalformedSetupAndDefinitionLists(t *testing.T) {
	t.Parallel()
	if _, err := NewCollector(nil, Options{}); err == nil {
		t.Fatal("nil source accepted")
	}
	for _, n := range []int{-1, 65} {
		if _, err := NewCollector(sourceFor(t, "a"), Options{Concurrency: n}); err == nil {
			t.Fatal("unbounded concurrency accepted")
		}
	}
	defaultCollector, err := NewCollector(sourceFor(t, "a"), Options{})
	if err != nil || defaultCollector.clock == nil || defaultCollector.concurrency != 4 {
		t.Fatalf("default options: %v", err)
	}
	c := collector(t, sourceFor(t, "a"), newClock())
	//lint:ignore SA1012 Verify defensive rejection of an invalid nil context.
	//nolint:staticcheck // Exercise rejection of an absent context.
	if _, err := c.Discover(nil, policy("a"), Request{}); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := c.Discover(context.Background(), nil, Request{}); err == nil {
		t.Fatal("nil configuration accepted")
	}
	if _, err := c.Refresh(context.Background(), nil); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Discover(ctx, policy("a"), Request{Nodes: []string{"n1"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery: %v", err)
	}
	for _, defs := range [][]json.RawMessage{{definition(t, "a", nil), definition(t, "a", nil)}, {json.RawMessage(`{"storage":"a","type":"nfs","content":null}`)}, {json.RawMessage(`{"storage":"a"}`)}} {
		s := sourceFor(t, "a")
		s.defs = defs
		if _, err := collector(t, s, newClock()).Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1"}}); err == nil {
			t.Fatal("malformed definitions accepted")
		}
	}
	for _, request := range []Request{{}, {Nodes: []string{""}}, {Nodes: []string{"n1", "n1"}}, {Nodes: []string{"n1"}, SetNames: []string{"missing"}}, {Nodes: []string{"n1"}, SetNames: []string{" "}}, {Nodes: []string{"n1"}, CompanionStorageIDs: []string{" "}}} {
		if _, err := c.Discover(context.Background(), policy("a"), request); err == nil {
			t.Fatal("malformed request accepted")
		}
	}
}
