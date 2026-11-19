package ha

import (
	"context"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func TestWithDLBRecordReplacesTheToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		comment string
		dlb     bool
		want    string
	}{
		{stageMarker, true, stageMarker + " bosh-dlb=1"},
		{stageMarker, false, stageMarker + " bosh-dlb=0"},
		{stageMarker + " bosh-dlb=1", false, stageMarker + " bosh-dlb=0"},
		{stageMarker + " bosh-dlb=0 bosh-failback-hold=1000", true, stageMarker + " bosh-failback-hold=1000 bosh-dlb=1"},
	} {
		if got := WithDLBRecord(tc.comment, tc.dlb); got != tc.want {
			t.Errorf("WithDLBRecord(%q, %v) = %q, want %q", tc.comment, tc.dlb, got, tc.want)
		}
	}
}

func TestDLBRecordReadsOnlyAWellFormedToken(t *testing.T) {
	t.Parallel()
	for comment, want := range map[string]struct{ dlb, ok bool }{
		stageMarker:                       {false, false},
		stageMarker + " bosh-dlb=1":       {true, true},
		stageMarker + " bosh-dlb=0":       {false, true},
		stageMarker + " bosh-dlb=":        {false, false},
		stageMarker + " bosh-dlb=yes":     {false, false},
		stageMarker + " bosh-dlb=1 x=y":   {true, true},
		"bosh-dlb=1 " + stageMarker + " ": {true, true},
	} {
		c := comment
		if dlb, ok := DLBRecord(&c); dlb != want.dlb || ok != want.ok {
			t.Errorf("DLBRecord(%q) = %v, %v, want %v, %v", comment, dlb, ok, want.dlb, want.ok)
		}
	}
	if _, ok := DLBRecord(nil); ok {
		t.Error("a nil comment carries no record")
	}
}

// TestInDLBPrefersTheRecordOverTheFlag proves the CPI reads DLB membership
// from its own record, so an operator who turns auto-rebalance on or off in
// the GUI never changes whether the CPI treats the VM as a DLB VM. A resource
// an earlier release wrote, which carries no record, falls back to the flag.
func TestInDLBPrefersTheRecordOverTheFlag(t *testing.T) {
	t.Parallel()
	on, off := sdkclient.PVEBool(true), sdkclient.PVEBool(false)
	keys := keysOf(keysPVE92)
	for name, tc := range map[string]struct {
		comment string
		flag    *sdkclient.PVEBool
		want    bool
	}{
		"record off, flag unset": {stageMarker + " bosh-dlb=0", nil, false},
		"record off, flag on":    {stageMarker + " bosh-dlb=0", &on, false},
		"record on, flag off":    {stageMarker + " bosh-dlb=1", &off, true},
		"no record, flag unset":  {stageMarker, nil, true},
		"no record, flag off":    {stageMarker, &off, false},
	} {
		c := tc.comment
		if got := keys.InDLB(&c, tc.flag); got != tc.want {
			t.Errorf("%s: InDLB %v, want %v", name, got, tc.want)
		}
	}
	c := stageMarker
	if keysOf(keysPVE91).InDLB(&c, nil) {
		t.Error("a release without auto-rebalance must not read a VM without a record as a DLB VM")
	}
}

func TestStageRecordsTheDecisionsDLBMembership(t *testing.T) {
	t.Parallel()
	for _, dlb := range []bool{false, true} {
		rig := &stageRig{}
		d := stageDecision()
		d.DLB = dlb
		if err := rig.stager().Stage(context.Background(), d, stageMarker); err != nil {
			t.Fatal(err)
		}
		c := rig.comment
		if got, ok := DLBRecord(&c); !ok || got != dlb {
			t.Fatalf("dlb=%v: comment %q records %v, %v", dlb, rig.comment, got, ok)
		}
	}
	// A staged resource whose record differs gets the decision's record.
	rig := &stageRig{exists: true, state: haStateIgnored, comment: stageMarker + " bosh-dlb=1",
		failback: pveBool(false), rebalance: pveBool(false)}
	if err := rig.stager().Stage(context.Background(), stageDecision(), stageMarker); err != nil {
		t.Fatal(err)
	}
	if rig.comment != stageMarker+" bosh-dlb=0" {
		t.Fatalf("comment %q, want the record of a VM outside the DLB", rig.comment)
	}
}

// TestReregisterSnapshotReadsTheDLBRecord proves a re-registration keeps the
// DLB membership the CPI recorded, whatever the operator set the flag to.
func TestReregisterSnapshotReadsTheDLBRecord(t *testing.T) {
	r := newReaddRig()
	r.comment = readdMarker + " bosh-dlb=0"
	r.rebalance = true
	var got ReaddRecord
	in := r.input()
	write := in.WriteIntent
	in.WriteIntent = func(ctx context.Context, node, tag string, rec ReaddRecord) error {
		got = rec
		return write(ctx, node, tag, rec)
	}
	res, err := r.run(t, in)
	r.registeredAgain(t, res, err)
	if got.AutoRebalance {
		t.Fatalf("the resource records a VM outside the DLB, so the record must too: %+v", got)
	}
}
