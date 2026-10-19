package handlers_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const (
	liveUUID  = "6A1B2C3D-0000-4000-8000-00000000000A"
	otherUUID = "7b1b2c3d-0000-4000-8000-00000000000b"
	liveOwner = "bosh-owner--6a1b2c3d-0000-4000-8000-00000000000a"
	otherOwn  = "bosh-owner--7b1b2c3d-0000-4000-8000-00000000000b"
)

// twoRefTemplates adds a template every ref in the coverage tests can prune.
func twoRefTemplates(f *pruneFake) {
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
}

func scanOnly(t *testing.T, f *pruneFake) handlers.StemcellRefsScan {
	t.Helper()
	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return scan
}

func TestStemcellRefsCoverage_UncoveredGuestMarksEveryRefUnknown(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "pre-094-vm", "bosh-cpi;director--bosh;deployment--cf", nil)
	f.addVM(301, "covered-vm", "bosh-cpi;bosh-owner--"+strings.ToLower(newRef)+";director--bosh;deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	if !rep.OwnershipUnknown || !strings.Contains(rep.OwnershipUnknownReason, "(300)") {
		t.Fatalf("report ownership unknown = %v reason %q; want unknown naming 300",
			rep.OwnershipUnknown, rep.OwnershipUnknownReason)
	}
	if len(rep.Refs) == 0 {
		t.Fatal("report has no refs")
	}
	for _, r := range rep.Refs {
		if !r.OwnershipUnknown || r.OwnedVMCount != nil || !r.PruneBlocked {
			t.Errorf("ref %q: unknown %v owned %v blocked %v; want unknown, nil count, and blocked",
				r.Ref, r.OwnershipUnknown, r.OwnedVMCount, r.PruneBlocked)
		}
		if len(r.WouldFree) != 0 || len(r.Blocked) != 0 || len(r.KeptReplicas) != 0 {
			t.Errorf("ref %q lists would-free %v blocked %v kept %v while the prune is blocked",
				r.Ref, r.WouldFree, r.Blocked, r.KeptReplicas)
		}
	}
	if len(rep.Uncovered) != 1 {
		t.Fatalf("uncovered groups = %+v, want one", rep.Uncovered)
	}
	g := rep.Uncovered[0]
	if g.DirectorName != "bosh" || g.Deployment != "cf" || !slices.Equal(g.VMIDs, []int64{300}) || !g.Adoptable {
		t.Errorf("group = %+v", g)
	}
	want := "cpi stemcell-refs adopt-owners --config <cpi.json> --director-uuid <director-uuid> --director-name bosh"
	if !slices.Equal(g.Commands, []string{want}) {
		t.Errorf("commands = %q, want %q", g.Commands, want)
	}
	if !strings.Contains(g.UUIDSource, "`bosh env`") {
		t.Errorf("uuid source = %q, want it to name bosh env", g.UUIDSource)
	}
}

func TestStemcellRefsCoverage_PruneRefusesWithAndWithoutAllowOwned(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		f := newPruneFake()
		twoRefTemplates(f)
		f.addVM(300, "pre-094-vm", "bosh-cpi;director--bosh;deployment--cf", nil)

		deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef, AllowOwnedVMs: allow})
		if !plan.Refused || !plan.CoverageIncomplete || !plan.OwnershipUnknown {
			t.Fatalf("allow=%v: plan refused %v coverage incomplete %v unknown %v; want all set",
				allow, plan.Refused, plan.CoverageIncomplete, plan.OwnershipUnknown)
		}
		if !strings.Contains(plan.RefusalReason, "--allow-owned-vms does not override this") ||
			plan.OwnershipUnknownReason != plan.RefusalReason {
			t.Errorf("allow=%v: refusal %q, unknown reason %q", allow, plan.RefusalReason, plan.OwnershipUnknownReason)
		}
		if len(plan.Uncovered) != 1 || !slices.Equal(plan.Uncovered[0].VMIDs, []int64{300}) {
			t.Errorf("allow=%v: uncovered = %+v", allow, plan.Uncovered)
		}
		if _, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); !errors.Is(err, handlers.ErrStemcellRefsPruneRefused) {
			t.Fatalf("allow=%v: apply err = %v, want refusal", allow, err)
		}
		if f.writeCount() != 0 || len(f.deleted) != 0 {
			t.Fatalf("allow=%v: refused apply changed PVE: writes %v deleted %v", allow, f.writes, f.deleted)
		}
	}
}

