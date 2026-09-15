package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	rank "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

type planFixtureSource struct {
	mu       sync.Mutex
	defs     []json.RawMessage
	statuses map[string][]json.RawMessage
	reads    int
}

func (s *planFixtureSource) Definitions(context.Context) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.defs, nil
}
func (s *planFixtureSource) Statuses(_ context.Context, node string) ([]json.RawMessage, error) {
	return s.statuses[node], nil
}
func planJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func planFixture(t *testing.T, adjust func(*planFixtureSource, *config.CPIConfig)) (StoragePlanRequest, *inv.Collector, *planFixtureSource) {
	t.Helper()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	src := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	for _, id := range []string{"a", "b", "p", "source"} {
		src.defs = append(src.defs, planJSON(t, map[string]any{"storage": id, "type": "nfs", "shared": 1, "server": "nas", "export": "/" + id, "content": "images,iso,import"}))
		for _, node := range []string{"n1", "n2"} {
			src.statuses[node] = append(src.statuses[node], planJSON(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30}))
		}
	}
	cfg := &config.CPIConfig{VMStorage: "a", EphemeralStorageSet: "E", StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}, "P": {Names: []string{"p"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	if adjust != nil {
		adjust(src, cfg)
	}
	collector, e := inv.NewCollector(src, inv.Options{Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	companions := []string{"source"}
	if len(src.defs) > 4 {
		companions = append(companions, "local-source")
	}
	snap, e := collector.Discover(context.Background(), cfg, inv.Request{Nodes: []string{"n1", "n2"}, CompanionStorageIDs: companions})
	if e != nil {
		t.Fatal(e)
	}
	selected, e := ResolveStoragePlacementSelectors(cfg, "create_vm", nil, false)
	if e != nil {
		t.Fatal(e)
	}
	return StoragePlanRequest{Selection: selected, Inventory: snap, Groups: []StoragePlanNodeGroup{{AZ: "z", Nodes: []string{"n1", "n2"}}}, Namespace: "director", AllocationKey: "agent", SeedSet: true, RootBytes: 1 << 30, Sources: []StorageRootSource{{Node: "n1", StorageID: "source", VolumeID: "source:import/stemcell.qcow2", VirtualBytes: 5 << 30}}, SearchBudget: 100, Clock: func() time.Time { return now }}, collector, src
}
func TestStoragePlanStorageBeforeNodeAndNoMutation(t *testing.T) {
	r, _, src := planFixture(t, nil)
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if plan.Targets[0].VirtualBytes != 5<<30 || plan.Targets[0].Mechanism != "import" {
		t.Fatalf("source facts lost: %+v", plan.Targets[0])
	}
	if src.reads != 1 {
		t.Fatalf("planner rediscovered: %d", src.reads)
	}
	encoded, _ := json.Marshal(plan)
	if strings.Contains(string(encoded), "Password") {
		t.Fatal("secret config serialized")
	}
	it.MarkSubmitted()
	_, e = it.Next(context.Background())
	var typed *StoragePlanError
	if !errors.As(e, &typed) || typed.Kind != StoragePlanReconciliation || typed.CPI().OkToRetry() {
		t.Fatalf("unsafe submitted retry: %v", e)
	}
}
func TestStoragePlanComputeWinnerCannotReachBacking(t *testing.T) {
	r, _, _ := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.StorageSets["E"] = config.StorageSet{Names: []string{"a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}
		s.statuses["n1"][0] = planJSON(t, map[string]any{"storage": "a", "active": 0, "enabled": 1, "total": 100 << 30, "avail": 80 << 30})
	})
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if p.Node != "n2" {
		t.Fatalf("picked inaccessible compute winner: %s", p.Node)
	}
}
func TestStoragePlanNodeMultiplicityDoesNotBiasRank(t *testing.T) {
	r, _, _ := planFixture(t, nil)
	first := func(req StoragePlanRequest) string {
		t.Helper()
		it, e := NewStoragePlanIterator(req)
		if e != nil {
			t.Fatal(e)
		}
		p, e := it.Next(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		return p.Targets[0].StorageID
	}
	want := first(r)
	r.Groups[0].Nodes = []string{"n2", "n1", "n2", "n1"}
	if got := first(r); got != want {
		t.Fatalf("node multiplicity changed backing %s -> %s", want, got)
	}
}

func TestStoragePlanDifferentNodeMechanismCostsUseConservativeBackingWeight(t *testing.T) {
	r, _, _ := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.StorageSets["E"] = config.StorageSet{Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "weighted_free_space", Version: 1}}
		s.defs = append(s.defs, planJSON(t, map[string]any{"storage": "local-source", "type": "dir", "shared": 0, "path": "/local", "content": "images", "nodes": "n2"}))
		s.statuses["n2"] = append(s.statuses["n2"], planJSON(t, map[string]any{"storage": "local-source", "active": 1, "enabled": 1, "total": 100 << 30, "avail": 80 << 30}))
	})
	// Add a measured local template through the same frozen discovery request.
	// Node n1 imports five GiB; n2 clones twelve GiB including auxiliary disks.
	r.Sources = append(r.Sources, StorageRootSource{Node: "n2", StorageID: "local-source", VolumeID: "local-source:100/base-100-disk-0.raw", TemplateVMID: 100, VirtualBytes: 5 << 30, ScratchBytes: 7 << 30})
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if p.Targets[0].Source.StorageID != "source" {
		t.Fatal("undiscovered alternate source used")
	}
	if p.Rankings[0].Candidate.Member.AllocationBytes != 12<<30 {
		t.Fatal("node-dependent cost was not frozen conservatively")
	}
}

func TestStoragePlanBundledDisksHaveSeparateConservedCharges(t *testing.T) {
	r, _, _ := planFixture(t, nil)
	selection, e := ResolveStoragePlacementSelectors(r.Selection.Policy, "create_vm", nil, true)
	if e != nil {
		t.Fatal(e)
	}
	r.Selection = selection
	r.EphemeralBytes = 2 << 30
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Targets) != 2 || p.Targets[0].StorageID != p.Targets[1].StorageID {
		t.Fatalf("bundle split: %+v", p.Targets)
	}
	if len(p.Charges) != 2 || p.Rankings[0].Candidate.Member.AllocationBytes != 7<<30 {
		t.Fatalf("bundle rank/ledger not conserved: %+v", p)
	}
}

