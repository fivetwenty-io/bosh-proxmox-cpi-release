// create_vm_network.go builds VM network interfaces: static-IP conflict
// pre-checks, NIC configuration/bridges, and the network helpers used to
// build agent and response network views.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// nicL2Domain identifies the (bridge, vlan) pair NICs are grouped by for the
// IP-conflict pre-check — two NICs share an L2 domain, and can therefore
// genuinely collide on the same IP, iff their bridge AND vlan tag are both
// equal (absent tag == absent tag, vlan 0). Without the vlan half of this
// tuple, a plain trunk bridge carrying several VLANs would report a false
// conflict between two guests that legitimately reuse the same address in
// separate L2 domains.
type nicL2Domain struct {
	bridge string
	vlan   int
}

// collectStaticIPsForConflictCheck extracts the bare IP addresses from the
// parsed network specs that carry a static (manual, non-DHCP) assignment.
// Dynamic (type=="dynamic") and VIP networks are skipped.
//
// collectStaticIPsForConflictCheck groups static IPs by their (bridge, vlan)
// L2 domain so that the caller can call detectIPConflict once per domain with
// the correct NIC filter, preventing conflicts on any domain from being
// silently missed.
//
// Bridge and vlan are resolved via resolveNICBridgeAndVLAN — the IDENTICAL
// precedence chain configureNICs' resolveNICAttributes uses (VM-level
// network_defaults[key] > per-NIC spec.CloudProperties[key] > resolver
// default) — so this pre-check can never classify a NIC onto a different
// domain than create_vm actually attaches it to (the previous
// implementation ignored cloud_properties.network_defaults.bridge entirely,
// so a VM-level bridge override silently defeated the duplicate-IP guard).
//
// Returns a map[nicL2Domain][]IP. Networks of type dynamic/vip or with
// empty/DHCP IPs are skipped. An empty map means no static IPs were found;
// callers must check len(result) > 0 before calling detectIPConflict.
func collectStaticIPsForConflictCheck(parsed *createVMParsedArgs, cfg *config.CPIConfig) map[nicL2Domain][]string {
	// Resolve the default bridge using the same layered logic as configureNICs.
	// Errors from an unknown vm_type selector are suppressed here: this is a
	// pre-flight check and the main create_vm path will surface the error later.
	defaultBridge, _, _ := resolveVMNICDefaultsWithError(cfg, parsed.cloudProps, parsed.cloudPropsMap)

	result := make(map[nicL2Domain][]string)
	for netName := range parsed.networks {
		spec := parsed.networks[netName]
		switch strings.ToLower(spec.Type) {
		case nicTypeManual:
			if spec.IP == "" || strings.EqualFold(spec.IP, "dhcp") {
				continue
			}
			// A malformed vlan value here is suppressed, not returned: this is a
			// pre-flight check (collectStaticIPsForConflictCheck has no error
			// return), and configureNICs — which always runs before this
			// pre-check, see runIPConflictChecks' step ordering in create_vm.go —
			// resolves the SAME network through resolveNICAttributes and will
			// already have failed the create with the same malformed value
			// before this code path is ever reached.
			bridge, vlan, _ := resolveNICBridgeAndVLAN(parsed.cloudProps.NetworkDefaults, spec.CloudProperties, defaultBridge, netName)
			domain := nicL2Domain{bridge: bridge, vlan: vlan}
			result[domain] = append(result[domain], spec.IP)
		default:
			// dynamic, vip, "" → skip
		}
	}
	return result
}

