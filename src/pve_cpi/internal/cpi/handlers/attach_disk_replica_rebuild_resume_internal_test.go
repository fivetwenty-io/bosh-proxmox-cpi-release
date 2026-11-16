package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestReconcileVMReplica_WaitsForAJobMarkedForRemoval is a rebuild whose
// removal timed out. The job is marked for removal and the token is on the
// pin. A touch call keeps the token and sets nothing up while pvesr has not
// removed the job, and once the job is gone the next touch call sets it up
// again and removes the token.
func TestReconcileVMReplica_WaitsForAJobMarkedForRemoval(t *testing.T) {
	r := newRebuildRig()
	r.repl.removeOnSchedule = false
	r.repl.jobs[0].RemoveJob = "full"
	r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p, ok := ha.ReplicaRebuild(r.pinComment()); !ok || p != "pve02" || len(r.repl.created) != 0 {
		t.Fatalf("pin %q created %v; a job pvesr has not removed yet keeps the token and gets no new job",
			r.pinComment(), r.repl.created)
	}
	r.repl.jobs = nil
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ha.ReplicaRebuild(r.pinComment()); ok || len(r.repl.created) != 1 ||
		r.repl.created[0].Target != "pve02" {
		t.Fatalf("pin %q created %v; once the job is gone it is set up again and the token goes",
			r.pinComment(), r.repl.created)
	}
}

// TestAttachReplicaRebuildFinish_AfterTheVMLeftHA checks that a rebuild
// whose attach took the VM out of HA sets nothing up, because the removal
// from HA retired the replica on purpose, and that a token left on a pin is
// removed.
func TestAttachReplicaRebuildFinish_AfterTheVMLeftHA(t *testing.T) {
	t.Run("pin removed", func(t *testing.T) {
		r := newRebuildRig()
		rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
		if err != nil || rb == nil {
			t.Fatalf("rebuild %+v err %v", rb, err)
		}
		r.tags = "bosh-cpi"
		r.stub.missing = true
		delete(r.stub.rules, "bosh-na-100")
		if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(r.repl.jobs) != 0 || len(r.repl.created) != 0 || hasReplicaTag(r.tags) {
			t.Fatalf("jobs %v created %v tags %q; a VM out of HA gets no job and no replicate tag back",
				r.repl.jobs, r.repl.created, r.tags)
		}
	})
	cases := map[string]func(r *replicaRig){
		"HA resource gone": func(r *replicaRig) { r.stub.missing = true },
		"HA resource foreign": func(r *replicaRig) {
			r.stub.comment = haIdentity{Owner: "other-director"}.marker()
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRebuildRig()
			rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
			if err != nil || rb == nil {
				t.Fatalf("rebuild %+v err %v", rb, err)
			}
			mutate(r)
			if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(r.repl.created) != 0 || len(r.repl.jobs) != 0 {
				t.Fatalf("created %v jobs %v; nothing is set up for a VM out of this Director's HA",
					r.repl.created, r.repl.jobs)
			}
			if _, ok := ha.ReplicaRebuild(r.pinComment()); ok {
				t.Fatalf("pin %q keeps the token", r.pinComment())
			}
		})
	}
	t.Run("a VM still in HA whose tag went gets both back", func(t *testing.T) {
		r := newRebuildRig()
		rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
		if err != nil || rb == nil {
			t.Fatalf("rebuild %+v err %v", rb, err)
		}
		r.tags = "bosh-cpi;" + haManagedTag
		if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(r.repl.created) != 1 || !hasReplicaTag(r.tags) {
			t.Fatalf("created %v tags %q; a VM under this Director's HA keeps its replica", r.repl.created, r.tags)
		}
	})
}

// TestPrepareAttachReplicaRebuild_RetryAfterTheDiskLandedFinishesTheRebuild
// is the retry of an attach whose disk landed and whose job could not be set
// up again. The disk is in the VM now, so it needs no new rebuild, but the
// token on the pin makes the retry finish the one the first try started, as
// its error says.
func TestPrepareAttachReplicaRebuild_RetryAfterTheDiskLandedFinishesTheRebuild(t *testing.T) {
	r := newRebuildRig()
	r.repl.jobs = nil
	r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	rd := parkedNFSDisk()
	rd.volid = "nfs:100/vm-100-disk-1.qcow2"
	rd.holder = &pve.DiskHolder{Found: true, IsParker: false, VMID: 100, Node: "pve01"}
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", rd)
	if err != nil || rb == nil || rb.partner != "pve02" {
		t.Fatalf("rebuild %+v err %v, want the pending one to pve02", rb, err)
	}
	if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ha.ReplicaRebuild(r.pinComment()); ok || len(r.repl.created) != 1 {
		t.Fatalf("pin %q created %v; the retry sets the job up and removes the token", r.pinComment(),
			r.repl.created)
	}
}

