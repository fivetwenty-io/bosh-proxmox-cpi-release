package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// stagingLog is the ordered record of every HA write a staging test makes, so
// a test can show that no resource requests started before its confinement.
type stagingLog struct{ events []string }

func (l *stagingLog) add(e string) { l.events = append(l.events, e) }

// stageResFake is the HA resource of one VM as the staging order reads and
// writes it.
type stageResFake struct {
	log       *stagingLog
	exists    bool
	state     string
	comment   string
	createErr error
	updateErr error
}

var _ ha.ResourceAPI = (*stageResFake)(nil)

func (f *stageResFake) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	if !f.exists {
		return nil, errors.New("no such resource '" + sid + "'")
	}
	state, comment := f.state, f.comment
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: &comment}, nil
}

func (f *stageResFake) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.exists, f.state = true, *p.State
	if p.Comment != nil {
		f.comment = *p.Comment
	}
	f.log.add("stage:" + f.state)
	return nil
}

func (f *stageResFake) UpdateHaResources(_ context.Context, _ string, p *cluster.UpdateHaResourcesParams) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if p.State != nil {
		f.state = *p.State
		f.log.add("request:" + f.state)
	}
	return nil
}

func (f *stageResFake) DeleteHaResources(context.Context, string, *cluster.DeleteHaResourcesParams) error {
	f.exists = false
	f.log.add("unstage")
	return nil
}

// stagingNAStub is the rule store of naClusterStub with the pin writes
// recorded in the staging log, and an error the rule writes can return.
type stagingNAStub struct {
	*naClusterStub
	log     *stagingLog
	ruleErr error
}

func (s *stagingNAStub) CreateHaRules(ctx context.Context, p *cluster.CreateHaRulesParams) error {
	if s.ruleErr != nil {
		return s.ruleErr
	}
	s.log.add("pin:" + p.Rule)
	return s.naClusterStub.CreateHaRules(ctx, p)
}

func (s *stagingNAStub) DeleteHaRules(ctx context.Context, rule string) error {
	s.log.add("unpin:" + rule)
	return s.naClusterStub.DeleteHaRules(ctx, rule)
}

// stagingProfile is a healthy full-HA profile over pve01 to pve03 with shared
// NFS storage and node-local LVM-thin storage.
func stagingProfile(mode string) *ha.Profile {
	p := &ha.Profile{
		Mode: mode, Effective: ha.EffectiveFull, MinVersion: "9.2.4", HAVersionOK: true, DLBSupported: true,
		RulesSupported: true, SysConsole: true, VMReplicate: true,
		Storages: []pve.StorageInfo{
			{Name: "nfs", Type: pve.StorageTypeNFS, Content: "images,iso"},
			{Name: testPinStorage, Type: pve.StorageTypeLVMThin, Content: "images,rootdir"},
		},
	}
	for _, n := range []string{"pve01", "pve02", "pve03"} {
		p.Nodes = append(p.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
	}
	return p
}

// stagingRig wires one staged create_vm call to fakes.
type stagingRig struct {
	log       *stagingLog
	rules     *stagingNAStub
	res       *stageResFake
	deps      Deps
	h         *createdVMHA
	parsed    *createVMParsedArgs
	landing   string
	waitErr   error
	watchdog  pve.WatchdogState
	started   int
	startErr  error
	cloudProp createVMCloudProps
}

// newStagingRig returns a rig for VM 100 placed on pve01 in AZ z1 of pve01 and
// pve02, with pin_az on and the given strictness, under mode.
func newStagingRig(t *testing.T, mode string, strict bool) *stagingRig {
	t.Helper()
	sl := &stagingLog{}
	r := &stagingRig{
		log:       sl,
		rules:     &stagingNAStub{naClusterStub: newNAStub(), log: sl},
		res:       &stageResFake{log: sl},
		landing:   "pve01",
		watchdog:  pve.WatchdogArmed,
		cloudProp: createVMCloudProps{AvailabilityZone: "z1"},
	}
	cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}})
	cfg.HA = haPinOn(new(strict))
	cfg.HA.Mode = haChoiceOf(mode)
	profile := stagingProfile(mode)
	r.deps = Deps{
		Config: cfg, RequestDirectorUUID: aaDirector, PVE: &icPVEClient{clusterSvc: r.rules},
		HAProfile: func(context.Context) (*ha.Profile, error) { return profile, nil },
		Agent:     &icAgentStub{}, Logger: log.NewNopLogger(),
	}
	r.parsed = &createVMParsedArgs{cloudProps: r.cloudProp}
	return r
}

