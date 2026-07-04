// Guest-agent IP fan-out conflict probe for create_vm pre-create guard.
//
// Scope and limitations:
//
//   - Calls the QEMU guest agent on each running VM in the cluster to collect
//     dynamically assigned IP addresses. Detects DHCP-assigned addresses that
//     the static-config scan (detectIPConflict) cannot see.
//   - Fails open per guest: an unreachable or error-returning guest agent is
//     logged at debug level and skipped. create_vm is never failed due to a
//     probe error on an individual guest.
//   - Only running VMs (status=="running") are probed. Stopped, paused, or
//     template VMs are skipped.
//   - Loopback (127.0.0.0/8, ::1) and link-local (169.254.0.0/16, fe80::/10)
//     addresses reported by a guest are excluded from conflict checks.
//   - Bounded concurrency: at most 16 concurrent guest-agent calls.
//
// This file is safe to call concurrently; it holds no shared mutable state.
package handlers

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"golang.org/x/sync/errgroup"
)

// loopbackV4 is the loopback network for IPv4 (127.0.0.0/8).
var loopbackV4 = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("127.0.0.0/8")
	return n
}()

// linkLocalV4 is the link-local network for IPv4 (169.254.0.0/16).
var linkLocalV4 = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("169.254.0.0/16")
	return n
}()

// linkLocalV6 is the link-local network for IPv6 (fe80::/10).
var linkLocalV6 = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("fe80::/10")
	return n
}()

// isSkippedGuestIP reports whether the given IP should be excluded from
// conflict checking. Excluded addresses: loopback (127/8, ::1) and
// link-local (169.254/16, fe80::/10).
func isSkippedGuestIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	if loopbackV4.Contains(ip) {
		return true
	}
	if linkLocalV4.Contains(ip) {
		return true
	}
	if linkLocalV6.Contains(ip) {
		return true
	}
	return false
}

// agentNetworkEntry is one interface object in the guest-agent network-get-interfaces response.
// The PVE agent returns a list of these under a "result" envelope or directly as a JSON array.
type agentNetworkEntry struct {
	Name        string           `json:"name"`
	IPAddresses []agentIPAddress `json:"ip-addresses"`
}

// agentIPAddress is one IP address entry within an agentNetworkEntry.
type agentIPAddress struct {
	Type    string `json:"ip-address-type"` // "ipv4" or "ipv6"
	Address string `json:"ip-address"`
	Prefix  int    `json:"prefix"`
}

// agentNetworkResponseEnvelope handles the PVE-wrapped form {"result": [...]}.
type agentNetworkResponseEnvelope struct {
	Result []agentNetworkEntry `json:"result"`
}

// parseAgentNetworkInterfaces attempts to decode the raw JSON returned by
// ListQemuAgentNetworkGetInterfaces. PVE returns the data in two possible shapes:
//
//  1. Bare array:    [{"name":"eth0","ip-addresses":[...]}]
//  2. Wrapped form:  {"result":[{"name":"eth0","ip-addresses":[...]}]}
//
// Both shapes are handled; on any parse failure the function returns nil so
// the caller can apply fail-open semantics.
func parseAgentNetworkInterfaces(raw []byte) []agentNetworkEntry {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 {
		return nil
	}

	// Try bare array first.
	if trimmed[0] == '[' {
		var ifaces []agentNetworkEntry
		if err := json.Unmarshal(raw, &ifaces); err == nil {
			return ifaces
		}
	}

	// Try wrapped envelope.
	var envelope agentNetworkResponseEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil {
		return envelope.Result
	}

	return nil
}

