// Package handlers — create_network handler.
//
// Implements the BOSH CPI v2 create_network method. When cloud_properties
// indicate an SDN zone or the config NetworkMode is "sdn", the handler creates
// a PVE SDN vnet (and optionally a subnet and zone). Otherwise it falls back to
// creating a Linux bridge via the nodes API.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// HandleCreateNetwork returns a Handler that implements the BOSH create_network method.
//
// Arguments:
//
//	[0] network_spec — JSON object:
//	      type             string (manual|dynamic|vip)
//	      range            string (CIDR, optional)
//	      gateway          string (optional)
//	      netmask_bits     int    (optional)
//	      cloud_properties map    (zone, zone_type, vnet, bridge, node)
//
// Returns a 3-element array:
//
//	[network_cid, address_properties, cloud_properties_out]
//
// Routing:
//   - SDN path: cloud_properties.zone set, OR config.SDNZone set, OR NetworkMode=="sdn".
//   - Bridge path: NetworkMode=="bridge", OR only bridge/NetworkBridge available.
//   - Auto (default): SDN when a zone is resolvable; bridge fallback otherwise.
//   - No routing info → cpierrors.Cloud.
func HandleCreateNetwork(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return createNetwork(ctx, deps, args)
	})
}

func createNetwork(ctx context.Context, deps Deps, args []json.RawMessage) (any, error) {
	if len(args) < 1 {
		return nil, cpierrors.Cloud("create_network: missing required argument network_spec")
	}

	spec, err := parseNetworkSpec(args[0])
	if err != nil {
		return nil, err
	}

	cfg := deps.Config
	cp := spec.CloudProperties

	// Determine path.
	zone := cpStr(cp, "zone")
	if zone == "" {
		zone = cfg.SDNZone
	}
	vnet := cpStr(cp, "vnet")
	bridge := cpStr(cp, "bridge")
	if bridge == "" {
		bridge = cfg.NetworkBridge
	}

	useSDN := false
	switch cfg.NetworkMode {
	case "sdn":
		useSDN = true
	case "bridge":
		useSDN = false
	default: // "auto"
		// Use SDN when a zone is resolvable or vnet name is explicitly provided.
		useSDN = zone != "" || vnet != ""
	}

	if useSDN {
		return createNetworkSDN(ctx, deps, spec, zone, vnet)
	}

	// Bridge path.
	if bridge == "" {
		return nil, cpierrors.Cloud(
			"create_network: cannot determine network path — " +
				"supply cloud_properties.zone+vnet (SDN) or cloud_properties.bridge / config.network_bridge (bridge)",
		)
	}
	return createNetworkBridge(ctx, deps, spec, bridge)
}

