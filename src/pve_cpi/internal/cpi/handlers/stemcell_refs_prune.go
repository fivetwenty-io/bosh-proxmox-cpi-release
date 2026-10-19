package handlers

// stemcell_refs_prune.go: the operator side of the director-ref set (D3).
//
// A rebuilt director gets a new UUID, and its old UUID stays in director_refs
// on every stemcell cache template it registered. No CPI call ever removes
// it, so those templates can never be freed. `cpi stemcell-refs` reports the
// refs on every cache template and prunes one named ref, or frees one
// unreferenced template, on an operator's say-so.
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
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// Action kinds a StemcellRefsPlan can hold. The first five mutate PVE on
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
	// StemcellRefsActionDestroy destroys one unreferenced template (--vmid).
	StemcellRefsActionDestroy = "destroy"
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
	// DirectorTags is every director-- tag, which is write-only history.
	DirectorTags []string `json:"director_tags"`
	// RefsTagsDisagree is true when the sanitized description refs and the
	// director tag values are not the same set.
	RefsTagsDisagree bool `json:"refs_tags_disagree"`
	// DescriptionState is provenance, blank, unparseable, or unread.
	DescriptionState string `json:"description_state"`
	// Replica is true for per-node and per-storage replicas, whose refs are
	// a fossil of their creation.
	Replica        bool `json:"replica"`
	DestroyPending bool `json:"destroy_pending"`
	// Legacy marks a template from before 0.9.2: the cache naming pattern, a
	// blank description, and no generation marker. Only a legacy template's
	// blank description counts as zero refs.
	Legacy bool `json:"legacy"`
	// RefsUnknown is true when the description cannot tell us the ref set:
	// it was unread, it does not parse, or it is blank on a template that is
	// not legacy. Such a template is never changed.
	RefsUnknown bool `json:"refs_unknown"`
	// Untracked marks a provenance template with no generation marker. It
	// comes from a CPI generation that never recorded refs, so an empty ref
	// set on it proves nothing about who uses it.
	Untracked bool `json:"untracked"`
	// LinkedClones lists the guests whose disks name this template's base
	// volume as their parent.
	LinkedClones []int64 `json:"linked_clones"`
	ReadError    string  `json:"read_error,omitempty"`

	// description is the raw description the scan read, kept so the apply
	// can prove the template has not changed since the plan.
	description string
}

// StemcellRefsGuest is one non-template guest and its owner tags.
type StemcellRefsGuest struct {
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// OwnerTags holds the guest's bosh-owner-- tags, lowercased.
	OwnerTags []string `json:"owner_tags,omitempty"`
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
// own config could not be read is kept with ReadError set.
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
			scan.Guests = append(scan.Guests, StemcellRefsGuest{
				VMID: int64(g.VMID), Node: g.Node, Name: g.Name, OwnerTags: ownerTagsOf(g.SplitTags()),
			})
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
	return scan, nil
}

