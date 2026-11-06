package pve

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// HATarget is the state a caller asks SetHAState to bring an HA-managed VM to.
type HATarget string

const (
	// HATargetStarted asks for the VM to run under HA.
	HATargetStarted HATarget = "started"

	// HATargetStopped asks for the VM to be stopped and to stay stopped under HA.
	HATargetStopped HATarget = "stopped"
)

// haStateDisabled is the request that clears the error state. SetHAState sends
// it only while it clears an error and never as a caller's target, because it
// stops a running guest.
const haStateDisabled = "disabled"

// QEMU power states SetHAState reads from the owner node.
const (
	haPowerRunning = "running"
	haPowerStopped = "stopped"
)

const (
	// haStateDefaultPollInterval is the poll cadence when the caller sets none.
	haStateDefaultPollInterval = 2 * time.Second

	// haStateWriteMaxAttempts bounds the attempts of one state request. Every
	// attempt after a failure follows a re-read that proved the request did not
	// land.
	haStateWriteMaxAttempts = 4
)

// HACaller says what the caller of SetHAState does with the VM afterwards,
// because clearing an error stops the guest on one path and must never stop it
// on the other.
type HACaller int

const (
	// HACallerTeardown is the teardown protocol, which destroys the VM next.
	// Stopping the guest to clear an error costs it nothing.
	HACallerTeardown HACaller = iota + 1

	// HACallerKeep is a caller that keeps the VM, such as reboot_vm. A running
	// guest must survive whatever SetHAState does.
	HACallerKeep
)

func (c HACaller) valid() bool { return c == HACallerTeardown || c == HACallerKeep }

// HAStateOutcome is what SetHAState achieved when it returns no error.
type HAStateOutcome string

const (
	// HAStateReached means the VM reached the target state.
	HAStateReached HAStateOutcome = "reached"

	// HAStateNeedsReregistration means the resource is in error while the VM
	// that BOSH keeps is running. SetHAState wrote nothing, because the only
	// request PVE accepts would stop the guest. The caller clears the error by
	// removing the resource and registering it again, and then asks for its
	// state again. A caller that cannot re-register returns
	// SetHAStateResult.ReregistrationError.
	HAStateNeedsReregistration HAStateOutcome = "needs_reregistration"
)

// HAStateCluster is the subset of the PVE cluster API that SetHAState uses. It
// is satisfied by the sdk cluster service.
type HAStateCluster interface {
	HAServiceStateCluster
	UpdateHaResources(ctx context.Context, sid string, params *sdkcluster.UpdateHaResourcesParams) error
}

// HAStateQEMU is the subset of the qemu service that SetHAState uses: the power
// read on the owner node and the direct start and stop calls, which PVE sends
// through ha-manager for an HA-managed VM. It is satisfied by qemu.Service.
type HAStateQEMU interface {
	Status(ctx context.Context, node string, vmid int) (map[string]interface{}, error)
	Start(ctx context.Context, node string, vmid int) (string, error)
	Stop(ctx context.Context, node string, vmid int) (string, error)
}

// HAStateDeps holds the clients SetHAState calls.
type HAStateDeps struct {
	Cluster HAStateCluster
	Nodes   NodeHealthNodes
	QEMU    HAStateQEMU

	// AwaitTask waits for the task of a direct start or stop call, for at most
	// maxWait. HAStateDepsFor wires it to AwaitTask.
	AwaitTask func(ctx context.Context, node, upid string, maxWait time.Duration) error
}

// HAStateDepsFor returns the SetHAState clients of c.
func HAStateDepsFor(c Client) HAStateDeps {
	return HAStateDeps{
		Cluster: c.Cluster(),
		Nodes:   c.Nodes(),
		QEMU:    c.QEMU(),
		AwaitTask: func(ctx context.Context, node, upid string, maxWait time.Duration) error {
			return AwaitTask(ctx, c, node, upid, WithMaxWait(maxWait))
		},
	}
}

