// create_network handler: implements the BOSH CPI v2 create_network method.
// When cloud_properties indicate an SDN zone or the config NetworkMode is
// "sdn", creates a PVE SDN vnet (and optionally a subnet and zone). Otherwise
// falls back to creating a Linux bridge via the nodes API.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// networkModeBridge is the config.NetworkMode value that forces the bridge path.
// Defined as a constant because the literal "bridge" appears in both the mode
// switch and as a PVE node API Type string; keeping them named avoids the goconst
// threshold while making the distinct semantics explicit.
const networkModeBridge = "bridge"

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

	// Build a layered resolver so operator profiles (vm_type / disk_type) can
	// supply attribute defaults above config but below direct call values.
	r, rErr := newLayeredResolver(cp, cfg)
	if rErr != nil {
		return nil, rErr
	}

	// Determine path using layered resolution with config fallbacks.
	var zone string
	if v, ok := r.String("zone"); ok {
		zone = v
	} else {
		zone = cfg.SDNZone
	}

	var vnet string
	if v, ok := r.String("vnet"); ok {
		vnet = v
	}

	var bridge string
	if v, ok := r.String(nicCPKeyBridge); ok {
		bridge = v
	} else if cfg.NetworkBridge != "" {
		bridge = cfg.NetworkBridge
	}

	useSDN := false
	switch cfg.NetworkMode {
	case "sdn":
		useSDN = true
	case networkModeBridge:
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

// sdnZoneArgs carries all parameters needed by resolveOrCreateSDNZone.
type sdnZoneArgs struct {
	zone              string
	zoneType          string
	sdnAutoManageZone bool
}

// sdnVnetArgs carries all parameters needed by createVnetIdempotent.
type sdnVnetArgs struct {
	vnet string
	zone string
}

// sdnSubnetArgs carries all parameters needed by createSubnetIdempotent.
type sdnSubnetArgs struct {
	vnet    string
	subnet  string
	gateway string
}

// sdnApplyArgs carries all parameters needed by applyWithRollback.
type sdnApplyArgs struct {
	opCtx string
}

// resolveOrCreateSDNZone checks that the named zone exists in PVE. When the
// zone is absent and sdnAutoManageZone is true, it creates the zone. The bool
// return value is true only when THIS call created the zone; callers must
// delete it on rollback when true. Original-call errors are wrapped through
// pve.WrapError so transient classes keep their Retriable type.
func resolveOrCreateSDNZone(
	ctx context.Context,
	_ Deps,
	clusterSvc sdkcluster.Service,
	args sdnZoneArgs,
) (zoneCreated bool, err error) {
	_, zoneGetErr := clusterSvc.GetSdnZones(ctx, args.zone, nil)
	if zoneGetErr == nil {
		// Zone already exists — nothing to create.
		return false, nil
	}
	if !isSDNNotFound(zoneGetErr) {
		return false, cpierrors.Wrap(
			pve.WrapError(zoneGetErr),
			fmt.Sprintf("create_network: get SDN zone %q", args.zone),
		)
	}
	// Zone does not exist in PVE.
	if !args.sdnAutoManageZone {
		return false, cpierrors.Cloud(
			"create_network: SDN zone %q not found and sdn_auto_manage_zone is false — "+
				"create the zone in PVE or enable sdn_auto_manage_zone",
			args.zone,
		)
	}
	zoneType := args.zoneType
	if zoneType == "" {
		zoneType = "simple"
	}
	if err := clusterSvc.CreateSdnZones(ctx, &sdkcluster.CreateSdnZonesParams{
		Zone: args.zone,
		Type: zoneType,
	}); err != nil {
		return false, cpierrors.Wrap(
			pve.WrapError(err),
			fmt.Sprintf("create_network: create SDN zone %q", args.zone),
		)
	}
	return true, nil
}

// createVnetIdempotent probes PVE for an existing vnet and creates it when
// absent. A 409 conflict from CreateSdnVnets (concurrent creation race) is
// treated as idempotent success; the bool is false in that case so rollback
// does not delete a vnet this call did not own. The bool return is true only
// when THIS call created the vnet; callers must delete it on rollback.
func createVnetIdempotent(
	ctx context.Context,
	_ Deps,
	clusterSvc sdkcluster.Service,
	args sdnVnetArgs,
) (vnetCreated bool, err error) {
	_, vnetGetErr := clusterSvc.GetSdnVnets(ctx, args.vnet, nil)
	if vnetGetErr == nil {
		// Vnet already exists — treat as idempotent, vnetCreated=false.
		return false, nil
	}
	if !isSDNNotFound(vnetGetErr) {
		return false, cpierrors.Wrap(
			pve.WrapError(vnetGetErr),
			fmt.Sprintf("create_network: get SDN vnet %q", args.vnet),
		)
	}
	// Vnet does not exist — create it.
	if createErr := clusterSvc.CreateSdnVnets(ctx, &sdkcluster.CreateSdnVnetsParams{
		Vnet: args.vnet,
		Zone: args.zone,
	}); createErr != nil {
		// 409 conflict = already exists from a concurrent call; treat as idempotent.
		if isSDNConflict(createErr) {
			return false, nil
		}
		return false, createErr
	}
	return true, nil
}

// createSubnetIdempotent creates the subnet on the named vnet. A 409 conflict
// (subnet already exists) is treated as idempotent success; the bool stays
// false so rollback does not delete a pre-existing subnet. The bool return is
// true only when THIS call created the subnet.
func createSubnetIdempotent(
	ctx context.Context,
	_ Deps,
	clusterSvc sdkcluster.Service,
	args sdnSubnetArgs,
) (subnetCreated bool, err error) {
	subnetParams := &sdkcluster.CreateSdnVnetsSubnetsParams{
		Subnet: args.subnet,
		Type:   "subnet",
	}
	if args.gateway != "" {
		gw := args.gateway
		subnetParams.Gateway = &gw
	}
	if createErr := clusterSvc.CreateSdnVnetsSubnets(ctx, args.vnet, subnetParams); createErr != nil {
		// 409 = subnet already exists; idempotent.
		if isSDNConflict(createErr) {
			return false, nil
		}
		return false, createErr
	}
	return true, nil
}

// applyWithRollback calls applySDN and, on failure, invokes rollbackFn then
// returns the original apply error. rollbackFn must use a detached context
// (contextWithoutCancel) so cleanup completes even when the caller's context
// is cancelled; the caller is responsible for constructing that context and
// closing over it inside rollbackFn.
func applyWithRollback(
	ctx context.Context,
	deps Deps,
	clusterSvc sdkcluster.Service,
	args sdnApplyArgs,
	rollbackFn func(),
) error {
	if err := applySDN(ctx, deps, clusterSvc, args.opCtx); err != nil {
		rollbackFn()
		return err
	}
	return nil
}

// createNetworkSDN implements the SDN vnet creation flow.
//
// Rollback discipline: every best-effort cleanup call uses a context derived
// via contextWithoutCancel so a caller-cancelled request still releases the
// staged PVE state. Original-call errors are wrapped through pve.WrapError so
// transient classes (5xx, ConnectionError, TimeoutError, storage lock) keep
// their Retriable type all the way back to the BOSH director.
// nolint:gocognit // Orchestration shell for 4 sequential SDN phases (zone, vnet, subnet, apply) with rollback chain; cognitive floor is set by the phase count, not by local complexity. Phase logic lives in resolveOrCreateSDNZone, createVnetIdempotent, createSubnetIdempotent, applyWithRollback.
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

	zoneType, err := validateCreateNetworkSDNPreflight(cfg, cp, zone, vnet)
	if err != nil {
		return nil, err
	}

	// Phase 1: verify zone exists in PVE; create it when sdn_auto_manage_zone is enabled.
	createdZone, err := resolveOrCreateSDNZone(ctx, deps, clusterSvc, sdnZoneArgs{
		zone:              zone,
		zoneType:          zoneType,
		sdnAutoManageZone: cfg.SDNAutoManageZone,
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: idempotent vnet create. A concurrent caller may have already
	// created the vnet between our GetSdnVnets probe and our CreateSdnVnets call;
	// the 409 conflict path inside createVnetIdempotent handles that race.
	vnetCreated, vnetCreateErr := createVnetIdempotent(ctx, deps, clusterSvc, sdnVnetArgs{
		vnet: vnet,
		zone: zone,
	})
	if vnetCreateErr != nil {
		// Best-effort rollback of zone we created this call. Use a context
		// detached from the parent's cancellation so cleanup runs even after
		// the caller aborts.
		if createdZone {
			rollbackCtx := contextWithoutCancel(ctx)
			if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
				deps.Logger.Warn("create_network: rollback delete zone failed", log.Err(delErr))
			}
			// Apply rollback so the staged zone deletion is committed.
			if applyErr := applySDN(rollbackCtx, deps, clusterSvc,
				"create_network: rollback zone after vnet-create failure"); applyErr != nil {
				deps.Logger.Warn(
					"create_network: rollback apply failed after zone delete",
					log.Err(applyErr),
				)
			}
		}
		return nil, cpierrors.Wrap(
			pve.WrapError(vnetCreateErr),
			fmt.Sprintf("create_network: create SDN vnet %q", vnet),
		)
	}

	// Phase 3: create subnet when range is present.
	subnetCreated := false
	if spec.Range != "" {
		var subnetCreateErr error
		subnetCreated, subnetCreateErr = createSubnetIdempotent(ctx, deps, clusterSvc, sdnSubnetArgs{
			vnet:    vnet,
			subnet:  spec.Range,
			gateway: spec.Gateway,
		})
		if subnetCreateErr != nil {
			// Best-effort rollback of what THIS call created. vnetCreated /
			// createdZone guard against deleting pre-existing resources.
			rollbackCtx := contextWithoutCancel(ctx)
			if vnetCreated {
				if delErr := clusterSvc.DeleteSdnVnets(rollbackCtx, vnet, nil); delErr != nil {
					deps.Logger.Warn("create_network: rollback delete vnet failed", log.Err(delErr))
				}
			}
			if createdZone {
				if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
					deps.Logger.Warn("create_network: rollback delete zone failed", log.Err(delErr))
				}
			}
			if vnetCreated || createdZone {
				// Apply to commit the staged rollback deletions.
				if applyErr := applySDN(rollbackCtx, deps, clusterSvc,
					"create_network: rollback after subnet-create failure"); applyErr != nil {
					deps.Logger.Warn(
						"create_network: rollback apply failed after subnet-create failure",
						log.Err(applyErr),
					)
				}
			}
			return nil, cpierrors.Wrap(
				pve.WrapError(subnetCreateErr),
				fmt.Sprintf("create_network: create subnet %q on vnet %q", spec.Range, vnet),
			)
		}
	}

	// Phase 4: apply SDN config to data plane. On failure roll back only what
	// THIS call created (subnetCreated / vnetCreated / createdZone guard against
	// touching pre-existing state). Rollback runs on a detached context so it
	// completes even when the caller cancelled the request.
	rollbackCtx := contextWithoutCancel(ctx)
	applyErr := applyWithRollback(ctx, deps, clusterSvc, sdnApplyArgs{opCtx: "create_network"},
		func() {
			if subnetCreated {
				if delErr := clusterSvc.DeleteSdnVnetsSubnets(rollbackCtx, vnet, spec.Range, nil); delErr != nil {
					deps.Logger.Warn("create_network: rollback delete subnet failed", log.Err(delErr))
				}
			}
			if vnetCreated {
				if delErr := clusterSvc.DeleteSdnVnets(rollbackCtx, vnet, nil); delErr != nil {
					deps.Logger.Warn("create_network: rollback delete vnet failed", log.Err(delErr))
				}
			}
			if createdZone {
				if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
					deps.Logger.Warn("create_network: rollback delete zone failed", log.Err(delErr))
				}
			}
			// Apply to commit the staged rollback deletions. Every SDN mutation
			// must be followed by UpdateSdn or it stays pending in /etc/pve/sdn/.cfg.
			if subnetCreated || vnetCreated || createdZone {
				if rb2Err := applySDN(rollbackCtx, deps, clusterSvc,
					"create_network: rollback after apply failure"); rb2Err != nil {
					deps.Logger.Warn(
						"create_network: rollback apply failed after main apply failure",
						log.Err(rb2Err),
					)
				}
			}
		},
	)
	if applyErr != nil {
		// Return the apply error as-is. applySDN wraps the SDK error with its
		// own context prefix already; adding another wrap layer here would
		// double-prefix without adding information.
		return nil, applyErr
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
		"zone":          zone,
		"vnet":          vnet,
		nicCPKeyBridge: vnet,
	}
	return []any{vnet, addrProps, cloudPropsOut}, nil
}

