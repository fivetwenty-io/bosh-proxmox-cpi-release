package handlers

import (
	"errors"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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

// The sweep turns failback off only once the widening goes ahead. A guard
// that skips the VM, or a pin the rule writer finds changed since the check
// read it, leaves the pin on the source, and the HA resource gets no write.
func TestReplicaWidenPass_SkippedWideningWritesNoFailback(t *testing.T) {
	cases := map[string]func(s *sweepRig){
		"the guard finds the volumes changed": func(s *sweepRig) {
			s.onClaimed = func() { s.cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G" }
		},
		"the guard finds a PVE lock": func(s *sweepRig) {
			s.onClaimed = func() { s.cfg["lock"] = "migrate" }
		},
		"the pin changed since the check read it": func(s *sweepRig) {
			restamped := time.Now().Truncate(time.Second)
			s.hooked.onStatus = func(call int) {
				if call == 2 {
					s.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, restamped) })
				}
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSweepRig(t)
			on := sdkclient.PVEBool(true)
			s.stub.failback = &on
			writes := 0
			s.stub.onFailbackWrite = func(bool) { writes++ }
			setup(s)
			res := onlyResult(t, s.run(t, ReplicaWidenOptions{}))
			if res.Outcome == ReplicaWidened || s.widened() {
				t.Fatalf("result %+v, pin %q; want the source pin kept", res, s.pinNodes())
			}
			if writes != 0 || s.stub.failback == nil || !bool(*s.stub.failback) {
				t.Fatalf("failback writes %d; want none for a widening that did not go ahead", writes)
			}
		})
	}
}
