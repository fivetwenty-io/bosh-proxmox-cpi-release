package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// PVE HA for parkers.
//
// A parker whose disks are all on shared storage that at least two placeable
// nodes reach is registered with PVE HA as stopped, so its config follows a
// failover of its node and the disks it holds stay reachable. A parker never
// runs, so HA can never start a second writer of its disks. The registration
// follows the staging order of a VM: the parker's resource is staged as
// ignored, its pin is written, the pin is verified, and only then does the
// resource request stopped. A parker whose access set is narrower than the
// configured node set gets a strict pin on that set, so PVE HA never recovers
// it onto a node where one of its disks is missing. The parker selection rule
// (parker_selection.go) keeps every later park from narrowing that set.
//
// When the last disk leaves a registered parker, the parker is destroyed
// through the teardown protocol with purge=1, which removes its HA resource and
// its rule memberships with it. An unregistered parker stays when it empties,
// as it always has.
//
// The work runs after a park, in the parker pool sweep of the park's node, and
// it is best-effort like the rest of that sweep: a park that has landed is
// never taken back for it, and anything it could not finish waits for the next
// sweep on that node. Every change to one parker runs under that parker's VMID
// claim, which is the lock its protection window takes too, so no park can
// attach a disk while the registration reads the parker or while the
// retirement destroys it. Off mode, and a cluster where the CPI may not write
// HA state, change nothing here.

// parkerHAReconcileTimeout bounds the HA pass of one parker pool sweep, so a
// slow cluster never holds up the request whose park has already landed.
const parkerHAReconcileTimeout = 2 * time.Minute

// parkerHAStateIgnored and parkerHAStateStopped are the requested states a
// parker's HA resource passes through.
const (
	parkerHAStateIgnored = "ignored"
	parkerHAStateStopped = haRequestStopped
)

// parkerRetireCaller names the parker retirement in the teardown's errors and
// logs.
const parkerRetireCaller = "parker retirement"

// reconcileParkerHA registers, finishes the staging of, or retires each parker
// of this Director on node, as the package comment above describes. It never
// returns an error, because the park it follows has already landed; every
// failure is logged and left to the next sweep.
func reconcileParkerHA(ctx context.Context, deps Deps, node string, cfg pve.ParkerConfig) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return
	}
	logger := deps.Log(ctx)
	if strings.TrimSpace(deps.RequestDirectorUUID) == "" {
		logger.Debug("parker HA: the request carries no director_uuid to mark a parker's HA resource with, so the "+
			"parkers stay as they are", log.String("node", node))
		return
	}
	client := unguardedPVE(deps.PVE)
	if client == nil || client.Cluster() == nil {
		return
	}
	udeps := deps
	udeps.PVE = client

	rctx, cancel := context.WithTimeout(ctx, parkerHAReconcileTimeout)
	defer cancel()
	// The pass takes claims of its own, one parker at a time. A claim set
	// the park carried would let withVMIDClaim reuse a claim the park holds,
	// so the pass drops it.
	rctx = context.WithValue(rctx, vmidClaimSetKey{}, (*vmidClaimSet)(nil))

	parkers, err := pve.ListParkersForNode(rctx, client, node, cfg)
	if err != nil {
		logger.Info("parker HA: the parkers of the node could not be listed; the next sweep tries again",
			log.String("node", node), log.Err(err))
		return
	}
	if len(parkers) == 0 {
		return
	}
	profile, err := resolveHAProfile(rctx, udeps)
	if err != nil {
		logger.Info("parker HA: the HA profile could not be resolved; the parkers stay as they are until the next "+
			"sweep", log.String("node", node), log.Err(err))
		return
	}
	if !profile.WritesAllowed() {
		logger.Debug("parker HA: the CPI may not write HA state on this cluster, so the parkers stay as they are",
			log.String("node", node), log.String("reason", profile.Reason))
		return
	}
	r := &parkerHA{
		deps: udeps, logger: logger, node: node, cfg: cfg, profile: profile,
		owner:   deps.RequestDirectorUUID,
		managed: deps.Config.HAModeValue() == config.HAModeManaged,
	}
	for _, vmid := range parkers {
		if rctx.Err() != nil {
			logger.Info("parker HA: the pass ran out of its time budget; the remaining parkers wait for the next "+
				"sweep", log.String("node", node))
			return
		}
		if err := r.reconcile(rctx, vmid); err != nil {
			logger.Info("parker HA: the parker's HA registration did not change; the next sweep tries again",
				log.Int("parker_vmid", vmid), log.String("node", node), log.Err(err))
		}
	}
}

