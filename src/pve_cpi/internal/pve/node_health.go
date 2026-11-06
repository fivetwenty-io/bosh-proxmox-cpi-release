package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

const (
	// NodeHealthCRMMaxAge is the oldest CRM timestamp, measured on the entry
	// node's clock, that NodeHealth still treats as a live view of the cluster.
	NodeHealthCRMMaxAge = 60 * time.Second

	// NodeHealthLRMMaxAge is the oldest LRM timestamp, measured on the entry
	// node's clock, that counts as a live answer for that node. It is the limit
	// PVE uses for its own status display.
	NodeHealthLRMMaxAge = 30 * time.Second

	// NodeHealthMaxFutureSkew is how far ahead of the entry node's clock a CRM
	// or LRM timestamp may lie before NodeHealth calls it stale. A few seconds
	// of drift between cluster members is normal; more than that means the
	// timestamp cannot be trusted.
	NodeHealthMaxFutureSkew = 5 * time.Second
)

// Node states in the CRM's node_status map.
const (
	crmNodeOnline      = "online"
	crmNodeMaintenance = "maintenance"
	crmNodeUnknown     = "unknown"
	crmNodeFence       = "fence"
	crmNodeGone        = "gone"
)

// Node states and modes in the LRM's lrm_status map.
const (
	lrmStateActive      = "active"
	lrmStateWaitForLock = "wait_for_agent_lock"
	lrmStateMaintenance = "maintenance"
	lrmModeMaintenance  = "maintenance"
	lrmModeRestart      = "restart"
	lrmModeShutdown     = "shutdown"
)

// Row types in GET /cluster/status. A clustered host reports a cluster row (some
// releases name it quorum) that carries the quorate flag; a standalone host
// reports only its own node row.
const (
	clusterRowTypeQuorum  = "quorum"
	clusterRowTypeCluster = "cluster"
)

// NodeHealthState is the helper's overall reading of one node.
type NodeHealthState string

const (
	// NodeHealthOnline means the node is online, with quorum, and no source
	// reports a problem with it. It can still be unplaceable, for example while
	// its LRM is restarting; read NodeHealthEntry.Placeable for that answer.
	NodeHealthOnline NodeHealthState = "online"

	// NodeHealthMaintenance means PVE or an operator tag reports the node in
	// maintenance. It stays a proxy target, but it receives nothing new.
	NodeHealthMaintenance NodeHealthState = "maintenance"

	// NodeHealthDead means the node is offline, fenced, gone, or reported
	// unknown by a fresh CRM view. Calls about VMs on it must wait or defer.
	NodeHealthDead NodeHealthState = "dead"

	// NodeHealthUnknown means the sources cannot say, for example because the
	// entry node is not quorate or the node's LRM timestamp is stale.
	NodeHealthUnknown NodeHealthState = "unknown"
)

// WatchdogState is what the structured LRM entry says about a node's watchdog.
type WatchdogState string

const (
	// WatchdogArmed means the LRM entry is fresh and in state active or
	// maintenance, so the node's watchdog is running.
	WatchdogArmed WatchdogState = "armed"

	// WatchdogIdle means the LRM entry is fresh and waiting for the agent lock,
	// so the node has no active service and its watchdog is not running.
	WatchdogIdle WatchdogState = "idle"

	// WatchdogUnknown covers every other answer, including a missing or stale
	// LRM entry and every view that is not a fresh manager status.
	WatchdogUnknown WatchdogState = "unknown"
)

// HAView names which HA source NodeHealth trusted for a report.
type HAView string

const (
	// HAViewManagerStatus means a quorate entry node returned a manager status
	// whose CRM timestamp is fresh on the entry node's clock.
	HAViewManagerStatus HAView = "manager_status"

	// HAViewLRMText means manager status was empty or unreadable, so only the
	// text of the lrm entries in status/current informed the report. It can
	// detect maintenance and silent LRMs, and it never answers the watchdog.
	HAViewLRMText HAView = "lrm_text"

	// HAViewNotConfigured means every HA source answered and none holds a CRM or
	// LRM view, so the report rests on cluster status alone.
	HAViewNotConfigured HAView = "not_configured"

	// HAViewStale means the CRM timestamp is older than NodeHealthCRMMaxAge or
	// lies in the future, so no HA source was trusted.
	HAViewStale HAView = "stale"

	// HAViewUnreadable means an HA source failed to answer, or the entry node's
	// clock could not be read, so no HA source was trusted.
	HAViewUnreadable HAView = "unreadable"

	// HAViewNotQuorate means the entry node is not quorate. No HA source was
	// read, and every node is reported as unknown.
	HAViewNotQuorate HAView = "not_quorate"
)

