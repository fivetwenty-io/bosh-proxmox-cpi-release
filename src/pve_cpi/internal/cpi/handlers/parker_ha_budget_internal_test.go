package handlers

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedParkerClient is the parker HA fake whose HA side is an
// HADeleteScene, so the teardown of a registered parker reads the CRM, takes
// the HA stop, and destroys the parker the way it does for a VM PVE HA
// manages. The parker's config, snapshots, and protection writes come from
// the phClient. A destroy with purge removes the parker's config and its HA
// resource, as PVE does.
type managedParkerClient struct {
	*phClient
	scene     *HADeleteScene
	mu        sync.Mutex
	destroyed []*sdknodes.DeleteQemuParams
}

func (c *managedParkerClient) Cluster() cluster.Service {
	stub := newNAStub()
	stub.members = c.scene.Members
	return &HASceneCluster{Service: stub, Scene: c.scene, UseIndex: true}
}

func (c *managedParkerClient) Nodes() sdknodes.Service {
	return &managedParkerNodes{phNodes: &phNodes{c: c.phClient}, c: c}
}

func (c *managedParkerClient) Tasks() tasks.Service { return haSceneTasks{} }

func (c *managedParkerClient) Pools() pve.PoolService { return c.scene.lockPoolService() }

func (c *managedParkerClient) EffectivePermissions(context.Context, string) (map[string]bool, error) {
	return c.scene.perms()
}

func (c *managedParkerClient) ActiveMoveTasks(_ context.Context, node string) ([]pve.ActiveTask, error) {
	return c.scene.activeMoveTasks(node)
}

func (c *managedParkerClient) destroys() []*sdknodes.DeleteQemuParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.destroyed)
}

type managedParkerNodes struct {
	*phNodes
	c *managedParkerClient
}

func (n *managedParkerNodes) ListTime(ctx context.Context, node string) (*sdknodes.ListTimeResponse, error) {
	return (&HASceneNodes{Scene: n.c.scene}).ListTime(ctx, node)
}

func (n *managedParkerNodes) DeleteQemu(
	_ context.Context, node, vmid string, params *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.c.mu.Lock()
	n.c.destroyed = append(n.c.destroyed, params)
	n.c.mu.Unlock()
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return nil, err
	}
	n.c.phClient.mu.Lock()
	delete(n.c.configs, id)
	n.c.phClient.mu.Unlock()
	n.c.scene.Do(func(s *HADeleteScene) {
		s.record("DeleteQemu:" + node + "/" + vmid)
		delete(s.IndexNode, id)
		if params != nil && params.Purge != nil && *params.Purge {
			delete(s.Requested, "vm:"+vmid)
			delete(s.Services, "vm:"+vmid)
			delete(s.Comments, "vm:"+vmid)
		}
	})
	upid := "UPID:" + node + ":destroy-" + vmid
	resp := sdknodes.DeleteQemuResponse(upid)
	return &resp, nil
}

// managedParkerRig is an empty parker 90000 on pve01 of this Director, which
// PVE HA manages as stopped, under pve.ha.mode managed.
type managedParkerRig struct {
	client *managedParkerClient
	scene  *HADeleteScene
	deps   Deps
}

func newManagedParkerRig() *managedParkerRig {
	scene := NewHADeleteScene()
	scene.Members = []string{"pve01", "pve02", "pve03"}
	scene.IndexTags = "bosh-cpi;bosh-parker"
	scene.Manage(90000, phNode, parkerHAStateStopped, "stopped")
	scene.Comments[phSID] = ourMarker()
	pc := newPHClient(map[int]map[string]any{90000: phParker()}, newPHCluster("pve01", "pve02", "pve03"))
	client := &managedParkerClient{phClient: pc, scene: scene}
	profile := phProfile(config.HAModeManaged)
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeManaged)}
	return &managedParkerRig{client: client, scene: scene, deps: Deps{
		Config: cfg, RequestDirectorUUID: aaDirector, PVE: client, Logger: log.NewNopLogger(),
		HAProfile: func(context.Context) (*ha.Profile, error) { return profile, nil },
	}}
}

