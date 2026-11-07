package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ReplicaPartnerKey is the key of the comment token on a replicate VM's pin
// that names the partner node holding a verified replica. The CPI writes it
// only together with a pin widened to the partner, after a sync it verified,
// and removes it whenever it narrows the pin, so a pin that carries it says
// the partner's copy covered every volume the VM held when the pin was last
// written.
const ReplicaPartnerKey = "bosh-replica"

// ReplicaChangedKey is the key of the comment token on a replicate VM's pin
// that records, in Unix seconds on the CPI's clock, when the VM's volume set
// last changed. A VM with no allocation journal record has nowhere else to
// keep that time, and a sync only counts once it started clearly after it.
const ReplicaChangedKey = "bosh-replica-changed"

// ReplicaJobType is the only replication job type PVE offers.
const ReplicaJobType = "local"

// withCommentToken returns comment with every key token replaced by one that
// carries value, or with every key token removed when value is empty.
func withCommentToken(comment, key, value string) string {
	fields := strings.Fields(comment)
	kept := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		if k, _, ok := strings.Cut(f, "="); ok && k == key {
			continue
		}
		kept = append(kept, f)
	}
	if value != "" {
		kept = append(kept, key+"="+value)
	}
	return strings.Join(kept, " ")
}

// commentToken returns the value of the first key token in comment.
func commentToken(comment, key string) (string, bool) {
	for _, f := range strings.Fields(comment) {
		k, v, ok := strings.Cut(f, "=")
		if ok && k == key && v != "" {
			return v, true
		}
	}
	return "", false
}

// WithReplicaPartner returns comment with its partner token naming partner,
// or with the token removed when partner is empty.
func WithReplicaPartner(comment, partner string) string {
	return withCommentToken(comment, ReplicaPartnerKey, strings.TrimSpace(partner))
}

// ReplicaPartner returns the partner a pin's comment names as holding a
// verified replica.
func ReplicaPartner(comment string) (string, bool) {
	return commentToken(comment, ReplicaPartnerKey)
}

// WithReplicaChanged returns comment with its volume-change token set to at.
func WithReplicaChanged(comment string, at time.Time) string {
	return withCommentToken(comment, ReplicaChangedKey, strconv.FormatInt(at.Unix(), 10))
}

