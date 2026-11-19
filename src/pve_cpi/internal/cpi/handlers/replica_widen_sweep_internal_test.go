package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// sweepCluster is the cluster of the replica widening sweep's tests: the
// replicate rig's HA stub, with a guest listing and an HA manager status that
// the tests set, and hooks that run before a read so a test can change the
// cluster between the sweep's reads.
type sweepCluster struct {
	*attachHAStub
	rows      []map[string]any
	listErr   error
	listCalls int
	// services is the HA manager's state of each service.
	services map[string]string
	// onResource runs before each HA resource read, with the call's number.
	onResource    func(call int)
	resourceCalls int
}

func (s *sweepCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse,
	error,
) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := cluster.ListResourcesResponse{}
	for _, row := range s.rows {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

func (s *sweepCluster) ListHaStatusManagerStatus(context.Context) (*cluster.ListHaStatusManagerStatusResponse,
	error,
) {
	services := map[string]any{}
	for sid, state := range s.services {
		services[sid] = map[string]any{"state": state, "node": "pve01"}
	}
	raw, _ := json.Marshal(map[string]any{"manager_status": map[string]any{"service_status": services}})
	resp := cluster.ListHaStatusManagerStatusResponse(raw)
	return &resp, nil
}

func (s *sweepCluster) GetHaResources(ctx context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	s.resourceCalls++
	if s.onResource != nil {
		s.onResource(s.resourceCalls)
	}
	return s.attachHAStub.GetHaResources(ctx, sid)
}

// sweepPoolsClient is the rig's client with a pool service, so the sweep's
// VMID claims are real.
type sweepPoolsClient struct {
	*attachRunClient
	pools *exclusiveLockPools
}

func (c *sweepPoolsClient) Pools() pve.PoolService { return c.pools }

// statusHookReplica is the rig's replication fake with a hook that runs
// before each status read.
type statusHookReplica struct {
	*fakeReplica
	onStatus func(call int)
	calls    int
}

func (f *statusHookReplica) Status(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	f.calls++
	if f.onStatus != nil {
		f.onStatus(f.calls)
	}
	return f.fakeReplica.Status(ctx, node, id)
}

// sweepRig is the replicate rig with VM 100 listed on pve01 with the
// replicate tag and this Director's owner tag, a source-only pin whose last
// change was two hours ago, this Director's job to pve02, a sync that covers
// the root volume, and an HA manager that runs the VM.
type sweepRig struct {
	*replicaRig
	cluster *sweepCluster
	pools   *exclusiveLockPools
	hooked  *statusHookReplica
	changed time.Time
}

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	r := newReplicaRig(config.HAModeAuto)
	s := &sweepRig{replicaRig: r, pools: newExclusiveLockPools()}
	s.tags = "bosh-cpi;" + haReplicaTag + ";" + OwnerTagFor(aaDirector)
	s.changed = time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	s.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, s.changed) })
	s.ownJob(1)
	s.repl.status = verifiedSync(s.changed)
	s.repl.listed[replicaRootVolume] = true
	s.cluster = &sweepCluster{attachHAStub: r.stub, services: map[string]string{"vm:100": "started"}}
	s.cluster.rows = []map[string]any{sweepRow(100, "pve01", s.tags)}
	run := r.deps.PVE.(*attachRunClient)
	run.cluster = s.cluster
	r.deps.PVE = &sweepPoolsClient{attachRunClient: run, pools: s.pools}
	s.hooked = &statusHookReplica{fakeReplica: r.repl}
	r.deps.Replication = s.hooked
	return s
}

func sweepRow(vmid int, node, tags string) map[string]any {
	return map[string]any{"type": "qemu", "vmid": vmid, "node": node, "tags": tags, "id": "qemu/" + itoa(vmid)}
}

// pass returns a pass over the rig with no caps, the candidates in VMID order,
// and no journal record.
func (s *sweepRig) pass(opts ReplicaWidenOptions) *replicaWidenPass {
	p := newReplicaWidenPass(s.deps, opts)
	p.shuffle = func([]replicaWidenCandidate) {}
	return p
}

