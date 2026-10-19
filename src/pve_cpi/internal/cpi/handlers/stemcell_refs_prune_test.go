package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// pruneFakeGuest is one guest in pruneFake's cluster.
type pruneFakeGuest struct {
	node     string
	name     string
	template bool
	cfg      map[string]any
	// gen is the config generation. A config read reports it as the digest,
	// a write that carries a different digest is refused as stale, and each
	// accepted write bumps it.
	gen int
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
	// pools is the lock store every deps() client shares, so a write can
	// check whether its guest's per-VMID lock is held.
	pools *noopPoolService
	// unlockedWrites records each UpdateQemuConfig made while the guest's
	// per-VMID lock was not held.
	unlockedWrites []string
	// onWrite, when set, runs after each UpdateQemuConfig with f.mu held, so
	// a test can change a guest the way a concurrent caller would.
	onWrite func(vmid int64, g *pruneFakeGuest)
	// onRead, when set, runs before each config read with f.mu held, so a
	// test can change a guest between a plan and the apply's locked read.
	onRead func(vmid int64, g *pruneFakeGuest)
	// staleWrites makes the next N digest-guarded writes to a guest fail as
	// stale, the way a writer that slipped in after the read would.
	staleWrites map[int64]int
	// noDigest leaves the digest out of a guest's config reads.
	noDigest map[int64]bool
	// running marks guests the listing and a status read report as running.
	running map[int64]bool
	// statusOverride, when set for a guest, is the status a status read
	// reports for it, whatever the listing says.
	statusOverride map[int64]string
	// sizes holds the size a storage content read reports for each volid,
	// and sizeErr the error it returns instead.
	sizes   map[string]int64
	sizeErr map[string]error
	// checkNode makes a config read on a node other than the guest's fail
	// the way PVE does, with the pmxcfs "does not exist" answer.
	checkNode bool
	// blockRead makes a config read of a guest wait for its context to end.
	blockRead map[int64]bool
	// listErr, when set, fails the guest listing.
	listErr error
}

func newPruneFake() *pruneFake {
	return &pruneFake{
		guests:         map[int64]*pruneFakeGuest{},
		configErr:      map[int64]error{},
		deleteErr:      map[int64]error{},
		tagsAtDelete:   map[int64]string{},
		pools:          &noopPoolService{},
		staleWrites:    map[int64]int{},
		noDigest:       map[int64]bool{},
		running:        map[int64]bool{},
		statusOverride: map[int64]string{},
		sizes:          map[string]int64{},
		sizeErr:        map[string]error{},
		blockRead:      map[int64]bool{},
	}
}

func (f *pruneFake) addTemplate(vmid int64, name, desc, tags string) {
	f.guests[vmid] = &pruneFakeGuest{node: vmNode, name: name, template: true,
		cfg: map[string]any{"description": desc, "tags": tags, "name": name, "template": float64(1)}}
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

// readConfig serves a guest config read. It runs onRead first, so a test can
// change the guest between the scan and the write, and it reports the
// guest's generation as the config digest unless noDigest says otherwise.
func (f *pruneFake) readConfig(ctx context.Context, node string, vmid int64) (map[string]any, error) {
	f.mu.Lock()
	blocked := f.blockRead[vmid]
	f.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.configErr[vmid]; err != nil {
		return nil, err
	}
	g, ok := f.guests[vmid]
	if ok && f.onRead != nil {
		f.onRead(vmid, g)
		g, ok = f.guests[vmid]
	}
	if !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	if f.checkNode && g.node != node {
		return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "Configuration file 'nodes/" + node +
			"/qemu-server/" + strconv.FormatInt(vmid, 10) + ".conf' does not exist"}
	}
	out := map[string]any{}
	for k, v := range g.cfg {
		out[k] = v
	}
	if !f.noDigest[vmid] {
		out["digest"] = "gen-" + strconv.Itoa(g.gen)
	}
	return out, nil
}

// updateConfig serves a guest config write. It records writes made without
// the per-VMID lock, refuses a stale digest the way PVE does, and bumps the
// guest's generation on every accepted write.
func (f *pruneFake) updateConfig(vmidStr string, p *sdknodes.UpdateQemuConfigParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := strconv.ParseInt(vmidStr, 10, 64)
	g, ok := f.guests[id]
	if !ok {
		return &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	if _, held, _ := f.pools.get("bosh-lock-vm-" + vmidStr); !held {
		f.unlockedWrites = append(f.unlockedWrites, vmidStr)
	}
	if p.Digest != nil {
		if f.staleWrites[id] > 0 {
			f.staleWrites[id]--
			g.gen++
			return pve.ErrConfigDigestStale
		}
		if *p.Digest != "gen-"+strconv.Itoa(g.gen) {
			return pve.ErrConfigDigestStale
		}
	}
	g.gen++
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
	if f.onWrite != nil {
		f.onWrite(id, g)
	}
	return nil
}

func (f *pruneFake) deps() handlers.Deps {
	qemu := &stemcellMockQEMU{configFn: func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		return f.readConfig(ctx, node, int64(vmid))
	}}
	nodes := &stemcellMockNodes{
		updateConfigFn: func(_ context.Context, _, vmidStr string, p *sdknodes.UpdateQemuConfigParams) error {
			return f.updateConfig(vmidStr, p)
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
		if f.listErr != nil {
			return nil, f.listErr
		}
		out := make(sdkcluster.ListResourcesResponse, 0, len(f.guests))
		for vmid, g := range f.guests {
			tmpl := 0
			if g.template {
				tmpl = 1
			}
			tags, _ := g.cfg["tags"].(string)
			status := "stopped"
			if f.running[vmid] {
				status = "running"
			}
			raw, _ := json.Marshal(map[string]any{
				"type": "qemu", "vmid": vmid, "node": g.node, "name": g.name, "tags": tags, "template": tmpl,
				"status": status,
			})
			out = append(out, raw)
		}
		return &out, nil
	}}
	deps := buildDeleteStemcellDeps(deleteStemcellDepsOpts{qemuSvc: qemu, nodesSvc: nodes, clusterSvc: cluster})
	mc := deps.PVE.(*stemcellMockClient)
	mc.poolsSvc = f.pools
	mc.qemuSvc = &pruneFakeQEMU{stemcellMockQEMU: qemu, f: f}
	mc.nodesSvc = &pruneFakeNodes{stemcellMockNodes: nodes, f: f}
	return deps
}

