package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// attachHAStub is the cluster of an attach-time HA decision: the rule store of
// naClusterStub, the HA resource of VM 100, and an index row that places the
// VM on pve01.
type attachHAStub struct {
	*naClusterStub
	missing   bool
	getErr    error
	state     string
	comment   string
	rebalance bool
}

func (s *attachHAStub) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	switch {
	case s.getErr != nil:
		return nil, s.getErr
	case s.missing:
		return nil, errors.New("no such resource '" + sid + "' (HTTP500)")
	}
	state, comment := s.state, s.comment
	rebalance := sdkclient.PVEBool(s.rebalance)
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: &comment, AutoRebalance: &rebalance}, nil
}

func (s *attachHAStub) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse,
	error,
) {
	raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": 100, "node": "pve01", "id": "qemu/100"})
	return &cluster.ListResourcesResponse{raw}, nil
}

// attachProfile is the staging profile with two more NFS storages: nfs2 on
// every node, and nfs-ab on pve01 and pve02 only.
func attachProfile(mode string) *ha.Profile {
	p := stagingProfile(mode)
	p.Storages = append(p.Storages,
		pve.StorageInfo{Name: "nfs2", Type: pve.StorageTypeNFS, Content: "images"},
		pve.StorageInfo{Name: "nfs-ab", Type: pve.StorageTypeNFS, Nodes: []string{"pve01", "pve02"}, Content: "images"},
	)
	return p
}

// attachRig returns the deps of an attach to VM 100, which runs on pve01 with
// a root disk on nfs and is registered by aaDirector, and its cluster stub.
func attachRig(mode string) (Deps, *attachHAStub, *ha.Profile) {
	stub := &attachHAStub{naClusterStub: newNAStub(), state: "started",
		comment: haIdentity{Owner: aaDirector}.marker()}
	stub.members = []string{"pve01", "pve02", "pve03"}
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(mode)}
	profile := attachProfile(mode)
	q := &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3"}, nil
	}}
	deps := Deps{
		Config: cfg, RequestDirectorUUID: aaDirector, PVE: &icPVEClient{clusterSvc: stub, qemuSvc: q},
		HAProfile: func(context.Context) (*ha.Profile, error) { return profile, nil },
		Agent:     &icAgentStub{}, Logger: log.NewNopLogger(),
	}
	return deps, stub, profile
}

// recordPin stores a non-strict pin for VM 100 owned by aaDirector over pve01,
// pve02, and pve03 that records storages.
func (s *attachHAStub) recordPin(storages ...string) {
	nodes, strict := "pve01,pve02,pve03", false
	comment := ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), storages)
	s.rules["bosh-na-100"] = cluster.CreateHaRulesParams{
		Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &comment, Nodes: &nodes,
		Strict: &strict,
	}
}

// pin stores a pin for VM 100 owned by aaDirector over nodes, strict or not,
// that records storages.
func (s *attachHAStub) pin(strict bool, nodes string, storages ...string) {
	comment := ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), storages)
	s.rules["bosh-na-100"] = cluster.CreateHaRulesParams{
		Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &comment, Nodes: &nodes,
		Strict: &strict,
	}
}

func sharedDisk() resolvedDisk {
	return resolvedDisk{volid: "nfs2:vm-100-disk-1", stableID: "abcd1234"}
}

