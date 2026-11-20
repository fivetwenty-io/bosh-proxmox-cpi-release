package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// retireRigClient is the managed parker fake with the hooks the retirement of
// an emptied parker is tested through: a destroy that can fail and that
// records whether the parker's claim was held while it ran, a purge that
// prunes the parker from its rules the way PVE does, pending changes served on
// the pending endpoint, and a hook that runs at the first config read made
// while the parker's claim is held.
type retireRigClient struct {
	*managedParkerClient
	mu            sync.Mutex
	destroyErr    error
	heldAtDestroy []bool
	pending       map[string]string
	onClaimedRead func()
	claimedReads  int
	// claimedStatus counts the power-state reads of parker 90000 made while
	// its claim is held, which only the retirement's guard makes, after the
	// read under the claim decided to retire.
	claimedStatus int
}

func (c *retireRigClient) QEMU() qemu.Service {
	return &retireRigQEMU{Service: c.managedParkerClient.QEMU(), c: c}
}

type retireRigQEMU struct {
	qemu.Service
	c *retireRigClient
}

func (q *retireRigQEMU) Status(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if vmid == 90000 && q.c.pools().held(90000) {
		q.c.mu.Lock()
		q.c.claimedStatus++
		q.c.mu.Unlock()
	}
	return q.Service.Status(ctx, node, vmid)
}

func (c *retireRigClient) Nodes() sdknodes.Service {
	return &retireRigNodes{Service: c.managedParkerClient.Nodes(), c: c}
}

func (c *retireRigClient) pools() *exclusiveLockPools { return c.scene.lockPoolService() }

type retireRigNodes struct {
	sdknodes.Service
	c *retireRigClient
}

