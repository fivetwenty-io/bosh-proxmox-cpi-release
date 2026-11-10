package endpointcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// trustSecond is the second configured endpoint of trustKey.
const trustSecond = "pve1b.dc1.example.com:8006"

// trustKey is a two-endpoint key with the given verify_ssl, seeded at
// mergeSeed.
func trustKey(verify bool) Key {
	key := payloadTestKey()
	key.Endpoints = []string{mergeSeed, trustSecond}
	key.VerifySSL = verify
	return key
}

// trustPin is a pin of 32 copies of b, which is payloadPin of b in hex.
func trustPin(b byte) [sha256.Size]byte {
	var pin [sha256.Size]byte
	for i := range pin {
		pin[i] = b
	}
	return pin
}

// trustRoot is the identity payloadCluster stores.
var trustRoot = trustPin(0x3c)

// trustNode is an observed node named name with the given pins, at the
// address the seed derives for it.
func trustNode(name string, pins ...byte) ObservedNode {
	node := ObservedNode{Name: name, Address: name + ".dc1.example.com:8006", Online: true}
	for _, b := range pins {
		node.Pins = append(node.Pins, trustPin(b))
	}
	return node
}

// trustSame is an observed node with the pins payloadNode stores.
func trustSame(name string) ObservedNode { return trustNode(name, 0xa1, 0x9e) }

// trustObservation is an ok, quorate observation of nodes through mergeSeed
// under the stored identity.
func trustObservation(nodes ...ObservedNode) Observation {
	return Observation{Via: mergeSeed, RootCA: trustRoot, Outcome: OutcomeOK, Quorate: true, Nodes: nodes}
}

// trustView reads stored as UpdatePayload does and returns the view it hands
// the build function.
func trustView(stored *Payload, key Key) Stored {
	state, _ := readState(stored, key, payloadNow)
	return state.view()
}

// trustStored is one stored file the trust tests decide against.
type trustStored struct {
	name string
	make func(key Key) *Payload
}

// trustStoredFiles are the stored states every combination runs against.
func trustStoredFiles() []trustStored {
	return []trustStored{
		{"no file", func(Key) *Payload { return nil }},
		{"marks only", func(key Key) *Payload {
			return &Payload{Key: key, Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}}
		}},
		{"an invalid file", func(key Key) *Payload {
			return &Payload{Key: key, Cluster: &ClusterIdentity{RootCASHA256: "x"}}
		}},
		{"identity only", func(key Key) *Payload {
			return &Payload{Key: key, Cluster: ptr(payloadCluster(payloadNow.Add(-time.Hour)))}
		}},
		{"fresh nodes", func(key Key) *Payload { return payloadFor(key, "pve2", "pve3") }},
		{"a node past 24 hours", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
			return p
		}},
		{"a section dated ahead", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
			return p
		}},
		{"a node in another case", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[0].Name = "PVE2"
			return p
		}},
		{"a node that no longer derives", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[0].Address = "pve2.attacker.example"
			return p
		}},
		{"a no_candidates file", func(key Key) *Payload {
			p := payloadFor(key)
			p.Discovery.Outcome = OutcomeNoCandidates
			p.Discovery.Nodes = []DiscoveredNode{}
			return p
		}},
		{"a recorded conflict", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Cluster.IdentityConflict = true
			return p
		}},
		{"an identity seen ahead", func(key Key) *Payload {
			p := payloadFor(key, "pve2")
			p.Cluster.ObservedAt = payloadNow.Add(time.Hour)
			return p
		}},
	}
}

// trustObserved is one observation the trust tests decide.
type trustObserved struct {
	name string
	obs  Observation
}

