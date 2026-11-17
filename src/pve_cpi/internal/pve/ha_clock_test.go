package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// shutdownClockErr is what a node answers to GET /nodes/{node}/time once
// systemd has begun shutting it down: pveproxy still answers, but timedatectl
// exits 1, so the call fails with a 500 that carries the command's failure.
func shutdownClockErr() error {
	return sdkerrors.ParseAPIError(500,
		[]byte(`{"data":null,"message":"command 'timedatectl show '--property=Timezone' --value' failed: exit code 1\n"}`))
}

// haManagerBodyMaster is haManagerBody with the CRM master named master.
func haManagerBodyMaster(
	crmTS int64, served, master string, nodeStatus map[string]string, lrm map[string]map[string]any,
	services map[string]any,
) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{
			"master_node": master, "node_status": nodeStatus, "service_status": services, "timestamp": crmTS,
		},
		"lrm_status": lrm,
		"quorum":     map[string]any{"node": served, "quorate": "1"},
	})
	return raw
}

// fastBackoff is a test context whose retry backoff is a millisecond, so a
// failing assertion costs milliseconds and not the real backoff curve.
func fastBackoff(t *testing.T) context.Context {
	t.Helper()
	return pve.WithTestBackoff(t.Context(), func(int) time.Duration { return time.Millisecond })
}

// TestNodeHealth_EntryClockServerErrorUsesPeerClock covers an entry node that
// is shutting down. Its API answers and the cluster-scoped reads work, but its
// own clock read fails with a 500. The HA view stays readable on another
// member's clock, and the entry node's clock is read once, not retried.
func TestNodeHealth_EntryClockServerErrorUsesPeerClock(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}
	ctx := fastBackoff(t)

	r, err := pve.NodeHealth(ctx, c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewManagerStatus {
		t.Fatalf("HA = %q (%s); want the manager status trusted on a peer's clock", r.HA, r.HAReason)
	}
	if r.EntryNode != "pve1" || r.ClockNode != "pve2" {
		t.Errorf("entry %q clock %q; want entry pve1 and clock pve2", r.EntryNode, r.ClockNode)
	}
	if !strings.Contains(r.ClockFallback, "timedatectl") || !strings.Contains(r.ClockFallback, "pve2") {
		t.Errorf("ClockFallback = %q; want the entry's failure and the peer named", r.ClockFallback)
	}
	if want := []string{"pve1", "pve2"}; !slices.Equal(n.asked, want) {
		t.Errorf("clock reads = %v; want %v", n.asked, want)
	}
}

// TestReadHAServiceState_EntryClockServerErrorStaysKnown is the delete_vm read
// that deferred while its entry node was shutting down. It must now be known,
// through the same entry node, with the timestamps measured on a peer's clock.
func TestReadHAServiceState_EntryClockServerErrorStaysKnown(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}

	st, err := pve.ReadHAServiceState(fastBackoff(t), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Known {
		t.Fatalf("the model must be known on a peer's clock: %s", st.Reason)
	}
	if st.EntryNode != "pve1" || st.ClockNode != "pve2" || st.EntryTime != nhNow || !st.CRMPresent {
		t.Errorf("state %+v; want entry pve1, clock pve2, and the peer's time", st)
	}
	haWant(t, st, pve.HAProceed)
}

// TestReadHAServiceState_PeerClockPrefersCRMMaster checks that the CRM master,
// whose clock wrote the CRM timestamp, is the first peer the read asks.
func TestReadHAServiceState_PeerClockPrefersCRMMaster(t *testing.T) {
	t.Parallel()
	s := newHAScene("started")
	c := s.cluster()
	c.manager = haManagerBodyMaster(s.crmTS, "pve1", "pve3", s.nodes, s.lrm, map[string]any{
		"vm:100": haSvc("started", "pve2", "u-1"), "vm:200": haSvc("started", "pve3", "other"),
	})
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}

	st, err := pve.ReadHAServiceState(fastBackoff(t), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Known || st.ClockNode != "pve3" {
		t.Fatalf("known %v clock %q (%s); want the master pve3's clock", st.Known, st.ClockNode, st.Reason)
	}
	for _, asked := range n.asked {
		if asked == "pve2" {
			t.Errorf("clock reads = %v; the master must answer before any other peer is asked", n.asked)
		}
	}
}

