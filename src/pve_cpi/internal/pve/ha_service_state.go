package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// Raw states of the CRM's service_status entries. These are the only values a
// decision may read as a state. The display states that status/current shows
// for people, such as queued, starting, stopping, deleting, and clearing error
// flag, are deliberately absent, so a value that is not in this list reads as
// unknown rather than as a state.
const (
	haRawStopped         = "stopped"
	haRawStarted         = "started"
	haRawError           = "error"
	haRawRequestStop     = "request_stop"
	haRawRequestStart    = "request_start"
	haRawRequestStartBal = "request_start_balance"
	haRawMigrate         = "migrate"
	haRawRelocate        = "relocate"
	haRawFreeze          = "freeze"
	haRawFence           = "fence"
	haRawRecovery        = "recovery"
)

// Requests in the resource configuration. PVE reads the old name enabled as
// started.
const (
	haRequestedIgnored = "ignored"
	haRequestedEnabled = "enabled"
	haRequestedStarted = "started"
	haRequestedStopped = "stopped"
)

// haRequestedStates lists every request HARequestedState can return for a
// state PVE accepts. The old name enabled is absent because it reads as
// started.
var haRequestedStates = map[string]struct{}{
	haRequestedStarted: {}, haRequestedStopped: {}, haStateDisabled: {}, haRequestedIgnored: {},
}

// HARequestedState returns the requested state of an HA resource the way PVE
// HA reads it. A resource registered without a state carries none in its
// configuration, and PVE omits the field when it answers for it. The CRM reads
// such a resource as started, which is also the schema's default, and it
// reads the old name enabled as started too, so both read as started here. A
// nil or blank state therefore means started, never an unknown request. The
// value is otherwise returned as PVE sent it, trimmed, so a caller can still
// tell a value PVE does not define with HARequestedStateValid.
func HARequestedState(state *string) string {
	if state == nil {
		return haRequestedStarted
	}
	requested := strings.TrimSpace(*state)
	if requested == "" || requested == haRequestedEnabled {
		return haRequestedStarted
	}
	return requested
}

// HARequestedStateValid reports whether a value from HARequestedState is a
// request PVE HA defines.
func HARequestedStateValid(requested string) bool {
	_, ok := haRequestedStates[requested]
	return ok
}

// haServiceRawStates lists every raw CRM state that HAServiceState accepts.
var haServiceRawStates = map[string]struct{}{
	haRawStopped: {}, haRawStarted: {}, haRawError: {}, haRawRequestStop: {}, haRawRequestStart: {},
	haRawRequestStartBal: {}, haRawMigrate: {}, haRawRelocate: {}, haRawFreeze: {}, haRawFence: {},
	haRawRecovery: {},
}

// HADecisionKind says what a caller will do with the state it reads, because the
// freshness rules depend on it.
type HADecisionKind int

const (
	// HADecisionLive is a decision that acts on a live node, which means
	// proceeding through the teardown protocol on an online node, waiting for
	// motion, the watchdog check, or a SetHAState poll. It needs the LRM
	// timestamp of the VM's node to be fresh. A service in recovery is never a
	// live-node decision, so it defers.
	HADecisionLive HADecisionKind = iota + 1

	// HADecisionRecovery is a decision that may handle a dead node: recording a
	// recovery episode, running the grace window, or the dead-node completion
	// path. For a service in recovery it accepts the failed source's stale LRM
	// timestamp, because a powered-off node cannot refresh it. Every other state
	// it reads under the live-node rules.
	HADecisionRecovery
)

func (k HADecisionKind) valid() bool { return k == HADecisionLive || k == HADecisionRecovery }

// HADisposition is what the disposition table says a caller does with a service.
type HADisposition string

