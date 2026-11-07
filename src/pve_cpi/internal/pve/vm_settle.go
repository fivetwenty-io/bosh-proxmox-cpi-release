package pve

import (
	"context"
	"fmt"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// DefaultMotionWait is the migrate-lock budget AwaitVMSettled and
// MutateVMConfig use when the caller passes none. It matches the default of
// pve.ha.motion_wait_sec, which callers that hold the config pass instead.
const DefaultMotionWait = 600 * time.Second

// Wait budgets for the locks that are not a migration. A backup, a snapshot,
// or a clone holds its lock for as long as it copies data, so it gets two
// minutes. Every other lock PVE takes for a task, such as create or rollback,
// gets one minute.
const (
	settleTaskLockBudget  = 120 * time.Second
	settleOtherLockBudget = 60 * time.Second
)

// settleDeadlineReserve is the time AwaitVMSettled leaves on a context that
// carries a deadline, so that a wait that runs out still returns its own
// retriable error, naming the lock, before the method's operation timeout
// cuts the call off.
const settleDeadlineReserve = 5 * time.Second

// SettleDeadlineReserve is the part of a context's deadline AwaitVMSettled
// leaves unspent, so a caller that bounds the wait to a time of its own sets
// the context's deadline this much later.
const SettleDeadlineReserve = settleDeadlineReserve

// The settle poll starts at two seconds and grows to ten, so a short lock
// clears quickly and a long migration costs one cluster scan every ten
// seconds.
const (
	settlePollBase = 2 * time.Second
	settlePollCap  = 10 * time.Second
)

// settleClockKey carries the clock WithVMSettleClockForTest installs.
type settleClockKey struct{}

// WithVMSettleClockForTest returns a derived context whose AwaitVMSettled and
// MutateVMConfig calls read time from now and wait through sleep, so tests
// can drive a ten-minute migration without real waits. sleep must honor ctx.
// Production code MUST NOT call this.
func WithVMSettleClockForTest(
	ctx context.Context, now func() time.Time, sleep func(ctx context.Context, d time.Duration) error,
) context.Context {
	return context.WithValue(ctx, settleClockKey{}, lockClock{now: now, sleep: sleep})
}

// settleClockFrom returns the clock installed by WithVMSettleClockForTest, or
// the wall clock.
func settleClockFrom(ctx context.Context) lockClock {
	if clk, ok := ctx.Value(settleClockKey{}).(lockClock); ok && clk.now != nil && clk.sleep != nil {
		return clk
	}
	return defaultLockClock()
}

// VMSettleRequest names the VM AwaitVMSettled waits for.
type VMSettleRequest struct {
	VMID int

	// MotionWait is the budget for a migrate lock or an HA migration, which is
	// pve.ha.motion_wait_sec. Zero or less means DefaultMotionWait.
	MotionWait time.Duration

	// Label names the calling operation in log lines and errors.
	Label string

	// ProbeFoundSettles makes a VM that only the config probe found count as
	// settled, where the wait would otherwise poll for up to a minute until
	// the cluster index has a row for it. A caller sets it only when its
	// next step is a MutateVMConfig change, which meets a lock or a move
	// the probe could not see and handles it, so it needs no scan row to be
	// safe. A VM that was just created is in that state until the index
	// catches up.
	ProbeFoundSettles bool

	// ScanFailureBudget caps how long a cluster scan that keeps failing is
	// polled before the wait gives up with the scan's last error. Zero or
	// less means the 60-second default. A caller that holds up an operator,
	// or that has not written anything yet and can fail closed at once, sets
	// a smaller figure. It never lengthens a budget a lock or an HA state
	// has set.
	ScanFailureBudget time.Duration

	// IgnoreHAState makes the wait read nothing of the row's HA state: it
	// neither refuses a service in fence or recovery nor waits for an HA
	// migration. A caller sets it when pve.ha.mode is off, where the CPI reads
	// no HA state. The lock on the row still holds the wait.
	IgnoreHAState bool

	// notAfter, when set, ends the wait no later than this time on the wait's
	// own clock. MutateVMConfig sets it so its settle waits share the
	// protocol's budget.
	notAfter time.Time
}

func (r VMSettleRequest) motionWait() time.Duration {
	if r.MotionWait <= 0 {
		return DefaultMotionWait
	}
	return r.MotionWait
}

func (r VMSettleRequest) scanFailureBudget() time.Duration {
	if r.ScanFailureBudget <= 0 {
		return settleOtherLockBudget
	}
	return r.ScanFailureBudget
}

func (r VMSettleRequest) label() string {
	if r.Label == "" {
		return "await_vm_settled"
	}
	return r.Label
}

// settleObservation is what one poll of the cluster scan says about a VM.
type settleObservation struct {
	// settled means the VM has no lock and a stable HA state.
	settled bool
	// refusal is set when waiting cannot help, and the wait returns it at once
	// as a retriable error.
	refusal string
	// budget is how long the wait may run, from its start, for what this poll
	// saw.
	budget time.Duration
	// reason says what the VM is waiting on, for the log and the error.
	reason string
}

// Raw HA states the scan can show that the wait treats as settled. The scan's
// hastate falls back to the configured request while the CRM has not
// processed a service, so the request names ignored, disabled, and the old
// enabled appear here too. An error state is settled for the wait's purposes,
// because the CRM takes no action on a service in error, and the callers that
// must clear an error decide that themselves.
var settleStableHAStates = map[string]struct{}{
	"": {}, haRawStarted: {}, haRawStopped: {}, haRawError: {},
	haRequestedIgnored: {}, "disabled": {}, haRequestedEnabled: {},
}

// observeSettle classifies one scan location. It never reads anything itself.
func observeSettle(loc VMLocation, motionWait time.Duration, probeFoundSettles, ignoreHAState bool) settleObservation {
	if !loc.FromScan && probeFoundSettles && loc.FromConfig {
		return settleObservation{settled: true}
	}
	if !loc.FromScan {
		// The index has no row yet, so a config probe found the VM and the
		// lock and HA state are unknown. The index catches up within seconds,
		// so the wait polls again under the shortest budget.
		return settleObservation{
			budget: settleOtherLockBudget,
			reason: fmt.Sprintf("VM is on node %q but the cluster index has no row for it yet", loc.Node),
		}
	}
	lock := strings.ToLower(strings.TrimSpace(loc.Lock))
	haState := strings.ToLower(strings.TrimSpace(loc.HAState))
	if ignoreHAState {
		haState = ""
	}

	switch haState {
	case haRawFence, haRawRecovery:
		// The VM's node is fenced or being fenced. Recovery runs on the scale
		// of fencing, and the method table answers a dead node with a
		// retriable error, so the wait does not hold the call open for it.
		return settleObservation{refusal: fmt.Sprintf("its HA state is %s on node %q", haState, loc.Node)}
	}
	if lock == vmLockSuspended {
		return settleObservation{
			refusal: fmt.Sprintf("it is locked (%s) on node %q, which only resuming the VM clears", lock, loc.Node),
		}
	}

	var budget time.Duration
	var reasons []string
	if lock != "" {
		budget = settleLockBudget(lock, motionWait)
		reasons = append(reasons, fmt.Sprintf("locked (%s)", lock))
	}
	if _, stable := settleStableHAStates[haState]; !stable {
		haBudget := settleOtherLockBudget
		if haState == haRawMigrate || haState == haRawRelocate {
			haBudget = motionWait
		}
		budget = max(budget, haBudget)
		reasons = append(reasons, fmt.Sprintf("HA state %s", haState))
	}
	if len(reasons) == 0 {
		return settleObservation{settled: true}
	}
	return settleObservation{
		budget: budget,
		reason: fmt.Sprintf("%s on node %q", strings.Join(reasons, ", "), loc.Node),
	}
}

// settleLockBudget returns the wait budget for a guest-config lock type.
func settleLockBudget(lock string, motionWait time.Duration) time.Duration {
	switch lock {
	case "migrate":
		return motionWait
	case "backup", "snapshot", "snapshot-delete", "clone":
		return settleTaskLockBudget
	default:
		return settleOtherLockBudget
	}
}

// AwaitVMSettled waits until the VM is safe to act on, which means the
// cluster scan shows no guest-config lock and a stable HA state, and it
// returns the VM's location from the poll that saw it settled. It re-resolves
// the node on every poll through FindVMAuthoritative, so it follows a
// migration to the VM's new node, and a caller must act on the node it
// returns rather than one it held before.
//
// The budget depends on what the VM waits on. A migrate lock, and an HA
// migrate or relocate, get req.MotionWait. A backup, snapshot, or clone lock
// gets 120 seconds, and any other lock or unsettled HA state gets 60 seconds.
// The budget runs from the start of the wait and is the largest that any
// poll has called for, so a migration that hands over to a backup does not
// restart the clock. On a context with a deadline the wait ends a few seconds
// before it, so the total stays inside the method's operation timeout. When
// the budget runs out, AwaitVMSettled returns a retriable error that names
// the lock or state and the node.
//
// Some states return a retriable error at once, because waiting cannot clear
// them on the scale of one call. Those are an HA state of fence or recovery,
// which mean the node is dead or being fenced, and a suspended lock, which
// only resuming the VM clears.
//
// A scan that fails is polled again under the 60-second budget, or under
// req.ScanFailureBudget when that is smaller, and the last failure is returned
// when the budget runs out. A VM that FindVMAuthoritative
// cannot find is returned as Found=false with no error. That answer is not a
// proof of absence, and a caller that needs one runs the absence proof.
//
// A caller whose next step is a MutateVMConfig change may set
// req.ProbeFoundSettles, so a VM that only the config probe found, because the
// index has no row for it yet, ends the wait at once instead of polling for up
// to a minute for the row.
//
// The wait observes one moment. HA can start a migration the instant it
// returns, so it does not make a mutation safe on its own, and every
// VM-config change goes through MutateVMConfig, which handles a migration
// that starts after the wait.
func AwaitVMSettled(ctx context.Context, c Client, logger *log.Logger, req VMSettleRequest) (VMLocation, error) {
	if req.VMID <= 0 {
		return VMLocation{}, cpierrors.Cloud("%s: await VM settled: VM ID %d is not valid", req.label(), req.VMID)
	}
	clk := settleClockFrom(ctx)
	motionWait := req.motionWait()
	start := clk.now()
	var budget time.Duration
	var lastErr error
	reason := ""
	for poll := 0; ; poll++ {
		loc, err := FindVMAuthoritative(ctx, c, req.VMID)
		var obs settleObservation
		switch {
		case err != nil:
			lastErr = err
			obs = settleObservation{budget: req.scanFailureBudget(), reason: "the cluster scan failed"}
		case !loc.Found:
			return loc, nil
		default:
			lastErr = nil
			obs = observeSettle(loc, motionWait, req.ProbeFoundSettles, req.IgnoreHAState)
		}
		if obs.settled {
			if poll > 0 && logger != nil {
				logger.Info("pve: VM settled",
					log.String("op", req.label()),
					log.Int("vmid", req.VMID),
					log.String("node", loc.Node),
					log.Int("waited_ms", int(clk.now().Sub(start)/time.Millisecond)),
				)
			}
			return loc, nil
		}
		if obs.refusal != "" {
			return VMLocation{}, cpierrors.Retriable("%s: PVE VM %d cannot be acted on now: %s",
				req.label(), req.VMID, obs.refusal)
		}
		reason = obs.reason
		budget = max(budget, obs.budget)
		limit := start.Add(budget)
		if dl, ok := ctx.Deadline(); ok && dl.Add(-settleDeadlineReserve).Before(limit) {
			limit = dl.Add(-settleDeadlineReserve)
		}
		if !req.notAfter.IsZero() && req.notAfter.Before(limit) {
			limit = req.notAfter
		}
		now := clk.now()
		if !now.Before(limit) {
			return VMLocation{}, settleExhausted(req, reason, now.Sub(start), lastErr)
		}
		if poll == 0 && logger != nil {
			logger.Info("pve: waiting for VM to settle",
				log.String("op", req.label()),
				log.Int("vmid", req.VMID),
				log.String("reason", reason),
				log.Int("budget_ms", int(limit.Sub(start)/time.Millisecond)),
			)
		}
		d := expBackoffDuration(settlePollBase, poll, settlePollCap)
		if remaining := limit.Sub(now); d > remaining {
			d = remaining
		}
		if sleepErr := clk.sleep(ctx, d); sleepErr != nil {
			return VMLocation{}, cpierrors.WrapAs(sleepErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: wait for PVE VM %d to settle ended (%s)", req.label(), req.VMID, reason))
		}
	}
}

// settleExhausted is the retriable error a wait returns when its budget runs
// out.
func settleExhausted(req VMSettleRequest, reason string, waited time.Duration, lastErr error) error {
	msg := fmt.Sprintf("%s: PVE VM %d did not settle within %s: %s",
		req.label(), req.VMID, waited.Round(time.Second), reason)
	if lastErr != nil {
		return cpierrors.WrapAs(lastErr, cpierrors.TypeRetriableCloud, msg)
	}
	return cpierrors.Retriable("%s", msg)
}
