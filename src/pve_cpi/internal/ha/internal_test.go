package ha

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComputeQuorumRejectsUnusableMembers(t *testing.T) {
	if q := computeQuorum(nil, QDevice{}); q.Known || q.UnknownReason == "" {
		t.Fatalf("no members must be unknown: %+v", q)
	}
	q := computeQuorum([]Member{{Name: "a", Votes: 1}, {Name: "b", Votes: -1}}, QDevice{})
	if q.Known || !strings.Contains(q.UnknownReason, "node b") {
		t.Fatalf("negative votes must be unknown: %+v", q)
	}
}

func TestComputeQuorumArithmetic(t *testing.T) {
	cases := []struct {
		name     string
		members  []Member
		qd       QDevice
		survives bool
	}{
		{"two nodes", []Member{{"a", true, 1}, {"b", true, 1}}, QDevice{}, false},
		{"two nodes and a connected QDevice", []Member{{"a", true, 1}, {"b", true, 1}},
			QDevice{Configured: true, Votes: 1, Status: QDeviceConnected}, true},
		{"three nodes", []Member{{"a", true, 1}, {"b", true, 1}, {"c", true, 1}}, QDevice{}, true},
		{"four nodes", []Member{{"a", true, 1}, {"b", true, 1}, {"c", true, 1}, {"d", true, 1}}, QDevice{}, true},
		{"weighted 2-1-1", []Member{{"a", true, 2}, {"b", true, 1}, {"c", true, 1}}, QDevice{}, false},
		{"weighted 3-1-1", []Member{{"a", true, 3}, {"b", true, 1}, {"c", true, 1}}, QDevice{}, false},
		{"weighted 2-2-1", []Member{{"a", true, 2}, {"b", true, 2}, {"c", true, 1}}, QDevice{}, true},
		{"a single node", []Member{{"a", true, 1}}, QDevice{}, false},
		{"all zero votes", []Member{{"a", true, 0}, {"b", true, 0}}, QDevice{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := computeQuorum(tc.members, tc.qd)
			if !q.Known || q.SurvivesOne != tc.survives {
				t.Fatalf("quorum = %+v, want survives=%v", q, tc.survives)
			}
		})
	}
}

func TestParseCPUInfoShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		known   bool
		wantErr bool
	}{
		"missing":         {raw: ``},
		"null":            {raw: `null`},
		"no model":        {raw: `{"flags":"fpu"}`},
		"blank model":     {raw: `{"model":"  "}`},
		"model only":      {raw: `{"model":"Xeon"}`, known: true},
		"wrong shape":     {raw: `[1]`, wantErr: true},
		"model not text":  {raw: `{"model":{"a":1}}`, wantErr: true},
		"extra fields ok": {raw: `{"model":"Xeon","cpus":4,"mhz":"2400.0"}`, known: true},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := parseCPUInfo(json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr || c.Known != tc.known {
				t.Fatalf("cpu=%+v err=%v", c, err)
			}
		})
	}
}

func TestParseCRSShapes(t *testing.T) {
	cases := []struct {
		raw     string
		mode    string
		rebal   bool
		auto    bool
		wantErr bool
	}{
		{raw: ``, mode: "basic"},
		{raw: `null`, mode: "basic"},
		{raw: `{}`, mode: "basic"},
		{raw: `{"ha":"static"}`, mode: "static"},
		{raw: `{"ha":"dynamic","ha-rebalance-on-start":"1","ha-auto-rebalance":true}`, mode: "dynamic", rebal: true, auto: true},
		{raw: `{"ha-rebalance-on-start":0}`, mode: "basic"},
		{raw: `"ha=static,ha-rebalance-on-start=1"`, mode: "static", rebal: true},
		{raw: `"junk,ha=static"`, mode: "static"},
		{raw: `7`, wantErr: true},
		{raw: `{"ha":`, wantErr: true},
		{raw: `"unterminated`, wantErr: true},
	}
	for _, tc := range cases {
		c, err := parseCRS(json.RawMessage(tc.raw))
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err = %v", tc.raw, err)
		}
		if !tc.wantErr && (c.Mode != tc.mode || c.RebalanceOnStart != tc.rebal || c.AutoRebalance != tc.auto || !c.Known) {
			t.Fatalf("%q: crs = %+v", tc.raw, c)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]struct {
		v  Version
		ok bool
	}{
		"9.2.4":     {Version{9, 2, 4}, true},
		"9.0":       {Version{9, 0, 0}, true},
		"8.4.1~2":   {Version{8, 4, 1}, true},
		"":          {},
		"nine":      {},
		"9":         {},
		"pve-9.2.4": {},
	} {
		got, ok := parseVersion(in)
		if ok != want.ok || got != want.v {
			t.Fatalf("parseVersion(%q) = %+v, %v", in, got, ok)
		}
	}
	if got, ok := parseVersion(" 9.1.7 "); !ok || got != (Version{9, 1, 7}) {
		t.Fatalf("surrounding space must be trimmed: %+v", got)
	}
	if !(Version{9, 2, 0}).AtLeast(9, 2) || (Version{9, 1, 9}).AtLeast(9, 2) || !(Version{10, 0, 0}).AtLeast(9, 2) {
		t.Fatal("AtLeast compares major then minor")
	}
}

func TestQDeviceAdviceNeedsKnownVotes(t *testing.T) {
	if got := qdeviceAdvice(Quorum{}, QDevice{Read: true}); got != "" {
		t.Fatalf("unknown votes must give no advice: %q", got)
	}
	if got := qdeviceAdvice(Quorum{Known: true, VotingNodes: 0}, QDevice{Read: true}); got != "" {
		t.Fatalf("no voting nodes must give no advice: %q", got)
	}
}