func (n *retireRigNodes) ListQemuPending(
	ctx context.Context, node, vmid string,
) (*sdknodes.ListQemuPendingResponse, error) {
	if vmid == "90000" && n.c.pools().held(90000) {
		n.c.mu.Lock()
		n.c.claimedReads++
		hook := n.c.onClaimedRead
		first := n.c.claimedReads == 1
		n.c.mu.Unlock()
		if first && hook != nil {
			hook()
		}
	}
	resp, err := n.Service.ListQemuPending(ctx, node, vmid)
	if err != nil || resp == nil || vmid != "90000" {
		return resp, err
	}
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	out := slices.Clone(*resp)
	for key, value := range n.c.pending {
		raw, err := json.Marshal(map[string]any{"key": key, "pending": value})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

func (n *retireRigNodes) DeleteQemu(
	ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams,
) (*sdknodes.DeleteQemuResponse, error) {
	n.c.mu.Lock()
	n.c.heldAtDestroy = append(n.c.heldAtDestroy, n.c.pools().held(90000))
	destroyErr := n.c.destroyErr
	n.c.mu.Unlock()
	if destroyErr != nil {
		n.c.managedParkerClient.mu.Lock()
		n.c.destroyed = append(n.c.destroyed, params)
		n.c.managedParkerClient.mu.Unlock()
		return nil, destroyErr
	}
	resp, err := n.Service.DeleteQemu(ctx, node, vmid, params)
	if err == nil && params != nil && params.Purge != nil && *params.Purge {
		n.c.scene.Do(func(s *HADeleteScene) { s.pruneRules("vm:" + vmid) })
	}
	return resp, err
}

// emptiedRig is an empty parker 90000 on pve01 of this Director that PVE HA
// manages as stopped, under pve.ha.mode managed, with its pin in place.
type emptiedRig struct {
	*managedParkerRig
	client *retireRigClient
}

func newEmptiedParkerRig() *emptiedRig {
	base := newManagedParkerRig()
	client := &retireRigClient{managedParkerClient: base.client, pending: map[string]string{}}
	base.deps.PVE = client
	base.scene.Do(func(s *HADeleteScene) {
		s.Rules[naRuleNameFor(90000)] = map[string]any{
			"type": naRuleType, "resources": phSID, "nodes": "pve01,pve02,pve03", "strict": 0,
			"comment": ourMarker(),
		}
	})
	return &emptiedRig{managedParkerRig: base, client: client}
}

func (r *emptiedRig) retire() {
	retireEmptiedParkerNow(context.Background(), r.deps, phNode, 90000, "test")
}

func (r *emptiedRig) pinPresent() bool {
	present := false
	r.scene.Do(func(s *HADeleteScene) { _, present = s.Rules[naRuleNameFor(90000)] })
	return present
}

func (r *emptiedRig) resourcePresent() bool {
	present := false
	r.scene.Do(func(s *HADeleteScene) { _, present = s.Requested[phSID] })
	return present
}

func (r *emptiedRig) parkerPresent() bool {
	r.client.phClient.mu.Lock()
	defer r.client.phClient.mu.Unlock()
	_, ok := r.client.configs[90000]
	return ok
}

func (r *emptiedRig) setConfig(mutate func(cfg map[string]any)) {
	r.client.phClient.mu.Lock()
	defer r.client.phClient.mu.Unlock()
	mutate(r.client.configs[90000])
}

// requireUntouched fails unless the parker, its protection, its HA resource,
// and its pin are all as the rig built them.
func (r *emptiedRig) requireUntouched(t *testing.T) {
	t.Helper()
	if n := len(r.client.destroys()); n != 0 {
		t.Fatalf("the parker must stay; destroys %d", n)
	}
	if got := r.client.protectionWrites(); len(got) != 0 {
		t.Fatalf("a parker that stays keeps its protection; writes %v", got)
	}
	if w := r.haWrites(); len(w) != 0 {
		t.Fatalf("a parker that stays costs no HA write; writes %v", w)
	}
	if !r.parkerPresent() || !r.pinPresent() {
		t.Fatalf("the parker and its pin must stay; parker %v pin %v", r.parkerPresent(), r.pinPresent())
	}
}

// requireRetired fails unless the parker was destroyed once with purge and no
// sweep of unreferenced volumes, and its HA resource and pin are gone.
func (r *emptiedRig) requireRetired(t *testing.T) {
	t.Helper()
	destroys := r.client.destroys()
	if len(destroys) == 0 {
		t.Fatalf("the emptied parker must be destroyed; events %v", r.scene.EventsSnapshot())
	}
	last := destroys[len(destroys)-1]
	if last.Purge == nil || !*last.Purge {
		t.Fatal("the destroy must purge, which removes the HA resource and its rule memberships")
	}
	if last.DestroyUnreferencedDisks == nil || *last.DestroyUnreferencedDisks {
		t.Fatal("the destroy must not sweep unreferenced volumes named for the parker")
	}
	if r.parkerPresent() || r.resourcePresent() || r.pinPresent() {
		t.Fatalf("the parker, its HA resource, and its pin must be gone; parker %v resource %v pin %v",
			r.parkerPresent(), r.resourcePresent(), r.pinPresent())
	}
	if r.client.pools().held(90000) {
		t.Fatal("the retirement must release the parker's claim")
	}
}

// TestRetireEmptiedParker_DestroysAnEmptyManagedParker proves the call that
// emptied a parker PVE HA manages destroys it with its HA resource and pin,
// clears its protection only right before the destroy, and holds the
// parker's claim for the whole destroy.
func TestRetireEmptiedParker_DestroysAnEmptyManagedParker(t *testing.T) {
	r := newEmptiedParkerRig()
	r.retire()

	r.requireRetired(t)
	if n := len(r.client.destroys()); n != 1 {
		t.Fatalf("one destroy, got %d", n)
	}
	if got := r.client.protectionWrites(); !slices.Equal(got, []string{"90000:false"}) {
		t.Fatalf("protection writes %v, want one clear right before the destroy", got)
	}
	if !slices.Equal(r.client.heldAtDestroy, []bool{true}) {
		t.Fatalf("the destroy must run under the parker's claim; held %v", r.client.heldAtDestroy)
	}
}

// TestRetireEmptiedParker_KeepsAParkerThatIsNotEmptyOrNotOurs proves every
// parker the retirement must not take stays as it is, with no destroy, no
// protection write, and no HA write.
func TestRetireEmptiedParker_KeepsAParkerThatIsNotEmptyOrNotOurs(t *testing.T) {
	cases := map[string]func(r *emptiedRig){
		"PVE HA does not manage it": func(r *emptiedRig) {
			r.scene.Do(func(s *HADeleteScene) {
				delete(s.Requested, phSID)
				delete(s.Services, phSID)
				delete(s.Comments, phSID)
			})
		},
		"it holds a disk": func(r *emptiedRig) {
			r.setConfig(func(cfg map[string]any) { cfg["scsi0"] = "nfs:vm-90000-disk-0" })
		},
		"an unused entry names a volume": func(r *emptiedRig) {
			r.setConfig(func(cfg map[string]any) { cfg["unused0"] = "nfs:vm-90000-disk-1" })
		},
		"a pending change names a volume": func(r *emptiedRig) {
			r.client.pending["scsi1"] = "nfs:vm-700-disk-9"
		},
		"a transfer record names a volume": func(r *emptiedRig) {
			r.setConfig(func(cfg map[string]any) { cfg["description"] = phParkerWithRecord()["description"] })
		},
		"its HA resource carries another Director's marker": func(r *emptiedRig) {
			r.scene.Do(func(s *HADeleteScene) {
				s.Comments[phSID] = haIdentity{Owner: "11111111-2222-3333-4444-555555555555"}.marker()
			})
		},
		"it is a migration mover": func(r *emptiedRig) {
			r.setConfig(func(cfg map[string]any) { cfg["tags"] = "bosh-cpi;bosh-parker;" + pve.DiskMoverTag })
		},
		"it carries a snapshot": func(r *emptiedRig) {
			r.client.snapshots[90000] = []string{"before-upgrade"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newEmptiedParkerRig()
			setup(r)
			r.retire()
			r.requireUntouched(t)
		})
	}
	t.Run("off mode reads nothing", func(t *testing.T) {
		r := newEmptiedParkerRig()
		r.deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeOff)}
		r.retire()
		r.requireUntouched(t)
		if events := r.scene.EventsSnapshot(); len(events) != 0 {
			t.Fatalf("off mode must not touch PVE HA; events %v", events)
		}
	})
}