// probeGuestAgentIPConflict calls the QEMU guest agent on every running VM in
// the cluster and checks whether any of the collected IP addresses collide with
// targetIPs. Returns a CPICloud error describing the conflict when one is found.
//
// Fail-open semantics apply per guest: an unreachable or error-returning guest
// agent is logged at debug level and skipped. create_vm is never blocked by a
// guest agent error.
//
// Parameters:
//   - ctx: cancellation context; cancellation stops in-flight goroutines.
//   - deps: handler dependencies (PVE client, config, logger).
//   - logger: scoped handler logger for structured log output.
//   - targetIPs: the IP addresses (plain, no CIDR) to check.
//     Empty or nil slice returns nil immediately.
//
// Concurrency: bounded to maxAgentProbeWorkers concurrent guest-agent calls.
//
//nolint:gocognit // Structured probe: list → parallel per-VM agent call → IP parse. Cognitive load inherent to the three-phase algorithm.
func probeGuestAgentIPConflict(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	targetIPs []string,
) error {
	if len(targetIPs) == 0 {
		return nil
	}

	// Build a lookup set using the canonical net.IP representation so that
	// "10.0.0.5" and "::ffff:10.0.0.5" compare equal.
	targetSet := make(map[string]string, len(targetIPs)) // canonical → original
	for _, raw := range targetIPs {
		if raw == "" {
			continue
		}
		parsed := net.ParseIP(raw)
		if parsed == nil {
			continue
		}
		targetSet[canonicalIP(parsed)] = raw
	}
	if len(targetSet) == 0 {
		return nil
	}

	// Phase 1: list all running VM resources from the cluster.
	typeStr := "vm"
	var resources *sdkcluster.ListResourcesResponse
	listErr := pve.RetryOnTransient(ctx, logger, "probe_agent_ip_list_resources", 0, func() error {
		var inner error
		resources, inner = deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if listErr != nil {
		// Infra error enumerating cluster resources — fail open so create_vm
		// is never blocked by a probe/cluster-API error.
		logger.Debug(
			"probe_agent_ip: skipping probe, ListResources failed",
			log.Err(listErr),
		)
		return nil
	}
	if resources == nil || len(*resources) == 0 {
		return nil
	}

	type vmEntry struct {
		VMID   int64  `json:"vmid"`
		Node   string `json:"node"`
		Status string `json:"status"`
		Name   string `json:"name"`
	}

	// Parse resource list; keep only running VMs.
	entries := make([]vmEntry, 0, len(*resources))
	for _, raw := range *resources {
		var e vmEntry
		if err := json.Unmarshal(raw, &e); err != nil || e.VMID <= 0 {
			continue
		}
		if !strings.EqualFold(e.Status, "running") {
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
		return nil
	}

	// Phase 2: parallel guest-agent fan-out.
	const maxAgentProbeWorkers = 16
	sem := make(chan struct{}, maxAgentProbeWorkers)

	var (
		conflictMu sync.Mutex
		found      error
	)

	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()

	g, probeCtx := errgroup.WithContext(probeCtx)

	for i := range entries {
		entry := entries[i]

		g.Go(func() error {
			// Acquire semaphore.
			select {
			case sem <- struct{}{}:
			case <-probeCtx.Done():
				return nil
			}
			defer func() { <-sem }()

			// Abort early if conflict already found.
			select {
			case <-probeCtx.Done():
				return nil
			default:
			}

			vmidStr := strconv.FormatInt(entry.VMID, 10)

			resp, agentErr := deps.PVE.Nodes().ListQemuAgentNetworkGetInterfaces(probeCtx, entry.Node, vmidStr)
			if agentErr != nil {
				// Fail-open: agent unreachable/unavailable → skip.
				logger.Debug(
					"probe_agent_ip: skipping guest agent error",
					log.String("node", entry.Node),
					log.String("vmid", vmidStr),
					log.Err(agentErr),
				)
				return nil
			}
			if resp == nil {
				return nil
			}

			ifaces := parseAgentNetworkInterfaces([]byte(*resp))
			conflictIP, conflictOrig := findConflictingIP(ifaces, targetSet)
			if conflictIP == "" {
				return nil
			}

			conflictMu.Lock()
			if found == nil {
				found = cpierrors.Cloud(
					"create_vm: active IP conflict detected — address %s is already assigned "+
						"to running guest VM %d (%s) on node %s; "+
						"choose a different IP or stop/reconfigure the conflicting guest before retrying",
					conflictOrig, entry.VMID, entry.Name, entry.Node,
				)
				cancelProbe()
			}
			conflictMu.Unlock()
			return nil
		})
	}

	if waitErr := g.Wait(); waitErr != nil {
		// Per-guest worker errors are already handled as fail-open inside each
		// goroutine (they return nil). If Wait() somehow surfaces a non-nil
		// error it is an infrastructure anomaly — fail open, never block create_vm.
		logger.Debug(
			"probe_agent_ip: skipping probe, errgroup.Wait returned error",
			log.Err(waitErr),
		)
		return nil
	}

	return found
}

// canonicalIP returns a normalized string key for ip suitable for set
// membership checks. IPv4-in-IPv6 addresses (::ffff:x.x.x.x) are collapsed
// to the 4-byte IPv4 representation so "10.0.0.5" and "::ffff:10.0.0.5"
// compare equal.
func canonicalIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// normalizeGuestAddr strips a CIDR prefix-length suffix ("/<n>") and/or an
// IPv6 zone-ID suffix ("%<zone>") from a raw address string before calling
// net.ParseIP. PVE guest agents may report addresses in either plain form
// ("10.0.0.5"), CIDR form ("10.0.0.5/24"), or IPv6-with-zone form
// ("fe80::1%eth0"). net.ParseIP rejects both suffixed forms, so without
// normalization those addresses are silently skipped, causing missed conflicts.
func normalizeGuestAddr(raw string) string {
	// Strip CIDR suffix: "10.0.0.5/24" → "10.0.0.5".
	if idx := strings.IndexByte(raw, '/'); idx >= 0 {
		raw = raw[:idx]
	}
	// Strip IPv6 zone ID: "fe80::1%eth0" → "fe80::1".
	if idx := strings.IndexByte(raw, '%'); idx >= 0 {
		raw = raw[:idx]
	}
	return raw
}

// findConflictingIP scans ifaces for any IP address that is in targetSet and
// is not a loopback or link-local address. Returns the canonical key and the
// original target string for the first conflict found, or ("","") if clean.
//
// Address strings are normalized with normalizeGuestAddr before parsing so
// that CIDR-suffixed ("10.0.0.5/24") and zone-suffixed ("fe80::1%eth0") forms
// returned by PVE guest agents are handled correctly rather than silently
// dropped by net.ParseIP.
func findConflictingIP(ifaces []agentNetworkEntry, targetSet map[string]string) (string, string) {
	for _, iface := range ifaces {
		for _, addr := range iface.IPAddresses {
			if addr.Address == "" {
				continue
			}
			normalized := normalizeGuestAddr(addr.Address)
			if normalized == "" {
				continue
			}
			parsed := net.ParseIP(normalized)
			if parsed == nil {
				continue
			}
			if isSkippedGuestIP(parsed) {
				continue
			}
			key := canonicalIP(parsed)
			if orig, hit := targetSet[key]; hit {
				return key, orig
			}
		}
	}
	return "", ""
}
