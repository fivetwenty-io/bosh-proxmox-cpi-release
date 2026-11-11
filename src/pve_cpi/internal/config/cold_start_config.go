package config

import (
	"slices"
	"sort"
	"strings"
)

// ColdStartNodeNames returns the node names of cfg's placement.az_map that
// the CPI may try when it starts with no usable endpoint cache and every
// configured endpoint is down. It returns nil whenever any condition fails or
// cannot be determined, because the names it returns are the only way a token
// can reach a node the operator never listed in pve.host or pve.endpoints.
//
// Every one of these must hold.
//   - Discovery resolves to on (see EffectiveDiscoveryMode), so an explicit
//     off, an unusable mode, password auth under auto, a second configured
//     endpoint under auto, and an entry that inherited the job's credentials
//     under looser TLS all rule it out.
//   - verify_ssl is true and pve.ca_cert is set, so a candidate has to chain
//     to the operator's own trust anchor.
//   - The config authenticates with an API token. A password turns the cold
//     start off even when the mode is explicitly on.
//   - placement.az_map names at least one node.
//   - The map belongs to cfg. That is true for the job-level block, which is
//     the job itself, for an entry that named pve_placement itself, and for
//     an entry whose endpoint list is the job's (see SameEndpointSet). An
//     entry that inherits the job's map under another list gets none, because
//     an az_map names the nodes of one cluster.
//
// cfg is the effective config of the job or of one cpi-config entry, and job
// is the job-level config the entry was built from. The job-level block
// passes the same pointer for both, and a copy of the job does not count as
// the job. A nil job leaves only an entry that named its own placement.
//
// The names come in AZ-name order and then the operator's order within each
// AZ, without duplicates or empty names. They are not checked against the
// node-name helper, which sits in internal/endpointcache and cannot be
// imported here. The caller drops and logs each name that fails
// endpointcache.ValidateNodeName, and derives the addresses.
func ColdStartNodeNames(cfg, job *CPIConfig) []string {
	if !coldStartConfigAllows(cfg) || !ownsColdStartMap(cfg, job) {
		return nil
	}
	azNames := make([]string, 0, len(cfg.Placement.AZMap))
	for az := range cfg.Placement.AZMap {
		azNames = append(azNames, az)
	}
	sort.Strings(azNames)
	var names []string
	for _, az := range azNames {
		for _, node := range cfg.Placement.AZMap[az] {
			if node == "" || slices.Contains(names, node) {
				continue
			}
			names = append(names, node)
		}
	}
	return names
}

// coldStartConfigAllows reports whether cfg meets the conditions on its own
// settings, which are discovery on, verify_ssl true, a CA certificate, token
// auth, and a non-empty az_map.
func coldStartConfigAllows(cfg *CPIConfig) bool {
	if cfg == nil {
		return false
	}
	if mode, _ := EffectiveDiscoveryMode(cfg); mode != DiscoveryModeOn {
		return false
	}
	if !cfg.VerifySSLValue() || strings.TrimSpace(cfg.PVECACertPEM) == "" {
		return false
	}
	if cfg.APIToken == "" {
		return false
	}
	return cfg.Placement != nil && len(cfg.Placement.AZMap) > 0
}

// ownsColdStartMap reports whether cfg's az_map names nodes of cfg's own
// cluster. See ColdStartNodeNames for the three ways it can.
func ownsColdStartMap(cfg, job *CPIConfig) bool {
	return cfg == job || cfg.placementNamedByEntry || SameEndpointSet(cfg, job)
}