// parkerHA carries one HA pass over the parkers of a node. deps.PVE is the
// client no managed allocation guard wraps.
type parkerHA struct {
	deps    Deps
	logger  *log.Logger
	node    string
	cfg     pve.ParkerConfig
	profile *ha.Profile
	owner   string
	managed bool
}

// parkerHAAction is what one parker needs from the pass.
type parkerHAAction int

const (
	parkerHANothing parkerHAAction = iota
	parkerHARegister
	parkerHAFinishStaging
	parkerHARetire
)

// parkerSnapshot is what the pass reads of one parker before it acts.
type parkerSnapshot struct {
	resource *cluster.GetHaResourcesResponse
	views    pve.QemuViews
	gone     bool
}

// reconcile brings one parker's HA registration in line with what it holds.
// It decides from reads that take no lock first, so a parker that needs
// nothing costs no claim, and it reads again under the parker's claim before
// it changes anything.
func (r *parkerHA) reconcile(ctx context.Context, vmid int) error {
	snap, err := r.read(ctx, vmid)
	if err != nil {
		return err
	}
	if r.action(vmid, snap) == parkerHANothing {
		return nil
	}
	claimErr := withVMIDClaim(ctx, r.deps, vmid, "parker-ha/"+strconv.Itoa(vmid), claimForSweep, r.logger,
		func(claimCtx context.Context) error {
			snap, err := r.read(claimCtx, vmid)
			if err != nil {
				return err
			}
			switch r.action(vmid, snap) {
			case parkerHARegister, parkerHAFinishStaging:
				return r.register(claimCtx, vmid, snap)
			case parkerHARetire:
				return r.retire(claimCtx, vmid)
			default:
				return nil
			}
		})
	if claimErr != nil && isVMIDClaimHeld(claimErr) {
		r.logger.Debug("parker HA: a park or another pass holds the parker's claim; the next sweep tries again",
			log.Int("parker_vmid", vmid), log.String("node", r.node))
		return nil
	}
	return claimErr
}

// read reads the parker's HA resource and both views of its config on the
// pass's node. A parker whose config left the node, because HA moved it or
// it was destroyed, reads as gone, and the sweep of the node it is on now
// handles it.
func (r *parkerHA) read(ctx context.Context, vmid int) (parkerSnapshot, error) {
	sid := haResourceSid(vmid)
	res, err := r.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		res = nil
	case err != nil:
		return parkerSnapshot{}, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("parker HA: read the HA resource of parker vmid %d", vmid))
	case res == nil:
		return parkerSnapshot{}, cpierrors.Retriable("parker HA: the read of the HA resource of parker vmid %d "+
			"returned nothing", vmid)
	}
	views, err := pve.ReadQemuViews(ctx, r.deps.PVE, r.node, vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return parkerSnapshot{resource: res, gone: true}, nil
		}
		return parkerSnapshot{}, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("parker HA: read the config of parker vmid %d on node %s", vmid, r.node))
	}
	return parkerSnapshot{resource: res, views: views}, nil
}

