// create_network handler: implements the BOSH CPI v2 create_network method.
// When cloud_properties indicate an SDN zone or the config NetworkMode is
// "sdn", creates a PVE SDN vnet (and optionally a subnet and zone). Otherwise
// falls back to creating a Linux bridge via the nodes API.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"strings"

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

// defaultSDNZoneName is the turnkey zone the CPI creates when the SDN path is
// active, sdn_auto_manage_zone is enabled, and neither cloud_properties.zone
// nor config sdn_zone names one. A fixed name keeps repeat deployments
// converging on a single CPI-owned zone instead of inventing one per network.
const defaultSDNZoneName = "bosh"

// PVE SDN zone plugin types. vlan/qinq/vxlan/evpn vnets carry a tag (VLAN ID
// or VNI); simple-zone vnets are untagged per-node bridges.
const (
	zoneTypeSimple = "simple"
	zoneTypeVlan   = "vlan"
	zoneTypeQinq   = "qinq"
	zoneTypeVxlan  = "vxlan"
	zoneTypeEvpn   = "evpn"
)

// vlanMaxTag is the 12-bit VLAN ID ceiling that caps vnet tags in vlan and
// qinq zones; vxlan/evpn VNIs may use the full 24-bit space.
const vlanMaxTag = 4094

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
		// Turnkey zone name: when the SDN path is active with auto-manage
		// enabled and no zone was named anywhere, the CPI owns a fixed zone.
		// Explicit network_mode=auto with no zone never reaches here (the
		// switch above already routed it to the bridge path).
		if zone == "" && cfg.SDNAutoManageZoneEnabled() {
			zone = defaultSDNZoneName
		}
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
// zoneType is the EFFECTIVE zone type (from PVE when the zone pre-exists,
// else the configured type) — it decides whether the vnet needs a tag.
// explicitTag is cloud_properties.vnet_tag (0 = auto-allocate when needed).
type sdnVnetArgs struct {
	vnet        string
	zone        string
	zoneType    string
	explicitTag int
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
//
// The string return is the EFFECTIVE zone type: when the zone pre-exists in
// PVE its actual plugin type governs downstream vnet tagging (an operator's
// vxlan zone must get tagged vnets even if the CPI config still says simple);
// when this call creates the zone the configured type is authoritative. EVPN
// zones are never auto-created — an EVPN fabric (controller, BGP peers) is
// operator infrastructure, so an absent EVPN zone fails fast regardless of
// sdnAutoManageZone.
func resolveOrCreateSDNZone(
	ctx context.Context,
	deps Deps,
	clusterSvc sdkcluster.Service,
	args sdnZoneArgs,
) (effectiveZoneType string, zoneCreated bool, err error) {
	zoneType := args.zoneType
	if zoneType == "" {
		zoneType = zoneTypeVxlan
	}

	resp, zoneGetErr := clusterSvc.GetSdnZones(ctx, args.zone, nil)
	if zoneGetErr == nil {
		// Zone already exists — nothing to create. Prefer the actual type from
		// PVE; fall back to the configured type on a sparse response.
		if resp != nil {
			var z struct {
				Type string `json:"type"`
			}
			if jsonErr := json.Unmarshal(*resp, &z); jsonErr == nil && z.Type != "" {
				zoneType = z.Type
			}
		}
		return zoneType, false, nil
	}
	if !isSDNNotFound(zoneGetErr) {
		return "", false, cpierrors.Wrap(
			pve.WrapError(zoneGetErr),
			fmt.Sprintf("create_network: get SDN zone %q", args.zone),
		)
	}
	// Zone does not exist in PVE.
	if zoneType == zoneTypeEvpn {
		return "", false, cpierrors.Cloud(
			"create_network: EVPN zone %q not found — the CPI never creates EVPN zones. "+
				"Create the zone and its BGP controller in PVE (Datacenter → SDN) first; "+
				"the CPI then manages only vnets and subnets inside it",
			args.zone,
		)
	}
	if !args.sdnAutoManageZone {
		return "", false, cpierrors.Cloud(
			"create_network: SDN zone %q not found and sdn_auto_manage_zone is false — "+
				"create the zone in PVE or enable sdn_auto_manage_zone",
			args.zone,
		)
	}
	params, paramsErr := buildZoneCreateParams(ctx, deps, args.zone, zoneType)
	if paramsErr != nil {
		return "", false, paramsErr
	}
	if err := clusterSvc.CreateSdnZones(ctx, params); err != nil {
		return "", false, cpierrors.Wrap(
			pve.WrapError(err),
			fmt.Sprintf("create_network: create SDN zone %q", args.zone),
		)
	}
	return zoneType, true, nil
}

// buildZoneCreateParams assembles the POST /cluster/sdn/zones payload for a
// CPI-created zone. vxlan zones need a peer list — explicit sdn_vxlan_peers
// when set, else the online cluster node IPs; zero derivable peers is a hard
// error because PVE would accept the zone but no tunnel would ever come up.
// vlan zones need the underlay bridge (PVE rejects vlan zones without one).
// The optional sdn_zone_mtu override applies to every type; when unset PVE
// derives the vnet MTU from the underlay (e.g. 1450 on a 1500 underlay).
func buildZoneCreateParams(
	ctx context.Context,
	deps Deps,
	zone string,
	zoneType string,
) (*sdkcluster.CreateSdnZonesParams, error) {
	cfg := deps.Config
	params := &sdkcluster.CreateSdnZonesParams{
		Zone: zone,
		Type: zoneType,
	}
	if cfg.SDNZoneMTU != nil {
		mtu := *cfg.SDNZoneMTU
		params.Mtu = &mtu
	}
	switch zoneType {
	case zoneTypeVxlan:
		peers := cfg.SDNVxlanPeers
		if len(peers) == 0 {
			derived, peersErr := pve.ClusterNodePeerIPs(ctx, deps.PVE)
			if peersErr != nil {
				return nil, cpierrors.Wrap(peersErr,
					fmt.Sprintf("create_network: derive VXLAN peers for zone %q", zone))
			}
			peers = derived
		}
		if len(peers) == 0 {
			return nil, cpierrors.Cloud(
				"create_network: cannot create VXLAN zone %q — no peer IPs derivable from "+
					"/cluster/status and sdn_vxlan_peers is empty; set pve.sdn_vxlan_peers explicitly",
				zone,
			)
		}
		// The API contract is a comma-separated peer list (the PVE UI shows
		// space-separated, but the schema says comma).
		joined := strings.Join(peers, ",")
		params.Peers = &joined
	case zoneTypeVlan:
		if cfg.NetworkBridge == "" {
			return nil, cpierrors.Cloud(
				"create_network: cannot create vlan zone %q — PVE requires an underlay bridge; "+
					"set pve.network_bridge",
				zone,
			)
		}
		bridge := cfg.NetworkBridge
		params.Bridge = &bridge
	}
	return params, nil
}

// zoneTypeRequiresTag reports whether vnets in the given zone type carry a
// tag (VLAN ID for vlan/qinq, VNI for vxlan/evpn). Simple-zone vnets are
// untagged per-node bridges.
func zoneTypeRequiresTag(zoneType string) bool {
	switch zoneType {
	case zoneTypeVlan, zoneTypeQinq, zoneTypeVxlan, zoneTypeEvpn:
		return true
	default:
		return false
	}
}

// resolveVnetTag returns the tag to bake into a NEW vnet: the explicit
// cloud_properties.vnet_tag when set, else an auto-allocated VNI from the
// configured band. Called only after the vnet-existence probe reports absent,
// so retries and pre-existing vnets never burn VNIs. vlan/qinq zones cap tags
// at 4094 (12-bit VLAN ID); an explicit tag on an untagged zone type is a
// config contradiction and errors rather than being silently dropped.
func resolveVnetTag(ctx context.Context, deps Deps, zoneType string, explicitTag int) (int, error) {
	if !zoneTypeRequiresTag(zoneType) {
		if explicitTag != 0 {
			return 0, cpierrors.Cloud(
				"create_network: cloud_properties.vnet_tag is only valid for vlan|qinq|vxlan|evpn zones; "+
					"the target zone is type %q",
				zoneType,
			)
		}
		return 0, nil
	}
	capped := zoneType == zoneTypeVlan || zoneType == zoneTypeQinq
	if explicitTag != 0 {
		if capped && explicitTag > vlanMaxTag {
			return 0, cpierrors.Cloud(
				"create_network: cloud_properties.vnet_tag %d exceeds the %s-zone maximum %d",
				explicitTag, zoneType, vlanMaxTag,
			)
		}
		return explicitTag, nil
	}
	cfg := deps.Config
	start, end := cfg.SDNVNIRangeStart, cfg.SDNVNIRangeEnd
	if start == 0 {
		start = 5000
	}
	if end == 0 {
		end = 5999
	}
	if capped {
		if start > vlanMaxTag {
			return 0, cpierrors.Cloud(
				"create_network: sdn_vni_range_start %d exceeds the %s-zone tag maximum %d — "+
					"set sdn_vni_range within 1..%d or supply cloud_properties.vnet_tag",
				start, zoneType, vlanMaxTag, vlanMaxTag,
			)
		}
		if end > vlanMaxTag {
			end = vlanMaxTag
		}
	}
	return pve.NextVNI(ctx, deps.PVE, start, end)
}

// createVnetIdempotent probes PVE for an existing vnet and creates it when
// absent. A 409 conflict from CreateSdnVnets (concurrent creation race) is
// treated as idempotent success; the bool is false in that case so rollback
// does not delete a vnet this call did not own. The bool return is true only
// when THIS call created the vnet; callers must delete it on rollback.
//
// Tagged zone types (vlan/qinq/vxlan/evpn) get a vnet tag: the explicit
// cloud_properties.vnet_tag when supplied, else an auto-allocated VNI. Tag
// resolution runs strictly after the existence probe so pre-existing vnets
// and director retries never consume VNIs from the band.
func createVnetIdempotent(
	ctx context.Context,
	deps Deps,
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
	tag, tagErr := resolveVnetTag(ctx, deps, args.zoneType, args.explicitTag)
	if tagErr != nil {
		return false, tagErr
	}
	createParams := &sdkcluster.CreateSdnVnetsParams{
		Vnet: args.vnet,
		Zone: args.zone,
	}
	if tag != 0 {
		tag64 := int64(tag)
		createParams.Tag = &tag64
	}
	if createErr := clusterSvc.CreateSdnVnets(ctx, createParams); createErr != nil {
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

	zoneType, explicitTag, err := validateCreateNetworkSDNPreflight(cfg, cp, zone, vnet)
	if err != nil {
		return nil, err
	}

	// Phase 1: verify zone exists in PVE; create it when sdn_auto_manage_zone is
	// enabled. The effective zone type comes back from PVE when the zone
	// pre-exists so vnet tagging matches reality, not just config.
	effectiveZoneType, createdZone, err := resolveOrCreateSDNZone(ctx, deps, clusterSvc, sdnZoneArgs{
		zone:              zone,
		zoneType:          zoneType,
		sdnAutoManageZone: cfg.SDNAutoManageZoneEnabled(),
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: idempotent vnet create. A concurrent caller may have already
	// created the vnet between our GetSdnVnets probe and our CreateSdnVnets call;
	// the 409 conflict path inside createVnetIdempotent handles that race.
	vnetCreated, vnetCreateErr := createVnetIdempotent(ctx, deps, clusterSvc, sdnVnetArgs{
		vnet:        vnet,
		zone:        zone,
		zoneType:    effectiveZoneType,
		explicitTag: explicitTag,
	})
	if vnetCreateErr != nil {
		// Best-effort rollback of zone we created this call. Use a context
		// detached from the parent's cancellation so cleanup runs even after
		// the caller aborts.
		if createdZone {
			rollbackCtx := contextWithoutCancel(ctx)
			if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
				deps.Log(rollbackCtx).Warn("create_network: rollback delete zone failed", log.Err(delErr))
			}
			// Apply rollback so the staged zone deletion is committed.
			if applyErr := applySDN(rollbackCtx, deps, clusterSvc,
				"create_network: rollback zone after vnet-create failure"); applyErr != nil {
				deps.Log(rollbackCtx).Warn(
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
					deps.Log(rollbackCtx).Warn("create_network: rollback delete vnet failed", log.Err(delErr))
				}
			}
			if createdZone {
				if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
					deps.Log(rollbackCtx).Warn("create_network: rollback delete zone failed", log.Err(delErr))
				}
			}
			if vnetCreated || createdZone {
				// Apply to commit the staged rollback deletions.
				if applyErr := applySDN(rollbackCtx, deps, clusterSvc,
					"create_network: rollback after subnet-create failure"); applyErr != nil {
					deps.Log(rollbackCtx).Warn(
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
					deps.Log(rollbackCtx).Warn("create_network: rollback delete subnet failed", log.Err(delErr))
				}
			}
			if vnetCreated {
				if delErr := clusterSvc.DeleteSdnVnets(rollbackCtx, vnet, nil); delErr != nil {
					deps.Log(rollbackCtx).Warn("create_network: rollback delete vnet failed", log.Err(delErr))
				}
			}
			if createdZone {
				if delErr := clusterSvc.DeleteSdnZones(rollbackCtx, zone, nil); delErr != nil {
					deps.Log(rollbackCtx).Warn("create_network: rollback delete zone failed", log.Err(delErr))
				}
			}
			// Apply to commit the staged rollback deletions. Every SDN mutation
			// must be followed by UpdateSdn or it stays pending in /etc/pve/sdn/.cfg.
			if subnetCreated || vnetCreated || createdZone {
				if rb2Err := applySDN(rollbackCtx, deps, clusterSvc,
					"create_network: rollback after apply failure"); rb2Err != nil {
					deps.Log(rollbackCtx).Warn(
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

	// Optional produce-side eventual-consistency gate. SDN apply commits the
	// config but data-plane realization is asynchronous; when enabled, poll the
	// running cluster SDN config until this vnet converges so create_network does
	// not report success before the next create_vm can resolve the bridge. On
	// timeout the error is retriable and the director re-drives. Off → no poll.
	if cfg.NetworkResolveEnabled() {
		if waitErr := pve.WaitForSDNVnetConverged(ctx, deps.PVE, vnet,
			cfg.NetworkResolveRetriesValue(),
			time.Duration(cfg.NetworkResolveTimeoutSecValue())*time.Second); waitErr != nil {
			return nil, cpierrors.Wrap(waitErr, "create_network")
		}
	}

	// Build BOSH 3-element response.
	// Bridge name == vnet name: PVE realizes every SDN vnet as a per-node
	// Linux bridge named after the vnet, for all zone types.
	addrProps := map[string]any{
		"range":    spec.Range,
		"gateway":  spec.Gateway,
		"reserved": []string{},
	}
	cloudPropsOut := map[string]any{
		"zone":         zone,
		"vnet":         vnet,
		nicCPKeyBridge: vnet,
	}
	return []any{vnet, addrProps, cloudPropsOut}, nil
}

// validateCreateNetworkSDNPreflight performs the pre-PVE-call validation for
// the SDN create_network path:
//   - vnet name is required + matches the PVE vnet name grammar.
//   - zone must be resolvable. With sdn_auto_manage_zone enabled the routing
//     layer already filled the turnkey default ("bosh"), so an empty zone here
//     means auto-manage is off and the operator must name one.
//   - cloud_properties.vnet_tag, when set, must be a valid 24-bit VNI. The
//     vlan/qinq 4094 cap is enforced later where the effective zone type is
//     known.
//
// Returns the resolved zone type used for auto-create (resolution order:
// resolver call CP → disk_type profile → vm_type profile → config.SDNZoneType)
// and the explicit vnet tag (0 = unset).
func validateCreateNetworkSDNPreflight(cfg *config.CPIConfig, cp map[string]any, zone, vnet string) (string, int, error) {
	if vnet == "" {
		return "", 0, cpierrors.Cloud(
			"create_network: cloud_properties.vnet is required for the SDN path",
		)
	}
	if err := validateVnetName(vnet); err != nil {
		return "", 0, err
	}

	if zone == "" {
		return "", 0, cpierrors.Cloud(
			"create_network: SDN zone is required — set cloud_properties.zone, config sdn_zone, " +
				"or enable sdn_auto_manage_zone",
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
		return "", 0, rErr
	}
	var zoneType string
	if v, ok := r.String("zone_type"); ok {
		zoneType = v
	} else {
		zoneType = cfg.SDNZoneType
	}
	var vnetTag int
	if v, ok := r.Int("vnet_tag"); ok {
		if v < 1 || v > 16777215 {
			return "", 0, cpierrors.Cloud(
				"create_network: cloud_properties.vnet_tag must be within 1..16777215, got %d", v,
			)
		}
		vnetTag = v
	}
	return zoneType, vnetTag, nil
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
