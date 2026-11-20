package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// MigrateAPI is the part of the cluster API that asks PVE HA to migrate a
// resource. The SDK's cluster service satisfies it.
type MigrateAPI interface {
	CreateHaResourcesMigrate(ctx context.Context, sid string,
		params *sdkcluster.CreateHaResourcesMigrateParams) (*sdkcluster.CreateHaResourcesMigrateResponse, error)
}

// MoveFailure says why MoveIntoSet did not bring a VM into a node set.
type MoveFailure int

const (
	// MoveNodeDown means the VM's node is offline, fenced, or gone, so no
	// migration can start from it until PVE HA recovers the VM or the node
	// comes back.
	MoveNodeDown MoveFailure = iota + 1
	// MoveRefused means PVE refused the move, or the CPI could not ask for
	// one, such as when no node of the set can take the VM now.
	MoveRefused
	// MoveIncomplete means PVE accepted the move and the VM is still outside
	// the set, because the CRM dropped the request, the migration failed, or
	// the wait ran out.
	MoveIncomplete
)

// MoveError is the error MoveIntoSet returns. Reason says what happened in
// words an operator reads, and Err is the cause when there is one.
type MoveError struct {
	Failure MoveFailure
	Reason  string
	Err     error
}

func (e *MoveError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *MoveError) Unwrap() error { return e.Err }

// The CRM raw states of a resource PVE HA is moving.
const (
	haStateMigrate  = "migrate"
	haStateRelocate = "relocate"
)

// MoveInput asks MoveIntoSet to bring an HA-managed VM into Set.
type MoveInput struct {
	VMID int
	// Node is the node the caller last saw the VM on.
	Node string
	// Set is the node set the VM must end up in.
	Set []string
	// Targets lists the nodes of Set that can take the VM now, best first.
	// MoveTargets builds it.
	Targets []string
	// Budget bounds the wait for the move, and PollInterval is the pause
	// between reads.
	Budget       time.Duration
	PollInterval time.Duration
	// BeforeMigrate, when set, runs right before the migrate request, after
	// every check that sends nothing, so it runs only for a move that PVE HA
	// is asked for. The CPI turns failback off there, because failback on
	// would have the CRM pull the VM back to the node its old pin prefers. An
	// error stops the move with MoveRefused, and no request is sent.
	BeforeMigrate func(ctx context.Context) error
	// OnComigrated, when set, receives the resources a migrate response
	// names as comigrated. PVE queues the request before it reports them, so
	// they move with the VM, and the move goes on.
	OnComigrated func(sids []string)
	// AwaitUnanswered, when set, treats a migrate request that PVE never
	// answered, such as one lost on the wire or cut off by a gateway, as one
	// PVE may have queued. The move then waits as it would after an answer,
	// and a CRM that drops the request ends it with MoveIncomplete instead of
	// MoveRefused. A request cut off by the end of ctx returns
	// MoveIncomplete.
	AwaitUnanswered bool
}