// validateCreateNetworkSDNPreflight performs the pre-PVE-call validation for
// the SDN create_network path:
//   - vnet name is required + matches the PVE vnet name grammar.
//   - zone must be supplied (cloud_properties.zone or config.sdn_zone). When
//     sdn_auto_manage_zone is true the CPI will create the zone if absent, but
//     it does NOT invent a zone name; the operator must still supply one.
//
// Returns the resolved zone type used for auto-create. Resolution order:
// resolver (call CP → disk_type profile → vm_type profile) → config.SDNZoneType.
func validateCreateNetworkSDNPreflight(cfg *config.CPIConfig, cp map[string]any, zone, vnet string) (string, error) {
	if vnet == "" {
		return "", cpierrors.Cloud(
			"create_network: cloud_properties.vnet is required for the SDN path",
		)
	}
	if err := validateVnetName(vnet); err != nil {
		return "", err
	}

	if zone == "" {
		if !cfg.SDNAutoManageZone {
			return "", cpierrors.Cloud(
				"create_network: SDN zone is required — set cloud_properties.zone, config sdn_zone, " +
					"or enable sdn_auto_manage_zone",
			)
		}
		return "", cpierrors.Cloud(
			"create_network: cloud_properties.zone is required when sdn_auto_manage_zone is true " +
				"and no sdn_zone is configured in the CPI config",
		)
	}

	// Resolve zone_type via layered resolver so profiles can supply it.
	// Errors from newLayeredResolver have already been checked upstream
	// (createNetwork builds and validates the resolver before calling this
	// function). Build a fresh resolver here using only the call CP; the
	// profile layers have no zone_type in most configurations so the extra
	// allocations are negligible, and correctness trumps micro-optimization.
	r, rErr := newLayeredResolver(cp, cfg)
	if rErr != nil {
		// Resolver error here means an unknown vm_type/disk_type in the call CP.
		// Return it as-is — it is already a CloudError.
		return "", rErr
	}
	var zoneType string
	if v, ok := r.String("zone_type"); ok {
		zoneType = v
	} else {
		zoneType = cfg.SDNZoneType
	}
	return zoneType, nil
}

