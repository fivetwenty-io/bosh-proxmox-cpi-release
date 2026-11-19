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
// A parker's HA resource and its pin can outlive the parker's config. A
// destroy that an operator ran by hand leaves both behind, and so does a
// retirement whose destroy landed but whose pin delete failed, which leaves
// the pin. PVE's CRM drops the service of a VM that has no config, so the
// leftover resource never fails over and nothing but this step removes it.
//
// The step runs at the end of the sweep's parker HA pass, and it removes a
// leftover only when it can prove both that the leftover is this Director's
// parker's and that the parker is gone. Ownership is the marker the
// registration writes: a resource carries this Director as owner and names no
// deployment or group, and a pin is the parker's bosh-na-<vmid> node-affinity
// rule with this Director's marker that names no other resource. Both name a
// VMID in this config's parker band, under the parked strategy, whose band no
// other VMID range may overlap. Absence is a cluster index that lists no guest
// at the VMID, a config read on every node the pin names that answers that the
// config does not exist, from nodes the cluster reports able to answer, and a
// probe of every member that finds the config nowhere while the CRM holds no
// service for the VMID. Any other answer keeps the leftover, and the doctor
// reports it.
//
// The node a resource last named is the pin's. The CRM's status names a node
// for the service only while it still holds the service, and while it holds
// it the proof refuses, because the CRM holds a service while a recovery
// moves its guest's config. Once the guest's config is gone the CRM drops the
// service, so a resource that has no pin names no node, and it stays for the
// doctor to report.
//
// The proof runs again under the parker's VMID claim, which every
// registration and retirement of the parker holds, taken with a single try.
// The create path writes a parker's config first, and only a registration
// under the claim writes its resource and its pin after that, so a parker
// that is still being created has a config the read under the claim finds,
// and its leftovers stay. PVE records no creation time on a resource or a
// rule, so that order is what keeps the step from racing a create.

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
// keeps its pin, so the next sweep finds the same pair and the pin still names
// the node to read.
func (r *parkerHA) reconcileOrphan(ctx context.Context, o parkerHAOrphan) {
	logger := r.logger.With(log.Int("parker_vmid", o.vmid))
	if len(o.nodes) == 0 {
		logger.Debug("parker HA: an HA resource names a parker no member lists, but no pin names a node to prove " +
			"the parker gone on, so it stays for the doctor to report")
		return
	}
	err := withVMIDClaim(ctx, r.deps, o.vmid, "parker-ha-orphan/"+strconv.Itoa(o.vmid), claimForSweep, logger,
		func(claimCtx context.Context) error {
			if err := r.proveParkerGone(claimCtx, o); err != nil {
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

// proveParkerGone is the absence proof under the claim: the index lists no
// guest at the VMID, every node the pin names answers that the config does
// not exist and is a node the cluster reports able to answer, and a probe of
// every member finds the config nowhere while no HA source but the resource
// under removal holds the VMID.
func (r *parkerHA) proveParkerGone(ctx context.Context, o parkerHAOrphan) error {
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
	for _, node := range o.nodes {
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
	for _, node := range o.nodes {
		if err := requireProxyTarget(ctx, r.deps, parkerHAOrphanCaller, o.vmid, node); err != nil {
			return err
		}
	}
	opts := []pve.FindVMOption{pve.WithOfflineMemberTie(journalOfflineMemberTie(r.deps.Config))}
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
	return nil
}

// removeOrphan removes the parker's resource and then its pin. Each removal
// reads its object again and keeps one that no longer carries this
// Director's parker marker.
func (r *parkerHA) removeOrphan(ctx context.Context, o parkerHAOrphan, logger *log.Logger) error {
	sid := "vm:" + strconv.Itoa(o.vmid)
	if o.resource {
		if err := removeOwnedHaResource(ctx, r.deps.PVE.Cluster(), sid, r.owner); err != nil {
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
