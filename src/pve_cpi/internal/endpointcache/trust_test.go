package endpointcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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
	return state.view(payloadNow)
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
		{"a second node past 24 hours", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
			return p
		}},
		{"a node in another case past 24 hours", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[0].Name = "PVE2"
			p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
			return p
		}},
		{"a node with one pin", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Discovery.Nodes[0].Pins = []string{payloadPin("a1")}
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
		{"a recorded conflict and a section dated ahead", func(key Key) *Payload {
			p := payloadFor(key, "pve2", "pve3")
			p.Cluster.IdentityConflict = true
			p.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
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
	changedVia := changed(trustSame("pve2"), trustSame("pve3"))
	changedVia.Via = trustSecond
	notDerived := trustObservation(trustNode("pve2", 0x55), trustSame("pve3"))
	notDerived.Nodes[0].Address = "pve2.attacker.example:8006"
	noStatus := trustObservation(trustSame("pve2"), trustSame("pve3"))
	noStatus.Outcome, noStatus.Quorate = OutcomeNoStatus, false
	noStatus.Nodes[1].Online = false
	return []trustObserved{
		{"the same nodes and pins", trustObservation(trustSame("pve2"), trustSame("pve3"))},
		{"a new node", trustObservation(trustSame("pve2"), trustSame("pve3"), trustNode("pve4", 0x44))},
		{"a node left out", trustObservation(trustSame("pve3"))},
		{"the same pins reordered and repeated", trustObservation(trustNode("pve2", 0x9e, 0xa1, 0x9e), trustSame("pve3"))},
		{"a partially overlapping pin set", trustObservation(trustNode("pve2", 0xa1, 0x55), trustSame("pve3"))},
		{"an overlap with the shared pin last", trustObservation(trustNode("pve2", 0x55, 0xa1), trustSame("pve3"))},
		{"an overlap with the other stored pin last", trustObservation(trustNode("pve2", 0x55, 0x9e), trustSame("pve3"))},
		{"an overlap of three pins, shared pin first", trustObservation(trustNode("pve2", 0xa1, 0x55, 0x56), trustSame("pve3"))},
		{"an overlap of three pins, shared pin second", trustObservation(trustNode("pve2", 0x55, 0xa1, 0x56), trustSame("pve3"))},
		{"an overlap of three pins, shared pin third", trustObservation(trustNode("pve2", 0x55, 0x56, 0xa1), trustSame("pve3"))},
		// Against the stored node with one pin this set grows by two pins.
		{"a pin set that only grows", trustObservation(trustNode("pve2", 0xa1, 0x9e, 0x55), trustSame("pve3"))},
		{"a pin set that shrinks", trustObservation(trustNode("pve2", 0xa1), trustSame("pve3"))},
		{"a disjoint pin set", trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3"))},
		{"a disjoint pin set for the second node", trustObservation(trustSame("pve2"), trustNode("pve3", 0x55, 0x56))},
		{"a disjoint pin set in another case", trustObservation(trustNode("PVE2", 0x55, 0x56), trustSame("pve3"))},
		{"a disjoint pin set in another case for the second node", trustObservation(trustSame("pve2"), trustNode("PVE3", 0x55))},
		{"a shrink and a disjoint set together", trustObservation(trustNode("pve2", 0x9e), trustNode("pve3", 0x57))},
		{"a changed identity", changed(trustSame("pve2"), trustSame("pve3"))},
		{"a changed identity and pins", changed(trustNode("pve2", 0x55), trustSame("pve3"))},
		{"a changed identity through the second configured endpoint", changedVia},
		{"no_candidates", noNodes(OutcomeNoCandidates)},
		{"standalone", noNodes(OutcomeStandalone)},
		{"public_suffix", noNodes(OutcomePublicSuffix)},
		{"no_status with an offline node", noStatus},
		{"through the second configured endpoint", via},
		{"an ambiguous node", trustObservation(trustSame("pve2"), trustNode("0x7f000001", 0x55))},
		{"a node the seed does not derive", notDerived},
		{"the seed's own node", trustObservation(trustNode("pve1", 0x11), trustSame("pve2"), trustSame("pve3"))},
		{"the second configured endpoint's node", trustObservation(trustSame("pve2"), trustNode("PVE1B", 0x12), trustSame("pve3"))},
	}
}

// TestDecideTrustAgreesWithMerge runs every stored file and observation under
// both verify_ssl values through DecideTrust, and every decision through
// Merge. The merged file must equal the one expectTrust builds from the
// fixture and the observation alone. Merge must also accept the observation
// as observed exactly when the rules accept every pin, and refuse it for the
// same reason and the same nodes when they do not.
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

// trustExpectation is what the trust rules should make of one observation.
type trustExpectation struct {
	outcome TrustOutcome
	log     TrustLog
	drop    bool
	changed []string
	shrunk  []string
	// refused names the nodes whose observed pins leave out a stored pin
	// under verify_ssl false, which Merge refuses when handed the observation
	// as observed.
	refused []string
	via     string
	// file is the file Merge should write for the decision.
	file *Payload
	// observed is the observation as a result with no pin rule applied,
	// which Merge must accept or refuse as the outcome says.
	observed *DiscoveryResult
}

// expectTrust builds the expected decision and merged file from the stored
// fixture and the observation, at payloadNow. It uses only the derivation,
// the node-name check, and the endpoint grammar, and never observedResult,
// validateDiscovery, or the pin comparisons under test. Whether the stored
// fixture is a usable file at all is Validate's answer.
func expectTrust(t *testing.T, stored *Payload, key Key, obs Observation) trustExpectation {
	t.Helper()
	now := payloadNow
	host, port, ok := config.NormalizeEndpoint(obs.Via)
	if !ok {
		t.Fatalf("the matrix holds an observation through %q", obs.Via)
	}
	via := net.JoinHostPort(host, strconv.Itoa(port))
	root := hex.EncodeToString(obs.RootCA[:])

	var prior *ClusterIdentity
	var known []DiscoveredNode
	var marks map[string]DownMark
	if stored != nil {
		if _, _, err := Validate(stored, key, now); err == nil {
			prior = stored.Cluster
			if stored.Discovery != nil {
				known = stored.Discovery.Nodes
			}
			marks = stored.Marks
		}
	}

	nodes := expectedNodes(key, obs, now)
	wait := time.Hour
	if obs.Outcome != OutcomeOK && obs.Outcome != OutcomeNoStatus {
		wait = 24 * time.Hour
	}
	section := func(nodes []DiscoveredNode) *DiscoverySection {
		return &DiscoverySection{
			DiscoveredAt: now, NextRefreshAfter: now.Add(wait), Outcome: obs.Outcome, Quorate: obs.Quorate, Nodes: nodes,
		}
	}
	e := trustExpectation{via: via, observed: &DiscoveryResult{
		Cluster:   ClusterIdentity{RootCASHA256: root, ObservedAt: now, ObservedVia: via},
		Discovery: *section(cloneNodes(nodes)),
	}}
	written := func(cluster ClusterIdentity, nodes []DiscoveredNode) *Payload {
		file := &Payload{Key: key, Cluster: &cluster, Discovery: section(nodes), Marks: marks}
		file.Key.Endpoints = slices.Clone(key.Endpoints)
		return file
	}
	attempted := func(conflict bool) *Payload {
		file := stored.clone()
		file.Cluster.IdentityConflict = file.Cluster.IdentityConflict || conflict
		// An attempt never makes a section visible that a reader hides for
		// its time, so it leaves that section as stored.
		if file.Discovery != nil && !file.Discovery.DiscoveredAt.After(now.Add(FutureSkew)) {
			file.Discovery.DiscoveredAt, file.Discovery.NextRefreshAfter = now, now.Add(24*time.Hour)
		}
		return file
	}

	switch {
	case prior != nil && prior.IdentityConflict:
		e.outcome, e.log, e.drop, e.file = TrustConflictStands, TrustLogWarn, true, attempted(false)
		return e
	case prior != nil && prior.RootCASHA256 != root && !key.VerifySSL:
		e.outcome, e.log, e.drop, e.file = TrustIdentityRefused, TrustLogError, true, attempted(true)
		return e
	case prior != nil && prior.RootCASHA256 != root:
		e.outcome, e.log, e.drop = TrustIdentityReplaced, TrustLogError, true
		e.file = written(e.observed.Cluster, cloneNodes(nodes))
		return e
	}

	e.changed, e.shrunk, e.refused = expectPins(nodes, known, key.VerifySSL)
	cluster := e.observed.Cluster
	if prior != nil {
		cluster.ObservedAt, cluster.ObservedVia = prior.ObservedAt, prior.ObservedVia
	}
	e.file = written(cluster, nodes)
	switch {
	case len(e.changed) > 0 && !key.VerifySSL:
		e.outcome, e.log = TrustPinsRefused, TrustLogWarn
	case len(e.changed) > 0:
		e.outcome = TrustPinsReplaced
	case prior == nil:
		e.outcome = TrustFirstUse
	default:
		e.outcome = TrustConfirmed
	}
	if e.log == TrustLogNone && len(e.shrunk) > 0 {
		e.log = TrustLogDebug
	}
	return e
}

// expectedNodes returns the nodes of obs that the result keeps, in the form
// the file stores, confirmed at now. A node is kept when its name is valid, its
// address is the one the seed derives for that name, and that address is not a
// configured endpoint.
func expectedNodes(key Key, obs Observation, now time.Time) []DiscoveredNode {
	nodes := make([]DiscoveredNode, 0, len(obs.Nodes))
	for _, n := range obs.Nodes {
		if ValidateNodeName(n.Name) != nil {
			continue
		}
		derived, err := DeriveAddress(key.Endpoints[0], n.Name)
		if err != nil || !strings.EqualFold(derived, n.Address) || slices.Contains(key.Endpoints, strings.ToLower(n.Address)) {
			continue
		}
		nodeHost, nodePort, _ := net.SplitHostPort(n.Address)
		portNumber, _ := strconv.Atoi(nodePort)
		var pins []string
		for _, pin := range n.Pins {
			if text := hex.EncodeToString(pin[:]); !slices.Contains(pins, text) {
				pins = append(pins, text)
			}
		}
		nodes = append(nodes, DiscoveredNode{
			Name: n.Name, Address: strings.ToLower(nodeHost), Port: portNumber, Pins: pins, Online: n.Online, ConfirmedAt: now,
		})
	}
	return nodes
}

// expectPins applies the pin rule to nodes against the known nodes by counting
// the stored pins each observed set keeps and the pins it adds. It returns the
// names of the nodes whose pins changed, the names of those whose set is a
// strict subset of the stored one, and the names of those whose set leaves out
// a stored pin under verify_ssl false, which is the union of the two. Under
// verify_ssl false a node that keeps every stored pin is unchanged, a node
// that only loses pins keeps its stored pins and is confirmed, and any other
// known node takes its stored pins, and its stored confirmation too when it
// keeps none of them.
func expectPins(nodes, known []DiscoveredNode, verify bool) (changed, shrunk, refused []string) {
	for i := range nodes {
		j := slices.IndexFunc(known, func(k DiscoveredNode) bool { return strings.EqualFold(k.Name, nodes[i].Name) })
		if j < 0 {
			continue
		}
		old := known[j]
		var kept int
		for _, pin := range nodes[i].Pins {
			if slices.Contains(old.Pins, pin) {
				kept++
			}
		}
		added := len(nodes[i].Pins) - kept
		switch {
		case kept == len(old.Pins) && added == 0:
		case verify:
			changed = append(changed, nodes[i].Name)
		case kept == len(old.Pins):
		case added == 0:
			shrunk = append(shrunk, nodes[i].Name)
			refused = append(refused, nodes[i].Name)
			nodes[i].Pins = slices.Clone(old.Pins)
		default:
			changed = append(changed, nodes[i].Name)
			refused = append(refused, nodes[i].Name)
			nodes[i].Pins = slices.Clone(old.Pins)
			if kept == 0 {
				nodes[i].ConfirmedAt = old.ConfirmedAt
			}
		}
	}
	return changed, shrunk, refused
}

// checkAgreement checks decision against expectTrust, runs it through Merge
// against stored, and checks the merged file and Merge's own reading of the
// observation as observed.
func checkAgreement(t *testing.T, stored *Payload, key Key, obs Observation, decision TrustDecision) {
	t.Helper()
	want := expectTrust(t, stored, key, obs)
	if decision.Outcome != want.outcome || decision.Log != want.log || decision.DropDiscovered != want.drop ||
		!reflect.DeepEqual(decision.PinsChanged, want.changed) || !reflect.DeepEqual(decision.PinsShrunk, want.shrunk) ||
		decision.Via != want.via {
		t.Fatalf("decision %s log %d drop %v changed %v shrunk %v via %q, want %s log %d drop %v changed %v shrunk %v via %q",
			decision.Outcome, decision.Log, decision.DropDiscovered, decision.PinsChanged, decision.PinsShrunk, decision.Via,
			want.outcome, want.log, want.drop, want.changed, want.shrunk, want.via)
	}
	if !decision.Writes() {
		t.Fatalf("decision %s writes nothing", decision.Outcome)
	}
	next, _, err := Merge(stored, key, decision.Change, payloadNow)
	if err != nil || next == nil {
		t.Fatalf("Merge refused a %s decision: %+v, %v", decision.Outcome, next, err)
	}
	if next.Cluster == nil || next.Cluster.ObservedVia == "" {
		t.Errorf("the merged file carries no observed_via: %+v", next.Cluster)
	}
	if !reflect.DeepEqual(next, want.file) {
		t.Errorf("merged file differs:\n got %s\nwant %s", trustDump(next), trustDump(want.file))
	}

	_, _, err = Merge(stored, key, Change{Result: want.observed}, payloadNow)
	var refusal *TrustRefusalError
	switch want.outcome {
	case TrustConflictStands:
		if !errors.Is(err, ErrIdentityConflict) {
			t.Errorf("trust kept a conflict, and Merge said %v", err)
		}
	case TrustIdentityRefused:
		if !errors.As(err, &refusal) || !refusal.IdentityChanged {
			t.Errorf("trust refused the identity, and Merge said %v", err)
		}
	default:
		// Merge refuses an observation that leaves out a stored pin, whether
		// or not the decision calls it a refusal, because the decision hands
		// Merge the stored pins in its place.
		if len(want.refused) > 0 {
			if !errors.As(err, &refusal) || refusal.IdentityChanged || !reflect.DeepEqual(refusal.Nodes, want.refused) {
				t.Errorf("an observation leaving out the pins of %v, and Merge said %v", want.refused, err)
			}
		} else if err != nil {
			t.Errorf("trust accepted every pin as observed, and Merge said %v", err)
		}
	}
}

// trustDump formats p with its sections and nodes spelled out.
func trustDump(p *Payload) string {
	if p == nil {
		return "<nil>"
	}
	out := fmt.Sprintf("cluster %+v marks %v", p.Cluster, p.Marks)
	if p.Discovery != nil {
		d := p.Discovery
		out += fmt.Sprintf(" discovery {at %v next %v %s quorate %v}", d.DiscoveredAt, d.NextRefreshAfter, d.Outcome, d.Quorate)
		for _, node := range d.Nodes {
			out += fmt.Sprintf(" %+v", node)
		}
	}
	return out
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
	shrunk  []string
	// check inspects the merged file. Every decision writes.
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

// trustNodeIn returns the node named name in p, and fails when there is none.
func trustNodeIn(t *testing.T, p *Payload, name string) DiscoveredNode {
	t.Helper()
	for _, node := range p.Discovery.Nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("no node %s in %v", name, trustNodeNames(p))
	return DiscoveredNode{}
}

// wantPinsAt checks one node's pins and confirmation in the merged file.
func wantPinsAt(name string, confirmed time.Time, pins ...byte) func(t *testing.T, next *Payload) {
	return func(t *testing.T, next *Payload) {
		t.Helper()
		node := trustNodeIn(t, next, name)
		want := make([]string, 0, len(pins))
		for _, b := range pins {
			want = append(want, payloadPin(hex.EncodeToString([]byte{b})))
		}
		if !reflect.DeepEqual(node.Pins, want) || !node.ConfirmedAt.Equal(confirmed) {
			t.Errorf("%s pins %v confirmed %v, want %v confirmed %v", name, node.Pins, node.ConfirmedAt, want, confirmed)
		}
	}
}

// wantTimes checks the merged section's two times.
func wantTimes(discovered time.Time, next time.Duration) func(t *testing.T, p *Payload) {
	return func(t *testing.T, p *Payload) {
		t.Helper()
		if !p.Discovery.DiscoveredAt.Equal(discovered) || !p.Discovery.NextRefreshAfter.Equal(discovered.Add(next)) {
			t.Errorf("discovered %v next %v, want %v and %v later", p.Discovery.DiscoveredAt, p.Discovery.NextRefreshAfter,
				discovered, next)
		}
	}
}

// all runs every check in order.
func all(checks ...func(t *testing.T, next *Payload)) func(t *testing.T, next *Payload) {
	return func(t *testing.T, next *Payload) {
		t.Helper()
		for _, check := range checks {
			check(t, next)
		}
	}
}

// attemptOnly checks that the merged file is stored with only the section's
// times moved to now and now plus MaxRefreshWait, and the conflict flag set
// when conflict is true.
func attemptOnly(stored func(Key) *Payload, verify, conflict bool) func(t *testing.T, next *Payload) {
	return func(t *testing.T, next *Payload) {
		t.Helper()
		want := stored(trustKey(verify))
		want.Cluster.IdentityConflict = want.Cluster.IdentityConflict || conflict
		want.Discovery.DiscoveredAt, want.Discovery.NextRefreshAfter = payloadNow, payloadNow.Add(MaxRefreshWait)
		if !reflect.DeepEqual(next, want) {
			t.Errorf("the attempt changed more than the times:\n got %s\nwant %s", trustDump(next), trustDump(want))
		}
	}
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
	stamp := payloadNow.Add(-5 * time.Minute)
	same := trustObservation(trustSame("pve2"), trustSame("pve3"))
	newNode := trustObservation(trustSame("pve2"), trustSame("pve3"), trustNode("pve4", 0x44))
	overlap := trustObservation(trustNode("pve2", 0xa1, 0x55), trustSame("pve3"), trustNode("pve4", 0x44))
	shrink := trustObservation(trustNode("pve2", 0xa1), trustSame("pve3"))
	grow := trustObservation(trustNode("pve2", 0x9e, 0xa1, 0x55), trustSame("pve3"))
	disjoint := trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3"))
	configured := trustObservation(trustNode("pve1", 0x11), trustNode("pve1b", 0x12), trustSame("pve2"))
	identity := trustObservation(trustNode("pve2", 0x55), trustNode("pve5", 0x66))
	identity.RootCA = trustPin(0xff)
	keepsIdentity := func(t *testing.T, next *Payload) {
		t.Helper()
		if next.Cluster.RootCASHA256 != payloadPin("3c") || !next.Cluster.ObservedAt.Equal(stamp) ||
			next.Cluster.ObservedVia != mergeSeed {
			t.Errorf("identity %+v, want the stored one with its first sighting", next.Cluster)
		}
	}
	hourly := wantTimes(payloadNow, RefreshInterval)
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
			check:   all(keepsIdentity, hourly, wantPinsAt("pve2", payloadNow, 0xa1, 0x9e), wantPinsAt("pve3", payloadNow, 0xa1, 0x9e)),
		},
		{
			name: "verify_ssl false, a new node is added on first use", stored: fresh, obs: newNode,
			outcome: TrustConfirmed, check: all(wantNodes("pve2", "pve3", "pve4"), wantPinsAt("pve4", payloadNow, 0x44)),
		},
		{
			name: "verify_ssl false, a node left out stops being known", stored: fresh,
			obs: trustObservation(trustSame("pve3")), outcome: TrustConfirmed, check: wantNodes("pve3"),
		},
		{
			name:   "verify_ssl false, a partially overlapping pin set keeps the stored pins and confirms the node",
			stored: fresh, obs: overlap, outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
			check: all(keepsIdentity, hourly, wantNodes("pve2", "pve3", "pve4"),
				wantPinsAt("pve2", payloadNow, 0xa1, 0x9e), wantPinsAt("pve3", payloadNow, 0xa1, 0x9e),
				wantPinsAt("pve4", payloadNow, 0x44)),
		},
		{
			name:   "verify_ssl false, a pin set that shrinks keeps the stored pins and confirms the node without a refusal",
			stored: fresh, obs: shrink, outcome: TrustConfirmed, log: TrustLogDebug, shrunk: []string{"pve2"},
			check: all(keepsIdentity, hourly, wantPinsAt("pve2", payloadNow, 0xa1, 0x9e), wantPinsAt("pve3", payloadNow, 0xa1, 0x9e)),
		},
		{
			name: "verify_ssl false, a pin set that shrinks for a node past 24 hours confirms it again", stored: expired,
			obs: shrink, outcome: TrustConfirmed, log: TrustLogDebug, shrunk: []string{"pve2"},
			check: wantPinsAt("pve2", payloadNow, 0xa1, 0x9e),
		},
		{
			name: "verify_ssl false, a shrink beside a disjoint set still refuses and warns", stored: fresh,
			obs:     trustObservation(trustNode("pve2", 0x9e), trustNode("pve3", 0x57)),
			outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve3"}, shrunk: []string{"pve2"},
			check: all(wantPinsAt("pve2", payloadNow, 0xa1, 0x9e), wantPinsAt("pve3", stamp, 0xa1, 0x9e)),
		},
		{
			name: "verify_ssl false, an overlap with the shared pin last confirms the node and refuses the change", stored: fresh,
			obs:     trustObservation(trustNode("pve2", 0x55, 0x56, 0x9e), trustSame("pve3")),
			outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
			check: wantPinsAt("pve2", payloadNow, 0xa1, 0x9e),
		},
		{
			name: "verify_ssl false, a pin set that grows is accepted", stored: fresh, obs: grow, outcome: TrustConfirmed,
			check: all(keepsIdentity, hourly, wantPinsAt("pve2", payloadNow, 0x9e, 0xa1, 0x55)),
		},
		{
			name: "verify_ssl false, a pin set that grows is accepted for a node past 24 hours", stored: expired,
			obs: grow, outcome: TrustConfirmed, check: wantPinsAt("pve2", payloadNow, 0x9e, 0xa1, 0x55),
		},
		{
			name: "verify_ssl false, a disjoint pin set keeps the stored pins and confirmation", stored: fresh,
			obs: disjoint, outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
			check: all(hourly, wantPinsAt("pve2", stamp, 0xa1, 0x9e), wantPinsAt("pve3", payloadNow, 0xa1, 0x9e)),
		},
		{
			name: "verify_ssl false, a disjoint pin set past 24 hours stays known and unloaded", stored: expired,
			obs: disjoint, outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				wantPinsAt("pve2", payloadNow.Add(-48*time.Hour), 0xa1, 0x9e)(t, next)
				state, _ := readState(next, trustKey(false), payloadNow)
				if names := trustNodeNames(state.usable); !reflect.DeepEqual(names, []string{"pve3"}) {
					t.Errorf("a reader loads %v, want pve3 alone", names)
				}
				if len(state.known) != 2 || state.known[0].Name != "pve2" {
					t.Errorf("known %+v, want pve2 still known", state.known)
				}
			},
		},
		{
			name: "verify_ssl false, an overlapping pin set past 24 hours confirms the node again", stored: expired,
			obs: overlap, outcome: TrustPinsRefused, log: TrustLogWarn, changed: []string{"pve2"},
			check: wantPinsAt("pve2", payloadNow, 0xa1, 0x9e),
		},
		{
			name: "verify_ssl false, a node past 24 hours with the same pins is confirmed again", stored: expired,
			obs: same, outcome: TrustConfirmed, check: wantPinsAt("pve2", payloadNow, 0xa1, 0x9e),
		},
		{
			name: "verify_ssl false, nodes that are configured endpoints are left out", stored: func(Key) *Payload { return nil },
			obs: configured, outcome: TrustFirstUse, check: wantNodes("pve2"),
		},
		{
			name: "verify_ssl true, nodes that are configured endpoints are left out", verify: true, stored: fresh,
			obs: configured, outcome: TrustConfirmed, check: wantNodes("pve2"),
		},
		{
			name: "verify_ssl false, a changed identity is recorded as a conflict", stored: fresh, obs: identity,
			outcome: TrustIdentityRefused, log: TrustLogError, drop: true, check: attemptOnly(fresh, false, true),
		},
		{
			name: "verify_ssl false, a recorded conflict refuses the stored identity too", stored: conflict, obs: same,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true, check: attemptOnly(conflict, false, false),
		},
		{
			name: "verify_ssl false, a recorded conflict refuses another identity", stored: conflict, obs: identity,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true, check: attemptOnly(conflict, false, false),
		},
		{
			name: "verify_ssl true, a changed identity replaces the nodes and pins", verify: true, stored: fresh,
			obs: identity, outcome: TrustIdentityReplaced, log: TrustLogError, drop: true,
			check: func(t *testing.T, next *Payload) {
				t.Helper()
				wantNodes("pve2", "pve5")(t, next)
				if next.Cluster.RootCASHA256 != payloadPin("ff") || !next.Cluster.ObservedAt.Equal(payloadNow) ||
					next.Cluster.ObservedVia != mergeSeed {
					t.Errorf("identity %+v", next.Cluster)
				}
				wantPinsAt("pve2", payloadNow, 0x55)(t, next)
			},
		},
		{
			name: "verify_ssl true, a changed pin set replaces the old one", verify: true, stored: expired,
			obs: overlap, outcome: TrustPinsReplaced, changed: []string{"pve2"},
			check: all(keepsIdentity, wantNodes("pve2", "pve3", "pve4"), wantPinsAt("pve2", payloadNow, 0xa1, 0x55)),
		},
		{
			name: "verify_ssl true, a pin set that shrinks replaces the old one", verify: true, stored: fresh,
			obs: shrink, outcome: TrustPinsReplaced, changed: []string{"pve2"}, check: wantPinsAt("pve2", payloadNow, 0xa1),
		},
		{
			name: "verify_ssl true, a pin set that grows replaces the old one", verify: true, stored: fresh,
			obs: grow, outcome: TrustPinsReplaced, changed: []string{"pve2"},
			check: wantPinsAt("pve2", payloadNow, 0x9e, 0xa1, 0x55),
		},
		{
			name: "verify_ssl true, a recorded conflict still refuses", verify: true, stored: conflict, obs: same,
			outcome: TrustConflictStands, log: TrustLogWarn, drop: true, check: attemptOnly(conflict, true, false),
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
				!reflect.DeepEqual(decision.PinsChanged, tc.changed) || !reflect.DeepEqual(decision.PinsShrunk, tc.shrunk) {
				t.Fatalf("decision %s log %d drop %v changed %v shrunk %v, want %s log %d drop %v changed %v shrunk %v",
					decision.Outcome, decision.Log, decision.DropDiscovered, decision.PinsChanged, decision.PinsShrunk,
					tc.outcome, tc.log, tc.drop, tc.changed, tc.shrunk)
			}
			if decision.ObservedRootCA != hex.EncodeToString(tc.obs.RootCA[:]) || decision.Via != mergeSeed {
				t.Errorf("observed root %q via %q", decision.ObservedRootCA, decision.Via)
			}
			if stored != nil && decision.StoredRootCA != payloadPin("3c") {
				t.Errorf("stored root %q", decision.StoredRootCA)
			}
			if !decision.Writes() {
				t.Fatalf("decision %s writes nothing", decision.Outcome)
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
// nodes is due again in an hour and one that found none waits 24 hours, that
// the written section carries the observed outcome and quorum flag, and that
// a decision stamps the discovery and its nodes with the clock.
func TestDecideTrustSchedulesTheNextRefresh(t *testing.T) {
	key := trustKey(false)
	cases := map[DiscoveryOutcome]time.Duration{
		OutcomeOK: time.Hour, OutcomeNoStatus: time.Hour,
		OutcomeNoCandidates: 24 * time.Hour, OutcomeStandalone: 24 * time.Hour, OutcomePublicSuffix: 24 * time.Hour,
	}
	for outcome, wait := range cases {
		for _, quorate := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s quorate %v", outcome, quorate), func(t *testing.T) {
				obs := trustObservation()
				if outcome.holdsNodes() {
					obs = trustObservation(trustSame("pve2"))
				}
				obs.Outcome, obs.Quorate = outcome, quorate
				decision, err := DecideTrust(Stored{}, key, obs, payloadNow)
				if err != nil {
					t.Fatal(err)
				}
				next, _, err := Merge(nil, key, decision.Change, payloadNow)
				if err != nil {
					t.Fatal(err)
				}
				section := next.Discovery
				if section.Outcome != outcome || section.Quorate != quorate {
					t.Errorf("written outcome %s quorate %v, want %s and %v", section.Outcome, section.Quorate, outcome, quorate)
				}
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
	}
	if RefreshInterval != time.Hour || PinRefreshFloor != 10*time.Minute {
		t.Errorf("RefreshInterval %v and PinRefreshFloor %v, want an hour and 10 minutes", RefreshInterval, PinRefreshFloor)
	}
}

// TestDecideTrustRefusalsScheduleTheNextRefresh checks the times each refusal
// writes. A refused identity and a standing conflict wait 24 hours, and a
// refused pin change waits an hour, so none of them is due again at once.
func TestDecideTrustRefusalsScheduleTheNextRefresh(t *testing.T) {
	key := trustKey(false)
	conflicted := payloadFor(key, "pve2")
	conflicted.Cluster.IdentityConflict = true
	other := trustObservation(trustSame("pve2"))
	other.RootCA = trustPin(0xff)
	refusals := map[string]struct {
		stored  *Payload
		obs     Observation
		outcome TrustOutcome
		wait    time.Duration
	}{
		"identity_refused": {payloadFor(key, "pve2"), other, TrustIdentityRefused, MaxRefreshWait},
		"conflict_stands":  {conflicted, trustObservation(trustSame("pve2")), TrustConflictStands, MaxRefreshWait},
		"pins_refused":     {payloadFor(key, "pve2"), trustObservation(trustNode("pve2", 0x55)), TrustPinsRefused, RefreshInterval},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			decision, err := DecideTrust(trustView(tc.stored, key), key, tc.obs, payloadNow)
			if err != nil || decision.Outcome != tc.outcome {
				t.Fatalf("DecideTrust: %+v, %v", decision, err)
			}
			next, _, err := Merge(tc.stored, key, decision.Change, payloadNow)
			if err != nil {
				t.Fatal(err)
			}
			wantTimes(payloadNow, tc.wait)(t, next)
			if next.Discovery.Outcome != OutcomeOK {
				t.Errorf("written outcome %s", next.Discovery.Outcome)
			}
			if p, _, err := Validate(next, key, payloadNow); err != nil || RefreshDue(p, payloadNow.Add(time.Minute), false) {
				t.Errorf("a refresh is due a minute after %s: %v", name, err)
			}
		})
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
		"a view read at another clock": {
			stored: func() Stored {
				view := trustView(payloadFor(key, "pve2"), key)
				view.Now = payloadNow.Add(time.Minute)
				return view
			}(),
			obs: trustObservation(trustSame("pve2")),
		},
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

// TestDecideTrustLeavesOutNodesMergeLeavesOut shows that an ambiguous name, an
// address the seed does not derive, and a node whose derived address is a
// configured endpoint are left out, as Merge leaves them out, so their pins
// are not compared and Merge reports the drop.
func TestDecideTrustLeavesOutNodesMergeLeavesOut(t *testing.T) {
	for _, verify := range []bool{false, true} {
		key := trustKey(verify)
		stored := payloadFor(key, "pve2", "pve3")
		obs := trustObservation(
			trustNode("pve2", 0x55), trustSame("pve3"), trustNode("localhost", 0x77),
			trustNode("pve1", 0x11), trustNode("Pve1B", 0x12),
		)
		obs.Nodes[0].Address = "pve2.attacker.example:8006"
		decision, err := DecideTrust(trustView(stored, key), key, obs, payloadNow)
		if err != nil || decision.Outcome != TrustConfirmed || len(decision.PinsChanged) != 0 {
			t.Fatalf("verify_ssl %v: DecideTrust: %+v, %v", verify, decision, err)
		}
		next, drops, err := Merge(stored, key, decision.Change, payloadNow)
		if err != nil {
			t.Fatal(err)
		}
		wantNodes("pve3")(t, next)
		want := []Drop{
			{Record: "node 0", Reason: DropNotDerived}, {Record: "node 2", Reason: DropAmbiguousName},
			{Record: "node 3", Reason: DropConfiguredEndpoint}, {Record: "node 4", Reason: DropConfiguredEndpoint},
		}
		if !reflect.DeepEqual(drops, want) {
			t.Errorf("verify_ssl %v: drops %v, want %v", verify, drops, want)
		}
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
		TrustLogDebug: {slog.LevelDebug, true},
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
	if !(TrustDecision{Change: Change{Attempt: &DiscoveryAttempt{At: payloadNow}}}).Writes() {
		t.Error("an attempt does not write")
	}
	if (TrustDecision{Change: Change{Marks: map[string]DownMark{mergeSeed: {}}}}).Writes() {
		t.Error("marks alone count as a trust write")
	}
}

// trustUpdate decides obs through UpdatePayload at now, as the discovery
// worker does, with the store's clock, and returns the decision.
func trustUpdate(t *testing.T, s *Store, now time.Time, obs Observation) TrustDecision {
	t.Helper()
	var decision TrustDecision
	result, err := s.UpdatePayload(context.Background(), now, func(stored Stored) (Change, error) {
		var err error
		decision, err = DecideTrust(stored, s.Key(), obs, stored.Now)
		return decision.Change, err
	})
	if err != nil || !result.Locked {
		t.Fatalf("UpdatePayload: %+v, %v", result, err)
	}
	if !decision.Writes() || !result.Written {
		t.Fatalf("%s decision: nothing written", decision.Outcome)
	}
	return decision
}

// trustStoredFile reads the store's file as written, without validation.
func trustStoredFile(t *testing.T, s *Store) *Payload {
	t.Helper()
	var p Payload
	if err := s.Read(&p); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return &p
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
	// pve2 comes back with pins that share none of its stored ones. It keeps
	// its stored pins and its first confirmation, so it stays known and
	// unloaded, while pve3 is confirmed and the refresh moves an hour on.
	if !RefreshDue(read(later), later, false) {
		t.Fatal("no refresh due 25 hours after the first discovery")
	}
	d := trustUpdate(t, s, later, trustObservation(trustNode("pve2", 0x55), trustSame("pve3")))
	if d.Outcome != TrustPinsRefused || d.Log != TrustLogWarn || !reflect.DeepEqual(d.PinsChanged, []string{"pve2"}) {
		t.Fatalf("pin change past 24 hours: %+v", d)
	}
	if got := trustNodeNames(read(later)); !reflect.DeepEqual(got, []string{"pve3"}) {
		t.Fatalf("nodes loaded after the refused pin change %v", got)
	}
	raw := trustStoredFile(t, s)
	if pve2 := trustNodeIn(t, raw, "pve2"); !reflect.DeepEqual(pve2.Pins, []string{payloadPin("a1"), payloadPin("9e")}) ||
		!pve2.ConfirmedAt.Equal(start) {
		t.Fatalf("pve2 after the refused pin change %+v", pve2)
	}
	if p := read(later); RefreshDue(p, later.Add(59*time.Minute), false) || !RefreshDue(p, later.Add(time.Hour), false) {
		t.Fatalf("refresh after the refused pin change %+v", p.Discovery)
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
	if !p.Discovery.NextRefreshAfter.Equal(later.Add(MaxRefreshWait)) || RefreshDue(p, later.Add(48*time.Hour), true) {
		t.Fatalf("refresh after the conflict %+v", p.Discovery)
	}
	for _, obs := range []Observation{other, trustObservation(trustSame("pve2"))} {
		if d := trustUpdate(t, s, later.Add(time.Hour), obs); d.Outcome != TrustConflictStands || d.Log != TrustLogWarn {
			t.Fatalf("after the conflict: %+v", d)
		}
	}
	// A standing conflict moves only the section's times, and the stored
	// nodes and pins stay for the operator to inspect.
	raw = trustStoredFile(t, s)
	if !raw.Discovery.DiscoveredAt.Equal(later.Add(time.Hour)) ||
		!reflect.DeepEqual(trustNodeNames(raw), []string{"pve2", "pve3"}) ||
		!reflect.DeepEqual(nodePins(raw, "pve3"), []string{payloadPin("33")}) {
		t.Fatalf("the file after the standing conflict %s", trustDump(raw))
	}
}

// TestStoreTrustAcceptsAPinSetThatGrows walks a verify_ssl false file whose
// first discovery pinned pve2 by its /nodes fingerprint alone, because its
// certificates/info read failed, through the full discoveries that follow.
// The grown set is accepted, so discovery never wedges and the nodes keep
// loading.
func TestStoreTrustAcceptsAPinSetThatGrows(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	if d := trustUpdate(t, s, payloadNow, trustObservation(trustNode("pve2", 0xa1), trustSame("pve3"))); d.Outcome != TrustFirstUse {
		t.Fatalf("first discovery: %s", d.Outcome)
	}
	full := trustObservation(trustSame("pve2"), trustSame("pve3"))
	for _, after := range []time.Duration{2 * time.Hour, 10 * time.Hour, 26 * time.Hour, 72 * time.Hour} {
		now := payloadNow.Add(after)
		d := trustUpdate(t, s, now, full)
		if d.Outcome != TrustConfirmed || d.Log != TrustLogNone || len(d.PinsChanged) != 0 {
			t.Fatalf("+%v: %+v", after, d)
		}
		p, _, err := s.ReadPayload(now, nil)
		if err != nil || !reflect.DeepEqual(trustNodeNames(p), []string{"pve2", "pve3"}) ||
			!reflect.DeepEqual(nodePins(p, "pve2"), []string{payloadPin("a1"), payloadPin("9e")}) {
			t.Fatalf("+%v: %+v, %v", after, p, err)
		}
		if !p.Discovery.NextRefreshAfter.Equal(now.Add(RefreshInterval)) || RefreshDue(p, now, false) {
			t.Fatalf("+%v: next refresh %v", after, p.Discovery.NextRefreshAfter)
		}
	}
}

// TestStoreTrustRefusalsMoveTheNextRefresh shows that a refused identity and a
// standing conflict each record an attempt, so the stored refresh time leaves
// the past, and that the attempt leaves every node and pin as stored.
func TestStoreTrustRefusalsMoveTheNextRefresh(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	trustUpdate(t, s, payloadNow, trustObservation(trustSame("pve2")))
	before := trustStoredFile(t, s)
	other := trustObservation(trustSame("pve2"))
	other.RootCA = trustPin(0xff)
	steps := []struct {
		at      time.Time
		obs     Observation
		outcome TrustOutcome
	}{
		{payloadNow.Add(2 * time.Hour), other, TrustIdentityRefused},
		{payloadNow.Add(3 * time.Hour), trustObservation(trustSame("pve2")), TrustConflictStands},
	}
	for _, step := range steps {
		if d := trustUpdate(t, s, step.at, step.obs); d.Outcome != step.outcome {
			t.Fatalf("%s: %+v", step.outcome, d)
		}
		raw := trustStoredFile(t, s)
		if !raw.Discovery.DiscoveredAt.Equal(step.at) || !raw.Discovery.NextRefreshAfter.Equal(step.at.Add(MaxRefreshWait)) {
			t.Errorf("%s: discovered %v next %v", step.outcome, raw.Discovery.DiscoveredAt, raw.Discovery.NextRefreshAfter)
		}
		if !reflect.DeepEqual(raw.Discovery.Nodes, before.Discovery.Nodes) || raw.Discovery.Outcome != before.Discovery.Outcome ||
			raw.Cluster.RootCASHA256 != before.Cluster.RootCASHA256 || !raw.Cluster.IdentityConflict {
			t.Errorf("%s changed more than the times: %s", step.outcome, trustDump(raw))
		}
	}
}

// TestDecideTrustTakesTheStoresClock shows that UpdatePayload hands build its
// own clock, and that a decision taken at another clock is refused before it
// reaches the merge, whose checks it would fail or pass at the wrong time.
func TestDecideTrustTakesTheStoresClock(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	at := payloadNow.Add(17 * time.Second)
	var seen time.Time
	if _, err := s.UpdatePayload(context.Background(), at, func(stored Stored) (Change, error) {
		seen = stored.Now
		return Change{}, nil
	}); err != nil || !seen.Equal(at) {
		t.Fatalf("build saw the clock %v, %v, want %v", seen, err, at)
	}

	key := trustKey(false)
	view := trustView(payloadFor(key, "pve2"), key)
	for _, skew := range []time.Duration{time.Nanosecond, -time.Nanosecond, 20 * time.Second, time.Minute, -time.Hour} {
		decision, err := DecideTrust(view, key, trustObservation(trustSame("pve2")), payloadNow.Add(skew))
		if !errors.Is(err, ErrInvalidChange) || decision.Writes() || decision.Outcome != "" {
			t.Errorf("skew %v: %+v, %v, want ErrInvalidChange", skew, decision, err)
		}
	}
	decision, err := DecideTrust(view, key, trustObservation(trustSame("pve2")), view.Now)
	if err != nil || decision.Outcome != TrustConfirmed {
		t.Fatalf("at the view's clock: %+v, %v", decision, err)
	}
	// The same instant in another zone is the same clock.
	elsewhere := view.Now.In(time.FixedZone("elsewhere", 5*3600))
	if decision, err = DecideTrust(view, key, trustObservation(trustSame("pve2")), elsewhere); err != nil ||
		decision.Outcome != TrustConfirmed {
		t.Fatalf("at the view's clock in another zone: %+v, %v", decision, err)
	}
	// A view with no clock skips the check, which is why a caller passes only
	// the view UpdatePayload builds.
	view.Now = time.Time{}
	if decision, err = DecideTrust(view, key, trustObservation(trustSame("pve2")), payloadNow.Add(time.Minute)); err != nil ||
		decision.Outcome != TrustConfirmed {
		t.Fatalf("a view with no clock: %+v, %v", decision, err)
	}
}

// TestStoreEmptyChangeKeepsTheTrustRecords shows what an empty change writes.
// The identity, the discovery section, and the pins stay as stored, and the
// merge still removes a mark that expired.
func TestStoreEmptyChangeKeepsTheTrustRecords(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	trustUpdate(t, s, payloadNow, trustObservation(trustSame("pve2")))
	if _, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	before := trustStoredFile(t, s)
	later := payloadNow.Add(5 * time.Minute)
	if res, err := s.UpdatePayload(context.Background(), later, func(Stored) (Change, error) { return Change{}, nil }); err != nil ||
		!res.Written {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	after := trustStoredFile(t, s)
	if len(before.Marks) != 1 || len(after.Marks) != 0 {
		t.Errorf("marks before %v after %v, want the expired mark removed", before.Marks, after.Marks)
	}
	if !reflect.DeepEqual(after.Cluster, before.Cluster) || !reflect.DeepEqual(after.Discovery, before.Discovery) {
		t.Errorf("an empty change moved a trust record:\n got %s\nwant %s", trustDump(after), trustDump(before))
	}
}

// TestRefreshDue covers each branch of the refresh predicate.
func TestRefreshDue(t *testing.T) {
	key := trustKey(false)
	section := func(attempt, next time.Time) *Payload {
		p := payloadFor(key, "pve2")
		p.Discovery.DiscoveredAt, p.Discovery.NextRefreshAfter = attempt, next
		return p
	}
	conflicted := func(p *Payload) *Payload {
		p.Cluster.IdentityConflict = true
		return p
	}
	identityOnly := &Payload{Key: key, Cluster: ptr(payloadCluster(payloadNow))}
	ahead, _, err := Validate(section(payloadNow.Add(time.Hour), payloadNow.Add(2*time.Hour)), key, payloadNow)
	if err != nil || ahead.Discovery != nil {
		t.Fatalf("a section dated ahead: %+v, %v", ahead, err)
	}
	hourAgo, inAnHour := payloadNow.Add(-time.Hour), payloadNow.Add(time.Hour)
	floor := payloadNow.Add(-PinRefreshFloor)
	// A discovery that held no nodes waits MaxRefreshWait, whatever the pins.
	noNodes := func(outcome DiscoveryOutcome, attempt time.Time) *Payload {
		p := section(attempt, attempt.Add(MaxRefreshWait))
		p.Discovery.Outcome, p.Discovery.Nodes = outcome, []DiscoveredNode{}
		valid, _, err := Validate(p, key, payloadNow)
		if err != nil {
			t.Fatalf("a %s section: %v", outcome, err)
		}
		return valid
	}
	type refreshCase struct {
		p         *Payload
		pinFailed bool
		want      bool
	}
	cases := map[string]refreshCase{
		"no file":                                      {nil, false, true},
		"no file after a pin failure":                  {nil, true, true},
		"a conflict with the refresh passed":           {conflicted(section(hourAgo.Add(-time.Hour), hourAgo)), false, false},
		"a conflict after a pin failure":               {conflicted(section(hourAgo, inAnHour)), true, false},
		"a conflict with no section":                   {conflicted(identityOnly.clone()), true, false},
		"an identity and no node list":                 {identityOnly, false, true},
		"a section validation dropped for its time":    {ahead, false, true},
		"a refresh not yet due":                        {section(payloadNow.Add(-time.Minute), inAnHour), false, false},
		"a refresh due at this instant":                {section(hourAgo, payloadNow), false, true},
		"a refresh past due":                           {section(hourAgo.Add(-time.Hour), hourAgo), false, true},
		"a refresh time validation dropped":            {section(payloadNow.Add(-time.Minute), time.Time{}), false, true},
		"a pin failure within the floor":               {section(payloadNow.Add(-5*time.Minute), inAnHour), true, false},
		"a pin failure exactly at the floor":           {section(floor, inAnHour), true, false},
		"a pin failure past the floor":                 {section(floor.Add(-time.Second), inAnHour), true, true},
		"an old attempt with no pin failure":           {section(payloadNow.Add(-30*time.Minute), inAnHour), false, false},
		"a pin failure never holds back a due refresh": {section(payloadNow.Add(-time.Minute), payloadNow.Add(-time.Second)), true, true},
	}
	for _, outcome := range []DiscoveryOutcome{OutcomeNoCandidates, OutcomeStandalone, OutcomePublicSuffix} {
		prefix := string(outcome) + " "
		cases[prefix+"an hour after the discovery"] = refreshCase{noNodes(outcome, hourAgo), false, false}
		cases[prefix+"just before 24 hours"] = refreshCase{noNodes(outcome, payloadNow.Add(-MaxRefreshWait+time.Second)), false, false}
		cases[prefix+"after 24 hours"] = refreshCase{noNodes(outcome, payloadNow.Add(-MaxRefreshWait)), false, true}
		cases[prefix+"a pin failure past the floor"] = refreshCase{noNodes(outcome, payloadNow.Add(-11*time.Minute)), true, false}
		cases[prefix+"a pin failure an hour after the discovery"] = refreshCase{noNodes(outcome, hourAgo), true, false}
		cases[prefix+"a pin failure after 24 hours"] = refreshCase{noNodes(outcome, payloadNow.Add(-MaxRefreshWait)), true, true}
	}
	for name, tc := range cases {
		if got := RefreshDue(tc.p, payloadNow, tc.pinFailed); got != tc.want {
			t.Errorf("%s: RefreshDue = %v, want %v", name, got, tc.want)
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
