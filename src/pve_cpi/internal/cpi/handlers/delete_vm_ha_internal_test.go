package handlers

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haDeleteClock is the episode store's settable clock.
type haDeleteClock struct{ t time.Time }

func (c *haDeleteClock) now() time.Time { return c.t }

// haDeleteFixture is a delete's view of an HA scene.
type haDeleteFixture struct {
	scene  *HADeleteScene
	deps   Deps
	clock  *haDeleteClock
	logs   *log.Observer
	logger *log.Logger
	cache  string
}

// newHADeleteFixture builds deps over scene whose pve.ha.motion_wait_sec is
// motionSec, with an episode record in a private temporary directory and a
// logger whose entries the test can read. The motion poll is shortened so a
// wait runs in milliseconds.
func newHADeleteFixture(t *testing.T, scene *HADeleteScene, motionSec int) *haDeleteFixture {
	t.Helper()
	restore := SetHADeleteMotionPollForTest(10 * time.Millisecond)
	t.Cleanup(restore)
	clock := &haDeleteClock{t: time.Unix(1_800_000_000, 0)}
	cache := filepath.Join(t.TempDir(), "cache")
	logger, logs := log.NewObservedLogger(slog.LevelDebug)
	deps := Deps{
		Config:     &config.CPIConfig{HA: &config.HAConfig{MotionWaitSec: &motionSec}},
		PVE:        newHASceneClient(scene),
		Logger:     logger,
		HAEpisodes: &ha.EpisodeStore{Dir: cache, Now: clock.now},
	}
	return &haDeleteFixture{scene: scene, deps: deps, clock: clock, logs: logs, logger: logger, cache: cache}
}

func (f *haDeleteFixture) gate(t *testing.T, node string) (*pve.HAServiceState, error) {
	t.Helper()
	return gateHADelete(t.Context(), f.deps, 101, node, f.logger)
}

func (f *haDeleteFixture) logged(level slog.Level, message string) bool {
	for _, e := range f.logs.All() {
		if e.Level == level && strings.Contains(e.Message, message) {
			return true
		}
	}
	return false
}

// requireRetriable fails unless err is a retriable CPI error that names want.
func requireHADeferral(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a retriable error naming %q, got nil", want)
	}
	if !okToRetryCPIError(err) {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not name %q", err, w)
		}
	}
}

// deadNode marks pve-node2 offline in cluster status and unknown to the CRM,
// the view while the CRM waits to fence it. Its LRM stopped writing long ago.
func deadNode(s *HADeleteScene) {
	s.Offline["pve-node2"] = true
	s.NodeState["pve-node2"] = "unknown"
	s.LRMAge["pve-node2"] = 4 * 3600
}

