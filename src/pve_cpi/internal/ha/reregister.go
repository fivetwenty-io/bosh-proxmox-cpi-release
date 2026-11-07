package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// This file holds the re-registration order that clears the error state of a
// running VM that BOSH keeps. PVE accepts only disabled for a resource in
// error, and disabled stops the guest, so the order removes the resource,
// which clears the error and leaves the guest running, and registers it again
// through the staging order:
//
//  1. Write the re-registration tag and the record of the resource to the VM
//     config. A failed write fails the call, and nothing is removed.
//  2. Remove the resource with purge and wait for the CRM to drop it.
//  3. Stage the resource as ignored with the ownership marker.
//  4. Write the VM's pin and its anti-affinity membership, and verify them.
//  5. Apply the watchdog check and request started.
//  6. Check the landing node, then remove the tag and the record.
//
// The tag lives on the VM config because the ownership marker in the
// resource's comment disappears with the resource. A crash after the removal
// leaves a running VM with no HA resource, and the tag is what makes that
// state visible to the next call and to the deferred-teardown sweep, which
// finish the order from the state they find.

// ReaddTagPrefix starts the re-registration tag, bosh-ha-readd-<unix seconds>.
const ReaddTagPrefix = "bosh-ha-readd-"

// readdTagPattern matches a re-registration tag and nothing else.
var readdTagPattern = regexp.MustCompile(`^bosh-ha-readd-(\d{1,19})$`)

// ReaddRecordKey is the key of the re-registration record in the BOSH
// sentinel of the VM's description.
const ReaddRecordKey = "bosh_ha_readd"

// Errors the re-registration wraps, so a caller can tell them apart.
var (
	// ErrReaddBlocked means the re-registration cannot go on for a reason
	// that a retry alone does not clear, such as a profile that may not write
	// HA state, a decision that gives the VM no HA, or a node outside the VM's
	// admissible set. The VM keeps what it has, and the error names what the
	// operator can do.
	ErrReaddBlocked = errors.New("the HA re-registration cannot go on")

	// ErrReaddVMStopped means the owner node reports the VM stopped, so there
	// is no running guest to keep and the caller's disabled path applies.
	ErrReaddVMStopped = errors.New("the VM is stopped, so its HA error clears with disabled")
)

// ReaddTag renders the re-registration tag for t. A time before the epoch
// renders as the epoch, so every tag the CPI writes parses back.
func ReaddTag(t time.Time) string {
	return ReaddTagPrefix + strconv.FormatInt(max(t.Unix(), 0), 10)
}

// ReaddTagTime parses a re-registration tag and reports whether tag is one.
func ReaddTagTime(tag string) (time.Time, bool) {
	m := readdTagPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(tag)))
	if m == nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// ReaddTagOf returns the oldest re-registration tag in tags and its time, and
// false when tags carry none.
func ReaddTagOf(tags []string) (string, time.Time, bool) {
	var found string
	var at time.Time
	for _, tag := range tags {
		t, ok := ReaddTagTime(tag)
		if !ok {
			continue
		}
		if found == "" || t.Before(at) {
			found, at = strings.TrimSpace(tag), t
		}
	}
	return found, at, found != ""
}

// WithoutReaddTags returns tags with every re-registration tag removed.
func WithoutReaddTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if _, ok := ReaddTagTime(tag); !ok {
			out = append(out, tag)
		}
	}
	return out
}

// ReaddRecord is what the re-registration needs from the HA resource and the
// pin after the removal has taken them away: the resource's comment, which
// carries the ownership marker, its auto-rebalance flag, which says the VM is
// a DLB VM, and the nodes of the VM's pin and whether it was strict, which is
// a constraint the decision must keep.
type ReaddRecord struct {
	Comment       string   `json:"comment"`
	AutoRebalance bool     `json:"auto_rebalance,omitempty"`
	PinNodes      []string `json:"pin_nodes,omitempty"`
	PinStrict     bool     `json:"pin_strict,omitempty"`
}

// ReaddRecordOf reads the re-registration record from a VM description. It
// returns nil and no error when the description carries none, and an error
// when the record is there but cannot be read or names no comment.
func ReaddRecordOf(desc string) (*ReaddRecord, error) {
	_, raw := pve.ParseSentinel(desc)
	value, ok := raw[ReaddRecordKey]
	if !ok {
		return nil, nil
	}
	var rec ReaddRecord
	if err := json.Unmarshal(value, &rec); err != nil {
		return nil, fmt.Errorf("the HA re-registration record is not readable: %w", err)
	}
	if strings.TrimSpace(rec.Comment) == "" {
		return nil, errors.New("the HA re-registration record names no resource comment")
	}
	return &rec, nil
}

// WithReaddRecord returns desc with rec as its re-registration record, or
// without one when rec is nil. The description's other text and every other
// sentinel key stay as they were.
func WithReaddRecord(desc string, rec *ReaddRecord) (string, error) {
	nonBOSH, raw := pve.ParseSentinel(desc)
	if rec == nil {
		if _, ok := raw[ReaddRecordKey]; !ok {
			return desc, nil
		}
		delete(raw, ReaddRecordKey)
		return pve.RenderSentinel(nonBOSH, raw)
	}
	value, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	raw[ReaddRecordKey] = value
	return pve.RenderSentinel(nonBOSH, raw)
}

