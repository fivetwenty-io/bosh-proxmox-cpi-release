package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// The reasons ownVM gives for a VM this Director must not act on. They appear
// in refusals and logs, so an operator can grep for them.
const (
	// ownershipReasonClusterMismatch: the CID's cluster stamp disagrees with
	// the VM's cluster tag, or the VM has no cluster tag and the live cluster
	// identity could not be proven to match the stamp.
	ownershipReasonClusterMismatch = "cluster-mismatch"
	// ownershipReasonTokenMismatch: the cluster is proven the same and the VM
	// carries a different per-VM token, so the VM the CID named is gone and
	// this VMID has been reused.
	ownershipReasonTokenMismatch = "token-mismatch"
	// ownershipReasonTokenMissing: the CID carries a token and the VM carries
	// no token tag. That may be our own VM after a tag edit, so it is never
	// answered as absence.
	ownershipReasonTokenMissing = "token-missing"
	// ownershipReasonTokenConflict: the VM carries several token tags and only
	// some of them match, which no VM the CPI created ever does.
	ownershipReasonTokenConflict = "token-conflict"
	ownershipReasonParker        = "parker"
	ownershipReasonTemplate      = "template"
	// ownershipReasonOtherDirector: an owner tag names a Director other than
	// the one making this request, or the VM carries two different owners.
	ownershipReasonOtherDirector = "other-director"
	// ownershipReasonNotBOSH: the VM carries no BOSH marker at all, or the
	// VMID names a container rather than a QEMU VM.
	ownershipReasonNotBOSH = "not-bosh"
)

// vmOwnershipVerdict is ownVM's answer. At most one of Owned, Gone, and
// Absent is true, and none is when the VM is foreign. Reason and Detail are
// empty for an owned VM and for an absent one.
type vmOwnershipVerdict struct {
	// Owned is true when this Director may act on the VM.
	Owned bool
	// Legacy is true for an owned VM that carries no owner tag and was
	// accepted on its bosh-cpi tag or its Director metadata lines, which only
	// a bare CID can reach.
	Legacy bool
	// Gone is true for a token mismatch on a proven cluster: the VM the CID
	// named no longer exists, and another VM now holds its VMID.
	Gone bool
	// Absent is true when the config read on the located node answered
	// not-found and two sweeps of every member's config then proved the VM
	// exists nowhere. A member the cluster reports offline leaves absence
	// unproven. Only checkVMOwnership and absentVMVerdict set it, never ownVM,
	// and each handler gives it the answer it already gives a VM it could not
	// locate.
	Absent bool
	// Elsewhere is true for a foreign verdict on a VMID that is absent from
	// this cluster, when the CID carries a cluster stamp that is neither this
	// cluster's live stamp nor one of its declared aliases. The CID's VM may
	// still run on the cluster the stamp names, so no handler may answer that
	// it is gone. Only absentVMVerdict sets it.
	Elsewhere bool
	// Reason is one of the ownershipReason constants.
	Reason string
	// Detail names both identities in one clause, for refusals and logs. It
	// carries tag values and Director UUIDs, never credentials.
	Detail string
}

// foreign reports whether the verdict forbids acting on the VM while still
// meaning it exists, which is everything that is neither owned nor gone.
func (v vmOwnershipVerdict) foreign() bool { return !v.Owned && !v.Gone && !v.Absent }

// vmOwnershipView is what ownVM reads about a VM. The guard builds it from the
// scan row the handler already holds and reads the config only when the row
// cannot settle the answer.
type vmOwnershipView struct {
	// Tags is the VM's tag list, lowercased.
	Tags []string
	// Template is the PVE template flag.
	Template bool
	// Type is the resource type from the scan row ("qemu" or "lxc"), or ""
	// when the caller does not know it.
	Type string
	// description returns the VM's notes. ownVM calls it only when the
	// verdict depends on them, so a VM with an owner or bosh-cpi tag never
	// costs a read.
	description func() (string, error)
	// clusterMatches reports whether a CID's cluster stamp names the cluster
	// this request talks to, for a VM that carries no cluster tag.
	// checkVMOwnership sets it to a clusterStampProbe, which reads the live
	// identity at most once per check. A failed read is an error, never a
	// verdict. nil means no live identity is available, and the stamp is then
	// treated as a mismatch.
	clusterMatches func(stamp string) (bool, error)
}

