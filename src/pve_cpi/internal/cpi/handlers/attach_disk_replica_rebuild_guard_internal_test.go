package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

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
	cases := map[string]func(r *replicaRig){
		"HA resource gone": func(r *replicaRig) { r.stub.missing = true },
		"HA resource foreign": func(r *replicaRig) {
			r.stub.comment = haIdentity{Owner: "other-director"}.marker()
		},
		"HA resource gone with the token left and the job gone": func(r *replicaRig) {
			r.stub.missing = true
			r.repl.jobs = nil
			r.setPin("pve01", func(c string) string { return ha.WithReplicaRebuild(c, "pve02") })
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRebuildRig()
			setup(r)
			jobs, pin := len(r.repl.jobs), r.pinComment()
			rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
			if err != nil || rb != nil {
				t.Fatalf("rebuild %+v err %v, want none for a VM out of this Director's HA", rb, err)
			}
			if r.repl.called("delete:") || len(r.repl.jobs) != jobs || r.pinComment() != pin {
				t.Fatalf("calls %v jobs %v pin %q; nothing changes", r.repl.calls, r.repl.jobs, r.pinComment())
			}
		})
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
				r.tasks.migrated("pve01", time.Now().Add(-time.Minute), time.Time{}, "")
			},
			reason: "is running",
		},
		"task listing unreadable": {
			setup:  func(r *replicaRig) { r.tasks.migrationErr["pve01"] = errNeedsSysAudit },
			reason: "list the migration tasks on node pve01",
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

// haMotionCluster is the rebuild rig's cluster with a fresh CRM view that
// holds the VM's HA service in the state the test sets.
type haMotionCluster struct {
	taggedIndexStub
	manager func() *cluster.ListHaStatusManagerStatusResponse
}

func (c haMotionCluster) ListHaStatusManagerStatus(context.Context) (*cluster.ListHaStatusManagerStatusResponse,
	error,
) {
	return c.manager(), nil
}

// haMotionRig is the rebuild rig with VM 100 under PVE HA, whose CRM reports
// the VM in state on pve01 with the manager status manager builds from the
// current time.
func haMotionRig(state string, manager func(now int64, services map[string]HASceneService,
) *cluster.ListHaStatusManagerStatusResponse) *replicaRig {
	r := newRebuildRig()
	r.stub.resources["vm:100"] = true
	client := r.deps.PVE.(*attachRunClient)
	services := map[string]HASceneService{"vm:100": {State: state, Node: "pve01", UID: "u1"}}
	r.deps.PVE = haMotionClient{attachRunClient: client, cluster: haMotionCluster{
		taggedIndexStub: client.cluster.(taggedIndexStub),
		manager: func() *cluster.ListHaStatusManagerStatusResponse {
			return manager(time.Now().Unix(), services)
		},
	}}
	return r
}

// runningCRM is the manager status of a CRM that runs a round every few
// seconds: its last round and the LRM's report after it are newer than the
// entry node's clock at the read before.
func runningCRM(now int64, services map[string]HASceneService) *cluster.ListHaStatusManagerStatusResponse {
	return HAFreshManagerStatus(now+6, "pve01", []string{"pve02", "pve03"}, services)
}

// haMotionClient serves haMotionCluster and a node clock, so the CRM view
// reads as fresh.
type haMotionClient struct {
	*attachRunClient
	cluster haMotionCluster
}

func (c haMotionClient) Cluster() cluster.Service { return c.cluster }

func (c haMotionClient) Nodes() sdknodes.Service {
	return HAClockNodes{Service: c.attachRunClient.Nodes()}
}

// TestPrepareAttachReplicaRebuild_WaitsForAnHAMove is a rebuild of a VM whose
// HA service PVE is migrating or relocating. The job stays and the attach is
// retriable, while a started service lets the rebuild remove the job.
func TestPrepareAttachReplicaRebuild_WaitsForAnHAMove(t *testing.T) {
	for _, state := range []string{"migrate", "relocate", "started"} {
		t.Run(state, func(t *testing.T) {
			r := haMotionRig(state, runningCRM)
			rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
			if state == "started" {
				if err != nil || rb == nil || !rb.removed || !r.repl.called("delete:100-0:force=false") {
					t.Fatalf("rebuild %+v err %v calls %v; a settled VM has its job removed", rb, err, r.repl.calls)
				}
				return
			}
			if rb != nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
				!strings.Contains(err.Error(), "may be migrating") {
				t.Fatalf("rebuild %+v err %v, want the retriable deferral", rb, err)
			}
			if r.repl.called("delete:") || len(r.repl.jobs) != 1 {
				t.Fatalf("calls %v jobs %v; the job stays while PVE HA moves the VM", r.repl.calls, r.repl.jobs)
			}
		})
	}
}

// TestPrepareAttachReplicaRebuild_WaitsForACRMRoundAfterThePin is a rebuild
// of a VM under PVE HA whose CRM has run no round since the narrowed pin
// landed, or whose LRM has not reported since that round. A round that was
// running when the pin landed may have started a migration under the wider
// pin that no task or HA state shows yet, so the job stays and the attach is
// retriable, with the narrowed pin and its token in place.
func TestPrepareAttachReplicaRebuild_WaitsForACRMRoundAfterThePin(t *testing.T) {
	defer SetHADeleteMotionPollForTest(10 * time.Millisecond)()
	for name, tc := range map[string]struct {
		manager func(int64, map[string]HASceneService) *cluster.ListHaStatusManagerStatusResponse
		reason  string
	}{
		"no CRM round": {
			manager: func(now int64, services map[string]HASceneService) *cluster.ListHaStatusManagerStatusResponse {
				return HAFreshManagerStatus(now, "pve01", []string{"pve02", "pve03"}, services)
			},
			reason: "no CRM round has run since the narrowed pin",
		},
		"no LRM report after the round": {
			manager: func(now int64, services map[string]HASceneService) *cluster.ListHaStatusManagerStatusResponse {
				scene := &HADeleteScene{Now: now + 6, Members: []string{"pve01", "pve02", "pve03"}, CRMAge: 5,
					LRMAge: map[string]int64{"pve01": 6, "pve02": 6, "pve03": 6}, Services: services}
				return scene.managerBody()
			},
			reason: "has not reported since the CRM round after the narrowed pin",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := haMotionRig("started", tc.manager)
			wait := 1
			r.deps.Config.HA.MotionWaitSec = &wait
			rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
			if rb != nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
				!strings.Contains(err.Error(), "may be migrating") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("rebuild %+v err %v, want the retriable deferral for %q", rb, err, tc.reason)
			}
			if r.repl.called("delete:") || len(r.repl.jobs) != 1 {
				t.Fatalf("calls %v jobs %v; the job stays until a CRM round after the pin", r.repl.calls, r.repl.jobs)
			}
			if p, ok := ha.ReplicaRebuild(r.pinComment()); !ok || p != "pve02" || r.pinNodes() != "pve01" {
				t.Fatalf("pin %q nodes %q, want the narrowed pin with its token", r.pinComment(), r.pinNodes())
			}
		})
	}
}

