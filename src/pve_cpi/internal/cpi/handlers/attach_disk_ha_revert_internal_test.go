package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// attachMoveTaskClient is icPVEClient with a node's active move task list.
type attachMoveTaskClient struct {
	*icPVEClient
	tasks   []pve.ActiveTask
	listErr error
	asked   []string
}

func (c *attachMoveTaskClient) ActiveMoveTasks(_ context.Context, node string) ([]pve.ActiveTask, error) {
	c.asked = append(c.asked, node)
	return c.tasks, c.listErr
}

// hostCPUConfig serves the config of VM 100 with a host CPU, whose migration
// domain the HA decision has to prove.
func hostCPUConfig(deps Deps) {
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "host"}, nil
	}}
}

// TestDecideAttachHA_TransientRefusalDefersRatherThanLeave runs the decision
// for a host CPU VM under HA whose HA refusal may pass. The VM keeps its HA
// state and the attach waits, because only a refusal on proven facts takes a
// VM out of HA. The last case proves every node's CPU, so the refusal is
// permanent and the VM leaves HA.
func TestDecideAttachHA_TransientRefusalDefersRatherThanLeave(t *testing.T) {
	offline := func(p *ha.Profile, name string) {
		for i := range p.Nodes {
			if p.Nodes[i].Name == name {
				p.Nodes[i].Online = false
			}
		}
	}
	for name, tc := range map[string]struct {
		setup func(*ha.Profile)
		leave bool
	}{
		"the placed node's CPU is unknown": {setup: func(p *ha.Profile) {
			p.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"pve02", "pve03"}}}
			p.UnknownCPUNodes = []string{"pve01"}
		}},
		"a node of the CPU domain is down": {setup: func(p *ha.Profile) {
			p.CPUDomains = []ha.CPUDomain{
				{Model: "EPYC", Nodes: []string{"pve01", "pve02"}}, {Model: "Xeon", Nodes: []string{"pve03"}},
			}
			offline(p, "pve02")
		}},
		"another node's CPU is unknown": {setup: func(p *ha.Profile) {
			p.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"pve01"}}, {Model: "Xeon",
				Nodes: []string{"pve03"}}}
			p.UnknownCPUNodes = []string{"pve02"}
		}},
		"the storage definitions are unreadable": {setup: func(p *ha.Profile) {
			p.CPUDomains = []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"pve01", "pve02", "pve03"}}}
			p.Storages = nil
			p.StoragesErr = "permission denied"
		}},
		"every CPU is known and differs": {leave: true, setup: func(p *ha.Profile) {
			p.CPUDomains = []ha.CPUDomain{
				{Model: "EPYC", Nodes: []string{"pve01"}}, {Model: "Xeon", Nodes: []string{"pve02"}},
				{Model: "Core", Nodes: []string{"pve03"}},
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			deps, stub, profile := attachRig(config.HAModeAuto)
			stub.recordPin("nfs")
			hostCPUConfig(deps)
			tc.setup(profile)
			h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
			if tc.leave {
				if err != nil || h == nil || h.action != attachHALeave {
					t.Fatalf("h %+v err %v, want the VM to leave HA on a refusal it proved", h, err)
				}
				return
			}
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || h != nil {
				t.Fatalf("h %+v err %v, want a retriable deferral", h, err)
			}
			if stub.resDelCalls != 0 || stub.deleteCalls != 0 {
				t.Fatalf("a deferral must change no HA state, resource deletes %d rule deletes %d",
					stub.resDelCalls, stub.deleteCalls)
			}
		})
	}
}

// narrowingRun decides a narrowing attach of VM 100 and wires its barrier to
// res, with the move task client in place of the rig's client.
func narrowingRun(t *testing.T, deps Deps, stub *attachHAStub) (*attachHA, *attachRunRes, *attachMoveTaskClient) {
	t.Helper()
	mc := &attachMoveTaskClient{icPVEClient: deps.PVE.(*icPVEClient)}
	// The barrier's proof that the attach changed nothing reads the pending
	// config through the nodes service.
	mc.nodesSvc = &icNodesService{}
	deps.PVE = mc
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded || h.newPin == nil {
		t.Fatalf("decision: h %+v err %v, want a guarded narrowing", h, err)
	}
	res := &attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	return h, res, mc
}

// TestAttachHARun_FailedNarrowingOfAPinlessVMKeepsItsHAResource narrows a VM
// that had no pin and fails the attach before it changes anything. The barrier
// removes the pin it wrote and nothing else, so the VM keeps its HA resource
// and goes back to started.
func TestAttachHARun_FailedNarrowingOfAPinlessVMKeepsItsHAResource(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	h, res, mc := narrowingRun(t, deps, stub)
	if h.oldPin != nil {
		t.Fatalf("the VM must start with no pin, got %v", h.oldPin)
	}
	attachErr := errors.New("qmp: device_add failed")
	err := h.run(context.Background(), func(context.Context) (string, error) {
		if _, ok := stub.rules["bosh-na-100"]; !ok {
			t.Error("the narrowed pin must be written before the attach")
		}
		res.log.add("attach")
		return "pve01", attachErr
	})
	if !errors.Is(err, attachErr) {
		t.Fatalf("err = %v, want the attach failure", err)
	}
	if _, ok := stub.rules["bosh-na-100"]; ok {
		t.Error("the barrier must remove the pin it wrote for a VM that had none")
	}
	if res.gone || stub.resDelCalls != 0 || slices.Contains(res.log.events, "leave") {
		t.Fatalf("the VM's HA resource must survive the revert: gone %v deletes %d events %v", res.gone,
			stub.resDelCalls, res.log.events)
	}
	if res.state != "started" {
		t.Errorf("state %q, want the barrier to put the VM back to started", res.state)
	}
	if !slices.Equal(mc.asked, []string{"pve01"}) {
		t.Errorf("move tasks asked on %v, want pve01 before the revert", mc.asked)
	}
}