// scanStemcellTemplate builds the scan record for one template and reports
// whether it is recognizably a stemcell cache template.
func scanStemcellTemplate(g pve.GuestRef, cfg map[string]any, cfgErr error) (StemcellRefsTemplate, bool) {
	t := StemcellRefsTemplate{VMID: int64(g.VMID), Node: g.Node, Name: g.Name}
	var prov stemcellProvenance
	provOK := false
	if cfgErr != nil && cfg == nil {
		t.Tags = g.SplitTags()
		t.DescriptionState = stemcellRefsDescUnread
		t.ReadError = cfgErr.Error()
	} else {
		t.Tags = parseTagsField(stringConfigField(cfg, jsonKeyTags))
		t.description = stringConfigField(cfg, pveConfigKeyDescription)
		if name := stringConfigField(cfg, pveConfigKeyName); name != "" {
			t.Name = name
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

// ownerTagsOf returns the bosh-owner-- tags among tokens, lowercased.
func ownerTagsOf(tokens []string) []string {
	var out []string
	for _, tok := range tokens {
		if strings.HasPrefix(strings.ToLower(tok), ownerTagPrefix) {
			out = append(out, strings.ToLower(tok))
		}
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
// DirectorRef and VMID is set.
type StemcellRefsPruneRequest struct {
	// DirectorRef is trimmed, then matched against description refs by
	// trimmed equality, and through directorTagFor against tags.
	DirectorRef string
	// VMID names one unreferenced template to free.
	VMID int64
	// AllowOwnedVMs lets a DirectorRef prune proceed while non-template
	// guests still carry the ref's owner tag, or when ownership cannot be
	// determined at all, as for the unknown-director sentinel.
	AllowOwnedVMs bool
}

// StemcellRefsTemplateRef names a template inside a plan action.
type StemcellRefsTemplateRef struct {
	VMID int64    `json:"vmid"`
	Node string   `json:"node"`
	Name string   `json:"name"`
	Tags []string `json:"-"`

	// description is the scanned description, so the sweep can prove the
	// replica still holds no ref when it destroys it.
	description string
}

// StemcellRefsKeptReplica is a replica a destroy leaves in place, with the
// reason it is not swept.
type StemcellRefsKeptReplica struct {
	VMID   int64  `json:"vmid"`
	Node   string `json:"node"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// StemcellRefsAction is one planned step on one template.
type StemcellRefsAction struct {
	Kind string `json:"kind"`
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	SHA8 string `json:"sha8,omitempty"`
	// Reason explains a blocked or skipped action, or a tag the plan keeps.
	Reason string `json:"reason,omitempty"`
	// MatchedRef is the ref exactly as the description stores it, which can
	// differ from the requested ref by surrounding space.
	MatchedRef string `json:"matched_ref,omitempty"`
	// RemainingRefs is the ref set a remove-ref leaves behind.
	RemainingRefs []string `json:"remaining_refs,omitempty"`
	// DropTag is the director tag the action removes.
	DropTag string `json:"drop_tag,omitempty"`
	// SweepReplicas lists the replicas destroyed after this template, when
	// it is the last non-replica template of its sha8. Each one passes the
	// --vmid preconditions on its own: zero refs, no linked clones.
	SweepReplicas []StemcellRefsTemplateRef `json:"sweep_replicas,omitempty"`
	// KeptReplicas lists the replicas of that sha8 the destroy leaves in
	// place, such as a replica that holds refs of its own.
	KeptReplicas []StemcellRefsKeptReplica `json:"kept_replicas,omitempty"`
	LinkedClones []int64                   `json:"linked_clones,omitempty"`
	// Failure is true for a blocked or skipped action that makes the
	// command exit 1.
	Failure bool `json:"failure"`

	description string
	tags        []string
	// planned is set by the planner, so the apply refuses a mutating action
	// it did not produce, such as one decoded from JSON.
	planned bool
}

// Mutating reports whether applying the action changes PVE.
func (a StemcellRefsAction) Mutating() bool {
	switch a.Kind {
	case StemcellRefsActionRemoveRef, StemcellRefsActionRemoveRefAndDestroy,
		StemcellRefsActionResumeDestroy, StemcellRefsActionRemoveTag, StemcellRefsActionDestroy:
		return true
	}
	return false
}

// destroys reports whether applying the action destroys its template.
func (a StemcellRefsAction) destroys() bool {
	switch a.Kind {
	case StemcellRefsActionRemoveRefAndDestroy, StemcellRefsActionResumeDestroy, StemcellRefsActionDestroy:
		return true
	}
	return false
}

// StemcellRefsPlan is what a prune will do. The dry run prints it and the
// apply executes it unchanged.
type StemcellRefsPlan struct {
	// Mode is "director" or "vmid".
	Mode        string `json:"mode"`
	DirectorRef string `json:"director_ref,omitempty"`
	DirectorTag string `json:"director_tag,omitempty"`
	OwnerTag    string `json:"owner_tag,omitempty"`
	VMID        int64  `json:"vmid,omitempty"`
	// OwnedVMs lists the non-template guests carrying the ref's owner tag.
	OwnedVMs []StemcellRefsGuest `json:"owned_vms,omitempty"`
	// OwnershipUnknown is true when no owner tag can exist for the ref, as
	// for the unknown-director sentinel: create_vm stamps no owner tag on a
	// VM made without a director UUID, so OwnedVMs cannot see those VMs.
	OwnershipUnknown bool `json:"ownership_unknown"`
	// Refused is true when the plan must not be applied at all.
	Refused       bool                 `json:"refused"`
	RefusalReason string               `json:"refusal_reason,omitempty"`
	Actions       []StemcellRefsAction `json:"actions"`
	// CloneScanComplete and ScanErrors are carried from the scan.
	CloneScanComplete bool     `json:"clone_scan_complete"`
	ScanErrors        []string `json:"scan_errors,omitempty"`
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
	switch {
	case ref != "" && req.VMID != 0:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: name a director ref or a VMID, not both")
	case ref == "" && req.VMID <= 0:
		return StemcellRefsPlan{}, cpierrors.Cloud("stemcell-refs: name a non-empty director ref or a positive VMID")
	}
	plan := StemcellRefsPlan{
		Actions:           []StemcellRefsAction{},
		CloneScanComplete: scan.CloneScanComplete,
		ScanErrors:        scan.ScanErrors,
	}
	if ref != "" {
		planDirectorPrune(scan, ref, req.AllowOwnedVMs, &plan)
	} else {
		planVMIDPrune(scan, req.VMID, &plan)
	}
	attachReplicaSweeps(scan, &plan)
	return plan, nil
}

// newStemcellRefsAction starts an action for template t.
func newStemcellRefsAction(kind string, t *StemcellRefsTemplate) StemcellRefsAction {
	return StemcellRefsAction{
		Kind: kind, VMID: t.VMID, Node: t.Node, Name: t.Name, SHA8: t.SHA8,
		description: t.description, tags: append([]string(nil), t.Tags...), planned: true,
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

// planDestroy fills a destroy-kind action, or turns it into a blocked one
// when the clone check fails.
func planDestroy(scan StemcellRefsScan, kind string, t *StemcellRefsTemplate) StemcellRefsAction {
	act := newStemcellRefsAction(kind, t)
	if reason := cloneBlock(scan, t); reason != "" {
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
	for i := range scan.Templates {
		if act, ok := planDirectorTemplate(scan, &scan.Templates[i], ref, plan.DirectorTag); ok {
			plan.Actions = append(plan.Actions, act)
		}
	}
}

// planDirectorOwnership fills the owned-VM guard. It refuses the plan, unless
// allowOwned is set, when non-template guests carry the ref's owner tag, or
// when no owner tag can exist for the ref at all. The second case covers the
// unknown-director sentinel: create_vm stamps no owner tag on a VM made
// without a director UUID, so an owned count of zero would prove nothing.
func planDirectorOwnership(scan StemcellRefsScan, ref string, allowOwned bool, plan *StemcellRefsPlan) {
	if plan.OwnerTag != "" {
		for _, g := range scan.Guests {
			if stringSliceContains(g.OwnerTags, plan.OwnerTag) {
				plan.OwnedVMs = append(plan.OwnedVMs, g)
			}
		}
	}
	plan.OwnershipUnknown = ref == unknownDirectorRef || plan.OwnerTag == ""
	if allowOwned {
		return
	}
	switch {
	case plan.OwnershipUnknown:
		plan.Refused = true
		plan.RefusalReason = fmt.Sprintf(
			"ownership cannot be determined for %s: VMs created without a director UUID carry no owner tag, "+
				"so the owned-VM check cannot see the callers that use this ref. Pass --allow-owned-vms only "+
				"when we know no such caller still deploys from these templates", ref)
	case len(plan.OwnedVMs) > 0:
		ids := make([]int64, 0, len(plan.OwnedVMs))
		for _, g := range plan.OwnedVMs {
			ids = append(ids, g.VMID)
		}
		plan.Refused = true
		plan.RefusalReason = fmt.Sprintf(
			"%d non-template VM(s) still carry the owner tag %s (%s); the director may still be alive. "+
				"Pass --allow-owned-vms only when we know those VMs are orphans",
			len(ids), plan.OwnerTag, joinVMIDs(ids))
	}
}

// stemcellRefsTagKept explains a director tag the plan keeps.
const stemcellRefsTagKept = "kept: another ref maps to it, or it is the template's only generation marker"

// skipStemcellRefsAction is a skip on t with reason.
func skipStemcellRefsAction(t *StemcellRefsTemplate, reason string, failure bool) StemcellRefsAction {
	act := newStemcellRefsAction(StemcellRefsActionSkip, t)
	act.Reason = reason
	act.Failure = failure
	return act
}

// planDirectorTemplate plans one template for a director prune, or reports
// false when the template neither holds the ref nor carries its tag.
func planDirectorTemplate(scan StemcellRefsScan, t *StemcellRefsTemplate, ref, dirTag string) (StemcellRefsAction, bool) {
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
	case t.Replica:
		if !inDesc && tagTok == "" {
			return StemcellRefsAction{}, false
		}
		return skipStemcellRefsAction(t, "replica that holds the ref or its tag; the prune never edits a replica, "+
			"and never destroys a replica that holds refs of its own", false), true
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
		return planDirectorPending(scan, t, spellings, tagTok)
	case inDesc:
		return planDirectorRemoveRef(scan, t, spellings[0], tagTok), true
	case tagTok != "":
		if _, dropped := dropDirectorTag(t.Tags, tagTok, t.DescriptionRefs); dropped {
			act := newStemcellRefsAction(StemcellRefsActionRemoveTag, t)
			act.DropTag = tagTok
			return act, true
		}
		return skipStemcellRefsAction(t, "tag "+tagTok+" "+stemcellRefsTagKept, false), true
	}
	return StemcellRefsAction{}, false
}

// planDirectorPending plans a template already marked bosh-destroy-pending.
func planDirectorPending(scan StemcellRefsScan, t *StemcellRefsTemplate, spellings []string, tagTok string) (StemcellRefsAction, bool) {
	switch {
	case len(spellings) == 1 && len(removeStringFromSlice(t.DescriptionRefs, spellings[0])) == 0:
		act := planDestroy(scan, StemcellRefsActionResumeDestroy, t)
		act.MatchedRef = spellings[0]
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
		if _, dropped := dropDirectorTag(t.Tags, tagTok, remaining); dropped {
			act.DropTag = tagTok
		} else {
			act.Reason = "tag " + tagTok + " " + stemcellRefsTagKept
		}
	}
	return act
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
// known sha8, and that t backs no linked clone. A replica swept with its
// anchor must pass the same check.
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
	return cloneBlock(scan, t)
}

func planVMIDPrune(scan StemcellRefsScan, vmid int64, plan *StemcellRefsPlan) {
	plan.Mode = "vmid"
	plan.VMID = vmid
	var target *StemcellRefsTemplate
	for i := range scan.Templates {
		if scan.Templates[i].VMID == vmid {
			target = &scan.Templates[i]
			break
		}
	}
	if target == nil {
		plan.Refused = true
		switch name, ok := scan.otherTemplates[vmid]; {
		case ok:
			plan.RefusalReason = fmt.Sprintf("VMID %d (%s) is a template, but not a recognizable stemcell cache template", vmid, name)
		case guestByVMID(scan, vmid) != nil:
			plan.RefusalReason = fmt.Sprintf("VMID %d is not a template", vmid)
		default:
			plan.RefusalReason = fmt.Sprintf("no guest with VMID %d was found in the cluster", vmid)
		}
		return
	}
	t := target
	act := newStemcellRefsAction(StemcellRefsActionDestroy, t)
	reason := zeroRefBlock(scan, t)
	if reason == "" && t.Replica {
		if anchor, ok := liveAnchorFor(scan, t.SHA8, t.VMID); ok {
			reason = fmt.Sprintf("replica of live template %d; it is freed with that template", anchor.VMID)
		}
	}
	if reason != "" {
		act.Kind = StemcellRefsActionBlocked
		act.Reason = reason
		act.Failure = true
		if len(t.LinkedClones) > 0 {
			act.LinkedClones = t.LinkedClones
		}
		plan.Refused = true
		plan.RefusalReason = reason
	}
	plan.Actions = append(plan.Actions, act)
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
// destroys carries the sweep. A replica is swept only when it passes the
// --vmid preconditions on its own (zeroRefBlock): it holds no ref, its
// description can be read and parsed, and it backs no linked clone. Every
// other replica is listed in KeptReplicas with its reason and left alone.
func attachReplicaSweeps(scan StemcellRefsScan, plan *StemcellRefsPlan) {
	destroying := map[int64]bool{}
	carrier := map[string]int{}
	for i := range plan.Actions {
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
		replicas, survivor := orphanedReplicas(scan, sha8, destroying)
		if survivor {
			continue
		}
		a := &plan.Actions[idx]
		for _, r := range replicas {
			if reason := zeroRefBlock(scan, r); reason != "" {
				a.KeptReplicas = append(a.KeptReplicas,
					StemcellRefsKeptReplica{VMID: r.VMID, Node: r.Node, Name: r.Name, Reason: reason})
				continue
			}
			a.SweepReplicas = append(a.SweepReplicas, StemcellRefsTemplateRef{
				VMID: r.VMID, Node: r.Node, Name: r.Name, Tags: append([]string(nil), r.Tags...), description: r.description,
			})
		}
	}
}

// orphanedReplicas returns the replicas of sha8 that the plan does not
// already destroy, and whether a non-replica template of sha8 survives it.
func orphanedReplicas(scan StemcellRefsScan, sha8 string, destroying map[int64]bool) ([]*StemcellRefsTemplate, bool) {
	var replicas []*StemcellRefsTemplate
	survivor := false
	for i := range scan.Templates {
		t := &scan.Templates[i]
		if t.SHA8 != sha8 || destroying[t.VMID] {
			continue
		}
		if t.Replica {
			replicas = append(replicas, t)
		} else {
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
	Action  StemcellRefsAction `json:"action"`
	Outcome string             `json:"outcome"`
	Error   string             `json:"error,omitempty"`
	// Destroyed lists every template the action destroyed, replicas
	// included.
	Destroyed     []int64  `json:"destroyed,omitempty"`
	RemainingRefs []string `json:"remaining_refs,omitempty"`
	TagRemoved    bool     `json:"tag_removed,omitempty"`
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
		if act.destroys() && act.SHA8 != "" && len(r.Destroyed) == 0 && r.Outcome != StemcellRefsOutcomeGone {
			sha8Unfinished[act.SHA8] = true
		}
		if r.Failure {
			res.Failures++
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

// pveFlagSet reads a PVE boolean config value, which arrives as a JSON
// number, a bool, or a string depending on the endpoint.
func pveFlagSet(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x == 1
	case int:
		return x == 1
	case int64:
		return x == 1
	case string:
		return x == "1" || x == "true"
	}
	return false
}

// stemcellRefsVerifier returns the check every locked apply step runs on its
// fresh config read. The guest must still be a template, keep the scanned
// name, and keep the scanned description and tag set; anything else is
// ErrStemcellRefsChanged and the step writes nothing.
func stemcellRefsVerifier(name, description string, tags []string) func(cfg map[string]any) error {
	return func(cfg map[string]any) error {
		cfgName := stringConfigField(cfg, pveConfigKeyName)
		switch {
		case !pveFlagSet(cfg[pveConfigKeyTemplate]):
			return fmt.Errorf("%w: the guest is no longer a template", ErrStemcellRefsChanged)
		case cfgName != "" && cfgName != name:
			return fmt.Errorf("%w: the guest is now named %q", ErrStemcellRefsChanged, cfgName)
		case stringConfigField(cfg, pveConfigKeyDescription) != description:
			return fmt.Errorf("%w: its description changed", ErrStemcellRefsChanged)
		case !sameTagSet(parseTagsField(stringConfigField(cfg, jsonKeyTags)), tags):
			return fmt.Errorf("%w: its tags changed", ErrStemcellRefsChanged)
		}
		return nil
	}
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
	r := StemcellRefsActionResult{Action: act}
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
	alog := logger.With(log.String("node", act.Node), log.Int64("vmid", act.VMID), log.String("kind", act.Kind))
	verify := stemcellRefsVerifier(act.Name, act.description, act.tags)
	ranDestroy := false
	destroy := func(ctx context.Context) error {
		ranDestroy = true
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
	switch act.Kind {
	case StemcellRefsActionRemoveRef:
		refuseDestroy := func(context.Context) error {
			return cpierrors.Cloud("stemcell-refs: the plan keeps template %d, so it is not destroyed", act.VMID)
		}
		opts.dropTag = act.DropTag
		var out deregisterOutcome
		out, err = deregisterStemcellDirectorRefWith(ctx, deps, alog, act.Node, act.VMID, ref, refuseDestroy, opts)
		if err == nil {
			r.RemainingRefs = out.remaining
			r.TagRemoved = out.tagDropped
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
		gone, r.TagRemoved, err = removeStemcellDirectorTagLocked(ctx, deps, alog, act.Node, act.VMID, act.DropTag, verify)
		if err == nil && gone {
			r.Outcome = StemcellRefsOutcomeGone
		}
	case StemcellRefsActionDestroy:
		var gone bool
		gone, err = destroyUnreferencedTemplateLocked(ctx, deps, alog, act.Node, act.VMID, verify, destroy)
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
			r.Error = fmt.Sprintf("template still backs one or more linked-clone VMs; delete or migrate them, "+
				"then rerun (the destroy is marked pending and resumes): %s", err.Error())
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
			if err := destroyTemplateVM(ctx, deps, rep.Node, rep.VMID, stemcellRefsCIDLabel); err != nil {
				return err
			}
			destroyed = true
			return nil
		}
		verify := stemcellRefsVerifier(rep.Name, rep.description, rep.Tags)
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
// fresh read, calls verify on that read, and applies dropDirectorTag's rules
// against the description refs it finds. It never destroys.
func removeStemcellDirectorTagLocked(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	vmid int64,
	dropTag string,
	verify func(cfg map[string]any) error,
) (gone, removed bool, err error) {
	vmidInt := int(vmid) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	owner := fmt.Sprintf("stemcell/director_ref/prune_tag/%d", vmid)
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
		prov, _ := parseStemcellProvenanceFromDescription(description)
		kept, dropped := dropDirectorTag(tags, dropTag, prov.DirectorRefs)
		if !dropped {
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
		return nil
	})
	return gone, removed, err
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
	// as for the unknown-director sentinel, whose VMs carry no owner tag.
	OwnedVMCount     *int    `json:"owned_vm_count"`
	OwnershipUnknown bool    `json:"ownership_unknown"`
	OwnedVMs         []int64 `json:"owned_vms"`
	// WouldFree lists the templates a prune of this ref would destroy, as
	// if --allow-owned-vms were passed.
	WouldFree []int64 `json:"would_free"`
	// Blocked lists the templates a prune would leave because of a safety
	// rule.
	Blocked []int64 `json:"blocked"`
	// Note flags a ref the prune cannot address, such as a blank one.
	Note string `json:"note,omitempty"`
}

// StemcellRefsUnreferenced is a template a --vmid prune could free, or a
// zero-ref or refs-unknown template it would refuse, with the planner's
// reason.
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
}

// StemcellRefsReport is the read-only report.
type StemcellRefsReport struct {
	Templates         []StemcellRefsTemplate     `json:"templates"`
	Refs              []StemcellRefsRefSummary   `json:"refs"`
	Unreferenced      []StemcellRefsUnreferenced `json:"unreferenced"`
	CloneScanComplete bool                       `json:"clone_scan_complete"`
	ScanErrors        []string                   `json:"scan_errors,omitempty"`
}

// BuildStemcellRefsReport summarizes scan. Each ref's WouldFree and Blocked
// columns come from PlanStemcellRefsPrune, and each unreferenced template's
// verdict from the --vmid planner, so the report never predicts something
// the prune would not do.
func BuildStemcellRefsReport(scan StemcellRefsScan) StemcellRefsReport {
	rep := StemcellRefsReport{
		Templates:         scan.Templates,
		Refs:              []StemcellRefsRefSummary{},
		Unreferenced:      []StemcellRefsUnreferenced{},
		CloneScanComplete: scan.CloneScanComplete,
		ScanErrors:        scan.ScanErrors,
	}
	if rep.Templates == nil {
		rep.Templates = []StemcellRefsTemplate{}
	}
	for _, ref := range stemcellRefNames(scan) {
		rep.Refs = append(rep.Refs, summarizeStemcellRef(scan, ref))
	}
	for i := range scan.Templates {
		if u, ok := unreferencedStemcellTemplate(scan, &scan.Templates[i]); ok {
			rep.Unreferenced = append(rep.Unreferenced, u)
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
		OwnedVMs: []int64{}, WouldFree: []int64{}, Blocked: []int64{},
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
	for _, g := range plan.OwnedVMs {
		s.OwnedVMs = append(s.OwnedVMs, g.VMID)
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
		}
	}
	s.OwnershipUnknown = plan.OwnershipUnknown
	if !s.OwnershipUnknown {
		n := len(s.OwnedVMs)
		s.OwnedVMCount = &n
	}
	return s
}

// unreferencedStemcellTemplate returns t's unreferenced-table row with the
// --vmid planner's verdict. A template appears when it provably holds no ref,
// when its blank description leaves its refs unknown (never freeable), or
// when the planner would free it.
func unreferencedStemcellTemplate(scan StemcellRefsScan, t *StemcellRefsTemplate) (StemcellRefsUnreferenced, bool) {
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
	if !freeable {
		u.Reason = plan.RefusalReason
	}
	return u, true
}

// joinVMIDs renders VMIDs as a comma-separated list.
func joinVMIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}
