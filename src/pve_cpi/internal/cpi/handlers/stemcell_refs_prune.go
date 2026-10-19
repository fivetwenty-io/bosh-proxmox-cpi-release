package handlers

// stemcell_refs_prune.go: the operator side of the director-ref set (D3).
//
// A rebuilt director gets a new UUID, and its old UUID stays in director_refs
// on every stemcell cache template it registered. No CPI call ever removes
// it, so those templates can never be freed. `cpi stemcell-refs` reports the
// refs on every cache template and prunes one named ref, frees one
// unreferenced template, or drops the director tags no ref backs, on an
// operator's say-so.
//
// The work splits into four exported pieces so cmd/cpi stays thin:
//
//   - ScanStemcellRefs reads the cluster once (strict enumeration plus one
//     config read per guest).
//   - PlanStemcellRefsPrune turns a scan and a request into a plan. It is the
//     only planner: the dry run prints its result, the apply executes that
//     same value, and the report's "would free" column comes from it too.
//   - ApplyStemcellRefsPrune executes a plan. Every mutation takes the
//     per-VMID lock, re-reads the template inside it, and skips the template
//     when it is no longer a template, or its name, description, or tags no
//     longer match the scan.
//   - BuildStemcellRefsReport summarizes a scan for the read-only report.
//
// Ref removal goes through deregisterStemcellDirectorRefWith, the same
// locked read-modify-write delete_stemcell uses, so the last-ref ordering
// (stamp bosh-destroy-pending, release, destroy) is shared rather than
// copied.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// Action kinds a StemcellRefsPlan can hold. The first seven mutate PVE on
// apply; blocked and skip never do.
const (
	// StemcellRefsActionRemoveRef removes the ref from a template that keeps
	// other refs, and removes its director tag in the same write when that
	// is safe.
	StemcellRefsActionRemoveRef = "remove-ref"
	// StemcellRefsActionRemoveRefAndDestroy removes the template's last ref
	// and destroys it the way delete_stemcell does.
	StemcellRefsActionRemoveRefAndDestroy = "remove-ref-and-destroy"
	// StemcellRefsActionResumeDestroy finishes the destroy of a template an
	// earlier last-ref call already marked bosh-destroy-pending.
	StemcellRefsActionResumeDestroy = "resume-destroy"
	// StemcellRefsActionRemoveTag removes a director tag from a template
	// whose description does not hold the ref. It never destroys.
	StemcellRefsActionRemoveTag = "remove-tag"
	// StemcellRefsActionRemoveTags removes every director tag that no
	// description ref maps to (--reconcile-tags). It never changes the
	// description and never destroys.
	StemcellRefsActionRemoveTags = "remove-tags"
	// StemcellRefsActionDestroy destroys one unreferenced template (--vmid).
	StemcellRefsActionDestroy = "destroy"
	// StemcellRefsActionDestroyUntracked destroys one untracked template that
	// holds no ref and backs no linked clone (--vmid with --untracked).
	StemcellRefsActionDestroyUntracked = "destroy-untracked"
	// StemcellRefsActionBlocked is a template the request names that the
	// plan will not change because of a safety rule, such as linked clones.
	StemcellRefsActionBlocked = "blocked"
	// StemcellRefsActionSkip is a template the plan leaves alone, with the
	// reason; Failure says whether that counts against the exit code.
	StemcellRefsActionSkip = "skip"
)

// Description states recorded on a scanned template.
const (
	stemcellRefsDescProvenance  = "provenance"
	stemcellRefsDescBlank       = "blank"
	stemcellRefsDescUnparseable = "unparseable"
	stemcellRefsDescUnread      = "unread"
)

// stemcellRefsCIDLabel stands in for the stemcell CID in destroyTemplateVM's
// log fields, because the operator prune works from VMIDs, not CIDs.
const stemcellRefsCIDLabel = "stemcell-refs-prune"

// PVE config keys the prune reads besides the description and tags.
const (
	pveConfigKeyName     = "name"
	pveConfigKeyTemplate = "template"
)

// ErrStemcellRefsChanged marks a template whose description or tags changed
// between the plan and the locked re-read at apply time. The template is left
// as it is and the action is reported as skipped.
var ErrStemcellRefsChanged = errors.New("stemcell-refs: template changed since the plan was made")

// ErrStemcellRefsPruneRefused is returned by ApplyStemcellRefsPrune for a
// plan whose Refused flag is set; nothing is changed.
var ErrStemcellRefsPruneRefused = errors.New("stemcell-refs: prune refused")

// directorTagPrefix starts every director reference tag.
const directorTagPrefix = "director--"

// legacyStemcellTemplateName matches the cache template naming pattern
// (bosh-stemcell-<name>-<version>, with or without the -<sha8> suffix). It is
// how --vmid recognizes a template built before 0.9.2, which has a blank
// description and no tags.
var legacyStemcellTemplateName = regexp.MustCompile(`^bosh-stemcell-[A-Za-z0-9][A-Za-z0-9.-]*$`)

// linkedCloneBaseVolume finds a base volume of another guest inside a disk
// volid. A linked clone's disk names its base as a parent path segment: file
// storage writes "stor:<T>/base-<T>-disk-0.qcow2/<C>/vm-<C>-disk-0.qcow2",
// and lvmthin, zfs, and rbd write "stor:base-<T>-disk-0/vm-<C>-disk-0". The
// template's own disk has no "/" after the base segment, so it never matches.
var linkedCloneBaseVolume = regexp.MustCompile(`(?:^|[:/])base-([1-9]\d*)-disk-[^/,]*/`)

// linkedCloneDiskKey matches the config keys that can hold a disk volid.
var linkedCloneDiskKey = regexp.MustCompile(`^(?:virtio|scsi|sata|ide|efidisk|tpmstate|unused)\d+$`)

// StemcellRefsTemplate is one stemcell cache template as the scan saw it.
type StemcellRefsTemplate struct {
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// SHA8 comes from the provenance, or from the bosh-stemcell-sha- tag
	// when the provenance has none.
	SHA8 string   `json:"sha8,omitempty"`
	Tags []string `json:"tags"`
	// DescriptionRefs is the description's director_refs set, the only
	// source of truth for whether the template stays.
	DescriptionRefs []string `json:"description_refs"`
	// DirectorTags is every director-- tag. delete_stemcell drops a departing
	// director's tag while other refs remain, so the tags name the directors
	// that hold the template, but a template from an earlier release can
	// still carry stale ones, which prune --reconcile-tags removes.
	DirectorTags []string `json:"director_tags"`
	// RefsTagsDisagree is true when the sanitized description refs and the
	// director tag values are not the same set.
	RefsTagsDisagree bool `json:"refs_tags_disagree"`
	// DescriptionState is provenance, blank, unparseable, or unread.
	DescriptionState string `json:"description_state"`
	// DescriptionBytes is the description's length in bytes, which PVE caps
	// at 8192. It is 0 when the config could not be read.
	DescriptionBytes int `json:"description_bytes"`
	// DescriptionNearLimit is true when the description is within 1024 bytes
	// of that cap, so registering more refs may soon fail.
	DescriptionNearLimit bool `json:"description_near_limit"`
	// Replica is true for per-node and per-storage replicas, whose refs are
	// a fossil of their creation.
	Replica        bool `json:"replica"`
	DestroyPending bool `json:"destroy_pending"`
	// Legacy marks a template from before 0.9.2: the cache naming pattern, a
	// blank description, and no generation marker, meaning no director-- tag
	// and no bosh-stemcell-cache tag. It may carry sha8 or replica tags. Only
	// a legacy template's blank description counts as zero refs.
	Legacy bool `json:"legacy"`
	// RefsUnknown is true when the description cannot tell us the ref set:
	// it was unread, it does not parse, or it is blank on a template that is
	// not legacy. Such a template is never changed.
	RefsUnknown bool `json:"refs_unknown"`
	// Untracked marks a provenance template with no generation marker. It
	// comes from a CPI generation that never recorded refs, so an empty ref
	// set on it proves nothing about who uses it. Only an explicit --vmid
	// prune with --untracked destroys it.
	Untracked bool `json:"untracked"`
	// LinkedClones lists the guests whose disks name this template's base
	// volume as their parent.
	LinkedClones []int64 `json:"linked_clones"`
	// Running is true when the guest listing reports the template as
	// running, which --untracked refuses.
	Running   bool   `json:"running"`
	ReadError string `json:"read_error,omitempty"`

	// description is the raw description the scan read, kept so the apply
	// can prove the template has not changed since the plan.
	description string
	// cfgName is the name field of the config the scan read, "" when the
	// config had none, so the apply can prove the name has not changed.
	cfgName string
	// volumes lists the disk volumes in the template's config that belong
	// to the template itself, which its destroy frees.
	volumes []StemcellRefsVolume
}

// StemcellRefsVolume is one disk volume a destroy would free.
type StemcellRefsVolume struct {
	// Slot is the config key that names the volume, such as scsi0.
	Slot string `json:"slot"`
	// Volume is the full volid, <storage>:<name>.
	Volume  string `json:"volume"`
	Storage string `json:"storage"`
	// SizeBytes is the size PVE reports for the volume, or null when it
	// could not be read.
	SizeBytes *int64 `json:"size_bytes"`
	// SizeError says why the size could not be read.
	SizeError string `json:"size_error,omitempty"`
}

// StemcellRefsFrees is what one destroy would free: each disk volume with its
// storage and size, and the total of the sizes that could be read.
type StemcellRefsFrees struct {
	Volumes    []StemcellRefsVolume `json:"volumes"`
	TotalBytes int64                `json:"total_bytes"`
	// SizesUnknown counts the volumes whose size could not be read, so
	// TotalBytes is a lower bound while it is not zero.
	SizesUnknown int `json:"sizes_unknown"`
}

// StemcellRefsGuest is one non-template guest, its kind, and the tags the
// owned-VM guard reads.
type StemcellRefsGuest struct {
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// Kind is parker, bosh, or other (the StemcellRefsGuest* constants).
	Kind string `json:"kind,omitempty"`
	// OwnerTags holds the guest's bosh-owner-- tags, lowercased.
	OwnerTags []string `json:"owner_tags,omitempty"`
	// DirectorTags and DeploymentTags hold the guest's director-- and
	// deployment-- tags as stored.
	DirectorTags   []string `json:"director_tags,omitempty"`
	DeploymentTags []string `json:"deployment_tags,omitempty"`
	// Instance is the BOSH instance the guest records, as <instance
	// group>/<index>, from its instance-group-- (or job--) and index-- tags,
	// or from its notes when it carries no such tags. It is the instance
	// group alone when the index is unknown, and "" when the group is.
	Instance string `json:"instance,omitempty"`
	// NotesMetadata is true when the guest's notes hold the Director
	// metadata lines the ownership guard reads (hasDirectorMetadataLines).
	NotesMetadata bool `json:"notes_metadata,omitempty"`
	// ReadError is the config read failure for a guest the scan could only
	// see in the listing, "" when the read succeeded.
	ReadError string `json:"read_error,omitempty"`
	// Uncovered is true for a BOSH guest that carries no owner tag, and for
	// any guest whose config could not be read. Either makes ownership
	// unknown for every ref.
	Uncovered bool `json:"uncovered,omitempty"`

	// tags is every tag token the scan read, from the guest's config when it
	// could be read and from the listing otherwise.
	tags []string
	// notesDirector and notesDeployment are the values of the Director
	// metadata lines in the guest's notes, for the report's advice.
	notesDirector, notesDeployment string
}

// StemcellRefsScan is one read of the cluster.
type StemcellRefsScan struct {
	Templates []StemcellRefsTemplate `json:"templates"`
	Guests    []StemcellRefsGuest    `json:"guests"`
	// CloneScanComplete is false when some guest's config could not be read,
	// so the linked-clone lists cannot prove absence. Every destroy is then
	// blocked.
	CloneScanComplete bool     `json:"clone_scan_complete"`
	ScanErrors        []string `json:"scan_errors,omitempty"`
	// Uncovered groups the BOSH guests that carry no owner tag. While it is
	// not empty, ownership is unknown for every ref and every director prune
	// is refused.
	Uncovered []StemcellRefsUncoveredGroup `json:"uncovered,omitempty"`

	// otherTemplates holds templates that are not recognizably stemcell
	// cache templates (base images, for example), so --vmid can say why it
	// refuses one.
	otherTemplates map[int64]string
}

// ScanStemcellRefs reads every guest in the cluster through the strict
// enumeration (pve.ListGuestsAuthoritative), because the scan feeds absence
// proofs: no linked clone, no owned VM. It then reads each guest's config
// once, for the template descriptions and the disk volids. An enumeration
// failure is returned as an error. A config read that fails on one guest is
// recorded in ScanErrors and clears CloneScanComplete, and a template whose
// own config could not be read is kept with ReadError set. Every non-template
// guest is sorted into a kind (parker, BOSH guest, or other) by the same
// evidence the ownership guard accepts, tags or the Director lines in its
// notes. The BOSH guests that carry no owner tag, and every non-template
// guest whose config could not be read, are grouped in Uncovered.
func ScanStemcellRefs(ctx context.Context, deps Deps) (StemcellRefsScan, error) {
	scan := StemcellRefsScan{CloneScanComplete: true, otherTemplates: map[int64]string{}}
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return scan, cpierrors.Cloud("stemcell-refs: PVE client unavailable")
	}
	logger := deps.Log(ctx)
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, logger)
	if err != nil {
		return scan, cpierrors.Wrap(err, "stemcell-refs: enumerate cluster guests")
	}
	sort.Slice(guests, func(i, j int) bool { return guests[i].VMID < guests[j].VMID })

	clones := map[int64][]int64{}
	for _, g := range guests {
		if g.VMID <= 0 {
			continue
		}
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, g.Node, g.VMID)
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
				// Destroyed between the listing and the read.
				continue
			}
			scan.CloneScanComplete = false
			scan.ScanErrors = append(scan.ScanErrors,
				fmt.Sprintf("vmid %d on node %s: config read failed: %v", g.VMID, g.Node, cfgErr))
			cfg = nil
		}
		for _, base := range linkedCloneBases(cfg, int64(g.VMID)) {
			clones[base] = append(clones[base], int64(g.VMID))
		}
		if !g.Template {
			// The config is the guest's own record; the listing's tags stand
			// in only when the config could not be read.
			// A guest whose config could not be read is uncovered whatever
			// its listing says, because neither its current tags nor its
			// notes are known.
			tags := g.SplitTags()
			desc, readErr := "", ""
			if cfg != nil {
				tags = parseTagsField(stringConfigField(cfg, jsonKeyTags))
				desc = stringConfigField(cfg, "description")
			} else {
				readErr = cfgErr.Error()
			}
			scan.Guests = append(scan.Guests, newStemcellRefsGuest(int64(g.VMID), g.Node, g.Name, tags, desc, readErr))
			continue
		}
		t, recognized := scanStemcellTemplate(g, cfg, cfgErr)
		if !recognized {
			scan.otherTemplates[int64(g.VMID)] = g.Name
			continue
		}
		scan.Templates = append(scan.Templates, t)
	}
	for i := range scan.Templates {
		list := clones[scan.Templates[i].VMID]
		sort.Slice(list, func(a, b int) bool { return list[a] < list[b] })
		scan.Templates[i].LinkedClones = list
	}
	scan.Uncovered = groupUncoveredGuests(scan.Guests)
	return scan, nil
}

