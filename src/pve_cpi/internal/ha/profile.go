// Package ha resolves what the CPI may do with PVE HA on one cluster. Resolve
// probes the cluster, runs the quorum math, and returns a Profile that holds the
// effective mode, the reason for any downgrade, and the cluster facts the rest
// of the HA code reads. The probes are fail-closed: a source that cannot be read
// never counts as a prerequisite that is met.
package ha

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Effective is what the CPI may do with HA after the probes.
type Effective string

const (
	// EffectiveOff means pve.ha.mode is off. The CPI registers nothing and only
	// reads HA state and cleans up HA objects that already exist.
	EffectiveOff Effective = "off"

	// EffectiveObserveOnly means a prerequisite is missing. The CPI reads HA
	// state, filters unhealthy nodes, defers deletes, and cleans up existing HA
	// objects, and it writes no registrations, rules, or replication jobs. In
	// managed mode this is also the state of a failed call, and
	// Profile.PrerequisiteError carries the error.
	EffectiveObserveOnly Effective = "observe-only"

	// EffectiveFull means every cluster prerequisite is met and the CPI may
	// write HA state, subject to the decision for each VM.
	EffectiveFull Effective = "full"
)

// TopologyUnknown is the Profile.Topology when the storage definitions could
// not be read, or no storage can hold VM disks.
const TopologyUnknown = "unknown"

// Sources of Profile.Topology.
const (
	TopologyFromConfig  = "config"
	TopologyFromProbe   = "probe"
	TopologyFromNothing = "none"
)

// The PVE versions HA depends on. HA rules arrived in PVE 9.0, and the dynamic
// load balancer in 9.2.
const (
	minHAVersionMajor  = 9
	minHAVersionMinor  = 0
	minDLBVersionMajor = 9
	minDLBVersionMinor = 2
)

// NodeFacts holds what the profile knows about one cluster node.
type NodeFacts struct {
	Name   string
	Online bool

	// Votes is the node's configured quorum_votes, and VotesKnown is false when
	// they could not be read.
	Votes      int
	VotesKnown bool

	// Version is the pve-manager version, empty when unknown.
	Version    string
	VersionErr string

	// CPU is the node's CPU, and CPUErr says why it is unknown.
	CPU    CPUInfo
	CPUErr string
}

// Profile is the resolved HA state of one cluster.
type Profile struct {
	// Mode is the configured pve.ha.mode.
	Mode string

	// Effective is what the CPI may do. Reason is the first prerequisite that is
	// not met, or empty when Effective is full or off. Gaps lists every
	// prerequisite that is not met, in decision-table order, and is filled in
	// mode off too when the cluster was probed.
	Effective Effective
	Reason    string
	Gaps      []string

	// Advice is a recommendation that never changes Effective or any decision,
	// such as adding a QDevice. It is empty when there is none.
	Advice string

	// Topology is shared, local, mixed, or unknown. TopologySource says whether
	// the operator set it (config) or the storage definitions gave it (probe).
	Topology       string
	TopologySource string

	// EntryNode is the node that answered, and Clustered is false for a
	// standalone host. Quorate is the entry node's quorate flag.
	EntryNode string
	Clustered bool
	Quorate   bool

	// Nodes holds one entry per cluster member, sorted by name.
	Nodes []NodeFacts

	// MinVersion is the lowest version among the online nodes, empty when any
	// online node's version is unknown. HAVersionOK is true when every online
	// node is at the HA floor and the rules endpoint confirmed it. DLBSupported
	// is true when HAVersionOK holds and every online node is at 9.2 or later.
	MinVersion     string
	HAVersionOK    bool
	RulesSupported bool
	DLBSupported   bool

	// CRMMaster is the node that runs the CRM master, empty when none does. An
	// idle CRM's manager status still names the node that ran its last round,
	// and the profile leaves that node out, because no node runs the master.
	CRMMaster string

	// Health is the merged node health report, nil when it could not be built.
	// HealthErr says why.
	Health    *pve.NodeHealthReport
	HealthErr string

	// Quorum holds both quorum checks and QDevice holds the quorum device.
	Quorum  Quorum
	QDevice QDevice

	// SysConsole and VMReplicate are the token privileges. PermissionsErr says
	// why they could not be read.
	SysConsole     bool
	VMReplicate    bool
	PermissionsErr string

	// CRS is the cluster's resource scheduling setting.
	CRS CRS

	// Storages holds the storage definitions, StoragesErr says why they could
	// not be read.
	Storages    []pve.StorageInfo
	StoragesErr string

	// Replication holds the replication jobs, ReplicationErr says why they
	// could not be read.
	Replication    []ReplicationJob
	ReplicationErr string

	// partial is set when probe stopped before it read the cluster facts, so
	// the gates after it have nothing to test.
	partial bool

	// crmNote is advice recorded when the CRM is idle because no HA resource
	// exists. finish folds it into Advice, and String prints crm=idle.
	crmNote string

	// CPUDomains groups the nodes by CPU model and flags. UnknownCPUNodes lists
	// the nodes whose CPU is unknown, which belong to no domain.
	CPUDomains      []CPUDomain
	UnknownCPUNodes []string
}

