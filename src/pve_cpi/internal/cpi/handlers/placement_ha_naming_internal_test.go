package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaDirector is the Director UUID the anti-affinity fixtures run as.
const aaDirector = "dir-1"

// aaDeployment is the deployment the anti-affinity fixtures run in.
const aaDeployment = "d"

// aaOtherDirector is a second Director that shares the fixture cluster.
const aaOtherDirector = "dir-2"

// aaIdent is the identity of a group in the fixture Director and deployment.
func aaIdent(group string) haIdentity {
	return haIdentity{Owner: aaDirector, Deployment: aaDeployment, Group: group}
}

// aaRule and aaTag are the hashed rule name and member tag of the "web" group.
var (
	aaRule = aaIdent("web").ruleName()
	aaTag  = aaIdent("web").ruleName()
	// aaLock is the name of the group's cluster lock sentinel.
	aaLock = "bosh-lock-aa-" + aaIdent("web").hash()
)

// pve9Profile is an HA profile on a cluster whose nodes all run PVE 9.2.
func pve9Profile(context.Context) (*ha.Profile, error) {
	return &ha.Profile{
		HAVersionOK: true,
		Nodes:       []ha.NodeFacts{{Name: "pve1", Online: true, Version: "9.2.4"}, {Name: "pve2", Online: true, Version: "9.2.1"}},
	}, nil
}

func TestHAIdentityRuleNameIsHashed(t *testing.T) {
	t.Parallel()
	name := aaIdent("web").ruleName()
	if !strings.HasPrefix(name, "bosh-aa-") || len(name) != len("bosh-aa-")+12 {
		t.Fatalf("rule name = %q; want bosh-aa- plus 12 hex characters", name)
	}
	if name == "bosh-aa-web" {
		t.Fatal("the rule name must not be the bare group name")
	}
	if aaIdent("web").ruleName() != name {
		t.Fatal("the rule name must be deterministic")
	}
}

func TestHAIdentityHashSeparatesDirectorsDeploymentsAndGroups(t *testing.T) {
	t.Parallel()
	base := aaIdent("web")
	seen := map[string]string{base.ruleName(): "base"}
	for label, other := range map[string]haIdentity{
		"director":   {Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"},
		"deployment": {Owner: aaDirector, Deployment: "other", Group: "web"},
		"group":      {Owner: aaDirector, Deployment: aaDeployment, Group: "api"},
		"shifted":    {Owner: aaDirector + aaDeployment[:1], Deployment: aaDeployment[1:], Group: "web"},
	} {
		if prev, dup := seen[other.ruleName()]; dup {
			t.Errorf("%s collides with %s: %s", label, prev, other.ruleName())
		}
		seen[other.ruleName()] = label
	}
}

func TestHAIdentityMarkerRoundTrips(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: "d-1", Deployment: "my dep", Group: "web"}
	m, ok := parseHaMarker(ident.marker())
	if !ok {
		t.Fatalf("marker %q did not parse", ident.marker())
	}
	if m.Owner != "d-1" || m.Deployment != "my_dep" || m.Group != "web" {
		t.Errorf("parsed marker = %+v", m)
	}
	if got := (haIdentity{Owner: "d-1"}).marker(); got != "bosh-cpi:v1 owner=d-1 deployment=- group=-" {
		t.Errorf("marker with unset fields = %q", got)
	}
	if _, ok := parseHaMarker("BOSH anti-affinity for instance group web"); ok {
		t.Error("an old CPI comment must not parse as a marker")
	}
}

func TestHAOwnershipOf(t *testing.T) {
	t.Parallel()
	mine := aaIdent("web").marker()
	theirs := haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}.marker()
	cases := []struct {
		name, comment, owner string
		want                 haOwnership
	}{
		{"ours", mine, aaDirector, haOwnedByUs},
		{"other Director", theirs, aaDirector, haOwnedByOther},
		{"unmarked", "BOSH anti-affinity for instance group web", aaDirector, haUnmarked},
		{"empty comment", "", aaDirector, haUnmarked},
		{"no Director UUID owns nothing", mine, "", haOwnedByOther},
	}
	for _, tc := range cases {
		if got := haOwnershipOf(tc.comment, tc.owner); got != tc.want {
			t.Errorf("%s: ownership = %d; want %d", tc.name, got, tc.want)
		}
	}
}

