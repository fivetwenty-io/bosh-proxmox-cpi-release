package handlers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// pveEncodedMarker returns comment as PVE's API returns it after write_config
// encoded it rounds more times than its one read decodes. One round is also
// the form a healthy replication.cfg holds.
func pveEncodedMarker(comment string, rounds int) string {
	enc := strings.NewReplacer("%", "%25", ":", "%3A")
	for range rounds {
		comment = enc.Replace(comment)
	}
	return comment
}

func TestParseReplicaJobMarkerReadsRepeatedEncoding(t *testing.T) {
	t.Parallel()
	canonical := renderReplicaJobMarker(replicaOwner, 3)
	for name, comment := range map[string]string{
		"one round":                   pveEncodedMarker(canonical, 1),
		"two rounds, the API form":    pveEncodedMarker(canonical, 2),
		"three rounds, the file form": pveEncodedMarker(canonical, 3),
		"lab API form":                "bosh-cpi%253Av1 owner=" + replicaOwner + " gen=3",
		"lab file form":               "bosh-cpi%25253Av1 owner=" + replicaOwner + " gen=3",
		"encoded with an unknown key": pveEncodedMarker(canonical+" note=kept", 2),
	} {
		marker, ok := parseReplicaJobMarker(comment)
		if !ok || marker.Owner != replicaOwner || marker.Generation != 3 || !marker.Encoded {
			t.Errorf("%s: parseReplicaJobMarker(%q) = %+v, %v; want owner %s gen 3 encoded", name, comment, marker,
				ok, replicaOwner)
		}
	}
	if marker, ok := parseReplicaJobMarker(canonical); !ok || marker.Encoded {
		t.Errorf("parseReplicaJobMarker(%q) = %+v, %v; want a canonical marker", canonical, marker, ok)
	}
	for _, bad := range []string{
		"bosh-cpi%3Av2 owner=x gen=1",
		"bosh-cpi%253Av1 gen=1",
		"bosh-cpi%253Av1 owner=x",
		"bosh-cpi%253Av1 owner=x gen=0",
		"bosh-cpi%3Av1 owner=x gen=1 gen=2",
		"replicated 100% bosh-cpi:v1 owner=x gen=1",
		"bosh%2Dcpi%3Av1 owner=x gen=1",
		"bosh-cpi%3Av1%25 owner=x gen=1",
	} {
		if marker, ok := parseReplicaJobMarker(bad); ok {
			t.Errorf("%q parsed as %+v", bad, marker)
		}
	}
}

func TestReplicaJobOwnedAcceptsAnEncodedMarkerOfThisDirectorOnly(t *testing.T) {
	t.Parallel()
	jobs := []ha.ReplicaJobState{
		{ID: "100-0", Guest: 100, JobNum: 0, Target: "pve02",
			Comment: pveEncodedMarker(renderReplicaJobMarker(aaDirector, 1), 2)},
		{ID: "100-1", Guest: 100, JobNum: 1, Target: "pve03",
			Comment: pveEncodedMarker(renderReplicaJobMarker(aaOtherDirector, 1), 2)},
	}
	owned, foreign, err := guestReplicaJobs(jobs, 100, aaDirector)
	if err != nil || owned == nil || owned.ID != "100-0" || len(foreign) != 1 || foreign[0].ID != "100-1" {
		t.Fatalf("owned %+v foreign %+v err %v; want 100-0 owned and 100-1 foreign", owned, foreign, err)
	}
}

func TestParseHaMarkerReadsRepeatedEncoding(t *testing.T) {
	t.Parallel()
	canonical := haIdentity{Owner: aaDirector, Deployment: "dep", Group: "web"}.marker()
	for rounds := 1; rounds <= 3; rounds++ {
		comment := pveEncodedMarker(canonical, rounds)
		m, ok := parseHaMarker(comment)
		if !ok || m.Owner != aaDirector || m.Deployment != "dep" || m.Group != "web" {
			t.Errorf("parseHaMarker(%q) = %+v, %v", comment, m, ok)
		}
		if got := haOwnershipOf(comment, aaDirector); got != haOwnedByUs {
			t.Errorf("haOwnershipOf(%q) = %v, want owned by us", comment, got)
		}
	}
	if _, ok := parseHaMarker("bosh-cpi%3Av2 owner=" + aaDirector); ok {
		t.Error("an encoded marker of another version must not parse")
	}
}

// ownEncodedJob gives VM 100 this Director's job at gen with its comment as
// PVE lists it after encoding it rounds more times than it decodes.
func (r *replicaRig) ownEncodedJob(gen uint64, rounds int) {
	r.repl.addJob("100-0", "pve01", "pve02", pveEncodedMarker(renderReplicaJobMarker(aaDirector, gen), rounds))
}

