package ha

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// narrowUseRemoval selects the barrier's fallback, which removes the HA
// resource around the mutation and registers it again, in place of the
// ignored request. It is an internal switch, off by default, for a PVE release
// whose CRM keeps acting on an ignored resource. Even when it is on, the
// fallback removes nothing unless the caller supplies PersistSnapshot, which no
// caller does yet, because a crash after the removal would otherwise lose the
// VM's rule memberships.
var narrowUseRemoval = false

// narrowMarkKey opens the token the barrier appends to the HA resource's
// comment while it holds the resource in ignored. The token names the state to
// put back, so a later call can tell the barrier's own ignored from an
// operator's and can restore what the resource had before.
const narrowMarkKey = "bosh-narrow-from="

// Defaults for the barrier's waits.
const (
	NarrowDefaultBudget = 5 * time.Minute
	narrowDefaultPoll   = 2 * time.Second
)

// narrowUnwindTimeout is the least time each step of the barrier's unwind
// gets, which is the revert of the pin and the request that puts back the
// resource's state. The unwind runs after the mutation, and the mutation can
// spend nearly all of the request's time, as an attach does when it waits for
// a parker's lock until just before the request's deadline. An unwind on what
// is left of that time would leave the VM ignored, where PVE HA won't recover
// it, until a later call finished the barrier.
const narrowUnwindTimeout = 30 * time.Second

// unwindContext returns the context one step of the barrier's unwind runs on,
// plus the cancel the caller must defer. Under a request deadline the step
// keeps the request's values, is free of its cancellation, and gets the
// longer of narrowUnwindTimeout and the time the request still has, so it
// never gets less time than it had before. A request with no deadline that is
// still live keeps its own context, because the step has always had unbounded
// time there. One that a stop signal has already ended gets
// narrowUnwindTimeout, so the state still goes back.
func unwindContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, bounded := ctx.Deadline()
	if !bounded && ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	budget := narrowUnwindTimeout
	if bounded {
		budget = max(budget, time.Until(deadline))
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// ResourceAPI is the part of the cluster API the barrier uses on the VM's HA
// resource. The SDK's cluster service satisfies it.
type ResourceAPI interface {
	GetHaResources(ctx context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error)
	CreateHaResources(ctx context.Context, params *sdkcluster.CreateHaResourcesParams) error
	UpdateHaResources(ctx context.Context, sid string, params *sdkcluster.UpdateHaResourcesParams) error
	DeleteHaResources(ctx context.Context, sid string, params *sdkcluster.DeleteHaResourcesParams) error
}

// NarrowDeps are the reads and writes the barrier makes. NarrowDepsFor wires
// them to a client.
type NarrowDeps struct {
	Resources ResourceAPI
	// ReadState reads the VM's normalized HA state, with node as the scan's
	// node.
	ReadState func(ctx context.Context, vmid int, node string) (*pve.HAServiceState, error)
	// Settle waits until the VM has no lock and no HA motion, and returns
	// where it is.
	Settle func(ctx context.Context, vmid int) (pve.VMLocation, error)
	// Mutate runs a config mutation, pve.MutateVMConfig in production.
	Mutate func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error)
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Logger *log.Logger
}

// NarrowDepsFor wires the barrier to a client.
func NarrowDepsFor(c pve.Client, logger *log.Logger, motionWait time.Duration) NarrowDeps {
	return NarrowDeps{
		Resources: c.Cluster(),
		ReadState: func(ctx context.Context, vmid int, node string) (*pve.HAServiceState, error) {
			return pve.ReadHAServiceState(ctx, c.Cluster(), c.Nodes(), pve.HAServiceStateInput{
				VMID: vmid, VMNode: node, Kind: pve.HADecisionLive,
			})
		},
		Settle: func(ctx context.Context, vmid int) (pve.VMLocation, error) {
			return pve.AwaitVMSettled(ctx, c, logger, pve.VMSettleRequest{
				VMID: vmid, MotionWait: motionWait, Label: "ha narrow",
			})
		},
		Mutate: func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
			return pve.MutateVMConfig(ctx, c, logger, m)
		},
		Logger: logger,
	}
}