// TestAttachHARun_RevertKeepsAPinSomeoneElseChanged changes the pin while the
// failed attach runs. The revert does not remove a rule this attach did not
// write, and it never removes the HA resource.
func TestAttachHARun_RevertKeepsAPinSomeoneElseChanged(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	h, res, _ := narrowingRun(t, deps, stub)
	err := h.run(context.Background(), func(context.Context) (string, error) {
		pin := stub.rules["bosh-na-100"]
		nodes := "pve01"
		pin.Nodes = &nodes
		stub.rules["bosh-na-100"] = pin
		return "pve01", errors.New("qmp: device_add failed")
	})
	if err == nil {
		t.Fatal("the attach failure must reach the caller")
	}
	pin, ok := stub.rules["bosh-na-100"]
	if !ok || pin.Nodes == nil || *pin.Nodes != "pve01" {
		t.Fatalf("the changed pin must stay, got %v", pin)
	}
	if res.gone || stub.resDelCalls != 0 {
		t.Fatalf("the VM's HA resource must survive, gone %v deletes %d", res.gone, stub.resDelCalls)
	}
}

// TestAttachHARun_RevertWaitsForARunningTransfer fails a narrowing attach
// while a task still moves the VM or one of its disks. The revert keeps the
// narrower pin and the error is retriable, because widening the pin then
// could let PVE HA place the VM where the moved disk does not reach. With no
// task running the old pin comes back.
func TestAttachHARun_RevertWaitsForARunningTransfer(t *testing.T) {
	for name, tc := range map[string]struct {
		lock    string
		tasks   []pve.ActiveTask
		listErr error
		revert  bool
	}{
		"a disk moved to the VM":  {tasks: []pve.ActiveTask{{ID: "90030-scsi0>100-scsi1", Type: "qmmove"}}},
		"a disk moved off the VM": {tasks: []pve.ActiveTask{{ID: "100", Type: "qmmove"}}},
		"a migration lock":        {lock: "migrate"},
		"an unread task list":     {listErr: errors.New("Sys.Audit missing")},
		"another VM's move":       {tasks: []pve.ActiveTask{{ID: "101-scsi0>102-scsi1", Type: "qmmove"}}, revert: true},
		"no task":                 {revert: true},
	} {
		t.Run(name, func(t *testing.T) {
			deps, stub, _ := attachRig(config.HAModeAuto)
			stub.recordPin("nfs")
			h, res, mc := narrowingRun(t, deps, stub)
			mc.tasks, mc.listErr = tc.tasks, tc.listErr
			mc.qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
				cfg := map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G"}
				if tc.lock != "" {
					cfg["lock"] = tc.lock
				}
				return cfg, nil
			}}
			attachErr := errors.New("qmp: device_add failed")
			err := h.run(context.Background(), func(context.Context) (string, error) { return "pve01", attachErr })
			if !errors.Is(err, attachErr) {
				t.Fatalf("err = %v, want the attach failure kept", err)
			}
			pin := stub.rules["bosh-na-100"]
			restored := pin.Nodes != nil && *pin.Nodes == "pve01,pve02,pve03"
			if restored != tc.revert {
				t.Fatalf("pin nodes %v, want the old pin back %v", derefString(pin.Nodes), tc.revert)
			}
			running := tc.lock != "" || (len(tc.tasks) > 0 && !tc.revert)
			if running != cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, retriable want %v", err, running)
			}
			if running && !strings.Contains(err.Error(), "still running") {
				t.Errorf("err = %v, want it to say a task is still running", err)
			}
			if res.gone || stub.resDelCalls != 0 {
				t.Fatalf("the VM's HA resource must survive, gone %v deletes %d", res.gone, stub.resDelCalls)
			}
		})
	}
}

// TestAttachMovingReadsTheLockAndTheMoveTasks covers each answer attachMoving
// gives the barrier.
func TestAttachMovingReadsTheLockAndTheMoveTasks(t *testing.T) {
	cfgOf := func(cfg map[string]any, err error) *icQEMUService {
		return &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) { return cfg, err }}
	}
	plain := map[string]any{"scsi0": "nfs:vm-100-disk-0"}
	for name, tc := range map[string]struct {
		client  pve.Client
		want    bool
		wantErr bool
	}{
		"a lock": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(map[string]any{
			"lock": "migrate"}, nil)}}, want: true},
		"a move off the VM": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(plain, nil)},
			tasks: []pve.ActiveTask{{ID: "100-scsi1>200-scsi0"}}}, want: true},
		"a move onto the VM": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(plain, nil)},
			tasks: []pve.ActiveTask{{ID: "200-scsi0>100-scsi1"}}}, want: true},
		"another VM's move": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(plain, nil)},
			tasks: []pve.ActiveTask{{ID: "1000"}, {ID: "200-scsi0>1000-scsi1"}}}},
		"nothing running": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(plain, nil)}}},
		"an unreadable config": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(nil,
			errors.New("connection reset"))}}, wantErr: true},
		"an unreadable task list": {client: &attachMoveTaskClient{icPVEClient: &icPVEClient{qemuSvc: cfgOf(plain,
			nil)}, listErr: errors.New("Sys.Audit missing")}, wantErr: true},
		"a client with no task reader": {client: &icPVEClient{qemuSvc: cfgOf(plain, nil)}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := &attachHA{deps: Deps{PVE: tc.client}, vmid: 100}
			got, err := h.attachMoving(context.Background(), "pve01")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("moving %v err %v, want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
