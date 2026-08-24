// Datacenter firewall master-switch probe (§1.4). PVE's firewall has three
// enforcement levels — datacenter, host, VM — and the datacenter master
// switch defaults OFF. The CPI programs VM-level firewall state (per-NIC
// firewall flag, security_groups, ipfilter/allowed_address_pairs) correctly
// regardless of the master switch, so every API call can succeed while zero
// packets are actually filtered. This file adds a best-effort, once-per-
// process probe that warns when that gap is present.
package handlers

import (
	"context"
	"fmt"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// firewallMasterSwitchProbedClusters guards the once-per-cluster probe of the
// PVE datacenter firewall master switch (GET /cluster/firewall/options). The
// master switch is a cluster-wide, rarely-changed setting, so one probe (and,
// if warranted, one Warn) per cluster per CPI process is sufficient —
// re-querying on every create_vm call would add an avoidable API round-trip
// to every deploy without adding information. Keyed per cluster rather than a
// plain process-wide sync.Once because per-request context overrides let one
// CPI process serve several PVE clusters: a process-lifetime Once would probe
// whichever cluster the first create_vm targeted and silently skip every
// other cluster's probe, hiding an unenforced-firewall warning exactly where
// it was never checked.
var firewallMasterSwitchProbedClusters sync.Map

// clusterIdentity returns a stable per-cluster key for warn-once state:
// host:port of the effective config. Returns "" for a nil config (callers
// treat that as one shared identity, matching the pre-override behavior).
func clusterIdentity(cfg *config.CPIConfig) string {
	if cfg == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
}

// firewallFeatureInPlay reports whether any firewall-affecting feature is
// requested for this VM: an enabled VM-level firewall flag, a non-empty
// effective security_groups list, or a NIC declaring allowed_address_pairs
// (which drives ipfilter seeding in applyVIPAllowedAddressPairs). A VM that
// requests none of these gets no master-switch probe — zero extra API calls,
// matching this CPI's byte-identical-when-unused convention.
//
// allowed_address_pairs presence is checked via parseVIPEntries ignoring any
// error: malformed entries were already rejected pre-mutation by
// validateVIPAllowedAddressPairs (step 1b), so by the time this runs (step
// 8b, after the VM exists) every entry that reaches here is well-formed; a
// defensive parse error here is treated as "no VIPs" rather than surfaced
// twice.
func firewallFeatureInPlay(effectiveGroups []string, firewallEnabled bool, networks map[string]createVMNetworkSpec) bool {
	if len(effectiveGroups) > 0 || firewallEnabled {
		return true
	}
	for name := range networks {
		if vips, err := parseVIPEntries(networks[name].CloudProperties); err == nil && len(vips) > 0 {
			return true
		}
	}
	return false
}

// probeFirewallMasterSwitch queries the PVE datacenter firewall master switch
// exactly once per CPI process. When it is off (or the field is absent —
// PVE's own default), it logs a structured Warn explaining that the
// VM-level/security-group/ipfilter rules this CPI just programmed are
// unenforced until the operator enables it (Datacenter → Firewall → Options →
// Enable), plus the anti-lockout caveat for doing so. It is a no-op after the
// first invocation regardless of outcome (including the fail-open paths
// below), so it never re-queries or re-warns within the same process.
//
// Fail-open: deps.PVE/Cluster() being unavailable, an API error (including
// 403 — GET /cluster/firewall/options requires Sys.Audit, which the
// documented token posture grants, but a misconfigured token should not
// break create_vm over a diagnostic probe), or an empty response all log a
// single "could not verify" Warn and return. This probe NEVER fails create_vm
// and never blocks on retries.
func probeFirewallMasterSwitch(ctx context.Context, deps Deps, logger *log.Logger) {
	if _, probed := firewallMasterSwitchProbedClusters.LoadOrStore(clusterIdentity(deps.Config), struct{}{}); probed {
		return
	}
	func() {
		if deps.PVE == nil || deps.PVE.Cluster() == nil {
			return
		}
		resp, err := deps.PVE.Cluster().ListFirewallOptions(ctx)
		if err != nil {
			logger.Warn("create_vm: could not verify PVE datacenter firewall master switch state " +
				"(GET /cluster/firewall/options failed — requires Sys.Audit); firewall enforcement " +
				"status is unknown for VMs created by this CPI process")
			return
		}
		if resp == nil {
			logger.Warn("create_vm: PVE datacenter firewall options response was empty; firewall " +
				"enforcement status is unknown for VMs created by this CPI process")
			return
		}
		// PVE booleans decode as integers (1/0), not JSON true/false; a nil
		// Enable field means the key was absent, which PVE treats as its own
		// default of disabled.
		enabled := resp.Enable != nil && *resp.Enable != 0
		if enabled {
			return
		}
		logger.Warn("create_vm: PVE datacenter firewall master switch is disabled " +
			"(Datacenter > Firewall > Options > Enable = 0); VM-level firewall rules, security_groups, " +
			"and allowed_address_pairs ipfilter allowlists programmed by this CPI are configured but NOT " +
			"enforced until the master switch is enabled. Enabling it activates cluster-wide host-level " +
			"enforcement — before doing so, ensure a management allow rule and explicit allowances for " +
			"required cluster traffic (e.g. Ceph, VXLAN UDP 4789, BGP TCP 179 where used) already exist, " +
			"or the change can lock out cluster nodes. See docs/configuration.md for details.")
	}()
}
