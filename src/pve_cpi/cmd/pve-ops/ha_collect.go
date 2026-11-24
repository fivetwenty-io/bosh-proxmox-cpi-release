package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
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
	// haCPIVolumePattern matches the names the CPI gives a VM's volumes on a
	// block storage such as a ZFS pool: the cloned root and the persistent
	// disks a reassignment renames to the VM (vm-<vmid>-disk-<n>), the
	// ephemeral disk (vm-<vmid>-ephemeral-<n>), the cloud-init drive, and the
	// allocations the managed lifecycle names (vm-<vmid>-bosh-...).
	haCPIVolumePattern = regexp.MustCompile(
		`^[^:]+:(?:[^/]+/)?vm-(\d+)-(?:disk-\d+|ephemeral-\d+|cloudinit|bosh-[A-Za-z0-9._-]+)(?:\.[a-z0-9]+)?$`)
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
// looks. owner is empty when the marker names no Director. The marker is read
// through ha.MarkerFields, so a comment PVE encoded again still reads as the
// marker it holds.
func haMarkerOwner(comment string) (owner string, ok bool) {
	fields, _, ok := ha.MarkerFields(comment)
	if !ok {
		return "", false
	}
	for _, f := range fields {
		if key, val, found := strings.Cut(f, "="); found && key == "owner" && val != "-" {
			owner = val
		}
	}
	return owner, true
}

// haJobMarker reports whether a replication job's comment holds the marker the
// CPI writes, which carries one owner and one positive generation. A comment
// without both is not a job the CPI can vouch for. It reads the comment
// through ha.ParseReplicaJobMarker, the reading the CPI's handlers use, so the
// doctor counts a job as the CPI's exactly when the CPI does, encoded again or
// not.
func haJobMarker(comment string) bool {
	_, ok := ha.ParseReplicaJobMarker(comment)
	return ok
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
	// holdReported holds the services already reported as hold_blocks_maintenance,
	// so the stranded check does not name them a second time.
	holdReported map[string]bool
	// aaDefaults holds the default pattern of each group under --cpi-config
	// (see groupDefaults), and entryChecks the plans of the groups under
	// each entry's settings, keyed by the entry's name, as groupPlacer
	// builds them.
	aaDefaults  map[string]aapattern.Pattern
	entryChecks map[string]map[string]handlers.AntiAffinityGroupCheck

	ctx   context.Context
	r     HAReader
	cfg   *config.CPIConfig
	owner string
	now   time.Time
	rep   *haReport
	p     *ha.Profile

	resources   map[string]HAResourceRow
	resourcesOK bool
	states      map[string]string
	statesOK    bool
	svcNodes    map[string]string
	svcNodesOK  bool
	clusterVMs  map[int]pve.GuestRef
	clusterOK   bool
	rules       []ha.Rule
	rulesOK     bool
	guests      []*haGuest
	byVMID      map[int]*haGuest
	guestsOK    bool
	// guestsPartial is true when offline nodes kept some guests out of the
	// list, so a VMID missing from it may still name a guest.
	guestsPartial bool
	// configsFailed is true when the config of an owned guest could not be
	// read, which leaves the volumes that guest holds out of the referenced set.
	configsFailed bool
	storages      map[string]pve.StorageInfo
	storagesOK    bool
	jobs          []ha.ReplicaJobState
	jobsOK        bool
	jobStatus     map[string]ha.ReplicaSyncStatus
	schedules     map[string]string
	schedErr      error
	awaiting      map[int]bool
	journal       JournalView
	journalOK     bool
	memoryErr     error

	// scope says which VMs this run judges under the cluster's local policy.
	// It is nil for a run that reads one effective config.
	scope *haEntryScope

	// euid is the doctor's own effective user, which the cache directory
	// checks compare with the CPI's user.
	euid int
	// workers, when set, replaces systemWorkers, so a test can say where the
	// Director runs its workers.
	workers func() workerLayout
}

// collectHA reads the cluster r reaches and builds the doctor's report. cfg is
// the effective CPI config for the cluster, owner is the requesting Director's
// UUID when the run has one, and now is the clock every age is measured on.
func collectHA(ctx context.Context, r HAReader, cfg *config.CPIConfig, owner string, now time.Time) *haReport {
	return collectHAScoped(ctx, r, cfg, owner, now, nil)
}

