package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fakeReplica is the replication surface of the replicate policy's handler
// tests. Jobs it creates run from source.
type fakeReplica struct {
	source      string
	jobs        []ha.ReplicaJobState
	listErr     error
	listCalls   int
	createErr   error
	createLands bool
	created     []ha.ReplicaJobSpec
	calls       []string
	// removeOnSchedule makes a requested run remove a job marked for removal,
	// the way PVE's runner does after it cleaned up the target.
	removeOnSchedule bool
	scheduleErr      error
	status           ha.ReplicaSyncStatus
	statusErr        error
	listed           map[string]bool
	volumesErr       error
}

func newFakeReplica() *fakeReplica {
	return &fakeReplica{source: "pve01", listed: map[string]bool{}}
}

func (f *fakeReplica) ListJobs(context.Context) ([]ha.ReplicaJobState, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slices.Clone(f.jobs), nil
}

func (f *fakeReplica) CreateJob(_ context.Context, spec ha.ReplicaJobSpec) error {
	f.calls = append(f.calls, "create:"+spec.ID)
	f.created = append(f.created, spec)
	if f.createErr == nil || f.createLands {
		f.addJob(spec.ID, f.source, spec.Target, spec.Comment)
	}
	return f.createErr
}

func (f *fakeReplica) addJob(id, source, target, comment string) {
	guest, num, _ := strings.Cut(id, "-")
	g, _ := strconv.Atoi(guest)
	n, _ := strconv.Atoi(num)
	f.jobs = append(f.jobs, ha.ReplicaJobState{ID: id, Guest: g, JobNum: n, Source: source, Target: target,
		Comment: comment})
}

func (f *fakeReplica) SetComment(_ context.Context, id, comment string) error {
	f.calls = append(f.calls, "comment:"+id)
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].Comment = comment
		}
	}
	return nil
}

func (f *fakeReplica) DeleteJob(_ context.Context, id string, force bool) error {
	f.calls = append(f.calls, "delete:"+id+":force="+strconv.FormatBool(force))
	for i := range f.jobs {
		if f.jobs[i].ID != id {
			continue
		}
		if force {
			f.jobs = slices.Delete(f.jobs, i, i+1)
		} else {
			f.jobs[i].RemoveJob = "full"
		}
		return nil
	}
	return nil
}

func (f *fakeReplica) ScheduleNow(_ context.Context, node, id string) error {
	f.calls = append(f.calls, "schedule:"+node+"/"+id)
	if f.removeOnSchedule {
		f.jobs = slices.DeleteFunc(f.jobs, func(j ha.ReplicaJobState) bool { return j.ID == id && j.RemoveJob != "" })
	}
	return f.scheduleErr
}

func (f *fakeReplica) Status(_ context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	f.calls = append(f.calls, "status:"+node+"/"+id)
	return f.status, f.statusErr
}

func (f *fakeReplica) Volumes(_ context.Context, node, storage string) (map[string]bool, error) {
	f.calls = append(f.calls, "volumes:"+node+"/"+storage)
	if f.volumesErr != nil {
		return nil, f.volumesErr
	}
	return f.listed, nil
}

func (f *fakeReplica) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// replicaRig is VM 100 on pve01 with its root on node-local ZFS storage,
// registered by aaDirector, and a replicate policy with a schedule and a
// rate.
type replicaRig struct {
	deps    Deps
	stub    *attachHAStub
	repl    *fakeReplica
	nodes   *lrNodesStub
	profile *ha.Profile
	tags    string
	cfg     map[string]any
}

const replicaRootVolume = "local-zfs:vm-100-disk-0"

func newReplicaRig(mode string) *replicaRig {
	r := &replicaRig{repl: newFakeReplica(), tags: "bosh-cpi;" + haManagedTag,
		cfg: map[string]any{"scsi0": replicaRootVolume + ",size=10G"}}
	r.stub = &attachHAStub{naClusterStub: newNAStub(), state: "started",
		comment: haIdentity{Owner: aaDirector}.marker()}
	r.stub.members = []string{"pve01", "pve02", "pve03"}
	r.nodes = &lrNodesStub{}
	r.nodes.onUpdate = func() {
		if r.nodes.updatedTags != nil {
			r.tags = *r.nodes.updatedTags
		}
	}
	q := &lrQEMUStub{configFn: func(call int) (map[string]any, error) {
		out := map[string]any{"tags": r.tags, "digest": "d" + strconv.Itoa(call)}
		for k, v := range r.cfg {
			out[k] = v
		}
		return out, nil
	}}
	wait, rate, schedule := 60, 40, "*/15"
	cfg := lrTokenConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(mode), LocalPolicy: haChoiceOf(config.HALocalPolicyReplicate),
		MotionWaitSec: &wait, Replication: &config.HAReplicationConfig{Schedule: &schedule, RateMbps: &rate}}
	r.profile = stagingProfile(mode)
	r.profile.Storages = append(r.profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	r.deps = Deps{
		Config: cfg, RequestDirectorUUID: aaDirector, Logger: log.NewNopLogger(), Replication: r.repl,
		PVE:       &attachRunClient{lrClient: &lrClient{nodes: r.nodes, qemu: q}, cluster: r.stub},
		HAProfile: func(context.Context) (*ha.Profile, error) { return r.profile, nil },
	}
	return r
}

// setPin stores the pin of VM 100 over nodes with comment tokens added to
// aaDirector's marker and the volume record.
func (r *replicaRig) setPin(nodes string, tokens func(string) string) {
	comment := ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), []string{"local-zfs"})
	if tokens != nil {
		comment = tokens(comment)
	}
	strict := true
	r.stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{
		Rule: "bosh-na-100", Type: naRuleType, Resources: "vm:100", Comment: &comment, Nodes: &nodes,
		Strict: &strict,
	}
}

func (r *replicaRig) pinNodes() string { return derefString(r.stub.rules["bosh-na-100"].Nodes) }

func (r *replicaRig) pinComment() string { return derefString(r.stub.rules["bosh-na-100"].Comment) }

// ownJob gives VM 100 this Director's job to pve02 at generation gen.
func (r *replicaRig) ownJob(gen uint64) {
	r.repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, gen))
}

func replicaDecision() *ha.VMDecision {
	return &ha.VMDecision{VMID: 100, SID: "vm:100", Policy: ha.PolicyReplicate, Partner: "pve02"}
}