// WritesAllowed reports whether the CPI may write HA state.
func (p *Profile) WritesAllowed() bool {
	return p != nil && p.Effective == EffectiveFull
}

// PrerequisiteError returns the error a managed-mode call fails with, or nil.
// It is non-nil only when the mode is managed and a prerequisite is missing,
// and it names every missing prerequisite. Auto mode never fails a call for a
// missing prerequisite.
func (p *Profile) PrerequisiteError() error {
	if p == nil || p.Mode != config.HAModeManaged || p.Effective == EffectiveFull {
		return nil
	}
	return cpierrors.Cloud("pve.ha.mode is managed and the cluster does not meet its prerequisites: %s",
		strings.Join(p.Gaps, "; "))
}

// HAStateReadable reports whether the HA manager status gave a trustworthy
// answer. An idle CRM on a cluster with no HA resource is one, because it holds
// no state to read. When it did not, placement in auto and managed mode falls
// back to the nodes that cluster status reports online.
func (p *Profile) HAStateReadable() bool {
	if p == nil || p.Health == nil {
		return false
	}
	switch p.Health.HA {
	case pve.HAViewManagerStatus, pve.HAViewNotConfigured, pve.HAViewIdle:
		return true
	default:
		return false
	}
}

// OnlineNodes returns the names of the nodes cluster status reports online.
func (p *Profile) OnlineNodes() []string {
	var out []string
	for i := range p.Nodes {
		if p.Nodes[i].Online {
			out = append(out, p.Nodes[i].Name)
		}
	}
	return out
}

// DomainOf returns the CPU migration domain that holds node, and false when the
// node's CPU is unknown.
func (p *Profile) DomainOf(node string) (CPUDomain, bool) {
	for _, d := range p.CPUDomains {
		if d.Contains(node) {
			return d, true
		}
	}
	return CPUDomain{}, false
}

// Resolve probes the cluster named by deps and returns its Profile, and it logs
// the profile line. Mode off returns at once without probing unless
// Deps.ProbeWhenOff is set. A probe that fails never returns an error: it is
// recorded in the profile and counts as a prerequisite that is not met. Resolve
// returns an error only for a caller mistake, such as a missing client. In
// managed mode the caller then calls PrerequisiteError.
func Resolve(ctx context.Context, deps Deps) (*Profile, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ha.Resolve: ctx must not be nil")
	}
	p := &Profile{Mode: deps.Config.HAModeValue(), Topology: TopologyUnknown, TopologySource: TopologyFromNothing}
	logger := deps.Log
	if logger == nil {
		logger = log.FromContext(ctx)
	}
	if p.Mode == config.HAModeOff && !deps.ProbeWhenOff {
		p.Effective = EffectiveOff
		logger.Debug(p.LogLine())
		return p, nil
	}
	if deps.Cluster == nil || deps.Nodes == nil || deps.Storage == nil {
		return nil, cpierrors.Cloud("ha.Resolve: cluster, nodes, and storage clients must not be nil")
	}

	p.probe(ctx, deps)
	p.finish(deps.Config)

	logger.Info(p.LogLine())
	if p.Mode != config.HAModeOff && p.Quorum.OneFailureFromLoss() {
		logger.Warn("ha: the cluster is one failure from losing quorum",
			log.String("nodes", strings.Join(p.Quorum.FragileNodes, ",")),
			log.Int("participating_votes", p.Quorum.Participating),
			log.Int("expected_votes", p.Quorum.Expected))
	}
	return p, nil
}

