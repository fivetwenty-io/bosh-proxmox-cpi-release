package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// DiskHolder identifies the active bus slot on a live VM currently holding a
// disk volume.
type DiskHolder struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Slot string `json:"slot"`
}

// SentinelMatch records a bosh_attached_disks/bosh_parked_disks sentinel hit
// on one VM's description for a given bare volid, independent of whether
// that VM's bus slots currently hold the volume. A sentinel match with no
// corresponding bus-slot holder anywhere in the cluster flags a stale
// sentinel (e.g. a disk manually detached outside the CPI).
type SentinelMatch struct {
	VMID        int              `json:"vmid"`
	Node        string           `json:"node"`
	AttachedCID string           `json:"attached_cid,omitempty"`
	Parked      *parkedDiskEntry `json:"parked,omitempty"`
}

// DiskLocateResult is the result of locating a disk volume across the
// cluster.
type DiskLocateResult struct {
	BareVolid string `json:"bare_volid"`
	// StableID is the bpd- identity token from the CID envelope, when the
	// located CID carries one; the scan then also matches drive entries by
	// their serial= option (the identity survives PVE's move_disk rename,
	// the birth volid does not).
	StableID string `json:"stable_id,omitempty"`
	// CurrentVolid is the volid the holder's drive entry actually carries.
	// Differs from BareVolid after a reassignment renamed the volume.
	CurrentVolid    string          `json:"current_volid,omitempty"`
	Holder          *DiskHolder     `json:"holder,omitempty"`
	SentinelMatches []SentinelMatch `json:"sentinel_matches,omitempty"`
}

// locateDisk scans every cluster VM for bareVolid on an active bus slot
// (scsi/virtio/ide/sata — this is the exact set qemu.ParseDisks recognizes,
// the same helper internal/pve's own FindVMByDiskVolid uses) and,
// independently, for a bosh_attached_disks or bosh_parked_disks sentinel
// entry naming bareVolid on every VM's description regardless of bus-slot
// presence.
//
// VMs whose config cannot be fetched are skipped (best-effort operator
// scan, matching scripts/disk-audit's tolerance of individual VM fetch
// failures) rather than aborting the whole locate.
//
// Returns an error only when the initial cluster VM listing itself fails;
// per-VM config fetch failures are silently skipped.
func locateDisk(ctx context.Context, r Reader, bareVolid, stableID string) (*DiskLocateResult, error) {
	if bareVolid == "" {
		return nil, fmt.Errorf("pve-cid: locateDisk: bareVolid must not be empty")
	}

	vms, err := r.ListClusterVMs(ctx)
	if err != nil {
		return nil, err
	}
	// Deterministic scan order for reproducible output across runs (map
	// iteration inside ListClusterVMs' JSON decode is already ordered by
	// slice append, but sort explicitly so callers never depend on API
	// response ordering).
	sort.Slice(vms, func(i, j int) bool { return vms[i].VMID < vms[j].VMID })

	result := &DiskLocateResult{BareVolid: bareVolid, StableID: stableID}

	for _, vm := range vms {
		cfg, cfgErr := r.VMConfig(ctx, vm.Node, vm.VMID)
		if cfgErr != nil || cfg == nil {
			continue
		}

		disks := qemu.ParseDisks(cfg)
		if result.Holder == nil {
			if slot, ok := pve.FindDiskIDByVolID(disks, bareVolid); ok {
				result.Holder = &DiskHolder{VMID: vm.VMID, Node: vm.Node, Slot: slot}
				result.CurrentVolid = bareVolid
			} else if stableID != "" {
				// Mirror the CPI's identity resolution: a serial=<stableID>
				// drive option identifies the disk after a reassignment
				// renamed the volume away from its birth volid.
				for slot, optStr := range disks {
					if serial, has := pve.StableIDFromDriveOptStr(optStr); has && serial == stableID {
						result.Holder = &DiskHolder{VMID: vm.VMID, Node: vm.Node, Slot: slot}
						result.CurrentVolid = bareVolidFromDriveOptStr(optStr)
						break
					}
				}
			}
		}

		desc := pve.DescriptionFromConfig(cfg)
		match := SentinelMatch{VMID: vm.VMID, Node: vm.Node}
		matched := false
		if cid, ok := readAttachedDiskCID(desc, stableID, bareVolid); ok {
			match.AttachedCID = cid
			matched = true
		}
		if entry, ok := readParkedDiskEntry(desc, bareVolid, stableID); ok {
			e := entry
			match.Parked = &e
			matched = true
		}
		if matched {
			result.SentinelMatches = append(result.SentinelMatches, match)
		}
	}

	return result, nil
}

// bareVolidFromDriveOptStr strips the option suffix from a PVE drive value.
func bareVolidFromDriveOptStr(optStr string) string {
	if idx := strings.IndexByte(optStr, ','); idx >= 0 {
		return optStr[:idx]
	}
	return optStr
}