// createNetworkSDN implements the SDN vnet creation flow.
func createNetworkSDN(
	ctx context.Context,
	deps Deps,
	spec *networkSpec,
	zone string,
	vnet string,
) (any, error) {
	cfg := deps.Config
	cp := spec.CloudProperties
	clusterSvc := deps.PVE.Cluster()

	// vnet name is required.
	if vnet == "" {
		return nil, cpierrors.Cloud(
			"create_network: cloud_properties.vnet is required for the SDN path",
		)
	}
	if err := validateVnetName(vnet); err != nil {
		return nil, err
	}

	// Resolve zone.
	//
	// What sdn_auto_manage_zone does and does NOT do:
	//   - false (default): zone must exist in PVE before create_network is called.
	//     The CPI returns an error if the zone is absent or unspecified.
	//   - true: the CPI will CREATE the zone in PVE when it does not already exist.
	//     It does NOT derive or invent a zone name — the operator must still supply
	//     cloud_properties.zone or config.sdn_zone. The flag only relaxes the
	//     "zone not found" error into an auto-create action.
	createdZone := false
	if zone == "" {
		if !cfg.SDNAutoManageZone {
			return nil, cpierrors.Cloud(
				"create_network: SDN zone is required — set cloud_properties.zone, config sdn_zone, " +
					"or enable sdn_auto_manage_zone",
			)
		}
		// sdn_auto_manage_zone is true but no zone name was provided (neither
		// cloud_properties.zone nor config.sdn_zone). The flag cannot auto-create a
		// zone without a name; the operator must supply one.
		return nil, cpierrors.Cloud(
			"create_network: cloud_properties.zone is required when sdn_auto_manage_zone is true " +
				"and no sdn_zone is configured in the CPI config",
		)
	}

	// Verify zone exists in PVE; create it when sdn_auto_manage_zone is enabled.
	_, zoneGetErr := clusterSvc.GetSdnZones(ctx, zone, nil)
	if zoneGetErr != nil {
		if !isSDNNotFound(zoneGetErr) {
			return nil, cpierrors.Wrap(zoneGetErr, fmt.Sprintf("create_network: get SDN zone %q", zone))
		}
		// Zone does not exist in PVE.
		if !cfg.SDNAutoManageZone {
			return nil, cpierrors.Cloud(
				"create_network: SDN zone %q not found and sdn_auto_manage_zone is false — "+
					"create the zone in PVE or enable sdn_auto_manage_zone",
				zone,
			)
		}
		zoneType := cpStr(cp, "zone_type")
		if zoneType == "" {
			zoneType = cfg.SDNZoneType
		}
		if zoneType == "" {
			zoneType = "simple"
		}
		if err := clusterSvc.CreateSdnZones(ctx, &sdkcluster.CreateSdnZonesParams{
			Zone: zone,
			Type: zoneType,
		}); err != nil {
			return nil, cpierrors.Wrap(err, fmt.Sprintf("create_network: create SDN zone %q", zone))
		}
		createdZone = true
	}

	// Idempotent vnet create.
	vnetCreated := false
	_, vnetGetErr := clusterSvc.GetSdnVnets(ctx, vnet, nil)
	if vnetGetErr != nil {
		if !isSDNNotFound(vnetGetErr) {
			return nil, cpierrors.Wrap(vnetGetErr, fmt.Sprintf("create_network: get SDN vnet %q", vnet))
		}
		// Vnet does not exist — create it.
		if err := clusterSvc.CreateSdnVnets(ctx, &sdkcluster.CreateSdnVnetsParams{
			Vnet: vnet,
			Zone: zone,
		}); err != nil {
			// 409 conflict = already exists from a concurrent call; treat as idempotent.
			if !isSDNConflict(err) {
				// Best-effort rollback of zone we created this call.
				if createdZone {
					_ = clusterSvc.DeleteSdnZones(ctx, zone, nil)
					// Apply rollback so the staged zone deletion is committed.
					if applyErr := applySDN(ctx, deps, clusterSvc, "create_network: rollback zone after vnet-create failure"); applyErr != nil {
						deps.Logger.Warn("create_network: rollback apply failed after zone delete", log.Err(applyErr))
					}
				}
				return nil, cpierrors.Wrap(err, fmt.Sprintf("create_network: create SDN vnet %q", vnet))
			}
			// 409: vnet exists from concurrent call — idempotent, do not mark vnetCreated.
		} else {
			vnetCreated = true
		}
	}
	// If GetSdnVnets succeeded (vnet already existed), vnetCreated=false — idempotent.

	// Create subnet when range is present.
	subnetCreated := false
	if spec.Range != "" {
		subnetParams := &sdkcluster.CreateSdnVnetsSubnetsParams{
			Subnet: spec.Range,
			Type:   "subnet",
		}
		if spec.Gateway != "" {
			gw := spec.Gateway
			subnetParams.Gateway = &gw
		}
		if err := clusterSvc.CreateSdnVnetsSubnets(ctx, vnet, subnetParams); err != nil {
			// 409 = subnet already exists; idempotent.
			if !isSDNConflict(err) {
				// Best-effort rollback of what THIS call created.
				// vnetCreated guards against deleting a pre-existing vnet.
				if vnetCreated {
					_ = clusterSvc.DeleteSdnVnets(ctx, vnet, nil)
				}
				if createdZone {
					_ = clusterSvc.DeleteSdnZones(ctx, zone, nil)
				}
				if vnetCreated || createdZone {
					// Apply to commit the staged rollback deletions.
					if applyErr := applySDN(ctx, deps, clusterSvc, "create_network: rollback after subnet-create failure"); applyErr != nil {
						deps.Logger.Warn("create_network: rollback apply failed after subnet-create failure", log.Err(applyErr))
					}
				}
				return nil, cpierrors.Wrap(err, fmt.Sprintf("create_network: create subnet %q on vnet %q", spec.Range, vnet))
			}
			// 409: subnet already existed — idempotent, subnetCreated stays false.
		} else {
			subnetCreated = true
		}
	}

	// Apply SDN config to data plane.
	if err := applySDN(ctx, deps, clusterSvc, "create_network"); err != nil {
		// Best-effort rollback on apply failure. Only undo what THIS call created.
		// subnetCreated guards against deleting a pre-existing subnet (F-7).
		if subnetCreated {
			_ = clusterSvc.DeleteSdnVnetsSubnets(ctx, vnet, spec.Range, nil)
		}
		if vnetCreated {
			_ = clusterSvc.DeleteSdnVnets(ctx, vnet, nil)
		}
		if createdZone {
			_ = clusterSvc.DeleteSdnZones(ctx, zone, nil)
		}
		// Apply to commit the staged rollback deletions (D-05: every mutation followed by UpdateSdn).
		if subnetCreated || vnetCreated || createdZone {
			if applyErr := applySDN(ctx, deps, clusterSvc, "create_network: rollback after apply failure"); applyErr != nil {
				deps.Logger.Warn("create_network: rollback apply failed after main apply failure", log.Err(applyErr))
			}
		}
		return nil, err
	}

	// Build BOSH 3-element response.
	// Bridge name == vnet name: PVE simple zone realizes the vnet as a
	// Linux bridge named identically to the vnet.
	addrProps := map[string]any{
		"range":    spec.Range,
		"gateway":  spec.Gateway,
		"reserved": []string{},
	}
	cloudPropsOut := map[string]any{
		"zone":   zone,
		"vnet":   vnet,
		"bridge": vnet,
	}
	return []any{vnet, addrProps, cloudPropsOut}, nil
}

