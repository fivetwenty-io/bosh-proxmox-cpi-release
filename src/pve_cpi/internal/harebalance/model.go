// Package harebalance plans how pve-ops ha rebalance moves one Director's HA
// VMs back into a good spread after a rolling node maintenance. It is pure: it
// takes a model of the cluster that the caller read, and it returns an ordered
// list of live migrations, each with a phase and a reason, and the VMs it left
// where they are. The caller runs one move, reads the cluster again, and plans
// again from the fresh model.
//
// A node's load is the fraction of its memory that its guests reserve, their
// configured memory whatever their run state, over its total memory. The
// spread is the highest load minus the lowest among the nodes that can take
// work. The plan has three phases.
//
//   - Separation moves a member of an anti-affinity rule off a node that
//     holds another member of that rule, and evens out the members of one
//     anti-affinity group, its split rules included, to at most
//     ceil(members / nodes) on each node.
//   - AZ restore returns each VM that sits outside its AZ to the node of its
//     AZ with the lowest load.
//   - Memory evening moves VMs that may run on more than one node of their AZ
//     while the spread is wide, one move at a time, each one cutting the sum of
//     squared loads.
//
// No move takes a VM outside its targets or its AZ, onto a node that holds
// another member of one of its anti-affinity rules, or onto a node that the
// memory and headroom filter of placement.Filter rejects.
package harebalance

// Phase names the phase of the plan that made a move.
type Phase string

// The phases of a plan, in the order they run.
const (
	PhaseSeparate Phase = "separate"
	PhaseRestore  Phase = "restore"
	PhaseSpread   Phase = "spread"
)

// SkipKind names why a VM is not moved.
type SkipKind string

// The reasons a VM is not moved. The caller sets every kind but SkipPinRewrite
// and SkipNoTarget, which the planner sets.
const (
	// SkipReplicate is a VM of the replicate policy. A move to its partner
	// would reverse its replication job, and a move anywhere else would leave
	// it without a replica.
	SkipReplicate SkipKind = "replicate"
	// SkipHold is a VM whose strict pin names one node.
	SkipHold SkipKind = "hold"
	// SkipLocalDisk is a VM with a volume on node-local storage.
	SkipLocalDisk SkipKind = "local_disk"
	// SkipDLB is a member of PVE's CRS load balancer.
	SkipDLB SkipKind = "dlb"
	// SkipNotStarted is a VM whose HA state is not started.
	SkipNotStarted SkipKind = "not_started"
	// SkipLocked is a VM with a PVE config lock.
	SkipLocked SkipKind = "locked"
	// SkipNarrowing is a VM the narrowing barrier holds.
	SkipNarrowing SkipKind = "narrowing"
	// SkipPositiveAffinity is a VM in a positive resource affinity rule.
	SkipPositiveAffinity SkipKind = "positive_affinity"
	// SkipFailbackPending is a VM with failback on that is off its
	// highest-priority online node, which PVE HA returns on its own.
	SkipFailbackPending SkipKind = "failback_pending"
	// SkipRebalancePending is a VM that carries a rebalance mark that the run
	// could not finish.
	SkipRebalancePending SkipKind = "rebalance_pending"
	// SkipUndecided is a VM whose HA decision or pin could not be read into a
	// set of targets.
	SkipUndecided SkipKind = "undecided"
	// SkipRefused is a VM whose move this run tried and left where it was,
	// because PVE refused it, a write lost a race, or another call held the
	// VM, so no later plan of the run chooses it again.
	SkipRefused SkipKind = "refused"
	// SkipPinRewrite is a VM whose move would need a pin that changes more
	// than the priorities of the pin it has.
	SkipPinRewrite SkipKind = "pin_rewrite"
	// SkipNoTarget is a VM outside its AZ that no node of its AZ can take now.
	SkipNoTarget SkipKind = "no_target"
)

// Node is one node of the cluster.
type Node struct {
	Name string
	// TotalMemBytes is the node's memory. A node without it takes no work.
	TotalMemBytes int64
	// Online and Maintenance say whether the node can take work. Only a node
	// that is online and not in maintenance is a target and counts in the
	// spread.
	Online      bool
	Maintenance bool
}

// Guest is one guest of the cluster, this Director's or not. Its memory
// counts in the load of its node.
type Guest struct {
	VMID     int
	Node     string
	MemBytes int64
	// HA and Admissible feed the headroom estimate (see placement.HeadroomGuest).
	HA         bool
	Admissible []string
}

// VM is one of this Director's VMs. Every VM must also be in Model.Guests.
type VM struct {
	VMID     int
	Name     string
	Node     string
	MemBytes int64

	// Skip, when set, keeps the VM where it is, and SkipReason says why.
	Skip       SkipKind
	SkipReason string

	// Targets lists the nodes the VM may run on: the nodes of its pin, inside
	// its admissible set and its CPU migration domain.
	Targets []string
	// Hard is the VM's hard admissible set, for the headroom estimate.
	Hard []string
	// AZ lists the nodes of the VM's AZ, read from its pin, and AZName names
	// the AZ. An empty AZ means the pin carries no AZ preference, as a flat
	// pin does, and every target is in the VM's AZ.
	AZ     []string
	AZName string

	// Rewrite names the targets a move to which rewrites the VM's pin.
	Rewrite map[string]bool
	// PinRefused names the targets whose pin, the one the CPI would write for
	// the VM there, changes more than priorities, with the reason. A move to
	// such a target is never planned.
	PinRefused map[string]string
}

// Rule is one live anti-affinity rule.
type Rule struct {
	ID string
	// Group names the anti-affinity group the rule belongs to, which is the
	// rule's name without the suffix of a split rule.
	Group   string
	Members []int
}

// Model is the cluster as the caller read it.
type Model struct {
	Nodes  []Node
	Guests []Guest
	VMs    []VM
	Rules  []Rule
	// HeadroomMode is pve.ha.headroom, which placement.Filter applies.
	HeadroomMode string
}

// Options shape a plan.
type Options struct {
	// MaxMoves caps the moves the plan returns. The rest count as deferred.
	MaxMoves int
	// StartThreshold and StopThreshold are the hysteresis of memory evening.
	// It starts when the spread is above StartThreshold and stops when the
	// spread is at or below StopThreshold.
	StartThreshold, StopThreshold float64
	// RestoreOnly skips memory evening.
	RestoreOnly bool
	// Spreading says an earlier plan of the same run started memory evening,
	// so this plan goes on while the spread is above StopThreshold.
	Spreading bool
}

// Move is one planned live migration.
type Move struct {
	VMID         int
	Name         string
	From, To     string
	Phase        Phase
	Reason       string
	PinRewritten bool
}

// Skip is a VM the plan leaves where it is.
type Skip struct {
	VMID   int
	Name   string
	Node   string
	Kind   SkipKind
	Reason string
}

// NodeLoad is a node's committed memory as a fraction of its memory, and the
// number of guests it holds.
type NodeLoad struct {
	Committed float64
	Guests    int
}

// Plan is what Build returns.
type Plan struct {
	Moves    []Move
	Skipped  []Skip
	Deferred int
	// Before and After give the load of every node that can take work, now and
	// once every planned move, the deferred ones included, has run.
	Before, After map[string]NodeLoad
	// SpreadBefore and SpreadAfter are the spread now and after the plan.
	SpreadBefore, SpreadAfter float64
	// Spreading says memory evening ran in this plan.
	Spreading bool
}
