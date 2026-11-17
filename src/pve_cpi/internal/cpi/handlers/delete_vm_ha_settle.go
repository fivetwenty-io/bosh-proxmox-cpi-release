package handlers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A delete_vm that the BOSH resurrector sends during a node outage defers while
// PVE HA fences and recovers the VM. When the node returns, PVE HA restarts the
// VM, and a resurrector run that scanned the agent while the guest was still
// booting can reach delete_vm a moment later. The HA state then reads started
// on a live node, which on its own lets the delete proceed, so the VM that PVE
// HA has just restarted would be destroyed and rebuilt.
//
// The delete cannot tell a resurrector delete from any other, so it uses its
// own history instead. Each delete_vm that defers an HA-managed VM for fence, a
// dead node, or recovery records when it deferred (see
// recordHARecoveryDeferral). So does each delete_vm that defers because the
// VM's HA state is unknown or the cluster has lost quorum, since PVE HA may be
// recovering the VM on members the CPI cannot see. A later delete_vm that finds
// the service started with a recent deferral recorded lets the VM settle for a
// short window of uptime, so the Director's next scan can find its agent
// answering and keep the VM. A delete of a VM with no recorded deferral never
// waits.
const (
	// haRecoverySettleWindow is how long a VM that PVE HA restarted after a
	// deferred delete may run before the delete proceeds. A BOSH stemcell boots
	// and its agent answers in 30 to 90 seconds, and the resurrector scans again
	// about every two minutes while the problem is open, so five minutes leaves
	// room for a slow boot and two more scans.
	haRecoverySettleWindow = 5 * time.Minute

	// haRecoveryDeferralLife is how long after the last recovery deferral the
	// record still counts. The resurrector defers on every retry while the node
	// is down, so the last deferral comes at most one retry before the node
	// returns, and PVE HA restarts the VM within about a minute after that.
	// Settle deferrals never refresh the record, so this is also the hard bound
	// on how late a delete can still wait for a VM to settle.
	haRecoveryDeferralLife = 15 * time.Minute
)

// haRawStateStarted is the raw CRM state of a service that PVE HA holds
// running.
const haRawStateStarted = "started"

// haRecoveryCaller is the only caller whose deferrals are recorded and whose
// deletes settle. The create_vm rollback and the deferred-teardown sweep never
// read or write the record.
const haRecoveryCaller = "delete_vm"

// recordHARecoveryDeferral records that a delete_vm deferred a VM that PVE HA
// may be recovering. That is an HA-managed VM in fence, on a dead node, or in
// recovery, which are the dispositions defer and grace_window, and any VM
// whose HA state is unknown, unless a known state proves PVE HA does not
// manage it. An unknown state can hide a recovery that the next read finds
// finished, so it is recorded as a recovery is. A record for a VM that turns
// out not to be HA-managed changes nothing, because only a managed service
// ever waits on it. A failure to record is logged, and the deferral it
// belongs to stands unchanged.
func recordHARecoveryDeferral(
	deps Deps, caller string, st *pve.HAServiceState, disp pve.HADisposition, logger *log.Logger,
) {
	if st == nil {
		return
	}
	switch disp {
	case pve.HADefer, pve.HAGraceWindow:
		if !st.Managed() {
			return
		}
	case pve.HAUnknown:
		if st.Known && !st.Managed() {
			return
		}
	default:
		return
	}
	recordHARecoveryDeferralOf(deps, caller, st.SID, logger)
}

// recordHARecoveryDeferralOf records a delete_vm deferral of the HA service sid
// without reading its HA state. The quorum gate uses it, because the cluster
// that lost quorum cannot say whether PVE HA manages the VM, and the majority
// side may be recovering it. Only delete_vm records.
func recordHARecoveryDeferralOf(deps Deps, caller, sid string, logger *log.Logger) {
	if caller != haRecoveryCaller {
		return
	}
	if err := haEpisodes(deps, logger).RecordRecoveryDeferral(sid); err != nil && logger != nil {
		logger.Warn(caller+": the HA recovery deferral could not be recorded, so a delete that reaches the VM "+
			"right after PVE HA restarts it does not wait for it to settle",
			log.String("sid", sid), log.Err(err))
	}
}