func TestRetireTornDownReplicaRetiresAnEncodedJob(t *testing.T) {
	tagged := map[string]any{"tags": "bosh-cpi;" + haReplicaTag, "scsi0": replicaRootVolume + ",size=10G"}
	for rounds := 1; rounds <= 3; rounds++ {
		r := newReplicaRig(config.HAModeAuto)
		r.ownEncodedJob(1, rounds)
		r.repl.removeOnSchedule = true
		if err := retireTornDownReplica(crhCtx(), r.deps, log.NewNopLogger(), "delete_vm", 100, "pve01", tagged,
			nil); err != nil {
			t.Fatalf("%d rounds: unexpected error: %v", rounds, err)
		}
		want := []string{"delete:100-0:force=false", "schedule:pve01/100-0"}
		if !slices.Equal(r.repl.calls, want) || len(r.repl.jobs) != 0 {
			t.Fatalf("%d rounds: calls %v jobs %v, want %v; an encoded marker is still this Director's", rounds,
				r.repl.calls, r.repl.jobs, want)
		}
	}
}

func TestReconcileVMReplica_WidensOverAnEncodedJob(t *testing.T) {
	r := newReplicaRig(config.HAModeAuto)
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownEncodedJob(1, 2)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	loc := pve.VMLocation{Node: "pve01", Found: true, Tags: "bosh-cpi;" + haReplicaTag}
	if err := reconcileVMReplica(crhCtx(), r.deps, "set_vm_metadata", "100", 100, loc,
		log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.pinNodes() != "pve01,pve02" {
		t.Fatalf("pin nodes %q, want the partner added over the encoded job", r.pinNodes())
	}
	if r.repl.called("comment:") {
		t.Fatalf("calls %v; a widen with no marker write of its own leaves the comment alone", r.repl.calls)
	}
}

func TestWidenWithJournalAcceptsAnEncodedMarker(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve01", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.ownEncodedJob(1, 3)
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	handle := replicaHandle(t, changed)
	widened, err := widenOf(r, handle).run(crhCtx())
	if err != nil || !widened {
		t.Fatalf("widened %v err %v", widened, err)
	}
	if sync := handle.Record().Replication.Sync; sync == nil || sync.JobGeneration != 1 {
		t.Fatalf("sync evidence %+v", sync)
	}
}

func TestWidenAfterAFailoverRewritesAnEncodedMarker(t *testing.T) {
	changed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	r := newReplicaRig(config.HAModeManaged)
	r.setPin("pve02", func(c string) string { return ha.WithReplicaChanged(c, changed) })
	r.repl.source = "pve02"
	r.repl.addJob("100-0", "pve02", "pve01", pveEncodedMarker(renderReplicaJobMarker(aaDirector, 1), 2))
	r.repl.status = verifiedSync(changed)
	r.repl.listed[replicaRootVolume] = true
	handle := replicaHandle(t, changed)
	w := widenOf(r, handle)
	w.node = "pve02"
	widened, err := w.run(crhCtx())
	if err != nil || !widened {
		t.Fatalf("widened %v err %v", widened, err)
	}
	if got, want := r.repl.jobs[0].Comment, renderReplicaJobMarker(aaDirector, 2); got != want {
		t.Fatalf("job comment %q, want the canonical marker %q", got, want)
	}
}

func TestMarkKeptReplicaJobRewritesAnEncodedMarker(t *testing.T) {
	canonical := renderReplicaJobMarker(aaDirector, 4)
	t.Run("encoded at the same generation is rewritten", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.ownEncodedJob(4, 2)
		if err := markKeptReplicaJob(crhCtx(), r.repl, "attach_disk", r.repl.jobs[0], 100, aaDirector,
			4); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(r.repl.calls, []string{"comment:100-0"}) || r.repl.jobs[0].Comment != canonical {
			t.Fatalf("calls %v comment %q, want one write of %q", r.repl.calls, r.repl.jobs[0].Comment, canonical)
		}
	})
	t.Run("canonical at the same generation is left alone", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.ownJob(4)
		if err := markKeptReplicaJob(crhCtx(), r.repl, "attach_disk", r.repl.jobs[0], 100, aaDirector,
			4); err != nil || len(r.repl.calls) != 0 {
			t.Fatalf("err %v calls %v, want no write", err, r.repl.calls)
		}
	})
	t.Run("encoded again before the read back still confirms", func(t *testing.T) {
		r := newReplicaRig(config.HAModeManaged)
		r.ownEncodedJob(3, 1)
		api := &reencodingReplica{fakeReplica: r.repl}
		if err := markKeptReplicaJob(crhCtx(), api, "attach_disk", r.repl.jobs[0], 100, aaDirector,
			4); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// reencodingReplica stores every comment write the way a later PVE pass that
// writes replication.cfg twice leaves it.
type reencodingReplica struct{ *fakeReplica }

func (f *reencodingReplica) SetComment(ctx context.Context, id, comment string) error {
	return f.fakeReplica.SetComment(ctx, id, pveEncodedMarker(comment, 1))
}
