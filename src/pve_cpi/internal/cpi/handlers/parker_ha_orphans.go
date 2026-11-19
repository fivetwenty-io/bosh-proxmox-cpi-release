package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// HA leftovers of a parker that is gone.
//
// A parker's HA resource and its pin can outlive the parker's config. PVE
// refuses to destroy a guest that HA manages unless the destroy purges it,
// and a purge removes the HA resource before the config, so a destroy through
// the API leaves no resource behind. A config that someone deleted from
// /etc/pve by hand, or that went with the directory of a node removed from
// the cluster, leaves both behind. A retirement whose destroy landed but
// whose pin delete failed leaves the pin. PVE's CRM drops the service of a VM
// that has no config, so the leftover resource never fails over and nothing
// but this step removes it.
//
// The step runs at the end of the sweep's parker HA pass, only when the HA
// profile lets the CPI write HA state, and it removes a leftover only when it
// can prove both that the leftover is this Director's parker's and that the
// parker is gone. Ownership is the marker the registration writes. A resource
// carries this Director as owner and names no deployment or group, and a pin
// is the parker's bosh-na-<vmid> node-affinity rule with this Director's
// marker that names no other resource. Both name a VMID in this config's
// parker band, under the parked strategy, whose band no other VMID range may
// overlap. The removal reads the resource again and keeps it unless it still
// carries that marker.
//
// Absence takes a quorate cluster and a cluster index that lists no guest at
// the VMID. Every node the pin names that is still a cluster member must
// answer a config read that the config does not exist, and must be a node the
// cluster reports able to answer. A node that both the member list and the
// cluster status leave out was removed from the cluster, so it is not read,
// and a config left in its directory would still show in the index. Then a
// probe of every member must find the config nowhere, skip no member as
// offline, and see the CRM hold no service for the VMID. Any other answer
// keeps the leftover, and the doctor reports it.
//
// A resource that has no pin names no node, because the CRM's status names a
// node for a service only while it still holds the service, and while it holds
// it the proof refuses. The probe of every member is what proves such a
// parker gone, which is why the proof needs every member online.
//
// The proof runs again under the parker's VMID claim, which every
// registration, park, and retirement of the parker holds, taken with a single
// try. A parker's create takes no claim, so the claim alone does not keep the
// step from racing a create. The VMID allocation does that. Outside off mode
// a parker's allocation reads the HA resources and rules and never picks a
// VMID that a resource or a bosh-na pin still names, so no parker is created
// at a VMID while its leftovers exist, and none inherits them. The step
// removes the resource before the pin, so the VMID stays out of the
// allocation's reach until both are gone. PVE records no creation time on a
// resource or a rule, which is why the allocation, and not a timestamp, keeps
// a new parker and an old parker's leftovers apart.

// parkerHAOrphan is one VMID in the parker band whose leftovers the step may
// remove.
type parkerHAOrphan struct {
	vmid int
	// resource is set when this Director's parker resource vm:<vmid> exists.
	resource bool
	// pin is set when this Director's pin of the parker exists and names no
	// other resource.
	pin bool
	// nodes are the nodes the pin names, sorted, whose answers the absence
	// proof needs.
	nodes []string
}

// parkerHAOrphanCaller names the step in its errors and logs.
const parkerHAOrphanCaller = "parker HA leftover cleanup"

// reconcileOrphans removes the HA resource and the pin of every parker of
// this Director whose config no member holds, as the comment above describes.
// It never returns an error: a leftover it could not prove, or could not
// remove, waits for the next sweep.
func (r *parkerHA) reconcileOrphans(ctx context.Context) {
	if r.deps.Config.DetachedDiskStrategyValue() != config.DetachedDiskStrategyParked {
		return
	}
	orphans, err := r.orphanCandidates(ctx)
	if err != nil {
		r.logger.Debug("parker HA: the HA leftovers of gone parkers could not be listed; the next sweep tries "+
			"again", log.String("node", r.node), log.Err(err))
		return
	}
	// The profile is resolved only once a leftover is found, so a sweep that
	// finds none never resolves it.
	if len(orphans) == 0 || !r.loadProfile(ctx) {
		return
	}
	for _, o := range orphans {
		if ctx.Err() != nil {
			r.logger.Info("parker HA: the pass ran out of its time budget; the remaining HA leftovers of gone "+
				"parkers wait for the next sweep", log.String("node", r.node))
			return
		}
		r.reconcileOrphan(ctx, o)
	}
}

