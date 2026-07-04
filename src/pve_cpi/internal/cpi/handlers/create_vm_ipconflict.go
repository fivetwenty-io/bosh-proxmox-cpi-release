// IP-conflict detector for create_vm pre-create guard.
//
// Scope and honest limitations:
//
//   - Detects only statically-configured IPs stored in VM config ipconfig{N} keys.
//   - Cannot detect DHCP-assigned addresses: PVE provides no ARP/lease-table API.
//   - Cannot detect IPs on physical hosts, containers, or devices outside PVE's
//     management plane.
//   - Bridge-filter is best-effort: if a VM config has no bridge in its net{N} key
//     (unusual but possible), that NIC is skipped for bridge-specific scans.
//
// The function is safe to call concurrently; it holds no shared mutable state.
package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"golang.org/x/sync/errgroup"
)

// IPConflict describes a VM whose static ipconfig already claims one of the
// requested target IPs on the scanned bridge or vnet.
type IPConflict struct {
	// VMID is the PVE VM identifier of the conflicting guest.
	VMID int
	// Name is the VM name from the config (empty string when PVE returns no name field).
	Name string
	// IP is the conflicting IP address as found in the VM's ipconfig (no CIDR prefix).
	IP string
}

// IPConflictCloudError formats a CloudError whose message names the conflicting
// guest so the BOSH director log contains actionable information. Call this
// from create_vm when detectIPConflict returns a non-nil *IPConflict.
func IPConflictCloudError(conflict *IPConflict, bridge string) *cpierrors.Error {
	return cpierrors.Cloud(
		"create_vm: IP conflict detected — address %s is already statically assigned "+
			"to VM %d (%s) on bridge/vnet %q; "+
			"choose a different IP or remove the conflicting assignment before retrying",
		conflict.IP, conflict.VMID, conflict.Name, bridge,
	)
}

