package placement

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// Headroom modes, spelled as the pve.ha.headroom property spells them.
const (
	// HeadroomOff skips the check.
	HeadroomOff = "off"
	// HeadroomWarn logs a short estimate and places the VM anyway.
	HeadroomWarn = "warn"
	// HeadroomEnforce refuses a placement whose estimate is short.
	HeadroomEnforce = "enforce"
)

const bytesPerMiB = int64(1024 * 1024)

// ErrHeadroomShort is wrapped by the error EnforceHeadroom returns, so a caller
// can tell a short N+1 estimate from any other placement failure.
var ErrHeadroomShort = errors.New("ha: headroom estimate short")

// HeadroomGuest is one guest that already exists in the cluster. The check
// needs every guest, HA or not, because a survivor's free memory is its total
// memory minus the configured memory of everything that runs there.
type HeadroomGuest struct {
	// VMID identifies the guest. A guest whose VMID matches the VM being placed
	// is dropped, because the check counts the VM once, on its planned node.
	VMID int

	// Node is the node the guest runs on.
	Node string

	// MemBytes is the guest's configured memory, whatever its run state.
	MemBytes int64

	// HA says the guest is registered with PVE HA, so the CRM would recover it
	// when its node fails. A guest that is not HA is lost with its node and is
	// never placed on a survivor.
	HA bool

	// Admissible is the guest's hard admissible set, which only matters for an
	// HA guest. An empty set means the guest may run nowhere, so the check treats
	// it as fitting on no node. A caller whose guest has no hard constraint
	// passes every configured node.
	Admissible []string
}

// HeadroomVM is the VM being placed.
type HeadroomVM struct {
	// VMID identifies the VM.
	VMID int

	// MemBytes is the VM's configured memory. It must be positive.
	MemBytes int64

	// Admissible is the VM's hard admissible set when it does not depend on the
	// node the VM is placed on. An empty set means the VM may run nowhere.
	Admissible []string

	// AdmissibleFor, when set, returns the hard admissible set for a VM placed
	// on the named node and takes precedence over Admissible. It exists for a
	// set that depends on the planned node, such as a CPU migration domain.
	AdmissibleFor func(node string) []string
}

// HeadroomCheck carries everything the N+1 estimate needs besides the node
// facts. The facts supply each node's total memory, whether it is online, and
// whether it is in maintenance.
type HeadroomCheck struct {
	// Mode is off, warn, or enforce. The empty string means off. Any other value
	// is treated as enforce, so a mistyped mode never lets a short estimate pass.
	Mode string

	// VM is the HA VM being placed.
	VM HeadroomVM

	// Guests lists every existing guest of the cluster.
	Guests []HeadroomGuest

	// GuestsUnknown says the guest list could not be read. The estimate cannot
	// run, so it reports itself unavailable, which counts as short.
	GuestsUnknown bool
}

// HeadroomShortfall describes the first VM that fits on no surviving node after
// one node fails.
type HeadroomShortfall struct {
	// FailedNode is the node assumed to fail.
	FailedNode string

	// VMID and MemBytes name the VM that does not fit.
	VMID     int
	MemBytes int64

	// LargestFreeBytes and LargestFreeNode name the survivor in the VM's pin
	// with the most free memory at that point. LargestFreeNode is empty when no
	// node of the pin survives.
	LargestFreeBytes int64
	LargestFreeNode  string
}

// Line renders the shortfall as the log line and the error text carry it.
func (s HeadroomShortfall) Line() string {
	mem := mibCeil(s.MemBytes)
	if s.LargestFreeNode == "" {
		return fmt.Sprintf("ha: headroom estimate short after losing %s: VM %d (%d MiB) fits on no surviving node "+
			"in its pin, and none of those nodes is online", s.FailedNode, s.VMID, mem)
	}
	return fmt.Sprintf("ha: headroom estimate short after losing %s: VM %d (%d MiB) fits on no surviving node "+
		"in its pin, largest free %d MiB on %s", s.FailedNode, s.VMID, mem, s.LargestFreeBytes/bytesPerMiB,
		s.LargestFreeNode)
}

