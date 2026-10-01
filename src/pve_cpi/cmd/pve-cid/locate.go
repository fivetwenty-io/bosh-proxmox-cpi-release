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

// DiskHolderMatch is one active bus slot that matches the disk, either because
// it names the located volid or because its drive carries the disk's serial.
type DiskHolderMatch struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Slot string `json:"slot"`
	// Volid is the volid this slot actually carries.
	Volid string `json:"volid"`
	// Match is "volid" or "serial", saying which test the slot passed.
	Match string `json:"match"`
}

// UnusedReference is one unusedN entry that names the disk's volume, under its
// located volid or under a name a serial-matched slot carries.
type UnusedReference struct {
	VMID  int    `json:"vmid"`
	Node  string `json:"node"`
	Slot  string `json:"slot"`
	Volid string `json:"volid"`
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
	CurrentVolid string `json:"current_volid,omitempty"`
	// Holder is the first matching active slot in VMID order, kept for
	// callers that read a single holder. Holders lists every match.
	Holder          *DiskHolder     `json:"holder,omitempty"`
	SentinelMatches []SentinelMatch `json:"sentinel_matches,omitempty"`
	// Holders lists every active slot on every guest that matches the disk,
	// in VMID order. More than one guest here means the volume is named
	// twice, and the Director's VM CID decides which guest really holds it.
	Holders []DiskHolderMatch `json:"holders,omitempty"`
	// UnusedRefs lists every unusedN entry that names the disk's volume.
	UnusedRefs []UnusedReference `json:"unused_refs,omitempty"`
	// UnreadableVMIDs lists the guests whose config could not be read, so a
	// match there would be missing from this result.
	UnreadableVMIDs []int `json:"unreadable_vmids,omitempty"`
}

// locateDisk scans every cluster VM for the disk on an active bus slot
// (scsi/virtio/ide/sata, the exact set qemu.ParseDisks recognizes and the same
// helper internal/pve's own FindVMByDiskVolid uses), by its volid or, when
// stableID is set, by a serial=<stableID> drive option. It records every
// match on every guest rather than stopping at the first, because a volume
// that two guests name is exactly what an operator runs this to find, and it
// then records every unusedN entry that names the volume under any of the
// names those slots carry. Independently, it records a bosh_attached_disks or
// bosh_parked_disks sentinel entry naming the disk on any VM's description.
//
// Holder and CurrentVolid keep their meaning: the first match in VMID order,
// with a volid match ahead of a serial match on the same guest.
//
// A VM whose config cannot be fetched is listed in UnreadableVMIDs rather
// than aborting the whole locate.
//
// Returns an error only when the initial cluster VM listing itself fails.
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

	type readVM struct {
		vm  ClusterVM
		cfg map[string]any
	}
	var read []readVM
	for _, vm := range vms {
		cfg, cfgErr := r.VMConfig(ctx, vm.Node, vm.VMID)
		if cfgErr != nil || cfg == nil {
			result.UnreadableVMIDs = append(result.UnreadableVMIDs, vm.VMID)
			continue
		}
		read = append(read, readVM{vm: vm, cfg: cfg})

		result.Holders = append(result.Holders, matchingSlots(vm, qemu.ParseDisks(cfg), bareVolid, stableID)...)

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

	if len(result.Holders) > 0 {
		first := result.Holders[0]
		result.Holder = &DiskHolder{VMID: first.VMID, Node: first.Node, Slot: first.Slot}
		result.CurrentVolid = first.Volid
	}

	// An unused entry carries no serial, so it is matched by name: the
	// located volid, and every name a matching active slot carries, which is
	// how a volume renamed for one guest shows up on another guest's unused
	// entry.
	names := map[string]bool{bareVolid: true}
	for _, h := range result.Holders {
		names[h.Volid] = true
	}
	for _, rv := range read {
		unused := pve.FindUnusedDiskEntries(rv.cfg)
		slots := make([]string, 0, len(unused))
		for slot := range unused {
			slots = append(slots, slot)
		}
		sort.Strings(slots)
		for _, slot := range slots {
			if names[unused[slot]] {
				result.UnusedRefs = append(result.UnusedRefs, UnusedReference{VMID: rv.vm.VMID, Node: rv.vm.Node, Slot: slot, Volid: unused[slot]})
			}
		}
	}

	return result, nil
}