// TestReadHAServiceState_PeerClockSkipsOfflineMember checks that a member that
// cluster status lists offline is never asked for its clock.
func TestReadHAServiceState_PeerClockSkipsOfflineMember(t *testing.T) {
	t.Parallel()
	s := newHAScene("started")
	s.rows = []json.RawMessage{
		nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 0, false, ""), nhNode("pve3", 1, false, ""),
	}
	s.svcNode = "pve3"
	c := s.cluster()
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}
	ctx := fastBackoff(t)

	r, err := pve.NodeHealth(ctx, c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ClockNode != "pve3" {
		t.Fatalf("clock %q (%s %s); want pve3", r.ClockNode, r.HA, r.HAReason)
	}
	if slices.Contains(n.asked, "pve2") {
		t.Errorf("clock reads = %v; the offline pve2 must not be asked", n.asked)
	}
}

// TestHAClock_OnlyServerErrorsBorrowAPeerClock checks the failures that must
// keep the HA view unreadable as before: a failure to reach the entry node, a
// 4xx, and an answer with no clock never move the clock read to a peer.
func TestHAClock_OnlyServerErrorsBorrowAPeerClock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"connection refused", errors.New("dial tcp 10.0.0.1:8006: connect: connection refused")},
		{"permission denied", &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"}},
		{"bad request", &sdkerrors.APIError{HTTPCode: 400, Message: "bad node name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newHAScene("started").cluster()
			n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": tc.err}}
			r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if r.HA != pve.HAViewUnreadable || r.ClockNode != "" {
				t.Fatalf("HA %q clock %q; want unreadable with no clock", r.HA, r.ClockNode)
			}
			for _, asked := range n.asked {
				if asked != "pve1" {
					t.Errorf("clock reads = %v; only the entry node may be asked", n.asked)
				}
			}
		})
	}
}

// TestHAClock_EveryPeerFailsStaysUnreadable checks that a broken entry node
// whose peers cannot answer either still leaves the view unreadable, with a
// reason that names the entry's failure and each peer's.
func TestHAClock_EveryPeerFailsStaysUnreadable(t *testing.T) {
	t.Parallel()
	c := newHAScene("started").cluster()
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{
		"pve1": shutdownClockErr(),
		"pve2": &sdkerrors.APIError{HTTPCode: 595, Message: "no route to host"},
		"pve3": shutdownClockErr(),
	}}
	r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("HA = %q; want unreadable", r.HA)
	}
	for _, want := range []string{"entry node clock unreadable", "timedatectl", "pve2", "pve3"} {
		if !strings.Contains(r.HAReason, want) {
			t.Errorf("reason %q lacks %q", r.HAReason, want)
		}
	}
	if want := []string{"pve1", "pve2", "pve3"}; !slices.Equal(n.asked, want) {
		t.Errorf("clock reads = %v; want one read each, %v", n.asked, want)
	}
}

// TestHAClock_EntrySeeingNoOnlinePeerBorrowsNoClock is the minority-partition
// guard. An entry node that sees every other member offline has no peer whose
// clock it could borrow, so the delete's HA read stays unknown however its own
// clock fails. The offline members would answer if they were asked, so a read
// that asked them would turn the model known. The first case also reports the
// cluster as not quorate, which stops the read before any clock, and the
// second reports it quorate, so only the choice of peers keeps the read unknown.
func TestHAClock_EntrySeeingNoOnlinePeerBorrowsNoClock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		quorate int
	}{
		{"not quorate", 0},
		{"quorate but every peer offline", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newHAScene("started")
			s.rows = []json.RawMessage{
				nhClusterRow(tc.quorate), nhNode("pve1", 1, true, ""), nhNode("pve2", 0, false, ""), nhNode("pve3", 0, false, ""),
			}
			c := s.cluster()
			n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}

			st, err := pve.ReadHAServiceState(fastBackoff(t), c, n,
				pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive})
			if err != nil {
				t.Fatal(err)
			}
			if st.Known {
				t.Fatalf("an entry node with no online peer must leave the model unknown: %+v", st)
			}
			for _, asked := range n.asked {
				if asked != "pve1" {
					t.Errorf("clock reads = %v; the entry must not ask a member that is offline", n.asked)
				}
			}
		})
	}
}

