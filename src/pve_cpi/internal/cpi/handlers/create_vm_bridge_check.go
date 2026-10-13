// create_vm_bridge_check.go holds the per-node plain-bridge check. One
// collector turns a create_vm network spec into the bridges its NICs attach to.
// The SDN gate in configureNICs, the storage-plan node ranking, the live
// placement filter, and the pre-clone guard all read that collector, so they
// cannot disagree about which bridges a VM needs.
//
// The check covers plain Linux bridges only. A bridge that names an SDN vnet,
// or whose SDN membership cannot be determined, is skipped here and left to
// pve.ResolveNodeBridgeOnNode, which polls because a new vnet may not exist on
// every node yet. placement.bridge_check chooses between off, warn, and
// enforce for the plain-path filter and the guard. The storage-plan path
// enforces at every value.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// nicBridgeRef ties one bridge to the network (NIC) that needs it, so a warning
// or an error can name the NIC.
type nicBridgeRef struct {
	bridge  string
	network string
	vip     bool
}

// collectNICBridgeRefs resolves the bridge of every network in the NIC plan with
// the precedence configureNICs uses (network_defaults, then the network's own
// cloud properties, then defaultBridge), including every member of a nic_group.
// The result is sorted by bridge then network name, so it is deterministic.
func collectNICBridgeRefs(parsed *createVMParsedArgs, defaultBridge string) ([]nicBridgeRef, error) {
	var refs []nicBridgeRef
	for _, entry := range planNICs(parsed.networks) {
		for _, name := range entry.names {
			spec := parsed.networks[name]
			bridge, _, err := resolveNICBridgeAndVLAN(parsed.cloudProps.NetworkDefaults, spec.CloudProperties, defaultBridge, name)
			if err != nil {
				return nil, err
			}
			if bridge == "" {
				continue
			}
			refs = append(refs, nicBridgeRef{bridge: bridge, network: name, vip: strings.EqualFold(spec.Type, nicTypeVIP)})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].bridge != refs[j].bridge {
			return refs[i].bridge < refs[j].bridge
		}
		return refs[i].network < refs[j].network
	})
	return refs, nil
}

// collectNICBridges returns the distinct, sorted bridges the VM's NICs attach
// to. It is the single implementation behind configureNICs' SDN gate, the
// storage-plan node ranking, and the plain-path bridge check.
func collectNICBridges(parsed *createVMParsedArgs, defaultBridge string) ([]string, error) {
	refs, err := collectNICBridgeRefs(parsed, defaultBridge)
	if err != nil {
		return nil, err
	}
	return distinctBridges(refs), nil
}

func distinctBridges(refs []nicBridgeRef) []string {
	var out []string
	for _, ref := range refs {
		if len(out) == 0 || out[len(out)-1] != ref.bridge {
			out = append(out, ref.bridge)
		}
	}
	return out
}

// plainBridgeRefs drops VIP networks and every ref whose bridge is an SDN vnet
// or whose SDN membership cannot be determined. A skipped bridge stays with the
// existing SDN gate, which tolerates a vnet that is still converging.
func plainBridgeRefs(ctx context.Context, deps Deps, parsed *createVMParsedArgs, refs []nicBridgeRef) []nicBridgeRef {
	if deps.PVE == nil {
		return nil
	}
	skip := make(map[string]bool)
	var plain []nicBridgeRef
	for _, ref := range refs {
		if ref.vip {
			continue
		}
		drop, seen := skip[ref.bridge]
		if !seen {
			isVnet, err := pve.IsSDNVnet(ctx, deps.PVE, ref.bridge)
			if err != nil {
				// The skip is mandated, since a vnet may still be converging, but
				// it must not be silent. Without SDN read access the plain-bridge
				// check does nothing for this bridge.
				// Once per create_vm call, however many call sites reach here.
				if parsed.sdnSkipWarned == nil {
					parsed.sdnSkipWarned = make(map[string]bool)
				}
				if !parsed.sdnSkipWarned[ref.bridge] {
					parsed.sdnSkipWarned[ref.bridge] = true
					deps.Log(ctx).Warn("bridge check: SDN membership could not be read; bridge left to the SDN gate",
						log.String("bridge", ref.bridge), log.String("nic", ref.network), log.Err(err))
				}
			}
			drop = err != nil || isVnet
			skip[ref.bridge] = drop
		}
		if !drop {
			plain = append(plain, ref)
		}
	}
	return plain
}

// plainCheckRefs returns the plain-bridge refs a create_vm needs checked, or
// nil when the VM has none or the NIC settings cannot be resolved. A resolution
// error is left for configureNICs to report, because it reports the same error.
func plainCheckRefs(ctx context.Context, deps Deps, parsed *createVMParsedArgs) []nicBridgeRef {
	if parsed == nil || deps.Config == nil {
		return nil
	}
	defaultBridge, _, err := resolveVMNICDefaultsWithError(deps.Config, parsed.cloudProps, parsed.cloudPropsMap)
	if err != nil {
		return nil
	}
	refs, err := collectNICBridgeRefs(parsed, defaultBridge)
	if err != nil {
		return nil
	}
	return plainBridgeRefs(ctx, deps, parsed, refs)
}

