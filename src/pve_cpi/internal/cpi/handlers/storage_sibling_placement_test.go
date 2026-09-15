package handlers

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// siblingMemberCapacity is one member's observed capacity, which is what
// decides the order least_utilized puts the two members in.
type siblingMemberCapacity struct{ total, available uint64 }

// siblingPlacementFixture ranks the root over both members of set E under
// least_utilized. The shared plan fixture otherwise pins the root to storage a
// and never ranks the set, and its spread strategy ignores free space, so
// neither would show a sibling's claim moving a placement.
func siblingPlacementFixture(t *testing.T, members map[string]siblingMemberCapacity,
	anti *config.StorageAntiAffinity) StoragePlanRequest {
	t.Helper()
	request, _, _ := planFixture(t, func(source *planFixtureSource, cfg *config.CPIConfig) {
		cfg.RootStorageSet = "E"
		cfg.StorageSets["E"] = config.StorageSet{
			Names:        []string{"a", "b"},
			Strategy:     config.StoragePlacementStrategy{Name: "least_utilized", Version: 1},
			AntiAffinity: anti,
		}
		// planFixture appends one status per node in storage order, so index 0
		// is member a and index 1 is member b.
		for index, id := range []string{"a", "b"} {
			capacity, declared := members[id]
			if !declared {
				continue
			}
			for _, node := range []string{"n1", "n2"} {
				source.statuses[node][index] = planJSON(t, map[string]any{
					"storage": id, "active": 1, "enabled": 1,
					"total": capacity.total, "avail": capacity.available,
				})
			}
		}
	})
	return request
}