func TestStoragePlanConfigSnapshotAndUnknownCompanion(t *testing.T) {
	r, _, _ := planFixture(t, nil)
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	r.Selection.Root.Value = "missing"
	r.Groups[0].Nodes[0] = "missing"
	if _, e = it.Next(context.Background()); e != nil {
		t.Fatalf("caller changed immutable request: %v", e)
	}
	r, _, _ = planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.EphemeralStorageSet = ""
		c.VMStorage = "source"
		s.defs[3] = planJSON(t, map[string]any{"storage": "source", "type": "mystery", "content": "images", "shared": 1})
	})
	it, e = NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = it.Next(context.Background()); e == nil {
		t.Fatal("unknown companion backend accepted")
	}
}
func TestStoragePlanBudgetCancellationFreshness(t *testing.T) {
	for _, mode := range []string{"budget", "cancel", "stale"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := planFixture(t, nil)
			ctx := context.Background()
			want := StoragePlanObservation
			if mode == "budget" {
				r.SearchBudget = 1
				want = StoragePlanBudget
			}
			if mode == "cancel" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if mode == "stale" {
				r.Clock = func() time.Time { return time.Date(2026, 9, 8, 12, 1, 0, 0, time.UTC) }
			}
			it, e := NewStoragePlanIterator(r)
			if e != nil {
				t.Fatal(e)
			}
			_, e = it.Next(ctx)
			var pe *StoragePlanError
			if !errors.As(e, &pe) || pe.Kind != want {
				t.Fatalf("got %v want %s", e, want)
			}
		})
	}
}
func TestStoragePlanFuturePAndStrictDeployment(t *testing.T) {
	r, _, _ := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.PersistentStorageSet = "P"
		for _, node := range []string{"n1", "n2"} {
			s.statuses[node][2] = planJSON(t, map[string]any{"storage": "p", "active": 0, "enabled": 1, "total": 100 << 30, "avail": 80 << 30})
		}
	})
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = it.Next(context.Background()); e == nil {
		t.Fatal("inaccessible future P accepted")
	}
	if e = PreflightStorageDeployment(context.Background(), r.Inventory, []string{"n1", "n2"}, []string{"E", "P"}, nil); e == nil {
		t.Fatal("strict deployment preflight passed")
	}
}
func TestStoragePlanSplitDebitsDomain(t *testing.T) {
	r, _, _ := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"one": {Members: []string{"a", "b"}}}
		for _, node := range []string{"n1", "n2"} {
			for index, id := range []string{"a", "b"} {
				s.statuses[node][index] = planJSON(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": 10 << 30, "avail": 8 << 30})
			}
		}
	})
	selected, e := ResolveStoragePlacementSelectors(r.Selection.Policy, "create_vm", map[string]any{"root_disk_pool": "a", "ephemeral_disk_pool": "b"}, true)
	if e != nil {
		t.Fatal(e)
	}
	r.Selection = selected
	r.EphemeralBytes = 4 << 30
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = it.Next(context.Background()); e == nil {
		t.Fatal("split exceeded shared domain without root debit")
	}
}