func (s *sweepRig) run(t *testing.T, opts ReplicaWidenOptions) ReplicaWidenReport {
	t.Helper()
	report, err := s.pass(opts).run(crhCtx())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	return report
}

func (s *sweepRig) widened() bool {
	p, ok := ha.ReplicaPartner(s.pinComment())
	return s.pinNodes() == "pve01,pve02" && ok && p == "pve02"
}

func onlyResult(t *testing.T, report ReplicaWidenReport) ReplicaWidenResult {
	t.Helper()
	if len(report.Results) != 1 {
		t.Fatalf("results %+v, want one", report.Results)
	}
	return report.Results[0]
}

func TestReplicaWidenPass_WidensOnAVerifiedSync(t *testing.T) {
	s := newSweepRig(t)
	report := s.run(t, ReplicaWidenOptions{})
	res := onlyResult(t, report)
	if res.Outcome != ReplicaWidened || res.Partner != "pve02" || !s.widened() {
		t.Fatalf("result %+v, pin %q %q; want the pin widened to pve02", res, s.pinNodes(), s.pinComment())
	}
	if s.pools.held(100) {
		t.Fatal("the pass left VM 100 claimed")
	}
	if s.pools.creates != 1 {
		t.Fatalf("pool creates %d, want the one VMID claim", s.pools.creates)
	}
}

func TestReplicaWidenPass_SyncInsideTheFiveMinutesDoesNotCount(t *testing.T) {
	cases := map[string]struct {
		lastSync time.Duration
	}{
		"started a minute after the change":   {lastSync: time.Minute},
		"started four minutes after":          {lastSync: 4 * time.Minute},
		"started exactly five minutes after":  {lastSync: aj.ReplicaClockSkewTolerance},
		"started before the change":           {lastSync: -time.Minute},
		"started thirty seconds after change": {lastSync: 30 * time.Second},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSweepRig(t)
			s.repl.status = ha.ReplicaSyncStatus{LastSync: s.changed.Add(tc.lastSync)}
			res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
			if res.Outcome != ReplicaWidenSkipped || s.widened() || s.pinNodes() != "pve01" {
				t.Fatalf("result %+v, pin %q; want the source alone", res, s.pinNodes())
			}
			if !strings.Contains(res.Reason, "not clearly after") {
				t.Fatalf("reason %q, want the sync gate's", res.Reason)
			}
			if s.pools.creates != 0 {
				t.Fatal("the pass claimed a VM whose sync cannot count")
			}
		})
	}
}

func TestReplicaWidenPass_RecentChangeWaitsWithoutAStatusRead(t *testing.T) {
	s := newSweepRig(t)
	recent := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	s.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, recent) })
	s.repl.status = ha.ReplicaSyncStatus{LastSync: time.Now()}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "less than five minutes ago") {
		t.Fatalf("result %+v", res)
	}
	if s.hooked.calls != 0 || s.pools.creates != 0 || s.widened() {
		t.Fatalf("status reads %d, claims %d; want neither for a change under five minutes old", s.hooked.calls,
			s.pools.creates)
	}
}