// NodeHealthCluster is the subset of the PVE cluster API that NodeHealth reads.
// It is satisfied by the sdk cluster service.
type NodeHealthCluster interface {
	ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error)
	ListHaStatusManagerStatus(ctx context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error)
	ListHaStatusCurrent(ctx context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error)
}

// NodeHealthNodes is the subset of the PVE nodes API that NodeHealth reads. It
// is satisfied by the sdk nodes service.
type NodeHealthNodes interface {
	ListTime(ctx context.Context, node string) (*sdknodes.ListTimeResponse, error)
}

// NodeHealthOptions holds the optional inputs of NodeHealth.
type NodeHealthOptions struct {
	// MaintenanceTags lists node tags that the operator applies to mark a node
	// in maintenance. A node that carries any of them is in maintenance whatever
	// PVE reports, and the result is the union of both sources.
	MaintenanceTags []string
}

// NodeHealthEntry is the helper's answer for one node.
type NodeHealthEntry struct {
	// Name is the node name.
	Name string

	// Health is the overall reading of the node.
	Health NodeHealthState

	// Maintenance is true when PVE or an operator tag reports the node in
	// maintenance. A dead node can also carry it.
	Maintenance bool

	// Placeable is true only for a node that is online, has quorum, is not in
	// maintenance, is not fenced, and, when a fresh manager status exists, has a
	// fresh LRM entry whose mode allows new work. Only placeable nodes can
	// receive new VMs or migrations.
	Placeable bool

	// ProxyTarget is true when PVE's cluster view says the node is online, not
	// fenced, and not gone, so a call addressed to it by name can be answered.
	// A node in maintenance still qualifies. This is a statement about PVE's
	// view of the node, never a probe of an API endpoint.
	ProxyTarget bool

	// DeadOrFenced is true when the node is offline, unknown, fenced, or gone.
	// Calls about VMs on such a node must wait or defer rather than act.
	DeadOrFenced bool

	// Watchdog is the node's watchdog state from its structured LRM entry.
	Watchdog WatchdogState

	// CRMState is the node's raw state in the CRM's node_status map, or empty
	// when no fresh manager status was trusted or the map has no entry for it.
	CRMState string

	// Reason is a short explanation of the answer, for logs and operator output.
	Reason string
}

// NodeHealthReport is the merged health reading of every cluster member.
type NodeHealthReport struct {
	// EntryNode is the name of the node that answered, from the local flag of
	// cluster status. It is empty when no source named it.
	EntryNode string

	// Quorate is whether the entry node reports the cluster quorate. It is
	// false only when the cluster row of cluster status says so.
	Quorate bool

	// QuorumReported is false when cluster status carried no cluster row, as on a
	// standalone host. Quorate is then true by default rather than by report, so
	// a caller that must have the flag before it acts should check this first.
	QuorumReported bool

	// HA names which HA source the report trusted, and HAReason says why when
	// that source is not the structured manager status.
	HA       HAView
	HAReason string

	// Nodes holds one entry per node, sorted by name.
	Nodes []NodeHealthEntry

	index map[string]int
}

// Node returns the entry for name, and false when no source mentions the node.
func (r *NodeHealthReport) Node(name string) (NodeHealthEntry, bool) {
	if r == nil {
		return NodeHealthEntry{}, false
	}
	i, ok := r.index[name]
	if !ok {
		return NodeHealthEntry{}, false
	}
	return r.Nodes[i], true
}

// Placeable reports whether name can receive new VMs or migrations. A node the
// report does not mention is not placeable.
func (r *NodeHealthReport) Placeable(name string) bool {
	e, ok := r.Node(name)
	return ok && e.Placeable
}

// ProxyTarget reports whether a call addressed to name can be answered. A node
// the report does not mention is not a proxy target.
func (r *NodeHealthReport) ProxyTarget(name string) bool {
	e, ok := r.Node(name)
	return ok && e.ProxyTarget
}

// DeadOrFenced reports whether name is offline, unknown, fenced, or gone. A node
// the report does not mention is not reported dead, because nothing says it is.
func (r *NodeHealthReport) DeadOrFenced(name string) bool {
	e, ok := r.Node(name)
	return ok && e.DeadOrFenced
}