type planVolumeReader struct {
	response *sdknodes.GetStorageContentResponse
	err      error
	calls    int
}

func (s *planVolumeReader) GetStorageContent(context.Context, string, string, string) (*sdknodes.GetStorageContentResponse, error) {
	s.calls++
	return s.response, s.err
}
func TestStoragePlanImportVirtualSize(t *testing.T) {
	reader := &planVolumeReader{response: &sdknodes.GetStorageContentResponse{Format: "qcow2", Size: sdkclient.PVEInt(9 << 30), Used: sdkclient.PVEInt(1 << 20)}}
	fact, e := ObserveStorageRootSource(context.Background(), reader, "n1", "source:import/disk.qcow2", 0)
	if e != nil {
		t.Fatal(e)
	}
	if fact.VirtualBytes != 9<<30 {
		t.Fatal("used/file bytes mistaken for virtual size")
	}
	for _, response := range []*sdknodes.GetStorageContentResponse{nil, {Format: "qcow2", Size: 0}, {Format: "qcow2", Size: -1}, {Format: "raw", Size: 5}} {
		reader.response = response
		if _, e = ObserveStorageRootSource(context.Background(), reader, "n1", "source:import/disk.qcow2", 0); e == nil {
			t.Fatalf("accepted %+v", response)
		}
	}
}
func TestStoragePlanDefinitionChangeStopsMutation(t *testing.T) {
	r, collector, source := planFixture(t, nil)
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ledger := inv.NewLedger()
	for _, charge := range p.Charges {
		ledger, e = ledger.WithPlanned(r.Inventory, charge.Charge)
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock); e != nil {
		t.Fatal(e)
	}
	id := p.Targets[0].StorageID
	for n, raw := range source.defs {
		var d map[string]any
		if e = json.Unmarshal(raw, &d); e != nil {
			t.Fatal(e)
		}
		if d["storage"] == id {
			d["export"] = "/repointed"
			source.defs[n] = planJSON(t, d)
		}
	}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock); e == nil {
		t.Fatal("repointed definition accepted")
	}
}

func TestStoragePlanISOUsesOriginalPinAndRevalidatesPolicy(t *testing.T) {
	r, collector, _ := planFixture(t, nil)
	r.ISOBytes = 1 << 20
	r.OriginalISOStorage = "source"
	r.ISOFollowRoot = true
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if p.Targets[len(p.Targets)-1].StorageID != "source" {
		t.Fatal("explicit original ISO pin replaced")
	}
	ledger := inv.NewLedger()
	for _, c := range p.Charges {
		ledger, e = ledger.WithPlanned(r.Inventory, c.Charge)
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock, StoragePlanInfrastructurePolicy{OriginalISOStorage: "source", FollowRoot: true}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock, StoragePlanInfrastructurePolicy{OriginalISOStorage: "a"}); e == nil {
		t.Fatal("restrictive infrastructure edit accepted")
	}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock); e == nil {
		t.Fatal("missing original infrastructure policy accepted")
	}
}

//nolint:gocognit // Keep the ordered inventory mutation matrix with its shared admission assertions.
func TestStoragePlanRevalidationContributingSourcesAndDomains(t *testing.T) {
	for _, change := range []string{"source-outage", "domain-repoint", "domain-policy"} {
		t.Run(change, func(t *testing.T) {
			r, collector, source := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
				c.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"shared": {Members: []string{"a", "b"}}}
			})
			it, e := NewStoragePlanIterator(r)
			if e != nil {
				t.Fatal(e)
			}
			p, e := it.Next(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			ledger := inv.NewLedger()
			for _, c := range p.Charges {
				ledger, e = ledger.WithPlanned(r.Inventory, c.Charge)
				if e != nil {
					t.Fatal(e)
				}
			}
			clock := r.Clock
			switch change {
			case "source-outage":
				for _, node := range []string{"n1", "n2"} {
					source.statuses[node][3] = planJSON(t, map[string]any{"storage": "source", "active": 0, "enabled": 1, "total": 100 << 30, "avail": 80 << 30})
				}
				later := clock().Add(time.Minute)
				clock = func() time.Time { return later }
				collector, e = inv.NewCollector(source, inv.Options{Clock: clock})
				if e != nil {
					t.Fatal(e)
				}
			case "domain-repoint":
				other := "a"
				if p.Targets[0].StorageID == "a" {
					other = "b"
				}
				for index, raw := range source.defs {
					var definition map[string]any
					if e = json.Unmarshal(raw, &definition); e != nil {
						t.Fatal(e)
					}
					if definition["storage"] == other {
						definition["export"] = "/changed"
						source.defs[index] = planJSON(t, definition)
					}
				}
			case "domain-policy":
				r.Selection.Policy.StorageCapacityDomains["shared"] = config.StorageCapacityDomain{Members: []string{"a"}}
			}
			if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, clock); e == nil {
				t.Fatalf("accepted %s", change)
			}
		})
	}
}