// orphanCandidates lists the VMIDs in the band that carry this Director's
// parker resource or pin, and that the cluster index lists no guest at. It
// reads no config and takes no claim, so its answer only picks the VMIDs the
// proof under the claim then runs for.
func (r *parkerHA) orphanCandidates(ctx context.Context) ([]parkerHAOrphan, error) {
	svc := r.deps.PVE.Cluster()
	byVMID := map[int]*parkerHAOrphan{}
	entry := func(vmid int) *parkerHAOrphan {
		if byVMID[vmid] == nil {
			byVMID[vmid] = &parkerHAOrphan{vmid: vmid}
		}
		return byVMID[vmid]
	}

	resources, err := svc.ListHaResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list the HA resources: %w", err)
	}
	if resources == nil {
		return nil, errors.New("list the HA resources: no rows returned")
	}
	for _, raw := range *resources {
		var row struct {
			SID     string `json:"sid"`
			Comment string `json:"comment"`
		}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		id, isVM := strings.CutPrefix(row.SID, "vm:")
		if !isVM {
			continue
		}
		vmid, ok := r.bandVMID(id)
		if ok && r.parkerMarked(row.Comment) {
			entry(vmid).resource = true
		}
	}

	rulesResp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, fmt.Errorf("list the HA rules: %w", err)
	}
	rules, err := ha.ParseRuleList(rulesResp)
	if err != nil {
		return nil, fmt.Errorf("parse the HA rules: %w", err)
	}
	pinNodes := map[int][]string{}
	for i := range rules.Rules {
		rule := &rules.Rules[i]
		id, ok := strings.CutPrefix(rule.ID, naRuleNamePrefix)
		if !ok {
			continue
		}
		vmid, ok := r.bandVMID(id)
		if !ok || !r.parkerPin(rule, vmid) {
			continue
		}
		entry(vmid).pin = true
		pinNodes[vmid] = sortedRuleNodes(rule)
	}
	if len(byVMID) == 0 {
		return nil, nil
	}

	guests, err := pve.ListIndexedGuests(ctx, r.deps.PVE)
	if err != nil {
		return nil, err
	}
	for _, g := range guests {
		delete(byVMID, g.VMID)
	}
	if len(byVMID) == 0 {
		return nil, nil
	}

	out := make([]parkerHAOrphan, 0, len(byVMID))
	for vmid, o := range byVMID {
		o.nodes = pinNodes[vmid]
		out = append(out, *o)
	}
	slices.SortFunc(out, func(a, b parkerHAOrphan) int { return a.vmid - b.vmid })
	return out, nil
}

// bandVMID parses id, the VMID part of a SID or a pin's name, as a VMID in the
// parker band. It must spell the VMID canonically.
func (r *parkerHA) bandVMID(id string) (int, bool) {
	vmid, err := strconv.Atoi(id)
	if err != nil || vmid <= 0 || strconv.Itoa(vmid) != id {
		return 0, false
	}
	return vmid, vmid >= r.cfg.VMIDRangeStart && vmid <= r.cfg.VMIDRangeEnd
}

// parkerMarked reports whether comment is the marker a parker registration
// writes for this Director: owner set to it, and no deployment or group, which
// every VM resource names.
func (r *parkerHA) parkerMarked(comment string) bool {
	if haOwnershipOf(comment, r.owner) != haOwnedByUs {
		return false
	}
	m, _ := parseHaMarker(comment)
	return m.Deployment == "" && m.Group == ""
}

// parkerPin reports whether rule is this Director's pin of parker vmid that
// names no resource but the parker's.
func (r *parkerHA) parkerPin(rule *ha.Rule, vmid int) bool {
	if rule.Type != naRuleType || haOwnershipOf(rule.Comment, r.owner) != haOwnedByUs {
		return false
	}
	sid := "vm:" + strconv.Itoa(vmid)
	for _, res := range rule.Resources {
		if res != sid {
			return false
		}
	}
	return true
}

