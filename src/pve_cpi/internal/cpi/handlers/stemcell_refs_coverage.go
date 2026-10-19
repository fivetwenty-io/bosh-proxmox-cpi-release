package handlers

// stemcell_refs_coverage.go: whether the owned-VM guard of a director prune
// can see every BOSH guest.
//
// The guard counts the VMs a ref owns by the bosh-owner--<uuid> tag, which
// first shipped in 0.9.4. A VM made by an earlier CPI carries no owner tag
// until a 0.9.4 or later set_vm_metadata reaches it, so a live director whose
// VMs predate the tag counts zero owned VMs. Zero then means "we can't tell",
// not "none", so the scan sorts every non-template guest into a kind and
// treats ownership as unknown for every ref while any BOSH guest has no owner
// tag.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Guest kinds a stemcell-refs scan assigns to non-template guests.
const (
	// StemcellRefsGuestParker is a disk parker. It counts as owned by a ref
	// when it carries that ref's director-- tag, and it never makes the
	// coverage incomplete, because a parker records its director in that tag
	// rather than in an owner tag.
	StemcellRefsGuestParker = "parker"
	// StemcellRefsGuestBOSH is any other guest that carries bosh-cpi, a
	// director-- tag, a deployment-- tag, or an owner tag. It counts as owned
	// by a ref when it carries the ref's owner tag.
	StemcellRefsGuestBOSH = "bosh"
	// StemcellRefsGuestOther is a guest BOSH doesn't manage. The guard
	// ignores it.
	StemcellRefsGuestOther = "other"
)

// deploymentTagPrefix starts the deployment tag set_vm_metadata writes.
const deploymentTagPrefix = "deployment--"

// createEnvDirectorName is the director name every bosh create-env sends in
// set_vm_metadata, so every create-env Director VM carries director--bosh-init
// and the name alone can't tell two of them apart.
const createEnvDirectorName = "bosh-init"

// stemcellRefsConfigPath is the placeholder the report prints for the
// operator's CPI config path in the commands it suggests.
const stemcellRefsConfigPath = "<cpi.json>"

// StemcellRefsUncoveredGroup is a set of BOSH guests without an owner tag
// that share a director name and a deployment.
type StemcellRefsUncoveredGroup struct {
	// DirectorName is the value of the guests' director-- tag, "" when they
	// carry none, or several values joined by "," when a guest carries more
	// than one.
	DirectorName string `json:"director_name"`
	// Deployment is the value of the guests' deployment-- tag, with the same
	// conventions as DirectorName.
	Deployment string  `json:"deployment"`
	VMIDs      []int64 `json:"vmids"`
	// Adoptable is true when adopt-owners can stamp these guests: each one
	// carries bosh-cpi and exactly one director-- tag, at most one
	// deployment-- tag, and no stemcell marker.
	Adoptable bool `json:"adoptable"`
	// Commands are the adopt-owners commands that cover the group, empty when
	// the group isn't adoptable.
	Commands []string `json:"commands,omitempty"`
	// UUIDSource says where the operator finds the director UUID for the
	// commands, or why the group can't be adopted.
	UUIDSource string `json:"uuid_source"`
}

// newStemcellRefsGuest builds the scan record for a non-template guest from
// its tag tokens.
func newStemcellRefsGuest(vmid int64, node, name string, tags []string) StemcellRefsGuest {
	g := StemcellRefsGuest{VMID: vmid, Node: node, Name: name, OwnerTags: ownerTagsOf(tags)}
	g.tags = append([]string(nil), tags...)
	for _, tok := range tags {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, directorTagPrefix):
			g.DirectorTags = append(g.DirectorTags, tok)
		case strings.HasPrefix(low, deploymentTagPrefix):
			g.DeploymentTags = append(g.DeploymentTags, tok)
		}
	}
	switch {
	case tagsContainFold(tags, pve.ParkerTag):
		g.Kind = StemcellRefsGuestParker
	case tagsContainFold(tags, ownershipTag) || len(g.DirectorTags) > 0 || len(g.DeploymentTags) > 0 ||
		len(g.OwnerTags) > 0:
		g.Kind = StemcellRefsGuestBOSH
	default:
		g.Kind = StemcellRefsGuestOther
	}
	g.Uncovered = g.Kind == StemcellRefsGuestBOSH && len(g.OwnerTags) == 0
	return g
}