// probe runs every read and fills the cluster facts. It never decides.
func (p *Profile) probe(ctx context.Context, deps Deps) {
	cs, err := readClusterStatus(ctx, deps.Cluster)
	if err != nil {
		p.addGap("cluster status unreadable: " + err.Error())
		p.partial = true
		return
	}
	p.EntryNode, p.Clustered, p.Quorate = cs.Entry, cs.Clustered, cs.Quorate
	for _, n := range cs.Nodes {
		p.Nodes = append(p.Nodes, NodeFacts{Name: n.Name, Online: n.Online})
	}
	if !cs.Clustered {
		p.addGap("this host is not a cluster member, because cluster status has no cluster row")
		p.partial = true
		return
	}
	if !cs.Quorate {
		p.addGap("the entry node does not report the cluster quorate")
		p.partial = true
		return
	}

	report, herr := pve.NodeHealth(ctx, deps.Cluster, deps.Nodes, pve.NodeHealthOptions{MaintenanceTags: deps.MaintenanceTags})
	if herr != nil {
		p.HealthErr = herr.Error()
	}
	p.Health = report
	if report != nil && report.EntryNode != "" && p.EntryNode != "" && report.EntryNode != p.EntryNode {
		p.addGap(fmt.Sprintf("cluster status came from entry node %s but node health from %s", p.EntryNode, report.EntryNode))
	}
	master, merr := readCRMMaster(ctx, deps.Cluster)
	p.CRMMaster = master
	p.checkCRM(ctx, deps, merr)

	source := deps.Slow
	if source == nil {
		source = liveSlow{deps: deps}
	}
	slow, serr := source.Slow(ctx, cs.Name, cs.Nodes)
	if serr != nil || slow == nil {
		msg := "slow facts unavailable"
		if serr != nil {
			msg += ": " + serr.Error()
		}
		slow = &SlowFacts{
			RulesErr: msg, ConfigVotesErr: msg, PermissionsErr: msg, StoragesErr: msg,
			QDevice: QDevice{Status: QDeviceUnknown, ReadErr: msg},
		}
	}
	p.applySlow(ctx, slow, deps)

	p.CRS = readCRS(ctx, deps.Cluster)
	p.Replication, p.ReplicationErr = readReplication(ctx, deps.Cluster)
}

