package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// nicIPForwardingEnabled reports whether ip_forwarding=true is set in a NIC's
// cloud_properties. Returns false for nil or absent key.
func nicIPForwardingEnabled(cp map[string]any) bool {
	if cp == nil {
		return false
	}
	v, ok := cp["ip_forwarding"].(bool)
	return ok && v
}

// fwGroupRuleType is the PVE firewall rule type used to reference a cluster
// firewall group from a VM: a rule with type=group and action=<group name>.
const fwGroupRuleType = "group"

// enableVMFirewall enables the VM-level PVE firewall without attaching any group
// rules. It is called standalone when the operator has set the firewall flag but
// provided no security groups, and is also called by applySecurityGroups after
// group rules are attached so that the enable path is never duplicated.
//
// PVE API faults are wrapped retriable via pve.WrapError. Every error path
// returns to create_vm, which rolls the VM back.
func enableVMFirewall(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) error {
	nodeSvc := deps.PVE.Nodes()
	vmidStr := strconv.Itoa(vmid)
	enableFW := true
	if optErr := nodeSvc.UpdateQemuFirewallOptions(ctx, node, vmidStr, &sdknodes.UpdateQemuFirewallOptionsParams{
		Enable: &enableFW,
	}); optErr != nil {
		return cpierrors.Wrap(pve.WrapError(optErr),
			fmt.Sprintf("create_vm: enable VM firewall vmid=%d", vmid))
	}
	logger.Info("create_vm: VM-level firewall enabled", log.Int("vmid", vmid))
	return nil
}

// applySecurityGroups attaches each named PVE cluster firewall group to the VM
// as a group-type firewall rule and enables the VM-level firewall so the rules
// take effect. The named groups must already exist in PVE
// (/cluster/firewall/groups); the CPI references group content but never creates
// or modifies it.
//
// A requested group that does not exist is reported as a non-retriable
// CloudError (an operator misconfiguration the director should not retry). PVE
// API faults on the rule-create / firewall-enable calls are wrapped retriable
// via pve.WrapError. Every error path returns to create_vm, which rolls the VM
// back, so a partially-firewalled VM is never left behind.
//
// Called after VM start with the resolved node and vmid. A nil/empty groups
// slice is handled by the caller (no call is made), so this function assumes at
// least one group.
func applySecurityGroups(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	groups []string,
	logger *log.Logger,
) error {
	nodeSvc := deps.PVE.Nodes()
	vmidStr := strconv.Itoa(vmid)

	// 1. Validate every requested group exists before mutating the VM.
	existing, err := listFirewallGroupNames(ctx, deps)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err), "create_vm: list firewall groups")
	}
	for _, g := range groups {
		if _, ok := existing[g]; !ok {
			return cpierrors.Cloud(
				"create_vm: PVE firewall group %q not found; create it in PVE before referencing it in security_groups", g,
			)
		}
	}

	// 2. Attach each group as a group-type rule on the VM. In the PVE firewall
	//    API a group reference is a rule with type=group and action=<group name>.
	for _, g := range groups {
		enable := int64(1)
		groupName := g
		if ruleErr := nodeSvc.CreateQemuFirewallRules(ctx, node, vmidStr, &sdknodes.CreateQemuFirewallRulesParams{
			Type:   fwGroupRuleType,
			Action: groupName,
			Enable: &enable,
		}); ruleErr != nil {
			return cpierrors.Wrap(pve.WrapError(ruleErr),
				fmt.Sprintf("create_vm: attach firewall group %q vmid=%d", g, vmid))
		}
	}

	// 3. Enable the VM-level firewall so the attached group rules filter traffic.
	//    Delegate to enableVMFirewall to keep the enable path in one place.
	if err := enableVMFirewall(ctx, deps, node, vmid, logger); err != nil {
		return err
	}

	logger.Info("create_vm: applied firewall security groups",
		log.Int("vmid", vmid),
		log.Int("group_count", len(groups)),
	)
	return nil
}