// stage builds the call's HA state the way decideCreatedVMHA does, with the
// stager's resource client, watchdog read, and state wait on the rig.
func (r *stagingRig) stage(t *testing.T) {
	t.Helper()
	d := testRegisteredDecision(r.deps, 100, r.parsed.cloudProps, "pve01")
	profile, _ := r.deps.HAProfile(context.Background())
	h := &createdVMHA{
		deps: r.deps, logger: log.NewNopLogger(), vmid: 100, ident: haIdentity{Owner: aaDirector},
		profile: profile, decision: d, storages: []string{testPinStorage},
	}
	h.stager = h.newStager(r.deps.Config.HAModeValue() == config.HAModeManaged)
	h.stager.Resources = r.res
	h.stager.Watchdog = func(context.Context, string) (pve.WatchdogState, error) { return r.watchdog, nil }
	h.stager.SetStarted = func(_ context.Context, vmid int, node string) (*pve.SetHAStateResult, error) {
		r.log.add("wait:" + node)
		if r.waitErr != nil {
			return nil, r.waitErr
		}
		return &pve.SetHAStateResult{Outcome: pve.HAStateReached, SID: haResourceSid(vmid), VMID: vmid,
			Node: r.landing}, nil
	}
	if err := h.stager.Stage(context.Background(), d, h.ident.marker()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	r.h = h
}

func (r *stagingRig) start(context.Context) error {
	r.started++
	r.log.add("start-direct")
	return r.startErr
}

// confineAndActivate runs steps 3 to 6 of the staging order on the rig.
func (r *stagingRig) confineAndActivate() (string, error) {
	ctx := context.Background()
	if err := r.h.confine(ctx, r.parsed, "pve01"); err != nil {
		return "", err
	}
	return r.h.activate(ctx, "pve01", r.start)
}

func TestCreatedVMHA_StagesConfinesThenActivates(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.stage(t)
	landing, err := r.confineAndActivate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"stage:ignored", "pin:bosh-na-100", "request:started", "wait:pve01"}
	if !slices.Equal(r.log.events, want) {
		t.Fatalf("events %v, want %v", r.log.events, want)
	}
	if landing != "pve01" || !r.h.haActivated() || r.h.staged() || r.started != 0 {
		t.Fatalf("landing %q activated %v staged %v direct starts %d", landing, r.h.haActivated(), r.h.staged(),
			r.started)
	}
	pin := r.rules.rules["bosh-na-100"]
	if pin.Comment == nil || !strings.Contains(*pin.Comment, "bosh-vols="+testPinStorage) {
		t.Errorf("the pin must carry the volume record, comment %v", pin.Comment)
	}
}

func TestCreatedVMHA_LandingElsewhereInTheAdmissibleSet(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.landing = "pve02"
	r.stage(t)
	landing, err := r.confineAndActivate()
	if err != nil || landing != "pve02" {
		t.Fatalf("landing %q err %v, want pve02", landing, err)
	}
}

func TestCreatedVMHA_LandingOutsideTheAdmissibleSetFails(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.landing = "pve03"
	r.stage(t)
	_, err := r.confineAndActivate()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "outside its hard") {
		t.Fatalf("err = %v, want a retriable landing refusal", err)
	}
}

func TestCreatedVMHA_StateWaitErrorFails(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.waitErr = cpierrors.Retriable("timed out")
	r.stage(t)
	if _, err := r.confineAndActivate(); err == nil {
		t.Fatalf("err = %v, want the state wait error", err)
	}
	if r.h.haActivated() || r.started != 0 {
		t.Fatal("a failed state wait must not count as activated or start the VM directly")
	}
}