// TestGateHADeleteStateTable walks the delete's HA state table: which states
// let the delete proceed, and which defer it with a retriable error that says
// what the CPI waits for.
func TestGateHADeleteStateTable(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(s *HADeleteScene)
		node        string
		wantManaged bool
		wantErr     []string
	}{
		{name: "not HA-managed on an online node", setup: func(s *HADeleteScene) { s.Place(101, "pve-node1") }},
		{name: "not HA-managed on a cluster without HA", setup: func(s *HADeleteScene) {
			s.NoCRM = true
			s.Place(101, "pve-node1")
		}},
		{name: "ignored resource", wantManaged: true, setup: func(s *HADeleteScene) {
			s.Manage(101, "pve-node1", "ignored", "")
		}},
		{name: "started", wantManaged: true, setup: func(s *HADeleteScene) {
			s.Manage(101, "pve-node1", "started", "started")
		}},
		{name: "stopped", wantManaged: true, setup: func(s *HADeleteScene) {
			s.Manage(101, "pve-node1", "stopped", "stopped")
		}},
		{name: "error", wantManaged: true, setup: func(s *HADeleteScene) {
			s.Manage(101, "pve-node1", "started", "error")
		}},
		{name: "service in fence", node: "pve-node2", wantErr: []string{"deferred", "fence", "pve-node2"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "fence")
				deadNode(s)
				s.NodeState["pve-node2"] = "fence"
			}},
		{name: "node in fence", node: "pve-node2", wantErr: []string{"deferred", "fenced"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "started")
				s.NodeState["pve-node2"] = "fence"
			}},
		{name: "node not online", node: "pve-node2", wantErr: []string{"deferred", "is not online"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "started")
				deadNode(s)
			}},
		{name: "unmanaged VM on an offline node", node: "pve-node2", wantErr: []string{"gives no proof"},
			setup: func(s *HADeleteScene) {
				s.Place(101, "pve-node2")
				deadNode(s)
			}},
		{name: "recovery on a node the CRM does not report fenced", wantErr: []string{"HA state is unknown",
			"does not report node pve-node1 fenced"}, setup: func(s *HADeleteScene) {
			s.Manage(101, "pve-node1", "started", "recovery")
		}},
		{name: "stale CRM timestamp", wantErr: []string{"HA state is unknown", "CRM timestamp"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node1", "started", "started")
				s.CRMAge = 600
			}},
		{name: "stale LRM timestamp", wantErr: []string{"HA state is unknown", "no usable state"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node1", "started", "started")
				s.LRMAge["pve-node1"] = 3600
			}},
		{name: "the CRM holds the VM on another node", wantErr: []string{"places vm:101 on node pve-node2"},
			setup: func(s *HADeleteScene) {
				s.Manage(101, "pve-node2", "started", "started")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scene := NewHADeleteScene()
			tc.setup(scene)
			f := newHADeleteFixture(t, scene, 600)
			node := tc.node
			if node == "" {
				node = "pve-node1"
			}
			st, err := f.gate(t, node)
			if len(tc.wantErr) > 0 {
				requireHADeferral(t, err, tc.wantErr...)
				if st != nil {
					t.Fatal("a deferred gate must not return a state to act on")
				}
				return
			}
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			if !st.Known || st.Node != node || st.Managed() != tc.wantManaged {
				t.Fatalf("gate state: known=%v node=%s managed=%v, want managed=%v", st.Known, st.Node, st.Managed(),
					tc.wantManaged)
			}
			if len(scene.EventsSnapshot()) != 0 {
				t.Fatalf("the gate must not write anything: %v", scene.EventsSnapshot())
			}
		})
	}
}

// TestGateHADeleteWaitsForMotionToSettle holds the service in each motion
// state for a few reads, then lets it settle; the delete waits and proceeds.
func TestGateHADeleteWaitsForMotionToSettle(t *testing.T) {
	for _, raw := range []string{"request_stop", "request_start", "migrate", "relocate", "freeze"} {
		t.Run(raw, func(t *testing.T) {
			scene := NewHADeleteScene()
			scene.Manage(101, "pve-node1", "started", raw)
			scene.OnRead = func(s *HADeleteScene, reads int) {
				if reads == 5 {
					s.Services["vm:101"] = HASceneService{State: "started", Node: "pve-node1", UID: "uid-101"}
				}
			}
			f := newHADeleteFixture(t, scene, 30)
			st, err := f.gate(t, "pve-node1")
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			if st.Raw != "started" || scene.ManagerReads < 5 {
				t.Fatalf("the gate proceeded on %q after %d reads, want started after the motion", st.Raw,
					scene.ManagerReads)
			}
			if !f.logged(slog.LevelInfo, "waiting for HA motion to settle") {
				t.Fatal("the wait for HA motion was not logged")
			}
		})
	}
}

func TestGateHADeleteMotionBudgetRunsOut(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "freeze")
	f := newHADeleteFixture(t, scene, 1)
	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "did not settle within the 1s motion budget", `raw HA state "freeze"`)
}

func TestGateHADeleteWaitsForTheCRMToProcessANewResource(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "")
	f := newHADeleteFixture(t, scene, 1)
	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "has not processed it yet", "did not settle")
}

func TestGateHADeleteRefusesAVMThatMovedDuringTheWait(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "migrate")
	scene.OnRead = func(s *HADeleteScene, reads int) {
		if reads == 2 {
			s.IndexNode[101] = "pve-node2"
		}
	}
	f := newHADeleteFixture(t, scene, 30)
	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "moved while the delete waited", `now "pve-node2"`)
}

