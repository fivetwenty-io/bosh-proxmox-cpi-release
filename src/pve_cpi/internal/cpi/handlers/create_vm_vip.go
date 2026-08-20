package handlers

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// normalizeVIPEntry normalizes a single VIP entry to canonical CIDR notation.
//
// Accepted inputs:
//   - bare IPv4 address ("10.0.0.5")      → "10.0.0.5/32"
//   - bare IPv6 address ("::1")            → "::1/128"
//   - valid CIDR ("10.0.0.0/24")          → canonical form ("10.0.0.0/24")
//   - empty string                         → error
//   - unparseable string ("bad")           → error
//
// For CIDRs, net.ParseCIDR is used which returns the network address; since
// the operator intends to allow a specific host address (not the network base),
// we preserve the original host IP with the parsed prefix length.
func normalizeVIPEntry(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("allowed_address_pairs entry must not be empty")
	}

	// Try CIDR first (contains "/").
	if strings.Contains(trimmed, "/") {
		ip, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			return "", fmt.Errorf("allowed_address_pairs entry %q is not a valid IP or CIDR: %w", raw, err)
		}
		bits, _ := ipNet.Mask.Size()
		return fmt.Sprintf("%s/%d", ip.String(), bits), nil
	}

	// Try bare IP address.
	ip := net.ParseIP(trimmed)
	if ip == nil {
		return "", fmt.Errorf("allowed_address_pairs entry %q is not a valid IP or CIDR", raw)
	}
	if ip.To4() != nil {
		return ip.String() + "/32", nil
	}
	return ip.String() + "/128", nil
}

