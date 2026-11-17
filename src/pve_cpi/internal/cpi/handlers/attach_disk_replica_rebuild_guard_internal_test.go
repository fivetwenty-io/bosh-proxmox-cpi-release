package handlers

import (
	"errors"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// errNeedsSysAudit is a task listing refused for want of Sys.Audit.
var errNeedsSysAudit = errors.New("listing tasks on node pve01 needs Sys.Audit on /nodes/pve01")

// TestPrepareAttachReplicaRebuild_LeaveRetiresTheJobItself is an attach of a
// parked NFS disk whose HA decision takes the replicate VM out of HA, because
// only pve01 reaches the disk's storage. The removal from HA retires the job
// by itself, so the rebuild neither removes the job nor narrows the pin.
func TestPrepareAttachReplicaRebuild_LeaveRetiresTheJobItself(t *testing.T) {
	r := newRebuildRig()
	for i := range r.profile.Storages {
		if r.profile.Storages[i].Name == "nfs" {
			r.profile.Storages[i].Shared, r.profile.Storages[i].Nodes = true, []string{"pve01"}
		}
	}
	pin := r.pinComment()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb != nil {
		t.Fatalf("rebuild %+v err %v, want none for an attach that leaves HA", rb, err)
	}
	if r.repl.called("delete:") || len(r.repl.jobs) != 1 || r.pinComment() != pin || r.pinNodes() != "pve01" {
		t.Fatalf("calls %v jobs %v pin %q over %q; the leave path retires the job, not the rebuild",
			r.repl.calls, r.repl.jobs, r.pinComment(), r.pinNodes())
	}
}

// TestPrepareAttachReplicaRebuild_HAWriteRefusalKeepsTheJob is an attach whose
// HA decision refuses because the token lacks Sys.Console. The refusal comes
// before the rebuild removes anything, so the job and the pin stay.
func TestPrepareAttachReplicaRebuild_HAWriteRefusalKeepsTheJob(t *testing.T) {
	r := newRebuildRig()
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	r.profile.Effective, r.profile.SysConsole = ha.EffectiveObserveOnly, false
	pin := r.pinComment()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if rb != nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(),
		"Sys.Console") {
		t.Fatalf("rebuild %+v err %v, want the retriable refusal naming Sys.Console", rb, err)
	}
	if r.repl.called("delete:") || len(r.repl.jobs) != 1 || r.pinComment() != pin || r.pinNodes() != "pve01,pve02" {
		t.Fatalf("calls %v jobs %v pin %q over %q; a refused attach keeps the job and the pin", r.repl.calls,
			r.repl.jobs, r.pinComment(), r.pinNodes())
	}
}

// TestPrepareAttachReplicaRebuild_VMOutOfHAGetsNoRebuild is a candidate attach
// to a VM that has its pin and its job but no HA resource of this Director.
// The leave path's resume owns that VM, so the rebuild changes nothing.
func TestPrepareAttachReplicaRebuild_VMOutOfHAGetsNoRebuild(t *testing.T) {
	r := newRebuildRig()
	r.stub.missing = true
	pin := r.pinComment()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb != nil {
		t.Fatalf("rebuild %+v err %v, want none for a VM out of HA", rb, err)
	}
	if r.repl.called("delete:") || len(r.repl.jobs) != 1 || r.pinComment() != pin {
		t.Fatalf("calls %v jobs %v pin %q; nothing changes", r.repl.calls, r.repl.jobs, r.pinComment())
	}
}