func TestGateHADeleteMotionWaitEndsWithTheContext(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "freeze")
	f := newHADeleteFixture(t, scene, 30)
	ctx, cancel := context.WithCancel(t.Context())
	scene.OnRead = func(_ *HADeleteScene, reads int) {
		if reads == 2 {
			cancel()
		}
	}
	_, err := gateHADelete(ctx, f.deps, 101, "pve-node1", f.logger)
	if err == nil {
		t.Fatal("a cancelled wait must fail")
	}
	if !okToRetryCPIError(err) {
		t.Fatalf("a cancelled wait must be retriable, got %v", err)
	}
}

// recoveryScene is a service in recovery after its node died, which the CRM
// reports unknown while it waits to fence it. The dead node's LRM timestamp is
// hours old, which the recovery read expects.
func recoveryScene() *HADeleteScene {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node2", "started", "recovery")
	deadNode(scene)
	return scene
}

func TestGateHADeleteRecoveryGraceWindow(t *testing.T) {
	f := newHADeleteFixture(t, recoveryScene(), 600)

	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "still trying to recover it", "10m0s of the 10m0s grace window remain")

	f.clock.t = f.clock.t.Add(4 * time.Minute)
	_, err = f.gate(t, "pve-node2")
	requireHADeferral(t, err, "6m0s of the 10m0s grace window remain")

	f.clock.t = f.clock.t.Add(7 * time.Minute)
	_, err = f.gate(t, "pve-node2")
	requireHADeferral(t, err, "grace window expired 1m0s ago", "Confirm that node pve-node2 is powered off",
		"mv /etc/pve/nodes/pve-node2/qemu-server/101.conf /etc/pve/nodes/<live node>/qemu-server/")
	if !f.logged(slog.LevelWarn, "grace window expired") {
		t.Fatal("the grace window's expiry was not logged")
	}
}

// TestGateHADeleteRecoveryAfterALongFenceStartsAFreshWindow fences the node
// for hours before the CRM moves the service to recovery. The fence is not
// part of the episode, so the grace window starts at the first recovery read.
func TestGateHADeleteRecoveryAfterALongFenceStartsAFreshWindow(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node2", "started", "fence")
	deadNode(scene)
	scene.NodeState["pve-node2"] = "fence"
	f := newHADeleteFixture(t, scene, 600)
	for range 3 {
		_, err := f.gate(t, "pve-node2")
		requireHADeferral(t, err, "fence")
		f.clock.t = f.clock.t.Add(2 * time.Hour)
	}
	scene.Do(func(s *HADeleteScene) {
		s.Services["vm:101"] = HASceneService{State: "recovery", Node: "pve-node2", UID: "uid-101"}
		s.NodeState["pve-node2"] = "unknown"
	})
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "10m0s of the 10m0s grace window remain")
}

func TestGateHADeleteRecoveryNewUIDRestartsTheWindow(t *testing.T) {
	scene := recoveryScene()
	f := newHADeleteFixture(t, scene, 600)
	if _, err := f.gate(t, "pve-node2"); err == nil {
		t.Fatal("recovery must defer")
	}
	f.clock.t = f.clock.t.Add(5 * time.Minute)
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "5m0s of the 10m0s grace window remain")

	scene.Do(func(s *HADeleteScene) {
		s.Services["vm:101"] = HASceneService{State: "recovery", Node: "pve-node2", UID: "uid-second-episode"}
	})
	f.clock.t = f.clock.t.Add(time.Minute)
	_, err = f.gate(t, "pve-node2")
	requireHADeferral(t, err, "10m0s of the 10m0s grace window remain")
}

func TestGateHADeleteRecoveryLostRecordRestartsTheWindow(t *testing.T) {
	f := newHADeleteFixture(t, recoveryScene(), 600)
	if _, err := f.gate(t, "pve-node2"); err == nil {
		t.Fatal("recovery must defer")
	}
	if err := os.RemoveAll(f.cache); err != nil {
		t.Fatal(err)
	}
	f.clock.t = f.clock.t.Add(30 * time.Minute)
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "10m0s of the 10m0s grace window remain")
}

func TestGateHADeleteRecoveryUnreadableRecordIsLoggedAndRestarts(t *testing.T) {
	f := newHADeleteFixture(t, recoveryScene(), 600)
	if err := os.MkdirAll(f.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cache, ha.EpisodeFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "10m0s of the 10m0s grace window remain")
	if !f.logged(slog.LevelWarn, "episode record is unavailable") {
		t.Fatal("an unavailable episode record must be logged")
	}
}