// TestHAClock_StandaloneEntryKeepsRetrying checks that an entry node with no
// online peer retries a server error as before, so a standalone host whose
// clock read fails once still gets its retries.
func TestHAClock_StandaloneEntryKeepsRetrying(t *testing.T) {
	t.Parallel()
	s := newHAScene("started")
	s.rows = []json.RawMessage{nhNode("pve1", 1, true, "")}
	c := s.cluster()
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}
	ctx := fastBackoff(t)

	r, err := pve.NodeHealth(ctx, c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("HA = %q; want unreadable", r.HA)
	}
	if len(n.asked) < 2 {
		t.Errorf("clock reads = %v; want the server error retried on the entry node", n.asked)
	}
}

// borrowedScene is a scene whose entry node pve1 cannot read its own clock, so
// every HA read borrows another member's clock.
func borrowedScene(t *testing.T, mutate func(s *haScene), nodeNow map[string]int64) (*haCluster, *nhNodes) {
	t.Helper()
	s := newHAScene("started")
	if mutate != nil {
		mutate(s)
	}
	return s.cluster(), &nhNodes{now: nhNow, nodeNow: nodeNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}
}

// TestHAClock_BorrowedClockRejectsStaleCRM checks that a borrowed clock still
// measures the age of the CRM timestamp. The CRM master died, so its last round
// is 95 seconds old on the peer's clock, and the entry node's own clock would
// have read it fresh. Both readers must refuse it.
func TestHAClock_BorrowedClockRejectsStaleCRM(t *testing.T) {
	t.Parallel()
	c, n := borrowedScene(t, nil, map[string]int64{"pve2": nhNow + 90})

	r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewStale || r.ClockNode != "pve2" {
		t.Fatalf("HA %q clock %q (%s); want a stale view measured on pve2", r.HA, r.ClockNode, r.HAReason)
	}
	if !strings.Contains(r.HAReason, "on node pve2's clock") {
		t.Errorf("reason %q must say the age was measured on pve2's clock", r.HAReason)
	}

	st, err := pve.ReadHAServiceState(fastBackoff(t), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive})
	if err != nil {
		t.Fatal(err)
	}
	if st.Known {
		t.Fatalf("a stale CRM on a borrowed clock must leave the model unknown: %+v", st)
	}
	haWant(t, st, pve.HAUnknown)
}

// TestHAClock_BorrowedClockRejectsStaleLRM checks that a borrowed clock still
// measures the age of an LRM timestamp. The CRM is fresh on the peer's clock,
// and the LRM of the VM's node is 45 seconds old on it, though the entry node's
// clock would have read it fresh.
func TestHAClock_BorrowedClockRejectsStaleLRM(t *testing.T) {
	t.Parallel()
	c, n := borrowedScene(t, nil, map[string]int64{"pve2": nhNow + 43})

	st, err := pve.ReadHAServiceState(fastBackoff(t), c, n,
		pve.HAServiceStateInput{VMID: 100, VMNode: "pve2", Kind: pve.HADecisionLive})
	if err != nil {
		t.Fatal(err)
	}
	if st.ClockNode != "pve2" || !st.CRMPresent {
		t.Fatalf("state %+v; want a live CRM measured on pve2", st)
	}
	if st.LRMFresh {
		t.Fatalf("an LRM 45 seconds old on the borrowed clock must not be fresh: %+v", st)
	}
	haWant(t, st, pve.HAUnknown)
}