// NarrowInput is one narrowing change to an active VM.
type NarrowInput struct {
	VMID int
	// Node is the node the caller last resolved the VM on.
	Node string
	// OldPin is the VM's pin as read now, nil when it has none, and NewPin the
	// pin the change needs, nil when it needs none.
	OldPin *Rule
	NewPin *Rule
	// WritePin writes a pin through the rules writer and verifies it. A nil
	// pin comes only from putting OldPin back on a VM that had no pin, and it
	// removes the pin the change wrote and nothing else: the VM's HA resource
	// stays, because the barrier restores its state next.
	WritePin func(ctx context.Context, pin *Rule) error
	// Mutation is the config change. Its Node is set from the drain.
	Mutation pve.VMConfigMutation
	// CheckReach re-reads the config after the mutation and checks that every
	// node of NewPin reaches the VM's volumes. nil checks nothing.
	CheckReach func(ctx context.Context, node string) error
	// Unchanged answers, after a mutation that returned an error, whether the
	// VM's config on node provably holds none of the change. When it does, the
	// barrier writes OldPin back before it restores the state, so a change that
	// never landed leaves the VM no narrower than before. nil, false, or an
	// error keeps the narrower pin, which over-confines the VM but never lets
	// PVE HA move it to a node the change does not reach.
	Unchanged func(ctx context.Context, node string) (bool, error)
	// Moving answers, after a mutation that returned an error and before
	// Unchanged, whether a migration, relocation, or disk move task for the
	// VM is still running on node. Such a task can still land the change
	// after the old pin is back, so true keeps the narrower pin and makes the
	// barrier's error retriable. An error, or a nil Moving, also keeps the
	// narrower pin, so a caller that cannot tell never gets a wider pin.
	Moving func(ctx context.Context, node string) (bool, error)
	// SysConsole says the token holds Sys.Console, which the ignored and
	// started requests need.
	SysConsole bool
	// Budget bounds every wait of the barrier, and PollInterval is the pause
	// between drain reads.
	Budget       time.Duration
	PollInterval time.Duration
	// Snapshot and Restore serve the removal fallback. Snapshot returns the
	// CPI-owned rules that name the VM, and Restore writes NewPin and puts the
	// VM back into those rules. PersistSnapshot stores the snapshot where a
	// later call finds it after a crash, before anything is removed. Without
	// it the fallback refuses to run.
	Snapshot        func(ctx context.Context) ([]Rule, error)
	Restore         func(ctx context.Context, rules []Rule, pin *Rule) error
	PersistSnapshot func(ctx context.Context, rules []Rule) error
	// Reregister clears a resource the barrier finds in the error state. A
	// running guest that BOSH keeps must never get disabled, so the barrier
	// hands such a resource to the re-registration order of
	// ReregisterErrored, with st as the decision read and between as the
	// change to make while the VM has no HA resource. between runs the
	// mutation and the landing check of the barrier. nil keeps the retriable
	// error that tells the operator to clear the error.
	Reregister func(ctx context.Context, st *pve.HAServiceState,
		between func(ctx context.Context, node string) (pve.VMLocation, error)) (pve.VMLocation, error)
}

