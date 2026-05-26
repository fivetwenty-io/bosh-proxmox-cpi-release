// Package handlers — delete_network handler.
//
// Implements the BOSH CPI v2 delete_network method. The handler determines
// whether the network_cid refers to an SDN vnet (by probing GetSdnVnets) or a
// Linux bridge, then tears it down accordingly. Both paths are fully idempotent:
// absent resources return nil.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// HandleDeleteNetwork returns a Handler that implements the BOSH delete_network method.
//
// Arguments:
//
//	[0] network_cid — string: the vnet name or bridge iface returned by create_network.
//
// Returns null (nil). Idempotent: absent resources are not errors.
//
// Routing: probes GetSdnVnets(network_cid). Found → SDN delete path; 404 → bridge
// fallback. Both paths apply their respective network reload after mutations.
//
// Zone auto-delete: only when ALL hold:
//  1. config.SDNAutoManageZone == true
//  2. zone != config.SDNZone (pinned zone never deleted)
//  3. ListSdnVnets filtered by zone returns 0 remaining vnets
func HandleDeleteNetwork(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return nil, deleteNetwork(ctx, deps, args)
	})
}

func deleteNetwork(ctx context.Context, deps Deps, args []json.RawMessage) error {
	if len(args) < 1 {
		return cpierrors.Cloud("delete_network: missing required argument network_cid")
	}

	var networkCID string
	if err := json.Unmarshal(args[0], &networkCID); err != nil {
		return cpierrors.Cloud("delete_network: network_cid must be a JSON string: %s", err.Error())
	}
	if strings.TrimSpace(networkCID) == "" {
		return cpierrors.Cloud("delete_network: network_cid must not be empty")
	}

	clusterSvc := deps.PVE.Cluster()

	// Probe: is network_cid an SDN vnet?
	vnetResp, vnetErr := clusterSvc.GetSdnVnets(ctx, networkCID, nil)
	if vnetErr != nil {
		if !isSDNNotFound(vnetErr) {
			// Unexpected error probing SDN — surface it.
			return cpierrors.Wrap(vnetErr, fmt.Sprintf("delete_network: probe SDN vnet %q", networkCID))
		}
		// SDN vnet not found — try bridge path.
		return deleteNetworkBridge(ctx, deps, networkCID)
	}

	// SDN path.
	zone := decodeVnetZone(vnetResp)
	return deleteNetworkSDN(ctx, deps, networkCID, zone)
}

// deleteNetworkSDN tears down an SDN vnet and its subnets, applies the SDN
// config, then conditionally removes the parent zone (see zone auto-delete rules
// in HandleDeleteNetwork).
func deleteNetworkSDN(ctx context.Context, deps Deps, vnet, zone string) error {
	cfg := deps.Config
	clusterSvc := deps.PVE.Cluster()

	// 1. Delete subnets first (PVE requires subnets removed before vnet).
	subnetsResp, subnetsErr := clusterSvc.ListSdnVnetsSubnets(ctx, vnet, nil)
	if subnetsErr != nil && !isSDNNotFound(subnetsErr) {
		return cpierrors.Wrap(subnetsErr, fmt.Sprintf("delete_network: list subnets for vnet %q", vnet))
	}
	subnetIDs := extractSubnetIDs(subnetsResp)
	for _, subnetID := range subnetIDs {
		if err := clusterSvc.DeleteSdnVnetsSubnets(ctx, vnet, subnetID, nil); err != nil {
			if isSDNNotFound(err) {
				continue // already gone
			}
			return cpierrors.Wrap(err, fmt.Sprintf("delete_network: delete subnet %q from vnet %q", subnetID, vnet))
		}
	}

	// 2. Delete the vnet.
	if err := clusterSvc.DeleteSdnVnets(ctx, vnet, nil); err != nil {
		if !isSDNNotFound(err) {
			return cpierrors.Wrap(err, fmt.Sprintf("delete_network: delete SDN vnet %q", vnet))
		}
		// Already gone — idempotent continue.
	}

	// 3. Apply SDN.
	if err := applySDN(ctx, deps, clusterSvc, "delete_network"); err != nil {
		return err
	}

	// 4. Conditional zone teardown (see zone auto-delete rules).
	// All three conditions must hold: auto-manage enabled, zone not pinned, zone now empty.
	if zone == "" || !cfg.SDNAutoManageZone {
		return nil
	}
	if cfg.SDNZone != "" && strings.EqualFold(zone, cfg.SDNZone) {
		// Pinned zone — never delete.
		return nil
	}

	// Count remaining vnets in this zone.
	allVnets, listErr := clusterSvc.ListSdnVnets(ctx, nil)
	if listErr != nil {
		// Non-fatal: we cannot confirm zone is empty; leave zone intact rather than
		// risk deleting a zone that still has vnets.
		return nil
	}
	if countVnetsByZone(allVnets, zone) > 0 {
		return nil
	}

	// Zone is empty and auto-managed — delete it and apply.
	if err := clusterSvc.DeleteSdnZones(ctx, zone, nil); err != nil {
		if !isSDNNotFound(err) {
			return cpierrors.Wrap(err, fmt.Sprintf("delete_network: delete SDN zone %q", zone))
		}
	}
	if err := applySDN(ctx, deps, clusterSvc, "delete_network (zone)"); err != nil {
		return err
	}
	return nil
}

// deleteNetworkBridge removes a Linux bridge via the nodes API.
// Uses config.Node as the target (delete_network receives only the network_cid;
// per-bridge node tracking is not stored between calls).
func deleteNetworkBridge(ctx context.Context, deps Deps, iface string) error {
	node := deps.Config.Node
	if node == "" {
		// Cannot locate the bridge without a node. Surface a clear error rather than
		// silently succeeding, because the bridge may still exist on some node.
		return cpierrors.Cloud(
			"delete_network: config.node is required for bridge delete path (network_cid %q)", iface,
		)
	}

	if err := deps.PVE.Nodes().DeleteNetwork2(ctx, node, iface); err != nil {
		if isSDNNotFound(err) {
			// Bridge already gone — idempotent.
			return nil
		}
		return cpierrors.Wrap(err, fmt.Sprintf("delete_network: delete bridge %q on node %q", iface, node))
	}

	// Reload node network config.
	if _, err := deps.PVE.Nodes().UpdateNetwork(ctx, node, nil); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("delete_network: reload network on node %q", node))
	}
	return nil
}