// sortedRuleNodes returns the nodes rule names, sorted.
func sortedRuleNodes(rule *ha.Rule) []string {
	nodes := make([]string, 0, len(rule.Nodes))
	for node := range rule.Nodes {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	return nodes
}

// reconcileOrphan proves, under the parker's claim, that parker o.vmid is gone,
// and removes its resource and then its pin. A resource whose removal fails
// keeps its pin, so the next sweep finds the same pair.
func (r *parkerHA) reconcileOrphan(ctx context.Context, o parkerHAOrphan) {
	logger := r.logger.With(log.Int("parker_vmid", o.vmid))
	err := withVMIDClaim(ctx, r.deps, o.vmid, "parker-ha-orphan/"+strconv.Itoa(o.vmid), claimForSweep, logger,
		func(claimCtx context.Context) error {
			if err := r.proveParkerGone(claimCtx, o, logger); err != nil {
				return err
			}
			return r.removeOrphan(claimCtx, o, logger)
		})
	switch {
	case err == nil:
	case isVMIDClaimHeld(err):
		logger.Debug("parker HA: another request holds the parker's claim, so its HA leftovers wait for the next " +
			"sweep")
	default:
		logger.Info("parker HA: the HA leftovers of a parker no member lists stay, because the CPI could not "+
			"prove the parker gone or could not remove them; the next sweep tries again", log.Err(err))
	}
}

// proveParkerGone is the absence proof under the claim. The cluster must be
// quorate, and the index must list no guest at the VMID. Every node the pin
// names that is still a member must answer that the config does not exist,
// and must be a node the cluster reports able to answer. A probe of every
// member must then find the config nowhere, skip no member as offline, and
// see no HA source but the resource under removal hold the VMID.
func (r *parkerHA) proveParkerGone(ctx context.Context, o parkerHAOrphan, logger *log.Logger) error {
	quorum, err := pve.ReadClusterQuorum(ctx, r.deps.PVE.Cluster())
	if err != nil {
		return fmt.Errorf("%s: read the cluster's quorum: %w", parkerHAOrphanCaller, err)
	}
	if !quorum.Quorate {
		return cpierrors.Retriable("%s: the cluster is not quorate", parkerHAOrphanCaller)
	}
	guests, err := pve.ListIndexedGuests(ctx, r.deps.PVE)
	if err != nil {
		return err
	}
	for _, g := range guests {
		if g.VMID == o.vmid {
			return cpierrors.Cloud("%s: the cluster index lists vmid %d on node %s", parkerHAOrphanCaller,
				o.vmid, g.Node)
		}
	}
	nodes, err := r.pinMembers(ctx, o, logger)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		_, err := pve.ReadQemuViews(ctx, r.deps.PVE, node, o.vmid)
		switch {
		case err == nil:
			return cpierrors.Cloud("%s: node %s holds the config of vmid %d", parkerHAOrphanCaller, node, o.vmid)
		case pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err):
		default:
			return fmt.Errorf("%s: read the config of vmid %d on node %s: %w", parkerHAOrphanCaller, o.vmid,
				node, err)
		}
	}
	for _, node := range nodes {
		if err := requireProxyTarget(ctx, r.deps, parkerHAOrphanCaller, o.vmid, node); err != nil {
			return err
		}
	}
	var excluded []string
	opts := []pve.FindVMOption{
		pve.WithOfflineMemberTie(journalOfflineMemberTie(r.deps.Config)),
		pve.WithExcludedOfflineMembers(&excluded),
	}
	if o.resource {
		opts = append(opts, pve.WithHAResourceUnderRemoval("vm:"+strconv.Itoa(o.vmid)))
	}
	loc, err := pve.FindVMByConfigProbe(ctx, r.deps.PVE, o.vmid, opts...)
	if err != nil {
		return err
	}
	if loc.Found {
		return cpierrors.Cloud("%s: node %s holds the config of vmid %d", parkerHAOrphanCaller, loc.Node, o.vmid)
	}
	if len(excluded) > 0 {
		return cpierrors.Retriable("%s: the probe skipped members %v as offline, and the proof needs every "+
			"member's answer", parkerHAOrphanCaller, excluded)
	}
	return nil
}

// pinMembers returns the nodes the pin names that the proof must read. It
// leaves out only a node that both the cluster's member list and its status
// leave out, which is what a node removed from the cluster looks like. A
// config left in such a node's directory still shows in the cluster index,
// which the proof read first. Either read failing fails the proof, so a node
// is never skipped because the CPI could not tell whether it is a member.
func (r *parkerHA) pinMembers(ctx context.Context, o parkerHAOrphan, logger *log.Logger) ([]string, error) {
	if len(o.nodes) == 0 {
		return nil, nil
	}
	members, err := pve.ListClusterMemberNames(ctx, r.deps.PVE)
	if err != nil {
		return nil, fmt.Errorf("%s: list the cluster's members: %w", parkerHAOrphanCaller, err)
	}
	report, err := pve.NodeHealth(ctx, r.deps.PVE.Cluster(), r.deps.PVE.Nodes(), pve.NodeHealthOptions{SkipHA: true})
	if err != nil {
		return nil, fmt.Errorf("%s: read the cluster's status: %w", parkerHAOrphanCaller, err)
	}
	out := make([]string, 0, len(o.nodes))
	for _, node := range o.nodes {
		if _, listed := report.Node(node); slices.Contains(members, node) || listed {
			out = append(out, node)
			continue
		}
		logger.Debug("parker HA: the pin of a gone parker names a node that is no longer a cluster member, so the "+
			"proof does not read it", log.String("node", node))
	}
	return out, nil
}

// removeOrphan removes the parker's resource and then its pin. Each removal
// reads its object again and keeps one that no longer carries this
// Director's parker marker.
func (r *parkerHA) removeOrphan(ctx context.Context, o parkerHAOrphan, logger *log.Logger) error {
	sid := "vm:" + strconv.Itoa(o.vmid)
	if o.resource {
		err := removeOwnedHaResourceIf(ctx, r.deps.PVE.Cluster(), sid, r.owner, func(comment string) error {
			if !r.parkerMarked(comment) {
				return cpierrors.Cloud("%s: %s no longer carries this Director's parker marker", parkerHAOrphanCaller,
					sid)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("remove HA resource %s: %w", sid, err)
		}
		logger.Info("parker HA: removed the HA resource of a parker no member holds a config for",
			log.String("sid", sid))
	}
	if !o.pin {
		return nil
	}
	err := deleteNodeAffinityRule(ctx, r.deps, o.vmid, logger, func(cur *ha.Rule) error {
		if !r.parkerPin(cur, o.vmid) {
			return cpierrors.Cloud("%s: rule %s is no longer this Director's pin of parker vmid %d",
				parkerHAOrphanCaller, cur.ID, o.vmid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.Info("parker HA: removed the pin of a parker no member holds a config for",
		log.String("rule", naRuleNameFor(o.vmid)))
	return nil
}
