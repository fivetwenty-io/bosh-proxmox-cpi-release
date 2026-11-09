package ha

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fakeReplication is an in-memory ReplicationAPI. Each call is logged in
// order, so a test can show what a retirement sent and when.
type fakeReplication struct {
	jobs      []ReplicaJobState
	listErr   error
	status    map[string]ReplicaSyncStatus
	statusErr error
	// statusSeq, when set, answers successive status reads in order and then
	// repeats its last entry.
	statusSeq []ReplicaSyncStatus
	listing   map[string]map[string]bool
	listErr2  error
	schedErr  error
	deleteErr error
	createErr error
	// removeOnSchedule makes a job marked for removal disappear when its run
	// is requested, the way PVE's runner removes it.
	removeOnSchedule bool
	// listFailFrom, when set, fails the job list from that read on, counted
	// from 1, with listErr or a default error.
	listFailFrom int
	lists        int
	calls        []string
}

func (f *fakeReplication) log(s string) { f.calls = append(f.calls, s) }

func (f *fakeReplication) ListJobs(context.Context) ([]ReplicaJobState, error) {
	f.log("list")
	f.lists++
	if f.listFailFrom > 0 && f.lists >= f.listFailFrom {
		if f.listErr != nil {
			return nil, f.listErr
		}
		return nil, errors.New("the job list could not be read")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slices.Clone(f.jobs), nil
}

func (f *fakeReplication) CreateJob(_ context.Context, spec ReplicaJobSpec) error {
	f.log("create:" + spec.ID)
	if f.createErr != nil {
		return f.createErr
	}
	guest, num, _ := parseReplicaJobID(spec.ID)
	f.jobs = append(f.jobs, ReplicaJobState{ID: spec.ID, Guest: guest, JobNum: num, Target: spec.Target,
		Comment: spec.Comment})
	return nil
}

func (f *fakeReplication) SetComment(_ context.Context, id, comment string) error {
	f.log("comment:" + id)
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			f.jobs[i].Comment = comment
			return nil
		}
	}
	return errors.New("no such job")
}