// NarrowActive applies a change that narrows where an HA-managed VM may run,
// without letting the CRM move it in between. The order is:
//
//  1. Request ignored, so the CRM stops acting on the VM.
//  2. Wait for the drain: the CRM's service_status no longer lists the
//     service, in a CRM round newer than the request on the entry node's
//     clock, the VM's node has an LRM report newer than that round, the VM
//     holds no lock, and the scan names the same node. When the VM moved
//     outside the narrower set, put back the old state under the old pin and
//     return a retriable error.
//  3. Write the narrower pin and verify it.
//  4. Run the mutation.
//  5. Re-read the config and check volume reach and the VM's node.
//  6. Request the state the resource had before the barrier. There is no
//     watchdog check here, because the VM stays where it is.
//
// The ignored request also appends a token to the resource's comment that
// names the state to put back, and the last request removes it. A crash at any
// step leaves an ignored resource that carries the token, and the next call
// finishes the order and restores that state. A resource an operator set to
// ignored carries no token: the barrier leaves its state alone and it stays
// ignored. A widening change skips the barrier: it runs the mutation and then
// writes the pin. A resource in error returns a retriable error, and the
// barrier never requests disabled.
func NarrowActive(ctx context.Context, deps NarrowDeps, in NarrowInput) (pve.VMLocation, error) {
	b := &barrier{deps: deps, in: in, sid: pve.HAServiceSID(in.VMID)}
	b.defaults()
	if in.VMID <= 0 || in.Mutation.Apply == nil {
		return pve.VMLocation{}, cpierrors.Cloud("ha narrow: VM ID %d and a mutation are required", in.VMID)
	}
	narrowing := Narrows(in.OldPin, in.NewPin)
	if !in.SysConsole {
		if !narrowing {
			return b.widen(ctx)
		}
		return pve.VMLocation{}, cpierrors.Retriable("ha narrow: VM %d needs its HA resource set to ignored for a "+
			"narrowing change, and the token lacks the Sys.Console privilege that request needs", in.VMID)
	}
	res, err := deps.Resources.GetHaResources(ctx, b.sid)
	switch {
	case err != nil && (pve.IsNotFound(err) || isRuleNotFound(err)):
		if !narrowing {
			return b.widen(ctx)
		}
		// Not HA-managed: nothing can move the VM, so no barrier is needed,
		// and PVE takes no pin for a resource it does not hold.
		return b.mutateAndCheck(ctx, in.Node)
	case err != nil:
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: read the HA resource of VM %d", in.VMID))
	}
	b.comment = stripNarrowMark(res.Comment)
	current := haStateStarted
	if res.State != nil && *res.State != "" {
		current = *res.State
	}
	// An ignored resource that carries the barrier's token is what a crash
	// inside an earlier barrier leaves, so the barrier finishes it whether or
	// not this change narrows, and puts back the state the token names. A token
	// on a resource that is no longer ignored is stale, and the state it has
	// now is the one to keep.
	from, marked := narrowMarkOf(res.Comment)
	resumed := marked && current == haStateIgnored
	b.restore, b.marked = current, resumed
	if resumed {
		b.restore = from
	}
	if !narrowing && !resumed {
		return b.widen(ctx)
	}
	// An operator's ignored resource is out of the CRM's hands already, so it
	// never needs the removal.
	if narrowUseRemoval && !resumed && b.restore != haStateIgnored {
		return b.viaRemoval(ctx)
	}
	return b.viaIgnored(ctx, resumed)
}

type barrier struct {
	deps NarrowDeps
	in   NarrowInput
	sid  string
	// comment is the resource's comment without the barrier's token.
	comment *string
	// restore is the requested state to put back at the end, and marked says
	// the resource's comment carries the barrier's token.
	restore string
	marked  bool
	// tolerateError makes the drain wait out a service the CRM still lists in
	// the error state, which is what a removed errored resource leaves until
	// the CRM's next round drops it.
	tolerateError bool
}

