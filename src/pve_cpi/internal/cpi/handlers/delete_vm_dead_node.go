package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// The dead-node delete completion. A legacy delete of an HA-managed VM whose
// node PVE has fenced cannot run through the teardown, because every call
// about the VM is answered by its node, and that node is gone. PVE's proof
// that the VM stopped is the fence itself: the CRM holds the service in
// recovery on a node it reports unknown. Once the recovery grace window has
// expired without PVE HA recovering the VM, the completion requests stopped,
// narrows the VM's node-affinity pin to the live nodes of the VM's hard
// admissible set, which its pin records, removes the VM from its
// anti-affinity rules, and waits for the CRM's recovery to move the config
// onto one of those nodes. The delete then runs again against that node,
// through the same gate, ownership guard, and teardown as any other delete,
// and the destroy removes the pin with the HA resource. Before anything is
// stopped or destroyed there, the delete checks every volume the config names
// against the node it now runs on.
//
// The completion never removes the pin, because the CRM's recovery may then
// pick any node, including one that lacks the VM's volumes. Every refusal and
// every unfinished step is a retriable HA deferral, so the Director retries
// and the next call resumes where this one stopped: each write is idempotent
// and each one is checked against a fresh read first.

// deadNodeDelete is the caller name the completion's errors and logs carry.
const deadNodeDelete = "delete_vm"

// haRawStateRecovery is the raw CRM state of a service whose node was fenced.
const haRawStateRecovery = "recovery"

// haRequestStopped is the requested HA state the completion sends.
const haRequestStopped = "stopped"

// deadNodeCompletionKey marks a context whose delete already ran the
// completion, so the delete it runs again never starts a second one.
type deadNodeCompletionKey struct{}

func withDeadNodeCompletionDone(ctx context.Context) context.Context {
	return context.WithValue(ctx, deadNodeCompletionKey{}, true)
}

func deadNodeCompletionDone(ctx context.Context) bool {
	done, _ := ctx.Value(deadNodeCompletionKey{}).(bool)
	return done
}

// deadNodeCompletion is one run of the completion for one VM.
type deadNodeCompletion struct {
	deps   Deps
	vmCID  string
	cid    vmcid.VMCID
	vmid   int
	sid    string
	loc    pve.VMLocation
	logger *log.Logger
	// gateErr is the gate's deferral, returned unchanged whenever the
	// evidence does not prove a fenced node with an expired grace window.
	gateErr error

	source string
	// uid is the CRM's uid of the recovery episode the first read proved, so
	// the proof under the rules lock can tell a new episode apart.
	uid     string
	owner   string
	profile *ha.Profile
	reach   ha.DeadNodeReach
	// storages is the storage index the deletion destinations read the reach
	// of the VM's recorded storages from, read once per completion.
	storages ha.StorageIndex
}

// completeDeadNodeDelete runs the completion for the legacy delete of the VM
// the scan located at loc, whose HA gate answered gateErr. It returns gateErr
// unchanged unless gateErr is an HA deferral, the scan shows the service in
// recovery, and a fresh read proves the fence and an expired grace window.
// Past that point every answer is the completion's own: a retriable deferral
// that says what blocks it, or the result of the delete that runs again once
// the CRM has moved the config onto a deletion destination.
func completeDeadNodeDelete(
	ctx context.Context, deps Deps, vmCID string, cid vmcid.VMCID, loc pve.VMLocation, gateErr error,
	logger *log.Logger,
) (any, error) {
	if gateErr == nil || !isWholeHADeferral(gateErr) || deadNodeCompletionDone(ctx) ||
		loc.HAState != haRawStateRecovery || pve.TagsMarkParker(loc.Tags) || deps.PVE == nil {
		return nil, gateErr
	}
	c := &deadNodeCompletion{
		deps: deps, vmCID: vmCID, cid: cid, vmid: cid.VMID, sid: haResourceSid(cid.VMID), loc: loc,
		logger: logger, gateErr: gateErr,
	}
	proven, err := c.proveExpiredFence(ctx)
	if err != nil || !proven {
		return nil, gateErr
	}
	if err := c.checkPreconditions(ctx); err != nil {
		return nil, err
	}
	if err := c.requestStopped(ctx); err != nil {
		return nil, err
	}
	dests, err := c.narrowRules(ctx)
	if err != nil {
		return nil, err
	}
	node, err := c.awaitMove(ctx, dests)
	if err != nil {
		return nil, err
	}
	c.logger.Info(deadNodeDelete+": the CRM moved the fenced VM's config onto a deletion destination; "+
		"running the delete there", log.String("sid", c.sid), log.String("dead_node", c.source),
		log.String("node", node))
	return deleteAfterDeadNodeMove(ctx, deps, vmCID, cid)
}