// TestGateHADeleteRecoveryWithAStaleCRMIsUnknown pins that the grace window
// needs a fresh CRM view: a stale one is unknown, never recovery.
func TestGateHADeleteRecoveryWithAStaleCRMIsUnknown(t *testing.T) {
	scene := recoveryScene()
	scene.CRMAge = 600
	f := newHADeleteFixture(t, scene, 600)
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "HA state is unknown", "CRM timestamp")
	if _, statErr := os.Stat(filepath.Join(f.cache, ha.EpisodeFileName)); !os.IsNotExist(statErr) {
		t.Fatal("an unknown state must not start a grace window")
	}
}

// stopFixture reads the gate's state for the stop tests.
func stopFixture(t *testing.T, scene *HADeleteScene, motionSec int) (*haDeleteFixture, *pve.HAServiceState) {
	t.Helper()
	f := newHADeleteFixture(t, scene, motionSec)
	st, err := f.gate(t, "pve-node1")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	return f, st
}

func (f *haDeleteFixture) stop(t *testing.T, st *pve.HAServiceState) (haStopOutcome, error) {
	t.Helper()
	return stopHAManagedVMForDelete(t.Context(), f.deps, "delete_vm", st, 101, f.logger)
}

func TestStopHAManagedVMWithSysConsoleWritesTheHAResource(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	f, st := stopFixture(t, scene, 30)
	outcome, err := f.stop(t, st)
	if err != nil || outcome != haStopReached {
		t.Fatalf("stop: outcome %d err %v", outcome, err)
	}
	events := scene.EventsSnapshot()
	if !slices.Equal(events, []string{"UpdateHaResources:vm:101=stopped"}) {
		t.Fatalf("the stop must go through the HA resource alone, got %v", events)
	}
	scene.Do(func(s *HADeleteScene) {
		if _, ok := s.Requested["vm:101"]; !ok {
			t.Fatal("the HA resource must stay configured until the destroy purges it")
		}
	})
}

// TestStopHAManagedVMWithAnObserveOnlyToken stops through the direct call
// when the token holds VM.PowerMgmt and VM.Allocate but not Sys.Console, and
// when the permissions cannot be read at all.
func TestStopHAManagedVMWithAnObserveOnlyToken(t *testing.T) {
	for name, perms := range map[string]map[string]bool{
		"observe-only": {"VM.PowerMgmt": true, "VM.Allocate": true, "VM.Audit": true},
		"unreadable":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			scene := NewHADeleteScene()
			scene.Manage(101, "pve-node1", "started", "started")
			scene.Perms = perms
			f, st := stopFixture(t, scene, 30)
			outcome, err := f.stop(t, st)
			if err != nil || outcome != haStopReached {
				t.Fatalf("stop: outcome %d err %v", outcome, err)
			}
			if events := scene.EventsSnapshot(); !slices.Equal(events, []string{"Stop:pve-node1/101"}) {
				t.Fatalf("without Sys.Console the stop is the direct call alone, got %v", events)
			}
		})
	}
}

func TestStopHAManagedVMErroredAndStoppedWritesNothing(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "error")
	scene.Perms = map[string]bool{"VM.PowerMgmt": true, "VM.Allocate": true}
	f, st := stopFixture(t, scene, 30)
	outcome, err := f.stop(t, st)
	if err != nil || outcome != haStopNoHAWrite {
		t.Fatalf("stop: outcome %d err %v", outcome, err)
	}
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("an errored, stopped VM without Sys.Console gets no HA write, got %v", events)
	}
}

func TestStopHAManagedVMErroredAndRunningNamesTheCommand(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "error")
	scene.Perms = map[string]bool{"VM.PowerMgmt": true, "VM.Allocate": true}
	scene.Do(func(s *HADeleteScene) { s.Power[101] = "running" })
	f, st := stopFixture(t, scene, 30)
	_, err := f.stop(t, st)
	requireHADeferral(t, err, "Sys.Console", "ha-manager set vm:101 --state disabled")
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a refused stop must write nothing, got %v", events)
	}
}