func (b *barrier) defaults() {
	if b.in.Budget <= 0 {
		b.in.Budget = NarrowDefaultBudget
	}
	if b.in.PollInterval <= 0 {
		b.in.PollInterval = narrowDefaultPoll
	}
	if b.deps.Now == nil {
		b.deps.Now = time.Now
	}
	if b.deps.Logger == nil {
		b.deps.Logger = log.NewNopLogger()
	}
	if b.deps.Sleep == nil {
		b.deps.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
}

// widen runs a change that does not narrow the VM's nodes: the mutation first,
// so the wider pin never admits a node the old config cannot use, then the pin.
func (b *barrier) widen(ctx context.Context) (pve.VMLocation, error) {
	loc, err := b.mutate(ctx, b.in.Node)
	if err != nil {
		return loc, err
	}
	if b.in.WritePin != nil && !SameRule(b.in.NewPin, b.in.OldPin) {
		if err := b.in.WritePin(ctx, b.in.NewPin); err != nil {
			return loc, err
		}
	}
	return loc, nil
}

func (b *barrier) mutate(ctx context.Context, node string) (pve.VMLocation, error) {
	m := b.in.Mutation
	m.VMID = b.in.VMID
	if node != "" {
		m.Node = node
	}
	return b.deps.Mutate(ctx, m)
}

// mutateAndCheck runs the mutation and the reach check for a VM that HA does
// not manage, and writes nothing to HA.
func (b *barrier) mutateAndCheck(ctx context.Context, node string) (pve.VMLocation, error) {
	loc, err := b.mutate(ctx, node)
	if err != nil {
		return loc, err
	}
	if b.in.CheckReach != nil {
		if err := b.in.CheckReach(ctx, loc.Node); err != nil {
			return loc, err
		}
	}
	return loc, nil
}

// viaIgnored runs the barrier with the ignored request. resumed says the
// resource was already ignored, which is what a crash in an earlier call
// leaves.
func (b *barrier) viaIgnored(ctx context.Context, resumed bool) (pve.VMLocation, error) {
	st, err := b.readState(ctx, b.in.Node)
	if err != nil {
		return pve.VMLocation{}, err
	}
	if st.Raw == haStateError {
		return b.errored(ctx, st)
	}
	if !resumed && b.restore != haStateIgnored {
		if err := b.request(ctx, haStateIgnored, true); err != nil {
			return pve.VMLocation{}, err
		}
		b.deps.Logger.Info("ha narrow: requested ignored before a narrowing change",
			log.Int("vmid", b.in.VMID), log.String("sid", b.sid), log.String("restore", b.restore))
	}
	// The request time is the entry node's clock read after the request
	// landed, so any CRM round newer than it came after the request.
	after, err := b.readState(ctx, st.Node)
	if err != nil {
		return pve.VMLocation{}, b.restoreAfter(ctx, err)
	}
	loc, err := b.drain(ctx, after.EntryTime, after.Node)
	if err != nil {
		return loc, err
	}
	if b.in.WritePin != nil {
		if err := b.in.WritePin(ctx, b.in.NewPin); err != nil {
			return loc, b.restoreAfter(ctx, fmt.Errorf("ha narrow: write the narrower pin of VM %d: %w", b.in.VMID, err))
		}
	}
	drained := loc.Node
	loc, err = b.mutate(ctx, drained)
	if err != nil {
		// The VM sits inside the narrower set, which the old config allows
		// too, so starting it again is safe whether the mutation landed or not.
		// A mutation proven not to have landed gets the old pin back first. A
		// task that is still running may land it yet, so it keeps the
		// narrower pin, and the call defers until the task is done.
		if running := b.revertPin(ctx, drained); running != nil {
			err = cpierrors.WrapAs(fmt.Errorf("%w; %w", err, running), cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"ha narrow: VM %d keeps its narrower pin until the task is done", b.in.VMID))
		}
		return loc, b.restoreAfter(ctx, err)
	}
	if err := b.checkLanding(ctx, loc.Node); err != nil {
		// The resource stays ignored, because HA could otherwise recover the
		// VM onto a node that cannot reach its volumes. The next call finishes.
		return loc, err
	}
	if err := b.finish(ctx); err != nil {
		return loc, err
	}
	return loc, nil
}

// errored hands a resource in the error state to the caller's re-registration,
// with the barrier's mutation and landing check as the change to make while
// the VM has no HA resource. Without a re-registration it returns the
// retriable error that tells the operator to clear the error. The barrier
// never requests disabled.
func (b *barrier) errored(ctx context.Context, st *pve.HAServiceState) (pve.VMLocation, error) {
	if b.in.Reregister == nil {
		return pve.VMLocation{}, cpierrors.Retriable("ha narrow: the HA resource of VM %d is in the error state; "+
			"the operator must clear it, and the CPI never requests disabled", b.in.VMID)
	}
	b.deps.Logger.Info("ha narrow: the HA resource is in the error state, so the change runs inside its "+
		"re-registration", log.Int("vmid", b.in.VMID), log.String("sid", b.sid), log.String("uid", st.UID))
	return b.in.Reregister(ctx, st, func(ctx context.Context, node string) (pve.VMLocation, error) {
		loc, err := b.mutate(ctx, node)
		if err != nil {
			return loc, err
		}
		return loc, b.checkLanding(ctx, loc.Node)
	})
}

