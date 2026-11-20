package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// drRun returns a drain or an undrain of pve01 over f, with a settle timeout of
// 30 minutes.
func drRun(f *fakeRebalanceIO, command string, apply bool) *drainRun {
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto)}
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector, Logger: log.NewNopLogger()}
	r := newDrainRun(deps, command, "pve01", apply, 30*time.Minute, f)
	r.rebalance = RebalanceOptions{Apply: apply, SettleTimeout: 30 * time.Minute}.withDefaults()
	return r
}

// drMaintenance puts pve01 in PVE HA maintenance.
func drMaintenance(f *fakeRebalanceIO) {
	e := &f.profileVal.Health.Nodes[0]
	e.Maintenance, e.Placeable, e.CRMState, e.LRMMode = true, false, "maintenance", "maintenance"
}

// drHold adds a VM of the deployment cf that a strict pin keeps on pve01
// alone, so it can't leave.
func drHold(f *fakeRebalanceIO, vmid int) *rbVM {
	v := f.addVM(vmid, "pve01", 4)
	v.tags += ";" + deploymentTagPrefix + "cf"
	f.pins[vmid] = rbPin(vmid, map[string]int{"pve01": 0})
	f.pins[vmid].Strict = true
	return v
}

// drVMs renders the report's VMs as "vmid fate outcome".
func drVMs(vms []DrainVM) []string {
	out := make([]string, 0, len(vms))
	for i := range vms {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%d %s %s", vms[i].VMID, vms[i].Fate, vms[i].Outcome)))
	}
	return out
}

// drGate returns the maintenance gate of gates.
func drGate(t *testing.T, gates []RebalanceGate) RebalanceGate {
	t.Helper()
	for _, g := range gates {
		if g.Name == RebalanceGateMaintenance {
			return g
		}
	}
	t.Fatalf("no maintenance gate in %v", gates)
	return RebalanceGate{}
}

func TestDrainRun_DryRunPlansTheStopsAndWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	f.addVM(101, "pve01", 4)
	drHold(f, 102)
	rep := drRun(f, DrainCommand, false).drain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Applied || rep.Halted != "" {
		t.Fatalf("exit %d applied %v halted %q", rep.ExitCode, rep.Applied, rep.Halted)
	}
	if got, want := drVMs(rep.VMs), []string{"101 leaves", "102 stays planned"}; !slices.Equal(got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if g := drGate(t, rep.Gates); !g.OK ||
		!strings.Contains(g.Detail, "for --apply, the HA manager doesn't report node pve01 in maintenance") ||
		!strings.Contains(g.Detail, "node-maintenance enable pve01") {
		t.Fatalf("maintenance gate %+v", g)
	}
	if !slices.Equal(rep.Deployments, []string{"cf"}) || !strings.Contains(rep.Reminder, "resurrection off for `cf`") {
		t.Fatalf("deployments %v reminder %q", rep.Deployments, rep.Reminder)
	}
	if len(f.callsWith("runlock")) != 0 || len(f.callsWith("stop ")) != 0 || len(f.callsWith("record ")) != 0 {
		t.Fatalf("a dry run took a lock or wrote: %v", f.calls)
	}
}

