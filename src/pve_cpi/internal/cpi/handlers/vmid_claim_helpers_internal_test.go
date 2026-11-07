package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// heldVMIDClaimContext returns ctx carrying a claim set that already holds
// vmid with an open window, the way a create's or delete's context does once
// its claim is taken. withVMIDClaim and withVMIDLock reuse the held claim
// instead of acquiring the lock, and nothing releases it, so a test can drive
// the work under a claim without the pool calls that take one.
func heldVMIDClaimContext(ctx context.Context, vmid int) context.Context {
	return vmidClaimContextWithWindow(ctx, vmid, time.Now().Add(time.Hour))
}

// vmidClaimContextWithWindow is heldVMIDClaimContext with the window ending at
// windowEnd, so a test can hold a claim whose window has already closed.
func vmidClaimContextWithWindow(ctx context.Context, vmid int, windowEnd time.Time) context.Context {
	ctx, set := withVMIDClaimSet(ctx)
	set.mu.Lock()
	defer set.mu.Unlock()
	set.held[vmid] = &vmidClaimEntry{windowEnd: windowEnd}
	return ctx
}

// exclusiveLockPools is a pool service whose create refuses a pool that
// already exists, the way PVE does, so two claims on one VMID exclude each
// other the way they do against a cluster.
type exclusiveLockPools struct {
	mu      sync.Mutex
	pools   map[string]string
	creates int
}

func newExclusiveLockPools() *exclusiveLockPools {
	return &exclusiveLockPools{pools: map[string]string{}}
}

func (p *exclusiveLockPools) AddVM(context.Context, string, int64) error        { return nil }
func (p *exclusiveLockPools) MoveVMToPool(context.Context, string, int64) error { return nil }
func (p *exclusiveLockPools) PoolHasVM(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (p *exclusiveLockPools) CreatePool(_ context.Context, poolID, comment string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	if _, ok := p.pools[poolID]; ok {
		return fmt.Errorf("pool '%s' already exists", poolID)
	}
	p.pools[poolID] = comment
	return nil
}

func (p *exclusiveLockPools) DeletePool(_ context.Context, poolID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pools[poolID]; !ok {
		return fmt.Errorf("pool '%s' does not exist", poolID)
	}
	delete(p.pools, poolID)
	return nil
}

func (p *exclusiveLockPools) GetPoolComment(_ context.Context, poolID string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	comment, ok := p.pools[poolID]
	return comment, ok, nil
}

// hold records a claim on vmid by owner that expires at exp, as another
// process's acquire would.
func (p *exclusiveLockPools) hold(vmid int, owner string, exp time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pools[vmidLockPool(vmid)] = "owner=" + owner + " exp=" + strconv.FormatInt(exp.Unix(), 10)
}

// held reports whether any process claims vmid.
func (p *exclusiveLockPools) held(vmid int) bool {
	_, ok := p.comment(vmid)
	return ok
}

// comment returns the claim recorded on vmid.
func (p *exclusiveLockPools) comment(vmid int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	comment, ok := p.pools[vmidLockPool(vmid)]
	return comment, ok
}

// expiry returns the expiry the claim on vmid records.
func (p *exclusiveLockPools) expiry(vmid int) (time.Time, bool) {
	comment, ok := p.comment(vmid)
	if !ok {
		return time.Time{}, false
	}
	for field := range strings.FieldsSeq(comment) {
		if rest, found := strings.CutPrefix(field, "exp="); found {
			secs, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}

func vmidLockPool(vmid int) string {
	return pve.ClusterLockPoolName(fmt.Sprintf("vm-%d", vmid))
}

// claimClient is the lock-recovery client with a pool service, so the work
// under test takes real VMID claims.
type claimClient struct {
	*lrClient
	pools pve.PoolService
}

func (c claimClient) Pools() pve.PoolService { return c.pools }
