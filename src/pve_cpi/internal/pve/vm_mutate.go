package pve

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// mutateMaxRounds bounds how many times MutateVMConfig settles and
// re-resolves after PVE refused a change for a lock or a missing config, and
// mutateMaxAmbiguous bounds how many results it may re-check after an
// ambiguous answer. The protocol's time budget is the real bound; these only
// stop an identity that keeps moving from looping until the deadline.
const (
	mutateMaxRounds    = 10
	mutateMaxAmbiguous = DefaultStorageLockMaxAttempts
)

// VMConfigMutation is one VM-config change for MutateVMConfig.
type VMConfigMutation struct {
	VMID int

	// Node is the node the caller last resolved the VM on, usually from
	// AwaitVMSettled. When it is empty, MutateVMConfig waits for the VM to
	// settle and uses the node that wait returns.
	Node string

	// MotionWait is pve.ha.motion_wait_sec, the budget for a migrate lock.
	// Zero or less means DefaultMotionWait.
	MotionWait time.Duration

	// Label names the calling operation in log lines and errors.
	Label string

	// Budget, when positive, caps the protocol's time budget at this long from
	// the start of the call, even when the context's deadline or twice the
	// motion budget would allow more. A caller that holds a lock for the call
	// or that runs a best-effort step sets it, so the time it can spend is
	// known. Zero leaves the budget as MutateVMConfig's own doc describes.
	Budget time.Duration

	// Apply sends the change to node once. It must not retry on its own,
	// because the protocol decides whether sending again is safe, and the
	// context it receives turns off the SDK's own retries for that reason. It
	// must address every identity it names through the values Reresolve last
	// set, never through ones it captured before the call.
	Apply func(ctx context.Context, node string) error

	// Landed reports whether the change is already in the VM's config on node,
	// which is the VM's current owner. MutateVMConfig calls it after an
	// ambiguous result, such as a transport error that may hide a write that
	// landed, and before it sends anything again. When Landed is nil, an
	// ambiguous result is returned as a retriable error and nothing is sent
	// again.
	Landed func(ctx context.Context, node string) (bool, error)

	// Reresolve re-resolves every other identity the change names, such as a
	// source parker or a replication job, and checks that the storage the
	// change uses still fits vm.Node. MutateVMConfig calls it after every
	// refusal for a lock or a missing config, once the VM has settled and
	// before it sends the change again, and after an ambiguous result whose
	// re-read finds the VM on another node, before it sends there. An error stops the protocol and is
	// returned as it is, so it should be retriable when the identity is only
	// moving. Nil means the change names no other identity.
	Reresolve func(ctx context.Context, vm VMLocation) error
}

func (m VMConfigMutation) label() string {
	if m.Label == "" {
		return "mutate_vm_config"
	}
	return m.Label
}

func (m VMConfigMutation) motionWait() time.Duration {
	if m.MotionWait <= 0 {
		return DefaultMotionWait
	}
	return m.MotionWait
}