func TestStoragePlanCurrentHeadroomAndStalePreflight(t *testing.T) {
	r, collector, _ := planFixture(t, nil)
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ledger := inv.NewLedger()
	for _, c := range p.Charges {
		ledger, e = ledger.WithPlanned(r.Inventory, c.Charge)
		if e != nil {
			t.Fatal(e)
		}
	}
	enabled := true
	margin := 90 * 1024
	r.Selection.Policy.Placement = &config.PlacementConfig{ReserveStorageHeadroom: &enabled, StorageHeadroomMB: &margin}
	if _, _, e = RevalidateStorageAllocationPlan(context.Background(), collector, r.Inventory, r.Inventory, r.Selection, p, ledger, r.Clock); e == nil {
		t.Fatal("tightened global headroom ignored")
	}
	if e = PreflightStorageDeployment(context.Background(), r.Inventory, []string{"n1"}, []string{"E"}, nil, func() time.Time { return r.Clock().Add(time.Minute) }); e == nil {
		t.Fatal("stale deployment preflight passed")
	}
}

// siblingPlanRequest ranks the root over both members of set E under
// least_utilized, because the fixture otherwise pins the root to storage a.
func siblingPlanRequest(t *testing.T) StoragePlanRequest {
	t.Helper()
	r, _, _ := planFixture(t, func(_ *planFixtureSource, c *config.CPIConfig) {
		c.RootStorageSet = "E"
		c.StorageSets["E"] = config.StorageSet{Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "least_utilized", Version: 1}}
	})
	return r
}

func siblingPlanFor(t *testing.T, r StoragePlanRequest) *StorageAllocationPlan {
	t.Helper()
	it, e := NewStoragePlanIterator(r)
	if e != nil {
		t.Fatal(e)
	}
	p, e := it.Next(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestStoragePlanWithoutSiblingBytesIsUnchanged(t *testing.T) {
	r := siblingPlanRequest(t)
	base := siblingPlanFor(t, r)
	seeded := r
	seeded.SiblingMemberBytes = map[string]uint64{}
	seeded.SiblingDomainBytes = map[string]uint64{}
	seeded.SiblingGroupCounts = map[string]int{}
	first, e := json.Marshal(base)
	if e != nil {
		t.Fatal(e)
	}
	second, e := json.Marshal(siblingPlanFor(t, seeded))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("empty sibling maps moved the plan:\n%s\n%s", first, second)
	}
	if strings.Contains(string(first), `"Group"`) {
		t.Fatalf("an empty group was marshaled into the plan: %s", first)
	}
	if base.Group != "" {
		t.Fatalf("plan invented a group: %q", base.Group)
	}
}

func TestStoragePlanCarriesTheRequestGroup(t *testing.T) {
	const group = "deployment--cf/instance-group--diego-cell"
	r := siblingPlanRequest(t)
	r.Group = group
	p := siblingPlanFor(t, r)
	if p.Group != group {
		t.Fatalf("plan lost the group: %q", p.Group)
	}
	encoded, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(encoded), `"Group":"`+group+`"`) {
		t.Fatalf("group missing from the persisted plan: %s", encoded)
	}
}

func TestStoragePlanSeedsTheRootLedgerWithSiblingBytes(t *testing.T) {
	r := siblingPlanRequest(t)
	base := siblingPlanFor(t, r)
	if base.Targets[0].Role != storageRoleRoot {
		t.Fatalf("fixture no longer ranks a root first: %+v", base.Targets[0])
	}
	chosen := base.Targets[0].StorageID
	pair, ok := r.Inventory.Pair("n1", chosen)
	if !ok {
		t.Fatalf("fixture lost storage %q", chosen)
	}
	// Claim nearly every byte of the winner's backing on behalf of a peer. The
	// seeding is the only thing that changes, so the root has to move.
	seeded := r
	seeded.SiblingMemberBytes = map[string]uint64{pair.CapacityKey: 79 << 30}
	moved := siblingPlanFor(t, seeded)
	if moved.Targets[0].StorageID == chosen {
		t.Fatalf("sibling bytes never reached the root ledger: %+v", moved.Targets[0])
	}
	// A key no candidate owns leaves the ranking exactly where it was.
	elsewhere := r
	elsewhere.SiblingMemberBytes = map[string]uint64{pair.CapacityKey + "-elsewhere": 79 << 30}
	if got := siblingPlanFor(t, elsewhere).Targets[0].StorageID; got != chosen {
		t.Fatalf("an unrelated capacity key moved the root: %s", got)
	}
}