// ownVM decides whether the Director making a request owns the VM a CID
// reached. The checks run in a fixed order, and the first one that fails
// decides:
//
//  1. A stamped CID's cluster must match the VM's bosh-cluster-- tag, or, when
//     the VM has none, the live cluster identity.
//  2. On a proven cluster, a different bosh-cid-- token means the CID's VM is
//     gone, and no token tag at all means the VM is foreign.
//  3. A container, a parker, and a stemcell template are foreign.
//  4. An owner tag naming another Director is foreign, and so are two
//     different owner tags unless both name the requester. With no request
//     UUID a single owner tag passes, because there is nothing to compare.
//  5. A VM with no owner tag, no bosh-cpi tag, and no Director metadata lines
//     in its notes is foreign.
//
// Anything left is owned. It is legacy when it carries no owner tag, which a
// stamped CID never reaches as legacy, because its token already matched. A
// bosh-deleting VM gets no special treatment, so another Director's
// mid-delete VM stays foreign.
//
// The only error is a failure to read the notes or to prove the cluster
// identity. Both are retriable, because nothing has been decided yet. The
// cluster identity error is classed here, and the notes reader classes its
// own.
func ownVM(cid vmcid.VMCID, requestUUID string, vm vmOwnershipView) (vmOwnershipVerdict, error) {
	clusterTags := tagValues(vm.Tags, clusterStampTagPrefix)
	tokenTags := tagValues(vm.Tags, cidStampTagPrefix)
	owners := tagValues(vm.Tags, ownerTagPrefix)
	request := strings.ToLower(sanitizeTagValue(requestUUID))

	if cid.Stamped() {
		if v, done, err := ownVMStamp(cid, clusterTags, tokenTags, vm.clusterMatches); done || err != nil {
			return v, err
		}
	}

	if vm.Type != "" && vm.Type != resourceTypeQemu {
		return foreignVerdict(ownershipReasonNotBOSH,
			fmt.Sprintf("the VMID names a %s guest, not a QEMU VM", vm.Type)), nil
	}
	if contains(vm.Tags, pve.ParkerTag) {
		return foreignVerdict(ownershipReasonParker, "the VM is a disk parker"), nil
	}
	if vm.Template || contains(vm.Tags, stemcellCacheTag) || contains(vm.Tags, stemcellMarkerTag) {
		return foreignVerdict(ownershipReasonTemplate, "the VM is a stemcell template"), nil
	}
	if otherOwner(owners, request) {
		return foreignVerdict(ownershipReasonOtherDirector, fmt.Sprintf(
			"the VM is owned by Director %s, and this request comes from Director %s",
			strings.Join(owners, " and "), orNone(request))), nil
	}
	if len(owners) > 0 {
		return vmOwnershipVerdict{Owned: true}, nil
	}

	if !contains(vm.Tags, ownershipTag) {
		desc, err := vm.description()
		if err != nil {
			return vmOwnershipVerdict{}, err
		}
		if !hasDirectorMetadataLines(desc) {
			return foreignVerdict(ownershipReasonNotBOSH,
				"the VM carries no owner tag, no bosh-cpi tag, and no Director metadata in its notes"), nil
		}
	}
	return vmOwnershipVerdict{Owned: true, Legacy: !cid.Stamped()}, nil
}