// MutateVMConfig sends one VM-config change by the protocol that keeps it
// safe against a migration that starts mid-call. AwaitVMSettled only observes
// one moment, so PVE's config lock is what serializes the write with a
// migration, and this helper handles the case where the write loses that
// race.
//
//  1. Any HA policy change the mutation needs is the caller's, and it is made
//     before the call, inside the narrowing barrier when it narrows an active
//     resource.
//  2. The change goes to the node the VM is on now, which is m.Node or, when
//     that is empty, the node a settle wait returns.
//  3. When PVE refuses it because the config is locked, or because the config
//     is missing on that node, the helper waits through AwaitVMSettled,
//     re-resolves the VM (with FindVMByConfigProbe when the index still names
//     the node that answered config-missing), calls m.Reresolve for every
//     other identity and the storage fit, and sends it again.
//  4. When the result is ambiguous, which means a transport fault, a 5xx, or
//     another answer that may hide a write that landed, the helper re-reads
//     through m.Landed on the VM's current owner, and it sends again only
//     when the change did not land. When that owner is another node, it calls
//     m.Reresolve for the new node before it sends there.
//
// The budget for the whole protocol is the method's operation timeout, which
// is the context's deadline. When the context has none, the protocol bounds
// itself at twice the motion budget. A positive m.Budget shortens either one.
// Running out of it returns a retriable
// error that names the lock or the identity that kept moving. A suspended
// lock returns the retriable lock error at once, because only resuming the VM
// clears it, and every other error PVE gives is returned as it came.
//
// It returns the VM's location as of the attempt that landed. When the change
// landed on the caller's m.Node at the first attempt, that location carries
// only the node, because nothing was read.
func MutateVMConfig(ctx context.Context, c Client, logger *log.Logger, m VMConfigMutation) (VMLocation, error) {
	if m.VMID <= 0 || m.Apply == nil {
		return VMLocation{}, cpierrors.Cloud("%s: mutate VM config: VM ID %d and an apply step are required",
			m.label(), m.VMID)
	}
	clk := settleClockFrom(ctx)
	limit := clk.now().Add(2 * m.motionWait())
	if dl, ok := ctx.Deadline(); ok {
		limit = dl.Add(-settleDeadlineReserve)
	}
	if m.Budget > 0 {
		if capped := clk.now().Add(m.Budget); capped.Before(limit) {
			limit = capped
		}
	}
	settleReq := VMSettleRequest{VMID: m.VMID, MotionWait: m.MotionWait, Label: m.label()}

	loc := VMLocation{Node: m.Node, Found: m.Node != ""}
	if m.Node == "" {
		settled, err := awaitVMSettled(ctx, c, logger, settleReq, limit)
		if err != nil {
			return VMLocation{}, err
		}
		if !settled.Found {
			return VMLocation{}, cpierrors.Retriable(
				"%s: PVE VM %d is in neither the cluster index nor any member's config; the change was not sent",
				m.label(), m.VMID)
		}
		loc = settled
	}

	rounds, ambiguous := 0, 0
	for {
		err := m.Apply(sdkclient.WithRetries(ctx, 0), loc.Node)
		if err == nil {
			return loc, nil
		}
		if stopsRetry(err) {
			return VMLocation{}, err
		}
		locked, missing := IsVMConfigLocked(err), IsPmxcfsConfigMissing(err)
		switch {
		case locked && !isSettlingVMConfigLock(err):
			return VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
				"%s: PVE VM %d on node %q is locked (%s), which only resuming the VM clears; the change was not applied",
				m.label(), m.VMID, loc.Node, VMConfigLockType(err)))
		case locked || missing:
			reason := fmt.Sprintf("PVE VM %d is locked (%s) on node %q", m.VMID, VMConfigLockType(err), loc.Node)
			if missing {
				reason = fmt.Sprintf("PVE VM %d has no config on node %q, so it moved", m.VMID, loc.Node)
			}
			rounds++
			if rounds > mutateMaxRounds || !clk.now().Before(limit) {
				return VMLocation{}, mutateExhausted(m, reason, err)
			}
			logMutateRetry(logger, m, loc.Node, reason, err)
			next, rerr := m.settleAndReresolve(ctx, c, logger, settleReq, limit, loc.Node, missing)
			if rerr != nil {
				return VMLocation{}, rerr
			}
			loc = next
		case isAnsweredClientError(err):
			// PVE answered with a verdict about the request, so the outcome is
			// known even when the context ended at the same moment.
			return VMLocation{}, err
		case ctx.Err() != nil:
			return VMLocation{}, mutateOutcomeUnknown(m, loc.Node, err)
		case IsRetryableOrLockFault(err):
			ambiguous++
			if ambiguous > mutateMaxAmbiguous || !clk.now().Before(limit) || m.Landed == nil {
				return VMLocation{}, mutateOutcomeUnknown(m, loc.Node, err)
			}
			logMutateRetry(logger, m, loc.Node, "the result was ambiguous; re-reading before sending again", err)
			next, landed, aerr := m.afterAmbiguous(ctx, c, clk, limit, loc, ambiguous, err)
			if aerr != nil {
				return VMLocation{}, aerr
			}
			if landed {
				return next, nil
			}
			loc = next
		default:
			return VMLocation{}, err
		}
	}
}

