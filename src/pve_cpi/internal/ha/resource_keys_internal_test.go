package ha

import (
	"context"
	"slices"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The pve-manager releases the resource key tests run against. PVE 9.0 and
// 9.1 before 9.1.14 accept failback but not auto-rebalance, and 9.1.14 is the
// first release whose HA manager accepts both.
const (
	keysPVE90   = "9.0.11"
	keysPVE91   = "9.1.13"
	keysPVE9114 = "9.1.14"
	keysPVE92   = "9.2.4"
)

func keysOf(version string) ResourceKeys { return (&Profile{MinVersion: version}).ResourceKeys() }

func TestResourceKeysFollowTheLowestOnlineRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		want    ResourceKeys
	}{
		{"", ResourceKeys{}},
		{"8.4.1", ResourceKeys{}},
		{"9.0", ResourceKeys{Failback: true}},
		{keysPVE90, ResourceKeys{Failback: true}},
		{"9.1.0", ResourceKeys{Failback: true}},
		{keysPVE91, ResourceKeys{Failback: true}},
		{keysPVE9114, ResourceKeys{Failback: true, AutoRebalance: true}},
		{"9.1.19~1", ResourceKeys{Failback: true, AutoRebalance: true}},
		{"9.2.0", ResourceKeys{Failback: true, AutoRebalance: true}},
		{keysPVE92, ResourceKeys{Failback: true, AutoRebalance: true}},
		{"10.0.1", ResourceKeys{Failback: true, AutoRebalance: true}},
	} {
		if got := keysOf(tc.version); got != tc.want {
			t.Errorf("PVE %q: keys %+v, want %+v", tc.version, got, tc.want)
		}
	}
	var none *Profile
	if got := none.ResourceKeys(); got != (ResourceKeys{}) {
		t.Errorf("a nil profile: keys %+v, want none", got)
	}
}

// TestResourceKeysTakeTheOldestOnlineNode proves one node on a release
// without auto-rebalance keeps the key out of every write, because every node
// parses resources.cfg and rejects a key its release does not know.
func TestResourceKeysTakeTheOldestOnlineNode(t *testing.T) {
	t.Parallel()
	p := &Profile{Nodes: []NodeFacts{
		{Name: "a", Online: true, Version: keysPVE92},
		{Name: "b", Online: true, Version: keysPVE91},
		{Name: "c", Online: false, Version: "8.4.1"},
	}}
	p.summarizeVersions(&SlowFacts{RulesSupported: true})
	if got := p.ResourceKeys(); got != (ResourceKeys{Failback: true}) {
		t.Fatalf("keys %+v with a node on PVE %s, want failback alone", got, keysPVE91)
	}
}

func TestResourceKeysReadAnUnsetAutoRebalanceOnlyWhereTheKeyExists(t *testing.T) {
	t.Parallel()
	for _, version := range []string{keysPVE90, keysPVE91} {
		k := keysOf(version)
		if k.AutoRebalanceOf(nil) {
			t.Errorf("PVE %s has no auto-rebalance key, so an unset flag must not read as a DLB VM", version)
		}
		if !k.AutoRebalanceOf(pveBool(true)) || k.AutoRebalanceOf(pveBool(false)) {
			t.Errorf("PVE %s: a set flag must read as written", version)
		}
	}
	for _, version := range []string{keysPVE9114, keysPVE92} {
		k := keysOf(version)
		if !k.AutoRebalanceOf(nil) {
			t.Errorf("PVE %s defaults auto-rebalance to on, so an unset flag reads as on", version)
		}
		if k.AutoRebalanceOf(pveBool(false)) {
			t.Errorf("PVE %s: a flag written off must read as off", version)
		}
	}
}

// keyRig is a stageRig that keeps the parameters of every create and update.
type keyRig struct {
	stageRig
	creates []sdkcluster.CreateHaResourcesParams
	updates []sdkcluster.UpdateHaResourcesParams
}

func (r *keyRig) CreateHaResources(ctx context.Context, p *sdkcluster.CreateHaResourcesParams) error {
	r.creates = append(r.creates, *p)
	return r.stageRig.CreateHaResources(ctx, p)
}

func (r *keyRig) UpdateHaResources(ctx context.Context, sid string, p *sdkcluster.UpdateHaResourcesParams) error {
	r.updates = append(r.updates, *p)
	return r.stageRig.UpdateHaResources(ctx, sid, p)
}

func TestStageSendsOnlyTheResourceKeysTheReleaseAccepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version   string
		rebalance bool
	}{
		{keysPVE90, false},
		{keysPVE91, false},
		{keysPVE9114, true},
		{keysPVE92, true},
	} {
		rig := &keyRig{}
		s := &Stager{Resources: rig, Owned: ownedMarker, Keys: keysOf(tc.version)}
		if err := s.Stage(context.Background(), stageDecision(), stageMarker); err != nil {
			t.Fatalf("PVE %s: %v", tc.version, err)
		}
		if len(rig.creates) != 1 {
			t.Fatalf("PVE %s: %d creates, want 1", tc.version, len(rig.creates))
		}
		got := rig.creates[0]
		if got.Failback == nil || *got.Failback {
			t.Errorf("PVE %s: failback %v, want off", tc.version, got.Failback)
		}
		if (got.AutoRebalance != nil) != tc.rebalance {
			t.Errorf("PVE %s: auto-rebalance sent %v, want sent %v", tc.version, got.AutoRebalance != nil,
				tc.rebalance)
		}
		if tc.rebalance && *got.AutoRebalance {
			t.Errorf("PVE %s: auto-rebalance on for a VM outside the DLB", tc.version)
		}
	}
}