func (r *managedParkerRig) haWrites() []string {
	var out []string
	for _, e := range r.scene.EventsSnapshot() {
		if strings.HasPrefix(e, "UpdateHaResources:") || strings.HasPrefix(e, "CreateHaResources:") ||
			strings.HasPrefix(e, "DeleteHaResources:") {
			out = append(out, e)
		}
	}
	return out
}

// TestReconcileParkerHA_RetiresAManagedParkerUnderThePassDeadline runs the
// pass as a sweep runs it, with its own deadline, on an empty parker PVE HA
// manages. The retirement's HA stop must leave the teardown's destroy reserve
// free, which the pass's deadline alone is too short for, so the parker must
// be destroyed with purge, and the HA stop of a parker that already requests
// stopped writes nothing.
func TestReconcileParkerHA_RetiresAManagedParkerUnderThePassDeadline(t *testing.T) {
	r := newManagedParkerRig()
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())

	destroys := r.client.destroys()
	if len(destroys) != 1 {
		t.Fatalf("the empty managed parker must be destroyed once; destroys %d events %v", len(destroys),
			r.scene.EventsSnapshot())
	}
	if p := destroys[0].Purge; p == nil || !*p {
		t.Fatal("the destroy must purge, which removes the HA resource and its rule memberships")
	}
	if w := r.haWrites(); len(w) != 0 {
		t.Fatalf("a parker that requests stopped needs no HA write to stop; writes %v", w)
	}
	if got := r.client.protectionWrites(); !slices.Equal(got, []string{"90000:false"}) {
		t.Fatalf("protection writes %v, want one clear right before the destroy", got)
	}
}

// TestReconcileParkerHA_ARequestTooShortForARetirementSkipsIt proves a
// request whose deadline leaves less than a retirement's budget leaves the
// empty parker for a later sweep without touching it.
func TestReconcileParkerHA_ARequestTooShortForARetirementSkipsIt(t *testing.T) {
	r := newManagedParkerRig()
	ctx, cancel := context.WithTimeout(context.Background(), parkerRetireBudget()-time.Minute)
	defer cancel()
	reconcileParkerHA(ctx, r.deps, phNode, phParkerCfg())

	if len(r.client.destroys()) != 0 || len(r.haWrites()) != 0 || len(r.client.protectionWrites()) != 0 {
		t.Fatalf("a skipped retirement changes nothing; destroys %d events %v protection %v",
			len(r.client.destroys()), r.scene.EventsSnapshot(), r.client.protectionWrites())
	}
}

// TestParkerHA_RetireWithBudget covers the retirement's budget decisions.
func TestParkerHA_RetireWithBudget(t *testing.T) {
	if parkerRetireBudget() <= haDeleteDestroyReserve() || parkerRetireBudget() <= parkerHAReconcileTimeout {
		t.Fatalf("a retirement's budget %s must cover the destroy reserve %s and exceed the pass's %s",
			parkerRetireBudget(), haDeleteDestroyReserve(), parkerHAReconcileTimeout)
	}
	t.Run("one retirement per pass", func(t *testing.T) {
		r := newManagedParkerRig()
		pass := &parkerHA{deps: r.deps, logger: log.NewNopLogger(), node: phNode, cfg: phParkerCfg(),
			owner: aaDirector, managed: true, retired: true, base: context.Background()}
		if err := pass.retireWithBudget(90000); err != nil || len(r.client.destroys()) != 0 {
			t.Fatalf("a second retirement waits for the next sweep; err %v destroys %d", err,
				len(r.client.destroys()))
		}
	})
	t.Run("a parker that empties during the pass waits under the pass's claim", func(t *testing.T) {
		r := newManagedParkerRig()
		pass := &parkerHA{deps: r.deps, logger: log.NewNopLogger(), node: phNode, cfg: phParkerCfg(),
			owner: aaDirector, managed: true, profile: phProfile(config.HAModeManaged)}
		if err := pass.claimed(context.Background(), 90000, false); err != nil || len(r.client.destroys()) != 0 {
			t.Fatalf("only a retirement's own budget retires; err %v destroys %d", err, len(r.client.destroys()))
		}
	})
}

