package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// readdRig is VM 101 running on pve-node1 with its HA resource in the error
// state, registered by aaDirector, with a root disk on nfs and the CPI's
// non-strict pin over the three scene members.
type readdRig struct {
	scene   *HADeleteScene
	deps    Deps
	profile *ha.Profile
}

const readdVMID = 101

func newReaddRig(t *testing.T, mode string) *readdRig {
	t.Helper()
	scene := NewHADeleteScene()
	scene.Tick = 10
	scene.Manage(readdVMID, "pve-node1", "started", "error")
	sid := haResourceSid(readdVMID)
	marker := haIdentity{Owner: aaDirector}.marker()
	scene.Do(func(s *HADeleteScene) {
		s.Power[readdVMID] = "running"
		s.Comments[sid] = marker
		s.Configs = map[int]map[string]any{readdVMID: {
			"tags":   "bosh-cpi;" + haManagedTag,
			"scsi0":  "nfs:vm-101-disk-0,size=10G",
			"cpu":    "x86-64-v3",
			"digest": "digest-0",
		}}
		s.Rules[naRuleNameFor(readdVMID)] = map[string]any{
			"type": naRuleType, "resources": sid, "nodes": "pve-node1,pve-node2,pve-node3", "strict": 0,
			"comment": ha.WithVolumeRecord(marker, []string{"nfs"}),
		}
	})
	profile := stagingProfile(mode)
	profile.Nodes = nil
	for _, n := range scene.Members {
		profile.Nodes = append(profile.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
	}
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(mode)}
	r := &readdRig{scene: scene, profile: profile}
	r.deps = Deps{
		Config: cfg, RequestDirectorUUID: aaDirector, PVE: newHASceneClient(scene),
		HAProfile: func(context.Context) (*ha.Profile, error) { return r.profile, nil },
		Agent:     &icAgentStub{}, Logger: log.NewNopLogger(),
	}
	return r
}

func (r *readdRig) run(t *testing.T, opts readdOptions) (ha.ReregisterResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return reregisterErroredVM(ctx, r.deps, "test", readdVMID, "pve-node1", log.NewNopLogger(), opts)
}

func (r *readdRig) tags() []string {
	var out []string
	r.scene.Do(func(s *HADeleteScene) {
		raw, _ := s.Configs[readdVMID]["tags"].(string)
		out = parseTagsField(raw)
	})
	return out
}

func (r *readdRig) description() string {
	var out string
	r.scene.Do(func(s *HADeleteScene) { out, _ = s.Configs[readdVMID]["description"].(string) })
	return out
}

func (r *readdRig) rule(name string) map[string]any {
	var out map[string]any
	r.scene.Do(func(s *HADeleteScene) {
		if row, ok := s.Rules[name]; ok {
			out = make(map[string]any, len(row))
			for k, v := range row {
				out[k] = v
			}
		}
	})
	return out
}

func (r *readdRig) requested() (string, bool) {
	var state string
	var ok bool
	r.scene.Do(func(s *HADeleteScene) { state, ok = s.Requested[haResourceSid(readdVMID)] })
	return state, ok
}

// requireNeverDisabled fails when any event asked an HA resource for disabled.
func requireNeverDisabled(t *testing.T, events []string) {
	t.Helper()
	for _, e := range events {
		if strings.HasSuffix(e, "=disabled") {
			t.Fatalf("an HA resource was asked for disabled, which would stop the guest: %v", events)
		}
	}
}

// requireRegisteredAgain checks the end state of a finished order: the
// resource is started, the tag and the record are gone, the VM carries the
// bosh-ha-managed tag, and its pin records its volumes.
func (r *readdRig) requireRegisteredAgain(t *testing.T) {
	t.Helper()
	if state, ok := r.requested(); !ok || state != "started" {
		t.Fatalf("resource state %q present %v, want started", state, ok)
	}
	tags := r.tags()
	if _, _, tagged := ha.ReaddTagOf(tags); tagged {
		t.Fatalf("tags %v still carry the re-registration tag", tags)
	}
	if !slices.Contains(tags, haManagedTag) {
		t.Fatalf("tags %v, want %s", tags, haManagedTag)
	}
	if rec, err := ha.ReaddRecordOf(r.description()); err != nil || rec != nil {
		t.Fatalf("record %+v err %v, want none", rec, err)
	}
	pin := r.rule(naRuleNameFor(readdVMID))
	if pin == nil {
		t.Fatal("the pin is missing")
	}
	comment, _ := pin["comment"].(string)
	if rec, ok := ha.VolumeRecord(comment); !ok || !slices.Contains(rec, "nfs") {
		t.Fatalf("pin comment %q, want a volume record naming nfs", comment)
	}
	if d, _ := pin["disable"].(int); d != 0 {
		t.Fatalf("pin %v is disabled", pin)
	}
}