// antiAffinityGroup is the shape prepareManagedVMPlan builds, a sanitized
// deployment and instance group pair joined by a slash.
const antiAffinityGroup = "deployment--cf/instance-group--diego-cell"

// antiAffinityRequest ranks the root over both members of set E under
// least_utilized, so headroom decides the order the partition then reworks.
func antiAffinityRequest(t *testing.T, anti *config.StorageAntiAffinity, adjust func(*planFixtureSource)) StoragePlanRequest {
	t.Helper()
	r, _, _ := planFixture(t, func(s *planFixtureSource, c *config.CPIConfig) {
		c.RootStorageSet = "E"
		c.StorageSets["E"] = config.StorageSet{
			Names:        []string{"a", "b"},
			Strategy:     config.StoragePlacementStrategy{Name: "least_utilized", Version: 1},
			AntiAffinity: anti,
		}
		if adjust != nil {
			adjust(s)
		}
	})
	return r
}

func siblingCapacityKey(t *testing.T, r StoragePlanRequest, id string) string {
	t.Helper()
	pair, ok := r.Inventory.Pair("n1", id)
	if !ok {
		t.Fatalf("fixture lost storage %q", id)
	}
	return pair.CapacityKey
}

func otherMember(id string) string {
	if id == "a" {
		return "b"
	}
	return "a"
}

func TestStoragePlanAntiAffinityPartitionOrdersBySiblingCount(t *testing.T) {
	preferred := siblingPlanFor(t, antiAffinityRequest(t, nil, nil)).Targets[0].StorageID
	other := otherMember(preferred)
	onPreferred := func(preferredKey, _ string) map[string]int { return map[string]int{preferredKey: 1} }
	onBoth := func(preferredKey, otherKey string) map[string]int {
		return map[string]int{preferredKey: 1, otherKey: 1}
	}
	cases := []struct {
		name       string
		group      string
		anti       *config.StorageAntiAffinity
		counts     func(preferredKey, otherKey string) map[string]int
		want       string
		wantReason string
	}{
		{name: "no counts leaves the strategy in charge", group: antiAffinityGroup, want: preferred},
		{
			name: "a sibling on the winner moves the root", group: antiAffinityGroup, counts: onPreferred,
			want: other, wantReason: "anti-affinity in band 20%, bucket 0, siblings 0",
		},
		{
			name: "a sibling on each member returns the strategy order", group: antiAffinityGroup, counts: onBoth,
			want: preferred, wantReason: "anti-affinity in band 20%, bucket 1, siblings 1",
		},
		{name: "a request with no group never partitions", counts: onPreferred, want: preferred},
		{
			name: "the none scope never partitions", group: antiAffinityGroup,
			anti:   &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeNone},
			counts: onPreferred, want: preferred,
		},
		{
			name: "the deployment scope still partitions", group: antiAffinityGroup,
			anti:   &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeDeployment},
			counts: onPreferred, want: other, wantReason: "anti-affinity in band 20%, bucket 0, siblings 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := antiAffinityRequest(t, tc.anti, nil)
			r.Group = tc.group
			if tc.counts != nil {
				r.SiblingGroupCounts = tc.counts(siblingCapacityKey(t, r, preferred), siblingCapacityKey(t, r, other))
			}
			plan := siblingPlanFor(t, r)
			if got := plan.Targets[0].StorageID; got != tc.want {
				t.Fatalf("root landed on %s, want %s", got, tc.want)
			}
			reason := plan.Rankings[0].Reason
			if reason == "" {
				t.Fatal("the strategy stopped explaining its own ordering")
			}
			if tc.wantReason == "" {
				if strings.Contains(reason, "anti-affinity") {
					t.Fatalf("an unpartitioned ranking claimed anti-affinity: %q", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("winner reason %q lacks %q", reason, tc.wantReason)
			}
		})
	}
}

func TestStoragePlanAntiAffinityBandBoundsThePreference(t *testing.T) {
	// planFixture appends one status per node in storage order, so index 1 is
	// member b. Leaving it nearly full puts it far outside the default band.
	fill := func(s *planFixtureSource) {
		for _, node := range []string{"n1", "n2"} {
			s.statuses[node][1] = planJSON(t, map[string]any{
				"storage": "b", "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(10) << 30,
			})
		}
	}
	wide := 100
	cases := []struct {
		name string
		anti *config.StorageAntiAffinity
		want string
	}{
		{name: "the default band leaves the full member behind", want: "a"},
		{
			name: "a band of one hundred points lets the sibling count decide",
			anti: &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeInstanceGroup, UtilizationBandPct: &wide},
			want: "b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := antiAffinityRequest(t, tc.anti, fill)
			r.Group = antiAffinityGroup
			// Member a holds the only sibling, and b holds none. Only the band
			// decides whether b's empty count is allowed to outrank a's headroom.
			r.SiblingGroupCounts = map[string]int{siblingCapacityKey(t, r, "a"): 1}
			if got := siblingPlanFor(t, r).Targets[0].StorageID; got != tc.want {
				t.Fatalf("root landed on %s, want %s", got, tc.want)
			}
		})
	}
}

