// delete_vm_routes.go — advertised-route SDN subnet cleanup on delete_vm.
//
// create_vm stamps an advrt-<vnet>-<hash8> provenance tag per advertised
// route (see advrt_provenance.go). When the VM is deleted, each route's
// subnet is removed from its vnet — unless another live VM carries the same
// tag (routers deployed in pairs share routes; the last one out cleans up).
//
// Failure contract: ENTIRELY fail-open. delete_vm has already destroyed the
// VM by the time this runs; a cleanup failure must never fail the delete.
// Every error path logs a Warn naming the leftover coordinates and returns.
// Concurrent deletes of two route-sharing VMs can each see the other alive
// and both skip — a leaked subnet (logged), never a wrong delete.
package handlers

import (
	"context"
	"errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cleanupAdvertisedRoutes removes the SDN subnets recorded in the deleted
// VM's advrt provenance tags. tagsRaw is the VM's tag string captured from
// the cluster scan BEFORE the destroy (the VM no longer exists when this
// runs). All I/O uses a detached context so caller cancellation cannot leave
// a half-applied SDN config.
func cleanupAdvertisedRoutes(ctx context.Context, deps Deps, vmid int, tagsRaw string, logger *log.Logger) {
	refs := parseAdvertisedRouteTags(tagsRaw)
	if len(refs) == 0 {
		return
	}
	// Bounded: this cleanup is documented as entirely fail-open, so a tight
	// deadline costs nothing while keeping a hung SDN mutation from pinning
	// the handler past its operation budget.
	opCtx, opCancel := detachedContext(ctx, rollbackCleanupTimeout)
	defer opCancel()
	clusterSvc := deps.PVE.Cluster()

	shared := advrtTagsHeldByOthers(opCtx, deps, vmid, logger)
	if shared == nil {
		// Refcount scan failed — cannot prove sole ownership of any route.
		// Fail open: leave every subnet in place.
		return
	}

	deletedAny := false
	for _, ref := range refs {
		if shared[ref.tag] {
			logger.Debug("delete_vm: advertised-route subnet retained — shared with another VM",
				log.String("tag", ref.tag))
			continue
		}
		if deleteAdvertisedRouteSubnet(opCtx, deps, ref, logger) {
			deletedAny = true
		}
	}
	if !deletedAny {
		return
	}
	if aErr := applySDN(opCtx, deps, clusterSvc, "delete_vm: apply SDN after advertised-route cleanup"); aErr != nil {
		logger.Warn("delete_vm: advertised-route cleanup apply failed — subnet deletions stay pending",
			log.Err(aErr))
	}
}

// advrtTagsHeldByOthers returns the set of advrt tags carried by any OTHER
// VM in the cluster. The scan reads authoritative per-node listings
// (ListGuestsAuthoritative), not the /cluster/resources index: the index
// lags by minutes, and a young VM sharing the route tag would be invisible
// to it, turning "sole holder" into a wrong answer that deletes a subnet
// still in use. nil signals the scan failed and callers must not delete
// anything (fail open, leave subnets in place).
func advrtTagsHeldByOthers(ctx context.Context, deps Deps, vmid int, logger *log.Logger) map[string]bool {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, logger)
	if err != nil {
		logger.Warn("delete_vm: advertised-route cleanup skipped — could not list cluster VMs for refcount",
			log.Err(err))
		return nil
	}
	shared := make(map[string]bool)
	for _, g := range guests {
		if g.VMID == vmid {
			continue
		}
		for _, ref := range parseAdvertisedRouteTags(g.Tags) {
			shared[ref.tag] = true
		}
	}
	return shared
}

// deleteAdvertisedRouteSubnet finds the subnet whose hash matches ref within
// ref.vnet and deletes it. Returns true when a subnet was actually deleted
// (the caller then commits with one applySDN). The CIDR is never decoded
// from the hash: each existing subnet's cidr is re-hashed and compared, so
// any CIDR shape (IPv4/IPv6) matches losslessly. Vnet-gone and subnet-gone
// are idempotent successes.
func deleteAdvertisedRouteSubnet(ctx context.Context, deps Deps, ref advrtTagRef, logger *log.Logger) bool {
	subnets, listErr := pve.ListSDNVnetSubnets(ctx, deps.PVE, ref.vnet)
	if listErr != nil {
		if errors.Is(listErr, pve.ErrSDNNotFound) {
			// Vnet already gone — nothing to clean.
			return false
		}
		logger.Warn("delete_vm: advertised-route cleanup — could not list subnets; subnet stays",
			log.String("vnet", ref.vnet), log.String("tag", ref.tag), log.Err(listErr))
		return false
	}
	deleted := false
	for _, s := range subnets {
		cidr := s.Cidr
		if cidr == "" || advrtHash8(ref.vnet, cidr) != ref.hash8 {
			continue
		}
		if dErr := pve.DeleteSDNVnetSubnet(ctx, deps.PVE, ref.vnet, s.Subnet); dErr != nil {
			if errors.Is(dErr, pve.ErrSDNNotFound) {
				continue
			}
			logger.Warn("delete_vm: advertised-route subnet delete failed — operator cleanup required",
				log.String("vnet", ref.vnet), log.String("subnet", s.Subnet), log.Err(dErr))
			continue
		}
		logger.Info("delete_vm: advertised-route subnet removed",
			log.String("vnet", ref.vnet), log.String("subnet", s.Subnet))
		deleted = true
	}
	return deleted
}
