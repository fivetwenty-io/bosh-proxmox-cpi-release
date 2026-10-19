package handlers

// stemcell_refs_adopt.go: `cpi stemcell-refs adopt-owners`.
//
// A director prune refuses while any BOSH guest carries no owner tag, and
// every VM a CPI before 0.9.4 created is such a guest until something stamps
// the tag. adopt-owners stamps bosh-owner--<director uuid> on the guests of
// one live director, which the operator names by its UUID, its name, and the
// deployments `bosh deployments` lists for it, so the guard can count them.
//
// The pieces mirror the prune. PlanStemcellRefsAdopt is the only planner, and
// the dry run prints its result. ApplyStemcellRefsAdopt executes that same
// value: each guest is written under the per-VMID lock after a fresh read
// that rechecks the guest's own conditions, with the config digest of that
// read, and a guest that fails is reported while the rest carry on.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// Apply outcomes particular to adopt-owners. The shared ones are
// StemcellRefsOutcomeApplied, StemcellRefsOutcomeChanged, and
// StemcellRefsOutcomeFailed.
const (
	// StemcellRefsOutcomeAlreadyOwned is a guest that carried the owner tag
	// by the time the apply read it, so nothing was written.
	StemcellRefsOutcomeAlreadyOwned = "already-owned"
	// StemcellRefsOutcomeNotFound is a guest the apply could not find on the
	// node the scan saw it on. It may have been deleted or migrated.
	StemcellRefsOutcomeNotFound = "not-found"
)

// adoptOwnerWriteAttempts bounds the read-check-write rounds for one guest.
// A round ends early when PVE refuses the write because the config digest
// changed after the read, which a writer that doesn't take the per-VMID lock
// can cause.
const adoptOwnerWriteAttempts = 3

// directorUUIDValue matches the shape of a BOSH director UUID, and of the
// director_id a bosh create-env state file records.
var directorUUIDValue = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrStemcellRefsAdoptRefused is returned by ApplyStemcellRefsAdopt for a plan
// whose Refused flag is set; nothing is changed.
var ErrStemcellRefsAdoptRefused = errors.New("stemcell-refs: adopt-owners refused")

// errAdoptGuestChanged marks a guest whose fresh read no longer meets the
// conditions the plan chose it on.
var errAdoptGuestChanged = errors.New("stemcell-refs: guest changed since the plan was made")

// errAdoptGuestNotFound marks a guest the locked read could not find.
var errAdoptGuestNotFound = errors.New("stemcell-refs: guest not found")

// StemcellRefsAdoptRequest names the director whose guests adopt-owners
// stamps, and the deployments it stamps them in.
type StemcellRefsAdoptRequest struct {
	// DirectorUUID is the director's UUID, or the director_id of a
	// create-env state file. It becomes the owner tag.
	DirectorUUID string
	// DirectorName is the director's raw name. Guests are matched by
	// director-- plus sanitizeTagValue of it, the tag set_vm_metadata writes.
	DirectorName string
	// Deployments names the director's deployments, as `bosh deployments`
	// on that director prints them. At least one is required. Guests are
	// matched by deployment-- plus sanitizeTagValue of each, and each one
	// must match a guest that carries the director tag.
	Deployments []string
	// VMIDs, when set, narrows the guests further. Each one must be a
	// non-template guest that carries the director tag and one of the
	// deployment tags.
	VMIDs []int64
}

// StemcellRefsAdoptGuest is one guest in an adopt plan.
type StemcellRefsAdoptGuest struct {
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// Deployment is the value of the guest's deployment-- tag, "" when it
	// carries none, or several values joined by "," when it carries more.
	Deployment     string   `json:"deployment"`
	DeploymentTags []string `json:"deployment_tags,omitempty"`
	OwnerTags      []string `json:"owner_tags,omitempty"`
	DirectorTags   []string `json:"director_tags,omitempty"`
	// Reason says why a guest is skipped or conflicts.
	Reason string `json:"reason,omitempty"`
}

