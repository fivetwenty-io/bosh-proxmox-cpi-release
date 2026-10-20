package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const (
	untrackedSha  = "abcd1111"
	untrackedName = untrackedNamePrefix + untrackedSha
	// untrackedNamePrefix is the name create_stemcell builds for provDesc's
	// stemcell, test version 1.0, before it appends the sha8.
	untrackedNamePrefix = "bosh-stemcell-test-1-0-"
	gib                 = int64(1) << 30
	mib                 = int64(1) << 20
)

// addUntracked adds untracked template vmid with no ref and one owned disk.
func addUntracked(f *pruneFake, vmid int64, sha8 string) {
	vol := "local-lvm:base-" + strconv.FormatInt(vmid, 10) + "-disk-0"
	f.addTemplate(vmid, untrackedNamePrefix+sha8, provDesc(sha8), untrackedTags(sha8, false))
	f.guests[vmid].cfg["scsi0"] = vol + ",size=4G"
	f.sizes[vol] = 4 * gib
}

func untrackedPlan(t *testing.T, f *pruneFake, vmids ...int64) (handlers.Deps, handlers.StemcellRefsPlan) {
	t.Helper()
	req := handlers.StemcellRefsPruneRequest{Untracked: true}
	if len(vmids) == 1 {
		req.VMID = vmids[0]
	} else {
		req.VMIDs = vmids
	}
	return scanAndPlan(t, f, req)
}