func TestHARuleWriteGate(t *testing.T) {
	t.Parallel()
	mixed := func(context.Context) (*ha.Profile, error) {
		return &ha.Profile{HAVersionOK: true, Nodes: []ha.NodeFacts{
			{Name: "pve1", Online: true, Version: "9.2.4"},
			{Name: "pve2", Online: false, Version: "8.4.1"},
		}}, nil
	}
	unconfirmed := func(context.Context) (*ha.Profile, error) {
		return &ha.Profile{Nodes: []ha.NodeFacts{{Name: "pve1", Online: true, Version: "9.2.4"}}}, nil
	}
	cases := []struct {
		name    string
		profile func(context.Context) (*ha.Profile, error)
		wantErr string
	}{
		{"all nodes on PVE 9", pve9Profile, ""},
		{"one node below PVE 9", mixed, "pve2 (8.4.1)"},
		{"floor not confirmed", unconfirmed, "does not confirm"},
	}
	for _, tc := range cases {
		deps := Deps{HAProfile: tc.profile}
		err := haRuleWriteGate(context.Background(), deps)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v; want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestEnsureAntiAffinityRefusesBelowPVE9(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	deps := aaDeps(stub)
	deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		return &ha.Profile{HAVersionOK: true, Nodes: []ha.NodeFacts{{Name: "pve1", Online: true, Version: "8.4.1"}}}, nil
	}
	if err := ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), 101, log.NewNopLogger()); err == nil {
		t.Fatal("a cluster with a node below PVE 9 must refuse the rule write")
	}
	if len(stub.rules) != 0 || len(stub.resources) != 0 || stub.createRuleCalls != 0 {
		t.Errorf("nothing may be written: rules=%v resources=%v", stub.rules, stub.resources)
	}
}

func TestEnsureAntiAffinityWritesHashedRuleWithMarkers(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Fatalf("rule %s = %q", aaRule, got)
	}
	if _, bare := stub.rules["bosh-aa-web"]; bare {
		t.Error("the bare group name must no longer be a rule name")
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
		t.Errorf("rule comment = %q; want the ownership marker", got)
	}
	if got := stub.resComments["vm:101"]; got != aaIdent("web").marker() {
		t.Errorf("resource comment = %q; want the ownership marker", got)
	}
}

func TestEnsureAntiAffinityTwoDirectorsOneClusterKeepSeparateRules(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	other := haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag), aaQEMU(200, other.ruleName()))
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("director one: %v", err)
	}
	otherDeps := aaDeps(stub)
	otherDeps.RequestDirectorUUID = aaOtherDirector
	if err := ensureAntiAffinityMembership(context.Background(), otherDeps, other, 201, log.NewNopLogger()); err != nil {
		t.Fatalf("director two: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101" {
		t.Errorf("director one rule = %q", got)
	}
	if got := stub.rules[other.ruleName()]; got != "vm:200,vm:201" {
		t.Errorf("director two rule = %q", got)
	}
}

func TestEnsureAntiAffinityLeavesForeignRuleUnderOurName(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, aaTag))
	stub.seedRuleWithComment(aaRule, "vm:100,vm:300",
		haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}.marker())
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 101, log.NewNopLogger()); err == nil {
		t.Fatal("a rule that carries another Director's marker must not be rewritten")
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:300" {
		t.Errorf("foreign rule changed to %q", got)
	}
}

// legacyTags are the tags an older CPI's set_vm_metadata left on a guest of the
// fixture group, plus the owner tag.
func legacyTags(owner string) string {
	return "deployment--" + aaDeployment + ";job--web;" + ownerTagFor(owner)
}

func TestEnsureAntiAffinityMigratesOnlyOwnMembersOutOfOldRule(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	// An old CPI's rule holds two members from this Director and two from
	// another Director that also has a group named web.
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101,vm:200,vm:201", "BOSH anti-affinity for instance group web")
	stub.listResourcesFn = aaResourcesFn(
		aaQEMU(100, legacyTags(aaDirector)),
		aaQEMU(101, legacyTags(aaDirector)),
		aaQEMU(200, legacyTags(aaOtherDirector)),
		aaQEMU(201, legacyTags(aaOtherDirector)),
	)
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("hashed rule = %q; want vm:100,vm:101,vm:102", got)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:200,vm:201" {
		t.Errorf("old rule = %q; want only the other Director's members, vm:200,vm:201", got)
	}
	if got := stub.ruleComments["bosh-aa-web"]; got != "BOSH anti-affinity for instance group web" {
		t.Errorf("old rule comment = %q; it must stay unmarked", got)
	}
}

func TestEnsureAntiAffinityDeletesOldRuleOnlyWhenEmpty(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101", "BOSH anti-affinity for instance group web")
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, legacyTags(aaDirector)), aaQEMU(101, legacyTags(aaDirector)))
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := stub.rules["bosh-aa-web"]; ok {
		t.Error("the old rule must be deleted once every member has moved")
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Errorf("hashed rule = %q", got)
	}
}