func TestReplicaWidenPass_OwnershipFilter(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	cases := map[string]struct {
		setup func(s *sweepRig)
		// listed is false when the VM must not appear in the report at all.
		listed bool
		reason string
	}{
		"owner tag of another Director": {
			setup: func(s *sweepRig) {
				s.cluster.rows = []map[string]any{sweepRow(100, "pve01", "bosh-cpi;"+haReplicaTag+";"+OwnerTagFor(other))}
			},
		},
		"no replicate tag": {
			setup: func(s *sweepRig) { s.cluster.rows = []map[string]any{sweepRow(100, "pve01", "bosh-cpi")} },
		},
		"a template": {
			setup: func(s *sweepRig) {
				row := sweepRow(100, "pve01", s.tags)
				row["template"] = 1
				s.cluster.rows = []map[string]any{row}
			},
		},
		"pin of another Director": {
			setup: func(s *sweepRig) {
				comment := ha.WithReplicaChanged(haIdentity{Owner: other}.marker(), s.changed)
				pin := s.stub.rules["bosh-na-100"]
				pin.Comment = &comment
				s.stub.rules["bosh-na-100"] = pin
			},
		},
		"HA resource of another Director": {
			setup:  func(s *sweepRig) { s.stub.comment = haIdentity{Owner: other}.marker() },
			listed: true, reason: "HA resource does not carry this Director's marker",
		},
		"job of another Director": {
			setup: func(s *sweepRig) {
				s.repl.jobs = nil
				s.repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(other, 1))
			},
			listed: true, reason: "no replication job of this Director",
		},
		"config owned by another Director": {
			setup: func(s *sweepRig) {
				// The listing row is stale and still says ours; the config is
				// node-local truth.
				s.tags = "bosh-cpi;" + haReplicaTag + ";" + OwnerTagFor(other)
			},
			listed: true, reason: "not this Director's",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSweepRig(t)
			tc.setup(s)
			report := s.run(t, ReplicaWidenOptions{})
			if s.widened() || s.pinNodes() != "pve01" {
				t.Fatalf("pin %q widened for a VM this Director does not own", s.pinNodes())
			}
			if !tc.listed {
				if len(report.Results) != 0 || s.pools.creates != 0 {
					t.Fatalf("results %+v, claims %d; want the VM left out", report.Results, s.pools.creates)
				}
				return
			}
			res := onlyResult(t, report)
			if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("result %+v, want a skip for %q", res, tc.reason)
			}
		})
	}
}

func TestReplicaWidenPass_NoDirectorUUIDDoesNotRun(t *testing.T) {
	s := newSweepRig(t)
	s.deps.RequestDirectorUUID = ""
	if _, err := s.pass(ReplicaWidenOptions{}).run(crhCtx()); err == nil {
		t.Fatal("a pass with no director UUID ran")
	}
	if s.cluster.listCalls != 0 || s.widened() {
		t.Fatal("a pass with no director UUID read the cluster")
	}
}