func TestStemcellRefsCoverage_GuestKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		tags      string
		kind      string
		uncovered bool
	}{
		{"bosh-cpi only", "bosh-cpi", handlers.StemcellRefsGuestBOSH, true},
		{"director tag only", "director--bosh", handlers.StemcellRefsGuestBOSH, true},
		{"deployment tag only, mixed case", "Deployment--cf", handlers.StemcellRefsGuestBOSH, true},
		{"owner tag only", "bosh-owner--" + strings.ToLower(newRef), handlers.StemcellRefsGuestBOSH, false},
		{"covered", "bosh-cpi;bosh-owner--abc;director--bosh", handlers.StemcellRefsGuestBOSH, false},
		{"parker without director", "bosh-cpi;bosh-parker;vm-prefix--vm", handlers.StemcellRefsGuestParker, false},
		{"stemcell marker on a non-template", "bosh-cpi;bosh-stemcell;director--bosh", handlers.StemcellRefsGuestBOSH, true},
		{"not BOSH", "web;prod", handlers.StemcellRefsGuestOther, false},
		{"untagged", "", handlers.StemcellRefsGuestOther, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPruneFake()
			twoRefTemplates(f)
			f.addVM(300, "guest", tc.tags, nil)
			scan := scanOnly(t, f)
			if len(scan.Guests) != 1 {
				t.Fatalf("guests = %+v", scan.Guests)
			}
			g := scan.Guests[0]
			if g.Kind != tc.kind || g.Uncovered != tc.uncovered {
				t.Errorf("kind %q uncovered %v; want %q and %v", g.Kind, g.Uncovered, tc.kind, tc.uncovered)
			}
			if got := len(scan.Uncovered) > 0; got != tc.uncovered {
				t.Errorf("scan uncovered groups = %+v", scan.Uncovered)
			}
		})
	}
}

func TestStemcellRefsCoverage_ParkerCountsAsOwned(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(500, "parker-1", "bosh-cpi;bosh-parker;director--"+oldRef+";vm-prefix--vm", nil)
	f.addVM(501, "parker-2", "bosh-cpi;bosh-parker;director--"+newRef+";vm-prefix--vm", nil)

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if plan.CoverageIncomplete {
		t.Fatalf("parkers made coverage incomplete: %+v", plan.Uncovered)
	}
	if !plan.Refused || len(plan.OwnedVMs) != 1 || plan.OwnedVMs[0].VMID != 500 {
		t.Fatalf("plan refused %v owned %+v; want a refusal naming parker 500", plan.Refused, plan.OwnedVMs)
	}
	_, allowed := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef, AllowOwnedVMs: true})
	if allowed.Refused {
		t.Fatalf("--allow-owned-vms plan still refused: %s", allowed.RefusalReason)
	}
}

func TestStemcellRefsCoverage_NonBOSHGuestIsIgnored(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "app-server", "web;prod", nil)
	f.addVM(301, "untagged", "", nil)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if plan.Refused || plan.OwnershipUnknown || len(plan.OwnedVMs) != 0 {
		t.Fatalf("plan = refused %v unknown %v owned %+v: %s", plan.Refused, plan.OwnershipUnknown,
			plan.OwnedVMs, plan.RefusalReason)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply err %v failures %d", err, res.Failures)
	}
}

func TestStemcellRefsCoverage_FullyCoveredClusterPrunes(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "new-dir-vm", "bosh-cpi;bosh-owner--"+strings.ToLower(newRef)+";director--bosh;deployment--cf", nil)
	f.addVM(301, "parker", "bosh-cpi;bosh-parker;director--"+newRef+";vm-prefix--vm", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	if rep.OwnershipUnknown || len(rep.Uncovered) != 0 {
		t.Fatalf("report unknown %v uncovered %+v", rep.OwnershipUnknown, rep.Uncovered)
	}
	if rep.Uncovered == nil {
		t.Error("report uncovered is nil; JSON needs an empty list")
	}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if plan.Refused || plan.CoverageIncomplete {
		t.Fatalf("covered cluster refused: %s", plan.RefusalReason)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply err %v failures %d: %+v", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{102}) {
		t.Errorf("deleted = %v, want [102]", f.deleted)
	}
}

