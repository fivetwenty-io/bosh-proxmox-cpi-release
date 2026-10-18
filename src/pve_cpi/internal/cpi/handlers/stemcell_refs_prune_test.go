package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// pruneFakeGuest is one guest in pruneFake's cluster.
type pruneFakeGuest struct {
	node     string
	name     string
	template bool
	cfg      map[string]any
}

// pruneFake is a small stateful cluster for the stemcell-refs tests: the
// listing, config reads, config writes, and destroys all act on one map, so
// a test can check what an apply actually changed.
type pruneFake struct {
	mu        sync.Mutex
	guests    map[int64]*pruneFakeGuest
	configErr map[int64]error
	deleteErr map[int64]error
	// writes records every UpdateQemuConfig as "vmid:field", in order.
	writes []string
	// deleted records destroyed VMIDs in order.
	deleted []int64
	// tagsAtDelete records each destroyed guest's tags at destroy time.
	tagsAtDelete map[int64]string
}

func newPruneFake() *pruneFake {
	return &pruneFake{
		guests:       map[int64]*pruneFakeGuest{},
		configErr:    map[int64]error{},
		deleteErr:    map[int64]error{},
		tagsAtDelete: map[int64]string{},
	}
}

func (f *pruneFake) addTemplate(vmid int64, name, desc, tags string) {
	f.guests[vmid] = &pruneFakeGuest{node: vmNode, name: name, template: true,
		cfg: map[string]any{"description": desc, "tags": tags}}
}

func (f *pruneFake) addVM(vmid int64, name, tags string, disks map[string]string) {
	cfg := map[string]any{"tags": tags}
	for k, v := range disks {
		cfg[k] = v
	}
	f.guests[vmid] = &pruneFakeGuest{node: vmNode, name: name, cfg: cfg}
}

func (f *pruneFake) cfgString(vmid int64, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.guests[vmid]
	if !ok {
		return ""
	}
	s, _ := g.cfg[key].(string)
	return s
}

func (f *pruneFake) setCfg(vmid int64, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guests[vmid].cfg[key] = value
}

func (f *pruneFake) deps() handlers.Deps {
	qemu := &stemcellMockQEMU{configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := f.configErr[int64(vmid)]; err != nil {
			return nil, err
		}
		g, ok := f.guests[int64(vmid)]
		if !ok {
			return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
		}
		out := map[string]any{}
		for k, v := range g.cfg {
			out[k] = v
		}
		return out, nil
	}}
	nodes := &stemcellMockNodes{
		updateConfigFn: func(_ context.Context, _, vmidStr string, p *sdknodes.UpdateQemuConfigParams) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			id, _ := strconv.ParseInt(vmidStr, 10, 64)
			g, ok := f.guests[id]
			if !ok {
				return &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
			}
			if p.Description != nil {
				g.cfg["description"] = *p.Description
				f.writes = append(f.writes, vmidStr+":description")
			}
			if p.Tags != nil {
				g.cfg["tags"] = *p.Tags
				f.writes = append(f.writes, vmidStr+":tags")
			}
			if p.Delete != nil && *p.Delete == "tags" {
				delete(g.cfg, "tags")
				f.writes = append(f.writes, vmidStr+":tags")
			}
			return nil
		},
		deleteQemuFn: func(_ context.Context, _, vmidStr string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			id, _ := strconv.ParseInt(vmidStr, 10, 64)
			if err := f.deleteErr[id]; err != nil {
				return nil, err
			}
			if g, ok := f.guests[id]; ok {
				f.tagsAtDelete[id], _ = g.cfg["tags"].(string)
			}
			delete(f.guests, id)
			f.deleted = append(f.deleted, id)
			raw := sdknodes.DeleteQemuResponse(`""`)
			return &raw, nil
		},
	}
	cluster := &stemcellMockCluster{listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make(sdkcluster.ListResourcesResponse, 0, len(f.guests))
		for vmid, g := range f.guests {
			tmpl := 0
			if g.template {
				tmpl = 1
			}
			tags, _ := g.cfg["tags"].(string)
			raw, _ := json.Marshal(map[string]any{
				"type": "qemu", "vmid": vmid, "node": g.node, "name": g.name, "tags": tags, "template": tmpl,
			})
			out = append(out, raw)
		}
		return &out, nil
	}}
	return buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemu, nodesSvc: nodes, clusterSvc: cluster})
}