func TestReplicaWidenPass_SkipsAVMThatMovesOrRecovers(t *testing.T) {
	for _, state := range []string{"migrate", "relocate", "recovery", "fence", "freeze", "error", "request_stop"} {
		t.Run(state, func(t *testing.T) {
			s := newSweepRig(t)
			s.cluster.services["vm:100"] = state
			res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
			if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, state) || s.widened() {
				t.Fatalf("result %+v, pin %q", res, s.pinNodes())
			}
			if s.pools.creates != 0 {
				t.Fatal("the pass claimed a VM in the middle of a move or a recovery")
			}
		})
	}
	t.Run("PVE lock on the config", func(t *testing.T) {
		s := newSweepRig(t)
		s.cfg["lock"] = "migrate"
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "lock") || s.widened() {
			t.Fatalf("result %+v, pin %q", res, s.pinNodes())
		}
	})
	t.Run("not in the HA manager's status", func(t *testing.T) {
		s := newSweepRig(t)
		delete(s.cluster.services, "vm:100")
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome != ReplicaWidenSkipped || s.widened() {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("away from its pinned node", func(t *testing.T) {
		s := newSweepRig(t)
		s.cluster.rows = []map[string]any{sweepRow(100, "pve02", s.tags)}
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "away from its pinned node") ||
			s.widened() {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestReplicaWidenPass_SkipsAVMTheBarrierHolds(t *testing.T) {
	cases := map[string]func(s *sweepRig){
		"ignored with the barrier's token": func(s *sweepRig) {
			s.stub.state = "ignored"
			s.stub.comment = haIdentity{Owner: aaDirector}.marker() + " bosh-narrow-from=started"
		},
		"ignored by an operator": func(s *sweepRig) { s.stub.state = "ignored" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSweepRig(t)
			setup(s)
			res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
			if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "ignored") || s.widened() {
				t.Fatalf("result %+v, pin %q", res, s.pinNodes())
			}
		})
	}
}

// A narrowing that starts after the check verified the sync and before the
// widened pin is written must win: the guard under the rules lock reads the
// barrier's ignored request, and the pin keeps the source alone.
func TestReplicaWidenPass_RacingNarrowingBarrierWins(t *testing.T) {
	s := newSweepRig(t)
	s.cluster.onResource = func(call int) {
		if call == 2 {
			s.stub.state = "ignored"
			s.stub.comment = haIdentity{Owner: aaDirector}.marker() + " bosh-narrow-from=started"
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "narrowing barrier") || s.widened() {
		t.Fatalf("result %+v, pin %q %q; want the barrier's narrowing kept", res, s.pinNodes(), s.pinComment())
	}
	if s.cluster.resourceCalls != 2 {
		t.Fatalf("HA resource reads %d, want the claim's read and the guard's", s.cluster.resourceCalls)
	}
}

// A narrowing that rewrites the pin with a new change time while the check
// runs leaves the check's evidence stale, so the write refuses and the pin
// the narrowing wrote stays.
func TestReplicaWidenPass_RacingPinNarrowingKeepsTheNarrowedPin(t *testing.T) {
	s := newSweepRig(t)
	restamped := time.Now().Truncate(time.Second)
	s.hooked.onStatus = func(call int) {
		if call == 2 {
			s.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, restamped) })
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome == ReplicaWidened || s.widened() {
		t.Fatalf("result %+v, pin %q; want the narrowed pin kept", res, s.pinNodes())
	}
	if got, ok := ha.ReplicaChanged(s.pinComment()); !ok || !got.Equal(restamped) {
		t.Fatalf("pin comment %q lost the narrowing's change time", s.pinComment())
	}
}

// A change to the VM's volumes between the check and the write, such as an
// attach the barrier ran to the end, is caught by the guard.
func TestReplicaWidenPass_RacingVolumeChangeSkips(t *testing.T) {
	s := newSweepRig(t)
	s.cluster.onResource = func(call int) {
		if call == 2 {
			s.cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "volumes changed") || s.widened() {
		t.Fatalf("result %+v, pin %q", res, s.pinNodes())
	}
}

func TestReplicaWidenPass_RacingJobChangeSkips(t *testing.T) {
	cases := map[string]func(s *sweepRig){
		"disabled":            func(s *sweepRig) { s.repl.jobs[0].Disabled = true },
		"marked for removal":  func(s *sweepRig) { s.repl.jobs[0].RemoveJob = "full" },
		"retargeted":          func(s *sweepRig) { s.repl.jobs[0].Target = "pve03" },
		"replaced by another": func(s *sweepRig) { s.repl.jobs[0].ID = "100-1"; s.repl.jobs[0].JobNum = 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSweepRig(t)
			s.cluster.onResource = func(call int) {
				if call == 2 {
					change(s)
				}
			}
			res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
			if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "replication job") || s.widened() {
				t.Fatalf("result %+v, pin %q", res, s.pinNodes())
			}
		})
	}
}

// A delete that holds the VM's claim keeps the pass away from the VM.
func TestReplicaWidenPass_RacingDeleteHoldsTheClaim(t *testing.T) {
	s := newSweepRig(t)
	s.pools.hold(100, "delete_vm/100", time.Now().Add(time.Hour))
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "another call claims the VM") ||
		s.widened() {
		t.Fatalf("result %+v, pin %q", res, s.pinNodes())
	}
	if s.cluster.resourceCalls != 0 {
		t.Fatal("the pass read the VM's HA resource without its claim")
	}
	if comment, _ := s.pools.comment(100); !strings.Contains(comment, "delete_vm/100") {
		t.Fatalf("the delete's claim changed: %q", comment)
	}
}

// A delete that removed the VM's pin and HA resource after the pass listed it
// leaves nothing for the pass to write, and the pass recreates nothing.
func TestReplicaWidenPass_RacingDeleteRecreatesNothing(t *testing.T) {
	t.Run("pin removed during the check", func(t *testing.T) {
		s := newSweepRig(t)
		s.hooked.onStatus = func(call int) {
			if call == 2 {
				delete(s.stub.rules, "bosh-na-100")
			}
		}
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome == ReplicaWidened {
			t.Fatalf("result %+v", res)
		}
		if _, ok := s.stub.rules["bosh-na-100"]; ok || s.stub.createCalls != 0 {
			t.Fatalf("the pass wrote the deleted VM's pin back (creates %d)", s.stub.createCalls)
		}
	})
	t.Run("HA resource removed before the write", func(t *testing.T) {
		s := newSweepRig(t)
		s.cluster.onResource = func(call int) {
			if call == 2 {
				s.stub.missing = true
			}
		}
		res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
		if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "no HA resource") || s.widened() {
			t.Fatalf("result %+v, pin %q", res, s.pinNodes())
		}
		if s.stub.createCalls != 0 || s.stub.updateCalls != 0 {
			t.Fatal("the pass wrote a rule for a VM whose HA resource is gone")
		}
	})
}