// deleteAfterDeadNodeMove runs the delete again once the CRM has moved the
// config onto a deletion destination. With an allocation journal configured
// the delete first runs the journal's check again, which now reads the config
// on the destination: a VM whose description carries an allocation marker
// without a journal record is refused there, before anything is destroyed. The
// context marks the completion as done, so the delete it runs never starts a
// second one.
func deleteAfterDeadNodeMove(ctx context.Context, deps Deps, vmCID string, cid vmcid.VMCID) (any, error) {
	done := withDeadNodeCompletionDone(ctx)
	handled, err := deleteManagedVMIfRecorded(done, deps, vmcid.PathOf(cid.VMID), cid.VMID)
	if handled || err != nil {
		return nil, managedDeleteVMError(err)
	}
	return deleteLegacyVMClaimed(done, deps, vmCID, cid)
}

// deferf is a retriable HA deferral that names the VM and the dead node.
func (c *deadNodeCompletion) deferf(format string, args ...any) error {
	prefix := fmt.Sprintf("%s: VM %d on dead node %s: ", deadNodeDelete, c.vmid, c.source)
	return deferHA(cpierrors.Retriable(prefix+format, args...))
}

// deferWrap is deferf for a failure that has an error of its own.
func (c *deadNodeCompletion) deferWrap(err error, format string, args ...any) error {
	prefix := fmt.Sprintf("%s: VM %d on dead node %s: ", deadNodeDelete, c.vmid, c.source)
	return deferHA(cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, prefix+fmt.Sprintf(format, args...)))
}

// readFenceEvidence reads node health and the service's HA state from one
// view, and returns why that view does not prove the service is in recovery on
// a fenced node, or "" when it does. A single failed call is never the proof:
// the proof is a fresh structured CRM view from a quorate entry node that holds
// the service in recovery on a node the CRM reports unknown and cluster status
// reports offline.
func (c *deadNodeCompletion) readFenceEvidence(
	ctx context.Context,
) (*pve.NodeHealthReport, *pve.HAServiceState, string, error) {
	clusterAPI, nodesAPI := c.deps.PVE.Cluster(), c.deps.PVE.Nodes()
	if clusterAPI == nil || nodesAPI == nil {
		return nil, nil, "", errors.New("the PVE client has no cluster or nodes service")
	}
	report, err := pve.NodeHealth(ctx, clusterAPI, nodesAPI, haHealthOptions(c.deps))
	if err != nil {
		return nil, nil, "", err
	}
	st, err := pve.ReadHAServiceState(ctx, clusterAPI, nodesAPI, pve.HAServiceStateInput{
		VMID: c.vmid, VMNode: c.loc.Node, Kind: pve.HADecisionRecovery, Health: report,
	})
	if err != nil {
		return nil, nil, "", err
	}
	return report, st, fenceEvidenceGap(report, st, c.loc.Node), nil
}

// fenceEvidenceGap returns why report and st do not prove that the service is
// in recovery on the fenced node scanNode, or "" when they do.
func fenceEvidenceGap(report *pve.NodeHealthReport, st *pve.HAServiceState, scanNode string) string {
	switch {
	case report == nil || st == nil:
		return "no HA view was read"
	case report.HA != pve.HAViewManagerStatus:
		return fmt.Sprintf("the HA view is %s, not a fresh manager status", report.HA)
	case !report.Quorate || !report.QuorumReported:
		return "the entry node does not report a quorate cluster"
	case !st.Known:
		return st.Reason
	case !st.Configured:
		return st.SID + " has no configured HA resource"
	case st.Raw != haRawStateRecovery:
		return fmt.Sprintf("%s is in %q, not in recovery", st.SID, st.Raw)
	case st.Node != scanNode:
		return fmt.Sprintf("the CRM holds %s on node %s, but the scan located the VM on node %s",
			st.SID, st.Node, scanNode)
	case !report.DeadOrFenced(st.Node) || report.ProxyTarget(st.Node) || report.Placeable(st.Node):
		return fmt.Sprintf("node %s is not reported dead", st.Node)
	}
	if disp, reason := st.Disposition(); disp != pve.HAGraceWindow {
		return fmt.Sprintf("the HA state answers %s (%s), not the recovery grace window", disp, reason)
	}
	return ""
}

// proveExpiredFence reports whether a fresh read proves the fence and the
// recovery episode has outlasted the grace window. Any doubt answers false, and
// the gate's deferral stands.
func (c *deadNodeCompletion) proveExpiredFence(ctx context.Context) (bool, error) {
	_, st, gap, err := c.readFenceEvidence(ctx)
	if err != nil {
		c.logger.Warn(deadNodeDelete+": the HA view for the dead-node completion is unreadable; deferring",
			log.String("sid", c.sid), log.Err(err))
		return false, err
	}
	if gap != "" {
		c.logger.Debug(deadNodeDelete+": the dead-node completion does not apply", log.String("sid", c.sid),
			log.String("reason", gap))
		return false, nil
	}
	c.source, c.uid = st.Node, st.UID
	ep, recErr := haEpisodes(c.deps).Observe(st.SID, st.UID)
	if recErr != nil {
		c.logger.Warn(deadNodeDelete+": the HA recovery episode record is unavailable, so the dead-node "+
			"completion waits for the grace window again", log.String("sid", st.SID), log.Err(recErr))
		return false, nil
	}
	return ep.Elapsed() >= haMotionWait(c.deps), nil
}

