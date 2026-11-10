package endpointcache

import (
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func validatedPayload(t *testing.T, p *Payload) (*Payload, []Drop) {
	t.Helper()
	got, drops, err := Validate(p, p.Key, payloadNow)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return got, drops
}

func TestValidateAcceptsAWellFormedPayload(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2", "pve3")
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow.Add(-time.Minute), Class: MarkTLSHandshake}}
	p.ColdStart = &ColdStartHint{Name: "pve2", ProbedAt: payloadNow.Add(-10 * time.Minute)}
	got, drops := validatedPayload(t, p)
	if len(drops) != 0 {
		t.Errorf("a well-formed payload dropped %v", drops)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("Validate changed a well-formed payload:\n got %+v\nwant %+v", got, p)
	}
	if got == p || got.Discovery == p.Discovery {
		t.Error("Validate returned the caller's payload, not a copy")
	}
}

func TestValidateDoesNotChangeItsInput(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2", "localhost")
	p.Discovery.Nodes[0].Pins = []string{strings.ToUpper(payloadPin("ab"))}
	before := p.clone()
	if _, _, err := Validate(p, p.Key, payloadNow); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, before) {
		t.Error("Validate changed its input")
	}
}

func TestValidateRefusesANilPayloadAndAWrongKey(t *testing.T) {
	if _, _, err := Validate(nil, payloadTestKey(), payloadNow); !errors.Is(err, ErrPayloadInvalid) {
		t.Errorf("a nil payload: %v", err)
	}
	other := payloadTestKey()
	other.CredentialID = "0000000000000000"
	if _, _, err := Validate(payloadFor(other), payloadTestKey(), payloadNow); !errors.Is(err, ErrKeyMismatch) ||
		!errors.Is(err, ErrFileInvalid) {
		t.Errorf("a payload for another key: %v", err)
	}
}

func TestValidateRefusesTheWholeFile(t *testing.T) {
	tooMany := payloadFor(payloadTestKey())
	for i := range MaxNodes + 1 {
		tooMany.Discovery.Nodes = append(tooMany.Discovery.Nodes, payloadNode("n"+string(rune('a'+i%26))+string(rune('a'+i/26)), payloadNow))
	}
	cases := map[string]func(p *Payload){
		"node name with dots": func(p *Payload) {
			p.Discovery.Nodes[0].Name = "api.attacker.example"
		},
		"node name of 64 characters": func(p *Payload) {
			p.Discovery.Nodes[0].Name = strings.Repeat("a", 64)
		},
		"node name with a leading hyphen": func(p *Payload) { p.Discovery.Nodes[0].Name = "-pve2" },
		"empty node name":                 func(p *Payload) { p.Discovery.Nodes[0].Name = "" },
		"node name with a space":          func(p *Payload) { p.Discovery.Nodes[0].Name = "pve 2" },
		"address with a space":            func(p *Payload) { p.Discovery.Nodes[0].Address = "pve2 .dc1.example.com" },
		"address with a trailing dot":     func(p *Payload) { p.Discovery.Nodes[0].Address = "pve2.dc1.example.com." },
		"empty address":                   func(p *Payload) { p.Discovery.Nodes[0].Address = "" },
		"address with a port":             func(p *Payload) { p.Discovery.Nodes[0].Address = "pve2.dc1.example.com:8006" },
		"port zero":                       func(p *Payload) { p.Discovery.Nodes[0].Port = 0 },
		"port above range":                func(p *Payload) { p.Discovery.Nodes[0].Port = 65536 },
		"no pins":                         func(p *Payload) { p.Discovery.Nodes[0].Pins = nil },
		"four pins": func(p *Payload) {
			p.Discovery.Nodes[0].Pins = []string{payloadPin("01"), payloadPin("02"), payloadPin("03"), payloadPin("04")}
		},
		"short pin":   func(p *Payload) { p.Discovery.Nodes[0].Pins = []string{payloadPin("a1")[:62]} },
		"long pin":    func(p *Payload) { p.Discovery.Nodes[0].Pins = []string{payloadPin("a1") + "00"} },
		"non-hex pin": func(p *Payload) { p.Discovery.Nodes[0].Pins = []string{strings.Repeat("zz", 32)} },
		"colon-spelled pin": func(p *Payload) {
			p.Discovery.Nodes[0].Pins = []string{"A1:" + strings.Repeat("a1:", 30) + "a1"}
		},
		"repeated node name": func(p *Payload) {
			p.Discovery.Nodes = append(p.Discovery.Nodes, payloadNode("PVE2", payloadNow))
		},
		"unknown outcome":              func(p *Payload) { p.Discovery.Outcome = "dial: connection refused" },
		"empty outcome":                func(p *Payload) { p.Discovery.Outcome = "" },
		"short cluster digest":         func(p *Payload) { p.Cluster.RootCASHA256 = payloadPin("3c")[:16] },
		"non-hex cluster digest":       func(p *Payload) { p.Cluster.RootCASHA256 = strings.Repeat("zz", 32) },
		"cluster observed via no host": func(p *Payload) { p.Cluster.ObservedVia = ".:8006" },
		"nodes without a cluster":      func(p *Payload) { p.Cluster = nil },
		"expired nodes without a cluster": func(p *Payload) {
			p.Cluster = nil
			p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		},
		"hint name with dots": func(p *Payload) { p.ColdStart = &ColdStartHint{Name: "pve2.example.com", ProbedAt: payloadNow} },
		"hint name empty":     func(p *Payload) { p.ColdStart = &ColdStartHint{ProbedAt: payloadNow} },
		"more than 64 nodes":  func(p *Payload) { p.Discovery.Nodes = tooMany.Discovery.Nodes },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2")
			mutate(p)
			_, _, err := Validate(p, p.Key, payloadNow)
			if !errors.Is(err, ErrPayloadInvalid) || !errors.Is(err, ErrFileInvalid) {
				t.Fatalf("Validate: %v, want ErrPayloadInvalid", err)
			}
		})
	}
}