// TestParkerRetire_SnapshotsRefuseBeforeAnyHARead proves the snapshot guard
// runs before the teardown reads or writes HA state, so a parker that must
// stay costs no HA write on every sweep.
func TestParkerRetire_SnapshotsRefuseBeforeAnyHARead(t *testing.T) {
	r := newManagedParkerRig()
	r.client.snapshots[90000] = []string{"before-upgrade"}
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())

	if len(r.client.destroys()) != 0 || len(r.client.protectionWrites()) != 0 {
		t.Fatal("a parker with snapshots must stay")
	}
	if events := r.scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("the snapshot guard must refuse before any HA write; events %v", events)
	}
	var reads int
	r.scene.Do(func(s *HADeleteScene) { reads = s.ManagerReads })
	if reads != 0 {
		t.Fatalf("the snapshot guard must refuse before the teardown reads the CRM; manager reads %d", reads)
	}
}

// protectionCtxNodes records whether each protection write ran on a live
// context.
type protectionCtxNodes struct {
	*phNodes
	mu   sync.Mutex
	live []bool
}

func (n *protectionCtxNodes) UpdateQemuConfig(
	ctx context.Context, node, vmid string, p *sdknodes.UpdateQemuConfigParams,
) error {
	n.mu.Lock()
	n.live = append(n.live, ctx.Err() == nil)
	n.mu.Unlock()
	return n.phNodes.UpdateQemuConfig(ctx, node, vmid, p)
}

type protectionCtxClient struct {
	*phClient
	nodes *protectionCtxNodes
}

func (c *protectionCtxClient) Nodes() sdknodes.Service { return c.nodes }