// TestSetUpReplica_KeptJobCarriesTheJournalGeneration is a rebuild whose
// removal was recorded in the VM's journal record and never reached PVE. The
// setup keeps the job and writes its marker for the generation the journal
// confirms, so the next verified sync can widen the pin.
func TestSetUpReplica_KeptJobCarriesTheJournalGeneration(t *testing.T) {
	r := newRebuildRig()
	r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	handle := replicaHandle(t, time.Now().Add(-time.Hour))
	if err := handle.RetireReplicationJob(); err != nil {
		t.Fatal(err)
	}
	if err := setUpReplica(crhCtx(), r.deps, log.NewNopLogger(), "attach_disk", "pve01", 100, replicaDecision(),
		r.profile, handle); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rep := handle.Record().Replication
	marker, ok := parseReplicaJobMarker(r.repl.jobs[0].Comment)
	if !ok || rep.Job.Generation != marker.Generation || len(r.repl.created) != 0 {
		t.Fatalf("journal generation %d, marker %+v, created %v; the kept job carries the journal's generation",
			rep.Job.Generation, marker, r.repl.created)
	}
}

// TestPrepareAttachReplicaRebuild_RefusedAttachKeepsTheJob checks that an
// attach its own checks refuse, here for the VM's snapshots, never removes
// the VM's job or narrows its pin.
func TestPrepareAttachReplicaRebuild_RefusedAttachKeepsTheJob(t *testing.T) {
	r := newRebuildRig()
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	pin := r.pinComment()
	r.deps.PVE.(*attachRunClient).qemu.snapshots = []map[string]any{
		{"name": "before-upgrade"}, {"name": "current"},
	}
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err == nil || rb != nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("rebuild %+v err %v, want the snapshot refusal", rb, err)
	}
	if r.repl.called("delete:") || len(r.repl.jobs) != 1 || r.pinComment() != pin || r.pinNodes() != "pve01,pve02" {
		t.Fatalf("calls %v jobs %v pin %q over %q; a refused attach keeps the job and the pin", r.repl.calls,
			r.repl.jobs, r.pinComment(), r.pinNodes())
	}
}

// TestPrepareAttachReplicaRebuild_VMOnANodeItsPinDoesNotName checks that a
// VM running on a node its pin names neither as source nor as partner gets
// no rebuild, so neither the pin nor the job moves to that node.
func TestPrepareAttachReplicaRebuild_VMOnANodeItsPinDoesNotName(t *testing.T) {
	r := newRebuildRig()
	client := r.deps.PVE.(*attachRunClient)
	stub := client.cluster.(taggedIndexStub)
	stub.node = "pve03"
	r.deps.PVE = &attachRunClient{lrClient: client.lrClient, cluster: stub}
	pin := r.pinComment()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb != nil {
		t.Fatalf("rebuild %+v err %v, want none", rb, err)
	}
	if r.repl.called("delete:") || r.pinComment() != pin || r.pinNodes() != "pve01" {
		t.Fatalf("calls %v pin %q over %q; nothing changes", r.repl.calls, r.pinComment(), r.pinNodes())
	}
}

// TestPrepareAttachReplicaRebuild_VMOnItsPartnerWaitsForTheReconcile checks
// that a VM which failed over to its partner has its pin reconciled first,
// and that a reconcile which cannot narrow the pin makes the attach
// retriable without removing the job.
func TestPrepareAttachReplicaRebuild_VMOnItsPartnerWaitsForTheReconcile(t *testing.T) {
	r := newRebuildRig()
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	client := r.deps.PVE.(*attachRunClient)
	stub := client.cluster.(taggedIndexStub)
	stub.node = "pve02"
	r.deps.PVE = &attachRunClient{lrClient: client.lrClient, cluster: stub}
	r.deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		return nil, cpierrors.Retriable("profile unavailable")
	}
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || rb != nil ||
		!strings.Contains(err.Error(), "after its failover") {
		t.Fatalf("rebuild %+v err %v, want the failover's retriable error", rb, err)
	}
	if r.repl.called("delete:") || r.pinNodes() != "pve01,pve02" {
		t.Fatalf("calls %v pin over %q; the job stays until the pin names the node the VM runs on",
			r.repl.calls, r.pinNodes())
	}
}

// strandedRig is the rebuild rig with VM 100 in the shape a removal from HA
// under an earlier release left. The VM keeps the replicate tag and this
// Director's job, and has no HA-managed tag, no pin, and no HA resource.
func strandedRig() *replicaRig {
	r := newRebuildRig()
	r.tags = "bosh-cpi;" + haReplicaTag
	delete(r.stub.rules, "bosh-na-100")
	r.stub.missing = true
	return r
}