// siblingPlacementJournal enrolls an allocation journal over a temporary
// directory, so these tests read the records a concurrent create would really
// have written rather than synthetic ones.
func siblingPlacementJournal(t *testing.T, namespace string) *aj.Journal {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(t.Context(), dir, namespace, aj.Enrollment{
		ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit",
		CompleteHistoricalAudit: true, PreviousWriterFenced: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	return journal
}

// siblingPlacementPeer ranks one allocation for agentID and writes its record,
// which is the state a create that started moments earlier leaves behind.
func siblingPlacementPeer(t *testing.T, journal *aj.Journal, request StoragePlanRequest, agentID string) *aj.Handle {
	t.Helper()
	plan := siblingPlanFor(t, request)
	shape := &createVMShape{node: plan.Node, vmStorage: plan.Targets[0].StorageID,
		rootDiskKey: "virtio0", rootDiskGiB: 5, vmDiskFormat: "qcow2"}
	execution, err := freezeManagedVMExecution(request.Selection.Policy, shape)
	if err != nil {
		t.Fatal(err)
	}
	plan.VMExecution = execution
	intent, err := storageJournalIntent("create_vm", []json.RawMessage{json.RawMessage(`{}`)},
		request.Selection, request.Inventory, plan)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.AcquireVM(t.Context(), agentID, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	return handle
}

// siblingPlacementSelf stands for the allocation identifier a first placement
// mints, which never matches a record already in the journal.
const siblingPlacementSelf = "alloc-self"

// siblingPlacementRank charges a set of journal records against one request and
// ranks it through the two halves the first-placement callback runs.
func siblingPlacementRank(t *testing.T, request StoragePlanRequest, records []aj.Record,
	group string) *managedVMPlan {
	t.Helper()
	observed := &managedVMPlan{selection: request.Selection, inventory: request.Inventory,
		request: request, snapshotStart: request.Clock(), group: group}
	observed.request.Group = group
	if err := applyManagedVMSiblings(observed, records, siblingPlacementSelf); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Config: request.Selection.Policy, Logger: log.NewNopLogger()}
	if err := rankManagedVMPlan(t.Context(), deps, &createVMParsedArgs{}, observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func siblingPlacementRootStorage(t *testing.T, record aj.Record) string {
	t.Helper()
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := managedVMRoleTarget(plan, storageRoleRoot)
	if !ok {
		t.Fatalf("allocation %s froze no root target", record.ID)
	}
	return root.StorageID
}

func siblingPlacementRootStorageOf(t *testing.T, observed *managedVMPlan) string {
	t.Helper()
	root, ok := managedVMRoleTarget(observed.plan, storageRoleRoot)
	if !ok {
		t.Fatalf("plan froze no root target: %+v", observed.plan)
	}
	return root.StorageID
}

const siblingPlacementGiB = uint64(1) << 30

// A peer whose record is already in the journal has to be charged against the
// next placement, which then lands on the other member.
func TestManagedVMPlacementChargesAnInFlightSibling(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}, nil)
	if got := siblingPlacementRootStorageOf(t, siblingPlacementRank(t, request, nil, "")); got != "a" {
		t.Fatalf("fixture baseline chose %q, want a", got)
	}
	journal := siblingPlacementJournal(t, request.Namespace)
	peer := siblingPlacementPeer(t, journal, request, "peer")
	if got := siblingPlacementRootStorage(t, peer.Record()); got != "a" {
		t.Fatalf("peer landed on %q, want a", got)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	observed := siblingPlacementRank(t, request, records, "")
	if got := siblingPlacementRootStorageOf(t, observed); got != "b" {
		t.Fatalf("second placement chose %q, want b", got)
	}
	key := siblingCapacityKey(t, request, "a")
	if observed.request.SiblingMemberBytes[key] == 0 {
		t.Fatalf("request carries %v, want the peer's claim on %q",
			observed.request.SiblingMemberBytes, key)
	}
}

// Member a is the least utilized of the two but holds only six gibibytes, so
// one peer's claim leaves it unable to take a second root. The planner has to
// find member b rather than fail the placement on capacity.
func TestManagedVMPlacementFindsRoomWhenASiblingFillsTheWinner(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 8 * siblingPlacementGiB, available: 6 * siblingPlacementGiB},
		"b": {total: 1000 * siblingPlacementGiB, available: 100 * siblingPlacementGiB},
	}, nil)
	if got := siblingPlacementRootStorageOf(t, siblingPlacementRank(t, request, nil, "")); got != "a" {
		t.Fatalf("fixture baseline chose %q, want a", got)
	}
	journal := siblingPlacementJournal(t, request.Namespace)
	peer := siblingPlacementPeer(t, journal, request, "peer")
	if got := siblingPlacementRootStorage(t, peer.Record()); got != "a" {
		t.Fatalf("peer landed on %q, want a", got)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	observed := siblingPlacementRank(t, request, records, "")
	if got := siblingPlacementRootStorageOf(t, observed); got != "b" {
		t.Fatalf("second placement chose %q, want b", got)
	}
	// Member a was excluded by the capacity gate rather than merely outranked,
	// and the plan says so where an operator can read it.
	if !strings.Contains(strings.Join(observed.plan.Rejections, "; "), "member a admission") {
		t.Fatalf("no rejection names member a: %q", observed.plan.Rejections)
	}
}

// A peer that reached a resting state charges nothing, because the bytes it
// holds are already in the capacity the snapshot reports. This is the
// assertion that keeps the accounting from turning into an accumulator.
func TestManagedVMPlacementIgnoresASettledSibling(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}, nil)
	journal := siblingPlacementJournal(t, request.Namespace)
	peer := siblingPlacementPeer(t, journal, request, "peer")
	siblingPlacementSettle(t, peer)
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	observed := siblingPlacementRank(t, request, records, "")
	if got := siblingPlacementRootStorageOf(t, observed); got != "a" {
		t.Fatalf("a settled peer moved the placement to %q, want a", got)
	}
	if len(observed.request.SiblingMemberBytes) != 0 {
		t.Fatalf("a settled peer charged %v", observed.request.SiblingMemberBytes)
	}
}