// TestRetireEmptiedParker_AHeldWindowLeavesTheParkerForTheSweep proves a
// retirement that finds the parker's window held, as a park into the parker
// holds it, takes no claim, destroys nothing, and never waits for the window,
// and that a retirement once the window is released destroys the parker.
func TestRetireEmptiedParker_AHeldWindowLeavesTheParkerForTheSweep(t *testing.T) {
	r := newEmptiedParkerRig()
	err := pve.RunUnderParkerLock(context.Background(), r.client, log.NewNopLogger(), 90000, "park",
		func(context.Context) error {
			if !r.client.pools().held(90000) {
				t.Fatal("the window must hold the parker's lock")
			}
			r.retire()
			return nil
		})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	r.requireUntouched(t)

	r.retire()
	r.requireRetired(t)
}

// TestRetireEmptiedParker_AParkThatLandsBeforeTheClaimKeepsTheParker proves
// the read under the parker's claim decides, not the read before it: a disk
// that lands between the two keeps the parker.
func TestRetireEmptiedParker_AParkThatLandsBeforeTheClaimKeepsTheParker(t *testing.T) {
	for name, land := range map[string]func(cfg map[string]any){
		"a disk in a slot":  func(cfg map[string]any) { cfg["scsi0"] = "nfs:vm-700-disk-1" },
		"a transfer record": func(cfg map[string]any) { cfg["description"] = phParkerWithRecord()["description"] },
	} {
		t.Run(name, func(t *testing.T) {
			r := newEmptiedParkerRig()
			r.client.onClaimedRead = func() { r.setConfig(land) }
			r.retire()
			if r.client.claimedReads == 0 {
				t.Fatal("the retirement must read the parker again under its claim")
			}
			if r.client.claimedStatus != 0 {
				t.Fatalf("the read under the claim must keep the parker before the retirement's guard runs; "+
					"the guard read its power state %d times", r.client.claimedStatus)
			}
			r.requireUntouched(t)
		})
	}
}

// TestRetireEmptiedParker_AFailedDestroyLeavesStateTheSweepFinishes proves a
// destroy that fails keeps the parker protected, registered, and pinned, so
// the doctor sees a consistent parker and the next sweep on its node retires
// it.
func TestRetireEmptiedParker_AFailedDestroyLeavesStateTheSweepFinishes(t *testing.T) {
	r := newEmptiedParkerRig()
	r.client.destroyErr = errors.New("unable to destroy VM 90000 - storage is not online (HTTP 400)")
	r.retire()

	if !r.parkerPresent() || !r.resourcePresent() || !r.pinPresent() {
		t.Fatalf("a failed destroy keeps the parker, its HA resource, and its pin; parker %v resource %v pin %v",
			r.parkerPresent(), r.resourcePresent(), r.pinPresent())
	}
	if got := r.client.protectionWrites(); !slices.Equal(got, []string{"90000:false", "90000:true"}) {
		t.Fatalf("protection writes %v, want the clear and then the restore", got)
	}
	if r.client.pools().held(90000) {
		t.Fatal("a failed retirement must release the parker's claim")
	}

	r.client.mu.Lock()
	r.client.destroyErr = nil
	r.client.mu.Unlock()
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	r.requireRetired(t)
}