// TestStoragePlanSpreadSetSpreadsThroughThePartitionOnly pins the claim the
// documentation makes. A spread set ranks by rendezvous hash, so sibling bytes
// can never move it and its spreading comes from the partition alone.
func TestStoragePlanSpreadSetSpreadsThroughThePartitionOnly(t *testing.T) {
	base, _, _ := planFixture(t, func(_ *planFixtureSource, c *config.CPIConfig) { c.RootStorageSet = "E" })
	chosen := siblingPlanFor(t, base).Targets[0].StorageID
	other := otherMember(chosen)
	key := siblingCapacityKey(t, base, chosen)
	// One peer, expressed first as the bytes it claimed and then as the record
	// it wrote. Ten gibibytes keep both members inside the default band, so the
	// second run turns on the bucket order and nothing else.
	bytesOnly := base
	bytesOnly.SiblingMemberBytes = map[string]uint64{key: 10 << 30}
	if got := siblingPlanFor(t, bytesOnly).Targets[0].StorageID; got != chosen {
		t.Fatalf("sibling bytes reordered a spread set: %s became %s", chosen, got)
	}
	partitioned := bytesOnly
	partitioned.Group = antiAffinityGroup
	partitioned.SiblingGroupCounts = map[string]int{key: 1}
	plan := siblingPlanFor(t, partitioned)
	if got := plan.Targets[0].StorageID; got != other {
		t.Fatalf("the partition never moved a spread set: root landed on %s, want %s", got, other)
	}
	if !strings.Contains(plan.Rankings[0].Reason, "anti-affinity in band 20%, bucket 0, siblings 0") {
		t.Fatalf("winner reason lost the partition evidence: %q", plan.Rankings[0].Reason)
	}
}

// TestStoragePlanSpreadSiblingBytesChangeFeasibilityOnly documents the one
// thing a sibling's claim can do to a spread set. The rendezvous hash reads
// the request tuple and the backing key and never reads free space, so bytes
// a peer claimed cannot reorder the members. They still reach the capacity
// gate, so a claim large enough to fill the winner excludes it and the other
// member takes the root. An operator reading the placement documentation
// should find exactly this behaviour, and this test fails if either half of
// it changes.
func TestStoragePlanSpreadSiblingBytesChangeFeasibilityOnly(t *testing.T) {
	base, _, _ := planFixture(t, func(_ *planFixtureSource, c *config.CPIConfig) { c.RootStorageSet = "E" })
	if name := base.Selection.Root.Set.Strategy.Name; name != "spread" {
		t.Fatalf("the fixture root set now ranks by %q, want spread", name)
	}
	chosen := siblingPlanFor(t, base).Targets[0].StorageID
	other := otherMember(chosen)
	key := siblingCapacityKey(t, base, chosen)
	// Each member reports eighty gibibytes available and the root charge is the
	// five gibibyte source. Forty gibibytes of peer claims leave the winner
	// comfortably able to take this root as well.
	feasible := base
	feasible.SiblingMemberBytes = map[string]uint64{key: 40 << 30}
	plan := siblingPlanFor(t, feasible)
	if got := plan.Targets[0].StorageID; got != chosen {
		t.Fatalf("sibling bytes reordered a spread set: %s became %s", chosen, got)
	}
	if len(plan.Rejections) != 0 {
		t.Fatalf("a member that still fits was rejected: %q", plan.Rejections)
	}
	// Seventy-nine gibibytes leave one gibibyte behind, which the five
	// gibibyte root no longer fits into.
	infeasible := base
	infeasible.SiblingMemberBytes = map[string]uint64{key: 79 << 30}
	moved := siblingPlanFor(t, infeasible)
	if got := moved.Targets[0].StorageID; got != other {
		t.Fatalf("a filled member still took the root: %s, want %s", got, other)
	}
	if !strings.Contains(strings.Join(moved.Rejections, "; "), "member "+chosen+" admission") {
		t.Fatalf("no rejection names member %s: %q", chosen, moved.Rejections)
	}
	if strings.Contains(moved.Rankings[0].Reason, "anti-affinity") {
		t.Fatalf("a request with no group claimed anti-affinity: %q", moved.Rankings[0].Reason)
	}
}