func TestStemcellRefsPruneUntracked_DryRunListsVolumesAndTotal(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(110, untrackedName, provDesc(untrackedSha), untrackedTags(untrackedSha, false))
	cfg := f.guests[110].cfg
	cfg["scsi0"] = "local-lvm:base-110-disk-0,size=4G"
	cfg["efidisk0"] = "local-lvm:base-110-disk-1,efitype=4m,size=4M"
	cfg["virtio1"] = "nfs:110/base-110-disk-2.qcow2,size=1G"
	// An ISO and a parent volume of another guest are not freed by the
	// destroy, so they are not listed.
	cfg["ide2"] = "local:iso/noble.iso,media=cdrom"
	cfg["scsi1"] = "local-lvm:base-90-disk-0/vm-110-disk-3,size=2G"
	f.sizes["local-lvm:base-110-disk-0"] = 4 * gib
	f.sizes["local-lvm:base-110-disk-1"] = 4 * mib
	f.sizes["local-lvm:base-90-disk-0/vm-110-disk-3"] = 2 * gib
	f.sizeErr["nfs:110/base-110-disk-2.qcow2"] = &sdkerrors.APIError{HTTPCode: 500, Message: "storage offline"}

	deps, plan := untrackedPlan(t, f, 110)
	if plan.Refused || plan.Mode != "untracked" || len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v; want one untracked action", plan)
	}
	handlers.MeasureStemcellRefsPlan(context.Background(), deps, &plan)
	a := plan.Actions[0]
	if a.Kind != handlers.StemcellRefsActionDestroyUntracked || a.Frees == nil {
		t.Fatalf("action = %+v; want destroy-untracked with the volumes it frees", a)
	}
	vols := make([]string, 0, len(a.Frees.Volumes))
	for _, v := range a.Frees.Volumes {
		vols = append(vols, v.Slot+"="+v.Volume)
	}
	want := []string{
		"efidisk0=local-lvm:base-110-disk-1", "scsi0=local-lvm:base-110-disk-0",
		"scsi1=local-lvm:base-90-disk-0/vm-110-disk-3", "virtio1=nfs:110/base-110-disk-2.qcow2",
	}
	if !slices.Equal(vols, want) {
		t.Fatalf("volumes = %v; want %v", vols, want)
	}
	wantTotal := 4*gib + 4*mib + 2*gib
	if a.Frees.TotalBytes != wantTotal || a.Frees.SizesUnknown != 1 || plan.TotalBytes != wantTotal ||
		plan.SizesUnknown != 1 {
		t.Errorf("totals = action %d/%d plan %d/%d; want %d with one size unknown", a.Frees.TotalBytes,
			a.Frees.SizesUnknown, plan.TotalBytes, plan.SizesUnknown, wantTotal)
	}
	nfs := a.Frees.Volumes[3]
	if nfs.Storage != "nfs" || nfs.SizeBytes != nil || !strings.Contains(nfs.SizeError, "storage offline") {
		t.Errorf("nfs volume = %+v; want an unknown size with the read error", nfs)
	}
	if v := a.Frees.Volumes[1]; v.SizeBytes == nil || *v.SizeBytes != 4*gib || v.Storage != "local-lvm" {
		t.Errorf("scsi0 volume = %+v; want 4 GiB on local-lvm", v)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"mode":"untracked"`, `"kind":"destroy-untracked"`, `"size_bytes":null`,
		`"total_bytes":`, `"sizes_unknown":1`} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("plan JSON lacks %s: %s", s, raw)
		}
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("a dry run changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPruneUntracked_ApplyDestroysTemplate(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)

	deps, plan := untrackedPlan(t, f, 110)
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 || len(res.Results) != 1 {
		t.Fatalf("apply: err %v %+v", err, res)
	}
	r := res.Results[0]
	if r.Outcome != handlers.StemcellRefsOutcomeApplied || !slices.Equal(r.Destroyed, []int64{110}) {
		t.Errorf("result = %+v; want applied with 110 destroyed", r)
	}
	if !slices.Equal(f.deleted, []int64{110}) {
		t.Fatalf("deleted = %v; want [110]", f.deleted)
	}
	if !strings.Contains(f.tagsAtDelete[110], "bosh-destroy-pending") {
		t.Errorf("tags at destroy = %q; want bosh-destroy-pending stamped first", f.tagsAtDelete[110])
	}
	if !slices.Equal(f.writes, []string{"110:tags"}) || len(f.unlockedWrites) != 0 {
		t.Errorf("writes %v unlocked %v; want one locked tag write", f.writes, f.unlockedWrites)
	}
}

func TestStemcellRefsPruneUntracked_StampIsDigestGuarded(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.staleWrites[110] = 1

	deps, plan := untrackedPlan(t, f, 110)
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged {
		t.Fatalf("apply: err %v %+v; want changed-since-plan on a stale digest", err, res.Results)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("deleted %v writes %v; a stale stamp must leave the template alone", f.deleted, f.writes)
	}

	g := newPruneFake()
	addUntracked(g, 110, untrackedSha)
	g.noDigest[110] = true
	deps, plan = untrackedPlan(t, g, 110)
	res, err = handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || !strings.Contains(res.Results[0].Error, "no digest") {
		t.Fatalf("apply without a digest: err %v %+v; want a refusal", err, res.Results)
	}
	if len(g.deleted) != 0 || g.writeCount() != 0 {
		t.Fatalf("deleted %v writes %v; want nothing written without a digest", g.deleted, g.writes)
	}
}

func TestStemcellRefsPruneUntracked_TrackedAndUntrackedRefusals(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), cacheTemplateTags(shaA))
	// 102's stale director tag is its only generation marker.
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB), untrackedTags(shaB, false)+";director--"+oldRef)
	addUntracked(f, 110, untrackedSha)

	for _, vmid := range []int64{101, 102} {
		_, plan := untrackedPlan(t, f, vmid)
		if !plan.Refused || !strings.Contains(plan.RefusalReason, "tracked template") ||
			!strings.Contains(plan.RefusalReason, "without --untracked") {
			t.Errorf("--untracked %d: refused %v reason %q; want a tracked-template refusal", vmid, plan.Refused,
				plan.RefusalReason)
		}
	}
	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 110})
	if !plan.Refused || !strings.Contains(plan.RefusalReason, "prune --vmid 110 --untracked") {
		t.Errorf("--vmid 110: refused %v reason %q; want a refusal that names --untracked", plan.Refused,
			plan.RefusalReason)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("a plan changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPruneUntracked_RefsAndUnknownRefsRefuse(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(120, untrackedNamePrefix+"cccc3333", provDesc("cccc3333", oldRef, newRef),
		untrackedTags("cccc3333", false))
	// 121 has a blank description and 122 one that does not parse.
	f.addTemplate(121, "bosh-stemcell-d-1-dddd4444", "", untrackedTags("dddd4444", false))
	f.addTemplate(122, "bosh-stemcell-e-1-eeee5555", "{not json", untrackedTags("eeee5555", false))

	_, plan := untrackedPlan(t, f, 120)
	if !plan.Refused || !strings.Contains(plan.RefusalReason, oldRef) || !strings.Contains(plan.RefusalReason, newRef) {
		t.Errorf("--untracked 120: refused %v reason %q; want a refusal that lists both refs", plan.Refused,
			plan.RefusalReason)
	}
	for _, vmid := range []int64{121, 122} {
		if _, plan := untrackedPlan(t, f, vmid); !plan.Refused {
			t.Errorf("--untracked %d: plan not refused for a template whose refs are unknown", vmid)
		}
	}
}

func TestStemcellRefsPruneUntracked_LinkedCloneAndIncompleteScanBlock(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-110-disk-0/vm-200-disk-0,size=4G"})

	deps, plan := untrackedPlan(t, f, 110)
	if !plan.Refused || !strings.Contains(plan.RefusalReason, "200") || len(plan.Actions) != 1 ||
		!slices.Equal(plan.Actions[0].LinkedClones, []int64{200}) {
		t.Fatalf("plan = %+v; want refused for linked clone 200", plan)
	}
	if _, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); !errors.Is(err,
		handlers.ErrStemcellRefsPruneRefused) {
		t.Fatalf("apply of a refused plan: err %v; want ErrStemcellRefsPruneRefused", err)
	}

	g := newPruneFake()
	addUntracked(g, 110, untrackedSha)
	g.addVM(200, "unreadable", "", nil)
	g.configErr[200] = &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}
	_, plan = untrackedPlan(t, g, 110)
	if !plan.Refused || !strings.Contains(plan.RefusalReason, "incomplete") {
		t.Fatalf("plan = %+v; want refused for an incomplete clone scan", plan)
	}
	if len(f.deleted)+len(g.deleted) != 0 || f.writeCount()+g.writeCount() != 0 {
		t.Fatal("a refused prune changed PVE")
	}
}

func TestStemcellRefsPruneUntracked_RunningRefuses(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.running[110] = true

	if _, plan := untrackedPlan(t, f, 110); !plan.Refused || !strings.Contains(plan.RefusalReason, "running") {
		t.Errorf("--untracked 110: refused %v reason %q; want a running refusal", plan.Refused, plan.RefusalReason)
	}

	// A template the listing showed stopped but PVE reports running under
	// the lock is kept.
	g := newPruneFake()
	addUntracked(g, 112, untrackedSha)
	deps, plan := untrackedPlan(t, g, 112)
	g.statusOverride[112] = "running"
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged ||
		!strings.Contains(res.Results[0].Error, "running") {
		t.Fatalf("apply: err %v %+v; want changed-since-plan for a running template", err, res.Results)
	}
	if len(g.deleted) != 0 || g.writeCount() != 0 {
		t.Fatalf("deleted %v writes %v; a running template must be left alone", g.deleted, g.writes)
	}
}

func TestStemcellRefsPruneUntracked_LockedRecheck(t *testing.T) {
	t.Parallel()
	// olderDesc is provenance from before path identity, which the
	// template may carry under its name with or without the sha8 suffix.
	olderDesc := `{"name":"test","version":"1.0","sha8":"` + untrackedSha + `","created":"2026-05-01T00:00:00Z"}`
	cases := []struct {
		name   string
		change func(f *pruneFake, g *pruneFakeGuest)
		want   string
		// desc, when set, replaces the template's description before the
		// plan.
		desc string
	}{
		{"marker", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["tags"] = untrackedTags(untrackedSha, false) + ";bosh-stemcell-cache"
		}, "generation marker bosh-stemcell-cache", ""},
		{"ref", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["description"] = provDesc(untrackedSha, newRef)
		}, "holds director_refs (" + newRef + ")", ""},
		{"pending", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["tags"] = untrackedTags(untrackedSha, false) + ";bosh-destroy-pending"
		}, "its tags changed", ""},
		{"renamed", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["name"] = "bosh-stemcell-test-1-0"
		}, "its name changed", olderDesc},
		{"other zero-ref provenance", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["description"] = strings.Replace(provDesc(untrackedSha), `"created":"2026-08-03T00:00:00Z"`,
				`"created":"2026-08-04T00:00:00Z"`, 1)
		}, "its description changed", ""},
		{"extra tag", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["tags"] = untrackedTags(untrackedSha, false) + ";keep-me"
		}, "its tags changed", ""},
		{"not a template", func(_ *pruneFake, g *pruneFakeGuest) {
			delete(g.cfg, "template")
		}, "no longer a template", ""},
		{"clone", func(f *pruneFake, _ *pruneFakeGuest) {
			f.guests[300] = &pruneFakeGuest{node: vmNode, name: "late-clone",
				cfg: map[string]any{"scsi0": "local-lvm:base-110-disk-0/vm-300-disk-0,size=4G"}}
		}, "linked clone(s) (300)", ""},
		{"unreadable guest", func(f *pruneFake, _ *pruneFakeGuest) {
			f.guests[301] = &pruneFakeGuest{node: vmNode, name: "late-vm", cfg: map[string]any{}}
			f.configErr[301] = &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}
		}, "could not read the config of vmid 301", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPruneFake()
			addUntracked(f, 110, untrackedSha)
			if tc.desc != "" {
				f.guests[110].cfg["description"] = tc.desc
			}
			deps, plan := untrackedPlan(t, f, 110)
			if plan.Refused {
				t.Fatalf("plan refused: %s", plan.RefusalReason)
			}
			changed := false
			f.onRead = func(vmid int64, g *pruneFakeGuest) {
				if vmid == 110 && !changed {
					changed = true
					tc.change(f, g)
				}
			}
			res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
			if err != nil || res.Failures != 1 || !strings.Contains(res.Results[0].Error, tc.want) {
				t.Fatalf("apply: err %v %+v; want a failure naming %q", err, res.Results, tc.want)
			}
			if len(f.deleted) != 0 || f.writeCount() != 0 {
				t.Fatalf("deleted %v writes %v; a changed template must be left alone", f.deleted, f.writes)
			}
		})
	}
}

func TestStemcellRefsPruneUntracked_GoneBeforeApply(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	deps, plan := untrackedPlan(t, f, 110)
	delete(f.guests, 110)
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeGone {
		t.Fatalf("apply: err %v %+v; want already-gone", err, res.Results)
	}
}

func TestStemcellRefsPruneUntracked_MovedBeforeApplyIsNotGone(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.checkNode = true
	addUntracked(f, 110, untrackedSha)
	deps, plan := untrackedPlan(t, f, 110)
	f.guests[110].node = "pve-node3"
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged {
		t.Fatalf("apply: err %v %+v; want changed-since-plan for a moved template", err, res.Results)
	}
	if e := res.Results[0].Error; !strings.Contains(e, "pve-node3") || !strings.Contains(e, "rerun") {
		t.Errorf("error = %q; want the new node and a rerun instruction", e)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("deleted %v writes %v; a moved template must be left alone", f.deleted, f.writes)
	}

	// When absence cannot be proven, the template is not reported as gone.
	g := newPruneFake()
	addUntracked(g, 110, untrackedSha)
	deps, plan = untrackedPlan(t, g, 110)
	delete(g.guests, 110)
	g.listErr = &sdkerrors.APIError{HTTPCode: 500, Message: "cluster resources unavailable"}
	res, err = handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeFailed {
		t.Fatalf("apply: err %v %+v; want a failure when the listing cannot confirm absence", err, res.Results)
	}
}

func TestStemcellRefsPruneUntracked_CloneRescanHasADeadline(t *testing.T) {
	// Not parallel: it shortens the package's rescan limit.
	restore := handlers.SetUntrackedCloneRescanTimeoutForTest(50 * time.Millisecond)
	defer restore()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.addVM(200, "vm", "", map[string]string{"scsi0": "local-lvm:vm-200-disk-0,size=4G"})
	deps, plan := untrackedPlan(t, f, 110)
	f.mu.Lock()
	f.blockRead[200] = true
	f.mu.Unlock()

	type outcome struct {
		res handlers.StemcellRefsApplyResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
		done <- outcome{res, err}
	}()
	var out outcome
	select {
	case out = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("apply still running after 10s; want the rescan limit to end it")
	}
	if out.err != nil || out.res.Failures != 1 || !strings.Contains(out.res.Results[0].Error, "did not finish within") {
		t.Fatalf("apply: err %v %+v; want the rescan cut off by its own limit", out.err, out.res.Results)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("deleted %v writes %v; an unfinished rescan must leave the template alone", f.deleted, f.writes)
	}
}

func TestStemcellRefsPruneUntracked_RepeatedVMIDsJudgedPerTemplate(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), cacheTemplateTags(shaA))
	addUntracked(f, 111, "abcd2222")
	f.running[111] = true
	addUntracked(f, 112, "abcd3333")
	// 155 is an untracked replica of live anchor 102. --untracked judges it
	// on its own, because 102's destroy would never sweep it.
	f.addTemplate(102, "bosh-stemcell-a-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(155, untrackedNamePrefix+shaB, provDesc(shaB), untrackedTags(shaB, true))

	deps, plan := untrackedPlan(t, f, 110, 101, 111, 999, 112, 110, 155)
	if plan.Refused || !slices.Equal(plan.VMIDs, []int64{110, 101, 111, 999, 112, 155}) || plan.VMID != 0 {
		t.Fatalf("plan refused %v vmids %v vmid %d; want each VMID judged once", plan.Refused, plan.VMIDs, plan.VMID)
	}
	kinds := map[int64]string{}
	for _, a := range plan.Actions {
		kinds[a.VMID] = a.Kind
	}
	for vmid, want := range map[int64]string{
		110: handlers.StemcellRefsActionDestroyUntracked, 101: handlers.StemcellRefsActionBlocked,
		111: handlers.StemcellRefsActionBlocked, 999: handlers.StemcellRefsActionBlocked,
		112: handlers.StemcellRefsActionDestroyUntracked, 155: handlers.StemcellRefsActionDestroyUntracked,
	} {
		if kinds[vmid] != want {
			t.Errorf("%d kind = %q; want %q", vmid, kinds[vmid], want)
		}
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 3 {
		t.Fatalf("apply: err %v failures %d %+v; want the three refusals as failures", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{110, 112, 155}) {
		t.Fatalf("deleted = %v; want [110 112 155]", f.deleted)
	}

	// Plain --vmid repeats too, and an untracked template among the VMIDs
	// is refused with a reason that names --untracked.
	g := newPruneFake()
	g.addTemplate(130, "bosh-stemcell-l-1-2", "", "bosh-stemcell-sha-llll1111;bosh-stemcell-node-pve-node2")
	addUntracked(g, 110, untrackedSha)
	deps, plan = scanAndPlan(t, g, handlers.StemcellRefsPruneRequest{VMIDs: []int64{130, 110}})
	if plan.Refused || len(plan.Actions) != 2 {
		t.Fatalf("plain plan = %+v; want two actions", plan)
	}
	if b := actionFor(t, plan, 110); b.Kind != handlers.StemcellRefsActionBlocked ||
		!strings.Contains(b.Reason, "--untracked") {
		t.Errorf("110 action = %+v; want blocked with a reason that names --untracked", b)
	}
	res, err = handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || !slices.Equal(g.deleted, []int64{130}) {
		t.Fatalf("plain apply: err %v failures %d deleted %v", err, res.Failures, g.deleted)
	}
}

func TestStemcellRefsPruneUntracked_RequestValidation(t *testing.T) {
	t.Parallel()
	for _, req := range []handlers.StemcellRefsPruneRequest{
		{Untracked: true},
		{Untracked: true, DirectorRef: oldRef},
		{Untracked: true, DirectorRef: oldRef, VMID: 110},
		{Untracked: true, ReconcileTags: true},
		{Untracked: true, VMID: 110, AllowOwnedVMs: true},
		{VMID: 110, VMIDs: []int64{111}},
		{VMIDs: []int64{110, 0}},
		{Untracked: true, VMIDs: []int64{-4}},
	} {
		if _, err := handlers.PlanStemcellRefsPrune(handlers.StemcellRefsScan{}, req); err == nil {
			t.Errorf("request %+v accepted", req)
		}
	}
}

func TestStemcellRefsPruneUntracked_ReportNamesCommandOrBlockers(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	addUntracked(f, 111, "abcd2222")
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-111-disk-0/vm-200-disk-0"})
	f.addTemplate(103, "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", "", "")

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	rowsFor := func(rep handlers.StemcellRefsReport) map[int64]handlers.StemcellRefsUnreferenced {
		rows := map[int64]handlers.StemcellRefsUnreferenced{}
		for _, u := range rep.Unreferenced {
			rows[u.VMID] = u
		}
		return rows
	}
	rows := rowsFor(handlers.BuildStemcellRefsReportFor(scan, "/var/vcap/jobs/pve_cpi/config/cpi.json"))
	if u := rows[110]; !u.Freeable ||
		u.FreeCommand != "cpi stemcell-refs prune --config /var/vcap/jobs/pve_cpi/config/cpi.json --vmid 110 --untracked" {
		t.Errorf("110 row = %+v; want the --untracked command with the config path", u)
	}
	if u := rows[111]; u.Freeable || u.FreeCommand != "" || !slices.Equal(u.BlockingClones, []int64{200}) {
		t.Errorf("111 row = %+v; want clone 200 named as the blocker", u)
	}
	if u := rows[103]; !u.Freeable ||
		u.FreeCommand != "cpi stemcell-refs prune --config /var/vcap/jobs/pve_cpi/config/cpi.json --vmid 103" {
		t.Errorf("103 row = %+v; want the plain --vmid command with the config path", u)
	}

	// A path the shell would split is quoted, and a report built without a
	// path prints the placeholder the adopt-owners commands use.
	rows = rowsFor(handlers.BuildStemcellRefsReportFor(scan, "/tmp/my cpi's.json"))
	if u := rows[103]; u.FreeCommand != `cpi stemcell-refs prune --config '/tmp/my cpi'\''s.json' --vmid 103` {
		t.Errorf("103 row = %+v; want the config path shell-quoted", u)
	}
	rows = rowsFor(handlers.BuildStemcellRefsReport(scan))
	if u := rows[103]; u.FreeCommand != "cpi stemcell-refs prune --config <cpi.json> --vmid 103" {
		t.Errorf("103 row = %+v; want the <cpi.json> placeholder", u)
	}
}