// action decides what the parker needs. Only a parker that is still this
// Director's, by its tags and its name, is ever changed, and only an HA
// resource that carries this Director's marker counts as the CPI's own.
func (r *parkerHA) action(vmid int, snap parkerSnapshot) parkerHAAction {
	if snap.gone || !r.ownsParker(vmid, snap.views.Holding()) {
		return parkerHANothing
	}
	empty := parkerEmpty(snap.views)
	if snap.resource == nil {
		if empty {
			return parkerHANothing
		}
		return parkerHARegister
	}
	if haOwnershipOf(resourceComment(snap.resource), r.owner) != haOwnedByUs {
		return parkerHANothing
	}
	if empty {
		return parkerHARetire
	}
	if resourceState(snap.resource) == parkerHAStateIgnored {
		return parkerHAFinishStaging
	}
	return parkerHANothing
}

// ownsParker reports whether the config names a parker of this Director and
// prefix in the band.
func (r *parkerHA) ownsParker(vmid int, holding map[string]any) bool {
	tags, _ := pve.ConfigString(holding, "tags")
	name, _ := pve.ConfigString(holding, "name")
	return pve.ParkerOwnedBy(vmid, tags, name, r.cfg)
}

// parkerEmpty reports whether a parker holds nothing: no volume on any disk
// key in either config view, and no provenance record, finished or not, so no
// transfer is still moving a disk in.
func parkerEmpty(views pve.QemuViews) bool {
	holding := views.Holding()
	return len(pve.ConfigAccessVolumes(holding, "")) == 0 &&
		len(pve.ConfigAccessVolumes(views.PendingReplacements(), "")) == 0 &&
		!pve.ParkerHasRecords(holding)
}

// register runs the staging order for a parker that holds disks: it decides,
// stages the resource as ignored, writes and verifies the pin, and requests
// stopped. A parker the pass staged earlier resumes at the same order. Any
// step that fails leaves the resource staged as ignored, which the CRM never
// acts on, and the next sweep finishes it.
func (r *parkerHA) register(ctx context.Context, vmid int, snap parkerSnapshot) error {
	vols, err := r.volumes(ctx, vmid, snap.views)
	if err != nil {
		return err
	}
	d, err := r.profile.DecideVM(ha.VMInput{Config: r.deps.Config, VMID: vmid, Node: r.node, Volumes: vols})
	if err != nil {
		return err
	}
	staged := snap.resource != nil
	if !d.Registers() || d.Policy != ha.PolicyShared {
		return r.refuse(ctx, vmid, d, staged)
	}
	stager := newHAStager(r.deps, r.owner, r.profile, r.managed, r.logger)
	if err := stager.CheckWatchdog(ctx, d, r.node); err != nil {
		if errors.Is(err, ha.ErrWatchdogNotArmed) {
			r.logger.Info("parker HA: the parker stays as it is, because registering it could arm the HA watchdog of "+
				"a node while the cluster is one failure from losing quorum", log.Int("parker_vmid", vmid),
				log.String("node", r.node), log.Err(err))
			return nil
		}
		return err
	}
	if err := stager.Stage(ctx, d, haIdentity{Owner: r.owner}.marker()); err != nil {
		return err
	}
	storages := ha.VolumeStorages(vols)
	pin, err := writeMergedPinRule(ctx, r.deps, r.profile, vmid, pinRequest{Decision: d, Storages: storages}, r.logger)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("parker HA: write the pin of parker vmid %d; it stays staged as "+
			"ignored", vmid))
	}
	if pin == nil {
		return cpierrors.Retriable("parker HA: the pin of parker vmid %d was not planned; it stays staged as ignored",
			vmid)
	}
	if err := stager.Verify(ctx, d, pin, ""); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("parker HA: verify the pin of parker vmid %d; it stays staged as "+
			"ignored", vmid))
	}
	if err := r.requestStopped(ctx, d.SID); err != nil {
		return err
	}
	r.logger.Info("parker HA: the parker is registered with PVE HA as stopped, so its config follows a failover "+
		"of its node", log.Int("parker_vmid", vmid), log.String("node", r.node),
		log.String("nodes", strings.Join(d.Admissible, ",")), log.Bool("strict_pin", pin.Strict))
	return nil
}