func TestStemcellRefsCoverage_ReportGroupsCreateEnvDirectors(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(400, "bosh-a", "bosh-cpi;director--bosh-init;deployment--bosh", nil)
	f.addVM(401, "bosh-b", "bosh-cpi;director--bosh-init;deployment--bosh", nil)
	f.addVM(402, "bosh-c", "bosh-cpi;director--bosh-init;deployment--lab-bosh", nil)
	f.addVM(403, "odd", "bosh-cpi;bosh-stemcell;director--bosh-init;deployment--lab-bosh", nil)
	f.addVM(404, "cf-vm", "bosh-cpi;director--prod-bosh;deployment--cf", nil)

	rep := handlers.BuildStemcellRefsReport(scanOnly(t, f))
	if len(rep.Uncovered) != 4 {
		t.Fatalf("groups = %+v, want four", rep.Uncovered)
	}
	base := "cpi stemcell-refs adopt-owners --config <cpi.json> --director-uuid <director-uuid> --director-name "
	shared, single, odd, named := rep.Uncovered[0], rep.Uncovered[1], rep.Uncovered[2], rep.Uncovered[3]
	if !slices.Equal(shared.VMIDs, []int64{400, 401}) || !slices.Equal(shared.Commands, []string{
		base + "bosh-init --vmid 400 --deployment bosh", base + "bosh-init --vmid 401 --deployment bosh",
	}) {
		t.Errorf("shared deployment group = %+v", shared)
	}
	if !slices.Equal(single.VMIDs, []int64{402}) ||
		!slices.Equal(single.Commands, []string{base + "bosh-init --deployment lab-bosh"}) {
		t.Errorf("single create-env group = %+v", single)
	}
	for _, g := range []handlers.StemcellRefsUncoveredGroup{shared, single} {
		if !strings.Contains(g.UUIDSource, "director_id in the create-env state file") ||
			!strings.Contains(g.UUIDSource, "not the UUID that `bosh env` prints") {
			t.Errorf("create-env uuid source = %q", g.UUIDSource)
		}
	}
	if odd.Adoptable || len(odd.Commands) != 0 || !slices.Equal(odd.VMIDs, []int64{403}) ||
		!strings.Contains(odd.UUIDSource, "stemcell marker bosh-stemcell") {
		t.Errorf("stemcell-marker group = %+v", odd)
	}
	if named.DirectorName != "prod-bosh" || !slices.Equal(named.Commands, []string{base + "prod-bosh"}) {
		t.Errorf("named director group = %+v", named)
	}
	if !strings.Contains(rep.OwnershipUnknownReason, "5 BOSH guest(s)") ||
		!strings.Contains(rep.OwnershipUnknownReason, "(400, 401, 402, 403, 404)") {
		t.Errorf("reason = %q", rep.OwnershipUnknownReason)
	}
}

// adoptFixture is a cluster with one live director, "bosh", whose VMs predate
// owner tags, plus guests adopt-owners must leave alone.
func adoptFixture() *pruneFake {
	f := newPruneFake()
	twoRefTemplates(f)
	f.addVM(300, "cf-router", "bosh-cpi;director--bosh;deployment--cf", nil)
	f.addVM(301, "cf-api", "bosh-cpi;director--bosh;deployment--cf", nil)
	f.addVM(302, "redis", "bosh-cpi;director--bosh;deployment--redis", nil)
	f.addVM(303, "other-dir", "bosh-cpi;director--other;deployment--cf", nil)
	f.addVM(304, "parker", "bosh-cpi;bosh-parker;director--"+liveUUID+";vm-prefix--vm", nil)
	f.addVM(305, "app", "web", nil)
	return f
}

