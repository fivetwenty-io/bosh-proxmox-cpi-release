package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
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