// bridgePlan is what the live placement filter needs from bridge_check. At
// enforce the filter rejects a node that lacks a required bridge. At warn the
// filter rejects nothing and logs, per surviving candidate node, what enforce
// would have rejected. The zero value means off, or a path with no plain
// bridges, and the filter does nothing.
type bridgePlan struct {
	required []string
	checker  func(node, bridge string) (bool, error)
	warnOnly bool
}

// placementBridgePlan builds the plan for one create_vm call. The checker is
// the call's shared one, so the pre-clone guard reuses every listing the filter
// made.
func placementBridgePlan(ctx context.Context, deps Deps, parsed *createVMParsedArgs) bridgePlan {
	if deps.Config == nil || deps.PVE == nil || parsed.storagePlan != nil {
		return bridgePlan{}
	}
	mode := deps.Config.BridgeCheckMode()
	if mode == config.BridgeCheckOff {
		return bridgePlan{}
	}
	required := distinctBridges(plainCheckRefs(ctx, deps, parsed))
	if len(required) == 0 {
		return bridgePlan{}
	}
	return bridgePlan{
		required: required,
		checker:  parsed.sharedBridgeChecker(ctx, deps.PVE).check,
		warnOnly: mode == config.BridgeCheckWarn,
	}
}

// apply sets the filter fields on req. At warn it leaves req alone, so nothing
// is rejected.
func (b bridgePlan) apply(req *placement.Request) {
	if b.warnOnly || len(b.required) == 0 {
		return
	}
	req.RequiredBridges = b.required
	req.BridgeChecker = b.checker
}

// warnGaps logs a Warn for every surviving node that lacks a required bridge,
// or whose network list could not be read, when the plan is log-only. It
// rejects nothing.
func (b bridgePlan) warnGaps(logger *log.Logger, pass []placement.NodeFacts, az string) {
	if !b.warnOnly || len(b.required) == 0 {
		return
	}
	for _, f := range pass {
		for _, bridge := range b.required {
			present, err := b.checker(f.Node, bridge)
			switch {
			case err != nil:
				logger.Warn("create_vm: bridge check: network lookup failed; enforce would reject this node as a transient failure, warn keeps it",
					log.String("node", f.Node), log.String("bridge", bridge), log.String("az", az), log.Err(err))
			case !present:
				logger.Warn("create_vm: bridge check: bridge is missing; enforce would reject this node, warn keeps it",
					log.String("node", f.Node), log.String("bridge", bridge), log.String("az", az))
			}
		}
	}
}

// bridgeCheckListTimeout is the one time budget for every network listing the
// bridge check makes in a create_vm call. It starts at the first listing, so a
// stalled cluster costs the call at most this long in total, not this long per
// candidate node, and a node that accepts the connection and then stalls cannot
// hold the call for the PVE client's long default timeout.
const bridgeCheckListTimeout = 20 * time.Second

// sharedBridgeChecker returns the call's bridge checker, creating it on first
// use. The placement filter and the pre-clone guard both use it, so each node's
// network list is read once per create_vm.
func (p *createVMParsedArgs) sharedBridgeChecker(ctx context.Context, c pve.Client) *nodeBridgeChecker {
	if p.nodeBridgeCheck == nil {
		p.nodeBridgeCheck = newNodeBridgeCheckerTimeout(ctx, c, bridgeCheckListTimeout)
	}
	return p.nodeBridgeCheck
}

// nodeBridgeChecker reads each node's interface list once per create_vm call
// and answers every bridge from that read. A failed read is remembered and
// returned as an error each time, so a lookup failure never turns into a
// proven absence.
//
// The placement filter pass (check) shares one time budget of limit, counted
// from the first read, so a stalled cluster costs the call limit in total.
// The pre-clone guard (checkFresh) lists a node that is not cached with its own
// fresh limit, because the guard can run long after the filter pass, and an
// expired shared budget would refuse a healthy node. Both write to the same
// cache, so a node is listed at most once per call, with one exception. When
// the shared budget runs out, check caches a deadline error for every node it
// reaches afterwards, and some of those nodes were never listed. Such an entry
// is flagged, and checkFresh lists the node once more under a fresh budget and
// caches the new result unflagged. Any other cached error, such as an API
// failure or a cancelled request, is reused as it is.
type nodeBridgeChecker struct {
	ctx      context.Context
	client   pve.Client
	limit    time.Duration
	mu       sync.Mutex
	cache    map[string]nodeListing
	deadline time.Time
}