func TestReregisterErroredVM_FullOrderNeverDisables(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	res, err := r.run(t, readdOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != ha.ReaddRegistered || res.Node != "pve-node1" {
		t.Fatalf("result %+v, want registered on pve-node1", res)
	}
	events := r.scene.EventsSnapshot()
	requireNeverDisabled(t, events)
	r.requireRegisteredAgain(t)
	order := []string{"UpdateQemuConfig:101", "DeleteHaResources:vm:101", "CreateHaResources:vm:101=ignored",
		"UpdateHaResources:vm:101=started"}
	last := -1
	for _, want := range order {
		i := slices.Index(events, want)
		if i < 0 || i < last {
			t.Fatalf("events %v, want %v in order", events, order)
		}
		last = i
	}
}

// TestReregisterErroredVM_RuleAnswers covers each answer PVE may give about
// the rules that name a removed resource.
func TestReregisterErroredVM_RuleAnswers(t *testing.T) {
	t.Run("rules pruned", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		if _, err := r.run(t, readdOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.requireRegisteredAgain(t)
		if !slices.Contains(r.scene.EventsSnapshot(), "CreateHaRules:bosh-na-101=vm:101") {
			t.Fatalf("events %v, want the pin written again", r.scene.EventsSnapshot())
		}
	})
	t.Run("rules still name the removed resource", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.scene.Do(func(s *HADeleteScene) { s.PurgeKeepsRules = true })
		if _, err := r.run(t, readdOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.requireRegisteredAgain(t)
	})
	t.Run("a rule PVE disabled while the resource was gone is enabled again", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.scene.Do(func(s *HADeleteScene) { s.DisableOrphanRules = true })
		if _, err := r.run(t, readdOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.requireRegisteredAgain(t)
	})
	t.Run("a refused removal leaves the CPI's rules first", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.scene.Do(func(s *HADeleteScene) { s.RefuseRemoval = true })
		if _, err := r.run(t, readdOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		events := r.scene.EventsSnapshot()
		leave := slices.Index(events, "DeleteHaRules:bosh-na-101")
		removed := -1
		for i, e := range events {
			if e == "DeleteHaResources:vm:101" {
				removed = i
			}
		}
		if leave < 0 || removed < leave {
			t.Fatalf("events %v, want the pin deleted before the removal that lands", events)
		}
		r.requireRegisteredAgain(t)
	})
}

// TestReregisterErroredVM_AntiAffinityMembership re-adds the VM to its group's
// rule and enables the rule PVE disabled while the resource was gone.
func TestReregisterErroredVM_AntiAffinityMembership(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.deps.Config.Placement = &config.PlacementConfig{AntiAffinity: &config.AntiAffinityConfig{Enabled: new(true)}}
	ident := haIdentity{Owner: aaDirector, Deployment: "dep", Group: "web"}
	sid := haResourceSid(readdVMID)
	r.scene.Do(func(s *HADeleteScene) {
		s.DisableOrphanRules = true
		s.Comments[sid] = ident.marker()
		s.Rules[ident.ruleName()] = map[string]any{
			"type": haRuleType, "affinity": "negative", "resources": sid + ",vm:102",
			"comment": ident.marker(),
		}
		groupTags := "bosh-cpi;" + ident.ruleName()
		s.Configs[readdVMID]["tags"] = groupTags + ";" + haManagedTag
		s.IndexNode[102] = "pve-node2"
		s.Configs[102] = map[string]any{"tags": groupTags + ";" + haManagedTag}
	})
	if _, err := r.run(t, readdOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.requireRegisteredAgain(t)
	rule := r.rule(ident.ruleName())
	if rule == nil {
		t.Fatal("the group's anti-affinity rule is gone")
	}
	if d, _ := rule["disable"].(int); d != 0 {
		t.Fatalf("rule %v stays disabled", rule)
	}
	if res, _ := rule["resources"].(string); !strings.Contains(res, sid) {
		t.Fatalf("rule resources %q, want %s", res, sid)
	}
}

// TestReregisterErroredVM_CrashAfterRemovalIsFinished models a crash right
// after the removal. The VM runs with no resource and carries the tag, and the
// next call's hook finishes the order.
func TestReregisterErroredVM_CrashAfterRemovalIsFinished(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	crash := errors.New("crash")
	_, err := r.run(t, readdOptions{between: func(context.Context, string) (pve.VMLocation, error) {
		return pve.VMLocation{}, crash
	}})
	if !errors.Is(err, crash) {
		t.Fatalf("err = %v, want the crash", err)
	}
	if _, ok := r.requested(); ok {
		t.Fatal("the resource must be gone after the crash")
	}
	tags := r.tags()
	if _, _, tagged := ha.ReaddTagOf(tags); !tagged {
		t.Fatalf("tags %v, want the re-registration tag kept", tags)
	}
	loc := pve.VMLocation{Node: "pve-node1", Tags: strings.Join(tags, ";"), Found: true}
	if err := finishPendingHAReadd(context.Background(), r.deps, "next", readdVMID, loc, log.NewNopLogger()); err != nil {
		t.Fatalf("the next call's hook: %v", err)
	}
	r.requireRegisteredAgain(t)
	requireNeverDisabled(t, r.scene.EventsSnapshot())
}

func TestReregisterErroredVM_PreconditionsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(r *readdRig)
		want string
	}{
		"no Sys.Console":   {func(r *readdRig) { r.profile.SysConsole = false }, "Sys.Console"},
		"no director_uuid": {func(r *readdRig) { r.deps.RequestDirectorUUID = "" }, "director_uuid"},
		"a dead node":      {func(r *readdRig) { r.scene.MarkNodeDead("pve-node1") }, ""},
		"a stale LRM": {func(r *readdRig) {
			r.scene.Do(func(s *HADeleteScene) { s.LRMAge["pve-node1"] = 3600 })
		}, ""},
		// The VM's own strict pin no longer holds its node, so it was moved
		// out of band, and the order defers and names both ways out.
		"outside its strict pin": {func(r *readdRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.Rules[naRuleNameFor(readdVMID)]["nodes"] = "pve-node2,pve-node3"
				s.Rules[naRuleNameFor(readdVMID)]["strict"] = 1
			})
		}, "widen the pin"},
		"a config lock": {func(r *readdRig) {
			r.scene.Do(func(s *HADeleteScene) { s.Configs[readdVMID]["lock"] = "migrate" })
		}, "config lock"},
		"a disk move task": {func(r *readdRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.MoveTasks = map[string][]pve.ActiveTask{"pve-node1": {{Type: "qmmove", ID: "101"}}}
			})
		}, "disk move task"},
		"an unreadable task list": {func(r *readdRig) {
			r.scene.Do(func(s *HADeleteScene) { s.MoveTasksErr = errors.New("permission denied - (HTTP403)") })
		}, "could not tell"},
		"a node below the PVE 9 floor": {func(r *readdRig) { r.profile.Nodes[2].Version = "8.4.1" }, "PVE 9 floor"},
		"another holder of the VMID claim": {func(r *readdRig) {
			r.scene.lockPoolService().hold(readdVMID, "set_vm_metadata/101", time.Now().Add(time.Hour))
		}, "set_vm_metadata/101"},
	} {
		t.Run(name, func(t *testing.T) {
			shortenVMIDLockWait(t, 50*time.Millisecond)
			r := newReaddRig(t, config.HAModeAuto)
			tc.edit(r)
			_, err := r.run(t, readdOptions{})
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, want retriable", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
			for _, e := range r.scene.EventsSnapshot() {
				if strings.Contains(e, "HaResources") {
					t.Fatalf("events %v, want no HA resource write", r.scene.EventsSnapshot())
				}
			}
			if state, ok := r.requested(); !ok || state != "started" {
				t.Fatalf("resource %q %v, want it left in place", state, ok)
			}
		})
	}
}