// pruneFakeQEMU adds the status read to the stemcell QEMU mock.
type pruneFakeQEMU struct {
	*stemcellMockQEMU
	f *pruneFake
}

func (q *pruneFakeQEMU) Status(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	id := int64(vmid)
	if _, ok := q.f.guests[id]; !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	if s, ok := q.f.statusOverride[id]; ok {
		return map[string]any{"status": s}, nil
	}
	if q.f.running[id] {
		return map[string]any{"status": "running"}, nil
	}
	return map[string]any{"status": "stopped"}, nil
}

// pruneFakeNodes adds the storage content read to the stemcell nodes mock.
type pruneFakeNodes struct {
	*stemcellMockNodes
	f *pruneFake
}

func (n *pruneFakeNodes) GetStorageContent(
	_ context.Context, _, storage, volume string,
) (*sdknodes.GetStorageContentResponse, error) {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	volid := storage + ":" + volume
	if err := n.f.sizeErr[volid]; err != nil {
		return nil, err
	}
	size, ok := n.f.sizes[volid]
	if !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "volume not found"}
	}
	return &sdknodes.GetStorageContentResponse{Size: sdkclient.PVEInt(size)}, nil
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
	if n := refs[newRef].OwnedVMCount; n == nil || *n != 1 {
		t.Errorf("new ref owned VMs = %v, want 1", n)
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
	// 102's stale tag is its only generation marker, so the prune drops it
	// and adds bosh-stemcell-cache in the same write.
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, newRef), "bosh-stemcell-sha-"+shaB+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionRemoveTag || a.DropTag != "director--"+oldRef ||
		a.AddsCacheMarker {
		t.Fatalf("101 action = %+v", a)
	}
	// Changed by design: 102 used to be a non-failing skip that kept the
	// stale tag as its only marker. Tags now name current holders, so the
	// stale tag goes and bosh-stemcell-cache takes over as the marker.
	if b := actionFor(t, plan, 102); b.Kind != handlers.StemcellRefsActionRemoveTag || !b.AddsCacheMarker || b.Failure {
		t.Fatalf("102 action = %+v; want a remove-tag that adds the cache marker", b)
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
	// Changed by design, as above: 102 keeps a generation marker through
	// bosh-stemcell-cache rather than through the stale director tag.
	if tags := f.cfgString(102, "tags"); strings.Contains(tags, "director--"+oldRef) ||
		!strings.Contains(tags, "bosh-stemcell-cache") {
		t.Errorf("102 tags = %q; want the stale tag gone and bosh-stemcell-cache added", tags)
	}
	if got := descRefs(t, f.cfgString(102, "description")); !slices.Equal(got, []string{newRef}) {
		t.Errorf("102 description refs changed to %v", got)
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

// replicaTags is a per-node replica's tag set for sha8 on node2.
func replicaTags(sha8 string) string {
	return cacheTemplateTags(sha8) + ";bosh-stemcell-node-pve-node2"
}

// addReplicaFixture is the cluster the director-prune replica tests share:
// anchor 101 and four replicas of shaA on pve-node2. 150 holds only oldRef,
// 151 holds no ref, 152 holds oldRef and newRef, and 153 holds only oldRef
// but backs linked clone 200.
func addReplicaFixture(f *pruneFake) {
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), replicaTags(shaA)+";director--"+oldRef)
	f.addTemplate(151, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA))
	f.addTemplate(152, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		replicaTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(153, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), replicaTags(shaA)+";director--"+oldRef)
	for _, id := range []int64{150, 151, 152, 153} {
		f.guests[id].node = "pve-node2"
	}
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-153-disk-0/vm-200-disk-0"})
}

