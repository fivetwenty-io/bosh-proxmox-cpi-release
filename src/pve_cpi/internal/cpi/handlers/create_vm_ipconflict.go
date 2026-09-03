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
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
// from create_vm when detectIPConflict returns a non-nil *IPConflict. vlan is
// the L2 domain's VLAN tag (0 = untagged); when non-zero it is named in the
// message so an operator running several VLANs on one trunk bridge can tell
// which domain actually conflicted.
func IPConflictCloudError(conflict *IPConflict, bridge string, vlan int) *cpierrors.Error {
	if vlan != 0 {
		return cpierrors.Cloud(
			"create_vm: IP conflict detected — address %s is already statically assigned "+
				"to VM %d (%s) on bridge/vnet %q vlan %d; "+
				"choose a different IP or remove the conflicting assignment before retrying",
			conflict.IP, conflict.VMID, conflict.Name, bridge, vlan,
		)
	}
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
//     bridge (and vlan) filtering and matches any NIC (wider scan, potentially
//     slower).
//   - vlan: VLAN tag to further restrict the bridge filter to one L2 domain
//     (0 = untagged; ignored when bridge is ""). A NIC matches iff its net{N}
//     bridge= equals bridge AND its tag= equals vlan (absent tag== vlan 0) —
//     see nicTagMatches. This keeps two guests on the same trunk bridge but
//     different VLANs from being reported as conflicting.
//   - excludeVMID: VMID to exclude from the scan. Pass the just-created VM's
//     VMID here so the new VM cannot conflict with its own ipconfig entries.
//     Pass 0 (or any value <= 0) to disable exclusion and scan all VMs.
//
// Algorithm:
//  1. Authoritative per-node listings (pve.ListGuestsAuthoritative) → all
//     (vmid, node) pairs, without the /cluster/resources index lag.
//  2. Per-VM: QEMU().Config(node, vmid) → parse ipconfig{N} for static IPs.
//     Concurrency is bounded to maxIPConflictWorkers goroutines.
//  3. When bridge != "", also parse net{N} keys to restrict to NICs on that
//     (bridge, vlan) L2 domain.
//  4. First conflict found cancels remaining goroutines and is returned.
//
// Error handling:
//   - Transport faults from the enumeration → retriable where applicable.
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
	vlan int,
	excludeVMID int,
) (*IPConflict, error) {
	if len(targetIPs) == 0 {
		return nil, nil
	}

	// Build a lookup set for O(1) target-IP checks. Keys are canonicalized so
	// that two spellings of the same IPv6 address (fd00:0:0::5 and fd00::5)
	// compare equal; IPv4 is unaffected.
	targetSet := make(map[string]struct{}, len(targetIPs))
	for _, ip := range targetIPs {
		if key := canonicalIPString(ip); key != "" {
			targetSet[key] = struct{}{}
		}
	}
	if len(targetSet) == 0 {
		return nil, nil
	}

	// Phase 1: enumerate every cluster guest from authoritative per-node
	// listings, not the /cluster/resources index: the index lags by minutes,
	// and a young VM already holding the target IP is exactly the conflict
	// this guard exists to catch. The enumeration classifies its own failures
	// (retriable for a partial fleet). Tolerant form: an offline member must
	// not block every create_vm, and its guests could not be scanned anyway,
	// because phase 2's config read is served by the guest's own node. The
	// index-fed baseline had the same blind spot (the candidate row survived,
	// its config read failed and was skipped); what must not happen is
	// passing that blind spot off as a clean proof, so a non-empty exclusion
	// is surfaced as a Warn naming the unverified members.
	guests, excludedMembers, listErr := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
	if listErr != nil {
		return nil, cpierrors.Wrap(listErr, "detect_ip_conflict: enumerate cluster guests")
	}
	if len(excludedMembers) > 0 {
		deps.Log(ctx).Warn("detect_ip_conflict: cluster members reported offline were not scanned;"+
			" static IPs owned by their guests are unverified",
			log.String("nodes", strings.Join(excludedMembers, ",")))
	}
	if len(guests) == 0 {
		return nil, nil
	}

	type vmEntry struct {
		VMID int64
		Node string
		Name string
	}

	entries := make([]vmEntry, 0, len(guests))
	for _, g := range guests {
		entries = append(entries, vmEntry{VMID: int64(g.VMID), Node: g.Node, Name: g.Name})
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
			bridgeNICs := nicIndicesOnBridge(cfg, bridge, vlan)

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
// key in cfg both references bridge (nicIsOnBridge) AND matches vlan
// (nicTagMatches) — i.e. sits in the same (bridge, vlan) L2 domain. When
// bridge is empty, returns nil to signal "no bridge filter" — callers treat
// nil as "all indices match" (vlan is ignored in that case: an empty bridge
// disables domain filtering entirely, not just the bridge half of it).
func nicIndicesOnBridge(cfg map[string]any, bridge string, vlan int) map[int]struct{} {
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
		netStr, ok := pve.ConfigStringValue(val)
		if !ok {
			continue
		}
		if nicIsOnBridge(netStr, bridge) && nicTagMatches(netStr, vlan) {
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

// nicTagMatches reports whether netVal's tag= (VLAN ID) segment matches vlan,
// under a deliberately asymmetric rule:
//
//   - netVal carries an explicit tag= segment (hasTag == true): exact match
//     only — matches iff tag == vlan. Two guests both stating their VLAN
//     explicitly are on the same L2 domain iff the tags agree; this mirrors
//     the same rule create_vm attaches NICs under (resolveNICBridgeAndVLAN /
//     configureNICs' ",tag=<n>" assembly).
//   - netVal carries no tag= segment (or an unparseable one): matches ANY
//     vlan, including 0. On a VLAN-aware trunk bridge with a configured
//     native VLAN N, an untagged existing NIC and a new NIC requesting
//     vlan: N are in the SAME L2 domain — but which VLAN an untagged port's
//     native VLAN actually is cannot be determined from the VM config
//     alone. A conflict GUARD must stay conservative in the face of that
//     ambiguity (false positive: an extra "possible conflict" the operator
//     can rule out; false negative: a real duplicate IP silently missed),
//     so an untagged existing NIC is treated as a wildcard rather than as
//     "vlan 0 only" — this restores the pre-tag-aware bridge-only
//     conservatism exactly where the ambiguity exists.
func nicTagMatches(netVal string, vlan int) bool {
	tag := 0
	hasTag := false
	for _, seg := range strings.Split(netVal, ",") {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], "tag") {
			if n, err := strconv.Atoi(strings.TrimSpace(kv[1])); err == nil {
				tag = n
				hasTag = true
			}
			break
		}
	}
	if !hasTag {
		return true
	}
	return tag == vlan
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
	// Scan in key order. A VM that collides on both its IPv4 and its IPv6
	// address has two answers, and map iteration would report a different
	// one each run.
	keys := make([]string, 0, len(cfg))
	for key := range cfg {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := cfg[key]
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

		ipStr, ok := pve.ConfigStringValue(val)
		if !ok {
			continue
		}
		// A NIC can hold one address per family, so check both.
		for _, ip := range extractStaticIPs(ipStr) {
			key := canonicalIPString(ip)
			if key == "" {
				continue
			}
			if _, hit := targetSet[key]; hit {
				return &IPConflict{
					VMID: vmid,
					Name: vmName,
					IP:   ip,
				}
			}
		}
	}
	return nil
}

// extractStaticIPs parses a PVE ipconfig value and returns every bare static
// address it configures — at most one per family, IPv4 first. Dynamic entries
// ("dhcp", "auto", "auto6") and unrecognised values contribute nothing, so a
// fully dynamic NIC yields an empty slice.
//
// PVE ipconfig format examples:
//
//	"ip=dhcp"                                  → []            (dynamic)
//	"ip=10.0.0.5/24,gw=10.0.0.1"               → ["10.0.0.5"]
//	"ip6=auto"                                 → []            (dynamic)
//	"ip=10.0.0.5/24"                           → ["10.0.0.5"]
//	"ip=10.0.0.5"                              → ["10.0.0.5"]  (no prefix, still static)
//	"ip=10.0.0.5/24,ip6=fd00::5/64,gw6=fd00::1" → ["10.0.0.5", "fd00::5"]
func extractStaticIPs(ipconfig string) []string {
	var v4, v6 string
	for _, seg := range strings.Split(ipconfig, ",") {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		if key != "ip" && key != "ip6" {
			continue
		}
		val := strings.TrimSpace(kv[1])
		if val == "" ||
			strings.EqualFold(val, "dhcp") ||
			strings.EqualFold(val, "auto") ||
			strings.EqualFold(val, "auto6") {
			continue
		}
		// Strip CIDR prefix when present.
		if idx := strings.Index(val, "/"); idx >= 0 {
			val = val[:idx]
		}
		if val == "" {
			continue
		}
		if key == "ip" {
			v4 = val
		} else {
			v6 = val
		}
	}
	out := make([]string, 0, 2)
	if v4 != "" {
		out = append(out, v4)
	}
	if v6 != "" {
		out = append(out, v6)
	}
	return out
}

// canonicalIPString reduces an address string to the form canonicalIP produces,
// so two spellings of one address compare equal. Returns "" when s is not an
// IP address at all.
func canonicalIPString(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return ""
	}
	return canonicalIP(ip)
}
