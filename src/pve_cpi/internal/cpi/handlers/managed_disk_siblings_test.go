package handlers

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// managedDiskSiblingFixture builds the two-member persistent set these tests
// plan against. Member a rests at 20 percent utilization and member b at 40,
// so least_utilized orders the two members strictly and the seed the planner
// draws for each allocation never decides the winner.
func managedDiskSiblingFixture(t *testing.T, strategy string) (Deps, *StoragePlacementSelection, *layeredResolver, *diskSiblingPVE, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	set := config.StorageSet{Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: strategy, Version: 1}}
	cfg := &config.CPIConfig{
		Node: "n1", DiskStorage: "legacy-forbidden", DetachedDiskStrategy: "free",
		StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: dir,
		PersistentStorageSet: "P", StorageSets: map[string]config.StorageSet{"P": set},
	}
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{}}
	client := &diskSiblingPVE{
		managedDiskTestPVE: managedDiskTestPVE{state: state},
		clusterNodes:       []string{"n1"},
		available:          map[string]uint64{"a": 80 << 30, "b": 60 << 30},
	}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	selection, err := ResolveStoragePlacementSelectors(cfg, "create_disk", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := newLayeredResolver(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return deps, selection, resolver, client, dir
}

func managedDiskSiblingPlan(t *testing.T, deps Deps, selection *StoragePlacementSelection, resolver *layeredResolver, siblings []aj.Record) *managedDiskRequest {
	t.Helper()
	m, err := prepareManagedDisk(t.Context(), deps, selection, 1025, createDiskCloudProperties{}, "", resolver, siblings)
	if err != nil {
		t.Fatalf("prepare managed disk: %v", err)
	}
	if m == nil || m.plan == nil || len(m.plan.Targets) != 1 {
		t.Fatalf("prepare managed disk returned no persistent target: %+v", m)
	}
	return m
}

// A peer that claimed member a moments ago has to be charged against the next
// persistent allocation, which then lands on member b.
func TestManagedDiskPlanChargesInFlightSibling(t *testing.T) {
	deps, selection, resolver, _, _ := managedDiskSiblingFixture(t, "least_utilized")
	base := managedDiskSiblingPlan(t, deps, selection, resolver, nil)
	target := base.plan.Targets[0]
	if target.StorageID != "a" {
		t.Fatalf("fixture baseline chose %q, want a", target.StorageID)
	}
	claim := uint64(40) << 30
	peerPlan := siblingTestPlan(siblingTestCharge("persistent", target.CapacityKey, target.DomainKey, claim))
	peer := siblingTestRecord(t, "alloc-peer", aj.Planned, peerPlan)
	next := managedDiskSiblingPlan(t, deps, selection, resolver, []aj.Record{peer})
	if next.plan.Targets[0].StorageID != "b" {
		t.Fatalf("second allocation chose %q, want b", next.plan.Targets[0].StorageID)
	}
	if next.iterator.req.SiblingMemberBytes[target.CapacityKey] != claim {
		t.Fatalf("request carries %v for %q, want %d", next.iterator.req.SiblingMemberBytes, target.CapacityKey, claim)
	}
	if next.id == base.id {
		t.Fatal("two allocations share one identifier")
	}
}

// A record that reached ready_to_return charges no bytes, so only a sibling
// count partition could move this placement. The persistent path never runs
// one, even when the namespace holds records of one instance group.
func TestManagedDiskPathNeverPartitionsBySiblingGroup(t *testing.T) {
	deps, selection, resolver, _, _ := managedDiskSiblingFixture(t, "least_utilized")
	base := managedDiskSiblingPlan(t, deps, selection, resolver, nil)
	target := base.plan.Targets[0]
	group := "deployment--cf/instance-group--diego-cell"
	peers := []aj.Record{
		siblingTestRecord(t, "alloc-peer-a", aj.ReadyToReturn, siblingTestGroupPlan(group, target.CapacityKey)),
		siblingTestRecord(t, "alloc-peer-b", aj.Adopted, siblingTestGroupPlan(group, target.CapacityKey)),
	}
	next := managedDiskSiblingPlan(t, deps, selection, resolver, peers)
	if next.plan.Targets[0].StorageID != target.StorageID {
		t.Fatalf("grouped resting peers moved the allocation to %q, want %q", next.plan.Targets[0].StorageID, target.StorageID)
	}
	if next.iterator.req.SiblingGroupCounts != nil {
		t.Fatalf("persistent request carries sibling counts %v, want none", next.iterator.req.SiblingGroupCounts)
	}
	if next.iterator.req.Group != "" {
		t.Fatalf("persistent request carries group %q, want an empty one", next.iterator.req.Group)
	}
	if len(next.iterator.req.SiblingMemberBytes) != 0 || len(next.iterator.req.SiblingDomainBytes) != 0 {
		t.Fatalf("resting peers charged bytes: member %v domain %v", next.iterator.req.SiblingMemberBytes, next.iterator.req.SiblingDomainBytes)
	}
}

// diskSiblingPVE records which node each read reached, so a test can tell the
// cluster-wide journal and admission scope apart from the narrower set of
// nodes that planning discovers.
type diskSiblingPVE struct {
	managedDiskTestPVE
	clusterNodes []string
	// available is the free space each storage reports on every node, which is
	// what gives least_utilized a strict order over the two members.
	available map[string]uint64
	mu        sync.Mutex
	calls     []string
}

func (c *diskSiblingPVE) Nodes() nodes.Service {
	return diskSiblingNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, client: c}
}

