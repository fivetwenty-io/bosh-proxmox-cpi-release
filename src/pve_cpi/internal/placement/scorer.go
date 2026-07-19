// Package placement provides node scoring and selection for CPI placement decisions.
//
// The scorer uses a weighted sum over live cluster facts to rank candidate nodes.
// Weights are configurable; DefaultWeights returns production-appropriate defaults.
// An injectable *rand.Rand source makes tie-breaking deterministic in tests.
package placement

import (
	"math/rand"
	"sort"
)

// Weights controls relative importance of each scoring axis.
// All weights must be ≥ 0. A weight of 0 (or unset) is treated as "use the
// default" — it does NOT disable that axis. To suppress an axis, set its
// weight to a very small positive value or remove it from the candidate set
// upstream. The zero-means-default behaviour is enforced by ApplyDefaults and
// EffectiveWeights.
type Weights struct {
	// Mem weights the free-memory fraction (freeBytes / totalBytes). Default 1.0.
	Mem float64

	// Storage weights the free-storage fraction (freeStorageBytes / totalStorageBytes).
	// Scoring skips this axis when NodeFacts.TotalStorageBytes == 0. Default 0.5.
	Storage float64

	// CPU weights the CPU headroom fraction ((maxCPU - usedCPU) / maxCPU).
	// Scoring skips this axis when NodeFacts.MaxCPU == 0. Default 0.5.
	CPU float64

	// GuestCount weights inverse guest density: 1/(1+GuestCount).
	// Higher values prefer nodes with fewer running VMs. Default 0.3.
	GuestCount float64

	// AntiAffinity is the penalty multiplier applied per same-group VM already
	// on a node. The penalty is subtracted: −AntiAffinity × avoidCount.
	// Default 5.0.
	AntiAffinity float64

	// MemorySignal selects which memory fact backs the Mem axis:
	//
	//   MemorySignalReserved ("reserved") — the Mem axis uses
	//     reservedFree = (TotalMemBytes − CommittedMemBytes) / TotalMemBytes,
	//     clamped to [0,1] (an overcommitted node scores 0 on this axis rather
	//     than going negative). CommittedMemBytes sums every resident guest's
	//     configured (maxmem) memory regardless of run state, so the axis
	//     tracks reservations rather than actual host memory in use. This
	//     matters for sequential creates of freshly-booted VMs, which touch
	//     only a fraction of their reserved RAM: the resident-memory signal
	//     barely moves between creates and the deterministic scorer keeps
	//     picking the same node, while the reserved signal drops by a full
	//     guest's reservation on every create and fans placements out.
	//
	//   Anything else (including the zero value "") — legacy resident-memory
	//     signal: (FreeMemBytes / TotalMemBytes), no clamping. Byte-identical
	//     to pre-feature releases. Zero value intentionally means "legacy" (not
	//     "reserved") so existing callers that build a Weights literal without
	//     setting this field keep today's exact scores; the "reserved" default
	//     for new deployments is applied at the config layer (see
	//     config.CPIConfig.MemorySignalValue), which explicitly sets this field
	//     when constructing Weights for create_vm.
	MemorySignal string
}

// Memory-signal mode constants for Weights.MemorySignal. Exported so callers
// (config accessors, handlers) can reference them instead of duplicating the
// string literals.
const (
	// MemorySignalReserved selects the reserved/committed-memory Mem axis.
	MemorySignalReserved = "reserved"
	// MemorySignalResident selects the legacy resident-memory Mem axis.
	MemorySignalResident = "resident"
)

// DefaultWeights returns the production weight defaults.
func DefaultWeights() Weights {
	return Weights{
		Mem:          1.0,
		Storage:      0.5,
		CPU:          0.5,
		GuestCount:   0.3,
		AntiAffinity: 5.0,
	}
}