func TestReplicaWidenPass_JournalRecordHoldsTheWidening(t *testing.T) {
	s := newSweepRig(t)
	handle := replicaHandle(t, s.changed)
	if _, err := handle.PrepareReplicaVolumeSet([]string{replicaRootVolume, "local-zfs:vm-100-disk-1"},
		time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	p := s.pass(ReplicaWidenOptions{})
	p.record = func(_ context.Context, vmid int, _ map[string]any, fn func(*aj.Handle) error) error {
		if vmid != 100 {
			t.Fatalf("record for VM %d", vmid)
		}
		return fn(handle)
	}
	report, err := p.run(crhCtx())
	if err != nil {
		t.Fatal(err)
	}
	res := onlyResult(t, report)
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "pending") || s.widened() {
		t.Fatalf("result %+v, pin %q", res, s.pinNodes())
	}
}

func TestReplicaWidenPass_BusyJournalRecordSkips(t *testing.T) {
	s := newSweepRig(t)
	p := s.pass(ReplicaWidenOptions{})
	p.record = func(context.Context, int, map[string]any, func(*aj.Handle) error) error {
		return errVMRecordBusy
	}
	report, err := p.run(crhCtx())
	if err != nil {
		t.Fatal(err)
	}
	res := onlyResult(t, report)
	if res.Outcome != ReplicaWidenSkipped || !strings.Contains(res.Reason, "journal record") || s.widened() {
		t.Fatalf("result %+v", res)
	}
}

func TestReplicaWidenPass_DryRunWritesNothing(t *testing.T) {
	s := newSweepRig(t)
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{DryRun: true}))
	if res.Outcome != ReplicaWouldWiden || res.Partner != "pve02" {
		t.Fatalf("result %+v", res)
	}
	if s.widened() || s.pools.creates != 0 || s.stub.updateCalls != 0 || s.cluster.resourceCalls != 0 {
		t.Fatal("a dry run claimed or wrote")
	}
}

// addPinnedVMs lists more VMs like VM 100, each with a source-only pin, this
// Director's job, and a running HA service.
func (s *sweepRig) addPinnedVMs(vmids ...int) {
	for _, vmid := range vmids {
		name := naRuleNameFor(vmid)
		comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
			[]string{"local-zfs"}), s.changed)
		nodes, strict := "pve01", true
		s.stub.rules[name] = cluster.CreateHaRulesParams{Rule: name, Type: naRuleType,
			Resources: haResourceSid(vmid), Comment: &comment, Nodes: &nodes, Strict: &strict}
		s.repl.addJob(itoa(vmid)+"-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
		s.cluster.services[haResourceSid(vmid)] = "started"
		s.cluster.rows = append(s.cluster.rows, sweepRow(vmid, "pve01", s.tags))
	}
}