func (c *diskSiblingPVE) record(kind, node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, kind+":"+node)
}

func (c *diskSiblingPVE) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

// nodesFor returns the sorted distinct nodes one kind of read reached.
func (c *diskSiblingPVE) nodesFor(kind string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := []string{}
	for _, call := range c.calls {
		if node, ok := strings.CutPrefix(call, kind+":"); ok {
			seen = append(seen, node)
		}
	}
	slices.Sort(seen)
	return slices.Compact(seen)
}

// firstCall returns the position of the first read of one kind, or -1.
func (c *diskSiblingPVE) firstCall(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.IndexFunc(c.calls, func(call string) bool { return strings.HasPrefix(call, kind+":") })
}

type diskSiblingNodes struct {
	managedDiskTestNodes
	client *diskSiblingPVE
}

func (n diskSiblingNodes) ListNodes(context.Context) (*nodes.ListNodesResponse, error) {
	response := nodes.ListNodesResponse{}
	for _, name := range n.client.clusterNodes {
		raw, err := json.Marshal(map[string]any{"node": name, "status": "online"})
		if err != nil {
			return nil, err
		}
		response = append(response, raw)
	}
	return &response, nil
}

func (n diskSiblingNodes) ListCertificatesInfo(_ context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	n.client.record("certificates", node)
	raw, err := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("11:", 32), ":")})
	if err != nil {
		return nil, err
	}
	response := nodes.ListCertificatesInfoResponse{raw}
	return &response, nil
}

func (n diskSiblingNodes) ListStorage(_ context.Context, node string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	n.client.record("status", node)
	response := make(nodes.ListStorageResponse, 0, len(n.client.available))
	for _, id := range []string{"a", "b"} {
		raw, err := json.Marshal(map[string]any{"storage": id, "active": 1, "enabled": 1, "total": 100 << 30, "avail": n.client.available[id]})
		if err != nil {
			return nil, err
		}
		response = append(response, raw)
	}
	return &response, nil
}

func (n diskSiblingNodes) ListStorageContent(ctx context.Context, node, pool string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	n.client.record("content", node)
	return n.managedDiskTestNodes.ListStorageContent(ctx, node, pool, params)
}

func (n diskSiblingNodes) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	n.client.record("guests", node)
	return n.managedDiskTestNodes.ListQemu(ctx, node, params)
}

// The journal opens and admission runs over every node in the cluster, and both
// of them run before the plan exists. Planning keeps its own narrower scope,
// which here is the single configured node.
func TestManagedDiskOpensJournalAndAdmitsAcrossTheCluster(t *testing.T) {
	deps, selection, resolver, client, dir := managedDiskSiblingFixture(t, "least_utilized")
	client.clusterNodes = []string{"n1", "n2", "n3"}
	identity, err := pve.ObserveStorageClusterIdentity(t.Context(), client.Nodes(), []string{"n1"})
	if err != nil {
		t.Fatal(err)
	}
	enrollment := aj.Enrollment{ClusterID: identity.ID(), AuthorityID: "authority", AuditID: "complete-fixture-audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true}
	journal, err := aj.Initialize(t.Context(), dir, "namespace", enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	client.reset()
	args := []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}
	cid, err := createManagedDisk(t.Context(), deps, args, selection, 1025, createDiskCloudProperties{}, "", resolver)
	if err != nil {
		t.Fatalf("create managed disk: %v", err)
	}
	if _, ok := cid.(string); !ok {
		t.Fatalf("create managed disk returned %T, want a CID string", cid)
	}
	if certificates := client.nodesFor("certificates"); !slices.Equal(certificates, client.clusterNodes) {
		t.Fatalf("journal identity observed %v, want every cluster node %v", certificates, client.clusterNodes)
	}
	// The audit lists storage content, planning reads node storage status, and
	// the audit has to have finished before the first ranking input is read.
	content, status := client.firstCall("content"), client.firstCall("status")
	if content < 0 || status < 0 || content > status {
		t.Fatalf("admission content read at %d and planning status read at %d; admission must come first", content, status)
	}
	for _, node := range []string{"n2", "n3"} {
		if !slices.Contains(client.nodesFor("content"), node) {
			t.Fatalf("admission skipped node %q; it reached %v", node, client.nodesFor("content"))
		}
	}
	if planned := client.nodesFor("status"); !slices.Equal(planned, []string{"n1"}) {
		t.Fatalf("planning discovered %v, want only the configured node", planned)
	}
}