func TestValidateErrorsNeverEchoTheFile(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2")
	p.Discovery.Nodes[0].Name = "api.attacker.example"
	_, _, err := Validate(p, p.Key, payloadNow)
	if err == nil || strings.Contains(err.Error(), "attacker") {
		t.Errorf("error %v echoes a value from the file", err)
	}
}

func TestValidateAcceptsExactlyMaxNodes(t *testing.T) {
	p := payloadFor(payloadTestKey())
	for i := range MaxNodes {
		p.Discovery.Nodes = append(p.Discovery.Nodes, payloadNode("n"+string(rune('a'+i%26))+string(rune('a'+i/26)), payloadNow))
	}
	got, drops := validatedPayload(t, p)
	if len(got.Discovery.Nodes) != MaxNodes || len(drops) != 0 {
		t.Errorf("%d nodes, drops %v", len(got.Discovery.Nodes), drops)
	}
}

func TestValidateAcceptsAnEmptyDiscovery(t *testing.T) {
	for _, outcome := range []DiscoveryOutcome{OutcomeNoCandidates, OutcomeStandalone, OutcomePublicSuffix} {
		p := payloadFor(payloadTestKey())
		p.Discovery.Outcome = outcome
		p.Discovery.NextRefreshAfter = payloadNow.Add(MaxRefreshWait)
		got, drops := validatedPayload(t, p)
		if got.Discovery == nil || got.Discovery.Outcome != outcome || len(got.Discovery.Nodes) != 0 || len(drops) != 0 {
			t.Errorf("outcome %s: %+v, drops %v", outcome, got.Discovery, drops)
		}
		if !got.Discovery.NextRefreshAfter.Equal(p.Discovery.NextRefreshAfter) {
			t.Errorf("outcome %s lost its back-off: %v", outcome, got.Discovery.NextRefreshAfter)
		}
	}
}