const (
	// HAProceed means the caller may go on with its own work, which for
	// delete_vm is the teardown protocol.
	HAProceed HADisposition = "proceed"

	// HAWaitForMotion means the CRM or LRM is still acting on the service, so
	// the caller waits through AwaitVMSettled.
	HAWaitForMotion HADisposition = "wait_for_motion"

	// HADefer means the caller returns a retriable error with no time limit.
	HADefer HADisposition = "defer"

	// HAGraceWindow means the service is in recovery on a fenced node, so the
	// caller runs the grace window. It is returned only to a recovery decision.
	HAGraceWindow HADisposition = "grace_window"

	// HAUnknown means the model cannot say, and the caller defers in every mode.
	HAUnknown HADisposition = "unknown"
)

// HAServiceState is the normalized model of one VM's HA service. It merges the
// CRM's service_status entry, the resource's configured request, and the
// freshness checks of NodeHealth, and it answers unknown, with a reason,
// whenever a field is missing, a timestamp the decision needs is stale, or the
// entry node changed during the read.
type HAServiceState struct {
	// SID is the HA service ID, vm:<vmid>.
	SID  string
	VMID int

	// Kind is the decision kind the caller stated.
	Kind HADecisionKind

	// Known is false when the model cannot answer. Reason then says why, and
	// every other field except SID, VMID, and Kind may be incomplete.
	Known  bool
	Reason string

	// Configured is true when /cluster/ha/resources lists the service, and
	// Requested is its configured request, with the old name enabled read as
	// started.
	Configured bool
	Requested  string

	// InCRM is true when the CRM's service_status holds the service. Raw is the
	// CRM's raw state and UID its identifier, which changes on every state
	// change, so the pair of SID and UID names one episode. Both are empty when
	// InCRM is false.
	InCRM bool
	Raw   string
	UID   string

	// Node is the node the decision is about. It is the CRM's node when the CRM
	// holds the service, and the scan's node otherwise. ScanNode is what the
	// caller passed from the scan.
	Node     string
	ScanNode string

	// NodeHealth is the health report's entry for Node.
	NodeHealth NodeHealthEntry

	// CRMPresent is true when a fresh CRM view was read. LRMFresh is true when
	// the LRM entry for Node carries a timestamp that is fresh on the entry
	// node's clock.
	CRMPresent bool
	LRMFresh   bool

	// EntryNode is the entry node every read of this decision came through.
	EntryNode string

	// EntryTime is the clock of ClockNode at the read, CRMStamp is the CRM
	// round's timestamp, and LRMStamp is the timestamp of Node's LRM entry, all
	// in seconds since the epoch. Each is zero when the read had no CRM view or
	// the entry carried none. A caller that waits for the CRM to act on a
	// request compares them across reads: a CRM round newer than the request's
	// EntryTime came after the request, and an LRM report newer than that
	// round came after the CRM acted.
	EntryTime int64
	CRMStamp  int64
	LRMStamp  int64

	// ClockNode names the member whose clock EntryTime came from. It is the
	// entry node unless the entry node answered its own clock read with a
	// server error, which a node in the middle of a shutdown does, and then it
	// is another online member, the CRM master first. It is empty when the read
	// had no CRM view.
	ClockNode string
}

// Managed reports whether PVE HA holds the VM, either as a configured resource
// or as a service in the CRM's state.
func (s *HAServiceState) Managed() bool { return s != nil && (s.Configured || s.InCRM) }