// replicaHandle returns VM 100's journal record, replicated from pve01 to
// pve02 by a confirmed job 100-0 whose volume set changed at changed.
func replicaHandle(t *testing.T, changed time.Time) *aj.Handle {
	t.Helper()
	journal := siblingPlacementJournal(t, "replica")
	handle, err := journal.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: strings.Repeat("a", 64),
		PolicyFingerprint: strings.Repeat("b", 64), PlanVersion: 1, Plan: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	job := aj.ReplicaJob{ID: "100-0", Guest: 100, Source: "pve01", Target: "pve02"}
	if err := handle.BeginReplication(aaDirector, job, []string{replicaRootVolume}, changed, nil); err != nil {
		t.Fatal(err)
	}
	if err := handle.ConfirmReplicationJob(1); err != nil {
		t.Fatal(err)
	}
	return handle
}

func TestReplicaPinShape(t *testing.T) {
	widened := ha.WithReplicaPartner("m", "pve02")
	cases := map[string]struct {
		pin             *ha.Rule
		source, partner string
		ok              bool
	}{
		"nil":            {},
		"preference":     {pin: &ha.Rule{Nodes: map[string]int{"pve01": 0}}},
		"disabled":       {pin: &ha.Rule{Strict: true, Disabled: true, Nodes: map[string]int{"pve01": 0}}},
		"source only":    {pin: &ha.Rule{Strict: true, Nodes: map[string]int{"pve01": 0}}, source: "pve01", ok: true},
		"two, no token":  {pin: &ha.Rule{Strict: true, Nodes: map[string]int{"pve01": 0, "pve02": 0}}},
		"one with token": {pin: &ha.Rule{Strict: true, Comment: widened, Nodes: map[string]int{"pve01": 0}}},
		"token elsewhere": {pin: &ha.Rule{Strict: true, Comment: ha.WithReplicaPartner("m", "pve03"),
			Nodes: map[string]int{"pve01": 0, "pve02": 0}}},
		"widened": {pin: &ha.Rule{Strict: true, Comment: widened, Nodes: map[string]int{"pve01": 0, "pve02": 0}},
			source: "pve01", partner: "pve02", ok: true},
	}
	for name, tc := range cases {
		source, partner, ok := replicaPinShape(tc.pin)
		if source != tc.source || partner != tc.partner || ok != tc.ok {
			t.Errorf("%s: got %q %q %v, want %q %q %v", name, source, partner, ok, tc.source, tc.partner, tc.ok)
		}
	}
}

func TestReplicaPinWidenAndNarrow(t *testing.T) {
	pin := &ha.Rule{ID: "bosh-na-100", Strict: true, Nodes: map[string]int{"pve01": 3}, Resources: []string{"vm:100"},
		Comment: ha.WithReplicaChanged("m", time.Unix(100, 0))}
	wide := widenedReplicaPin(pin, "pve02")
	if wide.Nodes["pve02"] != 3 || wide.Nodes["pve01"] != 3 || len(pin.Nodes) != 1 {
		t.Fatalf("widened %v (input %v); the partner takes the lowest priority and the input stays", wide.Nodes,
			pin.Nodes)
	}
	if p, ok := ha.ReplicaPartner(wide.Comment); !ok || p != "pve02" {
		t.Fatalf("widened comment %q", wide.Comment)
	}
	at := time.Unix(5000, 0)
	narrow := narrowedReplicaPin(wide, "pve02", at)
	if !slices.Equal(narrow.NodeNames(), []string{"pve02"}) {
		t.Fatalf("narrowed nodes %v", narrow.NodeNames())
	}
	if _, ok := ha.ReplicaPartner(narrow.Comment); ok {
		t.Fatalf("narrowed comment %q keeps the partner", narrow.Comment)
	}
	if got, ok := ha.ReplicaChanged(narrow.Comment); !ok || !got.Equal(at) {
		t.Fatalf("narrowed change time %v", got)
	}
}

func TestGuestReplicaJobs(t *testing.T) {
	mine := ha.ReplicaJobState{ID: "100-0", Guest: 100, Target: "pve02", Comment: renderReplicaJobMarker(aaDirector, 2)}
	theirs := ha.ReplicaJobState{ID: "100-1", Guest: 100, JobNum: 1, Target: "pve03", Comment: "operator job"}
	other := ha.ReplicaJobState{ID: "101-0", Guest: 101, Comment: renderReplicaJobMarker(aaDirector, 1)}
	owned, foreign, err := guestReplicaJobs([]ha.ReplicaJobState{other, theirs, mine}, 100, aaDirector)
	if err != nil || owned == nil || owned.ID != "100-0" || len(foreign) != 1 || foreign[0].ID != "100-1" {
		t.Fatalf("owned %v foreign %v err %v", owned, foreign, err)
	}
	if owned, _, _ := guestReplicaJobs([]ha.ReplicaJobState{mine}, 100, ""); owned != nil {
		t.Fatal("a request with no director_uuid owns no job")
	}
	second := mine
	second.ID, second.JobNum = "100-2", 2
	if _, _, err := guestReplicaJobs([]ha.ReplicaJobState{mine, second}, 100, aaDirector); err == nil {
		t.Fatal("two owned jobs must be an error")
	}
}

func TestReplicaOtherNode(t *testing.T) {
	job := ha.ReplicaJobState{Source: "pve01", Target: "pve02"}
	if got := replicaOtherNode(job, "pve01"); got != "pve02" {
		t.Fatalf("on the source: %q", got)
	}
	if got := replicaOtherNode(job, "pve02"); got != "pve01" {
		t.Fatalf("on the unswapped target: %q", got)
	}
	if got := replicaOtherNode(ha.ReplicaJobState{Target: "pve02"}, "pve02"); got != "" {
		t.Fatalf("no other node: %q", got)
	}
}

func TestReplicaNodeOnline(t *testing.T) {
	ctx := context.Background()
	p := &ha.Profile{Nodes: []ha.NodeFacts{{Name: "pve01", Online: true}, {Name: "pve02"}}}
	online := replicaNodeOnline(p, nil)
	if up, err := online(ctx, "pve01"); !up || err != nil {
		t.Fatalf("pve01: %v %v", up, err)
	}
	if up, err := online(ctx, "pve02"); up || err != nil {
		t.Fatalf("pve02: %v %v", up, err)
	}
	if up, err := online(ctx, "pve09"); up || err != nil {
		t.Fatalf("a non-member: %v %v", up, err)
	}
	if _, err := replicaNodeOnline(nil, errors.New("boom"))(ctx, "pve01"); err == nil {
		t.Fatal("a profile error must be an error")
	}
	if _, err := replicaNodeOnline(&ha.Profile{}, nil)(ctx, "pve01"); err == nil {
		t.Fatal("a profile with no node must be an error")
	}
}

