package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

const (
	untrackedSha  = "uuuu1111"
	untrackedName = "bosh-stemcell-u-1-" + untrackedSha
	gib           = int64(1) << 30
	mib           = int64(1) << 20
)

// addUntracked adds untracked template vmid with no ref and one owned disk.
func addUntracked(f *pruneFake, vmid int64, sha8 string) {
	vol := "local-lvm:base-" + strconv.FormatInt(vmid, 10) + "-disk-0"
	f.addTemplate(vmid, "bosh-stemcell-u-1-"+sha8, provDesc(sha8), untrackedTags(sha8, false))
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
	f.addTemplate(120, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", oldRef, newRef),
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

func TestStemcellRefsPruneUntracked_RunningAndDestroyPendingRefuse(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.running[110] = true
	addUntracked(f, 111, "vvvv2222")
	f.guests[111].cfg["tags"] = untrackedTags("vvvv2222", false) + ";bosh-destroy-pending"

	if _, plan := untrackedPlan(t, f, 110); !plan.Refused || !strings.Contains(plan.RefusalReason, "running") {
		t.Errorf("--untracked 110: refused %v reason %q; want a running refusal", plan.Refused, plan.RefusalReason)
	}
	if _, plan := untrackedPlan(t, f, 111); !plan.Refused ||
		!strings.Contains(plan.RefusalReason, "bosh-destroy-pending") {
		t.Errorf("--untracked 111: refused %v reason %q; want a destroy-pending refusal", plan.Refused,
			plan.RefusalReason)
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
	cases := []struct {
		name   string
		change func(f *pruneFake, g *pruneFakeGuest)
		want   string
	}{
		{"marker", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["tags"] = untrackedTags(untrackedSha, false) + ";bosh-stemcell-cache"
		}, "generation marker bosh-stemcell-cache"},
		{"ref", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["description"] = provDesc(untrackedSha, newRef)
		}, "holds director_refs (" + newRef + ")"},
		{"pending", func(_ *pruneFake, g *pruneFakeGuest) {
			g.cfg["tags"] = untrackedTags(untrackedSha, false) + ";bosh-destroy-pending"
		}, "bosh-destroy-pending is already set"},
		{"not a template", func(_ *pruneFake, g *pruneFakeGuest) {
			delete(g.cfg, "template")
		}, "no longer a template"},
		{"clone", func(f *pruneFake, _ *pruneFakeGuest) {
			f.guests[300] = &pruneFakeGuest{node: vmNode, name: "late-clone",
				cfg: map[string]any{"scsi0": "local-lvm:base-110-disk-0/vm-300-disk-0,size=4G"}}
		}, "linked clone(s) (300)"},
		{"unreadable guest", func(f *pruneFake, _ *pruneFakeGuest) {
			f.guests[301] = &pruneFakeGuest{node: vmNode, name: "late-vm", cfg: map[string]any{}}
			f.configErr[301] = &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}
		}, "could not read the config of vmid 301"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPruneFake()
			addUntracked(f, 110, untrackedSha)
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

func TestStemcellRefsPruneUntracked_RepeatedVMIDsJudgedPerTemplate(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addUntracked(f, 110, untrackedSha)
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), cacheTemplateTags(shaA))
	addUntracked(f, 111, "vvvv2222")
	f.running[111] = true
	addUntracked(f, 112, "wwww3333")
	// 155 is an untracked replica of live anchor 102. --untracked judges it
	// on its own, because 102's destroy would never sweep it.
	f.addTemplate(102, "bosh-stemcell-a-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(155, "bosh-stemcell-a-1-"+shaB, provDesc(shaB), untrackedTags(shaB, true))

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
	addUntracked(f, 111, "vvvv2222")
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-111-disk-0/vm-200-disk-0"})
	f.addTemplate(103, "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", "", "")

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	rows := map[int64]handlers.StemcellRefsUnreferenced{}
	for _, u := range handlers.BuildStemcellRefsReport(scan).Unreferenced {
		rows[u.VMID] = u
	}
	if u := rows[110]; !u.Freeable || u.FreeCommand != "cpi stemcell-refs prune --vmid 110 --untracked" {
		t.Errorf("110 row = %+v; want the --untracked command", u)
	}
	if u := rows[111]; u.Freeable || u.FreeCommand != "" || !slices.Equal(u.BlockingClones, []int64{200}) {
		t.Errorf("111 row = %+v; want clone 200 named as the blocker", u)
	}
	if u := rows[103]; !u.Freeable || u.FreeCommand != "cpi stemcell-refs prune --vmid 103" {
		t.Errorf("103 row = %+v; want the plain --vmid command", u)
	}
}
