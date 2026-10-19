package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// addReconcileFixture builds the cluster the reconcile-tags tests share.
//
//   - 101 holds newRef and carries a stale tag for oldRef beside its own.
//   - 102 holds newRef, and its stale oldRef tag is its only marker.
//   - 103 holds oldRef and carries only its own tag, so it is consistent.
//   - 104 has a description that does not parse.
//   - 105 has a blank description, so its refs are unknown.
//   - 106 holds newRef, carries a stale tag, and is already pending destroy.
//   - 107 holds no ref and carries a tag for oldRef, so reconcile skips it.
//   - 108 has a config that cannot be read.
func addReconcileFixture(f *pruneFake) {
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, newRef), "bosh-stemcell-sha-"+shaB+";director--"+oldRef)
	f.addTemplate(103, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", oldRef),
		cacheTemplateTags("cccc3333")+";director--"+oldRef)
	f.addTemplate(104, "bosh-stemcell-d-1-dddd4444", "hand-edited notes", cacheTemplateTags("dddd4444")+";director--"+oldRef)
	f.addTemplate(105, "bosh-stemcell-e-1-eeee5555", "", cacheTemplateTags("eeee5555")+";director--"+oldRef)
	f.addTemplate(106, "bosh-stemcell-f-1-ffff6666", provDesc("ffff6666", newRef),
		cacheTemplateTags("ffff6666")+";director--"+oldRef+";bosh-destroy-pending")
	f.addTemplate(107, "bosh-stemcell-g-1-77777777", provDesc("77777777"), cacheTemplateTags("77777777")+";director--"+oldRef)
	f.addTemplate(108, "bosh-stemcell-h-1-88888888", provDesc("88888888", newRef),
		cacheTemplateTags("88888888")+";director--"+oldRef)
	f.configErr[108] = errors.New("connection reset")
}

func TestStemcellRefsPrune_ReconcileTagsPlan(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addReconcileFixture(f)

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{ReconcileTags: true})
	if f.writeCount() != 0 {
		t.Fatalf("planning wrote %v; the dry run must change nothing", f.writes)
	}
	if plan.Mode != "reconcile-tags" || plan.HasFailures() {
		t.Fatalf("plan mode %q failures %v; want reconcile-tags with no failures: %+v",
			plan.Mode, plan.HasFailures(), plan.Actions)
	}
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveTags || !slices.Equal(a.DropTags, []string{"director--" + oldRef}) ||
		!slices.Equal(a.KeptTags, []string{"director--" + newRef}) || a.AddsCacheMarker {
		t.Errorf("101 action = %+v; want oldRef's tag dropped and newRef's kept", a)
	}
	if b := actionFor(t, plan, 102); b.Kind != handlers.StemcellRefsActionRemoveTags || !b.AddsCacheMarker {
		t.Errorf("102 action = %+v; want a drop that adds the cache marker", b)
	}
	if g := actionFor(t, plan, 107); g.Kind != handlers.StemcellRefsActionSkip || g.Failure ||
		!strings.Contains(g.Reason, "holds no ref") {
		t.Errorf("107 action = %+v; want a non-failing skip because the template holds no ref", g)
	}
	for _, vmid := range []int64{104, 105, 106, 108} {
		if s := actionFor(t, plan, vmid); s.Kind != handlers.StemcellRefsActionSkip || s.Failure {
			t.Errorf("%d action = %+v; want a non-failing skip", vmid, s)
		}
	}
	for i := range plan.Actions {
		act := plan.Actions[i]
		if act.VMID == 103 {
			t.Errorf("103 is consistent but got an action: %+v", act)
		}
		if act.Kind != handlers.StemcellRefsActionRemoveTags && act.Kind != handlers.StemcellRefsActionSkip {
			t.Errorf("reconcile planned %s on %d; it may only remove tags or skip", act.Kind, act.VMID)
		}
	}

	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mode":"reconcile-tags"`, `"kind":"remove-tags"`, `"drop_tags":["director--` + oldRef + `"]`,
		`"kept_tags":["director--` + newRef + `"]`, `"adds_cache_marker":true`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("plan JSON %s lacks %s", raw, key)
		}
	}
}

func TestStemcellRefsPrune_ReconcileTagsApply(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addReconcileFixture(f)
	descBefore := map[int64]string{}
	for vmid := range f.guests {
		descBefore[vmid] = f.cfgString(vmid, "description")
	}
	tagsBefore := map[int64]string{}
	for _, vmid := range []int64{103, 104, 105, 106, 107, 108} {
		tagsBefore[vmid] = f.cfgString(vmid, "tags")
	}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{ReconcileTags: true})
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	for _, r := range res.Results {
		if r.Action.Kind != handlers.StemcellRefsActionRemoveTags {
			continue
		}
		if r.Outcome != handlers.StemcellRefsOutcomeApplied || !r.TagRemoved {
			t.Errorf("%d result = %+v; want applied with tags removed", r.Action.VMID, r)
		}
		if r.CacheMarkerAdded != (r.Action.VMID == 102) {
			t.Errorf("%d cache marker added = %v", r.Action.VMID, r.CacheMarkerAdded)
		}
	}

	if tags := f.cfgString(101, "tags"); strings.Contains(tags, "director--"+oldRef) ||
		!strings.Contains(tags, "director--"+newRef) {
		t.Errorf("101 tags = %q", tags)
	}
	if tags := f.cfgString(102, "tags"); strings.Contains(tags, "director--"+oldRef) ||
		!strings.Contains(tags, "bosh-stemcell-cache") {
		t.Errorf("102 tags = %q; want the stale tag gone and the cache marker added", tags)
	}
	for vmid, want := range tagsBefore {
		if got := f.cfgString(vmid, "tags"); got != want {
			t.Errorf("%d tags changed from %q to %q", vmid, want, got)
		}
	}
	for vmid, want := range descBefore {
		if got := f.cfgString(vmid, "description"); got != want {
			t.Errorf("%d description rewritten from %q to %q", vmid, want, got)
		}
	}
	for _, w := range f.writes {
		if strings.HasSuffix(w, ":description") {
			t.Errorf("reconcile wrote a description: %v", f.writes)
		}
	}
	if len(f.deleted) != 0 {
		t.Errorf("reconcile destroyed %v", f.deleted)
	}
	if len(f.unlockedWrites) != 0 {
		t.Errorf("writes outside the lock: %v", f.unlockedWrites)
	}

	// A second run finds nothing left to drop.
	_, rerun := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{ReconcileTags: true})
	for _, act := range rerun.Actions {
		if act.Kind == handlers.StemcellRefsActionRemoveTags {
			t.Errorf("rerun still plans %+v", act)
		}
	}
}

