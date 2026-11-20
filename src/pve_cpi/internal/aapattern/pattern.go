// Package aapattern holds the anti-affinity patterns an operator can set for
// an instance group, and the engine that turns a group's members into the
// negative HA rules a pattern asks for.
//
// A pattern is a list of terms. Each term names a kind, which is block,
// stride, window, or sets, and two members are kept apart when any term says
// so. The package decodes a pattern from the JSON or YAML value an operator
// writes, checks its shape, renders it in the short canonical form the CPI
// records in an HA resource's comment, and parses that form back. It has no
// dependency on the rest of the CPI, so the config parser, the handlers, and
// the doctor can all use it.
package aapattern

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Kind names one kind of pattern term.
type Kind string

// The four kinds of pattern term.
const (
	// KindBlock sorts a domain's members by index and fills rules of size
	// members from the lowest index.
	KindBlock Kind = "block"
	// KindStride keeps the members of each residue class mod K apart.
	KindStride Kind = "stride"
	// KindWindow keeps any two members whose indexes are less than K apart
	// apart.
	KindWindow Kind = "window"
	// KindSets keeps the members of each listed set of indexes apart.
	KindSets Kind = "sets"
)

// The bounds the shape check enforces.
const (
	// MaxTerms is the most terms one pattern may list.
	MaxTerms = 8
	// MinK and MaxK bound the K of a stride or window term.
	MinK = 2
	MaxK = 64
	// MinBlockSize is the smallest size a block term may name.
	MinBlockSize = 2
	// MaxSets is the most sets one sets term may list.
	MaxSets = 32
	// MinSetSize and MaxSetSize bound the indexes in one set.
	MinSetSize = 2
	MaxSetSize = 64
	// MaxIndex is the largest index a sets term may name. Rule names carry
	// an index in at most five digits (see PatternRuleNumber).
	MaxIndex = 99999
	// MaxCanonicalLen bounds the canonical form, which the CPI writes into
	// an HA resource's comment beside other tokens, and PVE caps that
	// comment at 4096 characters.
	MaxCanonicalLen = 1024
)

// Term is one term of a pattern.
type Term struct {
	Kind Kind
	// Size is a block term's rule size, or 0 for the default of one fewer
	// member than the domain's nodes.
	Size int
	// K is a stride or window term's width.
	K int
	// Sets holds a sets term's sets of indexes, each sorted.
	Sets [][]int
}

// Pattern is an operator's anti-affinity pattern. The zero value has no terms
// and means the default, a single block term.
type Pattern struct {
	Terms []Term
	// Unsafe is true when the operator set drain_safe to false, which lets
	// the pattern's rules give a member more partners than a drain can
	// tolerate.
	Unsafe bool
}

// Default returns the built-in pattern, a single block term of the default
// size.
func Default() Pattern {
	return Pattern{Terms: []Term{{Kind: KindBlock}}}
}

// IsZero reports whether p names no term.
func (p Pattern) IsZero() bool {
	return len(p.Terms) == 0 && !p.Unsafe
}

// OrDefault returns p, or the default pattern when p names no term.
func (p Pattern) OrDefault() Pattern {
	if len(p.Terms) == 0 {
		return Pattern{Terms: Default().Terms, Unsafe: p.Unsafe}
	}
	return p
}

// Clone returns a deep copy of p, so a caller can hand it on without sharing
// its terms or sets.
func (p Pattern) Clone() Pattern {
	out := Pattern{Unsafe: p.Unsafe}
	if p.Terms == nil {
		return out
	}
	out.Terms = make([]Term, len(p.Terms))
	for i, t := range p.Terms {
		out.Terms[i] = t
		if t.Sets != nil {
			out.Terms[i].Sets = make([][]int, len(t.Sets))
			for j, set := range t.Sets {
				out.Terms[i].Sets[j] = append([]int(nil), set...)
			}
		}
	}
	return out
}

// HasKind reports whether p has a term of kind k.
func (p Pattern) HasKind(k Kind) bool {
	for _, t := range p.Terms {
		if t.Kind == k {
			return true
		}
	}
	return false
}

// IsDefaultBlock reports whether p is a single block term of the default
// size that keeps drain safety.
func (p Pattern) IsDefaultBlock() bool {
	p = p.OrDefault()
	return len(p.Terms) == 1 && p.Terms[0].Kind == KindBlock && p.Terms[0].Size == 0 && !p.Unsafe
}