// SetHAStateInput names the VM, the target, and the caller.
type SetHAStateInput struct {
	// VMID is the guest ID. It must be positive.
	VMID int

	// Node is the node the cluster scan names for the VM. The CRM's node wins
	// once the CRM holds the service.
	Node string

	// Target is the state the caller wants. The teardown protocol may only ask
	// for HATargetStopped.
	Target HATarget

	// Caller says whether the VM is torn down next or kept.
	Caller HACaller

	// SysConsole is the profile's Sys.Console answer. With it, SetHAState
	// writes the HA resource's requested state. Without it, it uses the direct
	// start or stop call, which needs only VM.PowerMgmt, and it cannot clear an
	// error.
	SysConsole bool

	// Budget bounds the whole call, every wait included. Callers pass
	// pve.ha.motion_wait_sec. It must be positive.
	Budget time.Duration

	// PollInterval is the poll cadence, two seconds when zero.
	PollInterval time.Duration

	// HealthOptions applies to the node health report of every read.
	HealthOptions NodeHealthOptions
}

// SetHAStateResult is what SetHAState returns with a nil error.
type SetHAStateResult struct {
	Outcome HAStateOutcome
	SID     string
	VMID    int

	// Node is the node the VM is on according to State.
	Node string

	// ClearedError is true when SetHAState requested disabled to clear an
	// error before it asked for the target.
	ClearedError bool

	// State is the last read. For HAStateNeedsReregistration it is the decision
	// read, whose SID and UID a re-registration compares with a fresh read
	// before it removes the resource.
	State *HAServiceState
}

// ReregistrationError returns the retriable error that a caller returns when
// the result asks for a re-registration it cannot perform. It names the error
// state and the commands that clear it without stopping the guest. It returns
// nil for any other outcome.
func (r *SetHAStateResult) ReregistrationError() error {
	if r == nil || r.Outcome != HAStateNeedsReregistration {
		return nil
	}
	return cpierrors.Retriable("%s is in the HA error state while its VM runs on node %s, and the only state "+
		"request PVE accepts there, disabled, would stop a guest that BOSH keeps; clear the error without "+
		"stopping the guest by running 'ha-manager remove %s' and then 'ha-manager add %s --state started', "+
		"restore the resource's HA rules, and retry",
		r.SID, r.Node, r.SID, r.SID)
}

// SetHAState brings an HA-managed VM to target and waits until it gets there,
// within in.Budget.
//
// A direct start or stop of an HA-managed VM returns once ha-manager has queued
// the request, so the call alone proves nothing. With Sys.Console, SetHAState
// writes the requested state with PUT /cluster/ha/resources/{sid}. Without it,
// it makes the direct call, which PVE redirects through ha-manager. Either way
// it then polls the normalized HA state as a live-node decision, and the target
// counts as reached only when the CRM shows it, the configured request matches,
// and the owner node reports the matching power state.
//
// A held transport error leaves a write ambiguous, because the request may
// have landed. SetHAState then re-reads the resource and compares its request
// with what it meant to write before it retries anything.
//
// PVE refuses every request except disabled for a resource in error. The
// teardown caller, and a kept VM whose owner node reports it stopped, get the
// disabled request and a wait for the CRM to show stopped, and only then the
// target. A running kept VM never gets disabled: the result asks for a
// re-registration instead and SetHAState writes nothing. Clearing an error needs
// Sys.Console, so without it the call returns a retriable error that names the
// privilege and the command that fits the caller.
//
// SetHAState writes nothing and returns a retriable error when the first read
// is unknown, says to defer, shows a resource that is not configured or is
// ignored, or stays in motion toward another state past the budget. A wrong
// call returns a non-retriable error, and a request that PVE definitely
// refused, such as a permission check or a failed task, keeps the class that
// WrapError gives it.
func SetHAState(ctx context.Context, deps HAStateDeps, in SetHAStateInput) (*SetHAStateResult, error) {
	if err := validateSetHAState(ctx, deps, in); err != nil {
		return nil, err
	}
	interval := in.PollInterval
	if interval <= 0 {
		interval = haStateDefaultPollInterval
	}
	run := &haStateRun{
		deps:     deps,
		in:       in,
		sid:      HAServiceSID(in.VMID),
		deadline: time.Now().Add(in.Budget),
		interval: interval,
		logger:   log.FromContext(ctx),
	}

	st, err := run.settle(ctx)
	if err != nil {
		return nil, err
	}
	run.node = st.Node
	res := &SetHAStateResult{SID: run.sid, VMID: in.VMID, Node: st.Node, State: st}

	if st.Raw == haRawError {
		cleared, needsReadd, clearErr := run.clearError(ctx, st)
		if clearErr != nil {
			return nil, clearErr
		}
		if needsReadd {
			res.Outcome = HAStateNeedsReregistration
			return res, nil
		}
		res.ClearedError = true
		st = cleared
	}

	if st.Requested != string(in.Target) {
		if err := run.request(ctx, string(in.Target)); err != nil {
			return nil, err
		}
	}
	final, err := run.await(ctx, string(in.Target))
	if err != nil {
		return nil, err
	}
	res.Outcome = HAStateReached
	res.Node = final.Node
	res.State = final
	run.logger.Info("ha: VM reached its requested state",
		log.String("sid", run.sid), log.String("state", string(in.Target)), log.String("node", final.Node),
		log.Bool("cleared_error", res.ClearedError), log.Bool("direct_call", !in.SysConsole))
	return res, nil
}