// TestParkerRetire_RestoresProtectionOnALiveContext proves the restore after
// a failed retirement runs on a context of its own when the retirement's
// context has ended, so the parker is protected again.
func TestParkerRetire_RestoresProtectionOnALiveContext(t *testing.T) {
	pc := newPHClient(map[int]map[string]any{90000: phParker()}, newPHCluster("pve01"))
	nodes := &protectionCtxNodes{phNodes: &phNodes{c: pc}}
	pass := &parkerHA{
		deps:   Deps{Config: icMinConfig(), PVE: &protectionCtxClient{phClient: pc, nodes: nodes}},
		logger: log.NewNopLogger(), node: phNode, cfg: phParkerCfg(), owner: aaDirector,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pass.restoreProtection(ctx, 90000, log.NewNopLogger())

	if got := pc.protectionWrites(); !slices.Equal(got, []string{"90000:true"}) {
		t.Fatalf("protection writes %v, want the parker protected again", got)
	}
	if !slices.Equal(nodes.live, []bool{true}) {
		t.Fatalf("the restore must run on a live context; live %v", nodes.live)
	}
}

// TestReconcileParkerHA_PermissionDeniedHaltsThePass proves an HA read PVE
// refuses for a missing grant stops the pass at the first parker and writes
// nothing, in both modes that read.
func TestReconcileParkerHA_PermissionDeniedHaltsThePass(t *testing.T) {
	for _, mode := range []string{config.HAModeAuto, config.HAModeManaged} {
		t.Run(mode, func(t *testing.T) {
			r := newPHRig(mode, map[int]map[string]any{
				90000: phParker("nfs:vm-90000-disk-0"),
				90001: phParker("nfs:vm-90001-disk-0"),
			})
			r.cluster.getErr = phForbidden()
			reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
			if r.cluster.getCalls != 1 || len(r.cluster.log.events) != 0 {
				t.Fatalf("the pass must stop after the first refused read; reads %d events %v",
					r.cluster.getCalls, r.cluster.log.events)
			}
		})
	}
	t.Run("the error names the grant and is permanent", func(t *testing.T) {
		r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
		r.cluster.getErr = phForbidden()
		pass := r.pass()
		err := pass.reconcile(context.Background(), 90000)
		if err == nil || okToRetryCPIError(err) || !pass.halted {
			t.Fatalf("want a permanent error that halts the pass; err %v halted %v", err, pass.halted)
		}
	})
}

// TestParkerHA_ReadFailuresAreClassifiedByCause proves the pass's reads keep
// a 4xx permanent and a 5xx retriable.
func TestParkerHA_ReadFailuresAreClassifiedByCause(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	r.cluster.getErr = crh500("pve02 unreachable")
	if _, err := r.pass().read(context.Background(), 90000); err == nil || !okToRetryCPIError(err) {
		t.Fatalf("a 5xx may pass; err %v", err)
	}
	r.cluster.getErr = nil
	r.client.configErr[90000] = sdkerrors.ParseAPIError(400, []byte("invalid vmid"))
	if _, err := r.pass().read(context.Background(), 90000); err == nil || okToRetryCPIError(err) {
		t.Fatalf("a refusal of the request cannot pass; err %v", err)
	}
}

// TestReconcileParkerHA_ResolvesTheProfileOnlyWhenAParkerNeedsIt proves a
// pass whose parkers need nothing never resolves the HA profile, and that a
// pass resolves it at most once.
func TestReconcileParkerHA_ResolvesTheProfileOnlyWhenAParkerNeedsIt(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{
		90000: phParker("nfs:vm-90000-disk-0"),
		90001: phParker("nfs:vm-90001-disk-0"),
	})
	r.cluster.res[phSID] = &phResource{state: parkerHAStateStopped, comment: ourMarker()}
	r.cluster.res["vm:90001"] = &phResource{state: parkerHAStateStopped, comment: ourMarker()}
	resolved := 0
	profile := r.profile
	r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { resolved++; return profile, nil }
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	if resolved != 0 {
		t.Fatalf("registered parkers that hold disks need nothing; profile resolved %d times", resolved)
	}

	delete(r.cluster.res, phSID)
	delete(r.cluster.res, "vm:90001")
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	if resolved != 1 {
		t.Fatalf("a pass resolves the profile once however many parkers need it; resolved %d times", resolved)
	}
}

// claimCountingClient serves the VMID claims a pass takes from lock pools
// that count them.
type claimCountingClient struct {
	*phClient
	pools *exclusiveLockPools
}

func (c *claimCountingClient) Pools() pve.PoolService { return c.pools }

func (c *claimCountingClient) claims() int {
	c.pools.mu.Lock()
	defer c.pools.mu.Unlock()
	return c.pools.creates
}

// TestReconcileParkerHA_ANodeLocalParkerTakesNoClaim proves an unregistered
// parker that can never register costs no claim and no decision.
func TestReconcileParkerHA_ANodeLocalParkerTakesNoClaim(t *testing.T) {
	r := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("local-lvm:vm-90000-disk-0")})
	c := &claimCountingClient{phClient: r.client, pools: newExclusiveLockPools()}
	r.deps.PVE = c
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	if c.claims() != 0 || len(r.cluster.log.events) != 0 {
		t.Fatalf("a node-local parker must take no claim; claims %d events %v", c.claims(), r.cluster.log.events)
	}

	r = newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})
	c = &claimCountingClient{phClient: r.client, pools: newExclusiveLockPools()}
	r.deps.PVE = c
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	if c.claims() == 0 || r.state() != parkerHAStateStopped {
		t.Fatalf("a parker that registers takes its claim; claims %d state %q", c.claims(), r.state())
	}
}