// StemcellRefsAdoptPlan is what adopt-owners will do. The dry run prints it
// and the apply executes it unchanged.
type StemcellRefsAdoptPlan struct {
	DirectorUUID string `json:"director_uuid"`
	DirectorName string `json:"director_name"`
	DirectorTag  string `json:"director_tag"`
	OwnerTag     string `json:"owner_tag"`
	// Deployments are the requested deployments, one per distinct tag and
	// sorted by tag, and DeploymentTags[i] is the tag of Deployments[i].
	Deployments    []string `json:"deployments"`
	DeploymentTags []string `json:"deployment_tags"`
	// OtherDeployments are the deployments of guests that carry the director
	// tag that the run doesn't name. Their guests are left alone. When two
	// directors share a name, they belong to the other one.
	OtherDeployments []string `json:"other_deployments"`
	VMIDs            []int64  `json:"vmids,omitempty"`
	// Targets are the guests the apply stamps, sorted by deployment and
	// VMID.
	Targets []StemcellRefsAdoptGuest `json:"targets"`
	// AlreadyOwned carry the owner tag already.
	AlreadyOwned []StemcellRefsAdoptGuest `json:"already_owned"`
	// Skipped are selected guests that carry no owner tag, but adopt-owners
	// never stamps them, such as a guest with a stemcell marker. They keep
	// ownership unknown until an operator deals with them.
	Skipped []StemcellRefsAdoptGuest `json:"skipped"`
	// Conflicts are guests anywhere in the cluster that contradict the
	// request. Any conflict refuses the plan.
	Conflicts     []StemcellRefsAdoptGuest `json:"conflicts"`
	Refused       bool                     `json:"refused"`
	RefusalReason string                   `json:"refusal_reason,omitempty"`
	ScanErrors    []string                 `json:"scan_errors,omitempty"`

	// planned is set by the planner, so the apply refuses a plan it did not
	// produce, such as one decoded from JSON.
	planned bool
}

// createEnv reports whether the plan names the create-env director name.
func (p *StemcellRefsAdoptPlan) createEnv() bool {
	return strings.EqualFold(p.DirectorTag, directorTagPrefix+createEnvDirectorName)
}

// deploymentSet returns the plan's deployment tags, lowercased, as a set.
func (p *StemcellRefsAdoptPlan) deploymentSet() map[string]bool {
	set := make(map[string]bool, len(p.DeploymentTags))
	for _, t := range p.DeploymentTags {
		set[strings.ToLower(t)] = true
	}
	return set
}

// StemcellRefsAdoptGuestResult is what applying the plan did to one guest.
type StemcellRefsAdoptGuestResult struct {
	Guest   StemcellRefsAdoptGuest `json:"guest"`
	Outcome string                 `json:"outcome"`
	Error   string                 `json:"error,omitempty"`
	// Failure is true when the guest counts against the exit code.
	Failure bool `json:"failure"`
}

// StemcellRefsAdoptResult is the whole apply. A refused plan yields the same
// shape with Refused set and no results.
type StemcellRefsAdoptResult struct {
	Plan     StemcellRefsAdoptPlan          `json:"plan"`
	Refused  bool                           `json:"refused"`
	Results  []StemcellRefsAdoptGuestResult `json:"results"`
	Failures int                            `json:"failures"`
}

// ValidateStemcellRefsAdoptRequest returns the error in a malformed request,
// or nil. Nothing here needs the cluster.
func ValidateStemcellRefsAdoptRequest(req StemcellRefsAdoptRequest) error {
	uuid := strings.TrimSpace(req.DirectorUUID)
	name := sanitizeTagValue(req.DirectorName)
	switch {
	case !directorUUIDValue.MatchString(uuid):
		return cpierrors.Cloud("stemcell-refs: the director UUID %q is not a UUID; copy it from `bosh env`, "+
			"or for a create-env Director from director_id in its state file", req.DirectorUUID)
	case name == "":
		return cpierrors.Cloud("stemcell-refs: the director name %q has no characters a tag can hold", req.DirectorName)
	case directorUUIDValue.MatchString(name):
		return cpierrors.Cloud("stemcell-refs: the director name %q is a UUID; pass the director's name, "+
			"which `bosh env` prints as Name, and its UUID separately", req.DirectorName)
	case len(req.Deployments) == 0:
		return cpierrors.Cloud("stemcell-refs: name the director's deployments with --deployment, as " +
			"`bosh -e <env> deployments` on that director lists them; for a create-env Director, use the " +
			"deployment name in its create-env manifest")
	}
	for _, d := range req.Deployments {
		if sanitizeTagValue(d) == "" {
			return cpierrors.Cloud("stemcell-refs: the deployment %q has no characters a tag can hold", d)
		}
	}
	for _, id := range req.VMIDs {
		if id <= 0 {
			return cpierrors.Cloud("stemcell-refs: VMID %d is not a positive integer", id)
		}
	}
	return nil
}