// foreignPin puts a pin for VM 100 that another Director owns in the rule
// store, which the pin writer refuses to touch.
func (r *stagingRig) foreignPin() {
	other := haIdentity{Owner: "dir-other"}.marker()
	nodes := "pve01"
	r.rules.rules["bosh-na-100"] = cluster.CreateHaRulesParams{
		Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &other, Nodes: &nodes,
	}
}

func TestCreatedVMHA_AutoPinRefusalRunsWithoutHA(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.foreignPin()
	r.stage(t)
	landing, err := r.confineAndActivate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"stage:ignored", "unstage", "start-direct"}
	if !slices.Equal(r.log.events, want) {
		t.Fatalf("events %v, want %v", r.log.events, want)
	}
	if landing != "pve01" || r.h.haActivated() || r.h.decision.Outcome != ha.OutcomeSkip {
		t.Fatalf("landing %q activated %v outcome %v", landing, r.h.haActivated(), r.h.decision.Outcome)
	}
}

func TestCreatedVMHA_AutoRetriablePinFailureFailsTheCall(t *testing.T) {
	// A write that never lands is retriable, so the call fails and the
	// Director retries it, rather than run a VM without HA for a transient.
	r := newStagingRig(t, config.HAModeAuto, true)
	r.rules.ruleErr = errors.New("cfs lock timeout")
	r.stage(t)
	_, err := r.confineAndActivate()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
	if slices.Contains(r.log.events, "request:started") || r.started != 0 {
		t.Fatalf("events %v: nothing may start without its confinement", r.log.events)
	}
}

func TestCreatedVMHA_ManagedPinWriteFailureFailsTheCall(t *testing.T) {
	r := newStagingRig(t, config.HAModeManaged, true)
	r.foreignPin()
	r.stage(t)
	if _, err := r.confineAndActivate(); err == nil {
		t.Fatal("a managed call must fail when its pin cannot be written")
	}
	if slices.Contains(r.log.events, "request:started") || r.started != 0 {
		t.Fatalf("events %v: nothing may start without its confinement", r.log.events)
	}
	if !r.h.staged() {
		t.Fatal("the staged resource stays ignored for the rollback to remove")
	}
}

// disablePin marks the stored pin disabled, the shape of an operator edit or a
// rule PVE refused between the write and its verification.
func (r *stagingRig) disablePin() {
	p := r.rules.rules["bosh-na-100"]
	on := true
	p.Disable = &on
	r.rules.rules["bosh-na-100"] = p
}

func TestCreatedVMHA_UnverifiedConfinement(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		wantErr    bool
		wantEvents []string
	}{
		{config.HAModeAuto, false, []string{"stage:ignored", "pin:bosh-na-100", "unstage", "unpin:bosh-na-100",
			"start-direct"}},
		{config.HAModeManaged, true, []string{"stage:ignored", "pin:bosh-na-100"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newStagingRig(t, tc.mode, true)
			r.stage(t)
			ctx := context.Background()
			if err := r.h.confine(ctx, r.parsed, "pve01"); err != nil {
				t.Fatalf("confine: %v", err)
			}
			r.disablePin()
			_, err := r.h.activate(ctx, "pve01", r.start)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ha.ErrConfinementUnverified) {
				t.Fatalf("err = %v, want ErrConfinementUnverified", err)
			}
			if !slices.Equal(r.log.events, tc.wantEvents) {
				t.Fatalf("events %v, want %v", r.log.events, tc.wantEvents)
			}
		})
	}
}

func TestCreatedVMHA_WatchdogCheckOnAFragileQuorum(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		watchdog  pve.WatchdogState
		activated bool
	}{
		{"auto armed", config.HAModeAuto, pve.WatchdogArmed, true},
		{"auto idle", config.HAModeAuto, pve.WatchdogIdle, false},
		{"auto unknown", config.HAModeAuto, pve.WatchdogUnknown, false},
		{"managed idle", config.HAModeManaged, pve.WatchdogIdle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStagingRig(t, tc.mode, true)
			r.watchdog = tc.watchdog
			r.stage(t)
			r.h.stager.FragileQuorum = true
			if _, err := r.confineAndActivate(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.h.haActivated() != tc.activated {
				t.Fatalf("activated = %v, want %v; events %v", r.h.haActivated(), tc.activated, r.log.events)
			}
			if !tc.activated && (r.started != 1 || slices.Contains(r.log.events, "request:started")) {
				t.Fatalf("a VM whose node has no armed watchdog runs without HA; events %v", r.log.events)
			}
		})
	}
}