// refuse handles a decision that does not register the parker with shared
// HA. An unregistered parker stays unregistered. A staged one stays staged
// while the refusal may pass on its own, such as a node that is down or a
// storage read that failed, and is unstaged, with its pin removed, only when
// the refusal rests on proven, permanent facts.
func (r *parkerHA) refuse(ctx context.Context, vmid int, d *ha.VMDecision, staged bool) error {
	reason := d.Reason
	if f := d.Failure(); f != nil {
		reason = f.Error()
	}
	if !staged {
		r.logger.Debug("parker HA: the parker stays unregistered", log.Int("parker_vmid", vmid),
			log.String("node", r.node), log.String("reason", reason))
		return nil
	}
	if d.Transient || d.PlaceableShortfall {
		return cpierrors.Retriable("parker HA: parker vmid %d stays staged as ignored, because its HA decision "+
			"refused for a reason that may pass: %s", vmid, reason)
	}
	stager := newHAStager(r.deps, r.owner, r.profile, r.managed, r.logger)
	d.MarkStaged()
	if err := stager.Unstage(ctx, d, reason); err != nil {
		return err
	}
	if err := deleteNodeAffinityRule(ctx, r.deps, vmid, r.logger, nil); err != nil {
		r.logger.Warn("parker HA: the pin of the unstaged parker could not be removed; it names no resource now",
			log.Int("parker_vmid", vmid), log.Err(err))
	}
	return nil
}

// volumes lists every volume the parker holds or is receiving for its HA
// decision: the volumes of both config views, the volumes of any snapshot,
// and the volumes its provenance records name, which counts a disk an
// unfinished transfer is still moving in. Every volume but an ISO counts as a
// disk.
func (r *parkerHA) volumes(ctx context.Context, vmid int, views pve.QemuViews) ([]ha.VMVolume, error) {
	holding := views.Holding()
	out := haVolumesOfConfig(holding, r.node, nil)
	seen := map[string]bool{}
	for _, v := range out {
		seen[v.Volume] = true
	}
	add := func(volume string) {
		volume = strings.TrimSpace(volume)
		if volume == "" || seen[volume] {
			return
		}
		seen[volume] = true
		out = append(out, ha.VMVolume{Volume: volume, HomeNode: r.node, Kind: ha.VolumeDisk})
	}
	for _, v := range haVolumesOfConfig(views.PendingReplacements(), r.node, nil) {
		add(v.Volume)
	}
	all, err := pve.VMAccessVolumes(ctx, r.deps.PVE, r.node, vmid, holding)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker HA: read the snapshot volumes of parker vmid %d", vmid))
	}
	for _, v := range all {
		add(v.Volume)
	}
	for _, v := range pve.ParkerRecordedVolumes(holding) {
		add(v)
	}
	slices.SortFunc(out, func(a, b ha.VMVolume) int { return strings.Compare(a.Volume, b.Volume) })
	return out, nil
}

