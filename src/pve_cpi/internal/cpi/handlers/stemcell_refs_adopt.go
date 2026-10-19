package handlers

// stemcell_refs_adopt.go: `cpi stemcell-refs adopt-owners`.
//
// A director prune refuses while any BOSH guest carries no owner tag, and
// every VM a CPI before 0.9.4 created is such a guest until a later
// set_vm_metadata backfills the tag. adopt-owners stamps
// bosh-owner--<director uuid> on the guests of one live director, which the
// operator names by its UUID and its name, so the guard can count them.
//
// The pieces mirror the prune. PlanStemcellRefsAdopt is the only planner, and
// the dry run prints its result. ApplyStemcellRefsAdopt executes that same
// value: each guest is written under the per-VMID lock after a fresh read
// that rechecks every condition, with the config digest of that read, and a
// guest that fails is reported while the rest carry on.

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
// stamps, and optionally narrows the guests it considers.
type StemcellRefsAdoptRequest struct {
	// DirectorUUID is the director's UUID, or the director_id of a
	// create-env state file. It becomes the owner tag.
	DirectorUUID string
	// DirectorName is the director's raw name. Guests are matched by
	// director-- plus sanitizeTagValue of it, the tag set_vm_metadata writes.
	DirectorName string
	// Deployment, when set, keeps only the guests whose deployment-- tag
	// matches deployment-- plus sanitizeTagValue of it.
	Deployment string
	// VMIDs, when set, keeps only these guests. Each one must be a
	// non-template guest that carries the director tag.
	VMIDs []int64
}

// StemcellRefsAdoptGuest is one guest in an adopt plan.
type StemcellRefsAdoptGuest struct {
	VMID int64  `json:"vmid"`
	Node string `json:"node"`
	Name string `json:"name"`
	// Deployment is the value of the guest's deployment-- tag, "" when it
	// carries none.
	Deployment   string   `json:"deployment"`
	OwnerTags    []string `json:"owner_tags,omitempty"`
	DirectorTags []string `json:"director_tags,omitempty"`
	// Reason says why a guest is skipped or conflicts.
	Reason string `json:"reason,omitempty"`
}

// StemcellRefsAdoptPlan is what adopt-owners will do. The dry run prints it
// and the apply executes it unchanged.
type StemcellRefsAdoptPlan struct {
	DirectorUUID  string  `json:"director_uuid"`
	DirectorName  string  `json:"director_name"`
	DirectorTag   string  `json:"director_tag"`
	OwnerTag      string  `json:"owner_tag"`
	Deployment    string  `json:"deployment,omitempty"`
	DeploymentTag string  `json:"deployment_tag,omitempty"`
	VMIDs         []int64 `json:"vmids,omitempty"`
	// Targets are the guests the apply stamps, sorted by deployment and
	// VMID.
	Targets []StemcellRefsAdoptGuest `json:"targets"`
	// AlreadyOwned carry the owner tag already.
	AlreadyOwned []StemcellRefsAdoptGuest `json:"already_owned"`
	// Skipped carry the director tag and no owner tag, but adopt-owners
	// never stamps them, such as a guest with a stemcell marker. They keep
	// ownership unknown until an operator deals with them.
	Skipped []StemcellRefsAdoptGuest `json:"skipped"`
	// Conflicts pair the director name with another owner tag, or the owner
	// tag with another director name. Any conflict refuses the plan.
	Conflicts     []StemcellRefsAdoptGuest `json:"conflicts"`
	Refused       bool                     `json:"refused"`
	RefusalReason string                   `json:"refusal_reason,omitempty"`
	ScanErrors    []string                 `json:"scan_errors,omitempty"`

	// planned is set by the planner, so the apply refuses a plan it did not
	// produce, such as one decoded from JSON.
	planned bool
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
	case req.Deployment != "" && sanitizeTagValue(req.Deployment) == "":
		return cpierrors.Cloud("stemcell-refs: the deployment %q has no characters a tag can hold", req.Deployment)
	}
	for _, id := range req.VMIDs {
		if id <= 0 {
			return cpierrors.Cloud("stemcell-refs: VMID %d is not a positive integer", id)
		}
	}
	if strings.EqualFold(name, createEnvDirectorName) && req.Deployment == "" && len(req.VMIDs) == 0 {
		return cpierrors.Cloud("stemcell-refs: every bosh create-env Director VM is tagged director--%s, so name "+
			"the one to adopt with --deployment or --vmid", createEnvDirectorName)
	}
	return nil
}