// siblingPlacementSettle walks a peer allocation to its resting state the way
// the create path walks it: it records the root volume, observes it as
// acquired, and only then returns the CID. Writing the end state directly
// would skip the transition the journal enforces.
func siblingPlacementSettle(t *testing.T, peer *aj.Handle) {
	t.Helper()
	plan, err := activeStorageAllocationPlan(peer.Record())
	if err != nil {
		t.Fatal(err)
	}
	root, ok := managedVMRoleTarget(plan, storageRoleRoot)
	if !ok {
		t.Fatalf("peer froze no root target: %+v", plan)
	}
	volume := root.StorageID + ":101/vm-101-disk-0.qcow2"
	step, err := storageMutationIntent(peer, "vm.root.virtio0", aj.Target{
		Node: root.Node, VMID: 101, Storage: root.StorageID,
		Backing: root.BackingKey, IntendedVolume: volume,
	}, plan.Charges)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(peer, step, []string{volume}, true); err != nil {
		t.Fatal(err)
	}
	settled := peer.Record()
	settled.State = aj.ReadyToReturn
	settled.CID = "101"
	if err := peer.Save(settled); err != nil {
		t.Fatal(err)
	}
}

// The anti-affinity scope comes from the root role's own set, so an operator
// who declares the none scope on that set stops the count without touching any
// other set or any other role.
func TestManagedVMPlacementReadsTheScopeFromTheRootSet(t *testing.T) {
	members := map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}
	cases := []struct {
		name   string
		anti   *config.StorageAntiAffinity
		counts bool
	}{
		{name: "a set that declares nothing takes the default scope", counts: true},
		{
			name: "the none scope counts no siblings at all",
			anti: &config.StorageAntiAffinity{Scope: config.StorageAntiAffinityScopeNone},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := siblingPlacementFixture(t, members, testCase.anti)
			grouped := request
			grouped.Group = antiAffinityGroup
			journal := siblingPlacementJournal(t, request.Namespace)
			peer := siblingPlacementPeer(t, journal, grouped, "peer")
			if got := siblingPlacementRootStorage(t, peer.Record()); got != "a" {
				t.Fatalf("peer landed on %q, want a", got)
			}
			records, err := journal.List()
			if err != nil {
				t.Fatal(err)
			}
			observed := siblingPlacementRank(t, request, records, antiAffinityGroup)
			counts := observed.request.SiblingGroupCounts
			if !testCase.counts {
				if counts != nil {
					t.Fatalf("the none scope still counted %v", counts)
				}
				return
			}
			if counts[siblingCapacityKey(t, request, "a")] != 1 {
				t.Fatalf("counts %v miss the peer on member a", counts)
			}
		})
	}
}

// siblingPlacementEnv builds the create_vm env shape the group derivation
// reads, a BOSH group name beside the group names it decomposes into.
func siblingPlacementEnv(group string, groups ...string) map[string]any {
	names := make([]any, 0, len(groups)+1)
	for _, name := range groups {
		names = append(names, name)
	}
	return map[string]any{"bosh": map[string]any{"group": group, "groups": append(names, group)}}
}

