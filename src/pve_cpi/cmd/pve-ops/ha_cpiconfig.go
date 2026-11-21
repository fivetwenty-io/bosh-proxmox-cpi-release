package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
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
	// DirectorUUID names the Director whose entry this is. A BOSH cpi-config
	// does not carry it, so the doctor reads it from this optional key, from
	// the document's own director_uuid key, or from --director-uuid.
	DirectorUUID string `json:"director_uuid"`
}

// cpiConfigDirectorUUID reads the optional director_uuid key of a cpi-config
// document that is an object. A bare list has none.
func cpiConfigDirectorUUID(data []byte) string {
	var doc struct {
		DirectorUUID string `json:"director_uuid"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.DirectorUUID)
}

// decodeEntryList decodes a JSON list of cpis entries. It decodes the list as
// plain struct decoding does, so a malformed entry reports the text and the
// field path that decoding gives, and then reads the same bytes a second time
// to keep the discovery mode in each entry's properties as the raw text it was
// written with, as the request context does. The override refusal repeats a
// mode of the wrong type, and the decoded form cannot give back 5.0 or an
// object's spacing. Every other property keeps its decoded type. The second
// pass cannot fail where the first succeeded, since it reads less, and it
// returns its error all the same.
func decodeEntryList(data []byte) ([]cpiConfigEntry, error) {
	var list []cpiConfigEntry
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	var members []struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, err
	}
	if len(members) != len(list) {
		return nil, fmt.Errorf("the cpis list read as %d entries and then as %d", len(list), len(members))
	}
	for i := range list {
		if len(members[i].Properties) != len(list[i].Properties) {
			return nil, fmt.Errorf("the properties of cpis entry %d read as %d keys and then as %d",
				i+1, len(list[i].Properties), len(members[i].Properties))
		}
		for key, raw := range members[i].Properties {
			list[i].Properties[key] = jsonrpc.KeepRawDiscoveryMode(key, raw, list[i].Properties[key])
		}
	}
	return list, nil
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
		list, err := decodeEntryList([]byte(trimmed))
		if err != nil {
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
	list, err := decodeEntryList(raw)
	if err != nil {
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
func runHACPIConfig(ctx context.Context, path string, base *config.CPIConfig, logger *log.Logger, directorUUID string, asJSON bool,
	stdout, stderr io.Writer, deps haDeps,
) int {
	data, err := readLimited(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha: cpi-config file: %s\n", err)
		return exitUsage
	}
	entries, err := parseCPIConfig(data)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pve-ops ha: %s\n", err)
		return exitUsage
	}
	documentUUID := cpiConfigDirectorUUID(data)
	multi := &haMultiReport{Clusters: []*haReport{}, SharedExports: []haSharedFinding{}}
	layouts := make([]pve.ClusterVMIDLayout, 0, len(entries))
	owners := make([]string, len(entries))
	var unowned []string
	for i := range entries {
		owners[i] = strings.TrimSpace(entries[i].DirectorUUID)
		if owners[i] == "" {
			owners[i] = documentUUID
		}
		if owners[i] == "" {
			owners[i] = strings.TrimSpace(directorUUID)
		}
		if owners[i] == "" {
			unowned = append(unowned, entries[i].Name)
		}
	}
	scopes := entryScopes(entries, base, owners)
	// One ledger serves every entry, so a node one entry found unreachable is
	// not asked again by the next entry that reaches the same cluster.
	ledger := newHANodeLedger()
	for i := range entries {
		report, layout := checkCPIConfigEntry(ctx, &entries[i], base, logger, deps, scopes[i], ledger, owners[i])
		multi.Clusters = append(multi.Clusters, report)
		layouts = append(layouts, layout)
	}
	if at := noDirectorUUIDReport(multi.Clusters, owners); at >= 0 {
		adviseNoDirectorUUID(multi.Clusters[at], logger, unowned)
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

// noDirectorUUIDReport returns the index of the report that carries the
// once-per-run advice about entries with no owner, or -1 when every entry has
// one. It is the first such entry whose cluster the doctor read, because an
// entry it couldn't read judged nothing under the band-only scope the advice
// describes. When it read none of them, it is the first such entry, so the
// advice is never dropped.
func noDirectorUUIDReport(reports []*haReport, owners []string) int {
	first := -1
	for i, r := range reports {
		if owners[i] != "" {
			continue
		}
		if r.Effective != "unknown" {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}

// adviseNoDirectorUUID says once per run, on the report noDirectorUUIDReport
// picks, that the entries named in unowned are scoped by their VM
// ID bands alone. The band is the only boundary left for those entries, so a
// VM another Director created inside a band is judged as the entry's. The
// condition is one setting of the run, not of each cluster, so repeating it on
// every entry would only bury the other findings.
func adviseNoDirectorUUID(report *haReport, logger *log.Logger, unowned []string) {
	if len(unowned) == 0 {
		return
	}
	names := joinNames(unowned)
	if logger != nil {
		logger.Warn("the doctor has no Director UUID for some cpi-config entries, so it scopes each of them by its VM ID band alone",
			log.String("entries", strings.Join(unowned, ",")))
	}
	if len(unowned) == 1 {
		report.advise(haKindAdvice, unowned[0], "no Director UUID is known for the entry %s, so the doctor scopes it by its "+
			"VM ID band alone and judges every VM in the band, including one that another Director created there. Set "+
			"director_uuid on the entry or on the document, or pass --director-uuid", names)
		return
	}
	report.advise(haKindAdvice, unowned[0], "no Director UUID is known for the entries %s, so the doctor scopes each of "+
		"them by its VM ID band alone and judges every VM in the band, including one that another Director created there. "+
		"Set director_uuid on each entry or on the document, or pass --director-uuid. The doctor says this once for the "+
		"run, on the first of those entries whose cluster it read", names)
}

// joinNames lists names in prose, with an Oxford comma before the last of
// three or more.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

// checkCPIConfigEntry reports one cpis entry and gathers the layout the
// cross-cluster check compares. Any step that cannot finish leaves the layout
// unchecked with the reason, and the report carries the same reason as a
// finding. ledger is the run's record of nodes that cannot answer.
func checkCPIConfigEntry(ctx context.Context, entry *cpiConfigEntry, base *config.CPIConfig, logger *log.Logger,
	deps haDeps, scope *haEntryScope, ledger *haNodeLedger, owner string,
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
	report := collectHAScoped(ctx, newLedgerHAReader(reader, ledger), eff, owner, deps.now(), scope)
	report.Cluster = entry.Name
	for _, key := range unknown {
		report.advise(haKindAdvice, key, "the entry's property %q is not one the CPI reads, so it was ignored", key)
	}
	return report, layout
}

// haEntryScope says which VMs one cpi-config entry judges under its local
// policy. A VM carries no mark of the entry that created it, so the entry's VM
// ID band is the only boundary the CPI itself draws. Entries that share a
// cluster and a band cannot be told apart, and a VM in that band is judged once,
// by the first of them, without naming a policy as if it were known.
type haEntryScope struct {
	// Name is the entry's name, and Band is its VM ID band.
	Name string
	Band pve.VMIDBand
	// Peers names the other entries on the same cluster and band, and
	// PeerPolicies holds the local policy of each, in the same order.
	Peers        []string
	PeerPolicies []string
	// Policy is this entry's own local policy.
	Policy string
	// AntiAffinity says whether this entry writes anti-affinity rules, and
	// PeerAntiAffinity says the same for each peer, in the same order as Peers.
	AntiAffinity     bool
	PeerAntiAffinity []bool
	// PeerConfigs holds the effective configuration of each peer, in the same
	// order as Peers, so a per-VM check can tell whether a peer writes what
	// the VM carries.
	PeerConfigs []*config.CPIConfig
	// Leader is true for the first entry of its group.
	Leader bool
	// Cluster lists every entry that reaches the same cluster, this one
	// included, in cpi-config order, whatever its band.
	Cluster []haClusterEntry
}

// haClusterEntry is one cpi-config entry on the cluster a run reads. A group's
// members can lie in the bands of several entries, and each entry plans the
// group with its own default pattern when no member's create named one.
type haClusterEntry struct {
	Name string
	Band pve.VMIDBand
	// AntiAffinity says whether the entry writes anti-affinity rules, and
	// Pattern is its pve.ha.anti_affinity_pattern default.
	AntiAffinity bool
	Pattern      aapattern.Pattern
	// AZMap is the entry's placement.az_map, which decides the domain a plan
	// gives each member that a strict pin confines.
	AZMap map[string][]string
	// Owner is the Director UUID the run judges the entry for, or "" when it
	// knows none.
	Owner string
	// Self marks the entry the scope belongs to.
	Self bool
}

// holds reports whether g could have been created through the entry: its
// band holds g's VM ID, and its Director may judge g (see ownerAdmits).
func (e *haClusterEntry) holds(g *haGuest) bool {
	return e.Band.Contains(g.ref.VMID) && ownerAdmits(e.Owner, g)
}

// ambiguous reports whether the entries that share the band disagree on the
// local policy, which is the only case where the owner changes the verdict.
func (s *haEntryScope) ambiguous() bool {
	if s == nil {
		return false
	}
	for _, p := range s.PeerPolicies {
		if p != s.Policy {
			return true
		}
	}
	return false
}

// antiAffinityAmbiguous reports whether the entries that share the band
// disagree on whether they write anti-affinity rules. A group tag carries no
// mark of the entry that stamped it, so then the doctor cannot tell which
// groups should have a rule.
func (s *haEntryScope) antiAffinityAmbiguous() bool {
	if s == nil {
		return false
	}
	for _, p := range s.PeerAntiAffinity {
		if p != s.AntiAffinity {
			return true
		}
	}
	return false
}

func (s *haEntryScope) peerList() string {
	return strings.Join(s.Peers, ", ")
}

func (s *haEntryScope) policyList() string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append([]string{s.Policy}, s.PeerPolicies...) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " or ")
}

// entryScopes works out the scope of every entry. An entry whose properties do
// not apply gets nil and is reported as unchecked by its own run.
// owners holds the Director UUID the run judges each entry for, in the same
// order, and may be nil when no entry has one.
func entryScopes(entries []cpiConfigEntry, base *config.CPIConfig, owners []string) []*haEntryScope {
	type facts struct {
		key     string
		host    string
		name    string
		policy  string
		aa      bool
		band    pve.VMIDBand
		pattern aapattern.Pattern
		azMap   map[string][]string
		cfg     *config.CPIConfig
		owner   string
	}
	all := make([]*facts, len(entries))
	for i := range entries {
		eff, _, _, err := config.ApplyContextOverrides(base, entries[i].Properties)
		if err != nil {
			continue
		}
		band := pve.VMIDBandsFromConfig(eff).VM
		all[i] = &facts{
			key: eff.Host + "|" + band.String(), host: eff.Host, name: entries[i].Name, policy: eff.HALocalPolicyValue(),
			band: band, aa: antiAffinityWritten(eff), pattern: eff.HAAntiAffinityPatternValue(), cfg: eff,
		}
		if i < len(owners) {
			all[i].owner = owners[i]
		}
		if eff.Placement != nil {
			all[i].azMap = eff.Placement.AZMap
		}
	}
	out := make([]*haEntryScope, len(entries))
	for i, f := range all {
		if f == nil {
			continue
		}
		sc := &haEntryScope{Name: f.name, Band: f.band, Policy: f.policy, AntiAffinity: f.aa, Leader: true}
		for j, o := range all {
			if o != nil && o.host == f.host {
				sc.Cluster = append(sc.Cluster, haClusterEntry{
					Name: o.name, Band: o.band, AntiAffinity: o.aa, Pattern: o.pattern, AZMap: o.azMap, Owner: o.owner,
					Self: j == i,
				})
			}
			if o == nil || j == i || o.key != f.key {
				continue
			}
			sc.Peers = append(sc.Peers, o.name)
			sc.PeerPolicies = append(sc.PeerPolicies, o.policy)
			sc.PeerAntiAffinity = append(sc.PeerAntiAffinity, o.aa)
			sc.PeerConfigs = append(sc.PeerConfigs, o.cfg)
			if j < i {
				sc.Leader = false
			}
		}
		out[i] = sc
	}
	return out
}