// StemcellTemplateHit is one cache template matched by sha8 during a
// stemcell locate scan.
type StemcellTemplateHit struct {
	VMID          int      `json:"vmid"`
	Node          string   `json:"node"`
	Name          string   `json:"name"`
	HasProvenance bool     `json:"has_provenance"`
	DirectorRefs  []string `json:"director_refs,omitempty"`
}

// StemcellLocateResult is the result of locating a stemcell path CID across
// the cluster.
type StemcellLocateResult struct {
	CID          string                `json:"cid"`
	Kind         string                `json:"kind"`
	Storage      string                `json:"storage"`
	VolumePath   string                `json:"volume_path"`
	Filename     string                `json:"filename"`
	SHA8         string                `json:"sha8"`
	VolumeExists bool                  `json:"volume_exists"`
	Templates    []StemcellTemplateHit `json:"templates,omitempty"`
}

// locateStemcell resolves decoded (a stemcell path CID) to its per-cluster
// cache template(s) — found via the sha8 embedded in the CID's filename,
// through the same cluster-scoped FindTemplatesBySHATagCluster the CPI's
// create/delete_stemcell paths use — and reports whether the backing qcow2
// still exists on the named storage.
func locateStemcell(ctx context.Context, r Reader, decoded *DecodedCID) (*StemcellLocateResult, error) {
	if decoded.Family != FamilyStemcellLight && decoded.Family != FamilyStemcellHeavy {
		return nil, fmt.Errorf("pve-cid: locateStemcell: %q is not a stemcell CID", decoded.Raw)
	}

	kind := string(pve.StemcellKindLight)
	if decoded.Family == FamilyStemcellHeavy {
		kind = string(pve.StemcellKindHeavy)
	}

	result := &StemcellLocateResult{
		CID:        decoded.Raw,
		Kind:       kind,
		Storage:    decoded.Storage,
		VolumePath: decoded.VolumePath,
		Filename:   decoded.Filename,
		SHA8:       decoded.SHA8,
	}

	if result.SHA8 == "" {
		return nil, fmt.Errorf(
			"pve-cid: locateStemcell: could not extract sha8 from filename %q — cannot look up cache templates",
			decoded.Filename,
		)
	}

	refs, err := r.TemplatesBySHA8(ctx, result.SHA8)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		hit := StemcellTemplateHit{VMID: int(ref.VMID), Node: ref.Node, Name: ref.Name}
		if cfg, cfgErr := r.VMConfig(ctx, ref.Node, int(ref.VMID)); cfgErr == nil && cfg != nil {
			desc := pve.DescriptionFromConfig(cfg)
			if prov, ok := parseTemplateProvenance(desc); ok {
				hit.HasProvenance = true
				hit.DirectorRefs = prov.DirectorRefs
			}
		}
		result.Templates = append(result.Templates, hit)
	}

	// The storage-content check needs a node to issue the request against;
	// prefer a template's own node (guaranteed to see the storage the CID
	// names, since the template was built there), falling back to any
	// cluster node when no cache template exists yet.
	node := ""
	if len(refs) > 0 {
		node = refs[0].Node
	} else if nodes, nerr := r.ListNodes(ctx); nerr == nil && len(nodes) > 0 {
		node = nodes[0]
	}
	if node != "" {
		if volid, verr := r.FindStemcellVolume(ctx, node, result.Storage, result.Filename); verr == nil && volid != "" {
			result.VolumeExists = true
		}
	}

	return result, nil
}

// resolveBareVolid accepts either a pvd-/pvz- envelope CID or a raw PVE
// volid ("<storage>:<volume>") and returns the bare volid to scan for.
// "pve-cid locate" is the one subcommand that accepts a raw volid directly
// (an operator convenience "decode" deliberately does not extend to, since
// bare volids are never a Director-visible CID — see DecodeCID's doc
// comment).
func resolveBareVolid(raw string) (bareVolid, stableID string, err error) {
	if strings.HasPrefix(raw, "pvd-") || strings.HasPrefix(raw, "pvz-") {
		bareCID, meta, decErr := pve.ParseEncodedDiskCID(raw)
		if decErr != nil {
			return "", "", fmt.Errorf("pve-cid: %w", decErr)
		}
		if meta != nil {
			stableID = meta.ID
		}
		return bareCID, stableID, nil
	}
	if _, _, parseErr := pve.ParseDiskCID(raw); parseErr != nil {
		return "", "", fmt.Errorf(
			"pve-cid: locate: %q is neither a stemcell CID (\":light:\"/\":heavy:\"), "+
				"a disk CID (\"pvd-\"/\"pvz-\"), nor a raw volid (\"<storage>:<volume>\"): %w",
			raw, parseErr,
		)
	}
	return raw, "", nil
}

