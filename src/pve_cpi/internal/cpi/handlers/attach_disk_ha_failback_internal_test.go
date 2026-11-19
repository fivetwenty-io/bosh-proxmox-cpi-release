package handlers

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func failbackFlag(b bool) *sdkclient.PVEBool {
	v := sdkclient.PVEBool(b)
	return &v
}

// holdAttach decides the attach of a node-local disk to VM 100 under the hold
// policy, which pins the VM to its node, and wires the barrier to res.
func holdAttach(t *testing.T, res func(comment string) *attachRunRes) (*attachHA, *attachRunRes) {
	t.Helper()
	deps, stub, _ := attachRig(config.HAModeAuto)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	if h.decision.Failback {
		t.Fatal("a VM that gains a node-local disk must not keep failback")
	}
	r := res(stub.comment)
	h.narrow = barrierView(r)
	return h, r
}

func TestAttachHARun_LocalDiskTurnsFailbackOffBeforeTheAttach(t *testing.T) {
	h, res := holdAttach(t, func(comment string) *attachRunRes {
		return &attachRunRes{log: &stagingLog{}, state: "started", comment: comment, failback: failbackFlag(true)}
	})
	err := h.run(context.Background(), func(context.Context) (string, error) {
		if res.failback == nil || res.failback.Bool() {
			t.Error("the disk lands while the resource still has failback on")
		}
		res.log.add("attach")
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.log.events) == 0 || res.log.events[0] != "failback:false" ||
		!slices.Contains(res.log.events, "attach") || slices.Contains(res.log.events, "failback:true") {
		t.Fatalf("events %v, want failback turned off first and never back on", res.log.events)
	}
}

func TestAttachHARun_UnsetFailbackReadsAsOnAndIsTurnedOff(t *testing.T) {
	h, res := holdAttach(t, func(comment string) *attachRunRes {
		return &attachRunRes{log: &stagingLog{}, state: "started", comment: comment}
	})
	if err := h.run(context.Background(), func(context.Context) (string, error) { return "pve01", nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.failback == nil || res.failback.Bool() {
		t.Fatalf("failback %v, want an explicit off", res.failback)
	}
}

func TestAttachHARun_FailbackWriteFailureStopsTheAttach(t *testing.T) {
	h, res := holdAttach(t, func(comment string) *attachRunRes {
		return &attachRunRes{log: &stagingLog{}, state: "started", comment: comment, failback: failbackFlag(true),
			failbackErr: errors.New("cfs lock timeout (HTTP500)")}
	})
	attached := false
	err := h.run(context.Background(), func(context.Context) (string, error) { attached = true; return "pve01", nil })
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable error", err)
	}
	if attached || len(res.log.events) != 0 {
		t.Fatalf("attached %v events %v; nothing may change before failback is off", attached, res.log.events)
	}
}

func TestAttachHARun_SharedDiskTurnsFailbackOnAfterTheAttach(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded || !h.decision.Failback {
		t.Fatalf("decision: h %+v err %v, want a guarded attach that keeps failback", h, err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment, failback: failbackFlag(false)}
	h.narrow = barrierView(res)
	if err := h.run(context.Background(), func(context.Context) (string, error) {
		sl.add("attach")
		return "pve01", nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"request:ignored", "attach", "request:started", "failback:true"}
	if !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
}

func TestAttachHARun_FailbackOffKeyKeepsASharedVMAtZero(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	off := false
	deps.Config.HA.Failback = &off
	stub.recordPin("nfs")
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded || h.decision.Failback {
		t.Fatalf("decision: h %+v err %v, want a guarded attach without failback", h, err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment, failback: failbackFlag(false)}
	h.narrow = barrierView(res)
	if err := h.run(context.Background(), func(context.Context) (string, error) { return "pve01", nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slices.Contains(sl.events, "failback:true") || slices.Contains(sl.events, "failback:false") {
		t.Fatalf("events %v; a resource already at 0 needs no write", sl.events)
	}
}

// TestAttachHARun_SkippedDecisionTurnsFailbackOffBeforeALocalDisk attaches a
// node-local disk to a hold VM whose pin already records the disk's storage,
// so the attach skips the HA decision. The resource reads failback on, which a
// resource can carry from an operator or an older release, and the attach must
// still turn it off before the disk lands.
func TestAttachHARun_SkippedDecisionTurnsFailbackOffBeforeALocalDisk(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.pin(true, "pve01", "nfs", testPinStorage)
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil || h.action != attachHANone {
		t.Fatalf("decision: h %+v err %v, want a skipped decision that still guards failback", h, err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	if err := h.run(context.Background(), func(context.Context) (string, error) {
		if res.failback == nil || res.failback.Bool() {
			t.Error("the node-local disk lands while the resource still has failback on")
		}
		sl.add("attach")
		return "pve01", nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"failback:false", "attach"}; !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
}

// TestAttachHARun_SkippedDecisionFailbackWriteFailureStopsTheAttach fails a
// skipped-decision attach of a node-local disk whose failback cannot be
// turned off, before anything changes.
func TestAttachHARun_SkippedDecisionFailbackWriteFailureStopsTheAttach(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.pin(true, "pve01", "nfs", testPinStorage)
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	res := &attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment,
		failbackErr: errors.New("cfs lock timeout (HTTP500)")}
	h.narrow = barrierView(res)
	attached := false
	err = h.run(context.Background(), func(context.Context) (string, error) { attached = true; return "pve01", nil })
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || attached {
		t.Fatalf("err = %v attached %v, want a retriable error before the attach", err, attached)
	}
}

// TestDecideAttachHA_SkippedDecisionLeavesFailbackForASharedDisk keeps the
// skip as it was for a disk on shared storage, whose failback may stay on.
func TestDecideAttachHA_SkippedDecisionLeavesFailbackForASharedDisk(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs", "nfs2")
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil || h != nil {
		t.Fatalf("decision: h %+v err %v, want no HA action", h, err)
	}
}

// sharedResCluster answers the HA resource of VM 100 from res, the resource
// an attach's barrier reads and writes, and the rest from the attach stub, so
// a reconcile in another process and an attach act on one resource.
type sharedResCluster struct {
	*attachHAStub
	res *attachRunRes
}

func (c *sharedResCluster) GetHaResources(ctx context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	return c.res.GetHaResources(ctx, sid)
}

func (c *sharedResCluster) UpdateHaResources(ctx context.Context, sid string,
	p *cluster.UpdateHaResourcesParams,
) error {
	return c.res.UpdateHaResources(ctx, sid, p)
}

// raceRig is a hold attach of a node-local disk to VM 100, whose pin is
// non-strict over three nodes and records nfs, and the deps of a reconcile in
// another process that acts on the same HA resource and VM config.
type raceRig struct {
	h      *attachHA
	res    *attachRunRes
	sl     *stagingLog
	vmCfg  map[string]any
	rdeps  Deps
	reads  int
	onRead func(read int) map[string]any
}

func newRaceRig(t *testing.T, failback *sdkclient.PVEBool) *raceRig {
	t.Helper()
	deps, stub, _ := attachRig(config.HAModeAuto)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.recordPin("nfs")
	r := &raceRig{sl: &stagingLog{},
		vmCfg: map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3", "tags": haManagedTag}}
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return maps.Clone(r.vmCfg), nil
	}}
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil || h.action != attachHAGuarded || h.decision.Failback {
		t.Fatalf("decision: h %+v err %v, want a guarded attach without failback", h, err)
	}
	r.h = h
	r.res = &attachRunRes{log: r.sl, state: "started", comment: stub.comment, failback: failback}
	h.narrow = barrierView(r.res)
	r.rdeps = deps
	r.rdeps.Logger = log.NewNopLogger()
	r.rdeps.PVE = &icPVEClient{clusterSvc: &sharedResCluster{attachHAStub: stub, res: r.res},
		qemuSvc: &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
			r.reads++
			if r.onRead != nil {
				if cfg := r.onRead(r.reads); cfg != nil {
					return cfg, nil
				}
			}
			return maps.Clone(r.vmCfg), nil
		}}}
	return r
}

// attach is the rest of the attach: it lands the node-local disk in the VM's
// config and checks that failback is off when it does.
func (r *raceRig) attach(t *testing.T) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if ha.FailbackOf(r.res.failback) {
			t.Error("the node-local disk lands while the resource has failback on")
		}
		r.vmCfg["scsi1"] = testPinStorage + ":vm-100-disk-1,size=1G"
		r.sl.add("attach")
		return "pve01", nil
	}
}

func (r *raceRig) reconcile() {
	reconcileVMFailback(context.Background(), r.rdeps, "set_vm_metadata", 100,
		pve.VMLocation{Node: "pve01", Found: true, Tags: haManagedTag}, r.rdeps.Logger)
}

// TestAttachHARun_ReconcileThatReadBeforeTheAttachCannotRaiseFailback reads
// the resource and the config in a reconcile before the attach, runs the whole
// attach, and only then lets the reconcile write. The reconcile decided from a
// config without the node-local disk, so it wants failback on, and the write
// must not land.
func TestAttachHARun_ReconcileThatReadBeforeTheAttachCannotRaiseFailback(t *testing.T) {
	r := newRaceRig(t, failbackFlag(false))
	r.onRead = func(read int) map[string]any {
		if read != 1 {
			return nil
		}
		stale := maps.Clone(r.vmCfg)
		if err := r.h.run(context.Background(), r.attach(t)); err != nil {
			t.Errorf("attach: %v", err)
		}
		return stale
	}
	r.reconcile()
	if ha.FailbackOf(r.res.failback) {
		t.Fatalf("failback is on after the race, events %v", r.sl.events)
	}
	if slices.Contains(r.sl.events, "failback:true") {
		t.Fatalf("events %v; the reconcile's write must not land", r.sl.events)
	}
}

// TestAttachHARun_ReconcileBeforeTheBarrierIsUndoneBeforeTheDiskLands runs a
// whole reconcile right after the attach turned failback off and before the
// barrier holds the resource. The reconcile sees a config without the new
// disk and turns failback on, and the attach must turn it off again before
// the disk lands.
func TestAttachHARun_ReconcileBeforeTheBarrierIsUndoneBeforeTheDiskLands(t *testing.T) {
	r := newRaceRig(t, nil)
	r.res.afterFailbackWrite = r.reconcile
	if err := r.h.run(context.Background(), r.attach(t)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !slices.Contains(r.sl.events, "failback:true") {
		t.Fatalf("events %v; the reconcile was meant to turn failback on in between", r.sl.events)
	}
	if ha.FailbackOf(r.res.failback) {
		t.Fatalf("failback is on after the attach, events %v", r.sl.events)
	}
}

// TestAttachHARun_SkippedDecisionTurnsFailbackOffAgainAfterTheDiskLands has a
// call in another process turn failback on while a skipped-decision attach of
// a node-local disk lands it, and the attach turns it off again.
func TestAttachHARun_SkippedDecisionTurnsFailbackOffAgainAfterTheDiskLands(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	stub.pin(true, "pve01", "nfs", testPinStorage)
	h, err := decideAttachHA(context.Background(), deps, "100", localDisk())
	if err != nil || h == nil || !h.failbackOff {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	if err := h.run(context.Background(), func(context.Context) (string, error) {
		res.failback = failbackFlag(true)
		sl.add("attach")
		return "pve01", nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"failback:false", "attach", "failback:false"}; !slices.Equal(sl.events, want) {
		t.Fatalf("events %v, want %v", sl.events, want)
	}
}

// TestAttachHARun_ReplicateTaggedVMKeepsFailbackOffWithASharedDisk attaches a
// shared disk to a VM with the replicate tag whose volumes are all shared.
// The decision alone would allow failback, and the attach must apply the
// replica checks the reconcile makes and leave it off.
func TestAttachHARun_ReplicateTaggedVMKeepsFailbackOffWithASharedDisk(t *testing.T) {
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3",
			"tags": haManagedTag + ";" + haReplicaTag}, nil
	}}
	h, err := decideAttachHA(context.Background(), deps, "100", narrowingDisk())
	if err != nil || h == nil || h.action != attachHAGuarded || !h.replicaTagged {
		t.Fatalf("decision: h %+v err %v, want a guarded attach of a replicate-tagged VM", h, err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: stub.comment, failback: failbackFlag(false)}
	h.narrow = barrierView(res)
	if err := h.run(context.Background(), func(context.Context) (string, error) { return "pve01", nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slices.Contains(sl.events, "failback:true") || ha.FailbackOf(res.failback) {
		t.Fatalf("events %v; a replicate-tagged VM keeps failback off", sl.events)
	}
}