func planAdopt(t *testing.T, f *pruneFake, req handlers.StemcellRefsAdoptRequest) (handlers.Deps, handlers.StemcellRefsAdoptPlan) {
	t.Helper()
	deps := f.deps()
	scan, err := handlers.ScanStemcellRefs(context.Background(), deps)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := handlers.PlanStemcellRefsAdopt(scan, req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return deps, plan
}

func adoptIDs(gs []handlers.StemcellRefsAdoptGuest) []int64 {
	out := make([]int64, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.VMID)
	}
	return out
}

func outcomes(res handlers.StemcellRefsAdoptResult) map[int64]string {
	out := map[int64]string{}
	for i := range res.Results {
		out[res.Results[i].Guest.VMID] = res.Results[i].Outcome
	}
	return out
}

func TestStemcellRefsAdopt_DryRunThenApply(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
	if plan.Refused {
		t.Fatalf("plan refused: %s", plan.RefusalReason)
	}
	if plan.OwnerTag != liveOwner || plan.DirectorTag != "director--bosh" {
		t.Errorf("owner tag %q director tag %q", plan.OwnerTag, plan.DirectorTag)
	}
	if got := adoptIDs(plan.Targets); !slices.Equal(got, []int64{300, 301, 302}) {
		t.Errorf("targets = %v, want [300 301 302] sorted by deployment", got)
	}
	if plan.Targets[0].Deployment != "cf" || plan.Targets[2].Deployment != "redis" {
		t.Errorf("target deployments = %+v", plan.Targets)
	}
	if f.writeCount() != 0 {
		t.Fatalf("planning wrote %d times; the dry run must change nothing", f.writeCount())
	}

	res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply err %v failures %d: %+v", err, res.Failures, res.Results)
	}
	for _, id := range []int64{300, 301, 302} {
		if outcomes(res)[id] != handlers.StemcellRefsOutcomeApplied {
			t.Errorf("vmid %d outcome %q", id, outcomes(res)[id])
		}
		if tags := f.cfgString(id, "tags"); !strings.HasPrefix(tags, "bosh-cpi;"+liveOwner+";") {
			t.Errorf("vmid %d tags = %q; want the owner tag at the head", id, tags)
		}
	}
	for _, id := range []int64{303, 304, 305} {
		if strings.Contains(f.cfgString(id, "tags"), "bosh-owner--") {
			t.Errorf("vmid %d was stamped: %q", id, f.cfgString(id, "tags"))
		}
	}
	if len(f.unlockedWrites) != 0 {
		t.Errorf("writes outside the per-VMID lock: %v", f.unlockedWrites)
	}

	// A second run finds the guests already owned and writes nothing.
	_, again := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
	if len(again.Targets) != 0 || !slices.Equal(adoptIDs(again.AlreadyOwned), []int64{300, 301, 302}) {
		t.Errorf("rerun targets %v already owned %v", adoptIDs(again.Targets), adoptIDs(again.AlreadyOwned))
	}
}

func TestStemcellRefsAdopt_DeploymentSelector(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	_, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
		DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis",
	})
	if plan.Refused || !slices.Equal(adoptIDs(plan.Targets), []int64{302}) || plan.DeploymentTag != "deployment--redis" {
		t.Fatalf("plan = %+v", plan)
	}
	_, none := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
		DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "missing",
	})
	if !none.Refused || !strings.Contains(none.RefusalReason, "no guest the selectors keep carries director--bosh") {
		t.Errorf("unmatched deployment: refused %v reason %q", none.Refused, none.RefusalReason)
	}
}