// Disposition applies the disposition table to the model and returns what a
// caller does, with a short reason for the error text or the log.
func (s *HAServiceState) Disposition() (HADisposition, string) {
	switch {
	case s == nil:
		return HAUnknown, "no HA state was read"
	case !s.Known:
		return HAUnknown, s.Reason
	}
	node := s.NodeHealth
	nodeOnline := node.ProxyTarget && !node.DeadOrFenced &&
		(node.Health == NodeHealthOnline || node.Health == NodeHealthMaintenance)

	if !s.Managed() {
		switch {
		case node.DeadOrFenced:
			return HADefer, fmt.Sprintf("%s is not HA-managed and node %s is not online, so PVE gives no proof "+
				"that the VM stopped", s.SID, s.Node)
		case node.ProxyTarget:
			return HAProceed, s.SID + " is not HA-managed"
		default:
			return HAUnknown, fmt.Sprintf("%s is not HA-managed and node %s has no usable state", s.SID, s.Node)
		}
	}

	if s.Raw == haRawFence || node.CRMState == crmNodeFence {
		return HADefer, fmt.Sprintf("%s is in %s: node %s is fenced or being fenced",
			s.SID, orUnset(s.Raw, "an unprocessed state"), s.Node)
	}

	if s.Raw == haRawRecovery {
		if node.CRMState == crmNodeUnknown && node.DeadOrFenced {
			if s.Kind == HADecisionRecovery {
				return HAGraceWindow, fmt.Sprintf("%s is in recovery after the fence of node %s", s.SID, s.Node)
			}
			return HADefer, fmt.Sprintf("%s is in recovery after the fence of node %s", s.SID, s.Node)
		}
		return HAUnknown, fmt.Sprintf("%s is in recovery but the CRM does not report node %s fenced", s.SID, s.Node)
	}

	if node.DeadOrFenced {
		return HADefer, fmt.Sprintf("%s is %s and node %s is not online (%s)",
			s.SID, orUnset(s.Raw, "unprocessed"), s.Node, node.Reason)
	}

	if !s.InCRM {
		// The CRM has not processed the service yet.
		if s.Requested == haRequestedIgnored {
			if !nodeOnline {
				return HAUnknown, fmt.Sprintf("%s is ignored and node %s has no usable state", s.SID, s.Node)
			}
			if s.CRMPresent && !s.LRMFresh {
				return HAUnknown, fmt.Sprintf("%s is ignored and the LRM of node %s has no fresh timestamp", s.SID, s.Node)
			}
			return HAProceed, s.SID + " is ignored, so the CRM never moves it"
		}
		return HAWaitForMotion, fmt.Sprintf("%s requests %q and the CRM has not processed it yet", s.SID, s.Requested)
	}

	if !nodeOnline {
		return HAUnknown, fmt.Sprintf("%s is %s and node %s has no usable state (%s)", s.SID, s.Raw, s.Node, node.Reason)
	}
	if !s.LRMFresh {
		return HAUnknown, fmt.Sprintf("%s is %s and the LRM of node %s has no fresh timestamp", s.SID, s.Raw, s.Node)
	}
	switch s.Raw {
	case haRawStopped, haRawStarted, haRawError:
		return HAProceed, fmt.Sprintf("%s is %s on online node %s", s.SID, s.Raw, s.Node)
	case haRawRequestStop, haRawRequestStart, haRawRequestStartBal, haRawMigrate, haRawRelocate:
		return HAWaitForMotion, fmt.Sprintf("%s is in %s", s.SID, s.Raw)
	case haRawFreeze:
		return HAWaitForMotion, s.SID + " is frozen while the LRM restarts"
	default:
		return HAUnknown, fmt.Sprintf("%s has the unrecognized CRM state %q", s.SID, s.Raw)
	}
}