func TestReregisterErroredVM_StopAfterRemoval(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	res, err := r.run(t, readdOptions{stopAfterRemoval: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != ha.ReaddRemoved {
		t.Fatalf("result %+v, want removed", res)
	}
	if _, ok := r.requested(); ok {
		t.Fatal("the resource must be gone")
	}
	tags := r.tags()
	if _, _, tagged := ha.ReaddTagOf(tags); tagged || slices.Contains(tags, haManagedTag) {
		t.Fatalf("tags %v, want neither the re-registration tag nor %s", tags, haManagedTag)
	}
	requireNeverDisabled(t, r.scene.EventsSnapshot())
}

func TestFinishPendingHAReadd_Skips(t *testing.T) {
	ctx := context.Background()
	t.Run("no tag", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		loc := pve.VMLocation{Node: "pve-node1", Tags: "bosh-cpi", Found: true}
		if err := finishPendingHAReadd(ctx, r.deps, "t", readdVMID, loc, log.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if len(r.scene.EventsSnapshot()) != 0 {
			t.Fatalf("events %v, want none", r.scene.EventsSnapshot())
		}
	})
	t.Run("mode off", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeOff)
		loc := pve.VMLocation{Node: "pve-node1", Tags: "bosh-cpi;" + ha.ReaddTag(time.Now()), Found: true}
		if err := finishPendingHAReadd(ctx, r.deps, "t", readdVMID, loc, log.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if len(r.scene.EventsSnapshot()) != 0 {
			t.Fatalf("events %v, want none", r.scene.EventsSnapshot())
		}
	})
	t.Run("a guarded client", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.deps.PVE = &managedAllocationClient{Client: r.deps.PVE}
		loc := pve.VMLocation{Node: "pve-node1", Tags: ha.ReaddTag(time.Now()), Found: true}
		if err := finishPendingHAReadd(ctx, r.deps, "t", readdVMID, loc, log.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if len(r.scene.EventsSnapshot()) != 0 {
			t.Fatalf("events %v, want none", r.scene.EventsSnapshot())
		}
	})
	t.Run("a blocked order lets the call go on", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.profile.SysConsole = false
		loc := pve.VMLocation{Node: "pve-node1", Tags: ha.ReaddTag(time.Now()), Found: true}
		if err := finishPendingHAReadd(ctx, r.deps, "t", readdVMID, loc, log.NewNopLogger()); err != nil {
			t.Fatalf("err = %v, want the call to go on", err)
		}
	})
}