func TestStopHAManagedVMIgnoredResourceStopsDirectly(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "ignored", "")
	f, st := stopFixture(t, scene, 30)
	outcome, err := f.stop(t, st)
	if err != nil || outcome != haStopNotManaged {
		t.Fatalf("stop: outcome %d err %v", outcome, err)
	}
}

func TestStopHAManagedVMHeldByTheCRMWithoutAResourceDefers(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "", "started")
	f, st := stopFixture(t, scene, 30)
	_, err := f.stop(t, st)
	requireHADeferral(t, err, "still holds vm:101 although no HA resource is configured")
}

func TestStopHAManagedVMThatNeverStopsIsRetriable(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.StopApplies = false
	f, st := stopFixture(t, scene, 1)
	_, err := f.stop(t, st)
	requireHADeferral(t, err, "stop HA-managed VM 101 through PVE HA")
}

// TestStopHAManagedVMStoppedOnAnotherNodeIsRetriable moves the service to
// another node as the stop lands, so the destroy would run against the wrong
// node.
func TestStopHAManagedVMStoppedOnAnotherNodeIsRetriable(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	f, st := stopFixture(t, scene, 30)
	scene.OnRead = func(s *HADeleteScene, _ int) {
		if s.Requested["vm:101"] == "stopped" {
			s.Services["vm:101"] = HASceneService{State: "stopped", Node: "pve-node2", UID: "uid-101"}
		}
	}
	_, err := f.stop(t, st)
	requireHADeferral(t, err)
}

// TestStopThenCrashThenRetryProceedsToTheDestroy models a delete that stopped
// the VM through PVE HA and crashed before the destroy. The retry finds the
// resource still configured and stopped, proceeds, and asks for nothing more.
func TestStopThenCrashThenRetryProceedsToTheDestroy(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	f, st := stopFixture(t, scene, 30)
	if _, err := f.stop(t, st); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	retry, err := f.gate(t, "pve-node1")
	if err != nil {
		t.Fatalf("retry gate: %v", err)
	}
	if !retry.Managed() || retry.Raw != "stopped" {
		t.Fatalf("retry state: managed=%v raw=%s", retry.Managed(), retry.Raw)
	}
	outcome, err := f.stop(t, retry)
	if err != nil || outcome != haStopReached {
		t.Fatalf("retry stop: outcome %d err %v", outcome, err)
	}
	if events := scene.EventsSnapshot(); len(events) != 1 {
		t.Fatalf("the retry must not ask PVE HA again, got %v", events)
	}
}

func TestCleanupHAMembershipRefusesFenceAndUnknown(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Rules["bosh-na-101"] = map[string]any{"type": "node-affinity", "resources": "vm:101", "nodes": "pve-node2"}
	f := newHADeleteFixture(t, scene, 600)
	fenced := &pve.HAServiceState{SID: "vm:101", VMID: 101, Known: true, Configured: true, InCRM: true,
		Raw: "fence", Node: "pve-node2"}
	nodeFenced := &pve.HAServiceState{SID: "vm:101", VMID: 101, Known: true, Configured: true, InCRM: true,
		Raw: "started", Node: "pve-node2", NodeHealth: pve.NodeHealthEntry{CRMState: "fence"}}
	unknown := &pve.HAServiceState{SID: "vm:101", VMID: 101}
	for name, st := range map[string]*pve.HAServiceState{
		"fence": fenced, "node fence": nodeFenced, "unknown": unknown, "unread": nil,
	} {
		t.Run(name, func(t *testing.T) {
			err := cleanupHAMembership(t.Context(), f.deps, st, 101, f.logger)
			requireHADeferral(t, err, "HA membership cleanup of VM 101 refused")
			if events := scene.EventsSnapshot(); len(events) != 0 {
				t.Fatalf("a refused cleanup must touch nothing, got %v", events)
			}
		})
	}
	cleanupHAMembershipAfterDestroy(t.Context(), f.deps, fenced, 101, f.logger)
	if !f.logged(slog.LevelWarn, "HA membership cleanup after the destroy refused") {
		t.Fatal("a refusal after the destroy must be logged")
	}
}