// requestStopped takes the staged resource out of staging with one request
// for stopped and reads it back. A write that failed counts as done only when
// the read shows the resource requesting stopped.
func (r *parkerHA) requestStopped(ctx context.Context, sid string) error {
	svc := r.deps.PVE.Cluster()
	stopped := parkerHAStateStopped
	writeErr := svc.UpdateHaResources(ctx, sid, &cluster.UpdateHaResourcesParams{State: &stopped})
	got, readErr := svc.GetHaResources(ctx, sid)
	switch {
	case readErr != nil && writeErr != nil:
		return cpierrors.WrapAs(errors.Join(writeErr, readErr), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker HA: request stopped for %s; it may still be staged as ignored", sid))
	case readErr != nil:
		return cpierrors.WrapAs(pve.WrapError(readErr), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker HA: read %s back after requesting stopped", sid))
	case got == nil:
		return cpierrors.Retriable("parker HA: the read of %s after requesting stopped returned nothing", sid)
	case resourceState(got) == parkerHAStateStopped:
		return nil
	case writeErr != nil:
		return cpierrors.WrapAs(pve.WrapError(writeErr), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker HA: request stopped for %s; it is still staged as %s", sid, resourceState(got)))
	}
	return cpierrors.Retriable("parker HA: %s requests %q after the request for stopped", sid, resourceState(got))
}

// retire destroys an empty parker of this Director that PVE HA manages,
// through the teardown protocol, whose destroy passes purge=1 and so removes
// the parker's HA resource and its rule memberships. The teardown reads the
// HA state once and defers on anything but a state that lets the destroy
// proceed on this node. Its preserve and ownership steps prove the parker
// still empty and still this Director's right before the destroy, and the
// ownership step clears the parker's protection last, because PVE refuses to
// destroy a protected VM. A teardown that fails after that sets protection
// again.
func (r *parkerHA) retire(ctx context.Context, vmid int) error {
	logger := r.logger.With(log.Int("parker_vmid", vmid), log.String("node", r.node))
	cleared := false
	teardown := r.retireTeardown(vmid, logger, &cleared)
	result, err := teardown.run(ctx, r.deps)
	if err != nil {
		if cleared {
			r.restoreProtection(ctx, vmid, logger)
		}
		return cpierrors.Wrap(err, fmt.Sprintf("parker HA: retire empty parker vmid %d (stage %s)", vmid,
			result.stage))
	}
	if !result.gone {
		if ruleErr := deleteNodeAffinityRule(ctx, r.deps, vmid, logger, nil); ruleErr != nil {
			logger.Warn("parker HA: the pin of the retired parker could not be removed; it names no resource now",
				log.Err(ruleErr))
		}
	}
	logger.Info("parker HA: the last disk left the parker, so it was destroyed with its HA resource")
	return nil
}

// retireTeardown builds the teardown of an empty parker. cleared reports
// whether the ownership step cleared the parker's protection.
func (r *parkerHA) retireTeardown(vmid int, logger *log.Logger, cleared *bool) *vmTeardown {
	return &vmTeardown{
		caller: parkerRetireCaller,
		vmCID:  strconv.Itoa(vmid),
		vmid:   vmid,
		node:   r.node,
		logger: logger,
		gate:   teardownGateNoWait,
		// An empty parker has nothing to sweep, and a sweep could free a
		// volume named for the parker that a crashed transfer left behind.
		destroyUnreferenced: false,
		stop: func(ctx context.Context) (bool, error) {
			return r.retireStopCheck(ctx, vmid)
		},
		preserve: func(ctx context.Context) (bool, error) {
			views, err := pve.ReadQemuViews(ctx, r.deps.PVE, r.node, vmid)
			if err != nil {
				return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
					"%s: read the config of parker vmid %d", parkerRetireCaller, vmid))
			}
			if !parkerEmpty(views) {
				return false, cpierrors.Retriable("%s: parker vmid %d holds a disk again, so it stays",
					parkerRetireCaller, vmid)
			}
			return false, nil
		},
		owned: func(ctx context.Context, holding pve.QemuHolding) error {
			return r.retireOwned(ctx, vmid, holding, logger, cleared)
		},
	}
}

// retireStopCheck is the teardown's stop step for a parker PVE HA does not
// manage at that moment. A parker never runs, so the step sends no stop: a
// stopped parker goes on to the destroy, and a parker that runs is left
// alone, because something outside the CPI started it.
func (r *parkerHA) retireStopCheck(ctx context.Context, vmid int) (bool, error) {
	status, err := r.deps.PVE.QEMU().Status(ctx, r.node, vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			goneErr := confirmVMGone(ctx, r.deps, parkerRetireCaller, vmid, r.node, "the power state read")
			if goneErr != nil {
				return false, goneErr
			}
			return true, nil
		}
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read the power state of parker vmid %d", parkerRetireCaller, vmid))
	}
	if state, _ := pve.ConfigString(status, "status"); state != vmPowerStateStopped {
		return false, cpierrors.Cloud("%s: parker vmid %d on node %s is %s, and a parker never runs, so the CPI "+
			"leaves it for an operator to inspect", parkerRetireCaller, vmid, r.node, orUnsetState(state))
	}
	return false, nil
}