// adoptGuestFrom renders a scanned guest for an adopt plan.
func adoptGuestFrom(g *StemcellRefsGuest, reason string) StemcellRefsAdoptGuest {
	return StemcellRefsAdoptGuest{
		VMID: g.VMID, Node: g.Node, Name: g.Name,
		Deployment:     strings.Join(tagSuffixes(g.DeploymentTags, deploymentTagPrefix), ","),
		DeploymentTags: g.DeploymentTags,
		OwnerTags:      g.OwnerTags,
		DirectorTags:   g.DirectorTags,
		Reason:         reason,
	}
}

// adoptInRequest reports whether g carries the director tag and at least one
// of the requested deployment tags.
func adoptInRequest(g *StemcellRefsGuest, dirTag string, deps map[string]bool) bool {
	if !tagsContainFold(g.DirectorTags, dirTag) {
		return false
	}
	for _, d := range g.DeploymentTags {
		if deps[strings.ToLower(d)] {
			return true
		}
	}
	return false
}

// adoptWithinRequest reports whether every director-- and deployment-- tag
// g carries is one the request names, and it carries at least one of each.
func adoptWithinRequest(g *StemcellRefsGuest, dirTag string, deps map[string]bool) bool {
	if len(g.DirectorTags) == 0 || len(g.DeploymentTags) == 0 {
		return false
	}
	for _, d := range g.DirectorTags {
		if !strings.EqualFold(d, dirTag) {
			return false
		}
	}
	for _, d := range g.DeploymentTags {
		if !deps[strings.ToLower(d)] {
			return false
		}
	}
	return true
}

// adoptPairConflict returns why g, any guest in the cluster, contradicts the
// plan's pair, or "". Two checks run on every guest:
//
//   - A guest that carries the plan's owner tag must carry only the
//     director tag and deployment tags the request names. Otherwise that UUID
//     already belongs to another director or deployment.
//   - A guest that carries the director tag in a requested deployment must
//     carry no owner tag but the plan's, because a deployment belongs to one
//     director.
//
// A guest with the director name in a deployment the request doesn't name
// may carry another owner tag. That is how two directors share a name.
func adoptPairConflict(g *StemcellRefsGuest, plan *StemcellRefsAdoptPlan, deps map[string]bool) string {
	var reasons []string
	if stringSliceContains(g.OwnerTags, plan.OwnerTag) && !adoptWithinRequest(g, plan.DirectorTag, deps) {
		reasons = append(reasons, fmt.Sprintf("it carries %s with director tags [%s] and deployment tags [%s], "+
			"outside the director name and deployments this run names, so that UUID already belongs to another "+
			"director or deployment", plan.OwnerTag, strings.Join(g.DirectorTags, ", "),
			strings.Join(g.DeploymentTags, ", ")))
	}
	if adoptInRequest(g, plan.DirectorTag, deps) {
		var others []string
		for _, o := range g.OwnerTags {
			if o != plan.OwnerTag {
				others = append(others, o)
			}
		}
		if len(others) > 0 {
			reasons = append(reasons, fmt.Sprintf("it carries %s in a deployment this run names, with the owner "+
				"tag %s, and a deployment belongs to one director", plan.DirectorTag, strings.Join(others, ", ")))
		}
	}
	return strings.Join(reasons, "; ")
}