// HeadroomResult is the outcome of one estimate.
type HeadroomResult struct {
	// Shortfalls holds one entry for each failing node whose VMs do not all fit,
	// sorted by node name.
	Shortfalls []HeadroomShortfall

	// Unavailable is non-empty when the estimate could not run. The result then
	// counts as short, because a placement that cannot be shown to have headroom
	// is not treated as having it.
	Unavailable string
}

// Short reports whether the estimate found a shortfall or could not run.
func (r HeadroomResult) Short() bool { return r.Unavailable != "" || len(r.Shortfalls) > 0 }

// Lines returns one line for each shortfall, or the unavailable line.
func (r HeadroomResult) Lines() []string {
	if r.Unavailable != "" {
		return []string{"ha: headroom estimate unavailable: " + r.Unavailable}
	}
	lines := make([]string, 0, len(r.Shortfalls))
	for _, s := range r.Shortfalls {
		lines = append(lines, s.Line())
	}
	return lines
}

// EstimateHeadroom runs the N+1 memory estimate for vm placed on plannedNode.
//
// For each online node taken as the one that fails, it gives every other online
// node that is not in maintenance a free figure, which is the node's total
// memory minus the configured memory of every guest on it, HA or not, with the
// VM counted on plannedNode. It then places the failed node's HA guests, largest
// first, each onto the surviving node of its hard admissible set that has the
// most free memory. The first guest that fits on none of them ends that
// scenario with a shortfall. The result is an estimate, because it ignores host
// overhead, ballooning, and the order in which the CRM recovers services.
//
// The estimate counts memory only. Offline nodes neither fail nor survive, and
// a node in maintenance can fail but is not a recovery target. A facts list
// that does not hold plannedNode online, a VM without a positive memory figure,
// or an unreadable guest list makes the estimate unavailable.
func EstimateHeadroom(facts []NodeFacts, check HeadroomCheck, plannedNode string) HeadroomResult {
	if check.GuestsUnknown {
		return HeadroomResult{Unavailable: "the cluster's guests could not be read"}
	}
	if check.VM.MemBytes <= 0 {
		return HeadroomResult{Unavailable: fmt.Sprintf("VM %d has no configured memory", check.VM.VMID)}
	}
	if !nodeOnline(facts, plannedNode) {
		return HeadroomResult{Unavailable: fmt.Sprintf("planned node %q is not an online node of the cluster", plannedNode)}
	}

	guests := estimateGuests(check, plannedNode)

	failing := make([]string, 0, len(facts))
	for _, f := range facts {
		if f.Online {
			failing = append(failing, f.Node)
		}
	}
	sort.Strings(failing)

	var result HeadroomResult
	for _, failed := range failing {
		if s, short := estimateFailure(facts, guests, failed); short {
			result.Shortfalls = append(result.Shortfalls, s)
		}
	}
	return result
}

// nodeOnline reports whether facts holds an online node named node.
func nodeOnline(facts []NodeFacts, node string) bool {
	for _, f := range facts {
		if f.Node == node && f.Online {
			return true
		}
	}
	return false
}

// estimateGuests returns the guests of the estimate: the existing guests, minus
// any that carries the VM's VMID, plus the VM itself on plannedNode.
func estimateGuests(check HeadroomCheck, plannedNode string) []HeadroomGuest {
	admissible := check.VM.Admissible
	if check.VM.AdmissibleFor != nil {
		admissible = check.VM.AdmissibleFor(plannedNode)
	}
	guests := make([]HeadroomGuest, 0, len(check.Guests)+1)
	for _, g := range check.Guests {
		if check.VM.VMID > 0 && g.VMID == check.VM.VMID {
			continue
		}
		guests = append(guests, g)
	}
	return append(guests, HeadroomGuest{
		VMID:       check.VM.VMID,
		Node:       plannedNode,
		MemBytes:   check.VM.MemBytes,
		HA:         true,
		Admissible: admissible,
	})
}