// tagsContainFold reports whether tags hold want, ignoring case.
func tagsContainFold(tags []string, want string) bool {
	for _, tok := range tags {
		if strings.EqualFold(tok, want) {
			return true
		}
	}
	return false
}

// tagSuffixes returns what follows prefix in each token, in order. Every token
// must start with prefix, compared without regard to case.
func tagSuffixes(tokens []string, prefix string) []string {
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, tok[len(prefix):])
	}
	return out
}

// stemcellRefsGuestOwnedBy reports whether g counts as owned by the ref whose
// owner tag is ownerTag and whose director tag is dirTag. A parker counts by
// its director-- tag, in either the rendering registerStemcellDirectorRef uses
// or the one the parker code uses. Any other BOSH guest counts by its owner
// tag.
func stemcellRefsGuestOwnedBy(g *StemcellRefsGuest, ref, ownerTag, dirTag string) bool {
	switch g.Kind {
	case StemcellRefsGuestParker:
		parkerTag := pve.ParkerDirectorTag(ref)
		for _, tok := range g.DirectorTags {
			if (dirTag != directorTagPrefix && strings.EqualFold(tok, dirTag)) ||
				(parkerTag != "" && strings.EqualFold(tok, parkerTag)) {
				return true
			}
		}
		return false
	case StemcellRefsGuestBOSH:
		return ownerTag != "" && stringSliceContains(g.OwnerTags, ownerTag)
	}
	return false
}

// adoptableGuest reports whether adopt-owners may stamp g, which carries no
// owner tag, and the reason when it may not.
func adoptableGuest(g *StemcellRefsGuest) (bool, string) {
	switch {
	case g.Kind != StemcellRefsGuestBOSH:
		return false, "it is not a BOSH guest"
	case !tagsContainFold(g.tags, ownershipTag):
		return false, "it carries no " + ownershipTag + " tag, so the CPI did not create it"
	case backfillExcludedTag(g.tags) != "":
		return false, "it carries the stemcell marker " + backfillExcludedTag(g.tags) +
			", so it is not an instance VM"
	case len(g.DirectorTags) == 0:
		return false, "it carries no director-- tag, so no director name selects it"
	case len(g.DirectorTags) > 1:
		return false, fmt.Sprintf("it carries %d director-- tags (%s), so its director is ambiguous",
			len(g.DirectorTags), strings.Join(g.DirectorTags, ", "))
	case strings.EqualFold(g.DirectorTags[0], directorTagPrefix):
		return false, "its director-- tag has no value"
	case len(g.DeploymentTags) > 1:
		return false, fmt.Sprintf("it carries %d deployment-- tags (%s), so its deployment is ambiguous",
			len(g.DeploymentTags), strings.Join(g.DeploymentTags, ", "))
	}
	return true, ""
}

