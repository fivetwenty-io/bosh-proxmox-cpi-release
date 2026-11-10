package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cpiConfigEntry is one element of the cpis list of a BOSH cpi-config.
type cpiConfigEntry struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// UnmarshalJSON decodes an entry as plain struct decoding would, and then keeps
// the discovery mode in its properties as the raw text it was written with
// when it is not a string, a boolean, or null, as the request context does.
// The override refusal repeats such a value, and the decoded form cannot give
// back 5.0 or an object's spacing. Every other property keeps its decoded type.
func (e *cpiConfigEntry) UnmarshalJSON(data []byte) error {
	type plain cpiConfigEntry
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Properties) > 0 {
		var members struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(data, &members); err != nil {
			return err
		}
		for key, raw := range members.Properties {
			decoded.Properties[key] = jsonrpc.KeepRawDiscoveryMode(key, raw, decoded.Properties[key])
		}
	}
	*e = cpiConfigEntry(decoded)
	return nil
}

// haCPIType is the cpis entry type this CPI serves. An entry of any other type
// belongs to another CPI, so the doctor neither probes nor compares it.
const haCPIType = "pve"

// cpiConfigRenderedKeys are top-level keys of the CPI's own rendered config,
// /var/vcap/jobs/pve_cpi/config/cpi.json. A file that holds them and no cpis
// list is that config, which --config takes.
var cpiConfigRenderedKeys = []string{"host", "node", "vm_storage", "disk_storage", "user", "realm"}

// parseCPIConfig reads the type: pve entries of a cpi-config document. It
// accepts JSON only, because the CLI carries no YAML parser. The document is
// either an object with a cpis list or the bare list. "bosh cpi-config" prints
// YAML, so convert it first, for example with "yq -o=json . cpi-config.yml".
// The rendered job config, /var/vcap/jobs/pve_cpi/config/cpi.json, is JSON as
// well but holds the base config and no cpis, so --config takes it and this
// flag refuses it. An entry whose type is not pve is dropped, and an entry
// with no type counts as pve.
func parseCPIConfig(data []byte) ([]cpiConfigEntry, error) {
	entries, err := decodeCPIEntries(data)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("the cpi-config file has no cpis entries")
	}
	seen := map[string]bool{}
	out := make([]cpiConfigEntry, 0, len(entries))
	for i := range entries {
		name := strings.TrimSpace(entries[i].Name)
		if name == "" {
			return nil, fmt.Errorf("cpis entry %d has no name", i+1)
		}
		if seen[name] {
			return nil, fmt.Errorf("cpis entry name %q appears more than once", name)
		}
		seen[name] = true
		entries[i].Name = name
		if kind := strings.TrimSpace(entries[i].Type); kind != "" && !strings.EqualFold(kind, haCPIType) {
			continue
		}
		out = append(out, entries[i])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the cpi-config file has no cpis entry of type %s", haCPIType)
	}
	return out, nil
}

// decodeCPIEntries decodes the cpis list from either accepted document shape.
func decodeCPIEntries(data []byte) ([]cpiConfigEntry, error) {
	const notJSON = "the cpi-config file is not JSON with a cpis list (convert the YAML from \"bosh cpi-config\" first, " +
		"for example with \"yq -o=json . cpi-config.yml\")"
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var list []cpiConfigEntry
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("%s: %w", notJSON, err)
		}
		return list, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", notJSON, err)
	}
	raw, ok := doc["cpis"]
	if !ok {
		for _, key := range cpiConfigRenderedKeys {
			if _, rendered := doc[key]; rendered {
				return nil, errors.New("the file is the CPI's own config (the rendered " +
					"/var/vcap/jobs/pve_cpi/config/cpi.json), which --config takes; --cpi-config takes the Director's " +
					"cpi-config as JSON with a cpis list")
			}
		}
		return nil, errors.New("the cpi-config file has no cpis entries")
	}
	var list []cpiConfigEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the cpis value of the cpi-config file is not a list of entries: %w", err)
	}
	return list, nil
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