// legacyMinimalDesc is the description a CPI that predates path-identity
// stemcells wrote when stemcell provenance was off: every provenance string
// field empty, and the retired stemcell_refs count naming the template.
func legacyMinimalDesc(vmid int64) string {
	return `{"name":"","version":"","sha8":"","created":"","stemcell_refs":"template:` +
		strconv.FormatInt(vmid, 10) + `"}`
}

func TestStemcellRefsPruneUntracked_LegacyMinimalProvenanceQualifies(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	// The fields a real legacy template carries: a name with no sha suffix,
	// minimal notes, and the sha8 only in its tag.
	f.addTemplate(30430, "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", legacyMinimalDesc(30430),
		"bosh-cpi;bosh-stemcell-sha-891b3b74")
	f.guests[30430].cfg["virtio0"] = "local-lvm-data:base-30430-disk-0,size=5G"
	f.sizes["local-lvm-data:base-30430-disk-0"] = 5 * gib

	deps, plan := untrackedPlan(t, f, 30430)
	if plan.Refused || len(plan.Actions) != 1 || plan.Actions[0].Kind != handlers.StemcellRefsActionDestroyUntracked ||
		plan.Actions[0].SHA8 != "891b3b74" {
		t.Fatalf("plan = %+v; want one destroy-untracked action for sha8 891b3b74", plan)
	}
	if got := plan.Actions[0].LegacyRefs; !slices.Equal(got, []string{"template:30430"}) {
		t.Errorf("legacy refs = %q; want the self-reference template:30430 shown", got)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 || !slices.Equal(f.deleted, []int64{30430}) {
		t.Fatalf("apply: err %v deleted %v %+v; want 30430 destroyed", err, f.deleted, res.Results)
	}
}

func TestStemcellRefsPruneUntracked_IdentityNeedsNameAndProvenance(t *testing.T) {
	t.Parallel()
	const (
		hexA = "a1b2c3d4"
		hexB = "0dcd8626"
	)
	nameA, nameB := untrackedNamePrefix+hexA, untrackedNamePrefix+hexB
	tagA := "bosh-cpi;bosh-stemcell-sha-" + hexA
	minimal := func(extra string) string {
		return `{"name":"","version":"","sha8":"","created":""` + extra + `}`
	}
	cases := []struct {
		label, name, desc, tags, want string
	}{
		{"golden image", "ubuntu-golden-image", `{"kind":"golden","owner":"team-x"}`, "",
			`name "ubuntu-golden-image" does not follow stemcell cache naming`},
		{"null description", nameA, "null", untrackedTags(hexA, false),
			"lacks the string field(s) name, version, sha8, created"},
		{"empty object", nameA, "{}", untrackedTags(hexA, false),
			"lacks the string field(s) name, version, sha8, created"},
		{"golden kind under a cache name", nameA, `{"kind":"golden"}`, untrackedTags(hexA, false),
			"is not stemcell provenance"},
		{"description sha8 differs from the tag", nameA, provDesc(hexA), untrackedTags(hexB, false),
			"description sha8 " + hexA + " does not match its bosh-stemcell-sha-" + hexB + " tag"},
		{"name suffix differs from the sha8", nameB, provDesc(hexA), untrackedTags(hexA, false),
			"name suffix " + hexB + " does not match sha8 " + hexA},
		{"minimal notes name suffix differs from the tag", "bosh-stemcell-u-1-" + hexB, legacyMinimalDesc(140),
			tagA, "name suffix " + hexB + " does not match sha8 " + hexA},
		{"two sha tags disagree", nameA, provDesc(hexA),
			untrackedTags(hexA, false) + ";bosh-stemcell-sha-" + hexB, "conflicting bosh-stemcell-sha- tags"},
		{"provenance without a sha8", "bosh-stemcell-u-1",
			`{"name":"u","version":"1","sha8":"","created":"2026-08-03T00:00:00Z"}`, "bosh-cpi",
			"provenance records no sha8"},
		{"minimal notes without a sha tag", "bosh-stemcell-u-1", legacyMinimalDesc(140), "bosh-cpi",
			"carries no bosh-stemcell-sha- tag"},
		{"full provenance without a sha tag", "bosh-stemcell-golden-1",
			`{"name":"golden","version":"1","sha8":"deadbeef","created":""}`, "",
			"carries no bosh-stemcell-sha- tag"},
		{"suffix-free full provenance without a sha tag", "bosh-stemcell-golden",
			`{"name":"golden","version":"1","sha8":"deadbeef","created":""}`, "bosh-cpi",
			"carries no bosh-stemcell-sha- tag"},
		{"name names another stemcell", "bosh-stemcell-other-9",
			`{"name":"u","version":"1","sha8":"` + hexA + `","created":""}`, tagA,
			`name "bosh-stemcell-other-9" does not match its provenance, stemcell u version 1`},
		{"name names another version", "bosh-stemcell-u-2-" + hexA,
			`{"name":"u","version":"1","sha8":"` + hexA + `","created":""}`, tagA,
			`name "bosh-stemcell-u-2-` + hexA + `" does not match its provenance, stemcell u version 1`},
		{"path identity without a name suffix", "bosh-stemcell-test-1-0", provDesc(hexA), untrackedTags(hexA, false),
			"has no sha8 suffix"},
		{"minimal notes without stemcell_refs", "bosh-stemcell-u-1", minimal(""), tagA,
			"lacks the stemcell_refs field"},
		{"minimal notes naming another template", "bosh-stemcell-u-1",
			minimal(`,"stemcell_refs":"template:99999"`), tagA,
			`stemcell_refs entry "template:99999" is not template:140`},
		{"minimal notes with a garbage ref", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":"x"`), tagA,
			`stemcell_refs entry "x" is not template:140`},
		{"minimal notes with one foreign ref among its own", "bosh-stemcell-u-1",
			minimal(`,"stemcell_refs":"template:140,template:7"`), tagA,
			`stemcell_refs entry "template:7" is not template:140`},
		{"minimal notes with extra fields", "bosh-stemcell-u-1",
			minimal(`,"stemcell_refs":"template:140","purpose":"y","owner":"x"`), tagA,
			"extra field(s) owner, purpose"},
		{"minimal notes with a non-string ref", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":5`), tagA,
			"stemcell_refs field is not a string"},
		{"minimal notes whose refs were emptied", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":""`), tagA,
			"stemcell_refs field is empty"},
		{"minimal notes with comma-only refs", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":" , "`), tagA,
			"stemcell_refs field is empty"},
		{"minimal notes with null refs", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":null`), tagA,
			"stemcell_refs field is not a string"},
		{"minimal notes with a null name", "bosh-stemcell-u-1",
			`{"name":null,"version":"","sha8":"","created":"","stemcell_refs":"template:140"}`, tagA,
			"lacks the string field(s) name"},
		{"full provenance with a null sha8", nameA,
			`{"name":"test","version":"1.0","sha8":null,"created":""}`, untrackedTags(hexA, false),
			"lacks the string field(s) sha8"},
		{"full provenance with a null created", nameA,
			`{"name":"test","version":"1.0","sha8":"` + hexA + `","created":null}`, untrackedTags(hexA, false),
			"lacks the string field(s) created"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			f := newPruneFake()
			f.addTemplate(140, tc.name, tc.desc, tc.tags)
			_, plan := untrackedPlan(t, f, 140)
			if !plan.Refused || !strings.Contains(plan.RefusalReason, tc.want) {
				t.Fatalf("refused %v reason %q; want a refusal naming %q", plan.Refused, plan.RefusalReason, tc.want)
			}
			if len(f.deleted) != 0 || f.writeCount() != 0 {
				t.Fatalf("a refused plan changed PVE: writes %v deleted %v", f.writes, f.deleted)
			}
		})
	}

	qualifying := []struct {
		label, name, desc, tags string
		legacyRefs              []string
	}{
		// A provenance write from before the sha suffix keeps an older name,
		// and its sha8 tag agrees.
		{"older provenance under an older name", "bosh-stemcell-u-1-2",
			`{"name":"u","version":"1.2","sha8":"` + hexA + `","created":"2026-05-01T00:00:00Z"}`,
			"bosh-cpi;bosh-stemcell-sha-" + hexA + ";bosh-stemcell", nil},
		{"older provenance under a suffixed name", "bosh-stemcell-u-1-2-" + hexA,
			`{"name":"u","version":"1.2","sha8":"` + hexA + `","created":"2026-05-01T00:00:00Z"}`, tagA, nil},
		{"minimal notes naming the template", "bosh-stemcell-u-1", minimal(`,"stemcell_refs":" template:140 "`), tagA,
			[]string{"template:140"}},
	}
	for _, tc := range qualifying {
		f := newPruneFake()
		f.addTemplate(140, tc.name, tc.desc, tc.tags)
		_, plan := untrackedPlan(t, f, 140)
		if plan.Refused || len(plan.Actions) != 1 || plan.Actions[0].Kind != handlers.StemcellRefsActionDestroyUntracked {
			t.Errorf("%s: refused %v reason %q; want it to qualify", tc.label, plan.Refused, plan.RefusalReason)
			continue
		}
		if got := plan.Actions[0].LegacyRefs; !slices.Equal(got, tc.legacyRefs) {
			t.Errorf("%s: legacy refs = %q; want %q", tc.label, got, tc.legacyRefs)
		}
	}
}

