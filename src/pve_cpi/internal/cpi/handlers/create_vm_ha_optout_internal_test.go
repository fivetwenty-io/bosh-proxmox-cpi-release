package handlers

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haOptOutConfig returns a config in the HA mode given, with a vm_type
// profile "solo" that sets ha to false.
func haOptOutConfig(mode string) *config.CPIConfig {
	cfg := isoHABaseConfig("local")
	cfg.HA = haAntiAffinityOn()
	if mode == config.HAModeOff {
		cfg.HA = haModeOff()
	} else {
		cfg.HA.Mode = haChoiceOf(mode)
	}
	cfg.VMTypes = map[string]config.TypeProfile{"solo": {CloudProperties: map[string]any{"ha": false}}}
	return cfg
}

// TestResolveCreateVMHAOptOut_ParsesTheProperty pins that only ha false opts
// the VM out, that unset and true change nothing, that any other value fails
// the create as a config error, and that mode off validates the property but
// takes no VM out of HA, because no VM is in HA there.
func TestResolveCreateVMHAOptOut_ParsesTheProperty(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mode       string
		cloudProps map[string]any
		want       bool
		wantErr    bool
	}{
		"unset":                   {mode: config.HAModeAuto, cloudProps: map[string]any{}},
		"true":                    {mode: config.HAModeAuto, cloudProps: map[string]any{"ha": true}},
		"false":                   {mode: config.HAModeAuto, cloudProps: map[string]any{"ha": false}, want: true},
		"false under managed":     {mode: config.HAModeManaged, cloudProps: map[string]any{"ha": false}, want: true},
		"vm_type false":           {mode: config.HAModeAuto, cloudProps: map[string]any{"vm_type": "solo"}, want: true},
		"call true beats vm_type": {mode: config.HAModeAuto, cloudProps: map[string]any{"vm_type": "solo", "ha": true}},
		"string":                  {mode: config.HAModeAuto, cloudProps: map[string]any{"ha": "false"}, wantErr: true},
		"number":                  {mode: config.HAModeAuto, cloudProps: map[string]any{"ha": 0.0}, wantErr: true},
		"map":                     {mode: config.HAModeAuto, cloudProps: map[string]any{"ha": map[string]any{}}, wantErr: true},
		"false under off":         {mode: config.HAModeOff, cloudProps: map[string]any{"ha": false}},
		"string under off":        {mode: config.HAModeOff, cloudProps: map[string]any{"ha": "no"}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parsed := &createVMParsedArgs{cloudPropsMap: tc.cloudProps}
			err := resolveCreateVMHAOptOut(parsed, haOptOutConfig(tc.mode))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "cloud property ha") {
					t.Fatalf("err = %v; want a config error that names the ha cloud property", err)
				}
				if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
					t.Fatalf("err = %v; a manifest error is not retriable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed.cloudProps.haOff != tc.want {
				t.Fatalf("haOff = %v; want %v", parsed.cloudProps.haOff, tc.want)
			}
		})
	}
}

// TestDecideCreatedVMHA_OptedOutVMStagesNothing pins that a VM with ha false
// gets no HA resource and no rule in auto and managed mode, that managed mode
// does not fail it even when the request carries no director_uuid, and that
// the create logs one Info line that names the VM.
func TestDecideCreatedVMHA_OptedOutVMStagesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mode, owner string
	}{
		"auto":             {mode: config.HAModeAuto, owner: aaDirector},
		"managed":          {mode: config.HAModeManaged, owner: aaDirector},
		"managed no owner": {mode: config.HAModeManaged},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps, rules, parsed := decideRig(tc.mode, tc.owner, nil)
			parsed.cloudProps.haOff = true
			var buf bytes.Buffer
			h, err := decideCreatedVMHA(context.Background(), deps, infoLogger(t, &buf), parsed,
				&createVMShape{node: "pve01"}, 100)
			if h != nil || err != nil {
				t.Fatalf("h %v err %v; want no HA and no error", h, err)
			}
			if len(rules.resources) != 0 || len(rules.rules) != 0 {
				t.Fatalf("resources %v rules %v; want nothing written", rules.resources, rules.rules)
			}
			out := buf.String()
			if strings.Count(out, "the ha cloud property keeps the VM out of PVE HA") != 1 ||
				!strings.Contains(out, "100") || !strings.Contains(out, "INFO") {
				t.Fatalf("log %q; want one Info line that names VM 100 and the ha cloud property", out)
			}
		})
	}
}

// TestDecideCreatedVMHA_ModeOffIgnoresTheOptOut pins that mode off logs
// nothing about the opt-out, because no VM is in HA there.
func TestDecideCreatedVMHA_ModeOffIgnoresTheOptOut(t *testing.T) {
	t.Parallel()
	cfg := icMinConfig()
	cfg.HA = haModeOff()
	deps := Deps{Config: cfg, Logger: log.NewNopLogger()}
	parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{haOff: true}}
	var buf bytes.Buffer
	h, err := decideCreatedVMHA(context.Background(), deps, infoLogger(t, &buf), parsed,
		&createVMShape{node: "pve01"}, 100)
	if h != nil || err != nil || buf.Len() != 0 {
		t.Fatalf("h %v err %v log %q; want nothing under mode off", h, err, buf.String())
	}
}