// scanStemcellTemplate builds the scan record for one template and reports
// whether it is recognizably a stemcell cache template.
func scanStemcellTemplate(g pve.GuestRef, cfg map[string]any, cfgErr error) (StemcellRefsTemplate, bool) {
	t := StemcellRefsTemplate{VMID: int64(g.VMID), Node: g.Node, Name: g.Name, Running: g.Status == vmPowerStateRunning}
	var prov stemcellProvenance
	provOK := false
	if cfgErr != nil && cfg == nil {
		t.Tags = g.SplitTags()
		t.DescriptionState = stemcellRefsDescUnread
		t.ReadError = cfgErr.Error()
	} else {
		t.Tags = parseTagsField(stringConfigField(cfg, jsonKeyTags))
		t.description = stringConfigField(cfg, pveConfigKeyDescription)
		t.cfgName = stringConfigField(cfg, pveConfigKeyName)
		t.volumes = ownedTemplateVolumes(cfg, t.VMID)
		t.DescriptionBytes = len(t.description)
		t.DescriptionNearLimit = stemcellDescriptionNearLimit(t.DescriptionBytes)
		if t.cfgName != "" {
			t.Name = t.cfgName
		}
		switch {
		case strings.TrimSpace(t.description) == "":
			t.DescriptionState = stemcellRefsDescBlank
		default:
			prov, provOK = parseStemcellProvenanceFromDescription(t.description)
			if provOK {
				t.DescriptionState = stemcellRefsDescProvenance
				t.DescriptionRefs = append([]string(nil), prov.DirectorRefs...)
			} else {
				t.DescriptionState = stemcellRefsDescUnparseable
			}
		}
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	if t.DescriptionRefs == nil {
		t.DescriptionRefs = []string{}
	}
	t.DirectorTags = []string{}
	for _, tok := range t.Tags {
		if strings.HasPrefix(tok, directorTagPrefix) {
			t.DirectorTags = append(t.DirectorTags, tok)
		}
		if strings.HasPrefix(tok, stemcellSHATagPrefix) && t.SHA8 == "" {
			t.SHA8 = strings.TrimPrefix(tok, stemcellSHATagPrefix)
		}
	}
	if provOK && prov.SHA8 != "" {
		t.SHA8 = prov.SHA8
	}
	t.Replica = pve.TemplateRef{Tags: strings.Join(t.Tags, pveTagJoinSep)}.IsReplica()
	t.DestroyPending = stringSliceContains(t.Tags, stemcellDestroyPendingTag)
	nameMatches := legacyStemcellTemplateName.MatchString(t.Name)
	marker := pve.HasStemcellGenerationMarker(t.Tags)
	t.Legacy = nameMatches && t.DescriptionState == stemcellRefsDescBlank && !marker
	t.RefsUnknown = t.DescriptionState == stemcellRefsDescUnread || t.DescriptionState == stemcellRefsDescUnparseable ||
		(t.DescriptionState == stemcellRefsDescBlank && !t.Legacy)
	t.Untracked = t.DescriptionState == stemcellRefsDescProvenance && !marker
	if t.DescriptionState == stemcellRefsDescProvenance || t.DescriptionState == stemcellRefsDescBlank {
		t.RefsTagsDisagree = refsAndTagsDisagree(t.DescriptionRefs, t.DirectorTags)
	}
	t.LinkedClones = []int64{}

	recognized := nameMatches || marker || t.Replica ||
		(provOK && (prov.SHA8 != "" || prov.CID != "" || prov.Kind != "" || len(prov.DirectorRefs) > 0))
	for _, tok := range t.Tags {
		if tok == stemcellMarkerTag || tok == stemcellCacheTag || strings.HasPrefix(tok, stemcellSHATagPrefix) {
			recognized = true
		}
	}
	return t, recognized
}

// ownedTemplateVolume matches the last path segment of a volume name that
// belongs to a guest, base-<vmid>-... or vm-<vmid>-..., and captures the VMID.
var ownedTemplateVolume = regexp.MustCompile(`^(?:base|vm)-([1-9]\d*)-`)

// ownedTemplateVolumes returns the disk volumes in cfg that belong to guest
// vmid, sorted by config key. A volume belongs to the guest when the last
// segment of its name starts with base-<vmid>- or vm-<vmid>-, which is what a
// destroy with purge frees; an ISO, a parent base volume of another guest,
// and an empty drive are left out.
func ownedTemplateVolumes(cfg map[string]any, vmid int64) []StemcellRefsVolume {
	var out []StemcellRefsVolume
	for key := range cfg {
		if !linkedCloneDiskKey.MatchString(key) {
			continue
		}
		volid := stringConfigField(cfg, key)
		if i := strings.IndexByte(volid, ','); i >= 0 {
			volid = volid[:i]
		}
		storage, name, err := pve.ParseDiskCID(volid)
		if err != nil {
			continue
		}
		last := name
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			last = name[i+1:]
		}
		m := ownedTemplateVolume.FindStringSubmatch(last)
		if len(m) < 2 || m[1] != strconv.FormatInt(vmid, 10) {
			continue
		}
		out = append(out, StemcellRefsVolume{Slot: key, Volume: volid, Storage: storage})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// refsKnownZero reports whether t provably holds no ref: a provenance
// description with an empty ref set, or a legacy template's blank one.
func refsKnownZero(t *StemcellRefsTemplate) bool {
	if t.Legacy {
		return true
	}
	return t.DescriptionState == stemcellRefsDescProvenance && len(t.DescriptionRefs) == 0
}

// storedRefSpellings returns the distinct entries of refs that equal ref once
// surrounding space is trimmed, in the order they appear.
func storedRefSpellings(refs []string, ref string) []string {
	var out []string
	for _, r := range refs {
		if strings.TrimSpace(r) == ref && !stringSliceContains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// refsAndTagsDisagree compares the sanitized description refs with the
// director tag values as sets, without regard to case.
func refsAndTagsDisagree(refs, directorTags []string) bool {
	want := map[string]bool{}
	for _, r := range refs {
		if v := strings.ToLower(sanitizeTagValue(r)); v != "" {
			want[v] = true
		}
	}
	have := map[string]bool{}
	for _, tok := range directorTags {
		if v := strings.ToLower(strings.TrimPrefix(tok, directorTagPrefix)); v != "" {
			have[v] = true
		}
	}
	if len(want) != len(have) {
		return true
	}
	for v := range want {
		if !have[v] {
			return true
		}
	}
	return false
}

// ownerTagsOf returns the distinct bosh-owner-- tags among tokens, sorted and
// as stored. It reads them the way ownVM does, through tagValues: the prefix
// must match exactly and the value must not be empty, so a hand-edited
// Bosh-Owner--x counts here only when it would count for the ownership guard,
// which is never.
func ownerTagsOf(tokens []string) []string {
	values := tagValues(tokens, ownerTagPrefix)
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, ownerTagPrefix+v)
	}
	return out
}

// linkedCloneBases returns the VMIDs of the guests whose base volumes this
// guest's disks name as their parent, excluding the guest itself.
func linkedCloneBases(cfg map[string]any, self int64) []int64 {
	if cfg == nil {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for key := range cfg {
		if !linkedCloneDiskKey.MatchString(key) {
			continue
		}
		volid := stringConfigField(cfg, key)
		if i := strings.IndexByte(volid, ','); i >= 0 {
			volid = volid[:i]
		}
		for _, m := range linkedCloneBaseVolume.FindAllStringSubmatch(volid, -1) {
			base, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || base == self || seen[base] {
				continue
			}
			seen[base] = true
			out = append(out, base)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// StemcellRefsPruneRequest names what a prune acts on. Exactly one of
// DirectorRef, a VMID (VMID or VMIDs), and ReconcileTags is set.
type StemcellRefsPruneRequest struct {
	// VMIDs names several templates to free, each judged on its own, in
	// place of VMID. A VMID named twice is judged once.
	VMIDs []int64
	// Untracked frees untracked templates by VMID. It needs a VMID, and it
	// relaxes only the untracked refusal: every template it names must be an
	// untracked stemcell cache template by name and provenance, hold no ref,
	// back no linked clone, and not be running. A template an earlier
	// --untracked apply marked bosh-destroy-pending is destroyed again, which
	// resumes that destroy.
	Untracked bool
	// ConfigPath is the --config path the command ran with. It appears in
	// the rerun command an apply error names; "" prints a placeholder.
	ConfigPath string
	// ReconcileTags plans the removal of every director tag that no
	// description ref maps to, on every cache template the scan can read.
	// It never removes a ref and never destroys anything.
	ReconcileTags bool
	// DirectorRef is trimmed, then matched against description refs by
	// trimmed equality, and through directorTagFor against tags.
	DirectorRef string
	// VMID names one unreferenced template to free.
	VMID int64
	// AllowOwnedVMs lets a DirectorRef prune proceed while guests the ref
	// owns still exist, or when no owner tag can exist for the ref, as for
	// the unknown-director sentinel. It never overrides a scan in which some
	// BOSH guest carries no owner tag at all.
	AllowOwnedVMs bool
}

// StemcellRefsTemplateRef names a template inside a plan action.
type StemcellRefsTemplateRef struct {
	VMID int64    `json:"vmid"`
	Node string   `json:"node"`
	Name string   `json:"name"`
	Tags []string `json:"-"`

	// description and cfgName are the scanned description and config name,
	// so the sweep can prove the replica still holds no ref and has not been
	// renamed when it destroys it.
	description string
	cfgName     string
}

// StemcellRefsKeptReplica is a replica a destroy leaves in place, with the
// reason it is not swept.
type StemcellRefsKeptReplica struct {
	VMID   int64  `json:"vmid"`
	Node   string `json:"node"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	// FreeableByVMID is true when a --vmid prune of the replica would pass
	// once the plan's destroys are done.
	FreeableByVMID bool `json:"freeable_by_vmid"`
}

// StemcellRefsAction is one planned step on one template.
type StemcellRefsAction struct {
	Kind string `json:"kind"`
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	SHA8 string `json:"sha8,omitempty"`
	// Replica is true when the action's template is a per-node or
	// per-storage replica.
	Replica bool `json:"replica,omitempty"`
	// Reason explains a blocked or skipped action, or a tag the plan keeps.
	Reason string `json:"reason,omitempty"`
	// MatchedRef is the ref exactly as the description stores it, which can
	// differ from the requested ref by surrounding space.
	MatchedRef string `json:"matched_ref,omitempty"`
	// RemainingRefs is the ref set a remove-ref leaves behind.
	RemainingRefs []string `json:"remaining_refs,omitempty"`
	// DropTag is the director tag the action removes.
	DropTag string `json:"drop_tag,omitempty"`
	// DropTags lists the director tags a remove-tags action removes.
	DropTags []string `json:"drop_tags,omitempty"`
	// KeptTags lists the director tags a remove-tags action keeps, because
	// a description ref maps to each one.
	KeptTags []string `json:"kept_tags,omitempty"`
	// AddsCacheMarker is true when removing DropTag or DropTags would leave the kept
	// template with no generation marker, so the same write adds
	// bosh-stemcell-cache to keep it visible to delete_stemcell.
	AddsCacheMarker bool `json:"adds_cache_marker,omitempty"`
	// SweepReplicas lists the replicas destroyed after this template, when
	// it is the last non-replica template of its sha8. Each one passes the
	// --vmid preconditions on its own: zero refs, no linked clones.
	SweepReplicas []StemcellRefsTemplateRef `json:"sweep_replicas,omitempty"`
	// KeptReplicas lists the replicas of that sha8 the destroy leaves in
	// place, such as a replica that holds refs of its own.
	KeptReplicas []StemcellRefsKeptReplica `json:"kept_replicas,omitempty"`
	LinkedClones []int64                   `json:"linked_clones,omitempty"`
	// Frees lists what a destroy-untracked action would free, once
	// MeasureStemcellRefsPlan has read the sizes.
	Frees *StemcellRefsFrees `json:"frees,omitempty"`
	// ResumesPending is true when a destroy-untracked action's template
	// already carries bosh-destroy-pending from an earlier apply whose
	// destroy did not finish, so the apply resumes that destroy.
	ResumesPending bool `json:"resumes_pending,omitempty"`
	// Failure is true for a blocked or skipped action that makes the
	// command exit 1.
	Failure bool `json:"failure"`

	description string
	cfgName     string
	tags        []string
	// planned is set by the planner, so the apply refuses a mutating action
	// it did not produce, such as one decoded from JSON.
	planned bool
	// liveAnchorBlocked is set on a --vmid replica that passed every other
	// check and was blocked only because a non-replica template of its sha8
	// is live in the scan.
	liveAnchorBlocked bool
	// sweptWith is the template whose destroy sweeps this replica, set when
	// the plan destroys every non-replica template of its sha8.
	sweptWith int64
}

// Mutating reports whether applying the action changes PVE.
func (a StemcellRefsAction) Mutating() bool {
	switch a.Kind {
	case StemcellRefsActionRemoveRef, StemcellRefsActionRemoveRefAndDestroy, StemcellRefsActionResumeDestroy,
		StemcellRefsActionRemoveTag, StemcellRefsActionRemoveTags, StemcellRefsActionDestroy,
		StemcellRefsActionDestroyUntracked:
		return true
	}
	return false
}

// destroys reports whether applying the action destroys its template.
func (a StemcellRefsAction) destroys() bool {
	switch a.Kind {
	case StemcellRefsActionRemoveRefAndDestroy, StemcellRefsActionResumeDestroy, StemcellRefsActionDestroy,
		StemcellRefsActionDestroyUntracked:
		return true
	}
	return false
}

// StemcellRefsPlan is what a prune will do. The dry run prints it and the
// apply executes it unchanged.
type StemcellRefsPlan struct {
	// Mode is "director", "vmid", "untracked", or "reconcile-tags".
	Mode        string `json:"mode"`
	DirectorRef string `json:"director_ref,omitempty"`
	DirectorTag string `json:"director_tag,omitempty"`
	OwnerTag    string `json:"owner_tag,omitempty"`
	// VMID is the one VMID a --vmid prune names. A prune that names several
	// lists them in VMIDs and leaves VMID at zero.
	VMID  int64   `json:"vmid,omitempty"`
	VMIDs []int64 `json:"vmids,omitempty"`
	// TotalBytes adds up the volume sizes across every destroy-untracked
	// action, and SizesUnknown counts the volumes whose size could not be
	// read. MeasureStemcellRefsPlan fills both.
	TotalBytes   int64 `json:"total_bytes,omitempty"`
	SizesUnknown int   `json:"sizes_unknown,omitempty"`
	// OwnedVMs lists the BOSH guests carrying the ref's owner tag and the
	// parkers carrying its director tag.
	OwnedVMs []StemcellRefsGuest `json:"owned_vms,omitempty"`
	// OwnershipUnknown is true when OwnedVMs can't be trusted to be complete.
	// That happens when no owner tag can exist for the ref, as for the
	// unknown-director sentinel, because create_vm stamps no owner tag on a
	// VM made without a director UUID. It also happens when CoverageIncomplete
	// is set.
	OwnershipUnknown bool `json:"ownership_unknown"`
	// CoverageIncomplete is true when some BOSH guest carries no owner tag,
	// so it could belong to this ref's director. The plan is then refused
	// whatever AllowOwnedVMs says.
	CoverageIncomplete bool `json:"coverage_incomplete"`
	// OwnershipUnknownReason explains OwnershipUnknown.
	OwnershipUnknownReason string `json:"ownership_unknown_reason,omitempty"`
	// Uncovered is carried from the scan, so the dry run can name the guests
	// to adopt.
	Uncovered []StemcellRefsUncoveredGroup `json:"uncovered,omitempty"`
	// Refused is true when the plan must not be applied at all.
	Refused       bool                 `json:"refused"`
	RefusalReason string               `json:"refusal_reason,omitempty"`
	Actions       []StemcellRefsAction `json:"actions"`
	// CloneScanComplete and ScanErrors are carried from the scan.
	CloneScanComplete bool     `json:"clone_scan_complete"`
	ScanErrors        []string `json:"scan_errors,omitempty"`

	// configPath is the request's ConfigPath, for the rerun command an apply
	// error names.
	configPath string
}

// HasFailures reports whether the plan, applied as it stands, would end with
// exit 1: it is refused, or some action is a blocked or failing skip.
func (p StemcellRefsPlan) HasFailures() bool {
	if p.Refused {
		return true
	}
	for i := range p.Actions {
		if p.Actions[i].Failure {
			return true
		}
	}
	return false
}

// PlanStemcellRefsPrune is the single planner behind the dry run, the apply,
// and the report's "would free" column. It never touches PVE. It returns an
// error only for a malformed request.
func PlanStemcellRefsPrune(scan StemcellRefsScan, req StemcellRefsPruneRequest) (StemcellRefsPlan, error) {
	ref := strings.TrimSpace(req.DirectorRef)
	vmids, vmidErr := stemcellRefsRequestVMIDs(req)
	if vmidErr != nil {
		return StemcellRefsPlan{}, vmidErr
	}
	switch {
	case req.ReconcileTags && (ref != "" || len(vmids) > 0 || req.Untracked):
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: reconcile tags on its own, without a director ref or a VMID")
	case req.ReconcileTags && req.AllowOwnedVMs:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: allowing owned VMs applies only to a director ref")
	case req.ReconcileTags:
	case ref != "" && len(vmids) > 0:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: name a director ref or a VMID, not both")
	case req.Untracked && ref != "":
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: untracked applies only to a VMID, not to a director ref")
	case req.Untracked && len(vmids) == 0:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: untracked needs a positive VMID")
	case req.Untracked && req.AllowOwnedVMs:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: allowing owned VMs applies only to a director ref")
	case ref == "" && len(vmids) == 0:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: name a non-empty director ref or a positive VMID")
	}
	plan := StemcellRefsPlan{
		Actions:           []StemcellRefsAction{},
		CloneScanComplete: scan.CloneScanComplete,
		ScanErrors:        scan.ScanErrors,
		configPath:        req.ConfigPath,
	}
	switch {
	case req.ReconcileTags:
		// Reconciling never destroys, so it has no replicas to sweep.
		planReconcileTags(scan, &plan)
		return plan, nil
	case req.Untracked:
		// An untracked destroy sweeps no replicas: an untracked replica is
		// freed only when we name it, and a tracked one keeps its own rules.
		planUntrackedPrune(scan, vmids, &plan)
		return plan, nil
	case ref != "":
		planDirectorPrune(scan, ref, req.AllowOwnedVMs, &plan)
	default:
		planVMIDPrune(scan, vmids, &plan)
	}
	attachReplicaSweeps(scan, &plan)
	return plan, nil
}

// stemcellRefsRequestVMIDs returns the distinct VMIDs req names, in the order
// it names them, or an error when it names one both ways or a VMID that is
// not positive.
func stemcellRefsRequestVMIDs(req StemcellRefsPruneRequest) ([]int64, error) {
	if req.VMID != 0 && len(req.VMIDs) > 0 {
		return nil, cpierrors.Cloud("stemcell-refs: name the VMIDs in VMID or in VMIDs, not both")
	}
	if req.VMID < 0 {
		return nil, cpierrors.Cloud("stemcell-refs: name a non-empty director ref or a positive VMID")
	}
	if req.VMID > 0 {
		return []int64{req.VMID}, nil
	}
	var out []int64
	for _, id := range req.VMIDs {
		if id <= 0 {
			return nil, cpierrors.Cloud("stemcell-refs: VMID %d is not positive", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// planReconcileTags plans prune --reconcile-tags. Every cache template whose
// description refs are known gets one remove-tags action when it carries a
// director tag that none of those refs maps to; the action keeps every tag a
// ref maps to and adds bosh-stemcell-cache when the drops would leave no
// generation marker. A template whose refs are unknown (unread, unparseable,
// or a blank description that is not legacy) and that carries a director tag
// gets a non-failing skip, and so does a template already marked
// bosh-destroy-pending, whose tags the destroy is about to make moot, and a
// template whose ref set is empty, because its director tags are what keep it
// reachable by its director's orphan prune.
// Nothing here removes a ref or destroys a template, because the description
// refs alone decide whether a template stays.
func planReconcileTags(scan StemcellRefsScan, plan *StemcellRefsPlan) {
	plan.Mode = stemcellRefsModeReconcileTags
	for i := range scan.Templates {
		t := &scan.Templates[i]
		if len(t.DirectorTags) == 0 {
			continue
		}
		switch {
		case t.DescriptionState == stemcellRefsDescUnread:
			plan.Actions = append(plan.Actions, skipStemcellRefsAction(t,
				"config could not be read, so its refs are unknown: "+t.ReadError, false))
			continue
		case t.DescriptionState == stemcellRefsDescUnparseable:
			plan.Actions = append(plan.Actions, skipStemcellRefsAction(t,
				"description does not parse as provenance JSON, so its refs are unknown and its tags stay", false))
			continue
		case t.RefsUnknown:
			plan.Actions = append(plan.Actions, skipStemcellRefsAction(t,
				"blank description on a template that carries director or cache tags, so its refs are unknown "+
					"and its tags stay", false))
			continue
		case t.DestroyPending:
			plan.Actions = append(plan.Actions, skipStemcellRefsAction(t,
				"destroy already pending; its tags stay until the destroy finishes", false))
			continue
		case len(t.DescriptionRefs) == 0:
			plan.Actions = append(plan.Actions, skipStemcellRefsAction(t,
				"holds no ref, so its tags stay for the last holder's orphan prune", false))
			continue
		}
		kept, dropped, added := reconcileDirectorTags(t.Tags, t.DescriptionRefs)
		if len(dropped) == 0 {
			continue
		}
		act := newStemcellRefsAction(StemcellRefsActionRemoveTags, t)
		act.DropTags = dropped
		act.AddsCacheMarker = added
		for _, tok := range kept {
			if strings.HasPrefix(tok, directorTagPrefix) {
				act.KeptTags = append(act.KeptTags, tok)
			}
		}
		plan.Actions = append(plan.Actions, act)
	}
}

// newStemcellRefsAction starts an action for template t.
func newStemcellRefsAction(kind string, t *StemcellRefsTemplate) StemcellRefsAction {
	return StemcellRefsAction{
		Kind: kind, VMID: t.VMID, Node: t.Node, Name: t.Name, SHA8: t.SHA8, Replica: t.Replica,
		description: t.description, cfgName: t.cfgName, tags: append([]string(nil), t.Tags...), planned: true,
	}
}

// cloneBlock returns the reason a destroy of t must be blocked, or "".
func cloneBlock(scan StemcellRefsScan, t *StemcellRefsTemplate) string {
	if len(t.LinkedClones) > 0 {
		return fmt.Sprintf("template backs %d linked clone(s) (%s); delete or migrate them first",
			len(t.LinkedClones), joinVMIDs(t.LinkedClones))
	}
	if !scan.CloneScanComplete {
		return "the linked-clone scan is incomplete because some guest configs could not be read, so no destroy is safe"
	}
	return ""
}

// stemcellRefsUntrackedReason is why no path of the prune but an explicit
// --vmid with --untracked destroys untracked template t.
func stemcellRefsUntrackedReason(t *StemcellRefsTemplate) string {
	return fmt.Sprintf("untracked template: it has provenance JSON but no generation marker "+
		"(no bosh-stemcell-cache or director-- tag), so it comes from a CPI generation that never recorded refs "+
		"and its ref set proves nothing; only prune --vmid %d --untracked frees it, and only while it holds "+
		"no ref and backs no linked clone", t.VMID)
}

// destroyBlock returns the reason no destroy of t is safe, or "": t is
// untracked, or the clone check fails. Every destroying path checks it except
// --untracked, which replaces the untracked refusal with untrackedBlock.
func destroyBlock(scan StemcellRefsScan, t *StemcellRefsTemplate) string {
	if t.Untracked {
		return stemcellRefsUntrackedReason(t)
	}
	return cloneBlock(scan, t)
}

// untrackedBlock returns why --untracked must not free t, judged on t's own
// config, or "". It holds the conditions on t alone: t carries no generation
// marker, untrackedIdentityBlock proves it a stemcell cache template by its
// name and its provenance, and it holds no ref. A bosh-destroy-pending tag
// does not block it, because only an earlier --untracked apply stamps an
// untracked template, and destroying it again resumes that destroy. The
// planner adds the running and linked-clone checks, and the apply runs this
// same check on its locked read.
func untrackedBlock(t *StemcellRefsTemplate) string {
	var markers []string
	for _, tok := range t.Tags {
		if tok == stemcellCacheTag || (strings.HasPrefix(tok, directorTagPrefix) && len(tok) > len(directorTagPrefix)) {
			markers = append(markers, tok)
		}
	}
	switch {
	case t.DescriptionState == stemcellRefsDescUnread:
		return "config could not be read: " + t.ReadError
	case t.DescriptionState == stemcellRefsDescUnparseable:
		return "description does not parse as provenance JSON, so its refs are unknown and the template is never changed"
	case t.DescriptionState == stemcellRefsDescBlank:
		return "blank description, so it is not an untracked template; --untracked frees only a template whose " +
			"description is provenance JSON, a legacy template is freed with --vmid alone, and any other blank one " +
			"is never changed"
	case len(markers) > 0 || pve.HasStemcellGenerationMarker(t.Tags):
		if len(markers) == 0 {
			markers = []string{"tag"}
		}
		return fmt.Sprintf("tracked template: it carries the generation marker %s, so --untracked refuses it; "+
			"free it with prune --vmid %d without --untracked", strings.Join(markers, ", "), t.VMID)
	}
	if reason := untrackedIdentityBlock(t); reason != "" {
		return "not proven to be a stemcell cache template: " + reason
	}
	switch {
	case len(t.DescriptionRefs) > 0:
		return fmt.Sprintf("holds director_refs (%s); --untracked frees only a template that holds no ref",
			strings.Join(t.DescriptionRefs, ", "))
	case !t.Untracked:
		return "not an untracked template"
	}
	return ""
}

// planDestroy fills a destroy-kind action, or turns it into a blocked one
// when destroyBlock fails.
func planDestroy(scan StemcellRefsScan, kind string, t *StemcellRefsTemplate) StemcellRefsAction {
	act := newStemcellRefsAction(kind, t)
	if reason := destroyBlock(scan, t); reason != "" {
		act.Kind = StemcellRefsActionBlocked
		act.Reason = reason
		act.LinkedClones = t.LinkedClones
		act.Failure = true
	}
	return act
}

// matchingDirectorTag returns t's tag token equal to dirTag, ignoring case.
func matchingDirectorTag(tags []string, dirTag string) string {
	if dirTag == directorTagPrefix {
		return ""
	}
	for _, tok := range tags {
		if strings.EqualFold(tok, dirTag) {
			return tok
		}
	}
	return ""
}

func planDirectorPrune(scan StemcellRefsScan, ref string, allowOwned bool, plan *StemcellRefsPlan) {
	plan.Mode = "director"
	plan.DirectorRef = ref
	plan.DirectorTag = directorTagFor(ref)
	plan.OwnerTag = ownerTagFor(ref)
	planDirectorOwnership(scan, ref, allowOwned, plan)
	// Non-replica templates are planned first, so each replica's plan can
	// see which templates of its sha8 survive, and so the apply reaches a
	// replica only after every template it depends on.
	for i := range scan.Templates {
		if t := &scan.Templates[i]; !t.Replica {
			if act, ok := planDirectorTemplate(scan, t, ref, plan.DirectorTag, nil); ok {
				plan.Actions = append(plan.Actions, act)
			}
		}
	}
	destroying := map[int64]bool{}
	for i := range plan.Actions {
		if plan.Actions[i].destroys() {
			destroying[plan.Actions[i].VMID] = true
		}
	}
	for i := range scan.Templates {
		if t := &scan.Templates[i]; t.Replica {
			if act, ok := planDirectorTemplate(scan, t, ref, plan.DirectorTag, destroying); ok {
				plan.Actions = append(plan.Actions, act)
			}
		}
	}
}

// planDirectorOwnership fills the owned-VM guard. A BOSH guest counts as owned
// when it carries the ref's owner tag, and a parker when it carries the ref's
// director tag. The plan is refused in three cases:
//
//   - Some BOSH guest carries no owner tag. That guest could belong to the
//     ref's director, so a count of zero proves nothing for any ref, and
//     allowOwned does not override the refusal.
//   - No owner tag can exist for the ref, as for the unknown-director
//     sentinel, because create_vm stamps no owner tag on a VM made without a
//     director UUID. allowOwned overrides this one.
//   - Guests the ref owns still exist. allowOwned overrides this one too.
func planDirectorOwnership(scan StemcellRefsScan, ref string, allowOwned bool, plan *StemcellRefsPlan) {
	for i := range scan.Guests {
		if stemcellRefsGuestOwnedBy(&scan.Guests[i], ref, plan.OwnerTag, plan.DirectorTag) {
			plan.OwnedVMs = append(plan.OwnedVMs, scan.Guests[i])
		}
	}
	sentinel := ref == unknownDirectorRef || plan.OwnerTag == ""
	plan.CoverageIncomplete = len(scan.Uncovered) > 0
	plan.Uncovered = scan.Uncovered
	plan.OwnershipUnknown = sentinel || plan.CoverageIncomplete
	sentinelReason := fmt.Sprintf(
		"ownership cannot be determined for %s: VMs created without a director UUID carry no owner tag, "+
			"so the owned-VM check cannot see the callers that use this ref. Pass --allow-owned-vms only "+
			"when we know no such caller still deploys from these templates", ref)
	switch {
	case plan.CoverageIncomplete:
		plan.OwnershipUnknownReason = stemcellRefsCoverageReason(scan.Uncovered)
		plan.Refused = true
		plan.RefusalReason = plan.OwnershipUnknownReason
		return
	case sentinel:
		plan.OwnershipUnknownReason = sentinelReason
	}
	if allowOwned {
		return
	}
	switch {
	case plan.OwnershipUnknown:
		plan.Refused = true
		plan.RefusalReason = sentinelReason
	case len(plan.OwnedVMs) > 0:
		ids := make([]int64, 0, len(plan.OwnedVMs))
		for i := range plan.OwnedVMs {
			ids = append(ids, plan.OwnedVMs[i].VMID)
		}
		plan.Refused = true
		plan.RefusalReason = fmt.Sprintf(
			"%d non-template VM(s) the ref owns still exist (%s): BOSH guests that carry the owner tag %s, "+
				"or parkers that carry the director tag %s; the director may still be alive. "+
				"Pass --allow-owned-vms only when we know those VMs are orphans",
			len(ids), joinVMIDs(ids), plan.OwnerTag, plan.DirectorTag)
	}
}

// stemcellRefsTagKept explains a director tag the plan keeps.
const stemcellRefsTagKept = "kept: another ref the template holds maps to it"

// stemcellRefsModeReconcileTags is the plan mode of prune --reconcile-tags.
const stemcellRefsModeReconcileTags = "reconcile-tags"

// skipStemcellRefsAction is a skip on t with reason.
func skipStemcellRefsAction(t *StemcellRefsTemplate, reason string, failure bool) StemcellRefsAction {
	act := newStemcellRefsAction(StemcellRefsActionSkip, t)
	act.Reason = reason
	act.Failure = failure
	return act
}

// planDirectorTemplate plans one template for a director prune, or reports
// false when the template neither holds the ref nor carries its tag. Replicas
// are planned like any other template, except that their destroys follow
// replicaDestroyBlock; destroying names the non-replica templates the plan
// destroys, and is nil while those are being planned.
func planDirectorTemplate(
	scan StemcellRefsScan, t *StemcellRefsTemplate, ref, dirTag string, destroying map[int64]bool,
) (StemcellRefsAction, bool) {
	var spellings []string
	if t.DescriptionState == stemcellRefsDescProvenance {
		spellings = storedRefSpellings(t.DescriptionRefs, ref)
	}
	inDesc := len(spellings) > 0
	tagTok := matchingDirectorTag(t.Tags, dirTag)
	switch {
	case t.DescriptionState == stemcellRefsDescUnread:
		return skipStemcellRefsAction(t,
			"config could not be read, so we cannot tell whether it holds the ref: "+t.ReadError, true), true
	case t.DescriptionState == stemcellRefsDescUnparseable:
		if tagTok == "" && !strings.Contains(t.description, ref) {
			return StemcellRefsAction{}, false
		}
		return skipStemcellRefsAction(t,
			"description does not parse as provenance JSON, so the template is never changed", true), true
	case t.RefsUnknown:
		// A blank description on a template that is not legacy.
		if tagTok == "" {
			return StemcellRefsAction{}, false
		}
		return skipStemcellRefsAction(t, "blank description on a template that carries director or cache tags, "+
			"so its refs are unknown and the template is never changed", true), true
	case len(spellings) > 1:
		return skipStemcellRefsAction(t, fmt.Sprintf(
			"the description holds the ref under %d spellings (%q); fix it by hand", len(spellings), spellings), true), true
	case t.DestroyPending:
		return planDirectorPending(scan, t, spellings, tagTok, destroying)
	case inDesc && t.Replica:
		return planDirectorReplicaRemoveRef(scan, t, spellings[0], tagTok, destroying), true
	case inDesc:
		return planDirectorRemoveRef(scan, t, spellings[0], tagTok), true
	case tagTok != "":
		if _, dropped, added := dropDirectorTagForKept(t.Tags, tagTok, t.DescriptionRefs); dropped {
			act := newStemcellRefsAction(StemcellRefsActionRemoveTag, t)
			act.DropTag = tagTok
			act.AddsCacheMarker = added
			return act, true
		}
		return skipStemcellRefsAction(t, "tag "+tagTok+" "+stemcellRefsTagKept, false), true
	}
	return StemcellRefsAction{}, false
}

// planDirectorPending plans a template already marked bosh-destroy-pending.
func planDirectorPending(
	scan StemcellRefsScan, t *StemcellRefsTemplate, spellings []string, tagTok string, destroying map[int64]bool,
) (StemcellRefsAction, bool) {
	switch {
	case len(spellings) == 1 && len(removeStringFromSlice(t.DescriptionRefs, spellings[0])) == 0:
		act := planDestroy(scan, StemcellRefsActionResumeDestroy, t)
		act.MatchedRef = spellings[0]
		if !t.Replica || !act.destroys() {
			return act, true
		}
		if reason := replicaDestroyBlock(scan, t, destroying); reason != "" {
			act.Kind = StemcellRefsActionBlocked
			act.Reason = reason
			act.Failure = true
		}
		return act, true
	case len(spellings) > 0:
		return skipStemcellRefsAction(t,
			"destroy is pending but other refs remain, which no CPI path leaves behind; inspect it by hand", true), true
	case tagTok != "":
		return skipStemcellRefsAction(t,
			"destroy already pending; the ref is only in its tags, so the prune leaves it", false), true
	}
	return StemcellRefsAction{}, false
}

// planDirectorRemoveRef plans the removal of matched, the stored spelling of
// the ref, from t's description. The last ref goes with a destroy, which the
// clone check can block; a template that is blocked keeps its last ref.
func planDirectorRemoveRef(scan StemcellRefsScan, t *StemcellRefsTemplate, matched, tagTok string) StemcellRefsAction {
	remaining := removeStringFromSlice(t.DescriptionRefs, matched)
	if len(remaining) == 0 {
		act := planDestroy(scan, StemcellRefsActionRemoveRefAndDestroy, t)
		act.MatchedRef = matched
		return act
	}
	act := newStemcellRefsAction(StemcellRefsActionRemoveRef, t)
	act.MatchedRef = matched
	act.RemainingRefs = remaining
	if tagTok != "" {
		if _, dropped, added := dropDirectorTagForKept(t.Tags, tagTok, remaining); dropped {
			act.DropTag = tagTok
			act.AddsCacheMarker = added
		} else {
			act.Reason = "tag " + tagTok + " " + stemcellRefsTagKept
		}
	}
	return act
}

// planDirectorReplicaRemoveRef plans the removal of matched from replica t.
// The ref goes even when it is the replica's last one, because a replica's
// refs only record the calls that seeded it. A replica left with no ref is
// destroyed only when replicaDestroyBlock allows it; otherwise it stays with
// an empty ref set, and the action says why.
func planDirectorReplicaRemoveRef(
	scan StemcellRefsScan, t *StemcellRefsTemplate, matched, tagTok string, destroying map[int64]bool,
) StemcellRefsAction {
	if len(removeStringFromSlice(t.DescriptionRefs, matched)) > 0 {
		return planDirectorRemoveRef(scan, t, matched, tagTok)
	}
	reason := replicaDestroyBlock(scan, t, destroying)
	kind := StemcellRefsActionRemoveRefAndDestroy
	if reason != "" {
		kind = StemcellRefsActionRemoveRef
	}
	act := newStemcellRefsAction(kind, t)
	act.MatchedRef = matched
	var notes []string
	if reason != "" {
		notes = append(notes, "the replica stays with no refs: "+reason)
	}
	if tagTok != "" {
		// DropTag also serves a destroy that the apply turns into a keep.
		if _, dropped, added := dropDirectorTagForKept(t.Tags, tagTok, nil); dropped {
			act.DropTag = tagTok
			act.AddsCacheMarker = added && reason != ""
		} else if reason != "" {
			notes = append(notes, "tag "+tagTok+" "+stemcellRefsTagKept)
		}
	}
	act.Reason = strings.Join(notes, "; ")
	return act
}

// replicaDestroyBlock returns why a director prune must keep replica t once
// it holds no ref, or "". These are the --vmid preconditions for a replica:
// a known sha8, a tracked template, no linked clone, and no non-replica
// template of its sha8 that survives the plan. A linked clone of the replica blocks only the
// replica, because the clone depends on the replica's base volume and not on
// its anchor's. The template flag is checked under the lock at apply time.
func replicaDestroyBlock(scan StemcellRefsScan, t *StemcellRefsTemplate, destroying map[int64]bool) string {
	if t.SHA8 == "" {
		return "replica with no known sha8, so we cannot prove its anchor is gone"
	}
	if reason := destroyBlock(scan, t); reason != "" {
		return reason
	}
	for i := range scan.Templates {
		a := &scan.Templates[i]
		if !a.Replica && a.SHA8 == t.SHA8 && !destroying[a.VMID] {
			return fmt.Sprintf("replica of template %d, which this prune keeps; it is freed with that template", a.VMID)
		}
	}
	return ""
}

// liveAnchorFor returns a non-replica template other than self with sha8,
// or false when none exists.
func liveAnchorFor(scan StemcellRefsScan, sha8 string, self int64) (StemcellRefsTemplate, bool) {
	if sha8 == "" {
		return StemcellRefsTemplate{}, false
	}
	for i := range scan.Templates {
		if t := &scan.Templates[i]; t.VMID != self && !t.Replica && t.SHA8 == sha8 {
			return *t, true
		}
	}
	return StemcellRefsTemplate{}, false
}

// zeroRefBlock returns why t fails the --vmid preconditions that concern t
// alone, or "". Those preconditions are that t provably holds no ref (a
// blank description counts only on a legacy template), that a replica has a
// known sha8, that t is not untracked, and that t backs no linked clone. A
// replica swept with its anchor must pass the same check.
func zeroRefBlock(scan StemcellRefsScan, t *StemcellRefsTemplate) string {
	switch {
	case t.DescriptionState == stemcellRefsDescUnread:
		return "config could not be read: " + t.ReadError
	case t.DescriptionState == stemcellRefsDescUnparseable:
		return "description does not parse as provenance JSON, so the template is never changed"
	case t.RefsUnknown:
		return "blank description on a template that is not a legacy cache template (it carries a director or " +
			"cache tag, or its name does not follow legacy cache naming), so its refs are unknown"
	case t.Replica && len(t.DescriptionRefs) > 0:
		return fmt.Sprintf("replica that holds refs of its own (%s); a replica that holds refs is never destroyed",
			strings.Join(t.DescriptionRefs, ", "))
	case len(t.DescriptionRefs) > 0:
		return fmt.Sprintf("still referenced by %s; prune those refs with --director first",
			strings.Join(t.DescriptionRefs, ", "))
	case t.Replica && t.SHA8 == "":
		return "replica with no known sha8, so we cannot prove its anchor is gone"
	}
	return destroyBlock(scan, t)
}

// stemcellRefsModeVMID and stemcellRefsModeUntracked are the plan modes of a
// --vmid prune, without and with --untracked.
const (
	stemcellRefsModeVMID      = "vmid"
	stemcellRefsModeUntracked = "untracked"
)

// pruneTarget returns the scanned stemcell cache template with vmid, or nil
// and the reason a --vmid prune refuses that VMID.
func pruneTarget(scan StemcellRefsScan, vmid int64) (*StemcellRefsTemplate, string) {
	for i := range scan.Templates {
		if scan.Templates[i].VMID == vmid {
			return &scan.Templates[i], ""
		}
	}
	switch name, ok := scan.otherTemplates[vmid]; {
	case ok:
		return nil, fmt.Sprintf("VMID %d (%s) is a template, but not a recognizable stemcell cache template", vmid, name)
	case guestByVMID(scan, vmid) != nil:
		return nil, fmt.Sprintf("VMID %d is not a template", vmid)
	}
	return nil, fmt.Sprintf("no guest with VMID %d was found in the cluster", vmid)
}

// planVMIDTargets fills plan with one action per VMID, each judged on its own
// by judge. A VMID that names no stemcell cache template gets a blocked
// action with no node or name. With one VMID, a refusal refuses the whole
// plan, as a single --vmid prune always has; with several, a refusal blocks
// only its own action, so the others still apply and the run exits 1.
func planVMIDTargets(
	scan StemcellRefsScan, vmids []int64, plan *StemcellRefsPlan,
	judge func(t *StemcellRefsTemplate) StemcellRefsAction,
) {
	if len(vmids) == 1 {
		plan.VMID = vmids[0]
	} else {
		plan.VMIDs = append([]int64(nil), vmids...)
	}
	for _, vmid := range vmids {
		var act StemcellRefsAction
		t, reason := pruneTarget(scan, vmid)
		if t == nil {
			act = StemcellRefsAction{Kind: StemcellRefsActionBlocked, VMID: vmid, Reason: reason, Failure: true,
				planned: true}
		} else {
			act = judge(t)
		}
		if len(vmids) == 1 && act.Kind == StemcellRefsActionBlocked {
			plan.Refused = true
			plan.RefusalReason = act.Reason
			if t == nil {
				// A single VMID that names no cache template has always
				// refused with no action.
				return
			}
		}
		plan.Actions = append(plan.Actions, act)
	}
}

// blockStemcellRefsAction turns act into a failing blocked action with reason.
func blockStemcellRefsAction(act *StemcellRefsAction, t *StemcellRefsTemplate, reason string) {
	act.Kind = StemcellRefsActionBlocked
	act.Reason = reason
	act.Failure = true
	act.Frees = nil
	if len(t.LinkedClones) > 0 {
		act.LinkedClones = t.LinkedClones
	}
}

// planVMIDPrune plans prune --vmid without --untracked. Each template it
// names must pass zeroRefBlock, and a replica whose anchor is still live is
// left for that anchor's destroy to sweep.
func planVMIDPrune(scan StemcellRefsScan, vmids []int64, plan *StemcellRefsPlan) {
	plan.Mode = stemcellRefsModeVMID
	planVMIDTargets(scan, vmids, plan, func(t *StemcellRefsTemplate) StemcellRefsAction {
		act := newStemcellRefsAction(StemcellRefsActionDestroy, t)
		reason := zeroRefBlock(scan, t)
		if reason == "" && t.Replica {
			if anchor, ok := liveAnchorFor(scan, t.SHA8, t.VMID); ok {
				reason = fmt.Sprintf("replica of live template %d; it is freed with that template", anchor.VMID)
				act.liveAnchorBlocked = true
			}
		}
		if reason != "" {
			blockStemcellRefsAction(&act, t, reason)
		}
		return act
	})
	coverReplicasOfDestroyedAnchors(scan, plan)
}

// coverReplicasOfDestroyedAnchors turns the live-anchor block of each
// replica a --vmid prune names into a non-failing skip when the same prune
// destroys every non-replica template of the replica's sha8. The last of
// those destroys sweeps the replica, as attachReplicaSweeps arranges, so the
// replica is freed with that template.
func coverReplicasOfDestroyedAnchors(scan StemcellRefsScan, plan *StemcellRefsPlan) {
	destroying := map[int64]bool{}
	carrier := map[string]int64{}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if !a.destroys() {
			continue
		}
		destroying[a.VMID] = true
		if a.SHA8 != "" && !isReplicaVMID(scan, a.VMID) {
			carrier[a.SHA8] = a.VMID
		}
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if !a.liveAnchorBlocked || a.SHA8 == "" || carrier[a.SHA8] == 0 {
			continue
		}
		if _, survivor := sha8Replicas(scan, a.SHA8, destroying); survivor {
			continue
		}
		a.Kind = StemcellRefsActionSkip
		a.Failure = false
		a.LinkedClones = nil
		a.liveAnchorBlocked = false
		a.sweptWith = carrier[a.SHA8]
		a.Reason = fmt.Sprintf("replica of template %d, which this prune destroys; it is freed with template %d "+
			"in the same sweep", a.sweptWith, a.sweptWith)
	}
}

// planUntrackedPrune plans prune --vmid with --untracked. Each template it
// names must be a recognized stemcell cache template that untrackedBlock
// passes, that the listing does not report as running, and that cloneBlock
// passes, so a linked clone or an incomplete clone scan still blocks it. A
// replica is judged like any other template: its anchor's destroy never
// sweeps an untracked replica, so the live-anchor rule of a plain --vmid
// prune would leave it with no way out.
func planUntrackedPrune(scan StemcellRefsScan, vmids []int64, plan *StemcellRefsPlan) {
	plan.Mode = stemcellRefsModeUntracked
	planVMIDTargets(scan, vmids, plan, func(t *StemcellRefsTemplate) StemcellRefsAction {
		act := newStemcellRefsAction(StemcellRefsActionDestroyUntracked, t)
		act.Frees = &StemcellRefsFrees{Volumes: append([]StemcellRefsVolume{}, t.volumes...)}
		act.ResumesPending = t.DestroyPending
		reason := untrackedBlock(t)
		if reason == "" && t.Running {
			reason = "the guest listing reports the template as running, so it is not freed"
		}
		if reason == "" {
			reason = cloneBlock(scan, t)
		}
		if reason != "" {
			blockStemcellRefsAction(&act, t, reason)
			act.ResumesPending = false
		}
		return act
	})
}

// guestByVMID returns the non-template guest with vmid, or nil.
func guestByVMID(scan StemcellRefsScan, vmid int64) *StemcellRefsGuest {
	for i := range scan.Guests {
		if scan.Guests[i].VMID == vmid {
			return &scan.Guests[i]
		}
	}
	return nil
}

// attachReplicaSweeps decides what happens to a sha8's replicas when a plan
// destroys every non-replica template of that sha8. The last of those
// destroys carries the sweep. Any replica without an action of its own is
// swept only when it passes the --vmid preconditions on its own
// (zeroRefBlock): it holds no ref, its description can be read and parsed,
// it is tracked, and it backs no linked clone. Every other such replica is
// listed in KeptReplicas with its reason and left alone. A replica with an
// action of its own is left to that action; when that action neither
// destroys it nor changes its refs and it holds no ref, it is listed in
// KeptReplicas too, so no zero-ref replica the sweep leaves goes unreported.
func attachReplicaSweeps(scan StemcellRefsScan, plan *StemcellRefsPlan) {
	destroying := map[int64]bool{}
	plannedKind := map[int64]string{}
	// covered holds the replicas whose own skip action says the sweep frees
	// them, so the sweep takes them as if they had no action.
	covered := map[int64]int64{}
	carrier := map[string]int{}
	for i := range plan.Actions {
		if plan.Actions[i].sweptWith != 0 {
			covered[plan.Actions[i].VMID] = plan.Actions[i].sweptWith
			continue
		}
		plannedKind[plan.Actions[i].VMID] = plan.Actions[i].Kind
		if plan.Actions[i].destroys() {
			destroying[plan.Actions[i].VMID] = true
		}
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if !a.destroys() || a.SHA8 == "" || isReplicaVMID(scan, a.VMID) {
			continue
		}
		carrier[a.SHA8] = i
	}
	for sha8, idx := range carrier {
		replicas, survivor := sha8Replicas(scan, sha8, destroying)
		if survivor {
			continue
		}
		a := &plan.Actions[idx]
		for _, r := range replicas {
			if kind, ok := plannedKind[r.VMID]; ok {
				if k, listed := ownActionKeptReplica(scan, r, kind, a.VMID); listed {
					a.KeptReplicas = append(a.KeptReplicas, k)
				}
				continue
			}
			if reason := zeroRefBlock(scan, r); reason != "" {
				a.KeptReplicas = append(a.KeptReplicas,
					StemcellRefsKeptReplica{VMID: r.VMID, Node: r.Node, Name: r.Name, Reason: reason})
				continue
			}
			a.SweepReplicas = append(a.SweepReplicas, StemcellRefsTemplateRef{
				VMID: r.VMID, Node: r.Node, Name: r.Name, Tags: append([]string(nil), r.Tags...),
				description: r.description, cfgName: r.cfgName,
			})
			if covered[r.VMID] == a.VMID {
				delete(covered, r.VMID)
			}
		}
	}
	uncoverReplicas(scan, plan, covered)
}

// uncoverReplicas turns back into a failing block the skip action of every
// replica in covered, which no sweep carries after all, so a replica the
// plan said would be freed is never quietly left in place.
func uncoverReplicas(scan StemcellRefsScan, plan *StemcellRefsPlan, covered map[int64]int64) {
	for i := range plan.Actions {
		a := &plan.Actions[i]
		carrierVMID, ok := covered[a.VMID]
		if !ok || a.sweptWith == 0 {
			continue
		}
		a.Kind = StemcellRefsActionBlocked
		a.Failure = true
		a.sweptWith = 0
		a.Reason = fmt.Sprintf("replica of template %d, but that template's destroy does not sweep it; "+
			"free it with prune --vmid %d once template %d is gone", carrierVMID, a.VMID, carrierVMID)
		if r := templateByVMID(scan, a.VMID); r != nil && len(r.LinkedClones) > 0 {
			a.LinkedClones = r.LinkedClones
		}
	}
}

// templateByVMID returns the scanned template with vmid, or nil.
func templateByVMID(scan StemcellRefsScan, vmid int64) *StemcellRefsTemplate {
	for i := range scan.Templates {
		if scan.Templates[i].VMID == vmid {
			return &scan.Templates[i]
		}
	}
	return nil
}

// ownActionKeptReplica returns the KeptReplicas entry for replica r, which
// has an action of kind in the plan, or false when that action already
// explains what happens to r. A remove-tag or skip on a replica that holds
// no ref leaves it in place without a sweep, so it is listed with whether a
// --vmid prune can free it once carrier, its sha8's last main template, is
// gone.
func ownActionKeptReplica(scan StemcellRefsScan, r *StemcellRefsTemplate, kind string, carrier int64) (StemcellRefsKeptReplica, bool) {
	if (kind != StemcellRefsActionRemoveTag && kind != StemcellRefsActionSkip) || !refsKnownZero(r) {
		return StemcellRefsKeptReplica{}, false
	}
	k := StemcellRefsKeptReplica{VMID: r.VMID, Node: r.Node, Name: r.Name}
	if block := zeroRefBlock(scan, r); block != "" {
		k.Reason = fmt.Sprintf("holds no ref but has its own %s action, so the sweep leaves it; "+
			"a --vmid prune would refuse it: %s", kind, block)
		return k, true
	}
	k.FreeableByVMID = true
	k.Reason = fmt.Sprintf("holds no ref but has its own %s action, so the sweep leaves it; "+
		"once template %d is destroyed, free it with prune --vmid %d", kind, carrier, r.VMID)
	return k, true
}

// sha8Replicas returns the replicas of sha8, and whether a non-replica
// template of sha8 survives the plan.
func sha8Replicas(scan StemcellRefsScan, sha8 string, destroying map[int64]bool) ([]*StemcellRefsTemplate, bool) {
	var replicas []*StemcellRefsTemplate
	survivor := false
	for i := range scan.Templates {
		t := &scan.Templates[i]
		switch {
		case t.SHA8 != sha8:
		case t.Replica:
			replicas = append(replicas, t)
		case !destroying[t.VMID]:
			survivor = true
		}
	}
	return replicas, survivor
}

// isReplicaVMID reports whether the scanned template vmid is a replica.
func isReplicaVMID(scan StemcellRefsScan, vmid int64) bool {
	for i := range scan.Templates {
		if scan.Templates[i].VMID == vmid {
			return scan.Templates[i].Replica
		}
	}
	return false
}

// Apply outcomes.
const (
	StemcellRefsOutcomeApplied = "applied"
	StemcellRefsOutcomeGone    = "already-gone"
	StemcellRefsOutcomeChanged = "changed-since-plan"
	StemcellRefsOutcomeFailed  = "failed"
	StemcellRefsOutcomeSkipped = "skipped"
)

// StemcellRefsActionResult is what applying one action did.
type StemcellRefsActionResult struct {
	Action StemcellRefsAction `json:"action"`
	// AppliedKind is the kind the apply ran. It differs from Action.Kind only
	// for a replica whose planned destroy was turned into a ref removal, or
	// skipped, because another template of its sha8 was not destroyed.
	AppliedKind string `json:"applied_kind"`
	Outcome     string `json:"outcome"`
	Error       string `json:"error,omitempty"`
	// KeptReason says why a template the plan meant to destroy was kept.
	KeptReason string `json:"kept_reason,omitempty"`
	// Destroyed lists every template the action destroyed, replicas
	// included.
	Destroyed     []int64  `json:"destroyed,omitempty"`
	RemainingRefs []string `json:"remaining_refs,omitempty"`
	TagRemoved    bool     `json:"tag_removed,omitempty"`
	// CacheMarkerAdded is true when the write that removed the tag also
	// added bosh-stemcell-cache, because the tag was the last generation
	// marker of a template the prune keeps.
	CacheMarkerAdded bool `json:"cache_marker_added,omitempty"`
	// Notes records replicas preserved or failed during the sweep.
	Notes []string `json:"notes,omitempty"`
	// Failure is true when the action counts against the exit code.
	Failure bool `json:"failure"`
}

// StemcellRefsApplyResult is the whole apply. A refused plan yields the same
// shape with Refused set and no results, so a JSON consumer reads one shape.
type StemcellRefsApplyResult struct {
	Plan    StemcellRefsPlan           `json:"plan"`
	Refused bool                       `json:"refused"`
	Results []StemcellRefsActionResult `json:"results"`
	// Failures counts the results with Failure set.
	Failures int `json:"failures"`
}

// ApplyStemcellRefsPrune executes plan. A refused plan changes nothing and
// returns ErrStemcellRefsPruneRefused with a result whose Refused flag is set.
// Each action runs on its own: a failure is recorded in its result and the
// apply moves on, so the caller exits 1 when Failures is non-zero.
func ApplyStemcellRefsPrune(ctx context.Context, deps Deps, plan StemcellRefsPlan) (StemcellRefsApplyResult, error) {
	res := StemcellRefsApplyResult{Plan: plan, Results: []StemcellRefsActionResult{}}
	if plan.Refused {
		res.Refused = true
		return res, ErrStemcellRefsPruneRefused
	}
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return res, cpierrors.Cloud("stemcell-refs: PVE client unavailable")
	}
	logger := deps.Log(ctx).With(log.String("operation", "stemcell_refs.prune"))
	sha8Unfinished := map[string]bool{}
	for i := range plan.Actions {
		act := &plan.Actions[i]
		r := applyStemcellRefsAction(ctx, deps, logger, plan, *act, sha8Unfinished)
		if act.destroys() && !act.Replica && act.SHA8 != "" && len(r.Destroyed) == 0 &&
			r.Outcome != StemcellRefsOutcomeGone {
			sha8Unfinished[act.SHA8] = true
		}
		if r.Failure {
			res.Failures++
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

// stemcellRefsVerifier returns the check every locked apply step runs on its
// fresh config read. The guest must still be a template, keep the config
// name the scan read (cfgName, "" when the config had none), and keep the
// scanned description and tag set; anything else is ErrStemcellRefsChanged
// and the step writes nothing.
func stemcellRefsVerifier(cfgName, description string, tags []string) func(cfg map[string]any) error {
	return func(cfg map[string]any) error {
		freshName := stringConfigField(cfg, pveConfigKeyName)
		switch {
		case !pve.ConfigFlag(cfg, pveConfigKeyTemplate):
			return fmt.Errorf("%w: the guest is no longer a template", ErrStemcellRefsChanged)
		case freshName != cfgName:
			return fmt.Errorf("%w: its name changed from %q to %q", ErrStemcellRefsChanged, cfgName, freshName)
		case stringConfigField(cfg, pveConfigKeyDescription) != description:
			return fmt.Errorf("%w: its description changed", ErrStemcellRefsChanged)
		case !sameTagSet(parseTagsField(stringConfigField(cfg, jsonKeyTags)), tags):
			return fmt.Errorf("%w: its tags changed", ErrStemcellRefsChanged)
		}
		return nil
	}
}

// confirmDestroyPending re-reads a template just before its destroy, after
// the lock that stamped bosh-destroy-pending has been released. A
// registerStemcellDirectorRef call in that window removes the stamp when it
// adds a live ref, so a missing stamp aborts the destroy with
// ErrStemcellRefsChanged and the template is kept. A guest that is already
// gone is left to destroyTemplateVM, which treats that as success. This
// narrows the window delete_stemcell's envelope leaves open; it can't close
// it, because the destroy itself runs outside the lock.
func confirmDestroyPending(ctx context.Context, deps Deps, node string, vmid int64) error {
	cfg, err := deps.PVE.QEMU().Config(ctx, node, int(vmid)) //nolint:gosec // VMID is bounded by PVE valid range
	if err != nil {
		if stemcellRefsGone(err) {
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("stemcell-refs: re-read config vmid %d node %q before the destroy", vmid, node))
	}
	if !stringSliceContains(parseTagsField(stringConfigField(cfg, jsonKeyTags)), stemcellDestroyPendingTag) {
		return fmt.Errorf("%w: %s was removed before the destroy, so a director registered against it; "+
			"the template is kept", ErrStemcellRefsChanged, stemcellDestroyPendingTag)
	}
	return nil
}

// stemcellRefsGone reports whether a config read says the guest no longer
// exists: a 404, or the pmxcfs "configuration file does not exist" answer
// that destroyTemplateVM also treats as gone.
func stemcellRefsGone(err error) bool {
	return pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err)
}

func applyStemcellRefsAction(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	plan StemcellRefsPlan,
	act StemcellRefsAction,
	sha8Unfinished map[string]bool,
) StemcellRefsActionResult {
	r := StemcellRefsActionResult{Action: act, AppliedKind: act.Kind}
	if !act.Mutating() {
		r.Outcome = StemcellRefsOutcomeSkipped
		r.Failure = act.Failure
		return r
	}
	if !act.planned {
		r.Outcome = StemcellRefsOutcomeFailed
		r.Error = "stemcell-refs: the action did not come from the planner, so it is not applied"
		r.Failure = true
		return r
	}
	kind, skip := replicaApplyKind(act, sha8Unfinished, &r)
	if skip {
		return r
	}
	r.AppliedKind = kind
	alog := logger.With(log.String("node", act.Node), log.Int64("vmid", act.VMID), log.String("kind", kind))
	verify := stemcellRefsVerifier(act.cfgName, act.description, act.tags)
	ranDestroy := false
	destroy := func(ctx context.Context) error {
		ranDestroy = true
		if err := confirmDestroyPending(ctx, deps, act.Node, act.VMID); err != nil {
			return err
		}
		if err := destroyTemplateVM(ctx, deps, act.Node, act.VMID, stemcellRefsCIDLabel); err != nil {
			return err
		}
		r.Destroyed = append(r.Destroyed, act.VMID)
		sweepStemcellRefsReplicas(ctx, deps, alog, act, sha8Unfinished, &r)
		return nil
	}
	ref := act.MatchedRef
	if ref == "" {
		ref = plan.DirectorRef
	}
	opts := deregisterOptions{verify: verify, goneOnConfigMissing: true}

	var err error
	switch kind {
	case StemcellRefsActionRemoveRef:
		refuseDestroy := func(context.Context) error {
			return cpierrors.Cloud("stemcell-refs: the plan keeps template %d, so it is not destroyed", act.VMID)
		}
		opts.dropTag = act.DropTag
		// A replica may be kept with no refs; any other template keeps at
		// least one, which the verifier's description check guarantees.
		opts.keepWhenEmpty = act.Replica
		var out deregisterOutcome
		out, err = deregisterStemcellDirectorRefWith(ctx, deps, alog, act.Node, act.VMID, ref, refuseDestroy, opts)
		if err == nil {
			r.RemainingRefs = out.remaining
			r.TagRemoved = out.tagDropped
			r.CacheMarkerAdded = out.markerAdded
			if out.destroyed {
				r.Outcome = StemcellRefsOutcomeGone
			}
		}
	case StemcellRefsActionRemoveRefAndDestroy, StemcellRefsActionResumeDestroy:
		var out deregisterOutcome
		out, err = deregisterStemcellDirectorRefWith(ctx, deps, alog, act.Node, act.VMID, ref, destroy, opts)
		if err == nil && out.destroyed && !ranDestroy {
			r.Outcome = StemcellRefsOutcomeGone
		}
	case StemcellRefsActionRemoveTag:
		var gone bool
		gone, r.TagRemoved, r.CacheMarkerAdded, err = removeStemcellDirectorTagLocked(ctx, deps, alog, act.Node, act.VMID,
			act.DropTag, verify)
		if err == nil && gone {
			r.Outcome = StemcellRefsOutcomeGone
		}
	case StemcellRefsActionRemoveTags:
		var gone bool
		gone, r.TagRemoved, r.CacheMarkerAdded, err = reconcileStemcellDirectorTagsLocked(ctx, deps, alog, act.Node,
			act.VMID, verify)
		if err == nil && gone {
			r.Outcome = StemcellRefsOutcomeGone
		}
	case StemcellRefsActionDestroy:
		var gone bool
		gone, err = destroyUnreferencedTemplateLocked(ctx, deps, alog, act.Node, act.VMID, verify, destroy)
		if err == nil && gone {
			r.Outcome = StemcellRefsOutcomeGone
		}
	case StemcellRefsActionDestroyUntracked:
		var gone bool
		gone, err = destroyUntrackedTemplateLocked(ctx, deps, alog, act, plan.configPath, verify, destroy)
		if err == nil && gone {
			r.Outcome = StemcellRefsOutcomeGone
		}
	}
	if err == nil && r.Outcome == StemcellRefsOutcomeGone && len(act.SweepReplicas) > 0 {
		ids := make([]int64, 0, len(act.SweepReplicas))
		for _, rep := range act.SweepReplicas {
			ids = append(ids, rep.VMID)
		}
		r.Notes = append(r.Notes, fmt.Sprintf("replicas %s kept: template %d was already gone, so no sweep ran; "+
			"run the report again and free them with --vmid", joinVMIDs(ids), act.VMID))
	}

	switch {
	case errors.Is(err, ErrStemcellRefsChanged):
		r.Outcome = StemcellRefsOutcomeChanged
		r.Error = err.Error()
		r.Failure = true
		alog.Warn("stemcell-refs: template changed since the plan; skipped", log.Err(err))
	case err != nil:
		r.Outcome = StemcellRefsOutcomeFailed
		r.Error = err.Error()
		r.Failure = true
		if pve.IsBaseVolumeInUse(err) {
			r.Error = stemcellRefsBaseInUseError(kind, plan.configPath, act.VMID, err)
		}
		alog.Warn("stemcell-refs: action failed", log.Err(err))
	case r.Outcome == "":
		r.Outcome = StemcellRefsOutcomeApplied
		if r.Failure {
			r.Outcome = StemcellRefsOutcomeFailed
		}
	}
	return r
}

// stemcellRefsBaseInUseError is the result error for an action whose destroy
// PVE refused because the template's base volume still backs a linked clone.
// The template carries bosh-destroy-pending by then, so the message says how
// the destroy resumes: an untracked destroy resumes only with the exact
// --untracked --apply command, any other destroy on a plain rerun.
func stemcellRefsBaseInUseError(kind, configPath string, vmid int64, err error) string {
	if kind == StemcellRefsActionDestroyUntracked {
		return fmt.Sprintf("template still backs one or more linked-clone VMs; delete or migrate them. %s: %s",
			untrackedResumeNote(configPath, vmid), err.Error())
	}
	return fmt.Sprintf("template still backs one or more linked-clone VMs; delete or migrate them, "+
		"then rerun (the destroy is marked pending and resumes): %s", err.Error())
}

// replicaApplyKind returns the kind the apply runs for act. A replica's
// destroy depends on every non-replica template of its sha8 going first, so
// when one of those destroys did not finish, a remove-ref-and-destroy only
// removes the ref and a resumed destroy is skipped; both record the reason in
// r.KeptReason. skip reports that r is final.
func replicaApplyKind(act StemcellRefsAction, sha8Unfinished map[string]bool, r *StemcellRefsActionResult) (string, bool) {
	// An untracked destroy never depends on an anchor, as planUntrackedPrune
	// explains.
	if !act.Replica || !act.destroys() || act.Kind == StemcellRefsActionDestroyUntracked || !sha8Unfinished[act.SHA8] {
		return act.Kind, false
	}
	r.KeptReason = fmt.Sprintf("replica %d not destroyed: another template of sha8 %s was not destroyed",
		act.VMID, act.SHA8)
	if act.Kind != StemcellRefsActionRemoveRefAndDestroy {
		r.AppliedKind = StemcellRefsActionSkip
		r.Outcome = StemcellRefsOutcomeSkipped
		r.Failure = true
		return act.Kind, true
	}
	return StemcellRefsActionRemoveRef, false
}

// sweepStemcellRefsReplicas destroys the replicas planned to go with act's
// template. Each one goes through destroyUnreferencedTemplateLocked with its
// own scanned name, description, and tags, so a replica that gained a ref,
// changed, or stopped being a template since the plan is kept. It sweeps
// nothing when an earlier destroy of the same sha8 did not finish, because a
// non-replica template of that sha8 then survives.
func sweepStemcellRefsReplicas(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	act StemcellRefsAction,
	sha8Unfinished map[string]bool,
	r *StemcellRefsActionResult,
) {
	if len(act.SweepReplicas) == 0 {
		return
	}
	if sha8Unfinished[act.SHA8] {
		r.Notes = append(r.Notes, "replicas kept: another template of sha8 "+act.SHA8+" was not destroyed")
		return
	}
	for _, rep := range act.SweepReplicas {
		destroyed := false
		destroyReplica := func(ctx context.Context) error {
			if err := confirmDestroyPending(ctx, deps, rep.Node, rep.VMID); err != nil {
				return err
			}
			if err := destroyTemplateVM(ctx, deps, rep.Node, rep.VMID, stemcellRefsCIDLabel); err != nil {
				return err
			}
			destroyed = true
			return nil
		}
		verify := stemcellRefsVerifier(rep.cfgName, rep.description, rep.Tags)
		gone, err := destroyUnreferencedTemplateLocked(ctx, deps, logger, rep.Node, rep.VMID, verify, destroyReplica)
		switch {
		case errors.Is(err, ErrStemcellRefsChanged):
			r.Failure = true
			r.Notes = append(r.Notes, fmt.Sprintf("replica %d kept: %v", rep.VMID, err))
		case err != nil && pve.IsBaseVolumeInUse(err):
			r.Failure = true
			r.Notes = append(r.Notes, fmt.Sprintf("replica %d still backs linked-clone VMs: %v", rep.VMID, err))
		case err != nil:
			r.Failure = true
			r.Notes = append(r.Notes, fmt.Sprintf("replica %d destroy failed: %v", rep.VMID, err))
		case gone:
			r.Notes = append(r.Notes, fmt.Sprintf("replica %d was already gone", rep.VMID))
		case destroyed:
			r.Destroyed = append(r.Destroyed, rep.VMID)
		}
	}
}

// sameTagSet compares two tag lists as sets.
func sameTagSet(a, b []string) bool {
	set := map[string]bool{}
	for _, t := range a {
		set[t] = true
	}
	other := map[string]bool{}
	for _, t := range b {
		other[t] = true
		if !set[t] {
			return false
		}
	}
	return len(set) == len(other)
}

// removeStemcellDirectorTagLocked removes dropTag from a template whose
// description does not hold the ref. It runs under the per-VMID lock with a
// fresh read, calls verify on that read, and applies dropDirectorTagForKept's
// rules against the description refs it finds, so a template left without a
// generation marker gains bosh-stemcell-cache in the same write. It never
// destroys.
func removeStemcellDirectorTagLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	dropTag string,
	verify func(cfg map[string]any) error,
) (gone, removed, markerAdded bool, err error) {
	owner := fmt.Sprintf("stemcell/director_ref/prune_tag/%d", vmid)
	return rewriteStemcellTagsLocked(ctx, deps, logger, node, vmid, owner, verify,
		func(tags, refs []string, _ bool) ([]string, bool, bool) {
			return dropDirectorTagForKept(tags, dropTag, refs)
		})
}

// reconcileStemcellDirectorTagsLocked is the apply of a remove-tags action
// (--reconcile-tags). Under the per-VMID lock it re-reads the template, calls
// verify on that read, and drops every director tag no description ref maps
// to, through reconcileDirectorTags, adding bosh-stemcell-cache in the same
// write when no generation marker would be left. It never touches the
// description and never destroys.
func reconcileStemcellDirectorTagsLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	verify func(cfg map[string]any) error,
) (gone, removed, markerAdded bool, err error) {
	owner := fmt.Sprintf("stemcell/director_ref/reconcile_tags/%d", vmid)
	return rewriteStemcellTagsLocked(ctx, deps, logger, node, vmid, owner, verify,
		func(tags, refs []string, refsKnown bool) ([]string, bool, bool) {
			if !refsKnown || len(refs) == 0 {
				// The verifier already proved the description is the one
				// the plan parsed; this keeps a description that does not
				// parse from ever reading as an empty ref set. A template
				// with no refs keeps its tags, as the plan does.
				return tags, false, false
			}
			kept, dropped, added := reconcileDirectorTags(tags, refs)
			return kept, len(dropped) > 0, added
		})
}

// rewriteStemcellTagsLocked is the locked tags-only write behind remove-tag
// and remove-tags. Under the per-VMID lock it reads the template, reports gone
// for a 404 or a missing pmxcfs config, calls verify, and passes the fresh
// tags and description refs to compute. When compute reports a change, the
// kept tags are written in one UpdateQemuConfig call, or the tags key is
// deleted when none are left.
func rewriteStemcellTagsLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	owner string,
	verify func(cfg map[string]any) error,
	compute func(tags, refs []string, refsKnown bool) (kept []string, changed, markerAdded bool),
) (gone, removed, markerAdded bool, err error) {
	vmidInt := int(vmid) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	err = withVMIDLock(ctx, deps.PVE.Pools(), vmidInt, owner, logger, func() error {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmidInt)
		if cfgErr != nil {
			if stemcellRefsGone(cfgErr) {
				gone = true
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("stemcell-refs: read config vmid %d node %q", vmid, node))
		}
		if verifyErr := verify(cfg); verifyErr != nil {
			return verifyErr
		}
		description := stringConfigField(cfg, pveConfigKeyDescription)
		tags := parseTagsField(stringConfigField(cfg, jsonKeyTags))
		prov, provOK := parseStemcellProvenanceFromDescription(description)
		kept, changed, added := compute(tags, prov.DirectorRefs, provOK)
		if !changed {
			return nil
		}
		params := &sdknodes.UpdateQemuConfigParams{}
		joined := strings.Join(kept, pveTagJoinSep)
		if joined == "" {
			del := jsonKeyTags
			params.Delete = &del
		} else {
			params.Tags = &joined
		}
		if updErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.FormatInt(vmid, 10), params); updErr != nil {
			return cpierrors.Wrap(pve.WrapError(updErr),
				fmt.Sprintf("stemcell-refs: write tags vmid %d node %q", vmid, node))
		}
		removed = true
		markerAdded = added
		return nil
	})
	return gone, removed, markerAdded, err
}

// destroyUnreferencedTemplateLocked frees one template that holds no ref.
// Under the per-VMID lock it re-reads the template, calls verify, and stamps
// bosh-destroy-pending with deregister's own stampDestroyPendingTagLocked.
// destroy runs after the lock releases, the same envelope
// deregisterStemcellDirectorRef uses for a last-ref destroy.
func destroyUnreferencedTemplateLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	verify func(cfg map[string]any) error,
	destroy func(context.Context) error,
) (gone bool, err error) {
	vmidInt := int(vmid) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	owner := fmt.Sprintf("stemcell/director_ref/prune_vmid/%d", vmid)
	lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmidInt, owner, logger, func() error {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmidInt)
		if cfgErr != nil {
			if stemcellRefsGone(cfgErr) {
				gone = true
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("stemcell-refs: read config vmid %d node %q", vmid, node))
		}
		if verifyErr := verify(cfg); verifyErr != nil {
			return verifyErr
		}
		return stampDestroyPendingTagLocked(ctx, deps, node, vmid, parseTagsField(stringConfigField(cfg, jsonKeyTags)))
	})
	if lockErr != nil || gone {
		return gone, lockErr
	}
	if destroyErr := destroy(ctx); destroyErr != nil {
		return false, cpierrors.Wrap(pve.WrapError(destroyErr),
			fmt.Sprintf("stemcell-refs: destroy unreferenced template vmid %d node %q", vmid, node))
	}
	return false, nil
}

// destroyUntrackedTemplateLocked frees one untracked template (--untracked).
// Under the per-VMID lock it re-reads the template and holds that read to
// every condition the plan chose it on, in this order:
//
//   - It is still a template that the scan would recognize as a stemcell
//     cache template, and untrackedBlock passes on it: no generation marker,
//     a stemcell cache name and provenance, and no ref.
//   - verify passes, so its name, description, and tags are the ones the
//     plan saw.
//   - PVE reports it as stopped.
//   - A fresh read of every guest in the cluster finds no linked clone of
//     it, and every one of those configs reads, within
//     untrackedCloneRescanTimeout.
//
// Only then does it stamp bosh-destroy-pending, in a write guarded by the
// first read's config digest, so a change made while the checks ran refuses
// the stamp. A template that already carries the stamp from an earlier apply
// keeps it, and the destroy resumes. destroy runs after the lock releases,
// the same envelope destroyUnreferencedTemplateLocked uses. A changed
// condition returns an error that wraps ErrStemcellRefsChanged, and nothing
// is written. A failure once the stamp may have landed names the rerun that
// resumes the destroy, built with configPath.
//
// A locked read that finds no config on act.Node proves nothing on its own,
// because a template migrated since the plan answers the same way, so gone is
// reported only after untrackedConfirmGone finds the VMID nowhere in the
// cluster.
func destroyUntrackedTemplateLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	act StemcellRefsAction,
	configPath string,
	verify func(cfg map[string]any) error,
	destroy func(context.Context) error,
) (gone bool, err error) {
	vmidInt := int(act.VMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	owner := fmt.Sprintf("stemcell/director_ref/prune_untracked/%d", act.VMID)
	lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmidInt, owner, logger, func() error {
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, act.Node, vmidInt)
		if cfgErr != nil {
			if stemcellRefsGone(cfgErr) {
				gone = true
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("stemcell-refs: read config vmid %d node %q", act.VMID, act.Node))
		}
		digest, ok := pve.ConfigString(cfg, "digest")
		if !ok || digest == "" {
			return cpierrors.Cloud("stemcell-refs: the config read of vmid %d carries no digest, so nothing "+
				"was written", act.VMID)
		}
		if recheckErr := untrackedRecheck(act, cfg); recheckErr != nil {
			return recheckErr
		}
		if verifyErr := verify(cfg); verifyErr != nil {
			return verifyErr
		}
		if statusErr := untrackedStoppedCheck(ctx, deps, act.Node, vmidInt); statusErr != nil {
			return statusErr
		}
		if cloneErr := untrackedCloneRecheck(ctx, deps, logger, act.VMID); cloneErr != nil {
			return cloneErr
		}
		tags := parseTagsField(stringConfigField(cfg, jsonKeyTags))
		stampErr := stampDestroyPendingTagGuarded(ctx, deps, act.Node, act.VMID, tags, digest)
		if pve.IsConfigDigestStale(stampErr) {
			return fmt.Errorf("%w: its config changed between the locked read and the %s stamp",
				ErrStemcellRefsChanged, stemcellDestroyPendingTag)
		}
		if stampErr != nil {
			// The write may have landed even though it reported an error.
			return cpierrors.Wrap(stampErr, "stemcell-refs: if the stamp landed, "+
				untrackedResumeNote(configPath, act.VMID))
		}
		return nil
	})
	if lockErr == nil && gone {
		return untrackedConfirmGone(ctx, deps, logger, act, configPath)
	}
	if lockErr != nil {
		return false, lockErr
	}
	if destroyErr := destroy(ctx); destroyErr != nil {
		if errors.Is(destroyErr, ErrStemcellRefsChanged) || pve.IsBaseVolumeInUse(destroyErr) {
			// A removed stamp means nothing is pending, and the base-volume
			// answer gets its own message from the caller.
			return false, cpierrors.Wrap(pve.WrapError(destroyErr),
				fmt.Sprintf("stemcell-refs: destroy untracked template vmid %d node %q", act.VMID, act.Node))
		}
		return false, cpierrors.Wrap(pve.WrapError(destroyErr),
			fmt.Sprintf("stemcell-refs: destroy untracked template vmid %d node %q failed; %s", act.VMID,
				act.Node, untrackedResumeNote(configPath, act.VMID)))
	}
	return false, nil
}

// untrackedResumeNote says that the untracked template vmid carries
// bosh-destroy-pending and names the exact rerun that resumes its destroy.
func untrackedResumeNote(configPath string, vmid int64) string {
	return fmt.Sprintf("the template now carries %s, and rerunning %s resumes the destroy once the cause is "+
		"fixed", stemcellDestroyPendingTag, stemcellRefsPruneCommand(configPath, vmid, true, true))
}

// untrackedConfirmGone decides the outcome when the locked read found no
// config for act's template on act.Node. The template counts as gone only
// when the strict cluster listing no longer has its VMID. When the listing
// still has it, on any node, the template moved or is mid-migration since the
// plan, so the result wraps ErrStemcellRefsChanged with the node it is on and
// the rerun that plans it there. A listing that fails proves nothing either
// way and is returned as an error.
func untrackedConfirmGone(
	ctx context.Context, deps Deps, logger *log.Logger, act StemcellRefsAction, configPath string,
) (bool, error) {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, logger)
	if err != nil {
		return false, cpierrors.Wrap(err, fmt.Sprintf("stemcell-refs: vmid %d has no config on node %q, and the "+
			"cluster listing that would prove it gone failed", act.VMID, act.Node))
	}
	for _, g := range guests {
		if int64(g.VMID) == act.VMID {
			return false, fmt.Errorf("%w: vmid %d is no longer on node %s but the cluster lists it on node %s; "+
				"rerun %s to plan it where it is now", ErrStemcellRefsChanged, act.VMID, act.Node, g.Node,
				stemcellRefsPruneCommand(configPath, act.VMID, true, false))
		}
	}
	return true, nil
}

// untrackedRecheck holds a locked config read of act's template to the
// conditions untrackedBlock checks on the scan, and to the template flag and
// the scan's recognition of a stemcell cache template.
func untrackedRecheck(act StemcellRefsAction, cfg map[string]any) error {
	if !pve.ConfigFlag(cfg, pveConfigKeyTemplate) {
		return fmt.Errorf("%w: the guest is no longer a template", ErrStemcellRefsChanged)
	}
	g := pve.GuestRef{VMID: int(act.VMID), Node: act.Node, Name: act.Name, Template: true} //nolint:gosec // VMID is bounded by PVE valid range
	t, recognized := scanStemcellTemplate(g, cfg, nil)
	if !recognized {
		return fmt.Errorf("%w: it is no longer a recognizable stemcell cache template", ErrStemcellRefsChanged)
	}
	if reason := untrackedBlock(&t); reason != "" {
		return fmt.Errorf("%w: %s", ErrStemcellRefsChanged, reason)
	}
	return nil
}

// untrackedStoppedCheck reads the template's status and refuses anything but
// stopped.
func untrackedStoppedCheck(ctx context.Context, deps Deps, node string, vmid int) error {
	st, err := deps.PVE.QEMU().Status(ctx, node, vmid)
	if err != nil {
		return cpierrors.Wrap(pve.WrapError(err),
			fmt.Sprintf("stemcell-refs: read status vmid %d node %q", vmid, node))
	}
	if state, _ := pve.ConfigString(st, "status"); state != vmPowerStateStopped {
		return fmt.Errorf("%w: PVE reports its status as %q, not %s", ErrStemcellRefsChanged, state,
			vmPowerStateStopped)
	}
	return nil
}

// untrackedCloneRecheck lists every guest in the cluster again, through the
// strict enumeration, and reads each one's config for a disk based on
// template vmid, all within untrackedCloneRescanTimeout; a rescan the
// deadline cuts off is an error. A clone it finds wraps ErrStemcellRefsChanged. A listing or
// config read that fails is an error, because it leaves the absence of a
// clone unproven; a guest destroyed between the listing and its read is
// skipped, as the scan skips it.
func untrackedCloneRecheck(ctx context.Context, deps Deps, logger *log.Logger, vmid int64) error {
	rctx, cancel := context.WithTimeout(ctx, untrackedCloneRescanTimeout)
	defer cancel()
	err := untrackedCloneRescan(rctx, deps, logger, vmid)
	if err != nil && ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return cpierrors.Cloud("stemcell-refs: the linked-clone check did not finish within %s, so no destroy "+
			"is safe; nothing was written: %v", untrackedCloneRescanTimeout, err)
	}
	return err
}

// untrackedCloneRescanTimeout bounds untrackedCloneRecheck, which runs under
// the VMID lock with one config read per guest in the cluster. It is counted
// in vmidLockBodies and stays well inside the lock's TTL. It is a variable
// only so a test can shorten it.
var untrackedCloneRescanTimeout = 10 * time.Minute

// untrackedCloneRescan is untrackedCloneRecheck's scan, run under its
// deadline.
func untrackedCloneRescan(ctx context.Context, deps Deps, logger *log.Logger, vmid int64) error {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, logger)
	if err != nil {
		return cpierrors.Wrap(err, "stemcell-refs: list cluster guests for the linked-clone check")
	}
	var clones []int64
	for _, g := range guests {
		if g.VMID <= 0 || int64(g.VMID) == vmid {
			continue
		}
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, g.Node, g.VMID)
		if cfgErr != nil {
			if stemcellRefsGone(cfgErr) {
				continue
			}
			return cpierrors.Wrap(pve.WrapError(cfgErr), fmt.Sprintf("stemcell-refs: the linked-clone check "+
				"could not read the config of vmid %d on node %s, so no destroy is safe", g.VMID, g.Node))
		}
		if slices.Contains(linkedCloneBases(cfg, int64(g.VMID)), vmid) {
			clones = append(clones, int64(g.VMID))
		}
	}
	if len(clones) > 0 {
		sort.Slice(clones, func(i, j int) bool { return clones[i] < clones[j] })
		return fmt.Errorf("%w: template now backs %d linked clone(s) (%s)", ErrStemcellRefsChanged, len(clones),
			joinVMIDs(clones))
	}
	return nil
}

// MeasureStemcellRefsPlan reads the size of every volume that a
// destroy-untracked action in plan would free, through the PVE storage
// content read the CPI uses for volume sizes elsewhere, and fills each
// action's total and the plan's. A size it cannot read is left null with the
// reason and counted in SizesUnknown, so a failed read never fails the run.
func MeasureStemcellRefsPlan(ctx context.Context, deps Deps, plan *StemcellRefsPlan) {
	plan.TotalBytes, plan.SizesUnknown = 0, 0
	for i := range plan.Actions {
		f := plan.Actions[i].Frees
		if f == nil {
			continue
		}
		f.TotalBytes, f.SizesUnknown = 0, 0
		for j := range f.Volumes {
			v := &f.Volumes[j]
			size, err := stemcellRefsVolumeSize(ctx, deps, plan.Actions[i].Node, v.Volume)
			if err != nil {
				v.SizeBytes = nil
				v.SizeError = err.Error()
				f.SizesUnknown++
				continue
			}
			v.SizeBytes = &size
			v.SizeError = ""
			f.TotalBytes += size
		}
		plan.TotalBytes += f.TotalBytes
		plan.SizesUnknown += f.SizesUnknown
	}
}

// stemcellRefsVolumeSize returns the size in bytes PVE reports for volid on
// node.
func stemcellRefsVolumeSize(ctx context.Context, deps Deps, node, volid string) (int64, error) {
	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return 0, errors.New("PVE client unavailable")
	}
	storage, name, err := pve.ParseDiskCID(volid)
	if err != nil {
		return 0, err
	}
	info, err := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, name)
	if err != nil {
		return 0, fmt.Errorf("storage content read failed: %w", err)
	}
	if info == nil || info.Size.Int() <= 0 {
		return 0, errors.New("storage content read reported no size")
	}
	return info.Size.Int(), nil
}

// StemcellRefsRefSummary is one distinct ref in the report.
type StemcellRefsRefSummary struct {
	Ref         string `json:"ref"`
	DirectorTag string `json:"director_tag"`
	OwnerTag    string `json:"owner_tag"`
	// DescriptionTemplates hold the ref in their description, matched by
	// trimmed equality.
	DescriptionTemplates []int64 `json:"description_templates"`
	// TagOnlyTemplates carry the ref's tag without holding the ref.
	TagOnlyTemplates []int64 `json:"tag_only_templates"`
	// OwnedVMCount is null when ownership cannot be determined for the ref,
	// as for the unknown-director sentinel, whose VMs carry no owner tag, or
	// for any ref while some BOSH guest carries no owner tag.
	OwnedVMCount     *int    `json:"owned_vm_count"`
	OwnershipUnknown bool    `json:"ownership_unknown"`
	OwnedVMs         []int64 `json:"owned_vms"`
	// OwnershipUnknownReason explains OwnershipUnknown.
	OwnershipUnknownReason string `json:"ownership_unknown_reason,omitempty"`
	// WouldFree lists the templates a prune of this ref would destroy, as
	// if --allow-owned-vms were passed. It is empty when PruneBlocked is set.
	WouldFree []int64 `json:"would_free"`
	// PruneBlocked is true when a prune of this ref is refused even with
	// --allow-owned-vms, because some BOSH guest carries no owner tag.
	PruneBlocked bool `json:"prune_blocked"`
	// Blocked lists the templates a prune would leave because of a safety
	// rule.
	Blocked []int64 `json:"blocked"`
	// KeptReplicas lists the replicas a prune would leave in place holding
	// no ref, with the reason and whether --vmid could free them afterwards.
	KeptReplicas []StemcellRefsKeptReplica `json:"kept_replicas"`
	// Note flags a ref the prune cannot address, such as a blank one.
	Note string `json:"note,omitempty"`
}

// StemcellRefsUnreferenced is a template a --vmid prune could free, or a
// zero-ref or refs-unknown template it would refuse, with the planner's
// reason. Every untracked template appears too, judged by the --untracked
// planner.
type StemcellRefsUnreferenced struct {
	VMID        int64  `json:"vmid"`
	Node        string `json:"node"`
	Name        string `json:"name"`
	Legacy      bool   `json:"legacy"`
	Replica     bool   `json:"replica"`
	RefsUnknown bool   `json:"refs_unknown"`
	Untracked   bool   `json:"untracked"`
	Freeable    bool   `json:"freeable"`
	Reason      string `json:"reason,omitempty"`
	// FreeCommand is the prune that frees the template, set only when
	// Freeable is true.
	FreeCommand string `json:"free_command,omitempty"`
	// BlockingClones lists the linked clones that keep the template from
	// being freed.
	BlockingClones []int64 `json:"blocking_clones,omitempty"`
}

// StemcellRefsReport is the read-only report.
type StemcellRefsReport struct {
	Templates    []StemcellRefsTemplate     `json:"templates"`
	Refs         []StemcellRefsRefSummary   `json:"refs"`
	Unreferenced []StemcellRefsUnreferenced `json:"unreferenced"`
	// DescriptionsNearLimit counts the templates whose description is within
	// 1024 bytes of PVE's 8192-byte cap (DescriptionNearLimit).
	DescriptionsNearLimit int      `json:"descriptions_near_limit"`
	CloneScanComplete     bool     `json:"clone_scan_complete"`
	ScanErrors            []string `json:"scan_errors,omitempty"`
	// OwnershipUnknown is true when some BOSH guest carries no owner tag, so
	// ownership is unknown for every ref and every director prune is refused.
	OwnershipUnknown       bool   `json:"ownership_unknown"`
	OwnershipUnknownReason string `json:"ownership_unknown_reason,omitempty"`
	// Uncovered groups those guests by director name and deployment, with
	// the adopt-owners commands that cover each group.
	Uncovered []StemcellRefsUncoveredGroup `json:"uncovered"`
}

// BuildStemcellRefsReport summarizes scan. Each ref's WouldFree and Blocked
// columns come from PlanStemcellRefsPrune, and each unreferenced template's
// verdict from the --vmid planner, so the report never predicts something
// the prune would not do.
func BuildStemcellRefsReport(scan StemcellRefsScan) StemcellRefsReport {
	return BuildStemcellRefsReportFor(scan, "")
}

// BuildStemcellRefsReportFor is BuildStemcellRefsReport with the --config
// path the report ran with, which each FreeCommand carries so it runs as
// printed. An empty configPath prints the <cpi.json> placeholder.
func BuildStemcellRefsReportFor(scan StemcellRefsScan, configPath string) StemcellRefsReport {
	rep := StemcellRefsReport{
		Templates:         scan.Templates,
		Refs:              []StemcellRefsRefSummary{},
		Unreferenced:      []StemcellRefsUnreferenced{},
		CloneScanComplete: scan.CloneScanComplete,
		ScanErrors:        scan.ScanErrors,
		Uncovered:         []StemcellRefsUncoveredGroup{},
	}
	if rep.Templates == nil {
		rep.Templates = []StemcellRefsTemplate{}
	}
	if len(scan.Uncovered) > 0 {
		rep.OwnershipUnknown = true
		rep.OwnershipUnknownReason = stemcellRefsCoverageReason(scan.Uncovered)
		rep.Uncovered = scan.Uncovered
	}
	for _, ref := range stemcellRefNames(scan) {
		rep.Refs = append(rep.Refs, summarizeStemcellRef(scan, ref))
	}
	for i := range scan.Templates {
		if u, ok := unreferencedStemcellTemplate(scan, &scan.Templates[i], configPath); ok {
			rep.Unreferenced = append(rep.Unreferenced, u)
		}
		if scan.Templates[i].DescriptionNearLimit {
			rep.DescriptionsNearLimit++
		}
	}
	return rep
}

// stemcellRefNames returns every distinct ref in scan, trimmed and sorted. A
// blank ref is listed as "" so the report can flag it. A tag-only ref is
// listed by its tag value unless some description ref maps to the same tag.
func stemcellRefNames(scan StemcellRefsScan) []string {
	refs := map[string]bool{}
	descTagValues := map[string]bool{}
	for i := range scan.Templates {
		for _, r := range scan.Templates[i].DescriptionRefs {
			r = strings.TrimSpace(r)
			refs[r] = true
			descTagValues[strings.ToLower(sanitizeTagValue(r))] = true
		}
	}
	for i := range scan.Templates {
		for _, tok := range scan.Templates[i].DirectorTags {
			v := strings.TrimPrefix(tok, directorTagPrefix)
			if v != "" && !descTagValues[strings.ToLower(v)] {
				refs[v] = true
			}
		}
	}
	names := make([]string, 0, len(refs))
	for r := range refs {
		names = append(names, r)
	}
	sort.Strings(names)
	return names
}

// summarizeStemcellRef builds the report row for ref from the same planner
// the prune uses.
func summarizeStemcellRef(scan StemcellRefsScan, ref string) StemcellRefsRefSummary {
	s := StemcellRefsRefSummary{
		Ref: ref, DescriptionTemplates: []int64{}, TagOnlyTemplates: []int64{},
		OwnedVMs: []int64{}, WouldFree: []int64{}, Blocked: []int64{}, KeptReplicas: []StemcellRefsKeptReplica{},
	}
	if ref != "" {
		s.DirectorTag = directorTagFor(ref)
		s.OwnerTag = ownerTagFor(ref)
	}
	for i := range scan.Templates {
		t := &scan.Templates[i]
		switch {
		case len(storedRefSpellings(t.DescriptionRefs, ref)) > 0:
			s.DescriptionTemplates = append(s.DescriptionTemplates, t.VMID)
		case s.DirectorTag != "" && matchingDirectorTag(t.Tags, s.DirectorTag) != "":
			s.TagOnlyTemplates = append(s.TagOnlyTemplates, t.VMID)
		}
	}
	if ref == "" {
		s.OwnershipUnknown = true
		s.Note = "blank ref: the prune cannot address it, so these templates stay until it is removed by hand"
		return s
	}
	plan, err := PlanStemcellRefsPrune(scan, StemcellRefsPruneRequest{DirectorRef: ref, AllowOwnedVMs: true})
	if err != nil {
		return s
	}
	for i := range plan.OwnedVMs {
		s.OwnedVMs = append(s.OwnedVMs, plan.OwnedVMs[i].VMID)
	}
	s.OwnershipUnknown = plan.OwnershipUnknown
	s.OwnershipUnknownReason = plan.OwnershipUnknownReason
	if !s.OwnershipUnknown {
		n := len(s.OwnedVMs)
		s.OwnedVMCount = &n
	}
	// A prune that is refused even with --allow-owned-vms changes nothing,
	// so the report lists no template as freed, blocked, or kept by it.
	s.PruneBlocked = plan.CoverageIncomplete
	if s.PruneBlocked {
		return s
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		switch {
		case a.destroys():
			s.WouldFree = append(s.WouldFree, a.VMID)
			for _, r := range a.SweepReplicas {
				s.WouldFree = append(s.WouldFree, r.VMID)
			}
		case a.Failure:
			s.Blocked = append(s.Blocked, a.VMID)
		case a.Kind == StemcellRefsActionRemoveRef && a.Replica && len(a.RemainingRefs) == 0:
			s.KeptReplicas = append(s.KeptReplicas,
				StemcellRefsKeptReplica{VMID: a.VMID, Node: a.Node, Name: a.Name, Reason: a.Reason})
		}
		s.KeptReplicas = append(s.KeptReplicas, a.KeptReplicas...)
	}
	return s
}

// unreferencedStemcellTemplate returns t's unreferenced-table row with the
// --vmid planner's verdict. A template appears when it provably holds no ref,
// when its blank description leaves its refs unknown (never freeable), or
// when the planner would free it.
func unreferencedStemcellTemplate(
	scan StemcellRefsScan, t *StemcellRefsTemplate, configPath string,
) (StemcellRefsUnreferenced, bool) {
	if t.Untracked {
		return untrackedStemcellTemplate(scan, t, configPath)
	}
	candidate := refsKnownZero(t) || (t.RefsUnknown && t.DescriptionState == stemcellRefsDescBlank)
	plan, err := PlanStemcellRefsPrune(scan, StemcellRefsPruneRequest{VMID: t.VMID})
	if err != nil {
		return StemcellRefsUnreferenced{}, false
	}
	freeable := !plan.Refused && len(plan.Actions) == 1 && plan.Actions[0].Kind == StemcellRefsActionDestroy
	if !candidate && !freeable {
		return StemcellRefsUnreferenced{}, false
	}
	u := StemcellRefsUnreferenced{
		VMID: t.VMID, Node: t.Node, Name: t.Name, Legacy: t.Legacy, Replica: t.Replica,
		RefsUnknown: t.RefsUnknown, Untracked: t.Untracked, Freeable: freeable,
	}
	if freeable {
		u.FreeCommand = stemcellRefsPruneCommand(configPath, t.VMID, false, false)
	} else {
		u.Reason = plan.RefusalReason
		u.BlockingClones = t.LinkedClones
	}
	return u, true
}

// untrackedStemcellTemplate returns the unreferenced-table row of untracked
// template t with the --untracked planner's verdict. Every untracked template
// gets a row, so the report names either the prune that frees it or what
// stops that prune, such as the refs it holds or the linked clones it backs.
func untrackedStemcellTemplate(
	scan StemcellRefsScan, t *StemcellRefsTemplate, configPath string,
) (StemcellRefsUnreferenced, bool) {
	plan, err := PlanStemcellRefsPrune(scan, StemcellRefsPruneRequest{VMID: t.VMID, Untracked: true})
	if err != nil {
		return StemcellRefsUnreferenced{}, false
	}
	freeable := !plan.Refused && len(plan.Actions) == 1 &&
		plan.Actions[0].Kind == StemcellRefsActionDestroyUntracked
	u := StemcellRefsUnreferenced{
		VMID: t.VMID, Node: t.Node, Name: t.Name, Legacy: t.Legacy, Replica: t.Replica,
		RefsUnknown: t.RefsUnknown, Untracked: true, Freeable: freeable,
	}
	if freeable {
		u.FreeCommand = stemcellRefsPruneCommand(configPath, t.VMID, true, false)
	} else {
		u.Reason = plan.RefusalReason
		u.BlockingClones = t.LinkedClones
	}
	return u, true
}

// stemcellRefsPruneCommand is the prune --vmid command for vmid, with
// --untracked and --apply when asked, and the --config path shell-quoted, or
// the <cpi.json> placeholder when configPath is "". It is the dry run unless
// apply is set, so a printed command never changes PVE by itself.
func stemcellRefsPruneCommand(configPath string, vmid int64, untracked, apply bool) string {
	cfg := stemcellRefsConfigPath
	if configPath != "" {
		cfg = shellQuoteArg(configPath)
	}
	c := fmt.Sprintf("cpi stemcell-refs prune --config %s --vmid %d", cfg, vmid)
	if untracked {
		c += " --untracked"
	}
	if apply {
		c += " --apply"
	}
	return c
}

// shellSafeArg matches an argument a POSIX shell passes through unchanged.
var shellSafeArg = regexp.MustCompile(`^[A-Za-z0-9_./:=+,@%-]+$`)

// shellQuoteArg returns s as one POSIX shell word: unchanged when no
// character in it is special, and single-quoted otherwise, with each single
// quote in it closed, escaped with a backslash, and reopened.
func shellQuoteArg(s string) string {
	if shellSafeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// joinVMIDs renders VMIDs as a comma-separated list.
func joinVMIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}