// resolveEffectiveSecurityGroups returns the ordered security group list to apply
// to a new VM. Precedence (highest first):
//
//  1. callGroups — the parsed cloud_properties.security_groups from the create_vm call.
//  2. resolver.StringSlice("security_groups") — disk_type then vm_type profile layers.
//  3. cfg.SecurityGroups — global config default.
//
// A nil/empty result means no firewall group API calls should be made.
//
// callCP is the raw cloud_properties map used to build the layered resolver.
// A nil callCP is treated as an empty map. Unknown vm_type or disk_type selectors
// in callCP return a non-retriable CloudError.
func resolveEffectiveSecurityGroups(callCP map[string]any, cfg *config.CPIConfig, callGroups []string) ([]string, error) {
	if len(callGroups) > 0 {
		return callGroups, nil
	}
	r, err := newLayeredResolver(callCP, cfg)
	if err != nil {
		return nil, err
	}
	if ss, ok := r.StringSlice("security_groups"); ok {
		return ss, nil
	}
	if len(cfg.SecurityGroups) > 0 {
		return cfg.SecurityGroups, nil
	}
	return nil, nil
}

// resolveEffectiveFirewall returns whether the VM-level firewall should be
// enabled. Precedence (highest first):
//
//  1. resolver.Bool("firewall") — per-call cloud_properties or profile layer.
//     Explicit false here overrides config (returned as (false, nil)).
//  2. cfg.VMFirewallEnabled() — global config default (*bool VMFirewall field).
//
// Returns (false, nil) when no layer sets the flag (nil VMFirewall + no callCP key).
//
// A nil callCP is treated as an empty map. Unknown selectors return a CloudError.
func resolveEffectiveFirewall(callCP map[string]any, cfg *config.CPIConfig) (bool, error) {
	r, err := newLayeredResolver(callCP, cfg)
	if err != nil {
		return false, err
	}
	if v, ok := r.Bool("firewall"); ok {
		return v, nil
	}
	return cfg.VMFirewallEnabled(), nil
}