// groupUncoveredGuests groups the BOSH guests without an owner tag by director
// name, deployment, and whether adopt-owners can stamp them, and attaches the
// commands that cover each group. The groups come back sorted.
func groupUncoveredGuests(guests []StemcellRefsGuest) []StemcellRefsUncoveredGroup {
	type key struct {
		director, deployment string
		adoptable            bool
	}
	groups := map[key]*StemcellRefsUncoveredGroup{}
	reasons := map[key][]string{}
	for i := range guests {
		g := &guests[i]
		if !g.Uncovered {
			continue
		}
		ok, reason := adoptableGuest(g)
		k := key{
			director:   strings.Join(tagSuffixes(g.DirectorTags, directorTagPrefix), ","),
			deployment: strings.Join(tagSuffixes(g.DeploymentTags, deploymentTagPrefix), ","),
			adoptable:  ok,
		}
		grp, seen := groups[k]
		if !seen {
			grp = &StemcellRefsUncoveredGroup{DirectorName: k.director, Deployment: k.deployment, Adoptable: ok}
			groups[k] = grp
		}
		grp.VMIDs = append(grp.VMIDs, g.VMID)
		if !ok {
			reasons[k] = append(reasons[k], fmt.Sprintf("vmid %d: %s", g.VMID, reason))
		}
	}
	out := make([]StemcellRefsUncoveredGroup, 0, len(groups))
	for k, grp := range groups {
		sort.Slice(grp.VMIDs, func(a, b int) bool { return grp.VMIDs[a] < grp.VMIDs[b] })
		if grp.Adoptable {
			grp.Commands, grp.UUIDSource = adoptOwnersCommands(grp)
		} else {
			grp.UUIDSource = "adopt-owners can't stamp these guests (" + strings.Join(reasons[k], "; ") +
				"). A deploy by the owning director on CPI 0.9.4 or later backfills the owner tag through " +
				"set_vm_metadata; otherwise find out what each VM is, and delete it or remove the BOSH tags " +
				"from a VM BOSH doesn't manage"
		}
		out = append(out, *grp)
	}
	sort.Slice(out, func(a, b int) bool {
		switch {
		case out[a].DirectorName != out[b].DirectorName:
			return out[a].DirectorName < out[b].DirectorName
		case out[a].Deployment != out[b].Deployment:
			return out[a].Deployment < out[b].Deployment
		}
		return out[a].Adoptable && !out[b].Adoptable
	})
	return out
}

// adoptOwnersCommands returns the adopt-owners commands that cover an
// adoptable group, and where the director UUID comes from. A named director's
// group needs one command for the whole director. A create-env group needs a
// selector that picks exactly one guest, because every create-env Director VM
// carries director--bosh-init: --deployment when the group holds one guest,
// and one --vmid command per guest otherwise, since two create-env Directors
// can share a deployment name.
func adoptOwnersCommands(grp *StemcellRefsUncoveredGroup) ([]string, string) {
	base := "cpi stemcell-refs adopt-owners --config " + stemcellRefsConfigPath +
		" --director-uuid <director-uuid> --director-name " + grp.DirectorName
	if !strings.EqualFold(grp.DirectorName, createEnvDirectorName) {
		return []string{base}, fmt.Sprintf("<director-uuid> is the UUID that `bosh env` prints for the director "+
			"named %s", grp.DirectorName)
	}
	source := "these are bosh create-env Director VMs, so <director-uuid> is the director_id in the create-env " +
		"state file of the deployment that created each one, not the UUID that `bosh env` prints"
	if len(grp.VMIDs) == 1 && grp.Deployment != "" {
		return []string{base + " --deployment " + grp.Deployment}, source
	}
	cmds := make([]string, 0, len(grp.VMIDs))
	for _, id := range grp.VMIDs {
		cmd := fmt.Sprintf("%s --vmid %d", base, id)
		if grp.Deployment != "" {
			cmd += " --deployment " + grp.Deployment
		}
		cmds = append(cmds, cmd)
	}
	return cmds, source
}

// uncoveredVMIDs returns every VMID in groups, sorted.
func uncoveredVMIDs(groups []StemcellRefsUncoveredGroup) []int64 {
	var ids []int64
	for _, g := range groups {
		ids = append(ids, g.VMIDs...)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

// stemcellRefsCoverageReason explains why ownership is unknown for every ref
// while groups is not empty.
func stemcellRefsCoverageReason(groups []StemcellRefsUncoveredGroup) string {
	ids := uncoveredVMIDs(groups)
	return fmt.Sprintf("ownership is unknown for every ref: %d BOSH guest(s) carry no %s<uuid> tag (%s), so we "+
		"can't rule out that one of them belongs to the director whose ref this is. Run `cpi stemcell-refs report` "+
		"to see them grouped by director and deployment, stamp them with `cpi stemcell-refs adopt-owners`, and "+
		"prune again. --allow-owned-vms does not override this",
		len(ids), ownerTagPrefix, joinVMIDs(ids))
}