func TestEnsureAntiAffinityDoesNotClaimGuestsWithoutOwnerEvidence(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no owner tag":        "deployment--" + aaDeployment + ";job--web",
		"another Director":    legacyTags(aaOtherDirector),
		"another deployment":  "deployment--other;job--web;" + ownerTagFor(aaDirector),
		"another group":       "deployment--" + aaDeployment + ";job--api;" + ownerTagFor(aaDirector),
		"deployment is empty": "job--web;" + ownerTagFor(aaDirector),
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101", "BOSH anti-affinity for instance group web")
			stub.listResourcesFn = aaResourcesFn(aaQEMU(100, tags), aaQEMU(101, tags))
			if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101" {
				t.Errorf("old rule = %q; it must be untouched", got)
			}
			if _, ok := stub.rules[aaRule]; ok {
				t.Errorf("no hashed rule is due with a single member, got %q", stub.rules[aaRule])
			}
		})
	}
}

func TestEnsureAntiAffinityLeavesMarkedLegacyNamedRuleAlone(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.seedRuleWithComment("bosh-aa-web", "vm:100,vm:101",
		haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}.marker())
	stub.listResourcesFn = aaResourcesFn(aaQEMU(100, legacyTags(aaDirector)), aaQEMU(101, legacyTags(aaDirector)))
	if err := ensureAntiAffinityMembership(context.Background(), aaDeps(stub), aaIdent("web"), 102, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-aa-web"]; got != "vm:100,vm:101" {
		t.Errorf("a marked rule must never be migrated; got %q", got)
	}
}

func TestApplyAntiAffinityMembershipSkipsWithoutDirectorUUID(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	deps := aaDeps(stub)
	deps.Config = aaMembershipConfig("off", false, 1)
	deps.RequestDirectorUUID = ""
	if err := applyAntiAffinityMembership(context.Background(), deps, 101, aaEnvForWebGroup(), log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stub.rules) != 0 || len(stub.resources) != 0 {
		t.Errorf("no HA object may be written without a director_uuid: %v %v", stub.rules, stub.resources)
	}
}

func TestRemoveAntiAffinityOnlyTouchesOwnAndUnmarkedRules(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.resources["vm:101"] = true
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")
	foreign := haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "api"}
	stub.seedRuleWithComment(foreign.ruleName(), "vm:101,vm:300", foreign.marker())
	stub.seedRuleWithComment("bosh-aa-db", "vm:101,vm:400,vm:401", "BOSH anti-affinity for instance group db")
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:102" {
		t.Errorf("own rule = %q; want vm:100,vm:102", got)
	}
	if got := stub.ruleComments[aaRule]; got != aaIdent("web").marker() {
		t.Errorf("own rule lost its marker: %q", got)
	}
	if got := stub.rules[foreign.ruleName()]; got != "vm:101,vm:300" {
		t.Errorf("another Director's rule changed to %q", got)
	}
	if got := stub.rules["bosh-aa-db"]; got != "vm:400,vm:401" {
		t.Errorf("unmarked rule = %q; want the VM removed", got)
	}
}

func TestRemoveAntiAffinityKeepsResourceMarkedForAnotherDirector(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.resources["vm:101"] = true
	stub.resComments["vm:101"] = haIdentity{Owner: aaOtherDirector}.marker()
	if err := removeAntiAffinityMembership(context.Background(), aaDeps(stub), 101, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stub.resources["vm:101"] {
		t.Error("a resource marked for another Director must not be deregistered")
	}
}

func TestBuildVMShapeStampsTheHashedMemberTag(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := &config.CPIConfig{Node: "pve", VMStorage: "local-lvm", VMIDRangeStart: 100,
		Placement: &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: &enabled}}}
	deps := Deps{Config: cfg, PVE: &shapeTestPVEClient{}, Logger: log.NewNopLogger(), RequestDirectorUUID: aaDirector}
	parsed := minimalParsedArgs("test-storage")
	parsed.env = aaEnvForWebGroup()

	shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve1")
	if err != nil {
		t.Fatalf("buildVMShapeForNode: %v", err)
	}
	if !tagsContain(shape.initialTags, aaTag) {
		t.Errorf("initial tags %q lack the member tag %s", shape.initialTags, aaTag)
	}
	if tagsContain(shape.initialTags, "job--web") {
		t.Errorf("initial tags %q must not carry the old member tag", shape.initialTags)
	}

	deps.RequestDirectorUUID = ""
	shape, err = buildVMShapeForNode(context.Background(), deps, parsed, "pve1")
	if err != nil {
		t.Fatalf("buildVMShapeForNode without a director: %v", err)
	}
	if strings.Contains(shape.initialTags, haRuleNamePrefix) {
		t.Errorf("initial tags %q carry a member tag without a director_uuid", shape.initialTags)
	}
}

func TestOperatorTagCannotClaimTheMemberTagPrefix(t *testing.T) {
	t.Parallel()
	if !hasCPIOwnedPrefix(aaTag) {
		t.Errorf("%s must count as a CPI-owned tag so an operator tag cannot forge it", aaTag)
	}
}