func TestValidateDropsAmbiguousNamesAlone(t *testing.T) {
	for _, name := range []string{"123", "0x7f000001", "0X7F", "localhost", "LocalHost", "ip6-localhost", "ip6-loopback"} {
		t.Run(name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2", "pve3")
			p.Discovery.Nodes[1] = payloadNode(name, payloadNow)
			p.Discovery.Nodes[1].Address = name
			p.ColdStart = &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}
			got, drops := validatedPayload(t, p)
			if len(got.Discovery.Nodes) != 1 || got.Discovery.Nodes[0].Name != "pve2" {
				t.Errorf("nodes %+v, want only pve2", got.Discovery.Nodes)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropAmbiguousName}}) {
				t.Errorf("drops %v", drops)
			}
			if got.ColdStart == nil || got.Cluster == nil {
				t.Errorf("the rest of the file did not stand: %+v", got)
			}
		})
	}
}

func TestValidateDropsAnAmbiguousHintAlone(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2")
	p.ColdStart = &ColdStartHint{Name: "localhost", ProbedAt: payloadNow}
	got, drops := validatedPayload(t, p)
	if got.ColdStart != nil || len(got.Discovery.Nodes) != 1 {
		t.Errorf("got %+v", got)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "cold_start", Reason: DropAmbiguousName}}) {
		t.Errorf("drops %v", drops)
	}
}

func TestValidateBoundsUnconfirmedNodes(t *testing.T) {
	cases := []struct {
		name      string
		confirmed time.Time
		kept      bool
		reason    DropReason
	}{
		{"confirmed now", payloadNow, true, ""},
		{"23 hours old", payloadNow.Add(-23 * time.Hour), true, ""},
		{"exactly 24 hours old", payloadNow.Add(-NodeTTL), true, ""},
		{"24 hours and a second old", payloadNow.Add(-NodeTTL - time.Second), false, DropExpired},
		{"a week old", payloadNow.Add(-7 * 24 * time.Hour), false, DropExpired},
		{"zero time", time.Time{}, false, DropExpired},
		{"29 seconds ahead", payloadNow.Add(29 * time.Second), true, ""},
		{"30 seconds ahead", payloadNow.Add(FutureSkew), true, ""},
		{"31 seconds ahead", payloadNow.Add(31 * time.Second), false, DropFuture},
		{"a day ahead", payloadNow.Add(24 * time.Hour), false, DropFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2", "pve3")
			p.Discovery.Nodes[0].ConfirmedAt = tc.confirmed
			got, drops := validatedPayload(t, p)
			if tc.kept {
				if len(got.Discovery.Nodes) != 2 || len(drops) != 0 {
					t.Errorf("nodes %+v, drops %v", got.Discovery.Nodes, drops)
				}
				return
			}
			if len(got.Discovery.Nodes) != 1 || got.Discovery.Nodes[0].Name != "pve3" {
				t.Errorf("nodes %+v, want only pve3, dropped and not clamped", got.Discovery.Nodes)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "node 0", Reason: tc.reason}}) {
				t.Errorf("drops %v", drops)
			}
		})
	}
}

func TestValidateDropsMarksThatAreOldAheadOrMalformed(t *testing.T) {
	const seed = "pve1.dc1.example.com:8006"
	cases := []struct {
		name   string
		mark   DownMark
		reason DropReason
	}{
		{"just now", DownMark{DownAt: payloadNow, Class: MarkDial}, ""},
		{"119 seconds old", DownMark{DownAt: payloadNow.Add(-119 * time.Second), Class: MarkDNS}, ""},
		{"two minutes old", DownMark{DownAt: payloadNow.Add(-MarkTTL), Class: MarkDial}, DropExpired},
		{"an hour old", DownMark{DownAt: payloadNow.Add(-time.Hour), Class: MarkPin}, DropExpired},
		{"zero time", DownMark{Class: MarkDial}, DropExpired},
		{"30 seconds ahead", DownMark{DownAt: payloadNow.Add(FutureSkew), Class: MarkTLSVerify}, ""},
		{"31 seconds ahead", DownMark{DownAt: payloadNow.Add(31 * time.Second), Class: MarkDial}, DropFuture},
		{"unknown class", DownMark{DownAt: payloadNow, Class: "dial: connection refused"}, DropInvalid},
		{"empty class", DownMark{DownAt: payloadNow}, DropInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2")
			p.Marks = map[string]DownMark{seed: tc.mark}
			got, drops := validatedPayload(t, p)
			if tc.reason == "" {
				if len(got.Marks) != 1 || len(drops) != 0 {
					t.Errorf("marks %v, drops %v", got.Marks, drops)
				}
				return
			}
			if len(got.Marks) != 0 {
				t.Errorf("mark kept: %v", got.Marks)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "mark", Reason: tc.reason}}) {
				t.Errorf("drops %v", drops)
			}
			if got.Discovery == nil || len(got.Discovery.Nodes) != 1 {
				t.Errorf("the rest of the file did not stand: %+v", got)
			}
		})
	}
}

