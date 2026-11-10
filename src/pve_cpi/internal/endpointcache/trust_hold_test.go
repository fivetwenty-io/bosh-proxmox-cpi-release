package endpointcache

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// heldRequest is one discovery against a stored file whose nodes are past
// NodeTTL, and the drops Merge and UpdatePayload must report for it.
type heldRequest struct {
	name   string
	stored func(key Key) *Payload
	obs    Observation
	// drops are the records Merge reports. A stored node the result holds is
	// not among them, because the file keeps it.
	drops []Drop
	// nodes are the names the merged file holds.
	nodes []string
}

func heldRequests() []heldRequest {
	expiredSecond := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		return p
	}
	expiredFirstInUpperCase := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[0].Name = "PVE2"
		p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		return p
	}
	bothExpired := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		p.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-72 * time.Hour)
		return p
	}
	notDerivedAndExpired := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[0].Address = "pve2.attacker.example"
		p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
		return p
	}
	aheadFirst := func(key Key) *Payload {
		p := payloadFor(key, "pve2", "pve3")
		p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(time.Hour)
		return p
	}
	return []heldRequest{
		{
			name: "a held second node", stored: expiredSecond,
			obs:   trustObservation(trustSame("pve2"), trustNode("pve3", 0x55, 0x56)),
			drops: nil, nodes: []string{"pve2", "pve3"},
		},
		{
			name: "a held first node stored in another case", stored: expiredFirstInUpperCase,
			obs:   trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3")),
			drops: nil, nodes: []string{"pve2", "pve3"},
		},
		{
			name: "a held node dated ahead", stored: aheadFirst,
			obs:   trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3")),
			drops: nil, nodes: []string{"pve2", "pve3"},
		},
		{
			name: "a held node beside a node that is confirmed again", stored: bothExpired,
			obs:   trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3")),
			drops: []Drop{{Record: "node 1", Reason: DropExpired}}, nodes: []string{"pve2", "pve3"},
		},
		{
			// The held node comes back at the address the seed derives, and
			// the read drop for the address it was stored with still stands.
			name: "a held node whose stored address no longer derives", stored: notDerivedAndExpired,
			obs:   trustObservation(trustNode("pve2", 0x55, 0x56), trustSame("pve3")),
			drops: []Drop{{Record: "node 0", Reason: DropNotDerived}}, nodes: []string{"pve2", "pve3"},
		},
		{
			name: "a node confirmed again is not held", stored: expiredSecond,
			obs:   trustObservation(trustSame("pve2"), trustSame("pve3")),
			drops: []Drop{{Record: "node 1", Reason: DropExpired}}, nodes: []string{"pve2", "pve3"},
		},
		{
			name: "a node the observation leaves out", stored: expiredSecond,
			obs:   trustObservation(trustSame("pve2")),
			drops: []Drop{{Record: "node 1", Reason: DropExpired}}, nodes: []string{"pve2"},
		},
	}
}

// TestMergeReportsNoDropForAHeldNode shows that a stored node the result
// carries as held, which the file keeps although a reader hides it for its
// time, is not reported as dropped. A node the file does not keep still is.
func TestMergeReportsNoDropForAHeldNode(t *testing.T) {
	key := trustKey(false)
	for _, tc := range heldRequests() {
		t.Run(tc.name, func(t *testing.T) {
			stored := tc.stored(key)
			decision, err := DecideTrust(trustView(stored, key), key, tc.obs, payloadNow)
			if err != nil {
				t.Fatal(err)
			}
			next, drops, err := Merge(stored, key, decision.Change, payloadNow)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(drops, tc.drops) {
				t.Errorf("drops %v, want %v", drops, tc.drops)
			}
			if got := trustNodeNames(next); !reflect.DeepEqual(got, tc.nodes) {
				t.Errorf("nodes %v, want %v", got, tc.nodes)
			}
		})
	}
}