// trustObservations are the observations every combination runs.
func trustObservations() []trustObserved {
	changed := func(nodes ...ObservedNode) Observation {
		obs := trustObservation(nodes...)
		obs.RootCA = trustPin(0xff)
		return obs
	}
	noNodes := func(outcome DiscoveryOutcome) Observation {
		obs := trustObservation()
		obs.Outcome = outcome
		obs.Quorate = false
		return obs
	}
	via := trustObservation(trustSame("pve2"), trustSame("pve3"))
	via.Via = "PVE1B.dc1.example.com"
	notDerived := trustObservation(trustNode("pve2", 0x55), trustSame("pve3"))
	notDerived.Nodes[0].Address = "pve2.attacker.example:8006"
	return []trustObserved{
		{"the same nodes and pins", trustObservation(trustSame("pve2"), trustSame("pve3"))},
		{"a new node", trustObservation(trustSame("pve2"), trustSame("pve3"), trustNode("pve4", 0x44))},
		{"a node left out", trustObservation(trustSame("pve3"))},
		{"the same pins reordered and repeated", trustObservation(trustNode("pve2", 0x9e, 0xa1, 0x9e), trustSame("pve3"))},
		{"a changed pin set", trustObservation(trustNode("pve2", 0xa1, 0x55), trustSame("pve3"))},
		{"a pin set that only grows", trustObservation(trustNode("pve2", 0xa1, 0x9e, 0x55), trustSame("pve3"))},
		{"a changed identity", changed(trustSame("pve2"), trustSame("pve3"))},
		{"a changed identity and pins", changed(trustNode("pve2", 0x55), trustSame("pve3"))},
		{"no_candidates", noNodes(OutcomeNoCandidates)},
		{"standalone", noNodes(OutcomeStandalone)},
		{"public_suffix", noNodes(OutcomePublicSuffix)},
		{"no_status", func() Observation {
			obs := trustObservation(trustSame("pve2"), trustSame("pve3"))
			obs.Outcome, obs.Quorate = OutcomeNoStatus, false
			return obs
		}()},
		{"through the second configured endpoint", via},
		{"an ambiguous node", trustObservation(trustSame("pve2"), trustNode("0x7f000001", 0x55))},
		{"a node the seed does not derive", notDerived},
	}
}

// TestDecideTrustAgreesWithMerge runs every stored file and observation under
// both verify_ssl values through DecideTrust, and every decision through
// Merge. Merge never refuses a change trust accepted. A result trust refused
// for a trust rule is one Merge refuses too, for the same reason and the same
// nodes.
func TestDecideTrustAgreesWithMerge(t *testing.T) {
	decided := map[TrustOutcome]int{}
	for _, verify := range []bool{true, false} {
		key := trustKey(verify)
		for _, file := range trustStoredFiles() {
			for _, observed := range trustObservations() {
				name := fmt.Sprintf("verify_ssl %v/%s/%s", verify, file.name, observed.name)
				t.Run(name, func(t *testing.T) {
					stored := file.make(key)
					decision, err := DecideTrust(trustView(stored, key), key, observed.obs, payloadNow)
					if err != nil {
						t.Fatalf("DecideTrust: %v", err)
					}
					decided[decision.Outcome]++
					checkAgreement(t, stored, key, observed.obs, decision)
				})
			}
		}
	}
	// Every outcome but pins_replaced under verify_ssl false shows up, so the
	// matrix exercises each rule.
	for _, outcome := range []TrustOutcome{
		TrustFirstUse, TrustConfirmed, TrustIdentityReplaced, TrustIdentityRefused,
		TrustPinsReplaced, TrustPinsRefused, TrustConflictStands,
	} {
		if decided[outcome] == 0 {
			t.Errorf("no combination decided %s", outcome)
		}
	}
}