// TestCleanupHAMembershipFollowsTheHAState removes anti-affinity membership
// only for an HA-managed VM, whatever the pve.ha knobs say, and always
// removes the VM's own node-affinity pin.
func TestCleanupHAMembershipFollowsTheHAState(t *testing.T) {
	for _, managed := range []bool{true, false} {
		t.Run(map[bool]string{true: "managed", false: "unmanaged"}[managed], func(t *testing.T) {
			scene := NewHADeleteScene()
			scene.Rules["bosh-aa-web"] = map[string]any{
				"type": "resource-affinity", "affinity": "negative", "resources": "vm:101,vm:102,vm:103",
			}
			scene.Rules["bosh-na-101"] = map[string]any{"type": "node-affinity", "resources": "vm:101"}
			f := newHADeleteFixture(t, scene, 600)
			st := &pve.HAServiceState{SID: "vm:101", VMID: 101, Known: true, Configured: managed, Raw: "stopped",
				Node: "pve-node1"}
			if err := cleanupHAMembership(t.Context(), f.deps, st, 101, f.logger); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			scene.Do(func(s *HADeleteScene) {
				if _, pinned := s.Rules["bosh-na-101"]; pinned {
					t.Fatal("the VM's node-affinity pin must be removed")
				}
				resources, _ := s.Rules["bosh-aa-web"]["resources"].(string)
				if named := strings.Contains(resources, "vm:101"); named == managed {
					t.Fatalf("anti-affinity rule resources %q after cleanup of a managed=%v VM", resources, managed)
				}
			})
		})
	}
}

func TestLogHasVMHAState(t *testing.T) {
	scene := recoveryScene()
	f := newHADeleteFixture(t, scene, 600)

	logHasVMHAState(t.Context(), f.deps, pve.VMLocation{Found: true, Node: "pve-node2"}, 101, f.logger)
	if scene.ManagerReads != 0 {
		t.Fatal("a VM the scan shows without HA state must not cost an HA read")
	}

	logHasVMHAState(t.Context(), f.deps, pve.VMLocation{Found: true, Node: "pve-node2", HAState: "started"}, 101,
		f.logger)
	var entry *log.Entry
	for _, e := range f.logs.All() {
		if e.Message == "has_vm: VM exists and is HA-managed" {
			entry = &e
		}
	}
	if entry == nil {
		t.Fatal("has_vm must log the HA state of an HA-managed VM")
	}
	if entry.Attrs["raw_state"] != "recovery" || entry.Attrs["disposition"] != string(pve.HAGraceWindow) {
		t.Fatalf("has_vm log attrs: %v", entry.Attrs)
	}
	if len(scene.EventsSnapshot()) != 0 {
		t.Fatal("has_vm must not write anything")
	}
}

// TestHaDeleteSysConsoleFailsClosed answers false for a client that cannot
// read permissions.
func TestHaDeleteSysConsoleFailsClosed(t *testing.T) {
	logger := log.NewNopLogger()
	if haDeleteSysConsole(t.Context(), Deps{PVE: struct{ pve.Client }{}}, logger) {
		t.Fatal("a client without a permission reader must answer false")
	}
	scene := NewHADeleteScene()
	if !haDeleteSysConsole(t.Context(), Deps{PVE: newHASceneClient(scene)}, logger) {
		t.Fatal("a held Sys.Console must answer true")
	}
	scene.Perms = map[string]bool{"Sys.Console": false}
	if !haDeleteSysConsole(t.Context(), Deps{PVE: newHASceneClient(scene)}, logger) {
		t.Fatal("a privilege that does not propagate is still held on /")
	}
}

func TestHaMotionWaitDefaults(t *testing.T) {
	if haMotionWait(Deps{}) != pve.DefaultMotionWait {
		t.Fatal("nil config must use the default motion wait")
	}
	if opts := haHealthOptions(Deps{}); len(opts.MaintenanceTags) != 0 {
		t.Fatal("nil config has no maintenance tags")
	}
	if haEpisodes(Deps{}) == nil {
		t.Fatal("the default episode store must not be nil")
	}
	if err := sleepOrDone(t.Context(), 0); err != nil {
		t.Fatalf("a zero sleep on a live context: %v", err)
	}
}