func TestReplicaWidenPass_CapsBoundTheWork(t *testing.T) {
	t.Run("claim attempts", func(t *testing.T) {
		s := newSweepRig(t)
		s.addPinnedVMs(101, 102, 103)
		report := s.run(t, ReplicaWidenOptions{MaxAttempts: 2})
		if len(report.Results) != 2 || report.Deferred != 2 || s.pools.creates != 2 {
			t.Fatalf("results %+v, deferred %d, claims %d; want two claimed and two deferred", report.Results,
				report.Deferred, s.pools.creates)
		}
	})
	t.Run("widenings", func(t *testing.T) {
		s := newSweepRig(t)
		s.addPinnedVMs(101, 102)
		report := s.run(t, ReplicaWidenOptions{MaxWidened: 1})
		if report.Count(ReplicaWidened) != 1 || report.Deferred != 2 {
			t.Fatalf("results %+v, deferred %d; want one widening and two deferred", report.Results,
				report.Deferred)
		}
	})
	t.Run("status reads", func(t *testing.T) {
		s := newSweepRig(t)
		s.addPinnedVMs(101, 102, 103)
		report := s.run(t, ReplicaWidenOptions{MaxStatusReads: 1, DryRun: true})
		if report.Deferred != 3 || s.hooked.calls != 1 || len(report.Results) != 1 {
			t.Fatalf("results %+v, deferred %d, status reads %d; want one read and three deferred",
				report.Results, report.Deferred, s.hooked.calls)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		s := newSweepRig(t)
		s.addPinnedVMs(101)
		ctx, cancel := context.WithCancel(crhCtx())
		cancel()
		report, err := s.pass(ReplicaWidenOptions{}).run(ctx)
		if err == nil && (report.Count(ReplicaWidened) != 0 || s.pools.creates != 0) {
			t.Fatalf("results %+v, claims %d; want no work after the context ends", report.Results,
				s.pools.creates)
		}
	})
}

func TestReplicaWidenRecordFor(t *testing.T) {
	record := func(id, cid string, state aj.State) aj.Record {
		return aj.Record{ID: id, Kind: "vm", CID: cid, State: state, Namespace: "director", AgentID: "agent-" + id}
	}
	marked := func(t *testing.T, r aj.Record) map[string]any {
		t.Helper()
		hash := sha256.Sum256([]byte(r.AgentID))
		marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm",
			Namespace: r.Namespace, AllocationID: r.ID, AgentSHA256: hex.EncodeToString(hash[:])})
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"scsi0": replicaRootVolume, "description": marker}
	}
	const liveID, goneID, otherID = "6f1c2a3b-0000-4000-8000-000000000001", "6f1c2a3b-0000-4000-8000-000000000002",
		"6f1c2a3b-0000-4000-8000-000000000003"
	live := record(liveID, "100", aj.State("vm_created"))
	twin := record(liveID, "100", aj.State("vm_created"))
	gone := record(goneID, "100", aj.Deleted)
	other := record(otherID, "101", aj.State("vm_created"))
	plain := map[string]any{"scsi0": replicaRootVolume}
	doubled := marked(t, live)
	doubled["description"] = doubled["description"].(string) + doubled["description"].(string)

	if r, err := replicaWidenRecordFor([]aj.Record{gone, other}, 100, plain); err != nil || r != nil {
		t.Fatalf("no live record and no marker: %v %v; want no record", r, err)
	}
	if r, err := replicaWidenRecordFor([]aj.Record{gone, live, other}, 100, marked(t, live)); err != nil ||
		r == nil || r.ID != liveID {
		t.Fatalf("the marker's record: %v %v", r, err)
	}
	refusals := map[string]struct {
		records []aj.Record
		cfg     map[string]any
	}{
		"a live record the config does not name": {records: []aj.Record{live}, cfg: plain},
		"two live records carry the marker":      {records: []aj.Record{live, twin}, cfg: marked(t, live)},
		"the marker names a deleted record":      {records: []aj.Record{gone}, cfg: marked(t, gone)},
		"a marker that does not parse":           {records: nil, cfg: doubled},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			r, err := replicaWidenRecordFor(tc.records, 100, tc.cfg)
			if r != nil || !errors.Is(err, errReplicaWidenRecordUnmatched) {
				t.Fatalf("record %v, err %v; want the refusal", r, err)
			}
		})
	}
}