// checkCRM records a gap unless a fresh CRM master exists or the HA stack is
// idle. PVE's CRM stays idle and holds no manager lock while the cluster has
// no HA resource, so registering the first resource is what starts it, and a
// gap there would keep the profile from ever reaching full. A quorate cluster
// whose manager status names no master and whose resource list was read and
// is empty has never left that state. A CRM that went idle after the last
// resource was removed still names its old master, and node health reports it
// idle. A resource list that cannot be read, or that is not empty, keeps the
// gap so the check fails closed.
//
// An idle CRM writes nothing, so its manager status cannot tell an idle stack
// from a dead one. Either idle case therefore also needs pve-ha-crm to run on
// the entry node, because a profile that reached full on a dead stack would
// register resources that no CRM ever processes.
func (p *Profile) checkCRM(ctx context.Context, deps Deps, masterErr error) {
	note := ""
	switch {
	case masterErr != nil:
		p.addGap("HA manager status unreadable: " + masterErr.Error())
	case p.Health == nil:
		p.addGap("node health unreadable: " + p.HealthErr)
	case p.Health.HA == pve.HAViewIdle:
		p.CRMMaster = ""
		note = "HA stack idle, no resources configured"
	case p.CRMMaster == "" && p.crmIdle(ctx, deps.Cluster):
		note = "HA stack idle, no resources configured yet"
	case p.Health.HA != pve.HAViewManagerStatus:
		p.addGap(fmt.Sprintf("no CRM master with a fresh status (HA view %s: %s)", p.Health.HA, p.Health.HAReason))
	case p.CRMMaster == "":
		p.addGap("manager status names no CRM master")
	}
	if note == "" {
		return
	}
	state, err := readCRMServiceState(ctx, deps.Nodes, p.EntryNode)
	switch {
	case err != nil:
		p.addGap(fmt.Sprintf("the HA stack is idle, and the state of %s on %s is unreadable: %v",
			crmServiceName, p.EntryNode, err))
	case state != serviceRunning:
		p.addGap(fmt.Sprintf("the HA stack is idle, and %s on %s is not running (state %q)",
			crmServiceName, p.EntryNode, state))
	default:
		p.crmNote = note
	}
}

// crmIdle reports whether the cluster has no HA resources, read successfully.
func (p *Profile) crmIdle(ctx context.Context, api ClusterAPI) bool {
	var resp *sdkcluster.ListHaResourcesResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_resources", 0, func() error {
		var inner error
		resp, inner = api.ListHaResources(ctx, nil)
		return inner
	})
	return err == nil && resp != nil && len(*resp) == 0
}

// applySlow copies the slow facts into the profile and runs the quorum math.
func (p *Profile) applySlow(ctx context.Context, slow *SlowFacts, deps Deps) {
	qd := slow.QDevice
	if slow.Cached && qd.Configured {
		// A cache keeps the QDevice's configuration but never its connection
		// status, so the status is read again on every call.
		fresh := readQDevice(ctx, deps.Cluster)
		if fresh.Read && fresh.Configured {
			qd.Status = fresh.Status
		} else {
			qd = fresh
		}
	}
	p.QDevice = qd

	cpus := make(map[string]CPUInfo, len(p.Nodes))
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if sf, ok := slow.Nodes[n.Name]; ok {
			n.Version, n.VersionErr, n.CPU, n.CPUErr = sf.Version, sf.VersionErr, sf.CPU, sf.CPUErr
		} else if n.Online {
			n.VersionErr, n.CPUErr = "node was not probed", "node was not probed"
		}
		if v, ok := slow.ConfigVotes[n.Name]; ok {
			n.Votes, n.VotesKnown = v, true
		}
		cpus[n.Name] = n.CPU
	}
	p.CPUDomains, p.UnknownCPUNodes = groupCPUDomains(cpus)

	p.Quorum = p.computeQuorum(slow)
	p.RulesSupported = slow.RulesSupported
	p.summarizeVersions(slow)
	p.SysConsole, p.VMReplicate, p.PermissionsErr = slow.SysConsole, slow.VMReplicate, slow.PermissionsErr
	p.Storages, p.StoragesErr = slow.Storages, slow.StoragesErr
}

// computeQuorum builds the member list and runs both quorum checks. The votes
// must cover every node cluster status lists, and every node the cluster config
// lists must be in the list too, or the reading is unusable and both checks
// fail closed.
func (p *Profile) computeQuorum(slow *SlowFacts) Quorum {
	if slow.ConfigVotesErr != "" {
		return Quorum{UnknownReason: "configured votes unreadable: " + slow.ConfigVotesErr}
	}
	members := make([]Member, 0, len(p.Nodes))
	seen := make(map[string]bool, len(p.Nodes))
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if !n.VotesKnown {
			return Quorum{UnknownReason: fmt.Sprintf("node %s is not in the cluster config", n.Name)}
		}
		seen[n.Name] = true
		members = append(members, Member{Name: n.Name, Online: n.Online, Votes: n.Votes})
	}
	names := make([]string, 0, len(slow.ConfigVotes))
	for name := range slow.ConfigVotes {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		members = append(members, Member{Name: name, Online: false, Votes: slow.ConfigVotes[name]})
	}
	return computeQuorum(members, p.QDevice)
}