func TestWithReaddTag(t *testing.T) {
	tag := ha.ReaddTag(time.Unix(1700000000, 0))
	if got := withReaddTag([]string{"a", tag}, tag); !slices.Equal(got, []string{"a", tag}) {
		t.Fatalf("got %v, want unchanged", got)
	}
	other := ha.ReaddTag(time.Unix(1600000000, 0))
	if got := withReaddTag([]string{"a", other, tag}, tag); !slices.Equal(got, []string{"a", tag}) {
		t.Fatalf("got %v, want one tag", got)
	}
	if got := withReaddTag([]string{"a"}, tag); !slices.Equal(got, []string{"a", tag}) {
		t.Fatalf("got %v, want the tag added", got)
	}
}

func TestSoleAZOfNode(t *testing.T) {
	cfg := icMinConfig()
	if got := soleAZOfNode(cfg, "pve-node1"); got != "" {
		t.Fatalf("no placement: %q", got)
	}
	cfg.Placement = &config.PlacementConfig{AZMap: map[string][]string{
		"z1": {"pve-node1", "pve-node2"}, "z2": {"pve-node3"}, "z3": {"pve-node3"},
	}}
	if got := soleAZOfNode(cfg, "pve-node1"); got != "z1" {
		t.Fatalf("got %q, want z1", got)
	}
	if got := soleAZOfNode(cfg, "pve-node3"); got != "" {
		t.Fatalf("got %q, want none for a node in two AZs", got)
	}
}

// readdPoolsClient is the scene's client with a pool service, so the sweep can
// take its VMID claims.
type readdPoolsClient struct {
	*haSceneClient
	pools pve.PoolService
}

func (c *readdPoolsClient) Pools() pve.PoolService { return c.pools }

// crashAfterRemoval leaves the rig in the state a crash right after the
// removal leaves, with the tag dated at born.
func (r *readdRig) crashAfterRemoval(t *testing.T, born time.Time) {
	t.Helper()
	crash := errors.New("crash")
	if _, err := r.run(t, readdOptions{between: func(context.Context, string) (pve.VMLocation, error) {
		return pve.VMLocation{}, crash
	}}); !errors.Is(err, crash) {
		t.Fatalf("err = %v, want the crash", err)
	}
	r.scene.Do(func(s *HADeleteScene) {
		tags := ha.WithoutReaddTags(parseTagsField(s.Configs[readdVMID]["tags"].(string)))
		s.Configs[readdVMID]["tags"] = strings.Join(append(tags, ha.ReaddTag(born)), ";")
		s.Events = nil
	})
}

func TestSweepDeferredTeardowns_FinishesADueReadd(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.deps.PVE = &readdPoolsClient{haSceneClient: r.deps.PVE.(*haSceneClient), pools: &wbNoopPoolService{}}
	r.crashAfterRemoval(t, time.Now().Add(-24*time.Hour))
	sweepDeferredTeardowns(context.Background(), r.deps, log.NewNopLogger())
	r.requireRegisteredAgain(t)
	events := r.scene.EventsSnapshot()
	requireNeverDisabled(t, events)
	for _, e := range events {
		if strings.HasPrefix(e, "Stop:") || strings.HasPrefix(e, "DeleteQemu") {
			t.Fatalf("events %v, want the VM kept, never torn down", events)
		}
	}
}

func TestSweepDeferredTeardowns_LeavesAYoungReadd(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.deps.PVE = &readdPoolsClient{haSceneClient: r.deps.PVE.(*haSceneClient), pools: &wbNoopPoolService{}}
	r.crashAfterRemoval(t, time.Now())
	sweepDeferredTeardowns(context.Background(), r.deps, log.NewNopLogger())
	if events := r.scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("events %v, want a young tag left alone", events)
	}
	if _, ok := r.requested(); ok {
		t.Fatal("the sweep must not register a VM whose tag is young")
	}
}

func TestSweepDeferredTeardowns_ModeOffFinishesNoReadd(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.deps.PVE = &readdPoolsClient{haSceneClient: r.deps.PVE.(*haSceneClient), pools: &wbNoopPoolService{}}
	r.crashAfterRemoval(t, time.Now().Add(-24*time.Hour))
	r.deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeOff)}
	sweepDeferredTeardowns(context.Background(), r.deps, log.NewNopLogger())
	if events := r.scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("events %v, want nothing in mode off", events)
	}
	if _, _, tagged := ha.ReaddTagOf(r.tags()); !tagged {
		t.Fatal("mode off must keep the tag")
	}
}