func validateSetHAState(ctx context.Context, deps HAStateDeps, in SetHAStateInput) error {
	switch {
	case ctx == nil:
		return cpierrors.Cloud("SetHAState: ctx must not be nil")
	case deps.Cluster == nil || deps.Nodes == nil || deps.QEMU == nil:
		return cpierrors.Cloud("SetHAState: cluster, nodes, and qemu clients must not be nil")
	case !in.SysConsole && deps.AwaitTask == nil:
		return cpierrors.Cloud("SetHAState: the direct path needs a task awaiter")
	case in.VMID <= 0:
		return cpierrors.Cloud("SetHAState: VM ID %d must be positive", in.VMID)
	case in.Target != HATargetStarted && in.Target != HATargetStopped:
		return cpierrors.Cloud("SetHAState: target %q is not started or stopped", in.Target)
	case !in.Caller.valid():
		return cpierrors.Cloud("SetHAState: caller %d is not set", in.Caller)
	case in.Caller == HACallerTeardown && in.Target != HATargetStopped:
		return cpierrors.Cloud("SetHAState: the teardown protocol may only ask for stopped, not %q", in.Target)
	case in.Budget <= 0:
		return cpierrors.Cloud("SetHAState: budget %s must be positive", in.Budget)
	}
	return nil
}

// haStateRun carries one SetHAState call.
type haStateRun struct {
	deps     HAStateDeps
	in       SetHAStateInput
	sid      string
	deadline time.Time
	interval time.Duration
	logger   *log.Logger

	// node is the VM's node from the latest known read. The direct call goes
	// there.
	node string
}

// read takes one normalized read as a live-node decision.
func (r *haStateRun) read(ctx context.Context) (*HAServiceState, error) {
	return ReadHAServiceState(ctx, r.deps.Cluster, r.deps.Nodes, HAServiceStateInput{
		VMID: r.in.VMID, VMNode: r.in.Node, Kind: HADecisionLive, HealthOptions: r.in.HealthOptions,
	})
}

