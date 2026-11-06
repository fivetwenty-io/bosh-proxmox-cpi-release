package ha

import (
	"context"
	"fmt"
	"slices"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// narrowUseRemoval selects the barrier's fallback, which removes the HA
// resource around the mutation and registers it again, in place of the
// ignored request. It is an internal switch, off by default, for a PVE release
// whose CRM keeps acting on an ignored resource.
var narrowUseRemoval = false

// Defaults for the barrier's waits.
const (
	NarrowDefaultBudget = 5 * time.Minute
	narrowDefaultPoll   = 2 * time.Second
)

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
	// pin removes the VM's pin.
	WritePin func(ctx context.Context, pin *Rule) error
	// Mutation is the config change. Its Node is set from the drain.
	Mutation pve.VMConfigMutation
	// CheckReach re-reads the config after the mutation and checks that every
	// node of NewPin reaches the VM's volumes. nil checks nothing.
	CheckReach func(ctx context.Context, node string) error
	// SysConsole says the token holds Sys.Console, which the ignored and
	// started requests need.
	SysConsole bool
	// Budget bounds every wait of the barrier, and PollInterval is the pause
	// between drain reads.
	Budget       time.Duration
	PollInterval time.Duration
	// Snapshot and Restore serve the removal fallback. Snapshot returns the
	// CPI-owned rules that name the VM, and Restore writes NewPin and puts the
	// VM back into those rules.
	Snapshot func(ctx context.Context) ([]Rule, error)
	Restore  func(ctx context.Context, rules []Rule, pin *Rule) error
}

// NarrowActive applies a change that narrows where an HA-managed VM may run,
// without letting the CRM move it in between. The order is:
//
//  1. Request ignored, so the CRM stops acting on the VM.
//  2. Wait for the drain: the CRM's service_status no longer lists the
//     service, in a CRM round newer than the request on the entry node's
//     clock, the VM's node has an LRM report newer than that round, the VM
//     holds no lock, and the scan names the same node. When the VM moved
//     outside the narrower set, request started again under the old pin and
//     return a retriable error.
//  3. Write the narrower pin and verify it.
//  4. Run the mutation.
//  5. Re-read the config and check volume reach and the VM's node.
//  6. Request started again. There is no watchdog check here, because the VM
//     stays where it is.
//
// A crash at any step leaves an ignored resource, and the next call finds it
// ignored and finishes the order. A widening change skips the barrier: it runs
// the mutation and then writes the pin. A resource in error returns a
// retriable error, and the barrier never requests disabled.
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
	b.comment = res.Comment
	// An ignored resource is what a crash inside an earlier barrier leaves, so
	// the barrier finishes it whether or not this change narrows.
	resumed := res.State != nil && *res.State == haStateIgnored
	if !narrowing && !resumed {
		return b.widen(ctx)
	}
	if narrowUseRemoval && !resumed {
		return b.viaRemoval(ctx)
	}
	return b.viaIgnored(ctx, resumed)
}

