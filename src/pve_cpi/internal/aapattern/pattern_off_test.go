package aapattern

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecode_OffAndFalseMeanNoRules(t *testing.T) {
	t.Parallel()
	for _, v := range []any{"off", false} {
		p, err := Decode(v)
		if err != nil {
			t.Fatalf("Decode(%v): %v", v, err)
		}
		if !p.Off || len(p.Terms) != 0 || p.Unsafe {
			t.Fatalf("Decode(%v) = %+v; want the off pattern", v, p)
		}
		if p.Canonical() != "off" {
			t.Fatalf("Decode(%v).Canonical() = %q; want off", v, p.Canonical())
		}
	}
	for _, raw := range []string{`"off"`, `false`} {
		p, err := DecodeJSON([]byte(raw))
		if err != nil || !p.Off {
			t.Fatalf("DecodeJSON(%s) = %+v, %v; want the off pattern", raw, p, err)
		}
	}
}

func TestDecode_BareBlockIsTheDefaultBlock(t *testing.T) {
	t.Parallel()
	p, err := DecodeJSON([]byte(`"block"`))
	if err != nil {
		t.Fatalf("DecodeJSON(\"block\"): %v", err)
	}
	if p.Off || !p.IsDefaultBlock() || p.Canonical() != "block" {
		t.Fatalf("DecodeJSON(\"block\") = %+v; want the default block", p)
	}
}

func TestDecode_RefusesTrueAndOtherStrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want []string
	}{
		{`true`, []string{"true names no pattern", "off", "block"}},
		{`"stride"`, []string{`"stride"`, "off", "block", "a map"}},
		{`"Off"`, []string{`"Off"`, "off", "block"}},
		{`""`, []string{`""`, "off", "block"}},
		{`[{"block": {}}, "off"]`, []string{"term 2", "must be a map"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			_, err := DecodeJSON([]byte(tc.raw))
			if err == nil {
				t.Fatalf("DecodeJSON(%s) succeeded; want an error", tc.raw)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("DecodeJSON(%s) err = %v; want one that says %q", tc.raw, err, w)
				}
			}
		})
	}
}

func TestParse_OffRoundTripsAndStandsAlone(t *testing.T) {
	t.Parallel()
	p, err := Parse(Off().Canonical())
	if err != nil {
		t.Fatalf("Parse(off): %v", err)
	}
	if !p.Off || len(p.Terms) != 0 || p.Unsafe {
		t.Fatalf("Parse(off) = %+v; want the off pattern", p)
	}
	for _, s := range []string{"off+block:2", "block+off", "off!unsafe", "off:2"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", s)
		}
	}
}

func TestPattern_OffKeepsItsMeaning(t *testing.T) {
	t.Parallel()
	off := Off()
	if off.IsZero() {
		t.Fatal("the off pattern must not count as unset")
	}
	if !off.OrDefault().Off || len(off.OrDefault().Terms) != 0 {
		t.Fatalf("OrDefault() = %+v; want the off pattern kept", off.OrDefault())
	}
	if !off.Clone().Off {
		t.Fatal("Clone() dropped Off")
	}
	if off.IsDefaultBlock() || off.HasKind(KindBlock) {
		t.Fatal("the off pattern must not count as a block pattern")
	}
	if err := off.Validate(); err != nil {
		t.Fatalf("Validate(off): %v", err)
	}
	for _, bad := range []Pattern{
		{Off: true, Terms: []Term{{Kind: KindBlock}}},
		{Off: true, Unsafe: true},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) succeeded; want an error", bad)
		}
	}
	if off.String() != "off" {
		t.Fatalf("String() = %q; want off", off.String())
	}
}

func TestPattern_OffJSONRoundTrip(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Off())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"off"` {
		t.Fatalf("Marshal(off) = %s; want \"off\"", raw)
	}
	var back Pattern
	if err := json.Unmarshal(raw, &back); err != nil || !back.Off {
		t.Fatalf("Unmarshal(%s) = %+v, %v; want the off pattern", raw, back, err)
	}
	var fromFalse Pattern
	if err := json.Unmarshal([]byte(`false`), &fromFalse); err != nil || !fromFalse.Off {
		t.Fatalf("Unmarshal(false) = %+v, %v; want the off pattern", fromFalse, err)
	}
}

func TestLayout_OffWritesNoRule(t *testing.T) {
	t.Parallel()
	in := Input{
		Base: base, Pattern: Off(), Split: true, Domains: oneDomain(3), Members: members("", seq(5)...),
		Rules: map[string][]string{base: {key(0), key(1)}, base + "-2": {key(2), key(3)}},
	}
	in.Members = append(in.Members, Member{Key: "hold:5", Index: 5, Reserved: true})
	out := Layout(in)
	if len(out.Rules) != 0 || len(out.Refused) != 0 || len(out.Trimmed) != 0 || len(out.Open) != 0 ||
		len(out.Waiting) != 0 || len(out.Alone) != 0 || len(out.Overflow) != 0 || out.Unsafe {
		t.Fatalf("Layout(off) = %+v; want an empty plan", out)
	}
	if got := HeldPartners(in, "hold:5"); len(got) != 0 {
		t.Fatalf("HeldPartners(off) = %v; want none", got)
	}
}
