// Internal tests for the legacy anti-affinity rule migration under concurrent
// writers, the scorer's view of pre-upgrade group members, the per-request HA
// profile, and HA resource deregistration when ownership cannot be read.
package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// aaLegacyRuleComment is the comment an older CPI gave the group's rule.
const aaLegacyRuleComment = "BOSH anti-affinity for instance group web"

// aaLegacyLock is the sentinel an older CPI locks bosh-aa-web under.
const aaLegacyLock = "bosh-lock-aa-web"

// --------------------------------------------------------------------------
// legacy rule migration
// --------------------------------------------------------------------------

func TestMigrationDeletesOldRuleWhenOneForeignMemberRemains(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101,vm:200", aaLegacyRuleComment)
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(200, legacyTags(aaOtherDirector)),
	)
	events := []string{}
	stub.events = &events
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("hashed rule = %q; want vm:100,vm:101,vm:102", got)
	}
	if got, ok := stub.rules["bosh-aa-web"]; ok {
		t.Errorf("a one-member remainder spreads nothing, so the old rule must be gone; got %q", got)
	}
	if slices.Contains(events, "create-rule:bosh-aa-web") {
		t.Errorf("no one-member rule may be created; events %v", events)
	}
}

func TestMigrationAbortsWhenOldRuleChangesBetweenReads(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101,vm:200,vm:201", aaLegacyRuleComment)
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(200, legacyTags(aaOtherDirector)),
		aaQEMU(201, legacyTags(aaOtherDirector)),
	)
	// An older Director adds vm:202 to the shared rule after this create took
	// its snapshot and before the migration reads the rule again. The reads
	// are the membership plan, its re-plan, the writer's check, the
	// migration's snapshot, and the migration's re-read.
	stub.beforeListRules = func(call int) {
		if call == 5 {
			stub.rules["bosh-aa-web"] += ",vm:202"
		}
	}
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("hashed rule = %q; the membership write must not depend on the migration", got)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101,vm:200,vm:201,vm:202" {
		t.Errorf("old rule = %q; a rule that changed since the snapshot must be left as the other writer left it", got)
	}
	if stub.listRulesCalls != 5 {
		t.Errorf("ListHaRules calls = %d; want the membership write's three, the snapshot, and one re-read",
			stub.listRulesCalls)
	}

	// The next create of the group sees a stable rule and migrates it.
	stub.beforeListRules = nil
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(102, aaTag),
		aaQEMU(200, legacyTags(aaOtherDirector)),
		aaQEMU(201, legacyTags(aaOtherDirector)),
		aaQEMU(202, legacyTags(aaOtherDirector)),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 103, log.NewNopLogger()); err != nil {
		t.Fatalf("retry: unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:200,vm:201,vm:202" {
		t.Errorf("old rule after retry = %q; want only the other Director's members", got)
	}
}

func TestMigrationHoldsTheOlderCPIsLockKey(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101,vm:200,vm:201", aaLegacyRuleComment)
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(200, legacyTags(aaOtherDirector)),
		aaQEMU(201, legacyTags(aaOtherDirector)),
	)
	pools := newAALockPools(&events)
	deps := aaDepsLock(aaLockConfig("pool", false, 30), stub, pools)
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:200,vm:201" {
		t.Fatalf("old rule = %q; want vm:200,vm:201", got)
	}
	idx := func(ev string) int { return slices.Index(events, ev) }
	acquire, reread := idx("lock-create:"+aaLegacyLock), -1
	for i := acquire + 1; acquire >= 0 && i < len(events); i++ {
		if events[i] == "list-rules" {
			reread = i
			break
		}
	}
	update, release := idx("update-rule:bosh-aa-web"), idx("lock-delete:"+aaLegacyLock)
	if acquire < 0 || reread < 0 || update < 0 || release < 0 {
		t.Fatalf("the migration must take %s, re-read, rewrite, and release; events %v", aaLegacyLock, events)
	}
	if acquire >= reread || reread >= update || update >= release {
		t.Errorf("want acquire < re-read < update < release; events %v", events)
	}
	if slices.Contains(events, "delete-rule:bosh-aa-web") || slices.Contains(events, "create-rule:bosh-aa-web") {
		t.Errorf("the old rule must change in place, never by delete and create; events %v", events)
	}
	if hashedRelease := idx("lock-delete:" + aaLock); hashedRelease < release {
		t.Errorf("the hashed lock must be held for the whole migration; events %v", events)
	}
	if len(pools.pools) != 0 {
		t.Errorf("every lock must be released, still held: %v", pools.pools)
	}
}