func TestSetUpCreatedReplica_CreatesTheJobAndKeepsTheSourcePin(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01", nil)
	before := time.Now().Add(-time.Second)
	err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasReplicaTag(r.tags) {
		t.Fatalf("tags %q, want %s", r.tags, haReplicaTag)
	}
	if len(r.repl.created) != 1 {
		t.Fatalf("created %v", r.repl.created)
	}
	spec := r.repl.created[0]
	marker, ok := parseReplicaJobMarker(spec.Comment)
	if spec.ID != "100-0" || spec.Target != "pve02" || spec.Schedule != "*/15" || spec.RateMbps != 40 || !ok ||
		marker.Generation != 1 || marker.Owner != markerValue(aaDirector) {
		t.Fatalf("job spec %+v marker %+v", spec, marker)
	}
	if r.pinNodes() != "pve01" {
		t.Fatalf("pin nodes %q; the pin names the source alone before a verified sync", r.pinNodes())
	}
	changed, ok := ha.ReplicaChanged(r.pinComment())
	if !ok || changed.Before(before.Truncate(time.Second)) {
		t.Fatalf("pin comment %q records no change time from this create", r.pinComment())
	}
	if !slices.Contains(r.repl.calls, "schedule:pve01/100-0") {
		t.Fatalf("calls %v; the first sync is requested", r.repl.calls)
	}
}

func TestSetUpCreatedReplica_NeverWaitsForTheFirstSync(t *testing.T) {
	// The create requests the first sync and returns. It never reads the sync
	// status or the target's listing, so even a sync that would verify leaves
	// the source-only pin, and a later touch call widens it.
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		r := newReplicaRig(mode)
		r.setPin("pve01", nil)
		r.repl.status = ha.ReplicaSyncStatus{LastSync: time.Now().Add(time.Hour)}
		r.repl.listed[replicaRootVolume] = true
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile,
			nil)
		if err != nil || r.pinNodes() != "pve01" {
			t.Fatalf("%s: err %v pin %q", mode, err, r.pinNodes())
		}
		if !r.repl.called("schedule:pve01/100-0") || r.repl.called("status") || r.repl.called("volumes") {
			t.Fatalf("%s: calls %v; the create requests the sync and reads no sync state", mode, r.repl.calls)
		}
	}
}

func TestSetUpCreatedReplica_ScheduleFailureKeepsTheCreate(t *testing.T) {
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve01", nil)
	r.repl.scheduleErr = errors.New("connection reset")
	err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile, nil)
	if err != nil || len(r.repl.created) != 1 || r.pinNodes() != "pve01" {
		t.Fatalf("err %v created %v pin %q; PVE's schedule runs the first sync", err, r.repl.created, r.pinNodes())
	}
}

func TestSetUpCreatedReplica_TransientFailureIsRetriableInEveryMode(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		r := newReplicaRig(mode)
		r.setPin("pve01", nil)
		r.repl.listErr = errors.New("connection reset")
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile,
			nil)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || len(r.repl.created) != 0 {
			t.Fatalf("%s: err %v created %v; a failure that proves nothing defers", mode, err, r.repl.created)
		}
	}
}

func TestSetUpCreatedReplica_VolumeTheJobCannotCopy(t *testing.T) {
	// A persistent disk a disk ID owns sits on node-local ZFS storage, and
	// PVE's job never copies it. The VM keeps the source-only pin in auto and
	// the create is refused in managed.
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		r := newReplicaRig(mode)
		r.setPin("pve01", nil)
		r.cfg["scsi1"] = "local-zfs:vm-9001-disk-0,size=5G"
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile,
			nil)
		if len(r.repl.created) != 0 || r.pinNodes() != "pve01" {
			t.Fatalf("%s: created %v pin %q", mode, r.repl.created, r.pinNodes())
		}
		switch {
		case mode == config.HAModeAuto && err != nil:
			t.Fatalf("auto keeps the source-only pin, got %v", err)
		case mode == config.HAModeManaged && (err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud)):
			t.Fatalf("managed refuses, got %v", err)
		}
	}
}

func TestSetUpCreatedReplica_ResumeReusesTheOwnedJob(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01", nil)
	r.ownJob(1)
	err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile, nil)
	if err != nil || len(r.repl.created) != 0 {
		t.Fatalf("err %v created %v; the owned job is reused", err, r.repl.created)
	}
}

func TestSetUpCreatedReplica_ForeignJob(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		r := newReplicaRig(mode)
		r.setPin("pve01", nil)
		r.repl.addJob("100-0", "pve01", "pve03", "an operator's job")
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(),
			r.profile, nil)
		switch {
		case len(r.repl.created) != 0:
			t.Fatalf("%s: created %v next to a foreign job", mode, r.repl.created)
		case mode == config.HAModeAuto && err != nil:
			t.Fatalf("auto degrades to the source-only pin, got %v", err)
		case mode == config.HAModeManaged && (err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud)):
			t.Fatalf("managed refuses, got %v", err)
		}
	}
}

func TestSetUpCreatedReplica_FailedCreate(t *testing.T) {
	t.Run("refused and absent abandons it", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.setPin("pve01", nil)
		r.repl.createErr = &sdkerrors.APIError{HTTPCode: 400, Message: "target has no zfs pool"}
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(),
			r.profile, nil)
		if err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err %v, want the refusal", err)
		}
	})
	t.Run("transient stays retriable", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.setPin("pve01", nil)
		r.repl.createErr = &sdkerrors.APIError{HTTPCode: 503, Message: "busy"}
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(),
			r.profile, nil)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err %v, want retriable", err)
		}
	})
	t.Run("applied despite the error", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.setPin("pve01", nil)
		r.repl.createErr, r.repl.createLands = errors.New("connection reset"), true
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(),
			r.profile, nil)
		if err != nil {
			t.Fatalf("err %v; the job that landed with our marker is the job", err)
		}
	})
	t.Run("auto degrades", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01", nil)
		r.repl.createErr = &sdkerrors.APIError{HTTPCode: 400, Message: "refused"}
		if err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(),
			r.profile, nil); err != nil {
			t.Fatalf("auto: %v", err)
		}
	})
}

