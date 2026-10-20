package handlers_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

func refSummary(t *testing.T, rep handlers.StemcellRefsReport, ref string) handlers.StemcellRefsRefSummary {
	t.Helper()
	for i := range rep.Refs {
		if rep.Refs[i].Ref == ref {
			return rep.Refs[i]
		}
	}
	t.Fatalf("report has no ref %q: %+v", ref, rep.Refs)
	return handlers.StemcellRefsRefSummary{}
}

func TestStemcellRefsReport_PairsRefWithCreateEnvDirectorVM(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	// oldRef belongs to a create-env Director VM, newRef to a deployed Director's VM.
	f.addVM(4441, "bosh-0", "bosh-cpi;bosh-owner--"+strings.ToLower(oldRef)+";director--BOSH-Init;deployment--lab-bosh", nil)
	f.addVM(300, "router", "bosh-cpi;bosh-owner--"+strings.ToLower(newRef)+";director--prod-bosh;deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	old := refSummary(t, rep, oldRef)
	want := []handlers.StemcellRefsCreateEnvDirector{
		{VMID: 4441, Node: "pve", Name: "bosh-0", Deployments: []string{"lab-bosh"}},
	}
	if len(old.CreateEnvDirectors) != 1 || old.CreateEnvDirectors[0].VMID != want[0].VMID ||
		old.CreateEnvDirectors[0].Name != want[0].Name || !slices.Equal(old.CreateEnvDirectors[0].Deployments, want[0].Deployments) {
		t.Fatalf("create-env directors = %+v, want %+v", old.CreateEnvDirectors, want)
	}
	if old.PossibleCreateEnvRef {
		t.Error("a ref paired with a guest must not be a possible create-env ref")
	}
	other := refSummary(t, rep, newRef)
	if other.CreateEnvDirectors == nil || len(other.CreateEnvDirectors) != 0 {
		t.Errorf("deployed Director's ref pairing = %#v, want an empty non-nil list", other.CreateEnvDirectors)
	}
	if rep.PossibleCreateEnvRefNote != "" {
		t.Errorf("note = %q, want none while every ref has an owner", rep.PossibleCreateEnvRefNote)
	}
}

func TestStemcellRefsReport_PairingNeedsTheBoshInitDirectorTag(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "router", "bosh-cpi;bosh-owner--"+strings.ToLower(oldRef)+";director--prod-bosh;deployment--cf", nil)
	f.addVM(301, "router2", "bosh-cpi;bosh-owner--"+strings.ToLower(newRef)+";deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	for _, ref := range []string{oldRef, newRef} {
		if got := refSummary(t, rep, ref).CreateEnvDirectors; len(got) != 0 {
			t.Errorf("ref %s paired with %+v, want no pairing without director--bosh-init", ref, got)
		}
	}
}

func TestStemcellRefsReport_PairingListsEveryGuestAndDeployment(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	owner := "bosh-owner--" + strings.ToLower(oldRef)
	f.addVM(4450, "bosh-b", "bosh-cpi;"+owner+";director--bosh-init;deployment--b;deployment--a", nil)
	f.addVM(4441, "bosh-a", "bosh-cpi;"+owner+";director--bosh-init", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	got := refSummary(t, rep, oldRef).CreateEnvDirectors
	if len(got) != 2 || got[0].VMID != 4441 || got[1].VMID != 4450 {
		t.Fatalf("pairings = %+v, want 4441 then 4450", got)
	}
	if len(got[0].Deployments) != 0 || !slices.Equal(got[1].Deployments, []string{"a", "b"}) {
		t.Errorf("deployments = %v and %v, want none and [a b]", got[0].Deployments, got[1].Deployments)
	}
}

func TestStemcellRefsReport_FlagsRefsNoOwnerTagMatches(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	// Every BOSH guest is covered, and only newRef has an owner.
	f.addVM(300, "router", "bosh-cpi;bosh-owner--"+strings.ToLower(newRef)+";director--prod-bosh;deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	if rep.OwnershipUnknown {
		t.Fatalf("ownership unknown: %s", rep.OwnershipUnknownReason)
	}
	if !refSummary(t, rep, oldRef).PossibleCreateEnvRef {
		t.Error("a ref no owner tag matches must be a possible create-env ref")
	}
	if refSummary(t, rep, newRef).PossibleCreateEnvRef {
		t.Error("a ref an owner tag matches must not be a possible create-env ref")
	}
	for _, want := range []string{"director_id", "create-env", "bosh env", "state file",
		"adopt-owners --director-name bosh-init", "Stamping owner tags on older VMs", "stale"} {
		if !strings.Contains(rep.PossibleCreateEnvRefNote, want) {
			t.Errorf("note %q lacks %q", rep.PossibleCreateEnvRefNote, want)
		}
	}
}

func TestStemcellRefsReport_NoteIsAbsentWhenOwnershipIsUnknown(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "pre-094-vm", "bosh-cpi;director--bosh;deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	if !rep.OwnershipUnknown {
		t.Fatal("fixture should leave ownership unknown")
	}
	if rep.PossibleCreateEnvRefNote != "" {
		t.Errorf("note = %q, want none while OWNERSHIP UNKNOWN covers the case", rep.PossibleCreateEnvRefNote)
	}
	for _, r := range rep.Refs {
		if r.PossibleCreateEnvRef {
			t.Errorf("ref %s flagged while ownership is unknown", r.Ref)
		}
	}
}

func TestStemcellRefsReport_NoteSkipsBlankSentinelAndParkerRefs(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, "unknown-director", "", oldRef),
		cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addVM(500, "parker", "bosh-cpi;bosh-parker;director--"+oldRef+";vm-prefix--vm", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	for _, ref := range []string{"unknown-director", "", oldRef} {
		if refSummary(t, rep, ref).PossibleCreateEnvRef {
			t.Errorf("ref %q flagged as a possible create-env ref", ref)
		}
	}
	if rep.PossibleCreateEnvRefNote != "" {
		t.Errorf("note = %q, want none", rep.PossibleCreateEnvRefNote)
	}
}

func TestStemcellRefsReport_HintsDoNotChangePrunePlans(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(4441, "bosh-0", "bosh-cpi;bosh-owner--"+strings.ToLower(oldRef)+";director--bosh-init;deployment--lab-bosh", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if !plan.Refused || len(plan.OwnedVMs) != 1 || plan.OwnedVMs[0].VMID != 4441 {
		t.Fatalf("plan refused %v owned %+v; the pairing must leave the owned-VM refusal alone", plan.Refused, plan.OwnedVMs)
	}
	if n := refSummary(t, rep, oldRef).OwnedVMCount; n == nil || *n != 1 {
		t.Errorf("owned count = %v, want 1", n)
	}
}