// newAdoptPlan builds the plan's request fields: the tags, the requested
// deployments with one entry per distinct tag sorted by tag, and the
// requested VMIDs deduplicated and sorted.
func newAdoptPlan(scan StemcellRefsScan, req StemcellRefsAdoptRequest) StemcellRefsAdoptPlan {
	plan := StemcellRefsAdoptPlan{
		DirectorUUID: strings.TrimSpace(req.DirectorUUID),
		DirectorName: req.DirectorName,
		DirectorTag:  directorTagPrefix + sanitizeTagValue(req.DirectorName),
		Deployments:  []string{}, DeploymentTags: []string{}, OtherDeployments: []string{},
		Targets: []StemcellRefsAdoptGuest{}, AlreadyOwned: []StemcellRefsAdoptGuest{},
		Skipped: []StemcellRefsAdoptGuest{}, Conflicts: []StemcellRefsAdoptGuest{},
		ScanErrors: scan.ScanErrors,
		planned:    true,
	}
	plan.OwnerTag = ownerTagFor(plan.DirectorUUID)
	byTag := map[string]string{}
	for _, d := range req.Deployments {
		tag := deploymentTagPrefix + sanitizeTagValue(d)
		if _, seen := byTag[strings.ToLower(tag)]; !seen {
			byTag[strings.ToLower(tag)] = strings.TrimSpace(d)
			plan.DeploymentTags = append(plan.DeploymentTags, tag)
		}
	}
	sort.Slice(plan.DeploymentTags, func(a, b int) bool {
		return strings.ToLower(plan.DeploymentTags[a]) < strings.ToLower(plan.DeploymentTags[b])
	})
	for _, tag := range plan.DeploymentTags {
		plan.Deployments = append(plan.Deployments, byTag[strings.ToLower(tag)])
	}
	seen := map[int64]bool{}
	for _, id := range req.VMIDs {
		if !seen[id] {
			seen[id] = true
			plan.VMIDs = append(plan.VMIDs, id)
		}
	}
	sort.Slice(plan.VMIDs, func(a, b int) bool { return plan.VMIDs[a] < plan.VMIDs[b] })
	return plan
}

// PlanStemcellRefsAdopt is the single planner behind the adopt-owners dry run
// and apply. It never touches PVE. It returns an error only for a malformed
// request; a request the cluster's state rules out comes back as a refused
// plan.
//
// The selected guests carry the director tag and one of the requested
// deployment tags, and are among the requested VMIDs when any are named. A
// selected guest is a target when it has no owner tag and adoptableGuest
// accepts it, already owned when it carries the plan's owner tag, and skipped
// otherwise. The pairing checks in adoptPairConflict run on every guest in
// the cluster, selected or not, and any conflict refuses the plan.
//
// The plan is also refused when the scan could not read every guest, when a
// requested deployment matches no guest with the director name, and when a
// requested VMID is not a selected guest. For the create-env name, bosh-init,
// the selection must hold exactly one guest, the owner tag may sit on no
// other guest, and every guest without director--bosh-init must already be
// covered, so the deployed directors are adopted first and a UUID copied from
// `bosh env` by mistake shows up as a conflict.
func PlanStemcellRefsAdopt(scan StemcellRefsScan, req StemcellRefsAdoptRequest) (StemcellRefsAdoptPlan, error) {
	if err := ValidateStemcellRefsAdoptRequest(req); err != nil {
		return StemcellRefsAdoptPlan{}, err
	}
	plan := newAdoptPlan(scan, req)
	deps := plan.deploymentSet()
	vmidSet := map[int64]bool{}
	for _, id := range plan.VMIDs {
		vmidSet[id] = true
	}

	matched := map[string]bool{}
	others := map[string]bool{}
	var selected []int64
	for i := range scan.Guests {
		g := &scan.Guests[i]
		named := tagsContainFold(g.DirectorTags, plan.DirectorTag)
		if named {
			for _, d := range g.DeploymentTags {
				if deps[strings.ToLower(d)] {
					matched[strings.ToLower(d)] = true
				} else {
					others[tagValueKey(d, deploymentTagPrefix)] = true
				}
			}
		}
		isSelected := g.Kind != StemcellRefsGuestParker && adoptInRequest(g, plan.DirectorTag, deps) &&
			(len(vmidSet) == 0 || vmidSet[g.VMID])
		reason := adoptPairConflict(g, &plan, deps)
		if plan.createEnv() && !isSelected && stringSliceContains(g.OwnerTags, plan.OwnerTag) {
			reason = joinReasons(reason, fmt.Sprintf("it carries %s too, and a create-env director_id owns "+
				"exactly one VM", plan.OwnerTag))
		}
		if reason != "" {
			plan.Conflicts = append(plan.Conflicts, adoptGuestFrom(g, reason))
		}
		if !isSelected {
			continue
		}
		selected = append(selected, g.VMID)
		switch ok, why, _ := adoptableGuest(g); {
		case reason != "":
		case len(g.OwnerTags) > 0:
			plan.AlreadyOwned = append(plan.AlreadyOwned, adoptGuestFrom(g, ""))
		case !ok:
			plan.Skipped = append(plan.Skipped, adoptGuestFrom(g, why))
		default:
			plan.Targets = append(plan.Targets, adoptGuestFrom(g, ""))
		}
	}
	if len(others) > 0 {
		plan.OtherDeployments = sortedKeys(others)
	}

	problems := adoptScanProblems(scan)
	problems = append(problems, adoptDeploymentProblems(&plan, matched)...)
	problems = append(problems, adoptVMIDProblems(scan, &plan, deps)...)
	if plan.createEnv() {
		problems = append(problems, adoptCreateEnvProblems(scan, &plan, selected, len(matched) == len(deps))...)
	}
	if len(plan.Conflicts) > 0 {
		ids := make([]int64, 0, len(plan.Conflicts))
		for i := range plan.Conflicts {
			ids = append(ids, plan.Conflicts[i].VMID)
		}
		problems = append(problems, fmt.Sprintf("%d guest(s) (%s) contradict pairing %s with %s in deployment(s) "+
			"%s; check the UUID, the name, and the deployments against `bosh -e <env> env` and "+
			"`bosh -e <env> deployments`", len(ids), capVMIDs(ids), plan.OwnerTag, plan.DirectorTag,
			strings.Join(plan.Deployments, ", ")))
	}
	if len(problems) > 0 {
		plan.Refused = true
		plan.RefusalReason = strings.Join(problems, "; ")
	}
	sortAdoptGuests(plan.Targets)
	sortAdoptGuests(plan.AlreadyOwned)
	sortAdoptGuests(plan.Skipped)
	sortAdoptGuests(plan.Conflicts)
	return plan, nil
}