// ownVMStamp runs the cluster and token checks for a stamped CID. done is
// true when they decided the verdict, and false when the stamp matched and
// the remaining checks still apply.
func ownVMStamp(cid vmcid.VMCID, clusterTags, tokenTags []string, clusterMatches func(string) (bool, error)) (vmOwnershipVerdict, bool, error) {
	if len(clusterTags) > 0 {
		if !allEqual(clusterTags, cid.Cluster) {
			// pve.cluster_stamp_aliases plays no part here. The VM's own tag
			// records the cluster it was created on, and a stamp never
			// changes after create, so an alias can't make the two agree.
			return foreignVerdict(ownershipReasonClusterMismatch, fmt.Sprintf(
				"the CID names cluster %s, and the VM records cluster %s. pve.cluster_stamp_aliases doesn't change "+
					"this answer, because the VM's own tag records its cluster",
				cid.Cluster, strings.Join(clusterTags, " and "))), true, nil
		}
	} else {
		if clusterMatches == nil {
			return foreignVerdict(ownershipReasonClusterMismatch, fmt.Sprintf(
				"the CID names cluster %s, and the VM records no cluster, so the cluster cannot be proven", cid.Cluster)), true, nil
		}
		same, err := clusterMatches(cid.Cluster)
		if err != nil {
			return vmOwnershipVerdict{}, true, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("prove the identity of cluster %s for VM %d", cid.Cluster, cid.VMID))
		}
		if !same && cid.Cluster == vmcid.UnknownCluster {
			return foreignVerdict(ownershipReasonClusterMismatch, fmt.Sprintf(
				"the CID carries the unknown cluster stamp %s, and the VM records no cluster, so the cluster cannot be proven",
				cid.Cluster)), true, nil
		}
		if !same {
			return foreignVerdict(ownershipReasonClusterMismatch, fmt.Sprintf(
				"the CID names cluster %s, and the VM records no cluster on a cluster with a different identity", cid.Cluster)), true, nil
		}
	}

	switch {
	case len(tokenTags) == 0:
		return foreignVerdict(ownershipReasonTokenMissing, fmt.Sprintf(
			"the CID carries token %s, and the VM carries no token tag", cid.Token)), true, nil
	case allEqual(tokenTags, cid.Token):
		return vmOwnershipVerdict{}, false, nil
	case contains(tokenTags, cid.Token):
		return foreignVerdict(ownershipReasonTokenConflict, fmt.Sprintf(
			"the CID carries token %s, and the VM carries tokens %s", cid.Token, strings.Join(tokenTags, " and "))), true, nil
	default:
		return vmOwnershipVerdict{Gone: true, Reason: ownershipReasonTokenMismatch, Detail: fmt.Sprintf(
			"the CID carries token %s, and the VM at this VMID carries token %s", cid.Token, strings.Join(tokenTags, " and "))}, true, nil
	}
}

func foreignVerdict(reason, detail string) vmOwnershipVerdict {
	return vmOwnershipVerdict{Reason: reason, Detail: detail}
}

// otherOwner reports whether the owner tag values name a Director other than
// request. Two different owners are foreign even when one of them is the
// requester, because a VM the CPI created carries exactly one, and they are
// foreign when there is no request UUID either, since nothing says which of
// the two is asking.
func otherOwner(owners []string, request string) bool {
	switch {
	case len(owners) == 0:
		return false
	case len(owners) > 1:
		return true
	case request == "":
		return false
	default:
		return owners[0] != request
	}
}