func TestDrainRun_ApplyRefusesUntilTheNodeIsInMaintenance(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drHold(f, 102)
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateMaintenance {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if g := drGate(t, rep.Gates); g.OK ||
		!strings.Contains(g.Detail, "`ha-manager crm-command node-maintenance enable pve01`") {
		t.Fatalf("maintenance gate %+v", g)
	}
	if len(f.callsWith("stop ")) != 0 || f.release != 1 {
		t.Fatalf("a refused run wrote or kept its lock: %v", f.calls)
	}

	// An operator tag alone moves nothing, so it doesn't count either.
	f.profileVal.Health.Nodes[0].Maintenance = true
	rep = drRun(f, DrainCommand, true).drain(context.Background())
	if g := drGate(t, rep.Gates); g.OK || !strings.Contains(g.Detail, "tag alone") {
		t.Fatalf("maintenance gate %+v", g)
	}
}

func TestDrainRun_ApplyWaitsForPVEThenStopsWhatCantLeave(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	f.addVM(101, "pve01", 4)
	drHold(f, 102)
	f.addVM(103, "pve01", 4).tags += ";" + deploymentTagPrefix + "redis"
	f.addVM(104, "pve02", 4)
	f.addVM(105, "pve03", 4)
	f.extraRules = append(f.extraRules, negativeRule("bosh-aa-one", 103, 104, 105))
	f.moving[101] = true
	f.stopSettle = 3
	// PVE HA moves VM 101 to pve02 by the third read.
	f.observeHook = func(n int) {
		if n == 3 {
			f.vms[101].node = "pve02"
			delete(f.moving, 101)
		}
	}
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Halted != "" {
		t.Fatalf("exit %d halted %q vms %v", rep.ExitCode, rep.Halted, rep.VMs)
	}
	if got, want := drVMs(rep.VMs), []string{"102 stays stopped", "103 stays stopped"}; !slices.Equal(got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if !strings.Contains(rep.VMs[1].Reason, "VM 104 on pve02 in bosh-aa-one, and VM 105 on pve03") {
		t.Fatalf("reason %q", rep.VMs[1].Reason)
	}
	for _, id := range []int{102, 103} {
		v := f.vms[id]
		if v.state != "stopped" || !strings.HasSuffix(v.comment, " "+ha.DrainMarkKey+"pve01") {
			t.Fatalf("VM %d state %q comment %q", id, v.state, v.comment)
		}
	}
	if f.vms[101].state != "started" || len(f.callsWith("stop 101")) != 0 {
		t.Fatal("the drain stopped a VM that could leave")
	}
	// No stop starts before PVE HA moved VM 101, each stop runs under the
	// record and the claim, and the run waits for the stops before it lets
	// the lock go.
	firstStop := slices.Index(f.calls, "stop 102")
	if f.snapshots < 3 || firstStop < slices.Index(f.calls, "sleep 5s") {
		t.Fatalf("a stop came before the wait: %v", f.calls)
	}
	if got := f.calls[firstStop-2 : firstStop+3]; !slices.Equal(got,
		[]string{"record 102", "claim 102", "stop 102", "unclaim 102", "unrecord 102"}) {
		t.Fatalf("the stop ran outside its locks: %v", got)
	}
	if f.calls[len(f.calls)-1] != "release" || len(f.callsWith("states")) < 3 {
		t.Fatalf("the run didn't wait for the stops: %v", f.calls)
	}
	if !slices.Equal(rep.Deployments, []string{"cf", "redis"}) ||
		!strings.Contains(rep.Reminder, "`cf` and `redis` until `pve-ops ha undrain pve01 --apply`") {
		t.Fatalf("deployments %v reminder %q", rep.Deployments, rep.Reminder)
	}
}

func TestDrainRun_AVMThatCouldLeaveButStaysEndsWithExit1(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	f.addVM(101, "pve01", 4)
	drHold(f, 102)
	r := drRun(f, DrainCommand, true)
	r.settle = time.Minute
	rep := r.drain(context.Background())
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "1 VM that could leave node pve01") {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if vm := rep.VMs[0]; vm.Outcome != DrainOutcomeFailed || !strings.Contains(vm.Error, "pve02 or pve03") {
		t.Fatalf("vm %+v", vm)
	}
	if len(f.callsWith("stop ")) != 0 || f.vms[102].state != "started" {
		t.Fatal("a drain that timed out stopped a VM")
	}
}

func TestDrainRun_ABusyOrFailedStopLeavesTheVMRunningAndExits1(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	drHold(f, 102)
	drHold(f, 103)
	drHold(f, 104)
	f.claimErr[102] = fmt.Errorf("%w: another call claims the VM", errRebalanceBusy)
	f.drainHook = func(vmid int, _ bool) (bool, error) {
		if vmid == 104 {
			return false, errors.New("500 Internal Server Error")
		}
		return true, nil
	}
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitError ||
		rep.Halted != "the drain left 2 VMs that can't leave running; run it again to stop them" {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if got, want := drVMs(rep.VMs), []string{"102 stays busy", "103 stays stopped", "104 stays failed"}; !slices.Equal(
		got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if f.vms[102].state != "started" || f.vms[104].state != "started" {
		t.Fatal("a stop that didn't land changed the VM")
	}
}

func TestDrainRun_ARerunGoesOnFromTheTokens(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	done := drHold(f, 102)
	done.state, done.comment = "stopped", ha.WithDrainMark(done.comment, "pve01")
	drHold(f, 103)
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitOK {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if got, want := drVMs(rep.VMs), []string{"102 not_started stopped_earlier", "103 stays stopped"}; !slices.Equal(
		got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if len(f.callsWith("stop 102")) != 0 || len(f.callsWith("stop 103")) != 1 {
		t.Fatalf("stops %v", f.callsWith("stop "))
	}
	if !strings.Contains(rep.VMs[0].Reason, "the drain of pve01 stopped it") || !slices.Equal(rep.Deployments,
		[]string{"cf"}) {
		t.Fatalf("reason %q deployments %v", rep.VMs[0].Reason, rep.Deployments)
	}
}

func TestDrainRun_ADigestConflictReadsAndWritesAgain(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	drHold(f, 102)
	drHold(f, 103)
	tries := map[int]int{}
	f.drainHook = func(vmid int, _ bool) (bool, error) {
		tries[vmid]++
		// VM 102 loses its first write, and VM 103 loses every write.
		return vmid == 102 && tries[vmid] > 1, nil
	}
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if got, want := drVMs(rep.VMs), []string{"102 stays stopped", "103 stays failed"}; !slices.Equal(got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if tries[102] != 2 || tries[103] != ha.CommentWriteAttempts || !strings.Contains(rep.VMs[1].Error,
		"other writers changed the HA resources on every try") {
		t.Fatalf("tries %v error %q", tries, rep.VMs[1].Error)
	}
	if rep.ExitCode != RebalanceExitError {
		t.Fatalf("exit %d", rep.ExitCode)
	}
}

func TestDrainRun_AStopThatDoesntSettleKeepsItsTokenAndExits1(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	drHold(f, 102)
	f.stopSettle = 1000
	r := drRun(f, DrainCommand, true)
	r.settle = time.Minute
	rep := r.drain(context.Background())
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "1 VM didn't reach stopped") {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if vm := rep.VMs[0]; vm.Outcome != DrainOutcomeLeftPending || !strings.Contains(vm.Error, "reported it started") {
		t.Fatalf("vm %+v", vm)
	}
	if _, ok := ha.DrainStopped(&f.vms[102].comment); !ok {
		t.Fatal("a stop that didn't settle lost its token")
	}
}

func TestDrainRun_AHeldRunLockRefuses(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	f.lockErr = pve.ErrClusterLockHeld
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateRunLock || len(f.callsWith("snapshot")) != 0 {
		t.Fatalf("exit %d halted %q calls %v", rep.ExitCode, rep.Halted, f.calls)
	}
}

// drStopped adds a VM of the deployment cf on node that a drain of pve01
// stopped.
func drStopped(f *fakeRebalanceIO, vmid int, node string) *rbVM {
	v := f.addVM(vmid, node, 4)
	v.tags += ";" + deploymentTagPrefix + "cf"
	v.state, v.comment = "stopped", ha.WithDrainMark(v.comment, "pve01")
	return v
}

func TestUndrainRun_RefusesWhileTheNodeIsInMaintenance(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	drStopped(f, 102, "pve01")
	rep := drRun(f, UndrainCommand, true).undrain(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateMaintenance {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if g := drGate(t, rep.Gates); !strings.Contains(g.Detail,
		"`ha-manager crm-command node-maintenance disable pve01`") {
		t.Fatalf("gate %+v", g)
	}
	if len(f.callsWith("start ")) != 0 || f.vms[102].state != "stopped" {
		t.Fatal("a refused undrain started a VM")
	}
}

func TestUndrainRun_StartsTheTokenedVMsAndThenRebalances(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drStopped(f, 102, "pve01")
	other := drStopped(f, 103, "pve02")
	other.comment = ha.WithDrainMark(haIdentity{Owner: aaDirector}.marker(), "pve02")
	f.addVM(104, "pve02", 4)
	f.addVM(105, "pve03", 4)
	f.startSettle = 2
	rep := drRun(f, UndrainCommand, true).undrain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Halted != "" {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if got := drVMs(rep.VMs); !slices.Equal(got, []string{"102  started"}) {
		t.Fatalf("vms %v", got)
	}
	if v := f.vms[102]; v.state != "started" || strings.Contains(v.comment, ha.DrainMarkKey) {
		t.Fatalf("VM 102 state %q comment %q", v.state, v.comment)
	}
	if f.vms[103].state != "stopped" || len(f.callsWith("start 103")) != 0 {
		t.Fatal("the undrain of pve01 started a VM another drain stopped")
	}
	if rep.Rebalance == nil || rep.Rebalance.ExitCode != RebalanceExitOK || !rep.Rebalance.Applied {
		t.Fatalf("rebalance %+v", rep.Rebalance)
	}
	// The rebalance runs under the undrain's run lock, after the start has
	// settled.
	if len(f.callsWith("runlock")) != 1 || f.release != 1 {
		t.Fatalf("run lock calls %v release %d", f.callsWith("runlock"), f.release)
	}
	if !strings.Contains(rep.Reminder, "resurrection back on for `cf`") {
		t.Fatalf("reminder %q", rep.Reminder)
	}
}

func TestUndrainRun_NoRebalanceStopsAfterTheStarts(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drStopped(f, 102, "pve01")
	r := drRun(f, UndrainCommand, true)
	r.noRebalance = true
	rep := r.undrain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Rebalance != nil || f.vms[102].state != "started" {
		t.Fatalf("exit %d rebalance %v state %q", rep.ExitCode, rep.Rebalance, f.vms[102].state)
	}
}

func TestUndrainRun_AFailedStartSkipsTheRebalance(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drStopped(f, 102, "pve01")
	drStopped(f, 103, "pve01")
	f.drainHook = func(vmid int, _ bool) (bool, error) {
		if vmid == 103 {
			return false, errors.New("500 Internal Server Error")
		}
		return true, nil
	}
	rep := drRun(f, UndrainCommand, true).undrain(context.Background())
	if rep.ExitCode != RebalanceExitError || rep.Rebalance != nil ||
		rep.Halted != "the undrain left 1 VM stopped; run it again to start it, and the rebalance didn't run" {
		t.Fatalf("exit %d rebalance %v halted %q", rep.ExitCode, rep.Rebalance, rep.Halted)
	}
	if f.vms[102].state != "started" || f.vms[103].state != "stopped" {
		t.Fatal("the starts didn't go on past the failure")
	}
	if _, ok := ha.DrainStopped(&f.vms[103].comment); !ok {
		t.Fatal("a VM whose start failed lost its token")
	}
}

func TestUndrainRun_DryRunListsTheStartsAndWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	drStopped(f, 102, "pve01")
	rep := drRun(f, UndrainCommand, false).undrain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Rebalance != nil {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if got := drVMs(rep.VMs); !slices.Equal(got, []string{"102  planned"}) {
		t.Fatalf("vms %v", got)
	}
	if g := drGate(t, rep.Gates); !g.OK || !strings.Contains(g.Detail, "for --apply") {
		t.Fatalf("gate %+v", g)
	}
	if len(f.callsWith("runlock")) != 0 || len(f.callsWith("start ")) != 0 || len(rep.Warnings) == 0 {
		t.Fatalf("a dry run took a lock or wrote: %v", f.calls)
	}
}

func TestDrainGates_TheNodesAndMaintenanceGates(t *testing.T) {
	t.Parallel()
	p := rbProfile()
	p.Health.Nodes[0].CRMState = "maintenance"
	if g := drainNodesGate(p, "pve01"); !g.OK {
		t.Fatalf("nodes gate %+v", g)
	}
	if g := drainNodesGate(p, "pve09"); g.OK || !strings.Contains(g.Detail, "pve09 is not a node") {
		t.Fatalf("nodes gate %+v", g)
	}
	p.Health.Nodes[1].Maintenance = true
	if g := drainNodesGate(p, "pve01"); g.OK || !strings.Contains(g.Detail, "pve02 is in maintenance too") {
		t.Fatalf("nodes gate %+v", g)
	}
	p.Health.Nodes[1].Maintenance = false
	if g := maintenanceGate(p, "pve01", true, false); !g.OK {
		t.Fatalf("drain gate %+v", g)
	}
	if g := maintenanceGate(p, "pve01", true, true); g.OK {
		t.Fatalf("undrain gate %+v", g)
	}
	p.Health.Nodes[0].CRMState = ""
	p.Health.Nodes[0].Maintenance = true
	if g := maintenanceGate(p, "pve01", true, true); g.OK || !strings.Contains(g.Detail, "operator maintenance tag") {
		t.Fatalf("undrain gate with a tag %+v", g)
	}
	if g := maintenanceGate(p, "pve01", false, true); !g.OK || !strings.HasPrefix(g.Detail, "for --apply") {
		t.Fatalf("dry undrain gate %+v", g)
	}
	if g := maintenanceGate(p, "pve09", false, false); g.OK {
		t.Fatalf("an unknown node passed %+v", g)
	}
}

func TestDrainRun_AnUnreadableStorageListRefusesAndStopsNothing(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	f.addVM(101, "pve01", 4)
	drHold(f, 102)
	f.profileVal.StoragesErr = "GET /storage: 500 Internal Server Error"
	rep := drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != DrainGateFates {
		t.Fatalf("exit %d halted %q gates %+v", rep.ExitCode, rep.Halted, rep.Gates)
	}
	var gate RebalanceGate
	for _, g := range rep.Gates {
		if g.Name == DrainGateFates {
			gate = g
		}
	}
	if gate.OK || !strings.Contains(gate.Detail, "VM 101") || strings.Contains(gate.Detail, "VM 102") {
		t.Fatalf("fates gate %+v", gate)
	}
	if got, want := drVMs(rep.VMs), []string{"101 undecided", "102 stays planned"}; !slices.Equal(got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if len(f.callsWith("stop ")) != 0 || f.vms[101].state != "started" || f.vms[102].state != "started" {
		t.Fatalf("a drain that couldn't decide stopped a VM: %v", f.calls)
	}

	// A storage read that fails while PVE HA moves VMs keeps the wait going,
	// and the drain stops the VM that can't leave once the read recovers.
	f = newFakeRebalanceIO()
	drMaintenance(f)
	f.addVM(101, "pve01", 4)
	drHold(f, 102)
	f.moving[101] = true
	f.stopSettle = 1
	f.observeHook = func(n int) {
		switch n {
		case 2:
			f.profileVal.StoragesErr = "GET /storage: 500 Internal Server Error"
			delete(f.moving, 101)
		case 3:
			f.profileVal.StoragesErr = ""
			f.vms[101].node = "pve02"
		}
	}
	rep = drRun(f, DrainCommand, true).drain(context.Background())
	if rep.ExitCode != RebalanceExitOK || !slices.Equal(drVMs(rep.VMs), []string{"102 stays stopped"}) {
		t.Fatalf("exit %d halted %q vms %v", rep.ExitCode, rep.Halted, drVMs(rep.VMs))
	}
}

func TestDrainRun_ASoftPinnedVMThatPVEKeepsIsStoppedAtTheTimeout(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	// The soft pin puts only pve01 in its highest priority class.
	f.addVM(101, "pve01", 4).tags += ";" + deploymentTagPrefix + "cf"
	f.pins[101] = rbPin(101, map[string]int{"pve01": 2, "pve02": 1, "pve03": 1})
	drHold(f, 102)
	r := drRun(f, DrainCommand, true)
	r.settle = time.Minute
	rep := r.drain(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Halted != "" {
		t.Fatalf("exit %d halted %q vms %+v", rep.ExitCode, rep.Halted, rep.VMs)
	}
	if got, want := drVMs(rep.VMs), []string{"101 stays stopped", "102 stays stopped"}; !slices.Equal(got, want) {
		t.Fatalf("vms %v, want %v", got, want)
	}
	if !strings.Contains(rep.VMs[0].Reason, "soft node-affinity rule "+naRuleNameFor(101)) {
		t.Fatalf("reason %q", rep.VMs[0].Reason)
	}
	if by, ok := ha.DrainStopped(&f.vms[101].comment); !ok || by != "pve01" || f.vms[101].state != "stopped" {
		t.Fatalf("VM 101 state %q comment %q", f.vms[101].state, f.vms[101].comment)
	}
	if !slices.Equal(rep.Deployments, []string{"cf"}) {
		t.Fatalf("deployments %v", rep.Deployments)
	}

	// The plan warns about such a VM before any wait.
	f = newFakeRebalanceIO()
	f.addVM(101, "pve01", 4)
	f.pins[101] = rbPin(101, map[string]int{"pve01": 2, "pve02": 1, "pve03": 1})
	rep = drRun(f, DrainCommand, false).drain(context.Background())
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "VM 101") ||
		!strings.Contains(rep.Warnings[0], "stops it once the settle timeout runs out") {
		t.Fatalf("warnings %q", rep.Warnings)
	}
}

func TestDrainRun_ASoftPinnedVMStillWaitsWhileAnotherVMThatCouldLeaveStays(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	drMaintenance(f)
	f.addVM(101, "pve01", 4)
	f.pins[101] = rbPin(101, map[string]int{"pve01": 2, "pve02": 1, "pve03": 1})
	// A pin with several nodes in its top class holds nothing.
	f.addVM(103, "pve01", 4)
	f.pins[103] = rbPin(103, map[string]int{"pve01": 2, "pve02": 2, "pve03": 1})
	r := drRun(f, DrainCommand, true)
	r.settle = time.Minute
	rep := r.drain(context.Background())
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "2 VMs that could leave node pve01") {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if len(f.callsWith("stop ")) != 0 {
		t.Fatalf("a drain that timed out with a VM it can't explain stopped a VM: %v", f.calls)
	}
}

func TestUndrainRun_TheRebalanceStopsMovingBeforeTheUndrainLeaseRunsOut(t *testing.T) {
	t.Parallel()
	f := newFakeRebalanceIO()
	rbSkewed(f)
	drStopped(f, 110, "pve01")
	// The start of VM 110 runs long, so the rebalance begins late in the
	// undrain's lease.
	f.drainHook = func(vmid int, stop bool) (bool, error) {
		if vmid == 110 && !stop {
			f.clock = f.clock.Add(100 * time.Minute)
		}
		return true, nil
	}
	r := drRun(f, UndrainCommand, true)
	rr := newRebalanceRun(r.deps, r.rebalance, f)
	rep := r.undrain(context.Background())
	if len(f.lockTTLs) != 1 || f.lockTTLs[0] != r.settle+rr.runLockTTL() {
		t.Fatalf("lease %v, want %v", f.lockTTLs, r.settle+rr.runLockTTL())
	}
	if rep.ExitCode != RebalanceExitOK || rep.Rebalance == nil || rep.Rebalance.Deferred == 0 {
		t.Fatalf("exit %d halted %q rebalance %+v", rep.ExitCode, rep.Halted, rep.Rebalance)
	}
	if len(f.callsWith("migrate ")) != 0 {
		t.Fatalf("the rebalance started a move the undrain's lease could not cover: %v", f.calls)
	}
}