func TestStemcellRefsPruneUntracked_ResumesPendingDestroy(t *testing.T) {
	t.Parallel()
	const cfgPath = "/var/vcap/jobs/pve_cpi/config/cpi.json"
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.deleteErr[110] = &sdkerrors.APIError{HTTPCode: 500, Message: "storage busy"}
	req := handlers.StemcellRefsPruneRequest{VMID: 110, Untracked: true, ConfigPath: cfgPath}

	// The first apply stamps bosh-destroy-pending, then the destroy fails.
	deps, plan := scanAndPlan(t, f, req)
	if plan.Refused || plan.Actions[0].ResumesPending {
		t.Fatalf("plan = %+v; want a fresh destroy", plan)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeFailed {
		t.Fatalf("apply: err %v %+v; want a failed destroy", err, res.Results)
	}
	wantCmd := "cpi stemcell-refs prune --config " + cfgPath + " --vmid 110 --untracked --apply"
	if e := res.Results[0].Error; !strings.Contains(e, "bosh-destroy-pending") || !strings.Contains(e, wantCmd) {
		t.Fatalf("error = %q; want it to say the template is marked pending and to name %q", e, wantCmd)
	}
	if !strings.Contains(f.cfgString(110, "tags"), "bosh-destroy-pending") || len(f.deleted) != 0 {
		t.Fatalf("tags %q deleted %v; want the stamp kept and the template in place", f.cfgString(110, "tags"),
			f.deleted)
	}

	// The rerun the error names resumes the destroy.
	delete(f.deleteErr, 110)
	deps, plan = scanAndPlan(t, f, req)
	if plan.Refused || len(plan.Actions) != 1 || !plan.Actions[0].ResumesPending ||
		plan.Actions[0].Kind != handlers.StemcellRefsActionDestroyUntracked {
		t.Fatalf("rerun plan = %+v; want a destroy-untracked that resumes the pending destroy", plan)
	}
	writes := f.writeCount()
	res, err = handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeApplied {
		t.Fatalf("rerun apply: err %v %+v; want applied", err, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{110}) || f.writeCount() != writes {
		t.Fatalf("deleted %v writes %v; want 110 destroyed with no second stamp", f.deleted, f.writes)
	}
}

func TestStemcellRefsPruneUntracked_ChangeDuringCloneRescanIsCaught(t *testing.T) {
	t.Parallel()
	// markDuringRescan adds bosh-stemcell-cache to 110 when the lock-held
	// clone rescan reads guest 200, the way an edit in the PVE UI would.
	markDuringRescan := func(f *pruneFake) {
		marked := false
		f.onRead = func(vmid int64, _ *pruneFakeGuest) {
			if vmid == 200 && !marked {
				marked = true
				g := f.guests[110]
				g.cfg["tags"] = g.cfg["tags"].(string) + ";bosh-stemcell-cache"
				g.gen++
			}
		}
	}
	setup := func() *pruneFake {
		f := newPruneFake()
		addUntracked(f, 110, untrackedSha)
		f.addVM(200, "vm", "", map[string]string{"scsi0": "local-lvm:vm-200-disk-0,size=4G"})
		return f
	}
	wantChanged := func(t *testing.T, label string, res handlers.StemcellRefsApplyResult, err error) {
		t.Helper()
		if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged ||
			!strings.Contains(res.Results[0].Error, "changed during the linked-clone check") {
			t.Fatalf("%s: err %v %+v; want changed-since-plan for an edit during the rescan", label, err, res.Results)
		}
	}

	// A fresh destroy.
	f := setup()
	deps, plan := untrackedPlan(t, f, 110)
	markDuringRescan(f)
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	wantChanged(t, "fresh", res, err)
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("fresh: deleted %v writes %v; want the template left alone", f.deleted, f.writes)
	}

	// A resumed destroy, whose stamp is already in place, so no
	// digest-guarded write would notice the edit.
	g := setup()
	g.deleteErr[110] = &sdkerrors.APIError{HTTPCode: 500, Message: "storage busy"}
	deps, plan = untrackedPlan(t, g, 110)
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 1 {
		t.Fatalf("first apply: err %v %+v; want a failed destroy that leaves the stamp", err, res.Results)
	}
	delete(g.deleteErr, 110)
	deps, plan = untrackedPlan(t, g, 110)
	if plan.Refused || !plan.Actions[0].ResumesPending {
		t.Fatalf("rerun plan = %+v; want a resume", plan)
	}
	writes := g.writeCount()
	markDuringRescan(g)
	res, err = handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	wantChanged(t, "resume", res, err)
	if len(g.deleted) != 0 || g.writeCount() != writes {
		t.Fatalf("resume: deleted %v writes %v; want the now-tracked template left alone", g.deleted, g.writes)
	}
}

func TestStemcellRefsPruneUntracked_BaseVolumeInUseNamesTheRerun(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.deleteErr[110] = &sdkerrors.APIError{HTTPCode: 400,
		Message: "base volume 'base-110-disk-0' is still in use by linked clone"}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 110, Untracked: true,
		ConfigPath: "/x/cpi.json"})
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeFailed {
		t.Fatalf("apply: err %v %+v; want a failed destroy", err, res.Results)
	}
	want := "delete or migrate them. The template now carries bosh-destroy-pending, and rerunning " +
		"cpi stemcell-refs prune --config /x/cpi.json --vmid 110 --untracked --apply resumes the destroy"
	if e := res.Results[0].Error; !strings.Contains(e, want) {
		t.Fatalf("error = %q; want it to contain %q", e, want)
	}
}
