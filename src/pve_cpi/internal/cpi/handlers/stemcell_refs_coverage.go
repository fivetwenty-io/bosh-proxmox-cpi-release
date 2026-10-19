package handlers

// stemcell_refs_coverage.go: whether the owned-VM guard of a director prune
// can see every BOSH guest.
//
// The guard counts the VMs a ref owns by the bosh-owner--<uuid> tag, which
// first shipped in 0.9.4. A VM made by an earlier CPI carries no owner tag
// until something stamps it, so a live director whose VMs predate the tag
// counts zero owned VMs. Zero then means "we can't tell", not "none", so the
// scan sorts every non-template guest into a kind and treats ownership as
// unknown for every ref while any BOSH guest has no owner tag, or while any
// guest's config could not be read.

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
	// coverage incomplete on its own, because a parker records its director
	// in that tag rather than in an owner tag.
	StemcellRefsGuestParker = "parker"
	// StemcellRefsGuestBOSH is any other guest that carries bosh-cpi, a
	// director-- tag, a deployment-- tag, or an owner tag, or whose notes
	// hold the Director metadata lines the ownership guard reads. It counts
	// as owned by a ref when it carries the ref's owner tag.
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

// stemcellRefsListCap bounds how many VMIDs or reasons one line of refusal or
// report text names. The report's groups still list every VMID.
const stemcellRefsListCap = 20

// Advice the report gives for a group adopt-owners can't stamp, by reason.
const (
	adviceReadFailed = "Find out why the config read fails, for example a node that is down or a permission " +
		"the CPI's API token lacks, and run the report again."
	adviceRetag = "Confirm which director runs it. Then recreate the instance with " +
		"`bosh -d <deployment> recreate <instance>` on CPI 0.9.4 or later, which writes the full tag set with " +
		"the owner tag, or add the missing bosh-cpi, director--<name>, and deployment--<deployment> tags in PVE " +
		"and run adopt-owners. If BOSH doesn't manage it, remove its BOSH tags and the Director lines from its " +
		"notes, or delete it."
	adviceAmbiguous = "Confirm which director and deployment run it, remove the director-- or deployment-- tags " +
		"that don't belong to it, and run adopt-owners, or recreate the instance with " +
		"`bosh -d <deployment> recreate <instance>` on CPI 0.9.4 or later."
	adviceStemcellLeftover = "A stemcell marker on a VM that is not a template usually marks a leftover from a " +
		"failed stemcell upload. Confirm that no deployment uses it, then delete it."
	adviceParkerLike = "A director-- tag that holds a UUID is how a disk parker records its director. Confirm " +
		"what the VM is, then restore its bosh-parker tag if it is a parker, or delete it."
	adviceCreateEnvShared = "Recreate this Director with `bosh create-env` on CPI 0.9.4 or later, which stamps " +
		"its owner tag, or delete the VM if that Director is gone."
)

// StemcellRefsUncoveredGroup is a set of guests that leave ownership unknown
// and share a director name, a deployment, and whether adopt-owners can
// stamp them.
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
	// was read, carries bosh-cpi, exactly one director-- tag that names a
	// director, exactly one deployment-- tag, and no stemcell marker, and no
	// other guest makes the pairing check refuse it.
	Adoptable bool `json:"adoptable"`
	// Commands are the adopt-owners commands that cover the group, empty when
	// the group isn't adoptable.
	Commands []string `json:"commands,omitempty"`
	// UUIDSource says where the operator finds the director UUID for the
	// commands, or, for a group adopt-owners can't stamp, why not and what to
	// do instead.
	UUIDSource string `json:"uuid_source"`
}