// checkAgreement runs decision through Merge against stored and checks that
// the two agree.
func checkAgreement(t *testing.T, stored *Payload, key Key, obs Observation, decision TrustDecision) {
	t.Helper()
	if decision.Writes() {
		next, _, err := Merge(stored, key, decision.Change, payloadNow)
		if err != nil || next == nil {
			t.Fatalf("Merge refused an accepted %s decision: %+v, %v", decision.Outcome, next, err)
		}
		if decision.Change.Result != nil {
			via := next.Cluster.ObservedVia
			if via != "" && via != mergeSeed && via != trustSecond {
				t.Errorf("observed_via %q is not a configured endpoint", via)
			}
		}
		return
	}
	_, raw, _, err := observedResult(obs, key, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	cluster := ClusterIdentity{RootCASHA256: decision.ObservedRootCA, ObservedVia: mergeSeed, ObservedAt: payloadNow}
	_, _, err = Merge(stored, key, Change{Result: &DiscoveryResult{Cluster: cluster, Discovery: raw}}, payloadNow)
	var refusal *TrustRefusalError
	switch decision.Outcome {
	case TrustConflictStands:
		if !errors.Is(err, ErrIdentityConflict) {
			t.Errorf("trust kept a conflict, and Merge said %v", err)
		}
	case TrustPinsRefused:
		if !errors.As(err, &refusal) || refusal.IdentityChanged || !reflect.DeepEqual(refusal.Nodes, decision.PinsChanged) {
			t.Errorf("trust refused pins for %v, and Merge said %v", decision.PinsChanged, err)
		}
	case TrustIdentityRefused:
		if !errors.As(err, &refusal) || !refusal.IdentityChanged {
			t.Errorf("trust refused the identity, and Merge said %v", err)
		}
	default:
		t.Errorf("decision %s writes nothing", decision.Outcome)
	}
}

// trustCase is one row of the trust table.
type trustCase struct {
	name    string
	verify  bool
	stored  func(key Key) *Payload
	obs     Observation
	outcome TrustOutcome
	log     TrustLog
	drop    bool
	changed []string
	// check inspects the merged file of a decision that writes.
	check func(t *testing.T, next *Payload)
}

// trustNodeNames returns the names of p's discovered nodes in order.
func trustNodeNames(p *Payload) []string {
	if p.Discovery == nil {
		return nil
	}
	names := make([]string, 0, len(p.Discovery.Nodes))
	for _, node := range p.Discovery.Nodes {
		names = append(names, node.Name)
	}
	return names
}

// wantNodes checks the merged file's node names.
func wantNodes(names ...string) func(t *testing.T, next *Payload) {
	return func(t *testing.T, next *Payload) {
		t.Helper()
		if got := trustNodeNames(next); !reflect.DeepEqual(got, names) && (len(got) != 0 || len(names) != 0) {
			t.Errorf("nodes %v, want %v", got, names)
		}
	}
}

// nodePins returns the pins of the node named name in p.
func nodePins(p *Payload, name string) []string {
	for _, node := range p.Discovery.Nodes {
		if node.Name == name {
			return node.Pins
		}
	}
	return nil
}

func trustCases() []trustCase {
	fresh := func(key Key) *Payload { return payloadFor(key, "pve2", "pve3") }
	expired := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		return p
	}
	conflict := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Cluster.IdentityConflict = true
		return p
	}
	same := trustObservation(trustSame("pve2"), trustSame("pve3"))
	newNode := trustObservation(trustSame("pve2"), trustSame("pve3"), trustNode("pve4", 0x44))
	pinChange := trustObservation(trustNode("pve2", 0xa1, 0x55), trustSame("pve3"), trustNode("pve4", 0x44))
	identity := trustObservation(trustNode("pve2", 0x55), trustNode("pve5", 0x66))
	identity.RootCA = trustPin(0xff)
	keepsIdentity := func(t *testing.T, next *Payload) {
		t.Helper()
		if next.Cluster.RootCASHA256 != payloadPin("3c") || !next.Cluster.ObservedAt.Equal(payloadNow.Add(-5*time.Minute)) {
			t.Errorf("identity %+v, want the stored one with its first sighting", next.Cluster)
		}
	}
	return []trustCase{
		{
			name: "verify_ssl false, first use records the identity and pins", stored: func(Key) *Payload { return nil },
			obs: same, outcome: TrustFirstUse,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				wantNodes("pve2", "pve3")(t, next)
				if next.Cluster.RootCASHA256 != payloadPin("3c") || !next.Cluster.ObservedAt.Equal(payloadNow) ||
					next.Cluster.ObservedVia != mergeSeed {
					t.Errorf("identity %+v", next.Cluster)
				}
			},
		},
		{
			name: "verify_ssl true, first use records the identity and pins", verify: true,
			stored: func(Key) *Payload { return nil }, obs: same, outcome: TrustFirstUse, check: wantNodes("pve2", "pve3"),
		},
		{
			name: "verify_ssl false, the same identity and pins confirm the nodes", stored: fresh, obs: same,
			outcome: TrustConfirmed,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				keepsIdentity(t, next)
				for _, node := range next.Discovery.Nodes {
					if !node.ConfirmedAt.Equal(payloadNow) {
						t.Errorf("node %s confirmed at %v, want now", node.Name, node.ConfirmedAt)
					}
				}
			},
		},
		{
			name: "verify_ssl false, a new node is added on first use", stored: fresh, obs: newNode,
			outcome: TrustConfirmed,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				wantNodes("pve2", "pve3", "pve4")(t, next)
				if !reflect.DeepEqual(nodePins(next, "pve4"), []string{payloadPin("44")}) {
					t.Errorf("pve4 pins %v", nodePins(next, "pve4"))
				}
			},
		},
		{
			name: "verify_ssl false, a node left out stops being known", stored: fresh,
			obs: trustObservation(trustSame("pve3")), outcome: TrustConfirmed, check: wantNodes("pve3"),
		},
		{
			name: "verify_ssl false, a changed pin set is refused", stored: fresh, obs: pinChange,
			outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
		},
		{
			name: "verify_ssl false, a changed pin set is refused for a node past 24 hours", stored: expired,
			obs: pinChange, outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
		},
		{
			name: "verify_ssl false, a node past 24 hours with the same pins is confirmed again", stored: expired,
			obs: same, outcome: TrustConfirmed,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				if !next.Discovery.Nodes[0].ConfirmedAt.Equal(payloadNow) {
					t.Errorf("pve2 confirmed at %v", next.Discovery.Nodes[0].ConfirmedAt)
				}
			},
		},
		{
			name: "verify_ssl false, a changed identity is recorded as a conflict", stored: fresh, obs: identity,
			outcome: TrustIdentityRefused, log: TrustLogError, drop: true,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				want := payloadFor(trustKey(false), "pve2", "pve3")
				want.Cluster.IdentityConflict = true
				if !reflect.DeepEqual(next, want) {
					t.Errorf("the conflict changed more than the flag:\n got %+v\nwant %+v", next, want)
				}
			},
		},
		{
			name: "verify_ssl false, a recorded conflict refuses the stored identity too", stored: conflict, obs: same,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true,
		},
		{
			name: "verify_ssl false, a recorded conflict refuses another identity", stored: conflict, obs: identity,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true,
		},
		{
			name: "verify_ssl true, a changed identity replaces the nodes and pins", verify: true, stored: fresh,
			obs: identity, outcome: TrustIdentityReplaced, log: TrustLogError, drop: true,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				wantNodes("pve2", "pve5")(t, next)
				if next.Cluster.RootCASHA256 != payloadPin("ff") || !next.Cluster.ObservedAt.Equal(payloadNow) {
					t.Errorf("identity %+v", next.Cluster)
				}
				if !reflect.DeepEqual(nodePins(next, "pve2"), []string{payloadPin("55")}) {
					t.Errorf("pve2 pins %v", nodePins(next, "pve2"))
				}
			},
		},
		{
			name: "verify_ssl true, a changed pin set replaces the old one", verify: true, stored: expired,
			obs: pinChange, outcome: TrustPinsReplaced, changed: []string{"pve2"},
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				keepsIdentity(t, next)
				wantNodes("pve2", "pve3", "pve4")(t, next)
				if !reflect.DeepEqual(nodePins(next, "pve2"), []string{payloadPin("a1"), payloadPin("55")}) {
					t.Errorf("pve2 pins %v", nodePins(next, "pve2"))
				}
			},
		},
		{
			name: "verify_ssl true, a recorded conflict still refuses", verify: true, stored: conflict, obs: same,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true,
		},
	}
}

