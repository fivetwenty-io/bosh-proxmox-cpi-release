package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// taggedIndexStub is the attach stub whose cluster index lists VM 100 with
// the tags tags returns, so a VM location carries them, on node, or on pve01
// when node is empty.
type taggedIndexStub struct {
	*attachHAStub
	tags func() string
	node string
}

func (s taggedIndexStub) ListResources(context.Context, *cluster.ListResourcesParams) (
	*cluster.ListResourcesResponse, error,
) {
	node := s.node
	if node == "" {
		node = "pve01"
	}
	raw, _ := json.Marshal(map[string]any{"type": "qemu", "vmid": 100, "node": node, "id": "qemu/100",
		"tags": s.tags()})
	return &cluster.ListResourcesResponse{raw}, nil
}

// parkedNFSDisk is a stable-ID disk on shared NFS storage that a parker holds
// under the parker's own name, the shape a detach from a VM whose name the
// disk carried leaves.
func parkedNFSDisk() resolvedDisk {
	return resolvedDisk{diskCID: "d", volid: "nfs:90000/vm-90000-disk-0.qcow2", stableID: "s",
		holder: &pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "pve01"}}
}

// newRebuildRig is the replica rig with VM 100 tagged as replicated, listed
// in the index with its tags, pinned to its source pve01, and replicated to
// pve02 by this Director's job, with every storage classified as shared NFS
// that cannot replicate.
func newRebuildRig() *replicaRig {
	r := newReplicaRig(config.HAModeAuto)
	r.tags = "bosh-cpi;" + haManagedTag + ";" + haReplicaTag
	r.setPin("pve01", nil)
	r.ownJob(1)
	r.repl.removeOnSchedule = true
	client := r.deps.PVE.(*attachRunClient)
	r.deps.PVE = &attachRunClient{lrClient: client.lrClient, tasks: r.tasks,
		cluster: taggedIndexStub{attachHAStub: r.stub, tags: func() string { return r.tags }}}
	r.deps.Resolver = infoResolver{base: backendResolverOrDefault(r.deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	return r
}

func TestPrepareAttachReplicaRebuild_NarrowsThePinAndRetiresTheJob(t *testing.T) {
	r := newRebuildRig()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || rb.partner != "pve02" || rb.vmid != 100 {
		t.Fatalf("rebuild %+v err %v, want one to pve02", rb, err)
	}
	if r.pinNodes() != "pve01" {
		t.Fatalf("pin nodes %q, want the source alone", r.pinNodes())
	}
	if p, ok := ha.ReplicaRebuild(r.pinComment()); !ok || p != "pve02" {
		t.Fatalf("pin comment %q, want the rebuild token naming pve02", r.pinComment())
	}
	if !r.repl.called("delete:100-0:force=false") || r.repl.called("delete:100-0:force=true") ||
		len(r.repl.jobs) != 0 {
		t.Fatalf("calls %v jobs %v; the job goes with cleanup and is never forced", r.repl.calls, r.repl.jobs)
	}
}

func TestPrepareAttachReplicaRebuild_NoRebuild(t *testing.T) {
	cases := map[string]func(r *replicaRig, rd *resolvedDisk){
		"birth-named disk": func(_ *replicaRig, rd *resolvedDisk) { rd.volid = "nfs:700/vm-700-disk-0.qcow2" },
		"disk not parked":  func(_ *replicaRig, rd *resolvedDisk) { rd.holder.IsParker = false },
		"legacy disk":      func(_ *replicaRig, rd *resolvedDisk) { rd.stableID = "" },
		"VM not tagged":    func(r *replicaRig, _ *resolvedDisk) { r.tags = "bosh-cpi;" + haManagedTag },
		"HA off":           func(r *replicaRig, _ *resolvedDisk) { r.deps.Config.HA.Mode = haChoiceOf(config.HAModeOff) },
		"no director_uuid": func(r *replicaRig, _ *resolvedDisk) { r.deps.RequestDirectorUUID = "" },
		"node-local storage": func(r *replicaRig, _ *resolvedDisk) {
			r.deps.Resolver = infoResolver{base: pve.NewStaticBackendResolver(r.deps.PVE, "pve01"),
				info: pve.StorageInfo{Type: pve.StorageTypeLVMThin, Content: "images"}}
		},
		"foreign job": func(r *replicaRig, _ *resolvedDisk) {
			r.repl.jobs = nil
			r.repl.addJob("100-0", "pve01", "pve02", "operator job")
		},
		"no job and no token": func(r *replicaRig, _ *resolvedDisk) { r.repl.jobs = nil },
	}
	for name, mutate := range cases {
		r := newRebuildRig()
		rd := parkedNFSDisk()
		mutate(r, &rd)
		pin := r.pinComment()
		rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", rd)
		if err != nil || rb != nil {
			t.Fatalf("%s: rebuild %+v err %v, want none", name, rb, err)
		}
		if r.repl.called("delete:") || r.pinComment() != pin {
			t.Fatalf("%s: calls %v pin %q; no rebuild changes nothing", name, r.repl.calls, r.pinComment())
		}
	}
}

func TestPrepareAttachReplicaRebuild_NarrowsAWidenedPin(t *testing.T) {
	r := newRebuildRig()
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || rb.partner != "pve02" {
		t.Fatalf("rebuild %+v err %v, want one to pve02", rb, err)
	}
	if _, widened := ha.ReplicaPartner(r.pinComment()); r.pinNodes() != "pve01" || widened {
		t.Fatalf("pin %q over %q; PVE HA must not fail the VM over to the copy the removal deletes",
			r.pinComment(), r.pinNodes())
	}
}

func TestPrepareAttachReplicaRebuild_PendingTokenIsResumedWithoutARemoval(t *testing.T) {
	r := newRebuildRig()
	r.repl.jobs = nil
	r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil || rb.partner != "pve02" {
		t.Fatalf("rebuild %+v err %v, want the pending one to pve02", rb, err)
	}
	if r.repl.called("delete:") {
		t.Fatalf("calls %v; a job that is gone is not removed again", r.repl.calls)
	}
}

func TestPrepareAttachReplicaRebuild_FailuresAreRetriable(t *testing.T) {
	cases := map[string]func(r *replicaRig){
		"job list": func(r *replicaRig) { r.repl.listErr = errors.New("connection reset") },
		"partner offline": func(r *replicaRig) {
			r.profile.Nodes[1].Online = false
		},
	}
	for name, mutate := range cases {
		r := newRebuildRig()
		mutate(r)
		_, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.repl.called("delete:100-0:force=true") {
			t.Fatalf("%s: err %v calls %v; a failure is retriable and never forces", name, err, r.repl.calls)
		}
	}
}

func TestAttachReplicaRebuildFinish_SetsTheJobUpAgainAndClearsTheToken(t *testing.T) {
	r := newRebuildRig()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil {
		t.Fatalf("rebuild %+v err %v", rb, err)
	}
	if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.repl.created) != 1 || r.repl.created[0].Target != "pve02" || len(r.repl.jobs) != 1 {
		t.Fatalf("created %v jobs %v, want one job to pve02", r.repl.created, r.repl.jobs)
	}
	if _, ok := ha.ReplicaRebuild(r.pinComment()); ok {
		t.Fatalf("pin comment %q keeps the rebuild token", r.pinComment())
	}
	if r.pinNodes() != "pve01" || !hasReplicaTag(r.tags) {
		t.Fatalf("pin nodes %q tags %q; the VM stays tagged and pinned to its source until a verified sync",
			r.pinNodes(), r.tags)
	}
}

func TestAttachReplicaRebuildFinish_FailureKeepsTheToken(t *testing.T) {
	r := newRebuildRig()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil {
		t.Fatalf("rebuild %+v err %v", rb, err)
	}
	r.repl.listErr = errors.New("connection reset")
	err = rb.finish(crhCtx(), r.deps, nil)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "pve02") {
		t.Fatalf("err %v; an attach that landed without its job back is retriable", err)
	}
	if p, ok := ha.ReplicaRebuild(r.pinComment()); !ok || p != "pve02" {
		t.Fatalf("pin comment %q, want the token kept for the retry", r.pinComment())
	}
	attachErr := cpierrors.Cloud("the attach failed")
	if err := rb.finish(crhCtx(), r.deps, attachErr); !errors.Is(err, attachErr) {
		t.Fatalf("err %v; a failed attach keeps its own error", err)
	}
}