func (f *pruneFake) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func provDesc(sha8 string, refs ...string) string { return directorRefsDescJSON(sha8, refs...) }

func descRefs(t *testing.T, desc string) []string {
	t.Helper()
	var p struct {
		DirectorRefs []string `json:"director_refs"`
	}
	if err := json.Unmarshal([]byte(desc), &p); err != nil {
		t.Fatalf("description is not JSON: %v (%q)", err, desc)
	}
	return p.DirectorRefs
}

func scanAndPlan(t *testing.T, f *pruneFake, req handlers.StemcellRefsPruneRequest) (handlers.Deps, handlers.StemcellRefsPlan) {
	t.Helper()
	deps := f.deps()
	scan, err := handlers.ScanStemcellRefs(context.Background(), deps)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := handlers.PlanStemcellRefsPrune(scan, req)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return deps, plan
}

func actionFor(t *testing.T, plan handlers.StemcellRefsPlan, vmid int64) handlers.StemcellRefsAction {
	t.Helper()
	for i := range plan.Actions {
		if plan.Actions[i].VMID == vmid {
			return plan.Actions[i]
		}
	}
	t.Fatalf("no plan action for vmid %d in %+v", vmid, plan.Actions)
	return handlers.StemcellRefsAction{}
}

const (
	oldRef = "11111111-old"
	newRef = "22222222-new"
	shaA   = "aaaa1111"
	shaB   = "bbbb2222"
)