func TestValidateNormalizesMarkKeys(t *testing.T) {
	p := payloadFor(payloadTestKey())
	older := payloadNow.Add(-30 * time.Second)
	p.Marks = map[string]DownMark{
		"PVE1.dc1.example.com:8006": {DownAt: older, Class: MarkDial},
		"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDNS},
		"not an endpoint":           {DownAt: payloadNow, Class: MarkDial},
		"pve9.dc1.example.com":      {DownAt: payloadNow, Class: MarkDial},
	}
	got, drops := validatedPayload(t, p)
	mark, ok := got.Marks["pve1.dc1.example.com:8006"]
	if len(got.Marks) != 1 || !ok || mark.Class != MarkDNS || !mark.DownAt.Equal(payloadNow) {
		t.Errorf("marks %v, want only the newer mark under the lowercase key", got.Marks)
	}
	if len(drops) != 2 {
		t.Errorf("drops %v, want the two keys that are not endpoints with ports", drops)
	}
}

func TestValidateBoundsTheHint(t *testing.T) {
	cases := []struct {
		name   string
		probed time.Time
		reason DropReason
	}{
		{"probed now", payloadNow, ""},
		{"59 minutes old", payloadNow.Add(-59 * time.Minute), ""},
		{"an hour old", payloadNow.Add(-HintTTL), DropExpired},
		{"a day old", payloadNow.Add(-24 * time.Hour), DropExpired},
		{"30 seconds ahead", payloadNow.Add(FutureSkew), ""},
		{"31 seconds ahead", payloadNow.Add(31 * time.Second), DropFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2")
			p.ColdStart = &ColdStartHint{Name: "pve3", ProbedAt: tc.probed}
			got, drops := validatedPayload(t, p)
			if tc.reason == "" {
				if got.ColdStart == nil || len(drops) != 0 {
					t.Errorf("hint %+v, drops %v", got.ColdStart, drops)
				}
				return
			}
			if got.ColdStart != nil {
				t.Errorf("hint kept: %+v", got.ColdStart)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "cold_start", Reason: tc.reason}}) {
				t.Errorf("drops %v", drops)
			}
			if len(got.Discovery.Nodes) != 1 {
				t.Errorf("the rest of the file did not stand: %+v", got)
			}
		})
	}
}

func TestValidateDropsADiscoveryDatedInTheFuture(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2")
	p.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial}}
	got, drops := validatedPayload(t, p)
	if got.Discovery != nil {
		t.Errorf("a discovery dated an hour ahead was kept: %+v", got.Discovery)
	}
	if got.Cluster == nil || len(got.Marks) != 1 {
		t.Errorf("the rest of the file did not stand: %+v", got)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "discovery", Reason: DropFuture}}) {
		t.Errorf("drops %v", drops)
	}
	p.Discovery.DiscoveredAt = payloadNow.Add(FutureSkew)
	if got, _ := validatedPayload(t, p); got.Discovery == nil {
		t.Error("a discovery within the skew was dropped")
	}
}