func TestDecideAttachHA_NoAction(t *testing.T) {
	ctx := context.Background()
	t.Run("mode off", func(t *testing.T) {
		deps, _, _ := attachRig(config.HAModeOff)
		deps.PVE = nil
		if h, err := decideAttachHA(ctx, deps, "100", sharedDisk()); h != nil || err != nil {
			t.Fatalf("h %v err %v", h, err)
		}
	})
	t.Run("unparsable CID", func(t *testing.T) {
		deps, _, _ := attachRig(config.HAModeAuto)
		if h, err := decideAttachHA(ctx, deps, "vm-x", sharedDisk()); h != nil || err != nil {
			t.Fatalf("h %v err %v", h, err)
		}
	})
	t.Run("no resource", func(t *testing.T) {
		deps, stub, _ := attachRig(config.HAModeAuto)
		stub.missing = true
		if h, err := decideAttachHA(ctx, deps, "100", sharedDisk()); h != nil || err != nil {
			t.Fatalf("h %v err %v", h, err)
		}
	})
	t.Run("another Director's resource", func(t *testing.T) {
		deps, stub, _ := attachRig(config.HAModeAuto)
		stub.comment = haIdentity{Owner: "dir-other"}.marker()
		if h, err := decideAttachHA(ctx, deps, "100", sharedDisk()); h != nil || err != nil {
			t.Fatalf("h %v err %v", h, err)
		}
	})
	t.Run("storage already recorded", func(t *testing.T) {
		deps, stub, _ := attachRig(config.HAModeAuto)
		stub.recordPin("nfs", "nfs2")
		deps.HAProfile = func(context.Context) (*ha.Profile, error) {
			t.Fatal("a recorded storage needs no profile")
			return nil, nil
		}
		if h, err := decideAttachHA(ctx, deps, "100", sharedDisk()); h != nil || err != nil {
			t.Fatalf("h %v err %v", h, err)
		}
	})
}

func TestDecideAttachHA_ManagedLegacyDiskOnARecordedStorageIsDecided(t *testing.T) {
	// A recorded storage skips the decision, but in managed mode a disk with no
	// stable ID must still meet the decision's legacy refusal.
	deps, stub, _ := attachRig(config.HAModeManaged)
	stub.recordPin("nfs", "nfs2")
	h, err := decideAttachHA(context.Background(), deps, "100", resolvedDisk{volid: "nfs2:vm-100-disk-1"})
	if err == nil || h != nil || !strings.Contains(err.Error(), "no stable ID") {
		t.Fatalf("h %+v err %v, want the legacy refusal", h, err)
	}
	if h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk()); h != nil || err != nil {
		t.Fatalf("a stable-ID disk on a recorded storage: h %+v err %v, want no action", h, err)
	}
}

func TestDecideAttachHA_ReadFailuresAreRetriable(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.getErr = errors.New("connection reset")
	_, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
	deps, _, _ = attachRig(config.HAModeAuto)
	deps.HAProfile = func(context.Context) (*ha.Profile, error) { return nil, errors.New("status unreadable") }
	_, err = decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("profile: err = %v, want retriable", err)
	}
}

func TestDecideAttachHA_ForeignPinFails(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	other := haIdentity{Owner: "dir-other"}.marker()
	nodes := "pve01"
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &other, Nodes: &nodes}
	_, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err == nil || !strings.Contains(err.Error(), "another Director") {
		t.Fatalf("err = %v, want the foreign pin refusal", err)
	}
}

func TestDecideAttachHA_PendingBarrierRunsTheDecision(t *testing.T) {
	// A recorded storage normally skips the decision, but a resource an earlier
	// barrier left ignored with its token must have the barrier finished.
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs", "nfs2")
	stub.state = "ignored"
	stub.comment = haIdentity{Owner: aaDirector}.marker() + " bosh-narrow-from=started"
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHAGuarded {
		t.Fatalf("h %+v, want the barrier to run", h)
	}
}

func TestDecideAttachHA_ProfileThatMayNotWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*ha.Profile)
		want string
	}{
		"no Sys.Console": {func(p *ha.Profile) { p.Effective, p.SysConsole = ha.EffectiveObserveOnly, false }, "Sys.Console"},
		"a gap":          {func(p *ha.Profile) { p.Effective, p.Reason = ha.EffectiveObserveOnly, "no quorum" }, "no quorum"},
	} {
		t.Run(name, func(t *testing.T) {
			deps, _, profile := attachRig(config.HAModeAuto)
			tc.edit(profile)
			_, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a retriable error naming %q", err, tc.want)
			}
		})
	}
}

