// delete_network handler: implements the BOSH CPI v2 delete_network method.
// Probes the SDN backend first to determine whether network_cid refers to a
// PVE SDN vnet; if so tears down subnets, the vnet, and (conditionally) the
// parent zone. If the SDN probe reports the entity absent, falls back to
// deleting the Linux bridge of the same name via the nodes API.
//
// Idempotency: every NotFound response is treated as success. Concurrent
// deletes targeting the same network_cid both return nil. All cleanup I/O
// runs on a context derived via contextWithoutCancel so that a cancelled
// parent context does not leave half-deleted SDN state pending an apply.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// HandleDeleteNetwork returns a Handler that implements the BOSH delete_network method.
//
// Arguments:
//
//	[0] network_cid — string: the vnet name or bridge iface returned by create_network.
//
// Returns null (nil). Idempotent: absent resources are not errors.
//
// Routing: probes pve.GetSDNVnet(network_cid). Found → SDN delete path. When
// errors.Is(err, pve.ErrSDNNotFound) → bridge fallback. Any other probe error
// is wrapped and returned to the caller.
//
// Zone auto-delete: only when ALL of the following hold:
//  1. config.SDNAutoManageZone is enabled
//  2. zone != config.SDNZone (the pinned zone is never deleted by the CPI)
//  3. the zone is not an EVPN zone (operator-owned fabric, never CPI-deleted)
//  4. ListSDNVnets filtered by zone returns 0 remaining vnets
func HandleDeleteNetwork(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		return nil, deleteNetwork(ctx, deps, args)
	})
}

func deleteNetwork(ctx context.Context, deps Deps, args []json.RawMessage) error {
	if deps.PVE == nil {
		return cpierrors.Cloud("delete_network: PVE client is not configured")
	}
	if deps.Config == nil {
		return cpierrors.Cloud("delete_network: CPI config is not configured")
	}
	if len(args) < 1 {
		return cpierrors.Cloud("delete_network: missing required argument network_cid")
	}

	var networkCID string
	if err := json.Unmarshal(args[0], &networkCID); err != nil {
		return cpierrors.Cloud("delete_network: network_cid must be a JSON string: %s", err.Error())
	}
	networkCID = strings.TrimSpace(networkCID)
	if networkCID == "" {
		return cpierrors.Cloud("delete_network: network_cid must not be empty")
	}

	// Run all PVE I/O on a context that survives parent cancellation. The CPI
	// surface may invoke this handler from a request whose ctx is cancelled
	// once the JSON-RPC response is written; deletes must run to completion
	// to avoid leaving half-applied SDN state.
	opCtx := contextWithoutCancel(ctx)

	// Probe SDN to decide which path to take.
	vnet, probeErr := pve.GetSDNVnet(opCtx, deps.PVE, networkCID)
	switch {
	case probeErr == nil:
		zone := ""
		if vnet != nil {
			zone = vnet.Zone
		}
		return deleteNetworkSDN(opCtx, deps, networkCID, zone)
	case errors.Is(probeErr, pve.ErrSDNNotFound):
		// SDN reports the vnet absent. Try the bridge path; if no bridge
		// exists either, that path also returns nil for idempotency.
		return deleteNetworkBridge(opCtx, deps, networkCID)
	default:
		return cpierrors.Wrap(pve.WrapError(probeErr),
			fmt.Sprintf("delete_network: probe SDN vnet %q", networkCID))
	}
}

// deleteNetworkSDN tears down an SDN vnet and its subnets, applies the SDN
// configuration, then conditionally removes the parent zone subject to the
// rules documented on HandleDeleteNetwork.
//
// Every NotFound response is swallowed so the function is idempotent across
// concurrent or repeated invocations against the same vnet.
func deleteNetworkSDN(ctx context.Context, deps Deps, vnet, zone string) error {
	// 1. Subnets must be removed before the vnet itself; PVE rejects vnet
	//    delete while subnets remain.
	subnets, err := pve.ListSDNVnetSubnets(ctx, deps.PVE, vnet)
	if err != nil && !errors.Is(err, pve.ErrSDNNotFound) {
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("delete_network: list subnets for vnet %q", vnet))
	}
	for _, s := range subnets {
		if s.Subnet == "" {
			continue
		}
		if dErr := pve.DeleteSDNVnetSubnet(ctx, deps.PVE, vnet, s.Subnet); dErr != nil {
			if errors.Is(dErr, pve.ErrSDNNotFound) {
				continue
			}
			return cpierrors.Wrap(pve.WrapError(dErr),
				fmt.Sprintf("delete_network: delete subnet %q from vnet %q", s.Subnet, vnet))
		}
	}

	// 2. Delete the vnet. 404 → already gone, treat as success.
	if vErr := pve.DeleteSDNVnet(ctx, deps.PVE, vnet); vErr != nil {
		if !errors.Is(vErr, pve.ErrSDNNotFound) {
			return cpierrors.Wrap(pve.WrapError(vErr),
				fmt.Sprintf("delete_network: delete SDN vnet %q", vnet))
		}
	}

	// 3. Commit the deletions so subsequent SDN reads observe the new state.
	//    Routed through applySDN (not pve.ApplySDN) so async zone types
	//    (vlan/vxlan/evpn/qinq) that return a UPID are awaited to completion.
	//    Otherwise a follow-on create_network reusing this vnet name could
	//    race the pending delete-apply.
	clusterSvc := deps.PVE.Cluster()
	if aErr := applySDN(ctx, deps, clusterSvc,
		fmt.Sprintf("delete_network: apply SDN after deleting vnet %q", vnet)); aErr != nil {
		return aErr
	}

	// 4. Conditional zone teardown. Four guards must all hold; any failure
	//    leaves the zone in place rather than risk deleting a zone that is
	//    still in use by another vnet or pinned by configuration.
	if err := maybeDeleteOrphanedZone(ctx, deps, zone); err != nil {
		return err
	}
	return nil
}