// newStemcellRefsGuest builds the scan record for a non-template guest from
// its tag tokens, its notes, and the config read error, "" when the config
// was read.
func newStemcellRefsGuest(vmid int64, node, name string, tags []string, desc, readErr string) StemcellRefsGuest {
	g := StemcellRefsGuest{VMID: vmid, Node: node, Name: name, OwnerTags: ownerTagsOf(tags), ReadError: readErr}
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
	g.Instance = instanceOf(tags, desc)
	g.NotesMetadata = hasDirectorMetadataLines(desc)
	g.notesDirector, g.notesDeployment, _ = directorMetadataValues(desc)
	switch {
	case tagsContainFold(tags, pve.ParkerTag):
		g.Kind = StemcellRefsGuestParker
	case tagsContainFold(tags, ownershipTag) || len(g.DirectorTags) > 0 || len(g.DeploymentTags) > 0 ||
		len(g.OwnerTags) > 0 || g.NotesMetadata:
		g.Kind = StemcellRefsGuestBOSH
	default:
		g.Kind = StemcellRefsGuestOther
	}
	g.Uncovered = readErr != "" || (g.Kind == StemcellRefsGuestBOSH && len(g.OwnerTags) == 0)
	return g
}

// instanceOf returns the BOSH instance a guest records as <instance
// group>/<index>, the group alone when the index is unknown, or "" when the
// group is unknown. Each part comes from the guest's tags when they hold
// exactly one value for it: instance-group--, or job-- when that is missing,
// and index--. Otherwise it comes from the instance_group (or job) and index
// lines set_vm_metadata writes into the notes, sanitized the way the tags
// are. A part with several different values is unknown, because the guest
// then doesn't say which instance it is.
func instanceOf(tags []string, desc string) string {
	notes := map[string]string{}
	for _, line := range strings.Split(desc, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
		if value = sanitizeTagValue(strings.TrimSpace(value)); ok && value != "" {
			notes[key] = value
		}
	}
	part := func(tagPrefixes []string, noteKeys []string) string {
		for _, prefix := range tagPrefixes {
			switch vals := tagValues(tags, prefix); len(vals) {
			case 0:
				continue
			case 1:
				return vals[0]
			default:
				return ""
			}
		}
		for _, k := range noteKeys {
			if v := notes[k]; v != "" {
				return v
			}
		}
		return ""
	}
	group := part([]string{"instance-group--", "job--"}, []string{"instance_group", "job"})
	index := part([]string{"index--"}, []string{"index"})
	switch {
	case group == "":
		return ""
	case index == "":
		return group
	}
	return group + "/" + index
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
// owner tag. When it may not, it returns the reason and the advice for the
// way out. It looks at g alone; adoptIndex.createEnvBlock adds the check
// that needs the rest of the cluster.
func adoptableGuest(g *StemcellRefsGuest) (ok bool, reason, advice string) {
	if g.ReadError != "" {
		return false, "its config could not be read (" + g.ReadError + ")", adviceReadFailed
	}
	if g.Kind != StemcellRefsGuestBOSH {
		return false, "it is not a BOSH guest", ""
	}
	if marker := backfillExcludedTag(g.tags); marker != "" {
		return false, "it carries the stemcell marker " + marker + ", so it is not an instance VM", adviceStemcellLeftover
	}
	if !tagsContainFold(g.tags, ownershipTag) {
		if g.NotesMetadata && len(g.DirectorTags) == 0 && len(g.DeploymentTags) == 0 {
			return false, fmt.Sprintf("only the Director lines in its notes mark it as a BOSH VM (director %s, "+
				"deployment %s), and it carries no %s tag", g.notesDirector, g.notesDeployment, ownershipTag), adviceRetag
		}
		return false, "it carries no " + ownershipTag + " tag, so adopt-owners can't tell that the CPI created it",
			adviceRetag
	}
	return adoptableGuestTags(g)
}

// adoptableGuestTags is adoptableGuest's check of the director-- and
// deployment-- tags.
func adoptableGuestTags(g *StemcellRefsGuest) (ok bool, reason, advice string) {
	switch {
	case len(g.DirectorTags) == 0:
		return false, "it carries no director-- tag, so no director name selects it", adviceRetag
	case len(g.DirectorTags) > 1:
		return false, fmt.Sprintf("it carries %d director-- tags (%s), so its director is ambiguous",
			len(g.DirectorTags), strings.Join(g.DirectorTags, ", ")), adviceAmbiguous
	case strings.EqualFold(g.DirectorTags[0], directorTagPrefix):
		return false, "its director-- tag has no value", adviceRetag
	case directorUUIDValue.MatchString(g.DirectorTags[0][len(directorTagPrefix):]):
		return false, "its director-- tag holds a UUID rather than a director name", adviceParkerLike
	case len(g.DeploymentTags) == 0:
		return false, "it carries no deployment-- tag, so no --deployment selects it", adviceRetag
	case len(g.DeploymentTags) > 1:
		return false, fmt.Sprintf("it carries %d deployment-- tags (%s), so its deployment is ambiguous",
			len(g.DeploymentTags), strings.Join(g.DeploymentTags, ", ")), adviceAmbiguous
	case strings.EqualFold(g.DeploymentTags[0], deploymentTagPrefix):
		return false, "its deployment-- tag has no value", adviceRetag
	}
	return true, "", ""
}

// tagValueKey is the lowercased value after prefix in tok, the form the
// coverage code compares director and deployment names in.
func tagValueKey(tok, prefix string) string {
	return strings.ToLower(tok[len(prefix):])
}

// adoptNameInfo is what the cluster says about one director name.
type adoptNameInfo struct {
	// owners holds every owner tag value on a guest with the name.
	owners map[string]bool
	// deployments holds every deployment value on a guest with the name.
	deployments map[string]bool
	// ownersByDeployment holds the owner tag values per deployment value.
	ownersByDeployment map[string]map[string]bool
	// adoptableDeployments holds the deployments of the name's uncovered
	// guests that adopt-owners can stamp.
	adoptableDeployments map[string]bool
}

// adoptIndex is the cluster-wide view the report uses to suggest adopt-owners
// commands that the pairing checks accept.
type adoptIndex struct {
	byName map[string]*adoptNameInfo
	// otherUncovered counts the uncovered guests that don't carry
	// director--bosh-init, which keep adopt-owners from adopting a
	// create-env Director.
	otherUncovered int
}

func newAdoptIndex(guests []StemcellRefsGuest) *adoptIndex {
	idx := &adoptIndex{byName: map[string]*adoptNameInfo{}}
	for i := range guests {
		g := &guests[i]
		if g.Uncovered && !tagsContainFold(g.DirectorTags, directorTagPrefix+createEnvDirectorName) {
			idx.otherUncovered++
		}
		for _, d := range g.DirectorTags {
			info := idx.name(tagValueKey(d, directorTagPrefix))
			for _, dep := range g.DeploymentTags {
				depKey := tagValueKey(dep, deploymentTagPrefix)
				info.deployments[depKey] = true
				if info.ownersByDeployment[depKey] == nil {
					info.ownersByDeployment[depKey] = map[string]bool{}
				}
				for _, o := range g.OwnerTags {
					info.ownersByDeployment[depKey][o[len(ownerTagPrefix):]] = true
				}
			}
			for _, o := range g.OwnerTags {
				info.owners[o[len(ownerTagPrefix):]] = true
			}
		}
	}
	return idx
}

func (idx *adoptIndex) name(key string) *adoptNameInfo {
	info, ok := idx.byName[key]
	if !ok {
		info = &adoptNameInfo{owners: map[string]bool{}, deployments: map[string]bool{},
			ownersByDeployment: map[string]map[string]bool{}, adoptableDeployments: map[string]bool{}}
		idx.byName[key] = info
	}
	return info
}

// createEnvBlock returns why adopt-owners would refuse g, a create-env
// Director VM that adoptableGuest accepts, or "". Another create-env VM in
// the same deployment that carries an owner tag refuses any UUID: a
// different one fails the one-director-per-deployment check, and the same
// one fails the rule that a director_id owns exactly one VM.
func (idx *adoptIndex) createEnvBlock(g *StemcellRefsGuest) string {
	info := idx.byName[createEnvDirectorName]
	if info == nil {
		return ""
	}
	depKey := tagValueKey(g.DeploymentTags[0], deploymentTagPrefix)
	owners := sortedKeys(info.ownersByDeployment[depKey])
	if len(owners) == 0 {
		return ""
	}
	return fmt.Sprintf("another create-env Director VM in deployment %s already carries the owner tag %s%s, "+
		"so adopt-owners refuses every UUID for this one", depKey, ownerTagPrefix, strings.Join(owners, ", "+ownerTagPrefix))
}

// groupUncoveredGuests groups the guests that leave ownership unknown by
// director name, deployment, and whether adopt-owners can stamp them, and
// attaches the commands or the advice for each group. The groups come back
// sorted.
func groupUncoveredGuests(guests []StemcellRefsGuest) []StemcellRefsUncoveredGroup {
	type key struct {
		director, deployment string
		adoptable            bool
	}
	idx := newAdoptIndex(guests)
	groups := map[key]*StemcellRefsUncoveredGroup{}
	reasons := map[key][]string{}
	advice := map[key][]string{}
	for i := range guests {
		g := &guests[i]
		if !g.Uncovered {
			continue
		}
		ok, reason, adv := adoptableGuest(g)
		if ok && strings.EqualFold(tagValueKey(g.DirectorTags[0], directorTagPrefix), createEnvDirectorName) {
			if block := idx.createEnvBlock(g); block != "" {
				ok, reason, adv = false, block, adviceCreateEnvShared
			}
		}
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
		if ok {
			idx.name(tagValueKey(g.DirectorTags[0], directorTagPrefix)).
				adoptableDeployments[tagValueKey(g.DeploymentTags[0], deploymentTagPrefix)] = true
			continue
		}
		reasons[k] = append(reasons[k], fmt.Sprintf("vmid %d: %s", g.VMID, reason))
		if adv != "" && !stringSliceContains(advice[k], adv) {
			advice[k] = append(advice[k], adv)
		}
	}
	out := make([]StemcellRefsUncoveredGroup, 0, len(groups))
	for k, grp := range groups {
		sort.Slice(grp.VMIDs, func(a, b int) bool { return grp.VMIDs[a] < grp.VMIDs[b] })
		if grp.Adoptable {
			grp.Commands, grp.UUIDSource = idx.commands(grp)
		} else {
			grp.UUIDSource = "adopt-owners can't stamp these guests (" + capJoin(reasons[k], "; ") + "). " +
				strings.Join(advice[k], " ")
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

// commands returns the adopt-owners commands that cover an adoptable group,
// and where the director UUID comes from.
//
// No command ever carries a UUID. An owner tag on some of the name's guests
// doesn't prove whose the uncovered ones are, because two directors can share
// a name, so the command prints the placeholder and the note says where the
// operator reads the value.
//
// A named director gets one command, the same on each of its groups, because
// the pairing check refuses a UUID that already sits on a deployment the run
// doesn't name. When the name's guests carry no owner tag yet, the command
// names every deployment of the name's adoptable guests. When they carry one
// owner tag, the command names every deployment of the name, and the note
// says which deployments carry that tag without claiming the uncovered guests
// belong to it. When they carry several, two directors share the name, and
// the command names only the group's deployment.
//
// A create-env Director VM gets one command per VM, with --vmid and
// --deployment, because every create-env VM carries director--bosh-init and
// two of them can share a deployment name.
func (idx *adoptIndex) commands(grp *StemcellRefsUncoveredGroup) ([]string, string) {
	nameKey := strings.ToLower(grp.DirectorName)
	info := idx.name(nameKey)
	cmd := func(uuid string, deployments []string) string {
		c := "cpi stemcell-refs adopt-owners --config " + stemcellRefsConfigPath + " --director-uuid " + uuid +
			" --director-name " + grp.DirectorName
		for _, d := range deployments {
			c += " --deployment " + d
		}
		return c
	}
	if nameKey == createEnvDirectorName {
		source := "These are bosh create-env Director VMs, so <director_id> is the director_id in the create-env " +
			"state file of the deployment that created each one, not the UUID that `bosh env` prints."
		if idx.otherUncovered > 0 {
			source += " adopt-owners refuses bosh-init while any other guest leaves ownership unknown, so deal " +
				"with the other groups first."
		}
		cmds := make([]string, 0, len(grp.VMIDs))
		for _, id := range grp.VMIDs {
			cmds = append(cmds, fmt.Sprintf("%s --vmid %d", cmd("<director_id>", []string{grp.Deployment}), id))
		}
		return cmds, source
	}
	tag := directorTagPrefix + grp.DirectorName
	owners := sortedKeys(info.owners)
	switch {
	case len(owners) == 0:
		return []string{cmd("<director-uuid>", sortedKeys(info.adoptableDeployments))}, fmt.Sprintf(
			"<director-uuid> is the UUID that `bosh -e <env> env` prints for the director whose VMs carry %s. "+
				"The command names every deployment whose uncovered VMs carry that tag. Compare them with "+
				"`bosh -e <env> deployments` on that director, drop any it doesn't list, and add any it lists that "+
				"are missing, because two directors can share a name.", tag)
	case len(owners) == 1:
		var owned []string
		for _, dep := range sortedKeys(info.deployments) {
			if info.ownersByDeployment[dep][owners[0]] {
				owned = append(owned, dep)
			}
		}
		return []string{cmd("<director-uuid>", sortedKeys(info.deployments))}, fmt.Sprintf(
			"<director-uuid> is the UUID that `bosh -e <env> env` prints for the director whose "+
				"`bosh -e <env> deployments` lists these deployments. The command names every deployment whose VMs "+
				"carry %s. VMs in deployment(s) %s already carry %s%s, which doesn't prove whose the uncovered VMs "+
				"are, because two directors can share a name. Compare the deployments with `bosh -e <env> "+
				"deployments` on the director you adopt for and drop any it doesn't list; adopt-owners refuses a "+
				"UUID that contradicts an owner tag on any of them.",
			tag, strings.Join(owned, ", "), ownerTagPrefix, owners[0])
	}
	return []string{cmd("<director-uuid>", []string{grp.Deployment})}, fmt.Sprintf(
		"VMs tagged %s carry the owner tags of %d directors (%s), so more than one director uses the name. Use "+
			"the UUID of the director whose `bosh -e <env> deployments` lists %s, and name every deployment that "+
			"director lists.", tag, len(owners), strings.Join(owners, ", "), grp.Deployment)
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

// capVMIDs renders ids, naming at most stemcellRefsListCap of them.
func capVMIDs(ids []int64) string {
	if len(ids) <= stemcellRefsListCap {
		return joinVMIDs(ids)
	}
	return fmt.Sprintf("%s, and %d more", joinVMIDs(ids[:stemcellRefsListCap]), len(ids)-stemcellRefsListCap)
}

// capJoin joins items with sep, naming at most stemcellRefsListCap of them.
func capJoin(items []string, sep string) string {
	if len(items) <= stemcellRefsListCap {
		return strings.Join(items, sep)
	}
	return fmt.Sprintf("%s%sand %d more", strings.Join(items[:stemcellRefsListCap], sep), sep,
		len(items)-stemcellRefsListCap)
}

// stemcellRefsCoverageReason explains why ownership is unknown for every ref
// while groups is not empty. It names at most stemcellRefsListCap VMIDs,
// because it is repeated for every ref, and the report lists them all.
func stemcellRefsCoverageReason(groups []StemcellRefsUncoveredGroup) string {
	ids := uncoveredVMIDs(groups)
	return fmt.Sprintf("ownership is unknown for every ref: %d guest(s) are BOSH guests with no %s<uuid> tag or "+
		"guests whose config the scan could not read (%s), so we can't rule out that one of them belongs to the "+
		"director whose ref this is. Run `cpi stemcell-refs report` to see them grouped by director and "+
		"deployment, stamp them with `cpi stemcell-refs adopt-owners` or follow the report's advice, and prune "+
		"again. --allow-owned-vms does not override this",
		len(ids), ownerTagPrefix, capVMIDs(ids))
}