func TestDecideAttachHA_SharedDiskKeepsThePinAndAddsTheRecord(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.action != attachHAGuarded || h.newPin == nil || h.oldPin == nil {
		t.Fatalf("action %v newPin %v oldPin %v", h.action, h.newPin, h.oldPin)
	}
	recorded, ok := ha.VolumeRecord(h.newPin.Comment)
	if !ok || !slices.Contains(recorded, "nfs") || !slices.Contains(recorded, "nfs2") {
		t.Fatalf("record %v, want nfs and nfs2", recorded)
	}
	if !slices.Equal(h.newPin.NodeNames(), []string{"pve01", "pve02", "pve03"}) || h.newPin.Strict {
		t.Fatalf("new pin %v strict %v, want the old nodes and not strict", h.newPin.NodeNames(), h.newPin.Strict)
	}
	if h.node != "pve01" {
		t.Errorf("node %q, want pve01", h.node)
	}
}

func TestDecideAttachHA_StrictPinIsTrimmedAndStaysStrict(t *testing.T) {
	// A strict pin, such as the AZ pin of a strict pin_az, is a hard
	// constraint. A disk that fewer nodes reach trims it to those nodes, and
	// it stays strict.
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.pin(true, "pve01,pve02,pve03", "nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHAGuarded || h.newPin == nil {
		t.Fatalf("h %+v, want a guarded attach with a new pin", h)
	}
	if !h.newPin.Strict || !slices.Equal(h.newPin.NodeNames(), []string{"pve01", "pve02"}) {
		t.Fatalf("new pin %v strict %v, want strict pve01 and pve02", h.newPin.NodeNames(), h.newPin.Strict)
	}
	if rec, ok := ha.VolumeRecord(h.newPin.Comment); !ok || !slices.Equal(rec, []string{"nfs", "nfs-ab"}) {
		t.Errorf("record %v, want nfs and nfs-ab", rec)
	}
}

func TestDecideAttachHA_RebalancePreferenceIsNoConstraint(t *testing.T) {
	// Under rebalance on start the CPI writes a single-node pin that is not
	// strict, as a preference. A shared disk keeps the VM in HA, and the new
	// pin does not become strict.
	deps, stub, profile := attachRig(config.HAModeAuto)
	profile.CRS = ha.CRS{Known: true, Mode: "static", RebalanceOnStart: true}
	stub.rebalance = true
	stub.pin(false, "pve01", "nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHAGuarded || h.newPin == nil {
		t.Fatalf("h %+v, want the VM to stay in HA with a new pin", h)
	}
	if h.newPin.Strict {
		t.Fatalf("new pin %v is strict; a preference must never become strict", h.newPin.NodeNames())
	}
	if rec, ok := ha.VolumeRecord(h.newPin.Comment); !ok || !slices.Equal(rec, []string{"nfs", "nfs2"}) {
		t.Errorf("record %v, want nfs and nfs2", rec)
	}
	if len(h.decision.Admissible) < 2 {
		t.Errorf("admissible %v; the preference must not shrink the access set", h.decision.Admissible)
	}
}

func TestDecideAttachHA_NodeOutageDefersRatherThanLeave(t *testing.T) {
	// pve01 and pve02 reach the new disk, but pve02 is down. The VM keeps its
	// HA state, and the attach waits for the node.
	deps, stub, profile := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	for i := range profile.Nodes {
		if profile.Nodes[i].Name == "pve02" {
			profile.Nodes[i].Online = false
		}
	}
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
		t.Fatalf("h %+v err %v, want a retriable deferral", h, err)
	}
	if !strings.Contains(err.Error(), "healthy again") {
		t.Errorf("err = %v, want the deferral to say it waits for the nodes", err)
	}
}