// createNetworkBridge implements the Linux bridge creation flow via the nodes API.
func createNetworkBridge(
	ctx context.Context,
	deps Deps,
	spec *networkSpec,
	bridge string,
) (any, error) {
	cp := spec.CloudProperties
	node := cpStr(cp, "node")
	if node == "" {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud(
			"create_network: target node not set — supply cloud_properties.node or config.node",
		)
	}

	autostart := true
	if err := deps.PVE.Nodes().CreateNetwork(ctx, node, &sdknodes.CreateNetworkParams{
		Iface:     bridge,
		Type:      "bridge",
		Autostart: &autostart,
	}); err != nil {
		// 409 = bridge already exists; idempotent.
		if !isSDNConflict(err) {
			return nil, cpierrors.Wrap(err, fmt.Sprintf("create_network: create bridge %q on node %q", bridge, node))
		}
	}

	// Apply / reload network config on the node.
	if _, err := deps.PVE.Nodes().UpdateNetwork(ctx, node, nil); err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("create_network: reload network on node %q", node))
	}

	addrProps := map[string]any{
		"range":    spec.Range,
		"gateway":  spec.Gateway,
		"reserved": []string{},
	}
	cloudPropsOut := map[string]any{
		"bridge": bridge,
		"node":   node,
	}
	return []any{bridge, addrProps, cloudPropsOut}, nil
}
