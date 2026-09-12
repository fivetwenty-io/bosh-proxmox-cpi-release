// Package storageplacement ranks immutable, eligible storage backings. Inventory,
// eligibility, plan execution, and recovery belong to its callers.
package storageplacement

import (
	"crypto/rand"
	"fmt"
	"math/bits"
)

// Policy identifies an installed algorithm with frozen versioned semantics.
type Policy struct {
	Name    string
	Version int
}

// RequestSnapshot is a value-only snapshot for one planning attempt or split-role
// continuation. SeedSet distinguishes an injected all-zero seed from no entropy.
// RequestedBytes describes the logical request; candidate charges include the
// physical requirements of their chosen clone/allocation mechanisms.
type RequestSnapshot struct {
	Policy          Policy
	Namespace       string
	AllocationKey   string
	AllocationGroup string
	RequestedBytes  uint64
	Seed            [32]byte
	SeedSet         bool
}

// CapacityBudget carries a consolidated observation and charges in bytes.
// OutstandingBytes excludes AllocationBytes. MaxUtilizationPct=0 means no
// additional ceiling. The planner must combine all applicable reserves/ceilings
// before calling Rank. Values are copied, never shared through pointers or maps.
type CapacityBudget struct {
	TotalBytes        uint64
	AvailableBytes    uint64
	ReserveBytes      uint64
	OutstandingBytes  uint64
	AllocationBytes   uint64
	MaxUtilizationPct int
}

// BudgetProjection is the admitted result of a capacity observation and its
// outstanding/new charges. ResidualBytes includes reserves and ceilings but is
// not floored; an exact fit has zero residual. ProjectedUsedBytes does not count
// a reserve as physical usage.
type BudgetProjection struct {
	ResidualBytes      uint64
	ProjectedUsedBytes uint64
	TotalBytes         uint64
}

// ProjectBudget enforces checked reserve, ceiling, and aggregate charge limits.
// Inventory/ledger callers can share exactly the admission arithmetic used by
// every built-in strategy. It neither mutates the input nor reserves storage.
func ProjectBudget(b CapacityBudget) (BudgetProjection, error) {
	p, err := project(b)
	if err != nil {
		return BudgetProjection{}, err
	}
	return BudgetProjection{ResidualBytes: p.residual, ProjectedUsedBytes: p.used, TotalBytes: p.total}, nil
}

// EligibleCandidate represents exactly one canonical physical backing, not a
// node observation. DomainKey names a declared capacity domain; empty means an
// independent domain whose budget is Member. Domain observations and policy
// limits must agree across members, but candidate-specific charges may differ.
type EligibleCandidate struct {
	StorageID  string
	BackingKey string
	DomainKey  string
	Member     CapacityBudget
	Domain     CapacityBudget
}

// RankedCandidate preserves the exact input value. Scores are diagnostics only;
// least-utilized ordering compares integer ratios without floating-point loss.
// DomainScore is populated for hierarchical strategies. Rendezvous is the exact
// SHA-256 tie score; residuals include the one-byte weighted exact-fit floor.
type RankedCandidate struct {
	Candidate    EligibleCandidate
	Score        float64
	DomainScore  float64
	Rendezvous   [32]byte
	MemberWeight uint64
	DomainWeight uint64
	Reason       string
}

// NewSeed creates entropy once, before a planning attempt. Rank never redraws.
func NewSeed() ([32]byte, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return [32]byte{}, fmt.Errorf("generate storage placement seed: %w", err)
	}
	return seed, nil
}

type projection struct {
	residual uint64
	used     uint64
	total    uint64
}

func project(b CapacityBudget) (projection, error) {
	if b.TotalBytes == 0 || b.AvailableBytes > b.TotalBytes {
		return projection{}, fmt.Errorf("invalid total/available capacity: %d/%d", b.TotalBytes, b.AvailableBytes)
	}
	if b.MaxUtilizationPct < 0 || b.MaxUtilizationPct > 100 {
		return projection{}, fmt.Errorf("invalid utilization ceiling: %d", b.MaxUtilizationPct)
	}
	charges, carry := bits.Add64(b.OutstandingBytes, b.AllocationBytes, 0)
	if carry != 0 {
		return projection{}, fmt.Errorf("capacity charges overflow")
	}
	if b.ReserveBytes > b.AvailableBytes {
		return projection{}, fmt.Errorf("reserve exceeds available capacity")
	}
	usable := b.AvailableBytes - b.ReserveBytes
	used := b.TotalBytes - b.AvailableBytes
	if b.MaxUtilizationPct != 0 {
		hi, lo := bits.Mul64(b.TotalBytes, uint64(b.MaxUtilizationPct))
		ceiling, _ := bits.Div64(hi, lo, 100)
		if used > ceiling {
			return projection{}, fmt.Errorf("current utilization exceeds ceiling")
		}
		usable = min(usable, ceiling-used)
	}
	if charges > usable {
		return projection{}, fmt.Errorf("capacity charges %d exceed usable bytes %d", charges, usable)
	}
	return projection{residual: usable - charges, used: used + charges, total: b.TotalBytes}, nil
}

// compareRatio compares a.used/a.total with b.used/b.total using full 128-bit
// products. Both totals have already been checked as positive by project.
func compareRatio(a, b projection) int {
	ah, al := bits.Mul64(a.used, b.total)
	bh, bl := bits.Mul64(b.used, a.total)
	if ah < bh || ah == bh && al < bl {
		return -1
	}
	if ah > bh || ah == bh && al > bl {
		return 1
	}
	return 0
}
