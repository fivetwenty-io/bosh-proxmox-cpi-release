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

// Prefixes of the rejection reason Filter records for a node the enforced
// headroom check removes.
const (
	// HeadroomShortPrefix starts the reason when the estimate ran and found a
	// shortfall. A shortfall is a real shortage of memory, so a caller treats
	// the refusal as permanent until the cluster changes.
	HeadroomShortPrefix = "headroom short: "

	// HeadroomUnavailablePrefix starts the reason when the estimate could not
	// run, because the guest list was unreadable, a guest or the VM has no
	// positive memory figure, or the planned node is not online. The node is
	// still removed in enforce mode, but the refusal is transient, so a caller
	// retries it rather than reporting a shortage.
	HeadroomUnavailablePrefix = "headroom unavailable: "
)

// ErrHeadroomShort is wrapped by the error EnforceHeadroom returns, so a caller
// can tell a short N+1 estimate from any other placement failure. It is wrapped
// for an unavailable estimate too, which also wraps ErrHeadroomUnavailable.
var ErrHeadroomShort = errors.New("ha: headroom estimate short")

// ErrHeadroomUnavailable is wrapped, next to ErrHeadroomShort, by the error
// EnforceHeadroom returns when the estimate could not run. It marks the refusal
// as transient, as opposed to a real shortage of memory.
var ErrHeadroomUnavailable = errors.New("ha: headroom estimate unavailable")

// HeadroomGuest is one guest that already exists in the cluster. The check
// needs every guest, HA or not, because a survivor's free memory is its total
// memory minus the configured memory of everything that runs there.
type HeadroomGuest struct {
	// VMID identifies the guest. A guest whose VMID matches the VM being placed
	// is dropped, because the check counts the VM once, on its planned node.
	VMID int

	// Node is the node the guest runs on.
	Node string

	// MemBytes is the guest's configured memory, whatever its run state. A caller
	// whose guest has no readable memory figure leaves it zero, and the estimate
	// then reports itself unavailable, as it does for any figure that is not
	// positive, when the guest runs on an online node.
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
// that does not hold plannedNode online, a VM or a guest on an online node
// without a positive memory figure, or an unreadable guest list makes the
// estimate unavailable.
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
	for _, g := range guests {
		if g.MemBytes <= 0 && nodeOnline(facts, g.Node) {
			return HeadroomResult{Unavailable: fmt.Sprintf("guest %d on %s has no configured memory", g.VMID, g.Node)}
		}
	}

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
	s, short, _ := packFailure(facts, guests, failed)
	return s, short
}

// packFailure is the packing core of estimateFailure. Besides the shortfall it
// returns the free memory of every surviving node, as it stands when the
// packing ends: after every lost HA guest was placed, or at the guest that fit
// nowhere. A survivor is an online node that is not the failed one and is not
// in maintenance.
func packFailure(facts []NodeFacts, guests []HeadroomGuest, failed string) (HeadroomShortfall, bool, map[string]int64) {
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
			}, true, free
		}
		free[node] -= g.MemBytes
	}
	return HeadroomShortfall{}, false, free
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
// ErrHeadroomShort and carries the same lines. An estimate that could not run
// also wraps ErrHeadroomUnavailable, so a caller can tell the transient refusal
// from a real shortage. A nil check is a no-op, and a nil logger discards the
// warn-mode lines.
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
		if logger == nil {
			logger = log.NewNopLogger()
		}
		for _, line := range lines {
			logger.Warn(line, log.String("node", plannedNode), log.Int("vmid", check.VM.VMID))
		}
		return nil
	}
	if result.Unavailable != "" {
		return fmt.Errorf("%w: %w: placing VM %d on %s: %s", ErrHeadroomShort, ErrHeadroomUnavailable, check.VM.VMID,
			plannedNode, strings.Join(lines, "; "))
	}
	return fmt.Errorf("%w: placing VM %d on %s: %s", ErrHeadroomShort, check.VM.VMID, plannedNode,
		strings.Join(lines, "; "))
}

// headroomRejection returns the Filter rejection reason for a candidate node
// when the check is enforced and the estimate for that node is short, and ""
// otherwise. A shortfall starts HeadroomShortPrefix, and an estimate that could
// not run starts HeadroomUnavailablePrefix, so a caller can retry the second.
func headroomRejection(facts []NodeFacts, check *HeadroomCheck, node string) string {
	if check == nil || !headroomEnforced(check.Mode) {
		return ""
	}
	result := EstimateHeadroom(facts, *check, node)
	if !result.Short() {
		return ""
	}
	prefix := HeadroomShortPrefix
	if result.Unavailable != "" {
		prefix = HeadroomUnavailablePrefix
	}
	return prefix + strings.Join(result.Lines(), "; ")
}