func TestValidateDropsARefreshTimeFurtherAheadThanTheBackOff(t *testing.T) {
	cases := []struct {
		name string
		next time.Time
		kept bool
	}{
		{"an hour ahead", payloadNow.Add(time.Hour), true},
		{"the back-off ahead", payloadNow.Add(MaxRefreshWait), true},
		{"the back-off and the skew ahead", payloadNow.Add(MaxRefreshWait + FutureSkew), true},
		{"past the back-off and the skew", payloadNow.Add(MaxRefreshWait + FutureSkew + time.Second), false},
		{"a year ahead", payloadNow.Add(365 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2")
			p.Discovery.NextRefreshAfter = tc.next
			got, drops := validatedPayload(t, p)
			if tc.kept {
				if !got.Discovery.NextRefreshAfter.Equal(tc.next) || len(drops) != 0 {
					t.Errorf("refresh time %v, drops %v", got.Discovery.NextRefreshAfter, drops)
				}
				return
			}
			if !got.Discovery.NextRefreshAfter.IsZero() {
				t.Errorf("refresh time %v was kept or clamped, want it dropped", got.Discovery.NextRefreshAfter)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "next_refresh_after", Reason: DropFuture}}) {
				t.Errorf("drops %v", drops)
			}
			if len(got.Discovery.Nodes) != 1 {
				t.Errorf("the nodes did not stand: %+v", got.Discovery)
			}
		})
	}
}

func TestValidateKeepsAFutureObservationOfTheCluster(t *testing.T) {
	// observed_at gates nothing, and the identity is what keeps a refused
	// change refused, so a skewed time must not make the identity vanish.
	p := payloadFor(payloadTestKey(), "pve2")
	p.Cluster.ObservedAt = payloadNow.Add(time.Hour)
	got, drops := validatedPayload(t, p)
	if got.Cluster == nil || len(got.Discovery.Nodes) != 1 || len(drops) != 0 {
		t.Errorf("cluster %+v, nodes %+v, drops %v", got.Cluster, got.Discovery.Nodes, drops)
	}
}

func TestValidateLoadsNoNodeOfAConflictedIdentity(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2", "pve3")
	p.Cluster.IdentityConflict = true
	p.Cluster.ObservedAt = payloadNow.Add(time.Hour)
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial}}
	got, drops := validatedPayload(t, p)
	if got.Cluster == nil || !got.Cluster.IdentityConflict || got.Cluster.RootCASHA256 != payloadPin("3c") {
		t.Errorf("cluster %+v, want the conflicted identity kept", got.Cluster)
	}
	if got.Discovery == nil || len(got.Discovery.Nodes) != 0 || got.Discovery.Outcome != OutcomeOK {
		t.Errorf("discovery %+v, want the section without its nodes", got.Discovery)
	}
	if len(got.Marks) != 1 {
		t.Errorf("the rest of the file did not stand: %+v", got)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "nodes", Reason: DropIdentityConflict}}) {
		t.Errorf("drops %v", drops)
	}
	// With no usable node left there is nothing to drop and no record of it.
	for i := range p.Discovery.Nodes {
		p.Discovery.Nodes[i].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	}
	if _, drops := validatedPayload(t, p); len(drops) != 2 || drops[0].Reason != DropExpired || drops[1].Reason != DropExpired {
		t.Errorf("expired nodes of a conflicted identity: drops %v", drops)
	}
}

func TestValidateRefusesNodesUnderAnOutcomeThatHoldsNone(t *testing.T) {
	for _, outcome := range []DiscoveryOutcome{OutcomeNoCandidates, OutcomePublicSuffix, OutcomeStandalone} {
		t.Run(string(outcome), func(t *testing.T) {
			p := payloadFor(payloadTestKey(), "pve2")
			p.Discovery.Outcome = outcome
			if _, _, err := Validate(p, p.Key, payloadNow); !errors.Is(err, ErrPayloadInvalid) || !errors.Is(err, ErrFileInvalid) {
				t.Errorf("Validate: %v, want ErrPayloadInvalid", err)
			}
			// An expired node is still a node the outcome says does not exist.
			p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
			if _, _, err := Validate(p, p.Key, payloadNow); !errors.Is(err, ErrPayloadInvalid) {
				t.Errorf("Validate with an expired node: %v, want ErrPayloadInvalid", err)
			}
		})
	}
	for _, outcome := range []DiscoveryOutcome{OutcomeOK, OutcomeNoStatus} {
		p := payloadFor(payloadTestKey(), "pve2")
		p.Discovery.Outcome = outcome
		if got, _ := validatedPayload(t, p); len(got.Discovery.Nodes) != 1 {
			t.Errorf("outcome %s lost its node: %+v", outcome, got.Discovery)
		}
	}
}