// TestDecideTrustRules checks each trust rule's outcome, log level, and the
// file it writes.
func TestDecideTrustRules(t *testing.T) {
	for _, tc := range trustCases() {
		t.Run(tc.name, func(t *testing.T) {
			key := trustKey(tc.verify)
			stored := tc.stored(key)
			decision, err := DecideTrust(trustView(stored, key), key, tc.obs, payloadNow)
			if err != nil {
				t.Fatalf("DecideTrust: %v", err)
			}
			if decision.Outcome != tc.outcome || decision.Log != tc.log || decision.DropDiscovered != tc.drop ||
				!reflect.DeepEqual(decision.PinsChanged, tc.changed) {
				t.Fatalf("decision %s log %d drop %v changed %v, want %s log %d drop %v changed %v",
					decision.Outcome, decision.Log, decision.DropDiscovered, decision.PinsChanged,
					tc.outcome, tc.log, tc.drop, tc.changed)
			}
			if decision.ObservedRootCA != hex.EncodeToString(tc.obs.RootCA[:]) {
				t.Errorf("observed root %q", decision.ObservedRootCA)
			}
			if stored != nil && decision.StoredRootCA != payloadPin("3c") {
				t.Errorf("stored root %q", decision.StoredRootCA)
			}
			if tc.check == nil {
				if decision.Writes() {
					t.Fatalf("a refused decision writes %+v", decision.Change)
				}
				return
			}
			next, _, err := Merge(stored, key, decision.Change, payloadNow)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			tc.check(t, next)
		})
	}
}

