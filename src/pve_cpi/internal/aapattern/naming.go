package aapattern

import (
	"strconv"
	"strings"
)

// A group's rules share the group's base name, bosh-aa-<hash12>. A block
// term's rules keep the names the CPI has always used, the base name for the
// first rule and base-2, base-3, and so on after it, so a group on the default
// pattern sees no renames. Every other rule is a pattern rule named
// base-p<n>, where n packs the rule's domain, its term, and a number local to
// the term (see PatternRuleNumber).

// Factors of a pattern rule's number.
const (
	domainFactor = 10_000_000
	termFactor   = 100_000
	classFactor  = 1000
	// MaxChunk is the highest chunk number a stride class may use.
	MaxChunk = classFactor - 1
)

// ChunkName names a block term's n-th rule, the first being the base name.
func ChunkName(base string, n int) string {
	if n <= 1 {
		return base
	}
	return base + "-" + strconv.Itoa(n)
}

// ChunkIndex returns n when id is the base's n-th block rule.
func ChunkIndex(base, id string) (int, bool) {
	if id == base {
		return 1, true
	}
	rest, ok := strings.CutPrefix(id, base+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 2 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// PatternRuleName names the base's pattern rule number n.
func PatternRuleName(base string, n int) string {
	return base + "-p" + strconv.Itoa(n)
}

// PatternRuleIndex returns n when id is the base's pattern rule number n.
func PatternRuleIndex(base, id string) (int, bool) {
	rest, ok := strings.CutPrefix(id, base+"-p")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// IsGroupRule reports whether id is one of the base's rules, a block rule or
// a pattern rule.
func IsGroupRule(base, id string) bool {
	if _, ok := ChunkIndex(base, id); ok {
		return true
	}
	_, ok := PatternRuleIndex(base, id)
	return ok
}

// PatternRuleNumber packs a pattern rule's number. domain is 0 for a group
// spread over the whole cluster and the AZ's position plus one otherwise, term
// is the term's position in the pattern from 1, or 0 for the rule the CPI adds
// to keep the two lowest indexes apart, and local is the number within the
// term. For a stride term, local is 1000 times the residue class plus the
// class's chunk number. For a window term, it is the window's lowest index,
// and for a sets term it is the set's position from 1.
func PatternRuleNumber(domain, term, local int) int {
	return domain*domainFactor + term*termFactor + local
}

// SplitPatternRuleNumber unpacks a pattern rule's number.
func SplitPatternRuleNumber(n int) (domain, term, local int) {
	return n / domainFactor, n % domainFactor / termFactor, n % termFactor
}

// StrideLocal packs a stride rule's local number from its class and chunk.
func StrideLocal(class, chunk int) int {
	return class*classFactor + chunk
}

// SplitStrideLocal unpacks a stride rule's local number.
func SplitStrideLocal(local int) (class, chunk int) {
	return local / classFactor, local % classFactor
}
