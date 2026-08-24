// SDN eventual-consistency gates. A freshly-created SDN vnet is not immediately
// usable everywhere: applySDN commits the config, but the data-plane
// realization (ifupdown2 reload, pmxcfs propagation) is asynchronous and
// per-node. These bounded-poll helpers let create_network confirm a vnet has
// converged into the running cluster config (produce side) and let create_vm
// confirm a NIC's bridge is actually present on the target node before
// attaching to it (consume side), so a deploy does not boot a NIC into a bridge
// that does not yet exist. Both are opt-in (retries<=0 disables) and bounded by
// a retry count plus an absolute timeout.
package pve

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// networkResolvePollInterval is the base wait between eventual-consistency
// polls. No jitter is added: these polls are per-request and bounded by an
// explicit retry count plus timeout, so the synchronized-waiter concern that
// motivates jitter in the cluster-lock acquire loop does not apply here.
const networkResolvePollInterval = time.Second

// pollUntilResolved runs check immediately, then up to retries more times,
// sleeping networkResolvePollInterval between attempts and bounded additionally
// by timeout (measured against clk.now). It returns true as soon as check
// reports resolved, false when the retry/timeout budget is exhausted without
// resolution, or a non-nil error if check or the sleep (ctx cancel) fails.
func pollUntilResolved(
	ctx context.Context, retries int, timeout time.Duration, clk lockClock,
	check func(context.Context) (bool, error),
) (bool, error) {
	deadline := clk.now().Add(timeout)
	for attempt := 0; ; attempt++ {
		ok, err := check(ctx)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		if attempt >= retries {
			return false, nil
		}
		if !clk.now().Before(deadline) {
			return false, nil
		}
		if sleepErr := clk.sleep(ctx, networkResolvePollInterval); sleepErr != nil {
			return false, sleepErr
		}
	}
}

// WaitForSDNVnetConverged is the produce-side eventual-consistency gate for
// create_network. After applySDN commits a vnet, this polls the running
// (non-pending) cluster SDN vnet list until the named vnet appears in committed
// config or the retry/timeout budget is exhausted. On exhaustion it returns a
// TypeRetriableCloud error so the BOSH director re-drives create_network.
// retries<=0 disables the gate (returns nil immediately) so behavior is
// byte-identical when unconfigured.
//
// Scope: this confirms the vnet is committed to running cluster config, not that
// it has been realized as a usable bridge on any particular node — per-node
// ifupdown2 realization can still lag commit. The authoritative per-node check
// is the consume-side ResolveNodeBridgeOnNode, which create_vm runs against the
// node the VM actually lands on.
func WaitForSDNVnetConverged(
	ctx context.Context, c Client, vnet string, retries int, timeout time.Duration,
) error {
	return waitForSDNVnetConverged(ctx, c, vnet, retries, timeout, defaultLockClock())
}

func waitForSDNVnetConverged(
	ctx context.Context, c Client, vnet string, retries int, timeout time.Duration, clk lockClock,
) error {
	if c == nil || retries <= 0 || strings.TrimSpace(vnet) == "" {
		return nil
	}
	svc := c.Cluster()
	if svc == nil {
		return nil
	}
	resolved, err := pollUntilResolved(ctx, retries, timeout, clk, func(ctx context.Context) (bool, error) {
		return sdnVnetInRunningConfig(ctx, svc, vnet)
	})
	if err != nil {
		return err
	}
	if !resolved {
		return cpierrors.WrapAs(
			cpierrors.Cloud(
				"create_network: SDN vnet %q has not converged into running config within the retry/timeout budget",
				vnet),
			cpierrors.TypeRetriableCloud,
			"create_network: SDN convergence pending")
	}
	return nil
}