// TestUpdatePayloadReportsNoDropForAHeldNode shows that UpdatePayload reports
// what Merge does, and that it still reports the read drops when the build
// function fails and nothing is written.
func TestUpdatePayloadReportsNoDropForAHeldNode(t *testing.T) {
	for _, tc := range heldRequests() {
		t.Run(tc.name, func(t *testing.T) {
			s := openStoreFor(t, newStoreDir(t), KeyConfig{
				Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
			})
			seed := tc.stored(s.Key())
			if _, err := Update(context.Background(), s, func(*Payload) (*Payload, error) { return seed, nil }); err != nil {
				t.Fatal(err)
			}
			var decision TrustDecision
			result, err := s.UpdatePayload(context.Background(), payloadNow, func(stored Stored) (Change, error) {
				var err error
				decision, err = DecideTrust(stored, s.Key(), tc.obs, stored.Now)
				return decision.Change, err
			})
			if err != nil || !result.Written {
				t.Fatalf("UpdatePayload: %+v, %v", result, err)
			}
			if !reflect.DeepEqual(result.Dropped, tc.drops) {
				t.Errorf("dropped %v, want %v", result.Dropped, tc.drops)
			}
		})
	}

	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	seed := payloadFor(s.Key(), "pve2", "pve3")
	seed.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	if _, err := Update(context.Background(), s, func(*Payload) (*Payload, error) { return seed, nil }); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("build failed")
	result, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{}, failure
	})
	if !errors.Is(err, failure) || result.Written ||
		!reflect.DeepEqual(result.Dropped, []Drop{{Record: "node 1", Reason: DropExpired}}) {
		t.Errorf("a failed build: %+v, %v, want the read drop and no write", result, err)
	}
}

// TestMergeHoldsOnlyAnExpiredOrFutureNode shows that a result carrying a
// stored node with its stored pins and confirmation keeps it only when a
// reader hides it for its time. A node the derivation no longer gives, or one
// whose address is a configured endpoint, is dropped for that reason, because
// the merge is the last guard against a caller that builds its own change.
func TestMergeHoldsOnlyAnExpiredOrFutureNode(t *testing.T) {
	key := trustKey(false)
	old := payloadNow.Add(-48 * time.Hour)
	cases := []struct {
		name   string
		stored string
		move   func(n *DiscoveredNode)
		reason DropReason
	}{
		{"an address the seed does not derive", "pve2", func(n *DiscoveredNode) { n.Address = "pve2.attacker.example" }, DropNotDerived},
		{"a port the seed does not derive", "pve2", func(n *DiscoveredNode) { n.Port = 8007 }, DropNotDerived},
		{"a configured endpoint", "pve1b", func(*DiscoveredNode) {}, DropConfiguredEndpoint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := payloadFor(key, tc.stored)
			stored.Discovery.Nodes[0].ConfirmedAt = old
			result := mergeResult(tc.stored)
			result.Discovery.DiscoveredAt = payloadNow
			result.Discovery.Nodes[0] = stored.Discovery.Nodes[0]
			tc.move(&result.Discovery.Nodes[0])

			next, drops, err := Merge(stored, key, Change{Result: result}, payloadNow)
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Discovery.Nodes) != 0 {
				t.Errorf("the merge wrote %+v", next.Discovery.Nodes)
			}
			if !hasDrop(drops, "node 0", tc.reason) {
				t.Errorf("drops %v, want node 0 dropped as %s", drops, tc.reason)
			}
		})
	}
}

// hasDrop reports whether drops holds a drop of record for reason.
func hasDrop(drops []Drop, record string, reason DropReason) bool {
	for _, drop := range drops {
		if drop.Record == record && drop.Reason == reason {
			return true
		}
	}
	return false
}

// TestAttemptNeverShowsASectionAReaderHides shows that an attempt through
// UpdatePayload leaves a section dated beyond FutureSkew as stored, hidden
// from a reader, while a standing conflict is still recorded.
func TestAttemptNeverShowsASectionAReaderHides(t *testing.T) {
	s := openStoreFor(t, newStoreDir(t), KeyConfig{
		Endpoints: []string{mergeSeed, trustSecond}, VerifySSL: false, User: storeTestUser,
	})
	seed := payloadFor(s.Key(), "pve2")
	seed.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	seed.Discovery.NextRefreshAfter = payloadNow.Add(2 * time.Hour)
	seed.Cluster.IdentityConflict = true
	if _, err := Update(context.Background(), s, func(*Payload) (*Payload, error) { return seed, nil }); err != nil {
		t.Fatal(err)
	}
	d := trustUpdate(t, s, payloadNow, trustObservation(trustSame("pve2")))
	if d.Outcome != TrustConflictStands {
		t.Fatalf("outcome %s", d.Outcome)
	}
	raw := trustStoredFile(t, s)
	if !reflect.DeepEqual(raw.Discovery, seed.Discovery) || !raw.Cluster.IdentityConflict {
		t.Errorf("the file after the attempt:\n got %s\nwant %s", trustDump(raw), trustDump(seed))
	}
	if p, _, err := s.ReadPayload(payloadNow, nil); err != nil || p.Discovery != nil {
		t.Errorf("a reader sees %+v, %v, want the section hidden", p, err)
	}
}