func TestStemcellRefsPrune_DirectorRemovesRefFromReplicas(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	addReplicaFixture(f)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	// The dry run lists the anchor destroy with its sweep, and one action
	// for each replica that holds the ref, after the anchor.
	if len(plan.Actions) != 4 || plan.Actions[0].VMID != 101 || plan.HasFailures() {
		t.Fatalf("plan = %+v; want 101 first, then one action per replica that holds the ref", plan.Actions)
	}
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy || len(a.SweepReplicas) != 1 ||
		a.SweepReplicas[0].VMID != 151 || len(a.KeptReplicas) != 0 {
		t.Fatalf("101 action = %+v; want a destroy that sweeps 151 and leaves the rest to their own actions", a)
	}
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy || !r.Replica {
		t.Fatalf("150 action = %+v; want the replica freed with its only ref", r)
	}
	r152 := actionFor(t, plan, 152)
	if r152.Kind != handlers.StemcellRefsActionRemoveRef || !slices.Equal(r152.RemainingRefs, []string{newRef}) ||
		r152.DropTag != "director--"+oldRef {
		t.Fatalf("152 action = %+v; want the ref and tag removed and newRef kept", r152)
	}
	r153 := actionFor(t, plan, 153)
	if r153.Kind != handlers.StemcellRefsActionRemoveRef || r153.DropTag != "director--"+oldRef ||
		!strings.Contains(r153.Reason, "linked clone") || r153.Failure {
		t.Fatalf("153 action = %+v; want the ref and tag removed and the replica kept for its clone", r153)
	}

	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{101, 151, 150}) {
		t.Fatalf("deleted = %v, want [101 151 150]", f.deleted)
	}
	if got := descRefs(t, f.cfgString(152, "description")); !slices.Equal(got, []string{newRef}) {
		t.Errorf("replica 152 refs = %v, want [%s]", got, newRef)
	}
	if tags := f.cfgString(152, "tags"); strings.Contains(tags, "director--"+oldRef) || !strings.Contains(tags, "director--"+newRef) {
		t.Errorf("replica 152 tags = %q", tags)
	}
	if got := descRefs(t, f.cfgString(153, "description")); len(got) != 0 {
		t.Errorf("replica 153 refs = %v, want none", got)
	}
	if tags := f.cfgString(153, "tags"); strings.Contains(tags, "director--"+oldRef) || strings.Contains(tags, "bosh-destroy-pending") {
		t.Errorf("replica 153 tags = %q; want the director tag gone and no pending stamp", tags)
	}
	for _, id := range []int64{150, 151} {
		if !strings.Contains(f.tagsAtDelete[id], "bosh-destroy-pending") {
			t.Errorf("replica %d destroyed without the pending stamp; tags %q", id, f.tagsAtDelete[id])
		}
	}
	if len(f.unlockedWrites) != 0 {
		t.Errorf("writes outside the lock: %v", f.unlockedWrites)
	}
}

func TestStemcellRefsPrune_ReplicaKeptWhenAnchorDestroyFails(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), replicaTags(shaA)+";director--"+oldRef)
	f.deleteErr[101] = errors.New("boom")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy {
		t.Fatalf("150 action = %+v", r)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 1 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted = %v; want nothing while anchor 101 survives", f.deleted)
	}
	if got := descRefs(t, f.cfgString(150, "description")); len(got) != 0 {
		t.Errorf("replica 150 refs = %v; want the ref removed even though the replica stays", got)
	}
	r150 := res.Results[1]
	if r150.Action.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy ||
		r150.AppliedKind != handlers.StemcellRefsActionRemoveRef || r150.Outcome != handlers.StemcellRefsOutcomeApplied ||
		!strings.Contains(r150.KeptReason, "replica 150 not destroyed") || len(r150.Destroyed) != 0 {
		t.Errorf("150 result = %+v; want the destroy recorded as a remove-ref with the kept reason", r150)
	}
	raw, err := json.Marshal(r150)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"applied_kind":"remove-ref"`) || !strings.Contains(string(raw), `"kept_reason":"replica 150`) {
		t.Errorf("150 result JSON = %s; want applied_kind and kept_reason", raw)
	}
}

func TestStemcellRefsPrune_VMIDSweepsOnlyEligibleReplicas(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), cacheTemplateTags(shaA))
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), replicaTags(shaA))
	f.addTemplate(151, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA))
	f.addTemplate(152, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA))
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-152-disk-0/vm-200-disk-0"})

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 101})
	if plan.Refused || len(plan.Actions) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	a := plan.Actions[0]
	if a.Kind != handlers.StemcellRefsActionDestroy || len(a.SweepReplicas) != 1 || a.SweepReplicas[0].VMID != 151 {
		t.Fatalf("action = %+v; want destroy of 101 that sweeps only 151", a)
	}
	kept := map[int64]string{}
	for _, k := range a.KeptReplicas {
		kept[k.VMID] = k.Reason
	}
	if !strings.Contains(kept[150], "holds refs") || !strings.Contains(kept[152], "linked clone") || len(kept) != 2 {
		t.Fatalf("kept replicas = %+v; want 150 for its refs and 152 for its clone", a.KeptReplicas)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{101, 151}) || !slices.Equal(res.Results[0].Destroyed, []int64{101, 151}) {
		t.Fatalf("deleted = %v destroyed = %v, want [101 151]", f.deleted, res.Results[0].Destroyed)
	}
}

func TestStemcellRefsPrune_VMIDReplicaRules(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	// No anchor of shaB survives, but 160 holds a ref of its own.
	f.addTemplate(160, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), replicaTags(shaB))
	// 161 has no sha8 anywhere, so its anchor cannot be proven gone.
	f.addTemplate(161, "bosh-stemcell-c-1", `{"name":"c","version":"1","director_refs":[]}`,
		"bosh-stemcell-cache;bosh-stemcell-node-pve-node2")
	// 162 holds no ref and has no live anchor, so --vmid frees it.
	f.addTemplate(162, "bosh-stemcell-d-1-dddd4444", provDesc("dddd4444"), replicaTags("dddd4444"))

	cases := map[int64]string{160: "holds refs of its own", 161: "no known sha8"}
	for vmid, want := range cases {
		_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: vmid})
		if !plan.Refused || !strings.Contains(plan.RefusalReason, want) {
			t.Errorf("vmid %d: refused=%v reason %q; want %q", vmid, plan.Refused, plan.RefusalReason, want)
		}
	}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 162})
	if plan.Refused {
		t.Fatalf("zero-ref replica without a live anchor refused: %s", plan.RefusalReason)
	}
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{162}) {
		t.Fatalf("deleted = %v, want [162]", f.deleted)
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
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionRemoveRef ||
		!strings.Contains(r.Reason, "replica of template 101, which this prune keeps") {
		t.Fatalf("150 action = %+v; want its ref removed and the replica kept with its live anchor", r)
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
		105:  "does not parse as provenance JSON",
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
		deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: ref, AllowOwnedVMs: true})
		if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy {
			t.Fatalf("%s: action = %+v", ref, a)
		}
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
		if err != nil || res.Failures != 0 || !slices.Equal(f.deleted, []int64{101}) {
			t.Fatalf("%s: err %v failures %d deleted %v", ref, err, res.Failures, f.deleted)
		}
	}
}

