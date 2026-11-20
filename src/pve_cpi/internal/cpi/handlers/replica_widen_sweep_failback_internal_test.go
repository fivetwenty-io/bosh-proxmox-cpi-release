package handlers

import (
	"errors"
	"strings"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// The sweep turns failback off on the VM's HA resource before the widened pin
// names the partner, as the touch path does, so PVE never holds a replicate VM
// with failback on under a pin that lets it fail back to its old source and
// reverse the replication job.
func TestReplicaWidenPass_TurnsFailbackOffBeforeThePinNamesThePartner(t *testing.T) {
	s := newSweepRig(t)
	on := sdkclient.PVEBool(true)
	s.stub.failback = &on
	offWrites, partnerAtOff := 0, false
	s.stub.onFailbackWrite = func(v bool) {
		if !v {
			offWrites++
			partnerAtOff = partnerAtOff || s.widened()
		}
	}
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidened || !s.widened() {
		t.Fatalf("result %+v, pin %q; want the pin widened", res, s.pinNodes())
	}
	if s.stub.failback == nil || bool(*s.stub.failback) {
		t.Fatal("failback reads on after the widening, want off")
	}
	if offWrites != 1 || partnerAtOff {
		t.Fatalf("failback off writes %d, pin named the partner at the write %v; want one write before the pin "+
			"names the partner", offWrites, partnerAtOff)
	}
}

// A failback write that fails leaves the pin on the source, so the sweep never
// widens a pin while the flag may still be on.
func TestReplicaWidenPass_FailbackWriteThatFailsKeepsTheSourcePin(t *testing.T) {
	s := newSweepRig(t)
	on := sdkclient.PVEBool(true)
	s.stub.failback = &on
	s.stub.failbackErr = errors.New("HTTP 500: unable to write resources.cfg")
	res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
	if res.Outcome != ReplicaWidenFailed || !strings.Contains(res.Reason, "failback") || s.widened() {
		t.Fatalf("result %+v, pin %q; want a failure that keeps the source pin", res, s.pinNodes())
	}
	if s.pools.held(100) {
		t.Fatal("the pass kept the VM's claim after the failback write failed")
	}
}