// sdnVnetInRunningConfig reports whether vnet is present in the committed
// (non-pending) cluster SDN vnet list. A list error propagates so the caller
// can decide; transient classes keep their retriable type via WrapError.
func sdnVnetInRunningConfig(ctx context.Context, svc sdkcluster.Service, vnet string) (bool, error) {
	pending := false
	resp, err := svc.ListSdnVnets(ctx, &sdkcluster.ListSdnVnetsParams{Pending: &pending})
	if err != nil {
		return false, WrapError(err)
	}
	if resp == nil {
		return false, nil
	}
	for _, raw := range *resp {
		if decodeVnetName(raw) == vnet {
			return true, nil
		}
	}
	return false, nil
}

// vnetNameCacheTTL bounds how often the per-cluster SDN vnet-membership cache
// is refreshed. Short enough that a genuinely new/renamed vnet is picked up
// within roughly one TTL window of create_vm calls; long enough that a
// plain-bridge cluster (zero vnets, ever) pays at most one ListSDNVnets call
// per TTL window rather than once per create_vm and once per unique NIC
// bridge per create_vm — the two call sites that share this cache (see
// cachedVnetNames).
const vnetNameCacheTTL = 30 * time.Second

// vnetNameCache is a process-wide, TTL-bounded, mutex-protected cache of "is
// this bridge name a known SDN vnet" membership, keyed by the pve.Client
// instance so a multi-cluster CPI process (per-request pve_* overrides —
// context_override.go's bounded-LRU client bundle cache) never mixes vnet
// membership across distinct clusters: each override identity gets its own
// Client, and the concrete type backing that interface (a pointer) is
// comparable, so it is safe and correct as a map key.
//
// Two call sites share this cache: cachedVnetNames' callers in the handlers
// package (mtu=1 vnet-MTU inheritance and vlan/tag membership, decoupled
// from network_mode) and bridgeIsSDNVnet in this file (the
// consume-side eventual-consistency gate). Both need the identical answer to
// "is this bridge an SDN vnet"; sharing one cache means a plain-bridge
// cluster pays the ListSDNVnets cost once per TTL window rather than once
// per call site per create_vm.
var vnetNameCache = newVnetCache()

type vnetCache struct {
	mu       sync.Mutex
	entries  map[Client]vnetCacheEntry
	negative map[Client]vnetNegativeCacheEntry
}

type vnetCacheEntry struct {
	names map[string]struct{}
	exp   time.Time
}

// vnetNegativeCacheEntry holds the most recent ListSDNVnets failure for a
// given Client, with an expiry. Mirrors StorageInfoCache's negCache/negTTL
// pattern (see internal/pve/storage_info.go): a failed refresh is replayed
// from here for vnetNameNegativeCacheTTL rather than re-attempted on every
// call, so a cluster where the listing always errors (SDN not configured,
// insufficient permissions) does not pay a failed API round trip on every
// single create_vm forever.
type vnetNegativeCacheEntry struct {
	err error
	exp time.Time
}

// vnetNameNegativeCacheTTL bounds how long a failed SDN vnet listing is
// replayed from the negative cache before the next attempt is allowed.
// Shorter than vnetNameCacheTTL's positive-result rationale does not apply
// symmetrically here: a stuck failure is worse to pin for a long window than
// a stale success, so this stays on the same order of magnitude as
// StorageInfoCache's negativeCacheTTL rather than matching the (longer)
// positive TTL above.
const vnetNameNegativeCacheTTL = 30 * time.Second

func newVnetCache() *vnetCache {
	return &vnetCache{
		entries:  make(map[Client]vnetCacheEntry),
		negative: make(map[Client]vnetNegativeCacheEntry),
	}
}