func TestStemcellRefsPrune_SentinelOwnershipUnknown(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, "unknown-director"),
		cacheTemplateTags(shaA)+";director--unknown-director")
	// A UUID-less caller's VM carries no owner tag at all.
	f.addVM(300, "uuidless-vm", "", nil)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: "unknown-director"})
	if !plan.Refused || !plan.OwnershipUnknown || !strings.Contains(plan.RefusalReason, "ownership cannot be determined") {
		t.Fatalf("plan = %+v; want a refusal saying ownership cannot be determined", plan)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if !errors.Is(err, handlers.ErrStemcellRefsPruneRefused) || !res.Refused {
		t.Fatalf("apply err = %v refused = %v", err, res.Refused)
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("refused sentinel prune changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range handlers.BuildStemcellRefsReport(scan).Refs {
		if r.Ref == "unknown-director" && (r.OwnedVMCount != nil || !r.OwnershipUnknown) {
			t.Errorf("report owned count = %v unknown = %v; want unknown, not 0", r.OwnedVMCount, r.OwnershipUnknown)
		}
	}
}

func TestStemcellRefsPrune_BlankDescriptionRefsUnknown(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	// A director registered on 101, then its description was blanked.
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, "", cacheTemplateTags(shaA)+";director--"+oldRef)
	// 102 has only the cache marker and a blank description.
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, "", "bosh-stemcell-cache")
	// 103 is a true legacy template.
	f.addTemplate(103, "bosh-stemcell-legacy-1-2", "", "")

	for _, vmid := range []int64{101, 102} {
		_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: vmid})
		if !plan.Refused || !strings.Contains(plan.RefusalReason, "refs are unknown") {
			t.Errorf("vmid %d: refused=%v reason %q; want refs unknown", vmid, plan.Refused, plan.RefusalReason)
		}
	}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, plan, 101); a.Kind != handlers.StemcellRefsActionSkip || !a.Failure ||
		!strings.Contains(a.Reason, "refs are unknown") {
		t.Fatalf("director mode on 101 = %+v; want a failing skip, not remove-tag", a)
	}
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 1 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if f.writeCount() != 0 || len(f.deleted) != 0 {
		t.Fatalf("blank-description templates were touched: writes %v deleted %v", f.writes, f.deleted)
	}

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	unref := map[int64]handlers.StemcellRefsUnreferenced{}
	for _, u := range handlers.BuildStemcellRefsReport(scan).Unreferenced {
		unref[u.VMID] = u
	}
	for _, vmid := range []int64{101, 102} {
		if u, ok := unref[vmid]; !ok || u.Freeable || !u.RefsUnknown {
			t.Errorf("report row for %d = %+v (present %v); want refs unknown, not freeable", vmid, u, ok)
		}
	}
	if u := unref[103]; !u.Freeable || !u.Legacy || u.RefsUnknown {
		t.Errorf("report row for legacy 103 = %+v; want freeable", u)
	}
}

func TestStemcellRefsPrune_WritesHappenInsideTheLock(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(104, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", newRef),
		cacheTemplateTags("cccc3333")+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 0 {
		t.Fatalf("director apply: err %v %+v", err, res.Results)
	}
	deps, plan = scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 0 {
		t.Fatalf("vmid apply: err %v %+v", err, res.Results)
	}
	// 101 gets a ref and tag write, 102 a pending stamp, 104 a tag write,
	// and 105 a pending stamp.
	if f.writeCount() < 4 {
		t.Fatalf("writes = %v; expected one per template", f.writes)
	}
	if len(f.unlockedWrites) != 0 {
		t.Fatalf("writes made without the per-VMID lock: %v", f.unlockedWrites)
	}
}

func TestStemcellRefsPrune_SanitizedRefs(t *testing.T) {
	t.Parallel()
	const odd = "Old.Dir_1"
	const oddTag = "director--Old-Dir-1"
	f := newPruneFake()
	// 101 holds odd and newRef; its odd tag goes.
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, odd, newRef),
		cacheTemplateTags(shaA)+";"+oddTag+";director--"+newRef)
	// 102 holds odd and a second ref that sanitizes to the same tag, which
	// therefore stays.
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, odd, "Old_Dir.1"), cacheTemplateTags(shaB)+";"+oddTag)
	f.addVM(300, "live-vm", "bosh-owner--old-dir-1", nil)

	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: odd})
	if !plan.Refused || len(plan.OwnedVMs) != 1 || plan.OwnerTag != "bosh-owner--old-dir-1" {
		t.Fatalf("plan = %+v; want the owner guard to find VM 300 through the sanitized tag", plan)
	}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: odd, AllowOwnedVMs: true})
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRef || a.DropTag != oddTag {
		t.Fatalf("101 action = %+v; want remove-ref dropping %s", a, oddTag)
	}
	b := actionFor(t, plan, 102)
	if b.Kind != handlers.StemcellRefsActionRemoveRef || b.DropTag != "" || !strings.Contains(b.Reason, "kept") {
		t.Fatalf("102 action = %+v; want remove-ref that keeps the shared tag", b)
	}
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if tags := f.cfgString(101, "tags"); strings.Contains(tags, oddTag) {
		t.Errorf("101 tags = %q; %s should be gone", tags, oddTag)
	}
	if got := descRefs(t, f.cfgString(102, "description")); !slices.Equal(got, []string{"Old_Dir.1"}) {
		t.Errorf("102 refs = %v", got)
	}
	if tags := f.cfgString(102, "tags"); !strings.Contains(tags, oddTag) {
		t.Errorf("102 tags = %q; the shared tag must stay", tags)
	}
}