func TestDecideAttachHA_PendingLeaveFinishesTheRemoval(t *testing.T) {
	// An earlier attach removed the VM's resource but not its pin. The next
	// attach finishes the removal before it attaches.
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.missing = true
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHALeave || h.node != "pve01" || h.oldPin == nil {
		t.Fatalf("h %+v, want a leave on pve01 that names the pin", h)
	}

	// A pin another Director owns is not this call's to finish.
	deps, stub, _ = attachRig(config.HAModeAuto)
	stub.missing = true
	stub.recordPin("nfs")
	other := haIdentity{Owner: aaOtherDirector}.marker()
	p := stub.rules["bosh-na-100"]
	p.Comment = &other
	stub.rules["bosh-na-100"] = p
	if h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk()); h != nil || err != nil {
		t.Fatalf("a foreign pin: h %+v err %v, want no action", h, err)
	}
}

func TestDecideAttachHA_LocalDiskLeavesHA(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100",
		resolvedDisk{volid: testPinStorage + ":vm-100-disk-1", stableID: "abcd1234"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.action != attachHALeave {
		t.Fatalf("h %+v, want the VM to leave HA under local_policy none", h)
	}
}

func TestDecideAttachHA_MalformedVolidIsDiskNotFound(t *testing.T) {
	deps, _, _ := attachRig(config.HAModeAuto)
	_, err := decideAttachHA(context.Background(), deps, "100", resolvedDisk{volid: "novolid"})
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("err = %v, want DiskNotFound", err)
	}
}

func TestAttachNewPin(t *testing.T) {
	profile := attachProfile(config.HAModeAuto)
	old := &ha.Rule{ID: "bosh-na-100", Type: ha.RuleTypeNodeAffinity, Resources: []string{"vm:100"},
		Nodes: map[string]int{"pve01": 2, "pve02": 1, "pve03": 1}, Comment: "m", Errors: []string{"x: y"}, Strict: true}
	d := &ha.VMDecision{VMID: 100, SID: "vm:100", Outcome: ha.OutcomeRegister, Admissible: []string{"pve01", "pve02"}}
	pin, err := attachNewPin(d, old, profile, &ha.RuleSet{}, []string{"nfs", "nfs2"}, aaDirector)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(pin.NodeNames(), []string{"pve01", "pve02"}) || pin.Nodes["pve01"] != 2 {
		t.Errorf("nodes %v, want the admissible nodes with their priorities", pin.Nodes)
	}
	if !pin.Strict || len(pin.Errors) != 0 {
		t.Errorf("strict %v errors %v, want a strict pin with no errors", pin.Strict, pin.Errors)
	}
	if rec, ok := ha.VolumeRecord(pin.Comment); !ok || !slices.Equal(rec, []string{"nfs", "nfs2"}) {
		t.Errorf("record %v", rec)
	}
	if len(old.Nodes) != 3 || !old.Strict {
		t.Error("the old pin must not be changed")
	}

	none := &ha.VMDecision{VMID: 100, SID: "vm:100", Outcome: ha.OutcomeRegister, Admissible: []string{"pve09"}}
	if _, err := attachNewPin(none, old, profile, &ha.RuleSet{}, []string{"nfs"}, aaDirector); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) {
		t.Errorf("a pin with no admissible node: err = %v, want retriable", err)
	}
	if _, err := attachNewPin(d, old, profile, &ha.RuleSet{}, nil, aaDirector); err == nil {
		t.Error("a pin with no volume to record must fail")
	}
	fresh, err := attachNewPin(d, nil, profile, &ha.RuleSet{}, []string{"nfs"}, aaDirector)
	if err != nil || fresh == nil {
		t.Fatalf("a VM with no pin gets the decision's pin: %v %v", fresh, err)
	}
}