func TestSetUpCreatedReplica_JournalRecordsTheJob(t *testing.T) {
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve01", nil)
	journal := siblingPlacementJournal(t, "replica-create")
	handle, err := journal.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: strings.Repeat("a", 64),
		PolicyFingerprint: strings.Repeat("b", 64), PlanVersion: 1, Plan: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	if err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile,
		handle); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rep := handle.Record().Replication
	if rep == nil || rep.Job == nil || rep.Job.ID != "100-0" || rep.PendingJob != nil || rep.Owner != aaDirector {
		t.Fatalf("replication record %+v", rep)
	}
	if set, ok := rep.CommittedVolumeSet(); !ok || !slices.Equal(set.VolIDs, []string{replicaRootVolume}) {
		t.Fatalf("volume set %+v", set)
	}
}

func TestSetUpCreatedReplica_NotReplicate(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	for _, d := range []*ha.VMDecision{nil, {Policy: ha.PolicyHold}, {Policy: ha.PolicyReplicate}} {
		if err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, d, r.profile,
			nil); err != nil {
			t.Fatal(err)
		}
	}
	if r.repl.listCalls != 0 || len(r.repl.calls) != 0 || r.nodes.updateCalls != 0 {
		t.Fatalf("calls %v; a VM without a replicate decision reads nothing", r.repl.calls)
	}
}

func TestSetUpCreatedReplica_NoReplicatedVolume(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		r := newReplicaRig(mode)
		r.setPin("pve01", nil)
		r.cfg = map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G"}
		err := setUpCreatedReplica(crhCtx(), r.deps, log.NewNopLogger(), "pve01", 100, replicaDecision(), r.profile,
			nil)
		if (mode == config.HAModeManaged) != (err != nil) || len(r.repl.created) != 0 {
			t.Fatalf("%s: err %v created %v", mode, err, r.repl.created)
		}
	}
}

func TestRetireTornDownReplica(t *testing.T) {
	tagged := map[string]any{"tags": "bosh-cpi;" + haReplicaTag, "scsi0": replicaRootVolume + ",size=10G"}
	t.Run("target online lets PVE clean up", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.ownJob(1)
		r.repl.removeOnSchedule = true
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged,
			nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"delete:100-0:force=false", "schedule:pve01/100-0"}
		if !slices.Equal(r.repl.calls, want) || len(r.repl.jobs) != 0 {
			t.Fatalf("calls %v jobs %v, want %v", r.repl.calls, r.repl.jobs, want)
		}
	})
	t.Run("target offline forces and records the orphan", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.ownJob(1)
		r.profile.Nodes[1].Online = false
		handle := replicaHandle(t, time.Now().Add(-time.Hour))
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged,
			handle); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(r.repl.calls, []string{"delete:100-0:force=true"}) {
			t.Fatalf("calls %v", r.repl.calls)
		}
		rep := handle.Record().Replication
		if !rep.Retired || len(rep.Copies) != 1 || rep.Copies[0].Kind != aj.ReplicaCopyOrphan ||
			rep.Copies[0].Node != "pve02" || !slices.Equal(rep.Copies[0].VolIDs, []string{replicaRootVolume}) {
			t.Fatalf("record %+v", rep)
		}
	})
	t.Run("resume after the journal recorded the retirement", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.ownJob(1)
		r.repl.removeOnSchedule = true
		handle := replicaHandle(t, time.Now().Add(-time.Hour))
		if err := handle.RetireReplicationJob(); err != nil {
			t.Fatal(err)
		}
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged,
			handle); err != nil || len(r.repl.jobs) != 0 {
			t.Fatalf("err %v jobs %v", err, r.repl.jobs)
		}
	})
	t.Run("job gone marks the record retired", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		handle := replicaHandle(t, time.Now().Add(-time.Hour))
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", nil,
			handle); err != nil || !handle.Record().Replication.Retired {
			t.Fatalf("err %v record %+v", err, handle.Record().Replication)
		}
	})
	t.Run("no tag and no record reads nothing", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01",
			map[string]any{"tags": "bosh-cpi"}, nil); err != nil || r.repl.listCalls != 0 {
			t.Fatalf("err %v reads %d", err, r.repl.listCalls)
		}
	})
	t.Run("foreign job stays", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.repl.addJob("100-0", "pve01", "pve02", "operator job")
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged,
			nil); err != nil || len(r.repl.calls) != 0 {
			t.Fatalf("err %v calls %v", err, r.repl.calls)
		}
	})
	t.Run("no director_uuid defers", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.deps.RequestDirectorUUID = ""
		err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged, nil)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("list failure defers", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.repl.listErr = errors.New("connection reset")
		err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged, nil)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestRetireReplicaWithoutDestroyNeverForces(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.ownJob(1)
	r.profile.Nodes[1].Online = false
	rr := &replicaRetire{deps: r.deps, logger: log.NewNopLogger(), api: r.repl, caller: "detach_disk", vmid: 100,
		node: "pve01", online: replicaNodeOnline(r.profile, nil)}
	err := rr.run(crhCtx())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.repl.called("delete:100-0:force=true") {
		t.Fatalf("err %v calls %v; only a destroy may force", err, r.repl.calls)
	}
}

// verifiedSync is a sync that started an hour after the pin's change.
func verifiedSync(changed time.Time) ha.ReplicaSyncStatus {
	return ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour)}
}

func TestReconcileVMReplica_WidensOnAVerifiedSync(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownJob(1)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	loc := pve.VMLocation{Node: "pve01", Found: true, Tags: "bosh-cpi;" + haReplicaTag}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.pinNodes() != "pve01,pve02" {
		t.Fatalf("pin nodes %q, want the partner added", r.pinNodes())
	}
	if p, ok := ha.ReplicaPartner(r.pinComment()); !ok || p != "pve02" {
		t.Fatalf("pin comment %q", r.pinComment())
	}
}