// joinReasons joins two conflict reasons, either of which may be "".
func joinReasons(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// adoptScanProblems refuses a plan built on a scan that could not read every
// guest, because the pairing checks must see every guest's tags.
func adoptScanProblems(scan StemcellRefsScan) []string {
	if len(scan.ScanErrors) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("the scan could not read %d guest config(s), so the pairing checks can't see "+
		"every guest; fix the reads the scan errors name and run adopt-owners again", len(scan.ScanErrors))}
}

// adoptDeploymentProblems returns one problem for each requested deployment
// that no guest with the director name carries.
func adoptDeploymentProblems(plan *StemcellRefsAdoptPlan, matched map[string]bool) []string {
	var problems []string
	for i, tag := range plan.DeploymentTags {
		if !matched[strings.ToLower(tag)] {
			problems = append(problems, fmt.Sprintf("no guest tagged %s carries %s, so --deployment %q matches "+
				"nothing; check it against `bosh -e <env> deployments`", plan.DirectorTag, tag, plan.Deployments[i]))
		}
	}
	return problems
}

// adoptCreateEnvProblems returns the problems particular to the create-env
// name: a selection that is not exactly one guest, and any uncovered guest
// that doesn't carry director--bosh-init. allMatched is false when a
// requested deployment already failed to match, which reports that problem
// on its own.
func adoptCreateEnvProblems(scan StemcellRefsScan, plan *StemcellRefsAdoptPlan, selected []int64,
	allMatched bool) []string {
	var problems []string
	if allMatched && len(selected) != 1 {
		problems = append(problems, fmt.Sprintf("the selection holds %d guest(s) tagged %s in deployment(s) %s "+
			"(%s), and a bosh create-env deployment has exactly one VM; narrow it with --vmid", len(selected),
			plan.DirectorTag, strings.Join(plan.Deployments, ", "), dashOrVMIDs(selected)))
	}
	var blocking []int64
	for i := range scan.Guests {
		g := &scan.Guests[i]
		if g.Uncovered && !tagsContainFold(g.DirectorTags, plan.DirectorTag) {
			blocking = append(blocking, g.VMID)
		}
	}
	if len(blocking) > 0 {
		problems = append(problems, fmt.Sprintf("adopt-owners refuses %s while %d other guest(s) leave ownership "+
			"unknown (%s). Adopt the deployed directors first, because once their VMs carry their owner tags, a "+
			"UUID copied from `bosh env` by mistake shows up here as a conflict", createEnvDirectorName,
			len(blocking), capVMIDs(blocking)))
	}
	return problems
}