func TestStoragePlanDiskShapedRequestNeverPartitions(t *testing.T) {
	var cfg *config.CPIConfig
	r, _, _ := planFixture(t, func(_ *planFixtureSource, c *config.CPIConfig) {
		// The persistent set has to stay clear of VMStorage and of set E, so it
		// ranks the two companion shares instead.
		c.PersistentStorageSet = "P"
		c.StorageSets["P"] = config.StorageSet{
			Names:    []string{"p", "source"},
			Strategy: config.StoragePlacementStrategy{Name: "least_utilized", Version: 1},
		}
		cfg = c
	})
	selection, e := ResolveStoragePlacementSelectors(cfg, "create_disk", nil, false)
	if e != nil {
		t.Fatal(e)
	}
	r.Selection = selection
	r.Sources = nil
	r.RootBytes = 0
	r.PersistentBytes = 1 << 30
	plan := siblingPlanFor(t, r)
	if plan.Targets[0].Role != storageRolePersistent {
		t.Fatalf("fixture is not disk shaped: %+v", plan.Targets[0])
	}
	chosen := plan.Targets[0].StorageID
	// A namespace full of same-group records cannot reorder a persistent role,
	// because create_disk computes no group and the partition is root only.
	counted := r
	counted.Group = antiAffinityGroup
	counted.SiblingGroupCounts = map[string]int{siblingCapacityKey(t, r, chosen): 3}
	moved := siblingPlanFor(t, counted)
	if got := moved.Targets[0].StorageID; got != chosen {
		t.Fatalf("a disk-shaped request partitioned: %s became %s", chosen, got)
	}
	if strings.Contains(moved.Rankings[0].Reason, "anti-affinity") {
		t.Fatalf("a persistent ranking claimed anti-affinity: %q", moved.Rankings[0].Reason)
	}
}

func TestStoragePlanDedicatedEphemeralNeverPartitions(t *testing.T) {
	var cfg *config.CPIConfig
	r, _, _ := planFixture(t, func(_ *planFixtureSource, c *config.CPIConfig) {
		// A root set of its own keeps the two disks from bundling, so the
		// ephemeral disk gets a ranking call rather than the root's target.
		c.RootStorageSet = "R"
		c.StorageSets["R"] = config.StorageSet{
			Names:    []string{"p"},
			Strategy: config.StoragePlacementStrategy{Name: "least_utilized", Version: 1},
		}
		c.StorageSets["E"] = config.StorageSet{
			Names:    []string{"a", "b"},
			Strategy: config.StoragePlacementStrategy{Name: "least_utilized", Version: 1},
		}
		cfg = c
	})
	selection, e := ResolveStoragePlacementSelectors(cfg, "create_vm", nil, true)
	if e != nil {
		t.Fatal(e)
	}
	r.Selection = selection
	r.EphemeralBytes = 1 << 30
	plan := siblingPlanFor(t, r)
	if len(plan.Targets) < 2 || plan.Targets[1].Role != storageRoleEphemeral || len(plan.Rankings) < 2 {
		t.Fatalf("fixture no longer ranks a dedicated ephemeral disk: %+v", plan.Targets)
	}
	chosen := plan.Targets[1].StorageID
	// The ephemeral call is not primary, so counts on its winner leave it alone.
	counted := r
	counted.Group = antiAffinityGroup
	counted.SiblingGroupCounts = map[string]int{siblingCapacityKey(t, r, chosen): 3}
	moved := siblingPlanFor(t, counted)
	if got := moved.Targets[1].StorageID; got != chosen {
		t.Fatalf("the dedicated ephemeral call partitioned: %s became %s", chosen, got)
	}
	if strings.Contains(moved.Rankings[1].Reason, "anti-affinity") {
		t.Fatalf("an ephemeral ranking claimed anti-affinity: %q", moved.Rankings[1].Reason)
	}
}