// WatchdogOf returns the watchdog state of name, or WatchdogUnknown when the
// report does not mention the node.
func (r *NodeHealthReport) WatchdogOf(name string) WatchdogState {
	e, ok := r.Node(name)
	if !ok {
		return WatchdogUnknown
	}
	return e.Watchdog
}

// clusterNodeRow is one node row of GET /cluster/status.
type clusterNodeRow struct {
	Name   string
	Online bool
	Local  bool
	Tags   string
}

// clusterView is what NodeHealth keeps from GET /cluster/status.
type clusterView struct {
	nodes     map[string]clusterNodeRow
	entry     string
	clustered bool // a cluster or quorum row exists
	quorate   bool // the quorate flag of that row
}

// managerView is what NodeHealth keeps from GET /cluster/ha/status/manager_status.
type managerView struct {
	nodeStatus map[string]string
	lrm        map[string]lrmWire
	crmStamp   int64
	quorumNode string
}

// lrmWire is the wire shape of one lrm_status entry.
type lrmWire struct {
	Mode      string           `json:"mode"`
	State     string           `json:"state"`
	Timestamp sdkclient.PVEInt `json:"timestamp"`
}

// empty reports whether the response carries no CRM or LRM view at all.
func (m *managerView) empty() bool {
	return len(m.nodeStatus) == 0 && len(m.lrm) == 0 && m.crmStamp == 0
}

// lrmTextState is what the free-text status of an lrm entry says about a node.
type lrmTextState int

const (
	lrmTextOK lrmTextState = iota
	lrmTextMaintenance
	lrmTextSilent
)

// NodeHealth merges every health source into one report. It reads cluster
// status, the structured manager status with its node_status and lrm_status
// maps, the entry node's clock, and, only when manager status is empty or
// unreadable, the text of the lrm entries in status/current. It then applies
// the operator's maintenance tags as an extra input.
//
// Only a failed cluster status read returns an error, because without the
// membership list nothing can be classified. Every other failed source degrades
// the report: HA names the view that was trusted and HAReason says why. When
// the entry node is not quorate, no HA source is read and every node is
// reported unknown. The helper never probes or demotes an API endpoint.
func NodeHealth(
	ctx context.Context,
	clusterAPI NodeHealthCluster,
	nodesAPI NodeHealthNodes,
	opts NodeHealthOptions,
) (*NodeHealthReport, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("NodeHealth: ctx must not be nil")
	}
	if clusterAPI == nil {
		return nil, cpierrors.Cloud("NodeHealth: cluster client must not be nil")
	}
	if nodesAPI == nil {
		return nil, cpierrors.Cloud("NodeHealth: nodes client must not be nil")
	}

	view, err := readClusterView(ctx, clusterAPI)
	if err != nil {
		return nil, err
	}

	report := &NodeHealthReport{EntryNode: view.entry, Quorate: view.quorate, QuorumReported: view.clustered}
	if !view.quorate {
		report.HA = HAViewNotQuorate
		report.HAReason = "entry node does not report the cluster quorate"
		report.Nodes = unknownEntries(view, opts, report.HAReason)
		report.reindex()
		return report, nil
	}

	mgr, mgrErr := readManagerView(ctx, clusterAPI)
	if report.EntryNode == "" && mgr != nil {
		report.EntryNode = mgr.quorumNode
	}

	var ha haInputs
	switch {
	case mgr != nil && !mgr.empty():
		ha = resolveManagerView(ctx, nodesAPI, report, mgr)
	default:
		reason := "manager status is empty"
		if mgrErr != nil {
			reason = "manager status unreadable: " + mgrErr.Error()
		}
		ha = resolveTextFallback(ctx, clusterAPI, report, reason, mgrErr != nil)
	}

	report.Nodes = classifyNodes(view, report.HA, ha, opts)
	report.reindex()
	return report, nil
}

// haInputs carries the trusted HA data into classification.
type haInputs struct {
	nodeStatus map[string]string
	lrm        map[string]lrmWire
	entryTime  int64 // entry node's clock in Unix seconds
	text       map[string]lrmTextState
}

func (r *NodeHealthReport) reindex() {
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Name < r.Nodes[j].Name })
	r.index = make(map[string]int, len(r.Nodes))
	for i, e := range r.Nodes {
		r.index[e.Name] = i
	}
}