// matchingSlots returns every active slot of one guest that matches the disk,
// volid matches first and serial matches after, each in slot order.
func matchingSlots(vm ClusterVM, disks map[string]string, bareVolid, stableID string) []DiskHolderMatch {
	slots := make([]string, 0, len(disks))
	for slot := range disks {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	var byVolid, bySerial []DiskHolderMatch
	for _, slot := range slots {
		optStr := disks[slot]
		if optStr == bareVolid || strings.HasPrefix(optStr, bareVolid+",") {
			byVolid = append(byVolid, DiskHolderMatch{VMID: vm.VMID, Node: vm.Node, Slot: slot, Volid: bareVolid, Match: "volid"})
			continue
		}
		// Mirror the CPI's identity resolution: a serial=<stableID> drive
		// option identifies the disk after a reassignment renamed the volume
		// away from its birth volid.
		if stableID != "" {
			if serial, has := pve.StableIDFromDriveOptStr(optStr); has && serial == stableID {
				bySerial = append(bySerial, DiskHolderMatch{VMID: vm.VMID, Node: vm.Node, Slot: slot, Volid: bareVolidFromDriveOptStr(optStr), Match: "serial"})
			}
		}
	}
	return append(byVolid, bySerial...)
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
	printDiskLocateWarnings(stderr, result)
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
	for _, h := range result.Holders {
		if result.Holder != nil && h.VMID == result.Holder.VMID && h.Slot == result.Holder.Slot {
			continue
		}
		_, _ = fmt.Fprintf(w, "also held by: vmid=%d node=%s slot=%s volid=%s (matched by %s)\n", h.VMID, h.Node, h.Slot, h.Volid, h.Match)
	}
	for _, u := range result.UnusedRefs {
		_, _ = fmt.Fprintf(w, "unused: vmid=%d node=%s slot=%s volid=%s\n", u.VMID, u.Node, u.Slot, u.Volid)
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

// locateDocsPointer names the runbook section on volumes that more than one
// guest names. The docs do not ship in the release, so it names the repository
// as well as the file.
const locateDocsPointer = `see "Auditing parked disks with scripts/disk-audit" in docs/operations.md of bosh-proxmox-cpi-release`

// printDiskLocateWarnings writes to stderr when more than one guest names the
// disk, counting active slots and unused entries, and when a guest's config
// could not be read. Neither changes the exit code.
func printDiskLocateWarnings(w io.Writer, result *DiskLocateResult) {
	guests := map[int]bool{}
	for _, h := range result.Holders {
		guests[h.VMID] = true
	}
	for _, u := range result.UnusedRefs {
		guests[u.VMID] = true
	}
	if len(guests) > 1 {
		msg := fmt.Sprintf("warning: %d guests name this disk (active slots: %d, unused entries: %d).",
			len(guests), len(result.Holders), len(result.UnusedRefs))
		if len(result.Holders) > 0 {
			msg += " The holder shows only the first active slot in VMID order."
		}
		msg += " The Director's VM CID decides which guest really holds the disk." +
			" Leave every reference in place until that is settled; " + locateDocsPointer
		_, _ = fmt.Fprintln(w, msg)
	}
	if len(result.UnreadableVMIDs) > 0 {
		ids := make([]string, 0, len(result.UnreadableVMIDs))
		for _, id := range result.UnreadableVMIDs {
			ids = append(ids, fmt.Sprint(id))
		}
		_, _ = fmt.Fprintf(w, "warning: the configs of %d VM(s) could not be read (%s), so this result may be incomplete\n",
			len(result.UnreadableVMIDs), strings.Join(ids, ", "))
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