// detectIPConflict scans all QEMU VMs in the cluster and returns the first
// *IPConflict found where a VM's static ipconfig{N} entry claims an IP in
// targetIPs on the named bridge. Returns nil, nil when no conflict exists.
//
// Parameters:
//   - ctx: cancellation context; cancelled context stops in-flight goroutines.
//   - deps: handler dependencies (PVE client, logger).
//   - targetIPs: the IP addresses (plain, no CIDR prefix) to check; e.g. ["10.0.0.5"].
//     Empty or nil slice returns nil, nil immediately.
//   - bridge: optional bridge/vnet name to filter NICs; empty string disables
//     bridge filtering and matches any NIC (wider scan, potentially slower).
//   - excludeVMID: VMID to exclude from the scan. Pass the just-created VM's
//     VMID here so the new VM cannot conflict with its own ipconfig entries.
//     Pass 0 (or any value <= 0) to disable exclusion and scan all VMs.
//
// Algorithm:
//  1. cluster.ListResources(type=vm) → all (vmid, node) pairs.
//  2. Per-VM: QEMU().Config(node, vmid) → parse ipconfig{N} for static IPs.
//     Concurrency is bounded to maxIPConflictWorkers goroutines.
//  3. When bridge != "", also parse net{N} keys to restrict to NICs on that bridge.
//  4. First conflict found cancels remaining goroutines and is returned.
//
// Error handling:
//   - Transport faults from ListResources → pve.WrapError → retriable where applicable.
//   - Per-VM Config fetch errors → logged at debug, VM skipped (same policy as
//     findVMsHostingDisk; templates and ephemeral VMs often return errors here).
//   - First non-skip error from a worker goroutine propagates to the caller.
//
//nolint:gocognit // Structured scan: list → parallel per-VM Config fetch → IP parse. Cognitive load is inherent to the three-phase algorithm, not to local complexity.
func detectIPConflict(
	ctx context.Context,
	deps Deps,
	targetIPs []string,
	bridge string,
	excludeVMID int,
) (*IPConflict, error) {
	if len(targetIPs) == 0 {
		return nil, nil
	}

	// Build a lookup set for O(1) target-IP checks.
	targetSet := make(map[string]struct{}, len(targetIPs))
	for _, ip := range targetIPs {
		if ip != "" {
			targetSet[ip] = struct{}{}
		}
	}
	if len(targetSet) == 0 {
		return nil, nil
	}

	// Phase 1: list all VM resources from the cluster.
	typeStr := "vm"
	var resources *sdkcluster.ListResourcesResponse
	listErr := pve.RetryOnTransient(ctx, deps.Log(ctx), "detect_ip_conflict_list_resources", 0, func() error {
		var inner error
		resources, inner = deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if listErr != nil {
		return nil, cpierrors.Wrap(
			pve.WrapError(listErr),
			"detect_ip_conflict: list cluster VM resources",
		)
	}
	if resources == nil || len(*resources) == 0 {
		return nil, nil
	}

	type vmEntry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Name string `json:"name"`
	}

	// Parse resource list into typed entries; skip malformed rows.
	entries := make([]vmEntry, 0, len(*resources))
	for _, raw := range *resources {
		var e vmEntry
		if err := json.Unmarshal(raw, &e); err != nil || e.VMID <= 0 {
			continue
		}
		if e.Node == "" {
			e.Node = deps.Config.Node
		}
		if e.Node == "" {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	// Phase 2: parallel per-VM config fetch + ipconfig parse.
	// Use errgroup with a bounded semaphore channel to cap concurrent PVE API calls.
	const maxIPConflictWorkers = 16
	sem := make(chan struct{}, maxIPConflictWorkers)

	// conflictMu guards first-conflict write; errgroup cancels context on first error.
	var (
		conflictMu sync.Mutex
		found      *IPConflict
	)

	// scanCtx is cancelled either by the parent or as soon as a conflict is found
	// so remaining goroutines abort early.
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()

	g, scanCtx := errgroup.WithContext(scanCtx)

	for i := range entries {
		entry := entries[i] // capture loop variable

		g.Go(func() error {
			// Acquire semaphore slot.
			select {
			case sem <- struct{}{}:
			case <-scanCtx.Done():
				return nil
			}
			defer func() { <-sem }()

			// Abort if a conflict was already found by a sibling goroutine.
			select {
			case <-scanCtx.Done():
				return nil
			default:
			}

			vmid := int(entry.VMID)

			// Skip the just-created VM so it cannot conflict with its own
			// ipconfig entries. excludeVMID <= 0 disables this exclusion.
			if excludeVMID > 0 && vmid == excludeVMID {
				return nil
			}

			cfg, cfgErr := deps.PVE.QEMU().Config(scanCtx, entry.Node, vmid)
			if cfgErr != nil {
				// Skip VMs whose config cannot be fetched (templates, ephemeral).
				// This mirrors the skip policy in findVMsHostingDisk.
				deps.Log(scanCtx).Debug(
					"detect_ip_conflict: skipping VM config fetch error",
					log.String("node", entry.Node),
					log.Int("vmid", vmid),
					log.Err(cfgErr),
				)
				return nil
			}
			if cfg == nil {
				return nil
			}

			// Determine which NIC indices are on the target bridge (when filtering).
			bridgeNICs := nicIndicesOnBridge(cfg, bridge)

			// Scan ipconfig{N} for static IPs.
			conflict := parseIPConflict(cfg, targetSet, bridgeNICs, bridge, vmid, entry.Name)
			if conflict == nil {
				return nil
			}

			conflictMu.Lock()
			if found == nil {
				found = conflict
				cancelScan() // signal siblings to abort
			}
			conflictMu.Unlock()
			return nil
		})
	}

	if waitErr := g.Wait(); waitErr != nil {
		return nil, cpierrors.Wrap(pve.WrapError(waitErr), "detect_ip_conflict: VM config scan")
	}

	return found, nil
}

// nicIndicesOnBridge returns the set of NIC indices (0, 1, ...) whose net{N}
// key in cfg contains "bridge=<targetBridge>" or "bridge=<targetBridge>,".
// When bridge is empty, returns nil to signal "no bridge filter" — callers
// treat nil as "all indices match".
func nicIndicesOnBridge(cfg map[string]any, bridge string) map[int]struct{} {
	if bridge == "" {
		return nil // no filter
	}
	result := make(map[int]struct{})
	for key, val := range cfg {
		if !strings.HasPrefix(key, "net") {
			continue
		}
		idx, err := strconv.Atoi(key[3:])
		if err != nil {
			continue
		}
		netStr, ok := val.(string)
		if !ok {
			continue
		}
		if nicIsOnBridge(netStr, bridge) {
			result[idx] = struct{}{}
		}
	}
	return result
}

// nicIsOnBridge reports whether netVal (a raw PVE net{N} string such as
// "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,firewall=1") references the named bridge.
func nicIsOnBridge(netVal, bridge string) bool {
	// Parse comma-separated key=value segments.
	for _, seg := range strings.Split(netVal, ",") {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], "bridge") && strings.EqualFold(kv[1], bridge) {
			return true
		}
	}
	return false
}

// parseIPConflict inspects ipconfig{N} entries in cfg for static IPs that
// collide with targetSet. bridgeNICs gates which NIC indices are considered:
//   - nil → no bridge filter, all ipconfig{N} entries are checked.
//   - non-nil → only indices present in bridgeNICs are checked.
//
// Returns the first *IPConflict found, or nil when the VM is clean.
func parseIPConflict(
	cfg map[string]any,
	targetSet map[string]struct{},
	bridgeNICs map[int]struct{},
	bridge string, // informational only
	vmid int,
	vmName string,
) *IPConflict {
	_ = bridge // used only for clarity in the caller; logged via IPConflictCloudError
	for key, val := range cfg {
		if !strings.HasPrefix(key, "ipconfig") {
			continue
		}
		suffix := key[len("ipconfig"):]
		idx, err := strconv.Atoi(suffix)
		if err != nil {
			// Malformed ipconfig key — skip silently.
			continue
		}

		// Apply bridge filter when active.
		if bridgeNICs != nil {
			if _, onBridge := bridgeNICs[idx]; !onBridge {
				continue
			}
		}

		ipStr, ok := val.(string)
		if !ok {
			continue
		}
		ip := extractStaticIP(ipStr)
		if ip == "" {
			continue
		}
		if _, hit := targetSet[ip]; hit {
			return &IPConflict{
				VMID: vmid,
				Name: vmName,
				IP:   ip,
			}
		}
	}
	return nil
}

// extractStaticIP parses a PVE ipconfig value and returns the bare IP address
// when the entry is static (not "dhcp" or "auto6"). Returns "" for dynamic or
// unrecognised entries.
//
// PVE ipconfig format examples:
//
//	"ip=dhcp"                        → "" (dynamic)
//	"ip=10.0.0.5/24,gw=10.0.0.1"   → "10.0.0.5"
//	"ip6=auto"                       → "" (dynamic)
//	"ip=10.0.0.5/24"                → "10.0.0.5"
//	"ip=10.0.0.5"                   → "10.0.0.5" (no prefix, still static)
func extractStaticIP(ipconfig string) string {
	for _, seg := range strings.Split(ipconfig, ",") {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])

		// Only consider "ip" (IPv4); skip "ip6" (different conflict domain).
		if key != "ip" {
			continue
		}
		if val == "" || strings.EqualFold(val, "dhcp") {
			return ""
		}
		// Strip CIDR prefix when present.
		if idx := strings.Index(val, "/"); idx >= 0 {
			return val[:idx]
		}
		return val
	}
	return ""
}