// runLocate implements "pve-cid locate <cid|volid> [--config PATH] [--json]".
func runLocate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pve-cid locate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to CPI JSON config file (default: $PVE_CPI_CONFIG or "+defaultConfigPath+")")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of a table")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: pve-cid locate <cid|volid> [--config PATH] [--json]")
		fs.PrintDefaults()
	}
	positionals, err := parseWithPositionals(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintf(stderr, "pve-cid locate: expected exactly one CID or volid argument, got %d\n", len(positionals))
		fs.Usage()
		return exitUsage
	}
	raw := positionals[0]

	_, reader, err := loadConfigAndReader(*configPath, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitError
	}

	ctx := context.Background()

	if strings.HasPrefix(raw, ":") {
		decoded, decErr := DecodeCID(raw)
		if decErr != nil {
			_, _ = fmt.Fprintln(stderr, decErr)
			return exitError
		}
		result, locErr := locateStemcell(ctx, reader, decoded)
		if locErr != nil {
			_, _ = fmt.Fprintln(stderr, locErr)
			return exitError
		}
		if *jsonOut {
			return writeJSON(stdout, stderr, result)
		}
		printStemcellLocateResult(stdout, result)
		return exitOK
	}

	bareVolid, stableID, resErr := resolveBareVolid(raw)
	if resErr != nil {
		_, _ = fmt.Fprintln(stderr, resErr)
		return exitUsage
	}
	result, locErr := locateDisk(ctx, reader, bareVolid, stableID)
	if locErr != nil {
		_, _ = fmt.Fprintln(stderr, locErr)
		return exitError
	}
	if *jsonOut {
		return writeJSON(stdout, stderr, result)
	}
	printDiskLocateResult(stdout, result)
	return exitOK
}

func printDiskLocateResult(w io.Writer, result *DiskLocateResult) {
	_, _ = fmt.Fprintf(w, "volid: %s\n", result.BareVolid)
	if result.StableID != "" {
		_, _ = fmt.Fprintf(w, "stable_id: %s\n", result.StableID)
	}
	if result.CurrentVolid != "" && result.CurrentVolid != result.BareVolid {
		_, _ = fmt.Fprintf(w, "current_volid: %s (renamed by reassignment; envelope volid is the birth record)\n", result.CurrentVolid)
	}
	if result.Holder != nil {
		_, _ = fmt.Fprintf(w, "holder: vmid=%d node=%s slot=%s\n", result.Holder.VMID, result.Holder.Node, result.Holder.Slot)
	} else {
		_, _ = fmt.Fprintln(w, "holder: unattached (no active bus slot found on any cluster VM)")
	}

	if len(result.SentinelMatches) == 0 {
		_, _ = fmt.Fprintln(w, "sentinels: none")
	} else {
		for _, m := range result.SentinelMatches {
			if m.AttachedCID != "" {
				_, _ = fmt.Fprintf(w, "sentinel: bosh_attached_disks on vmid=%d node=%s cid=%s\n", m.VMID, m.Node, m.AttachedCID)
			}
			if m.Parked != nil {
				_, _ = fmt.Fprintf(w, "sentinel: bosh_parked_disks on vmid=%d node=%s parked_at=%s source_vm=%s\n",
					m.VMID, m.Node, m.Parked.ParkedAt, m.Parked.SourceVMCID)
			}
		}
		if result.Holder == nil {
			_, _ = fmt.Fprintln(w, "warning: sentinel entry found with no matching bus-slot holder anywhere in the cluster — possibly stale")
		}
	}
}

func printStemcellLocateResult(w io.Writer, result *StemcellLocateResult) {
	_, _ = fmt.Fprintf(w, "cid: %s\n", result.CID)
	_, _ = fmt.Fprintf(w, "kind: %s\n", result.Kind)
	_, _ = fmt.Fprintf(w, "storage: %s\n", result.Storage)
	_, _ = fmt.Fprintf(w, "filename: %s\n", result.Filename)
	_, _ = fmt.Fprintf(w, "sha8: %s\n", result.SHA8)
	_, _ = fmt.Fprintf(w, "volume_exists: %t\n", result.VolumeExists)
	if len(result.Templates) == 0 {
		_, _ = fmt.Fprintln(w, "cache templates: none found")
		return
	}
	for _, t := range result.Templates {
		_, _ = fmt.Fprintf(w, "cache template: vmid=%d node=%s name=%s\n", t.VMID, t.Node, t.Name)
		if !t.HasProvenance {
			_, _ = fmt.Fprintln(w, "  provenance: none (no parseable description JSON)")
			continue
		}
		if len(t.DirectorRefs) == 0 {
			_, _ = fmt.Fprintln(w, "  director_refs: (empty — no director currently holds a live reference)")
		} else {
			_, _ = fmt.Fprintf(w, "  director_refs: %s\n", strings.Join(t.DirectorRefs, ", "))
		}
	}
}