// revertPin writes OldPin back after a mutation that failed, when Moving says
// no task for the VM is still running and Unchanged then proves the config
// holds none of the change. Moving is asked first, because a task that has
// ended cannot land the change after Unchanged reads, while one that ends
// between the two reads could. Any other answer keeps the narrower pin. A
// failed write is logged, because the narrower pin it leaves only
// over-confines the VM, and the restore that follows must still run. It
// returns an error that names the running task when one is still running,
// and nil otherwise. It runs on a context of its own (unwindContext), because
// the mutation that failed may have spent the request's time.
func (b *barrier) revertPin(ctx context.Context, node string) error {
	if b.in.Unchanged == nil || b.in.WritePin == nil || node == "" {
		return nil
	}
	ctx, cancel := unwindContext(ctx)
	defer cancel()
	if b.in.Moving == nil {
		b.deps.Logger.Warn("ha narrow: whether a task for the VM is still running cannot be told; the narrower "+
			"pin stays", log.Int("vmid", b.in.VMID), log.String("sid", b.sid))
		return nil
	}
	moving, err := b.in.Moving(ctx, node)
	switch {
	case err != nil:
		b.deps.Logger.Warn("ha narrow: could not tell whether a task for the VM is still running; the narrower "+
			"pin stays", log.Int("vmid", b.in.VMID), log.String("sid", b.sid), log.Err(err))
		return nil
	case moving:
		b.deps.Logger.Warn("ha narrow: a migration, relocation, or disk move task for the VM is still running; "+
			"the narrower pin stays", log.Int("vmid", b.in.VMID), log.String("sid", b.sid))
		return fmt.Errorf("a migration, relocation, or disk move task for VM %d is still running on node %s",
			b.in.VMID, node)
	}
	unchanged, err := b.in.Unchanged(ctx, node)
	switch {
	case err != nil:
		b.deps.Logger.Warn("ha narrow: could not tell whether the failed change landed; the narrower pin stays",
			log.Int("vmid", b.in.VMID), log.String("sid", b.sid), log.Err(err))
		return nil
	case !unchanged:
		return nil
	}
	if err := b.in.WritePin(ctx, b.in.OldPin); err != nil {
		b.deps.Logger.Warn("ha narrow: the change did not land, and the old pin could not be written back; the "+
			"narrower pin stays", log.Int("vmid", b.in.VMID), log.String("sid", b.sid), log.Err(err))
		return nil
	}
	b.deps.Logger.Info("ha narrow: the change did not land, so the old pin is back",
		log.Int("vmid", b.in.VMID), log.String("sid", b.sid))
	return nil
}

// checkLanding re-reads the config, checks volume reach from the narrower
// set, and checks the VM is on one of its nodes.
func (b *barrier) checkLanding(ctx context.Context, node string) error {
	if b.in.CheckReach != nil {
		if err := b.in.CheckReach(ctx, node); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ha narrow: VM %d stays ignored because its volumes are not reachable after the change",
					b.in.VMID))
		}
	}
	if !b.inNarrowSet(node) {
		return cpierrors.Retriable("ha narrow: VM %d is on node %s, outside the narrower set, after the change; "+
			"it stays ignored", b.in.VMID, node)
	}
	return nil
}

func (b *barrier) inNarrowSet(node string) bool {
	set, all := allowedNodes(b.in.NewPin)
	return all || set[node]
}

// drain waits until the CRM has dropped the service in a round after
// requestTime, the node's LRM has reported after that round, and the VM is
// settled on one node. It returns where the VM is.
func (b *barrier) drain(ctx context.Context, requestTime int64, node string) (pve.VMLocation, error) {
	deadline := b.deps.Now().Add(b.in.Budget)
	var dropRound int64
	var why string
	for {
		st, err := b.deps.ReadState(ctx, b.in.VMID, node)
		switch {
		case err != nil:
			why = err.Error()
		case !st.Known:
			why = st.Reason
		case st.Raw == haStateError && !b.tolerateError:
			return pve.VMLocation{}, cpierrors.Retriable("ha narrow: the HA resource of VM %d went into the error "+
				"state during the drain; the CPI never requests disabled", b.in.VMID)
		case st.InCRM:
			why = fmt.Sprintf("the CRM still lists %s as %s", b.sid, st.Raw)
			dropRound = 0
		case st.CRMStamp <= requestTime:
			why = "no CRM round has run since the ignored request"
		default:
			if dropRound == 0 {
				dropRound = st.CRMStamp
			}
			if st.LRMStamp <= dropRound {
				why = fmt.Sprintf("the LRM of node %s has not reported since the CRM dropped the service", st.Node)
				break
			}
			loc, err := b.deps.Settle(ctx, b.in.VMID)
			if err != nil {
				return loc, b.restoreAfter(ctx, err)
			}
			if loc.Node != st.Node {
				// The VM moved before the CRM let go: read its new node's LRM.
				why = fmt.Sprintf("the VM is on %s but the state read named %s", loc.Node, st.Node)
				node, dropRound = loc.Node, 0
				break
			}
			if !b.inNarrowSet(loc.Node) {
				return loc, b.restoreAfter(ctx, cpierrors.Retriable("ha narrow: VM %d drained on node %s, which is "+
					"outside the narrower set; its requested state was put back under its old pin", b.in.VMID, loc.Node))
			}
			return loc, nil
		}
		if !b.deps.Now().Before(deadline) {
			return pve.VMLocation{}, b.restoreAfter(ctx, cpierrors.Retriable("ha narrow: VM %d did not drain "+
				"within %s: %s", b.in.VMID, b.in.Budget, why))
		}
		if err := b.deps.Sleep(ctx, b.in.PollInterval); err != nil {
			// The request ended during the drain. Nothing has changed the VM
			// or its pin yet, so its old state is safe to put back.
			return pve.VMLocation{}, b.restoreAfter(ctx, err)
		}
	}
}