type nodeListing struct {
	names map[string]struct{}
	err   error
	// budgetExpired marks an error the filter pass's shared budget caused, so
	// the node may never have been listed at all. checkFresh lists such a node
	// again, once, under a fresh budget.
	budgetExpired bool
}

// newNodeBridgeChecker returns a checker for the placement filter and guard.
func newNodeBridgeChecker(ctx context.Context, c pve.Client) *nodeBridgeChecker {
	return newNodeBridgeCheckerTimeout(ctx, c, bridgeCheckListTimeout)
}

// newNodeBridgeCheckerTimeout is newNodeBridgeChecker with an explicit bound.
// A listing that runs out of time is a failed read, which callers treat as a
// lookup failure and never as a proven absence.
func newNodeBridgeCheckerTimeout(ctx context.Context, c pve.Client, limit time.Duration) *nodeBridgeChecker {
	return &nodeBridgeChecker{ctx: ctx, client: c, limit: limit, cache: make(map[string]nodeListing)}
}

// check answers for the placement filter pass, under the shared budget.
func (n *nodeBridgeChecker) check(node, bridge string) (bool, error) {
	return n.answer(node, bridge, false)
}

// checkFresh answers for the pre-clone guard. A cached node reuses its listing.
// An uncached node is listed under a fresh budget of its own.
func (n *nodeBridgeChecker) checkFresh(node, bridge string) (bool, error) {
	return n.answer(node, bridge, true)
}

func (n *nodeBridgeChecker) answer(node, bridge string, fresh bool) (bool, error) {
	n.mu.Lock()
	l, ok := n.cache[node]
	if !ok || (fresh && l.budgetExpired) {
		var deadline time.Time
		if fresh {
			deadline = time.Now().Add(n.limit)
		} else {
			if n.deadline.IsZero() {
				n.deadline = time.Now().Add(n.limit)
			}
			deadline = n.deadline
		}
		lctx, cancel := context.WithDeadline(n.ctx, deadline)
		l = nodeListing{}
		l.names, l.err = pve.NodeBridgeNames(lctx, n.client, node)
		cancel()
		l.budgetExpired = !fresh && errors.Is(l.err, context.DeadlineExceeded) && n.ctx.Err() == nil
		n.cache[node] = l
	}
	n.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	_, present := l.names[bridge]
	return present, nil
}

// verifyBridgesOnNode is the guard that runs just before the clone. It covers
// every node the filter did not vet: an operator target_node pin, a local-disk
// pin, placement disabled, and each alternate in the fallback loop.
//
//   - off: no check and no cluster call.
//   - warn (the default): a missing bridge, or a failed lookup, logs a Warn that
//     names the node, the bridge, and the NIC, and the create continues.
//   - enforce: proven absence fails with a non-retriable error, and a failed
//     lookup fails with a retriable one.
//
// The guard reads through the call's shared checker, so a node the placement
// filter already listed is not listed again.
//
// The storage-plan path is skipped, because ObserveStoragePlanCompute already
// gates its node on the same network check.
func verifyBridgesOnNode(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, node string) error {
	if deps.Config == nil || deps.PVE == nil || parsed.storagePlan != nil {
		return nil
	}
	mode := deps.Config.BridgeCheckMode()
	if mode == config.BridgeCheckOff {
		return nil
	}
	refs := plainCheckRefs(ctx, deps, parsed)
	if len(refs) == 0 {
		return nil
	}
	check := parsed.sharedBridgeChecker(ctx, deps.PVE).checkFresh
	az := parsed.cloudProps.AvailabilityZone
	for _, ref := range refs {
		present, err := check(node, ref.bridge)
		switch {
		case err != nil && mode == config.BridgeCheckEnforce:
			return cpierrors.Retriable(
				"create_vm: cannot confirm bridge %q for network %q on node %q: %s", ref.bridge, ref.network, node, err.Error())
		case err != nil:
			logger.Warn("create_vm: bridge check: network lookup failed; continuing because bridge_check is warn",
				log.String("node", node), log.String("bridge", ref.bridge), log.String("nic", ref.network), log.Err(err))
		case !present && mode == config.BridgeCheckEnforce:
			return cpierrors.Cloud("create_vm: bridge %q for network %q does not exist on node %q%s; "+
				"create the bridge on every node of the AZ, or use an SDN vnet", ref.bridge, ref.network, node, azSuffix(az))
		case !present:
			logger.Warn("create_vm: bridge check: bridge is missing on the chosen node; continuing because bridge_check is warn",
				log.String("node", node), log.String("bridge", ref.bridge), log.String("nic", ref.network))
		}
	}
	return nil
}

func azSuffix(az string) string {
	if az == "" {
		return ""
	}
	return fmt.Sprintf(" (availability_zone %q)", az)
}