func TestAttachNewPin_NonStrictPinIsRebuiltAndStaysAPreference(t *testing.T) {
	// A pin that is not strict, such as the single-node preference that
	// rebalance on start writes, is no constraint. The attach rebuilds the pin
	// from the decision, so it keeps every admissible node and is never made
	// strict.
	profile := attachProfile(config.HAModeAuto)
	d := &ha.VMDecision{VMID: 100, SID: "vm:100", Outcome: ha.OutcomeRegister, Admissible: []string{"pve01", "pve02"}}
	for name, old := range map[string]*ha.Rule{
		"preference": {ID: "bosh-na-100", Type: ha.RuleTypeNodeAffinity, Resources: []string{"vm:100"},
			Nodes: map[string]int{"pve01": 2}, Comment: "m"},
		"disabled strict": {ID: "bosh-na-100", Type: ha.RuleTypeNodeAffinity, Resources: []string{"vm:100"},
			Nodes: map[string]int{"pve01": 2}, Comment: "m", Strict: true, Disabled: true},
	} {
		pin, err := attachNewPin(d, old, profile, &ha.RuleSet{}, []string{"nfs", "nfs2"}, aaDirector)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if pin.Strict {
			t.Errorf("%s: a pin rebuilt from a preference must not be strict", name)
		}
		if !slices.Equal(pin.NodeNames(), []string{"pve01", "pve02"}) {
			t.Errorf("%s: nodes %v, want every admissible node", name, pin.NodeNames())
		}
		if rec, ok := ha.VolumeRecord(pin.Comment); !ok || !slices.Equal(rec, []string{"nfs", "nfs2"}) {
			t.Errorf("%s: record %v", name, rec)
		}
		if len(old.Nodes) != 1 {
			t.Errorf("%s: the old pin must not be changed", name)
		}
	}
}

func TestAttachHAInput(t *testing.T) {
	cfg := icMinConfig()
	cfg.HA = haModeAuto()
	deps := Deps{Config: cfg}
	vmCfg := map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "hostpci0": "0000:01:00.0"}
	rebalance := sdkclient.PVEBool(true)
	old := &ha.Rule{ID: "bosh-na-100", Nodes: map[string]int{"pve01": 0, "pve02": 0}, Strict: true}
	in := attachHAInput(deps, 100, "pve01", vmCfg, &cluster.GetHaResourcesResponse{AutoRebalance: &rebalance}, old,
		resolvedDisk{volid: "nfs2:vm-100-disk-1"})
	if !in.InDLBAZ {
		t.Error("a resource that rebalances is a DLB VM")
	}
	var legacy []string
	for _, v := range in.Volumes {
		if v.Legacy {
			legacy = append(legacy, v.Volume)
		}
	}
	if !slices.Equal(legacy, []string{"nfs2:vm-100-disk-1"}) {
		t.Errorf("legacy %v, want only the new disk, which carries no stable ID", legacy)
	}
	names := make([]string, 0, len(in.Constraints))
	for _, c := range in.Constraints {
		names = append(names, c.Name)
	}
	if !slices.Contains(names, "the VM's pin bosh-na-100") || !slices.Contains(names, pciConstraintName) {
		t.Errorf("constraints %v, want the pin and the PCI device", names)
	}

	// A pin that is not strict is a preference, so it bounds nothing.
	for _, loose := range []*ha.Rule{
		{ID: "bosh-na-100", Nodes: map[string]int{"pve01": 0}},
		{ID: "bosh-na-100", Nodes: map[string]int{"pve01": 0}, Strict: true, Disabled: true},
	} {
		in := attachHAInput(deps, 100, "pve01", vmCfg, &cluster.GetHaResourcesResponse{AutoRebalance: &rebalance},
			loose, resolvedDisk{volid: "nfs2:vm-100-disk-1"})
		for _, c := range in.Constraints {
			if c.Name == "the VM's pin bosh-na-100" {
				t.Errorf("strict %v disabled %v: a pin that binds nothing became a constraint", loose.Strict,
					loose.Disabled)
			}
		}
	}
}