// readState reads the state and refuses an unknown one.
func (b *barrier) readState(ctx context.Context, node string) (*pve.HAServiceState, error) {
	st, err := b.deps.ReadState(ctx, b.in.VMID, node)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: read the HA state of VM %d", b.in.VMID))
	}
	if st == nil || !st.Known {
		reason := "no state"
		if st != nil {
			reason = st.Reason
		}
		return nil, cpierrors.Retriable("ha narrow: the HA state of VM %d is unknown: %s", b.in.VMID, reason)
	}
	return st, nil
}

// request sets the resource's requested state in one write with its comment.
// mark puts the barrier's token, which names the state to restore, on the
// comment, and otherwise the comment goes back to what it was before the
// barrier. A held error is settled with a read of the resource.
func (b *barrier) request(ctx context.Context, state string, mark bool) error {
	want := state
	p := &sdkcluster.UpdateHaResourcesParams{State: &want}
	switch {
	case mark:
		c := withNarrowMark(b.comment, b.restore)
		p.Comment = &c
	case b.comment != nil && *b.comment != "":
		p.Comment = b.comment
	case b.marked:
		del := "comment"
		p.Delete = &del
	}
	err := b.deps.Resources.UpdateHaResources(ctx, b.sid, p)
	if err == nil {
		b.marked = mark
		return nil
	}
	got, readErr := b.deps.Resources.GetHaResources(ctx, b.sid)
	if readErr == nil && got != nil && got.State != nil && *got.State == state {
		if _, has := narrowMarkOf(got.Comment); has == mark {
			b.marked = mark
			return nil
		}
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("ha narrow: request %s for %s", state, b.sid))
}

// finish puts back the state the resource had before the barrier and drops
// the token. A resource an operator left ignored was never touched, so it
// needs no write. The request runs on a context of its own (unwindContext),
// so a mutation that spent the request's time, or a request that a stop
// signal ended, still gets the VM back under PVE HA.
func (b *barrier) finish(ctx context.Context) error {
	if !b.marked && b.restore == haStateIgnored {
		return nil
	}
	ctx, cancel := unwindContext(ctx)
	defer cancel()
	return b.request(ctx, b.restore, false)
}

// restoreAfter puts back the state the resource had before the barrier after
// a failure that left the VM where its old config may run, and returns cause.
// A failed restore is logged, and the resource stays ignored with its token for
// the next call to finish.
func (b *barrier) restoreAfter(ctx context.Context, cause error) error {
	if err := b.finish(ctx); err != nil {
		b.deps.Logger.Warn("ha narrow: could not restore the requested state; the resource stays ignored and the "+
			"next call finishes it", log.String("sid", b.sid), log.String("restore", b.restore), log.Err(err))
	}
	return cause
}

// narrowMarkOf returns the state the barrier's token on comment names, and
// whether the comment carries one.
func narrowMarkOf(comment *string) (string, bool) {
	if comment == nil {
		return "", false
	}
	for _, f := range strings.Fields(*comment) {
		if state, ok := strings.CutPrefix(f, narrowMarkKey); ok && state != "" {
			return state, true
		}
	}
	return "", false
}

// stripNarrowMark returns comment without the barrier's token, keeping the rest
// as it was. It returns nil when the comment was nil.
func stripNarrowMark(comment *string) *string {
	if comment == nil {
		return nil
	}
	out := *comment
	for _, f := range strings.Fields(*comment) {
		if !strings.HasPrefix(f, narrowMarkKey) {
			continue
		}
		if strings.Contains(out, " "+f) {
			out = strings.Replace(out, " "+f, "", 1)
		} else {
			out = strings.Replace(out, f, "", 1)
		}
	}
	return &out
}