// CachedVnetNames returns the set of SDN vnet names currently defined
// (pending included) for c, from the process-wide cache when fresh or via a
// live ListSDNVnets otherwise. This is the exported entry point used by
// create_vm's NIC-attachment path (mtu=1 inheritance, vlan/tag membership);
// bridgeIsSDNVnet below uses the unexported cachedVnetNames directly.
//
// A listing failure returns (nil, err) and is cached in the per-Client
// negative cache for vnetNameNegativeCacheTTL, so a cluster where the
// listing always errors (SDN not configured, insufficient permissions) is
// replayed from the negative cache rather than re-attempted on every call —
// the sdnVnetNameSet call in create_vm_network.go runs on every single
// create_vm regardless of network_mode, so an uncached failure would mean a
// failed API round trip on every create_vm forever. Callers must treat a
// non-nil error as "membership unknown, not a vnet" rather than propagating
// it — see sdnVnetNameSet for the reference caller.
func CachedVnetNames(ctx context.Context, c Client) (map[string]struct{}, error) {
	return cachedVnetNames(ctx, c)
}

func cachedVnetNames(ctx context.Context, c Client) (map[string]struct{}, error) {
	if c == nil {
		return nil, cpierrors.Cloud("CachedVnetNames: client must not be nil")
	}
	now := time.Now()

	vnetNameCache.mu.Lock()
	if entry, ok := vnetNameCache.entries[c]; ok && now.Before(entry.exp) {
		names := entry.names
		vnetNameCache.mu.Unlock()
		return names, nil
	}
	if neg, ok := vnetNameCache.negative[c]; ok && now.Before(neg.exp) {
		cachedErr := neg.err
		vnetNameCache.mu.Unlock()
		return nil, cachedErr
	}
	vnetNameCache.mu.Unlock()

	vnets, err := ListSDNVnets(ctx, c)
	if err != nil {
		vnetNameCache.mu.Lock()
		vnetNameCache.negative[c] = vnetNegativeCacheEntry{err: err, exp: now.Add(vnetNameNegativeCacheTTL)}
		vnetNameCache.mu.Unlock()
		return nil, err
	}
	set := make(map[string]struct{}, len(vnets))
	for _, v := range vnets {
		set[v.Vnet] = struct{}{}
	}

	vnetNameCache.mu.Lock()
	// A fresh success clears any prior negative-cache entry for this Client so
	// a subsequent failure starts a new TTL window rather than reusing a stale
	// expiry from before the cluster recovered.
	delete(vnetNameCache.negative, c)
	// Opportunistic sweep of expired entries on every write: bounds the map's
	// long-run size to the number of DISTINCT Client instances that have
	// actually made a request recently, rather than every Client instance
	// ever constructed over a long-running process's lifetime (e.g. every
	// per-request override bundle context_override.go's own LRU has since
	// evicted). Cheap — proportional to the (small, cluster-count-bounded)
	// map size, run only when this goroutine already holds the lock to
	// write a new entry.
	for k, e := range vnetNameCache.entries {
		if !now.Before(e.exp) {
			delete(vnetNameCache.entries, k)
		}
	}
	for k, e := range vnetNameCache.negative {
		if !now.Before(e.exp) {
			delete(vnetNameCache.negative, k)
		}
	}
	vnetNameCache.entries[c] = vnetCacheEntry{names: set, exp: now.Add(vnetNameCacheTTL)}
	vnetNameCache.mu.Unlock()

	return set, nil
}

// ResolveNodeBridgeOnNode is the consume-side eventual-consistency gate for
// create_vm. Before writing a NIC's bridge into the VM config, it confirms the
// bridge is realized on the target node. It only gates SDN-managed vnets: a
// bridge that does not name a known SDN vnet (an external/static Linux bridge
// such as vmbr0) passes straight through (returns nil) so legitimately external
// bridges are never blocked. For an SDN vnet it polls nodes.ListNetwork(node)
// for an interface whose iface matches bridge until the bridge appears or the
// retry/timeout budget is exhausted; on exhaustion it returns a
// TypeRetriableCloud error so create_vm re-drives rather than attaching a NIC to
// a bridge that does not yet exist on the node. retries<=0 disables the gate
// (returns nil) so behavior is byte-identical when unconfigured. The gate is
// best-effort: if SDN-membership cannot be determined it fails open.
func ResolveNodeBridgeOnNode(
	ctx context.Context, c Client, node, bridge string, retries int, timeout time.Duration,
) error {
	return resolveNodeBridgeOnNode(ctx, c, node, bridge, retries, timeout, defaultLockClock())
}