func (f *fakeReplication) DeleteJob(_ context.Context, id string, force bool) error {
	if force {
		f.log("force:" + id)
	} else {
		f.log("delete:" + id)
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
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
	return errors.New("no such job")
}

func (f *fakeReplication) ScheduleNow(_ context.Context, node, id string) error {
	f.log("schedule:" + node + ":" + id)
	if f.schedErr != nil {
		return f.schedErr
	}
	if f.removeOnSchedule {
		f.jobs = slices.DeleteFunc(f.jobs, func(j ReplicaJobState) bool { return j.ID == id && j.RemoveJob != "" })
	}
	return nil
}

func (f *fakeReplication) Status(_ context.Context, node, id string) (ReplicaSyncStatus, error) {
	f.log("status:" + node + ":" + id)
	if f.statusErr != nil {
		return ReplicaSyncStatus{}, f.statusErr
	}
	if len(f.statusSeq) > 0 {
		st := f.statusSeq[0]
		if len(f.statusSeq) > 1 {
			f.statusSeq = f.statusSeq[1:]
		}
		return st, nil
	}
	return f.status[node+"/"+id], nil
}

func (f *fakeReplication) Volumes(_ context.Context, node, storage string) (map[string]bool, error) {
	f.log("volumes:" + node + ":" + storage)
	if f.listErr2 != nil {
		return nil, f.listErr2
	}
	return f.listing[node+"/"+storage], nil
}

// fakeClock is a clock a fake sleep advances.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

var replicaT0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func TestReplicaPinTokens(t *testing.T) {
	t.Parallel()
	c := "bosh-cpi:v1 owner=d1 bosh-vols=local-zfs"
	c = WithReplicaPartner(c, "pve02")
	if p, ok := ReplicaPartner(c); !ok || p != "pve02" {
		t.Fatalf("partner = %q, %v", p, ok)
	}
	c = WithReplicaPartner(c, "pve03")
	if strings.Count(c, ReplicaPartnerKey+"=") != 1 {
		t.Fatalf("the partner token repeats: %q", c)
	}
	c = WithReplicaPartner(c, "")
	if _, ok := ReplicaPartner(c); ok || !strings.HasPrefix(c, "bosh-cpi:v1 owner=d1") {
		t.Fatalf("the partner token stays or the marker went: %q", c)
	}
	c = WithReplicaChanged(c, replicaT0)
	if at, ok := ReplicaChanged(c); !ok || !at.Equal(replicaT0) {
		t.Fatalf("changed = %v, %v", at, ok)
	}
	for _, bad := range []string{"bosh-replica-changed=x", "bosh-replica-changed=-4", "bosh-replica-changed=", ""} {
		if _, ok := ReplicaChanged(bad); ok {
			t.Fatalf("%q read as a time", bad)
		}
	}
}

func TestParseReplicaJobs(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		json.RawMessage(`{"id":"101-1","guest":101,"jobnum":1,"target":"b","source":"a","comment":"c","disable":1}`),
		json.RawMessage(`{"id":"101-0","guest":"101","target":"c","remove_job":"full"}`),
	}
	jobs := ParseReplicaJobs(rows)
	if len(jobs) != 2 || jobs[0].ID != "101-0" || jobs[0].RemoveJob != "full" || jobs[1].JobNum != 1 ||
		!jobs[1].Disabled || jobs[1].Source != "a" || jobs[0].Fault != "" || jobs[1].Fault != "" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if n := NextJobNum(jobs, 101); n != 2 {
		t.Fatalf("next job number = %d, want 2", n)
	}
	if n := NextJobNum(jobs, 102); n != 0 {
		t.Fatalf("next job number of a guest with no job = %d", n)
	}
	if got := ReplicaJobID(101, 2); got != "101-2" {
		t.Fatalf("id = %q", got)
	}
	mine, err := GuestJobs(jobs, 101)
	if err != nil || len(mine) != 2 || mine[0].JobNum != 0 {
		t.Fatalf("guest jobs = %+v, %v", mine, err)
	}
	// Each bad row is kept with a fault and the guest it provably names.
	for bad, guest := range map[string]int{
		`{"id":"x","target":"b"}`:                 0,
		`{"id":"x","guest":101,"target":"b"}`:     101,
		`{"id":"101-0","guest":102,"target":"b"}`: 0,
		`{"id":"101-0","guest":101}`:              101,
		`{"id":"0-1","target":"b"}`:               0,
		`{"id":"101--1","target":"b"}`:            0,
		`not json`:                                0,
		`{"id":7}`:                                0,
	} {
		got := ParseReplicaJobs([]json.RawMessage{json.RawMessage(bad)})
		if len(got) != 1 || got[0].Fault == "" || got[0].Guest != guest {
			t.Fatalf("row %s: %+v; want a fault for guest %d", bad, got, guest)
		}
	}
}

// A faulted row blocks only the guest it may belong to, so one guest's broken
// row never stops another guest's replication calls.
func TestGuestJobsScopesFaults(t *testing.T) {
	t.Parallel()
	rows := []json.RawMessage{
		json.RawMessage(`{"id":"101-0","guest":101,"target":"b"}`),
		json.RawMessage(`{"id":"555-0","guest":555}`),
	}
	jobs := ParseReplicaJobs(rows)
	if mine, err := GuestJobs(jobs, 101); err != nil || len(mine) != 1 {
		t.Fatalf("guest 101 = %+v, %v; want its job, with 555's fault ignored", mine, err)
	}
	if _, err := GuestJobs(jobs, 555); err == nil {
		t.Fatal("guest 555's faulted row read as no job")
	}
	unknown := ParseReplicaJobs([]json.RawMessage{rows[0], rows[1], json.RawMessage(`{"id":"x","target":"b"}`)})
	if _, err := GuestJobs(unknown, 101); err == nil {
		t.Fatal("a row of no provable guest did not block the lookup")
	}
	dup := ParseReplicaJobs([]json.RawMessage{
		json.RawMessage(`{"id":"101-0","guest":101,"target":"b"}`),
		json.RawMessage(`{"id":"101-0","guest":101,"target":"c"}`),
		json.RawMessage(`{"id":"202-0","guest":202,"target":"c"}`),
	})
	if _, err := GuestJobs(dup, 101); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("a repeated ID read as one job: %v", err)
	}
	if mine, err := GuestJobs(dup, 202); err != nil || len(mine) != 1 {
		t.Fatalf("guest 202 = %+v, %v", mine, err)
	}
	if n := NextJobNum(unknown, 555); n != 1 {
		t.Fatalf("next job number = %d; a faulted row's number is still taken", n)
	}
	f := &fakeReplication{jobs: dup}
	if _, _, err := findReplicaJob(context.Background(), f, "101-0"); err == nil {
		t.Fatal("findReplicaJob took the first of two rows with one ID")
	}
	if _, _, err := findReplicaJob(context.Background(), f, "bad"); err == nil {
		t.Fatal("findReplicaJob looked up an ID with no guest")
	}
}