// runIPConflictChecks runs the static ipconfig{N} scan (step 5b) and, when
// enabled, the guest-agent active probe (step 5c). Returns nil when
// EnsureNoIPConflictsEnabled is false. The vmid argument is the newly created
// VM so its own ipconfig entries are excluded from conflict detection.
func runIPConflictChecks(ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, vmid int) error {
	if !deps.Config.EnsureNoIPConflictsEnabled() {
		return nil
	}

	// 5b. Static ipconfig{N} scan — DHCP/dynamic addresses are not visible here.
	ipsByDomain := collectStaticIPsForConflictCheck(parsed, deps.Config)
	for domain, ips := range ipsByDomain {
		// Pass vmid as excludeVMID so the newly created VM's own ipconfig
		// entries are not treated as a conflict against itself.
		conflict, conflictErr := detectIPConflict(ctx, deps, ips, domain.bridge, domain.vlan, vmid)
		if conflictErr != nil {
			return cpierrors.Wrap(conflictErr, "create_vm: IP-conflict pre-flight")
		}
		if conflict != nil {
			return IPConflictCloudError(conflict, domain.bridge, domain.vlan)
		}
	}

	// 5c. Active IP probe via guest agent (opt-in: ip_conflict_probe=agent).
	//
	// Extends the static-config scan with a live fan-out to running VM guest
	// agents, detecting DHCP-assigned and dynamically configured addresses
	// that do not appear in ipconfig{N} keys. Fail-open per guest: an
	// unreachable agent is logged and skipped, never blocking provisioning.
	if deps.Config.ActiveIPProbeEnabled() {
		var allTargetIPs []string
		for _, ips := range ipsByDomain {
			allTargetIPs = append(allTargetIPs, ips...)
		}
		if probeErr := probeGuestAgentIPConflict(ctx, deps, logger, allTargetIPs); probeErr != nil {
			return cpierrors.Wrap(probeErr, "create_vm: active IP probe")
		}
	}
	return nil
}

// resolveVMNICDefaultsWithError resolves the VM-level NIC bridge and model
// defaults using the layered resolver. Returns a CloudError when cpMap contains
// an unknown vm_type or disk_type selector.
//
// Precedence for bridge:
//  1. cp.NetworkBridge (call struct field, non-empty wins)
//  2. profile layers via r.String("network_bridge")
//  3. config.NetworkBridge
//  4. defaultNetworkBridge ("vmbr0")
//
// Precedence for model:
//  1. cp.NetworkModel (call struct field, non-empty wins)
//  2. profile layers via r.String("network_model")
//  3. built-in default "virtio"
//
// Per-NIC spec.CloudProperties["bridge"] / ["model"] overrides sit above these
// VM-level defaults and are applied in configureNICs after this call.
func resolveVMNICDefaultsWithError(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (bridge, model string, err error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", "", err
	}

	// Bridge resolution: call struct field (non-empty) → profile → config → constant.
	bridge = cfg.NetworkBridge
	if bridge == "" {
		bridge = defaultNetworkBridge
	}
	if cp.NetworkBridge != "" {
		bridge = cp.NetworkBridge
	} else if v, ok := r.String("network_bridge"); ok {
		// Profile layers only; call layer is already covered by cp.NetworkBridge above.
		// r.String reads all layers in order — call layer first — but since we only
		// land here when cp.NetworkBridge is empty, any non-empty "network_bridge" in
		// the call map would be redundant with the struct field. Profile wins over
		// config when the struct field is empty.
		bridge = v
	}

	// Model resolution: call struct field (non-empty) → profile → built-in "virtio".
	model = "virtio"
	if cp.NetworkModel != "" {
		model = cp.NetworkModel
	} else if v, ok := r.String("network_model"); ok {
		model = v
	}

	return bridge, model, nil
}

// resolveCloneMode returns the effective clone_mode by consulting the layered
// resolver (call cloud_properties → profile layers) then falling back to
// config.CloneMode. An empty config.CloneMode defaults to "auto".
// Returns a CloudError when cpMap contains an unknown vm_type or disk_type selector.
func resolveCloneMode(cfg *config.CPIConfig, cpMap map[string]any) (string, error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", err
	}
	if v, ok := r.String("clone_mode"); ok {
		return v, nil
	}
	mode := cfg.CloneMode
	if mode == "" {
		mode = config.CloneModeAuto
	}
	return mode, nil
}