func orUnset(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// HAServiceStateCluster is the subset of the PVE cluster API that
// ReadHAServiceState reads. It is satisfied by the sdk cluster service. The
// resource list comes from NodeHealthCluster.
type HAServiceStateCluster interface {
	NodeHealthCluster
	GetHaResources(ctx context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error)
}

// HAServiceStateInput names the VM and the decision.
type HAServiceStateInput struct {
	// VMID is the guest ID. It must be positive.
	VMID int

	// VMNode is the node the cluster scan names for the VM. It is the node the
	// decision is about while the CRM holds no entry for the service.
	VMNode string

	// Kind is the decision kind. It must be HADecisionLive or HADecisionRecovery.
	Kind HADecisionKind

	// Health is the node health report taken for this decision. When nil,
	// ReadHAServiceState reads one. A caller that holds a report for other
	// reasons passes it, so the whole decision rests on one view.
	Health *NodeHealthReport

	// HealthOptions applies only when Health is nil.
	HealthOptions NodeHealthOptions
}

// HAServiceSID returns the HA service ID of a VM.
func HAServiceSID(vmid int) string { return "vm:" + strconv.Itoa(vmid) }

// haViewReadable reports whether a health report's HA view is one that
// ReadHAServiceState can build a state from.
func haViewReadable(view HAView) bool {
	switch view {
	case HAViewManagerStatus, HAViewNotConfigured, HAViewIdle:
		return true
	default:
		return false
	}
}

// idleManagerConflict returns why a manager status contradicts a health report
// that found the CRM idle, or "" when there is no conflict. A service or a
// disarm request in the manager status means the CRM was not idle after all.
func idleManagerConflict(view HAView, mgr *haManagerView) string {
	switch {
	case view != HAViewIdle:
		return ""
	case len(mgr.services) > 0:
		return "manager status holds services although the health report found the CRM idle"
	case mgr.disarmed:
		return "manager status holds a disarm request although the health report found the CRM idle"
	default:
		return ""
	}
}

// ReadHAServiceState builds the normalized HA model for one VM. Every source
// failing to answer, every missing field, and every stale timestamp that the
// decision needs yields a model with Known=false and a reason, never an error,
// so a caller defers on unknown the way it defers on any other deferral. The
// error return is only for a call that is wrong, such as a nil client, a
// non-positive VM ID, or an unstated decision kind.
//
// The read goes through one entry node. The health report names it, the
// manager status names the node that served it, and the cluster status is read
// again after the other reads, so a wrapper that moved to another entry node in
// the middle of the decision makes the model unknown.
func ReadHAServiceState(
	ctx context.Context,
	clusterAPI HAServiceStateCluster,
	nodesAPI NodeHealthNodes,
	in HAServiceStateInput,
) (*HAServiceState, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ReadHAServiceState: ctx must not be nil")
	}
	if clusterAPI == nil {
		return nil, cpierrors.Cloud("ReadHAServiceState: cluster client must not be nil")
	}
	if nodesAPI == nil {
		return nil, cpierrors.Cloud("ReadHAServiceState: nodes client must not be nil")
	}
	if in.VMID <= 0 {
		return nil, cpierrors.Cloud("ReadHAServiceState: VM ID %d must be positive", in.VMID)
	}
	if !in.Kind.valid() {
		return nil, cpierrors.Cloud("ReadHAServiceState: decision kind %d is not set", in.Kind)
	}

	st := &HAServiceState{SID: HAServiceSID(in.VMID), VMID: in.VMID, Kind: in.Kind, ScanNode: in.VMNode}
	unknown := func(format string, args ...any) (*HAServiceState, error) {
		st.Known = false
		st.Reason = fmt.Sprintf(format, args...)
		return st, nil
	}

	health := in.Health
	if health == nil {
		var err error
		health, err = NodeHealth(ctx, clusterAPI, nodesAPI, in.HealthOptions)
		if err != nil {
			return unknown("node health unreadable: %v", err)
		}
	}
	st.EntryNode = health.EntryNode
	switch {
	case !health.Quorate:
		return unknown("%s", ReasonNotQuorate)
	case health.EntryNode == "":
		return unknown("no source names the entry node")
	case !haViewReadable(health.HA):
		return unknown("the HA view is %s: %s", health.HA, health.HAReason)
	}

	mgr, err := readHAManagerStatus(ctx, clusterAPI)
	if err != nil {
		return unknown("manager status unreadable: %v", err)
	}
	if mgr.quorumNode != "" && mgr.quorumNode != health.EntryNode {
		return unknown("manager status came through node %s but the health report came through %s",
			mgr.quorumNode, health.EntryNode)
	}

	// An idle CRM leaves its last round in manager status. The model reads it
	// the way it reads a cluster without HA, because the CRM holds no service,
	// and a resource configured since then reads as one the CRM has not
	// processed yet.
	if reason := idleManagerConflict(health.HA, mgr); reason != "" {
		return unknown("%s", reason)
	}
	var entryTime int64
	switch {
	case health.HA == HAViewIdle:
		// The idle CRM's last round is not a live view, so CRMPresent stays false.
	case mgr.empty() && health.HA == HAViewManagerStatus:
		return unknown("manager status is empty although the health report trusted one")
	case !mgr.empty() && health.HA != HAViewManagerStatus:
		return unknown("manager status holds a CRM view that the health report did not trust")
	case !mgr.empty():
		if mgr.quorumNode == "" {
			return unknown("manager status does not name the node that served it")
		}
		clock, clockErr := readHAClock(ctx, nodesAPI, health.EntryNode, health.clockPeers, "ha_service_state_entry_time")
		if clockErr != nil {
			return unknown("entry node clock unreadable: %v", clockErr)
		}
		entryTime = clock.now
		if mgr.crmStamp <= 0 {
			return unknown("manager status carries no CRM timestamp")
		}
		if !timestampFresh(entryTime, mgr.crmStamp, NodeHealthCRMMaxAge) {
			return unknown("CRM timestamp is %s %s", describeAge(entryTime, mgr.crmStamp), clock.describe())
		}
		st.CRMPresent = true
		st.EntryTime = entryTime
		st.ClockNode = clock.node
		st.CRMStamp = mgr.crmStamp
	}

	serviceNode, reason := st.readService(mgr)
	if reason != "" {
		return unknown("%s", reason)
	}
	if reason = st.readConfigured(ctx, clusterAPI); reason != "" {
		return unknown("%s", reason)
	}

	st.Node = st.ScanNode
	if st.InCRM {
		st.Node = serviceNode
	}
	if st.Node == "" {
		return unknown("no node is known for %s", st.SID)
	}
	entry, ok := health.Node(st.Node)
	if !ok {
		return unknown("the health report does not mention node %s", st.Node)
	}
	st.NodeHealth = entry
	if st.CRMPresent {
		lrm, has := mgr.lrm[st.Node]
		if has {
			st.LRMStamp = lrm.Timestamp.Int()
		}
		st.LRMFresh = has && lrm.Timestamp.Int() > 0 &&
			timestampFresh(entryTime, lrm.Timestamp.Int(), NodeHealthLRMMaxAge)
	}

	// The entry node must be the same one after the reads as before them.
	after, err := readClusterView(ctx, clusterAPI)
	if err != nil {
		return unknown("cluster status unreadable on the closing check: %v", err)
	}
	// NodeHealth falls back to the CRM's quorum node when no cluster-status
	// row carries local, so an empty entry here means cluster status never
	// marked the answering node, not that the entry moved.
	switch {
	case after.entry == "":
		return unknown("cluster status marks no local node, so the node that answered cannot be confirmed")
	case !after.quorate:
		return unknown("the entry node %s lost quorum during the read", after.entry)
	case after.entry != health.EntryNode:
		return unknown("the entry node changed from %s to %s during the read", health.EntryNode, after.entry)
	}

	st.Known = true
	return st, nil
}