// listFirewallGroupNames returns the set of PVE cluster firewall group names.
// The /cluster/firewall/groups response is an array of objects each carrying a
// "group" field; entries that fail to decode are skipped.
func listFirewallGroupNames(ctx context.Context, deps Deps) (map[string]struct{}, error) {
	resp, err := deps.PVE.Cluster().ListFirewallGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if resp == nil {
		return out, nil
	}
	for _, raw := range *resp {
		var entry struct {
			Group string `json:"group"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Group != "" {
			out[entry.Group] = struct{}{}
		}
	}
	return out, nil
}

// applyIPForwarding iterates the networks map and, for each NIC with
// cloud_properties.ip_forwarding=true, sets firewall=0 on that NIC via a
// read-modify-write: the current net{i} property string is read from PVE via
// the QEMU Config API, the firewall token is cleared (or set to 0), and the
// full corrected string is written back. This preserves model, bridge, MAC
// address, and any other tokens PVE has assigned since configureNICs ran.
//
// When ip_forwarding=true on a NIC:
//   - The per-NIC firewall bit is explicitly cleared (firewall=0). Any
//     firewall=1 set by configureNICs or applyVIPAllowedAddressPairs for that
//     NIC is overridden.
//   - The §7.14 ipfilter is NOT applied for that NIC index. This is enforced
//     in applyVIPAllowedAddressPairs by calling nicIPForwardingEnabled before
//     seeding the ipset (ip_forwarding NICs are excluded from fwCount so the
//     enable gate never fires for them alone). See create_vm_vip.go.
//
// PVE API errors are wrapped retriable via pve.WrapError. The caller rolls
// back the VM on any non-nil return. No API calls are made when no NIC has
// ip_forwarding=true (byte-identical path).
func applyIPForwarding(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	networks map[string]createVMNetworkSpec,
	logger *log.Logger,
) error {
	// Derive NIC indices from the same plan configureNICs used, so a
	// nic_group that folded two networks onto one interface does not shift
	// every later index and send this write to the wrong NIC.
	plan := planNICs(networks)

	// Quick-exit when no NIC needs forwarding — zero API calls (byte-identical).
	anyForwarding := false
	for name := range networks {
		if nicIPForwardingEnabled(networks[name].CloudProperties) {
			anyForwarding = true
			break
		}
	}
	if !anyForwarding {
		return nil
	}

	// Read the current VM config once for all NICs that need updating.
	// qemu.Service.Config returns map[string]any with keys like "net0", "net1".
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		return cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("create_vm: ip_forwarding: read VM config vmid=%d", vmid))
	}

	nodeSvc := deps.PVE.Nodes()
	vmidStr := strconv.Itoa(vmid)

	for _, entry := range plan {
		i := entry.index
		// On a shared NIC the flag is a property of the interface, so any
		// member asking for forwarding turns it on for the whole NIC.
		name := ""
		for _, member := range entry.names {
			if nicIPForwardingEnabled(networks[member].CloudProperties) {
				name = member
				break
			}
		}
		if name == "" {
			continue
		}

		// Read-modify-write: fetch the current NIC string, patch the firewall
		// token to 0, and write the full corrected string back. This preserves
		// model, bridge, MAC, and any other tokens PVE assigned (e.g. queues=).
		//
		// PVE net{N} string format: "model[=macaddr],bridge=...[,token=val,...]"
		// Setting net{N}="firewall=0" (partial) REPLACES the entire NIC definition
		// in PVE, destroying model/bridge/MAC — so we must write the full string.
		netKey := fmt.Sprintf("net%d", i)
		currentStr, ok := vmCfg[netKey].(string)
		if !ok || currentStr == "" {
			// PVE Config() did not return a string for this NIC index. This is
			// unexpected (configureNICs wrote net{i} before start), but guard
			// rather than emit a bare "firewall=0" that would destroy model/bridge.
			// Log a warning and skip; the NIC will have whatever firewall state
			// PVE defaults to rather than a potentially destructive partial write.
			logger.Warn("create_vm: ip_forwarding: net config absent for NIC; firewall=0 not applied",
				log.Int(metadataKeyVMID, vmid),
				log.String("net", netKey),
				log.String("network", name),
			)
			continue
		}
		patched := patchNICFirewallToken(currentStr, false)

		if setErr := nodeSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
			Net: map[int]string{i: patched},
		}); setErr != nil {
			return cpierrors.Wrap(pve.WrapError(setErr),
				fmt.Sprintf("create_vm: ip_forwarding: disable NIC firewall %s vmid=%d", netKey, vmid))
		}
		logger.Info("create_vm: ip_forwarding: per-NIC firewall disabled for router/NAT NIC",
			log.Int(metadataKeyVMID, vmid),
			log.String("net", netKey),
			log.String("network", name),
			log.String("net_string", patched),
		)
	}
	return nil
}

// patchNICFirewallToken modifies the firewall token in a PVE NIC property
// string. It removes any existing "firewall=N" token and, when enabled=false,
// appends "firewall=0" explicitly. When enabled=true, "firewall=1" is appended.
// Tokens are comma-separated. An empty input string is valid (PVE will reject
// it with a parse error, but the caller handles that via WrapError).
func patchNICFirewallToken(nicStr string, enabled bool) string {
	// Split on comma, drop any "firewall=..." token, rebuild.
	tokens := strings.Split(nicStr, ",")
	filtered := tokens[:0]
	for _, tok := range tokens {
		lower := strings.ToLower(strings.TrimSpace(tok))
		if strings.HasPrefix(lower, "firewall=") {
			continue
		}
		if tok != "" {
			filtered = append(filtered, tok)
		}
	}
	fwVal := "firewall=0"
	if enabled {
		fwVal = "firewall=1"
	}
	filtered = append(filtered, fwVal)
	return strings.Join(filtered, ",")
}