func TestCreatedVMHA_WatchdogIgnoredOnASafeQuorum(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.watchdog = pve.WatchdogUnknown
	r.stage(t)
	if _, err := r.confineAndActivate(); err != nil || !r.h.haActivated() {
		t.Fatalf("err %v activated %v; the check applies only one failure from quorum loss", err,
			r.h.haActivated())
	}
}

func TestCreatedVMHA_PCIVMNeedsAStrictPinToItsNode(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, false)
	r.parsed.cloudProps.PCIPassthroughs = []PCIPassthrough{{Address: "0000:01:00.0"}}
	r.stage(t)
	_, err := r.confineAndActivate()
	if err == nil || !strings.Contains(err.Error(), "PCI passthrough") {
		t.Fatalf("err = %v, want the PCI confinement failure", err)
	}
	if slices.Contains(r.log.events, "request:started") || r.started != 0 {
		t.Fatalf("events %v: a PCI VM without its confinement must not start", r.log.events)
	}
}

func TestCreatedVMHA_UnstageRefusalStopsTheDirectStart(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.watchdog = pve.WatchdogIdle
	r.stage(t)
	r.h.stager.FragileQuorum = true
	r.res.comment = "an operator took the resource over"
	_, err := r.confineAndActivate()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.started != 0 {
		t.Fatalf("err %v direct starts %d; a resource the CPI no longer owns stays and nothing starts", err,
			r.started)
	}
}

func TestCreatedVMHA_NilIsANoOp(t *testing.T) {
	var h *createdVMHA
	if h.staged() || h.haActivated() {
		t.Fatal("a nil value stages and activates nothing")
	}
	if err := h.confine(context.Background(), &createVMParsedArgs{}, "pve01"); err != nil {
		t.Fatalf("confine: %v", err)
	}
	node, err := h.activate(context.Background(), "pve01", func(context.Context) error {
		t.Fatal("a VM without HA was started before; activate must not start it again")
		return nil
	})
	if err != nil || node != "pve01" {
		t.Fatalf("activate: node %q err %v", node, err)
	}
}

// decideRig wires decideCreatedVMHA to fakes for VM 100 on pve01 whose root
// disk lives on the shared storage nfs.
func decideRig(mode, owner string, profileErr error) (Deps, *stagingNAStub, *createVMParsedArgs) {
	sl := &stagingLog{}
	rules := &stagingNAStub{naClusterStub: newNAStub(), log: sl}
	cfg := naPinConfig(map[string][]string{"z1": {"pve01", "pve02"}})
	cfg.HA = haPinOn(nil)
	cfg.HA.Mode = haChoiceOf(mode)
	profile := stagingProfile(mode)
	q := &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3"}, nil
	}}
	deps := Deps{
		Config: cfg, RequestDirectorUUID: owner, PVE: &icPVEClient{clusterSvc: rules, qemuSvc: q},
		HAProfile: func(context.Context) (*ha.Profile, error) {
			if profileErr != nil {
				return nil, profileErr
			}
			return profile, nil
		},
		Agent: &icAgentStub{}, Logger: log.NewNopLogger(),
	}
	parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{AvailabilityZone: "z1"}}
	return deps, rules, parsed
}

func TestDecideCreatedVMHA_ModeOffReadsNothing(t *testing.T) {
	cfg := icMinConfig()
	cfg.HA = haModeOff()
	deps := Deps{Config: cfg, Logger: log.NewNopLogger()}
	h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), &createVMParsedArgs{},
		&createVMShape{node: "pve01"}, 100)
	if h != nil || err != nil {
		t.Fatalf("h %v err %v, want nothing under mode off", h, err)
	}
}

func TestDecideCreatedVMHA_NoOwner(t *testing.T) {
	deps, rules, parsed := decideRig(config.HAModeAuto, "", nil)
	h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100)
	if h != nil || err != nil || len(rules.resources) != 0 {
		t.Fatalf("auto: h %v err %v resources %v, want no HA", h, err, rules.resources)
	}
	deps, _, parsed = decideRig(config.HAModeManaged, "", nil)
	if _, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100); err == nil || !strings.Contains(err.Error(), "director_uuid") {
		t.Fatalf("managed: err = %v, want the missing director_uuid", err)
	}
}