// TestStemcellRefsPrune_ReconcileTagsMatchesCaseAndSanitize pins that a tag a
// ref backs only through case folding or sanitizing is kept, and that a
// replica gets the same treatment as a main template.
func TestStemcellRefsPrune_ReconcileTagsMatchesCaseAndSanitize(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, "a.b"), "bosh-stemcell-sha-"+shaA+";director--a-b")
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, "abc"), "bosh-stemcell-sha-"+shaB+";director--ABC")
	f.addTemplate(103, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", "a.b"),
		cacheTemplateTags("cccc3333")+";director--a-b;director--"+oldRef)

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{ReconcileTags: true})
	for _, act := range plan.Actions {
		switch act.VMID {
		case 101, 102:
			t.Errorf("%d is backed through sanitize or case but got an action: %+v", act.VMID, act)
		case 103:
			if act.Kind != handlers.StemcellRefsActionRemoveTags ||
				!slices.Equal(act.DropTags, []string{"director--" + oldRef}) ||
				!slices.Equal(act.KeptTags, []string{"director--a-b"}) {
				t.Errorf("103 action = %+v; want only the unbacked tag dropped", act)
			}
		}
	}
	if len(plan.Actions) != 1 {
		t.Errorf("plan = %+v; want one action for 103", plan.Actions)
	}
}

func TestStemcellRefsPrune_ReconcileTagsChangedSincePlan(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{ReconcileTags: true})
	// oldRef registers again between the plan and the apply, so its tag is
	// backed by a ref once more.
	f.setCfg(101, "description", provDesc(shaA, newRef, oldRef))
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged {
		t.Fatalf("result = %+v; want changed-since-plan", res.Results)
	}
	if f.writeCount() != 0 {
		t.Fatalf("changed template was touched: %v", f.writes)
	}
	if !strings.Contains(f.cfgString(101, "tags"), "director--"+oldRef) {
		t.Error("101 lost a tag its new ref backs")
	}
}

func TestStemcellRefsPrune_ReconcileTagsRequestValidation(t *testing.T) {
	t.Parallel()
	for _, req := range []handlers.StemcellRefsPruneRequest{
		{ReconcileTags: true, DirectorRef: oldRef},
		{ReconcileTags: true, VMID: 101},
		{ReconcileTags: true, AllowOwnedVMs: true},
	} {
		if _, err := handlers.PlanStemcellRefsPrune(handlers.StemcellRefsScan{}, req); err == nil {
			t.Errorf("request %+v accepted", req)
		}
	}
	if _, err := handlers.PlanStemcellRefsPrune(handlers.StemcellRefsScan{},
		handlers.StemcellRefsPruneRequest{ReconcileTags: true}); err != nil {
		t.Errorf("reconcile on its own rejected: %v", err)
	}
}

// paddedDesc returns a provenance description of exactly n bytes, padded
// through a foreign key the CPI preserves.
func paddedDesc(t *testing.T, sha8 string, n int, refs ...string) string {
	t.Helper()
	base := strings.TrimSuffix(provDesc(sha8, refs...), "}") + `,"pad":"`
	pad := n - len(base) - len(`"}`)
	if pad < 0 {
		t.Fatalf("description base is already %d bytes", len(base))
	}
	return base + strings.Repeat("x", pad) + `"}`
}

func TestStemcellRefsReport_FlagsDescriptionsNearLimit(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, paddedDesc(t, shaA, 7168, newRef),
		cacheTemplateTags(shaA)+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, paddedDesc(t, shaB, 7167, newRef),
		cacheTemplateTags(shaB)+";director--"+newRef)

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	rep := handlers.BuildStemcellRefsReport(scan)
	byID := map[int64]handlers.StemcellRefsTemplate{}
	for _, tm := range rep.Templates {
		byID[tm.VMID] = tm
	}
	if tm := byID[101]; !tm.DescriptionNearLimit || tm.DescriptionBytes != 7168 {
		t.Errorf("101 = %d bytes near %v; want 7168 bytes flagged", tm.DescriptionBytes, tm.DescriptionNearLimit)
	}
	if tm := byID[102]; tm.DescriptionNearLimit || tm.DescriptionBytes != 7167 {
		t.Errorf("102 = %d bytes near %v; want 7167 bytes not flagged", tm.DescriptionBytes, tm.DescriptionNearLimit)
	}
	if rep.DescriptionsNearLimit != 1 {
		t.Errorf("descriptions near limit = %d, want 1", rep.DescriptionsNearLimit)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"descriptions_near_limit":1`, `"description_near_limit":true`, `"description_bytes":7168`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("report JSON lacks %s", key)
		}
	}
}