// readService fills the CRM fields from the service_status entry and returns the
// node that the entry names. It returns a reason when the entry is present but
// unusable.
func (s *HAServiceState) readService(mgr *haManagerView) (node, reason string) {
	raw, present := mgr.services[s.SID]
	if !present {
		return "", ""
	}
	var wire struct {
		State string `json:"state"`
		Node  string `json:"node"`
		UID   string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return "", fmt.Sprintf("the CRM entry for %s does not parse: %v", s.SID, err)
	}
	if _, ok := haServiceRawStates[wire.State]; !ok {
		return "", fmt.Sprintf("the CRM entry for %s has the state %q, which is not a raw CRM state", s.SID, wire.State)
	}
	if wire.Node == "" {
		return "", fmt.Sprintf("the CRM entry for %s names no node", s.SID)
	}
	if wire.UID == "" {
		return "", fmt.Sprintf("the CRM entry for %s carries no uid", s.SID)
	}
	s.InCRM = true
	s.Raw = wire.State
	s.UID = wire.UID
	return wire.Node, ""
}

// readConfigured fills the configured request from /cluster/ha/resources. It
// returns a reason when the list is unreadable or the entry for the VM is
// unusable. A list that cannot be attributed row by row is unusable, because
// the row it cannot read might be this VM's.
//
// A row that carries no state is a resource registered without one, which PVE
// HA reads as started. The list does not promise to carry every stored field,
// so the resource itself is read before the request is taken as started, and
// a failed read of it leaves the state unknown. A state PVE does not define,
// or one that is not a string, is unknown too.
func (s *HAServiceState) readConfigured(ctx context.Context, clusterAPI HAServiceStateCluster) string {
	var resp *sdkcluster.ListHaResourcesResponse
	err := RetryOnTransient(ctx, nil, "ha_service_state_resources", 0, func() error {
		var inner error
		resp, inner = clusterAPI.ListHaResources(ctx, nil)
		return inner
	})
	if err != nil {
		return fmt.Sprintf("HA resources unreadable: %v", err)
	}
	if resp == nil {
		return "HA resources answered with no body"
	}
	found := false
	for _, raw := range *resp {
		var row struct {
			SID   string  `json:"sid"`
			State *string `json:"state"`
		}
		if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
			return fmt.Sprintf("an HA resource row does not parse: %v", jsonErr)
		}
		if row.SID != s.SID || found {
			continue
		}
		requested := HARequestedState(row.State)
		if row.State == nil || strings.TrimSpace(*row.State) == "" {
			var reason string
			if requested, reason = readConfiguredStateOf(ctx, clusterAPI, s.SID); reason != "" {
				return reason
			}
		}
		if !HARequestedStateValid(requested) {
			return fmt.Sprintf("the HA resource %s requests %q, which is not a state PVE HA defines", s.SID, requested)
		}
		found = true
		s.Configured = true
		s.Requested = requested
	}
	return ""
}