// configureNICs builds and applies the NIC configuration for the new VM from
// the networks map. Returns the ordered list of network names (used later for
// MAC extraction) and any error.
func configureNICs(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
) ([]string, error) {
	// Build an ordered list of network names for deterministic NIC assignment.
	netNames := sortedNetworkNames(parsed.networks)

	// VM-level bridge and model defaults via layered resolver.
	// Per-NIC spec.CloudProperties["bridge"]/["model"] overrides are applied below.
	defaultBridge, defaultModel, err := resolveVMNICDefaultsWithError(deps.Config, parsed.cloudProps, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}

	// SDN vnets run at a reduced MTU (VXLAN encapsulation spends ~50 bytes;
	// PVE derives e.g. 1450 on a 1500 underlay). NICs attached to a vnet get
	// mtu=1 — "inherit the bridge MTU" — so the guest never emits an
	// oversized frame. Membership is decided by the actual SDN vnet list in
	// EVERY network_mode (this used to be gated on network_mode,
	// which is a create_network/delete_network routing knob, not a NIC
	// attribute — a NIC genuinely attached to a pre-existing SDN vnet must
	// get mtu=1 even under network_mode: bridge). The list itself is cached
	// (pve.CachedVnetNames, short TTL) so a plain-bridge cluster pays the
	// cost at most once per TTL window, not once per create_vm.
	vnetNames := sdnVnetNameSet(ctx, deps, logger, len(netNames))

	// Build net map[int]string and ipconfig map[int]string for UpdateQemuConfigParams
	netMap := make(map[int]string, len(netNames))
	ipconfigMap := make(map[int]string, len(netNames))
	// bridgeSet collects the finalized bridge for each NIC so the optional SDN
	// eventual-consistency gate can resolve them all on the target node before
	// any config write (no partial netN= on a not-yet-realized bridge).
	bridgeSet := make(map[string]struct{}, len(netNames))
	var nameservers []string
	firstNS := true

	for i, name := range netNames {
		spec := parsed.networks[name]

		attrs, attrErr := resolveNICAttributes(
			deps, parsed.cloudProps.NetworkDefaults, spec.CloudProperties, defaultBridge, defaultModel, name)
		if attrErr != nil {
			return nil, attrErr
		}
		bridge, model, nicFirewall, vlan, mtu := attrs.bridge, attrs.model, attrs.firewall, attrs.vlan, attrs.mtu

		// net0 = "virtio,bridge=vmbr0" (no MAC — PVE assigns one)
		netMap[i] = fmt.Sprintf("%s,bridge=%s", model, bridge)
		if nicFirewall {
			netMap[i] += ",firewall=1"
		}
		if vlan != 0 {
			netMap[i] += fmt.Sprintf(",tag=%d", vlan)
			// An explicit vlan tag (Pattern A) on a bridge that is itself an SDN
			// vnet (Pattern B) mixes the two documented alternatives — the vnet
			// may already encode a VLAN of its own. Warn rather than fail: PVE
			// is the authority on whether the combination is actually invalid,
			// and will reject the config outright when it is.
			if _, onVnet := vnetNames[bridge]; onVnet {
				logger.Warn("create_vm: network sets an explicit vlan tag on a bridge that is itself an SDN vnet (mixing per-NIC VLAN tagging with an SDN vnet-per-VLAN pattern)",
					log.String("network", name),
					log.String("bridge", bridge),
					log.Int("vlan", vlan),
				)
			}
		}
		// mtu is a virtio-only option (PVE rejects it on e1000/rtl8139) —
		// resolveNICAttributes already validated that above for an explicit
		// per-NIC/network_defaults value. An explicit mtu always wins over
		// the automatic vnet-derived mtu=1 inheritance below — never emit
		// both mtu= segments.
		switch {
		case mtu != 0:
			netMap[i] += fmt.Sprintf(",mtu=%d", mtu)
		default:
			_, isVnet := vnetNames[bridge]
			switch {
			case isVnet && strings.HasPrefix(model, "virtio"):
				netMap[i] += ",mtu=1"
			case isVnet:
				// Non-virtio model on an SDN vnet: the guest cannot inherit the
				// vnet's (typically reduced, VXLAN-encapsulated) MTU the way a
				// virtio NIC does, and stays at the PVE default of 1500. That
				// mismatch is the "small packets pass, large packets hang"
				// blackhole — see docs/troubleshooting.md's "Small packets pass,
				// large packets hang (SDN MTU)" entry. Warn at create time rather
				// than leaving the operator to discover it via a hung transfer.
				logger.Warn("create_vm: non-virtio NIC model on an SDN vnet will not auto-track the vnet MTU",
					log.String("network", name),
					log.String("model", model),
					log.String("vnet", bridge),
				)
			}
		}
		if bridge != "" {
			bridgeSet[bridge] = struct{}{}
		}

		// ipconfig: dynamic → dhcp; manual → ip=<cidr>,gw=<gw>
		switch strings.ToLower(spec.Type) {
		case nicTypeDynamic, "":
			ipconfigMap[i] = "ip=dhcp"
		case nicTypeManual:
			if spec.IP != "" {
				// Warn when a static IP has no gateway — this is likely an
				// operator oversight. The VM still deploys; routing may be
				// impaired without a default gateway.
				if spec.Gateway == "" {
					logger.Warn("create_vm: manual network has no gateway",
						log.String("network", name))
				}
				cidr := ipToCIDR(spec.IP, spec.Netmask)
				cfg := "ip=" + cidr
				if spec.Gateway != "" {
					cfg += ",gw=" + spec.Gateway
				}
				ipconfigMap[i] = cfg
			} else {
				ipconfigMap[i] = "ip=dhcp"
			}
		case "vip":
			// VIP networks are routing-level, no ipconfig needed
		}

		// Collect DNS servers from all specs (first spec's DNS takes precedence)
		if firstNS && len(spec.DNS) > 0 {
			nameservers = spec.DNS
			firstNS = false
		}
	}

	nicParams := &sdknodes.UpdateQemuConfigParams{
		Net:      netMap,
		Ipconfig: ipconfigMap,
	}
	if len(nameservers) > 0 {
		ns := strings.Join(nameservers, " ")
		nicParams.Nameserver = &ns
	}
	// Propagate search domain to PVE cloud-init searchdomain when any network
	// spec supplies one via cloud_properties "search_domain", "dns_search", or
	// "domain". First non-empty value wins across specs. When absent the field
	// is left unset — byte-identical to pre-existing behavior.
	if sd := pickSearchDomain(netNames, parsed.networks); sd != "" {
		nicParams.Searchdomain = &sd
	}

	// Optional consume-side eventual-consistency gate. Resolve every NIC bridge
	// on the target node before writing any netN= so a not-yet-realized SDN
	// bridge cannot leave a partial config.
	if err := resolveNICBridges(ctx, deps, shape.node, bridgeSet); err != nil {
		return nil, err
	}

	if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, shape.node, strconv.Itoa(vmid), nicParams); err != nil {
		return nil, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf("create_vm: configure NICs vmid=%d: %s", vmid, err.Error()))
	}

	return netNames, nil
}

