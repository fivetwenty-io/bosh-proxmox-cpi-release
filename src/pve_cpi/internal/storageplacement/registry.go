package storageplacement

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Strategy may only rank the supplied eligible values. Registry.Rank enforces
// complete, one-to-one output and rejects changes to supplied candidate facts.
type Strategy interface {
	Rank(RequestSnapshot, []EligibleCandidate) ([]RankedCandidate, error)
}

// Registration adds a compiled algorithm to a new immutable registry.
type Registration struct {
	Policy   Policy
	Strategy Strategy
}

// Registry has no mutation operations and can be shared across requests. Custom
// strategy implementations must also be safe for concurrent use.
type Registry struct{ strategies map[Policy]Strategy }

var builtins = builtinRegistry()

func builtinRegistry() *Registry {
	return &Registry{strategies: map[Policy]Strategy{
		{Name: "spread", Version: 1}:              builtinStrategy{kind: spread},
		{Name: "weighted_free_space", Version: 1}: builtinStrategy{kind: weighted},
		{Name: "least_utilized", Version: 1}:      builtinStrategy{kind: least},
	}}
}

// NewRegistry includes the three built-ins and any additional compiled policies.
// Existing versions cannot be replaced; changed semantics need a new version.
func NewRegistry(extra ...Registration) (*Registry, error) {
	r := builtinRegistry()
	for _, registration := range extra {
		p := registration.Policy
		if strings.TrimSpace(p.Name) == "" || p.Version < 1 || registration.Strategy == nil {
			return nil, fmt.Errorf("invalid strategy registration %q/v%d", p.Name, p.Version)
		}
		if _, exists := r.strategies[p]; exists {
			return nil, fmt.Errorf("duplicate strategy registration %q/v%d", p.Name, p.Version)
		}
		r.strategies[p] = registration.Strategy
	}
	return r, nil
}

// ValidateStrategy rejects names and versions not installed in this CPI binary.
func ValidateStrategy(name string, version int) error {
	return builtins.ValidateStrategy(name, version)
}

// ValidateStrategy reports whether this registry contains the exact policy.
func (r *Registry) ValidateStrategy(name string, version int) error {
	if r == nil {
		return fmt.Errorf("nil strategy registry")
	}
	if _, exists := r.strategies[Policy{Name: name, Version: version}]; !exists {
		return fmt.Errorf("unsupported storage strategy %q/v%d", name, version)
	}
	return nil
}

// Rank runs an installed built-in through the central contract validator.
func Rank(request RequestSnapshot, candidates []EligibleCandidate) ([]RankedCandidate, error) {
	return builtins.Rank(request, candidates)
}

// Rank protects caller-owned input, checks admission facts, and validates the
// entire result. A strategy error never selects a different algorithm.
func (r *Registry) Rank(request RequestSnapshot, candidates []EligibleCandidate) ([]RankedCandidate, error) {
	if err := r.ValidateStrategy(request.Policy.Name, request.Policy.Version); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Namespace) == "" || strings.TrimSpace(request.AllocationKey) == "" || strings.TrimSpace(request.AllocationGroup) == "" {
		return nil, fmt.Errorf("ranking requires namespace, allocation key, and allocation group")
	}
	if _, err := tuple(request); err != nil {
		return nil, fmt.Errorf("ranking identity: %w", err)
	}
	if _, err := prepare(candidates); err != nil {
		return nil, err
	}
	input := slices.Clone(candidates)
	result, err := r.strategies[request.Policy].Rank(request, input)
	if err != nil {
		return nil, fmt.Errorf("rank %s/v%d: %w", request.Policy.Name, request.Policy.Version, err)
	}
	// Sorting an input slice is harmless, but changing facts is a contract error.
	if err := validateCandidates(candidates, input); err != nil {
		return nil, fmt.Errorf("strategy mutated input: %w", err)
	}
	output := make([]EligibleCandidate, len(result))
	for i := range result {
		output[i] = result[i].Candidate
		if math.IsNaN(result[i].Score) || math.IsInf(result[i].Score, 0) || math.IsNaN(result[i].DomainScore) || math.IsInf(result[i].DomainScore, 0) {
			return nil, fmt.Errorf("strategy returned nonfinite diagnostic score")
		}
	}
	if err := validateCandidates(candidates, output); err != nil {
		return nil, fmt.Errorf("invalid strategy ranking: %w", err)
	}
	return slices.Clone(result), nil
}

func validateCandidates(original, returned []EligibleCandidate) error {
	if len(original) != len(returned) {
		return fmt.Errorf("candidate count changed from %d to %d", len(original), len(returned))
	}
	expected := make(map[string]EligibleCandidate, len(original))
	for cIndex := range original {
		c := original[cIndex]
		expected[c.BackingKey] = c
	}
	for rangeIndex119 := range returned {
		want, exists := expected[returned[rangeIndex119].BackingKey]
		if !exists {
			return fmt.Errorf("foreign or duplicate candidate %q", returned[rangeIndex119].BackingKey)
		}
		if returned[rangeIndex119] != want {
			return fmt.Errorf("candidate facts changed for %q", returned[rangeIndex119].BackingKey)
		}
		delete(expected, returned[rangeIndex119].BackingKey)
	}
	return nil
}
