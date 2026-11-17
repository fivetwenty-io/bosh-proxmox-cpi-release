package handlers

import (
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// These tests drive the delete's HA gate through a recovery deferral and the
// settle that follows it, on the gate fixture's clock. The marker's age runs
// on that clock, and the VM's uptime comes from the scene, as status/current
// reports it from the node.

// restartInPlace has node pve-node2 come back and PVE HA hold vm:101 started
// on it again, with the VM running for uptime seconds.
func restartInPlace(s *HADeleteScene, uptime int64) {
	s.Offline["pve-node2"] = false
	s.NodeState["pve-node2"] = "online"
	s.LRMAge["pve-node2"] = 2
	s.Services["vm:101"] = HASceneService{State: "started", Node: "pve-node2", UID: "uid-restart"}
	s.IndexHAState[101] = "started"
	delete(s.Power, 101)
	s.Uptime[101] = uptime
}

// requireDeferralHeld fails unless the fixture's record holds a deferral of
// vm:101 exactly when want says so.
func (f *haDeleteFixture) requireDeferralHeld(t *testing.T, want bool) {
	t.Helper()
	_, held, err := f.deps.HAEpisodes.LastRecoveryDeferral("vm:101")
	if err != nil {
		t.Fatalf("read the recovery deferral: %v", err)
	}
	if held != want {
		t.Fatalf("recovery deferral of vm:101 held = %v, want %v", held, want)
	}
}

func (f *haDeleteFixture) statusReads() int {
	var n int
	f.scene.Do(func(s *HADeleteScene) { n = s.StatusReads })
	return n
}

// TestGateHADeleteSettlesAVMThatHARestartedInPlace is the outage the
// resurrector meets. The delete defers while the VM is in recovery on the dead
// node, and the node comes back. PVE HA restarts the VM in place, and the
// resurrector's next delete arrives while the guest is still booting. That
// delete must wait, and the one after the VM has run for the settle window
// proceeds and clears the deferral.
func TestGateHADeleteSettlesAVMThatHARestartedInPlace(t *testing.T) {
	scene := recoveryScene()
	f := newHADeleteFixture(t, scene, 600)

	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "grace window remain")
	f.requireDeferralHeld(t, true)

	f.clock.t = f.clock.t.Add(2 * time.Minute)
	scene.Do(func(s *HADeleteScene) { restartInPlace(s, 32) })
	st, err := f.gate(t, "pve-node2")
	if st != nil {
		t.Fatalf("a VM PVE HA restarted 32s ago must not be released to the destroy: %+v", st)
	}
	requireHADeferral(t, err, "PVE HA restarted it on node pve-node2 32s ago", "deferred its delete 2m0s ago",
		"has run for 5m0s", "proceeds 4m28s from now")
	if !isWholeHADeferral(err) {
		t.Fatalf("the settle wait must be an HA deferral, so no journal step is left behind: %v", err)
	}
	if ev := scene.EventsSnapshot(); len(ev) != 0 {
		t.Fatalf("the settle wait must write nothing; events: %v", ev)
	}
	f.requireDeferralHeld(t, true)

	// The resurrector's next scan two minutes on finds the VM booted. Were it
	// to delete again, the wait still holds.
	f.clock.t = f.clock.t.Add(2 * time.Minute)
	scene.Do(func(s *HADeleteScene) { s.Uptime[101] = 152 })
	_, err = f.gate(t, "pve-node2")
	requireHADeferral(t, err, "proceeds 2m28s from now")

	f.clock.t = f.clock.t.Add(3 * time.Minute)
	scene.Do(func(s *HADeleteScene) { s.Uptime[101] = 300 })
	st, err = f.gate(t, "pve-node2")
	if err != nil || st == nil || st.Raw != "started" {
		t.Fatalf("a VM that has run for the settle window must proceed: st=%+v err=%v", st, err)
	}
	f.requireDeferralHeld(t, false)
	if !f.logged(slog.LevelInfo, "run past the settle window") {
		t.Fatal("the release of the deferral was not logged")
	}
}