// checkPreconditions runs every read-only check before the first write. Each
// refusal is a retriable deferral that names what the operator can change.
func (c *deadNodeCompletion) checkPreconditions(ctx context.Context) error {
	if c.deps.Config == nil || c.deps.Config.HAModeValue() == config.HAModeOff {
		return c.deferf("the recovery grace window expired, but pve.ha.mode is off, so the CPI writes no HA " +
			"state and cannot move the config off the dead node; bring the node back, or follow the dead-node " +
			"procedure in the operations guide")
	}
	c.owner = c.deps.RequestDirectorUUID
	if c.owner == "" {
		return c.deferf("the request carries no director_uuid, so the CPI cannot mark or check the HA rules " +
			"that would move the config off the dead node")
	}
	sysConsole, err := haDeleteSysConsole(ctx, c.deps, deadNodeDelete, c.vmid)
	if err != nil {
		return deferHA(err)
	}
	if !sysConsole {
		return c.deferf("the recovery grace window expired, but the token lacks Sys.Console on /, which the " +
			"HA rule edits that move the config off the dead node need; the CPI runs observe-only and keeps " +
			"deferring until the privilege is granted or the node is back")
	}
	profile, err := haRuleWriteProfile(ctx, c.deps)
	if err != nil {
		return c.deferWrap(err, "the HA rules cannot be written")
	}
	c.profile = profile
	if err := c.checkOwnership(ctx); err != nil {
		return err
	}
	set, err := c.readRules(ctx)
	if err != nil {
		return err
	}
	if blockers := ha.DeadNodeRuleBlockers(set, c.sid, naRuleNameFor(c.vmid), c.ownedRule); len(blockers) > 0 {
		return c.blocked(blockers)
	}
	return c.computeReach(ctx, set)
}

// blocked is the deferral for HA rules the completion may not edit.
func (c *deadNodeCompletion) blocked(blockers []string) error {
	return c.deferf("the HA rules cannot be narrowed to move the config off the dead node: %s; the CPI "+
		"edits only its own pin and anti-affinity rules, so resolve these rules or follow the dead-node "+
		"procedure in the operations guide", strings.Join(blockers, "; "))
}