// settleAndReresolve is step 3 of the protocol. It waits for the VM to
// settle, finds the node it is on now, and re-resolves every other identity.
// refusedNode is the node that refused the change, and configMissing says
// whether it refused because it holds no config for the VM.
func (m VMConfigMutation) settleAndReresolve(
	ctx context.Context, c Client, logger *log.Logger, req VMSettleRequest, limit time.Time,
	refusedNode string, configMissing bool,
) (VMLocation, error) {
	loc, err := awaitVMSettled(ctx, c, logger, req, limit)
	if err != nil {
		return VMLocation{}, err
	}
	if configMissing && (!loc.Found || loc.Node == refusedNode) {
		// The node that answered config-missing is the one the index still
		// names, or the index has lost the VM. Either way the index is
		// contradicted, so only a probe of every member can say where it went.
		probed, perr := FindVMByConfigProbe(ctx, c, m.VMID)
		if perr != nil {
			return VMLocation{}, perr
		}
		loc = probed
	}
	if !loc.Found {
		return VMLocation{}, cpierrors.Retriable(
			"%s: PVE VM %d refused the change on node %q and no member holds its config now; "+
				"the change was not sent again, and absence is not proven here", m.label(), m.VMID, refusedNode)
	}
	if m.Reresolve != nil {
		if rerr := m.Reresolve(ctx, loc); rerr != nil {
			return VMLocation{}, rerr
		}
	}
	return loc, nil
}

// afterAmbiguous runs recheck after an ambiguous result on loc. When the
// change did not land and the VM is now on another node, it calls m.Reresolve
// for that node, because the identities resolved for the old node no longer
// hold, and returns its error as it is.
func (m VMConfigMutation) afterAmbiguous(
	ctx context.Context, c Client, clk lockClock, limit time.Time, loc VMLocation, ambiguous int, sendErr error,
) (VMLocation, bool, error) {
	next, landed, err := m.recheck(ctx, c, clk, limit, ambiguous, sendErr)
	if err != nil || landed {
		return next, landed, err
	}
	if next.Node != loc.Node && m.Reresolve != nil {
		if rerr := m.Reresolve(ctx, next); rerr != nil {
			return VMLocation{}, false, rerr
		}
	}
	return next, false, nil
}

// recheck is step 4 of the protocol. It finds the VM's current owner and asks
// m.Landed whether the change is already there. A read that fails leaves the
// outcome unknown, so recheck backs off and reads again until the budget or
// the ambiguous-result bound runs out. It returns the owner it read from.
// ambiguous is how many ambiguous results the protocol has met, and it picks
// up the backoff curve where the earlier ones left it.
func (m VMConfigMutation) recheck(
	ctx context.Context, c Client, clk lockClock, limit time.Time, ambiguous int, sendErr error,
) (VMLocation, bool, error) {
	lastErr := sendErr
	for reads := 0; ; reads++ {
		if serr := mutateBackoff(ctx, clk, limit, ambiguous-1+reads, lastErr); serr != nil {
			return VMLocation{}, false, mutateOutcomeUnknown(m, "", errors.Join(sendErr, serr))
		}
		loc, landed, err := m.readLanded(ctx, c)
		if err == nil {
			return loc, landed, nil
		}
		lastErr = err
		if reads+1 >= mutateMaxAmbiguous || !clk.now().Before(limit) || ctx.Err() != nil {
			return VMLocation{}, false, mutateOutcomeUnknown(m, loc.Node, errors.Join(sendErr, err))
		}
	}
}