// MoveIntoSet asks PVE HA to migrate an HA-managed VM to a node of in.Set and
// waits until the VM runs there, settled. It is the HA migrate request, the
// same one ha-manager migrate sends, so the CRM moves the VM the way it moves
// any HA resource and keeps the VM under HA the whole time. The order is:
//
//  1. Read the VM's HA state. A node that is offline, fenced, or gone returns
//     MoveNodeDown, and nothing is sent.
//  2. Wait until the VM settles, so the request never fights a migration,
//     relocation, or lock in flight. A VM that settles inside the set needs
//     no request.
//  3. Check that PVE HA holds the VM with a requested state of started or
//     stopped, the only states the CRM migrates, run in.BeforeMigrate, and
//     send the request to the first target the VM is not on. A refusal, or a response that names
//     resources blocking the move, returns MoveRefused. A response that names comigrated resources
//     goes to in.OnComigrated, and the move goes on, because PVE has queued it already. With
//     in.AwaitUnanswered, a request PVE never answered goes on to the wait as well.
//  4. Wait for the move. A VM that settles inside the set ends the wait. The
//     CRM reads its commands at the start of a round, so the first round
//     stamped after the request may still predate it, and only a second round
//     that leaves the VM where it was proves the CRM dropped the request. That,
//     a resource in the error state, or a wait that runs out returns
//     MoveIncomplete.
//
// It returns where the VM is once it runs inside the set.
func MoveIntoSet(ctx context.Context, deps NarrowDeps, in MoveInput) (pve.VMLocation, error) {
	b := &barrier{deps: deps, in: NarrowInput{Budget: in.Budget, PollInterval: in.PollInterval}}
	b.defaults()
	deps = b.deps
	in.Budget, in.PollInterval = b.in.Budget, b.in.PollInterval
	set := toSet(in.Set)
	sid := pve.HAServiceSID(in.VMID)
	where := strings.Join(in.Set, ",")

	st, err := deps.ReadState(ctx, in.VMID, in.Node)
	if fail := moveReadFailure(st, err); fail != nil {
		return pve.VMLocation{}, fail
	}
	if st.NodeHealth.DeadOrFenced {
		return pve.VMLocation{}, moveNodeDown(st.Node)
	}
	loc, err := deps.Settle(ctx, in.VMID)
	if err != nil {
		return loc, &MoveError{Failure: MoveIncomplete, Reason: "the VM did not settle before the move", Err: err}
	}
	if set[loc.Node] {
		return loc, nil
	}
	st, err = deps.ReadState(ctx, in.VMID, loc.Node)
	if fail := moveReadFailure(st, err); fail != nil {
		return loc, fail
	}
	if st.NodeHealth.DeadOrFenced {
		return loc, moveNodeDown(st.Node)
	}
	switch {
	case !st.Managed():
		return loc, &MoveError{Failure: MoveRefused, Reason: "PVE HA does not manage the VM"}
	case st.Raw == haStateError:
		return loc, &MoveError{Failure: MoveRefused, Reason: "the VM's HA resource is in the error state"}
	case st.Requested != haStateStarted && st.Requested != haStateStopped:
		return loc, &MoveError{Failure: MoveRefused, Reason: fmt.Sprintf("the VM's HA resource requests %s, "+
			"and PVE HA migrates only a resource that requests started or stopped", st.Requested)}
	}
	if deps.Migrate == nil {
		return loc, &MoveError{Failure: MoveRefused, Reason: "the CPI has no client to ask PVE HA for the move"}
	}
	target := ""
	for _, n := range in.Targets {
		if set[n] && n != loc.Node {
			target = n
			break
		}
	}
	if target == "" {
		return loc, &MoveError{Failure: MoveRefused, Reason: fmt.Sprintf("no node of %s can take the VM now",
			where)}
	}
	if in.BeforeMigrate != nil {
		if err := in.BeforeMigrate(ctx); err != nil {
			return loc, &MoveError{Failure: MoveRefused, Reason: fmt.Sprintf("the CPI could not prepare the "+
				"migration of %s to node %s", sid, target), Err: err}
		}
	}
	resp, err := deps.Migrate.CreateHaResourcesMigrate(ctx, sid, &sdkcluster.CreateHaResourcesMigrateParams{
		Node: target,
	})
	if err != nil {
		if _, answered := pve.AnsweredStatus(err); !answered && in.AwaitUnanswered {
			if ctx.Err() != nil {
				return loc, &MoveError{Failure: MoveIncomplete, Reason: fmt.Sprintf("the migrate request of %s "+
					"to node %s ended before PVE answered, so PVE may have queued it", sid, target),
					Err: pve.WrapError(err)}
			}
			deps.Logger.Warn("ha move: PVE never answered the migrate request, so the move waits in case PVE "+
				"queued it", log.Int("vmid", in.VMID), log.String("sid", sid), log.String("to", target),
				log.Err(err))
			return awaitMove(ctx, deps, in, set, loc.Node, target)
		}
		return loc, &MoveError{Failure: MoveRefused, Reason: fmt.Sprintf("PVE refused to migrate %s to node %s",
			sid, target), Err: pve.WrapError(err)}
	}
	if blocking := moveBlockers(resp); blocking != "" {
		return loc, &MoveError{Failure: MoveRefused, Reason: fmt.Sprintf("PVE HA will not migrate %s to node %s, "+
			"because %s", sid, target, blocking)}
	}
	if partners := moveComigrated(resp); len(partners) > 0 {
		deps.Logger.Warn("ha move: PVE HA moves other resources with the VM, because a positive resource "+
			"affinity rule names them", log.Int("vmid", in.VMID), log.String("sid", sid),
			log.String("to", target), log.String("comigrated", strings.Join(partners, ",")))
		if in.OnComigrated != nil {
			in.OnComigrated(partners)
		}
	}
	deps.Logger.Info("ha move: asked PVE HA to migrate the VM into its node set",
		log.Int("vmid", in.VMID), log.String("sid", sid), log.String("from", loc.Node),
		log.String("to", target), log.String("set", where))
	return awaitMove(ctx, deps, in, set, loc.Node, target)
}

