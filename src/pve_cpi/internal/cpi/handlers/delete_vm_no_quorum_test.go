package handlers_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// noQuorumPools refuses every sentinel create the way PVE does when it cannot
// take its user.cfg lock because the cluster has no quorum.
type noQuorumPools struct {
	noopPoolService
	creates atomic.Int32
}

func (p *noQuorumPools) CreatePool(context.Context, string, string) error {
	p.creates.Add(1)
	return errors.Join(errors.New("pools.CreatePools: failed to execute POST request to \"/pools\" with context: "+
		"HTTP POST request to \"/pools\" with context failed: API request failed"),
		sdkerrors.ParseAPIError(500,
			[]byte(`{"data":null,"message":"create pool failed: cfs-lock 'file-user_cfg' error: no quorum!\n"}`)))
}

const quorumDeferral = "delete_vm: VM 101 deferred because its HA state is unknown: " +
	"the entry node does not report the cluster quorate"

// TestHandleDeleteVM_DefersWithoutQuorumBeforeAnyWrite deletes a VM on a
// cluster whose entry node reports it not quorate. The delete defers with the
// unknown-HA-state text before it takes the VMID claim, so it makes no lock
// create, no stop, and no destroy. It records the deferral, so a delete that
// reaches the VM after PVE HA restarts it lets it settle.
func TestHandleDeleteVM_DefersWithoutQuorumBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	scene.NotQuorate = true
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())
	pools := &noQuorumPools{}
	setPools(t, d, pools)

	err := d.run()
	requireHARetriable(t, err, quorumDeferral, "check 'ha-manager status'")
	if errors.Is(err, pve.ErrClusterLockStateUnknown) || strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("the deferral reads as an uncertain lock: %v", err)
	}
	if n := pools.creates.Load(); n != 0 {
		t.Fatalf("the delete made %d lock creates on a cluster without quorum", n)
	}
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a deferred delete must write nothing; events: %v", events)
	}
	d.requireDeferralHeld(t, true)
}

// TestHandleDeleteVM_LockRefusedForNoQuorumDefers covers quorum lost after
// the delete's own check. The VMID claim's create is the first write to meet
// it, and PVE refuses that create with its cfs-lock "no quorum!" answer. The
// delete defers with the same text, and it never calls the lock's outcome
// unknown.
func TestHandleDeleteVM_LockRefusedForNoQuorumDefers(t *testing.T) {
	t.Parallel()
	scene := handlers.NewHADeleteScene()
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())
	pools := &noQuorumPools{}
	setPools(t, d, pools)

	err := d.run()
	requireHARetriable(t, err, quorumDeferral)
	if strings.Contains(err.Error(), "unknown outcome") || strings.Contains(err.Error(), "cluster lock state unknown") {
		t.Fatalf("the deferral still calls the refused lock create unknown: %v", err)
	}
	if pools.creates.Load() == 0 {
		t.Fatal("the delete never tried its VMID claim")
	}
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("a deferred delete must write nothing; events: %v", events)
	}
	d.requireDeferralHeld(t, true)
}

// setPools serves pools as the pool service of d's client.
func setPools(t *testing.T, d *legacyHADelete, pools pve.PoolService) {
	t.Helper()
	wrapped, ok := d.deps.PVE.(*handlers.HAScenePermsClient)
	if !ok {
		t.Fatalf("unexpected client %T", d.deps.PVE)
	}
	inner, ok := wrapped.Client.(*mockPVEClient)
	if !ok {
		t.Fatalf("unexpected inner client %T", wrapped.Client)
	}
	inner.poolsSvc = pools
}