// awaitHARecoverySettle decides whether a delete_vm that the HA table lets
// proceed waits for a VM that PVE HA has just restarted after this CPI deferred
// its delete. It returns nil when the delete proceeds and a retriable error
// when it waits.
//
// The delete waits only when all of these hold: the caller is delete_vm, the
// delete is not the one the dead-node completion runs after moving a stopped
// config (which clears the deferral and proceeds), PVE HA holds the service started, the record holds a deferral of the
// service younger than haRecoveryDeferralLife, and the VM has run for less than
// haRecoverySettleWindow or is not running yet. A record that cannot be read,
// a stale deferral, a VM that HA does not hold started, and a running VM whose
// uptime cannot be read all proceed, and every outcome that proceeds after
// finding a deferral clears it. A power state that cannot be read waits,
// because the stop and the destroy that follow need the same API, and the
// deferral's life still bounds the wait.
func awaitHARecoverySettle(
	ctx context.Context, deps Deps, caller string, st *pve.HAServiceState, logger *log.Logger,
) error {
	if caller != haRecoveryCaller || st == nil || !st.Managed() {
		return nil
	}
	store := haEpisodes(deps, logger)
	if deadNodeCompletionDone(ctx) {
		// The completion requested the VM stopped and moved its config, so
		// there is no restart to wait for, and the deferral that led here is
		// spent.
		if err := store.ClearRecoveryDeferral(st.SID); err != nil {
			logger.Warn(caller+": the HA recovery deferral could not be cleared; it expires on its own",
				log.String("sid", st.SID), log.Err(err))
		}
		return nil
	}
	deferral, held, err := store.LastRecoveryDeferral(st.SID)
	if err != nil {
		logger.Warn(caller+": the HA recovery deferral record is unreadable, so the delete proceeds without "+
			"checking whether PVE HA has just restarted the VM", log.String("sid", st.SID), log.Err(err))
		return nil
	}
	if !held {
		return nil
	}
	age := deferral.Age()
	fields := []log.Field{
		log.String("sid", st.SID), log.String("raw_state", st.Raw), log.String("ha_node", st.Node),
		log.String("deferred_ago", age.Round(time.Second).String()),
		log.String("window", haRecoverySettleWindow.String()),
	}
	release := func(msg string, extra ...log.Field) error {
		logger.Info(caller+": "+msg, append(fields, extra...)...)
		if err := store.ClearRecoveryDeferral(st.SID); err != nil {
			logger.Warn(caller+": the HA recovery deferral could not be cleared; it expires on its own",
				log.String("sid", st.SID), log.Err(err))
		}
		return nil
	}
	if age >= haRecoveryDeferralLife {
		return release("the last HA recovery deferral is too old to hold the delete; proceeding")
	}
	if st.Raw != haRawStateStarted {
		return release("PVE HA does not hold the VM started, so there is no restart to wait for; proceeding")
	}
	limit := haRecoveryDeferralLife - age

	status, err := deps.PVE.QEMU().Status(ctx, st.Node, st.VMID)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d: the CPI deferred this VM's delete %s ago while PVE HA recovered it, and the power state "+
				"of the VM on node %s could not be read to tell whether PVE HA has just restarted it; the next "+
				"retry reads it again, and a retry %s from now proceeds whatever it reads",
			caller, st.VMID, age.Round(time.Second), st.Node, limit.Round(time.Second)))
	}
	power, _ := pve.ConfigString(status, "status")
	if power != vmPowerStateRunning {
		logger.Info(caller+": PVE HA holds the VM started but it is not running yet; the delete waits for it",
			append(fields, log.String("power", power))...)
		return cpierrors.Retriable(
			"%s: VM %d deferred: the CPI deferred its delete %s ago while PVE HA recovered it, and PVE HA now "+
				"holds it started on node %s but the VM is not running yet. The delete waits so PVE HA can start "+
				"the VM and its agent can answer the Director's next scan. A retry proceeds once the VM has run "+
				"for %s, and %s from now at the latest",
			caller, st.VMID, age.Round(time.Second), st.Node, haRecoverySettleWindow, limit.Round(time.Second))
	}
	uptime, ok := statusUptime(status)
	if !ok {
		logger.Warn(caller+": the running VM's status carries no readable uptime, so the delete proceeds "+
			"without waiting for it to settle", fields...)
		return release("the HA recovery deferral no longer holds the delete; proceeding")
	}
	if uptime >= haRecoverySettleWindow {
		return release("the VM has run past the settle window since PVE HA restarted it; proceeding",
			log.String("uptime", uptime.Round(time.Second).String()))
	}
	wait := min(haRecoverySettleWindow-uptime, limit)
	logger.Info(caller+": PVE HA restarted the VM after a deferred delete; the delete waits for it to settle",
		append(fields, log.String("uptime", uptime.Round(time.Second).String()),
			log.String("remaining", wait.Round(time.Second).String()))...)
	return cpierrors.Retriable(
		"%s: VM %d deferred: PVE HA restarted it on node %s %s ago, after the CPI deferred its delete %s ago "+
			"while PVE HA recovered it. The delete waits until the VM has run for %s, so the Director's next scan "+
			"can find its agent answering and keep the VM. A retry proceeds %s from now, and if the agent never "+
			"answers, the VM is replaced then",
		caller, st.VMID, st.Node, uptime.Round(time.Second), age.Round(time.Second), haRecoverySettleWindow,
		wait.Round(time.Second))
}

// statusUptimeCap is the longest uptime statusUptime reports. Any uptime past
// the settle window decides the same way, so the cap only keeps the conversion
// to a duration from overflowing.
const statusUptimeCap = 100 * 365 * 24 * time.Hour

// statusUptime reads the uptime that status/current reports for a running VM,
// in seconds. PVE sends it as a number, and the Perl wire format can send it
// as a string, so both are read. A value that is not a finite, non-negative
// number reads as missing.
func statusUptime(status map[string]any) (time.Duration, bool) {
	raw, ok := pve.ConfigString(status, "uptime")
	if !ok {
		return 0, false
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(secs) || secs < 0 {
		return 0, false
	}
	if secs >= statusUptimeCap.Seconds() {
		return statusUptimeCap, true
	}
	return time.Duration(secs * float64(time.Second)), true
}