// summarizeVersions finds the lowest version among the online nodes and sets
// the version floor flags. A node that is offline was not asked and is skipped,
// while an online node whose version is unknown fails the floor closed.
func (p *Profile) summarizeVersions(slow *SlowFacts) {
	var lowest Version
	var lowestText string
	complete, found := true, false
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if !n.Online {
			continue
		}
		v, ok := parseVersion(n.Version)
		if !ok {
			complete = false
			continue
		}
		if !found || v.less(lowest) {
			lowest, lowestText = v, n.Version
		}
		found = true
	}
	if !complete || !found {
		return
	}
	p.MinVersion = lowestText
	p.HAVersionOK = lowest.AtLeast(minHAVersionMajor, minHAVersionMinor) && slow.RulesSupported
	p.DLBSupported = p.HAVersionOK && lowest.AtLeast(minDLBVersionMajor, minDLBVersionMinor)
}

// addGap records a prerequisite that is not met.
func (p *Profile) addGap(reason string) {
	p.Gaps = append(p.Gaps, reason)
}

// finish runs the remaining gates in decision-table order, resolves the
// topology and the advice, and sets the effective mode. It runs only after
// probe, and a probe that stopped early has already recorded its gap.
func (p *Profile) finish(cfg *config.CPIConfig) {
	if !p.partial {
		p.gateSurvival()
		p.gateVersion()
		p.gatePrivileges()
		p.gateOnlineNodes()
	}
	p.Topology, p.TopologySource = resolveTopology(cfg.HAStorageTopologyValue(), p.Storages, p.StoragesErr)
	p.Advice = qdeviceAdvice(p.Quorum, p.QDevice)
	if p.crmNote != "" {
		if p.Advice != "" {
			p.Advice += "; "
		}
		p.Advice += p.crmNote
	}

	switch {
	case p.Mode == config.HAModeOff:
		p.Effective = EffectiveOff
	case len(p.Gaps) == 0:
		p.Effective = EffectiveFull
	default:
		p.Effective = EffectiveObserveOnly
		p.Reason = p.Gaps[0]
	}
}

func (p *Profile) gateSurvival() {
	q := p.Quorum
	switch {
	case !q.Known:
		p.addGap("cannot prove the configured votes survive the loss of one node: " + q.UnknownReason)
	case !q.SurvivesOne:
		p.addGap(fmt.Sprintf("%d configured votes do not survive the loss of one node "+
			"(%d expected votes, %d remain if %s fails, and quorum needs more than %d; QDevice %s)",
			q.Expected, q.Expected, q.RemainingAfterLoss, q.HeaviestNode, q.Expected/2, p.qdeviceWord()))
	}
}

func (p *Profile) gateVersion() {
	if p.HAVersionOK {
		return
	}
	var unreadable []string
	for i := range p.Nodes {
		if p.Nodes[i].Online && p.Nodes[i].Version == "" {
			unreadable = append(unreadable, p.Nodes[i].Name)
		}
	}
	switch {
	case len(unreadable) > 0:
		p.addGap("PVE version unreadable on " + strings.Join(unreadable, ", "))
	case p.MinVersion == "":
		p.addGap("PVE version unreadable, because no node is online")
	case !p.RulesSupported:
		p.addGap("the HA rules endpoint did not answer, so the PVE 9 floor is unconfirmed")
	default:
		p.addGap(fmt.Sprintf("PVE %s is below the PVE %d floor for HA writes", p.MinVersion, minHAVersionMajor))
	}
}

