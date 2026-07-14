package handlers

import (
	"context"
	"fmt"
	"net"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

// AdvertisedRoute describes a single SDN subnet entry to create for a
// router or NAT VM. The CPI calls POST /cluster/sdn/vnets/{VNet}/subnets with
// Destination as the subnet CIDR, then calls applySDN (PUT /cluster/sdn) to
// commit the change to the FRR-managed logical-router fabric.
//
// Destination maps to the CreateSdnVnetsSubnetsParams.Subnet field (the PVE
// SDN subnet object identifier, which is the CIDR string). Type is always
// "subnet" (PVE requirement).
//
// Limitation: route injection only reaches a routing control plane in an
// EVPN zone — the sole zone type with one (FRR/BGP). Static logical-router
// routes that do not map to a full subnet CIDR are not supported. When the
// vnet's zone is not EVPN (e.g. vxlan/simple), PVE may accept the subnet
// create but no route is injected — the CPI logs a warning and continues
// (fail-open) rather than reporting success it cannot deliver.
type AdvertisedRoute struct {
	// VNet is the PVE SDN vnet name (1–8 lowercase alphanumeric characters).
	VNet string `json:"vnet"`
	// Destination is the CIDR that should be routed via this VM's interface.
	// Must be a valid IPv4 or IPv6 CIDR (e.g. "10.64.0.0/16").
	Destination string `json:"destination"`
}

// sdnSubnetType is the PVE subnet type required for SDN subnet creation.
// PVE validates this field server-side; passing any other value results in
// a 400 error.
const sdnSubnetType = "subnet"

// evpnZoneType is the only SDN zone type with a routing control plane
// (FRR/BGP); advertised routes injected into any other zone type never
// reach a router. Matches the delete_network EVPN guard spelling.
const evpnZoneType = "evpn"

// validateAdvertisedRoutes checks all entries in routes for a non-empty vnet
// name and a well-formed CIDR destination. Returns a non-retriable CloudError
// on the first invalid entry. Called from parseCreateVMArgs before any VM is
// created so malformed input never produces an orphan VM.
func validateAdvertisedRoutes(routes []AdvertisedRoute) error {
	for i, r := range routes {
		if strings.TrimSpace(r.VNet) == "" {
			return cpierrors.Cloud(
				"create_vm: advertised_routes[%d].vnet must not be empty", i)
		}
		if !vnetNameRE.MatchString(r.VNet) {
			return cpierrors.Cloud(
				"create_vm: advertised_routes[%d].vnet %q is invalid — must be 1–8 lowercase alphanumeric characters [a-z0-9]",
				i, r.VNet)
		}
		if err := validateCIDR(r.Destination); err != nil {
			return cpierrors.Cloud(
				"create_vm: advertised_routes[%d].destination %q: %s", i, r.Destination, err.Error())
		}
	}
	return nil
}

// validateCIDR returns an error when s is not a valid IPv4 or IPv6 CIDR.
func validateCIDR(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("destination must not be empty")
	}
	_, _, err := net.ParseCIDR(s)
	if err != nil {
		return fmt.Errorf("not a valid CIDR: %w", err)
	}
	return nil
}