func TestStemcellRefsAdopt_PairingRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tags    string
		reason  string
		scoped  handlers.StemcellRefsAdoptRequest
		refused bool
	}{
		{
			name:   "name already paired with another UUID",
			tags:   "bosh-cpi;" + otherOwn + ";director--bosh;deployment--cf",
			reason: "that director name already belongs to another UUID",
		},
		{
			name:   "UUID already paired with another name",
			tags:   "bosh-cpi;" + liveOwner + ";director--other-name;deployment--cf",
			reason: "that UUID already belongs to another director name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := adoptFixture()
			f.addVM(310, "conflict", tc.tags, nil)
			deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
			if !plan.Refused || !slices.Equal(adoptIDs(plan.Conflicts), []int64{310}) ||
				!strings.Contains(plan.Conflicts[0].Reason, tc.reason) {
				t.Fatalf("plan refused %v conflicts %+v", plan.Refused, plan.Conflicts)
			}
			res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
			if !errors.Is(err, handlers.ErrStemcellRefsAdoptRefused) || !res.Refused {
				t.Fatalf("apply err %v refused %v", err, res.Refused)
			}
			if f.writeCount() != 0 {
				t.Fatalf("refused apply wrote: %v", f.writes)
			}

			// The check looks only at the selected guests, so a selection
			// that leaves the conflicting guest out goes ahead.
			_, scoped := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
				DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis",
			})
			if scoped.Refused || !slices.Equal(adoptIDs(scoped.Targets), []int64{302}) {
				t.Errorf("scoped plan refused %v (%s) targets %v", scoped.Refused, scoped.RefusalReason,
					adoptIDs(scoped.Targets))
			}
		})
	}
}

func TestStemcellRefsAdopt_SkipsGuestsItNeverStamps(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	f.addVM(320, "marker", "bosh-cpi;bosh-stemcell;director--bosh;deployment--cf", nil)
	f.addVM(321, "no-bosh-cpi", "director--bosh;deployment--cf", nil)
	f.addVM(322, "two-deployments", "bosh-cpi;director--bosh;deployment--cf;deployment--redis", nil)
	_, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
	if plan.Refused {
		t.Fatalf("plan refused: %s", plan.RefusalReason)
	}
	if !slices.Equal(adoptIDs(plan.Skipped), []int64{320, 321, 322}) || !slices.Equal(adoptIDs(plan.Targets), []int64{300, 301, 302}) {
		t.Errorf("skipped %v targets %v", adoptIDs(plan.Skipped), adoptIDs(plan.Targets))
	}
}

func TestStemcellRefsAdopt_RecheckUnderTheLock(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
	// Between the plan and the apply: another director claims 300, the
	// director itself stamps 301, and 302 loses its director tag.
	f.onRead = func(vmid int64, g *pruneFakeGuest) {
		switch vmid {
		case 300:
			g.cfg["tags"] = "bosh-cpi;" + otherOwn + ";director--bosh;deployment--cf"
		case 301:
			g.cfg["tags"] = "bosh-cpi;" + liveOwner + ";director--bosh;deployment--cf"
		case 302:
			g.cfg["tags"] = "bosh-cpi;deployment--redis"
		}
	}
	res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := outcomes(res)
	if got[300] != handlers.StemcellRefsOutcomeChanged || got[301] != handlers.StemcellRefsOutcomeAlreadyOwned ||
		got[302] != handlers.StemcellRefsOutcomeChanged {
		t.Errorf("outcomes = %v", got)
	}
	if res.Failures != 2 {
		t.Errorf("failures = %d, want 2", res.Failures)
	}
	if f.writeCount() != 0 {
		t.Errorf("the recheck let writes through: %v", f.writes)
	}
}

func TestStemcellRefsAdopt_FailedGuestDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh"})
	f.configErr[300] = &sdkerrors.APIError{HTTPCode: 500, Message: "pmxcfs busy"}
	f.mu.Lock()
	delete(f.guests, 301)
	f.mu.Unlock()
	res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := outcomes(res)
	if got[300] != handlers.StemcellRefsOutcomeFailed || got[301] != handlers.StemcellRefsOutcomeNotFound ||
		got[302] != handlers.StemcellRefsOutcomeApplied {
		t.Errorf("outcomes = %v", got)
	}
	if res.Failures != 2 {
		t.Errorf("failures = %d, want 2", res.Failures)
	}
	if !strings.HasPrefix(f.cfgString(302, "tags"), "bosh-cpi;"+liveOwner) {
		t.Errorf("302 tags = %q", f.cfgString(302, "tags"))
	}
}