func TestReconcileVMReplica_UnprovenSyncKeepsTheSourcePin(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	cases := map[string]struct {
		status ha.ReplicaSyncStatus
		listed bool
	}{
		"rising failure count":                 {status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Hour), FailCount: 3}},
		"sync started before the change":       {status: ha.ReplicaSyncStatus{LastSync: changed.Add(-time.Minute)}, listed: true},
		"sync inside the clock-skew tolerance": {status: ha.ReplicaSyncStatus{LastSync: changed.Add(time.Minute)}, listed: true},
		"new disk not on the target yet":       {status: verifiedSync(changed)},
	}
	for name, tc := range cases {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
		r.ownJob(1)
		r.repl.status = tc.status
		r.repl.listed[replicaRootVolume] = tc.listed
		loc := pve.VMLocation{Node: "pve01", Found: true, Tags: haReplicaTag}
		if err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.pinNodes() != "pve01" {
			t.Fatalf("%s: pin nodes %q, want the source alone", name, r.pinNodes())
		}
	}
}

func TestReconcileVMReplica_NoAction(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour)
	tagged := pve.VMLocation{Node: "pve01", Found: true, Tags: haReplicaTag}
	cases := map[string]func(r *replicaRig) pve.VMLocation{
		"untagged": func(*replicaRig) pve.VMLocation { return pve.VMLocation{Node: "pve01", Found: true} },
		"mode off": func(r *replicaRig) pve.VMLocation {
			r.deps.Config.HA.Mode = haChoiceOf(config.HAModeOff)
			return tagged
		},
		"no director_uuid": func(r *replicaRig) pve.VMLocation { r.deps.RequestDirectorUUID = ""; return tagged },
		"widened already": func(r *replicaRig) pve.VMLocation {
			r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
			return tagged
		},
		"on a node the pin does not name": func(*replicaRig) pve.VMLocation {
			return pve.VMLocation{Node: "pve03", Found: true, Tags: haReplicaTag}
		},
	}
	for name, setup := range cases {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
		r.ownJob(1)
		loc := setup(r)
		before := r.pinNodes()
		if err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.pinNodes() != before || r.repl.called("schedule") || r.repl.called("status") {
			t.Fatalf("%s: pin %q calls %v", name, r.pinNodes(), r.repl.calls)
		}
		if name == "mode off" && (r.repl.listCalls != 0 || len(r.repl.calls) != 0) {
			t.Fatalf("mode off: %d list reads and calls %v; off mode reads nothing", r.repl.listCalls, r.repl.calls)
		}
	}
}

func TestReconcileVMReplica_FailoverNarrowingErrorFailsTheCall(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	r.repl.listErr = errors.New("connection reset")
	loc := pve.VMLocation{Node: "pve02", Found: true, Tags: haReplicaTag}
	err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.pinNodes() != "pve01,pve02" {
		t.Fatalf("err %v pin %q; a narrowing that cannot run defers and keeps the pin", err, r.pinNodes())
	}
}

// failoverView is barrierView for a VM that runs on node, with the mutation
// run on that node.
func failoverView(res *attachRunRes, node string) ha.NarrowDeps {
	v := barrierView(res)
	read := v.ReadState
	v.ReadState = func(ctx context.Context, vmid int, n string) (*pve.HAServiceState, error) {
		st, err := read(ctx, vmid, n)
		if st != nil {
			st.Node = node
		}
		return st, err
	}
	v.Settle = func(context.Context, int) (pve.VMLocation, error) {
		return pve.VMLocation{Node: node, Found: true}, nil
	}
	v.Mutate = func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
		return pve.VMLocation{Node: node, Found: true}, m.Apply(ctx, node)
	}
	return v
}