// applyAdvertisedRoutes injects an SDN subnet for each entry in routes and
// calls applySDN once after all subnets are created to commit the changes.
// Returns nil immediately when routes is empty (byte-identical path: no API
// calls made).
//
// Before injecting, each distinct vnet's zone type is checked: a non-EVPN
// zone gets a Warn — PVE accepts the subnet there, but no route reaches a
// routing control plane. The check is fail-open (lookup errors are logged at
// Debug and skipped) and never blocks injection.
//
// Failure contract:
//   - If CreateSdnVnetsSubnets or applySDN returns an error, all subnets that
//     were created during this call are removed on a best-effort basis via
//     DeleteSdnVnetsSubnets. If removal fails, a warning is logged naming the
//     leftover subnet so the operator can clean up manually.
//   - PVE API errors are wrapped retriable via pve.WrapError so the director
//     can re-drive transient failures.
//   - The caller (createVM/createVMWithFallback) rolls back the VM on any
//     non-nil return from this function.
func applyAdvertisedRoutes(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	routes []AdvertisedRoute,
	logger *log.Logger,
) error {
	if len(routes) == 0 {
		return nil
	}

	warnNonEVPNRouteZones(ctx, deps, vmid, routes, logger)

	clusterSvc := deps.PVE.Cluster()

	// Track subnets created in this call for rollback on failure.
	type injected struct {
		vnet   string
		subnet string
	}
	done := make([]injected, 0, len(routes))

	rollbackSubnets := func(causeErr error) error {
		for _, s := range done {
			if delErr := clusterSvc.DeleteSdnVnetsSubnets(ctx, s.vnet, s.subnet, nil); delErr != nil {
				// Removal failed — log with leftover coordinates for operator action.
				if logger != nil {
					logger.Warn("create_vm: advertised_routes rollback: leftover SDN subnet after failure; operator cleanup required",
						log.Int(metadataKeyVMID, vmid),
						log.String("vnet", s.vnet),
						log.String("subnet", s.subnet),
						log.Err(delErr),
					)
				}
			}
		}
		return causeErr
	}

	for _, r := range routes {
		if err := createSDNSubnet(ctx, clusterSvc, r.VNet, r.Destination); err != nil {
			// Treat "already exists" (conflict) as idempotent success — the
			// subnet is already present in the vnet, which is the desired state.
			// This matches the ipset-create path (create_vm_vip.go:278) and the
			// vnet-create path (create_network.go). Without this guard, a director
			// retry after a partial-create+failed-rollback would wedge permanently:
			// every subsequent attempt would find the leftover subnet and fail.
			if isSDNConflict(err) {
				if logger != nil {
					logger.Debug("create_vm: advertised_routes: subnet already exists (idempotent)",
						log.Int(metadataKeyVMID, vmid),
						log.String("vnet", r.VNet),
						log.String("subnet", r.Destination),
					)
				}
				// Do not add to `done` — a pre-existing subnet must not be
				// deleted by this call's rollback path (we did not create it).
				continue
			}
			wrappedErr := cpierrors.Wrap(pve.WrapError(err),
				fmt.Sprintf("create_vm: advertised_routes: create subnet %q on vnet %q vmid=%d",
					r.Destination, r.VNet, vmid))
			return rollbackSubnets(wrappedErr)
		}
		done = append(done, injected{vnet: r.VNet, subnet: r.Destination})
	}

	// Commit all staged subnet additions in one apply. A failure here still
	// triggers subnet rollback so no dangling SDN state is left.
	if err := applySDN(ctx, deps, clusterSvc, fmt.Sprintf("create_vm: advertised_routes vmid=%d", vmid)); err != nil {
		return rollbackSubnets(cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("create_vm: advertised_routes: applySDN vmid=%d", vmid)))
	}

	if logger != nil {
		logger.Info("create_vm: advertised_routes: SDN subnets injected",
			log.Int(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Int("count", len(routes)),
		)
	}
	return nil
}

// warnNonEVPNRouteZones resolves each distinct vnet in routes to its zone and
// the zone to its type, then warns when the type is not EVPN — the only zone
// type where an advertised route reaches a routing control plane. Entirely
// fail-open: a vnet or zone lookup error is logged at Debug and skipped, so
// this check can never block or fail create_vm.
func warnNonEVPNRouteZones(
	ctx context.Context,
	deps Deps,
	vmid int,
	routes []AdvertisedRoute,
	logger *log.Logger,
) {
	if logger == nil {
		return
	}
	seen := make(map[string]bool, len(routes))
	for _, r := range routes {
		if seen[r.VNet] {
			continue
		}
		seen[r.VNet] = true

		vnetInfo, err := pve.GetSDNVnet(ctx, deps.PVE, r.VNet)
		if err != nil || vnetInfo == nil || vnetInfo.Zone == "" {
			logger.Debug("create_vm: advertised_routes: zone-type check skipped — could not resolve vnet's zone",
				log.Int(metadataKeyVMID, vmid),
				log.String("vnet", r.VNet),
				log.Err(err),
			)
			continue
		}
		zoneInfo, err := pve.GetSDNZone(ctx, deps.PVE, vnetInfo.Zone)
		if err != nil || zoneInfo == nil {
			logger.Debug("create_vm: advertised_routes: zone-type check skipped — could not read zone type",
				log.Int(metadataKeyVMID, vmid),
				log.String("vnet", r.VNet),
				log.String("zone", vnetInfo.Zone),
				log.Err(err),
			)
			continue
		}
		if !strings.EqualFold(zoneInfo.Type, evpnZoneType) {
			logger.Warn("create_vm: advertised_routes: zone has no routing control plane — subnet accepted but no route is injected; use an evpn zone for real forwarding",
				log.Int(metadataKeyVMID, vmid),
				log.String("vnet", r.VNet),
				log.String("zone", vnetInfo.Zone),
				log.String("zone_type", zoneInfo.Type),
			)
		}
	}
}

// createSDNSubnet posts a single subnet to the named vnet. Extracted so tests
// can verify the exact params without re-testing the full applyAdvertisedRoutes
// state-machine.
func createSDNSubnet(ctx context.Context, clusterSvc sdkcluster.Service, vnet, destination string) error {
	return clusterSvc.CreateSdnVnetsSubnets(ctx, vnet, &sdkcluster.CreateSdnVnetsSubnetsParams{
		Subnet: destination,
		Type:   sdnSubnetType,
	})
}