// NodeFacts holds observed resource facts for a single cluster node.
// All values are populated by GatherNodeFacts; zero values degrade scoring
// gracefully (see individual field docs).
type NodeFacts struct {
	// Node is the PVE node name, e.g. "pve01". Required.
	Node string

	// Online is true when PVE reports the node as reachable (online==1).
	Online bool

	// InMaintenance is true when GatherNodeFacts detected that this node is
	// in a maintenance or degraded HA state, or carries an operator maintenance
	// tag. Zero value (false) means not in maintenance, which is the safe default
	// for callers that do not populate this field (e.g. tests that construct
	// NodeFacts by hand).
	InMaintenance bool

	// FreeMemBytes is current free memory in bytes (Maxmem - Mem from cluster status).
	// Backs the Mem axis when Weights.MemorySignal is MemorySignalResident (or unset).
	FreeMemBytes int64

	// TotalMemBytes is total memory in bytes (Maxmem). Zero disables the Mem axis
	// under either memory signal.
	TotalMemBytes int64

	// CommittedMemBytes is the sum of configured (maxmem) memory across every
	// QEMU guest resident on this node, regardless of run state — a stopped
	// guest still reserves its configured RAM once BOSH starts it, so stopped
	// guests are included. Populated from cluster/resources by GatherNodeFacts;
	// zero when ListResources failed (non-fatal, same fail-open as GuestCount)
	// or when the node genuinely hosts no guests. Backs the Mem axis when
	// Weights.MemorySignal is MemorySignalReserved.
	CommittedMemBytes int64

	// FreeStorageBytes is available bytes on the target storage pool. Zero when
	// ListStorage failed or the node has no matching storage; disables Storage axis.
	FreeStorageBytes int64

	// TotalStorageBytes is total size of the target storage pool. Zero disables
	// the Storage axis.
	TotalStorageBytes int64

	// CPUUsed is the current CPU utilisation fraction [0, 1.0] reported by PVE
	// (the "cpu" field in cluster status). Used with MaxCPU to compute headroom.
	CPUUsed float64

	// MaxCPU is the number of logical CPU threads on this node. Zero disables the
	// CPU axis.
	MaxCPU int64

	// GuestCount is the number of QEMU VMs currently resident on this node.
	// Populated from cluster/resources; zero when ListResources failed (non-fatal).
	GuestCount int

	// SameGroupCount is the number of VMs on this node belonging to the same
	// BOSH instance group as the VM being placed. Used by the anti-affinity axis.
	SameGroupCount int
}