func TestDecideCreatedVMHA_ProfileError(t *testing.T) {
	boom := errors.New("cluster status unreadable")
	deps, rules, parsed := decideRig(config.HAModeAuto, aaDirector, boom)
	h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100)
	if h != nil || err != nil || len(rules.resources) != 0 {
		t.Fatalf("auto: h %v err %v, want no HA", h, err)
	}
	deps, _, parsed = decideRig(config.HAModeManaged, aaDirector, boom)
	if _, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("managed: err = %v, want retriable", err)
	}
}

func TestDecideCreatedVMHA_ConfigReadErrorIsRetriable(t *testing.T) {
	deps, rules, parsed := decideRig(config.HAModeAuto, aaDirector, nil)
	deps.PVE = &icPVEClient{clusterSvc: rules, qemuSvc: &icQEMUService{
		configFn: func(context.Context, string, int) (map[string]any, error) { return nil, errors.New("500") },
	}}
	if _, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
}

func TestDecideCreatedVMHA_SharedStorageStagesTheResource(t *testing.T) {
	deps, rules, parsed := decideRig(config.HAModeAuto, aaDirector, nil)
	h, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !h.staged() || !rules.resources["vm:100"] {
		t.Fatalf("staged %v resources %v, want vm:100 staged", h.staged(), rules.resources)
	}
	if got := rules.resComments["vm:100"]; got != (haIdentity{Owner: aaDirector}).marker() {
		t.Errorf("resource comment %q, want the ownership marker", got)
	}
	if !slices.Equal(h.storages, []string{"nfs"}) {
		t.Errorf("storages %v, want [nfs]", h.storages)
	}
	if len(rules.rules) != 0 {
		t.Errorf("the decision writes no rule; rules %v", rules.rules)
	}
}

func TestDecideCreatedVMHA_StageFailureFailsTheCall(t *testing.T) {
	deps, rules, parsed := decideRig(config.HAModeAuto, aaDirector, nil)
	failing := &stageCreateFails{naClusterStub: rules.naClusterStub}
	deps.PVE.(*icPVEClient).clusterSvc = failing
	_, err := decideCreatedVMHA(context.Background(), deps, log.NewNopLogger(), parsed,
		&createVMShape{node: "pve01"}, 100)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable stage failure", err)
	}
}

// stageCreateFails refuses every HA resource registration.
type stageCreateFails struct{ *naClusterStub }

func (s *stageCreateFails) CreateHaResources(context.Context, *cluster.CreateHaResourcesParams) error {
	return errors.New("cfs lock timeout")
}

func TestHAVolumesOfConfig(t *testing.T) {
	cfg := map[string]any{
		"scsi0": "nfs:vm-100-disk-0,size=10G",
		"scsi1": "nfs:vm-100-disk-1,size=10G,replicate=0",
		"ide2":  "nfs:iso/cfg.iso,media=cdrom",
	}
	got := map[string]ha.VMVolume{}
	for _, v := range haVolumesOfConfig(cfg, "pve01", map[string]bool{"nfs:vm-100-disk-1": true}) {
		got[v.Volume] = v
	}
	if v := got["nfs:vm-100-disk-0"]; v.Kind != ha.VolumeDisk || v.NoReplicate || v.Legacy {
		t.Errorf("root disk %+v", v)
	}
	if v := got["nfs:vm-100-disk-1"]; !v.NoReplicate || !v.Legacy {
		t.Errorf("legacy replicate=0 disk %+v", v)
	}
	if v := got["nfs:iso/cfg.iso"]; v.Kind != ha.VolumeISO {
		t.Errorf("cdrom %+v", v)
	}
}