// readClusterView reads GET /cluster/status once and extracts membership, the
// entry node, and the quorate flag as the entry node sees it.
func readClusterView(ctx context.Context, clusterAPI NodeHealthCluster) (*clusterView, error) {
	var resp *sdkcluster.ListStatusResponse
	err := RetryOnTransient(ctx, nil, "node_health_cluster_status", 0, func() error {
		var inner error
		resp, inner = clusterAPI.ListStatus(ctx)
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "node health: list cluster status")
	}
	if resp == nil {
		return nil, cpierrors.Cloud("node health: nil response from cluster status")
	}

	type statusRow struct {
		Type    string            `json:"type"`
		Name    string            `json:"name"`
		Online  sdkclient.PVEBool `json:"online"`
		Local   sdkclient.PVEBool `json:"local"`
		Quorate sdkclient.PVEBool `json:"quorate"`
		Tags    string            `json:"tags"`
	}

	view := &clusterView{nodes: make(map[string]clusterNodeRow)}
	for _, raw := range *resp {
		var row statusRow
		if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
			// A single bad row must not hide the rest of the membership.
			continue
		}
		switch row.Type {
		case resourceTypeNode:
			if row.Name == "" {
				continue
			}
			view.nodes[row.Name] = clusterNodeRow{
				Name: row.Name, Online: row.Online.Bool(), Local: row.Local.Bool(), Tags: row.Tags,
			}
			if row.Local.Bool() {
				view.entry = row.Name
			}
		case clusterRowTypeCluster, clusterRowTypeQuorum:
			view.clustered = true
			view.quorate = row.Quorate.Bool()
		}
	}
	if !view.clustered {
		// A standalone host reports no cluster row, so a single node row with no
		// cluster row counts as quorate; QuorumReported stays false for callers
		// that must have the flag before they act. Two or more node rows without
		// a cluster row is a response we cannot read quorum from, so the view
		// fails closed as non-quorate.
		view.quorate = len(view.nodes) <= 1
	}
	return view, nil
}