// TestGateHADeleteWithoutADeferralNeverWaits is the ordinary delete of a VM
// that PVE HA started seconds ago, such as the cleanup of a failed deploy. No
// delete of it ever deferred, so it proceeds at once and reads no status for
// the settle check, even while another VM's deferral is recorded.
func TestGateHADeleteWithoutADeferralNeverWaits(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Uptime[101] = 10
	f := newHADeleteFixture(t, scene, 600)
	if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:102"); err != nil {
		t.Fatal(err)
	}

	st, err := f.gate(t, "pve-node1")
	if err != nil || st == nil {
		t.Fatalf("a fresh VM with no recovery deferral must proceed: st=%+v err=%v", st, err)
	}
	if n := f.statusReads(); n != 0 {
		t.Fatalf("a delete with no recovery deferral must read no status, read %d", n)
	}
	if _, held, _ := f.deps.HAEpisodes.LastRecoveryDeferral("vm:102"); !held {
		t.Fatal("another VM's deferral must stay")
	}
}

// TestGateHADeleteSettleIsBoundedByTheDeferralLife keeps the VM's uptime under
// the settle window, as a guest that HA restarts in a loop would. The wait
// still ends fifteen minutes after the last recovery deferral, and the settle
// deferrals never extend it.
func TestGateHADeleteSettleIsBoundedByTheDeferralLife(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Uptime[101] = 10
	f := newHADeleteFixture(t, scene, 600)
	if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}

	f.clock.t = f.clock.t.Add(14 * time.Minute)
	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "proceeds 1m0s from now")

	f.clock.t = f.clock.t.Add(time.Minute)
	st, err := f.gate(t, "pve-node1")
	if err != nil || st == nil {
		t.Fatalf("a deferral fifteen minutes old must no longer hold the delete: st=%+v err=%v", st, err)
	}
	f.requireDeferralHeld(t, false)
}

// TestGateHADeleteSettleClearsWhenHADoesNotHoldTheVMStarted lets the delete of
// a VM that PVE HA holds stopped proceed at once and clears the deferral,
// because there is no restart to wait for.
func TestGateHADeleteSettleClearsWhenHADoesNotHoldTheVMStarted(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "stopped", "stopped")
	f := newHADeleteFixture(t, scene, 600)
	if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}

	st, err := f.gate(t, "pve-node1")
	if err != nil || st == nil {
		t.Fatalf("a VM HA holds stopped must proceed: st=%+v err=%v", st, err)
	}
	f.requireDeferralHeld(t, false)
	if n := f.statusReads(); n != 0 {
		t.Fatalf("a VM HA holds stopped needs no status read, read %d", n)
	}
}

// TestGateHADeleteSettleWaitsForAVMThatIsNotRunningYet has PVE HA hold the VM
// started while the node has not started it yet. The delete waits, bounded by
// the deferral's life.
func TestGateHADeleteSettleWaitsForAVMThatIsNotRunningYet(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.Power[101] = "stopped"
	f := newHADeleteFixture(t, scene, 600)
	if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}
	f.clock.t = f.clock.t.Add(5 * time.Minute)

	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "not running yet", "10m0s from now at the latest")
	f.requireDeferralHeld(t, true)
}

// TestGateHADeleteSettleDefersWhenThePowerStateIsUnreadable keeps the delete
// deferring while a fresh deferral is recorded and status/current fails,
// because the stop and the destroy need the same API.
func TestGateHADeleteSettleDefersWhenThePowerStateIsUnreadable(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.StatusErr = errors.New("500 status read failed (HTTP500)")
	f := newHADeleteFixture(t, scene, 600)
	if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
		t.Fatal(err)
	}

	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "could not be read", "proceeds whatever it reads")
	f.requireDeferralHeld(t, true)
}

// TestGateHADeleteSettleOnlyForDeleteVM keeps the create_vm rollback and the
// deferred-teardown sweep out of the record. Their deferrals record nothing,
// and a recorded deferral never holds them.
func TestGateHADeleteSettleOnlyForDeleteVM(t *testing.T) {
	for _, caller := range []string{"create_vm: rollback", "deferred teardown sweep"} {
		t.Run(caller, func(t *testing.T) {
			scene := recoveryScene()
			f := newHADeleteFixture(t, scene, 600)
			_, err := gateHADelete(t.Context(), f.deps, caller, 101, "pve-node2", f.logger)
			requireHADeferral(t, err, "grace window remain")
			f.requireDeferralHeld(t, false)

			if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
				t.Fatal(err)
			}
			scene.Do(func(s *HADeleteScene) { restartInPlace(s, 10) })
			st, err := gateHADelete(t.Context(), f.deps, caller, 101, "pve-node2", f.logger)
			if err != nil || st == nil {
				t.Fatalf("%s must not wait for a settle: st=%+v err=%v", caller, st, err)
			}
			if n := f.statusReads(); n != 0 {
				t.Fatalf("%s must read no status for a settle, read %d", caller, n)
			}
		})
	}
}