// adoptGuestFrom renders a scanned guest for an adopt plan.
func adoptGuestFrom(g *StemcellRefsGuest, reason string) StemcellRefsAdoptGuest {
	return StemcellRefsAdoptGuest{
		VMID: g.VMID, Node: g.Node, Name: g.Name,
		Deployment:   strings.Join(tagSuffixes(g.DeploymentTags, deploymentTagPrefix), ","),
		OwnerTags:    g.OwnerTags,
		DirectorTags: g.DirectorTags,
		Reason:       reason,
	}
}

// adoptConflict returns why g pairs the director tag with another owner tag,
// or the owner tag with another director tag, or "".
func adoptConflict(g *StemcellRefsGuest, dirTag, ownerTag string) string {
	carriesName := tagsContainFold(g.DirectorTags, dirTag)
	var otherOwners []string
	ownsTag := false
	for _, o := range g.OwnerTags {
		if o == ownerTag {
			ownsTag = true
		} else {
			otherOwners = append(otherOwners, o)
		}
	}
	var otherNames []string
	for _, d := range g.DirectorTags {
		if !strings.EqualFold(d, dirTag) {
			otherNames = append(otherNames, d)
		}
	}
	switch {
	case carriesName && len(otherOwners) > 0:
		return fmt.Sprintf("it carries %s with the owner tag %s, so that director name already belongs to "+
			"another UUID", dirTag, strings.Join(otherOwners, ", "))
	case ownsTag && len(otherNames) > 0:
		return fmt.Sprintf("it carries %s with %s, so that UUID already belongs to another director name",
			ownerTag, strings.Join(otherNames, ", "))
	}
	return ""
}

