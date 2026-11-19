package handlers

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// orphanRigClient is the emptied parker fake with a cluster that records,
// for every HA write, whether parker 90000's claim was held while it ran.
type orphanRigClient struct {
	*retireRigClient
	mu          sync.Mutex
	heldAtWrite map[string]bool
}

func (c *orphanRigClient) Cluster() cluster.Service {
	return &orphanRigCluster{Service: c.retireRigClient.Cluster(), c: c}
}

func (c *orphanRigClient) noteWrite(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heldAtWrite[what] = c.pools().held(90000)
}

func (c *orphanRigClient) writesHeld() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for k, v := range c.heldAtWrite {
		out[k] = v
	}
	return out
}

type orphanRigCluster struct {
	cluster.Service
	c *orphanRigClient
}

func (cl *orphanRigCluster) DeleteHaResources(
	ctx context.Context, sid string, params *cluster.DeleteHaResourcesParams,
) error {
	cl.c.noteWrite("resource")
	return cl.Service.DeleteHaResources(ctx, sid, params)
}

func (cl *orphanRigCluster) UpdateHaRules(ctx context.Context, rule string, params *cluster.UpdateHaRulesParams) error {
	cl.c.noteWrite("pin")
	return cl.Service.UpdateHaRules(ctx, rule, params)
}

func (cl *orphanRigCluster) DeleteHaRules(ctx context.Context, rule string) error {
	cl.c.noteWrite("pin")
	return cl.Service.DeleteHaRules(ctx, rule)
}

// orphanRig is parker 90000 of this Director after a destroy that removed its
// config but left its HA resource and its pin behind. The config is gone
// from every node and from the cluster index, and the HA manager no longer
// lists the service, as PVE drops a service whose VM has no config. No other
// parker is on the node.
type orphanRig struct {
	*emptiedRig
	orphans *orphanRigClient
}

func newOrphanRig() *orphanRig {
	base := newEmptiedParkerRig()
	base.client.phClient.mu.Lock()
	delete(base.client.configs, 90000)
	base.client.phClient.mu.Unlock()
	base.scene.Do(func(s *HADeleteScene) {
		delete(s.IndexNode, 90000)
		delete(s.Services, phSID)
	})
	client := &orphanRigClient{retireRigClient: base.client, heldAtWrite: map[string]bool{}}
	base.deps.PVE = client
	return &orphanRig{emptiedRig: base, orphans: client}
}

// sweep runs the parker HA pass of a sweep on node pve01 over cfg's band.
func (r *orphanRig) sweep(cfg pve.ParkerConfig) {
	reconcileParkerHA(context.Background(), r.deps, phNode, cfg)
}

func (r *orphanRig) haRemovals() []string {
	var out []string
	for _, e := range r.scene.EventsSnapshot() {
		if strings.HasPrefix(e, "DeleteHaResources:") || strings.HasPrefix(e, "DeleteHaRules:") ||
			strings.HasPrefix(e, "UpdateHaRules:") {
			out = append(out, e)
		}
	}
	return out
}

// requireRemoved fails unless the pin is gone, and the resource too when
// resource is set, each removed while the parker's claim was held, and the
// claim was released after. A resource is only ever removed with its pin,
// because the pin names the node its absence proof reads.
func (r *orphanRig) requireRemoved(t *testing.T, resource bool) {
	t.Helper()
	if resource && r.resourcePresent() {
		t.Fatalf("the leftover HA resource must be removed; events %v", r.scene.EventsSnapshot())
	}
	if r.pinPresent() {
		t.Fatalf("the leftover pin must be removed; events %v", r.scene.EventsSnapshot())
	}
	for what, held := range r.orphans.writesHeld() {
		if !held {
			t.Fatalf("the %s removal must run under the parker's claim", what)
		}
	}
	if r.client.pools().held(90000) {
		t.Fatal("the removal must release the parker's claim")
	}
	if n := len(r.client.destroys()); n != 0 {
		t.Fatalf("a parker that is gone is never destroyed again; destroys %d", n)
	}
}

