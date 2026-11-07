package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cpiConfigEntry is one element of the cpis list of a BOSH cpi-config.
type cpiConfigEntry struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// parseCPIConfig reads the cpis list of a cpi-config document. It accepts JSON
// only, because the CLI carries no YAML parser; convert a YAML cpi-config with
// "bosh int cpi-config.yml --path /cpis" or any YAML-to-JSON tool first.
func parseCPIConfig(data []byte) ([]cpiConfigEntry, error) {
	var doc struct {
		CPIs []cpiConfigEntry `json:"cpis"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the cpi-config file is not JSON with a cpis list (convert YAML to JSON first): %w", err)
	}
	if len(doc.CPIs) == 0 {
		return nil, errors.New("the cpi-config file has no cpis entries")
	}
	seen := map[string]bool{}
	for i := range doc.CPIs {
		name := strings.TrimSpace(doc.CPIs[i].Name)
		if name == "" {
			return nil, fmt.Errorf("cpis entry %d has no name", i+1)
		}
		if seen[name] {
			return nil, fmt.Errorf("cpis entry name %q appears more than once", name)
		}
		seen[name] = true
		doc.CPIs[i].Name = name
	}
	return doc.CPIs, nil
}

// haSharedFinding is a cross-cluster finding in the JSON output.
type haSharedFinding struct {
	Kind     string   `json:"kind"`
	Failing  bool     `json:"failing"`
	Clusters []string `json:"clusters"`
	Backings []string `json:"backings,omitempty"`
	Detail   string   `json:"detail"`
}

// haMultiReport is the doctor's report over every cpis entry.
type haMultiReport struct {
	Clusters      []*haReport       `json:"clusters"`
	SharedExports []haSharedFinding `json:"shared_exports"`
}

// failing reports whether any cluster report holds a problem or any
// cross-cluster finding fails.
func (m *haMultiReport) failing() bool {
	for _, r := range m.Clusters {
		if r.Problems() > 0 {
			return true
		}
	}
	for _, f := range m.SharedExports {
		if f.Failing {
			return true
		}
	}
	return false
}

// runHACPIConfig runs the doctor against every cpis entry and then the
// cross-cluster check over what it read. An entry whose cluster cannot be read
// is reported as unchecked and takes part in no comparison.
func runHACPIConfig(ctx context.Context, path string, base *config.CPIConfig, logger *log.Logger, asJSON bool,
	stdout, stderr io.Writer, deps haDeps,
) int {
	data, err := readLimited(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-cid ha: cpi-config file: %s\n", err)
		return exitUsage
	}
	entries, err := parseCPIConfig(data)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-cid ha: %s\n", err)
		return exitUsage
	}
	multi := &haMultiReport{Clusters: []*haReport{}, SharedExports: []haSharedFinding{}}
	layouts := make([]pve.ClusterVMIDLayout, 0, len(entries))
	for i := range entries {
		report, layout := checkCPIConfigEntry(ctx, &entries[i], base, logger, deps)
		multi.Clusters = append(multi.Clusters, report)
		layouts = append(layouts, layout)
	}
	for _, f := range pve.CheckSharedExports(layouts) {
		multi.SharedExports = append(multi.SharedExports, haSharedFinding{
			Kind: string(f.Kind), Failing: f.Failing(), Clusters: f.Clusters, Backings: f.Backings, Detail: f.Detail,
		})
	}
	if asJSON {
		if code := writeJSON(stdout, stderr, multi); code != exitOK {
			return code
		}
	} else {
		for _, r := range multi.Clusters {
			_, _ = fmt.Fprintf(stdout, "cluster %s\n", r.Cluster)
			r.writeText(stdout, "  ")
			_, _ = fmt.Fprintln(stdout)
		}
		_, _ = fmt.Fprintln(stdout, "shared exports across clusters:")
		if len(multi.SharedExports) == 0 {
			_, _ = fmt.Fprintln(stdout, "  none")
		}
		for _, f := range multi.SharedExports {
			severity := haSeverityAdvice
			if f.Failing {
				severity = haSeverityProblem
			}
			_, _ = fmt.Fprintf(stdout, "  [%s] %s [%s]", severity, f.Kind, strings.Join(f.Clusters, ", "))
			if len(f.Backings) > 0 {
				_, _ = fmt.Fprintf(stdout, " on %s", strings.Join(f.Backings, ", "))
			}
			_, _ = fmt.Fprintf(stdout, ": %s\n", f.Detail)
		}
	}
	if multi.failing() {
		return exitError
	}
	return exitOK
}

// checkCPIConfigEntry reports one cpis entry and gathers the layout the
// cross-cluster check compares. Any step that cannot finish leaves the layout
// unchecked with the reason, and the report carries the same reason as a
// finding.
func checkCPIConfigEntry(ctx context.Context, entry *cpiConfigEntry, base *config.CPIConfig, logger *log.Logger,
	deps haDeps,
) (*haReport, pve.ClusterVMIDLayout) {
	layout := pve.ClusterVMIDLayout{Name: entry.Name}
	fail := func(mode, reason string) (*haReport, pve.ClusterVMIDLayout) {
		layout.Unchecked = reason
		report := &haReport{
			Cluster: entry.Name, Profile: "ha: mode=" + mode + " effective=unknown", Mode: mode, Effective: "unknown",
			Headroom: "unavailable", Nodes: []haNode{}, CPUDomains: []haDomain{}, Findings: []haFinding{},
		}
		report.unchecked("cluster", "%s", reason)
		return report, layout
	}
	eff, _, unknown, err := config.ApplyContextOverrides(base, entry.Properties)
	if err != nil {
		return fail(base.HAModeValue(), fmt.Sprintf("the entry's properties do not apply: %s", err))
	}
	layout.Bands = pve.VMIDBandsFromConfig(eff)
	reader, err := deps.openReader(eff, logger)
	if err != nil {
		return fail(eff.HAModeValue(), fmt.Sprintf("cannot reach the cluster: %s", err))
	}
	storages, err := reader.Storages(ctx)
	if err != nil {
		return fail(eff.HAModeValue(), fmt.Sprintf("the storage index could not be read: %s", err))
	}
	next, err := reader.NextID(ctx)
	if err != nil {
		return fail(eff.HAModeValue(), fmt.Sprintf("the datacenter next-id range could not be read: %s", err))
	}
	layout.Storages, layout.NextID = storages, next
	report := collectHA(ctx, reader, eff, "", deps.now())
	report.Cluster = entry.Name
	for _, key := range unknown {
		report.advise(haKindAdvice, key, "the entry's property %q is not one the CPI reads, so it was ignored", key)
	}
	return report, layout
}