// TestManagedVMDisposalsOutsideDeleteVMIgnoreTheSettleRecord gates the VM of
// a rolled-back managed create_vm attempt and of an operator's storage-journal
// cleanup. Neither VM was handed to the Director, so neither gate records a
// deferral, and a recorded deferral never holds either one.
func TestManagedVMDisposalsOutsideDeleteVMIgnoreTheSettleRecord(t *testing.T) {
	for _, caller := range []string{managedVMRollbackCaller, managedVMCleanupCaller} {
		t.Run(caller, func(t *testing.T) {
			scene := recoveryScene()
			f := newHADeleteFixture(t, scene, 600)
			_, err := locateAndGateManagedVM(t.Context(), f.deps, caller, "pve-node2", 101)
			requireHADeferral(t, err, caller+": VM 101", "grace window remain")
			f.requireDeferralHeld(t, false)

			if err := f.deps.HAEpisodes.RecordRecoveryDeferral("vm:101"); err != nil {
				t.Fatal(err)
			}
			scene.Do(func(s *HADeleteScene) { restartInPlace(s, 10) })
			st, err := locateAndGateManagedVM(t.Context(), f.deps, caller, "pve-node2", 101)
			if err != nil || st == nil {
				t.Fatalf("%s must not wait for a settle: st=%+v err=%v", caller, st, err)
			}
		})
	}
}

// TestRecordHARecoveryDeferralForStatesThatMayHideARecovery records a
// deferral for the defer and grace window answers of a managed VM and for
// every unknown state, except one whose known state proves PVE HA does not
// manage the VM. Motion, proceed, and an unmanaged VM's defer record nothing.
func TestRecordHARecoveryDeferralForStatesThatMayHideARecovery(t *testing.T) {
	managed := &pve.HAServiceState{SID: "vm:101", VMID: 101, Configured: true, InCRM: true}
	cases := []struct {
		name   string
		caller string
		st     *pve.HAServiceState
		disp   pve.HADisposition
		want   bool
	}{
		{"defer", "delete_vm", managed, pve.HADefer, true},
		{"grace window", "delete_vm", managed, pve.HAGraceWindow, true},
		{"motion", "delete_vm", managed, pve.HAWaitForMotion, false},
		{"unknown", "delete_vm", managed, pve.HAUnknown, true},
		{"unknown and unread", "delete_vm", &pve.HAServiceState{SID: "vm:101", VMID: 101}, pve.HAUnknown, true},
		{"unknown on a known unmanaged VM", "delete_vm",
			&pve.HAServiceState{SID: "vm:101", VMID: 101, Known: true}, pve.HAUnknown, false},
		{"proceed", "delete_vm", managed, pve.HAProceed, false},
		{"not managed", "delete_vm", &pve.HAServiceState{SID: "vm:101", VMID: 101}, pve.HADefer, false},
		{"no state", "delete_vm", nil, pve.HADefer, false},
		{"unknown for the rollback", "create_vm: rollback", managed, pve.HAUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHADeleteFixture(t, NewHADeleteScene(), 600)
			recordHARecoveryDeferral(f.deps, tc.caller, tc.st, tc.disp, f.logger)
			f.requireDeferralHeld(t, tc.want)
		})
	}
}

// TestGateHADeleteSettlesAVMRestartedAfterAnUnknownState defers a delete
// because the LRM of the VM's node has no fresh timestamp, as it reads while
// PVE HA decides to fence the node. The deferral is recorded, so when the
// next delete finds the VM restarted 20 seconds ago it waits, and the delete
// after the settle window proceeds and clears the record.
func TestGateHADeleteSettlesAVMRestartedAfterAnUnknownState(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.LRMAge["pve-node1"] = 3600
	f := newHADeleteFixture(t, scene, 600)

	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "HA state is unknown")
	f.requireDeferralHeld(t, true)

	f.clock.t = f.clock.t.Add(time.Minute)
	scene.Do(func(s *HADeleteScene) {
		s.LRMAge["pve-node1"] = 2
		s.Uptime[101] = 20
	})
	st, err := f.gate(t, "pve-node1")
	if st != nil {
		t.Fatalf("a VM restarted 20s after an unknown-state deferral must not be released: %+v", st)
	}
	requireHADeferral(t, err, "PVE HA restarted it on node pve-node1 20s ago", "proceeds 4m40s from now")

	scene.Do(func(s *HADeleteScene) { s.Uptime[101] = 300 })
	if st, err := f.gate(t, "pve-node1"); err != nil || st == nil {
		t.Fatalf("a VM that has run for the settle window must proceed: st=%+v err=%v", st, err)
	}
	f.requireDeferralHeld(t, false)
}