// TestRefuseManagedHABeforeCreate_OptedOutVMIsNotRefused pins that managed
// mode does not refuse a VM that opted out of HA, even when the request
// carries no director_uuid and the decision would refuse any other VM.
func TestRefuseManagedHABeforeCreate_OptedOutVMIsNotRefused(t *testing.T) {
	t.Parallel()
	deps, _, parsed := decideRig(config.HAModeManaged, "", nil)
	if err := refuseManagedHABeforeCreate(context.Background(), deps, parsed); err == nil {
		t.Fatal("the rig must refuse a VM that keeps HA, or the test proves nothing")
	}
	parsed.cloudProps.haOff = true
	if err := refuseManagedHABeforeCreate(context.Background(), deps, parsed); err != nil {
		t.Fatalf("err = %v; an opted-out VM is not a managed-mode failure", err)
	}
}

// TestHARegistrationFeatures_OptedOutVMHasNone pins that an opted-out VM asks
// for no HA feature, so storage placement, the ISO check, and the resurrector
// line treat it as a VM without HA.
func TestHARegistrationFeatures_OptedOutVMHasNone(t *testing.T) {
	t.Parallel()
	deps := aaOffHADeps(t, "")
	parsed := aaOffParsed(t, deps, map[string]any{})
	if len(haRegistrationFeatures(deps, parsed.cloudProps, "pve01", parsed.env)) == 0 {
		t.Fatal("the rig must enable a registration feature, or the test proves nothing")
	}
	parsed.cloudProps.haOff = true
	if got := haRegistrationFeatures(deps, parsed.cloudProps, "pve01", parsed.env); len(got) != 0 {
		t.Fatalf("features = %v; want none for an opted-out VM", got)
	}
}

// TestBuildHeadroomPlan_OptedOutVMNeedsNoRoom pins that the headroom check
// skips an opted-out VM, because PVE HA never restarts it elsewhere.
func TestBuildHeadroomPlan_OptedOutVMNeedsNoRoom(t *testing.T) {
	t.Parallel()
	deps, _, parsed := decideRig(config.HAModeAuto, aaDirector, nil)
	parsed.cloudProps.haOff = true
	if p := buildHeadroomPlan(context.Background(), deps, parsed.cloudProps, nil, nil, nil, bridgePlan{}, nil,
		log.NewNopLogger()); p != nil {
		t.Fatalf("plan = %+v; want no headroom plan for an opted-out VM", p)
	}
}

// TestAntiAffinityPartnersForCreate_OptedOutVMHasNoPartners pins that
// placement charges an opted-out VM for no rule partner, because the VM joins
// no anti-affinity rule.
func TestAntiAffinityPartnersForCreate_OptedOutVMHasNoPartners(t *testing.T) {
	t.Parallel()
	deps, _, fleet := aaPartnerFleet(t)
	aaDeleteMember(t, deps, fleet, 101)
	ctx, env := context.Background(), aaEnvForWebGroup()
	if got := antiAffinityPartnersForCreate(ctx, deps, env, createVMCloudProps{}, "block"); len(got) == 0 {
		t.Fatal("the rig must give the recreate partners, or the test proves nothing")
	}
	if got := antiAffinityPartnersForCreate(ctx, deps, env, createVMCloudProps{haOff: true}, "block"); got != nil {
		t.Fatalf("partners = %v; want none for an opted-out VM", got)
	}
}

// TestJoinAntiAffinityAtMetadata_OptedOutVMJoinsNoRule pins that the
// set_vm_metadata join after the create writes no rule for an opted-out
// member, which carries the group tag and bosh-ha-off but no HA resource.
func TestJoinAntiAffinityAtMetadata_OptedOutVMJoinsNoRule(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag+";index--1"),
		aaQEMU(102, aaTag+";"+haOffTag+";index--2"))
	markWebMembers(stub, 100, 101)
	joinAntiAffinityAtMetadata(context.Background(), aaMetadataDeps(stub), 102, map[string]any{"index": "2"},
		log.NewNopLogger())
	for id, members := range stub.rules {
		if strings.Contains(members, "vm:102") {
			t.Fatalf("rule %s = %q; an opted-out VM joins no rule", id, members)
		}
	}
}

// TestBuildVMShapeForNode_TagsAnOptedOutVM pins that the create writes
// bosh-ha-off into the VM's first tag list, and writes it only for a VM that
// opted out.
func TestBuildVMShapeForNode_TagsAnOptedOutVM(t *testing.T) {
	t.Parallel()
	for name, haOff := range map[string]bool{"opted out": true, "keeps HA": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := Deps{Config: haOptOutConfig(config.HAModeAuto), PVE: &shapeTestPVEClient{},
				Logger: log.NewNopLogger()}
			parsed := minimalParsedArgs("test-storage")
			parsed.cloudProps.haOff = haOff
			shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve01")
			if err != nil {
				t.Fatalf("buildVMShapeForNode: %v", err)
			}
			got := false
			for _, tag := range strings.Split(shape.initialTags, ";") {
				if tag == haOffTag {
					got = true
				}
			}
			if got != haOff {
				t.Fatalf("initial tags %q; want %s present %v", shape.initialTags, haOffTag, haOff)
			}
		})
	}
}