func TestNarrowAfterFailover_NarrowsToTheNewSourceAndRequestsASync(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	// PVE swapped the job's direction after the failover.
	r.repl.addJob("100-0", "pve02", "pve01", renderReplicaJobMarker(aaDirector, 1))
	set, err := readHARuleSet(context.Background(), r.deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	pin := set.Find("bosh-na-100")
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "started", comment: r.stub.comment}
	err = narrowAfterFailover(crhCtx(), r.deps, log.NewNopLogger(), "reboot_vm", r.repl, failoverView(res, "pve02"),
		r.profile, 100, "pve02", pin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.pinNodes() != "pve02" {
		t.Fatalf("pin nodes %q, want the new source alone", r.pinNodes())
	}
	if _, ok := ha.ReplicaPartner(r.pinComment()); ok {
		t.Fatalf("pin comment %q keeps the partner", r.pinComment())
	}
	if _, ok := ha.ReplicaChanged(r.pinComment()); !ok {
		t.Fatalf("pin comment %q records no change", r.pinComment())
	}
	if !slices.Contains(r.repl.calls, "schedule:pve02/100-0") {
		t.Fatalf("calls %v; the sync is requested from the new source", r.repl.calls)
	}
	if !slices.Equal(sl.events, []string{"request:ignored", "request:started"}) {
		t.Fatalf("events %v; the narrowing runs inside the barrier", sl.events)
	}
}

func TestWidenWithJournalBindsTheEvidence(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownJob(1)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	handle := replicaHandle(t, changed)
	w := &replicaWiden{deps: r.deps, logger: log.NewNopLogger(), api: r.repl, profile: r.profile, vmid: 100,
		node: "pve01", handle: handle}
	widened, err := w.run(crhCtx())
	if err != nil || !widened {
		t.Fatalf("widened %v err %v", widened, err)
	}
	sync := handle.Record().Replication.Sync
	if sync == nil || sync.JobGeneration != 1 || sync.VolumeSetGeneration != 1 || sync.Target != "pve02" {
		t.Fatalf("sync evidence %+v", sync)
	}
}

func TestWidenWithJournalRefusesAMismatchedRecord(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	cases := map[string]func(r *replicaRig, h *aj.Handle){
		"marker generation differs": func(r *replicaRig, _ *aj.Handle) { r.ownJob(2) },
		"job generation pending": func(r *replicaRig, h *aj.Handle) {
			r.ownJob(1)
			if _, err := h.PrepareReplicationJob(aj.ReplicaJob{ID: "100-0", Guest: 100, Source: "pve01",
				Target: "pve02"}); err != nil {
				t.Fatal(err)
			}
		},
		"volume change pending": func(r *replicaRig, h *aj.Handle) {
			r.ownJob(1)
			if _, err := h.PrepareReplicaVolumeSet([]string{replicaRootVolume, "local-zfs:vm-100-disk-1"},
				changed, nil); err != nil {
				t.Fatal(err)
			}
		},
		"record retired": func(r *replicaRig, h *aj.Handle) {
			r.ownJob(1)
			if err := h.RetireReplicationJob(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		r := newReplicaRig(config.HAModeManaged)
		r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
		r.repl.status = verifiedSync(changed)
		r.repl.listed[replicaRootVolume] = true
		handle := replicaHandle(t, changed)
		setup(r, handle)
		w := &replicaWiden{deps: r.deps, logger: log.NewNopLogger(), api: r.repl, profile: r.profile, vmid: 100,
			node: "pve01", handle: handle}
		if widened, err := w.run(crhCtx()); err != nil || widened || r.pinNodes() != "pve01" {
			t.Fatalf("%s: widened %v err %v pin %q", name, widened, err, r.pinNodes())
		}
	}
}

func TestWidenRecordsANewVolumeSetBeforeTheCheck(t *testing.T) {
	// An attach added a disk after the record's set. The widen records the new
	// set, and the sync must follow the set's change before it counts.
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownJob(1)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	r.repl.listed["local-zfs:vm-100-disk-1"] = true
	handle := replicaHandle(t, changed)
	w := &replicaWiden{deps: r.deps, logger: log.NewNopLogger(), api: r.repl, profile: r.profile, vmid: 100,
		node: "pve01", handle: handle}
	if widened, err := w.run(crhCtx()); err != nil || !widened {
		t.Fatalf("widened %v err %v", widened, err)
	}
	set, ok := handle.Record().Replication.CommittedVolumeSet()
	if !ok || set.Generation != 2 || len(set.VolIDs) != 2 {
		t.Fatalf("volume set %+v", set)
	}
}

func TestReplicaPinComment(t *testing.T) {
	if replicaPinComment("bosh-cpi:v1 owner=x") {
		t.Fatal("a plain pin is not a replicate pin")
	}
	if !replicaPinComment(ha.WithReplicaPartner("m", "pve02")) ||
		!replicaPinComment(ha.WithReplicaChanged("m", time.Unix(10, 0))) {
		t.Fatal("a pin with a replicate token is a replicate pin")
	}
}

func TestBackendReplicaLookup(t *testing.T) {
	withPolicy := func(mode string) *config.CPIConfig {
		cfg := lrTokenConfig()
		cfg.HA = &config.HAConfig{LocalPolicy: haChoiceOf(config.HALocalPolicyReplicate)}
		if mode != "" {
			cfg.HA.Mode = haChoiceOf(mode)
		}
		return cfg
	}
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want bool
	}{
		{"nil config", nil, false},
		{"no HA block", lrTokenConfig(), false},
		{"replicate with mode unset", withPolicy(""), false},
		{"replicate with mode off", withPolicy(config.HAModeOff), false},
		{"replicate in auto", withPolicy(config.HAModeAuto), true},
		{"replicate in managed", withPolicy(config.HAModeManaged), true},
	}
	for _, c := range cases {
		if got := replicaLookupEnabled(c.cfg); got != c.want {
			t.Fatalf("%s: lookup enabled %v, want %v", c.name, got, c.want)
		}
		if BackendReplicaLookup(c.cfg) == nil {
			t.Fatalf("%s: the option must always be set", c.name)
		}
	}
}

func TestAttachToAReplicateVMNarrowsAndRestampsTheChange(t *testing.T) {
	// The VM's widened pin names pve01 and its partner pve02. A new disk on
	// node-local ZFS storage has no replica yet, so the attach narrows the pin
	// to pve01 inside the barrier and records the change. After the disk
	// lands it stamps the change again, and it requests no sync and reads no
	// sync state, so only a sync after the attach finished can widen the pin.
	deps, stub, profile := attachRig(config.HAModeAuto)
	wait := 60
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
	deps.Config.HA.MotionWaitSec = &wait
	profile.Storages = append(profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	repl := newFakeReplica()
	repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication = repl
	cfg := map[string]any{"scsi0": replicaRootVolume + ",size=10G", "cpu": "x86-64-v3"}
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return cfg, nil
	}}
	comment := ha.WithReplicaPartner(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), "pve02")
	nodes, strict := "pve01,pve02", true
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes, Strict: &strict}
	disk := resolvedDisk{volid: "local-zfs:vm-100-disk-1", stableID: "abcd1234"}
	h, err := decideAttachHA(context.Background(), deps, "100", disk)
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v; a recorded storage does not skip a replicate VM's attach", h, err)
	}
	if !slices.Equal(h.newPin.NodeNames(), []string{"pve01"}) || h.replicaChanged.IsZero() {
		t.Fatalf("new pin %v changed %v, want the source alone with the change recorded", h.newPin.NodeNames(),
			h.replicaChanged)
	}
	if _, ok := ha.ReplicaPartner(h.newPin.Comment); ok {
		t.Fatalf("new pin comment %q keeps the partner", h.newPin.Comment)
	}
	res := &attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment}
	h.narrow = barrierView(res)
	h.deps = deps
	err = h.run(context.Background(), func(context.Context) (string, error) {
		cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pin := stub.rules["bosh-na-100"]
	if derefString(pin.Nodes) != "pve01" {
		t.Fatalf("pin nodes %q; the attach before the first sync keeps the source alone", derefString(pin.Nodes))
	}
	stamped, ok := ha.ReplicaChanged(derefString(pin.Comment))
	if !ok || stamped.Before(h.replicaChanged.Truncate(time.Second)) {
		t.Fatalf("pin comment %q; the change is stamped no earlier than the decision at %v",
			derefString(pin.Comment), h.replicaChanged)
	}
	if len(repl.calls) != 0 {
		t.Fatalf("calls %v; the attach requests no sync and reads no sync state", repl.calls)
	}
}

func widenOf(r *replicaRig, handle *aj.Handle) *replicaWiden {
	return &replicaWiden{deps: r.deps, logger: log.NewNopLogger(), api: r.repl, profile: r.profile, vmid: 100,
		node: "pve01", handle: handle}
}

func TestWidenStampsAMissingChangeAndWaitsForALaterCall(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01", nil)
	r.ownJob(1)
	r.repl.status = ha.ReplicaSyncStatus{LastSync: time.Now().Add(time.Hour)}
	r.repl.listed[replicaRootVolume] = true
	at := time.Unix(7000, 0)
	w := widenOf(r, nil)
	w.now = func() time.Time { return at }
	widened, err := w.run(crhCtx())
	if err != nil || widened || r.pinNodes() != "pve01" {
		t.Fatalf("widened %v err %v pin %q; a pin with no change time never widens", widened, err, r.pinNodes())
	}
	if got, ok := ha.ReplicaChanged(r.pinComment()); !ok || !got.Equal(at) {
		t.Fatalf("pin comment %q, want the change stamped at %v", r.pinComment(), at)
	}
	if r.repl.listCalls != 0 || len(r.repl.calls) != 0 {
		t.Fatalf("reads %d calls %v; the stamp reads no job state", r.repl.listCalls, r.repl.calls)
	}
}

func TestWidenRefusesAJobThatCannotCopy(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	cases := map[string]func(r *replicaRig){
		"disabled job":            func(r *replicaRig) { r.repl.jobs[0].Disabled = true },
		"job marked for removal":  func(r *replicaRig) { r.repl.jobs[0].RemoveJob = "full" },
		"persistent disk on ZFS":  func(r *replicaRig) { r.cfg["scsi1"] = "local-zfs:vm-9001-disk-0,size=5G" },
		"volume on unknown store": func(r *replicaRig) { r.cfg["scsi1"] = "elsewhere:vm-100-disk-1,size=5G" },
	}
	for name, setup := range cases {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
		r.ownJob(1)
		r.repl.status = verifiedSync(changed)
		r.repl.listed[replicaRootVolume] = true
		r.repl.listed["local-zfs:vm-9001-disk-0"] = true
		setup(r)
		widened, err := widenOf(r, nil).run(crhCtx())
		if err != nil || widened || r.pinNodes() != "pve01" {
			t.Fatalf("%s: widened %v err %v pin %q", name, widened, err, r.pinNodes())
		}
		if r.repl.called("status") {
			t.Fatalf("%s: calls %v; a job that cannot cover the VM is never checked", name, r.repl.calls)
		}
	}
}

func TestWidenAfterAFailoverAdoptsTheSwappedJob(t *testing.T) {
	// HA moved VM 100 to pve02, the narrowing pinned it there, and PVE swapped
	// job 100-0 to run from pve02 to pve01. The record still names the old
	// direction, so the widen moves it to the new one under generation 2.
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve02", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.repl.source = "pve02"
	r.repl.addJob("100-0", "pve02", "pve01", renderReplicaJobMarker(aaDirector, 1))
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	handle := replicaHandle(t, changed)
	w := widenOf(r, handle)
	w.node = "pve02"
	widened, err := w.run(crhCtx())
	if err != nil || !widened {
		t.Fatalf("widened %v err %v", widened, err)
	}
	rep := handle.Record().Replication
	if rep.PendingJob != nil || rep.Job == nil || rep.Job.Source != "pve02" || rep.Job.Target != "pve01" ||
		rep.Job.Generation != 2 {
		t.Fatalf("record job %+v pending %+v, want pve02 to pve01 at generation 2", rep.Job, rep.PendingJob)
	}
	if marker, ok := parseReplicaJobMarker(r.repl.jobs[0].Comment); !ok || marker.Generation != 2 {
		t.Fatalf("job comment %q, want the marker at generation 2", r.repl.jobs[0].Comment)
	}
	if r.pinNodes() != "pve01,pve02" {
		t.Fatalf("pin nodes %q, want the old source added back as the partner", r.pinNodes())
	}
}

func TestWidenAfterAFailoverRefusesAJobOfAnotherOwner(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve02", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.repl.addJob("100-0", "pve02", "pve01", renderReplicaJobMarker(aaDirector, 5))
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	handle := replicaHandle(t, changed)
	w := widenOf(r, handle)
	w.node = "pve02"
	if widened, err := w.run(crhCtx()); err != nil || widened || r.pinNodes() != "pve02" {
		t.Fatalf("widened %v err %v pin %q; a marker of another generation proves nothing", widened, err,
			r.pinNodes())
	}
	if rep := handle.Record().Replication; rep.Job.Generation != 1 || rep.PendingJob != nil {
		t.Fatalf("record %+v; a refused adoption records nothing", rep)
	}
}

// rulesErrCluster fails every HA rule read.
type rulesErrCluster struct {
	*attachHAStub
	err error
}

func (c *rulesErrCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse,
	error,
) {
	return nil, c.err
}

func TestReconcileVMReplica_UnreadablePinDefers(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	client, ok := r.deps.PVE.(*attachRunClient)
	if !ok {
		t.Fatal("the rig's client is not an attachRunClient")
	}
	client.cluster = &rulesErrCluster{attachHAStub: r.stub, err: errors.New("connection reset")}
	loc := pve.VMLocation{Node: "pve02", Found: true, Tags: haReplicaTag}
	err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v; a narrowing the VM may owe is never skipped for a failed read", err)
	}
}