// withNarrowMark returns comment with the token that names restore appended.
func withNarrowMark(comment *string, restore string) string {
	token := narrowMarkKey + restore
	if comment == nil || *comment == "" {
		return token
	}
	return *comment + " " + token
}

// viaRemoval is the fallback barrier. It snapshots the CPI-owned rules that
// name the VM and persists the snapshot, removes the HA resource, waits for the
// CRM and the config to drop it, runs the mutation, registers the resource
// again as ignored with its old comment, the barrier's token, and failback
// off, rewrites the narrower pin and the rule memberships, and puts back the
// state the resource had. It refuses before any write unless the snapshot can
// be persisted, because a crash after the removal would otherwise lose the
// VM's memberships.
func (b *barrier) viaRemoval(ctx context.Context) (pve.VMLocation, error) {
	if b.in.Snapshot == nil || b.in.Restore == nil || b.in.PersistSnapshot == nil {
		return pve.VMLocation{}, cpierrors.Cloud("ha narrow: the removal fallback needs a rule snapshot that is " +
			"persisted before the removal, and a restore")
	}
	st, err := b.readState(ctx, b.in.Node)
	if err != nil {
		return pve.VMLocation{}, err
	}
	if st.Raw == haStateError {
		return b.errored(ctx, st)
	}
	rules, err := b.in.Snapshot(ctx)
	if err != nil {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "ha narrow: snapshot the VM's rules")
	}
	if err := b.in.PersistSnapshot(ctx, rules); err != nil {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			"ha narrow: persist the VM's rule snapshot before the removal")
	}
	purge := true
	if err := b.deps.Resources.DeleteHaResources(ctx, b.sid, &sdkcluster.DeleteHaResourcesParams{Purge: &purge}); err != nil &&
		!pve.IsNotFound(err) && !isRuleNotFound(err) {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: remove %s for the fallback barrier", b.sid))
	}
	deadline := b.deps.Now().Add(b.in.Budget)
	node := st.Node
	for {
		cur, readErr := b.deps.ReadState(ctx, b.in.VMID, node)
		if readErr == nil && cur.Known && !cur.Managed() {
			break
		}
		if !b.deps.Now().Before(deadline) {
			return pve.VMLocation{}, b.reregister(ctx, rules, cpierrors.Retriable("ha narrow: %s did not leave the "+
				"CRM within %s", b.sid, b.in.Budget))
		}
		if err := b.deps.Sleep(ctx, b.in.PollInterval); err != nil {
			return pve.VMLocation{}, err
		}
	}
	loc, err := b.deps.Settle(ctx, b.in.VMID)
	if err != nil {
		return loc, b.reregister(ctx, rules, err)
	}
	loc, mutErr := b.mutate(ctx, loc.Node)
	if mutErr != nil {
		return loc, b.reregister(ctx, rules, mutErr)
	}
	if err := b.checkLanding(ctx, loc.Node); err != nil {
		return loc, b.reregister(ctx, rules, err)
	}
	if err := b.reregister(ctx, rules, nil); err != nil {
		return loc, err
	}
	return loc, nil
}

// reregister registers the resource again as ignored with the barrier's
// token, restores its rules with the narrower pin, and puts back the state the
// resource had, unless cause is set, in which case the resource stays ignored
// and cause is returned.
func (b *barrier) reregister(ctx context.Context, rules []Rule, cause error) error {
	ignored, failback := haStateIgnored, false
	comment := withNarrowMark(b.comment, b.restore)
	err := b.deps.Resources.CreateHaResources(ctx, &sdkcluster.CreateHaResourcesParams{
		Sid: b.sid, State: &ignored, Comment: &comment, Failback: &failback,
	})
	if err != nil && !isRuleAlreadyDefined(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: register %s again after the fallback barrier", b.sid))
	}
	b.marked = true
	if cause != nil {
		return cause
	}
	pin := b.in.NewPin
	kept := slices.DeleteFunc(slices.Clone(rules), func(r Rule) bool { return r.Type == RuleTypeNodeAffinity })
	if err := b.in.Restore(ctx, kept, pin); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: restore the rules of %s", b.sid))
	}
	return b.finish(ctx)
}