// TestRetireEmptiedParker_RemovesAPinPVEKeptOrDisabled proves the pin is gone
// after the retirement even on a PVE that keeps or disables a rule whose
// resource the purge removed, so no disabled pin is left for the doctor.
func TestRetireEmptiedParker_RemovesAPinPVEKeptOrDisabled(t *testing.T) {
	for name, set := range map[string]func(s *HADeleteScene){
		"disabled": func(s *HADeleteScene) { s.DisableOrphanRules = true },
		"kept":     func(s *HADeleteScene) { s.PurgeKeepsRules = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newEmptiedParkerRig()
			r.scene.Do(set)
			r.retire()
			r.requireRetired(t)
		})
	}
}

// TestRetireEmptiedParker_ARetireOnlyPassRegistersNothing proves a parker the
// read under the claim finds holding a disk again is not registered or
// restaged by the retirement, which leaves that to the sweep.
func TestRetireEmptiedParker_ARetireOnlyPassRegistersNothing(t *testing.T) {
	r := newEmptiedParkerRig()
	r.scene.Do(func(s *HADeleteScene) {
		s.Requested[phSID] = parkerHAStateIgnored
	})
	r.client.onClaimedRead = func() { r.setConfig(func(cfg map[string]any) { cfg["scsi0"] = "nfs:vm-700-disk-1" }) }
	r.retire()
	r.requireUntouched(t)
}

// TestRetireEmptiedParker_SkipsANodeOrVMIDItCannotName proves the shared
// entry calls the retirement only for a parker it can name.
func TestRetireEmptiedParker_SkipsANodeOrVMIDItCannotName(t *testing.T) {
	var calls int
	defer setRetireEmptiedParkerForTest(func(context.Context, Deps, string, int, string) { calls++ })()
	retireEmptiedParker(context.Background(), Deps{}, "", 90000, "test")
	retireEmptiedParker(context.Background(), Deps{}, phNode, 0, "test")
	retireEmptiedHolder(context.Background(), Deps{}, pve.DiskHolder{Found: true, VMID: 700, Node: phNode}, "test")
	retireEmptiedHolder(context.Background(), Deps{}, pve.DiskHolder{VMID: 90000, Node: phNode, IsParker: true}, "test")
	if calls != 0 {
		t.Fatalf("no parker named, so no retirement; calls %d", calls)
	}
	retireEmptiedHolder(context.Background(), Deps{},
		pve.DiskHolder{Found: true, VMID: 90000, Node: phNode, IsParker: true}, "test")
	if calls != 1 {
		t.Fatalf("a parker holder is retired; calls %d", calls)
	}
}

// setParkerRetireSettleWaitForTest replaces how long the drained retirement
// waits for PVE HA to finish stopping an emptied parker, and returns a restore
// function. The value is process-wide, so tests using it must not call
// t.Parallel.
func setParkerRetireSettleWaitForTest(d time.Duration) func() {
	prev := parkerRetireSettleWaitNs.Swap(int64(d))
	return func() { parkerRetireSettleWaitNs.Store(prev) }
}

// registrationStopRig is the emptied parker of emptiedRig whose HA resource the
// CRM is still stopping, the way it is a few seconds after create_disk
// registered the parker and requested stopped. The CRM finishes the stop at
// manager status read stopAt, or never when stopAt is zero. One read of the HA
// state makes two manager status reads, the health report's and the state's,
// so an odd stopAt finishes the stop between two whole reads.
type registrationStopRig struct {
	*emptiedRig
	mu sync.Mutex
	// heldWhileStopping counts the manager status reads made while the CRM
	// still reported request_stop and the parker's claim was held.
	heldWhileStopping int
}