func TestValidateNormalizesPinsAndAddresses(t *testing.T) {
	p := payloadFor(payloadTestKey(), "PVE2")
	p.Discovery.Nodes[0].Address = "PVE2.DC1.Example.com"
	p.Discovery.Nodes[0].Pins = []string{strings.ToUpper(payloadPin("ab")), payloadPin("ab"), payloadPin("cd")}
	p.Cluster.RootCASHA256 = strings.ToUpper(payloadPin("3c"))
	got, _ := validatedPayload(t, p)
	node := got.Discovery.Nodes[0]
	if node.Address != "pve2.dc1.example.com" || !reflect.DeepEqual(node.Pins, []string{payloadPin("ab"), payloadPin("cd")}) {
		t.Errorf("node %+v", node)
	}
	if got.Cluster.RootCASHA256 != payloadPin("3c") {
		t.Errorf("digest %s", got.Cluster.RootCASHA256)
	}
}

func TestValidateDropsANodeWhoseAddressTheSeedDoesNotDerive(t *testing.T) {
	cases := []struct {
		name   string
		seed   string
		node   DiscoveredNode
		reason DropReason
	}{
		{"foreign domain", "", DiscoveredNode{Name: "pve2", Address: "pve2.attacker.example", Port: 8006}, DropNotDerived},
		{"another host", "", DiscoveredNode{Name: "pve2", Address: "evil.example.com", Port: 8006}, DropNotDerived},
		{"address that only starts with the name", "", DiscoveredNode{Name: "pve2", Address: "pve2evil.example.com", Port: 8006}, DropNotDerived},
		{"sibling domain", "", DiscoveredNode{Name: "pve2", Address: "pve2.dc2.example.com", Port: 8006}, DropNotDerived},
		{"bare name under a dotted seed", "", DiscoveredNode{Name: "pve2", Address: "pve2", Port: 8006}, DropNotDerived},
		{"IPv6 literal", "", DiscoveredNode{Name: "pve2", Address: "::1", Port: 8006}, DropNotDerived},
		{"port other than the seed's", "", DiscoveredNode{Name: "pve2", Address: "pve2.dc1.example.com", Port: 22}, DropNotDerived},
		{"host that is a public suffix", "pve1.amazonaws.com:8006", DiscoveredNode{Name: "s3", Address: "s3.amazonaws.com", Port: 8006}, DropPublicSuffix},
		{"dotted name under a bare seed", "pve1:8006", DiscoveredNode{Name: "pve2", Address: "pve2.dc1.example.com", Port: 8006}, DropNotDerived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := payloadTestKey()
			if tc.seed != "" {
				key.Endpoints = []string{tc.seed}
			}
			p := payloadFor(key, "pve3")
			p.Cluster.ObservedVia = key.Endpoints[0]
			p.Discovery.Nodes[0].Address = derivedHost(t, key, "pve3")
			node := tc.node
			node.Pins = []string{payloadPin("a1")}
			node.ConfirmedAt = payloadNow
			p.Discovery.Nodes = append(p.Discovery.Nodes, node)
			got, drops := validatedPayload(t, p)
			if len(got.Discovery.Nodes) != 1 || got.Discovery.Nodes[0].Name != "pve3" {
				t.Errorf("nodes %+v, want only pve3", got.Discovery.Nodes)
			}
			if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: tc.reason}}) {
				t.Errorf("drops %v", drops)
			}
			if got.Cluster == nil {
				t.Error("the identity was dropped with the node")
			}
		})
	}
}