// TestDecideTrustSchedulesTheNextRefresh checks that a discovery that found
// nodes is due again in an hour and one that found none waits 24 hours, and
// that a decision stamps the discovery and its nodes with the clock.
func TestDecideTrustSchedulesTheNextRefresh(t *testing.T) {
	key := trustKey(false)
	cases := map[DiscoveryOutcome]time.Duration{
		OutcomeOK: time.Hour, OutcomeNoStatus: time.Hour,
		OutcomeNoCandidates: 24 * time.Hour, OutcomeStandalone: 24 * time.Hour, OutcomePublicSuffix: 24 * time.Hour,
	}
	for outcome, wait := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			obs := trustObservation()
			if outcome.holdsNodes() {
				obs = trustObservation(trustSame("pve2"))
			}
			obs.Outcome = outcome
			decision, err := DecideTrust(Stored{}, key, obs, payloadNow)
			if err != nil {
				t.Fatal(err)
			}
			section := decision.Change.Result.Discovery
			if !section.DiscoveredAt.Equal(payloadNow) || !section.NextRefreshAfter.Equal(payloadNow.Add(wait)) {
				t.Errorf("discovered %v, next refresh %v, want now and now+%v", section.DiscoveredAt, section.NextRefreshAfter, wait)
			}
			for _, node := range section.Nodes {
				if !node.ConfirmedAt.Equal(payloadNow) {
					t.Errorf("node confirmed at %v", node.ConfirmedAt)
				}
			}
		})
	}
	if RefreshInterval != time.Hour {
		t.Errorf("RefreshInterval is %v, want an hour", RefreshInterval)
	}
}

// TestDecideTrustOnlyAConfiguredEndpointWritesTrust shows that an observation
// that did not come through one of the key's endpoints changes nothing,
// whatever the stored state, and that one through any configured endpoint, in
// any spelling, is judged.
func TestDecideTrustOnlyAConfiguredEndpointWritesTrust(t *testing.T) {
	refused := []string{
		"", "   ", "pve2.dc1.example.com:8006", "pve1.dc1.example.com:8007", "pve1.other.example:8006",
		"10.0.0.2:8006", "not an endpoint", "https://pve1.dc1.example.com:8006",
	}
	for _, verify := range []bool{true, false} {
		key := trustKey(verify)
		for _, file := range trustStoredFiles() {
			for _, via := range refused {
				obs := trustObservation(trustNode("pve2", 0x55), trustNode("pve9", 0x99))
				obs.Via = via
				decision, err := DecideTrust(trustView(file.make(key), key), key, obs, payloadNow)
				if !errors.Is(err, ErrUnconfiguredSource) || !errors.Is(err, ErrInvalidChange) || decision.Writes() ||
					decision.Outcome != "" {
					t.Errorf("verify_ssl %v, %s, via %q: %+v, %v, want ErrUnconfiguredSource", verify, file.name, via, decision, err)
				}
			}
		}
	}
	accepted := map[string]string{
		mergeSeed:                    mergeSeed,
		trustSecond:                  trustSecond,
		"PVE1.DC1.example.com.":      mergeSeed,
		"pve1b.dc1.example.com":      trustSecond,
		"Pve1B.Dc1.Example.Com:8006": trustSecond,
	}
	for via, normalized := range accepted {
		obs := trustObservation(trustSame("pve2"))
		obs.Via = via
		decision, err := DecideTrust(Stored{}, trustKey(false), obs, payloadNow)
		if err != nil || decision.Outcome != TrustFirstUse || decision.Change.Result.Cluster.ObservedVia != normalized {
			t.Errorf("via %q: %+v, %v, want first use through %s", via, decision, err, normalized)
		}
	}
	if strings.Contains(ErrUnconfiguredSource.Error(), "pve") {
		t.Errorf("%v names a host", ErrUnconfiguredSource)
	}
}