func TestStemcellRefsPrune_TrimmedRefMatch(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, " "+oldRef+" ", newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRef || a.MatchedRef != " "+oldRef+" " {
		t.Fatalf("action = %+v; want remove-ref of the stored spelling", a)
	}
	if res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan); err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if got := descRefs(t, f.cfgString(101, "description")); !slices.Equal(got, []string{newRef}) {
		t.Errorf("refs = %v, want [%s]", got, newRef)
	}
}

func TestStemcellRefsPrune_ResumeDestroyApplies(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef),
		cacheTemplateTags(shaB)+";director--"+oldRef+";bosh-destroy-pending")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if a := actionFor(t, plan, 102); a.Kind != handlers.StemcellRefsActionResumeDestroy {
		t.Fatalf("action = %+v; want resume-destroy", a)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v %+v", err, res.Results)
	}
	if res.Results[0].Outcome != handlers.StemcellRefsOutcomeApplied || !slices.Equal(res.Results[0].Destroyed, []int64{102}) {
		t.Fatalf("result = %+v", res.Results[0])
	}
	if !slices.Equal(f.deleted, []int64{102}) {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestStemcellRefsPrune_UnfinishedSha8KeepsReplicas(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(102, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA))
	f.deleteErr[101] = &sdkerrors.APIError{HTTPCode: 400, Message: "storage busy"}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if b := actionFor(t, plan, 102); len(b.SweepReplicas) != 1 || b.SweepReplicas[0].VMID != 150 {
		t.Fatalf("102 action = %+v; want it to carry the sweep of 150", b)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !slices.Equal(f.deleted, []int64{102}) {
		t.Fatalf("deleted = %v; want 102 only, with replica 150 kept while 101 survives", f.deleted)
	}
	notes := strings.Join(res.Results[1].Notes, " ")
	if !strings.Contains(notes, "replicas kept") {
		t.Errorf("102 notes = %q; want the kept replicas reported", notes)
	}
}

func TestStemcellRefsPrune_ConfigMissingCountsAsGone(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-b-1-"+shaB, provDesc(shaB), replicaTags(shaB))
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")

	missing := &sdkerrors.APIError{HTTPCode: 500, Message: "Configuration file 'nodes/pve/qemu-server/1.conf' does not exist"}
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	vdeps, vplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	f.configErr[101] = missing
	f.configErr[102] = missing
	f.configErr[105] = missing

	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("director apply: err %v %+v", err, res.Results)
	}
	for _, r := range res.Results {
		if r.Outcome != handlers.StemcellRefsOutcomeGone {
			t.Errorf("vmid %d outcome %s; want already-gone", r.Action.VMID, r.Outcome)
		}
	}
	// 102 carried the sweep of 150; the already-gone carrier says so.
	if notes := strings.Join(res.Results[1].Notes, " "); !strings.Contains(notes, "replicas 150 kept") {
		t.Errorf("102 notes = %q; want replica 150 reported as kept", notes)
	}
	vres, err := handlers.ApplyStemcellRefsPrune(context.Background(), vdeps, vplan)
	if err != nil || vres.Failures != 0 || vres.Results[0].Outcome != handlers.StemcellRefsOutcomeGone {
		t.Fatalf("vmid apply: err %v %+v", err, vres.Results)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("gone templates were touched: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPrune_VerifyChecksTemplateAndName(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")
	f.addTemplate(106, "bosh-stemcell-legacy-1-3", "", "")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	_, plan2 := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 106})
	f.addTemplate(107, "bosh-stemcell-legacy-1-4", "", "")
	_, plan3 := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 107})
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef, newRef),
		cacheTemplateTags(shaB)+";director--"+oldRef+";director--"+newRef)
	_, dplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	// 105 became an ordinary VM, 106 was renamed, 107 lost its name, and 102
	// was renamed, all after the plan.
	f.guests[105].cfg["template"] = float64(0)
	f.setCfg(106, "name", "someone-else")
	f.mu.Lock()
	delete(f.guests[107].cfg, "name")
	f.mu.Unlock()
	f.setCfg(102, "name", "renamed")

	for _, p := range []handlers.StemcellRefsPlan{plan, plan2, plan3, dplan} {
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, p)
		if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged {
			t.Errorf("%s %d: err %v %+v; want changed-since-plan", p.Mode, p.VMID, err, res.Results)
		}
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("changed guests were touched: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsPrune_UnplannedActionIsNotApplied(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded handlers.StemcellRefsPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, decoded)
	if err != nil || res.Failures != 1 || !strings.Contains(res.Results[0].Error, "planner") {
		t.Fatalf("decoded plan apply: err %v %+v; want the action rejected", err, res.Results)
	}
	if len(f.deleted) != 0 || f.writeCount() != 0 {
		t.Fatalf("decoded plan changed PVE: writes %v deleted %v", f.writes, f.deleted)
	}
}