// TestStatusUptime reads the uptime status/current reports in each shape the
// wire can carry it, and treats anything that is not a finite, non-negative
// number as missing.
func TestStatusUptime(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   time.Duration
		ok     bool
	}{
		{"float", map[string]any{"uptime": float64(32)}, 32 * time.Second, true},
		{"int64", map[string]any{"uptime": int64(301)}, 301 * time.Second, true},
		{"string", map[string]any{"uptime": " 45 "}, 45 * time.Second, true},
		{"zero", map[string]any{"uptime": 0}, 0, true},
		{"huge", map[string]any{"uptime": math.MaxFloat64}, statusUptimeCap, true},
		{"missing", map[string]any{"status": "running"}, 0, false},
		{"null", map[string]any{"uptime": nil}, 0, false},
		{"text", map[string]any{"uptime": "soon"}, 0, false},
		{"negative", map[string]any{"uptime": float64(-5)}, 0, false},
		{"not a number", map[string]any{"uptime": math.NaN()}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := statusUptime(tc.status)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("statusUptime = %s, %v; want %s, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestGateHADeleteGraceTextForAVMPinnedToTheDeadNodeAlone words the grace
// window deferral of a VM whose strict pin admits the dead node alone. PVE HA
// can recover it nowhere else, so neither the running nor the expired text
// promises a recovery onto a live node or a config move, and both say that PVE
// HA restarts it in place.
func TestGateHADeleteGraceTextForAVMPinnedToTheDeadNodeAlone(t *testing.T) {
	scene := recoveryScene()
	scene.Rules["bosh-na-101"] = map[string]any{"type": "node-affinity", "resources": "vm:101",
		"nodes": "pve-node2", "strict": 1}
	f := newHADeleteFixture(t, scene, 600)

	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "admits node pve-node2 alone", "restarts it in place once the node returns",
		"10m0s of the 10m0s grace window remain", "after it expires for the same reason", "up to 5m0s of uptime")
	if strings.Contains(err.Error(), "onto a live node") {
		t.Fatalf("the running text must not promise a recovery onto a live node: %v", err)
	}

	f.clock.t = f.clock.t.Add(11 * time.Minute)
	_, err = f.gate(t, "pve-node2")
	requireHADeferral(t, err, "grace window expired 1m0s ago", "no live node can take the VM's config",
		"restarts the VM in place once the node returns", "'ha-manager status' for vm:101")
	if strings.Contains(err.Error(), "moves the config onto a live node") {
		t.Fatalf("the expired text must not promise a config move: %v", err)
	}
}

// TestGateHADeleteGraceTextKeepsTheRecoveryForOtherPins keeps the general text
// for every pin that leaves PVE HA another node, or that PVE HA may not apply,
// and when the rules cannot be read.
func TestGateHADeleteGraceTextKeepsTheRecoveryForOtherPins(t *testing.T) {
	cases := []struct {
		name string
		rule map[string]any
	}{
		{"no pin", nil},
		{"pin that names another node too", map[string]any{"type": "node-affinity", "resources": "vm:101",
			"nodes": "pve-node1,pve-node2", "strict": 1}},
		{"pin that is not strict", map[string]any{"type": "node-affinity", "resources": "vm:101",
			"nodes": "pve-node2"}},
		{"disabled pin", map[string]any{"type": "node-affinity", "resources": "vm:101", "nodes": "pve-node2",
			"strict": 1, "disable": 1}},
		{"pin of another resource", map[string]any{"type": "node-affinity", "resources": "vm:102",
			"nodes": "pve-node2", "strict": 1}},
		{"negative pin", map[string]any{"type": "node-affinity", "resources": "vm:101", "nodes": "pve-node2",
			"strict": 1, "affinity": "negative"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scene := recoveryScene()
			if tc.rule != nil {
				scene.Rules["bosh-na-101"] = tc.rule
			}
			f := newHADeleteFixture(t, scene, 600)
			_, err := f.gate(t, "pve-node2")
			requireHADeferral(t, err, "still trying to recover it", "onto a live node")
			if strings.Contains(err.Error(), "in place") {
				t.Fatalf("the general text must not claim an in-place restart: %v", err)
			}
		})
	}
}