// TestDecideTrustRefusesWhatMergeCannotWrite shows that an observation Merge
// would refuse, a stored view of another key, and a zero clock are refused
// with ErrInvalidChange and decide nothing.
func TestDecideTrustRefusesWhatMergeCannotWrite(t *testing.T) {
	key := trustKey(false)
	tooMany := trustObservation()
	for i := range MaxNodes + 1 {
		tooMany.Nodes = append(tooMany.Nodes, trustNode("n"+strconv.Itoa(i), 0x11))
	}
	with := func(mutate func(*Observation)) Observation {
		obs := trustObservation(trustSame("pve2"), trustSame("pve3"))
		mutate(&obs)
		return obs
	}
	cases := map[string]struct {
		stored    Stored
		obs       Observation
		zeroClock bool
	}{
		"a zero identity":             {obs: with(func(o *Observation) { o.RootCA = [sha256.Size]byte{} })},
		"a name that fails pve-node":  {obs: with(func(o *Observation) { o.Nodes[0].Name = "pve_2" })},
		"a dotted name":               {obs: with(func(o *Observation) { o.Nodes[0].Name = "api.attacker.example" })},
		"an unknown outcome":          {obs: with(func(o *Observation) { o.Outcome = "error" })},
		"nodes under no_candidates":   {obs: with(func(o *Observation) { o.Outcome = OutcomeNoCandidates })},
		"nodes under standalone":      {obs: with(func(o *Observation) { o.Outcome = OutcomeStandalone })},
		"nodes under public_suffix":   {obs: with(func(o *Observation) { o.Outcome = OutcomePublicSuffix })},
		"a repeated name":             {obs: with(func(o *Observation) { o.Nodes[1].Name = "PVE2" })},
		"more than 64 nodes":          {obs: tooMany},
		"a node without pins":         {obs: with(func(o *Observation) { o.Nodes[0].Pins = nil })},
		"a zero pin":                  {obs: with(func(o *Observation) { o.Nodes[0].Pins[1] = [sha256.Size]byte{} })},
		"four pins":                   {obs: with(func(o *Observation) { o.Nodes[0] = trustNode("pve2", 1, 2, 3, 4) })},
		"an address without a port":   {obs: with(func(o *Observation) { o.Nodes[0].Address = "pve2.dc1.example.com" })},
		"a port that is not a number": {obs: with(func(o *Observation) { o.Nodes[0].Address = "pve2.dc1.example.com:https" })},
		"a port out of range":         {obs: with(func(o *Observation) { o.Nodes[0].Address = "pve2.dc1.example.com:70000" })},
		"an address off the grammar":  {obs: with(func(o *Observation) { o.Nodes[0].Address = "pve2/x:8006" })},
		"a zero clock":                {obs: trustObservation(trustSame("pve2")), zeroClock: true},
		"a stored view of another key": {
			stored: trustView(payloadFor(trustKey(true), "pve2"), trustKey(true)),
			obs:    trustObservation(trustSame("pve2")),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			now := payloadNow
			if tc.zeroClock {
				now = time.Time{}
			}
			decision, err := DecideTrust(tc.stored, key, tc.obs, now)
			if !errors.Is(err, ErrInvalidChange) || errors.Is(err, ErrUnconfiguredSource) || decision.Writes() ||
				decision.Outcome != "" {
				t.Errorf("DecideTrust: %+v, %v, want ErrInvalidChange", decision, err)
			}
			if strings.Contains(err.Error(), "attacker") {
				t.Errorf("error %v echoes a value", err)
			}
		})
	}
}