// collectHAScoped is collectHA for a run that judges one cpi-config entry among
// several. scope narrows the VMs the local policy is judged on to the ones the
// entry can own.
func collectHAScoped(ctx context.Context, r HAReader, cfg *config.CPIConfig, owner string, now time.Time,
	scope *haEntryScope,
) *haReport {
	c := &haCollector{
		ctx: ctx, r: r, cfg: cfg, owner: owner, now: now, euid: os.Geteuid(), scope: scope,
		rep:    &haReport{Mode: cfg.HAModeValue(), Nodes: []haNode{}, CPUDomains: []haDomain{}, Findings: []haFinding{}},
		byVMID: map[int]*haGuest{}, jobStatus: map[string]ha.ReplicaSyncStatus{}, awaiting: map[int]bool{},
		holdReported: map[string]bool{},
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
	c.settleDrain()
	c.profileFindings()
	c.ruleFindings()
	c.softPinFindings()
	c.failbackFindings()
	c.rebalancePendingFindings()
	c.antiAffinityFindings()
	c.cpuFindings()
	c.protectionFindings()
	c.maintenanceStrandedFindings()
	c.recoveryStrandedFindings()
	c.replicationFindings()
	c.headroomFindings()
	c.spreadSkewedFindings()
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
	for i := range p.Health.Nodes {
		if p.Health.Nodes[i].Name == name {
			return p.Health.Nodes[i], true
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
			line.Placeable, line.Maintenance, line.NotesTag = h.Placeable, h.Maintenance, h.NotesTag
			line.Watchdog, line.Reason = fmt.Sprint(h.Watchdog), h.Reason
			if h.LRMMode != "" || h.LRMState != "" {
				line.LRM = "mode=" + h.LRMMode + "/state=" + h.LRMState
			}
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
	c.readServiceStates()
	c.readServiceNodes()
	c.readClusterVMs()
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

func (c *haCollector) readServiceStates() {
	states, err := c.r.ServiceStates(c.ctx)
	if err != nil {
		c.rep.unchecked("ha-service-state", "the HA manager status could not be read, so services waiting in recovery are unchecked: %v", err)
		return
	}
	c.states, c.statesOK = states, true
}

func (c *haCollector) readServiceNodes() {
	nodes, err := c.r.ServiceNodes(c.ctx)
	if err != nil {
		c.rep.unchecked("ha-service-node", "the HA manager status could not be read, so a node in maintenance is not called drained: %v", err)
		return
	}
	c.svcNodes, c.svcNodesOK = nodes, true
}

func (c *haCollector) readClusterVMs() {
	refs, err := c.r.ClusterVMs(c.ctx)
	if err != nil {
		c.rep.unchecked("cluster-resources", "the cluster's VM list could not be read, so a service waiting in recovery on a node that is down is unchecked: %v", err)
		return
	}
	c.clusterVMs = make(map[int]pve.GuestRef, len(refs))
	for _, ref := range refs {
		c.clusterVMs[ref.VMID] = ref
	}
	c.clusterOK = true
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
		c.guestsPartial = true
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
			c.configsFailed = true
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
// state, a down node, a node notes line that misses the maintenance marker,
// quorum risk, the QDevice recommendation, and a CRS mode that leaves DLB
// without effect.
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
	c.entryClockFinding()
	if p.Health != nil {
		// Every entry of one Director on the cluster sees the same nodes, so
		// only the entry that reports the cluster findings reports a down node.
		// The notes are another matter, because an entry's token may fail to
		// read what another entry's token reads, so the advice on them comes
		// from the first entry that read the node's notes.
		cluster := c.reportsCluster()
		for i := range p.Health.Nodes {
			n := &p.Health.Nodes[i]
			if n.DeadOrFenced && cluster {
				rep.problem(haKindNodeDown, n.Name, "the node is offline, unknown, fenced, or gone: %s", n.Reason)
			}
			if n.NotesUnread {
				rep.notesUnread = append(rep.notesUnread, n.Name)
				continue
			}
			if !c.readsNotesFirst(n.Name) {
				continue
			}
			for _, line := range n.NotesNearMiss {
				rep.advise(haKindNotesNearMiss, n.Name, "the line %q in the node's notes looks like a maintenance "+
					"marker but is not one, so the CPI still places VMs on the node; write it as \"tags: <tag>\" "+
					"at the start of its own line to take the node out", line)
			}
		}
		if p.Health.NotesErr != nil {
			rep.unchecked("node-notes", "the notes of a node could not be read, so the doctor cannot tell whether "+
				"they carry the maintenance marker, and the CPI places nothing on that node: %v", p.Health.NotesErr)
		}
	} else if p.HealthErr != "" {
		rep.unchecked("node-health", "node health could not be built: %s", p.HealthErr)
	}
	if p.Quorum.OneFailureFromLoss() && c.reportsCluster() {
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

// entryClockFinding reports an entry node whose own clock cannot be read. A
// node does that while systemd shuts it down, or when systemd-timedated is
// broken. When a peer's clock was borrowed the HA view stays good, so the
// finding is advice, and the doctor reads the entry node's clock once more so
// that a one-off server error on a healthy node reports nothing. When no
// clock answered, the profile is observe-only, so the finding is a problem.
func (c *haCollector) entryClockFinding() {
	h := c.p.Health
	if h == nil {
		return
	}
	entry := h.EntryNode
	if h.ClockFallback != "" {
		if entry != "" {
			if err := c.r.EntryClock(c.ctx, entry); err == nil {
				return
			}
		}
		c.rep.advise(haKindEntryClock, entry,
			"%s, and the doctor's own read of that clock failed too. A node does this while systemd shuts it down. Otherwise check timedatectl and systemd-timedated on %s, or put another node first in pve.endpoints",
			h.ClockFallback, entry)
		return
	}
	if h.HA == pve.HAViewUnreadable && strings.HasPrefix(h.HAReason, "entry node clock unreadable") {
		c.rep.problem(haKindEntryClock, entry,
			"no node's clock answered, so the HA view is unreadable and the profile is observe-only (%s). Check timedatectl and systemd-timedated on %s and on the other nodes, or put another node first in pve.endpoints",
			h.HAReason, entry)
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
	return ok && c.owner != "" && owner != "" && owner != haOwnerValue(c.owner)
}

// haOwnerValue spells a Director UUID the way the CPI writes it into a marker,
// with each run of whitespace replaced by an underscore.
func haOwnerValue(owner string) string {
	return strings.Join(strings.Fields(owner), "_")
}

// cpiRuleName reports whether a rule's name is one the CPI gives rules.
func cpiRuleName(id string) bool {
	return strings.HasPrefix(id, haPinRulePrefix) || strings.HasPrefix(id, haGroupRulePrefix)
}

// unmarkedRuleDetail says what the CPI does with a rule that carries a CPI
// name but no ownership marker. For an anti-affinity rule it describes the
// migration create_vm runs. That migration moves this Director's members that
// the hashed rules now hold out of the old rule, and it leaves every other
// member in place, so other Directors' members keep the old rule's spread. It
// deletes the rule once fewer than two members would remain, because such a
// rule spreads nothing.
func unmarkedRuleDetail(id string) string {
	if strings.HasPrefix(id, haGroupRulePrefix) {
		return "the anti-affinity rule has a CPI name but no ownership marker, so an older CPI wrote it. When this " +
			"CPI next creates a VM in the group, it moves this Director's members that its own marked rules hold out " +
			"of this rule and leaves other Directors' members in place, and it deletes the rule once fewer than two " +
			"members would remain"
	}
	return "the rule has a CPI name but no ownership marker, so an older CPI wrote it"
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
		if (!marked && !cpiRuleName(r.ID)) || c.foreign(r.Comment) || !c.reportsRule(r) {
			continue
		}
		if !marked {
			c.rep.problem(haKindRuleUnmarked, r.ID, "%s", unmarkedRuleDetail(r.ID))
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
// cluster, and each group with a member its rule does not name. A group whose
// rules or HA resources carry a CPI marker that names it is planned the way
// the CPI plans it instead, and layoutFindings reports what that plan finds.
//
// Entries that share a cluster and a VM ID band stamp the same group tags, so
// when they disagree on whether they write rules, a group with no rule may have
// come from an entry that writes none. Such a group is left unjudged and the
// band gets one advice. A group that has a rule was written by an entry with
// rules on, so it is still judged. Only the first entry of the band reports.
//
// Under --cpi-config a group is planned with the default pattern of the
// entries that write its rules, which are the entries with rules on whose
// bands hold its members. When those entries set different defaults and no
// member's create named a pattern, the layout the CPI plans depends on which
// entry serves the call, so the group's layout is left unjudged and the group
// gets one advice. A group's members are gathered from every entry's band on
// the cluster, not only this run's, so every entry sees the same group and
// exactly one of them judges it (see judgesGroup).
func (c *haCollector) antiAffinityFindings() {
	if !c.guestsOK || !c.rulesOK {
		return
	}
	mixed := c.scope.antiAffinityAmbiguous()
	if !mixed && !c.antiAffinityWrites() {
		return
	}
	groups := map[string][]*haGuest{}
	for _, g := range c.guests {
		if !g.owned || !c.groupMember(g) {
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
	defaults, split := c.groupDefaults(groups, tags)
	c.aaDefaults = defaults
	checks := c.layoutChecks(defaults)
	unjudged := false
	for _, tag := range tags {
		chk, planned := checks[tag]
		if !c.judgesGroup(groups[tag], chk.Owner, tag) {
			continue
		}
		if planned {
			if owners, ok := split[tag]; ok && !chk.PatternRecorded && chk.Problem == "" {
				c.adviseSplitDefaults(tag, chk.Group, owners)
				continue
			}
			if c.adviseContestedDelete(tag, groups[tag], chk) {
				continue
			}
			c.layoutFindings(chk)
			continue
		}
		rules := c.groupRules(tag)
		if mixed && len(rules) == 0 {
			unjudged = true
			continue
		}
		c.checkGroup(tag, groups[tag], rules)
	}
	if unjudged {
		c.rep.advise(haKindAntiAffinityAmbiguous, c.scope.Band.String(),
			"the cpi-config entries %s share this cluster and VM ID band and disagree on pve.ha.anti_affinity, "+
				"so the doctor cannot tell which groups with no rule should have one and does not judge them",
			c.scope.peerList())
	}
}

// judgesGroup reports whether this run judges a group. Under --cpi-config a
// group belongs to the entry that placed it, which groupPlacer finds over
// every member of the group, whatever band holds it, and only that entry's
// run judges it, because entries that share a cluster can map AZs
// differently and so plan one group over different nodes. A group with no
// such entry, or whose entry's cluster the doctor could not read, is judged by
// the first entry on the cluster, in cpi-config order, that the doctor read
// and whose band holds one of its members, so it too is judged once.
// owner is the Director the group's marker names, or "", and tag is the
// group's tag, which is also its base rule name.
func (c *haCollector) judgesGroup(members []*haGuest, owner, tag string) bool {
	if c.scope == nil {
		return true
	}
	if placer, ok := c.groupPlacer(members, owner, tag); ok && !placer.Unread {
		return placer.Self
	}
	entries := c.clusterEntries()
	for i := range entries {
		e := &entries[i]
		if e.Unread {
			continue
		}
		for _, g := range members {
			if e.holds(g) {
				return e.Self
			}
		}
	}
	return false
}

// groupMember reports whether g counts as a member of its groups in this run.
// It is a VM of this run's Director, or one with no owner tag, and under
// --cpi-config it lies in the band of some entry on the cluster, because a
// group's members can lie in the bands of several entries.
func (c *haCollector) groupMember(g *haGuest) bool {
	if !ownerAdmits(c.owner, g) {
		return false
	}
	if c.scope == nil {
		return true
	}
	entries := c.clusterEntries()
	for i := range entries {
		if entries[i].holds(g) {
			return true
		}
	}
	return false
}

// clusterEntries returns the entries on this run's cluster in cpi-config
// order. A scope built without that list stands for its band alone, and it
// speaks for the band only when it is the band's first entry.
func (c *haCollector) clusterEntries() []haClusterEntry {
	if len(c.scope.Cluster) > 0 {
		return c.scope.Cluster
	}
	return []haClusterEntry{{Name: c.scope.Name, Band: c.scope.Band, AntiAffinity: c.scope.AntiAffinity,
		Owner: c.owner, Self: c.scope.Leader}}
}

// groupPlacer returns the entry whose placement a group belongs to, from the
// group's owners (see groupOwners). The CPI writes a group's rules from the
// plan of the entry that placed it, so that entry's plan holds the rules as
// they stand, while another entry's plan, made over its own AZ map, may hold
// different rules. Each owner's settings plan the group (see entryCheck), and
// the owner whose plan could be made wins over one whose plan could not, then
// the owner whose plan deletes the fewest of the group's rules. A tie goes to
// the owner whose AZ map holds the strict pins of the most members, then to
// the owner whose AZs that hold them are smallest, the way a plan picks a
// member's domain, then to the owner whose AZ map names the nodes that the
// soft pins of the most members prefer (see
// handlers.AntiAffinityPreferenceMisses), because only an entry with that AZ
// writes such a preference, then to the owner whose plan writes the fewest
// changes,
// and then to the first in cpi-config order. A pin is read as the Director's
// that owner names, or the run's own when the marker names none. It returns
// false when the group has no owner.
func (c *haCollector) groupPlacer(members []*haGuest, owner, tag string) (haClusterEntry, bool) {
	owners := groupOwners(c.clusterEntries(), members)
	if len(owners) == 0 {
		return haClusterEntry{}, false
	}
	if owner == "" {
		owner = c.owner
	}
	set := &ha.RuleSet{Rules: c.rules}
	sids := make([]string, 0, len(members))
	for _, g := range members {
		sids = append(sids, g.sid)
	}
	type fit struct{ problem, deletes, misses, width, preferences, writes int }
	score := func(e *haClusterEntry) fit {
		var f fit
		f.misses, f.width = handlers.AntiAffinityPlacementFit(e.AZMap, set, owner, sids)
		f.preferences = handlers.AntiAffinityPreferenceMisses(e.AZMap, set, owner, sids)
		if chk, ok := c.entryCheck(e, tag); ok {
			if chk.Problem != "" || chk.ForeignRule != "" {
				f.problem = 1
			}
			f.deletes = len(planDeletes(chk))
			f.writes = len(chk.Drift)
		}
		return f
	}
	less := func(a, b fit) bool {
		for _, d := range [][2]int{{a.problem, b.problem}, {a.deletes, b.deletes}, {a.misses, b.misses},
			{a.width, b.width}, {a.preferences, b.preferences}, {a.writes, b.writes}} {
			if d[0] != d[1] {
				return d[0] < d[1]
			}
		}
		return false
	}
	best, bestFit := 0, score(&owners[0])
	for i := 1; i < len(owners); i++ {
		if f := score(&owners[i]); less(f, bestFit) {
			best, bestFit = i, f
		}
	}
	return owners[best], true
}

// entryCheck returns the plan of the group tag under the settings of entry
// e, which the doctor builds once for each entry and keeps. It returns false
// when the scope carries no settings for e, as a scope built by hand does, or
// when the plan does not cover the group.
func (c *haCollector) entryCheck(e *haClusterEntry, tag string) (handlers.AntiAffinityGroupCheck, bool) {
	if e.Config == nil {
		return handlers.AntiAffinityGroupCheck{}, false
	}
	if c.entryChecks == nil {
		c.entryChecks = map[string]map[string]handlers.AntiAffinityGroupCheck{}
	}
	checks, ok := c.entryChecks[e.Name]
	if !ok {
		checks = c.checksUnder(e.Config, c.aaDefaults)
		c.entryChecks[e.Name] = checks
	}
	chk, ok := checks[tag]
	return chk, ok
}

// planDeletes returns the rules a plan deletes.
func planDeletes(chk handlers.AntiAffinityGroupCheck) []string {
	var out []string
	for _, d := range chk.Drift {
		if rule, ok := strings.CutPrefix(d, "delete "); ok {
			out = append(out, rule)
		}
	}
	return out
}

// adviseContestedDelete reports a group whose plan under this run's settings
// deletes a rule that the plan of another of the group's owners keeps or
// creates, and reports true, so the doctor neither reports the delete as drift
// nor offers a fix that removes a rule another entry's plan wants. Only a
// plan that could be made counts.
func (c *haCollector) adviseContestedDelete(tag string, members []*haGuest, chk handlers.AntiAffinityGroupCheck,
) bool {
	if c.scope == nil {
		return false
	}
	deletes := planDeletes(chk)
	if len(deletes) == 0 {
		return false
	}
	owners := groupOwners(c.clusterEntries(), members)
	for i := range owners {
		e := &owners[i]
		if e.Self {
			continue
		}
		other, ok := c.entryCheck(e, tag)
		if !ok || other.Problem != "" {
			continue
		}
		for _, rule := range deletes {
			if _, wanted := other.Layout[rule]; wanted {
				c.rep.advise(haKindAntiAffinityAmbiguous, tag,
					"the plan of group %s under this entry's settings deletes the rule %s, which the plan under the "+
						"settings of the cpi-config entry %s keeps, so the doctor does not judge the group's layout "+
						"and does not offer to fix it; the entries map the group's nodes differently, so serve the "+
						"group through one entry, or give the entries one placement.az_map",
					chk.Group, rule, e.Name)
				return true
			}
		}
	}
	return false
}

// groupDefaults returns the default pattern each group is planned with under
// --cpi-config, keyed by the group's tag, and, for each group whose owning
// entries set different defaults, those entries. A group's owners are the
// entries on this cluster that write anti-affinity rules and whose band holds
// at least one of its members. A group with no owner is planned with the
// run's own default, as is every group outside --cpi-config.
func (c *haCollector) groupDefaults(groups map[string][]*haGuest, tags []string,
) (map[string]aapattern.Pattern, map[string][]haClusterEntry) {
	if c.scope == nil {
		return nil, nil
	}
	defaults := map[string]aapattern.Pattern{}
	split := map[string][]haClusterEntry{}
	for _, tag := range tags {
		owners := groupOwners(c.clusterEntries(), groups[tag])
		if len(owners) == 0 {
			continue
		}
		agree := true
		for i := 1; i < len(owners); i++ {
			if owners[i].Pattern.Canonical() != owners[0].Pattern.Canonical() {
				agree = false
			}
		}
		if agree {
			defaults[tag] = owners[0].Pattern
		} else {
			split[tag] = owners
		}
	}
	return defaults, split
}

// groupOwners returns the entries that write anti-affinity rules and that
// hold at least one of members (see haClusterEntry.holds), in cpi-config
// order.
func groupOwners(entries []haClusterEntry, members []*haGuest) []haClusterEntry {
	var out []haClusterEntry
	for i := range entries {
		e := &entries[i]
		if !e.AntiAffinity {
			continue
		}
		for _, g := range members {
			if e.holds(g) {
				out = append(out, *e)
				break
			}
		}
	}
	return out
}

// adviseSplitDefaults reports a group whose owning entries set different
// default patterns while no member's create named one.
func (c *haCollector) adviseSplitDefaults(tag, group string, owners []haClusterEntry) {
	names := make([]string, 0, len(owners))
	patterns := make([]string, 0, len(owners))
	for i := range owners {
		names = append(names, owners[i].Name)
		patterns = append(patterns, owners[i].Name+" "+owners[i].Pattern.Canonical())
	}
	c.rep.advise(haKindAntiAffinityAmbiguous, tag,
		"the cpi-config entries %s write the rules of group %s and set different pve.ha.anti_affinity_pattern "+
			"defaults (%s), and no member's create named a pattern through ha_anti_affinity, so the layout the CPI "+
			"plans depends on which entry serves the call, and the doctor does not judge the group's layout; set "+
			"ha_anti_affinity on the group's vm_type, or give the entries one default",
		strings.Join(names, ", "), group, strings.Join(patterns, ", "))
}

// antiAffinityWrites reports whether the CPI writes anti-affinity rules at all.
// set_vm_metadata places a member in its group's rules only when
// pve.ha.anti_affinity is not off and scheduler-soft anti-affinity is enabled,
// so a cluster set up otherwise has no rule to judge a group against.
func (c *haCollector) antiAffinityWrites() bool {
	return antiAffinityWritten(c.cfg)
}

// antiAffinityWritten is antiAffinityWrites for one effective config.
func antiAffinityWritten(cfg *config.CPIConfig) bool {
	return cfg.HAAntiAffinityValue() != config.HARuleOff && cfg.AntiAffinityEnabled()
}

// layoutChecks plans every group that a CPI marker names, the way the CPI
// plans it, and returns each check by the group's base rule name. defaults
// holds the default pattern of some groups by their tag, which is also their
// base rule name, and every other group is planned with the run's default.
// Without the HA resources the plan cannot tell which members may join, so
// nothing is planned.
func (c *haCollector) layoutChecks(defaults map[string]aapattern.Pattern) map[string]handlers.AntiAffinityGroupCheck {
	return c.checksUnder(c.cfg, defaults)
}

// checksUnder is layoutChecks under the settings cfg.
func (c *haCollector) checksUnder(cfg *config.CPIConfig, defaults map[string]aapattern.Pattern,
) map[string]handlers.AntiAffinityGroupCheck {
	if !c.resourcesOK || c.p == nil {
		return nil
	}
	in := handlers.AntiAffinityCheckInput{
		Config: cfg, Profile: c.p, Owner: c.owner, GuestsComplete: !c.guestsPartial,
		Rules: &ha.RuleSet{Rules: c.rules}, Resources: make(map[string]string, len(c.resources)), Now: c.now,
		GroupPatterns: defaults,
	}
	for _, g := range c.guests {
		in.Guests = append(in.Guests, g.ref)
	}
	for sid, row := range c.resources {
		in.Resources[sid] = row.Comment
	}
	out := map[string]handlers.AntiAffinityGroupCheck{}
	checks := handlers.CheckAntiAffinityGroups(in)
	for i := range checks {
		out[checks[i].Rule] = checks[i]
	}
	return out
}

// layoutFindings reports what the plan of one group finds: rules its pattern
// gave up for drain safety, a pattern that gave drain safety up, rules that
// hold as many members as their nodes, and rules that differ from the plan.
func (c *haCollector) layoutFindings(chk handlers.AntiAffinityGroupCheck) {
	if chk.Problem != "" {
		c.unplannedFinding(chk.Rule, chk.Problem, chk.OtherEntry)
		return
	}
	if chk.ForeignRule != "" {
		c.unplannedFinding(chk.Rule, chk.ForeignRule, true)
		return
	}
	if chk.Pattern == aapattern.Off().Canonical() {
		c.groupOffFinding(chk)
		return
	}
	if len(chk.Trimmed) > 0 {
		c.rep.advise(haKindAAPatternTrimmed, chk.Rule,
			"the pattern %s of group %s asks for rules %s, which the CPI leaves out, because each would leave a "+
				"member with no free node while one node drains", chk.Pattern, chk.Group, strings.Join(chk.Trimmed, ", "))
	}
	if chk.BlocksDrain {
		c.rep.advise(haKindAAPatternBlocksDrain, chk.Rule,
			"the pattern %s of group %s sets drain_safe to false, so a drain of one node can leave a member "+
				"with no node to move to, and pve-ops ha drain stops such a member through HA", chk.Pattern, chk.Group)
	}
	for _, f := range chk.Full {
		text := "rule %s holds %d members on %d nodes, so a drain of one node leaves a member with no free node"
		if chk.BlocksDrain {
			c.rep.advise(haKindAARuleFull, f.Rule, text+", which the pattern's drain_safe false allows",
				f.Rule, f.Members, f.Nodes)
			continue
		}
		c.rep.problem(haKindAARuleFull, f.Rule, text+"; pve-ops ha --fix-anti-affinity --apply rewrites it",
			f.Rule, f.Members, f.Nodes)
	}
	for _, h := range chk.LapsedHolds {
		why := "expired at " + h.Until.Format(time.RFC3339)
		if h.Consumed {
			why = "has been taken by a member with its index"
		}
		c.rep.advise(haKindAAHoldLapsed, chk.Rule,
			"the hold of index %d of group %s, recorded on %s, %s, so it keeps no place, and the next write of "+
				"the group's rules drops it, as does pve-ops ha --fix-anti-affinity --apply",
			h.Index, chk.Group, strings.Join(h.Carriers, ", "), why)
	}
	for _, r := range chk.PinRestores {
		c.pinRestoreFinding(chk, r)
	}
	if len(chk.Drift) > 0 {
		c.rep.advise(haKindAALayoutDrift, chk.Rule,
			"the rules of group %s differ from the layout the CPI plans, which the next set_vm_metadata "+
				"of a member writes, or pve-ops ha --fix-anti-affinity --apply now: %s",
			chk.Group, strings.Join(chk.Drift, "; "))
	}
}

// groupOffFinding reports what is left of a group that opted out of the
// anti-affinity rules with the pattern off. Its plan deletes every rule of the
// group and keeps no hold, so the rules and holds still recorded from before
// the switch get one advice, which names each write that removes them, and
// a group with nothing left gets none. A pin restore is reported as for any
// group.
func (c *haCollector) groupOffFinding(chk handlers.AntiAffinityGroupCheck) {
	for _, r := range chk.PinRestores {
		c.pinRestoreFinding(chk, r)
	}
	left := slices.Clone(chk.Drift)
	for _, h := range chk.LapsedHolds {
		left = append(left, fmt.Sprintf("drop the hold of index %d from %s", h.Index, strings.Join(h.Carriers, ", ")))
	}
	if len(left) == 0 {
		return
	}
	c.rep.advise(haKindAAGroupOff, chk.Rule,
		"group %s has opted out of the anti-affinity rules with the pattern off, and it still has rules or holds "+
			"from before the switch, which the next set_vm_metadata or delete_vm of a member removes, or "+
			"pve-ops ha --fix-anti-affinity --apply now: %s",
		chk.Group, strings.Join(left, "; "))
}

// unplannedFinding reports a group whose rules could not be planned, with the
// reason why. It is a problem, except in a run of one entry when the reason
// names a rule that only another cpi_config entry's placement.az_map explains. Such
// a run can't see the other entries, and the group most likely belongs to one
// of them, so it gives advice that names --cpi-config, which judges the group
// by the entry that placed it and keeps the problem when no entry does.
func (c *haCollector) unplannedFinding(rule, reason string, otherEntry bool) {
	if otherEntry && c.scope == nil {
		c.rep.advise(haKindAALayoutDrift, rule, "%s", otherEntryAdvice("the doctor", reason))
		return
	}
	c.rep.problem(haKindAALayoutDrift, rule, "the group's rules could not be planned: %s", reason)
}

// pinRestoreFinding reports a member's pin that still records the priorities
// its join flattened while the member is in no resource-affinity rule.
func (c *haCollector) pinRestoreFinding(chk handlers.AntiAffinityGroupCheck, r handlers.AntiAffinityPinRestore) {
	when := "the next write of the group's rules, such as a member's set_vm_metadata, %s, as does pve-ops ha " +
		"--fix-anti-affinity --apply"
	if r.Deferred {
		when = "the group's first write after its live holds lapse or their recreates join %s, as does pve-ops ha " +
			"--fix-anti-affinity --apply then"
	}
	if r.Action == "drop" {
		c.rep.advise(haKindAAPinRestore, r.Pin,
			"pin %s of %s in group %s records the priorities a join flattened, but the pin changed since, because "+
				"%s, and "+when, r.Pin, r.SID, chk.Group, r.Reason,
			"drops the record and keeps the pin's nodes and priorities")
		return
	}
	c.rep.advise(haKindAAPinRestore, r.Pin,
		"pin %s of %s in group %s has one priority on every node, which a join gave it, while %s is in no "+
			"anti-affinity rule now, and "+when, r.Pin, r.SID, chk.Group, r.SID,
		"writes back the priorities "+r.Nodes)
}

// groupRules returns the rules that carry a group's name: the rule itself,
// the numbered rules an overflow split adds, and the rules a pattern adds.
func (c *haCollector) groupRules(tag string) []*ha.Rule {
	var rules []*ha.Rule
	for i := range c.rules {
		if aapattern.IsGroupRule(tag, c.rules[i].ID) {
			rules = append(rules, &c.rules[i])
		}
	}
	return rules
}

// checkGroup judges one group against its rules and the cluster's node count.
// A rule holds at most one member fewer than the cluster's nodes, so a group
// with more members than that overflows its first rule.
func (c *haCollector) checkGroup(tag string, members []*haGuest, rules []*ha.Rule) {
	nodes := len(c.p.Nodes)
	if room := nodes - 1; nodes > 0 && len(members) > room {
		if len(rules) == 0 {
			c.rep.advise(haKindAntiAffinitySkipped, tag,
				"the group has %d members but a rule holds at most %d on the cluster's %d nodes, and the group "+
					"has no rule", len(members), room, nodes)
		} else {
			text := fmt.Sprintf("the group has %d members but a rule holds at most %d on the cluster's %d nodes, "+
				"so it is split across %d rules", len(members), room, nodes, len(rules))
			for _, g := range members {
				if g.resource != nil && !namedByAny(rules, g.sid) {
					text += fmt.Sprintf("; %s is in no anti-affinity rule for group %s", g.sid, tag)
				}
			}
			c.rep.advise(haKindAntiAffinitySplit, tag, "%s", text)
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
			"the group's rule does not name %s, so the group is short of a member until set_vm_metadata or "+
				"pve-ops ha --fix-anti-affinity --apply adds it",
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
		if !g.owned || g.ref.Template || g.cfg == nil || !c.judgesVM(g) {
			continue
		}
		cpu, _ := pve.ConfigStringValue(g.cfg["cpu"])
		if !ha.CPUTypeNeedsDomain(cpu) {
			continue
		}
		dom, known := c.p.DomainOf(g.ref.Node)
		if len(c.p.CPUDomains) > 1 {
			c.cpuMultiDomain(g, cpu, dom, known)
		}
		if !c.off() && c.p.Effective != ha.EffectiveObserveOnly && c.resourcesOK && g.resource == nil && !c.inFlight(g) &&
			(!known || len(dom.Nodes) < 2) {
			why := "the CPU of node " + g.ref.Node + " is unknown"
			if known {
				why = fmt.Sprintf("its CPU migration domain holds %d node(s)", len(dom.Nodes))
			}
			c.rep.problem(haKindCPUDomainTooSmall, g.sid,
				"the VM has CPU type %s and got no HA, because %s; set pve.cpu_type to a named model and run bosh recreate on the instance", cpu, why)
		}
	}
}

// cpuMultiDomain judges a host or max VM on a cluster with several CPU
// migration domains against the node-affinity pin it actually has. A pin that
// names a node outside the VM's domain is a problem, and every other case is
// advice worded for what the VM has.
func (c *haCollector) cpuMultiDomain(g *haGuest, cpu string, dom ha.CPUDomain, known bool) {
	n := len(c.p.CPUDomains)
	if !known {
		c.rep.advise(haKindCPUHostMulti, g.sid,
			"CPU type %s on a cluster with %d CPU migration domains needs a domain, and the CPU of node %s cannot place the VM in one; set pve.cpu_type to a named model and run bosh recreate on the instance to let it move",
			cpu, n, g.ref.Node)
		return
	}
	domain := strings.Join(dom.Nodes, ", ")
	switch {
	case !c.resourcesOK || !c.rulesOK:
		c.rep.advise(haKindCPUHostMulti, g.sid,
			"CPU type %s on a cluster with %d CPU migration domains needs the domain %s, and the doctor could not read the HA resources and rules to check the VM's pin; set pve.cpu_type to a named model and run bosh recreate on the instance to let it move",
			cpu, n, domain)
		return
	case g.resource == nil:
		c.rep.advise(haKindCPUHostMulti, g.sid,
			"CPU type %s on a cluster with %d CPU migration domains needs the domain %s, and the VM is not under HA, so no node-affinity rule moves it; set pve.cpu_type to a named model and run bosh recreate on the instance to let it move",
			cpu, n, domain)
		return
	}
	pin := c.pinOf(g.ref.VMID)
	if pin == nil || pin.Disabled || pin.Type != ha.RuleTypeNodeAffinity || !pin.HasResource(g.sid) {
		c.rep.problem(haKindCPUHostMulti, g.sid,
			"CPU type %s on a cluster with %d CPU migration domains needs the domain %s, and the VM is under HA with no enabled node-affinity pin, so HA may move it to any node, including one whose CPU differs, and crash the guest; run bosh recreate on the instance so the CPI registers HA with a pin inside the domain, or set pve.cpu_type to a named model first and then recreate it",
			cpu, n, domain)
		return
	}
	var outside []string
	for node := range pin.Nodes {
		if !dom.Contains(node) {
			outside = append(outside, node)
		}
	}
	sort.Strings(outside)
	if len(outside) > 0 {
		c.rep.problem(haKindCPUPinCrossesDomain, g.sid,
			"CPU type %s needs the domain %s, but the node-affinity rule %s also names %s, so a maintenance drain or a manual migration can move the VM to a node whose CPU differs and crash the guest; "+
				"narrow the pin to the domain by running bosh recreate on the instance, which makes the CPI register HA again, or set pve.cpu_type to a named model and then run bosh recreate on the instance",
			cpu, domain, pin.ID, strings.Join(outside, ", "))
		return
	}
	c.rep.advise(haKindCPUHostMulti, g.sid,
		"CPU type %s on a cluster with %d CPU migration domains needs the domain %s, and the node-affinity rule %s names only nodes inside it; set pve.cpu_type to a named model and run bosh recreate on the instance to let the VM move to other domains",
		cpu, n, domain, pin.ID)
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

// judgesForProtection reports whether the protection findings apply to g. A
// parker holds detached disks by design and is stopped, so it is never an
// unprotected VM.
func (c *haCollector) judgesForProtection(g *haGuest) bool {
	return g.owned && !g.ref.Template && g.cfg != nil && !g.hasTag(pve.ParkerTag) && c.judgesVM(g)
}

// protectionFindings reports local-disk VMs that run unprotected, VMs whose
// confinement pin is gone, and hold VMs on a node entering maintenance.
func (c *haCollector) protectionFindings() {
	if c.off() || !c.guestsOK || !c.resourcesOK || !c.storagesOK {
		return
	}
	policy := c.cfg.HALocalPolicyValue()
	for _, g := range c.guests {
		if !c.judgesForProtection(g) {
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
			if c.scope.ambiguous() {
				if c.scope.Leader {
					c.rep.advise(haKindLocalUnprotected, g.sid,
						"VM %d on %s holds node-local volumes (%s) and runs without HA; the cpi-config entries %s share its cluster and VM ID band "+
							"and set pve.ha.local_policy to %s, so the doctor cannot tell which entry owns it",
						g.ref.VMID, g.ref.Node, strings.Join(local, ", "), c.scope.peerList(), c.scope.policyList())
				}
				continue
			}
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
		if len(pin.Nodes) == 1 && !c.replicates(g.ref.VMID, pin) && haBlocksDrain(g.resource.State) {
			if c.inMaintenance(g.ref.Node) {
				c.holdReported[g.sid] = true
				c.rep.problem(haKindHoldMaintenance, g.sid,
					"VM %d is held on %s, which is entering maintenance, and its node-local volumes keep it from moving; PVE cannot drain the node until the VM is stopped",
					g.ref.VMID, g.ref.Node)
			}
		}
	}
}

// settleDrain fills the drain facts of each node line that is in maintenance.
// The LRM enters state maintenance as soon as maintenance is requested and
// keeps the agent lock and the watchdog until no service is left on the node,
// so its state cannot say whether the drain has finished. PVE counts every HA
// service, containers included, so the doctor reads each service's node from
// the manager status. A node counts as drained once no HA service whose
// requested state keeps it running is on it. A service that keeps running and
// has no node in the status, an unreadable status, or an unreadable resource
// list leaves the answer unknown, and the node is not called drained. A VM
// that the guest listing places on the node also counts as running there.
func (c *haCollector) settleDrain() {
	for i := range c.rep.Nodes {
		n := &c.rep.Nodes[i]
		// A node in maintenance only by the marker in its notes is not
		// drained by PVE, so it has no drain to settle.
		if !n.Maintenance || n.NotesTag != "" {
			continue
		}
		known := c.resourcesOK && c.svcNodesOK
		running := false
		if c.resourcesOK {
			for sid, row := range c.resources {
				if !haBlocksDrain(row.State) {
					continue
				}
				switch node, listed := c.svcNodes[sid]; {
				case listed && node == n.Name:
					running = true
				case !listed || node == "":
					known = false
				}
			}
		}
		for _, g := range c.guests {
			if g.ref.Node == n.Name && g.resource != nil && haBlocksDrain(g.resource.State) {
				running = true
				break
			}
		}
		n.DrainComplete = known && !running
		if n.LRM != "mode=maintenance/state=maintenance" {
			continue
		}
		switch {
		case n.DrainComplete:
			n.Watchdog = string(pve.WatchdogIdle)
		case running:
			n.Watchdog = string(pve.WatchdogArmed)
		}
	}
}

// haBlocksDrain says whether a requested HA state keeps a service running on
// its node, so that a drain cannot finish until the service moves or stops.
func haBlocksDrain(state string) bool {
	return haRunsUnderHA(state)
}

// inMaintenance says whether a node is in PVE HA maintenance, which the CRM
// drains. A node in maintenance only by the marker in its notes keeps its
// services, so it does not count. A caller that holds a service whose requested state keeps it running on the
// node has already shown that the drain is not finished, because PVE keeps the
// node's agent lock until the last such service has left. It says no when the
// health report could not be built, which is already reported as unchecked.
func (c *haCollector) inMaintenance(name string) bool {
	if c.p.Health == nil {
		return false
	}
	h, found := healthEntry(c.p, name)
	return found && haMaintenance(h)
}

// haMaintenance says whether a health entry is in PVE HA maintenance. The
// notes are read only for a node that is otherwise placeable, so an entry
// with a notes tag is in maintenance by its notes alone, which keeps the CPI
// from placing on it but moves nothing and leaves it open to the CRM.
func haMaintenance(h pve.NodeHealthEntry) bool {
	return h.Maintenance && h.NotesTag == ""
}

// maintenanceStrandedFindings reports each started HA VM that is still running
// on a node in maintenance, once and with the reason it cannot leave when the
// doctor can see one. The CRM moves every service off such a node, and one that
// has nowhere to go stays behind and keeps the node's LRM from finishing its
// drain. A hold VM is already reported as hold_blocks_maintenance, and a node
// with no service left that keeps running has nothing left to report. A node health
// report that could not be built is already reported as unchecked, so this
// check says nothing in that case.
func (c *haCollector) maintenanceStrandedFindings() {
	if c.off() || !c.guestsOK || !c.resourcesOK || c.p.Health == nil {
		return
	}
	for _, g := range c.guests {
		if !g.owned || g.ref.Template || g.resource == nil || !c.judgesVM(g) || c.holdReported[g.sid] {
			continue
		}
		if !strings.EqualFold(g.resource.State, "started") {
			continue
		}
		if !c.inMaintenance(g.ref.Node) {
			continue
		}
		reason := c.cannotLeaveReason(g)
		if reason == "" {
			// A soft node-affinity pin does not hold a VM on a node in maintenance, because PVE
			// moves it to the next priority class, so with no reason the VM is most likely on its way.
			c.rep.advise(haKindMaintenanceStranded, g.sid,
				"%s is still running on %s, which is in maintenance, and the doctor sees nothing that stops it from leaving, so PVE is expected to move it. "+
					"We check again after the HA manager's next round, because the node's HA agent can't finish draining until the VM has moved or stopped",
				g.sid, g.ref.Node)
			continue
		}
		c.rep.problem(haKindMaintenanceStranded, g.sid,
			"%s is still running on %s, which is in maintenance, so the node's HA agent can't restart until it moves or stops%s",
			g.sid, g.ref.Node, reason)
	}
}

// cannotLeaveReason says why a service on a node in maintenance has no node to
// move to, or returns nothing when the doctor sees no such reason. It starts
// from the nodes PVE would move the service to, which recoveryNodes gives, and
// narrows them the way PVE does: a strict pin keeps only its nodes, and then
// each node that holds an active partner from the VM's own negative
// resource-affinity rules drops out. The first step that leaves no node gives
// the reason.
func (c *haCollector) cannotLeaveReason(g *haGuest) string {
	takers := c.recoveryNodes(g.ref.Node)
	if len(takers) == 0 {
		return ", and no other online node is out of HA maintenance"
	}
	if pin := c.pinOf(g.ref.VMID); c.rulesOK && pin != nil && !pin.Disabled && pin.Strict {
		takers = slices.DeleteFunc(takers, func(n string) bool { return !hasNode(pin.Nodes, n) })
		if len(takers) == 0 {
			return ", and its strict node-affinity pin names no other node it can move to"
		}
	}
	if c.partnersHoldEvery(g, takers) {
		return ", and every other node that can take it holds a partner from its anti-affinity rules"
	}
	return ""
}

// recoveryStrandedFindings reports each HA VM the HA manager holds in the
// recovery state. After a node fails and is fenced, the manager recovers each
// of its services onto another node, and one with no admissible node stays in
// recovery until one appears. The failed node is offline, so the per-node guest
// listing cannot ask it, and the VM is matched through the cluster's VM list
// instead, which still names it with its node and tags. A manager status that
// could not be read is already reported as unchecked, so this check says
// nothing in that case.
func (c *haCollector) recoveryStrandedFindings() {
	if c.off() || !c.resourcesOK || !c.statesOK {
		return
	}
	sids := make([]string, 0, len(c.states))
	for sid, state := range c.states {
		if strings.EqualFold(state, "recovery") {
			sids = append(sids, sid)
		}
	}
	sort.Strings(sids)
	for _, sid := range sids {
		if _, ok := c.resources[sid]; !ok {
			continue
		}
		g := c.recoveryGuest(sid)
		if g == nil || !g.owned || g.ref.Template || !c.judgesVM(g) {
			continue
		}
		why := ""
		pin := c.pinOf(g.ref.VMID)
		switch {
		case c.rulesOK && pin != nil && len(pin.Nodes) == 1 && hasNode(pin.Nodes, g.ref.Node):
			why = fmt.Sprintf(", because its strict pin names only %s", g.ref.Node)
		case c.partnersHoldEvery(g, c.recoveryNodes(g.ref.Node)):
			why = ", because every other node holds a partner from its anti-affinity rules"
		}
		c.rep.problem(haKindRecoveryStranded, g.sid,
			"%s was on %s and waits in recovery with no node it may run on%s", g.sid, g.ref.Node, why)
	}
}

func hasNode(nodes map[string]int, name string) bool {
	_, ok := nodes[name]
	return ok
}

// recoveryGuest returns the VM behind an HA service in recovery. The cluster's
// VM list comes first because it names a VM on an offline node, and the
// per-node listing is the fallback when that list could not be read.
func (c *haCollector) recoveryGuest(sid string) *haGuest {
	vmid, err := strconv.Atoi(strings.TrimPrefix(sid, "vm:"))
	if err != nil || vmid <= 0 {
		return nil
	}
	ref, ok := c.clusterVMs[vmid]
	if !c.clusterOK || !ok {
		if g := c.byVMID[vmid]; g != nil {
			return g
		}
		return nil
	}
	g := &haGuest{ref: ref, tags: map[string]bool{}, sid: sid}
	for _, t := range ref.SplitTags() {
		g.tags[strings.ToLower(strings.TrimSpace(t))] = true
	}
	g.owned = g.hasTag(haOwnershipTag)
	return g
}

// recoveryNodes returns the online nodes other than from that are not in PVE
// HA maintenance, which are the nodes the CRM may move or recover a service
// onto. The CRM knows nothing of the CPI's own placement filters or of the
// maintenance marker in a node's notes, so a node the CPI won't place on still
// counts. A node the health report doesn't list counts too, because the doctor
// can't show that the CRM refuses it.
func (c *haCollector) recoveryNodes(from string) []string {
	var out []string
	for i := range c.p.Nodes {
		n := &c.p.Nodes[i]
		if !n.Online || n.Name == from {
			continue
		}
		// The CRM can still move a VM to a node marked only in its notes.
		if h, ok := healthEntry(c.p, n.Name); ok && haMaintenance(h) {
			continue
		}
		out = append(out, n.Name)
	}
	return out
}

// partnersHoldEvery reports whether every node in nodes holds an active
// partner of g, and nodes is not empty. A partner is another resource of an
// enabled negative resource-affinity rule that names g and that PVE applies,
// so a rule with feasibility errors doesn't count. Only the rules that name g
// count, not every member of g's group, because a group that splits into
// several rules, or leaves a member out of every rule, keeps g apart only from
// the members its own rules name. Partners from every such rule add up, the
// operator's rules included, because PVE applies them all. A partner the HA
// manager reports stopped, disabled, or ignored holds no node, because PVE
// counts only active services.
func (c *haCollector) partnersHoldEvery(g *haGuest, nodes []string) bool {
	if !c.rulesOK || !c.clusterOK || len(nodes) == 0 {
		return false
	}
	held := map[string]bool{}
	for i := range c.rules {
		r := &c.rules[i]
		if r.Type != ha.RuleTypeResourceAffinity || r.Affinity != ha.AffinityNegative || r.Disabled ||
			len(r.Errors) > 0 || !r.HasResource(g.sid) {
			continue
		}
		for _, partner := range r.Resources {
			if node, ok := c.activePartnerNode(partner, g.sid); ok {
				held[node] = true
			}
		}
	}
	for _, n := range nodes {
		if !held[n] {
			return false
		}
	}
	return true
}

// activePartnerNode returns the node of the partner sid when it is a VM other
// than self that the HA manager counts as active.
func (c *haCollector) activePartnerNode(sid, self string) (string, bool) {
	if sid == self {
		return "", false
	}
	if c.statesOK {
		switch strings.ToLower(c.states[sid]) {
		case "stopped", "disabled", "ignored":
			return "", false
		}
	}
	vmid, err := strconv.Atoi(strings.TrimPrefix(sid, "vm:"))
	if err != nil || !strings.HasPrefix(sid, "vm:") {
		return "", false
	}
	ref, ok := c.clusterVMs[vmid]
	return ref.Node, ok && ref.Node != ""
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
// are not being removed, do not belong to another Director, and, under a
// cpi-config entry, name a VM the entry judges (see judgesVMID).
func (c *haCollector) markedJobs() []ha.ReplicaJobState {
	var out []ha.ReplicaJobState
	for _, j := range c.jobs {
		if j.Fault == "" && j.RemoveJob == "" && haJobMarker(j.Comment) && !c.foreign(j.Comment) && c.judgesVMID(j.Guest) {
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
		c.encodedMarker(j)
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

// encodedMarker advises on a CPI job whose comment PVE percent-encoded again
// after the CPI wrote it. PVE does that to every job's comment when one
// scheduler pass writes replication.cfg more than once, as when HA recovers
// several replicated guests onto one node. The job is one markedJobs keeps,
// and it reads as encoded through ha.ParseReplicaJobMarker, the reading the
// CPI uses, so the doctor and the CPI agree on which jobs carry the marker and
// which of those are encoded. The CPI writes the plain marker back on the
// next set_vm_metadata or reboot_vm on the job's VM, with or without an
// allocation journal. A CPI that predates the tolerant reader disowns the job
// until then, so the advice also names the command that restores the plain
// comment now. That command writes ha.DecodedMarkerComment, which is the
// comment the CPI writes.
func (c *haCollector) encodedMarker(j ha.ReplicaJobState) {
	if m, ok := ha.ParseReplicaJobMarker(j.Comment); !ok || !m.Encoded {
		return
	}
	c.rep.advise(haKindReplicaMarkerEncode, "job "+j.ID,
		"PVE percent-encoded the comment of replication job %s again when it rewrote replication.cfg, so it "+
			"reads %q. This CPI still reads it as its marker, but an older CPI does not. The CPI writes the plain "+
			"marker back on the next set_vm_metadata or reboot_vm on VM %d, whether or not an allocation journal is "+
			"configured. To restore it now, run pvesr update %s --comment %s",
		j.ID, j.Comment, j.Guest, j.ID, haShellQuote(ha.DecodedMarkerComment(j.Comment)))
}

// haShellQuote quotes s as one POSIX shell word, so advice an operator pastes
// into a shell cannot run text that a job comment carries. It wraps s in single
// quotes and writes each single quote inside it as '\”.
func haShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		if rep != nil && rep.Job != nil && !rep.Retired && c.judgesVMID(rep.Job.Guest) {
			vmids[rep.Job.Guest] = true
		}
	}
	for _, g := range c.guests {
		if c.judges(g) && g.hasTag(haReplicaTag) {
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
// When the run knows the requesting Director, the job's marker must name that
// Director, the way the CPI picks the one job its pin speaks for. More than one
// such job is an error, because the CPI refuses to pick among them too.
func (c *haCollector) liveJob(vmid int) (*ha.ReplicaJobState, error) {
	var found *ha.ReplicaJobState
	ours := haOwnerValue(c.owner)
	for _, j := range c.markedJobs() {
		if j.Guest != vmid {
			continue
		}
		if owner, _ := haMarkerOwner(j.Comment); c.owner != "" && owner != ours {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("VM %d has replication jobs %s and %s that both carry this Director's marker, "+
				"and the CPI refuses to pick one", vmid, found.ID, j.ID)
		}
		job := j
		found = &job
	}
	return found, nil
}

// replicaTagged reports whether any CPI-owned VM carries the replicate tag.
func (c *haCollector) replicaTagged() bool {
	for _, g := range c.guests {
		if c.judges(g) && g.hasTag(haReplicaTag) {
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
		source, partner, shaped := ha.ReplicaPinShape(pin)
		if !shaped || partner == "" {
			source, partner = "", token
			if rec != nil && rec.Replication.Job != nil {
				partner, source = rec.Replication.Job.Target, rec.Replication.Job.Source
			}
		}
		if partner != "" && g.ref.Node == partner {
			c.onPartner(g, pin, partner, source)
		}
		if !shaped && token != "" && pin.Strict && !pin.Disabled && len(pin.NodeNames()) == 1 {
			c.rep.advise(haKindReplicaPinUnshaped, g.sid,
				"replicate VM %d has pin %s, which names only %s but still carries the %s=%s token, so the "+
					"widen sweep skips it as not a replicate pin. Remove the token from the pin's comment, then run "+
					"pve-ops ha --widen-replicas --apply to widen it", id, pin.ID, pin.NodeNames()[0],
				ha.ReplicaPartnerKey, token)
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

// onPartner judges a replicate VM that runs on node, the partner its pin
// names, with source the node the pin or the journal names as its source. It
// applies ha.JudgePartnerMove, the rule the CPI applies when a call touches
// the VM. A planned migration whose reversed sync has run is advice, because
// the next reboot_vm or set_vm_metadata keeps both nodes and moves the
// partner token. Anything else is a problem, since the VM got there by a
// failover or by a move the CPI has not reconciled yet. While no reversed sync
// has run, PVE's next run rolls the old source's copy back to match.
func (c *haCollector) onPartner(g *haGuest, pin *ha.Rule, node, source string) {
	id := g.ref.VMID
	move := ha.PartnerMove{Node: node, OldSource: source}
	if c.jobsOK {
		job, err := c.liveJob(id)
		if err != nil {
			c.rep.problem(haKindReplicaOnPartner, g.sid, "replicate VM %d runs on its partner %s, and the move can't "+
				"be judged: %v", id, node, err)
			return
		}
		move.Job = job
	}
	for i := range c.p.Nodes {
		if c.p.Nodes[i].Name == source {
			move.OldSourceOnline = c.p.Nodes[i].Online
		}
	}
	if pin != nil {
		move.ChangedAt, _ = ha.ReplicaChanged(pin.Comment)
	}
	var unsynced string
	move.Synced, unsynced = c.reversedSync(g, move)
	var unread error
	if move.Synced {
		move.Migration, unread = c.migrationEvidence(id, node, source, move.ChangedAt)
	}
	verdict := ha.JudgePartnerMove(move)
	const reconcile = "the next reboot_vm or set_vm_metadata keeps both nodes and names %s as the partner once " +
		"it confirms that sync"
	const moved = "replicate VM %d runs on its partner %s after a failover, or after a move the CPI has not " +
		"reconciled yet"
	const synced = "a sync from %s back to %s that covers the VM's volumes has run since"
	switch verdict {
	case ha.PartnerMovePlanned:
		c.rep.advise(haKindReplicaOnPartner, g.sid, "replicate VM %d runs on its partner %s after a planned "+
			"migration, and "+synced+"; "+reconcile, id, node, node, source, source)
	case ha.PartnerMoveRecovered:
		c.rep.problem(haKindReplicaOnPartner, g.sid, moved+"; no migration task on %s explains the move, so writes "+
			"made on %s after its last sync before the move were lost, and "+synced+"; "+reconcile, id, node,
			source, source, node, source, source)
	case ha.PartnerMoveRestarted:
		c.rep.problem(haKindReplicaOnPartner, g.sid, moved+"; a migration from %s explains the move, but the VM "+
			"also started outside a migration since the pin last changed, which an HA recovery does, so writes "+
			"made after the last sync before that start may have been lost, and "+synced+"; "+reconcile, id, node,
			source, node, source, source)
	case ha.PartnerMoveUnproven:
		c.rep.problem(haKindReplicaOnPartner, g.sid, moved+"; "+synced+", but the task lists or task logs that "+
			"would prove a planned migration could not be read (%v), so writes made on %s after its last sync "+
			"before the move may have been lost; "+reconcile, id, node, node, source, unread, source, source)
	default:
		detail := ""
		if verdict == ha.PartnerMoveUndecided && unsynced != "" {
			detail = fmt.Sprintf("; no sync from %s back to %s that covers the VM's volumes is confirmed yet (%s)",
				node, source, unsynced)
		}
		if source != "" && move.Job != nil && !move.Job.Disabled && move.Job.RemoveJob == "" {
			detail += fmt.Sprintf("; PVE's next run copies from %s back to %s and rolls %s's copy back to match",
				node, source, source)
		}
		c.rep.problem(haKindReplicaOnPartner, g.sid, moved+"%s", id, node, detail)
	}
}

// reversedSync runs the CPI's sync gate (see ha.CheckReplicaSync) on the
// reversed job of a VM that runs on its partner, with the volumes the CPI
// would check, so the doctor and the next call reach the same verdict. It
// reports whether a sync from the VM's node covers those volumes on the old
// source after the pin's last change, and otherwise why not. A job that isn't
// reversed needs no sync, because the move then reads as a failover anyway.
func (c *haCollector) reversedSync(g *haGuest, move ha.PartnerMove) (bool, string) {
	job := move.Job
	switch {
	case job == nil || move.Node == "" || move.OldSource == "" || job.Source != move.Node ||
		job.Target != move.OldSource:
		return false, ""
	case g.cfg == nil || !c.storagesOK:
		return false, "the VM's config or the storage index could not be read"
	}
	storages := make([]pve.StorageInfo, 0, len(c.storages))
	for _, name := range slices.Sorted(maps.Keys(c.storages)) {
		storages = append(storages, c.storages[name])
	}
	volids, gaps := ha.ReplicaVolumes(g.ref.VMID, g.volumes, storages)
	if len(gaps) > 0 {
		return false, "the VM holds node-local volumes its job does not copy: " + strings.Join(gaps, "; ")
	}
	verdict, err := ha.CheckReplicaSync(c.ctx, doctorReplicaReads{c: c}, ha.ReplicaSyncCheck{
		JobID: job.ID, Source: move.Node, Target: move.OldSource, VolIDs: volids, ChangedAt: move.ChangedAt,
	})
	switch {
	case err != nil:
		return false, err.Error()
	case !verdict.Verified:
		return false, verdict.Reason
	}
	return true, ""
}

// errDoctorReadOnly is the answer of every write doctorReplicaReads is asked
// for.
var errDoctorReadOnly = errors.New("the doctor makes no replication writes")

// doctorReplicaReads is the read side of ha.ReplicationAPI over the doctor's
// reader, so the doctor can run the CPI's sync gate. It reuses the job statuses
// the doctor already read, and every write fails.
type doctorReplicaReads struct{ c *haCollector }

func (d doctorReplicaReads) ListJobs(context.Context) ([]ha.ReplicaJobState, error) {
	return d.c.jobs, nil
}

func (doctorReplicaReads) CreateJob(context.Context, ha.ReplicaJobSpec) error {
	return errDoctorReadOnly
}

func (doctorReplicaReads) SetComment(context.Context, string, string) error { return errDoctorReadOnly }

func (doctorReplicaReads) DeleteJob(context.Context, string, bool) error { return errDoctorReadOnly }

func (doctorReplicaReads) ScheduleNow(context.Context, string, string) error {
	return errDoctorReadOnly
}

func (d doctorReplicaReads) Status(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	if st, ok := d.c.jobStatus[id]; ok {
		return st, nil
	}
	return d.c.r.ReplicaStatus(ctx, node, id)
}

func (d doctorReplicaReads) Volumes(ctx context.Context, node, storage string) (map[string]bool, error) {
	return d.c.r.StorageVolumes(ctx, node, storage)
}

// migrationEvidence reads the qmigrate and qmstart tasks of the VM vmid on
// the old source and on node, and judges them (see ha.ReadMigrationEvidence),
// the read the CPI makes too. A listing or log read that fails is
// MigrationUnread with its cause.
func (c *haCollector) migrationEvidence(
	vmid int, node, source string, changedAt time.Time,
) (ha.MigrationEvidence, error) {
	return ha.ReadMigrationEvidence(c.ctx, c.r, vmid, node, source, changedAt)
}

// notRecoverable judges a replicate VM whose pin does not yet record a
// verified replica. create_vm does not wait for the first sync. The pin widens
// once a sync that started after the last volume change is verified, on a
// later reboot_vm or set_vm_metadata, in the sweep after any CPI call that
// takes the HA rules lock, or with pve-ops ha --widen-replicas --apply, so a VM
// with a working job is only awaiting that sync.
func (c *haCollector) notRecoverable(g *haGuest, pin *ha.Rule, rec *aj.Record) {
	id := g.ref.VMID
	if !c.jobsOK {
		return
	}
	job, err := c.liveJob(id)
	switch {
	case err != nil:
		c.rep.problem(haKindReplicaNotRecover, g.sid, "replicate VM %d can't be judged: %v", id, err)
		return
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
	const widens = "; once a sync is verified, the pin widens to the partner after the next CPI call that takes the HA " +
		"rules lock, or now with pve-ops ha --widen-replicas --apply"
	switch {
	case pin != nil && !st.LastSync.IsZero() && (!haveChange || st.LastSync.After(changed.Add(aj.ReplicaClockSkewTolerance))):
		c.awaiting[id] = true
		c.rep.advise(haKindReplicaAwaiting, g.sid,
			"replicate VM %d: job %s synced at %s, but the pin does not yet name the partner as holding a replica%s",
			id, job.ID, st.LastSync.UTC().Format(time.RFC3339), widens)
	case c.firstSyncOverdue(job.ID, changed, haveChange):
		c.rep.problem(haKindReplicaNotRecover, g.sid,
			"replicate VM %d is configured to replicate but job %s has not completed a sync that started after the volume change at %s, "+
				"more than twice its schedule since", id, job.ID, changed.UTC().Format(time.RFC3339))
	case pin == nil:
		// With no pin there is nothing for a later call to widen, so the VM is
		// not awaiting a widening. The local-volume and HA findings speak for it.
		return
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
// VM's and that no replication job targets, and the volumes of this
// Director's VMs that outlived their VM (see goneVMVolume).
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
	jobGuests := map[int]bool{}
	for _, j := range c.jobs {
		jobGuests[j.Guest] = true
		if j.Fault == "" {
			covered[strconv.Itoa(j.Guest)+"@"+j.Target] = true
		}
	}
	gone := c.goneVMVolumeCheck(jobGuests)
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
			c.orphanVolumesOn(node.Name, s.Name, covered, gone)
		}
	}
}

// orphanVolumesOn checks the volumes of storage on node for orphanVolumes.
// covered holds "<vmid>@<target>" for each healthy job, and gone is the check
// for a volume whose VM is gone.
func (c *haCollector) orphanVolumesOn(node, storage string, covered map[string]bool, gone func(string, string, int)) {
	volumes, err := c.r.StorageVolumes(c.ctx, node, storage)
	if err != nil {
		c.rep.unchecked("storage "+storage+"@"+node,
			"the volumes of %s on %s could not be listed, so replica orphans there are unchecked: %v", storage, node, err)
		return
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
		if !ok {
			gone(volid, node, vmid)
			continue
		}
		if !g.owned || !c.judgesVM(g) || g.ref.Node == node || covered[m[1]+"@"+node] {
			continue
		}
		c.rep.problem(haKindReplicaOrphanVolume, volid,
			"%s on %s is a copy of VM %d, which runs on %s, and no replication job targets %s for it; remove it by hand once nothing needs it",
			volid, node, vmid, g.ref.Node, node)
	}
}

// goneVMVolumeCheck returns the check orphanVolumes runs on a volume whose
// VMID names no guest in the cluster. Such a volume is most often the copy a
// replication job left on its target when PVE purged the job with the VM, as
// a forced retirement with no journal does, or as a destroy does when the CPI
// left a job it did not recognize as its own. Once the VM is gone the volume
// carries no ownership marker, and the PVE API does not list its replication
// snapshots, so the doctor infers that it is this Director's only from the
// VMID, which must fall inside the configured VM band, and under --cpi-config
// in a band this entry judges (see judgesVMID), and from a name the
// CPI gives a VM's volumes. It also requires that no guest config the doctor
// read references the volume, that no replication job names the VMID, and
// that the journal has not recorded the copy already, which recordedCopies
// reports. Because the ownership is inferred, the finding is advice. The
// check is off while offline nodes keep guests out of the list, because a
// missing VMID then proves nothing.
func (c *haCollector) goneVMVolumeCheck(jobGuests map[int]bool) func(volid, node string, vmid int) {
	if c.guestsPartial || c.cfg == nil {
		return func(string, string, int) {}
	}
	if c.configsFailed {
		c.rep.unchecked("replica_gone_vm_volume", "the config of an owned VM could not be read, so a volume it holds "+
			"cannot be told from a leftover, and the check for volumes of a VM that no longer exists is skipped")
		return func(string, string, int) {}
	}
	start, end := c.cfg.VMIDRangeStart, c.cfg.VMIDRangeEnd
	referenced := map[string]bool{}
	for _, g := range c.guests {
		for _, v := range g.volumes {
			referenced[v.Volume] = true
		}
	}
	recorded := map[string]bool{}
	if c.journalOK {
		for i := range c.journal.Records {
			rep := c.journal.Records[i].Replication
			if rep == nil {
				continue
			}
			for _, cp := range rep.Copies {
				for _, v := range cp.VolIDs {
					recorded[v+"@"+cp.Node] = true
				}
			}
		}
	}
	return func(volid, node string, vmid int) {
		if vmid < start || vmid > end || !c.judgesVMID(vmid) || jobGuests[vmid] || referenced[volid] ||
			recorded[volid+"@"+node] || !haCPIVolumePattern.MatchString(volid) {
			return
		}
		c.rep.advise(haKindReplicaGoneVM, volid,
			"%s on %s carries VMID %d, which falls in this Director's VM band (%d to %d) and names no guest in the "+
				"cluster, and its name is one the CPI gives a VM's volumes. It is most likely a replica copy that PVE "+
				"left when it purged the VM's job, or a volume a deletion left. Nothing removes it, so we remove it "+
				"by hand, in this order. First, check the whole cluster, because the doctor read only the configs "+
				"of the VMs it owns and so cannot rule out a VM it does not own. Run grep -s -l -e '%s' -e '%d' "+
				"/etc/pve/nodes/*/qemu-server/*.conf /etc/pve/nodes/*/lxc/*.conf and pvesh get /cluster/resources --type vm | grep -w %d, and "+
				"expect both to print nothing. Also run pvesr list | grep -w %d on every node, which lists only that "+
				"node's jobs, and expect no output. A config that names the volume means some VM or container owns it, so we "+
				"leave it alone. Second, on %s, run pvesm path %s, which prints /dev/zvol/ followed by the dataset, and then "+
				"zfs list -t snapshot -o name <dataset> to list the snapshots, which should all be __replicate_ "+
				"snapshots. Third, on %s, run pvesm free %s, which normally removes the volume with its snapshots. Only "+
				"if PVE refuses because snapshots remain, destroy only this volume's __replicate_ snapshots with zfs destroy <dataset>@<snapshot>, and run pvesm free "+
				"again. The procedure is in docs/troubleshooting.md#removing-the-volume-of-a-vm-that-no-longer-exists",
			volid, node, vmid, start, end, volumeName(volid), vmid, vmid, vmid, node, volid, node, volid)
	}
}

// volumeName returns the part of a volume ID after the storage ID.
func volumeName(volid string) string {
	if _, name, ok := strings.Cut(volid, ":"); ok {
		return name
	}
	return volid
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
			hg.Admissible = c.admissible(g, all)
			// A strict pin to the guest's own node is a hold: the guest waits for its
			// node and is never recovered elsewhere.
			hg.HA = len(hg.Admissible) != 1 || hg.Admissible[0] != g.ref.Node
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
	c.parkerLeftovers()
}

// parkerLeftovers reports this Director's parker resources and parker pins
// that name a VMID in the parker band no guest holds. The parker HA pass of a
// sweep removes such a leftover once it proves the parker gone, so one that
// the doctor sees has not been proven yet. The HA manager may still hold the
// service, the cluster may not be quorate or may report a member offline, a
// node the pin names may not answer, or no sweep has run since. The check
// matches the sweep's ownership rule, so it needs the run's Director UUID and
// reports only a marker that names that Director, and a pin only when it is a
// node-affinity rule that names the parker alone. It also needs the parked
// strategy, a guest list that every node answered, and the cluster's VM list,
// so a VMID missing from both proves the guest gone. Off mode has its own
// leftover findings.
func (c *haCollector) parkerLeftovers() {
	if c.off() || c.owner == "" || c.cfg == nil ||
		c.cfg.DetachedDiskStrategyValue() != config.DetachedDiskStrategyParked ||
		!c.guestsOK || c.guestsPartial || !c.clusterOK {
		return
	}
	ours := func(comment string) bool {
		owner, marked := haMarkerOwner(comment)
		return marked && owner == haOwnerValue(c.owner)
	}
	start, end := c.cfg.ParkedDiskVMIDRangeStartValue(), c.cfg.ParkedDiskVMIDRangeEndValue()
	gone := func(id string) (int, bool) {
		vmid, err := strconv.Atoi(id)
		if err != nil || strconv.Itoa(vmid) != id || vmid < start || vmid > end || c.parkerReportedEarlier(vmid) {
			return 0, false
		}
		_, listed := c.byVMID[vmid]
		_, indexed := c.clusterVMs[vmid]
		return vmid, !listed && !indexed
	}
	if c.resourcesOK {
		sids := make([]string, 0, len(c.resources))
		for sid := range c.resources {
			sids = append(sids, sid)
		}
		sort.Strings(sids)
		for _, sid := range sids {
			row := c.resources[sid]
			id, isVM := strings.CutPrefix(sid, "vm:")
			if !isVM || !ours(row.Comment) || haMarkerNamesGroup(row.Comment) {
				continue
			}
			if vmid, ok := gone(id); ok {
				c.rep.problem(haKindParkerResourceOrphan, sid,
					"the CPI-owned resource names parker VM %d, which no node holds a config for; the parker sweep "+
						"removes it once it proves the parker gone, and if it stays, remove it with ha-manager remove %s",
					vmid, sid)
			}
		}
	}
	if c.rulesOK {
		for i := range c.rules {
			r := &c.rules[i]
			id, isPin := strings.CutPrefix(r.ID, haPinRulePrefix)
			if !isPin || r.Type != ha.RuleTypeNodeAffinity || !ours(r.Comment) || !namesOnly(r, "vm:"+id) {
				continue
			}
			if vmid, ok := gone(id); ok {
				c.rep.problem(haKindParkerPinOrphan, r.ID,
					"the CPI-owned pin names parker VM %d, which no node holds a config for; the parker sweep "+
						"removes it once it proves the parker gone, and if it stays, remove it with ha-manager rules "+
						"remove %s", vmid, r.ID)
			}
		}
	}
}

// parkerReportedEarlier reports whether an earlier entry on the cluster, one
// of the same Director that the doctor read and that parks detached disks,
// holds vmid in its parker band, so that entry reports a leftover of it.
func (c *haCollector) parkerReportedEarlier(vmid int) bool {
	if c.scope == nil {
		return false
	}
	for i := range c.scope.Cluster {
		e := &c.scope.Cluster[i]
		if e.Self {
			return false
		}
		if c.sharesDirector(e) && e.Parkers.Contains(vmid) {
			return true
		}
	}
	return false
}

// namesOnly reports whether every resource rule names is sid.
func namesOnly(r *ha.Rule, sid string) bool {
	for _, res := range r.Resources {
		if res != sid {
			return false
		}
	}
	return true
}

// haMarkerNamesGroup reports whether a marker names a deployment or an
// instance group, which a VM's resource does and a parker's never does.
func haMarkerNamesGroup(comment string) bool {
	fields, _, ok := ha.MarkerFields(comment)
	if !ok {
		return false
	}
	for _, f := range fields {
		key, val, found := strings.Cut(f, "=")
		if found && (key == "deployment" || key == "group") && val != "" && val != ha.MarkerUnset {
			return true
		}
	}
	return false
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
		if c.foreign(row.Comment) || !c.reportsSID(sid) {
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
		if ruleMarked(&c.rules[i]) && !c.foreign(c.rules[i].Comment) && c.reportsRule(&c.rules[i]) {
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
		if !c.judgesVM(g) {
			continue
		}
		c.ownerTagMissing(g)
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

// ownerTagMissing advises on a VM the CPI created that carries no owner tag
// of any Director. Such a VM predates the owner tag, so the run cannot tell
// whose it is. The doctor still judges it, because it is far likelier to be this
// Director's than a stranger's, and it names the command that stamps the tag.
// A run that names no Director has nothing to stamp, so it says nothing.
func (c *haCollector) ownerTagMissing(g *haGuest) {
	if c.owner == "" || !c.judges(g) {
		return
	}
	for t := range g.tags {
		if strings.HasPrefix(strings.ToLower(t), strings.ToLower(handlers.OwnerTagPrefix)) {
			return
		}
	}
	c.rep.advise(haKindOwnerTagMissing, g.sid,
		"VM %d on %s carries bosh-cpi but no owner tag, so it predates the owner tag and the doctor cannot tell which Director owns it. "+
			"The doctor judges it as this Director's, and `cpi stemcell-refs adopt-owners` stamps the owner tag on the Director's older VMs",
		g.ref.VMID, g.ref.Node)
}

// judgesVM reports whether the local policy of this run applies to g. A VM
// whose owner tag names another Director belongs to that Director's policy,
// and under a cpi-config entry a VM outside the entry's VM ID band cannot have
// been created through it. A VM with no owner tag at all passes, because it
// predates the owner tag and is far likelier this Director's than a stranger's;
// ownerTagMissing advises on it and names adopt-owners.
func (c *haCollector) judgesVM(g *haGuest) bool {
	return ownerAdmits(c.owner, g) && c.judgesVMID(g.ref.VMID)
}

// ownerAdmits reports whether the Director owner may judge g: a run that
// names no Director judges every VM, and otherwise g must carry owner's tag or
// no owner tag at all. The guest's tags are lowercased when they are read, and
// OwnerTagFor lowercases the UUID, so the comparison ignores case.
func ownerAdmits(owner string, g *haGuest) bool {
	if owner == "" {
		return true
	}
	mine := handlers.OwnerTagFor(owner)
	other := false
	for t := range g.tags {
		if strings.HasPrefix(t, handlers.OwnerTagPrefix) {
			if t == mine {
				return true
			}
			other = true
		}
	}
	return !other
}

// judges reports whether this run judges the findings about g. It is a VM the
// CPI created, not a template, and one that judgesVM lets through.
func (c *haCollector) judges(g *haGuest) bool {
	return g.owned && !g.ref.Template && c.judgesVM(g)
}

// judgesVMID reports whether this run reports on vmid as a VM of its own. A
// run with no cpi-config entry judges every VM ID. Under an entry the VM ID
// must lie in the entry's band, and the entry must be the first on the cluster
// whose band holds it among the entries of the same Director that the doctor
// read, so a VM that two such entries' bands share is reported once.
func (c *haCollector) judgesVMID(vmid int) bool {
	self, held := c.firstHolder(vmid)
	return held && self
}

// reportsVMID reports whether this run reports a resource, a rule, or a
// planned move that names vmid. These belong to the Director whether or not an
// entry's band holds the VM ID, so the first entry whose band holds it reports
// them, and when no entry's band does, the entry that reports the Director's
// cluster findings does (see reportsCluster).
func (c *haCollector) reportsVMID(vmid int) bool {
	if self, held := c.firstHolder(vmid); held {
		return self
	}
	return c.reportsCluster()
}

// reportsSID is reportsVMID for an HA resource ID. A resource that names no VM
// ID is the Director's cluster finding.
func (c *haCollector) reportsSID(sid string) bool {
	if vmid, ok := sidVMID(sid); ok {
		return c.reportsVMID(vmid)
	}
	return c.reportsCluster()
}

// reportsRule is reportsVMID for a rule. A pin names its VM in its ID, and
// every other rule is the Director's cluster finding.
func (c *haCollector) reportsRule(r *ha.Rule) bool {
	if id, ok := strings.CutPrefix(r.ID, haPinRulePrefix); ok {
		if vmid, err := strconv.Atoi(id); err == nil && strconv.Itoa(vmid) == id {
			return c.reportsVMID(vmid)
		}
	}
	return c.reportsCluster()
}

// firstHolder finds the first entry on the cluster, in cpi-config order, whose
// band holds vmid and that is either this run's entry or an entry of the same
// Director that the doctor read. held is false when there is none, and self
// says whether it is this run's entry. A scope built without the cluster's
// entries stands for its own band alone.
func (c *haCollector) firstHolder(vmid int) (self, held bool) {
	if c.scope == nil {
		return true, true
	}
	if len(c.scope.Cluster) == 0 {
		in := c.scope.Band.Contains(vmid)
		return in, in
	}
	for i := range c.scope.Cluster {
		e := &c.scope.Cluster[i]
		if e.Band.Contains(vmid) && (e.Self || c.sharesDirector(e)) {
			return e.Self, true
		}
	}
	return false, false
}

// reportsCluster reports whether this run reports the findings that belong to
// its Director's part of the cluster as a whole, such as a group rule or the
// memory spread. Every entry of the same Director on the cluster reads the
// same rules and resources, so only the first that the doctor read reports
// them. A run with no cpi-config entry always does.
func (c *haCollector) reportsCluster() bool {
	if c.scope == nil {
		return true
	}
	for i := range c.scope.Cluster {
		e := &c.scope.Cluster[i]
		if e.Self {
			return true
		}
		if c.sharesDirector(e) {
			return false
		}
	}
	return true
}

// readsNotesFirst reports whether this run is the first entry of its Director
// on the cluster to have read the notes of node, so it gives the advice on
// them. An entry the doctor could not read, and an entry whose token could not
// read that node's notes, hand the advice to the next entry that did. The
// caller has already seen that this run read the notes.
func (c *haCollector) readsNotesFirst(node string) bool {
	if c.scope == nil {
		return true
	}
	for i := range c.scope.Cluster {
		e := &c.scope.Cluster[i]
		if e.Self {
			return true
		}
		if c.sharesDirector(e) && !slices.Contains(e.NotesUnread, node) {
			return false
		}
	}
	return true
}

// sharesDirector reports whether e is another entry the doctor read that it
// judges for this run's Director, so e reports what this run would report.
func (c *haCollector) sharesDirector(e *haClusterEntry) bool {
	return !e.Self && !e.Unread && strings.EqualFold(haOwnerValue(e.Owner), haOwnerValue(c.owner))
}

// sidVMID returns the VM ID an HA resource ID names.
func sidVMID(sid string) (int, bool) {
	id, ok := strings.CutPrefix(sid, "vm:")
	if !ok {
		return 0, false
	}
	vmid, err := strconv.Atoi(id)
	if err != nil || strconv.Itoa(vmid) != id {
		return 0, false
	}
	return vmid, true
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

// siblingCreation reports a missing directory beside the allocation journal,
// with what the CPI does about it. choice is the doctor's probe of the layers
// with no override.
func (c *haCollector) siblingCreation(choice endpointcache.Choice, j cacheJudge) {
	if choice.Layer == endpointcache.LayerJournal && createdOnFirstUse(choice) {
		journal := filepath.Clean(c.cfg.StorageAllocationJournalDir)
		reason, caveat := j.workerSide(filepath.Dir(choice.Path), journal, "the allocation journal directory")
		if reason != "" {
			c.rep.advise(haKindCacheDirectory, choice.Path,
				"%s does not exist, and the CPI does not create it because %s, so the cache and the HA recovery "+
					"episode record use %s instead; list the journal's parent directory in "+
					"director.cpi_additional_volumes with writable: true and mount_only: true, so the workers see "+
					"the journal and the cache on one mount%s", cacheDirectoryName(endpointcache.LayerJournal),
				reason, ha.FallbackDescription(endpointcache.LayerTemporary), j.judgedAsNote())
			return
		}
		c.rep.advise(haKindCacheDirectory, choice.Path,
			"%s does not exist, and the CPI creates it with mode 0700 on first use, because its parent "+
				"directory belongs to uid %d, which the CPI runs as, and sits on the same mount as the allocation "+
				"journal directory%s; cpi provision-journal also creates it%s",
			cacheDirectoryName(endpointcache.LayerJournal), j.uid, caveatClause(caveat), j.judgedAsNote())
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

// createdOnFirstUse reports whether choice is the probe's refusal of a missing
// directory that the CPI creates the first time it chooses it. The check must
// not hand a nil *Refusal to errors.Is, which would call its Unwrap method.
func createdOnFirstUse(choice endpointcache.Choice) bool {
	return choice.Err != nil && errors.Is(choice.Err, endpointcache.ErrCreatedOnFirstUse)
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
	// bpmStore, when set, replaces defaultBPMStore, so a test can lay out
	// BPM volumes under its own directory.
	bpmStore string
	// workers says where the Director runs the workers that run the CPI.
	workers workerLayout
}

// cacheJudge returns the judge for this run. The owner of the allocation
// journal directory is the CPI's user when the config names a journal that
// exists, and the doctor's own user stands in for it otherwise.
func (c *haCollector) cacheJudge() cacheJudge {
	j := cacheJudge{uid: c.euid, doctor: c.euid, workers: c.workerLayout()}
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

// overrideCreation says what the CPI does with a pve.endpoint_cache_dir that
// does not exist. creates is true when it creates the directory, and caveat is
// then what the finding adds about the workers' mounts. Otherwise reason says
// why it does not, or is empty when the check cannot tell, which is when no
// journal directory names the CPI's user and the doctor cannot see the CPI's
// temporary directory, whose mount decides it. A parent that the host puts on
// the journal's mount but that a BPM worker sees on another is a reason too,
// when the doctor knows the workers run under BPM. When it cannot tell, the
// caveat says what happens under BPM.
func (j cacheJudge) overrideCreation(dir endpointcache.DirOptions) (creates bool, reason, caveat string) {
	if dir.JournalDir == "" && !j.checksTemporary() {
		return false, "", ""
	}
	choice := dir.ProbeAs(j.uid)
	defer func() { _ = choice.Close() }()
	if createdOnFirstUse(choice) {
		anchor, what := "", "the CPI's temporary directory"
		if dir.JournalDir != "" {
			anchor, what = filepath.Clean(dir.JournalDir), "the allocation journal directory"
		}
		reason, caveat := j.workerSide(filepath.Dir(dir.Override), anchor, what)
		return reason == "", reason, caveat
	}
	var notCreated *endpointcache.NotCreatedError
	if choice.Err != nil && errors.As(choice.Err, &notCreated) {
		return false, notCreated.Reason.Error(), ""
	}
	return false, "", ""
}

// caveatClause joins a non-empty caveat onto a finding as a clause of its own.
func caveatClause(caveat string) string {
	if caveat == "" {
		return ""
	}
	return "; " + caveat
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
	creates, reason, caveat := j.overrideCreation(dir)
	if creates {
		anchor := "the allocation journal directory"
		if dir.JournalDir == "" {
			// The doctor judged against its own temporary directory, which
			// stands for the CPI's only when the two name the same place.
			anchor = fmt.Sprintf("this check's temporary directory, %s, which answers for the CPI only when "+
				"the CPI's TMPDIR names the same directory", os.TempDir())
		}
		c.rep.advise(haKindCacheDirectory, path,
			"pve.endpoint_cache_dir does not exist, and the CPI creates it with mode 0700 on first use, because "+
				"its parent directory belongs to uid %d, which the CPI runs as, and sits on the same mount as %s%s; "+
				"the HA recovery episode record then sits in it, so a grace window that is running restarts "+
				"once%s", j.uid, anchor, caveatClause(caveat), j.judgedAsNote())
		return
	}
	missing := "pve.endpoint_cache_dir does not exist"
	switch {
	case reason != "" && dir.JournalDir == "":
		// The doctor judged against its own temporary directory, whose owner
		// and mount stand for the CPI's only when the two name the same place.
		missing += fmt.Sprintf(", and the CPI does not create it because %s%s; this check used its own "+
			"temporary directory, %s, which answers for the CPI only when the CPI's TMPDIR names the same "+
			"directory,", reason, j.judgedAsNote(), os.TempDir())
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

// otherEntryAdvice is the advice on a group whose plan failed under one cpis
// entry's settings for a reason that only another entry's placement.az_map
// explains. who names the command that gives it, and reason is the plan's.
func otherEntryAdvice(who, reason string) string {
	return "the group's rules could not be planned under this entry's settings, but another cpi_config entry's " +
		"placement.az_map may own the group, and a run of one entry can't see the others, so " + who +
		" doesn't count it as a problem; run pve-ops ha --cpi-config to judge the group by the entry that " +
		"placed it. The plan found that " + reason
}