// readConfiguredStateOf reads the requested state of one HA resource from
// GET /cluster/ha/resources/{sid}, for a list row that carried none. A
// resource that carries no state here either is one PVE HA reads as started.
// It returns a reason, which leaves the caller's state unknown, when the read
// fails, answers with no body, or answers for another resource.
func readConfiguredStateOf(ctx context.Context, clusterAPI HAServiceStateCluster, sid string) (string, string) {
	var resp *sdkcluster.GetHaResourcesResponse
	err := RetryOnTransient(ctx, nil, "ha_service_state_resource", 0, func() error {
		var inner error
		resp, inner = clusterAPI.GetHaResources(ctx, sid)
		return inner
	})
	switch {
	case err != nil:
		return "", fmt.Sprintf("the HA resource %s lists no requested state, and the read of the resource "+
			"failed: %v", sid, err)
	case resp == nil:
		return "", fmt.Sprintf("the HA resource %s lists no requested state, and the read of the resource "+
			"answered with no body", sid)
	case resp.Sid != "" && resp.Sid != sid:
		return "", fmt.Sprintf("the HA resource %s lists no requested state, and the read of the resource "+
			"answered for %s", sid, resp.Sid)
	}
	return HARequestedState(resp.State), ""
}

// ReadHAServiceStates returns the raw CRM state of every service in manager
// status, keyed by service ID such as vm:100. A service whose entry does not
// parse or carries no state is left out, so a caller never reads a state the
// CRM did not report. A cluster whose manager status is empty returns an empty
// map.
func ReadHAServiceStates(ctx context.Context, clusterAPI NodeHealthCluster) (map[string]string, error) {
	if clusterAPI == nil {
		return nil, cpierrors.Cloud("ReadHAServiceStates: cluster client must not be nil")
	}
	mgr, err := readHAManagerStatus(ctx, clusterAPI)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(mgr.services))
	for sid, raw := range mgr.services {
		var wire struct {
			State string `json:"state"`
		}
		if json.Unmarshal(raw, &wire) != nil || wire.State == "" {
			continue
		}
		out[sid] = wire.State
	}
	return out, nil
}