// sdnVnetNameSet returns the set of SDN vnet names currently defined
// (pending included) so configureNICs can hand vnet-attached virtio NICs
// mtu=1 (inherit the bridge MTU) and decide vlan/tag membership. Deliberately
// FAIL-OPEN: any listing failure returns an empty set and the VM creates
// without the mtu option — a guest at the underlay MTU on an external bridge
// is unaffected, and a guest on a vnet degrades to the pre-existing behavior
// rather than blocking create_vm. Membership is decided by the actual vnet
// list in EVERY network_mode (see the call site's comment); the
// list itself comes from pve.CachedVnetNames, a short-TTL process-wide cache
// shared with the consume-side bridge-resolve gate, so this is skipped
// entirely (nil, no API call) only when the VM has no NICs at all.
func sdnVnetNameSet(ctx context.Context, deps Deps, logger *log.Logger, nicCount int) map[string]struct{} {
	if nicCount == 0 {
		return nil
	}
	set, err := pve.CachedVnetNames(ctx, deps.PVE)
	if err != nil {
		logger.Debug("create_vm: SDN vnet listing failed; NICs get no mtu inheritance and vlan/tag membership is treated as not-a-vnet",
			log.Err(err))
		return nil
	}
	return set
}

// resolveNICBridgeAndVLAN computes the effective bridge and VLAN tag for one
// NIC using precedence (highest first): VM-level network_defaults[key] >
// per-NIC spec cloud_properties[key] > the resolver default bridge (vlan has
// no resolver-default source; absent anywhere resolves to 0 — untagged).
//
// Extracted as its own function, rather than duplicated inline, so
// collectStaticIPsForConflictCheck (the IP-conflict pre-check, which must
// classify NICs into the exact same L2 domains create_vm actually attaches
// them to) and resolveNICAttributes (the real NIC-assembly path) can never
// drift apart — bugs N1 (network_defaults.bridge invisible to the
// pre-check) and N3 (pre-check VLAN-blind) both stem from that kind of
// duplication.
//
// Returns a non-retriable CloudError, naming nicName and the offending
// cloud_properties key, when a vlan key is PRESENT but not integer-shaped
// (coerceInt fails) — e.g. null, a bool, or an array. Silently treating an
// unparseable vlan as "absent" would attach the NIC untagged (vlan 0, the
// bridge's native/management VLAN) with no indication anything was wrong.
func resolveNICBridgeAndVLAN(netDefaults, nicCP map[string]any, defaultBridge, nicName string) (bridge string, vlan int, err error) {
	bridge = defaultBridge
	if v, ok := nicCP[nicCPKeyBridge].(string); ok && v != "" {
		bridge = v
	}
	if v, ok := nicCP[nicCPKeyVLAN]; ok {
		n, ok2 := coerceInt(v)
		if !ok2 {
			return "", 0, cpierrors.Cloud(
				"create_vm: network %q cloud_properties.%s must be an integer, got %v (%T)",
				nicName, nicCPKeyVLAN, v, v)
		}
		vlan = n
	}
	if v, ok := netDefaults[nicCPKeyBridge].(string); ok && v != "" {
		bridge = v
	}
	if v, ok := netDefaults[nicCPKeyVLAN]; ok {
		n, ok2 := coerceInt(v)
		if !ok2 {
			return "", 0, cpierrors.Cloud(
				"create_vm: network %q cloud_properties.network_defaults.%s must be an integer, got %v (%T)",
				nicName, nicCPKeyVLAN, v, v)
		}
		vlan = n
	}
	return bridge, vlan, nil
}

