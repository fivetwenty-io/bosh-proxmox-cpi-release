package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The names the CPI writes onto PVE objects. They are spelled again here
// because the handlers keep them unexported, and the doctor must recognise
// exactly what the handlers wrote.
const (
	// haOwnershipTag marks a VM the CPI created.
	haOwnershipTag = "bosh-cpi"
	// haManagedTag marks a VM the CPI registered with PVE HA.
	haManagedTag = "bosh-ha-managed"
	// haReplicaTag marks a VM that the replicate local policy set up a
	// replication job for.
	haReplicaTag = "bosh-ha-replica"
	// haBirthTagPrefix starts the tag a create_vm writes until it finishes.
	haBirthTagPrefix = "bosh-creating-"
	// haMarkerPrefix opens the ownership marker in a rule, resource, or
	// replication job comment.
	haMarkerPrefix = "bosh-cpi:v1"
	// haPinRulePrefix starts the name of a node-affinity pin, bosh-na-<vmid>.
	haPinRulePrefix = "bosh-na-"
	// haGroupRulePrefix starts the name of an anti-affinity rule and of its
	// member tag, bosh-aa-<hash12>.
	haGroupRulePrefix = "bosh-aa-"
)

var (
	// haGroupTagPattern matches an anti-affinity member tag.
	haGroupTagPattern = regexp.MustCompile(`^bosh-aa-[0-9a-f]{12}$`)
	// haBirthTagPattern matches a birth tag and captures its Unix time.
	haBirthTagPattern = regexp.MustCompile(`^bosh-creating-(\d{1,19})$`)
	// haVolumeGuestPattern captures the VMID a guest volume's name carries.
	haVolumeGuestPattern = regexp.MustCompile(`^[^:]+:(?:[^/]+/)?vm-(\d+)-`)
)

// haRunsUnderHA says whether a requested state leaves the guest running under HA.
func haRunsUnderHA(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "started", "enabled":
		return true
	}
	return false
}

// haMarkerOwner reads the ownership marker at the start of a comment. ok is
// false when the comment holds none, which is how an object from an older CPI
// looks. owner is empty when the marker names no Director.
func haMarkerOwner(comment string) (owner string, ok bool) {
	fields := strings.Fields(comment)
	if len(fields) == 0 || fields[0] != haMarkerPrefix {
		return "", false
	}
	for _, f := range fields[1:] {
		if key, val, found := strings.Cut(f, "="); found && key == "owner" && val != "-" {
			owner = val
		}
	}
	return owner, true
}

// haJobMarker reports whether a replication job's comment holds the marker the
// CPI writes, which carries an owner and a positive generation. A comment
// without both is not a job the CPI can vouch for.
func haJobMarker(comment string) bool {
	fields := strings.Fields(comment)
	if len(fields) == 0 || fields[0] != haMarkerPrefix {
		return false
	}
	var owner, gen bool
	for _, f := range fields[1:] {
		key, val, found := strings.Cut(f, "=")
		if !found {
			continue
		}
		switch key {
		case "owner":
			owner = val != "" && val != "-"
		case "gen":
			n, err := strconv.ParseUint(val, 10, 64)
			gen = err == nil && n > 0
		}
	}
	return owner && gen
}

// haGuest is a cluster guest with the facts the doctor derives from it.
type haGuest struct {
	ref      pve.GuestRef
	memBytes int64
	tags     map[string]bool
	owned    bool
	sid      string
	resource *HAResourceRow
	cfg      map[string]any
	volumes  []ha.VMVolume
}

func (g *haGuest) hasTag(tag string) bool { return g.tags[tag] }