func TestRetireStrandedReplica(t *testing.T) {
	t.Run("a touch call retires the job and removes the tag", func(t *testing.T) {
		r := strandedRig()
		loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
		if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
			log.NewNopLogger()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !r.repl.called("delete:100-0:force=false") || r.repl.called("delete:100-0:force=true") ||
			len(r.repl.jobs) != 0 || hasReplicaTag(r.tags) {
			t.Fatalf("calls %v jobs %v tags %q; the job goes with cleanup and the tag goes", r.repl.calls,
				r.repl.jobs, r.tags)
		}
	})
	t.Run("an attach retires the job and needs no rebuild", func(t *testing.T) {
		r := strandedRig()
		rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
		if err != nil || rb != nil {
			t.Fatalf("rebuild %+v err %v, want none", rb, err)
		}
		if len(r.repl.jobs) != 0 || hasReplicaTag(r.tags) {
			t.Fatalf("jobs %v tags %q; the stranded job and tag go before the reassignment", r.repl.jobs, r.tags)
		}
		if _, ok := r.stub.rules["bosh-na-100"]; ok {
			t.Fatalf("a pin was written for a VM out of HA")
		}
	})
	t.Run("a foreign job stays", func(t *testing.T) {
		r := strandedRig()
		r.repl.jobs = nil
		r.repl.addJob("100-0", "pve01", "pve02", "operator job")
		loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
		if err := retireStrandedReplica(crhCtx(), r.deps, log.NewNopLogger(), "set_vm_metadata", "100", 100,
			loc); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.repl.called("delete:") || len(r.repl.jobs) != 1 {
			t.Fatalf("calls %v jobs %v; a job without this Director's marker stays", r.repl.calls, r.repl.jobs)
		}
	})
	t.Run("a partner that cannot clean up keeps the job and the tag", func(t *testing.T) {
		r := strandedRig()
		r.profile.Nodes[1].Online = false
		loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
		err := retireStrandedReplica(crhCtx(), r.deps, log.NewNopLogger(), "set_vm_metadata", "100", 100, loc)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.repl.called("delete:100-0:force=true") ||
			!hasReplicaTag(r.tags) {
			t.Fatalf("err %v calls %v tags %q; the retirement waits and never forces", err, r.repl.calls, r.tags)
		}
	})
	shapes := map[string]func(r *replicaRig){
		"HA-managed tag": func(r *replicaRig) { r.tags = "bosh-cpi;" + haManagedTag + ";" + haReplicaTag },
		"pending re-registration": func(r *replicaRig) {
			r.tags = "bosh-cpi;" + haReplicaTag + ";" + ha.ReaddTag(time.Now())
		},
		"HA resource": func(r *replicaRig) { r.stub.missing = false },
		"foreign pin": func(r *replicaRig) {
			r.setPin("pve01", nil)
			rule := r.stub.rules["bosh-na-100"]
			comment := haIdentity{Owner: "other-director"}.marker()
			rule.Comment = &comment
			r.stub.rules["bosh-na-100"] = rule
		},
		"HA resource unreadable": func(r *replicaRig) {
			r.stub.getErr = cpierrors.Retriable("connection reset")
		},
	}
	for name, mutate := range shapes {
		t.Run("a VM with a "+name+" keeps its job", func(t *testing.T) {
			r := strandedRig()
			mutate(r)
			loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
			err := retireStrandedReplica(crhCtx(), r.deps, log.NewNopLogger(), "set_vm_metadata", "100", 100, loc)
			if name == "HA resource unreadable" {
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
					t.Fatalf("err %v, want a retriable error", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.repl.called("delete:") || len(r.repl.jobs) != 1 || !hasReplicaTag(r.tags) {
				t.Fatalf("calls %v jobs %v tags %q; only the stranded shape is retired", r.repl.calls, r.repl.jobs,
					r.tags)
			}
		})
	}
}

// TestReplicaRetire_OfflinePartnerNamesTheForcedRemoval checks that a
// removal that waits for a partner which cannot clean up names the command
// an operator runs when that node will not come back.
func TestReplicaRetire_OfflinePartnerNamesTheForcedRemoval(t *testing.T) {
	r := newRebuildRig()
	r.profile.Nodes[1].Online = false
	_, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
		!strings.Contains(err.Error(), "pvesr delete 100-0 --force 1") ||
		!strings.Contains(err.Error(), "node pve02") || r.repl.called("delete:100-0:force=true") {
		t.Fatalf("err %v calls %v; the retriable error names the forced removal and the CPI never forces it",
			err, r.repl.calls)
	}
}