// tagValues returns the sorted, distinct values of every tag that starts with
// prefix, compared without regard to case.
func tagValues(tags []string, prefix string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range tags {
		if !strings.HasPrefix(t, prefix) {
			continue
		}
		v := strings.TrimPrefix(t, prefix)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func allEqual(values []string, want string) bool {
	for _, v := range values {
		if v != want {
			return false
		}
	}
	return len(values) > 0
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// hasDirectorMetadataLines reports whether a VM's notes hold the lines
// set_vm_metadata writes from the Director's metadata: a "director:" line, a
// "deployment:" line, and a "name: <job>/<id>" line, each with a value. Both
// this CPI's buildDescription and the legacy Perl CPI write them as plain
// "key: value" lines. The <!--BOSH: sentinel does not count, because parkers
// and templates carry it too.
func hasDirectorMetadataLines(desc string) bool {
	var director, deployment, name bool
	for _, line := range strings.Split(desc, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		switch key {
		case "director":
			director = true
		case "deployment":
			deployment = true
		case "name":
			job, id, slash := strings.Cut(strings.TrimSpace(value), "/")
			name = slash && job != "" && id != ""
		}
	}
	return director && deployment && name
}

// vmOwnershipGuardWarns reports whether this request's ownership guard runs in
// "warn", which relaxes only has_vm and get_disks.
func vmOwnershipGuardWarns(deps Deps) bool {
	return deps.Config.VMOwnershipGuardValue() == config.VMOwnershipGuardWarn
}

// checkVMOwnership runs ownVM for a VM the handler has located. cfg is the VM
// config when the handler already read it, or nil. A config the caller read
// is node-local truth, so its tags and template flag decide the verdict and
// the scan row's are ignored. The row can be minutes stale, and a VMID that
// another Director's create_vm reused after our VM was destroyed still shows
// our tags there. Without a config, a row whose tags make the VM ours stands,
// so the common case costs no read. The config is read here when the row
// carried no tags, which PVE does for an untagged VM and for a row it did not
// populate, when the verdict depends on the notes, and when the row says the
// VM is not ours, because that row can be the stale one and the config then
// decides. A location that FindVMAuthoritative took from the config probe
// already carries the config's tags and template flag, so it is judged as it
// stands. A container is judged from its row alone, because the QEMU config
// endpoint cannot answer for it.
//
// A read that answers not-found does not end the check, because the VM may
// have moved since the scan, and the row may name a VM that is gone. The VM is
// located again by probing every member's config (readAtVMNode). Proven
// absence gives an Absent verdict, and a hit on another node is judged from
// that node's config. Any other failed read, and a probe that proves nothing,
// is a retriable error and never a verdict.
func checkVMOwnership(ctx context.Context, deps Deps, op string, cid vmcid.VMCID, loc pve.VMLocation, cfg map[string]any) (vmOwnershipVerdict, error) {
	readConfig := func() (map[string]any, error) {
		if cfg != nil {
			return cfg, nil
		}
		if deps.PVE == nil || deps.PVE.QEMU() == nil {
			return nil, cpierrors.Retriable("%s: cannot read VM %d config to check its ownership", op, cid.VMID)
		}
		read, at, err := readAtVMNode(ctx, deps, op, cid.VMID, loc.Node, func(node string) (map[string]any, error) {
			return deps.PVE.QEMU().Config(ctx, node, cid.VMID)
		})
		if err != nil {
			return nil, err
		}
		if read == nil {
			read = map[string]any{}
		}
		loc.Node = at
		cfg = read
		return cfg, nil
	}

	qemu := loc.Type == "" || loc.Type == resourceTypeQemu
	// A stamped CID that reaches a VM with no cluster tag is judged against
	// the live cluster identity, read at most once per check and only then.
	stamps := newClusterStampProbe(ctx, deps, loc.Node)
	judge := func(tagsRaw string, template bool) (vmOwnershipVerdict, error) {
		view := vmOwnershipView{
			Tags:           lowerTags(parseTagsField(tagsRaw)),
			Template:       template,
			Type:           loc.Type,
			clusterMatches: stamps.matches,
			description: func() (string, error) {
				if !qemu {
					return "", nil
				}
				read, err := readConfig()
				if err != nil {
					return "", err
				}
				return pve.DescriptionFromConfig(read), nil
			},
		}
		return ownVM(cid, deps.RequestDirectorUUID, view)
	}
	// judgeConfig judges a config alone. Its template flag is authoritative,
	// so the row's flag never counts once the config has been read.
	judgeConfig := func(read map[string]any) (vmOwnershipVerdict, error) {
		tagsRaw, _ := pve.ConfigString(read, jsonKeyTags)
		return judge(tagsRaw, pve.ConfigFlag(read, "template"))
	}

	verdict, err := func() (vmOwnershipVerdict, error) {
		switch {
		case !qemu, loc.FromConfig && cfg == nil:
			return judge(loc.Tags, loc.Template)
		case cfg != nil:
			return judgeConfig(cfg)
		case strings.TrimSpace(loc.Tags) == "":
			read, err := readConfig()
			if err != nil {
				return vmOwnershipVerdict{}, err
			}
			return judgeConfig(read)
		}

		// The row's tags answer for the common case, which is our own VM and
		// no read. A row that says the VM is not ours can be the stale one,
		// though, because a VMID our create_vm just took from a destroyed VM
		// of another Director still shows that Director's tags there. So we
		// read the config once and let it decide before anything is refused
		// or answered as gone.
		verdict, err := judge(loc.Tags, loc.Template)
		if err != nil || verdict.Owned {
			return verdict, err
		}
		read, err := readConfig()
		if err != nil {
			return vmOwnershipVerdict{}, err
		}
		return judgeConfig(read)
	}()
	if errors.Is(err, errVMAbsent) {
		return vmOwnershipVerdict{Absent: true}, nil
	}
	return verdict, err
}

// errVMAbsent is readAtVMNode's answer when the VM's node answered not-found
// and two sweeps of every member's config proved the VM exists nowhere.
// checkVMOwnership and checkVMOwnershipByConfig turn it into an Absent
// verdict. detachDiskJudgeVM returns it unwrapped to detachDiskResolveSlot,
// which matches it with errors.Is to answer already detached, so a caller on
// that path must keep its identity and never wrap it.
var errVMAbsent = errors.New("the VM exists on no cluster member")

// readAtVMNode runs one of the guard's reads, a config or the config views,
// on the node the handler located the VM on, and returns the result with the
// node it came from. A not-found or pmxcfs config-missing answer there does
// not prove the VM is gone, because it may have migrated since the scan, and
// the scan row may be the stale one. So the VM is located again with
// pve.FindVMByConfigProbe, which skips the index row that was just
// contradicted. Proven absence returns errVMAbsent, and a hit runs the read
// once more on the node it names. Every other failure is retriable,
// including a member the cluster reports offline, which the probe cannot
// read and so cannot count as absent.
func readAtVMNode[T any](ctx context.Context, deps Deps, op string, vmid int, node string, read func(node string) (T, error)) (T, string, error) {
	var zero T
	got, err := read(node)
	if err == nil {
		return got, node, nil
	}
	if !pve.IsNotFound(err) && !pve.IsPmxcfsConfigMissing(err) {
		return zero, node, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read VM %d config to check its ownership", op, vmid))
	}
	moved, lookErr := pve.FindVMByConfigProbe(ctx, deps.PVE, vmid)
	if lookErr != nil {
		return zero, node, cpierrors.WrapAs(lookErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: VM %d config is gone from node %s, and locating the VM again failed", op, vmid, node))
	}
	if !moved.Found || moved.Node == "" {
		deps.Log(ctx).Info(op+": VM config is gone from its node, and no member holds the VM",
			log.Int("vmid", vmid), log.String("node", node))
		return zero, node, errVMAbsent
	}
	got, err = read(moved.Node)
	if err != nil {
		return zero, node, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read VM %d config on node %s, where it moved, to check its ownership", op, vmid, moved.Node))
	}
	return got, moved.Node, nil
}

func lowerTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, strings.ToLower(t))
	}
	return out
}

// foreignVMError is the non-retriable refusal a destroy or mutation returns
// for a VM this Director does not own. It names the reason and both
// identities, and the escapes for a record that can never be acted on.
func foreignVMError(op, vmCID string, vmid int, v vmOwnershipVerdict) error {
	if v.Elsewhere {
		return cpierrors.Cloud(
			"%s: refusing to answer for CID %s, because VM %d does not exist on this cluster and the CID names another "+
				"cluster (%s): %s. The VM may still run on the cluster the CID names, for example after the request's "+
				"cpi_config entry was pointed at a different cluster. Point the entry back at that cluster, or, if this "+
				"cluster's root CA was replaced, list the old stamp in pve.cluster_stamp_aliases. If the VM is really gone, "+
				"drop the record with cloud-check's \"Delete VM reference\" or bosh delete-deployment --force",
			op, vmCID, vmid, v.Reason, v.Detail)
	}
	return cpierrors.Cloud(
		"%s: refusing to act on VM %d for CID %s, which this Director does not own (%s): %s. "+
			"If the Director's record is stale, drop it with cloud-check's \"Delete VM reference\" or bosh delete-deployment --force",
		op, vmid, vmCID, v.Reason, v.Detail)
}

// logForeignVM records a foreign verdict at error level with the reason and
// both identities. Every field is a tag value, a VMID, or a Director UUID.
func logForeignVM(logger *log.Logger, msg string, v vmOwnershipVerdict) {
	logger.Error(msg,
		log.String("ownership_reason", v.Reason),
		log.String("ownership_detail", v.Detail),
	)
}

// refuseUnownedVM is the guard for the handlers that act on a VM:
// delete_vm's legacy path, reboot_vm, set_vm_metadata, attach_disk,
// detach_disk, and snapshot_disk. nil means the VM is ours, legacy or not, and
// the caller proceeds. A VM the check found absent is judged once more by
// absentVMVerdict, so a stamped CID that names another cluster is refused
// rather than answered as gone. See refuseVerdict for the other answers.
func refuseUnownedVM(ctx context.Context, deps Deps, op, vmCID string, cid vmcid.VMCID, loc pve.VMLocation, cfg map[string]any, logger *log.Logger) error {
	verdict, err := checkVMOwnership(ctx, deps, op, cid, loc, cfg)
	if err == nil && verdict.Absent {
		verdict, err = absentVMVerdict(ctx, deps, op, cid)
	}
	if err != nil {
		return err
	}
	return refuseVerdict(op, vmCID, cid, verdict, logger)
}

// refuseVerdict maps a verdict to a mutating handler's answer. A token
// mismatch answers VMNotFound, because the VM the CID named is gone, and so
// does proven absence. Every other foreign verdict is a non-retriable refusal
// in both guard modes, because warn relaxes only the two read answers.
func refuseVerdict(op, vmCID string, cid vmcid.VMCID, verdict vmOwnershipVerdict, logger *log.Logger) error {
	switch {
	case verdict.Absent:
		logger.Info(op + ": the VM is gone from every cluster member — answering VMNotFound")
		return cpierrors.VMNotFound(vmCID)
	case verdict.Gone:
		logger.Info(op+": the VMID now holds a different VM than the CID names — answering VMNotFound",
			log.String("ownership_reason", verdict.Reason), log.String("ownership_detail", verdict.Detail))
		return cpierrors.VMNotFound(vmCID)
	case verdict.foreign():
		logForeignVM(logger, op+": refusing a VM this Director does not own", verdict)
		return foreignVMError(op, vmCID, cid.VMID, verdict)
	}
	return nil
}

// checkVMOwnershipByConfig is the verdict for a mutation that holds no config
// yet when it must decide. A row that still shows our tags can be the stale
// one after another Director's create_vm reused the VMID, and the handler
// writes to the VM's config next, so the config decides before the first
// write. A location that FindVMAuthoritative took from the config probe
// already is that config, so it is judged with no read. A container keeps its
// row verdict, because the QEMU config endpoint cannot answer for it. The
// read follows checkVMOwnership's rules, so a not-found locates the VM again
// and a failed read is retriable.
func checkVMOwnershipByConfig(ctx context.Context, deps Deps, op string, cid vmcid.VMCID, loc pve.VMLocation) (vmOwnershipVerdict, error) {
	if (loc.Type != "" && loc.Type != resourceTypeQemu) || loc.FromConfig {
		return checkVMOwnership(ctx, deps, op, cid, loc, nil)
	}
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return vmOwnershipVerdict{}, cpierrors.Retriable("%s: cannot read VM %d config to check its ownership", op, cid.VMID)
	}
	cfg, at, err := readAtVMNode(ctx, deps, op, cid.VMID, loc.Node, func(node string) (map[string]any, error) {
		return deps.PVE.QEMU().Config(ctx, node, cid.VMID)
	})
	if errors.Is(err, errVMAbsent) {
		return vmOwnershipVerdict{Absent: true}, nil
	}
	if err != nil {
		return vmOwnershipVerdict{}, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	loc.Node = at
	return checkVMOwnership(ctx, deps, op, cid, loc, cfg)
}

// refuseUnownedVMByConfig is refuseUnownedVM for a mutation that holds no
// config yet, which is attach_disk. See checkVMOwnershipByConfig.
func refuseUnownedVMByConfig(ctx context.Context, deps Deps, op, vmCID string, cid vmcid.VMCID, loc pve.VMLocation, logger *log.Logger) error {
	verdict, err := checkVMOwnershipByConfig(ctx, deps, op, cid, loc)
	if err == nil && verdict.Absent {
		verdict, err = absentVMVerdict(ctx, deps, op, cid)
	}
	if err != nil {
		return err
	}
	return refuseVerdict(op, vmCID, cid, verdict, logger)
}

// clusterStampProbe answers whether a cluster stamp names the cluster this
// request talks to. It reads the live stamp at most once, and only when a
// stamp is neither the unknown stamp nor a declared alias, so a check whose
// verdict the VM's own tags settle costs no read.
type clusterStampProbe struct {
	ctx  context.Context
	deps Deps
	node string

	read bool
	live string
	err  error
}

// newClusterStampProbe returns a probe that reads the live stamp from node
// first, which is the node the handler located the VM on or the configured
// node, and from the other online members after it.
func newClusterStampProbe(ctx context.Context, deps Deps, node string) *clusterStampProbe {
	return &clusterStampProbe{ctx: ctx, deps: deps, node: node}
}

// liveStamp returns this cluster's stamp, reading it on the first call.
func (p *clusterStampProbe) liveStamp() (string, error) {
	if !p.read {
		p.read = true
		if p.deps.PVE == nil {
			p.err = cpierrors.Retriable("no PVE client is available to read the cluster identity")
		} else {
			p.live, p.err = pve.ReadClusterStamp(p.ctx, p.deps.PVE, p.node)
		}
	}
	return p.live, p.err
}

// matches reports whether stamp names this cluster, which holds when it equals
// the live stamp or a declared cluster_stamp_aliases entry. The unknown stamp a create writes
// when it could not read the identity names no cluster, so it never matches,
// and it costs no read. A failed read is returned as is, and the caller
// classes it.
func (p *clusterStampProbe) matches(stamp string) (bool, error) {
	if stamp == "" || stamp == vmcid.UnknownCluster {
		return false, nil
	}
	if p.deps.Config.ClusterStampKnown(stamp, "") {
		return true, nil
	}
	live, err := p.liveStamp()
	if err != nil {
		return false, err
	}
	return stamp == live, nil
}

// absentVMVerdict judges a CID whose VMID FindVMAuthoritative or the guard
// proved absent from every member of this cluster. A bare CID, and a CID
// carrying the unknown stamp, prove nothing about which cluster they name, so
// they get Absent and the answer each handler already gives a missing VM. A
// stamped CID reads the live cluster identity once. A stamp that matches it,
// or one of the declared aliases, is a proven absence, and anything else is
// a cluster-mismatch verdict with Elsewhere set, because the CID's VM may
// still run on the cluster the stamp names. A failed read is retriable,
// because nothing has been decided.
//
// Only on a mismatch does the verdict cost one more call, a scan for guests on
// this cluster that carry the CID's stamp. Such guests are what a root CA
// rotation leaves behind, so the detail suggests the alias key. They are only
// a hint, never a match, because a remote migration and a backup restore both
// copy the tag from another cluster.
func absentVMVerdict(ctx context.Context, deps Deps, op string, cid vmcid.VMCID) (vmOwnershipVerdict, error) {
	if !cid.KnownCluster() {
		return vmOwnershipVerdict{Absent: true}, nil
	}
	node := ""
	if deps.Config != nil {
		node = deps.Config.Node
	}
	probe := newClusterStampProbe(ctx, deps, node)
	same, err := probe.matches(cid.Cluster)
	if err != nil {
		return vmOwnershipVerdict{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d is absent from this cluster, and the cluster identity could not be read to tell whether CID "+
				"stamp %s names this cluster", op, cid.VMID, cid.Cluster))
	}
	if same {
		return vmOwnershipVerdict{Absent: true}, nil
	}
	detail := fmt.Sprintf("the CID names cluster %s, this cluster's stamp is %s, and no cluster_stamp_aliases entry "+
		"lists %s", cid.Cluster, probe.live, cid.Cluster)
	tag := clusterStampTagPrefix + cid.Cluster
	if count, countErr := pve.CountGuestsTagged(ctx, deps.PVE, tag); countErr != nil {
		deps.Log(ctx).Debug(op+": could not count the guests that carry the CID's cluster tag",
			log.String("tag", tag), log.Err(countErr))
	} else if count > 0 {
		detail += fmt.Sprintf(". %d guests on this cluster carry the tag %s, which a root CA rotation leaves behind "+
			"but a remote migration or a backup restore also copies. If this cluster's root CA was replaced, add %s "+
			"to pve.cluster_stamp_aliases", count, tag, cid.Cluster)
	}
	return vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch, Detail: detail, Elsewhere: true}, nil
}