func TestAttachHARun_NoActionAttaches(t *testing.T) {
	calls := 0
	attach := func(context.Context) (string, error) { calls++; return "pve01", nil }
	var h *attachHA
	if err := h.run(context.Background(), attach); err != nil || calls != 1 {
		t.Fatalf("nil: err %v calls %d", err, calls)
	}
	if err := (&attachHA{}).run(context.Background(), attach); err != nil || calls != 2 {
		t.Fatalf("none: err %v calls %d", err, calls)
	}
}

// attachRunRes is the HA resource the barrier and the removal read and write.
type attachRunRes struct {
	log     *stagingLog
	state   string
	comment string
	gone    bool
}

func (r *attachRunRes) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	if r.gone {
		return nil, errors.New("no such resource '" + sid + "'")
	}
	state, comment := r.state, r.comment
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: &comment}, nil
}

func (r *attachRunRes) CreateHaResources(context.Context, *cluster.CreateHaResourcesParams) error {
	r.log.add("register")
	return nil
}

func (r *attachRunRes) UpdateHaResources(_ context.Context, _ string, p *cluster.UpdateHaResourcesParams) error {
	if p.State != nil {
		r.state = *p.State
		r.log.add("request:" + r.state)
	}
	switch {
	case p.Comment != nil:
		r.comment = *p.Comment
	case p.Delete != nil && *p.Delete == "comment":
		r.comment = ""
	}
	return nil
}

func (r *attachRunRes) DeleteHaResources(context.Context, string, *cluster.DeleteHaResourcesParams) error {
	r.gone = true
	r.log.add("leave")
	return nil
}

// barrierView answers the barrier's state reads: the CRM lists the service
// on the first read, and every later read shows a round newer than the
// request with an LRM report newer still, the shape of a CRM that let go.
func barrierView(r *attachRunRes) ha.NarrowDeps {
	reads := 0
	now := time.Unix(1000, 0)
	return ha.NarrowDeps{
		Resources: r,
		ReadState: func(context.Context, int, string) (*pve.HAServiceState, error) {
			reads++
			if reads == 1 {
				return &pve.HAServiceState{Known: true, InCRM: true, Configured: true, Raw: "started", Node: "pve01",
					EntryTime: 100, CRMStamp: 90, LRMStamp: 95}, nil
			}
			return &pve.HAServiceState{Known: true, Configured: !r.gone, Node: "pve01", EntryTime: 110,
				CRMStamp: 120 + int64(reads), LRMStamp: 130 + int64(reads)}, nil
		},
		Settle: func(context.Context, int) (pve.VMLocation, error) {
			return pve.VMLocation{Node: "pve01", Found: true}, nil
		},
		Now: func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(10 * time.Second)
			return nil
		},
		Logger: log.NewNopLogger(),
	}
}

func TestAttachHARun_LeaveRemovesHAThenAttaches(t *testing.T) {
	sl := &stagingLog{}
	rules := &stagingNAStub{naClusterStub: newNAStub(), log: sl}
	comment := haIdentity{Owner: aaDirector}.marker()
	nodes := "pve01,pve02"
	rules.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes}
	tagged := true
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		if tagged {
			return map[string]any{"tags": "bosh-cpi;" + haManagedTag, "digest": "d1"}, nil
		}
		return map[string]any{"tags": "bosh-cpi", "digest": "d2"}, nil
	}}
	nodesStub := &lrNodesStub{onUpdate: func() { tagged = false; sl.add("untag") }}
	client := &attachRunClient{lrClient: &lrClient{nodes: nodesStub, qemu: q}, cluster: rules}
	cfg := lrTokenConfig()
	cfg.HA = haModeAuto()
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector, PVE: client, Logger: log.NewNopLogger(),
		HAProfile: pve9Profile}
	res := &attachRunRes{log: sl, state: "started", comment: comment}
	h := &attachHA{deps: deps, logger: log.NewNopLogger(), vmid: 100, sid: "vm:100", node: "pve01",
		action: attachHALeave, profile: stagingProfile(config.HAModeAuto), narrow: barrierView(res),
		disk: sharedDisk()}
	attach := func(context.Context) (string, error) { sl.add("attach"); return "pve01", nil }
	if err := h.run(crhCtx(), attach); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"leave", "untag", "unpin:bosh-na-100", "attach"}
	if !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
}