func TestStemcellRefsReport(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(103, "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", "", "")
	f.addTemplate(104, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", "debug-repro"), cacheTemplateTags("cccc3333"))
	f.addTemplate(9000, "ubuntu-noble-base", "", "")
	f.addVM(200, "vm-clone", "bosh-owner--"+newRef, map[string]string{"scsi0": "local-lvm:base-102-disk-0/vm-200-disk-0,size=10G"})

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	rep := handlers.BuildStemcellRefsReport(scan)
	if len(rep.Templates) != 4 {
		t.Fatalf("templates = %d, want 4 (base image excluded): %+v", len(rep.Templates), rep.Templates)
	}
	byID := map[int64]handlers.StemcellRefsTemplate{}
	for _, tm := range rep.Templates {
		byID[tm.VMID] = tm
	}
	if byID[101].RefsTagsDisagree {
		t.Error("101: refs and tags agree, but the report says they disagree")
	}
	if !byID[104].RefsTagsDisagree {
		t.Error("104: ref debug-repro has no tag, but the report says they agree")
	}
	if !slices.Equal(byID[102].LinkedClones, []int64{200}) {
		t.Errorf("102 linked clones = %v, want [200]", byID[102].LinkedClones)
	}
	if !byID[103].Legacy {
		t.Error("103 should be flagged legacy")
	}

	refs := map[string]handlers.StemcellRefsRefSummary{}
	for _, r := range rep.Refs {
		refs[r.Ref] = r
	}
	old := refs[oldRef]
	if !slices.Equal(old.DescriptionTemplates, []int64{101, 102}) {
		t.Errorf("old ref templates = %v", old.DescriptionTemplates)
	}
	if len(old.WouldFree) != 0 || !slices.Equal(old.Blocked, []int64{102}) {
		t.Errorf("old ref would free %v blocked %v; want none freed and 102 blocked by its clone", old.WouldFree, old.Blocked)
	}
	if refs[newRef].OwnedVMCount != 1 {
		t.Errorf("new ref owned VMs = %d, want 1", refs[newRef].OwnedVMCount)
	}
	if !slices.Equal(refs["debug-repro"].WouldFree, []int64{104}) {
		t.Errorf("debug-repro would free %v, want [104]", refs["debug-repro"].WouldFree)
	}
	if len(rep.Unreferenced) != 1 || rep.Unreferenced[0].VMID != 103 || !rep.Unreferenced[0].Freeable {
		t.Errorf("unreferenced = %+v, want legacy 103 freeable", rep.Unreferenced)
	}
	if f.writeCount() != 0 {
		t.Errorf("report wrote %d times; it must be read-only", f.writeCount())
	}
}

func TestStemcellRefsPrune_DryRunMatchesApply(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: "  " + oldRef + " "})
	if f.writeCount() != 0 {
		t.Fatalf("planning wrote %d times; the dry run must change nothing", f.writeCount())
	}
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRef || a.DropTag != "director--"+oldRef ||
		!slices.Equal(a.RemainingRefs, []string{newRef}) {
		t.Errorf("101 action = %+v", a)
	}
	if b := actionFor(t, plan, 102); b.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy {
		t.Errorf("102 action = %+v", b)
	}
	if plan.HasFailures() {
		t.Errorf("plan has failures: %+v", plan)
	}

	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 0 {
		t.Fatalf("apply failures = %d: %+v", res.Failures, res.Results)
	}
	if len(res.Results) != len(plan.Actions) {
		t.Fatalf("results %d, actions %d", len(res.Results), len(plan.Actions))
	}
	for i, r := range res.Results {
		if r.Action.Kind != plan.Actions[i].Kind || r.Action.VMID != plan.Actions[i].VMID {
			t.Errorf("result %d ran %s on %d; the plan said %s on %d",
				i, r.Action.Kind, r.Action.VMID, plan.Actions[i].Kind, plan.Actions[i].VMID)
		}
		if r.Outcome != handlers.StemcellRefsOutcomeApplied {
			t.Errorf("result %d outcome %s", i, r.Outcome)
		}
	}
	if got := descRefs(t, f.cfgString(101, "description")); !slices.Equal(got, []string{newRef}) {
		t.Errorf("101 refs after apply = %v", got)
	}
	if tags := f.cfgString(101, "tags"); strings.Contains(tags, "director--"+oldRef) || !strings.Contains(tags, "director--"+newRef) {
		t.Errorf("101 tags after apply = %q", tags)
	}
	if !slices.Equal(f.deleted, []int64{102}) {
		t.Errorf("deleted = %v, want [102]", f.deleted)
	}
	if !strings.Contains(f.tagsAtDelete[102], "bosh-destroy-pending") {
		t.Errorf("102 destroyed without the pending stamp; tags were %q", f.tagsAtDelete[102])
	}
}

func TestStemcellRefsPrune_OwnedVMsRefuse(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addVM(300, "live-vm", "bosh-owner--"+strings.ToLower(oldRef), nil)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if !plan.Refused || len(plan.OwnedVMs) != 1 || plan.OwnedVMs[0].VMID != 300 {
		t.Fatalf("plan = %+v; want refusal naming VM 300", plan)
	}
	if _, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); !errors.Is(err, handlers.ErrStemcellRefsPruneRefused) {
		t.Fatalf("apply err = %v, want refusal", err)
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("refused apply changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}

	_, allowed := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef, AllowOwnedVMs: true})
	if allowed.Refused {
		t.Fatalf("--allow-owned-vms plan still refused: %+v", allowed)
	}
}

