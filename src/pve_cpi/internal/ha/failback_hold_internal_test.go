package ha

import (
	"context"
	"errors"
	"slices"
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