// TestCreateTimeTagPrefixes_ReserveTheOptOutTag pins that no operator tag can
// write bosh-ha-off, so only the create's own decision marks a VM as opted
// out.
func TestCreateTimeTagPrefixes_ReserveTheOptOutTag(t *testing.T) {
	t.Parallel()
	if !hasCreateTimePrefix(haOffTag) || !hasCreateTimePrefix(strings.ToUpper(haOffTag)) {
		t.Fatalf("%s must be a create-time tag", haOffTag)
	}
}

// TestHALocalRootPredicate_OptedOutVMKeepsItsCloneMode pins that the clone
// treats an opted-out VM as a VM without HA, so a node-local root under the
// hold policy keeps a linked clone and managed mode does not refuse it.
func TestHALocalRootPredicate_OptedOutVMKeepsItsCloneMode(t *testing.T) {
	t.Parallel()
	for _, haMode := range []string{config.HAModeManaged, config.HAModeAuto} {
		t.Run(haMode, func(t *testing.T) {
			t.Parallel()
			deps := managedCloneDeps(haMode, true)
			if !haLocalRootPredicate(t.Context(), deps, false)(managedCloneStorage) {
				t.Fatal("the rig must hold the root under HA, or the test proves nothing")
			}
			if haLocalRootPredicate(t.Context(), deps, true)(managedCloneStorage) {
				t.Fatal("an opted-out VM's root is not held under HA")
			}
			shape := &createVMShape{vmStorage: managedCloneStorage, haOff: true}
			mode, err := cloneModeForHA(t.Context(), deps, log.NewNopLogger(), shape, config.CloneModeLinked)
			if err != nil || mode != config.CloneModeLinked {
				t.Fatalf("clone mode %q err %v; want the linked clone kept", mode, err)
			}
			request := managedCloneRequest(t, true, "linked")
			request.HoldsLocalRoot = haLocalRootPredicate(t.Context(), deps, true)
			observed := &managedVMPlan{selection: request.Selection, inventory: request.Inventory, request: request,
				snapshotStart: request.Clock()}
			parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{haOff: true}}
			if err := rankManagedVMPlan(t.Context(), deps, parsed, observed); err != nil {
				t.Fatalf("an opted-out VM's linked root was refused: %v", err)
			}
			if got := managedCloneRoot(t, observed).Mechanism; got != storageMechanismLinkedClone {
				t.Fatalf("root mechanism = %q; want %q", got, storageMechanismLinkedClone)
			}
		})
	}
}

// TestBuildVMShapeForNode_OptOutTagSurvivesTheTagCap pins that bosh-ha-off
// survives the tag-length cap the way the ownership tags do. Long operator
// tags push the create's first tag list past the cap, and the cap drops
// operator tags rather than the opt-out marker. A later merge, such as the one
// set_vm_metadata runs over a list PVE may hand back sorted, keeps it too.
func TestBuildVMShapeForNode_OptOutTagSurvivesTheTagCap(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: haOptOutConfig(config.HAModeAuto), PVE: &shapeTestPVEClient{}, Logger: log.NewNopLogger(),
		RequestDirectorUUID: aaDirector}
	parsed := minimalParsedArgs("test-storage")
	parsed.cloudProps.haOff = true
	parsed.cloudProps.Tags = map[string]string{}
	for _, k := range []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"} {
		parsed.cloudProps.Tags[k] = strings.Repeat("v", 60)
	}
	shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve01")
	if err != nil {
		t.Fatalf("buildVMShapeForNode: %v", err)
	}
	if len(shape.initialTags) > maxTagLength {
		t.Fatalf("initial tags are %d bytes, over the %d-byte cap", len(shape.initialTags), maxTagLength)
	}
	entries := parseTagsField(shape.initialTags)
	if !slices.Contains(entries, haOffTag) {
		t.Fatalf("initial tags %q lost %s to the tag-length cap", shape.initialTags, haOffTag)
	}
	if !slices.ContainsFunc(entries, func(e string) bool { return strings.HasPrefix(e, "k") }) {
		t.Fatalf("initial tags %q kept no operator tag; the rig must leave room for some", shape.initialTags)
	}

	sorted := slices.Clone(entries)
	slices.Sort(sorted)
	extra := []string{"director--" + strings.Repeat("d", 120), "deployment--" + strings.Repeat("e", 120)}
	merged, dropped := mergeTagListReporting(hoistHeadTags(sorted), extra, maxTagLength)
	if !slices.Contains(parseTagsField(merged), haOffTag) || slices.Contains(dropped, haOffTag) {
		t.Fatalf("merged %q dropped %v; want %s kept", merged, dropped, haOffTag)
	}
}
