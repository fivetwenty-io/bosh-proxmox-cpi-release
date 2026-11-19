package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// replicaStampRig is an attach to a replicate VM on pve01 whose pin, with
// comment and nodes, records local-zfs. The new disk is on local-zfs too.
func replicaStampRig(t *testing.T, comment, nodes string) (*attachHA, *attachHAStub, map[string]any) {
	t.Helper()
	deps, stub, profile := attachRig(config.HAModeAuto)
	wait := 60
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
	deps.Config.HA.MotionWaitSec = &wait
	profile.Storages = append(profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	repl := newFakeReplica()
	repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication = repl
	cfg := map[string]any{"scsi0": replicaRootVolume + ",size=10G", "cpu": "x86-64-v3"}
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return cfg, nil
	}}
	strict := true
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes, Strict: &strict}
	disk := resolvedDisk{volid: "local-zfs:vm-100-disk-1", stableID: "abcd1234"}
	h, err := decideAttachHA(context.Background(), deps, "100", disk)
	if err != nil || h == nil || h.action != attachHAGuarded {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	h.narrow = barrierView(&attachRunRes{log: &stagingLog{}, state: "started", comment: stub.comment})
	h.deps = deps
	return h, stub, cfg
}

func pinStamp(t *testing.T, stub *attachHAStub) time.Time {
	t.Helper()
	pin := stub.rules["bosh-na-100"]
	at, ok := ha.ReplicaChanged(derefString(pin.Comment))
	if !ok {
		t.Fatalf("pin comment %q records no change", derefString(pin.Comment))
	}
	return at
}

// A replicate VM's pin must not let a sync verify the VM's volume set while
// an attach changes it. While the attach runs, the pin records a change no
// sync can have started after, and once the disk lands it records when the
// attach finished. Both the widening path of the barrier, where the pin's
// nodes stay as they are, and its narrowing path hold the pin that way.
func TestAttachToAReplicateVMHoldsTheReplicaUnprovenWhileItRuns(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	marker := haIdentity{Owner: aaDirector}.marker()
	cases := map[string]struct{ comment, nodes string }{
		"source-only pin, which the barrier widens": {
			ha.WithReplicaChanged(ha.WithVolumeRecord(marker, []string{"local-zfs"}), old), "pve01",
		},
		"widened pin, which the barrier narrows": {
			ha.WithReplicaChanged(ha.WithReplicaPartner(ha.WithVolumeRecord(marker, []string{"local-zfs"}),
				"pve02"), old), "pve01,pve02",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, stub, cfg := replicaStampRig(t, c.comment, c.nodes)
			var during time.Time
			var inside time.Time
			err := h.run(context.Background(), func(context.Context) (string, error) {
				inside = time.Now()
				during = pinStamp(t, stub)
				cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
				return "pve01", nil
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !during.After(inside) {
				t.Fatalf("the pin recorded the change at %v while the attach ran at %v; a sync that starts "+
					"before the disk lands could prove the old volume set", during, inside)
			}
			if limit := inside.Add(3 * time.Minute).Add(time.Second); during.After(limit) {
				t.Fatalf("the pin recorded the change at %v while the attach ran, past the attach's bound %v",
					during, limit)
			}
			after := pinStamp(t, stub)
			if after.Before(inside.Truncate(time.Second)) || after.After(time.Now()) {
				t.Fatalf("the pin records the change at %v after the attach, want the time it finished, "+
					"between %v and now", after, inside)
			}
			if derefString(stub.rules["bosh-na-100"].Nodes) != "pve01" {
				t.Fatalf("pin nodes %q, want the source alone", derefString(stub.rules["bosh-na-100"].Nodes))
			}
		})
	}
}

// A request deadline earlier than the attach's own bound ends the attach's
// work, and the disk then lands within the landing allowance after it, since
// a PVE task can run on after the call stops waiting, so the pin records
// that instead.
func TestAttachToAReplicateVMRecordsTheRequestDeadlineWhileItRuns(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), old)
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	deps, stub, profile := attachRig(config.HAModeAuto)
	wait := 60
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
	deps.Config.HA.MotionWaitSec = &wait
	profile.Storages = append(profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	repl := newFakeReplica()
	repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication = repl
	nodes, strict := "pve01", true
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes, Strict: &strict}
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return map[string]any{"scsi0": replicaRootVolume + ",size=10G", "cpu": "x86-64-v3"}, nil
	}}
	h, err := decideAttachHA(ctx, deps, "100", resolvedDisk{volid: "local-zfs:vm-100-disk-1", stableID: "ab12"})
	if err != nil || h == nil {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	got, ok := ha.ReplicaChanged(h.newPin.Comment)
	want := deadline.Add(attachReplicaLandingWaits * time.Duration(wait) * time.Second)
	if !ok || got.After(want) || got.Before(want.Truncate(time.Second).Add(-time.Second)) {
		t.Fatalf("new pin records %v, want the request's deadline and the landing allowance, %v", got, want)
	}
}