func TestReaddQueue(t *testing.T) {
	q := deferredTeardownQueue{now: time.Unix(1700100000, 0), age: time.Hour}
	old := ha.ReaddTag(time.Unix(1700000000, 0))
	young := ha.ReaddTag(time.Unix(1700099000, 0))
	if !q.readdDue("bosh-cpi;"+old) || q.readdDue("bosh-cpi;"+young) || q.readdDue("bosh-cpi") {
		t.Fatal("readdDue misjudged a tag's age")
	}
	if q.queued("bosh-deleting;" + old) {
		t.Fatal("a VM with a re-registration tag must never be queued for teardown")
	}
	row := stragglerRow{Type: resourceTypeQemu, Node: "n1", Tags: old}
	row.VMID = 101
	if !row.readdCandidate(q, aaDirector) {
		t.Fatal("an owned row with a due tag is a candidate")
	}
	row.Tags = old + ";" + pve.ParkerTag
	if row.readdCandidate(q, aaDirector) {
		t.Fatal("a parker is never a candidate")
	}
}

// readdSnapQEMU is the scene's QEMU service with a VM that has no snapshots,
// for the attach's snapshot guard.
type readdSnapQEMU struct{ *HASceneQEMU }

func (readdSnapQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return []map[string]any{{"name": "current"}}, nil
}

type readdAttachClient struct{ *haSceneClient }

func (c readdAttachClient) QEMU() qemu.Service {
	return readdSnapQEMU{HASceneQEMU: c.haSceneClient.QEMU().(*HASceneQEMU)}
}

