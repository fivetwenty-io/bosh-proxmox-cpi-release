package handlers

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// TestCreateVM_SetManagedCreateHoldsAndBindsItsClaim runs a set-managed
// create of a group that placement spreads, and checks that it holds a claim
// on the node it builds on while PVE creates the VM, that the claim names the
// VMID by then, so a sibling that counts the VM from its member tag leaves the
// claim out, and that the claim is gone once create_vm returns.
func TestCreateVM_SetManagedCreateHoldsAndBindsItsClaim(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	on := true
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E",
		StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory,
		AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport,
		Placement: &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}},
		StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"},
			Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{
		ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit",
		CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{
		"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
	client := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}},
		journal: journal}
	store := spreadClaimStore(t)
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger(), RequestDirectorUUID: aaDirector,
		SpreadClaims: func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return store }}

	created := false
	client.onCreate = func(node string, vmid int) {
		created = true
		untagged := []placement.NodeFacts{{Node: node}}
		if got := claimCounts(t, deps, untagged); !maps.Equal(got, map[string]int{node: 1}) {
			t.Errorf("claims while PVE creates the VM = %v; want this create's claim on %s", got, node)
		}
		tagged := []placement.NodeFacts{{Node: node, SameGroupCount: 1, SameGroupVMIDs: []int{vmid}}}
		if got := claimCounts(t, deps, tagged); len(got) != 0 {
			t.Errorf("claims once the tag counts VM %d = %v; want none, because the claim names the VMID",
				vmid, got)
		}
	}
	env, err := json.Marshal(aaEnvForWebGroup())
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(`"spread-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`),
		json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`),
		env}
	if _, err := createVM(t.Context(), deps, args); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the create never reached PVE")
	}
	if got := claimCounts(t, deps, []placement.NodeFacts{{Node: "n1"}}); len(got) != 0 {
		t.Fatalf("claims after create_vm returned = %v; want none", got)
	}
}

// managedSpreadPick plans one set-managed create of the web group over r, a
// request on shared storage, the way the managed path does. It ranks the
// compute nodes through the create's claim, plans the storage over that
// ranking, and moves the claim to the node the plan settles on.
func managedSpreadPick(deps Deps, spread *spreadClaims, r StoragePlanRequest) (string, error) {
	parsed := spreadParsed()
	parsed.spread = spread
	groups, err := ObserveStoragePlanCompute(context.Background(), deps, parsed, nil)
	if err != nil {
		return "", err
	}
	r.Groups = groups
	it, err := NewStoragePlanIterator(r)
	if err != nil {
		return "", err
	}
	plan, err := it.Next(context.Background())
	if err != nil {
		return "", err
	}
	spread.move(context.Background(), plan.Node)
	return plan.Node, nil
}

// TestManagedCreates_ParallelSiblingsOnSharedStorageSpreadAcrossNodes plans
// two set-managed creates of one group at the same moment on NFS storage that
// both nodes share, before either VM carries the member tag. The storage
// charges cannot tell the nodes apart, because both nodes share one capacity
// key. Without claims both creates pick the same node, and with them each
// pick sees the other's claim, so the two land on the two nodes.
func TestManagedCreates_ParallelSiblingsOnSharedStorageSpreadAcrossNodes(t *testing.T) {
	t.Parallel()
	on := true
	newDeps := func(store *spreadclaim.Store) Deps {
		deps := buildPlacementDeps([]map[string]any{onlineNode("n1"), onlineNode("n2")}, func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}}
		})
		deps.RequestDirectorUUID = aaDirector
		if store != nil {
			deps.SpreadClaims = func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return store }
		}
		return deps
	}

	request := func() StoragePlanRequest {
		r, _, _ := planFixture(t, nil)
		return r
	}
	plain := newDeps(nil)
	first, err := managedSpreadPick(plain, nil, request())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := managedSpreadPick(plain, nil, request()); err != nil || again != first {
		t.Fatalf("without claims the second create picked %q (%v); want %q, as the first did", again, err, first)
	}

	deps := newDeps(spreadClaimStore(t))
	claims := []*spreadClaims{mustSpreadClaims(t, deps), mustSpreadClaims(t, deps)}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		picks []string
	)
	start := make(chan struct{})
	for _, c := range claims {
		r := request()
		wg.Go(func() {
			<-start
			node, err := managedSpreadPick(deps, c, r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			picks = append(picks, node)
		})
	}
	close(start)
	wg.Wait()
	slices.Sort(picks)
	if want := []string{"n1", "n2"}; !slices.Equal(picks, want) {
		t.Fatalf("parallel set-managed creates picked %v; want one on each node, %v", picks, want)
	}
}

// TestCreateVM_ResumedSetManagedCreateHoldsAClaim runs a set-managed create
// whose first attempt closes, so the second create_vm resumes the journal
// record and builds the VM again through the closed-attempt retry. The
// resumed create must hold a claim on its node while PVE creates the VM,
// as the first attempt did, so siblings that place meanwhile count it.
func TestCreateVM_ResumedSetManagedCreateHoldsAClaim(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	on := true
	limit := 1
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E",
		StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory,
		AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport,
		Placement: &config.PlacementConfig{FallbackMax: &limit, AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}},
		StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"},
			Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{
		ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit",
		CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{
		"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
	// failStatus fails the first attempt after its root is created, and
	// pauseReplan fails the in-call re-plan after the cleanup, so the first
	// create_vm returns with the attempt closed.
	client := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}},
		journal: journal, failStatus: true, pauseReplan: true}
	store := spreadClaimStore(t)
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger(), RequestDirectorUUID: aaDirector,
		SpreadClaims: func(*config.CPIConfig, string, *log.Logger) *spreadclaim.Store { return store }}
	var seen []int
	client.onCreate = func(node string, _ int) {
		seen = append(seen, claimCounts(t, deps, []placement.NodeFacts{{Node: node}})[node])
	}
	env, err := json.Marshal(aaEnvForWebGroup())
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(`"spread-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`),
		json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`),
		env}
	if _, err := createVM(t.Context(), deps, args); err == nil {
		t.Fatal("the first create_vm succeeded; want it to stop after its cleanup")
	}
	closed, found, err := journal.InspectVM("spread-agent")
	if err != nil || !found || !managedVMAttemptClosed(closed) {
		t.Fatalf("the first create_vm left no closed attempt to resume (found %t): %v", found, err)
	}
	if _, err := createVM(t.Context(), deps, args); err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 1}; !slices.Equal(seen, want) {
		t.Fatalf("claims seen as PVE created each VM = %v; want %v, one claim at the first create and at "+
			"the resumed one", seen, want)
	}
	if got := claimCounts(t, deps, []placement.NodeFacts{{Node: "n1"}}); len(got) != 0 {
		t.Fatalf("claims after the resumed create_vm returned = %v; want none", got)
	}
}