// ReaddIntent is what the VM config says about a re-registration: the tag,
// empty when there is none, and the record. RecordErr says why a record that
// is there could not be read.
type ReaddIntent struct {
	Tag       string
	Record    *ReaddRecord
	RecordErr error
}

// Tagged reports whether the VM carries the re-registration tag.
func (i ReaddIntent) Tagged() bool { return i.Tag != "" }

// ReaddOutcome is how a re-registration ended.
type ReaddOutcome string

const (
	// ReaddNothing means there was nothing to clear: the resource is not in
	// error and the VM carries no tag, or the resource is another order's.
	ReaddNothing ReaddOutcome = "nothing"
	// ReaddRegistered means the VM runs under HA again and the tag is gone.
	ReaddRegistered ReaddOutcome = "registered"
	// ReaddWithoutHA means the VM runs without HA, because auto refused its
	// confinement or its watchdog, or its decision gives it no HA, and the tag
	// is gone.
	ReaddWithoutHA ReaddOutcome = "without_ha"
	// ReaddRemoved means the resource was removed and not registered again,
	// because the caller asked to stop after the removal, and the tag is gone.
	ReaddRemoved ReaddOutcome = "removed"
	// ReaddForeign means a resource without this Director's marker holds the
	// VM now, so the tag was removed and nothing else was written.
	ReaddForeign ReaddOutcome = "foreign"
)

// ReregisterResult is the outcome of a re-registration and the node the VM
// is on at its end.
type ReregisterResult struct {
	Outcome ReaddOutcome
	Node    string
}

// Confinement is what a caller's confinement write gives back: the pin it
// wrote, the anti-affinity rule it placed the VM in, or "" for none, and, in
// auto, why a write that failed leaves the VM unconfined.
type Confinement struct {
	Pin          *Rule
	AntiAffinity string
	Unconfined   string
}

// ReregisterInput is one re-registration. The callbacks carry the parts that
// need the VM config and the CPI's rule writers.
type ReregisterInput struct {
	VMID int
	// Node is the node the caller last resolved the VM on.
	Node string
	// Profile is the resolved HA profile.
	Profile *Profile
	// Decided is the state read the caller's decision was made on, nil when
	// the caller made none. Its service ID and uid must match a fresh read
	// before anything is written.
	Decided *pve.HAServiceState
	// StopAfterRemoval ends the order after the removal, for a caller whose
	// decision drops HA for the VM anyway.
	StopAfterRemoval bool
	// PinID names the VM's pin.
	PinID string
	// Budget bounds every wait, and PollInterval is the pause between drain
	// reads.
	Budget       time.Duration
	PollInterval time.Duration
	// Stager runs the staging order.
	Stager *Stager

	// Power reads the VM's power state on node.
	Power func(ctx context.Context, node string) (string, error)
	// ReadIntent reads the tag and the record from the VM config.
	ReadIntent func(ctx context.Context, node string) (ReaddIntent, error)
	// WriteIntent writes tag and rec to the VM config in one write, through
	// pve.MutateVMConfig.
	WriteIntent func(ctx context.Context, node, tag string, rec ReaddRecord) error
	// ClearIntent removes the tag and the record in one write. Registered
	// also adds the bosh-ha-managed tag, and WithoutHA and Removed remove it.
	ClearIntent func(ctx context.Context, node string, outcome ReaddOutcome) error
	// Decide makes the VM's eligibility decision on node from its volumes now,
	// with st as its state read and rec as what the resource and pin held.
	Decide func(ctx context.Context, node string, st *pve.HAServiceState, rec ReaddRecord) (*VMDecision, error)
	// Moving reports whether the VM on node holds a config lock or has a
	// migration, relocation, or disk move task running. The order changes
	// nothing in HA while it does.
	Moving func(ctx context.Context, node string) (bool, error)
	// PendingOnly finishes only an order an earlier call left part way: a VM
	// with no re-registration tag is left as it is, even when its resource is
	// in error.
	PendingOnly bool
	// LeaveRules removes the VM from its own rules and leaves the resource,
	// for a removal that PVE refuses while a rule names the resource.
	LeaveRules func(ctx context.Context) error
	// Between is the change to make while the VM has no HA resource, which
	// runs only in the call that removed it. nil makes none.
	Between func(ctx context.Context, node string) (pve.VMLocation, error)
	// Confine writes the staged VM's anti-affinity membership and its pin,
	// repairing a CPI-owned rule that PVE disabled while the resource was
	// gone.
	Confine func(ctx context.Context, d *VMDecision, rec ReaddRecord, node string) (Confinement, error)
	// WithoutHA removes the VM's pin and the anti-affinity rules its group no
	// longer needs, for a VM that goes on without HA. Its failure is logged.
	WithoutHA func(ctx context.Context, d *VMDecision, rec ReaddRecord, reason string) error
}