// haManagerView is what ReadHAServiceState keeps from manager status. It holds
// the service_status entries that node health does not read.
type haManagerView struct {
	crmStamp   int64
	nodeStatus map[string]string
	lrm        map[string]lrmWire
	services   map[string]json.RawMessage
	quorumNode string
	disarmed   bool
}

// empty reports whether manager status carries no CRM or LRM view at all.
func (m *haManagerView) empty() bool {
	return len(m.nodeStatus) == 0 && len(m.lrm) == 0 && len(m.services) == 0 && m.crmStamp == 0
}

// readHAManagerStatus reads GET /cluster/ha/status/manager_status. The CRM's
// fields sit under manager_status, the LRM map under lrm_status, and the node
// that served the call under quorum; the parser also accepts the CRM fields at
// the top level. A body that does not parse is an error, and a missing or null
// body is an empty view.
func readHAManagerStatus(ctx context.Context, clusterAPI NodeHealthCluster) (*haManagerView, error) {
	var resp *sdkcluster.ListHaStatusManagerStatusResponse
	err := RetryOnTransient(ctx, nil, "ha_service_state_manager_status", 0, func() error {
		var inner error
		resp, inner = clusterAPI.ListHaStatusManagerStatus(ctx)
		return inner
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(*resp) == 0 || string(*resp) == "null" {
		return &haManagerView{}, nil
	}

	type crmFields struct {
		NodeStatus    json.RawMessage `json:"node_status"`
		LRMStatus     json.RawMessage `json:"lrm_status"`
		ServiceStatus json.RawMessage `json:"service_status"`
		Disarm        json.RawMessage `json:"disarm"`
		Timestamp     json.RawMessage `json:"timestamp"`
	}
	var top struct {
		crmFields
		ManagerStatus json.RawMessage `json:"manager_status"`
		Quorum        struct {
			Node string `json:"node"`
		} `json:"quorum"`
	}
	if jsonErr := json.Unmarshal(*resp, &top); jsonErr != nil {
		return nil, jsonErr
	}
	crm := top.crmFields
	if isJSONObject(top.ManagerStatus) {
		var nested crmFields
		if jsonErr := json.Unmarshal(top.ManagerStatus, &nested); jsonErr != nil {
			return nil, jsonErr
		}
		if len(nested.NodeStatus) > 0 {
			crm.NodeStatus = nested.NodeStatus
		}
		if len(nested.LRMStatus) > 0 && len(crm.LRMStatus) == 0 {
			crm.LRMStatus = nested.LRMStatus
		}
		if len(nested.ServiceStatus) > 0 {
			crm.ServiceStatus = nested.ServiceStatus
		}
		if len(nested.Disarm) > 0 {
			crm.Disarm = nested.Disarm
		}
		if len(nested.Timestamp) > 0 {
			crm.Timestamp = nested.Timestamp
		}
	}

	view := &haManagerView{quorumNode: top.Quorum.Node, disarmed: jsonPresent(crm.Disarm)}
	if jsonErr := decodeHAMap(crm.NodeStatus, &view.nodeStatus); jsonErr != nil {
		return nil, fmt.Errorf("node_status: %w", jsonErr)
	}
	if jsonErr := decodeHAMap(crm.LRMStatus, &view.lrm); jsonErr != nil {
		return nil, fmt.Errorf("lrm_status: %w", jsonErr)
	}
	if jsonErr := decodeHAMap(crm.ServiceStatus, &view.services); jsonErr != nil {
		return nil, fmt.Errorf("service_status: %w", jsonErr)
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
