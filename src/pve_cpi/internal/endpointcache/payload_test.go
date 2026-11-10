package endpointcache

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// payloadNow is the reader's clock in the payload tests.
var payloadNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// payloadPin returns a pin made of the two-digit hex byte repeated 32 times.
func payloadPin(b string) string { return strings.Repeat(b, 32) }

// payloadNode returns a valid node named name that was confirmed at confirmed.
func payloadNode(name string, confirmed time.Time) DiscoveredNode {
	return DiscoveredNode{
		Name:        name,
		Address:     name + ".dc1.example.com",
		Port:        8006,
		Pins:        []string{payloadPin("a1"), payloadPin("9e")},
		Online:      true,
		ConfirmedAt: confirmed,
	}
}

// payloadCluster returns a valid cluster identity observed at observed.
func payloadCluster(observed time.Time) ClusterIdentity {
	return ClusterIdentity{
		RootCASHA256: payloadPin("3c"),
		ObservedAt:   observed,
		ObservedVia:  "pve1.dc1.example.com:8006",
	}
}

// payloadDiscovery returns a valid discovery section with no nodes, taken just
// before payloadNow.
func payloadDiscovery() DiscoverySection {
	at := payloadNow.Add(-5 * time.Minute)
	return DiscoverySection{
		DiscoveredAt:     at,
		NextRefreshAfter: at.Add(time.Hour),
		Outcome:          OutcomeOK,
		Quorate:          true,
	}
}

// payloadFor returns a payload for the store's key holding a cluster identity
// and a discovery of the named nodes, confirmed five minutes before
// payloadNow.
func payloadFor(key Key, names ...string) *Payload {
	cluster := payloadCluster(payloadNow.Add(-5 * time.Minute))
	section := payloadDiscovery()
	for _, name := range names {
		section.Nodes = append(section.Nodes, payloadNode(name, payloadNow.Add(-5*time.Minute)))
	}
	return &Payload{Key: key, Cluster: &cluster, Discovery: &section}
}

// payloadTestKey is a key to build payloads for when no store is open.
func payloadTestKey() Key {
	return Key{
		Endpoints:    []string{"pve1.dc1.example.com:8006"},
		VerifySSL:    true,
		CASHA256:     SystemCA,
		CredentialID: "1f6a09c2e4b7d385",
	}
}

func TestPayloadJSONShape(t *testing.T) {
	key := payloadTestKey()
	p := payloadFor(key, "pve2")
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial}}
	p.ColdStart = &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"key", "cluster", "discovery", "marks", "cold_start"} {
		if _, ok := generic[field]; !ok {
			t.Errorf("payload has no %q member: %s", field, data)
		}
	}
	hint, _ := generic["cold_start"].(map[string]any)
	if len(hint) != 2 || hint["name"] != "pve2" || hint["probed_at"] == nil {
		t.Errorf("cold_start holds %v, want only name and probed_at", hint)
	}
	for _, want := range []string{
		`"root_ca_sha256"`, `"observed_at"`, `"observed_via"`, `"identity_conflict"`, `"discovered_at"`,
		`"next_refresh_after"`, `"outcome":"ok"`, `"quorate":true`, `"confirmed_at"`, `"down_at"`, `"class":"dial"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("payload JSON lacks %s: %s", want, data)
		}
	}
}

func TestPayloadOmitsEmptySections(t *testing.T) {
	data, err := json.Marshal(&Payload{Key: payloadTestKey()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "cluster") || strings.Contains(string(data), "discovery") ||
		strings.Contains(string(data), "marks") || strings.Contains(string(data), "cold_start") {
		t.Errorf("an empty payload writes sections: %s", data)
	}
}

func TestEnumerationsAcceptOnlyTheirValues(t *testing.T) {
	for _, o := range []DiscoveryOutcome{OutcomeOK, OutcomeNoStatus, OutcomeNoCandidates, OutcomeStandalone, OutcomePublicSuffix} {
		if !o.Valid() {
			t.Errorf("outcome %q is refused", o)
		}
	}
	for _, o := range []DiscoveryOutcome{"", "OK", "dial: connection refused", "failed"} {
		if o.Valid() {
			t.Errorf("outcome %q is accepted", o)
		}
	}
	for _, c := range []MarkClass{MarkDial, MarkDNS, MarkTLSHandshake, MarkTLSVerify, MarkPin} {
		if !c.Valid() {
			t.Errorf("class %q is refused", c)
		}
	}
	for _, c := range []MarkClass{"", "DIAL", "dial: connection refused", "timeout"} {
		if c.Valid() {
			t.Errorf("class %q is accepted", c)
		}
	}
}

func TestCloneSharesNothing(t *testing.T) {
	p := payloadFor(payloadTestKey(), "pve2")
	p.Marks = map[string]DownMark{"pve1.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial}}
	p.ColdStart = &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}
	c := p.clone()
	c.Key.Endpoints[0] = "changed"
	c.Cluster.RootCASHA256 = "changed"
	c.Discovery.Nodes[0].Pins[0] = "changed"
	c.Marks["x"] = DownMark{}
	c.ColdStart.Name = "changed"
	if p.Key.Endpoints[0] == "changed" || p.Cluster.RootCASHA256 == "changed" || p.Discovery.Nodes[0].Pins[0] == "changed" ||
		len(p.Marks) != 1 || p.ColdStart.Name == "changed" {
		t.Errorf("clone shares state with the original: %+v", p)
	}
	var none *Payload
	if none.clone() != nil {
		t.Error("clone of nil is not nil")
	}
	var section *DiscoverySection
	if section.clone() != nil {
		t.Error("clone of a nil section is not nil")
	}
}

func TestBoundsAreTheSpecsValues(t *testing.T) {
	// Each bound is written out as the spec states it, so a changed constant
	// fails here even when its own boundary test is built from the constant.
	ints := []struct {
		name      string
		got, want int
	}{
		{"MaxNodes", MaxNodes, 64},
		{"MaxPinsPerNode", MaxPinsPerNode, 3},
		{"MaxOtherMarks", MaxOtherMarks, 64},
		{"maxFileBytes", maxFileBytes, 64 * 1024},
	}
	for _, b := range ints {
		if b.got != b.want {
			t.Errorf("%s = %d, want %d", b.name, b.got, b.want)
		}
	}
	durations := []struct {
		name      string
		got, want time.Duration
	}{
		{"MarkTTL", MarkTTL, 2 * time.Minute},
		{"HintTTL", HintTTL, time.Hour},
		{"NodeTTL", NodeTTL, 24 * time.Hour},
		{"MaxRefreshWait", MaxRefreshWait, 24 * time.Hour},
		{"FutureSkew", FutureSkew, 30 * time.Second},
	}
	for _, b := range durations {
		if b.got != b.want {
			t.Errorf("%s = %v, want %v", b.name, b.got, b.want)
		}
	}
}