type barrier struct {
	deps    NarrowDeps
	in      NarrowInput
	sid     string
	comment *string
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
		return pve.VMLocation{}, cpierrors.Retriable("ha narrow: the HA resource of VM %d is in the error state; "+
			"the operator must clear it, and the CPI never requests disabled", b.in.VMID)
	}
	if !resumed {
		if err := b.request(ctx, haStateIgnored); err != nil {
			return pve.VMLocation{}, err
		}
		b.deps.Logger.Info("ha narrow: requested ignored before a narrowing change",
			log.Int("vmid", b.in.VMID), log.String("sid", b.sid))
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
	loc, err = b.mutate(ctx, loc.Node)
	if err != nil {
		// The VM sits inside the narrower set, which the old config allows
		// too, so starting it again is safe whether the mutation landed or not.
		return loc, b.restoreAfter(ctx, err)
	}
	if err := b.checkLanding(ctx, loc.Node); err != nil {
		// The resource stays ignored, because HA could otherwise recover the
		// VM onto a node that cannot reach its volumes. The next call finishes.
		return loc, err
	}
	if err := b.request(ctx, haStateStarted); err != nil {
		return loc, err
	}
	return loc, nil
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
		case st.Raw == haStateError:
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
					"outside the narrower set; it was set back to started under its old pin", b.in.VMID, loc.Node))
			}
			return loc, nil
		}
		if !b.deps.Now().Before(deadline) {
			return pve.VMLocation{}, b.restoreAfter(ctx, cpierrors.Retriable("ha narrow: VM %d did not drain "+
				"within %s: %s", b.in.VMID, b.in.Budget, why))
		}
		if err := b.deps.Sleep(ctx, b.in.PollInterval); err != nil {
			return pve.VMLocation{}, err
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

// request sets the resource's requested state and settles a held error with a
// read of the resource.
func (b *barrier) request(ctx context.Context, state string) error {
	want := state
	err := b.deps.Resources.UpdateHaResources(ctx, b.sid, &sdkcluster.UpdateHaResourcesParams{State: &want})
	if err == nil {
		return nil
	}
	got, readErr := b.deps.Resources.GetHaResources(ctx, b.sid)
	if readErr == nil && got != nil && got.State != nil && *got.State == state {
		return nil
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("ha narrow: request %s for %s", state, b.sid))
}

// restoreAfter requests started again after a failure that left the VM where
// its old config may run, and returns cause. A failed restore is logged, and
// the resource stays ignored for the next call to finish.
func (b *barrier) restoreAfter(ctx context.Context, cause error) error {
	if err := b.request(ctx, haStateStarted); err != nil {
		b.deps.Logger.Warn("ha narrow: could not request started again; the resource stays ignored and the next "+
			"call finishes it", log.String("sid", b.sid), log.Err(err))
	}
	return cause
}

// viaRemoval is the fallback barrier. It snapshots the CPI-owned rules that
// name the VM, removes the HA resource, waits for the CRM and the config to
// drop it, runs the mutation, registers the resource again as ignored with its
// old comment and failback off, rewrites the narrower pin and the rule
// memberships, and requests started.
func (b *barrier) viaRemoval(ctx context.Context) (pve.VMLocation, error) {
	if b.in.Snapshot == nil || b.in.Restore == nil {
		return pve.VMLocation{}, cpierrors.Cloud("ha narrow: the removal fallback needs a rule snapshot and restore")
	}
	st, err := b.readState(ctx, b.in.Node)
	if err != nil {
		return pve.VMLocation{}, err
	}
	if st.Raw == haStateError {
		return pve.VMLocation{}, cpierrors.Retriable("ha narrow: the HA resource of VM %d is in the error state; "+
			"the operator must clear it, and the CPI never requests disabled", b.in.VMID)
	}
	rules, err := b.in.Snapshot(ctx)
	if err != nil {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "ha narrow: snapshot the VM's rules")
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

// reregister registers the resource again as ignored, restores its rules
// with the narrower pin, and requests started, unless cause is set, in which
// case the resource stays ignored and cause is returned.
func (b *barrier) reregister(ctx context.Context, rules []Rule, cause error) error {
	ignored, failback := haStateIgnored, false
	err := b.deps.Resources.CreateHaResources(ctx, &sdkcluster.CreateHaResourcesParams{
		Sid: b.sid, State: &ignored, Comment: b.comment, Failback: &failback,
	})
	if err != nil && !isRuleAlreadyDefined(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: register %s again after the fallback barrier", b.sid))
	}
	if cause != nil {
		return cause
	}
	pin := b.in.NewPin
	kept := slices.DeleteFunc(slices.Clone(rules), func(r Rule) bool { return r.Type == RuleTypeNodeAffinity })
	if err := b.in.Restore(ctx, kept, pin); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha narrow: restore the rules of %s", b.sid))
	}
	return b.request(ctx, haStateStarted)
}