// Request describes placement requirements for a single VM create operation.
type Request struct {
	// RequiredCPU is the minimum number of CPU cores the target node must have
	// available (MaxCPU ≥ RequiredCPU). Zero means no CPU filter.
	RequiredCPU int64

	// RequiredMemBytes is the minimum free memory in bytes. Zero means no memory
	// filter beyond what the scorer naturally penalizes.
	RequiredMemBytes int64

	// RequiredStorageBytes is the minimum free storage in bytes on the target VM
	// storage pool. Zero means no storage filter (byte-identical to prior releases).
	// When > 0 and the node has storage facts (TotalStorageBytes > 0), Filter
	// rejects any node whose FreeStorageBytes < RequiredStorageBytes with reason
	// "insufficient free storage". When TotalStorageBytes == 0 (storage facts
	// unavailable), the node passes — fail-open matches the soft-axis skip
	// semantics in Score. Set by create_vm only when
	// placement.reserve_storage_headroom is enabled; default 0.
	RequiredStorageBytes int64

	// MaxUtilizationPct is the ceiling (0-100) on projected utilization of the
	// target VM storage pool after adding PlannedAddBytes. Zero means no
	// utilization filter (byte-identical to prior releases). When > 0 and the
	// node has storage facts (TotalStorageBytes > 0), Filter rejects any node
	// whose (TotalStorageBytes-FreeStorageBytes+PlannedAddBytes) /
	// TotalStorageBytes × 100 exceeds MaxUtilizationPct, with reason "storage
	// utilization ceiling exceeded". When TotalStorageBytes == 0 (storage
	// facts unavailable), the node passes — fail-open, matching
	// RequiredStorageBytes. Callers populate this only in "enforce" mode for
	// storage.max_utilization_pct; "warn" mode leaves this 0 and performs its
	// own advisory check against the chosen node after Filter/Score run, since
	// warn mode must never remove a node from the candidate set.
	MaxUtilizationPct int

	// PlannedAddBytes is the disk footprint (bytes) this create_vm call is
	// about to add to the target VM storage pool — the same pool
	// RequiredStorageBytes/TotalStorageBytes/FreeStorageBytes describe. Only
	// consulted when MaxUtilizationPct > 0.
	PlannedAddBytes int64

	// CandidateNodes restricts scoring to the named nodes when non-empty.
	// An empty slice means all online nodes are candidates.
	CandidateNodes []string

	// AvoidNodes maps node names to a penalty count for anti-affinity scoring.
	// The penalty deducted from a node's score is Weights.AntiAffinity × avoidCount.
	// Pass nil or an empty map when anti-affinity is not active.
	AvoidNodes map[string]int

	// ExcludeMaintenanceNodes, when true, causes Filter to hard-reject any node
	// whose NodeFacts.InMaintenance is true. Zero value (false) preserves legacy
	// behavior: maintenance nodes pass through Filter unchanged.
	ExcludeMaintenanceNodes bool

	// RequiredPCIAddresses lists PCI device addresses (e.g. "0000:01:00.0") that
	// the target node must expose. When non-empty, PCIChecker is called for each
	// candidate node; nodes that return (false, nil) or (_, non-nil error) are
	// rejected. When empty, no PCI filter is applied (byte-identical path).
	RequiredPCIAddresses []string

	// PCIChecker is a callback that reports whether the named node exposes all
	// addresses in RequiredPCIAddresses. Filter calls it only when
	// RequiredPCIAddresses is non-empty. A non-nil error causes the node to be
	// rejected (fail-safe). Nil checker with non-empty RequiredPCIAddresses is a
	// no-op: the node passes (callers that do not provide I/O skip the check).
	PCIChecker func(node string) (bool, error)
}

// ScoredNode is a node with its computed score.
type ScoredNode struct {
	Node  string
	Score float64
}

// Filter returns the subset of facts that satisfy the hard constraints in req:
//   - Node must be online.
//   - Node must be in req.CandidateNodes when that slice is non-empty.
//   - Node must have MaxCPU ≥ req.RequiredCPU when RequiredCPU > 0.
//   - Node must have FreeMemBytes ≥ req.RequiredMemBytes when RequiredMemBytes > 0.
//   - Node must have FreeStorageBytes ≥ req.RequiredStorageBytes when
//     RequiredStorageBytes > 0 AND TotalStorageBytes > 0. When TotalStorageBytes == 0
//     (storage facts unavailable), the node passes — fail-open matches the soft-axis
//     skip semantics in Score.
//   - Node's projected utilization ((TotalStorageBytes-FreeStorageBytes+PlannedAddBytes)
//     / TotalStorageBytes × 100) must be ≤ req.MaxUtilizationPct when MaxUtilizationPct
//     > 0 AND TotalStorageBytes > 0. Fail-open on TotalStorageBytes == 0, same as
//     RequiredStorageBytes above.
//   - When req.RequiredPCIAddresses is non-empty and req.PCIChecker is set, the
//     node must pass the PCI device check (fail-safe: error → reject).
//
// Each excluded node is accompanied by a human-readable rejection reason.
// Filter returns (nil, nil) when facts is empty.
func Filter(facts []NodeFacts, req Request) (pass []NodeFacts, rejections map[string]string) {
	if len(facts) == 0 {
		return nil, nil
	}

	// Build candidate set for O(1) lookup when the list is non-empty.
	candidateSet := make(map[string]struct{}, len(req.CandidateNodes))
	for _, n := range req.CandidateNodes {
		candidateSet[n] = struct{}{}
	}

	rejections = make(map[string]string)

	for _, f := range facts {
		if !f.Online {
			rejections[f.Node] = "node offline"
			continue
		}

		if req.ExcludeMaintenanceNodes && f.InMaintenance {
			rejections[f.Node] = "node in maintenance"
			continue
		}

		if len(candidateSet) > 0 {
			if _, ok := candidateSet[f.Node]; !ok {
				rejections[f.Node] = "not in candidate node set"
				continue
			}
		}

		if req.RequiredCPU > 0 && f.MaxCPU < req.RequiredCPU {
			rejections[f.Node] = "insufficient CPU"
			continue
		}

		if req.RequiredMemBytes > 0 && f.FreeMemBytes < req.RequiredMemBytes {
			rejections[f.Node] = "insufficient free memory"
			continue
		}

		// Storage capacity hard filter. Fail-open when TotalStorageBytes == 0:
		// storage facts are unavailable for this node (ListStorage error or no
		// matching pool), so we do not penalize it — the soft scorer already
		// skips the Storage axis under the same condition.
		if req.RequiredStorageBytes > 0 && f.TotalStorageBytes > 0 && f.FreeStorageBytes < req.RequiredStorageBytes {
			rejections[f.Node] = "insufficient free storage"
			continue
		}

		// Utilization-ceiling hard filter (storage.max_utilization_pct,
		// enforce mode only — see MaxUtilizationPct doc). Fail-open when
		// TotalStorageBytes == 0, matching the RequiredStorageBytes fail-open
		// semantics above.
		if exceedsUtilizationCeiling(f, req) {
			rejections[f.Node] = "storage utilization ceiling exceeded"
			continue
		}

		if len(req.RequiredPCIAddresses) > 0 && req.PCIChecker != nil {
			present, checkErr := req.PCIChecker(f.Node)
			if checkErr != nil {
				rejections[f.Node] = "PCI device check error: " + checkErr.Error()
				continue
			}
			if !present {
				rejections[f.Node] = "missing required PCI device"
				continue
			}
		}

		pass = append(pass, f)
	}

	if len(rejections) == 0 {
		rejections = nil
	}
	return pass, rejections
}