func TestStemcellRefsReport_FlagsBlankRefs(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, "", newRef), cacheTemplateTags(shaA)+";director--"+newRef)
	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	var blank *handlers.StemcellRefsRefSummary
	rep := handlers.BuildStemcellRefsReport(scan)
	for i := range rep.Refs {
		if rep.Refs[i].Ref == "" {
			blank = &rep.Refs[i]
		}
	}
	if blank == nil || blank.Note == "" || !slices.Equal(blank.DescriptionTemplates, []int64{101}) {
		t.Fatalf("refs = %+v; want a flagged blank-ref row naming 101", rep.Refs)
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

func TestStemcellRefsPrune_VerifyAcceptsNumericTemplateFlag(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")
	// A client that decodes with UseNumber hands the flag over as json.Number.
	f.guests[105].cfg["template"] = json.Number("1")
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 || !slices.Equal(f.deleted, []int64{105}) {
		t.Fatalf("apply: err %v %+v deleted %v; want 105 destroyed", err, res.Results, f.deleted)
	}
}

// untrackedTags carry sha8 and node tags but no generation marker.
func untrackedTags(sha8 string, replica bool) string {
	tags := "bosh-stemcell;bosh-stemcell-sha-" + sha8
	if replica {
		tags += ";bosh-stemcell-node-pve-node2"
	}
	return tags
}

func TestStemcellRefsPrune_UntrackedIsNeverDestroyed(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	// 101 is the anchor of shaA and holds only oldRef.
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	// 155 is an untracked zero-ref replica of shaA, and 156 an untracked
	// replica of shaA that holds only oldRef.
	f.addTemplate(155, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), untrackedTags(shaA, true))
	f.addTemplate(156, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), untrackedTags(shaA, true))
	// 110 is an untracked zero-ref main template.
	f.addTemplate(110, "bosh-stemcell-u-1-abcd1111", provDesc("abcd1111"), untrackedTags("abcd1111", false))
	// 120 is an untracked main template that holds only oldRef, and 121 one
	// whose last-ref destroy is already pending.
	f.addTemplate(120, "bosh-stemcell-c-1-cccc3333", provDesc("cccc3333", oldRef), untrackedTags("cccc3333", false))
	f.addTemplate(121, "bosh-stemcell-d-1-dddd4444", provDesc("dddd4444", oldRef),
		untrackedTags("dddd4444", false)+";bosh-destroy-pending")
	// 130 is legacy: a blank description with sha8 and node tags but no
	// director-- or cache tag. It stays freeable.
	f.addTemplate(130, "bosh-stemcell-l-1-2", "", "bosh-stemcell-sha-llll1111;bosh-stemcell-node-pve-node2")

	for _, vmid := range []int64{110, 155} {
		_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: vmid})
		if !plan.Refused || !strings.Contains(plan.RefusalReason, "untracked") {
			t.Errorf("--vmid %d: refused=%v reason %q; want an untracked refusal", vmid, plan.Refused, plan.RefusalReason)
		}
	}
	if _, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 130}); plan.Refused {
		t.Errorf("--vmid 130: legacy template with sha8 and node tags refused: %s", plan.RefusalReason)
	}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	a := actionFor(t, plan, 101)
	if a.Kind != handlers.StemcellRefsActionRemoveRefAndDestroy || len(a.SweepReplicas) != 0 {
		t.Fatalf("101 action = %+v; want a destroy that sweeps nothing", a)
	}
	var kept155 bool
	for _, k := range a.KeptReplicas {
		if k.VMID == 155 && strings.Contains(k.Reason, "untracked") && !k.FreeableByVMID {
			kept155 = true
		}
	}
	if !kept155 {
		t.Errorf("101 kept replicas = %+v; want 155 kept as untracked", a.KeptReplicas)
	}
	if r := actionFor(t, plan, 156); r.Kind != handlers.StemcellRefsActionRemoveRef || !strings.Contains(r.Reason, "untracked") {
		t.Errorf("156 action = %+v; want the ref removed and the replica kept as untracked", r)
	}
	for _, vmid := range []int64{120, 121} {
		if b := actionFor(t, plan, vmid); b.Kind != handlers.StemcellRefsActionBlocked || !b.Failure ||
			!strings.Contains(b.Reason, "untracked") {
			t.Errorf("%d action = %+v; want blocked as untracked", vmid, b)
		}
	}

	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 2 {
		t.Fatalf("apply: err %v failures %d %+v; want the two blocked templates as failures", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{101}) {
		t.Fatalf("deleted = %v; want only anchor 101", f.deleted)
	}
	if got := descRefs(t, f.cfgString(120, "description")); !slices.Equal(got, []string{oldRef}) {
		t.Errorf("120 refs = %v; a blocked template keeps its ref", got)
	}

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	unref := map[int64]handlers.StemcellRefsUnreferenced{}
	for _, u := range handlers.BuildStemcellRefsReport(scan).Unreferenced {
		unref[u.VMID] = u
	}
	// No prune but --untracked frees an untracked template, so the report
	// names that command for each one that holds no ref, and the reason for
	// each one it refuses.
	for _, vmid := range []int64{110, 155, 156} {
		want := fmt.Sprintf("cpi stemcell-refs prune --config <cpi.json> --vmid %d --untracked", vmid)
		if u, ok := unref[vmid]; !ok || !u.Untracked || !u.Freeable || u.FreeCommand != want {
			t.Errorf("report row for %d = %+v (present %v); want untracked and freeable with %q", vmid, u, ok, want)
		}
	}
	for _, vmid := range []int64{120, 121} {
		if u, ok := unref[vmid]; !ok || u.Freeable || u.FreeCommand != "" || !strings.Contains(u.Reason, "holds director_refs") {
			t.Errorf("report row for %d = %+v (present %v); want refused for its ref", vmid, u, ok)
		}
	}
}