func TestReconcileVMReplica_ProfileFailure(t *testing.T) {
	boom := func(context.Context) (*ha.Profile, error) { return nil, errors.New("cluster status unreadable") }
	t.Run("on the partner it defers", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
		r.deps.HAProfile = boom
		loc := pve.VMLocation{Node: "pve02", Found: true, Tags: haReplicaTag}
		err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger())
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || r.pinNodes() != "pve01,pve02" {
			t.Fatalf("err %v pin %q", err, r.pinNodes())
		}
	})
	t.Run("on the source it leaves the pin", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, time.Now().Add(-time.Hour)) })
		r.deps.HAProfile = boom
		loc := pve.VMLocation{Node: "pve01", Found: true, Tags: haReplicaTag}
		err := reconcileVMReplica(crhCtx(), r.deps, "reboot_vm", "100", 100, loc, log.NewNopLogger())
		if err != nil || r.pinNodes() != "pve01" {
			t.Fatalf("err %v pin %q; a widen that cannot run waits for a later call", err, r.pinNodes())
		}
	})
}

func TestNarrowAfterFailover_ErroredResourceReregistersUnderTheClaim(t *testing.T) {
	// HA recovered the VM to pve02 and left its resource in the error state.
	// The narrowing runs inside the re-registration, which takes the VMID
	// claim before it writes anything. This rig has no claim pool, so the
	// claim fails and nothing in HA changes.
	r := newReplicaRig(config.HAModeAuto)
	r.setPin("pve01,pve02", func(c string) string { return ha.WithReplicaPartner(c, "pve02") })
	r.repl.addJob("100-0", "pve02", "pve01", renderReplicaJobMarker(aaDirector, 1))
	set, err := readHARuleSet(context.Background(), r.deps.PVE.Cluster())
	if err != nil {
		t.Fatal(err)
	}
	sl := &stagingLog{}
	res := &attachRunRes{log: sl, state: "error", comment: r.stub.comment}
	view := failoverView(res, "pve02")
	read := view.ReadState
	view.ReadState = func(ctx context.Context, vmid int, n string) (*pve.HAServiceState, error) {
		st, err := read(ctx, vmid, n)
		if st != nil && st.Raw == "started" {
			st.Raw = "error"
		}
		return st, err
	}
	err = narrowAfterFailover(crhCtx(), r.deps, log.NewNopLogger(), "reboot_vm", r.repl, view, r.profile, 100,
		"pve02", set.Find("bosh-na-100"))
	if err == nil || strings.Contains(err.Error(), "operator must clear") || !strings.Contains(err.Error(), "vm-100") {
		t.Fatalf("err %v; the errored resource goes to the re-registration, which takes the VMID claim", err)
	}
	if r.pinNodes() != "pve01,pve02" || len(sl.events) != 0 {
		t.Fatalf("pin %q events %v; a re-registration that cannot take the claim writes nothing", r.pinNodes(),
			sl.events)
	}
}