// ReplicaChanged returns the volume-change time a pin's comment records. A
// token that does not parse as a positive Unix time reads as missing, which
// leaves the VM's replica unproven.
func ReplicaChanged(comment string) (time.Time, bool) {
	raw, ok := commentToken(comment, ReplicaChangedKey)
	if !ok {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

// ReplicaJobState is one row of PVE's replication job configuration.
type ReplicaJobState struct {
	ID        string
	Guest     int
	JobNum    int
	Target    string
	Source    string
	Comment   string
	RemoveJob string
	Disabled  bool
	// Fault names why the row cannot be used, such as a malformed field or an
	// ID listed twice. Guest is then the guest the row provably names, and 0
	// when it names none.
	Fault string
}

// ReplicaSyncStatus is the part of a job's status on its source node that the
// sync gate reads. LastSync is the start of the last successful run, and it is
// zero when the job never synced.
type ReplicaSyncStatus struct {
	LastSync  time.Time
	FailCount int64
	Error     string
}

// ReplicationAPI is the PVE surface the replicate policy uses.
type ReplicationAPI interface {
	// ListJobs returns every replication job of the cluster.
	ListJobs(ctx context.Context) ([]ReplicaJobState, error)
	// CreateJob creates a local replication job.
	CreateJob(ctx context.Context, spec ReplicaJobSpec) error
	// SetComment rewrites a job's comment.
	SetComment(ctx context.Context, id, comment string) error
	// DeleteJob deletes a job. Without force PVE marks it for a full removal,
	// which the job's source node runs, and with force PVE drops the job's
	// config and cleans up nothing.
	DeleteJob(ctx context.Context, id string, force bool) error
	// ScheduleNow asks the job's source node to run it now.
	ScheduleNow(ctx context.Context, node, id string) error
	// Status reads a job's status on node, which must be the job's source.
	Status(ctx context.Context, node, id string) (ReplicaSyncStatus, error)
	// Volumes lists the volume IDs a storage holds on node.
	Volumes(ctx context.Context, node, storage string) (map[string]bool, error)
}

// ReplicaJobSpec is a job the CPI creates.
type ReplicaJobSpec struct {
	ID       string
	Target   string
	Schedule string
	RateMbps float64
	Comment  string
}

// ReplicaJobID returns the PVE job ID of guest's job number jobnum.
func ReplicaJobID(guest, jobnum int) string {
	return strconv.Itoa(guest) + "-" + strconv.Itoa(jobnum)
}

// GuestJobs returns the usable jobs of guest, sorted by job number. A faulted
// row that belongs to guest, or to no provable guest, is an error, because it
// could be the job a caller looks for. A faulted row that provably belongs to
// another guest is ignored, so that guest's oddity never blocks this one.
func GuestJobs(jobs []ReplicaJobState, guest int) ([]ReplicaJobState, error) {
	var out []ReplicaJobState
	for _, j := range jobs {
		switch {
		case j.Fault != "" && (j.Guest == 0 || j.Guest == guest):
			return nil, fmt.Errorf("the replication jobs of guest %d cannot be told apart: %s", guest, j.Fault)
		case j.Fault != "":
			continue
		case j.Guest == guest:
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].JobNum < out[b].JobNum })
	return out, nil
}

// NextJobNum returns the lowest job number no row of guest uses, faulted rows
// included.
func NextJobNum(jobs []ReplicaJobState, guest int) int {
	used := map[int]bool{}
	for _, j := range jobs {
		if j.Guest == guest {
			used[j.JobNum] = true
		}
	}
	n := 0
	for used[n] {
		n++
	}
	return n
}

// replicationClient is ReplicationAPI over a CPI client.
type replicationClient struct{ c pve.Client }

// NewReplicationAPI returns the replication surface of c. Reads retry
// transient failures; writes do not, because a write PVE may have applied is
// settled by the caller's re-read rather than sent twice.
func NewReplicationAPI(c pve.Client) ReplicationAPI { return &replicationClient{c: c} }

func (r *replicationClient) ListJobs(ctx context.Context) ([]ReplicaJobState, error) {
	var resp *sdkcluster.ListReplicationResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_replication_list", 0, func() error {
		var inner error
		resp, inner = r.c.Cluster().ListReplication(ctx)
		return inner
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("the replication job list returned nothing")
	}
	return ParseReplicaJobs(*resp), nil
}