// derivedHost returns the host the key's seed derives for name.
func derivedHost(t *testing.T, key Key, name string) string {
	t.Helper()
	address, err := DeriveAddress(key.Endpoints[0], name)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func TestValidateAcceptsTheAddressesTheSeedDerives(t *testing.T) {
	for _, seed := range []string{"pve1:8006", "10.0.0.1:8006", "[2001:db8::1]:8006", "PVE1.DC1.Example.com:9006"} {
		t.Run(seed, func(t *testing.T) {
			key := payloadTestKey()
			key.Endpoints = []string{seed}
			p := payloadFor(key, "PVE2")
			p.Cluster.ObservedVia = ""
			address, err := DeriveAddress(seed, "PVE2")
			if err != nil {
				t.Fatal(err)
			}
			host, port, _ := net.SplitHostPort(address)
			p.Discovery.Nodes[0].Address = strings.ToUpper(host)
			p.Discovery.Nodes[0].Port, _ = strconv.Atoi(port)
			got, drops := validatedPayload(t, p)
			lower := strings.ToLower(host)
			if len(got.Discovery.Nodes) != 1 || len(drops) != 0 || got.Discovery.Nodes[0].Address != lower {
				t.Errorf("nodes %+v, drops %v", got.Discovery.Nodes, drops)
			}
		})
	}
}

func TestValidateDropsEveryNodeWhenTheSeedDerivesNone(t *testing.T) {
	cases := []struct {
		seed   string
		domain string
		reason DropReason
	}{
		{"pve1.co.uk:8006", "co.uk", DropPublicSuffix},
		{"pve1.sch.uk:8006", "sch.uk", DropPublicSuffix},
		{"pve1.dc1.123:8006", "dc1.123", DropNotDerived},
	}
	for _, tc := range cases {
		t.Run(tc.seed, func(t *testing.T) {
			key := payloadTestKey()
			key.Endpoints = []string{tc.seed}
			p := payloadFor(key, "pve2", "pve3")
			p.Cluster.ObservedVia = tc.seed
			for i := range p.Discovery.Nodes {
				p.Discovery.Nodes[i].Address = p.Discovery.Nodes[i].Name + "." + tc.domain
			}
			got, drops := validatedPayload(t, p)
			if len(got.Discovery.Nodes) != 0 || got.Cluster == nil {
				t.Errorf("nodes %+v, cluster %+v, want no node and the identity kept", got.Discovery.Nodes, got.Cluster)
			}
			want := []Drop{{Record: "node 0", Reason: tc.reason}, {Record: "node 1", Reason: tc.reason}}
			if !reflect.DeepEqual(drops, want) {
				t.Errorf("drops %v, want %v", drops, want)
			}
		})
	}
}

func TestValidateObservedViaNamesAConfiguredEndpoint(t *testing.T) {
	key := payloadTestKey()
	key.Endpoints = []string{"pve1.dc1.example.com:8006", "pve9.dc1.example.com:8006"}
	cases := []struct {
		via  string
		want string
		drop bool
	}{
		{"", "", false},
		{"pve1.dc1.example.com:8006", "pve1.dc1.example.com:8006", false},
		{"PVE9.DC1.example.com.:8006", "pve9.dc1.example.com:8006", false},
		{"pve1.dc1.example.com", "pve1.dc1.example.com:8006", false},
		{"evil.example:1", "", true},
		{"pve1.dc1.example.com:8007", "", true},
		{"pve2.dc1.example.com:8006", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.via, func(t *testing.T) {
			p := payloadFor(key, "pve2")
			p.Cluster.ObservedVia = tc.via
			got, drops := validatedPayload(t, p)
			if got.Cluster == nil || got.Cluster.ObservedVia != tc.want || len(got.Discovery.Nodes) != 1 {
				t.Errorf("cluster %+v, nodes %+v, want observed_via %q and the rest kept", got.Cluster, got.Discovery.Nodes, tc.want)
			}
			var want []Drop
			if tc.drop {
				want = []Drop{{Record: "cluster observed_via", Reason: DropNotConfigured}}
			}
			if !reflect.DeepEqual(drops, want) {
				t.Errorf("drops %v, want %v", drops, want)
			}
		})
	}
}

func TestReadStoredKnowsEveryNodeThatPassesTheFormat(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2", "pve3", "pve4", "pve5", "localhost")
	p.Discovery.Nodes[0].Pins = []string{strings.ToUpper(payloadPin("ab"))}
	p.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	p.Discovery.Nodes[2].ConfirmedAt = payloadNow.Add(time.Hour)
	p.Discovery.Nodes[3].Address = "pve5.attacker.example"
	p.Discovery.Nodes[4].Address = "localhost"
	state, _, err := readStored(p, p.Key, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(state.known))
	for _, node := range state.known {
		names = append(names, node.Name)
	}
	if !reflect.DeepEqual(names, []string{"pve2", "pve3", "pve4", "pve5"}) {
		t.Errorf("known %v, want every node but the ambiguous name", names)
	}
	if !reflect.DeepEqual(state.known[0].Pins, []string{payloadPin("ab")}) {
		t.Errorf("known pins %v, want the normalized spelling", state.known[0].Pins)
	}
	if len(state.usable.Discovery.Nodes) != 1 || state.usable.Discovery.Nodes[0].Name != "pve2" {
		t.Errorf("usable %+v, want only pve2", state.usable.Discovery.Nodes)
	}
	if !reflect.DeepEqual(state.discovery, p.Discovery) {
		t.Errorf("stored section %+v, want the section as written", state.discovery)
	}
	// A section dated ahead is left out of use, and its nodes are still known.
	p.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	state, _, err = readStored(p, p.Key, payloadNow)
	if err != nil || state.usable.Discovery != nil || len(state.known) != 4 || !reflect.DeepEqual(state.discovery, p.Discovery) {
		t.Errorf("section ahead: %+v, %v", state, err)
	}
}

func TestValidateAcceptsAHintOnly(t *testing.T) {
	p := &Payload{Key: payloadTestKey(), ColdStart: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}}
	got, drops := validatedPayload(t, p)
	if got.ColdStart == nil || got.Discovery != nil || got.Cluster != nil || len(drops) != 0 {
		t.Errorf("got %+v, drops %v", got, drops)
	}
}