// maybeDeleteOrphanedZone deletes zone iff:
//  1. zone is non-empty,
//  2. config.SDNAutoManageZone is enabled,
//  3. zone is not the operator-pinned config.SDNZone,
//  4. zone is not an EVPN zone (EVPN fabric — controller, BGP peers — is
//     operator infrastructure the CPI never creates, so it never deletes), and
//  5. no remaining vnets reference zone.
//
// Any failure to enumerate vnets or to read the zone type is non-fatal: the
// function returns nil without deleting, preserving the invariant "never
// delete a zone the CPI cannot confirm is empty and CPI-deletable". The
// turnkey zone ("bosh") is deliberately NOT pinned — the CPI created it, so
// removing it when its last vnet goes is correct turnkey hygiene.
func maybeDeleteOrphanedZone(ctx context.Context, deps Deps, zone string) error {
	cfg := deps.Config
	if zone == "" || !cfg.SDNAutoManageZoneEnabled() {
		return nil
	}
	if cfg.SDNZone != "" && strings.EqualFold(zone, cfg.SDNZone) {
		return nil
	}

	zoneInfo, zoneErr := pve.GetSDNZone(ctx, deps.PVE, zone)
	switch {
	case errors.Is(zoneErr, pve.ErrSDNNotFound):
		// Zone already gone — nothing to tear down.
		return nil
	case zoneErr != nil:
		// Cannot confirm the zone type — fail closed against deletion.
		deps.Log(ctx).Warn("delete_network: zone teardown skipped — could not read zone type",
			log.String("zone", zone),
			log.Err(zoneErr))
		return nil
	case zoneInfo != nil && strings.EqualFold(zoneInfo.Type, "evpn"):
		deps.Log(ctx).Debug("delete_network: zone retained — EVPN zones are operator-owned",
			log.String("zone", zone))
		return nil
	}

	allVnets, listErr := pve.ListSDNVnets(ctx, deps.PVE)
	if listErr != nil {
		// Cannot confirm zone is empty — do not delete. This is the
		// safe default; the operator can clean up manually if needed.
		deps.Log(ctx).Warn("delete_network: zone teardown skipped — could not list vnets",
			log.String("zone", zone),
			log.Err(listErr))
		return nil
	}
	for _, v := range allVnets {
		if strings.EqualFold(v.Zone, zone) {
			// Another vnet still references this zone; keep it.
			return nil
		}
	}

	if dErr := pve.DeleteSDNZone(ctx, deps.PVE, zone); dErr != nil {
		if !errors.Is(dErr, pve.ErrSDNNotFound) {
			return cpierrors.Wrap(pve.WrapError(dErr),
				fmt.Sprintf("delete_network: delete SDN zone %q", zone))
		}
		// 404 → already gone, fall through to apply for consistency.
	}
	clusterSvc := deps.PVE.Cluster()
	if aErr := applySDN(ctx, deps, clusterSvc,
		fmt.Sprintf("delete_network: apply SDN after deleting zone %q", zone)); aErr != nil {
		return aErr
	}
	return nil
}

// deleteNetworkBridge removes a Linux bridge via the nodes API. Used when the
// SDN probe reports no vnet of the given name.
//
// The handler tracks no per-bridge node mapping between create and delete, so
// the target node is taken from config.Node. A missing config.Node forces an
// explicit error rather than a silent success — the bridge may still exist on
// some node, and BOSH would otherwise believe the network has been removed.
//
// 404 (bridge absent on the target node) is treated as success for idempotency.
func deleteNetworkBridge(ctx context.Context, deps Deps, iface string) error {
	node := strings.TrimSpace(deps.Config.Node)
	if node == "" {
		return cpierrors.Cloud(
			"delete_network: config.node is required for bridge delete path (network_cid %q)", iface,
		)
	}

	nodesSvc := deps.PVE.Nodes()
	if nodesSvc == nil {
		return cpierrors.Cloud("delete_network: nodes service unavailable")
	}

	if err := nodesSvc.DeleteNetwork2(ctx, node, iface); err != nil {
		if pve.IsNotFound(err) {
			// Bridge already gone — idempotent success.
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("delete_network: delete bridge %q on node %q", iface, node))
	}

	// Reload the node's network config so the bridge removal takes effect.
	if _, err := nodesSvc.UpdateNetwork(ctx, node, nil); err != nil {
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("delete_network: reload network on node %q", node))
	}
	return nil
}