// HeadroomMargin is the N+1 memory margin of a cluster as it stands, which the
// doctor command reports. Unlike EstimateHeadroom it places no new VM: it plays
// out the loss of each online node against the guests that exist.
type HeadroomMargin struct {
	// Percent is the smallest margin across the scenarios, rounded down. It is
	// meaningful only when the margin is neither short nor unavailable.
	Percent int

	// TightestNode is the failed node of the scenario that gave Percent.
	TightestNode string

	// Shortfalls holds one entry for each failing node whose HA guests do not
	// all fit on the survivors, sorted by node name.
	Shortfalls []HeadroomShortfall

	// Unavailable is non-empty when the margin could not be worked out, such as
	// when a guest has no memory figure or no node loss leaves a survivor.
	Unavailable string
}

// Short reports whether some node loss leaves an HA guest without a place.
func (m HeadroomMargin) Short() bool { return m.Unavailable == "" && len(m.Shortfalls) > 0 }

// Summary renders the margin as the doctor prints it after "headroom=", which
// is ok(N%), short(<node>) with a "+" and the count of further nodes when more
// than one node loss is short, or unavailable.
func (m HeadroomMargin) Summary() string {
	switch {
	case m.Unavailable != "":
		return "unavailable"
	case len(m.Shortfalls) > 0:
		out := "short(" + m.Shortfalls[0].FailedNode
		if extra := len(m.Shortfalls) - 1; extra > 0 {
			out += fmt.Sprintf("+%d", extra)
		}
		return out + ")"
	default:
		return fmt.Sprintf("ok(%d%%)", m.Percent)
	}
}

// EstimateHeadroomMargin works out the margin of the cluster described by facts
// and guests. For each online node taken as the one that fails, it packs that
// node's HA guests onto the survivors exactly as EstimateHeadroom does, and the
// scenario's margin is the free memory left on the survivors divided by their
// total memory. A survivor is an online node that is not in maintenance and is
// not the failed one, and a survivor's free memory is never counted below zero.
// The result's Percent is the smallest margin across the scenarios, rounded
// down. A scenario in which an HA guest fits on no survivor is a shortfall, and
// a scenario that leaves no survivor with memory figures has no margin and is
// skipped. The margin is unavailable when the guest list could not be read, an
// online node or a guest on one has no positive memory figure, no node is
// online, or no scenario has a margin.
func EstimateHeadroomMargin(facts []NodeFacts, guests []HeadroomGuest, guestsUnknown bool) HeadroomMargin {
	if guestsUnknown {
		return HeadroomMargin{Unavailable: "the cluster's guests could not be read"}
	}
	var failing []string
	for _, f := range facts {
		if !f.Online {
			continue
		}
		if f.TotalMemBytes <= 0 {
			return HeadroomMargin{Unavailable: fmt.Sprintf("node %s has no total memory figure", f.Node)}
		}
		failing = append(failing, f.Node)
	}
	if len(failing) == 0 {
		return HeadroomMargin{Unavailable: "the cluster has no online node"}
	}
	for _, g := range guests {
		if g.MemBytes <= 0 && nodeOnline(facts, g.Node) {
			return HeadroomMargin{Unavailable: fmt.Sprintf("guest %d on %s has no configured memory", g.VMID, g.Node)}
		}
	}
	sort.Strings(failing)

	var m HeadroomMargin
	measured := false
	for _, failed := range failing {
		shortfall, short, free := packFailure(facts, guests, failed)
		if short {
			m.Shortfalls = append(m.Shortfalls, shortfall)
			continue
		}
		var left, total int64
		for _, f := range facts {
			if f.Node == failed || !f.Online || f.InMaintenance {
				continue
			}
			total += f.TotalMemBytes
			left += max(free[f.Node], 0)
		}
		if total <= 0 {
			continue
		}
		percent := int(left * 100 / total)
		if !measured || percent < m.Percent {
			m.Percent, m.TightestNode, measured = percent, failed, true
		}
	}
	if !measured && len(m.Shortfalls) == 0 {
		return HeadroomMargin{Unavailable: "no node loss leaves a surviving node to measure"}
	}
	return m
}
