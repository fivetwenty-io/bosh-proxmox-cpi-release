package ha

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWithFailbackHoldReplacesAnEarlierToken(t *testing.T) {
	t.Parallel()
	at := time.Unix(2000, 0)
	got := WithFailbackHold("bosh-cpi:v1 owner=us "+FailbackHoldKey+"1000 group=g", at)
	if want := "bosh-cpi:v1 owner=us group=g " + FailbackHoldKey + "2000"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	stripped, had := StripFailbackHold(got)
	if !had || stripped != "bosh-cpi:v1 owner=us group=g" {
		t.Fatalf("stripped %q had %v", stripped, had)
	}
	if again, had := StripFailbackHold(stripped); had || again != stripped {
		t.Fatalf("a comment without a token changed: %q had %v", again, had)
	}
}

func TestFailbackHeldOnlyWhileTheTokenIsFresh(t *testing.T) {
	t.Parallel()
	now := time.Unix(100000, 0)
	cases := map[string]struct {
		comment string
		held    bool
	}{
		"no token":                {"bosh-cpi:v1 owner=us", false},
		"a fresh token":           {WithFailbackHold("bosh-cpi:v1 owner=us", now.Add(-time.Minute)), true},
		"an expired token":        {WithFailbackHold("bosh-cpi:v1 owner=us", now.Add(-FailbackHoldTTL)), false},
		"a token far ahead":       {WithFailbackHold("bosh-cpi:v1 owner=us", now.Add(FailbackHoldTTL)), false},
		"a token a little ahead":  {WithFailbackHold("bosh-cpi:v1 owner=us", now.Add(time.Minute)), true},
		"a token that is no time": {"bosh-cpi:v1 owner=us " + FailbackHoldKey + "soon", false},
		"an empty token":          {"bosh-cpi:v1 owner=us " + FailbackHoldKey, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := FailbackHeld(&c.comment, now); got != c.held {
				t.Fatalf("held %v, want %v", got, c.held)
			}
		})
	}
	if FailbackHeld(nil, now) {
		t.Fatal("a nil comment is held")
	}
}

func TestHoldFailbackWritesTheTokenAndFailbackOffInOneWrite(t *testing.T) {
	t.Parallel()
	// Failback already reads off, and the hold still writes, because the
	// write is what changes the digest an earlier read raises against.
	rig := &stageRig{exists: true, state: "started", comment: stageMarker, failback: pveBool(false)}
	at := time.Unix(5000, 0)
	tok, err := HoldFailback(context.Background(), rig, "vm:101", at, ownedMarker)
	if err != nil {
		t.Fatal(err)
	}
	if tok != FailbackHoldKey+"5000" || rig.comment != stageMarker+" "+tok || rig.failback.Bool() {
		t.Fatalf("token %q comment %q failback %v", tok, rig.comment, rig.failback.Bool())
	}
	if !slices.Equal(rig.events, []string{"get", "update:-"}) {
		t.Fatalf("events %v, want one read and one write", rig.events)
	}
}

func TestHoldFailbackLeavesResourcesItMustNotTouch(t *testing.T) {
	t.Parallel()
	for name, rig := range map[string]*stageRig{
		"a missing resource": {},
		"another Director's": {exists: true, state: "started", comment: "bosh-cpi:v1 owner=them"},
	} {
		tok, err := HoldFailback(context.Background(), rig, "vm:101", time.Unix(1, 0), ownedMarker)
		if err != nil || tok != "" || slices.Contains(rig.events, "update:-") {
			t.Fatalf("%s: token %q err %v events %v", name, tok, err, rig.events)
		}
	}
	rig := &stageRig{exists: true, state: "started", comment: stageMarker, updateErr: errors.New("cfs lock")}
	if _, err := HoldFailback(context.Background(), rig, "vm:101", time.Unix(1, 0), ownedMarker); err == nil {
		t.Fatal("a write that fails returned no error")
	}
}

func TestReleaseFailbackHoldDropsOnlyItsOwnToken(t *testing.T) {
	t.Parallel()
	mine := FailbackHoldKey + "5000"
	rig := &stageRig{exists: true, state: "started", comment: stageMarker + " " + mine, failback: pveBool(false)}
	released, err := ReleaseFailbackHold(context.Background(), rig, "vm:101", mine, ownedMarker)
	if err != nil || !released || rig.comment != stageMarker || rig.failback.Bool() {
		t.Fatalf("released %v err %v comment %q failback %v", released, err, rig.comment, rig.failback.Bool())
	}
	later := stageMarker + " " + FailbackHoldKey + "6000"
	rig = &stageRig{exists: true, state: "started", comment: later}
	released, err = ReleaseFailbackHold(context.Background(), rig, "vm:101", mine, ownedMarker)
	if err != nil || released || rig.comment != later {
		t.Fatalf("a later hold was dropped: released %v err %v comment %q", released, err, rig.comment)
	}
}