// parseVIPEntries reads cloud_properties["allowed_address_pairs"] and returns
// a deduplicated, normalized slice of CIDR strings. First-seen order preserved
// for determinism.
//
// Returns (nil, nil) when the key is absent or nil — the byte-identical path.
// Returns an error on type mismatch (non-string element) or invalid IP/CIDR.
func parseVIPEntries(cp map[string]any) ([]string, error) {
	if cp == nil {
		return nil, nil
	}
	raw, ok := cp["allowed_address_pairs"]
	if !ok || raw == nil {
		return nil, nil
	}

	var strs []string
	switch v := raw.(type) {
	case []string:
		strs = v
	case []any:
		strs = make([]string, 0, len(v))
		for idx, elem := range v {
			s, ok := elem.(string)
			if !ok {
				return nil, fmt.Errorf("allowed_address_pairs entry must be string (index %d got %T)", idx, elem)
			}
			strs = append(strs, s)
		}
	default:
		return nil, fmt.Errorf("allowed_address_pairs must be a list of strings (got %T)", raw)
	}

	seen := make(map[string]struct{}, len(strs))
	out := make([]string, 0, len(strs))
	for _, s := range strs {
		normalized, err := normalizeVIPEntry(s)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// validateVIPAllowedAddressPairs performs early, pre-mutation validation of all
// per-NIC allowed_address_pairs entries across every network in the manifest.
// On any malformed entry it returns a non-retriable cpierrors.Cloud that names
// the network and bad value. No PVE calls are made.
func validateVIPAllowedAddressPairs(networks map[string]createVMNetworkSpec) error {
	names := sortedNetworkNames(networks)
	for _, name := range names {
		spec := networks[name]
		_, err := parseVIPEntries(spec.CloudProperties)
		if err != nil {
			return cpierrors.Cloud(
				"create_vm: network %q has invalid allowed_address_pairs: %v", name, err)
		}
	}
	return nil
}

// nicIsStatic reports whether a NIC spec represents a static (manual) IP
// assignment — type=="manual", IP non-empty, and not literally "dhcp".
func nicIsStatic(spec createVMNetworkSpec) bool {
	return strings.EqualFold(spec.Type, nicTypeManual) &&
		spec.IP != "" &&
		!strings.EqualFold(spec.IP, "dhcp")
}

// nicFirewallEnabled reports whether the per-NIC firewall flag is enabled for
// the given spec, mirroring the resolution at create_vm.go:2492-2498.
// Global cfg.VMFirewallEnabled() is the base; per-NIC cloud_properties["firewall"]
// bool overrides it.
func nicFirewallEnabled(spec createVMNetworkSpec, cfg *config.CPIConfig) bool {
	enabled := cfg.VMFirewallEnabled()
	if cp, ok := spec.CloudProperties["firewall"].(bool); ok {
		enabled = cp
	}
	return enabled
}

// nicState captures resolved per-NIC properties for a single VIP-apply pass.
type nicState struct {
	idx       int
	fw        bool
	static    bool
	vips      []string // normalized CIDRs; nil if none
	ipForward bool     // true when cloud_properties.ip_forwarding=true
	// ips holds every static address the NIC carries, in NIC-member order.
	// A plain NIC has one; a dual-stack NIC (nic_group) has its IPv4 and
	// IPv6 address, and both must be allowed through ipfilter.
	ips []string
}

// applyVIPAllowedAddressPairs seeds PVE ipfilter ipsets for every firewalled NIC
// and enables the VM-level ipfilter option. It is called after the VM is started
// (step 8c) and is best-effort: every PVE API failure is logged as a warning and
// returns nil — the VM is left working with ipfilter OFF rather than risking
// lockout from an incomplete allowlist.
//
// Algorithm (strict ordering guarantees no lockout):
//  1. Enumerate NICs in planNICs order; compute per-NIC fw/static/vips as the
//     union over the networks sharing each NIC.
//  2. If no NIC has any VIP entries → return nil (byte-identical path; no PVE calls).
//  3. Warn for any NIC that carries VIPs but has firewall disabled (ipfilter would
//     not apply to it; operator probably misconfigured).
//  4. Safety guard: if ANY firewalled NIC is DHCP/dynamic → warn + return nil.
//     Enabling VM-global ipfilter without knowing that NIC's IP would lock it out.
//  5. For each firewalled NIC: build entry list = {its own addresses} ∪ vips, dedup,
//     stable order. Create ipset "ipfilter-net{N}"; tolerate "already exists".
//     Add each entry via CreateQemuFirewallIpset2; any error → warn + return nil.
//  6. After ALL firewalled NICs are fully seeded: UpdateQemuFirewallOptions{Ipfilter:&true}.
//     Error → warn + return nil. Success → log.Info.
//  7. Return nil (always — only validateVIPAllowedAddressPairs returns fatal errors).
//
//nolint:unparam // error return is always nil by design: best-effort, fail-open.
func applyVIPAllowedAddressPairs(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	networks map[string]createVMNetworkSpec,
	logger *log.Logger,
) error {
	// Same NIC plan configureNICs used — ipfilter ipsets are named after the
	// NIC index, so a nic_group must not be allowed to shift them.
	plan := planNICs(networks)
	vmidStr := strconv.Itoa(vmid)
	nodeSvc := deps.PVE.Nodes()

	// Step 1: compute per-NIC state. Properties of a NIC shared by several
	// networks are the union of its members': the firewall is on if any
	// member enables it, VIP entries accumulate, and every member's address
	// is one the NIC legitimately sends from.
	states := make([]nicState, 0, len(plan))
	for _, entry := range plan {
		state := nicState{idx: entry.index}
		for _, name := range entry.names {
			spec := networks[name]
			vips, err := parseVIPEntries(spec.CloudProperties)
			if err != nil {
				// Entry already validated early — this is a defensive path; treat as
				// warn + skip-all (fail-open) rather than surface a post-mutation error.
				logger.Warn("create_vm: applyVIPAllowedAddressPairs: unexpected parse error (fail-open)",
					log.Int(metadataKeyVMID, vmid), log.String("net", name), log.Err(err))
				return nil
			}
			state.vips = append(state.vips, vips...)
			if nicFirewallEnabled(spec, deps.Config) {
				state.fw = true
			}
			if nicIPForwardingEnabled(spec.CloudProperties) {
				state.ipForward = true
			}
			if nicIsStatic(spec) {
				state.static = true
				state.ips = append(state.ips, spec.IP)
			}
		}
		// A NIC counts as static only when no member leaves it dynamic:
		// one DHCP member is enough to make its final address unknowable,
		// which is exactly the lockout case step 4 refuses to risk.
		for _, name := range entry.names {
			spec := networks[name]
			if !nicIsStatic(spec) && !strings.EqualFold(spec.Type, nicTypeVIP) {
				state.static = false
				break
			}
		}
		states = append(states, state)
	}

	// Step 2: bail if no NIC carries any VIP.
	anyVIPs := false
	for i := range states {
		if len(states[i].vips) > 0 {
			anyVIPs = true
			break
		}
	}
	if !anyVIPs {
		return nil
	}

	// Step 3: warn for VIPs on non-firewalled NICs.
	for i := range states {
		if len(states[i].vips) > 0 && !states[i].fw {
			logger.Warn("create_vm: allowed_address_pairs on NIC ignored: firewall not enabled on NIC",
				log.Int(metadataKeyVMID, vmid), log.String("net", fmt.Sprintf("net%d", states[i].idx)))
		}
	}

	// Step 4: safety guard — skip if any firewalled, non-ip_forwarding NIC is
	// DHCP/dynamic OR has an unparseable primary IP. Both cases would cause VM
	// lockout if ipfilter were enabled. ip_forwarding NICs are excluded because
	// they will have firewall=0 applied by applyIPForwarding and are never seeded
	// into an ipset; including them in this guard would incorrectly abort ipfilter
	// for the remaining static NICs.
	for i := range states {
		if !states[i].fw || states[i].ipForward {
			continue
		}
		if !states[i].static {
			logger.Warn("create_vm: VIP ipfilter skipped: firewalled DHCP/dynamic NIC present; cannot safely enable VM-global ipfilter",
				log.Int(metadataKeyVMID, vmid), log.String("net", fmt.Sprintf("net%d", states[i].idx)))
			return nil
		}
		// Validate every address the NIC carries before seeding any ipsets.
		for _, ip := range states[i].ips {
			if _, err := normalizeVIPEntry(ip); err != nil {
				logger.Warn("create_vm: VIP ipfilter skipped: firewalled NIC primary IP unparseable; cannot guarantee allowlist, ipfilter not enabled",
					log.Int(metadataKeyVMID, vmid), log.String("net", fmt.Sprintf("net%d", states[i].idx)),
					log.String("ip", ip), log.Err(err))
				return nil
			}
		}
	}

	// Step 5: seed ipsets for ALL firewalled NICs that are NOT ip_forwarding.
	// NICs with ip_forwarding=true are router/NAT NICs: they forward packets not
	// in their own ipset, so applying ipfilter on them would silently drop
	// forwarded traffic. Such NICs are excluded from both ipset seeding and the
	// fwCount that gates the ipfilter enable.
	fwCount := 0
	for i := range states {
		if !states[i].fw {
			continue
		}
		// Skip ip_forwarding NICs — firewall=0 will be applied by applyIPForwarding;
		// seeding an ipset for them would conflict with that intent.
		if states[i].ipForward {
			continue
		}
		fwCount++
		ipsetName := fmt.Sprintf("ipfilter-net%d", states[i].idx)

		// Build deduplicated entry list: the NIC's own addresses first, then VIPs.
		entries := buildIPSetEntries(states[i].ips, states[i].vips)

		// Create the ipset; tolerate "already exists".
		if createErr := nodeSvc.CreateQemuFirewallIpset(ctx, node, vmidStr,
			&sdknodes.CreateQemuFirewallIpsetParams{Name: ipsetName}); createErr != nil {
			if !strings.Contains(strings.ToLower(createErr.Error()), "already exists") {
				logger.Warn("create_vm: ipset create failed (ipfilter not enabled, fail-open)",
					log.Int(metadataKeyVMID, vmid), log.String("ipset", ipsetName), log.Err(createErr))
				return nil
			}
			logger.Debug("create_vm: ipset already exists, adding entries",
				log.Int(metadataKeyVMID, vmid), log.String("ipset", ipsetName))
		}

		// Add each entry.
		for _, entry := range entries {
			if entryErr := nodeSvc.CreateQemuFirewallIpset2(ctx, node, vmidStr, ipsetName,
				&sdknodes.CreateQemuFirewallIpset2Params{Cidr: entry}); entryErr != nil {
				logger.Warn("create_vm: ipset entry add failed (ipfilter not enabled, fail-open)",
					log.Int(metadataKeyVMID, vmid), log.String("ipset", ipsetName),
					log.String("entry", entry), log.Err(entryErr))
				return nil
			}
		}
	}

	// If there were no firewalled NICs to seed, skip the ipfilter enable — VIPs
	// on non-firewalled NICs have already been warned about in step 3.
	if fwCount == 0 {
		return nil
	}

	// Step 6: enable VM-level firewall AND ipfilter only after ALL ipsets are
	// fully seeded. Setting Enable alongside Ipfilter is required: ipfilter is
	// inert when the VM-level firewall is off, so enabling both in one call
	// ensures the allowlist is actually enforced.
	trueVal := true
	if optErr := nodeSvc.UpdateQemuFirewallOptions(ctx, node, vmidStr,
		&sdknodes.UpdateQemuFirewallOptionsParams{Enable: &trueVal, Ipfilter: &trueVal}); optErr != nil {
		logger.Warn("create_vm: UpdateQemuFirewallOptions(enable=true, ipfilter=true) failed (fail-open)",
			log.Int(metadataKeyVMID, vmid), log.Err(optErr))
		return nil
	}

	logger.Info("create_vm: VIP ipfilter ipsets seeded and ipfilter enabled",
		log.Int(metadataKeyVMID, vmid), log.Int("firewalled_nics", fwCount))

	return nil
}

// buildIPSetEntries returns the deduplicated, stable-order list of CIDR entries
// for an ipset: the NIC's own addresses first (so primary connectivity is
// always first), followed by the VIP entries in their normalized order.
// Duplicates are dropped preserving first-seen occurrence.
func buildIPSetEntries(nicIPs []string, vips []string) []string {
	seen := make(map[string]struct{})
	var out []string

	add := func(entry string) {
		if entry == "" {
			return
		}
		if _, dup := seen[entry]; dup {
			return
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}

	// The NIC's own addresses always come first, in member order. Normalize
	// each: a bare IP gains /32 (IPv4) or /128 (IPv6).
	for _, ip := range nicIPs {
		if ip == "" || strings.EqualFold(ip, "dhcp") {
			continue
		}
		normalized, err := normalizeVIPEntry(ip)
		if err == nil {
			add(normalized)
		}
	}

	for _, v := range vips {
		add(v)
	}

	// A NIC carrying an IPv6 address needs its link-local and multicast
	// ranges in the set. PVE only auto-generates ipfilter-net{N} while no
	// ipset of that name exists; creating our own replaces that generation
	// wholesale, and a set holding just the global address drops the
	// neighbor and router solicitations IPv6 sources from fe80::/10 and
	// sends to ff02::/16. Neighbor resolution would never complete and the
	// NIC's IPv6 would be dead while its IPv4 kept working.
	for _, entry := range out {
		if strings.Contains(entry, ":") {
			out = append(out, "fe80::/10", "ff02::/16")
			break
		}
	}
	return out
}