// The pin is written before the attach under a compare-and-swap. A pin that
// changed since the decision, such as one another call widened, fails the
// attach retriably before it attaches anything.
func TestAttachToAReplicateVMRefusesAPinThatChangedSinceTheDecision(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), old)
	h, stub, _ := replicaStampRig(t, comment, "pve01")
	changed := ha.WithReplicaChanged(comment, old.Add(time.Minute))
	rule := stub.rules["bosh-na-100"]
	rule.Comment = &changed
	stub.rules["bosh-na-100"] = rule
	attached := false
	err := h.run(context.Background(), func(context.Context) (string, error) {
		attached = true
		return "pve01", nil
	})
	if err == nil || attached {
		t.Fatalf("err %v attached %v; a pin that changed since the decision fails the attach before it runs",
			err, attached)
	}
	if !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("err %v, want it to say the pin changed", err)
	}
}

// A sweep that widens the pin between the decision and the hold only adds the
// partner the replica was verified on, so the attach takes the widened pin as
// the one it started from instead of failing. The barrier then narrows the
// pin to the source, recording the latest time the attach can land, before it
// attaches anything, and the pin ends on the source with the time the attach
// finished.
func TestAttachToAReplicateVMNarrowsAPinASweepWidenedSinceTheDecision(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), old)
	h, stub, cfg := replicaStampRig(t, comment, "pve01")
	widened := ha.WithReplicaPartner(comment, "pve02")
	nodes := "pve01,pve02"
	rule := stub.rules["bosh-na-100"]
	rule.Comment, rule.Nodes = &widened, &nodes
	stub.rules["bosh-na-100"] = rule
	var inside, during time.Time
	var duringNodes string
	err := h.run(context.Background(), func(context.Context) (string, error) {
		inside = time.Now()
		during = pinStamp(t, stub)
		duringNodes = derefString(stub.rules["bosh-na-100"].Nodes)
		cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("run: %v; a pin a sweep widened since the decision must not fail the attach", err)
	}
	if duringNodes != "pve01" || !during.After(inside) {
		t.Fatalf("while the attach ran the pin named %q and recorded the change at %v (attach at %v); want the "+
			"source alone and a change no sync can have started after", duringNodes, during, inside)
	}
	pin := stub.rules["bosh-na-100"]
	if partner, _ := ha.ReplicaPartner(derefString(pin.Comment)); derefString(pin.Nodes) != "pve01" || partner != "" {
		t.Fatalf("pin nodes %q comment %q after the attach, want the source alone", derefString(pin.Nodes),
			derefString(pin.Comment))
	}
	if after := pinStamp(t, stub); after.Before(inside.Truncate(time.Second)) || after.After(time.Now()) {
		t.Fatalf("the pin records the change at %v after the attach, want the time it finished", after)
	}
}