// Decode reads a pattern from the value an operator wrote, as the JSON
// decoder hands it over. A pattern is a map with one of block, stride,
// window, or sets and an optional drain_safe, or a list of up to MaxTerms
// such maps. A nil value is an error, because a caller that meant "not set"
// does not call Decode.
func Decode(v any) (Pattern, error) {
	var p Pattern
	var drainSafe *bool
	switch x := v.(type) {
	case map[string]any:
		t, ds, err := decodeTerm(x, "")
		if err != nil {
			return Pattern{}, err
		}
		p.Terms, drainSafe = []Term{t}, ds
	case []any:
		if len(x) == 0 {
			return Pattern{}, errors.New("an anti-affinity pattern list must name at least one term")
		}
		if len(x) > MaxTerms {
			return Pattern{}, fmt.Errorf("an anti-affinity pattern may list at most %d terms, got %d", MaxTerms,
				len(x))
		}
		for i, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				return Pattern{}, fmt.Errorf("term %d of the anti-affinity pattern must be a map, got %s", i+1,
					typeName(item))
			}
			t, ds, err := decodeTerm(m, fmt.Sprintf("term %d: ", i+1))
			if err != nil {
				return Pattern{}, err
			}
			if ds != nil {
				if drainSafe != nil && *drainSafe != *ds {
					return Pattern{}, errors.New("the terms of the anti-affinity pattern set drain_safe to " +
						"different values")
				}
				drainSafe = ds
			}
			p.Terms = append(p.Terms, t)
		}
	case nil:
		return Pattern{}, errors.New("the anti-affinity pattern is empty")
	default:
		return Pattern{}, fmt.Errorf("the anti-affinity pattern must be a map or a list of maps, got %s",
			typeName(v))
	}
	p.Unsafe = drainSafe != nil && !*drainSafe
	if err := p.Validate(); err != nil {
		return Pattern{}, err
	}
	return p, nil
}

// DecodeJSON reads a pattern from raw JSON. See Decode.
func DecodeJSON(raw []byte) (Pattern, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return Pattern{}, fmt.Errorf("the anti-affinity pattern is not valid JSON: %w", err)
	}
	return Decode(v)
}

// decodeTerm reads one term map. It returns the term and the drain_safe value
// the map names, or nil when it names none.
func decodeTerm(m map[string]any, where string) (Term, *bool, error) {
	var t Term
	var drainSafe *bool
	kinds := 0
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		val := m[key]
		switch Kind(key) {
		case KindBlock:
			kinds++
			size, err := decodeBlock(val)
			if err != nil {
				return Term{}, nil, fmt.Errorf("%sblock: %w", where, err)
			}
			t = Term{Kind: KindBlock, Size: size}
		case KindStride, KindWindow:
			kinds++
			k, err := decodeInt(val)
			if err != nil || k < MinK || k > MaxK {
				return Term{}, nil, fmt.Errorf("%s%s must be an integer from %d to %d, got %s", where, key, MinK,
					MaxK, describe(val))
			}
			t = Term{Kind: Kind(key), K: k}
		case KindSets:
			kinds++
			sets, err := decodeSets(val)
			if err != nil {
				return Term{}, nil, fmt.Errorf("%ssets: %w", where, err)
			}
			t = Term{Kind: KindSets, Sets: sets}
		default:
			if key != "drain_safe" {
				return Term{}, nil, fmt.Errorf("%sunknown anti-affinity pattern key %q; the keys are block, stride, "+
					"window, sets, and drain_safe", where, key)
			}
			b, ok := val.(bool)
			if !ok {
				return Term{}, nil, fmt.Errorf("%sdrain_safe must be a boolean, got %s", where, describe(val))
			}
			drainSafe = &b
		}
	}
	switch {
	case kinds == 0:
		return Term{}, nil, fmt.Errorf("%san anti-affinity pattern term must name one of block, stride, window, "+
			"or sets", where)
	case kinds > 1:
		return Term{}, nil, fmt.Errorf("%san anti-affinity pattern term must name only one of block, stride, "+
			"window, or sets; write a list to combine them", where)
	}
	return t, drainSafe, nil
}

// decodeBlock reads a block term's value, which is null, an empty map, or a
// map with size.
func decodeBlock(v any) (int, error) {
	if v == nil {
		return 0, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("must be a map with an optional size, got %s", describe(v))
	}
	size := 0
	for key, val := range m {
		if key != "size" {
			return 0, fmt.Errorf("unknown key %q; a block takes only size", key)
		}
		n, err := decodeInt(val)
		if err != nil || n < MinBlockSize || n > math.MaxInt32 {
			return 0, fmt.Errorf("size must be an integer of at least %d, got %s", MinBlockSize, describe(val))
		}
		size = n
	}
	return size, nil
}