// TestPrepareAttachReplicaRebuild_WaitsForAMoveInFlight checks that the job
// stays while the VM may be migrating onto the partner's copy, which the
// removal would delete. The pin keeps the rebuild token, so a later call
// finds the rebuild.
func TestPrepareAttachReplicaRebuild_WaitsForAMoveInFlight(t *testing.T) {
	cases := map[string]struct {
		setup  func(r *replicaRig)
		reason string
	}{
		"migrate lock": {
			setup:  func(r *replicaRig) { r.cfg["lock"] = "migrate" },
			reason: "config lock",
		},
		"disk move task": {
			setup: func(r *replicaRig) {
				r.tasks.moves["pve01"] = []pve.ActiveTask{{UPID: "UPID:pve01:1:1:1:qmmove:100:root@pam:",
					Type: "qmmove", ID: "100", Status: "RUNNING"}}
			},
			reason: "disk move",
		},
		"migration running": {
			setup: func(r *replicaRig) {
				r.tasks.migrated("pve01", 100, time.Now().Add(-time.Minute), time.Time{}, "")
			},
			reason: "is running",
		},
		"task listing unreadable": {
			setup:  func(r *replicaRig) { r.tasks.migrations, r.tasks.err["pve01"] = nil, errNeedsSysAudit },
			reason: "Sys.Audit",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRebuildRig()
			tc.setup(r)
			rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
			if rb != nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
				!strings.Contains(err.Error(), "may be migrating") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("rebuild %+v err %v, want the retriable deferral naming %q", rb, err, tc.reason)
			}
			if r.repl.called("delete:") || len(r.repl.jobs) != 1 {
				t.Fatalf("calls %v jobs %v; the job stays while the VM may be moving", r.repl.calls, r.repl.jobs)
			}
			if p, ok := ha.ReplicaRebuild(r.pinComment()); !ok || p != "pve02" || r.pinNodes() != "pve01" {
				t.Fatalf("pin %q over %q; the narrowed pin keeps the rebuild token", r.pinComment(), r.pinNodes())
			}
		})
	}
}

// TestPrepareAttachReplicaRebuild_ResumesAfterADeferredRemoval runs a rebuild
// whose removal waited for a migration, then the retry once the migration
// has ended: the retry removes the job.
func TestPrepareAttachReplicaRebuild_ResumesAfterADeferredRemoval(t *testing.T) {
	r := newRebuildRig()
	r.cfg["lock"] = "migrate"
	if _, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk()); err == nil {
		t.Fatal("the removal ran while the VM held the migrate lock")
	}
	delete(r.cfg, "lock")
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || !rb.removed || !r.repl.called("delete:100-0:force=false") {
		t.Fatalf("rebuild %+v err %v calls %v; the retry removes the job", rb, err, r.repl.calls)
	}
}

// TestAttachReplicaRebuildMarksOnlyARemovedJob checks that only a rebuild
// whose job is gone or on its way out marks the attach's context, so a
// rebuild whose job stayed keeps that job in the attach's refusals.
func TestAttachReplicaRebuildMarksOnlyARemovedJob(t *testing.T) {
	var none *attachReplicaRebuild
	if replicaRebuildInFlight(none.marked(crhCtx())) {
		t.Fatal("an attach with no rebuild is marked")
	}
	kept := newRebuildRig()
	kept.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	rd := parkedNFSDisk()
	rd.volid = "nfs:100/vm-100-disk-1.qcow2"
	rd.holder = &pve.DiskHolder{Found: true, IsParker: false, VMID: 100, Node: "pve01"}
	rb, err := prepareAttachReplicaRebuild(crhCtx(), kept.deps, "100", rd)
	if err != nil || rb == nil || rb.removed || replicaRebuildInFlight(rb.marked(crhCtx())) {
		t.Fatalf("rebuild %+v err %v; a rebuild whose job stayed must not mark the attach", rb, err)
	}
	gone := newRebuildRig()
	gone.repl.jobs = nil
	gone.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	rb, err = prepareAttachReplicaRebuild(crhCtx(), gone.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || !rb.removed || !replicaRebuildInFlight(rb.marked(crhCtx())) {
		t.Fatalf("rebuild %+v err %v; a rebuild whose job is gone marks the attach", rb, err)
	}
	removed := newRebuildRig()
	rb, err = prepareAttachReplicaRebuild(crhCtx(), removed.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || !rb.removed || !replicaRebuildInFlight(rb.marked(crhCtx())) {
		t.Fatalf("rebuild %+v err %v; a rebuild that removed the job marks the attach", rb, err)
	}
}