func TestForReaderDropsTheHintAloneWhenTheMapDoesNotNameIt(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2")
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial}}
	p.ColdStart = &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}
	before := p.clone()

	got, drops := ForReader(p, []string{"pve1", "pve2"})
	if got.ColdStart != nil {
		t.Error("the hint stayed although the map does not name its node")
	}
	if got.Cluster == nil || len(got.Discovery.Nodes) != 1 || len(got.Marks) != 1 {
		t.Errorf("the rest of the file did not stand: %+v", got)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "cold_start", Reason: DropNotInAZMap}}) {
		t.Errorf("drops %v", drops)
	}
	if !reflect.DeepEqual(p, before) {
		t.Error("ForReader changed its input")
	}

	if got, drops := ForReader(p, []string{"pve1", "pve3"}); got.ColdStart == nil || len(drops) != 0 {
		t.Errorf("a hint the map names was dropped: %+v, %v", got, drops)
	}
	if got, _ := ForReader(p, nil); got.ColdStart != nil {
		t.Error("an empty map names no hint")
	}
	// The comparison is exact, so a hint matches only a node the map names in
	// the same spelling.
	lower := payloadFor(payloadTestKey(), "pve2")
	lower.ColdStart = &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}
	if got, drops := ForReader(lower, []string{"PVE2"}); got.ColdStart != nil ||
		!reflect.DeepEqual(drops, []Drop{{Record: "cold_start", Reason: DropNotInAZMap}}) {
		t.Errorf("a hint named pve2 under a map naming PVE2: %+v, %v", got.ColdStart, drops)
	}
	noHint := payloadFor(payloadTestKey(), "pve2")
	if got, drops := ForReader(noHint, nil); got != noHint || len(drops) != 0 {
		t.Errorf("a payload with no hint changed: %+v, %v", got, drops)
	}
	if got, drops := ForReader(nil, nil); got != nil || len(drops) != 0 {
		t.Errorf("a nil payload: %+v, %v", got, drops)
	}
}

func TestDropFormatsWithoutValues(t *testing.T) {
	if got := (Drop{Record: "node 2", Reason: DropFuture}).String(); got != "node 2 dropped: future" {
		t.Errorf("String() = %q", got)
	}
}