func resolveNodeBridgeOnNode(
	ctx context.Context, c Client, node, bridge string, retries int, timeout time.Duration, clk lockClock,
) error {
	if c == nil || retries <= 0 || strings.TrimSpace(node) == "" || strings.TrimSpace(bridge) == "" {
		return nil
	}
	// Only gate SDN-managed vnets. A bridge that is not an SDN vnet is external/
	// static and must pass straight through. If membership cannot be determined,
	// fail open rather than block the deploy on the guard's own lookup blip.
	managed, err := bridgeIsSDNVnet(ctx, c, bridge)
	if err != nil || !managed {
		return nil
	}
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return nil
	}
	resolved, err := pollUntilResolved(ctx, retries, timeout, clk, func(ctx context.Context) (bool, error) {
		return nodeHasBridge(ctx, nodesSvc, node, bridge), nil
	})
	if err != nil {
		return err
	}
	if !resolved {
		return cpierrors.WrapAs(
			cpierrors.Cloud(
				"create_vm: SDN bridge %q is not yet present on node %q within the retry/timeout budget", bridge, node),
			cpierrors.TypeRetriableCloud,
			"create_vm: SDN bridge not yet realized on node")
	}
	return nil
}

// bridgeIsSDNVnet reports whether bridge names a known SDN vnet, via the
// shared process-wide vnetNameCache (see cachedVnetNames) — the same cache
// create_vm's mtu=1/vlan-tag membership check uses, so a plain-bridge cluster
// pays the ListSDNVnets cost at most once per TTL window rather than once per
// call site. The underlying list includes pending state, so a vnet created
// moments earlier (and still realizing on nodes) is correctly recognized as
// SDN-managed.
func bridgeIsSDNVnet(ctx context.Context, c Client, bridge string) (bool, error) {
	if c.Cluster() == nil {
		return false, nil
	}
	names, err := cachedVnetNames(ctx, c)
	if err != nil {
		return false, err
	}
	_, ok := names[bridge]
	return ok, nil
}

// nodeHasBridge reports whether bridge appears as an interface on node. A
// node-network read failure is treated as "not yet present" (returns false, not
// an error) so the bounded poll keeps retrying through a transient blip in the
// very convergence window it is waiting on, rather than aborting the deploy.
func nodeHasBridge(ctx context.Context, nodesSvc sdknodes.Service, node, bridge string) bool {
	// PVE 9.2's plain GET /nodes/<node>/network lists only
	// /etc/network/interfaces entries — realized SDN vnet bridges (which live
	// in interfaces.d/sdn) appear ONLY under type=any_bridge. Without the
	// filter a live bridge is never observed and the gate always exhausts its
	// budget. Releases that reject the filter value fall back to the plain
	// listing, which on those releases includes SDN vnets.
	anyBridge := "any_bridge"
	resp, err := nodesSvc.ListNetwork(ctx, node, &sdknodes.ListNetworkParams{Type: &anyBridge})
	if err != nil || resp == nil {
		resp, err = nodesSvc.ListNetwork(ctx, node, nil)
	}
	if err != nil || resp == nil {
		return false
	}
	for _, raw := range *resp {
		if decodeIfaceName(raw) == bridge {
			return true
		}
	}
	return false
}

// decodeVnetName extracts the "vnet" field from a raw SDN vnet row; "" on any
// decode failure.
func decodeVnetName(raw json.RawMessage) string {
	var row struct {
		Vnet string `json:"vnet"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return ""
	}
	return row.Vnet
}

// decodeIfaceName extracts the "iface" field from a raw node-network row; "" on
// any decode failure.
func decodeIfaceName(raw json.RawMessage) string {
	var row struct {
		Iface string `json:"iface"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return ""
	}
	return row.Iface
}
