package endpointcache

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// nodesSecond is a second configured endpoint, beside mergeSeed.
const nodesSecond = "pve1b.dc1.example.com:8006"

// nodesKey is mergeKey with a second endpoint, so a name can be stored for each.
func nodesKey(verifySSL bool) Key {
	key := mergeKey()
	key.Endpoints = []string{mergeSeed, nodesSecond}
	key.VerifySSL = verifySSL
	return key
}

// nodesResult is mergeResult carrying names as the cluster's endpoint_nodes.
func nodesResult(names map[string]string) *DiscoveryResult {
	result := mergeResult("pve2")
	result.Cluster.EndpointNodes = names
	return result
}

// nodesStored is a stored file under key whose cluster holds names.
func nodesStored(key Key, names map[string]string) *Payload {
	p := payloadFor(key, "pve2")
	p.Cluster.EndpointNodes = names
	return p
}

func TestEndpointNodesJSONShape(t *testing.T) {
	p := nodesStored(nodesKey(true), map[string]string{mergeSeed: "pve1"})
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"endpoint_nodes":{"pve1.dc1.example.com:8006":"pve1"}`) {
		t.Errorf("payload JSON lacks endpoint_nodes: %s", data)
	}
	bare, err := json.Marshal(payloadFor(nodesKey(true), "pve2"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "endpoint_nodes") {
		t.Errorf("a cluster with no names writes endpoint_nodes: %s", bare)
	}
}

func TestValidateKeepsTheEndpointNodes(t *testing.T) {
	key := nodesKey(true)
	p := nodesStored(key, map[string]string{mergeSeed: "pve1", nodesSecond: "pve9"})
	got, drops := validatedPayload(t, p)
	if len(drops) != 0 {
		t.Errorf("drops %v", drops)
	}
	want := map[string]string{mergeSeed: "pve1", nodesSecond: "pve9"}
	if !reflect.DeepEqual(got.Cluster.EndpointNodes, want) {
		t.Errorf("endpoint_nodes %v, want %v", got.Cluster.EndpointNodes, want)
	}
	// The copy Validate returns shares nothing with the caller's map.
	got.Cluster.EndpointNodes[mergeSeed] = "changed"
	if p.Cluster.EndpointNodes[mergeSeed] != "pve1" {
		t.Error("Validate returned the caller's endpoint_nodes map")
	}
}

func TestValidateReadsAFileFromBeforeEndpointNodes(t *testing.T) {
	p := payloadFor(nodesKey(true), "pve2")
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "endpoint_nodes") {
		t.Fatalf("the fixture already holds the field: %s", data)
	}
	var stored Payload
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	got, drops, err := Validate(&stored, p.Key, payloadNow)
	if err != nil || len(drops) != 0 {
		t.Fatalf("Validate: %v, drops %v", err, drops)
	}
	if got.Cluster == nil || len(got.Cluster.EndpointNodes) != 0 || len(got.Discovery.Nodes) != 1 {
		t.Errorf("a file from before the field read as %+v", got)
	}
}

func TestValidateNormalizesTheEndpointNodeKeys(t *testing.T) {
	p := nodesStored(nodesKey(true), map[string]string{"PVE1.DC1.Example.com:8006": "pve1"})
	got, drops := validatedPayload(t, p)
	if len(drops) != 0 || !reflect.DeepEqual(got.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %v, drops %v", got.Cluster.EndpointNodes, drops)
	}
	// Two spellings of one endpoint keep the first by key order and drop the other.
	p = nodesStored(nodesKey(true), map[string]string{
		"PVE1.dc1.example.com:8006": "pve1",
		mergeSeed:                   "pve7",
	})
	got, drops = validatedPayload(t, p)
	if len(got.Cluster.EndpointNodes) != 1 || len(drops) != 1 || drops[0].Reason != DropInvalid {
		t.Errorf("endpoint_nodes %v, drops %v", got.Cluster.EndpointNodes, drops)
	}
}

func TestValidateDropsAnEndpointNodeForAnUnconfiguredEndpoint(t *testing.T) {
	p := nodesStored(nodesKey(true), map[string]string{
		mergeSeed:                      "pve1",
		"gone.dc1.example.com:8006":    "pve4",
		"pve1.dc1.example.com:443":     "pve5",
		"pve2.dc1.example.com:8006":    "pve2",
		"another.dc1.example.com:8006": "pve6",
	})
	got, drops := validatedPayload(t, p)
	if !reflect.DeepEqual(got.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %v, want only the configured endpoint", got.Cluster.EndpointNodes)
	}
	if len(drops) != 4 {
		t.Fatalf("drops %v, want four", drops)
	}
	for _, d := range drops {
		if d.Record != "endpoint node" || d.Reason != DropNotConfigured {
			t.Errorf("drop %v", d)
		}
	}
	// The rest of the file stands, the identity included.
	if got.Cluster.RootCASHA256 != payloadPin("3c") || len(got.Discovery.Nodes) != 1 {
		t.Errorf("the drop took more than the entries: %+v", got)
	}
	// Nothing left leaves no map at all.
	p = nodesStored(nodesKey(true), map[string]string{"gone.dc1.example.com:8006": "pve4"})
	got, _ = validatedPayload(t, p)
	if got.Cluster.EndpointNodes != nil {
		t.Errorf("endpoint_nodes %v, want none", got.Cluster.EndpointNodes)
	}
}

func TestValidateDropsAnAmbiguousEndpointNodeAlone(t *testing.T) {
	p := nodesStored(nodesKey(true), map[string]string{mergeSeed: "pve1", nodesSecond: "0x10"})
	got, drops := validatedPayload(t, p)
	if !reflect.DeepEqual(got.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %v", got.Cluster.EndpointNodes)
	}
	if len(drops) != 1 || drops[0].Reason != DropAmbiguousName {
		t.Errorf("drops %v", drops)
	}
}

func TestValidateRefusesAnInvalidEndpointNode(t *testing.T) {
	cases := map[string]map[string]string{
		"name with dots":         {mergeSeed: "pve1.example.com"},
		"name of 64 characters":  {mergeSeed: strings.Repeat("a", 64)},
		"empty name":             {mergeSeed: ""},
		"name with a space":      {mergeSeed: "pve 1"},
		"name with a hyphen led": {mergeSeed: "-pve1"},
		"key with no port":       {"pve1.dc1.example.com": "pve1"},
		"empty key":              {"": "pve1"},
		"key with a space":       {"pve1 .dc1.example.com:8006": "pve1"},
		"invalid beside valid":   {mergeSeed: "pve1", nodesSecond: "pve 9"},
	}
	for name, nodes := range cases {
		t.Run(name, func(t *testing.T) {
			p := nodesStored(nodesKey(true), nodes)
			_, _, err := Validate(p, p.Key, payloadNow)
			if !errors.Is(err, ErrPayloadInvalid) || !errors.Is(err, ErrFileInvalid) {
				t.Fatalf("Validate: %v, want ErrPayloadInvalid", err)
			}
			if strings.Contains(err.Error(), "pve 9") || strings.Contains(err.Error(), "pve1.example.com") {
				t.Errorf("error %v echoes the file", err)
			}
		})
	}
}

func TestValidateLoadsNoEndpointNodeOfAConflictedIdentity(t *testing.T) {
	p := nodesStored(nodesKey(false), map[string]string{mergeSeed: "pve1"})
	p.Cluster.IdentityConflict = true
	got, _ := validatedPayload(t, p)
	// Validate keeps the map so a merge can carry it, and the pve reader
	// declines to apply a name from a conflicted identity.
	if !got.Cluster.IdentityConflict || !reflect.DeepEqual(got.Cluster.EndpointNodes, p.Cluster.EndpointNodes) {
		t.Errorf("cluster %+v", got.Cluster)
	}
}

func TestMergeStoresTheEndpointNodes(t *testing.T) {
	key := nodesKey(true)
	next, _, err := Merge(nil, key, Change{Result: nodesResult(map[string]string{mergeSeed: "pve1"})}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %v", next.Cluster.EndpointNodes)
	}
}

func TestMergeJoinsEndpointNodesOfTheSameIdentity(t *testing.T) {
	key := nodesKey(true)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1"})
	// A refresh that read through the second endpoint adds its name and keeps
	// the first, which that refresh had no read for.
	next, _, err := Merge(stored, key, Change{Result: nodesResult(map[string]string{nodesSecond: "pve9"})}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{mergeSeed: "pve1", nodesSecond: "pve9"}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, want) {
		t.Errorf("endpoint_nodes %v, want %v", next.Cluster.EndpointNodes, want)
	}
	// A later name for an endpoint wins over the stored one.
	next, _, err = Merge(next, key, Change{Result: nodesResult(map[string]string{mergeSeed: "pve4"})}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]string{mergeSeed: "pve4", nodesSecond: "pve9"}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, want) {
		t.Errorf("endpoint_nodes %v, want %v", next.Cluster.EndpointNodes, want)
	}
	// A result with none of its own leaves the stored names.
	next, _, err = Merge(next, key, Change{Result: nodesResult(nil)}, payloadNow)
	if err != nil || !reflect.DeepEqual(next.Cluster.EndpointNodes, want) {
		t.Errorf("endpoint_nodes %v, err %v, want %v", next.Cluster.EndpointNodes, err, want)
	}
}

func TestMergeDoesNotChangeTheStoredEndpointNodes(t *testing.T) {
	key := nodesKey(true)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1"})
	result := nodesResult(map[string]string{nodesSecond: "pve9"})
	if _, _, err := Merge(stored, key, Change{Result: result}, payloadNow); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) ||
		!reflect.DeepEqual(result.Cluster.EndpointNodes, map[string]string{nodesSecond: "pve9"}) {
		t.Errorf("stored %v, result %v", stored.Cluster.EndpointNodes, result.Cluster.EndpointNodes)
	}
}

func TestMergeReplacesEndpointNodesWhenTheIdentityChangesUnderVerifySSLTrue(t *testing.T) {
	key := nodesKey(true)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1", nodesSecond: "pve9"})
	changed := nodesResult(map[string]string{mergeSeed: "pve3"})
	changed.Cluster.RootCASHA256 = payloadPin("ff")
	next, _, err := Merge(stored, key, Change{Result: changed}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve3"}) {
		t.Errorf("endpoint_nodes %v, want only the new cluster's", next.Cluster.EndpointNodes)
	}
	// A changed identity that names nothing leaves nothing of the old cluster.
	changed.Cluster.EndpointNodes = nil
	next, _, err = Merge(stored, key, Change{Result: changed}, payloadNow)
	if err != nil || len(next.Cluster.EndpointNodes) != 0 {
		t.Errorf("endpoint_nodes %v, err %v, want none", next.Cluster.EndpointNodes, err)
	}
}

func TestMergeKeepsEndpointNodesWhenTheIdentityChangeIsRefusedUnderVerifySSLFalse(t *testing.T) {
	key := nodesKey(false)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1"})
	changed := nodesResult(map[string]string{mergeSeed: "pve3"})
	changed.Cluster.RootCASHA256 = payloadPin("ff")
	next, _, err := Merge(stored, key, Change{Result: changed}, payloadNow)
	if !errors.Is(err, ErrIdentityChanged) || next != nil {
		t.Fatalf("Merge: %+v, %v, want an identity refusal", next, err)
	}
	// The caller records the refusal as a conflict, and the stored names stand.
	recorded, _, err := Merge(stored, key, Change{Conflict: true}, payloadNow)
	if err != nil || !recorded.Cluster.IdentityConflict ||
		!reflect.DeepEqual(recorded.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("recorded %+v, err %v", recorded, err)
	}
	// A standing conflict refuses a result, and a mark write passes the names over.
	recorded.Cluster.RootCASHA256 = payloadPin("3c")
	if next, _, err := Merge(recorded, key, Change{Result: nodesResult(map[string]string{mergeSeed: "pve3"})},
		payloadNow); !errors.Is(err, ErrIdentityConflict) || next != nil {
		t.Errorf("a result over the conflict: %+v, %v", next, err)
	}
	marked, _, err := Merge(recorded, key,
		Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}}, payloadNow)
	if err != nil || !reflect.DeepEqual(marked.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("a mark write changed the names: %+v, %v", marked, err)
	}
}

func TestMergeKeepsEndpointNodesWhenAMarkOrAHintIsWritten(t *testing.T) {
	key := nodesKey(true)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1"})
	next, _, err := Merge(stored, key,
		Change{Marks: map[string]DownMark{nodesSecond: {DownAt: payloadNow, Class: MarkDial}}}, payloadNow)
	if err != nil || !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("a mark write: %+v, %v", next, err)
	}
}

func TestMergeDropsAnEndpointNodeForAnUnconfiguredEndpoint(t *testing.T) {
	key := nodesKey(true)
	result := nodesResult(map[string]string{mergeSeed: "pve1", "gone.dc1.example.com:8006": "pve4"})
	next, drops, err := Merge(nil, key, Change{Result: result}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %v", next.Cluster.EndpointNodes)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "endpoint node", Reason: DropNotConfigured}}) {
		t.Errorf("drops %v", drops)
	}
	// A configuration that left the endpoint out drops the stored name on the
	// next write, because the stored file is read against the new key.
	narrowed := mergeKey()
	stored := nodesStored(nodesKey(true), map[string]string{mergeSeed: "pve1", nodesSecond: "pve9"})
	stored.Key = narrowed
	next, _, err = Merge(stored, narrowed, Change{Result: nodesResult(nil)}, payloadNow)
	if err != nil || !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("endpoint_nodes %+v, err %v", next, err)
	}
}

func TestMergeRefusesAResultWithAnInvalidEndpointNode(t *testing.T) {
	for name, names := range map[string]map[string]string{
		"bad name":         {mergeSeed: "pve1.example.com"},
		"empty name":       {mergeSeed: ""},
		"key not endpoint": {"not an endpoint": "pve1"},
	} {
		t.Run(name, func(t *testing.T) {
			stored := nodesStored(nodesKey(true), map[string]string{mergeSeed: "pve1"})
			next, _, err := Merge(stored, nodesKey(true), Change{Result: nodesResult(names)}, payloadNow)
			if !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange", next, err)
			}
		})
	}
}

func TestMergeTreatsAStoredFileWithAnInvalidEndpointNodeAsMissing(t *testing.T) {
	stored := nodesStored(nodesKey(true), map[string]string{mergeSeed: "pve 1"})
	result := nodesResult(map[string]string{nodesSecond: "pve9"})
	next, drops, err := Merge(stored, nodesKey(true), Change{Result: result}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{nodesSecond: "pve9"}) || len(drops) == 0 {
		t.Errorf("endpoint_nodes %v, drops %v, want the unusable file replaced", next.Cluster.EndpointNodes, drops)
	}
}

func TestStoreRoundTripsTheEndpointNodes(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	first, second := s.Key().Endpoints[0], s.Key().Endpoints[1]
	result := mergeResult("pve2")
	result.Cluster.ObservedVia = first
	result.Cluster.EndpointNodes = map[string]string{first: "pve1"}
	putResult(t, s, payloadNow, result)

	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 {
		t.Fatalf("ReadPayload: %v, drops %v", err, drops)
	}
	if !reflect.DeepEqual(got.Cluster.EndpointNodes, map[string]string{first: "pve1"}) {
		t.Fatalf("endpoint_nodes %v", got.Cluster.EndpointNodes)
	}
	// A second process's refresh adds its endpoint's name beside the first.
	again := mergeResult("pve2")
	again.Cluster.ObservedVia = second
	again.Cluster.EndpointNodes = map[string]string{second: "pve9"}
	putResult(t, s, payloadNow, again)
	got, _, err = s.ReadPayload(payloadNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{first: "pve1", second: "pve9"}
	if !maps.Equal(got.Cluster.EndpointNodes, want) {
		t.Errorf("endpoint_nodes %v, want %v", got.Cluster.EndpointNodes, want)
	}
}

func TestDecideTrustCarriesTheObservedEndpointNodes(t *testing.T) {
	key := nodesKey(true)
	obs := trustObservation(trustSame("pve2"))
	obs.EndpointNodes = map[string]string{mergeSeed: "pve1", "gone.dc1.example.com:8006": "pve4"}
	decision, err := DecideTrust(trustView(nil, key), key, obs, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	got := decision.Change.Result.Cluster.EndpointNodes
	if !reflect.DeepEqual(got, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("result endpoint_nodes %v, want only the configured endpoint", got)
	}
	// Writing the decision stores the name.
	next, _, err := Merge(nil, key, decision.Change, payloadNow)
	if err != nil || !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("merged %+v, err %v", next, err)
	}
}

func TestDecideTrustRefusesAnObservedEndpointNodeThatFailsTheFormat(t *testing.T) {
	key := nodesKey(true)
	obs := trustObservation(trustSame("pve2"))
	obs.EndpointNodes = map[string]string{mergeSeed: "pve1.example.com"}
	if _, err := DecideTrust(trustView(nil, key), key, obs, payloadNow); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("DecideTrust: %v, want ErrInvalidChange", err)
	}
}

func TestDecideTrustKeepsStoredEndpointNodesWhenTheIdentityIsRefused(t *testing.T) {
	key := nodesKey(false)
	stored := nodesStored(key, map[string]string{mergeSeed: "pve1"})
	obs := trustObservation(trustSame("pve2"))
	obs.RootCA = trustPin(0xff)
	obs.EndpointNodes = map[string]string{mergeSeed: "pve3"}
	decision, err := DecideTrust(trustView(stored, key), key, obs, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != TrustIdentityRefused || decision.Change.Result != nil {
		t.Fatalf("decision %+v, want a refusal that writes no result", decision)
	}
	next, _, err := Merge(stored, key, decision.Change, payloadNow)
	if err != nil || !reflect.DeepEqual(next.Cluster.EndpointNodes, map[string]string{mergeSeed: "pve1"}) {
		t.Errorf("merged %+v, err %v", next, err)
	}
}