// PlanStemcellRefsAdopt is the single planner behind the adopt-owners dry run
// and apply. It never touches PVE. It returns an error only for a malformed
// request; a request the cluster's state rules out comes back as a refused
// plan.
//
// It considers the non-template guests the selectors keep, or every one when
// none is set. Parkers are left out, because a parker's director-- tag holds a
// UUID and adopt-owners never stamps one. A guest that pairs the director
// name with another owner tag, or the owner tag with another director name,
// refuses the plan. A guest that carries the director tag is a target when it
// has no owner tag and adoptableGuest accepts it, already owned when it
// carries the owner tag, and skipped otherwise.
func PlanStemcellRefsAdopt(scan StemcellRefsScan, req StemcellRefsAdoptRequest) (StemcellRefsAdoptPlan, error) {
	if err := ValidateStemcellRefsAdoptRequest(req); err != nil {
		return StemcellRefsAdoptPlan{}, err
	}
	plan := StemcellRefsAdoptPlan{
		DirectorUUID: strings.TrimSpace(req.DirectorUUID),
		DirectorName: req.DirectorName,
		DirectorTag:  directorTagPrefix + sanitizeTagValue(req.DirectorName),
		Deployment:   req.Deployment,
		Targets:      []StemcellRefsAdoptGuest{}, AlreadyOwned: []StemcellRefsAdoptGuest{},
		Skipped: []StemcellRefsAdoptGuest{}, Conflicts: []StemcellRefsAdoptGuest{},
		ScanErrors: scan.ScanErrors,
		planned:    true,
	}
	plan.OwnerTag = ownerTagFor(plan.DirectorUUID)
	if req.Deployment != "" {
		plan.DeploymentTag = deploymentTagPrefix + sanitizeTagValue(req.Deployment)
	}
	vmidSet := map[int64]bool{}
	for _, id := range req.VMIDs {
		if !vmidSet[id] {
			vmidSet[id] = true
			plan.VMIDs = append(plan.VMIDs, id)
		}
	}
	sort.Slice(plan.VMIDs, func(a, b int) bool { return plan.VMIDs[a] < plan.VMIDs[b] })

	var problems []string
	var selectedNamed []int64
	for i := range scan.Guests {
		g := &scan.Guests[i]
		if g.Kind == StemcellRefsGuestParker || !adoptSelected(g, plan.DeploymentTag, vmidSet) {
			continue
		}
		if reason := adoptConflict(g, plan.DirectorTag, plan.OwnerTag); reason != "" {
			plan.Conflicts = append(plan.Conflicts, adoptGuestFrom(g, reason))
		}
		if !tagsContainFold(g.DirectorTags, plan.DirectorTag) {
			continue
		}
		selectedNamed = append(selectedNamed, g.VMID)
		switch ok, reason := adoptableGuest(g); {
		case adoptConflict(g, plan.DirectorTag, plan.OwnerTag) != "":
		case len(g.OwnerTags) > 0:
			plan.AlreadyOwned = append(plan.AlreadyOwned, adoptGuestFrom(g, ""))
		case !ok:
			plan.Skipped = append(plan.Skipped, adoptGuestFrom(g, reason))
		default:
			plan.Targets = append(plan.Targets, adoptGuestFrom(g, ""))
		}
	}
	problems = append(problems, adoptVMIDProblems(scan, plan, vmidSet)...)
	selector := plan.DeploymentTag != "" || len(vmidSet) > 0
	switch {
	case strings.EqualFold(sanitizeTagValue(req.DirectorName), createEnvDirectorName) && len(selectedNamed) != 1:
		problems = append(problems, fmt.Sprintf("the selection holds %d guest(s) tagged %s (%s), and a bosh "+
			"create-env deployment has exactly one VM; narrow it with --vmid", len(selectedNamed), plan.DirectorTag,
			dashOrVMIDs(selectedNamed)))
	case selector && len(selectedNamed) == 0:
		problems = append(problems, fmt.Sprintf("no guest the selectors keep carries %s", plan.DirectorTag))
	}
	if len(plan.Conflicts) > 0 {
		ids := make([]int64, 0, len(plan.Conflicts))
		for _, c := range plan.Conflicts {
			ids = append(ids, c.VMID)
		}
		problems = append(problems, fmt.Sprintf("%d guest(s) (%s) already pair %s or %s with a different "+
			"partner, so this pair would contradict them; check both values against `bosh env`",
			len(ids), joinVMIDs(ids), plan.DirectorTag, plan.OwnerTag))
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

// adoptSelected reports whether the selectors keep g. An empty deploymentTag
// and an empty vmidSet keep everything.
func adoptSelected(g *StemcellRefsGuest, deploymentTag string, vmidSet map[int64]bool) bool {
	if deploymentTag != "" && !tagsContainFold(g.DeploymentTags, deploymentTag) {
		return false
	}
	return len(vmidSet) == 0 || vmidSet[g.VMID]
}

// adoptVMIDProblems returns one problem for each requested VMID that does not
// name a non-template guest carrying the director tag and, when a deployment
// is named, the deployment tag.
func adoptVMIDProblems(scan StemcellRefsScan, plan StemcellRefsAdoptPlan, vmidSet map[int64]bool) []string {
	var problems []string
	for _, id := range plan.VMIDs {
		g := guestByVMID(scan, id)
		switch {
		case g == nil && vmidSet[id] && isTemplateVMID(scan, id):
			problems = append(problems, fmt.Sprintf("VMID %d is a template", id))
		case g == nil:
			problems = append(problems, fmt.Sprintf("no non-template guest with VMID %d was found in the cluster", id))
		case g.Kind == StemcellRefsGuestParker:
			problems = append(problems, fmt.Sprintf("VMID %d is a parker", id))
		case !tagsContainFold(g.DirectorTags, plan.DirectorTag):
			problems = append(problems, fmt.Sprintf("VMID %d does not carry %s", id, plan.DirectorTag))
		case plan.DeploymentTag != "" && !tagsContainFold(g.DeploymentTags, plan.DeploymentTag):
			problems = append(problems, fmt.Sprintf("VMID %d does not carry %s", id, plan.DeploymentTag))
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
	return joinVMIDs(ids)
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
	for _, target := range plan.Targets {
		r := StemcellRefsAdoptGuestResult{Guest: target}
		alog := logger.With(log.String("node", target.Node), log.Int64("vmid", target.VMID))
		outcome, err := adoptOwnerLocked(ctx, deps, alog, plan, target)
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
	ctx context.Context, deps Deps, logger *log.Logger, plan StemcellRefsAdoptPlan, target StemcellRefsAdoptGuest,
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
// target on. It reports owned when the guest already carries exactly the
// plan's owner tag, and returns errAdoptGuestChanged when any condition no
// longer holds.
func adoptRecheck(plan StemcellRefsAdoptPlan, target StemcellRefsAdoptGuest, cfg map[string]any,
	tags []string) (bool, error) {
	changed := func(why string) (bool, error) {
		return false, fmt.Errorf("%w: vmid %d %s", errAdoptGuestChanged, target.VMID, why)
	}
	if pve.ConfigFlag(cfg, pveConfigKeyTemplate) {
		return changed("is now a template")
	}
	g := newStemcellRefsGuest(target.VMID, target.Node, target.Name, tags)
	if reason := adoptConflict(&g, plan.DirectorTag, plan.OwnerTag); reason != "" {
		return changed("now conflicts: " + reason)
	}
	if !tagsContainFold(g.DirectorTags, plan.DirectorTag) {
		return changed("no longer carries " + plan.DirectorTag)
	}
	if plan.DeploymentTag != "" && !tagsContainFold(g.DeploymentTags, plan.DeploymentTag) {
		return changed("no longer carries " + plan.DeploymentTag)
	}
	if len(g.OwnerTags) > 0 {
		// adoptConflict already ruled out any owner tag but the plan's.
		return true, nil
	}
	if ok, reason := adoptableGuest(&g); !ok {
		return changed("can no longer be adopted: " + reason)
	}
	return false, nil
}