func TestStemcellRefsPrune_TagOnly(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	// 101 holds newRef in its description and only a stale tag for oldRef.
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	// 102's stale tag is its only generation marker, so the prune keeps it.
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, newRef), "bosh-stemcell-sha-"+shaB+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionRemoveTag || a.DropTag != "director--"+oldRef {
		t.Fatalf("101 action = %+v", a)
	}
	if b := actionFor(t, plan, 102); b.Kind != handlers.StemcellRefsActionSkip || b.Failure {
		t.Fatalf("102 action = %+v; want a non-failing skip that keeps the marker", b)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if tags := f.cfgString(101, "tags"); strings.Contains(tags, "director--"+oldRef) {
		t.Errorf("101 tags = %q; stale tag still present", tags)
	}
	if got := descRefs(t, f.cfgString(101, "description")); !slices.Equal(got, []string{newRef}) {
		t.Errorf("101 description refs changed to %v", got)
	}
	if len(f.deleted) != 0 {
		t.Errorf("tag-only prune destroyed %v", f.deleted)
	}
	if !strings.Contains(f.cfgString(102, "tags"), "director--"+oldRef) {
		t.Error("102 lost its only generation marker")
	}
}

func TestStemcellRefsPrune_UnparseableDescription(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, "hand-edited notes", cacheTemplateTags(shaA)+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionSkip || !a.Failure {
		t.Fatalf("101 action = %+v; want a failing skip", a)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 1 || f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("failures %d writes %v deleted %v", res.Failures, f.writes, f.deleted)
	}
}

func TestStemcellRefsPrune_LastRefDestroySweepsReplicas(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";bosh-stemcell-node-pve-node2")
	f.guests[150].node = "pve-node2"

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy || len(a.SweepReplicas) != 1 || a.SweepReplicas[0].VMID != 150 {
		t.Fatalf("101 action = %+v; want destroy with replica 150 swept", a)
	}
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionSkip || r.Failure {
		t.Fatalf("replica action = %+v; want a non-failing skip", r)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{101, 150}) {
		t.Fatalf("deleted = %v, want [101 150]", f.deleted)
	}
}

func TestStemcellRefsPrune_ReplicaKeptWhileAnchorSurvives(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";bosh-stemcell-node-pve-node2")

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionRemoveRef || len(a.SweepReplicas) != 0 {
		t.Fatalf("101 action = %+v", a)
	}
	_, vplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 150})
	if !vplan.Refused {
		t.Fatalf("--vmid on a replica of a live template was not refused: %+v", vplan)
	}
}

func TestStemcellRefsPrune_LinkedCloneBlocks(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addVM(200, "clone", "", map[string]string{"virtio0": "nfs:102/base-102-disk-0.qcow2/200/vm-200-disk-0.qcow2,size=8G"})

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	b := actionFor(t, plan, 102)
	if b.Kind != handlers.StemcellRefsActionBlocked || !b.Failure || !slices.Equal(b.LinkedClones, []int64{200}) {
		t.Fatalf("102 action = %+v; want blocked by clone 200", b)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 1 || f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("failures %d writes %v deleted %v", res.Failures, f.writes, f.deleted)
	}
	if got := descRefs(t, f.cfgString(102, "description")); !slices.Equal(got, []string{oldRef}) {
		t.Errorf("blocked template's refs changed to %v", got)
	}
}

func TestStemcellRefsPrune_IncompleteCloneScanBlocksDestroy(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addVM(200, "unreadable", "", nil)
	f.configErr[200] = &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if plan.CloneScanComplete {
		t.Fatal("scan reported complete despite an unreadable guest")
	}
	if b := actionFor(t, plan, 102); b.Kind != handlers.StemcellRefsActionBlocked {
		t.Fatalf("102 action = %+v; want blocked", b)
	}
}

func TestStemcellRefsPrune_BaseVolumeInUseIsReported(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(103, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", oldRef), cacheTemplateTags("cccc3333")+";director--"+oldRef)
	f.deleteErr[102] = &sdkerrors.APIError{HTTPCode: 400, Message: "base volume 'base-102-disk-0' is still in use by linked clone"}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 1 {
		t.Fatalf("failures = %d, want 1: %+v", res.Failures, res.Results)
	}
	if res.Results[0].Outcome != handlers.StemcellRefsOutcomeFailed || !strings.Contains(res.Results[0].Error, "linked-clone") {
		t.Errorf("102 result = %+v", res.Results[0])
	}
	if !slices.Equal(f.deleted, []int64{103}) {
		t.Errorf("deleted = %v; the apply must continue past 102 and free 103", f.deleted)
	}
	if !strings.Contains(f.cfgString(102, "tags"), "bosh-destroy-pending") {
		t.Error("102 should keep the pending stamp so a rerun resumes the destroy")
	}

	// A rerun plans a resume of the pending destroy.
	_, rerun := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, rerun, 102); a.Kind != handlers.StemcellRefsActionResumeDestroy {
		t.Errorf("rerun action = %+v; want resume-destroy", a)
	}
}