func (p *Profile) gatePrivileges() {
	if p.SysConsole {
		return
	}
	if p.PermissionsErr != "" {
		p.addGap("missing Sys.Console on / (token permissions unreadable: " + p.PermissionsErr + ")")
		return
	}
	p.addGap("missing Sys.Console on /")
}

func (p *Profile) gateOnlineNodes() {
	online := len(p.OnlineNodes())
	if online < 2 {
		p.addGap(fmt.Sprintf("fewer than two nodes are online (%d of %d)", online, len(p.Nodes)))
	}
}

// qdeviceWord names the QDevice's state for the log line.
func (p *Profile) qdeviceWord() string {
	switch {
	case !p.QDevice.Read:
		return string(QDeviceUnknown)
	case !p.QDevice.Configured:
		return "no"
	}
	return string(p.QDevice.Status)
}

// resolveTopology returns the storage topology. A configured value other than
// auto wins. Under auto, the storages that can hold VM disks decide: all shared
// is shared, none shared is local, and anything else is mixed.
func resolveTopology(configured string, storages []pve.StorageInfo, storagesErr string) (string, string) {
	if configured != config.HAStorageTopologyAuto {
		return configured, TopologyFromConfig
	}
	if storagesErr != "" {
		return TopologyUnknown, TopologyFromNothing
	}
	shared, local := 0, 0
	for i := range storages {
		s := &storages[i]
		if s.Disabled || !holdsVMDisks(s.Content) {
			continue
		}
		if s.IsShared() {
			shared++
		} else {
			local++
		}
	}
	switch {
	case shared == 0 && local == 0:
		return TopologyUnknown, TopologyFromNothing
	case local == 0:
		return config.HAStorageTopologyShared, TopologyFromProbe
	case shared == 0:
		return config.HAStorageTopologyLocal, TopologyFromProbe
	}
	return config.HAStorageTopologyMixed, TopologyFromProbe
}

// holdsVMDisks reports whether a storage content list names VM disk images or
// container volumes. An empty list counts, because PVE then uses the plugin
// default, which for a disk storage includes both.
func holdsVMDisks(content string) bool {
	if strings.TrimSpace(content) == "" {
		return true
	}
	for _, c := range strings.Split(content, ",") {
		switch strings.TrimSpace(c) {
		case "images", "rootdir":
			return true
		}
	}
	return false
}

// LogLine renders the profile as the single startup line. Headroom is not part
// of the profile, so a caller that computes it appends its own field.
func (p *Profile) LogLine() string {
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ha: mode=%s effective=%s", p.Mode, p.Effective)
	if p.Reason != "" {
		fmt.Fprintf(&b, " reason=%s", p.Reason)
	}
	if p.Effective == EffectiveOff && len(p.Nodes) == 0 {
		return b.String()
	}
	version := p.MinVersion
	if version == "" {
		version = "unknown"
	}
	crm := "none"
	switch {
	case p.CRMMaster != "":
		crm = "master@" + p.CRMMaster
	case p.crmNote != "":
		crm = "idle"
	}
	votes, survives := "unknown", "unknown"
	if p.Quorum.Known {
		votes = fmt.Sprintf("%d/%d", p.Quorum.Participating, p.Quorum.Expected)
		survives = yes(p.Quorum.SurvivesOne)
	}
	online := len(p.OnlineNodes())
	crs := "unknown"
	if p.CRS.Known {
		crs = p.CRS.Mode
	}
	fmt.Fprintf(&b, " topology=%s pve=%s rules=%s crm=%s quorum=%s votes=%s survives_one=%s qdevice=%s nodes=%d/%d"+
		" sys_console=%s crs=%s rebalance_on_start=%s",
		p.Topology, version, yes(p.RulesSupported), crm, yes(p.Quorate && p.Clustered), votes, survives,
		p.qdeviceWord(), online, len(p.Nodes), yes(p.SysConsole), crs, yes(p.CRS.RebalanceOnStart))
	if p.Advice != "" {
		fmt.Fprintf(&b, " advice=%s", p.Advice)
	}
	return b.String()
}