func TestAttachReplicaRebuildFinish_ProvenRefusalInAutoDropsTheToken(t *testing.T) {
	// A refusal on proven facts leaves the VM pinned to its source without a
	// replica in auto, the way create_vm's setup does, and no later call could
	// set the job up either, so the token goes.
	r := newRebuildRig()
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if err != nil || rb == nil {
		t.Fatalf("rebuild %+v err %v", rb, err)
	}
	r.repl.createErr = errors.New("replication target node pve02 refuses the job")
	if err := rb.finish(crhCtx(), r.deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := ha.ReplicaRebuild(r.pinComment()); ok || r.pinNodes() != "pve01" || len(r.repl.jobs) != 0 {
		t.Fatalf("pin %q over %q jobs %v", r.pinComment(), r.pinNodes(), r.repl.jobs)
	}
}

func TestReconcileVMReplica_FinishesAPendingRebuild(t *testing.T) {
	r := newRebuildRig()
	r.repl.jobs = nil
	r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
	loc := pve.VMLocation{Node: "pve01", Found: true, Tags: r.tags}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.repl.created) != 1 || r.repl.created[0].Target != "pve02" {
		t.Fatalf("created %v, want the job to pve02 set up again", r.repl.created)
	}
	if _, ok := ha.ReplicaRebuild(r.pinComment()); ok || r.pinNodes() != "pve01" {
		t.Fatalf("pin %q over %q; the token goes and the pin keeps the source", r.pinComment(), r.pinNodes())
	}
}