// An attach whose steps before the landing ran longer than the land-by time
// it recorded leaves room for records a later one right before the disk
// lands, so the pin's time stays no earlier than the landing. An attach that
// reaches the landing in time writes nothing more.
func TestAttachToAReplicateVMRecordsALaterLandByBeforeALateLanding(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), old)
	h, stub, cfg := replicaStampRig(t, comment, "pve01")
	var held, timely, late, landing time.Time
	err := h.run(context.Background(), func(ctx context.Context) (string, error) {
		held = pinStamp(t, stub)
		if err := beforeAttachLanding(ctx); err != nil {
			return "", err
		}
		timely = pinStamp(t, stub)
		late = time.Now().Add(2 * time.Minute)
		h.clock = func() time.Time { return late }
		if err := beforeAttachLanding(ctx); err != nil {
			return "", err
		}
		landing = pinStamp(t, stub)
		cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !timely.Equal(held) {
		t.Fatalf("a landing in time moved the pin's time from %v to %v; want no write", held, timely)
	}
	if want := late.Add(2 * time.Minute).Truncate(time.Second); landing.Before(want) {
		t.Fatalf("before a late landing at %v the pin records %v; want no earlier than %v, the landing's "+
			"allowance after it", late, landing, want)
	}
	if derefString(stub.rules["bosh-na-100"].Nodes) != "pve01" {
		t.Fatalf("pin nodes %q, want the source alone", derefString(stub.rules["bosh-na-100"].Nodes))
	}
}

// A pass that widened the pin after its land-by time while the attach still
// ran, before the disk landed, is narrowed back to the source with a later
// land-by time before the disk lands, and the attach succeeds.
func TestAttachToAReplicateVMNarrowsAPinWidenedBeforeALateLanding(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	comment := ha.WithReplicaChanged(ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(),
		[]string{"local-zfs"}), old)
	h, stub, cfg := replicaStampRig(t, comment, "pve01")
	var late, landing time.Time
	var landingNodes string
	err := h.run(context.Background(), func(ctx context.Context) (string, error) {
		rule := stub.rules["bosh-na-100"]
		widened := ha.WithReplicaPartner(derefString(rule.Comment), "pve02")
		nodes := "pve01,pve02"
		rule.Comment, rule.Nodes = &widened, &nodes
		stub.rules["bosh-na-100"] = rule
		late = time.Now().Add(2 * time.Minute)
		h.clock = func() time.Time { return late }
		if err := beforeAttachLanding(ctx); err != nil {
			return "", err
		}
		landing = pinStamp(t, stub)
		landingNodes = derefString(stub.rules["bosh-na-100"].Nodes)
		cfg["scsi1"] = "local-zfs:vm-100-disk-1,size=5G"
		return "pve01", nil
	})
	if err != nil {
		t.Fatalf("run: %v; a pin widened before the landing must not fail the attach", err)
	}
	if partner, _ := ha.ReplicaPartner(derefString(stub.rules["bosh-na-100"].Comment)); landingNodes != "pve01" ||
		partner != "" {
		t.Fatalf("before the landing the pin named %q; want the source alone", landingNodes)
	}
	if want := late.Add(2 * time.Minute).Truncate(time.Second); landing.Before(want) {
		t.Fatalf("before the landing the pin records %v; want no earlier than %v", landing, want)
	}
}

// Without a request deadline, the land-by time is the attach's own bound,
// and a deadline earlier than that bound gives the deadline and the landing
// allowance, whichever is earlier.
func TestAttachReplicaLandBy(t *testing.T) {
	deps, _, _ := attachRig(config.HAModeAuto)
	wait := 60
	deps.Config.HA.MotionWaitSec = &wait
	mw := time.Duration(wait) * time.Second
	now := time.Now()
	if got := attachReplicaLandBy(context.Background(), deps, now); !got.Equal(now.Add(attachReplicaLandWaits * mw)) {
		t.Fatalf("no deadline: %v", got)
	}
	near := now.Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), near)
	defer cancel()
	if got := attachReplicaLandBy(ctx, deps, now); !got.Equal(near.Add(attachReplicaLandingWaits * mw)) {
		t.Fatalf("near deadline: %v, want %v", got, near.Add(attachReplicaLandingWaits*mw))
	}
	far := now.Add(time.Hour)
	ctxFar, cancelFar := context.WithDeadline(context.Background(), far)
	defer cancelFar()
	if got := attachReplicaLandBy(ctxFar, deps, now); !got.Equal(now.Add(attachReplicaLandWaits * mw)) {
		t.Fatalf("far deadline: %v", got)
	}
}