// decodeSets reads a sets term's value, a list of lists of indexes.
func decodeSets(v any) ([][]int, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a list of sets of indexes, got %s", describe(v))
	}
	if len(list) == 0 || len(list) > MaxSets {
		return nil, fmt.Errorf("must list from 1 to %d sets, got %d", MaxSets, len(list))
	}
	out := make([][]int, 0, len(list))
	for i, item := range list {
		raw, ok := item.([]any)
		if !ok {
			return nil, fmt.Errorf("set %d must be a list of indexes, got %s", i+1, describe(item))
		}
		if len(raw) < MinSetSize || len(raw) > MaxSetSize {
			return nil, fmt.Errorf("set %d must hold from %d to %d indexes, got %d", i+1, MinSetSize, MaxSetSize,
				len(raw))
		}
		set := make([]int, 0, len(raw))
		for _, x := range raw {
			n, err := decodeInt(x)
			if err != nil || n < 0 || n > MaxIndex {
				return nil, fmt.Errorf("set %d must hold indexes from 0 to %d, got %s", i+1, MaxIndex, describe(x))
			}
			if slices.Contains(set, n) {
				return nil, fmt.Errorf("set %d names index %d twice", i+1, n)
			}
			set = append(set, n)
		}
		slices.Sort(set)
		out = append(out, set)
	}
	return out, nil
}

// decodeInt reads an integer from a JSON number, which the decoder hands
// over as a float64 or a json.Number, or from a Go int.
func decodeInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, errors.New("out of range")
		}
		return int(n), nil
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, errors.New("not an integer")
		}
		return int(n), nil
	case json.Number:
		i, err := strconv.ParseInt(string(n), 10, 32)
		if err != nil {
			return 0, err
		}
		return int(i), nil
	}
	return 0, errors.New("not a number")
}

