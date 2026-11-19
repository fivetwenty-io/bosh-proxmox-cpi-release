package handlers

import (
	"context"
	"errors"
	"slices"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
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