// checkOwnership proves the VM and its HA resource are this Director's before
// anything is written. The config cannot be read on the dead node, so the
// VM's verdict comes from the scan row's tags, and a verdict that needs the
// config fails and defers. The HA resource must carry this Director's marker.
func (c *deadNodeCompletion) checkOwnership(ctx context.Context) error {
	verdict, err := checkVMOwnership(ctx, c.deps, deadNodeDelete, c.cid, c.loc, nil)
	if err != nil {
		return c.deferWrap(err, "the VM's ownership cannot be proven while its config is on the dead node")
	}
	if !verdict.Owned || verdict.foreign() || verdict.Gone || verdict.Absent {
		logForeignVM(c.logger, deadNodeDelete+": the dead-node completion refuses a VM it cannot prove is ours",
			verdict)
		return c.deferf("the scan does not prove this Director owns the VM (%s), so the CPI does not move its "+
			"config off the dead node", orUnsetReason(verdict.Reason))
	}
	res, err := c.deps.PVE.Cluster().GetHaResources(ctx, c.sid)
	if err != nil {
		return c.deferWrap(err, "read HA resource %s", c.sid)
	}
	if res == nil {
		return c.deferf("HA resource %s answered with no body", c.sid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	if haOwnershipOf(comment, c.owner) != haOwnedByUs {
		return c.deferf("HA resource %s does not carry this Director's ownership marker, so the CPI does not "+
			"move its config off the dead node", c.sid)
	}
	if res.Group != nil && strings.TrimSpace(*res.Group) != "" {
		return c.deferf("HA resource %s is in HA group %q, which the CPI cannot narrow; move it to node-affinity "+
			"rules, or follow the dead-node procedure in the operations guide", c.sid, *res.Group)
	}
	return nil
}

func orUnsetReason(reason string) string {
	if reason == "" {
		return "no ownership reason"
	}
	return reason
}

// ownedRule reports whether r carries this Director's ownership marker.
func (c *deadNodeCompletion) ownedRule(r *ha.Rule) bool {
	return c.owner != "" && haOwnershipOf(r.Comment, c.owner) == haOwnedByUs
}

// readRules reads and normalizes the HA rules.
func (c *deadNodeCompletion) readRules(ctx context.Context) (*ha.RuleSet, error) {
	resp, err := c.deps.PVE.Cluster().ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, c.deferWrap(err, "read the HA rules")
	}
	set, err := ha.ParseRuleList(resp)
	if err != nil {
		return nil, c.deferWrap(err, "parse the HA rules")
	}
	return set, nil
}

// computeReach reads the node health and computes the deletion destinations
// from the VM's pin in set. An empty set defers, naming what keeps the config
// on the dead node.
func (c *deadNodeCompletion) computeReach(ctx context.Context, set *ha.RuleSet) error {
	report, err := pve.NodeHealth(ctx, c.deps.PVE.Cluster(), c.deps.PVE.Nodes(), haHealthOptions(c.deps))
	if err != nil {
		return c.deferWrap(err, "read node health to compute the deletion destinations")
	}
	reach, err := c.destinations(ctx, report, set)
	if err != nil {
		return err
	}
	c.reach = reach
	return nil
}

// destinations computes the deletion destinations from report, set, and the
// storage index, and defers when there are none. A VM whose pin carries no
// record of its volumes defers too, because nothing then says which nodes
// reach them.
func (c *deadNodeCompletion) destinations(
	ctx context.Context, report *pve.NodeHealthReport, set *ha.RuleSet,
) (ha.DeadNodeReach, error) {
	if report == nil {
		return ha.DeadNodeReach{}, c.deferf("no node health was read to compute the deletion destinations")
	}
	if c.storages == nil {
		storages, err := readStorageIndex(ctx, c.deps)
		if err != nil {
			return ha.DeadNodeReach{}, c.deferWrap(err, "read the storage index to compute the deletion destinations")
		}
		index, err := ha.NewStorageIndex(storages)
		if err != nil {
			return ha.DeadNodeReach{}, c.deferWrap(err, "index the storages to compute the deletion destinations")
		}
		c.storages = index
	}
	// The reach reads the in-memory storage index, so it runs detached from the
	// call's cancellation as it always has, while keeping the call's affinity
	// scope.
	reach, err := ha.DeletionDestinations(context.WithoutCancel(ctx), c.source, reportMembers(report), set, c.sid, naRuleNameFor(c.vmid),
		c.ownedRule, report.Placeable, c.storages)
	if err != nil {
		return reach, c.deferWrap(err, "compute the deletion destinations")
	}
	if reach.Replica != "" {
		if err := c.confirmReplicaJob(ctx, reach.Replica); err != nil {
			return reach, err
		}
	}
	if len(reach.Destinations) == 0 {
		return reach, c.noDestinations(reach)
	}
	return reach, nil
}

// confirmReplicaJob checks the replication job behind a replica the VM's pin
// claims on partner before the completion moves the config there. The job
// must carry this Director's marker, be enabled and not marked for removal,
// and pair the dead node with partner in either direction. Anything else, a
// job list that cannot be read included, defers, because the token alone does
// not prove a copy exists. The delete on the partner then proves each copy is
// on the node.
func (c *deadNodeCompletion) confirmReplicaJob(ctx context.Context, partner string) error {
	jobs, err := replicationAPIFor(c.deps).ListJobs(ctx)
	if err != nil {
		return c.deferWrap(err, "list the replication jobs to confirm the replica on node %s", partner)
	}
	job, _, err := guestReplicaJobs(jobs, c.vmid, c.deps.RequestDirectorUUID)
	switch {
	case err != nil:
		return c.deferWrap(err, "read the replication job behind the replica on node %s", partner)
	case job == nil:
		return c.deferf("the pin claims a replica on node %s, and the VM has no replication job of this Director",
			partner)
	case job.Disabled || job.RemoveJob != "":
		return c.deferf("the pin claims a replica on node %s, and replication job %s is disabled or marked for "+
			"removal", partner, job.ID)
	}
	forward := job.Target == partner && (job.Source == "" || job.Source == c.source)
	swapped := job.Target == c.source && job.Source == partner
	if !forward && !swapped {
		return c.deferf("the pin claims a replica on node %s, and replication job %s runs from %s to %s", partner,
			job.ID, orNone(job.Source), job.Target)
	}
	return nil
}

// reportMembers returns the cluster members a node health report names.
func reportMembers(report *pve.NodeHealthReport) []string {
	if report == nil {
		return nil
	}
	members := make([]string, 0, len(report.Nodes))
	for i := range report.Nodes {
		members = append(members, report.Nodes[i].Name)
	}
	return members
}

// noDestinations is the deferral for an empty set of deletion destinations.
func (c *deadNodeCompletion) noDestinations(reach ha.DeadNodeReach) error {
	var why string
	switch {
	case reach.PinnedToSource():
		why = fmt.Sprintf("the VM's strict pin %s admits dead node %s alone, which is the pin of a VM with a "+
			"node-local volume, of a hold VM, or of a VM whose hard constraints leave one node, so no live node "+
			"is known to reach its volumes", naRuleNameFor(c.vmid), c.source)
	case len(reach.Reach) == 0 && len(reach.Admissible) > 0:
		why = fmt.Sprintf("the storages %s that the VM's pin records reach dead node %s alone, so the VM holds a "+
			"volume no live node can reach", strings.Join(reach.Recorded, ", "), c.source)
	case len(reach.Unplaceable) > 0:
		why = fmt.Sprintf("node %s is admitted by the VM's pin but is not online with a fresh LRM timestamp",
			strings.Join(reach.Unplaceable, ", "))
	default:
		why = "no live node is admitted by the VM's pin"
	}
	c.logger.Warn(deadNodeDelete+": no deletion destination for the fenced VM; deferring",
		log.String("sid", c.sid), log.String("dead_node", c.source), log.String("reason", why))
	return c.deferf("the recovery grace window expired, and there is no deletion destination: %s. The delete "+
		"keeps deferring. Bring the node back, or follow the dead-node procedure in the operations guide. That "+
		"procedure moves the config by hand for a VM whose volumes are shared or disposable, and for a "+
		"persistent disk on the dead node it removes the disk reference with bosh cck once the data is given up",
		why)
}

// readStorageIndex reads and decodes the whole PVE storage index. An entry
// that does not decode is an error, because a storage left out could be the
// one that keeps a volume on the dead node.
func readStorageIndex(ctx context.Context, deps Deps) ([]pve.StorageInfo, error) {
	svc := deps.PVE.ClusterStorage()
	if svc == nil {
		return nil, errors.New("the PVE client has no cluster storage service")
	}
	resp, err := svc.ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("the storage index answered with no body")
	}
	out := make([]pve.StorageInfo, 0, len(*resp))
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			return nil, fmt.Errorf("decode a storage index entry: %w", perr)
		}
		out = append(out, info)
	}
	return out, nil
}