// The hold names the digest of the read it changed the comment from, so a
// token another writer adds between that read and the write is never lost.
// A refused write is read and merged again.
func TestHoldFailbackComparesTheDigest(t *testing.T) {
	t.Parallel()
	rig := &stageRig{exists: true, state: "started", comment: stageMarker, failback: pveBool(true)}
	rig.beforeUpdate = func(n int) {
		if n == 1 {
			rig.comment += " bosh-rebalance-to=c"
			rig.version++
		}
	}
	tok, err := HoldFailback(context.Background(), rig, "vm:101", time.Unix(5000, 0), ownedMarker)
	if err != nil {
		t.Fatal(err)
	}
	if want := stageMarker + " bosh-rebalance-to=c " + tok; tok == "" || rig.comment != want || rig.failback.Bool() {
		t.Fatalf("token %q comment %q failback %v, want comment %q", tok, rig.comment, rig.failback.Bool(), want)
	}
	if want := []string{"get", "update:-", "get", "update:-"}; !slices.Equal(rig.events, want) {
		t.Fatalf("events %v, want %v", rig.events, want)
	}
}

// A hold whose every write is refused, or whose read names no digest, never
// writes the comment unchecked. It turns failback off with a write of the
// flag alone, which leaves the comment as it is, and fails, so the attach
// asks for no move without the hold.
func TestHoldFailbackThatCannotCompareTurnsFailbackOffAndFails(t *testing.T) {
	t.Parallel()
	start := stageMarker + " bosh-rebalance-to=c"
	busy := &stageRig{exists: true, state: "started", comment: start, failback: pveBool(true)}
	busy.beforeUpdate = func(int) { busy.version++ }
	tok, err := HoldFailback(context.Background(), busy, "vm:101", time.Unix(5000, 0), ownedMarker)
	if err == nil || tok != "" || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("token %q err %v; want a refusal", tok, err)
	}
	if busy.comment != start || busy.failback.Bool() || busy.updates != CommentWriteAttempts+1 {
		t.Fatalf("comment %q failback %v after %d updates; want the comment kept and failback off", busy.comment,
			busy.failback.Bool(), busy.updates)
	}

	blind := &stageRig{exists: true, state: "started", comment: start, failback: pveBool(true), noDigest: true}
	tok, err = HoldFailback(context.Background(), blind, "vm:101", time.Unix(5000, 0), ownedMarker)
	if err == nil || tok != "" || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("token %q err %v; want a refusal", tok, err)
	}
	if blind.comment != start || blind.failback.Bool() || blind.updates != 1 {
		t.Fatalf("comment %q failback %v after %d updates; want one write of failback off alone", blind.comment,
			blind.failback.Bool(), blind.updates)
	}
}

// The release names the digest too. A refused write is read again, and a
// token another writer added stays, while a later hold that replaced this one
// is left in place.
func TestReleaseFailbackHoldComparesTheDigest(t *testing.T) {
	t.Parallel()
	mine := FailbackHoldKey + "5000"
	rig := &stageRig{exists: true, state: "started", comment: stageMarker + " " + mine, failback: pveBool(false)}
	rig.beforeUpdate = func(n int) {
		if n == 1 {
			rig.comment += " bosh-rebalance-to=c"
			rig.version++
		}
	}
	released, err := ReleaseFailbackHold(context.Background(), rig, "vm:101", mine, ownedMarker)
	if err != nil || !released || rig.comment != stageMarker+" bosh-rebalance-to=c" {
		t.Fatalf("released %v err %v comment %q", released, err, rig.comment)
	}

	later := stageMarker + " " + FailbackHoldKey + "6000"
	rig = &stageRig{exists: true, state: "started", comment: stageMarker + " " + mine}
	rig.beforeUpdate = func(n int) {
		if n == 1 {
			rig.comment = later
			rig.version++
		}
	}
	released, err = ReleaseFailbackHold(context.Background(), rig, "vm:101", mine, ownedMarker)
	if err != nil || released || rig.comment != later {
		t.Fatalf("a later hold was dropped: released %v err %v comment %q", released, err, rig.comment)
	}
}

// A release whose every write is refused, or whose read names no digest,
// fails and leaves the hold in place, so failback stays off until the hold
// expires.
func TestReleaseFailbackHoldThatCannotCompareKeepsTheHold(t *testing.T) {
	t.Parallel()
	mine := FailbackHoldKey + "5000"
	start := stageMarker + " " + mine + " bosh-rebalance-to=c"
	busy := &stageRig{exists: true, state: "started", comment: start, failback: pveBool(false)}
	busy.beforeUpdate = func(int) { busy.version++ }
	released, err := ReleaseFailbackHold(context.Background(), busy, "vm:101", mine, ownedMarker)
	if err == nil || released || busy.comment != start || busy.updates != CommentWriteAttempts ||
		!strings.Contains(err.Error(), "changed") {
		t.Fatalf("released %v err %v comment %q updates %d", released, err, busy.comment, busy.updates)
	}

	blind := &stageRig{exists: true, state: "started", comment: start, failback: pveBool(false), noDigest: true}
	released, err = ReleaseFailbackHold(context.Background(), blind, "vm:101", mine, ownedMarker)
	if err == nil || released || blind.comment != start || blind.updates != 0 ||
		!strings.Contains(err.Error(), "digest") {
		t.Fatalf("released %v err %v comment %q updates %d", released, err, blind.comment, blind.updates)
	}
}