func TestStemcellRefsPrune_ChangedSincePlanIsSkipped(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	// A director registers between the plan and the apply.
	f.setCfg(102, "description", provDesc(shaB, oldRef, newRef))
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged {
		t.Fatalf("result = %+v", res.Results)
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("changed template was touched: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPrune_VMIDLegacyTemplate(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(103, "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364", "", "")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 103})
	if plan.Refused || len(plan.Actions) != 1 || plan.Actions[0].Kind != handlers.StemcellRefsActionDestroy {
		t.Fatalf("plan = %+v", plan)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{103}) {
		t.Fatalf("deleted = %v", f.deleted)
	}
	if !strings.Contains(f.tagsAtDelete[103], "bosh-destroy-pending") {
		t.Errorf("legacy template destroyed without the pending stamp; tags %q", f.tagsAtDelete[103])
	}
}

func TestStemcellRefsPrune_VMIDRefusals(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(104, "bosh-stemcell-legacy-1-2", "", "")
	f.addVM(201, "clone", "", map[string]string{"scsi0": "local-lvm:base-104-disk-0/vm-201-disk-0"})
	f.addTemplate(9000, "ubuntu-noble-base", "", "")
	f.addTemplate(105, "bosh-stemcell-x-1-2", "not json", "")

	cases := map[int64]string{
		101:  "still referenced",
		104:  "linked clone",
		9000: "not a recognizable stemcell cache template",
		201:  "not a template",
		777:  "no guest",
		105:  "not provenance JSON",
	}
	for vmid, want := range cases {
		deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: vmid})
		if !plan.Refused || !strings.Contains(plan.RefusalReason, want) {
			t.Errorf("vmid %d: refused=%v reason %q; want %q", vmid, plan.Refused, plan.RefusalReason, want)
			continue
		}
		if _, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); !errors.Is(err, handlers.ErrStemcellRefsPruneRefused) {
			t.Errorf("vmid %d: apply err = %v", vmid, err)
		}
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("refusals changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPrune_RequestValidation(t *testing.T) {
	t.Parallel()
	for _, req := range []handlers.StemcellRefsPruneRequest{
		{},
		{DirectorRef: "   "},
		{DirectorRef: oldRef, VMID: 5},
		{VMID: -1},
	} {
		if _, err := handlers.PlanStemcellRefsPrune(handlers.StemcellRefsScan{}, req); err == nil {
			t.Errorf("request %+v accepted", req)
		}
	}
}

func TestStemcellRefsPrune_SentinelRefs(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"unknown-director", "debug-repro"} {
		f := newPruneFake()
		f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, ref), cacheTemplateTags(shaA)+";director--"+ref)
		deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: ref})
		if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy {
			t.Fatalf("%s: action = %+v", ref, a)
		}
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
		if err != nil || res.Failures != 0 || !slices.Equal(f.deleted, []int64{101}) {
			t.Fatalf("%s: err %v failures %d deleted %v", ref, err, res.Failures, f.deleted)
		}
	}
}

func TestStemcellRefsPrune_JSONShape(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mode":"director"`, `"kind":"remove-ref-and-destroy"`, `"refused":false`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("plan JSON %s lacks %s", raw, key)
		}
	}
	if strings.Contains(string(raw), "2026-08-03T00:00:00Z") {
		t.Errorf("plan JSON leaks the raw description snapshot: %s", raw)
	}
}