// retireOwned is the teardown's ownership step. It proves, on both config
// views the teardown read and on a fresh read of the HA resource, that the
// VM is still an empty parker of this Director whose HA resource carries this
// Director's marker, and then clears the parker's protection as the last
// change before the destroy.
func (r *parkerHA) retireOwned(
	ctx context.Context, vmid int, holding pve.QemuHolding, logger *log.Logger, cleared *bool,
) error {
	if !r.ownsParker(vmid, holding.Config) {
		return cpierrors.Cloud("%s: VM %d on node %s is no longer a parker of this Director, so it stays",
			parkerRetireCaller, vmid, r.node)
	}
	if len(pve.ConfigAccessVolumes(holding.Config, "")) > 0 || len(pve.ConfigAccessVolumes(holding.Replaced, "")) > 0 ||
		pve.ParkerHasRecords(holding.Config) {
		return cpierrors.Retriable("%s: parker vmid %d holds a disk again, so it stays", parkerRetireCaller, vmid)
	}
	snaps, err := pve.HasSnapshots(ctx, r.deps.PVE, r.node, vmid)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: list the snapshots of parker vmid %d", parkerRetireCaller, vmid))
	}
	if len(snaps) > 0 {
		return cpierrors.Cloud("%s: parker vmid %d carries snapshots %s, whose volumes the destroy would free, so "+
			"it stays for an operator to inspect", parkerRetireCaller, vmid, strings.Join(snaps, ","))
	}
	res, err := r.deps.PVE.Cluster().GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return cpierrors.Retriable("%s: the HA resource of parker vmid %d is gone, so the parker stays",
			parkerRetireCaller, vmid)
	case err != nil:
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read the HA resource of parker vmid %d", parkerRetireCaller, vmid))
	case res == nil || haOwnershipOf(resourceComment(res), r.owner) != haOwnedByUs:
		return cpierrors.Retriable("%s: the HA resource of parker vmid %d does not carry this Director's marker, so "+
			"the parker stays", parkerRetireCaller, vmid)
	}
	if err := pve.SetParkerProtection(ctx, r.deps.PVE, logger, r.node, vmid, false); err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: clear the protection of parker vmid %d", parkerRetireCaller, vmid))
	}
	*cleared = true
	return nil
}

// restoreProtection sets the parker's protection again after a retirement
// that cleared it and then failed. A parker that is gone needs none.
func (r *parkerHA) restoreProtection(ctx context.Context, vmid int, logger *log.Logger) {
	if err := pve.SetParkerProtection(ctx, r.deps.PVE, logger, r.node, vmid, true); err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return
		}
		logger.Warn("parker HA: the retirement failed and the parker's protection could not be set again; the next "+
			"park into it sets it", log.Err(err))
	}
}

// resourceComment returns the comment of an HA resource read, or "".
func resourceComment(res *cluster.GetHaResourcesResponse) string {
	if res == nil || res.Comment == nil {
		return ""
	}
	return *res.Comment
}

// resourceState returns the requested state of an HA resource read, or "".
func resourceState(res *cluster.GetHaResourcesResponse) string {
	if res == nil || res.State == nil {
		return ""
	}
	return strings.TrimSpace(*res.State)
}

// orUnsetState names an empty power state.
func orUnsetState(state string) string {
	if strings.TrimSpace(state) == "" {
		return "in an unknown power state"
	}
	return state
}