func (in *ReregisterInput) validate() error {
	switch {
	case in.VMID <= 0:
		return cpierrors.Cloud("ha readd: VM ID must be positive, got %d", in.VMID)
	case strings.TrimSpace(in.Node) == "":
		return cpierrors.Cloud("ha readd: the VM's node must not be empty")
	case in.Profile == nil:
		return cpierrors.Cloud("ha readd: the HA profile must be set")
	case in.Stager == nil || in.Stager.Resources == nil || in.Stager.Owned == nil || in.Stager.ReadRules == nil:
		return cpierrors.Cloud("ha readd: the stager, its resource client, its ownership check, and its rules " +
			"reader must be set")
	case in.Power == nil || in.ReadIntent == nil || in.WriteIntent == nil || in.ClearIntent == nil ||
		in.Decide == nil || in.Moving == nil || in.LeaveRules == nil || in.Confine == nil || in.WithoutHA == nil:
		return cpierrors.Cloud("ha readd: the power read, the intent reads and writes, the decision, the task " +
			"check, the rule leave, the confinement write, and the without-HA cleanup must be set")
	case strings.TrimSpace(in.PinID) == "":
		return cpierrors.Cloud("ha readd: the pin ID must be set")
	}
	return nil
}

// ReregisterErrored clears the error state of a running VM that BOSH keeps by
// removing its HA resource and registering it again, and it finishes an order
// that an earlier call left part way. It reads the tag on the VM config and
// the resource, and goes on from what it finds:
//
//   - A resource in error goes through the whole order once every
//     precondition holds: the profile may write HA state, the VM's node is
//     online with a fresh LRM, a fresh state read has the service ID and uid
//     of the decision read, the owner node reports the VM running, and the
//     VM's node lies in its hard admissible set. A stopped VM returns an error
//     that wraps ErrReaddVMStopped, so the caller's disabled path applies.
//   - A tag with no resource is a crash after the removal, and the order goes
//     on from the drain with the record the tag write saved.
//   - A tag with this Director's ignored resource is a crash after the stage,
//     and the order goes on from the confinement.
//   - A tag with this Director's active resource is a crash after the started
//     request, and the order checks the landing and removes the tag.
//   - No tag and no error is nothing to do, and with PendingOnly no tag is
//     nothing to do whatever the resource's state.
//
// No step that changes HA runs while the VM holds a config lock or has a
// migration, relocation, or disk move task running (see Moving).
//
// Every transient condition is a retriable error that writes nothing more, and
// the tag stays for the next call. A condition a retry does not clear is a
// retriable error that wraps ErrReaddBlocked. Nothing here ever requests
// disabled.
func ReregisterErrored(ctx context.Context, deps NarrowDeps, in ReregisterInput) (ReregisterResult, error) {
	if err := in.validate(); err != nil {
		return ReregisterResult{}, err
	}
	if deps.ReadState == nil || deps.Settle == nil {
		return ReregisterResult{}, cpierrors.Cloud("ha readd: the state read and the settle wait must be set")
	}
	r := &readd{deps: deps, in: in, sid: pve.HAServiceSID(in.VMID)}
	r.defaults()
	return r.run(ctx)
}

type readd struct {
	deps NarrowDeps
	in   ReregisterInput
	sid  string
}

func (r *readd) defaults() {
	if r.in.Budget <= 0 {
		r.in.Budget = NarrowDefaultBudget
	}
	if r.deps.Now == nil {
		r.deps.Now = time.Now
	}
	if r.deps.Logger == nil {
		r.deps.Logger = log.NewNopLogger()
	}
}