// TestStageAdoptsAResourceWithoutAutoRebalanceOnAReleaseWithoutTheKey proves
// the adoption of a staged resource compares only the keys the release has,
// so a resource PVE 9.1 reads back without auto-rebalance needs no update.
func TestStageAdoptsAResourceWithoutAutoRebalanceOnAReleaseWithoutTheKey(t *testing.T) {
	t.Parallel()
	for _, version := range []string{keysPVE90, keysPVE91} {
		rig := &keyRig{stageRig: stageRig{exists: true, state: haStateIgnored,
			comment: WithDLBRecord(stageMarker, false), failback: pveBool(false)}}
		s := &Stager{Resources: rig, Owned: ownedMarker, Keys: keysOf(version)}
		if err := s.Stage(context.Background(), stageDecision(), stageMarker); err != nil {
			t.Fatalf("PVE %s: %v", version, err)
		}
		if !slices.Equal(rig.events, []string{"create:ignored", "get"}) {
			t.Fatalf("PVE %s: events %v, want no update", version, rig.events)
		}
	}

	rig := &keyRig{stageRig: stageRig{exists: true, state: haStateIgnored, comment: stageMarker,
		failback: pveBool(true)}}
	s := &Stager{Resources: rig, Owned: ownedMarker, Keys: keysOf(keysPVE91)}
	if err := s.Stage(context.Background(), stageDecision(), stageMarker); err != nil {
		t.Fatal(err)
	}
	if len(rig.updates) != 1 || rig.updates[0].AutoRebalance != nil || rig.updates[0].Failback == nil ||
		*rig.updates[0].Failback {
		t.Fatalf("updates %+v, want failback off and no auto-rebalance", rig.updates)
	}
}

// TestStageSendsFailbackOffEvenWhenTheReleaseIsUnknown proves a stager whose
// release is unknown still writes failback off, because an omitted key is
// PVE's default of on, and PVE refusing an unknown key fails the call where a
// silent default would leave failback on.
func TestStageSendsFailbackOffEvenWhenTheReleaseIsUnknown(t *testing.T) {
	t.Parallel()
	rig := &keyRig{}
	s := &Stager{Resources: rig, Owned: ownedMarker}
	if err := s.Stage(context.Background(), stageDecision(), stageMarker); err != nil {
		t.Fatal(err)
	}
	if got := rig.creates[0]; got.Failback == nil || *got.Failback || got.AutoRebalance != nil {
		t.Fatalf("create %+v, want failback off and no auto-rebalance", got)
	}

	on := &keyRig{}
	d := stageDecision()
	d.Failback = true
	s = &Stager{Resources: on, Owned: ownedMarker}
	if err := s.Stage(context.Background(), d, stageMarker); err != nil {
		t.Fatal(err)
	}
	if got := on.creates[0]; got.Failback != nil {
		t.Fatalf("create %+v, want failback left to PVE's default of on", got)
	}
}

func TestNarrowRemovalFallbackSendsOnlyTheResourceKeysTheReleaseAccepts(t *testing.T) {
	narrowUseRemoval = true
	t.Cleanup(func() { narrowUseRemoval = false })
	for _, tc := range []struct {
		version   string
		rebalance bool
	}{
		{keysPVE90, false},
		{keysPVE91, false},
		{keysPVE9114, true},
		{keysPVE92, true},
	} {
		r := &barrierRig{state: "started", settleOn: []string{"a"}, snapshot: []Rule{pinAB},
			states: []*pve.HAServiceState{crm("started", 100, 90, 95), crm("started", 100, 90, 95),
				{Known: true, Node: "a"}}}
		in := r.input(&pinAB, &pinA)
		in.Keys = keysOf(tc.version)
		if _, err := NarrowActive(context.Background(), r.deps(), in); err != nil {
			t.Fatalf("PVE %s: %v", tc.version, err)
		}
		if len(r.created) != 1 {
			t.Fatalf("PVE %s: registered %d times, want 1", tc.version, len(r.created))
		}
		got := r.created[0]
		if got.Failback == nil || *got.Failback {
			t.Errorf("PVE %s: failback %v, want off", tc.version, got.Failback)
		}
		if (got.AutoRebalance != nil) != tc.rebalance {
			t.Errorf("PVE %s: auto-rebalance sent %v, want sent %v", tc.version, got.AutoRebalance != nil,
				tc.rebalance)
		}
	}
}

func TestReregisterSnapshotReadsAnUnsetAutoRebalanceAsOffWithoutTheKey(t *testing.T) {
	for _, version := range []string{keysPVE90, keysPVE91} {
		r := newReaddRig()
		r.rebalanceUnset = true
		var got ReaddRecord
		in := r.input()
		in.Stager.Keys = keysOf(version)
		write := in.WriteIntent
		in.WriteIntent = func(ctx context.Context, node, tag string, rec ReaddRecord) error {
			got = rec
			return write(ctx, node, tag, rec)
		}
		res, err := r.run(t, in)
		r.registeredAgain(t, res, err)
		if got.AutoRebalance {
			t.Fatalf("PVE %s has no auto-rebalance key, so the record must not name a DLB VM: %+v", version, got)
		}
	}
}