func TestAttachHARun_RefusalBeforeTheLeaveChangesNoHA(t *testing.T) {
	// The snapshot guard refuses the attach. It runs before the leave, so the
	// VM keeps its HA resource, its pin, and its tag.
	sl := &stagingLog{}
	rules := &stagingNAStub{naClusterStub: newNAStub(), log: sl}
	comment := haIdentity{Owner: aaDirector}.marker()
	nodes := "pve01,pve02"
	rules.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes}
	q := &lrQEMUStub{snapshots: []map[string]any{{"name": "before-upgrade"}, {"name": "current"}}}
	nodesStub := &lrNodesStub{onUpdate: func() { sl.add("untag") }}
	client := &attachRunClient{lrClient: &lrClient{nodes: nodesStub, qemu: q}, cluster: rules}
	cfg := lrTokenConfig()
	cfg.HA = haModeAuto()
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector, PVE: client, Logger: log.NewNopLogger(),
		HAProfile: pve9Profile}
	res := &attachRunRes{log: sl, state: "started", comment: comment}
	h := &attachHA{deps: deps, logger: log.NewNopLogger(), vmid: 100, sid: "vm:100", node: "pve01",
		action: attachHALeave, profile: stagingProfile(config.HAModeAuto), narrow: barrierView(res),
		disk: sharedDisk()}
	attached := false
	err := h.run(crhCtx(), func(context.Context) (string, error) { attached = true; return "pve01", nil })
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("err = %v, want the snapshot guard's refusal", err)
	}
	if attached || len(sl.events) != 0 || res.gone || res.state != "started" {
		t.Fatalf("attached %v events %v gone %v state %q; a refusal must change nothing", attached, sl.events,
			res.gone, res.state)
	}
	if _, ok := rules.rules["bosh-na-100"]; !ok {
		t.Fatal("a refusal must keep the pin")
	}
}

func TestAttachHARun_LeaveRefusalAttachesNothing(t *testing.T) {
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "error", comment: haIdentity{Owner: aaDirector}.marker()}
	deps, _, _ := attachRig(config.HAModeAuto)
	h := &attachHA{deps: deps, logger: log.NewNopLogger(), vmid: 100, sid: "vm:100", node: "pve01",
		action: attachHALeave, profile: stagingProfile(config.HAModeAuto), narrow: barrierView(res),
		disk: sharedDisk()}
	attached := false
	err := h.run(context.Background(), func(context.Context) (string, error) {
		attached = true
		return "pve01", nil
	})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || attached {
		t.Fatalf("err %v attached %v; a resource in the error state stays and nothing is attached", err, attached)
	}
}