// ParseReplicaJobs reads the rows of GET /cluster/replication. A row that
// does not parse, that carries no <guest>-<jobnum> ID or no target, that names
// a guest its ID disagrees with, or whose ID another row repeats, is kept with
// its Fault set and the guest it provably names, rather than skipped, because
// a skipped row could be the job a caller looks for. GuestJobs turns a fault
// into an error only for the guest it may belong to.
func ParseReplicaJobs(rows []json.RawMessage) []ReplicaJobState {
	jobs := make([]ReplicaJobState, 0, len(rows))
	byID := map[string]int{}
	for i, raw := range rows {
		job := parseReplicaJobRow(raw, i)
		if job.Fault == "" {
			if first, dup := byID[job.ID]; dup {
				fault := fmt.Sprintf("replication job %q is listed twice", job.ID)
				job.Fault = fault
				jobs[first].Fault = fault
			} else {
				byID[job.ID] = len(jobs)
			}
		}
		jobs = append(jobs, job)
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
	return jobs
}

// parseReplicaJobRow reads one row of the job list, which is entry i.
func parseReplicaJobRow(raw json.RawMessage, i int) ReplicaJobState {
	var row struct {
		ID        string             `json:"id"`
		Guest     sdkclient.PVEInt   `json:"guest"`
		JobNum    sdkclient.PVEInt   `json:"jobnum"`
		Target    string             `json:"target"`
		Source    string             `json:"source"`
		Comment   string             `json:"comment"`
		RemoveJob string             `json:"remove_job"`
		Disable   *sdkclient.PVEBool `json:"disable"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		guest, _ := replicaRowGuest(raw)
		return ReplicaJobState{Guest: guest, Fault: fmt.Sprintf("replication job entry %d is unreadable: %v", i, err)}
	}
	guest, num, ok := parseReplicaJobID(row.ID)
	named := int(max(row.Guest.Int(), 0))
	job := ReplicaJobState{
		ID: row.ID, Guest: guest, JobNum: num, Target: row.Target, Source: row.Source, Comment: row.Comment,
		RemoveJob: row.RemoveJob, Disabled: row.Disable != nil && row.Disable.Bool(),
	}
	switch {
	case !ok:
		job.Guest = named
		job.Fault = fmt.Sprintf("replication job %q has no <guest>-<jobnum> ID", row.ID)
	case named != 0 && named != guest:
		job.Guest = 0
		job.Fault = fmt.Sprintf("replication job %q names guest %d", row.ID, named)
	case strings.TrimSpace(row.Target) == "":
		job.Fault = fmt.Sprintf("replication job %q names no target", row.ID)
	}
	return job
}

// replicaRowGuest reads the guest an unreadable row names in its ID or its
// guest field, and is 0 when the row names none or names two that disagree.
func replicaRowGuest(raw json.RawMessage) (int, bool) {
	var row struct {
		ID    json.RawMessage `json:"id"`
		Guest json.RawMessage `json:"guest"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return 0, false
	}
	fromID, fromGuest := 0, 0
	var id string
	if json.Unmarshal(row.ID, &id) == nil {
		if g, _, ok := parseReplicaJobID(id); ok {
			fromID = g
		}
	}
	var guest sdkclient.PVEInt
	if json.Unmarshal(row.Guest, &guest) == nil && guest.Int() > 0 {
		fromGuest = int(guest.Int())
	}
	switch {
	case fromID != 0 && fromGuest != 0 && fromID != fromGuest:
		return 0, false
	case fromID != 0:
		return fromID, true
	case fromGuest != 0:
		return fromGuest, true
	}
	return 0, false
}

// parseReplicaJobID splits "<guest>-<jobnum>".
func parseReplicaJobID(id string) (int, int, bool) {
	g, n, ok := strings.Cut(id, "-")
	if !ok {
		return 0, 0, false
	}
	guest, err := strconv.Atoi(g)
	if err != nil || guest <= 0 {
		return 0, 0, false
	}
	num, err := strconv.Atoi(n)
	if err != nil || num < 0 {
		return 0, 0, false
	}
	return guest, num, true
}

func (r *replicationClient) CreateJob(ctx context.Context, spec ReplicaJobSpec) error {
	p := &sdkcluster.CreateReplicationParams{Id: spec.ID, Target: spec.Target, Type: ReplicaJobType}
	if s := strings.TrimSpace(spec.Schedule); s != "" {
		p.Schedule = &s
	}
	if spec.RateMbps > 0 {
		rate := spec.RateMbps
		p.Rate = &rate
	}
	if spec.Comment != "" {
		comment := spec.Comment
		p.Comment = &comment
	}
	return r.c.Cluster().CreateReplication(ctx, p)
}

func (r *replicationClient) SetComment(ctx context.Context, id, comment string) error {
	return r.c.Cluster().UpdateReplication(ctx, id, &sdkcluster.UpdateReplicationParams{Comment: &comment})
}

func (r *replicationClient) DeleteJob(ctx context.Context, id string, force bool) error {
	var p *sdkcluster.DeleteReplicationParams
	if force {
		p = &sdkcluster.DeleteReplicationParams{Force: &force}
	}
	return r.c.Cluster().DeleteReplication(ctx, id, p)
}

func (r *replicationClient) ScheduleNow(ctx context.Context, node, id string) error {
	_, err := r.c.Nodes().CreateReplicationScheduleNow(ctx, node, id)
	return err
}

func (r *replicationClient) Status(ctx context.Context, node, id string) (ReplicaSyncStatus, error) {
	var raw []byte
	err := pve.RetryOnTransient(ctx, nil, "ha_replication_status", 0, func() error {
		resp, inner := r.c.Nodes().ListReplicationStatus(ctx, node, id)
		if inner == nil && resp != nil {
			raw = *resp
		}
		return inner
	})
	if err != nil {
		return ReplicaSyncStatus{}, err
	}
	return ParseReplicaSyncStatus(raw)
}

// ParseReplicaSyncStatus reads a job status object. A status with no body is
// an error, because a missing status proves no sync.
func ParseReplicaSyncStatus(raw []byte) (ReplicaSyncStatus, error) {
	if len(strings.TrimSpace(string(raw))) == 0 || string(raw) == "null" {
		return ReplicaSyncStatus{}, errors.New("the replication job status returned nothing")
	}
	var row struct {
		LastSync  *sdkclient.PVEInt `json:"last_sync"`
		FailCount *sdkclient.PVEInt `json:"fail_count"`
		Error     string            `json:"error"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return ReplicaSyncStatus{}, fmt.Errorf("the replication job status is unreadable: %w", err)
	}
	var st ReplicaSyncStatus
	if row.LastSync != nil && row.LastSync.Int() > 0 {
		st.LastSync = time.Unix(row.LastSync.Int(), 0).UTC()
	}
	if row.FailCount != nil {
		st.FailCount = row.FailCount.Int()
	}
	st.Error = strings.TrimSpace(row.Error)
	return st, nil
}

func (r *replicationClient) Volumes(ctx context.Context, node, storage string) (map[string]bool, error) {
	var listing []json.RawMessage
	err := pve.RetryOnTransient(ctx, nil, "ha_replication_target_listing", 0, func() error {
		resp, inner := r.c.Nodes().ListStorageContent(ctx, node, storage, nil)
		if inner == nil {
			if resp == nil || *resp == nil {
				return fmt.Errorf("the content listing of storage %s on node %s returned nothing", storage, node)
			}
			listing = *resp
		}
		return inner
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(listing))
	for _, raw := range listing {
		var item struct {
			Volid string `json:"volid"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Volid == "" {
			return nil, fmt.Errorf("the content listing of storage %s on node %s is malformed", storage, node)
		}
		out[item.Volid] = true
	}
	return out, nil
}

// ReplicaSyncCheck names what one sync gate checks: the job, the node it runs
// from now, the node its copy is on, and the volume set it must cover with
// the time that set changed on the CPI's clock.
type ReplicaSyncCheck struct {
	JobID     string
	Source    string
	Target    string
	VolIDs    []string
	ChangedAt time.Time
}

// ReplicaSyncVerdict is the answer of the sync gate. Verified is true only
// when every test passed, and Reason names the first test that failed.
type ReplicaSyncVerdict struct {
	Verified bool
	Reason   string
	Status   ReplicaSyncStatus
}

// ReplicaSyncEarliest returns the earliest last_sync that can count for a
// volume set that changed at changedAt.
func ReplicaSyncEarliest(changedAt time.Time) time.Time {
	return changedAt.Add(aj.ReplicaClockSkewTolerance)
}

// CheckReplicaSync runs the sync gate once. A sync covers the volume set when
// the job's status on its source shows a last_sync later than the set's
// change by more than the clock-skew tolerance, no error, a failure count of
// zero, and when every volume of the set is on the target's listing. A read
// that fails returns its error and no verdict, so the caller can tell an
// unproven replica from an unread one.
func CheckReplicaSync(ctx context.Context, api ReplicationAPI, c ReplicaSyncCheck) (ReplicaSyncVerdict, error) {
	switch {
	case api == nil:
		return ReplicaSyncVerdict{}, errors.New("replica sync gate: no replication client")
	case c.JobID == "" || c.Source == "" || c.Target == "":
		return ReplicaSyncVerdict{}, errors.New("replica sync gate: the job, its source, and its target must be named")
	case c.Source == c.Target:
		return ReplicaSyncVerdict{}, fmt.Errorf("replica sync gate: job %s runs from and to node %s", c.JobID, c.Source)
	case len(c.VolIDs) == 0:
		return ReplicaSyncVerdict{Reason: "the VM holds no volume the job replicates"}, nil
	case c.ChangedAt.IsZero():
		return ReplicaSyncVerdict{Reason: "the time the VM's volume set changed is not recorded"}, nil
	}
	st, err := api.Status(ctx, c.Source, c.JobID)
	if err != nil {
		return ReplicaSyncVerdict{}, fmt.Errorf("read the status of replication job %s on node %s: %w", c.JobID,
			c.Source, err)
	}
	v := ReplicaSyncVerdict{Status: st}
	earliest := ReplicaSyncEarliest(c.ChangedAt)
	switch {
	case st.Error != "":
		v.Reason = "the job reports an error: " + st.Error
		return v, nil
	case st.FailCount != 0:
		v.Reason = fmt.Sprintf("the job has failed %d time(s) since its last successful sync", st.FailCount)
		return v, nil
	case st.LastSync.IsZero():
		v.Reason = "the job has not synced yet"
		return v, nil
	case !st.LastSync.After(earliest):
		v.Reason = fmt.Sprintf("the last sync started at %s, which is not clearly after the volume set changed "+
			"at %s", st.LastSync.Format(time.RFC3339), c.ChangedAt.UTC().Format(time.RFC3339))
		return v, nil
	}
	byStorage := map[string][]string{}
	for _, volid := range c.VolIDs {
		storage, _, ok := strings.Cut(volid, ":")
		if !ok || storage == "" {
			return ReplicaSyncVerdict{}, fmt.Errorf("replica sync gate: volume %q names no storage", volid)
		}
		byStorage[storage] = append(byStorage[storage], volid)
	}
	storages := make([]string, 0, len(byStorage))
	for s := range byStorage {
		storages = append(storages, s)
	}
	sort.Strings(storages)
	for _, storage := range storages {
		listed, err := api.Volumes(ctx, c.Target, storage)
		if err != nil {
			return ReplicaSyncVerdict{}, fmt.Errorf("list storage %s on target node %s: %w", storage, c.Target, err)
		}
		for _, volid := range byStorage[storage] {
			if !listed[volid] {
				v.Reason = fmt.Sprintf("volume %s is not on node %s yet", volid, c.Target)
				return v, nil
			}
		}
	}
	v.Verified = true
	return v, nil
}

// sleepCtx pauses for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ReplicaRetirement is one retirement of a replication job the CPI owns.
type ReplicaRetirement struct {
	// JobID names the job, and LiveNode is the node the VM's live copy is on.
	JobID    string
	LiveNode string
	// VolIDs are the volumes the job copies, which a forced removal leaves on
	// the other node.
	VolIDs []string
	// Online answers whether the job's other node is online. An error reads
	// as online, so the retirement tries the removal that cleans up.
	Online func(ctx context.Context, node string) (bool, error)
	// AllowForce lets the retirement drop the job with force=1 when the plain
	// removal cannot finish. Only a VM that is being destroyed allows it.
	AllowForce bool
	// RecordOrphan records the copy a forced removal leaves behind before the
	// force is sent. An error stops the retirement before the force.
	RecordOrphan func(ctx context.Context, node string, volids []string) error
	Budget       time.Duration
	PollInterval time.Duration
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
	Logger       *log.Logger
}

// ReplicaRetireOutcome is what a retirement did.
type ReplicaRetireOutcome struct {
	// Gone is true when the job is no longer in PVE's configuration.
	Gone bool
	// Forced is true when the job was dropped with force=1, and Orphan names
	// the node that keeps the copy then.
	Forced bool
	Orphan string
}

// ErrReplicaRetireIncomplete marks a retirement that could not remove the job
// and was not allowed to force it.
var ErrReplicaRetireIncomplete = errors.New("the replication job is still configured")

// RetireReplicaJob runs steps 3 and 4 of a retirement. The caller has settled
// HA and power, confirmed which copy is live, and recorded in the journal that
// the job no longer names a usable replica. The job is deleted without keep,
// so PVE's runner on the live node removes the other node's copy, and the
// retirement asks for that run now and waits up to the budget for the job to
// disappear. A job already marked for removal by an earlier attempt is not
// deleted again. When the other node is offline, or the job is still there
// after the wait, and AllowForce is set, the copy left behind is recorded and
// logged first and the job is dropped with force=1. Without AllowForce the
// retirement returns ErrReplicaRetireIncomplete and leaves the job. A job
// whose row names no node besides the live one is never forced, because the
// copy it leaves could not be named; it gets the removal that cleans up, and
// ErrReplicaRetireIncomplete when that does not finish. A wait whose job list
// could not be read returns the read error and never forces. A job that is
// already gone returns Gone with nothing done. The caller passes only a job
// it owns.
func RetireReplicaJob(ctx context.Context, api ReplicationAPI, in ReplicaRetirement) (ReplicaRetireOutcome, error) {
	if api == nil {
		return ReplicaRetireOutcome{}, errors.New("replica retirement: no replication client")
	}
	if in.JobID == "" || in.LiveNode == "" {
		return ReplicaRetireOutcome{}, errors.New("replica retirement: the job and the live node must be named")
	}
	in = in.withDefaults()
	job, found, err := findReplicaJob(ctx, api, in.JobID)
	if err != nil {
		return ReplicaRetireOutcome{}, err
	}
	if !found {
		return ReplicaRetireOutcome{Gone: true}, nil
	}
	other := job.Target
	if other == in.LiveNode {
		// PVE has not swapped the job after a failover yet, so the copy that
		// is not live is on the job's recorded source.
		other = job.Source
	}
	if other == "" {
		// The row names no node besides the live one, so the copy a forced
		// drop would leave cannot be named or recorded. The removal that
		// cleans up is still tried, and the force is never sent.
		in.Logger.Warn("replicate: the replication job names no node besides the live one; trying the removal "+
			"that cleans up, without a forced drop", log.String("job", in.JobID), log.String("node", in.LiveNode))
		out, done, err := retireWithCleanup(ctx, api, in, job)
		if err != nil || done {
			return out, err
		}
		return ReplicaRetireOutcome{}, fmt.Errorf("%w: job %s names no node that holds its copy, so the CPI does "+
			"not drop it with force; remove the job and the copy by hand", ErrReplicaRetireIncomplete, in.JobID)
	}
	if replicaPartnerOnline(ctx, in, other) {
		out, done, err := retireWithCleanup(ctx, api, in, job)
		if err != nil || done {
			return out, err
		}
	}
	if !in.AllowForce {
		return ReplicaRetireOutcome{}, fmt.Errorf("%w: job %s did not finish its removal of the copy on node %s "+
			"within %s", ErrReplicaRetireIncomplete, in.JobID, other, in.Budget)
	}
	return forceRetireReplicaJob(ctx, api, in, other)
}

// withDefaults fills the clock, sleep, poll interval, and logger a caller
// left unset.
func (in ReplicaRetirement) withDefaults() ReplicaRetirement {
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.Sleep == nil {
		in.Sleep = sleepCtx
	}
	if in.PollInterval <= 0 {
		in.PollInterval = 5 * time.Second
	}
	if in.Logger == nil {
		in.Logger = log.NewNopLogger()
	}
	return in
}

// replicaPartnerOnline reports whether the node holding the copy that is not
// live is up. A failed read counts as up, so the retirement tries the removal
// that cleans the copy before it considers the force.
func replicaPartnerOnline(ctx context.Context, in ReplicaRetirement, other string) bool {
	if in.Online == nil {
		return true
	}
	up, err := in.Online(ctx, other)
	if err != nil {
		in.Logger.Warn("replicate: the state of the replica node could not be read; trying the removal that "+
			"cleans it up", log.String("job", in.JobID), log.String("node", other), log.Err(err))
		return true
	}
	return up
}

// retireWithCleanup deletes the job without keep, asks for its run now, and
// waits up to the budget for it to disappear. It reports done when the job
// is gone; a job still there after the wait returns not done and no error.
// A wait whose last read of the job list failed returns that error, because
// the job may be gone already and a forced drop would rest on no evidence.
func retireWithCleanup(
	ctx context.Context, api ReplicationAPI, in ReplicaRetirement, job ReplicaJobState,
) (ReplicaRetireOutcome, bool, error) {
	if job.RemoveJob == "" {
		if err := api.DeleteJob(ctx, in.JobID, false); err != nil {
			_, still, rerr := findReplicaJob(ctx, api, in.JobID)
			if rerr != nil {
				return ReplicaRetireOutcome{}, false, fmt.Errorf("delete replication job %s: %w (and the re-read "+
					"failed: %w)", in.JobID, err, rerr)
			}
			if !still {
				return ReplicaRetireOutcome{Gone: true}, true, nil
			}
			return ReplicaRetireOutcome{}, false, fmt.Errorf("delete replication job %s: %w", in.JobID, err)
		}
	}
	if err := api.ScheduleNow(ctx, in.LiveNode, in.JobID); err != nil {
		in.Logger.Info("replicate: the request to run the job's removal now failed; its schedule runs it",
			log.String("job", in.JobID), log.String("node", in.LiveNode), log.Err(err))
	}
	deadline := in.Now().Add(in.Budget)
	for {
		_, still, rerr := findReplicaJob(ctx, api, in.JobID)
		if rerr == nil && !still {
			return ReplicaRetireOutcome{Gone: true}, true, nil
		}
		if !in.Now().Add(in.PollInterval).Before(deadline) {
			if rerr != nil {
				return ReplicaRetireOutcome{}, false, fmt.Errorf("confirm the removal of replication job %s: %w",
					in.JobID, rerr)
			}
			return ReplicaRetireOutcome{}, false, nil
		}
		if err := in.Sleep(ctx, in.PollInterval); err != nil {
			return ReplicaRetireOutcome{}, false, err
		}
	}
}

// forceRetireReplicaJob records the copy left on other, drops the job with
// force=1, and confirms it is gone.
func forceRetireReplicaJob(
	ctx context.Context, api ReplicationAPI, in ReplicaRetirement, other string,
) (ReplicaRetireOutcome, error) {
	volids := slices.Clone(in.VolIDs)
	sort.Strings(volids)
	if in.RecordOrphan != nil {
		if err := in.RecordOrphan(ctx, other, volids); err != nil {
			return ReplicaRetireOutcome{}, fmt.Errorf("record the copy that replication job %s leaves on node %s: "+
				"%w", in.JobID, other, err)
		}
	}
	if err := api.DeleteJob(ctx, in.JobID, true); err != nil {
		_, still, rerr := findReplicaJob(ctx, api, in.JobID)
		if rerr != nil || still {
			return ReplicaRetireOutcome{}, fmt.Errorf("force the removal of replication job %s: %w", in.JobID, err)
		}
	}
	if _, still, err := findReplicaJob(ctx, api, in.JobID); err != nil {
		return ReplicaRetireOutcome{}, fmt.Errorf("confirm the forced removal of replication job %s: %w",
			in.JobID, err)
	} else if still {
		return ReplicaRetireOutcome{}, fmt.Errorf("replication job %s is still configured after its forced "+
			"removal", in.JobID)
	}
	in.Logger.Warn("replicate: the replication job was dropped without cleanup, and its copy stays on the node; "+
		"remove those volumes by hand once the node is back", log.String("job", in.JobID),
		log.String("node", other), log.String("volumes", strings.Join(volids, ",")))
	return ReplicaRetireOutcome{Gone: true, Forced: true, Orphan: other}, nil
}

// findReplicaJob reads the job id from the cluster's job list. A faulted row
// that may belong to the job's guest is an error, because it may be the job.
func findReplicaJob(ctx context.Context, api ReplicationAPI, id string) (ReplicaJobState, bool, error) {
	guest, _, ok := parseReplicaJobID(id)
	if !ok {
		return ReplicaJobState{}, false, fmt.Errorf("replication job %q has no <guest>-<jobnum> ID", id)
	}
	jobs, err := api.ListJobs(ctx)
	if err != nil {
		return ReplicaJobState{}, false, fmt.Errorf("list the replication jobs: %w", err)
	}
	mine, err := GuestJobs(jobs, guest)
	if err != nil {
		return ReplicaJobState{}, false, err
	}
	for _, j := range mine {
		if j.ID == id {
			return j, true, nil
		}
	}
	return ReplicaJobState{}, false, nil
}

// ReplicaVolumes splits the volumes of guest into the ones a replication job
// of guest copies and the gaps, the node-local volumes it leaves on the
// source. PVE replicates a volume only when it sits on node-local ZFS pool
// storage, is not a CD-ROM, has its replicate flag on, and is owned by the
// guest itself (get_replicatable_volumes in qemu-server), so a persistent disk
// the CPI names after a disk ID is a gap. A volume on shared storage needs no
// copy and is in neither list. A volume on a storage the index does not hold,
// or one that names no storage, is a gap, because nothing proves it is
// shared. Both answers are sorted, and each gap is a sentence that names the
// volume.
func ReplicaVolumes(guest int, volumes []VMVolume, storages []pve.StorageInfo) (replicated, gaps []string) {
	byName := make(map[string]pve.StorageInfo, len(storages))
	for i := range storages {
		byName[storages[i].Name] = storages[i]
	}
	seen := map[string]bool{}
	for _, v := range volumes {
		if seen[v.Volume] {
			continue
		}
		seen[v.Volume] = true
		if gap := replicaGap(guest, v, byName); gap != "" {
			gaps = append(gaps, gap)
			continue
		}
		storage, _, _ := strings.Cut(v.Volume, ":")
		if info := byName[storage]; !info.IsShared() {
			replicated = append(replicated, v.Volume)
		}
	}
	sort.Strings(replicated)
	sort.Strings(gaps)
	return replicated, gaps
}

// replicaGap names why a replication job of guest leaves v on the source, and
// is empty when the job copies v or v is on shared storage.
func replicaGap(guest int, v VMVolume, byName map[string]pve.StorageInfo) string {
	storage, _, ok := strings.Cut(v.Volume, ":")
	if !ok || storage == "" {
		return fmt.Sprintf("volume %s names no storage, so nothing shows a replica of it", v.Volume)
	}
	info, known := byName[storage]
	switch {
	case !known:
		return fmt.Sprintf("volume %s is on storage %s, which the storage index does not hold", v.Volume, storage)
	case info.IsShared():
		return ""
	case v.kind() == VolumeISO:
		return fmt.Sprintf("volume %s is a node-local CD-ROM, which replication does not copy", v.Volume)
	case info.Type != pve.StorageTypeZFSPool:
		return fmt.Sprintf("volume %s is on node-local %s storage, which replication does not copy", v.Volume,
			info.Type)
	case v.NoReplicate:
		return fmt.Sprintf("volume %s has its replicate flag off", v.Volume)
	}
	if owner, ok := pve.VolumeOwnerVMID(v.Volume); !ok || owner != guest {
		return fmt.Sprintf("volume %s is not owned by VM %d, and replication copies only the volumes a guest "+
			"owns", v.Volume, guest)
	}
	return ""
}
