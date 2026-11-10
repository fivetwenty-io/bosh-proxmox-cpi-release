package ha

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CPUInfo is one node's CPU as GET /nodes/{node}/status reports it.
type CPUInfo struct {
	// Known is true when the node's status was read and named a CPU model. A
	// node whose status cannot be read has an unknown CPU, and no CPU migration
	// domain includes it.
	Known bool `json:"known"`

	// Model is the CPU model string.
	Model string `json:"model"`

	// Flags are the CPU flags PVE reports, sorted and without duplicates. They
	// are empty when PVE reports none.
	Flags []string `json:"flags"`
}

// CPUDomain is a set of nodes that report the same CPU model and the same
// flags. A VM whose CPU type is host or max can migrate only inside its
// domain.
type CPUDomain struct {
	// Model is the CPU model string the nodes share.
	Model string

	// Flags are the CPU flags the nodes share.
	Flags []string

	// Nodes lists the nodes of the domain, sorted by name.
	Nodes []string
}

// Contains reports whether node is a member of the domain.
func (d CPUDomain) Contains(node string) bool {
	for _, n := range d.Nodes {
		if n == node {
			return true
		}
	}
	return false
}

// key identifies the domain. Two nodes with the same model whose flags differ
// get different keys, and so does a node that reports no flags beside one that
// reports some, because a node without flags cannot be shown to match.
func (c CPUInfo) key() string {
	return c.Model + "\x00" + strings.Join(c.Flags, " ")
}

// parseCPUInfo reads the cpuinfo object of a node status. A missing or null
// object, or one without a model, is an unknown CPU rather than an error,
// because a node that reports no model simply has no domain. A value of the
// wrong shape is an error.
func parseCPUInfo(raw json.RawMessage) (CPUInfo, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return CPUInfo{}, nil
	}
	var wire struct {
		Model string `json:"model"`
		Flags string `json:"flags"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return CPUInfo{}, fmt.Errorf("cpuinfo: %w", err)
	}
	model := strings.TrimSpace(wire.Model)
	if model == "" {
		return CPUInfo{}, nil
	}
	return CPUInfo{Known: true, Model: model, Flags: normalizeFlags(wire.Flags)}, nil
}

// normalizeFlags splits a space-separated flag list into a sorted slice
// without duplicates.
func normalizeFlags(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	sort.Strings(fields)
	out := fields[:0]
	for i, f := range fields {
		if i == 0 || f != fields[i-1] {
			out = append(out, f)
		}
	}
	return out
}

// groupCPUDomains groups the nodes with a known CPU into domains and returns
// them sorted by their first node, along with the sorted names of the nodes
// whose CPU is unknown.
func groupCPUDomains(cpus map[string]CPUInfo) (domains []CPUDomain, unknown []string) {
	byKey := make(map[string]*CPUDomain)
	for name, cpu := range cpus {
		if !cpu.Known {
			unknown = append(unknown, name)
			continue
		}
		d, ok := byKey[cpu.key()]
		if !ok {
			d = &CPUDomain{Model: cpu.Model, Flags: cpu.Flags}
			byKey[cpu.key()] = d
		}
		d.Nodes = append(d.Nodes, name)
	}
	for _, d := range byKey {
		sort.Strings(d.Nodes)
		domains = append(domains, *d)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Nodes[0] < domains[j].Nodes[0] })
	sort.Strings(unknown)
	return domains, unknown
}