// readdAttach decides and runs an attach of rd to the errored VM of the rig.
// The attach adds the disk on scsi1 of the scene's config and records when it
// ran.
func (r *readdRig) readdAttach(t *testing.T, rd resolvedDisk) error {
	t.Helper()
	r.deps.PVE = readdAttachClient{haSceneClient: r.deps.PVE.(*haSceneClient)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := decideAttachHA(ctx, r.deps, "101", rd)
	if err != nil {
		return err
	}
	if h == nil {
		t.Fatal("the attach decision took no HA action")
	}
	return h.run(ctx, func(context.Context) (string, error) {
		r.scene.Do(func(s *HADeleteScene) {
			s.Configs[readdVMID]["scsi1"] = rd.volid + ",size=1G"
			s.Events = append(s.Events, "attach")
		})
		return "pve-node1", nil
	})
}

func TestAttachHA_NarrowingAttachOnAnErroredVMReregisters(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.profile.Storages = append(r.profile.Storages, pve.StorageInfo{Name: "nfs-ab", Type: pve.StorageTypeNFS,
		Nodes: []string{"pve-node1", "pve-node2"}, Content: "images"})
	if err := r.readdAttach(t, resolvedDisk{volid: "nfs-ab:vm-101-disk-1", stableID: "abcd1234"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := r.scene.EventsSnapshot()
	requireNeverDisabled(t, events)
	removed := slices.Index(events, "DeleteHaResources:vm:101")
	attached := slices.Index(events, "attach")
	staged := slices.Index(events, "CreateHaResources:vm:101=ignored")
	if removed < 0 || attached < removed || staged < attached {
		t.Fatalf("events %v, want the removal, the attach, and then the staging", events)
	}
	r.requireRegisteredAgain(t)
	pin := r.rule(naRuleNameFor(readdVMID))
	nodes, _ := pin["nodes"].(string)
	if !strings.Contains(nodes, "pve-node1") || !strings.Contains(nodes, "pve-node2") ||
		strings.Contains(nodes, "pve-node3") {
		t.Fatalf("pin nodes %q, want the pin narrowed to pve-node1 and pve-node2", nodes)
	}
	if rec, ok := ha.VolumeRecord(pin["comment"].(string)); !ok || !slices.Contains(rec, "nfs-ab") {
		t.Fatalf("pin record %v, want nfs-ab", rec)
	}
}

func TestAttachHA_LocalDiskUnderNoneOnAnErroredVMStopsAfterTheRemoval(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	if err := r.readdAttach(t, resolvedDisk{volid: testPinStorage + ":vm-101-disk-1", stableID: "abcd1234"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	events := r.scene.EventsSnapshot()
	requireNeverDisabled(t, events)
	if slices.Contains(events, "CreateHaResources:vm:101=ignored") {
		t.Fatalf("events %v, want no registration under none", events)
	}
	if !slices.Contains(events, "attach") {
		t.Fatalf("events %v, want the attach to run", events)
	}
	if _, ok := r.requested(); ok {
		t.Fatal("the resource must be gone")
	}
	tags := r.tags()
	if _, _, tagged := ha.ReaddTagOf(tags); tagged || slices.Contains(tags, haManagedTag) {
		t.Fatalf("tags %v, want neither the re-registration tag nor %s", tags, haManagedTag)
	}
	if r.rule(naRuleNameFor(readdVMID)) != nil {
		t.Fatal("the pin must be gone")
	}
}

func TestAttachHA_GuardedClientDefersAnErroredVM(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	h := &attachHA{deps: r.deps, vmid: readdVMID, node: "pve-node1", logger: log.NewNopLogger(),
		narrow: ha.NarrowDepsFor(r.deps.PVE, log.NewNopLogger(), time.Minute)}
	h.deps.PVE = &managedAllocationClient{Client: r.deps.PVE}
	st := &pve.HAServiceState{Known: true, Raw: "error", Node: "pve-node1"}
	_, err := h.reregister(context.Background(), st, nil)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "ha-manager remove") {
		t.Fatalf("err = %v, want a retriable error naming the commands", err)
	}
	h.narrow = ha.NarrowDepsFor(r.deps.PVE, log.NewNopLogger(), time.Minute)
	if _, _, err := h.leaveErrored(context.Background()); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("leave: err = %v, want a retriable deferral", err)
	}
	if len(r.scene.EventsSnapshot()) != 0 {
		t.Fatalf("events %v, want nothing written", r.scene.EventsSnapshot())
	}
}

// TestFinishPendingHAReaddForCID covers the hook that attach_disk and
// detach_disk run before they touch the VM. A crash after the removal is
// finished, and a CID that does not parse, a VM without the tag, and mode off
// write nothing.
func TestFinishPendingHAReaddForCID(t *testing.T) {
	ctx := context.Background()
	t.Run("a crash after the removal is finished", func(t *testing.T) {
		r := newReaddRig(t, config.HAModeAuto)
		r.crashAfterRemoval(t, time.Now())
		if err := finishPendingHAReaddForCID(ctx, r.deps, "attach_disk", "101"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.requireRegisteredAgain(t)
		requireNeverDisabled(t, r.scene.EventsSnapshot())
	})
	for _, tc := range []struct {
		name, mode, cid string
	}{
		{"a CID that does not parse", config.HAModeAuto, "not-a-cid"},
		{"no tag", config.HAModeAuto, "101"},
		{"mode off", config.HAModeOff, "101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReaddRig(t, tc.mode)
			if err := finishPendingHAReaddForCID(ctx, r.deps, "attach_disk", tc.cid); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, e := range r.scene.EventsSnapshot() {
				if !strings.HasPrefix(e, "List") && !strings.HasPrefix(e, "Get") {
					t.Fatalf("events %v, want no write", r.scene.EventsSnapshot())
				}
			}
		})
	}
}

// gatedLockPools holds the first acquire of one VMID claim open. The create
// that takes the claim closes entered and waits for release before it
// returns, so a test can run a second caller while the first holds the claim.
type gatedLockPools struct {
	*exclusiveLockPools
	pool    string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newGatedLockPools(vmid int) *gatedLockPools {
	return &gatedLockPools{exclusiveLockPools: newExclusiveLockPools(), pool: vmidLockPool(vmid),
		entered: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedLockPools) CreatePool(ctx context.Context, poolID, comment string) error {
	if err := p.exclusiveLockPools.CreatePool(ctx, poolID, comment); err != nil {
		return err
	}
	if poolID == p.pool {
		p.once.Do(func() {
			close(p.entered)
			<-p.release
		})
	}
	return nil
}

// createAttempts counts the creates every caller tried, taken or refused.
func (p *gatedLockPools) createAttempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

func (p *gatedLockPools) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first caller never took the VMID claim")
	}
}

// requireStagedOnce fails unless the events stage the VM's HA resource exactly
// once.
func requireStagedOnce(t *testing.T, events []string) {
	t.Helper()
	staged := 0
	for _, e := range events {
		if e == "CreateHaResources:vm:101=ignored" {
			staged++
		}
	}
	if staged != 1 {
		t.Fatalf("events %v, want the HA resource staged exactly once", events)
	}
}

// requireNoHAWrite fails when any event touched an HA resource.
func requireNoHAWrite(t *testing.T, events []string) {
	t.Helper()
	for _, e := range events {
		if strings.Contains(e, "HaResources") || strings.HasPrefix(e, "Update") || strings.HasPrefix(e, "Create") {
			t.Fatalf("events %v, want nothing written", events)
		}
	}
}

// TestReaddRace_HandlerClaimMakesTheSweepSkip races a handler's hook and the
// sweep on the same due re-registration. The hook takes the VMID claim first,
// so the sweep skips the VM and writes nothing, and the hook finishes the
// order alone.
func TestReaddRace_HandlerClaimMakesTheSweepSkip(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.crashAfterRemoval(t, time.Now().Add(-24*time.Hour))
	pools := newGatedLockPools(readdVMID)
	r.deps.PVE = &readdPoolsClient{haSceneClient: r.deps.PVE.(*haSceneClient), pools: pools}
	loc := pve.VMLocation{Node: "pve-node1", Tags: strings.Join(r.tags(), ";"), Found: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- finishPendingHAReadd(ctx, r.deps, "set_vm_metadata", readdVMID, loc, log.NewNopLogger())
	}()
	pools.waitEntered(t)
	// The sweep sizes its budget from its own context, which a request
	// deadline this short would leave no time to claim in.
	sweepDeferredTeardowns(context.Background(), r.deps, log.NewNopLogger())
	requireNoHAWrite(t, r.scene.EventsSnapshot())
	if _, ok := r.requested(); ok {
		t.Fatal("the sweep registered the VM while the hook held its claim")
	}
	if n := pools.createAttempts(); n < 2 {
		t.Fatalf("%d claim attempts, want the sweep to have tried the claim the hook holds", n)
	}
	close(pools.release)
	if err := <-done; err != nil {
		t.Fatalf("the hook: %v", err)
	}
	r.requireRegisteredAgain(t)
	events := r.scene.EventsSnapshot()
	requireStagedOnce(t, events)
	requireNeverDisabled(t, events)
}

// TestReaddRace_SweepClaimMakesTheHandlerDefer is the other order: the sweep
// takes the VMID claim first, so the hook defers with a retriable error and
// writes nothing, and the sweep finishes the order alone.
func TestReaddRace_SweepClaimMakesTheHandlerDefer(t *testing.T) {
	shortenVMIDLockWait(t, 50*time.Millisecond)
	r := newReaddRig(t, config.HAModeAuto)
	r.crashAfterRemoval(t, time.Now().Add(-24*time.Hour))
	pools := newGatedLockPools(readdVMID)
	r.deps.PVE = &readdPoolsClient{haSceneClient: r.deps.PVE.(*haSceneClient), pools: pools}
	loc := pve.VMLocation{Node: "pve-node1", Tags: strings.Join(r.tags(), ";"), Found: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		sweepDeferredTeardowns(context.Background(), r.deps, log.NewNopLogger())
	}()
	pools.waitEntered(t)
	err := finishPendingHAReadd(ctx, r.deps, "set_vm_metadata", readdVMID, loc, log.NewNopLogger())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable deferral while the sweep holds the claim", err)
	}
	requireNoHAWrite(t, r.scene.EventsSnapshot())
	close(pools.release)
	<-swept
	r.requireRegisteredAgain(t)
	events := r.scene.EventsSnapshot()
	requireStagedOnce(t, events)
	requireNeverDisabled(t, events)
}

// editReaddRecord rewrites the re-registration record a crash after the
// removal left in the VM's description.
func (r *readdRig) editReaddRecord(t *testing.T, edit func(*ha.ReaddRecord)) {
	t.Helper()
	rec, err := ha.ReaddRecordOf(r.description())
	if err != nil || rec == nil {
		t.Fatalf("record %+v err %v, want the record a crash leaves", rec, err)
	}
	edit(rec)
	desc, err := ha.WithReaddRecord(r.description(), rec)
	if err != nil {
		t.Fatal(err)
	}
	r.scene.Do(func(s *HADeleteScene) { s.Configs[readdVMID]["description"] = desc })
}

// TestReregisterErroredVM_ResumeOutsideTheRecordedStrictPinDefers resumes an
// order whose record keeps a strict pin the VM's node lies outside. The order
// defers and keeps its tag, as the attach does for a VM under HA, rather than
// leave the VM without HA.
func TestReregisterErroredVM_ResumeOutsideTheRecordedStrictPinDefers(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.crashAfterRemoval(t, time.Now())
	r.editReaddRecord(t, func(rec *ha.ReaddRecord) { rec.PinNodes, rec.PinStrict = []string{"pve-node2"}, true })
	_, err := r.run(t, readdOptions{})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "widen the pin") {
		t.Fatalf("err = %v, want a retriable deferral naming the pin", err)
	}
	if _, _, tagged := ha.ReaddTagOf(r.tags()); !tagged {
		t.Fatalf("tags %v, want the re-registration tag kept", r.tags())
	}
	requireNoHAWrite(t, r.scene.EventsSnapshot())
}

// TestReregisterErroredVM_ResumeKeepsHAWithoutTheDynamicLoadBalancer resumes
// an order for a VM in the DLB AZ on a cluster where a node runs below PVE
// 9.2. The VM ran under HA, so it registers again with the dynamic load
// balancer off instead of leaving HA.
func TestReregisterErroredVM_ResumeKeepsHAWithoutTheDynamicLoadBalancer(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.crashAfterRemoval(t, time.Now())
	r.editReaddRecord(t, func(rec *ha.ReaddRecord) { rec.AutoRebalance = true })
	r.profile.DLBSupported, r.profile.MinVersion = false, "9.1.2"
	if _, err := r.run(t, readdOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.requireRegisteredAgain(t)
	requireNeverDisabled(t, r.scene.EventsSnapshot())
}

func TestReaddDecisionInput_MatchesTheAttach(t *testing.T) {
	cfg := icMinConfig()
	vmCfg := map[string]any{"scsi0": "nfs:vm-101-disk-0,size=10G", "cpu": "x86-64-v3"}
	in := readdDecisionInput(cfg, readdVMID, "pve-node1", vmCfg, nil,
		ha.ReaddRecord{Comment: "c", PinNodes: []string{"pve-node1", "pve-node2"}, PinStrict: true})
	if !in.HAManaged {
		t.Fatal("a re-registration decides for a VM under HA")
	}
	if len(in.Constraints) != 1 || !in.Constraints[0].Pin ||
		!slices.Equal(in.Constraints[0].Nodes, []string{"pve-node1", "pve-node2"}) {
		t.Fatalf("constraints %+v, want the recorded strict pin marked as the VM's pin", in.Constraints)
	}
	in = readdDecisionInput(cfg, readdVMID, "pve-node1", vmCfg, nil,
		ha.ReaddRecord{Comment: "c", PinNodes: []string{"pve-node1"}})
	if len(in.Constraints) != 0 {
		t.Fatalf("constraints %+v, want none for a non-strict pin", in.Constraints)
	}
}

// TestAttachHA_ErroredVMDecisionChangedUnderTheClaimDefers changes the VM's
// disks between the attach's decision and its run, so the decision under the
// VMID claim no longer registers. The attach defers and writes nothing.
func TestAttachHA_ErroredVMDecisionChangedUnderTheClaimDefers(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	r.profile.Storages = append(r.profile.Storages, pve.StorageInfo{Name: "nfs-ab", Type: pve.StorageTypeNFS,
		Nodes: []string{"pve-node1", "pve-node2"}, Content: "images"})
	r.deps.PVE = readdAttachClient{haSceneClient: r.deps.PVE.(*haSceneClient)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := decideAttachHA(ctx, r.deps, "101", resolvedDisk{volid: "nfs-ab:vm-101-disk-1", stableID: "abcd1234"})
	if err != nil || h == nil {
		t.Fatalf("h %v err %v, want an HA action", h, err)
	}
	r.scene.Do(func(s *HADeleteScene) { s.Configs[readdVMID]["scsi2"] = testPinStorage + ":vm-101-disk-2,size=1G" })
	err = h.run(ctx, func(context.Context) (string, error) {
		t.Fatal("the attach ran on a decision that changed")
		return "", nil
	})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("err = %v, want a retriable deferral", err)
	}
	requireNoHAWrite(t, r.scene.EventsSnapshot())
	if state, ok := r.requested(); !ok || state != "started" {
		t.Fatalf("resource %q %v, want it left in place", state, ok)
	}
}

// TestDecideAttachHA_ManagedTransientRefusalDefers checks that a refusal for
// a reason that may pass defers the attach in managed mode instead of failing
// it for good.
func TestDecideAttachHA_ManagedTransientRefusalDefers(t *testing.T) {
	r := newReaddRig(t, config.HAModeManaged)
	r.profile.Storages = append(r.profile.Storages, pve.StorageInfo{Name: "nfs-ab", Type: pve.StorageTypeNFS,
		Nodes: []string{"pve-node1", "pve-node2"}, Content: "images"})
	r.profile.Nodes[2].Version = "8.4.1"
	r.deps.PVE = readdAttachClient{haSceneClient: r.deps.PVE.(*haSceneClient)}
	_, err := decideAttachHA(context.Background(), r.deps, "101",
		resolvedDisk{volid: "nfs-ab:vm-101-disk-1", stableID: "abcd1234"})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable deferral", err)
	}
	requireNoHAWrite(t, r.scene.EventsSnapshot())
}

// TestReregisterErroredVM_DecisionWithoutVolumesWritesNothing gives the order
// a registering decision that names no volume. The pin could not record any,
// so the order fails before the removal and writes nothing.
func TestReregisterErroredVM_DecisionWithoutVolumesWritesNothing(t *testing.T) {
	r := newReaddRig(t, config.HAModeAuto)
	decided := false
	_, err := r.run(t, readdOptions{decide: func(
		_ context.Context, node string, st *pve.HAServiceState, rec ha.ReaddRecord,
	) (*ha.VMDecision, []ha.VMVolume, error) {
		var vmCfg map[string]any
		r.scene.Do(func(s *HADeleteScene) { vmCfg = s.Configs[readdVMID] })
		d, err := r.profile.DecideVM(readdDecisionInput(r.deps.Config, readdVMID, node, vmCfg, st, rec))
		decided = err == nil && d.Registers()
		return d, nil, err
	}})
	if !decided {
		t.Fatal("the decision must register, so the missing volumes are what refuses the order")
	}
	if err == nil || !strings.Contains(err.Error(), "names no volume") {
		t.Fatalf("err = %v, want the refusal of a decision without volumes", err)
	}
	requireNoHAWrite(t, r.scene.EventsSnapshot())
	if state, ok := r.requested(); !ok || state != "started" {
		t.Fatalf("resource %q %v, want it left in place", state, ok)
	}
}