// movingIndexCluster is the rebuild rig's cluster whose index moves VM 100 to
// pve02 once the rebuild has written the VM's pin, as a migration that starts
// between the pin write and the job's removal would.
type movingIndexCluster struct {
	taggedIndexStub
	pinWritten *bool
}

func (c movingIndexCluster) UpdateHaRules(ctx context.Context, rule string, p *cluster.UpdateHaRulesParams) error {
	*c.pinWritten = true
	return c.taggedIndexStub.UpdateHaRules(ctx, rule, p)
}

func (c movingIndexCluster) ListResources(ctx context.Context, p *cluster.ListResourcesParams) (
	*cluster.ListResourcesResponse, error,
) {
	if *c.pinWritten {
		c.node = "pve02"
	}
	return c.taggedIndexStub.ListResources(ctx, p)
}

// TestPrepareAttachReplicaRebuild_WaitsForAVMThatLeftItsNode is a rebuild
// whose VM is on another node by the time the job would be removed. The job
// stays and the attach is retriable.
func TestPrepareAttachReplicaRebuild_WaitsForAVMThatLeftItsNode(t *testing.T) {
	r := newRebuildRig()
	client := r.deps.PVE.(*attachRunClient)
	written := false
	client.cluster = movingIndexCluster{taggedIndexStub: client.cluster.(taggedIndexStub), pinWritten: &written}
	rb, err := prepareAttachReplicaRebuild(crhCtx(), r.deps, "100", parkedNFSDisk())
	if !written {
		t.Fatalf("rebuild %+v err %v; the rebuild never wrote the pin", rb, err)
	}
	if rb != nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
		!strings.Contains(err.Error(), "runs on node pve02") {
		t.Fatalf("rebuild %+v err %v, want the retriable deferral naming pve02", rb, err)
	}
	if r.repl.called("delete:") || len(r.repl.jobs) != 1 {
		t.Fatalf("calls %v jobs %v; the job stays while the VM is elsewhere", r.repl.calls, r.repl.jobs)
	}
}