// sleep waits one poll interval, or less when the deadline is nearer. It
// returns false when the budget is spent or the context ended.
func (r *haStateRun) sleep(ctx context.Context) bool {
	left := time.Until(r.deadline)
	if left <= 0 {
		return false
	}
	d := r.interval
	if d > left {
		d = left
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ended returns the retriable error for a context that ended, or nil.
func (r *haStateRun) ended(ctx context.Context, doing string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cpierrors.WrapAs(ctxErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("SetHAState: %s for %s ended with the call", doing, r.sid))
	}
	return nil
}

// settle reads the service until a decision may act on it. Nothing has been
// written yet, so every answer that is not a plain go-ahead returns a
// retriable error, except motion, which it waits through within the budget.
// Motion toward the caller's own target is left to await.
func (r *haStateRun) settle(ctx context.Context) (*HAServiceState, error) {
	for {
		st, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if !st.Known {
			return nil, cpierrors.Retriable("SetHAState: the HA state of %s is unknown (%s), so nothing was written; "+
				"retry when the cluster answers", r.sid, st.Reason)
		}
		switch {
		case !st.Configured && st.InCRM:
			return nil, cpierrors.Retriable("SetHAState: the CRM still lists %s but it has no HA resource, so it "+
				"is being removed; retry when the CRM drops it", r.sid)
		case !st.Configured:
			return nil, cpierrors.Retriable("SetHAState: %s is not an HA resource, so there is no HA state to "+
				"change", r.sid)
		case st.Requested == haRequestedIgnored:
			return nil, cpierrors.Retriable("SetHAState: %s is ignored, so the CRM does not manage it and its state "+
				"is not changed here; finish or undo whatever set it ignored, then retry", r.sid)
		}
		disp, why := st.Disposition()
		switch disp {
		case HAProceed:
			return st, nil
		case HAWaitForMotion:
			if st.Requested == string(r.in.Target) && st.Raw != haRawError {
				return st, nil
			}
			r.logger.Debug("ha: waiting for motion before a state request",
				log.String("sid", r.sid), log.String("reason", why))
			if !r.sleep(ctx) {
				if err := r.ended(ctx, "the wait for motion"); err != nil {
					return nil, err
				}
				return nil, cpierrors.Retriable("SetHAState: %s is still in motion after %s (%s), so nothing was "+
					"written; retry when it settles", r.sid, r.in.Budget, why)
			}
		default:
			return nil, cpierrors.Retriable("SetHAState: %s cannot change state now (%s), so nothing was written",
				r.sid, why)
		}
	}
}

// clearError handles a resource in error. It returns the state read after the
// error cleared, or needsReadd when the VM is a running guest that BOSH keeps,
// or an error. It never sends disabled to a guest that a kept caller cannot
// prove stopped.
func (r *haStateRun) clearError(
	ctx context.Context, st *HAServiceState,
) (cleared *HAServiceState, needsReadd bool, err error) {
	power, powerErr := r.power(ctx, st.Node)
	stopped := powerErr == nil && power == haPowerStopped

	if !r.in.SysConsole {
		return nil, false, r.errorWithoutPrivilege(st, power, powerErr)
	}
	if r.in.Caller == HACallerKeep {
		if powerErr != nil {
			return nil, false, cpierrors.Wrap(powerErr, fmt.Sprintf("SetHAState: %s is in the HA error state and "+
				"the power state of its VM on node %s is unreadable, so the error is not cleared", r.sid, st.Node))
		}
		if !stopped {
			r.logger.Info("ha: errored resource runs a kept VM, so it needs re-registration",
				log.String("sid", r.sid), log.String("node", st.Node), log.String("uid", st.UID),
				log.String("power", power))
			return nil, true, nil
		}
	}

	r.logger.Info("ha: clearing the error state with disabled",
		log.String("sid", r.sid), log.String("node", st.Node), log.String("uid", st.UID),
		log.Bool("teardown", r.in.Caller == HACallerTeardown), log.String("power", power))
	if err := r.request(ctx, haStateDisabled); err != nil {
		return nil, false, err
	}
	cleared, err = r.await(ctx, haStateDisabled)
	if err != nil {
		return nil, false, err
	}
	return cleared, false, nil
}

// errorWithoutPrivilege is the retriable error for a resource in error when the
// token lacks Sys.Console. The command it names fits the caller: disabled stops
// the guest, so it is offered only to the teardown caller and for a kept VM
// that the owner node reports stopped.
func (r *haStateRun) errorWithoutPrivilege(st *HAServiceState, power string, powerErr error) error {
	switch {
	case r.in.Caller == HACallerTeardown:
		return cpierrors.Retriable("SetHAState: %s is in the HA error state on node %s and clearing it needs "+
			"Sys.Console on /, which the token lacks; run 'ha-manager set %s --state disabled' and retry",
			r.sid, st.Node, r.sid)
	case powerErr == nil && power == haPowerStopped:
		return cpierrors.Retriable("SetHAState: %s is in the HA error state with its VM stopped on node %s, and "+
			"clearing it needs Sys.Console on /, which the token lacks; run 'ha-manager set %s --state disabled' "+
			"and then 'ha-manager set %s --state %s', and retry",
			r.sid, st.Node, r.sid, r.sid, r.in.Target)
	default:
		state := power
		if powerErr != nil {
			state = "unreadable"
		}
		return cpierrors.Retriable("SetHAState: %s is in the HA error state while its VM on node %s is %s, and "+
			"clearing it without stopping the guest needs Sys.Console on /, which the token lacks; run "+
			"'ha-manager remove %s' and then 'ha-manager add %s --state started', restore the resource's HA rules, "+
			"and retry",
			r.sid, st.Node, state, r.sid, r.sid)
	}
}

// power reads the VM's power state on node.
func (r *haStateRun) power(ctx context.Context, node string) (string, error) {
	var status map[string]interface{}
	err := RetryOnTransient(ctx, nil, "ha_state_power", 0, func() error {
		var inner error
		status, inner = r.deps.QEMU.Status(ctx, node, r.in.VMID)
		return inner
	})
	if err != nil {
		return "", WrapError(err)
	}
	state, ok := ConfigString(status, "status")
	if !ok || state == "" {
		return "", cpierrors.Retriable("node %s reported no power state for VM %d", node, r.in.VMID)
	}
	return state, nil
}

// request asks for state, through the HA resource with Sys.Console and through
// the direct call without it. After a failed attempt it re-reads the resource,
// and a request that landed counts as sent. Only a failure that the re-read
// proved did not land, and that is transient, is tried again.
func (r *haStateRun) request(ctx context.Context, state string) error {
	var lastErr error
	for attempt := 0; attempt < haStateWriteMaxAttempts; attempt++ {
		err := r.send(ctx, state)
		if err == nil {
			r.logger.Info("ha: state requested",
				log.String("sid", r.sid), log.String("state", state), log.Bool("direct_call", !r.in.SysConsole))
			return nil
		}
		if r.landed(ctx, state) {
			r.logger.Info("ha: state request landed despite its error",
				log.String("sid", r.sid), log.String("state", state), log.Err(err))
			return nil
		}
		lastErr = err
		if !IsTransientTransport(err) && !IsPVEPushback(err) {
			break
		}
		d := TransientBackoffFor(ctx, attempt)
		if IsPVEPushback(err) && backoffFromCtx(ctx) == nil {
			d = PushbackBackoff(attempt)
		}
		if attempt == haStateWriteMaxAttempts-1 || time.Until(r.deadline) <= d {
			break
		}
		r.logger.Info("ha: retrying a state request that did not land",
			log.String("sid", r.sid), log.String("state", state), log.Int("attempt", attempt+1), log.Err(err))
		select {
		case <-ctx.Done():
			return cpierrors.WrapAs(contextEndedDuringBackoff(ctx.Err(), err), cpierrors.TypeRetriableCloud,
				fmt.Sprintf("SetHAState: requesting %s for %s ended with the call", state, r.sid))
		case <-time.After(d):
		}
	}
	return cpierrors.Wrap(WrapError(lastErr), fmt.Sprintf("SetHAState: requesting %s for %s failed", state, r.sid))
}

// send makes one state request.
func (r *haStateRun) send(ctx context.Context, state string) error {
	if r.in.SysConsole {
		return r.deps.Cluster.UpdateHaResources(ctx, r.sid, &sdkcluster.UpdateHaResourcesParams{State: &state})
	}
	node := r.node
	if node == "" {
		node = r.in.Node
	}
	var upid string
	var err error
	switch state {
	case string(HATargetStarted):
		upid, err = r.deps.QEMU.Start(ctx, node, r.in.VMID)
	case string(HATargetStopped):
		upid, err = r.deps.QEMU.Stop(ctx, node, r.in.VMID)
	default:
		return cpierrors.Cloud("SetHAState: the direct path cannot request %q", state)
	}
	if err != nil {
		return err
	}
	if upid == "" {
		return nil
	}
	left := time.Until(r.deadline)
	if left < time.Second {
		left = time.Second
	}
	if awaitErr := r.deps.AwaitTask(ctx, node, upid, left); awaitErr != nil {
		if IsTaskExitVerdict(awaitErr) {
			return cpierrors.Wrap(awaitErr, fmt.Sprintf("SetHAState: PVE refused the direct %s request for %s",
				state, r.sid))
		}
		// The task was created, so the request was sent. Whether ha-manager
		// queued it is what the poll finds out.
		r.logger.Info("ha: direct state request task did not resolve, polling the state instead",
			log.String("sid", r.sid), log.String("upid", upid), log.Err(awaitErr))
	}
	return nil
}

// landed reports whether a request for state reached the resource
// configuration after its call failed. A re-read that fails proves nothing,
// so it reports false.
func (r *haStateRun) landed(ctx context.Context, state string) bool {
	var resp *sdkcluster.GetHaResourcesResponse
	err := RetryOnTransient(ctx, nil, "ha_state_reread", 0, func() error {
		var inner error
		resp, inner = r.deps.Cluster.GetHaResources(ctx, r.sid)
		return inner
	})
	if err != nil || resp == nil || resp.State == nil {
		return false
	}
	got := strings.TrimSpace(*resp.State)
	if got == haRequestedEnabled {
		got = haRequestedStarted
	}
	return got == state
}

// await polls until the service shows state. For disabled, the CRM shows
// stopped. A read that is unknown, or a target the CRM has not reached yet,
// keeps the poll going until the budget runs out. A fence, a recovery, a new
// error, or a resource that disappears ends it at once.
func (r *haStateRun) await(ctx context.Context, state string) (*HAServiceState, error) {
	wantRaw, wantPower := state, haPowerStopped
	switch state {
	case haStateDisabled:
		wantRaw = haRawStopped
	case string(HATargetStarted):
		wantPower = haPowerRunning
	}
	for {
		st, err := r.read(ctx)
		if err != nil {
			return nil, err
		}
		if st.Known && st.Node != "" {
			r.node = st.Node
		}
		done, why, stop := r.check(ctx, st, state, wantRaw, wantPower)
		if stop != nil {
			return nil, stop
		}
		if done {
			return st, nil
		}
		if !r.sleep(ctx) {
			if err := r.ended(ctx, "the wait for "+state); err != nil {
				return nil, err
			}
			return nil, cpierrors.Retriable("SetHAState: %s did not reach %s within %s (last: %s); retry when "+
				"it settles", r.sid, state, r.in.Budget, why)
		}
	}
}

// check judges one poll read. It returns done when the target is reached, the
// reason it is not there yet, and a non-nil stop error when the poll must end.
func (r *haStateRun) check(
	ctx context.Context, st *HAServiceState, state, wantRaw, wantPower string,
) (done bool, why string, stop error) {
	if !st.Known {
		return false, "state unknown: " + st.Reason, nil
	}
	if !st.Configured && !st.InCRM {
		return false, "", cpierrors.Retriable("SetHAState: %s disappeared from HA while it was asked for %s",
			r.sid, state)
	}
	if st.Raw == haRawError && state != haStateDisabled {
		return false, "", cpierrors.Retriable("SetHAState: %s entered the HA error state on node %s while it "+
			"was asked for %s, so PVE HA gave up on it; retry, and the next attempt clears the error first",
			r.sid, st.Node, state)
	}
	disp, reason := st.Disposition()
	switch disp {
	case HADefer, HAGraceWindow:
		return false, "", cpierrors.Retriable("SetHAState: %s cannot reach %s now (%s)", r.sid, state, reason)
	case HAProceed:
	default:
		return false, reason, nil
	}
	if st.Requested != state {
		return false, fmt.Sprintf("%s requests %q, not %q", r.sid, st.Requested, state), nil
	}
	if st.Raw != wantRaw {
		return false, fmt.Sprintf("%s is %s on node %s", r.sid, st.Raw, st.Node), nil
	}
	power, err := r.power(ctx, st.Node)
	if err != nil {
		return false, fmt.Sprintf("the power state on node %s is unreadable: %v", st.Node, err), nil
	}
	if power != wantPower {
		return false, fmt.Sprintf("the CRM shows %s but node %s reports the VM %s", st.Raw, st.Node, power), nil
	}
	return true, "", nil
}