// readLanded locates the VM's owner and runs m.Landed there. A config-missing
// answer from the owner the index names means the VM moved, so the read runs
// once more on the node FindVMByConfigProbe finds.
func (m VMConfigMutation) readLanded(ctx context.Context, c Client) (VMLocation, bool, error) {
	loc, err := FindVMAuthoritative(ctx, c, m.VMID)
	if err != nil {
		return VMLocation{}, false, err
	}
	if !loc.Found {
		return VMLocation{}, false, cpierrors.Retriable("%s: PVE VM %d is in neither the cluster index nor any "+
			"member's config, so whether the change landed is unknown", m.label(), m.VMID)
	}
	landed, err := m.Landed(ctx, loc.Node)
	if err != nil && IsPmxcfsConfigMissing(err) {
		probed, perr := FindVMByConfigProbe(ctx, c, m.VMID)
		if perr != nil {
			return loc, false, perr
		}
		if !probed.Found || probed.Node == loc.Node {
			return loc, false, err
		}
		loc = probed
		landed, err = m.Landed(ctx, loc.Node)
	}
	if err != nil {
		return loc, false, err
	}
	return loc, landed, nil
}

// isAnsweredClientError reports whether err carries a resolved 4xx other than
// 429, which is PVE's verdict about the request and never hides a write.
func isAnsweredClientError(err error) bool {
	code, ok := apiHTTPCode(err)
	return ok && code >= 400 && code < 500 && code != 429
}

// mutateBackoff sleeps the retry curve's wait for attempt before a re-read,
// clamped to the protocol's budget. The curve follows the error the way
// RetryOnTransientOrLock picks it, and WithTestBackoff overrides it.
func mutateBackoff(ctx context.Context, clk lockClock, limit time.Time, attempt int, err error) error {
	d, _ := retryOrLockCurve(IsPVEPushback(err), IsStorageLockTimeout(err), IsClusterNotQuorate(err),
		isSettlingVMConfigLock(err), attempt)
	if override := backoffFromCtx(ctx); override != nil {
		d = override(attempt)
	}
	if remaining := limit.Sub(clk.now()); d > remaining {
		d = max(remaining, 0)
	}
	return clk.sleep(ctx, d)
}

// awaitVMSettled is AwaitVMSettled bounded by a caller's limit as well as the
// context's deadline, so the settle waits inside MutateVMConfig share the
// protocol's budget. A derived context cannot carry that limit, because a
// test clock's times are not wall-clock times.
func awaitVMSettled(
	ctx context.Context, c Client, logger *log.Logger, req VMSettleRequest, limit time.Time,
) (VMLocation, error) {
	req.notAfter = limit
	return AwaitVMSettled(ctx, c, logger, req)
}

func logMutateRetry(logger *log.Logger, m VMConfigMutation, node, reason string, err error) {
	if logger == nil {
		return
	}
	logger.Info("pve: VM config change refused or unconfirmed; following the VM before sending again",
		log.String("op", m.label()),
		log.Int("vmid", m.VMID),
		log.String("node", node),
		log.String("reason", reason),
		log.Err(err),
	)
}

// mutateExhausted is the retriable error for a protocol whose budget or
// round bound ran out while the VM stayed locked or kept moving.
func mutateExhausted(m VMConfigMutation, reason string, err error) error {
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("%s: VM config change not applied before the budget ran out: %s", m.label(), reason))
}

// mutateOutcomeUnknown is the retriable error for a change whose result PVE
// never confirmed either way.
func mutateOutcomeUnknown(m VMConfigMutation, node string, err error) error {
	where := ""
	if node != "" {
		where = fmt.Sprintf(" on node %q", node)
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("%s: PVE VM %d config change%s has an unknown outcome; it was not sent again",
			m.label(), m.VMID, where))
}