func TestStemcellRefsPrune_KeptTemplateGainsCacheMarker(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	// Replica 157's director tag is its only generation marker, and it
	// backs a linked clone, so the prune keeps it with no ref.
	f.addTemplate(157, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef),
		untrackedTags(shaA, true)+";director--"+oldRef)
	f.addVM(200, "clone", "", map[string]string{"scsi0": "local-lvm:base-157-disk-0/vm-200-disk-0"})
	// 160 holds no ref, and its stale director tag is its only marker.
	f.addTemplate(160, "bosh-stemcell-b-1-"+shaB, provDesc(shaB), untrackedTags(shaB, false)+";director--"+oldRef)

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if r := actionFor(t, plan, 157); r.Kind != handlers.StemcellRefsActionRemoveRef || !r.AddsCacheMarker {
		t.Fatalf("157 action = %+v; want a kept remove-ref that adds the cache marker", r)
	}
	if r := actionFor(t, plan, 160); r.Kind != handlers.StemcellRefsActionRemoveTag || !r.AddsCacheMarker {
		t.Fatalf("160 action = %+v; want a remove-tag that adds the cache marker", r)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	for _, r := range res.Results {
		if r.Action.VMID != 101 && (!r.TagRemoved || !r.CacheMarkerAdded) {
			t.Errorf("%d result = %+v; want the tag removed and the marker added", r.Action.VMID, r)
		}
	}
	for _, vmid := range []int64{157, 160} {
		tags := f.cfgString(vmid, "tags")
		if strings.Contains(tags, "director--") || !strings.Contains(tags, "bosh-stemcell-cache") {
			t.Errorf("%d tags = %q; want the director tag gone and bosh-stemcell-cache added", vmid, tags)
		}
	}
	if len(f.unlockedWrites) != 0 {
		t.Errorf("writes outside the lock: %v", f.unlockedWrites)
	}
	// 160 is still a tracked zero-ref template, so --vmid can free it.
	if _, vplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 160}); vplan.Refused {
		t.Errorf("--vmid 160 refused after the prune: %s", vplan.RefusalReason)
	}
}

func TestStemcellRefsPrune_DowngradedReplicaGainsCacheMarker(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), untrackedTags(shaA, true)+";director--"+oldRef)
	f.deleteErr[101] = errors.New("boom")

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if r := res.Results[1]; !r.CacheMarkerAdded || r.AppliedKind != handlers.StemcellRefsActionRemoveRef {
		t.Errorf("150 result = %+v; want a remove-ref that added the cache marker", r)
	}
	if tags := f.cfgString(150, "tags"); strings.Contains(tags, "director--") || !strings.Contains(tags, "bosh-stemcell-cache") {
		t.Errorf("150 tags = %q; want the cache marker in place of the director tag", tags)
	}
}

func TestStemcellRefsPrune_ZeroRefReplicaWithOwnActionIsListed(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	// 157 holds no ref but carries a stale director tag, and 158 also has
	// its destroy pending.
	f.addTemplate(157, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA)+";director--"+oldRef)
	f.addTemplate(158, "bosh-stemcell-a-1-"+shaA, provDesc(shaA),
		replicaTags(shaA)+";director--"+oldRef+";bosh-destroy-pending")
	for _, id := range []int64{157, 158} {
		f.guests[id].node = "pve-node2"
	}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if r := actionFor(t, plan, 157); r.Kind != handlers.StemcellRefsActionRemoveTag {
		t.Fatalf("157 action = %+v; want remove-tag", r)
	}
	if r := actionFor(t, plan, 158); r.Kind != handlers.StemcellRefsActionSkip || r.Failure {
		t.Fatalf("158 action = %+v; want a non-failing skip", r)
	}
	a := actionFor(t, plan, 101)
	kept := map[int64]handlers.StemcellRefsKeptReplica{}
	for _, k := range a.KeptReplicas {
		kept[k.VMID] = k
	}
	for _, vmid := range []int64{157, 158} {
		k, ok := kept[vmid]
		if !ok || !k.FreeableByVMID || !strings.Contains(k.Reason, "--vmid "+strconv.FormatInt(vmid, 10)) {
			t.Errorf("kept replica %d = %+v (listed %v); want it listed as freeable by --vmid", vmid, k, ok)
		}
	}

	scan, err := handlers.ScanStemcellRefs(context.Background(), f.deps())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range handlers.BuildStemcellRefsReport(scan).Refs {
		if r.Ref != oldRef {
			continue
		}
		var ids []int64
		for _, k := range r.KeptReplicas {
			ids = append(ids, k.VMID)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []int64{157, 158}) {
			t.Errorf("report kept replicas for %s = %+v; want 157 and 158", oldRef, r.KeptReplicas)
		}
	}

	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 0 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	if !slices.Equal(f.deleted, []int64{101}) {
		t.Fatalf("deleted = %v; want only 101", f.deleted)
	}
	// With the anchor gone, --vmid frees the replica the plan listed.
	vdeps, vplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 157})
	if vplan.Refused {
		t.Fatalf("--vmid 157 refused: %s", vplan.RefusalReason)
	}
	if vres, err := handlers.ApplyStemcellRefsPrune(context.Background(), vdeps, vplan); err != nil || vres.Failures != 0 {
		t.Fatalf("--vmid apply: err %v %+v", err, vres.Results)
	}
	if !slices.Contains(f.deleted, 157) {
		t.Errorf("deleted = %v; want 157 freed by --vmid", f.deleted)
	}
}