// exceedsUtilizationCeiling reports whether f's projected utilization —
// (TotalStorageBytes-FreeStorageBytes+PlannedAddBytes)/TotalStorageBytes×100
// — exceeds req.MaxUtilizationPct. Always false when the gate is off
// (MaxUtilizationPct <= 0) or storage facts are unavailable
// (TotalStorageBytes <= 0, fail-open, matching the RequiredStorageBytes
// fail-open semantics in Filter). Extracted from Filter to keep its
// cognitive complexity within the project threshold.
func exceedsUtilizationCeiling(f NodeFacts, req Request) bool {
	if req.MaxUtilizationPct <= 0 || f.TotalStorageBytes <= 0 {
		return false
	}
	used := f.TotalStorageBytes - f.FreeStorageBytes
	if used < 0 {
		used = 0
	}
	projectedPct := float64(used+req.PlannedAddBytes) / float64(f.TotalStorageBytes) * 100
	return projectedPct > float64(req.MaxUtilizationPct)
}

// Score computes a weighted score for each fact in facts using w.
// The returned slice is ranked descending by score (highest first).
// Ties are broken deterministically by node name (lexicographic order is
// stable; the caller is responsible for any randomized tie-breaking via Pick).
//
// Score never returns an error; malformed or zero-value facts degrade gracefully:
//   - TotalMemBytes == 0 → Mem axis contributes 0 (no divide-by-zero).
//   - TotalStorageBytes == 0 → Storage axis contributes 0.
//   - MaxCPU == 0 → CPU axis contributes 0.
func Score(facts []NodeFacts, w Weights, avoidNodes map[string]int) []ScoredNode {
	scored := make([]ScoredNode, 0, len(facts))

	for _, f := range facts {
		s := computeScore(f, w, avoidNodes)
		scored = append(scored, ScoredNode{Node: f.Node, Score: s})
	}

	// Sort descending by score; stable sub-sort by node name for equal scores.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Node < scored[j].Node
	})

	return scored
}