// requireKept fails unless the step wrote nothing and the leftovers named are
// still in place.
func (r *orphanRig) requireKept(t *testing.T, resource, pin bool) {
	t.Helper()
	if got := r.haRemovals(); len(got) != 0 {
		t.Fatalf("a leftover the step cannot prove is kept costs no write; writes %v", got)
	}
	if resource && !r.resourcePresent() {
		t.Fatal("the HA resource must stay")
	}
	if pin && !r.pinPresent() {
		t.Fatal("the pin must stay")
	}
}

// TestParkerHAOrphans_RemovesALeftoverResourceAndItsPin proves the parker
// sweep removes the HA resource and the pin of a parker of this Director whose
// config no node holds, under the parker's claim, naming the node to read
// from the pin when the HA manager no longer lists the service.
func TestParkerHAOrphans_RemovesALeftoverResourceAndItsPin(t *testing.T) {
	r := newOrphanRig()
	r.sweep(phParkerCfg())
	r.requireRemoved(t, true)
	if got := r.orphans.writesHeld(); !got["resource"] {
		t.Fatalf("the resource removal must be one of the writes; writes %v", got)
	}
}

// TestParkerHAOrphans_RemovesAPinThatNamesAMissingParker proves a pin of this
// Director whose parker and HA resource are both gone is removed.
func TestParkerHAOrphans_RemovesAPinThatNamesAMissingParker(t *testing.T) {
	r := newOrphanRig()
	r.scene.Do(func(s *HADeleteScene) {
		delete(s.Requested, phSID)
		delete(s.Comments, phSID)
	})
	r.sweep(phParkerCfg())
	r.requireRemoved(t, false)
	if got := r.orphans.writesHeld(); !got["pin"] {
		t.Fatalf("the pin removal must be one of the writes; writes %v", got)
	}
}

// TestParkerHAOrphans_KeepsWhatItCannotProve proves every leftover the step
// cannot prove is this Director's parker's, or cannot prove gone, stays in
// place with no write, for the doctor to report.
func TestParkerHAOrphans_KeepsWhatItCannotProve(t *testing.T) {
	other := haIdentity{Owner: "another-director"}.marker()
	cases := []struct {
		name     string
		cfg      pve.ParkerConfig
		mutate   func(r *orphanRig)
		resource bool
		pin      bool
	}{
		{name: "the cluster index lists the parker", mutate: func(r *orphanRig) {
			r.scene.Place(90000, "pve02")
		}, resource: true, pin: true},
		{name: "a node holds the config the index does not list yet", mutate: func(r *orphanRig) {
			r.client.phClient.mu.Lock()
			r.client.configs[90000] = phParker()
			r.client.phClient.mu.Unlock()
		}, resource: true, pin: true},
		{name: "the config read fails", mutate: func(r *orphanRig) {
			r.client.phClient.mu.Lock()
			r.client.configErr[90000] = &sdkerrors.APIError{HTTPCode: 500, Message: "proxy timeout"}
			r.client.phClient.mu.Unlock()
		}, resource: true, pin: true},
		{name: "another Director's resource and pin", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.Comments[phSID] = other
				s.Rules[naRuleNameFor(90000)]["comment"] = other
			})
		}, resource: true, pin: true},
		{name: "an unmarked resource and pin", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) {
				delete(s.Comments, phSID)
				s.Rules[naRuleNameFor(90000)]["comment"] = "kept by hand"
			})
		}, resource: true, pin: true},
		{name: "a resource marked for an instance group", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.Comments[phSID] = haIdentity{Owner: aaDirector, Deployment: "cf", Group: "router"}.marker()
				delete(s.Rules, naRuleNameFor(90000))
			})
		}, resource: true},
		{name: "a pin that names another resource too", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) {
				delete(s.Requested, phSID)
				delete(s.Comments, phSID)
				s.Rules[naRuleNameFor(90000)]["resources"] = phSID + ",vm:100"
			})
		}, pin: true},
		{name: "a VMID outside the parker band", cfg: pve.ParkerConfig{VMIDRangeStart: 91000, VMIDRangeEnd: 91999},
			resource: true, pin: true},
		{name: "the free strategy", mutate: func(r *orphanRig) {
			r.deps.Config.DetachedDiskStrategy = "free"
		}, resource: true, pin: true},
		{name: "no pin to name a node to read", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) { delete(s.Rules, naRuleNameFor(90000)) })
		}, resource: true},
		{name: "the HA manager still holds the service", mutate: func(r *orphanRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.Services[phSID] = HASceneService{State: "stopped", Node: "pve02", UID: "uid-90000"}
			})
		}, resource: true, pin: true},
		{name: "a node the pin names is not one the cluster reports online", mutate: func(r *orphanRig) {
			r.scene.MarkNodeDead("pve03")
		}, resource: true, pin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newOrphanRig()
			if tc.mutate != nil {
				tc.mutate(r)
			}
			cfg := tc.cfg
			if cfg.VMIDRangeStart == 0 {
				cfg = phParkerCfg()
			}
			r.sweep(cfg)
			r.requireKept(t, tc.resource, tc.pin)
		})
	}
}