func newRegistrationStopRig(t *testing.T, stopAt int) *registrationStopRig {
	t.Helper()
	t.Cleanup(SetHADeleteMotionPollForTest(time.Millisecond))
	r := &registrationStopRig{emptiedRig: newEmptiedParkerRig()}
	pools := r.client.pools()
	r.scene.Do(func(s *HADeleteScene) {
		s.Services[phSID] = HASceneService{State: haRawStateRequestStop, Node: phNode, UID: "uid-90000"}
		s.IndexHAState[90000] = haRawStateRequestStop
		s.Power[90000] = vmPowerStateStopped
		s.OnRead = func(s *HADeleteScene, reads int) {
			if s.Services[phSID].State != haRawStateRequestStop {
				return
			}
			if pools.held(90000) {
				r.mu.Lock()
				r.heldWhileStopping++
				r.mu.Unlock()
			}
			if stopAt > 0 && reads >= stopAt {
				s.Services[phSID] = HASceneService{State: "stopped", Node: phNode, UID: "uid-90000-stopped"}
				s.IndexHAState[90000] = "stopped"
			}
		}
	})
	return r
}

// retireThroughDrain runs the retirement the way attach_disk and create_vm run
// it, from the queue their handler drains once it has released its gates and
// locks.
func (r *registrationStopRig) retireThroughDrain() {
	ctx, drain := deferParkerRetirements(context.Background())
	retireEmptiedParker(ctx, r.deps, phNode, 90000, "attach_disk")
	drain()
}

func (r *registrationStopRig) claimsTaken() int {
	pools := r.client.pools()
	pools.mu.Lock()
	defer pools.mu.Unlock()
	return pools.creates
}

// TestRetireEmptiedParker_TheDrainWaitsOutTheRegistrationStop proves the
// retirement an attach runs once it has released its locks waits, holding no
// claim, while PVE HA finishes the stop that the parker's registration
// requested, and then destroys the parker with its HA resource and pin. A
// fresh deploy empties a parker a few seconds after create_disk registered
// it, while the CRM still reports request_stop, so without the wait every
// fresh deploy leaves an empty HA parker behind.
func TestRetireEmptiedParker_TheDrainWaitsOutTheRegistrationStop(t *testing.T) {
	r := newRegistrationStopRig(t, 5)
	r.retireThroughDrain()

	r.requireRetired(t)
	if r.heldWhileStopping != 0 {
		t.Fatalf("the wait for the HA stop must hold no claim on the parker; %d reads held it",
			r.heldWhileStopping)
	}
}

// TestRetireEmptiedParker_AStopThatOutlastsTheWaitLeavesTheParkerForTheSweep
// proves a stop that PVE HA has not finished when the wait runs out leaves
// the parker exactly as it was, with no claim taken, logs why it stays, and
// leaves the retirement to the parker sweep that the next park on the node
// runs.
func TestRetireEmptiedParker_AStopThatOutlastsTheWaitLeavesTheParkerForTheSweep(t *testing.T) {
	r := newRegistrationStopRig(t, 0)
	t.Cleanup(setParkerRetireSettleWaitForTest(20 * time.Millisecond))
	logger, observer := log.NewObservedLogger(log.LevelInfo)
	r.deps.Logger = logger
	r.retireThroughDrain()

	r.requireUntouched(t)
	if n := r.claimsTaken(); n != 0 {
		t.Fatalf("a retirement whose wait ran out takes no claim on the parker; claims %d", n)
	}
	found := false
	for _, e := range observer.All() {
		if strings.Contains(e.Message, "still stopping the emptied parker") &&
			strings.Contains(e.Message, "next parker sweep on its node retires it") &&
			e.Attrs["raw_state"] == haRawStateRequestStop {
			found = true
		}
	}
	if !found {
		t.Fatalf("the log must say the parker stays because PVE HA was still stopping it; entries %+v",
			observer.All())
	}

	r.scene.Do(func(s *HADeleteScene) {
		s.Services[phSID] = HASceneService{State: "stopped", Node: phNode, UID: "uid-90000-stopped"}
		s.IndexHAState[90000] = "stopped"
	})
	reconcileParkerHA(context.Background(), r.deps, phNode, phParkerCfg())
	r.requireRetired(t)
}

// TestRetireEmptiedParker_AnInlineRetirementDoesNotWaitForTheStop proves the
// emptying paths that retire inline, while they may still hold the disk's
// lifecycle lock, read the HA state once and leave a parker PVE HA is still
// stopping for the sweep, as they always have, so no request queued on those
// locks waits for the CRM.
func TestRetireEmptiedParker_AnInlineRetirementDoesNotWaitForTheStop(t *testing.T) {
	r := newRegistrationStopRig(t, 3)
	r.retire()
	r.requireUntouched(t)
}