func TestCollapseRepresentativeMergesConservativelyAndNamesWhatItDrops(t *testing.T) {
	const key = "nfs://nas/a"
	option := func(c rank.EligibleCandidate) storagePlanOption { return storagePlanOption{candidate: c} }
	member := func(outstanding, allocation uint64) rank.EligibleCandidate {
		return rank.EligibleCandidate{
			StorageID: "a", BackingKey: "stale",
			Member: rank.CapacityBudget{
				TotalBytes: 100 << 30, AvailableBytes: 80 << 30,
				OutstandingBytes: outstanding, AllocationBytes: allocation,
			},
		}
	}
	domain := func(outstanding, allocation uint64) rank.EligibleCandidate {
		c := member(0, 1<<30)
		c.DomainKey = "d"
		c.Domain = rank.CapacityBudget{
			TotalBytes: 100 << 30, AvailableBytes: 80 << 30,
			OutstandingBytes: outstanding, AllocationBytes: allocation,
		}
		return c
	}
	t.Run("the merged charge is the larger of each", func(t *testing.T) {
		got, projected, e := collapseRepresentative(key, []storagePlanOption{
			option(member(10<<30, 5<<30)), option(member(4<<30, 15<<30)),
		})
		if e != nil {
			t.Fatal(e)
		}
		if got.Member.OutstandingBytes != 10<<30 || got.Member.AllocationBytes != 15<<30 {
			t.Fatalf("merge understated the key: %+v", got.Member)
		}
		if got.BackingKey != key {
			t.Fatalf("representative kept a stale backing key: %q", got.BackingKey)
		}
		// Twenty gibibytes already used, plus the merged twenty-five.
		if projected.ProjectedUsedBytes != 45<<30 || projectedBudgetUtilizationPct(projected) != 45 {
			t.Fatalf("projection does not describe the merged charge: %+v", projected)
		}
	})
	t.Run("an infeasible member merge names the capacity key", func(t *testing.T) {
		_, _, e := collapseRepresentative(key, []storagePlanOption{
			option(member(60<<30, 5<<30)), option(member(0, 30<<30)),
		})
		if e == nil || !strings.Contains(e.Error(), "capacity key "+key+" cannot hold the combined charge") {
			t.Fatalf("the gate dropped the key without saying so: %v", e)
		}
	})
	t.Run("an infeasible domain merge names the domain and the key", func(t *testing.T) {
		_, _, e := collapseRepresentative(key, []storagePlanOption{
			option(domain(60<<30, 5<<30)), option(domain(0, 30<<30)),
		})
		if e == nil || !strings.Contains(e.Error(), "capacity domain d cannot hold the combined charge for "+key) {
			t.Fatalf("the domain gate dropped the key without saying so: %v", e)
		}
	})
	t.Run("an empty key is an error rather than a panic", func(t *testing.T) {
		if _, _, e := collapseRepresentative(key, nil); e == nil {
			t.Fatal("collapsing nothing succeeded")
		}
	})
}

// TestStoragePlanRejectionsReachThePlan proves the path every capacity
// exclusion now takes. A member the planner drops lands in the plan's
// rejections and raises the candidate rejection counter, so an operator can
// tell a member the ceiling excluded from one the ranking never saw.
func TestStoragePlanRejectionsReachThePlan(t *testing.T) {
	r := antiAffinityRequest(t, nil, func(s *planFixtureSource) {
		for _, node := range []string{"n1", "n2"} {
			s.statuses[node][0] = planJSON(t, map[string]any{
				"storage": "a", "active": 0, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30,
			})
		}
	})
	var roles []string
	r.OnCandidateRejected = func(role string) { roles = append(roles, role) }
	plan := siblingPlanFor(t, r)
	if plan.Targets[0].StorageID != "b" {
		t.Fatalf("fixture no longer excludes member a: %+v", plan.Targets[0])
	}
	if !strings.Contains(strings.Join(plan.Rejections, "; "), "storage a is not eligible") {
		t.Fatalf("an excluded member left no trace in the plan: %q", plan.Rejections)
	}
	if !slices.Contains(roles, storageRoleRoot) {
		t.Fatalf("the rejection never reached the candidate counter: %q", roles)
	}
}

func TestStorageAntiAffinityGroupKey(t *testing.T) {
	cases := []struct {
		name, group, scope, want string
	}{
		{
			"the instance group scope compares the whole pair",
			antiAffinityGroup, config.StorageAntiAffinityScopeInstanceGroup, antiAffinityGroup,
		},
		{
			"the deployment scope compares the deployment half",
			antiAffinityGroup, config.StorageAntiAffinityScopeDeployment, "deployment--cf",
		},
		{
			"a group with no instance group half is already a deployment key",
			"deployment--cf", config.StorageAntiAffinityScopeDeployment, "deployment--cf",
		},
		{"the none scope makes every allocation a stranger", antiAffinityGroup, config.StorageAntiAffinityScopeNone, ""},
		{"an unknown scope makes every allocation a stranger", antiAffinityGroup, "region", ""},
		{"an empty group has no key", "", config.StorageAntiAffinityScopeInstanceGroup, ""},
		{"a blank group has no key", "   ", config.StorageAntiAffinityScopeDeployment, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StorageAntiAffinityGroupKey(tc.group, tc.scope); got != tc.want {
				t.Fatalf("group key is %q, want %q", got, tc.want)
			}
		})
	}
}