func TestAttachHARun_WideningWritesThePinAfterTheAttach(t *testing.T) {
	// A disk on storage every node reaches keeps the pin's nodes, so the change
	// does not narrow: the attach runs with no barrier and the pin then gains
	// the storage in its record.
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	sl := &stagingLog{}
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	err = h.run(context.Background(), func(context.Context) (string, error) {
		sl.add("attach")
		// A widening writes the pin only after the attach, so the pin does
		// not record the new storage while the attach runs.
		pin := stub.rules["bosh-na-100"]
		if rec, _ := ha.VolumeRecord(derefString(pin.Comment)); slices.Contains(rec, "nfs2") {
			t.Errorf("the pin recorded nfs2 before the attach ran, comment %v", derefString(pin.Comment))
		}
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(sl.events, []string{"attach"}) {
		t.Fatalf("events %v, want the attach alone", sl.events)
	}
	pin := stub.rules["bosh-na-100"]
	if rec, ok := ha.VolumeRecord(derefString(pin.Comment)); !ok || !slices.Contains(rec, "nfs2") {
		t.Fatalf("the pin must record nfs2 after the attach, comment %v", pin.Comment)
	}
}

// narrowingDisk is a disk on nfs-ab, which only pve01 and pve02 reach.
func narrowingDisk() resolvedDisk {
	return resolvedDisk{volid: "nfs-ab:vm-100-disk-1", stableID: "abcd1234"}
}

func TestAttachHARun_NarrowRunsTheAttachInsideTheBarrier(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	sl := &stagingLog{}
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	if !slices.Equal(h.newPin.NodeNames(), []string{"pve01", "pve02"}) || !h.newPin.Strict {
		t.Fatalf("new pin %v strict %v, want a strict pin to pve01 and pve02", h.newPin.NodeNames(),
			h.newPin.Strict)
	}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "scsi1": "nfs-ab:vm-100-disk-1"}, nil
	}}
	h.deps = deps
	err = h.run(context.Background(), func(context.Context) (string, error) {
		if res.state != "ignored" {
			t.Errorf("the attach runs while the resource is ignored, state %q", res.state)
		}
		pin := stub.rules["bosh-na-100"]
		if pin.Nodes == nil || *pin.Nodes == "pve01,pve02,pve03" {
			t.Errorf("the narrowed pin is written before the attach, nodes %v", pin.Nodes)
		}
		sl.add("attach")
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"request:ignored", "attach", "request:started"}
	if !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
	pin := stub.rules["bosh-na-100"]
	if rec, ok := ha.VolumeRecord(derefString(pin.Comment)); !ok || !slices.Contains(rec, "nfs-ab") {
		t.Fatalf("the barrier must write the pin that records nfs-ab, comment %v", pin.Comment)
	}
	if res.comment != stub.comment {
		t.Errorf("the barrier token must be gone after the barrier, comment %q", res.comment)
	}
}

func TestAttachHARun_NarrowReachFailureFails(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil {
		t.Fatalf("decision: %v", err)
	}
	// After the attach the VM also holds a node-local volume that only pve01
	// reaches, so pve02 of the new pin does not reach every volume.
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "scsi1": testPinStorage + ":vm-100-disk-1"}, nil
	}}
	h.deps = deps
	res := &attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	err = h.run(context.Background(), func(context.Context) (string, error) { return "pve01", nil })
	if err == nil || !strings.Contains(err.Error(), "do not reach") {
		t.Fatalf("err = %v, want the reach failure", err)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// attachRunClient is lrClient with a rule store of its own.
type attachRunClient struct {
	*lrClient
	cluster cluster.Service
}

func (c *attachRunClient) Cluster() cluster.Service { return c.cluster }

func TestAttachUnchangedReadsTheNewDisksStorage(t *testing.T) {
	cases := map[string]struct {
		cfg     map[string]any
		readErr error
		want    bool
		wantErr bool
	}{
		"no volume on the storage": {cfg: map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G"}, want: true},
		"the disk landed": {cfg: map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G",
			"scsi1": "nfs-ab:vm-100-disk-1,size=5G"}},
		"the config is unreadable": {readErr: errors.New("connection reset"), wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			q := &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
				return tc.cfg, tc.readErr
			}}
			h := &attachHA{deps: Deps{PVE: &icPVEClient{qemuSvc: q, nodesSvc: &icNodesService{}}}, vmid: 100,
				disk: narrowingDisk()}
			got, err := h.attachUnchanged(context.Background(), "pve01")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("unchanged %v err %v, want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
	h := &attachHA{vmid: 100, disk: resolvedDisk{volid: "novolid"}}
	if _, err := h.attachUnchanged(context.Background(), "pve01"); err == nil {
		t.Fatal("a disk that names no storage must not read as unchanged")
	}
}