// adoptVMIDProblems returns one problem for each requested VMID that does not
// name a non-template guest carrying the director tag and one of the
// requested deployment tags.
func adoptVMIDProblems(scan StemcellRefsScan, plan *StemcellRefsAdoptPlan, deps map[string]bool) []string {
	var problems []string
	for _, id := range plan.VMIDs {
		g := guestByVMID(scan, id)
		switch {
		case g == nil && isTemplateVMID(scan, id):
			problems = append(problems, fmt.Sprintf("VMID %d is a template", id))
		case g == nil:
			problems = append(problems, fmt.Sprintf("no non-template guest with VMID %d was found in the cluster", id))
		case g.Kind == StemcellRefsGuestParker:
			problems = append(problems, fmt.Sprintf("VMID %d is a parker", id))
		case !tagsContainFold(g.DirectorTags, plan.DirectorTag):
			problems = append(problems, fmt.Sprintf("VMID %d does not carry %s", id, plan.DirectorTag))
		case !adoptInRequest(g, plan.DirectorTag, deps):
			problems = append(problems, fmt.Sprintf("VMID %d carries none of %s", id,
				strings.Join(plan.DeploymentTags, ", ")))
		}
	}
	return problems
}

// isTemplateVMID reports whether the scan saw vmid as a template of any kind.
func isTemplateVMID(scan StemcellRefsScan, vmid int64) bool {
	if _, ok := scan.otherTemplates[vmid]; ok {
		return true
	}
	for i := range scan.Templates {
		if scan.Templates[i].VMID == vmid {
			return true
		}
	}
	return false
}

// dashOrVMIDs renders ids, or "no VMIDs" when there are none.
func dashOrVMIDs(ids []int64) string {
	if len(ids) == 0 {
		return "no VMIDs"
	}
	return capVMIDs(ids)
}

// sortAdoptGuests orders guests by deployment, then VMID, so the dry run can
// print them grouped by deployment.
func sortAdoptGuests(gs []StemcellRefsAdoptGuest) {
	sort.Slice(gs, func(a, b int) bool {
		if gs[a].Deployment != gs[b].Deployment {
			return gs[a].Deployment < gs[b].Deployment
		}
		return gs[a].VMID < gs[b].VMID
	})
}