// refuseAbsentVM is the answer a mutating handler gives a VMID it proved
// absent, which is VMNotFound for a proven absence and the cluster-mismatch
// refusal when a stamped CID names another cluster. It always returns an
// error, and a failed identity read is retriable.
func refuseAbsentVM(ctx context.Context, deps Deps, op, vmCID string, cid vmcid.VMCID, logger *log.Logger) error {
	verdict, err := absentVMVerdict(ctx, deps, op, cid)
	if err != nil {
		return err
	}
	if verdict.Absent {
		return cpierrors.VMNotFound(vmCID)
	}
	return refuseVerdict(op, vmCID, cid, verdict, logger)
}

// refuseAbsentElsewhere is for a handler that keeps its own answer to a
// proven absence. It returns nil when the VMID is absent and the CID names
// this cluster or no cluster, the cluster-mismatch refusal when a stamped CID
// names another cluster, and the retriable error when the identity read
// failed.
func refuseAbsentElsewhere(ctx context.Context, deps Deps, op, vmCID string, cid vmcid.VMCID, logger *log.Logger) error {
	verdict, err := absentVMVerdict(ctx, deps, op, cid)
	if err != nil {
		return err
	}
	if verdict.Absent {
		return nil
	}
	return refuseVerdict(op, vmCID, cid, verdict, logger)
}