func TestStemcellRefsPrune_PendingReplicaRules(t *testing.T) {
	t.Parallel()
	pending := replicaTags(shaA) + ";director--" + oldRef + ";bosh-destroy-pending"

	// The anchor keeps newRef, so a pending replica's resume is blocked.
	f := newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef, newRef),
		cacheTemplateTags(shaA)+";director--"+oldRef+";director--"+newRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), pending)
	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionBlocked || !r.Failure ||
		!strings.Contains(r.Reason, "replica of template 101, which this prune keeps") {
		t.Fatalf("150 action = %+v; want blocked by the surviving anchor", r)
	}

	// The anchor's destroy fails, so the planned resume is skipped.
	f = newPruneFake()
	f.addTemplate(101, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), cacheTemplateTags(shaA)+";director--"+oldRef)
	f.addTemplate(150, "bosh-stemcell-a-1-"+shaA, provDesc(shaA, oldRef), pending)
	f.deleteErr[101] = errors.New("boom")
	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	if r := actionFor(t, plan, 150); r.Kind != handlers.StemcellRefsActionResumeDestroy {
		t.Fatalf("150 action = %+v; want resume-destroy", r)
	}
	res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
	if err != nil || res.Failures != 2 {
		t.Fatalf("apply: err %v failures %d %+v", err, res.Failures, res.Results)
	}
	r := res.Results[1]
	if r.Outcome != handlers.StemcellRefsOutcomeSkipped || !r.Failure || r.AppliedKind != handlers.StemcellRefsActionSkip ||
		!strings.Contains(r.KeptReason, "replica 150 not destroyed") {
		t.Errorf("150 result = %+v; want a failing skip with the kept reason", r)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted = %v; want nothing", f.deleted)
	}
}

func TestStemcellRefsPrune_DestroyAbortsWhenPendingStampIsGone(t *testing.T) {
	t.Parallel()
	f := newPruneFake()
	f.addTemplate(102, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(105, "bosh-stemcell-legacy-1-2", "", "")
	// A director registers right after the pending stamp lands: it adds its
	// ref and removes the stamp, as registerStemcellDirectorRef does.
	f.onWrite = func(vmid int64, g *pruneFakeGuest) {
		tags, _ := g.cfg["tags"].(string)
		if !strings.Contains(tags, "bosh-destroy-pending") {
			return
		}
		var kept []string
		for tag := range strings.SplitSeq(tags, ";") {
			if tag != "bosh-destroy-pending" {
				kept = append(kept, tag)
			}
		}
		g.cfg["tags"] = strings.Join(kept, ";")
		if vmid == 102 {
			g.cfg["description"] = provDesc(shaB, oldRef, newRef)
		}
	}

	deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{DirectorRef: oldRef})
	vdeps, vplan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMID: 105})
	for _, run := range []struct {
		deps handlers.Deps
		plan handlers.StemcellRefsPlan
	}{{deps, plan}, {vdeps, vplan}} {
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), run.deps, run.plan)
		if err != nil || res.Failures != 1 || res.Results[0].Outcome != handlers.StemcellRefsOutcomeChanged ||
			!strings.Contains(res.Results[0].Error, "bosh-destroy-pending was removed") {
			t.Errorf("%s apply: err %v %+v; want changed-since-plan with no destroy", run.plan.Mode, err, res.Results)
		}
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted = %v; a re-registered template must not be destroyed", f.deleted)
	}
}

func TestStemcellRefsPrune_VMIDAnchorAndReplicaTogether(t *testing.T) {
	t.Parallel()
	for _, order := range [][]int64{{130, 131}, {131, 130}} {
		f := newPruneFake()
		f.addTemplate(130, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), cacheTemplateTags(shaA))
		f.addTemplate(131, "bosh-stemcell-a-1-"+shaA, provDesc(shaA), replicaTags(shaA))

		deps, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMIDs: order})
		if plan.Refused || plan.HasFailures() {
			t.Fatalf("order %v: plan = %+v; want no refusal and no failing action", order, plan)
		}
		anchor, replica := actionFor(t, plan, 130), actionFor(t, plan, 131)
		if anchor.Kind != handlers.StemcellRefsActionDestroy || len(anchor.SweepReplicas) != 1 ||
			anchor.SweepReplicas[0].VMID != 131 || len(anchor.KeptReplicas) != 0 {
			t.Fatalf("order %v: 130 action = %+v; want a destroy that sweeps 131", order, anchor)
		}
		if replica.Kind != handlers.StemcellRefsActionSkip || replica.Failure ||
			!strings.Contains(replica.Reason, "freed with template 130") {
			t.Fatalf("order %v: 131 action = %+v; want a non-failing skip freed with template 130", order, replica)
		}
		res, err := handlers.ApplyStemcellRefsPrune(context.Background(), deps, plan)
		if err != nil || res.Failures != 0 {
			t.Fatalf("order %v: apply: err %v %+v", order, err, res.Results)
		}
		if !slices.Equal(f.deleted, []int64{130, 131}) {
			t.Fatalf("order %v: deleted = %v; want [130 131]", order, f.deleted)
		}
	}

	// An anchor the plan keeps still blocks its replica.
	f := newPruneFake()
	f.addTemplate(132, "bosh-stemcell-b-1-"+shaB, provDesc(shaB, oldRef), cacheTemplateTags(shaB)+";director--"+oldRef)
	f.addTemplate(133, "bosh-stemcell-b-1-"+shaB, provDesc(shaB), replicaTags(shaB))
	_, plan := scanAndPlan(t, f, handlers.StemcellRefsPruneRequest{VMIDs: []int64{132, 133}})
	if r := actionFor(t, plan, 133); r.Kind != handlers.StemcellRefsActionBlocked || !r.Failure ||
		!strings.Contains(r.Reason, "replica of live template 132") {
		t.Fatalf("133 action = %+v; want blocked while its anchor 132 is kept", r)
	}
}