// describe renders a value for an error message.
func describe(v any) string {
	if v == nil {
		return "null"
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// typeName names the JSON type of a decoded value.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "a map"
	case []any:
		return "a list"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64, json.Number, int, int64:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

// Validate checks the shape of a pattern built in code or parsed from its
// canonical form, with the same bounds Decode enforces.
func (p Pattern) Validate() error {
	if len(p.Terms) > MaxTerms {
		return fmt.Errorf("an anti-affinity pattern may list at most %d terms, got %d", MaxTerms, len(p.Terms))
	}
	blocks := 0
	for i, t := range p.Terms {
		switch t.Kind {
		case KindBlock:
			blocks++
			if t.Size != 0 && t.Size < MinBlockSize {
				return fmt.Errorf("term %d: block size must be at least %d, got %d", i+1, MinBlockSize, t.Size)
			}
		case KindStride, KindWindow:
			if t.K < MinK || t.K > MaxK {
				return fmt.Errorf("term %d: %s must be from %d to %d, got %d", i+1, t.Kind, MinK, MaxK, t.K)
			}
		case KindSets:
			if err := validateSets(t.Sets); err != nil {
				return fmt.Errorf("term %d: sets: %w", i+1, err)
			}
		default:
			return fmt.Errorf("term %d: unknown kind %q", i+1, t.Kind)
		}
	}
	if blocks > 1 {
		return errors.New("an anti-affinity pattern may hold only one block term, because every block term " +
			"would write the same rules")
	}
	if n := len(p.Canonical()); n > MaxCanonicalLen {
		return fmt.Errorf("the anti-affinity pattern is too long to record on an HA resource: its short form "+
			"is %d characters, and the most is %d", n, MaxCanonicalLen)
	}
	return nil
}

// validateSets checks the sets of a sets term.
func validateSets(sets [][]int) error {
	if len(sets) == 0 || len(sets) > MaxSets {
		return fmt.Errorf("must list from 1 to %d sets, got %d", MaxSets, len(sets))
	}
	for i, set := range sets {
		if len(set) < MinSetSize || len(set) > MaxSetSize {
			return fmt.Errorf("set %d must hold from %d to %d indexes, got %d", i+1, MinSetSize, MaxSetSize, len(set))
		}
		seen := map[int]bool{}
		for _, n := range set {
			if n < 0 || n > MaxIndex {
				return fmt.Errorf("set %d must hold indexes from 0 to %d, got %d", i+1, MaxIndex, n)
			}
			if seen[n] {
				return fmt.Errorf("set %d names index %d twice", i+1, n)
			}
			seen[n] = true
		}
	}
	return nil
}

// unsafeSuffix ends the canonical form of a pattern that gave up drain safety.
const unsafeSuffix = "!unsafe"

// Canonical renders p in the short form the CPI records in an HA resource's
// comment, such as "block", "block:3", "stride:2+sets:0.1", or
// "window:3!unsafe". A pattern with no terms renders as the default block.
// The form holds no whitespace and no '=', so it is one marker token.
func (p Pattern) Canonical() string {
	terms := p.OrDefault().Terms
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		switch t.Kind {
		case KindBlock:
			if t.Size > 0 {
				parts = append(parts, "block:"+strconv.Itoa(t.Size))
			} else {
				parts = append(parts, "block")
			}
		case KindStride, KindWindow:
			parts = append(parts, string(t.Kind)+":"+strconv.Itoa(t.K))
		case KindSets:
			sets := make([]string, 0, len(t.Sets))
			for _, set := range t.Sets {
				nums := make([]string, 0, len(set))
				for _, n := range set {
					nums = append(nums, strconv.Itoa(n))
				}
				sets = append(sets, strings.Join(nums, "."))
			}
			parts = append(parts, "sets:"+strings.Join(sets, "/"))
		default:
			parts = append(parts, string(t.Kind))
		}
	}
	out := strings.Join(parts, "+")
	if p.Unsafe {
		out += unsafeSuffix
	}
	return out
}

// String renders p in its canonical form.
func (p Pattern) String() string { return p.Canonical() }

// Parse reads a pattern from its canonical form. See Canonical.
func Parse(s string) (Pattern, error) {
	var p Pattern
	body, unsafe := strings.CutSuffix(s, unsafeSuffix)
	p.Unsafe = unsafe
	if body == "" {
		return Pattern{}, errors.New("the anti-affinity pattern is empty")
	}
	for part := range strings.SplitSeq(body, "+") {
		kind, arg, hasArg := strings.Cut(part, ":")
		t := Term{Kind: Kind(kind)}
		switch t.Kind {
		case KindBlock:
			if hasArg {
				n, err := strconv.Atoi(arg)
				if err != nil {
					return Pattern{}, fmt.Errorf("block size %q is not a number", arg)
				}
				t.Size = n
			}
		case KindStride, KindWindow:
			n, err := strconv.Atoi(arg)
			if !hasArg || err != nil {
				return Pattern{}, fmt.Errorf("%s needs a number, got %q", kind, arg)
			}
			t.K = n
		case KindSets:
			if !hasArg || arg == "" {
				return Pattern{}, errors.New("sets needs at least one set")
			}
			for raw := range strings.SplitSeq(arg, "/") {
				var set []int
				for num := range strings.SplitSeq(raw, ".") {
					n, err := strconv.Atoi(num)
					if err != nil {
						return Pattern{}, fmt.Errorf("set %q holds %q, which is not an index", raw, num)
					}
					set = append(set, n)
				}
				slices.Sort(set)
				t.Sets = append(t.Sets, set)
			}
		default:
			return Pattern{}, fmt.Errorf("unknown anti-affinity pattern kind %q", kind)
		}
		p.Terms = append(p.Terms, t)
	}
	if err := p.Validate(); err != nil {
		return Pattern{}, err
	}
	return p, nil
}

// MarshalJSON renders p in the shape Decode reads, so a config that holds a
// pattern survives a round trip through JSON.
func (p Pattern) MarshalJSON() ([]byte, error) {
	terms := make([]map[string]any, 0, len(p.Terms))
	for _, t := range p.Terms {
		m := map[string]any{}
		switch t.Kind {
		case KindBlock:
			b := map[string]any{}
			if t.Size > 0 {
				b["size"] = t.Size
			}
			m["block"] = b
		case KindStride, KindWindow:
			m[string(t.Kind)] = t.K
		case KindSets:
			m["sets"] = t.Sets
		}
		terms = append(terms, m)
	}
	if len(terms) == 0 {
		terms = append(terms, map[string]any{"block": map[string]any{}})
	}
	if p.Unsafe {
		terms[0]["drain_safe"] = false
	}
	if len(terms) == 1 {
		return json.Marshal(terms[0])
	}
	return json.Marshal(terms)
}

// UnmarshalJSON reads p with Decode.
func (p *Pattern) UnmarshalJSON(raw []byte) error {
	got, err := DecodeJSON(raw)
	if err != nil {
		return err
	}
	*p = got
	return nil
}