func (r *readd) run(ctx context.Context) (ReregisterResult, error) {
	intent, err := r.in.ReadIntent(ctx, r.in.Node)
	if err != nil {
		return ReregisterResult{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha readd: read the re-registration tag of VM %d", r.in.VMID))
	}
	if r.in.PendingOnly && !intent.Tagged() {
		return ReregisterResult{Outcome: ReaddNothing, Node: r.in.Node}, nil
	}
	res, err := r.in.Stager.Resources.GetHaResources(ctx, r.sid)
	switch {
	case err != nil && isResourceNotFound(err):
		if !intent.Tagged() {
			return ReregisterResult{Outcome: ReaddNothing, Node: r.in.Node}, nil
		}
		return r.resumeRemoved(ctx, intent)
	case err != nil:
		return ReregisterResult{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha readd: read %s", r.sid))
	case res == nil:
		return ReregisterResult{}, cpierrors.Retriable("ha readd: the read of %s returned nothing", r.sid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	if !r.in.Stager.Owned(comment) {
		return r.foreign(ctx, intent)
	}
	st, err := r.read(ctx, r.in.Node)
	if err != nil {
		return ReregisterResult{}, err
	}
	requested := ""
	if res.State != nil {
		requested = *res.State
	}
	switch {
	case st.Raw == haStateError:
		return r.removeAndRegister(ctx, intent, res, st)
	case !intent.Tagged():
		return ReregisterResult{Outcome: ReaddNothing, Node: st.Node}, nil
	case NarrowPending(res.State, res.Comment):
		// The barrier's token says its own order holds the resource, and the
		// next narrowing call finishes it. The tag stays until then.
		r.deps.Logger.Info("ha readd: a narrowing barrier holds the resource; the re-registration waits for it",
			log.String("sid", r.sid))
		return ReregisterResult{Outcome: ReaddNothing, Node: st.Node}, nil
	case requested == haStateIgnored:
		return r.resumeStaged(ctx, intent, res, st)
	default:
		return r.resumeActive(ctx, intent, res, st, requested)
	}
}

// foreign handles a resource that does not carry this Director's marker. With
// the tag, the resource is one an operator added after this order removed
// ours, so the order has nothing left to do and the tag goes. Without it, the
// resource is not the CPI's to remove.
func (r *readd) foreign(ctx context.Context, intent ReaddIntent) (ReregisterResult, error) {
	if !intent.Tagged() {
		return ReregisterResult{}, r.blocked("%s does not carry this Director's ownership marker, so the CPI "+
			"leaves it in place; clear its error without stopping the guest by running 'ha-manager remove %s' and "+
			"then 'ha-manager add %s --state started'", r.sid, r.sid, r.sid)
	}
	if err := r.in.ClearIntent(ctx, r.in.Node, ReaddForeign); err != nil {
		return ReregisterResult{}, r.transient(err, "remove the re-registration tag of VM %d", r.in.VMID)
	}
	r.deps.Logger.Warn("ha readd: a resource without this Director's marker holds the VM now, so the "+
		"re-registration ends and its tag is removed", log.String("sid", r.sid))
	return ReregisterResult{Outcome: ReaddForeign, Node: r.in.Node}, nil
}

// removeAndRegister runs the whole order for a resource in error.
func (r *readd) removeAndRegister(
	ctx context.Context, intent ReaddIntent, res *sdkcluster.GetHaResourcesResponse, st *pve.HAServiceState,
) (ReregisterResult, error) {
	if err := r.writable(st.Node); err != nil {
		return ReregisterResult{}, err
	}
	if err := r.nodeReady(st); err != nil {
		return ReregisterResult{}, err
	}
	if d := r.in.Decided; d != nil && (d.SID != st.SID || d.UID != st.UID) {
		return ReregisterResult{}, cpierrors.Retriable("ha readd: the HA state of %s changed since the decision "+
			"read (uid %s, now %s), so nothing is removed; retry", r.sid, orDash(d.UID), orDash(st.UID))
	}
	power, err := r.in.Power(ctx, st.Node)
	switch {
	case err != nil:
		return ReregisterResult{}, r.transient(err, "read the power state of VM %d on node %s", r.in.VMID, st.Node)
	case power == "stopped":
		return ReregisterResult{}, cpierrors.WrapAs(fmt.Errorf("%w: %s on node %s", ErrReaddVMStopped, r.sid,
			st.Node), cpierrors.TypeRetriableCloud, "ha readd")
	case power != "running":
		return ReregisterResult{}, cpierrors.Retriable("ha readd: VM %d is %s on node %s, so nothing is removed; "+
			"retry once it runs", r.in.VMID, orDash(power), st.Node)
	}
	rec, err := r.snapshot(ctx, res, intent.Record)
	if err != nil {
		return ReregisterResult{}, err
	}
	d, err := r.in.Decide(ctx, st.Node, st, rec)
	if err != nil {
		return ReregisterResult{}, err
	}
	if err := r.judge(d, st.Node, !r.in.StopAfterRemoval); err != nil {
		if errors.Is(err, errReaddNoHA) {
			if r.in.StopAfterRemoval {
				err = nil
			} else {
				return ReregisterResult{}, r.blocked("the HA decision gives VM %d no HA (%s), so the CPI does not "+
					"re-register it; clear the error without stopping the guest by running 'ha-manager remove %s'",
					r.in.VMID, d.Reason, r.sid)
			}
		}
		if err != nil {
			return ReregisterResult{}, err
		}
	}
	if err := r.settled(ctx, st.Node); err != nil {
		return ReregisterResult{}, err
	}
	// The tag already on the VM keeps its age, so a crash loop never resets
	// the sweep's clock.
	tag := intent.Tag
	if tag == "" {
		tag = ReaddTag(r.deps.Now())
	}
	if err := r.in.WriteIntent(ctx, st.Node, tag, rec); err != nil {
		return ReregisterResult{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"ha readd: write the re-registration tag of VM %d; nothing was removed", r.in.VMID))
	}
	again, err := r.read(ctx, st.Node)
	if err != nil {
		return ReregisterResult{}, err
	}
	if again.Raw != haStateError || again.UID != st.UID || again.SID != st.SID {
		return ReregisterResult{}, cpierrors.Retriable("ha readd: the HA state of %s changed before the removal "+
			"(%s, uid %s), so nothing is removed; the next call finishes the re-registration", r.sid,
			orDash(again.Raw), orDash(again.UID))
	}
	if err := r.remove(ctx); err != nil {
		return ReregisterResult{}, err
	}
	r.deps.Logger.Info("ha readd: removed the errored HA resource; the guest keeps running",
		log.String("sid", r.sid), log.String("uid", st.UID), log.String("node", again.Node), log.String("tag", tag))
	if r.in.StopAfterRemoval {
		loc, err := drainRemoved(ctx, r.deps, r.in.VMID, again.Node, r.in.Budget, r.in.PollInterval, true)
		if err != nil {
			return ReregisterResult{}, err
		}
		if err := r.in.ClearIntent(ctx, loc.Node, ReaddRemoved); err != nil {
			return ReregisterResult{}, r.transient(err, "remove the re-registration tag of VM %d", r.in.VMID)
		}
		return ReregisterResult{Outcome: ReaddRemoved, Node: loc.Node}, nil
	}
	return r.register(ctx, rec, again.Node, true)
}

// resumeRemoved finishes an order whose removal landed: the VM carries the tag
// and has no HA resource.
func (r *readd) resumeRemoved(ctx context.Context, intent ReaddIntent) (ReregisterResult, error) {
	if err := r.writable(r.in.Node); err != nil {
		r.deps.Logger.Warn("ha readd: the VM runs without HA, and its re-registration tag stays until a call "+
			"that may write HA state finishes it", log.String("sid", r.sid), log.String("tag", intent.Tag),
			log.Err(err))
		return ReregisterResult{}, err
	}
	if intent.Record == nil {
		why := "the VM config carries no record of it"
		if intent.RecordErr != nil {
			why = intent.RecordErr.Error()
		}
		r.deps.Logger.Warn("ha readd: the VM runs without HA and the record of its HA resource is missing, so the "+
			"CPI cannot register it again", log.String("sid", r.sid), log.String("tag", intent.Tag),
			log.String("reason", why))
		return ReregisterResult{}, r.blocked("VM %d carries the re-registration tag %s but %s; register it by hand "+
			"with 'ha-manager add %s --state started', restore its HA rules, and remove the tag", r.in.VMID,
			intent.Tag, why, r.sid)
	}
	return r.register(ctx, *intent.Record, r.in.Node, false)
}

// register runs the order from the drain on, for a VM whose resource is gone.
// removedHere says this call removed it, which is when Between runs. A call
// that removed the resource checked for a running task before the removal, and
// Between is its own change, so only a call that finishes an earlier one's
// removal checks again here.
func (r *readd) register(ctx context.Context, rec ReaddRecord, node string, removedHere bool) (ReregisterResult, error) {
	loc, err := drainRemoved(ctx, r.deps, r.in.VMID, node, r.in.Budget, r.in.PollInterval, true)
	if err != nil {
		return ReregisterResult{}, err
	}
	node = loc.Node
	if !removedHere {
		if err := r.settled(ctx, node); err != nil {
			return ReregisterResult{}, err
		}
	}
	if removedHere && r.in.Between != nil {
		moved, err := r.in.Between(ctx, node)
		if err != nil {
			// The VM keeps no HA resource and its tag, and the next call or
			// the sweep registers it from the volumes it holds then.
			r.deps.Logger.Warn("ha readd: the change between the removal and the registration failed; the VM "+
				"runs without HA until the next call finishes the re-registration", log.String("sid", r.sid),
				log.Err(err))
			return ReregisterResult{Node: node}, err
		}
		if moved.Node != "" {
			node = moved.Node
		}
	}
	st, err := r.read(ctx, node)
	if err != nil {
		return ReregisterResult{}, err
	}
	d, err := r.in.Decide(ctx, node, st, rec)
	if err != nil {
		return ReregisterResult{}, err
	}
	return r.stageAndActivate(ctx, d, rec, node)
}

// resumeStaged finishes an order that staged the resource: the VM carries the
// tag and this Director's resource requests ignored.
func (r *readd) resumeStaged(
	ctx context.Context, intent ReaddIntent, res *sdkcluster.GetHaResourcesResponse, st *pve.HAServiceState,
) (ReregisterResult, error) {
	if err := r.writable(st.Node); err != nil {
		r.deps.Logger.Warn("ha readd: the staged VM runs without HA, and its re-registration tag stays until a "+
			"call that may write HA state finishes it", log.String("sid", r.sid), log.Err(err))
		return ReregisterResult{}, err
	}
	if err := r.settled(ctx, st.Node); err != nil {
		return ReregisterResult{}, err
	}
	rec, err := r.snapshot(ctx, res, intent.Record)
	if err != nil {
		return ReregisterResult{}, err
	}
	d, err := r.in.Decide(ctx, st.Node, st, rec)
	if err != nil {
		return ReregisterResult{}, err
	}
	return r.stageAndActivate(ctx, d, rec, st.Node)
}

// resumeActive finishes an order whose started request landed: the VM carries
// the tag and this Director's resource requests another state than ignored.
func (r *readd) resumeActive(
	ctx context.Context, intent ReaddIntent, res *sdkcluster.GetHaResourcesResponse, st *pve.HAServiceState,
	requested string,
) (ReregisterResult, error) {
	node := st.Node
	if requested == haStateStarted {
		if err := r.writable(node); err != nil {
			return ReregisterResult{}, err
		}
		if err := r.settled(ctx, node); err != nil {
			return ReregisterResult{}, err
		}
		rec, err := r.snapshot(ctx, res, intent.Record)
		if err != nil {
			return ReregisterResult{}, err
		}
		d, err := r.in.Decide(ctx, node, st, rec)
		if err != nil {
			return ReregisterResult{}, err
		}
		if err := r.judge(d, node, false); err != nil {
			if errors.Is(err, errReaddNoHA) {
				return ReregisterResult{}, r.blocked("VM %d runs under HA again, but its HA decision now gives it "+
					"no HA (%s); its re-registration tag stays for the operator", r.in.VMID, d.Reason)
			}
			return ReregisterResult{}, err
		}
		started, err := r.in.Stager.SetStarted(ctx, r.in.VMID, node)
		if err != nil {
			return ReregisterResult{}, err
		}
		switch {
		case started == nil:
			return ReregisterResult{}, cpierrors.Retriable("ha readd: the state wait for %s returned no result", r.sid)
		case started.Outcome == pve.HAStateNeedsReregistration:
			return ReregisterResult{}, started.ReregistrationError()
		case started.Outcome != pve.HAStateReached:
			return ReregisterResult{}, cpierrors.Retriable("ha readd: %s did not reach started (%s)", r.sid,
				started.Outcome)
		}
		node = strings.TrimSpace(started.Node)
		if !slices.Contains(d.Admissible, node) {
			return ReregisterResult{Node: node}, cpierrors.Retriable("ha readd: PVE HA runs %s on node %s, which lies "+
				"outside its hard admissible set %s; the re-registration tag stays until it runs inside it",
				r.sid, orDash(node), strings.Join(d.Admissible, ","))
		}
	}
	if err := r.in.ClearIntent(ctx, node, ReaddRegistered); err != nil {
		return ReregisterResult{}, r.transient(err, "remove the re-registration tag of VM %d", r.in.VMID)
	}
	r.deps.Logger.Info("ha readd: the VM runs under HA again; removed its re-registration tag",
		log.String("sid", r.sid), log.String("node", node), log.String("requested", requested))
	return ReregisterResult{Outcome: ReaddRegistered, Node: node}, nil
}

// stageAndActivate runs steps 3 to 6 of the order with the decision d.
func (r *readd) stageAndActivate(
	ctx context.Context, d *VMDecision, rec ReaddRecord, node string,
) (ReregisterResult, error) {
	if err := r.judge(d, node, true); err != nil {
		if errors.Is(err, errReaddNoHA) {
			return r.withoutHA(ctx, d, rec, node, d.Reason)
		}
		return ReregisterResult{}, err
	}
	stager := *r.in.Stager
	if err := stager.Stage(ctx, d, rec.Comment); err != nil {
		return ReregisterResult{}, err
	}
	conf, err := r.in.Confine(ctx, d, rec, node)
	if err != nil {
		// The resource stays ignored, which the CRM never acts on, and the
		// next call finishes the confinement.
		return ReregisterResult{}, err
	}
	reason := conf.Unconfined
	if reason == "" {
		if conf.Pin == nil {
			return ReregisterResult{}, cpierrors.Cloud("ha readd: the confinement write for %s returned no pin", r.sid)
		}
		// A rules read that fails proves nothing about the rules, so it
		// defers rather than take the VM out of HA.
		readFailed := false
		read := stager.ReadRules
		stager.ReadRules = func(ctx context.Context) (*RuleSet, error) {
			set, err := read(ctx)
			readFailed = err != nil || set == nil
			return set, err
		}
		err := stager.Verify(ctx, d, conf.Pin, conf.AntiAffinity)
		switch {
		case err == nil:
		case readFailed:
			return ReregisterResult{}, err
		case errors.Is(err, ErrConfinementUnverified) && !stager.Managed:
			reason = err.Error()
		default:
			return ReregisterResult{}, err
		}
	}
	if reason == "" {
		switch err := stager.CheckWatchdog(ctx, d, node); {
		case err == nil:
		case errors.Is(err, ErrWatchdogNotArmed):
			reason = err.Error()
		default:
			return ReregisterResult{}, err
		}
	}
	if reason != "" {
		return r.withoutHA(ctx, d, rec, node, reason)
	}
	landing, err := stager.Activate(ctx, d, node)
	if err != nil {
		return ReregisterResult{Node: landing}, err
	}
	if err := r.in.ClearIntent(ctx, landing, ReaddRegistered); err != nil {
		return ReregisterResult{Node: landing}, r.transient(err, "remove the re-registration tag of VM %d",
			r.in.VMID)
	}
	r.deps.Logger.Info("ha readd: the VM runs under HA again", log.String("sid", r.sid),
		log.String("node", landing))
	return ReregisterResult{Outcome: ReaddRegistered, Node: landing}, nil
}

// withoutHA lets the VM go on without HA: it removes a resource this call
// staged, removes the VM's pin and the rules its group no longer needs, and
// removes the tag and the record.
func (r *readd) withoutHA(
	ctx context.Context, d *VMDecision, rec ReaddRecord, node, reason string,
) (ReregisterResult, error) {
	if d.StagedIgnored {
		stager := *r.in.Stager
		if err := stager.Unstage(ctx, d, reason); err != nil {
			return ReregisterResult{}, err
		}
	}
	if err := r.in.WithoutHA(ctx, d, rec, reason); err != nil {
		r.deps.Logger.Warn("ha readd: the rules of the VM that runs without HA could not all be removed; they name "+
			"no resource of this VM now", log.String("sid", r.sid), log.Err(err))
	}
	if err := r.in.ClearIntent(ctx, node, ReaddWithoutHA); err != nil {
		return ReregisterResult{}, r.transient(err, "remove the re-registration tag of VM %d", r.in.VMID)
	}
	r.deps.Logger.Warn("ha readd: the VM runs without HA", log.String("sid", r.sid), log.String("node", node),
		log.String("reason", reason))
	return ReregisterResult{Outcome: ReaddWithoutHA, Node: node}, nil
}

// errReaddNoHA is judge's answer for a decision that gives the VM no HA.
var errReaddNoHA = errors.New("the decision gives the VM no HA")

// judge checks a decision. One that refused HA for a reason that may pass
// returns a retriable error in every mode, managed included, where the refusal
// is also a failure, so the transient answer comes before the failure's own.
// Fewer placeable nodes than the VM's shared volumes reach is retriable too.
// Any other failing decision returns its error, one that gives no HA returns
// errReaddNoHA, and with needNode a node outside the hard admissible set
// returns a blocked error that names it, because registering the VM there
// would ask the CRM to move it.
func (r *readd) judge(d *VMDecision, node string, needNode bool) error {
	switch {
	case d == nil:
		return cpierrors.Cloud("ha readd: the decision for VM %d is missing", r.in.VMID)
	case d.Transient:
		msg := fmt.Sprintf("ha readd: the HA decision for VM %d refused HA for a reason that may pass; the "+
			"re-registration waits", r.in.VMID)
		if failure := d.Failure(); failure != nil {
			return cpierrors.WrapAs(failure, cpierrors.TypeRetriableCloud, msg)
		}
		return cpierrors.Retriable("%s (%s)", msg, d.Reason)
	case d.PlaceableShortfall:
		return cpierrors.Retriable("ha readd: nodes %s reach every volume of VM %d, but fewer than two of them can "+
			"take it now; the re-registration waits until enough of them are healthy", strings.Join(d.Access, ","),
			r.in.VMID)
	case d.Outcome == OutcomeFail:
		if failure := d.Failure(); failure != nil {
			return failure
		}
		return cpierrors.Cloud("ha readd: VM %d: the HA decision failed the call: %s", r.in.VMID, d.Reason)
	case !d.Registers():
		return errReaddNoHA
	case needNode && !slices.Contains(d.Admissible, node):
		return r.blocked("VM %d runs on node %s, which lies outside its hard admissible set %s, so registering "+
			"it again would make PVE HA move it; move the VM to one of those nodes, or clear the error by hand",
			r.in.VMID, orDash(node), strings.Join(d.Admissible, ","))
	}
	return nil
}

// writable returns the blocked error of a profile that may not write HA state,
// which names Sys.Console when the token lacks it and the commands that clear
// the error without stopping the guest.
func (r *readd) writable(node string) error {
	p := r.in.Profile
	if p.WritesAllowed() && p.SysConsole {
		return nil
	}
	why := p.Reason
	switch {
	case !p.SysConsole:
		why = "the token lacks the Sys.Console privilege on / that the HA state requests need"
	case why == "":
		why = strings.Join(p.Gaps, "; ")
	}
	if why == "" {
		why = "pve.ha.mode does not let the CPI write HA state"
	}
	return r.blocked("%s is in the HA error state or still unregistered while its VM runs on node %s, and the CPI "+
		"cannot register it again: %s; clear the error without stopping the guest by running 'ha-manager remove "+
		"%s' and then 'ha-manager add %s --state started', restore the resource's HA rules, and retry",
		r.sid, orDash(node), why, r.sid, r.sid)
}

// settled returns a retriable error while the VM on node holds a config lock
// or has a migration, relocation, or disk move task running, and when that
// cannot be told, so the order changes nothing in HA until the task is done.
func (r *readd) settled(ctx context.Context, node string) error {
	moving, err := r.in.Moving(ctx, node)
	switch {
	case err != nil:
		return r.transient(err, "could not tell whether a migration, relocation, or disk move task for VM %d is "+
			"running on node %s, so nothing more changes in HA; retry", r.in.VMID, orDash(node))
	case moving:
		r.deps.Logger.Info("ha readd: the VM holds a config lock or a task moves it; the re-registration waits",
			log.String("sid", r.sid), log.String("node", node))
		return cpierrors.Retriable("ha readd: VM %d on node %s holds a config lock or has a migration, relocation, "+
			"or disk move task running, so nothing more changes in HA; retry once the task is done", r.in.VMID,
			orDash(node))
	}
	return nil
}

// nodeReady checks that the VM's node is online with a fresh LRM, which the
// removal needs, because the CRM must be able to act on the resource the
// order registers there. A known state already proves the entry node quorate.
func (r *readd) nodeReady(st *pve.HAServiceState) error {
	h := st.NodeHealth
	switch {
	case h.DeadOrFenced || h.Health != pve.NodeHealthOnline:
		return cpierrors.Retriable("ha readd: node %s of %s is %s (%s), so nothing is removed; retry once it is "+
			"online", st.Node, r.sid, h.Health, orDash(h.Reason))
	case !st.CRMPresent || !st.LRMFresh:
		return cpierrors.Retriable("ha readd: the LRM of node %s has no fresh timestamp, so nothing is removed; "+
			"retry once it reports", st.Node)
	}
	return nil
}

// snapshot builds the record of the resource and the VM's pin. The live pin
// counts when it is this Director's and enabled, and otherwise the record an
// earlier call saved keeps its pin, because PVE may have pruned the pin
// together with the resource.
func (r *readd) snapshot(
	ctx context.Context, res *sdkcluster.GetHaResourcesResponse, saved *ReaddRecord,
) (ReaddRecord, error) {
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	rec := ReaddRecord{
		Comment:       strings.TrimSpace(*stripNarrowMark(&comment)),
		AutoRebalance: pveBoolIs(res.AutoRebalance, true),
	}
	set, err := r.in.Stager.ReadRules(ctx)
	if err != nil {
		return ReaddRecord{}, r.transient(err, "read the HA rules of VM %d", r.in.VMID)
	}
	if set == nil {
		return ReaddRecord{}, cpierrors.Retriable("ha readd: the read of the HA rules of VM %d returned nothing",
			r.in.VMID)
	}
	if pin := set.Find(r.in.PinID); pin != nil && !pin.Disabled && r.in.Stager.Owned(pin.Comment) {
		rec.PinNodes, rec.PinStrict = pin.NodeNames(), pin.Strict
	} else if saved != nil {
		rec.PinNodes, rec.PinStrict = slices.Clone(saved.PinNodes), saved.PinStrict
	}
	return rec, nil
}

// remove removes the errored resource with purge. A refusal PVE answers may
// come from a rule that still names the resource, so the VM leaves its own
// rules first and the removal is sent once more. A failure with no answer is
// settled with a read, and only a read that shows the resource gone counts.
func (r *readd) remove(ctx context.Context) error {
	purge := true
	del := func() error {
		return r.in.Stager.Resources.DeleteHaResources(ctx, r.sid, &sdkcluster.DeleteHaResourcesParams{Purge: &purge})
	}
	err := del()
	if err == nil || isResourceNotFound(err) {
		return nil
	}
	if _, answered := pve.AnsweredStatus(err); answered {
		r.deps.Logger.Warn("ha readd: PVE refused the removal of the errored resource; the VM leaves its own rules "+
			"and the removal is sent again", log.String("sid", r.sid), log.Err(err))
		if leaveErr := r.in.LeaveRules(ctx); leaveErr != nil {
			return cpierrors.WrapAs(fmt.Errorf("%w; leaving the VM's rules failed: %w", err, leaveErr),
				cpierrors.TypeRetriableCloud, fmt.Sprintf("ha readd: remove %s", r.sid))
		}
		if err = del(); err == nil || isResourceNotFound(err) {
			return nil
		}
	}
	if _, readErr := r.in.Stager.Resources.GetHaResources(ctx, r.sid); readErr != nil && isResourceNotFound(readErr) {
		return nil
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf("ha readd: remove %s; the "+
		"re-registration tag stays and the next call retries the removal", r.sid))
}

// read reads the VM's state and refuses an unknown one, which covers a
// non-quorate entry node.
func (r *readd) read(ctx context.Context, node string) (*pve.HAServiceState, error) {
	st, err := r.deps.ReadState(ctx, r.in.VMID, node)
	if err != nil {
		return nil, r.transient(err, "read the HA state of VM %d", r.in.VMID)
	}
	if st == nil || !st.Known {
		reason := "no state"
		if st != nil {
			reason = st.Reason
		}
		return nil, cpierrors.Retriable("ha readd: the HA state of VM %d is unknown (%s), so nothing more is "+
			"written; retry", r.in.VMID, reason)
	}
	return st, nil
}

func (r *readd) transient(err error, format string, args ...any) error {
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "ha readd: "+fmt.Sprintf(format, args...))
}

func (r *readd) blocked(format string, args ...any) error {
	return cpierrors.WrapAs(fmt.Errorf("%w: %s", ErrReaddBlocked, fmt.Sprintf(format, args...)),
		cpierrors.TypeRetriableCloud, "ha readd")
}