// ApplyStemcellRefsAdopt executes plan. A refused plan changes nothing and
// returns ErrStemcellRefsAdoptRefused with a result whose Refused flag is
// set. Each target is written on its own: a failure is recorded in its
// result and the apply moves on, so the caller exits 1 when Failures is
// non-zero.
func ApplyStemcellRefsAdopt(ctx context.Context, deps Deps, plan StemcellRefsAdoptPlan) (StemcellRefsAdoptResult, error) {
	res := StemcellRefsAdoptResult{Plan: plan, Results: []StemcellRefsAdoptGuestResult{}}
	if plan.Refused {
		res.Refused = true
		return res, ErrStemcellRefsAdoptRefused
	}
	if !plan.planned {
		return res, cpierrors.Cloud("stemcell-refs: the adopt plan did not come from the planner, so it is not applied")
	}
	if deps.PVE == nil || deps.PVE.QEMU() == nil || deps.PVE.Nodes() == nil {
		return res, cpierrors.Cloud("stemcell-refs: PVE client unavailable")
	}
	logger := deps.Log(ctx).With(log.String("operation", "stemcell_refs.adopt_owners"),
		log.String("owner_tag", plan.OwnerTag), log.String("director_tag", plan.DirectorTag))
	for i := range plan.Targets {
		target := plan.Targets[i]
		r := StemcellRefsAdoptGuestResult{Guest: target}
		alog := logger.With(log.String("node", target.Node), log.Int64("vmid", target.VMID))
		outcome, err := adoptOwnerLocked(ctx, deps, alog, &plan, &target)
		switch {
		case errors.Is(err, errAdoptGuestChanged):
			r.Outcome = StemcellRefsOutcomeChanged
		case errors.Is(err, errAdoptGuestNotFound):
			r.Outcome = StemcellRefsOutcomeNotFound
		case err != nil:
			r.Outcome = StemcellRefsOutcomeFailed
		default:
			r.Outcome = outcome
		}
		if err != nil {
			r.Error = err.Error()
			r.Failure = true
			res.Failures++
			alog.Warn("stemcell-refs: owner tag not stamped", log.Err(err))
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

// adoptOwnerLocked stamps plan.OwnerTag on one target. Under the per-VMID
// lock it reads the guest's config, rechecks every condition the plan chose
// the guest on, and writes the tags with the owner tag at the head of the
// list, guarded by the digest of that read. A digest refusal starts another
// round, up to adoptOwnerWriteAttempts. It returns the outcome on success.
func adoptOwnerLocked(
	ctx context.Context, deps Deps, logger *log.Logger, plan *StemcellRefsAdoptPlan, target *StemcellRefsAdoptGuest,
) (string, error) {
	vmidInt := int(target.VMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	owner := fmt.Sprintf("stemcell/adopt_owner/%d", target.VMID)
	outcome := ""
	err := withVMIDLock(ctx, deps.PVE.Pools(), vmidInt, owner, logger, func() error {
		for attempt := 1; attempt <= adoptOwnerWriteAttempts; attempt++ {
			cfg, cfgErr := deps.PVE.QEMU().Config(ctx, target.Node, vmidInt)
			if cfgErr != nil {
				if stemcellRefsGone(cfgErr) {
					return fmt.Errorf("%w: vmid %d is not on node %s; it may have been deleted or migrated, "+
						"so run the report again", errAdoptGuestNotFound, target.VMID, target.Node)
				}
				return cpierrors.Wrap(pve.WrapError(cfgErr),
					fmt.Sprintf("stemcell-refs: read config vmid %d node %q", target.VMID, target.Node))
			}
			digest, ok := pve.ConfigString(cfg, "digest")
			if !ok || digest == "" {
				return cpierrors.Cloud("stemcell-refs: the config read of vmid %d carries no digest, so nothing "+
					"was written", target.VMID)
			}
			tags := parseTagsField(stringConfigField(cfg, jsonKeyTags))
			owned, checkErr := adoptRecheck(plan, target, cfg, tags)
			if checkErr != nil {
				return checkErr
			}
			if owned {
				outcome = StemcellRefsOutcomeAlreadyOwned
				return nil
			}
			joined := strings.Join(withOwnerTag(tags, plan.OwnerTag), pveTagJoinSep)
			params := &sdknodes.UpdateQemuConfigParams{Tags: &joined, Digest: &digest}
			updErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, target.Node, strconv.FormatInt(target.VMID, 10), params)
			switch {
			case updErr == nil:
				outcome = StemcellRefsOutcomeApplied
				logger.Info("stemcell-refs: stamped the owner tag on a guest that predates owner tags")
				return nil
			case pve.IsConfigDigestStale(updErr):
				logger.Debug("stemcell-refs: config changed between the read and the write; reading again",
					log.Int("attempt", attempt))
				continue
			case pve.IsNotFound(updErr):
				return fmt.Errorf("%w: vmid %d disappeared from node %s before the write", errAdoptGuestNotFound,
					target.VMID, target.Node)
			}
			return cpierrors.Wrap(pve.WrapError(updErr),
				fmt.Sprintf("stemcell-refs: write tags vmid %d node %q", target.VMID, target.Node))
		}
		return cpierrors.Cloud("stemcell-refs: the config of vmid %d changed between our read and our write on "+
			"each of %d attempts, so nothing was written; run adopt-owners again", target.VMID, adoptOwnerWriteAttempts)
	})
	return outcome, err
}

// adoptRecheck holds a fresh config read to every condition the plan chose
// target on that the guest itself decides. It reports owned when the guest
// already carries the plan's owner tag, and returns errAdoptGuestChanged when
// any condition no longer holds. The cluster-wide checks ran when the plan
// was made and are not repeated here.
func adoptRecheck(plan *StemcellRefsAdoptPlan, target *StemcellRefsAdoptGuest, cfg map[string]any,
	tags []string) (bool, error) {
	changed := func(why string) (bool, error) {
		return false, fmt.Errorf("%w: vmid %d %s", errAdoptGuestChanged, target.VMID, why)
	}
	if pve.ConfigFlag(cfg, pveConfigKeyTemplate) {
		return changed("is now a template")
	}
	g := newStemcellRefsGuest(target.VMID, target.Node, target.Name, tags, stringConfigField(cfg, "description"), "")
	deps := plan.deploymentSet()
	if reason := adoptPairConflict(&g, plan, deps); reason != "" {
		return changed("now conflicts: " + reason)
	}
	if !tagsContainFold(g.DirectorTags, plan.DirectorTag) {
		return changed("no longer carries " + plan.DirectorTag)
	}
	if !adoptInRequest(&g, plan.DirectorTag, deps) {
		return changed("no longer carries any of " + strings.Join(plan.DeploymentTags, ", "))
	}
	if len(g.OwnerTags) > 0 {
		// adoptPairConflict already ruled out any owner tag but the plan's.
		return true, nil
	}
	if ok, reason, _ := adoptableGuest(&g); !ok {
		return changed("can no longer be adopted: " + reason)
	}
	return false, nil
}