func TestRetireLeftReplica(t *testing.T) {
	leaving := func(r *replicaRig, tagged bool) *attachHA {
		return &attachHA{deps: r.deps, logger: log.NewNopLogger(), vmid: 100, sid: "vm:100", node: "pve01",
			action: attachHALeave, profile: r.profile, replicaTagged: tagged}
	}
	t.Run("tagged VM retires its job and loses the tag", func(t *testing.T) {
		r := newRebuildRig()
		h := leaving(r, true)
		if err := h.retireLeftReplica(crhCtx(), "pve01"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(r.repl.calls, []string{"delete:100-0:force=false", "schedule:pve01/100-0"}) ||
			len(r.repl.jobs) != 0 {
			t.Fatalf("calls %v jobs %v", r.repl.calls, r.repl.jobs)
		}
		if h.leftReplica == nil || h.leftReplica.job != "100-0" || h.leftReplica.other != "pve02" {
			t.Fatalf("left %+v, want job 100-0 with its copy on pve02", h.leftReplica)
		}
		if hasReplicaTag(r.tags) {
			t.Fatalf("tags %q keep the replicate tag", r.tags)
		}
	})
	t.Run("untagged VM reads no job", func(t *testing.T) {
		r := newRebuildRig()
		h := leaving(r, false)
		if err := h.retireLeftReplica(crhCtx(), "pve01"); err != nil || r.repl.listCalls != 0 ||
			h.leftReplica != nil {
			t.Fatalf("err %v reads %d left %+v", err, r.repl.listCalls, h.leftReplica)
		}
	})
	t.Run("foreign job stays and the tag goes", func(t *testing.T) {
		r := newRebuildRig()
		r.repl.jobs = nil
		r.repl.addJob("100-0", "pve01", "pve02", "operator job")
		h := leaving(r, true)
		if err := h.retireLeftReplica(crhCtx(), "pve01"); err != nil || r.repl.called("delete:") {
			t.Fatalf("err %v calls %v", err, r.repl.calls)
		}
		if hasReplicaTag(r.tags) {
			t.Fatalf("tags %q keep the replicate tag", r.tags)
		}
	})
	t.Run("partner offline defers and keeps the tag", func(t *testing.T) {
		r := newRebuildRig()
		r.profile.Nodes[1].Online = false
		h := leaving(r, true)
		err := h.retireLeftReplica(crhCtx(), "pve01")
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.repl.called("delete:100-0:force=true") {
			t.Fatalf("err %v calls %v; a leave never forces", err, r.repl.calls)
		}
		if !hasReplicaTag(r.tags) || h.leftReplica != nil {
			t.Fatalf("tags %q left %+v; the retry needs the tag", r.tags, h.leftReplica)
		}
	})
}

func TestAttachPreflightRefusesANodeLocalDiskOnAReplicatedVM(t *testing.T) {
	profile := stagingProfile(config.HAModeAuto)
	profile.Storages = append(profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	profile.Replication = []ha.ReplicationJob{{ID: "100-0", Guest: 100, Source: "pve01", Target: "pve02"}}
	repl := newFakeReplica()
	repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
	h := &attachHA{profile: profile, vmid: 100, vmCID: "100", action: attachHAGuarded,
		deps: Deps{RequestDirectorUUID: aaDirector, Replication: repl},
		disk: resolvedDisk{diskCID: "d", volid: testPinStorage + ":vm-100-disk-1"}}
	err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{})
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), testPinStorage) ||
		!strings.Contains(err.Error(), "the replication job 100-0") {
		t.Fatalf("err %v; want a permanent refusal naming the storage and the job", err)
	}
	h.disk.volid = "local-zfs:vm-100-disk-1"
	if err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{}); err != nil {
		t.Fatalf("a ZFS pool disk refused: %v", err)
	}
	h.disk.volid = "nfs:100/vm-100-disk-1.qcow2"
	if err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{}); err != nil {
		t.Fatalf("a shared disk refused: %v", err)
	}
	h.disk.volid = testPinStorage + ":vm-100-disk-1"
	h.action, h.replicaTagged = attachHALeave, true
	if err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{}); err != nil {
		t.Fatalf("the leave of a tagged VM retires its job first, so nothing is refused: %v", err)
	}
	repl.addJob("100-1", "pve01", "pve03", "operator job")
	err = h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{})
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "the replication job 100-1") ||
		strings.Contains(err.Error(), "100-0") {
		t.Fatalf("err %v; the leave keeps a job without this Director's marker, so the VM is refused in HA", err)
	}
	repl.listErr = errors.New("connection reset")
	if err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{}); !cpierrors.IsType(err,
		cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v; a job list that cannot be read keeps the VM in HA and is retriable", err)
	}
	repl.listErr = nil
	h.action, h.replicaTagged = attachHAGuarded, false
	if err := h.refuseReplicatedAttach(withReplicaRebuild(context.Background()), pve.DiskHolder{}); err != nil {
		t.Fatalf("an attach whose rebuild removed the job is refused from stale facts: %v", err)
	}
}

func TestAttachWriteFailureNamesTheReplicatedLocalRefusal(t *testing.T) {
	refusal := errors.New("500 Internal Server Error: cannot add non-replicatable volume to a replicated VM")
	for name, err := range map[string]error{
		"raw":   refusal,
		"typed": pve.WrapError(refusal),
	} {
		got := attachWriteFailure(context.Background(), Deps{}, "attach_disk", "100", 100, "pve01", "d",
			testPinStorage+":vm-100-disk-1", "update the VM config", err)
		if !cpierrors.IsType(got, cpierrors.TypeCloud) || !strings.Contains(got.Error(), testPinStorage) ||
			!strings.Contains(got.Error(), "ZFS pool") {
			t.Fatalf("%s: err %v; want a permanent Cloud error naming the storage and the remedy", name, got)
		}
	}
}