// TestHAClock_MasterClockDecidesTheVerdict shows why the CRM master is asked
// first. The master wrote the CRM timestamp on its own clock, so its clock reads
// the timestamp as fresh. pve2 sorts first and runs 70 seconds ahead, so it
// would read the same timestamp as stale.
func TestHAClock_MasterClockDecidesTheVerdict(t *testing.T) {
	t.Parallel()
	nodeNow := map[string]int64{"pve2": nhNow + 70}

	t.Run("master first", func(t *testing.T) {
		t.Parallel()
		s := newHAScene("started")
		c := s.cluster()
		c.manager = haManagerBodyMaster(s.crmTS, "pve1", "pve3", s.nodes, s.lrm, map[string]any{
			"vm:100": haSvc("started", "pve2", "u-1"), "vm:200": haSvc("started", "pve3", "other"),
		})
		n := &nhNodes{now: nhNow, nodeNow: nodeNow, nodeErr: map[string]error{"pve1": shutdownClockErr()}}

		r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if r.HA != pve.HAViewManagerStatus || r.ClockNode != "pve3" {
			t.Fatalf("HA %q clock %q (%s); want a fresh view on the master pve3's clock", r.HA, r.ClockNode, r.HAReason)
		}
	})

	t.Run("peer first", func(t *testing.T) {
		t.Parallel()
		// With the entry node as the master there is no master peer, so pve2
		// answers first and its clock reads the same timestamp as stale.
		c, n := borrowedScene(t, nil, nodeNow)
		r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if r.HA != pve.HAViewStale || r.ClockNode != "pve2" {
			t.Fatalf("HA %q clock %q (%s); want a stale view on pve2's clock", r.HA, r.ClockNode, r.HAReason)
		}
	})
}

// TestNodeHealth_FailingEntryIsNotPlaceable checks that an entry node whose own
// clock read failed takes no new work, since the failure is how a node looks
// while it shuts down, and that it stays a proxy target.
func TestNodeHealth_FailingEntryIsNotPlaceable(t *testing.T) {
	t.Parallel()
	c, n := borrowedScene(t, nil, nil)
	r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ClockFallback == "" {
		t.Fatalf("the clock must have been borrowed: %+v", r)
	}
	if r.Placeable("pve1") {
		t.Error("the entry node whose clock read failed must not be placeable")
	}
	if !r.ProxyTarget("pve1") {
		t.Error("the entry node must stay a proxy target")
	}
	if !r.Placeable("pve2") || !r.Placeable("pve3") {
		t.Error("the other members must stay placeable")
	}

	healthy, err := pve.NodeHealth(fastBackoff(t), newHAScene("started").cluster(), &nhNodes{now: nhNow},
		pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !healthy.Placeable("pve1") {
		t.Error("a healthy entry node must stay placeable")
	}
}

// TestHAClock_HungPeerIsCutOff checks that the walk over peers has a time
// limit. The first peer accepts the call and never answers, so the walk must
// end when its limit does, and must not move on to the next peer.
func TestHAClock_HungPeerIsCutOff(t *testing.T) {
	t.Parallel()
	c, n := borrowedScene(t, nil, nil)
	n.hang = map[string]bool{"pve2": true}
	ctx := pve.WithTestHAClockWalkLimit(fastBackoff(t), 20*time.Millisecond)

	r, err := pve.NodeHealth(ctx, c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("HA = %q (%s); want unreadable when the walk runs out of time", r.HA, r.HAReason)
	}
	if want := []string{"pve1", "pve2"}; !slices.Equal(n.asked, want) {
		t.Errorf("clock reads = %v; want the walk to stop at the hung peer, %v", n.asked, want)
	}
}

// TestHAClock_WalkAsksTheMasterAndTwoPeers checks that a large cluster whose
// peers all fail costs the entry's read, the master, and two other peers.
func TestHAClock_WalkAsksTheMasterAndTwoPeers(t *testing.T) {
	t.Parallel()
	s := newHAScene("started")
	s.rows = []json.RawMessage{
		nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
		nhNode("pve4", 1, false, ""), nhNode("pve5", 1, false, ""),
	}
	c := s.cluster()
	c.manager = haManagerBodyMaster(s.crmTS, "pve1", "pve5", s.nodes, s.lrm, map[string]any{
		"vm:100": haSvc("started", "pve2", "u-1"), "vm:200": haSvc("started", "pve3", "other"),
	})
	n := &nhNodes{now: nhNow, nodeErr: map[string]error{
		"pve1": shutdownClockErr(), "pve2": shutdownClockErr(), "pve3": shutdownClockErr(),
		"pve4": shutdownClockErr(), "pve5": shutdownClockErr(),
	}}

	r, err := pve.NodeHealth(fastBackoff(t), c, n, pve.NodeHealthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.HA != pve.HAViewUnreadable {
		t.Fatalf("HA = %q; want unreadable", r.HA)
	}
	if want := []string{"pve1", "pve5", "pve2", "pve3"}; !slices.Equal(n.asked, want) {
		t.Errorf("clock reads = %v; want the entry, the master, and two peers, %v", n.asked, want)
	}
}