func TestParseReplicaSyncStatus(t *testing.T) {
	t.Parallel()
	st, err := ParseReplicaSyncStatus([]byte(`{"last_sync":1794057600,"fail_count":"2","error":" boom "}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSync.Unix() != 1794057600 || st.FailCount != 2 || st.Error != "boom" {
		t.Fatalf("status = %+v", st)
	}
	st, err = ParseReplicaSyncStatus([]byte(`{"last_sync":0}`))
	if err != nil || !st.LastSync.IsZero() {
		t.Fatalf("a job that never synced reads as %+v, %v", st, err)
	}
	for _, bad := range []string{"", "null", "[1]"} {
		if _, err := ParseReplicaSyncStatus([]byte(bad)); err == nil {
			t.Fatalf("status %q parsed", bad)
		}
	}
}

// syncedReplication is a job 101-0 from a to b whose last sync started an
// hour after replicaT0 and whose copy of local-zfs:vm-101-disk-0 is on b.
func syncedReplication() *fakeReplication {
	return &fakeReplication{
		jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}},
		status: map[string]ReplicaSyncStatus{
			"a/101-0": {LastSync: replicaT0.Add(time.Hour)},
		},
		listing: map[string]map[string]bool{"b/local-zfs": {"local-zfs:vm-101-disk-0": true}},
	}
}

func syncCheck() ReplicaSyncCheck {
	return ReplicaSyncCheck{JobID: "101-0", Source: "a", Target: "b", VolIDs: []string{"local-zfs:vm-101-disk-0"},
		ChangedAt: replicaT0}
}

func TestCheckReplicaSyncVerifies(t *testing.T) {
	t.Parallel()
	v, err := CheckReplicaSync(context.Background(), syncedReplication(), syncCheck())
	if err != nil || !v.Verified {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
}

func TestCheckReplicaSyncRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(f *fakeReplication, c *ReplicaSyncCheck)
		reason string
	}{
		{"failed initial sync", func(f *fakeReplication, _ *ReplicaSyncCheck) {
			f.status["a/101-0"] = ReplicaSyncStatus{Error: "zfs recv failed", FailCount: 1}
		}, "error"},
		{"rising failure count", func(f *fakeReplication, _ *ReplicaSyncCheck) {
			f.status["a/101-0"] = ReplicaSyncStatus{LastSync: replicaT0.Add(time.Hour), FailCount: 3}
		}, "failed 3"},
		{"never synced", func(f *fakeReplication, _ *ReplicaSyncCheck) {
			f.status["a/101-0"] = ReplicaSyncStatus{}
		}, "not synced"},
		{"sync started before the change and finished after it", func(f *fakeReplication, c *ReplicaSyncCheck) {
			f.status["a/101-0"] = ReplicaSyncStatus{LastSync: replicaT0.Add(-30 * time.Second)}
		}, "not clearly after"},
		{"sync inside the clock-skew tolerance", func(f *fakeReplication, _ *ReplicaSyncCheck) {
			f.status["a/101-0"] = ReplicaSyncStatus{LastSync: replicaT0.Add(aj.ReplicaClockSkewTolerance)}
		}, "not clearly after"},
		{"attach before the new disk's first sync", func(_ *fakeReplication, c *ReplicaSyncCheck) {
			c.VolIDs = append(c.VolIDs, "local-zfs:vm-101-disk-1")
		}, "vm-101-disk-1 is not on node b"},
		{"no volumes", func(_ *fakeReplication, c *ReplicaSyncCheck) { c.VolIDs = nil }, "no volume"},
		{"no change time", func(_ *fakeReplication, c *ReplicaSyncCheck) { c.ChangedAt = time.Time{} }, "not recorded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, c := syncedReplication(), syncCheck()
			tc.mutate(f, &c)
			v, err := CheckReplicaSync(context.Background(), f, c)
			if err != nil || v.Verified || !strings.Contains(v.Reason, tc.reason) {
				t.Fatalf("verdict = %+v, %v; want a refusal naming %q", v, err, tc.reason)
			}
		})
	}
}

func TestCheckReplicaSyncReadErrorsAndBadInput(t *testing.T) {
	t.Parallel()
	f := syncedReplication()
	f.statusErr = errors.New("node a unreachable")
	if _, err := CheckReplicaSync(context.Background(), f, syncCheck()); err == nil {
		t.Fatal("a status read that failed returned a verdict")
	}
	f = syncedReplication()
	f.listErr2 = errors.New("listing failed")
	if _, err := CheckReplicaSync(context.Background(), f, syncCheck()); err == nil {
		t.Fatal("a target listing that failed returned a verdict")
	}
	bad := []ReplicaSyncCheck{{}, {JobID: "1-0", Source: "a", Target: "a", VolIDs: []string{"s:v"}, ChangedAt: replicaT0}}
	for _, c := range bad {
		if _, err := CheckReplicaSync(context.Background(), syncedReplication(), c); err == nil {
			t.Fatalf("check %+v ran", c)
		}
	}
	if _, err := CheckReplicaSync(context.Background(), nil, syncCheck()); err == nil {
		t.Fatal("a nil client ran")
	}
	c := syncCheck()
	c.VolIDs = []string{"nostorage"}
	if _, err := CheckReplicaSync(context.Background(), syncedReplication(), c); err == nil {
		t.Fatal("a volume with no storage passed")
	}
}

// retirement is a retirement of 101-0 whose VM lives on a, with the target's
// state given by online.
func retirement(clock *fakeClock, online bool, force bool, rec *[]string) ReplicaRetirement {
	return ReplicaRetirement{
		JobID: "101-0", LiveNode: "a", VolIDs: []string{"local-zfs:vm-101-disk-1", "local-zfs:vm-101-disk-0"},
		Online:     func(context.Context, string) (bool, error) { return online, nil },
		AllowForce: force,
		RecordOrphan: func(_ context.Context, node string, volids []string) error {
			*rec = append(*rec, "orphan:"+node+":"+strings.Join(volids, ","))
			return nil
		},
		Budget: 30 * time.Second, PollInterval: 5 * time.Second, Now: clock.now, Sleep: clock.sleep,
	}
}

func TestRetireReplicaJobTargetOnline(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}, removeOnSchedule: true}
	var rec []string
	out, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec))
	if err != nil || !out.Gone || out.Forced || len(rec) != 0 {
		t.Fatalf("outcome = %+v, %v, records %v", out, err, rec)
	}
	if !slices.Contains(f.calls, "delete:101-0") || slices.Contains(f.calls, "force:101-0") {
		t.Fatalf("calls = %v; want a plain delete and no force", f.calls)
	}
}

func TestRetireReplicaJobTargetOfflineForcesAndRecordsTheOrphanFirst(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}}
	var rec []string
	in := retirement(clock, false, true, &rec)
	in.RecordOrphan = func(_ context.Context, node string, volids []string) error {
		f.log("orphan:" + node + ":" + strings.Join(volids, ","))
		return nil
	}
	out, err := RetireReplicaJob(context.Background(), f, in)
	if err != nil || !out.Gone || !out.Forced || out.Orphan != "b" {
		t.Fatalf("outcome = %+v, %v", out, err)
	}
	want := []string{"list", "orphan:b:local-zfs:vm-101-disk-0,local-zfs:vm-101-disk-1", "force:101-0", "list"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestRetireReplicaJobStillThereAfterTheWaitForces(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}}
	var rec []string
	out, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec))
	if err != nil || !out.Forced || len(rec) != 1 {
		t.Fatalf("outcome = %+v, %v, records %v", out, err, rec)
	}
	if !slices.Contains(f.calls, "delete:101-0") {
		t.Fatalf("the plain delete never ran: %v", f.calls)
	}
}

func TestRetireReplicaJobWithoutForceLeavesTheJob(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}}
	var rec []string
	_, err := RetireReplicaJob(context.Background(), f, retirement(clock, false, false, &rec))
	if !errors.Is(err, ErrReplicaRetireIncomplete) || len(rec) != 0 || slices.Contains(f.calls, "force:101-0") {
		t.Fatalf("err = %v, records %v, calls %v", err, rec, f.calls)
	}
}

// TestRetireReplicaJobResumesAfterEachStep crashes the retirement between
// each pair of its steps and runs it again, and the second run always ends
// with the job gone and at most one orphan record.
func TestRetireReplicaJobResumesAfterEachStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		job  *ReplicaJobState
		want []string
	}{
		{"after the journal recorded the retirement", &ReplicaJobState{ID: "101-0", Guest: 101, Target: "b"},
			[]string{"list", "delete:101-0", "schedule:a:101-0", "list"}},
		{"after the plain delete", &ReplicaJobState{ID: "101-0", Guest: 101, Target: "b", RemoveJob: "full"},
			[]string{"list", "schedule:a:101-0", "list"}},
		{"after the force", nil, []string{"list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{t: replicaT0}
			f := &fakeReplication{removeOnSchedule: true}
			if tc.job != nil {
				f.jobs = []ReplicaJobState{*tc.job}
			}
			var rec []string
			out, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec))
			if err != nil || !out.Gone || len(rec) != 0 {
				t.Fatalf("outcome = %+v, %v, records %v", out, err, rec)
			}
			if !slices.Equal(f.calls, tc.want) {
				t.Fatalf("calls = %v, want %v", f.calls, tc.want)
			}
		})
	}
	// A crash after the orphan record and before the force leaves the job,
	// and the resumed run records the same copy again before it forces.
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b", RemoveJob: "full"}}}
	var rec []string
	out, err := RetireReplicaJob(context.Background(), f, retirement(clock, false, true, &rec))
	if err != nil || !out.Forced || len(rec) != 1 {
		t.Fatalf("outcome = %+v, %v, records %v", out, err, rec)
	}
}

func TestRetireReplicaJobAfterAnUnswappedFailover(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Source: "b", Target: "a"}}}
	var rec []string
	var asked string
	in := retirement(clock, false, true, &rec)
	in.Online = func(_ context.Context, node string) (bool, error) { asked = node; return false, nil }
	out, err := RetireReplicaJob(context.Background(), f, in)
	if err != nil || out.Orphan != "b" || asked != "b" {
		t.Fatalf("outcome = %+v, %v, asked %q; want the old source b as the copy left behind", out, err, asked)
	}
}

func TestRetireReplicaJobFailures(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	var rec []string
	if _, err := RetireReplicaJob(context.Background(), nil, retirement(clock, true, true, &rec)); err == nil {
		t.Fatal("a nil client retired")
	}
	if _, err := RetireReplicaJob(context.Background(), &fakeReplication{}, ReplicaRetirement{}); err == nil {
		t.Fatal("an unnamed job retired")
	}
	f := &fakeReplication{listErr: errors.New("list failed")}
	if _, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec)); err == nil {
		t.Fatal("a list that failed retired")
	}
	f = &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}},
		deleteErr: errors.New("permission denied")}
	if _, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec)); err == nil ||
		slices.Contains(f.calls, "force:101-0") {
		t.Fatalf("a refused plain delete went on: %v, %v", err, f.calls)
	}
	f = &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}}
	in := retirement(clock, false, true, &rec)
	in.RecordOrphan = func(context.Context, string, []string) error { return errors.New("journal full") }
	if _, err := RetireReplicaJob(context.Background(), f, in); err == nil || slices.Contains(f.calls, "force:101-0") {
		t.Fatalf("a failed orphan record still forced: %v, %v", err, f.calls)
	}
	f = &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}}
	in = retirement(clock, true, true, &rec)
	in.Online = func(context.Context, string) (bool, error) { return false, errors.New("status unreadable") }
	f.removeOnSchedule = true
	if out, err := RetireReplicaJob(context.Background(), f, in); err != nil || out.Forced {
		t.Fatalf("an unreadable node state did not try the cleaning removal: %+v, %v", out, err)
	}
}

// A job list that keeps failing during the wait proves nothing about the
// job, so the retirement returns the read error and never forces.
func TestRetireReplicaJobUnreadableWaitNeverForces(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "b"}}, listFailFrom: 2}
	var rec []string
	_, err := RetireReplicaJob(context.Background(), f, retirement(clock, true, true, &rec))
	if err == nil || !strings.Contains(err.Error(), "confirm the removal") {
		t.Fatalf("err = %v; want the read error", err)
	}
	if slices.Contains(f.calls, "force:101-0") || len(rec) != 0 {
		t.Fatalf("calls %v, records %v; want no force and no orphan record", f.calls, rec)
	}
}

// A row that names no node besides the live one gets the cleaning removal,
// and is never forced, because the copy it leaves could not be named.
func TestRetireReplicaJobWithNoOtherNodeNeverForces(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: replicaT0}
	f := &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "a"}}}
	var rec []string
	_, err := RetireReplicaJob(context.Background(), f, retirement(clock, false, true, &rec))
	if !errors.Is(err, ErrReplicaRetireIncomplete) {
		t.Fatalf("err = %v; want an incomplete retirement", err)
	}
	if !slices.Contains(f.calls, "delete:101-0") || slices.Contains(f.calls, "force:101-0") || len(rec) != 0 {
		t.Fatalf("calls %v, records %v; want the cleaning removal alone", f.calls, rec)
	}
	f = &fakeReplication{jobs: []ReplicaJobState{{ID: "101-0", Guest: 101, Target: "a"}}, removeOnSchedule: true}
	out, err := RetireReplicaJob(context.Background(), f, retirement(clock, false, true, &rec))
	if err != nil || !out.Gone || out.Forced {
		t.Fatalf("outcome = %+v, %v; want the cleaning removal to finish", out, err)
	}
}

func TestReplicaVolumes(t *testing.T) {
	t.Parallel()
	storages := []pve.StorageInfo{
		{Name: "local-zfs", Type: pve.StorageTypeZFSPool},
		{Name: "nfs", Type: pve.StorageTypeNFS, Shared: true},
		{Name: "local-lvm", Type: pve.StorageTypeLVMThin},
		{Name: "local", Type: pve.StorageTypeDir},
	}
	got, gaps := ReplicaVolumes(101, []VMVolume{
		{Volume: "local-zfs:vm-101-disk-1"}, {Volume: "nfs:101/cloudinit.iso", Kind: VolumeISO},
		{Volume: "local-zfs:vm-101-disk-0"}, {Volume: "local-zfs:vm-101-disk-0"},
		{Volume: "nfs:101/vm-101-disk-5.qcow2", NoReplicate: true},
		{Volume: "local-zfs:base-101-disk-6"},
	}, storages)
	if !slices.Equal(got, []string{"local-zfs:base-101-disk-6", "local-zfs:vm-101-disk-0",
		"local-zfs:vm-101-disk-1"}) || len(gaps) != 0 {
		t.Fatalf("got %v, gaps %v", got, gaps)
	}
	for vol, want := range map[VMVolume]string{
		{Volume: "local-lvm:vm-101-disk-2"}:                     "node-local lvmthin storage",
		{Volume: "local-zfs:vm-101-disk-3", NoReplicate: true}:  "replicate flag off",
		{Volume: "unknown:vm-101-disk-4"}:                       "does not hold",
		{Volume: "nostorage"}:                                   "names no storage",
		{Volume: "local:iso/seed.iso", Kind: VolumeISO}:         "CD-ROM",
		{Volume: "local-zfs:vm-9001-disk-0"}:                    "not owned by VM 101",
		{Volume: "local-zfs:subvol-101-disk-0"}:                 "not owned by VM 101",
		{Volume: "local-zfs:vm-101-disk-7", Kind: VolumeISO}:    "CD-ROM",
		{Volume: "local:101/vm-101-disk-8.qcow2", Legacy: true}: "node-local dir storage",
	} {
		got, gaps := ReplicaVolumes(101, []VMVolume{{Volume: "local-zfs:vm-101-disk-0"}, vol}, storages)
		if !slices.Equal(got, []string{"local-zfs:vm-101-disk-0"}) || len(gaps) != 1 ||
			!strings.Contains(gaps[0], want) || !strings.Contains(gaps[0], vol.Volume) {
			t.Fatalf("%+v: got %v, gaps %v; want a gap that says %q", vol, got, gaps, want)
		}
	}
}

// TestDeletionDestinationsCountsAVerifiedReplica is the three-node case of a
// replicate VM: its node-local disk lives on a and, after a verified sync,
// on b, so the completion names b alone. Before the first sync the pin names
// a alone and carries no replica token, so the completion keeps deferring.
func TestDeletionDestinationsCountsAVerifiedReplica(t *testing.T) {
	t.Parallel()
	idx, err := NewStorageIndex([]pve.StorageInfo{{Name: "local-zfs", Type: pve.StorageTypeZFSPool}})
	if err != nil {
		t.Fatal(err)
	}
	widened := deadNodePin(true, map[string]int{"a": 0, "b": 0})
	widened.Comment = "owner=us bosh-vols=local-zfs bosh-replica=b"
	got, err := DeletionDestinations(context.Background(), "a", deadNodeMembers, deadNodeRules(widened), "vm:101", "bosh-na-101",
		ownedByUs, placeableExcept("a"), idx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Destinations, []string{"b"}) || got.Replica != "b" || got.PinnedToSource() {
		t.Fatalf("got %+v; want b alone", got)
	}
	configured := deadNodePin(true, map[string]int{"a": 0})
	configured.Comment = "owner=us bosh-vols=local-zfs"
	got, err = DeletionDestinations(context.Background(), "a", deadNodeMembers, deadNodeRules(configured), "vm:101", "bosh-na-101",
		ownedByUs, placeableExcept("a"), idx)
	if err != nil || !got.PinnedToSource() || got.Replica != "" {
		t.Fatalf("got %+v, %v; want the source alone before the first sync", got, err)
	}
	// A token naming a node the pin leaves out is no record of a replica the
	// CRM could recover from.
	stray := deadNodePin(true, map[string]int{"a": 0})
	stray.Comment = "owner=us bosh-vols=local-zfs bosh-replica=c"
	got, err = DeletionDestinations(context.Background(), "a", deadNodeMembers, deadNodeRules(stray), "vm:101", "bosh-na-101",
		ownedByUs, placeableExcept("a"), idx)
	if err != nil || !got.PinnedToSource() {
		t.Fatalf("got %+v, %v; want the source alone", got, err)
	}
	// A token that names the dead node itself counts no replica.
	self := deadNodePin(true, map[string]int{"a": 0, "b": 0})
	self.Comment = "owner=us bosh-vols=local-zfs bosh-replica=a"
	got, err = DeletionDestinations(context.Background(), "a", deadNodeMembers, deadNodeRules(self), "vm:101", "bosh-na-101",
		ownedByUs, placeableExcept("a"), idx)
	if err != nil || !got.PinnedToSource() || got.Replica != "" {
		t.Fatalf("got %+v, %v; want the source alone for a token naming the dead node", got, err)
	}
	// A recorded node-local storage that is not a ZFS pool has no replica,
	// whatever the token says.
	mixed, err := NewStorageIndex([]pve.StorageInfo{
		{Name: "local-zfs", Type: pve.StorageTypeZFSPool}, {Name: "local-lvm", Type: pve.StorageTypeLVMThin},
	})
	if err != nil {
		t.Fatal(err)
	}
	lvm := deadNodePin(true, map[string]int{"a": 0, "b": 0})
	lvm.Comment = "owner=us bosh-vols=local-lvm,local-zfs bosh-replica=b"
	got, err = DeletionDestinations(context.Background(), "a", deadNodeMembers, deadNodeRules(lvm), "vm:101", "bosh-na-101",
		ownedByUs, placeableExcept("a"), mixed)
	if err != nil || !got.PinnedToSource() || got.Replica != "" {
		t.Fatalf("got %+v, %v; want the source alone with a non-ZFS node-local storage recorded", got, err)
	}
}
