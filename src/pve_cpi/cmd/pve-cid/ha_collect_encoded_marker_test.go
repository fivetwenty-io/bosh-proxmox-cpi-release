package main

import (
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// pveEncoded returns comment as PVE's API lists it after write_config encoded
// it rounds more times than its one read decodes.
func pveEncoded(comment string, rounds int) string {
	enc := strings.NewReplacer("%", "%25", ":", "%3A")
	for range rounds {
		comment = enc.Replace(comment)
	}
	return comment
}

func TestMarkerParsingReadsRepeatedEncoding(t *testing.T) {
	t.Parallel()
	job := haMarker + " gen=4"
	forms := map[string]string{
		"one round":                   pveEncoded(job, 1),
		"two rounds, the API form":    pveEncoded(job, 2),
		"three rounds, the file form": pveEncoded(job, 3),
		"lab API form":                "bosh-cpi%253Av1 owner=" + haDirector + " gen=4",
		"lab file form":               "bosh-cpi%25253Av1 owner=" + haDirector + " gen=4",
	}
	for name, comment := range forms {
		if !haJobMarker(comment) {
			t.Errorf("%s: haJobMarker(%q) = false, want the marker", name, comment)
		}
		if owner, ok := haMarkerOwner(comment); !ok || owner != haDirector {
			t.Errorf("%s: haMarkerOwner(%q) = %q, %v; want %s", name, comment, owner, ok, haDirector)
		}
	}
	for _, comment := range []string{
		"bosh-cpi%3Av2 owner=" + haDirector + " gen=4",
		"bosh-cpi%253Av1 owner=" + haDirector,
		"bosh-cpi%253Av1 owner=" + haDirector + " gen=0",
		"bosh-cpi%253Av1 owner=- gen=1",
		"sync 100% bosh-cpi:v1 owner=" + haDirector + " gen=4",
		"bosh%2Dcpi%3Av1 owner=" + haDirector + " gen=4",
	} {
		if haJobMarker(comment) {
			t.Errorf("haJobMarker(%q) = true, want no marker", comment)
		}
	}
}

// TestCollectJudgesJobsWhoseMarkerPVEEncodedAgain covers the state after HA
// recovered several replicated guests onto one node: PVE encoded every job's
// comment again, yet the doctor still judges the jobs, finds the VM
// recoverable, and says that PVE rewrote the comment.
func TestCollectJudgesJobsWhoseMarkerPVEEncodedAgain(t *testing.T) {
	t.Parallel()
	for rounds := 1; rounds <= 3; rounds++ {
		f := replicaFake()
		f.jobs[0].Comment = pveEncoded(haMarker+" gen=1", rounds)
		f.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour), FailCount: 1, Error: "no route"}
		r := runCollect(t, f, haCfg("auto", "replicate"))
		want(t, r, haKindReplicaJobFailing, haSeverityProblem, "101-0", "1 time(s)")
		want(t, r, haKindReplicaJobStale, haSeverityProblem, "101-0")
		want(t, r, haKindReplicaMarkerEncode, haSeverityAdvice, "101-0",
			"pvesr update 101-0 --comment '"+haMarker+" gen=1'")
		wantNone(t, r, haKindReplicaNotRecover)
	}

	// A comment PVE never encoded again draws no advice.
	wantNone(t, runCollect(t, replicaFake(), haCfg("auto", "replicate")), haKindReplicaMarkerEncode)

	// Another Director's encoded job stays foreign.
	g := replicaFake()
	g.jobs[0].Comment = pveEncoded("bosh-cpi:v1 owner=aaaaaaaa-0000-0000-0000-000000000000 gen=1", 2)
	g.status["101-0"] = ha.ReplicaSyncStatus{LastSync: haNow.Add(-time.Hour), FailCount: 3}
	wantNone(t, runCollect(t, g, haCfg("auto", "replicate")), haKindReplicaJobFailing, haKindReplicaMarkerEncode)
}

// TestEncodedMarkerAdviceQuotesTheCommentForAShell: a job comment that holds a
// single quote and a command after the marker stays one quoted word in the
// pvesr command the doctor prints.
func TestEncodedMarkerAdviceQuotesTheCommentForAShell(t *testing.T) {
	t.Parallel()
	f := replicaFake()
	f.jobs[0].Comment = pveEncoded(haMarker+" gen=1 x';touch${IFS}/tmp/p;'", 2)
	r := runCollect(t, f, haCfg("auto", "replicate"))
	want(t, r, haKindReplicaMarkerEncode, haSeverityAdvice,
		`pvesr update 101-0 --comment '`+haMarker+` gen=1 x'\''`+";touch${IFS}/tmp/p;"+`'\'''`)
}

func TestJobMarkerRefusesARepeatedOwnerOrGeneration(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{
		haMarker + " owner=" + haDirector,
		"bosh-cpi:v1 owner=a owner=b gen=2",
		"bosh-cpi:v1 owner=a owner=a gen=2",
		"bosh-cpi:v1 owner=a gen=2 gen=3",
		"bosh-cpi:v1 owner=a owner=- gen=2",
	} {
		if haJobMarker(comment) {
			t.Errorf("haJobMarker(%q) = true, want the CPI's refusal", comment)
		}
	}
	if !haJobMarker("bosh-cpi:v1 owner=a gen=2") {
		t.Error("a single owner and generation must still read as the marker")
	}
}

func TestForeignNormalisesTheDirectorUUIDLikeTheMarker(t *testing.T) {
	t.Parallel()
	c := &haCollector{owner: "dir ector"}
	if c.foreign("bosh-cpi:v1 owner=dir_ector gen=1") {
		t.Error("a marker the CPI wrote for this Director must not read as foreign")
	}
	if !c.foreign("bosh-cpi:v1 owner=other gen=1") {
		t.Error("another Director's marker must read as foreign")
	}
}