// computeScore returns the weighted score for a single node.
// The formula is a normalized weighted sum. Each axis is a fraction in [0, 1]
// (or 0 when the denominator is zero). The anti-affinity penalty is unbounded negative.
//
//	score = w.Mem * memFraction   (see memAxisFraction for the two MemorySignal modes)
//	      + w.Storage * (freeStorageBytes / totalStorageBytes)   [when totalStorageBytes > 0]
//	      + w.CPU * headroomFraction                             [when maxCPU > 0]
//	      + w.GuestCount * (1 / (1 + guestCount))
//	      − w.AntiAffinity * sameGroupCount
func computeScore(f NodeFacts, w Weights, avoidNodes map[string]int) float64 {
	var score float64

	// Memory axis.
	if w.Mem > 0 && f.TotalMemBytes > 0 {
		score += w.Mem * memAxisFraction(f, w)
	}

	// Storage axis.
	if w.Storage > 0 && f.TotalStorageBytes > 0 {
		score += w.Storage * (float64(f.FreeStorageBytes) / float64(f.TotalStorageBytes))
	}

	// CPU headroom axis: (1 − usedFraction).
	if w.CPU > 0 && f.MaxCPU > 0 {
		headroom := 1.0 - f.CPUUsed
		if headroom < 0 {
			headroom = 0
		}
		if headroom > 1 {
			headroom = 1
		}
		score += w.CPU * headroom
	}

	// Guest count axis: inverse density.
	if w.GuestCount > 0 {
		score += w.GuestCount * (1.0 / (1.0 + float64(f.GuestCount)))
	}

	// Anti-affinity penalty: already-counted same-group VMs on this node.
	sameGroup := f.SameGroupCount
	if avoidNodes != nil {
		if extra, ok := avoidNodes[f.Node]; ok && extra > sameGroup {
			sameGroup = extra
		}
	}
	if w.AntiAffinity > 0 && sameGroup > 0 {
		score -= w.AntiAffinity * float64(sameGroup)
	}

	return score
}

// memAxisFraction returns the [0,1] fraction that backs the Mem axis for f
// under w.MemorySignal. Callers must already have checked f.TotalMemBytes > 0.
//
//   - MemorySignalReserved: reservedFree = (TotalMemBytes − CommittedMemBytes)
//     / TotalMemBytes, clamped to [0,1]. An overcommitted node (CommittedMemBytes
//     > TotalMemBytes, e.g. thin-provisioned memory or facts gathered mid-migration)
//     clamps to 0 rather than going negative and inverting the scorer.
//   - any other value (including the zero value ""): legacy resident-memory
//     fraction (FreeMemBytes / TotalMemBytes), unclamped — byte-identical to
//     pre-feature releases.
func memAxisFraction(f NodeFacts, w Weights) float64 {
	if w.MemorySignal != MemorySignalReserved {
		return float64(f.FreeMemBytes) / float64(f.TotalMemBytes)
	}
	reservedFree := float64(f.TotalMemBytes-f.CommittedMemBytes) / float64(f.TotalMemBytes)
	if reservedFree < 0 {
		reservedFree = 0
	}
	if reservedFree > 1 {
		reservedFree = 1
	}
	return reservedFree
}

// Pick selects the best-scoring node from ranked, breaking any tie among the
// top-N equal-scored nodes using rng. When ranked is empty, Pick returns "".
//
// rng is used only when two or more nodes share the highest score; pass a
// seeded *rand.Rand for deterministic tests. Pass nil to use a local random
// source (non-deterministic). When rng is nil Pick creates a local rand source.
func Pick(ranked []ScoredNode, rng *rand.Rand) string {
	if len(ranked) == 0 {
		return ""
	}
	if len(ranked) == 1 {
		return ranked[0].Node
	}

	// Collect all nodes that share the top score.
	topScore := ranked[0].Score
	tied := []string{ranked[0].Node}
	for _, sn := range ranked[1:] {
		if sn.Score != topScore {
			break
		}
		tied = append(tied, sn.Node)
	}

	if len(tied) == 1 {
		return tied[0]
	}

	// Break tie with rng.
	if rng == nil {
		rng = rand.New(rand.NewSource(0)) // #nosec G404 -- non-crypto random for tie-break
	}
	return tied[rng.Intn(len(tied))]
}