func TestManagedVMStorageGroup(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]any
		createEnv string
		want      string
	}{
		{
			name: "a deployed instance group names both halves",
			env:  siblingPlacementEnv("bosh-cf-diego-cell", "bosh", "cf", "diego-cell"),
			want: "deployment--cf/instance-group--diego-cell",
		},
		{
			name: "characters outside the tag alphabet become dashes",
			env:  siblingPlacementEnv("bosh-cf_prod.eu-diego_cell", "bosh", "cf_prod.eu", "diego_cell"),
			want: "deployment--cf-prod-eu/instance-group--diego-cell",
		},
		{
			name:      "a create-env deployment with no instance group has no group",
			env:       map[string]any{},
			createEnv: "bosh-1",
		},
		{name: "an env with no bosh block has no group"},
		{
			name: "a deployment name that sanitizes to nothing has no group",
			env:  siblingPlacementEnv("bosh-x-web", "bosh", "___", "web"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &config.CPIConfig{CreateEnvDeployment: testCase.createEnv}
			if got := managedVMStorageGroup(cfg, testCase.env); got != testCase.want {
				t.Fatalf("group is %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestManagedVMAntiAffinityScopeFallsBackToTheDefault(t *testing.T) {
	if got := managedVMAntiAffinityScope(nil); got != config.DefaultStorageAntiAffinityScope {
		t.Fatalf("a nil selection reports scope %q", got)
	}
	if got := managedVMAntiAffinityScope(&StoragePlacementSelection{}); got != config.DefaultStorageAntiAffinityScope {
		t.Fatalf("a selection with no root role reports scope %q", got)
	}
	set := config.StorageSet{AntiAffinity: &config.StorageAntiAffinity{
		Scope: config.StorageAntiAffinityScopeDeployment,
	}}
	selection := &StoragePlacementSelection{Root: &StorageRoleSelection{Set: &set}}
	if got := managedVMAntiAffinityScope(selection); got != config.StorageAntiAffinityScopeDeployment {
		t.Fatalf("the root set's scope is %q", got)
	}
}

// The first placement ranks inside the journal's planning callback, so the
// record it writes already accounts for every sibling the journal held when
// the index lock was taken.
func TestManagedVMFirstPlacementRanksInsideTheCallback(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}, nil)
	journal := siblingPlacementJournal(t, request.Namespace)
	peer := siblingPlacementPeer(t, journal, request, "peer")
	if got := siblingPlacementRootStorage(t, peer.Record()); got != "a" {
		t.Fatalf("peer landed on %q, want a", got)
	}
	prepared := &managedVMPlan{selection: request.Selection, inventory: request.Inventory,
		request: request, snapshotStart: request.Clock(), group: antiAffinityGroup}
	prepared.request.Group = antiAffinityGroup
	placement := &managedVMFirstPlacement{
		deps:      Deps{Config: request.Selection.Policy, Logger: log.NewNopLogger()},
		parsed:    &createVMParsedArgs{agentID: "agent"},
		selection: request.Selection,
		prepared:  prepared,
		args:      []json.RawMessage{json.RawMessage(`{}`)},
	}
	ran := 0
	var saw []aj.Record
	callback := func(id string, siblings []aj.Record) (aj.Intent, error) {
		ran++
		saw = siblings
		return placement.plan(t.Context(), id, siblings)
	}
	handle, err := journal.AcquireVMPlanned(t.Context(), "agent", callback)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	if ran != 1 {
		t.Fatalf("the planning callback ran %d times", ran)
	}
	if len(saw) != 1 || saw[0].ID != peer.Record().ID {
		t.Fatalf("the callback saw %d records, want the peer's alone", len(saw))
	}
	if placement.shape == nil || placement.shape.vmStorage != "b" {
		t.Fatalf("the callback left shape %+v", placement.shape)
	}
	written, err := activeStorageAllocationPlan(handle.Record())
	if err != nil {
		t.Fatal(err)
	}
	if got := siblingPlacementRootStorage(t, handle.Record()); got != "b" {
		t.Fatalf("the record froze member %q, want b", got)
	}
	if written.Group != antiAffinityGroup {
		t.Fatalf("the record froze group %q, want %q", written.Group, antiAffinityGroup)
	}
	if written.VMExecution == nil {
		t.Fatal("the callback froze no VM execution")
	}
}

// Two first placements that start at the same moment have to see one another,
// and the journal's index lock is what makes that true. Each ranks inside its
// own planning callback, so the one that takes the lock second reads the
// record the first wrote and charges it before it ranks. The journal owns the
// same assertion one layer down; this one pins the handler shape that
// createManagedVM builds around it.
func TestManagedVMConcurrentFirstPlacementsRankAgainstEachOther(t *testing.T) {
	request := siblingPlacementFixture(t, map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}, nil)
	journal := siblingPlacementJournal(t, request.Namespace)
	type attempt struct {
		agent     string
		planned   string
		siblings  []string
		handle    *aj.Handle
		err       error
		placement *managedVMFirstPlacement
		calls     atomic.Int32
	}
	attempts := []*attempt{{agent: "agent-one"}, {agent: "agent-two"}}
	for _, current := range attempts {
		// Every placement carries its own prepared plan, because the callback
		// ranks into it. What they share was frozen before either started.
		prepared := &managedVMPlan{selection: request.Selection, inventory: request.Inventory,
			request: request, snapshotStart: request.Clock()}
		current.placement = &managedVMFirstPlacement{
			deps:      Deps{Config: request.Selection.Policy, Logger: log.NewNopLogger()},
			parsed:    &createVMParsedArgs{agentID: current.agent},
			selection: request.Selection,
			prepared:  prepared,
			args:      []json.RawMessage{json.RawMessage(`{}`)},
		}
	}
	var waiting sync.WaitGroup
	for _, current := range attempts {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			current.handle, current.err = journal.AcquireVMPlanned(t.Context(), current.agent,
				func(id string, siblings []aj.Record) (aj.Intent, error) {
					current.calls.Add(1)
					current.planned = id
					for _, record := range siblings {
						current.siblings = append(current.siblings, record.ID)
					}
					return current.placement.plan(t.Context(), id, siblings)
				})
		}()
	}
	waiting.Wait()
	t.Cleanup(func() {
		for _, current := range attempts {
			if current.handle == nil {
				continue
			}
			if err := current.handle.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	for _, current := range attempts {
		if current.err != nil {
			t.Fatalf("%s: %v", current.agent, current.err)
		}
		if calls := current.calls.Load(); calls != 1 {
			t.Fatalf("%s planned %d times, want exactly one", current.agent, calls)
		}
		if got := current.handle.Record().ID; got != current.planned {
			t.Fatalf("%s recorded %s, planned %s", current.agent, got, current.planned)
		}
	}
	first, second := attempts[0], attempts[1]
	if len(first.siblings) > len(second.siblings) {
		first, second = second, first
	}
	if len(first.siblings) != 0 {
		t.Fatalf("the first placement saw siblings %v in an empty journal", first.siblings)
	}
	if len(second.siblings) != 1 || second.siblings[0] != first.planned {
		t.Fatalf("the second placement saw %v, want the first record %s alone",
			second.siblings, first.planned)
	}
	// Member a is the least utilized of the two, so the first placement takes
	// it and the second has to rank onto member b.
	if got := siblingPlacementRootStorage(t, first.handle.Record()); got != "a" {
		t.Fatalf("the first placement froze member %q, want a", got)
	}
	if got := siblingPlacementRootStorage(t, second.handle.Record()); got != "b" {
		t.Fatalf("the second placement froze member %q, want b", got)
	}
}

// A namespace holding no sibling records has to place exactly as the release
// before this one placed. The sibling-aware path runs in full here, over an
// empty journal, and the plan it produces has to encode byte for byte as the
// plan a request carrying no sibling fields and no group produces. This is the
// regression guard for every deployment that upgrades into the feature.
func TestManagedVMPlacementOverAnEmptyJournalMatchesTheUnseededPlan(t *testing.T) {
	members := map[string]siblingMemberCapacity{
		"a": {total: 100 * siblingPlacementGiB, available: 83 * siblingPlacementGiB},
		"b": {total: 100 * siblingPlacementGiB, available: 80 * siblingPlacementGiB},
	}
	cases := []struct {
		name    string
		request func(*testing.T) StoragePlanRequest
	}{
		{
			name: "the shared fixture, whose root is pinned to one storage",
			request: func(t *testing.T) StoragePlanRequest {
				t.Helper()
				request, _, _ := planFixture(t, nil)
				return request
			},
		},
		{
			name: "a root ranked over both members of a set",
			request: func(t *testing.T) StoragePlanRequest {
				t.Helper()
				return siblingPlacementFixture(t, members, nil)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := testCase.request(t)
			journal := siblingPlacementJournal(t, request.Namespace)
			records, err := journal.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 0 {
				t.Fatalf("a fresh journal already holds %d records", len(records))
			}
			observed := siblingPlacementRank(t, request, records, "")
			seeded, err := json.Marshal(observed.plan)
			if err != nil {
				t.Fatal(err)
			}
			// The pre-change shape: no sibling maps, no counts, and no group.
			bare := request
			bare.SiblingMemberBytes = nil
			bare.SiblingDomainBytes = nil
			bare.SiblingGroupCounts = nil
			bare.Group = ""
			unseeded, err := json.Marshal(siblingPlanFor(t, bare))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(seeded, unseeded) {
				t.Fatalf("an empty journal moved the plan:\n%s\n%s", seeded, unseeded)
			}
			if strings.Contains(string(seeded), `"Group"`) {
				t.Fatalf("an empty group reached the persisted plan: %s", seeded)
			}
		})
	}
}