func TestMigrationDefersWhenTheOlderCPIsLockIsUnavailable(t *testing.T) {
	events := []string{}
	stub := newAAStub()
	stub.events = &events
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101,vm:200,vm:201", aaLegacyRuleComment)
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(200, legacyTags(aaOtherDirector)),
		aaQEMU(201, legacyTags(aaOtherDirector)),
	)
	pools := newAALockPools(&events)
	pools.createErr = func(id string) error {
		if id == aaLegacyLock {
			return fmt.Errorf("pmxcfs unavailable")
		}
		return nil
	}
	deps := aaDepsLock(aaLockConfig("pool", false, 30), stub, pools)
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("a deferred migration must not fail the create: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("hashed rule = %q; want vm:100,vm:101,vm:102", got)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101,vm:200,vm:201" {
		t.Errorf("old rule = %q; it must be untouched without the older CPI's lock", got)
	}
}

// --------------------------------------------------------------------------
// scorer group membership
// --------------------------------------------------------------------------

func TestAntiAffinityScorerGroupCarriesHashedTagAndLegacyProof(t *testing.T) {
	t.Parallel()
	on := true
	cfg := icMinConfig()
	cfg.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}}
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector}

	got := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	if got.tag != aaTag {
		t.Errorf("tag = %q; want the hashed member tag %q", got.tag, aaTag)
	}
	wantLegacy := []string{"job--web", "deployment--" + aaDeployment, ownerTagFor(aaDirector)}
	if !slices.Equal(got.legacy, wantLegacy) {
		t.Errorf("legacy = %v; want the migration's proof %v", got.legacy, wantLegacy)
	}
	if !got.enabled() {
		t.Error("the scorer must spread")
	}
	if !inScorerGroup(got, legacyTags(aaDirector)) {
		t.Error("a pre-upgrade member of this Director must count")
	}
	if !inScorerGroup(got, aaTag) {
		t.Error("a member with the hashed tag must count")
	}
	if inScorerGroup(got, legacyTags(aaOtherDirector)) {
		t.Error("another Director's guest with the same group name must not count")
	}
}

func TestAntiAffinityScorerGroupWithoutDirectorUUIDStillSpreads(t *testing.T) {
	t.Parallel()
	on := true
	cfg := icMinConfig()
	cfg.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &on}}
	deps := Deps{Config: cfg}

	got := antiAffinityScorerGroup(deps, aaEnvForWebGroup())
	if got.tag != "" {
		t.Errorf("tag = %q; no hashed tag exists without a director_uuid", got.tag)
	}
	if !got.enabled() {
		t.Fatal("spreading must stay on without a director_uuid")
	}
	if want := []string{"job--web", "deployment--" + aaDeployment}; !slices.Equal(got.legacy, want) {
		t.Errorf("legacy = %v; want %v", got.legacy, want)
	}
	if !inScorerGroup(got, "deployment--"+aaDeployment+";job--web") {
		t.Error("a member with job-- and deployment-- must count")
	}

	cfg.Placement.AntiAffinity.Enabled = new(bool)
	if g := antiAffinityScorerGroup(deps, aaEnvForWebGroup()); g.enabled() {
		t.Errorf("anti-affinity off must not spread, got %+v", g)
	}
}

// inScorerGroup applies the scorer's membership rule to one guest's tags.
func inScorerGroup(g scorerGroup, tags string) bool {
	if g.tag != "" && tagsContain(tags, g.tag) {
		return true
	}
	return len(g.legacy) > 0 && tagsContainAll(tags, g.legacy)
}

// --------------------------------------------------------------------------
// per-request HA profile through ha.Resolve
// --------------------------------------------------------------------------

// haProbeCluster answers the cluster reads ha.Resolve makes on top of the
// anti-affinity fixture. The cluster is one quorate node, pve1, and the reads
// the profile can do without are refused, which only records gaps.
type haProbeCluster struct {
	*aaClusterStub
}

func (c *haProbeCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	resp := cluster.ListStatusResponse{
		[]byte(`{"type":"cluster","name":"lab","quorate":1,"nodes":2}`),
		[]byte(`{"type":"node","name":"pve1","online":1,"local":1}`),
		[]byte(`{"type":"node","name":"pve2","online":1}`),
	}
	return &resp, nil
}