// TestDecideTrustLeavesOutNodesMergeLeavesOut shows that an ambiguous name and
// an address the seed does not derive are left out, as Merge leaves them out,
// so their pins are not compared and Merge reports the drop.
func TestDecideTrustLeavesOutNodesMergeLeavesOut(t *testing.T) {
	key := trustKey(false)
	stored := payloadFor(key, "pve2", "pve3")
	obs := trustObservation(trustNode("pve2", 0x55), trustSame("pve3"), trustNode("localhost", 0x77))
	obs.Nodes[0].Address = "pve2.attacker.example:8006"
	decision, err := DecideTrust(trustView(stored, key), key, obs, payloadNow)
	if err != nil || decision.Outcome != TrustConfirmed || len(decision.PinsChanged) != 0 {
		t.Fatalf("DecideTrust: %+v, %v", decision, err)
	}
	next, drops, err := Merge(stored, key, decision.Change, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	wantNodes("pve3")(t, next)
	want := []Drop{{Record: "node 0", Reason: DropNotDerived}, {Record: "node 2", Reason: DropAmbiguousName}}
	if !reflect.DeepEqual(drops, want) {
		t.Errorf("drops %v, want %v", drops, want)
	}
}

func TestTrustLogLevel(t *testing.T) {
	cases := map[TrustLog]struct {
		level slog.Level
		logs  bool
	}{
		TrustLogNone:  {slog.LevelDebug, false},
		TrustLogWarn:  {slog.LevelWarn, true},
		TrustLogError: {slog.LevelError, true},
		TrustLog(9):   {slog.LevelDebug, false},
	}
	for l, want := range cases {
		if level, logs := l.Level(); level != want.level || logs != want.logs {
			t.Errorf("TrustLog(%d).Level() = %v, %v, want %v, %v", l, level, logs, want.level, want.logs)
		}
	}
}

func TestTrustDecisionWrites(t *testing.T) {
	if (TrustDecision{}).Writes() {
		t.Error("an empty decision writes")
	}
	if !(TrustDecision{Change: Change{Conflict: true}}).Writes() {
		t.Error("a conflict does not write")
	}
	if !(TrustDecision{Change: Change{Result: &DiscoveryResult{}}}).Writes() {
		t.Error("a result does not write")
	}
	if (TrustDecision{Change: Change{Marks: map[string]DownMark{mergeSeed: {}}}}).Writes() {
		t.Error("marks alone count as a trust write")
	}
}

// trustUpdate decides obs through UpdatePayload at now, as the discovery
// worker does, and returns the decision.
func trustUpdate(t *testing.T, s *Store, now time.Time, obs Observation) TrustDecision {
	t.Helper()
	var decision TrustDecision
	result, err := s.UpdatePayload(context.Background(), now, func(stored Stored) (Change, error) {
		var err error
		decision, err = DecideTrust(stored, s.Key(), obs, now)
		return decision.Change, err
	})
	if err != nil || !result.Locked {
		t.Fatalf("UpdatePayload: %+v, %v", result, err)
	}
	// A decision that records nothing still lets the merge write the stored
	// file back as it was read, so only a recording decision must write.
	if decision.Writes() && !result.Written {
		t.Fatalf("%s decision: nothing written", decision.Outcome)
	}
	return decision
}

// TestStoreTrustOnFirstUseLifecycle walks a verify_ssl false file through the
// rules over two days through the store, as the discovery worker drives them.
func TestStoreTrustOnFirstUseLifecycle(t *testing.T) {
	dir := newStoreDir(t)
	s := openStoreFor(t, dir, KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	read := func(now time.Time) *Payload {
		t.Helper()
		p, _, err := s.ReadPayload(now, nil)
		if err != nil {
			t.Fatalf("ReadPayload: %v", err)
		}
		return p
	}
	start := payloadNow
	if d := trustUpdate(t, s, start, trustObservation(trustSame("pve2"), trustSame("pve3"))); d.Outcome != TrustFirstUse {
		t.Fatalf("first discovery: %s", d.Outcome)
	}
	if got := trustNodeNames(read(start)); !reflect.DeepEqual(got, []string{"pve2", "pve3"}) {
		t.Fatalf("nodes after first use %v", got)
	}

	// A day later nothing is loaded, but the nodes are still known, so their
	// pins still bind.
	later := start.Add(25 * time.Hour)
	if got := trustNodeNames(read(later)); len(got) != 0 {
		t.Fatalf("unconfirmed nodes loaded after 24 hours: %v", got)
	}
	before := readStoreFile(t, filepath.Join(dir, s.FileName()))
	d := trustUpdate(t, s, later, trustObservation(trustNode("pve2", 0x55), trustSame("pve3")))
	if d.Outcome != TrustPinsRefused || d.Log != TrustLogWarn || !reflect.DeepEqual(d.PinsChanged, []string{"pve2"}) {
		t.Fatalf("pin change past 24 hours: %+v", d)
	}
	if after := readStoreFile(t, filepath.Join(dir, s.FileName())); string(after) != string(before) {
		t.Fatal("a refused pin change rewrote the file")
	}

	// The same pins confirm both nodes again.
	if d := trustUpdate(t, s, later, trustObservation(trustSame("pve2"), trustSame("pve3"))); d.Outcome != TrustConfirmed {
		t.Fatalf("reconfirmation: %s", d.Outcome)
	}
	if got := trustNodeNames(read(later)); !reflect.DeepEqual(got, []string{"pve2", "pve3"}) {
		t.Fatalf("nodes after reconfirmation %v", got)
	}

	// A discovery that leaves pve3 out forgets it, and pve3 comes back on
	// first use with new pins.
	if d := trustUpdate(t, s, later, trustObservation(trustSame("pve2"))); d.Outcome != TrustConfirmed {
		t.Fatalf("pve3 left out: %s", d.Outcome)
	}
	d = trustUpdate(t, s, later, trustObservation(trustSame("pve2"), trustNode("pve3", 0x33)))
	if d.Outcome != TrustConfirmed || len(d.PinsChanged) != 0 {
		t.Fatalf("pve3 back: %+v", d)
	}
	if p := read(later); !reflect.DeepEqual(nodePins(p, "pve3"), []string{payloadPin("33")}) {
		t.Fatalf("pve3 pins %v", nodePins(p, "pve3"))
	}

	// A changed identity records the conflict, the reader loads no node,
	// and every later observation is refused.
	other := trustObservation(trustSame("pve2"))
	other.RootCA = trustPin(0xff)
	d = trustUpdate(t, s, later, other)
	if d.Outcome != TrustIdentityRefused || d.Log != TrustLogError || !d.DropDiscovered ||
		d.StoredRootCA != payloadPin("3c") || d.ObservedRootCA != payloadPin("ff") {
		t.Fatalf("identity change: %+v", d)
	}
	p := read(later)
	if !p.Cluster.IdentityConflict || p.Cluster.RootCASHA256 != payloadPin("3c") || len(p.Discovery.Nodes) != 0 {
		t.Fatalf("after the conflict: %+v %+v", p.Cluster, p.Discovery)
	}
	for _, obs := range []Observation{other, trustObservation(trustSame("pve2"))} {
		if d := trustUpdate(t, s, later.Add(time.Hour), obs); d.Outcome != TrustConflictStands || d.Log != TrustLogWarn {
			t.Fatalf("after the conflict: %+v", d)
		}
	}
}

// TestStoreVerifiedTrustLifecycle walks a verify_ssl true file through an
// identity change and a pin change through the store.
func TestStoreVerifiedTrustLifecycle(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: true, User: storeTestUser,
	})
	trustUpdate(t, s, payloadNow, trustObservation(trustSame("pve2"), trustSame("pve3")))

	other := trustObservation(trustNode("pve7", 0x77))
	other.RootCA, other.Via = trustPin(0xff), trustSecond
	d := trustUpdate(t, s, payloadNow.Add(time.Hour), other)
	if d.Outcome != TrustIdentityReplaced || d.Log != TrustLogError || !d.DropDiscovered {
		t.Fatalf("identity change: %+v", d)
	}
	p, _, err := s.ReadPayload(payloadNow.Add(time.Hour), nil)
	if err != nil || p.Cluster.RootCASHA256 != payloadPin("ff") || p.Cluster.ObservedVia != trustSecond ||
		!reflect.DeepEqual(trustNodeNames(p), []string{"pve7"}) {
		t.Fatalf("after the identity change: %+v, %v", p, err)
	}
	// The next discovery under the new identity is no change, so the Error
	// line is logged once.
	again := trustObservation(trustNode("pve7", 0x78))
	again.RootCA = trustPin(0xff)
	d = trustUpdate(t, s, payloadNow.Add(2*time.Hour), again)
	if d.Outcome != TrustPinsReplaced || d.Log != TrustLogNone || !reflect.DeepEqual(d.PinsChanged, []string{"pve7"}) {
		t.Fatalf("pin change: %+v", d)
	}
	p, _, err = s.ReadPayload(payloadNow.Add(2*time.Hour), nil)
	if err != nil || !reflect.DeepEqual(nodePins(p, "pve7"), []string{payloadPin("78")}) ||
		!p.Cluster.ObservedAt.Equal(payloadNow.Add(time.Hour)) || p.Cluster.ObservedVia != trustSecond {
		t.Fatalf("after the pin change: %+v, %v", p, err)
	}
}
