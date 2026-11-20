package aapattern

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecode_AcceptsEveryKindAndList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{`{"block": {}}`, "block"},
		{`{"block": null}`, "block"},
		{`{"block": {"size": 3}}`, "block:3"},
		{`{"stride": 2}`, "stride:2"},
		{`{"window": 3, "drain_safe": false}`, "window:3!unsafe"},
		{`{"window": 3, "drain_safe": true}`, "window:3"},
		{`{"sets": [[0, 1, 2], [3, 1, 0]]}`, "sets:0.1.2/0.1.3"},
		{`[{"stride": 2}, {"sets": [[0, 1]]}]`, "stride:2+sets:0.1"},
		{`[{"stride": 2}, {"block": {}, "drain_safe": false}]`, "stride:2+block!unsafe"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			p, err := DecodeJSON([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeJSON(%s): %v", tc.raw, err)
			}
			if got := p.Canonical(); got != tc.want {
				t.Fatalf("Canonical() = %q; want %q", got, tc.want)
			}
			back, err := Parse(p.Canonical())
			if err != nil {
				t.Fatalf("Parse(%q): %v", p.Canonical(), err)
			}
			if back.Canonical() != tc.want {
				t.Fatalf("Parse round trip = %q; want %q", back.Canonical(), tc.want)
			}
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			again, err := DecodeJSON(raw)
			if err != nil {
				t.Fatalf("DecodeJSON(%s) after Marshal: %v", raw, err)
			}
			if again.Canonical() != tc.want {
				t.Fatalf("JSON round trip = %q; want %q", again.Canonical(), tc.want)
			}
		})
	}
}

func TestDecode_RefusesBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{`"block"`, "must be a map or a list"},
		{`[]`, "at least one term"},
		{`{}`, "must name one of"},
		{`{"drain_safe": false}`, "must name one of"},
		{`{"stride": 2, "window": 3}`, "only one of"},
		{`{"strde": 2}`, "unknown anti-affinity pattern key"},
		{`{"stride": 1}`, "from 2 to 64"},
		{`{"stride": 65}`, "from 2 to 64"},
		{`{"window": 2.5}`, "from 2 to 64"},
		{`{"window": "3"}`, "from 2 to 64"},
		{`{"block": {"size": 1}}`, "at least 2"},
		{`{"block": {"sise": 3}}`, "unknown key"},
		{`{"block": 3}`, "must be a map"},
		{`{"sets": []}`, "from 1 to 32"},
		{`{"sets": [[0]]}`, "from 2 to 64"},
		{`{"sets": [[0, 0]]}`, "twice"},
		{`{"sets": [[0, -1]]}`, "from 0 to"},
		{`{"window": 2, "drain_safe": "no"}`, "boolean"},
		{`[{"stride": 2, "drain_safe": false}, {"sets": [[0, 1]], "drain_safe": true}]`, "different values"},
		{`[{"block": {}}, {"block": {"size": 2}}]`, "only one block"},
		{`[1]`, "must be a map"},
		{`[{"stride":2},{"stride":2},{"stride":2},{"stride":2},{"stride":2},{"stride":2},{"stride":2},` +
			`{"stride":2},{"stride":2}]`, "at most 8"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeJSON([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DecodeJSON(%s) err = %v; want one that says %q", tc.raw, err, tc.want)
			}
		})
	}
}

func TestDecode_RefusesAPatternTooLongToRecord(t *testing.T) {
	t.Parallel()
	sets := make([]string, 0, 32)
	for s := range 32 {
		nums := make([]string, 0, 64)
		for i := range 64 {
			nums = append(nums, json.Number(itoa(10000+s*64+i)).String())
		}
		sets = append(sets, "["+strings.Join(nums, ",")+"]")
	}
	_, err := DecodeJSON([]byte(`{"sets": [` + strings.Join(sets, ",") + `]}`))
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v; want the pattern refused as too long to record", err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestParse_RefusesBadForms(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "!unsafe", "stride", "stride:x", "window:1", "sets:", "sets:0", "sets:0.a",
		"block:1", "bogus", "block+block"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", s)
		}
	}
}

func TestPattern_DefaultAndZero(t *testing.T) {
	t.Parallel()
	var zero Pattern
	if !zero.IsZero() || zero.Canonical() != "block" || !zero.IsDefaultBlock() {
		t.Fatalf("zero pattern: IsZero=%v Canonical=%q IsDefaultBlock=%v", zero.IsZero(), zero.Canonical(),
			zero.IsDefaultBlock())
	}
	p, _ := Parse("block:3")
	if p.IsDefaultBlock() {
		t.Fatal("block:3 must not count as the default block")
	}
}

func TestNaming_RoundTrips(t *testing.T) {
	t.Parallel()
	base := "bosh-aa-0123456789ab"
	if ChunkName(base, 1) != base || ChunkName(base, 3) != base+"-3" {
		t.Fatalf("ChunkName = %q, %q", ChunkName(base, 1), ChunkName(base, 3))
	}
	if n, ok := ChunkIndex(base, base+"-3"); !ok || n != 3 {
		t.Fatalf("ChunkIndex(-3) = %d, %v", n, ok)
	}
	n := PatternRuleNumber(2, 3, StrideLocal(5, 7))
	name := PatternRuleName(base, n)
	if name != base+"-p20305007" {
		t.Fatalf("PatternRuleName = %q", name)
	}
	if _, ok := ChunkIndex(base, name); ok {
		t.Fatal("a pattern rule must not read as a block rule")
	}
	got, ok := PatternRuleIndex(base, name)
	if !ok || got != n {
		t.Fatalf("PatternRuleIndex = %d, %v; want %d", got, ok, n)
	}
	d, term, local := SplitPatternRuleNumber(got)
	class, chunk := SplitStrideLocal(local)
	if d != 2 || term != 3 || class != 5 || chunk != 7 {
		t.Fatalf("split = %d %d %d %d", d, term, class, chunk)
	}
	for _, id := range []string{base, base + "-2", base + "-p0", base + "-p100001"} {
		if !IsGroupRule(base, id) {
			t.Errorf("IsGroupRule(%q) = false", id)
		}
	}
	for _, id := range []string{base + "-1", base + "-02", base + "-p", base + "-p01", base + "-px", "bosh-aa-other"} {
		if IsGroupRule(base, id) {
			t.Errorf("IsGroupRule(%q) = true", id)
		}
	}
}