// createNetworkBridge implements the Linux bridge creation flow via the nodes API.
//
// cloudPropNodeKey is the network cloud_properties field naming the PVE node a
// bridge belongs to; read from the create_network spec and echoed back out.
const cloudPropNodeKey = "node"

// PVE's POST /nodes/{node}/network creates a staged interface entry; the
// follow-up UpdateNetwork (PUT /nodes/{node}/network) reloads ifupdown2 so
// the bridge is realized on the host. Both calls flow through pve.WrapError
// so transient transport faults bubble back to BOSH as Retriable.
func createNetworkBridge(
	ctx context.Context,
	deps Deps,
	spec *networkSpec,
	bridge string,
) (any, error) {
	cp := spec.CloudProperties
	// Resolve node via layered resolver so profiles can supply the target node.
	// Errors from newLayeredResolver are already caught by createNetwork upstream;
	// a second call here handles the slim case where cp changed (it hasn't) —
	// guard the error path defensively for correctness.
	var node string
	if r, rErr := newLayeredResolver(cp, deps.Config); rErr == nil {
		if v, ok := r.String(cloudPropNodeKey); ok {
			node = v
		}
	}
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
		Type:      networkModeBridge,
		Autostart: &autostart,
	}); err != nil {
		// 409 = bridge already exists; idempotent.
		if !isSDNConflict(err) {
			return nil, cpierrors.Wrap(
				pve.WrapError(err),
				fmt.Sprintf("create_network: create bridge %q on node %q", bridge, node),
			)
		}
	}

	// Apply / reload network config on the node.
	if _, err := deps.PVE.Nodes().UpdateNetwork(ctx, node, nil); err != nil {
		return nil, cpierrors.Wrap(
			pve.WrapError(err),
			fmt.Sprintf("create_network: reload network on node %q", node),
		)
	}

	addrProps := map[string]any{
		"range":    spec.Range,
		"gateway":  spec.Gateway,
		"reserved": []string{},
	}
	cloudPropsOut := map[string]any{
		nicCPKeyBridge:   bridge,
		cloudPropNodeKey: node,
	}
	return []any{bridge, addrProps, cloudPropsOut}, nil
}