func TestRecordLegacyDisk(t *testing.T) {
	p := &createVMParsedArgs{}
	p.recordLegacyDisk(resolvedDisk{volid: "nfs:vm-9-disk-0", stableID: "abcd1234"})
	p.recordLegacyDisk(resolvedDisk{})
	if p.legacyDiskVolids != nil {
		t.Fatalf("a stable-ID disk and an empty one record nothing, got %v", p.legacyDiskVolids)
	}
	p.recordLegacyDisk(resolvedDisk{volid: "nfs:vm-9-disk-1"})
	if !p.legacyDiskVolids["nfs:vm-9-disk-1"] || len(p.legacyDiskVolids) != 1 {
		t.Fatalf("legacy %v, want the disk without a stable ID", p.legacyDiskVolids)
	}
}

func TestConfigHasLinkedClone(t *testing.T) {
	if !configHasLinkedClone(map[string]any{"scsi0": "nfs:base-9000-disk-0/vm-100-disk-0.qcow2,size=10G"}) {
		t.Error("a volume named after its base is a linked clone")
	}
	if configHasLinkedClone(map[string]any{"scsi0": "nfs:100/vm-100-disk-0.qcow2,size=10G"}) {
		t.Error("a full clone is not a linked clone")
	}
}

// taggedVMClient serves the rig's rule store and a VM config whose tags the
// tag writes change, so the HA-managed tag can be read back.
func taggedVMClient(rules cluster.Service, tags *string, writes *[]string) *attachRunClient {
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		return map[string]any{"tags": *tags, "digest": "d"}, nil
	}}
	nodesStub := &lrNodesStub{}
	client := &attachRunClient{lrClient: &lrClient{nodes: nodesStub, qemu: q}, cluster: rules}
	nodesStub.onUpdate = func() {
		p := nodesStub.updateParams[len(nodesStub.updateParams)-1]
		switch {
		case p.Tags != nil:
			*tags = *p.Tags
		case p.Delete != nil:
			*tags = ""
		}
		*writes = append(*writes, *tags)
	}
	return client
}

func TestCompleteCreatedVM_ActivatesTagsAndFollowsTheLanding(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	r.landing = "pve02"
	tags, writes := "bosh-cpi", []string{}
	r.deps.PVE = taggedVMClient(r.rules, &tags, &writes)
	r.stage(t)
	shape := &createVMShape{node: "pve01"}
	if err := completeCreatedVM(crhCtx(), r.deps, log.NewNopLogger(), r.parsed, shape, 100, r.h, r.start); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shape.node != "pve02" {
		t.Errorf("shape node %q, want the landing node pve02", shape.node)
	}
	if !slices.Equal(writes, []string{"bosh-cpi;" + haManagedTag}) {
		t.Errorf("tag writes %v, want one write adding %s", writes, haManagedTag)
	}
	if r.started != 0 {
		t.Error("PVE HA starts a registered VM; the CPI must not start it directly")
	}
}

func TestCompleteCreatedVM_WithoutHAWritesNoTag(t *testing.T) {
	r := newStagingRig(t, config.HAModeAuto, true)
	tags, writes := "bosh-cpi", []string{}
	r.deps.PVE = taggedVMClient(r.rules, &tags, &writes)
	shape := &createVMShape{node: "pve01"}
	if err := completeCreatedVM(crhCtx(), r.deps, log.NewNopLogger(), r.parsed, shape, 100, nil, r.start); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(writes) != 0 || r.started != 0 || shape.node != "pve01" {
		t.Errorf("writes %v starts %d node %q; a VM without HA was started before and gets no tag", writes,
			r.started, shape.node)
	}
}

func TestHAManagedTagAddAndRemove(t *testing.T) {
	tags, writes := "bosh-cpi;custom", []string{}
	deps := Deps{Config: lrTokenConfig(), Logger: log.NewNopLogger()}
	deps.PVE = taggedVMClient(newNAStub(), &tags, &writes)
	if err := addHAManagedTag(crhCtx(), deps, "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := addHAManagedTag(crhCtx(), deps, "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("add again: %v", err)
	}
	if err := removeHAManagedTag(crhCtx(), deps, "pve01", 100, "test: untag", log.NewNopLogger()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	want := []string{"bosh-cpi;custom;" + haManagedTag, "bosh-cpi;custom"}
	if !slices.Equal(writes, want) {
		t.Fatalf("writes %v, want %v; a tag already present needs no write", writes, want)
	}
}