func TestConfirmReplicaJobForTheDeadNodeCompletion(t *testing.T) {
	cases := map[string]struct {
		setup func(f *fakeReplica)
		ok    bool
	}{
		"forward job":           {setup: func(f *fakeReplica) { f.addJob("100-0", "pve01", "pve02", "") }, ok: true},
		"swapped job":           {setup: func(f *fakeReplica) { f.addJob("100-0", "pve02", "pve01", "") }, ok: true},
		"job with no source":    {setup: func(f *fakeReplica) { f.addJob("100-0", "", "pve02", "") }, ok: true},
		"no job":                {setup: func(*fakeReplica) {}},
		"foreign job":           {setup: func(f *fakeReplica) { f.addJob("100-0", "pve01", "pve02", "operator job") }},
		"job to another node":   {setup: func(f *fakeReplica) { f.addJob("100-0", "pve01", "pve03", "") }},
		"job from another node": {setup: func(f *fakeReplica) { f.addJob("100-0", "pve03", "pve02", "") }},
		"disabled job": {setup: func(f *fakeReplica) {
			f.addJob("100-0", "pve01", "pve02", "")
			f.jobs[0].Disabled = true
		}},
		"job marked for removal": {setup: func(f *fakeReplica) {
			f.addJob("100-0", "pve01", "pve02", "")
			f.jobs[0].RemoveJob = "full"
		}},
		"list fails": {setup: func(f *fakeReplica) { f.listErr = errors.New("connection reset") }},
	}
	for name, tc := range cases {
		r := newReplicaRig(config.HAModeAuto)
		tc.setup(r.repl)
		for i := range r.repl.jobs {
			if r.repl.jobs[i].Comment == "" {
				r.repl.jobs[i].Comment = renderReplicaJobMarker(aaDirector, 1)
			}
		}
		c := &deadNodeCompletion{deps: r.deps, vmid: 100, source: "pve01", logger: log.NewNopLogger()}
		err := c.confirmReplicaJob(crhCtx(), "pve02")
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err %v, want ok %v", name, err, tc.ok)
		}
	}
}

func TestDeadNodeReplicaCopies(t *testing.T) {
	// VM 100's config moved from dead pve01 to its partner pve02. Only a
	// volume the job copies and pve02 lists counts as living on pve02.
	storages := []pve.StorageInfo{{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"},
		{Name: "nfs", Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	live := map[string]any{"scsi0": replicaRootVolume + ",size=10G", "scsi1": "local-zfs:vm-9001-disk-0,size=5G",
		"scsi2": "nfs:vm-100-disk-2,size=5G"}
	wide := func(c string) string { return ha.WithReplicaPartner(c, "pve02") }
	t.Run("listed copy counts and the persistent disk does not", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01,pve02", wide)
		r.repl.listed[replicaRootVolume] = true
		r.repl.listed["local-zfs:vm-9001-disk-0"] = true
		got, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil)
		if err != nil || len(got) != 1 || !got[replicaRootVolume] {
			t.Fatalf("copies %v err %v; the job never copies a disk ID's volume", got, err)
		}
		if !slices.Equal(r.repl.calls, []string{"volumes:pve02/local-zfs"}) {
			t.Fatalf("calls %v; each storage is listed once", r.repl.calls)
		}
	})
	t.Run("missing copy does not count", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01,pve02", wide)
		got, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("copies %v err %v", got, err)
		}
	})
	t.Run("no replicate pin reads nothing", func(t *testing.T) {
		for name, setup := range map[string]func(r *replicaRig){
			"no pin":          func(*replicaRig) {},
			"source-only pin": func(r *replicaRig) { r.setPin("pve01", nil) },
			"partner is elsewhere": func(r *replicaRig) {
				r.setPin("pve01,pve03", func(c string) string { return ha.WithReplicaPartner(c, "pve03") })
			},
		} {
			r := newReplicaRig(config.HAModeAuto)
			setup(r)
			r.repl.listed[replicaRootVolume] = true
			got, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil)
			if err != nil || len(got) != 0 || len(r.repl.calls) != 0 {
				t.Fatalf("%s: copies %v err %v calls %v", name, got, err, r.repl.calls)
			}
		}
	})
	t.Run("pin of another Director reads nothing", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01,pve02", wide)
		r.deps.RequestDirectorUUID = "another-director"
		r.repl.listed[replicaRootVolume] = true
		got, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil)
		if err != nil || len(got) != 0 || len(r.repl.calls) != 0 {
			t.Fatalf("copies %v err %v calls %v", got, err, r.repl.calls)
		}
	})
	t.Run("unreadable listing is an error", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		r.setPin("pve01,pve02", wide)
		r.repl.volumesErr = errors.New("storage offline")
		if _, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil); err == nil {
			t.Fatal("a listing that fails must be an error")
		}
	})
	t.Run("unreadable rules are an error", func(t *testing.T) {
		r := newReplicaRig(config.HAModeAuto)
		client, ok := r.deps.PVE.(*attachRunClient)
		if !ok {
			t.Fatal("the rig's client is not an attachRunClient")
		}
		client.cluster = &rulesErrCluster{attachHAStub: r.stub, err: errors.New("connection reset")}
		if _, err := deadNodeReplicaCopies(crhCtx(), r.deps, 100, "pve02", storages, live, nil); err == nil {
			t.Fatal("a rule read that fails must be an error")
		}
	})
}