// The wrapper runs the sweep after a request that took the HA rules lock, and
// returns the handler's answer as it stands whatever the sweep does.
func TestWithReplicaWidenSweep_RunsAfterACallThatTookTheLock(t *testing.T) {
	s := newSweepRig(t)
	wantErr := errors.New("the call's own failure")
	h := withReplicaWidenSweep(handlerFunc(func(ctx context.Context) (any, error) {
		if err := withHARulesLock(ctx, s.deps, 100, s.deps.Logger, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if s.cluster.listCalls != 0 {
			t.Fatal("the sweep ran inside the call")
		}
		return "answer", wantErr
	}))
	out, err := h.Handle(crhCtx(), nil, jsonrpc.Context{})
	if out != "answer" || !errors.Is(err, wantErr) {
		t.Fatalf("handler answer %v %v changed", out, err)
	}
	if !s.widened() {
		t.Fatalf("pin %q; want the sweep to widen it after the call", s.pinNodes())
	}
}

func TestWithReplicaWidenSweep_SkipsACallThatDidNotTakeTheLock(t *testing.T) {
	s := newSweepRig(t)
	h := withReplicaWidenSweep(handlerFunc(func(context.Context) (any, error) { return "answer", nil }))
	if out, err := h.Handle(crhCtx(), nil, jsonrpc.Context{}); out != "answer" || err != nil {
		t.Fatalf("handler answer %v %v", out, err)
	}
	if s.cluster.listCalls != 0 || s.widened() {
		t.Fatal("the sweep ran after a call that never took the HA rules lock")
	}
}

func TestWithReplicaWidenSweep_FailureNeverFailsTheCall(t *testing.T) {
	s := newSweepRig(t)
	s.cluster.listErr = errors.New("cluster index unavailable")
	h := withReplicaWidenSweep(handlerFunc(func(ctx context.Context) (any, error) {
		return "answer", withHARulesLock(ctx, s.deps, 100, s.deps.Logger, func(context.Context) error { return nil })
	}))
	out, err := h.Handle(crhCtx(), nil, jsonrpc.Context{})
	if out != "answer" || err != nil {
		t.Fatalf("handler answer %v %v; want the call's own success", out, err)
	}
	if s.cluster.listCalls != 1 || s.widened() {
		t.Fatalf("list calls %d; want the sweep to try once and change nothing", s.cluster.listCalls)
	}
}

// The sweep's own rule writes take the rules lock too, and must not start a
// second sweep.
func TestWithReplicaWidenSweep_RunsOncePerCall(t *testing.T) {
	s := newSweepRig(t)
	h := withReplicaWidenSweep(handlerFunc(func(ctx context.Context) (any, error) {
		for range 3 {
			_ = withHARulesLock(ctx, s.deps, 100, s.deps.Logger, func(context.Context) error { return nil })
		}
		return nil, nil
	}))
	if _, err := h.Handle(crhCtx(), nil, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	if s.cluster.listCalls != 1 || !s.widened() {
		t.Fatalf("list calls %d, widened %v; want one sweep that widened", s.cluster.listCalls, s.widened())
	}
}

// handlerFunc adapts a body that reads only the context to a CPI handler.
type handlerFunc func(ctx context.Context) (any, error)

func (f handlerFunc) Handle(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
	return f(ctx)
}