// resolveNICAttributes computes the effective bridge, model, per-NIC firewall
// flag, VLAN tag, and explicit MTU for one NIC. Precedence for every key
// (highest first):
//
//	VM-level network_defaults[key] (§7.34)
//	  > per-NIC spec cloud_properties[key]
//	  > resolver default (bridge/model only — struct field / profile /
//	    config / const; vlan/mtu have no resolver-default source, absent
//	    anywhere resolves to 0 — untagged / no explicit MTU)
//
// Supported keys: bridge, model, firewall, vlan, mtu. Unknown keys are
// silently ignored — cloud_properties are loosely typed. The firewall flag
// here only selects the NIC's firewall=1 bit; the VM-level firewall must
// also be enabled for filtering to take effect (see applySecurityGroups).
//
// Returns a non-retriable CloudError, naming nicName, when:
//   - vlan is present and not integer-shaped (via resolveNICBridgeAndVLAN).
//   - vlan is present and outside 1..4094 (the 802.1Q VLAN ID space).
//   - mtu is present and not integer-shaped.
//   - mtu is present and is neither exactly 1 (inherit) nor within 576..65520.
//   - mtu is present and the effective model is not virtio-prefixed — PVE
//     rejects the mtu option on e1000/rtl8139/etc.
//
// nicAttributes groups the resolved fields so resolveNICAttributes stays
// within the project's max-results limit; see the doc comment there.
type nicAttributes struct {
	bridge   string
	model    string
	firewall bool
	vlan     int
	mtu      int
}

func resolveNICAttributes(
	deps Deps, netDefaults, nicCP map[string]any, defaultBridge, defaultModel, nicName string,
) (nicAttributes, error) {
	bridge, vlan, err := resolveNICBridgeAndVLAN(netDefaults, nicCP, defaultBridge, nicName)
	if err != nil {
		return nicAttributes{}, err
	}

	model := defaultModel
	if cp, ok := nicCP[nicCPKeyModel].(string); ok && cp != "" {
		model = cp
	}
	firewall := deps.Config.VMFirewallEnabled()
	if cp, ok := nicCP[nicCPKeyFirewall].(bool); ok {
		firewall = cp
	}
	if v, ok := netDefaults[nicCPKeyModel].(string); ok && v != "" {
		model = v
	}
	if v, ok := netDefaults[nicCPKeyFirewall].(bool); ok {
		firewall = v
	}

	mtu := 0
	if v, ok := nicCP[nicCPKeyMTU]; ok {
		n, ok2 := coerceInt(v)
		if !ok2 {
			return nicAttributes{}, cpierrors.Cloud(
				"create_vm: network %q cloud_properties.%s must be an integer, got %v (%T)",
				nicName, nicCPKeyMTU, v, v)
		}
		mtu = n
	}
	if v, ok := netDefaults[nicCPKeyMTU]; ok {
		n, ok2 := coerceInt(v)
		if !ok2 {
			return nicAttributes{}, cpierrors.Cloud(
				"create_vm: network %q cloud_properties.network_defaults.%s must be an integer, got %v (%T)",
				nicName, nicCPKeyMTU, v, v)
		}
		mtu = n
	}

	if vlan != 0 && (vlan < 1 || vlan > vlanMaxTag) {
		return nicAttributes{}, cpierrors.Cloud(
			"create_vm: network %q vlan tag must be within 1..%d, got %d", nicName, vlanMaxTag, vlan)
	}
	if mtu != 0 && mtu != 1 && (mtu < 576 || mtu > 65520) {
		return nicAttributes{}, cpierrors.Cloud(
			"create_vm: network %q mtu must be 1 (inherit bridge MTU) or within 576..65520, got %d",
			nicName, mtu)
	}
	if mtu != 0 && !strings.HasPrefix(model, "virtio") {
		return nicAttributes{}, cpierrors.Cloud(
			"create_vm: network %q mtu is only valid on a virtio NIC model, got model %q",
			nicName, model)
	}

	return nicAttributes{bridge: bridge, model: model, firewall: firewall, vlan: vlan, mtu: mtu}, nil
}