// tagsWithPrefix returns the guest's tags that start with prefix, sorted.
func (g *haGuest) tagsWithPrefix(prefix string) []string {
	var out []string
	for t := range g.tags {
		if strings.HasPrefix(t, prefix) {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// haCollector gathers one cluster's report. Every read goes through the
// HAReader, and a read that fails becomes an unchecked finding instead of an
// absent one.
type haCollector struct {
	ctx   context.Context
	r     HAReader
	cfg   *config.CPIConfig
	owner string
	now   time.Time
	rep   *haReport
	p     *ha.Profile

	resources   map[string]HAResourceRow
	resourcesOK bool
	rules       []ha.Rule
	rulesOK     bool
	guests      []*haGuest
	byVMID      map[int]*haGuest
	guestsOK    bool
	storages    map[string]pve.StorageInfo
	storagesOK  bool
	jobs        []ha.ReplicaJobState
	jobsOK      bool
	jobStatus   map[string]ha.ReplicaSyncStatus
	schedules   map[string]string
	schedErr    error
	awaiting    map[int]bool
	journal     JournalView
	journalOK   bool
	memoryErr   error

	// euid is the doctor's own effective user, which the cache directory
	// checks compare with the CPI's user.
	euid int
}

// collectHA reads the cluster r reaches and builds the doctor's report. cfg is
// the effective CPI config for the cluster, owner is the requesting Director's
// UUID when the run has one, and now is the clock every age is measured on.
func collectHA(ctx context.Context, r HAReader, cfg *config.CPIConfig, owner string, now time.Time) *haReport {
	c := &haCollector{
		ctx: ctx, r: r, cfg: cfg, owner: owner, now: now, euid: os.Geteuid(),
		rep:    &haReport{Mode: cfg.HAModeValue(), Nodes: []haNode{}, CPUDomains: []haDomain{}, Findings: []haFinding{}},
		byVMID: map[int]*haGuest{}, jobStatus: map[string]ha.ReplicaSyncStatus{}, awaiting: map[int]bool{},
	}
	p, err := r.Profile(ctx)
	if err != nil || p == nil {
		reason := "no profile returned"
		if err != nil {
			reason = err.Error()
		}
		c.rep.Profile = "ha: mode=" + c.rep.Mode + " effective=unknown"
		c.rep.Effective = "unknown"
		c.rep.Headroom = "unavailable"
		c.rep.unchecked("profile", "the HA profile could not be resolved: %s", reason)
		return c.rep
	}
	c.p = p
	c.fillProfile()
	c.readSources()
	c.profileFindings()
	c.ruleFindings()
	c.antiAffinityFindings()
	c.cpuFindings()
	c.protectionFindings()
	c.replicationFindings()
	c.headroomFindings()
	c.leftoverFindings()
	c.cacheDirectoryFindings()
	return c.rep
}

// healthEntry returns the health entry of a node. It scans the report's node
// list rather than the report's lookup, so it reads the same list the doctor
// prints.
func healthEntry(p *ha.Profile, name string) (pve.NodeHealthEntry, bool) {
	if p.Health == nil {
		return pve.NodeHealthEntry{}, false
	}
	for _, e := range p.Health.Nodes {
		if e.Name == name {
			return e, true
		}
	}
	return pve.NodeHealthEntry{}, false
}

// off reports whether the configured mode is off.
func (c *haCollector) off() bool { return c.p.Mode == config.HAModeOff }

// fillProfile copies the profile into the report's header, node lines, and CPU
// domains.
func (c *haCollector) fillProfile() {
	p := c.p
	c.rep.Mode, c.rep.Effective, c.rep.Reason = p.Mode, string(p.Effective), p.Reason
	for i := range p.Nodes {
		n := &p.Nodes[i]
		line := haNode{Name: n.Name, Online: n.Online, Version: n.Version, CPU: n.CPU.Model}
		if n.VotesKnown {
			votes := n.Votes
			line.Votes = &votes
		}
		if h, ok := healthEntry(p, n.Name); ok {
			line.Health = fmt.Sprint(h.Health)
			line.Placeable, line.Maintenance = h.Placeable, h.Maintenance
			line.Watchdog, line.Reason = fmt.Sprint(h.Watchdog), h.Reason
		}
		c.rep.Nodes = append(c.rep.Nodes, line)
	}
	for _, d := range p.CPUDomains {
		c.rep.CPUDomains = append(c.rep.CPUDomains, haDomain{Model: d.Model, Flags: d.Flags, Nodes: d.Nodes})
	}
	c.rep.UnknownCPUNodes = append(c.rep.UnknownCPUNodes, p.UnknownCPUNodes...)
}

// readSources reads the cluster objects the later checks share.
func (c *haCollector) readSources() {
	c.readResources()
	c.readRules()
	c.readGuests()
	c.readStorages()
	c.readJobs()
	c.readJournal()
}

func (c *haCollector) readResources() {
	rows, err := c.r.Resources(c.ctx)
	if err != nil {
		c.rep.unchecked("ha-resources", "the HA resources could not be read, so leftovers and unprotected VMs are unchecked: %v", err)
		return
	}
	c.resources = make(map[string]HAResourceRow, len(rows))
	for _, row := range rows {
		c.resources[row.SID] = row
	}
	c.resourcesOK = true
}

func (c *haCollector) readRules() {
	rules, err := c.r.Rules(c.ctx)
	if err != nil {
		why := "so rule problems, pins, and rule leftovers are unchecked"
		if !c.p.RulesSupported {
			why += " (the HA rules API needs PVE 9.0 or later, and a refused or timed-out read looks the same)"
		}
		c.rep.unchecked("ha-rules", "the HA rules could not be read, %s: %v", why, err)
		return
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	c.rules, c.rulesOK = rules, true
}

func (c *haCollector) readGuests() {
	refs, excluded, err := c.r.Guests(c.ctx)
	if err != nil {
		c.rep.unchecked("guests", "the cluster's guests could not be listed, so every VM check is unchecked: %v", err)
		return
	}
	if len(excluded) > 0 {
		c.rep.advise(haKindGuestsNotListed, strings.Join(excluded, ","),
			"these offline nodes could not be asked, so their guests are missing from every VM check")
	}
	memory, memErr := c.r.GuestMemory(c.ctx)
	sort.Slice(refs, func(i, j int) bool { return refs[i].VMID < refs[j].VMID })
	for _, ref := range refs {
		g := &haGuest{ref: ref, tags: map[string]bool{}, sid: "vm:" + strconv.Itoa(ref.VMID)}
		for _, t := range ref.SplitTags() {
			g.tags[strings.ToLower(strings.TrimSpace(t))] = true
		}
		g.owned = g.hasTag(haOwnershipTag)
		g.memBytes = memory[ref.VMID]
		if row, ok := c.resources[g.sid]; ok {
			g.resource = &row
		}
		c.guests = append(c.guests, g)
		c.byVMID[ref.VMID] = g
	}
	c.guestsOK = true
	c.memoryErr = memErr
	c.readConfigs()
}

// readConfigs reads the config of every VM the CPI owns that is not a
// template, because the CPU type and the volumes a VM holds come from it.
func (c *haCollector) readConfigs() {
	for _, g := range c.guests {
		if !g.owned || g.ref.Template {
			continue
		}
		cfg, err := c.r.GuestConfig(c.ctx, g.ref.Node, g.ref.VMID)
		if err != nil {
			c.rep.unchecked(g.sid, "the config of VM %d on %s could not be read, so its CPU type and volumes are unchecked: %v",
				g.ref.VMID, g.ref.Node, err)
			continue
		}
		g.cfg = cfg
	}
}

func (c *haCollector) readStorages() {
	list, err := c.r.Storages(c.ctx)
	if err != nil {
		c.rep.unchecked("storages", "the storage index could not be read, so node-local volumes are unchecked: %v", err)
		return
	}
	c.storages = make(map[string]pve.StorageInfo, len(list))
	for i := range list {
		c.storages[list[i].Name] = list[i]
	}
	c.storagesOK = true
	for _, g := range c.guests {
		if g.cfg != nil {
			g.volumes = volumesOfConfig(g.cfg, g.ref.Node)
		}
	}
}

func (c *haCollector) readJobs() {
	jobs, err := c.r.ReplicationJobs(c.ctx)
	if err != nil {
		c.rep.unchecked("replication", "the replication jobs could not be read, so replication state is unchecked: %v", err)
		return
	}
	c.jobs, c.jobsOK = jobs, true
}

func (c *haCollector) readJournal() {
	nodes := make([]string, 0, len(c.p.Nodes))
	for i := range c.p.Nodes {
		nodes = append(nodes, c.p.Nodes[i].Name)
	}
	view, err := c.r.Journal(c.ctx, nodes)
	if err != nil {
		c.rep.unchecked("journal", "the allocation journal could not be read, so recorded replica orphans are unchecked: %v", err)
		return
	}
	c.journal, c.journalOK = view, true
}

// volumesOfConfig lists the volumes a VM config references as the HA decision
// reads them: a CD-ROM is an ISO, and a drive with replicate=0 is one PVE
// replication skips.
func volumesOfConfig(cfg map[string]any, node string) []ha.VMVolume {
	access := pve.ConfigAccessVolumes(cfg, node)
	opts := map[string]map[string]string{}
	for _, raw := range cfg {
		text, ok := pve.ConfigStringValue(raw)
		if !ok || !strings.Contains(text, ":") {
			continue
		}
		parts := strings.Split(text, ",")
		volid := strings.TrimSpace(strings.TrimPrefix(parts[0], "file="))
		o := map[string]string{}
		for _, part := range parts[1:] {
			if k, v, found := strings.Cut(strings.TrimSpace(part), "="); found {
				if k == "file" {
					volid = strings.TrimSpace(v)
					continue
				}
				o[k] = strings.TrimSpace(v)
			}
		}
		if volid != "" {
			opts[volid] = o
		}
	}
	out := make([]ha.VMVolume, 0, len(access))
	for _, v := range access {
		vol := ha.VMVolume{Volume: v.Volume, HomeNode: v.HomeNode, Kind: ha.VolumeDisk}
		o := opts[v.Volume]
		if o["media"] == "cdrom" {
			vol.Kind = ha.VolumeISO
		}
		if o["replicate"] == "0" {
			vol.NoReplicate = true
		}
		out = append(out, vol)
	}
	return out
}

// localVolumes returns the disk volumes of g that sit on node-local storage.
func (c *haCollector) localVolumes(g *haGuest) []string {
	if !c.storagesOK {
		return nil
	}
	var out []string
	for _, v := range g.volumes {
		if v.Kind == ha.VolumeISO {
			continue
		}
		name, _, ok := strings.Cut(v.Volume, ":")
		info, known := c.storages[name]
		if ok && known && !info.IsShared() {
			out = append(out, v.Volume)
		}
	}
	sort.Strings(out)
	return out
}

// pinOf returns the node-affinity pin rule of vmid.
func (c *haCollector) pinOf(vmid int) *ha.Rule {
	name := haPinRulePrefix + strconv.Itoa(vmid)
	for i := range c.rules {
		if c.rules[i].ID == name {
			return &c.rules[i]
		}
	}
	return nil
}

// profileFindings reports what the profile itself shows: an observe-only
// state, a down node, quorum risk, the QDevice recommendation, and a CRS mode
// that leaves DLB without effect.
func (c *haCollector) profileFindings() {
	p, rep := c.p, c.rep
	headline := p.LogLine()
	rep.Profile = headline
	if !c.off() && p.Effective == ha.EffectiveObserveOnly {
		reason := p.Reason
		if reason == "" {
			reason = "a prerequisite is not met"
		}
		detail := "the profile is observe-only, so the CPI registers nothing and writes no rules: " + reason
		if len(p.Gaps) > 1 {
			detail += " (all gaps: " + strings.Join(p.Gaps, "; ") + ")"
		}
		rep.problem(haKindObserveOnly, "", "%s", detail)
	}
	if p.Health != nil {
		for _, n := range p.Health.Nodes {
			if n.DeadOrFenced {
				rep.problem(haKindNodeDown, n.Name, "the node is offline, unknown, fenced, or gone: %s", n.Reason)
			}
		}
	} else if p.HealthErr != "" {
		rep.unchecked("node-health", "node health could not be built: %s", p.HealthErr)
	}
	if p.Quorum.OneFailureFromLoss() {
		for _, name := range p.Quorum.FragileNodes {
			rep.problem(haKindQuorumFragile, name,
				"losing this node would leave the survivors without quorum (%d of %d votes participate now)",
				p.Quorum.Participating, p.Quorum.Expected)
		}
	}
	if strings.HasPrefix(p.Advice, "add-qdevice") {
		rep.advise(haKindQDeviceAdvice, "", "%s; a QDevice breaks an even split", p.Advice)
	} else if p.Advice != "" {
		rep.advise(haKindAdvice, "", "%s", p.Advice)
	}
	if c.cfg.HADLBEnabledValue() && p.CRS.Known {
		switch {
		case p.CRS.Mode == "basic":
			rep.advise(haKindCRSMode, "",
				"pve.ha.dlb.enabled is on, but the cluster's CRS mode is basic, so the dynamic load balancer has no effect; the CPI never writes CRS")
		case !p.CRS.AutoRebalance:
			rep.advise(haKindCRSMode, "",
				"pve.ha.dlb.enabled is on, but the cluster's ha-auto-rebalance is off, so nothing rebalances; the CPI never writes CRS")
		}
	}
}

// ruleMarked reports whether a rule carries the CPI's ownership marker.
func ruleMarked(r *ha.Rule) bool {
	_, ok := haMarkerOwner(r.Comment)
	return ok
}

// foreign reports whether a comment's marker names another Director than the
// one the run speaks for. A run with no Director UUID treats every marker as
// its own, and so does a marker that names no owner.
func (c *haCollector) foreign(comment string) bool {
	owner, ok := haMarkerOwner(comment)
	return ok && c.owner != "" && owner != "" && owner != c.owner
}

// cpiRuleName reports whether a rule's name is one the CPI gives rules.
func cpiRuleName(id string) bool {
	return strings.HasPrefix(id, haPinRulePrefix) || strings.HasPrefix(id, haGroupRulePrefix)
}

// ruleFindings reports CPI rules that PVE disabled or marked conflicting, and
// the unmarked rules an older CPI left.
func (c *haCollector) ruleFindings() {
	if !c.rulesOK {
		return
	}
	for i := range c.rules {
		r := &c.rules[i]
		marked := ruleMarked(r)
		if (!marked && !cpiRuleName(r.ID)) || c.foreign(r.Comment) {
			continue
		}
		if !marked {
			c.rep.problem(haKindRuleUnmarked, r.ID,
				"the rule has a CPI name but no ownership marker, so it comes from an older CPI that the current one will not edit")
		}
		if r.Disabled {
			c.rep.problem(haKindRuleProblem, r.ID, "PVE has this rule disabled, so it constrains nothing")
		}
		if len(r.Errors) > 0 {
			c.rep.problem(haKindRuleProblem, r.ID, "PVE marks this rule conflicting: %s", strings.Join(r.Errors, "; "))
		}
	}
}

// antiAffinityFindings reports each anti-affinity group that overflowed its
// cluster, and each group with a member its rule does not name.
func (c *haCollector) antiAffinityFindings() {
	if !c.guestsOK || !c.rulesOK {
		return
	}
	groups := map[string][]*haGuest{}
	for _, g := range c.guests {
		if !g.owned {
			continue
		}
		for t := range g.tags {
			if haGroupTagPattern.MatchString(t) {
				groups[t] = append(groups[t], g)
			}
		}
	}
	tags := make([]string, 0, len(groups))
	for t := range groups {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		c.checkGroup(tag, groups[tag], c.groupRules(tag))
	}
}

// groupRules returns the rules that carry a group's name: the rule itself and
// the numbered rules an overflow split adds.
func (c *haCollector) groupRules(tag string) []*ha.Rule {
	var rules []*ha.Rule
	for i := range c.rules {
		id := c.rules[i].ID
		if id == tag || strings.HasPrefix(id, tag+"-") && isDigits(strings.TrimPrefix(id, tag+"-")) {
			rules = append(rules, &c.rules[i])
		}
	}
	return rules
}

// checkGroup judges one group against its rules and the cluster's node count.
func (c *haCollector) checkGroup(tag string, members []*haGuest, rules []*ha.Rule) {
	nodes := len(c.p.Nodes)
	if nodes > 0 && len(members) > nodes {
		if len(rules) == 0 {
			c.rep.advise(haKindAntiAffinitySkipped, tag,
				"the group has %d members but the cluster has %d nodes, so skip wrote no rule", len(members), nodes)
		} else {
			c.rep.advise(haKindAntiAffinitySplit, tag,
				"the group has %d members but the cluster has %d nodes, so it is split across %d rules",
				len(members), nodes, len(rules))
		}
		return
	}
	if len(members) < 2 || !c.resourcesOK {
		return
	}
	var missing []string
	for _, g := range members {
		if g.resource != nil && !namedByAny(rules, g.sid) {
			missing = append(missing, g.sid)
		}
	}
	if len(missing) > 0 {
		c.rep.problem(haKindAntiAffinityShort, tag,
			"the group's rule does not name %s, so the group is short of a member until a later create_vm adds it",
			strings.Join(missing, ", "))
	}
}

// namedByAny reports whether any of the rules names sid.
func namedByAny(rules []*ha.Rule, sid string) bool {
	for _, r := range rules {
		if r.HasResource(sid) {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cpuFindings reports nodes whose CPU is unreadable, VMs whose host or max CPU
// type confines them to one domain, and VMs that got no HA because their
// domain is too small.
func (c *haCollector) cpuFindings() {
	if len(c.p.UnknownCPUNodes) > 0 {
		c.rep.advise(haKindCPUUnknown, strings.Join(c.p.UnknownCPUNodes, ","),
			"the CPU model of these nodes could not be read, so they belong to no migration domain")
	}
	if !c.guestsOK {
		return
	}
	for _, g := range c.guests {
		if !g.owned || g.ref.Template || g.cfg == nil {
			continue
		}
		cpu, _ := pve.ConfigStringValue(g.cfg["cpu"])
		if !ha.CPUTypeNeedsDomain(cpu) {
			continue
		}
		dom, known := c.p.DomainOf(g.ref.Node)
		if len(c.p.CPUDomains) > 1 {
			where := "a domain the CPU of node " + g.ref.Node + " cannot place"
			if known {
				where = "the domain " + strings.Join(dom.Nodes, ", ")
			}
			c.rep.advise(haKindCPUHostMulti, g.sid,
				"CPU type %s on a cluster with %d CPU migration domains confines the VM's pin to %s; set pve.cpu_type to a named model to let it move",
				cpu, len(c.p.CPUDomains), where)
		}
		if !c.off() && c.p.Effective != ha.EffectiveObserveOnly && c.resourcesOK && g.resource == nil && !c.inFlight(g) &&
			(!known || len(dom.Nodes) < 2) {
			why := "the CPU of node " + g.ref.Node + " is unknown"
			if known {
				why = fmt.Sprintf("its CPU migration domain holds %d node(s)", len(dom.Nodes))
			}
			c.rep.problem(haKindCPUDomainTooSmall, g.sid,
				"the VM has CPU type %s and got no HA, because %s; set pve.cpu_type to a named model", cpu, why)
		}
	}
}

// pinAllowsNode reports whether a pin is an enabled strict rule that names the
// VM and the node it runs on.
func pinAllowsNode(pin *ha.Rule, sid, node string) (bool, string) {
	switch {
	case pin == nil:
		return false, "no confinement pin exists"
	case pin.Disabled:
		return false, "the pin " + pin.ID + " is disabled"
	case !pin.Strict:
		return false, "the pin " + pin.ID + " is not strict"
	case !pin.HasResource(sid):
		return false, "the pin " + pin.ID + " does not name " + sid
	}
	if _, ok := pin.Nodes[node]; !ok {
		return false, "the pin " + pin.ID + " does not name node " + node
	}
	return true, ""
}

// protectionFindings reports local-disk VMs that run unprotected, VMs whose
// confinement pin is gone, and hold VMs on a node entering maintenance.
func (c *haCollector) protectionFindings() {
	if c.off() || !c.guestsOK || !c.resourcesOK || !c.storagesOK {
		return
	}
	policy := c.cfg.HALocalPolicyValue()
	for _, g := range c.guests {
		if !g.owned || g.ref.Template || g.cfg == nil {
			continue
		}
		local := c.localVolumes(g)
		if len(local) == 0 {
			continue
		}
		if g.resource == nil {
			if c.inFlight(g) {
				continue
			}
			detail := fmt.Sprintf("VM %d on %s holds node-local volumes (%s) and runs without HA; pve.ha.local_policy is %s",
				g.ref.VMID, g.ref.Node, strings.Join(local, ", "), policy)
			if policy == config.HALocalPolicyNone {
				c.rep.advise(haKindLocalUnprotected, g.sid, "%s", detail)
			} else {
				c.rep.problem(haKindLocalUnprotected, g.sid, "%s", detail)
			}
			continue
		}
		if !c.rulesOK || !g.hasTag(haManagedTag) {
			continue
		}
		pin := c.pinOf(g.ref.VMID)
		ok, why := pinAllowsNode(pin, g.sid, g.ref.Node)
		if !ok {
			c.rep.problem(haKindPinMissing, g.sid,
				"VM %d holds node-local volumes but its confinement pin is not in force: %s", g.ref.VMID, why)
			continue
		}
		if len(pin.Nodes) == 1 && !c.replicates(g.ref.VMID, pin) {
			if c.p.Health != nil {
				if h, found := healthEntry(c.p, g.ref.Node); found && h.Maintenance {
					c.rep.problem(haKindHoldMaintenance, g.sid,
						"VM %d is held on %s, which is entering maintenance, and its node-local volumes keep it from moving; PVE cannot drain the node until the VM is stopped",
						g.ref.VMID, g.ref.Node)
				}
			}
		}
	}
}

// inFlight reports whether g carries a birth tag younger than the sweep's
// threshold, which means a create is still running and the VM is not yet
// expected to be under HA.
func (c *haCollector) inFlight(g *haGuest) bool {
	for _, t := range g.tagsWithPrefix(haBirthTagPrefix) {
		if born, ok := birthTagTime(t); ok && c.now.Sub(born) <= c.birthTagAge() {
			return true
		}
	}
	return false
}

// birthTagAge is how old a birth tag must be before the sweep looks at its VM:
// the create timeout plus the HA motion wait. A re-registration tag gets the
// same allowance, because the order that writes it waits on the same motion.
func (c *haCollector) birthTagAge() time.Duration {
	return time.Duration(c.cfg.OperationTimeoutCreateSec()+c.cfg.HAMotionWaitSecValue()) * time.Second
}

func birthTagTime(tag string) (time.Time, bool) {
	m := haBirthTagPattern.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// replicates reports whether vmid is a replicate VM: its pin names a replica
// partner, or a replication job the CPI marked belongs to it.
func (c *haCollector) replicates(vmid int, pin *ha.Rule) bool {
	if pin != nil {
		if _, ok := ha.ReplicaPartner(pin.Comment); ok {
			return true
		}
	}
	for _, j := range c.markedJobs() {
		if j.Guest == vmid {
			return true
		}
	}
	return false
}

// markedJobs returns the usable replication jobs that carry the CPI's marker,
// are not being removed, and do not belong to another Director.
func (c *haCollector) markedJobs() []ha.ReplicaJobState {
	var out []ha.ReplicaJobState
	for _, j := range c.jobs {
		if j.Fault == "" && j.RemoveJob == "" && haJobMarker(j.Comment) && !c.foreign(j.Comment) {
			out = append(out, j)
		}
	}
	return out
}

// replicationFindings reports replicate VMs that run on their partner or are
// not recoverable, jobs that are stale or failing, replica volumes no job
// covers, and the orphan copies a forced retirement recorded.
func (c *haCollector) replicationFindings() {
	c.readReplicaStatuses()
	c.recoveryState()
	c.jobHealth()
	c.orphanVolumes()
	c.recordedCopies()
}

// readReplicaStatuses reads the schedule and the status of each CPI
// replication job once, for the checks that judge a job's syncs.
func (c *haCollector) readReplicaStatuses() {
	if !c.jobsOK {
		return
	}
	c.schedules, c.schedErr = c.r.ReplicationSchedules(c.ctx)
	if c.schedErr != nil {
		c.rep.unchecked("replication-schedules",
			"the replication schedules could not be read, so stale syncs cannot be judged: %v", c.schedErr)
	}
	for _, j := range c.markedJobs() {
		if j.Source == "" {
			continue
		}
		st, err := c.r.ReplicaStatus(c.ctx, j.Source, j.ID)
		if err != nil {
			c.rep.unchecked("job "+j.ID, "the status of replication job %s on %s could not be read: %v", j.ID, j.Source, err)
			continue
		}
		c.jobStatus[j.ID] = st
	}
}

// jobHealth checks each CPI replication job's last sync and failure count.
func (c *haCollector) jobHealth() {
	if !c.jobsOK {
		return
	}
	for _, j := range c.markedJobs() {
		st, ok := c.jobStatus[j.ID]
		if !ok {
			continue
		}
		if st.FailCount > 0 {
			detail := fmt.Sprintf("replication job %s (%s to %s) has failed %d time(s) in a row", j.ID, j.Source, j.Target, st.FailCount)
			if st.Error != "" {
				detail += ": " + st.Error
			}
			c.rep.problem(haKindReplicaJobFailing, "job "+j.ID, "%s", detail)
		}
		if c.schedErr != nil {
			continue
		}
		schedule := c.schedules[j.ID]
		interval, judged := scheduleInterval(schedule)
		switch {
		case st.LastSync.IsZero():
			if !c.awaiting[j.Guest] {
				c.rep.advise(haKindReplicaJobStale, "job "+j.ID, "replication job %s has not completed a sync yet", j.ID)
			}
		case !judged:
			c.rep.advise(haKindReplicaScheduleSkip, "job "+j.ID,
				"the schedule %q of replication job %s is not one the doctor can measure staleness against", schedule, j.ID)
		case c.now.Sub(st.LastSync) > 2*interval:
			c.rep.problem(haKindReplicaJobStale, "job "+j.ID,
				"the last successful sync of replication job %s was %s ago, more than twice its %s schedule (%s)",
				j.ID, c.now.Sub(st.LastSync).Round(time.Second), interval, schedule)
		}
	}
}

// journalRecord returns the journal record of the replicated VM vmid.
func (c *haCollector) journalRecord(vmid int) *aj.Record {
	for i := range c.journal.Records {
		rep := c.journal.Records[i].Replication
		if rep == nil {
			continue
		}
		if (rep.Job != nil && rep.Job.Guest == vmid) || (rep.PendingJob != nil && rep.PendingJob.Guest == vmid) {
			return &c.journal.Records[i]
		}
	}
	return nil
}

// replicaVMs returns the VMIDs of the VMs that replicate: those a CPI-marked
// job names, those whose journal record holds a live job, and those that carry
// the replicate tag, so a VM whose job is gone is still found.
func (c *haCollector) replicaVMs() []int {
	vmids := map[int]bool{}
	for _, j := range c.markedJobs() {
		vmids[j.Guest] = true
	}
	for i := range c.journal.Records {
		rep := c.journal.Records[i].Replication
		if rep != nil && rep.Job != nil && !rep.Retired {
			vmids[rep.Job.Guest] = true
		}
	}
	for _, g := range c.guests {
		if g.owned && !g.ref.Template && g.hasTag(haReplicaTag) {
			vmids[g.ref.VMID] = true
		}
	}
	ids := make([]int, 0, len(vmids))
	for id := range vmids {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// liveJob returns the CPI-marked replication job of vmid that is still in use.
func (c *haCollector) liveJob(vmid int) *ha.ReplicaJobState {
	jobs := c.markedJobs()
	for i := range jobs {
		if jobs[i].Guest == vmid {
			return &jobs[i]
		}
	}
	return nil
}

// replicaTagged reports whether any CPI-owned VM carries the replicate tag.
func (c *haCollector) replicaTagged() bool {
	for _, g := range c.guests {
		if g.owned && !g.ref.Template && g.hasTag(haReplicaTag) {
			return true
		}
	}
	return false
}

// recoveryState reports each replicate VM that runs on its partner, and each
// that PVE could not yet recover from its partner. A VM whose job is still
// working toward the sync that lets the CPI widen its pin is awaiting that
// sync, which is advice, and only a VM with no job that can produce the sync,
// or whose first sync is overdue, is not recoverable.
func (c *haCollector) recoveryState() {
	if !c.guestsOK {
		return
	}
	for _, id := range c.replicaVMs() {
		g, ok := c.byVMID[id]
		if !ok {
			continue
		}
		pin := c.pinOf(id)
		token := ""
		if pin != nil {
			token, _ = ha.ReplicaPartner(pin.Comment)
		}
		rec := c.journalRecord(id)
		partner, source := token, ""
		if rec != nil && rec.Replication.Job != nil {
			partner, source = rec.Replication.Job.Target, rec.Replication.Job.Source
		}
		if partner != "" && g.ref.Node == partner {
			back := ""
			if source != "" {
				back = fmt.Sprintf("; PVE's next run copies from %s back to %s and rolls %s's copy back to match", partner, source, source)
			}
			c.rep.problem(haKindReplicaOnPartner, g.sid,
				"replicate VM %d runs on its partner %s after a recovery%s", id, partner, back)
		}
		recoverable := token != ""
		if rec != nil {
			recoverable = rec.Replication.Recoverable()
		}
		if recoverable || c.inFlight(g) || rec != nil && rec.Replication.Retired {
			continue
		}
		c.notRecoverable(g, pin, rec)
	}
}

// notRecoverable judges a replicate VM whose pin does not yet record a
// verified replica. create_vm does not wait for the first sync, and the pin
// widens on a later reboot_vm or set_vm_metadata once a sync that started
// after the last volume change is verified, so a VM with a working job is only
// awaiting that sync.
func (c *haCollector) notRecoverable(g *haGuest, pin *ha.Rule, rec *aj.Record) {
	id := g.ref.VMID
	if !c.jobsOK {
		return
	}
	job := c.liveJob(id)
	switch {
	case job == nil:
		c.rep.problem(haKindReplicaNotRecover, g.sid,
			"replicate VM %d is configured to replicate but no replication job the CPI marked exists for it, so no replica is kept", id)
		return
	case job.Disabled:
		c.rep.problem(haKindReplicaNotRecover, g.sid,
			"replicate VM %d is configured to replicate but its replication job %s is disabled, so no replica is kept", id, job.ID)
		return
	}
	st, read := c.jobStatus[job.ID]
	if !read {
		return
	}
	changed, haveChange := c.volumeChange(pin, rec)
	const widens = "; the pin widens to the partner on the next reboot_vm or set_vm_metadata call once a sync is verified"
	switch {
	case !st.LastSync.IsZero() && (!haveChange || st.LastSync.After(changed.Add(aj.ReplicaClockSkewTolerance))):
		c.awaiting[id] = true
		c.rep.advise(haKindReplicaAwaiting, g.sid,
			"replicate VM %d: job %s synced at %s, but the pin does not yet name the partner as holding a replica%s",
			id, job.ID, st.LastSync.UTC().Format(time.RFC3339), widens)
	case c.firstSyncOverdue(job.ID, changed, haveChange):
		c.rep.problem(haKindReplicaNotRecover, g.sid,
			"replicate VM %d is configured to replicate but job %s has not completed a sync that started after the volume change at %s, "+
				"more than twice its schedule since", id, job.ID, changed.UTC().Format(time.RFC3339))
	default:
		c.awaiting[id] = true
		what := "has not completed its first sync"
		if !st.LastSync.IsZero() {
			what = "has not completed a sync that started after the last volume change"
		}
		c.rep.advise(haKindReplicaAwaiting, g.sid, "replicate VM %d is awaiting its first sync: job %s %s%s", id, job.ID, what, widens)
	}
}

// volumeChange returns when the VM's volume set last changed, from the
// journal record when it holds one and otherwise from the pin's token.
func (c *haCollector) volumeChange(pin *ha.Rule, rec *aj.Record) (time.Time, bool) {
	if rec != nil {
		if set, ok := rec.Replication.LatestVolumeSet(); ok && !set.ChangedAt.IsZero() {
			return set.ChangedAt, true
		}
	}
	if pin != nil {
		return ha.ReplicaChanged(pin.Comment)
	}
	return time.Time{}, false
}

// firstSyncOverdue reports whether a job still owes the sync that follows a
// volume change, more than twice its schedule interval plus the clock-skew
// tolerance after the change. A schedule the doctor cannot measure never
// makes a job overdue.
func (c *haCollector) firstSyncOverdue(jobID string, changed time.Time, haveChange bool) bool {
	if !haveChange || c.schedErr != nil {
		return false
	}
	interval, judged := scheduleInterval(c.schedules[jobID])
	return judged && c.now.Sub(changed) > 2*interval+aj.ReplicaClockSkewTolerance
}

// orphanVolumes lists replica volumes that sit on a node other than their
// VM's and that no replication job targets.
func (c *haCollector) orphanVolumes() {
	if !c.guestsOK || !c.storagesOK || !c.jobsOK {
		return
	}
	want := c.cfg.HALocalPolicyValue() == config.HALocalPolicyReplicate || len(c.jobs) > 0 || c.replicaTagged()
	if !want {
		return
	}
	var storages []pve.StorageInfo
	for name := range c.storages {
		if s := c.storages[name]; s.Type == pve.StorageTypeZFSPool && !s.Disabled && !s.IsShared() {
			storages = append(storages, s)
		}
	}
	sort.Slice(storages, func(i, j int) bool { return storages[i].Name < storages[j].Name })
	covered := map[string]bool{}
	for _, j := range c.jobs {
		if j.Fault == "" {
			covered[strconv.Itoa(j.Guest)+"@"+j.Target] = true
		}
	}
	for i := range c.p.Nodes {
		node := c.p.Nodes[i]
		if !node.Online {
			continue
		}
		for si := range storages {
			s := &storages[si]
			if len(s.Nodes) > 0 && !containsString(s.Nodes, node.Name) {
				continue
			}
			volumes, err := c.r.StorageVolumes(c.ctx, node.Name, s.Name)
			if err != nil {
				c.rep.unchecked("storage "+s.Name+"@"+node.Name,
					"the volumes of %s on %s could not be listed, so replica orphans there are unchecked: %v", s.Name, node.Name, err)
				continue
			}
			ids := make([]string, 0, len(volumes))
			for volid := range volumes {
				ids = append(ids, volid)
			}
			sort.Strings(ids)
			for _, volid := range ids {
				m := haVolumeGuestPattern.FindStringSubmatch(volid)
				if m == nil {
					continue
				}
				vmid, _ := strconv.Atoi(m[1])
				g, ok := c.byVMID[vmid]
				if !ok || !g.owned || g.ref.Node == node.Name || covered[m[1]+"@"+node.Name] {
					continue
				}
				c.rep.problem(haKindReplicaOrphanVolume, volid,
					"%s on %s is a copy of VM %d, which runs on %s, and no replication job targets %s for it; remove it by hand once nothing needs it",
					volid, node.Name, vmid, g.ref.Node, node.Name)
			}
		}
	}
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// recordedCopies lists the copies the journal recorded after a retirement,
// which no live job keeps current, and says so when there is no journal to read.
func (c *haCollector) recordedCopies() {
	if !c.journalOK {
		return
	}
	if !c.journal.Configured {
		if len(c.markedJobs()) > 0 || c.cfg.HALocalPolicyValue() == config.HALocalPolicyReplicate || c.replicaTagged() {
			c.rep.advise(haKindReplicaNoJournal, "",
				"no allocation journal is configured, so the orphan copies a forced retirement records are unchecked")
		}
		return
	}
	for i := range c.journal.Records {
		rec := &c.journal.Records[i]
		if rec.Replication == nil {
			continue
		}
		for _, cp := range rec.Replication.Copies {
			subject := fmt.Sprintf("%s@%s", strings.Join(cp.VolIDs, ","), cp.Node)
			detail := fmt.Sprintf("job %s left a copy of %s on %s, recorded %s", cp.JobID, strings.Join(cp.VolIDs, ", "), cp.Node,
				cp.RecordedAt.UTC().Format(time.RFC3339))
			switch cp.Kind {
			case aj.ReplicaCopyOrphan:
				c.rep.problem(haKindReplicaRecordedOrph, subject, "%s after a forced job removal; remove it by hand", detail)
			default:
				c.rep.advise(haKindReplicaSuperseded, subject, "%s; a later retirement removes it", detail)
			}
		}
	}
}

// headroomFindings works out the N+1 memory margin and reports a shortfall or
// an unavailable estimate.
func (c *haCollector) headroomFindings() {
	margin := c.margin()
	c.rep.Headroom = margin.Summary()
	c.rep.Profile += " headroom=" + c.rep.Headroom
	switch {
	case margin.Unavailable != "":
		c.rep.advise(haKindHeadroomUnavailable, "", "the N+1 memory margin could not be worked out: %s", margin.Unavailable)
	case margin.Short():
		for _, s := range margin.Shortfalls {
			c.rep.problem(haKindHeadroomShort, s.FailedNode, "%s", strings.TrimPrefix(s.Line(), "ha: "))
		}
	}
}

// margin builds the headroom inputs from the guests, the HA resources, and the
// pins, and runs the estimate.
func (c *haCollector) margin() placement.HeadroomMargin {
	if !c.guestsOK {
		return placement.HeadroomMargin{Unavailable: "the cluster's guests could not be read"}
	}
	if c.memoryErr != nil {
		return placement.HeadroomMargin{Unavailable: "the cluster's guest memory could not be read: " + c.memoryErr.Error()}
	}
	if !c.resourcesOK || !c.rulesOK {
		return placement.HeadroomMargin{Unavailable: "the HA resources and rules could not be read"}
	}
	facts, err := c.r.NodeFacts(c.ctx)
	if err != nil {
		return placement.HeadroomMargin{Unavailable: "node facts could not be read: " + err.Error()}
	}
	all := make([]string, 0, len(c.p.Nodes))
	for i := range c.p.Nodes {
		all = append(all, c.p.Nodes[i].Name)
	}
	guests := make([]placement.HeadroomGuest, 0, len(c.guests))
	for _, g := range c.guests {
		if g.ref.Template {
			continue
		}
		hg := placement.HeadroomGuest{VMID: g.ref.VMID, Node: g.ref.Node, MemBytes: g.memBytes}
		if g.resource != nil && haRunsUnderHA(g.resource.State) {
			hg.HA, hg.Admissible = true, c.admissible(g, all)
		}
		guests = append(guests, hg)
	}
	return placement.EstimateHeadroomMargin(facts, guests, false)
}

// admissible returns the nodes an HA guest may be recovered on: the nodes of
// its strict enabled node-affinity rule, else its CPU migration domain when its
// CPU type needs one, else every node.
func (c *haCollector) admissible(g *haGuest, all []string) []string {
	for i := range c.rules {
		r := &c.rules[i]
		if r.Type == ha.RuleTypeNodeAffinity && r.Strict && !r.Disabled && r.HasResource(g.sid) {
			return r.NodeNames()
		}
	}
	if g.cfg != nil {
		if cpu, _ := pve.ConfigStringValue(g.cfg["cpu"]); ha.CPUTypeNeedsDomain(cpu) {
			if dom, ok := c.p.DomainOf(g.ref.Node); ok {
				return dom.Nodes
			}
			return []string{g.ref.Node}
		}
	}
	return all
}

// leftoverFindings reports what an older CPI, a staged registration, a crashed
// call, or a mode switch to off left behind.
func (c *haCollector) leftoverFindings() {
	c.resourceLeftovers()
	c.ruleLeftovers()
	c.guestLeftovers()
}

// resourceLeftovers reports CPI resources left in the ignored state, and the
// resources an off mode left in place.
func (c *haCollector) resourceLeftovers() {
	if !c.resourcesOK {
		return
	}
	sids := make([]string, 0, len(c.resources))
	for sid := range c.resources {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		row := c.resources[sid]
		if c.foreign(row.Comment) {
			continue
		}
		_, marked := haMarkerOwner(row.Comment)
		if marked && strings.EqualFold(row.State, "ignored") {
			c.rep.problem(haKindResourceIgnored, sid,
				"the CPI-owned resource is left in the ignored state by staging or by the narrowing barrier")
		}
		if c.off() && (marked || c.ownedResource(sid)) {
			c.rep.problem(haKindOffModeResource, sid,
				"the CPI-owned HA resource still exists while pve.ha.mode is off; remove it in the PVE UI or with ha-manager remove")
		}
	}
}

// ruleLeftovers reports the CPI rules an off mode left in place.
func (c *haCollector) ruleLeftovers() {
	if !c.off() || !c.rulesOK {
		return
	}
	for i := range c.rules {
		if ruleMarked(&c.rules[i]) && !c.foreign(c.rules[i].Comment) {
			c.rep.problem(haKindOffModeRule, c.rules[i].ID,
				"the CPI-owned HA rule still exists while pve.ha.mode is off; remove it in the PVE UI or with ha-manager rules remove")
		}
	}
}

// guestLeftovers reports stale birth tags, stale re-registration tags, and
// managed VMs that have no HA resource.
func (c *haCollector) guestLeftovers() {
	if !c.guestsOK {
		return
	}
	for _, g := range c.guests {
		for _, t := range g.tagsWithPrefix(haBirthTagPrefix) {
			if born, ok := birthTagTime(t); ok && c.now.Sub(born) > c.birthTagAge() {
				c.rep.problem(haKindBirthTagStale, g.sid,
					"VM %d on %s has carried the birth tag %s for %s, longer than the %s the sweep waits, so its create did not finish",
					g.ref.VMID, g.ref.Node, t, c.now.Sub(born).Round(time.Second), c.birthTagAge())
			}
		}
		for _, t := range g.tagsWithPrefix(ha.ReaddTagPrefix) {
			if at, ok := ha.ReaddTagTime(t); ok && c.now.Sub(at) > c.birthTagAge() {
				c.rep.problem(haKindReaddTag, g.sid,
					"VM %d on %s has carried the re-registration tag %s since %s, longer than the %s a re-registration is given, so it did not finish",
					g.ref.VMID, g.ref.Node, t, at.UTC().Format(time.RFC3339), c.birthTagAge())
			}
		}
		if c.resourcesOK && g.owned && g.hasTag(haManagedTag) && g.resource == nil {
			c.rep.problem(haKindManagedNoResource, g.sid,
				"VM %d on %s carries %s but has no HA resource", g.ref.VMID, g.ref.Node, haManagedTag)
		}
	}
}

// ownedResource reports whether sid names a VM the CPI created.
func (c *haCollector) ownedResource(sid string) bool {
	kind, id, ok := strings.Cut(sid, ":")
	if !ok || kind != "vm" {
		return false
	}
	vmid, err := strconv.Atoi(id)
	if err != nil {
		return false
	}
	g, found := c.byVMID[vmid]
	return found && g.owned
}

// cacheDirectoryFindings reports a cache directory the CPI cannot use as it
// stands. The directory holds the cache files and the HA recovery episode
// record, and the CPI chooses it in layers. The first is pve.endpoint_cache_dir,
// the next is the endpoint-cache directory beside the allocation journal, and
// the last is a directory under the temporary directory that the CPI creates.
// The CPI also creates a missing directory of the first two layers when its
// parent belongs to the CPI's user and shares a mount with a directory that
// persists, and the doctor says whether it would and, when it would not, why. A
// layer that applies and fails the private checks ends the search, which
// leaves the record unwritten, so a recovery grace window never starts and the
// dead-node handover never happens. The doctor asks the same question the
// episode store asks, for every layer, without creating anything, and it asks
// it for the CPI's user, as cacheJudge describes, because the private checks
// require the user that runs them as the owner. A pve.endpoint_cache_dir that
// does not exist and that the CPI does not create is the one refusal that
// falls through to the layers after it, and creating it later moves the record
// and restarts a running grace window once.
func (c *haCollector) cacheDirectoryFindings() {
	c.cacheFindingsAs(c.cacheJudge())
}

// cacheFindingsAs is cacheDirectoryFindings with the judge given.
func (c *haCollector) cacheFindingsAs(j cacheJudge) {
	dir := endpointcache.OptionsFor(c.cfg).Named(ha.EpisodeName)
	if path := c.cfg.EndpointCacheDir; path != "" && c.overrideFindings(path, dir, j) {
		return
	}
	if c.cfg.EndpointCacheDir == "" {
		if path, text, blocked := j.journalGap(dir); text != "" {
			c.rep.advise(haKindCacheDirectory, path, "%s", text)
			if blocked {
				return
			}
		}
	}
	choice, caveat := j.probe(dir)
	defer func() { _ = choice.Close() }()
	if c.cfg.EndpointCacheDir == "" {
		c.siblingCreation(choice, j)
	}
	switch {
	case choice.Layer == endpointcache.LayerJournal && createdOnFirstUse(choice):
		// siblingCreation said that the CPI creates it.
	case choice.Layer == endpointcache.LayerTemporary && !j.checksTemporary():
		// The directory the CPI's user would have is not one the doctor can
		// see, so nothing is said about it.
	case choice.Layer == endpointcache.LayerTemporary && missingDirectory(choice):
		// The CPI creates this directory on first use, so nothing is wrong yet.
	case caveat != "":
		c.rep.advise(haKindCacheDirectory, choice.Path, "%s", caveat)
	case choice.Usable() || choice.Err == nil:
	default:
		c.rep.advise(haKindCacheDirectory, choice.Path, "%s%s", cacheRefusalText(choice), j.ownerNote(choice.Err))
	}
}

// mountCaveat is what every finding that says the CPI creates a directory adds,
// because the CPI may run in a BPM mount namespace that this check cannot see.
const mountCaveat = "this check sees the host's mounts, and under BPM the CPI creates it only when that parent " +
	"directory is mounted into the worker, as it is when director.cpi_additional_volumes lists it or a " +
	"directory above it"

// siblingCreation reports a missing directory beside the allocation journal,
// with what the CPI does about it. choice is the doctor's probe of the layers
// with no override.
func (c *haCollector) siblingCreation(choice endpointcache.Choice, j cacheJudge) {
	if choice.Layer == endpointcache.LayerJournal && createdOnFirstUse(choice) {
		c.rep.advise(haKindCacheDirectory, choice.Path,
			"%s does not exist, and the CPI creates it with mode 0700 on first use, because its parent "+
				"directory belongs to uid %d, which the CPI runs as, and sits on the same mount as the allocation "+
				"journal directory; %s; cpi provision-journal also creates it%s",
			cacheDirectoryName(endpointcache.LayerJournal), j.uid, mountCaveat, j.judgedAsNote())
		return
	}
	for _, declined := range choice.Declined {
		var notCreated *endpointcache.NotCreatedError
		if declined.Layer != endpointcache.LayerJournal || !errors.As(declined, &notCreated) {
			continue
		}
		c.rep.advise(haKindCacheDirectory, declined.Path,
			"%s does not exist, and the CPI does not create it because %v, so the cache and the HA recovery "+
				"episode record use %s instead; run cpi provision-journal, or create it with mode 0700 for the "+
				"CPI's user%s", cacheDirectoryName(endpointcache.LayerJournal), notCreated.Reason,
			ha.FallbackDescription(choice.Layer), j.judgedAsNote())
	}
}

// judgedAsNote is what a finding about creating a directory adds when no
// allocation journal directory names the CPI's user, because the doctor then
// judged as its own user.
func (j cacheJudge) judgedAsNote() string {
	if j.known {
		return ""
	}
	return fmt.Sprintf("; this check judged as its own user, uid %d, because no allocation journal directory "+
		"names the CPI's user", j.doctor)
}

// createdOnFirstUse reports whether choice is the probe's refusal of a missing
// directory that the CPI creates the first time it chooses it. The check must
// not hand a nil *Refusal to errors.Is, which would call its Unwrap method.
func createdOnFirstUse(choice endpointcache.Choice) bool {
	return choice.Err != nil && errors.Is(choice.Err, endpointcache.ErrCreatedOnFirstUse)
}

// overrideCreation says what the CPI does with a pve.endpoint_cache_dir that
// does not exist. creates is true when it creates the directory. Otherwise
// reason says why it does not, or is empty when the check cannot tell, which is
// when no journal directory names the CPI's user and the doctor cannot see the
// CPI's temporary directory, whose mount decides it.
func (j cacheJudge) overrideCreation(dir endpointcache.DirOptions) (creates bool, reason string) {
	if dir.JournalDir == "" && !j.checksTemporary() {
		return false, ""
	}
	choice := dir.ProbeAs(j.uid)
	defer func() { _ = choice.Close() }()
	if createdOnFirstUse(choice) {
		return true, ""
	}
	var notCreated *endpointcache.NotCreatedError
	if choice.Err != nil && errors.As(choice.Err, &notCreated) {
		return false, notCreated.Reason.Error()
	}
	return false, ""
}

// missingDirectory reports whether choice names a directory that does not exist,
// which the CPI creates on first use. A choice that was not refused names one
// that exists. The check must not hand a nil *Refusal to errors.Is, which would
// call its Unwrap method.
func missingDirectory(choice endpointcache.Choice) bool {
	return choice.Err != nil && errors.Is(choice.Err, fs.ErrNotExist)
}

// cacheJudge says whose decision the doctor's cache directory checks stand
// for. The private checks require the user that runs them as the owner of the
// directory, with no exception for root, and the doctor usually runs as root
// while the CPI runs as vcap, so the doctor judges as the CPI's user when it
// can tell who that is.
type cacheJudge struct {
	// uid is the user the private checks require as the owner.
	uid int
	// known is true when uid is the CPI's user, read from the owner of the
	// allocation journal directory, which pre-start provisions for that user.
	// Otherwise uid is the doctor's own user.
	known bool
	// doctor is the doctor's own effective user.
	doctor int
	// ancestors, when set, replaces the search of the directories above the
	// cache directory, so a test can name one the doctor itself can still
	// reach.
	ancestors func(path string, uid int) (*searchBlock, error)
}

// cacheJudge returns the judge for this run. The owner of the allocation
// journal directory is the CPI's user when the config names a journal that
// exists, and the doctor's own user stands in for it otherwise.
func (c *haCollector) cacheJudge() cacheJudge {
	j := cacheJudge{uid: c.euid, doctor: c.euid}
	journal := c.cfg.StorageAllocationJournalDir
	if journal == "" {
		return j
	}
	info, err := os.Lstat(journal)
	if err != nil || !info.IsDir() {
		return j
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		j.uid, j.known = int(st.Uid), true
	}
	return j
}

// checksTemporary reports whether the doctor can judge the temporary layer.
// That directory's name carries the user it belongs to, and it sits under the
// temporary directory of the CPI's own environment, so the doctor sees it only
// when it runs as the CPI's user. A doctor that runs as root with no journal
// to name the CPI's user is taken to be another user, because the CPI does not
// run as root.
func (j cacheJudge) checksTemporary() bool {
	if j.known {
		return j.uid == j.doctor
	}
	return j.doctor != 0
}

// probe chooses the cache directory for dir as the CPI's user would, as far
// as the doctor can judge. The caveat is empty when the choice stands for the
// CPI's decision. When the CPI's user is unknown and a directory fails only
// because another user owns it, the directory is judged again as its owner,
// and when it passes, the caveat says the CPI uses it only when it runs as
// that owner. When the doctor could not read a directory it judged for
// another user, the caveat says so. A directory the doctor reaches as itself
// but the judged user cannot, because a directory above it grants that user no
// search access, is a caveat too, since the CPI gets permission denied there.
// The caller closes the choice.
func (j cacheJudge) probe(dir endpointcache.DirOptions) (endpointcache.Choice, string) {
	choice, caveat, judged := j.judgeDirectory(dir)
	if choice.Err != nil || judged == j.doctor {
		return choice, caveat
	}
	if text := j.reachText(cacheDirectoryName(choice.Layer), choice.Path, judged); text != "" {
		return choice, text
	}
	return choice, caveat
}

// searchAbove asks which directory on the way to path the user judged cannot
// search. The doctor opens the path with its own credentials, so the answer
// comes from the permission bits and not from a test.
func (j cacheJudge) searchAbove(path string, judged int) (*searchBlock, error) {
	if j.ancestors != nil {
		return j.ancestors(path, judged)
	}
	return ancestorBlock(path, judged, groupsOf)
}

// reachText says why the user judged cannot open the directory called what at
// path, because a directory above it grants that user no search access, or
// that the doctor could not tell. It is empty when the user can reach the
// directory.
func (j cacheJudge) reachText(what, path string, judged int) string {
	block, err := j.searchAbove(path, judged)
	switch {
	case err != nil:
		return fmt.Sprintf("%s was not checked for search access by uid %d, which the CPI runs as, because "+
			"the directories above it could not be tested (%v); the CPI cannot open it unless that user can "+
			"search every directory above it", what, judged, err)
	case block != nil:
		return fmt.Sprintf("%s sits below %s, which uid %d cannot search because it is owned by uid %d with "+
			"mode %04o, so the CPI, running as that user, gets permission denied opening it, keeps no HA recovery "+
			"record, and the dead-node handover cannot start; give uid %d search access to %s",
			what, block.dir, judged, block.owner, block.mode, judged, block.dir)
	}
	return ""
}

// missingReach says what the CPI meets when it looks for the directory called
// what at path, which does not exist as the doctor sees it. The CPI looks it up
// as its own user, and a directory above it that user cannot search gives
// permission denied, not a missing directory, so the CPI does not instead
// move on, and the doctor must not say it does. The walk covers the
// directories down to the first one that does not exist. The text is empty
// when the user can search all of them, and blocked is true when a directory
// definitely stops it. instead is what the CPI does with a missing directory.
func (j cacheJudge) missingReach(what, path, instead string) (text string, blocked bool) {
	if j.uid == j.doctor {
		return "", false
	}
	block, err := j.searchAbove(path, j.uid)
	switch {
	case block != nil:
		return fmt.Sprintf("%s does not exist as this check sees it, and it sits below %s, which uid %d cannot "+
			"search because it is owned by uid %d with mode %04o, so the CPI, running as that user, gets permission "+
			"denied looking for it and does not find it missing; it does not %s, keeps no HA recovery record, and "+
			"the dead-node handover cannot start; give uid %d search access to %s",
			what, block.dir, j.uid, block.owner, block.mode, instead, j.uid, block.dir), true
	case err != nil:
		return fmt.Sprintf("%s does not exist as this check sees it, and the check could not tell whether uid %d, "+
			"which the CPI runs as, can search the directories above it (%v); if that user cannot, the CPI gets "+
			"permission denied and does not find it missing, so it does not %s, keeps no HA recovery record, and "+
			"the dead-node handover cannot start", what, j.uid, err, instead), false
	}
	return "", false
}

// journalGap reports the directory beside the allocation journal directory
// when it does not exist as the doctor sees it and the CPI's user cannot get
// there to see that, because the CPI would then be refused at that directory
// and not move on to the temporary one. It returns the path of the directory
// beside the journal and the text, which is empty when there is nothing to
// say.
func (j cacheJudge) journalGap(dir endpointcache.DirOptions) (path, text string, blocked bool) {
	if dir.JournalDir == "" {
		return "", "", false
	}
	journal := filepath.Clean(dir.JournalDir)
	if !filepath.IsAbs(journal) || journal == string(filepath.Separator) ||
		filepath.Base(journal) == endpointcache.JournalSiblingName {
		return "", "", false
	}
	path = filepath.Join(filepath.Dir(journal), endpointcache.JournalSiblingName)
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return "", "", false
	}
	text, blocked = j.missingReach(cacheDirectoryName(endpointcache.LayerJournal), path,
		"move on to the temporary cache directory")
	return path, text, blocked
}

// judgeDirectory is the choice probe makes before it looks at the directories
// above it, with the user it judged as.
func (j cacheJudge) judgeDirectory(dir endpointcache.DirOptions) (endpointcache.Choice, string, int) {
	choice := dir.ProbeAs(j.uid)
	if choice.Err == nil {
		return choice, "", j.uid
	}
	what := cacheDirectoryName(choice.Layer)
	if errors.Is(choice.Err, fs.ErrPermission) && j.uid != j.doctor {
		unread := fmt.Sprintf("%s could not be read by this check, which ran as uid %d, so it was not judged "+
			"as the CPI's user, uid %d, would judge it; run the check as root or as that user", what, j.doctor, j.uid)
		return choice, unread, j.uid
	}
	var unsafe *privatefile.UnsafePathError
	if j.known || !errors.As(choice.Err, &unsafe) || !errors.Is(unsafe.Cause, privatefile.ErrUnsafeOwnership) ||
		unsafe.UID < 0 || choice.Layer == endpointcache.LayerTemporary {
		// The temporary directory's name carries the user it belongs to, so one
		// that another user owns is never the CPI's directory, and judging it
		// again as its owner would look for a different directory.
		return choice, "", j.uid
	}
	owner := unsafe.UID
	again := dir.ProbeAs(owner)
	if again.Err != nil {
		_ = choice.Close()
		return again, "", owner
	}
	_ = choice.Close()
	note := fmt.Sprintf("%s is owned by uid %d, and this check ran as uid %d with no allocation journal "+
		"directory to show which user the CPI runs as; the directory passes every other check, so the CPI keeps "+
		"its HA recovery record there when it runs as uid %d and keeps none when it runs as another user, and the "+
		"dead-node handover then cannot start; confirm uid %d is the CPI's user", what, owner, j.doctor, owner, owner)
	return again, note, owner
}

// overrideFindings reports what is wrong with pve.endpoint_cache_dir itself,
// and returns true when it reported something.
func (c *haCollector) overrideFindings(path string, dir endpointcache.DirOptions, j cacheJudge) bool {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.missingOverride(path, dir, j)
	case err != nil:
		c.rep.advise(haKindCacheDirectory, path, "pve.endpoint_cache_dir could not be inspected: %v", err)
	case info.Mode()&fs.ModeSymlink != 0:
		c.rep.advise(haKindCacheDirectory, path,
			"pve.endpoint_cache_dir is a symbolic link, which the CPI refuses, so it keeps no HA recovery record "+
				"and the dead-node handover cannot start; point the setting at the directory itself")
	case !info.IsDir():
		c.rep.advise(haKindCacheDirectory, path,
			"pve.endpoint_cache_dir is not a directory, so the CPI keeps no HA recovery record and the "+
				"dead-node handover cannot start")
	case info.Mode().Perm()&0o077 != 0:
		c.rep.advise(haKindCacheDirectory, path,
			"pve.endpoint_cache_dir has mode %04o and must grant no group or other access, so the CPI refuses it "+
				"and keeps no HA recovery record, which means the dead-node handover cannot start; "+
				"run chmod 0700 on it",
			info.Mode().Perm())
	default:
		return false
	}
	return true
}

// missingOverride reports a pve.endpoint_cache_dir that does not exist. The
// episode record then goes to the layer that answers after the override, which
// the finding names, or nowhere when that layer is refused too. A temporary
// layer the doctor cannot judge is named as where the record goes, because
// the CPI creates its own. A directory above the override that the CPI's user
// cannot search is reported instead, because the CPI is refused there and
// falls back to nothing.
func (c *haCollector) missingOverride(path string, dir endpointcache.DirOptions, j cacheJudge) {
	if text, blocked := j.missingReach("pve.endpoint_cache_dir", path, "fall back to another directory"); text != "" {
		c.rep.advise(haKindCacheDirectory, path, "%s", text)
		if blocked {
			return
		}
	}
	creates, reason := j.overrideCreation(dir)
	if creates {
		anchor := "the allocation journal directory"
		if dir.JournalDir == "" {
			anchor = "the CPI's temporary directory"
		}
		c.rep.advise(haKindCacheDirectory, path,
			"pve.endpoint_cache_dir does not exist, and the CPI creates it with mode 0700 on first use, because "+
				"its parent directory belongs to uid %d, which the CPI runs as, and sits on the same mount as %s; "+
				"%s; the HA recovery episode record then sits in it, so a grace window that is running restarts "+
				"once%s", j.uid, anchor, mountCaveat, j.judgedAsNote())
		return
	}
	missing := "pve.endpoint_cache_dir does not exist"
	switch {
	case reason != "":
		missing += ", and the CPI does not create it because " + reason + j.judgedAsNote() + ","
	default:
		missing += ", and this check cannot tell whether the CPI creates it, because with no allocation journal " +
			"directory that depends on the CPI's temporary directory, which this check does not see,"
	}
	if _, text, blocked := j.journalGap(dir); text != "" {
		c.rep.advise(haKindCacheDirectory, path, "%s and %s", missing, text)
		if blocked {
			return
		}
	}
	fallback, caveat := j.probe(dir.WithoutOverride())
	defer func() { _ = fallback.Close() }()
	temporary := fallback.Layer == endpointcache.LayerTemporary
	toBeCreated := temporary && (!j.checksTemporary() || missingDirectory(fallback)) ||
		fallback.Layer == endpointcache.LayerJournal && createdOnFirstUse(fallback)
	switch {
	case caveat != "" && !toBeCreated:
		c.rep.advise(haKindCacheDirectory, path,
			"%s so the HA recovery episode record goes to %s instead, and %s",
			missing, ha.FallbackDescription(fallback.Layer), caveat)
	case fallback.Usable() || toBeCreated:
		c.rep.advise(haKindCacheDirectory, path,
			"%s so the HA recovery episode record sits in %s instead; create it with mode 0700, and note that "+
				"doing so moves the record and restarts any grace window that is running",
			missing, ha.FallbackDescription(fallback.Layer))
	case fallback.Err != nil:
		c.rep.advise(haKindCacheDirectory, path,
			"%s and %s cannot be used either (%v), so the CPI keeps no HA recovery record and the dead-node "+
				"handover cannot start; create pve.endpoint_cache_dir with mode 0700",
			missing, ha.FallbackDescription(fallback.Layer), fallback.Err.Err)
	}
}

// cacheDirectoryName says in words which directory a layer names.
func cacheDirectoryName(layer endpointcache.Layer) string {
	if layer == endpointcache.LayerOverride {
		return "pve.endpoint_cache_dir"
	}
	return ha.FallbackDescription(layer)
}

// cacheRefusalText says in words which directory a refused layer named and why
// it was refused.
func cacheRefusalText(choice endpointcache.Choice) string {
	return fmt.Sprintf("%s is refused (%v), so the CPI keeps no HA recovery record and the dead-node handover "+
		"cannot start", cacheDirectoryName(choice.Layer), choice.Err.Err)
}

// ownerNote adds what an owner mismatch means to the operator. When the CPI's
// user is known, the refusal was judged as that user, so the note names it and
// says to give the directory to it. Otherwise the doctor can report only the
// owner it sees and the user it ran as.
func (j cacheJudge) ownerNote(refusal *endpointcache.Refusal) string {
	var unsafe *privatefile.UnsafePathError
	if !errors.As(refusal, &unsafe) || !errors.Is(unsafe.Cause, privatefile.ErrUnsafeOwnership) {
		return ""
	}
	if refusal.Layer == endpointcache.LayerTemporary {
		return fmt.Sprintf("; the name of this directory carries uid %d, which the CPI runs as when it uses it, "+
			"and another user owns it, so remove it and the CPI creates its own on first use", j.uid)
	}
	if j.known {
		return fmt.Sprintf("; the CPI runs as uid %d, which owns the allocation journal directory, so give this "+
			"directory to that user", j.uid)
	}
	return fmt.Sprintf("; the CPI runs as the user that owns the directory, and this check ran as uid %d, "+
		"so confirm the owner is the CPI's user", j.doctor)
}