func TestStemcellRefsAdopt_DigestGuard(t *testing.T) {
	t.Parallel()
	t.Run("one stale write is retried", func(t *testing.T) {
		t.Parallel()
		f := adoptFixture()
		deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis",
		})
		f.staleWrites[302] = 1
		res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
		if err != nil || res.Failures != 0 || outcomes(res)[302] != handlers.StemcellRefsOutcomeApplied {
			t.Fatalf("err %v results %+v", err, res.Results)
		}
	})
	t.Run("a digest that stays stale fails the guest", func(t *testing.T) {
		t.Parallel()
		f := adoptFixture()
		deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis",
		})
		f.staleWrites[302] = 10
		res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
		if err != nil || res.Failures != 1 || outcomes(res)[302] != handlers.StemcellRefsOutcomeFailed {
			t.Fatalf("err %v results %+v", err, res.Results)
		}
		if strings.Contains(f.cfgString(302, "tags"), "bosh-owner--") {
			t.Errorf("302 stamped despite stale digests: %q", f.cfgString(302, "tags"))
		}
	})
	t.Run("a read without a digest writes nothing", func(t *testing.T) {
		t.Parallel()
		f := adoptFixture()
		deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis",
		})
		f.noDigest[302] = true
		res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
		if err != nil || res.Failures != 1 || f.writeCount() != 0 {
			t.Fatalf("err %v failures %d writes %v", err, res.Failures, f.writes)
		}
	})
}

func TestStemcellRefsAdopt_RequestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  handlers.StemcellRefsAdoptRequest
		want string
	}{
		{"uuid not a uuid", handlers.StemcellRefsAdoptRequest{DirectorUUID: "bosh", DirectorName: "bosh"}, "is not a UUID"},
		{"name sanitizes to empty", handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "!!"}, "no characters"},
		{"name is a uuid", handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: otherUUID}, "is a UUID"},
		{"vmid not positive", handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh", VMIDs: []int64{0}}, "positive"},
		{"bosh-init without a selector", handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "bosh-init"}, "--deployment or --vmid"},
		{"bosh-init in another case", handlers.StemcellRefsAdoptRequest{DirectorUUID: liveUUID, DirectorName: "BOSH-INIT"}, "--deployment or --vmid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := handlers.ValidateStemcellRefsAdoptRequest(tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if _, perr := handlers.PlanStemcellRefsAdopt(handlers.StemcellRefsScan{}, tc.req); perr == nil {
				t.Fatal("the planner accepted a request validation refuses")
			}
		})
	}
}

func TestStemcellRefsAdopt_VMIDSelector(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	_, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
		DirectorUUID: liveUUID, DirectorName: "bosh", VMIDs: []int64{301, 300, 301},
	})
	if plan.Refused || !slices.Equal(adoptIDs(plan.Targets), []int64{300, 301}) || !slices.Equal(plan.VMIDs, []int64{300, 301}) {
		t.Fatalf("plan = refused %v (%s) targets %v vmids %v", plan.Refused, plan.RefusalReason,
			adoptIDs(plan.Targets), plan.VMIDs)
	}
	for _, tc := range []struct {
		vmid int64
		want string
	}{
		{999, "no non-template guest with VMID 999"},
		{101, "VMID 101 is a template"},
		{303, "VMID 303 does not carry director--bosh"},
		{304, "VMID 304 is a parker"},
	} {
		_, bad := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: liveUUID, DirectorName: "bosh", VMIDs: []int64{300, tc.vmid},
		})
		if !bad.Refused || !strings.Contains(bad.RefusalReason, tc.want) {
			t.Errorf("vmid %d: refused %v reason %q", tc.vmid, bad.Refused, bad.RefusalReason)
		}
	}
	_, wrongDep := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
		DirectorUUID: liveUUID, DirectorName: "bosh", Deployment: "redis", VMIDs: []int64{300},
	})
	if !wrongDep.Refused || !strings.Contains(wrongDep.RefusalReason, "VMID 300 does not carry deployment--redis") {
		t.Errorf("vmid outside the deployment: refused %v reason %q", wrongDep.Refused, wrongDep.RefusalReason)
	}
}

