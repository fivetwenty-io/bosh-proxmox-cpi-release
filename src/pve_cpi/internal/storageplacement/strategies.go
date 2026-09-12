package storageplacement

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

type strategyKind uint8

const (
	spread strategyKind = iota
	weighted
	least
)

type builtinStrategy struct{ kind strategyKind }

type domainIdentity struct{ class, key string }
type preparedCandidate struct {
	candidate      EligibleCandidate
	member, domain projection
	domainID       domainIdentity
	ranked         RankedCandidate
}

func domainOf(c EligibleCandidate) domainIdentity {
	if c.DomainKey != "" {
		return domainIdentity{class: "declared", key: c.DomainKey}
	}
	return domainIdentity{class: "backing", key: c.BackingKey}
}

func prepare(candidates []EligibleCandidate) ([]preparedCandidate, error) {
	prepared := make([]preparedCandidate, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	storageIDs := make(map[string]bool, len(candidates))
	domains := make(map[string]CapacityBudget)
	for cIndex := range candidates {
		c := candidates[cIndex]
		if strings.TrimSpace(c.StorageID) == "" || strings.TrimSpace(c.BackingKey) == "" {
			return nil, fmt.Errorf("candidate requires storage and backing identities")
		}
		if c.DomainKey != "" && strings.TrimSpace(c.DomainKey) == "" {
			return nil, fmt.Errorf("blank capacity domain")
		}
		if _, err := EncodeTuple(c.StorageID, c.BackingKey, c.DomainKey); err != nil {
			return nil, fmt.Errorf("candidate identity: %w", err)
		}
		if seen[c.BackingKey] || storageIDs[c.StorageID] {
			return nil, fmt.Errorf("duplicate backing or storage identity %q", c.StorageID)
		}
		seen[c.BackingKey], storageIDs[c.StorageID] = true, true
		member, err := project(c.Member)
		if err != nil {
			return nil, fmt.Errorf("member %q: %w", c.StorageID, err)
		}
		domain := member
		if c.DomainKey != "" {
			facts := c.Domain
			facts.OutstandingBytes, facts.AllocationBytes = 0, 0
			if previous, exists := domains[c.DomainKey]; exists && previous != facts {
				return nil, fmt.Errorf("inconsistent capacity domain %q", c.DomainKey)
			}
			domains[c.DomainKey] = facts
			domain, err = project(c.Domain)
			if err != nil {
				return nil, fmt.Errorf("domain %q: %w", c.DomainKey, err)
			}
		} else if c.Domain != (CapacityBudget{}) {
			return nil, fmt.Errorf("undeclared domain must use member budget for %q", c.StorageID)
		}
		prepared = append(prepared, preparedCandidate{candidate: c, member: member, domain: domain, domainID: domainOf(c)})
	}
	return prepared, nil
}

func (s builtinStrategy) Rank(request RequestSnapshot, candidates []EligibleCandidate) ([]RankedCandidate, error) {
	if s.kind == weighted && !request.SeedSet {
		return nil, fmt.Errorf("weighted ranking requires a frozen seed")
	}
	prepared, err := prepare(candidates)
	if err != nil {
		return nil, err
	}
	weights := make(map[domainIdentity]uint64)
	for cIndex := range prepared {
		c := prepared[cIndex]
		weight := max(uint64(1), c.domain.residual)
		if previous, exists := weights[c.domainID]; !exists || weight < previous {
			weights[c.domainID] = weight
		}
	}
	domainScores := make(map[domainIdentity]float64, len(weights))
	if s.kind == weighted {
		for id, weight := range weights {
			score, err := weightedScore(request, weight, "domain", id.class, id.key)
			if err != nil {
				return nil, err
			}
			domainScores[id] = score
		}
	}
	for i := range prepared {
		c := &prepared[i]
		hash, err := rendezvous(request, c.candidate.BackingKey)
		if err != nil {
			return nil, err
		}
		c.ranked = RankedCandidate{Candidate: c.candidate, Rendezvous: hash}
		switch s.kind {
		case spread:
			c.ranked.Reason = "descending SHA-256 rendezvous score"
		case weighted:
			c.ranked.MemberWeight = max(uint64(1), c.member.residual)
			c.ranked.DomainWeight = weights[c.domainID]
			c.ranked.DomainScore = domainScores[c.domainID]
			c.ranked.Score, err = weightedScore(request, c.ranked.MemberWeight, "member", c.candidate.BackingKey)
			if err != nil {
				return nil, err
			}
			c.ranked.Reason = "ascending domain then member exponential-race score"
		case least:
			c.ranked.DomainScore = float64(c.domain.used) / float64(c.domain.total)
			c.ranked.Score = float64(c.member.used) / float64(c.member.total)
			c.ranked.Reason = "ascending projected domain then member utilization; rendezvous ties"
		}
	}
	slices.SortFunc(prepared, func(a, b preparedCandidate) int { return comparePrepared(s.kind, a, b) })
	result := make([]RankedCandidate, len(prepared))
	for i := range prepared {
		result[i] = prepared[i].ranked
	}
	return result, nil
}

func comparePrepared(kind strategyKind, a, b preparedCandidate) int {
	switch kind {
	case weighted:
		if a.domainID != b.domainID {
			if order := compareFloat(a.ranked.DomainScore, b.ranked.DomainScore); order != 0 {
				return order
			}
			if order := strings.Compare(a.domainID.class, b.domainID.class); order != 0 {
				return order
			}
			return strings.Compare(a.domainID.key, b.domainID.key)
		}
		if order := compareFloat(a.ranked.Score, b.ranked.Score); order != 0 {
			return order
		}
		return strings.Compare(a.candidate.BackingKey, b.candidate.BackingKey)
	case least:
		if order := compareRatio(a.domain, b.domain); order != 0 {
			return order
		}
		if order := compareRatio(a.member, b.member); order != 0 {
			return order
		}
	}
	if order := bytes.Compare(b.ranked.Rendezvous[:], a.ranked.Rendezvous[:]); order != 0 {
		return order
	}
	return strings.Compare(a.candidate.BackingKey, b.candidate.BackingKey)
}

func compareFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