// readManagerView reads GET /cluster/ha/status/manager_status. The response is
// an object that holds the CRM's state under manager_status, the LRM map under
// lrm_status, and the entry node under quorum; the parser also accepts the CRM
// fields at the top level. A body that does not parse is an error; a missing or
// null body is an empty view.
func readManagerView(ctx context.Context, clusterAPI NodeHealthCluster) (*managerView, error) {
	var resp *sdkcluster.ListHaStatusManagerStatusResponse
	err := RetryOnTransient(ctx, nil, "node_health_manager_status", 0, func() error {
		var inner error
		resp, inner = clusterAPI.ListHaStatusManagerStatus(ctx)
		return inner
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(*resp) == 0 || string(*resp) == "null" {
		return &managerView{}, nil
	}

	var top struct {
		ManagerStatus json.RawMessage `json:"manager_status"`
		LRMStatus     json.RawMessage `json:"lrm_status"`
		NodeStatus    json.RawMessage `json:"node_status"`
		Timestamp     json.RawMessage `json:"timestamp"`
		Quorum        struct {
			Node string `json:"node"`
		} `json:"quorum"`
	}
	if jsonErr := json.Unmarshal(*resp, &top); jsonErr != nil {
		return nil, jsonErr
	}

	crm := struct {
		NodeStatus json.RawMessage `json:"node_status"`
		LRMStatus  json.RawMessage `json:"lrm_status"`
		Timestamp  json.RawMessage `json:"timestamp"`
	}{NodeStatus: top.NodeStatus, LRMStatus: top.LRMStatus, Timestamp: top.Timestamp}
	if isJSONObject(top.ManagerStatus) {
		var nested struct {
			NodeStatus json.RawMessage `json:"node_status"`
			LRMStatus  json.RawMessage `json:"lrm_status"`
			Timestamp  json.RawMessage `json:"timestamp"`
		}
		if jsonErr := json.Unmarshal(top.ManagerStatus, &nested); jsonErr != nil {
			return nil, jsonErr
		}
		if len(nested.NodeStatus) > 0 {
			crm.NodeStatus = nested.NodeStatus
		}
		if len(nested.LRMStatus) > 0 && len(crm.LRMStatus) == 0 {
			crm.LRMStatus = nested.LRMStatus
		}
		if len(nested.Timestamp) > 0 {
			crm.Timestamp = nested.Timestamp
		}
	}

	view := &managerView{quorumNode: top.Quorum.Node}
	if jsonErr := decodeHAMap(crm.NodeStatus, &view.nodeStatus); jsonErr != nil {
		return nil, fmt.Errorf("node_status: %w", jsonErr)
	}
	if jsonErr := decodeHAMap(crm.LRMStatus, &view.lrm); jsonErr != nil {
		return nil, fmt.Errorf("lrm_status: %w", jsonErr)
	}
	if len(crm.Timestamp) > 0 && string(crm.Timestamp) != "null" {
		var ts sdkclient.PVEInt
		if jsonErr := json.Unmarshal(crm.Timestamp, &ts); jsonErr != nil {
			return nil, fmt.Errorf("timestamp: %w", jsonErr)
		}
		view.crmStamp = ts.Int()
	}
	return view, nil
}

// decodeHAMap decodes an optional JSON object into dst. A missing or null field
// leaves dst empty; any other shape is an error, because a manager status that
// cannot be read must not pass for one that is empty.
func decodeHAMap(raw json.RawMessage, dst any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		return fmt.Errorf("want an object, got %.20s", trimmed)
	}
	return json.Unmarshal(raw, dst)
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

// resolveManagerView decides whether a non-empty manager status is trustworthy
// and records the verdict on the report. It returns the inputs to classify
// with, which are empty when the view is not trusted.
func resolveManagerView(
	ctx context.Context,
	nodesAPI NodeHealthNodes,
	report *NodeHealthReport,
	mgr *managerView,
) haInputs {
	if report.EntryNode == "" {
		report.HA = HAViewUnreadable
		report.HAReason = "entry node unknown, so timestamps cannot be measured"
		return haInputs{}
	}

	var timeResp *sdknodes.ListTimeResponse
	err := RetryOnTransient(ctx, nil, "node_health_entry_time", 0, func() error {
		var inner error
		timeResp, inner = nodesAPI.ListTime(ctx, report.EntryNode)
		return inner
	})
	if err != nil || timeResp == nil || timeResp.Time.Int() <= 0 {
		report.HA = HAViewUnreadable
		report.HAReason = "entry node clock unreadable, so timestamps cannot be measured"
		if err != nil {
			report.HAReason += ": " + err.Error()
		}
		return haInputs{}
	}
	now := timeResp.Time.Int()

	if mgr.crmStamp <= 0 {
		report.HA = HAViewStale
		report.HAReason = "manager status carries no CRM timestamp"
		return haInputs{}
	}
	if !timestampFresh(now, mgr.crmStamp, NodeHealthCRMMaxAge) {
		report.HA = HAViewStale
		report.HAReason = "CRM timestamp is " + describeAge(now, mgr.crmStamp) + " on the entry node's clock"
		return haInputs{}
	}

	report.HA = HAViewManagerStatus
	return haInputs{nodeStatus: mgr.nodeStatus, lrm: mgr.lrm, entryTime: now}
}

// timestampFresh reports whether a timestamp is at most maxAge behind now and
// no more than NodeHealthMaxFutureSkew ahead of it.
func timestampFresh(now, stamp int64, maxAge time.Duration) bool {
	age := now - stamp
	return age <= int64(maxAge/time.Second) && age >= -int64(NodeHealthMaxFutureSkew/time.Second)
}

func describeAge(now, stamp int64) string {
	age := now - stamp
	if age < 0 {
		return (time.Duration(-age) * time.Second).String() + " in the future"
	}
	return (time.Duration(age) * time.Second).String() + " old"
}

// resolveTextFallback reads the lrm entries of status/current when manager
// status gave nothing usable. managerFailed says that manager status errored,
// as opposed to answering with an empty body.
func resolveTextFallback(
	ctx context.Context,
	clusterAPI NodeHealthCluster,
	report *NodeHealthReport,
	reason string,
	managerFailed bool,
) haInputs {
	var resp *sdkcluster.ListHaStatusCurrentResponse
	err := RetryOnTransient(ctx, nil, "node_health_ha_status_current", 0, func() error {
		var inner error
		resp, inner = clusterAPI.ListHaStatusCurrent(ctx)
		return inner
	})
	if err != nil {
		report.HA = HAViewUnreadable
		report.HAReason = reason + "; status/current unreadable: " + err.Error()
		if !managerFailed {
			report.HAReason = "manager status is empty; status/current unreadable: " + err.Error()
		}
		return haInputs{}
	}

	text := make(map[string]lrmTextState)
	if resp != nil {
		for _, raw := range *resp {
			var entry struct {
				Type   string `json:"type"`
				Node   string `json:"node"`
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil || entry.Type != "lrm" {
				continue
			}
			node := entry.Node
			if node == "" {
				node = strings.TrimPrefix(entry.ID, "lrm:")
			}
			if node == "" {
				continue
			}
			text[node] = parseLRMText(entry.Status)
		}
	}

	if len(text) == 0 {
		if managerFailed {
			report.HA = HAViewUnreadable
			report.HAReason = reason + "; status/current has no lrm entries"
		} else {
			report.HA = HAViewNotConfigured
			report.HAReason = "manager status and status/current hold no HA view"
		}
		return haInputs{}
	}

	report.HA = HAViewLRMText
	report.HAReason = reason + "; using the text of the lrm entries"
	return haInputs{text: text}
}

// parseLRMText reads the parenthesized state in an lrm entry's status text,
// such as "pve1 (active, Wed Nov  6 12:00:00 2026)". It is only a fallback and
// never decides anything about a watchdog.
func parseLRMText(status string) lrmTextState {
	lower := strings.ToLower(status)
	open := strings.Index(lower, "(")
	if open < 0 {
		return lrmTextOK
	}
	inner := lower[open+1:]
	if end := strings.IndexAny(inner, ",)"); end >= 0 {
		inner = inner[:end]
	}
	switch {
	case strings.Contains(inner, "maintenance"):
		return lrmTextMaintenance
	case strings.Contains(inner, "old timestamp"), strings.Contains(inner, "dead"),
		strings.Contains(inner, "unable"):
		return lrmTextSilent
	default:
		return lrmTextOK
	}
}

// tagMatch reports whether the node's tag list holds any operator tag.
func tagMatch(nodeTags string, want []string) bool {
	if nodeTags == "" || len(want) == 0 {
		return false
	}
	for _, tag := range strings.FieldsFunc(nodeTags, func(r rune) bool {
		return r == ';' || r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		for _, w := range want {
			if w != "" && tag == w {
				return true
			}
		}
	}
	return false
}

// unknownEntries answers "unknown" for every member when the entry node is not
// quorate. Operator maintenance tags still show, since they need no cluster.
func unknownEntries(view *clusterView, opts NodeHealthOptions, reason string) []NodeHealthEntry {
	entries := make([]NodeHealthEntry, 0, len(view.nodes))
	for name, row := range view.nodes {
		entries = append(entries, NodeHealthEntry{
			Name:        name,
			Health:      NodeHealthUnknown,
			Maintenance: tagMatch(row.Tags, opts.MaintenanceTags),
			Watchdog:    WatchdogUnknown,
			Reason:      reason,
		})
	}
	return entries
}

// classifyNodes builds one entry for every node any trusted source names.
func classifyNodes(view *clusterView, haView HAView, ha haInputs, opts NodeHealthOptions) []NodeHealthEntry {
	names := make(map[string]struct{}, len(view.nodes))
	for name := range view.nodes {
		names[name] = struct{}{}
	}
	if haView == HAViewManagerStatus {
		for name := range ha.nodeStatus {
			names[name] = struct{}{}
		}
	}

	entries := make([]NodeHealthEntry, 0, len(names))
	for name := range names {
		entries = append(entries, classifyNode(name, view, haView, ha, opts))
	}
	return entries
}

// classifyNode merges the sources for one node.
func classifyNode(
	name string,
	view *clusterView,
	haView HAView,
	ha haInputs,
	opts NodeHealthOptions,
) NodeHealthEntry {
	row, inCluster := view.nodes[name]
	e := NodeHealthEntry{
		Name:        name,
		Watchdog:    WatchdogUnknown,
		Maintenance: inCluster && tagMatch(row.Tags, opts.MaintenanceTags),
	}

	// Cluster status is always readable and decides whether the node is up at
	// all. A node it does not list, or lists offline, is dead whatever else says.
	if !inCluster || !row.Online {
		e.Health = NodeHealthDead
		e.DeadOrFenced = true
		e.Reason = "offline in cluster status"
		if !inCluster {
			e.Reason = "not listed in cluster status"
		}
		if haView == HAViewManagerStatus {
			e.CRMState = ha.nodeStatus[name]
		}
		return e
	}

	switch haView {
	case HAViewManagerStatus:
		classifyWithManagerStatus(&e, ha)
	case HAViewLRMText:
		state, listed := ha.text[name]
		classifyWithLRMText(&e, state, listed)
	default:
		classifyFromClusterStatus(&e)
	}
	return e
}

// classifyFromClusterStatus answers when no HA source is trusted. The node is
// online per cluster status, so it is placeable unless an operator tag says
// otherwise.
func classifyFromClusterStatus(e *NodeHealthEntry) {
	e.ProxyTarget = true
	if e.Maintenance {
		e.Health = NodeHealthMaintenance
		e.Reason = "operator maintenance tag"
		return
	}
	e.Health = NodeHealthOnline
	e.Placeable = true
	e.Reason = "online in cluster status"
}

// classifyWithLRMText applies the text fallback to a node that cluster status
// lists as online.
func classifyWithLRMText(e *NodeHealthEntry, state lrmTextState, listed bool) {
	e.ProxyTarget = true
	switch {
	case state == lrmTextMaintenance:
		e.Maintenance = true
		e.Health = NodeHealthMaintenance
		e.Reason = "lrm status text reports maintenance"
	case e.Maintenance:
		e.Health = NodeHealthMaintenance
		e.Reason = "operator maintenance tag"
	case listed && state == lrmTextSilent:
		e.Health = NodeHealthUnknown
		e.Reason = "lrm status text reports an old timestamp"
	default:
		e.Health = NodeHealthOnline
		e.Placeable = true
		e.Reason = "online in cluster status"
	}
}

// classifyWithManagerStatus applies the fresh CRM view and the LRM entry to a
// node that cluster status lists as online.
func classifyWithManagerStatus(e *NodeHealthEntry, ha haInputs) {
	e.CRMState = ha.nodeStatus[e.Name]

	// A fresh CRM view that reports the node unknown, fenced, or gone settles the
	// question. The node's LRM timestamp is stale because the node cannot refresh
	// it, so it must never turn this answer into "unknown".
	switch e.CRMState {
	case crmNodeUnknown, crmNodeFence, crmNodeGone:
		e.Health = NodeHealthDead
		e.DeadOrFenced = true
		e.Reason = "CRM reports the node " + e.CRMState
		return
	}

	e.ProxyTarget = true

	lrm, hasLRM := ha.lrm[e.Name]
	lrmFresh := hasLRM && lrm.Timestamp.Int() > 0 &&
		timestampFresh(ha.entryTime, lrm.Timestamp.Int(), NodeHealthLRMMaxAge)
	if lrmFresh {
		switch lrm.State {
		case lrmStateActive, lrmStateMaintenance:
			e.Watchdog = WatchdogArmed
		case lrmStateWaitForLock:
			e.Watchdog = WatchdogIdle
		}
	}

	if e.CRMState == crmNodeMaintenance {
		e.Maintenance = true
	}
	if lrmFresh && (lrm.State == lrmStateMaintenance || lrm.Mode == lrmModeMaintenance) {
		e.Maintenance = true
	}

	switch {
	case e.Maintenance:
		e.Health = NodeHealthMaintenance
		e.Reason = maintenanceReason(e.CRMState, lrmFresh && (lrm.State == lrmStateMaintenance ||
			lrm.Mode == lrmModeMaintenance))
	case e.CRMState != crmNodeOnline:
		e.Health = NodeHealthUnknown
		e.Reason = "CRM has no usable state for the node"
	case !lrmFresh:
		e.Health = NodeHealthUnknown
		e.Reason = "LRM entry is missing or older than the freshness limit"
	case lrm.Mode == lrmModeRestart || lrm.Mode == lrmModeShutdown:
		e.Health = NodeHealthOnline
		e.Reason = "LRM mode is " + lrm.Mode
	case lrm.State != lrmStateActive && lrm.State != lrmStateWaitForLock:
		// A fresh entry in any other state, such as lost_agent_lock, says the LRM
		// is not running normally, so the node takes no new work.
		e.Health = NodeHealthUnknown
		e.Reason = "LRM state is " + lrm.State
	default:
		e.Health = NodeHealthOnline
		e.Placeable = true
		e.Reason = "online with a fresh LRM"
	}
}

func maintenanceReason(crmState string, lrmSays bool) string {
	switch {
	case crmState == crmNodeMaintenance:
		return "CRM reports maintenance"
	case lrmSays:
		return "LRM reports maintenance"
	default:
		return "operator maintenance tag"
	}
}