// resolveNICBridges is the consume-side SDN eventual-consistency gate. When
// enabled, it confirms each bridge in bridgeSet is realized on node before the
// caller writes the NIC config. A bridge that is not an SDN vnet (external/
// static, e.g. vmbr0) passes through untouched; a bridge still converging
// surfaces as a retriable error so the director re-drives rather than attaching
// a NIC to a bridge that does not yet exist. Off (retries 0) → no calls.
func resolveNICBridges(ctx context.Context, deps Deps, node string, bridgeSet map[string]struct{}) error {
	if !deps.Config.NetworkResolveEnabled() {
		return nil
	}
	retries := deps.Config.NetworkResolveRetriesValue()
	timeout := time.Duration(deps.Config.NetworkResolveTimeoutSecValue()) * time.Second
	for bridge := range bridgeSet {
		if gateErr := pve.ResolveNodeBridgeOnNode(ctx, deps.PVE, node, bridge, retries, timeout); gateErr != nil {
			return cpierrors.Wrap(gateErr, fmt.Sprintf("create_vm: resolve bridge %q on node %q", bridge, node))
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// sortedNetworkNames returns network names in a deterministic order.
// "default" network (if present) is first; remaining names are alphabetical.
//
// The previous implementation iterated the tail of a pre-built slice that
// already had "default" at index 0, which meant the bubble-sort only ran
// over non-default names when "default" was present. When "default" was
// absent the slice had no guaranteed ordering because Go map iteration is
// randomised — bug B8. This implementation collects non-default names into
// a fresh slice, sorts them with sort.Strings (correct O(n log n)), then
// prepends "default" only if it exists.
// --------------------------------------------------------------------------
func sortedNetworkNames(networks map[string]createVMNetworkSpec) []string {
	names := make([]string, 0, len(networks))
	hasDefault := false
	for n := range networks {
		if n == defaultNetworkName {
			hasDefault = true
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if hasDefault {
		return append([]string{defaultNetworkName}, names...)
	}
	return names
}

// --------------------------------------------------------------------------
// ipToCIDR converts a dotted-decimal netmask to a prefix length and returns
// "ip/prefix". Falls back to "/32" if the netmask cannot be parsed.
// --------------------------------------------------------------------------
func ipToCIDR(ip, netmask string) string {
	prefix := netmaskToCIDR(netmask)
	return fmt.Sprintf("%s/%d", ip, prefix)
}

// netmaskToCIDR counts set bits in a dotted-decimal subnet mask.
func netmaskToCIDR(netmask string) int {
	if netmask == "" {
		return 32
	}
	parts := strings.Split(netmask, ".")
	if len(parts) != 4 {
		return 32
	}
	bits := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return 32
		}
		for b := 7; b >= 0; b-- {
			if (n>>uint(b))&1 == 1 {
				bits++
			}
		}
	}
	return bits
}

// --------------------------------------------------------------------------
// validateNetworkContainment checks that every manual static-IP network whose
// spec carries a Range CIDR has its IP within that range. Returns a
// non-retriable CloudError on the first violation; returns nil when all
// networks pass. Skip conditions (no error): Type != "manual", IP == "",
// Range == "".
// --------------------------------------------------------------------------
func validateNetworkContainment(networks map[string]createVMNetworkSpec) error {
	// Process names in sorted order so the first reported error is deterministic.
	names := sortedNetworkNames(networks)
	for _, name := range names {
		spec := networks[name]
		if !strings.EqualFold(spec.Type, nicTypeManual) || spec.IP == "" || spec.Range == "" {
			continue
		}
		_, cidrNet, err := net.ParseCIDR(spec.Range)
		if err != nil {
			return cpierrors.Cloud(
				"create_vm: network %q has malformed range %q: %s",
				name, spec.Range, err.Error())
		}
		ip := net.ParseIP(spec.IP)
		if ip == nil {
			return cpierrors.Cloud(
				"create_vm: network %q has malformed IP %q",
				name, spec.IP)
		}
		if !cidrNet.Contains(ip) {
			return cpierrors.Cloud(
				"create_vm: network %q IP %s is outside declared range %s",
				name, spec.IP, spec.Range)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// pickSearchDomain scans the ordered network specs and returns the first
// non-empty search domain found under the cloud_properties keys
// "search_domain", "dns_search", or "domain" (first key wins per spec,
// first spec wins across specs). Returns "" when none found.
// --------------------------------------------------------------------------
func pickSearchDomain(netNames []string, networks map[string]createVMNetworkSpec) string {
	for _, name := range netNames {
		spec := networks[name]
		for _, key := range []string{"search_domain", "dns_search", "domain"} {
			if v, ok := spec.CloudProperties[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// --------------------------------------------------------------------------
// buildAgentNetworks converts CPI network specs to agent.NetworkSpec map.
// --------------------------------------------------------------------------
func buildAgentNetworks(networks map[string]createVMNetworkSpec) map[string]agent.NetworkSpec {
	out := make(map[string]agent.NetworkSpec, len(networks))
	for name := range networks {
		spec := networks[name]
		out[name] = agent.NetworkSpec{
			Type:    spec.Type,
			IP:      spec.IP,
			Netmask: spec.Netmask,
			Gateway: spec.Gateway,
			DNS:     spec.DNS,
			Default: spec.Default,
		}
	}
	return out
}

// --------------------------------------------------------------------------
// buildResponseNetworks constructs the v2 response networks map.
// It copies input specs and fills in the MAC address read from PVE VM config.
// PVE stores NICs as "net0" → "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0,...".
// --------------------------------------------------------------------------
func buildResponseNetworks(
	networks map[string]createVMNetworkSpec,
	orderedNames []string,
	vmCfg map[string]any,
) map[string]createVMNetworkSpec {
	// Build index → MAC lookup from VM config
	macByIndex := extractMACsFromConfig(vmCfg)

	out := make(map[string]createVMNetworkSpec, len(networks))
	for i, name := range orderedNames {
		spec := networks[name]
		if mac, ok := macByIndex[i]; ok {
			spec.MAC = mac
		}
		out[name] = spec
	}
	// Copy any names not in orderedNames (defensive)
	for name := range networks {
		if _, exists := out[name]; !exists {
			out[name] = networks[name]
		}
	}
	return out
}

// extractMACsFromConfig parses "net0", "net1", ... keys from PVE VM config and
// returns map[nicIndex]macAddress. PVE net value format:
//
//	"virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1"
//	  or
//	"virtio,bridge=vmbr0"   (no MAC, PVE assigns later)
func extractMACsFromConfig(cfg map[string]any) map[int]string {
	macs := make(map[int]string)
	for key, val := range cfg {
		if !strings.HasPrefix(key, "net") {
			continue
		}
		idxStr := strings.TrimPrefix(key, "net")
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		valStr, ok := val.(string)
		if !ok {
			continue
		}
		mac := parseMACFromNetValue(valStr)
		if mac != "" {
			macs[idx] = mac
		}
	}
	return macs
}

// parseMACFromNetValue extracts the MAC from a PVE net value string.
// Format: "model=MAC,bridge=X,..." or "model,bridge=X,..."
// The MAC is the part after "=" in the first segment if it contains ":".
func parseMACFromNetValue(val string) string {
	segments := strings.Split(val, ",")
	if len(segments) == 0 {
		return ""
	}
	// First segment is either "model" or "model=MAC"
	first := segments[0]
	eqIdx := strings.Index(first, "=")
	if eqIdx < 0 {
		return ""
	}
	mac := first[eqIdx+1:]
	// Validate it looks like a MAC (contains ":")
	if strings.Count(mac, ":") == 5 {
		return strings.ToLower(mac)
	}
	return ""
}