// requestStopped requests stopped for the HA resource, which PVE accepts
// during recovery, so the CRM leaves the VM stopped wherever it recovers it.
// The same write adds the dead-node token to the resource's comment, which
// the delete on the destination reads. A write whose answer is lost is read
// back, and it counts when the resource shows both.
func (c *deadNodeCompletion) requestStopped(ctx context.Context) error {
	svc := c.deps.PVE.Cluster()
	res, err := svc.GetHaResources(ctx, c.sid)
	if err != nil {
		return c.deferWrap(err, "read HA resource %s before requesting stopped", c.sid)
	}
	if res == nil {
		return c.deferf("HA resource %s answered with no body", c.sid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	if haOwnershipOf(comment, c.owner) != haOwnedByUs {
		return c.deferf("HA resource %s no longer carries this Director's ownership marker", c.sid)
	}
	if resourceRequestsStopped(res, c.source) {
		return nil
	}
	state := haRequestStopped
	tokened := ha.WithDeadNodeSource(comment, c.source)
	params := &cluster.UpdateHaResourcesParams{State: &state, Comment: &tokened}
	if res.Digest != "" {
		digest := res.Digest
		params.Digest = &digest
	}
	c.logger.Info(deadNodeDelete+": requesting stopped for the fenced VM's HA resource",
		log.String("sid", c.sid), log.String("dead_node", c.source))
	writeErr := svc.UpdateHaResources(ctx, c.sid, params)
	if writeErr == nil {
		return nil
	}
	back, readErr := svc.GetHaResources(ctx, c.sid)
	if readErr == nil && resourceRequestsStopped(back, c.source) {
		c.logger.Info(deadNodeDelete+": the stop request's answer was lost, and the resource shows it landed",
			log.String("sid", c.sid), log.Err(writeErr))
		return nil
	}
	return c.deferWrap(writeErr, "request stopped for HA resource %s", c.sid)
}

// resourceRequestsStopped reports whether res requests stopped and carries the
// dead-node token naming source.
func resourceRequestsStopped(res *cluster.GetHaResourcesResponse, source string) bool {
	if res == nil || res.State == nil || *res.State != haRequestStopped || res.Comment == nil {
		return false
	}
	got, ok := ha.DeadNodeSource(*res.Comment)
	return ok && got == source
}

// narrowRules writes the strict pin and removes the VM from its anti-affinity
// rules under the rules lock, and returns the destinations the pin names. The
// fence is proven again under the lock, for the same recovery episode, and
// the destinations are computed again from that read, so a node that came
// back or a rule that changed since the first read stops the completion
// before any rule changes.
//
// The order of the edits keeps the recovery inside the destinations at every
// step. When every node the current rules allow, other than the dead one, is
// in the reach, the anti-affinity release goes first, because a strict pin
// next to a full anti-affinity rule can make the rule set infeasible.
// Otherwise the pin goes first, because removing the VM from an anti-affinity
// rule can let the CRM recover it at once. That first pin is a bridge: it
// names the destinations and the dead node, so the anti-affinity rule's
// members keep enough nodes for the rule set to stay feasible, while the CRM
// still cannot recover the VM outside the destinations, because the dead node
// cannot receive it. The release follows, and then the final pin, which names
// the destinations alone. A bridge that is not in effect stops the completion
// with nothing else changed, because a release without a pin in effect would
// let the CRM recover the VM onto any node.
func (c *deadNodeCompletion) narrowRules(ctx context.Context) ([]string, error) {
	var dests []string
	err := withHARulesLock(ctx, c.deps, c.vmid, c.logger, func() error {
		report, st, gap, err := c.readFenceEvidence(ctx)
		if err != nil {
			return c.deferWrap(err, "read the HA view again before the rule edits")
		}
		if gap != "" {
			return c.deferf("the fence is no longer proven (%s), so no HA rule was changed; the next retry "+
				"reads the VM's state again", gap)
		}
		if st.UID != c.uid {
			return c.deferf("the CRM began a new recovery episode for %s (uid %s, was %s), so no HA rule was "+
				"changed; the grace window runs again for the new episode", c.sid, orNone(st.UID), orNone(c.uid))
		}
		set, err := c.readRules(ctx)
		if err != nil {
			return err
		}
		pinID := naRuleNameFor(c.vmid)
		if blockers := ha.DeadNodeRuleBlockers(set, c.sid, pinID, c.ownedRule); len(blockers) > 0 {
			return c.blocked(blockers)
		}
		reach, err := c.destinations(ctx, report, set)
		if err != nil {
			return err
		}
		c.reach, dests = reach, reach.Destinations
		comment := ha.DeadNodePinComment(set.Find(pinID), c.ownedRule, haIdentity{Owner: c.owner}.marker(),
			c.source)
		final := ha.DeadNodePin(pinID, c.sid, comment, dests)
		writer := newHARuleWriter(c.deps, c.profile, c.logger)
		// A rule that is not in effect after a write fails the completion in
		// every mode, and the writer never removes the HA resource: the
		// resource is what lets the CRM recover the VM at all.
		writer.Managed = true
		writer.Repair = ha.RepairDeps{}
		if !c.allowedWithinReach(set, reportMembers(report)) {
			bridge := ha.DeadNodePin(pinID, c.sid, comment, append(slices.Clone(dests), c.source))
			if err := c.writePin(ctx, writer, bridge, "bridge pin"); err != nil {
				return err
			}
		}
		if err := c.releaseAntiAffinity(ctx, writer, pinID); err != nil {
			return err
		}
		return c.writePin(ctx, writer, final, "strict pin")
	})
	if err != nil {
		return nil, deferHA(err)
	}
	return dests, nil
}

// allowedWithinReach reports whether every node other than the dead one that
// the current rules let the recovery choose is in the reach.
func (c *deadNodeCompletion) allowedWithinReach(set *ha.RuleSet, members []string) bool {
	for _, n := range ha.AllowedNodes(set, c.sid, members) {
		if n != c.source && !slices.Contains(c.reach.Reach, n) {
			return false
		}
	}
	return true
}

// writePin writes want, which what names in the logs and errors, and checks
// that it is in effect.
func (c *deadNodeCompletion) writePin(ctx context.Context, writer *ha.RuleWriter, want ha.Rule, what string) error {
	res, err := writer.Apply(ctx, "dead-node "+what, ha.PlanDeadNodePin(want, c.ownedRule))
	if err != nil {
		return c.deferWrap(err, "write the %s %q", what, want.ID)
	}
	set := res.Set
	if len(res.Wrote) == 0 {
		// Nothing was written, so the writer verified nothing. Read again so
		// the check below sees PVE's current errors for the pin.
		if set, err = c.readRules(ctx); err != nil {
			return err
		}
	}
	if ok, why := ha.DeadNodePinInEffect(set, want); !ok {
		return c.deferf("the %s is not in effect (%s), so the CPI changes nothing else; the next retry writes "+
			"it again", what, why)
	}
	c.logger.Info(deadNodeDelete+": the "+what+" is in effect", log.String("sid", c.sid),
		log.String("rule", want.ID), log.String("nodes", want.NodesParam()))
	return nil
}

// releaseAntiAffinity removes the VM from this Director's anti-affinity rules.
func (c *deadNodeCompletion) releaseAntiAffinity(ctx context.Context, writer *ha.RuleWriter, pinID string) error {
	res, err := writer.Apply(ctx, "dead-node anti-affinity", ha.PlanAntiAffinityRelease(c.sid, pinID, c.ownedRule))
	if err != nil {
		return c.deferWrap(err, "remove %s from its anti-affinity rules", c.sid)
	}
	for i := range res.Wrote {
		op := &res.Wrote[i]
		c.logger.Info(deadNodeDelete+": removed the fenced VM from its anti-affinity rule",
			log.String("sid", c.sid), log.String("rule", op.Rule.ID), log.String("op", string(op.Kind)))
	}
	return nil
}

// awaitMove waits for the CRM to move the config onto one of dests and for
// the scan to show the VM there, and returns that node. The wait lasts
// pve.ha.motion_wait_sec, cut short so the delete's deadline still holds the
// stop and destroy reserves. A VM that leaves recovery for a node outside
// dests, such as the dead node coming back, is a retriable error, so the next
// call locates it again. Running out of time is a deferral, and the next call
// resumes: the pin, the stop request, and the released anti-affinity stay.
func (c *deadNodeCompletion) awaitMove(ctx context.Context, dests []string) (string, error) {
	wait := haMotionWait(c.deps)
	limit, clamped := haDeleteWaitLimit(ctx, wait, haDeleteStopReserve+haDeleteDestroyReserve())
	for {
		st, err := readHADeleteState(ctx, c.deps, deadNodeDelete, c.vmid, c.source)
		if err != nil {
			return "", c.deferWrap(err, "read the HA state while waiting for the CRM to move the config")
		}
		if st.Known {
			switch {
			case st.Raw == haRawStateFence || st.NodeHealth.CRMState == haNodeFence:
				return "", c.deferf("%s is in fence on node %s while the CPI waits for the CRM to move the "+
					"config; the next retry reads it again", st.SID, st.Node)
			case !st.Managed():
				return "", c.deferf("%s is no longer HA-managed, so the CRM will not move the config off the "+
					"dead node; the next retry reads the VM's state again", st.SID)
			case st.Raw != haRawStateRecovery && slices.Contains(dests, st.Node):
				moved, err := c.scanShows(ctx, st.Node)
				if err != nil {
					return "", err
				}
				if moved {
					return st.Node, nil
				}
			case st.Raw != haRawStateRecovery && st.Node == c.source:
				return "", c.deferf("dead node %s is back, and the CRM holds %s there in %q; the next retry "+
					"runs the delete against that node", c.source, st.SID, st.Raw)
			case st.Raw != haRawStateRecovery:
				return "", c.deferf("the CRM holds %s on node %s in %q, outside the deletion destinations %s; "+
					"the next retry locates the VM again and runs the delete against the node that holds it",
					st.SID, st.Node, st.Raw, strings.Join(dests, ", "))
			}
		}
		remaining := time.Until(limit)
		if remaining <= 0 {
			if clamped {
				return "", c.deferf("the delete's deadline leaves no more time to wait for the CRM to move the "+
					"config onto %s; the strict pin stays, and the next retry resumes", strings.Join(dests, ", "))
			}
			return "", c.deferf("the CRM did not move the config onto %s within the %s motion budget; the "+
				"strict pin stays, and the next retry resumes", strings.Join(dests, ", "), wait)
		}
		if err := sleepOrDone(ctx, min(haDeleteMotionPoll(), remaining)); err != nil {
			return "", c.deferWrap(err, "the wait for the CRM to move the config was cancelled")
		}
	}
}

// scanShows reports whether the cluster scan locates the VM on node.
func (c *deadNodeCompletion) scanShows(ctx context.Context, node string) (bool, error) {
	loc, err := findVMAuthoritative(ctx, c.deps, c.vmid)
	if err != nil {
		return false, c.deferWrap(err, "locate the VM after the CRM moved its config")
	}
	return loc.Found && loc.Node == node, nil
}

// refuseDeadNodeStrandedVolumes is the delete's check on a VM whose config
// the dead-node completion moved, which its HA resource's dead-node token
// marks. The move was decided without the config, so before anything stops,
// detaches, or destroys the VM, the delete proves from the config it now reads
// that node reaches every volume, with each node-local volume counted as
// living on the dead node, unless the VM's pin names node as the partner that
// holds a verified replica and node's storage listing shows the copy of a
// volume the replication job copies (see deadNodeReplicaCopies). A VM it
// cannot prove that for defers, so a volume the destroy could not reach is
// never left behind as an orphan, and a persistent disk is never destroyed. It returns the dead node the token
// names when the check passes, so the teardown can prove the VM's absence
// while that node stays down. A VM without a configured HA resource, or whose
// resource carries no token, passes with no source, and so does a VM back on
// the node the token names, which came back before the config moved.
func refuseDeadNodeStrandedVolumes(
	ctx context.Context, deps Deps, vmid int, node string, st *pve.HAServiceState, liveConfig, pending map[string]any,
	logger *log.Logger,
) (string, error) {
	if st == nil || !st.Configured {
		return "", nil
	}
	sid := haResourceSid(vmid)
	res, err := deps.PVE.Cluster().GetHaResources(ctx, sid)
	if err != nil {
		if isHaNotFound(err) {
			return "", nil
		}
		return "", deferHA(cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: read HA resource %s to check whether its config left a dead node", deadNodeDelete, sid)))
	}
	if res == nil || res.Comment == nil {
		return "", nil
	}
	source, moved := ha.DeadNodeSource(*res.Comment)
	if !moved || source == node {
		return "", nil
	}
	refuse := func(format string, args ...any) error {
		return deferHA(cpierrors.Retriable("%s: VM %d on node %s, whose config left dead node %s: "+format,
			append([]any{deadNodeDelete, vmid, node, source}, args...)...))
	}
	volumes, err := pve.VMAccessVolumes(ctx, deps.PVE, node, vmid, liveConfig)
	if err != nil {
		return "", refuse("read the VM's volumes: %v; retry", err)
	}
	seen := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		seen[v.Volume] = true
	}
	for _, v := range pve.ConfigAccessVolumes(pending, node) {
		if !seen[v.Volume] {
			seen[v.Volume] = true
			volumes = append(volumes, v)
		}
	}
	storages, err := readStorageIndex(ctx, deps)
	if err != nil {
		return "", refuse("read the storage index: %v; retry", err)
	}
	index, err := ha.NewStorageIndex(storages)
	if err != nil {
		return "", refuse("index the storages: %v; retry", err)
	}
	onNode, err := deadNodeReplicaCopies(ctx, deps, vmid, node, storages, liveConfig, pending)
	if err != nil {
		return "", refuse("%v; retry", err)
	}
	for i := range volumes {
		volumes[i].HomeNode = source
		if onNode[volumes[i].Volume] {
			volumes[i].HomeNode = node
		}
	}
	reach, err := pve.VolumeAccessSet(ctx, index, []string{node, source}, volumes)
	if err != nil {
		return "", refuse("compute the reach of the VM's volumes: %v", err)
	}
	if !slices.Contains(reach, node) {
		logger.Warn(deadNodeDelete+": the moved VM holds a volume its node cannot reach; deferring",
			log.String("sid", sid), log.String("dead_node", source), log.String("node", node))
		return "", refuse("node %s does not reach every volume the config names, so the delete would leave a "+
			"volume behind on the dead node; bring the dead node back, or follow the dead-node procedure in the "+
			"operations guide", node)
	}
	logger.Info(deadNodeDelete+": the moved VM's volumes are all reachable from its node",
		log.String("sid", sid), log.String("dead_node", source), log.String("node", node))
	return source, nil
}

// deadNodeReplicaCopies returns the volumes of a VM moved off a dead node that
// a verified replica holds on node, the node the config moved to. Only a VM
// whose pin carries this Director's marker and names node as its replication
// partner has any: those are the volumes its replication job copies (see
// ha.ReplicaVolumes), each kept only when node's storage listing shows it.
// Every other volume, a persistent disk a disk ID owns included, still lives
// on the dead node. A VM with no such pin returns none and reads nothing past
// the rules. A read that fails is an error, which the caller defers on.
func deadNodeReplicaCopies(
	ctx context.Context, deps Deps, vmid int, node string, storages []pve.StorageInfo,
	liveConfig, pending map[string]any,
) (map[string]bool, error) {
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		return nil, fmt.Errorf("read the HA rules to check for a replica on node %s: %w", node, err)
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin == nil || haOwnershipOf(pin.Comment, deps.RequestDirectorUUID) != haOwnedByUs {
		return nil, nil
	}
	if partner, ok := ha.ReplicaPartner(pin.Comment); !ok || partner != node {
		return nil, nil
	}
	vols := append(haVolumesOfConfig(liveConfig, node, nil), haVolumesOfConfig(pending, node, nil)...)
	replicated, _ := ha.ReplicaVolumes(vmid, vols, storages)
	api := replicationAPIFor(deps)
	listings := map[string]map[string]bool{}
	out := make(map[string]bool, len(replicated))
	for _, volid := range replicated {
		storage, _, _ := strings.Cut(volid, ":")
		listed, read := listings[storage]
		if !read {
			listed, err = api.Volumes(ctx, node, storage)
			if err != nil {
				return nil, fmt.Errorf("list storage %s on node %s to find the replica of volume %s: %w", storage,
					node, volid, err)
			}
			listings[storage] = listed
		}
		if listed[volid] {
			out[volid] = true
		}
	}
	return out, nil
}

// locateAfterDeadNodeMove locates VM vmid again after a step on node answered
// that it is missing, for a VM whose config the dead-node completion moved off
// source. It is locateAfterConfigMissing with one difference. The config
// probe cannot reach source while that node is down, so the absence proof
// rests on the cluster index for source's guests, as findVMAuthoritative's
// does. PVE keeps a VMID's config in one node directory of the cluster
// filesystem, and the index lists offline members' guests, so an index miss,
// clean probes of every online member, and no HA resource or CRM service for
// the VMID prove the VM gone from source as well. Any other offline member
// ties the VM to that member and leaves the proof unproven, so the tolerance
// covers source alone. node must still be one the cluster reports able to
// answer.
func locateAfterDeadNodeMove(
	ctx context.Context, deps Deps, op string, vmid int, node, source string,
) (pve.VMLocation, error) {
	if err := requireProxyTarget(ctx, deps, op, vmid, node); err != nil {
		return pve.VMLocation{}, err
	}
	onlySource := func(_ context.Context, _ int, offline []string) (string, string, error) {
		for _, member := range offline {
			if member != source {
				return member, "the dead-node delete covers only dead node " + source, nil
			}
		}
		return "", "", nil
	}
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid,
		pve.WithOfflineMemberTie(journalOfflineMemberTie(deps.Config)), pve.WithOfflineMemberTie(onlySource))
	if err != nil {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d config is gone from node %s, and locating the VM again failed", op, vmid, node))
	}
	if loc.Found && loc.Node == "" {
		return pve.VMLocation{}, cpierrors.Retriable(
			"%s: VM %d config is gone from node %s, and the member that holds it now could not be named", op, vmid, node)
	}
	return loc, nil
}