// estimateFailure plays out the loss of one node. It returns the shortfall and
// true when an HA guest of the failed node fits on no survivor in its set.
func estimateFailure(facts []NodeFacts, guests []HeadroomGuest, failed string) (HeadroomShortfall, bool) {
	used := make(map[string]int64, len(facts))
	for _, g := range guests {
		used[g.Node] += g.MemBytes
	}
	free := make(map[string]int64, len(facts))
	for _, f := range facts {
		if f.Node == failed || !f.Online || f.InMaintenance {
			continue
		}
		free[f.Node] = f.TotalMemBytes - used[f.Node]
	}

	var lost []HeadroomGuest
	for _, g := range guests {
		if g.Node == failed && g.HA {
			lost = append(lost, g)
		}
	}
	sort.SliceStable(lost, func(i, j int) bool {
		if lost[i].MemBytes != lost[j].MemBytes {
			return lost[i].MemBytes > lost[j].MemBytes
		}
		return lost[i].VMID < lost[j].VMID
	})

	for _, g := range lost {
		node, largest, largestNode := bestSurvivor(free, g.Admissible, g.MemBytes)
		if node == "" {
			return HeadroomShortfall{
				FailedNode:       failed,
				VMID:             g.VMID,
				MemBytes:         g.MemBytes,
				LargestFreeBytes: largest,
				LargestFreeNode:  largestNode,
			}, true
		}
		free[node] -= g.MemBytes
	}
	return HeadroomShortfall{}, false
}

// bestSurvivor picks, among the survivors in admissible, the one with the most
// free memory that holds need, breaking ties by node name. It returns "" when
// none holds it. It always returns the largest free figure among those
// survivors and the node that has it, for the shortfall line, with the node
// empty when no survivor is in the set.
func bestSurvivor(free map[string]int64, admissible []string, need int64) (node string, largest int64, largestNode string) {
	set := append([]string(nil), admissible...)
	sort.Strings(set)
	for i, n := range set {
		if i > 0 && set[i-1] == n {
			continue
		}
		f, ok := free[n]
		if !ok {
			continue
		}
		if f < 0 {
			f = 0
		}
		if largestNode == "" || f > largest {
			largest, largestNode = f, n
		}
	}
	if largestNode != "" && largest >= need {
		node = largestNode
	}
	return node, largest, largestNode
}

// mibCeil converts bytes to MiB, rounding up so a VM never reads smaller than it is.
func mibCeil(b int64) int64 {
	if b <= 0 {
		return 0
	}
	return (b + bytesPerMiB - 1) / bytesPerMiB
}

// headroomEnforced reports whether mode refuses a short estimate.
func headroomEnforced(mode string) bool {
	switch mode {
	case "", HeadroomOff, HeadroomWarn:
		return false
	default:
		return true
	}
}

// EnforceHeadroom applies the configured mode to the estimate for vm placed on
// plannedNode. In off mode it does nothing. In warn mode it logs each line of a
// short estimate and returns nil, so the VM is placed anyway. In enforce mode,
// and in any mode it does not recognize, it returns an error that wraps
// ErrHeadroomShort and carries the same lines. A nil check is a no-op.
func EnforceHeadroom(logger *log.Logger, facts []NodeFacts, check *HeadroomCheck, plannedNode string) error {
	if check == nil || check.Mode == "" || check.Mode == HeadroomOff {
		return nil
	}
	result := EstimateHeadroom(facts, *check, plannedNode)
	if !result.Short() {
		return nil
	}
	lines := result.Lines()
	if !headroomEnforced(check.Mode) {
		for _, line := range lines {
			logger.Warn(line, log.String("node", plannedNode), log.Int("vmid", check.VM.VMID))
		}
		return nil
	}
	return fmt.Errorf("%w: placing VM %d on %s: %s", ErrHeadroomShort, check.VM.VMID, plannedNode,
		strings.Join(lines, "; "))
}

// headroomRejection returns the Filter rejection reason for a candidate node
// when the check is enforced and the estimate for that node is short, and ""
// otherwise. The reason is not one of the transient reasons the handlers know,
// so an all-nodes-short result surfaces as a permanent placement failure
// rather than a silent retry.
func headroomRejection(facts []NodeFacts, check *HeadroomCheck, node string) string {
	if check == nil || !headroomEnforced(check.Mode) {
		return ""
	}
	result := EstimateHeadroom(facts, *check, node)
	if !result.Short() {
		return ""
	}
	return "headroom short: " + strings.Join(result.Lines(), "; ")
}
