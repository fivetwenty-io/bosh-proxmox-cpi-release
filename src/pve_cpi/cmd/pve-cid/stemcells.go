package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// stemcellFilenamePattern matches the canonical stemcell qcow2 filename
// shape built by internal/pve.BuildStemcellFilename:
// "bosh-stemcell-<name>-<version>-<sha8>.qcow2". Storage content items whose
// basename does not match this shape are not stemcell files and are
// excluded from the inventory.
var stemcellFilenamePattern = regexp.MustCompile(`^bosh-stemcell-.+-[0-9a-f]{8}\.qcow2$`)

// StemcellFileRecord is one bosh-stemcell-*.qcow2 content item found on the
// scanned storage. Node is the PVE node the content listing was queried on:
// on shared storage every node reports the same content and Node is the
// single node the scan used; on node-local storage each node's copy is
// listed separately and Node identifies which node actually holds that
// backing file — the field per-node presence tracking depends on to avoid
// treating a replica's own-node copy as absent just because it was not the
// node the scan happened to check.
type StemcellFileRecord struct {
	Storage  string `json:"storage"`
	Node     string `json:"node"`
	VolID    string `json:"volid"`
	Filename string `json:"filename"`
	SHA8     string `json:"sha8"`
}

// StemcellTemplateRecord is one bosh-stemcell-tagged cache template found by
// the cluster-wide scan, with tag-derived and (when parseable)
// description-provenance-derived fields.
type StemcellTemplateRecord struct {
	VMID          int      `json:"vmid"`
	Node          string   `json:"node"`
	Name          string   `json:"name"`
	NameTag       string   `json:"name_tag,omitempty"`
	VersionTag    string   `json:"version_tag,omitempty"`
	SHA8Tag       string   `json:"sha8_tag,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	CID           string   `json:"cid,omitempty"`
	HasProvenance bool     `json:"has_provenance"`
	DirectorRefs  []string `json:"director_refs,omitempty"`

	// CurrentGeneration reports whether this template carries a marker
	// proving it belongs to a CPI generation that uses reference tags — the
	// cache tag or a non-empty director-- ref (pve.HasStemcellGenerationMarker,
	// the same predicate the CPI's own template lookup filters on).
	//
	// false means a previous-generation leftover: it carries the same content
	// sha tag, but the running CPI will neither adopt nor delete it. Such a
	// template is deliberately still listed — an operator needs to see it to
	// clean it up by hand — so this flag is what keeps "listed" from reading
	// as "owned".
	CurrentGeneration bool `json:"current_generation"`
}

// StemcellInventoryEntry groups every cache template and qcow2 file sharing
// one sha8 — the key both the template's "bosh-stemcell-sha-<sha8>" tag and
// the qcow2 filename's trailing "-<sha8>.qcow2" segment carry, per
// internal/pve.BuildStemcellFilename/BuildTemplateNameWithSHA.
//
// A template with no sha tag (an S-3-population template: server download
// without cloud_properties.sha256, or a stale prefix-dedup artifact) cannot
// be correlated by sha8 at all. Rather than collapsing every such template
// into one SHA8=="" bucket — which would present unrelated templates as one
// logical stemcell — buildStemcellInventory gives each its own single-template
// entry with Untagged set and SHA8 left empty; VMID identifies it uniquely.
type StemcellInventoryEntry struct {
	SHA8          string                   `json:"sha8"`
	Untagged      bool                     `json:"untagged,omitempty"`
	Templates     []StemcellTemplateRecord `json:"templates,omitempty"`
	Files         []StemcellFileRecord     `json:"files,omitempty"`
	Orphan        bool                     `json:"orphan"`
	OrphanReasons []string                 `json:"orphan_reasons,omitempty"`
}

// buildStemcellInventory scans the cluster for bosh-stemcell-tagged cache
// templates and groups them by sha8 tag, scans the named storage's import/
// content for backing qcow2 files, and classifies each group as an orphan
// candidate per classifyStemcellOrphan.
//
// Storage-content scanning scope depends on whether storage is cluster-shared
// (via r.StorageIsShared, the same GET /storage "shared" classification
// internal/pve.StorageInfo.IsShared uses):
//
//   - shared: one content listing on node covers the whole cluster (PVE
//     serves the same content index for a shared storage regardless of which
//     node answers), so a single-node scan is sufficient and cheapest.
//   - node-local, or undetermined (r.StorageIsShared's known=false — treated
//     as local, the safer / more-scanning choice): every replica of a
//     replicated stemcell holds its own on-disk copy under the same storage
//     ID, so a single-node scan only ever sees that one node's copy and
//     reports every other replica as a missing backing file. Scanning every
//     cluster node and recording which node each file was found on (Node on
//     StemcellFileRecord) lets classifyStemcellOrphan check presence against
//     each template's own node instead of against one arbitrarily chosen
//     scan node.
func buildStemcellInventory(ctx context.Context, r Reader, node, storage string) ([]StemcellInventoryEntry, error) {
	vms, err := r.ListClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	templatesBySHA8, untaggedTemplates := collectStemcellTemplates(ctx, r, vms)

	sharedStorage := storageScanIsShared(ctx, r, storage)
	scanNodes, err := resolveStemcellScanNodes(ctx, r, node, sharedStorage)
	if err != nil {
		return nil, err
	}
	filesBySHA8, err := scanStemcellFiles(ctx, r, scanNodes, storage)
	if err != nil {
		return nil, err
	}

	out := assembleStemcellInventory(templatesBySHA8, filesBySHA8, untaggedTemplates, sharedStorage)
	sortStemcellInventory(out)
	return out, nil
}

// storageScanIsShared re-derives the shared/known pair from r.StorageIsShared
// for use in classification (kept as a separate, named call so the
// scan-scope decision in resolveStemcellScanNodes and the classification
// input here read the same underlying fact without threading an extra bool
// through buildStemcellInventory's signature).
func storageScanIsShared(ctx context.Context, r Reader, storage string) bool {
	shared, known := r.StorageIsShared(ctx, storage)
	return known && shared
}

// collectStemcellTemplates walks vms for bosh-stemcell-tagged templates,
// decoding each one's provenance (best-effort: a config read or provenance
// parse failure just leaves HasProvenance false rather than failing the
// whole scan). Templates carrying a sha8 tag are grouped by that tag;
// templates with no sha tag are returned separately (untagged) so callers
// can give each its own inventory entry instead of collapsing them into one
// SHA8=="" bucket.
func collectStemcellTemplates(ctx context.Context, r Reader, vms []ClusterVM) (map[string][]StemcellTemplateRecord, []StemcellTemplateRecord) {
	templatesBySHA8 := make(map[string][]StemcellTemplateRecord)
	var untaggedTemplates []StemcellTemplateRecord
	for i := range vms {
		vm := &vms[i]
		if !vm.Template {
			// PVE copies a template's tags onto every clone, so a running VM
			// built from a cache template carries the full stemcell tag set.
			// Without this check the inventory reported live VMs as templates
			// — on the V5 baseline, 12 of AZ1's 15 reported templates were
			// running cf VMs. This mirrors listClusterQemuTemplates, the
			// filter the CPI's own lookup uses.
			continue
		}
		tokens := splitPVETags(vm.Tags)
		// Either marker qualifies. The bare bosh-stemcell tag alone missed
		// cache templates whose tag set carries a sha tag and director-- refs
		// but not that exact token (hasTagToken is an exact-token match) —
		// VMID 30006 was the live example.
		//
		// pve.HasStemcellGenerationMarker is the CPI's OWN predicate, shared
		// rather than re-derived so the tool's notion of "this generation"
		// cannot drift from the CPI's. The CLI accepts it ALONGSIDE the bare
		// marker rather than instead of it: the CPI ignores
		// previous-generation templates on purpose (adopting one would
		// destroy a live template on the first last-ref delete), but an
		// operator inventory must still list them — surfacing leftovers is
		// most of what an inventory is for. They are listed and LABELLED,
		// never silently mixed in with templates this CPI owns.
		currentGeneration := pve.HasStemcellGenerationMarker(tokens)
		if !hasTagToken(tokens, stemcellMarkerTag) && !currentGeneration {
			continue
		}
		rec := StemcellTemplateRecord{
			VMID:       vm.VMID,
			Node:       vm.Node,
			Name:       vm.Name,
			NameTag:    tagValue(tokens, stemcellNameTagPrefix),
			VersionTag: tagValue(tokens, stemcellVersionTagPrefix),
			SHA8Tag:    tagValue(tokens, stemcellSHATagPrefix),

			CurrentGeneration: currentGeneration,
		}
		if cfg, cfgErr := r.VMConfig(ctx, vm.Node, vm.VMID); cfgErr == nil && cfg != nil {
			desc := pve.DescriptionFromConfig(cfg)
			if prov, ok := parseTemplateProvenance(desc); ok {
				rec.HasProvenance = true
				rec.Kind = prov.Kind
				rec.CID = prov.CID
				rec.DirectorRefs = prov.DirectorRefs
			}
		}
		if rec.SHA8Tag == "" {
			untaggedTemplates = append(untaggedTemplates, rec)
			continue
		}
		templatesBySHA8[rec.SHA8Tag] = append(templatesBySHA8[rec.SHA8Tag], rec)
	}
	return templatesBySHA8, untaggedTemplates
}

// resolveStemcellScanNodes returns the set of PVE nodes buildStemcellInventory
// must query storage content on. sharedStorage == true needs only node (PVE
// serves the same content index cluster-wide for a shared storage);
// sharedStorage == false — node-local storage, or storage whose shared
// classification could not be determined — is scanned on every cluster node
// so a replica's own-node backing file is never reported missing just
// because it lives on a node the scan did not happen to check. Falls
// back to []string{node} when the cluster node list cannot be read or comes
// back empty, matching the pre-fix single-node behavior rather than failing
// the whole inventory.
func resolveStemcellScanNodes(ctx context.Context, r Reader, node string, sharedStorage bool) ([]string, error) {
	if sharedStorage {
		return []string{node}, nil
	}
	allNodes, err := r.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	if len(allNodes) == 0 {
		return []string{node}, nil
	}
	return allNodes, nil
}

// scanStemcellFiles queries storage content on every node in scanNodes,
// keeps only import/ items matching the canonical stemcell qcow2 filename
// shape, and groups the resulting StemcellFileRecords by sha8.
func scanStemcellFiles(ctx context.Context, r Reader, scanNodes []string, storage string) (map[string][]StemcellFileRecord, error) {
	filesBySHA8 := make(map[string][]StemcellFileRecord)
	for _, scanNode := range scanNodes {
		files, err := r.ListStorageContent(ctx, scanNode, storage)
		if err != nil {
			return nil, err
		}
		for i := range files {
			f := &files[i]
			if !strings.Contains(f.VolID, "/import/") && !strings.Contains(f.VolID, ":import/") {
				continue
			}
			filename := path.Base(f.VolID)
			if !stemcellFilenamePattern.MatchString(filename) {
				continue
			}
			sha8 := sha8FromStemcellFilename(filename)
			rec := StemcellFileRecord{Storage: storage, Node: scanNode, VolID: f.VolID, Filename: filename, SHA8: sha8}
			filesBySHA8[sha8] = append(filesBySHA8[sha8], rec)
		}
	}
	return filesBySHA8, nil
}

// assembleStemcellInventory pairs templatesBySHA8/filesBySHA8 groups and
// untaggedTemplates into classified StemcellInventoryEntry values.
func assembleStemcellInventory(
	templatesBySHA8 map[string][]StemcellTemplateRecord,
	filesBySHA8 map[string][]StemcellFileRecord,
	untaggedTemplates []StemcellTemplateRecord,
	sharedStorage bool,
) []StemcellInventoryEntry {
	keySet := make(map[string]struct{}, len(templatesBySHA8)+len(filesBySHA8))
	for k := range templatesBySHA8 {
		keySet[k] = struct{}{}
	}
	for k := range filesBySHA8 {
		keySet[k] = struct{}{}
	}

	out := make([]StemcellInventoryEntry, 0, len(keySet)+len(untaggedTemplates))
	for k := range keySet {
		entry := StemcellInventoryEntry{
			SHA8:      k,
			Templates: templatesBySHA8[k],
			Files:     filesBySHA8[k],
		}
		entry.OrphanReasons = classifyStemcellOrphan(entry, sharedStorage)
		entry.Orphan = len(entry.OrphanReasons) > 0
		out = append(out, entry)
	}
	for i := range untaggedTemplates {
		entry := StemcellInventoryEntry{Untagged: true, Templates: untaggedTemplates[i : i+1]}
		entry.OrphanReasons = classifyStemcellOrphan(entry, sharedStorage)
		entry.Orphan = len(entry.OrphanReasons) > 0
		out = append(out, entry)
	}
	return out
}

// sortStemcellInventory orders entries by SHA8, breaking ties on the first
// template's VMID (every untagged entry shares SHA8 == "", so this is what
// keeps their relative order deterministic run to run).
func sortStemcellInventory(entries []StemcellInventoryEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].SHA8 != entries[j].SHA8 {
			return entries[i].SHA8 < entries[j].SHA8
		}
		iVMID, jVMID := 0, 0
		if len(entries[i].Templates) > 0 {
			iVMID = entries[i].Templates[0].VMID
		}
		if len(entries[j].Templates) > 0 {
			jVMID = entries[j].Templates[0].VMID
		}
		return iVMID < jVMID
	})
}

// classifyStemcellOrphan returns the set of reasons entry is an orphan
// candidate ("safe to review for deletion"), per the operator-facing
// contract of "pve-cid stemcells --orphans":
//
//   - templates carrying zero director references (the aggregate across
//     every template sharing this sha8, since replicas legitimately split
//     as separate template rows but share one logical ref set)
//   - an untagged template (no sha8 tag): its backing qcow2 cannot be
//     correlated by sha8 at all, a distinct condition from "correlated but
//     missing" below
//   - a qcow2 file with no correlated cache template at all — its kind
//     (light/heavy) cannot be determined, since kind lives only in template
//     provenance
//   - a template whose backing qcow2 file is absent from the scanned
//     storage's content listing (freeze completed but the file is gone —
//     the file:template relationship broke). On shared storage any file
//     found for the sha8 proves the backing exists; on node-local (or
//     undetermined) storage the file must have been found specifically on
//     that template's own node, per sharedStorage.
func classifyStemcellOrphan(entry StemcellInventoryEntry, sharedStorage bool) []string {
	var reasons []string

	if len(entry.Templates) > 0 {
		totalRefs := 0
		for i := range entry.Templates {
			totalRefs += len(entry.Templates[i].DirectorRefs)
		}
		if totalRefs == 0 {
			reasons = append(reasons, "template(s) carry zero director references")
		}
	}

	switch {
	case entry.Untagged:
		for i := range entry.Templates {
			t := &entry.Templates[i]
			if t.HasProvenance {
				reasons = append(reasons, fmt.Sprintf(
					"template %d (%s): no sha tag — backing qcow2 file cannot be correlated by sha8", t.VMID, t.Name))
			}
		}
	case len(entry.Templates) == 0 && len(entry.Files) > 0:
		reasons = append(reasons, "qcow2 file has no correlated cache template; kind (light/heavy) cannot be determined")
	case len(entry.Templates) > 0:
		filesByNode := make(map[string]bool, len(entry.Files))
		haveAnyFile := len(entry.Files) > 0
		for i := range entry.Files {
			filesByNode[entry.Files[i].Node] = true
		}
		for i := range entry.Templates {
			t := &entry.Templates[i]
			if !t.HasProvenance {
				continue
			}
			if sharedStorage {
				if !haveAnyFile {
					reasons = append(reasons, fmt.Sprintf(
						"template %d (%s): backing qcow2 file not found on the scanned storage", t.VMID, t.Name))
				}
				continue
			}
			if !filesByNode[t.Node] {
				reasons = append(reasons, fmt.Sprintf(
					"template %d (%s) on node %s: backing qcow2 file not found on that node's copy of the storage",
					t.VMID, t.Name, t.Node))
			}
		}
	}

	return reasons
}

func filterOrphans(entries []StemcellInventoryEntry) []StemcellInventoryEntry {
	out := make([]StemcellInventoryEntry, 0, len(entries))
	for _, e := range entries {
		if e.Orphan {
			out = append(out, e)
		}
	}
	return out
}

// runStemcells implements:
//
//	pve-cid stemcells [--storage ID] [--node N] [--orphans] [--config PATH] [--json]
func runStemcells(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pve-cid stemcells", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to CPI JSON config file (default: $PVE_CPI_CONFIG or "+defaultConfigPath+")")
	storageFlag := fs.String("storage", "", "PVE storage ID to scan for stemcell qcow2 files (default: the CPI config's stemcell_storage)")
	nodeFlag := fs.String("node", "", "PVE node to query storage content on when the storage is cluster-shared, or to fall back to if the cluster node list cannot be read on node-local storage (default: the CPI config's node, or the first cluster node)")
	orphans := fs.Bool("orphans", false, "show only orphan candidates (zero director refs, untagged templates, uncorrelated files, missing backing files)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of a table")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: pve-cid stemcells [--storage ID] [--node N] [--orphans] [--config PATH] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "pve-cid stemcells: unexpected positional arguments: %v\n", fs.Args())
		fs.Usage()
		return exitUsage
	}

	cfg, reader, err := loadConfigAndReader(*configPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	ctx := context.Background()

	storage := *storageFlag
	if storage == "" {
		storage = cfg.StemcellStorage
	}
	if storage == "" {
		_, _ = fmt.Fprintln(stderr, "pve-cid stemcells: no stemcell storage configured (pve.stemcell_storage) and --storage not given")
		return exitError
	}

	node := *nodeFlag
	if node == "" {
		node = cfg.Node
	}
	if node == "" {
		nodes, nerr := reader.ListNodes(ctx)
		if nerr != nil {
			_, _ = fmt.Fprintln(stderr, nerr)
			return exitError
		}
		if len(nodes) == 0 {
			_, _ = fmt.Fprintln(stderr, "pve-cid stemcells: no cluster nodes found and --node not given")
			return exitError
		}
		sort.Strings(nodes)
		node = nodes[0]
	}

	entries, err := buildStemcellInventory(ctx, reader, node, storage)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}
	if *orphans {
		entries = filterOrphans(entries)
	}

	if *jsonOut {
		return writeJSON(stdout, stderr, entries)
	}
	printStemcellInventoryTable(stdout, entries)
	return exitOK
}

func printStemcellInventoryTable(w io.Writer, entries []StemcellInventoryEntry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "no stemcell templates or files found")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SHA8\tNAME\tVERSION\tTEMPLATES\tGENERATION\tFILES\tDIRECTOR_REFS\tORPHAN")
	for _, e := range entries {
		name, version := "-", "-"
		refCount := 0
		if len(e.Templates) > 0 {
			name = firstNonEmpty(e.Templates[0].NameTag, "-")
			version = firstNonEmpty(e.Templates[0].VersionTag, "-")
			seen := make(map[string]struct{})
			for i := range e.Templates {
				for _, ref := range e.Templates[i].DirectorRefs {
					seen[ref] = struct{}{}
				}
			}
			refCount = len(seen)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%d\t%t\n",
			stemcellEntryLabel(e), name, version, len(e.Templates),
			stemcellEntryGeneration(e), len(e.Files), refCount, e.Orphan)
	}
	_ = tw.Flush()

	for _, e := range entries {
		if len(e.OrphanReasons) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n%s orphan reasons:\n", stemcellEntryLabel(e))
		for _, reason := range e.OrphanReasons {
			_, _ = fmt.Fprintf(w, "  - %s\n", reason)
		}
	}
}

// stemcellEntryGeneration summarises the GENERATION column for e: whether the
// templates grouped under this sha8 belong to the running CPI's generation.
//
//	current  — every template carries a cache tag or a director-- ref
//	previous — none do: leftovers the CPI will neither adopt nor delete,
//	           listed so an operator can find and remove them by hand
//	mixed    — both, which normally means a rebuild left the old anchor behind
//	-        — no templates in this entry (a file with no template at all)
//
// Without this column a previous-generation leftover is indistinguishable
// from a template this CPI owns, and "it appears in the inventory" reads as
// "the CPI is managing it" — which is exactly backwards for a leftover.
func stemcellEntryGeneration(e StemcellInventoryEntry) string {
	if len(e.Templates) == 0 {
		return "-"
	}
	current, previous := 0, 0
	for i := range e.Templates {
		if e.Templates[i].CurrentGeneration {
			current++
			continue
		}
		previous++
	}
	switch {
	case previous == 0:
		return "current"
	case current == 0:
		return "previous"
	default:
		return "mixed"
	}
}

// stemcellEntryLabel returns the SHA8 column value for e: the sha8 tag for a
// correlated group, or "untagged:<vmid>" for an Untagged entry (SHA8 is
// always "" on those, and every unrelated untagged template gets its own
// entry, so the VMID alone identifies it unambiguously).
func stemcellEntryLabel(e StemcellInventoryEntry) string {
	if e.Untagged && len(e.Templates) > 0 {
		return fmt.Sprintf("untagged:%d", e.Templates[0].VMID)
	}
	return e.SHA8
}

func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