// TestParkerHAOrphans_AHeldClaimKeepsTheLeftovers proves a park or another
// pass that holds the parker's claim keeps the leftovers in place, and that
// the next sweep after the claim is released removes them.
func TestParkerHAOrphans_AHeldClaimKeepsTheLeftovers(t *testing.T) {
	r := newOrphanRig()
	err := pve.RunUnderParkerLock(context.Background(), r.client, log.NewNopLogger(), 90000, "park",
		func(context.Context) error {
			r.sweep(phParkerCfg())
			return nil
		})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	r.requireKept(t, true, true)

	r.sweep(phParkerCfg())
	r.requireRemoved(t, true)
}

// TestParkerHAOrphans_AParkerCreatedBeforeTheClaimedReadKeepsItsLeftovers
// proves a parker created at the VMID after the step listed the cluster, and
// before its read under the claim, keeps the resource and the pin, because the
// read under the claim finds its config.
func TestParkerHAOrphans_AParkerCreatedBeforeTheClaimedReadKeepsItsLeftovers(t *testing.T) {
	r := newOrphanRig()
	r.client.onClaimedRead = func() {
		r.client.phClient.mu.Lock()
		r.client.configs[90000] = phParker()
		r.client.phClient.mu.Unlock()
	}
	r.sweep(phParkerCfg())
	r.requireKept(t, true, true)
	if r.client.claimedReads == 0 {
		t.Fatal("the step must read the parker's config under its claim")
	}
}

// TestParkerHAOrphans_AFailedResourceRemovalKeepsThePin proves the pin of a
// leftover resource whose removal failed stays, so the next sweep finds the
// same pair and the node to read from the pin.
func TestParkerHAOrphans_AFailedResourceRemovalKeepsThePin(t *testing.T) {
	r := newOrphanRig()
	r.scene.Do(func(s *HADeleteScene) { s.RefuseRemoval = true })
	r.sweep(phParkerCfg())
	if !r.resourcePresent() || !r.pinPresent() {
		t.Fatalf("a failed removal keeps both; resource %v pin %v", r.resourcePresent(), r.pinPresent())
	}
	if got := r.haRemovals(); !slices.Equal(got, []string{"DeleteHaResources:" + phSID}) {
		t.Fatalf("writes %v, want the one refused resource removal", got)
	}

	r.scene.Do(func(s *HADeleteScene) { s.RefuseRemoval = false })
	r.sweep(phParkerCfg())
	r.requireRemoved(t, true)
}