// awaitMove waits for the move MoveIntoSet asked for (see its step 4).
func awaitMove(ctx context.Context, deps NarrowDeps, in MoveInput, set map[string]bool, from, target string) (
	pve.VMLocation, error,
) {
	deadline := deps.Now().Add(in.Budget)
	node := from
	var requestTime, firstRound int64
	timed := false
	why := "no CRM round has run since the request"
	for {
		st, err := deps.ReadState(ctx, in.VMID, node)
		if err == nil && st != nil && st.Known && !timed {
			// The first read after the request gives the request time on the
			// entry node's clock, so any CRM round newer than it came after
			// the request landed.
			requestTime, timed = st.EntryTime, true
		}
		switch {
		case err != nil:
			why = err.Error()
		case st == nil || !st.Known:
			why = "the VM's HA state is unknown"
			if st != nil && st.Reason != "" {
				why = st.Reason
			}
		case st.NodeHealth.DeadOrFenced:
			return pve.VMLocation{}, moveNodeDown(st.Node)
		case st.Raw == haStateError:
			return pve.VMLocation{}, &MoveError{Failure: MoveIncomplete,
				Reason: "the VM's HA resource went into the error state during the move"}
		case st.Raw == haStateMigrate || st.Raw == haStateRelocate:
			why = fmt.Sprintf("PVE HA is moving the VM to node %s", target)
			firstRound = 0
		case set[st.Node]:
			loc, err := deps.Settle(ctx, in.VMID)
			if err != nil {
				return loc, &MoveError{Failure: MoveIncomplete, Reason: "the VM did not settle after the move",
					Err: err}
			}
			if set[loc.Node] {
				deps.Logger.Info("ha move: the VM runs inside its node set", log.Int("vmid", in.VMID),
					log.String("node", loc.Node))
				return loc, nil
			}
			why = fmt.Sprintf("the VM is on %s but the state read named %s", loc.Node, st.Node)
			node, firstRound = loc.Node, 0
		case st.CRMStamp == 0 || st.CRMStamp <= requestTime:
			why = "no CRM round has run since the request"
		case firstRound == 0:
			firstRound = st.CRMStamp
			why = "the CRM has run one round since the request, which may have started before it"
		case st.CRMStamp > firstRound:
			return pve.VMLocation{}, &MoveError{Failure: MoveIncomplete, Reason: fmt.Sprintf("PVE HA ran a round "+
				"after the request and left the VM on node %s, so it dropped the request or the migration failed",
				st.Node)}
		}
		if !deps.Now().Before(deadline) {
			return pve.VMLocation{}, &MoveError{Failure: MoveIncomplete, Reason: fmt.Sprintf("the VM did not "+
				"reach a node of %s within %s (%s)", strings.Join(in.Set, ","), in.Budget, why)}
		}
		if err := deps.Sleep(ctx, in.PollInterval); err != nil {
			return pve.VMLocation{}, &MoveError{Failure: MoveIncomplete, Reason: "the wait for the move ended",
				Err: err}
		}
	}
}

// moveReadFailure turns a state read that cannot answer into a MoveError.
func moveReadFailure(st *pve.HAServiceState, err error) error {
	switch {
	case err != nil:
		return &MoveError{Failure: MoveIncomplete, Reason: "the VM's HA state could not be read", Err: err}
	case st == nil || !st.Known:
		reason := "the VM's HA state is unknown"
		if st != nil && st.Reason != "" {
			reason += " (" + st.Reason + ")"
		}
		return &MoveError{Failure: MoveIncomplete, Reason: reason}
	}
	return nil
}

func moveNodeDown(node string) error {
	return &MoveError{Failure: MoveNodeDown, Reason: fmt.Sprintf("node %s is offline, fenced, or gone, so no "+
		"migration can start from it", node)}
}

// moveBlockers describes the resources a migrate response names as blocking
// the move, and returns "" when it names none.
func moveBlockers(resp *sdkcluster.CreateHaResourcesMigrateResponse) string {
	if resp == nil || len(resp.BlockingResources) == 0 {
		return ""
	}
	var parts []string
	for _, raw := range resp.BlockingResources {
		var b struct {
			SID   string `json:"sid"`
			Cause string `json:"cause"`
		}
		if err := json.Unmarshal(raw, &b); err != nil || b.SID == "" {
			parts = append(parts, string(raw))
			continue
		}
		switch b.Cause {
		case "node-affinity":
			parts = append(parts, b.SID+" has a node affinity rule that excludes the node")
		case "resource-affinity":
			parts = append(parts, b.SID+" has a resource affinity rule that keeps it apart from the node")
		default:
			parts = append(parts, fmt.Sprintf("%s blocks it (%s)", b.SID, b.Cause))
		}
	}
	return strings.Join(parts, ", and ")
}

// moveComigrated returns the resources a migrate response names as moving
// with the VM. PVE lists each as a service id, and an entry of another shape
// is kept as its raw JSON so it still reaches the caller.
func moveComigrated(resp *sdkcluster.CreateHaResourcesMigrateResponse) []string {
	if resp == nil {
		return nil
	}
	var out []string
	for _, raw := range resp.ComigratedResources {
		var sid string
		if err := json.Unmarshal(raw, &sid); err == nil && sid != "" {
			out = append(out, sid)
			continue
		}
		var obj struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.SID != "" {
			out = append(out, obj.SID)
			continue
		}
		out = append(out, string(raw))
	}
	return out
}

// MoveTargets returns the nodes of set that can take a VM now, other than
// node, best first. A node pin gives a higher priority to a better node, and
// nodes of equal priority come in name order. A nil pin ranks every node the
// same.
func MoveTargets(p *Profile, set []string, pin *Rule, node string) []string {
	placeable := map[string]bool{}
	if p != nil {
		placeable = p.placeableSet()
	}
	var out []string
	for _, n := range normalizeNodes(set) {
		if n != node && placeable[n] {
			out = append(out, n)
		}
	}
	priority := func(n string) int {
		if pin == nil {
			return 0
		}
		return pin.Nodes[n]
	}
	slices.SortStableFunc(out, func(a, b string) int { return priority(b) - priority(a) })
	return out
}