func TestStemcellRefsAdopt_CreateEnvDirectors(t *testing.T) {
	t.Parallel()
	const uuidA = "aaaaaaaa-0000-4000-8000-000000000001"
	const uuidB = "bbbbbbbb-0000-4000-8000-000000000002"
	newCluster := func() *pruneFake {
		f := newPruneFake()
		twoRefTemplates(f)
		f.addVM(400, "director-a", "bosh-cpi;director--bosh-init;deployment--bosh-a", nil)
		f.addVM(401, "director-b", "bosh-cpi;director--bosh-init;deployment--bosh-b", nil)
		return f
	}

	t.Run("two Directors in one cluster are each adopted by deployment", func(t *testing.T) {
		t.Parallel()
		f := newCluster()
		for _, step := range []struct {
			uuid, deployment string
			vmid             int64
		}{{uuidA, "bosh-a", 400}, {uuidB, "bosh-b", 401}} {
			deps, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
				DirectorUUID: step.uuid, DirectorName: "bosh-init", Deployment: step.deployment,
			})
			if plan.Refused || !slices.Equal(adoptIDs(plan.Targets), []int64{step.vmid}) {
				t.Fatalf("%s: refused %v (%s) targets %v", step.deployment, plan.Refused, plan.RefusalReason,
					adoptIDs(plan.Targets))
			}
			res, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan)
			if err != nil || res.Failures != 0 {
				t.Fatalf("%s: apply err %v failures %d", step.deployment, err, res.Failures)
			}
		}
		if !strings.Contains(f.cfgString(400, "tags"), "bosh-owner--"+uuidA) ||
			!strings.Contains(f.cfgString(401, "tags"), "bosh-owner--"+uuidB) {
			t.Errorf("tags 400 %q 401 %q", f.cfgString(400, "tags"), f.cfgString(401, "tags"))
		}
		if rep := handlers.BuildStemcellRefsReport(scanOnly(t, f)); rep.OwnershipUnknown {
			t.Errorf("ownership still unknown after both adoptions: %+v", rep.Uncovered)
		}
		// Adopting Director A's VM for Director B now conflicts.
		_, clash := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: uuidB, DirectorName: "bosh-init", VMIDs: []int64{400},
		})
		if !clash.Refused || !slices.Equal(adoptIDs(clash.Conflicts), []int64{400}) {
			t.Errorf("cross adoption: refused %v conflicts %v", clash.Refused, adoptIDs(clash.Conflicts))
		}
	})

	t.Run("two create-env guests in one selection are refused", func(t *testing.T) {
		t.Parallel()
		f := newCluster()
		f.addVM(402, "director-c", "bosh-cpi;director--bosh-init;deployment--bosh-a", nil)
		_, plan := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: uuidA, DirectorName: "bosh-init", Deployment: "bosh-a",
		})
		if !plan.Refused || !strings.Contains(plan.RefusalReason, "holds 2 guest(s) tagged director--bosh-init (400, 402)") ||
			!strings.Contains(plan.RefusalReason, "--vmid") {
			t.Fatalf("refused %v reason %q", plan.Refused, plan.RefusalReason)
		}
		_, narrowed := planAdopt(t, f, handlers.StemcellRefsAdoptRequest{
			DirectorUUID: uuidA, DirectorName: "bosh-init", Deployment: "bosh-a", VMIDs: []int64{402},
		})
		if narrowed.Refused || !slices.Equal(adoptIDs(narrowed.Targets), []int64{402}) {
			t.Errorf("narrowed: refused %v (%s) targets %v", narrowed.Refused, narrowed.RefusalReason,
				adoptIDs(narrowed.Targets))
		}
	})
}

func TestStemcellRefsAdopt_UnplannedOrRefusedPlanIsNotApplied(t *testing.T) {
	t.Parallel()
	f := adoptFixture()
	deps := f.deps()
	plan := handlers.StemcellRefsAdoptPlan{
		OwnerTag: liveOwner, DirectorTag: "director--bosh",
		Targets: []handlers.StemcellRefsAdoptGuest{{VMID: 300, Node: vmNode, Name: "cf-router"}},
	}
	if _, err := handlers.ApplyStemcellRefsAdopt(context.Background(), deps, plan); err == nil {
		t.Fatal("an unplanned plan was applied")
	}
	if f.writeCount() != 0 {
		t.Fatalf("writes = %v", f.writes)
	}
}