func (c *haProbeCluster) ListHaStatusManagerStatus(context.Context) (*cluster.ListHaStatusManagerStatusResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (c *haProbeCluster) ListHaStatusCurrent(context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (c *haProbeCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (c *haProbeCluster) ListConfigQdevice(context.Context) (*cluster.ListConfigQdeviceResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (c *haProbeCluster) ListOptions(context.Context) (*cluster.ListOptionsResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (c *haProbeCluster) ListReplication(context.Context) (*cluster.ListReplicationResponse, error) {
	return nil, fmt.Errorf("not wired")
}

// haProbeNodes answers the per-node reads of the profile and counts the
// version reads, one per probe of the online node.
type haProbeNodes struct {
	nodes.Service
	version      string
	versionCalls int
}

func (n *haProbeNodes) ListVersion(context.Context, string) (*nodes.ListVersionResponse, error) {
	n.versionCalls++
	return &nodes.ListVersionResponse{Release: strings.SplitN(n.version, ".", 2)[0], Version: n.version}, nil
}

func (n *haProbeNodes) ListStatus(context.Context, string) (*nodes.ListStatusResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (n *haProbeNodes) ListTime(context.Context, string) (*nodes.ListTimeResponse, error) {
	return nil, fmt.Errorf("not wired")
}

func (n *haProbeNodes) ListQemu(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	return &nodes.ListQemuResponse{}, nil
}

// haProbeStorage refuses the storage listing, which the profile records as a gap.
type haProbeStorage struct {
	clusterstorage.Service
}

func (haProbeStorage) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	return nil, fmt.Errorf("not wired")
}

// haProbeClient adds the cluster storage service ha.Resolve needs.
type haProbeClient struct {
	*icPVEClient
}

func (haProbeClient) ClusterStorage() clusterstorage.Service { return haProbeStorage{} }

// haProbeDeps builds Deps with no HAProfile, so the rule writers reach ha.Resolve.
func haProbeDeps(stub *aaClusterStub, nodesSvc *haProbeNodes) Deps {
	return Deps{
		Config:              aaMembershipConfig("off", false, 0),
		RequestDirectorUUID: aaDirector,
		PVE: haProbeClient{icPVEClient: &icPVEClient{
			clusterSvc: &haProbeCluster{aaClusterStub: stub},
			nodesSvc: &icNodesService{Service: nodesSvc, listFn: func(ctx context.Context, p *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return stub.ListResources(ctx, p)
			}},
		}},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

func TestHAProfileResolvedOnceThroughTheProbeForBothWriters(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	nodesSvc := &haProbeNodes{version: "9.2.4"}
	deps := withRequestHAProfile(haProbeDeps(stub, nodesSvc))

	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("anti-affinity: unexpected error: %v", err)
	}
	if err := ensureNodeAffinityPin(context.Background(), deps, 101, []string{"pve1"}, false, log.NewNopLogger()); err != nil {
		t.Fatalf("node-affinity: unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Errorf("anti-affinity rule = %q; want vm:100,vm:101", got)
	}
	if got := stub.rules[naRuleNameFor(101)]; got != "vm:101" {
		t.Errorf("node-affinity rule = %q; want vm:101", got)
	}
	// One version read per online node, which is one probe for the request.
	if nodesSvc.versionCalls != 2 {
		t.Errorf("the profile read node versions %d times; want one probe of both nodes for the whole request",
			nodesSvc.versionCalls)
	}
}

func TestHAProfileProbeRefusesBelowPVE9OnceForBothWriters(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	nodesSvc := &haProbeNodes{version: "8.4.1"}
	deps := withRequestHAProfile(haProbeDeps(stub, nodesSvc))

	aaErr := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101, log.NewNopLogger())
	naErr := ensureNodeAffinityPin(context.Background(), deps, 101, []string{"pve1"}, false, log.NewNopLogger())
	for name, err := range map[string]error{"anti-affinity": aaErr, "node-affinity": naErr} {
		if err == nil || !strings.Contains(err.Error(), "PVE 9") {
			t.Errorf("%s: err = %v; want the PVE 9 refusal", name, err)
		}
	}
	if len(stub.rules) != 0 || len(stub.resources) != 0 {
		t.Errorf("nothing may be written below PVE 9: rules %v resources %v", stub.rules, stub.resources)
	}
	if nodesSvc.versionCalls != 2 {
		t.Errorf("the profile read node versions %d times; want one probe of both nodes", nodesSvc.versionCalls)
	}
}

// --------------------------------------------------------------------------
// deregistration
// --------------------------------------------------------------------------

func TestDeregisterSkipsPurgeWhenOwnershipReadFails(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.resources["vm:101"] = true
	stub.getResErr = fmt.Errorf("connection reset by peer")
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stub.resources["vm:101"] {
		t.Error("a resource whose ownership could not be read must not be purged")
	}
}

func TestDeregisterPurgesWhenOwnershipReadSaysNotFound(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.getResErr = fmt.Errorf("no such resource 'vm:101'")
	stub.resources["vm:101"] = true
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.resources["vm:101"] {
		t.Error("a not-found read must still send the not-found-tolerant purge")
	}
}