func TestEnsureNodeAffinityPinWritesMarkers(t *testing.T) {
	t.Parallel()
	stub := newNAStub()
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100, []string{"pve01", "pve02"}, true,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := haIdentity{Owner: aaDirector}.marker()
	rule := stub.rules["bosh-na-100"]
	if rule.Comment == nil || *rule.Comment != want {
		t.Errorf("rule comment = %v; want %q", rule.Comment, want)
	}
	if got := stub.resComments["vm:100"]; got != want {
		t.Errorf("resource comment = %q; want %q", got, want)
	}
}

func TestEnsureNodeAffinityPinAddsMarkerToUnmarkedRule(t *testing.T) {
	t.Parallel()
	stub := newNAStub()
	old := "BOSH AZ node-affinity pin for vm:100"
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &old}
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100, []string{"pve01"}, true,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stub.rules["bosh-na-100"].Comment; got == nil || *got != (haIdentity{Owner: aaDirector}).marker() {
		t.Errorf("rule comment = %v; want the marker", got)
	}
}

func TestEnsureNodeAffinityPinLeavesAnotherDirectorsRule(t *testing.T) {
	t.Parallel()
	stub := newNAStub()
	theirs := haIdentity{Owner: aaOtherDirector}.marker()
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &theirs}
	if err := ensureNodeAffinityPin(context.Background(), naDeps(stub), 100, []string{"pve01"}, true,
		log.NewNopLogger()); err == nil {
		t.Fatal("a pin that carries another Director's marker must not be rewritten")
	}
	if stub.deleteCalls != 0 || stub.createCalls != 0 {
		t.Errorf("nothing may change: deletes=%d creates=%d", stub.deleteCalls, stub.createCalls)
	}
}

func TestEnsureNodeAffinityPinRefusesBelowPVE9AndWithoutDirector(t *testing.T) {
	t.Parallel()
	stub := newNAStub()
	deps := naDeps(stub)
	deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		return &ha.Profile{HAVersionOK: true, Nodes: []ha.NodeFacts{{Name: "pve1", Online: true, Version: "8.4.1"}}}, nil
	}
	if err := ensureNodeAffinityPin(context.Background(), deps, 100, []string{"pve01"}, true, log.NewNopLogger()); err == nil {
		t.Error("a node below PVE 9 must refuse the pin")
	}
	deps = naDeps(stub)
	deps.RequestDirectorUUID = ""
	if err := ensureNodeAffinityPin(context.Background(), deps, 100, []string{"pve01"}, true, log.NewNopLogger()); err == nil {
		t.Error("a request without a director_uuid must refuse the pin")
	}
	if len(stub.rules) != 0 || len(stub.resources) != 0 {
		t.Errorf("nothing may be written: rules=%v resources=%v", stub.rules, stub.resources)
	}
}

func TestRemoveNodeAffinityPinOnlyDeletesOwnOrUnmarkedRule(t *testing.T) {
	t.Parallel()
	theirs := haIdentity{Owner: aaOtherDirector}.marker()
	mine := haIdentity{Owner: aaDirector}.marker()
	cases := []struct {
		name    string
		comment *string
		deleted bool
	}{
		{"another Director's", &theirs, false},
		{"ours", &mine, true},
		{"unmarked", nil, true},
	}
	for _, tc := range cases {
		stub := newNAStub()
		stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType, Comment: tc.comment}
		if err := removeNodeAffinityPin(context.Background(), naDeps(stub), 100, log.NewNopLogger()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, kept := stub.rules["bosh-na-100"]; kept == tc.deleted {
			t.Errorf("%s: rule kept = %v; want deleted = %v", tc.name, kept, tc.deleted)
		}
	}
}

func TestNoResourceAffinityRuleSendsStrict(t *testing.T) {
	t.Parallel()
	var sent *cluster.CreateHaRulesParams
	svc := &captureRuleService{capture: &sent}
	if err := createNegativeRule(context.Background(), svc, "bosh-aa-x", "vm:1,vm:2", "bosh-cpi:v1 owner=o"); err != nil {
		t.Fatal(err)
	}
	if sent == nil || sent.Strict != nil {
		t.Fatalf("a resource-affinity rule must not carry strict; sent %+v", sent)
	}
	if sent.Type != "resource-affinity" || sent.Affinity == nil || *sent.Affinity != "negative" {
		t.Errorf("unexpected rule %+v", sent)
	}
}

// captureRuleService records the CreateHaRules params it receives.
type captureRuleService struct {
	cluster.Service
	capture **cluster.CreateHaRulesParams
}

func (s *captureRuleService) CreateHaRules(_ context.Context, p *cluster.CreateHaRulesParams) error {
	*s.capture = p
	return nil
}
